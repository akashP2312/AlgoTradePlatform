"""Realized vol and a HAR-RV forecast from the daily index bars in index_eod.

Realized vol, per index and trailing window (5, 10, 21, 63 trading days), four ways:
  cc  close-to-close, zero mean
  pk  Parkinson, from the high-low range
  gk  Garman-Klass, from open, high, low and close
  yz  Yang-Zhang: overnight, open-to-close and Rogers-Satchell, combined
pk and gk see the session only; cc and yz include the overnight gap, which on an
Indian index is a large share of the variance.

The forecast is a log-HAR (Corsi 2009) of close-to-close variance over the next 21
trading days, on the daily, weekly (5) and monthly (22) means of a daily range
variance (overnight gap squared plus the Garman-Klass session term). It is walked
forward: each day's fit uses only targets already realized by that day's close, so
every stored forecast is out of sample. The log fit is turned back into variance with
Duan's smearing factor. The 10/90% bands come from the log errors of earlier
out-of-sample forecasts (from the in-sample residuals until there are enough).

The scorecard compares the forecast with two benchmarks over the same days: the
trailing 21-day close-to-close vol (tomorrow looks like the last month) and India VIX
(the market's 30-day implied vol; NIFTY only). QLIKE and MSE are on variance; VIX
carries the variance premium, so it is expected to sit above what is realized.

Usage:
  pmt-vol                         recompute everything from index_eod
  options: --und NIFTY,BANKNIFTY  --dry-run (compute and print, write nothing)

It reads index_eod and writes rv_eod, har_eod and vol_score only.
"""

from __future__ import annotations

import argparse
import logging
import math
import sys
from dataclasses import dataclass, field
from datetime import date

import numpy as np

from . import config

log = logging.getLogger("vol")

MODEL = "vol-1"
YEAR = 252
WINDOWS = (5, 10, 21, 63)
HORIZON = 21
HAR_LAGS = (1, 5, 22)
MIN_TRAIN = 500  # targets realized before the first forecast
MIN_OOS = 250  # out-of-sample errors before the bands use them
BAND = (0.10, 0.90)
VIX_CONTRACT = "NSE|INDIAVIX|||IDX"
_K_PK = 1.0 / (4.0 * math.log(2.0))
_K_GK = 2.0 * math.log(2.0) - 1.0


@dataclass
class Bars:
    dates: list[date]
    o: np.ndarray
    h: np.ndarray
    l: np.ndarray
    c: np.ndarray

    @classmethod
    def of(cls, rows: list[tuple]) -> "Bars":
        """From (date, open, high, low, close) rows in date order."""
        a = np.array([r[1:] for r in rows], float).reshape(-1, 4)
        return cls([r[0] for r in rows], a[:, 0], a[:, 1], a[:, 2], a[:, 3])

    def __len__(self) -> int:
        return len(self.dates)


@dataclass
class Parts:
    """Per-day log pieces; index 0 has no previous close, so r and ro are nan there."""
    r: np.ndarray  # close to close
    ro: np.ndarray  # previous close to open
    rc: np.ndarray  # open to close
    hl: np.ndarray  # high to low
    rs: np.ndarray  # Rogers-Satchell term


def parts(b: Bars) -> Parts:
    prev = np.concatenate([[np.nan], b.c[:-1]])
    lh, ll, lo, lc = np.log(b.h), np.log(b.l), np.log(b.o), np.log(b.c)
    return Parts(
        r=np.log(b.c / prev),
        ro=np.log(b.o / prev),
        rc=lc - lo,
        hl=lh - ll,
        rs=(lh - lc) * (lh - lo) + (ll - lc) * (ll - lo),
    )


def _roll(x: np.ndarray, n: int) -> np.ndarray:
    """Windows of n ending at each index, nan-padded at the start: shape (len, n)."""
    out = np.full((len(x), n), np.nan)
    if len(x) >= n:
        out[n - 1:] = np.lib.stride_tricks.sliding_window_view(x, n)
    return out


