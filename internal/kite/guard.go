// Package kite wraps Kite Connect for the terminal: a read-only, rate-limited HTTP
// transport, the REST client built on it, the instrument dump, the shared login
// session, and the WebSocket ticker.
package kite

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Class is a Kite rate-limit bucket.
type Class string

const (
	ClassQuote      Class = "quote"
	ClassHistorical Class = "historical"
	ClassOther      Class = "other"
)

// ErrRefused is returned (wrapped) for any request the read-only allowlist rejects.
// The terminal must never place, modify or cancel orders, and never log out: the
// Python bot owns the order path and shares this login.
var ErrRefused = errors.New("kite guard: refused (read-only terminal)")

// Classify reports the rate-limit class of an allowed request, or ok=false when the
// request is refused. p is the API path without the base URL's own path.
func Classify(method, p string) (Class, bool) {
	p = path.Clean("/" + strings.TrimPrefix(p, "/"))
	seg := strings.Split(strings.TrimPrefix(p, "/"), "/")
	switch method {
	case http.MethodGet:
		switch {
		case p == "/quote" || p == "/quote/ltp" || p == "/quote/ohlc":
			return ClassQuote, true
		case len(seg) == 4 && seg[0] == "instruments" && seg[1] == "historical" && seg[2] != "" && seg[3] != "":
			return ClassHistorical, true
		case p == "/instruments" || (len(seg) == 2 && seg[0] == "instruments" && seg[1] != "historical"):
			return ClassOther, true
		case p == "/user/profile" || p == "/user/margins" || (len(seg) == 3 && seg[0] == "user" && seg[1] == "margins"):
			return ClassOther, true
		case p == "/portfolio/positions" || p == "/portfolio/holdings":
			return ClassOther, true
		case p == "/orders" || p == "/trades":
			return ClassOther, true
		}
	case http.MethodPost:
		if p == "/session/token" { // request-token exchange during the morning login only
			return ClassOther, true
		}
	}
	return "", false
}

// pacer spaces requests at a fixed interval; it never bursts, which keeps us clear
// of Kite's per-second limits even when several goroutines share a class.
type pacer struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
}

func newPacer(perSec int) *pacer {
	if perSec <= 0 {
		perSec = 1
	}
	// 5% headroom: Kite counts on its own clock, and a request landing exactly on the
	// boundary is the one that draws a 429.
	return &pacer{interval: time.Second * 105 / 100 / time.Duration(perSec)}
}

func (p *pacer) wait(ctx context.Context) error {
	p.mu.Lock()
	now := time.Now()
	at := p.next
	if at.Before(now) {
		at = now
	}
	p.next = at.Add(p.interval)
	p.mu.Unlock()
	d := time.Until(at)
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Limits are requests per second per class.
type Limits struct {
	Quote, Historical, Other int
}

// Guard is an http.RoundTripper that enforces the allowlist and the rate limits
// and retries GETs that draw a 429.
type Guard struct {
	next     http.RoundTripper
	basePath string
	pacers   map[Class]*pacer
	retries  int
	backoff  time.Duration

	requests  atomic.Int64
	refused   atomic.Int64
	throttled atomic.Int64
}

// NewGuard wraps next (http.DefaultTransport when nil). baseURL is Kite's root URL;
// its path, if any, is stripped before the allowlist check.
func NewGuard(next http.RoundTripper, baseURL string, l Limits) *Guard {
	if next == nil {
		next = http.DefaultTransport
	}
	bp := ""
	if i := strings.Index(baseURL, "://"); i >= 0 {
		rest := baseURL[i+3:]
		if j := strings.IndexByte(rest, '/'); j >= 0 {
			bp = strings.TrimRight(rest[j:], "/")
		}
	}
	return &Guard{
		next: next, basePath: bp, retries: 3, backoff: time.Second,
		pacers: map[Class]*pacer{
			ClassQuote:      newPacer(l.Quote),
			ClassHistorical: newPacer(l.Historical),
			ClassOther:      newPacer(l.Other),
		},
	}
}

// GuardStats are counters for the SYS page.
type GuardStats struct{ Requests, Refused, Throttled int64 }

func (g *Guard) Stats() GuardStats {
	return GuardStats{g.requests.Load(), g.refused.Load(), g.throttled.Load()}
}

func (g *Guard) RoundTrip(req *http.Request) (*http.Response, error) {
	p := strings.TrimPrefix(req.URL.Path, g.basePath)
	class, ok := Classify(req.Method, p)
	if !ok {
		g.refused.Add(1)
		slog.Error("kite guard refused request", "method", req.Method, "path", p)
		return nil, fmt.Errorf("%w: %s %s", ErrRefused, req.Method, p)
	}
	pc := g.pacers[class]
	for attempt := 0; ; attempt++ {
		if err := pc.wait(req.Context()); err != nil {
			return nil, err
		}
		g.requests.Add(1)
		resp, err := g.next.RoundTrip(req)
		if err != nil || resp.StatusCode != http.StatusTooManyRequests || req.Method != http.MethodGet || attempt >= g.retries {
			if err == nil && resp.StatusCode == http.StatusTooManyRequests {
				g.throttled.Add(1)
			}
			return resp, err
		}
		g.throttled.Add(1)
		wait := retryAfter(resp, g.backoff<<attempt)
		resp.Body.Close()
		slog.Warn("kite 429, retrying", "class", class, "path", p, "attempt", attempt+1, "wait", wait)
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-req.Context().Done():
			t.Stop()
			return nil, req.Context().Err()
		}
	}
}

// retryAfter honours a Retry-After header in seconds, else jitters base by ±50%.
func retryAfter(resp *http.Response, base time.Duration) time.Duration {
	if s := resp.Header.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 && n < 60 {
			return time.Duration(n) * time.Second
		}
	}
	return base/2 + time.Duration(rand.Int64N(int64(base)))
}
