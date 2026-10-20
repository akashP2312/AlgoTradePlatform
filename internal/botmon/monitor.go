package botmon

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"pmt/internal/bus"
	"pmt/internal/calendar"
	"pmt/internal/clock"
	"pmt/internal/config"
)

// Level is an alert's severity; the zero value is clear.
type Level int

const (
	OK Level = iota
	Warn
	Crit
)

func (l Level) String() string { return [...]string{"ok", "warn", "crit"}[l] }

const (
	RuleHeartbeat = "bot_heartbeat" // the daily run did not happen
	RuleLedger    = "bot_ledger"    // the ledger cannot be read
	RuleRecon     = "bot_recon"     // ledger and broker disagree (live mode only)

	failAfter = 3 // consecutive read failures before an alert
)

var ruleOrder = []string{RuleHeartbeat, RuleLedger, RuleRecon}

// Rule is one alert's live state, as published in bot.<name>.state.
type Rule struct {
	Rule  string `json:"rule"`
	Level string `json:"lvl"`
	Since int64  `json:"since,omitempty"` // when it reached its current level, ms
	Msg   string `json:"msg,omitempty"`

	lvl Level
}

// Change is a level transition to send.
type Change struct {
	Rule string
	From Level
	To   Level
	Msg  string
}

// Recon statuses.
const (
	ReconNotCompared = "not_compared" // paper mode: there is nothing at the broker to compare
	ReconWaiting     = "waiting"      // live mode, no broker read yet
	ReconOK          = "ok"
	ReconMismatch    = "mismatch"
)

// ReconItem is one symbol where the ledger and the broker differ.
type ReconItem struct {
	Sym    string `json:"sym"`
	Ledger int64  `json:"ledger"`
	Broker int64  `json:"broker"`
	Note   string `json:"note"` // pending | mismatch | extra
}

type Recon struct {
	Status string      `json:"status"`
	At     int64       `json:"at,omitempty"` // broker read time, ms
	Msg    string      `json:"msg,omitempty"`
	Items  []ReconItem `json:"items,omitempty"`
}

// Pos is a ledger position with the last run's mark. Distance to stop is display
// only: botmon never acts on it.
type Pos struct {
	LPos
	Mark   *float64 `json:"mark,omitempty"`
	PnLPct *float64 `json:"pnl_pct,omitempty"`
	Value  *float64 `json:"value,omitempty"`
	PnL    *float64 `json:"pnl,omitempty"`
	ToStop *float64 `json:"to_stop,omitempty"` // % the mark sits above the stop
}

// State is what botmon publishes on bot.<name>.state.
type State struct {
	TS         int64    `json:"ts"`
	Name       string   `json:"name"`
	Mode       string   `json:"mode"`
	ReadAt     int64    `json:"read_at,omitempty"`
	Err        string   `json:"err,omitempty"`
	AsOf       string   `json:"as_of,omitempty"`  // the last day the bot ran for
	Expected   string   `json:"expected"`         // the day it should have run for by now
	Behind     int      `json:"behind"`           // trading days missed
	Cash       *float64 `json:"cash,omitempty"`
	Equity     *float64 `json:"equity,omitempty"`
	MonthStart *float64 `json:"month_start,omitempty"`
	MonthPct   *float64 `json:"month_pct,omitempty"`
	Positions  []Pos    `json:"positions"`
	Pending    []LOrder `json:"pending"`
	Trades     []LTrade `json:"trades"`
	Curve      []EqPt   `json:"curve"`
	Realised   float64  `json:"realised"`
	NTrades    int      `json:"n_trades"`
	Recon      Recon    `json:"recon"`
	Rules      []Rule   `json:"rules"`
}

// Monitor holds the latest ledger and broker reads and the alert levels. It is not
// safe for concurrent use; the service locks around it.
type Monitor struct {
	B   config.Bot
	Cal *calendar.Calendar

	l       *Ledger
	readAt  time.Time
	readErr string
	fails   int

	broker      map[string]int64 // live mode: tradingsymbol -> qty held at the broker
	brokerAt    time.Time
	brokerErr   string
	brokerFails int

	rules map[string]*Rule
}

func New(b config.Bot, cal *calendar.Calendar) *Monitor {
	m := &Monitor{B: b, Cal: cal, rules: map[string]*Rule{}}
	for _, r := range ruleOrder {
		m.rules[r] = &Rule{Rule: r, Level: OK.String()}
	}
	return m
}

func (m *Monitor) Live() bool { return m.B.Mode == "live" }

