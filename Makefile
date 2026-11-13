BINARY_DIR := bin
VELOD_BIN := $(BINARY_DIR)/velod
VELOCTL_BIN := $(BINARY_DIR)/veloctl
VELOCOR_CSI_BIN := $(BINARY_DIR)/velocor-csi

MODULE := github.com/velocor/velocor
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "v0.1.0-dev")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
BUILD_TIME ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")

LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.BuildTime=$(BUILD_TIME)

IMAGE_PREFIX ?= registry.xphyrlab.net/velocor
SPDK_IMAGE ?= $(IMAGE_PREFIX)/velocor-spdk:latest
VELOD_IMAGE ?= $(IMAGE_PREFIX)/velocor-velod:latest
CSI_IMAGE ?= $(IMAGE_PREFIX)/velocor-csi:latest
BOOTC_IMAGE ?= $(IMAGE_PREFIX)/velocor-os:latest
PODMAN ?= podman
SKOPEO ?= skopeo

.PHONY: all build build-velod build-veloctl build-csi proto proto-lint test lint clean \
	containers container-spdk container-velod container-csi container-bootc \
	push-containers push-spdk push-velod push-csi push-bootc

all: build

build: build-velod build-veloctl build-csi

build-velod:
	@mkdir -p $(BINARY_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(VELOD_BIN) ./cmd/velod

build-veloctl:
	@mkdir -p $(BINARY_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(VELOCTL_BIN) ./cmd/veloctl

build-csi:
	@mkdir -p $(BINARY_DIR)
	go build -ldflags "$(LDFLAGS)" -o $(VELOCOR_CSI_BIN) ./cmd/velocor-csi

containers: container-spdk container-velod container-csi container-bootc

container-spdk:
	$(PODMAN) build -t $(SPDK_IMAGE) -f deploy/containers/Containerfile.spdk .

container-velod:
	$(PODMAN) build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t $(VELOD_IMAGE) -f deploy/containers/Containerfile.velod .

container-csi:
	$(PODMAN) build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t $(CSI_IMAGE) -f deploy/containers/Containerfile.csi .

container-bootc:
	$(PODMAN) build -t $(BOOTC_IMAGE) -f deploy/bootc/Containerfile .

push-containers: push-spdk push-velod push-csi push-bootc

push-spdk: container-spdk
	$(PODMAN) push $(SPDK_IMAGE)

push-velod: container-velod
	$(PODMAN) push $(VELOD_IMAGE)

push-csi: container-csi
	$(PODMAN) push $(CSI_IMAGE)

push-bootc: container-bootc
	$(PODMAN) push $(BOOTC_IMAGE)

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
