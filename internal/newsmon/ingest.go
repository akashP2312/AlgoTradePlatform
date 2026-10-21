package newsmon

import (
	"context"
	"log/slog"
	"slices"
	"sort"
	"sync"
	"time"

	"pmt/internal/bus"
	"pmt/internal/clock"
	"pmt/internal/config"
	"pmt/internal/store"
)

// Subject and KV key of the headline feed (bucket news).
const (
	SubjFeed = "news.feed"
	KeyFeed  = "feed"
)

// Feed is news.feed: the latest headlines, newest first, and each source's health.
type Feed struct {
	TS      int64          `json:"ts"`
	Items   []Item         `json:"items"`
	Sources []SourceStatus `json:"sources"`
}

// SourceStatus is one source's poll health. Times are epoch ms.
type SourceStatus struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Trust   string `json:"trust"`
	Region  string `json:"region"`
	State   string `json:"state"` // starting, ok, backoff, open, half_open, blocked, disabled, stub
	Err     string `json:"err,omitempty"`
	Fails   int    `json:"fails,omitempty"`
	LastTry int64  `json:"last_try,omitempty"`
	LastOK  int64  `json:"last_ok,omitempty"`
	Next    int64  `json:"next,omitempty"`
	Polls   int    `json:"polls"`
	New     int    `json:"new"`               // headlines first seen from this source since start (backfill excluded)
	LagMed  int64  `json:"lag_med,omitempty"` // median seen − published over the last lagKeep new headlines
	LagMax  int64  `json:"lag_max,omitempty"` // and the worst of them
	Note    string `json:"note,omitempty"`

	lags []int64
}

const lagKeep = 50

// Ingest holds the deduplicated headlines and each source's status. Pollers add
// to it from their own goroutines.
type Ingest struct {
	Cfg config.Feeds
	DB  store.Querier // nil: nothing is stored

	mu     sync.Mutex
	ded    *Deduper
	items  []Item // newest first, at most Cfg.Keep
	status map[string]*SourceStatus
	order  []string
	dirty  bool
}

func NewIngest(cfg config.Feeds, db store.Querier) *Ingest {
	in := &Ingest{Cfg: cfg, DB: db, ded: NewDeduper(cfg.DedupeTTL, cfg.Similar), status: map[string]*SourceStatus{}}
	for _, s := range cfg.Sources {
		st := &SourceStatus{ID: s.ID, Name: s.Name, Trust: s.Trust, Region: s.Region, State: "starting", Note: s.Note}
		switch {
		case s.Kind == "paid":
			st.State = "stub"
		case !s.Enabled:
			st.State = "disabled"
		}
		in.status[s.ID] = st
		in.order = append(in.order, s.ID)
	}
	return in
}

// Add takes one poll's entries from src, seen at seen. Entries from a source's
// first poll are backfill: they fill the feed but carry no lag and are not new.
// It returns how many headlines were new.
func (in *Ingest) Add(ctx context.Context, src config.Source, loc *time.Location, raws []RawItem, backfill bool, seen time.Time) int {
	real := clock.Real(seen)
	var ins []Item
	type alsoUpd struct{ id, src string }
	var also []alsoUpd

	in.mu.Lock()
	st := in.status[src.ID]
	for _, r := range raws {
		it, ok := Normalize(src, loc, r, seen)
		if !ok || it.age(real) > in.Cfg.MaxAge {
			continue
		}
		origID, origSrc, dup := in.ded.Check(&it)
		if dup {
			if origSrc != src.ID && in.addAlso(origID, src.ID) {
				also = append(also, alsoUpd{origID, src.ID})
			}
			continue
		}
		if !backfill && it.Pub != 0 && !it.DateOnly {
			it.LagMs = max(0, real.Sub(bus.FromMs(it.Pub)).Milliseconds())
			if st != nil {
				st.lags = append(st.lags, it.LagMs)
				if len(st.lags) > lagKeep {
					st.lags = st.lags[len(st.lags)-lagKeep:]
				}
				st.LagMed, st.LagMax = median(st.lags), slices.Max(st.lags)
			}
		}
		if !backfill && st != nil {
			st.New++
		}
		ins = append(ins, it)
		in.insert(it)
	}
	if len(ins) > 0 || len(also) > 0 {
		in.dirty = true
	}
	in.mu.Unlock()

	if in.DB == nil {
		return len(ins)
	}
	for _, it := range ins {
		in.store(ctx, it)
	}
	for _, a := range also {
		if _, err := in.DB.Exec(ctx, `UPDATE news_items SET also = array_append(also, $2) WHERE id = $1 AND NOT ($2 = ANY(also))`, a.id, a.src); err != nil {
			slog.Warn("newsmon: news_items also", "err", err)
		}
	}
	return len(ins)
}

