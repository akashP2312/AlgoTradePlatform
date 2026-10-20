package ticker

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"pmt/internal/bus"
	"pmt/internal/contract"
	"pmt/internal/refdata"
)

// TestPruneLast runs against a JetStream-enabled server: PMT_TEST_NATS=nats://127.0.0.1:54222.
// It uses a scratch bucket, not lastvalue.
func TestPruneLast(t *testing.T) {
	url := os.Getenv("PMT_TEST_NATS")
	if url == "" {
		t.Skip("PMT_TEST_NATS not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := bus.Connect(url, "ticker-prune-test")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	name := fmt.Sprintf("prune_test_%d", time.Now().UnixNano())
	kv, err := c.JS.CreateKeyValue(ctx, jetstream.KeyValueConfig{Bucket: name, Storage: jetstream.MemoryStorage})
	if err != nil {
		t.Fatal(err)
	}
	defer c.JS.DeleteKeyValue(context.Background(), name)

	key := func(s string) contract.Key {
		k, err := contract.Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	live := []refdata.Inst{
		{Key: key("NSE|NIFTY|||IDX")},
		{Key: key("NFO|NIFTY|2026-11-09|25000|CE")},
	}
	gone := key("NFO|NIFTY|2026-11-10|25000|CE") // a holiday expiry the dump no longer lists
	for _, in := range append(live, refdata.Inst{Key: gone}) {
		if _, err := kv.Put(ctx, in.Key.Subject(), []byte("{}")); err != nil {
			t.Fatal(err)
		}
	}

	s := &Service{lkv: kv}
	s.pruneLast(ctx, live)
	l, err := kv.ListKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for k := range l.Keys() {
		got = append(got, k)
	}
	slices.Sort(got)
	want := []string{live[0].Key.Subject(), live[1].Key.Subject()}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("keys after prune = %v, want %v", got, want)
	}
}
