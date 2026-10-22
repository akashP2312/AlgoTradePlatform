import math
from datetime import date, timedelta

import numpy as np
import pytest

from pmt_iv import event
from pmt_iv.event import Fit, Moves, compute, kinks, load_events


def test_kinks_flag_the_segment_with_extra_variance():
    t = lambda d: d / 252
    fits = [Fit(date(2026, 1, 1), 5, t(5), 0.12), Fit(date(2026, 1, 2), 10, t(10), 0.12),
            Fit(date(2026, 1, 3), 15, t(15), 0.15)]
    k = kinks(fits)
    assert k[0][0] == pytest.approx(0.12) and k[1][0] == pytest.approx(0.12)
    r3 = (0.15 ** 2 * 15 - 0.12 ** 2 * 10) / 5
    assert k[2][0] == pytest.approx(math.sqrt(r3))
    assert k[0][1] == 0 and k[1][1] == 0  # the median rate is 0.12^2
    assert k[2][1] == pytest.approx((r3 - 0.0144) * t(5))
    assert kinks(fits[:2])[0][1] is None  # too few expiries for a typical rate
    inv = kinks([Fit(date(2026, 1, 1), 5, t(5), 0.2), Fit(date(2026, 1, 2), 10, t(10), 0.1)])
    assert inv[1] == (None, None)


def test_moves_hist_and_realized():
    d0 = date(2026, 1, 1)
    dates = [d0 + timedelta(days=i) for i in range(10)]
    closes = [100 * math.exp(0.01 * (i % 2)) for i in range(10)]  # alternating +-1%
    m = Moves(dates, closes)
    h, n = m.hist(dates[9], 1, back=5)
    assert n == 5 and h == pytest.approx(0.01)
    h2, n2 = m.hist(dates[9], 2, back=5)
    assert n2 == 4 and h2 == pytest.approx(0.0)
    assert m.hist(dates[0], 1) == (None, 0)
    assert m.realized(dates[2], dates[5]) == pytest.approx(0.01)
    assert m.realized(dates[2], dates[9] + timedelta(days=1)) is None  # not expired yet
    assert m.realized(dates[2] - timedelta(days=30), dates[5]) is None


def test_compute_and_events(tmp_path):
    p = tmp_path / "events.csv"
    p.write_text("# verify\ndate,underlying,name\n2026-01-08,*,policy\n2026-01-08,BANKNIFTY,other\n")
    ev = load_events(str(p), "NIFTY")
    assert ev == [(date(2026, 1, 8), "policy")]
    assert load_events(str(tmp_path / "none.csv"), "NIFTY") == []
    d0 = date(2026, 1, 1)
    dates = [d0 + timedelta(days=i) for i in range(30)]
    m = Moves(dates, list(100 + np.arange(30.0)))
    fits = {dates[0]: [Fit(dates[5], 5, 5 / 252, 0.12), Fit(dates[10], 10, 10 / 252, 0.12)]}
    rows = compute("NIFTY", fits, m, ev)
    assert rows[0]["implied_move"] == pytest.approx(0.12 * math.sqrt(5 / 252) * math.sqrt(2 / math.pi))
    assert rows[0]["events"] is None and rows[1]["events"] == "policy"
    assert rows[1]["real_move"] == pytest.approx(math.log(110 / 100))
    assert any("priced vs realized" in ln for ln in event.report(rows))
    assert "ON CONFLICT (trade_date, underlying, expiry)" in event._EV
