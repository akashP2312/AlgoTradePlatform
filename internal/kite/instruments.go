package kite

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"pmt/internal/contract"
)

// FetchInstruments downloads the full instrument dump (all exchanges) as raw CSV.
// The bytes are archived as-is; ParseInstruments turns them into rows.
func (r *REST) FetchInstruments(ctx context.Context) ([]byte, error) {
	return r.get(ctx, "/instruments")
}

// FetchInstrumentsFor downloads one exchange's dump (NFO, NSE, BFO, ...).
func (r *REST) FetchInstrumentsFor(ctx context.Context, exchange string) ([]byte, error) {
	return r.get(ctx, "/instruments/"+exchange)
}

// ParseInstruments reads Kite's instrument CSV by header name, so a reordered or
// extended header does not silently shift columns.
func ParseInstruments(raw []byte) ([]contract.Instrument, error) {
	cr := csv.NewReader(bytes.NewReader(raw))
	cr.ReuseRecord = true
	cr.FieldsPerRecord = -1
	head, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("instruments: header: %w", err)
	}
	col := map[string]int{}
	for i, h := range head {
		col[strings.TrimSpace(strings.TrimPrefix(h, "\ufeff"))] = i
	}
	for _, need := range []string{"instrument_token", "exchange_token", "tradingsymbol", "name",
		"expiry", "strike", "tick_size", "lot_size", "instrument_type", "segment", "exchange"} {
		if _, ok := col[need]; !ok {
			return nil, fmt.Errorf("instruments: missing column %q", need)
		}
	}
	get := func(rec []string, name string) string {
		if i := col[name]; i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}
	var out []contract.Instrument
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("instruments: line %d: %w", line, err)
		}
		tok, err := strconv.ParseUint(get(rec, "instrument_token"), 10, 32)
		if err != nil {
			return nil, fmt.Errorf("instruments: line %d: instrument_token: %w", line, err)
		}
		in := contract.Instrument{
			Token:          uint32(tok),
			TradingSymbol:  get(rec, "tradingsymbol"),
			Name:           get(rec, "name"),
			InstrumentType: get(rec, "instrument_type"),
			Segment:        get(rec, "segment"),
			Exchange:       get(rec, "exchange"),
		}
		if v, err := strconv.ParseUint(get(rec, "exchange_token"), 10, 32); err == nil {
			in.ExchangeToken = uint32(v)
		}
		in.Strike, _ = strconv.ParseFloat(get(rec, "strike"), 64)
		in.TickSize, _ = strconv.ParseFloat(get(rec, "tick_size"), 64)
		in.LotSize, _ = strconv.Atoi(get(rec, "lot_size"))
		if e := get(rec, "expiry"); e != "" {
			d, err := time.Parse("2006-01-02", e)
			if err != nil {
				return nil, fmt.Errorf("instruments: line %d: expiry %q: %w", line, e, err)
			}
			in.Expiry = d
		}
		out = append(out, in)
	}
	return out, nil
}
