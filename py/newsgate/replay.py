"""Would the trades this filter blocks have lost money?

Reads the strategy's trade ledger (read-only) and the logged risk-state changes
(/api/risk-log), labels each trade with the news state when it was entered, and
sums the outcomes by state. The NO_NEW_ENTRIES group is what the gate would have
blocked, and CAUTION what it would have shrunk. If those groups did no worse than
NORMAL, the filter costs more than it saves.

    python3 py/newsgate/replay.py --ledger ~/algo-trading/data/ledger_paper.db

Caveats, also printed with the result:
  * The ledger keeps entry dates, not times, so a trade counts as entered at
    --entry-at IST on that date. "touched" marks a trade whose session had any
    non-NORMAL state, in case the real entry came at another time.
  * The log has a row only when the level changes, and cannot show when newsmon
    was down; a trade from before the first logged change is UNLOGGED.
  * A few dozen trades say little. This is a sanity check, not a verdict.

Stdlib only, Python 3.9+.
"""

from __future__ import annotations

import argparse
import json
import sqlite3
import sys
import urllib.parse
import urllib.request
from dataclasses import dataclass
from datetime import date, datetime, time as dtime, timedelta, timezone
from typing import Any, Dict, Iterable, List, Optional, Sequence

IST = timezone(timedelta(hours=5, minutes=30))
SESSION = (dtime(9, 15), dtime(15, 30))
MAX_DAYS = 400  # the gateway's limit per request
UNLOGGED = "UNLOGGED"
ORDER = ("NORMAL", "CAUTION", "NO_NEW_ENTRIES", UNLOGGED)


@dataclass
class Trade:
    id: Any
    symbol: str
    engine: str
    entry_date: str
    pnl: Optional[float]
    r: Optional[float]
    state: str = UNLOGGED
    touched: bool = False


def ms(d: date, t: dtime) -> int:
    return int(datetime.combine(d, t, IST).timestamp() * 1000)


def state_at(changes: Sequence[Dict[str, Any]], prior: Optional[Dict[str, Any]], t_ms: int) -> str:
    """The logged state at t_ms. changes are oldest first; prior is the last before them."""
    cur = prior["state"] if prior else UNLOGGED
    for c in changes:
        if c["ts"] > t_ms:
            break
        cur = c["state"]
    return cur


def touched(changes: Sequence[Dict[str, Any]], prior: Optional[Dict[str, Any]], d: date) -> bool:
    """Whether any non-NORMAL state held during d's session."""
    lo, hi = ms(d, SESSION[0]), ms(d, SESSION[1])
    if state_at(changes, prior, lo) not in ("NORMAL", UNLOGGED):
        return True
    return any(lo < c["ts"] <= hi and c["state"] != "NORMAL" for c in changes)


def label(trades: Iterable[Trade], changes: Sequence[Dict[str, Any]], prior: Optional[Dict[str, Any]], entry_at: dtime) -> List[Trade]:
    out = []
    for t in trades:
        d = date.fromisoformat(t.entry_date[:10])
        t.state = state_at(changes, prior, ms(d, entry_at))
        t.touched = touched(changes, prior, d)
        out.append(t)
    return out


def summarize(trades: Iterable[Trade], key: str = "state") -> List[Dict[str, Any]]:
    """Per group: trades, closed, wins, total and average pnl, average R."""
    groups: Dict[str, List[Trade]] = {}
    for t in trades:
        k = t.state if key == "state" else ("touched" if t.touched else "clean")
        groups.setdefault(k, []).append(t)
    order = list(ORDER) if key == "state" else ["clean", "touched"]
    rows = []
    for k in sorted(groups, key=lambda g: order.index(g) if g in order else len(order)):
        ts = groups[k]
        closed = [t for t in ts if t.pnl is not None]
        rs = [t.r for t in closed if t.r is not None]
        tot = sum(t.pnl for t in closed)  # type: ignore[misc]
        rows.append({
            "group": k, "n": len(ts), "closed": len(closed), "wins": sum(1 for t in closed if t.pnl > 0),  # type: ignore[operator]
            "pnl": round(tot, 2), "avg_pnl": round(tot / len(closed), 2) if closed else None,
            "avg_r": round(sum(rs) / len(rs), 3) if rs else None,
        })
    return rows


