"""The news risk gate a strategy checks before every NEW entry.

newsmon publishes a risk state (NORMAL, CAUTION or NO_NEW_ENTRIES, with reasons and
an expiry) and the gateway serves it at /api/risk-state. This module turns it into an
entry decision. It is a filter, never a signal: it can only block or shrink a new
entry, it never opens one, and it never touches exits. Existing positions stay under
the strategy's own rules; the one exception is `force_close`, off by default, which
only sets `Decision.flatten` for the strategy to act on. Nothing here places orders.

An unknown state is not NORMAL: if the gateway is down, slow, says 503, sends
something unreadable, or the state is older than `stale_s`, the gate fails closed
(`on_unknown = "block"`) unless configured to allow.

Stdlib only and Python 3.9+, so a strategy can vendor this one file.
"""

from __future__ import annotations

import json
import time
import urllib.error
import urllib.request
from dataclasses import dataclass, field, fields
from typing import Any, Callable, Dict, List, Optional

NORMAL = "NORMAL"
CAUTION = "CAUTION"
NO_NEW_ENTRIES = "NO_NEW_ENTRIES"
UNKNOWN = "UNKNOWN"
LEVELS = (NORMAL, CAUTION, NO_NEW_ENTRIES)

VETO_NO_NEW = "news_no_new_entries"
VETO_CAUTION = "news_caution"
VETO_UNKNOWN = "news_unknown"


@dataclass(frozen=True)
class GateConfig:
    """The `news_gate:` section of a strategy's config."""

    enabled: bool = True
    url: str = "http://127.0.0.1:8091/api/risk-state"
    timeout_s: float = 2.0
    cache_s: float = 5.0
    # newsmon republishes every few seconds; the gateway marks a state over 60s old
    # stale, and the gate also checks the age itself.
    stale_s: float = 60.0
    # Size multiplier for a new entry under CAUTION; 0 blocks it like NO_NEW_ENTRIES.
    caution_size: float = 0.5
    # "block" (fail closed, the default) or "allow" when the state cannot be read.
    on_unknown: str = "block"
    # Off by default. When on, NO_NEW_ENTRIES sets Decision.flatten; the strategy
    # decides whether and how to close. The gate never closes anything itself.
    force_close: bool = False

    def __post_init__(self) -> None:
        if self.on_unknown not in ("block", "allow"):
            raise ValueError(f"news_gate.on_unknown must be block or allow, not {self.on_unknown!r}")
        if not 0 <= self.caution_size <= 1:
            raise ValueError(f"news_gate.caution_size must be within 0..1, not {self.caution_size}")
        for k in ("timeout_s", "stale_s"):
            if getattr(self, k) <= 0:
                raise ValueError(f"news_gate.{k} must be positive")
        if self.cache_s < 0:
            raise ValueError("news_gate.cache_s must not be negative")
        if not self.url.startswith(("http://", "https://")):
            raise ValueError(f"news_gate.url must be http(s), not {self.url!r}")

    @classmethod
    def from_dict(cls, d: Optional[Dict[str, Any]]) -> "GateConfig":
        """From a parsed YAML/JSON section; unknown keys are an error, not ignored."""
        d = dict(d or {})
        known = {f.name: f for f in fields(cls)}
        extra = sorted(set(d) - set(known))
        if extra:
            raise ValueError(f"news_gate: unknown keys {extra}")
        out: Dict[str, Any] = {}
        for k, v in d.items():
            typ = type(getattr(cls, k))
            if typ is bool:
                if not isinstance(v, bool):
                    raise ValueError(f"news_gate.{k} must be true or false")
                out[k] = v
            elif typ is float:
                if isinstance(v, bool) or not isinstance(v, (int, float)):
                    raise ValueError(f"news_gate.{k} must be a number")
                out[k] = float(v)
            else:
                if not isinstance(v, str):
                    raise ValueError(f"news_gate.{k} must be a string")
                out[k] = v
        return cls(**out)


@dataclass(frozen=True)
class Decision:
    """What the gate says about one new entry."""

    allow: bool
    size_mult: float  # 1 at NORMAL, caution_size at CAUTION, 0 when blocked
    state: str  # NORMAL, CAUTION, NO_NEW_ENTRIES or UNKNOWN
    veto: Optional[str] = None  # one of the VETO_* strings when blocked
    reasons: List[Dict[str, Any]] = field(default_factory=list)
    expires_at: Optional[int] = None  # epoch ms when the state may next ease
    stale: bool = False
    flatten: bool = False  # advisory, only with force_close; see GateConfig
    note: str = ""

    def vetoes(self) -> List[str]:
        """For appending to a strategy's own veto list: [] or [veto]."""
        return [self.veto] if self.veto else []


