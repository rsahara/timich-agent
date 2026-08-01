#!/usr/bin/env python3
"""Execute the recommended Linux semantic packs and attest their exact bytes."""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import socket
import struct
import subprocess
import sys
import tempfile
import time
from typing import Any, Optional
from urllib.error import URLError
from urllib.request import Request, urlopen
import zipfile
import zlib

from semantic_archive_budget import (
    SemanticArchiveBudgetError,
    enforce_semantic_working_set,
    inspect_semantic_archive,
)


PRODUCT = "timich-semantic-release-smoke"
PLATFORM = "linux-amd64"
REQUIRED_IMPORTS = ("numpy", "onnxruntime", "PIL", "transformers")


def main() -> int:
    args = parse_args()
    helper_path = args.helper_path.resolve()
    if not helper_path.is_file() or not os.access(helper_path, os.X_OK):
        raise SystemExit(f"bundled semantic helper is not executable: {helper_path}")
    assets_dir = args.assets_dir.resolve()
    registry_path = assets_dir / "semantic-models.json"
    registry = read_json(registry_path)
    model = recommended_entry(registry, "recommended", "models")
    runtime_pack = recommended_entry(registry, "recommendedRuntimePack", "runtimePacks")
    model_artifact = artifact_path(assets_dir, model, "model", allow_default=True)
    runtime_artifact = artifact_path(assets_dir, runtime_pack, "runtime pack", allow_default=False)
    preflight_smoke_archive_budget(assets_dir, model_artifact, runtime_artifact)

    with tempfile.TemporaryDirectory(prefix="timich-semantic-smoke-") as raw_temp:
        temp = Path(raw_temp)
        model_root = temp / "model"
        runtime_root = temp / "runtime"
        extract_zip(model_artifact, model_root)
        extract_zip(runtime_artifact, runtime_root)
        model_layout_path = one_file(model_root, "timich-model.json")
        runtime_layout_path = one_file(runtime_root, "timich-runtime.json")
        runtime_layout = read_json(runtime_layout_path)
        python_path = safe_file(runtime_layout_path.parent, runtime_layout.get("pythonPath"), "pythonPath")
        server_path = safe_file(runtime_layout_path.parent, runtime_layout.get("serverPath"), "serverPath")
        if not os.access(python_path, os.X_OK):
            raise SystemExit(f"bundled Python is not executable: {python_path}")
        environment = runtime_environment(python_path)
        interpreter = probe_interpreter(python_path, environment, args.timeout)
        runtime_report = smoke_server(
            helper_path,
            python_path,
            server_path,
            model_layout_path.parent,
            environment,
            args.timeout,
        )

    attestation = {
        "schemaVersion": 1,
        "product": PRODUCT,
        "platform": PLATFORM,
        "recommendedModel": str(model.get("id") or ""),
        "recommendedRuntimePack": str(runtime_pack.get("id") or ""),
        "registrySha256": sha256_file(registry_path),
        "assets": asset_snapshot(assets_dir),
        "interpreter": interpreter,
        "runtime": runtime_report,
    }
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_text(json.dumps(attestation, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    print(json.dumps(attestation, indent=2, sort_keys=True))
    return 0


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--assets-dir", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--helper-path", type=Path, required=True)
    parser.add_argument("--timeout", type=float, default=300.0)
    return parser.parse_args()


def read_json(path: Path) -> dict[str, Any]:
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise SystemExit(f"could not read JSON {path}: {error}") from error
    if not isinstance(payload, dict):
        raise SystemExit(f"{path} must contain an object")
    return payload


def recommended_entry(registry: dict[str, Any], key: str, collection_key: str) -> dict[str, Any]:
    expected = str(registry.get(key) or "").strip()
    if not expected:
        raise SystemExit(f"registry {key} is required")
    matches = [item for item in registry.get(collection_key) or [] if isinstance(item, dict) and str(item.get("id") or "").strip() == expected]
    if len(matches) != 1:
        raise SystemExit(f"registry {key} {expected!r} must resolve exactly once")
    return matches[0]


def artifact_path(assets_dir: Path, owner: dict[str, Any], label: str, allow_default: bool) -> Path:
    artifacts = owner.get("artifacts") or {}
    artifact = artifacts.get(PLATFORM) or (artifacts.get("default") if allow_default else None)
    if not isinstance(artifact, dict):
        raise SystemExit(f"recommended {label} has no {PLATFORM} artifact")
    filename = str(artifact.get("filename") or "").strip()
    if not filename or Path(filename).name != filename:
        raise SystemExit(f"recommended {label} artifact filename is unsafe")
    path = assets_dir / filename
    if not path.is_file():
        raise SystemExit(f"recommended {label} artifact is missing: {filename}")
    expected_sha = str(artifact.get("sha256") or "").strip().lower()
    if sha256_file(path) != expected_sha:
        raise SystemExit(f"recommended {label} artifact digest mismatch: {filename}")
    return path


def preflight_smoke_archive_budget(assets_dir: Path, model_artifact: Path, runtime_artifact: Path) -> None:
    try:
        model_stats = inspect_semantic_archive(model_artifact)
        runtime_stats = inspect_semantic_archive(runtime_artifact)
        downloaded_size = sum(path.stat().st_size for path in assets_dir.iterdir() if path.is_file())
        enforce_semantic_working_set(
            downloaded_size,
            max(model_stats.uncompressed_size_bytes, runtime_stats.uncompressed_size_bytes),
            model_stats.uncompressed_size_bytes + runtime_stats.uncompressed_size_bytes,
        )
    except SemanticArchiveBudgetError as error:
        raise SystemExit(str(error)) from error


def extract_zip(artifact: Path, destination: Path) -> None:
    destination.mkdir(parents=True)
    try:
        with zipfile.ZipFile(artifact) as archive:
            for info in archive.infolist():
                parts = Path(info.filename).parts
                if not parts or Path(info.filename).is_absolute() or ".." in parts or "\\" in info.filename:
                    raise SystemExit(f"unsafe archive path in {artifact.name}: {info.filename}")
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
    except zipfile.BadZipFile as error:
        raise SystemExit(f"invalid ZIP artifact {artifact}: {error}") from error


def one_file(root: Path, name: str) -> Path:
    matches = list(root.rglob(name))
    if len(matches) != 1 or not matches[0].is_file():
        raise SystemExit(f"{name} must occur exactly once in {root}")
    return matches[0]


def safe_file(root: Path, raw: Any, label: str) -> Path:
    value = str(raw or "").strip()
    relative = Path(value)
    if not value or relative.is_absolute() or ".." in relative.parts or "\\" in value:
        raise SystemExit(f"runtime {label} is unsafe")
    path = root.joinpath(*relative.parts).resolve()
    try:
        path.relative_to(root.resolve())
    except ValueError as error:
        raise SystemExit(f"runtime {label} escapes the runtime pack") from error
    if not path.is_file():
        raise SystemExit(f"runtime {label} is missing: {value}")
    return path


def runtime_environment(python_path: Path) -> dict[str, str]:
    python_home = python_path.parent.parent
    environment = {
        "HOME": str(python_home),
        "LANG": os.environ.get("LANG", "C.UTF-8"),
        "PATH": os.environ.get("PATH", "/usr/local/bin:/usr/bin:/bin"),
    }
    environment["PYTHONHOME"] = str(python_home)
    environment["PYTHONNOUSERSITE"] = "1"
    library_dir = python_home / "lib"
    if library_dir.is_dir():
        environment["LD_LIBRARY_PATH"] = str(library_dir)
    return environment


def probe_interpreter(python_path: Path, environment: dict[str, str], timeout: float) -> dict[str, Any]:
    script = """
import importlib, json, platform, sys
versions = {}
for name in ("numpy", "onnxruntime", "PIL", "transformers"):
    module = importlib.import_module(name)
    versions[name] = str(getattr(module, "__version__", "present"))
print(json.dumps({"implementation": platform.python_implementation(), "version": platform.python_version(), "executable": sys.executable, "imports": versions}, sort_keys=True))
"""
    try:
        result = subprocess.run(
            [str(python_path), "-c", script],
            check=True,
            capture_output=True,
            text=True,
            env=environment,
            timeout=timeout,
        )
    except (OSError, subprocess.CalledProcessError, subprocess.TimeoutExpired) as error:
        raise SystemExit(f"bundled Python probe failed: {error}") from error
    try:
        report = json.loads(result.stdout.strip())
    except json.JSONDecodeError as error:
        raise SystemExit("bundled Python probe did not return interpreter identity JSON") from error
    if report.get("implementation") != "CPython" or set(report.get("imports") or {}) != set(REQUIRED_IMPORTS):
        raise SystemExit("bundled Python identity/import contract is incomplete")
    return report


def smoke_server(
    helper_path: Path,
    python_path: Path,
    server_path: Path,
    model_layout: Path,
    environment: dict[str, str],
    timeout: float,
) -> dict[str, Any]:
    port = available_port()
    base_url = f"http://127.0.0.1:{port}"
    with tempfile.TemporaryFile() as output:
        process = subprocess.Popen(
            [str(python_path), str(server_path), "--runtime-layout", str(model_layout), "--host", "127.0.0.1", "--port", str(port)],
            stdout=output,
            stderr=output,
            env=environment,
        )
        try:
            deadline = time.monotonic() + timeout
            health: Optional[dict[str, Any]] = None
            while time.monotonic() < deadline:
                if process.poll() is not None:
                    output.seek(0)
                    raise SystemExit("semantic runtime exited before health check: " + output.read().decode("utf-8", errors="replace")[-4000:])
                try:
                    health = request_json(base_url + "/healthz")
                    break
                except (OSError, URLError, ValueError):
                    time.sleep(0.5)
            if health is None or health.get("status") != "ok":
                raise SystemExit("semantic runtime health check timed out")
            helper_environment = environment.copy()
            helper_environment["TIMICH_SEMANTIC_ONNX_SERVER_URL"] = base_url
            inspect = run_helper(
                helper_path,
                ["inspect", "--runtime-layout", str(model_layout)],
                helper_environment,
                timeout,
            )
            run_helper(
                helper_path,
                ["embed-text", "--runtime-layout", str(model_layout), "--text", "a photo of a beach"],
                helper_environment,
                timeout,
            )
            run_helper(
                helper_path,
                ["embed-image", "--runtime-layout", str(model_layout), "--content-type", "image/png", "--source", "release-smoke.png"],
                helper_environment,
                timeout,
                smoke_png_bytes(),
            )
            return {
                "health": "ok",
                "modelId": inspect["modelId"],
                "vectorSpaceId": inspect["vectorSpaceId"],
                "embeddingDim": inspect["embeddingDim"],
                "inputKind": inspect["inputKind"],
                "runtime": inspect["runtime"],
                "textEmbedding": "ok",
                "imageEmbedding": "ok",
            }
        finally:
            process.terminate()
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=10)


