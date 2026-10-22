"""The engine's state and computation, free of NATS so tests can drive it: a book of
the latest tick per contract, and the per-expiry IV surface, term structure and
30-day VIX computed from it.

Payloads (JSON, published on analytics.<...> and put to KV analytics):

analytics.<und>.iv.<yyyymmdd>
  ts, u, x (YYYY-MM-DD), tte (trading years), cal (calendar years), dte (trading
  days), df, r, f, fsrc (parity | future | spot), s (spot), fut, atm, rr25, bf25,
  fit {a b rho m s rmse ok arb[]} or null, age {med, max} (quote age, ms),
  rows [{k, kx (ln K/F), fit (iv), c {...}, p {...}}] where a side is
  {b, a, mid, spr (spread / mid), iv, delta, gamma, vega, theta, age} with nulls
  for anything that could not be computed.

analytics.<und>.term
  ts, u, rows [{x, dte, tte, f, atm, rr25, bf25, ok, calarb}]

analytics.<und>.vix
  ts, u, vix, nse (INDIA VIX ltp), diff, near/next {x, min, sigma2, f, k0, n}
"""

from __future__ import annotations

import math
import statistics
from dataclasses import dataclass, field
from datetime import date, datetime

from . import black76, clock, forward, smile, tte, vix
from .calendar import Calendar

VIX_INDEX = "INDIAVIX"


@dataclass
class Quote:
    bid: float = 0.0
    ask: float = 0.0
    ltp: float = 0.0
    recv: int = 0  # ms

    def mid(self, max_spread: float) -> tuple[float | None, float | None]:
        """(mid, spread / mid), mid None when the quote is one-sided, crossed or too wide."""
        if self.bid <= 0 or self.ask <= 0 or self.ask < self.bid:
            return None, None
        m = (self.bid + self.ask) / 2.0
        spr = (self.ask - self.bid) / m
        return (m if spr <= max_spread else None), spr


@dataclass
class Chain:
    und: str
    expiry: date
    calls: dict[float, Quote] = field(default_factory=dict)
    puts: dict[float, Quote] = field(default_factory=dict)
    fut: Quote | None = None


def _quote(t: dict) -> Quote:
    return Quote(float(t.get("b") or 0), float(t.get("a") or 0), float(t.get("p") or 0), int(t.get("r") or 0))


class Book:
    """The latest quote per contract for the configured underlyings."""

    def __init__(self, unds: list[str]):
        self.unds = set(unds)
        self.chains: dict[tuple[str, date], Chain] = {}
        self.futs: dict[tuple[str, date], Quote] = {}
        self.spot: dict[str, Quote] = {}
        self.ticks = 0

    def update(self, t: dict) -> tuple[str, date] | str | None:
        """Applies a tick; returns the (und, expiry) it touched, the index name for
        an index tick, or None when the tick is not for this engine."""
        c = t.get("c")
        if not c:
            return None
        p = c.split("|")
        if len(p) != 5:
            return None
        exch, name, exp, strike, kind = p
        if kind == "IDX":
            if name in self.unds or name == VIX_INDEX:
                self.spot[name] = _quote(t)
                self.ticks += 1
                return name
            return None
        if name not in self.unds or not exp:
            return None
        try:
            d = date.fromisoformat(exp)
        except ValueError:
            return None
        key = (name, d)
        if kind == "FUT":
            q = _quote(t)
            self.futs[key] = q
            if key in self.chains:
                self.chains[key].fut = q
            self.ticks += 1
            return key
        if kind not in ("CE", "PE"):
            return None
        try:
            k = float(strike)
        except ValueError:
            return None
        ch = self.chains.get(key)
        if ch is None:
            ch = self.chains[key] = Chain(name, d, fut=self.futs.get(key))
        (ch.calls if kind == "CE" else ch.puts)[k] = _quote(t)
        self.ticks += 1
        return key

    def expiries(self, und: str) -> list[date]:
        return sorted(d for u, d in self.chains if u == und)

    def spot_price(self, name: str) -> float | None:
        q = self.spot.get(name)
        return q.ltp if q and q.ltp > 0 else None


