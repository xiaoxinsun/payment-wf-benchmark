COMPOSE_DIR := deploy/compose
V1  := docker compose -f $(COMPOSE_DIR)/shared.yml -f $(COMPOSE_DIR)/v1.yml
V1A := docker compose -f $(COMPOSE_DIR)/shared.yml -f $(COMPOSE_DIR)/v1.yml -f $(COMPOSE_DIR)/v1a.yml
V1B := docker compose -f $(COMPOSE_DIR)/shared.yml -f $(COMPOSE_DIR)/v1.yml -f $(COMPOSE_DIR)/v1b.yml
V2  := docker compose -f $(COMPOSE_DIR)/shared.yml -f $(COMPOSE_DIR)/v2.yml
MODULES := core engine-temporal engine-dbos sims harness

.PHONY: image up-v1a up-v1b up-v2 down build test lint check-core-deps spike-temporal spike-dbos

image:
	docker build -f deploy/Dockerfile -t ibps-bench:local .

# Only one variant stack runs at a time (fairness: no cross-variant interference).
up-v1a: down image
	$(V1A) up -d
up-v1b: down image
	$(V1B) up -d
up-v2: down image
	$(V2) up -d
down:
	-$(V1A) down --remove-orphans
	-$(V1B) down --remove-orphans
	-$(V2) down --remove-orphans

build:
	@for m in $(MODULES); do (cd $$m && go build ./...) || exit 1; done

test:
	@for m in $(MODULES); do (cd $$m && go test ./...) || exit 1; done

lint: check-core-deps
	@for m in $(MODULES); do (cd $$m && golangci-lint run ./...) || exit 1; done

# core must stay engine-free: fail if any engine SDK shows up in its dependency graph.
check-core-deps:
	@cd core && if go list -deps ./... | grep -E 'go\.temporal\.io|dbos-inc/dbos-transact'; then \
	  echo "FAIL: core imports an engine SDK"; exit 1; else echo "core deps clean"; fi

spike-temporal:
	cd spikes/temporal && go run .
spike-dbos:
	cd spikes/dbos && go run . basic

# One-off: the DBOS engine tests need their own database on dbos-db (see engine-dbos/payment/workflow_test.go).
test-db:
	docker exec ibps-dbos-db-1 psql -U dbos -d dbos_sys -c "CREATE DATABASE dbos_test" || true
