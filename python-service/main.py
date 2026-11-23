"""HTTP API for token issuance. Dependencies: PostgreSQL and the Go gRPC token service."""
from __future__ import annotations

import json
import logging
import os
import threading
import time
from datetime import datetime, timezone
from contextlib import asynccontextmanager, contextmanager
from typing import Iterator

import grpc
import psycopg2
from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import Response
from opentelemetry import trace
from opentelemetry._logs import set_logger_provider
from opentelemetry.exporter.otlp.proto.grpc.trace_exporter import OTLPSpanExporter
from opentelemetry.instrumentation.fastapi import FastAPIInstrumentor
from opentelemetry.instrumentation.grpc import GrpcInstrumentorClient
from opentelemetry.instrumentation.psycopg2 import Psycopg2Instrumentor
from opentelemetry.propagate import set_global_textmap
from opentelemetry.sdk._logs import LoggerProvider, LoggingHandler
from opentelemetry.sdk._logs.export import BatchLogRecordProcessor
from opentelemetry.sdk.resources import Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor
from opentelemetry.trace.propagation.tracecontext import TraceContextTextMapPropagator
from prometheus_client import CONTENT_TYPE_LATEST, Counter, Gauge, Histogram, generate_latest
from psycopg2.pool import PoolError, ThreadedConnectionPool
from pydantic import BaseModel, Field, field_validator

import token_pb2
import token_pb2_grpc

SERVICE_NAME = os.getenv("OTEL_SERVICE_NAME", "python-token-service")
SERVICE_VERSION = os.getenv("SERVICE_VERSION", "1.0.0")
ENVIRONMENT = os.getenv("DEPLOYMENT_ENVIRONMENT", "development")
DATABASE_URL = os.getenv("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/tokendb")
GRPC_HOST = os.getenv("TOKEN_SERVICE_HOST", "localhost:50051")
GRPC_TLS_CA_CERT = os.getenv("GRPC_TLS_CA_CERT", "")
GRPC_TLS_CLIENT_CERT = os.getenv("GRPC_TLS_CLIENT_CERT", "")
GRPC_TLS_CLIENT_KEY = os.getenv("GRPC_TLS_CLIENT_KEY", "")
GRPC_TLS_SERVER_NAME = os.getenv("GRPC_TLS_SERVER_NAME", "")
TOKEN_TTL_SECONDS = int(os.getenv("TOKEN_TTL_SECONDS", "3600"))
DB_POOL_MIN = int(os.getenv("DB_POOL_MIN", "1"))
DB_POOL_MAX = int(os.getenv("DB_POOL_MAX", "10"))
DB_POOL_WAIT_TIMEOUT_SECONDS = float(os.getenv("DB_POOL_WAIT_TIMEOUT_SECONDS", "2"))
GRPC_TIMEOUT_SECONDS = float(os.getenv("GRPC_TIMEOUT_SECONDS", "3"))
if not (1 <= TOKEN_TTL_SECONDS <= 86400):
    raise ValueError("TOKEN_TTL_SECONDS must be between 1 and 86400")
if DB_POOL_MIN < 1 or DB_POOL_MAX < DB_POOL_MIN:
    raise ValueError("DB_POOL_MIN/DB_POOL_MAX are invalid")
if GRPC_TIMEOUT_SECONDS <= 0:
    raise ValueError("GRPC_TIMEOUT_SECONDS must be positive")
OTLP_ENDPOINT = os.getenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

# OpenTelemetry: W3C Trace Context is propagated automatically by the FastAPI,
# psycopg2 and gRPC instrumentors. Export is disabled only when no endpoint is set.
resource = Resource.create({
    "service.name": SERVICE_NAME,
    "service.version": SERVICE_VERSION,
    "deployment.environment.name": ENVIRONMENT,
})
trace_provider = TracerProvider(resource=resource)
if OTLP_ENDPOINT:
    trace_provider.add_span_processor(BatchSpanProcessor(OTLPSpanExporter()))
trace.set_tracer_provider(trace_provider)
set_global_textmap(TraceContextTextMapPropagator())
Psycopg2Instrumentor().instrument(tracer_provider=trace_provider)
GrpcInstrumentorClient().instrument(tracer_provider=trace_provider)

