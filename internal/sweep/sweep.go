// Package sweep snapshots option chains over REST: the full listed chain at 15:25
// (every expiry, every strike) and the backup recorder's narrow core window. Both
// produce the same bus.Chain the ticker cuts, with source "sweep" or "backup".
package sweep

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"pmt/internal/bus"
	"pmt/internal/clock"
	"pmt/internal/config"
	"pmt/internal/contract"
	"pmt/internal/kite"
	"pmt/internal/refdata"
)

// Target is one chain to snapshot: an underlying's options for one expiry, with the
// index and the future that price it.
type Target struct {
	Und, Exch string
	Expiry    time.Time
	Index     refdata.Inst // zero Token when the index is not listed
	Fut       refdata.Inst // future of the same expiry, else the nearest
	Opts      []refdata.Inst
}

// Quoter is the slice of the Kite client a snapshot needs.
type Quoter interface {
	Quotes(ctx context.Context, keys []string) (map[string]kite.Quote, error)
}

// Key is the exchange:tradingsymbol form /quote takes.
func Key(in refdata.Inst) string { return in.Exchange + ":" + in.TradingSymbol }

// All lists every live expiry and strike of the named underlyings: the 15:25 sweep.
func All(cfg *config.Config, insts []refdata.Inst, names []string, today time.Time) ([]Target, []string) {
	return targets(cfg, insts, names, today, func(ul config.Underlying, strikes []float64) ([]float64, int) {
		return strikes, -1
	})
}

// Core is the backup recorder's window: core_expiries by core_strikes either side of
// spot for every underlying with backup set. With no spot it centres on the middle
// listed strike and says so in the warnings.
func Core(cfg *config.Config, insts []refdata.Inst, spot map[string]float64, today time.Time) ([]Target, []string) {
	var names []string
	for _, ul := range cfg.Universe.Underlyings {
		if ul.Backup {
			names = append(names, ul.Name)
		}
	}
	var warn []string
	ts, w := targets(cfg, insts, names, today, func(ul config.Underlying, strikes []float64) ([]float64, int) {
		s := spot[ul.Name]
		if s <= 0 {
			s = strikes[len(strikes)/2]
		}
		return refdata.Window(strikes, s, ul.CoreStrikes), ul.CoreExpiries
	})
	for _, n := range names {
		if spot[n] <= 0 {
			warn = append(warn, fmt.Sprintf("%s: no spot, core window centred on the middle strike", n))
		}
	}
	return ts, append(warn, w...)
}

// pick returns the strikes to keep for one expiry and the number of expiries to
// take (-1 for all).
type pick func(ul config.Underlying, strikes []float64) ([]float64, int)

func targets(cfg *config.Config, all []refdata.Inst, names []string, today time.Time, choose pick) ([]Target, []string) {
	insts := refdata.Live(all, today)
	type exKey struct{ und, exp string }
	byExp := map[exKey][]refdata.Inst{}
	for _, in := range insts {
		if in.Key.Kind.IsOption() {
			k := exKey{in.Exchange + "|" + in.Key.Name, in.Key.Expiry.Format("2006-01-02")}
			byExp[k] = append(byExp[k], in)
		}
	}
	var out []Target
	var warn []string
	for _, name := range names {
		ul, ok := cfg.Underlying(name)
		if !ok {
			warn = append(warn, fmt.Sprintf("%s: not a configured underlying", name))
			continue
		}
		var idx refdata.Inst
		exch, sym, _ := strings.Cut(ul.Index, ":")
		if in, ok := refdata.BySymbol(insts, exch, sym); ok {
			idx = in
		} else {
			warn = append(warn, fmt.Sprintf("%s: index %q not in instruments", name, ul.Index))
		}
		futs := refdata.Futures(insts, ul.DerivExchange, ul.Name, today)
		exps := refdata.Expiries(insts, ul.DerivExchange, ul.Name, today)
		if len(exps) == 0 {
			warn = append(warn, fmt.Sprintf("%s: no live option expiries", name))
		}
		for i, exp := range exps {
			rows := byExp[exKey{ul.DerivExchange + "|" + ul.Name, exp.Format("2006-01-02")}]
			keep, n := choose(ul, refdata.Strikes(rows))
			if n >= 0 && i >= n {
				break
			}
			in := make(map[float64]bool, len(keep))
			for _, k := range keep {
				in[k] = true
			}
			t := Target{Und: ul.Name, Exch: ul.DerivExchange, Expiry: exp, Index: idx, Fut: refdata.FutureFor(futs, exp)}
			for _, r := range rows {
				if in[r.Key.Strike] {
					t.Opts = append(t.Opts, r)
				}
			}
			sort.Slice(t.Opts, func(a, b int) bool {
				if t.Opts[a].Key.Strike != t.Opts[b].Key.Strike {
					return t.Opts[a].Key.Strike < t.Opts[b].Key.Strike
				}
				return t.Opts[a].Key.Kind < t.Opts[b].Key.Kind // CE before PE
			})
			out = append(out, t)
		}
	}
	return out, warn
}

