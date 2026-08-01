# Timich SigLIP 2 ONNX Runtime

This directory contains the production helper-side runtime server used by
`timich-semantic-helper` when a SigLIP 2 ONNX model pack is installed.

In normal Agent runs, `timich-agent` manages this server automatically when an
ONNX model pack is installed. Native bundles auto-detect this directory next to
the `timich-agent` executable. If `.venv/bin/python` or `venv/bin/python` is
present in this directory, the Agent uses it; otherwise it falls back to
`python3` on `PATH`. Override with `semanticRuntime.onnxRuntime.*` or
`TIMICH_AGENT_SEMANTIC_ONNX_*`.

When shipped as a downloadable semantic runtime pack, the zip root must include
`timich-runtime.json`. `pythonPath` is required and must name the executable
Python bundled in the pack; runtime-pack installs do not fall back to host
Python:

```json
{
  "schemaVersion": 1,
  "product": "timich-semantic-runtime-pack",
  "runtime": "onnxruntime",
  "serverPath": "semantic-runtime/siglip2-onnx/server.py",
  "pythonPath": ".venv/bin/python"
}
```

Build a current-platform pack from the agent source root:

```sh
make semantic-runtime-pack
```

This creates:

- `dist/semantic-runtime-packs/*.zip`
- `*.zip.sha256`
- `*.spdx.json`
- `*.metadata.json`
- `*.registry.json` when `SEMANTIC_RUNTIME_PACK_BASE_URL` is set

Validate a built artifact before publishing:

```sh
make semantic-runtime-pack-validate
```

For a release candidate, validate checksum, metadata, SBOM, registry sidecar,
signature, bundled Python presence, and a runtime import smoke test:

```sh
make semantic-runtime-pack-validate \
  SEMANTIC_RUNTIME_PACK_PUBLIC_KEY=/path/to/release-signing-public-key.pem \
  SEMANTIC_RUNTIME_PACK_REQUIRE_SIGNATURE=1 \
  SEMANTIC_RUNTIME_PACK_REQUIRE_BUNDLED_PYTHON=1 \
  SEMANTIC_RUNTIME_PACK_SMOKE_IMPORT=1
```

The builder creates `.venv` with `python -m venv --copies` because the Agent
rejects symlinks while safely extracting runtime packs. It installs wheels from
`requirements.txt` with `--only-binary=:all:` by default. Set
`SEMANTIC_RUNTIME_PACK_ALLOW_SOURCE_BUILDS=1` only for controlled release
machines where native source builds are intended.

Before a replacement becomes active, the Agent launches its exact bundled
server/Python pair and verifies both text and image embeddings against every
installed compatible model layout. A failed layout keeps the previous runtime
pack active.

Runtime startup only sets `PYTHONHOME` for a Python environment that carries a
bundled standard library, checked by the presence of `encodings/__init__.py`
under the runtime root. This keeps ordinary developer `python -m venv`
environments usable while still isolating release runtime packs.

If `SEMANTIC_RUNTIME_PACK_BASE_URL` is set, the pack builder also writes a
`*.registry.json` fragment. Merge that fragment with the model-pack manifest
before publishing the release:

```sh
make semantic-model-registry
```

The merged `dist/semantic-models.json` is the Admin UI registry consumed by
release binaries.

Older NAS platforms can have older glibc/OpenSSL libraries than a current
Debian or Ubuntu build container. For QNAP-style native validation, build on a
manylinux2014-compatible image and pass
`SEMANTIC_RUNTIME_PACK_REQUIREMENTS=semantic-runtime/siglip2-onnx/requirements-legacy-linux.txt`;
that uses ONNX Runtime 1.16.3 and NumPy 1.x because newer ONNX Runtime wheels
require newer Linux ABIs. The builder copies Python standard-library files and
runtime owned shared-library dependencies such as OpenSSL into the pack, and
runtime startup prepends the bundled `.venv/lib` on Linux.

For release-grade native/QNAP artifacts, prefer a relocatable Python runtime
prepared for the target platform and pass it as:

```sh
make semantic-runtime-pack \
  SEMANTIC_RUNTIME_PACK_PYTHON_RUNTIME_ROOT=/path/to/portable-python-root
```

When this is omitted, the builder creates a local venv from
`SEMANTIC_RUNTIME_PACK_PYTHON` or `python3`, which is useful for development
smoke packs but may still depend on the host Python distribution. Do not publish
that development-venv output as a native/QNAP release artifact unless the strict
validation command above passes on the target platform. The builder carries the
macOS framework helper app and standard library when needed so local framework
Python packs can pass import smoke, but release-owned relocatable Python roots
are still preferred for native/QNAP artifacts.

To produce a detached RSA signature with OpenSSL:

```sh
make semantic-runtime-pack \
  SEMANTIC_RUNTIME_PACK_SIGNING_KEY=/path/to/release-signing-key.pem
```

Verify it with:

```sh
openssl dgst -sha256 \
  -verify /path/to/release-signing-public-key.pem \
  -signature dist/semantic-runtime-packs/<artifact>.zip.sig \
  dist/semantic-runtime-packs/<artifact>.zip
```

For manual troubleshooting, run it next to an installed model-pack runtime
layout:

```sh
python3 -m venv .venv-siglip2
. .venv-siglip2/bin/activate
pip install -r semantic-runtime/siglip2-onnx/requirements.txt

python semantic-runtime/siglip2-onnx/server.py \
  --runtime-layout /path/to/semantic-model-packs/timich-siglip2-base-patch16-224-multilingual-v1/runtime \
  --host 127.0.0.1 \
  --port 19188 \
  --provider cpu
```

Then point the bundled helper at the long-lived runtime:

```sh
export TIMICH_SEMANTIC_ONNX_SERVER_URL=http://127.0.0.1:19188
./timich-semantic-helper inspect --runtime-layout /path/to/runtime
```

For Intel NAS hardware with OpenVINO-enabled ONNX Runtime, use:

```sh
python semantic-runtime/siglip2-onnx/server.py \
  --runtime-layout /path/to/runtime \
  --provider openvino:GPU
```

The server keeps ONNX sessions and the SigLIP 2 processor loaded so backfill and
query-time text embedding avoid per-request model startup cost.
