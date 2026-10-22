"""The smile for one expiry: a raw SVI fit in total variance against log-moneyness,
with Gatheral's butterfly check, and the ATM / 25-delta skew read off it.

  w(k) = a + b (rho (k - m) + sqrt((k - m)^2 + s^2)),  k = ln(K / F),  w = iv^2 T

A fit that fails to converge, misses the quotes by more than max_rmse in vol, or
shows butterfly arbitrage over the quoted strikes (plus a margin) is reported with
ok = false; the skew then falls back to interpolating the raw ivs, so the screens
never show a number from a bad fit. A free fit with butterfly arbitrage there is
refit once with the density condition as a penalty before it is judged. Arbitrage
only out in the far wings, where there are no quotes to pin the fit, is reported as
wing = true but leaves ok alone.
"""

from __future__ import annotations

import math
from dataclasses import dataclass, field

import numpy as np
from scipy.optimize import least_squares

from . import black76

MIN_POINTS = 5
MAX_RMSE = 0.02  # in vol (2 vol points)
_GRID = np.linspace(-1.5, 1.5, 301)
MARGIN = 0.1  # in k, beyond the outermost quote, over which the arb checks still apply
ARB_PENALTY = 1.0  # weight of a negative g against a total-variance miss, in the refit


def grid(lo: float, hi: float, n: int = 201) -> np.ndarray:
    return np.linspace(lo - MARGIN, hi + MARGIN, n)


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
    wing: bool = False  # butterfly arbitrage beyond the checked range (info only)
    k_range: tuple[float, float] | None = None  # the quoted strikes, in k


def fit_svi(k: list[float], iv: list[float], T: float, max_rmse: float = MAX_RMSE) -> Fit:
    if len(k) < MIN_POINTS or T <= 0:
        return Fit(None, None, False, ["points"])
    ka = np.asarray(k, dtype=float)
    va = np.asarray(iv, dtype=float)
    wa = va * va * T
    # Weight the wings down a little: their quotes are the widest.
    wt = 1.0 / np.sqrt(1.0 + 4.0 * ka * ka)

    # Scale by the mean total variance so the tolerances mean the same at 2 days
    # as at 6 months; unscaled, short expiries stop on max_nfev short of the optimum.
    wbar = float(wa.mean())

    def resid(p):
        return (SVI(*p).w(ka) - wa) * wt / wbar

    wmin = float(wa.min())
    lo = [-wmin - 1.0, 1e-6, -0.999, ka.min() - 0.5, 1e-4]
    hi = [wmin + 1.0, 10.0, 0.999, ka.max() + 0.5, 2.0]
    best = None
    for rho0 in (-0.5, 0.0):
        x0 = [max(wmin * 0.9, 1e-6), 0.1, rho0, 0.0, 0.1]
        try:
            r = least_squares(resid, x0, bounds=(lo, hi), method="trf", max_nfev=20000)
        except (ValueError, FloatingPointError):
            continue
        if r.success and (best is None or r.cost < best.cost):
            best = r
    if best is None:
        return Fit(None, None, False, ["fit"])
    svi = SVI(*map(float, best.x))
    kr = (float(ka.min()), float(ka.max()))
    gk = grid(*kr)
    if _butterfly(svi, gk):
        # Refit from there with the density condition as a penalty: the free fit
        # often bends the wing just past the last quote, where nothing pins it.
        def penalised(p):
            q = SVI(*p)
            g = np.where(q.w(gk) > 0, q.g(gk), -1.0)
            return np.concatenate([resid(p), ARB_PENALTY * np.minimum(g - 1e-4, 0.0)])

        try:
            r = least_squares(penalised, best.x, bounds=(lo, hi), method="trf", max_nfev=20000)
            if r.success and np.all(np.isfinite(r.x)) and not _butterfly(SVI(*r.x), gk):
                svi = SVI(*map(float, r.x))
        except (ValueError, FloatingPointError):
            pass
    wm = svi.w(ka)
    if np.any(wm <= 0):
        return Fit(svi, None, False, ["negative"], k_range=kr)
    rmse = float(np.sqrt(np.mean((np.sqrt(wm / T) - va) ** 2)))
    arb = []
    if _butterfly(svi, gk):
        arb.append("butterfly")
    if rmse > max_rmse:
        arb.append("rmse")
    return Fit(svi, rmse, not arb, arb, wing=_butterfly(svi, _GRID), k_range=kr)


def _butterfly(svi: SVI, k: np.ndarray) -> bool:
    return bool(np.any(svi.w(k) <= 0) or np.any(svi.g(k) < -1e-9))


def calendar_arb(earlier: SVI, T1: float, later: SVI, T2: float, k: np.ndarray | None = None) -> bool:
    """True when total variance falls somewhere on k (default the wide grid) between
    two expiries (T1 < T2)."""
    if T2 <= T1:
        return False
    k = _GRID if k is None else k
    return bool(np.any(later.w(k) < earlier.w(k) - 1e-9))


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
            try:
                K = black76.strike_for_delta(self.F, self.T, v, delta)
            except OverflowError:  # a runaway vol from a far-dated fit: no strike to read
                return None
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
