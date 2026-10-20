import io
import math
from datetime import date, timedelta

import pytest

from pmt_iv import black76, clock
from pmt_iv.calendar import Calendar
from pmt_iv.engine import Quote
from pmt_iv.risk import Beta, Leg, Params, Risk, Surface, day_pnl, legs_from_book

NOW = clock.at(date(2026, 10, 19), "11:00")
WEEK = date(2026, 10, 27)
MONTH = date(2026, 11, 26)
cal = Calendar.read(io.StringIO("2026-10-20,FO,holiday,,,\n2026-11-24,FO,holiday,,,\n"))
F = 25000.0


def risk(**kw) -> Risk:
    return Risk(cal, Params(**kw))


def payload(r: Risk, x: date, F: float = F, iv=None, ts=None, und="NIFTY") -> dict:
    """A flat 15-vol smile as a fitted SVI, or iv(k) as raw chain rows (fit not ok)."""
    T, Tc = r.times(NOW, x)
    fit = {"ok": False}
    if iv is None:
        fit = {"a": 0.15**2 * T, "b": 0.0, "rho": 0.0, "m": 0.0, "s": 0.1, "rmse": 0.0, "ok": True, "arb": []}
        iv = lambda k: 0.15
    rows = []
    for K in range(int(F) - 2000, int(F) + 2001, 100):
        kx = math.log(K / F)
        v = iv(kx)
        rows.append({"k": float(K), "kx": kx, "c": {"iv": v}, "p": {"iv": v}})
    return {"u": und, "x": x.isoformat(), "f": F, "s": F - 20, "tte": T, "cal": Tc, "r": 0.065,
            "ts": ts if ts is not None else clock.ms(NOW) - 500, "atm": iv(0.0), "fit": fit, "rows": rows}


def row(k, K, q, e=WEEK, und="NIFTY", lot=75, c=None):
    c = c or f"{und}|{e}|{K}|{k}"
    return {"sym": c, "x": "NFO", "pr": "NRML", "c": c, "s": "tick." + c, "u": und, "k": k,
            "e": e.isoformat(), "K": K, "q": q, "lot": lot}


def fair_quote(r: Risk, K, call, x=WEEK, iv=0.15, half=1.0, age=500):
    T, Tc = r.times(NOW, x)
    v = black76.price(F, K, T, iv, call, math.exp(-0.065 * Tc))
    return Quote(v - half, v + half, v, clock.ms(NOW) - age)


def setup(r: Risk, rows, quotes=None, surfs=None, beta=None, now=NOW):
    surfs = surfs if surfs is not None else {("NIFTY", WEEK): Surface.from_payload(payload(r, WEEK))}
    return r.book(legs_from_book({"net": rows}), surfs, quotes or {}, beta or Beta.default(r.p), now)


def strangle(r: Risk):
    rows = [row("CE", 25400.0, -150), row("PE", 24600.0, -150)]
    qs = {rows[0]["c"]: fair_quote(r, 25400.0, True), rows[1]["c"]: fair_quote(r, 24600.0, False)}
    return rows, qs


def col(r: Risk, x: float) -> int:
    return list(r.p.spot_steps).index(x)


def test_centre_cell_is_zero_and_liquidation_crosses_the_spread():
    r = risk()
    rows, qs = strangle(r)
    out = setup(r, rows, qs)
    g = out["book"]["grid"]
    z = g["rows"][list(r.p.vol_rows).index(0.0)]
    assert z["mid"][col(r, 0.0)] == 0
    # Closing 300 units at a 1-rupee half-spread costs 300.
    assert z["liq"][col(r, 0.0)] == pytest.approx(-300, abs=1)
    assert out["book"]["close"] == pytest.approx(300, abs=1)
    # Spreads widen in the stress cells: the 10% column costs 1 + 10/5 = 3x.
    assert z["mid"][col(r, -10.0)] - z["liq"][col(r, -10.0)] == pytest.approx(900, abs=2)
    assert all(not l["flags"] for l in out["legs"])
    assert {l["src"] for l in out["legs"]} == {"quote"}


