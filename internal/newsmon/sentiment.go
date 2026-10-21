package newsmon

import (
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"

	"pmt/internal/bus"
)

// Sentiment is the rules file's tone lexicon. A headline's tone runs from -1
// (clearly negative for markets) to +1: (positive − negative) / (hits + 1), a
// phrase counting the other way when one of Negate's phrases ends within the
// three words before it. Tone is shown beside the feed for context; it never
// sets the risk state and is never a trade signal.
type Sentiment struct {
	Negative []string `yaml:"negative"`
	Positive []string `yaml:"positive"`
	Negate   []string `yaml:"negate"`

	neg, pos, not []*regexp.Regexp
}

func (s *Sentiment) compile() error {
	var err error
	for _, p := range []struct {
		name string
		src  []string
		dst  *[]*regexp.Regexp
	}{{"negative", s.Negative, &s.neg}, {"positive", s.Positive, &s.pos}, {"negate", s.Negate, &s.not}} {
		if *p.dst, err = compile(p.src); err != nil {
			return fmt.Errorf("sentiment.%s: %w", p.name, err)
		}
	}
	return nil
}

// toneMood labels a tone; between ±moodAt it is neutral.
const moodAt = 0.2

func toneMood(t float64) string {
	switch {
	case t <= -moodAt:
		return "neg"
	case t >= moodAt:
		return "pos"
	}
	return "neu"
}

// Tone scores one title. Overlapping phrases count once, the longest first.
func (s *Sentiment) Tone(title string) float64 {
	if s == nil || len(s.neg)+len(s.pos) == 0 {
		return 0
	}
	title = quotes.Replace(title)
	type span struct {
		a, b int
		sign int
	}
	var spans []span
	for _, g := range []struct {
		rs   []*regexp.Regexp
		sign int
	}{{s.neg, -1}, {s.pos, 1}} {
		for _, r := range g.rs {
			for _, m := range r.FindAllStringIndex(title, -1) {
				spans = append(spans, span{m[0], m[1], g.sign})
			}
		}
	}
	slices.SortFunc(spans, func(x, y span) int {
		if x.a != y.a {
			return x.a - y.a
		}
		return (y.b - y.a) - (x.b - x.a)
	})
	var p, n, end int
	for _, sp := range spans {
		if sp.a < end {
			continue
		}
		end = sp.b
		sign := sp.sign
		if s.negated(title[:sp.a]) {
			sign = -sign
		}
		if sign > 0 {
			p++
		} else {
			n++
		}
	}
	if p+n == 0 {
		return 0
	}
	return math.Round(float64(p-n)/float64(p+n+1)*100) / 100
}

// negated: one of the negating phrases is in the last three words of before.
func (s *Sentiment) negated(before string) bool {
	w := strings.Fields(before)
	if len(w) > 3 {
		w = w[len(w)-3:]
	}
	return anyOf(s.not, strings.Join(w, " "))
}

// Tone is news.feed's tone over the last Window: the impact-weighted mean of its
// headlines' tones (noise excluded), and how many read negative, neutral and
// positive. Display only.
type Tone struct {
	Window int64   `json:"window"` // ms
	N      int     `json:"n"`
	Score  float64 `json:"score"`
	Mood   string  `json:"mood"`
	Neg    int     `json:"neg"`
	Neu    int     `json:"neu"`
	Pos    int     `json:"pos"`
}

func feedTone(items []Item, now time.Time, window time.Duration) *Tone {
	if window <= 0 {
		return nil
	}
	t := &Tone{Window: window.Milliseconds(), Mood: "neu"}
	from := bus.Ms(now.Add(-window))
	var sum, wsum float64
	for i := range items {
		it := &items[i]
		if it.TS < from || slices.Contains(it.Tags, "noise") {
			continue
		}
		t.N++
		switch it.Mood {
		case "neg":
			t.Neg++
		case "pos":
			t.Pos++
		default:
			t.Neu++
		}
		w := float64(max(it.Score(), 10))
		sum += w * it.Tone
		wsum += w
	}
	if wsum > 0 {
		t.Score = math.Round(sum/wsum*100) / 100
		t.Mood = toneMood(t.Score)
	}
	return t
}
