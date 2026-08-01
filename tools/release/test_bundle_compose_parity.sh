#!/usr/bin/env bash
set -euo pipefail

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
agent_root=$(CDPATH= cd -- "$script_dir/../.." && pwd)
source_compose="$agent_root/compose.yaml"
local_media_compose="$agent_root/compose.local-media.example.yaml"
public_readme="$agent_root/README.md"
bundle_dockerfile="$agent_root/docker/release-bundle.Dockerfile"
renderer="$script_dir/render_bundle_compose.sh"
temporary_dir=$(mktemp -d)
trap 'rm -rf "$temporary_dir"' EXIT
bundle_compose="$temporary_dir/compose.yaml"

bash "$renderer" "$source_compose" "$bundle_compose" 0.4.0

grep -Fxq '      context: .' "$bundle_compose"
grep -Fxq '      dockerfile: Dockerfile' "$bundle_compose"
grep -Fxq '    image: timich-agent:0.4.0' "$bundle_compose"
grep -Fq 'source: "${TIMICH_AGENT_LOCAL_MEDIA_HOST_PATH:?' "$local_media_compose"
grep -Fxq '        target: /media/photos' "$local_media_compose"
grep -Fxq '        read_only: true' "$local_media_compose"
grep -Fq 'timich-agent_VERSION_linux_amd64.tar.gz' "$public_readme"
if grep -Fq 'timich-agent_VERSION_linux_arm64.tar.gz' "$public_readme"; then
  echo "public README must not advertise an unpublished linux arm64 bundle" >&2
  exit 1
fi
if grep -Fq 'replace the `timich-agent` binary' "$public_readme"; then
  echo "public README must update the complete native bundle" >&2
  exit 1
fi
grep -Fq 'compose_args+=(-f compose.local-media.yaml)' "$public_readme"
grep -Fq 'docker compose "${compose_args[@]}" down' "$public_readme"
grep -Fq 'docker compose "${compose_args[@]}" up -d --build' "$public_readme"
grep -Fq 'docker compose "${compose_args[@]}" logs -f' "$public_readme"
grep -Fq 'copy that complete' "$public_readme"
grep -Fq -- '-config /var/lib/timich-agent/agent.json' "$public_readme"
grep -Fq -- '-data-dir /var/lib/timich-agent/state' "$public_readme"

sed -n 's/^      \(TIMICH_AGENT_[A-Z0-9_]*\):.*/\1/p' "$source_compose" | sort -u > "$temporary_dir/source-environment"
sed -n 's/^      \(TIMICH_AGENT_[A-Z0-9_]*\):.*/\1/p' "$bundle_compose" | sort -u > "$temporary_dir/bundle-environment"
diff -u "$temporary_dir/source-environment" "$temporary_dir/bundle-environment"

for variable in \
  TIMICH_AGENT_SEMANTIC_MODEL_MANIFEST_URL \
  TIMICH_AGENT_SEMANTIC_RUNTIME_HELPER \
  TIMICH_AGENT_SEMANTIC_ONNX_DISABLED \
  TIMICH_AGENT_SEMANTIC_ONNX_SERVER_PATH \
  TIMICH_AGENT_SEMANTIC_ONNX_PYTHON \
  TIMICH_AGENT_SEMANTIC_ONNX_HOST \
  TIMICH_AGENT_SEMANTIC_ONNX_PORT \
  TIMICH_AGENT_SEMANTIC_ONNX_PROVIDER \
  TIMICH_AGENT_SEMANTIC_ONNX_TEXT_PROVIDER \
  TIMICH_AGENT_SEMANTIC_ONNX_IMAGE_PROVIDER \
  TIMICH_AGENT_SEMANTIC_ONNX_TEXT_TEMPLATE
do
  grep -Fxq "$variable" "$temporary_dir/bundle-environment"
done

if bash "$renderer" "$source_compose" "$temporary_dir/invalid.yaml" 0.4.0-rc.1 >/dev/null 2>&1; then
  echo "expected non-MAJOR.MINOR.PATCH Agent version to fail" >&2
  exit 1
fi

release_tag=v0.4.0-rc.9
release_base_url="https://github.com/rsahara/timich-agent/releases/download/$release_tag"
make -n -C "$agent_root" dist \
  DIST_OS=linux \
  DIST_ARCH=amd64 \
  TIMICH_AGENT_VERSION=0.4.0 \
  AGENT_DIST_TAG="$release_tag" \
  AGENT_DIST_BASE_URL="$release_base_url" \
  > "$temporary_dir/dist-dry-run"
grep -Fq \
  "\"# TIMICH_AGENT_SEMANTIC_MODEL_MANIFEST_URL=$release_base_url/semantic-models.json\"" \
  "$temporary_dir/dist-dry-run"
grep -Fq \
  'cp compose.local-media.example.yaml' \
  "$temporary_dir/dist-dry-run"
grep -Fq \
  'cp docker/release-bundle.Dockerfile' \
  "$temporary_dir/dist-dry-run"
grep -Fxq 'FROM debian:bookworm-slim' "$bundle_dockerfile"
grep -Fq 'libvips-tools' "$bundle_dockerfile"
grep -Fq \
  '"# TIMICH_AGENT_LOCAL_MEDIA_HOST_PATH=/share/Photos"' \
  "$temporary_dir/dist-dry-run"
grep -Fq \
  '"Compose updates must use the exact same -f file list for down, up, and logs."' \
  "$temporary_dir/dist-dry-run"
grep -Fq \
  '"state_root=/var/lib/timich-agent"' \
  "$temporary_dir/dist-dry-run"
grep -Fq -- \
  "-X main.releaseTag=$release_tag" \
  "$temporary_dir/dist-dry-run"

environment_manifest_url=https://example.invalid/environment-semantic-models.json
TIMICH_AGENT_SEMANTIC_MODEL_MANIFEST_URL="$environment_manifest_url" \
  make -n -C "$agent_root" build > "$temporary_dir/environment-build-dry-run"
grep -Fq -- \
  "-X main.semanticModelManifestURL=$environment_manifest_url" \
  "$temporary_dir/environment-build-dry-run"

environment_ldflags='-X main.version=environment-override'
TIMICH_AGENT_LDFLAGS="$environment_ldflags" \
  make -n -C "$agent_root" build > "$temporary_dir/environment-ldflags-dry-run"
grep -Fq -- \
  "-ldflags \"$environment_ldflags\"" \
  "$temporary_dir/environment-ldflags-dry-run"

make -n -C "$agent_root" build \
  AGENT_UPDATE_CHANNEL=prerelease \
  > "$temporary_dir/prerelease-build-dry-run"
grep -Fq -- \
  '-X main.updateManifestURL=https://api.github.com/repos/rsahara/timich-agent/releases?per_page=100&timich_channel=prerelease' \
  "$temporary_dir/prerelease-build-dry-run"

echo "bundle Compose parity tests passed"
