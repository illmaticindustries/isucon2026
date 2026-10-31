# Compatibility entry points; root tools verify the revision was pushed.
export INDEXES_REF ?= HEAD
.PHONY: deploy-indexes verify-indexes
deploy-indexes verify-indexes:
	$(MAKE) -C ../.. $@ REF="$$INDEXES_REF" HOSTS=asia2
