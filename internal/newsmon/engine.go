package newsmon

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"pmt/internal/bus"
	"pmt/internal/clock"
	"pmt/internal/config"
)

// Level is the risk state, ordered: the worst reason wins.
type Level int

const (
	Normal Level = iota
	Caution
	NoNewEntries
)

var levelNames = [...]string{"NORMAL", "CAUTION", "NO_NEW_ENTRIES"}

func (l Level) String() string {
	if l < Normal || l > NoNewEntries {
		return fmt.Sprintf("Level(%d)", int(l))
	}
	return levelNames[l]
}

func ParseLevel(s string) (Level, error) {
	for i, n := range levelNames {
		if n == s {
			return Level(i), nil
		}
	}
	return Normal, fmt.Errorf("unknown risk state %q", s)
}

func (l Level) MarshalJSON() ([]byte, error) { return json.Marshal(l.String()) }

func (l *Level) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := ParseLevel(s)
	*l = v
	return err
}

// Reason is one thing holding the state above NORMAL. Times are epoch ms.
type Reason struct {
	Level  Level  `json:"level"`
	Kind   string `json:"kind"` // "event"; headlines and market stress come later
	Event  string `json:"event,omitempty"`
	Name   string `json:"name"`
	Impact string `json:"impact,omitempty"`
	At     int64  `json:"at,omitempty"` // the event's time
	Until  int64  `json:"until"`        // when this reason stops holding this level
	Msg    string `json:"msg"`
	Verify bool   `json:"verify,omitempty"` // the event's date or time is unconfirmed
}

// Next is the next timed HIGH event, for the always-on countdown.
type Next struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	At     int64  `json:"at"`
	Verify bool   `json:"verify,omitempty"`
}

// RiskState is news.risk and the body of GET /api/risk-state.
type RiskState struct {
	TS        int64    `json:"ts"`
	State     Level    `json:"state"`
	Reasons   []Reason `json:"reasons"`
	Since     int64    `json:"since,omitempty"`      // when the state last changed level
	ExpiresAt int64    `json:"expires_at,omitempty"` // when the level next changes; 0 = nothing scheduled
	NextHigh  *Next    `json:"next_high,omitempty"`
}

// Windows picks an event's window: its category's if set, else its impact's.
type Windows struct {
	ByImpact   map[string]config.Window
	ByCategory map[string]config.Window
}

func (w Windows) For(e *Event) (config.Window, bool) {
	if c, ok := w.ByCategory[e.Category]; ok {
		return c, true
	}
	c, ok := w.ByImpact[e.Impact]
	return c, ok
}

// spans returns the event's NO_NEW_ENTRIES and CAUTION spans, each [from, to).
func (w Windows) spans(e *Event) (ne, ca [2]time.Time, ok bool) {
	if !e.Timed() {
		return ne, ca, false
	}
	win, ok := w.For(e)
	if !ok {
		return ne, ca, false
	}
	ne = [2]time.Time{e.At.Add(-win.NoEntryBefore), e.At.Add(win.NoEntryAfter)}
	ca = [2]time.Time{e.At.Add(-win.CautionBefore), e.At.Add(win.CautionAfter)}
	return ne, ca, true
}

func in(s [2]time.Time, t time.Time) bool { return !t.Before(s[0]) && t.Before(s[1]) }

// Eval returns the level at now and the reasons for it, worst first.
func Eval(now time.Time, events []Event, w Windows) (Level, []Reason) {
	lvl := Normal
	var rs []Reason
	for i := range events {
		e := &events[i]
		ne, ca, ok := w.spans(e)
		if !ok {
			continue
		}
		var r Reason
		switch {
		case in(ne, now):
			r = Reason{Level: NoNewEntries, Until: bus.Ms(ne[1])}
		case in(ca, now):
			r = Reason{Level: Caution, Until: bus.Ms(ca[1])}
		default:
			continue
		}
		r.Kind, r.Event, r.Name, r.Impact, r.At, r.Verify = "event", e.ID, e.Name, e.Impact, bus.Ms(e.At), e.Verify
		r.Msg = describe(now, e)
		rs = append(rs, r)
		if r.Level > lvl {
			lvl = r.Level
		}
	}
	sort.SliceStable(rs, func(i, j int) bool {
		if rs[i].Level != rs[j].Level {
			return rs[i].Level > rs[j].Level
		}
		return rs[i].At < rs[j].At
	})
	return lvl, rs
}

func describe(now time.Time, e *Event) string {
	d := e.At.Sub(now).Round(time.Minute)
	when := fmt.Sprintf("in %s", short(d))
	if d < 0 {
		when = fmt.Sprintf("%s ago", short(-d))
	} else if d == 0 {
		when = "now"
	}
	v := ""
	if e.Verify {
		v = " (time unverified)"
	}
	return fmt.Sprintf("%s %s %s%s", e.Impact, e.Name, when, v)
}

func short(d time.Duration) string {
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

// Assess is the full state at now: the level, its reasons, when the level next
// changes (the first window edge after now at which it differs; for NORMAL, the
// start of the next window), and the next HIGH event.
func Assess(now time.Time, events []Event, w Windows) RiskState {
	lvl, rs := Eval(now, events, w)
	if rs == nil {
		rs = []Reason{}
	}
	st := RiskState{TS: bus.Ms(now), State: lvl, Reasons: rs}

	var edges []time.Time
	for i := range events {
		ne, ca, ok := w.spans(&events[i])
		if !ok {
			continue
		}
		for _, t := range []time.Time{ne[0], ne[1], ca[0], ca[1]} {
			if t.After(now) {
				edges = append(edges, t)
			}
		}
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i].Before(edges[j]) })
	for _, t := range edges {
		if l, _ := Eval(t, events, w); l != lvl {
			st.ExpiresAt = bus.Ms(t)
			break
		}
	}

	for i := range events {
		e := &events[i]
		if e.Impact == "HIGH" && e.Timed() && e.At.After(now) && (st.NextHigh == nil || bus.Ms(e.At) < st.NextHigh.At) {
			st.NextHigh = &Next{ID: e.ID, Name: e.Name, At: bus.Ms(e.At), Verify: e.Verify}
		}
	}
	return st
}

// EventView is one row of news.events.
type EventView struct {
	Event
	At   int64  `json:"at,omitempty"`   // epoch ms, when timed
	Date string `json:"date,omitempty"` // yyyy-mm-dd, when the day is known
	TBC  bool   `json:"tbc,omitempty"`  // date not confirmed
}

// Calendar is news.events: everything from yesterday to the horizon, plus TBC rows.
type Calendar struct {
	TS     int64       `json:"ts"`
	Loaded int64       `json:"loaded,omitempty"` // when the event file was last read
	Err    string      `json:"err,omitempty"`    // the last load error; the previous events stay in force
	Events []EventView `json:"events"`
}

func View(now time.Time, events []Event, horizon time.Duration) []EventView {
	from := clock.Date(now).AddDate(0, 0, -1)
	to := now.Add(horizon)
	out := []EventView{}
	for _, e := range events {
		switch {
		case e.Date.IsZero():
			out = append(out, EventView{Event: e, TBC: true})
		case e.Date.Before(from) || e.Date.After(to):
			continue
		default:
			v := EventView{Event: e, Date: e.Date.Format("2006-01-02")}
			if e.Timed() {
				v.At = bus.Ms(e.At)
			}
			out = append(out, v)
		}
	}
	return out
}
