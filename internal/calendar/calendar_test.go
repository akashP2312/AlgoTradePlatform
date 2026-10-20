package calendar

import (
	"testing"
	"time"

	"pmt/internal/clock"
)

func day(s string) time.Time {
	d, err := time.ParseInLocation("2006-01-02", s, clock.IST)
	if err != nil {
		panic(err)
	}
	return d
}

func repo(t *testing.T) (*Calendar, []Rule) {
	t.Helper()
	c, err := Load("../../config/calendar/holidays.csv")
	if err != nil {
		t.Fatal(err)
	}
	r, err := LoadRules("../../config/expiry_rules.csv")
	if err != nil {
		t.Fatal(err)
	}
	return c, r
}

func TestSessions(t *testing.T) {
	c, _ := repo(t)
	if s := c.Session(day("2026-10-20")); s.Kind != Holiday {
		t.Fatalf("Dussehra = %v", s.Kind)
	}
	if s := c.Session(day("2026-10-19")); s.Kind != Normal || s.Open.Hour() != 9 || s.Open.Minute() != 15 {
		t.Fatalf("Monday = %+v", s)
	}
	if s := c.Session(day("2026-10-24")); s.Kind != Weekend {
		t.Fatalf("Saturday = %v", s.Kind)
	}
	if s := c.Session(day("2026-11-08")); s.Kind != Special || s.Open.Hour() != 18 {
		t.Fatalf("Muhurat Sunday = %+v", s)
	}
	if p := c.Prev(day("2026-10-21")); !p.Equal(day("2026-10-19")) {
		t.Fatalf("Prev = %v", p)
	}
	if !c.Covers(day("2026-06-01")) || c.Covers(day("2030-01-01")) {
		t.Fatal("Covers")
	}
}

func TestProjectNiftyTuesdays(t *testing.T) {
	c, r := repo(t)
	ex := Project(c, r, "NIFTY", day("2026-10-19"), day("2026-11-30"))
	want := map[string]string{
		"2026-10-20": "", // Dussehra holiday: weekly moves to Monday 19th
		"2026-10-19": "weekly",
		"2026-10-27": "monthly",
		"2026-11-03": "weekly",
		"2026-11-24": "", // Guru Nanak Jayanti: monthly moves to Monday 23rd
		"2026-11-23": "monthly",
	}
	got := map[string]string{}
	for _, e := range ex {
		got[e.Date.Format("2006-01-02")] = e.Cycle
	}
	for d, cyc := range want {
		if cyc == "" {
			if _, ok := got[d]; ok {
				t.Errorf("%s should have moved", d)
			}
			continue
		}
		if got[d] != cyc {
			t.Errorf("%s = %q want %q (all: %v)", d, got[d], cyc, got)
		}
	}
	bn := Project(c, r, "BANKNIFTY", day("2026-10-01"), day("2026-12-31"))
	if len(bn) != 3 {
		t.Fatalf("BANKNIFTY monthlies = %v", bn)
	}
	for _, e := range bn {
		if e.Cycle != "monthly" {
			t.Fatalf("BANKNIFTY weekly projected: %v", e)
		}
	}
	old := Project(c, r, "NIFTY", day("2025-08-01"), day("2025-08-31"))
	for _, e := range old {
		if e.Nominal.Weekday() != time.Thursday {
			t.Fatalf("pre-Sept-2025 NIFTY expiry not Thursday: %v", e)
		}
	}
}