// OnLedger records one ledger read. A busy ledger (the bot is writing) is not a
// failure; the last good read stands.
func (m *Monitor) OnLedger(l *Ledger, err error, now time.Time) {
	switch {
	case err == nil:
		m.l, m.readAt, m.readErr, m.fails = l, now, "", 0
	case err == ErrBusy:
	default:
		m.readErr = err.Error()
		m.fails++
	}
}

// OnBroker records one broker read: qty per tradingsymbol (live mode only).
func (m *Monitor) OnBroker(qty map[string]int64, err error, now time.Time) {
	if err != nil {
		m.brokerErr = err.Error()
		m.brokerFails++
		return
	}
	m.broker, m.brokerAt, m.brokerErr, m.brokerFails = qty, now, "", 0
}

// Expected is the trading day the bot should have run for by now: today once
// run_at + grace has passed, else the trading day before.
func (m *Monitor) Expected(now time.Time) time.Time {
	today := clock.Date(now)
	d := m.Cal.OnOrBefore(today)
	if d.Equal(today) {
		if at, err := clock.AtHMS(d, m.B.RunAt); err == nil && now.Before(at.Add(m.B.Grace)) {
			d = m.Cal.Prev(d)
		}
	}
	return d
}

// asOf is the ledger's last_asof day, or false when the bot has never run.
func (m *Monitor) asOf() (time.Time, bool) {
	if m.l == nil {
		return time.Time{}, false
	}
	s := m.l.Meta["last_asof"]
	if len(s) > 10 {
		s = s[:10]
	}
	t, err := time.ParseInLocation("2006-01-02", s, clock.IST)
	return t, err == nil
}

// Behind counts the trading days in (asof, expected]: runs the bot has missed.
func (m *Monitor) Behind(now time.Time) (int, bool) {
	asof, ok := m.asOf()
	if !ok {
		return 0, false
	}
	exp := m.Expected(now)
	if !asof.Before(exp) {
		return 0, true
	}
	return len(m.Cal.TradingDays(asof.AddDate(0, 0, 1), exp)), true
}

// Recon compares the ledger's quantities with the broker's. Only symbols the ledger
// holds or has an open order for are compared: the account can hold other stock the
// bot does not own. A symbol with an open order is "pending" (the fill is still to
// land on one side); a ledger quantity the broker does not hold is a mismatch; the
// broker holding more than the ledger is "extra", noted but not an alert.
func (m *Monitor) Recon() Recon {
	if !m.Live() {
		return Recon{Status: ReconNotCompared, Msg: "paper mode: the ledger is the only book"}
	}
	if m.broker == nil {
		r := Recon{Status: ReconWaiting, Msg: "no broker holdings read yet"}
		if m.brokerErr != "" {
			r.Msg = m.brokerErr
		}
		return r
	}
	if m.l == nil {
		return Recon{Status: ReconWaiting, Msg: "no ledger read yet", At: bus.Ms(m.brokerAt)}
	}
	led := map[string]int64{}
	for _, p := range m.l.Positions {
		led[p.Sym] += p.Qty
	}
	pend := map[string]bool{}
	for _, o := range m.l.Pending {
		pend[o.Sym] = true
	}
	syms := map[string]bool{}
	for s := range led {
		syms[s] = true
	}
	for s := range pend {
		syms[s] = true
	}
	var items []ReconItem
	bad := 0
	for s := range syms {
		lq, bq := led[s], m.broker[s]
		if lq == bq {
			continue
		}
		it := ReconItem{Sym: s, Ledger: lq, Broker: bq}
		switch {
		case pend[s]:
			it.Note = "pending"
		case bq < lq:
			it.Note = "mismatch"
			bad++
		default:
			it.Note = "extra"
		}
		items = append(items, it)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Sym < items[j].Sym })
	r := Recon{Status: ReconOK, At: bus.Ms(m.brokerAt), Items: items}
	if bad > 0 {
		r.Status = ReconMismatch
		var parts []string
		for _, it := range items {
			if it.Note == "mismatch" {
				parts = append(parts, fmt.Sprintf("%s ledger %d broker %d", it.Sym, it.Ledger, it.Broker))
			}
		}
		r.Msg = strings.Join(parts, "; ")
	}
	return r
}

