"""The riskengine service: follows book.positions, the ivengine's smiles
(analytics.<und>.iv.<yyyymmdd>), each open leg's own tick and the spot-vol beta
(risk.beta, from the spotvol job), and every interval reprices the book with
risk.Risk, publishing the result on risk.book and putting it to KV risk.

It only reads positions, ticks and analytics; it has no path to orders.
"""

from __future__ import annotations

import asyncio
import json
import logging
import math
import signal
import time

import nats
from nats.js.errors import BucketNotFoundError, KeyNotFoundError

from . import clock, config
from .calendar import Calendar
from .engine import Quote
from .risk import Beta, Leg, Params, Risk, Surface, day_pnl, legs_from_book, quote_from_tick

log = logging.getLogger("riskengine")

SERVICE = "riskengine"
HEALTH_EVERY = 5.0
BOOK_STALE_MS = 15_000  # the book service polls every 5s
OUT_KEY = "book"
BETA_KEY = "beta"


def _dumps(v: dict) -> bytes:
    return json.dumps(v, separators=(",", ":"), allow_nan=False).encode()


def params(cfg: config.Config) -> tuple[Params, float]:
    """Params and the publish interval from the risk: section of pmt.yaml."""
    r = cfg.risk
    d = Params()

    def num(k, dflt, lo=None):
        v = r.get(k, dflt)
        if not isinstance(v, (int, float)) or not math.isfinite(v) or (lo is not None and v < lo):
            raise ValueError(f"config: risk.{k} must be a number" + (f" >= {lo}" if lo is not None else ""))
        return float(v)

    def steps(k, dflt):
        v = r.get(k)
        if v is None:
            return dflt
        if not isinstance(v, list) or not v or not all(isinstance(x, (int, float)) for x in v):
            raise ValueError(f"config: risk.{k} must be a list of numbers")
        return tuple(sorted({float(x) for x in v} | {0.0}))

    und = r.get("und_beta") or {}
    if not isinstance(und, dict) or not all(isinstance(v, (int, float)) and v > 0 for v in und.values()):
        raise ValueError("config: risk.und_beta must map underlyings to positive numbers")
    p = Params(
        rate=cfg.rate,
        weekend_weight=cfg.weekend_weight,
        overnight_weight=cfg.overnight_weight,
        spot_steps=steps("spot_steps", d.spot_steps),
        vol_rows=steps("vol_rows", d.vol_rows),
        vol_cap=num("vol_cap", d.vol_cap, 1.0),
        stale_ms=int(num("stale_ms", d.stale_ms, 1000)),
        wide=num("wide", d.wide, 0.0),
        widen=num("widen", d.widen, 0.0),
        ref=str(r.get("ref") or d.ref),
        ref_lot=int(num("ref_lot", d.ref_lot, 1)),
        beta_dn=num("beta_dn", d.beta_dn),
        beta_up=num("beta_up", d.beta_up),
        und_beta={str(k): float(v) for k, v in und.items()},
    )
    interval = config._secs(r.get("interval"), 1.0)
    if interval <= 0:
        raise ValueError("config: risk.interval must be > 0")
    return p, interval


