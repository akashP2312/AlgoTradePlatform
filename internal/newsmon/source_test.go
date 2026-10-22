package newsmon

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"pmt/internal/config"
)

func TestParseRobots(t *testing.T) {
	body := `# comment
User-agent: Googlebot
Disallow: /

User-agent: pmt-news
User-agent: other
Disallow: /private/
Allow: /private/feed.xml
Disallow: /*.pdf$
Disallow: /search?

User-agent: *
Disallow: /rss/
`
	mine := ParseRobots(body, "pmt-news")
	for p, want := range map[string]bool{
		"/rss/markets.xml":  true, // the * group does not apply when ours exists
		"/private/x":        false,
		"/private/feed.xml": true,
		"/doc.pdf":          false,
		"/doc.pdf?x=1":      true,
		"/search?q=sensex":  false,
		"/":                 true,
	} {
		if got := robotsAllow(mine, p); got != want {
			t.Errorf("pmt-news %s = %v, want %v", p, got, want)
		}
	}
	star := ParseRobots(body, "somebot")
	if robotsAllow(star, "/rss/markets.xml") || !robotsAllow(star, "/private/x") {
		t.Error("star group fallback")
	}
	// A tie between Allow and Disallow goes to Allow.
	tie := ParseRobots("User-agent: *\nDisallow: /a\nAllow: /a\n", "pmt-news")
	if !robotsAllow(tie, "/a") {
		t.Error("tie not allowed")
	}
	// An empty Disallow allows everything; an HTML page has no rules.
	if !robotsAllow(ParseRobots("User-agent: *\nDisallow:\n", "pmt-news"), "/x") ||
		!robotsAllow(ParseRobots("<html><body>Not found</body></html>", "pmt-news"), "/x") {
		t.Error("empty rules disallow")
	}
}

func TestRobotsFetch(t *testing.T) {
	var mu sync.Mutex
	status, hits := 500, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		hits++
		w.WriteHeader(status)
		if status == 200 {
			fmt.Fprint(w, "User-agent: *\nDisallow: /no/\n")
		}
	}))
	defer srv.Close()
	r := &Robots{Client: &http.Client{Timeout: 2 * time.Second}, UA: "pmt-news/0.1", Token: "pmt-news", Refresh: time.Hour, Retry: 10 * time.Minute}
	ctx, now := context.Background(), time.Now()
	u, _ := url.Parse(srv.URL + "/feed.xml")
	no, _ := url.Parse(srv.URL + "/no/feed.xml")

	if r.Allowed(ctx, u, now) {
		t.Fatal("unreadable robots.txt allowed")
	}
	mu.Lock()
	status = 200
	mu.Unlock()
	if r.Allowed(ctx, u, now.Add(time.Minute)) || hits != 1 {
		t.Fatalf("rechecked before Retry: hits %d", hits)
	}
	if !r.Allowed(ctx, u, now.Add(11*time.Minute)) || r.Allowed(ctx, no, now.Add(11*time.Minute)) || hits != 2 {
		t.Fatalf("after retry: hits %d", hits)
	}
	mu.Lock()
	status = 404
	mu.Unlock()
	if !r.Allowed(ctx, no, now.Add(72*time.Minute)) || hits != 3 {
		t.Fatalf("missing robots.txt after refresh: hits %d", hits)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 21, 4, 0, 0, 0, time.UTC)
	for v, want := range map[string]time.Duration{
		"":                              0,
		"120":                           2 * time.Minute,
		" 30 ":                          30 * time.Second,
		"-5":                            0,
		"99999":                         time.Hour,
		"Wed, 21 Oct 2026 04:05:00 GMT": 5 * time.Minute,
		"Wed, 21 Oct 2026 03:00:00 GMT": 0,
		"soon":                          0,
	} {
		if got := parseRetryAfter(v, now); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestBackoff(t *testing.T) {
	for n, base := range map[int]time.Duration{1: time.Minute, 2: 2 * time.Minute, 3: 4 * time.Minute, 4: 8 * time.Minute, 5: 10 * time.Minute, 30: 10 * time.Minute} {
		for range 20 {
			if d := backoff(time.Minute, 10*time.Minute, n); d < base*8/10 || d > base*12/10 {
				t.Fatalf("backoff(n=%d) = %v, want %v ±20%%", n, d, base)
			}
		}
	}
}

// feedServer serves a scripted sequence of responses and records requests.
type feedServer struct {
	mu      sync.Mutex
	steps   []func(w http.ResponseWriter, r *http.Request)
	reqs    []*http.Request
	robots  func(w http.ResponseWriter)
	feedHit int
}

func (f *feedServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/robots.txt" {
		if f.robots != nil {
			f.robots(w)
		} else {
			w.WriteHeader(404)
		}
		return
	}
	f.reqs = append(f.reqs, r)
	step := f.steps[min(f.feedHit, len(f.steps)-1)]
	f.feedHit++
	step(w, r)
}

