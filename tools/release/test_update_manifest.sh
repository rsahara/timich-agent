#!/usr/bin/env bash
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
agent_root=$(CDPATH= cd -- "$script_dir/../.." && pwd)
temporary_dir=$(mktemp -d)
trap 'rm -rf "$temporary_dir"' EXIT
dist_dir="$temporary_dir/dist"
stage_dir="$temporary_dir/timich-agent_0.4.0_linux_amd64"
mkdir -p "$dist_dir" "$stage_dir"

cat > "$stage_dir/BUILDINFO.json" <<'JSON'
{
  "agentVersion": "0.4.0",
  "commit": "test-commit",
  "releaseTag": "v0.4.0",
  "goos": "linux",
  "goarch": "amd64"
}
JSON
archive="$dist_dir/timich-agent_0.4.0_linux_amd64.tar.gz"
tar -C "$temporary_dir" -czf "$archive" "$(basename "$stage_dir")"
archive_sha=$(shasum -a 256 "$archive" | awk '{print $1}')
printf '%s  %s\n' "$archive_sha" "$(basename "$archive")" > "$archive.sha256"

make -C "$agent_root" update-manifest \
  DIST_DIR="$dist_dir" \
  TIMICH_AGENT_VERSION=0.4.0 \
  TIMICH_COMMIT=test-commit \
  TIMICH_BUILT_AT=2026-07-23T00:00:00Z \
  AGENT_UPDATE_CHANNEL=stable \
  AGENT_DIST_TAG=v0.4.0 \
  AGENT_DIST_BASE_URL=https://github.com/rsahara/timich-agent/releases/download/v0.4.0 \
  >/dev/null

manifest="$dist_dir/agent-update-manifest.json"
jq -e '
  .channel == "stable" and
  .releaseTag == "v0.4.0" and
  (.artifacts | keys) == ["linux-amd64"] and
  (.artifacts["linux-amd64"].url | startswith("https://github.com/rsahara/timich-agent/releases/download/v0.4.0/")) and
  (.updateGuide.dockerCompose | join(" ") | test("compose.local-media.yaml")) and
  (.updateGuide.dockerCompose | join(" ") | test("exact same -f file list")) and
  (.updateGuide.manualBinary | join(" ") | test("complete new archive")) and
  (.updateGuide.manualBinary | join(" ") | test("both helpers")) and
  (.updateGuide.manualBinary | join(" ") | test("default relative .local")) and
  (.updateGuide.manualBinary | join(" ") | test("absolute -config and -data-dir")) and
  (.updateGuide.manualBinary | join(" ") | test("Replace the timich-agent binary") | not)
' "$manifest" >/dev/null

if make -C "$agent_root" update-manifest \
  DIST_DIR="$dist_dir" \
  TIMICH_AGENT_VERSION=0.4.0 \
  TIMICH_COMMIT=test-commit \
  TIMICH_BUILT_AT=2026-07-23T00:00:00Z \
  AGENT_UPDATE_CHANNEL=prerelease \
  AGENT_DIST_TAG=v0.4.0-rc.2 \
  AGENT_DIST_BASE_URL=https://github.com/rsahara/timich-agent/releases/download/v0.4.0-rc.2 \
  >/dev/null 2>&1; then
  echo "expected BUILDINFO releaseTag mismatch to fail manifest generation" >&2
  exit 1
fi

echo "update manifest tests passed"
