"""python3 -m unittest discover -s py/newsgate"""

from __future__ import annotations

import io
import json
import os
import sqlite3
import tempfile
import threading
import unittest
from contextlib import redirect_stdout
from datetime import date, time as dtime
from http.server import BaseHTTPRequestHandler, HTTPServer
from unittest import mock

import newsgate as ng
import replay as rp

NOW = 1_790_000_000_000


def st(state, age_s=2, **kw):
    return {"ts": NOW - age_s * 1000, "state": state, "reasons": kw.pop("reasons", []), **kw}


class TestDecide(unittest.TestCase):
    def test_states(self):
        cfg = ng.GateConfig()
        rs = [{"level": "NO_NEW_ENTRIES", "kind": "event", "name": "RBI", "msg": "RBI policy at 10:00"}]
        cases = [
            (st("NORMAL"), True, 1.0, None, "NORMAL"),
            (st("CAUTION", reasons=[{"msg": "VIX +16%"}]), True, 0.5, None, "CAUTION"),
            (st("NO_NEW_ENTRIES", reasons=rs, expires_at=NOW + 60_000), False, 0.0, ng.VETO_NO_NEW, "NO_NEW_ENTRIES"),
            (st("NORMAL", age_s=61), False, 0.0, ng.VETO_UNKNOWN, ng.UNKNOWN),
            (st("NORMAL", stale=True), False, 0.0, ng.VETO_UNKNOWN, ng.UNKNOWN),
            (None, False, 0.0, ng.VETO_UNKNOWN, ng.UNKNOWN),
            ({"state": "NORMAL"}, False, 0.0, ng.VETO_UNKNOWN, ng.UNKNOWN),
            ({"ts": True, "state": "NORMAL"}, False, 0.0, ng.VETO_UNKNOWN, ng.UNKNOWN),
            (st("CALM"), False, 0.0, ng.VETO_UNKNOWN, ng.UNKNOWN),
            ([1, 2], False, 0.0, ng.VETO_UNKNOWN, ng.UNKNOWN),
        ]
        for payload, allow, size, veto, state in cases:
            with self.subTest(payload=payload):
                d = ng.decide(payload, NOW, cfg)
                self.assertEqual((d.allow, d.size_mult, d.veto, d.state), (allow, size, veto, state))
                self.assertFalse(d.flatten)
                self.assertEqual(d.vetoes(), [veto] if veto else [])

        d = ng.decide(cases[2][0], NOW, cfg)
        self.assertEqual(d.expires_at, NOW + 60_000)
        self.assertIn("RBI policy", d.note)
        self.assertTrue(ng.decide(cases[3][0], NOW, cfg).stale)

    def test_options(self):
        block = ng.GateConfig(caution_size=0)
        d = ng.decide(st("CAUTION"), NOW, block)
        self.assertEqual((d.allow, d.veto), (False, ng.VETO_CAUTION))

        allow = ng.GateConfig(on_unknown="allow")
        d = ng.decide(None, NOW, allow, "gateway HTTP 503")
        self.assertEqual((d.allow, d.size_mult, d.state), (True, 1.0, ng.UNKNOWN))
        self.assertIn("HTTP 503", d.note)

        # force_close only flags NO_NEW_ENTRIES, and only when on.
        fc = ng.GateConfig(force_close=True)
        self.assertTrue(ng.decide(st("NO_NEW_ENTRIES"), NOW, fc).flatten)
        for s in ("NORMAL", "CAUTION"):
            self.assertFalse(ng.decide(st(s), NOW, fc).flatten)
        self.assertFalse(ng.decide(None, NOW, fc).flatten)

        off = ng.decide(None, NOW, ng.GateConfig(enabled=False))
        self.assertTrue(off.allow)

    def test_config(self):
        c = ng.GateConfig.from_dict({"caution_size": 0, "timeout_s": 1, "force_close": True})
        self.assertEqual((c.caution_size, c.timeout_s, c.force_close), (0.0, 1.0, True))
        self.assertEqual(ng.GateConfig.from_dict(None), ng.GateConfig())
        bad = [{"force_close": "yes"}, {"caution_size": 2}, {"on_unknown": "maybe"}, {"timeout_s": 0},
               {"url": "file:///etc/passwd"}, {"cache_s": "5"}, {"caution": 0.5}, {"timeout_s": True}]
        for d in bad:
            with self.subTest(d=d), self.assertRaises(ValueError):
                ng.GateConfig.from_dict(d)


