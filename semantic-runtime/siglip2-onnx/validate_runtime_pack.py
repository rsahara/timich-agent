#!/usr/bin/env python3
"""Validate a Timich semantic runtime pack release artifact."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import platform as py_platform
from pathlib import Path, PurePosixPath
import shutil
import stat
import subprocess
import sys
import tempfile
import zipfile

AGENT_DIR = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(AGENT_DIR / "tools" / "semantic"))

from semantic_archive_budget import SemanticArchiveBudgetError, preflight_semantic_archive_directory


RUNTIME = "onnxruntime"
LAYOUT_PRODUCT = "timich-semantic-runtime-pack"
ARTIFACT_PRODUCT = "timich-semantic-runtime-pack-artifact"
REGISTRY_PRODUCT = "timich-semantic-models"
MAX_EXTRACT_SIZE = 8 << 30


def main() -> int:
    args = parse_args()
    artifact = resolve_artifact(args).resolve()
    artifact_sha = sha256_file(artifact)
    artifact_size = artifact.stat().st_size

    sha_path = resolve_sidecar(args.sha256_file, artifact.with_suffix(artifact.suffix + ".sha256"))
    validate_sha256_file(sha_path, artifact.name, artifact_sha)

    metadata_path = resolve_sidecar(args.metadata, artifact.parent / f"{artifact.stem}.metadata.json")
    metadata = read_json(metadata_path)
    runtime_pack = validate_metadata(metadata, artifact, artifact_sha, artifact_size, args.expected_platform)
    validate_expected_owner(runtime_pack, args)

    sbom_filename = str(runtime_pack.get("sbom", {}).get("filename") or f"{artifact.stem}.spdx.json")
    sbom_path = resolve_sidecar(args.sbom, artifact.parent / sbom_filename)
    validate_sbom(sbom_path, runtime_pack.get("sbom", {}))

    registry_path = args.registry or artifact.parent / f"{artifact.stem}.registry.json"
    registry_validated = False
    if registry_path.is_file():
        validate_registry(read_json(registry_path), runtime_pack, args.allow_non_recommended)
        registry_validated = True

    signature_path = args.signature
    signature_metadata = runtime_pack.get("signature") or {}
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
    with tempfile.TemporaryDirectory(prefix="timich-runtime-pack-") as temp_dir:
        extraction_root = Path(temp_dir) / "runtime"
        extract_zip_safely(artifact, extraction_root)
        layout_report = validate_layout(extraction_root, args.require_bundled_python)
        smoke_report = None
        if args.smoke_import:
            smoke_report = smoke_import(extraction_root, layout_report, args.smoke_timeout)

    report = {
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
    }
    if smoke_report is not None:
        report["smokeImport"] = smoke_report
    print(json.dumps(report, indent=2, sort_keys=True))
    return 0


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="Validate a Timich semantic runtime pack artifact.")
    parser.add_argument("--artifact", type=Path, default=path_env("TIMICH_RUNTIME_PACK_ARTIFACT"))
    parser.add_argument("--output-dir", type=Path, default=Path(env("TIMICH_RUNTIME_PACK_OUTPUT_DIR", "dist/semantic-runtime-packs")))
    parser.add_argument("--sha256-file", type=Path, default=path_env("TIMICH_RUNTIME_PACK_SHA256_FILE"))
    parser.add_argument("--metadata", type=Path, default=path_env("TIMICH_RUNTIME_PACK_METADATA"))
    parser.add_argument("--sbom", type=Path, default=path_env("TIMICH_RUNTIME_PACK_SBOM"))
    parser.add_argument("--registry", type=Path, default=path_env("TIMICH_RUNTIME_PACK_REGISTRY"))
    parser.add_argument("--signature", type=Path, default=path_env("TIMICH_RUNTIME_PACK_SIGNATURE"))
    parser.add_argument("--public-key", type=Path, default=path_env("TIMICH_RUNTIME_PACK_PUBLIC_KEY"))
    parser.add_argument("--expected-platform", default=env("TIMICH_RUNTIME_PACK_EXPECTED_PLATFORM", ""))
    parser.add_argument("--expected-id")
    parser.add_argument("--expected-name")
    parser.add_argument("--expected-version")
    parser.add_argument("--expected-runtime")
    parser.add_argument("--require-signature", action="store_true", default=bool_env("TIMICH_RUNTIME_PACK_REQUIRE_SIGNATURE"))
    parser.add_argument("--require-bundled-python", action="store_true", default=bool_env("TIMICH_RUNTIME_PACK_REQUIRE_BUNDLED_PYTHON"))
    parser.add_argument("--smoke-import", action="store_true", default=bool_env("TIMICH_RUNTIME_PACK_SMOKE_IMPORT"))
    parser.add_argument("--smoke-timeout", type=float, default=float(env("TIMICH_RUNTIME_PACK_SMOKE_TIMEOUT", "30")))
    parser.add_argument("--allow-non-recommended", action="store_true")
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
            raise SystemExit(f"runtime pack artifact is missing: {args.artifact}")
        return args.artifact
    candidates = sorted(args.output_dir.glob("*.zip"), key=lambda path: path.stat().st_mtime, reverse=True)
    if not candidates:
        raise SystemExit(f"no runtime pack artifacts found in {args.output_dir}")
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


def validate_metadata(metadata: dict, artifact: Path, artifact_sha: str, artifact_size: int, expected_platform: str) -> dict:
    if metadata.get("schemaVersion") != 1:
        raise SystemExit("metadata schemaVersion must be 1")
    if metadata.get("product") != ARTIFACT_PRODUCT:
        raise SystemExit("metadata product is invalid")
    runtime_pack = metadata.get("runtimePack")
    if not isinstance(runtime_pack, dict):
        raise SystemExit("metadata runtimePack object is required")
    for key in ("id", "name", "version", "runtime", "platform", "artifact", "sbom"):
        if runtime_pack.get(key) in (None, ""):
            raise SystemExit(f"metadata runtimePack.{key} is required")
    if runtime_pack["runtime"] != RUNTIME:
        raise SystemExit(f"metadata runtime must be {RUNTIME}")
    if expected_platform and runtime_pack["platform"] != expected_platform:
        raise SystemExit(f"metadata platform is {runtime_pack['platform']!r}, want {expected_platform!r}")
    artifact_meta = runtime_pack.get("artifact")
    if not isinstance(artifact_meta, dict):
        raise SystemExit("metadata runtimePack.artifact object is required")
    if artifact_meta.get("filename") != artifact.name:
        raise SystemExit(f"metadata artifact filename is {artifact_meta.get('filename')!r}, want {artifact.name!r}")
    if str(artifact_meta.get("sha256", "")).lower() != artifact_sha:
        raise SystemExit("metadata artifact sha256 does not match artifact")
    if int(artifact_meta.get("sizeBytes") or 0) != artifact_size:
        raise SystemExit("metadata artifact sizeBytes does not match artifact")
    return runtime_pack


def validate_expected_owner(runtime_pack: dict, args: argparse.Namespace) -> None:
    expected_values = {
        "id": args.expected_id,
        "name": args.expected_name,
        "version": args.expected_version,
        "runtime": args.expected_runtime,
    }
    for key, expected in expected_values.items():
        if expected is not None and str(runtime_pack.get(key) or "").strip() != str(expected).strip():
            raise SystemExit(f"metadata runtimePack.{key} does not match expected owner")


def validate_sbom(path: Path, sbom_metadata: dict) -> None:
    sbom = read_json(path)
    if sbom.get("spdxVersion") != "SPDX-2.3":
        raise SystemExit("SBOM spdxVersion must be SPDX-2.3")
    if sbom.get("SPDXID") != "SPDXRef-DOCUMENT":
        raise SystemExit("SBOM SPDXID is invalid")
    packages = sbom.get("packages")
    if not isinstance(packages, list) or not packages:
        raise SystemExit("SBOM packages must not be empty")
    if not any(package.get("SPDXID") == "SPDXRef-Package-RuntimePack" for package in packages if isinstance(package, dict)):
        raise SystemExit("SBOM is missing SPDXRef-Package-RuntimePack")
    expected_sha = str(sbom_metadata.get("sha256") or "").lower()
    if expected_sha and expected_sha != sha256_file(path):
        raise SystemExit("metadata SBOM sha256 does not match SBOM file")
    expected_size = int(sbom_metadata.get("sizeBytes") or 0)
    if expected_size and expected_size != path.stat().st_size:
        raise SystemExit("metadata SBOM sizeBytes does not match SBOM file")


def validate_registry(registry: dict, runtime_pack: dict, allow_non_recommended: bool = False) -> None:
    if registry.get("schemaVersion") != 1:
        raise SystemExit("registry schemaVersion must be 1")
    if registry.get("product") != REGISTRY_PRODUCT:
        raise SystemExit("registry product is invalid")
    if not allow_non_recommended and registry.get("recommendedRuntimePack") != runtime_pack["id"]:
        raise SystemExit("registry recommendedRuntimePack does not match metadata")
    packs = registry.get("runtimePacks")
    if not isinstance(packs, list):
        raise SystemExit("registry runtimePacks must be a list")
    pack = next((item for item in packs if isinstance(item, dict) and item.get("id") == runtime_pack["id"]), None)
    if pack is None:
        raise SystemExit("registry does not contain the metadata runtime pack")
    if str(pack.get("version") or "").strip() != str(runtime_pack.get("version") or "").strip():
        raise SystemExit("registry runtime pack version does not match metadata")
    if str(pack.get("runtime") or "").strip() != str(runtime_pack.get("runtime") or "").strip():
        raise SystemExit("registry runtime pack runtime does not match metadata")
    platform = runtime_pack["platform"]
    artifact = (pack.get("artifacts") or {}).get(platform)
    if not isinstance(artifact, dict):
        raise SystemExit(f"registry does not contain artifact for platform {platform}")
    metadata_artifact = runtime_pack["artifact"]
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
            [
                "openssl",
                "dgst",
                "-sha256",
                "-verify",
                str(public_key),
                "-signature",
                str(signature),
                str(artifact),
            ],
            check=True,
            capture_output=True,
            text=True,
        )
    except FileNotFoundError as error:
        raise SystemExit("openssl is required to verify runtime pack signatures") from error
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
                raise SystemExit("runtime pack uncompressed size exceeds limit")
    if entries == 0:
        raise SystemExit("runtime pack zip contains no files")
    return {"fileCount": entries, "uncompressedSizeBytes": uncompressed_size}


def validate_zip_entry(info: zipfile.ZipInfo) -> None:
    relative_parts(info.filename)
    mode = (info.external_attr >> 16) & 0xFFFF
    if stat.S_IFMT(mode) == stat.S_IFLNK:
        raise SystemExit(f"runtime pack zip entry is a symlink: {info.filename}")


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
                shutil.copyfileobj(source, output)
            mode = (info.external_attr >> 16) & 0o777
            if mode:
                target.chmod(mode)


def validate_layout(root: Path, require_bundled_python: bool) -> dict:
    layout = read_json(root / "timich-runtime.json")
    if layout.get("schemaVersion") != 1:
        raise SystemExit("timich-runtime.json schemaVersion must be 1")
    if layout.get("product") != LAYOUT_PRODUCT:
        raise SystemExit("timich-runtime.json product is invalid")
    if layout.get("runtime") != RUNTIME:
        raise SystemExit(f"timich-runtime.json runtime must be {RUNTIME}")
    server_relative = str(layout.get("serverPath") or "")
    server_path = root.joinpath(*relative_parts(server_relative))
    if not server_path.is_file():
        raise SystemExit(f"timich-runtime.json serverPath is missing: {server_relative}")
    python_relative = str(layout.get("pythonPath") or "")
    if not python_relative:
        raise SystemExit("runtime pack must include pythonPath")
    python_path = root.joinpath(*relative_parts(python_relative))
    if not python_path.is_file():
        raise SystemExit(f"timich-runtime.json pythonPath is missing: {python_relative}")
    if not is_windows_platform() and not os.access(python_path, os.X_OK):
        raise SystemExit(f"timich-runtime.json pythonPath is not executable: {python_relative}")
    if require_bundled_python and bundled_python_home(python_path) is None:
        raise SystemExit("runtime pack bundled Python is missing standard library encodings")
    return {
        "runtime": layout["runtime"],
        "serverPath": server_relative,
        "pythonPath": python_relative,
        "bundledPython": bool(python_relative),
    }


def smoke_import(root: Path, layout_report: dict, timeout: float) -> dict:
    python_relative = str(layout_report.get("pythonPath") or "")
    server_relative = str(layout_report["serverPath"])
    python_path = root.joinpath(*relative_parts(python_relative)) if python_relative else Path(sys.executable)
    server_path = root.joinpath(*relative_parts(server_relative))
    env = os.environ.copy()
    python_home = bundled_python_home(python_path)
    if python_home is not None:
        env["PYTHONHOME"] = str(python_home)
        env["PYTHONNOUSERSITE"] = "1"
        library_env = bundled_python_library_env(python_home)
        if library_env is not None:
            key, value = library_env
            previous = env.get(key, "")
            env[key] = value if not previous else value + os.pathsep + previous
    try:
        result = subprocess.run(
            [str(python_path), str(server_path), "--help"],
            check=True,
            capture_output=True,
            text=True,
            env=env,
            timeout=timeout,
        )
    except subprocess.TimeoutExpired as error:
        raise SystemExit(f"runtime import smoke timed out after {timeout:.1f}s") from error
    except subprocess.CalledProcessError as error:
        output = (error.stderr or error.stdout or "").strip()
        raise SystemExit(f"runtime import smoke failed: {output}") from error
    return {
        "python": str(python_path),
        "pythonHome": str(python_home) if python_home is not None else "",
        "server": server_relative,
        "stdoutBytes": len(result.stdout.encode("utf-8")),
        "stderrBytes": len(result.stderr.encode("utf-8")),
    }


def bundled_python_home(python_path: Path) -> Path | None:
    root = python_path.parent.parent
    if not (root / "pyvenv.cfg").is_file():
        return None
    if not (root / "lib").is_dir():
        return None
    if not has_bundled_python_stdlib(root):
        return None
    return root


def has_bundled_python_stdlib(root: Path) -> bool:
    if (root / "Lib" / "encodings" / "__init__.py").is_file():
        return True
    lib = root / "lib"
    if not lib.is_dir():
        return False
    for child in lib.iterdir():
        if child.is_dir() and child.name.startswith("python") and (child / "encodings" / "__init__.py").is_file():
            return True
    return False


def bundled_python_library_env(python_home: Path) -> tuple[str, str] | None:
    library_path = python_home / "lib"
    if not library_path.is_dir():
        return None
    system = py_platform.system().lower()
    if system == "linux":
        return ("LD_LIBRARY_PATH", str(library_path))
    return None


def relative_parts(raw: str) -> tuple[str, ...]:
    if not raw or "\\" in raw:
        raise SystemExit(f"unsafe runtime pack path: {raw!r}")
    path = PurePosixPath(raw)
    if path.is_absolute():
        raise SystemExit(f"unsafe runtime pack path: {raw!r}")
    parts = path.parts
    if not parts or any(part in {"", ".", ".."} for part in parts):
        raise SystemExit(f"unsafe runtime pack path: {raw!r}")
    return tuple(parts)


def is_windows_platform() -> bool:
    return py_platform.system().lower().startswith("windows")


def sha256_file(path: Path) -> str:
    hasher = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            hasher.update(chunk)
    return hasher.hexdigest()


if __name__ == "__main__":
    raise SystemExit(main())
