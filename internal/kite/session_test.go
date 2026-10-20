package kite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pmt/internal/bus"
	"pmt/internal/clock"
)

func TestSessionExpiry(t *testing.T) {
	cases := map[string]string{
		"2026-10-19T08:15:00+05:30": "2026-10-20T06:00:00+05:30",
		"2026-10-19T23:59:00+05:30": "2026-10-20T06:00:00+05:30",
		"2026-10-20T01:00:00+05:30": "2026-10-20T06:00:00+05:30",
		"2026-10-20T06:00:00+05:30": "2026-10-21T06:00:00+05:30",
	}
	for in, want := range cases {
		ti, _ := time.Parse(time.RFC3339, in)
		w, _ := time.Parse(time.RFC3339, want)
		if got := SessionExpiry(ti); !got.Equal(w) {
			t.Errorf("SessionExpiry(%s) = %s want %s", in, got, want)
		}
	}
}

func TestTokenFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "kite_token.json")
	if _, err := ReadTokenFile(p); !errors.Is(err, ErrNoSession) {
		t.Fatalf("missing file: %v", err)
	}
	now := clock.Now()
	s := bus.Session{AccessToken: "abc", APIKey: "key", UserID: "AB1234", IssuedAt: now, ExpiresAt: SessionExpiry(now)}
	if err := WriteTokenFile(p, s); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", fi.Mode().Perm())
	}
	got, err := ReadTokenFile(p)
	if err != nil || got.AccessToken != "abc" || !got.ExpiresAt.Equal(s.ExpiresAt) {
		t.Fatalf("read back %v %v", got, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(p))
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %d entries", len(entries))
	}
	s.ExpiresAt = now.Add(-time.Minute)
	if err := WriteTokenFile(p, s); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTokenFile(p); !errors.Is(err, ErrNoSession) {
		t.Fatalf("expired token accepted: %v", err)
	}
}

func TestSessionKV(t *testing.T) {
	url := os.Getenv("PMT_TEST_NATS")
	if url == "" {
		t.Skip("PMT_TEST_NATS not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := bus.Connect(url, "kite-session-test")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	kv, err := c.KV(ctx, bus.KVSession)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := WatchSession(ctx, kv)
	if err != nil {
		t.Fatal(err)
	}
	now := clock.Now()
	s := bus.Session{AccessToken: "tok-" + now.Format("150405.000"), APIKey: "key", IssuedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := SaveSession(ctx, kv, s); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSession(ctx, kv)
	if err != nil || got.AccessToken != s.AccessToken {
		t.Fatalf("load %v %v", got, err)
	}
	for {
		select {
		case w := <-ch:
			if w.AccessToken == s.AccessToken {
				return
			}
		case <-ctx.Done():
			t.Fatal("watch never delivered the new session")
		}
	}
}
