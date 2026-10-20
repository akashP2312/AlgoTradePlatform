// Package loaders fetches and loads the nightly exchange files: the F&O
// bhavcopy (UDiFF since July 2024, the legacy format before it), NSE
// participant-wise open interest, and daily index closes (India VIX and the
// underlyings) from Kite day candles.
package loaders

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"pmt/internal/contract"
)

// EODRow is one eod_contracts row.
type EODRow struct {
	TradeDate  time.Time
	Key        contract.Key
	Open       *float64
	High       *float64
	Low        *float64
	Close      *float64
	Last       *float64
	PrevClose  *float64
	Settle     *float64
	UndPrice   *float64
	OI         *int64
	OIChange   *int64
	Volume     *int64
	VolumeUnit string
	Turnover   *float64 // rupees
	Trades     *int64
	LotSize    *int32
	Source     string
}

// UDiFFStart is the first trade date NSE publishes in the UDiFF format; earlier
// dates use the legacy file. Verify the exact cut-over date.
var UDiFFStart = time.Date(2024, 7, 8, 0, 0, 0, 0, time.UTC)

// BhavURL is the archive URL of the F&O bhavcopy for date. Verify both paths
// against nsearchives before relying on them.
func BhavURL(base string, date time.Time) string {
	base = strings.TrimRight(base, "/")
	if !date.Before(UDiFFStart) {
		return fmt.Sprintf("%s/content/fo/BhavCopy_NSE_FO_0_0_0_%s_F_0000.csv.zip", base, date.Format("20060102"))
	}
	return fmt.Sprintf("%s/content/historical/DERIVATIVES/%s/%s/fo%sbhav.csv.zip", base,
		date.Format("2006"), strings.ToUpper(date.Format("Jan")), strings.ToUpper(date.Format("02Jan2006")))
}

// Unzip returns the first file in a zip archive, or raw unchanged when it is
// not a zip (NSE sometimes serves the plain CSV).
func Unzip(raw []byte) ([]byte, error) {
	if !bytes.HasPrefix(raw, []byte("PK")) {
		return raw, nil
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, err
	}
	if len(zr.File) == 0 {
		return nil, errors.New("empty zip")
	}
	f, err := zr.File[0].Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 512<<20))
}

// ParseBhav detects the format from the header and parses either.
func ParseBhav(csvData []byte) ([]EODRow, error) {
	head, _, _ := bytes.Cut(csvData, []byte("\n"))
	switch {
	case bytes.Contains(head, []byte("FinInstrmTp")):
		return ParseUDiFF(csvData)
	case bytes.Contains(head, []byte("INSTRUMENT")):
		return ParseLegacy(csvData)
	}
	return nil, errors.New("bhavcopy: unknown header")
}

// table reads a CSV into rows addressed by header name.
type table struct {
	idx  map[string]int
	rows [][]string
}

func readTable(data []byte) (*table, error) {
	r := csv.NewReader(bytes.NewReader(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))))
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true
	all, err := r.ReadAll()
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, errors.New("empty csv")
	}
	t := &table{idx: map[string]int{}, rows: all[1:]}
	for i, h := range all[0] {
		t.idx[strings.TrimSpace(h)] = i
	}
	return t, nil
}

func (t *table) need(cols ...string) error {
	for _, c := range cols {
		if _, ok := t.idx[c]; !ok {
			return fmt.Errorf("missing column %s", c)
		}
	}
	return nil
}

func (t *table) get(row []string, col string) string {
	i, ok := t.idx[col]
	if !ok || i >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[i])
}

func fptr(s string) (*float64, error) {
	if s == "" || s == "-" {
		return nil, nil
	}
	v, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

func iptr(s string) (*int64, error) {
	if s == "" || s == "-" {
		return nil, nil
	}
	s = strings.ReplaceAll(s, ",", "")
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		f, ferr := strconv.ParseFloat(s, 64)
		if ferr != nil {
			return nil, err
		}
		v = int64(f)
	}
	return &v, nil
}

func scale(p *float64, k float64) *float64 {
	if p == nil {
		return nil
	}
	v := *p * k
	return &v
}

type fieldErr struct {
	errs []error
}

func (f *fieldErr) f(s string) *float64 {
	v, err := fptr(s)
	if err != nil {
		f.errs = append(f.errs, err)
	}
	return v
}

func (f *fieldErr) i(s string) *int64 {
	v, err := iptr(s)
	if err != nil {
		f.errs = append(f.errs, err)
	}
	return v
}

func date(s string, layouts ...string) (time.Time, error) {
	for _, l := range layouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("date %q", s)
}

// udiffKinds maps FinInstrmTp to our kind (options are refined by OptnTp).
var udiffKinds = map[string]contract.Kind{"IDF": contract.FUT, "STF": contract.FUT, "IDO": contract.CE, "STO": contract.CE}

