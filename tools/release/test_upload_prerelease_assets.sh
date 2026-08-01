#!/usr/bin/env bash
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
uploader="$script_dir/upload_prerelease_assets.sh"
test_root=$(mktemp -d)
trap 'rm -rf "$test_root"' EXIT

fake_bin="$test_root/bin"
dist_dir="$test_root/dist"
mkdir -p "$fake_bin" "$dist_dir"
printf 'bundle\n' > "$dist_dir/timich-agent_0.4.0_linux_amd64.tar.gz"
printf 'checksum\n' > "$dist_dir/timich-agent_0.4.0_linux_amd64.tar.gz.sha256"
printf '{}\n' > "$dist_dir/agent-update-manifest.json"
printf '{}\n' > "$dist_dir/semantic-models.json"

cat > "$fake_bin/gh" <<'FAKE_GH'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$FAKE_GH_LOG"
if [ "${1:-}" = "release" ] && [ "${2:-}" = "view" ]; then
  case "$FAKE_GH_RELEASE_STATE" in
    missing) exit 1 ;;
    published) jq -cn --arg target "$FAKE_GH_TARGET" --argjson assets "${FAKE_GH_ASSETS:-[]}" '{isDraft:false,targetCommitish:$target,assets:$assets}' ;;
    draft) jq -cn --arg target "$FAKE_GH_TARGET" --argjson assets "${FAKE_GH_ASSETS:-[]}" '{isDraft:true,targetCommitish:$target,assets:$assets}' ;;
    *) exit 1 ;;
  esac
  exit 0
fi
if [ "${1:-}" = "release" ] && { [ "${2:-}" = "create" ] || [ "${2:-}" = "upload" ]; }; then
  exit 0
fi
if [ "${1:-}" = "release" ] && [ "${2:-}" = "delete-asset" ]; then
  exit 0
fi
exit 1
FAKE_GH
chmod +x "$fake_bin/gh"

log_file="$test_root/gh.log"
public_sha=1111111111111111111111111111111111111111
if PATH="$fake_bin:$PATH" \
  FAKE_GH_LOG="$log_file" \
  FAKE_GH_RELEASE_STATE=published \
  FAKE_GH_TARGET="$public_sha" \
  PUBLIC_SOURCE_SHA="$public_sha" \
  TIMICH_AGENT_VERSION=0.4.0 \
  AGENT_DIST_TAG=v0.4.0-rc.2 \
  AGENT_DIST_STAGING_TAG=v0.4.0-rc.2 \
  DIST_OS=linux \
  DIST_ARCH=amd64 \
  DIST_DIR="$dist_dir" \
  SEMANTIC_MODEL_PACK_DIR="$test_root/model-packs" \
  SEMANTIC_RUNTIME_PACK_DIR="$test_root/runtime-packs" \
  MEDIA_RUNTIME_PACK_DIR="$test_root/media-packs" \
  bash "$uploader" >/dev/null 2>&1; then
  echo "expected local prerelease uploader to reject a published release" >&2
  exit 1
fi

if [ "$(wc -l < "$log_file" | tr -d '[:space:]')" != "1" ] || ! grep -q '^release view ' "$log_file"; then
  echo "published release rejection performed a GitHub mutation" >&2
  cat "$log_file" >&2
  exit 1
fi

: > "$log_file"
if PATH="$fake_bin:$PATH" \
  FAKE_GH_LOG="$log_file" \
  FAKE_GH_RELEASE_STATE=missing \
  FAKE_GH_TARGET="" \
  TIMICH_AGENT_VERSION=0.4.0 \
  AGENT_DIST_TAG=v0.4.0-rc.2 \
  DIST_OS=linux \
  DIST_ARCH=amd64 \
  DIST_DIR="$dist_dir" \
  SEMANTIC_MODEL_PACK_DIR="$test_root/model-packs" \
  SEMANTIC_RUNTIME_PACK_DIR="$test_root/runtime-packs" \
  MEDIA_RUNTIME_PACK_DIR="$test_root/media-packs" \
  bash "$uploader" >/dev/null 2>&1; then
  echo "expected local prerelease uploader to require PUBLIC_SOURCE_SHA" >&2
  exit 1
