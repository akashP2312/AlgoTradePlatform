import io
from datetime import date

import pytest

from pmt_iv import clock, tte
from pmt_iv.calendar import Calendar

CSV = """# comment
date,segment,kind,open,close,note
2026-10-20,FO,holiday,,,Dussehra
2026-11-08,FO,special,18:00,19:00,Muhurat
2026-11-09,EQ,holiday,,,equity only
"""


def cal():
    return Calendar.read(io.StringIO(CSV))


def test_calendar_sessions():
    c = cal()
    assert c.session(date(2026, 10, 20)).kind == "holiday"
    assert c.session(date(2026, 10, 24)).kind == "weekend"
    s = c.session(date(2026, 11, 8))
    assert s.kind == "special" and s.trading and s.open.hour == 18
    assert c.session(date(2026, 11, 9)).kind == "normal"  # other segments ignored
    n = c.session(date(2026, 10, 19))
    assert n.trading and (n.open.hour, n.open.minute, n.close.hour, n.close.minute) == (9, 15, 15, 30)


def test_calendar_rejects_bad_rows():
    with pytest.raises(ValueError):
        Calendar.read(io.StringIO("2026-01-01,FO,special,,\n"))
    with pytest.raises(ValueError):
        Calendar.read(io.StringIO("2026-01-01,FO,weird\n"))


def test_trading_days_across_holiday_and_weekend():
    tc = tte.TradingClock(cal(), 0.25)
    # Mon 19 Oct 09:15 to Tue 27 Oct 15:30: Mon, Wed, Thu, Fri, Mon, Tue are sessions;
    # Tue 20 (holiday), Sat, Sun count a quarter each.
    now = clock.at(date(2026, 10, 19), "09:15")
    d = tc.days(now, tte.expiry_instant(date(2026, 10, 27)))
    assert d == pytest.approx(6 + 3 * 0.25)
    # Half a session left on expiry day.
    mid = clock.at(date(2026, 10, 27), "12:22:30")
    assert tc.days(mid, tte.expiry_instant(date(2026, 10, 27))) == pytest.approx(0.5)
    assert tc.days(tte.expiry_instant(date(2026, 10, 27)), mid) == 0.0


def test_special_session_counts_at_weekend_weight():
    tc = tte.TradingClock(cal(), 0.25)
    now = clock.at(date(2026, 11, 7), "00:00")  # Saturday
    assert tc.days(now, clock.at(date(2026, 11, 9), "00:00")) == pytest.approx(0.5)


def test_calendar_years():
    a = clock.at(date(2026, 1, 1), "15:30")
    assert tte.calendar_years(a, clock.at(date(2027, 1, 1), "15:30")) == pytest.approx(1.0)


@pytest.mark.parametrize("s,want", [("-21600s", -21600), ("1h30m", 5400), ("250ms", 0.25), ("0", 0), ("+2m", 120)])
def test_parse_duration(s, want):
    assert clock.parse_duration(s) == pytest.approx(want)


@pytest.mark.parametrize("s", ["", "5", "1x", "1h junk"])
def test_parse_duration_rejects(s):
    with pytest.raises(ValueError):
        clock.parse_duration(s)
