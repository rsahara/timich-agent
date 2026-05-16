.DEFAULT_GOAL := help

-include ../../versions.mk

TIMICH_AGENT_VERSION ?= 0.1.0
TIMICH_AGENT_DIST_REPO ?= rsahara/timich-agent
TIMICH_COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
TIMICH_BUILT_AT ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
TIMICH_AGENT_UPDATE_MANIFEST_URL ?= https://github.com/$(TIMICH_AGENT_DIST_REPO)/releases/latest/download/agent-update-manifest.json
GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
TIMICH_AGENT_LDFLAGS ?= -X main.version=$(TIMICH_AGENT_VERSION) -X main.commit=$(TIMICH_COMMIT) -X main.builtAt=$(TIMICH_BUILT_AT) -X main.updateManifestURL=$(TIMICH_AGENT_UPDATE_MANIFEST_URL)
GOCACHE ?= $(abspath build/go-build-cache)
export GOCACHE

BUILD_DIR ?= build
BINARY ?= $(BUILD_DIR)/timich-agent
GO_BUILD_FLAGS ?=
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
AGENT_UPDATE_MANIFEST := $(DIST_DIR)/agent-update-manifest.json
AGENT_DIST_REPO ?= $(TIMICH_AGENT_DIST_REPO)
AGENT_DIST_TAG ?= v$(TIMICH_AGENT_VERSION)
AGENT_DIST_TITLE ?= Timich Agent $(TIMICH_AGENT_VERSION)
AGENT_DIST_NOTES ?= Timich Agent $(TIMICH_AGENT_VERSION) release artifacts.
AGENT_DIST_BASE_URL ?= https://github.com/$(AGENT_DIST_REPO)/releases/download/$(AGENT_DIST_TAG)
AGENT_DIST_NOTES_URL ?= https://github.com/$(AGENT_DIST_REPO)/releases/tag/$(AGENT_DIST_TAG)

.PHONY: help build test init run docker-build docker-run compose-up compose-down dist update-manifest publish-dist

help:
	@echo "timich-agent"
	@echo ""
	@echo "Available targets:"
	@echo "  make build   Build the timich-agent binary"
	@echo "  make test    Run timich-agent Go tests"
	@echo "  make init    Write a starter local config file"
	@echo "  make run     Run the local admin and media APIs"
	@echo "  make docker-build  Build the timich-agent Docker image"
	@echo "  make docker-run    Run timich-agent in Docker for foreground testing"
	@echo "  make compose-up    Start timich-agent with docker compose"
	@echo "  make compose-down  Stop the docker compose timich-agent"
	@echo "  make dist          Build a timich-agent release tarball"
	@echo "  make update-manifest"
	@echo "                     Build manifest from all agent tarballs in dist/"
	@echo "  make publish-dist  Build and upload agent artifacts to GitHub Releases"

build:
	@mkdir -p $(BUILD_DIR)
	go build $(GO_BUILD_FLAGS) -ldflags "$(TIMICH_AGENT_LDFLAGS)" -o $(BINARY) ./cmd/timich-agent

test:
	go test ./...

init:
	go run ./cmd/timich-agent init -config $(CONFIG_PATH) -data-dir $(DATA_DIR)

run:
	TIMICH_AGENT_CONFIG_PATH=$(CONFIG_PATH) go run ./cmd/timich-agent serve

