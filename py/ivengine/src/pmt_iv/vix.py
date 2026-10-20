"""A 30-day model-free implied vol in the style of the CBOE VIX, on NIFTY's near and
next monthly expiries, to set beside the published INDIA VIX tick.

NSE's INDIA VIX follows the CBOE method on NIFTY monthlies; the details this
module assumes and that need checking against the NSE white paper are marked
verify: the rate, the forward (parity at the strike where |C - P| is smallest,
as CBOE does), and rolling to the next pair 3 days before the near expiry.
"""

from __future__ import annotations

import math
from dataclasses import dataclass

MINUTES_YEAR = 525600.0
MINUTES_30D = 30 * 1440.0


@dataclass
class Quote:
    strike: float
    call_bid: float
    call_mid: float | None
    put_bid: float
    put_mid: float | None


@dataclass
class Term:
    sigma2: float  # annualised variance
    T: float  # calendar years
    minutes: float
    F: float
    K0: float
    strikes: int


def term_variance(quotes: list[Quote], minutes: float, rate: float) -> Term | None:
    """The CBOE variance for one expiry from its strip of quotes."""
    if minutes <= 0:
        return None
    T = minutes / MINUTES_YEAR
    growth = math.exp(rate * T)
    qs = sorted((q for q in quotes if q.strike > 0), key=lambda q: q.strike)
    both = [q for q in qs if q.call_mid is not None and q.put_mid is not None]
    if len(both) < 2:
        return None
    at = min(both, key=lambda q: abs(q.call_mid - q.put_mid))
    F = at.strike + growth * (at.call_mid - at.put_mid)
    below = [q for q in qs if q.strike <= F]
    if not below:
        return None
    K0 = below[-1].strike
    i0 = qs.index(below[-1])

    used: list[tuple[float, float]] = []  # (strike, Q)

    # Puts from K0 down, then calls from K0 up; each side stops at two zero bids in a row.
    zeros = 0
    for q in reversed(qs[:i0]):
        if q.put_bid <= 0 or q.put_mid is None:
            zeros += 1
            if zeros >= 2:
                break
            continue
        zeros = 0
        used.append((q.strike, q.put_mid))
    zeros = 0
    for q in qs[i0 + 1:]:
        if q.call_bid <= 0 or q.call_mid is None:
            zeros += 1
            if zeros >= 2:
                break
            continue
        zeros = 0
        used.append((q.strike, q.call_mid))
    k0 = qs[i0]
    mids = [m for m in (k0.put_mid, k0.call_mid) if m is not None]
    if not mids:
        return None
    used.append((k0.strike, sum(mids) / len(mids)))
    used.sort()
    if len(used) < 3:
        return None

    total = 0.0
    for i, (K, Q) in enumerate(used):
        if i == 0:
            dK = used[1][0] - K
        elif i == len(used) - 1:
            dK = K - used[i - 1][0]
        else:
            dK = (used[i + 1][0] - used[i - 1][0]) / 2.0
        total += dK / (K * K) * Q
    sigma2 = (2.0 / T) * growth * total - (1.0 / T) * (F / K0 - 1.0) ** 2
    if not (sigma2 > 0 and math.isfinite(sigma2)):
        return None
    return Term(sigma2, T, minutes, F, K0, len(used))


def vix(near: Term, nxt: Term) -> float | None:
    """The 30-day vol in vol points (e.g. 14.2), interpolating (or, when both
    expiries lie past 30 days, extrapolating) total variance in minutes."""
    n1, n2 = near.minutes, nxt.minutes
    if n2 <= n1:
        return None
    w1 = (n2 - MINUTES_30D) / (n2 - n1)
    w2 = (MINUTES_30D - n1) / (n2 - n1)
    v = (near.T * near.sigma2 * w1 + nxt.T * nxt.sigma2 * w2) * MINUTES_YEAR / MINUTES_30D
    return 100.0 * math.sqrt(v) if v > 0 else None
