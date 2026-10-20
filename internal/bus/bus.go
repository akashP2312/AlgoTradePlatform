// Package bus holds the NATS connection, the JetStream streams and KV buckets the
// terminal relies on, and the wire formats published on them.
package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Stream, bucket and subject names. Subjects follow the plan's subject table.
const (
	StreamTicks  = "TICKS"
	StreamChain  = "CHAIN"
	StreamOrders = "ORDERS"
	StreamAlerts = "ALERTS"

	KVLast    = "lastvalue"
	KVSession = "kite_session"

	SubjTicks   = "tick.>"
	SubjChain   = "chain.>"
	SubjOrder   = "order.update"
	SubjAlerts  = "alert.>"
	SubjHealth  = "sys.*.health"
	SessionKey  = "current"
	TickPrefix  = "tick."
	ChainPrefix = "chain."
)

// Conn bundles the core connection and its JetStream context.
type Conn struct {
	NC *nats.Conn
	JS jetstream.JetStream
}

// Connect dials NATS and never gives up reconnecting; name shows in server monitoring.
func Connect(url, name string) (*Conn, error) {
	if url == "" {
		return nil, fmt.Errorf("bus: nats_url is empty")
	}
	nc, err := nats.Connect(url,
		nats.Name(name),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
		nats.RetryOnFailedConnect(true),
		nats.ReconnectBufSize(64<<20),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				slog.Warn("nats disconnected", "err", err)
			}
		}),
		nats.ReconnectHandler(func(c *nats.Conn) { slog.Info("nats reconnected", "url", c.ConnectedUrlRedacted()) }),
	)
	if err != nil {
		return nil, fmt.Errorf("bus: %w", err)
	}
	js, err := jetstream.New(nc, jetstream.WithPublishAsyncMaxPending(4096))
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("bus: %w", err)
	}
	return &Conn{NC: nc, JS: js}, nil
}

func (c *Conn) Close() {
	if c != nil && c.NC != nil {
		_ = c.NC.Drain()
	}
}

// Streams returns the stream configurations, exported for the SYS page and tests.
func Streams() []jetstream.StreamConfig {
	return []jetstream.StreamConfig{
		{
			// Ticks are published on core NATS; this stream captures them so the
			// recorder can fall behind (or restart) for up to three hours without loss.
			Name: StreamTicks, Subjects: []string{SubjTicks},
			Storage: jetstream.FileStorage, Retention: jetstream.LimitsPolicy,
			MaxAge: 3 * time.Hour, MaxBytes: 4 << 30, Discard: jetstream.DiscardOld,
		},
		{
			Name: StreamChain, Subjects: []string{SubjChain},
			Storage: jetstream.FileStorage, MaxAge: 48 * time.Hour,
			Duplicates: 10 * time.Minute, Discard: jetstream.DiscardOld,
		},
		{
			Name: StreamOrders, Subjects: []string{SubjOrder},
			Storage: jetstream.FileStorage, MaxAge: 30 * 24 * time.Hour,
			Duplicates: 10 * time.Minute,
		},
		{
			Name: StreamAlerts, Subjects: []string{SubjAlerts},
			Storage: jetstream.FileStorage, MaxAge: 30 * 24 * time.Hour,
		},
	}
}

// Buckets returns the KV bucket configurations.
func Buckets() []jetstream.KeyValueConfig {
	return []jetstream.KeyValueConfig{
		{Bucket: KVLast, Description: "latest tick per contract", History: 1, Storage: jetstream.MemoryStorage},
		// The session entry outlives a calendar day: Kite's token expires around
		// 06:00 the next morning (verify), and the evening GIFT Nifty watch needs it.
		{Bucket: KVSession, Description: "Kite access token", History: 5, TTL: 24 * time.Hour, Storage: jetstream.FileStorage},
	}
}

// Ensure creates or updates every stream and bucket. Safe to call from every binary.
func (c *Conn) Ensure(ctx context.Context) error {
	for _, sc := range Streams() {
		if _, err := c.JS.CreateOrUpdateStream(ctx, sc); err != nil {
			return fmt.Errorf("bus: stream %s: %w", sc.Name, err)
		}
	}
	for _, kc := range Buckets() {
		if _, err := c.JS.CreateOrUpdateKeyValue(ctx, kc); err != nil {
			return fmt.Errorf("bus: bucket %s: %w", kc.Bucket, err)
		}
	}
	return nil
}

// KV opens a bucket that Ensure created.
func (c *Conn) KV(ctx context.Context, bucket string) (jetstream.KeyValue, error) {
	return c.JS.KeyValue(ctx, bucket)
}

// PublishChain puts one snapshot on the CHAIN stream, deduplicated by source,
// underlying, expiry and minute so a retried publish is stored once.
func PublishChain(ctx context.Context, js jetstream.JetStream, c Chain) error {
	exp, err := time.Parse("2006-01-02", c.Expiry)
	if err != nil {
		return fmt.Errorf("bus: chain expiry %q: %w", c.Expiry, err)
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = js.Publish(ctx, ChainSubject(c.Und, exp), b, jetstream.WithMsgID(ChainMsgID(c.Source, c.Und, exp, FromMs(c.TS))))
	return err
}
