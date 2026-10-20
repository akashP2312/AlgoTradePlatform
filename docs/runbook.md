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