docker-build:
	docker build -t $(DOCKER_IMAGE) .

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
	@printf '%s\n' \
		"TIMICH_AGENT_VERSION=$(TIMICH_AGENT_VERSION)" \
		"TIMICH_COMMIT=$(TIMICH_COMMIT)" \
		"TIMICH_BUILT_AT=$(TIMICH_BUILT_AT)" \
		"GOOS=$(DIST_OS)" \
		"GOARCH=$(DIST_ARCH)" > "$(DIST_STAGE)/VERSION"
	@printf '%s\n' \
		'{' \
		'  "agentVersion": "$(TIMICH_AGENT_VERSION)",' \
		'  "commit": "$(TIMICH_COMMIT)",' \
		'  "builtAt": "$(TIMICH_BUILT_AT)",' \
		'  "goos": "$(DIST_OS)",' \
		'  "goarch": "$(DIST_ARCH)"' \
		'}' > "$(DIST_STAGE)/BUILDINFO.json"
	@mkdir -p "$(DIST_STAGE)/docker"
	@cp docker/entrypoint.sh "$(DIST_STAGE)/docker/entrypoint.sh"
	@chmod +x "$(DIST_STAGE)/docker/entrypoint.sh"
	@printf '%s\n' \
		"FROM alpine:3.22" \
		"" \
		"RUN apk add --no-cache ca-certificates" \
		"" \
		"WORKDIR /app" \
		"" \
		"COPY timich-agent /usr/local/bin/timich-agent" \
		"COPY docker/entrypoint.sh /usr/local/bin/timich-agent-entrypoint" \
		"" \
		"RUN chmod +x /usr/local/bin/timich-agent-entrypoint && \\" \
		"	mkdir -p /var/lib/timich-agent" \
		"" \
		"EXPOSE 8081 8082" \
		"" \
		"ENTRYPOINT [\"/usr/local/bin/timich-agent-entrypoint\"]" \
		"CMD []" > "$(DIST_STAGE)/Dockerfile"
	@printf '%s\n' \
		"name: timich-agent" \
		"" \
		"services:" \
		"  timich-agent:" \
		"    build:" \
		"      context: ." \
		"      dockerfile: Dockerfile" \
		"    image: timich-agent:$(TIMICH_AGENT_VERSION)" \
		"    container_name: timich-agent" \
		"    init: true" \
		"    restart: unless-stopped" \
		"    environment:" \
		'      TIMICH_AGENT_NAME: "$${TIMICH_AGENT_NAME:-Timich Agent}"' \
		'      TIMICH_AGENT_DEVICE_LIMIT: "$${TIMICH_AGENT_DEVICE_LIMIT:-32}"' \
		'      TIMICH_AGENT_MEDIA_PUBLISHED_ADDR: "$${TIMICH_AGENT_MEDIA_PUBLISHED_ADDR:-$${TIMICH_AGENT_MEDIA_PORT:-8082}}"' \
		'      TIMICH_AGENT_REMOTE_BROWSING_ENABLED: "$${TIMICH_AGENT_REMOTE_BROWSING_ENABLED:-true}"' \
		"    ports:" \
		'      - "$${TIMICH_AGENT_ADMIN_PORT:-8081}:8081"' \
		'      - "$${TIMICH_AGENT_MEDIA_PORT:-8082}:8082"' \
		"    volumes:" \
		"      - ./.local:/var/lib/timich-agent" \
		"    healthcheck:" \
		"      test: [\"CMD-SHELL\", \"wget -q --spider http://127.0.0.1:8081/healthz\"]" \
		"      interval: 30s" \
		"      timeout: 5s" \
		"      retries: 3" \
		"      start_period: 5s" > "$(DIST_STAGE)/compose.yaml"
	@printf '%s\n' \
		"# Copy this file to .env before running docker compose if you want to customize defaults." \
		"# Display name shown in the Admin UI and paired app sessions." \
		"TIMICH_AGENT_NAME=Timich Agent" \
		"" \
		"# Maximum number of paired app devices allowed by this agent." \
		"TIMICH_AGENT_DEVICE_LIMIT=32" \
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
		"# TIMICH_AGENT_MEDIA_PUBLISHED_ADDR=10.0.111.128:18082" > "$(DIST_STAGE)/.env.example"
	@printf '%s\n' \
		"# Timich Agent Bundle" \
		"" \
		"This archive contains the timich-agent release binary and Docker Compose setup files." \
		"The license is included in LICENSE." \
		"" \
		"Docker Compose setup:" \
		"" \
		"\`\`\`sh" \
		"cp .env.example .env" \
		"# Optional: edit .env to rename the agent or opt out of Remote Browsing." \
		"docker compose -f compose.yaml up -d --build" \
		"docker compose -f compose.yaml logs -f" \
		"\`\`\`" \
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
		"" \
		"# Open the Admin UI URL shown in the logs to finish setup, manage devices," \
		"# configure the datasource, and run Remote Browsing checks." \
		"\`\`\`" > "$(DIST_STAGE)/README.md"
	@cp LICENSE "$(DIST_STAGE)/LICENSE"
	@tar -C "build/dist" -czf "$(DIST_ARCHIVE)" "$(DIST_NAME)"
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
			'  "channel": "stable",' \
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
			build_goos="$$(printf '%s' "$$buildinfo" | jq -r '.goos // ""')" && \
			build_goarch="$$(printf '%s' "$$buildinfo" | jq -r '.goarch // ""')" || { echo "Could not parse BUILDINFO.json in $$filename"; rm -f "$$tmp"; exit 1; }; \
			if [ "$$build_agent_version" != "$(TIMICH_AGENT_VERSION)" ]; then echo "BUILDINFO agentVersion mismatch for $$filename: $$build_agent_version"; rm -f "$$tmp"; exit 1; fi; \
			if [ "$$build_commit" != "$(TIMICH_COMMIT)" ]; then echo "BUILDINFO commit mismatch for $$filename: $$build_commit != $(TIMICH_COMMIT)"; rm -f "$$tmp"; exit 1; fi; \
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
			'      "Keep the existing .local directory; it contains agent settings, admin token, and paired devices.",' \
			'      "Download and extract the new Timich Agent bundle next to that .local directory.",' \
			'      "Run docker compose down, then docker compose up -d --build from the new bundle directory.",' \
			'      "Open the Admin UI again and confirm the new version is running."' \
			'    ],' \
			'    "manualBinary": [' \
			'      "Stop the supervised timich-agent process.",' \
			'      "Replace the timich-agent binary with the new release binary.",' \
			'      "Start the service again and confirm /version reports the new version."' \
			'    ]' \
			'  }' \
			'}' >> "$$tmp" && \
		mv "$$tmp" "$(AGENT_UPDATE_MANIFEST)"
	@echo "Wrote $(AGENT_UPDATE_MANIFEST)"

