// Command jobs runs the batch jobs: reference data, the 15:25 sweep, gap checks,
// the backup merge, the harvest, the nightly exchange files and the database
// backup. Every run is recorded in job_runs, which the SYS page shows.
//
//	jobs <job> [flags] [args]     # run one job now
//	jobs schedule                 # run config's timetable until stopped
//	jobs offset 09:10             # print the clock offset that makes now 09:10 IST (dry runs)
//
// No job places, modifies or cancels orders: this terminal is read-only.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"pmt/internal/app"
	"pmt/internal/backup"
	"pmt/internal/backuprec"
	"pmt/internal/bus"
	"pmt/internal/calendar"
	"pmt/internal/clock"
	"pmt/internal/config"
	"pmt/internal/harvest"
	"pmt/internal/kite"
	"pmt/internal/loaders"
	"pmt/internal/notify"
	"pmt/internal/recorder"
	"pmt/internal/refdata"
	"pmt/internal/store"
	"pmt/internal/sweep"
)

// job is one subcommand. It returns the detail recorded in job_runs.
type job struct {
	help string
	run  func(e *env, args []string) (string, error)
}

var jobs = map[string]job{
	"migrate":      {"apply database migrations", migrate},
	"calendar":     {"seed sessions and expiry rules from the calendar files", seedCalendar},
	"instruments":  {"fetch, archive and load today's instrument dump", instruments},
	"constituents": {"refresh the index constituent CSVs (verify the niftyindices URLs)", constituents},
	"sweep":        {"snapshot every live chain over REST and publish it (15:25)", sweepJob},
	"gaps":         {"[date] check a day's snapshots and ticks for missing minutes", gaps},
	"merge-backup": {"[--all] [date] load the backup recorder's JSONL for a day", mergeBackup},
	"harvest":      {"[--force] [date] fetch the day's 1-minute candles", harvestJob},
	"backfill":     {"[--lookback N --window N] [end] fetch past candles still available", backfill},
	"bhav":         {"[date] load the NSE F&O bhavcopy", bhav},
	"partoi":       {"[date] load NSE participant-wise open interest", partOI},
	"vix":          {"[from [to]] load India VIX and index day closes", vix},
	"backup":       {"dump the database, encrypted with age, into the outbox", backupJob},
	"restore-test": {"[--keep] restore the latest dump into a scratch database and check counts", restoreTest},
}

// unrecorded jobs run before job_runs exists, or are not jobs at all.
var unrecorded = map[string]bool{"migrate": true}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: jobs <job> [args]   |   jobs schedule   |   jobs offset HH:MM[:SS]")
	names := make([]string, 0, len(jobs))
	for n := range jobs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(os.Stderr, "  %-13s %s\n", n, jobs[n].help)
	}
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	name, args := os.Args[1], os.Args[2:]
	if name == "offset" {
		// Printed for the Makefile's dry run; needs no config.
		if len(args) < 1 || len(args) > 2 {
			fmt.Fprintln(os.Stderr, "usage: jobs offset HH:MM[:SS] [YYYY-MM-DD]")
			os.Exit(2)
		}
		d, err := clock.OffsetTo(args[0], strings.Join(args[1:], ""))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(d.String())
		return
	}
	if _, ok := jobs[name]; !ok && name != "schedule" {
		usage()
		os.Exit(2)
	}

	ctx, cancel, cfg := app.Start("jobs")
	defer cancel()
	pool, err := app.DB(ctx, cfg)
	if err != nil {
		app.Fatal("database", err)
	}
	defer pool.Close()
	e := &env{ctx: ctx, cfg: cfg, pool: pool, note: notify.FromEnv("jobs")}
	defer e.close()

	if name == "schedule" {
		if err := schedule(e); err != nil && ctx.Err() == nil {
			app.Fatal("schedule", err)
		}
		return
	}
	detail, err := e.record(name, args)
	if err != nil {
		app.Fatal(name, err)
	}
	fmt.Println(detail)
}

// env holds what jobs share; the bus and calendar are opened on first use.
type env struct {
	ctx  context.Context
	cfg  *config.Config
	pool *pgxpool.Pool
	note *notify.Notifier

	mu   sync.Mutex
	conn *bus.Conn
	cal  *calendar.Calendar
}

