"""The ivengine's view of config/pmt.yaml: the NATS URL, the option underlyings, the
ivengine section and the risk section (kept raw; risksvc parses it). ${VAR} expands
from the environment, unset variables to the empty string, as Go's os.ExpandEnv does."""

from __future__ import annotations

import os
import re
from dataclasses import dataclass, field

import yaml

from .clock import parse_duration

_VAR = re.compile(r"\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)")


def expand(s: str) -> str:
    return _VAR.sub(lambda m: os.environ.get(m.group(1) or m.group(2), ""), s)


@dataclass
class Underlying:
    name: str
    exchange: str  # derivatives exchange, NFO


@dataclass
class Config:
    nats_url: str
    underlyings: list[Underlying]
    rate: float = 0.065
    interval: float = 1.0
    idle_every: float = 10.0
    max_spread: float = 0.30
    weekend_weight: float = 0.25
    overnight_weight: float = 0.0
    forward_strikes: int = 5
    vix_roll_days: int = 3
    vix_underlying: str = "NIFTY"
    calendar: str = "config/calendar/holidays.csv"
    risk: dict = field(default_factory=dict)  # the risk: section, parsed by risksvc
    dir: str = field(default=".")


def path() -> str:
    return os.environ.get("PMT_CONFIG") or "config/pmt.yaml"


def _secs(v, default: float) -> float:
    if v is None or v == "":
        return default
    if isinstance(v, (int, float)):
        return float(v)
    return parse_duration(str(v))


def load(p: str | None = None) -> Config:
    p = p or path()
    with open(p) as f:
        raw = yaml.safe_load(expand(f.read())) or {}
    root = os.path.dirname(os.path.dirname(os.path.abspath(p)))
    iv = raw.get("ivengine") or {}
    unds = [
        Underlying(u["name"], u.get("deriv_exchange") or "NFO")
        for u in (raw.get("universe") or {}).get("underlyings") or []
    ]
    c = Config(
        nats_url=raw.get("nats_url") or os.environ.get("NATS_URL") or "nats://127.0.0.1:4222",
        underlyings=unds,
        rate=float(iv.get("rate", 0.065)),
        interval=_secs(iv.get("interval"), 1.0),
        idle_every=_secs(iv.get("idle_every"), 10.0),
        max_spread=float(iv.get("max_spread", 0.30)),
        weekend_weight=float(iv.get("weekend_weight", 0.25)),
        overnight_weight=float(iv.get("overnight_weight", 0.0)),
        forward_strikes=int(iv.get("forward_strikes", 5)),
        vix_roll_days=int(iv.get("vix_roll_days", 3)),
        vix_underlying=str(iv.get("vix_underlying", "NIFTY")),
        calendar=iv.get("calendar") or "config/calendar/holidays.csv",
        risk=dict(raw.get("risk") or {}),
        dir=root,
    )
    if not os.path.isabs(c.calendar):
        c.calendar = os.path.join(root, c.calendar)
    if not c.underlyings:
        raise ValueError("config: universe.underlyings is empty")
    if not 0 < c.max_spread <= 1:
        raise ValueError("config: ivengine.max_spread must be in (0, 1]")
    if c.interval <= 0 or c.idle_every < c.interval:
        raise ValueError("config: ivengine.interval must be > 0 and <= idle_every")
    return c
