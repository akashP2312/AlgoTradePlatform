package newsmon

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"pmt/internal/bus"
	"pmt/internal/clock"
	"pmt/internal/config"
)

// VIXWatch turns India VIX ticks into a CAUTION hold. It triggers when, during
// continuous trading, VIX is DayPct above the previous close, JumpPct above its
// low over Window, or at least Level, and the trigger lasts Persist. The hold
// runs until HoldFor after the trigger last held. Stale or missing ticks start
// nothing, and a hold already running still ends on time. Like a headline, a
// VIX spike only says to be careful: it never says which way to trade.
type VIXWatch struct {
	Cfg config.VIX

	mu      sync.Mutex
	samples []vixSample // continuous-phase ticks within Window, oldest first
	last    bus.Tick
	lastAt  time.Time // when last was seen, clock.Now() time
	start   time.Time // when the current trigger began; zero when not triggered
	held    time.Time // when the trigger last held after Persist
	from    time.Time // when the running hold began
	why     string    // the running hold's message
	ignored string    // why the last tick did not count
}

type vixSample struct {
	at time.Time
	p  float64
}

// Observe takes one VIX tick seen at now (clock.Now() time).
func (v *VIXWatch) Observe(t bus.Tick, now time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if t.LTP <= 0 {
		v.ignored = "no price"
		return
	}
	v.last, v.lastAt = t, now
	if v.stale(now) {
		// A replayed or seeded old print says nothing about now.
		v.ignored = "stale"
		v.start = time.Time{}
		return
	}
	if t.Phase != "c" {
		// Pre-open and after-hours prints are thin; only continuous trading counts.
		v.ignored = "phase " + t.Phase
		v.start = time.Time{}
		return
	}
	v.ignored = ""
	v.samples = append(v.samples, vixSample{now, t.LTP})
	i := 0
	for i < len(v.samples) && now.Sub(v.samples[i].at) > v.Cfg.Window {
		i++
	}
	v.samples = v.samples[i:]

	msg := v.trigger(t)
	if msg == "" {
		v.start = time.Time{}
		return
	}
	if v.start.IsZero() {
		v.start = now
	}
	if now.Sub(v.start) < v.Cfg.Persist {
		return
	}
	if v.held.IsZero() || !now.Before(v.held.Add(v.Cfg.HoldFor)) {
		v.from = v.start.Add(v.Cfg.Persist)
	}
	v.held, v.why = now, msg
}

// trigger says which spike test t passes, or "" for none.
func (v *VIXWatch) trigger(t bus.Tick) string {
	var why []string
	if c := v.Cfg.DayPct; c > 0 && t.Close > 0 {
		if d := pct(t.LTP, t.Close); d >= c {
			why = append(why, fmt.Sprintf("%+.1f%% vs prev close %.2f", d, t.Close))
		}
	}
	if c := v.Cfg.JumpPct; c > 0 && len(v.samples) > 0 {
		lo := v.samples[0]
		for _, s := range v.samples {
			if s.p < lo.p {
				lo = s
			}
		}
		if d := pct(t.LTP, lo.p); d >= c {
			why = append(why, fmt.Sprintf("%+.1f%% in %s from %.2f", d, v.Cfg.Window, lo.p))
		}
	}
	if c := v.Cfg.Level; c > 0 && t.LTP >= c {
		why = append(why, fmt.Sprintf("at or above %.0f", c))
	}
	if len(why) == 0 {
		return ""
	}
	return fmt.Sprintf("India VIX %.2f: %s", t.LTP, strings.Join(why, "; "))
}

func pct(p, base float64) float64 { return (p - base) / base * 100 }

// age is how old the last tick is, judged in real time from its receive stamp
// so a dry-run clock offset does not age it.
func (v *VIXWatch) age(now time.Time) time.Duration {
	recv := bus.FromMs(v.last.Recv)
	if recv.IsZero() {
		recv = clock.Real(v.lastAt)
	}
	return clock.Real(now).Sub(recv)
}

func (v *VIXWatch) stale(now time.Time) bool { return v.lastAt.IsZero() || v.age(now) > v.Cfg.Stale }

// Holds is the VIX hold at now, if one is running.
func (v *VIXWatch) Holds(now time.Time) []Hold {
	if v == nil {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.held.IsZero() {
		return nil
	}
	until := v.held.Add(v.Cfg.HoldFor)
	if now.Before(v.from) || !now.Before(until) {
		return nil
	}
	return []Hold{{Level: Caution, From: v.from, Until: until, Reason: Reason{
		Level: Caution, Kind: "vix", Name: "India VIX spike", Until: bus.Ms(until), Msg: v.why,
	}}}
}

// Health reports the last VIX print and why it is not being used, if it is not.
func (v *VIXWatch) Health(now time.Time) map[string]string {
	v.mu.Lock()
	defer v.mu.Unlock()
	d := map[string]string{}
	switch {
	case v.lastAt.IsZero():
		d["vix_err"] = "no tick yet on " + v.Cfg.Subject
		return d
	case v.stale(now):
		d["vix_err"] = fmt.Sprintf("last tick %s old", v.age(now).Round(time.Second))
	case v.ignored != "":
		d["vix_err"] = "not counted: " + v.ignored
	}
	d["vix"] = fmt.Sprintf("%.2f", v.last.LTP)
	if v.last.Close > 0 {
		d["vix_chg"] = fmt.Sprintf("%+.1f%%", pct(v.last.LTP, v.last.Close))
	}
	return d
}
