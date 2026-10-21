"""Daily implied vol history from the NSE F&O bhavcopy: for each trade date and
underlying, the bhavcopy prices go through the same engine as the live chain (parity
forward, Black-76 ivs, SVI fit, ATM and 25-delta skew), measured at that day's
close, and land in iv_eod with source 'bhav'.

A bhavcopy has one price per contract, not a bid and an ask, so each option enters
the engine as a zero-width quote at its settle price (the close when no settle).
Only options that traded that day (volume > 0), within max_kx of the future in
log-moneyness and worth at least min_premium go in: untraded strikes settle at
NSE's own theoretical price, and the cheapest wings are mostly tick size.

Usage:
  pmt-iv-eod                       every bhavcopy day without bhav rows yet
  pmt-iv-eod FROM [TO]             that range (YYYY-MM-DD), recomputing it
  options: --und NIFTY,BANKNIFTY   --dry-run (compute and print, write nothing)

It reads eod_contracts and writes iv_eod only.
"""

from __future__ import annotations

import argparse
import logging
import math
import os
import sys
from dataclasses import dataclass
from datetime import date

from . import clock, config
from .calendar import Calendar
from .engine import Book, Engine, Params

log = logging.getLogger("iveod")

MODEL = "bhav-settle-1"


@dataclass
class Filters:
    min_volume: int = 1
    max_kx: float = 0.20
    min_premium: float = 0.5


@dataclass
class Contract:
    expiry: date
    strike: float | None
    kind: str  # CE | PE | FUT
    price: float | None  # settle, else close
    volume: int | None
    und_price: float | None = None


def book_for(und: str, day: date, contracts: list[Contract], f: Filters) -> Book:
    """The engine's book for one underlying's bhavcopy on one day."""
    book = Book([und])
    futs: dict[date, float] = {}
    spot = None
    for c in contracts:
        if c.und_price and c.und_price > 0:
            spot = c.und_price
        if c.kind == "FUT" and c.price and c.price > 0:
            futs[c.expiry] = c.price
            book.update({"c": f"NFO|{und}|{c.expiry.isoformat()}||FUT", "b": c.price, "a": c.price, "p": c.price})
    if spot:
        book.update({"c": f"NSE|{und}|||IDX", "p": spot})
    # The moneyness filter centres on the expiry's own future, else the nearest one, else spot.
    near_fut = futs[min(futs)] if futs else None
    for c in contracts:
        if c.kind not in ("CE", "PE") or c.expiry <= day or not c.strike or not c.price:
            continue
        if c.price < f.min_premium or (c.volume or 0) < f.min_volume:
            continue
        ref = futs.get(c.expiry) or near_fut or spot
        if ref and abs(math.log(c.strike / ref)) > f.max_kx:
            continue
        book.update({"c": f"NFO|{und}|{c.expiry.isoformat()}|{c.strike}|{c.kind}", "b": c.price, "a": c.price, "p": c.price})
    return book


def compute(und: str, day: date, contracts: list[Contract], cal: Calendar, cfg: config.Config, f: Filters) -> list[dict]:
    """The iv_eod rows (as dicts) for one underlying on one day."""
    session = cal.session(day)
    at = session.close if session.close else clock.at(day, "15:30")
    book = book_for(und, day, contracts, f)
    # Zero-width quotes have no age and no spread to reject.
    eng = Engine(book, cal, Params(cfg.rate, 1.0, cfg.forward_strikes, cfg.vix_roll_days, max_age_ms=0),
                 cfg.weekend_weight, cfg.overnight_weight)
    out = []
    for d in book.expiries(und):
        res = eng.expiry(und, d, at)
        if res is None:
            continue
        p = res.payload
        fit = p.get("fit") or {}
        out.append({
            "trade_date": day, "underlying": und, "expiry": d, "source": "bhav", "ts": at,
            "tte": p["tte"], "dte": p["dte"], "fwd": p["f"], "fsrc": p["fsrc"], "spot": p["s"],
            "atm_iv": p["atm"], "rr25": p["rr25"], "bf25": p["bf25"],
            "fit_a": fit.get("a"), "fit_b": fit.get("b"), "fit_rho": fit.get("rho"), "fit_m": fit.get("m"),
            "fit_s": fit.get("s"), "rmse": fit.get("rmse"), "ok": bool(fit.get("ok")), "n_strikes": p["n"],
            "model_version": MODEL,
        })
    return out


# ---- database ---------------------------------------------------------------

