FUNCTIONS := queue-keeper strategy-keeper

.PHONY: install lint test functions check

# `npm test` compiles each function to WASM and runs its specs against the
# compiled artifact through the raw-mock oracle harness.
install:
	@for f in $(FUNCTIONS); do \
		echo "== mimic-functions/$$f =="; \
		(cd mimic-functions/$$f && npm install) || exit 1; \
	done

lint:
	@for f in $(FUNCTIONS); do \
		echo "== mimic-functions/$$f =="; \
		(cd mimic-functions/$$f && npm run lint) || exit 1; \
	done

test:
	@for f in $(FUNCTIONS); do \
		echo "== mimic-functions/$$f =="; \
		(cd mimic-functions/$$f && npm test) || exit 1; \
	done

functions: test

# What CI runs.
check: lint test
