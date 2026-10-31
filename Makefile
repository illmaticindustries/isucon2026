SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
export REF ?= HEAD
export HOSTS ?= asia1 europe1 america1
export SSH_OPTIONS ?= -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=yes

.PHONY: help build deploy preflight rollback status logs deploy-indexes verify-indexes go-build-local
help:
	@echo 'make preflight | build | deploy | rollback | status | logs [REF=<pushed commit>] [HOSTS="asia1 europe1 america1"]'
	@echo 'make deploy-indexes | verify-indexes REF=<pushed commit> HOSTS=asia2'

build deploy preflight rollback status logs deploy-indexes verify-indexes nginx-log alp:
	python3 scripts/deploy.py $@ --ref "$$REF" --hosts "$$HOSTS"

go-build-local: build

.PHONY: nginx-log alp journalctl deployed-version
journalctl: logs
deployed-version: status

.PHONY: enable-pgstats
enable-pgstats:
	git fetch origin
	git merge-base --is-ancestor "$$REF" origin/ops/make-deploy
	git show "$$REF:scripts/enable-pgstats.sh" | ssh $(SSH_OPTIONS) isucon@52.197.163.28 bash -s
