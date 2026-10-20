package ticker

import (
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	now := time.Date(2026, 10, 21, 11, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	th := Thresholds{Stale: 5 * time.Second, Halt: 30 * time.Second}
	live := Exchange{Last: ago(time.Second), AllUp: true}

	cases := []struct {
		name  string
		phase string
		b     Beat
		ex    Exchange
		want  string
	}{
		{"after close ignores silence", "a", Beat{}, Exchange{}, FeedClosed},
		{"other exchange", "x", Beat{}, Exchange{}, FeedClosed},
		{"pre-open", "p", Beat{SocketUp: true}, live, FeedPre},
		{"fresh index", "c", Beat{Index: ago(time.Second), SocketUp: true}, live, FeedOK},
		{"index quiet, future fresh", "c", Beat{Index: ago(time.Minute), Fut: ago(2 * time.Second), SocketUp: true}, live, FeedOK},
		{"edge is fresh", "c", Beat{Index: ago(5 * time.Second), SocketUp: true}, live, FeedOK},
		{"socket down beats everything", "c", Beat{Index: ago(time.Second)}, live, FeedDown},
		{"heartbeat silent, exchange ticking", "c", Beat{Index: ago(10 * time.Second), Fut: ago(10 * time.Second), SocketUp: true}, live, FeedStale},
		{"never seen, exchange ticking", "c", Beat{SocketUp: true}, live, FeedStale},
		{"everything silent past halt_after", "c", Beat{Index: ago(40 * time.Second), SocketUp: true},
			Exchange{Last: ago(31 * time.Second), AllUp: true}, FeedHalt},
		{"everything silent, not yet halt_after", "c", Beat{Index: ago(20 * time.Second), SocketUp: true},
			Exchange{Last: ago(20 * time.Second), AllUp: true}, FeedStale},
		{"no tick all day, sockets up", "c", Beat{SocketUp: true}, Exchange{AllUp: true}, FeedHalt},
		{"another socket down is not a halt", "c", Beat{Index: ago(40 * time.Second), SocketUp: true},
			Exchange{Last: ago(40 * time.Second), AllUp: false}, FeedStale},
	}
	for _, c := range cases {
		if got := Classify(now, c.phase, c.b, c.ex, th); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}

func TestWorst(t *testing.T) {
	if got := worst(nil); got != FeedOK {
		t.Errorf("empty: %s", got)
	}
	m := map[string]UndFeed{"NIFTY": {State: FeedOK}, "BANKNIFTY": {State: FeedStale}, "FINNIFTY": {State: FeedOK}}
	if got := worst(m); got != FeedStale {
		t.Errorf("got %s", got)
	}
	m["X"] = UndFeed{State: FeedDown}
	if got := worst(m); got != FeedDown {
		t.Errorf("got %s", got)
	}
	if !Worse(FeedHalt, FeedStale) || Worse(FeedClosed, FeedOK) || Worse(FeedOK, FeedOK) {
		t.Error("ranking")
	}
}

func TestNearFut(t *testing.T) {
	d := func(m, day int) time.Time { return time.Date(2026, time.Month(m), day, 0, 0, 0, 0, time.UTC) }
	u := &Universe{Chains: []ChainSpec{
		{Und: "NIFTY", Expiry: d(11, 24), Fut: 30},
		{Und: "NIFTY", Expiry: d(10, 27), Fut: 20},
		{Und: "NIFTY", Expiry: d(10, 21), Fut: 0},
		{Und: "BANKNIFTY", Expiry: d(10, 27), Fut: 40},
	}}
	if got := nearFut(u, "NIFTY"); got != 20 {
		t.Errorf("NIFTY: %d", got)
	}
	if got := nearFut(u, "FINNIFTY"); got != 0 {
		t.Errorf("FINNIFTY: %d", got)
	}
}
