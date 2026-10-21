"""Event premium: the move the option market prices to each expiry, against past moves.

From iv_eod (the daily bhavcopy fits) and index_eod, per index, trade date and expiry:
  - the implied move to expiry, the straddle-equivalent atm_iv * sqrt(tte) * sqrt(2/pi)
    (the expected absolute move if returns were normal at that vol);
  - the forward vol of the segment between the previous expiry and this one, and the
    kink: the segment's total variance above the curve's typical forward rate (the
    median across that day's expiries), as an event move;
  - the mean absolute move over the same number of trading days in the two years
    before, and the move that then happened to the expiry's close;
  - the scheduled events (config/calendar/events.csv, beside holidays.csv) that fall in
    the segment. That file starts empty: only verified dates belong in it.
The report puts today's curve of implied moves beside the past moves, and how implied
moves have compared with the realized ones by days to expiry. It describes what was
priced and what happened; it is not a signal.

Usage:
  pmt-event                       recompute everything
  options: --und NIFTY,BANKNIFTY  --dry-run (compute and print, write nothing)

It reads iv_eod and index_eod and writes event_eod only. Run it after pmt-iv-eod.
"""

from __future__ import annotations

import argparse
import bisect
import csv
import logging
import math
import os
import sys
from dataclasses import dataclass
from datetime import date

import numpy as np

from . import config
from .vol import _upsert, contract

log = logging.getLogger("event")

MODEL = "event-1"
SQ2PI = math.sqrt(2 / math.pi)
HIST = 504  # trading days of past moves behind hist_move
MIN_DTE = 1.0  # a fit on expiry day itself has no move left to price
HIST_DTE = 63  # hist_move only up to this many days: longer windows overlap too much to mean much
MIN_SEGS = 3  # expiries a day needs before its typical forward rate means anything
BUCKETS = ((1, 2), (3, 5), (6, 10), (11, 21), (22, 63))


@dataclass
class Fit:
    expiry: date
    dte: float
    tte: float
    atm: float


def kinks(fits: list[Fit]) -> list[tuple[float | None, float | None]]:
    """(segment forward vol, event variance) per fit, in tte order. A segment whose
    total variance falls (a calendar inversion) has no forward vol and no event; the
    event variance is None when the day has fewer than MIN_SEGS expiries."""
    out, rates = [], []
    w0, t0 = 0.0, 0.0
    for f in fits:
        w = f.atm ** 2 * f.tte
        dt = f.tte - t0
        r = (w - w0) / dt if dt > 0 else None
        rates.append(r if r is not None and r > 0 else None)
        w0, t0 = w, f.tte
    good = [r for r in rates if r is not None]
    base = float(np.median(good)) if len(good) >= MIN_SEGS else None
    t0 = 0.0
    for f, r in zip(fits, rates):
        ev = None if base is None or r is None else max(0.0, r - base) * (f.tte - t0)
        out.append((math.sqrt(r) if r is not None else None, ev))
        t0 = f.tte
    return out


class Moves:
    """Absolute close-to-close log moves over n trading days, by date."""

    def __init__(self, dates: list[date], closes: list[float]):
        self.dates = dates
        self.lc = np.log(np.asarray(closes, dtype=float))
        self._cum: dict[int, np.ndarray] = {}

    def _at(self, d: date) -> int:
        """The index of the last close on or before d, or -1."""
        return bisect.bisect_right(self.dates, d) - 1

    def hist(self, d: date, n: int, back: int = HIST) -> tuple[float | None, int]:
        """The mean |move over n days| over the windows that end on or before d and
        start no more than back days before it."""
        i = self._at(d)
        if n < 1 or i < 0:
            return None, 0
        if n not in self._cum:
            m = np.abs(self.lc[n:] - self.lc[:-n]) if len(self.lc) > n else np.zeros(0)
            self._cum[n] = np.concatenate([[0.0], np.cumsum(m)])
        cum = self._cum[n]
        # window k (0-based) ends at index k + n; keep those ending in [i - back + n, i]
        hi = i - n + 1
        lo = max(0, i - back)
        if hi <= lo:
            return None, 0
        return float((cum[hi] - cum[lo]) / (hi - lo)), hi - lo

    def realized(self, d: date, expiry: date) -> float | None:
        """|log move| from d's close to the expiry date's close; None until a close on
        or after expiry exists."""
        i, j = self._at(d), self._at(expiry)
        if i < 0 or self.dates[i] != d or not self.dates or self.dates[-1] < expiry or j < i:
            return None
        return float(abs(self.lc[j] - self.lc[i]))


def load_events(path: str, und: str) -> list[tuple[date, str]]:
    if not os.path.exists(path):
        return []
    with open(path) as f:
        rows = csv.DictReader(l for l in f if l.strip() and not l.lstrip().startswith("#"))
        return sorted((date.fromisoformat(r["date"].strip()), r["name"].strip()) for r in rows
                      if r.get("underlying", "*").strip() in ("*", und))


