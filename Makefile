SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
export REF ?= HEAD
export HOSTS ?= asia1 europe1 america1
export SSH_OPTIONS ?= -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=yes

.PHONY: help build deploy preflight rollback status logs deploy-indexes verify-indexes go-build-local nginx-deploy alp-install
help:
	@echo 'make preflight | build | deploy | rollback | status | logs [REF=<pushed commit>] [HOSTS="asia1 europe1 america1"]'
	@echo 'make deploy-indexes | verify-indexes REF=<pushed commit> HOSTS=asia2'
	@echo 'make nginx-deploy REF=<pushed commit> | alp-install | alp | nginx-log [HOSTS=...]'
	@echo 'Mutations require PORTAL_IDLE=yes after checking the portal.'
	@echo 'After Enqueue: make measure [MEASURE_SECONDS=60]'
	@echo 'Before Enqueue: make bench-reserve PORTAL_IDLE=yes; after completion: make bench-release PORTAL_IDLE=yes OPERATION_TOKEN=<token>'

build deploy preflight rollback status logs deploy-indexes verify-indexes nginx-log alp nginx-deploy alp-install:
	python3 scripts/deploy.py $@ --ref "$$REF" --hosts "$$HOSTS"

go-build-local: build

.PHONY: nginx-log alp journalctl deployed-version
journalctl: logs
deployed-version: status

# Portal confirmation is required for mutations and benchmark reservation.
export PORTAL_IDLE ?= no
export OPERATION_TOKEN ?=
export BENCH_NOTE ?= portal benchmark

.PHONY: enable-pgstats bench-reserve bench-release operation-status
enable-pgstats:
	python3 scripts/deploy.py $@ --ref "$$REF" --hosts asia2

bench-reserve:
	python3 scripts/operation_lock.py reserve

bench-release:
	python3 scripts/operation_lock.py release

operation-status:
	python3 scripts/operation_lock.py status

# Run right after Enqueue: top/pprof during load, then alp, logs and pg_stat_statements into measure/.
export MEASURE_SECONDS ?= 60
.PHONY: measure
measure:
	python3 scripts/measure.py
