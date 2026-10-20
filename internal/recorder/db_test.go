package recorder

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pmt/internal/bus"
	"pmt/internal/calendar"
	"pmt/internal/clock"
	"pmt/internal/config"
	"pmt/internal/store"
)

// The recorder tests share the test database with other packages, so every row
// they write uses the RCTEST underlying (or RC-prefixed order ids) and tokens
// from 9101, and only those rows are cleaned up.
const (
	rcOpt = "NFO|RCTEST|2026-10-27|25000|CE"
	rcIdx = "NSE|RCTEST|||IDX"
)

func testDB(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("PMT_TEST_DB")
	if url == "" {
		t.Skip("PMT_TEST_DB not set")
	}
	ctx := context.Background()
	pool, err := store.Open(ctx, url, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return ctx, pool
}

// cleaner deletes rows matching each statement now and when the test ends.
func cleaner(t *testing.T, ctx context.Context, pool *pgxpool.Pool, qs ...string) {
	t.Helper()
	clean := func() {
		for _, q := range qs {
			if _, err := pool.Exec(ctx, q); err != nil {
				t.Errorf("clean %q: %v", q, err)
			}
		}
	}
	clean()
	t.Cleanup(clean)
}

func inTx(t *testing.T, ctx context.Context, pool *pgxpool.Pool, f func(pgx.Tx) error) {
	t.Helper()
	if err := pgx.BeginFunc(ctx, pool, f); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, ctx context.Context, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func TestTicksAndBarsDB(t *testing.T) {
	ctx, pool := testDB(t)
	const c = "NFO|RCTEST|2026-10-27||FUT"
	cleaner(t, ctx, pool,
		`DELETE FROM ticks WHERE contract = '`+c+`'`,
		`DELETE FROM bars_1m WHERE contract = '`+c+`'`)

	s := newTicksSink(10, nil)
	for _, tk := range []bus.Tick{
		{C: c, T: 9150, Recv: at(9, 15, 1), LTT: at(9, 15, 0), LTP: 100, Vol: 300, OI: 50, Phase: "c"},
		{C: c, T: 9150, Recv: at(9, 15, 20), LTT: at(9, 15, 19), LTP: 104, Vol: 320, Phase: "c"},
	} {
		b, _ := json.Marshal(tk)
		if err := s.add(b, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.add([]byte(`{"c":"","r":1}`), nil); err == nil {
		t.Error("tick without contract accepted")
	}
	inTx(t, ctx, pool, func(tx pgx.Tx) error {
		n, err := s.flush(ctx, tx)
		if n != 2 {
			t.Errorf("copied %d ticks", n)
		}
		return err
	})
	s.reset()
	if got := count(t, ctx, pool, `SELECT count(*) FROM ticks WHERE contract = $1`, c); got != 2 {
		t.Fatalf("%d ticks stored", got)
	}

	// A restarted recorder that missed the tail of the opening minute writes it
	// again with less volume and no OI: the
	// stored bar keeps its higher volume, high and OI.
	b2 := NewBars(nil)
	b2.Add(&bus.Tick{C: c, Recv: at(9, 15, 40), LTT: at(9, 15, 39), LTP: 101, Vol: 310, Phase: "c"})
	inTx(t, ctx, pool, func(tx pgx.Tx) error { _, err := b2.Flush(ctx, tx); return err })

	var (
		o, h, l, cl float64
		v           int64
		oi          *int64
	)
	if err := pool.QueryRow(ctx, `SELECT open, high, low, close, volume, oi FROM bars_1m
		WHERE contract = $1 AND source = 'live' AND ts = $2`, c, clock.At(day, 9, 15, 0)).Scan(&o, &h, &l, &cl, &v, &oi); err != nil {
		t.Fatal(err)
	}
	if o != 100 || h != 104 || l != 100 || cl != 101 || v != 320 || oi == nil || *oi != 50 {
		t.Errorf("bar o=%v h=%v l=%v c=%v v=%v oi=%v", o, h, l, cl, v, oi)
	}
}

func TestOrdersDB(t *testing.T) {
	ctx, pool := testDB(t)
	cleaner(t, ctx, pool,
		`DELETE FROM orders WHERE order_id LIKE 'RCTEST-%'`,
		`DELETE FROM instrument_history WHERE underlying = 'RCTEST'`)
	if _, err := pool.Exec(ctx, `INSERT INTO instrument_history (token, valid_from, contract, exchange, segment,
		tradingsymbol, name, underlying, expiry, strike, kind, lot_size, tick_size, exchange_token)
		VALUES (9101, '2026-10-01', $1, 'NFO', 'NFO-OPT', 'RCTEST26OCT25000CE', 'RCTEST', 'RCTEST', '2026-10-27', 25000, 'CE', 75, 0.05, 1)`,
		rcOpt); err != nil {
		t.Fatal(err)
	}
	s := &ordersSink{}
	for _, raw := range []string{
		`{"order_id":"RCTEST-1","status":"COMPLETE","instrument_token":9101,"quantity":75,"average_price":101.5,"order_timestamp":"2026-10-19 09:20:00"}`,
		`{"order_id":"RCTEST-2","status":"REJECTED","instrument_token":-4,"price":"","order_timestamp":null}`,
	} {
		if err := s.add([]byte(raw), nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.add([]byte(`{"status":"OPEN"}`), nil); err == nil {
		t.Error("order without id accepted")
	}
	inTx(t, ctx, pool, func(tx pgx.Tx) error { _, err := s.flush(ctx, tx); return err })

	rows, err := pool.Query(ctx, `SELECT order_id, contract, quantity, average_price, raw->>'status'
		FROM orders WHERE order_id LIKE 'RCTEST-%' ORDER BY order_id`)
	if err != nil {
		t.Fatal(err)
	}
	type got struct {
		id       string
		contract *string
		qty      *int32
		avg      *float64
		status   string
	}
	var gs []got
	for rows.Next() {
		var g got
		if err := rows.Scan(&g.id, &g.contract, &g.qty, &g.avg, &g.status); err != nil {
			t.Fatal(err)
		}
		gs = append(gs, g)
	}
	if len(gs) != 2 {
		t.Fatalf("%d orders", len(gs))
	}
	if g := gs[0]; g.contract == nil || *g.contract != rcOpt || g.qty == nil || *g.qty != 75 || g.avg == nil || *g.avg != 101.5 || g.status != "COMPLETE" {
		t.Errorf("first order %+v", g)
	}
	if g := gs[1]; g.contract != nil || g.qty != nil || g.status != "REJECTED" {
		t.Errorf("second order %+v", g)
	}
}

// TestGapsAndMergeDB records a session with one hole in the ws snapshots and one
// in the index ticks, then checks the gap rows, a backup merge that fills the
// hole, and that a second gap run replaces the rows instead of adding to them.
func TestGapsAndMergeDB(t *testing.T) {
	ctx, pool := testDB(t)
	cleaner(t, ctx, pool,
		`DELETE FROM chain_snapshots WHERE underlying = 'RCTEST'`,
		`DELETE FROM ticks WHERE contract = '`+rcIdx+`'`,
		`DELETE FROM bars_1m WHERE contract = '`+rcIdx+`'`,
		`DELETE FROM recorder_gaps WHERE underlying = 'RCTEST' AND kind <> 'harvest'`,
		`DELETE FROM instrument_history WHERE underlying = 'RCTEST' OR token IN (9201, 9202)`)
	if _, err := pool.Exec(ctx, `INSERT INTO instrument_history (token, valid_from, contract, exchange, segment,
		tradingsymbol, name, underlying, expiry, strike, kind, lot_size, tick_size, exchange_token) VALUES
		(9201, '2026-10-01', $1, 'NFO', 'NFO-OPT', 'RCTEST26OCT25000CE', 'RCTEST', 'RCTEST', '2026-10-27', 25000, 'CE', 75, 0.05, 1),
		(9202, '2026-10-01', $2, 'NSE', 'INDICES', 'RCTEST IDX', 'RCTEST', 'RCTEST', NULL, NULL, 'IDX', 1, 0.05, 2)`,
		rcOpt, rcIdx); err != nil {
		t.Fatal(err)
	}
	c, err := calendar.Load("../../config/calendar/holidays.csv")
	if err != nil {
		t.Fatal(err)
	}
	se := c.Session(day)
	chain := func(m time.Time, src string) bus.Chain {
		return bus.Chain{TS: bus.Ms(m), Source: src, Und: "RCTEST", Expiry: "2026-10-27", Spot: 25000,
			Rows: []bus.ChainRow{{C: rcOpt, T: 9201, Strike: 25000, Type: "CE", LTP: 100}}}
	}
	hole := clock.At(day, 10, 0, 0) // ws missing 10:00, 10:01, 10:02
	tickHole := clock.At(day, 11, 0, 0)
	sink := &chainSink{}
	var ticks [][]any
	for m := se.Open; !m.After(se.Close); m = m.Add(time.Minute) {
		if m.Before(hole) || !m.Before(hole.Add(3*time.Minute)) {
			ch := chain(m, "ws")
			if err := sink.addChain(&ch); err != nil {
				t.Fatal(err)
			}
		}
		if m.Before(se.Close) && !m.Equal(tickHole) {
			row, err := tickRow(&bus.Tick{C: rcIdx, T: 9202, Recv: bus.Ms(m.Add(5 * time.Second)), LTP: 25000, Phase: "c"})
			if err != nil {
				t.Fatal(err)
			}
			ticks = append(ticks, row)
		}
	}
	backup := chain(hole.Add(time.Minute), "backup")
	if err := sink.addChain(&backup); err != nil {
		t.Fatal(err)
	}
	inTx(t, ctx, pool, func(tx pgx.Tx) error {
		if _, err := sink.flush(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"ticks"}, tickCols, pgx.CopyFromRows(ticks)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO bars_1m (ts, contract, source, open, high, low, close, volume)
			VALUES ($1, $2, 'kite', 1, 1, 1, 1, 0)`, tickHole, rcIdx)
		return err
	})

	cfg := &config.Config{Universe: config.Universe{Underlyings: []config.Underlying{
		{Name: "RCTEST", Index: "NSE:RCTEST IDX", DerivExchange: "NFO", WideExpiries: 2},
	}}}
	after := clock.At(day, 16, 0, 0)
	check := func(wantSnapFilled int) {
		t.Helper()
		rep, err := Gaps(ctx, pool, cfg, c, day, after)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Series != 2 || len(rep.Gaps) != 2 || rep.Missing != 4 || rep.Open != 3-wantSnapFilled {
			t.Fatalf("report %+v", rep)
		}
		for _, g := range rep.Gaps {
			switch g.Kind {
			case "snapshot":
				if !g.Start.Equal(hole) || g.Minutes != 3 || g.Filled != wantSnapFilled || g.FilledBy != "backup" {
					t.Errorf("snapshot gap %+v", g)
				}
			case "ticks":
				if !g.Start.Equal(tickHole) || g.Minutes != 1 || g.Filled != 1 || g.FilledBy != "harvest" || g.Detail != rcIdx {
					t.Errorf("ticks gap %+v", g)
				}
			}
		}
		if n := count(t, ctx, pool, `SELECT count(*) FROM recorder_gaps WHERE underlying = 'RCTEST' AND trade_date = '2026-10-19'`); n != 2 {
			t.Errorf("%d gap rows stored", n)
		}
	}
	check(1)

	// During the session only settled minutes count: at 10:02 nothing is judged
	// past 10:00, so the snapshot hole shows one minute.
	rep, err := Gaps(ctx, pool, cfg, c, day, clock.At(day, 10, 2, 30))
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range rep.Gaps {
		if g.Kind == "snapshot" && g.Minutes != 1 {
			t.Errorf("mid-session snapshot gap %+v", g)
		}
	}

	// Merge a backup file: 10:00 is new, 09:30 already has ws, one line is junk.
	path := filepath.Join(t.TempDir(), "chains.jsonl")
	var lines []string
	for _, m := range []time.Time{hole, clock.At(day, 9, 30, 0)} {
		b, _ := json.Marshal(chain(m, "ws")) // the merge forces source backup
		lines = append(lines, string(b))
	}
	lines = append(lines, "{not json")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := MergeBackup(ctx, pool, path, false)
	if err != nil {
		t.Fatal(err)
	}
	if st.Lines != 3 || st.Bad != 1 || st.Chains != 1 || st.Skipped != 1 || st.Rows != 1 || len(st.Dates) != 1 || !st.Dates[0].Equal(day) {
		t.Errorf("merge %+v", st)
	}
	if n := count(t, ctx, pool, `SELECT count(*) FROM chain_snapshots WHERE underlying = 'RCTEST' AND source = 'backup'`); n != 2 {
		t.Errorf("%d backup rows", n)
	}
	// Rerunning is harmless; with all set the 09:30 snapshot goes in too.
	st, err = MergeBackup(ctx, pool, path, true)
	if err != nil || st.Rows != 1 || st.Skipped != 0 {
		t.Errorf("merge all %+v %v", st, err)
	}
	check(2)
}
