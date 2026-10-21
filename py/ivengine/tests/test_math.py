import json
import math
from pathlib import Path

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


def test_svi_fits_live_smiles():
    # OTM ivs from 2026-10-21's session that the fit used to reject: the solver
    # hit its evaluation limit on a good fit, or the free fit bent the call wing
    # into butterfly arbitrage just past the last quote.
    data = json.loads((Path(__file__).parent / "data" / "smiles_20261021.json").read_text())
    for name, d in data.items():
        fit = smile.fit_svi(d["k"], d["iv"], d["T"])
        assert fit.ok, (name, fit.arb)
        assert fit.rmse < 0.003, name
        k = np.asarray(d["k"])
        grid = np.linspace(k.min() - smile.ARB_MARGIN, k.max() + smile.ARB_MARGIN, 301)
        assert fit.svi.g(grid).min() >= -1e-9, name
