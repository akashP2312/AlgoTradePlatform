package newsmon

import (
	"context"
	"encoding/json"
	"fmt"
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
	TS       int64          `json:"ts"`
	Items    []Item         `json:"items"`
	Sources  []SourceStatus `json:"sources"`
	Rules    int            `json:"rules"`               // classifier rules in force
	RulesErr string         `json:"rules_err,omitempty"` // why the rules file was not loaded; the previous rules stay
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
	Cfg   config.Feeds
	Class config.Classify
	DB    store.Querier // nil: nothing is stored
	LLM   *LLMClient    // nil: rules only

	mu       sync.Mutex
	rules    *Rules
	rulesErr string
	ded      *Deduper
	items    []Item // newest first, at most Cfg.Keep
	status   map[string]*SourceStatus
	order    []string
	dirty    bool
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
	type alsoUpd struct {
		id, src   string
		confirmed int64
	}
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
			if origSrc != src.ID {
				if ok, conf := in.addAlso(origID, src, seen); ok {
					also = append(also, alsoUpd{origID, src.ID, conf})
				}
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
		it.Backfill = backfill
		in.classify(&it)
		if in.trusted(it.Trust) || in.Class.Corroborate <= 1 {
			it.Confirmed = it.Seen
		}
		ins = append(ins, it)
		in.insert(it)
	}
	if len(ins) > 0 || len(also) > 0 {
		in.dirty = true
	}
	in.mu.Unlock()

	if in.LLM != nil {
		for i := range ins {
			// Only what the rules rate as possibly material but below CAUTION: the
			// model can only raise a score, and higher ones hold CAUTION already.
			if it := &ins[i]; !it.Backfill && it.Impact >= in.LLM.Cfg.MinRule && it.Impact < in.Class.CautionImpact {
				in.LLM.Enqueue(*it)
			}
		}
	}
	if in.DB == nil {
		return len(ins)
	}
	for _, it := range ins {
		in.store(ctx, it)
	}
	for _, a := range also {
		var conf *time.Time
		if a.confirmed != 0 {
			t := bus.FromMs(a.confirmed)
			conf = &t
		}
		if _, err := in.DB.Exec(ctx, `UPDATE news_items SET also = array_append(also, $2), confirmed = COALESCE(confirmed, $3)
			WHERE id = $1 AND NOT ($2 = ANY(also))`, a.id, a.src, conf); err != nil {
			slog.Warn("newsmon: news_items also", "err", err)
		}
	}
	return len(ins)
}

func (in *Ingest) trusted(trust string) bool { return slices.Contains(in.Class.Trusted, trust) }

// classify scores it with the rules in force. Under in.mu.
func (in *Ingest) classify(it *Item) {
	sc := in.rules.Classify(it)
	it.Impact, it.Tags, it.Rules = sc.Impact, sc.Tags, sc.Rules
}

// SetRules puts new classifier rules in force and rescores the feed, or, given
// an error, keeps the previous rules and reports why.
func (in *Ingest) SetRules(rs *Rules, err error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.dirty = true
	if err != nil {
		in.rulesErr = err.Error()
		return
	}
	in.rules, in.rulesErr = rs, ""
	for i := range in.items {
		in.classify(&in.items[i])
	}
}

// SetLLM records the model's rescore of a headline.
func (in *Ingest) SetLLM(ctx context.Context, id string, sc LLMScore) {
	in.mu.Lock()
	for i := range in.items {
		if in.items[i].ID == id {
			in.items[i].LLM = &sc
			in.dirty = true
		}
	}
	in.mu.Unlock()
	if in.DB == nil {
		return
	}
	b, _ := json.Marshal(sc)
	if _, err := in.DB.Exec(ctx, `UPDATE news_items SET llm = $2 WHERE id = $1`, id, b); err != nil {
		slog.Warn("newsmon: news_items llm", "err", err)
	}
}

// fresh says whether it was still news when it began to count: published at
// most Class.Fresh before. Without a publish time, backfill is not fresh.
func (in *Ingest) fresh(it *Item) bool {
	if it.Pub == 0 || it.DateOnly {
		return !it.Backfill
	}
	return clock.Real(bus.FromMs(it.Confirmed)).Sub(bus.FromMs(it.Pub)) <= in.Class.Fresh
}

