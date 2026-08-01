FROM alpine:3.22 AS build

ARG FFMPEG_VERSION=7.1.5
ARG FFMPEG_SOURCE_URL=https://ffmpeg.org/releases/ffmpeg-${FFMPEG_VERSION}.tar.xz
ARG FFMPEG_GPG_KEY_URL=https://ffmpeg.org/ffmpeg-devel.asc
ARG FFMPEG_GPG_FINGERPRINT=FCF986EA15E6E293A5644F10B4322F04D67658D8
ARG TIMICH_FFMPEG_PLATFORM=linux_amd64

WORKDIR /builder

COPY config-lgpl-decode-only.txt build-runtime.sh verify-source-signature.sh /builder/

RUN FFMPEG_VERSION="${FFMPEG_VERSION}" \
  FFMPEG_SOURCE_URL="${FFMPEG_SOURCE_URL}" \
  FFMPEG_GPG_KEY_URL="${FFMPEG_GPG_KEY_URL}" \
  FFMPEG_GPG_FINGERPRINT="${FFMPEG_GPG_FINGERPRINT}" \
  TIMICH_FFMPEG_PLATFORM="${TIMICH_FFMPEG_PLATFORM}" \
  TIMICH_FFMPEG_CONFIGURE="/builder/config-lgpl-decode-only.txt" \
  TIMICH_FFMPEG_OUTPUT="/runtime" \
  sh /builder/build-runtime.sh

FROM alpine:3.22

COPY --from=build /runtime /runtime

CMD ["sh", "-c", "set -eu; rm -rf /out/*; cp -a /runtime/. /out/"]