class TestGate(unittest.TestCase):
    def test_caches(self):
        calls, t = [], [NOW / 1000]
        payload = st("NORMAL")

        def fetch(cfg):
            calls.append(1)
            return payload, ""
        g = ng.NewsGate(ng.GateConfig(cache_s=5), clock=lambda: t[0], fetcher=fetch)
        self.assertTrue(g.entry().allow)
        t[0] += 4
        g.entry()
        self.assertEqual(len(calls), 1)
        t[0] += 1
        g.entry()
        self.assertEqual(len(calls), 2)
        # The cached payload ages too: a gateway gone silent goes stale, not NORMAL.
        g._fetch = lambda cfg: (None, "gateway unreachable")
        t[0] += 5
        self.assertEqual(g.vetoes(), [ng.VETO_UNKNOWN])


class _H(BaseHTTPRequestHandler):
    reply = (200, b"{}")
    delay = 0.0

    def do_GET(self):
        if _H.delay:
            threading.Event().wait(_H.delay)
        code, body = _H.reply
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *a):
        pass


class TestFetch(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.srv = HTTPServer(("127.0.0.1", 0), _H)
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()
        cls.url = f"http://127.0.0.1:{cls.srv.server_port}/api/risk-state"

    @classmethod
    def tearDownClass(cls):
        cls.srv.shutdown()
        cls.srv.server_close()

    def setUp(self):
        _H.delay = 0.0

    def gate(self, **kw):
        return ng.NewsGate(ng.GateConfig(url=self.url, cache_s=0, **kw))

    def test_ok(self):
        import time
        _H.reply = (200, json.dumps({"ts": int(time.time() * 1000), "state": "CAUTION", "reasons": [], "stale": False}).encode())
        d = self.gate().entry()
        self.assertEqual((d.allow, d.state, d.size_mult), (True, "CAUTION", 0.5))

    def test_fails_closed(self):
        _H.reply = (503, b'{"error":"no risk state yet"}')
        d = self.gate().entry()
        self.assertEqual((d.allow, d.veto), (False, ng.VETO_UNKNOWN))
        self.assertIn("503", d.note)

        _H.reply = (200, b"not json")
        self.assertIn("invalid JSON", self.gate().entry().note)

        _H.reply, _H.delay = (200, b"{}"), 0.6
        d = self.gate(timeout_s=0.2).entry()
        self.assertEqual(d.veto, ng.VETO_UNKNOWN)
        self.assertIn("unreachable", d.note)

    def test_refused(self):
        s = HTTPServer(("127.0.0.1", 0), _H)
        port = s.server_port
        s.server_close()
        d = ng.NewsGate(ng.GateConfig(url=f"http://127.0.0.1:{port}/api/risk-state")).entry()
        self.assertEqual((d.allow, d.veto), (False, ng.VETO_UNKNOWN))


def ist(d, hm):
    h, m = map(int, hm.split(":"))
    return rp.ms(date.fromisoformat(d), dtime(h, m))


CHANGES = [
    {"ts": ist("2026-10-05", "09:10"), "state": "NO_NEW_ENTRIES"},
    {"ts": ist("2026-10-05", "10:30"), "state": "NORMAL"},
    {"ts": ist("2026-10-06", "13:00"), "state": "CAUTION"},
    {"ts": ist("2026-10-06", "14:00"), "state": "NORMAL"},
]
PRIOR = {"ts": ist("2026-10-01", "08:00"), "state": "NORMAL"}


class TestReplay(unittest.TestCase):
    def test_state_at(self):
        self.assertEqual(rp.state_at(CHANGES, PRIOR, ist("2026-10-02", "09:15")), "NORMAL")
        self.assertEqual(rp.state_at(CHANGES, PRIOR, ist("2026-10-05", "09:15")), "NO_NEW_ENTRIES")
        self.assertEqual(rp.state_at(CHANGES, PRIOR, ist("2026-10-05", "10:30")), "NORMAL")
        self.assertEqual(rp.state_at(CHANGES, None, ist("2026-10-04", "09:15")), rp.UNLOGGED)
        self.assertEqual(rp.state_at([], None, 0), rp.UNLOGGED)

        self.assertTrue(rp.touched(CHANGES, PRIOR, date(2026, 10, 5)))
        self.assertTrue(rp.touched(CHANGES, PRIOR, date(2026, 10, 6)))  # CAUTION after the open
        self.assertFalse(rp.touched(CHANGES, PRIOR, date(2026, 10, 7)))
        self.assertFalse(rp.touched(CHANGES, None, date(2026, 9, 30)))

    def ledger(self):
        fd, path = tempfile.mkstemp(suffix=".db")
        os.close(fd)
        self.addCleanup(os.remove, path)
        con = sqlite3.connect(path)
        con.execute("""CREATE TABLE trades (id INTEGER PRIMARY KEY, symbol TEXT, engine TEXT, qty INTEGER,
            entry_date TEXT, entry_price REAL, exit_date TEXT, exit_price REAL, pnl REAL, r_multiple REAL,
            reason TEXT, days_held INTEGER)""")
        rows = [
            ("AAA", "breakout", "2026-10-02", 1200.0, 1.2),
            ("BBB", "breakout", "2026-10-05", -800.0, -1.0),  # blocked
            ("CCC", "rotation", "2026-10-05", -400.0, -0.5),  # blocked
            ("DDD", "breakout", "2026-10-06", 300.0, 0.4),    # NORMAL at 09:15, touched later
            ("EEE", "breakout", "2026-10-07", None, None),    # still open
        ]
        con.executemany("INSERT INTO trades (symbol, engine, entry_date, pnl, r_multiple) VALUES (?,?,?,?,?)", rows)
        con.commit()
        con.close()
        return path

    def test_summarize(self):
        path = self.ledger()
        ts = rp.label(rp.read_ledger(path), CHANGES, PRIOR, dtime(9, 15))
        self.assertEqual([t.state for t in ts], ["NORMAL", "NO_NEW_ENTRIES", "NO_NEW_ENTRIES", "NORMAL", "NORMAL"])
        by = {r["group"]: r for r in rp.summarize(ts)}
        self.assertEqual(by["NO_NEW_ENTRIES"], {"group": "NO_NEW_ENTRIES", "n": 2, "closed": 2, "wins": 0,
                                                "pnl": -1200.0, "avg_pnl": -600.0, "avg_r": -0.75})
        self.assertEqual((by["NORMAL"]["n"], by["NORMAL"]["closed"], by["NORMAL"]["pnl"]), (3, 2, 1500.0))
        touch = {r["group"]: r["n"] for r in rp.summarize(ts, "touched")}
        self.assertEqual(touch, {"clean": 2, "touched": 3})

        # A later entry time moves the 10-05 trades out of the block.
        late = rp.label(rp.read_ledger(path), CHANGES, PRIOR, dtime(11, 0))
        self.assertEqual(late[1].state, "NORMAL")
        self.assertEqual(len(rp.read_ledger(path, "rotation")), 1)

    def test_ledger_read_only(self):
        path = self.ledger()
        before = os.path.getmtime(path)
        with mock.patch.object(rp, "fetch_log", return_value=(PRIOR, CHANGES)), redirect_stdout(io.StringIO()) as out:
            self.assertEqual(rp.main(["--ledger", path, "--json"]), 0)
        body = json.loads(out.getvalue())
        self.assertEqual(body["from"], "2026-10-02")
        self.assertEqual(body["by_state"][1]["group"], "NO_NEW_ENTRIES")
        self.assertEqual(os.path.getmtime(path), before)
        uri = "file:" + path + "?mode=ro"
        with self.assertRaises(sqlite3.OperationalError):
            con = sqlite3.connect(uri, uri=True)
            con.execute("DELETE FROM trades")

        with mock.patch.object(rp, "fetch_log", return_value=(PRIOR, CHANGES)), redirect_stdout(io.StringIO()) as out:
            rp.main(["--ledger", path])
        self.assertIn("NO_NEW_ENTRIES is what the gate would have blocked", out.getvalue())


if __name__ == "__main__":
    unittest.main()
