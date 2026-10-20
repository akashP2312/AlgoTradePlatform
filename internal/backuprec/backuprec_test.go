package backuprec

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	kiteconnect "github.com/zerodha/gokiteconnect/v4"

	"pmt/internal/bus"
	"pmt/internal/calendar"
	"pmt/internal/clock"
	"pmt/internal/config"
	"pmt/internal/contract"
	"pmt/internal/kite"
	"pmt/internal/refdata"
)

var day = time.Date(2026, 10, 19, 0, 0, 0, 0, clock.IST)

func ist(d time.Time, hh, mm, ss int) time.Time { return clock.At(d, hh, mm, ss) }

// insts lists the NIFTY index, the October future and the 20 Oct weekly from
// 24800 to 25200 by 50.
func insts() []refdata.Inst {
	exp := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	rows := []contract.Instrument{
		{Token: 256265, TradingSymbol: "NIFTY 50", Segment: "INDICES", Exchange: "NSE", InstrumentType: "EQ"},
		{Token: 1000, TradingSymbol: "NIFTY26OctFUT", Name: "NIFTY", Expiry: time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC),
			InstrumentType: "FUT", Segment: "NFO-FUT", Exchange: "NFO"},
	}
	tok := uint32(1001)
	for k := 24800.0; k <= 25200; k += 50 {
		for _, typ := range []string{"CE", "PE"} {
			rows = append(rows, contract.Instrument{Token: tok, TradingSymbol: fmt.Sprintf("NIFTY26O20%.0f%s", k, typ),
				Name: "NIFTY", Expiry: exp, Strike: k, InstrumentType: typ, Segment: "NFO-OPT", Exchange: "NFO"})
			tok++
		}
	}
	return refdata.Resolve(rows)
}

type fakeClient struct {
	quoteErr error // returned by every Quotes call while set
	ltpErr   error
	quotes   int
	ltp      int
	fetches  int
}

func (f *fakeClient) Quotes(_ context.Context, keys []string) (map[string]kite.Quote, error) {
	f.quotes++
	if f.quoteErr != nil {
		return nil, f.quoteErr
	}
	at := ist(day, 10, 0, 18)
	out := map[string]kite.Quote{}
	for _, k := range keys {
		out[k] = kite.Quote{LTP: 100, Bid: 99.5, Ask: 100.5, BidQ: 75, AskQ: 150, OI: 1000, Vol: 50, Timestamp: at, LTT: at}
	}
	return out, nil
}

func (f *fakeClient) LTP(_ context.Context, keys []string) (map[string]float64, error) {
	f.ltp++
	if f.ltpErr != nil {
		return nil, f.ltpErr
	}
	out := map[string]float64{}
	for _, k := range keys {
		out[k] = 25012.35
	}
	return out, nil
}

func (f *fakeClient) FetchInstruments(context.Context) ([]byte, error) {
	f.fetches++
	return nil, errors.New("not used: instruments are preset")
}

