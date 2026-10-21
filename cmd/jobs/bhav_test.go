package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"pmt/internal/loaders"
)

func TestBhavRange(t *testing.T) {
	d := func(s string) time.Time { t, _ := time.Parse(time.DateOnly, s); return t }
	weekday := func(x time.Time) bool { return x.Weekday() != time.Saturday && x.Weekday() != time.Sunday }
	have := map[string]bool{"2026-10-06": true}
	var loaded []string
	load := func(x time.Time) (string, error) {
		s := x.Format(time.DateOnly)
		if s == "2026-10-08" {
			return "", fmt.Errorf("x: %w", loaders.ErrNotPublished)
		}
		loaded = append(loaded, s)
		return "ok " + s, nil
	}
	has := func(x time.Time) (bool, error) { return have[x.Format(time.DateOnly)], nil }

	// Mon 5 Oct to Mon 12 Oct 2026: six weekdays, one already loaded, one 404.
	out, err := bhavRange(context.Background(), weekday, d("2026-10-05"), d("2026-10-12"), 0, has, load)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(loaded, ","); got != "2026-10-05,2026-10-07,2026-10-09,2026-10-12" {
		t.Errorf("loaded %s", got)
	}
	if !strings.Contains(out, "4 loaded, 1 already there, 1 not published (2026-10-08)") {
		t.Errorf("summary %q", out)
	}

	// Any other error stops the run at that day.
	loaded = nil
	boom := func(x time.Time) (string, error) {
		if x.Format(time.DateOnly) == "2026-10-07" {
			return "", errors.New("HTTP 403")
		}
		loaded = append(loaded, x.Format(time.DateOnly))
		return "", nil
	}
	_, err = bhavRange(context.Background(), weekday, d("2026-10-05"), d("2026-10-12"), 0, has, boom)
	if err == nil || !strings.Contains(err.Error(), "2026-10-07: HTTP 403 (loaded 1, skipped 1") {
		t.Errorf("err %v", err)
	}
	if len(loaded) != 1 {
		t.Errorf("kept going after the error: %v", loaded)
	}

	// A cancelled context stops during the pause.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := bhavRange(ctx, weekday, d("2026-10-05"), d("2026-10-12"), time.Hour, has, load); !errors.Is(err, context.Canceled) {
		t.Errorf("cancel: %v", err)
	}
}
