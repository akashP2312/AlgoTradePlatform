package riskmon

import (
	"encoding/json"
	"testing"
	"time"

	"pmt/internal/bus"
	"pmt/internal/clock"
	"pmt/internal/config"
)

func limits() config.RiskLimits {
	return config.RiskLimits{
		Capital: 1_000_000, LossLimit: 20_000, LossWarn: 0.75, WorstPct: 5, WorstMove: 5,
		MarginWarn: 0.8, MarginCrit: 0.9, DeltaBand: 2, Drawdown: 10_000,
		Stale: 30 * time.Second, FlagAfter: 2 * time.Minute,
	}
}

func book(t *testing.T, m2m, dn float64, flags int) *RiskBook {
	t.Helper()
	fl := make([]map[string]string, flags)
	for i := range fl {
		fl[i] = map[string]string{"leg": "x", "why": "no smile"}
	}
	raw := map[string]any{
		"ts": 1, "steps": []float64{-10, -5, 0, 5, 10},
		"book": map[string]any{
			"dn": dn, "flags": fl,
			"grid": map[string]any{
				"rows": []any{map[string]any{"liq": []any{-300000, -40000, 0, -30000, -250000}}},
				"path": map[string]any{"liq": []any{-320000, -60000, 0, nil, -200000}},
			},
		},
		"pnl": map[string]any{"m2m": m2m, "pnl": m2m},
	}
	b, _ := json.Marshal(raw)
	var rb RiskBook
	if err := json.Unmarshal(b, &rb); err != nil {
		t.Fatal(err)
	}
	return &rb
}

func lvl(m *Monitor, rule string) Level { return m.rules[rule].lvl }

func TestWorstSkipsCellsBeyondTheMove(t *testing.T) {
	rb := book(t, 0, 0, 0)
	if w := rb.Worst(5); w == nil || *w != -60000 {
		t.Fatalf("worst within 5%% = %v, want -60000 (the path row)", w)
	}
	if w := rb.Worst(10); *w != -320000 {
		t.Fatalf("worst within 10%% = %v", *w)
	}
}

func TestPeakDrawdownAndDayRoll(t *testing.T) {
	m := New(limits())
	d1 := clock.Date(time.Date(2026, 10, 20, 12, 0, 0, 0, clock.IST))
	now := d1.Add(10 * time.Hour)
	for _, v := range []float64{1000, 5000, 2000, 4000} {
		m.OnBook(book(t, v, 0, 0), now, d1)
	}
	s := m.State(now, true)
	if *s.Peak != 5000 || s.DD != 1000 || s.MaxDD != 3000 {
		t.Fatalf("peak %v dd %v maxdd %v", *s.Peak, s.DD, s.MaxDD)
	}
	d2 := d1.AddDate(0, 0, 1)
	m.OnBook(book(t, -500, 0, 0), now.AddDate(0, 0, 1), d2)
	s = m.State(now.AddDate(0, 0, 1), true)
	if *s.Peak != -500 || s.MaxDD != 0 {
		t.Fatalf("after the roll: peak %v maxdd %v", *s.Peak, s.MaxDD)
	}
}

func TestRestoreKeepsThePeak(t *testing.T) {
	m := New(limits())
	d := clock.Date(time.Date(2026, 10, 20, 12, 0, 0, 0, clock.IST))
	m.Restore(d, []Snap{{M2M: 100}, {M2M: 9000}, {M2M: 3000}})
	m.OnBook(book(t, 4000, 0, 0), d.Add(11*time.Hour), d)
	s := m.State(d.Add(11*time.Hour), true)
	if *s.Peak != 9000 || s.MaxDD != 6000 || len(m.Day().Snaps) != 3 {
		t.Fatalf("peak %v maxdd %v rows %d", *s.Peak, s.MaxDD, len(m.Day().Snaps))
	}
}

