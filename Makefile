.DEFAULT_GOAL := help

-include ../../versions.mk

TIMICH_AGENT_VERSION ?= 0.4.0
TIMICH_AGENT_DIST_REPO ?= rsahara/timich-agent
TIMICH_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
TIMICH_BUILT_AT ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
AGENT_UPDATE_CHANNEL ?= stable
TIMICH_AGENT_STABLE_UPDATE_MANIFEST_URL ?= https://github.com/$(TIMICH_AGENT_DIST_REPO)/releases/latest/download/agent-update-manifest.json
TIMICH_AGENT_PRERELEASE_UPDATE_MANIFEST_URL ?= https://api.github.com/repos/$(TIMICH_AGENT_DIST_REPO)/releases?per_page=100&timich_channel=prerelease
TIMICH_AGENT_UPDATE_MANIFEST_URL ?= $(if $(filter prerelease,$(AGENT_UPDATE_CHANNEL)),$(TIMICH_AGENT_PRERELEASE_UPDATE_MANIFEST_URL),$(TIMICH_AGENT_STABLE_UPDATE_MANIFEST_URL))
HOST_GOOS := $(shell if command -v go >/dev/null 2>&1; then go env GOOS; else uname -s 2>/dev/null | tr '[:upper:]' '[:lower:]'; fi)
HOST_GOARCH := $(shell if command -v go >/dev/null 2>&1; then go env GOARCH; else uname -m 2>/dev/null | sed -e 's/^x86_64$$/amd64/' -e 's/^aarch64$$/arm64/'; fi)
GOOS ?= $(HOST_GOOS)
GOARCH ?= $(HOST_GOARCH)
TIMICH_SEMANTIC_HELPER_LDFLAGS ?= -X main.version=$(TIMICH_AGENT_VERSION) -X main.commit=$(TIMICH_COMMIT) -X main.builtAt=$(TIMICH_BUILT_AT)
GOCACHE ?= $(abspath build/go-build-cache)
export GOCACHE

BUILD_DIR ?= build
BINARY ?= $(BUILD_DIR)/timich-agent
HELPER_BINARY ?= $(BUILD_DIR)/timich-semantic-helper
MEDIA_HELPER_BINARY ?= $(BUILD_DIR)/timich-media-helper
GO_BUILD_FLAGS ?=
CARGO ?= cargo
MEDIA_HELPER_DOCKER ?= 0
MEDIA_HELPER_DOCKER_IMAGE ?= rust:1-alpine
MEDIA_HELPER_DOCKER_PLATFORM ?= linux/$(DIST_ARCH)
MEDIA_HELPER_MANIFEST ?= media-helper/Cargo.toml
MEDIA_HELPER_TARGET_DIR ?= $(abspath $(BUILD_DIR)/media-helper-target)
MEDIA_HELPER_RUSTFLAGS ?=
MEDIA_HELPER_STATIC_RUSTFLAGS ?= -C target-feature=+crt-static
DIST_MEDIA_HELPER_RUSTFLAGS ?= $(if $(filter linux,$(DIST_OS)),$(MEDIA_HELPER_STATIC_RUSTFLAGS),$(MEDIA_HELPER_RUSTFLAGS))
CONFIG_PATH ?= .local/agent.json
DATA_DIR ?= .local/state
DOCKER_IMAGE ?= timich-agent:local
DIST_DIR ?= dist
DIST_OS ?= $(GOOS)
DIST_ARCH ?= $(GOARCH)
DIST_NAME ?= timich-agent_$(TIMICH_AGENT_VERSION)_$(DIST_OS)_$(DIST_ARCH)
DIST_STAGE := build/dist/$(DIST_NAME)
DIST_STAGE_ABS := $(abspath $(DIST_STAGE))
DIST_ARCHIVE := $(DIST_DIR)/$(DIST_NAME).tar.gz
SEMANTIC_RUNTIME_PACK_ID ?= timich-siglip2-onnxruntime-runtime
SEMANTIC_RUNTIME_PACK_NAME ?= Timich SigLIP 2 ONNX Runtime
SEMANTIC_RUNTIME_PACK_VERSION ?= $(TIMICH_AGENT_VERSION)
SEMANTIC_RUNTIME_PACK_PLATFORM ?= $(DIST_OS)-$(DIST_ARCH)
SEMANTIC_RUNTIME_PACK_DIR ?= $(DIST_DIR)/semantic-runtime-packs
SEMANTIC_RUNTIME_PACK_BASE_URL ?=
SEMANTIC_RUNTIME_PACK_SIGNING_KEY ?=
SEMANTIC_RUNTIME_PACK_REQUIREMENTS ?= semantic-runtime/siglip2-onnx/requirements.txt
SEMANTIC_RUNTIME_PACK_PYTHON ?= python3
SEMANTIC_RUNTIME_PACK_PYTHON_RUNTIME_ROOT ?=
SEMANTIC_RUNTIME_PACK_ALLOW_SOURCE_BUILDS ?= 0
SEMANTIC_RUNTIME_PACK_KEEP_WORK ?= 0
SEMANTIC_RUNTIME_PACK_ARTIFACT ?=
SEMANTIC_RUNTIME_PACK_SHA256_FILE ?=
SEMANTIC_RUNTIME_PACK_METADATA ?=
SEMANTIC_RUNTIME_PACK_SBOM ?=
SEMANTIC_RUNTIME_PACK_REGISTRY ?=
SEMANTIC_RUNTIME_PACK_SIGNATURE ?=
SEMANTIC_RUNTIME_PACK_PUBLIC_KEY ?=
SEMANTIC_RUNTIME_PACK_EXPECTED_PLATFORM ?=
SEMANTIC_RUNTIME_PACK_REQUIRE_SIGNATURE ?= 0
SEMANTIC_RUNTIME_PACK_REQUIRE_BUNDLED_PYTHON ?= 0
SEMANTIC_RUNTIME_PACK_SMOKE_IMPORT ?= 0
SEMANTIC_RUNTIME_PACK_SMOKE_TIMEOUT ?= 30
SEMANTIC_MODEL_PACK_ID ?= timich-siglip2-base-patch16-224-onnx-multilingual-v1
SEMANTIC_MODEL_PACK_NAME ?= Timich SigLIP 2 Base Patch16 224 ONNX Multilingual
SEMANTIC_MODEL_PACK_VERSION ?= 2026.06
SEMANTIC_MODEL_PACK_BASE_URL ?= $(AGENT_DIST_BASE_URL)
SEMANTIC_MODEL_PACK_IMAGE_MODEL ?=
SEMANTIC_MODEL_PACK_TEXT_MODEL ?=
SEMANTIC_MODEL_PACK_PROCESSOR_DIR ?=
SEMANTIC_MODEL_PACK_UPSTREAM_MODEL ?= google/siglip2-base-patch16-224
SEMANTIC_MODEL_PACK_EMBEDDING_DIM ?= 768
SEMANTIC_MODEL_PACK_QUANTIZATION ?= fp32
SEMANTIC_MODEL_PACK_LICENSE ?=
SEMANTIC_MODEL_PACK_SIGNING_KEY ?=
SEMANTIC_MODEL_PACK_ARTIFACT ?=
SEMANTIC_MODEL_PACK_SHA256_FILE ?=
SEMANTIC_MODEL_PACK_METADATA ?=
SEMANTIC_MODEL_PACK_SBOM ?=
SEMANTIC_MODEL_PACK_REGISTRY ?=
SEMANTIC_MODEL_PACK_SIGNATURE ?=
SEMANTIC_MODEL_PACK_PUBLIC_KEY ?=
SEMANTIC_MODEL_PACK_REQUIRE_SIGNATURE ?= 0
SEMANTIC_MODEL_PACK_DIR ?= $(DIST_DIR)/semantic-model-packs
SEMANTIC_MODEL_REGISTRY ?= $(DIST_DIR)/semantic-models.json
SEMANTIC_MODEL_REGISTRY_INPUTS ?=
SEMANTIC_MODEL_REGISTRY_VERSION ?= $(TIMICH_AGENT_VERSION)
SEMANTIC_MODEL_REGISTRY_BASE_URL ?=
SEMANTIC_MODEL_REGISTRY_RECOMMENDED ?=
SEMANTIC_MODEL_REGISTRY_RECOMMENDED_RUNTIME_PACK ?=
SEMANTIC_RELEASE_REQUIRE_SIGNATURES ?= 0
MEDIA_RUNTIME_VIPS_SOURCE ?= media-runtime/libvips/$(DIST_OS)_$(DIST_ARCH)
MEDIA_RUNTIME_VIPS_REQUIRED ?= 0
MEDIA_LIBVIPS_RUNTIME_ID ?= timich-libvips-alpine-runtime
MEDIA_LIBVIPS_RUNTIME_NAME ?= Timich libvips Alpine Runtime
MEDIA_LIBVIPS_RUNTIME_VERSION ?= 8.16-alpine3.22
MEDIA_LIBVIPS_RUNTIME_PLATFORM ?= linux_amd64
MEDIA_LIBVIPS_RUNTIME_DOCKER_PLATFORM ?= linux/amd64
MEDIA_LIBVIPS_RUNTIME_BUILD_IMAGE ?= alpine:3.22
MEDIA_LIBVIPS_RUNTIME_DIR ?= media-runtime/libvips/$(MEDIA_LIBVIPS_RUNTIME_PLATFORM)
MEDIA_LIBVIPS_RUNTIME_PACK_DIR ?= $(DIST_DIR)/media-runtime-packs
MEDIA_LIBVIPS_RUNTIME_APK_PACKAGES ?=
MEDIA_RUNTIME_FFMPEG_SOURCE ?= media-runtime/ffmpeg/$(DIST_OS)_$(DIST_ARCH)
MEDIA_RUNTIME_FFMPEG_REQUIRED ?= 0
MEDIA_FFMPEG_RUNTIME_ID ?= timich-ffmpeg-lgpl-decode-runtime
MEDIA_FFMPEG_RUNTIME_NAME ?= Timich FFmpeg LGPL Decode Runtime
MEDIA_FFMPEG_RUNTIME_VERSION ?= 7.1.5
MEDIA_FFMPEG_RUNTIME_PLATFORM ?= linux_amd64
MEDIA_FFMPEG_RUNTIME_DOCKER_PLATFORM ?= linux/amd64
MEDIA_FFMPEG_RUNTIME_SOURCE_URL ?= https://ffmpeg.org/releases/ffmpeg-$(MEDIA_FFMPEG_RUNTIME_VERSION).tar.xz
MEDIA_FFMPEG_RUNTIME_GPG_KEY_URL ?= https://ffmpeg.org/ffmpeg-devel.asc
MEDIA_FFMPEG_RUNTIME_GPG_FINGERPRINT ?= FCF986EA15E6E293A5644F10B4322F04D67658D8
MEDIA_FFMPEG_RUNTIME_DOCKERFILE ?= media-runtime/ffmpeg/builder/linux-amd64.Dockerfile
MEDIA_FFMPEG_RUNTIME_IMAGE ?= timich-ffmpeg-runtime:$(MEDIA_FFMPEG_RUNTIME_VERSION)-$(MEDIA_FFMPEG_RUNTIME_PLATFORM)
MEDIA_FFMPEG_RUNTIME_DIR ?= media-runtime/ffmpeg/$(MEDIA_FFMPEG_RUNTIME_PLATFORM)
MEDIA_FFMPEG_RUNTIME_PACK_DIR ?= $(DIST_DIR)/media-runtime-packs
MEDIA_FFMPEG_RUNTIME_BASE_URL ?= $(AGENT_DIST_BASE_URL)
AGENT_UPDATE_MANIFEST := $(DIST_DIR)/agent-update-manifest.json
AGENT_DIST_REPO ?= $(TIMICH_AGENT_DIST_REPO)
AGENT_DIST_TAG ?= v$(TIMICH_AGENT_VERSION)
AGENT_DIST_TITLE ?= Timich Agent $(TIMICH_AGENT_VERSION)
AGENT_DIST_NOTES ?= Timich Agent $(TIMICH_AGENT_VERSION) release artifacts.
AGENT_DIST_BASE_URL ?= https://github.com/$(AGENT_DIST_REPO)/releases/download/$(AGENT_DIST_TAG)
AGENT_DIST_NOTES_URL ?= https://github.com/$(AGENT_DIST_REPO)/releases/tag/$(AGENT_DIST_TAG)
TIMICH_AGENT_SEMANTIC_MODEL_MANIFEST_URL ?= $(AGENT_DIST_BASE_URL)/semantic-models.json
TIMICH_AGENT_RELEASE_TAG ?= $(AGENT_DIST_TAG)
TIMICH_AGENT_LDFLAGS ?= -X main.version=$(TIMICH_AGENT_VERSION) -X main.commit=$(TIMICH_COMMIT) -X main.builtAt=$(TIMICH_BUILT_AT) -X main.releaseTag=$(TIMICH_AGENT_RELEASE_TAG) -X main.updateManifestURL=$(TIMICH_AGENT_UPDATE_MANIFEST_URL) -X main.semanticModelManifestURL=$(TIMICH_AGENT_SEMANTIC_MODEL_MANIFEST_URL)
PRERELEASE_VERSION ?= $(TIMICH_AGENT_VERSION)
PRERELEASE_TAG ?= v$(PRERELEASE_VERSION)-rc.1
PRERELEASE_DIST_DIR ?= $(DIST_DIR)/prerelease-$(PRERELEASE_TAG)
PRERELEASE_PUBLISH ?= 0
PUBLIC_SOURCE_SHA ?=

