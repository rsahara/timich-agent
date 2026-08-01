#!/usr/bin/env sh
set -eu

runtime_dir=${1:-}
if [ -z "$runtime_dir" ]; then
  echo "usage: verify.sh <runtime-dir>" >&2
  exit 2
fi
runtime_dir=$(CDPATH= cd -- "$runtime_dir" && pwd)

ffmpeg="$runtime_dir/bin/ffmpeg"
ffprobe="$runtime_dir/bin/ffprobe"

if [ ! -x "$ffmpeg" ]; then
  echo "missing executable ffmpeg: $ffmpeg" >&2
  exit 1
fi
if [ ! -x "$ffprobe" ]; then
  echo "missing executable ffprobe: $ffprobe" >&2
  exit 1
fi

run_ffmpeg() {
  if "$ffmpeg" -hide_banner -version >/dev/null 2>&1; then
    "$ffmpeg" "$@"
    return
  fi
  if ! command -v docker >/dev/null 2>&1; then
    echo "cannot execute ffmpeg directly and docker is not available for verification" >&2
    exit 1
  fi
  docker run --rm --platform "${TIMICH_MEDIA_FFMPEG_DOCKER_PLATFORM:-linux/amd64}" \
    -v "$runtime_dir:/runtime:ro" \
    alpine:3.22 \
    /runtime/bin/ffmpeg "$@"
}

buildconf=$(run_ffmpeg -hide_banner -buildconf 2>&1)
case "$buildconf" in
  *--enable-gpl*|*--enable-nonfree*)
    echo "ffmpeg runtime must not be built with --enable-gpl or --enable-nonfree" >&2
    exit 1
    ;;
esac

decoders=$(run_ffmpeg -hide_banner -decoders 2>&1)
encoders=$(run_ffmpeg -hide_banner -encoders 2>&1)
demuxers=$(run_ffmpeg -hide_banner -demuxers 2>&1)
muxers=$(run_ffmpeg -hide_banner -muxers 2>&1)

for decoder in h264 hevc mpeg4 mjpeg; do
  printf '%s\n' "$decoders" | grep -Eq "^[[:space:]]*V.*[[:space:]]${decoder}[[:space:]]" || {
    echo "missing required video decoder: $decoder" >&2
    exit 1
  }
done

printf '%s\n' "$encoders" | grep -Eq '^[[:space:]]*V.*[[:space:]]mjpeg[[:space:]]' || {
  echo "missing required MJPEG encoder" >&2
  exit 1
}

printf '%s\n' "$demuxers" | grep -Eq '^[[:space:]]*D[[:space:]]+mov,' || {
  echo "missing required MOV/MP4 demuxer" >&2
  exit 1
}

printf '%s\n' "$demuxers" | grep -Eq '^[[:space:]]*D[[:space:]]+image2[[:space:]]' || {
  echo "missing required image2 demuxer for Agent preflight smoke" >&2
  exit 1
}

printf '%s\n' "$muxers" | grep -Eq '^[[:space:]]*E[[:space:]]+image2[[:space:]]' || {
  echo "missing required image2 muxer" >&2
  exit 1
}

fixture=${TIMICH_MEDIA_FFMPEG_FIXTURE:-}
if [ -n "$fixture" ]; then
  if [ ! -f "$fixture" ]; then
    echo "fixture not found: $fixture" >&2
    exit 1
  fi
  tmp_dir=$(mktemp -d)
  trap 'rm -rf "$tmp_dir"' EXIT INT TERM
  output="$tmp_dir/poster.jpg"
  if "$ffmpeg" -hide_banner -version >/dev/null 2>&1; then
    "$ffmpeg" -hide_banner -loglevel error -nostdin -y -i "$fixture" \
      -map 0:v:0 -frames:v 1 -an -sn -dn "$output"
  else
    fixture_dir=$(CDPATH= cd -- "$(dirname -- "$fixture")" && pwd)
    fixture_name=$(basename -- "$fixture")
    docker run --rm --platform "${TIMICH_MEDIA_FFMPEG_DOCKER_PLATFORM:-linux/amd64}" \
      -v "$runtime_dir:/runtime:ro" \
      -v "$fixture_dir:/fixture:ro" \
      -v "$tmp_dir:/out" \
      alpine:3.22 \
      /runtime/bin/ffmpeg -hide_banner -loglevel error -nostdin -y \
      -i "/fixture/$fixture_name" -map 0:v:0 -frames:v 1 -an -sn -dn /out/poster.jpg
  fi
  if [ ! -s "$output" ]; then
    echo "poster smoke test did not produce output" >&2
    exit 1
  fi
fi

echo "ffmpeg runtime verified: $runtime_dir"
