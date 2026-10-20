// Package backup writes encrypted database backups and proves they restore.
//
// A backup is pg_dump in custom format streamed through age to the outbox
// directory, which a separate copy step (rclone or rsync, see deploy/) moves
// off the machine. Next to each dump goes a small JSON manifest of row counts
// taken just before the dump; the restore test decrypts the newest dump into a
// scratch database and checks every table has at least those rows.
//
// The age recipient (public key) is all the backup side needs. The identity
// (private key) is only needed to restore and should not live on the VM except
// while a restore test runs.
package backup

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"filippo.io/age"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Tables are counted into the manifest and checked after a restore. All of them
// only grow between the count and the dump, so restored >= counted.
var Tables = []string{
	"instrument_dumps", "instrument_history", "sessions", "expiry_rules",
	"ticks", "bars_1m", "chain_snapshots", "eod_contracts", "index_eod",
	"participant_oi", "orders", "recorder_gaps", "harvest_log",
}

// Conn is how the Postgres client tools reach the server. The password goes in
// PGPASSWORD, never on the command line.
type Conn struct {
	Host, Port, User, Password, Database, SSLMode string
}

// ParseConn reads a postgres:// URL.
func ParseConn(raw string) (Conn, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return Conn{}, fmt.Errorf("database url: %w", err)
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return Conn{}, errors.New("database url: want postgres://")
	}
	c := Conn{Host: u.Hostname(), Port: u.Port(), Database: strings.TrimPrefix(u.Path, "/"), SSLMode: u.Query().Get("sslmode")}
	if u.User != nil {
		c.User = u.User.Username()
		c.Password, _ = u.User.Password()
	}
	if c.Port == "" {
		c.Port = "5432"
	}
	return c, nil
}

func (c Conn) args(db string) []string {
	if db == "" {
		db = c.Database
	}
	a := []string{"--host", c.Host, "--port", c.Port, "--dbname", db}
	if c.User != "" {
		a = append(a, "--username", c.User)
	}
	return append(a, "--no-password")
}

func (c Conn) env() []string {
	env := append(os.Environ(), "PGPASSWORD="+c.Password)
	if c.SSLMode != "" {
		env = append(env, "PGSSLMODE="+c.SSLMode)
	}
	return env
}

// Tools are the command prefixes for pg_dump and pg_restore. Their major
// version must be at least the server's; the jobs image ships matching ones.
type Tools struct {
	Dump, Restore []string
	// Reach, when set, maps the connection to how the tools see the server
	// (tests run them inside the database container).
	Reach func(Conn) Conn
}

var DefaultTools = Tools{Dump: []string{"pg_dump"}, Restore: []string{"pg_restore"}}

func (t Tools) cmd(ctx context.Context, prefix []string, c Conn, db string, args ...string) *exec.Cmd {
	if t.Reach != nil {
		c = t.Reach(c)
	}
	all := append(slices.Clone(prefix[1:]), c.args(db)...)
	all = append(all, args...)
	cmd := exec.CommandContext(ctx, prefix[0], all...)
	cmd.Env = c.env()
	return cmd
}

// Manifest sits next to each dump, unencrypted: it holds counts, not data.
type Manifest struct {
	File      string           `json:"file"`
	CreatedAt time.Time        `json:"created_at"`
	Bytes     int64            `json:"bytes"`
	SHA256    string           `json:"sha256"` // of the encrypted file
	Counts    map[string]int64 `json:"counts"`
}

const prefix, suffix = "pmt-", ".dump.age"

// Result describes one backup.
type Result struct {
	Path     string
	Manifest Manifest
	Pruned   int
}

