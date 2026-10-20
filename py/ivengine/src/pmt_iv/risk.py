"""Book risk: Greeks in rupees and lots, the scenario grid, the time axis and the
breakeven move, for the open legs of book.positions, priced off the ivengine's
smiles (analytics.<und>.iv.<yyyymmdd>) and each leg's own live quote.

Model
  Each option keeps its own iv (from its quote when it has one, else the chain row,
  else the smile) and its residual against the smile. Under a shock the forward
  moves to F' = F (1 + x), the vol is read off the smile at the new moneyness
  (sticky moneyness) plus the residual plus the expiry's share of the vol shock:

    iv' = smile(ln(K / F')) + resid + dv30 * min(sqrt(30 / calendar DTE), cap) / 100

  dv30 is a 30-day vol shock in vol points, the scale of India VIX, so a weekly
  moves more than a quarterly for the same VIX move. The "path" row ties the vol
  shock to the spot move with the spot-vol beta (vol points per 1% move, separate
  for falls and rises), which is how the market actually moves: a -3% day comes with
  vol bid, not unchanged.

  Scenario P&L is model to model, sum q (V' - V), so the centre cell is zero. The
  liquidation figure also crosses the spread: |q| half-spread, widened in the
  stress cells, since spreads blow out exactly when the book would need closing.

Units (q is in units, as Kite reports it; lot sizes come from refdata)
  d   rupees per 1% move of the underlying       q delta F / 100
  dl  delta in lots of the leg's own underlying  q delta / lot
  dn  delta in NIFTY-future lots (beta-weighted)
  g   change in d per 1% move (rupees)           q gamma (F / 100)^2
  gl  change in dl per 1% move (lots)
  v   rupees per vol point                       q vega
  v30 rupees per 30-day vol point (VIX-scaled)   q vega scale
  t   rupees per day (a weekday, close to close)  q theta

Breakeven: the daily move at which gamma P&L pays the day's theta, sqrt(2 |t| / |g|)
percent, shown also in daily standard deviations of the nearest expiry's ATM vol.

Nothing here can place or change an order; it only reads.
"""

from __future__ import annotations

import math
from dataclasses import dataclass, field
from datetime import date, datetime, timedelta

from . import black76, clock, tte
from .calendar import Calendar
from .engine import Quote, _r
from .smile import SVI, Fit, Smile

SPOT_STEPS = (-10.0, -7.0, -5.0, -3.0, -2.0, -1.0, -0.5, 0.0, 0.5, 1.0, 2.0, 3.0, 5.0, 7.0, 10.0)
VOL_ROWS = (-5.0, -2.0, 0.0, 2.0, 5.0, 10.0)
MIN_VOL = 0.01
TICK = 0.05


@dataclass
class Params:
    rate: float = 0.065
    weekend_weight: float = 0.25
    overnight_weight: float = 0.0
    spot_steps: tuple[float, ...] = SPOT_STEPS  # percent
    vol_rows: tuple[float, ...] = VOL_ROWS  # 30-day vol points
    vol_cap: float = 2.5  # the most a short expiry's vol shock is scaled up
    stale_ms: int = 60_000  # a leg quote older than this in session is stale
    wide: float = 0.15  # a spread wider than this fraction of the mid is flagged
    widen: float = 1.0  # spread multiple added per 5% move and per 5 vol points up
    ref: str = "NIFTY"  # the underlying the book's delta is expressed in
    ref_lot: int = 75  # its lot when no leg in the book carries it
    beta_dn: float = -1.3  # vol points per 1% fall (negative: falls lift vol)
    beta_up: float = -0.5  # vol points per 1% rise
    und_beta: dict[str, float] = field(default_factory=dict)  # each underlying against ref


