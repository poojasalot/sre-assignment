// Token service: gRPC API backed by Redis with structured logs, RED metrics and tracing.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	grpcCodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	pb "token-service/proto"
)

var (
	logger     *slog.Logger
	otelLogger *slog.Logger
	// Read by the single Redis pool metrics collector goroutine.
	lastRedisPoolTimeouts uint32

	grpcRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "grpc_server_requests_total", Help: "Total gRPC requests",
	}, []string{"method", "status"})
	grpcDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "grpc_server_request_duration_seconds", Help: "gRPC request duration",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
	}, []string{"method"})
	tokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "tokens_minted_total", Help: "Total tokens minted",
	}, []string{"status"})
	redisOperations = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "redis_operations_total", Help: "Redis operations by command and result",
	}, []string{"operation", "status"})
	redisDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "redis_operation_duration_seconds", Help: "Redis operation duration",
		Buckets: []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1},
	}, []string{"operation"})
	redisDBKeys = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "redis_db_keys", Help: "Approximate number of keys in the selected Redis database",
	})
	redisPoolTotal = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "redis_pool_total_connections", Help: "Redis client pool total connections",
	})
	redisPoolIdle = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "redis_pool_idle_connections", Help: "Redis client pool idle connections",
	})
	redisPoolTimeouts = promauto.NewCounter(prometheus.CounterOpts{
		Name: "redis_pool_timeouts_total", Help: "Total Redis pool acquisition timeouts",
	})
	mintLatency = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "token_mint_duration_seconds", Help: "End-to-end token mint duration",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
	})
)

const serviceName = "go-token-service"

// redisStore is satisfied by *redis.Client in both single-node and Sentinel (failover) mode.
type redisStore interface {
	redis.Cmdable
	Close() error
}

func initOtel() func() {
	ctx := context.Background()
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			semconv.ServiceVersion(envOr("SERVICE_VERSION", "1.0.0")),
			attribute.String("deployment.environment.name", envOr("DEPLOYMENT_ENVIRONMENT", "development")),
		),
	)
	if err != nil {
		res = resource.Default()
	}

	tp := sdktrace.NewTracerProvider(sdktrace.WithResource(res))
	lp := sdklog.NewLoggerProvider(sdklog.WithResource(res))
	if rawEndpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); rawEndpoint != "" {
		insecureTransport := strings.HasPrefix(strings.ToLower(rawEndpoint), "http://") ||
			strings.EqualFold(os.Getenv("OTEL_EXPORTER_OTLP_INSECURE"), "true")
		endpoint := strings.TrimPrefix(strings.TrimPrefix(rawEndpoint, "http://"), "https://")
		traceOptions := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(endpoint)}
		if insecureTransport {
			traceOptions = append(traceOptions, otlptracegrpc.WithInsecure())
		}
		traceExporter, traceErr := otlptracegrpc.New(ctx, traceOptions...)
		if traceErr == nil {
			tp = sdktrace.NewTracerProvider(sdktrace.WithBatcher(traceExporter), sdktrace.WithResource(res))
		} else {
			logger.Warn("trace_exporter_init_failed", "error", traceErr.Error())
		}
		logOptions := []otlploggrpc.Option{otlploggrpc.WithEndpoint(endpoint)}
		if insecureTransport {
			logOptions = append(logOptions, otlploggrpc.WithInsecure())
		}
		logExporter, logErr := otlploggrpc.New(ctx, logOptions...)
		if logErr == nil {
			lp = sdklog.NewLoggerProvider(
				sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter)),
				sdklog.WithResource(res),
			)
		} else {
			logger.Warn("log_exporter_init_failed", "error", logErr.Error())
		}
	}

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	global.SetLoggerProvider(lp)
	otelLogger = otelslog.NewLogger(serviceName)
	return func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tp.Shutdown(shutdownCtx)
		_ = lp.Shutdown(shutdownCtx)
	}
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func logArgsWithTrace(ctx context.Context, args []any) []any {
	spanContext := trace.SpanContextFromContext(ctx)
	if spanContext.IsValid() {
		args = append(args, "trace_id", spanContext.TraceID().String(), "span_id", spanContext.SpanID().String())
	}
	return args
}

