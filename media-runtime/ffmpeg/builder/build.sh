#!/usr/bin/env sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
agent_root=$(CDPATH= cd -- "$script_dir/../../.." && pwd)

version=${TIMICH_MEDIA_FFMPEG_VERSION:-7.1.5}
platform=${TIMICH_MEDIA_FFMPEG_PLATFORM:-linux_amd64}
docker_platform=${TIMICH_MEDIA_FFMPEG_DOCKER_PLATFORM:-linux/amd64}
source_url=${TIMICH_MEDIA_FFMPEG_SOURCE_URL:-https://ffmpeg.org/releases/ffmpeg-${version}.tar.xz}
gpg_key_url=${TIMICH_MEDIA_FFMPEG_GPG_KEY_URL:-https://ffmpeg.org/ffmpeg-devel.asc}
gpg_fingerprint=${TIMICH_MEDIA_FFMPEG_GPG_FINGERPRINT:-FCF986EA15E6E293A5644F10B4322F04D67658D8}
dockerfile=${TIMICH_MEDIA_FFMPEG_DOCKERFILE:-$script_dir/linux-amd64.Dockerfile}
image=${TIMICH_MEDIA_FFMPEG_IMAGE:-timich-ffmpeg-runtime:${version}-${platform}}
build_strategy=${TIMICH_MEDIA_FFMPEG_BUILD_STRATEGY:-container}
build_image=${TIMICH_MEDIA_FFMPEG_BUILD_IMAGE:-alpine:3.22}
make_jobs=${TIMICH_MEDIA_FFMPEG_MAKE_JOBS:-2}
runtime_dir=${TIMICH_MEDIA_FFMPEG_RUNTIME_DIR:-$agent_root/media-runtime/ffmpeg/$platform}
pack_dir=${TIMICH_MEDIA_FFMPEG_PACK_DIR:-$agent_root/dist/media-runtime-packs}
base_url=${TIMICH_MEDIA_FFMPEG_BASE_URL:-}
name=${TIMICH_MEDIA_FFMPEG_NAME:-Timich FFmpeg LGPL Decode Runtime}
runtime_id=${TIMICH_MEDIA_FFMPEG_ID:-timich-ffmpeg-lgpl-decode-runtime}

if ! command -v docker >/dev/null 2>&1; then
  echo "docker is required to build the ffmpeg runtime" >&2
  exit 1
fi

mkdir -p "$runtime_dir" "$pack_dir"
runtime_dir_abs=$(CDPATH= cd -- "$runtime_dir" && pwd)
output_uid=$(id -u 2>/dev/null || true)
output_gid=$(id -g 2>/dev/null || true)

case "$build_strategy" in
  container)
    docker run --rm \
      --platform "$docker_platform" \
      -v "$script_dir:/builder:ro" \
      -v "$runtime_dir_abs:/out" \
      -e "FFMPEG_VERSION=$version" \
      -e "FFMPEG_SOURCE_URL=$source_url" \
      -e "FFMPEG_GPG_KEY_URL=$gpg_key_url" \
      -e "FFMPEG_GPG_FINGERPRINT=$gpg_fingerprint" \
      -e "TIMICH_FFMPEG_PLATFORM=$platform" \
      -e "TIMICH_FFMPEG_CONFIGURE=/builder/config-lgpl-decode-only.txt" \
      -e "TIMICH_FFMPEG_OUTPUT=/out" \
      -e "TIMICH_MEDIA_FFMPEG_MAKE_JOBS=$make_jobs" \
      -e "TIMICH_OUTPUT_UID=$output_uid" \
      -e "TIMICH_OUTPUT_GID=$output_gid" \
      "$build_image" \
      sh /builder/build-runtime.sh
    ;;
  image)
    docker build \
      --platform "$docker_platform" \
      -f "$dockerfile" \
      -t "$image" \
      --build-arg "FFMPEG_VERSION=$version" \
      --build-arg "FFMPEG_SOURCE_URL=$source_url" \
      --build-arg "FFMPEG_GPG_KEY_URL=$gpg_key_url" \
      --build-arg "FFMPEG_GPG_FINGERPRINT=$gpg_fingerprint" \
      --build-arg "TIMICH_FFMPEG_PLATFORM=$platform" \
      "$script_dir"

    docker run --rm \
      --platform "$docker_platform" \
      -v "$runtime_dir_abs:/out" \
      "$image"
    ;;
  *)
    echo "unsupported TIMICH_MEDIA_FFMPEG_BUILD_STRATEGY: $build_strategy" >&2
    exit 2
    ;;
esac

"$script_dir/verify.sh" "$runtime_dir"

python3 "$agent_root/tools/media/make_ffmpeg_runtime_pack.py" \
  --runtime-dir "$runtime_dir" \
  --output-dir "$pack_dir" \
  --id "$runtime_id" \
  --name "$name" \
  --version "$version" \
  --platform "$platform" \
  --base-url "$base_url"

echo "Runtime input: $runtime_dir"
echo "Pack output:   $pack_dir"