func (e *env) close() {
	if e.conn != nil {
		e.conn.Close()
	}
}

func (e *env) bus() (*bus.Conn, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.conn == nil {
		c, err := app.Bus(e.ctx, e.cfg, "pmt-jobs")
		if err != nil {
			return nil, err
		}
		e.conn = c
	}
	return e.conn, nil
}

func (e *env) calendar() (*calendar.Calendar, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.cal == nil {
		c, err := app.Calendar(e.cfg)
		if err != nil {
			return nil, err
		}
		e.cal = c
	}
	return e.cal, nil
}

// record runs a job and writes it to job_runs. A failure also goes to Telegram,
// with the job name only.
func (e *env) record(name string, args []string) (string, error) {
	j := jobs[name]
	if unrecorded[name] {
		return j.run(e, args)
	}
	run, err := store.StartJob(e.ctx, e.pool, name, strings.Join(args, " "))
	if err != nil {
		return "", fmt.Errorf("job_runs: %w (run `jobs migrate` first?)", err)
	}
	start := time.Now()
	detail, err := j.run(e, args)
	slog.Info("job finished", "job", name, "took", time.Since(start).Round(time.Millisecond).String(), "detail", detail, "err", err)
	// Finish even when ctx was cancelled, so the row does not stay 'running'.
	fctx, cancel := context.WithTimeout(context.WithoutCancel(e.ctx), 10*time.Second)
	defer cancel()
	if ferr := run.Finish(fctx, detail, err); ferr != nil {
		slog.Warn("job_runs finish", "job", name, "err", ferr)
	}
	if err != nil {
		e.note.Send(fctx, notify.Warn, "job-failed", "job %s failed", name)
	}
	return detail, err
}

// session finds today's Kite session: NATS KV first, then the token file.
func (e *env) session() (bus.Session, error) {
	now := clock.Now()
	if c, err := e.bus(); err == nil {
		if kv, err := c.KV(e.ctx, bus.KVSession); err == nil {
			if s, err := kite.LoadSession(e.ctx, kv); err == nil && s.Valid(now) {
				return s, nil
			}
		}
	} else {
		slog.Warn("jobs: nats unavailable, trying the token file", "err", err)
	}
	if p := e.cfg.Abs(e.cfg.BackupRec.TokenFile); p != "" {
		if s, err := kite.ReadTokenFile(p); err == nil && s.Valid(now) {
			return s, nil
		}
	}
	return bus.Session{}, fmt.Errorf("%w: run `login` first", kite.ErrNoSession)
}

func (e *env) rest() (*kite.REST, error) {
	s, err := e.session()
	if err != nil {
		return nil, err
	}
	key := s.APIKey
	if key == "" {
		key = e.cfg.Kite.APIKey
	}
	return kite.NewREST(e.cfg.Kite, key, s.AccessToken), nil
}

func (e *env) fetcher() loaders.Fetcher {
	return loaders.Fetcher{UserAgent: e.cfg.Loaders.UserAgent}
}

// day parses an optional YYYY-MM-DD argument; the default is today (IST).
func day(args []string, i int) (time.Time, error) {
	if len(args) <= i {
		return clock.Today(), nil
	}
	t, err := time.ParseInLocation(time.DateOnly, args[i], clock.IST)
	if err != nil {
		return time.Time{}, fmt.Errorf("date %q: want YYYY-MM-DD", args[i])
	}
	return t, nil
}

// tradingDay is day, but rejects dates the calendar marks closed.
func (e *env) tradingDay(args []string, i int) (time.Time, *calendar.Calendar, error) {
	d, err := day(args, i)
	if err != nil {
		return d, nil, err
	}
	cal, err := e.calendar()
	if err != nil {
		return d, nil, err
	}
	if !cal.IsTradingDay(d) {
		return d, cal, errNotTrading
	}
	return d, cal, nil
}

var errNotTrading = errors.New("not a trading day")

func notTrading(d time.Time) string {
	return d.Format(time.DateOnly) + ": not a trading day, nothing to do"
}

