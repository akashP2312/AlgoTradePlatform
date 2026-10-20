package loaders

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func dateCol(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

var eodCols = []string{"trade_date", "contract", "exchange", "underlying", "expiry", "strike", "kind",
	"open", "high", "low", "close", "last", "prev_close", "settle", "underlying_price",
	"oi", "oi_change", "volume", "volume_unit", "turnover", "trades", "lot_size", "source"}

// SaveEOD replaces rows by (trade_date, contract): COPY into a temp table, then
// one upsert, so a rerun of a day is idempotent.
func SaveEOD(ctx context.Context, pool *pgxpool.Pool, rows []EODRow) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	var n int64
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `CREATE TEMP TABLE eod_in (LIKE eod_contracts INCLUDING DEFAULTS) ON COMMIT DROP`); err != nil {
			return err
		}
		src := pgx.CopyFromSlice(len(rows), func(i int) ([]any, error) {
			r := &rows[i]
			var exp *time.Time
			var strike *float64
			if !r.Key.Expiry.IsZero() {
				e := dateCol(r.Key.Expiry)
				exp = &e
			}
			if r.Key.Strike != 0 {
				s := r.Key.Strike
				strike = &s
			}
			return []any{dateCol(r.TradeDate), r.Key.String(), r.Key.Exchange, r.Key.Name, exp, strike, string(r.Key.Kind),
				r.Open, r.High, r.Low, r.Close, r.Last, r.PrevClose, r.Settle, r.UndPrice,
				r.OI, r.OIChange, r.Volume, r.VolumeUnit, r.Turnover, r.Trades, r.LotSize, r.Source}, nil
		})
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"eod_in"}, eodCols, src); err != nil {
			return fmt.Errorf("copy: %w", err)
		}
		set := ""
		for _, c := range eodCols[2:] {
			set += fmt.Sprintf(", %s = EXCLUDED.%s", c, c)
		}
		tag, err := tx.Exec(ctx, `INSERT INTO eod_contracts SELECT DISTINCT ON (trade_date, contract) * FROM eod_in
			ON CONFLICT (trade_date, contract) DO UPDATE SET `+set[2:])
		n = tag.RowsAffected()
		return err
	})
	return n, err
}

// PartOI is one participant_oi row.
type PartOI struct {
	TradeDate   time.Time
	ClientType  string
	Instrument  string
	Long, Short int64
}

func SavePartOI(ctx context.Context, pool *pgxpool.Pool, rows []PartOI) error {
	b := &pgx.Batch{}
	for _, r := range rows {
		b.Queue(`INSERT INTO participant_oi (trade_date, client_type, instrument, long, short) VALUES ($1,$2,$3,$4,$5)
			ON CONFLICT (trade_date, client_type, instrument) DO UPDATE SET long = EXCLUDED.long, short = EXCLUDED.short`,
			dateCol(r.TradeDate), r.ClientType, r.Instrument, r.Long, r.Short)
	}
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { return tx.SendBatch(ctx, b).Close() })
}

// IndexBar is one index_eod row.
type IndexBar struct {
	TradeDate       time.Time
	Contract        string
	Open, High, Low *float64
	Close           float64
	Source          string
}

func SaveIndexEOD(ctx context.Context, pool *pgxpool.Pool, rows []IndexBar) error {
	b := &pgx.Batch{}
	for _, r := range rows {
		b.Queue(`INSERT INTO index_eod (trade_date, contract, open, high, low, close, source) VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (trade_date, contract) DO UPDATE SET open = EXCLUDED.open, high = EXCLUDED.high,
			low = EXCLUDED.low, close = EXCLUDED.close, source = EXCLUDED.source`,
			dateCol(r.TradeDate), r.Contract, r.Open, r.High, r.Low, r.Close, r.Source)
	}
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error { return tx.SendBatch(ctx, b).Close() })
}
