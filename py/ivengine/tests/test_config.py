import os

import pytest

from pmt_iv import config

YAML = """
nats_url: ${T_NATS}
universe:
  underlyings:
    - name: NIFTY
      deriv_exchange: NFO
    - name: BANKNIFTY
ivengine:
  rate: 0.07
  interval: 500ms
  idle_every: 10s
  max_spread: 0.25
  calendar: config/calendar/holidays.csv
"""


def write(tmp_path, text):
    d = tmp_path / "config"
    d.mkdir()
    p = d / "pmt.yaml"
    p.write_text(text)
    return p


def test_expand(monkeypatch):
    monkeypatch.setenv("T_A", "x")
    monkeypatch.delenv("T_B", raising=False)
    assert config.expand("${T_A}-$T_A-${T_B}.") == "x-x-."


def test_load(tmp_path, monkeypatch):
    monkeypatch.setenv("T_NATS", "nats://nats:4222")
    c = config.load(str(write(tmp_path, YAML)))
    assert c.nats_url == "nats://nats:4222"
    assert [(u.name, u.exchange) for u in c.underlyings] == [("NIFTY", "NFO"), ("BANKNIFTY", "NFO")]
    assert (c.rate, c.interval, c.idle_every, c.max_spread) == (0.07, 0.5, 10.0, 0.25)
    assert c.calendar == os.path.join(str(tmp_path), "config/calendar/holidays.csv")


def test_load_validates(tmp_path):
    with pytest.raises(ValueError):
        config.load(str(write(tmp_path, YAML.replace("max_spread: 0.25", "max_spread: 2"))))


def test_repo_config_loads(monkeypatch):
    here = os.path.dirname(__file__)
    p = os.path.join(here, "..", "..", "..", "config", "pmt.yaml")
    c = config.load(p)
    assert {u.name for u in c.underlyings} >= {"NIFTY"}
    assert os.path.exists(c.calendar)
