package ticker

import (
	"sort"
	"time"
)

// Feed states, worst last. The screens grey a whole underlying on anything but ok
// (or closed/pre), and leave per-strike freshness to each cell's own quote age, so
// an illiquid strike is never mistaken for a dead feed.
const (
	FeedClosed = "closed" // outside the continuous session or a non-trading day
	FeedPre    = "pre"    // pre-open
	FeedOK     = "ok"
	FeedStale  = "stale" // socket up, exchange ticking, but this underlying's heartbeat silent
	FeedHalt   = "halt"  // socket up, yet nothing on the exchange ticks: a market-wide halt
	FeedDown   = "down"  // the socket carrying the heartbeat is disconnected
)

var feedRank = map[string]int{FeedClosed: 0, FeedPre: 0, FeedOK: 1, FeedStale: 2, FeedHalt: 3, FeedDown: 4}

// Worse reports whether state a is worse than b.
func Worse(a, b string) bool { return feedRank[a] > feedRank[b] }

// Beat is what the classifier sees of one underlying: the last receive times (zero
// when never seen) of its heartbeat instruments, the index and the nearest future,
// and whether the sockets carrying them are up.
type Beat struct {
	Index, Fut time.Time
	SocketUp   bool
}

// Exchange is the last tick on any subscribed derivatives or cash token, and
// whether every socket is up.
type Exchange struct {
	Last  time.Time
	AllUp bool
}

// Thresholds are the configured silences.
type Thresholds struct {
	Stale time.Duration // heartbeat silence that marks an underlying stale
	Halt  time.Duration // exchange-wide silence, sockets up, that marks a halt
}

// Classify gives one underlying's feed state.
//
// A halt is recognised as silence on every subscribed token while the sockets stay
// up (Kite keeps sending its 1-byte heartbeats, so the connection is alive but the
// exchange sends nothing). verify: that Kite sends no ticks, rather than repeated
// unchanged ticks, while NSE halts trading; if it repeats ticks, an unchanged index
// exchange timestamp would be the signal instead.
func Classify(now time.Time, phase string, b Beat, ex Exchange, th Thresholds) string {
	switch phase {
	case "p":
		return FeedPre
	case "c":
	default:
		return FeedClosed
	}
	if !b.SocketUp {
		return FeedDown
	}
	if fresh(now, b.Index, th.Stale) || fresh(now, b.Fut, th.Stale) {
		return FeedOK
	}
	if ex.AllUp && !fresh(now, ex.Last, th.Halt) {
		return FeedHalt
	}
	return FeedStale
}

func fresh(now, t time.Time, within time.Duration) bool {
	return !t.IsZero() && now.Sub(t) <= within
}

// FeedStatus is published on sys.ticker.feed every second.
type FeedStatus struct {
	At      int64              `json:"at"`
	Phase   string             `json:"phase"`
	State   string             `json:"state"` // the worst underlying
	Sockets string             `json:"sockets"`
	LastAny int64              `json:"last_any"` // ms of the last tick on any token
	Und     map[string]UndFeed `json:"und"`
}

// UndFeed is one underlying's heartbeat: state and ages in ms (-1 when never seen).
type UndFeed struct {
	State    string `json:"state"`
	IndexAge int64  `json:"index_age"`
	FutAge   int64  `json:"fut_age"`
	Since    int64  `json:"since"` // ms the current state began
}

func ageMs(now, t time.Time) int64 {
	if t.IsZero() {
		return -1
	}
	return max(0, now.Sub(t).Milliseconds())
}

// worst returns the worst state across underlyings, ok when there are none.
func worst(m map[string]UndFeed) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	w := ""
	for _, k := range keys {
		if w == "" || Worse(m[k].State, w) {
			w = m[k].State
		}
	}
	if w == "" {
		return FeedOK
	}
	return w
}
