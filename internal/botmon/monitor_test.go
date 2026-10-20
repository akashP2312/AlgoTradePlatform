package botmon

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pmt/internal/calendar"
	"pmt/internal/clock"
	"pmt/internal/config"
)

// schema mirrors ~/algo-trading's ledger tables.
const schema = `
CREATE TABLE positions (symbol TEXT PRIMARY KEY, engine TEXT, qty INTEGER, entry_date TEXT,
	entry_price REAL, stop REAL, highest_close REAL, init_risk REAL);
CREATE TABLE trades (id INTEGER PRIMARY KEY, symbol TEXT, engine TEXT, qty INTEGER, entry_date TEXT,
	entry_price REAL, exit_date TEXT, exit_price REAL, pnl REAL, r_multiple REAL, reason TEXT, days_held INTEGER);
CREATE TABLE equity_history (date TEXT PRIMARY KEY, equity REAL, cash REAL, n_positions INTEGER);
CREATE TABLE pending_orders (id INTEGER PRIMARY KEY, created TEXT, symbol TEXT, side TEXT, qty INTEGER,
	stop REAL, engine TEXT, reason TEXT, status TEXT);
CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT);
`

func ledgerFile(t *testing.T, stmts ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ledger_paper.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range append([]string{schema}, stmts...) {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return path
}

func cal(t *testing.T) *calendar.Calendar {
	t.Helper()
	c, err := calendar.Load("../../config/calendar/holidays.csv")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func bot(mode string) config.Bot {
	return config.Bot{Name: "rotation", Dir: "/bot", Mode: mode, Poll: time.Minute, RunAt: "18:05:00", Grace: 30 * time.Minute}
}

func ist(y int, mo time.Month, d, hh, mm int) time.Time {
	return time.Date(y, mo, d, hh, mm, 0, 0, clock.IST)
}

func withAsOf(asof string) *Ledger {
	return &Ledger{Meta: map[string]string{"last_asof": asof}}
}

func TestReadLedger(t *testing.T) {
	path := ledgerFile(t,
		`INSERT INTO positions VALUES ('RADICO','rot',10,'2026-10-01',2500,2300,2700,200)`,
		`INSERT INTO trades (symbol,engine,qty,entry_date,entry_price,exit_date,exit_price,pnl,r_multiple,reason,days_held)
			VALUES ('OFSS','rot',2,'2026-08-01',9000,'2026-08-17',9500,1000,NULL,'regime',16),
			       ('LAURUSLABS','rot',5,'2026-08-01',500,'2026-08-17',480,-100,-0.5,'regime',16)`,
		`INSERT INTO equity_history VALUES ('2026-10-16',300000,275000,1),('2026-10-19',301000,275000,1)`,
		`INSERT INTO pending_orders (created,symbol,side,qty,stop,engine,reason,status)
			VALUES ('x','OFSS','BUY',1,NULL,'rot','entry','filled'),('y','POWERINDIA','BUY',3,1800,'rot','entry','approved')`,
		`INSERT INTO meta VALUES ('last_asof','2026-10-19'),('cash','275000'),('month_start_equity','295000'),
			('last_marks','{"RADICO": 4}')`,
	)
	l, err := ReadLedger(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Positions) != 1 || l.Positions[0].Qty != 10 || l.NTrades != 2 || l.Realised != 900 {
		t.Fatalf("positions %+v trades %d realised %v", l.Positions, l.NTrades, l.Realised)
	}
	if l.Trades[0].Sym != "LAURUSLABS" || l.Trades[0].R == nil || l.Trades[1].R != nil {
		t.Fatalf("trades newest first, nullable r: %+v", l.Trades)
	}
	if len(l.Pending) != 1 || l.Pending[0].Sym != "POWERINDIA" || *l.Pending[0].Stop != 1800 {
		t.Fatalf("pending: %+v", l.Pending)
	}
	if l.Curve[0].Date != "2026-10-16" || l.Curve[1].Equity != 301000 {
		t.Fatalf("curve oldest first: %+v", l.Curve)
	}

	m := New(bot("paper"), cal(t))
	m.OnLedger(l, nil, ist(2026, 10, 20, 12, 0))
	s := m.State(ist(2026, 10, 20, 12, 0))
	p := s.Positions[0]
	// mark 2600 = 2500 * 1.04; 13% above the 2300 stop; equity 275000 + 26000.
	if *p.Mark != 2600 || *p.PnL != 1000 || *p.ToStop != 13.04 || *s.Equity != 301000 || *s.MonthPct != 2.03 {
		t.Fatalf("mark %v pnl %v to_stop %v equity %v month %v", *p.Mark, *p.PnL, *p.ToStop, *s.Equity, *s.MonthPct)
	}
	if s.Recon.Status != ReconNotCompared {
		t.Fatalf("paper recon %+v", s.Recon)
	}

	// Never opens it for writing: the file is unchanged after a read.
	fi, _ := os.Stat(path)
	if _, err := ReadLedger(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	fj, _ := os.Stat(path)
	if !fi.ModTime().Equal(fj.ModTime()) || fi.Size() != fj.Size() {
		t.Fatal("the read changed the ledger")
	}
}

func TestReadLedgerSkipsWhileTheBotWrites(t *testing.T) {
	path := ledgerFile(t)
	if err := os.WriteFile(path+"-journal", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLedger(context.Background(), path); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if _, err := ReadLedger(context.Background(), filepath.Join(t.TempDir(), "none.db")); err == nil || errors.Is(err, ErrBusy) {
		t.Fatalf("a missing ledger should be a plain error, got %v", err)
	}
}

func TestExpectedRespectsRunTimeWeekendsAndHolidays(t *testing.T) {
	m := New(bot("paper"), cal(t))
	for _, c := range []struct {
		now  time.Time
		want string
	}{
		{ist(2026, 10, 19, 18, 30), "2026-10-16"}, // Monday inside the grace window: Friday's run
		{ist(2026, 10, 19, 18, 36), "2026-10-19"}, // past 18:35: Monday's run is due
		{ist(2026, 10, 20, 21, 0), "2026-10-19"},  // Tuesday is Dussehra: no run due
		{ist(2026, 10, 18, 12, 0), "2026-10-16"},  // Sunday
		{ist(2026, 10, 21, 9, 0), "2026-10-19"},   // Wednesday morning, before its run
	} {
		if got := m.Expected(c.now).Format("2006-01-02"); got != c.want {
			t.Errorf("Expected(%s) = %s, want %s", c.now.Format("Mon 01-02 15:04"), got, c.want)
		}
	}
}

func TestHeartbeat(t *testing.T) {
	m := New(bot("paper"), cal(t))
	step := func(asof string, now time.Time) []Change {
		m.OnLedger(withAsOf(asof), nil, now)
		return m.Eval(now)
	}
	// Up to date on the holiday: Monday's run is the latest due.
	if c := step("2026-10-19", ist(2026, 10, 20, 21, 0)); len(c) != 0 {
		t.Fatalf("holiday: %+v", c)
	}
	// Wednesday after 18:35 without a run: one behind.
	if c := step("2026-10-19", ist(2026, 10, 21, 18, 40)); len(c) != 1 || c[0].To != Warn || notifyLevel(c[0]) != "warn" {
		t.Fatalf("one missed: %+v", c)
	}
	// Thursday too: crit.
	if c := step("2026-10-19", ist(2026, 10, 22, 18, 40)); len(c) != 1 || c[0].To != Crit {
		t.Fatalf("two missed: %+v", c)
	}
	if n, _ := m.Behind(ist(2026, 10, 22, 18, 40)); n != 2 {
		t.Fatalf("behind %d, want 2 (Wed and Thu; Tue was a holiday)", n)
	}
	// It runs: clears.
	if c := step("2026-10-22", ist(2026, 10, 22, 18, 41)); len(c) != 1 || c[0].To != OK || notifyLevel(c[0]) != "info" {
		t.Fatalf("clear: %+v", c)
	}
	// A ledger with no last_asof has never run.
	if c := step("", ist(2026, 10, 22, 18, 42)); len(c) != 1 || c[0].To != Crit {
		t.Fatalf("never ran: %+v", c)
	}
}

func TestLedgerFailuresAlertAfterThree(t *testing.T) {
	m := New(bot("paper"), cal(t))
	now := ist(2026, 10, 20, 12, 0)
	m.OnLedger(nil, ErrBusy, now) // busy is not a failure
	for i := 0; i < 2; i++ {
		m.OnLedger(nil, errors.New("no such file"), now)
	}
	if c := m.Eval(now); len(c) != 0 || m.fails != 2 {
		t.Fatalf("two failures: %+v", c)
	}
	m.OnLedger(nil, errors.New("no such file"), now)
	if c := m.Eval(now); len(c) != 1 || c[0].Rule != RuleLedger || c[0].To != Warn {
		t.Fatalf("third failure: %+v", c)
	}
	m.OnLedger(withAsOf("2026-10-19"), nil, now)
	if c := m.Eval(now); len(c) != 1 || c[0].To != OK {
		t.Fatalf("recovery: %+v", c)
	}
}

func TestReconLive(t *testing.T) {
	m := New(bot("live"), cal(t))
	now := ist(2026, 10, 19, 19, 0)
	l := withAsOf("2026-10-19")
	l.Positions = []LPos{{Sym: "RADICO", Qty: 10}, {Sym: "OFSS", Qty: 2}, {Sym: "POWERINDIA", Qty: 3}}
	l.Pending = []LOrder{{Sym: "POWERINDIA", Side: "SELL", Qty: 3, Status: "proposed"}}
	m.OnLedger(l, nil, now)

	if rc := m.Recon(); rc.Status != ReconWaiting {
		t.Fatalf("before a broker read: %+v", rc)
	}
	// OFSS short at the broker, POWERINDIA differs but has an order open, the user's
	// own INFY and an extra RADICO share are not the bot's business.
	m.OnBroker(map[string]int64{"RADICO": 11, "OFSS": 1, "INFY": 50}, nil, now)
	rc := m.Recon()
	if rc.Status != ReconMismatch || len(rc.Items) != 3 {
		t.Fatalf("recon %+v", rc)
	}
	notes := map[string]string{}
	for _, it := range rc.Items {
		notes[it.Sym] = it.Note
	}
	if notes["OFSS"] != "mismatch" || notes["POWERINDIA"] != "pending" || notes["RADICO"] != "extra" {
		t.Fatalf("notes %v", notes)
	}
	var got []Change
	for _, c := range m.Eval(now) {
		if c.Rule == RuleRecon {
			got = append(got, c)
		}
	}
	if len(got) != 1 || got[0].To != Warn {
		t.Fatalf("recon alert: %+v", got)
	}
	m.OnBroker(map[string]int64{"RADICO": 10, "OFSS": 2}, nil, now)
	if rc := m.Recon(); rc.Status != ReconOK {
		t.Fatalf("matched: %+v", rc)
	}
	if c := m.Eval(now); len(c) != 1 || c[0].Rule != RuleRecon || c[0].To != OK {
		t.Fatalf("clear: %+v", c)
	}
	// Broker failures keep the last good read, and alert after three.
	for i := 0; i < 3; i++ {
		m.OnBroker(nil, errors.New("503"), now)
	}
	if c := m.Eval(now); len(c) != 1 || c[0].Rule != RuleRecon || c[0].To != Warn {
		t.Fatalf("broker failing: %+v", c)
	}
}

func TestPaperNeverRaisesRecon(t *testing.T) {
	m := New(bot("paper"), cal(t))
	now := ist(2026, 10, 19, 19, 0)
	l := withAsOf("2026-10-19")
	l.Positions = []LPos{{Sym: "RADICO", Qty: 10}}
	m.OnLedger(l, nil, now)
	m.OnBroker(map[string]int64{}, nil, now)
	if c := m.Eval(now); len(c) != 0 || m.Recon().Status != ReconNotCompared {
		t.Fatalf("paper: %+v %+v", c, m.Recon())
	}
}