.PHONY: help build build-helper build-media-helper test-media-helper media-helper-smoke test init run docker-build docker-run compose-up compose-down semantic-runtime-pack semantic-runtime-pack-validate semantic-model-pack semantic-model-pack-validate semantic-model-registry semantic-release-validate media-libvips-runtime-pack media-libvips-runtime-verify media-ffmpeg-runtime-pack media-ffmpeg-runtime-verify dist update-manifest publish-dist prerelease-stage prerelease-upload prerelease-publish

help:
	@echo "timich-agent"
	@echo ""
	@echo "Available targets:"
	@echo "  make build   Build the timich-agent binary"
	@echo "  make build-helper"
	@echo "               Build the timich-semantic-helper binary"
	@echo "  make build-media-helper"
	@echo "               Build the Rust timich-media-helper binary"
	@echo "  make test-media-helper"
	@echo "               Run timich-media-helper Rust tests"
	@echo "  make media-helper-smoke"
	@echo "               Smoke-test a built timich-media-helper against local media backends"
	@echo "  make test    Run timich-agent Go tests"
	@echo "  make init    Write a starter local config file"
	@echo "  make run     Run the local admin and media APIs"
	@echo "  make docker-build  Build the timich-agent Docker image"
	@echo "  make docker-run    Run timich-agent in Docker for foreground testing"
	@echo "  make compose-up    Start timich-agent with docker compose"
	@echo "  make compose-down  Stop the docker compose timich-agent"
	@echo "  make semantic-runtime-pack"
	@echo "                     Build a platform semantic runtime pack zip/checksum/SBOM"
	@echo "  make semantic-runtime-pack-validate"
	@echo "                     Validate a semantic runtime pack release artifact"
	@echo "  make semantic-model-pack"
	@echo "                     Build a SigLIP 2 ONNX semantic model pack zip/sidecars"
	@echo "  make semantic-model-pack-validate"
	@echo "                     Validate a semantic model pack release artifact"
	@echo "  make semantic-model-registry"
	@echo "                     Merge model/runtime registry fragments into semantic-models.json"
	@echo "  make semantic-release-validate"
	@echo "                     Validate semantic-models.json against local release artifacts"
	@echo "  make media-ffmpeg-runtime-pack"
	@echo "                     Build the native FFmpeg media runtime input and release pack"
	@echo "  make media-ffmpeg-runtime-verify"
	@echo "                     Verify the native FFmpeg media runtime input"
	@echo "  make media-libvips-runtime-pack"
	@echo "                     Build the native libvips media runtime input and release pack"
	@echo "  make media-libvips-runtime-verify"
	@echo "                     Verify the native libvips media runtime input"
	@echo "  make dist          Build a timich-agent release tarball"
	@echo "  make update-manifest"
	@echo "                     Build manifest from all agent tarballs in dist/"
	@echo "  make publish-dist  Deprecated; use the protected release workflow"
	@echo "  make prerelease-stage"
	@echo "                     Build and validate semantic-only staging artifacts"
	@echo "  make prerelease-upload"
	@echo "                     Upload semantic artifacts to the separate staging draft"
	@echo "  make prerelease-publish"
	@echo "                     Deprecated; only the protected workflow may publish"