def realized(b: Bars, n: int) -> dict[str, np.ndarray]:
    """Annualized cc, pk, gk and yz vol over the n days ending at each date; nan until
    there are n returns (the first day has none)."""
    p = parts(b)
    r2 = _roll(p.r ** 2, n)
    hl2 = _roll(p.hl ** 2, n)
    rc = _roll(p.rc, n)
    ro = _roll(p.ro, n)
    rs = _roll(p.rs, n)
    with np.errstate(invalid="ignore"):
        cc = np.sqrt(YEAR * r2.mean(axis=1))
        pk = np.sqrt(YEAR * _K_PK * hl2.mean(axis=1))
        gk = np.sqrt(YEAR * (0.5 * hl2 - _K_GK * rc ** 2).mean(axis=1))
        if n > 1:
            k = 0.34 / (1.34 + (n + 1) / (n - 1))
            var = ro.var(axis=1, ddof=1) + k * rc.var(axis=1, ddof=1) + (1 - k) * rs.mean(axis=1)
            yz = np.sqrt(np.maximum(YEAR * var, 0.0))
        else:
            yz = np.full(len(b), np.nan)
    # Windows that reach back to day 0 have no return there; the mean above is nan.
    return {"cc": cc, "pk": pk, "gk": gk, "yz": yz}


def daily_var(p: Parts) -> np.ndarray:
    """The HAR's daily variance: overnight gap squared plus the Garman-Klass session term."""
    return p.ro ** 2 + 0.5 * p.hl ** 2 - _K_GK * p.rc ** 2


def forward_var(p: Parts, h: int) -> np.ndarray:
    """Mean squared close-to-close return over the h days after each date; nan where
    those days are not in yet."""
    # r2[j] is day j+1's squared return; the window ending at t+h-1 covers t+1..t+h.
    r2 = np.concatenate([p.r[1:] ** 2, np.full(h, np.nan)])
    return _roll(r2, h).mean(axis=1)[h - 1: h - 1 + len(p.r)]


def har_features(v: np.ndarray) -> np.ndarray:
    """log of the 1, 5 and 22 day mean daily variance ending at each date, with a
    leading column of ones; rows without a full 22 days are nan."""
    floor = 1e-10  # a flat day must not send the log to -inf
    cols = [np.ones(len(v))]
    for n in HAR_LAGS:
        cols.append(np.log(np.maximum(_roll(v, n).mean(axis=1), floor)))
    return np.column_stack(cols)


@dataclass
class Forecast:
    i: int  # index into the bars
    fc: float  # annualized vol
    lo: float
    hi: float
    beta: np.ndarray
    n_train: int


@dataclass
class Har:
    h: int = HORIZON
    min_train: int = MIN_TRAIN
    min_oos: int = MIN_OOS
    band: tuple[float, float] = BAND
    out: list[Forecast] = field(default_factory=list)


def walk_forward(b: Bars, m: Har | None = None) -> list[Forecast]:
    """One out-of-sample HAR forecast per date once min_train targets are realized.

    At date t a target y_s (the variance over s+1..s+h) is known only when s+h <= t,
    so the fit for t uses those rows and nothing later."""
    m = m or Har()
    p = parts(b)
    X = har_features(daily_var(p))
    y = forward_var(p, m.h)
    with np.errstate(divide="ignore", invalid="ignore"):
        ly = np.log(y)
    good = np.isfinite(X).all(axis=1) & np.isfinite(ly)
    feat_ok = np.isfinite(X).all(axis=1)
    out: list[Forecast] = []
    oos: list[tuple[int, float]] = []  # (date index, log error of variance)
    for t in range(len(b)):
        if not feat_ok[t]:
            continue
        train = np.flatnonzero(good[: max(t - m.h + 1, 0)])
        if len(train) < m.min_train:
            continue
        beta, *_ = np.linalg.lstsq(X[train], ly[train], rcond=None)
        resid = ly[train] - X[train] @ beta
        smear = float(np.mean(np.exp(resid)))
        var = math.exp(float(X[t] @ beta)) * smear
        # Errors of earlier forecasts whose targets are realized by t.
        known = [e for (i, e) in oos if i + m.h <= t]
        errs = np.asarray(known) if len(known) >= m.min_oos else resid - math.log(smear)
        qlo, qhi = np.quantile(errs, m.band)
        fc = math.sqrt(YEAR * var)
        out.append(Forecast(t, fc, fc * math.exp(qlo / 2), fc * math.exp(qhi / 2), beta, len(train)))
        if good[t]:
            oos.append((t, float(ly[t] - math.log(var))))
    return out


