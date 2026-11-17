# Velocor build.
#
# Images are $(IMAGE_PREFIX)/<name>:$(TAG). TAG defaults to dev-<short sha>, so nothing here
# overwrites a shared tag by accident: `latest` and v* tags need RELEASE=true, a clean tree and a
# commit tagged $(VERSION) (CI's release job). The OS image always embeds the spdk/velod images
# of the same build, pinned by digest, so `push` builds and pushes them first.
#
#   make build test lint             Go binaries, unit tests, linters
#   make images                      build all container images locally (except the OS image)
#   make push                        push spdk/velod/csi, then build and push the OS image
#   make test-integration            Go integration tests against a real spdk_tgt (rootless podman)
#   make spdk-unit-tests             SPDK unit tests for the libraries our SPDK patches touch
#   make qcow2 QCOW2_KEYS=a.pub      bootable disk from the pushed OS image (needs KVM, no root)
#
# Registry credentials come from podman's usual auth file, or REGISTRY_AUTH_FILE.

SHELL := /bin/bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := build

MODULE := github.com/velocor/velocor
BINARY_DIR := bin
PODMAN ?= podman
SKOPEO ?= skopeo

GIT_SHA := $(shell git rev-parse --short=7 HEAD 2>/dev/null || echo unknown)
GIT_SHA_FULL := $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
GIT_DIRTY := $(shell git diff --quiet HEAD -- 2>/dev/null || echo dirty)
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(GIT_SHA)
# Commit time rather than "now", so rebuilding a commit produces the same binaries.
BUILD_TIME ?= $(shell git log -1 --format=%cI 2>/dev/null || echo unknown)
LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.BuildTime=$(BUILD_TIME)

IMAGE_PREFIX ?= ghcr.io/xphyr
TAG ?= dev-$(GIT_SHA)$(if $(GIT_DIRTY),-dirty)
# Additional tags pushed for the same images, e.g. EXTRA_TAGS="main" or (releases) "1.2 latest".
EXTRA_TAGS ?=
RELEASE ?= false
SPDK_IMAGE = $(IMAGE_PREFIX)/velocor-spdk:$(TAG)
VELOD_IMAGE = $(IMAGE_PREFIX)/velocor-velod:$(TAG)
CSI_IMAGE = $(IMAGE_PREFIX)/velocor-csi:$(TAG)
BOOTC_IMAGE = $(IMAGE_PREFIX)/velocor-os:$(TAG)
SPDK_TEST_IMAGE ?= localhost/velocor-spdk:test
CSI_PLATFORMS ?= linux/amd64
# true only for CI builds of pull requests, which push to a throwaway local registry.
INSECURE_REGISTRY ?= false
TLS_FLAG := $(if $(filter true,$(INSECURE_REGISTRY)),--tls-verify=false)
SKOPEO_TLS := $(if $(filter true,$(INSECURE_REGISTRY)),--src-tls-verify=false --dest-tls-verify=false)

# Extra labels, e.g. BUILD_LABELS="quay.expires-after=30d" so dev builds expire on Quay.
BUILD_LABELS ?=
LABEL_ARGS := --label org.opencontainers.image.source=https://github.com/xphyr/velocor \
	--label org.opencontainers.image.revision=$(GIT_SHA_FULL) \
	--label org.opencontainers.image.version=$(VERSION) \
	$(addprefix --label ,$(BUILD_LABELS))
# Fedora base images follow Fedora's rolling release tags (daily fixes). Each build resolves the
# tag to the digest it points to right then, builds from exactly that digest, and records it in
# the OCI base-image labels, so every image says which base it was built on.
FEDORA_IMAGE ?= quay.io/fedora/fedora:45
FEDORA_MINIMAL_IMAGE ?= quay.io/fedora/fedora-minimal:45
BOOTC_BASE_IMAGE ?= quay.io/fedora/fedora-bootc:45
# BASE_PIN_DIR (optional; CI sets it): resolve each base once and reuse it for every build that
# follows, so e.g. the integration-tested SPDK image and the pushed one share one base (and one
# SPDK compile) even if Fedora publishes in between. The files there record what was used.
BASE_PIN_DIR ?=
# $(call base-pin,<build arg>,<image ref>): shell prefix setting $$base_args for podman build.
define base-pin
pinfile="$(BASE_PIN_DIR)/$(1)"; \
	if [[ -n "$(BASE_PIN_DIR)" && -s "$$pinfile" ]]; then base="$$(cat "$$pinfile")"; \
	else base="$$(INSECURE_REGISTRY=false hack/pin-image.sh $(2))"; \
	  if [[ -n "$(BASE_PIN_DIR)" ]]; then mkdir -p "$(BASE_PIN_DIR)" && echo "$$base" > "$$pinfile"; fi; fi; \
	echo "base image $(2) -> $$base"; \
	base_args="--build-arg $(1)=$$base --label org.opencontainers.image.base.name=$(2) --label org.opencontainers.image.base.digest=$${base#*@}";