// Keys lists the distinct quote keys of targets in a stable order: indices and
// futures first, so a partial failure still prices the chains that did come back.
func Keys(ts []Target) []string {
	seen := map[string]bool{}
	var out []string
	add := func(in refdata.Inst) {
		if in.Token == 0 {
			return
		}
		if k := Key(in); !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for _, t := range ts {
		add(t.Index)
		add(t.Fut)
	}
	for _, t := range ts {
		for _, o := range t.Opts {
			add(o)
		}
	}
	return out
}

// Stats summarises one snapshot.
type Stats struct {
	Keys, Calls, Failed, Missing, Rows int
	Took                               time.Duration
}

func (s Stats) String() string {
	return fmt.Sprintf("%d keys in %d calls (%d failed), %d rows, %d missing, %s",
		s.Keys, s.Calls, s.Failed, s.Rows, s.Missing, s.Took.Round(time.Millisecond))
}

// RetryAfter is the pause before a failed batch is tried again.
var RetryAfter = 2 * time.Second

// Snap quotes every target in batches of kite.MaxQuoteKeys and builds one chain per
// target stamped with minute. A failed batch is retried once and then skipped; a
// token error stops the snapshot. It errors only when nothing came back.
func Snap(ctx context.Context, q Quoter, ts []Target, minute time.Time, source string) ([]bus.Chain, Stats, error) {
	start := time.Now()
	keys := Keys(ts)
	st := Stats{Keys: len(keys)}
	quotes := make(map[string]kite.Quote, len(keys))
	fetched := make(map[string]int64, len(keys))
	var lastErr error
	for lo := 0; lo < len(keys); lo += kite.MaxQuoteKeys {
		batch := keys[lo:min(lo+kite.MaxQuoteKeys, len(keys))]
		st.Calls++
		res, err := q.Quotes(ctx, batch)
		if err != nil && !kite.IsTokenError(err) && ctx.Err() == nil {
			select {
			case <-ctx.Done():
			case <-time.After(RetryAfter):
				st.Calls++
				res, err = q.Quotes(ctx, batch)
			}
		}
		if err != nil {
			if kite.IsTokenError(err) || ctx.Err() != nil {
				st.Took = time.Since(start)
				return nil, st, err
			}
			st.Failed++
			lastErr = err
			continue
		}
		at := bus.Ms(clock.Now())
		for k, v := range res {
			quotes[k], fetched[k] = v, at
		}
	}
	st.Took = time.Since(start)
	if len(keys) > 0 && len(quotes) == 0 {
		if lastErr == nil {
			lastErr = fmt.Errorf("sweep: no quotes returned for %d keys", len(keys))
		}
		return nil, st, lastErr
	}

	ms := bus.Ms(minute)
	var out []bus.Chain
	for _, t := range ts {
		c := bus.Chain{TS: ms, Source: source, Und: t.Und, Expiry: t.Expiry.Format("2006-01-02")}
		if t.Index.Token != 0 {
			c.Spot = quotes[Key(t.Index)].LTP
		}
		if t.Fut.Token != 0 {
			c.Fut = quotes[Key(t.Fut)].LTP
		}
		for _, o := range t.Opts {
			k := Key(o)
			qt, ok := quotes[k]
			if !ok {
				st.Missing++
				continue
			}
			c.Rows = append(c.Rows, Row(o, qt, fetched[k]))
		}
		if len(c.Rows) > 0 {
			st.Rows += len(c.Rows)
			out = append(out, c)
		}
	}
	return out, st, nil
}

// Row converts one REST quote, fetched at fetchedMs, into a chain row. Ages are
// measured from the fetch, not the minute, since a sweep takes several seconds.
func Row(o refdata.Inst, q kite.Quote, fetchedMs int64) bus.ChainRow {
	typ := "CE"
	if o.Key.Kind == contract.PE {
		typ = "PE"
	}
	r := bus.ChainRow{
		C: o.Contract, T: o.Token, Strike: o.Key.Strike, Type: typ,
		Bid: q.Bid, Ask: q.Ask, BidQ: uint32(q.BidQ), AskQ: uint32(q.AskQ),
		LTP: q.LTP, LTQ: uint32(q.LTQ), QuoteAge: -1, TradeAge: -1,
		OI: uint64(max(0, q.OI)), Vol: uint64(max(0, q.Vol)), Avg: q.Avg,
		TBQ: uint64(max(0, q.TBQ)), TSQ: uint64(max(0, q.TSQ)),
	}
	if !q.Timestamp.IsZero() {
		r.Exch = bus.Ms(q.Timestamp)
		r.QuoteAge = max(0, fetchedMs-r.Exch)
	}
	if !q.LTT.IsZero() {
		r.LTT = bus.Ms(q.LTT)
		r.TradeAge = max(0, fetchedMs-r.LTT)
	}
	return r
}

// Publish puts chains on the CHAIN stream for the recorder, returning how many
// were accepted.
func Publish(ctx context.Context, js jetstream.JetStream, chains []bus.Chain) (int, error) {
	n := 0
	var first error
	for _, c := range chains {
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := bus.PublishChain(pctx, js, c)
		cancel()
		if err != nil {
			if first == nil {
				first = fmt.Errorf("sweep: publish %s %s: %w", c.Und, c.Expiry, err)
			}
			continue
		}
		n++
	}
	return n, first
}