// addAlso records that src also carried the story id; false if it already had.
// Under in.mu.
func (in *Ingest) addAlso(id, src string) bool {
	for i := range in.items {
		if in.items[i].ID == id {
			if slices.Contains(in.items[i].Also, src) {
				return false
			}
			in.items[i].Also = append(in.items[i].Also, src)
			return true
		}
	}
	return true // not in the feed any more; the stored row still learns it
}

// insert puts it in the feed by sort time, newest first. Under in.mu.
func (in *Ingest) insert(it Item) {
	i := sort.Search(len(in.items), func(i int) bool { return in.items[i].TS < it.TS })
	in.items = slices.Insert(in.items, i, it)
	if len(in.items) > in.Cfg.Keep {
		in.items = in.items[:in.Cfg.Keep]
	}
}

func (in *Ingest) store(ctx context.Context, it Item) {
	var pub *time.Time
	if it.Pub != 0 {
		t := bus.FromMs(it.Pub)
		pub = &t
	}
	var lag *int64
	if it.LagMs != 0 {
		lag = &it.LagMs
	}
	cats := it.Cats
	if cats == nil {
		cats = []string{}
	}
	if _, err := in.DB.Exec(ctx, `INSERT INTO news_items (id, source, trust, region, title, summary, url, guid, cats, published, date_only, seen, ts, lag_ms)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14) ON CONFLICT (id) DO NOTHING`,
		it.ID, it.Source, it.Trust, it.Region, it.Title, it.Summary, it.URL, it.GUID, cats, pub, it.DateOnly,
		bus.FromMs(it.Seen), bus.FromMs(it.TS), lag); err != nil {
		slog.Warn("newsmon: news_items insert", "err", err)
	}
}

// Seed reloads the last DedupeTTL of headlines, so a restart neither repeats
// them nor starts with an empty feed.
func (in *Ingest) Seed(ctx context.Context, now time.Time) error {
	if in.DB == nil {
		return nil
	}
	rows, err := in.DB.Query(ctx, `SELECT id, source, trust, region, title, summary, url, guid, cats, published, date_only, seen, ts, lag_ms, also
		FROM news_items WHERE seen >= $1 ORDER BY seen`, now.Add(-in.Cfg.DedupeTTL))
	if err != nil {
		return err
	}
	defer rows.Close()
	in.mu.Lock()
	defer in.mu.Unlock()
	n := 0
	for rows.Next() {
		var it Item
		var pub *time.Time
		var seen, ts time.Time
		var lag *int64
		if err := rows.Scan(&it.ID, &it.Source, &it.Trust, &it.Region, &it.Title, &it.Summary, &it.URL, &it.GUID, &it.Cats,
			&pub, &it.DateOnly, &seen, &ts, &lag, &it.Also); err != nil {
			return err
		}
		if pub != nil {
			it.Pub = bus.Ms(*pub)
		}
		if lag != nil {
			it.LagMs = *lag
		}
		it.Seen, it.TS = bus.Ms(seen), bus.Ms(ts)
		if it.URL != "" {
			it.canon = CanonURL(it.URL)
		}
		if len(it.Also) == 0 {
			it.Also = nil
		}
		if len(it.Cats) == 0 {
			it.Cats = nil
		}
		in.ded.Check(&it)
		in.insert(it)
		n++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	in.dirty = n > 0
	slog.Info("newsmon: headlines reloaded", "items", n)
	return nil
}

// update changes a source's status under the lock.
func (in *Ingest) update(id string, f func(*SourceStatus)) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if st := in.status[id]; st != nil {
		f(st)
		in.dirty = true
	}
}

// Snapshot returns the feed, and whether it changed since the last one.
func (in *Ingest) Snapshot(now time.Time) (Feed, bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.ded.Prune(now)
	f := Feed{TS: bus.Ms(now), Items: slices.Clone(in.items), Sources: make([]SourceStatus, 0, len(in.order))}
	if f.Items == nil {
		f.Items = []Item{}
	}
	for _, id := range in.order {
		st := *in.status[id]
		st.lags = nil
		f.Sources = append(f.Sources, st)
	}
	d := in.dirty
	in.dirty = false
	return f, d
}

// Health counts sources that are polling fine, of those meant to poll.
func (in *Ingest) Health() (ok, polled, items int) {
	in.mu.Lock()
	defer in.mu.Unlock()
	for _, st := range in.status {
		if st.State == "disabled" || st.State == "stub" {
			continue
		}
		polled++
		if st.State == "ok" {
			ok++
		}
	}
	return ok, polled, len(in.items)
}

func median(xs []int64) int64 {
	s := slices.Clone(xs)
	slices.Sort(s)
	n := len(s)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}
