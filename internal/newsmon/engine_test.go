package newsmon

import (
	"strings"
	"testing"
	"time"

	"pmt/internal/bus"
	"pmt/internal/clock"
	"pmt/internal/config"
)

var testWin = Windows{
	ByImpact: map[string]config.Window{
		"HIGH":   {NoEntryBefore: 15 * time.Minute, NoEntryAfter: 15 * time.Minute, CautionBefore: 30 * time.Minute, CautionAfter: 45 * time.Minute},
		"MEDIUM": {CautionBefore: 10 * time.Minute, CautionAfter: 15 * time.Minute},
	},
	ByCategory: map[string]config.Window{"expiry": {CautionBefore: 60 * time.Minute}},
}

func at(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04", s, clock.IST)
	if err != nil {
		panic(err)
	}
	return t
}

func ev(id, impact, cat, when string) Event {
	e := Event{ID: id, Name: id, Impact: impact, Category: cat}
	if when != "" {
		e.At = at(when)
		e.Date = clock.Date(e.At)
	}
	return e
}

func TestEvalHighWindow(t *testing.T) {
	es := []Event{ev("rbi", "HIGH", "rbi_policy", "2026-12-04 10:00")}
	cases := []struct {
		now  string
		want Level
	}{
		{"2026-12-04 09:29", Normal},
		{"2026-12-04 09:30", Caution}, // T-30 inclusive
		{"2026-12-04 09:44", Caution},
		{"2026-12-04 09:45", NoNewEntries}, // T-15
		{"2026-12-04 10:00", NoNewEntries},
		{"2026-12-04 10:14", NoNewEntries},
		{"2026-12-04 10:15", Caution}, // T+15 exclusive for no-entry
		{"2026-12-04 10:44", Caution},
		{"2026-12-04 10:45", Normal}, // T+45 exclusive
	}
	for _, c := range cases {
		if got, _ := Eval(at(c.now), es, testWin); got != c.want {
			t.Errorf("%s: got %s want %s", c.now, got, c.want)
		}
	}
}

func TestEvalCategoryOverridesImpact(t *testing.T) {
	es := []Event{ev("exp", "MEDIUM", "expiry", "2026-10-27 15:30")}
	if l, _ := Eval(at("2026-10-27 14:35"), es, testWin); l != Caution {
		t.Fatalf("expiry T-55m: got %s want CAUTION (category window)", l)
	}
	if l, _ := Eval(at("2026-10-27 15:31"), es, testWin); l != Normal {
		t.Fatalf("expiry T+1m: got %s want NORMAL (no caution_after)", l)
	}
}

func TestUntimedAndUnknownImpactOpenNoWindow(t *testing.T) {
	es := []Event{ev("tbc", "HIGH", "us_cpi", ""), ev("low", "LOW", "in_wpi", "2026-11-14 12:00")}
	if l, rs := Eval(at("2026-11-14 12:00"), es, testWin); l != Normal || len(rs) != 0 {
		t.Fatalf("got %s %v", l, rs)
	}
}

func TestReasonsWorstFirst(t *testing.T) {
	es := []Event{
		ev("a-med", "MEDIUM", "in_cpi", "2026-11-12 16:00"),
		ev("b-high", "HIGH", "us_jobs", "2026-11-12 16:10"),
	}
	l, rs := Eval(at("2026-11-12 15:58"), es, testWin)
	if l != NoNewEntries || len(rs) != 2 || rs[0].Event != "b-high" || rs[1].Event != "a-med" {
		t.Fatalf("got %s %+v", l, rs)
	}
	if rs[0].Until != bus.Ms(at("2026-11-12 16:25")) {
		t.Fatalf("until: %d", rs[0].Until)
	}
}

func TestAssessExpiry(t *testing.T) {
	es := []Event{ev("rbi", "HIGH", "rbi_policy", "2026-12-04 10:00")}
	cases := []struct{ now, exp string }{
		{"2026-12-04 08:00", "2026-12-04 09:30"}, // NORMAL until caution starts
		{"2026-12-04 09:35", "2026-12-04 09:45"},
		{"2026-12-04 09:50", "2026-12-04 10:15"},
		{"2026-12-04 10:20", "2026-12-04 10:45"},
	}
	for _, c := range cases {
		st := Assess(at(c.now), es, testWin)
		if st.ExpiresAt != bus.Ms(at(c.exp)) {
			t.Errorf("%s: expires %v want %s", c.now, bus.FromMs(st.ExpiresAt).In(clock.IST), c.exp)
		}
		if st.Reasons == nil {
			t.Errorf("%s: reasons nil", c.now)
		}
	}
	if st := Assess(at("2026-12-04 11:00"), es, testWin); st.ExpiresAt != 0 || st.State != Normal {
		t.Fatalf("after: %+v", st)
	}
}

func TestAssessOverlapSkipsSameLevelEdges(t *testing.T) {
	// Two HIGH events 20m apart: caution of the second starts inside the first's
	// no-entry, so the first level change from 09:50 is the end of the second's no-entry.
	es := []Event{ev("a", "HIGH", "x", "2026-12-04 10:00"), ev("b", "HIGH", "x", "2026-12-04 10:20")}
	st := Assess(at("2026-12-04 09:50"), es, testWin)
	if st.State != NoNewEntries || st.ExpiresAt != bus.Ms(at("2026-12-04 10:35")) {
		t.Fatalf("got %s expires %v", st.State, bus.FromMs(st.ExpiresAt).In(clock.IST))
	}
}

func TestNextHigh(t *testing.T) {
	es := []Event{
		ev("med", "MEDIUM", "x", "2026-11-01 10:00"),
		ev("tbc", "HIGH", "x", ""),
		ev("h2", "HIGH", "x", "2026-11-20 10:00"),
		ev("h1", "HIGH", "x", "2026-11-06 19:00"),
	}
	es[3].Verify = true
	st := Assess(at("2026-10-21 12:00"), es, testWin)
	if st.NextHigh == nil || st.NextHigh.ID != "h1" || !st.NextHigh.Verify {
		t.Fatalf("got %+v", st.NextHigh)
	}
	if st := Assess(at("2026-12-01 12:00"), es, testWin); st.NextHigh != nil {
		t.Fatalf("none left: %+v", st.NextHigh)
	}
}

func TestLevelJSON(t *testing.T) {
	for _, l := range []Level{Normal, Caution, NoNewEntries} {
		b, _ := l.MarshalJSON()
		var got Level
		if err := got.UnmarshalJSON(b); err != nil || got != l {
			t.Fatalf("%s: %v %s", l, err, got)
		}
	}
}

func TestView(t *testing.T) {
	es := []Event{
		ev("old", "HIGH", "x", "2026-10-01 10:00"),
		ev("yday", "HIGH", "x", "2026-10-20 10:00"),
		ev("soon", "HIGH", "x", "2026-11-01 10:00"),
		ev("far", "HIGH", "x", "2027-06-01 10:00"),
		ev("tbc", "HIGH", "x", ""),
	}
	v := View(at("2026-10-21 12:00"), es, 92*24*time.Hour)
	var ids []string
	for _, e := range v {
		ids = append(ids, e.ID)
	}
	want := "yday soon tbc"
	if got := strings.Join(ids, " "); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if !v[2].TBC || v[1].At == 0 || v[1].Date != "2026-11-01" {
		t.Fatalf("rows: %+v", v)
	}
}