build:
	@mkdir -p $(BUILD_DIR)
	go build $(GO_BUILD_FLAGS) -ldflags "$(TIMICH_AGENT_LDFLAGS)" -o $(BINARY) ./cmd/timich-agent

build-helper:
	@mkdir -p $(BUILD_DIR)
	go build $(GO_BUILD_FLAGS) -ldflags "$(TIMICH_SEMANTIC_HELPER_LDFLAGS)" -o $(HELPER_BINARY) ./cmd/timich-semantic-helper

build-media-helper:
	@mkdir -p "$(BUILD_DIR)"
	@if [ "$(MEDIA_HELPER_DOCKER)" = "1" ]; then \
		if [ "$(DIST_OS)" != "linux" ]; then \
			echo "Docker-backed media helper builds support Linux targets only, got $(DIST_OS)/$(DIST_ARCH)" >&2; \
			exit 2; \
		fi; \
		command -v docker >/dev/null 2>&1 || { echo "docker is required when MEDIA_HELPER_DOCKER=1" >&2; exit 127; }; \
		mkdir -p "$(MEDIA_HELPER_TARGET_DIR)"; \
		target_dir="$$(CDPATH= cd -- "$(MEDIA_HELPER_TARGET_DIR)" && pwd)"; \
		docker run --rm \
			--platform "$(MEDIA_HELPER_DOCKER_PLATFORM)" \
			-u "$$(id -u):$$(id -g)" \
			-v "$$(pwd):/src" \
			-v "$$target_dir:/target" \
			-w /src \
			-e CARGO_HOME=/tmp/cargo-home \
			-e CARGO_TARGET_DIR=/target \
			-e RUSTFLAGS="$(MEDIA_HELPER_RUSTFLAGS)" \
			"$(MEDIA_HELPER_DOCKER_IMAGE)" \
			cargo build --manifest-path "$(MEDIA_HELPER_MANIFEST)" --release; \
	elif command -v "$(CARGO)" >/dev/null 2>&1; then \
		if [ "$(DIST_OS)/$(DIST_ARCH)" != "$(HOST_GOOS)/$(HOST_GOARCH)" ]; then \
			echo "native Cargo cannot build $(DIST_OS)/$(DIST_ARCH) from $(HOST_GOOS)/$(HOST_GOARCH); use MEDIA_HELPER_DOCKER=1 for Linux targets" >&2; \
			exit 2; \
		fi; \
		RUSTFLAGS="$(MEDIA_HELPER_RUSTFLAGS)" CARGO_TARGET_DIR="$(MEDIA_HELPER_TARGET_DIR)" "$(CARGO)" build --manifest-path "$(MEDIA_HELPER_MANIFEST)" --release; \
	else \
		echo "cargo not found; install Rust or set MEDIA_HELPER_DOCKER=1 for a Docker-backed Linux helper build" >&2; \
		exit 127; \
	fi
	@cp "$(MEDIA_HELPER_TARGET_DIR)/release/timich-media-helper" "$(MEDIA_HELPER_BINARY)"
	@chmod +x "$(MEDIA_HELPER_BINARY)"

test-media-helper:
	@command -v "$(CARGO)" >/dev/null 2>&1 || { echo "cargo not found; install Rust to test timich-media-helper" >&2; exit 127; }
	RUSTFLAGS="$(MEDIA_HELPER_RUSTFLAGS)" CARGO_TARGET_DIR="$(MEDIA_HELPER_TARGET_DIR)" "$(CARGO)" test --manifest-path "$(MEDIA_HELPER_MANIFEST)"

media-helper-smoke:
	@test -x "$(MEDIA_HELPER_BINARY)" || { echo "missing executable media helper: $(MEDIA_HELPER_BINARY). Run make build-media-helper first." >&2; exit 127; }
	TIMICH_MEDIA_HELPER_BINARY="$(MEDIA_HELPER_BINARY)" \
	TIMICH_MEDIA_HELPER_SMOKE_IMAGE="$(MEDIA_HELPER_SMOKE_IMAGE)" \
	TIMICH_MEDIA_HELPER_SMOKE_VIDEO="$(MEDIA_HELPER_SMOKE_VIDEO)" \
	python3 tools/media/smoke_media_helper.py

semantic-runtime-pack:
	@mkdir -p "$(SEMANTIC_RUNTIME_PACK_DIR)"
	TIMICH_RUNTIME_PACK_ID="$(SEMANTIC_RUNTIME_PACK_ID)" \
	TIMICH_RUNTIME_PACK_NAME="$(SEMANTIC_RUNTIME_PACK_NAME)" \
	TIMICH_RUNTIME_PACK_VERSION="$(SEMANTIC_RUNTIME_PACK_VERSION)" \
	TIMICH_RUNTIME_PACK_PLATFORM="$(SEMANTIC_RUNTIME_PACK_PLATFORM)" \
	TIMICH_RUNTIME_PACK_OUTPUT_DIR="$(SEMANTIC_RUNTIME_PACK_DIR)" \
	TIMICH_RUNTIME_PACK_ARTIFACT_BASE_URL="$(SEMANTIC_RUNTIME_PACK_BASE_URL)" \
	TIMICH_RUNTIME_PACK_SIGNING_KEY="$(SEMANTIC_RUNTIME_PACK_SIGNING_KEY)" \
	TIMICH_RUNTIME_PACK_REQUIREMENTS="$(SEMANTIC_RUNTIME_PACK_REQUIREMENTS)" \
	TIMICH_RUNTIME_PACK_PYTHON="$(SEMANTIC_RUNTIME_PACK_PYTHON)" \
	TIMICH_RUNTIME_PACK_PYTHON_RUNTIME_ROOT="$(SEMANTIC_RUNTIME_PACK_PYTHON_RUNTIME_ROOT)" \
	TIMICH_RUNTIME_PACK_ALLOW_SOURCE_BUILDS="$(SEMANTIC_RUNTIME_PACK_ALLOW_SOURCE_BUILDS)" \
	TIMICH_RUNTIME_PACK_KEEP_WORK="$(SEMANTIC_RUNTIME_PACK_KEEP_WORK)" \
	python3 semantic-runtime/siglip2-onnx/build_runtime_pack.py

