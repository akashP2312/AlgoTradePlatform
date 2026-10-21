package newsmon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"pmt/internal/bus"
	"pmt/internal/calendar"
	"pmt/internal/clock"
	"pmt/internal/config"
	"pmt/internal/notify"
	"pmt/internal/refdata"
	"pmt/internal/store"
)

// Subjects and KV keys newsmon publishes (bucket news).
const (
	SubjRisk   = "news.risk"
	SubjEvents = "news.events"
	KeyRisk    = "risk"
	KeyEvents  = "events"
)

type Service struct {
	Cfg    *config.Config
	Bus    *bus.Conn
	DB     store.Querier
	Notify *notify.Notifier
	Cal    *calendar.Calendar
	Feed   *Ingest // headlines; built in Run when nil

	mu      sync.Mutex
	file    []Event // from the event file
	expiry  []Event // derived from the listed instruments
	expDay  time.Time
	mtime   time.Time
	loaded  time.Time
	fileErr string
	expErr  string
	state   RiskState
	started bool
}

func (s *Service) Health() (bool, map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := map[string]string{
		"state":    s.state.State.String(),
		"events":   fmt.Sprint(len(s.file)),
		"expiries": fmt.Sprint(len(s.expiry)),
	}
	if s.fileErr != "" {
		d["events_err"] = s.fileErr
	}
	if s.expErr != "" {
		d["expiries_err"] = s.expErr
	}
	if s.Feed != nil {
		// A feed outage is reported, never a health failure: the calendar still filters.
		ok, n, items := s.Feed.Health()
		d["feed_sources"] = fmt.Sprintf("%d/%d ok", ok, n)
		d["feed_items"] = fmt.Sprint(items)
	}
	return s.started && s.fileErr == "", d
}

func (s *Service) windows() Windows {
	return Windows{ByImpact: s.Cfg.News.Windows, ByCategory: s.Cfg.News.Categories}
}

func (s *Service) all() []Event {
	out := make([]Event, 0, len(s.file)+len(s.expiry))
	out = append(append(out, s.file...), s.expiry...)
	Sort(out)
	return out
}

// reloadFile rereads the event file when its mtime moved. A bad edit keeps the
// previous events in force and says why.
func (s *Service) reloadFile(ctx context.Context) bool {
	path := s.Cfg.News.Events
	fi, err := os.Stat(path)
	if err == nil && fi.ModTime().Equal(s.mtime) && s.fileErr == "" {
		return false
	}
	var es []Event
	if err == nil {
		es, err = LoadEvents(path)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		if s.fileErr != err.Error() {
			slog.Warn("newsmon: event file", "path", path, "err", err)
			s.Notify.Send(ctx, notify.Warn, "news_events", "event file not loaded, previous events kept: %v", err)
		}
		s.fileErr = err.Error()
		if fi != nil {
			s.mtime = fi.ModTime()
		}
		return true
	}
	s.file, s.mtime, s.loaded, s.fileErr = es, fi.ModTime(), clock.Now(), ""
	slog.Info("newsmon: event file loaded", "events", len(es))
	return true
}

// loadExpiries derives F&O expiry events from the listed instruments, once a day.
func (s *Service) loadExpiries(ctx context.Context, today time.Time) error {
	unds := s.Cfg.News.Expiries
	insts, err := refdata.Current(ctx, s.DB, unds...)
	if err != nil {
		return err
	}
	exch := map[string]string{}
	for _, u := range s.Cfg.Universe.Underlyings {
		exch[u.Name] = u.DerivExchange
	}
	var out []Event
	for _, u := range unds {
		x := exch[u]
		if x == "" {
			x = "NFO"
		}
		es, err := ExpiryEvents(u, refdata.Expiries(insts, x, u, today), s.Cfg.News.ExpiryAt)
		if err != nil {
			return err
		}
		out = append(out, es...)
	}
	if len(out) == 0 {
		return errors.New("no listed expiries for " + strings.Join(unds, ", "))
	}
	s.mu.Lock()
	s.expiry, s.expDay = out, today
	s.mu.Unlock()
	slog.Info("newsmon: expiries loaded", "events", len(out))
	return nil
}

func (s *Service) lastLogged(ctx context.Context) (Level, error) {
	var st string
	err := s.DB.QueryRow(ctx, `SELECT state FROM news_risk_log ORDER BY ts DESC LIMIT 1`).Scan(&st)
	if errors.Is(err, pgx.ErrNoRows) {
		return Normal, nil
	}
	if err != nil {
		return Normal, err
	}
	return ParseLevel(st)
}

// logChange records a level change for backtests: what the filter said, when.
func (s *Service) logChange(ctx context.Context, st RiskState, prev Level) {
	rs, _ := json.Marshal(st.Reasons)
	var exp *time.Time
	if st.ExpiresAt != 0 {
		t := bus.FromMs(st.ExpiresAt)
		exp = &t
	}
	if _, err := s.DB.Exec(ctx, `INSERT INTO news_risk_log (ts, state, prev, reasons, expires_at) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (ts) DO NOTHING`, bus.FromMs(st.TS), st.State.String(), prev.String(), rs, exp); err != nil {
		slog.Warn("newsmon: risk log", "err", err)
	}
}

