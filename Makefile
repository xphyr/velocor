# Velocor build.
#
# Images are $(IMAGE_PREFIX)/<name>:$(TAG). TAG defaults to dev-<short sha>, so nothing here
# overwrites a shared tag by accident: `latest` and v* tags need RELEASE=true, a clean tree and a
# commit tagged $(VERSION) (CI's release job). The OS image always embeds the velod image of the
# same build, pinned by digest, so `push` builds and pushes it first.
#
#   make build test lint             Go binaries, unit tests, linters
#   make images                      build all container images locally (except the OS image)
#   make push                        push velod/csi, then build and push the OS image
#   make test-kernel                 data plane tests on real md/LVM/nvmet/LIO (runs sudo)
#   make test-kernel-vm              the same in a throwaway Fedora VM under KVM (no root; CI)
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
VELOD_IMAGE = $(IMAGE_PREFIX)/velocor-velod:$(TAG)
CSI_IMAGE = $(IMAGE_PREFIX)/velocor-csi:$(TAG)
BOOTC_IMAGE = $(IMAGE_PREFIX)/velocor-os:$(TAG)
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
# The release tags come from the Containerfiles' ARG defaults (which Renovate updates), so a
# Fedora release bump there is what builds use; hack/check-pins.sh checks they agree.
base-arg = $(shell sed -n 's/^ARG $(1)=//p' $(2) | head -n1)
FEDORA_MINIMAL_IMAGE ?= $(call base-arg,FEDORA_MINIMAL_IMAGE,deploy/containers/Containerfile.velod)
BOOTC_BASE_IMAGE ?= $(call base-arg,BOOTC_BASE_IMAGE,deploy/bootc/Containerfile)
# BASE_PIN_DIR (optional; CI sets it): resolve each base once and reuse it for every build that
# follows, even if Fedora publishes in between. The files there record what was used.
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

.PHONY: build build-velod build-veloctl build-csi test test-kernel test-kernel-vm lint shellcheck check-pins check-alerts \
	proto proto-lint clean images image-velod image-csi image-os \
	push push-velod push-csi push-os check-push qcow2

## Go ---------------------------------------------------------------------------------------

build: build-velod build-veloctl build-csi

build-velod build-veloctl build-csi: build-%:
	@mkdir -p $(BINARY_DIR)
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY_DIR)/$(subst csi,velocor-csi,$*) ./cmd/$(subst csi,velocor-csi,$*)

test:
	go test -race ./...

# The data plane's integration tests against real md, LVM, device-mapper,
# nvmet and LIO, on loop devices: they need root, the kernel modules and the storage tools, so
# they run on the lab VM or CI's runner, never inside a container. KERNEL_TEST_RUN filters them.
KERNEL_TEST_BIN ?= bin/kernel.test
KERNEL_TEST_RUN ?= .
test-kernel:
	@mkdir -p $(dir $(KERNEL_TEST_BIN))
	CGO_ENABLED=0 go test -c -tags kernelintegration -o $(KERNEL_TEST_BIN) ./internal/kernel/
	CGO_ENABLED=0 go test -c -tags kernelintegration -o $(KERNEL_TEST_BIN)plane ./internal/dataplane/kernelplane/
	cd internal/kernel && sudo $(abspath $(KERNEL_TEST_BIN)) -test.v -test.count=1 -test.timeout 30m -test.run '$(KERNEL_TEST_RUN)'
	sudo $(abspath $(KERNEL_TEST_BIN))plane -test.v -test.count=1 -test.timeout 30m -test.run '$(KERNEL_TEST_RUN)'

# The same tests in a throwaway Fedora Cloud VM under KVM: no root needed (CI runs this).
test-kernel-vm:
	hack/kernel-tests-vm.sh '$(KERNEL_TEST_RUN)'

# Without local installs, run the linters from their images, e.g.
#   GOLANGCI_LINT="podman run --rm -v $$PWD:/repo:z -w /repo docker.io/golangci/golangci-lint:v2.15.0 golangci-lint"
GOLANGCI_LINT ?= golangci-lint
SHELLCHECK ?= shellcheck

lint: check-pins shellcheck check-alerts
	go vet ./...
	go vet -tags kernelintegration ./internal/kernel ./internal/dataplane/kernelplane
	$(GOLANGCI_LINT) run --build-tags kernelintegration ./...