# ---- scorecard ---------------------------------------------------------------


def qlike(real_var: np.ndarray, fc_var: np.ndarray) -> float:
    """Patton's QLIKE, 0 for a perfect forecast; punishes forecasts that are too low
    more than ones that are too high."""
    x = real_var / fc_var
    return float(np.mean(x - np.log(x) - 1.0))


def mse(real_var: np.ndarray, fc_var: np.ndarray) -> float:
    return float(np.mean((real_var - fc_var) ** 2))


@dataclass
class Score:
    model: str
    n: int
    frm: date
    to: date
    qlike: float
    mse: float
    bias: float
    hit: float | None


def scorecard(dates: list[date], real: np.ndarray, models: dict[str, np.ndarray],
              lo: np.ndarray | None = None, hi: np.ndarray | None = None) -> list[Score]:
    """Each model's vol forecasts against real (annualized vols, nan where missing),
    over the days where real and every model are present, so the rows compare."""
    mask = np.isfinite(real)
    for v in models.values():
        mask &= np.isfinite(v) & (v > 0)
    idx = np.flatnonzero(mask)
    if len(idx) == 0:
        return []
    rv = real[idx] ** 2
    out = []
    for name, v in models.items():
        fv = v[idx] ** 2
        hit = None
        if name == "har" and lo is not None and hi is not None:
            hit = float(np.mean((real[idx] >= lo[idx]) & (real[idx] <= hi[idx])))
        out.append(Score(name, len(idx), dates[idx[0]], dates[idx[-1]], qlike(rv, fv), mse(rv, fv),
                         float(np.mean(v[idx]) - np.mean(real[idx])), hit))
    return out


# ---- the job ------------------------------------------------------------------


@dataclass
class Result:
    underlying: str
    rv_rows: list[dict]
    har_rows: list[dict]
    scores: list[Score]


def compute(und: str, b: Bars, vix: dict[date, float] | None) -> Result:
    """Every rv_eod, har_eod and vol_score row for one index."""
    rv_rows = []
    rvs = {n: realized(b, n) for n in WINDOWS}
    for i, d in enumerate(b.dates):
        for n in WINDOWS:
            row = {k: _f(rvs[n][k][i]) for k in ("cc", "pk", "gk", "yz")}
            if row["cc"] is None:
                continue
            rv_rows.append({"trade_date": d, "underlying": und, "win": n, **row, "model_version": MODEL})

    p = parts(b)
    real = np.sqrt(YEAR * forward_var(p, HORIZON))
    rv21 = rvs[21]["cc"] if 21 in rvs else realized(b, 21)["cc"]
    vx = np.array([vix.get(d, np.nan) / 100.0 for d in b.dates]) if vix else None
    fcs = walk_forward(b)
    n = len(b)
    fc = np.full(n, np.nan)
    lo = np.full(n, np.nan)
    hi = np.full(n, np.nan)
    har_rows = []
    for f in fcs:
        fc[f.i], lo[f.i], hi[f.i] = f.fc, f.lo, f.hi
        har_rows.append({
            "trade_date": b.dates[f.i], "underlying": und, "horizon": HORIZON, "fc": f.fc, "lo": f.lo, "hi": f.hi,
            "rv_fwd": _f(real[f.i]), "rv21": _f(rv21[f.i]), "vix": _f(vx[f.i]) if vx is not None else None,
            "b0": float(f.beta[0]), "b_d": float(f.beta[1]), "b_w": float(f.beta[2]), "b_m": float(f.beta[3]),
            "n_train": f.n_train, "model_version": MODEL,
        })
    models = {"har": fc, "rv21": rv21}
    if vx is not None:
        models["vix"] = vx
    return Result(und, rv_rows, har_rows, scorecard(b.dates, real, models, lo, hi))


