// Package riskmon watches the riskengine's risk.book and the broker's margins, keeps
// the day's P&L line with its peak and drawdown, writes a snapshot a minute, and
// raises alerts against risk.limits. It only reads the book: there is no order code
// here, and alerts carry figures, never account IDs or position details.
package riskmon

import (
	"fmt"
	"math"
	"time"

	"pmt/internal/bus"
	"pmt/internal/config"
)

// RiskBook is the part of risk.book riskmon reads.
type RiskBook struct {
	TS      int64     `json:"ts"`
	BookTS  int64     `json:"book_ts"`
	Session bool      `json:"session"`
	RefPx   *float64  `json:"ref_px"`
	Steps   []float64 `json:"steps"`
	Legs    []struct {
		ID string `json:"id"`
	} `json:"legs"`
	Unmod []struct {
		ID string `json:"id"`
	} `json:"unmod"`
	Book struct {
		D     float64 `json:"d"`
		Dn    float64 `json:"dn"`
		G     float64 `json:"g"`
		V     float64 `json:"v"`
		T     float64 `json:"t"`
		Flags []struct {
			Leg string `json:"leg"`
			Why string `json:"why"`
		} `json:"flags"`
		Grid struct {
			Rows []struct {
				Liq []*float64 `json:"liq"`
			} `json:"rows"`
			Path struct {
				Liq []*float64 `json:"liq"`
			} `json:"path"`
		} `json:"grid"`
	} `json:"book"`
	PnL *struct {
		M2M   float64 `json:"m2m"`
		PnL   float64 `json:"pnl"`
		BM2M  float64 `json:"bm2m"`
		BPnL  float64 `json:"bpnl"`
		Live  int     `json:"live"`
		Stale int     `json:"stale"`
	} `json:"pnl"`
}

// Worst is the lowest liquidation P&L on the grid (every vol row and the spot-vol
// path) among cells within maxMove % of spot; nil when the grid is empty.
func (rb *RiskBook) Worst(maxMove float64) *float64 {
	var w *float64
	take := func(liq []*float64) {
		for i, v := range liq {
			if v == nil || i >= len(rb.Steps) || math.Abs(rb.Steps[i]) > maxMove+1e-9 {
				continue
			}
			if w == nil || *v < *w {
				x := *v
				w = &x
			}
		}
	}
	for _, r := range rb.Book.Grid.Rows {
		take(r.Liq)
	}
	take(rb.Book.Grid.Path.Liq)
	return w
}

// Level is an alert's severity; the zero value is clear.
type Level int

const (
	OK Level = iota
	Warn
	Crit
)

func (l Level) String() string { return [...]string{"ok", "warn", "crit"}[l] }

// clearAt is the hysteresis: a level holds until its measure falls below this
// fraction of the threshold that raised it, so a figure hovering on a line does not
// flap.
const clearAt = 0.9

// Rule is one alert's live state, as published on risk.mon.
type Rule struct {
	Rule  string  `json:"rule"`
	Level string  `json:"lvl"`
	Since int64   `json:"since,omitempty"` // when it reached its current level, ms
	Msg   string  `json:"msg,omitempty"`
	Value float64 `json:"v"` // the measure, in units of the warn threshold (1 = at warn)

	lvl Level
}

// Change is a level transition to send.
type Change struct {
	Rule string
	From Level
	To   Level
	Msg  string
}

// Snap is one minute of the day's line.
type Snap struct {
	TS    int64    `json:"ts"`
	M2M   float64  `json:"m2m"`
	PnL   float64  `json:"pnl"`
	RefPx *float64 `json:"px"`
	D     float64  `json:"d"`
	Dn    float64  `json:"dn"`
	G     float64  `json:"g"`
	V     float64  `json:"v"`
	T     float64  `json:"t"`
	Worst *float64 `json:"worst"`
	Util  *float64 `json:"util"`
	Legs  int      `json:"legs"`
	Flags int      `json:"flags"`
}

