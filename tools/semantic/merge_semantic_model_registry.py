#!/usr/bin/env python3
"""Merge Timich semantic model/runtime registry fragments for release assets."""

from __future__ import annotations

import argparse
import json
import re
from pathlib import Path
from urllib.parse import urlparse


PRODUCT = "timich-semantic-models"
SHA256_RE = re.compile(r"^[0-9a-f]{64}$")
MAX_ARTIFACT_SIZE_BYTES = 8 * 1024 * 1024 * 1024


def main() -> int:
    args = parse_args()
    models: list[dict] = []
    runtime_packs: list[dict] = []
    model_ids: dict[str, dict] = {}
    runtime_pack_ids: dict[str, dict] = {}
    recommended = args.recommended.strip()
    recommended_runtime_pack = args.recommended_runtime_pack.strip()

    for path in args.inputs:
        manifest = read_json(path)
        validate_manifest_header(manifest, path)
        if not recommended:
            recommended = str(manifest.get("recommended", "")).strip()
        if not recommended_runtime_pack:
            recommended_runtime_pack = str(manifest.get("recommendedRuntimePack", "")).strip()

        for model in manifest.get("models") or []:
            normalize_model(model, path, args.base_url)
            add_unique(models, model_ids, model, "id", path, "model")
        for runtime_pack in manifest.get("runtimePacks") or []:
            normalize_runtime_pack(runtime_pack, path, args.base_url)
            add_unique(runtime_packs, runtime_pack_ids, runtime_pack, "id", path, "runtime pack")

    if not models and not runtime_packs:
        raise SystemExit("no models or runtime packs found in input manifests")
    if models and not recommended:
        recommended = models[0]["id"]
    if runtime_packs and not recommended_runtime_pack:
        recommended_runtime_pack = runtime_packs[0]["id"]
    if recommended and recommended not in model_ids:
        raise SystemExit(f"recommended model {recommended!r} was not found")
    if recommended_runtime_pack and recommended_runtime_pack not in runtime_pack_ids:
        raise SystemExit(f"recommended runtime pack {recommended_runtime_pack!r} was not found")

    output = {
        "schemaVersion": 1,
        "product": PRODUCT,
        "version": args.version.strip(),
    }
    if recommended:
        output["recommended"] = recommended
    if recommended_runtime_pack:
        output["recommendedRuntimePack"] = recommended_runtime_pack
    if models:
        output["models"] = models
    if runtime_packs:
        output["runtimePacks"] = runtime_packs

    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(output, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    print(json.dumps({
        "output": str(args.output),
        "models": len(models),
        "runtimePacks": len(runtime_packs),
        "recommended": recommended,
        "recommendedRuntimePack": recommended_runtime_pack,
    }, indent=2, sort_keys=True))
    return 0


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Merge Timich semantic model/runtime registry fragments.")
    parser.add_argument("--output", type=Path, required=True, help="path to write semantic-models.json")
    parser.add_argument("--version", default="", help="registry version string")
    parser.add_argument("--recommended", default="", help="recommended semantic model id")
    parser.add_argument("--recommended-runtime-pack", default="", help="recommended semantic runtime pack id")
    parser.add_argument("--base-url", default="", help="rewrite artifact URLs to this release asset base URL")
    parser.add_argument("inputs", nargs="+", type=Path, help="manifest or registry fragment JSON files")
    return parser.parse_args()


def read_json(path: Path) -> dict:
    try:
        with path.open("r", encoding="utf-8") as handle:
            payload = json.load(handle)
    except OSError as error:
        raise SystemExit(f"could not read {path}: {error}") from error
    except json.JSONDecodeError as error:
        raise SystemExit(f"could not parse {path}: {error}") from error
    if not isinstance(payload, dict):
        raise SystemExit(f"{path}: manifest must be a JSON object")
    return payload


def validate_manifest_header(manifest: dict, path: Path) -> None:
    if manifest.get("schemaVersion") != 1:
        raise SystemExit(f"{path}: schemaVersion must be 1")
    if str(manifest.get("product", "")).strip() != PRODUCT:
        raise SystemExit(f"{path}: product must be {PRODUCT}")


def add_unique(items: list[dict], seen: dict[str, dict], item: dict, key: str, path: Path, label: str) -> None:
    item_id = str(item.get(key, "")).strip()
    if item_id in seen:
        if stable_json(seen[item_id]) != stable_json(item):
            raise SystemExit(f"{path}: duplicate {label} {item_id!r} has different metadata")
        return
    seen[item_id] = item
    items.append(item)


def normalize_model(model: dict, path: Path, base_url: str) -> None:
    if not isinstance(model, dict):
        raise SystemExit(f"{path}: model entries must be JSON objects")
    required_strings = ("id", "name", "vectorSpaceId", "inputKind")
    trim_strings(model, required_strings + ("version", "runtime", "quantization", "license"))
    for field in required_strings:
        if not model.get(field):
            raise SystemExit(f"{path}: model {model.get('id', '')!r} missing {field}")
    if model["inputKind"] != "image":
        raise SystemExit(f"{path}: model {model['id']!r} inputKind must be image")
    if int(model.get("embeddingDim") or 0) <= 0:
        raise SystemExit(f"{path}: model {model['id']!r} embeddingDim must be positive")
    model["embeddingDim"] = int(model["embeddingDim"])
    model.pop("sizeBytes", None)
    languages = []
    for language in model.get("queryLanguages") or []:
        trimmed = str(language).strip()
        if trimmed:
            languages.append(trimmed)
    if languages:
        model["queryLanguages"] = languages
    elif "queryLanguages" in model:
        model.pop("queryLanguages")
    normalize_artifacts(model, path, f"model {model['id']!r}", base_url)


def normalize_runtime_pack(pack: dict, path: Path, base_url: str) -> None:
    if not isinstance(pack, dict):
        raise SystemExit(f"{path}: runtime pack entries must be JSON objects")
    required_strings = ("id", "name", "runtime")
    trim_strings(pack, required_strings + ("version", "license"))
    for field in required_strings:
        if not pack.get(field):
            raise SystemExit(f"{path}: runtime pack {pack.get('id', '')!r} missing {field}")
    pack.pop("sizeBytes", None)
    normalize_artifacts(pack, path, f"runtime pack {pack['id']!r}", base_url)


def trim_strings(payload: dict, fields: tuple[str, ...]) -> None:
    for field in fields:
        if field in payload and payload[field] is not None:
            payload[field] = str(payload[field]).strip()


def normalize_artifacts(owner: dict, path: Path, label: str, base_url: str) -> None:
    artifacts = owner.get("artifacts")
    if not isinstance(artifacts, dict) or not artifacts:
        raise SystemExit(f"{path}: {label} must include artifacts")
    normalized = {}
    for platform, artifact in artifacts.items():
        platform = str(platform).strip()
        if not platform:
            raise SystemExit(f"{path}: {label} artifact platform is required")
        if platform in normalized:
            raise SystemExit(f"{path}: {label} artifact platform {platform!r} is duplicated after normalization")
        if not isinstance(artifact, dict):
            raise SystemExit(f"{path}: {label} artifact {platform} must be an object")
        filename = str(artifact.get("filename", "")).strip()
        url = str(artifact.get("url", "")).strip()
        if base_url:
            url = f"{base_url.rstrip('/')}/{filename}"
        sha256 = str(artifact.get("sha256", "")).strip().lower()
        if not filename:
            raise SystemExit(f"{path}: {label} artifact {platform} filename is required")
        if not valid_url(url):
            raise SystemExit(f"{path}: {label} artifact {platform} URL must be http or https")
        if not SHA256_RE.match(sha256):
            raise SystemExit(f"{path}: {label} artifact {platform} sha256 must be a 64-char hex string")
        size_bytes = artifact.get("sizeBytes")
        if isinstance(size_bytes, bool) or not isinstance(size_bytes, int):
            raise SystemExit(f"{path}: {label} artifact {platform} sizeBytes must be an integer")
        if size_bytes <= 0 or size_bytes > MAX_ARTIFACT_SIZE_BYTES:
            raise SystemExit(
                f"{path}: {label} artifact {platform} sizeBytes must be between 1 and {MAX_ARTIFACT_SIZE_BYTES}"
            )
        normalized_artifact = {
            "filename": filename,
            "sha256": sha256,
            "url": url,
            "sizeBytes": size_bytes,
        }
        normalized[platform] = normalized_artifact
    owner["artifacts"] = normalized


def valid_url(value: str) -> bool:
    parsed = urlparse(value)
    return parsed.scheme in {"http", "https"} and bool(parsed.netloc)


def stable_json(value: dict) -> str:
    return json.dumps(value, sort_keys=True, separators=(",", ":"))


if __name__ == "__main__":
    raise SystemExit(main())
