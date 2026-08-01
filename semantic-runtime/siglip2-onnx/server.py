#!/usr/bin/env python3
"""Long-lived SigLIP 2 ONNX runtime server for Timich semantic helper calls."""

from __future__ import annotations

import argparse
import base64
import binascii
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from io import BytesIO
import json
import math
import os
from pathlib import Path
import sys
from typing import Any

import numpy as np
import onnxruntime as ort
from PIL import Image, UnidentifiedImageError
from transformers import AutoProcessor


PROTOCOL_VERSION = 1
MODEL_LAYOUT_FILE = "timich-model.json"


class AssetInputError(ValueError):
    """The request contains an image/text value that cannot be embedded."""


def normalize(values: Any) -> list[float]:
    if hasattr(values, "tolist"):
        values = values.tolist()
    if values and isinstance(values[0], list):
        values = values[0]
    vector = [float(value) for value in values]
    norm = math.sqrt(sum(value * value for value in vector))
    if norm == 0:
        return vector
    return [float(value / norm) for value in vector]


def providers_from_name(raw: str) -> list[Any]:
    provider = raw.strip().lower()
    if provider in {"", "cpu", "ort_cpu"}:
        return ["CPUExecutionProvider"]
    if provider.startswith("openvino"):
        device = "GPU"
        if ":" in raw:
            device = raw.split(":", 1)[1].strip() or device
        elif "_" in provider:
            device = provider.rsplit("_", 1)[1].upper()
        return [("OpenVINOExecutionProvider", {"device_type": device.upper()}), "CPUExecutionProvider"]
    raise ValueError(f"unsupported ONNX provider: {raw}")


def session_options() -> ort.SessionOptions:
    options = ort.SessionOptions()
    options.graph_optimization_level = ort.GraphOptimizationLevel.ORT_ENABLE_ALL
    return options


def safe_layout_path(runtime_layout: Path, value: str) -> Path:
    raw = value.strip()
    if not raw or "\\" in raw:
        raise ValueError("runtime layout contains an invalid path")
    path = Path(raw)
    if path.is_absolute() or ".." in path.parts:
        raise ValueError("runtime layout contains an unsafe path")
    resolved = runtime_layout / path
    if not resolved.is_file():
        raise FileNotFoundError(f"runtime file is missing: {raw}")
    return resolved


def read_layout(runtime_layout: Path) -> dict[str, Any]:
    path = runtime_layout / MODEL_LAYOUT_FILE
    with path.open("r", encoding="utf-8") as handle:
        layout = json.load(handle)
    if layout.get("schemaVersion") != 1:
        raise ValueError("timich-model.json schemaVersion must be 1")
    if layout.get("product") != "timich-semantic-model-pack":
        raise ValueError("timich-model.json product is invalid")
    for key in ("modelId", "vectorSpaceId", "embeddingDim", "inputKind", "runtime", "imageModel", "textModel", "tokenizer"):
        if str(layout.get(key, "")).strip() == "":
            raise ValueError(f"timich-model.json {key} is required")
    if layout["runtime"] != "onnxruntime":
        raise ValueError("SigLIP 2 ONNX runtime requires runtime=onnxruntime")
    return layout


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


class SigLIP2ONNXRuntime:
    def __init__(self, runtime_layout: Path, text_provider: str, image_provider: str, text_template: str) -> None:
        self.runtime_layout = runtime_layout
        self.layout = read_layout(runtime_layout)
        self.expected_dim = int(self.layout["embeddingDim"])
        self.text_template = text_template

        image_model = safe_layout_path(runtime_layout, str(self.layout["imageModel"]))
        text_model = safe_layout_path(runtime_layout, str(self.layout["textModel"]))
        tokenizer_marker = safe_layout_path(runtime_layout, str(self.layout["tokenizer"]))
        processor_path = tokenizer_marker.parent

        print(f"available ONNX providers: {ort.get_available_providers()}", file=sys.stderr, flush=True)
        print(f"loading SigLIP 2 processor: {processor_path}", file=sys.stderr, flush=True)
        self.processor = AutoProcessor.from_pretrained(str(processor_path))

        text_providers = providers_from_name(text_provider)
        image_providers = providers_from_name(image_provider)
        print(f"loading SigLIP 2 text ONNX: {text_model} providers={text_providers}", file=sys.stderr, flush=True)
        self.text_session = ort.InferenceSession(str(text_model), sess_options=session_options(), providers=text_providers)
        print(f"text ONNX providers: {self.text_session.get_providers()}", file=sys.stderr, flush=True)
        print(f"loading SigLIP 2 image ONNX: {image_model} providers={image_providers}", file=sys.stderr, flush=True)
        self.image_session = ort.InferenceSession(str(image_model), sess_options=session_options(), providers=image_providers)
        print(f"image ONNX providers: {self.image_session.get_providers()}", file=sys.stderr, flush=True)

    def inspect(self) -> dict[str, Any]:
        return identity_response(self.layout)

    def embed_text(self, text: str) -> list[float]:
        if self.text_template:
            text = self.text_template.replace("{query}", text)
        inputs = self.processor(text=[text], padding="max_length", max_length=64, truncation=True, return_tensors="np")
        input_ids = np.asarray(inputs["input_ids"], dtype=np.int64)
        embedding = self.text_session.run(None, {"input_ids": input_ids})[0]
        vector = normalize(embedding)
        if len(vector) != self.expected_dim:
            raise ValueError(f"text embedding dimension {len(vector)} does not match {self.expected_dim}")
        return vector

    def embed_image(self, raw: bytes) -> list[float]:
        try:
            with Image.open(BytesIO(raw)) as source:
                source.load()
                image = source.convert("RGB")
        except (UnidentifiedImageError, OSError, ValueError) as error:
            raise AssetInputError(f"invalid image input: {error}") from error
        inputs = self.processor(images=[image], return_tensors="np")
        pixel_values = np.asarray(inputs["pixel_values"], dtype=np.float32)
        embedding = self.image_session.run(None, {"pixel_values": pixel_values})[0]
        vector = normalize(embedding)
        if len(vector) != self.expected_dim:
            raise ValueError(f"image embedding dimension {len(vector)} does not match {self.expected_dim}")
        return vector


