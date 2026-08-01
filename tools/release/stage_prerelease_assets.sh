#!/usr/bin/env bash
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
agent_dir=$(CDPATH= cd -- "$script_dir/../.." && pwd)
cd "$agent_dir"

version=${TIMICH_AGENT_VERSION:-0.4.0}
repo=${AGENT_DIST_REPO:-${TIMICH_AGENT_DIST_REPO:-rsahara/timich-agent}}
tag=${AGENT_DIST_TAG:-v${version}-rc.1}
dist_dir=${DIST_DIR:-dist/prerelease-${tag}}
base_url=${AGENT_DIST_BASE_URL:-https://github.com/${repo}/releases/download/${tag}}
semantic_model_pack_dir=${SEMANTIC_MODEL_PACK_DIR:-dist/release-candidate/semantic-model-packs}
semantic_runtime_pack_dir=${SEMANTIC_RUNTIME_PACK_DIR:-dist/release-candidate/semantic-runtime-packs}
recommended_model=${SEMANTIC_MODEL_REGISTRY_RECOMMENDED:-timich-siglip2-base-patch16-224-onnx-multilingual-v1}
recommended_runtime=${SEMANTIC_MODEL_REGISTRY_RECOMMENDED_RUNTIME_PACK:-timich-siglip2-onnxruntime-runtime}
registry_version=${SEMANTIC_MODEL_REGISTRY_VERSION:-${tag#v}}

echo "Staging Timich Agent semantic prerelease artifacts"
echo "  version: $version"
echo "  tag:     $tag"
echo "  dist:    $dist_dir"
echo "  repo:    $repo"
echo "  base:    $base_url"

mkdir -p "$dist_dir"

make semantic-model-registry \
  DIST_DIR="$dist_dir" \
  SEMANTIC_MODEL_REGISTRY="$dist_dir/semantic-models.json" \
  SEMANTIC_MODEL_PACK_DIR="$semantic_model_pack_dir" \
  SEMANTIC_RUNTIME_PACK_DIR="$semantic_runtime_pack_dir" \
  SEMANTIC_MODEL_REGISTRY_VERSION="$registry_version" \
  SEMANTIC_MODEL_REGISTRY_BASE_URL="$base_url" \
  SEMANTIC_MODEL_REGISTRY_RECOMMENDED="$recommended_model" \
  SEMANTIC_MODEL_REGISTRY_RECOMMENDED_RUNTIME_PACK="$recommended_runtime"

make semantic-release-validate \
  DIST_DIR="$dist_dir" \
  SEMANTIC_MODEL_REGISTRY="$dist_dir/semantic-models.json" \
  SEMANTIC_MODEL_PACK_DIR="$semantic_model_pack_dir" \
  SEMANTIC_RUNTIME_PACK_DIR="$semantic_runtime_pack_dir" \
  AGENT_DIST_BASE_URL="$base_url" \
  SEMANTIC_RELEASE_REQUIRE_SIGNATURES="${SEMANTIC_RELEASE_REQUIRE_SIGNATURES:-0}"

echo "Semantic prerelease artifacts staged:"
find "$dist_dir" -maxdepth 2 -type f | sort