def test_greeks_match_finite_differences():
    r = risk()
    rows, qs = strangle(r)
    rows.append(row("CE", 25200.0, 75, e=WEEK))
    qs[rows[2]["c"]] = fair_quote(r, 25200.0, True)
    out = setup(r, rows, qs)
    legs = [Leg.from_row(x) for x in rows]
    surf = Surface.from_payload(payload(r, WEEK))
    marks = [r.mark(l, surf, qs[l.c], NOW) for l in legs]

    def P(x, dv=0.0):
        return sum(m.leg.q * (r.value(m, x, dv, NOW) - m.V) for m in marks)

    u = out["unds"][0]
    h = 0.001
    assert u["d"] == pytest.approx((P(h) - P(-h)) / (2 * h * 100), rel=1e-3)
    assert u["g"] == pytest.approx((P(h) - 2 * P(0) + P(-h)) / (h * 100) ** 2, rel=2e-2)
    assert u["v30"] == pytest.approx((P(0, 0.1) - P(0, -0.1)) / 0.2, rel=1e-3)
    # Delta in lots and in NIFTY lots agree for a NIFTY-only book, scaled by F / spot.
    assert u["dl"] == pytest.approx(sum(m.leg.q * m.g["delta"] for m in marks) / 75, abs=0.006)
    assert out["book"]["dn"] == pytest.approx(u["dl"] * F / (F - 20), rel=1e-3)


def test_breakeven_is_sqrt_two_theta_over_gamma():
    r = risk()
    rows, qs = strangle(r)
    b = setup(r, rows, qs)["book"]
    assert b["g"] < 0 < b["t"]
    assert b["be"]["pct"] == pytest.approx(math.sqrt(2 * b["t"] / -b["g"]), rel=1e-3)
    assert b["be"]["sd"] == pytest.approx(b["be"]["pct"] / (100 * b["sigday"]), rel=1e-2)


def test_vol_shock_scales_by_expiry_and_caps():
    r = risk()
    rows = [row("CE", 25000.0, 75, e=WEEK), row("CE", 25000.0, 75, e=MONTH)]
    surfs = {("NIFTY", d): Surface.from_payload(payload(r, d)) for d in (WEEK, MONTH)}
    out = setup(r, rows, surfs=surfs)
    sc = {l["x"]: l["scale"] for l in out["legs"]}
    assert sc[MONTH.isoformat()] == pytest.approx(math.sqrt(30 / out["legs"][1]["dte"]), rel=1e-3)
    assert sc[WEEK.isoformat()] == pytest.approx(math.sqrt(30 / out["legs"][0]["dte"]), rel=1e-3)
    assert sc[WEEK.isoformat()] > sc[MONTH.isoformat()]
    # A leg a few hours from expiry is capped.
    r2 = risk()
    late = clock.at(WEEK, "14:00")
    s = Surface.from_payload({**payload(r2, WEEK), "ts": clock.ms(late)})
    m = r2.mark(Leg.from_row(row("CE", 25000.0, 75)), s, None, late)
    assert m.scale == r2.p.vol_cap
    # The vol rows come off the smile with the leg's own residual kept.
    l = out["legs"][0]
    assert l["src"] == "chain" and l["resid"] == pytest.approx(0, abs=1e-9)


def test_sticky_moneyness_reads_the_skew():
    r = risk()
    skew = lambda k: 0.15 - 0.5 * k  # puts bid
    surf = Surface.from_payload(payload(r, WEEK, iv=skew))
    m = r.mark(Leg.from_row(row("PE", 24000.0, 75)), surf, None, NOW)
    assert "smile fit not ok (interpolated)" in m.flags
    assert m.iv == pytest.approx(skew(math.log(24000 / F)), rel=1e-3)
    # A 4% fall makes 24000 nearly ATM: its vol drops toward the ATM vol.
    T, Tc = r.times(NOW, WEEK)
    v = r.value(m, -0.04, 0.0, NOW)
    want = black76.price(F * 0.96, 24000.0, T, skew(math.log(24000 / (F * 0.96))), False, math.exp(-0.065 * Tc))
    assert v == pytest.approx(want, rel=1e-6)


