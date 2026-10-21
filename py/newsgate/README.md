# newsgate: the strategy hook for the news risk filter

News is a filter on **new entries**, never a trade signal. newsmon publishes a risk
state, and the gateway serves it:

| Endpoint | What |
|---|---|
| `GET /api/risk-state` | `{ts, state, reasons[], expires_at, since, stale}`; 503 until newsmon has published |
| WS `news.risk` | the same object, pushed on every change |
| `GET /api/risk-log?from=YYYY-MM-DD&to=YYYY-MM-DD` | every logged level change, plus the one in force at `from` (`prior`), for backtests |

`state` is `NORMAL`, `CAUTION` or `NO_NEW_ENTRIES`. Each reason carries its own
`until`, and `expires_at` is when the state may next ease.

## Files

- `newsgate.py`: `GateConfig`, `decide()` and `NewsGate`. One file, stdlib only, Python 3.9+, so a
  strategy can vendor it.
- `replay.py`: answers "would the trades this filter blocks have lost money?"
- `test_newsgate.py`: `make news-gate-test`.

## What the gate does

| State | New entry | Existing positions |
|---|---|---|
| NORMAL | allowed, size × 1 | untouched |
| CAUTION | allowed at size × `caution_size` (0.5); blocked if `caution_size: 0` | untouched |
| NO_NEW_ENTRIES | blocked (`news_no_new_entries`) | untouched, unless `force_close` is on |
| unknown | blocked (`news_unknown`) unless `on_unknown: allow` | untouched |

"Unknown" means the gateway is down, slow past `timeout_s`, answers non-200, sends
bad JSON, or the state is marked stale or is older than `stale_s`. An unknown state
is not NORMAL, so the gate fails closed by default.

`force_close` is off by default. When on, a NO_NEW_ENTRIES decision has
`flatten=True`. That is advice: the gate never places or cancels an order, and
whether and how to close stays the strategy's call.

## Wiring it into ~/algo-trading (not done for you)

The bot is untouched. To wire it in, `engine/risk.py` `entry_vetoes` is the natural
place, because an empty list there already means "the entry may proceed":

```python
# engine/risk.py
from newsgate import GateConfig, NewsGate   # vendor py/newsgate/newsgate.py

_gate = None

def news_gate(cfg: dict) -> NewsGate:
    global _gate
    if _gate is None:
        _gate = NewsGate(GateConfig.from_dict(cfg.get("news_gate")))
    return _gate

def entry_vetoes(state, new_risk_frac, regime_on, cfg) -> list[str]:
    ...
    vetoes += news_gate(cfg).vetoes()        # last, after the bot's own checks
    return vetoes
```

To use CAUTION's smaller size, the sizing code calls `news_gate(cfg).entry()` and
multiplies the quantity by `size_mult`. Call the gate only for **new** entries: exits,
stops and rebalances that reduce a position must never consult it.

```yaml
# config.yaml
news_gate:
  enabled: true
  url: http://127.0.0.1:8091/api/risk-state
  timeout_s: 2
  cache_s: 5          # one HTTP call per 5s at most
  stale_s: 60
  caution_size: 0.5   # 0 = CAUTION blocks too
  on_unknown: block   # or allow
  force_close: false  # advisory flatten flag on NO_NEW_ENTRIES
```

Unknown keys and wrong types in this section raise `ValueError` at start-up, so a typo
can't silently turn the gate off.

## Backtest: would the blocked trades have lost money?

```bash
python3 py/newsgate/replay.py --ledger ~/algo-trading/data/ledger_paper.db
```

The script opens the ledger read-only, fetches `/api/risk-log` across the ledger's date
range, labels each trade with the state in force at `--entry-at` (09:15 IST by default)
on its entry date, and prints totals by state. It also prints a second table, "touched",
for trades whose session saw any non-NORMAL state at all. Options: `--engine`, `--json`
and `--gateway`.

Caveats:

- The ledger keeps entry dates, not times, so entry times are assumed. Try a few
  `--entry-at` values.
- The log has a row only when the level changes, and it starts when newsmon first ran.
  Earlier trades are `UNLOGGED`, and a period when newsmon was down looks like its last
  state.
- A handful of trades proves nothing either way.
