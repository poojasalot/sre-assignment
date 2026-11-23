package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	grpcCodes "google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	pb "token-service/proto"
)

// fakeRedis implements the commands the token handlers use; other redis.Cmdable methods are unused.
type fakeRedis struct {
	redis.Cmdable
	values map[string]string
	events []map[string]any
	setErr error
	getErr error
}

func newFakeRedis() *fakeRedis { return &fakeRedis{values: map[string]string{}} }
func (f *fakeRedis) Set(_ context.Context, key string, value interface{}, _ time.Duration) *redis.StatusCmd {
	if f.setErr != nil {
		return redis.NewStatusResult("", f.setErr)
	}
	f.values[key] = value.(string)
	return redis.NewStatusResult("OK", nil)
}
func (f *fakeRedis) Get(_ context.Context, key string) *redis.StringCmd {
	if f.getErr != nil {
		return redis.NewStringResult("", f.getErr)
	}
	value, ok := f.values[key]
	if !ok {
		return redis.NewStringResult("", redis.Nil)
	}
	return redis.NewStringResult(value, nil)
}
func (f *fakeRedis) Ping(context.Context) *redis.StatusCmd { return redis.NewStatusResult("PONG", nil) }
func (f *fakeRedis) DBSize(context.Context) *redis.IntCmd {
	return redis.NewIntResult(int64(len(f.values)), nil)
}
func (f *fakeRedis) XAdd(_ context.Context, args *redis.XAddArgs) *redis.StringCmd {
	f.events = append(f.events, args.Values.(map[string]any))
	return redis.NewStringResult("1-0", nil)
}
func (f *fakeRedis) Close() error { return nil }

func newTestServer(store *fakeRedis) *server {
	return &server{rdb: store, audit: &auditQueue{rdb: store, consumer: "test"}}
}

func TestMintTokenStoresOpaqueToken(t *testing.T) {
	store := newFakeRedis()
	svc := newTestServer(store)
	response, err := svc.MintToken(context.Background(), &pb.MintTokenRequest{
		UserEmail: "ALICE@example.com", Permissions: []string{"read:data"}, TtlSeconds: 3600,
	})
	if err != nil {
		t.Fatalf("MintToken returned error: %v", err)
	}
	if len(response.Token) != 64 {
		t.Fatalf("expected 64-character token, got %d", len(response.Token))
	}
	if got := store.values["token:"+response.Token]; got != "alice@example.com" {
		t.Fatalf("token maps to %q, want normalized email", got)
	}
	if len(store.events) != 1 || store.events[0]["event"] != "token_minted" || store.events[0]["permission_count"] != 1 {
		t.Fatalf("expected one token_minted audit event, got %#v", store.events)
	}
}

func TestMintTokenRejectsInvalidTTL(t *testing.T) {
	svc := newTestServer(newFakeRedis())
	_, err := svc.MintToken(context.Background(), &pb.MintTokenRequest{UserEmail: "a@b.co", TtlSeconds: 0})
	if status.Code(err) != grpcCodes.InvalidArgument {
		t.Fatalf("got %v, want InvalidArgument", err)
	}
}

func TestMintTokenMapsRedisFailureToUnavailable(t *testing.T) {
	store := newFakeRedis()
	store.setErr = context.DeadlineExceeded
	svc := newTestServer(store)
	_, err := svc.MintToken(context.Background(), &pb.MintTokenRequest{UserEmail: "a@b.co", TtlSeconds: 30})
	if status.Code(err) != grpcCodes.Unavailable {
		t.Fatalf("got %v, want Unavailable", err)
	}
}

func TestValidateTokenFoundAndMissing(t *testing.T) {
	store := newFakeRedis()
	store.values["token:known"] = "alice@example.com"
	svc := newTestServer(store)
	valid, err := svc.ValidateToken(context.Background(), &pb.ValidateTokenRequest{Token: "known"})
	if err != nil || !valid.Valid || valid.UserEmail != "alice@example.com" {
		t.Fatalf("unexpected valid response: %#v, %v", valid, err)
	}
	missing, err := svc.ValidateToken(context.Background(), &pb.ValidateTokenRequest{Token: "missing"})
	if err != nil || missing.Valid {
		t.Fatalf("unexpected missing response: %#v, %v", missing, err)
	}
}

func TestValidateTokenMapsRedisFailureToUnavailable(t *testing.T) {
	store := newFakeRedis()
	store.getErr = context.DeadlineExceeded
	svc := newTestServer(store)
	_, err := svc.ValidateToken(context.Background(), &pb.ValidateTokenRequest{Token: "x"})
	if status.Code(err) != grpcCodes.Unavailable {
		t.Fatalf("got %v, want Unavailable", err)
	}
	if !strings.Contains(err.Error(), "token storage unavailable") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGRPCServerCredentialsFailsClosed(t *testing.T) {
	if creds, err := grpcServerCredentials(); creds != nil || err != nil {
		t.Fatalf("no TLS variables must mean plaintext, got %v, %v", creds, err)
	}
	t.Setenv("GRPC_TLS_CA_CERT", "/run/tls/ca.crt")
	if creds, err := grpcServerCredentials(); creds != nil || err == nil {
		t.Fatalf("a client CA without a server certificate must be an error")
	}
	t.Setenv("GRPC_TLS_SERVER_CERT", "/nonexistent/tls.crt")
	t.Setenv("GRPC_TLS_SERVER_KEY", "/nonexistent/tls.key")
	if creds, err := grpcServerCredentials(); creds != nil || err == nil {
		t.Fatalf("unreadable certificates must be an error, never a plaintext fallback")
	}
}
