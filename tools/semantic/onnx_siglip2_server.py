#!/usr/bin/env python3
"""Local ONNX SigLIP 2 embedding server for Timich dev testing."""

from __future__ import annotations

import base64
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from io import BytesIO
import json
import math
import os
import sys
from typing import Any

import numpy as np
import onnxruntime as ort
from PIL import Image

try:
    from transformers import AutoProcessor
except ModuleNotFoundError:  # OpenVINO runtime images may not bundle transformers.
    AutoProcessor = None  # type: ignore[assignment]


PROTOCOL_VERSION = 1
DEFAULT_HOST = "127.0.0.1"
DEFAULT_PORT = 19188
DEFAULT_PROCESSOR = "google/siglip2-base-patch16-224"
DEFAULT_TEXT_MODEL = "/private/tmp/timich-siglip2-onnx-int8/siglip2_text_int8_dynamic.onnx"
DEFAULT_IMAGE_MODEL = "/private/tmp/timich-siglip2-onnx-int8/siglip2_image_int8_dynamic.onnx"


def env_bool(name: str) -> bool:
    return os.environ.get(name, "").strip().lower() in {"1", "true", "yes", "on"}


def providers_from_env(kind: str) -> list[Any]:
    key = f"TIMICH_ONNX_SIGLIP2_{kind.upper()}_PROVIDER"
    raw = os.environ.get(key, os.environ.get("TIMICH_ONNX_SIGLIP2_PROVIDER", "cpu")).strip()
    provider = raw.lower()
    if provider in {"", "cpu", "ort_cpu"}:
        return ["CPUExecutionProvider"]
    if provider.startswith("openvino"):
        device = "GPU"
        if ":" in raw:
            device = raw.split(":", 1)[1].strip() or device
        elif "_" in provider:
            device = provider.rsplit("_", 1)[1].upper()
        return [("OpenVINOExecutionProvider", {"device_type": device.upper()}), "CPUExecutionProvider"]
    raise ValueError(f"unsupported {key} value: {raw}")


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


def session_options() -> ort.SessionOptions:
    options = ort.SessionOptions()
    options.graph_optimization_level = ort.GraphOptimizationLevel.ORT_ENABLE_ALL
    return options


def preprocess_siglip2_image(raw: bytes) -> np.ndarray:
    size = int(os.environ.get("TIMICH_ONNX_SIGLIP2_IMAGE_SIZE", "224"))
    image = Image.open(BytesIO(raw)).convert("RGB")
    image = image.resize((size, size), Image.Resampling.BICUBIC)
    pixels = np.asarray(image, dtype=np.float32) / 255.0
    pixels = (pixels - 0.5) / 0.5
    return np.transpose(pixels, (2, 0, 1))[None, :, :, :].astype(np.float32)


class ONNXSigLIP2Runtime:
    def __init__(self) -> None:
        processor_name = os.environ.get("TIMICH_ONNX_SIGLIP2_PROCESSOR", DEFAULT_PROCESSOR).strip() or DEFAULT_PROCESSOR
        cache_dir = os.environ.get("TIMICH_ONNX_SIGLIP2_CACHE", "").strip() or None
        text_model = os.environ.get("TIMICH_ONNX_SIGLIP2_TEXT_MODEL", DEFAULT_TEXT_MODEL).strip() or DEFAULT_TEXT_MODEL
        image_model = os.environ.get("TIMICH_ONNX_SIGLIP2_IMAGE_MODEL", DEFAULT_IMAGE_MODEL).strip() or DEFAULT_IMAGE_MODEL
        self.text_template = os.environ.get("TIMICH_ONNX_SIGLIP2_TEXT_TEMPLATE", "").strip()
        self.image_preprocess = os.environ.get("TIMICH_ONNX_SIGLIP2_IMAGE_PREPROCESS", "processor").strip().lower()
        skip_text = env_bool("TIMICH_ONNX_SIGLIP2_SKIP_TEXT")
        text_providers = providers_from_env("text")
        image_providers = providers_from_env("image")
        print(f"available ONNX providers: {ort.get_available_providers()}", file=sys.stderr, flush=True)
        self.processor = None
        if self.image_preprocess != "manual" or not skip_text:
            if AutoProcessor is None:
                raise RuntimeError("transformers is required unless TIMICH_ONNX_SIGLIP2_SKIP_TEXT=1 and TIMICH_ONNX_SIGLIP2_IMAGE_PREPROCESS=manual")
            print(f"loading ONNX SigLIP 2 processor: {processor_name}", file=sys.stderr, flush=True)
            self.processor = AutoProcessor.from_pretrained(processor_name, cache_dir=cache_dir)
        self.text_session = None
        if skip_text:
            print("skipping ONNX SigLIP 2 text model load", file=sys.stderr, flush=True)
        else:
            print(f"loading ONNX SigLIP 2 text model: {text_model} providers={text_providers}", file=sys.stderr, flush=True)
            self.text_session = ort.InferenceSession(text_model, sess_options=session_options(), providers=text_providers)
            print(f"text ONNX providers: {self.text_session.get_providers()}", file=sys.stderr, flush=True)
        print(f"loading ONNX SigLIP 2 image model: {image_model} providers={image_providers}", file=sys.stderr, flush=True)
        self.image_session = ort.InferenceSession(image_model, sess_options=session_options(), providers=image_providers)
        print(f"image ONNX providers: {self.image_session.get_providers()}", file=sys.stderr, flush=True)
        print("ONNX SigLIP 2 server ready", file=sys.stderr, flush=True)

    def embed_text(self, text: str, expected_dim: int) -> list[float]:
        if self.processor is None or self.text_session is None:
            raise RuntimeError("text embedding is unavailable in image-only mode")
        text = self.format_text(text)
        inputs = self.processor(text=[text], padding="max_length", max_length=64, truncation=True, return_tensors="np")
        input_ids = np.asarray(inputs["input_ids"], dtype=np.int64)
        embedding = self.text_session.run(None, {"input_ids": input_ids})[0]
        return to_vector(embedding, expected_dim)

    def embed_image(self, raw: bytes, expected_dim: int) -> list[float]:
        if self.image_preprocess == "manual":
            pixel_values = preprocess_siglip2_image(raw)
        else:
            if self.processor is None:
                raise RuntimeError("processor image preprocessing is unavailable")
            image = Image.open(BytesIO(raw)).convert("RGB")
            inputs = self.processor(images=[image], return_tensors="np")
            pixel_values = np.asarray(inputs["pixel_values"], dtype=np.float32)
        embedding = self.image_session.run(None, {"pixel_values": pixel_values})[0]
        return to_vector(embedding, expected_dim)

    def format_text(self, text: str) -> str:
        if not self.text_template:
            return text
        return self.text_template.replace("{query}", text)


class Handler(BaseHTTPRequestHandler):
    server: "ONNXSigLIP2HTTPServer"

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


class ONNXSigLIP2HTTPServer(ThreadingHTTPServer):
    def __init__(self, address: tuple[str, int], runtime: ONNXSigLIP2Runtime) -> None:
        super().__init__(address, Handler)
        self.runtime = runtime


def main() -> int:
    host = os.environ.get("TIMICH_ONNX_SIGLIP2_HOST", DEFAULT_HOST)
    port = int(os.environ.get("TIMICH_ONNX_SIGLIP2_PORT", str(DEFAULT_PORT)))
    runtime = ONNXSigLIP2Runtime()
    server = ONNXSigLIP2HTTPServer((host, port), runtime)
    print(f"listening on http://{host}:{port}", file=sys.stderr, flush=True)
    server.serve_forever()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
