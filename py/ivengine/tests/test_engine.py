import io
import json
import math
from datetime import date

import pytest

from pmt_iv import black76, clock, tte
from pmt_iv.calendar import Calendar
from pmt_iv.engine import Book, Engine, Params
from pmt_iv.smile import SVI

NOW = clock.at(date(2026, 10, 19), "11:00")
EXPIRIES = [date(2026, 10, 27), date(2026, 11, 3), date(2026, 11, 24), date(2026, 12, 29)]
F0 = 25050.0
TRUE = SVI(0.0, 0.03, -0.5, 0.01, 0.1)  # in total variance per unit trading time


def tick(c, bid, ask, ltp=None, recv=None):
    return {"c": c, "b": bid, "a": ask, "p": ltp if ltp is not None else (bid + ask) / 2, "r": recv or clock.ms(NOW) - 400}


def build(spread=0.5):
    cal = Calendar.read(io.StringIO("2026-10-20,FO,holiday,,,\n2026-11-24,FO,holiday,,,\n"))
    eng = Engine(Book(["NIFTY"]), cal, Params(rate=0.065, max_spread=0.3), 0.25)
    book = eng.book
    book.update(tick("NSE|NIFTY|||IDX", 0, 0, ltp=25000.0))
    book.update(tick("NSE|INDIAVIX|||IDX", 0, 0, ltp=14.5))
    for d in EXPIRIES:
        exp = tte.expiry_instant(d)
        T = eng.clock.years(NOW, exp)
        df = math.exp(-0.065 * tte.calendar_years(NOW, exp))
        for K in range(22000, 28050, 50):
            k = math.log(K / F0)
            s = math.sqrt(float(TRUE.w(k)) / 1.0 + 0.012)  # flat-ish base vol plus the SVI smile
            for kind, call in (("CE", True), ("PE", False)):
                p = black76.price(F0, K, T, s, call, df)
                if p < 0.2:
                    continue
                book.update(tick(f"NFO|NIFTY|{d.isoformat()}|{K}|{kind}", p - spread / 2, p + spread / 2))
        book.update(tick(f"NFO|NIFTY|{d.isoformat()}||FUT", F0 - 1, F0 + 1))
    return eng


def test_book_ignores_other_names_and_bad_keys():
    b = Book(["NIFTY"])
    assert b.update({"c": "NFO|BANKNIFTY|2026-10-27|50000|CE"}) is None
    assert b.update({"c": "garbage"}) is None
    assert b.update({}) is None
    assert b.update(tick("NFO|NIFTY|2026-10-27|25000|CE", 10, 11)) == ("NIFTY", date(2026, 10, 27))
    assert b.update(tick("NSE|INDIAVIX|||IDX", 0, 0, 14.0)) == "INDIAVIX"


def test_expiry_payload():
    eng = build()
    res = eng.expiry("NIFTY", EXPIRIES[1], NOW)
    pl = res.payload
    json.dumps(pl, allow_nan=False)
    assert pl["fsrc"] == "parity"
    assert pl["f"] == pytest.approx(F0, abs=0.5)
    assert res.ok and pl["fit"]["ok"], pl["fit"]
    atm_true = math.sqrt(float(TRUE.w(0.0)) + 0.012)
    assert pl["atm"] == pytest.approx(atm_true, abs=2e-3)
    assert pl["rr25"] < 0
    row = next(r for r in pl["rows"] if r["k"] == 25000.0)
    assert row["c"]["iv"] and row["p"]["iv"]
    assert row["c"]["delta"] - row["p"]["delta"] == pytest.approx(1.0, abs=1e-9)
    assert pl["age"]["med"] == 400


def test_spread_filter_drops_wide_quotes():
    eng = build()
    d = EXPIRIES[0]
    eng.book.update(tick(f"NFO|NIFTY|{d.isoformat()}|27000|CE", 1.0, 5.0))
    row = next(r for r in eng.expiry("NIFTY", d, NOW).payload["rows"] if r["k"] == 27000.0)
    assert row["c"]["mid"] is None and row["c"]["iv"] is None and row["c"]["spr"] > 0.3


def test_stale_quotes_leave_the_fit():
    eng = build()
    d = EXPIRIES[1]
    # A wing strike left at a stale, mispriced quote, as when the ticker's window moves.
    eng.book.update(tick(f"NFO|NIFTY|{d.isoformat()}|27500|CE", 30.0, 30.5, recv=clock.ms(NOW) - 600_000))
    pl = eng.expiry("NIFTY", d, NOW).payload
    row = next(r for r in pl["rows"] if r["k"] == 27500.0)
    assert row["c"]["b"] == 30.0 and row["c"]["mid"] is None and row["c"]["iv"] is None
    assert row["c"]["age"] == 600_000
    assert pl["fit"]["ok"], pl["fit"]


def test_expired_and_missing():
    eng = build()
    assert eng.expiry("NIFTY", date(2026, 10, 1), NOW) is None
    assert eng.expiry("NIFTY", EXPIRIES[0], tte.expiry_instant(EXPIRIES[0])) is None


def test_term_and_vix():
    eng = build()
    rs = [eng.expiry("NIFTY", d, NOW) for d in EXPIRIES]
    term = Engine.term("NIFTY", rs, clock.ms(NOW))
    assert [r["x"] for r in term["rows"]] == [d.isoformat() for d in EXPIRIES]
    assert term["rows"][0]["calarb"] is None
    assert Engine.monthlies(EXPIRIES) == [date(2026, 10, 27), date(2026, 11, 24), date(2026, 12, 29)]
    v = eng.vix("NIFTY", NOW)
    assert v["near"]["x"] == "2026-10-27" and v["next"]["x"] == "2026-11-24"
    assert 5 < v["vix"] < 40 and v["nse"] == 14.5
    assert v["diff"] == pytest.approx(v["vix"] - 14.5, abs=1e-3)
    # Three days before the near monthly, the pair rolls.
    late = clock.at(date(2026, 10, 24), "11:00")
    assert eng.vix("NIFTY", late)["near"]["x"] == "2026-11-24"
