#!/usr/bin/env python3
"""Timich semantic helper wrapper for the dev sentence-CLIP server."""

from __future__ import annotations

import argparse
import base64
import json
import os
import sys
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any


PROTOCOL_VERSION = 1
DEFAULT_SERVER_URL = "http://127.0.0.1:19183"


def read_layout(runtime_layout: str) -> dict[str, Any]:
    path = Path(runtime_layout) / "timich-model.json"
    with path.open("r", encoding="utf-8") as handle:
        return json.load(handle)


def identity_response(layout: dict[str, Any], loaded: bool, can_embed: bool, message_code: str = "") -> dict[str, Any]:
    payload: dict[str, Any] = {
        "protocolVersion": PROTOCOL_VERSION,
        "runtime": str(layout.get("runtime", "")).strip(),
        "modelId": str(layout.get("modelId", "")).strip(),
        "vectorSpaceId": str(layout.get("vectorSpaceId", "")).strip(),
        "embeddingDim": int(layout.get("embeddingDim", 0)),
        "inputKind": str(layout.get("inputKind", "")).strip(),
        "loaded": loaded,
        "canEmbed": can_embed,
    }
    if message_code:
        payload["messageCode"] = message_code
    return payload


def server_url() -> str:
    return os.environ.get("TIMICH_SENTENCE_CLIP_SERVER_URL", DEFAULT_SERVER_URL).rstrip("/")


def post_json(path: str, payload: dict[str, Any], timeout: float) -> dict[str, Any]:
    raw = json.dumps(payload).encode("utf-8")
    request = urllib.request.Request(
        server_url() + path,
        data=raw,
        headers={"Content-Type": "application/json", "Accept": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(request, timeout=timeout) as response:
        return json.loads(response.read().decode("utf-8"))


def emit(payload: dict[str, Any]) -> None:
    json.dump(payload, sys.stdout, separators=(",", ":"))
    sys.stdout.write("\n")


def command_inspect(args: argparse.Namespace) -> int:
    layout = read_layout(args.runtime_layout)
    try:
        emit(post_json("/inspect", {"layout": layout}, timeout=4.0))
    except (OSError, urllib.error.URLError, TimeoutError):
        emit(identity_response(layout, False, False, "semantic_runtime_sentence_clip_server_unavailable"))
    return 0


def command_embed_text(args: argparse.Namespace) -> int:
    layout = read_layout(args.runtime_layout)
    try:
        emit(post_json("/embed-text", {"layout": layout, "text": args.text}, timeout=30.0))
        return 0
    except (OSError, urllib.error.URLError, TimeoutError) as error:
        print(f"sentence-CLIP text embedding failed: {error}", file=sys.stderr)
        return 1


def command_embed_image(args: argparse.Namespace) -> int:
    layout = read_layout(args.runtime_layout)
    image_bytes = sys.stdin.buffer.read()
    payload = {
        "layout": layout,
        "contentType": args.content_type,
        "source": args.source or "",
        "imageBase64": base64.b64encode(image_bytes).decode("ascii"),
    }
    try:
        emit(post_json("/embed-image", payload, timeout=30.0))
        return 0
    except (OSError, urllib.error.URLError, TimeoutError) as error:
        print(f"sentence-CLIP image embedding failed: {error}", file=sys.stderr)
        return 1


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser()
    subparsers = parser.add_subparsers(dest="command", required=True)

    inspect = subparsers.add_parser("inspect")
    inspect.add_argument("--runtime-layout", required=True)
    inspect.set_defaults(func=command_inspect)

    embed_image = subparsers.add_parser("embed-image")
    embed_image.add_argument("--runtime-layout", required=True)
    embed_image.add_argument("--content-type", required=True)
    embed_image.add_argument("--source", default="")
    embed_image.set_defaults(func=command_embed_image)

    embed_text = subparsers.add_parser("embed-text")
    embed_text.add_argument("--runtime-layout", required=True)
    embed_text.add_argument("--text", required=True)
    embed_text.set_defaults(func=command_embed_text)

    return parser


def main(argv: list[str]) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    return int(args.func(args))


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
