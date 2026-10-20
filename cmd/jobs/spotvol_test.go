package main

import (
	"math"
	"testing"
	"time"
)

func TestFitBeta(t *testing.T) {
	// Synthetic history: VIX moves -1.5 pts per 1% fall and -0.4 per 1% rise;
	// BANKNIFTY moves 1.25x NIFTY.
	closes := map[string]map[string]float64{"VIX": {}, "NIFTY": {}, "BANKNIFTY": {}}
	d := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	n, v, b := 20000.0, 14.0, 45000.0
	for i := 0; i < 120; i++ {
		day := d.AddDate(0, 0, i).Format(time.DateOnly)
		closes["NIFTY"][day], closes["VIX"][day], closes["BANKNIFTY"][day] = n, v, b
		r := 0.01 * math.Sin(float64(i)*1.7) // in [-1%, 1%]
		if r < 0 {
			v += -1.5 * 100 * r
		} else {
			v += -0.4 * 100 * r
		}
		n *= 1 + r
		b *= 1 + 1.25*r
	}
	f, err := fitBeta(closes, "VIX", "NIFTY")
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(f.Dn+1.5) > 0.01 || math.Abs(f.Up+0.4) > 0.01 {
		t.Fatalf("dn %v up %v, want -1.5 -0.4", f.Dn, f.Up)
	}
	if math.Abs(f.Und["BANKNIFTY"]-1.25) > 0.01 {
		t.Fatalf("banknifty beta %v, want 1.25", f.Und["BANKNIFTY"])
	}
	if f.N != 119 || f.Src != "index_eod" || f.Asof == "" {
		t.Fatalf("bad meta %+v", f)
	}
}

func TestFitBetaTooShort(t *testing.T) {
	closes := map[string]map[string]float64{"VIX": {"2026-01-01": 14, "2026-01-02": 15}, "NIFTY": {"2026-01-01": 1, "2026-01-02": 0.99}}
	if _, err := fitBeta(closes, "VIX", "NIFTY"); err == nil {
		t.Fatal("want an error with two days")
	}
}