class Service:
    def __init__(self, cfg: config.Config):
        self.cfg = cfg
        self.p, self.interval = params(cfg)
        self.risk = Risk(Calendar.load(cfg.calendar), self.p)
        self.beta = Beta.default(self.p)
        self.book: dict | None = None
        self.legs: list[Leg] = []
        self.rows: list[dict] = []  # every book.net row, closed ones too, for the day's P&L
        self.surfaces: dict[tuple, Surface] = {}
        self.quotes: dict[str, Quote] = {}
        self.leg_subs: dict[str, object] = {}  # tick subject -> subscription
        self.dirty = True
        self.nc = None
        self.js = None
        self.kv = None
        self.lv = None
        self.last: dict | None = None
        self.stats = {"runs": 0, "published": 0, "errors": 0, "last_ms": 0.0}
        self.last_error = ""
        self.stop = asyncio.Event()

    # ---- input ------------------------------------------------------------

    def apply_book(self, data: bytes) -> None:
        try:
            b = json.loads(data)
        except ValueError:
            return
        if not isinstance(b, dict):
            return
        self.book = b
        self.legs = legs_from_book(b)
        self.rows = [r for r in b.get("net") or [] if isinstance(r, dict)]
        self.dirty = True

    def apply_surface(self, data: bytes) -> None:
        try:
            s = Surface.from_payload(json.loads(data))
        except (ValueError, KeyError, TypeError):
            return
        if s is not None:
            self.surfaces[(s.und, s.expiry)] = s
            self.dirty = True

    def apply_tick(self, data: bytes) -> None:
        try:
            t = json.loads(data)
        except ValueError:
            return
        c = t.get("c") if isinstance(t, dict) else None
        if c:
            self.quotes[c] = quote_from_tick(t)
            self.dirty = True

    def apply_beta(self, data: bytes) -> None:
        try:
            self.beta = Beta.from_payload(json.loads(data), self.p)
            self.dirty = True
        except (ValueError, TypeError, AttributeError) as e:
            self.note_error(f"beta: {e!r}")

    async def _on_book(self, msg) -> None:
        self.apply_book(msg.data)
        await self.follow_legs()

    async def _on_surface(self, msg) -> None:
        self.apply_surface(msg.data)

    async def _on_tick(self, msg) -> None:
        self.apply_tick(msg.data)

    async def _on_beta(self, msg) -> None:
        self.apply_beta(msg.data)

    async def follow_legs(self) -> None:
        """Subscribes to each open leg's tick subject (and drops closed ones), seeding
        a new leg from KV lastvalue so it is priced at once."""
        want = {lg.subj for lg in self.legs if lg.subj}
        for s in [s for s in self.leg_subs if s not in want]:
            try:
                await self.leg_subs.pop(s).unsubscribe()
            except Exception as e:
                log.warning("unsubscribe %s: %s", s, e)
        for s in sorted(want - set(self.leg_subs)):
            self.leg_subs[s] = await self.nc.subscribe(s, cb=self._on_tick)
            if self.lv is not None:
                try:
                    e = await self.lv.get(s.removeprefix("tick."))
                    if e.value:
                        self.apply_tick(e.value)
                except KeyNotFoundError:
                    pass
                except Exception as e:
                    self.note_error(f"lastvalue {s}: {e!r}")

    async def _bucket(self, name: str, wait: bool):
        for attempt in range(30 if wait else 1):
            try:
                return await self.js.key_value(name)
            except BucketNotFoundError:
                # The Go services create the buckets; wait for them rather than create a
                # bucket with a config that could drift from internal/bus.
                if attempt == 0:
                    log.warning("bucket %s not found yet%s", name, "; waiting" if wait else "")
                if wait:
                    await asyncio.sleep(2)
        return None

    async def _get(self, kv, key: str) -> bytes | None:
        if kv is None:
            return None
        try:
            e = await kv.get(key)
            return e.value or None
        except KeyNotFoundError:
            return None

    async def seed(self) -> None:
        an = await self._bucket("analytics", False)
        n = 0
        if an is not None:
            w = await an.watch("*.iv.*", ignore_deletes=True)
            try:
                while True:
                    e = await w.updates(timeout=5.0)
                    if e is None:
                        break
                    if e.value:
                        self.apply_surface(e.value)
                        n += 1
            except nats.errors.TimeoutError:
                log.warning("seed: watch analytics timed out")
            finally:
                await w.stop()
        b = await self._get(await self._bucket("book", False), "positions")
        if b:
            self.apply_book(b)
        bt = await self._get(self.kv, BETA_KEY)
        if bt:
            self.apply_beta(bt)
        await self.follow_legs()
        log.info("seed: %d smiles, %d open legs, beta %s", n, len(self.legs), self.beta.src)

    # ---- output -----------------------------------------------------------

    async def publish(self, payload: dict) -> None:
        b = _dumps(payload)
        await self.nc.publish("risk." + OUT_KEY, b)
        if self.kv is not None:
            try:
                await self.kv.put(OUT_KEY, b)
            except Exception as e:  # KV is a convenience for the gateway; the publish stands
                self.note_error(f"kv put {OUT_KEY}: {e}")
        self.stats["published"] += 1

    def note_error(self, s: str) -> None:
        self.stats["errors"] += 1
        self.last_error = s[:200]
        log.warning("%s", s)

    # ---- compute ----------------------------------------------------------

    async def run_once(self) -> None:
        if self.book is None:
            return
        now = clock.now()
        t0 = time.perf_counter()
        today = now.date()
        for k in [k for k in self.surfaces if k[1] < today]:
            del self.surfaces[k]
        try:
            out = self.risk.book(self.legs, self.surfaces, self.quotes, self.beta, now, self.book.get("ts"))
            out["pnl"] = day_pnl(self.rows, self.quotes, clock.ms(now), self.p.stale_ms)
        except Exception as e:
            self.note_error(f"book: {e!r}")
            return
        self.last = out
        await self.publish(out)
        self.stats["runs"] += 1
        self.stats["last_ms"] = round((time.perf_counter() - t0) * 1000, 1)

    async def loop(self) -> None:
        last_full = 0.0
        while not self.stop.is_set():
            start = time.monotonic()
            # Reprice on any input change, and at least every 5 intervals so time
            # decay and quote ages move with no ticks.
            if self.dirty or start - last_full >= 5 * self.interval:
                self.dirty = False
                last_full = start
                await self.run_once()
            wait = max(0.05, self.interval - (time.monotonic() - start))
            try:
                await asyncio.wait_for(self.stop.wait(), wait)
            except asyncio.TimeoutError:
                pass

    def health(self) -> dict:
        nowms = clock.ms(clock.now())
        detail = {
            "legs": str(len(self.legs)),
            "smiles": str(len(self.surfaces)),
            "quotes": str(len(self.quotes)),
            "beta": self.beta.src,
            "runs": str(self.stats["runs"]),
            "published": str(self.stats["published"]),
            "errors": str(self.stats["errors"]),
            "last_ms": str(self.stats["last_ms"]),
        }
        ok = self.nc is not None and self.nc.is_connected
        if self.last_error:
            detail["last_error"] = self.last_error
        if self.book is None:
            ok = False
            detail["book"] = "no book.positions yet"
        elif nowms - int(self.book.get("ts") or 0) > BOOK_STALE_MS:
            ok = False
            detail["book"] = "book.positions is stale"
        if self.last is not None:
            unmod = self.last.get("unmod") or []
            if unmod:
                detail["unmodelled"] = str(len(unmod))
        return {"service": SERVICE, "at": nowms, "ok": ok, "detail": detail}

    async def heartbeat(self) -> None:
        while not self.stop.is_set():
            try:
                await self.nc.publish(f"sys.{SERVICE}.health", _dumps(self.health()))
            except Exception as e:
                log.warning("health: %s", e)
            try:
                await asyncio.wait_for(self.stop.wait(), HEALTH_EVERY)
            except asyncio.TimeoutError:
                pass

    # ---- lifecycle --------------------------------------------------------

    async def run(self) -> None:
        self.nc = await nats.connect(
            self.cfg.nats_url, name=SERVICE, max_reconnect_attempts=-1, reconnect_time_wait=1,
        )
        self.js = self.nc.jetstream()
        self.kv = await self._bucket("risk", True)
        if self.kv is None:
            log.warning("bucket risk never appeared; publishing on core NATS only")
        self.lv = await self._bucket("lastvalue", False)
        # Subscribe before seeding so nothing falls between the two.
        await self.nc.subscribe("book.positions", cb=self._on_book)
        await self.nc.subscribe("analytics.*.iv.*", cb=self._on_surface, pending_msgs_limit=100_000)
        await self.nc.subscribe("risk." + BETA_KEY, cb=self._on_beta)
        await self.seed()
        log.info("riskengine: ref %s, interval %.2fs, clock offset %.0fs", self.p.ref, self.interval, clock.offset())
        tasks = [asyncio.create_task(self.loop()), asyncio.create_task(self.heartbeat())]
        await self.stop.wait()
        for t in tasks:
            t.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)
        await self.nc.drain()


def main() -> None:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s: %(message)s")
    svc = Service(config.load())

    async def _main():
        loop = asyncio.get_running_loop()
        for sig in (signal.SIGINT, signal.SIGTERM):
            loop.add_signal_handler(sig, svc.stop.set)
        await svc.run()

    asyncio.run(_main())


if __name__ == "__main__":
    main()
