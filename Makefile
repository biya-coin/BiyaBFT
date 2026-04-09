PACKAGE = github.com/meterio/supernova

GIT_COMMIT = $(shell git --no-pager log --pretty="%h" -n 1)
GIT_TAG = $(shell git tag -l --points-at HEAD)
METER_VERSION = $(shell cat cmd/supernova/VERSION)
DISCO_VERSION = $(shell cat cmd/disco/VERSION)

PACKAGES = `go list ./... | grep -v '/vendor/'`

MAJOR = $(shell go version | cut -d' ' -f3 | cut -b 3- | cut -d. -f1)
MINOR = $(shell go version | cut -d' ' -f3 | cut -b 3- | cut -d. -f2)
export GO111MODULE=on

.PHONY: supernova supernova_portable disco mdb all clean test supernova_bls12381

# 使用 BLS 12-381 以与 node 共识所需密钥一致（init 生成 BLS 私钥）
BUILD_TAGS ?= bls12381

# 若出现 "Caught SIGILL in blst_cgo_init"：当前 CPU 不支持 blst 的优化指令，请用 make supernova_portable 或 BLST_PORTABLE=1 make supernova
BLST_PORTABLE ?= 0
ifeq ($(BLST_PORTABLE),1)
export CGO_CFLAGS = -O -D__BLST_PORTABLE__
export CGO_CFLAGS_ALLOW = -O -D__BLST_PORTABLE__
endif

supernova:| go_version_check
	@echo "building $@..."
	@go build -v -o $(CURDIR)/bin/$@ -ldflags "-X main.version=$(METER_VERSION) -X main.gitCommit=$(GIT_COMMIT) -X main.gitTag=$(GIT_TAG)"  -tags '$(BUILD_TAGS)' ./cmd/supernova
	@echo "done. executable created at 'bin/$@'"

# 使用 blst 便携模式编译，避免在不支持 ADX/BMI2 等指令的 CPU 上出现 SIGILL
supernova_portable:
	$(MAKE) supernova BLST_PORTABLE=1


supernova_bls12381:| go_version_check
	@echo "building $@..."
	@go build -v -o $(CURDIR)/bin/$@ -ldflags "-X main.version=$(METER_VERSION) -X main.gitCommit=$(GIT_COMMIT) -X main.gitTag=$(GIT_TAG)" -tags '$(BUILD_TAGS)' ./cmd/supernova_bls12381
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