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

type fakeRedis struct {
	values map[string]string
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
func (f *fakeRedis) Close() error { return nil }

func TestMintTokenStoresOpaqueToken(t *testing.T) {
	store := newFakeRedis()
	svc := &server{rdb: store}
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
}

func TestMintTokenRejectsInvalidTTL(t *testing.T) {
	svc := &server{rdb: newFakeRedis()}
	_, err := svc.MintToken(context.Background(), &pb.MintTokenRequest{UserEmail: "a@b.co", TtlSeconds: 0})
	if status.Code(err) != grpcCodes.InvalidArgument {
		t.Fatalf("got %v, want InvalidArgument", err)
	}
}

func TestMintTokenMapsRedisFailureToUnavailable(t *testing.T) {
	store := newFakeRedis()
	store.setErr = context.DeadlineExceeded
	svc := &server{rdb: store}
	_, err := svc.MintToken(context.Background(), &pb.MintTokenRequest{UserEmail: "a@b.co", TtlSeconds: 30})
	if status.Code(err) != grpcCodes.Unavailable {
		t.Fatalf("got %v, want Unavailable", err)
	}
}

func TestValidateTokenFoundAndMissing(t *testing.T) {
	store := newFakeRedis()
	store.values["token:known"] = "alice@example.com"
	svc := &server{rdb: store}
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
	svc := &server{rdb: store}
	_, err := svc.ValidateToken(context.Background(), &pb.ValidateTokenRequest{Token: "x"})
	if status.Code(err) != grpcCodes.Unavailable {
		t.Fatalf("got %v, want Unavailable", err)
	}
	if !strings.Contains(err.Error(), "token storage unavailable") {
		t.Fatalf("unexpected error: %v", err)
	}
}
