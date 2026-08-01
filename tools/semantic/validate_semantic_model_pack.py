#!/usr/bin/env python3
"""Validate a Timich semantic model pack release artifact."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import shutil
import stat
import subprocess
import tempfile
import zipfile

from semantic_archive_budget import SemanticArchiveBudgetError, preflight_semantic_archive_directory


LAYOUT_PRODUCT = "timich-semantic-model-pack"
ARTIFACT_PRODUCT = "timich-semantic-model-pack-artifact"
REGISTRY_PRODUCT = "timich-semantic-models"
MAX_EXTRACT_SIZE = 8 << 30


def main() -> int:
    args = parse_args()
    artifact = resolve_artifact(args).resolve()
    artifact_sha = sha256_file(artifact)
    artifact_size = artifact.stat().st_size

    sha_path = resolve_sidecar(args.sha256_file, artifact.with_suffix(artifact.suffix + ".sha256"))
    validate_sha256_file(sha_path, artifact.name, artifact_sha)

    model_id_hint = artifact.name.removesuffix(".zip")
    metadata_path = resolve_sidecar(args.metadata, artifact.parent / f"{model_id_hint}.metadata.json")
    metadata = read_json(metadata_path)
    model_pack = validate_metadata(metadata, artifact, artifact_sha, artifact_size)
    validate_expected_owner(model_pack, args)

    sbom_filename = str(model_pack.get("sbom", {}).get("filename") or f"{model_pack['id']}.spdx.json")
    sbom_path = resolve_sidecar(args.sbom, artifact.parent / sbom_filename)
    validate_sbom(sbom_path, model_pack.get("sbom", {}))

    registry_path = args.registry or artifact.parent / f"{model_pack['id']}.registry.json"
    registry_validated = False
    if registry_path.is_file():
        validate_registry(read_json(registry_path), model_pack, args.allow_non_recommended, args.expected_platform)
        registry_validated = True

    signature_path = args.signature
    signature_metadata = model_pack.get("signature") or {}
    if signature_path is None and signature_metadata.get("filename"):
        signature_path = artifact.parent / str(signature_metadata["filename"])
    signature_verified = False
    if args.require_signature and signature_path is None:
        raise SystemExit("signature is required but no signature sidecar was found")
    if signature_path is not None:
        validate_signature_sidecar(signature_path, signature_metadata)
        if args.public_key is not None:
            verify_signature(args.public_key, signature_path, artifact)
            signature_verified = True
        elif args.require_signature:
            raise SystemExit("signature verification requires --public-key")

    zip_report = validate_zip(artifact)
    with tempfile.TemporaryDirectory(prefix="timich-model-pack-") as temp_dir:
        extraction_root = Path(temp_dir) / "model"
        extract_zip_safely(artifact, extraction_root)
        layout_report = validate_layout(extraction_root, model_pack)

    print(json.dumps({
        "status": "ok",
        "artifact": str(artifact),
        "sha256": artifact_sha,
        "sizeBytes": artifact_size,
        "metadata": str(metadata_path),
        "sbom": str(sbom_path),
        "registryValidated": registry_validated,
        "signatureVerified": signature_verified,
        "zip": zip_report,
        "layout": layout_report,
    }, indent=2, sort_keys=True))
    return 0


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Validate a Timich semantic model pack artifact.")
    parser.add_argument("--artifact", type=Path, default=path_env("TIMICH_MODEL_PACK_ARTIFACT"))
    parser.add_argument("--output-dir", type=Path, default=Path(env("TIMICH_MODEL_PACK_OUTPUT_DIR", "dist/semantic-model-packs")))
    parser.add_argument("--sha256-file", type=Path, default=path_env("TIMICH_MODEL_PACK_SHA256_FILE"))
    parser.add_argument("--metadata", type=Path, default=path_env("TIMICH_MODEL_PACK_METADATA"))
    parser.add_argument("--sbom", type=Path, default=path_env("TIMICH_MODEL_PACK_SBOM"))
    parser.add_argument("--registry", type=Path, default=path_env("TIMICH_MODEL_PACK_REGISTRY"))
    parser.add_argument("--signature", type=Path, default=path_env("TIMICH_MODEL_PACK_SIGNATURE"))
    parser.add_argument("--public-key", type=Path, default=path_env("TIMICH_MODEL_PACK_PUBLIC_KEY"))
    parser.add_argument("--require-signature", action="store_true", default=bool_env("TIMICH_MODEL_PACK_REQUIRE_SIGNATURE"))
    parser.add_argument("--allow-non-recommended", action="store_true")
    parser.add_argument("--expected-id")
    parser.add_argument("--expected-name")
    parser.add_argument("--expected-version")
    parser.add_argument("--expected-vector-space-id")
    parser.add_argument("--expected-embedding-dim", type=int)
    parser.add_argument("--expected-input-kind")
    parser.add_argument("--expected-runtime")
    parser.add_argument("--expected-platform")
    return parser.parse_args()


def env(name: str, default: str) -> str:
    value = os.environ.get(name, "").strip()
    return value if value else default


def path_env(name: str) -> Path | None:
    value = os.environ.get(name, "").strip()
    return Path(value) if value else None


def bool_env(name: str) -> bool:
    return os.environ.get(name, "").strip().lower() in {"1", "true", "yes", "on"}


def resolve_artifact(args: argparse.Namespace) -> Path:
    if args.artifact is not None:
        if not args.artifact.is_file():
            raise SystemExit(f"model pack artifact is missing: {args.artifact}")
        return args.artifact
    candidates = sorted(args.output_dir.glob("*.zip"), key=lambda path: path.stat().st_mtime, reverse=True)
    if not candidates:
        raise SystemExit(f"no model pack artifacts found in {args.output_dir}")
    return candidates[0]


def resolve_sidecar(explicit: Path | None, default_path: Path) -> Path:
    path = explicit or default_path
    if not path.is_file():
        raise SystemExit(f"required sidecar is missing: {path}")
    return path


def read_json(path: Path) -> dict:
    try:
        with path.open("r", encoding="utf-8") as handle:
            payload = json.load(handle)
    except json.JSONDecodeError as error:
        raise SystemExit(f"{path} is not valid JSON: {error}") from error
    if not isinstance(payload, dict):
        raise SystemExit(f"{path} must contain a JSON object")
    return payload


def validate_sha256_file(path: Path, artifact_name: str, artifact_sha: str) -> None:
    lines = [line.strip() for line in path.read_text(encoding="utf-8").splitlines() if line.strip()]
    if len(lines) != 1:
        raise SystemExit(f"{path} must contain exactly one checksum line")
    parts = lines[0].split()
    if len(parts) != 2:
        raise SystemExit(f"{path} must use '<sha256>  <filename>' format")
    if parts[0].lower() != artifact_sha:
        raise SystemExit(f"{path} checksum does not match artifact: got {parts[0].lower()} want {artifact_sha}")
    if parts[1] != artifact_name:
        raise SystemExit(f"{path} filename is {parts[1]!r}, want {artifact_name!r}")


def validate_metadata(metadata: dict, artifact: Path, artifact_sha: str, artifact_size: int) -> dict:
    if metadata.get("schemaVersion") != 1:
        raise SystemExit("metadata schemaVersion must be 1")
    if metadata.get("product") != ARTIFACT_PRODUCT:
        raise SystemExit("metadata product is invalid")
    model_pack = metadata.get("modelPack")
    if not isinstance(model_pack, dict):
        raise SystemExit("metadata modelPack object is required")
    for key in ("id", "name", "version", "vectorSpaceId", "embeddingDim", "inputKind", "runtime", "artifact", "sbom"):
        if model_pack.get(key) in (None, ""):
            raise SystemExit(f"metadata modelPack.{key} is required")
    if model_pack["inputKind"] != "image":
        raise SystemExit("metadata inputKind must be image")
    if int(model_pack["embeddingDim"]) <= 0:
        raise SystemExit("metadata embeddingDim must be positive")
    artifact_meta = model_pack.get("artifact")
    if not isinstance(artifact_meta, dict):
        raise SystemExit("metadata modelPack.artifact object is required")
    if artifact_meta.get("filename") != artifact.name:
        raise SystemExit(f"metadata artifact filename is {artifact_meta.get('filename')!r}, want {artifact.name!r}")
    if str(artifact_meta.get("sha256", "")).lower() != artifact_sha:
        raise SystemExit("metadata artifact sha256 does not match artifact")
    if int(artifact_meta.get("sizeBytes") or 0) != artifact_size:
        raise SystemExit("metadata artifact sizeBytes does not match artifact")
    return model_pack


def validate_expected_owner(model_pack: dict, args: argparse.Namespace) -> None:
    expected_values = {
        "id": args.expected_id,
        "name": args.expected_name,
        "version": args.expected_version,
        "vectorSpaceId": args.expected_vector_space_id,
        "inputKind": args.expected_input_kind,
        "runtime": args.expected_runtime,
    }
    for key, expected in expected_values.items():
        if expected is not None and str(model_pack.get(key) or "").strip() != str(expected).strip():
            raise SystemExit(f"metadata modelPack.{key} does not match expected owner")
    if args.expected_embedding_dim is not None and int(model_pack.get("embeddingDim") or 0) != args.expected_embedding_dim:
        raise SystemExit("metadata modelPack.embeddingDim does not match expected owner")


def validate_sbom(path: Path, sbom_metadata: dict) -> None:
    sbom = read_json(path)
    if sbom.get("spdxVersion") != "SPDX-2.3":
        raise SystemExit("SBOM spdxVersion must be SPDX-2.3")
    packages = sbom.get("packages")
    if not isinstance(packages, list) or not packages:
        raise SystemExit("SBOM packages must not be empty")
    if not any(package.get("SPDXID") == "SPDXRef-Package-ModelPack" for package in packages if isinstance(package, dict)):
        raise SystemExit("SBOM is missing SPDXRef-Package-ModelPack")
    expected_sha = str(sbom_metadata.get("sha256") or "").lower()
    if expected_sha and expected_sha != sha256_file(path):
        raise SystemExit("metadata SBOM sha256 does not match SBOM file")
    expected_size = int(sbom_metadata.get("sizeBytes") or 0)
    if expected_size and expected_size != path.stat().st_size:
        raise SystemExit("metadata SBOM sizeBytes does not match SBOM file")


def validate_registry(
    registry: dict,
    model_pack: dict,
    allow_non_recommended: bool = False,
    expected_platform: str | None = None,
) -> None:
    if registry.get("schemaVersion") != 1:
        raise SystemExit("registry schemaVersion must be 1")
    if registry.get("product") != REGISTRY_PRODUCT:
        raise SystemExit("registry product is invalid")
    if not allow_non_recommended and registry.get("recommended") != model_pack["id"]:
        raise SystemExit("registry recommended model does not match metadata")
    models = registry.get("models")
    if not isinstance(models, list):
        raise SystemExit("registry models must be a list")
    model = next((item for item in models if isinstance(item, dict) and item.get("id") == model_pack["id"]), None)
    if model is None:
        raise SystemExit("registry does not contain the metadata model pack")
    for key in ("version", "vectorSpaceId", "inputKind", "runtime"):
        if str(model.get(key) or "").strip() != str(model_pack.get(key) or "").strip():
            raise SystemExit(f"registry model {key} does not match metadata")
    if int(model.get("embeddingDim") or 0) != int(model_pack.get("embeddingDim") or 0):
        raise SystemExit("registry model embeddingDim does not match metadata")
    metadata_artifact = model_pack["artifact"]
    if expected_platform is not None:
        artifact = (model.get("artifacts") or {}).get(expected_platform)
    else:
        artifact = next(
            (
                item
                for item in (model.get("artifacts") or {}).values()
                if isinstance(item, dict) and item.get("filename") == metadata_artifact["filename"]
            ),
            None,
        )
    if not isinstance(artifact, dict):
        raise SystemExit("registry does not contain the metadata model artifact")
    if artifact.get("filename") != metadata_artifact["filename"]:
        raise SystemExit("registry artifact filename does not match metadata")
    if str(artifact.get("sha256", "")).lower() != str(metadata_artifact["sha256"]).lower():
        raise SystemExit("registry artifact sha256 does not match metadata")


def validate_signature_sidecar(path: Path, signature_metadata: dict) -> None:
    if not path.is_file():
        raise SystemExit(f"signature sidecar is missing: {path}")
    expected_sha = str(signature_metadata.get("sha256") or "").lower()
    if expected_sha and expected_sha != sha256_file(path):
        raise SystemExit("metadata signature sha256 does not match signature file")
    expected_size = int(signature_metadata.get("sizeBytes") or 0)
    if expected_size and expected_size != path.stat().st_size:
        raise SystemExit("metadata signature sizeBytes does not match signature file")


def verify_signature(public_key: Path, signature: Path, artifact: Path) -> None:
    if not public_key.is_file():
        raise SystemExit(f"public key is missing: {public_key}")
    try:
        subprocess.run(
            ["openssl", "dgst", "-sha256", "-verify", str(public_key), "-signature", str(signature), str(artifact)],
            check=True,
            capture_output=True,
            text=True,
        )
    except FileNotFoundError as error:
        raise SystemExit("openssl is required to verify model pack signatures") from error
    except subprocess.CalledProcessError as error:
        raise SystemExit(f"signature verification failed: {error.stderr.strip() or error.stdout.strip()}") from error


def validate_zip(path: Path) -> dict:
    try:
        preflight_semantic_archive_directory(path)
    except SemanticArchiveBudgetError as error:
        raise SystemExit(str(error)) from error
    entries = 0
    uncompressed_size = 0
    with zipfile.ZipFile(path) as archive:
        for info in archive.infolist():
            validate_zip_entry(info)
            if info.is_dir():
                continue
            entries += 1
            uncompressed_size += int(info.file_size)
            if uncompressed_size > MAX_EXTRACT_SIZE:
                raise SystemExit("model pack uncompressed size exceeds limit")
    if entries == 0:
        raise SystemExit("model pack zip contains no files")
    return {"fileCount": entries, "uncompressedSizeBytes": uncompressed_size}


def validate_zip_entry(info: zipfile.ZipInfo) -> None:
    relative_parts(info.filename)
    mode = (info.external_attr >> 16) & 0xFFFF
    if stat.S_IFMT(mode) == stat.S_IFLNK:
        raise SystemExit(f"model pack zip entry is a symlink: {info.filename}")


def extract_zip_safely(path: Path, destination: Path) -> None:
    destination.mkdir(parents=True, exist_ok=True)
    with zipfile.ZipFile(path) as archive:
        for info in archive.infolist():
            parts = relative_parts(info.filename)
            target = destination.joinpath(*parts)
            if info.is_dir():
                target.mkdir(parents=True, exist_ok=True)
                continue
            target.parent.mkdir(parents=True, exist_ok=True)
            with archive.open(info) as source, target.open("wb") as output:
                shutil.copyfileobj(source, output, length=1024 * 1024)
            mode = (info.external_attr >> 16) & 0o777
            if mode:
                target.chmod(mode)


def validate_layout(root: Path, model_pack: dict) -> dict:
    layout = read_json(root / "timich-model.json")
    if layout.get("schemaVersion") != 1:
        raise SystemExit("timich-model.json schemaVersion must be 1")
    if layout.get("product") != LAYOUT_PRODUCT:
        raise SystemExit("timich-model.json product is invalid")
    for key in ("modelId", "vectorSpaceId", "embeddingDim", "inputKind", "runtime", "imageModel", "textModel", "tokenizer"):
        if layout.get(key) in (None, ""):
            raise SystemExit(f"timich-model.json {key} is required")
    if layout["modelId"] != model_pack["id"]:
        raise SystemExit("timich-model.json modelId does not match metadata")
    if layout["vectorSpaceId"] != model_pack["vectorSpaceId"]:
        raise SystemExit("timich-model.json vectorSpaceId does not match metadata")
    if int(layout["embeddingDim"]) != int(model_pack["embeddingDim"]):
        raise SystemExit("timich-model.json embeddingDim does not match metadata")
    if layout["inputKind"] != model_pack["inputKind"]:
        raise SystemExit("timich-model.json inputKind does not match metadata")
    if layout["runtime"] != model_pack["runtime"]:
        raise SystemExit("timich-model.json runtime does not match metadata")
    for key in ("imageModel", "textModel", "tokenizer"):
        relative = str(layout[key])
        if not root.joinpath(*relative_parts(relative)).is_file():
            raise SystemExit(f"timich-model.json {key} is missing: {relative}")
    return {
        "modelId": layout["modelId"],
        "vectorSpaceId": layout["vectorSpaceId"],
        "embeddingDim": int(layout["embeddingDim"]),
        "runtime": layout["runtime"],
    }


def relative_parts(raw: str) -> tuple[str, ...]:
    if not raw or "\\" in raw:
        raise SystemExit(f"unsafe model pack path: {raw!r}")
    path = PurePosixPath(raw)
    if path.is_absolute():
        raise SystemExit(f"unsafe model pack path: {raw!r}")
    parts = path.parts
    if not parts or any(part in {"", ".", ".."} for part in parts):
        raise SystemExit(f"unsafe model pack path: {raw!r}")
    return tuple(parts)


def sha256_file(path: Path) -> str:
    hasher = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            hasher.update(chunk)
    return hasher.hexdigest()


if __name__ == "__main__":
    raise SystemExit(main())