log_provider = LoggerProvider(resource=resource)
if OTLP_ENDPOINT:
    from opentelemetry.exporter.otlp.proto.grpc._log_exporter import OTLPLogExporter

    log_provider.add_log_record_processor(BatchLogRecordProcessor(OTLPLogExporter()))
set_logger_provider(log_provider)


class TraceContextFilter(logging.Filter):
    """Attach the active span identifiers to every JSON log record."""

    def filter(self, record: logging.LogRecord) -> bool:
        span_context = trace.get_current_span().get_span_context()
        record.trace_id = format(span_context.trace_id, "032x") if span_context.is_valid else ""
        record.span_id = format(span_context.span_id, "016x") if span_context.is_valid else ""
        record.service = SERVICE_NAME
        record.environment = ENVIRONMENT
        return True


class JsonFormatter(logging.Formatter):
    def format(self, record: logging.LogRecord) -> str:
        payload = {
            "timestamp": datetime.fromtimestamp(record.created, tz=timezone.utc).isoformat(),
            "level": record.levelname,
            "service": getattr(record, "service", SERVICE_NAME),
            "environment": getattr(record, "environment", ENVIRONMENT),
            "message": record.getMessage(),
            "trace_id": getattr(record, "trace_id", ""),
            "span_id": getattr(record, "span_id", ""),
        }
        standard_fields = set(logging.makeLogRecord({}).__dict__) | {"message", "asctime"}
        for key, value in record.__dict__.items():
            if key not in standard_fields and key not in payload and not key.startswith("_"):
                payload[key] = value
        if record.exc_info:
            payload["exception"] = self.formatException(record.exc_info)
        return json.dumps(payload, separators=(",", ":"), ensure_ascii=False, default=str)


stream_handler = logging.StreamHandler()
stream_handler.setFormatter(JsonFormatter())
stream_handler.addFilter(TraceContextFilter())
handlers: list[logging.Handler] = [stream_handler]
if OTLP_ENDPOINT:
    handlers.append(LoggingHandler(level=logging.INFO, logger_provider=log_provider))
logging.basicConfig(level=os.getenv("LOG_LEVEL", "INFO").upper(), handlers=handlers, force=True)
logger = logging.getLogger(SERVICE_NAME)

# Explicit, low-cardinality application/ dependency metrics.
HTTP_REQUESTS = Counter(
    "http_server_requests_total", "HTTP requests", ["method", "route", "status_code"]
)
HTTP_DURATION = Histogram(
    "http_server_request_duration_seconds", "HTTP request duration", ["method", "route"],
    buckets=(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10),
)
DB_POOL_IN_USE = Gauge("db_pool_connections_in_use", "Connections currently checked out")
DB_POOL_MAX_GAUGE = Gauge("db_pool_connections_max", "Configured maximum DB connections")
DB_POOL_ACQUIRE = Histogram(
    "db_pool_acquire_duration_seconds", "Time spent acquiring a PostgreSQL connection",
    buckets=(0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2),
)
DB_QUERY_ERRORS = Counter("db_query_errors_total", "PostgreSQL query errors")
DB_POOL_ACQUIRE_ERRORS = Counter("db_pool_acquire_errors_total", "PostgreSQL pool acquisition failures")
TOKEN_ISSUE_ERRORS = Counter("token_issue_errors_total", "Token issue errors", ["reason"])
GRPC_CLIENT_REQUESTS = Counter(
    "grpc_client_requests_total", "Python gRPC client requests", ["method", "status"]
)
GRPC_CLIENT_DURATION = Histogram(
    "grpc_client_request_duration_seconds", "Python gRPC client request duration", ["method"],
    buckets=(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5),
)

_pool: ThreadedConnectionPool | None = None
_pool_lock = threading.Lock()
_pool_slots = threading.BoundedSemaphore(DB_POOL_MAX)
_pool_in_use = 0
_pool_max = DB_POOL_MAX
_grpc_channel: grpc.Channel | None = None


def _get_pool() -> ThreadedConnectionPool:
    global _pool
    if _pool is None:
        with _pool_lock:
            if _pool is None:
                _pool = ThreadedConnectionPool(DB_POOL_MIN, DB_POOL_MAX, DATABASE_URL,
                                               connect_timeout=3, application_name=SERVICE_NAME)
                DB_POOL_MAX_GAUGE.set(DB_POOL_MAX)
    return _pool


