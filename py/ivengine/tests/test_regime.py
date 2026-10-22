from datetime import date

from pmt_iv.regime import classify, compute


def test_rules_in_order():
    assert classify(0.85, -0.01, 1.0, 0.0)["label"] == "stress"
    assert classify(0.70, -0.01, 1.0, 0.0)["label"] == "elevated"  # inverted but below 80
    assert classify(0.85, 0.01, 1.0, 0.0)["label"] == "elevated"
    assert classify(0.20, 0.01, 1.5, 0.0)["label"] == "waking"
    k = classify(0.20, 0.01, 1.0, 0.02)
    assert k["label"] == "calm" and k["premium"] == "rich" and k["trend"] == "steady"
    assert classify(0.50, 0.0, 1.0, None)["label"] == "normal"
    assert classify(None, 0.01, 1.0, 0.0)["label"] is None
    assert classify(0.50, None, 1.0, 0.0)["label"] is None


def test_compute_picks_vix_and_falls_back_to_42d():
    d = date(2026, 1, 5)
    vrp = {d: (0.40, 0.90, 0.01, 0.02)}
    cm = {d: {21: 0.15, 42: 0.13}}
    rv = {d: {10: 0.2, 63: 0.1}}
    r = compute("NIFTY", vrp, cm, rv, use_vix=True)[0]
    assert r["level_src"] == "vix" and r["level_pct"] == 0.90 and r["premium"] == 0.02
    assert abs(r["term_slope"] + 0.02) < 1e-12 and r["rv_ratio"] == 2.0 and r["label"] == "stress"
    r = compute("BANKNIFTY", vrp, cm, rv, use_vix=False)[0]
    assert r["level_src"] == "iv30" and r["level"] == "mid" and r["label"] == "waking"