func logInfo(ctx context.Context, message string, args ...any) {
	if logger != nil {
		logger.InfoContext(ctx, message, logArgsWithTrace(ctx, append([]any{}, args...))...)
	}
	if otelLogger != nil {
		otelLogger.InfoContext(ctx, message, args...)
	}
}

func logWarn(ctx context.Context, message string, args ...any) {
	if logger != nil {
		logger.WarnContext(ctx, message, logArgsWithTrace(ctx, append([]any{}, args...))...)
	}
	if otelLogger != nil {
		otelLogger.WarnContext(ctx, message, args...)
	}
}

func logError(ctx context.Context, message string, args ...any) {
	if logger != nil {
		logger.ErrorContext(ctx, message, logArgsWithTrace(ctx, append([]any{}, args...))...)
	}
	if otelLogger != nil {
		otelLogger.ErrorContext(ctx, message, args...)
	}
}

func metricsUnaryInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	start := time.Now()
	resp, err := handler(ctx, req)
	code := status.Code(err).String()
	grpcRequests.WithLabelValues(info.FullMethod, code).Inc()
	grpcDuration.WithLabelValues(info.FullMethod).Observe(time.Since(start).Seconds())
	if err != nil {
		span := trace.SpanFromContext(ctx)
		span.RecordError(err)
		span.SetStatus(codes.Error, code)
	}
	return resp, err
}

func observeRedis(ctx context.Context, operation string, fn func(context.Context) error) error {
	// Use a sanitised client span rather than recording the raw Redis command.
	// SET/GET arguments contain opaque tokens and user identifiers.
	opCtx, span := otel.Tracer(serviceName).Start(
		ctx,
		"redis."+operation,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("db.system", "redis"),
			attribute.String("db.operation", operation),
		),
	)
	defer span.End()

	started := time.Now()
	err := fn(opCtx)
	redisDuration.WithLabelValues(operation).Observe(time.Since(started).Seconds())
	result := "success"
	if errors.Is(err, redis.Nil) {
		result = "not_found"
	} else if err != nil {
		result = "error"
		span.RecordError(err)
		span.SetStatus(codes.Error, "Redis operation failed")
	}
	redisOperations.WithLabelValues(operation, result).Inc()
	return err
}

type server struct {
	pb.UnimplementedTokenServiceServer
	rdb   redisStore
	audit *auditQueue
}

func (s *server) MintToken(ctx context.Context, req *pb.MintTokenRequest) (*pb.MintTokenResponse, error) {
	started := time.Now()
	if strings.TrimSpace(req.UserEmail) == "" {
		return nil, status.Error(grpcCodes.InvalidArgument, "user_email is required")
	}
	if req.TtlSeconds <= 0 || req.TtlSeconds > 86400 {
		return nil, status.Error(grpcCodes.InvalidArgument, "ttl_seconds must be between 1 and 86400")
	}
	if len(req.Permissions) > 100 {
		return nil, status.Error(grpcCodes.InvalidArgument, "too many permissions")
	}

	randomBytes := make([]byte, 32)
	if _, err := rand.Read(randomBytes); err != nil {
		tokensTotal.WithLabelValues("error").Inc()
		logError(ctx, "secure_token_generation_failed", "error", err.Error())
		return nil, status.Error(grpcCodes.Internal, "token generation failed")
	}
	token := hex.EncodeToString(randomBytes)
	ttl := time.Duration(req.TtlSeconds) * time.Second
	err := observeRedis(ctx, "set", func(opCtx context.Context) error {
		return s.rdb.Set(opCtx, "token:"+token, strings.ToLower(strings.TrimSpace(req.UserEmail)), ttl).Err()
	})
	if err != nil {
		tokensTotal.WithLabelValues("error").Inc()
		logError(ctx, "redis_token_write_failed", "error", err.Error())
		return nil, status.Error(grpcCodes.Unavailable, "token storage unavailable")
	}

	// The audit trail is best effort: a failed enqueue must not fail an issued token.
	if err := s.audit.publish(ctx, len(req.Permissions), req.TtlSeconds); err != nil {
		logWarn(ctx, "audit_event_publish_failed", "error", err.Error())
	}

	tokensTotal.WithLabelValues("success").Inc()
	mintLatency.Observe(time.Since(started).Seconds())
	logInfo(ctx, "token_minted", "permission_count", len(req.Permissions), "ttl_seconds", req.TtlSeconds)
	return &pb.MintTokenResponse{Token: token}, nil
}

