"""The ivengine service: seeds its book from KV lastvalue, follows the option, future
and index ticks of the configured underlyings, and every interval recomputes the
expiries whose quotes moved (all of them every idle_every), publishing each result
on analytics.<...> and putting it to KV analytics for the gateway to seed from.

It only reads ticks and writes analytics; it has no path to orders.
"""

from __future__ import annotations

import asyncio
import json
import logging
import signal
import time
from datetime import date

import nats
from nats.js.errors import BucketNotFoundError

from . import clock, config
from .calendar import Calendar
from .engine import VIX_INDEX, Book, Engine, Params

log = logging.getLogger("ivengine")

SERVICE = "ivengine"
HEALTH_EVERY = 5.0
STALE_MS = 60_000  # a quote older than this during a session is flagged in health


def _subjects(unds: list[str]) -> list[str]:
    subs = []
    for u in unds:
        n = u.lower()
        subs += [f"tick.nfo.{n}.>", f"tick.nse.{n}._._.idx"]
    subs.append(f"tick.nse.{VIX_INDEX.lower()}._._.idx")
    return subs


def _dumps(v: dict) -> bytes:
    return json.dumps(v, separators=(",", ":"), allow_nan=False).encode()


class Service:
    def __init__(self, cfg: config.Config):
        self.cfg = cfg
        self.unds = [u.name for u in cfg.underlyings]
        self.book = Book(self.unds)
        self.engine = Engine(
            self.book,
            Calendar.load(cfg.calendar),
            Params(cfg.rate, cfg.max_spread, cfg.forward_strikes, cfg.vix_roll_days),
            cfg.weekend_weight,
            cfg.overnight_weight,
        )
        self.dirty: set[tuple[str, date]] = set()
        self.index_dirty: set[str] = set()
        self.results: dict[tuple[str, date], object] = {}
        self.nc = None
        self.kv = None
        self.stats = {"runs": 0, "published": 0, "errors": 0, "last_ms": 0.0, "last_run": 0}
        self.last_error = ""
        self.stop = asyncio.Event()

    # ---- input ------------------------------------------------------------

    def apply(self, data: bytes) -> None:
        try:
            t = json.loads(data)
        except ValueError:
            return
        k = self.book.update(t)
        if isinstance(k, tuple):
            self.dirty.add(k)
        elif isinstance(k, str):
            self.index_dirty.add(k)

    async def _on_tick(self, msg) -> None:
        self.apply(msg.data)

    async def seed(self, js) -> None:
        """Loads the latest tick per contract from KV lastvalue, so a restart mid-day
        has a full chain at once instead of waiting for each strike to tick."""
        try:
            lv = await js.key_value("lastvalue")
        except BucketNotFoundError:
            log.warning("seed: bucket lastvalue not found; starting empty")
            return
        n = 0
        for subj in _subjects(self.unds):
            key = subj.removeprefix("tick.")
            w = await lv.watch(key, ignore_deletes=True)
            try:
                while True:
                    e = await w.updates(timeout=5.0)
                    if e is None:
                        break
                    if e.value:
                        self.apply(e.value)
                        n += 1
            except nats.errors.TimeoutError:
                log.warning("seed: watch %s timed out", key)
            finally:
                await w.stop()
        log.info("seed: %d ticks from lastvalue, %d chains", n, len(self.book.chains))

    # ---- output -----------------------------------------------------------

    async def publish(self, key: str, payload: dict) -> None:
        b = _dumps(payload)
        await self.nc.publish("analytics." + key, b)
        if self.kv is not None:
            try:
                await self.kv.put(key, b)
            except Exception as e:  # KV is a convenience for the gateway; the publish stands
                self.note_error(f"kv put {key}: {e}")
        self.stats["published"] += 1

    def note_error(self, s: str) -> None:
        self.stats["errors"] += 1
        self.last_error = s[:200]
        log.warning("%s", s)

    # ---- compute ----------------------------------------------------------

    async def run_once(self, full: bool) -> None:
        now = clock.now()
        nowms = clock.ms(now)
        t0 = time.perf_counter()
        todo = set(self.book.chains) if full else set(self.dirty)
        self.dirty.clear()
        touched: set[str] = set()
        for und, d in sorted(todo):
            try:
                res = self.engine.expiry(und, d, now)
            except Exception as e:
                self.note_error(f"expiry {und} {d}: {e!r}")
                continue
            if res is None:
                self.results.pop((und, d), None)
                continue
            self.results[(und, d)] = res
            touched.add(und)
            await self.publish(f"{und.lower()}.iv.{d:%Y%m%d}", res.payload)
        # Drop expiries that have passed.
        for key in [k for k in self.results if k[1] < now.date()]:
            del self.results[key]
        for und in sorted(touched | (set(self.unds) if full else set())):
            rs = [r for (u, _), r in self.results.items() if u == und]
            if rs:
                await self.publish(f"{und.lower()}.term", Engine.term(und, rs, nowms))
        vu = self.cfg.vix_underlying
        if full or vu in touched or VIX_INDEX in self.index_dirty:
            try:
                v = self.engine.vix(vu, now)
            except Exception as e:
                v = None
                self.note_error(f"vix: {e!r}")
            if v is not None:
                await self.publish(f"{vu.lower()}.vix", v)
        self.index_dirty.clear()
        self.stats["runs"] += 1
        self.stats["last_ms"] = round((time.perf_counter() - t0) * 1000, 1)
        self.stats["last_run"] = nowms

    async def loop(self) -> None:
        last_full = 0.0
        while not self.stop.is_set():
            start = time.monotonic()
            full = start - last_full >= self.cfg.idle_every
            if full:
                last_full = start
            if full or self.dirty or self.index_dirty:
                await self.run_once(full)
            wait = max(0.05, self.cfg.interval - (time.monotonic() - start))
            try:
                await asyncio.wait_for(self.stop.wait(), wait)
            except asyncio.TimeoutError:
                pass

    def health(self) -> dict:
        now = clock.now()
        nowms = clock.ms(now)
        detail = {
            "chains": str(len(self.book.chains)),
            "expiries": str(len(self.results)),
            "ticks": str(self.book.ticks),
            "runs": str(self.stats["runs"]),
            "published": str(self.stats["published"]),
            "errors": str(self.stats["errors"]),
            "last_ms": str(self.stats["last_ms"]),
        }
        ok = self.nc is not None and self.nc.is_connected
        if self.last_error:
            detail["last_error"] = self.last_error
        session = self.engine.cal.session(now.date())
        if session.trading and session.open <= now <= session.close:
            ages = [r.payload["age"]["med"] for r in self.results.values() if r.payload.get("age")]
            if ages:
                detail["age_med_ms"] = str(min(ages))
            if not self.results or (ages and min(ages) > STALE_MS):
                ok = False
                detail["stale"] = "no fresh option quotes during the session"
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
            pending_size=256 * 1024 * 1024,
        )
        js = self.nc.jetstream()
        for attempt in range(30):
            try:
                self.kv = await js.key_value("analytics")
                break
            except BucketNotFoundError:
                # The Go services create the buckets; wait for them rather than create a
                # bucket with a config that could drift from internal/bus.
                if attempt == 0:
                    log.warning("bucket analytics not found yet; waiting")
                await asyncio.sleep(2)
        else:
            log.warning("bucket analytics never appeared; publishing on core NATS only")
        # Subscribe before seeding so no tick falls between the two; a seeded tick
        # older than a live one is harmless (the next tick on that contract fixes it).
        for s in _subjects(self.unds):
            await self.nc.subscribe(s, cb=self._on_tick, pending_msgs_limit=1_000_000,
                                    pending_bytes_limit=512 * 1024 * 1024)
        await self.seed(js)
        log.info("ivengine: %s, interval %.2fs, idle_every %.1fs, clock offset %.0fs",
                 ",".join(self.unds), self.cfg.interval, self.cfg.idle_every, clock.offset())
        tasks = [asyncio.create_task(self.loop()), asyncio.create_task(self.heartbeat())]
        await self.stop.wait()
        for t in tasks:
            t.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)
        await self.nc.drain()


def main() -> None:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s: %(message)s")
    cfg = config.load()
    svc = Service(cfg)

    async def _main():
        loop = asyncio.get_running_loop()
        for sig in (signal.SIGINT, signal.SIGTERM):
            loop.add_signal_handler(sig, svc.stop.set)
        await svc.run()

    asyncio.run(_main())


if __name__ == "__main__":
    main()
