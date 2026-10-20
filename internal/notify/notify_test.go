package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestTelegramAndDedupe(t *testing.T) {
	var hits atomic.Int64
	var lastText atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/botT0K/sendMessage" {
			t.Errorf("path %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		v, _ := url.ParseQuery(string(b))
		if v.Get("chat_id") != "42" {
			t.Errorf("chat_id %q", v.Get("chat_id"))
		}
		lastText.Store(v.Get("text"))
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	n := New("ticker", "T0K", "42")
	n.api = srv.URL
	ctx := context.Background()
	n.Send(ctx, Warn, "feed_stale", "conn %d silent for %s", 1, 10*time.Second)
	n.Send(ctx, Warn, "feed_stale", "conn %d silent for %s", 1, 10*time.Second) // deduped
	n.Send(ctx, Crit, "feed_stale", "conn %d silent for %s", 2, 10*time.Second)
	if hits.Load() != 2 {
		t.Fatalf("hits = %d, want 2", hits.Load())
	}
	if s := lastText.Load().(string); !strings.Contains(s, "CRIT · ticker") || !strings.Contains(s, "conn 2 silent") {
		t.Fatalf("text = %q", s)
	}
}

func TestRedactsToken(t *testing.T) {
	n := New("jobs", "SECRET-TOKEN", "42")
	n.api = "http://127.0.0.1:1" // nothing listens
	err := n.telegram(context.Background(), "x")
	if err == nil || strings.Contains(err.Error(), "SECRET-TOKEN") {
		t.Fatalf("err = %v", err)
	}
}

func TestLogOnly(t *testing.T) {
	var n *Notifier
	n.Send(context.Background(), Info, "x", "nil notifier is a no-op")
	m := New("sys", "", "")
	if m.Enabled() {
		t.Fatal("enabled without credentials")
	}
	m.Send(context.Background(), Info, "x", "logs only")
}
