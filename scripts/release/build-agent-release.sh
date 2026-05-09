#!/usr/bin/env bash
set -euo pipefail

usage() {
	cat <<'USAGE'
Usage: build-agent-release.sh --version VERSION --output DIR [--platform GOOS/GOARCH ...]

Build Timich Agent release bundles, checksums, and agent-update-manifest.json.

Options:
  --version VERSION       Stable release version. Accepts 0.1.0 or v0.1.0.
  --output DIR            Directory for release artifacts.
  --platform GOOS/GOARCH  Target platform. May be repeated.
                          Defaults to linux/amd64 and linux/arm64.
USAGE
}

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
version=""
output=""
platforms=()

while [ "$#" -gt 0 ]; do
	case "$1" in
		--version)
			if [ "$#" -lt 2 ]; then
				usage >&2
				exit 2
			fi
			version="${2#v}"
			shift 2
			;;
		--output)
			if [ "$#" -lt 2 ]; then
				usage >&2
				exit 2
			fi
			output="$2"
			shift 2
			;;
		--platform)
			if [ "$#" -lt 2 ]; then
				usage >&2
				exit 2
			fi
			platforms+=("$2")
			shift 2
			;;
		-h|--help)
			usage
			exit 0
			;;
		*)
			echo "unknown argument: $1" >&2
			usage >&2
			exit 2
			;;
	esac
done

if [ -z "$version" ] || [ -z "$output" ]; then
	usage >&2
	exit 2
fi

if ! [[ "$version" =~ ^[0-9]+[.][0-9]+[.][0-9]+$ ]]; then
	echo "stable release version must look like 0.1.0 or v0.1.0: $version" >&2
	exit 2
fi

if [ "${#platforms[@]}" -eq 0 ]; then
	platforms=("linux/amd64" "linux/arm64")
fi

cd "$repo_root"

export GOCACHE="${GOCACHE:-$repo_root/build/go-build-cache}"

output_parent="$(dirname "$output")"
output_name="$(basename "$output")"
mkdir -p "$output_parent"
output_parent_abs="$(cd "$output_parent" && pwd)"
output_abs="$output_parent_abs/$output_name"
build_root="$repo_root/build/release"
stage_root="$build_root/stage"

rm -rf "$build_root" "$output_abs"
mkdir -p "$stage_root" "$output_abs"

commit="${TIMICH_COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || echo unknown)}"
built_at="${TIMICH_BUILT_AT:-$(date -u +"%Y-%m-%dT%H:%M:%SZ")}"
repository="${GITHUB_REPOSITORY:-rsahara/timich-agent}"
release_tag="v$version"
base_url="${TIMICH_AGENT_DIST_BASE_URL:-https://github.com/$repository/releases/download/$release_tag}"
notes_url="${TIMICH_AGENT_DIST_NOTES_URL:-https://github.com/$repository/releases/tag/$release_tag}"
update_manifest_url="${TIMICH_AGENT_UPDATE_MANIFEST_URL:-https://github.com/$repository/releases/latest/download/agent-update-manifest.json}"

sha256_file() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	else
		shasum -a 256 "$1" | awk '{print $1}'
	fi
}

write_bundle_readme() {
	local path="$1"
	cat > "$path" <<'README'
# Timich Agent Bundle

This archive contains the timich-agent release binary and Docker Compose setup files.

Docker Compose setup:

```sh
cp .env.example .env
# Optional: edit .env to rename the agent or opt out of Remote Browsing.
docker compose -f compose.yaml up -d --build
docker compose -f compose.yaml logs -f
```

On first run, the logs show the Admin UI URL. Open it from a trusted LAN
and create the admin token in the browser.

Quick checks:

```sh
./timich-agent version
./timich-agent version-json

# Open the Admin UI URL shown in the logs to finish setup, manage devices,
# configure the datasource, and run Remote Browsing checks.
```
README
}

write_bundle_dockerfile() {
	local path="$1"
	cat > "$path" <<'DOCKERFILE'
FROM alpine:3.22

RUN apk add --no-cache ca-certificates

WORKDIR /app

COPY timich-agent /usr/local/bin/timich-agent
COPY docker/entrypoint.sh /usr/local/bin/timich-agent-entrypoint

RUN chmod +x /usr/local/bin/timich-agent-entrypoint && \
	mkdir -p /var/lib/timich-agent

EXPOSE 8081 8082

ENTRYPOINT ["/usr/local/bin/timich-agent-entrypoint"]
CMD []
DOCKERFILE
}

write_bundle_compose() {
	local path="$1"
	local image_tag="$2"
	cat > "$path" <<COMPOSE
name: timich-agent

services:
  timich-agent:
    build:
      context: .
      dockerfile: Dockerfile
    image: timich-agent:$image_tag
    container_name: timich-agent
    init: true
    restart: unless-stopped
    environment:
      TIMICH_AGENT_NAME: "\${TIMICH_AGENT_NAME:-Timich Agent}"
      TIMICH_AGENT_DEVICE_LIMIT: "\${TIMICH_AGENT_DEVICE_LIMIT:-32}"
      TIMICH_AGENT_REMOTE_BROWSING_ENABLED: "\${TIMICH_AGENT_REMOTE_BROWSING_ENABLED:-true}"
    ports:
      - "\${TIMICH_AGENT_ADMIN_PORT:-8081}:8081"
      - "\${TIMICH_AGENT_MEDIA_PORT:-8082}:8082"
    volumes:
      - ./.local:/var/lib/timich-agent
    healthcheck:
      test: ["CMD-SHELL", "wget -q --spider http://127.0.0.1:8081/healthz"]
      interval: 30s
      timeout: 5s
      retries: 3
      start_period: 5s
COMPOSE
}