semantic-runtime-pack-validate:
	TIMICH_RUNTIME_PACK_OUTPUT_DIR="$(SEMANTIC_RUNTIME_PACK_DIR)" \
	TIMICH_RUNTIME_PACK_ARTIFACT="$(SEMANTIC_RUNTIME_PACK_ARTIFACT)" \
	TIMICH_RUNTIME_PACK_SHA256_FILE="$(SEMANTIC_RUNTIME_PACK_SHA256_FILE)" \
	TIMICH_RUNTIME_PACK_METADATA="$(SEMANTIC_RUNTIME_PACK_METADATA)" \
	TIMICH_RUNTIME_PACK_SBOM="$(SEMANTIC_RUNTIME_PACK_SBOM)" \
	TIMICH_RUNTIME_PACK_REGISTRY="$(SEMANTIC_RUNTIME_PACK_REGISTRY)" \
	TIMICH_RUNTIME_PACK_SIGNATURE="$(SEMANTIC_RUNTIME_PACK_SIGNATURE)" \
	TIMICH_RUNTIME_PACK_PUBLIC_KEY="$(SEMANTIC_RUNTIME_PACK_PUBLIC_KEY)" \
	TIMICH_RUNTIME_PACK_EXPECTED_PLATFORM="$(SEMANTIC_RUNTIME_PACK_EXPECTED_PLATFORM)" \
	TIMICH_RUNTIME_PACK_REQUIRE_SIGNATURE="$(SEMANTIC_RUNTIME_PACK_REQUIRE_SIGNATURE)" \
	TIMICH_RUNTIME_PACK_REQUIRE_BUNDLED_PYTHON="$(SEMANTIC_RUNTIME_PACK_REQUIRE_BUNDLED_PYTHON)" \
	TIMICH_RUNTIME_PACK_SMOKE_IMPORT="$(SEMANTIC_RUNTIME_PACK_SMOKE_IMPORT)" \
	TIMICH_RUNTIME_PACK_SMOKE_TIMEOUT="$(SEMANTIC_RUNTIME_PACK_SMOKE_TIMEOUT)" \
	python3 semantic-runtime/siglip2-onnx/validate_runtime_pack.py

semantic-model-pack:
	@mkdir -p "$(SEMANTIC_MODEL_PACK_DIR)"
	TIMICH_MODEL_PACK_OUTPUT_DIR="$(SEMANTIC_MODEL_PACK_DIR)" \
	TIMICH_MODEL_PACK_BASE_URL="$(SEMANTIC_MODEL_PACK_BASE_URL)" \
	TIMICH_MODEL_PACK_IMAGE_MODEL="$(SEMANTIC_MODEL_PACK_IMAGE_MODEL)" \
	TIMICH_MODEL_PACK_TEXT_MODEL="$(SEMANTIC_MODEL_PACK_TEXT_MODEL)" \
	TIMICH_MODEL_PACK_PROCESSOR_DIR="$(SEMANTIC_MODEL_PACK_PROCESSOR_DIR)" \
	TIMICH_MODEL_PACK_ID="$(SEMANTIC_MODEL_PACK_ID)" \
	TIMICH_MODEL_PACK_NAME="$(SEMANTIC_MODEL_PACK_NAME)" \
	TIMICH_MODEL_PACK_UPSTREAM_MODEL="$(SEMANTIC_MODEL_PACK_UPSTREAM_MODEL)" \
	TIMICH_MODEL_PACK_VERSION="$(SEMANTIC_MODEL_PACK_VERSION)" \
	TIMICH_MODEL_PACK_EMBEDDING_DIM="$(SEMANTIC_MODEL_PACK_EMBEDDING_DIM)" \
	TIMICH_MODEL_PACK_QUANTIZATION="$(SEMANTIC_MODEL_PACK_QUANTIZATION)" \
	TIMICH_MODEL_PACK_LICENSE="$(SEMANTIC_MODEL_PACK_LICENSE)" \
	TIMICH_MODEL_PACK_SIGNING_KEY="$(SEMANTIC_MODEL_PACK_SIGNING_KEY)" \
	python3 tools/semantic/make_siglip2_onnx_pack.py

semantic-model-pack-validate:
	TIMICH_MODEL_PACK_OUTPUT_DIR="$(SEMANTIC_MODEL_PACK_DIR)" \
	TIMICH_MODEL_PACK_ARTIFACT="$(SEMANTIC_MODEL_PACK_ARTIFACT)" \
	TIMICH_MODEL_PACK_SHA256_FILE="$(SEMANTIC_MODEL_PACK_SHA256_FILE)" \
	TIMICH_MODEL_PACK_METADATA="$(SEMANTIC_MODEL_PACK_METADATA)" \
	TIMICH_MODEL_PACK_SBOM="$(SEMANTIC_MODEL_PACK_SBOM)" \
	TIMICH_MODEL_PACK_REGISTRY="$(SEMANTIC_MODEL_PACK_REGISTRY)" \
	TIMICH_MODEL_PACK_SIGNATURE="$(SEMANTIC_MODEL_PACK_SIGNATURE)" \
	TIMICH_MODEL_PACK_PUBLIC_KEY="$(SEMANTIC_MODEL_PACK_PUBLIC_KEY)" \
	TIMICH_MODEL_PACK_REQUIRE_SIGNATURE="$(SEMANTIC_MODEL_PACK_REQUIRE_SIGNATURE)" \
	python3 tools/semantic/validate_semantic_model_pack.py

semantic-model-registry:
	@mkdir -p "$(DIST_DIR)"
	@inputs="$(SEMANTIC_MODEL_REGISTRY_INPUTS)"; \
		if [ -z "$$inputs" ]; then \
			inputs="$$(find "$(SEMANTIC_MODEL_PACK_DIR)" "$(SEMANTIC_RUNTIME_PACK_DIR)" -maxdepth 1 -type f \( -name 'manifest.json' -o -name '*.registry.json' \) 2>/dev/null | sort)"; \
		fi; \
		if [ -z "$$inputs" ]; then \
			echo "No semantic registry fragments found. Set SEMANTIC_MODEL_REGISTRY_INPUTS or build model/runtime pack fragments first."; \
			exit 1; \
		fi; \
		set -- $$inputs; \
		cmd="python3 tools/semantic/merge_semantic_model_registry.py --output $(SEMANTIC_MODEL_REGISTRY) --version $(SEMANTIC_MODEL_REGISTRY_VERSION)"; \
		if [ -n "$(SEMANTIC_MODEL_REGISTRY_RECOMMENDED)" ]; then cmd="$$cmd --recommended $(SEMANTIC_MODEL_REGISTRY_RECOMMENDED)"; fi; \
		if [ -n "$(SEMANTIC_MODEL_REGISTRY_RECOMMENDED_RUNTIME_PACK)" ]; then cmd="$$cmd --recommended-runtime-pack $(SEMANTIC_MODEL_REGISTRY_RECOMMENDED_RUNTIME_PACK)"; fi; \
		if [ -n "$(SEMANTIC_MODEL_REGISTRY_BASE_URL)" ]; then cmd="$$cmd --base-url $(SEMANTIC_MODEL_REGISTRY_BASE_URL)"; fi; \
		$$cmd "$$@"

semantic-release-validate:
	go run ./cmd/timich-semantic-helper validate-manifest \
		--manifest "$(SEMANTIC_MODEL_REGISTRY)" \
		--required-platform "$(DIST_OS)-$(DIST_ARCH)" \
		--require-recommended-model \
		--require-recommended-runtime-pack
	SEMANTIC_MODEL_REGISTRY="$(SEMANTIC_MODEL_REGISTRY)" \
	SEMANTIC_MODEL_PACK_DIR="$(SEMANTIC_MODEL_PACK_DIR)" \
	SEMANTIC_RUNTIME_PACK_DIR="$(SEMANTIC_RUNTIME_PACK_DIR)" \
	SEMANTIC_RELEASE_BASE_URL="$(AGENT_DIST_BASE_URL)" \
	SEMANTIC_RELEASE_REQUIRE_SIGNATURES="$(SEMANTIC_RELEASE_REQUIRE_SIGNATURES)" \
	python3 tools/semantic/validate_semantic_release.py