publish-dist: dist update-manifest
	@command -v gh >/dev/null 2>&1 || { echo "gh is required to publish release assets."; exit 1; }
	@if gh release view "$(AGENT_DIST_TAG)" --repo "$(AGENT_DIST_REPO)" >/dev/null 2>&1; then \
		echo "Using existing release $(AGENT_DIST_REPO) $(AGENT_DIST_TAG)"; \
	else \
		gh release create "$(AGENT_DIST_TAG)" \
			--repo "$(AGENT_DIST_REPO)" \
			--title "$(AGENT_DIST_TITLE)" \
			--notes "$(AGENT_DIST_NOTES)"; \
	fi
	@artifacts="$$(find "$(DIST_DIR)" -maxdepth 1 -type f \( -name 'timich-agent_$(TIMICH_AGENT_VERSION)_*_*.tar.gz' -o -name 'timich-agent_$(TIMICH_AGENT_VERSION)_*_*.tar.gz.sha256' \) | sort)" && \
		if [ -z "$$artifacts" ]; then echo "No agent release artifacts found for $(TIMICH_AGENT_VERSION) in $(DIST_DIR)."; exit 1; fi && \
		gh release upload "$(AGENT_DIST_TAG)" $$artifacts "$(AGENT_UPDATE_MANIFEST)" --repo "$(AGENT_DIST_REPO)" --clobber
	@echo "Published agent artifacts and $(AGENT_UPDATE_MANIFEST) to $(AGENT_DIST_REPO) $(AGENT_DIST_TAG)"
