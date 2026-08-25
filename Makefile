# W4 (freeze-watch) remains a CRE-era Go workflow pending its own migration
# (deferred). W1/W2 now run as Mimic functions and live in mimic-functions/.
HOST_PKGS := ./pkg/... ./contracts/...
WASM_PKGS := ./freeze-watch/
FUNCTIONS := queue-keeper strategy-keeper

.PHONY: tidy fmt fmt-check vet lint test build check functions functions-test

tidy:
	go mod tidy

fmt:
	gofmt -w pkg contracts freeze-watch

# The gate CI enforces; `make check` runs it so a formatting failure fails
# locally first instead of on the PR.
fmt-check:
	@unformatted="$$(gofmt -l pkg contracts freeze-watch)"; \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed on: $$unformatted"; \
		exit 1; \
	fi

vet:
	go vet $(HOST_PKGS)
	GOOS=wasip1 GOARCH=wasm go vet $(WASM_PKGS)

# Requires golangci-lint v2 — v1 refuses to run when it was built with an older
# Go than this module targets:
#   go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
lint:
	golangci-lint run $(HOST_PKGS)
	GOOS=wasip1 GOARCH=wasm golangci-lint run ./freeze-watch/...

test:
	go test $(HOST_PKGS)

build:
	GOOS=wasip1 GOARCH=wasm go build -o /tmp/freeze-watch.wasm ./freeze-watch/

# W1/W2 — Mimic functions. `npm test` compiles the WASM and runs the
# raw-mock oracle specs against it.
functions:
	@for f in $(FUNCTIONS); do \
		echo "== mimic-functions/$$f =="; \
		(cd mimic-functions/$$f && npm test) || exit 1; \
	done

functions-test:
	@for f in $(FUNCTIONS); do \
		(cd mimic-functions/$$f && npm test) || exit 1; \
	done

# What CI runs.
check: fmt-check vet lint test build functions