func (s *server) ValidateToken(ctx context.Context, req *pb.ValidateTokenRequest) (*pb.ValidateTokenResponse, error) {
	if strings.TrimSpace(req.Token) == "" {
		return nil, status.Error(grpcCodes.InvalidArgument, "token is required")
	}
	var email string
	err := observeRedis(ctx, "get", func(opCtx context.Context) error {
		var getErr error
		email, getErr = s.rdb.Get(opCtx, "token:"+req.Token).Result()
		return getErr
	})
	if errors.Is(err, redis.Nil) {
		return &pb.ValidateTokenResponse{Valid: false}, nil
	}
	if err != nil {
		logError(ctx, "redis_token_read_failed", "error", err.Error())
		return nil, status.Error(grpcCodes.Unavailable, "token storage unavailable")
	}
	return &pb.ValidateTokenResponse{Valid: true, UserEmail: email}, nil
}

func startMetricsAndHealthServer(rdb redisStore) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"alive"}`))
	})
	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if err := rdb.Ping(ctx).Err(); err != nil {
			http.Error(w, `{"status":"not_ready"}`, http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})
	server := &http.Server{Addr: ":9090", Handler: mux, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logError(context.Background(), "health_metrics_server_failed", "error", err.Error())
		}
	}()
	return server
}

func collectRedisDBSize(ctx context.Context, rdb redisStore) {
	collect := func() {
		queryCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		var size int64
		err := observeRedis(queryCtx, "dbsize", func(opCtx context.Context) error {
			var dbErr error
			size, dbErr = rdb.DBSize(opCtx).Result()
			return dbErr
		})
		if err == nil {
			redisDBKeys.Set(float64(size))
		}
		if poolClient, ok := rdb.(interface{ PoolStats() *redis.PoolStats }); ok {
			stats := poolClient.PoolStats()
			redisPoolTotal.Set(float64(stats.TotalConns))
			redisPoolIdle.Set(float64(stats.IdleConns))
			if stats.Timeouts >= lastRedisPoolTimeouts {
				redisPoolTimeouts.Add(float64(stats.Timeouts - lastRedisPoolTimeouts))
			} else {
				// The underlying client pool was reset; preserve the process counter monotonically.
				redisPoolTimeouts.Add(float64(stats.Timeouts))
			}
			lastRedisPoolTimeouts = stats.Timeouts
		}
	}
	collect()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			collect()
		}
	}
}

// grpcServerCredentials returns nil (plaintext local demo) unless GRPC_TLS_SERVER_CERT/KEY are set.
// Once TLS is requested, any certificate problem is fatal: it never falls back to plaintext.
// GRPC_TLS_CA_CERT additionally requires and verifies client certificates (mTLS).
func grpcServerCredentials() (credentials.TransportCredentials, error) {
	serverCert := os.Getenv("GRPC_TLS_SERVER_CERT")
	serverKey := os.Getenv("GRPC_TLS_SERVER_KEY")
	clientCA := os.Getenv("GRPC_TLS_CA_CERT")
	if serverCert == "" && serverKey == "" && clientCA == "" {
		return nil, nil
	}
	certificate, err := tls.LoadX509KeyPair(serverCert, serverKey)
	if err != nil {
		return nil, fmt.Errorf("load gRPC server certificate: %w", err)
	}
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
	}
	if clientCA != "" {
		caPEM, err := os.ReadFile(clientCA)
		if err != nil {
			return nil, fmt.Errorf("read gRPC client CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, errors.New("GRPC_TLS_CA_CERT contains no valid certificates")
		}
		tlsConfig.ClientCAs = pool
		tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return credentials.NewTLS(tlsConfig), nil
}

func main() {
	logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cleanupOtel := initOtel()
	defer cleanupOtel()

	transportCredentials, err := grpcServerCredentials()
	if err != nil {
		logError(context.Background(), "grpc_tls_config_invalid", "error", err.Error())
		os.Exit(1)
	}
	transportMode := "plaintext_demo"
	if transportCredentials != nil {
		transportMode = "tls"
	}

	redisAddr := envOr("REDIS_ADDR", "localhost:6379")
	redisPassword := os.Getenv("REDIS_PASSWORD")
	var rdb redisStore
	if rawSentinels := os.Getenv("REDIS_SENTINELS"); rawSentinels != "" {
		parts := strings.Split(rawSentinels, ",")
		sentinelAddrs := make([]string, 0, len(parts))
		for _, part := range parts {
			if addr := strings.TrimSpace(part); addr != "" {
				sentinelAddrs = append(sentinelAddrs, addr)
			}
		}
		rdb = redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName: envOr("REDIS_SENTINEL_MASTER", "mymaster"), SentinelAddrs: sentinelAddrs,
			Password: redisPassword, DB: 0,
			DialTimeout: 3 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second,
			PoolSize: 50, MinIdleConns: 5, MaxRetries: 2,
		})
	} else {
		rdb = redis.NewClient(&redis.Options{
			Addr: redisAddr, Password: redisPassword, DB: 0,
			DialTimeout: 3 * time.Second, ReadTimeout: 2 * time.Second, WriteTimeout: 2 * time.Second,
			PoolSize: 50, MinIdleConns: 5, MaxRetries: 2,
			MaintNotificationsConfig: &maintnotifications.Config{Mode: maintnotifications.ModeDisabled},
		})
	}
	httpServer := startMetricsAndHealthServer(rdb)
	collectorCtx, cancelCollector := context.WithCancel(context.Background())
	go collectRedisDBSize(collectorCtx, rdb)
	hostname, _ := os.Hostname()
	audit := &auditQueue{rdb: rdb, consumer: hostname}
	prometheus.MustRegister(newStreamCollector(rdb))
	auditDone := make(chan struct{})
	go func() { audit.run(collectorCtx); close(auditDone) }()

	lis, err := net.Listen("tcp", ":50051")
	if err != nil {
		logError(context.Background(), "grpc_listener_failed", "error", err.Error())
		cancelCollector()
		_ = rdb.Close()
		os.Exit(1)
	}
	serverOptions := []grpc.ServerOption{
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.UnaryInterceptor(metricsUnaryInterceptor),
	}
	if transportCredentials != nil {
		serverOptions = append(serverOptions, grpc.Creds(transportCredentials))
	}
	grpcServer := grpc.NewServer(serverOptions...)
	pb.RegisterTokenServiceServer(grpcServer, &server{rdb: rdb, audit: audit})

	serveErr := make(chan error, 1)
	go func() { serveErr <- grpcServer.Serve(lis) }()
	logInfo(context.Background(), "go_token_service_started", "grpc_port", 50051, "metrics_port", 9090, "grpc_transport", transportMode)

	signalCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case <-signalCtx.Done():
		logInfo(context.Background(), "shutdown_started")
	case err := <-serveErr:
		if err != nil {
			logError(context.Background(), "grpc_server_failed", "error", err.Error())
		}
	}

	cancelCollector()
	gracefulDone := make(chan struct{})
	go func() { grpcServer.GracefulStop(); close(gracefulDone) }()
	select {
	case <-gracefulDone:
	case <-time.After(10 * time.Second):
		grpcServer.Stop()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	<-auditDone
	if err := rdb.Close(); err != nil {
		logWarn(context.Background(), "redis_close_failed", "error", fmt.Sprint(err))
	}
}
