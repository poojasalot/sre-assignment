import grpc
import pytest
from fastapi.testclient import TestClient

import main


@pytest.fixture(scope="module")
def client():
    with TestClient(main.app) as test_client:
        yield test_client


class FakeRpcError(grpc.RpcError):
    def code(self):
        return grpc.StatusCode.UNAVAILABLE


def test_liveness(client):
    response = client.get("/health/live")
    assert response.status_code == 200
    assert response.json() == {"status": "alive"}


def test_invalid_email_is_rejected(client):
    response = client.post("/token", json={"user_email": "not-an-email"})
    assert response.status_code == 422


def test_unknown_user_returns_404(client, monkeypatch):
    monkeypatch.setattr(main, "get_user_permissions", lambda _: None)
    response = client.post("/token", json={"user_email": "nobody@example.com"})
    assert response.status_code == 404


def test_database_failure_returns_503(client, monkeypatch):
    def fail(_):
        import psycopg2
        raise psycopg2.OperationalError("database unavailable")

    monkeypatch.setattr(main, "get_user_permissions", fail)
    response = client.post("/token", json={"user_email": "alice@example.com"})
    assert response.status_code == 503


def test_token_service_failure_returns_503(client, monkeypatch):
    monkeypatch.setattr(main, "get_user_permissions", lambda _: ["read:data"])
    monkeypatch.setattr(main, "mint_token", lambda *_: (_ for _ in ()).throw(FakeRpcError()))
    response = client.post("/token", json={"user_email": "alice@example.com"})
    assert response.status_code == 503


def test_successful_token_request(client, monkeypatch):
    monkeypatch.setattr(main, "get_user_permissions", lambda _: ["read:data", "write:data"])
    monkeypatch.setattr(main, "mint_token", lambda *_: "test-token")
    response = client.post("/token", json={"user_email": "ALICE@example.com"})
    assert response.status_code == 200
    assert response.json()["token"] == "test-token"
    assert response.json()["permissions"] == ["read:data", "write:data"]
