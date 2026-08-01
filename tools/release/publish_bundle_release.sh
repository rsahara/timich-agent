#!/usr/bin/env bash
set -euo pipefail

target_repository=${1:-}
release_tag=${2:-}
public_source_sha=${3:-}
public_source_dir=${4:-}
title=${5:-}
notes_file=${6:-}
assets_dir=${7:-}
publish_release=${8:-}
prerelease=${9:-}
mark_latest=${10:-}
include_semantic_assets=${11:-}
semantic_smoke_attestation=${12:-}
semantic_staging_tag="${release_tag}-semantic-stage"
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
agent_dir=$(CDPATH= cd -- "$script_dir/../.." && pwd)
semantic_verify_dir=""
semantic_asset_paths=("")

cleanup() {
  if [ -n "$semantic_verify_dir" ] && [ -d "$semantic_verify_dir" ]; then
    rm -rf "$semantic_verify_dir"
  fi
}
trap cleanup EXIT

fail() {
  echo "bundle release publication failed: $*" >&2
  exit 2
}

if [ -z "${GH_TOKEN:-}" ]; then
  fail "GH_TOKEN is required"
fi
if [[ ! "$target_repository" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]]; then
  fail "target repository is invalid"
fi
if [[ ! "$release_tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[1-9][0-9]*)?$ ]]; then
  fail "release tag is invalid"
fi
if [[ ! "$public_source_sha" =~ ^[0-9a-f]{40}$ ]]; then
  fail "public source SHA must be a full lowercase commit SHA"
fi
if [ ! -d "$public_source_dir/.git" ]; then
  fail "public source checkout is missing"
fi
if [ -z "$title" ] || [ ! -f "$notes_file" ] || [ ! -d "$assets_dir" ]; then
  fail "release title, notes, and asset directory are required"
fi
for value_name in publish_release prerelease mark_latest include_semantic_assets; do
  value=${!value_name}
  case "$value" in
    true|false) ;;
    *) fail "$value_name must be true or false" ;;
  esac
done

download_and_verify_semantic_release_assets() {
  local base_url="https://github.com/${target_repository}/releases/download/${release_tag}"
  semantic_verify_dir=$(mktemp -d)
  bash "$script_dir/download_semantic_release_assets.sh" \
    "$target_repository" \
    "$semantic_staging_tag" \
    "$public_source_sha" \
    "$base_url" \
    "$semantic_verify_dir"
  if [ ! -f "$semantic_smoke_attestation" ]; then
    fail "secretless semantic smoke attestation is required"
  fi
  python3 "$agent_dir/tools/semantic/verify_semantic_smoke_attestation.py" \
    --assets-dir "$semantic_verify_dir" \
    --attestation "$semantic_smoke_attestation"
  while IFS= read -r semantic_asset; do
    semantic_asset_paths+=("$semantic_asset")
  done < <(find "$semantic_verify_dir" -maxdepth 1 -type f ! -name 'semantic-asset-snapshot.json' | sort)
}

upload_assets=()
while IFS= read -r asset; do
  upload_assets+=("$asset")
done < <(find "$assets_dir" -maxdepth 1 -type f | sort)
if [ "${#upload_assets[@]}" -eq 0 ]; then
  fail "no release assets were provided"
fi

is_managed_bundle_asset() {
  case "$1" in
    agent-update-manifest.json|\
    timich-agent_*.tar.gz|timich-agent_*.tar.gz.sha256|\
    timich-libvips-alpine-runtime_*.tar.gz|\
    timich-libvips-alpine-runtime_*.tar.gz.sha256|\
    timich-libvips-alpine-runtime_*.metadata.json|\
    timich-libvips-alpine-runtime_*.spdx.json|\
    timich-ffmpeg-lgpl-decode-runtime_*.tar.gz|\
    timich-ffmpeg-lgpl-decode-runtime_*.tar.gz.sha256|\
    timich-ffmpeg-lgpl-decode-runtime_*.metadata.json|\
    timich-ffmpeg-lgpl-decode-runtime_*.spdx.json)
      return 0
      ;;
    *)
      return 1
      ;;
  esac
}

has_local_asset() {
  local expected_name=$1
  local asset
  for asset in "${upload_assets[@]}"; do
    if [ "$(basename "$asset")" = "$expected_name" ]; then
      return 0
    fi
  done
  return 1
}

has_expected_release_asset() {
  local expected_name=$1
  local asset
  if has_local_asset "$expected_name"; then
    return 0
  fi
  for asset in "${semantic_asset_paths[@]}"; do
    if [ -n "$asset" ] && [ "$(basename "$asset")" = "$expected_name" ]; then
      return 0
    fi
  done
  return 1
}

if [ "$include_semantic_assets" = "true" ]; then
  download_and_verify_semantic_release_assets
fi

release_assets=("${upload_assets[@]}")
for asset in "${semantic_asset_paths[@]}"; do
  [ -n "$asset" ] || continue
  release_assets+=("$asset")
