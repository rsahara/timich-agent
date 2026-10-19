#!/usr/bin/env python3
"""Exercise release registry assembly with production runtime fragments."""

import argparse
import copy
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest


AGENT = Path(__file__).resolve().parents[2]
MERGER = AGENT / "tools/semantic/merge_semantic_model_registry.py"
SPEC = importlib.util.spec_from_file_location(
    "runtime_builder", AGENT / "semantic-runtime/siglip2-onnx/build_runtime_pack.py")
builder = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(builder)


class RegistryMergeTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.inputs = self.root / "runtime-packs"
        self.inputs.mkdir()
        self.fragments = {}
        for platform in ("linux-amd64", "linux-arm64"):
            artifact = self.inputs / f"runtime-{platform}.zip"
            artifact.write_bytes(platform.encode())
            args = argparse.Namespace(
                pack_id=builder.DEFAULT_PACK_ID, pack_name=builder.DEFAULT_PACK_NAME,
                version="0.5.0", license=builder.DEFAULT_LICENSE, platform=platform,
                artifact_base_url="https://example.com/releases/0.5.0")
            self.fragments[platform] = builder.build_registry_fragment(
                args, artifact, builder.sha256_file(artifact))

    def write_fragment(self, name, fragment):
        path = self.inputs / f"{name}.registry.json"
        path.write_text(json.dumps(fragment), encoding="utf-8")
        return path

    def merge(self, fragments, *options):
        paths = [self.write_fragment(str(i), item) for i, item in enumerate(fragments)]
        output = self.root / "merged.json"
        output.unlink(missing_ok=True)
        result = subprocess.run(
            [sys.executable, str(MERGER), "--output", str(output), "--version", "0.5.0",
             *options, *map(str, paths)], capture_output=True, text=True)
        return result, output

    def assert_success(self, result, output):
        self.assertEqual(result.returncode, 0, result.stderr)
        return json.loads(output.read_text(encoding="utf-8"))

    def assert_rejected(self, result, output, message):
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(message, result.stderr)
        self.assertFalse(output.exists(), "a failed merge must not publish a partial registry")

    def test_builder_fragments_merge_in_either_order(self):
        fragments = list(self.fragments.values())
        first = self.assert_success(*self.merge(fragments))
        second = self.assert_success(*self.merge(list(reversed(fragments))))
        self.assertEqual(first, second)
        self.assertEqual(first["version"], "0.5.0")
        self.assertEqual(first["recommendedRuntimePack"], builder.DEFAULT_PACK_ID)
        self.assertEqual(len(first["runtimePacks"]), 1)
        expected = {}
        for fragment in fragments:
            expected.update(fragment["runtimePacks"][0]["artifacts"])
        self.assertEqual(first["runtimePacks"][0]["artifacts"], expected)

    def test_identical_platform_overlap_is_idempotent(self):
        fragments = list(self.fragments.values())
        merged = self.assert_success(*self.merge(fragments))
        repeated = self.assert_success(*self.merge([merged, *fragments, merged]))
        self.assertEqual(repeated, merged)

    def test_common_metadata_conflicts_are_rejected(self):
        for field in ("name", "version", "runtime", "license"):
            with self.subTest(field=field):
                changed = copy.deepcopy(self.fragments["linux-arm64"])
                changed["runtimePacks"][0][field] += "-different"
                result, output = self.merge([self.fragments["linux-amd64"], changed])
                self.assert_rejected(result, output, "has different metadata")

    def test_same_platform_artifact_conflicts_are_rejected(self):
        original = self.fragments["linux-amd64"]
        for field, value in (("filename", "different.zip"), ("sha256", "f" * 64),
                             ("url", "https://example.com/different.zip"), ("sizeBytes", 100)):
            with self.subTest(field=field):
                changed = copy.deepcopy(original)
                changed["runtimePacks"][0]["artifacts"]["linux-amd64"][field] = value
                result, output = self.merge([original, self.fragments["linux-arm64"], changed])
                self.assert_rejected(result, output, "conflicting artifact for 'linux-amd64'")

    def test_merge_validates_incoming_artifacts(self):
        changed = copy.deepcopy(self.fragments["linux-arm64"])
        del changed["runtimePacks"][0]["artifacts"]["linux-arm64"]["sizeBytes"]
        result, output = self.merge([self.fragments["linux-amd64"], changed])
        self.assert_rejected(result, output, "sizeBytes must be an integer")

    def test_release_url_rewrite_applies_to_both_architectures(self):
        base = "https://example.com/releases/final"
        merged = self.assert_success(*self.merge(list(self.fragments.values()), "--base-url", base))
        for artifact in merged["runtimePacks"][0]["artifacts"].values():
            self.assertEqual(artifact["url"], f"{base}/{artifact['filename']}")

    def model_fragment(self):
        artifact = copy.deepcopy(self.fragments["linux-amd64"]["runtimePacks"][0]["artifacts"]["linux-amd64"])
        return {
            "schemaVersion": 1, "product": "timich-semantic-models", "recommended": "model",
            "models": [{"id": "model", "name": "Model", "vectorSpaceId": "space",
                        "inputKind": "image", "embeddingDim": 8, "artifacts": {"default": artifact}}],
        }

    def test_model_duplicates_remain_strict(self):
        model = self.model_fragment()
        merged = self.assert_success(*self.merge([model, model]))
        self.assertEqual(merged["models"], model["models"])
        changed = copy.deepcopy(model)
        changed["models"][0]["artifacts"]["linux-arm64"] = changed["models"][0]["artifacts"].pop("default")
        result, output = self.merge([model, changed])
        self.assert_rejected(result, output, "duplicate model 'model' has different metadata")

    def test_make_discovers_and_merges_model_and_runtime_fragments(self):
        for platform, fragment in self.fragments.items():
            self.write_fragment(platform, fragment)
        model_dir = self.root / "model-packs"
        model_dir.mkdir()
        model = self.model_fragment()
        (model_dir / "manifest.json").write_text(json.dumps(model), encoding="utf-8")
        output = self.root / "dist/semantic-models.json"
        result = subprocess.run(
            ["make", "--no-print-directory", "semantic-model-registry",
             f"DIST_DIR={output.parent}", f"SEMANTIC_MODEL_REGISTRY={output}",
             "SEMANTIC_MODEL_REGISTRY_VERSION=0.5.0", "SEMANTIC_MODEL_REGISTRY_INPUTS=",
             "SEMANTIC_MODEL_REGISTRY_BASE_URL=", "SEMANTIC_MODEL_REGISTRY_RECOMMENDED=",
             "SEMANTIC_MODEL_REGISTRY_RECOMMENDED_RUNTIME_PACK=",
             f"SEMANTIC_MODEL_PACK_DIR={model_dir}", f"SEMANTIC_RUNTIME_PACK_DIR={self.inputs}"],
            cwd=AGENT, capture_output=True, text=True)
        merged = self.assert_success(result, output)
        self.assertEqual(merged["recommended"], "model")
        self.assertEqual(merged["models"], model["models"])
        self.assertEqual(merged["recommendedRuntimePack"], builder.DEFAULT_PACK_ID)
        self.assertEqual(len(merged["runtimePacks"]), 1)
        self.assertEqual(set(merged["runtimePacks"][0]["artifacts"]), set(self.fragments))


if __name__ == "__main__":
    unittest.main()
