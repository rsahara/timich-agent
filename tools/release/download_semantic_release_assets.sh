#!/usr/bin/env bash
set -euo pipefail

target_repository=${1:-}
staging_tag=${2:-}
public_source_sha=${3:-}
release_base_url=${4:-}
output_dir=${5:-}
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
agent_dir=$(CDPATH= cd -- "$script_dir/../.." && pwd)

fail() {
  echo "semantic release asset download failed: $*" >&2
  exit 2
}

if [ -z "${GH_TOKEN:-}" ]; then
  fail "GH_TOKEN is required"
fi
if [[ ! "$target_repository" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]]; then
  fail "target repository is invalid"
fi
if [[ ! "$staging_tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[1-9][0-9]*)?-semantic-stage$ ]]; then
  fail "semantic staging tag is invalid"
fi
if [[ ! "$public_source_sha" =~ ^[0-9a-f]{40}$ ]]; then
  fail "public source SHA must be a full lowercase commit SHA"
fi
if [[ ! "$release_base_url" =~ ^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/releases/download/v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[1-9][0-9]*)?$ ]]; then
  fail "release base URL is invalid"
fi
if [ -z "$output_dir" ]; then
  fail "output directory is required"
fi
mkdir -p "$output_dir"
if find "$output_dir" -mindepth 1 -maxdepth 1 -print -quit | grep -q .; then
  fail "output directory must be empty"
fi

release_json=$(gh release view "$staging_tag" \
  --repo "$target_repository" \
  --json isDraft,targetCommitish,assets 2>/dev/null) || fail "semantic staging release $staging_tag is required"
if [ "$(jq -r '.isDraft' <<<"$release_json")" != "true" ]; then
  fail "semantic staging release $staging_tag must remain a draft"
fi
if [ "$(jq -r '.targetCommitish // ""' <<<"$release_json")" != "$public_source_sha" ]; then
  fail "semantic staging release $staging_tag targets the wrong public source"
fi
assets_json=$(jq -c '.assets' <<<"$release_json")

max_staging_asset_count=64
max_staging_asset_size_bytes=$((8 * 1024 * 1024 * 1024))
# Retain at least 4 GiB of the shared 10 GiB release working-set budget for
# validation/smoke extraction. Central-directory preflight below applies the
# exact retained-download plus extraction bound before any archive is unpacked.
max_staging_total_size_bytes=$((6 * 1024 * 1024 * 1024))
max_registry_size_bytes=$((1024 * 1024))
max_sidecar_size_bytes=$((16 * 1024 * 1024))
semantic_names=()
found_registry=false
asset_count=$(jq -r 'length' <<<"$assets_json")
if [[ ! "$asset_count" =~ ^[0-9]+$ ]] || [ "$asset_count" -le 0 ] || [ "$asset_count" -gt "$max_staging_asset_count" ]; then
  fail "semantic staging release must contain between 1 and $max_staging_asset_count assets"
fi
duplicate_remote_name=$(jq -r \
  '[sort_by(.name) | group_by(.name)[] | select(length > 1) | .[0].name] | first // empty' \
  <<<"$assets_json")
if [ -n "$duplicate_remote_name" ]; then
  fail "semantic release asset name is duplicated: $duplicate_remote_name"
fi
total_remote_size=0
while IFS=$'\t' read -r remote_name remote_size remote_digest; do
  if [[ ! "$remote_name" =~ ^[A-Za-z0-9._+-]+$ ]]; then
    fail "semantic release asset name is unsafe: $remote_name"
  fi
  if [[ ! "$remote_size" =~ ^[0-9]+$ ]] || [ "$remote_size" -le 0 ] || [ "$remote_size" -gt "$max_staging_asset_size_bytes" ]; then
    fail "semantic release asset $remote_name has an invalid declared size"
  fi
  if [[ ! "$remote_digest" =~ ^sha256:[0-9a-f]{64}$ ]]; then
    fail "semantic release asset $remote_name has an invalid digest"
  fi
  total_remote_size=$((total_remote_size + remote_size))
  if [ "$total_remote_size" -gt "$max_staging_total_size_bytes" ]; then
    fail "semantic staging release exceeds the $max_staging_total_size_bytes-byte download budget"
  fi
  semantic_names+=("$remote_name")
  if [ "$remote_name" = "semantic-models.json" ]; then
    found_registry=true
  fi
done < <(jq -r '.[] | [.name, .size, (.digest // "")] | @tsv' <<<"$assets_json")
if [ "$found_registry" != "true" ]; then
  fail "semantic-models.json is required"
fi

remote_asset_count() {
  jq -r --arg name "$1" '[.[] | select(.name == $name)] | length' <<<"$assets_json"
}

remote_asset_size() {
  jq -r --arg name "$1" '.[] | select(.name == $name) | .size' <<<"$assets_json"
}

download_and_verify() {
  local remote_name=$1
  local remote_size
  local remote_digest
  local file_limit_blocks
  local local_path="$output_dir/$remote_name"
  local local_size
  local local_sha256
  remote_size=$(remote_asset_size "$remote_name")
  remote_digest=$(jq -r --arg name "$remote_name" '.[] | select(.name == $name) | .digest' <<<"$assets_json")
  # GNU Bash reports and accepts ulimit -f in 1024-byte blocks outside POSIX
  # mode, which is how this script runs on the protected Ubuntu runner.
  file_limit_blocks=$(((remote_size + 1023) / 1024))
  (
    ulimit -f "$file_limit_blocks"
    gh release download "$staging_tag" \
      --repo "$target_repository" \
      --pattern "$remote_name" \
      --dir "$output_dir"
  )
  [ -f "$local_path" ] || fail "semantic release asset $remote_name was not downloaded"
  local_size=$(wc -c < "$local_path" | tr -d '[:space:]')
  local_sha256=$(sha256sum "$local_path" | awk '{print $1}')
  if [ "$remote_size" != "$local_size" ] || [ "$remote_digest" != "sha256:${local_sha256}" ]; then
    fail "semantic release asset $remote_name failed remote size/digest verification"
  fi
}

registry_size=$(remote_asset_size semantic-models.json)
if [ "$registry_size" -gt "$max_registry_size_bytes" ]; then
  fail "semantic-models.json exceeds the $max_registry_size_bytes-byte registry limit"
fi
download_and_verify semantic-models.json

(
  cd "$agent_dir"
  go run ./cmd/timich-semantic-helper validate-manifest \
    --manifest "$output_dir/semantic-models.json" \
    --required-platform linux-amd64 \
    --require-recommended-model \
    --require-recommended-runtime-pack
)

artifact_records=$(jq -c '
  [(.models // [])[] as $owner |
    ($owner.artifacts // {} | to_entries[]) as $artifact |
    {ownerKey: ("model:" + ($owner.id // "")), ownerId: ($owner.id // ""), filename: ($artifact.value.filename // ""), sizeBytes: ($artifact.value.sizeBytes // 0)}] +
  [(.runtimePacks // [])[] as $owner |
    ($owner.artifacts // {} | to_entries[]) as $artifact |
    {ownerKey: ("runtime:" + ($owner.id // "")), ownerId: ($owner.id // ""), filename: ($artifact.value.filename // ""), sizeBytes: ($artifact.value.sizeBytes // 0)}]
' "$output_dir/semantic-models.json")
if [ "$(jq -r 'length' <<<"$artifact_records")" -le 0 ]; then
  fail "semantic-models.json has no artifacts"
fi
repeated_filename=$(jq -r '
  [sort_by(.filename) | group_by(.filename)[] |
    select(length > 1) | .[0].filename] | first // empty
' <<<"$artifact_records")
if [ -n "$repeated_filename" ]; then
  fail "semantic artifact filename is referenced more than once: $repeated_filename"
fi

referenced_names=(semantic-models.json)

mark_referenced() {
  referenced_names+=("$1")
}

is_referenced() {
  local expected_name
  for expected_name in "${referenced_names[@]}"; do
    if [ "$expected_name" = "$1" ]; then
      return 0
    fi
  done
  return 1
}

require_remote_asset() {
  local remote_name=$1
  local kind=$2
  local matches
  local size
  matches=$(remote_asset_count "$remote_name")
  if [ "$matches" != "1" ]; then
    fail "semantic release requires exactly one $remote_name asset"
  fi
  size=$(remote_asset_size "$remote_name")
  if [ "$kind" = "sidecar" ] && [ "$size" -gt "$max_sidecar_size_bytes" ]; then
    fail "semantic sidecar $remote_name exceeds the $max_sidecar_size_bytes-byte limit"
  fi
  mark_referenced "$remote_name"
}

while IFS=$'\t' read -r owner_id filename declared_size; do
  if [[ ! "$owner_id" =~ ^[A-Za-z0-9._-]+$ ]] || [[ ! "$filename" =~ ^[A-Za-z0-9._+-]+$ ]]; then
    fail "semantic registry contains an unsafe owner or artifact filename"
  fi
  require_remote_asset "$filename" artifact
  if [ "$(remote_asset_size "$filename")" != "$declared_size" ]; then
    fail "semantic artifact $filename size does not match semantic-models.json"
  fi
  require_remote_asset "$filename.sha256" sidecar
  stem=${filename%.*}
  require_remote_asset "$stem.metadata.json" sidecar
  stem_sbom="$stem.spdx.json"
  owner_sbom="$owner_id.spdx.json"
  stem_sbom_count=$(remote_asset_count "$stem_sbom")
  owner_sbom_count=$(remote_asset_count "$owner_sbom")
  if [ "$stem_sbom" = "$owner_sbom" ]; then
    require_remote_asset "$stem_sbom" sidecar
  elif [ "$stem_sbom_count" = "1" ] && [ "$owner_sbom_count" = "0" ]; then
    require_remote_asset "$stem_sbom" sidecar
  elif [ "$stem_sbom_count" = "0" ] && [ "$owner_sbom_count" = "1" ]; then
    require_remote_asset "$owner_sbom" sidecar
  else
    fail "semantic artifact $filename must have exactly one recognized SBOM sidecar"
  fi
  if [ "$(remote_asset_count "$filename.sig")" = "1" ]; then
    require_remote_asset "$filename.sig" sidecar
  fi
done < <(jq -r '.[] | [.ownerId, .filename, (.sizeBytes | tostring)] | @tsv' <<<"$artifact_records")

for remote_name in "${semantic_names[@]}"; do
  if ! is_referenced "$remote_name"; then
    fail "semantic staging release contains an unreferenced asset: $remote_name"
  fi
done
for remote_name in "${semantic_names[@]}"; do
  if [ "$remote_name" != "semantic-models.json" ]; then
    download_and_verify "$remote_name"
  fi
done

SEMANTIC_MODEL_REGISTRY="$output_dir/semantic-models.json" \
SEMANTIC_MODEL_PACK_DIR="$output_dir" \
SEMANTIC_RUNTIME_PACK_DIR="$output_dir" \
SEMANTIC_RELEASE_BASE_URL="$release_base_url" \
python3 "$agent_dir/tools/semantic/validate_semantic_release.py" \
  --validate-pack-layouts \
  --reject-unreferenced-assets

jq -n \
  --arg stagingTag "$staging_tag" \
  --arg targetCommitish "$public_source_sha" \
  --argjson assets "$(jq -c 'sort_by(.name) | map({name, size, digest})' <<<"$assets_json")" \
  '{schemaVersion: 1, stagingTag: $stagingTag, targetCommitish: $targetCommitish, assets: $assets}' \
  > "$output_dir/semantic-asset-snapshot.json"
