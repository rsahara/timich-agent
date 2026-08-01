#!/usr/bin/env python3
"""Local SigLIP 2 embedding server for Timich dev testing."""

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
import torch
from transformers import AutoModel, AutoProcessor


PROTOCOL_VERSION = 1
DEFAULT_HOST = "127.0.0.1"
DEFAULT_PORT = 19185
MODEL = "google/siglip2-base-patch16-224"


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
    if hasattr(values, "detach"):
        values = values.detach().cpu()
    if hasattr(values, "tolist"):
        values = values.tolist()
    if values and isinstance(values[0], list):
        values = values[0]
    vector = normalize([float(value) for value in values])
    if len(vector) != expected_dim:
        raise ValueError(f"embedding dimension {len(vector)} does not match layout dimension {expected_dim}")
    return vector


def choose_device() -> torch.device:
    configured = os.environ.get("TIMICH_SIGLIP2_DEVICE", "auto").strip().lower()
    if configured and configured != "auto":
        return torch.device(configured)
    if torch.cuda.is_available():
        return torch.device("cuda")
    if getattr(torch.backends, "mps", None) is not None and torch.backends.mps.is_available():
        return torch.device("mps")
    return torch.device("cpu")


def move_inputs(inputs: Any, device: torch.device) -> Any:
    if hasattr(inputs, "to"):
        return inputs.to(device)
    return {key: value.to(device) if hasattr(value, "to") else value for key, value in inputs.items()}


class SigLIP2Runtime:
    def __init__(self) -> None:
        model_name = os.environ.get("TIMICH_SIGLIP2_MODEL", MODEL).strip() or MODEL
        self.text_template = os.environ.get("TIMICH_SIGLIP2_TEXT_TEMPLATE", "").strip()
        self.device = choose_device()
        print(f"loading SigLIP 2 model: {model_name} on {self.device}", file=sys.stderr, flush=True)
        self.processor = AutoProcessor.from_pretrained(model_name)
        self.model = AutoModel.from_pretrained(model_name).eval().to(self.device)
        print("SigLIP 2 server ready", file=sys.stderr, flush=True)

    def embed_text(self, text: str, expected_dim: int) -> list[float]:
        text = self.format_text(text)
        inputs = self.processor(text=[text], padding="max_length", max_length=64, truncation=True, return_tensors="pt")
        inputs = move_inputs(inputs, self.device)
        with torch.no_grad():
            embedding = self.model.get_text_features(**inputs)
        return to_vector(embedding, expected_dim)

    def embed_image(self, raw: bytes, expected_dim: int) -> list[float]:
        image = Image.open(BytesIO(raw)).convert("RGB")
        inputs = self.processor(images=[image], return_tensors="pt")
        inputs = move_inputs(inputs, self.device)
        with torch.no_grad():
            embedding = self.model.get_image_features(**inputs)
        return to_vector(embedding, expected_dim)

    def format_text(self, text: str) -> str:
        if not self.text_template:
            return text
        return self.text_template.replace("{query}", text)


class Handler(BaseHTTPRequestHandler):
    server: "SigLIP2HTTPServer"

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
            print(f"{self.path} failed: {error}", file=sys.stderr, flush=True)
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


class SigLIP2HTTPServer(ThreadingHTTPServer):
    def __init__(self, address: tuple[str, int], runtime: SigLIP2Runtime) -> None:
        super().__init__(address, Handler)
        self.runtime = runtime


def main() -> int:
    host = os.environ.get("TIMICH_SIGLIP2_HOST", DEFAULT_HOST)
    port = int(os.environ.get("TIMICH_SIGLIP2_PORT", str(DEFAULT_PORT)))
    runtime = SigLIP2Runtime()
    server = SigLIP2HTTPServer((host, port), runtime)
    print(f"listening on http://{host}:{port}", file=sys.stderr, flush=True)
    server.serve_forever()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