media-libvips-runtime-pack:
	TIMICH_MEDIA_LIBVIPS_ID="$(MEDIA_LIBVIPS_RUNTIME_ID)" \
	TIMICH_MEDIA_LIBVIPS_NAME="$(MEDIA_LIBVIPS_RUNTIME_NAME)" \
	TIMICH_MEDIA_LIBVIPS_VERSION="$(MEDIA_LIBVIPS_RUNTIME_VERSION)" \
	TIMICH_MEDIA_LIBVIPS_PLATFORM="$(MEDIA_LIBVIPS_RUNTIME_PLATFORM)" \
	TIMICH_MEDIA_LIBVIPS_DOCKER_PLATFORM="$(MEDIA_LIBVIPS_RUNTIME_DOCKER_PLATFORM)" \
	TIMICH_MEDIA_LIBVIPS_BUILD_IMAGE="$(MEDIA_LIBVIPS_RUNTIME_BUILD_IMAGE)" \
	TIMICH_MEDIA_LIBVIPS_RUNTIME_DIR="$(MEDIA_LIBVIPS_RUNTIME_DIR)" \
	TIMICH_MEDIA_LIBVIPS_PACK_DIR="$(MEDIA_LIBVIPS_RUNTIME_PACK_DIR)" \
	TIMICH_MEDIA_LIBVIPS_APK_PACKAGES="$(MEDIA_LIBVIPS_RUNTIME_APK_PACKAGES)" \
	media-runtime/libvips/builder/build.sh

media-libvips-runtime-verify:
	TIMICH_MEDIA_LIBVIPS_DOCKER_PLATFORM="$(MEDIA_LIBVIPS_RUNTIME_DOCKER_PLATFORM)" \
	media-runtime/libvips/builder/verify.sh "$(MEDIA_LIBVIPS_RUNTIME_DIR)"

media-ffmpeg-runtime-pack:
	TIMICH_MEDIA_FFMPEG_ID="$(MEDIA_FFMPEG_RUNTIME_ID)" \
	TIMICH_MEDIA_FFMPEG_NAME="$(MEDIA_FFMPEG_RUNTIME_NAME)" \
	TIMICH_MEDIA_FFMPEG_VERSION="$(MEDIA_FFMPEG_RUNTIME_VERSION)" \
	TIMICH_MEDIA_FFMPEG_PLATFORM="$(MEDIA_FFMPEG_RUNTIME_PLATFORM)" \
	TIMICH_MEDIA_FFMPEG_DOCKER_PLATFORM="$(MEDIA_FFMPEG_RUNTIME_DOCKER_PLATFORM)" \
	TIMICH_MEDIA_FFMPEG_SOURCE_URL="$(MEDIA_FFMPEG_RUNTIME_SOURCE_URL)" \
	TIMICH_MEDIA_FFMPEG_GPG_KEY_URL="$(MEDIA_FFMPEG_RUNTIME_GPG_KEY_URL)" \
	TIMICH_MEDIA_FFMPEG_GPG_FINGERPRINT="$(MEDIA_FFMPEG_RUNTIME_GPG_FINGERPRINT)" \
	TIMICH_MEDIA_FFMPEG_DOCKERFILE="$(MEDIA_FFMPEG_RUNTIME_DOCKERFILE)" \
	TIMICH_MEDIA_FFMPEG_IMAGE="$(MEDIA_FFMPEG_RUNTIME_IMAGE)" \
	TIMICH_MEDIA_FFMPEG_RUNTIME_DIR="$(MEDIA_FFMPEG_RUNTIME_DIR)" \
	TIMICH_MEDIA_FFMPEG_PACK_DIR="$(MEDIA_FFMPEG_RUNTIME_PACK_DIR)" \
	TIMICH_MEDIA_FFMPEG_BASE_URL="$(MEDIA_FFMPEG_RUNTIME_BASE_URL)" \
	media-runtime/ffmpeg/builder/build.sh

media-ffmpeg-runtime-verify:
	TIMICH_MEDIA_FFMPEG_DOCKER_PLATFORM="$(MEDIA_FFMPEG_RUNTIME_DOCKER_PLATFORM)" \
	media-runtime/ffmpeg/builder/verify.sh "$(MEDIA_FFMPEG_RUNTIME_DIR)"

test:
	go test ./...

init:
	go run ./cmd/timich-agent init -config $(CONFIG_PATH) -data-dir $(DATA_DIR)

run:
	TIMICH_AGENT_CONFIG_PATH=$(CONFIG_PATH) go run ./cmd/timich-agent serve

docker-build:
	docker build -t $(DOCKER_IMAGE) -f Dockerfile .

docker-run: docker-build
	mkdir -p .local
	docker run --rm --init \
		-p 8081:8081 \
		-p 8082:8082 \
		-v "$(PWD)/.local:/var/lib/timich-agent" \
		$(DOCKER_IMAGE)

compose-up:
	mkdir -p .local
	docker compose up --build -d

compose-down:
	docker compose down

