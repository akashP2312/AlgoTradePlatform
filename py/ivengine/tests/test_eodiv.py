import io
import math
from datetime import date

import pytest

from pmt_iv import black76, config, tte
from pmt_iv.calendar import Calendar
from pmt_iv.eodiv import _UPSERT, Contract, Filters, book_for, compute
from pmt_iv.engine import Engine, Params

DAY = date(2026, 10, 19)
X1, X2 = date(2026, 10, 27), date(2026, 11, 24)
F0, VOL, R = 25050.0, 0.14, 0.065


def cfg():
    return config.Config(nats_url="", underlyings=[], rate=R)


def bhav(cal):
    close = cal.session(DAY).close
    eng = Engine(None, cal, Params(), 0.25)
    out = [Contract(DAY, None, "FUT", F0, 1000, 25000.0)]  # an expiring future, skipped
    for x in (X1, X2):
        exp = tte.expiry_instant(x)
        T = eng.clock.years(close, exp)
        df = math.exp(-R * tte.calendar_years(close, exp))
        out.append(Contract(x, None, "FUT", F0, 5000, 25000.0))
        for K in range(20000, 30050, 100):
            for kind, call in (("CE", True), ("PE", False)):
                out.append(Contract(x, float(K), kind, round(black76.price(F0, K, T, VOL, call, df), 2), 10, 25000.0))
    out.append(Contract(X1, 25000.0, "CE", 999.0, 0, 25000.0))  # untraded: must not replace the traded one
    return out


@pytest.fixture
def cal():
    return Calendar.read(io.StringIO("2026-10-20,FO,holiday,,,\n"))


def test_flat_vol_comes_back(cal):
    rows = compute("NIFTY", DAY, bhav(cal), cal, cfg(), Filters())
    assert [r["expiry"] for r in rows] == [X1, X2]
    for r in rows:
        assert r["source"] == "bhav" and r["model_version"] == "bhav-settle-1"
        assert r["fsrc"] == "parity"
        assert r["fwd"] == pytest.approx(F0, abs=1.0)
        assert r["atm_iv"] == pytest.approx(VOL, abs=0.002)
        assert abs(r["rr25"]) < 0.003
        assert r["spot"] == 25000.0
        assert r["ts"] == cal.session(DAY).close
        assert r["n_strikes"] > 20


def test_filters(cal):
    cs = bhav(cal)
    book = book_for("NIFTY", DAY, cs, Filters(max_kx=0.05))
    ch = book.chains[("NIFTY", X1)]
    ks = set(ch.calls) | set(ch.puts)
    assert min(ks) >= F0 * math.exp(-0.05) and max(ks) <= F0 * math.exp(0.05)
    assert ch.calls[25000.0].bid != 999.0  # zero-volume row dropped
    assert all(q.bid >= 0.5 for q in ch.calls.values())  # min premium
    assert ("NIFTY", DAY) not in book.chains


def test_no_chain_no_rows(cal):
    assert compute("NIFTY", DAY, [], cal, cfg(), Filters()) == []


def test_upsert_covers_every_column():
    assert _UPSERT.count("%(") == 22
    assert "ON CONFLICT (trade_date, underlying, expiry, source)" in _UPSERT
    assert "source = excluded" not in _UPSERT
