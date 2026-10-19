#!/usr/bin/env python3
"""Regression checks for selecting and attesting native Linux runtime packs."""

import argparse
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import smoke_semantic_release as smoke
import validate_semantic_release as release
import verify_semantic_smoke_attestation as verify


AGENT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location(
    "runtime_builder", AGENT / "semantic-runtime/siglip2-onnx/build_runtime_pack.py")
builder = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(builder)


class PlatformTests(unittest.TestCase):
    def test_portable_python_includes_system_openssl_but_not_host_glibc(self):
        for library in ("/usr/lib64/libssl.so.1.1", "/usr/lib64/libcrypto.so.1.1",
                        "/lib/aarch64-linux-gnu/libffi.so.8"):
            self.assertTrue(builder.should_bundle_linux_shared_library(Path(library)))
        for library in ("/lib64/libc.so.6", "/lib/ld-linux-aarch64.so.1",
                        "/lib64/ld-linux-x86-64.so.2"):
            self.assertFalse(builder.should_bundle_linux_shared_library(Path(library)))

    def test_native_platform_mapping_and_unsupported_host(self):
        for machine, expected in (("x86_64", "linux-amd64"), ("aarch64", "linux-arm64")):
            with patch.object(smoke.platform, "machine", return_value=machine), patch.object(smoke.sys, "platform", "linux"):
                self.assertEqual(smoke.native_platform(), expected)
        with patch.object(smoke.platform, "machine", return_value="riscv64"):
            with self.assertRaises(SystemExit):
                smoke.native_platform()

    def test_arm64_requires_its_own_runtime_and_checks_bytes(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "arm64.zip").write_bytes(b"arm64 artifact")
            artifact = {"filename": "arm64.zip", "sha256": smoke.sha256_file(root / "arm64.zip")}
            with patch.object(smoke, "native_platform", return_value="linux-arm64"):
                with self.assertRaises(SystemExit):
                    smoke.artifact_path(root, {"artifacts": {"linux-amd64": artifact}}, "runtime", False)
                owner = {"artifacts": {"linux-arm64": artifact}}
                self.assertEqual(smoke.artifact_path(root, owner, "runtime", False), root / "arm64.zip")
                (root / "arm64.zip").write_bytes(b"changed")
                with self.assertRaises(SystemExit):
                    smoke.artifact_path(root, owner, "runtime", False)

    def test_wrong_runtime_build_target_fails_before_work_directory_removal(self):
        builder_path = AGENT / "semantic-runtime/siglip2-onnx/build_runtime_pack.py"
        other = "linux-arm64" if builder.detect_platform() == "linux-amd64" else "linux-amd64"
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            sentinel = root / "keep"
            sentinel.write_text("do not delete")
            result = subprocess.run([sys.executable, str(builder_path), "--platform", other,
                                     "--work-dir", str(root)], capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("require a native", result.stderr)
            self.assertTrue(sentinel.exists())

    def test_attestation_cannot_substitute_amd64_for_arm64(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "semantic-models.json").write_text("{}")
            attestation = root / "proof.json"
            attestation.write_text(json.dumps({"schemaVersion": 1, "product": verify.PRODUCT,
                                               "platform": "linux-amd64"}))
            args = argparse.Namespace(assets_dir=root, attestation=attestation, platform="linux-arm64")
            with patch.object(verify, "parse_args", return_value=args):
                with self.assertRaisesRegex(SystemExit, "platform is invalid"):
                    verify.main()

    def test_recommended_archive_selects_requested_architecture(self):
        registry = {"recommendedRuntimePack": "runtime", "runtimePacks": [
            {"id": "runtime", "artifacts": {
                "linux-amd64": {"filename": "amd64.zip"},
                "linux-arm64": {"filename": "arm64.zip"},
            }}]}
        result = release.recommended_archive_path(registry, "recommendedRuntimePack", "runtimePacks",
                                                  Path("packs"), allow_default=False, platform="linux-arm64")
        self.assertEqual(result, Path("packs/arm64.zip"))


if __name__ == "__main__":
    unittest.main()
