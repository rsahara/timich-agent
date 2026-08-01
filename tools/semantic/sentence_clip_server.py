#!/usr/bin/env python3
"""Local sentence-transformers CLIP embedding server for Timich dev testing."""

from __future__ import annotations

import base64
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from io import BytesIO
import json
import math
import os
import sys
from typing import Any

from PIL import Image
from sentence_transformers import SentenceTransformer


PROTOCOL_VERSION = 1
DEFAULT_HOST = "127.0.0.1"
DEFAULT_PORT = 19183
IMAGE_MODEL = "sentence-transformers/clip-ViT-B-32"
TEXT_MODEL = "sentence-transformers/clip-ViT-B-32-multilingual-v1"


def identity_response(layout: dict[str, Any], loaded: bool = True, can_embed: bool = True) -> dict[str, Any]:
    return {
        "protocolVersion": PROTOCOL_VERSION,
        "runtime": str(layout.get("runtime", "")).strip(),
        "modelId": str(layout.get("modelId", "")).strip(),
        "vectorSpaceId": str(layout.get("vectorSpaceId", "")).strip(),
        "embeddingDim": int(layout.get("embeddingDim", 0)),
        "inputKind": str(layout.get("inputKind", "")).strip(),
        "loaded": loaded,
        "canEmbed": can_embed,
    }


def normalize(values: list[float]) -> list[float]:
    norm = math.sqrt(sum(value * value for value in values))
    if norm == 0:
        return values
    return [float(value / norm) for value in values]


def to_vector(values: Any, expected_dim: int) -> list[float]:
    if hasattr(values, "tolist"):
        values = values.tolist()
    if values and isinstance(values[0], list):
        values = values[0]
    vector = normalize([float(value) for value in values])
    if len(vector) != expected_dim:
        raise ValueError(f"embedding dimension {len(vector)} does not match layout dimension {expected_dim}")
    return vector


class ClipRuntime:
    def __init__(self) -> None:
        image_name = os.environ.get("TIMICH_SENTENCE_CLIP_IMAGE_MODEL", IMAGE_MODEL)
        text_name = os.environ.get("TIMICH_SENTENCE_CLIP_TEXT_MODEL", TEXT_MODEL)
        self.text_routing = os.environ.get("TIMICH_SENTENCE_CLIP_TEXT_ROUTING", "auto").strip().lower() or "auto"
        print(f"loading image model: {image_name}", file=sys.stderr, flush=True)
        self.image_model = SentenceTransformer(image_name)
        print(f"loading text model: {text_name}", file=sys.stderr, flush=True)
        self.text_model = SentenceTransformer(text_name)
        print("sentence-CLIP server ready", file=sys.stderr, flush=True)

    def embed_text(self, text: str, expected_dim: int) -> list[float]:
        model = self.text_model
        if self.text_routing == "clip" or (self.text_routing == "auto" and is_ascii_text(text)):
            model = self.image_model
        embedding = model.encode([text], normalize_embeddings=True, convert_to_numpy=True)
        return to_vector(embedding, expected_dim)

    def embed_image(self, raw: bytes, expected_dim: int) -> list[float]:
        image = Image.open(BytesIO(raw)).convert("RGB")
        embedding = self.image_model.encode([image], normalize_embeddings=True, convert_to_numpy=True)
        return to_vector(embedding, expected_dim)


def is_ascii_text(text: str) -> bool:
    return all(ord(character) < 128 for character in text)


class Handler(BaseHTTPRequestHandler):
    server: "ClipHTTPServer"

    def do_GET(self) -> None:
        if self.path == "/healthz":
            self.write_json({"status": "ok"})
            return
        self.send_error(404)

    def do_POST(self) -> None:
        try:
            payload = self.read_json()
            layout = payload.get("layout") or {}
            if self.path == "/inspect":
                self.write_json(identity_response(layout))
                return
            if self.path == "/embed-text":
                text = str(payload.get("text") or "").strip()
                if not text:
                    raise ValueError("text is required")
                response = identity_response(layout)
                response["vector"] = self.server.runtime.embed_text(text, int(layout.get("embeddingDim", 0)))
                response["input"] = "text"
                self.write_json(response)
                return
            if self.path == "/embed-image":
                raw = base64.b64decode(str(payload.get("imageBase64") or ""), validate=True)
                if not raw:
                    raise ValueError("image input is required")
                response = identity_response(layout)
                response["vector"] = self.server.runtime.embed_image(raw, int(layout.get("embeddingDim", 0)))
                response["input"] = str(payload.get("source") or "image").strip() or "image"
                self.write_json(response)
                return
            self.send_error(404)
        except Exception as error:  # noqa: BLE001 - HTTP handler boundary
            self.write_json({"error": str(error)}, status=500)

    def read_json(self) -> dict[str, Any]:
        length = int(self.headers.get("Content-Length", "0"))
        raw = self.rfile.read(length)
        if not raw:
            return {}
        return json.loads(raw.decode("utf-8"))

    def write_json(self, payload: dict[str, Any], status: int = 200) -> None:
        raw = json.dumps(payload, separators=(",", ":")).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def log_message(self, format: str, *args: Any) -> None:
        print(format % args, file=sys.stderr)


class ClipHTTPServer(ThreadingHTTPServer):
    def __init__(self, address: tuple[str, int], runtime: ClipRuntime) -> None:
        super().__init__(address, Handler)
        self.runtime = runtime


def main() -> int:
    host = os.environ.get("TIMICH_SENTENCE_CLIP_HOST", DEFAULT_HOST)
    port = int(os.environ.get("TIMICH_SENTENCE_CLIP_PORT", str(DEFAULT_PORT)))
    runtime = ClipRuntime()
    server = ClipHTTPServer((host, port), runtime)
    print(f"listening on http://{host}:{port}", file=sys.stderr, flush=True)
    server.serve_forever()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
