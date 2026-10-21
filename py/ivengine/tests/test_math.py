import math

import numpy as np
import pytest

from pmt_iv import black76, forward, smile, vix


@pytest.mark.parametrize("call", [True, False])
@pytest.mark.parametrize("K", [22000.0, 25000.0, 28000.0])
def test_iv_round_trip(call, K):
    F, T, df, sigma = 25000.0, 30 / 280.25, math.exp(-0.065 * 30 / 365), 0.17
    p = black76.price(F, K, T, sigma, call, df)
    assert black76.iv(p, F, K, T, call, df) == pytest.approx(sigma, abs=1e-9)


def test_iv_rejects_below_intrinsic_and_bad_inputs():
    F, K, T = 25000.0, 24000.0, 0.1
    assert black76.iv(500.0, F, K, T, True) is None  # intrinsic is 1000
    assert black76.iv(0.0, F, K, T, True) is None
    assert black76.iv(10.0, F, K, 0.0, True) is None


def test_greeks_signs_and_parity():
    F, K, T, s = 25000.0, 25000.0, 0.05, 0.15
    c = black76.greeks(F, K, T, s, True)
    p = black76.greeks(F, K, T, s, False)
    assert c["delta"] - p["delta"] == pytest.approx(1.0)
    assert c["gamma"] == pytest.approx(p["gamma"]) and c["gamma"] > 0
    assert c["vega"] > 0 and c["theta"] < 0
    # Vega per vol point matches a finite difference.
    up = black76.price(F, K, T, s + 0.005, True) - black76.price(F, K, T, s - 0.005, True)
    assert c["vega"] == pytest.approx(up, rel=1e-3)


def test_strike_for_delta():
    F, T, s = 25000.0, 0.1, 0.2
    for d in (0.25, -0.25):
        K = black76.strike_for_delta(F, T, s, d)
        assert black76.greeks(F, K, T, s, d > 0)["delta"] == pytest.approx(d, abs=1e-6)


def _pairs(F, df, strikes, sigma=0.15, T=0.08):
    return [(k, black76.price(F, k, T, sigma, True, df), black76.price(F, k, T, sigma, False, df)) for k in strikes]


def test_parity_forward_recovers_F():
    df = math.exp(-0.065 * 0.08)
    pairs = _pairs(25037.5, df, range(24000, 26050, 50))
    assert forward.parity_forward(pairs, df, guess=24800) == pytest.approx(25037.5, abs=1e-6)
    F, src = forward.forward(pairs, df, spot=24900, future=None, rate=0.065, cal_years=0.08)
    assert src == "parity" and F == pytest.approx(25037.5, abs=1e-6)


def test_forward_falls_back():
    assert forward.forward([], 1.0, None, 25100.0, 0.065, 0.1) == (25100.0, "future")
    F, src = forward.forward([], 1.0, 25000.0, None, 0.065, 0.1)
    assert src == "spot" and F == pytest.approx(25000 * math.exp(0.0065))
    assert forward.forward([], 1.0, None, None, 0.065, 0.1) == (None, "none")


def test_svi_recovers_params_and_skew():
    true = smile.SVI(0.002, 0.04, -0.6, 0.02, 0.08)
    T = 0.1
    k = np.linspace(-0.25, 0.2, 25)
    iv = np.sqrt(true.w(k) / T)
    fit = smile.fit_svi(list(k), list(iv), T)
    assert fit.ok and fit.rmse < 1e-5
    assert np.allclose(fit.svi.w(k), true.w(k), atol=1e-7)
    sk = smile.Smile(25000.0, T, list(k), list(iv), fit).skew()
    assert sk["atm"] == pytest.approx(math.sqrt(float(true.w(0.0)) / T), abs=1e-6)
    assert sk["rr25"] < 0  # put skew
    assert sk["bf25"] > 0


def test_svi_flags():
    assert smile.fit_svi([0.0, 0.1], [0.2, 0.2], 0.1).arb == ["points"]
    # Strongly sloped wings with tiny curvature break Gatheral's condition.
    bad = smile.SVI(-0.05, 0.9, -0.99, 0.0, 0.01)
    assert np.any(bad.g(np.linspace(-1.5, 1.5, 301)) < 0)
    near, far = smile.SVI(0.01, 0.05, -0.3, 0, 0.1), smile.SVI(0.005, 0.05, -0.3, 0, 0.1)
    assert smile.calendar_arb(near, 0.05, far, 0.1)
    assert not smile.calendar_arb(far, 0.05, near, 0.1)