// Eval moves each rule to its new level and returns the transitions to send.
func (m *Monitor) Eval(now time.Time) []Change {
	type want struct {
		lvl Level
		msg string
	}
	w := map[string]want{}

	if m.l != nil {
		exp := m.Expected(now).Format("2006-01-02")
		switch n, ran := m.Behind(now); {
		case !ran:
			w[RuleHeartbeat] = want{Crit, fmt.Sprintf("%s has never completed a run (expected %s)", m.B.Name, exp)}
		case n == 1:
			w[RuleHeartbeat] = want{Warn, fmt.Sprintf("%s missed its run for %s (last %s)", m.B.Name, exp, m.l.Meta["last_asof"])}
		case n > 1:
			w[RuleHeartbeat] = want{Crit, fmt.Sprintf("%s is %d runs behind: last %s, expected %s", m.B.Name, n, m.l.Meta["last_asof"], exp)}
		}
	}
	if m.fails >= failAfter {
		w[RuleLedger] = want{Warn, fmt.Sprintf("%d ledger reads failed: %s", m.fails, m.readErr)}
	}
	if m.Live() {
		switch rc := m.Recon(); {
		case rc.Status == ReconMismatch:
			w[RuleRecon] = want{Warn, "ledger and broker differ: " + rc.Msg}
		case m.brokerFails >= failAfter:
			w[RuleRecon] = want{Warn, fmt.Sprintf("%d broker reads failed: %s", m.brokerFails, m.brokerErr)}
		}
	}

	var out []Change
	for _, name := range ruleOrder {
		r := m.rules[name]
		to := w[name]
		if to.lvl != OK {
			r.Msg = to.msg
		}
		if to.lvl == r.lvl {
			continue
		}
		msg := r.Msg
		if to.lvl == OK {
			msg = "cleared"
		}
		out = append(out, Change{Rule: name, From: r.lvl, To: to.lvl, Msg: msg})
		r.lvl, r.Level, r.Since = to.lvl, to.lvl.String(), bus.Ms(now)
		if to.lvl == OK {
			r.Msg, r.Since = "", 0
		}
	}
	return out
}

func f(v float64) *float64 { return &v }

func round(v float64, n int) float64 {
	p := math.Pow(10, float64(n))
	return math.Round(v*p) / p
}

// State is the snapshot to publish.
func (m *Monitor) State(now time.Time) State {
	s := State{
		TS: bus.Ms(now), Name: m.B.Name, Mode: m.B.Mode, Err: m.readErr,
		Expected:  m.Expected(now).Format("2006-01-02"),
		Positions: []Pos{}, Pending: []LOrder{}, Trades: []LTrade{}, Curve: []EqPt{},
		Recon: m.Recon(),
	}
	for _, name := range ruleOrder {
		s.Rules = append(s.Rules, *m.rules[name])
	}
	l := m.l
	if l == nil {
		return s
	}
	s.ReadAt = bus.Ms(m.readAt)
	s.AsOf = l.Meta["last_asof"]
	s.Behind, _ = m.Behind(now)
	if l.Pending != nil {
		s.Pending = l.Pending
	}
	if l.Trades != nil {
		s.Trades = l.Trades
	}
	if l.Curve != nil {
		s.Curve = l.Curve
	}
	s.Realised, s.NTrades = round(l.Realised, 2), l.NTrades
	if v, ok := l.MetaFloat("cash"); ok {
		s.Cash = f(round(v, 2))
	}
	marks := l.Marks()
	value, marked := 0.0, true
	for _, p := range l.Positions {
		ps := Pos{LPos: p}
		if pct, ok := marks[p.Sym]; ok && p.Entry > 0 {
			mk := p.Entry * (1 + pct/100)
			ps.Mark, ps.PnLPct = f(round(mk, 2)), f(round(pct, 2))
			ps.Value = f(round(mk*float64(p.Qty), 2))
			ps.PnL = f(round((mk-p.Entry)*float64(p.Qty), 2))
			if p.Stop > 0 {
				ps.ToStop = f(round((mk/p.Stop-1)*100, 2))
			}
			value += mk * float64(p.Qty)
		} else {
			marked = false
		}
		s.Positions = append(s.Positions, ps)
	}
	// Equity is cash plus the marked book; without a mark for every position fall
	// back to the bot's own last equity row.
	switch {
	case s.Cash != nil && marked:
		s.Equity = f(round(*s.Cash+value, 2))
	case len(l.Curve) > 0:
		s.Equity = f(l.Curve[len(l.Curve)-1].Equity)
	}
	if v, ok := l.MetaFloat("month_start_equity"); ok && v > 0 {
		s.MonthStart = f(round(v, 2))
		if s.Equity != nil {
			s.MonthPct = f(round((*s.Equity/v-1)*100, 2))
		}
	}
	return s
}