def _f(x) -> float | None:
    x = float(x)
    return x if math.isfinite(x) else None


def _upsert(table: str, cols: list[str], key: tuple[str, ...]) -> str:
    q = lambda c: f'"{c}"' if c == "to" else c
    return (f"INSERT INTO {table} ({', '.join(map(q, cols))}) VALUES ({', '.join('%(' + c + ')s' for c in cols)}) "
            f"ON CONFLICT ({', '.join(key)}) DO UPDATE SET "
            + ", ".join(f"{q(c)} = excluded.{q(c)}" for c in cols if c not in key))


_RV = _upsert("rv_eod", ["trade_date", "underlying", "win", "cc", "pk", "gk", "yz", "model_version"],
              ("trade_date", "underlying", "win"))
_HAR = _upsert("har_eod", ["trade_date", "underlying", "horizon", "fc", "lo", "hi", "rv_fwd", "rv21", "vix",
                           "b0", "b_d", "b_w", "b_m", "n_train", "model_version"],
               ("trade_date", "underlying", "horizon"))
_SCORE = _upsert("vol_score", ["asof", "underlying", "horizon", "model", "n", "frm", "to", "qlike", "mse", "bias",
                               "hit", "model_version"],
                 ("asof", "underlying", "horizon", "model"))


def contract(und: str) -> str:
    return f"NSE|{und}|||IDX"


def _bars(cur, c: str) -> list[tuple]:
    cur.execute("""SELECT trade_date, open, high, low, close FROM index_eod
                   WHERE contract = %s AND open > 0 AND high > 0 AND low > 0 AND close > 0 AND high >= low
                   ORDER BY trade_date""", (c,))
    return cur.fetchall()


def main(argv: list[str] | None = None) -> int:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s: %(message)s")
    ap = argparse.ArgumentParser(prog="pmt-vol", description="Realized vol and the HAR-RV forecast from index_eod.")
    ap.add_argument("--und", help="comma-separated underlyings (default: the configured ones)")
    ap.add_argument("--dry-run", action="store_true")
    a = ap.parse_args(argv)

    import psycopg

    from .eodiv import database_url

    cfg = config.load()
    unds = a.und.split(",") if a.und else [u.name for u in cfg.underlyings]
    with psycopg.connect(database_url()) as conn, conn.cursor() as cur:
        vix = {d: float(c) for d, *_, c in _bars(cur, VIX_CONTRACT)}
        for und in unds:
            rows = _bars(cur, contract(und))
            if len(rows) < MIN_TRAIN + HORIZON + max(HAR_LAGS):
                print(f"vol: {und}: {len(rows)} bars in index_eod, too few; skipped")
                continue
            res = compute(und, Bars.of(rows), vix if und == cfg.vix_underlying else None)
            asof = rows[-1][0]
            print(f"vol: {und} {rows[0][0]}..{asof}: {len(res.rv_rows)} rv rows, {len(res.har_rows)} forecasts")
            for s in res.scores:
                print(f"  {s.model:<5} n {s.n} {s.frm}..{s.to}  qlike {s.qlike:.4f}  mse {s.mse:.3e}  "
                      f"bias {s.bias:+.4f}" + (f"  band hit {s.hit:.2f}" if s.hit is not None else ""))
            if a.dry_run:
                continue
            cur.executemany(_RV, res.rv_rows)
            cur.executemany(_HAR, res.har_rows)
            cur.executemany(_SCORE, [{"asof": asof, "underlying": und, "horizon": HORIZON, "model": s.model,
                                      "n": s.n, "frm": s.frm, "to": s.to, "qlike": s.qlike, "mse": s.mse,
                                      "bias": s.bias, "hit": s.hit, "model_version": MODEL} for s in res.scores])
            conn.commit()
    if a.dry_run:
        print("vol: dry run, nothing written")
    return 0


if __name__ == "__main__":
    sys.exit(main())
