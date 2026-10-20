# Personal Market Terminal: Phase 0

Phase 0 records the Indian F&O market for a **read-only** personal terminal:
ticks, option-chain snapshots, the 15:25 sweep, a REST backup recorder, the
nightly 1-minute harvest, and the NSE end-of-day files. There is no order code
path anywhere: the Kite client's guard allows only market-data and account
reads plus the login exchange, and refuses everything else.

## Services (docker-compose.yml)

| Service    | Does                                                                 |
|------------|----------------------------------------------------------------------|
| db         | TimescaleDB 2.30 / Postgres 17                                       |
| nats       | NATS with JetStream (ticks, snapshots, session KV)                   |
| migrate    | applies `migrations/` and exits                                      |
| ticker     | Kite WebSocket feeds → NATS; builds per-minute chain snapshots       |
| recorder   | NATS → Timescale, batched                                            |
| backuprec  | independent REST poller of the core window → JSONL (token file only) |
| jobs       | timetable: instruments, calendar, sweep, gaps, merge, harvest, bhav, participant OI, VIX, backup |
| sys        | the bare SYS page and `/metrics` on 127.0.0.1:8090                   |
| prometheus | scrapes sys, 30 days                                                 |
| login      | (`--profile tools`) the daily human 2FA login                        |
| fakekite   | (dry run only) a synthetic Kite with the same wire formats           |

All ports bind 127.0.0.1. Reach them over Tailscale (`tailscale serve`) or SSH.

## Quick start (development machine)

```bash
cp .env.example .env
make all
make dryrun START=09:14:30 DATE=2026-10-19
```

The dry run starts the whole stack against `fakekite` with every clock shifted
so that "now" is 09:14:30 IST on DATE (today if DATE is left out), logs in with a fake request token, and serves the
SYS page on http://127.0.0.1:18090. `make dryrun-job JOB=backup` runs one job
inside it; `make dryrun-down` removes it and its volumes. Pick a trading day
for DATE: on a holiday the recorder correctly cuts no chains and the backup
recorder stays idle.

Integration tests need the dev Postgres and NATS containers (`make test-int`).

## Layout

```
cmd/        ticker recorder jobs login backuprec sys fakekite
internal/   kite (REST, WS, guard), ticker, recorder, sweep, harvest, loaders,
            refdata (instrument SCD), calendar, backup, backuprec, store, bus, ...
migrations/ SQL, embedded and applied by `jobs migrate`
config/     pmt.yaml, calendars, expiry rules, index constituents
deploy/     nats.conf, prometheus.yml, dry-run override, offsite and restore scripts
docs/       runbook.md
```

Facts not yet checked against the exchange or broker are marked `verify` in
code and config; `grep -rn -i verify` lists them.

See [docs/runbook.md](docs/runbook.md) for the VM, the daily login, backups and
the Phase 0 gate.
