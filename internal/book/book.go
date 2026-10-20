// Package book polls the broker's positions and margins and publishes them on
// book.positions and book.margins, with each row joined to its contract so the
// terminal can reprice it from ticks. It only reads: there is no order code here.
package book

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"pmt/internal/bus"
	"pmt/internal/clock"
	"pmt/internal/config"
	"pmt/internal/contract"
	"pmt/internal/kite"
	"pmt/internal/notify"
	"pmt/internal/refdata"
	"pmt/internal/store"
)

const (
	pollEvery   = 5 * time.Second
	maxBackoff  = time.Minute
	reloadEvery = time.Minute // at most this often when a symbol is missing
	failAlert   = 2 * time.Minute
)

// Source is what the poller reads; *kite.REST satisfies it.
type Source interface {
	Positions(ctx context.Context) (kite.Positions, error)
	Margins(ctx context.Context) (kite.Margins, error)
}

type Service struct {
	Cfg    *config.Config
	Bus    *bus.Conn
	DB     store.Querier
	Notify *notify.Notifier

	mu      sync.Mutex
	last    time.Time // last good poll
	lastErr string
	nPos    int
	waiting bool // the token died; waiting for a fresh login

	ref *refIndex
}

// Health reports the poller's state for the heartbeat.
func (s *Service) Health() (bool, map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := map[string]string{"positions": strconv.Itoa(s.nPos)}
	if s.waiting {
		d["state"] = "waiting for login"
	}
	if s.lastErr != "" {
		d["err"] = s.lastErr
	}
	if s.last.IsZero() {
		d["last_poll"] = "never"
		return false, d
	}
	age := clock.Now().Sub(s.last)
	d["last_poll"] = age.Round(time.Second).String()
	return age < 3*pollEvery, d
}

func (s *Service) Run(ctx context.Context) error {
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
	s.setWaiting(true)
	slog.Info("book: waiting for a kite session")
	var rest *kite.REST
	select {
	case <-ctx.Done():
		return ctx.Err()
	case se, ok := <-sessions:
		if !ok {
			return ctx.Err()
		}
		rest = s.client(se)
	}
	s.ref = &refIndex{db: s.DB}

	pub := func(subj, key string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if err := s.Bus.NC.Publish(subj, b); err != nil {
			return err
		}
		_, err = bkv.Put(ctx, key, b)
		return err
	}

	backoff := pollEvery
	next := time.Now()
	var failingSince time.Time
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case se, ok := <-sessions:
			if !ok {
				return nil
			}
			rest = s.client(se)
			backoff, next = pollEvery, time.Now()
			continue
		case <-t.C:
		}
		if s.isWaiting() || time.Now().Before(next) {
			continue
		}
		bk, mg, err := Poll(ctx, rest, s.ref)
		if err == nil {
			if err = pub(bus.BookPositions, "positions", bk); err == nil {
				err = pub(bus.BookMargins, "margins", mg)
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			s.fail(err)
			if kite.IsTokenError(err) {
				s.setWaiting(true)
				slog.Warn("book: token rejected; waiting for a new login")
				continue
			}
			if failingSince.IsZero() {
				failingSince = time.Now()
			} else if time.Since(failingSince) > failAlert {
				s.Notify.Send(ctx, notify.Warn, "book-poll", "positions poll failing for %s: %v",
					time.Since(failingSince).Round(time.Second), err)
			}
			slog.Warn("book: poll failed", "err", err, "retry_in", backoff)
			next = time.Now().Add(backoff)
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		failingSince = time.Time{}
		s.ok(len(bk.Net))
		backoff = pollEvery
		next = time.Now().Add(pollEvery)
	}
}

func (s *Service) client(se bus.Session) *kite.REST {
	key := se.APIKey
	if key == "" {
		key = s.Cfg.Kite.APIKey
	}
	slog.Info("book: using " + se.String())
	s.setWaiting(false)
	return kite.NewREST(s.Cfg.Kite, key, se.AccessToken)
}

func (s *Service) setWaiting(w bool) { s.mu.Lock(); s.waiting = w; s.mu.Unlock() }
func (s *Service) isWaiting() bool   { s.mu.Lock(); defer s.mu.Unlock(); return s.waiting }

func (s *Service) fail(err error) {
	s.mu.Lock()
	s.lastErr = err.Error()
	s.mu.Unlock()
}

func (s *Service) ok(n int) {
	s.mu.Lock()
	s.last, s.lastErr, s.nPos = clock.Now(), "", n
	s.mu.Unlock()
}