// ParseUDiFF parses the UDiFF F&O bhavcopy. TtlTradgVol is recorded with
// volume_unit 'udiff' until its unit (contracts or shares) is verified.
func ParseUDiFF(data []byte) ([]EODRow, error) {
	t, err := readTable(data)
	if err != nil {
		return nil, err
	}
	if err := t.need("TradDt", "FinInstrmTp", "TckrSymb", "XpryDt", "StrkPric", "OptnTp", "ClsPric", "OpnIntrst"); err != nil {
		return nil, fmt.Errorf("udiff: %w", err)
	}
	out := make([]EODRow, 0, len(t.rows))
	for n, row := range t.rows {
		if len(row) < 2 {
			continue
		}
		kind, ok := udiffKinds[t.get(row, "FinInstrmTp")]
		if !ok {
			continue
		}
		td, err := date(t.get(row, "TradDt"), time.DateOnly)
		if err != nil {
			return nil, fmt.Errorf("udiff line %d: %w", n+2, err)
		}
		exp, err := date(t.get(row, "XpryDt"), time.DateOnly)
		if err != nil {
			return nil, fmt.Errorf("udiff line %d: %w", n+2, err)
		}
		k := contract.Key{Exchange: "NFO", Name: t.get(row, "TckrSymb"), Expiry: exp, Kind: kind}
		var fe fieldErr
		if kind != contract.FUT {
			switch t.get(row, "OptnTp") {
			case "CE":
				k.Kind = contract.CE
			case "PE":
				k.Kind = contract.PE
			default:
				return nil, fmt.Errorf("udiff line %d: option type %q", n+2, t.get(row, "OptnTp"))
			}
			if s := fe.f(t.get(row, "StrkPric")); s != nil {
				k.Strike = *s
			}
		}
		r := EODRow{
			TradeDate: td, Key: k, Source: "udiff", VolumeUnit: "udiff",
			Open: fe.f(t.get(row, "OpnPric")), High: fe.f(t.get(row, "HghPric")), Low: fe.f(t.get(row, "LwPric")),
			Close: fe.f(t.get(row, "ClsPric")), Last: fe.f(t.get(row, "LastPric")), PrevClose: fe.f(t.get(row, "PrvsClsgPric")),
			Settle: fe.f(t.get(row, "SttlmPric")), UndPrice: fe.f(t.get(row, "UndrlygPric")),
			OI: fe.i(t.get(row, "OpnIntrst")), OIChange: fe.i(t.get(row, "ChngInOpnIntrst")),
			Volume: fe.i(t.get(row, "TtlTradgVol")), Turnover: fe.f(t.get(row, "TtlTrfVal")),
			Trades: fe.i(t.get(row, "TtlNbOfTxsExctd")),
		}
		if lot := fe.i(t.get(row, "NewBrdLotQty")); lot != nil {
			v := int32(*lot)
			r.LotSize = &v
		}
		if len(fe.errs) > 0 {
			return nil, fmt.Errorf("udiff line %d: %w", n+2, errors.Join(fe.errs...))
		}
		out = append(out, r)
	}
	return out, nil
}

var legacyKinds = map[string]contract.Kind{"FUTIDX": contract.FUT, "FUTSTK": contract.FUT, "OPTIDX": contract.CE, "OPTSTK": contract.CE}

// ParseLegacy parses the pre-UDiFF F&O bhavcopy. Volume is CONTRACTS and
// turnover is VAL_INLAKH converted to rupees.
func ParseLegacy(data []byte) ([]EODRow, error) {
	t, err := readTable(data)
	if err != nil {
		return nil, err
	}
	if err := t.need("INSTRUMENT", "SYMBOL", "EXPIRY_DT", "STRIKE_PR", "OPTION_TYP", "CLOSE", "OPEN_INT", "TIMESTAMP"); err != nil {
		return nil, fmt.Errorf("legacy: %w", err)
	}
	out := make([]EODRow, 0, len(t.rows))
	for n, row := range t.rows {
		if len(row) < 2 {
			continue
		}
		kind, ok := legacyKinds[t.get(row, "INSTRUMENT")]
		if !ok {
			continue
		}
		td, err := date(t.get(row, "TIMESTAMP"), "02-Jan-2006", "02-JAN-2006")
		if err != nil {
			return nil, fmt.Errorf("legacy line %d: %w", n+2, err)
		}
		exp, err := date(t.get(row, "EXPIRY_DT"), "02-Jan-2006", "02-JAN-2006")
		if err != nil {
			return nil, fmt.Errorf("legacy line %d: %w", n+2, err)
		}
		k := contract.Key{Exchange: "NFO", Name: t.get(row, "SYMBOL"), Expiry: exp, Kind: kind}
		var fe fieldErr
		if kind != contract.FUT {
			switch t.get(row, "OPTION_TYP") {
			case "CE":
				k.Kind = contract.CE
			case "PE":
				k.Kind = contract.PE
			default:
				return nil, fmt.Errorf("legacy line %d: option type %q", n+2, t.get(row, "OPTION_TYP"))
			}
			if s := fe.f(t.get(row, "STRIKE_PR")); s != nil {
				k.Strike = *s
			}
		}
		r := EODRow{
			TradeDate: td, Key: k, Source: "legacy", VolumeUnit: "contracts",
			Open: fe.f(t.get(row, "OPEN")), High: fe.f(t.get(row, "HIGH")), Low: fe.f(t.get(row, "LOW")),
			Close: fe.f(t.get(row, "CLOSE")), Settle: fe.f(t.get(row, "SETTLE_PR")),
			OI: fe.i(t.get(row, "OPEN_INT")), OIChange: fe.i(t.get(row, "CHG_IN_OI")),
			Volume: fe.i(t.get(row, "CONTRACTS")), Turnover: scale(fe.f(t.get(row, "VAL_INLAKH")), 1e5),
		}
		if len(fe.errs) > 0 {
			return nil, fmt.Errorf("legacy line %d: %w", n+2, errors.Join(fe.errs...))
		}
		out = append(out, r)
	}
	return out, nil
}
