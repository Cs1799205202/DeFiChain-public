# Common tasks. Everything also works with plain `go` commands; see README.md.

# Contracts compiled into client/artifacts (embedded in the Go SDK, read by pysdk).
# Add your own contract's name here, then run `make contracts`.
CONTRACTS := Token AMM Lending FeedOracle AMMOracle

.PHONY: all build test test-public vet devnet sim contracts forge-test check clean

all: build vet test-public

build:
	go build ./...

vet:
	go vet ./...

# Part 1 public tests: unit tests of your chain code, then the multi-node tests.
test-public:
	go test -count=1 ./chain/ ./tests/

# Everything, with the race detector (slower).
test:
	go test -race -count=1 ./...

devnet:
	go run ./cmd/devnet

sim:
	go run ./cmd/sim

# Needs solc >= 0.8.24 (https://docs.soliditylang.org/en/latest/installing-solidity.html).
contracts:
	solc --optimize --abi --bin --overwrite -o build/solc contracts/*.sol
	@for c in $(CONTRACTS); do cp build/solc/$$c.abi build/solc/$$c.bin client/artifacts/; done
	@echo "compiled $(CONTRACTS) into client/artifacts/"

# Solidity unit tests with Foundry (optional): https://book.getfoundry.sh
forge-test:
	cd contracts && forge test

# Before you submit: are you only changing files you are allowed to change?
check:
	go run ./cmd/labcheck

clean:
	rm -rf build devnet-data