def _unknown(cfg: GateConfig, note: str, stale: bool = False) -> Decision:
    if cfg.on_unknown == "allow":
        return Decision(True, 1.0, UNKNOWN, None, stale=stale, note=f"{note}; on_unknown=allow")
    return Decision(False, 0.0, UNKNOWN, VETO_UNKNOWN, stale=stale, note=note)


def decide(payload: Optional[Dict[str, Any]], now_ms: int, cfg: GateConfig, err: str = "") -> Decision:
    """The entry decision for one /api/risk-state payload (None when unreadable)."""
    if not cfg.enabled:
        return Decision(True, 1.0, UNKNOWN, note="news_gate disabled")
    if payload is None:
        return _unknown(cfg, err or "no risk state")
    if not isinstance(payload, dict):
        return _unknown(cfg, "risk state is not an object")
    state, ts = payload.get("state"), payload.get("ts")
    if state not in LEVELS or isinstance(ts, bool) or not isinstance(ts, (int, float)):
        return _unknown(cfg, f"unreadable risk state {str(payload)[:120]}")
    age = (now_ms - ts) / 1000
    if payload.get("stale") is True or age > cfg.stale_s:
        return _unknown(cfg, f"risk state {age:.0f}s old (newsmon down?)", stale=True)

    raw = payload.get("reasons")
    reasons = [r for r in raw if isinstance(r, dict)] if isinstance(raw, list) else []
    exp = payload.get("expires_at")
    exp = int(exp) if isinstance(exp, (int, float)) and not isinstance(exp, bool) else None
    why = "; ".join(str(r.get("msg") or r.get("name") or "") for r in reasons[:3])
    common = dict(state=state, reasons=reasons, expires_at=exp)

    if state == NO_NEW_ENTRIES:
        return Decision(False, 0.0, veto=VETO_NO_NEW, flatten=cfg.force_close, note=why, **common)
    if state == CAUTION:
        if cfg.caution_size <= 0:
            return Decision(False, 0.0, veto=VETO_CAUTION, note=why, **common)
        return Decision(True, cfg.caution_size, note=why, **common)
    return Decision(True, 1.0, **common)


def fetch(cfg: GateConfig) -> "tuple[Optional[Dict[str, Any]], str]":
    """GET the risk state: (payload, "") or (None, why it failed). Never raises."""
    req = urllib.request.Request(cfg.url, headers={"Accept": "application/json", "User-Agent": "pmt-newsgate/1"})
    try:
        with urllib.request.urlopen(req, timeout=cfg.timeout_s) as r:
            body = r.read(1 << 20)
    except urllib.error.HTTPError as e:
        return None, f"gateway HTTP {e.code}"
    except Exception as e:  # refused, timeout, DNS, TLS: all mean unknown
        return None, f"gateway unreachable: {type(e).__name__}: {e}"
    try:
        return json.loads(body), ""
    except ValueError:
        return None, "gateway sent invalid JSON"


class NewsGate:
    """Polls /api/risk-state, caching for cache_s, and decides new entries.

    Exits are never gated: call `entry()` only for a new position. A strategy that
    already listens on the gateway WS or NATS news.risk can call decide() directly.
    """

    def __init__(self, cfg: Optional[GateConfig] = None, clock: Callable[[], float] = time.time,
                 fetcher: Callable[[GateConfig], "tuple[Optional[Dict[str, Any]], str]"] = fetch):
        self.cfg = cfg or GateConfig()
        self._clock = clock
        self._fetch = fetcher
        self._at = float("-inf")
        self._got: "tuple[Optional[Dict[str, Any]], str]" = (None, "not fetched")

    def entry(self) -> Decision:
        now = self._clock()
        if self.cfg.enabled and now - self._at >= self.cfg.cache_s:
            self._got, self._at = self._fetch(self.cfg), now
        payload, err = self._got
        return decide(payload, int(now * 1000), self.cfg, err)

    def vetoes(self) -> List[str]:
        return self.entry().vetoes()
