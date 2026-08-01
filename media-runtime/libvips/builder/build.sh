#!/usr/bin/env sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
agent_root=$(CDPATH= cd -- "$script_dir/../../.." && pwd)

version=${TIMICH_MEDIA_LIBVIPS_VERSION:-8.16-alpine3.22}
platform=${TIMICH_MEDIA_LIBVIPS_PLATFORM:-linux_amd64}
docker_platform=${TIMICH_MEDIA_LIBVIPS_DOCKER_PLATFORM:-linux/amd64}
build_image=${TIMICH_MEDIA_LIBVIPS_BUILD_IMAGE:-alpine:3.22}
runtime_dir=${TIMICH_MEDIA_LIBVIPS_RUNTIME_DIR:-$agent_root/media-runtime/libvips/$platform}
pack_dir=${TIMICH_MEDIA_LIBVIPS_PACK_DIR:-$agent_root/dist/media-runtime-packs}
runtime_id=${TIMICH_MEDIA_LIBVIPS_ID:-timich-libvips-alpine-runtime}
apk_packages=${TIMICH_MEDIA_LIBVIPS_APK_PACKAGES:-}

if ! command -v docker >/dev/null 2>&1; then
  echo "docker is required to build the libvips runtime" >&2
  exit 1
fi

mkdir -p "$runtime_dir" "$pack_dir"
runtime_dir_abs=$(CDPATH= cd -- "$runtime_dir" && pwd)
pack_dir_abs=$(CDPATH= cd -- "$pack_dir" && pwd)
output_uid=$(id -u 2>/dev/null || true)
output_gid=$(id -g 2>/dev/null || true)

docker run --rm \
  --platform "$docker_platform" \
  -v "$script_dir:/builder:ro" \
  -v "$runtime_dir_abs:/out" \
  -e "TIMICH_LIBVIPS_PLATFORM=$platform" \
  -e "TIMICH_LIBVIPS_OUTPUT=/out" \
  -e "TIMICH_OUTPUT_UID=$output_uid" \
  -e "TIMICH_OUTPUT_GID=$output_gid" \
  ${apk_packages:+-e "TIMICH_LIBVIPS_APK_PACKAGES=$apk_packages"} \
  "$build_image" \
  sh /builder/build-runtime.sh

if [ -f "$runtime_dir_abs/bin/vips" ]; then
  chmod +x "$runtime_dir_abs/bin/vips"
fi
if [ -f "$runtime_dir_abs/bin/vips.real" ]; then
  chmod +x "$runtime_dir_abs/bin/vips.real"
fi

"$script_dir/verify.sh" "$runtime_dir"

artifact_name="${runtime_id}_${version}_${platform}.tar.gz"
artifact_path="$pack_dir_abs/$artifact_name"
tmp_dir=$(mktemp -d "$pack_dir_abs/.tmp-libvips-pack.XXXXXX")
trap 'rm -rf "$tmp_dir"' EXIT INT TERM

mkdir -p "$tmp_dir/media-runtime/libvips"
cp -a "$runtime_dir_abs/." "$tmp_dir/media-runtime/libvips/"
tar -C "$tmp_dir" -czf "$artifact_path" media-runtime/libvips

if command -v sha256sum >/dev/null 2>&1; then
  sha256sum "$artifact_path" > "$artifact_path.sha256"
else
  artifact_sha=$(shasum -a 256 "$artifact_path" | awk '{print $1}')
  printf '%s  %s\n' "$artifact_sha" "$artifact_name" > "$artifact_path.sha256"
fi

echo "Runtime input: $runtime_dir"
echo "Pack output:   $artifact_path"