// Mon is the live state on risk.mon, every second.
type Mon struct {
	TS      int64    `json:"ts"`
	Session bool     `json:"session"`
	Has     bool     `json:"has"`       // a risk.book has arrived today
	M2M     float64  `json:"m2m"`       // today's, at live ticks where fresh
	PnL     float64  `json:"pnl"`       // since each position opened
	BM2M    float64  `json:"bm2m"`      // the broker's own figure
	Live    int      `json:"live"`      // open legs priced off a fresh tick
	Stale   int      `json:"stale"`     // open legs left at the broker's price
	Peak    *float64 `json:"peak"`      // today's best M2M
	DD      float64  `json:"dd"`        // peak - m2m
	MaxDD   float64  `json:"maxdd"`     // the day's worst drawdown so far
	Limit   float64  `json:"limit"`     // the daily loss limit, rupees
	LimUsed *float64 `json:"lim_used"`  // -m2m / limit, 1 = at the limit
	Capital float64  `json:"cap"`       // the base for worst_pct
	Worst   *float64 `json:"worst"`     // worst liquidation cell within worst_move
	WMove   float64  `json:"wmove"`     // that move, %
	WPct    float64  `json:"wpct"`      // the worst_pct threshold
	Util    *float64 `json:"util"`      // margin used / (used + free)
	Used    float64  `json:"used"`      // margin used
	Free    float64  `json:"free"`      // margin free
	Dn      float64  `json:"dn"`        // net delta, NIFTY lots
	Band    float64  `json:"band"`      // the delta band, lots
	BookAge *int64   `json:"book_age"`  // ms since risk.book
	MgAge   *int64   `json:"mg_age"`    // ms since book.margins
	Rules   []Rule   `json:"rules"`
}

// Monitor holds the day's state. It is not safe for concurrent use; the service
// serialises calls.
type Monitor struct {
	L config.RiskLimits

	day   time.Time // trade date the peak and line belong to
	rb    *RiskBook
	rbAt  time.Time // when it arrived
	mg    *bus.Margins
	mgAt  time.Time
	peak  *float64
	maxDD float64
	snaps []Snap

	flagSince time.Time // a flagged or unmodelled leg has been present since
	rules     map[string]*Rule
}

// Rule names, stable: they key the dedupe and the alerts table.
const (
	RuleLoss     = "risk_loss_limit"
	RuleWorst    = "risk_worst_cell"
	RuleMargin   = "risk_margin"
	RuleDelta    = "risk_delta_band"
	RuleDrawdown = "risk_drawdown"
	RuleStale    = "risk_stale"
	RuleFlags    = "risk_legs_flagged"
)

var ruleOrder = []string{RuleLoss, RuleWorst, RuleMargin, RuleDelta, RuleDrawdown, RuleStale, RuleFlags}

func New(l config.RiskLimits) *Monitor {
	m := &Monitor{L: l, rules: map[string]*Rule{}}
	for _, r := range ruleOrder {
		m.rules[r] = &Rule{Rule: r, Level: OK.String()}
	}
	return m
}

// Restore seeds the day from snapshots already written (after a restart).
func (m *Monitor) Restore(day time.Time, snaps []Snap) {
	m.rollTo(day)
	for _, s := range snaps {
		m.track(s.M2M)
	}
	m.snaps = append(m.snaps[:0], snaps...)
}

func (m *Monitor) rollTo(day time.Time) {
	if m.day.Equal(day) {
		return
	}
	m.day, m.peak, m.maxDD, m.snaps = day, nil, 0, nil
}

func (m *Monitor) track(m2m float64) {
	if m.peak == nil || m2m > *m.peak {
		x := m2m
		m.peak = &x
	}
	m.maxDD = math.Max(m.maxDD, *m.peak-m2m)
}

// OnBook takes a risk.book; day is now's trade date.
func (m *Monitor) OnBook(rb *RiskBook, now, day time.Time) {
	m.rollTo(day)
	m.rb, m.rbAt = rb, now
	if rb.PnL != nil {
		m.track(rb.PnL.M2M)
	}
	if len(rb.Book.Flags)+len(rb.Unmod) == 0 {
		m.flagSince = time.Time{}
	} else if m.flagSince.IsZero() {
		m.flagSince = now
	}
}

func (m *Monitor) OnMargins(mg *bus.Margins, now time.Time) { m.mg, m.mgAt = mg, now }

