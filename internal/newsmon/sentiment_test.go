package newsmon

import (
	"testing"
	"time"

	"pmt/internal/bus"
)

// Synthetic headlines with the mood their tone must land in.
var toneFixtures = []struct {
	title, mood string
}{
	{"Sensex tumbles 1,200 points as global selloff deepens", "neg"},
	{"Nifty crashes 3% in bloodbath; investors lose Rs 9 lakh crore", "neg"},
	{"Rupee slides to record low against the dollar", "neg"},
	{"FPI outflows continue for a fifth week", "neg"},
	{"Moody's downgrades outlook; banks fall", "neg"},
	{"US slips toward recession as payrolls disappoint", "neg"},
	{"Sensex soars 900 points, Nifty ends at record high", "pos"},
	{"Markets rally as RBI signals rate cut", "pos"},
	{"Bank Nifty rebounds after strong quarterly growth", "pos"},
	{"Ceasefire deal brings relief to oil markets", "pos"},
	{"FII inflows boost Dalal Street", "pos"},
	{"RBI keeps repo rate unchanged at 5.5%", "neu"},
	{"SEBI issues consultation paper on index options", "neu"},
	{"Muhurat trading session timings announced", "neu"},
	// negation flips the phrase after it
	{"Analysts say it is not a crash, just a correction", "pos"},
	{"Nifty fails to rally despite strong earnings", "neg"},
	{"Markets don't fall despite weak cues", "pos"},
	{"Talks avert war in the region", "pos"},
	// mixed headlines lean toward their larger side, or stay neutral
	{"Sensex falls 300 points, then recovers", "neu"},
}

func TestToneFixtures(t *testing.T) {
	rs := repoRules(t)
	for _, f := range toneFixtures {
		got := rs.Sentiment.Tone(f.title)
		if got < -1 || got > 1 || toneMood(got) != f.mood {
			t.Errorf("%q: tone %.2f = %s, want %s", f.title, got, toneMood(got), f.mood)
		}
	}
}

func TestToneLongestMatch(t *testing.T) {
	rs := repoRules(t)
	// "record low" is one negative hit, not a positive "record high" or two hits.
	if got := rs.Sentiment.Tone("Rupee at record low"); got != -0.5 {
		t.Fatalf("record low = %v", got)
	}
	if got := rs.Sentiment.Tone("no change"); got != 0 {
		t.Fatalf("negate alone = %v", got)
	}
}

func TestFeedTone(t *testing.T) {
	now := fixtureNow
	items := []Item{
		{ID: "a", TS: bus.Ms(now.Add(-10 * time.Minute)), Tone: -0.6, Mood: "neg", Impact: 90},
		{ID: "b", TS: bus.Ms(now.Add(-20 * time.Minute)), Tone: 0.5, Mood: "pos", Impact: 10},
		{ID: "c", TS: bus.Ms(now.Add(-30 * time.Minute)), Tone: 0, Mood: "neu"},
		{ID: "d", TS: bus.Ms(now.Add(-5 * time.Minute)), Tone: 0.9, Tags: []string{"noise"}},
		{ID: "e", TS: bus.Ms(now.Add(-2 * time.Hour)), Tone: 0.9},
	}
	tn := feedTone(items, now, time.Hour)
	if tn == nil || tn.N != 3 || tn.Neg != 1 || tn.Neu != 1 || tn.Pos != 1 || tn.Mood != "neg" || tn.Score >= 0 {
		t.Fatalf("tone = %+v", tn)
	}
	if tn := feedTone(items[3:], now, time.Hour); tn.N != 0 || tn.Mood != "neu" {
		t.Fatal("noise and old items counted")
	}
}
