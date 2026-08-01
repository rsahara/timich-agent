#!/usr/bin/env sh
set -eu

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

version=${FFMPEG_VERSION:-7.1.5}
source_url=${FFMPEG_SOURCE_URL:-https://ffmpeg.org/releases/ffmpeg-${version}.tar.xz}
gpg_key_url=${FFMPEG_GPG_KEY_URL:-https://ffmpeg.org/ffmpeg-devel.asc}
gpg_fingerprint=${FFMPEG_GPG_FINGERPRINT:-FCF986EA15E6E293A5644F10B4322F04D67658D8}
platform=${TIMICH_FFMPEG_PLATFORM:-linux_amd64}
configure_file=${TIMICH_FFMPEG_CONFIGURE:-/builder/config-lgpl-decode-only.txt}
output_dir=${TIMICH_FFMPEG_OUTPUT:-/out}
make_jobs=${TIMICH_MEDIA_FFMPEG_MAKE_JOBS:-$(nproc)}

if [ ! -f "$configure_file" ]; then
  echo "missing ffmpeg configure profile: $configure_file" >&2
  exit 1
fi

apk add --no-cache \
  bash \
  build-base \
  ca-certificates \
  curl \
  gnupg \
  nasm \
  pkgconf \
  tar \
  xz

work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT INT TERM

cd "$work_dir"

curl -fsSL "$source_url" -o ffmpeg.tar.xz
curl -fsSL "${source_url}.asc" -o ffmpeg.tar.xz.asc
curl -fsSL "$gpg_key_url" -o ffmpeg-devel.asc
sh "$script_dir/verify-source-signature.sh" \
  ffmpeg-devel.asc \
  ffmpeg.tar.xz.asc \
  ffmpeg.tar.xz \
  "$gpg_fingerprint"
sha256sum ffmpeg.tar.xz > ffmpeg.tar.xz.sha256
tar --no-same-owner --no-same-permissions -xJf ffmpeg.tar.xz

cd "ffmpeg-${version}"
configure_flags=$(grep -Ev '^[[:space:]]*(#|$)' "$configure_file" | tr '\n' ' ')
./configure --prefix=/opt/timich-ffmpeg-runtime ${configure_flags}
make -j"$make_jobs"
make install

/opt/timich-ffmpeg-runtime/bin/ffmpeg -hide_banner -buildconf 2>&1 | tee /tmp/buildconf.txt
! grep -q -- '--enable-gpl' /tmp/buildconf.txt
! grep -q -- '--enable-nonfree' /tmp/buildconf.txt
/opt/timich-ffmpeg-runtime/bin/ffmpeg -hide_banner -decoders | grep -Eq '^[[:space:]]*V.*[[:space:]]h264[[:space:]]'
/opt/timich-ffmpeg-runtime/bin/ffmpeg -hide_banner -decoders | grep -Eq '^[[:space:]]*V.*[[:space:]]hevc[[:space:]]'
/opt/timich-ffmpeg-runtime/bin/ffmpeg -hide_banner -decoders | grep -Eq '^[[:space:]]*V.*[[:space:]]mpeg4[[:space:]]'
/opt/timich-ffmpeg-runtime/bin/ffmpeg -hide_banner -decoders | grep -Eq '^[[:space:]]*V.*[[:space:]]mjpeg[[:space:]]'
/opt/timich-ffmpeg-runtime/bin/ffmpeg -hide_banner -encoders | grep -Eq '^[[:space:]]*V.*[[:space:]]mjpeg[[:space:]]'
/opt/timich-ffmpeg-runtime/bin/ffmpeg -hide_banner -demuxers | grep -Eq '^[[:space:]]*D[[:space:]]+mov,'
/opt/timich-ffmpeg-runtime/bin/ffmpeg -hide_banner -demuxers | grep -Eq '^[[:space:]]*D[[:space:]]+image2[[:space:]]'
/opt/timich-ffmpeg-runtime/bin/ffmpeg -hide_banner -muxers | grep -Eq '^[[:space:]]*E[[:space:]]+image2[[:space:]]'

rm -rf "${output_dir:?}/"*
mkdir -p "${output_dir}/bin" "${output_dir}/LICENSES" "${output_dir}/THIRD_PARTY_NOTICES"
cp /opt/timich-ffmpeg-runtime/bin/ffmpeg "${output_dir}/bin/ffmpeg"
cp /opt/timich-ffmpeg-runtime/bin/ffprobe "${output_dir}/bin/ffprobe"
strip "${output_dir}/bin/ffmpeg" "${output_dir}/bin/ffprobe" || true
cp LICENSE.md "${output_dir}/LICENSES/FFmpeg-LICENSE.md"
cp COPYING.LGPLv2.1 "${output_dir}/LICENSES/FFmpeg-COPYING.LGPLv2.1"
cp "$work_dir/ffmpeg.tar.xz.sha256" "${output_dir}/SOURCE.sha256"
cp "$configure_file" "${output_dir}/CONFIGURE.txt"
{
  echo 'Timich FFmpeg runtime'
  echo
  echo 'This runtime is built from FFmpeg source for Timich local MP4/MOV poster extraction.'
  echo 'It is configured without --enable-gpl and without --enable-nonfree.'
  echo 'See LICENSES/ and SOURCE.sha256 for FFmpeg license and source identity.'
} > "${output_dir}/THIRD_PARTY_NOTICES/FFmpeg.txt"

source_sha=$(cut -d ' ' -f 1 "$work_dir/ffmpeg.tar.xz.sha256")
built_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
printf '%s\n' \
  '{' \
  '  "schemaVersion": 1,' \
  "  \"platform\": \"${platform}\"," \
  "  \"ffmpegVersion\": \"${version}\"," \
  "  \"sourceUrl\": \"${source_url}\"," \
  "  \"sourceSha256\": \"${source_sha}\"," \
  "  \"builtAt\": \"${built_at}\"," \
  '  "license": "LGPL-2.1-or-later"' \
  '}' > "${output_dir}/BUILDINFO.json"

if [ -n "${TIMICH_OUTPUT_UID:-}" ] && [ -n "${TIMICH_OUTPUT_GID:-}" ]; then
  chown -R "${TIMICH_OUTPUT_UID}:${TIMICH_OUTPUT_GID}" "$output_dir" || true
fi