func (m *Monitor) util() (*float64, float64, float64) {
	if m.mg == nil || !m.mg.Enabled {
		return nil, 0, 0
	}
	used, free := m.mg.Debits, m.mg.Net
	if used+free <= 0 {
		return nil, used, free
	}
	u := used / (used + free)
	return &u, used, free
}

func ageMs(now, at time.Time) *int64 {
	if at.IsZero() {
		return nil
	}
	a := now.Sub(at).Milliseconds()
	return &a
}

// State is the live risk.mon payload. Rules carry the levels from the last Eval.
func (m *Monitor) State(now time.Time, session bool) Mon {
	l := m.L
	s := Mon{
		TS: bus.Ms(now), Session: session, Limit: l.LossLimit, Capital: l.Capital,
		WMove: l.WorstMove, WPct: l.WorstPct, Band: l.DeltaBand, Peak: m.peak, MaxDD: m.maxDD,
		BookAge: ageMs(now, m.rbAt), MgAge: ageMs(now, m.mgAt),
	}
	s.Util, s.Used, s.Free = m.util()
	if rb := m.rb; rb != nil {
		s.Dn = rb.Book.Dn
		s.Worst = rb.Worst(l.WorstMove)
		if p := rb.PnL; p != nil {
			s.Has = true
			s.M2M, s.PnL, s.BM2M, s.Live, s.Stale = p.M2M, p.PnL, p.BM2M, p.Live, p.Stale
			if m.peak != nil {
				s.DD = *m.peak - p.M2M
			}
			if l.LossLimit > 0 {
				u := -p.M2M / l.LossLimit
				s.LimUsed = &u
			}
		}
	}
	for _, r := range ruleOrder {
		s.Rules = append(s.Rules, *m.rules[r])
	}
	return s
}

// level applies the hysteresis: x is the measure over the warn threshold, crit the
// crit threshold in the same units (0 when the rule has no crit).
func level(cur Level, x, crit float64) Level {
	up := OK
	switch {
	case crit > 0 && x >= crit:
		up = Crit
	case x >= 1:
		up = Warn
	}
	if up >= cur {
		return up
	}
	// Falling: hold each level until x is below clearAt of its threshold.
	if cur == Crit && x >= crit*clearAt {
		return Crit
	}
	if cur >= Warn && x >= clearAt {
		return Warn
	}
	return up
}

func inr(v float64) string {
	neg := v < 0
	n := int64(math.Round(math.Abs(v)))
	s := fmt.Sprint(n)
	// Indian grouping: last three, then pairs.
	if len(s) > 3 {
		head, tail := s[:len(s)-3], s[len(s)-3:]
		out := ""
		for len(head) > 2 {
			out = "," + head[len(head)-2:] + out
			head = head[:len(head)-2]
		}
		s = head + out + "," + tail
	}
	if neg {
		return "-" + s
	}
	return s
}