// Run counts rows, dumps the database through age into outbox and prunes local
// dumps older than keepDays (0 keeps everything).
func Run(ctx context.Context, pool *pgxpool.Pool, c Conn, tools Tools, recipients, outbox string, keepDays int, now time.Time) (Result, error) {
	var res Result
	rs, err := age.ParseRecipients(strings.NewReader(recipients))
	if err != nil {
		return res, fmt.Errorf("age recipient: %w", err)
	}
	if len(rs) == 0 {
		return res, errors.New("age recipient: none given")
	}
	counts, err := Count(ctx, pool)
	if err != nil {
		return res, err
	}
	if err := os.MkdirAll(outbox, 0o750); err != nil {
		return res, err
	}
	name := prefix + now.UTC().Format("20060102T150405Z") + suffix
	final := filepath.Join(outbox, name)
	tmp, err := os.CreateTemp(outbox, ".partial-*")
	if err != nil {
		return res, err
	}
	defer os.Remove(tmp.Name()) // no-op after the rename

	h := sha256.New()
	cw := &countWriter{w: io.MultiWriter(tmp, h)}
	enc, err := age.Encrypt(cw, rs...)
	if err != nil {
		tmp.Close()
		return res, err
	}
	var stderr bytes.Buffer
	cmd := tools.cmd(ctx, tools.Dump, c, "", "--format=custom", "--no-owner", "--no-privileges")
	cmd.Stdout = enc
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		tmp.Close()
		return res, fmt.Errorf("pg_dump: %w: %s", err, tail(stderr.String()))
	}
	if err := enc.Close(); err != nil {
		tmp.Close()
		return res, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return res, err
	}
	if err := tmp.Close(); err != nil {
		return res, err
	}
	if cw.n < 1024 {
		return res, fmt.Errorf("pg_dump wrote only %d bytes", cw.n)
	}
	m := Manifest{File: name, CreatedAt: now.UTC(), Bytes: cw.n, SHA256: hex.EncodeToString(h.Sum(nil)), Counts: counts}
	mj, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(strings.TrimSuffix(final, suffix)+".json", mj, 0o640); err != nil {
		return res, err
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return res, err
	}
	res.Path, res.Manifest = final, m
	if keepDays > 0 {
		res.Pruned, err = prune(outbox, now.AddDate(0, 0, -keepDays), final)
	}
	return res, err
}

// Count returns the row count of every table in Tables that exists.
func Count(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (map[string]int64, error) {
	out := map[string]int64{}
	for _, t := range Tables {
		var exists bool
		if err := q.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+t).Scan(&exists); err != nil {
			return nil, fmt.Errorf("count %s: %w", t, err)
		}
		if !exists {
			continue
		}
		var n int64
		if err := q.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{t}.Sanitize()).Scan(&n); err != nil {
			return nil, fmt.Errorf("count %s: %w", t, err)
		}
		out[t] = n
	}
	return out, nil
}

// prune removes dumps (and manifests) last modified before cutoff, never keep.
func prune(dir string, cutoff time.Time, keep string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) || !strings.HasSuffix(e.Name(), suffix) {
			continue
		}
		p := filepath.Join(dir, e.Name())
		info, err := e.Info()
		if err != nil || p == keep || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(p); err != nil {
			return n, err
		}
		os.Remove(strings.TrimSuffix(p, suffix) + ".json")
		n++
	}
	return n, nil
}

// Latest is the newest dump in dir.
func Latest(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), prefix) && strings.HasSuffix(e.Name(), suffix) {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "", fmt.Errorf("no backups in %s", dir)
	}
	slices.Sort(names) // the timestamp in the name sorts
	return filepath.Join(dir, names[len(names)-1]), nil
}

// RestoreResult describes one restore test.
type RestoreResult struct {
	File     string
	Database string
	Counts   map[string]int64
	Short    []string // tables with fewer rows than the manifest
	Warnings string   // the tail of pg_restore's stderr when it exited non-zero
	Took     time.Duration
}

func (r RestoreResult) String() string {
	var total int64
	for _, n := range r.Counts {
		total += n
	}
	s := fmt.Sprintf("restored %s into %s: %d tables, %d rows in %s", filepath.Base(r.File), r.Database, len(r.Counts), total, r.Took.Round(time.Second))
	if len(r.Short) > 0 {
		s += "; SHORT: " + strings.Join(r.Short, ", ")
	}
	if r.Warnings != "" {
		s += "; pg_restore warnings: " + r.Warnings
	}
	return s
}