func migrate(e *env, _ []string) (string, error) {
	applied, err := store.Migrate(e.ctx, e.pool)
	if err != nil {
		return "", err
	}
	if len(applied) == 0 {
		return "migrations: up to date", nil
	}
	return "migrations applied: " + strings.Join(applied, ", "), nil
}

func seedCalendar(e *env, _ []string) (string, error) {
	cal, err := e.calendar()
	if err != nil {
		return "", err
	}
	rules, err := app.Rules(e.cfg)
	if err != nil {
		return "", err
	}
	first, last := cal.Years()
	var years []int
	for y := first; y <= last; y++ {
		years = append(years, y)
	}
	n, err := store.SeedCalendar(e.ctx, e.pool, cal, rules, years...)
	if err != nil {
		return "", err
	}
	if !cal.Covers(clock.Today().AddDate(0, 3, 0)) {
		e.note.Send(e.ctx, notify.Warn, "calendar-short", "holiday calendar ends within 3 months")
	}
	return fmt.Sprintf("calendar %d-%d: %d sessions, %d rules", first, last, n, len(rules)), nil
}

func instruments(e *env, _ []string) (string, error) {
	// The dump may not need a session (verify); use one when there is one.
	r, err := e.rest()
	if err != nil {
		slog.Warn("instruments: no session, fetching without a token", "err", err)
		r = kite.NewREST(e.cfg.Kite, e.cfg.Kite.APIKey, "")
	}
	fetched := clock.Now()
	raw, err := r.FetchInstruments(e.ctx)
	if err != nil {
		return "", errors.New(kite.Describe(err))
	}
	today := clock.Date(fetched)
	path, sha, err := refdata.Archive(e.cfg.Abs(e.cfg.DataDir), today, raw)
	if err != nil {
		return "", fmt.Errorf("archive: %w", err)
	}
	rows, err := kite.ParseInstruments(raw)
	if err != nil {
		return "", err
	}
	st, err := refdata.Load(e.ctx, e.pool, today, rows, sha, path, fetched)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("instruments %s: %d rows, %d modelled, %d opened, %d closed, sha %.8s",
		today.Format(time.DateOnly), st.Rows, st.Modelled, st.Opened, st.Closed, st.SHA256), nil
}

// constituentFile maps our CSV name to the niftyindices list file. Verify both
// the host and these paths before relying on them.
func constituentURL(base, file string) string {
	name := strings.TrimSuffix(filepath.Base(file), ".csv")
	return strings.TrimRight(base, "/") + "/IndexConstituent/ind_" + name + "list.csv"
}