SHELL_SCRIPTS = $(shell git ls-files '*.sh' deploy/bootc/90-velocor-banner)
shellcheck:
	$(SHELLCHECK) -S warning $(SHELL_SCRIPTS)

check-pins:
	hack/check-pins.sh

# Alert rules: promtool syntax check and unit tests (deploy/monitoring).
check-alerts:
	hack/check-alerts.sh

proto:
	buf generate

proto-lint:
	buf lint

clean:
	rm -rf $(BINARY_DIR) coverage.txt

## Images -----------------------------------------------------------------------------------

images: image-velod image-csi

image-velod:
	$(call base-pin,FEDORA_MINIMAL_IMAGE,$(FEDORA_MINIMAL_IMAGE)) \
	$(PODMAN) build $(LABEL_ARGS) $$base_args $(GO_BUILD_ARGS) -t $(VELOD_IMAGE) -f deploy/containers/Containerfile.velod .

# Always built as a manifest list (also for one platform) so single- and multi-arch builds push
# the same way. The list is recreated each time; reusing it would accumulate old images.
# One build per platform, each on that platform's own base image (hack/platform-image.sh): CI's
# podman 4.9 resolves a FROM that names a list by digest for the build host whatever --platform
# says, even with --pull=always, so v0.2.0's arm64 image had x86-64 tools. check-image-arch.sh
# then refuses a list whose tools don't match their platform.
CSI_TOOLS := /usr/bin/ls /usr/bin/mount /usr/sbin/nvme /usr/sbin/iscsiadm /usr/sbin/mkfs.xfs /usr/sbin/mkfs.ext4 /usr/bin/velocor-csi
image-csi:
	-$(PODMAN) manifest rm $(CSI_IMAGE) >/dev/null 2>&1
	$(call base-pin,FEDORA_MINIMAL_IMAGE,$(FEDORA_MINIMAL_IMAGE)) \
	for platform in $$(tr , ' ' <<<"$(CSI_PLATFORMS)"); do \
	  pbase=$$(INSECURE_REGISTRY=false hack/platform-image.sh "$$base" "$$platform") || exit 1; \
	  echo "$$platform base image $$pbase"; \
	  $(PODMAN) build $(LABEL_ARGS) $$base_args --build-arg FEDORA_MINIMAL_IMAGE=$$pbase \
	    --label org.opencontainers.image.base.digest=$${pbase#*@} $(GO_BUILD_ARGS) \
	    --platform $$platform --manifest $(CSI_IMAGE) -f deploy/containers/Containerfile.csi . || exit 1; \
	done
	PODMAN=$(PODMAN) hack/check-image-arch.sh $(CSI_IMAGE) $(CSI_TOOLS)

# Needs the velod image in the registry: its digest is resolved there and pinned.
# The build pulls it, so registry credentials (REGISTRY_AUTH_FILE, else podman's default auth
# file) go in as a build secret, which is never stored in the image; without any, an empty file.
REGISTRY_AUTH_FILE ?= $(XDG_RUNTIME_DIR)/containers/auth.json
image-os:
	auth="$(REGISTRY_AUTH_FILE)"; \
	if [[ ! -s "$$auth" ]]; then mkdir -p $(BINARY_DIR) && echo '{}' > $(BINARY_DIR)/no-registry-auth.json && auth=$(BINARY_DIR)/no-registry-auth.json; fi; \
	velod=$$(INSECURE_REGISTRY=$(INSECURE_REGISTRY) hack/pin-image.sh $(VELOD_IMAGE)); \
	echo "OS image pins $$velod"; \
	$(call base-pin,BOOTC_BASE_IMAGE,$(BOOTC_BASE_IMAGE)) \
	$(PODMAN) build $(LABEL_ARGS) $$base_args $(TLS_FLAG) $(if $(TLS_FLAG),--network=host) --secret id=registry-auth,src="$$auth" \
		--build-arg VELOD_IMAGE=$$velod --build-arg IMAGE_TAG=$(TAG) \
		--build-arg INSECURE_REGISTRY=$(INSECURE_REGISTRY) \
		-t $(BOOTC_IMAGE) -f deploy/bootc/Containerfile .

## Push -------------------------------------------------------------------------------------

push: push-velod push-csi
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
