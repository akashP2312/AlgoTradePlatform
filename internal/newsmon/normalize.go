package newsmon

import (
	"crypto/sha1"
	"encoding/hex"
	"html"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"pmt/internal/bus"
	"pmt/internal/config"
)

// Item is one headline in news.feed. Times are epoch ms.
type Item struct {
	ID       string   `json:"id"`
	Source   string   `json:"src"`
	Trust    string   `json:"trust"`
	Region   string   `json:"region"`
	Title    string   `json:"title"`
	Summary  string   `json:"summary,omitempty"`
	URL      string   `json:"url,omitempty"`
	Cats     []string `json:"cats,omitempty"`
	Pub      int64    `json:"pub,omitempty"`       // the feed's timestamp; 0 when it had none
	DateOnly bool     `json:"date_only,omitempty"` // the feed gave a day, not a time
	Seen     int64    `json:"seen"`                // when newsmon first saw it
	TS       int64    `json:"ts"`                  // the sort time
	LagMs    int64    `json:"lag_ms,omitempty"`    // seen − pub, for items that arrived while running
	Also     []string `json:"also,omitempty"`      // other sources that carried the same story

	Impact    int       `json:"impact,omitempty"`    // the rules' score, 0-100: how much it may move markets, not which way
	Tags      []string  `json:"tags,omitempty"`      // of the rules it matched
	Rules     []string  `json:"rules,omitempty"`     // the rules it matched, highest first
	Backfill  bool      `json:"backfill,omitempty"`  // from a source's first poll
	Confirmed int64     `json:"confirmed,omitempty"` // when it began to count: a trusted source, or enough sources
	LLM       *LLMScore `json:"llm,omitempty"`       // the optional model rescore

	GUID  string `json:"-"`
	canon string // CanonURL(URL), the dedupe key
}

const summaryMax = 280

var (
	tagRe   = regexp.MustCompile(`(?s)<[^>]*>`)
	dropQ   = map[string]bool{"fbclid": true, "gclid": true, "ref": true, "from": true}
	stopSet = map[string]bool{}
)

func init() {
	for _, w := range strings.Fields(`a an the of to in on for and or by at as is are was were be with from
		its it this that after over amid into vs per s`) {
		stopSet[w] = true
	}
}

// CleanText unescapes HTML (twice: feeds double-escape), drops tags and collapses whitespace.
func CleanText(s string) string {
	s = html.UnescapeString(html.UnescapeString(s))
	s = tagRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	return strings.Join(strings.Fields(s), " ")
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

// CanonURL is the URL two copies of a link share: https, lower-case host, no
// fragment, no tracking parameters, sorted query, no trailing slash.
func CanonURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(html.UnescapeString(raw)))
	if err != nil || u.Host == "" {
		return ""
	}
	if u.Scheme == "http" {
		u.Scheme = "https"
	}
	u.Host = strings.TrimPrefix(strings.ToLower(u.Host), "www.")
	u.Fragment, u.RawFragment = "", ""
	q := u.Query()
	for k := range q {
		if strings.HasPrefix(strings.ToLower(k), "utm_") || dropQ[strings.ToLower(k)] {
			q.Del(k)
		}
	}
	u.RawQuery = q.Encode() // Encode sorts by key
	u.Path = strings.TrimSuffix(u.Path, "/")
	u.RawPath = ""
	return u.String()
}

// titleKey is a title reduced to lower-case letters and digits.
func titleKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// words are a title's distinct content words, for near-duplicate matching.
func words(s string) []string {
	f := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	seen := map[string]bool{}
	var out []string
	for _, w := range f {
		if !stopSet[w] && !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	sort.Strings(out)
	return out
}

func hash12(s string) string {
	h := sha1.Sum([]byte(s))
	return hex.EncodeToString(h[:6])
}

// Normalize turns a raw entry into an Item seen at seen (newsmon's clock).
// ok is false for entries without a title.
func Normalize(src config.Source, loc *time.Location, r RawItem, seen time.Time) (it Item, ok bool) {
	title := CleanText(r.Title)
	desc := CleanText(r.Desc)
	if src.TitleDesc && desc != "" {
		title, desc = title+": "+desc, ""
	}
	if title == "" {
		return Item{}, false
	}
	it = Item{
		Source: src.ID, Trust: src.Trust, Region: src.Region,
		Title: clip(title, 400), GUID: r.GUID, Seen: bus.Ms(seen), TS: bus.Ms(seen),
	}
	if u := strings.TrimSpace(html.UnescapeString(r.Link)); strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://") {
		it.URL, it.canon = u, CanonURL(u)
	}
	if desc != "" && !strings.EqualFold(desc, title) {
		it.Summary = clip(desc, summaryMax)
	}
	for _, c := range r.Cats {
		if c = CleanText(c); c != "" {
			it.Cats = append(it.Cats, c)
		}
	}
	if t, dateOnly, err := ParseTime(CleanText(r.Pub), loc); err == nil {
		it.Pub, it.DateOnly = bus.Ms(t), dateOnly
		if !dateOnly && t.Before(seen) {
			it.TS = bus.Ms(t)
		} else if dateOnly {
			// A day-only item sorts at when it was seen, but no later than its day's end.
			it.TS = min(it.Seen, bus.Ms(t.Add(24*time.Hour)))
		}
	}
	switch {
	case it.canon != "":
		it.ID = hash12("u:" + it.canon)
	case it.GUID != "":
		it.ID = hash12("g:" + src.ID + "|" + it.GUID)
	default:
		it.ID = hash12("t:" + src.ID + "|" + titleKey(title))
	}
	return it, true
}

// age is how old the item is at now: from its time, or for a day-only item from
// the end of its day; zero when it has no timestamp.
// Score is the headline's effective impact: the rules' score, or the model's
// when it is higher. The model can only raise it.
func (it *Item) Score() int {
	if it.LLM != nil && it.LLM.Impact > it.Impact {
		return it.LLM.Impact
	}
	return it.Impact
}

func (it *Item) age(now time.Time) time.Duration {
	if it.Pub == 0 {
		return 0
	}
	t := bus.FromMs(it.Pub)
	if it.DateOnly {
		t = t.Add(24 * time.Hour)
	}
	return now.Sub(t)
}
