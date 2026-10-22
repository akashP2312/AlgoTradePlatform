package newsmon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"pmt/internal/bus"
	"pmt/internal/clock"
	"pmt/internal/config"
)

const (
	maxFeedBytes  = 5 << 20
	maxRetryAfter = time.Hour
)

// poller polls one source. Failures (a network error, a status other than 2xx
// or 304, a body that is not a feed) back off exponentially; Fails of them in a
// row open the breaker, which waits Cool and then lets one trial poll through
// (half_open). The trial's success closes it, its failure reopens it.
type poller struct {
	src    config.Source
	loc    *time.Location
	cfg    config.Feeds
	client *http.Client
	robots *Robots
	in     *Ingest

	etag, lastMod string
	primed        bool // a poll succeeded: later entries are new, not backfill
	fails         int
	state         string
}

func newPoller(src config.Source, cfg config.Feeds, client *http.Client, robots *Robots, in *Ingest) (*poller, error) {
	loc, err := time.LoadLocation(src.TZ)
	if err != nil {
		return nil, err
	}
	return &poller{src: src, loc: loc, cfg: cfg, client: client, robots: robots, in: in, state: "starting"}, nil
}

// run polls until ctx ends, after a jitter so sources do not all fire at once.
func (p *poller) run(ctx context.Context) {
	wait := time.Duration(rand.Int64N(int64(min(p.cfg.Poll, 10*time.Second))))
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = p.poll(ctx)
	}
}

// poll makes one attempt and returns how long to wait before the next.
func (p *poller) poll(ctx context.Context) time.Duration {
	if p.state == "open" {
		p.state = "half_open"
	}
	start := time.Now()
	now := clock.Now()
	u, _ := url.Parse(p.src.URL)
	if !p.robots.Allowed(ctx, u, start) {
		p.state = "blocked"
		wait := p.cfg.Cool
		p.in.update(p.src.ID, func(st *SourceStatus) {
			st.State, st.Err, st.LastTry = p.state, "robots.txt disallows this feed or could not be read", bus.Ms(now)
			st.Next = bus.Ms(now.Add(wait))
		})
		slog.Warn("newsmon: feed blocked by robots.txt", "source", p.src.ID)
		return wait
	}

	raws, status, retryAfter, err := p.fetch(ctx)
	ms := time.Since(start).Milliseconds()
	if err != nil {
		if ctx.Err() != nil {
			return 0
		}
		p.fails++
		var wait time.Duration
		if p.state == "half_open" || p.fails >= p.cfg.Fails {
			p.state, wait = "open", p.cfg.Cool
		} else {
			p.state, wait = "backoff", backoff(p.cfg.Poll, p.cfg.BackoffMax, p.fails)
		}
		wait = max(wait, retryAfter)
		p.in.update(p.src.ID, func(st *SourceStatus) {
			st.State, st.Err, st.Fails, st.LastTry, st.Polls = p.state, err.Error(), p.fails, bus.Ms(now), st.Polls+1
			st.Next = bus.Ms(now.Add(wait))
		})
		slog.Warn("newsmon: feed poll failed", "source", p.src.ID, "status", status, "fails", p.fails, "state", p.state, "retry_in", wait.Round(time.Second), "ms", ms, "err", err)
		return wait
	}

	backfill := !p.primed
	n := p.in.Add(ctx, p.src, p.loc, raws, backfill, now)
	p.primed, p.fails, p.state = true, 0, "ok"
	wait := jitter(p.cfg.Poll, 0.05)
	p.in.update(p.src.ID, func(st *SourceStatus) {
		st.State, st.Err, st.Fails, st.LastTry, st.LastOK, st.Polls = "ok", "", 0, bus.Ms(now), bus.Ms(now), st.Polls+1
		st.Next = bus.Ms(now.Add(wait))
	})
	lvl := slog.LevelDebug
	if n > 0 || backfill {
		lvl = slog.LevelInfo
	}
	slog.Log(ctx, lvl, "newsmon: feed polled", "source", p.src.ID, "status", status, "items", len(raws), "new", n, "backfill", backfill, "ms", ms)
	return wait
}

// fetch makes a conditional GET. A 304 is success with no entries.
func (p *poller) fetch(ctx context.Context) (raws []RawItem, status int, retryAfter time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.src.URL, nil)
	if err != nil {
		return nil, 0, 0, err
	}
	req.Header.Set("User-Agent", p.cfg.UserAgent)
	req.Header.Set("Accept", "application/rss+xml, application/atom+xml, application/xml;q=0.9, text/xml;q=0.9, */*;q=0.5")
	if p.etag != "" {
		req.Header.Set("If-None-Match", p.etag)
	}
	if p.lastMod != "" {
		req.Header.Set("If-Modified-Since", p.lastMod)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, 0, 0, err
	}
	defer resp.Body.Close()
	status = resp.StatusCode
	switch {
	case status == http.StatusNotModified:
		return nil, status, 0, nil
	case status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable:
		return nil, status, parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()), errStatus(status)
	case status < 200 || status >= 300:
		return nil, status, 0, errStatus(status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes+1))
	if err != nil {
		return nil, status, 0, err
	}
	if len(b) > maxFeedBytes {
		return nil, status, 0, fmt.Errorf("feed over %d MB", maxFeedBytes>>20)
	}
	raws, err = ParseFeed(b)
	if err != nil {
		return nil, status, 0, err
	}
	p.etag, p.lastMod = resp.Header.Get("ETag"), resp.Header.Get("Last-Modified")
	return raws, status, 0, nil
}

// backoff is the wait after the n-th failure in a row: poll·2^(n−1), at most
// max, ±20%.
func backoff(poll, maxWait time.Duration, n int) time.Duration {
	d := poll
	for i := 1; i < n && d < maxWait; i++ {
		d *= 2
	}
	return jitter(min(d, maxWait), 0.2)
}

func jitter(d time.Duration, f float64) time.Duration {
	return time.Duration(float64(d) * (1 - f + 2*f*rand.Float64()))
}

// parseRetryAfter reads seconds or an HTTP date, capped at maxRetryAfter.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	var d time.Duration
	if s, err := strconv.Atoi(v); err == nil {
		d = time.Duration(s) * time.Second
	} else if t, err := http.ParseTime(v); err == nil {
		d = t.Sub(now)
	}
	return min(max(d, 0), maxRetryAfter)
}

// StartFeeds starts a poller per enabled RSS source. The paid source is a stub:
// listed, never polled.
func StartFeeds(ctx context.Context, cfg config.Feeds, in *Ingest) error {
	client := &http.Client{Timeout: cfg.Timeout}
	token, _, _ := strings.Cut(cfg.UserAgent, "/")
	robots := &Robots{Client: client, UA: cfg.UserAgent, Token: token, Refresh: cfg.Robots, Retry: cfg.Cool}
	var errs []error
	for _, s := range cfg.Sources {
		if !s.Enabled || s.Kind != "rss" {
			continue
		}
		p, err := newPoller(s, cfg, client, robots, in)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", s.ID, err))
			continue
		}
		go p.run(ctx)
	}
	return errors.Join(errs...)
}
