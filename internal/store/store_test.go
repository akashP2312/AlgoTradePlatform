package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"pmt/internal/calendar"
	"pmt/internal/clock"
)

// Integration tests run only when PMT_TEST_DB points at a disposable database.
func testPool(t *testing.T) context.Context {
	t.Helper()
	if os.Getenv("PMT_TEST_DB") == "" {
		t.Skip("PMT_TEST_DB not set")
	}
	return context.Background()
}

func TestMigrateSeedJobs(t *testing.T) {
	ctx := testPool(t)
	pool, err := Open(ctx, os.Getenv("PMT_TEST_DB"), 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	if _, err := Migrate(ctx, pool); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	again, err := Migrate(ctx, pool)
	if err != nil || len(again) != 0 {
		t.Fatalf("second migrate applied %v, err %v", again, err)
	}

	cal, err := calendar.Load("../../config/calendar/holidays.csv")
	if err != nil {
		t.Fatal(err)
	}
	rules, err := calendar.LoadRules("../../config/expiry_rules.csv")
	if err != nil {
		t.Fatal(err)
	}
	n, err := SeedCalendar(ctx, pool, cal, rules, 2026)
	if err != nil || n != 365 {
		t.Fatalf("seed: n=%d err=%v", n, err)
	}
	var kind, open string
	if err := pool.QueryRow(ctx, `SELECT kind, COALESCE(open_at::text, '') FROM sessions
		WHERE trade_date = '2026-11-08' AND segment = 'FO'`).Scan(&kind, &open); err != nil {
		t.Fatal(err)
	}
	if kind != "special" || open != "18:00:00" {
		t.Fatalf("2026-11-08 = %s %s", kind, open)
	}
	var d time.Time
	if err := pool.QueryRow(ctx, `SELECT trade_date FROM sessions WHERE trade_date = $1`,
		clock.At(time.Date(2026, 10, 20, 0, 0, 0, 0, clock.IST), 0, 0, 0)).Scan(&d); err != nil {
		t.Fatalf("IST midnight date round trip: %v", err)
	}
	if d.Format("2006-01-02") != "2026-10-20" {
		t.Fatalf("date round trip gave %v", d)
	}

	j, err := StartJob(ctx, pool, "test", "--x")
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Finish(ctx, "partial", errors.New("boom")); err != nil {
		t.Fatal(err)
	}
	jobs, err := LastJobs(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range jobs {
		if s.Job == "test" {
			found = s.Status == "error" && s.Detail == "partial: boom"
		}
	}
	if !found {
		t.Fatalf("job row not recorded: %+v", jobs)
	}
}
