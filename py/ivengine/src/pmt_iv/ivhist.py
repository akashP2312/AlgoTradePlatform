"""Implied vol history: the constant-maturity curve, its rank, and the vol premium.

From iv_eod (the daily bhavcopy fits), per index and trade date:
  - ATM IV, rr25 and bf25 at fixed tenors (5, 21, 42 and 63 trading days), between the
    two good fits around each tenor: ATM linear in total variance (iv^2 * dte), the
    skew linear in dte. There is no extrapolation: a tenor no two expiries bracket has
    no row that day.
  - each one's 252-day rank and percentile against its own earlier days.
From those, har_eod and India VIX in index_eod, per trade date:
  - the vol premium of iv30 (the 21-day point, about 30 calendar days) and of VIX over
    the HAR forecast of realized vol (ex ante) and over the vol then realized (ex post);
  - the 252-day rank and percentile of iv30 and VIX.
A premium describes how implied vol has related to realized vol; it is not a signal.

Usage:
  pmt-ivhist                      recompute everything
  options: --und NIFTY,BANKNIFTY  --dry-run (compute and print, write nothing)

It reads iv_eod, har_eod and index_eod and writes ivcm_eod and vrp_eod only. Run it
after pmt-iv-eod and pmt-vol.
"""

from __future__ import annotations

import argparse
import logging
import math
import sys
from dataclasses import dataclass
from datetime import date

import numpy as np

from . import config
from .vol import HORIZON, VIX_CONTRACT, _f, _upsert

log = logging.getLogger("ivhist")

MODEL = "ivhist-1"
TENORS = (5, 21, 42, 63)
IV30 = 21
WINDOW = 252  # earlier days a rank looks back over
MIN_HIST = 60  # earlier days before a rank is reported
MIN_DTE = 2.0  # fits closer to expiry than this are left out: the last days are noisy


@dataclass
class Point:
    dte: float
    atm: float
    rr25: float | None
    bf25: float | None


def interp(pts: list[Point], tenor: float) -> Point | None:
    """The curve at tenor from the nearest fits on each side; None unless bracketed."""
    lo = max((p for p in pts if p.dte <= tenor), key=lambda p: p.dte, default=None)
    hi = min((p for p in pts if p.dte >= tenor), key=lambda p: p.dte, default=None)
    if lo is None or hi is None:
        return None
    if hi.dte == lo.dte:
        return Point(float(tenor), lo.atm, lo.rr25, lo.bf25)
    u = (tenor - lo.dte) / (hi.dte - lo.dte)
    w = lo.atm ** 2 * lo.dte + u * (hi.atm ** 2 * hi.dte - lo.atm ** 2 * lo.dte)
    if w <= 0:
        return None
    lin = lambda a, b: a + u * (b - a) if a is not None and b is not None else None
    return Point(float(tenor), math.sqrt(w / tenor), lin(lo.rr25, hi.rr25), lin(lo.bf25, hi.bf25))


@dataclass
class Rank:
    rank: float | None
    pct: float | None
    n: int


def ranks(x: list[float | None], window: int = WINDOW, min_hist: int = MIN_HIST) -> list[Rank]:
    """For each value, its rank in [0, 1] between the low and high of it and up to window
    earlier values, and the share of those earlier values below it. Missing values are
    skipped, not counted; None until min_hist earlier values exist."""
    out: list[Rank] = []
    seen: list[float] = []
    for v in x:
        if v is None or not math.isfinite(v):
            out.append(Rank(None, None, 0))
            continue
        prior = np.asarray(seen[-window:])
        n = len(prior)
        if n < min_hist:
            out.append(Rank(None, None, n))
        else:
            lo, hi = min(prior.min(), v), max(prior.max(), v)
            out.append(Rank((v - lo) / (hi - lo) if hi > lo else 0.5, float(np.mean(prior < v)), n))
        seen.append(v)
    return out


@dataclass
class Result:
    underlying: str
    cm_rows: list[dict]
    vrp_rows: list[dict]


def curve(fits: dict[date, list[Point]]) -> dict[int, dict[date, Point]]:
    """The constant-maturity points per tenor and date."""
    out: dict[int, dict[date, Point]] = {t: {} for t in TENORS}
    for d in sorted(fits):
        for t in TENORS:
            p = interp(fits[d], t)
            if p is not None:
                out[t][d] = p
    return out


