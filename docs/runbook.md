# Runbook: Phase 0

## 1. VM

- A VM in an Indian region (Mumbai or Hyderabad), 4 vCPU, 16 GB RAM, 200 GB SSD
  to start. Ubuntu LTS, Docker Engine with the compose plugin, Tailscale.
- No inbound ports open to the internet except what the provider needs for SSH
  (better: SSH over Tailscale only, then close 22 publicly).
- Set the host clock to sync (chrony/systemd-timesyncd); containers run in IST.

```bash
sudo mkdir -p /opt/pmt && sudo chown $USER /opt/pmt
rsync -a --exclude .git --exclude data --exclude .env ./ vm:/opt/pmt/
ssh vm 'cd /opt/pmt && cp .env.example .env && chmod 600 .env'
```

Fill `.env` on the VM by hand: the Kite app key and secret, a database
password, the Telegram bot (optional) and the backup **public** key.

```bash
docker compose up -d --build
docker compose ps
```

The Kite Connect app's redirect URL must point at the login callback you use
(see below). Verify the login flow and session expiry time against current Kite
documentation before the first live session.

## 2. Daily login (before 09:00 IST)

The Kite access token expires each morning (around 06:00 IST, verify). Once a
trading day:

```bash
ssh vm
cd /opt/pmt && docker compose run --rm login
```

It prints the Kite login URL. Open it, sign in with 2FA, and paste the redirect
URL back. The session goes to NATS KV (ticker, jobs) and to the token file
(backup recorder). The token is never printed or logged. Never call logout:
that kills the session for the day.

If the login is missed, the ticker and backup recorder wait and alert; nothing
records until it is done. Afterwards, `jobs gaps` marks the missing minutes and
the 17:30 harvest refills 1-minute candles.

## 3. Daily timetable (IST, `jobs schedule`)

| Time  | Job          | Notes                                    |
|-------|--------------|------------------------------------------|
| 06:15 | instruments  | instrument dump → SCD                    |
| 06:20 | calendar     | sessions and expiries                    |
| 15:25 | sweep        | REST snapshot of every live chain        |
| 15:45 | gaps         | first pass                               |
| 16:15 | merge-backup | backup recorder JSONL → db               |
| 17:30 | harvest      | 1-minute candles with OI                 |
| 19:00 | bhav         | F&O bhavcopy (UDiFF)                     |
| 19:05 | partoi       | participant-wise OI                      |
| 19:10 | vix          | India VIX and index closes               |
| 19:30 | gaps         | final pass                               |
| 01:30 | backup       | encrypted dump to the outbox             |

Run a job by hand: `docker compose exec jobs jobs <job> [args]`; `jobs` with no
arguments lists them. Every run is recorded in `job_runs` and shown on SYS.

Day one: `docker compose exec jobs jobs backfill` fetches the candles Kite still
serves (lookback in `harvest.lookback_days`).

## 4. Backups

- Make an age keypair **on the backup host**, not the VM:
  `age-keygen -o ~/.config/pmt/age-key.txt`. Put only the public key (`age1...`)
  in the VM's `.env` as `PMT_BACKUP_AGE_RECIPIENT`.
- 01:30: `jobs backup` streams `pg_dump -Fc` through age into the outbox with
  a manifest of row counts, and prunes local copies older than 14 days.
- 02:15: `deploy/offsite.sh` (host cron) copies the outbox to
  `PMT_OFFSITE_DEST` with rsync over Tailscale. It never deletes anything at the
  destination. Only ciphertext and the counts manifest leave the VM.

```cron
15 2 * * * cd /opt/pmt && deploy/offsite.sh >>/var/log/pmt-offsite.log 2>&1
```

- Restore test, monthly and once before the gate, on the backup host:

```bash
make restore-test DIR=/srv/pmt-backups KEY=~/.config/pmt/age-key.txt
```

It restores the newest dump into a throwaway Timescale container, checks that
every table has at least the manifest's row count, and removes the container.
Record the result line in the gate log below. (pg_restore may print Timescale
catalog warnings; verify which ones are expected.)

## 5. Monitoring

- SYS page: http://127.0.0.1:8090 on the VM (through `tailscale serve` or an SSH
  tunnel). Feed state, last tick per connection, snapshot minutes, job runs, gaps,
  disk.
- Prometheus: 127.0.0.1:9090, scraping sys only.
- Telegram alerts: levels, counts and P&L only; never tokens or URLs.

## 6. Upgrades

```bash
rsync -a --exclude .git --exclude data --exclude .env ./ vm:/opt/pmt/
ssh vm 'cd /opt/pmt && docker compose up -d --build'
```

Upgrade outside market hours (after 19:30 IST or on a holiday). Migrations run
on start through the `migrate` service.

## 7. Phase 0 gate

Phase 0 is done when all of these hold, recorded here with dates:

- [ ] 5 consecutive trading sessions recorded unattended, apart from the login.
- [ ] Gaps found and marked by `jobs gaps` on each of those days, with none left
      unexplained.
