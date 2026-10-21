package main

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

func TestParseBhavFile(t *testing.T) {
	csv, err := os.ReadFile("../../internal/loaders/testdata/udiff.csv")
	if err != nil {
		t.Fatal(err)
	}
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	w, _ := zw.Create("BhavCopy_NSE_FO_0_0_0_20261016_F_0000.csv")
	w.Write(csv)
	zw.Close()
	for name, raw := range map[string][]byte{"csv": csv, "zip": zbuf.Bytes()} {
		rows, err := parseBhavFile(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := rows[0].TradeDate.Format(time.DateOnly); got != "2026-10-16" || len(rows) == 0 {
			t.Errorf("%s: %d rows for %s", name, len(rows), got)
		}
	}
	// A file with two trade dates is refused whole.
	lines := strings.Split(strings.TrimSpace(string(csv)), "\n")
	lines[2] = strings.Replace(lines[2], "2026-10-16,2026-10-16", "2026-10-15,2026-10-15", 1)
	if _, err := parseBhavFile([]byte(strings.Join(lines, "\n") + "\n")); err == nil || !strings.Contains(err.Error(), "mixes trade dates") {
		t.Errorf("mixed dates: %v", err)
	}
	if _, err := parseBhavFile([]byte("not,a,bhavcopy\n1,2,3\n")); err == nil {
		t.Error("garbage parsed")
	}
}

func TestBhavFiles(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"b.csv.zip", "a.CSV", ".hidden.zip", "notes.txt", "c.zip.crdownload"} {
		os.WriteFile(filepath.Join(dir, n), nil, 0o600)
	}
	os.Mkdir(filepath.Join(dir, "sub.zip"), 0o700)
	got, err := bhavFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range got {
		names = append(names, filepath.Base(f))
	}
	if s := strings.Join(names, ","); s != "a.CSV,b.csv.zip" {
		t.Errorf("files %s", s)
	}
}
