package refdata

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"pmt/internal/contract"
)

// day is the comparable form of a date across the UTC (keys) and IST (clock) conventions.
func day(t time.Time) string { return t.Format("2006-01-02") }

// Live drops contracts that expired before today.
func Live(insts []Inst, today time.Time) []Inst {
	td := day(today)
	out := insts[:0:0]
	for _, in := range insts {
		if in.Key.Expiry.IsZero() || day(in.Key.Expiry) >= td {
			out = append(out, in)
		}
	}
	return out
}

// Futures returns the live futures of a name on an exchange, nearest first.
func Futures(insts []Inst, exch, name string, today time.Time) []Inst {
	var out []Inst
	for _, in := range Live(insts, today) {
		if in.Key.Kind == contract.FUT && in.Exchange == exch && in.Key.Name == name {
			out = append(out, in)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key.Expiry.Before(out[j].Key.Expiry) })
	return out
}

// Expiries lists the live option expiries of a name, nearest first.
func Expiries(insts []Inst, exch, name string, today time.Time) []time.Time {
	seen := map[string]time.Time{}
	for _, in := range Live(insts, today) {
		if in.Key.Kind.IsOption() && in.Exchange == exch && in.Key.Name == name {
			seen[day(in.Key.Expiry)] = in.Key.Expiry
		}
	}
	out := make([]time.Time, 0, len(seen))
	for _, t := range seen {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

// WithMonthlies keeps the nearest wide expiries plus the nearest n monthlies (the
// last listed expiry of each calendar month), which the IV engine's 30-day VIX
// needs past the weeklies: the expiries the ticker streams. all is sorted ascending; so is the result.
func WithMonthlies(all []time.Time, wide, n int) []time.Time {
	out := all
	if len(out) > wide {
		out = append([]time.Time(nil), all[:wide]...)
	}
	if n <= 0 || len(out) == len(all) {
		return out
	}
	have := make(map[time.Time]bool, len(out))
	for _, e := range out {
		have[e] = true
	}
	for i, e := range all {
		if n == 0 {
			break
		}
		last := i == len(all)-1 || all[i+1].Month() != e.Month() || all[i+1].Year() != e.Year()
		if !last {
			continue
		}
		n--
		if !have[e] {
			out = append(out, e)
		}
	}
	return out
}

// Strikes lists the distinct strikes of rows, ascending.
func Strikes(rows []Inst) []float64 {
	seen := map[float64]bool{}
	var out []float64
	for _, r := range rows {
		if !seen[r.Key.Strike] {
			seen[r.Key.Strike] = true
			out = append(out, r.Key.Strike)
		}
	}
	sort.Float64s(out)
	return out
}

// Window is the listed strikes from n below the strike nearest spot to n above it.
// Counting listed strikes rather than steps copes with mixed strike intervals.
func Window(strikes []float64, spot float64, n int) []float64 {
	if len(strikes) == 0 {
		return nil
	}
	atm := Nearest(strikes, spot)
	lo, hi := max(0, atm-n), min(len(strikes), atm+n+1)
	return strikes[lo:hi]
}

// Nearest is the index of the strike closest to spot; ties go to the lower strike.
func Nearest(strikes []float64, spot float64) int {
	i := sort.SearchFloat64s(strikes, spot)
	if i == len(strikes) || (i > 0 && spot-strikes[i-1] <= strikes[i]-spot) {
		i--
	}
	return i
}

// FutureFor is the future expiring with exp, else the nearest of futs (which
// Futures returns nearest first). It is the zero Inst when futs is empty.
func FutureFor(futs []Inst, exp time.Time) Inst {
	for _, f := range futs {
		if day(f.Key.Expiry) == day(exp) {
			return f
		}
	}
	if len(futs) > 0 {
		return futs[0]
	}
	return Inst{}
}

// BySymbol finds an instrument by exchange and tradingsymbol.
func BySymbol(insts []Inst, exch, sym string) (Inst, bool) {
	for _, in := range insts {
		if in.Exchange == exch && in.TradingSymbol == sym {
			return in, true
		}
	}
	return Inst{}, false
}

// ResolveExtra resolves "EXCH:TRADINGSYMBOL", or "EXCH:NAME:FUT:n" for the nth
// nearest live future of a name.
func ResolveExtra(insts []Inst, spec string, today time.Time) (Inst, error) {
	parts := strings.Split(spec, ":")
	switch {
	case len(parts) == 2:
		if in, ok := BySymbol(insts, parts[0], parts[1]); ok {
			return in, nil
		}
	case len(parts) == 4 && parts[2] == "FUT":
		n, err := strconv.Atoi(parts[3])
		if err != nil || n < 1 {
			return Inst{}, fmt.Errorf("refdata: extra %q: bad future index", spec)
		}
		if f := Futures(insts, parts[0], parts[1], today); len(f) >= n {
			return f[n-1], nil
		}
	default:
		return Inst{}, fmt.Errorf("refdata: extra %q: want EXCH:SYMBOL or EXCH:NAME:FUT:n", spec)
	}
	return Inst{}, fmt.Errorf("refdata: extra %q not in today's instruments", spec)
}

// ReadConstituents reads the Symbol column of a niftyindices constituent CSV.
func ReadConstituents(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return ParseConstituents(f)
}

func ParseConstituents(r io.Reader) ([]string, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	head, err := cr.Read()
	if err != nil {
		return nil, err
	}
	col := -1
	for i, h := range head {
		if strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(h, "\xef\xbb\xbf")), "Symbol") {
			col = i
		}
	}
	if col < 0 {
		return nil, fmt.Errorf("refdata: constituents file has no Symbol column")
	}
	var out []string
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if col < len(rec) {
			if s := strings.TrimSpace(rec[col]); s != "" {
				out = append(out, s)
			}
		}
	}
}
