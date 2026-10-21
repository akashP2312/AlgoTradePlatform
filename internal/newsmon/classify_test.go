package newsmon

import (
	"context"
	"strings"
	"testing"
	"time"

	"pmt/internal/bus"
	"pmt/internal/config"
)

func repoRules(t *testing.T) *Rules {
	t.Helper()
	rs, err := LoadRules("../../config/news/rules.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return rs
}

// Synthetic headlines in the wording the feeds use, each with the rule expected
// to score it highest and the band its impact must fall in.
var classifyFixtures = []struct {
	src, trust, title string
	rule              string // highest rule; "" none
	lo, hi            int
}{
	// central banks
	{"rbi_press", "official", "RBI cuts repo rate by 25 basis points to 5.25%", "rbi_rate_action", 90, 90},
	{"et_markets", "media", "RBI keeps repo rate unchanged at 5.5%, maintains neutral stance", "rbi_rate_action", 90, 90},
	{"et_markets", "media", "RBI policy: What to expect from MPC meeting on Friday", "", 0, 0},
	{"mint_markets", "media", "RBI may cut repo rate by 25 bps, say economists", "rbi_rate_action", 60, 60},
	{"rbi_press", "official", "Monetary Policy Statement, 2026-27 Resolution of the Monetary Policy Committee (MPC) October 1-3, 2026", "rbi_policy_statement", 85, 85},
	{"et_markets", "media", "RBI announces unscheduled policy review, hikes repo rate by 50 bps", "rbi_emergency", 95, 95},
	{"bs_markets", "media", "RBI’s emergency move: repo rate hiked 50 bps", "rbi_emergency", 95, 95},
	{"fed_press", "official", "Federal Reserve issues FOMC statement", "fomc_statement", 85, 85},
	{"et_markets", "media", "Fed cuts interest rates by a quarter point", "fed_rate_action", 90, 90},
	{"et_markets", "media", "Fed holds rates steady, signals patience", "fed_rate_action", 90, 90},
	{"et_markets", "media", "Fed minutes show officials divided on rate cuts", "", 0, 0},
	{"mint_markets", "media", "Fed's Powell could signal pause in rate hikes", "", 0, 0},
	// market structure
	{"nse_ann", "exchange", "NSE: Trading halted in all segments due to technical glitch", "market_halt", 95, 95},
	{"et_markets", "media", "Zerodha Kite down for many users amid technical glitch", "exchange_outage", 90, 90},
	{"et_markets", "media", "Reliance stock hits lower circuit after results", "", 0, 0},
	{"mint_markets", "media", "SEBI tightens F&O rules: lot size raised, weekly expiries curbed", "sebi_fo_curbs", 80, 80},
	{"sebi", "official", "Recovery Certificate against Mr. A in the matter of B Ltd", "sebi_routine", 5, 5},
	{"et_markets", "media", "Muhurat trading 2026: date and timings", "special_session", 20, 20},
	// policy and tax
	{"et_markets", "media", "Budget 2027: STT on F&O hiked to 0.1%", "tax_change", 80, 80},
	{"bs_markets", "media", "Government raises capital gains tax on equities", "tax_change", 80, 80},
	{"et_markets", "media", "Lok Sabha election results 2029: counting begins", "election_result", 70, 70},
	// geopolitics
	{"et_markets", "media", "India launches air strikes on terror camps across border", "war_escalation", 85, 85},
	{"mint_markets", "media", "Trade war fears weigh on Asian markets", "", 0, 0},
	{"et_markets", "media", "Red Sea tensions push up shipping costs", "geopolitical_tension", 55, 55},
	// market moves
	{"et_markets", "media", "Sensex crashes 2,400 points; Nifty below 22,000 as global rout deepens", "index_big_drop", 90, 90},
	{"mint_markets", "media", "Nifty falls 3.2% in biggest single-day drop since 2020", "index_big_drop", 90, 90},
	{"et_markets", "media", "Nifty falls 0.5% as IT stocks drag", "index_move", 30, 30},
	{"et_markets", "media", "Sensex tanks 600 points from day's high", "index_move", 30, 30},
	{"et_markets", "media", "Is a market crash coming? Experts weigh in", "index_crash_word", 45, 45},
	{"et_markets", "media", "India VIX jumps 18% to highest level in a year", "vix_spike", 70, 70},
	{"et_markets", "media", "Rupee hits record low of 89.5 against dollar", "rupee_record_low", 70, 70},
	{"atom", "media", "Wall Street plunges as tech selloff deepens", "global_selloff", 60, 60},
	{"et_markets", "media", "Brent crude surges to 7-month high", "crude_shock", 50, 50},
	{"atom", "media", "Bank run fears at regional US lender", "banking_stress", 45, 45},
	// routine and noise
	{"et_markets", "media", "Infosys Q2 results: net profit rises 8%", "earnings", 20, 20},
	{"et_markets", "media", "Stocks to buy today: 5 picks from experts", "stock_tips", 0, 0},
	{"mint_markets", "media", "XYZ IPO GMP today: listing gain expected", "ipo", 10, 10},
	{"rbi_press", "official", "Money Market Operations as on October 20, 2026", "rbi_routine", 5, 5},
}

func TestClassifyFixtures(t *testing.T) {
	rs := repoRules(t)
	for _, f := range classifyFixtures {
		it := Item{Source: f.src, Trust: f.trust, Title: f.title}
		sc := rs.Classify(&it)
		top := ""
		if len(sc.Rules) > 0 {
			top = sc.Rules[0]
		}
		if top != f.rule || sc.Impact < f.lo || sc.Impact > f.hi {
			t.Errorf("%q: rule %q impact %d (rules %v), want %q in %d..%d", f.title, top, sc.Impact, sc.Rules, f.rule, f.lo, f.hi)
		}
	}
	if len(classifyFixtures) < 30 {
		t.Fatalf("only %d fixtures", len(classifyFixtures))
	}
}

func TestClassifyScope(t *testing.T) {
	rs := repoRules(t)
	// A source-limited rule does not fire for other feeds.
	it := Item{Source: "et_markets", Trust: "media", Title: "Monetary Policy Statement, 2026-27"}
	if sc := rs.Classify(&it); sc.Impact != 0 {
		t.Fatalf("policy statement from media = %+v", sc)
	}
	// title_summary rules see the summary; title rules do not.
	rs2, err := ParseRules([]byte(`
rules:
  - {id: a, impact: 50, any: [blackout], field: title_summary}
  - {id: b, impact: 60, any: [blackout]}
`))
	if err != nil {
		t.Fatal(err)
	}
	it = Item{Title: "Grid news", Summary: "A city-wide blackout hit trading desks"}
	if sc := rs2.Classify(&it); sc.Impact != 50 || len(sc.Rules) != 1 || sc.Rules[0] != "a" {
		t.Fatalf("title_summary = %+v", sc)
	}
	// Whole words: "may" does not damp "Mayank".
	rs3, err := ParseRules([]byte("damp: {by: 30, any: [may]}\nrules:\n  - {id: x, impact: 90, any: [halt]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if sc := rs3.Classify(&Item{Title: "Mayank calls a halt"}); sc.Impact != 90 {
		t.Fatalf("word boundary: %+v", sc)
	}
	if sc := rs3.Classify(&Item{Title: "Exchange may  halt"}); sc.Impact != 60 {
		t.Fatalf("damp: %+v", sc)
	}
	var nilRules *Rules
	if sc := nilRules.Classify(&Item{Title: "x"}); sc.Impact != 0 {
		t.Fatal("nil rules scored")
	}
}

func TestParseRulesRejects(t *testing.T) {
	for name, y := range map[string]string{
		"unknown field": "rules:\n  - {id: a, impact: 1, any: [x], wat: 1}\n",
		"no id":         "rules:\n  - {impact: 1, any: [x]}\n",
		"duplicate id":  "rules:\n  - {id: a, impact: 1, any: [x]}\n  - {id: a, impact: 2, any: [y]}\n",
		"impact range":  "rules:\n  - {id: a, impact: 120, any: [x]}\n",
		"no phrases":    "rules:\n  - {id: a, impact: 1}\n",
		"bad field":     "rules:\n  - {id: a, impact: 1, any: [x], field: body}\n",
		"bad trust":     "rules:\n  - {id: a, impact: 1, any: [x], trust: [blog]}\n",
		"bad regex":     "rules:\n  - {id: a, impact: 1, any: [\"re:(\"]}\n",
		"empty phrase":  "rules:\n  - {id: a, impact: 1, any: [\" \"]}\n",
		"damp range":    "damp: {by: -1}\nrules: []\n",
	} {
		if _, err := ParseRules([]byte(y)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func testClass() config.Classify {
	return config.Classify{CautionImpact: 80, CautionFor: 10 * time.Minute, Trusted: []string{"official", "exchange"}, Corroborate: 2, Fresh: 30 * time.Minute}
}

func classIngest(t *testing.T) *Ingest {
	t.Helper()
	in := NewIngest(testFeeds(), nil)
	in.Class = testClass()
	in.SetRules(repoRules(t), nil)
	return in
}

func src(id, trust string) config.Source {
	return config.Source{ID: id, Trust: trust, Region: "IN", TZ: "Asia/Kolkata"}
}

func raw(title, link string, pub time.Time) RawItem {
	r := RawItem{Title: title, Link: link}
	if !pub.IsZero() {
		r.Pub = pub.Format(time.RFC1123Z)
	}
	return r
}

func TestHoldTrustedSource(t *testing.T) {
	ctx := context.Background()
	in := classIngest(t)
	loc := mustLoc(t, "Asia/Kolkata")
	t0 := fixtureNow
	in.Add(ctx, src("rbi_press", "official"), loc, []RawItem{
		raw("RBI cuts repo rate by 25 basis points to 5.25%", "https://rbi.org.in/a", t0.Add(-2*time.Minute)),
		raw("India VIX jumps 18% to highest level in a year", "https://rbi.org.in/b", t0.Add(-time.Minute)),
	}, false, t0)

	hs := in.Holds(t0.Add(time.Minute))
	if len(hs) != 1 {
		t.Fatalf("holds = %+v", hs)
	}
	h := hs[0]
	if h.Level != Caution || !h.From.Equal(t0) || !h.Until.Equal(t0.Add(10*time.Minute)) ||
		h.Reason.Kind != "headline" || h.Reason.Source != "rbi_press" || h.Reason.Score != 90 ||
		!strings.Contains(h.Reason.Msg, "rbi_rate_action") {
		t.Fatalf("hold = %+v", h)
	}

	st := Assess(t0.Add(time.Minute), nil, testWin, hs...)
	if st.State != Caution || len(st.Reasons) != 1 || st.ExpiresAt != bus.Ms(t0.Add(10*time.Minute)) {
		t.Fatalf("assess = %+v", st)
	}
	if hs := in.Holds(t0.Add(10 * time.Minute)); len(hs) != 0 {
		t.Fatalf("hold outlived caution_for: %+v", hs)
	}
	if st := Assess(t0.Add(10*time.Minute), nil, testWin, in.Holds(t0.Add(10*time.Minute))...); st.State != Normal {
		t.Fatalf("after expiry = %+v", st)
	}
}

func TestHoldNeedsCorroboration(t *testing.T) {
	ctx := context.Background()
	loc := mustLoc(t, "Asia/Kolkata")
	t0 := fixtureNow
	title := "Zerodha Kite down for many users amid technical glitch"

	// Media alone does not hold; a second media source does, from when it is seen.
	in := classIngest(t)
	in.Add(ctx, src("et_markets", "media"), loc, []RawItem{raw(title, "https://et.example/a", t0.Add(-time.Minute))}, false, t0)
	if hs := in.Holds(t0.Add(time.Second)); len(hs) != 0 {
		t.Fatalf("single media source held: %+v", hs)
	}
	t1 := t0.Add(3 * time.Minute)
	in.Add(ctx, src("et_markets", "media"), loc, []RawItem{raw(title, "https://et.example/a", t0.Add(-time.Minute))}, false, t1)
	if hs := in.Holds(t1); len(hs) != 0 {
		t.Fatalf("same source twice held: %+v", hs)
	}
	in.Add(ctx, src("mint_markets", "media"), loc, []RawItem{raw(title, "https://mint.example/b", t0)}, false, t1)
	hs := in.Holds(t1.Add(time.Second))
	if len(hs) != 1 || !hs[0].From.Equal(t1) || !strings.Contains(hs[0].Reason.Msg, "et_markets +1") {
		t.Fatalf("corroborated = %+v", hs)
	}

	// A trusted source confirms a media story by itself.
	in = classIngest(t)
	in.Add(ctx, src("et_markets", "media"), loc, []RawItem{raw(title, "https://et.example/a", t0.Add(-time.Minute))}, false, t0)
	in.Add(ctx, src("nse_ann", "exchange"), loc, []RawItem{raw(title, "https://nse.example/c", t0)}, false, t1)
	if hs := in.Holds(t1); len(hs) != 1 {
		t.Fatalf("trusted corroboration = %+v", hs)
	}

	// corroborate: 1 lets media hold alone.
	in = classIngest(t)
	in.Class.Corroborate = 1
	in.Add(ctx, src("et_markets", "media"), loc, []RawItem{raw(title, "https://et.example/a", t0.Add(-time.Minute))}, false, t0)
	if hs := in.Holds(t0); len(hs) != 1 {
		t.Fatalf("corroborate 1 = %+v", hs)
	}
}

func TestHoldFreshness(t *testing.T) {
	ctx := context.Background()
	loc := mustLoc(t, "Asia/Kolkata")
	t0 := fixtureNow
	s := src("rbi_press", "official")
	for _, c := range []struct {
		name     string
		pub      time.Time
		backfill bool
		want     int
	}{
		{"fresh", t0.Add(-5 * time.Minute), false, 1},
		{"stale", t0.Add(-2 * time.Hour), false, 0},
		{"fresh backfill", t0.Add(-5 * time.Minute), true, 1},
		{"stale backfill", t0.Add(-40 * time.Minute), true, 0},
		{"no time, live", time.Time{}, false, 1},
		{"no time, backfill", time.Time{}, true, 0},
	} {
		in := classIngest(t)
		in.Add(ctx, s, loc, []RawItem{raw("RBI cuts repo rate by 25 basis points", "https://rbi.org.in/x", c.pub)}, c.backfill, t0)
		if hs := in.Holds(t0.Add(time.Second)); len(hs) != c.want {
			t.Errorf("%s: %d holds, want %d", c.name, len(hs), c.want)
		}
	}

	// A day-only date counts as no time.
	in := classIngest(t)
	r := RawItem{Title: "RBI cuts repo rate by 25 basis points", Link: "https://rbi.org.in/x", Pub: "21 Oct 2026"}
	in.Add(ctx, s, loc, []RawItem{r}, true, t0)
	if hs := in.Holds(t0.Add(time.Second)); len(hs) != 0 {
		t.Fatalf("day-only backfill held: %+v", hs)
	}
}

func TestHoldRulesAndLLM(t *testing.T) {
	ctx := context.Background()
	loc := mustLoc(t, "Asia/Kolkata")
	t0 := fixtureNow
	in := classIngest(t)
	in.Add(ctx, src("rbi_press", "official"), loc, []RawItem{raw("India VIX jumps 18% to highest level in a year", "https://rbi.org.in/v", t0)}, false, t0)
	if hs := in.Holds(t0); len(hs) != 0 {
		t.Fatalf("impact 70 held: %+v", hs)
	}

	// The model can raise a headline over the line, and the reason says so.
	id := in.items[0].ID
	in.SetLLM(ctx, id, LLMScore{Impact: 85, Why: "vol shock", Model: "m"})
	hs := in.Holds(t0.Add(time.Second))
	if len(hs) != 1 || hs[0].Reason.Score != 85 || !strings.Contains(hs[0].Reason.Msg, "llm: vol shock") {
		t.Fatalf("llm raise = %+v", hs)
	}
	// It cannot lower the rules' score.
	in.SetLLM(ctx, id, LLMScore{Impact: 10, Model: "m"})
	if got := in.items[0].Score(); got != 70 {
		t.Fatalf("score after lower = %d", got)
	}

	// A bad rules edit keeps the old rules and says why.
	in.SetRules(nil, errFake("rules: bad"))
	f, _ := in.Snapshot(t0)
	if f.RulesErr != "rules: bad" || f.Rules == 0 || in.items[0].Impact != 70 {
		t.Fatalf("bad edit: err %q rules %d impact %d", f.RulesErr, f.Rules, in.items[0].Impact)
	}
	// New rules rescore what is held.
	rs, err := ParseRules([]byte("rules:\n  - {id: vix, impact: 88, all: [vix]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	in.SetRules(rs, nil)
	if f, _ := in.Snapshot(t0); f.RulesErr != "" || f.Rules != 1 || in.items[0].Impact != 88 {
		t.Fatalf("rescore: %+v %d", f.RulesErr, in.items[0].Impact)
	}

	// Without caution_impact nothing holds.
	in.Class.CautionImpact = 0
	if hs := in.Holds(t0); hs != nil {
		t.Fatalf("disabled = %+v", hs)
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }
