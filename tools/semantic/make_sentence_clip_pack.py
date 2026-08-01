#!/usr/bin/env python3
"""Create a tiny Timich semantic model pack for the dev sentence-CLIP helper.

The zip stores metadata and marker files only. The Python helper downloads and
loads the actual SentenceTransformer models, so this pack is intentionally small
and only lets the Agent install a candidate profile through the normal flow.
"""

from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path
import zipfile


MODEL_ID = "timich-clip-vit-b32-multilingual-v1"
VERSION = "2026.06.dev"
VECTOR_SPACE_ID = f"{MODEL_ID}/d512"
ZIP_NAME = f"{MODEL_ID}.zip"


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--output-dir", required=True)
    parser.add_argument("--base-url", required=True)
    args = parser.parse_args()

    output_dir = Path(args.output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)
    zip_path = output_dir / ZIP_NAME
    base_url = args.base_url.rstrip("/")

    layout = {
        "schemaVersion": 1,
        "product": "timich-semantic-model-pack",
        "modelId": MODEL_ID,
        "vectorSpaceId": VECTOR_SPACE_ID,
        "embeddingDim": 512,
        "inputKind": "image",
        "runtime": "sentence-transformers-clip",
        "imageModel": "models/image-model.txt",
        "textModel": "models/text-model.txt",
        "tokenizer": "tokenizer/tokenizer.txt",
    }

    with zipfile.ZipFile(zip_path, "w", compression=zipfile.ZIP_DEFLATED) as archive:
        archive.writestr("timich-model.json", json.dumps(layout, indent=2, sort_keys=True))
        archive.writestr("models/image-model.txt", "sentence-transformers/clip-ViT-B-32\n")
        archive.writestr(
            "models/text-model.txt",
            "auto: sentence-transformers/clip-ViT-B-32 for ASCII, "
            "sentence-transformers/clip-ViT-B-32-multilingual-v1 otherwise\n",
        )
        archive.writestr("tokenizer/tokenizer.txt", "sentence-transformers/clip-ViT-B-32-multilingual-v1\n")

    zip_bytes = zip_path.read_bytes()
    manifest = {
        "schemaVersion": 1,
        "product": "timich-semantic-models",
        "version": VERSION,
        "recommended": MODEL_ID,
        "models": [
            {
                "id": MODEL_ID,
                "name": "Timich CLIP ViT-B/32 Multilingual Dev",
                "version": VERSION,
                "vectorSpaceId": VECTOR_SPACE_ID,
                "embeddingDim": 512,
                "inputKind": "image",
                "queryLanguages": ["multilingual"],
                "runtime": "sentence-transformers-clip",
                "quantization": "fp32",
                "license": "Apache-2.0 model pack metadata; upstream model licenses apply",
                "artifacts": {
                    "default": {
                        "filename": ZIP_NAME,
                        "url": f"{base_url}/{ZIP_NAME}",
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