def test_stale_and_wide_quotes_are_flagged_and_named():
    r = risk()
    rows, qs = strangle(r)
    qs[rows[0]["c"]] = fair_quote(r, 25400.0, True, age=120_000)
    qs[rows[1]["c"]] = fair_quote(r, 24600.0, False, half=8.0)
    out = setup(r, rows, qs)
    f = {l["name"]: l["flags"] for l in out["legs"]}
    assert any("old" in x for x in f["NIFTY 27OCT 25400 CE"])
    assert any("spread" in x for x in f["NIFTY 27OCT 24600 PE"])
    built_on = {x["leg"] for x in out["book"]["flags"]}
    assert built_on == {"NIFTY 27OCT 25400 CE", "NIFTY 27OCT 24600 PE"}
    assert out["unds"][0]["flags"] == out["book"]["flags"]
    # Off session the quote age is not a flag (the market is shut), but the spread still is.
    late = clock.at(date(2026, 10, 19), "20:00")
    out = setup(r, rows, qs, now=late)
    assert not out["session"]
    f = {l["name"]: l["flags"] for l in out["legs"]}
    assert not any("old" in x for x in f["NIFTY 27OCT 25400 CE"])


def test_no_quote_marks_off_the_chain_and_assumes_a_wide_market():
    r = risk()
    rows, _ = strangle(r)
    out = setup(r, rows)
    for l in out["legs"]:
        assert l["src"] == "chain" and l["flags"] == ["no quote"]
        assert l["half"] == pytest.approx(max(0.05, 0.15 * l["v0"]) / 2, rel=1e-2)


def test_future_leg():
    r = risk()
    rows = [row("FUT", 0, 75, e=MONTH, c="NIFTY|FUT")]
    qs = {"NIFTY|FUT": Quote(25099.5, 25100.5, 25100, clock.ms(NOW) - 300)}
    out = setup(r, rows, qs)
    l = out["legs"][0]
    assert l["d"] == pytest.approx(75 * 25100 / 100) and l["dl"] == pytest.approx(1)
    assert l["g"] == 0 and l["v"] == 0 and l["t"] == 0
    z = out["book"]["grid"]["rows"][list(r.p.vol_rows).index(0.0)]
    assert z["mid"][col(r, 1.0)] == pytest.approx(75 * 251, abs=1)


def test_time_axis_and_expiry_intrinsic():
    r = risk()
    rows, qs = strangle(r)
    out = setup(r, rows, qs)
    labels = [h["l"] for h in out["hz"]]
    assert labels[0] == "now" and labels[1] == "close"
    # 20 Oct is a holiday: the next open is the 21st.
    i = labels.index("open")
    assert out["hz"][i]["ts"] == clock.ms(clock.at(date(2026, 10, 21), "09:15"))
    assert labels[-1] == "exp 27OCT"
    t = out["book"]["time"]
    # Theta: the strangle earns by the close and more by the next open with spot unchanged.
    c0 = col(r, 0.0)
    assert 0 < t[1]["mid"][c0] < t[i]["mid"][c0]
    # At expiry each leg is intrinsic: a 5% fall puts the 24600 put 350 in the money.
    exp = t[-1]
    legs = [Leg.from_row(x) for x in rows]
    v0 = sum(m.leg.q * m.V for m in (r.mark(l, Surface.from_payload(payload(r, WEEK)), qs[l.c], NOW) for l in legs))
    want = -150 * max(24600 - F * 0.95, 0) - v0
    assert exp["mid"][col(r, -5.0)] == pytest.approx(want, abs=1)
    assert exp["liq"][col(r, -5.0)] == exp["mid"][col(r, -5.0)]  # settles, no spread to cross