func rssBody(items ...string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><rss version="2.0"><channel><title>t</title>`)
	for _, it := range items {
		b.WriteString(it)
	}
	b.WriteString(`</channel></rss>`)
	return b.String()
}

func rssItemAt(title, link string, at time.Time) string {
	return fmt.Sprintf(`<item><title>%s</title><link>%s</link><pubDate>%s</pubDate></item>`, title, link, at.Format(time.RFC1123Z))
}

func testPoller(t *testing.T, srv *httptest.Server) (*poller, *Ingest) {
	t.Helper()
	cfg := testFeeds()
	src := config.Source{ID: "t", Name: "Test", URL: srv.URL + "/rss.xml", Kind: "rss", Trust: "media", Region: "IN", Enabled: true, TZ: "Asia/Kolkata"}
	cfg.Sources = []config.Source{src}
	in := NewIngest(cfg, nil)
	client := &http.Client{Timeout: cfg.Timeout}
	robots := &Robots{Client: client, UA: cfg.UserAgent, Token: "pmt-news", Refresh: cfg.Robots, Retry: cfg.Cool}
	p, err := newPoller(src, cfg, client, robots, in)
	if err != nil {
		t.Fatal(err)
	}
	return p, in
}

func status(in *Ingest) SourceStatus {
	f, _ := in.Snapshot(time.Now())
	return f.Sources[0]
}

func within(t *testing.T, what string, d, lo, hi time.Duration) {
	t.Helper()
	if d < lo || d > hi {
		t.Errorf("%s: wait %v, want %v–%v", what, d, lo, hi)
	}
}

func TestPollerStates(t *testing.T) {
	now := time.Now()
	first := rssBody(rssItemAt("Sensex opens higher as banks gain in early trade", "https://x.test/1", now.Add(-30*time.Minute)))
	fs := &feedServer{}
	fs.steps = []func(http.ResponseWriter, *http.Request){
		func(w http.ResponseWriter, r *http.Request) { w.Header().Set("ETag", `"v1"`); fmt.Fprint(w, first) },
		func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("If-None-Match") == `"v1"` {
				w.WriteHeader(304)
				return
			}
			fmt.Fprint(w, first)
		},
		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) },
		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) },
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "1200")
			w.WriteHeader(503)
		},
		func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) },
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("ETag", `"v2"`)
			fmt.Fprint(w, rssBody(rssItemAt("RBI announces variable rate repo auction for Friday", "https://x.test/2", time.Now().Add(-2*time.Minute)), first[strings.Index(first, "<item>"):strings.Index(first, "</channel>")]))
		},
	}
	srv := httptest.NewServer(fs)
	defer srv.Close()
	p, in := testPoller(t, srv)
	ctx := context.Background()

	within(t, "first poll", p.poll(ctx), 57*time.Second, 63*time.Second)
	if st := status(in); st.State != "ok" || st.New != 0 || st.Polls != 1 {
		t.Errorf("after backfill: %+v", st)
	}
	if f, _ := in.Snapshot(time.Now()); len(f.Items) != 1 || f.Items[0].LagMs != 0 {
		t.Errorf("backfill items: %+v", f.Items)
	}
	within(t, "304", p.poll(ctx), 57*time.Second, 63*time.Second)
	within(t, "first failure", p.poll(ctx), 48*time.Second, 72*time.Second)
	if st := status(in); st.State != "backoff" || st.Fails != 1 || !strings.Contains(st.Err, "Internal Server Error") {
		t.Errorf("after 500: %+v", st)
	}
	within(t, "second failure", p.poll(ctx), 96*time.Second, 144*time.Second)
	// The third failure opens the breaker; Retry-After outlasts Cool.
	within(t, "503 retry-after", p.poll(ctx), 20*time.Minute, 20*time.Minute)
	if st := status(in); st.State != "open" || st.Fails != 3 {
		t.Errorf("after 503: %+v", st)
	}
	// The trial poll fails: open again for Cool.
	within(t, "failed trial", p.poll(ctx), 15*time.Minute, 15*time.Minute)
	if p.state != "open" {
		t.Errorf("state after failed trial = %s", p.state)
	}
	within(t, "recovered", p.poll(ctx), 57*time.Second, 63*time.Second)
	st := status(in)
	if st.State != "ok" || st.Fails != 0 || st.Err != "" || st.New != 1 || st.Polls != 7 {
		t.Errorf("after recovery: %+v", st)
	}
	if st.LagMed < (2*time.Minute).Milliseconds()-1000 || st.LagMed > (2*time.Minute).Milliseconds()+5000 {
		t.Errorf("lag %dms, want about 2m", st.LagMed)
	}
	if f, _ := in.Snapshot(time.Now()); len(f.Items) != 2 || !strings.HasPrefix(f.Items[0].Title, "RBI") {
		t.Errorf("items after recovery: %+v", f.Items)
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.reqs) != 7 {
		t.Fatalf("%d feed requests", len(fs.reqs))
	}
	for _, r := range fs.reqs {
		if r.Header.Get("User-Agent") != "pmt-news/0.1 (+test)" {
			t.Fatalf("user agent %q", r.Header.Get("User-Agent"))
		}
	}
	if fs.reqs[0].Header.Get("If-None-Match") != "" || fs.reqs[1].Header.Get("If-None-Match") != `"v1"` {
		t.Error("conditional GET headers")
	}
}

func TestPollerNotAFeed(t *testing.T) {
	fs := &feedServer{steps: []func(http.ResponseWriter, *http.Request){
		func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "<html><body>Access denied</body></html>") },
	}}
	srv := httptest.NewServer(fs)
	defer srv.Close()
	p, in := testPoller(t, srv)
	within(t, "html page", p.poll(context.Background()), 48*time.Second, 72*time.Second)
	if st := status(in); st.State != "backoff" || !strings.Contains(st.Err, "not RSS") {
		t.Errorf("html page: %+v", st)
	}
}

func TestPollerRobots(t *testing.T) {
	for name, robots := range map[string]func(w http.ResponseWriter){
		"disallow":   func(w http.ResponseWriter) { fmt.Fprint(w, "User-agent: pmt-news\nDisallow: /rss.xml\n") },
		"unreadable": func(w http.ResponseWriter) { w.WriteHeader(500) },
	} {
		t.Run(name, func(t *testing.T) {
			fs := &feedServer{robots: robots, steps: []func(http.ResponseWriter, *http.Request){
				func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, rssBody()) },
			}}
			srv := httptest.NewServer(fs)
			defer srv.Close()
			p, in := testPoller(t, srv)
			within(t, "blocked", p.poll(context.Background()), 15*time.Minute, 15*time.Minute)
			if st := status(in); st.State != "blocked" || st.Err == "" {
				t.Errorf("status: %+v", st)
			}
			fs.mu.Lock()
			defer fs.mu.Unlock()
			if fs.feedHit != 0 {
				t.Errorf("feed fetched %d times despite robots.txt", fs.feedHit)
			}
		})
	}
}