@contextmanager
def db_connection() -> Iterator[psycopg2.extensions.connection]:
    """Borrow a pooled connection and always return it, including on exceptions."""
    global _pool_in_use
    pool = _get_pool()
    started = time.perf_counter()
    if not _pool_slots.acquire(timeout=DB_POOL_WAIT_TIMEOUT_SECONDS):
        DB_POOL_ACQUIRE.observe(time.perf_counter() - started)
        DB_POOL_ACQUIRE_ERRORS.inc()
        raise PoolError("database connection pool acquisition timed out")
    try:
        conn = pool.getconn()
    except Exception:
        _pool_slots.release()
        DB_POOL_ACQUIRE.observe(time.perf_counter() - started)
        DB_POOL_ACQUIRE_ERRORS.inc()
        raise
    DB_POOL_ACQUIRE.observe(time.perf_counter() - started)
    with _pool_lock:
        _pool_in_use += 1
        DB_POOL_IN_USE.set(_pool_in_use)
    try:
        yield conn
        conn.commit()
    except Exception:
        conn.rollback()
        raise
    finally:
        try:
            pool.putconn(conn)
        finally:
            _pool_slots.release()
            with _pool_lock:
                _pool_in_use -= 1
                DB_POOL_IN_USE.set(_pool_in_use)


class TokenRequest(BaseModel):
    user_email: str = Field(min_length=3, max_length=255)

    @field_validator("user_email")
    @classmethod
    def validate_email_shape(cls, value: str) -> str:
        value = value.strip().lower()
        if "@" not in value or value.startswith("@") or value.endswith("@"):
            raise ValueError("user_email must be a valid email address")
        return value


@asynccontextmanager
async def lifespan(_: FastAPI):
    global _grpc_channel, _pool
    if GRPC_TLS_CA_CERT:
        with open(GRPC_TLS_CA_CERT, "rb") as ca_file:
            root_certificates = ca_file.read()
        client_certificate = None
        client_key = None
        if GRPC_TLS_CLIENT_CERT:
            with open(GRPC_TLS_CLIENT_CERT, "rb") as cert_file:
                client_certificate = cert_file.read()
        if GRPC_TLS_CLIENT_KEY:
            with open(GRPC_TLS_CLIENT_KEY, "rb") as key_file:
                client_key = key_file.read()
        credentials = grpc.ssl_channel_credentials(
            root_certificates=root_certificates,
            private_key=client_key,
            certificate_chain=client_certificate,
        )
        _grpc_channel = grpc.secure_channel(
            GRPC_HOST,
            credentials,
            options=[
                ("grpc.keepalive_time_ms", 30000),
                ("grpc.keepalive_timeout_ms", 10000),
                ("grpc.ssl_target_name_override", GRPC_TLS_SERVER_NAME),
            ],
        ) if GRPC_TLS_SERVER_NAME else grpc.secure_channel(
            GRPC_HOST,
            credentials,
            options=[("grpc.keepalive_time_ms", 30000), ("grpc.keepalive_timeout_ms", 10000)],
        )
        logger.info("service_started", extra={"grpc_target": GRPC_HOST, "grpc_transport": "tls"})
    else:
        _grpc_channel = grpc.insecure_channel(
            GRPC_HOST,
            options=[("grpc.keepalive_time_ms", 30000), ("grpc.keepalive_timeout_ms", 10000)],
        )
        logger.info("service_started", extra={"grpc_target": GRPC_HOST, "grpc_transport": "plaintext_demo"})
    yield
    if _grpc_channel is not None:
        _grpc_channel.close()
    if _pool is not None:
        _pool.closeall()
        _pool = None
    trace_provider.shutdown()
    log_provider.shutdown()


app = FastAPI(title="Token Issuance API", version=SERVICE_VERSION, lifespan=lifespan)
FastAPIInstrumentor.instrument_app(app, tracer_provider=trace_provider, excluded_urls="health/live,health/ready,metrics")