def _fut_price(q: Quote | None) -> float | None:
    if q is None:
        return None
    if q.bid > 0 and q.ask >= q.bid:
        return (q.bid + q.ask) / 2.0
    return q.ltp if q.ltp > 0 else None


def _r(x: float | None, n: int = 6) -> float | None:
    if x is None or not math.isfinite(x):
        return None
    return round(x, n)


@dataclass
class Params:
    rate: float = 0.065
    max_spread: float = 0.30
    forward_strikes: int = 5
    vix_roll_days: int = 3
    # Quotes older than this leave the forward and the smile: a strike the ticker no
    # longer streams (its window moved, or the feed restarted) keeps its last quote.
    max_age_ms: int = 300_000


@dataclass
class ExpiryResult:
    payload: dict
    svi: smile.SVI | None
    ok: bool
    T: float
    k_range: tuple[float, float] | None = None


class Engine:
    def __init__(self, book: Book, cal: Calendar, params: Params, weekend_weight: float = 0.25, overnight_weight: float = 0.0):
        self.book = book
        self.cal = cal
        self.p = params
        self.clock = tte.TradingClock(cal, weekend_weight, overnight_weight)

    # ---- one expiry -------------------------------------------------------

    def expiry(self, und: str, d: date, now: datetime) -> ExpiryResult | None:
        ch = self.book.chains.get((und, d))
        if ch is None:
            return None
        exp_at = tte.expiry_instant(d)
        if exp_at <= now:
            return None
        T = self.clock.years(now, exp_at)
        Tc = tte.calendar_years(now, exp_at)
        if T <= 0 or Tc <= 0:
            return None
        r = self.p.rate
        df = math.exp(-r * Tc)
        nowms = clock.ms(now)

        strikes = sorted(set(ch.calls) | set(ch.puts))
        mids: dict[float, tuple] = {}
        pairs = []
        def fresh(q: Quote | None) -> bool:
            return q is not None and not (self.p.max_age_ms and q.recv and nowms - q.recv > self.p.max_age_ms)

        for k in strikes:
            c, p = ch.calls.get(k), ch.puts.get(k)
            cm, cs = c.mid(self.p.max_spread) if fresh(c) else (None, None)
            pm, ps = p.mid(self.p.max_spread) if fresh(p) else (None, None)
            mids[k] = (cm, cs, pm, ps)
            if cm is not None and pm is not None:
                pairs.append((k, cm, pm))

        spot = self.book.spot_price(und)
        fut = _fut_price(ch.fut or self.book.futs.get((und, d)))
        F, fsrc = forward.forward(pairs, df, spot, fut, r, Tc, self.p.forward_strikes)
        if F is None:
            return None

        rows = []
        kx, ivs = [], []
        ages = []
        for k in strikes:
            cm, cs, pm, ps = mids[k]
            row = {"k": k, "kx": _r(math.log(k / F))}
            for side, q, m, spr, call in (("c", ch.calls.get(k), cm, cs, True), ("p", ch.puts.get(k), pm, ps, False)):
                if q is None:
                    row[side] = None
                    continue
                age = nowms - q.recv if q.recv else None
                if age is not None:
                    ages.append(age)
                v = black76.iv(m, F, k, T, call, df) if m is not None else None
                g = black76.greeks(F, k, T, v, call, df, self.clock.per_day) if v else None
                row[side] = {
                    "b": q.bid or None, "a": q.ask or None, "mid": _r(m, 4), "spr": _r(spr, 4),
                    "iv": _r(v), "age": age,
                    "delta": _r(g["delta"]) if g else None, "gamma": _r(g["gamma"], 9) if g else None,
                    "vega": _r(g["vega"], 4) if g else None, "theta": _r(g["theta"], 4) if g else None,
                }
                # The smile uses out-of-the-money quotes only: puts below F, calls at or above.
                if v and (call == (k >= F)):
                    kx.append(math.log(k / F))
                    ivs.append(v)
            rows.append(row)

        fit = smile.fit_svi(kx, ivs, T)
        sm = smile.Smile(F, T, kx, ivs, fit)
        sk = sm.skew() if ivs else {"atm": None, "rr25": None, "bf25": None}
        for row in rows:
            row["fit"] = _r(sm.iv_k(row["kx"])) if fit.ok and row["kx"] is not None else None

        payload = {
            "ts": nowms, "u": und, "x": d.isoformat(),
            "tte": _r(T, 8), "cal": _r(Tc, 8), "dte": _r(T * self.clock.per_day, 4), "df": _r(df, 8), "r": r,
            "f": _r(F, 4), "fsrc": fsrc, "s": spot, "fut": _r(fut, 4),
            "atm": _r(sk["atm"]), "rr25": _r(sk["rr25"]), "bf25": _r(sk["bf25"]),
            "fit": ({**{kk: _r(vv, 8) for kk, vv in fit.svi.params().items()}, "rmse": _r(fit.rmse, 6), "ok": fit.ok, "arb": fit.arb,
                    "wing": fit.wing}
                    if fit.svi is not None else {"ok": False, "arb": fit.arb}),
            "age": {"med": int(statistics.median(ages)), "max": max(ages)} if ages else None,
            "n": len(ivs),
            "rows": rows,
        }
        return ExpiryResult(payload, fit.svi if fit.ok else None, fit.ok, T, fit.k_range)

    # ---- term structure ---------------------------------------------------

    @staticmethod
    def term(und: str, results: list[ExpiryResult], nowms: int) -> dict:
        rows = []
        prev: ExpiryResult | None = None
        for res in sorted(results, key=lambda r: r.payload["x"]):
            pl = res.payload
            calarb = None
            if prev is not None and prev.svi is not None and res.svi is not None:
                # Compare only where both expiries have quotes (plus the margin).
                k = None
                if prev.k_range and res.k_range:
                    lo, hi = max(prev.k_range[0], res.k_range[0]), min(prev.k_range[1], res.k_range[1])
                    k = smile.grid(lo, hi) if lo < hi else None
                calarb = smile.calendar_arb(prev.svi, prev.T, res.svi, res.T, k)
            rows.append({"x": pl["x"], "dte": pl["dte"], "tte": pl["tte"], "f": pl["f"], "atm": pl["atm"],
                         "rr25": pl["rr25"], "bf25": pl["bf25"], "ok": res.ok, "calarb": calarb})
            if res.svi is not None:
                prev = res
        return {"ts": nowms, "u": und, "rows": rows}

    # ---- VIX --------------------------------------------------------------

    @staticmethod
    def monthlies(expiries: list[date]) -> list[date]:
        """The last observed expiry in each calendar month."""
        out = []
        for i, d in enumerate(expiries):
            if i == len(expiries) - 1 or (expiries[i + 1].year, expiries[i + 1].month) != (d.year, d.month):
                out.append(d)
        return out

    def vix(self, und: str, now: datetime) -> dict | None:
        months = [d for d in self.monthlies(self.book.expiries(und)) if tte.expiry_instant(d) > now]
        # Roll to the next pair once the near month is within vix_roll_days (verify against NSE).
        while months and (months[0] - now.date()).days <= self.p.vix_roll_days:
            months = months[1:]
        if len(months) < 2:
            return None
        terms = []
        for d in months[:2]:
            ch = self.book.chains[(und, d)]
            minutes = (tte.expiry_instant(d) - now).total_seconds() / 60.0
            qs = []
            for k in sorted(set(ch.calls) | set(ch.puts)):
                c, p = ch.calls.get(k), ch.puts.get(k)
                cm = c.mid(1.0)[0] if c else None
                pm = p.mid(1.0)[0] if p else None
                qs.append(vix.Quote(k, c.bid if c else 0.0, cm, p.bid if p else 0.0, pm))
            t = vix.term_variance(qs, minutes, self.p.rate)
            if t is None:
                return None
            terms.append((d, t))
        v = vix.vix(terms[0][1], terms[1][1])
        nse = self.book.spot_price(VIX_INDEX)
        side = lambda d, t: {"x": d.isoformat(), "min": round(t.minutes, 1), "sigma2": _r(t.sigma2, 8),
                             "f": _r(t.F, 4), "k0": t.K0, "n": t.strikes}
        return {"ts": clock.ms(now), "u": und, "vix": _r(v, 4), "nse": nse,
                "diff": _r(v - nse, 4) if v is not None and nse else None,
                "near": side(*terms[0]), "next": side(*terms[1])}