class Handler(BaseHTTPRequestHandler):
    server: "RuntimeHTTPServer"

    def do_GET(self) -> None:
        if self.path == "/healthz":
            self.write_json({"status": "ok", "modelId": self.server.runtime.layout["modelId"]})
            return
        self.send_error(404)

    def do_POST(self) -> None:
        try:
            payload = self.read_json()
            if self.path == "/inspect":
                self.write_json(self.server.runtime.inspect())
                return
            if self.path == "/embed-text":
                text = str(payload.get("text") or "").strip()
                if not text:
                    raise AssetInputError("text is required")
                response = self.server.runtime.inspect()
                response["vector"] = self.server.runtime.embed_text(text)
                response["input"] = "text"
                self.write_json(response)
                return
            if self.path == "/embed-image":
                try:
                    raw = base64.b64decode(str(payload.get("imageBase64") or ""), validate=True)
                except (binascii.Error, ValueError) as error:
                    raise AssetInputError(f"invalid image base64: {error}") from error
                if not raw:
                    raise AssetInputError("image input is required")
                response = self.server.runtime.inspect()
                response["vector"] = self.server.runtime.embed_image(raw)
                response["input"] = str(payload.get("source") or "image").strip() or "image"
                self.write_json(response)
                return
            self.send_error(404)
        except AssetInputError as error:
            print(f"{self.path} rejected asset input: {error}", file=sys.stderr, flush=True)
            self.write_json({"error": str(error), "errorClass": "asset_input"}, status=422)
        except Exception as error:  # noqa: BLE001 - HTTP handler boundary
            print(f"{self.path} failed: {error}", file=sys.stderr, flush=True)
            self.write_json({"error": str(error), "errorClass": "runtime_unavailable"}, status=500)

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


class RuntimeHTTPServer(ThreadingHTTPServer):
    def __init__(self, address: tuple[str, int], runtime: SigLIP2ONNXRuntime) -> None:
        super().__init__(address, Handler)
        self.runtime = runtime


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser()
    parser.add_argument("--runtime-layout", required=True, help="installed Timich semantic model-pack runtime layout")
    parser.add_argument("--host", default=os.environ.get("TIMICH_ONNX_SIGLIP2_HOST", "127.0.0.1"))
    parser.add_argument("--port", type=int, default=int(os.environ.get("TIMICH_ONNX_SIGLIP2_PORT", "19188")))
    parser.add_argument("--provider", default=os.environ.get("TIMICH_ONNX_SIGLIP2_PROVIDER", "cpu"))
    parser.add_argument("--text-provider", default=os.environ.get("TIMICH_ONNX_SIGLIP2_TEXT_PROVIDER", ""))
    parser.add_argument("--image-provider", default=os.environ.get("TIMICH_ONNX_SIGLIP2_IMAGE_PROVIDER", ""))
    parser.add_argument("--text-template", default=os.environ.get("TIMICH_ONNX_SIGLIP2_TEXT_TEMPLATE", ""))
    return parser


def main(argv: list[str]) -> int:
    args = build_parser().parse_args(argv)
    provider = str(args.provider)
    text_provider = str(args.text_provider or provider)
    image_provider = str(args.image_provider or provider)
    runtime = SigLIP2ONNXRuntime(Path(args.runtime_layout), text_provider, image_provider, str(args.text_template))
    server = RuntimeHTTPServer((str(args.host), int(args.port)), runtime)
    print(f"listening on http://{args.host}:{args.port}", file=sys.stderr, flush=True)
    server.serve_forever()
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