write_bundle_env_example() {
	local path="$1"
	cat > "$path" <<'ENVEXAMPLE'
# Copy this file to .env before running docker compose if you want to customize defaults.
# Display name shown in the Admin UI and paired app sessions.
TIMICH_AGENT_NAME=Timich Agent

# Maximum number of paired app devices allowed by this agent.
TIMICH_AGENT_DEVICE_LIMIT=32

# Remote Browsing starts automatically after admin setup, datasource setup, and app pairing.
# Set this to false to keep the agent local-only.
TIMICH_AGENT_REMOTE_BROWSING_ENABLED=true

# Change these only if the default host ports are already in use.
TIMICH_AGENT_ADMIN_PORT=8081
TIMICH_AGENT_MEDIA_PORT=8082
ENVEXAMPLE
}

artifacts=()

for platform in "${platforms[@]}"; do
	if [[ "$platform" != */* ]]; then
		echo "platform must look like GOOS/GOARCH: $platform" >&2
		exit 2
	fi
	dist_os="${platform%/*}"
	dist_arch="${platform#*/}"
	dist_name="timich-agent_${version}_${dist_os}_${dist_arch}"
	stage="$stage_root/$dist_name"
	archive="$output_abs/$dist_name.tar.gz"

	rm -rf "$stage"
	mkdir -p "$stage/docker"

	GOOS="$dist_os" GOARCH="$dist_arch" CGO_ENABLED=0 \
		go build -trimpath -buildvcs=false \
		-ldflags "-X main.version=$version -X main.commit=$commit -X main.builtAt=$built_at -X main.updateManifestURL=$update_manifest_url" \
		-o "$stage/timich-agent" \
		./cmd/timich-agent

	cat > "$stage/VERSION" <<VERSION
TIMICH_AGENT_VERSION=$version
TIMICH_COMMIT=$commit
TIMICH_BUILT_AT=$built_at
GOOS=$dist_os
GOARCH=$dist_arch
VERSION

	cat > "$stage/BUILDINFO.json" <<BUILDINFO
{
  "agentVersion": "$version",
  "commit": "$commit",
  "builtAt": "$built_at",
  "goos": "$dist_os",
  "goarch": "$dist_arch"
}
BUILDINFO

	cp docker/entrypoint.sh "$stage/docker/entrypoint.sh"
	chmod +x "$stage/docker/entrypoint.sh"
	write_bundle_dockerfile "$stage/Dockerfile"
	write_bundle_compose "$stage/compose.yaml" "$version"
	write_bundle_env_example "$stage/.env.example"
	write_bundle_readme "$stage/README.md"

	tar -C "$stage_root" -czf "$archive" "$dist_name"
	archive_sha="$(sha256_file "$archive")"
	printf '%s  %s\n' "$archive_sha" "$dist_name.tar.gz" > "$archive.sha256"
	artifacts+=("$dist_os/$dist_arch:$dist_name.tar.gz:$archive_sha")
done

manifest="$output_abs/agent-update-manifest.json"
{
	cat <<MANIFEST_HEAD
{
  "schemaVersion": 1,
  "product": "timich-agent",
  "version": "$version",
  "channel": "stable",
  "releasedAt": "$built_at",
  "commit": "$commit",
  "minimumSupportedVersion": "0.1.0",
  "notesUrl": "$notes_url",
  "artifacts": {
MANIFEST_HEAD

	first=1
	for artifact in "${artifacts[@]}"; do
		platform="${artifact%%:*}"
		rest="${artifact#*:}"
		filename="${rest%%:*}"
		sha="${rest#*:}"
		artifact_key="${platform//\//-}"
		if [ "$first" -eq 0 ]; then
			printf ',\n'
		fi
		first=0
		printf '    "%s": {\n' "$artifact_key"
		printf '      "filename": "%s",\n' "$filename"
		printf '      "url": "%s/%s",\n' "$base_url" "$filename"
		printf '      "sha256": "%s"\n' "$sha"
		printf '    }'
	done

	cat <<'MANIFEST_TAIL'

  },
  "updateGuide": {
    "dockerCompose": [
      "Keep the existing .local directory; it contains agent settings, admin token, and paired devices.",
      "Download and extract the new Timich Agent bundle next to that .local directory.",
      "Run docker compose down, then docker compose up -d --build from the new bundle directory.",
      "Open the Admin UI again and confirm the new version is running."
    ],
    "manualBinary": [
      "Stop the supervised timich-agent process.",
      "Replace the timich-agent binary with the new release binary.",
      "Start the service again and confirm /version reports the new version."
    ]
  }
}
MANIFEST_TAIL
} > "$manifest"

printf 'Wrote release artifacts to %s\n' "$output_abs"
