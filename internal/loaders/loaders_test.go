package loaders

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"pmt/internal/contract"
	"pmt/internal/kite"
	"pmt/internal/refdata"
	"pmt/internal/store"
)

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func d(y int, m time.Month, day int) time.Time { return time.Date(y, m, day, 0, 0, 0, 0, time.UTC) }

func TestBhavURL(t *testing.T) {
	base := "https://example.test/"
	if got, want := BhavURL(base, d(2026, 10, 16)), "https://example.test/content/fo/BhavCopy_NSE_FO_0_0_0_20261016_F_0000.csv.zip"; got != want {
		t.Errorf("udiff url %s", got)
	}
	if got, want := BhavURL(base, d(2024, 1, 18)), "https://example.test/content/historical/DERIVATIVES/2024/JAN/fo18JAN2024bhav.csv.zip"; got != want {
		t.Errorf("legacy url %s", got)
	}
	if got := PartOIURL(base, d(2026, 10, 6)); got != "https://example.test/content/nsccl/fao_participant_oi_06102026.csv" {
		t.Errorf("partoi url %s", got)
	}
}

func TestParseUDiFF(t *testing.T) {
	rows, err := ParseBhav(read(t, "udiff.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("%d rows, want 3 (unknown type skipped)", len(rows))
	}
	fut, ce, pe := rows[0], rows[1], rows[2]
	if got := fut.Key.String(); got != "NFO|LDTEST|2026-10-27||FUT" {
		t.Errorf("fut key %s", got)
	}
	if got := ce.Key.String(); got != "NFO|LDTEST|2026-10-27|25000|CE" {
		t.Errorf("ce key %s", got)
	}
	if pe.Key.Kind != contract.PE || *pe.Close != 70.10 {
		t.Errorf("pe %+v close %v", pe.Key, *pe.Close)
	}
	if *fut.OI != 1234500 || *fut.OIChange != -1500 || *fut.Turnover != 247500000000.50 || *fut.LotSize != 75 {
		t.Errorf("fut fields oi %d chg %d to %v lot %d", *fut.OI, *fut.OIChange, *fut.Turnover, *fut.LotSize)
	}
	if ce.VolumeUnit != "udiff" || ce.Source != "udiff" || !ce.TradeDate.Equal(d(2026, 10, 16)) || *ce.UndPrice != 25080.15 {
		t.Errorf("ce meta %+v", ce)
	}
}

func TestParseLegacy(t *testing.T) {
	rows, err := ParseBhav(read(t, "legacy.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("%d rows", len(rows))
	}
	if got := rows[0].Key.String(); got != "NFO|LDTEST|2024-01-25||FUT" {
		t.Errorf("fut key %s", got)
	}
	if got := rows[2].Key.String(); got != "NFO|LDSTK|2024-01-25|1500|PE" {
		t.Errorf("stk key %s", got)
	}
	if *rows[1].Volume != 500000 || rows[1].VolumeUnit != "contracts" || *rows[1].Turnover != 8e11 {
		t.Errorf("ce vol %d turnover %v", *rows[1].Volume, *rows[1].Turnover)
	}
	if !rows[0].TradeDate.Equal(d(2024, 1, 18)) || rows[0].Last != nil {
		t.Errorf("fut %+v", rows[0])
	}
}

func TestParseBadInput(t *testing.T) {
	if _, err := ParseBhav([]byte("a,b,c\n1,2,3\n")); err == nil {
		t.Error("unknown header accepted")
	}
	bad := bytes.Replace(read(t, "udiff.csv"), []byte("150.25,150.00"), []byte("abc,150.00"), 1)
	if _, err := ParseUDiFF(bad); err == nil {
		t.Error("bad number accepted")
	}
	noCol := bytes.Replace(read(t, "legacy.csv"), []byte("OPEN_INT"), []byte("OI"), 1)
	if _, err := ParseLegacy(noCol); err == nil {
		t.Error("missing column accepted")
	}
}

func TestUnzip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.Create("x.csv")
	w.Write([]byte("hello"))
	zw.Close()
	got, err := Unzip(buf.Bytes())
	if err != nil || string(got) != "hello" {
		t.Errorf("zip: %q %v", got, err)
	}
	if got, _ := Unzip([]byte("plain")); string(got) != "plain" {
		t.Errorf("plain: %q", got)
	}
}

func TestParsePartOI(t *testing.T) {
	rows, err := ParsePartOI(read(t, "partoi.csv"), d(2026, 10, 16))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5*7 {
		t.Fatalf("%d rows", len(rows))
	}
	find := func(ct, inst string) PartOI {
		for _, r := range rows {
			if r.ClientType == ct && r.Instrument == inst {
				return r
			}
		}
		t.Fatalf("no %s %s", ct, inst)
		return PartOI{}
	}
	if r := find("fii", "fut_idx"); r.Long != 10 || r.Short != 20 {
		t.Errorf("fii fut_idx %+v", r)
	}
	// Call long 50 / call short 70; put long 60 / put short 80.
	if r := find("fii", "opt_idx_ce"); r.Long != 50 || r.Short != 70 {
		t.Errorf("fii opt_idx_ce %+v", r)
	}
	if r := find("fii", "opt_idx_pe"); r.Long != 60 || r.Short != 80 {
		t.Errorf("fii opt_idx_pe %+v", r)
	}
	if r := find("total", "total"); r.Long != 4884 || r.Short != 5368 {
		t.Errorf("total %+v", r)
	}
	if _, err := ParsePartOI([]byte("title\nfoo,bar\n"), d(2026, 10, 16)); err == nil {
		t.Error("bad file accepted")
	}
}

func TestFetcher(t *testing.T) {
	var ua string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua = r.UserAgent()
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer srv.Close()
	f := Fetcher{UserAgent: "pmt-test/1"}
	b, err := f.Get(context.Background(), srv.URL+"/x")
	if err != nil || string(b) != "ok" || ua != "pmt-test/1" {
		t.Errorf("get %q %v ua %q", b, err, ua)
	}
	if _, err := f.Get(context.Background(), srv.URL+"/missing"); !errors.Is(err, ErrNotPublished) {
		t.Errorf("404: %v", err)
	}
}

type fakeHist map[uint32][]kite.Candle

func (f fakeHist) Historical(_ context.Context, token uint32, interval string, from, to time.Time, _ bool) ([]kite.Candle, error) {
	if interval != "day" {
		return nil, errors.New("interval " + interval)
	}
	return f[token], nil
}

func TestIndexDays(t *testing.T) {
	vix := refdata.Inst{Instrument: contract.Instrument{Token: 264969}, Contract: "NSE|INDIA VIX||0|IDX"}
	ts := time.Date(2026, 10, 16, 0, 0, 0, 0, time.FixedZone("IST", 19800))
	h := fakeHist{264969: {{TS: ts, O: 11, H: 12.5, L: 10.8, C: 11.9}}}
	bars, err := IndexDays(context.Background(), h, []refdata.Inst{vix}, d(2026, 10, 16), d(2026, 10, 16))
	if err != nil {
		t.Fatal(err)
	}
	if len(bars) != 1 || bars[0].Close != 11.9 || bars[0].TradeDate.Format(time.DateOnly) != "2026-10-16" || bars[0].Source != "kite" {
		t.Errorf("bars %+v", bars)
	}
}

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
	clean := func() {
		for _, q := range []string{
			`DELETE FROM eod_contracts WHERE underlying IN ('LDTEST','LDSTK')`,
			`DELETE FROM participant_oi WHERE trade_date = '1999-01-04'`,
			`DELETE FROM index_eod WHERE contract = 'LDTEST-IDX'`,
		} {
			if _, err := pool.Exec(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
	}
	clean()
	t.Cleanup(clean)
	return ctx, pool
}

func TestSaveDB(t *testing.T) {
	ctx, pool := testDB(t)
	rows, err := ParseUDiFF(read(t, "udiff.csv"))
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := ParseLegacy(read(t, "legacy.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := SaveEOD(ctx, pool, append(rows, legacy...)); err != nil || n != 6 {
		t.Fatalf("save: %d %v", n, err)
	}
	// A rerun replaces, not duplicates.
	*rows[1].Close = 151
	if _, err := SaveEOD(ctx, pool, rows); err != nil {
		t.Fatal(err)
	}
	var n int
	var close float64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eod_contracts WHERE underlying IN ('LDTEST','LDSTK')`).Scan(&n); err != nil || n != 6 {
		t.Errorf("count %d %v", n, err)
	}
	if err := pool.QueryRow(ctx, `SELECT close FROM eod_contracts WHERE contract = 'NFO|LDTEST|2026-10-27|25000|CE'`).Scan(&close); err != nil || close != 151 {
		t.Errorf("close %v %v", close, err)
	}
	var strike *float64
	if err := pool.QueryRow(ctx, `SELECT strike FROM eod_contracts WHERE contract = 'NFO|LDTEST|2026-10-27||FUT'`).Scan(&strike); err != nil || strike != nil {
		t.Errorf("fut strike %v %v", strike, err)
	}

	po, err := ParsePartOI(read(t, "partoi.csv"), d(1999, 1, 4))
	if err != nil {
		t.Fatal(err)
	}
	if err := SavePartOI(ctx, pool, po); err != nil {
		t.Fatal(err)
	}
	if err := SavePartOI(ctx, pool, po); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM participant_oi WHERE trade_date = '1999-01-04'`).Scan(&n); err != nil || n != 35 {
		t.Errorf("partoi count %d %v", n, err)
	}

	bar := IndexBar{TradeDate: d(2026, 10, 16), Contract: "LDTEST-IDX", Close: 11.9, Source: "kite"}
	if err := SaveIndexEOD(ctx, pool, []IndexBar{bar}); err != nil {
		t.Fatal(err)
	}
	bar.Close = 12
	if err := SaveIndexEOD(ctx, pool, []IndexBar{bar}); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT close FROM index_eod WHERE contract = 'LDTEST-IDX'`).Scan(&close); err != nil || close != 12 {
		t.Errorf("index close %v %v", close, err)
	}
}
