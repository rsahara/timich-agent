#!/usr/bin/env bash
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
agent_dir=$(CDPATH= cd -- "$script_dir/../.." && pwd)
cd "$agent_dir"

version=${TIMICH_AGENT_VERSION:-0.4.0}
repo=${AGENT_DIST_REPO:-${TIMICH_AGENT_DIST_REPO:-rsahara/timich-agent}}
tag=${AGENT_DIST_TAG:-v${version}-rc.1}
staging_tag=${tag}-semantic-stage
public_source_sha=${PUBLIC_SOURCE_SHA:-}
dist_dir=${DIST_DIR:-dist/prerelease-${tag}}
semantic_model_pack_dir=${SEMANTIC_MODEL_PACK_DIR:-dist/release-candidate/semantic-model-packs}
semantic_runtime_pack_dir=${SEMANTIC_RUNTIME_PACK_DIR:-dist/release-candidate/semantic-runtime-packs}
title=${AGENT_DIST_TITLE:-Timich Agent ${version} RC}

semantic_registry="$dist_dir/semantic-models.json"

require_file() {
  if [ ! -f "$1" ]; then
    echo "missing required artifact: $1" >&2
    exit 1
  fi
}

require_file "$semantic_registry"

if [[ ! "$public_source_sha" =~ ^[0-9a-f]{40}$ ]]; then
  echo "PUBLIC_SOURCE_SHA must be the full lowercase commit SHA of the matching public Timich Agent source" >&2
  exit 2
fi

command -v gh >/dev/null 2>&1 || { echo "gh is required to upload prerelease assets" >&2; exit 127; }

remote_assets_json='[]'
if release_json=$(gh release view "$staging_tag" --repo "$repo" --json isDraft,targetCommitish,assets 2>/dev/null); then
  if [ "$(jq -r '.isDraft' <<<"$release_json")" != "true" ]; then
    echo "semantic staging release $repo $staging_tag is already published and cannot be modified" >&2
    exit 2
  fi
  release_target=$(jq -r '.targetCommitish // ""' <<<"$release_json")
  if [ "$release_target" != "$public_source_sha" ]; then
    echo "semantic staging release $repo $staging_tag targets $release_target instead of $public_source_sha" >&2
    exit 2
  fi
  remote_assets_json=$(jq -c '.assets // []' <<<"$release_json")
  echo "Using existing semantic staging release $repo $staging_tag"
else
  echo "Creating semantic staging release $repo $staging_tag"
  gh release create "$staging_tag" \
    --repo "$repo" \
    --target "$public_source_sha" \
    --title "$title semantic staging" \
    --notes "Staging assets for protected publication of $tag. This draft is never published directly." \
    --draft \
    --prerelease \
    --latest=false
fi

small_assets=("$semantic_registry")
while IFS= read -r path; do
  small_assets+=("$path")
done < <(
  for dir in "$semantic_model_pack_dir" "$semantic_runtime_pack_dir"; do
    if [ -d "$dir" ]; then
      find "$dir" -maxdepth 1 -type f \( \
        -name '*.zip.sha256' -o \
        -name '*.metadata.json' -o \
        -name '*.spdx.json' -o \
        -name '*.sig' \
      \)
    fi
  done | sort
)
runtime_zips=("")
if [ -d "$semantic_runtime_pack_dir" ]; then
  while IFS= read -r path; do
    runtime_zips+=("$path")
  done < <(find "$semantic_runtime_pack_dir" -maxdepth 1 -type f -name '*.zip' | sort)
fi

model_zips=("")
if [ -d "$semantic_model_pack_dir" ]; then
  while IFS= read -r path; do
    model_zips+=("$path")
  done < <(find "$semantic_model_pack_dir" -maxdepth 1 -type f -name '*.zip' | sort)
fi

upload_assets=("${small_assets[@]}")
for path in "${runtime_zips[@]}" "${model_zips[@]}"; do
  [ -n "$path" ] || continue
  upload_assets+=("$path")
done

has_local_semantic_asset() {
  local expected_name=$1
  local path
  for path in "${upload_assets[@]}"; do
    if [ "$(basename "$path")" = "$expected_name" ]; then
      return 0
    fi
  done
  return 1
}

# The staging draft is an exact snapshot of the current local semantic assets.
# Remove renamed or deleted packs before uploading the replacement set.
while IFS= read -r remote_name; do
  if ! has_local_semantic_asset "$remote_name"; then
    gh release delete-asset "$staging_tag" "$remote_name" \
      --repo "$repo" \
      --yes
  fi
done < <(jq -r '.[].name' <<<"$remote_assets_json")

echo "Uploading semantic staging sidecars (${#small_assets[@]})"
gh release upload "$staging_tag" --repo "$repo" --clobber "${small_assets[@]}"

for path in "${runtime_zips[@]}"; do
  [ -n "$path" ] || continue
  echo "Uploading runtime pack: $path"
  gh release upload "$staging_tag" --repo "$repo" --clobber "$path"
done

for path in "${model_zips[@]}"; do
  [ -n "$path" ] || continue
  echo "Uploading model pack: $path"
  gh release upload "$staging_tag" --repo "$repo" --clobber "$path"
done

echo "Uploaded assets to semantic staging draft $repo $staging_tag"
echo "The protected Release Timich Agent Bundle workflow owns and publishes the final $tag draft."
