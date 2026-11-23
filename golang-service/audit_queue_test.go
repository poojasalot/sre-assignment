package main

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func newMiniRedis(t *testing.T) (*miniredis.Miniredis, *redis.Client) {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return mr, client
}

// runUntilAcknowledged runs the consumer until the newest entry is delivered and nothing is pending.
// (miniredis does not track group lag, so delivery is checked via the last delivered ID.)
func runUntilAcknowledged(t *testing.T, queue *auditQueue) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { queue.run(ctx); close(done) }()
	defer func() { stop(); <-done }()

	newest, err := queue.rdb.XRevRangeN(context.Background(), auditStream, "+", "-", 1).Result()
	if err != nil || len(newest) != 1 {
		t.Fatalf("stream must contain a message: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		groups, err := queue.rdb.XInfoGroups(context.Background(), auditStream).Result()
		if err == nil && len(groups) == 1 && groups[0].Pending == 0 && groups[0].LastDeliveredID == newest[0].ID {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("audit messages were not all acknowledged")
}

func TestAuditQueueConsumesAndContinuesTrace(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	otel.SetTextMapPropagator(propagation.TraceContext{})

	_, rdb := newMiniRedis(t)
	queue := &auditQueue{rdb: rdb, consumer: "test"}

	ctx, request := otel.Tracer("test").Start(context.Background(), "MintToken")
	if err := queue.publish(ctx, 2, 60); err != nil {
		t.Fatalf("publish: %v", err)
	}
	request.End()
	runUntilAcknowledged(t, queue)

	var consumer sdktrace.ReadOnlySpan
	for _, span := range recorder.Ended() {
		if span.Name() == "audit.process" {
			consumer = span
		}
	}
	if consumer == nil || consumer.SpanContext().TraceID() != request.SpanContext().TraceID() {
		t.Fatalf("consumer span must join the producer trace %s", request.SpanContext().TraceID())
	}
}

func TestAuditQueueReclaimsMessagesFromDeadConsumer(t *testing.T) {
	mr, rdb := newMiniRedis(t)
	ctx := context.Background()
	start := time.Now()
	mr.SetTime(start)
	if err := rdb.XGroupCreateMkStream(ctx, auditStream, auditGroup, "0").Err(); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if err := (&auditQueue{rdb: rdb}).publish(ctx, 1, 60); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// A consumer reads the message and dies before acknowledging it.
	if err := rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: auditGroup, Consumer: "crashed-pod", Streams: []string{auditStream, ">"}, Count: 1,
	}).Err(); err != nil {
		t.Fatalf("read: %v", err)
	}
	mr.SetTime(start.Add(auditClaimIdle + time.Second))

	runUntilAcknowledged(t, &auditQueue{rdb: rdb, consumer: "live-pod"})
}

func TestStreamCollectorReportsQueueDepth(t *testing.T) {
	_, rdb := newMiniRedis(t)
	queue := &auditQueue{rdb: rdb, consumer: "test"}
	ctx := context.Background()
	if err := rdb.XGroupCreateMkStream(ctx, auditStream, auditGroup, "0").Err(); err != nil {
		t.Fatalf("create group: %v", err)
	}
	for range 3 {
		if err := queue.publish(ctx, 1, 60); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	// Deliver one message without acknowledging it: lag 2, pending 1.
	if err := rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group: auditGroup, Consumer: "test", Streams: []string{auditStream, ">"}, Count: 1,
	}).Err(); err != nil {
		t.Fatalf("read: %v", err)
	}

	registry := prometheus.NewPedanticRegistry()
	registry.MustRegister(newStreamCollector(rdb))
	expected := `
# HELP redis_stream_length Entries currently retained in the stream
# TYPE redis_stream_length gauge
redis_stream_length{stream="token:events"} 3
# HELP redis_stream_pending_messages Messages delivered to consumers but not acknowledged
# TYPE redis_stream_pending_messages gauge
redis_stream_pending_messages{stream="token:events"} 1
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(expected),
		"redis_stream_length", "redis_stream_pending_messages"); err != nil {
		t.Fatal(err)
	}
	if count, err := testutil.GatherAndCount(registry, "redis_stream_lag_messages", "redis_stream_oldest_pending_age_seconds"); err != nil || count != 2 {
		t.Fatalf("expected lag and oldest-age series, got %d (%v)", count, err)
	}
}

func TestOldestAgeSeconds(t *testing.T) {
	if got := oldestAgeSeconds(&redis.XPending{Count: 0}); got != 0 {
		t.Fatalf("empty pending list must report 0, got %v", got)
	}
	id := strconv.FormatInt(time.Now().Add(-10*time.Second).UnixMilli(), 10) + "-0"
	if got := oldestAgeSeconds(&redis.XPending{Count: 1, Lower: id}); got < 9 || got > 12 {
		t.Fatalf("expected ~10s, got %v", got)
	}
}