func constituents(e *env, _ []string) (string, error) {
	var out []string
	f := e.fetcher()
	for _, p := range e.cfg.Universe.Constituents {
		path := e.cfg.Abs(p)
		url := constituentURL(e.cfg.Loaders.NiftyIndices, path)
		data, err := f.Get(e.ctx, url)
		if err != nil {
			return strings.Join(out, "; "), fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		syms, err := refdata.ParseConstituents(strings.NewReader(string(data)))
		if err != nil || len(syms) < 5 {
			return strings.Join(out, "; "), fmt.Errorf("%s: not a constituent list (%d symbols, %v)", filepath.Base(path), len(syms), err)
		}
		old, _ := refdata.ReadConstituents(path)
		added, removed := diff(old, syms)
		if err := writeAtomic(path, data); err != nil {
			return strings.Join(out, "; "), err
		}
		out = append(out, fmt.Sprintf("%s: %d symbols (+%d -%d)", filepath.Base(path), len(syms), len(added), len(removed)))
	}
	return strings.Join(out, "; "), nil
}

func diff(old, cur []string) (added, removed []string) {
	for _, s := range cur {
		if !slices.Contains(old, s) {
			added = append(added, s)
		}
	}
	for _, s := range old {
		if !slices.Contains(cur, s) {
			removed = append(removed, s)
		}
	}
	return added, removed
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func sweepJob(e *env, _ []string) (string, error) {
	today := clock.Today()
	if cal, err := e.calendar(); err == nil && !cal.IsTradingDay(today) {
		return notTrading(today), nil
	}
	r, err := e.rest()
	if err != nil {
		return "", err
	}
	conn, err := e.bus()
	if err != nil {
		return "", err
	}
	insts, err := refdata.Current(e.ctx, e.pool, e.cfg.Sweep.Underlyings...)
	if err != nil {
		return "", err
	}
	targets, warns := sweep.All(e.cfg, insts, e.cfg.Sweep.Underlyings, today)
	for _, w := range warns {
		slog.Warn("sweep: " + w)
	}
	if len(targets) == 0 {
		return "", errors.New("no sweep targets: is today's instrument dump loaded?")
	}
	minute := clock.Minute(clock.Now())
	chains, st, err := sweep.Snap(e.ctx, r, targets, minute, "sweep")
	if err != nil && len(chains) == 0 {
		return st.String(), errors.New(kite.Describe(err))
	}
	n, perr := sweep.Publish(e.ctx, conn.JS, chains)
	detail := fmt.Sprintf("%s published=%d", st.String(), n)
	return detail, errors.Join(err, perr)
}

func gaps(e *env, args []string) (string, error) {
	d, cal, err := e.tradingDay(args, 0)
	if errors.Is(err, errNotTrading) {
		return notTrading(d), nil
	}
	if err != nil {
		return "", err
	}
	return e.gapReport(cal, d)
}

func (e *env) gapReport(cal *calendar.Calendar, d time.Time) (string, error) {
	rep, err := recorder.Gaps(e.ctx, e.pool, e.cfg, cal, d, clock.Now())
	if err != nil {
		return "", err
	}
	detail := fmt.Sprintf("gaps %s: %d series, %d gaps, %d minutes missing, %d open",
		rep.Date.Format(time.DateOnly), rep.Series, len(rep.Gaps), rep.Missing, rep.Open)
	if rep.Open > 0 {
		e.note.Send(e.ctx, notify.Warn, "gaps-open", "%s: %d gaps, %d minutes still missing", rep.Date.Format(time.DateOnly), len(rep.Gaps), rep.Open)
	}
	return detail, nil
}

func mergeBackup(e *env, args []string) (string, error) {
	fl := flag.NewFlagSet("merge-backup", flag.ContinueOnError)
	all := fl.Bool("all", false, "load every snapshot, not only minutes the main recorder missed")
	if err := fl.Parse(args); err != nil {
		return "", err
	}
	d, cal, err := e.tradingDay(fl.Args(), 0)
	if errors.Is(err, errNotTrading) {
		return notTrading(d), nil
	}
	if err != nil {
		return "", err
	}
	path := backuprec.Path(e.cfg.Abs(e.cfg.BackupRec.OutDir), d)
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		e.note.Send(e.ctx, notify.Warn, "backup-missing", "%s: no backup recorder file", d.Format(time.DateOnly))
		return "no backup recorder file for " + d.Format(time.DateOnly), nil
	}
	st, err := recorder.MergeBackup(e.ctx, e.pool, path, *all)
	detail := fmt.Sprintf("merge %s: lines=%d bad=%d chains=%d skipped=%d rows=%d",
		d.Format(time.DateOnly), st.Lines, st.Bad, st.Chains, st.Skipped, st.Rows)
	if err != nil {
		return detail, err
	}
	// Recheck so the gap rows show what the backup filled.
	for _, gd := range st.Dates {
		g, err := e.gapReport(cal, gd)
		if err != nil {
			return detail, err
		}
		detail += "; " + g
	}
	return detail, nil
}

// harvestTargets lists the day's targets, with the constituents of every list.
func (e *env) harvestTargets(d time.Time) ([]harvest.Target, error) {
	insts, err := refdata.Current(e.ctx, e.pool)
	if err != nil {
		return nil, err
	}
	var cons []string
	for _, p := range e.cfg.Universe.Constituents {
		syms, err := refdata.ReadConstituents(e.cfg.Abs(p))
		if err != nil {
			return nil, fmt.Errorf("constituents %s: %w", p, err)
		}
		for _, s := range syms {
			if !slices.Contains(cons, s) {
				cons = append(cons, s)
			}
		}
	}
	targets, warns := harvest.Targets(e.cfg.Universe, insts, cons, d)
	for _, w := range warns {
		slog.Warn("harvest: " + w)
	}
	if len(targets) == 0 {
		return nil, errors.New("no harvest targets: is the instrument dump loaded?")
	}
	return targets, nil
}

func (e *env) harvester(force bool) (*harvest.Harvester, error) {
	r, err := e.rest()
	if err != nil {
		return nil, err
	}
	cal, err := e.calendar()
	if err != nil {
		return nil, err
	}
	return &harvest.Harvester{Pool: e.pool, Hist: r, Cal: cal, Force: force}, nil
}

func harvestJob(e *env, args []string) (string, error) {
	fl := flag.NewFlagSet("harvest", flag.ContinueOnError)
	force := fl.Bool("force", false, "refetch contract-days already harvested")
	if err := fl.Parse(args); err != nil {
		return "", err
	}
	d, _, err := e.tradingDay(fl.Args(), 0)
	if errors.Is(err, errNotTrading) {
		return notTrading(d), nil
	}
	if err != nil {
		return "", err
	}
	h, err := e.harvester(*force)
	if err != nil {
		return "", err
	}
	targets, err := e.harvestTargets(d)
	if err != nil {
		return "", err
	}
	st, err := h.Day(e.ctx, targets, d)
	if st.Failed > 0 {
		e.note.Send(e.ctx, notify.Warn, "harvest-failed", "harvest %s: %d of %d contracts failed", d.Format(time.DateOnly), st.Failed, st.Targets)
	}
	return st.String(), err
}

func backfill(e *env, args []string) (string, error) {
	fl := flag.NewFlagSet("backfill", flag.ContinueOnError)
	lookback := fl.Int("lookback", e.cfg.Harvest.LookbackDays, "calendar days back from end")
	window := fl.Int("window", e.cfg.Harvest.WindowDays, "days per request window")
	force := fl.Bool("force", false, "refetch windows already covered")
	if err := fl.Parse(args); err != nil {
		return "", err
	}
	end, err := day(fl.Args(), 0)
	if err != nil {
		return "", err
	}
	h, err := e.harvester(*force)
	if err != nil {
		return "", err
	}
	end = h.Cal.OnOrBefore(end)
	targets, err := e.harvestTargets(end)
	if err != nil {
		return "", err
	}
	st, err := h.Backfill(e.ctx, targets, end, *lookback, *window)
	return st.String(), err
}

// saveRaw keeps the exchange file as fetched, under data/<kind>/yyyy/.
func (e *env) saveRaw(kind string, d time.Time, url string, raw []byte) {
	dir := filepath.Join(e.cfg.Abs(e.cfg.DataDir), kind, d.Format("2006"))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		slog.Warn("raw archive", "err", err)
		return
	}
	name := d.Format(time.DateOnly) + "-" + filepath.Base(url)
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0o640); err != nil {
		slog.Warn("raw archive", "err", err)
	}
}

