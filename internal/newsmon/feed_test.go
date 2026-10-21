package newsmon

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"pmt/internal/bus"
	"pmt/internal/clock"
	"pmt/internal/config"
)

// The fixtures are synthetic headlines in the shapes the real feeds use.
var fixtureSources = []config.Source{
	{ID: "et_markets", Trust: "media", Region: "IN", TZ: "Asia/Kolkata"},
	{ID: "mint_markets", Trust: "media", Region: "IN", TZ: "Asia/Kolkata"},
	{ID: "bs_markets", Trust: "media", Region: "IN", TZ: "Asia/Kolkata"},
	{ID: "rbi_press", Trust: "official", Region: "IN", TZ: "Asia/Kolkata"},
	{ID: "sebi", Trust: "official", Region: "IN", TZ: "Asia/Kolkata"},
	{ID: "fed_press", Trust: "official", Region: "US", TZ: "America/New_York"},
	{ID: "nse_ann", Trust: "exchange", Region: "IN", TZ: "Asia/Kolkata", TitleDesc: true},
	{ID: "atom", Trust: "media", Region: "US", TZ: "UTC"},
	{ID: "rdf", Trust: "media", Region: "JP", TZ: "Asia/Tokyo"},
	{ID: "cp1252", Trust: "media", Region: "IN", TZ: "Asia/Kolkata"},
}

// fixtureNow is when the fixtures are read: 21 Oct 2026, 10:05 IST.
var fixtureNow = time.Date(2026, 10, 21, 10, 5, 0, 0, clock.IST)

func testFeeds() config.Feeds {
	return config.Feeds{
		Poll: time.Minute, Timeout: 2 * time.Second, UserAgent: "pmt-news/0.1 (+test)",
		Keep: 200, DedupeTTL: 24 * time.Hour, MaxAge: 48 * time.Hour, Similar: 0.8,
		Fails: 3, Cool: 15 * time.Minute, BackoffMax: 10 * time.Minute, Robots: 24 * time.Hour,
	}
}

func readFixture(t *testing.T, id string) []RawItem {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "feeds", id+".xml"))
	if err != nil {
		t.Fatal(err)
	}
	raws, err := ParseFeed(b)
	if err != nil {
		t.Fatalf("%s: %v", id, err)
	}
	return raws
}

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	l, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestParseFeedFixtures(t *testing.T) {
	want := map[string]int{"et_markets": 5, "mint_markets": 4, "bs_markets": 4, "rbi_press": 4, "sebi": 4,
		"fed_press": 3, "nse_ann": 3, "atom": 3, "rdf": 2, "cp1252": 1}
	total := 0
	for _, s := range fixtureSources {
		raws := readFixture(t, s.ID)
		if len(raws) != want[s.ID] {
			t.Errorf("%s: %d entries, want %d", s.ID, len(raws), want[s.ID])
		}
		for _, r := range raws {
			if strings.TrimSpace(r.Title) == "" || r.Pub == "" {
				t.Errorf("%s: entry without title or time: %+v", s.ID, r)
			}
		}
		total += len(raws)
	}
	if total < 30 {
		t.Fatalf("only %d fixture headlines", total)
	}

	atom := readFixture(t, "atom")
	if atom[0].Link != "https://example.com/markets/us-yields-climb" || atom[0].Pub != "2026-10-21T08:45:00+05:30" ||
		!slices.Equal(atom[0].Cats, []string{"bonds", "us"}) {
		t.Errorf("atom alternate link, published, categories: %+v", atom[0])
	}
	if atom[1].Pub != "2026-10-21T02:15:00Z" || !strings.Contains(atom[1].Desc, "Nikkei") {
		t.Errorf("atom updated and content fallback: %+v", atom[1])
	}
	if atom[2].Link != "" || atom[2].GUID != "tag:example.com,2026:3" {
		t.Errorf("atom entry without link: %+v", atom[2])
	}
	if rdf := readFixture(t, "rdf"); rdf[0].Pub != "2026-10-21T05:30:00+09:00" || rdf[0].Link != "https://example.org/a" {
		t.Errorf("rdf dc:date: %+v", rdf[0])
	}
	if cp := readFixture(t, "cp1252"); cp[0].Title != "Café Coffee Day shares jump 8% — lender settles dues" {
		t.Errorf("windows-1252 title = %q", cp[0].Title)
	}
	if fed := readFixture(t, "fed_press"); fed[0].GUID == "" || fed[0].Cats[0] != "Monetary Policy" {
		t.Errorf("fed guid and category: %+v", fed[0])
	}
}