@dataclass
class Beta:
    """The spot-vol beta (dv in vol points = dn or up times the move in percent) and
    each underlying's beta to the reference index. src is "default" until the
    spotvol job has measured them from index_eod."""

    dn: float
    up: float
    und: dict[str, float]
    src: str = "default"
    n: int = 0
    asof: str | None = None

    @classmethod
    def default(cls, p: Params) -> "Beta":
        return cls(p.beta_dn, p.beta_up, dict(p.und_beta))

    @classmethod
    def from_payload(cls, d: dict, p: Params) -> "Beta":
        def num(v, dflt):
            return float(v) if isinstance(v, (int, float)) and math.isfinite(v) else dflt

        und = dict(p.und_beta)
        for k, v in (d.get("und") or {}).items():
            if isinstance(v, (int, float)) and math.isfinite(v) and v > 0:
                und[str(k)] = float(v)
        return cls(num(d.get("dn"), p.beta_dn), num(d.get("up"), p.beta_up), und,
                   str(d.get("src") or "measured"), int(d.get("n") or 0), d.get("asof"))

    def path(self, x_pct: float) -> float:
        return (self.dn if x_pct < 0 else self.up) * x_pct

    def of(self, und: str, ref: str) -> float:
        return 1.0 if und == ref else self.und.get(und, 1.0)

    def payload(self) -> dict:
        return {"dn": self.dn, "up": self.up, "und": self.und, "src": self.src, "n": self.n, "asof": self.asof}


@dataclass
class Leg:
    """One open row of book.positions (q in units)."""

    id: str
    sym: str
    c: str
    subj: str
    und: str
    kind: str  # CE | PE | FUT | EQ | ""
    expiry: date | None
    K: float
    q: int
    lot: int
    product: str

    @classmethod
    def from_row(cls, r: dict) -> "Leg":
        e = r.get("e") or ""
        return cls(
            id=f"{r.get('x', '')}:{r.get('sym', '')}:{r.get('pr', '')}",
            sym=r.get("sym", ""), c=r.get("c") or "", subj=r.get("s") or "", und=r.get("u") or "",
            kind=r.get("k") or "", expiry=date.fromisoformat(e) if e else None,
            K=float(r.get("K") or 0.0), q=int(r.get("q") or 0), lot=int(r.get("lot") or 0),
            product=r.get("pr") or "",
        )

    @property
    def option(self) -> bool:
        return self.kind in ("CE", "PE")

    @property
    def name(self) -> str:
        if not self.c:
            return self.sym
        e = self.expiry.strftime("%d%b").upper() if self.expiry else ""
        if self.kind == "FUT":
            return f"{self.und} {e} FUT"
        if self.option:
            return f"{self.und} {e} {self.K:g} {self.kind}"
        return self.und or self.sym


def legs_from_book(book: dict) -> list[Leg]:
    return [Leg.from_row(r) for r in book.get("net") or [] if int(r.get("q") or 0) != 0]


def quote_from_tick(t: dict) -> Quote:
    return Quote(float(t.get("b") or 0), float(t.get("a") or 0), float(t.get("p") or 0), int(t.get("r") or 0))


@dataclass
class Surface:
    """One expiry's smile as the ivengine published it."""

    und: str
    expiry: date
    F: float
    spot: float | None
    T: float  # trading years at ts
    r: float
    ts: int
    atm: float | None
    ok: bool
    smile: Smile
    rows: dict[tuple[float, str], float]  # (strike, "c"|"p") -> iv

    @classmethod
    def from_payload(cls, pl: dict) -> "Surface | None":
        F, T = pl.get("f"), pl.get("tte")
        if not F or not T or F <= 0 or T <= 0:
            return None
        rows: dict[tuple[float, str], float] = {}
        ks, ivs = [], []
        for row in pl.get("rows") or []:
            K, kx = row.get("k"), row.get("kx")
            for side in ("c", "p"):
                q = row.get(side)
                v = q.get("iv") if q else None
                if not v:
                    continue
                rows[(float(K), side)] = float(v)
                if kx is not None and (side == "c") == (K >= F):
                    ks.append(float(kx))
                    ivs.append(float(v))
        f = pl.get("fit") or {}
        ok = bool(f.get("ok"))
        svi = SVI(f["a"], f["b"], f["rho"], f["m"], f["s"]) if ok and all(f.get(x) is not None for x in ("a", "b", "rho", "m", "s")) else None
        fit = Fit(svi, f.get("rmse"), svi is not None, list(f.get("arb") or []))
        return cls(
            und=pl.get("u", ""), expiry=date.fromisoformat(pl["x"]), F=float(F), spot=pl.get("s"),
            T=float(T), r=float(pl.get("r") or 0.0), ts=int(pl.get("ts") or 0), atm=pl.get("atm"),
            ok=fit.ok, smile=Smile(float(F), float(T), ks, ivs, fit), rows=rows,
        )

    def iv_k(self, k: float) -> float | None:
        v = self.smile.iv_k(k)
        return v if v and math.isfinite(v) and v > 0 else None