// Eval moves each rule to its new level and returns the transitions to send. Out of
// session every rule clears quietly: the alerts are for the trading day, and the
// next open re-raises anything still true.
func (m *Monitor) Eval(now time.Time, session bool) []Change {
	type want struct {
		x, crit float64
		on      bool
		msg     string
	}
	l := m.L
	st := m.State(now, session)
	w := map[string]want{}

	if st.Has && l.LossLimit > 0 {
		x := -st.M2M / (l.LossLimit * l.LossWarn)
		w[RuleLoss] = want{x, 1 / l.LossWarn, true,
			fmt.Sprintf("Day M2M %s is %.0f%% of the %s loss limit", inr(st.M2M), 100*-st.M2M/l.LossLimit, inr(l.LossLimit))}
	}
	if st.Worst != nil && l.WorstPct > 0 && l.Capital > 0 {
		pct := -*st.Worst / l.Capital * 100
		w[RuleWorst] = want{pct / l.WorstPct, 0, true,
			fmt.Sprintf("Worst scenario within ±%g%% loses %s, %.1f%% of capital (limit %g%%)", l.WorstMove, inr(-*st.Worst), pct, l.WorstPct)}
	}
	if st.Util != nil && l.MarginWarn > 0 {
		w[RuleMargin] = want{*st.Util / l.MarginWarn, l.MarginCrit / l.MarginWarn, true,
			fmt.Sprintf("Margin used %.0f%% (warn %.0f%%, crit %.0f%%)", 100**st.Util, 100*l.MarginWarn, 100*l.MarginCrit)}
	}
	if m.rb != nil && l.DeltaBand > 0 {
		w[RuleDelta] = want{math.Abs(st.Dn) / l.DeltaBand, 0, true,
			fmt.Sprintf("Net delta %+.2f NIFTY lots, outside ±%g", st.Dn, l.DeltaBand)}
	}
	if st.Has && l.Drawdown > 0 {
		w[RuleDrawdown] = want{st.DD / l.Drawdown, 0, true,
			fmt.Sprintf("M2M %s is %s off the day's peak", inr(st.M2M), inr(st.DD))}
	}
	if l.Stale > 0 {
		// Stale inputs are all-or-nothing: crit as soon as either feed stops.
		var what []string
		for _, f := range []struct {
			name string
			age  *int64
		}{{"risk.book", st.BookAge}, {"book.margins", st.MgAge}} {
			if f.age == nil || time.Duration(*f.age)*time.Millisecond > l.Stale {
				what = append(what, f.name)
			}
		}
		x := 0.0
		msg := ""
		if len(what) > 0 {
			x = 1
			msg = fmt.Sprintf("Risk inputs stale: %v (no update for %s)", what, l.Stale)
		}
		w[RuleStale] = want{x, 1, true, msg}
	}
	if l.FlagAfter > 0 && m.rb != nil {
		x := 0.0
		msg := ""
		n := len(m.rb.Book.Flags) + len(m.rb.Unmod)
		if !m.flagSince.IsZero() && now.Sub(m.flagSince) >= l.FlagAfter {
			x = 1
			msg = fmt.Sprintf("%d leg(s) flagged or unmodelled for %s; the risk figures may be off", n, now.Sub(m.flagSince).Round(time.Second))
		}
		w[RuleFlags] = want{x, 0, true, msg}
	}

	var out []Change
	for _, name := range ruleOrder {
		r := m.rules[name]
		wt, ok := w[name]
		to := OK
		if session && ok && wt.on {
			to = level(r.lvl, wt.x, wt.crit)
		}
		r.Value = math.Round(wt.x*1000) / 1000
		if to != OK {
			r.Msg = wt.msg
		}
		if to == r.lvl {
			continue
		}
		if session {
			msg := r.Msg
			if to == OK {
				msg = "cleared"
				if wt.msg != "" && name != RuleStale && name != RuleFlags {
					msg = "cleared: " + wt.msg
				}
			}
			out = append(out, Change{Rule: name, From: r.lvl, To: to, Msg: msg})
		}
		r.lvl, r.Level, r.Since = to, to.String(), bus.Ms(now)
		if to == OK {
			r.Msg, r.Since = "", 0
		}
	}
	return out
}

// Snapshot is the minute's row, or false when there is no fresh risk.book with a
// P&L to take it from.
func (m *Monitor) Snapshot(now time.Time) (Snap, bool) {
	rb := m.rb
	if rb == nil || rb.PnL == nil || (m.L.Stale > 0 && now.Sub(m.rbAt) > m.L.Stale) {
		return Snap{}, false
	}
	u, _, _ := m.util()
	b := rb.Book
	return Snap{
		TS: bus.Ms(now), M2M: rb.PnL.M2M, PnL: rb.PnL.PnL, RefPx: rb.RefPx,
		D: b.D, Dn: b.Dn, G: b.G, V: b.V, T: b.T, Worst: rb.Worst(m.L.WorstMove), Util: u,
		Legs: len(rb.Legs), Flags: len(b.Flags) + len(rb.Unmod),
	}, true
}

// Add appends a minute to the day's line.
func (m *Monitor) Add(s Snap) { m.snaps = append(m.snaps, s) }

// Day is the risk.day payload: today's line.
type Day struct {
	Date  string `json:"date"`
	Snaps []Snap `json:"rows"`
}

func (m *Monitor) Day() Day {
	return Day{Date: m.day.Format(time.DateOnly), Snaps: append([]Snap(nil), m.snaps...)}
}
