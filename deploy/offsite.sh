#!/bin/sh
# Copy the encrypted backup outbox off this machine. Run from cron on the VM
# host after the 01:30 backup job, e.g.
#   15 2 * * * cd /opt/pmt && deploy/offsite.sh >>/var/log/pmt-offsite.log 2>&1
# Files are age-encrypted before they reach the outbox, so the destination only
# ever holds ciphertext. Nothing is deleted at the destination.
set -eu
cd "$(dirname "$0")/.."
DEST=$(sed -n 's/^PMT_OFFSITE_DEST=//p' .env)
[ -n "$DEST" ] || { echo "PMT_OFFSITE_DEST is not set in .env" >&2; exit 1; }

STAGE=$(mktemp -d)
trap 'rm -rf "$STAGE"' EXIT
# The outbox lives in the appdata volume; copy it out of the running jobs container.
docker compose cp jobs:/data/backup/outbox/. "$STAGE"/
n=$(ls "$STAGE" | wc -l | tr -d ' ')
[ "$n" -gt 0 ] || { echo "$(date -u +%FT%TZ) outbox empty" >&2; exit 1; }
rsync -a --ignore-existing --partial "$STAGE"/ "$DEST"
echo "$(date -u +%FT%TZ) offsite: $n files to $DEST"