func TestLossLimitRaisesEscalatesHoldsAndClears(t *testing.T) {
	m := New(limits())
	d := clock.Date(time.Date(2026, 10, 20, 12, 0, 0, 0, clock.IST))
	now := d.Add(10 * time.Hour)
	m.OnMargins(&bus.Margins{Enabled: true, Debits: 100, Net: 900}, now)
	step := func(m2m float64) []Change {
		now = now.Add(time.Second)
		m.OnBook(book(t, m2m, 0, 0), now, d)
		m.OnMargins(&bus.Margins{Enabled: true, Debits: 100, Net: 900}, now)
		var out []Change
		for _, c := range m.Eval(now, true) {
			if c.Rule == RuleLoss {
				out = append(out, c)
			}
		}
		return out
	}
	if c := step(-14000); len(c) != 0 { // 70% of the limit, under the 75% warn
		t.Fatalf("early: %+v", c)
	}
	if c := step(-15500); len(c) != 1 || c[0].To != Warn {
		t.Fatalf("warn: %+v", c)
	}
	if c := step(-14500); len(c) != 0 || lvl(m, RuleLoss) != Warn { // inside the hysteresis
		t.Fatalf("hold: %+v %v", c, lvl(m, RuleLoss))
	}
	if c := step(-20500); len(c) != 1 || c[0].To != Crit || notifyLevel(c[0]) != "crit" {
		t.Fatalf("crit: %+v", c)
	}
	if c := step(-18500); len(c) != 0 { // 92.5%: crit holds above 90% of the limit
		t.Fatalf("crit hold: %+v", c)
	}
	if c := step(-1000); len(c) != 1 || c[0].To != OK || notifyLevel(c[0]) != "info" {
		t.Fatalf("clear: %+v", c)
	}
}

func TestOutOfSessionClearsQuietly(t *testing.T) {
	m := New(limits())
	d := clock.Date(time.Date(2026, 10, 20, 12, 0, 0, 0, clock.IST))
	now := d.Add(10 * time.Hour)
	m.OnBook(book(t, -30000, 5, 0), now, d)
	m.OnMargins(&bus.Margins{Enabled: true, Debits: 95, Net: 5}, now)
	got := map[string]Level{}
	for _, c := range m.Eval(now, true) {
		got[c.Rule] = c.To
	}
	want := map[string]Level{RuleLoss: Crit, RuleWorst: Warn, RuleMargin: Crit, RuleDelta: Warn, RuleDrawdown: OK}
	for r, l := range want {
		if got[r] != l {
			t.Errorf("%s = %v, want %v (all: %v)", r, got[r], l, got)
		}
	}
	if c := m.Eval(now.Add(time.Second), false); len(c) != 0 {
		t.Fatalf("out of session sent %+v", c)
	}
	for _, r := range m.State(now, false).Rules {
		if r.Level != "ok" {
			t.Fatalf("%s still %s out of session", r.Rule, r.Level)
		}
	}
}

func TestStaleAndFlags(t *testing.T) {
	m := New(limits())
	d := clock.Date(time.Date(2026, 10, 20, 12, 0, 0, 0, clock.IST))
	now := d.Add(10 * time.Hour)
	// Nothing has arrived: both inputs are stale.
	c := m.Eval(now, true)
	if lvl(m, RuleStale) != Crit || len(c) != 1 {
		t.Fatalf("no inputs: %+v", c)
	}
	m.OnBook(book(t, 0, 0, 1), now, d)
	m.OnMargins(&bus.Margins{Enabled: true, Debits: 1, Net: 99}, now)
	m.Eval(now, true)
	if lvl(m, RuleStale) != OK || lvl(m, RuleFlags) != OK {
		t.Fatal("fresh inputs should clear stale; a new flag waits flag_after")
	}
	later := now.Add(2*time.Minute + time.Second)
	m.OnBook(book(t, 0, 0, 1), later, d)
	m.OnMargins(&bus.Margins{Enabled: true, Debits: 1, Net: 99}, later)
	m.Eval(later, true)
	if lvl(m, RuleFlags) != Warn {
		t.Fatal("a flag present past flag_after should warn")
	}
	if _, ok := m.Snapshot(later.Add(31 * time.Second)); ok {
		t.Fatal("no snapshot from a stale risk.book")
	}
	if s, ok := m.Snapshot(later); !ok || s.Flags != 1 || *s.Util != 0.01 {
		t.Fatalf("snapshot %+v %v", s, ok)
	}
}

func TestInr(t *testing.T) {
	for in, want := range map[float64]string{0: "0", 999: "999", 1000: "1,000", 123456: "1,23,456", -12345678: "-1,23,45,678"} {
		if got := inr(in); got != want {
			t.Errorf("inr(%v) = %q, want %q", in, got, want)
		}
	}
}
