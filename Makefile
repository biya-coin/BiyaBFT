PACKAGE = github.com/meterio/supernova

GIT_COMMIT = $(shell git --no-pager log --pretty="%h" -n 1)
GIT_TAG = $(shell git tag -l --points-at HEAD)
METER_VERSION = $(shell cat cmd/supernova/VERSION)
DISCO_VERSION = $(shell cat cmd/disco/VERSION)

PACKAGES = `go list ./... | grep -v '/vendor/'`

MAJOR = $(shell go version | cut -d' ' -f3 | cut -b 3- | cut -d. -f1)
MINOR = $(shell go version | cut -d' ' -f3 | cut -b 3- | cut -d. -f2)
export GO111MODULE=on

.PHONY: supernova disco mdb all clean test build_bls gen_testnet testnet-init testnet-reset testnet-start testnet-stop testnet-check

build_bls:| go_version_check
	@echo "building with BLS12-381 support..."
	@go build -tags bls12381 -o main .
	@echo "done. executable created at 'main'"

supernova:| go_version_check
	@echo "building $@..."
	@go build -v -o $(CURDIR)/bin/$@ -ldflags "-X main.version=$(METER_VERSION) -X main.gitCommit=$(GIT_COMMIT) -X main.gitTag=$(GIT_TAG)"  -tags '$(BUILD_TAGS)' ./cmd/supernova
	@echo "done. executable created at 'bin/$@'"

mdb:| go_version_check
	@echo "building $@..."
	@go build -v -o $(CURDIR)/bin/$@ -ldflags "-X main.version=$(METER_VERSION) -X main.gitCommit=$(GIT_COMMIT) -X main.gitTag=$(GIT_TAG)" ./cmd/mdb
	@echo "done. executable created at 'bin/$@'"


disco:| go_version_check
	@echo "building $@..."
	@go build -v -o $(CURDIR)/bin/$@ -ldflags "-X main.version=$(DISCO_VERSION) -X main.gitCommit=$(GIT_COMMIT) -X main.gitTag=$(GIT_TAG)" ./cmd/disco
	@echo "done. executable created at 'bin/$@'"

dep:| go_version_check
	@go mod download

go_version_check:
	@if test $(MAJOR) -lt 1; then \
		echo "Go 1.13 or higher required"; \
		exit 1; \
	else \
		if test $(MAJOR) -eq 1 -a $(MINOR) -lt 13; then \
			echo "Go 1.13 or higher required"; \
			exit 1; \
		fi \
	fi

all: supernova disco mdb

clean:
	-rm -rf \
$(CURDIR)/bin/supernova \
$(CURDIR)/bin/disco 

test:| go_version_check
	@go test -cover $(PACKAGES)

## Testnet helpers (scripts live in scripts/)

# Build the testnet-config generator tool (requires bls12381 tag)
gen_testnet:| go_version_check
	@echo "Building gen_testnet tool..."
	@go build -tags bls12381 -o bin/gen_testnet ./cmd/gen_testnet
	@echo "Done. Binary at bin/gen_testnet"

# Generate testnet configs without starting nodes
testnet-init:| go_version_check
	@go run -tags bls12381 ./cmd/gen_testnet --out-dir ./mytestnet

# Wipe and regenerate testnet from scratch
testnet-reset:| go_version_check
	@echo "Stopping any running nodes..."
	@pkill -f "mytestnet/node" || true
	@sleep 1
	@echo "Removing old testnet data..."
	@rm -rf ./mytestnet
	@go run -tags bls12381 ./cmd/gen_testnet --out-dir ./mytestnet

# Start all nodes (auto-generates configs if not initialized)
testnet-start:
	@bash scripts/start_testnet.sh

testnet-stop:
	@pkill -f "mytestnet/node" || true

testnet-check:
	@bash scripts/check_testnet.sh