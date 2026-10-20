"""Black-76 on the forward: prices, implied vol (Jaeckel's Let's Be Rational) and
Greeks. T is in trading-time years (tte.TradingClock); df discounts the premium
from expiry with calendar time.

Greeks are in forward terms with no dividend yield, which for an index option
priced off its own synthetic forward is what a desk reads:
  delta  dV/dF                 (call 0..1, put -1..0, undiscounted)
  gamma  d2V/dF2               per rupee of underlying
  vega   dV/dsigma             per vol point (0.01)
  theta  dV/dt                 per trading-time day, negative for a long option
"""

from __future__ import annotations

import math

import py_lets_be_rational as lbr
from py_lets_be_rational.exceptions import VolatilityValueException

_SQRT2 = math.sqrt(2.0)
_INV_SQRT_2PI = 1.0 / math.sqrt(2.0 * math.pi)

MIN_IV = 1e-4
MAX_IV = 5.0


def ncdf(x: float) -> float:
    return 0.5 * math.erfc(-x / _SQRT2)


def npdf(x: float) -> float:
    return _INV_SQRT_2PI * math.exp(-0.5 * x * x)


def ninv(p: float) -> float:
    """Inverse normal CDF (Acklam's rational approximation, |error| < 1.2e-9)."""
    if not 0.0 < p < 1.0:
        raise ValueError("ninv: p must be in (0, 1)")
    a = (-3.969683028665376e01, 2.209460984245205e02, -2.759285104469687e02, 1.383577518672690e02, -3.066479806614716e01, 2.506628277459239e00)
    b = (-5.447609879822406e01, 1.615858368580409e02, -1.556989798598866e02, 6.680131188771972e01, -1.328068155288572e01)
    c = (-7.784894002430293e-03, -3.223964580411365e-01, -2.400758277161838e00, -2.549732539343734e00, 4.374664141464968e00, 2.938163982698783e00)
    d = (7.784695709041462e-03, 3.224671290700398e-01, 2.445134137142996e00, 3.754408661907416e00)
    lo = 0.02425
    if p < lo:
        q = math.sqrt(-2.0 * math.log(p))
        return (((((c[0] * q + c[1]) * q + c[2]) * q + c[3]) * q + c[4]) * q + c[5]) / ((((d[0] * q + d[1]) * q + d[2]) * q + d[3]) * q + 1.0)
    if p > 1.0 - lo:
        return -ninv(1.0 - p)
    q = p - 0.5
    r = q * q
    return (((((a[0] * r + a[1]) * r + a[2]) * r + a[3]) * r + a[4]) * r + a[5]) * q / (((((b[0] * r + b[1]) * r + b[2]) * r + b[3]) * r + b[4]) * r + 1.0)


def price(F: float, K: float, T: float, sigma: float, call: bool, df: float = 1.0) -> float:
    return df * lbr.black(F, K, sigma, T, 1 if call else -1)


def iv(premium: float, F: float, K: float, T: float, call: bool, df: float = 1.0) -> float | None:
    """The Black-76 vol that reprices premium, or None when the premium is at or
    outside the no-arbitrage bounds (below intrinsic, above the forward or strike)."""
    if not (premium > 0.0 and F > 0.0 and K > 0.0 and T > 0.0 and df > 0.0):
        return None
    try:
        v = lbr.implied_volatility_from_a_transformed_rational_guess(premium / df, F, K, T, 1 if call else -1)
    except VolatilityValueException:
        return None
    if not (math.isfinite(v) and MIN_IV <= v <= MAX_IV):
        return None
    return v


def greeks(F: float, K: float, T: float, sigma: float, call: bool, df: float = 1.0, year_days: float = 280.25) -> dict[str, float]:
    sq = math.sqrt(T)
    sv = sigma * sq
    d1 = (math.log(F / K) + 0.5 * sv * sv) / sv
    n1 = npdf(d1)
    delta = ncdf(d1) if call else ncdf(d1) - 1.0
    return {
        "delta": delta,
        "gamma": df * n1 / (F * sv),
        "vega": df * F * n1 * sq / 100.0,
        "theta": -df * F * n1 * sigma / (2.0 * sq) / year_days,
    }


def strike_for_delta(F: float, T: float, sigma: float, delta: float) -> float:
    """The strike whose undiscounted forward delta is delta (calls > 0, puts < 0)
    at vol sigma."""
    sv = sigma * math.sqrt(T)
    d1 = ninv(delta) if delta > 0 else ninv(1.0 + delta)
    return F * math.exp(-d1 * sv + 0.5 * sv * sv)
