#!/usr/bin/env python3
"""Create a tiny Timich semantic model pack for the dev SigLIP 2 helper."""

from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path
import zipfile


VERSION = "2026.06.dev"
RUNTIME_TRANSFORMERS_SIGLIP2 = "transformers-siglip2"
RUNTIME_ONNXRUNTIME = "onnxruntime"

VARIANTS = {
    "base-patch16-224": {
        "model_id": "timich-siglip2-base-patch16-224-multilingual-v1",
        "name": "Timich SigLIP 2 Base Patch16 224 Multilingual Dev",
        "upstream_model": "google/siglip2-base-patch16-224",
        "embedding_dim": 768,
        "runtime": RUNTIME_TRANSFORMERS_SIGLIP2,
        "quantization": "fp32",
    },
    "base-patch16-224-openvino-gpu": {
        "model_id": "timich-siglip2-base-patch16-224-openvino-gpu-multilingual-v1",
        "name": "Timich SigLIP 2 Base Patch16 224 OpenVINO GPU Multilingual Dev",
        "upstream_model": "google/siglip2-base-patch16-224",
        "embedding_dim": 768,
        "runtime": RUNTIME_TRANSFORMERS_SIGLIP2,
        "quantization": "fp32-openvino-gpu",
    },
    "base-patch16-224-int8-onnx": {
        "model_id": "timich-siglip2-base-patch16-224-int8-onnx-multilingual-v1",
        "name": "Timich SigLIP 2 Base Patch16 224 ONNX INT8 MatMul Multilingual Dev",
        "upstream_model": "google/siglip2-base-patch16-224",
        "embedding_dim": 768,
        "runtime": RUNTIME_ONNXRUNTIME,
        "quantization": "int8-dynamic-matmul",
    },
    "base-patch32-256": {
        "model_id": "timich-siglip2-base-patch32-256-multilingual-v1",
        "name": "Timich SigLIP 2 Base Patch32 256 Multilingual Dev",
        "upstream_model": "google/siglip2-base-patch32-256",
        "embedding_dim": 768,
        "runtime": RUNTIME_TRANSFORMERS_SIGLIP2,
        "quantization": "fp32",
    },
}


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--output-dir", required=True)
    parser.add_argument("--base-url", required=True)
    parser.add_argument("--variant", choices=sorted(VARIANTS), default="base-patch16-224")
    args = parser.parse_args()

    variant = VARIANTS[args.variant]
    model_id = variant["model_id"]
    upstream_model = variant["upstream_model"]
    embedding_dim = int(variant["embedding_dim"])
    runtime = str(variant["runtime"])
    quantization = str(variant["quantization"])
    vector_space_id = f"{model_id}/d{embedding_dim}"
    zip_name = f"{model_id}.zip"

    output_dir = Path(args.output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)
    zip_path = output_dir / zip_name
    base_url = args.base_url.rstrip("/")

    layout = {
        "schemaVersion": 1,
        "product": "timich-semantic-model-pack",
        "modelId": model_id,
        "vectorSpaceId": vector_space_id,
        "embeddingDim": embedding_dim,
        "inputKind": "image",
        "runtime": runtime,
        "imageModel": "models/image-model.txt",
        "textModel": "models/text-model.txt",
        "tokenizer": "tokenizer/tokenizer.txt",
    }

    with zipfile.ZipFile(zip_path, "w", compression=zipfile.ZIP_DEFLATED) as archive:
        archive.writestr("timich-model.json", json.dumps(layout, indent=2, sort_keys=True))
        archive.writestr("models/image-model.txt", upstream_model + "\n")
        archive.writestr("models/text-model.txt", upstream_model + "\n")
        archive.writestr("tokenizer/tokenizer.txt", upstream_model + "\n")

    zip_bytes = zip_path.read_bytes()
    manifest = {
        "schemaVersion": 1,
        "product": "timich-semantic-models",
        "version": VERSION,
        "recommended": model_id,
        "models": [
            {
                "id": model_id,
                "name": variant["name"],
                "version": VERSION,
                "vectorSpaceId": vector_space_id,
                "embeddingDim": embedding_dim,
                "inputKind": "image",
                "queryLanguages": ["multilingual"],
                "runtime": runtime,
                "quantization": quantization,
                "license": f"Apache-2.0 model pack metadata; {upstream_model} Apache-2.0",
                "artifacts": {
                    "default": {
                        "filename": zip_name,
                        "url": f"{base_url}/{zip_name}",
                        "sha256": hashlib.sha256(zip_bytes).hexdigest(),
                        "sizeBytes": len(zip_bytes),
                    }
                },
            }
        ],
    }
    manifest_path = output_dir / "manifest.json"
    manifest_path.write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8")

    print(json.dumps({"manifest": str(manifest_path), "artifact": str(zip_path)}, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
