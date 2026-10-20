"""F&O trading sessions from config/calendar/holidays.csv, the same seed and rules
as the Go calendar package: weekends and listed holidays are closed, special
sessions carry their own hours (even on a weekend), every other day is 09:15-15:30."""

from __future__ import annotations

import csv
from dataclasses import dataclass
from datetime import date, datetime

from . import clock

DEFAULT_OPEN = "09:15"
DEFAULT_CLOSE = "15:30"


@dataclass(frozen=True)
class Session:
    day: date
    kind: str  # normal | holiday | weekend | special
    open: datetime | None = None
    close: datetime | None = None

    @property
    def trading(self) -> bool:
        return self.kind in ("normal", "special")


class Calendar:
    def __init__(self, entries: dict[date, tuple[str, str, str]] | None = None):
        self._days = entries or {}

    @classmethod
    def load(cls, path: str) -> "Calendar":
        with open(path, newline="") as f:
            return cls.read(f)

    @classmethod
    def read(cls, lines) -> "Calendar":
        days: dict[date, tuple[str, str, str]] = {}
        rows = csv.reader(l for l in lines if not l.lstrip().startswith("#"))
        for i, row in enumerate(rows):
            if not row or (i == 0 and row[0] == "date"):
                continue
            if len(row) < 3:
                raise ValueError(f"calendar: row {i + 1}: too few fields")
            seg = row[1].strip()
            if seg and seg != "FO":
                continue
            kind = row[2].strip()
            op = row[3].strip() if len(row) > 3 else ""
            cl = row[4].strip() if len(row) > 4 else ""
            if kind not in ("holiday", "special"):
                raise ValueError(f"calendar: row {i + 1}: kind {kind!r}")
            if kind == "special" and not (op and cl):
                raise ValueError(f"calendar: row {i + 1}: special session needs open and close")
            days[date.fromisoformat(row[0].strip())] = (kind, op, cl)
        return cls(days)

    def session(self, d: date) -> Session:
        e = self._days.get(d)
        if e and e[0] == "special":
            return Session(d, "special", clock.at(d, e[1]), clock.at(d, e[2]))
        if d.weekday() >= 5:
            return Session(d, "weekend")
        if e and e[0] == "holiday":
            return Session(d, "holiday")
        return Session(d, "normal", clock.at(d, DEFAULT_OPEN), clock.at(d, DEFAULT_CLOSE))
