import math
from datetime import date, timedelta

import pytest

from pmt_iv import ivhist
from pmt_iv.ivhist import Point, compute, interp, ranks


def test_interp_total_variance_and_no_extrapolation():
    a, b = Point(10, 0.12, -0.02, 0.004), Point(30, 0.15, -0.04, 0.006)
    p = interp([a, b], 21)
    w = 0.12 ** 2 * 10 + (11 / 20) * (0.15 ** 2 * 30 - 0.12 ** 2 * 10)
    assert p.atm == pytest.approx(math.sqrt(w / 21))
    assert p.rr25 == pytest.approx(-0.02 + 0.55 * -0.02) and p.bf25 == pytest.approx(0.004 + 0.55 * 0.002)
    assert interp([a, b], 5) is None and interp([a, b], 42) is None
    assert interp([a, b], 10).atm == 0.12  # on an expiry: that fit
    assert interp([Point(10, 0.12, None, None), b], 21).rr25 is None


def test_ranks_use_earlier_days_only():
    x = [float(i) for i in range(100)] + [None, 50.0]
    r = ranks(x, window=80, min_hist=60)
    assert r[59].rank is None and r[59].n == 59
    assert r[60].n == 60 and r[60].rank == 1.0 and r[60].pct == 1.0
    assert r[100].rank is None and r[100].n == 0  # missing: not ranked, not counted
    last = r[101]  # window holds 20..99
    assert last.n == 80 and last.rank == pytest.approx(30 / 79) and last.pct == pytest.approx(30 / 80)
    flat = ranks([1.0] * 70, min_hist=60)
    assert flat[-1].rank == 0.5 and flat[-1].pct == 0.0


def test_compute_premia_and_rows():
    d0 = date(2026, 1, 1)
    days = [d0 + timedelta(days=i) for i in range(80)]
    fits = {d: [Point(15, 0.14, -0.03, 0.005), Point(35, 0.14, -0.03, 0.005)] for d in days}
    har = [(d, 0.11, 0.12 if i < 70 else None, 0.10) for i, d in enumerate(days)]
    vix = {d: 15.0 for d in days}
    res = compute("NIFTY", fits, har, vix)
    tenors = {r["tenor"] for r in res.cm_rows}
    assert tenors == {21}  # 5, 42 and 63 are not bracketed
    v = res.vrp_rows[-1]
    assert v["iv30"] == pytest.approx(0.14) and v["iv_ante"] == pytest.approx(0.03)
    assert v["vix_ante"] == pytest.approx(0.04) and v["var_vix_ante"] == pytest.approx(0.15 ** 2 - 0.11 ** 2)
    assert v["iv_post"] is None and res.vrp_rows[0]["iv_post"] == pytest.approx(0.02)
    assert v["n_iv"] == 79 and v["iv_rank"] == 0.5 and v["n_vix"] == 79
    assert compute("BANKNIFTY", fits, har, None).vrp_rows[-1]["vix"] is None
    assert any("premium ex ante" in ln for ln in ivhist.report(res))


def test_upserts():
    assert "ON CONFLICT (trade_date, underlying, tenor)" in ivhist._CM
    assert "ON CONFLICT (trade_date, underlying, horizon)" in ivhist._VRP
