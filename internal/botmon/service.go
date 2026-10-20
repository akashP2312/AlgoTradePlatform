package botmon

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"pmt/internal/bus"
	"pmt/internal/calendar"
	"pmt/internal/clock"
	"pmt/internal/config"
	"pmt/internal/kite"
	"pmt/internal/notify"
)

// Subject and KV key for a bot's state (bucket bot): bot.<name>.state.
func Subj(name string) string { return "bot." + Key(name) }
func Key(name string) string  { return name + ".state" }

// Service watches one bot's ledger. It only reads: the ledger file is opened
// read-only, and the broker is asked for holdings, never sent anything.
type Service struct {
	Cfg    *config.Config
	Bus    *bus.Conn
	Notify *notify.Notifier
	Cal    *calendar.Calendar

	mu sync.Mutex
	m  *Monitor
}

func (s *Service) Health() (bool, map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		return s.Cfg.Bot.Dir == "", map[string]string{"bot": "disabled"}
	}
	d := map[string]string{"bot": s.m.B.Name, "mode": s.m.B.Mode}
	if s.m.readAt.IsZero() {
		d["ledger"] = "never read"
	} else {
		d["ledger"] = clock.Now().Sub(s.m.readAt).Round(time.Second).String()
	}
	if s.m.readErr != "" {
		d["err"] = s.m.readErr
	}
	if s.m.l != nil {
		d["as_of"] = s.m.l.Meta["last_asof"]
		d["positions"] = strconv.Itoa(len(s.m.l.Positions))
	}
	return s.m.fails < failAfter, d
}

func (s *Service) Run(ctx context.Context) error {
	b := s.Cfg.Bot
	if b.Dir == "" {
		slog.Info("botmon: bot.dir is empty; disabled")
		<-ctx.Done()
		return nil
	}
	kv, err := s.Bus.KV(ctx, bus.KVBot)
	if err != nil {
		return err
	}
	m := New(b, s.Cal)
	s.mu.Lock()
	s.m = m
	s.mu.Unlock()
	path := b.Ledger()
	slog.Info("botmon: watching", "bot", b.Name, "mode", b.Mode, "ledger", path)

	if m.Live() {
		if err := s.watchBroker(ctx); err != nil {
			return err
		}
	}

	subj, key := Subj(b.Name), Key(b.Name)
	pub := func(v any) {
		buf, err := json.Marshal(v)
		if err == nil {
			if err = s.Bus.NC.Publish(subj, buf); err == nil {
				_, err = kv.Put(ctx, key, buf)
			}
		}
		if err != nil && ctx.Err() == nil {
			slog.Warn("botmon: publish", "subj", subj, "err", err)
		}
	}

	poll := func() {
		l, err := ReadLedger(ctx, path)
		if err != nil && !errors.Is(err, ErrBusy) && ctx.Err() == nil {
			slog.Warn("botmon: ledger read", "path", path, "err", err)
		}
		s.mu.Lock()
		m.OnLedger(l, err, clock.Now())
		s.mu.Unlock()
	}
	poll()
	next := time.Now().Add(b.Poll)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	lastPub := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		polled := false
		if !time.Now().Before(next) {
			poll()
			polled, next = true, time.Now().Add(b.Poll)
		}
		now := clock.Now()
		s.mu.Lock()
		changes := m.Eval(now)
		st := m.State(now)
		s.mu.Unlock()
		for _, c := range changes {
			s.Notify.Send(ctx, notifyLevel(c), c.Rule, "%s", c.Msg)
		}
		// The ledger changes once a day; publish on a new read, on a level change,
		// and every 15s so the panel's freshness and "behind" stay current.
		if polled || len(changes) > 0 || time.Since(lastPub) >= 15*time.Second {
			pub(st)
			lastPub = time.Now()
		}
	}
}

// watchBroker polls the broker's holdings and reads the CNC net positions from the
// book bucket, for the live-mode reconciliation. Waits for a Kite session.
func (s *Service) watchBroker(ctx context.Context) error {
	skv, err := s.Bus.KV(ctx, bus.KVSession)
	if err != nil {
		return err
	}
	bkv, err := s.Bus.KV(ctx, bus.KVBook)
	if err != nil {
		return err
	}
	sessions, err := kite.WatchSession(ctx, skv)
	if err != nil {
		return err
	}
	go func() {
		var rest *kite.REST
		t := time.NewTicker(s.Cfg.Bot.Poll)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case se, ok := <-sessions:
				if !ok {
					return
				}
				key := se.APIKey
				if key == "" {
					key = s.Cfg.Kite.APIKey
				}
				rest = kite.NewREST(s.Cfg.Kite, key, se.AccessToken)
			case <-t.C:
			}
			if rest == nil {
				continue
			}
			qty, err := brokerQty(ctx, rest, bkv)
			if err != nil && ctx.Err() == nil {
				slog.Warn("botmon: broker read", "err", err)
			}
			s.mu.Lock()
			s.m.OnBroker(qty, err, clock.Now())
			s.mu.Unlock()
		}
	}()
	return nil
}

// brokerQty is what the account holds per tradingsymbol: settled holdings plus T1,
// plus today's CNC net from book.positions (a sell today shows there as negative
// before holdings change). (verify) against a live account.
func brokerQty(ctx context.Context, rest *kite.REST, bkv jetstream.KeyValue) (map[string]int64, error) {
	hs, err := rest.Holdings(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]int64{}
	for _, h := range hs {
		out[h.TradingSymbol] += h.Quantity + h.T1Quantity
	}
	e, err := bkv.Get(ctx, "positions")
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return out, nil
		}
		return nil, err
	}
	var bk bus.Book
	if err := json.Unmarshal(e.Value(), &bk); err != nil {
		return nil, err
	}
	for _, p := range bk.Net {
		if p.Product == "CNC" {
			out[p.Sym] += p.Qty
		}
	}
	return out, nil
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
