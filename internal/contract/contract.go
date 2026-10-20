// Package contract defines the instrument key used everywhere instead of Kite's
// instrument tokens, which are reused after expiry:
//
//	NFO|NIFTY|2026-10-27|25000|CE   option
//	NFO|NIFTY|2026-10-27||FUT       future
//	NSE|NIFTY|||IDX                 index
//	NSE|RELIANCE|||EQ               cash equity
package contract

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Kind string

const (
	CE  Kind = "CE"
	PE  Kind = "PE"
	FUT Kind = "FUT"
	IDX Kind = "IDX"
	EQ  Kind = "EQ"
)

func (k Kind) IsOption() bool { return k == CE || k == PE }

type Key struct {
	Exchange string
	Name     string    // underlying name for derivatives and indices, symbol for EQ
	Expiry   time.Time // zero for IDX and EQ; a calendar date (midnight UTC)
	Strike   float64   // zero unless option
	Kind     Kind
}

const dateFmt = "2006-01-02"

// String renders the canonical pipe-separated key.
func (k Key) String() string {
	var b strings.Builder
	b.WriteString(k.Exchange)
	b.WriteByte('|')
	b.WriteString(k.Name)
	b.WriteByte('|')
	if !k.Expiry.IsZero() {
		b.WriteString(k.Expiry.Format(dateFmt))
	}
	b.WriteByte('|')
	if k.Kind.IsOption() {
		b.WriteString(FormatStrike(k.Strike))
	}
	b.WriteByte('|')
	b.WriteString(string(k.Kind))
	return b.String()
}

// FormatStrike prints strikes without trailing zeros: 25000, 102.5.
func FormatStrike(s float64) string { return strconv.FormatFloat(s, 'f', -1, 64) }

// Parse is the inverse of String.
func Parse(s string) (Key, error) {
	p := strings.Split(s, "|")
	if len(p) != 5 {
		return Key{}, fmt.Errorf("contract %q: want 5 fields", s)
	}
	k := Key{Exchange: p[0], Name: p[1], Kind: Kind(p[4])}
	if k.Exchange == "" || k.Name == "" {
		return Key{}, fmt.Errorf("contract %q: exchange and name required", s)
	}
	switch k.Kind {
	case CE, PE, FUT, IDX, EQ:
	default:
		return Key{}, fmt.Errorf("contract %q: unknown kind", s)
	}
	if p[2] != "" {
		d, err := time.Parse(dateFmt, p[2])
		if err != nil {
			return Key{}, fmt.Errorf("contract %q: %w", s, err)
		}
		k.Expiry = d
	}
	if p[3] != "" {
		v, err := strconv.ParseFloat(p[3], 64)
		if err != nil {
			return Key{}, fmt.Errorf("contract %q: %w", s, err)
		}
		k.Strike = v
	}
	if k.Kind.IsOption() && (k.Expiry.IsZero() || p[3] == "") {
		return Key{}, fmt.Errorf("contract %q: option needs expiry and strike", s)
	}
	if k.Kind == FUT && k.Expiry.IsZero() {
		return Key{}, fmt.Errorf("contract %q: future needs expiry", s)
	}
	return k, nil
}

// Subject renders a NATS subject suffix: lowercase, empty fields as "_", anything
// outside [a-z0-9-] as "_", and a strike's decimal point as "p".
//
//	NFO|NIFTY|2026-10-27|25000|CE -> nfo.nifty.20261027.25000.ce
func (k Key) Subject() string {
	exp := "_"
	if !k.Expiry.IsZero() {
		exp = k.Expiry.Format("20060102")
	}
	strike := "_"
	if k.Kind.IsOption() {
		strike = strings.ReplaceAll(FormatStrike(k.Strike), ".", "p")
	}
	return strings.Join([]string{tok(k.Exchange), tok(k.Name), exp, tok(strike), tok(string(k.Kind))}, ".")
}

func tok(s string) string {
	if s == "" {
		return "_"
	}
	b := []byte(strings.ToLower(s))
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			b[i] = '_'
		}
	}
	return string(b)
}

// Token makes a single subject token from free text, with the same rules.
func Token(s string) string { return tok(s) }

// indexNames maps Kite index tradingsymbols to underlying names.
var indexNames = map[string]string{
	"NIFTY 50":          "NIFTY",
	"NIFTY BANK":        "BANKNIFTY",
	"NIFTY FIN SERVICE": "FINNIFTY",
	"NIFTY MID SELECT":  "MIDCPNIFTY",
	"INDIA VIX":         "INDIAVIX",
	"SENSEX":            "SENSEX",
	"BANKEX":            "BANKEX",
}

// IndexName maps an index tradingsymbol to its short name; others are returned
// upper-cased with spaces kept (they are still valid key fields).
func IndexName(tradingsymbol string) string {
	if n, ok := indexNames[tradingsymbol]; ok {
		return n
	}
	return strings.ToUpper(tradingsymbol)
}

// IndexSymbol is the reverse of IndexName for the known indices.
func IndexSymbol(name string) (string, bool) {
	for s, n := range indexNames {
		if n == name {
			return s, true
		}
	}
	return "", false
}

// Instrument is one row of Kite's /instruments CSV.
type Instrument struct {
	Token          uint32
	ExchangeToken  uint32
	TradingSymbol  string
	Name           string
	Expiry         time.Time
	Strike         float64
	TickSize       float64
	LotSize        int
	InstrumentType string // CE, PE, FUT, EQ
	Segment        string // NFO-OPT, NFO-FUT, INDICES, NSE, ...
	Exchange       string
}

// FromInstrument builds the contract key for a Kite instrument row. ok is false for
// rows we do not model (spreads, unknown types).
func FromInstrument(in Instrument) (Key, bool) {
	k := Key{Exchange: in.Exchange}
	switch {
	case in.Segment == "INDICES":
		k.Name, k.Kind = IndexName(in.TradingSymbol), IDX
	case in.InstrumentType == "CE" || in.InstrumentType == "PE":
		if in.Expiry.IsZero() || in.Name == "" {
			return Key{}, false
		}
		k.Name, k.Expiry, k.Strike, k.Kind = in.Name, in.Expiry, in.Strike, Kind(in.InstrumentType)
	case in.InstrumentType == "FUT":
		if in.Expiry.IsZero() || in.Name == "" {
			return Key{}, false
		}
		k.Name, k.Expiry, k.Kind = in.Name, in.Expiry, FUT
	case in.InstrumentType == "EQ":
		k.Name, k.Kind = in.TradingSymbol, EQ
	default:
		return Key{}, false
	}
	return k, true
}

// Option is a convenience constructor.
func Option(exch, name string, expiry time.Time, strike float64, kind Kind) Key {
	return Key{Exchange: exch, Name: name, Expiry: DateOnly(expiry), Strike: strike, Kind: kind}
}

// DateOnly normalises a time to its calendar date at midnight UTC, the form used
// in keys and DATE columns.
func DateOnly(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
