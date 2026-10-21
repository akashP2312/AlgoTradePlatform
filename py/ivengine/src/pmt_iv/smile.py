"""The smile for one expiry: a raw SVI fit in total variance against log-moneyness,
with Gatheral's butterfly check, and the ATM / 25-delta skew read off it.

  w(k) = a + b (rho (k - m) + sqrt((k - m)^2 + s^2)),  k = ln(K / F),  w = iv^2 T

A fit that fails, misses the quotes by more than max_rmse in vol, or shows
butterfly arbitrage is reported with ok = false; the skew then falls back to
interpolating the raw ivs, so the screens never show a number from a bad fit.
Butterfly arbitrage is checked over the quoted strikes widened by ARB_MARGIN, which
covers the risk engine's spot shocks: SVI's extrapolation far past the last quote
says nothing about the market, and Gatheral's condition fails out there on fits
that match every quote.
"""

from __future__ import annotations

import math
from dataclasses import dataclass, field

import numpy as np
from scipy.optimize import least_squares

from . import black76

MIN_POINTS = 5
MAX_RMSE = 0.02  # in vol (2 vol points)
ARB_MARGIN = 0.25  # in log-moneyness, past the outermost quotes
ARB_PENALTY = 1.0  # weight of a negative g against a total-variance miss
_GRID = np.linspace(-1.5, 1.5, 301)


@dataclass
class SVI:
    a: float
    b: float
    rho: float
    m: float
    s: float

    def w(self, k):
        x = np.asarray(k, dtype=float) - self.m
        return self.a + self.b * (self.rho * x + np.sqrt(x * x + self.s * self.s))

    def g(self, k):
        """Gatheral's density condition; negative anywhere means butterfly arbitrage."""
        k = np.asarray(k, dtype=float)
        x = k - self.m
        r = np.sqrt(x * x + self.s * self.s)
        w = self.w(k)
        w1 = self.b * (self.rho + x / r)
        w2 = self.b * self.s * self.s / (r * r * r)
        return (1.0 - k * w1 / (2.0 * w)) ** 2 - (w1 * w1 / 4.0) * (1.0 / w + 0.25) + w2 / 2.0

    def params(self) -> dict[str, float]:
        return {"a": self.a, "b": self.b, "rho": self.rho, "m": self.m, "s": self.s}


@dataclass
class Fit:
    svi: SVI | None
    rmse: float | None
    ok: bool
    arb: list[str] = field(default_factory=list)


def fit_svi(k: list[float], iv: list[float], T: float, max_rmse: float = MAX_RMSE) -> Fit:
    if len(k) < MIN_POINTS or T <= 0:
        return Fit(None, None, False, ["points"])
    ka = np.asarray(k, dtype=float)
    va = np.asarray(iv, dtype=float)
    wa = va * va * T
    # Weight the wings down a little: their quotes are the widest.
    wt = 1.0 / np.sqrt(1.0 + 4.0 * ka * ka)

    def resid(p):
        return (SVI(*p).w(ka) - wa) * wt

    wmin = float(wa.min())
    lo = [-wmin - 1.0, 1e-6, -0.999, ka.min() - 0.5, 1e-4]
    hi = [wmin + 1.0, 10.0, 0.999, ka.max() + 0.5, 2.0]
    best = None
    for rho0 in (-0.5, 0.0):
        x0 = [max(wmin * 0.9, 1e-6), 0.1, rho0, 0.0, 0.1]
        try:
            r = least_squares(resid, x0, bounds=(lo, hi), method="trf", x_scale="jac", max_nfev=2000)
        except (ValueError, FloatingPointError):
            continue
        if best is None or r.cost < best.cost:
            best = r
    # Status 0 is the evaluation limit, not a failure: with quotes a few percent
    # either side of the money the five parameters trade off along a flat valley
    # and the solver crawls along it long after the fit is good. The rmse and
    # arbitrage checks below judge the result either way.
    if best is None or not (best.success or best.status == 0) or not np.all(np.isfinite(best.x)):
        return Fit(None, None, False, ["fit"])
    grid = np.linspace(ka.min() - ARB_MARGIN, ka.max() + ARB_MARGIN, 301)

    def butterfly(svi: SVI) -> bool:
        return bool(np.any(svi.w(grid) <= 0) or np.any(svi.g(grid) < -1e-9))

    svi = SVI(*map(float, best.x))
    if butterfly(svi):
        # Refit from there with the density condition as a penalty: the free fit
        # often bends the wing just past the last quote, where nothing pins it.
        def penalised(p):
            q = SVI(*p)
            w = q.w(grid)
            g = np.where(w > 0, q.g(grid), -1.0)
            return np.concatenate([resid(p), ARB_PENALTY * np.minimum(g - 1e-4, 0.0)])

        try:
            r = least_squares(penalised, best.x, bounds=(lo, hi), method="trf", x_scale="jac", max_nfev=2000)
            if (r.success or r.status == 0) and np.all(np.isfinite(r.x)) and not butterfly(SVI(*r.x)):
                svi = SVI(*map(float, r.x))
        except (ValueError, FloatingPointError):
            pass
    wm = svi.w(ka)
    if np.any(wm <= 0):
        return Fit(svi, None, False, ["negative"])
    rmse = float(np.sqrt(np.mean((np.sqrt(wm / T) - va) ** 2)))
    arb = []
    if butterfly(svi):
        arb.append("butterfly")
    if rmse > max_rmse:
        arb.append("rmse")
    return Fit(svi, rmse, not arb, arb)


def calendar_arb(earlier: SVI, T1: float, later: SVI, T2: float) -> bool:
    """True when total variance falls somewhere between two expiries (T1 < T2)."""
    if T2 <= T1:
        return False
    return bool(np.any(later.w(_GRID) < earlier.w(_GRID) - 1e-9))


class Smile:
    """The vol at any strike for one expiry: the fit when it is ok, else linear
    interpolation in log-moneyness over the raw OTM ivs (flat beyond the ends)."""

    def __init__(self, F: float, T: float, k: list[float], iv: list[float], fit: Fit):
        self.F, self.T, self.fit = F, T, fit
        order = np.argsort(k)
        self.k = np.asarray(k, dtype=float)[order]
        self.v = np.asarray(iv, dtype=float)[order]

    def iv_k(self, k: float) -> float | None:
        if self.fit.ok and self.fit.svi is not None:
            w = float(self.fit.svi.w(k))
            return math.sqrt(w / self.T) if w > 0 else None
        if len(self.k) == 0:
            return None
        return float(np.interp(k, self.k, self.v))

    def iv_strike(self, K: float) -> float | None:
        return self.iv_k(math.log(K / self.F))

    def atm(self) -> float | None:
        return self.iv_k(0.0)

    def delta_vol(self, delta: float, iters: int = 30) -> tuple[float, float] | None:
        """(strike, vol) of the option with this forward delta, by fixed point."""
        v = self.atm()
        if not v:
            return None
        K = self.F
        for _ in range(iters):
            K = black76.strike_for_delta(self.F, self.T, v, delta)
            nv = self.iv_strike(K)
            if not nv:
                return None
            if abs(nv - v) < 1e-7:
                v = nv
                break
            v = nv
        return K, v

    def skew(self) -> dict[str, float | None]:
        atm = self.atm()
        c = self.delta_vol(0.25)
        p = self.delta_vol(-0.25)
        rr = bf = None
        if atm and c and p:
            rr = c[1] - p[1]
            bf = (c[1] + p[1]) / 2.0 - atm
        return {"atm": atm, "rr25": rr, "bf25": bf}
