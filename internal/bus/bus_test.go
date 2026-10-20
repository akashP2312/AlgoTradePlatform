package bus

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

func TestWireRoundTrip(t *testing.T) {
	in := Tick{C: "NFO|NIFTY|2026-10-27|25000|CE", T: 12345, Mode: "f", Recv: 1, LTP: 101.5, Phase: "c",
		Depth: &Depth{Buy: [5]Level{{P: 101, Q: 75, N: 2}}}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Tick
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.C != in.C || out.LTP != in.LTP || out.Depth == nil || out.Depth.Buy[0].Q != 75 {
		t.Fatalf("round trip lost data: %s", b)
	}
	exp := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	if got := ChainSubject("NIFTY", exp); got != "chain.nifty.20261027" {
		t.Fatal(got)
	}
	s := Session{AccessToken: "secret-token", UserID: "AB1234", ExpiresAt: time.Now().Add(time.Hour)}
	if !s.Valid(time.Now()) || containsStr(s.String(), "secret") {
		t.Fatal("session validity or redaction wrong")
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestEnsure runs against a JetStream-enabled server: PMT_TEST_NATS=nats://127.0.0.1:54222.
func TestEnsure(t *testing.T) {
	url := os.Getenv("PMT_TEST_NATS")
	if url == "" {
		t.Skip("PMT_TEST_NATS not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := Connect(url, "pmt-test")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < 2; i++ { // idempotent
		if err := c.Ensure(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// A core-NATS tick publish lands in the TICKS stream.
	before := streamMsgs(t, ctx, c, StreamTicks)
	if err := c.NC.Publish("tick.nfo.nifty.20261027.25000.ce", []byte(`{"c":"x"}`)); err != nil {
		t.Fatal(err)
	}
	_ = c.NC.Flush()
	deadline := time.Now().Add(3 * time.Second)
	for streamMsgs(t, ctx, c, StreamTicks) <= before {
		if time.Now().After(deadline) {
			t.Fatal("tick not captured by TICKS stream")
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Chain dedupe: the same message ID twice stores one message.
	id := ChainMsgID("ws", "NIFTY", time.Now(), time.Now())
	for i := 0; i < 2; i++ {
		ack, err := c.JS.Publish(ctx, "chain.nifty.20261027", []byte(`{}`), jetstream.WithMsgID(id))
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 && !ack.Duplicate {
			t.Fatal("second chain publish was not flagged duplicate")
		}
	}
	kv, err := c.KV(ctx, KVSession)
	if err != nil {
		t.Fatal(err)
	}
	st, err := kv.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.TTL() != 24*time.Hour || st.History() != 5 {
		t.Fatalf("kite_session bucket ttl=%s history=%d", st.TTL(), st.History())
	}
}

func streamMsgs(t *testing.T, ctx context.Context, c *Conn, name string) uint64 {
	t.Helper()
	s, err := c.JS.Stream(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return info.State.Msgs
}
