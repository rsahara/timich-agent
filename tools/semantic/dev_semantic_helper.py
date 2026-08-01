#!/usr/bin/env python3
"""Route Timich dev semantic helper calls to local model-specific servers."""

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
RUNTIME_SENTENCE_CLIP = "sentence-transformers-clip"
RUNTIME_SIGLIP2 = "transformers-siglip2"
RUNTIME_ONNXRUNTIME = "onnxruntime"
DEFAULT_SENTENCE_CLIP_SERVER_URL = "http://127.0.0.1:19183"
DEFAULT_SIGLIP2_SERVER_URL = "http://127.0.0.1:19185"
DEFAULT_ONNX_SERVER_URL = "http://127.0.0.1:19188"
DEFAULT_SIGLIP2_SERVER_URLS = {
    "timich-siglip2-base-patch16-224-multilingual-v1": DEFAULT_SIGLIP2_SERVER_URL,
    "timich-siglip2-base-patch32-256-multilingual-v1": "http://127.0.0.1:19187",
}
DEFAULT_ONNX_SERVER_URLS = {
    "timich-siglip2-base-patch16-224-int8-onnx-multilingual-v1": DEFAULT_ONNX_SERVER_URL,
}


def read_layout(runtime_layout: str) -> dict[str, Any]:
    path = Path(runtime_layout) / "timich-model.json"
    with path.open("r", encoding="utf-8") as handle:
        return json.load(handle)


def server_url(layout: dict[str, Any]) -> str:
    runtime = str(layout.get("runtime", "")).strip()
    if runtime == RUNTIME_SENTENCE_CLIP:
        return os.environ.get("TIMICH_SENTENCE_CLIP_SERVER_URL", DEFAULT_SENTENCE_CLIP_SERVER_URL).rstrip("/")
    if runtime == RUNTIME_SIGLIP2:
        model_id = str(layout.get("modelId", "")).strip()
        if model_id:
            key = "TIMICH_SIGLIP2_SERVER_URL_" + "".join(character if character.isalnum() else "_" for character in model_id).upper()
            if value := os.environ.get(key, "").strip():
                return value.rstrip("/")
            if value := DEFAULT_SIGLIP2_SERVER_URLS.get(model_id):
                return value
        return os.environ.get("TIMICH_SIGLIP2_SERVER_URL", DEFAULT_SIGLIP2_SERVER_URL).rstrip("/")
    if runtime == RUNTIME_ONNXRUNTIME:
        model_id = str(layout.get("modelId", "")).strip()
        vector_space_id = str(layout.get("vectorSpaceId", "")).strip()
        if model_id:
            for key in (onnx_runtime_env_key(model_id, vector_space_id), onnx_runtime_env_key(model_id, "")):
                if value := os.environ.get(key, "").strip():
                    return value.rstrip("/")
            if value := DEFAULT_ONNX_SERVER_URLS.get(model_id):
                return value
        return os.environ.get("TIMICH_ONNX_SERVER_URL", DEFAULT_ONNX_SERVER_URL).rstrip("/")
    return ""


def onnx_runtime_env_key(model_id: str, vector_space_id: str) -> str:
    model_hex = model_id.strip().encode("utf-8").hex().upper()
    vector_hex = vector_space_id.strip().encode("utf-8").hex().upper()
    return f"TIMICH_ONNX_SERVER_URL_M_{model_hex}_V_{vector_hex}"


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


def post_json(layout: dict[str, Any], path: str, payload: dict[str, Any], timeout: float) -> dict[str, Any]:
    target = server_url(layout)
    if not target:
        return identity_response(layout, False, False, "semantic_runtime_dev_helper_unsupported")
    raw = json.dumps(payload).encode("utf-8")
    request = urllib.request.Request(
        target + path,
        data=raw,
        headers={"Content-Type": "application/json", "Accept": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(request, timeout=timeout) as response:
        return json.loads(response.read().decode("utf-8"))


def emit(payload: dict[str, Any]) -> None:
    json.dump(payload, sys.stdout, separators=(",", ":"))
    sys.stdout.write("\n")


def unavailable_message_code(layout: dict[str, Any]) -> str:
    runtime = str(layout.get("runtime", "")).strip()
    if runtime == RUNTIME_SENTENCE_CLIP:
        return "semantic_runtime_sentence_clip_server_unavailable"
    if runtime == RUNTIME_SIGLIP2:
        return "semantic_runtime_siglip2_server_unavailable"
    if runtime == RUNTIME_ONNXRUNTIME:
        return "semantic_runtime_onnx_server_unavailable"
    return "semantic_runtime_dev_helper_unsupported"


def command_inspect(args: argparse.Namespace) -> int:
    layout = read_layout(args.runtime_layout)
    try:
        emit(post_json(layout, "/inspect", {"layout": layout}, timeout=4.0))
    except (OSError, urllib.error.URLError, TimeoutError):
        emit(identity_response(layout, False, False, unavailable_message_code(layout)))
    return 0


def command_embed_text(args: argparse.Namespace) -> int:
    layout = read_layout(args.runtime_layout)
    try:
        emit(post_json(layout, "/embed-text", {"layout": layout, "text": args.text}, timeout=30.0))
        return 0
    except (OSError, urllib.error.URLError, TimeoutError) as error:
        print(f"semantic text embedding failed: {error}", file=sys.stderr)
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
        emit(post_json(layout, "/embed-image", payload, timeout=30.0))
        return 0
    except (OSError, urllib.error.URLError, TimeoutError) as error:
        print(f"semantic image embedding failed: {error}", file=sys.stderr)
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