func bhav(e *env, args []string) (string, error) {
	d, _, err := e.tradingDay(args, 0)
	if errors.Is(err, errNotTrading) {
		return notTrading(d), nil
	}
	if err != nil {
		return "", err
	}
	url := loaders.BhavURL(e.cfg.Loaders.NSEArchive, d)
	raw, err := e.fetcher().Get(e.ctx, url)
	if err != nil {
		return "", err
	}
	e.saveRaw("bhav", d, url, raw)
	data, err := loaders.Unzip(raw)
	if err != nil {
		return "", err
	}
	rows, err := loaders.ParseBhav(data)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", errors.New("bhavcopy has no F&O rows")
	}
	if !rows[0].TradeDate.Equal(time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)) {
		return "", fmt.Errorf("bhavcopy is for %s, not %s", rows[0].TradeDate.Format(time.DateOnly), d.Format(time.DateOnly))
	}
	n, err := loaders.SaveEOD(e.ctx, e.pool, rows)
	return fmt.Sprintf("bhav %s (%s): %d rows parsed, %d saved", d.Format(time.DateOnly), rows[0].Source, len(rows), n), err
}

func partOI(e *env, args []string) (string, error) {
	d, _, err := e.tradingDay(args, 0)
	if errors.Is(err, errNotTrading) {
		return notTrading(d), nil
	}
	if err != nil {
		return "", err
	}
	url := loaders.PartOIURL(e.cfg.Loaders.NSEArchive, d)
	raw, err := e.fetcher().Get(e.ctx, url)
	if err != nil {
		return "", err
	}
	e.saveRaw("partoi", d, url, raw)
	data, err := loaders.Unzip(raw)
	if err != nil {
		return "", err
	}
	rows, err := loaders.ParsePartOI(data, d)
	if err != nil {
		return "", err
	}
	if err := loaders.SavePartOI(e.ctx, e.pool, rows); err != nil {
		return "", err
	}
	return fmt.Sprintf("partoi %s: %d rows", d.Format(time.DateOnly), len(rows)), nil
}