// Holds are the headlines holding CAUTION at now: impact at least
// Class.CautionImpact, fresh, for Class.CautionFor after they began to count.
func (in *Ingest) Holds(now time.Time) []Hold {
	in.mu.Lock()
	defer in.mu.Unlock()
	c := in.Class
	if c.CautionImpact <= 0 || c.CautionFor <= 0 {
		return nil
	}
	var hs []Hold
	for i := range in.items {
		it := &in.items[i]
		n := it.Score()
		if it.Confirmed == 0 || n < c.CautionImpact || !in.fresh(it) {
			continue
		}
		from := bus.FromMs(it.Confirmed)
		until := from.Add(c.CautionFor)
		if now.Before(from) || !now.Before(until) {
			continue
		}
		why := "rules"
		if len(it.Rules) > 0 {
			why = it.Rules[0]
		}
		if it.LLM != nil && it.LLM.Impact > it.Impact {
			why = "llm: " + it.LLM.Why
		}
		by := it.Source
		if len(it.Also) > 0 {
			by = fmt.Sprintf("%s +%d", it.Source, len(it.Also))
		}
		hs = append(hs, Hold{Level: Caution, From: from, Until: until, Reason: Reason{
			Kind: "headline", Item: it.ID, Source: it.Source, Score: n, Name: it.Title,
			Msg: fmt.Sprintf("headline impact %d (%s, %s): %s", n, why, by, it.Title),
		}})
	}
	return hs
}

// addAlso records that src also carried the story id, seen at seen; false if it
// already had. The story begins to count (confirmed, epoch ms) once a trusted
// source carries it or Class.Corroborate distinct sources do. Under in.mu.
func (in *Ingest) addAlso(id string, src config.Source, seen time.Time) (bool, int64) {
	for i := range in.items {
		it := &in.items[i]
		if it.ID != id {
			continue
		}
		if slices.Contains(it.Also, src.ID) {
			return false, 0
		}
		it.Also = append(it.Also, src.ID)
		if it.Confirmed == 0 && (in.trusted(src.Trust) || 1+len(it.Also) >= in.Class.Corroborate) {
			it.Confirmed = bus.Ms(seen)
		}
		return true, it.Confirmed
	}
	return true, 0 // not in the feed any more; the stored row still learns it
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
	var conf *time.Time
	if it.Confirmed != 0 {
		t := bus.FromMs(it.Confirmed)
		conf = &t
	}
	tags, rules := it.Tags, it.Rules
	if tags == nil {
		tags = []string{}
	}
	if rules == nil {
		rules = []string{}
	}
	if _, err := in.DB.Exec(ctx, `INSERT INTO news_items (id, source, trust, region, title, summary, url, guid, cats, published, date_only, seen, ts, lag_ms,
			impact, tags, rules, backfill, confirmed)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19) ON CONFLICT (id) DO NOTHING`,
		it.ID, it.Source, it.Trust, it.Region, it.Title, it.Summary, it.URL, it.GUID, cats, pub, it.DateOnly,
		bus.FromMs(it.Seen), bus.FromMs(it.TS), lag, it.Impact, tags, rules, it.Backfill, conf); err != nil {
		slog.Warn("newsmon: news_items insert", "err", err)
	}
}

// Seed reloads the last DedupeTTL of headlines, so a restart neither repeats
// them nor starts with an empty feed (nor forgets a CAUTION a headline holds).
// They are rescored with the rules in force; the model's scores are kept.
func (in *Ingest) Seed(ctx context.Context, now time.Time) error {
	if in.DB == nil {
		return nil
	}
	rows, err := in.DB.Query(ctx, `SELECT id, source, trust, region, title, summary, url, guid, cats, published, date_only, seen, ts, lag_ms, also,
			backfill, confirmed, llm
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
		var conf *time.Time
		var llm []byte
		if err := rows.Scan(&it.ID, &it.Source, &it.Trust, &it.Region, &it.Title, &it.Summary, &it.URL, &it.GUID, &it.Cats,
			&pub, &it.DateOnly, &seen, &ts, &lag, &it.Also, &it.Backfill, &conf, &llm); err != nil {
			return err
		}
		if conf != nil {
			it.Confirmed = bus.Ms(*conf)
		}
		if len(llm) > 0 {
			var sc LLMScore
			if json.Unmarshal(llm, &sc) == nil {
				it.LLM = &sc
			}
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
		in.classify(&it)
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
	f := Feed{TS: bus.Ms(now), Items: slices.Clone(in.items), Sources: make([]SourceStatus, 0, len(in.order)), RulesErr: in.rulesErr}
	if in.rules != nil {
		f.Rules = len(in.rules.Rules)
	}
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
