"""pmt-daily: runs the Python EOD jobs once a trading day, after the Go jobs they read.

The Go scheduler (jobs schedule) runs only Go jobs and its image has no Python, so this
small loop in the ivengine image runs pmt-iv-eod and pmt-vol from config's pyschedule:
each entry falls due at its time IST on a trading day, then waits until every job in
its `after` list has finished ok today (job_runs, written by the Go scheduler), and runs
once. A run is recorded in job_runs like a Go job's, so the SYS page shows it. An entry
that already has a run today, ok or not, is not run again: rerun it by hand. One still
waiting at `until` is recorded as an error and dropped for the day. State lives in
job_runs only, so a restart picks up where it left off."""

from __future__ import annotations

import argparse
import logging
import subprocess
import sys
import time
from dataclasses import dataclass, field
from datetime import date, datetime, timedelta

import yaml

from . import clock, config
from .calendar import Calendar
from .eodiv import database_url

log = logging.getLogger("pmt-daily")

# The commands a pyschedule entry may name; config picks times, not programs.
COMMANDS = {"iv-eod": ["pmt-iv-eod"], "vol": ["pmt-vol"]}
TICK = 30.0
DETAIL = 500


@dataclass
class Entry:
    at: str
    job: str
    after: list[str] = field(default_factory=list)
    until: str = "23:00"


def entries(p: str | None = None) -> list[Entry]:
    with open(p or config.path()) as f:
        raw = yaml.safe_load(config.expand(f.read())) or {}
    out = []
    for e in raw.get("pyschedule") or []:
        x = Entry(str(e["at"]), str(e["job"]), [str(a) for a in e.get("after") or []], str(e.get("until") or "23:00"))
        if x.job not in COMMANDS:
            raise ValueError(f"pyschedule: unknown job {x.job!r}")
        if clock.at(date.today(), x.until) <= clock.at(date.today(), x.at):
            raise ValueError(f"pyschedule {x.job}: until must be after at")
        out.append(x)
    if not out:
        raise ValueError("config has no pyschedule")
    return out


def real(t: datetime) -> datetime:
    """A clock instant as the wall-clock time job_runs holds (undoes a dry-run offset)."""
    return t - timedelta(seconds=clock.offset())


def step(e: Entry, now: datetime, trading: bool, tried: bool, ok_since: set[str]) -> str:
    """What to do with e now: wait, run, give-up or skip (done for today, or not a
    trading day). tried is whether e has a run since today's due time; ok_since holds
    the jobs that finished ok since the start of today."""
    d = now.date()
    if not trading or tried or now < clock.at(d, e.at):
        return "skip" if not trading or tried else "wait"
    if all(a in ok_since for a in e.after):
        return "run"
    return "give-up" if now >= clock.at(d, e.until) else "wait"


class Runs:
    """job_runs, as the Go store package writes it."""

    def __init__(self, conn):
        self.conn = conn

    def tried(self, job: str, since: datetime) -> bool:
        return self.conn.execute("SELECT 1 FROM job_runs WHERE job = %s AND started_at >= %s LIMIT 1",
                                 (job, since)).fetchone() is not None

    def ok_since(self, jobs: list[str], since: datetime) -> set[str]:
        rows = self.conn.execute("SELECT DISTINCT job FROM job_runs WHERE job = ANY(%s) AND status = 'ok' "
                                 "AND started_at >= %s", (jobs, since)).fetchall()
        return {r[0] for r in rows}

    def start(self, job: str) -> int:
        return self.conn.execute("INSERT INTO job_runs (job, args, started_at, status) VALUES (%s, '', now(), "
                                 "'running') RETURNING id", (job,)).fetchone()[0]

    def finish(self, rid: int, ok: bool, detail: str) -> None:
        self.conn.execute("UPDATE job_runs SET finished_at = now(), status = %s, detail = NULLIF(%s, '') WHERE id = %s",
                          ("ok" if ok else "error", detail[-DETAIL:], rid))


def run(runs: Runs, job: str) -> bool:
    rid = runs.start(job)
    t = time.monotonic()
    p = subprocess.run(COMMANDS[job], capture_output=True, text=True)
    sys.stdout.write(p.stdout)
    sys.stderr.write(p.stderr)
    # The detail is the last top-level line of the job's report, else of its log.
    lines = [ln for ln in p.stdout.splitlines() if ln.strip() and not ln[0].isspace()]
    lines = lines or [ln for ln in p.stderr.splitlines() if ln.strip()]
    detail = lines[-1].strip() if lines else ""
    if p.returncode:
        detail = f"exit {p.returncode}: {detail}"
    runs.finish(rid, p.returncode == 0, detail)
    log.info("job finished: %s ok=%s took %.1fs %s", job, p.returncode == 0, time.monotonic() - t, detail)
    return p.returncode == 0


def tick(runs: Runs, es: list[Entry], cal: Calendar, now: datetime, waiting: set[str]) -> None:
    d = now.date()
    trading = cal.session(d).trading
    since = real(clock.at(d, "00:00"))
    deps = sorted({a for e in es for a in e.after})
    ok = runs.ok_since(deps, since) if deps and trading else set()
    for e in es:
        tried = trading and runs.tried(e.job, real(clock.at(d, e.at)))
        what = step(e, now, trading, tried, ok)
        if what == "run":
            waiting.discard(e.job)
            run(runs, e.job)
        elif what == "give-up":
            missing = ",".join(a for a in e.after if a not in ok)
            runs.finish(runs.start(e.job), False, f"gave up at {e.until}: no ok run today of {missing}")
            log.error("%s: gave up, %s not done", e.job, missing)
        elif what == "wait" and now >= clock.at(d, e.at) and e.job not in waiting:
            waiting.add(e.job)
            log.info("%s: due, waiting for %s", e.job, ",".join(a for a in e.after if a not in ok))


def main(argv: list[str] | None = None) -> int:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s: %(message)s")
    ap = argparse.ArgumentParser(prog="pmt-daily", description="Run pmt-iv-eod and pmt-vol daily (pyschedule).")
    ap.add_argument("--once", action="store_true", help="check once and exit, instead of looping")
    a = ap.parse_args(argv)

    import psycopg

    cfg = config.load()
    es = entries()
    cal = Calendar.load(cfg.calendar)
    log.info("pyschedule: %s", ", ".join(f"{e.at} {e.job} after {'+'.join(e.after) or '-'}" for e in es))
    waiting: set[str] = set()
    while True:
        try:
            with psycopg.connect(database_url(), autocommit=True) as conn:
                tick(Runs(conn), es, cal, clock.now(), waiting)
        except psycopg.Error as err:
            log.error("database: %s", err)
        if a.once:
            return 0
        time.sleep(TICK)


if __name__ == "__main__":
    sys.exit(main())