fi
if [ -s "$log_file" ]; then
  echo "missing public source SHA reached GitHub unexpectedly" >&2
  exit 1
fi

: > "$log_file"
PATH="$fake_bin:$PATH" \
  FAKE_GH_LOG="$log_file" \
  FAKE_GH_RELEASE_STATE=missing \
  FAKE_GH_TARGET="" \
  PUBLIC_SOURCE_SHA="$public_sha" \
  TIMICH_AGENT_VERSION=0.4.0 \
  AGENT_DIST_TAG=v0.4.0-rc.2 \
  DIST_OS=linux \
  DIST_ARCH=amd64 \
  DIST_DIR="$dist_dir" \
  SEMANTIC_MODEL_PACK_DIR="$test_root/model-packs" \
  SEMANTIC_RUNTIME_PACK_DIR="$test_root/runtime-packs" \
  MEDIA_RUNTIME_PACK_DIR="$test_root/media-packs" \
  bash "$uploader" >/dev/null
grep -q -- "^release create v0.4.0-rc.2-semantic-stage .*--target $public_sha" "$log_file"
grep -q '^release upload v0.4.0-rc.2-semantic-stage ' "$log_file"
if grep -Eq 'timich-agent_.*\.tar\.gz|agent-update-manifest|media-runtime' "$log_file"; then
  echo "semantic staging uploader included protected bundle or media assets" >&2
  exit 1
fi
if grep -Eq '^release (create|upload) v0.4.0-rc.2 ' "$log_file"; then
  echo "local uploader mutated the protected final release tag" >&2
  exit 1
fi

: > "$log_file"
PATH="$fake_bin:$PATH" \
  FAKE_GH_LOG="$log_file" \
  FAKE_GH_RELEASE_STATE=draft \
  FAKE_GH_TARGET="$public_sha" \
  FAKE_GH_ASSETS='[{"name":"obsolete-model.zip","size":1,"digest":"sha256:old"}]' \
  PUBLIC_SOURCE_SHA="$public_sha" \
  TIMICH_AGENT_VERSION=0.4.0 \
  AGENT_DIST_TAG=v0.4.0-rc.2 \
  DIST_OS=linux \
  DIST_ARCH=amd64 \
  DIST_DIR="$dist_dir" \
  SEMANTIC_MODEL_PACK_DIR="$test_root/model-packs" \
  SEMANTIC_RUNTIME_PACK_DIR="$test_root/runtime-packs" \
  MEDIA_RUNTIME_PACK_DIR="$test_root/media-packs" \
  bash "$uploader" >/dev/null
grep -q '^release delete-asset v0.4.0-rc.2-semantic-stage obsolete-model.zip ' "$log_file"

: > "$log_file"
if PATH="$fake_bin:$PATH" \
  FAKE_GH_LOG="$log_file" \
  FAKE_GH_RELEASE_STATE=draft \
  FAKE_GH_TARGET=2222222222222222222222222222222222222222 \
  PUBLIC_SOURCE_SHA="$public_sha" \
  TIMICH_AGENT_VERSION=0.4.0 \
  AGENT_DIST_TAG=v0.4.0-rc.2 \
  DIST_OS=linux \
  DIST_ARCH=amd64 \
  DIST_DIR="$dist_dir" \
  SEMANTIC_MODEL_PACK_DIR="$test_root/model-packs" \
  SEMANTIC_RUNTIME_PACK_DIR="$test_root/runtime-packs" \
  MEDIA_RUNTIME_PACK_DIR="$test_root/media-packs" \
  bash "$uploader" >/dev/null 2>&1; then
  echo "expected local prerelease uploader to reject a mismatched draft target" >&2
  exit 1
fi
if grep -Eq '^release (create|upload) ' "$log_file"; then
  echo "mismatched draft target caused a GitHub mutation" >&2
  exit 1
fi
