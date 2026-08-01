# FFmpeg Runtime Inputs

Release builds may copy a platform FFmpeg runtime from:

```text
media-runtime/ffmpeg/<goos>_<goarch>/
```

into the release archive as:

```text
media-runtime/ffmpeg/
  bin/ffmpeg
  bin/ffprobe
  LICENSES/
  THIRD_PARTY_NOTICES/
  SBOM.spdx.json
```

The agent auto-detects `media-runtime/ffmpeg/bin/ffmpeg` next to the
`timich-agent` executable when `TIMICH_AGENT_FFMPEG_PATH` and
`mediaRuntime.ffmpegPath` are empty. At execution time it prepends the bundled
`bin` and `lib` locations to the child process environment before invoking
`ffmpeg`.

Package the runtime for thumbnail decoding, not general media encoding:

- include MP4/MOV poster extraction support for H.264, HEVC, MPEG-4, and MJPEG
  camera/video assets
- include JPEG image output support for generated poster frames
- include `ffprobe` when video metadata normalization is enabled
- include license notices, source references, and SBOM data for all bundled
  native libraries and codecs
- validate with real camera-origin MP4 and MOV fixtures before publishing

Docker images install their own Alpine `ffmpeg` package, so this directory is
for direct native release archives.

## Building the Runtime

The repository tracks the build recipe, not generated FFmpeg binaries. Build the
default Linux amd64 runtime input with:

```sh
make media-ffmpeg-runtime-pack
```

This target:

- builds FFmpeg from the official release tarball inside Docker. The default
  strategy runs the source build in a `linux/amd64` Alpine container so it works
  without Docker buildx; set `TIMICH_MEDIA_FFMPEG_BUILD_STRATEGY=image` to use
  the Dockerfile path explicitly.
- verifies the FFmpeg release signature with the FFmpeg release signing key
- applies `builder/config-lgpl-decode-only.txt`
- fails if `--enable-gpl` or `--enable-nonfree` appears in the build
- verifies H.264, HEVC, MPEG-4, and MJPEG decode support plus JPEG poster output
- writes the dist input to `media-runtime/ffmpeg/linux_amd64/`
- writes a release pack, checksum, metadata, and SPDX SBOM to
  `dist/media-runtime-packs/`

Build release artifacts on native Linux amd64 hardware or an amd64 NAS/CI runner
when possible. Apple Silicon Docker amd64 emulation can run the final binary but
has shown compiler instability during this FFmpeg source build. The generated
binary is statically linked (`--extra-ldflags=-static`) so native NAS/QNAP hosts
do not need Alpine's musl loader installed.

Use the generated runtime input in a Linux amd64 Agent archive with:

```sh
DIST_OS=linux DIST_ARCH=amd64 MEDIA_RUNTIME_FFMPEG_REQUIRED=1 make dist
```

To smoke-test a camera-origin MP4/MOV fixture after building:

```sh
TIMICH_MEDIA_FFMPEG_FIXTURE=/path/to/video.mov make media-ffmpeg-runtime-verify
```

The default profile intentionally avoids libx264/libx265 and other GPL/nonfree
encoder dependencies. If release requirements change, add a separate profile
instead of weakening the default LGPL decode-only build.

## Validation Notes

2026-06-20 NAS validation:

- the QNAP host-provided `ffmpeg` was present but lacked the required H.264 and
  HEVC decoders
- a Docker-built dynamic-musl FFmpeg binary did not execute directly on the QNAP
  host, so the release profile now statically links the runtime
- the static FFmpeg 7.1.5 runtime executed on the QNAP host, exposed H.264,
  HEVC, MPEG-4, and MJPEG decoders plus the `image2` JPEG output path, and
  extracted a poster JPEG from a real H.264 MP4 fixture
- the generated linux amd64 runtime input was about 10 MiB, and the release pack
  tarball was about 3.9 MiB
