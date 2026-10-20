// Package store owns the Postgres pool, the embedded migration runner and the small
// bookkeeping helpers (job runs, gaps) every binary shares.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"pmt/migrations"
)

// Open connects and pings, retrying for up to wait so containers can start in any order.
func Open(ctx context.Context, url string, wait time.Duration) (*pgxpool.Pool, error) {
	if url == "" {
		return nil, fmt.Errorf("store: database_url is empty")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	cfg.MaxConns = 8
	cfg.ConnConfig.RuntimeParams["timezone"] = "Asia/Kolkata"
	cfg.ConnConfig.RuntimeParams["application_name"] = "pmt"
	deadline := time.Now().Add(wait)
	for {
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err == nil {
			if err = pool.Ping(ctx); err == nil {
				return pool, nil
			}
			pool.Close()
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("store: connect: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// migrationLock is a fixed advisory-lock key so two binaries never migrate at once.
const migrationLock = 0x706d74 // "pmt"

// Migrate applies every embedded *.sql file not yet recorded in schema_migrations,
// in name order, each in its own transaction. A file whose checksum changed after
// it was applied is an error: write a new migration instead.
func Migrate(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	return MigrateFS(ctx, pool, migrations.FS)
}

// MigrateFS is Migrate over any file system, for tests.
func MigrateFS(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) ([]string, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLock); err != nil {
		return nil, err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLock)

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    text PRIMARY KEY,
		sha256     text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return nil, err
	}
	done := map[string]string{}
	rows, err := conn.Query(ctx, `SELECT version, sha256 FROM schema_migrations`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var v, s string
		if err := rows.Scan(&v, &s); err != nil {
			return nil, err
		}
		done[v] = s
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	var applied []string
	for _, name := range names {
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return applied, err
		}
		sum := sha256.Sum256(body)
		hash := hex.EncodeToString(sum[:])
		version := strings.TrimSuffix(name, ".sql")
		if prev, ok := done[version]; ok {
			if prev != hash {
				return applied, fmt.Errorf("store: migration %s changed after it was applied", name)
			}
			continue
		}
		err = pgx.BeginFunc(ctx, conn.Conn(), func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, sha256) VALUES ($1, $2)`, version, hash)
			return err
		})
		if err != nil {
			return applied, fmt.Errorf("store: migration %s: %w", name, err)
		}
		applied = append(applied, version)
	}
	return applied, nil
}

// JobRun records one execution of a scheduled or manual job in job_runs.
type JobRun struct {
	pool *pgxpool.Pool
	ID   int64
}

func StartJob(ctx context.Context, pool *pgxpool.Pool, job, args string) (*JobRun, error) {
	j := &JobRun{pool: pool}
	err := pool.QueryRow(ctx,
		`INSERT INTO job_runs (job, args, started_at, status) VALUES ($1, $2, now(), 'running') RETURNING id`,
		job, args).Scan(&j.ID)
	return j, err
}

// LastOK is when job last started a run that finished ok; zero if never.
func LastOK(ctx context.Context, pool *pgxpool.Pool, job string) (time.Time, error) {
	var t *time.Time
	err := pool.QueryRow(ctx, `SELECT max(started_at) FROM job_runs WHERE job = $1 AND status = 'ok'`, job).Scan(&t)
	if err != nil || t == nil {
		return time.Time{}, err
	}
	return *t, nil
}

// Finish marks the run ok, or error with err's text.
func (j *JobRun) Finish(ctx context.Context, detail string, err error) error {
	status := "ok"
	if err != nil {
		status = "error"
		if detail != "" {
			detail += ": "
		}
		detail += err.Error()
	}
	_, e := j.pool.Exec(ctx,
		`UPDATE job_runs SET finished_at = now(), status = $2, detail = NULLIF($3, '') WHERE id = $1`,
		j.ID, status, detail)
	return e
}

// JobSummary is the latest run of one job, for the SYS page.
type JobSummary struct {
	Job        string
	Args       string
	StartedAt  time.Time
	FinishedAt *time.Time
	Status     string
	Detail     string
}

// LastJobs returns the latest run of every job.
func LastJobs(ctx context.Context, pool *pgxpool.Pool) ([]JobSummary, error) {
	rows, err := pool.Query(ctx, `SELECT DISTINCT ON (job) job, args, started_at, finished_at, status, COALESCE(detail, '')
		FROM job_runs ORDER BY job, started_at DESC`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (JobSummary, error) {
		var s JobSummary
		err := r.Scan(&s.Job, &s.Args, &s.StartedAt, &s.FinishedAt, &s.Status, &s.Detail)
		return s, err
	})
}

// Gap is one contiguous stretch of missing recording.
type Gap struct {
	TradeDate  time.Time
	Kind       string // snapshot | ticks | harvest
	Underlying string
	Expiry     *time.Time
	Start, End time.Time
	Minutes    int
	Filled     int
	FilledBy   string
	Detail     string
}

// UpsertGap inserts or refreshes a gap row keyed on (date, kind, underlying, expiry, start).
func UpsertGap(ctx context.Context, q Querier, g Gap) error {
	_, err := q.Exec(ctx, `INSERT INTO recorder_gaps
		(trade_date, kind, underlying, expiry, start_ts, end_ts, minutes, filled_minutes, filled_by, detail)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),NULLIF($10,''))
		ON CONFLICT (trade_date, kind, underlying, COALESCE(expiry, '1970-01-01'::date), start_ts)
		DO UPDATE SET end_ts = EXCLUDED.end_ts, minutes = EXCLUDED.minutes,
			filled_minutes = EXCLUDED.filled_minutes, filled_by = EXCLUDED.filled_by, detail = EXCLUDED.detail`,
		g.TradeDate, g.Kind, g.Underlying, g.Expiry, g.Start, g.End, g.Minutes, g.Filled, g.FilledBy, g.Detail)
	return err
}

// Querier is satisfied by *pgxpool.Pool, *pgx.Conn and pgx.Tx.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}
