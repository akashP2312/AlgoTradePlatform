// Package newsmon is the news risk filter: it turns a calendar of scheduled macro
// events (and, later, headlines and market stress) into a risk state of NORMAL,
// CAUTION or NO_NEW_ENTRIES with reasons and an expiry. The state is a filter on
// new entries, never a trade signal, and nothing here places or closes an order.
//
// It also polls RSS and Atom feeds (official, exchange and media sources), each
// isolated behind its own backoff and circuit breaker and gated by robots.txt,
// and publishes the deduplicated headlines on news.feed. Each headline is scored
// 0-100 by the editable rules file (and, optionally, raised by a model); a fresh
// one at news.classify.caution_impact holds CAUTION for a while once a trusted
// source publishes it or enough distinct sources carry it.
package newsmon

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"pmt/internal/clock"
)

// Event is one scheduled event. At is set when the time is known (only those open
// a window); Date alone when only the day is; neither when the date is TBC.
type Event struct {
	ID          string   `yaml:"id" json:"id"`
	Name        string   `yaml:"name" json:"name"`
	Country     string   `yaml:"country" json:"country"`
	DatetimeIST string   `yaml:"datetime_ist" json:"-"`
	Impact      string   `yaml:"impact" json:"impact"`
	Category    string   `yaml:"category" json:"category"`
	Affects     []string `yaml:"affects" json:"affects"`
	Verify      bool     `yaml:"verify" json:"verify"`
	Source      string   `yaml:"source" json:"source,omitempty"`
	Note        string   `yaml:"note" json:"note,omitempty"`
	// Derived marks events newsmon made itself (F&O expiries), not read from the file.
	Derived bool `yaml:"-" json:"derived,omitempty"`

	At   time.Time `yaml:"-" json:"-"`
	Date time.Time `yaml:"-" json:"-"`
}

// Timed reports whether the event has a time, and so a window.
func (e *Event) Timed() bool { return !e.At.IsZero() }

var (
	impacts = map[string]bool{"HIGH": true, "MEDIUM": true, "LOW": true}
	indices = map[string]bool{"NIFTY": true, "BANKNIFTY": true}
)

// LoadEvents reads and checks the event file.
func LoadEvents(path string) ([]Event, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseEvents(b)
}

// ParseEvents parses the event file's YAML: ids unique, impact HIGH, MEDIUM or
// LOW, affects a subset of NIFTY and BANKNIFTY, and datetime_ist one of
// "YYYY-MM-DD HH:MM", "YYYY-MM-DD" or "" (TBC), in IST.
func ParseEvents(b []byte) ([]Event, error) {
	var f struct {
		Events []Event `yaml:"events"`
	}
	if err := yaml.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("events: %w", err)
	}
	seen := map[string]bool{}
	for i := range f.Events {
		e := &f.Events[i]
		if e.ID == "" || e.Name == "" {
			return nil, fmt.Errorf("events[%d]: id and name are required", i)
		}
		if seen[e.ID] {
			return nil, fmt.Errorf("events: duplicate id %q", e.ID)
		}
		seen[e.ID] = true
		e.Impact = strings.ToUpper(e.Impact)
		if !impacts[e.Impact] {
			return nil, fmt.Errorf("events %s: impact %q is not HIGH, MEDIUM or LOW", e.ID, e.Impact)
		}
		if e.Category == "" {
			return nil, fmt.Errorf("events %s: category is required", e.ID)
		}
		for j, a := range e.Affects {
			e.Affects[j] = strings.ToUpper(a)
			if !indices[e.Affects[j]] {
				return nil, fmt.Errorf("events %s: affects %q is not NIFTY or BANKNIFTY", e.ID, a)
			}
		}
		if err := e.parseWhen(); err != nil {
			return nil, fmt.Errorf("events %s: %w", e.ID, err)
		}
	}
	Sort(f.Events)
	return f.Events, nil
}

func (e *Event) parseWhen() error {
	s := strings.TrimSpace(e.DatetimeIST)
	switch {
	case s == "":
		return nil
	case len(s) == len("2006-01-02"):
		d, err := time.ParseInLocation("2006-01-02", s, clock.IST)
		if err != nil {
			return fmt.Errorf("datetime_ist %q: %w", s, err)
		}
		e.Date = d
	default:
		t, err := time.ParseInLocation("2006-01-02 15:04", s, clock.IST)
		if err != nil {
			return fmt.Errorf("datetime_ist %q: want YYYY-MM-DD HH:MM, YYYY-MM-DD or empty", s)
		}
		e.At, e.Date = t, clock.Date(t)
	}
	return nil
}

// Sort orders events by date, then time (untimed first on a day), then id; TBC last.
func Sort(es []Event) {
	sort.SliceStable(es, func(i, j int) bool {
		a, b := &es[i], &es[j]
		if a.Date.IsZero() != b.Date.IsZero() {
			return !a.Date.IsZero()
		}
		if !a.Date.Equal(b.Date) {
			return a.Date.Before(b.Date)
		}
		if !a.At.Equal(b.At) {
			return a.At.Before(b.At)
		}
		return a.ID < b.ID
	})
}

// ExpiryEvents turns listed F&O expiry dates into MEDIUM "expiry" events at the
// configured time. Dates come from the instrument master, so holiday shifts follow.
func ExpiryEvents(und string, dates []time.Time, at string) ([]Event, error) {
	out := make([]Event, 0, len(dates))
	for _, d := range dates {
		day, err := time.ParseInLocation("2006-01-02", d.Format("2006-01-02"), clock.IST)
		if err != nil {
			return nil, err
		}
		t, err := clock.AtHMS(day, at)
		if err != nil {
			return nil, err
		}
		out = append(out, Event{
			ID: fmt.Sprintf("expiry-%s-%s", strings.ToLower(und), day.Format("2006-01-02")), Name: und + " F&O expiry",
			Country: "IN", Impact: "MEDIUM", Category: "expiry", Affects: []string{und},
			Note: "from the listed instruments", Derived: true, At: t, Date: day,
		})
	}
	return out, nil
}
