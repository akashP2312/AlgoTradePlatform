package riskmon

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"pmt/internal/bus"
	"pmt/internal/calendar"
	"pmt/internal/clock"
	"pmt/internal/config"
	"pmt/internal/notify"
	"pmt/internal/store"
)

// Subjects and KV keys riskmon publishes (bucket risk).
const (
	SubjMon = "risk.mon"
	SubjDay = "risk.day"
	KeyMon  = "mon"
	KeyDay  = "day"
)

type Service struct {
	Cfg    *config.Config
	Bus    *bus.Conn
	DB     store.Querier
	Notify *notify.Notifier
	Cal    *calendar.Calendar

	mu    sync.Mutex
	m     *Monitor
	snaps int
	err   string
}

func (s *Service) Health() (bool, map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := map[string]string{"snaps_today": strconv.Itoa(s.snaps)}
	if s.err != "" {
		d["err"] = s.err
	}
	if s.m == nil {
		return false, d
	}
	if s.m.rb == nil {
		d["risk_book"] = "never"
	} else {
		d["risk_book"] = clock.Now().Sub(s.m.rbAt).Round(time.Second).String()
	}
	return s.err == "", d
}

// session reports whether now is inside today's continuous session.
func (s *Service) session(now time.Time) bool {
	se := s.Cal.Session(now)
	return se.Trading() && !now.Before(se.Open) && !now.After(se.Close)
}

func (s *Service) Run(ctx context.Context) error {
	kv, err := s.Bus.KV(ctx, bus.KVRisk)
	if err != nil {
		return err
	}
	bkv, err := s.Bus.KV(ctx, bus.KVBook)
	if err != nil {
		return err
	}
	now := clock.Now()
	m := New(s.Cfg.Risk.Limits)
	snaps, err := LoadDay(ctx, s.DB, clock.Date(now))
	if err != nil {
		return err
	}
	m.Restore(clock.Date(now), snaps)
	s.mu.Lock()
	s.m, s.snaps = m, len(snaps)
	s.mu.Unlock()
	slog.Info("riskmon: restored the day", "snaps", len(snaps))

	onBook := func(b []byte) {
		var rb RiskBook
		if err := json.Unmarshal(b, &rb); err != nil {
			slog.Warn("riskmon: bad risk.book", "err", err)
			return
		}
		t := clock.Now()
		s.mu.Lock()
		m.OnBook(&rb, t, clock.Date(t))
		s.mu.Unlock()
	}
	onMargins := func(b []byte) {
		var mg bus.Margins
		if err := json.Unmarshal(b, &mg); err != nil {
			slog.Warn("riskmon: bad book.margins", "err", err)
			return
		}
		s.mu.Lock()
		m.OnMargins(&mg, clock.Now())
		s.mu.Unlock()
	}
	// Seed from KV, but only with values young enough to count as live: an old
	// risk.book left over from yesterday must still read as stale.
	seed := func(kv jetstream.KeyValue, key string, f func([]byte), ts func([]byte) int64) {
		e, err := kv.Get(ctx, key)
		if err != nil {
			if !errors.Is(err, jetstream.ErrKeyNotFound) {
				slog.Warn("riskmon: kv seed", "key", key, "err", err)
			}
			return
		}
		if age := clock.Now().Sub(bus.FromMs(ts(e.Value()))); age < s.Cfg.Risk.Limits.Stale || s.Cfg.Risk.Limits.Stale == 0 {
			f(e.Value())
		}
	}
	tsOf := func(b []byte) int64 {
		var x struct {
			TS int64 `json:"ts"`
		}
		_ = json.Unmarshal(b, &x)
		return x.TS
	}
	seed(kv, "book", onBook, tsOf)
	seed(bkv, "margins", onMargins, tsOf)

	for subj, f := range map[string]func([]byte){"risk.book": onBook, bus.BookMargins: onMargins} {
		sub, err := s.Bus.NC.Subscribe(subj, func(msg *nats.Msg) { f(msg.Data) })
		if err != nil {
			return err
		}
		defer sub.Unsubscribe()
	}

	pub := func(subj, key string, v any) {
		b, err := json.Marshal(v)
		if err == nil {
			if err = s.Bus.NC.Publish(subj, b); err == nil {
				_, err = kv.Put(ctx, key, b)
			}
		}
		if err != nil && ctx.Err() == nil {
			slog.Warn("riskmon: publish", "subj", subj, "err", err)
		}
	}
	s.mu.Lock()
	day := m.Day()
	s.mu.Unlock()
	pub(SubjDay, KeyDay, day)

	lastMin := clock.Minute(now)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		now := clock.Now()
		in := s.session(now)

		s.mu.Lock()
		m.rollTo(clock.Date(now))
		changes := m.Eval(now, in)
		mon := m.State(now, in)
		var snap Snap
		take := false
		if mi := clock.Minute(now); mi.After(lastMin) {
			lastMin = mi
			if in {
				snap, take = m.Snapshot(mi)
			}
		}
		s.mu.Unlock()

		for _, c := range changes {
			s.Notify.Send(ctx, notifyLevel(c), c.Rule, "%s", c.Msg)
		}
		pub(SubjMon, KeyMon, mon)
		if !take {
			continue
		}
		err := SaveSnap(ctx, s.DB, clock.Date(now), snap)
		s.mu.Lock()
		if err != nil {
			s.err = err.Error()
		} else {
			s.err = ""
			m.Add(snap)
			s.snaps = len(m.snaps)
		}
		day := m.Day()
		s.mu.Unlock()
		if err != nil {
			slog.Warn("riskmon: snapshot", "err", err)
		}
		pub(SubjDay, KeyDay, day)
	}
}

func notifyLevel(c Change) notify.Level {
	switch {
	case c.To == Crit:
		return notify.Crit
	case c.To == Warn && c.From == OK:
		return notify.Warn
	default: // cleared or stepped down
		return notify.Info
	}
}

// SaveSnap writes one minute; a second write for the same minute is ignored.
func SaveSnap(ctx context.Context, q store.Querier, day time.Time, s Snap) error {
	_, err := q.Exec(ctx, `INSERT INTO risk_snapshots
		(ts, trade_date, m2m, pnl, ref_px, d, dn, g, v, t, worst, util, legs, flags)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT (ts) DO NOTHING`,
		bus.FromMs(s.TS), day, s.M2M, s.PnL, s.RefPx, s.D, s.Dn, s.G, s.V, s.T, s.Worst, s.Util, s.Legs, s.Flags)
	return err
}

// LoadDay reads a trade date's snapshots in time order.
func LoadDay(ctx context.Context, q store.Querier, day time.Time) ([]Snap, error) {
	rows, err := q.Query(ctx, `SELECT ts, m2m, pnl, ref_px, COALESCE(d,0), COALESCE(dn,0), COALESCE(g,0),
		COALESCE(v,0), COALESCE(t,0), worst, util, COALESCE(legs,0), COALESCE(flags,0)
		FROM risk_snapshots WHERE trade_date = $1 ORDER BY ts`, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Snap
	for rows.Next() {
		var s Snap
		var ts time.Time
		if err := rows.Scan(&ts, &s.M2M, &s.PnL, &s.RefPx, &s.D, &s.Dn, &s.G, &s.V, &s.T, &s.Worst, &s.Util, &s.Legs, &s.Flags); err != nil {
			return nil, err
		}
		s.TS = bus.Ms(ts)
		out = append(out, s)
	}
	return out, rows.Err()
}
