"""Time to expiry.

Implied vols use a trading-time clock: each normal session counts one day, spread
evenly over 09:15-15:30, and each weekend, holiday or special-session day counts
`weekend_weight` of a day, spread over its 24 hours. The year is 252 trading days
plus 113 others at that weight, so a vol quoted here is comparable to an annual
calendar vol over a typical year. The weight (0.25) is a starting point to verify
against realised weekend moves; nothing downstream assumes its value.

Discounting and the VIX use plain calendar time (minutes / 525600).

Options expire at the close, 15:30 IST, on the expiry date.
"""

from __future__ import annotations

from datetime import date, datetime, timedelta

from . import clock
from .calendar import Calendar

EXPIRY_TIME = "15:30"
TRADING_DAYS = 252  # verify against the NSE count for the year
OTHER_DAYS = 113
CAL_YEAR_SECONDS = 365 * 86400.0


def expiry_instant(d: date) -> datetime:
    return clock.at(d, EXPIRY_TIME)


def calendar_years(now: datetime, expiry: datetime) -> float:
    return (expiry - now).total_seconds() / CAL_YEAR_SECONDS


def _overlap(a0: datetime, a1: datetime, b0: datetime, b1: datetime) -> float:
    return max(0.0, (min(a1, b1) - max(a0, b0)).total_seconds())


class TradingClock:
    def __init__(self, cal: Calendar, weekend_weight: float = 0.25):
        self.cal = cal
        self.w = weekend_weight
        self.year_days = TRADING_DAYS + weekend_weight * OTHER_DAYS

    def days(self, now: datetime, expiry: datetime) -> float:
        """Trading-time days from now to expiry (zero once expired)."""
        if expiry <= now:
            return 0.0
        total = 0.0
        d = now.astimezone(clock.IST).date()
        last = expiry.astimezone(clock.IST).date()
        while d <= last:
            s = self.cal.session(d)
            if s.kind == "normal":
                span = (s.close - s.open).total_seconds()
                total += _overlap(now, expiry, s.open, s.close) / span
            else:
                d0 = clock.at(d, "00:00")
                total += self.w * _overlap(now, expiry, d0, d0 + timedelta(days=1)) / 86400.0
            d += timedelta(days=1)
        return total

    def years(self, now: datetime, expiry: datetime) -> float:
        return self.days(now, expiry) / self.year_days