func vix(e *env, args []string) (string, error) {
	from, err := day(args, 0)
	if err != nil {
		return "", err
	}
	to := from
	if len(args) > 1 {
		if to, err = day(args, 1); err != nil {
			return "", err
		}
	}
	r, err := e.rest()
	if err != nil {
		return "", err
	}
	insts, err := refdata.Current(e.ctx, e.pool)
	if err != nil {
		return "", err
	}
	targets, warns := loaders.IndexTargets(e.cfg, insts)
	for _, w := range warns {
		slog.Warn("vix: " + w)
	}
	if len(targets) == 0 {
		return "", errors.New("no index targets: is the instrument dump loaded?")
	}
	bars, err := loaders.IndexDays(e.ctx, r, targets, from, to)
	if err != nil {
		return "", errors.New(kite.Describe(err))
	}
	if err := loaders.SaveIndexEOD(e.ctx, e.pool, bars); err != nil {
		return "", err
	}
	return fmt.Sprintf("index eod %s..%s: %d indices, %d bars", from.Format(time.DateOnly), to.Format(time.DateOnly), len(targets), len(bars)), nil
}

func backupJob(e *env, _ []string) (string, error) {
	rec := os.Getenv(e.cfg.Backup.RecipientEnv)
	if rec == "" {
		return "", fmt.Errorf("%s is not set: it holds the age public key (age1...)", e.cfg.Backup.RecipientEnv)
	}
	c, err := backup.ParseConn(e.cfg.DatabaseURL)
	if err != nil {
		return "", err
	}
	res, err := backup.Run(e.ctx, e.pool, c, backup.DefaultTools, rec, e.cfg.Abs(e.cfg.Backup.Outbox), e.cfg.Backup.KeepLocalDays, time.Now())
	if err != nil {
		e.note.Send(e.ctx, notify.Crit, "backup-failed", "nightly database backup failed")
		return "", err
	}
	var rows int64
	for _, n := range res.Manifest.Counts {
		rows += n
	}
	return fmt.Sprintf("backup %s: %d MB, %d rows, pruned %d", filepath.Base(res.Path), res.Manifest.Bytes>>20, rows, res.Pruned), nil
}

// identityEnv names the file holding the age private key. It is needed only for
// the restore test and should not live on the main host otherwise.
const identityEnv = "PMT_BACKUP_AGE_IDENTITY_FILE"

func restoreTest(e *env, args []string) (string, error) {
	fl := flag.NewFlagSet("restore-test", flag.ContinueOnError)
	keep := fl.Bool("keep", false, "keep the scratch database for inspection")
	file := fl.String("file", "", "dump to restore (default: newest in the outbox)")
	if err := fl.Parse(args); err != nil {
		return "", err
	}
	idPath := os.Getenv(identityEnv)
	if idPath == "" {
		return "", fmt.Errorf("%s is not set: it names the age identity file", identityEnv)
	}
	ids, err := os.ReadFile(idPath)
	if err != nil {
		return "", err
	}
	path := *file
	if path == "" {
		if path, err = backup.Latest(e.cfg.Abs(e.cfg.Backup.Outbox)); err != nil {
			return "", err
		}
	}
	c, err := backup.ParseConn(e.cfg.DatabaseURL)
	if err != nil {
		return "", err
	}
	res, err := backup.Restore(e.ctx, e.pool, c, backup.DefaultTools, string(ids), path, "pmt_restore_test", *keep)
	if err != nil {
		return res.String(), err
	}
	return res.String(), nil
}