// Poll reads positions and margins once and joins rows to refdata. A row whose
// symbol refdata does not know is still published, without a contract.
func Poll(ctx context.Context, src Source, ref Resolver) (bus.Book, bus.Margins, error) {
	ps, err := src.Positions(ctx)
	if err != nil {
		return bus.Book{}, bus.Margins{}, fmt.Errorf("positions: %w", err)
	}
	m, err := src.Margins(ctx)
	if err != nil {
		return bus.Book{}, bus.Margins{}, fmt.Errorf("margins: %w", err)
	}
	now := bus.Ms(clock.Now())
	bk := bus.Book{TS: now, Net: make([]bus.BookPos, 0, len(ps.Net)), Day: make([]bus.BookPos, 0, len(ps.Day))}
	for _, p := range ps.Net {
		bk.Net = append(bk.Net, row(ctx, p, ref))
	}
	for _, p := range ps.Day {
		bk.Day = append(bk.Day, row(ctx, p, ref))
	}
	e := m.Equity
	mg := bus.Margins{
		TS: now, Enabled: e.Enabled, Net: e.Net,
		Cash: e.Available.Cash, Opening: e.Available.OpeningBalance, Live: e.Available.LiveBalance,
		Collateral: e.Available.Collateral, Debits: e.Utilised.Debits, Span: e.Utilised.Span,
		Exposure: e.Utilised.Exposure, Premium: e.Utilised.OptionPremium,
		M2MRealised: e.Utilised.M2MRealised, M2MUnrealised: e.Utilised.M2MUnrealised,
	}
	return bk, mg, nil
}

func row(ctx context.Context, p kite.Position, ref Resolver) bus.BookPos {
	r := bus.BookPos{
		Sym: p.TradingSymbol, Exch: p.Exchange, Token: p.Token, Product: p.Product,
		Qty: p.Quantity, Overnight: p.OvernightQuantity, Mult: p.Multiplier, Avg: p.AveragePrice,
		Close: p.ClosePrice, LTP: p.LastPrice, PnL: p.PnL, M2M: p.M2M,
		Unrealised: p.Unrealised, Realised: p.Realised,
		BuyQty: p.BuyQuantity, BuyValue: p.BuyValue, SellQty: p.SellQuantity, SellValue: p.SellValue,
		DayBuyQty: p.DayBuyQuantity, DayBuyVal: p.DayBuyValue, DaySellQty: p.DaySellQuantity, DaySellVal: p.DaySellValue,
	}
	if ref == nil {
		return r
	}
	in, ok := ref.Lookup(ctx, p.Exchange, p.TradingSymbol)
	if !ok {
		return r
	}
	r.C, r.Subj, r.Und, r.Kind, r.Lot = in.Contract, bus.TickPrefix+in.Key.Subject(), in.Underlying, string(in.Key.Kind), in.LotSize
	if !in.Key.Expiry.IsZero() {
		r.Expiry = in.Key.Expiry.Format("2006-01-02")
	}
	if in.Key.Kind == contract.CE || in.Key.Kind == contract.PE {
		r.Strike = in.Key.Strike
	}
	return r
}

// Resolver finds the contract behind a broker symbol.
type Resolver interface {
	Lookup(ctx context.Context, exch, sym string) (refdata.Inst, bool)
}

// refIndex loads current refdata, reloading once a day and, when a symbol is
// missing (a contract listed this morning), at most once a minute.
type refIndex struct {
	db     store.Querier
	by     map[string]refdata.Inst
	day    time.Time
	loaded time.Time
}

func (x *refIndex) Lookup(ctx context.Context, exch, sym string) (refdata.Inst, bool) {
	today := clock.Today()
	if x.by == nil || !x.day.Equal(today) {
		x.load(ctx, today)
	}
	in, ok := x.by[exch+":"+sym]
	if !ok && time.Since(x.loaded) > reloadEvery {
		x.load(ctx, today)
		in, ok = x.by[exch+":"+sym]
	}
	return in, ok
}

func (x *refIndex) load(ctx context.Context, today time.Time) {
	x.loaded = time.Now()
	insts, err := refdata.Current(ctx, x.db)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			slog.Warn("book: refdata", "err", err)
		}
		return
	}
	by := make(map[string]refdata.Inst, len(insts))
	for _, in := range insts {
		by[in.Exchange+":"+in.TradingSymbol] = in
	}
	x.by, x.day = by, today
	slog.Info("book: refdata loaded", "instruments", len(by))
}