@app.middleware("http")
async def record_http_red_metrics(request: Request, call_next):
    start = time.perf_counter()
    status_code = 500
    try:
        response = await call_next(request)
        status_code = response.status_code
        return response
    finally:
        route_obj = request.scope.get("route")
        route = getattr(route_obj, "path", "unmatched")
        if route not in {"/metrics", "/health/live", "/health/ready"}:
            method = request.method
            HTTP_REQUESTS.labels(method, route, str(status_code)).inc()
            HTTP_DURATION.labels(method, route).observe(time.perf_counter() - start)


@app.get("/health/live")
def liveness():
    return {"status": "alive"}


@app.get("/health/ready")
def readiness():
    try:
        with db_connection() as conn:
            with conn.cursor() as cur:
                cur.execute("SELECT 1")
        if _grpc_channel is None:
            raise RuntimeError("gRPC channel has not started")
        grpc.channel_ready_future(_grpc_channel).result(timeout=0.5)
        return {"status": "ready"}
    except Exception as exc:
        logger.warning("readiness_check_failed", extra={"dependency": "postgres_or_grpc", "error_type": type(exc).__name__})
        raise HTTPException(status_code=503, detail="dependencies not ready") from exc


@app.get("/metrics")
def metrics():
    return Response(generate_latest(), media_type=CONTENT_TYPE_LATEST)


def get_user_permissions(user_email: str) -> list[str] | None:
    query = """
        SELECT ARRAY_AGG(DISTINCT gp.permission ORDER BY gp.permission)
        FROM users u
        JOIN memberships m ON u.id = m.user_id
        JOIN group_permissions gp ON m.group_id = gp.group_id
        WHERE u.email = %s
        GROUP BY u.id
    """
    try:
        with db_connection() as conn:
            with conn.cursor() as cur:
                cur.execute(query, (user_email,))
                row = cur.fetchone()
        return row[0] if row and row[0] else None
    except (psycopg2.Error, PoolError):
        DB_QUERY_ERRORS.inc()
        logger.exception("permission_lookup_failed")
        raise


def mint_token(user_email: str, permissions: list[str]) -> str:
    if _grpc_channel is None:
        raise grpc.RpcError("gRPC channel is not initialized")
    stub = token_pb2_grpc.TokenServiceStub(_grpc_channel)
    started = time.perf_counter()
    try:
        response = stub.MintToken(
            token_pb2.MintTokenRequest(
                user_email=user_email,
                permissions=permissions,
                ttl_seconds=TOKEN_TTL_SECONDS,
            ),
            timeout=GRPC_TIMEOUT_SECONDS,
        )
        GRPC_CLIENT_REQUESTS.labels("MintToken", "OK").inc()
        return response.token
    except grpc.RpcError as exc:
        code = exc.code() if callable(getattr(exc, "code", None)) else None
        GRPC_CLIENT_REQUESTS.labels("MintToken", getattr(code, "name", "UNKNOWN")).inc()
        raise
    finally:
        GRPC_CLIENT_DURATION.labels("MintToken").observe(time.perf_counter() - started)


@app.post("/token")
def request_token(req: TokenRequest):
    try:
        permissions = get_user_permissions(req.user_email)
    except (psycopg2.Error, PoolError) as exc:
        TOKEN_ISSUE_ERRORS.labels("database").inc()
        raise HTTPException(status_code=503, detail="permission store unavailable") from exc

    if not permissions:
        TOKEN_ISSUE_ERRORS.labels("user_not_found").inc()
        raise HTTPException(status_code=404, detail="user not found")

    try:
        token = mint_token(req.user_email, permissions)
    except grpc.RpcError as exc:
        TOKEN_ISSUE_ERRORS.labels("token_service").inc()
        code = exc.code() if callable(getattr(exc, "code", None)) else None
        logger.warning("token_service_call_failed", extra={"grpc_status": str(code)})
        status_code = 504 if code == grpc.StatusCode.DEADLINE_EXCEEDED else 503
        raise HTTPException(status_code=status_code, detail="token service unavailable") from exc

    # Never put the token or email address into application logs.
    logger.info("token_issued", extra={"permission_count": len(permissions)})
    return {"token": token, "permissions": permissions, "expires_in": TOKEN_TTL_SECONDS}
