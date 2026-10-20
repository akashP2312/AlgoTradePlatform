// Package calendar answers "is this a trading day, and when does it open" from the
// seeded holiday CSV, and projects option expiries from the dated expiry rules.
// Both seeds need checking against exchange circulars; listed expiries in the
// instrument dump always win over projections.
package calendar

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"pmt/internal/clock"
)

type Kind string

const (
	Normal  Kind = "normal"
	Holiday Kind = "holiday"
	Weekend Kind = "weekend"
	Special Kind = "special"
)

// Session is one day's schedule for a segment, in IST.
type Session struct {
	Date  time.Time // midnight IST
	Kind  Kind
	Open  time.Time // zero unless Normal or Special
	Close time.Time
	Note  string
}

func (s Session) Trading() bool { return s.Kind == Normal || s.Kind == Special }

// Default NSE/BSE F&O continuous-session hours.
const (
	DefaultOpen  = "09:15"
	DefaultClose = "15:30"
)

type entry struct {
	kind        Kind
	open, close string
	note        string
}

// Calendar holds the holiday seed for one segment (FO).
type Calendar struct {
	days map[string]entry // yyyy-mm-dd
	min  time.Time
	max  time.Time
}

// Load reads config/calendar/holidays.csv. Lines starting with # are comments.
func Load(path string) (*Calendar, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return Read(f)
}

// Read parses the holiday CSV (date,segment,kind,open,close,note).
func Read(r io.Reader) (*Calendar, error) {
	cr := csv.NewReader(r)
	cr.Comment = '#'
	cr.FieldsPerRecord = -1
	rows, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("calendar: %w", err)
	}
	c := &Calendar{days: map[string]entry{}}
	for i, row := range rows {
		if i == 0 && len(row) > 0 && row[0] == "date" {
			continue
		}
		if len(row) < 3 {
			return nil, fmt.Errorf("calendar: row %d: too few fields", i+1)
		}
		d, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(row[0]), clock.IST)
		if err != nil {
			return nil, fmt.Errorf("calendar: row %d: %w", i+1, err)
		}
		if seg := strings.TrimSpace(row[1]); seg != "" && seg != "FO" {
			continue
		}
		e := entry{kind: Kind(strings.TrimSpace(row[2]))}
		if len(row) > 4 {
			e.open, e.close = strings.TrimSpace(row[3]), strings.TrimSpace(row[4])
		}
		if len(row) > 5 {
			e.note = strings.TrimSpace(row[5])
		}
		switch e.kind {
		case Holiday:
		case Special:
			if _, _, _, err := clock.ParseHMS(e.open); err != nil {
				return nil, fmt.Errorf("calendar: row %d: special session needs open: %w", i+1, err)
			}
			if _, _, _, err := clock.ParseHMS(e.close); err != nil {
				return nil, fmt.Errorf("calendar: row %d: special session needs close: %w", i+1, err)
			}
		default:
			return nil, fmt.Errorf("calendar: row %d: kind %q", i+1, e.kind)
		}
		c.days[d.Format("2006-01-02")] = e
		if c.min.IsZero() || d.Before(c.min) {
			c.min = d
		}
		if d.After(c.max) {
			c.max = d
		}
	}
	return c, nil
}

// Covers reports whether the seed has any entry in d's year; outside it every
// weekday is assumed to trade, which the SYS page flags.
func (c *Calendar) Covers(d time.Time) bool {
	y := d.In(clock.IST).Year()
	return !c.min.IsZero() && y >= c.min.Year() && y <= c.max.Year()
}

// Session returns the F&O schedule for day d.
func (c *Calendar) Session(d time.Time) Session {
	d = clock.Date(d)
	s := Session{Date: d}
	e, ok := c.days[d.Format("2006-01-02")]
	switch {
	case ok && e.kind == Special:
		s.Kind, s.Note = Special, e.note
		s.Open = clock.MustAtHMS(d, e.open)
		s.Close = clock.MustAtHMS(d, e.close)
		return s
	case d.Weekday() == time.Saturday || d.Weekday() == time.Sunday:
		s.Kind = Weekend
		return s
	case ok && e.kind == Holiday:
		s.Kind, s.Note = Holiday, e.note
		return s
	}
	s.Kind = Normal
	s.Open = clock.MustAtHMS(d, DefaultOpen)
	s.Close = clock.MustAtHMS(d, DefaultClose)
	return s
}

// IsTradingDay reports whether d has a normal or special session.
func (c *Calendar) IsTradingDay(d time.Time) bool { return c.Session(d).Trading() }

// Prev returns the last trading day strictly before d.
func (c *Calendar) Prev(d time.Time) time.Time {
	d = clock.Date(d)
	for i := 0; i < 30; i++ {
		d = d.AddDate(0, 0, -1)
		if c.IsTradingDay(d) {
			return d
		}
	}
	return d
}

// Next returns the first trading day strictly after d.
func (c *Calendar) Next(d time.Time) time.Time {
	d = clock.Date(d)
	for i := 0; i < 30; i++ {
		d = d.AddDate(0, 0, 1)
		if c.IsTradingDay(d) {
			return d
		}
	}
	return d
}

