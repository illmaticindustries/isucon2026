SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

HOST ?=
SSH_OPTIONS ?= -o BatchMode=yes -o ConnectTimeout=10
INDEXES_REF ?= HEAD

# Deploy the committed SQL, without copying secrets or changing server files.
# Usage: make -C webapp/sql deploy-indexes HOST=isucon@<asia2-public-ip>
.PHONY: deploy-indexes verify-indexes check-host
check-host:
	@test -n "$(HOST)" || { echo 'Set HOST to the registered asia2 SSH destination'; exit 1; }

deploy-indexes: check-host
	git show '$(INDEXES_REF):webapp/sql/004-performance-indexes.sql' | \
	  ssh $(SSH_OPTIONS) '$(HOST)' 'sleep 1; set -eu; . /home/isucon/env.sh; export PGPASSWORD="$$ISUREAL_DB_PASSWORD"; exec psql -X --no-password --set=ON_ERROR_STOP=1 -h "$$ISUREAL_DB_HOST" -p "$$ISUREAL_DB_PORT" -U "$$ISUREAL_DB_USER" -d "$$ISUREAL_DB_NAME"'
	$(MAKE) verify-indexes

verify-indexes: check-host
	git show '$(INDEXES_REF):webapp/sql/verify-performance-indexes.sql' | \
	  ssh $(SSH_OPTIONS) '$(HOST)' 'sleep 1; set -eu; . /home/isucon/env.sh; export PGPASSWORD="$$ISUREAL_DB_PASSWORD"; exec psql -X --no-password --set=ON_ERROR_STOP=1 -h "$$ISUREAL_DB_HOST" -p "$$ISUREAL_DB_PORT" -U "$$ISUREAL_DB_USER" -d "$$ISUREAL_DB_NAME"'