done

tag_target=$(git -C "$public_source_dir" rev-parse -q --verify "refs/tags/${release_tag}^{commit}" 2>/dev/null || true)
if [ -n "$tag_target" ] && [ "$tag_target" != "$public_source_sha" ]; then
  fail "existing tag $release_tag points to $tag_target instead of $public_source_sha"
fi

release_exists=false
release_json=""
if release_json=$(gh release view "$release_tag" \
  --repo "$target_repository" \
  --json isDraft,targetCommitish,url 2>/dev/null); then
  release_exists=true
  if [ "$(jq -r '.isDraft' <<<"$release_json")" != "true" ]; then
    fail "release $release_tag is already published and cannot be modified"
  fi
  release_target=$(jq -r '.targetCommitish // ""' <<<"$release_json")
  if [ "$release_target" != "$public_source_sha" ]; then
    fail "draft release $release_tag targets $release_target instead of $public_source_sha"
  fi
fi

latest_value=false
if [ "$mark_latest" = "true" ]; then
  latest_value=true
fi

if [ "$release_exists" = "false" ]; then
  create_args=(
    "$release_tag"
    --repo "$target_repository"
    --target "$public_source_sha"
    --title "$title"
    --notes-file "$notes_file"
    --draft
    --latest="$latest_value"
  )
  if [ "$prerelease" = "true" ]; then
    create_args+=(--prerelease)
  fi
  gh release create "${create_args[@]}"
else
  gh release edit "$release_tag" \
    --repo "$target_repository" \
    --title "$title" \
    --notes-file "$notes_file" \
    --prerelease="$prerelease" \
    --latest="$latest_value"
fi

if [ "$(gh release view "$release_tag" --repo "$target_repository" --json isDraft --jq '.isDraft')" != "true" ]; then
  fail "release $release_tag stopped being a draft before asset upload"
fi

# The protected workflow is the only writer for the final release draft. Remove
# every asset outside the verified local bundle plus semantic staging snapshot.
remote_assets=$(gh release view "$release_tag" --repo "$target_repository" --json assets --jq '.assets')
while IFS= read -r remote_name; do
  if ! has_expected_release_asset "$remote_name"; then
    gh release delete-asset "$release_tag" "$remote_name" \
      --repo "$target_repository" \
      --yes
  fi
done < <(jq -r '.[].name' <<<"$remote_assets")

gh release upload "$release_tag" \
  --repo "$target_repository" \
  --clobber \
  "${release_assets[@]}"

remote_assets=$(gh release view "$release_tag" --repo "$target_repository" --json assets --jq '.assets')
for asset in "${release_assets[@]}"; do
  name=$(basename "$asset")
  local_sha256=$(sha256sum "$asset" | awk '{print $1}')
  local_size=$(wc -c < "$asset" | tr -d '[:space:]')
  matches=$(jq --arg name "$name" '[.[] | select(.name == $name)] | length' <<<"$remote_assets")
  if [ "$matches" -ne 1 ]; then
    fail "release asset $name was not uploaded exactly once"
  fi
  remote_size=$(jq -r --arg name "$name" '.[] | select(.name == $name) | .size' <<<"$remote_assets")
  remote_digest=$(jq -r --arg name "$name" '.[] | select(.name == $name) | .digest // ""' <<<"$remote_assets")
  if [ "$remote_size" != "$local_size" ]; then
    fail "release asset $name size mismatch"
  fi
  if [ "$remote_digest" != "sha256:${local_sha256}" ]; then
    fail "release asset $name digest mismatch"
  fi
done

while IFS= read -r remote_name; do
  if ! has_expected_release_asset "$remote_name"; then
    fail "unexpected release asset $remote_name remains after reconciliation"
  fi
done < <(jq -r '.[].name' <<<"$remote_assets")

verified_asset_snapshot=$(jq -c 'sort_by(.name) | map({name, size, digest})' <<<"$remote_assets")

if [ "$publish_release" = "true" ]; then
  final_release_json=$(gh release view "$release_tag" \
    --repo "$target_repository" \
    --json isDraft,targetCommitish,assets)
  if [ "$(jq -r '.isDraft' <<<"$final_release_json")" != "true" ]; then
    fail "release $release_tag stopped being a draft before publication"
  fi
  if [ "$(jq -r '.targetCommitish // ""' <<<"$final_release_json")" != "$public_source_sha" ]; then
    fail "release $release_tag target changed before publication"
  fi
  final_asset_snapshot=$(jq -c '.assets | sort_by(.name) | map({name, size, digest})' <<<"$final_release_json")
  if [ "$final_asset_snapshot" != "$verified_asset_snapshot" ]; then
    fail "release asset snapshot changed after verification"
  fi
  gh release edit "$release_tag" \
    --repo "$target_repository" \
    --draft=false \
    --prerelease="$prerelease" \
    --latest="$latest_value"
fi