// schedule runs config's timetable until ctx ends. Times are IST on the clock,
// so a dry run's offset applies. A job still running when it next falls due is
// skipped. A time missed while the daemon was down is caught up only for entries
// marked catch_up; run anything else by hand.
func schedule(e *env) error {
	if len(e.cfg.Schedule) == 0 {
		return errors.New("config has no schedule")
	}
	for _, s := range e.cfg.Schedule {
		if _, ok := jobs[s.Job]; !ok {
			return fmt.Errorf("schedule: unknown job %q", s.Job)
		}
		if _, _, _, err := clock.ParseHMS(s.At); err != nil {
			return fmt.Errorf("schedule %s: %w", s.Job, err)
		}
	}
	var (
		mu      sync.Mutex
		running = map[string]bool{}
		wg      sync.WaitGroup
	)
	defer wg.Wait()
	slog.Info("schedule started", "entries", len(e.cfg.Schedule))
	after := clock.Now()
	catchUp(e, after)
	for {
		at, due := next(e.cfg.Schedule, after)
		slog.Info("schedule: next", "at", at.Format(time.DateTime), "jobs", len(due))
		if !clock.SleepUntil(e.ctx.Done(), at) {
			return e.ctx.Err()
		}
		after = at
		trading := true
		if cal, err := e.calendar(); err == nil {
			trading = cal.IsTradingDay(clock.Date(at))
		} else {
			slog.Error("schedule: calendar", "err", err)
		}
		for _, s := range due {
			if s.TradingDayOnly && !trading {
				slog.Info("schedule: skipped, not a trading day", "job", s.Job)
				continue
			}
			key := s.Job + " " + s.Args
			mu.Lock()
			busy := running[key]
			running[key] = true
			mu.Unlock()
			if busy {
				slog.Warn("schedule: still running, skipped", "job", s.Job)
				continue
			}
			wg.Add(1)
			go func(s config.Schedule) {
				defer wg.Done()
				defer func() { mu.Lock(); delete(running, key); mu.Unlock() }()
				if _, err := e.record(s.Job, strings.Fields(s.Args)); err != nil {
					slog.Error("schedule: job failed", "job", s.Job, "err", err)
				}
			}(s)
		}
	}
}

// catchUp runs, in timetable order, each catch_up entry whose time today is at or
// before now and that has not finished ok since then. job_runs holds real
// timestamps, so the comparison undoes any dry-run offset.
func catchUp(e *env, now time.Time) {
	for _, s := range e.cfg.Schedule {
		if !s.CatchUp {
			continue
		}
		at := clock.MustAtHMS(clock.Date(now), s.At)
		if at.After(now) {
			continue
		}
		last, err := store.LastOK(e.ctx, e.pool, s.Job)
		if err != nil {
			slog.Error("schedule: catch-up check", "job", s.Job, "err", err)
			continue
		}
		if !last.Before(clock.Real(at)) {
			continue
		}
		slog.Info("schedule: catching up", "job", s.Job, "due", at.Format(time.DateTime))
		if _, err := e.record(s.Job, strings.Fields(s.Args)); err != nil {
			slog.Error("schedule: catch-up failed", "job", s.Job, "err", err)
		}
	}
}

// next is the first time strictly after t at which entries fall due, and those
// entries.
func next(entries []config.Schedule, t time.Time) (time.Time, []config.Schedule) {
	var best time.Time
	var due []config.Schedule
	d := clock.Date(t)
	for _, s := range entries {
		at := clock.MustAtHMS(d, s.At)
		if !at.After(t) {
			at = clock.MustAtHMS(d.AddDate(0, 0, 1), s.At)
		}
		switch {
		case best.IsZero() || at.Before(best):
			best, due = at, []config.Schedule{s}
		case at.Equal(best):
			due = append(due, s)
		}
	}
	return best, due
}
