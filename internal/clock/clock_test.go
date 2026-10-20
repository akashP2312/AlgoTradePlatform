package clock

import (
	"testing"
	"time"
)

func TestOffsetAndHelpers(t *testing.T) {
	SetOffset(0)
	d, err := OffsetTo("09:14:30", "2026-10-19")
	if err != nil {
		t.Fatal(err)
	}
	SetOffset(d)
	defer SetOffset(0)
	if n := Now(); n.Format(time.DateOnly) != "2026-10-19" {
		t.Fatalf("shifted date = %v", n)
	}
	if d, err = OffsetTo("09:14:30", ""); err != nil {
		t.Fatal(err)
	}
	SetOffset(d)
	n := Now()
	if n.Hour() != 9 || n.Minute() != 14 {
		t.Fatalf("shifted now = %v", n)
	}
	if got := Minute(At(n, 9, 15, 42)); got.Second() != 0 || got.Minute() != 15 {
		t.Fatalf("minute floor = %v", got)
	}
	if _, _, _, err := ParseHMS("25:00"); err == nil {
		t.Fatal("want range error")
	}
	if h, m, s, err := ParseHMS("15:25"); err != nil || h != 15 || m != 25 || s != 0 {
		t.Fatalf("ParseHMS = %d %d %d %v", h, m, s, err)
	}
	if Date(time.Date(2026, 10, 19, 20, 0, 0, 0, time.UTC)).Day() != 20 {
		t.Fatal("20:00 UTC is the next IST day")
	}
}