dist:
	@rm -rf "$(DIST_STAGE)"
	@mkdir -p "$(DIST_STAGE)" "$(DIST_DIR)"
	GOOS=$(DIST_OS) GOARCH=$(DIST_ARCH) CGO_ENABLED=0 \
		$(MAKE) build \
		BUILD_DIR="$(DIST_STAGE_ABS)" \
		BINARY="$(DIST_STAGE_ABS)/timich-agent" \
		GO_BUILD_FLAGS="-trimpath -buildvcs=false" \
		TIMICH_AGENT_VERSION="$(TIMICH_AGENT_VERSION)" \
		TIMICH_COMMIT="$(TIMICH_COMMIT)" \
		TIMICH_BUILT_AT="$(TIMICH_BUILT_AT)"
	GOOS=$(DIST_OS) GOARCH=$(DIST_ARCH) CGO_ENABLED=0 \
		$(MAKE) build-helper \
		BUILD_DIR="$(DIST_STAGE_ABS)" \
		HELPER_BINARY="$(DIST_STAGE_ABS)/timich-semantic-helper" \
		GO_BUILD_FLAGS="-trimpath -buildvcs=false" \
		TIMICH_AGENT_VERSION="$(TIMICH_AGENT_VERSION)" \
		TIMICH_COMMIT="$(TIMICH_COMMIT)" \
		TIMICH_BUILT_AT="$(TIMICH_BUILT_AT)"
	$(MAKE) build-media-helper \
		BUILD_DIR="$(DIST_STAGE_ABS)" \
		MEDIA_HELPER_BINARY="$(DIST_STAGE_ABS)/timich-media-helper" \
		MEDIA_HELPER_TARGET_DIR="$(abspath $(BUILD_DIR)/media-helper-target-$(DIST_OS)-$(DIST_ARCH))" \
		MEDIA_HELPER_RUSTFLAGS="$(DIST_MEDIA_HELPER_RUSTFLAGS)"
	python3 tools/release/verify_binary_target.py \
		--path "$(DIST_STAGE_ABS)/timich-media-helper" \
		--os "$(DIST_OS)" \
		--arch "$(DIST_ARCH)"
	@printf '%s\n' \
		"TIMICH_AGENT_VERSION=$(TIMICH_AGENT_VERSION)" \
		"TIMICH_COMMIT=$(TIMICH_COMMIT)" \
		"TIMICH_BUILT_AT=$(TIMICH_BUILT_AT)" \
		"TIMICH_AGENT_RELEASE_TAG=$(TIMICH_AGENT_RELEASE_TAG)" \
		"GOOS=$(DIST_OS)" \
		"GOARCH=$(DIST_ARCH)" > "$(DIST_STAGE)/VERSION"
	@printf '%s\n' \
		'{' \
		'  "agentVersion": "$(TIMICH_AGENT_VERSION)",' \
		'  "commit": "$(TIMICH_COMMIT)",' \
		'  "builtAt": "$(TIMICH_BUILT_AT)",' \
		'  "releaseTag": "$(TIMICH_AGENT_RELEASE_TAG)",' \
		'  "goos": "$(DIST_OS)",' \
		'  "goarch": "$(DIST_ARCH)"' \
		'}' > "$(DIST_STAGE)/BUILDINFO.json"
	@mkdir -p "$(DIST_STAGE)/docker"
	@cp docker/entrypoint.sh "$(DIST_STAGE)/docker/entrypoint.sh"
	@chmod +x "$(DIST_STAGE)/docker/entrypoint.sh"
	@mkdir -p "$(DIST_STAGE)/semantic-runtime"
	@cp -R semantic-runtime/siglip2-onnx "$(DIST_STAGE)/semantic-runtime/siglip2-onnx"
	@if [ -d "$(MEDIA_RUNTIME_VIPS_SOURCE)" ]; then \
		mkdir -p "$(DIST_STAGE)/media-runtime/libvips"; \
		cp -R "$(MEDIA_RUNTIME_VIPS_SOURCE)/." "$(DIST_STAGE)/media-runtime/libvips/"; \
		if [ -f "$(DIST_STAGE)/media-runtime/libvips/bin/vips" ]; then chmod +x "$(DIST_STAGE)/media-runtime/libvips/bin/vips"; fi; \
		if [ -f "$(DIST_STAGE)/media-runtime/libvips/bin/vips.exe" ]; then chmod +x "$(DIST_STAGE)/media-runtime/libvips/bin/vips.exe"; fi; \
	elif [ "$(MEDIA_RUNTIME_VIPS_REQUIRED)" = "1" ]; then \
		echo "Missing media runtime libvips source: $(MEDIA_RUNTIME_VIPS_SOURCE)" >&2; \
		exit 1; \
	fi
	@if [ -d "$(MEDIA_RUNTIME_FFMPEG_SOURCE)" ]; then \
		mkdir -p "$(DIST_STAGE)/media-runtime/ffmpeg"; \
		cp -R "$(MEDIA_RUNTIME_FFMPEG_SOURCE)/." "$(DIST_STAGE)/media-runtime/ffmpeg/"; \
		if [ -f "$(DIST_STAGE)/media-runtime/ffmpeg/bin/ffmpeg" ]; then chmod +x "$(DIST_STAGE)/media-runtime/ffmpeg/bin/ffmpeg"; fi; \
		if [ -f "$(DIST_STAGE)/media-runtime/ffmpeg/bin/ffprobe" ]; then chmod +x "$(DIST_STAGE)/media-runtime/ffmpeg/bin/ffprobe"; fi; \
		if [ -f "$(DIST_STAGE)/media-runtime/ffmpeg/bin/ffmpeg.exe" ]; then chmod +x "$(DIST_STAGE)/media-runtime/ffmpeg/bin/ffmpeg.exe"; fi; \
		if [ -f "$(DIST_STAGE)/media-runtime/ffmpeg/bin/ffprobe.exe" ]; then chmod +x "$(DIST_STAGE)/media-runtime/ffmpeg/bin/ffprobe.exe"; fi; \
	elif [ "$(MEDIA_RUNTIME_FFMPEG_REQUIRED)" = "1" ]; then \
		echo "Missing media runtime ffmpeg source: $(MEDIA_RUNTIME_FFMPEG_SOURCE)" >&2; \
		exit 1; \
	fi
	@printf '%s\n' \
		".local" \
		"media-runtime" > "$(DIST_STAGE)/.dockerignore"
	@cp docker/release-bundle.Dockerfile "$(DIST_STAGE)/Dockerfile"
	@bash tools/release/render_bundle_compose.sh \
		compose.yaml \
		"$(DIST_STAGE)/compose.yaml" \
		"$(TIMICH_AGENT_VERSION)"
	@cp compose.immich-network.example.yaml "$(DIST_STAGE)/compose.immich-network.example.yaml"
	@cp compose.local-media.example.yaml "$(DIST_STAGE)/compose.local-media.example.yaml"
	@printf '%s\n' \
		"# Copy this file to .env before running docker compose if you want to customize defaults." \
		"# Display name shown in the Admin UI and paired app sessions." \
		"TIMICH_AGENT_NAME=Timich Agent" \
		"" \
		"# Maximum number of paired app devices allowed by this agent." \
		"TIMICH_AGENT_DEVICE_LIMIT=32" \
		"" \
		"# Optional IANA timezone for agent-local dates, such as upload path date tokens." \
		"# Leave empty to use the container/process timezone." \
		"# TIMICH_AGENT_TIMEZONE=Asia/Tokyo" \
		"" \
		"# Remote Browsing starts automatically after admin setup, datasource setup, and app pairing." \
		"# Set this to false to keep the agent local-only." \
		"TIMICH_AGENT_REMOTE_BROWSING_ENABLED=true" \
		"" \
		"# Change these only if the default host ports are already in use." \
		"TIMICH_AGENT_ADMIN_PORT=8081" \
		"TIMICH_AGENT_MEDIA_PORT=8082" \
		"" \
		"# Optional phone-reachable Media API hint for QR/link candidates." \
		"# TIMICH_AGENT_MEDIA_PUBLISHED_ADDR=10.0.111.128:18082" \
		"" \
		"# Optional read-only host/NAS path used with compose.local-media.yaml." \
		"# The container path remains /media/photos and must also be registered" \
		"# in .local/agent.json under localMediaRoots." \
		"# TIMICH_AGENT_LOCAL_MEDIA_HOST_PATH=/share/Photos" \
		"" \
		"# Optional semantic model runtime helper path. Release bundles and Docker" \
		"# images auto-detect the bundled helper when this is omitted." \
		"# TIMICH_AGENT_SEMANTIC_RUNTIME_HELPER=/usr/local/bin/timich-semantic-helper" \
		"" \
		"# Optional semantic model/runtime registry override. Release binaries default" \
		"# to the semantic-models.json asset on their own release tag." \
		"# TIMICH_AGENT_SEMANTIC_MODEL_MANIFEST_URL=$(TIMICH_AGENT_SEMANTIC_MODEL_MANIFEST_URL)" \
		"" \
		"# Optional shared budget for heavy background work. Empty or omitted uses auto (1); 0 pauses heavy work." \
		"# TIMICH_AGENT_HEAVY_TASK_WORKERS=1" \
		"" \
		"# Optional Rust media helper path for native image/video runtime health." \
		"# Native bundles auto-detect timich-media-helper next to timich-agent when included." \
		"# TIMICH_AGENT_MEDIA_HELPER_PATH=/usr/local/bin/timich-media-helper" \
		"" \
		"# Optional libvips executable path override for local filesystem thumbnails." \
		"# Native bundles auto-detect media-runtime/libvips/bin/vips when included." \
		"# Docker images find /usr/bin/vips on PATH when this is omitted." \
		"# TIMICH_AGENT_VIPS_PATH=/usr/bin/vips" \
		"" \
		"# Optional ffmpeg executable path override for local MP4/MOV poster thumbnails." \
		"# Native bundles auto-detect media-runtime/ffmpeg/bin/ffmpeg when included." \
		"# Docker images find /usr/bin/ffmpeg on PATH when this is omitted." \
		"# TIMICH_AGENT_FFMPEG_PATH=/usr/bin/ffmpeg" \
		"" \
		"# Optional Agent-managed ONNX SigLIP 2 runtime overrides." \
		"# Native bundles auto-detect semantic-runtime/siglip2-onnx/server.py" \
		"# next to timich-agent, and use a bundle-local .venv/venv Python when present." \
		"# TIMICH_AGENT_SEMANTIC_ONNX_SERVER_PATH=/path/to/semantic-runtime/siglip2-onnx/server.py" \
		"# TIMICH_AGENT_SEMANTIC_ONNX_PYTHON=/path/to/python" \
		"# TIMICH_AGENT_SEMANTIC_ONNX_PROVIDER=cpu" > "$(DIST_STAGE)/.env.example"
	@printf '%s\n' \
		"# Timich Agent Bundle" \
		"" \
		"This archive contains the timich-agent release binary, timich-semantic-helper, timich-media-helper, and Docker Compose setup files." \
		"It also includes semantic-runtime/siglip2-onnx for native SigLIP 2 ONNX execution." \
		"Linux native bundles build timich-media-helper with static Rust runtime linking so it can run on NAS hosts without a matching musl loader." \
		"When platform media runtimes are provided at build time, native runs auto-detect media-runtime/libvips/bin/vips and media-runtime/ffmpeg/bin/ffmpeg for local filesystem thumbnails." \
		"Docker images built from the bundle install ffmpeg, vips-tools, and vips-heif for local filesystem thumbnail generation, including MP4/MOV posters and HEIC/HEIF inputs." \
		"The license is included in LICENSE." \
		"" \
		"Docker Compose setup:" \
		"" \
		"\`\`\`sh" \
		"cp .env.example .env" \
		"# Optional: edit .env to rename the agent, change host ports, or opt out of Remote Browsing." \
		"cp compose.immich-network.example.yaml compose.immich-network.yaml" \
		"# Optional: edit compose.immich-network.yaml if your Immich Compose network is not immich_default." \
		"docker compose -f compose.yaml -f compose.immich-network.yaml up -d --build" \
		"docker compose -f compose.yaml -f compose.immich-network.yaml logs -f" \
		"\`\`\`" \
		"" \
		"Most Immich Docker installs need the copied Immich network override." \
		"Use http://immich_server:2283 as the datasource URL in the Admin UI." \
		"If Immich runs directly on the host instead of Docker, omit" \
		"compose.immich-network.yaml from the compose commands." \
		"" \
		"Local filesystem setup (optional):" \
		"" \
		"\`\`\`sh" \
		"cp compose.local-media.example.yaml compose.local-media.yaml" \
		"# Set TIMICH_AGENT_LOCAL_MEDIA_HOST_PATH=/your/host/photo/path in .env." \
		"docker compose -f compose.yaml -f compose.immich-network.yaml -f compose.local-media.yaml up -d --build" \
		"# After the first start creates .local/agent.json, stop the Agent," \
		"# register {\"key\":\"nas-photos\",\"path\":\"/media/photos\"} in localMediaRoots," \
		"# then restart with the same Compose file list." \
		"\`\`\`" \
		"" \
		"The Local media override is read-only. The Admin UI enables Local datasource" \
		"creation only after /media/photos is registered in localMediaRoots and the" \
		"Agent has restarted." \
		"" \
		"Direct native runs use the bundled media-runtime/libvips/bin/vips when present." \
		"If this bundle does not include media-runtime/libvips for your platform, install libvips with HEIF support for your OS and set TIMICH_AGENT_VIPS_PATH if vips is not on PATH." \
		"Direct native runs also use bundled media-runtime/ffmpeg/bin/ffmpeg when present for MP4/MOV poster thumbnails." \
		"If this bundle does not include media-runtime/ffmpeg for your platform, install ffmpeg for your OS and set TIMICH_AGENT_FFMPEG_PATH if ffmpeg is not on PATH." \
		"" \
		"On first run, the logs show the Admin UI URL (default URL from the host is" \
		"http://127.0.0.1:8081/). Open it from a trusted LAN and create the admin" \
		"token in the browser." \
		"" \
		"Quick checks:" \
		"" \
		"\`\`\`sh" \
		"./timich-agent version" \
		"./timich-agent version-json" \
		"./timich-semantic-helper version" \
		"./timich-media-helper health --json" \
		"" \
		"# Open the Admin UI URL shown in the logs to finish setup, manage devices," \
		"# configure the datasource, and run Remote Browsing checks." \
		"\`\`\`" \
		"" \
		"One-time pre-release V2 catalog migration:" \
		"" \
		"Skip this section unless this installation is explicitly known to use the" \
		"unreleased catalog schema V2. Normal startup never migrates it. The command" \
		"below is part of this exact versioned timich-agent binary and refuses any" \
		"schema other than V2 or the already-current V3." \
		"" \
		"For Docker Compose, stop the old Agent, extract this complete new bundle," \
		"build without starting, then migrate the mounted state:" \
		"" \
		"\`\`\`sh" \
		"compose_args=(-f compose.yaml -f compose.immich-network.yaml)" \
		"if [ -f compose.local-media.yaml ]; then compose_args+=(-f compose.local-media.yaml); fi" \
		'docker compose "$${compose_args[@]}" down' \
		'# Extract the new complete bundle before continuing.' \
		'docker compose "$${compose_args[@]}" build timich-agent' \
		'docker compose "$${compose_args[@]}" run --rm --no-deps \' \
		'  --entrypoint /usr/local/bin/timich-agent timich-agent \' \
		'  pre-release-migrate-catalog-v2-v3 \' \
		'  --data-dir /var/lib/timich-agent/state \' \
		'  --backup /var/lib/timich-agent/backups/catalog-v2-before-v3.db \' \
		'  --confirm-agent-stopped' \
		'docker compose "$${compose_args[@]}" up -d' \
		'docker compose "$${compose_args[@]}" logs -f' \
		"\`\`\`" \
		"" \
		"For a stopped native service, use the exact configured data directory:" \
		"" \
		"\`\`\`sh" \
		"state_root=/var/lib/timich-agent" \
		'install -d -m 0700 "$$state_root/backups"' \
		'./timich-agent pre-release-migrate-catalog-v2-v3 \' \
		'  --data-dir "$$state_root/state" \' \
		'  --backup "$$state_root/backups/catalog-v2-before-v3.db" \' \
		'  --confirm-agent-stopped' \
		"\`\`\`" \
		"" \
		"Success reports the exact Agent version and commit, fromVersion 2, toVersion 3," \
		"and preserved asset/semantic counts. Keep the exclusive backup and previous" \
		"bundle until Gallery browsing and semantic search are verified." \
		"" \
		"Compose updates must use the exact same -f file list for down, up, and logs." \
		"Local datasource installations must include compose.local-media.yaml in every" \
		"one of those commands so /media/photos remains mounted." \
		"" \
		"Native updates replace this complete versioned bundle, not only timich-agent." \
		"If the current service uses the documented relative .local defaults, stop it" \
		"and copy the complete .local contents to a stable private directory outside" \
		"all bundle versions. Configure absolute -config and -data-dir paths (or the" \
		"equivalent environment variables) before changing bundles, and keep the old" \
		".local copy until the new service is verified." \
		"" \
		"\`\`\`sh" \
		"state_root=/var/lib/timich-agent" \
		"install -d -m 0700 \"\$$state_root\"" \
		"cp -a .local/. \"\$$state_root/\"" \
		"# Configure: timich-agent serve -config /var/lib/timich-agent/agent.json -data-dir /var/lib/timich-agent/state" \
		"\`\`\`" \
		"" \
		"After state is externalized, stop the supervisor," \
		"extract the new archive into a new directory, atomically repoint the service" \
		"working directory or current symlink, then verify all quick checks above" \
		"before removing the previous bundle." > "$(DIST_STAGE)/README.md"
	@cp LICENSE "$(DIST_STAGE)/LICENSE"
	@xattr -cr "$(DIST_STAGE)" 2>/dev/null || true
	@COPYFILE_DISABLE=1 tar --no-xattrs -C "build/dist" -czf "$(DIST_ARCHIVE)" "$(DIST_NAME)"
	@archive_sha="$$(shasum -a 256 "$(DIST_ARCHIVE)" | awk '{print $$1}')" && \
		printf '%s  %s\n' "$$archive_sha" "$(DIST_NAME).tar.gz" > "$(DIST_ARCHIVE).sha256"
	@echo "Wrote $(DIST_ARCHIVE)"