def test_overnight_weight_decays_the_book_close_to_open():
    def gap(ow):
        r = risk(overnight_weight=ow)
        rows, qs = strangle(r)
        out = setup(r, rows, qs)
        labels = [h["l"] for h in out["hz"]]
        t, c0 = out["book"]["time"], col(r, 0.0)
        return t[labels.index("open")]["mid"][c0] - t[labels.index("close")]["mid"][c0], out
    g0, _ = gap(0.0)
    g1, out = gap(0.25)
    assert 0 < g0 < g1
    # Theta is per weekday close to close: the overnight share lands between close and open.
    assert out["book"]["t"] > 0


def test_beta_path_and_book_beta_weighting():
    r = risk(und_beta={"BANKNIFTY": 1.2})
    rows, qs = strangle(r)
    rows.append(row("PE", 55000.0, 35, e=MONTH, und="BANKNIFTY", lot=35))
    surfs = {("NIFTY", WEEK): Surface.from_payload(payload(r, WEEK)),
             ("BANKNIFTY", MONTH): Surface.from_payload(payload(r, MONTH, F=56000.0, und="BANKNIFTY"))}
    out = setup(r, rows, qs, surfs=surfs)
    p = out["book"]["grid"]["path"]
    assert p["dv"][col(r, -3.0)] == pytest.approx(3.9)
    assert p["dv"][col(r, 2.0)] == pytest.approx(-1.0)
    bn = next(u for u in out["unds"] if u["u"] == "BANKNIFTY")
    nf = next(u for u in out["unds"] if u["u"] == "NIFTY")
    assert out["book"]["d"] == pytest.approx(nf["d"] + 1.2 * bn["d"], abs=0.05)
    assert out["beta"]["src"] == "default"
    # Measured betas override the defaults.
    b = Beta.from_payload({"dn": -1.0, "up": -0.2, "und": {"BANKNIFTY": 1.4}, "src": "measured", "n": 250, "asof": "2026-10-16"}, r.p)
    assert b.path(-3) == pytest.approx(3.0) and b.of("BANKNIFTY", "NIFTY") == 1.4


def test_unmodelled_rows_are_listed_not_dropped():
    r = risk()
    rows, qs = strangle(r)
    eq = {"sym": "RELIANCE", "x": "NSE", "pr": "CNC", "c": "NSE|RELIANCE|||EQ", "u": "RELIANCE", "k": "EQ", "q": 10}
    bare = {"sym": "MYSTERY", "x": "NFO", "pr": "MIS", "q": 5}
    closed = {**row("CE", 25000.0, 0)}
    out = setup(r, rows + [eq, bare, closed], qs)
    why = {u["name"]: u["why"] for u in out["unmod"]}
    assert why == {"RELIANCE": "EQ not modelled", "MYSTERY": "not in refdata"}
    assert len(out["legs"]) == 2
    assert {x["leg"] for x in out["book"]["flags"]} >= {"RELIANCE", "MYSTERY"}


def test_output_is_json_clean():
    import json

    r = risk()
    rows, qs = strangle(r)
    s = json.dumps(setup(r, rows, qs), allow_nan=False)
    assert "NaN" not in s


def test_day_pnl_moves_the_broker_figures_to_fresh_ticks():
    now = clock.ms(NOW)
    open_ = dict(row("CE", 25400.0, -150), p=100.0, m=1, m2m=-500.0, pnl=1200.0)
    stale = dict(row("PE", 24600.0, 75), p=50.0, m=1, m2m=300.0, pnl=-100.0)
    closed = dict(row("CE", 25600.0, 0), p=10.0, m=1, m2m=250.0, pnl=250.0)
    qs = {open_["c"]: Quote(101.0, 103.0, 102.0, now - 200),   # fresh: short 150 loses 2 each
          stale["c"]: Quote(55.0, 57.0, 56.0, now - 60_000),   # too old: keep the broker's p
          closed["c"]: Quote(20.0, 21.0, 20.5, now - 100)}     # closed: realised stands
    out = day_pnl([open_, stale, closed], qs, now, 5000)
    assert out["bm2m"] == 50.0 and out["bpnl"] == 1350.0
    assert out["m2m"] == 50.0 - 300.0 and out["pnl"] == 1350.0 - 300.0
    assert out["live"] == 1 and out["stale"] == 1
