// Package ticker streams the configured universe over Kite's WebSocket, publishes
// every tick on NATS, keeps the last-value KV current and cuts minute-aligned
// option chain snapshots.
package ticker

import (
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"pmt/internal/config"
	"pmt/internal/contract"
	"pmt/internal/refdata"
)

// ChainSpec is one recorded expiry of one underlying.
type ChainSpec struct {
	Und     string
	Exch    string
	Expiry  time.Time
	Strikes []float64          // ascending, the subscribed window
	CE, PE  map[float64]uint32 // strike -> token
	Fut     uint32             // future of the same expiry, else the nearest; 0 if none
}

// Universe is everything the ticker subscribes to, with the reason for each token.
type Universe struct {
	Modes   map[uint32]byte // token -> 'f' | 'q'
	Insts   map[uint32]refdata.Inst
	Chains  []ChainSpec
	Index   map[string]uint32  // underlying -> index token
	Centre  map[string]float64 // underlying -> the spot the window was centred on
	Step    map[string]float64 // underlying -> strike step near the money
	Trimmed int                // strikes per side dropped to fit the token cap
	Warn    []string

	day string // IST date the universe was built for
}

// Count is the number of subscribed tokens.
func (u *Universe) Count() int { return len(u.Modes) }

// Build selects the universe from today's instruments. spot holds the latest index
// level per underlying; a missing spot centres the window on the median listed strike.
func Build(cfg *config.Config, insts []refdata.Inst, spot map[string]float64, today time.Time) *Universe {
	limit := cfg.Kite.TokensPerConn * cfg.Kite.WSConnections
	for trim := 0; ; trim++ {
		u := build(cfg, insts, spot, today, trim)
		if u.Count() <= limit || trim > 200 {
			if trim > 0 {
				u.Warn = append(u.Warn, fmt.Sprintf("universe trimmed by %d strikes per side to fit %d tokens", trim, limit))
			}
			return u
		}
	}
}

func build(cfg *config.Config, all []refdata.Inst, spot map[string]float64, today time.Time, trim int) *Universe {
	insts := refdata.Live(all, today)
	u := &Universe{Modes: map[uint32]byte{}, Insts: map[uint32]refdata.Inst{}, Index: map[string]uint32{},
		Centre: map[string]float64{}, Step: map[string]float64{}, Trimmed: trim}
	add := func(in refdata.Inst, mode byte) {
		if m, ok := u.Modes[in.Token]; ok && m == 'f' {
			return
		}
		u.Modes[in.Token], u.Insts[in.Token] = mode, in
	}

	// Group options by underlying and expiry once.
	type exKey struct{ und, exp string }
	byExp := map[exKey][]refdata.Inst{}
	for _, in := range insts {
		if in.Key.Kind.IsOption() {
			k := exKey{in.Exchange + "|" + in.Key.Name, in.Key.Expiry.Format("2006-01-02")}
			byExp[k] = append(byExp[k], in)
		}
	}

	for _, ul := range cfg.Universe.Underlyings {
		exch, sym, _ := strings.Cut(ul.Index, ":")
		if idx, ok := refdata.BySymbol(insts, exch, sym); ok {
			add(idx, 'f')
			u.Index[ul.Name] = idx.Token
		} else {
			u.Warn = append(u.Warn, fmt.Sprintf("%s: index %q not in instruments", ul.Name, ul.Index))
		}
		futs := refdata.Futures(insts, ul.DerivExchange, ul.Name, today)
		for i, f := range futs {
			if i < ul.Futures {
				add(f, 'f')
			}
		}
		exps := refdata.Expiries(insts, ul.DerivExchange, ul.Name, today)
		if len(exps) > ul.WideExpiries {
			exps = exps[:ul.WideExpiries]
		}
		width := ul.WideStrikes - trim
		if width < 1 {
			width = 1
		}
		for ei, exp := range exps {
			rows := byExp[exKey{ul.DerivExchange + "|" + ul.Name, exp.Format("2006-01-02")}]
			strikes := strikeList(rows)
			s, ok := spot[ul.Name]
			if !ok || s <= 0 {
				s = strikes[len(strikes)/2]
			}
			if ei == 0 {
				u.Centre[ul.Name], u.Step[ul.Name] = s, stepNear(strikes, s)
			}
			win := Window(strikes, s, width)
			cs := ChainSpec{Und: ul.Name, Exch: ul.DerivExchange, Expiry: exp, Strikes: win,
				CE: map[float64]uint32{}, PE: map[float64]uint32{}, Fut: refdata.FutureFor(futs, exp).Token}
			in := make(map[float64]bool, len(win))
			for _, k := range win {
				in[k] = true
			}
			for _, r := range rows {
				if !in[r.Key.Strike] {
					continue
				}
				add(r, 'f')
				if r.Key.Kind == contract.CE {
					cs.CE[r.Key.Strike] = r.Token
				} else {
					cs.PE[r.Key.Strike] = r.Token
				}
			}
			u.Chains = append(u.Chains, cs)
		}
	}

	for _, spec := range cfg.Universe.Extras {
		in, err := refdata.ResolveExtra(insts, spec, today)
		if err != nil {
			u.Warn = append(u.Warn, err.Error())
			continue
		}
		mode := byte('f')
		if in.Key.Kind == contract.EQ {
			mode = 'q'
		}
		add(in, mode)
	}
	for _, path := range cfg.Universe.Constituents {
		syms, err := refdata.ReadConstituents(cfg.Abs(path))
		if err != nil {
			u.Warn = append(u.Warn, fmt.Sprintf("constituents %s: %v", path, err))
			continue
		}
		for _, s := range syms {
			if in, ok := refdata.BySymbol(insts, "NSE", s); ok {
				add(in, 'q')
			} else {
				u.Warn = append(u.Warn, fmt.Sprintf("constituent NSE:%s not in instruments", s))
			}
		}
	}
	return u
}