// Restore decrypts file with identities into a freshly created scratch
// database, checks the row counts against the manifest, and drops the scratch
// database unless keep is set. It fails when any table is short.
//
// TimescaleDB needs timescaledb_pre_restore()/post_restore() around the load and
// the same extension version on both sides. pg_restore commonly exits non-zero
// on hypertable catalog warnings that Timescale documents as harmless; those
// are reported, and the counts decide the result. Verify on the real image.
func Restore(ctx context.Context, admin *pgxpool.Pool, c Conn, tools Tools, identities, file, scratch string, keep bool) (RestoreResult, error) {
	start := time.Now()
	res := RestoreResult{File: file, Database: scratch}
	if !strings.HasPrefix(scratch, "pmt_restore") {
		return res, errors.New("scratch database name must start with pmt_restore")
	}
	ids, err := age.ParseIdentities(strings.NewReader(identities))
	if err != nil {
		return res, fmt.Errorf("age identity: %w", err)
	}
	var m Manifest
	mj, err := os.ReadFile(strings.TrimSuffix(file, suffix) + ".json")
	if err != nil {
		return res, fmt.Errorf("manifest: %w", err)
	}
	if err := json.Unmarshal(mj, &m); err != nil {
		return res, fmt.Errorf("manifest: %w", err)
	}
	if err := verifySHA(file, m.SHA256); err != nil {
		return res, err
	}

	ident := pgx.Identifier{scratch}.Sanitize()
	if _, err := admin.Exec(ctx, `DROP DATABASE IF EXISTS `+ident+` WITH (FORCE)`); err != nil {
		return res, fmt.Errorf("drop scratch: %w", err)
	}
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+ident); err != nil {
		return res, fmt.Errorf("create scratch: %w", err)
	}
	if !keep {
		defer admin.Exec(context.WithoutCancel(ctx), `DROP DATABASE IF EXISTS `+ident+` WITH (FORCE)`)
	}

	sc := c
	sc.Database = scratch
	db, err := pgx.Connect(ctx, sc.url())
	if err != nil {
		return res, fmt.Errorf("connect scratch: %w", err)
	}
	defer db.Close(context.WithoutCancel(ctx))
	if _, err := db.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS timescaledb`); err != nil {
		return res, fmt.Errorf("timescaledb: %w", err)
	}
	if _, err := db.Exec(ctx, `SELECT timescaledb_pre_restore()`); err != nil {
		return res, fmt.Errorf("pre_restore: %w", err)
	}

	f, err := os.Open(file)
	if err != nil {
		return res, err
	}
	defer f.Close()
	dec, err := age.Decrypt(bufio.NewReaderSize(f, 1<<20), ids...)
	if err != nil {
		return res, fmt.Errorf("decrypt: %w", err)
	}
	var stderr bytes.Buffer
	cmd := tools.cmd(ctx, tools.Restore, sc, scratch, "--no-owner", "--no-privileges")
	cmd.Stdin = dec
	cmd.Stderr = &stderr
	rerr := cmd.Run()
	if _, err := db.Exec(ctx, `SELECT timescaledb_post_restore()`); err != nil {
		return res, fmt.Errorf("post_restore: %w", err)
	}
	if rerr != nil {
		var ee *exec.ExitError
		if !errors.As(rerr, &ee) {
			return res, fmt.Errorf("pg_restore: %w", rerr)
		}
		res.Warnings = tail(stderr.String())
	}

	res.Counts, err = Count(ctx, db)
	if err != nil {
		return res, err
	}
	for _, t := range Tables {
		want, ok := m.Counts[t]
		if !ok {
			continue
		}
		if got, ok := res.Counts[t]; !ok || got < want {
			res.Short = append(res.Short, fmt.Sprintf("%s %d<%d", t, res.Counts[t], want))
		}
	}
	res.Took = time.Since(start)
	if len(res.Short) > 0 {
		return res, errors.New(res.String())
	}
	return res, nil
}

func (c Conn) url() string {
	u := url.URL{Scheme: "postgres", Host: c.Host + ":" + c.Port, Path: "/" + c.Database}
	if c.User != "" {
		u.User = url.UserPassword(c.User, c.Password)
	}
	if c.SSLMode != "" {
		u.RawQuery = "sslmode=" + url.QueryEscape(c.SSLMode)
	}
	return u.String()
}

func verifySHA(file, want string) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("%s: sha256 %s, manifest says %s", filepath.Base(file), got[:12], want[:min(12, len(want))])
	}
	return nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// tail keeps the last few lines of tool output for a job detail.
func tail(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	return strings.Join(lines, " | ")
}
