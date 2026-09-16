# Timich Media Helper

`timich-media-helper` is the first Rust boundary for Timich Agent local media
processing. It implements image EXIF capture-date inspection, ffprobe-backed
MP4/MOV capture-date and duration inspection, image rendition generation, and the MP4/MOV poster
extraction command used by local thumbnail generation.

Build and test from the directory containing the Agent Makefile:

```sh
make build-media-helper
make test-media-helper
```

Linux/QNAP native helper builds should use static Rust runtime linking:

```sh
MEDIA_HELPER_RUSTFLAGS="-C target-feature=+crt-static" make build-media-helper
```

On release builders without a local Rust toolchain, build the Linux helper
inside Docker:

```sh
DIST_OS=linux DIST_ARCH=amd64 MEDIA_HELPER_DOCKER=1 make build-media-helper
```

Run the health probe:

```sh
build/timich-media-helper health --json
```

Inspect image capture dates (JPEG, PNG, WebP, HEIC/HEIF) or video metadata:

```sh
build/timich-media-helper inspect-image --input /path/to/photo.heic
build/timich-media-helper inspect-video --input /path/to/video.mov
```

Both inspection commands return ordered `media.captureDates` candidates. The
Agent applies timezone and fallback policy. Image inspection reads EXIF in Rust
without an external runtime or pixel decoding; video inspection uses ffprobe
without decoding frames. An absent capture date produces an empty list. Video
`durationMs` is null when unavailable. Health's `captureImage` and `captureVideo`
flags report support separately from rendition generation.

Retain the dependency notices in `THIRD_PARTY_NOTICES.md` when distributing the
helper. Agent builds copy these alongside the binary, including release bundles.

Render an image rendition:

```sh
build/timich-media-helper render-image --input /path/to/image.heic --output /tmp/preview.jpg --max-edge 512 --quality 82
```

Extract a video poster:

```sh
build/timich-media-helper render-video-poster --input /path/to/video.mov --output /tmp/poster.jpg
```

The helper currently detects possible libvips, ffmpeg, and ffprobe backends from:

1. `TIMICH_MEDIA_HELPER_*` or existing `TIMICH_AGENT_*` media path overrides
2. for ffprobe, the directory containing an explicitly configured ffmpeg
3. bundle-local `media-runtime/...` directories next to the helper
4. executables on `PATH`

The health response is intentionally JSON-only so the Go Agent can consume it
without parsing human-readable command output.