@dataclass
class Mark:
    """A leg valued now: what the shocks are applied to."""

    leg: Leg
    surf: Surface | None
    F: float  # the expiry's forward (FUT: the future's own price)
    V: float  # model value per unit now
    iv: float | None
    src: str  # quote | chain | smile | fut
    resid: float
    scale: float  # this expiry's multiple of the 30-day vol shock
    half: float  # half the spread in rupees per unit
    mid: float | None
    spr: float | None
    age: int | None
    dte: float  # calendar days
    g: dict[str, float]  # per-unit Greeks: delta dV/dF, gamma, vega per vol point, theta per trading day
    flags: list[str]
    out: dict = field(default_factory=dict)  # the leg's rupee and lot Greeks


def _intrinsic(F: float, K: float, call: bool) -> float:
    return max(F - K, 0.0) if call else max(K - F, 0.0)


class Risk:
    def __init__(self, cal: Calendar, p: Params | None = None):
        self.cal = cal
        self.p = p or Params()
        self.clock = tte.TradingClock(cal, self.p.weekend_weight, self.p.overnight_weight)
        self._t: dict[tuple[datetime, date], tuple[float, float]] = {}

    # ---- time -------------------------------------------------------------

    def in_session(self, now: datetime) -> bool:
        s = self.cal.session(now.date())
        return s.trading and s.open <= now <= s.close

    def times(self, at: datetime, d: date) -> tuple[float, float]:
        """(trading years, calendar years) from at to d's expiry, cached per call."""
        key = (at, d)
        v = self._t.get(key)
        if v is None:
            exp = tte.expiry_instant(d)
            v = (self.clock.years(at, exp), max(0.0, tte.calendar_years(at, exp)))
            self._t[key] = v
        return v

    def horizons(self, now: datetime, expiries: list[date]) -> list[tuple[str, datetime]]:
        """now, today's close, the next session's open (the overnight gap is the spot
        step applied there) and each expiry in the book."""
        out: dict[datetime, list[str]] = {now: ["now"]}
        s = self.cal.session(now.date())
        if s.trading and now < s.close:
            out.setdefault(s.close, []).append("close")
        d = now.date()
        for _ in range(30):
            s = self.cal.session(d)
            if s.trading and s.open > now:
                out.setdefault(s.open, []).append("open")
                break
            d += timedelta(days=1)
        for e in sorted(set(expiries)):
            at = tte.expiry_instant(e)
            if at > now:
                out.setdefault(at, []).append("exp " + e.strftime("%d%b").upper())
        return [(" · ".join(v), k) for k, v in sorted(out.items())]

    # ---- marks ------------------------------------------------------------

    def mark(self, leg: Leg, surf: Surface | None, q: Quote | None, now: datetime, fut_px: float | None = None) -> Mark | str:
        """Values a leg now, or returns why it cannot be modelled."""
        if not leg.c:
            return "not in refdata"
        if leg.kind not in ("CE", "PE", "FUT"):
            return f"{leg.kind or 'unknown'} not modelled"
        if leg.expiry is None:
            return "no expiry"
        if tte.expiry_instant(leg.expiry) <= now:
            return "expired; awaiting settlement"
        nowms = clock.ms(now)
        flags: list[str] = []
        mid = spr = None
        age = None
        half = None
        if q is None:
            flags.append("no quote")
        else:
            mid, spr = q.mid(1.0)
            age = nowms - q.recv if q.recv else None
            if q.bid > 0 and q.ask >= q.bid:
                half = (q.ask - q.bid) / 2.0
            elif q.ltp > 0:
                flags.append("one-sided")
            if self.in_session(now) and (age is None or age > self.p.stale_ms):
                flags.append(f"quote {age // 1000}s old" if age is not None else "quote time unknown")
            if spr is not None and spr > self.p.wide:
                flags.append(f"spread {spr * 100:.0f}%")
        T, Tc = self.times(now, leg.expiry)
        dte = Tc * 365.0
        scale = min(math.sqrt(30.0 / dte), self.p.vol_cap) if dte > 0 else self.p.vol_cap

        if leg.kind == "FUT":
            px = mid or (q.ltp if q and q.ltp > 0 else None) or fut_px or (surf.F if surf else None)
            if not px:
                return "no price for the future"
            if not mid:
                flags.append("marked off " + ("ltp" if q and q.ltp > 0 else "the forward"))
            half = max(half or TICK / 2, TICK / 2)
            return Mark(leg, surf, px, px, None, "fut", 0.0, scale, half, mid, spr, age, dte,
                        {"delta": 1.0, "gamma": 0.0, "vega": 0.0, "theta": 0.0}, flags)

        if surf is None:
            return "no smile for this expiry"
        if not surf.ok:
            flags.append("smile fit not ok (interpolated)")
        if self.in_session(now) and nowms - surf.ts > self.p.stale_ms:
            flags.append(f"smile {(nowms - surf.ts) // 1000}s old")
        call = leg.kind == "CE"
        F, K = surf.F, leg.K
        df = math.exp(-surf.r * Tc)
        k = math.log(K / F)
        on_smile = surf.iv_k(k)
        iv, src = None, ""
        if mid is not None and (spr is None or spr <= 0.30):
            iv, src = black76.iv(mid, F, K, T, call, df), "quote"
        if not iv:
            iv, src = surf.rows.get((K, "c" if call else "p")), "chain"
        if not iv:
            iv, src = on_smile, "smile"
            if iv:
                flags.append("vol off the smile")
        if not iv:
            return "no vol (no quote and nothing on the smile)"
        resid = iv - on_smile if on_smile else 0.0
        V = black76.price(F, K, T, iv, call, df)
        if half is None:
            # No two-sided quote: assume a wide market rather than a free exit.
            half = max(TICK, self.p.wide * V) / 2.0
        half = max(half, TICK / 2)
        g = black76.greeks(F, K, T, iv, call, df, self.clock.per_day)
        g["delta"] *= df  # black76 gives N(d1); the premium is paid now, so dV/dF carries df
        return Mark(leg, surf, F, V, iv, src, resid, scale, half, mid, spr, age, dte, g, flags)

    # ---- shocks -----------------------------------------------------------

    def value(self, m: Mark, x: float, dv: float, at: datetime) -> float:
        """Per-unit value with the underlying moved by x (fraction), a 30-day vol
        shock of dv vol points, at time at."""
        leg = m.leg
        F2 = m.F * (1.0 + x)
        if leg.kind == "FUT":
            return F2
        call = leg.kind == "CE"
        T, Tc = self.times(at, leg.expiry)
        if T <= 0:
            return _intrinsic(F2, leg.K, call)
        base = m.surf.iv_k(math.log(leg.K / F2)) if m.surf else None
        iv = (base + m.resid) if base else m.iv
        iv = max(MIN_VOL, iv + dv * m.scale / 100.0)
        return black76.price(F2, leg.K, T, iv, call, math.exp(-m.surf.r * Tc))

    def factor(self, x_pct: float, dv: float) -> float:
        return 1.0 + self.p.widen * (abs(x_pct) / 5.0 + max(dv, 0.0) / 5.0)

    def pnl(self, marks: list[tuple[Mark, float]], x_pct: float, dv: float, at: datetime) -> tuple[float, float]:
        """(mid, liquidation) P&L of the marks, each (mark, beta) moved by beta x_pct."""
        mid = cost = 0.0
        for m, b in marks:
            xp = x_pct * b
            mid += m.leg.q * (self.value(m, xp / 100.0, dv, at) - m.V)
            if at < tte.expiry_instant(m.leg.expiry):
                cost += abs(m.leg.q) * m.half * self.factor(xp, dv)
        return mid, mid - cost

    def grid(self, marks: list[tuple[Mark, float]], beta: Beta, now: datetime, sig: float | None) -> dict:
        steps = self.p.spot_steps
        rows = []
        for dv in self.p.vol_rows:
            cells = [self.pnl(marks, x, dv, now) for x in steps]
            rows.append({"dv": dv, "mid": [_r(c[0], 0) for c in cells], "liq": [_r(c[1], 0) for c in cells]})
        pdv = [beta.path(x) for x in steps]
        cells = [self.pnl(marks, x, dv, now) for x, dv in zip(steps, pdv)]
        return {
            "sig": [_r(x / (100.0 * sig), 2) if sig else None for x in steps],
            "rows": rows,
            "path": {"dv": [_r(v, 2) for v in pdv], "mid": [_r(c[0], 0) for c in cells], "liq": [_r(c[1], 0) for c in cells]},
        }

    def timeaxis(self, marks: list[tuple[Mark, float]], beta: Beta, hz: list[tuple[str, datetime]]) -> list[dict]:
        out = []
        for label, at in hz:
            cells = [self.pnl(marks, x, beta.path(x), at) for x in self.p.spot_steps]
            out.append({"l": label, "ts": clock.ms(at), "mid": [_r(c[0], 0) for c in cells], "liq": [_r(c[1], 0) for c in cells]})
        return out

    # ---- the book ---------------------------------------------------------

    def book(self, legs: list[Leg], surfaces: dict[tuple[str, date], Surface], quotes: dict[str, Quote],
             beta: Beta, now: datetime, book_ts: int | None = None) -> dict:
        self._t.clear()
        p = self.p
        marks: list[Mark] = []
        unmod = []
        for leg in legs:
            surf = surfaces.get((leg.und, leg.expiry)) if leg.expiry else None
            fut_px = None
            if leg.kind == "FUT" and surf is None:
                # A future's own expiry may have no options; any surface gives a forward.
                near = sorted((s for (u, _), s in surfaces.items() if u == leg.und), key=lambda s: s.expiry)
                fut_px = near[0].F if near else None
            m = self.mark(leg, surf, quotes.get(leg.c), now, fut_px)
            if isinstance(m, str):
                unmod.append({"id": leg.id, "name": leg.name, "q": leg.q, "why": m})
            else:
                marks.append(m)

        by_und: dict[str, list[Mark]] = {}
        for m in marks:
            by_und.setdefault(m.leg.und, []).append(m)

        def nearest(und: str) -> Surface | None:
            ss = [s for (u, d), s in surfaces.items() if u == und and tte.expiry_instant(d) > now]
            return min(ss, key=lambda s: s.expiry) if ss else None

        def sigmas(und: str) -> tuple[float | None, float | None, Surface | None]:
            s = nearest(und)
            if s is None or not s.atm:
                return None, None, s
            T, _ = self.times(now, s.expiry)
            return s.atm * math.sqrt(T), s.atm / math.sqrt(self.clock.per_day), s

        ref_lot = next((m.leg.lot for m in marks if m.leg.und == p.ref and m.leg.lot), 0) or p.ref_lot
        rs = nearest(p.ref)
        ref_px = (rs.spot or rs.F) if rs else None

        legs_out = []
        for m in marks:
            q, F, lg, g = m.leg.q, m.F, m.leg, m.g
            b = beta.of(lg.und, p.ref)
            d = q * g["delta"] * F / 100.0
            m.out = {
                "d": d,
                "dl": q * g["delta"] / lg.lot if lg.lot else None,
                "dn": q * g["delta"] * b * F / (ref_px * ref_lot) if ref_px else None,
                "g": q * g["gamma"] * (F / 100.0) ** 2,
                "gl": q * g["gamma"] * (F / 100.0) / lg.lot if lg.lot else None,
                "v": q * g["vega"],
                "v30": q * g["vega"] * m.scale,
                "t": q * g["theta"],
            }
            legs_out.append({
                "id": lg.id, "name": lg.name, "u": lg.und, "x": lg.expiry.isoformat(), "k": lg.kind,
                "K": lg.K or None, "q": q, "lot": lg.lot, "pr": lg.product,
                "f": _r(F, 2), "v0": _r(m.V, 2), "mid": _r(m.mid, 2), "spr": _r(m.spr, 4), "half": _r(m.half, 2),
                "age": m.age, "iv": _r(m.iv, 4), "src": m.src, "resid": _r(m.resid, 4),
                "dte": _r(m.dte, 2), "scale": _r(m.scale, 3),
                **{k: _r(v, 2) for k, v in m.out.items()}, "flags": m.flags,
            })

        hz = self.horizons(now, [m.leg.expiry for m in marks])

        def agg(ms: list[Mark], betas: list[float]) -> dict:
            tot = {k: 0.0 for k in ("d", "dn", "g", "v", "v30", "t")}
            has_dn = all(m.out["dn"] is not None for m in ms)
            for m, b in zip(ms, betas):
                tot["d"] += m.out["d"] * b
                tot["g"] += m.out["g"] * b * b
                tot["v"] += m.out["v"]
                tot["v30"] += m.out["v30"]
                tot["t"] += m.out["t"]
                tot["dn"] += m.out["dn"] or 0.0
            out = {k: _r(v, 2) for k, v in tot.items()}
            if not has_dn:
                out["dn"] = None
            out["close"] = _r(sum(abs(m.leg.q) * m.half for m in ms), 0)
            out["flags"] = [{"leg": m.leg.name, "why": f} for m in ms for f in m.flags]
            return out

        def breakeven(g: float, t: float, sig_day: float | None) -> dict | None:
            if not g or not t:
                return None
            if g * t > 0:
                return {"pct": None, "sd": None, "why": "gamma and theta have the same sign"}
            x = math.sqrt(2.0 * abs(t) / abs(g))
            return {"pct": _r(x, 3), "sd": _r(x / (100.0 * sig_day), 2) if sig_day else None}

        unds_out = []
        for und in sorted(by_und):
            ms = by_und[und]
            sig_e, sig_d, ns = sigmas(und)
            a = agg(ms, [1.0] * len(ms))
            a["dl"] = _r(sum(m.out["dl"] or 0.0 for m in ms), 2)
            a["gl"] = _r(sum(m.out["gl"] or 0.0 for m in ms), 3)
            exps: dict[date, dict] = {}
            for m in ms:
                e = exps.setdefault(m.leg.expiry, {"x": m.leg.expiry.isoformat(), "dte": _r(m.dte, 2), "scale": _r(m.scale, 3), "v": 0.0, "v30": 0.0, "t": 0.0})
                e["v"] += m.out["v"]
                e["v30"] += m.out["v30"]
                e["t"] += m.out["t"]
            pairs = [(m, 1.0) for m in ms]
            unds_out.append({
                "u": und, "f": _r(ns.F, 2) if ns else None, "s": ns.spot if ns else None,
                "near": ns.expiry.isoformat() if ns else None, "atm": _r(ns.atm, 4) if ns else None,
                "sig": _r(sig_e, 5), "sigday": _r(sig_d, 5), "beta": beta.of(und, p.ref),
                **a,
                "ve": [{**e, "v": _r(e["v"], 0), "v30": _r(e["v30"], 0), "t": _r(e["t"], 0)} for _, e in sorted(exps.items())],
                "be": breakeven(a["g"] or 0.0, a["t"] or 0.0, sig_d),
                "grid": self.grid(pairs, beta, now, sig_e),
                "time": self.timeaxis(pairs, beta, hz),
            })

        betas = [beta.of(m.leg.und, p.ref) for m in marks]
        bk = agg(marks, betas)
        sig_e, sig_d, _ = sigmas(p.ref)
        pairs = list(zip(marks, betas))
        bk["be"] = breakeven(bk["g"] or 0.0, bk["t"] or 0.0, sig_d)
        bk["sig"], bk["sigday"] = _r(sig_e, 5), _r(sig_d, 5)
        bk["grid"] = self.grid(pairs, beta, now, sig_e)
        bk["time"] = self.timeaxis(pairs, beta, hz)
        bk["flags"] += [{"leg": u["name"], "why": u["why"]} for u in unmod]

        return {
            "ts": clock.ms(now), "book_ts": book_ts, "session": self.in_session(now),
            "ref": p.ref, "ref_lot": ref_lot, "ref_px": _r(ref_px, 2),
            "beta": beta.payload(),
            "steps": list(p.spot_steps), "vols": list(p.vol_rows), "cap": p.vol_cap,
            "hz": [{"l": l, "ts": clock.ms(at)} for l, at in hz],
            "legs": legs_out, "unmod": unmod, "unds": unds_out, "book": bk,
        }