// OnOrBefore returns d if it trades, else the previous trading day. Expiries that
// fall on a holiday move to it.
func (c *Calendar) OnOrBefore(d time.Time) time.Time {
	if c.IsTradingDay(d) {
		return clock.Date(d)
	}
	return c.Prev(d)
}

// TradingDays lists trading days in [from, to].
func (c *Calendar) TradingDays(from, to time.Time) []time.Time {
	var out []time.Time
	for d := clock.Date(from); !d.After(clock.Date(to)); d = d.AddDate(0, 0, 1) {
		if c.IsTradingDay(d) {
			out = append(out, d)
		}
	}
	return out
}

// Days lists every session (all kinds) in a year, for seeding the sessions table.
func (c *Calendar) Days(year int) []Session {
	var out []Session
	for d := time.Date(year, 1, 1, 0, 0, 0, 0, clock.IST); d.Year() == year; d = d.AddDate(0, 0, 1) {
		out = append(out, c.Session(d))
	}
	return out
}

// Rule is one dated expiry rule. Weekday 0=Sunday. Monthly means the last such
// weekday of the month. ValidTo is exclusive and zero for open-ended.
type Rule struct {
	Underlying string
	Cycle      string // weekly | monthly
	Weekday    time.Weekday
	ValidFrom  time.Time
	ValidTo    time.Time
	Note       string
}

// LoadRules reads config/expiry_rules.csv.
func LoadRules(path string) ([]Rule, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ReadRules(f)
}

func ReadRules(r io.Reader) ([]Rule, error) {
	cr := csv.NewReader(r)
	cr.Comment = '#'
	cr.FieldsPerRecord = -1
	rows, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("expiry rules: %w", err)
	}
	var out []Rule
	for i, row := range rows {
		if i == 0 && len(row) > 0 && row[0] == "underlying" {
			continue
		}
		if len(row) < 4 {
			return nil, fmt.Errorf("expiry rules: row %d: too few fields", i+1)
		}
		wd, err := strconv.Atoi(strings.TrimSpace(row[2]))
		if err != nil || wd < 0 || wd > 6 {
			return nil, fmt.Errorf("expiry rules: row %d: weekday %q", i+1, row[2])
		}
		r := Rule{Underlying: strings.TrimSpace(row[0]), Cycle: strings.TrimSpace(row[1]), Weekday: time.Weekday(wd)}
		if r.Cycle != "weekly" && r.Cycle != "monthly" {
			return nil, fmt.Errorf("expiry rules: row %d: cycle %q", i+1, r.Cycle)
		}
		if r.ValidFrom, err = time.ParseInLocation("2006-01-02", strings.TrimSpace(row[3]), clock.IST); err != nil {
			return nil, fmt.Errorf("expiry rules: row %d: %w", i+1, err)
		}
		if len(row) > 4 && strings.TrimSpace(row[4]) != "" {
			if r.ValidTo, err = time.ParseInLocation("2006-01-02", strings.TrimSpace(row[4]), clock.IST); err != nil {
				return nil, fmt.Errorf("expiry rules: row %d: %w", i+1, err)
			}
		}
		if len(row) > 5 {
			r.Note = strings.TrimSpace(row[5])
		}
		out = append(out, r)
	}
	return out, nil
}

func (r Rule) activeOn(d time.Time) bool {
	return !d.Before(r.ValidFrom) && (r.ValidTo.IsZero() || d.Before(r.ValidTo))
}

// Expiry is a projected expiry date.
type Expiry struct {
	Date    time.Time // midnight IST, after the holiday shift
	Nominal time.Time // the rule's weekday before the shift
	Cycle   string
}

// Project lists the expiries of underlying in [from, to] implied by the rules,
// shifted to the previous trading day when they fall on a holiday. A date that is
// both a weekly and a monthly expiry is reported once, as monthly.
func Project(c *Calendar, rules []Rule, underlying string, from, to time.Time) []Expiry {
	from, to = clock.Date(from), clock.Date(to)
	byDate := map[string]Expiry{}
	// Nominal dates can shift earlier than from; scan a week past to.
	for d := from; !d.After(to.AddDate(0, 0, 7)); d = d.AddDate(0, 0, 1) {
		for _, r := range rules {
			if r.Underlying != underlying || d.Weekday() != r.Weekday || !r.activeOn(d) {
				continue
			}
			if r.Cycle == "monthly" && d.AddDate(0, 0, 7).Month() == d.Month() {
				continue // not the last such weekday
			}
			e := Expiry{Date: c.OnOrBefore(d), Nominal: d, Cycle: r.Cycle}
			if e.Date.Before(from) || e.Date.After(to) {
				continue
			}
			key := e.Date.Format("2006-01-02")
			if prev, ok := byDate[key]; ok && prev.Cycle == "monthly" {
				continue
			}
			byDate[key] = e
		}
	}
	out := make([]Expiry, 0, len(byDate))
	for _, e := range byDate {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date.Before(out[j].Date) })
	return out
}

// Years returns the first and last year the holiday seed covers (0, 0 when empty).
func (c *Calendar) Years() (first, last int) {
	if c.min.IsZero() {
		return 0, 0
	}
	return c.min.Year(), c.max.Year()
}
