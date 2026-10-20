package store

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pmt/internal/calendar"
)

// SeedCalendar upserts every F&O session for the given years and replaces the
// expiry_rules table with rules, in one transaction.
func SeedCalendar(ctx context.Context, pool *pgxpool.Pool, cal *calendar.Calendar, rules []calendar.Rule, years ...int) (int, error) {
	n := 0
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		b := &pgx.Batch{}
		for _, y := range years {
			for _, s := range cal.Days(y) {
				b.Queue(`INSERT INTO sessions (trade_date, segment, kind, open_at, close_at, note)
					VALUES ($1, 'FO', $2, NULLIF($3, '')::time, NULLIF($4, '')::time, NULLIF($5, ''))
					ON CONFLICT (trade_date, segment) DO UPDATE SET kind = EXCLUDED.kind,
						open_at = EXCLUDED.open_at, close_at = EXCLUDED.close_at, note = EXCLUDED.note`,
					s.Date, string(s.Kind), hm(s.Open), hm(s.Close), s.Note)
				n++
			}
		}
		b.Queue(`DELETE FROM expiry_rules`)
		for _, r := range rules {
			var to *time.Time
			if !r.ValidTo.IsZero() {
				to = &r.ValidTo
			}
			b.Queue(`INSERT INTO expiry_rules (underlying, cycle, weekday, valid_from, valid_to, note)
				VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))`,
				r.Underlying, r.Cycle, int(r.Weekday), r.ValidFrom, to, r.Note)
		}
		return tx.SendBatch(ctx, b).Close()
	})
	return n, err
}

func hm(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("15:04:05")
}
