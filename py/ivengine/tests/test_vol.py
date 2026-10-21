import math
from datetime import date, timedelta

import numpy as np
import pytest

from pmt_iv import vol
from pmt_iv.vol import Bars, Har, forward_var, parts, qlike, realized, scorecard, walk_forward

SIGMA = 0.16


def gbm(n=1500, sigma=SIGMA, overnight=0.2, steps=400, seed=7):
    """Daily bars from a driftless GBM: a share `overnight` of each day's variance comes
    in the gap, the rest along a finely sampled session."""
    rng = np.random.default_rng(seed)
    sd = sigma / math.sqrt(vol.YEAR)
    rows, c = [], 100.0
    d0 = date(2016, 1, 1)
    for i in range(n):
        o = c * math.exp(rng.normal(0, sd * math.sqrt(overnight)))
        path = o * np.exp(np.cumsum(rng.normal(0, sd * math.sqrt((1 - overnight) / steps), steps)))
        path = np.concatenate([[o], path])
        c = float(path[-1])
        rows.append((d0 + timedelta(days=i), o, float(path.max()), float(path.min()), c))
    return Bars.of(rows)


@pytest.fixture(scope="module")
def bars():
    return gbm()


def test_estimators_recover_sigma(bars):
    rv = realized(bars, 63)
    for k in ("cc", "yz"):
        assert np.nanmean(rv[k]) == pytest.approx(SIGMA, rel=0.05), k
    # Session-only estimators see 80% of the variance; discrete sampling biases range a bit low.
    for k in ("pk", "gk"):
        assert np.nanmean(rv[k]) == pytest.approx(SIGMA * math.sqrt(0.8), rel=0.07), k


def test_windows_start_once_returns_exist(bars):
    rv = realized(bars, 5)
    assert np.isnan(rv["cc"][:5]).all()  # day 0 has no return, so the first full window ends at 5
    assert np.isfinite(rv["cc"][5:]).all() and np.isfinite(rv["yz"][5:]).all()


def test_formulas_on_hand_bars():
    rows = [(date(2026, 1, 1), 100, 101, 99, 100), (date(2026, 1, 2), 101, 103, 100, 102),
            (date(2026, 1, 5), 101, 102, 99.5, 100)]
    b = Bars.of(rows)
    rv = realized(b, 2)
    r = [math.log(102 / 100), math.log(100 / 102)]
    assert rv["cc"][2] == pytest.approx(math.sqrt(252 * (r[0] ** 2 + r[1] ** 2) / 2))
    hl = [math.log(103 / 100), math.log(102 / 99.5)]
    assert rv["pk"][2] == pytest.approx(math.sqrt(252 / (4 * math.log(2)) * (hl[0] ** 2 + hl[1] ** 2) / 2))
    oc = [math.log(102 / 101), math.log(100 / 101)]
    gk = [0.5 * hl[i] ** 2 - (2 * math.log(2) - 1) * oc[i] ** 2 for i in range(2)]
    assert rv["gk"][2] == pytest.approx(math.sqrt(252 * sum(gk) / 2))
    ro = [math.log(101 / 100), math.log(101 / 102)]
    h, l, o, c = np.log([[103, 102], [100, 99.5], [101, 101], [102, 100]])
    rs = (h - c) * (h - o) + (l - c) * (l - o)
    k = 0.34 / (1.34 + 3 / 1)
    var = np.var(ro, ddof=1) + k * np.var(oc, ddof=1) + (1 - k) * rs.mean()
    assert rv["yz"][2] == pytest.approx(math.sqrt(252 * var))


def test_forward_var_is_the_next_h_days(bars):
    p = parts(bars)
    y = forward_var(p, 3)
    assert y[10] == pytest.approx(np.mean(p.r[11:14] ** 2))
    assert np.isnan(y[-3:]).all() and np.isfinite(y[-4])


def test_walk_forward_has_no_lookahead(bars):
    m = Har(min_train=300, min_oos=100)
    fcs = walk_forward(bars, m)
    assert fcs and all(f.lo < f.fc < f.hi for f in fcs)
    # Scrambling every bar after a date must not change the forecast made on it.
    f = fcs[200]
    cut = f.i + 1
    rng = np.random.default_rng(1)
    s = rng.uniform(0.5, 2.0, len(bars) - cut)
    b2 = Bars(bars.dates, bars.o.copy(), bars.h.copy(), bars.l.copy(), bars.c.copy())
    for a in (b2.o, b2.h, b2.l, b2.c):
        a[cut:] *= s
    g = next(x for x in walk_forward(b2, m) if x.i == f.i)
    assert g.fc == pytest.approx(f.fc) and g.lo == pytest.approx(f.lo) and g.n_train == f.n_train
    # The first fit waits until min_train targets of h days are realized.
    assert fcs[0].n_train >= 300 and fcs[0].i >= 300 + m.h


def test_har_tracks_constant_vol(bars):
    fcs = walk_forward(bars, Har(min_train=300, min_oos=100))
    assert np.mean([f.fc for f in fcs]) == pytest.approx(SIGMA, rel=0.05)


def test_qlike_and_scorecard():
    x = np.array([0.01, 0.04])
    assert qlike(x, x) == 0.0
    assert qlike(x, 0.5 * x) > qlike(x, 2 * x) > 0  # too low costs more than too high
    d = [date(2026, 1, i) for i in range(1, 5)]
    real = np.array([0.1, 0.2, np.nan, 0.2])
    s = {r.model: r for r in scorecard(d, real, {"har": real.copy(), "vix": real + 0.05},
                                       real - 0.01, real + 0.01)}
    assert s["har"].qlike == 0 and s["har"].hit == 1.0 and s["har"].n == 3 and s["har"].to == d[3]
    assert s["vix"].bias == pytest.approx(0.05) and s["vix"].hit is None


def test_upsert_quotes_to():
    assert '"to" = excluded."to"' in vol._SCORE and "ON CONFLICT (asof, underlying, horizon, model)" in vol._SCORE
