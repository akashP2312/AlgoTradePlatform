package config

import (
	"os"
	"strings"
	"testing"
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
}
