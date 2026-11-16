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
BOOTC_BUILDER_IMAGE ?= quay.io/centos-bootc/bootc-image-builder:latest
QCOW2_OUTPUT_DIR ?= output
QCOW2_ROOTFS ?= xfs
BIB_CONFIG ?= $(if $(wildcard deploy/bootc/config.local.toml),deploy/bootc/config.local.toml,deploy/bootc/config.toml)
PODMAN ?= podman
# Extra image labels, e.g. BUILD_LABELS="quay.expires-after=14d" so dev builds expire on Quay.
BUILD_LABELS ?=
LABEL_ARGS := $(addprefix --label ,$(BUILD_LABELS))
SKOPEO ?= skopeo

.PHONY: all build build-velod build-veloctl build-csi proto proto-lint test lint clean spdk-test-image test-integration \
	containers container-spdk container-velod container-csi container-bootc \
	push-containers push-spdk push-velod push-csi push-bootc qcow2

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
	$(PODMAN) build $(LABEL_ARGS) -t $(SPDK_IMAGE) -f deploy/containers/Containerfile.spdk .

container-velod:
	$(PODMAN) build $(LABEL_ARGS) \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t $(VELOD_IMAGE) -f deploy/containers/Containerfile.velod .

container-csi:
	$(PODMAN) build $(LABEL_ARGS) \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BUILD_TIME=$(BUILD_TIME) \
		-t $(CSI_IMAGE) -f deploy/containers/Containerfile.csi .

container-bootc:
	$(PODMAN) build $(LABEL_ARGS) \
		--build-arg VELOD_IMAGE=$(VELOD_IMAGE) \
		--build-arg SPDK_IMAGE=$(SPDK_IMAGE) \
		-t $(BOOTC_IMAGE) -f deploy/bootc/Containerfile .

push-containers: push-spdk push-velod push-csi push-bootc

push-spdk: container-spdk
	$(PODMAN) push $(SPDK_IMAGE)

push-velod: container-velod
	$(PODMAN) push $(VELOD_IMAGE)

push-csi: container-csi
	$(PODMAN) push $(CSI_IMAGE)

push-bootc: container-bootc
	$(PODMAN) push $(BOOTC_IMAGE)

qcow2: container-bootc
	@mkdir -p $(QCOW2_OUTPUT_DIR)
	@if [ "$$(id -u)" -ne 0 ]; then \
		echo "Syncing $(BOOTC_IMAGE) into root container storage for bootc-image-builder..."; \
		$(PODMAN) save $(BOOTC_IMAGE) | sudo $(PODMAN) load; \
	fi
	$(if $(shell [ "$$(id -u)" -eq 0 ] || echo sudo),sudo ,)$(PODMAN) run --rm --privileged \
		--pull=newer \
		--security-opt label=type:unconfined_t \
		-v $(CURDIR)/$(QCOW2_OUTPUT_DIR):/output \
		-v /var/lib/containers/storage:/var/lib/containers/storage \
		$(if $(wildcard $(BIB_CONFIG)),-v $(CURDIR)/$(BIB_CONFIG):/config.toml:ro,) \
		$(BOOTC_BUILDER_IMAGE) \
		--type qcow2 \
		--rootfs $(QCOW2_ROOTFS) \
		--local \
		$(if $(wildcard $(BIB_CONFIG)),--config /config.toml,) \
		$(BOOTC_IMAGE)
	@echo "qcow2 image written to $(QCOW2_OUTPUT_DIR)/qcow2/disk.qcow2"

proto:
	buf generate

proto-lint:
	buf lint

test:
	go test -race -v ./...

# Integration tests against a real spdk_tgt (rootless podman; no root, hugepages or NVMe needed).
SPDK_TEST_IMAGE ?= localhost/velocor-spdk:test

spdk-test-image:
	podman build --target test -t $(SPDK_TEST_IMAGE) -f deploy/containers/Containerfile.spdk .

test-integration: spdk-test-image
	VELOCOR_IT_IMAGE=$(SPDK_TEST_IMAGE) go test -tags integration -count=1 -v -timeout 30m ./internal/integration/

lint:
	golangci-lint run ./...

clean:
	rm -rf $(BINARY_DIR) coverage.txt
