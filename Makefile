BINARY_DIR := bin
VELOD_BIN := $(BINARY_DIR)/velod
VELOCTL_BIN := $(BINARY_DIR)/veloctl

MODULE := github.com/velocor/velocor
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "v0.1.0-dev")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
BUILD_TIME ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.BuildTime=$(BUILD_TIME)

.PHONY: all build build-velod build-veloctl proto proto-lint test lint clean

all: build

build: build-velod build-veloctl

build-velod:
	@mkdir -p $(BINARY_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(VELOD_BIN) ./cmd/velod

build-veloctl:
	@mkdir -p $(BINARY_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(VELOCTL_BIN) ./cmd/veloctl

proto:
	buf generate

proto-lint:
	buf lint

test:
	go test -race -v ./...

lint:
	golangci-lint run ./...

clean:
	rm -rf $(BINARY_DIR) coverage.txt
