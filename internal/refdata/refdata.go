// Package refdata keeps the instrument archive: raw daily dumps on disk and the
// slowly changing token -> contract map in instrument_history.
package refdata

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pmt/internal/contract"
	"pmt/internal/store"
)

// Inst is one current instrument with its contract key resolved.
type Inst struct {
	contract.Instrument
	Key        contract.Key
	Contract   string
	Underlying string
}

// Underlying is the name chains group by: the derivative's name, the index's short
// name, or the equity symbol.
func Underlying(in contract.Instrument, k contract.Key) string {
	return k.Name
}

// Resolve keeps the rows we model and attaches their keys. Duplicate tokens keep the
// first row.
func Resolve(rows []contract.Instrument) []Inst {
	seen := make(map[uint32]bool, len(rows))
	out := make([]Inst, 0, len(rows))
	for _, in := range rows {
		k, ok := contract.FromInstrument(in)
		if !ok || seen[in.Token] {
			continue
		}
		seen[in.Token] = true
		out = append(out, Inst{Instrument: in, Key: k, Contract: k.String(), Underlying: Underlying(in, k)})
	}
	return out
}

// Stats summarises one load.
type Stats struct {
	Rows, Modelled       int
	Opened, Closed       int64
	SHA256, ArchivedPath string
}

// Archive writes the raw dump to dir/instruments/yyyy/yyyy-mm-dd-<sha8>.csv and
// returns the path and checksum.
func Archive(dir string, date time.Time, raw []byte) (string, string, error) {
	sum := sha256.Sum256(raw)
	hexsum := hex.EncodeToString(sum[:])
	p := filepath.Join(dir, "instruments", date.Format("2006"), date.Format("2006-01-02")+"-"+hexsum[:8]+".csv")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", "", err
	}
	if _, err := os.Stat(p); err == nil {
		return p, hexsum, nil
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return "", "", err
	}
	return p, hexsum, os.Rename(tmp, p)
}

// Load applies one day's dump to instrument_history. A token whose contract or
// contract terms changed is closed at date and reopened; a token missing from the
// dump is closed. Reloading the same date is idempotent.
func Load(ctx context.Context, pool *pgxpool.Pool, date time.Time, rows []contract.Instrument, sha, path string, fetched time.Time) (Stats, error) {
	date = contract.DateOnly(date)
	insts := Resolve(rows)
	st := Stats{Rows: len(rows), Modelled: len(insts), SHA256: sha, ArchivedPath: path}
	if len(insts) == 0 {
		return st, fmt.Errorf("refdata: dump for %s has no usable rows", date.Format("2006-01-02"))
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return st, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `CREATE TEMP TABLE inst_new (LIKE instrument_history) ON COMMIT DROP`); err != nil {
		return st, err
	}
	cols := []string{"token", "valid_from", "contract", "exchange", "segment", "tradingsymbol", "name",
		"underlying", "expiry", "strike", "kind", "lot_size", "tick_size", "exchange_token"}
	_, err = tx.CopyFrom(ctx, pgx.Identifier{"inst_new"}, cols, pgx.CopyFromSlice(len(insts), func(i int) ([]any, error) {
		in := insts[i]
		var exp, strike any
		if !in.Key.Expiry.IsZero() {
			exp = in.Key.Expiry
		}
		if in.Key.Kind.IsOption() {
			strike = in.Key.Strike
		}
		return []any{int64(in.Token), date, in.Contract, in.Exchange, in.Segment, in.TradingSymbol, in.Name,
			in.Underlying, exp, strike, string(in.Key.Kind), in.LotSize, in.TickSize, int64(in.ExchangeToken)}, nil
	}))
	if err != nil {
		return st, fmt.Errorf("refdata: copy: %w", err)
	}
	same := `n.token = h.token AND n.contract = h.contract AND n.tradingsymbol = h.tradingsymbol
		AND n.lot_size = h.lot_size AND n.tick_size = h.tick_size`
	// A same-day reload replaces rows opened today that no longer match.
	if _, err := tx.Exec(ctx, `DELETE FROM instrument_history h WHERE h.valid_from = $1
		AND NOT EXISTS (SELECT 1 FROM inst_new n WHERE `+same+`)`, date); err != nil {
		return st, err
	}
	// Reopen anything a same-day reload had closed today and that matches again.
	tag, err := tx.Exec(ctx, `UPDATE instrument_history h SET valid_to = NULL
		WHERE h.valid_to = $1 AND EXISTS (SELECT 1 FROM inst_new n WHERE `+same+`)
		AND NOT EXISTS (SELECT 1 FROM instrument_history o WHERE o.token = h.token AND o.valid_to IS NULL)`, date)
	if err != nil {
		return st, err
	}
	tag, err = tx.Exec(ctx, `UPDATE instrument_history h SET valid_to = $1
		WHERE h.valid_to IS NULL AND h.valid_from < $1
		AND NOT EXISTS (SELECT 1 FROM inst_new n WHERE `+same+`)`, date)
	if err != nil {
		return st, err
	}
	st.Closed = tag.RowsAffected()
	tag, err = tx.Exec(ctx, `INSERT INTO instrument_history SELECT n.* FROM inst_new n
		WHERE NOT EXISTS (SELECT 1 FROM instrument_history h WHERE h.token = n.token AND h.valid_to IS NULL)`)
	if err != nil {
		return st, err
	}
	st.Opened = tag.RowsAffected()
	if sha != "" {
		if _, err := tx.Exec(ctx, `INSERT INTO instrument_dumps (dump_date, fetched_at, sha256, rows, path)
			VALUES ($1,$2,$3,$4,$5) ON CONFLICT DO NOTHING`, date, fetched, sha, len(rows), path); err != nil {
			return st, err
		}
	}
	return st, tx.Commit(ctx)
}

// Current returns every open instrument, optionally limited to some underlyings.
func Current(ctx context.Context, q store.Querier, underlyings ...string) ([]Inst, error) {
	sql := `SELECT token, exchange_token, tradingsymbol, name, COALESCE(expiry, '0001-01-01'), COALESCE(strike, 0),
		tick_size, lot_size, kind, segment, exchange, contract, underlying
		FROM instrument_history WHERE valid_to IS NULL`
	var args []any
	if len(underlyings) > 0 {
		sql += ` AND underlying = ANY($1)`
		args = append(args, underlyings)
	}
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Inst, error) {
		var in Inst
		var tok, etok int64
		var exp time.Time
		var kind string
		err := r.Scan(&tok, &etok, &in.TradingSymbol, &in.Name, &exp, &in.Strike, &in.TickSize, &in.LotSize,
			&kind, &in.Segment, &in.Exchange, &in.Contract, &in.Underlying)
		if err != nil {
			return in, err
		}
		in.Token, in.ExchangeToken = uint32(tok), uint32(etok)
		if exp.Year() > 1 {
			in.Expiry = exp
		}
		in.InstrumentType = kind
		k, err := contract.Parse(in.Contract)
		in.Key = k
		return in, err
	})
}

// AsOf returns the contract a token meant on a date (tokens are reused after expiry).
func AsOf(ctx context.Context, q store.Querier, token uint32, date time.Time) (string, error) {
	var c string
	err := q.QueryRow(ctx, `SELECT contract FROM instrument_history WHERE token = $1 AND valid_from <= $2
		AND (valid_to IS NULL OR valid_to > $2)`, int64(token), contract.DateOnly(date)).Scan(&c)
	return c, err
}
