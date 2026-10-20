# Personal Market Terminal: Phase 0.
GO        ?= go
BIN       := bin
CMDS      := ticker recorder jobs login backuprec sys fakekite
COMPOSE   ?= docker compose
DEV_DB    ?= postgres://postgres:dev@127.0.0.1:55432/pmt?sslmode=disable
DEV_NATS  ?= nats://127.0.0.1:54222

.PHONY: all build test test-int vet tidy migrate up down logs ps dryrun dryrun-job dryrun-down restore-test clean

all: vet test build

build:
	@mkdir -p $(BIN)
	@for c in $(CMDS); do $(GO) build -trimpath -o $(BIN)/$$c ./cmd/$$c || exit 1; done

test:
	$(GO) test ./...

# Integration tests need the dev Postgres and NATS containers.
test-int:
	PMT_TEST_DB=$(DEV_DB) PMT_TEST_NATS=$(DEV_NATS) $(GO) test -count=1 ./...

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

migrate:
	DATABASE_URL=$(DEV_DB) $(GO) run ./cmd/jobs migrate

up:
	$(COMPOSE) up -d --build

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f --tail=100

ps:
	$(COMPOSE) ps

# Full stack against fakekite with every clock shifted so "now" is START IST on
# DATE (today when empty), e.g. make dryrun START=09:14:30 DATE=2026-10-19.
# Pick a trading day: on a holiday nothing is cut. Uses its own compose project and volumes.
START ?= 09:14:30
DATE  ?=
DRYKEY := data/dryrun/age-key.txt
DRYRUN  = $(COMPOSE) -p pmt-dryrun -f docker-compose.yml -f deploy/dryrun.override.yml
$(DRYKEY):
	@mkdir -p $(dir $@)
	$(GO) run filippo.io/age/cmd/age-keygen -o $@ 2>/dev/null
	@chmod 644 $@   # throwaway key; the container user must read it
dryrun: $(DRYKEY)
	DRYRUN_AGE_RECIPIENT=$$(sed -n 's/^# public key: //p' $(DRYKEY)) \
	PMT_CLOCK_OFFSET=$$($(GO) run ./cmd/jobs offset $(START) $(DATE)) \
	  $(COMPOSE) -p pmt-dryrun -f docker-compose.yml -f deploy/dryrun.override.yml up -d --build
	@echo "dry run up; SYS page on http://127.0.0.1:18090"

# Run one job inside the dry run, e.g. make dryrun-job JOB="backup".
dryrun-job: $(DRYKEY)
	DRYRUN_AGE_RECIPIENT=$$(sed -n 's/^# public key: //p' $(DRYKEY)) $(DRYRUN) exec jobs jobs $(JOB)

dryrun-down:
	$(DRYRUN) down -v

# On the backup host: make restore-test DIR=/srv/pmt-backups KEY=~/.config/pmt/age-key.txt
restore-test:
	deploy/restore-test.sh "$(DIR)" "$(KEY)"

clean:
	rm -rf $(BIN)
