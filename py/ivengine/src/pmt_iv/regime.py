"""Vol regime labels: a short name for today's vol conditions, from visible rules.

Per index and trade date, four readings, each put into a bucket by fixed cut-offs:
  - level: the 252-day percentile of India VIX (NIFTY) or of iv30 (the others), from
    vrp_eod (0 to 1): low below 0.33, high from 0.67, else mid;
  - term: the 63-day minus the 21-day constant-maturity ATM IV, from ivcm_eod (42-day
    when there is no 63-day point): up above +0.5 vol points, inverted below -0.5,
    else flat;
  - trend: 10-day over 63-day Yang-Zhang realized vol, from rv_eod: rising above 1.25,
    falling below 0.8, else steady;
  - premium: iv30 (or VIX) minus the HAR forecast, ex ante, from vrp_eod: rich above
    +1 vol point, cheap below -1, else fair.
The label is the first rule in RULES that matches; the row keeps the readings, their
buckets and the rule, so a label can always be checked by hand. A label describes the
conditions; it is not a signal.

Usage:
  pmt-regime                      recompute everything
  options: --und NIFTY,BANKNIFTY  --dry-run (compute and print, write nothing)

It reads vrp_eod, ivcm_eod and rv_eod and writes regime_eod only. Run it after
pmt-ivhist and pmt-vol.
"""

from __future__ import annotations

import argparse
import logging
import sys
from collections import Counter
from datetime import date

from . import config
from .vol import HORIZON, _f, _upsert

log = logging.getLogger("regime")

MODEL = "regime-1"
LEVEL = (0.33, 0.67)
TERM = 0.005
TREND = (0.8, 1.25)
PREMIUM = 0.01
STRESS_PCT = 0.80

# (label, rule text, test on the buckets and the level percentile), first match wins.
RULES = [
    ("stress", "level pct >= 0.80 and term inverted",
     lambda b, p: p >= STRESS_PCT and b["term"] == "inverted"),
    ("elevated", "level high", lambda b, p: b["level"] == "high"),
    ("waking", "realized rising, level not high", lambda b, p: b["trend"] == "rising"),
    ("calm", "level low, term up, realized not rising",
     lambda b, p: b["level"] == "low" and b["term"] == "up"),
    ("normal", "none of the above", lambda b, p: True),
]


def bucket(x: float | None, lo: float, hi: float, names: tuple[str, str, str]) -> str | None:
    if x is None:
        return None
    return names[0] if x < lo else names[2] if x > hi else names[1]


def classify(level_pct: float | None, slope: float | None, rv_ratio: float | None,
             premium: float | None) -> dict:
    b = {"level": None if level_pct is None else "low" if level_pct < LEVEL[0] else
         "high" if level_pct >= LEVEL[1] else "mid",
         "term": bucket(slope, -TERM, TERM, ("inverted", "flat", "up")),
         "trend": bucket(rv_ratio, TREND[0], TREND[1], ("falling", "steady", "rising")),
         "premium": bucket(premium, -PREMIUM, PREMIUM, ("cheap", "fair", "rich"))}
    label, rule = None, None
    # Level and term are needed for any label; trend and premium only inform.
    if b["level"] is not None and b["term"] is not None:
        for name, text, test in RULES:
            if test(b, level_pct):
                label, rule = name, text
                break
    return {**b, "label": label, "rule": rule}


def compute(und: str, vrp: dict[date, tuple], cm: dict[date, dict[int, float]],
            rv: dict[date, dict[int, float]], use_vix: bool) -> list[dict]:
    rows = []
    for d in sorted(set(vrp) | set(cm)):
        iv_pct, vix_pct, iv_ante, vix_ante = vrp.get(d, (None,) * 4)
        pct, prem = (vix_pct, vix_ante) if use_vix and vix_pct is not None else (iv_pct, iv_ante)
        src = "vix" if use_vix and vix_pct is not None else "iv30" if iv_pct is not None else None
        c = cm.get(d, {})
        far = c.get(63, c.get(42))
        slope = far - c[21] if far is not None and 21 in c else None
        r = rv.get(d, {})
        ratio = r[10] / r[63] if r.get(10) and r.get(63) else None
        k = classify(pct, slope, ratio, prem)
        rows.append({"trade_date": d, "underlying": und, "level_src": src, "level_pct": pct, "term_slope": slope,
                     "rv_ratio": ratio, "premium": prem, "level": k["level"], "term": k["term"],
                     "trend": k["trend"], "prem": k["premium"], "label": k["label"], "rule": k["rule"],
                     "model_version": MODEL})
    return rows