def compute(und: str, fits: dict[date, list[Point]], har: list[tuple], vix: dict[date, float] | None) -> Result:
    """Every ivcm_eod and vrp_eod row for one index. har rows are (trade_date, fc,
    rv_fwd, rv21) in date order; vix is India VIX in points by date."""
    cm = curve(fits)
    cm_rows = []
    for t, by in cm.items():
        ds = sorted(by)
        ra = ranks([by[d].atm for d in ds])
        rr = ranks([by[d].rr25 for d in ds])
        bf = ranks([by[d].bf25 for d in ds])
        for i, d in enumerate(ds):
            p = by[d]
            cm_rows.append({"trade_date": d, "underlying": und, "tenor": t, "atm_iv": p.atm, "rr25": p.rr25,
                            "bf25": p.bf25, "atm_rank": ra[i].rank, "atm_pct": ra[i].pct, "rr25_pct": rr[i].pct,
                            "bf25_pct": bf[i].pct, "n_hist": ra[i].n, "model_version": MODEL})

    iv30 = cm[IV30]
    iv_ds = sorted(iv30)
    iv_rank = dict(zip(iv_ds, ranks([iv30[d].atm for d in iv_ds])))
    vix_rank: dict[date, Rank] = {}
    if vix:
        vds = sorted(vix)
        vix_rank = dict(zip(vds, ranks([vix[d] / 100.0 for d in vds])))
    sub = lambda a, b: a - b if a is not None and b is not None else None
    vsub = lambda a, b: a * a - b * b if a is not None and b is not None else None
    vrp_rows = []
    for d, fc, rv_fwd, rv21 in har:
        iv = iv30[d].atm if d in iv30 else None
        vx = vix[d] / 100.0 if vix and d in vix else None
        ir, vr = iv_rank.get(d, Rank(None, None, 0)), vix_rank.get(d, Rank(None, None, 0))
        vrp_rows.append({
            "trade_date": d, "underlying": und, "horizon": HORIZON, "iv30": iv, "vix": vx, "har_fc": fc,
            "rv21": rv21, "rv_fwd": rv_fwd, "iv_ante": sub(iv, fc), "iv_post": sub(iv, rv_fwd),
            "vix_ante": sub(vx, fc), "vix_post": sub(vx, rv_fwd), "var_vix_ante": vsub(vx, fc),
            "var_vix_post": vsub(vx, rv_fwd), "iv_rank": ir.rank, "iv_pct": ir.pct, "vix_rank": vr.rank,
            "vix_pct": vr.pct, "n_iv": ir.n, "n_vix": vr.n, "model_version": MODEL,
        })
    return Result(und, cm_rows, vrp_rows)


# ---- report -------------------------------------------------------------------


def _pc(x: float | None, w: int = 6) -> str:
    return f"{100 * x:{w}.2f}" if x is not None else " " * (w - 1) + "-"


def report(res: Result) -> list[str]:
    """Today's curve against 5 and 21 trading days ago, and the premium so far."""
    out = []
    by: dict[int, dict[date, dict]] = {}
    for r in res.cm_rows:
        by.setdefault(r["tenor"], {})[r["trade_date"]] = r
    days = sorted({d for t in by.values() for d in t})
    if days:
        today = days[-1]
        back = {k: days[-1 - k] if len(days) > k else None for k in (5, 21)}
        out.append(f"  curve {today} (vs {back[5]}, {back[21]})   atm    -1w    -1m  rank  pct   rr25  pct   bf25  pct")
        for t in TENORS:
            r = by.get(t, {}).get(today)
            if r is None:
                out.append(f"    {t:>3}d  -")
                continue
            ago = [by[t].get(back[k]) if back[k] else None for k in (5, 21)]
            out.append(f"    {t:>3}d  {_pc(r['atm_iv'])} {_pc(ago[0]['atm_iv'] if ago[0] else None)} "
                       f"{_pc(ago[1]['atm_iv'] if ago[1] else None)} {_pc(r['atm_rank'], 5)} {_pc(r['atm_pct'], 4)} "
                       f"{_pc(r['rr25'])} {_pc(r['rr25_pct'], 4)} {_pc(r['bf25'])} {_pc(r['bf25_pct'], 4)}"
                       f"   (n {r['n_hist']})")
    for k, name in (("iv", "iv30"), ("vix", "vix")):
        post = [r[f"{k}_post"] for r in res.vrp_rows if r[f"{k}_post"] is not None]
        ante = [r[f"{k}_ante"] for r in res.vrp_rows if r[f"{k}_ante"] is not None]
        if not ante:
            continue
        last = next(r for r in reversed(res.vrp_rows) if r[f"{k}_ante"] is not None)
        line = (f"  {name:<4} premium ex ante: mean {_pc(float(np.mean(ante)))} pts over {len(ante)} days, "
                f"today {_pc(last[f'{k}_ante'])} ({last['trade_date']}: {name} {_pc(last['iv30' if k == 'iv' else 'vix'])}"
                f" vs har {_pc(last['har_fc'])}, rank {_pc(last[f'{k}_rank'], 5)} pct {_pc(last[f'{k}_pct'], 4)})")
        out.append(line)
        if post:
            out.append(f"       ex post: mean {_pc(float(np.mean(post)))} pts, above realized on "
                       f"{100 * float(np.mean(np.asarray(post) > 0)):.0f}% of {len(post)} days")
    return out