update-manifest:
	@mkdir -p "$(DIST_DIR)"
	@archives="$$(find "$(DIST_DIR)" -maxdepth 1 -type f -name 'timich-agent_$(TIMICH_AGENT_VERSION)_*_*.tar.gz' | sort)" && \
		if [ -z "$$archives" ]; then echo "No agent archives found for $(TIMICH_AGENT_VERSION) in $(DIST_DIR). Run make dist for each platform first."; exit 1; fi && \
		tmp="$(AGENT_UPDATE_MANIFEST).tmp" && \
		printf '%s\n' \
			'{' \
			'  "schemaVersion": 1,' \
			'  "product": "timich-agent",' \
			'  "version": "$(TIMICH_AGENT_VERSION)",' \
			'  "channel": "$(AGENT_UPDATE_CHANNEL)",' \
			'  "releaseTag": "$(AGENT_DIST_TAG)",' \
			'  "releasedAt": "$(TIMICH_BUILT_AT)",' \
			'  "commit": "$(TIMICH_COMMIT)",' \
			'  "minimumSupportedVersion": "0.1.0",' \
			'  "notesUrl": "$(AGENT_DIST_NOTES_URL)",' \
			'  "artifacts": {' > "$$tmp" && \
		first=1 && \
		for archive in $$archives; do \
			filename="$$(basename "$$archive")"; \
			suffix="$${filename#timich-agent_$(TIMICH_AGENT_VERSION)_}"; \
			suffix="$${suffix%.tar.gz}"; \
			artifact_os="$${suffix%_*}"; \
			artifact_arch="$${suffix##*_}"; \
			platform="$${artifact_os}-$${artifact_arch}"; \
			sha_file="$$archive.sha256"; \
			if [ ! -f "$$sha_file" ]; then echo "Missing checksum for $$filename"; rm -f "$$tmp"; exit 1; fi; \
			archive_sha="$$(awk '{print $$1}' "$$sha_file")"; \
			actual_sha="$$(shasum -a 256 "$$archive" | awk '{print $$1}')"; \
			if [ "$$archive_sha" != "$$actual_sha" ]; then echo "Checksum mismatch for $$filename"; rm -f "$$tmp"; exit 1; fi; \
			buildinfo_path="timich-agent_$(TIMICH_AGENT_VERSION)_$${artifact_os}_$${artifact_arch}/BUILDINFO.json"; \
			buildinfo="$$(tar -xOzf "$$archive" "$$buildinfo_path")" || { echo "Missing BUILDINFO.json in $$filename"; rm -f "$$tmp"; exit 1; }; \
			build_agent_version="$$(printf '%s' "$$buildinfo" | jq -r '.agentVersion // ""')" && \
			build_commit="$$(printf '%s' "$$buildinfo" | jq -r '.commit // ""')" && \
			build_release_tag="$$(printf '%s' "$$buildinfo" | jq -r '.releaseTag // ""')" && \
			build_goos="$$(printf '%s' "$$buildinfo" | jq -r '.goos // ""')" && \
			build_goarch="$$(printf '%s' "$$buildinfo" | jq -r '.goarch // ""')" || { echo "Could not parse BUILDINFO.json in $$filename"; rm -f "$$tmp"; exit 1; }; \
			if [ "$$build_agent_version" != "$(TIMICH_AGENT_VERSION)" ]; then echo "BUILDINFO agentVersion mismatch for $$filename: $$build_agent_version"; rm -f "$$tmp"; exit 1; fi; \
			if [ "$$build_commit" != "$(TIMICH_COMMIT)" ]; then echo "BUILDINFO commit mismatch for $$filename: $$build_commit != $(TIMICH_COMMIT)"; rm -f "$$tmp"; exit 1; fi; \
			if [ "$$build_release_tag" != "$(AGENT_DIST_TAG)" ]; then echo "BUILDINFO releaseTag mismatch for $$filename: $$build_release_tag != $(AGENT_DIST_TAG)"; rm -f "$$tmp"; exit 1; fi; \
			if [ "$$build_goos" != "$$artifact_os" ] || [ "$$build_goarch" != "$$artifact_arch" ]; then echo "BUILDINFO platform mismatch for $$filename: $$build_goos/$$build_goarch"; rm -f "$$tmp"; exit 1; fi; \
			if [ "$$first" -eq 0 ]; then printf ',\n' >> "$$tmp"; fi; \
			first=0; \
			printf '%s\n' \
				"    \"$$platform\": {" \
				"      \"filename\": \"$$filename\"," \
				"      \"url\": \"$(AGENT_DIST_BASE_URL)/$$filename\"," \
				"      \"sha256\": \"$$archive_sha\"" \
				"    }" >> "$$tmp"; \
		done && \
		printf '%s\n' \
			'' \
			'  },' \
				'  "updateGuide": {' \
				'    "dockerCompose": [' \
				'      "Keep the existing .env, .local directory, and copied Compose overrides such as compose.immich-network.yaml and compose.local-media.yaml.",' \
				'      "Download and extract the complete new Timich Agent bundle; do not copy individual executables between bundle versions.",' \
				'      "Run docker compose down, up, and logs with the exact same -f file list used at first run, including compose.local-media.yaml for every Local datasource installation.",' \
				'      "Open the Admin UI again and confirm the new version is running."' \
				'    ],' \
				'    "manualBinary": [' \
				'      "Stop the supervised timich-agent process.",' \
				'      "If the current install uses the default relative .local directory, copy its complete contents to a stable private directory outside bundle versions and configure absolute -config and -data-dir paths before switching.",' \
				'      "Extract the complete new archive into a new versioned directory; timich-agent, both helpers, semantic runtime, and platform media runtimes are one release set.",' \
				'      "Keep the original .local copy until the new service is verified, and never run old and new service processes against the shared state simultaneously.",' \
				'      "Atomically repoint the supervisor working directory or current symlink to the new bundle, start it, and verify Agent, helper, and runtime health before removing the previous bundle."' \
				'    ]' \
			'  }' \
			'}' >> "$$tmp" && \
		mv "$$tmp" "$(AGENT_UPDATE_MANIFEST)"
	@echo "Wrote $(AGENT_UPDATE_MANIFEST)"