def read_ledger(path: str, engine: Optional[str] = None) -> List[Trade]:
    """The ledger's trades, opened read-only so a replay can never change it."""
    uri = "file:" + urllib.parse.quote(path) + "?mode=ro"
    con = sqlite3.connect(uri, uri=True)
    try:
        q = "SELECT id, symbol, engine, entry_date, pnl, r_multiple FROM trades"
        args: tuple = ()
        if engine:
            q, args = q + " WHERE engine = ?", (engine,)
        rows = con.execute(q + " ORDER BY entry_date, id", args).fetchall()
    finally:
        con.close()
    return [Trade(*r) for r in rows if r[3]]


def fetch_log(gateway: str, frm: date, to: date, timeout: float = 10) -> "tuple[Optional[Dict[str, Any]], List[Dict[str, Any]]]":
    """prior and changes over [frm, to], in requests of at most MAX_DAYS."""
    prior, changes, start = None, [], frm
    while start <= to:
        end = min(to, start + timedelta(days=MAX_DAYS - 1))
        url = f"{gateway.rstrip('/')}/api/risk-log?from={start.isoformat()}&to={end.isoformat()}"
        req = urllib.request.Request(url, headers={"Accept": "application/json", "User-Agent": "pmt-newsgate-replay/1"})
        with urllib.request.urlopen(req, timeout=timeout) as r:
            body = json.load(r)
        if start == frm:
            prior = body.get("prior")
        changes.extend(body.get("changes") or [])
        start = end + timedelta(days=1)
    return prior, changes


def render(by_state: List[Dict[str, Any]], by_touch: List[Dict[str, Any]], first_log: Optional[int]) -> str:
    def table(rows: List[Dict[str, Any]], head: str) -> List[str]:
        out = [f"{head:<16}{'trades':>7}{'closed':>7}{'wins':>6}{'pnl':>13}{'avg pnl':>11}{'avg R':>8}"]
        for r in rows:
            f = lambda v, p: "—" if v is None else f"{v:{p}}"  # noqa: E731
            out.append(f"{r['group']:<16}{r['n']:>7}{r['closed']:>7}{r['wins']:>6}{r['pnl']:>13,.2f}"
                       f"{f(r['avg_pnl'], ',.2f'):>11}{f(r['avg_r'], '.3f'):>8}")
        return out
    lines = table(by_state, "state at entry") + [""] + table(by_touch, "session")
    since = datetime.fromtimestamp(first_log / 1000, IST).strftime("%Y-%m-%d %H:%M IST") if first_log else "none"
    lines += ["", f"news state known from: {since} (earlier trades are UNLOGGED)",
              "NO_NEW_ENTRIES is what the gate would have blocked; CAUTION what it would have shrunk.",
              "Entry times are assumed (the ledger keeps dates); newsmon downtime is not visible in the log.",
              "A filter, not a signal: this asks whether it would have avoided losers, nothing more."]
    return "\n".join(lines)


def main(argv: Optional[Sequence[str]] = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    ap.add_argument("--ledger", required=True, help="the strategy's SQLite ledger (opened read-only)")
    ap.add_argument("--gateway", default="http://127.0.0.1:8091", help="the terminal gateway")
    ap.add_argument("--entry-at", default="09:15", help="assumed IST entry time, HH:MM (default 09:15)")
    ap.add_argument("--engine", help="only this engine's trades")
    ap.add_argument("--json", action="store_true", help="print JSON instead of tables")
    a = ap.parse_args(argv)
    try:
        entry_at = dtime.fromisoformat(a.entry_at)
    except ValueError:
        ap.error("--entry-at must be HH:MM")

    trades = read_ledger(a.ledger, a.engine)
    if not trades:
        print("no trades in the ledger" + (f" for engine {a.engine}" if a.engine else ""))
        return 0
    frm = date.fromisoformat(trades[0].entry_date[:10])
    to = date.fromisoformat(trades[-1].entry_date[:10])
    try:
        prior, changes = fetch_log(a.gateway, frm, to)
    except Exception as e:
        print(f"cannot read {a.gateway}/api/risk-log: {e}", file=sys.stderr)
        return 1
    label(trades, changes, prior, entry_at)
    by_state, by_touch = summarize(trades), summarize(trades, "touched")
    first = (prior or (changes[0] if changes else None) or {}).get("ts")
    if a.json:
        print(json.dumps({"from": frm.isoformat(), "to": to.isoformat(), "entry_at": a.entry_at,
                          "first_logged": first, "by_state": by_state, "by_session": by_touch}, indent=2))
    else:
        print(f"{len(trades)} trades {frm} to {to}, entries assumed at {a.entry_at} IST\n")
        print(render(by_state, by_touch, first))
    return 0


if __name__ == "__main__":
    sys.exit(main())