# ---- the job ------------------------------------------------------------------

_CM = _upsert("ivcm_eod", ["trade_date", "underlying", "tenor", "atm_iv", "rr25", "bf25", "atm_rank", "atm_pct",
                           "rr25_pct", "bf25_pct", "n_hist", "model_version"], ("trade_date", "underlying", "tenor"))
_VRP = _upsert("vrp_eod", ["trade_date", "underlying", "horizon", "iv30", "vix", "har_fc", "rv21", "rv_fwd",
                           "iv_ante", "iv_post", "vix_ante", "vix_post", "var_vix_ante", "var_vix_post", "iv_rank",
                           "iv_pct", "vix_rank", "vix_pct", "n_iv", "n_vix", "model_version"],
               ("trade_date", "underlying", "horizon"))


def _fits(cur, und: str) -> dict[date, list[Point]]:
    cur.execute("""SELECT trade_date, dte, atm_iv, rr25, bf25 FROM iv_eod
                   WHERE underlying = %s AND source = 'bhav' AND ok AND atm_iv > 0 AND dte >= %s
                   ORDER BY trade_date, dte""", (und, MIN_DTE))
    out: dict[date, list[Point]] = {}
    for d, dte, atm, rr, bf in cur.fetchall():
        out.setdefault(d, []).append(Point(float(dte), float(atm), _f(rr) if rr is not None else None,
                                           _f(bf) if bf is not None else None))
    return out


def _har(cur, und: str) -> list[tuple]:
    cur.execute("SELECT trade_date, fc, rv_fwd, rv21 FROM har_eod WHERE underlying = %s AND horizon = %s "
                "ORDER BY trade_date", (und, HORIZON))
    return cur.fetchall()


def main(argv: list[str] | None = None) -> int:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s: %(message)s")
    ap = argparse.ArgumentParser(prog="pmt-ivhist", description="Constant-maturity IV, its rank, and the vol premium.")
    ap.add_argument("--und", help="comma-separated underlyings (default: the configured ones)")
    ap.add_argument("--dry-run", action="store_true")
    a = ap.parse_args(argv)

    import psycopg

    from .eodiv import database_url

    cfg = config.load()
    unds = a.und.split(",") if a.und else [u.name for u in cfg.underlyings]
    with psycopg.connect(database_url()) as conn, conn.cursor() as cur:
        cur.execute("SELECT trade_date, close FROM index_eod WHERE contract = %s AND close > 0", (VIX_CONTRACT,))
        vix = {d: float(c) for d, c in cur.fetchall()}
        for und in unds:
            fits, har = _fits(cur, und), _har(cur, und)
            if not fits and not har:
                print(f"ivhist: {und}: no iv_eod or har_eod rows; skipped")
                continue
            res = compute(und, fits, har, vix if und == cfg.vix_underlying else None)
            print(f"ivhist: {und}: {len(fits)} fit days, {len(res.cm_rows)} curve points, {len(res.vrp_rows)} premium rows")
            for line in report(res):
                print(line)
            if a.dry_run:
                continue
            cur.executemany(_CM, res.cm_rows)
            cur.executemany(_VRP, res.vrp_rows)
            conn.commit()
    if a.dry_run:
        print("ivhist: dry run, nothing written")
    return 0


if __name__ == "__main__":
    sys.exit(main())
