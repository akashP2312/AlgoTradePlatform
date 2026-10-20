package recorder

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"pmt/internal/bus"
	"pmt/internal/clock"
)

type MergeStats struct {
	Lines   int
	Bad     int // unparseable or rejected snapshots
	Chains  int // snapshots merged
	Skipped int // snapshots whose minute already has a ws snapshot
	Rows    int // rows inserted
	Dates   []time.Time
}

// mergeBatch is how many snapshots go in one transaction. Inserts skip rows
// already present, so a merge interrupted part way can simply be rerun.
const mergeBatch = 100

// MergeBackup loads a backup recorder JSONL file into chain_snapshots as source
// 'backup'. Unless all is set, a minute that already has a ws snapshot for the
// same series is skipped: the backup only fills holes.
func MergeBackup(ctx context.Context, pool *pgxpool.Pool, path string, all bool) (MergeStats, error) {
	var st MergeStats
	f, err := os.Open(path)
	if err != nil {
		return st, err
	}
	defer f.Close()

	type seriesDay struct {
		und, exp string
		day      int64
	}
	ws := map[seriesDay]map[int64]bool{}
	wsMinutes := func(c *bus.Chain, day time.Time) (map[int64]bool, error) {
		k := seriesDay{c.Und, c.Expiry, day.UnixMilli()}
		if m, ok := ws[k]; ok {
			return m, nil
		}
		m := map[int64]bool{}
		rows, err := pool.Query(ctx, `SELECT DISTINCT date_trunc('minute', ts) FROM chain_snapshots
			WHERE underlying = $1 AND expiry = $2 AND source = 'ws' AND ts >= $3 AND ts < $4`,
			c.Und, c.Expiry, day, day.AddDate(0, 0, 1))
		if err != nil {
			return nil, err
		}
		var ts time.Time
		if _, err := pgx.ForEachRow(rows, []any{&ts}, func() error { m[ts.UnixMilli()] = true; return nil }); err != nil {
			return nil, err
		}
		ws[k] = m
		return m, nil
	}

	sink := &chainSink{}
	pending := 0
	flush := func() error {
		if pending == 0 {
			return nil
		}
		err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
			n, err := sink.flush(ctx, tx)
			st.Rows += n
			return err
		})
		sink.reset()
		pending = 0
		return err
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		st.Lines++
		var c bus.Chain
		if err := json.Unmarshal(line, &c); err != nil || c.TS <= 0 {
			st.Bad++
			continue
		}
		c.Source = "backup"
		ts := clock.Minute(bus.FromMs(c.TS))
		day := clock.Date(ts)
		if !slices.ContainsFunc(st.Dates, day.Equal) {
			st.Dates = append(st.Dates, day)
		}
		if !all {
			m, err := wsMinutes(&c, day)
			if err != nil {
				return st, fmt.Errorf("ws minutes: %w", err)
			}
			if m[ts.UnixMilli()] {
				st.Skipped++
				continue
			}
		}
		if err := sink.addChain(&c); err != nil {
			slog.Warn("merge: snapshot rejected", "line", st.Lines, "err", err)
			st.Bad++
			continue
		}
		st.Chains++
		if pending++; pending >= mergeBatch {
			if err := flush(); err != nil {
				return st, err
			}
		}
	}
	if err := sc.Err(); err != nil {
		return st, fmt.Errorf("read %s line %d: %w", path, st.Lines+1, err)
	}
	return st, flush()
}