def available_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def request_json(url: str, payload: Optional[dict[str, Any]] = None) -> dict[str, Any]:
    raw = None if payload is None else json.dumps(payload).encode("utf-8")
    request = Request(url, data=raw, headers={"Content-Type": "application/json"} if raw is not None else {})
    with urlopen(request, timeout=5) as response:
        result = json.loads(response.read().decode("utf-8"))
    if not isinstance(result, dict):
        raise ValueError("runtime response is not an object")
    return result


def run_helper(
    helper_path: Path,
    arguments: list[str],
    environment: dict[str, str],
    timeout: float,
    stdin: Optional[bytes] = None,
) -> dict[str, Any]:
    try:
        result = subprocess.run(
            [str(helper_path), *arguments],
            input=stdin,
            check=True,
            capture_output=True,
            env=environment,
            timeout=timeout,
        )
    except (OSError, subprocess.CalledProcessError, subprocess.TimeoutExpired) as error:
        detail = ""
        if isinstance(error, subprocess.CalledProcessError):
            detail = error.stderr.decode("utf-8", errors="replace")[-4000:]
        raise SystemExit(f"bundled semantic helper contract failed: {error}: {detail}") from error
    try:
        payload = json.loads(result.stdout.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise SystemExit("bundled semantic helper did not return JSON") from error
    if not isinstance(payload, dict):
        raise SystemExit("bundled semantic helper response is not an object")
    return payload


def smoke_png_bytes() -> bytes:
    def chunk(kind: bytes, payload: bytes) -> bytes:
        checksum = zlib.crc32(kind + payload) & 0xFFFFFFFF
        return struct.pack(">I", len(payload)) + kind + payload + struct.pack(">I", checksum)

    header = struct.pack(">IIBBBBB", 1, 1, 8, 2, 0, 0, 0)
    pixels = zlib.compress(b"\x00\x80\x80\x80")
    image = b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", header) + chunk(b"IDAT", pixels) + chunk(b"IEND", b"")
    return image


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