def compute(und: str, fits: dict[date, list[Fit]], moves: Moves, events: list[tuple[date, str]]) -> list[dict]:
    rows = []
    for d in sorted(fits):
        fs = sorted(fits[d], key=lambda f: f.tte)
        prev = d
        for f, (sv, ev) in zip(fs, kinks(fs)):
            hm, hn = moves.hist(d, int(round(f.dte))) if f.dte <= HIST_DTE else (None, 0)
            names = [n for ed, n in events if prev < ed <= f.expiry]
            rows.append({"trade_date": d, "underlying": und, "expiry": f.expiry, "dte": f.dte, "atm_iv": f.atm,
                         "implied_move": f.atm * math.sqrt(f.tte) * SQ2PI, "seg_vol": sv, "event_var": ev,
                         "event_move": math.sqrt(ev) * SQ2PI if ev is not None else None, "hist_move": hm,
                         "hist_n": hn, "real_move": moves.realized(d, f.expiry),
                         "events": "; ".join(names) or None, "model_version": MODEL})
            prev = f.expiry
    return rows


# ---- report -------------------------------------------------------------------


def _pc(x: float | None, w: int = 6) -> str:
    return f"{100 * x:{w}.2f}" if x is not None else " " * (w - 1) + "-"


def report(rows: list[dict], n_exp: int = 8) -> list[str]:
    out = []
    if not rows:
        return out
    today = max(r["trade_date"] for r in rows)
    cur = [r for r in rows if r["trade_date"] == today][:n_exp]
    out.append(f"  moves priced on {today}   dte    iv  implied  past  ratio  segvol  event")
    for r in cur:
        ratio = f"{r['implied_move'] / r['hist_move']:6.2f}" if r["hist_move"] else "     -"
        out.append(f"    {r['expiry']}  {r['dte']:5.0f} {_pc(r['atm_iv'])} {_pc(r['implied_move'], 7)} "
                   f"{_pc(r['hist_move'], 5)} {ratio}  {_pc(r['seg_vol'])} {_pc(r['event_move'])}"
                   + (f"  [{r['events']}]" if r["events"] else ""))
    out.append("  priced vs realized, by dte     n  implied  realized  ratio  realized>implied")
    for lo, hi in BUCKETS:
        b = [r for r in rows if lo <= r["dte"] <= hi and r["real_move"] is not None]
        if not b:
            continue
        im = float(np.mean([r["implied_move"] for r in b]))
        rm = float(np.mean([r["real_move"] for r in b]))
        share = float(np.mean([r["real_move"] > r["implied_move"] for r in b]))
        out.append(f"    {lo:>2}-{hi:<2} days         {len(b):>5}  {_pc(im, 7)}  {_pc(rm, 8)}  {im / rm:5.2f}"
                   f"  {100 * share:5.0f}%")
    return out


# ---- the job ------------------------------------------------------------------

_EV = _upsert("event_eod", ["trade_date", "underlying", "expiry", "dte", "atm_iv", "implied_move", "seg_vol",
                            "event_var", "event_move", "hist_move", "hist_n", "real_move", "events", "model_version"],
              ("trade_date", "underlying", "expiry"))


def _fits(cur, und: str) -> dict[date, list[Fit]]:
    cur.execute("""SELECT trade_date, expiry, dte, tte, atm_iv FROM iv_eod
                   WHERE underlying = %s AND source = 'bhav' AND ok AND atm_iv > 0 AND tte > 0 AND dte >= %s
                   ORDER BY trade_date, tte""", (und, MIN_DTE))
    out: dict[date, list[Fit]] = {}
    for d, e, dte, tte, atm in cur.fetchall():
        out.setdefault(d, []).append(Fit(e, float(dte), float(tte), float(atm)))
    return out


def main(argv: list[str] | None = None) -> int:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s: %(message)s")
    ap = argparse.ArgumentParser(prog="pmt-event", description="The move priced to each expiry, against past moves.")
    ap.add_argument("--und", help="comma-separated underlyings (default: the configured ones)")
    ap.add_argument("--dry-run", action="store_true")
    a = ap.parse_args(argv)

    import psycopg

    from .eodiv import database_url

    cfg = config.load()
    unds = a.und.split(",") if a.und else [u.name for u in cfg.underlyings]
    ev_path = os.path.join(os.path.dirname(cfg.calendar), "events.csv")
    with psycopg.connect(database_url()) as conn, conn.cursor() as cur:
        for und in unds:
            fits = _fits(cur, und)
            cur.execute("SELECT trade_date, close FROM index_eod WHERE contract = %s AND close > 0 ORDER BY trade_date",
                        (contract(und),))
            bars = cur.fetchall()
            if not fits or not bars:
                print(f"event: {und}: no iv_eod or index_eod rows; skipped")
                continue
            events = load_events(ev_path, und)
            rows = compute(und, fits, Moves([b[0] for b in bars], [float(b[1]) for b in bars]), events)
            print(f"event: {und}: {len(fits)} fit days, {len(rows)} expiry rows, "
                  f"{sum(r['real_move'] is not None for r in rows)} with the move known, {len(events)} calendar events")
            for line in report(rows):
                print(line)
            if a.dry_run:
                continue
            cur.executemany(_EV, rows)
            conn.commit()
    if a.dry_run:
        print("event: dry run, nothing written")
    return 0


if __name__ == "__main__":
    sys.exit(main())