_COLS = ["trade_date", "underlying", "expiry", "source", "ts", "tte", "dte", "fwd", "fsrc", "spot",
         "atm_iv", "rr25", "bf25", "fit_a", "fit_b", "fit_rho", "fit_m", "fit_s", "rmse", "ok",
         "n_strikes", "model_version"]
_KEY = ("trade_date", "underlying", "expiry", "source")
_UPSERT = (
    f"INSERT INTO iv_eod ({', '.join(_COLS)}) VALUES ({', '.join('%(' + c + ')s' for c in _COLS)}) "
    f"ON CONFLICT ({', '.join(_KEY)}) DO UPDATE SET "
    + ", ".join(f"{c} = excluded.{c}" for c in _COLS if c not in _KEY)
)


def _load(cur, und: str, day: date) -> list[Contract]:
    cur.execute(
        """SELECT expiry, strike, kind, COALESCE(NULLIF(settle, 0), close), volume, underlying_price
           FROM eod_contracts
           WHERE trade_date = %s AND underlying = %s AND kind IN ('CE', 'PE', 'FUT') AND expiry >= %s""",
        (day, und, day),
    )
    return [Contract(e, float(k) if k is not None else None, kd, float(p) if p is not None else None,
                     v, float(u) if u is not None else None)
            for e, k, kd, p, v, u in cur.fetchall()]


def _days(cur, frm: date | None, to: date | None, unds: list[str]) -> list[date]:
    if frm is not None:
        cur.execute("SELECT DISTINCT trade_date FROM eod_contracts WHERE trade_date BETWEEN %s AND %s ORDER BY 1",
                    (frm, to or frm))
    else:
        cur.execute(
            """SELECT DISTINCT e.trade_date FROM eod_contracts e
               WHERE e.underlying = ANY(%s) AND NOT EXISTS (
                 SELECT 1 FROM iv_eod i WHERE i.trade_date = e.trade_date AND i.underlying = e.underlying
                   AND i.source = 'bhav')
               ORDER BY 1""",
            (unds,),
        )
    return [r[0] for r in cur.fetchall()]


def database_url() -> str:
    url = os.environ.get("DATABASE_URL", "")
    if not url:
        raise SystemExit("iveod: DATABASE_URL is not set")
    return url


def main(argv: list[str] | None = None) -> int:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s: %(message)s")
    ap = argparse.ArgumentParser(prog="pmt-iv-eod", description="Daily IV history from the bhavcopy into iv_eod.")
    ap.add_argument("frm", nargs="?", type=date.fromisoformat)
    ap.add_argument("to", nargs="?", type=date.fromisoformat)
    ap.add_argument("--und", help="comma-separated underlyings (default: the configured ones)")
    ap.add_argument("--dry-run", action="store_true")
    a = ap.parse_args(argv)
    if a.to and a.frm and a.to < a.frm:
        ap.error("TO is before FROM")

    import psycopg

    url = database_url()
    cfg = config.load()
    cal = Calendar.load(cfg.calendar)
    unds = a.und.split(",") if a.und else [u.name for u in cfg.underlyings]
    f = Filters()
    total = days_done = 0
    empty: list[str] = []
    with psycopg.connect(url) as conn:
        with conn.cursor() as cur:
            days = _days(cur, a.frm, a.to, unds)
        for day in days:
            rows = []
            with conn.cursor() as cur:
                for und in unds:
                    got = compute(und, day, _load(cur, und, day), cal, cfg, f)
                    if not got:
                        empty.append(f"{day} {und}")
                    rows += got
                if a.dry_run:
                    for r in rows:
                        print(f"{r['trade_date']} {r['underlying']:<10} {r['expiry']} dte {r['dte']:7.2f} "
                              f"fwd {r['fwd']:10.2f} {r['fsrc']:<6} atm {r['atm_iv'] or 0:.4f} "
                              f"rr25 {r['rr25'] or 0:+.4f} bf25 {r['bf25'] or 0:+.4f} n {r['n_strikes']} ok {r['ok']}")
                elif rows:
                    cur.executemany(_UPSERT, rows)
            if not a.dry_run:
                conn.commit()
            total += len(rows)
            days_done += 1
    msg = f"iveod: {days_done} days, {total} rows" + (" (dry run, nothing written)" if a.dry_run else "")
    if empty:
        msg += f"; no rows for {len(empty)} day-underlyings: " + ", ".join(empty[:10]) + (" ..." if len(empty) > 10 else "")
    print(msg)
    return 0


if __name__ == "__main__":
    sys.exit(main())
