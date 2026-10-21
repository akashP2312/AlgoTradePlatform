from datetime import date

import pytest

from pmt_iv import clock, daily
from pmt_iv.daily import Entry, step

D = date(2026, 10, 21)
E = Entry("19:20", "vol", ["vix", "spotvol"], "23:00")


def at(hms):
    return clock.at(D, hms)


def test_waits_for_its_time_then_its_jobs():
    assert step(E, at("19:19"), True, False, {"vix", "spotvol"}) == "wait"
    assert step(E, at("19:20"), True, False, {"vix"}) == "wait"
    assert step(E, at("19:20"), True, False, {"vix", "spotvol"}) == "run"
    assert step(E, at("23:00"), True, False, {"vix"}) == "give-up"


def test_runs_once_and_only_on_trading_days():
    assert step(E, at("20:00"), True, True, {"vix", "spotvol"}) == "skip"
    assert step(E, at("20:00"), False, False, {"vix", "spotvol"}) == "skip"


def test_entries_from_config(tmp_path):
    p = tmp_path / "pmt.yaml"
    p.write_text('pyschedule:\n  - {at: "19:05", job: iv-eod, after: [bhav]}\n')
    assert daily.entries(str(p)) == [Entry("19:05", "iv-eod", ["bhav"], "23:00")]
    p.write_text('pyschedule:\n  - {at: "19:05", job: rm, after: [bhav]}\n')
    with pytest.raises(ValueError, match="unknown job"):
        daily.entries(str(p))
    p.write_text('pyschedule:\n  - {at: "19:05", job: vol, until: "18:00"}\n')
    with pytest.raises(ValueError, match="until"):
        daily.entries(str(p))


def test_repo_config_parses():
    es = daily.entries("../../config/pmt.yaml")
    assert {e.job for e in es} == set(daily.COMMANDS)


class FakeRuns:
    def __init__(self, ok):
        self.ok, self.rows = ok, []

    def tried(self, job, since):
        return any(r[0] == job for r in self.rows)

    def ok_since(self, jobs, since):
        return self.ok

    def start(self, job):
        self.rows.append([job, "running", ""])
        return len(self.rows) - 1

    def finish(self, rid, ok, detail):
        self.rows[rid][1:] = ["ok" if ok else "error", detail]


class Cal:
    def __init__(self, trading):
        self.t = trading

    def session(self, d):
        return type("S", (), {"trading": self.t})()


def test_tick_records_runs(monkeypatch):
    monkeypatch.setattr(daily, "COMMANDS", {"vol": ["sh", "-c", "echo hello; echo done; echo '  indented'"]})
    r = FakeRuns({"vix", "spotvol"})
    daily.tick(r, [E], Cal(True), at("19:30"), set())
    daily.tick(r, [E], Cal(True), at("19:31"), set())  # already tried: no second run
    assert r.rows == [["vol", "ok", "done"]]
    monkeypatch.setattr(daily, "COMMANDS", {"vol": ["sh", "-c", "echo boom >&2; exit 3"]})
    r = FakeRuns({"vix"})
    daily.tick(r, [E], Cal(True), at("23:00"), set())
    assert r.rows == [["vol", "error", "gave up at 23:00: no ok run today of spotvol"]]
    r = FakeRuns({"vix", "spotvol"})
    daily.tick(r, [E], Cal(True), at("19:30"), set())
    assert r.rows == [["vol", "error", "exit 3: boom"]]
    r = FakeRuns({"vix", "spotvol"})
    daily.tick(r, [E], Cal(False), at("19:30"), set())
    assert r.rows == []
