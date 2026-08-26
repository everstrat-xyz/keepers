FUNCTIONS := queue-keeper strategy-keeper

.PHONY: install lint typecheck test functions check

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

# The deploy/trigger scripts are plain TypeScript against the Mimic SDK, and
# eslint does not report type errors. Without this they went unchecked long
# enough for create-trigger.ts to ship a trigger config the API would reject.
typecheck:
	@for f in $(FUNCTIONS); do \
		echo "== mimic-functions/$$f =="; \
		(cd mimic-functions/$$f && npm run typecheck) || exit 1; \
	done

test:
	@for f in $(FUNCTIONS); do \
		echo "== mimic-functions/$$f =="; \
		(cd mimic-functions/$$f && npm test) || exit 1; \
	done

functions: test

# What CI runs.
check: lint typecheck test