- [ ] Harvest complete for each of those days (job_runs green, candle counts sane).
- [ ] bhav, partoi and vix loaded for each of those days.
- [ ] One restore test passed on the backup host from an offsite copy.

| Date | Session | Gaps | Harvest | EOD | Notes |
|------|---------|------|---------|-----|-------|

## 8. Phase 3 gate

The gate: scenario P&L reconciles with broker P&L on real positions. It runs on a
live day (the login in section 2) with real F&O positions open in the account; the
dry-run stack's fakekite cannot pass it. Everything below only reads: KV gets, the
POS and RISK panels, and SELECTs.

It checks three things, from cheapest to most telling:

1. **Zero shock.** Repricing every leg at the broker's own LTP gives the broker's
   P&L. The POS panel's check column shows the difference per row and flags
   anything over ₹1.
2. **Live M2M.** RISK's M2M (the broker's M2M plus tick moves on fresh legs) sits
   on the broker's M2M right after each positions pull.
3. **Prediction.** The spot-vol path row of a captured grid, read at the spot move
   that then happened, plus theta for the elapsed time, matches the change in the
   broker's M2M over the same interval.

### On the day

1. After the login and 09:15, open POS and RISK. Confirm every open leg is
   modelled: no `risk_legs_flagged` alert, `unmod` empty, no flagged rows in POS.
   A flagged or unmodelled leg puts the book out of scope for the day; note it and
   stop.
2. **Zero shock**: at 09:30, 12:00 and 15:00, check that every POS check cell reads
   0. Note any row that doesn't, with its symbol and the difference.
3. **Live M2M**: at the same times, compare RISK's M2M with the broker M2M figure
   (`pnl.bm2m`) within a few seconds of a positions pull. They differ by tick moves
   since the pull, so on a quiet tape they should agree to within a few hundred
   rupees.
4. **Prediction**: capture risk.book every 30 minutes from 09:30 to 15:15:

   ```bash
   mkdir -p gate3/$(date +%F)
   scripts/gate3/capture.sh > gate3/$(date +%F)/$(date +%H%M).json
   ```

   `capture.sh` defaults to the container `pmt-riskengine-1`; check the name with
   `docker ps` on the VM and pass it as the first argument if it differs.
   Then compare consecutive captures (and wider pairs, e.g. 09:30 → 15:15):

   ```bash
   jq -n --slurpfile a gate3/DAY/0930.json --slurpfile b gate3/DAY/1000.json -f scripts/gate3/compare.jq
   ```

   Read the output fields as follows:
   - `pred`: the path row at `move_pct` plus `pred_theta`.
   - `broker`: the change in the broker's M2M.
   - `err`: `pred` minus `broker`.
   - `terminal`: the change in RISK's own M2M, for reference.
   - `same_legs`: must be true. A fill between the captures breaks the comparison.
   - `flagged`: must be 0.
   - `broker_age_s`: must be under a minute at both ends, so the broker figure is
     fresh.
5. After the close, summarise the day from the minute snapshots:

   ```sql
   -- docker exec pmt-db-1 psql -U pmt -d pmt
   SELECT trade_date, count(*) AS mins,
          min(ts AT TIME ZONE 'Asia/Kolkata')::time AS first,
          max(ts AT TIME ZONE 'Asia/Kolkata')::time AS last,
          round(min(m2m)) AS low, round(max(m2m)) AS high,
          round((array_agg(m2m ORDER BY ts DESC))[1]) AS close_m2m,
          max(flags) AS max_flags, min(legs) AS min_legs, max(legs) AS max_legs
   FROM risk_snapshots GROUP BY trade_date ORDER BY trade_date DESC LIMIT 5;
   ```

   `close_m2m` should match the M2M the broker shows after the close, within the
   last minute's move. The snapshots don't store the broker's M2M (`bm2m`) or the
   grid, so the prediction check depends on the captures above; keep them.

### Pass criteria (verify the tolerances after the first day)

An interval counts when `same_legs` is true, `flagged` is 0, both ends are fresh,
and the spot moved by at least 0.3%. Below that, quote noise and the broker's LTP
lag swamp the signal. A counted interval passes when |`err`| ≤ max(₹1,000, 10% of
|`broker`|). The vol path is beta's typical response, not the day's actual vol
move, so a large miss on a day the smile shifted is a finding to explain, not
automatically a fail.

Phase 3 is done when all of these hold:

- [ ] 3 sessions with real positions: zero-shock checks all 0, and live M2M in
      line at every check.
- [ ] At least 8 counted intervals across those sessions, with at least 80%
      passing. Every miss is explained in Notes (vol shift, fill, stale leg).
- [ ] Close M2M from the snapshots matches the broker's closing M2M on each of
      those days.

| Date | Legs | Zero shock | Live M2M | Intervals (pass/counted) | Close M2M vs broker | Notes |
|------|------|------------|----------|--------------------------|---------------------|-------|
