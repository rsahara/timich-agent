#!/usr/bin/env python3
"""Create a Timich SigLIP 2 ONNX semantic model pack from exported artifacts."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess
import uuid
import zipfile


VERSION = "2026.06"
PRODUCT = "timich-semantic-model-pack"
ARTIFACT_PRODUCT = "timich-semantic-model-pack-artifact"
REGISTRY_PRODUCT = "timich-semantic-models"
DEFAULT_MODEL_ID = "timich-siglip2-base-patch16-224-onnx-multilingual-v1"
DEFAULT_MODEL_NAME = "Timich SigLIP 2 Base Patch16 224 ONNX Multilingual"
DEFAULT_UPSTREAM_MODEL = "google/siglip2-base-patch16-224"
DEFAULT_ZIP_MTIME = (2026, 1, 1, 0, 0, 0)


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def processor_marker(processor_dir: Path) -> Path:
    for name in ("tokenizer.json", "preprocessor_config.json", "processor_config.json"):
        candidate = processor_dir / name
        if candidate.is_file():
            return candidate
    raise FileNotFoundError("processor directory must contain tokenizer.json, preprocessor_config.json, or processor_config.json")


def add_file(archive: zipfile.ZipFile, source: Path, target: str) -> None:
    info = zipfile.ZipInfo(target, DEFAULT_ZIP_MTIME)
    info.external_attr = (stat.S_IMODE(source.stat().st_mode) & 0o777) << 16
    info.compress_type = zipfile.ZIP_DEFLATED
    with source.open("rb") as handle, archive.open(info, "w", force_zip64=True) as output:
        shutil.copyfileobj(handle, output, length=1024 * 1024)


def add_processor_dir(archive: zipfile.ZipFile, processor_dir: Path) -> None:
    for path in sorted(processor_dir.rglob("*")):
        if path.is_dir():
            continue
        relative = path.relative_to(processor_dir).as_posix()
        add_file(archive, path, f"tokenizer/{relative}")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--output-dir", type=Path, default=Path(env("TIMICH_MODEL_PACK_OUTPUT_DIR", "dist/semantic-model-packs")))
    parser.add_argument("--base-url", default=env("TIMICH_MODEL_PACK_BASE_URL", ""))
    parser.add_argument("--image-model", type=Path, default=path_env("TIMICH_MODEL_PACK_IMAGE_MODEL"))
    parser.add_argument("--text-model", type=Path, default=path_env("TIMICH_MODEL_PACK_TEXT_MODEL"))
    parser.add_argument("--processor-dir", type=Path, default=path_env("TIMICH_MODEL_PACK_PROCESSOR_DIR"))
    parser.add_argument("--model-id", default=env("TIMICH_MODEL_PACK_ID", DEFAULT_MODEL_ID))
    parser.add_argument("--name", default=env("TIMICH_MODEL_PACK_NAME", DEFAULT_MODEL_NAME))
    parser.add_argument("--upstream-model", default=env("TIMICH_MODEL_PACK_UPSTREAM_MODEL", DEFAULT_UPSTREAM_MODEL))
    parser.add_argument("--version", default=env("TIMICH_MODEL_PACK_VERSION", VERSION))
    parser.add_argument("--embedding-dim", type=int, default=int(env("TIMICH_MODEL_PACK_EMBEDDING_DIM", "768")))
    parser.add_argument("--quantization", default=env("TIMICH_MODEL_PACK_QUANTIZATION", "fp32"))
    parser.add_argument("--license", default=env("TIMICH_MODEL_PACK_LICENSE", ""))
    parser.add_argument("--manifest-name", default="")
    parser.add_argument("--signing-key", type=Path, default=path_env("TIMICH_MODEL_PACK_SIGNING_KEY"))
    args = parser.parse_args()

    if args.image_model is None or not args.image_model.is_file():
        raise SystemExit("image ONNX model is required; set SEMANTIC_MODEL_PACK_IMAGE_MODEL or --image-model")
    if args.text_model is None or not args.text_model.is_file():
        raise SystemExit("text ONNX model is required; set SEMANTIC_MODEL_PACK_TEXT_MODEL or --text-model")
    if args.processor_dir is None or not args.processor_dir.is_dir():
        raise SystemExit("processor directory is required; set SEMANTIC_MODEL_PACK_PROCESSOR_DIR or --processor-dir")
    if not str(args.base_url).strip():
        raise SystemExit("model pack base URL is required; set SEMANTIC_MODEL_PACK_BASE_URL or --base-url")

    output_dir = args.output_dir
    output_dir.mkdir(parents=True, exist_ok=True)
    image_model = args.image_model
    text_model = args.text_model
    processor_dir = args.processor_dir
    marker = processor_marker(processor_dir)

    model_id = str(args.model_id)
    embedding_dim = int(args.embedding_dim)
    vector_space_id = f"{model_id}/d{embedding_dim}"
    zip_name = f"{model_id}.zip"
    zip_path = output_dir / zip_name
    license_text = str(args.license).strip()
    if not license_text:
        license_text = f"Apache-2.0 model pack metadata; {args.upstream_model} Apache-2.0"

    layout = {
        "schemaVersion": 1,
        "product": PRODUCT,
        "modelId": model_id,
        "vectorSpaceId": vector_space_id,
        "embeddingDim": embedding_dim,
        "inputKind": "image",
        "runtime": "onnxruntime",
        "imageModel": "models/image.onnx",
        "textModel": "models/text.onnx",
        "tokenizer": f"tokenizer/{marker.relative_to(processor_dir).as_posix()}",
        "metadata": {
            "upstreamModel": str(args.upstream_model),
            "quantization": str(args.quantization),
            "imageModelSha256": sha256_file(image_model),
            "textModelSha256": sha256_file(text_model),
        },
    }

    with zipfile.ZipFile(zip_path, "w", compression=zipfile.ZIP_DEFLATED) as archive:
        info = zipfile.ZipInfo("timich-model.json", DEFAULT_ZIP_MTIME)
        info.external_attr = 0o644 << 16
        info.compress_type = zipfile.ZIP_DEFLATED
        archive.writestr(info, json.dumps(layout, indent=2, sort_keys=True) + "\n")
        add_file(archive, image_model, "models/image.onnx")
        add_file(archive, text_model, "models/text.onnx")
        add_processor_dir(archive, processor_dir)

    zip_size = zip_path.stat().st_size
    zip_sha256 = sha256_file(zip_path)
    checksum_path = output_dir / f"{zip_name}.sha256"
    checksum_path.write_text(f"{zip_sha256}  {zip_name}\n", encoding="utf-8")
    base_url = str(args.base_url).rstrip("/")
    artifact = {
        "filename": zip_name,
        "url": f"{base_url}/{zip_name}",
        "sha256": zip_sha256,
        "sizeBytes": zip_size,
    }
    manifest = {
        "schemaVersion": 1,
        "product": REGISTRY_PRODUCT,
        "version": str(args.version),
        "recommended": model_id,
        "models": [
            {
                "id": model_id,
                "name": str(args.name),
                "version": str(args.version),
                "vectorSpaceId": vector_space_id,
                "embeddingDim": embedding_dim,
                "inputKind": "image",
                "queryLanguages": ["multilingual"],
                "runtime": "onnxruntime",
                "quantization": str(args.quantization),
                "license": license_text,
                "artifacts": {"default": artifact},
            }
        ],
    }
    manifest_name = str(args.manifest_name).strip() or f"{model_id}.registry.json"
    manifest_path = output_dir / manifest_name
    manifest_path.write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="utf-8")

    sbom_path = output_dir / f"{model_id}.spdx.json"
    sbom = build_spdx(args, zip_path)
    sbom_path.write_text(json.dumps(sbom, indent=2, sort_keys=True) + "\n", encoding="utf-8")

    signature = None
    if args.signing_key:
        signature_path = output_dir / f"{zip_name}.sig"
        sign_artifact(args.signing_key, zip_path, signature_path)
        signature = {
            "algorithm": "openssl-dgst-sha256-rsa",
            "filename": signature_path.name,
            "sizeBytes": signature_path.stat().st_size,
            "sha256": sha256_file(signature_path),
        }

    metadata_path = output_dir / f"{model_id}.metadata.json"
    metadata = {
        "schemaVersion": 1,
        "product": ARTIFACT_PRODUCT,
        "modelPack": {
            "id": model_id,
            "name": str(args.name),
            "version": str(args.version),
            "vectorSpaceId": vector_space_id,
            "embeddingDim": embedding_dim,
            "inputKind": "image",
            "runtime": "onnxruntime",
            "quantization": str(args.quantization),
            "license": license_text,
            "upstreamModel": str(args.upstream_model),
            "artifact": {
                "filename": zip_name,
                "sizeBytes": zip_size,
                "sha256": zip_sha256,
            },
            "sbom": {
                "filename": sbom_path.name,
                "sizeBytes": sbom_path.stat().st_size,
                "sha256": sha256_file(sbom_path),
            },
        },
    }
    if signature is not None:
        metadata["modelPack"]["signature"] = signature
    metadata_path.write_text(json.dumps(metadata, indent=2, sort_keys=True) + "\n", encoding="utf-8")

    print(json.dumps({
        "manifest": str(manifest_path),
        "artifact": str(zip_path),
        "sha256": str(checksum_path),
        "metadata": str(metadata_path),
        "sbom": str(sbom_path),
        "signed": signature is not None,
    }, sort_keys=True))
    return 0


def env(name: str, default: str) -> str:
    value = os.environ.get(name, "").strip()
    return value if value else default


def path_env(name: str) -> Path | None:
    value = os.environ.get(name, "").strip()
    return Path(value) if value else None


def now_iso() -> str:
    return dt.datetime.now(dt.timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")


def build_spdx(args: argparse.Namespace, artifact_path: Path) -> dict:
    document_id = f"SPDXRef-DOCUMENT-{uuid.uuid4()}"
    model_id = str(args.model_id)
    packages = [
        {
            "name": model_id,
            "SPDXID": "SPDXRef-Package-ModelPack",
            "versionInfo": str(args.version),
            "downloadLocation": "NOASSERTION",
            "filesAnalyzed": False,
            "licenseConcluded": "NOASSERTION",
            "licenseDeclared": str(args.license).strip() or "NOASSERTION",
            "copyrightText": "NOASSERTION",
        },
        {
            "name": str(args.upstream_model),
            "SPDXID": "SPDXRef-Package-UpstreamModel",
            "versionInfo": str(args.version),
            "downloadLocation": "NOASSERTION",
            "filesAnalyzed": False,
            "licenseConcluded": "NOASSERTION",
            "licenseDeclared": "Apache-2.0",
            "copyrightText": "NOASSERTION",
        },
    ]
    return {
        "spdxVersion": "SPDX-2.3",
        "dataLicense": "CC0-1.0",
        "SPDXID": "SPDXRef-DOCUMENT",
        "name": artifact_path.stem,
        "documentNamespace": f"https://timich.app/spdx/{document_id}",
        "creationInfo": {
            "created": now_iso(),
            "creators": ["Tool: timich-semantic-model-pack-builder"],
        },
        "packages": packages,
        "relationships": [
            {
                "spdxElementId": "SPDXRef-DOCUMENT",
                "relationshipType": "DESCRIBES",
                "relatedSpdxElement": "SPDXRef-Package-ModelPack",
            },
            {
                "spdxElementId": "SPDXRef-Package-ModelPack",
                "relationshipType": "CONTAINS",
                "relatedSpdxElement": "SPDXRef-Package-UpstreamModel",
            },
        ],
    }


def sign_artifact(signing_key: Path, artifact: Path, signature_path: Path) -> None:
    if not signing_key.is_file():
        raise SystemExit(f"signing key is missing: {signing_key}")
    try:
        subprocess.run(
            [
                "openssl",
                "dgst",
                "-sha256",
                "-sign",
                str(signing_key),
                "-out",
                str(signature_path),
                str(artifact),
            ],
            check=True,
            capture_output=True,
            text=True,
        )
    except FileNotFoundError as error:
        raise SystemExit("openssl is required to sign model pack artifacts") from error
    except subprocess.CalledProcessError as error:
        raise SystemExit(f"artifact signing failed: {error.stderr.strip() or error.stdout.strip()}") from error


if __name__ == "__main__":
    raise SystemExit(main())