publish-dist:
	@echo "publish-dist is disabled: publish Timich Agent releases only through the protected Release Timich Agent Bundle workflow." >&2
	@exit 2

prerelease-stage:
	TIMICH_AGENT_VERSION="$(PRERELEASE_VERSION)" \
	AGENT_DIST_REPO="$(AGENT_DIST_REPO)" \
	AGENT_DIST_TAG="$(PRERELEASE_TAG)" \
	DIST_DIR="$(PRERELEASE_DIST_DIR)" \
	SEMANTIC_MODEL_PACK_DIR="$(SEMANTIC_MODEL_PACK_DIR)" \
	SEMANTIC_RUNTIME_PACK_DIR="$(SEMANTIC_RUNTIME_PACK_DIR)" \
	tools/release/stage_prerelease_assets.sh

prerelease-upload:
	TIMICH_AGENT_VERSION="$(PRERELEASE_VERSION)" \
	AGENT_DIST_REPO="$(AGENT_DIST_REPO)" \
	AGENT_DIST_TAG="$(PRERELEASE_TAG)" \
	PUBLIC_SOURCE_SHA="$(PUBLIC_SOURCE_SHA)" \
	DIST_DIR="$(PRERELEASE_DIST_DIR)" \
	SEMANTIC_MODEL_PACK_DIR="$(SEMANTIC_MODEL_PACK_DIR)" \
	SEMANTIC_RUNTIME_PACK_DIR="$(SEMANTIC_RUNTIME_PACK_DIR)" \
	tools/release/upload_prerelease_assets.sh

prerelease-publish:
	@echo "prerelease-publish is disabled: local tooling may only stage or upload a draft; use the protected release workflow to publish." >&2
	@exit 2
