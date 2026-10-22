package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadRepoConfig(t *testing.T) {
	t.Setenv("PMT_DATA_DIR", "")
	t.Setenv("NATS_URL", "nats://x:4222")
	c, err := Load("../../config/pmt.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if c.NATSURL != "nats://x:4222" {
		t.Fatalf("nats url = %q", c.NATSURL)
	}
	if u, ok := c.Underlying("NIFTY"); !ok || u.WideStrikes != 50 || u.CoreStrikes != 20 {
		t.Fatalf("NIFTY = %+v", u)
	}
	if c.Ticker.StaleAfter.Seconds() != 5 {
		t.Fatalf("stale_after = %v", c.Ticker.StaleAfter)
	}
	if c.Backup.Outbox == "/backup/outbox" {
		t.Fatal("outbox not re-rooted under data dir")
	}
	if len(c.Schedule) == 0 {
		t.Fatal("no schedule")
	}
	if _, err := os.Stat(c.Abs("config/expiry_rules.csv")); err != nil {
		t.Fatalf("Abs: %v", err)
	}
	f := c.News.Feeds
	if f.Poll.Seconds() != 60 || f.Keep != 200 || f.Similar != 0.8 || f.Fails != 5 || f.Cool.Minutes() != 15 ||
		!strings.HasPrefix(f.UserAgent, "pmt-news/") || strings.Contains(f.UserAgent, "Mozilla") {
		t.Fatalf("feeds = %+v", f)
	}
	ids, paid := map[string]bool{}, 0
	for _, s := range f.Sources {
		if ids[s.ID] {
			t.Fatalf("duplicate source %s", s.ID)
		}
		ids[s.ID] = true
		if s.Kind == "paid" {
			paid++
			continue
		}
		if s.Kind != "rss" || !strings.HasPrefix(s.URL, "https://") || s.TZ == "" || s.Trust == "" {
			t.Fatalf("source %+v", s)
		}
	}
	if !ids["rbi_press"] || !ids["et_markets"] || paid != 1 {
		t.Fatalf("sources %v, paid %d", ids, paid)
	}
	k := c.News.Classify
	if k.CautionImpact != 80 || k.CautionFor != 10*time.Minute || k.Corroborate != 2 || k.Fresh != 30*time.Minute ||
		len(k.Trusted) != 2 || k.LLM.Enabled || k.LLM.KeyEnv != "ANTHROPIC_API_KEY" || k.LLM.PerHour <= 0 {
		t.Fatalf("classify = %+v", k)
	}
	if _, err := os.Stat(k.Rules); err != nil || !filepath.IsAbs(k.Rules) {
		t.Fatalf("rules %q: %v", k.Rules, err)
	}
	if k.ToneWindow != time.Hour {
		t.Fatalf("tone_window = %v", k.ToneWindow)
	}
	v := c.News.VIX
	if !v.Enabled || v.Subject != "tick.nse.indiavix._._.idx" || v.DayPct != 15 || v.JumpPct != 7 || v.Window != 15*time.Minute ||
		v.Level != 25 || v.Persist != 30*time.Second || v.HoldFor != 15*time.Minute || v.Stale != 2*time.Minute {
		t.Fatalf("vix = %+v", v)
	}
	for _, bad := range []VIX{
		{Enabled: true, Subject: "tick.nse.*", DayPct: 1, HoldFor: 1, Stale: 1},
		{Enabled: true, Subject: "nse.indiavix", DayPct: 1, HoldFor: 1, Stale: 1},
		{Enabled: true, Subject: "tick.x", HoldFor: 1, Stale: 1},
		{Enabled: true, Subject: "tick.x", JumpPct: 5, HoldFor: 1, Stale: 1},
		{Enabled: true, Subject: "tick.x", DayPct: -1, Level: 20, HoldFor: 1, Stale: 1},
		{Enabled: true, Subject: "tick.x", Level: 20, Stale: 1},
	} {
		if err := bad.check(); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if err := (VIX{}).check(); err != nil {
		t.Errorf("disabled rejected: %v", err)
	}
}
