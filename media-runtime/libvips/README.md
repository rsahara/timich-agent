# libvips Runtime Inputs

Release builds may copy a platform libvips runtime from:

```text
media-runtime/libvips/<goos>_<goarch>/
```

into the release archive as:

```text
media-runtime/libvips/
  bin/vips
  bin/vips.real
  lib/
  share/
  THIRD_PARTY_NOTICES/
```

The agent auto-detects `media-runtime/libvips/bin/vips` next to the
`timich-agent` executable when `TIMICH_AGENT_VIPS_PATH` and
`mediaRuntime.vipsPath` are empty. At execution time it prepends the bundled
`bin`, `lib`, `share`, `girepository-1.0`, and `vips-modules-*` locations to the
child process environment before invoking `vips`.

Package the runtime for local thumbnail decoding, not general media encoding:

- include libvips, libheif, and decoder codecs needed for camera-origin HEIC and
  HEIF files
- avoid GPL encoder dependencies such as x265 unless the release licensing
  posture intentionally changes
- include license notices, source references, and SBOM data for all bundled
  native libraries
- validate with real camera-origin JPEG, PNG, WebP, HEIC, and HEIF fixtures

Docker images install their own Alpine `vips-tools` and `vips-heif` packages, so
this directory is for direct native release archives.

## Builder

The current linux amd64 builder assembles an Alpine runtime and writes a wrapper
at `bin/vips`. The wrapper executes the bundled `bin/vips.real` through the
bundled musl loader in `lib/ld-musl-x86_64.so.1`, so it can run on QNAP native
hosts without installing Alpine libraries globally.

```sh
make media-libvips-runtime-pack
make media-libvips-runtime-verify
```

The builder writes:

- runtime input: `media-runtime/libvips/linux_amd64/`
- archive/checksum: `dist/media-runtime-packs/timich-libvips-alpine-runtime_*.tar.gz`

Verification runs the bundled `vips` directly when the host can execute the
target runtime. On macOS or another non-target host, `verify.sh` falls back to a
Docker smoke test with the generated runtime mounted read-only.

This is a release-candidate runtime input, not the final licensing posture. It
still records `licenseReviewRequired: true` because Alpine `vips-heif` can pull
HEVC-related codec libraries such as `x265-libs`.

## Validation Notes

2026-06-21 NAS validation:

- assembled the linux amd64 libvips runtime on the NAS Docker engine
- confirmed `media-runtime/libvips/bin/vips` runs directly on the QNAP host
  through the bundled musl loader
- confirmed `vips list classes` exposes HEIF loading
- confirmed native PNG-to-JPEG thumbnail smoke through `vips thumbnail`
- confirmed `timich-media-helper render-image` auto-detects the bundle-local
  libvips runtime and renders a JPEG on the QNAP host
- runtime input size was about 73 MB; archive size was about 26 MB

2026-06-20 NAS validation:

- the QNAP host did not have a native `vips` executable on `PATH`
- no camera-origin `.heic` or `.heif` fixture was present in the inspected NAS
  media roots, so real-camera HEIC/HEIF validation remains open
- Alpine `vips-tools` plus `vips-heif` on the NAS Docker engine exposed
  `heifload` and `heifsave`; a generated HEIC input could be rendered to a JPEG
  thumbnail through `vips thumbnail`
- the Alpine package path pulled `x265-libs` as a dependency of `vips-heif`.
  Treat that as a release licensing decision point: either accept the Docker
  image's GPL-adjacent dependency posture, or assemble a decode-focused libvips
  runtime that keeps HEIC/HEIF decoding support without encoder dependencies
  such as x265.
