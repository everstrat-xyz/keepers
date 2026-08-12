.PHONY: tidy simulate-queue simulate-strategy simulate supported-chains

tidy:
	go mod tidy

simulate-queue:
	cre workflow simulate queue-keeper --target staging-settings --trigger-index 0

simulate-strategy:
	cre workflow simulate strategy-keeper --target staging-settings --trigger-index 0

simulate: simulate-queue simulate-strategy

supported-chains:
	cre workflow supported-chains
