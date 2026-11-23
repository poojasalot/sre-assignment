package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/redis/go-redis/v9"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Every minted token publishes an audit event to a Redis Stream; a consumer group records and
// acknowledges it asynchronously. The W3C trace context travels inside the message, so the
// consumer span joins the trace of the request that produced the event.
const (
	auditStream       = "token:events"
	auditGroup        = "audit"
	auditStreamMaxLen = 10000
	// Consumers are named after the pod, so a crashed pod's unacknowledged messages are reclaimed
	// by any live consumer once they have been idle this long (at-least-once delivery).
	auditClaimIdle = 30 * time.Second
)

var (
	queueProcessed = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "queue_messages_processed_total", Help: "Queue messages handled by this consumer",
	}, []string{"queue", "status"})
	queueProcessing = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "queue_message_processing_duration_seconds", Help: "Time to process and acknowledge one queue message",
		Buckets: []float64{0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1},
	}, []string{"queue"})
)

type auditQueue struct {
	rdb      redis.Cmdable
	consumer string
}

func (q *auditQueue) publish(ctx context.Context, permissionCount int, ttlSeconds int32) error {
	return observeRedis(ctx, "xadd", func(opCtx context.Context) error {
		values := map[string]any{"event": "token_minted", "permission_count": permissionCount, "ttl_seconds": ttlSeconds}
		carrier := propagation.MapCarrier{}
		otel.GetTextMapPropagator().Inject(opCtx, carrier)
		for key, value := range carrier {
			values[key] = value
		}
		return q.rdb.XAdd(opCtx, &redis.XAddArgs{
			Stream: auditStream, MaxLen: auditStreamMaxLen, Approx: true, Values: values,
		}).Err()
	})
}

// run consumes the stream until ctx is cancelled, retrying with a pause when Redis is unavailable.
func (q *auditQueue) run(ctx context.Context) {
	for ctx.Err() == nil {
		if err := q.consume(ctx); err != nil && ctx.Err() == nil {
			logWarn(ctx, "audit_consumer_failed", "error", err.Error())
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}
}

func (q *auditQueue) consume(ctx context.Context) error {
	err := q.rdb.XGroupCreateMkStream(ctx, auditStream, auditGroup, "0").Err()
	if err != nil && !strings.HasPrefix(err.Error(), "BUSYGROUP") {
		return err
	}
	for {
		claimed, _, err := q.rdb.XAutoClaim(ctx, &redis.XAutoClaimArgs{
			Stream: auditStream, Group: auditGroup, Consumer: q.consumer,
			MinIdle: auditClaimIdle, Start: "0-0", Count: 10,
		}).Result()
		if err != nil {
			return err
		}
		for _, message := range claimed {
			q.process(ctx, message)
		}

		streams, err := q.rdb.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group: auditGroup, Consumer: q.consumer, Streams: []string{auditStream, ">"},
			Count: 10, Block: 2 * time.Second,
		}).Result()
		if err == redis.Nil {
			continue // block timeout, nothing new
		}
		if err != nil {
			return err
		}
		for _, stream := range streams {
			for _, message := range stream.Messages {
				q.process(ctx, message)
			}
		}
	}
}

func (q *auditQueue) process(ctx context.Context, message redis.XMessage) {
	started := time.Now()
	carrier := propagation.MapCarrier{}
	for key, value := range message.Values {
		carrier[key] = fmt.Sprint(value)
	}
	parent := otel.GetTextMapPropagator().Extract(ctx, carrier)
	spanCtx, span := otel.Tracer(serviceName).Start(parent, "audit.process",
		trace.WithSpanKind(trace.SpanKindConsumer),
		trace.WithAttributes(attribute.String("messaging.system", "redis"), attribute.String("messaging.message.id", message.ID)),
	)
	defer span.End()

	logInfo(spanCtx, "token_audit_recorded", "message_id", message.ID,
		"event", carrier["event"], "permission_count", carrier["permission_count"])

	// Acknowledge even while shutting down so a processed message is not redelivered.
	ackCtx, cancel := context.WithTimeout(context.WithoutCancel(spanCtx), 2*time.Second)
	defer cancel()
	status := "success"
	if err := observeRedis(ackCtx, "xack", func(opCtx context.Context) error {
		return q.rdb.XAck(opCtx, auditStream, auditGroup, message.ID).Err()
	}); err != nil {
		status = "error"
		logWarn(spanCtx, "audit_ack_failed", "message_id", message.ID, "error", err.Error())
	}
	queueProcessed.WithLabelValues(auditStream, status).Inc()
	queueProcessing.WithLabelValues(auditStream).Observe(time.Since(started).Seconds())
}

// streamCollector reports queue depth at scrape time. Total backlog = lag (not yet delivered)
// + pending (delivered, not yet acknowledged).
type streamCollector struct {
	rdb                          redis.Cmdable
	length, pending, lag, oldest *prometheus.Desc
}

func newStreamCollector(rdb redis.Cmdable) *streamCollector {
	labels := prometheus.Labels{"stream": auditStream}
	desc := func(name, help string) *prometheus.Desc { return prometheus.NewDesc(name, help, nil, labels) }
	return &streamCollector{
		rdb:     rdb,
		length:  desc("redis_stream_length", "Entries currently retained in the stream"),
		pending: desc("redis_stream_pending_messages", "Messages delivered to consumers but not acknowledged"),
		lag:     desc("redis_stream_lag_messages", "Messages not yet delivered to the consumer group"),
		oldest:  desc("redis_stream_oldest_pending_age_seconds", "Age of the oldest unacknowledged message"),
	}
}

func (c *streamCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.length
	ch <- c.pending
	ch <- c.lag
	ch <- c.oldest
}

func (c *streamCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	emit := func(desc *prometheus.Desc, value float64) {
		ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value)
	}

	if length, err := c.rdb.XLen(ctx, auditStream).Result(); err == nil {
		emit(c.length, float64(length))
	}
	groups, err := c.rdb.XInfoGroups(ctx, auditStream).Result()
	if err != nil {
		return // stream or group not created yet, or Redis unavailable
	}
	for _, group := range groups {
		if group.Name == auditGroup {
			emit(c.pending, float64(group.Pending))
			emit(c.lag, float64(group.Lag))
		}
	}
	if pending, err := c.rdb.XPending(ctx, auditStream, auditGroup).Result(); err == nil {
		emit(c.oldest, oldestAgeSeconds(pending))
	}
}

// oldestAgeSeconds derives the age from the oldest pending ID, whose prefix is a millisecond timestamp.
func oldestAgeSeconds(pending *redis.XPending) float64 {
	if pending.Count == 0 {
		return 0
	}
	millis, err := strconv.ParseInt(strings.SplitN(pending.Lower, "-", 2)[0], 10, 64)
	if err != nil {
		return 0
	}
	return max(0, time.Since(time.UnixMilli(millis)).Seconds())
}
