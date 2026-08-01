#!/usr/bin/env python3
"""Verify that a secretless semantic smoke attestation binds the release bytes."""

from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path
from typing import Any


PRODUCT = "timich-semantic-release-smoke"
PLATFORM = "linux-amd64"


def main() -> int:
    args = parse_args()
    assets_dir = args.assets_dir.resolve()
    registry_path = assets_dir / "semantic-models.json"
    registry = read_json(registry_path)
    attestation = read_json(args.attestation)
    if attestation.get("schemaVersion") != 1 or attestation.get("product") != PRODUCT:
        raise SystemExit("semantic smoke attestation contract is invalid")
    if attestation.get("platform") != PLATFORM:
        raise SystemExit("semantic smoke attestation platform is invalid")
    if attestation.get("recommendedModel") != registry.get("recommended"):
        raise SystemExit("semantic smoke attestation recommended model mismatch")
    if attestation.get("recommendedRuntimePack") != registry.get("recommendedRuntimePack"):
        raise SystemExit("semantic smoke attestation recommended runtime pack mismatch")
    if attestation.get("registrySha256") != sha256_file(registry_path):
        raise SystemExit("semantic smoke attestation registry digest mismatch")
    if attestation.get("assets") != asset_snapshot(assets_dir):
        raise SystemExit("semantic smoke attestation asset snapshot mismatch")
    runtime = attestation.get("runtime") or {}
    model = recommended_model(registry)
    expected_runtime = {
        "modelId": model.get("id"),
        "vectorSpaceId": model.get("vectorSpaceId"),
        "embeddingDim": model.get("embeddingDim"),
        "inputKind": model.get("inputKind"),
        "runtime": model.get("runtime"),
    }
    for key, expected in expected_runtime.items():
        if runtime.get(key) != expected:
            raise SystemExit(f"semantic smoke attestation runtime {key} mismatch")
    if runtime.get("health") != "ok" or runtime.get("textEmbedding") != "ok" or runtime.get("imageEmbedding") != "ok":
        raise SystemExit("semantic smoke attestation did not pass runtime health and both embedding paths")
    interpreter = attestation.get("interpreter") or {}
    if interpreter.get("implementation") != "CPython":
        raise SystemExit("semantic smoke attestation interpreter is not CPython")
    if set(interpreter.get("imports") or {}) != {"numpy", "onnxruntime", "PIL", "transformers"}:
        raise SystemExit("semantic smoke attestation required imports are incomplete")
    print(json.dumps({"status": "ok", "assets": len(attestation["assets"]), "platform": PLATFORM}, sort_keys=True))
    return 0


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--assets-dir", type=Path, required=True)
    parser.add_argument("--attestation", type=Path, required=True)
    return parser.parse_args()


def read_json(path: Path) -> dict[str, Any]:
    try:
        value = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise SystemExit(f"could not read JSON {path}: {error}") from error
    if not isinstance(value, dict):
        raise SystemExit(f"{path} must contain an object")
    return value


def recommended_model(registry: dict[str, Any]) -> dict[str, Any]:
    expected = registry.get("recommended")
    matches = [item for item in registry.get("models") or [] if isinstance(item, dict) and item.get("id") == expected]
    if len(matches) != 1:
        raise SystemExit("recommended model is invalid")
    return matches[0]


def asset_snapshot(assets_dir: Path) -> list[dict[str, Any]]:
    ignored = {"semantic-asset-snapshot.json", "semantic-smoke-attestation.json"}
    paths = sorted(path for path in assets_dir.iterdir() if path.is_file() and path.name not in ignored)
    return [{"name": path.name, "size": path.stat().st_size, "sha256": sha256_file(path)} for path in paths]


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


if __name__ == "__main__":
    raise SystemExit(main())
