#!/usr/bin/env sh
set -eu

runtime_dir=${1:-}
if [ -z "$runtime_dir" ]; then
  echo "usage: verify.sh <runtime-dir>" >&2
  exit 2
fi
runtime_dir=$(CDPATH= cd -- "$runtime_dir" && pwd)

vips="$runtime_dir/bin/vips"
if [ ! -x "$vips" ]; then
  echo "missing executable vips wrapper: $vips" >&2
  exit 1
fi
if [ ! -x "$runtime_dir/bin/vips.real" ]; then
  echo "missing executable vips.real: $runtime_dir/bin/vips.real" >&2
  exit 1
fi
if [ ! -f "$runtime_dir/lib/ld-musl-x86_64.so.1" ]; then
  echo "missing bundled musl loader" >&2
  exit 1
fi

runtime_parent=$(dirname -- "$runtime_dir")
tmp_dir=$(mktemp -d "$runtime_parent/.tmp-libvips-verify.XXXXXX")
trap 'rm -rf "$tmp_dir"' EXIT INT TERM

direct_vips=0
if "$vips" --version >/dev/null 2>&1; then
  direct_vips=1
elif ! command -v docker >/dev/null 2>&1; then
  echo "cannot execute libvips directly and docker is not available for verification" >&2
  exit 1
fi

run_vips() {
  if [ "$direct_vips" = "1" ]; then
    "$vips" "$@"
    return
  fi
  docker run --rm --platform "${TIMICH_MEDIA_LIBVIPS_DOCKER_PLATFORM:-linux/amd64}" \
    -v "$runtime_dir:/runtime:ro" \
    -v "$tmp_dir:/work" \
    alpine:3.22 \
    /runtime/bin/vips "$@"
}

run_vips --version >/dev/null
classes_output="$tmp_dir/vips-classes.txt"
run_vips list classes > "$classes_output"
grep -Eq 'heifload|VipsForeignLoadHeif' "$classes_output"

if [ "$direct_vips" = "1" ]; then
  input="$tmp_dir/input.png"
  output="$tmp_dir/output.jpg"
else
  input="/work/input.png"
  output="/work/output.jpg"
fi
host_output="$tmp_dir/output.jpg"
run_vips black "$input" 64 64 >/dev/null
run_vips thumbnail "$input" "$output[Q=82,strip]" 32 --height 32 --size down >/dev/null
if [ ! -s "$host_output" ]; then
  echo "libvips thumbnail smoke did not produce output" >&2
  exit 1
fi

fixture=${TIMICH_MEDIA_LIBVIPS_HEIC_FIXTURE:-}
if [ -n "$fixture" ]; then
  if [ ! -f "$fixture" ]; then
    echo "HEIC/HEIF fixture not found: $fixture" >&2
    exit 1
  fi
  host_heic_output="$tmp_dir/heic-output.jpg"
  if [ "$direct_vips" = "1" ]; then
    run_vips thumbnail "$fixture" "$host_heic_output[Q=82,strip]" 64 --height 64 --size down >/dev/null
  else
    fixture_dir=$(CDPATH= cd -- "$(dirname -- "$fixture")" && pwd)
    fixture_name=$(basename -- "$fixture")
    docker run --rm --platform "${TIMICH_MEDIA_LIBVIPS_DOCKER_PLATFORM:-linux/amd64}" \
      -v "$runtime_dir:/runtime:ro" \
      -v "$tmp_dir:/work" \
      -v "$fixture_dir:/fixture:ro" \
      alpine:3.22 \
      /runtime/bin/vips thumbnail "/fixture/$fixture_name" "/work/heic-output.jpg[Q=82,strip]" 64 --height 64 --size down >/dev/null
  fi
  if [ ! -s "$host_heic_output" ]; then
    echo "HEIC/HEIF fixture smoke did not produce output" >&2
    exit 1
  fi
fi

echo "libvips runtime verified: $runtime_dir"
