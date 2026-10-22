package newsmon

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"pmt/internal/clock"
)

func TestParseEvents(t *testing.T) {
	es, err := ParseEvents([]byte(`
events:
  - {id: b, name: B, impact: high, category: fomc, datetime_ist: "2026-12-10 00:30", affects: [nifty]}
  - {id: c, name: C, impact: LOW, category: x, datetime_ist: ""}
  - {id: a, name: A, impact: MEDIUM, category: x, datetime_ist: "2026-11-12"}
`))
	if err != nil {
		t.Fatal(err)
	}
	if es[0].ID != "a" || es[1].ID != "b" || es[2].ID != "c" {
		t.Fatalf("order: %s %s %s", es[0].ID, es[1].ID, es[2].ID)
	}
	if es[0].Timed() || es[0].Date.IsZero() {
		t.Fatalf("date-only: %+v", es[0])
	}
	b := es[1]
	if b.Impact != "HIGH" || b.Affects[0] != "NIFTY" || !b.At.Equal(time.Date(2026, 12, 10, 0, 30, 0, 0, clock.IST)) {
		t.Fatalf("b: %+v", b)
	}
	if !es[2].Date.IsZero() || es[2].Timed() {
		t.Fatalf("tbc: %+v", es[2])
	}
}

func TestParseEventsRejects(t *testing.T) {
	cases := map[string]string{
		"missing id":   `events: [{name: A, impact: HIGH, category: x}]`,
		"duplicate":    `events: [{id: a, name: A, impact: HIGH, category: x}, {id: a, name: B, impact: HIGH, category: x}]`,
		"bad impact":   `events: [{id: a, name: A, impact: HUGE, category: x}]`,
		"no category":  `events: [{id: a, name: A, impact: HIGH}]`,
		"bad affects":  `events: [{id: a, name: A, impact: HIGH, category: x, affects: [SENSEX]}]`,
		"bad datetime": `events: [{id: a, name: A, impact: HIGH, category: x, datetime_ist: "4 Dec 10am"}]`,
		"bad yaml":     `events: [`,
	}
	for name, y := range cases {
		if _, err := ParseEvents([]byte(y)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The shipped calendar must always parse, and every entry stays marked verify
// until someone has checked it against the primary source.
func TestShippedEventFile(t *testing.T) {
	_, f, _, _ := runtime.Caller(0)
	es, err := LoadEvents(filepath.Join(filepath.Dir(f), "../../config/news/events.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(es) == 0 {
		t.Fatal("no events")
	}
	for _, e := range es {
		if !e.Verify {
			t.Errorf("%s: verify is false; flip it only after checking %q", e.ID, e.Source)
		}
		if e.Source == "" {
			t.Errorf("%s: no source", e.ID)
		}
	}
}

func TestExpiryEvents(t *testing.T) {
	d := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC) // refdata keys are UTC dates
	es, err := ExpiryEvents("NIFTY", []time.Time{d}, "15:30:00")
	if err != nil {
		t.Fatal(err)
	}
	e := es[0]
	if e.ID != "expiry-nifty-2026-10-27" || !e.Derived || e.Category != "expiry" ||
		!e.At.Equal(time.Date(2026, 10, 27, 15, 30, 0, 0, clock.IST)) || !strings.Contains(e.Name, "NIFTY") {
		t.Fatalf("%+v", e)
	}
}
