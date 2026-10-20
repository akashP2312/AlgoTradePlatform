package recorder

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"pmt/internal/bus"
	"pmt/internal/config"
)

// TestRunEndToEnd publishes a tick, a chain and an order on JetStream and runs
// the recorder until all three are in Postgres. The streams are shared with
// other test packages, so the stored offsets are first moved to each stream's
// last sequence: the recorder skips everything older and records only ours.
func TestRunEndToEnd(t *testing.T) {
	url := os.Getenv("PMT_TEST_NATS")
	if url == "" {
		t.Skip("PMT_TEST_NATS not set")
	}
	ctx, pool := testDB(t)
	conn, err := bus.Connect(url, "pmt-recorder-test")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	cleaner(t, ctx, pool,
		`DELETE FROM ticks WHERE contract = '`+rcOpt+`'`,
		`DELETE FROM bars_1m WHERE contract = '`+rcOpt+`'`,
		`DELETE FROM chain_snapshots WHERE underlying = 'RCTEST'`,
		`DELETE FROM orders WHERE order_id LIKE 'RCTEST-%'`,
	)

	streams := map[string]string{
		ConsumerTicks: bus.StreamTicks, ConsumerChain: bus.StreamChain, ConsumerOrders: bus.StreamOrders,
	}
	for cons, name := range streams {
		if err := conn.JS.DeleteConsumer(ctx, name, cons); err != nil && !errors.Is(err, jetstream.ErrConsumerNotFound) {
			t.Fatal(err)
		}
		if err := SaveOffset(ctx, pool, cons, lastSeq(t, ctx, conn.JS, name)); err != nil {
			t.Fatal(err)
		}
	}

	// A first tick after the opening minute makes a bar only when it traded in
	// the minute it was received, so it carries a trade time.
	now := time.Now()
	tick, _ := json.Marshal(bus.Tick{C: rcOpt, T: 9201, Mode: "f", Recv: now.UnixMilli(), LTT: now.UnixMilli(), LTP: 101, Vol: 10, Phase: "c"})
	if _, err := conn.JS.Publish(ctx, "tick.nfo.rctest.20261027.25000.ce", tick); err != nil {
		t.Fatal(err)
	}
	// The CHAIN stream deduplicates by minute for ten minutes, so each run uses
	// a minute of its own.
	minute := time.Date(2026, 10, 19, 4, 0, 0, 0, time.UTC).Add(time.Duration(now.UnixNano()%100000) * time.Minute)
	chain := bus.Chain{TS: minute.UnixMilli(), Source: "ws", Und: "RCTEST", Expiry: "2026-10-27",
		Rows: []bus.ChainRow{{C: rcOpt, T: 9201, Strike: 25000, Type: "CE", LTP: 100}}}
	if err := bus.PublishChain(ctx, conn.JS, chain); err != nil {
		t.Fatal(err)
	}
	order := `{"order_id":"RCTEST-E2E","status":"OPEN","quantity":75,"order_timestamp":"2026-10-19 09:20:00"}`
	if _, err := conn.JS.Publish(ctx, bus.SubjOrder, []byte(order)); err != nil {
		t.Fatal(err)
	}

	rctx, cancel := context.WithCancel(ctx)
	rec := &Recorder{Pool: pool, JS: conn.JS, Cfg: config.Recorder{BatchRows: 100, FlushEvery: 200 * time.Millisecond}}
	done := make(chan error, 1)
	go func() { done <- rec.Run(rctx) }()

	deadline := time.Now().Add(20 * time.Second)
	for {
		tk := count(t, ctx, pool, `SELECT count(*) FROM ticks WHERE contract = '`+rcOpt+`'`)
		ch := count(t, ctx, pool, `SELECT count(*) FROM chain_snapshots WHERE underlying = 'RCTEST'`)
		od := count(t, ctx, pool, `SELECT count(*) FROM orders WHERE order_id = 'RCTEST-E2E'`)
		if tk == 1 && ch == 1 && od == 1 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("after 20s: ticks %d, chain %d, orders %d; %v", tk, ch, od, rec.Detail())
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM bars_1m WHERE contract = '`+rcOpt+`' AND source = 'live'`); n != 1 {
		t.Errorf("%d live bars", n)
	}
	// Each offset reached at least the message this test published.
	for cons, name := range streams {
		got, err := LoadOffset(ctx, pool, cons)
		if err != nil {
			t.Fatal(err)
		}
		if got == 0 || got > lastSeq(t, ctx, conn.JS, name) {
			t.Errorf("%s offset %d", cons, got)
		}
	}
	if rec.Ticks.Rows.Load() < 1 || rec.Chain.Rows.Load() < 1 || rec.Orders.Rows.Load() < 1 {
		t.Errorf("counters %v", rec.Detail())
	}
}

func lastSeq(t *testing.T, ctx context.Context, js jetstream.JetStream, name string) uint64 {
	t.Helper()
	s, err := js.Stream(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return info.State.LastSeq
}