def report(rows: list[dict], n: int = 10) -> list[str]:
    out = []
    labelled = [r for r in rows if r["label"]]
    if not labelled:
        return out
    out.append("  date          level         term          trend        premium      label")
    f = lambda x, s=100, w=6: f"{s * x:{w}.1f}" if x is not None else " " * (w - 1) + "-"
    for r in labelled[-n:]:
        out.append(f"    {r['trade_date']}  {f(r['level_pct'])} {r['level'] or '-':5}  {f(r['term_slope'])} "
                   f"{r['term'] or '-':8}  {f(r['rv_ratio'], 1, 5)} {r['trend'] or '-':7}  {f(r['premium'])} "
                   f"{r['prem'] or '-':5}  {r['label']}")
    cnt = Counter(r["label"] for r in labelled)
    out.append("  days by label: " + ", ".join(f"{k} {v}" for k, v in cnt.most_common()))
    return out


# ---- the job ------------------------------------------------------------------

_RG = _upsert("regime_eod", ["trade_date", "underlying", "level_src", "level_pct", "term_slope", "rv_ratio", "premium",
                             "level", "term", "trend", "prem", "label", "rule", "model_version"],
              ("trade_date", "underlying"))


def _load(cur, und: str):
    cur.execute("SELECT trade_date, iv_pct, vix_pct, iv_ante, vix_ante FROM vrp_eod "
                "WHERE underlying = %s AND horizon = %s", (und, HORIZON))
    vrp = {d: tuple(_f(x) if x is not None else None for x in rest) for d, *rest in cur.fetchall()}
    cur.execute("SELECT trade_date, tenor, atm_iv FROM ivcm_eod WHERE underlying = %s", (und,))
    cm: dict[date, dict[int, float]] = {}
    for d, t, a in cur.fetchall():
        cm.setdefault(d, {})[int(t)] = float(a)
    cur.execute("SELECT trade_date, win, yz FROM rv_eod WHERE underlying = %s AND win IN (10, 63) AND yz > 0", (und,))
    rv: dict[date, dict[int, float]] = {}
    for d, w, y in cur.fetchall():
        rv.setdefault(d, {})[int(w)] = float(y)
    return vrp, cm, rv


def main(argv: list[str] | None = None) -> int:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s: %(message)s")
    ap = argparse.ArgumentParser(prog="pmt-regime", description="Vol regime labels from visible rules.")
    ap.add_argument("--und", help="comma-separated underlyings (default: the configured ones)")
    ap.add_argument("--dry-run", action="store_true")
    a = ap.parse_args(argv)

    import psycopg

    from .eodiv import database_url

    cfg = config.load()
    unds = a.und.split(",") if a.und else [u.name for u in cfg.underlyings]
    with psycopg.connect(database_url()) as conn, conn.cursor() as cur:
        for und in unds:
            vrp, cm, rv = _load(cur, und)
            if not vrp and not cm:
                print(f"regime: {und}: no vrp_eod or ivcm_eod rows; skipped")
                continue
            rows = compute(und, vrp, cm, rv, und == cfg.vix_underlying)
            print(f"regime: {und}: {len(rows)} days, {sum(r['label'] is not None for r in rows)} labelled")
            for line in report(rows):
                print(line)
            if a.dry_run:
                continue
            cur.executemany(_RG, rows)
            conn.commit()
    if a.dry_run:
        print("regime: dry run, nothing written")
    return 0


if __name__ == "__main__":
    sys.exit(main())
