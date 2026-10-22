package newsmon

import (
	"strings"
	"testing"
	"time"

	"pmt/internal/bus"
	"pmt/internal/config"
)

func vixCfg() config.VIX {
	return config.VIX{Enabled: true, Subject: "tick.nse.indiavix._._.idx", DayPct: 15, JumpPct: 7, Window: 15 * time.Minute,
		Level: 25, Persist: 30 * time.Second, HoldFor: 15 * time.Minute, Stale: 2 * time.Minute}
}

// vtick is a continuous-phase VIX print received at at.
func vtick(p, cl float64, at time.Time) bus.Tick {
	return bus.Tick{C: "NSE|INDIAVIX", LTP: p, Close: cl, Phase: "c", Recv: bus.Ms(at)}
}

// feed observes p every 10s from t0 for d and returns the time after the last.
func feedVIX(w *VIXWatch, p, cl float64, t0 time.Time, d time.Duration) time.Time {
	t := t0
	for ; t.Sub(t0) <= d; t = t.Add(10 * time.Second) {
		w.Observe(vtick(p, cl, t), t)
	}
	return t
}

func TestVIXDayMove(t *testing.T) {
	w := &VIXWatch{Cfg: vixCfg()}
	t0 := fixtureNow
	w.Observe(vtick(16.5, 15, t0), t0) // +10%: under day_pct
	if hs := w.Holds(t0); len(hs) != 0 {
		t.Fatalf("holds under threshold: %+v", hs)
	}
	w.Cfg.JumpPct = 0                                                          // isolate the day test from the jump from 16.5
	w.Observe(vtick(17.5, 15, t0.Add(10*time.Second)), t0.Add(10*time.Second)) // +16.7%
	if hs := w.Holds(t0.Add(20 * time.Second)); len(hs) != 0 {
		t.Fatalf("held before persist: %+v", hs)
	}
	end := feedVIX(w, 17.5, 15, t0.Add(20*time.Second), 30*time.Second)
	hs := w.Holds(end)
	if len(hs) != 1 {
		t.Fatalf("holds = %+v", hs)
	}
	h := hs[0]
	if h.Level != Caution || h.Reason.Kind != "vix" || !h.From.Equal(t0.Add(40*time.Second)) ||
		!strings.Contains(h.Reason.Msg, "+16.7% vs prev close 15.00") {
		t.Fatalf("hold = %+v", h)
	}
	st := Assess(end, nil, testWin, hs...)
	if st.State != Caution || st.Reasons[0].Kind != "vix" || st.ExpiresAt != bus.Ms(h.Until) {
		t.Fatalf("assess = %+v", st)
	}

	// Back under the threshold: the hold runs hold_for past the last trigger, then ends.
	last := end.Add(-10 * time.Second)
	w.Observe(vtick(16, 15, end), end)
	if hs := w.Holds(last.Add(14 * time.Minute)); len(hs) != 1 || !hs[0].Until.Equal(last.Add(15*time.Minute)) {
		t.Fatalf("hold did not run on: %+v", hs)
	}
	if hs := w.Holds(last.Add(15 * time.Minute)); len(hs) != 0 {
		t.Fatalf("hold outlived hold_for: %+v", hs)
	}
}

func TestVIXBlipUnderPersist(t *testing.T) {
	w := &VIXWatch{Cfg: vixCfg()}
	t0 := fixtureNow
	w.Observe(vtick(18, 15, t0), t0)
	w.Observe(vtick(18, 15, t0.Add(20*time.Second)), t0.Add(20*time.Second))
	w.Observe(vtick(16, 15, t0.Add(25*time.Second)), t0.Add(25*time.Second)) // back before 30s
	w.Observe(vtick(18, 15, t0.Add(40*time.Second)), t0.Add(40*time.Second)) // the clock restarts
	if hs := w.Holds(t0.Add(45 * time.Second)); len(hs) != 0 {
		t.Fatalf("blip held: %+v", hs)
	}
}

