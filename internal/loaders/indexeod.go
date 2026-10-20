package loaders

import (
	"context"
	"fmt"
	"strings"
	"time"

	"pmt/internal/clock"
	"pmt/internal/config"
	"pmt/internal/harvest"
	"pmt/internal/refdata"
)

// VIXSymbol is India VIX in the Kite instrument dump.
const VIXSymbol = "NSE:INDIA VIX"

// IndexTargets is India VIX plus each underlying's index, resolved to tokens.
// Symbols not in the dump are returned as warnings.
func IndexTargets(cfg *config.Config, insts []refdata.Inst) ([]refdata.Inst, []string) {
	specs := []string{VIXSymbol}
	for _, ul := range cfg.Universe.Underlyings {
		if ul.Index != "" {
			specs = append(specs, ul.Index)
		}
	}
	var out []refdata.Inst
	var warns []string
	seen := map[uint32]bool{}
	for _, s := range specs {
		exch, sym, ok := strings.Cut(s, ":")
		if !ok {
			warns = append(warns, fmt.Sprintf("index %q is not EXCHANGE:SYMBOL", s))
			continue
		}
		in, ok := refdata.BySymbol(insts, exch, sym)
		if !ok {
			warns = append(warns, fmt.Sprintf("index %s not in instruments", s))
			continue
		}
		if !seen[in.Token] {
			seen[in.Token] = true
			out = append(out, in)
		}
	}
	return out, warns
}

// IndexDays fetches day candles for targets over [from, to] (IST dates).
func IndexDays(ctx context.Context, h harvest.Historian, targets []refdata.Inst, from, to time.Time) ([]IndexBar, error) {
	start := clock.Date(from)
	end := clock.Date(to).Add(24*time.Hour - time.Second)
	var out []IndexBar
	for _, in := range targets {
		cs, err := h.Historical(ctx, in.Token, "day", start, end, false)
		if err != nil {
			return out, fmt.Errorf("%s: %w", in.Contract, err)
		}
		for _, c := range cs {
			o, hi, lo := c.O, c.H, c.L
			out = append(out, IndexBar{TradeDate: clock.Date(c.TS), Contract: in.Contract,
				Open: &o, High: &hi, Low: &lo, Close: c.C, Source: "kite"})
		}
	}
	return out, nil
}