func loadCal(t *testing.T) *calendar.Calendar {
	t.Helper()
	c, err := calendar.Load("../../config/calendar/holidays.csv")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// service builds a Service writing under a temp dir, with a token file holding a
// fake session that expires tomorrow.
func service(t *testing.T, fc *fakeClient) (*Service, *int) {
	t.Helper()
	dir := t.TempDir()
	c := &config.Config{
		Dir:       dir,
		BackupRec: config.BackupRec{OutDir: filepath.Join(dir, "out"), PollSecond: 20, TokenFile: "kite-token.json"},
		Universe: config.Universe{Underlyings: []config.Underlying{
			{Name: "NIFTY", Index: "NSE:NIFTY 50", DerivExchange: "NFO", CoreExpiries: 1, CoreStrikes: 2, Backup: true},
		}},
	}
	writeToken(t, c, "fake-token-1")
	dials := 0
	s := &Service{
		Cfg:  c,
		Cal:  loadCal(t),
		Dial: func(bus.Session) Client { dials++; return fc },
		Out:  Writer{Dir: c.BackupRec.OutDir},
	}
	s.insts, s.instDay = insts(), clock.Date(day)
	return s, &dials
}

func writeToken(t *testing.T, c *config.Config, tok string) {
	t.Helper()
	now := clock.Now()
	if err := kite.WriteTokenFile(c.Abs(c.BackupRec.TokenFile), bus.Session{
		AccessToken: tok, APIKey: "fake-key", IssuedAt: now, ExpiresAt: now.Add(24 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
}

func readChains(t *testing.T, path string) []bus.Chain {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []bus.Chain
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var c bus.Chain
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatalf("line %d: %v", len(out)+1, err)
		}
		out = append(out, c)
	}
	return out
}

func TestWriterAppends(t *testing.T) {
	dir := t.TempDir()
	w := Writer{Dir: dir}
	if err := w.Write(day, nil); err != nil {
		t.Fatal(err)
	}
	p := Path(dir, ist(day, 14, 0, 0))
	if want := filepath.Join(dir, "2026-10-19", "chains.jsonl"); p != want {
		t.Errorf("path %s, want %s", p, want)
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Error("an empty write created the file")
	}
	for i := range 2 {
		c := bus.Chain{TS: bus.Ms(ist(day, 10, i, 0)), Source: "backup", Und: "NIFTY", Expiry: "2026-10-20"}
		if err := w.Write(day, []bus.Chain{c}); err != nil {
			t.Fatal(err)
		}
	}
	cs := readChains(t, p)
	if len(cs) != 2 || cs[1].TS != bus.Ms(ist(day, 10, 1, 0)) {
		t.Errorf("read back %+v", cs)
	}
}

func TestNext(t *testing.T) {
	s, _ := service(t, &fakeClient{})
	oct21 := day.AddDate(0, 0, 2) // 20 Oct is Dussehra
	for _, tc := range []struct {
		now, want time.Time
	}{
		{ist(day, 7, 0, 0), ist(day, 9, 15, 20)},
		{ist(day, 9, 15, 19), ist(day, 9, 15, 20)},
		{ist(day, 9, 15, 20), ist(day, 9, 16, 20)},
		{ist(day, 12, 34, 5), ist(day, 12, 34, 20)},
		{ist(day, 15, 29, 30), ist(day, 15, 30, 20)}, // the close minute is polled
		{ist(day, 15, 30, 20), ist(oct21, 9, 15, 20)},
		{ist(day, 18, 0, 0), ist(oct21, 9, 15, 20)},
	} {
		got, ok := s.next(tc.now)
		if !ok || !got.Equal(tc.want) {
			t.Errorf("next(%s) = %s, want %s", tc.now.Format("01-02 15:04:05"), got.Format("01-02 15:04:05"), tc.want.Format("01-02 15:04:05"))
		}
	}
}

func TestPollWritesCoreWindow(t *testing.T) {
	fc := &fakeClient{}
	s, dials := service(t, fc)
	minute := ist(day, 10, 0, 0)
	if err := s.Poll(context.Background(), minute); err != nil {
		t.Fatal(err)
	}
	if err := s.Poll(context.Background(), minute.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if *dials != 1 || fc.fetches != 0 {
		t.Errorf("dialled %d times, fetched instruments %d times", *dials, fc.fetches)
	}
	cs := readChains(t, Path(s.Out.Dir, day))
	if len(cs) != 2 {
		t.Fatalf("%d chains written", len(cs))
	}
	c := cs[0]
	// Spot 25012 rounds to 25000; two strikes each side, CE and PE.
	if c.Source != "backup" || c.Und != "NIFTY" || c.Expiry != "2026-10-20" || c.TS != bus.Ms(minute) || len(c.Rows) != 10 {
		t.Errorf("chain %s %s %s ts=%d rows=%d", c.Source, c.Und, c.Expiry, c.TS, len(c.Rows))
	}
	if c.Rows[0].Strike != 24900 || c.Rows[len(c.Rows)-1].Strike != 25100 {
		t.Errorf("strikes %v..%v", c.Rows[0].Strike, c.Rows[len(c.Rows)-1].Strike)
	}
	if s.C.Polls.Load() != 2 || s.C.Chains.Load() != 2 || s.C.Rows.Load() != 20 || s.C.Errors.Load() != 0 ||
		s.C.LastOK.Load() != minute.Add(time.Minute).UnixMilli() {
		t.Errorf("counters polls=%d chains=%d rows=%d errors=%d", s.C.Polls.Load(), s.C.Chains.Load(), s.C.Rows.Load(), s.C.Errors.Load())
	}
}

func TestPollSessionHandling(t *testing.T) {
	fc := &fakeClient{}
	s, dials := service(t, fc)
	ctx := context.Background()
	minute := ist(day, 11, 0, 0)

	// A token error drops the client; the next poll dials again with the same file.
	fc.quoteErr = kiteconnect.Error{ErrorType: kiteconnect.TokenError, Message: "expired"}
	if err := s.Poll(ctx, minute); !kite.IsTokenError(err) {
		t.Fatalf("poll err %v, want a token error", err)
	}
	if s.client != nil || s.C.Errors.Load() != 1 {
		t.Errorf("client kept after token error, errors=%d", s.C.Errors.Load())
	}
	fc.quoteErr = nil
	if err := s.Poll(ctx, minute.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if *dials != 2 {
		t.Errorf("%d dials, want a redial after the token error", *dials)
	}

	// A new token in the file is picked up without an error.
	writeToken(t, s.Cfg, "fake-token-2")
	if err := s.Poll(ctx, minute.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if *dials != 3 || s.sess.AccessToken != "fake-token-2" {
		t.Errorf("%d dials, session %q", *dials, s.sess.AccessToken)
	}

	// Spot failing is not fatal: the window falls back to the middle strike.
	fc.ltpErr = errors.New("ltp down")
	if err := s.Poll(ctx, minute.Add(3*time.Minute)); err != nil {
		t.Errorf("poll without spot: %v", err)
	}
	fc.ltpErr = nil

	// No token file: counted, nothing written.
	if err := os.Remove(s.Cfg.Abs(s.Cfg.BackupRec.TokenFile)); err != nil {
		t.Fatal(err)
	}
	before := s.C.Chains.Load()
	if err := s.Poll(ctx, minute.Add(4*time.Minute)); !errors.Is(err, kite.ErrNoSession) {
		t.Errorf("poll err %v, want ErrNoSession", err)
	}
	if s.C.NoSession.Load() != 1 || s.C.Chains.Load() != before || s.client != nil {
		t.Errorf("no-session counters %d chains %d", s.C.NoSession.Load(), s.C.Chains.Load())
	}
	b, err := os.ReadFile(Path(s.Out.Dir, day))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "fake-token") {
		t.Error("token written to the chain file")
	}
	if n := strings.Count(string(b), "\n"); n != 3 {
		t.Errorf("%d chains written, want 3", n)
	}
}
