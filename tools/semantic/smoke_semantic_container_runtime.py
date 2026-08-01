#!/usr/bin/env python3
"""Probe the exact Linux semantic runtime pack inside a release-bundle image."""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path, PurePosixPath
import subprocess
import tempfile

from smoke_semantic_release import (
    REQUIRED_IMPORTS,
    artifact_path,
    extract_zip,
    one_file,
    read_json,
    recommended_entry,
    safe_file,
)


CONTAINER_RUNTIME_ROOT = PurePosixPath("/opt/timich-semantic-runtime")


def main() -> int:
    args = parse_args()
    assets_dir = args.assets_dir.resolve()
    bundle_dir = args.bundle_dir.resolve()
    if not (bundle_dir / "Dockerfile").is_file():
        raise SystemExit(f"release bundle Dockerfile is missing: {bundle_dir}")

    registry = read_json(assets_dir / "semantic-models.json")
    runtime_pack = recommended_entry(
        registry, "recommendedRuntimePack", "runtimePacks"
    )
    runtime_artifact = artifact_path(
        assets_dir, runtime_pack, "runtime pack", allow_default=False
    )

    subprocess.run(
        ["docker", "build", "--tag", args.image, str(bundle_dir)],
        check=True,
    )

    with tempfile.TemporaryDirectory(prefix="timich-semantic-container-smoke-") as raw_temp:
        extracted_root = Path(raw_temp) / "runtime"
        extract_zip(runtime_artifact, extracted_root)
        runtime_layout_path = one_file(extracted_root, "timich-runtime.json")
        runtime_layout = read_json(runtime_layout_path)
        python_path = safe_file(
            runtime_layout_path.parent,
            runtime_layout.get("pythonPath"),
            "pythonPath",
        )
        python_relative = python_path.relative_to(runtime_layout_path.parent)
        container_python = CONTAINER_RUNTIME_ROOT.joinpath(*python_relative.parts)
        container_python_home = container_python.parent.parent

        script = """
import importlib, json, platform
versions = {}
for name in ("numpy", "onnxruntime", "PIL", "transformers"):
    module = importlib.import_module(name)
    versions[name] = str(getattr(module, "__version__", "present"))
print(json.dumps({"implementation": platform.python_implementation(), "version": platform.python_version(), "imports": versions}, sort_keys=True))
"""
        result = subprocess.run(
            [
                "docker",
                "run",
                "--rm",
                "--mount",
                f"type=bind,source={runtime_layout_path.parent},target={CONTAINER_RUNTIME_ROOT},readonly",
                "--env",
                f"HOME={container_python_home}",
                "--env",
                f"PYTHONHOME={container_python_home}",
                "--env",
                "PYTHONNOUSERSITE=1",
                "--env",
                f"LD_LIBRARY_PATH={container_python_home}/lib",
                "--entrypoint",
                str(container_python),
                args.image,
                "-c",
                script,
            ],
            check=True,
            capture_output=True,
            text=True,
            timeout=args.timeout,
        )

    try:
        report = json.loads(result.stdout.strip())
    except json.JSONDecodeError as error:
        raise SystemExit(
            "containerized runtime probe did not return interpreter identity JSON"
        ) from error
    if report.get("implementation") != "CPython" or set(
        report.get("imports") or {}
    ) != set(REQUIRED_IMPORTS):
        raise SystemExit("containerized runtime identity/import contract is incomplete")

    print(json.dumps(report, indent=2, sort_keys=True))
    return 0


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--assets-dir", type=Path, required=True)
    parser.add_argument("--bundle-dir", type=Path, required=True)
    parser.add_argument(
        "--image", default=f"timich-agent-semantic-smoke:{os.getpid()}"
    )
    parser.add_argument("--timeout", type=float, default=300.0)
    return parser.parse_args()


if __name__ == "__main__":
    raise SystemExit(main())
