package newsmon

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Robots caches robots.txt per host and answers whether a URL may be fetched.
// Rules come from the group naming the product token, else "*". The longest
// matching rule wins, Allow on a tie; * and $ are honored. A missing robots.txt
// (4xx) allows everything; one that cannot be read (5xx, network) disallows
// until it can, as the robots standard says.
type Robots struct {
	Client  *http.Client
	UA      string // sent as is
	Token   string // the product token matched against User-agent lines
	Refresh time.Duration
	Retry   time.Duration // recheck after an unreadable robots.txt

	mu    sync.Mutex
	hosts map[string]*robotsHost
}

type robotsHost struct {
	mu      sync.Mutex
	rules   []robotsRule
	deny    bool // unreadable: disallow everything
	fetched time.Time
	next    time.Time
}

type robotsRule struct {
	allow bool
	path  string
	re    *regexp.Regexp
}

// Allowed reports whether u may be fetched, reading robots.txt when due.
func (r *Robots) Allowed(ctx context.Context, u *url.URL, now time.Time) bool {
	key := u.Scheme + "://" + u.Host
	r.mu.Lock()
	if r.hosts == nil {
		r.hosts = map[string]*robotsHost{}
	}
	h := r.hosts[key]
	if h == nil {
		h = &robotsHost{}
		r.hosts[key] = h
	}
	r.mu.Unlock()

	h.mu.Lock()
	defer h.mu.Unlock()
	if !now.Before(h.next) {
		h.fetch(ctx, r, key, now)
	}
	if h.deny {
		return false
	}
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	return robotsAllow(h.rules, p)
}

func (h *robotsHost) fetch(ctx context.Context, r *Robots, base string, now time.Time) {
	ctx, cancel := context.WithTimeout(ctx, r.Client.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/robots.txt", nil)
	if err == nil {
		req.Header.Set("User-Agent", r.UA)
		var resp *http.Response
		resp, err = r.Client.Do(req)
		if err == nil {
			defer resp.Body.Close()
			switch {
			case resp.StatusCode >= 200 && resp.StatusCode < 300:
				b, _ := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
				h.rules, h.deny = ParseRobots(string(b), r.Token), false
			case resp.StatusCode >= 400 && resp.StatusCode < 500:
				h.rules, h.deny = nil, false
			default:
				err = errStatus(resp.StatusCode)
			}
		}
	}
	h.fetched = now
	if err != nil {
		h.deny, h.next = true, now.Add(r.Retry)
		return
	}
	h.next = now.Add(r.Refresh)
}

type errStatus int

func (e errStatus) Error() string { return "HTTP " + http.StatusText(int(e)) }

// ParseRobots returns the rules for token from a robots.txt body. A body that is
// not robots.txt (an HTML error page) has no rules, so allows everything.
func ParseRobots(body, token string) []robotsRule {
	token = strings.ToLower(token)
	type group struct {
		agents []string
		rules  []robotsRule
	}
	var groups []group
	var cur *group
	lastAgent := false
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		switch k {
		case "user-agent":
			if !lastAgent || cur == nil {
				groups = append(groups, group{})
				cur = &groups[len(groups)-1]
			}
			cur.agents = append(cur.agents, strings.ToLower(v))
			lastAgent = true
		case "allow", "disallow":
			lastAgent = false
			if cur == nil || (k == "disallow" && v == "") {
				continue
			}
			cur.rules = append(cur.rules, robotsRule{allow: k == "allow", path: v, re: robotsPattern(v)})
		default:
			lastAgent = false
		}
	}
	var mine, star []robotsRule
	found := false
	for _, g := range groups {
		for _, a := range g.agents {
			switch {
			case a == "*":
				star = append(star, g.rules...)
			case a != "" && strings.HasPrefix(token, a):
				mine, found = append(mine, g.rules...), true
			}
		}
	}
	if found {
		return mine
	}
	return star
}

func robotsPattern(p string) *regexp.Regexp {
	end := strings.HasSuffix(p, "$")
	p = strings.TrimSuffix(p, "$")
	parts := strings.Split(p, "*")
	for i := range parts {
		parts[i] = regexp.QuoteMeta(parts[i])
	}
	s := "^" + strings.Join(parts, ".*")
	if end {
		s += "$"
	}
	return regexp.MustCompile(s)
}

func robotsAllow(rules []robotsRule, path string) bool {
	best, allow := -1, true
	for _, r := range rules {
		if !r.re.MatchString(path) {
			continue
		}
		if n := len(r.path); n > best || (n == best && r.allow) {
			best, allow = n, r.allow
		}
	}
	return allow
}