func TestVIXJump(t *testing.T) {
	w := &VIXWatch{Cfg: vixCfg()}
	w.Cfg.DayPct = 0
	t0 := fixtureNow
	t1 := feedVIX(w, 14, 14, t0, 5*time.Minute)
	if hs := w.Holds(t1); len(hs) != 0 {
		t.Fatalf("flat held: %+v", hs)
	}
	end := feedVIX(w, 15.1, 14, t1, 40*time.Second) // +7.9% on the window low
	hs := w.Holds(end)
	if len(hs) != 1 || !strings.Contains(hs[0].Reason.Msg, "+7.9% in 15m0s from 14.00") {
		t.Fatalf("jump holds = %+v", hs)
	}

	// The low ages out of the window: a slow drift is not a jump.
	w2 := &VIXWatch{Cfg: w.Cfg}
	w2.Observe(vtick(14, 14, t0), t0)
	later := t0.Add(16 * time.Minute)
	feedVIX(w2, 15.1, 14, later, time.Minute)
	if hs := w2.Holds(later.Add(time.Minute)); len(hs) != 0 {
		t.Fatalf("old low counted: %+v", hs)
	}
}

func TestVIXLevel(t *testing.T) {
	w := &VIXWatch{Cfg: vixCfg()}
	w.Cfg.DayPct, w.Cfg.JumpPct = 0, 0
	end := feedVIX(w, 26, 25.8, fixtureNow, 40*time.Second)
	if hs := w.Holds(end); len(hs) != 1 || !strings.Contains(hs[0].Reason.Msg, "at or above 25") {
		t.Fatalf("level holds = %+v", hs)
	}
}

func TestVIXIgnores(t *testing.T) {
	t0 := fixtureNow
	// Pre-open and after-hours prints never trigger.
	w := &VIXWatch{Cfg: vixCfg()}
	for i := range 10 {
		at := t0.Add(time.Duration(i) * 10 * time.Second)
		tk := vtick(30, 15, at)
		tk.Phase = "p"
		w.Observe(tk, at)
	}
	if hs := w.Holds(t0.Add(2 * time.Minute)); len(hs) != 0 {
		t.Fatalf("pre-open held: %+v", hs)
	}
	if d := w.Health(t0.Add(90 * time.Second)); d["vix"] != "30.00" || d["vix_err"] != "not counted: phase p" {
		t.Fatalf("health = %v", d)
	}

	// An old print (yesterday's last value from KV) never triggers.
	w = &VIXWatch{Cfg: vixCfg()}
	for i := range 10 {
		at := t0.Add(time.Duration(i) * 10 * time.Second)
		tk := vtick(30, 15, t0.Add(-time.Hour))
		w.Observe(tk, at)
	}
	if hs := w.Holds(t0.Add(2 * time.Minute)); len(hs) != 0 {
		t.Fatalf("stale held: %+v", hs)
	}
	if d := w.Health(t0); !strings.HasPrefix(d["vix_err"], "last tick 1h") {
		t.Fatalf("health = %v", d)
	}

	// Ticks stop mid-hold: no new trigger, but the running hold ends on time.
	w = &VIXWatch{Cfg: vixCfg()}
	end := feedVIX(w, 18, 15, t0, 40*time.Second)
	last := end.Add(-10 * time.Second)
	if hs := w.Holds(last.Add(5 * time.Minute)); len(hs) != 1 {
		t.Fatalf("hold dropped on silence: %+v", hs)
	}
	if hs := w.Holds(last.Add(15 * time.Minute)); len(hs) != 0 {
		t.Fatalf("hold outlived hold_for: %+v", hs)
	}

	var nilW *VIXWatch
	if nilW.Holds(t0) != nil {
		t.Fatal("nil watch held")
	}
	if d := (&VIXWatch{Cfg: vixCfg()}).Health(t0); !strings.HasPrefix(d["vix_err"], "no tick yet") {
		t.Fatalf("health = %v", d)
	}
}

func TestVIXRetrigger(t *testing.T) {
	w := &VIXWatch{Cfg: vixCfg()}
	w.Cfg.JumpPct = 0
	t0 := fixtureNow
	end := feedVIX(w, 18, 15, t0, 40*time.Second)
	from := w.Holds(end)[0].From
	// A second spike inside the hold extends it and keeps its start.
	t1 := end.Add(5 * time.Minute)
	w.Observe(vtick(16, 15, end), end)
	e2 := feedVIX(w, 18.5, 15, t1, 40*time.Second)
	hs := w.Holds(e2)
	if len(hs) != 1 || !hs[0].From.Equal(from) || !hs[0].Until.Equal(e2.Add(-10*time.Second).Add(15*time.Minute)) {
		t.Fatalf("retrigger = %+v (from %v)", hs, from)
	}
}