endef
GO_BUILD_ARGS := --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg BUILD_TIME=$(BUILD_TIME)
DIGEST_DIR := $(BINARY_DIR)/digests

.PHONY: build build-velod build-veloctl build-csi test test-integration lint shellcheck check-pins \
	proto proto-lint clean images image-spdk image-spdk-test spdk-unit-tests image-velod image-csi image-os \
	push push-spdk push-velod push-csi push-os check-push qcow2

## Go ---------------------------------------------------------------------------------------

build: build-velod build-veloctl build-csi

build-velod build-veloctl build-csi: build-%:
	@mkdir -p $(BINARY_DIR)
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY_DIR)/$(subst csi,velocor-csi,$*) ./cmd/$(subst csi,velocor-csi,$*)

test:
	go test -race ./...

# Real spdk_tgt in rootless podman (no root, hugepages or NVMe needed).
test-integration: image-spdk-test
	VELOCOR_IT_IMAGE=$(SPDK_TEST_IMAGE) go test -tags integration -count=1 -v -timeout 30m ./internal/integration/

# Without local installs, run the linters from their images, e.g.
#   GOLANGCI_LINT="podman run --rm -v $$PWD:/repo:z -w /repo docker.io/golangci/golangci-lint:v2.15.0 golangci-lint"
GOLANGCI_LINT ?= golangci-lint
SHELLCHECK ?= shellcheck

lint: check-pins shellcheck
	go vet ./...
	go vet -tags integration ./internal/integration/
	$(GOLANGCI_LINT) run --build-tags integration ./...

SHELL_SCRIPTS = $(shell git ls-files '*.sh' deploy/bootc/90-velocor-banner)
shellcheck:
	$(SHELLCHECK) -S warning $(SHELL_SCRIPTS)

check-pins:
	hack/check-pins.sh

proto:
	buf generate

proto-lint:
	buf lint

clean:
	rm -rf $(BINARY_DIR) coverage.txt

## Images -----------------------------------------------------------------------------------

images: image-spdk image-velod image-csi

# The test stage shares the builder stage, so building it first costs one SPDK compile in total.
image-spdk-test:
	$(call base-pin,FEDORA_IMAGE,$(FEDORA_IMAGE)) \
	$(PODMAN) build $(LABEL_ARGS) $$base_args --target test -t $(SPDK_TEST_IMAGE) -f deploy/containers/Containerfile.spdk .

# SPDK's unit tests for the libraries our patches (deploy/containers/spdk-patches) touch; the
# build fails if one fails. A separate SPDK compile with unit tests enabled.
spdk-unit-tests:
	$(call base-pin,FEDORA_IMAGE,$(FEDORA_IMAGE)) \
	$(PODMAN) build $$base_args --target unit-tests -t localhost/velocor-spdk:unit-tests -f deploy/containers/Containerfile.spdk .

image-spdk:
	$(call base-pin,FEDORA_IMAGE,$(FEDORA_IMAGE)) \
	$(PODMAN) build $(LABEL_ARGS) $$base_args -t $(SPDK_IMAGE) -f deploy/containers/Containerfile.spdk .

image-velod:
	$(call base-pin,FEDORA_MINIMAL_IMAGE,$(FEDORA_MINIMAL_IMAGE)) \
	$(PODMAN) build $(LABEL_ARGS) $$base_args $(GO_BUILD_ARGS) -t $(VELOD_IMAGE) -f deploy/containers/Containerfile.velod .

# Always built as a manifest list (also for one platform) so single- and multi-arch builds push
# the same way. The list is recreated each time; reusing it would accumulate old images.
image-csi:
	-$(PODMAN) manifest rm $(CSI_IMAGE) >/dev/null 2>&1
	$(call base-pin,FEDORA_MINIMAL_IMAGE,$(FEDORA_MINIMAL_IMAGE)) \
	$(PODMAN) build $(LABEL_ARGS) $$base_args $(GO_BUILD_ARGS) --platform $(CSI_PLATFORMS) --manifest $(CSI_IMAGE) \
		-f deploy/containers/Containerfile.csi .