def test_svi_wing_only_arb_is_info_not_a_failure():
    # g >= 0 over the quotes (and the margin) but negative far out at |k| ~ 1.
    true = smile.SVI(-0.004118719781634082, 0.07347267771915938, -0.6933114095072151, 0.026413037513368213, 0.08922424683529111)
    T, k = 30 / 365, np.linspace(-0.12, 0.08, 15)
    assert np.any(true.g(smile._GRID) < 0) and np.all(true.g(smile.grid(k.min(), k.max())) >= 0)
    fit = smile.fit_svi(list(k), list(np.sqrt(true.w(k) / T)), T)
    assert fit.ok and fit.arb == [] and fit.wing
    assert fit.k_range == pytest.approx((-0.12, 0.08))


def test_svi_butterfly_inside_the_quotes_still_fails():
    bad = smile.SVI(-0.05, 0.9, -0.99, 0.0, 0.01)
    k = np.linspace(-0.1, 0.1, 11)
    assert np.any(bad.g(smile.grid(k.min(), k.max())) < -1e-9)
    w = np.maximum(bad.w(k), 1e-6)
    fit = smile.fit_svi(list(k), list(np.sqrt(w / 0.1)), 0.1)
    assert not fit.ok


def test_svi_converges_on_a_noisy_smile():
    # Two vol-point noise on a 30-day smile: the unscaled fit capped at 2000
    # evaluations stopped short here and reported "fit".
    T, k = 30 / 365, np.linspace(-0.12, 0.08, 15)
    iv = [0.4029, 0.3839, 0.3678, 0.3508, 0.3384, 0.3262, 0.3189, 0.3022, 0.3007, 0.286, 0.2838, 0.286,
          0.2839, 0.2855, 0.2927]
    fit = smile.fit_svi(list(k), iv, T)
    assert fit.ok and fit.rmse < 0.005


def test_calendar_arb_on_a_k_range():
    # Total variance crosses only out at k = -1: flagged on the wide grid, not over the quotes.
    near = smile.SVI(0.004, 0.02, -0.6, 0.0, 0.1)
    far = smile.SVI(0.006, 0.02, -0.5, 0.0, 0.1)
    assert smile.calendar_arb(near, 0.05, far, 0.1)
    assert not smile.calendar_arb(near, 0.05, far, 0.1, smile.grid(-0.1, 0.1))


def test_delta_vol_survives_a_runaway_fit():
    # A fit whose vol explodes away from the money: the 25-delta strike overflows.
    sv = smile.SVI(0.5, 10.0, 0.999, -0.5, 1e-4)
    fit = smile.Fit(sv, 0.0, True, [])
    sk = smile.Smile(25000.0, 1.5, [0.0], [1.0], fit).skew()
    assert sk["rr25"] is None


def test_smile_falls_back_to_raw_ivs():
    fit = smile.fit_svi([0.0], [0.2], 0.1)
    sm = smile.Smile(25000.0, 0.1, [-0.1, 0.0, 0.1], [0.22, 0.2, 0.19], fit)
    assert sm.atm() == pytest.approx(0.2)
    assert sm.iv_k(0.05) == pytest.approx(0.195)


def _strip(F, minutes, sigma, rate, lo=15000, hi=40000, step=100):
    T = minutes / vix.MINUTES_YEAR
    g = math.exp(rate * T)
    qs = []
    for k in range(lo, hi + step, step):
        c = black76.price(F, k, T, sigma, True, 1 / g)
        p = black76.price(F, k, T, sigma, False, 1 / g)
        qs.append(vix.Quote(k, c, c, p, p))
    return qs


def test_vix_flat_vol_equals_sigma():
    r = 0.065
    near = vix.term_variance(_strip(25000, 9 * 1440, 0.15, r), 9 * 1440, r)
    nxt = vix.term_variance(_strip(25100, 37 * 1440, 0.15, r), 37 * 1440, r)
    assert near.K0 == 25000
    assert vix.vix(near, nxt) == pytest.approx(15.0, abs=0.05)


def test_vix_stops_after_two_zero_bids():
    r, m = 0.065, 20 * 1440
    qs = _strip(25000, m, 0.15, r)
    # Zero the put bids at 20000 and 19900: nothing below 20000 may be used.
    for q in qs:
        if q.strike in (20000, 19900):
            q.put_bid = 0.0
    t = vix.term_variance(qs, m, r)
    full = vix.term_variance(_strip(25000, m, 0.15, r), m, r)
    assert t.strikes < full.strikes
