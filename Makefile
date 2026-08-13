# Host packages hold the shared Envelope/ABI/chain-config code; the workflow
# mains are //go:build wasip1 and are addressed separately, since `./...` on a
# host toolchain excludes every file in them.
HOST_PKGS := ./pkg/... ./contracts/...
WASM_PKGS := ./queue-keeper/ ./strategy-keeper/

.PHONY: tidy fmt vet test build check fixtures simulate-queue simulate-strategy simulate supported-chains

tidy:
	go mod tidy

fmt:
	gofmt -w pkg contracts queue-keeper strategy-keeper

vet:
	go vet $(HOST_PKGS)
	GOOS=wasip1 GOARCH=wasm go vet $(WASM_PKGS)

test:
	go test $(HOST_PKGS)

build:
	GOOS=wasip1 GOARCH=wasm go build -o /tmp/queue-keeper.wasm ./queue-keeper/
	GOOS=wasip1 GOARCH=wasm go build -o /tmp/strategy-keeper.wasm ./strategy-keeper/

# What CI runs.
check: vet test build

# Regenerate the Solidity-derived Envelope fixtures. Requires Foundry + jq.
fixtures:
	./scripts/gen-envelope-fixtures.sh

simulate-queue:
	cre workflow simulate queue-keeper --target staging-settings --trigger-index 0

simulate-strategy:
	cre workflow simulate strategy-keeper --target staging-settings --trigger-index 0

simulate: simulate-queue simulate-strategy

supported-chains:
	cre workflow supported-chains
