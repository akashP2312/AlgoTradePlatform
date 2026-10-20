#!/bin/sh
# Monthly restore test, run on the backup host (where the age private key lives),
# not on the VM. It starts a throwaway Timescale container, restores the newest
# dump with `jobs restore-test`, compares row counts against the manifest, and
# removes everything afterwards.
#   deploy/restore-test.sh /srv/pmt-backups ~/.config/pmt/age-key.txt
set -eu
DIR=${1:?usage: restore-test.sh <backup dir> <age identity file>}
KEY=${2:?usage: restore-test.sh <backup dir> <age identity file>}
cd "$(dirname "$0")/.."
P=pmt-restoretest
PW=restore-test-$$
cleanup() { docker rm -f $P-db >/dev/null 2>&1 || true; docker network rm $P >/dev/null 2>&1 || true; }
trap cleanup EXIT
docker network create $P >/dev/null
docker run -d --name $P-db --network $P -e POSTGRES_USER=pmt -e POSTGRES_DB=pmt \
  -e POSTGRES_PASSWORD=$PW timescale/timescaledb:2.30.2-pg17 >/dev/null
until docker exec $P-db pg_isready -U pmt -d pmt >/dev/null 2>&1; do sleep 1; done
docker build -q -t pmt:phase0 . >/dev/null
URL=postgres://pmt:$PW@$P-db:5432/pmt?sslmode=disable
docker run --rm --network $P -e DATABASE_URL=$URL -e PMT_DATA_DIR=/tmp pmt:phase0 jobs migrate
docker run --rm --network $P -e DATABASE_URL=$URL -e PMT_DATA_DIR=/tmp \
  -e PMT_BACKUP_AGE_IDENTITY_FILE=/run/age-key.txt \
  -v "$(cd "$DIR" && pwd)":/backups:ro -v "$KEY":/run/age-key.txt:ro \
  pmt:phase0 sh -c '/usr/local/bin/jobs restore-test -file "$(ls -1 /backups/*.age | sort | tail -n1)"'