func strikeList(rows []refdata.Inst) []float64 { return refdata.Strikes(rows) }

// Window is the listed strikes from n below the strike nearest spot to n above it.
func Window(strikes []float64, spot float64, n int) []float64 {
	return refdata.Window(strikes, spot, n)
}

func nearest(strikes []float64, spot float64) int { return refdata.Nearest(strikes, spot) }

// stepNear is the smallest strike interval around the money.
func stepNear(strikes []float64, spot float64) float64 {
	if len(strikes) < 2 {
		return 0
	}
	i := nearest(strikes, spot)
	step := math.Inf(1)
	for j := max(1, i-3); j <= min(len(strikes)-1, i+3); j++ {
		step = math.Min(step, strikes[j]-strikes[j-1])
	}
	return step
}

// NeedsRecentre reports whether spot has drifted far enough from the window centre.
func (u *Universe) NeedsRecentre(und string, spot float64, steps int) bool {
	c, step := u.Centre[und], u.Step[und]
	return c > 0 && step > 0 && steps > 0 && math.Abs(spot-c) >= float64(steps)*step
}

// Assign splits tokens over n connections. Tokens already placed keep their
// connection so a recentre only moves what changed; new tokens go to the least
// loaded connection.
func Assign(modes map[uint32]byte, prev map[uint32]int, n, perConn int) (map[uint32]int, []map[uint32]byte) {
	out := make(map[uint32]int, len(modes))
	sets := make([]map[uint32]byte, n)
	for i := range sets {
		sets[i] = map[uint32]byte{}
	}
	toks := make([]uint32, 0, len(modes))
	for t := range modes {
		toks = append(toks, t)
	}
	sort.Slice(toks, func(i, j int) bool { return toks[i] < toks[j] })
	var fresh []uint32
	for _, t := range toks {
		if c, ok := prev[t]; ok && c < n && len(sets[c]) < perConn {
			out[t], sets[c][t] = c, modes[t]
		} else {
			fresh = append(fresh, t)
		}
	}
	for _, t := range fresh {
		best := 0
		for i := range sets {
			if len(sets[i]) < len(sets[best]) {
				best = i
			}
		}
		if len(sets[best]) >= perConn {
			slog.Warn("ticker: token cap reached, dropping token", "token", t)
			continue
		}
		out[t], sets[best][t] = best, modes[t]
	}
	return out, sets
}
