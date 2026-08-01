#!/usr/bin/env python3
"""Smoke-test timich-media-helper against local media backends.

This script intentionally uses only the Python standard library so it can run on
NAS/native release hosts after the helper binary has been built or unpacked.
"""

from __future__ import annotations

import argparse
import binascii
import json
import os
from pathlib import Path
import struct
import subprocess
import sys
import tempfile
import zlib


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--helper",
        default=os.environ.get("TIMICH_MEDIA_HELPER_BINARY", "build/timich-media-helper"),
        help="Path to timich-media-helper.",
    )
    parser.add_argument(
        "--image-fixture",
        default=os.environ.get("TIMICH_MEDIA_HELPER_SMOKE_IMAGE", ""),
        help="Optional image fixture. A generated PNG is used when omitted.",
    )
    parser.add_argument(
        "--video-fixture",
        default=os.environ.get("TIMICH_MEDIA_HELPER_SMOKE_VIDEO", ""),
        help="Optional MP4/MOV fixture for poster extraction smoke.",
    )
    args = parser.parse_args()

    helper = Path(args.helper)
    if not helper.is_file():
        print(f"missing helper binary: {helper}", file=sys.stderr)
        return 127
    if not os.access(helper, os.X_OK):
        print(f"helper is not executable: {helper}", file=sys.stderr)
        return 127

    with tempfile.TemporaryDirectory(prefix="timich-media-helper-smoke-") as temp_name:
        temp_dir = Path(temp_name)
        image_fixture = Path(args.image_fixture) if args.image_fixture else temp_dir / "fixture.png"
        if not args.image_fixture:
            write_png_fixture(image_fixture)

        health = run_json([str(helper), "health", "--json"])
        require(health.get("schemaVersion") == 1, "health schemaVersion must be 1")
        require(health.get("ok") is True, "health ok must be true")
        capabilities = health.get("capabilities") or {}
        require(capabilities.get("renderImage") is True, "renderImage capability is required")

        rendition_path = temp_dir / "rendition.jpg"
        render_image = run_json(
            [
                str(helper),
                "render-image",
                "--input",
                str(image_fixture),
                "--output",
                str(rendition_path),
                "--max-edge",
                "64",
                "--quality",
                "82",
            ]
        )
        require(render_image.get("operation") == "render-image", "render-image operation mismatch")
        require_jpeg(rendition_path, "render-image output")

        video_fixture = Path(args.video_fixture) if args.video_fixture else None
        if video_fixture:
            require(video_fixture.is_file(), f"video fixture is missing: {video_fixture}")
            require(
                capabilities.get("renderVideoPoster") is True,
                "renderVideoPoster capability is required for video smoke",
            )
            poster_path = temp_dir / "poster.jpg"
            poster = run_json(
                [
                    str(helper),
                    "render-video-poster",
                    "--input",
                    str(video_fixture),
                    "--output",
                    str(poster_path),
                ]
            )
            require(poster.get("operation") == "render-video-poster", "render-video-poster operation mismatch")
            require_jpeg(poster_path, "render-video-poster output")

        print("timich-media-helper smoke: ok")
        return 0


def run_json(argv: list[str]) -> dict:
    process = subprocess.run(argv, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False)
    if process.returncode != 0:
        raise SystemExit(
            f"command failed ({process.returncode}): {shlex_join(argv)}\n"
            f"stdout:\n{process.stdout}\n"
            f"stderr:\n{process.stderr}"
        )
    try:
        return json.loads(process.stdout)
    except json.JSONDecodeError as err:
        raise SystemExit(
            f"command did not return JSON: {shlex_join(argv)}\n"
            f"error: {err}\n"
            f"stdout:\n{process.stdout}\n"
            f"stderr:\n{process.stderr}"
        ) from err


def write_png_fixture(path: Path, width: int = 96, height: int = 64) -> None:
    rows = []
    for y in range(height):
        row = bytearray([0])
        for x in range(width):
            row.extend(((x * 3) % 256, (y * 4) % 256, 160))
        rows.append(bytes(row))
    compressed = zlib.compress(b"".join(rows))
    body = b"".join(
        [
            b"\x89PNG\r\n\x1a\n",
            png_chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)),
            png_chunk(b"IDAT", compressed),
            png_chunk(b"IEND", b""),
        ]
    )
    path.write_bytes(body)


def png_chunk(kind: bytes, payload: bytes) -> bytes:
    crc = binascii.crc32(kind)
    crc = binascii.crc32(payload, crc) & 0xFFFFFFFF
    return struct.pack(">I", len(payload)) + kind + payload + struct.pack(">I", crc)


def require_jpeg(path: Path, label: str) -> None:
    require(path.is_file(), f"{label} was not created: {path}")
    body = path.read_bytes()
    require(len(body) > 4, f"{label} is empty")
    require(body.startswith(b"\xff\xd8"), f"{label} is not a JPEG")


def require(condition: bool, message: str) -> None:
    if not condition:
        raise SystemExit(message)


def shlex_join(argv: list[str]) -> str:
    return " ".join(shlex_quote(item) for item in argv)


def shlex_quote(value: str) -> str:
    return "'" + value.replace("'", "'\"'\"'") + "'"


if __name__ == "__main__":
    raise SystemExit(main())
