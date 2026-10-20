// Package clock is the single source of "now" for every service. In production it
// is the wall clock in IST; for dry runs PMT_CLOCK_OFFSET shifts it (for example
// "-21600s") so a full session can be replayed at any hour against fakekite.
package clock

import (
	"fmt"
	"os"
	"strings"
	"time"
	_ "time/tzdata" // IST must resolve inside minimal containers
)

// IST is Asia/Kolkata. India has no DST, so a fixed zone would also work, but the
// named zone keeps logs readable.
var IST *time.Location

var offset time.Duration

func init() {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		loc = time.FixedZone("IST", 5*3600+1800)
	}
	IST = loc
	if v := strings.TrimSpace(os.Getenv("PMT_CLOCK_OFFSET")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			panic(fmt.Sprintf("PMT_CLOCK_OFFSET %q: %v", v, err))
		}
		offset = d
	}
}

// Offset is the dry-run shift applied to the wall clock (zero in production).
func Offset() time.Duration { return offset }

// SetOffset is for tests.
func SetOffset(d time.Duration) { offset = d }

// Now returns the shifted wall clock in IST.
func Now() time.Time { return time.Now().Add(offset).In(IST) }

// Real converts a shifted time back to the wall clock, for timers.
func Real(t time.Time) time.Time { return t.Add(-offset) }

// Until is the real duration until shifted time t.
func Until(t time.Time) time.Duration { return t.Sub(Now()) }

// Date returns midnight IST of t's IST calendar day.
func Date(t time.Time) time.Time {
	t = t.In(IST)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, IST)
}

// Today is Date(Now()).
func Today() time.Time { return Date(Now()) }

// At returns the instant hh:mm:ss IST on day d.
func At(d time.Time, hh, mm, ss int) time.Time {
	d = d.In(IST)
	return time.Date(d.Year(), d.Month(), d.Day(), hh, mm, ss, 0, IST)
}

// ParseHMS parses "15:25" or "15:25:00".
func ParseHMS(s string) (hh, mm, ss int, err error) {
	var n int
	n, err = fmt.Sscanf(s, "%d:%d:%d", &hh, &mm, &ss)
	if err != nil && n == 2 {
		err = nil
	}
	if err != nil {
		return 0, 0, 0, fmt.Errorf("time %q: want hh:mm[:ss]", s)
	}
	if hh < 0 || hh > 23 || mm < 0 || mm > 59 || ss < 0 || ss > 59 {
		return 0, 0, 0, fmt.Errorf("time %q out of range", s)
	}
	return hh, mm, ss, nil
}

// AtHMS is At with a "hh:mm[:ss]" string.
func AtHMS(d time.Time, s string) (time.Time, error) {
	hh, mm, ss, err := ParseHMS(s)
	if err != nil {
		return time.Time{}, err
	}
	return At(d, hh, mm, ss), nil
}

// MustAtHMS panics on a bad literal; for constants only.
func MustAtHMS(d time.Time, s string) time.Time {
	t, err := AtHMS(d, s)
	if err != nil {
		panic(err)
	}
	return t
}

// Minute truncates t to the minute in IST.
func Minute(t time.Time) time.Time { return t.In(IST).Truncate(time.Minute) }

// SleepUntil blocks until shifted time t or until done closes. It returns false if
// done closed first.
func SleepUntil(done <-chan struct{}, t time.Time) bool {
	d := Until(t)
	if d <= 0 {
		return true
	}
	tm := time.NewTimer(d)
	defer tm.Stop()
	select {
	case <-tm.C:
		return true
	case <-done:
		return false
	}
}

// OffsetTo returns the offset that makes Now() read hh:mm:ss IST on day
// (YYYY-MM-DD, or today when empty), for `make dryrun START=09:14:30 DATE=...`.
func OffsetTo(hms, day string) (time.Duration, error) {
	real := time.Now().In(IST)
	d := real
	if day != "" {
		var err error
		if d, err = time.ParseInLocation(time.DateOnly, day, IST); err != nil {
			return 0, err
		}
	}
	target, err := AtHMS(d, hms)
	if err != nil {
		return 0, err
	}
	return target.Sub(real).Truncate(time.Second), nil
}
