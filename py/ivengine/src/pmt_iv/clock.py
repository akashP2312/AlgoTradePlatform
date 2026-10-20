"""The engine's single source of "now": the wall clock in IST, shifted by
PMT_CLOCK_OFFSET in a dry run, exactly as the Go services do (internal/clock)."""

from __future__ import annotations

import os
import re
import time
from datetime import date, datetime, timedelta, timezone

# India has no DST, so a fixed zone matches Asia/Kolkata and needs no tzdata.
IST = timezone(timedelta(hours=5, minutes=30), "IST")

_UNITS = {"ns": 1e-9, "us": 1e-6, "µs": 1e-6, "μs": 1e-6, "ms": 1e-3, "s": 1.0, "m": 60.0, "h": 3600.0}
_PART = re.compile(r"(\d+(?:\.\d*)?|\.\d+)(ns|us|µs|μs|ms|s|m|h)")


def parse_duration(s: str) -> float:
    """Parses a Go duration string ("-21600s", "1h30m", "250ms") into seconds."""
    s = s.strip()
    sign = 1.0
    if s and s[0] in "+-":
        sign, s = (-1.0 if s[0] == "-" else 1.0), s[1:]
    if s == "0":
        return 0.0
    pos, total = 0, 0.0
    for m in _PART.finditer(s):
        if m.start() != pos:
            break
        total += float(m.group(1)) * _UNITS[m.group(2)]
        pos = m.end()
    if not s or pos != len(s):
        raise ValueError(f"duration {s!r}: want a Go duration such as -21600s or 1h30m")
    return sign * total


_offset = parse_duration(os.environ["PMT_CLOCK_OFFSET"]) if os.environ.get("PMT_CLOCK_OFFSET", "").strip() else 0.0


def offset() -> float:
    """The dry-run shift in seconds (zero in production)."""
    return _offset


def set_offset(seconds: float) -> None:
    """For tests."""
    global _offset
    _offset = seconds


def now() -> datetime:
    return datetime.fromtimestamp(time.time() + _offset, IST)


def now_ms() -> int:
    return int((time.time() + _offset) * 1000)


def at(d: date, hms: str) -> datetime:
    """The instant hh:mm[:ss] IST on day d."""
    parts = [int(p) for p in hms.split(":")]
    if len(parts) == 2:
        parts.append(0)
    hh, mm, ss = parts
    return datetime(d.year, d.month, d.day, hh, mm, ss, tzinfo=IST)


def ms(t: datetime) -> int:
    return int(t.timestamp() * 1000)