# Needs the spdk and velod images in the registry: their digests are resolved there and pinned.
image-os:
	spdk=$$(INSECURE_REGISTRY=$(INSECURE_REGISTRY) hack/pin-image.sh $(SPDK_IMAGE)); \
	velod=$$(INSECURE_REGISTRY=$(INSECURE_REGISTRY) hack/pin-image.sh $(VELOD_IMAGE)); \
	echo "OS image pins $$spdk and $$velod"; \
	$(call base-pin,BOOTC_BASE_IMAGE,$(BOOTC_BASE_IMAGE)) \
	$(PODMAN) build $(LABEL_ARGS) $$base_args $(TLS_FLAG) $(if $(TLS_FLAG),--network=host) \
		--build-arg SPDK_IMAGE=$$spdk --build-arg VELOD_IMAGE=$$velod --build-arg IMAGE_TAG=$(TAG) \
		--build-arg INSECURE_REGISTRY=$(INSECURE_REGISTRY) \
		-t $(BOOTC_IMAGE) -f deploy/bootc/Containerfile .

## Push -------------------------------------------------------------------------------------

push: push-spdk push-velod push-csi
	$(MAKE) push-os

# Refuse anything that could overwrite a shared tag or publish an unidentifiable build.
check-push:
	@[[ -z "$(GIT_DIRTY)" ]] || { echo "refusing to push from a dirty tree (commit first)"; exit 1; }
	@for t in $(TAG) $(EXTRA_TAGS); do \
	  if [[ "$$t" == latest || "$$t" == v* || "$$t" =~ ^[0-9]+\.[0-9]+ ]] && [[ "$(RELEASE)" != true ]]; then \
	    echo "tag '$$t' is reserved for releases (RELEASE=true)"; exit 1; fi; \
	done
	@if [[ "$(RELEASE)" == true ]]; then \
	  [[ "$$(git describe --tags --exact-match 2>/dev/null)" == "$(VERSION)" ]] || \
	    { echo "RELEASE=true needs HEAD tagged $(VERSION)"; exit 1; }; fi

# push-one <local ref> <name>: push, record the digest, then push the extra tags. The extra
# tags are copied from repo@digest: skopeo rejects a reference with both a tag and a digest.
define push-one
	@mkdir -p $(DIGEST_DIR)
	$(PODMAN) push $(TLS_FLAG) --digestfile $(DIGEST_DIR)/$(2) $(1)
	for t in $(EXTRA_TAGS); do $(SKOPEO) copy -q $(SKOPEO_TLS) --all \
	  docker://$(IMAGE_PREFIX)/$(2)@$$(cat $(DIGEST_DIR)/$(2)) docker://$(IMAGE_PREFIX)/$(2):$$t; done
endef

push-spdk: check-push image-spdk
	$(call push-one,$(SPDK_IMAGE),velocor-spdk)

push-velod: check-push image-velod
	$(call push-one,$(VELOD_IMAGE),velocor-velod)

push-csi: check-push image-csi
	@mkdir -p $(DIGEST_DIR)
	$(PODMAN) manifest push $(TLS_FLAG) --all --digestfile $(DIGEST_DIR)/velocor-csi $(CSI_IMAGE) docker://$(CSI_IMAGE)
	for t in $(EXTRA_TAGS); do $(SKOPEO) copy -q $(SKOPEO_TLS) --all \
	  docker://$(IMAGE_PREFIX)/velocor-csi@$$(cat $(DIGEST_DIR)/velocor-csi) docker://$(IMAGE_PREFIX)/velocor-csi:$$t; done

push-os: check-push image-os
	$(call push-one,$(BOOTC_IMAGE),velocor-os)

## Disk image -------------------------------------------------------------------------------

QCOW2_OUTPUT ?= $(BINARY_DIR)/velocor-os-$(TAG).qcow2
QCOW2_SIZE ?= 32G
QCOW2_KEYS ?=

# Rootless (KVM only): a throwaway VM runs `bootc install to-disk` from the pushed OS image.
qcow2:
	hack/build-qcow2.sh --image $$(hack/pin-image.sh $(BOOTC_IMAGE)) --output $(QCOW2_OUTPUT) \
		--size $(QCOW2_SIZE) $(addprefix --authorized-keys ,$(QCOW2_KEYS))
