"""The forward for one expiry, from put-call parity on the quotes nearest the money:
F = K + (C - P) / df, the median over the n strikes nearest a first guess. The
guess is the same-expiry future when one is quoted, else spot carried at the rate.
If too few strikes have both sides quoted, the guess itself is the forward and
the source says which it was."""

from __future__ import annotations

import math
import statistics


def parity_forward(
    pairs: list[tuple[float, float, float]],  # (strike, call mid, put mid)
    df: float,
    guess: float,
    n: int = 5,
    min_pairs: int = 2,
) -> float | None:
    if guess <= 0 or not pairs:
        return None
    near = sorted(pairs, key=lambda p: abs(p[0] - guess))[:n]
    if len(near) < min_pairs:
        return None
    est = [k + (c - p) / df for k, c, p in near]
    # Re-centre once on the first estimate, in case the guess was a strike or two off.
    f0 = statistics.median(est)
    near = sorted(pairs, key=lambda p: abs(p[0] - f0))[:n]
    return statistics.median(k + (c - p) / df for k, c, p in near)


def forward(
    pairs: list[tuple[float, float, float]],
    df: float,
    spot: float | None,
    future: float | None,
    rate: float,
    cal_years: float,
    n: int = 5,
) -> tuple[float | None, str]:
    """Returns (forward, source) with source parity | future | spot | none."""
    if future and future > 0:
        guess, src = future, "future"
    elif spot and spot > 0:
        guess, src = spot * math.exp(rate * cal_years), "spot"
    else:
        # Parity alone, centred on the strike where call and put cross.
        if not pairs:
            return None, "none"
        guess = min(pairs, key=lambda p: abs(p[1] - p[2]))[0]
        src = "none"
    f = parity_forward(pairs, df, guess, n)
    if f is not None and f > 0:
        return f, "parity"
    return (guess, src) if src != "none" else (None, "none")
