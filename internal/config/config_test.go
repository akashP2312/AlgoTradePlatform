package config

import (
	"os"
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
}
