#!/usr/bin/env python3
"""Validate a Timich semantic release registry against local artifacts."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
from urllib.parse import urlparse

from semantic_archive_budget import (
    SemanticArchiveBudgetError,
    enforce_semantic_working_set,
    inspect_semantic_archive,
)


PRODUCT = "timich-semantic-models"
RUNTIME = "onnxruntime"
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
IDENTITY_PART_RE = re.compile(r"^[A-Za-z0-9._-]+$")
MAX_ARTIFACT_SIZE_BYTES = 8 * 1024 * 1024 * 1024


def main() -> int:
    args = parse_args()
    registry = read_json(args.registry)
    validate_header(registry)
    validate_registry_identities(registry)
    validate_artifact_platform_keys(registry)
    validate_artifact_filename_ownership(registry)
    preflight_release_archive_budget(registry, args)

    model_ids = {str(item.get("id", "")).strip() for item in registry.get("models") or [] if isinstance(item, dict)}
    runtime_pack_ids = {str(item.get("id", "")).strip() for item in registry.get("runtimePacks") or [] if isinstance(item, dict)}
    for runtime_pack in registry.get("runtimePacks") or []:
        if isinstance(runtime_pack, dict) and str(runtime_pack.get("runtime") or "").strip() != RUNTIME:
            raise SystemExit(f"runtime pack {runtime_pack.get('id')!r} runtime must be {RUNTIME}")
    recommended = str(registry.get("recommended") or "").strip()
    recommended_runtime_pack = str(registry.get("recommendedRuntimePack") or "").strip()
    if recommended and recommended not in model_ids:
        raise SystemExit(f"recommended model {recommended!r} is not present in models")
    if recommended_runtime_pack and recommended_runtime_pack not in runtime_pack_ids:
        raise SystemExit(f"recommended runtime pack {recommended_runtime_pack!r} is not present in runtimePacks")

    reports: list[dict] = []
    for model in registry.get("models") or []:
        if not isinstance(model, dict):
            raise SystemExit("model entries must be objects")
        reports.extend(validate_artifacts(
            model,
            "model",
            args.model_dir,
            args.base_url,
            args.require_signatures,
            args.registry,
            args.validate_pack_layouts,
        ))
    for runtime_pack in registry.get("runtimePacks") or []:
        if not isinstance(runtime_pack, dict):
            raise SystemExit("runtime pack entries must be objects")
        reports.extend(validate_artifacts(
            runtime_pack,
            "runtime pack",
            args.runtime_pack_dir,
            args.base_url,
            args.require_signatures,
            args.registry,
            args.validate_pack_layouts,
        ))

    if not reports:
        raise SystemExit("semantic registry has no artifacts")
    if args.reject_unreferenced_assets:
        validate_exact_asset_namespace(args, reports)
    print(json.dumps({
        "status": "ok",
        "registry": str(args.registry),
        "artifactCount": len(reports),
        "models": len(model_ids),
        "runtimePacks": len(runtime_pack_ids),
        "requireSignatures": args.require_signatures,
    }, indent=2, sort_keys=True))
    return 0


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Validate semantic-models.json against local release artifacts.")
    parser.add_argument("--registry", type=Path, default=Path(env("SEMANTIC_MODEL_REGISTRY", "dist/semantic-models.json")))
    parser.add_argument("--model-dir", type=Path, default=Path(env("SEMANTIC_MODEL_PACK_DIR", "dist/semantic-model-packs")))
    parser.add_argument("--runtime-pack-dir", type=Path, default=Path(env("SEMANTIC_RUNTIME_PACK_DIR", "dist/semantic-runtime-packs")))
    parser.add_argument("--base-url", default=env("SEMANTIC_RELEASE_BASE_URL", ""))
    parser.add_argument("--require-signatures", action="store_true", default=bool_env("SEMANTIC_RELEASE_REQUIRE_SIGNATURES"))
    parser.add_argument("--validate-pack-layouts", action="store_true")
    parser.add_argument("--reject-unreferenced-assets", action="store_true")
    return parser.parse_args()


def env(name: str, default: str) -> str:
    value = os.environ.get(name, "").strip()
    return value if value else default


def bool_env(name: str) -> bool:
    return os.environ.get(name, "").strip().lower() in {"1", "true", "yes", "on"}


def read_json(path: Path) -> dict:
    try:
        with path.open("r", encoding="utf-8") as handle:
            payload = json.load(handle)
    except OSError as error:
        raise SystemExit(f"could not read {path}: {error}") from error
    except json.JSONDecodeError as error:
        raise SystemExit(f"could not parse {path}: {error}") from error
    if not isinstance(payload, dict):
        raise SystemExit(f"{path} must contain a JSON object")
    return payload


def validate_header(registry: dict) -> None:
    if registry.get("schemaVersion") != 1:
        raise SystemExit("semantic registry schemaVersion must be 1")
    if registry.get("product") != PRODUCT:
        raise SystemExit(f"semantic registry product must be {PRODUCT}")


def validate_registry_identities(registry: dict) -> None:
    model_ids: set[str] = set()
    for index, model in enumerate(registry.get("models") or []):
        if not isinstance(model, dict):
            raise SystemExit("model entries must be objects")
        model_id = required_identity_part(model, "id", f"model {index}")
        if model_id in model_ids:
            raise SystemExit(f"semantic model {model_id!r} is duplicated")
        model_ids.add(model_id)
        required_identity_part(model, "version", f"semantic model {model_id!r}")
    runtime_ids: set[str] = set()
    for index, runtime_pack in enumerate(registry.get("runtimePacks") or []):
        if not isinstance(runtime_pack, dict):
            raise SystemExit("runtime pack entries must be objects")
        pack_id = required_identity_part(runtime_pack, "id", f"runtime pack {index}")
        if pack_id in runtime_ids:
            raise SystemExit(f"semantic runtime pack {pack_id!r} is duplicated")
        runtime_ids.add(pack_id)
        required_identity_part(runtime_pack, "version", f"semantic runtime pack {pack_id!r}")


def validate_artifact_platform_keys(registry: dict) -> None:
    for label, owners in (
        ("model", registry.get("models") or []),
        ("runtime pack", registry.get("runtimePacks") or []),
    ):
        for owner in owners:
            if not isinstance(owner, dict):
                continue
            owner_id = str(owner.get("id") or "").strip()
            artifacts = owner.get("artifacts")
            if not isinstance(artifacts, dict):
                continue
            normalized_platforms: set[str] = set()
            for raw_platform in artifacts:
                platform = str(raw_platform).strip()
                if not platform:
                    raise SystemExit(f"{label} {owner_id!r} artifact platform is required")
                if platform in normalized_platforms:
                    raise SystemExit(
                        f"{label} {owner_id!r} artifact platform {platform!r} "
                        "is duplicated after normalization"
                    )
                normalized_platforms.add(platform)


def validate_artifact_filename_ownership(registry: dict) -> None:
    owners_by_filename: dict[str, str] = {}
    for label, owners in (
        ("model", registry.get("models") or []),
        ("runtime pack", registry.get("runtimePacks") or []),
    ):
        for owner in owners:
            if not isinstance(owner, dict):
                continue
            owner_id = str(owner.get("id") or "").strip()
            owner_key = f"{label}:{owner_id}"
            artifacts = owner.get("artifacts") or {}
            if not isinstance(artifacts, dict):
                continue
            for artifact in artifacts.values():
                if not isinstance(artifact, dict):
                    continue
                filename = str(artifact.get("filename") or "").strip()
                if not filename:
                    continue
                previous_owner = owners_by_filename.get(filename)
                if previous_owner is not None:
                    raise SystemExit(
                        f"semantic artifact filename {filename!r} is referenced more than once "
                        f"by {previous_owner} and {owner_key}"
                    )
                owners_by_filename[filename] = owner_key


def preflight_release_archive_budget(registry: dict, args: argparse.Namespace) -> None:
    archive_stats = {}
    validation_extract_bytes = 0
    for collection_key, artifact_dir in (
        ("models", args.model_dir),
        ("runtimePacks", args.runtime_pack_dir),
    ):
        for owner in registry.get(collection_key) or []:
            if not isinstance(owner, dict):
                continue
            artifacts = owner.get("artifacts") or {}
            if not isinstance(artifacts, dict):
                continue
            for artifact in artifacts.values():
                if not isinstance(artifact, dict):
                    continue
                filename = str(artifact.get("filename") or "").strip()
                if not filename or Path(filename).name != filename:
                    raise SystemExit("semantic artifact filename is required and must be a basename")
                path = (artifact_dir / filename).resolve()
                if not path.is_file():
                    raise SystemExit(f"semantic artifact file is missing: {path}")
                try:
                    stats = inspect_semantic_archive(path)
                except SemanticArchiveBudgetError as error:
                    raise SystemExit(str(error)) from error
                archive_stats[path] = stats
                validation_extract_bytes = max(
                    validation_extract_bytes,
                    stats.uncompressed_size_bytes,
                )

    downloaded_size_bytes = semantic_downloaded_size_bytes(
        args.registry.parent,
        args.model_dir,
        args.runtime_pack_dir,
    )
    smoke_paths = [
        recommended_archive_path(registry, "recommended", "models", args.model_dir, allow_default=True),
        recommended_archive_path(
            registry,
            "recommendedRuntimePack",
            "runtimePacks",
            args.runtime_pack_dir,
            allow_default=False,
        ),
    ]
    smoke_extract_bytes = 0
    for path in smoke_paths:
        if path is None:
            continue
        stats = archive_stats.get(path.resolve())
        if stats is None:
            raise SystemExit(f"recommended semantic artifact is missing from validated archives: {path.name}")
        smoke_extract_bytes += stats.uncompressed_size_bytes
    try:
        enforce_semantic_working_set(
            downloaded_size_bytes,
            validation_extract_bytes,
            smoke_extract_bytes,
        )
    except SemanticArchiveBudgetError as error:
        raise SystemExit(str(error)) from error


def semantic_downloaded_size_bytes(*directories: Path) -> int:
    total = 0
    seen = set()
    for directory in directories:
        resolved_directory = directory.resolve()
        if resolved_directory in seen or not resolved_directory.is_dir():
            continue
        seen.add(resolved_directory)
        for path in resolved_directory.iterdir():
            if path.is_file():
                total += path.stat().st_size
    return total


def recommended_archive_path(
    registry: dict,
    recommended_key: str,
    collection_key: str,
    artifact_dir: Path,
    *,
    allow_default: bool,
) -> Path | None:
    recommended_id = str(registry.get(recommended_key) or "").strip()
    if not recommended_id:
        return None
    owner = next(
        (
            item
            for item in registry.get(collection_key) or []
            if isinstance(item, dict) and str(item.get("id") or "").strip() == recommended_id
        ),
        None,
    )
    if owner is None:
        return None
    artifacts = owner.get("artifacts") or {}
    if not isinstance(artifacts, dict):
        return None
    artifact = artifacts.get("linux-amd64")
    if not isinstance(artifact, dict) and allow_default:
        artifact = artifacts.get("default")
    if not isinstance(artifact, dict):
        return None
    filename = str(artifact.get("filename") or "").strip()
    if not filename or Path(filename).name != filename:
        return None
    return artifact_dir / filename


def required_identity_part(owner: dict, key: str, label: str) -> str:
    value = str(owner.get(key) or "").strip()
    if not IDENTITY_PART_RE.fullmatch(value) or value in {".", ".."}:
        raise SystemExit(f"{label} {key} is required and must contain only letters, digits, '.', '-', or '_'")
    return value


def validate_artifacts(
    owner: dict,
    label: str,
    artifact_dir: Path,
    base_url: str,
    require_signatures: bool,
    registry_path: Path,
    validate_pack_layouts: bool,
) -> list[dict]:
    owner_id = str(owner.get("id") or "").strip()
    if not owner_id:
        raise SystemExit(f"{label} id is required")
    artifacts = owner.get("artifacts")
    if not isinstance(artifacts, dict) or not artifacts:
        raise SystemExit(f"{label} {owner_id!r} has no artifacts")
    reports = []
    for platform, artifact in artifacts.items():
        platform = str(platform).strip()
        if not isinstance(artifact, dict):
            raise SystemExit(f"{label} {owner_id!r} artifact {platform!r} must be an object")
        filename = str(artifact.get("filename") or "").strip()
        url = str(artifact.get("url") or "").strip()
        sha256 = str(artifact.get("sha256") or "").strip().lower()
        if not filename:
            raise SystemExit(f"{label} {owner_id!r} artifact {platform!r} filename is required")
        if not valid_url(url):
            raise SystemExit(f"{label} {owner_id!r} artifact {platform!r} URL must be http or https")
        if base_url and url != f"{base_url.rstrip('/')}/{filename}":
            raise SystemExit(f"{label} {owner_id!r} artifact {platform!r} URL is {url!r}, want {base_url.rstrip('/')}/{filename}")
        if not SHA256_RE.match(sha256):
            raise SystemExit(f"{label} {owner_id!r} artifact {platform!r} sha256 must be a 64-char hex string")
        declared_size = artifact.get("sizeBytes")
        if isinstance(declared_size, bool) or not isinstance(declared_size, int):
            raise SystemExit(f"{label} {owner_id!r} artifact {platform!r} sizeBytes must be an integer")
        if declared_size <= 0 or declared_size > MAX_ARTIFACT_SIZE_BYTES:
            raise SystemExit(
                f"{label} {owner_id!r} artifact {platform!r} sizeBytes must be between 1 and {MAX_ARTIFACT_SIZE_BYTES}"
            )
        artifact_path = artifact_dir / filename
        if not artifact_path.is_file():
            raise SystemExit(f"{label} {owner_id!r} artifact file is missing: {artifact_path}")
        actual_sha = sha256_file(artifact_path)
        if actual_sha != sha256:
            raise SystemExit(f"{label} {owner_id!r} artifact {filename} sha256 mismatch: {actual_sha} != {sha256}")
        if declared_size != artifact_path.stat().st_size:
            raise SystemExit(f"{label} {owner_id!r} artifact {filename} sizeBytes mismatch")
        checksum_path = validate_checksum_sidecar(artifact_path, sha256)
        sidecars = validate_sidecars(artifact_path, owner_id, artifact_dir, require_signatures)
        if validate_pack_layouts:
            validate_pack_layout(label, owner, platform, artifact_path, checksum_path, sidecars, registry_path)
        reports.append({
            "id": owner_id,
            "platform": platform,
            "filename": filename,
            "files": [
                filename,
                checksum_path.name,
                sidecars["metadata"].name,
                sidecars["sbom"].name,
                *([sidecars["signature"].name] if sidecars["signature"] is not None else []),
            ],
        })
    return reports


def validate_checksum_sidecar(artifact_path: Path, sha256: str) -> Path:
    checksum_path = artifact_path.with_suffix(artifact_path.suffix + ".sha256")
    if not checksum_path.is_file():
        raise SystemExit(f"checksum sidecar is missing: {checksum_path}")
    lines = [line.strip() for line in checksum_path.read_text(encoding="utf-8").splitlines() if line.strip()]
    if len(lines) != 1:
        raise SystemExit(f"{checksum_path} must contain exactly one checksum line")
    parts = lines[0].split()
    if len(parts) != 2 or parts[0].lower() != sha256 or parts[1] != artifact_path.name:
        raise SystemExit(f"{checksum_path} does not match {artifact_path.name}")
    return checksum_path


def validate_sidecars(artifact_path: Path, owner_id: str, artifact_dir: Path, require_signature: bool) -> dict:
    metadata_path = artifact_dir / f"{artifact_path.stem}.metadata.json"
    if not metadata_path.is_file():
        raise SystemExit(f"metadata sidecar is missing: {metadata_path}")
    read_json(metadata_path)
    sbom_path = artifact_dir / f"{artifact_path.stem}.spdx.json"
    if not sbom_path.is_file():
        sbom_path = artifact_dir / f"{owner_id}.spdx.json"
    if not sbom_path.is_file():
        raise SystemExit(f"SBOM sidecar is missing: {sbom_path}")
    sbom = read_json(sbom_path)
    if sbom.get("spdxVersion") != "SPDX-2.3":
        raise SystemExit(f"SBOM {sbom_path} spdxVersion must be SPDX-2.3")
    signature_path = artifact_path.with_suffix(artifact_path.suffix + ".sig")
    if require_signature and not signature_path.is_file():
        raise SystemExit(f"signature sidecar is required but missing: {signature_path}")
    return {
        "metadata": metadata_path,
        "sbom": sbom_path,
        "signature": signature_path if signature_path.is_file() else None,
    }


def validate_pack_layout(
    label: str,
    owner: dict,
    platform: str,
    artifact_path: Path,
    checksum_path: Path,
    sidecars: dict,
    registry_path: Path,
) -> None:
    agent_dir = Path(__file__).resolve().parents[2]
    if label == "model":
        validator = Path(__file__).resolve().with_name("validate_semantic_model_pack.py")
        command = [
            sys.executable,
            str(validator),
            "--artifact", str(artifact_path),
            "--sha256-file", str(checksum_path),
            "--metadata", str(sidecars["metadata"]),
            "--sbom", str(sidecars["sbom"]),
            "--registry", str(registry_path),
            "--allow-non-recommended",
            "--expected-id", str(owner.get("id") or "").strip(),
            "--expected-name", str(owner.get("name") or "").strip(),
            "--expected-version", str(owner.get("version") or "").strip(),
            "--expected-vector-space-id", str(owner.get("vectorSpaceId") or "").strip(),
            "--expected-embedding-dim", str(owner.get("embeddingDim") or 0),
            "--expected-input-kind", str(owner.get("inputKind") or "").strip(),
            "--expected-runtime", str(owner.get("runtime") or "").strip(),
            "--expected-platform", platform,
        ]
    else:
        validator = agent_dir / "semantic-runtime" / "siglip2-onnx" / "validate_runtime_pack.py"
        command = [
            sys.executable,
            str(validator),
            "--artifact", str(artifact_path),
            "--sha256-file", str(checksum_path),
            "--metadata", str(sidecars["metadata"]),
            "--sbom", str(sidecars["sbom"]),
            "--registry", str(registry_path),
            "--expected-platform", platform,
            "--allow-non-recommended",
            "--expected-id", str(owner.get("id") or "").strip(),
            "--expected-name", str(owner.get("name") or "").strip(),
            "--expected-version", str(owner.get("version") or "").strip(),
            "--expected-runtime", str(owner.get("runtime") or "").strip(),
        ]
        if platform == "linux-amd64":
            command.append("--require-bundled-python")
    if sidecars["signature"] is not None:
        command.extend(["--signature", str(sidecars["signature"])])
    try:
        subprocess.run(command, check=True)
    except subprocess.CalledProcessError as error:
        raise SystemExit(f"{label} artifact {artifact_path.name!r} failed pack layout validation") from error


def validate_exact_asset_namespace(args: argparse.Namespace, reports: list[dict]) -> None:
    expected = {args.registry.name}
    for report in reports:
        expected.update(report["files"])
    directories = {args.registry.parent.resolve(), args.model_dir.resolve(), args.runtime_pack_dir.resolve()}
    actual = {
        path.name
        for directory in directories
        if directory.is_dir()
        for path in directory.iterdir()
        if path.is_file()
    }
    missing = sorted(expected - actual)
    extras = sorted(actual - expected)
    if missing:
        raise SystemExit("semantic release namespace is missing assets: " + ", ".join(missing))
    if extras:
        raise SystemExit("semantic release namespace has unreferenced assets: " + ", ".join(extras))


def valid_url(value: str) -> bool:
    parsed = urlparse(value)
    return parsed.scheme in {"http", "https"} and bool(parsed.netloc)


def sha256_file(path: Path) -> str:
    hasher = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            hasher.update(chunk)
    return hasher.hexdigest()


if __name__ == "__main__":
    raise SystemExit(main())