func same(a, b RiskState) bool {
	if a.State != b.State || a.ExpiresAt != b.ExpiresAt || len(a.Reasons) != len(b.Reasons) {
		return false
	}
	for i := range a.Reasons {
		if a.Reasons[i].Event != b.Reasons[i].Event || a.Reasons[i].Level != b.Reasons[i].Level {
			return false
		}
	}
	an, bn := a.NextHigh, b.NextHigh
	return (an == nil) == (bn == nil) && (an == nil || *an == *bn)
}

func (s *Service) Run(ctx context.Context) error {
	kv, err := s.Bus.KV(ctx, bus.KVNews)
	if err != nil {
		return err
	}
	pub := func(subj, key string, v any) {
		b, err := json.Marshal(v)
		if err != nil {
			slog.Warn("newsmon: marshal", "subj", subj, "err", err)
			return
		}
		if err := s.Bus.NC.Publish(subj, b); err != nil {
			slog.Warn("newsmon: publish", "subj", subj, "err", err)
		}
		if _, err := kv.Put(ctx, key, b); err != nil {
			slog.Warn("newsmon: kv put", "key", key, "err", err)
		}
	}
	pubEvents := func(now time.Time) {
		s.mu.Lock()
		c := Calendar{TS: bus.Ms(now), Err: s.fileErr, Events: View(now, s.all(), s.Cfg.News.Horizon)}
		if !s.loaded.IsZero() {
			c.Loaded = bus.Ms(s.loaded)
		}
		s.mu.Unlock()
		pub(SubjEvents, KeyEvents, c)
	}

	prev, err := s.lastLogged(ctx)
	if err != nil {
		slog.Warn("newsmon: last logged state", "err", err)
	}
	s.reloadFile(ctx)
	now := clock.Now()
	if err := s.loadExpiries(ctx, clock.Date(now)); err != nil {
		s.mu.Lock()
		s.expErr = err.Error()
		s.mu.Unlock()
		slog.Warn("newsmon: expiries", "err", err)
	}
	s.mu.Lock()
	s.state = RiskState{State: prev, Since: bus.Ms(now)}
	if s.Feed == nil {
		s.Feed = NewIngest(s.Cfg.News.Feeds, s.DB)
	}
	s.started = true
	s.mu.Unlock()
	pubEvents(now)
	if err := s.Feed.Seed(ctx, now); err != nil {
		slog.Warn("newsmon: reload headlines", "err", err)
	}
	if err := StartFeeds(ctx, s.Cfg.News.Feeds, s.Feed); err != nil {
		slog.Warn("newsmon: feeds", "err", err)
	}
	pubFeed := func(now time.Time, force bool) bool {
		f, dirty := s.Feed.Snapshot(now)
		if dirty || force {
			pub(SubjFeed, KeyFeed, f)
		}
		return dirty || force
	}
	pubFeed(now, true)

	t := time.NewTicker(time.Second)
	defer t.Stop()
	var lastPub, lastEvents, lastFile, lastExp time.Time
	lastFeed := now
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		now := clock.Now()
		changed := false
		if now.Sub(lastFile) >= 5*time.Second {
			lastFile = now
			changed = s.reloadFile(ctx)
		}
		s.mu.Lock()
		stale := !s.expDay.Equal(clock.Date(now))
		s.mu.Unlock()
		if stale && now.Sub(lastExp) >= time.Minute {
			lastExp = now
			err := s.loadExpiries(ctx, clock.Date(now))
			s.mu.Lock()
			if err != nil {
				s.expErr = err.Error()
			} else {
				s.expErr, changed = "", true
			}
			s.mu.Unlock()
		}
		if changed || now.Sub(lastEvents) >= time.Minute {
			lastEvents = now
			pubEvents(now)
		}
		if d := now.Sub(lastFeed); d >= 2*time.Second && pubFeed(now, d >= time.Minute) {
			lastFeed = now
		}

		s.mu.Lock()
		st := Assess(now, s.all(), s.windows())
		old := s.state
		if st.State != old.State {
			st.Since = st.TS
		} else {
			st.Since = old.Since
		}
		s.state = st
		s.mu.Unlock()

		if st.State != old.State {
			s.logChange(ctx, st, old.State)
			lvl := notify.Info
			if st.State == NoNewEntries {
				lvl = notify.Warn
			}
			msg := "no scheduled event window"
			if len(st.Reasons) > 0 {
				msg = st.Reasons[0].Msg
			}
			s.Notify.Send(ctx, lvl, "news_risk", "risk state %s → %s: %s", old.State, st.State, msg)
		}
		if !same(st, old) || now.Sub(lastPub) >= s.Cfg.News.Publish {
			lastPub = now
			pub(SubjRisk, KeyRisk, st)
		}
	}
}