func TestParseFeedRejects(t *testing.T) {
	for _, b := range []string{"<html><body>Service unavailable</body></html>", "", "not xml at all"} {
		if _, err := ParseFeed([]byte(b)); err == nil {
			t.Errorf("ParseFeed(%q) accepted", b)
		}
	}
	// An empty but valid feed is not an error.
	if raws, err := ParseFeed([]byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>x</title></channel></rss>`)); err != nil || len(raws) != 0 {
		t.Errorf("empty feed: %v %v", raws, err)
	}
	// A BOM and HTML entities are tolerated.
	raws, err := ParseFeed([]byte("\xef\xbb\xbf<rss><channel><item><title>Q&nbsp;2 results &mdash; beat</title></item></channel></rss>"))
	if err != nil || len(raws) != 1 || CleanText(raws[0].Title) != "Q 2 results — beat" {
		t.Errorf("bom and entities: %+v %v", raws, err)
	}
}

func TestParseTime(t *testing.T) {
	ist, ny := clock.IST, mustLoc(t, "America/New_York")
	utc := func(y int, m time.Month, d, hh, mm, ss int) time.Time {
		return time.Date(y, m, d, hh, mm, ss, 0, time.UTC)
	}
	cases := []struct {
		in       string
		loc      *time.Location
		want     time.Time
		dateOnly bool
	}{
		{"Wed, 21 Oct 2026 09:58:00 +0530", ist, utc(2026, 10, 21, 4, 28, 0), false},
		{"  Wed,  21 Oct 2026\n09:58:00 +0530 ", ist, utc(2026, 10, 21, 4, 28, 0), false},
		{"Thu, 1 Oct 2026 09:00:00 +0530", ist, utc(2026, 10, 1, 3, 30, 0), false},
		{"Wed, 21 Oct 2026 09:17:40 IST", ny, utc(2026, 10, 21, 3, 47, 40), false},
		{"Tue, 20 Oct 2026 14:30:00 EDT", ist, utc(2026, 10, 20, 18, 30, 0), false},
		{"Tue, 20 Oct 2026 18:00:00 GMT", ist, utc(2026, 10, 20, 18, 0, 0), false},
		{"Tue, 20 Oct 2026 14:30:00", ny, utc(2026, 10, 20, 18, 30, 0), false},
		{"Wed, 21 Oct 2026 10:00:00", ist, utc(2026, 10, 21, 4, 30, 0), false},
		{"21-Oct-2026 10:01:22", ist, utc(2026, 10, 21, 4, 31, 22), false},
		{"2026-10-21T08:45:00+05:30", ny, utc(2026, 10, 21, 3, 15, 0), false},
		{"2026-10-21T03:00:00.250Z", ist, time.Date(2026, 10, 21, 3, 0, 0, 250e6, time.UTC), false},
		{"2026-10-21 08:00:00", mustLoc(t, "Asia/Tokyo"), utc(2026, 10, 20, 23, 0, 0), false},
		{"21 Oct, 2026 +0530", ny, utc(2026, 10, 20, 18, 30, 0), true},
		{"21 Oct, 2026", ist, utc(2026, 10, 20, 18, 30, 0), true},
		{"2026-10-21", ny, utc(2026, 10, 21, 4, 0, 0), true},
	}
	for _, c := range cases {
		got, dateOnly, err := ParseTime(c.in, c.loc)
		if err != nil || !got.Equal(c.want) || dateOnly != c.dateOnly {
			t.Errorf("ParseTime(%q) = %v %v %v, want %v %v", c.in, got.UTC(), dateOnly, err, c.want, c.dateOnly)
		}
	}
	for _, s := range []string{"", "   ", "yesterday", "21/10/2026"} {
		if _, _, err := ParseTime(s, ist); err == nil {
			t.Errorf("ParseTime(%q) accepted", s)
		}
	}
}

func TestCanonURL(t *testing.T) {
	cases := map[string]string{
		"http://www.Example.com/a/?utm_source=x&b=2&a=1#frag":         "https://example.com/a?a=1&b=2",
		"https://example.com/a?fbclid=1&gclid=2&ref=rss":              "https://example.com/a",
		"https://x.com/a?x=1&amp;utm_medium=feed":                     "https://x.com/a?x=1",
		" https://economictimes.indiatimes.com/a/articleshow/101.cms": "https://economictimes.indiatimes.com/a/articleshow/101.cms",
		"not a url": "",
		"/relative": "",
	}
	for in, want := range cases {
		if got := CanonURL(in); got != want {
			t.Errorf("CanonURL(%q) = %q, want %q", in, got, want)
		}
	}
	if CanonURL("http://www.livemint.com/x.html?utm_source=a") != CanonURL("https://livemint.com/x.html") {
		t.Error("http/www copies differ")
	}
}

func TestNormalizeFixtures(t *testing.T) {
	ist := clock.IST
	et := readFixture(t, "et_markets")
	it, ok := Normalize(fixtureSources[0], ist, et[0], fixtureNow)
	if !ok {
		t.Fatal("ET entry dropped")
	}
	if it.Title != "RBI keeps repo rate unchanged at 5.5%, maintains neutral stance" ||
		it.Summary != "The Monetary Policy Committee voted 5-1 to hold rates." ||
		it.Source != "et_markets" || it.Trust != "media" || it.Region != "IN" {
		t.Errorf("ET item: %+v", it)
	}
	if it.canon != "https://economictimes.indiatimes.com/markets/rbi-policy/articleshow/101.cms" || len(it.ID) != 12 {
		t.Errorf("ET canon %q id %q", it.canon, it.ID)
	}
	if it.Pub != bus.Ms(time.Date(2026, 10, 21, 9, 58, 0, 0, ist)) || it.TS != it.Pub || it.Seen != bus.Ms(fixtureNow) {
		t.Errorf("ET times: pub %d ts %d seen %d", it.Pub, it.TS, it.Seen)
	}
	if crude, _ := Normalize(fixtureSources[0], ist, et[3], fixtureNow); crude.Title != "Crude oil prices & OPEC+ output: what to watch" {
		t.Errorf("double-escaped title = %q", crude.Title)
	}

	mint, _ := Normalize(fixtureSources[1], ist, readFixture(t, "mint_markets")[0], fixtureNow)
	if mint.Summary != "" {
		t.Errorf("summary repeating the title kept: %q", mint.Summary)
	}

	sebi, _ := Normalize(fixtureSources[4], ist, readFixture(t, "sebi")[0], fixtureNow)
	if !sebi.DateOnly || sebi.TS != sebi.Seen || sebi.age(fixtureNow) >= 0 {
		t.Errorf("date-only item: %+v age %v", sebi, sebi.age(fixtureNow))
	}
	// Seen after its day ended, a date-only item sorts at the day's end.
	late := time.Date(2026, 10, 23, 9, 0, 0, 0, ist)
	if s, _ := Normalize(fixtureSources[4], ist, readFixture(t, "sebi")[0], late); s.TS != bus.Ms(time.Date(2026, 10, 22, 0, 0, 0, 0, ist)) {
		t.Errorf("late date-only ts = %v", bus.FromMs(s.TS))
	}

	nse, _ := Normalize(fixtureSources[6], ist, readFixture(t, "nse_ann")[0], fixtureNow)
	if nse.Title != "Infosys Limited: Outcome of Board Meeting" || nse.Summary != "" {
		t.Errorf("title_desc: %+v", nse)
	}

	atom := readFixture(t, "atom")
	noLink, _ := Normalize(fixtureSources[7], time.UTC, atom[2], fixtureNow)
	if noLink.URL != "" || noLink.ID != hash12("g:atom|tag:example.com,2026:3") {
		t.Errorf("atom without link: %+v", noLink)
	}

	// A timestamp in the future sorts at when it was seen, not ahead of it.
	future := RawItem{Title: "Early bird headline from a skewed clock", Pub: "Wed, 21 Oct 2026 11:00:00 +0530"}
	if f, _ := Normalize(fixtureSources[0], ist, future, fixtureNow); f.TS != f.Seen {
		t.Errorf("future pub ts = %v", bus.FromMs(f.TS))
	}
	if _, ok := Normalize(fixtureSources[0], ist, RawItem{Title: " <b></b> ", Link: "https://x.com"}, fixtureNow); ok {
		t.Error("untitled entry kept")
	}
	if j, _ := Normalize(fixtureSources[0], ist, RawItem{Title: "x", Link: "javascript:alert(1)"}, fixtureNow); j.URL != "" {
		t.Errorf("non-http link kept: %q", j.URL)
	}
}

func dedupeItem(src, title, link string, at time.Time) *Item {
	it, _ := Normalize(config.Source{ID: src}, clock.IST, RawItem{Title: title, Link: link}, at)
	return &it
}

func TestDeduper(t *testing.T) {
	d := NewDeduper(24*time.Hour, 0.8)
	t0 := fixtureNow

	a := dedupeItem("et", "RBI keeps repo rate unchanged at 5.5%, maintains neutral stance", "https://et.com/1", t0)
	if _, _, dup := d.Check(a); dup {
		t.Fatal("first headline is a dup")
	}
	// Another source, nearly the same words: the same story.
	b := dedupeItem("mint", "RBI keeps repo rate unchanged at 5.5%, maintains neutral policy stance", "https://mint.com/9", t0.Add(time.Minute))
	if id, src, dup := d.Check(b); !dup || id != a.ID || src != "et" {
		t.Fatalf("near dup: %v %v %v", id, src, dup)
	}
	// A copy of the copy (its URL) still points at the original.
	c := dedupeItem("bs", "Repo rate on hold: what it means for borrowers", "http://www.mint.com/9?utm_source=x", t0.Add(2*time.Minute))
	if id, src, dup := d.Check(c); !dup || id != a.ID || src != "et" {
		t.Fatalf("copy of copy: %v %v %v", id, src, dup)
	}
	// Same story, different angle: not a dup.
	e := dedupeItem("mint", "RBI keeps repo rate unchanged; economists now see a cut in December", "https://mint.com/10", t0)
	if _, _, dup := d.Check(e); dup {
		t.Fatal("different headline matched")
	}
	// Near-duplicate matching is across sources only.
	f := dedupeItem("et", "RBI keeps repo rate unchanged at 5.5 percent, maintains neutral stance", "https://et.com/2", t0)
	if _, _, dup := d.Check(f); dup {
		t.Fatal("same-source update matched as near dup")
	}
	// The same title verbatim matches across sources.
	g1 := dedupeItem("et", "Sensex, Nifty open higher as IT stocks gain", "https://et.com/3", t0)
	g2 := dedupeItem("bs", "SENSEX, NIFTY OPEN HIGHER AS IT STOCKS GAIN!", "https://bs.com/3", t0)
	d.Check(g1)
	if id, _, dup := d.Check(g2); !dup || id != g1.ID {
		t.Fatal("verbatim title across sources not matched")
	}
	// The same URL is a dup even from the same source and with another title.
	if _, _, dup := d.Check(dedupeItem("et", "Sensex and Nifty: live updates", "https://et.com/3#live", t0)); !dup {
		t.Fatal("same URL not matched")
	}
}

func TestDeduperShortTitles(t *testing.T) {
	d := NewDeduper(24*time.Hour, 0.8)
	// Generic titles with their own links are different releases.
	if _, _, dup := d.Check(dedupeItem("rbi", "Press Release", "https://rbi.org.in/?prid=1", fixtureNow)); dup {
		t.Fatal("first press release dup")
	}
	if _, _, dup := d.Check(dedupeItem("rbi", "Press Release", "https://rbi.org.in/?prid=2", fixtureNow)); dup {
		t.Fatal("second press release with its own link deduped")
	}
	if _, _, dup := d.Check(dedupeItem("sebi", "Press Release", "https://sebi.gov.in/1", fixtureNow)); dup {
		t.Fatal("short title matched across sources")
	}
	// Without a link or GUID, a short title is all there is: same source, same title.
	if _, _, dup := d.Check(dedupeItem("x", "Gold steady", "", fixtureNow)); dup {
		t.Fatal("first linkless dup")
	}
	if _, _, dup := d.Check(dedupeItem("x", "Gold steady!", "", fixtureNow)); !dup {
		t.Fatal("linkless repeat not matched")
	}
	// Fewer than minWords content words never near-match.
	if _, _, dup := d.Check(dedupeItem("y", "Gold steady", "", fixtureNow)); dup {
		t.Fatal("short title near-matched across sources")
	}
}

func TestDeduperTTL(t *testing.T) {
	d := NewDeduper(time.Hour, 0.8)
	title := "Federal Reserve issues FOMC statement on rates"
	d.Check(dedupeItem("fed", title, "https://fed.gov/a", fixtureNow))
	if _, _, dup := d.Check(dedupeItem("et", title, "https://et.com/a", fixtureNow.Add(59*time.Minute))); !dup {
		t.Fatal("within ttl not matched")
	}
	d.Prune(fixtureNow.Add(2 * time.Hour))
	if len(d.keys) != 0 || len(d.recent) != 0 {
		t.Fatalf("prune left %d keys %d titles", len(d.keys), len(d.recent))
	}
	if _, _, dup := d.Check(dedupeItem("mint", title, "https://mint.com/a", fixtureNow.Add(2*time.Hour))); dup {
		t.Fatal("matched after ttl")
	}
}

// TestIngestFixtures runs every fixture through Ingest as one live poll each.
func TestIngestFixtures(t *testing.T) {
	cfg := testFeeds()
	for _, s := range fixtureSources {
		s.Enabled, s.Kind = true, "rss"
		cfg.Sources = append(cfg.Sources, s)
	}
	cfg.Sources = append(cfg.Sources, config.Source{ID: "off", Kind: "rss"}, config.Source{ID: "paid", Kind: "paid"})
	in := NewIngest(cfg, nil)
	ctx := context.Background()

	raws, added := 0, 0
	for _, s := range fixtureSources {
		r := readFixture(t, s.ID)
		raws += len(r)
		added += in.Add(ctx, s, mustLoc(t, s.TZ), r, false, fixtureNow)
	}
	// Three are past max_age (BS 1 Oct, SEBI 15 Oct, Fed 16 Oct); two are copies
	// (Mint's RBI headline of ET's, BS's market open of ET's).
	if raws != 33 || added != 28 {
		t.Fatalf("%d entries gave %d headlines, want 33 and 28", raws, added)
	}
	f, dirty := in.Snapshot(fixtureNow)
	if !dirty || len(f.Items) != 28 || f.TS != bus.Ms(fixtureNow) {
		t.Fatalf("snapshot: dirty %v items %d", dirty, len(f.Items))
	}
	if !slices.IsSortedFunc(f.Items, func(a, b Item) int { return int(b.TS - a.TS) }) {
		t.Error("feed not newest first")
	}
	byTitle := map[string]Item{}
	for _, it := range f.Items {
		byTitle[it.Title] = it
		if strings.Contains(it.Title, "Bank Nifty") || strings.Contains(it.Title, "Consultation paper") || strings.Contains(it.Title, "annual meeting") {
			t.Errorf("stale headline kept: %q", it.Title)
		}
	}
	rbi := byTitle["RBI keeps repo rate unchanged at 5.5%, maintains neutral stance"]
	if !slices.Equal(rbi.Also, []string{"mint_markets"}) || rbi.LagMs != (7*time.Minute).Milliseconds() {
		t.Errorf("ET RBI: also %v lag %d", rbi.Also, rbi.LagMs)
	}
	if open := byTitle["Sensex, Nifty open higher as IT stocks gain"]; open.Source != "et_markets" || !slices.Equal(open.Also, []string{"bs_markets"}) {
		t.Errorf("market open: %+v", open)
	}
	if s := byTitle["Circular on revised framework for F&O eligibility of stocks"]; s.LagMs != 0 || !s.DateOnly {
		t.Errorf("date-only item has lag: %+v", s)
	}

	states := map[string]SourceStatus{}
	for _, st := range f.Sources {
		states[st.ID] = st
	}
	if states["off"].State != "disabled" || states["paid"].State != "stub" || states["et_markets"].State != "starting" {
		t.Errorf("states: %+v", states)
	}
	if et := states["et_markets"]; et.New != 5 || et.LagMed == 0 || et.LagMax < et.LagMed {
		t.Errorf("et status: %+v", et)
	}
	if mint := states["mint_markets"]; mint.New != 3 {
		t.Errorf("mint new = %d, want 3 (one was ET's story)", mint.New)
	}
	if _, polled, items := in.Health(); polled != len(fixtureSources) || items != 28 {
		t.Errorf("health: polled %d items %d", polled, items)
	}

	// The next poll repeats the same entries: nothing new, nothing changed.
	for _, s := range fixtureSources {
		if n := in.Add(ctx, s, mustLoc(t, s.TZ), readFixture(t, s.ID), false, fixtureNow.Add(time.Minute)); n != 0 {
			t.Errorf("%s: repeat poll added %d", s.ID, n)
		}
	}
	if _, dirty := in.Snapshot(fixtureNow.Add(time.Minute)); dirty {
		t.Error("repeat poll marked the feed changed")
	}
}

func TestIngestBackfillAndKeep(t *testing.T) {
	cfg := testFeeds()
	cfg.Keep = 3
	src := fixtureSources[0]
	cfg.Sources = []config.Source{src}
	in := NewIngest(cfg, nil)
	if n := in.Add(context.Background(), src, clock.IST, readFixture(t, "et_markets"), true, fixtureNow); n != 5 {
		t.Fatalf("backfill added %d", n)
	}
	f, _ := in.Snapshot(fixtureNow)
	if len(f.Items) != 3 || f.Items[0].Title != "RBI keeps repo rate unchanged at 5.5%, maintains neutral stance" {
		t.Fatalf("keep 3: %+v", f.Items)
	}
	if st := f.Sources[0]; st.New != 0 || st.LagMed != 0 || f.Items[0].LagMs != 0 {
		t.Errorf("backfill counted as new: %+v", st)
	}
}

func TestMedian(t *testing.T) {
	for _, c := range []struct {
		in   []int64
		want int64
	}{{nil, 0}, {[]int64{5}, 5}, {[]int64{9, 1, 5}, 5}, {[]int64{4, 1, 3, 2}, 2}} {
		if got := median(c.in); got != c.want {
			t.Errorf("median(%v) = %d", c.in, got)
		}
	}
}
