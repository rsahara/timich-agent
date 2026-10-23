#!/usr/bin/env bash
set -euo pipefail

# Keep the staging reader and semantic validators covered after publisher retirement.
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PYTHONPATH="$script_dir/../semantic" python3 "$script_dir/../semantic/test_semantic_archive_budget.py"
test_root=$(mktemp -d)
trap 'rm -rf "$test_root"' EXIT
fake_bin="$test_root/bin"
state_dir="$test_root/state"
download_call_dir="$state_dir/download-calls"
mkdir -p "$fake_bin" "$download_call_dir" "$test_root/go-cache"
public_sha=0123456789012345678901234567890123456789

cat > "$fake_bin/gh" <<'FAKE_GH'
#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" != release ] || [ "${3:-}" != v0.4.0-rc.2-semantic-stage ]; then
  : > "$FAKE_GH_STATE/unexpected-operation"
  exit 1
fi
case "${2:-}" in
  view)
    state=$(cat "$FAKE_GH_STATE/staging-release-state")
    [ "$state" != missing ] || exit 1
    is_draft=true
    [ "$state" != published ] || is_draft=false
    jq -n --argjson isDraft "$is_draft" \
      --arg target "$(cat "$FAKE_GH_STATE/staging-release-target")" \
      --argjson assets "$(cat "$FAKE_GH_STATE/staging-assets.json")" \
      '{isDraft: $isDraft, targetCommitish: $target, assets: $assets}'
    ;;
  download)
    pattern=""
    output_dir=""
    previous=""
    for value in "$@"; do
      if [ "$previous" = --pattern ]; then pattern="$value"; fi
      if [ "$previous" = --dir ]; then output_dir="$value"; fi
      previous="$value"
    done
    [ -n "$pattern" ] && [ -n "$output_dir" ] && [ -f "$FAKE_GH_DOWNLOAD_DIR/$pattern" ] || exit 1
    : > "$FAKE_GH_STATE/download-calls/$pattern"
    cp "$FAKE_GH_DOWNLOAD_DIR/$pattern" "$output_dir/$pattern"
    if [ "${FAKE_GH_OVERSIZE_DOWNLOAD:-}" = "$pattern" ]; then
      current_size=$(wc -c < "$output_dir/$pattern" | tr -d '[:space:]')
      first_disallowed_size=$(( ((current_size + 1023) / 1024) * 1024 + 1 ))
      append_size=$((first_disallowed_size - current_size))
      dd if=/dev/zero bs=1 count="$append_size" >> "$output_dir/$pattern" 2>/dev/null
      : > "$FAKE_GH_STATE/download-calls/oversize-$pattern"
    fi
    ;;
  *)
    : > "$FAKE_GH_STATE/unexpected-operation"
    exit 1
    ;;
esac
FAKE_GH
chmod +x "$fake_bin/gh"

run_downloader() {
  local output_dir
  output_dir=$(mktemp -d "$test_root/delivery.XXXXXX")
  PATH="$fake_bin:$PATH" GOCACHE="$test_root/go-cache" GH_TOKEN=test-token \
    FAKE_GH_STATE="$state_dir" FAKE_GH_DOWNLOAD_DIR="$state_dir/downloads" \
    FAKE_GH_OVERSIZE_DOWNLOAD=${FAKE_GH_OVERSIZE_DOWNLOAD:-} \
    bash "$script_dir/download_semantic_release_assets.sh" \
      rsahara/timich-agent v0.4.0-rc.2-semantic-stage "$public_sha" \
      https://github.com/rsahara/timich-agent/releases/download/v0.4.0-rc.2 "$output_dir"
}
verify_attestation() {
  python3 "$script_dir/../semantic/verify_semantic_smoke_attestation.py" \
    --assets-dir "$state_dir/downloads" --platform linux-amd64 \
    --attestation "$state_dir/semantic-smoke-attestation.json"
}
reset_staging() {
  printf '%s\n' "$1" > "$state_dir/staging-release-state"
  printf '%s\n' "${2:-$public_sha}" > "$state_dir/staging-release-target"
  printf '%s\n' "${3:-[]}" > "$state_dir/staging-assets.json"
  find "$download_call_dir" -mindepth 1 -maxdepth 1 -type f -delete
}
download_was_called() {
  [ -f "$download_call_dir/$1" ]
}
any_download_was_called() {
  find "$download_call_dir" -mindepth 1 -maxdepth 1 -type f ! -name 'oversize-*' -print -quit | grep -q .
}
for state in missing published; do
  reset_staging "$state"
  if run_downloader >/dev/null 2>&1; then
    echo "expected $state staging release to be rejected" >&2
    exit 1
  fi
  if any_download_was_called; then exit 1; fi
done
reset_staging draft 0000000000000000000000000000000000000000
if run_downloader >/dev/null 2>&1; then
  echo 'expected mismatched staging source to be rejected' >&2
  exit 1
fi
if any_download_was_called; then exit 1; fi

mkdir -p "$state_dir/downloads"
printf '%s\n' '{"schemaVersion":1,"product":"timich-semantic-models","models":[],"runtimePacks":[]}' \
  > "$state_dir/downloads/semantic-models.json"
registry_size=$(wc -c < "$state_dir/downloads/semantic-models.json" | tr -d '[:space:]')
registry_sha=$(sha256sum "$state_dir/downloads/semantic-models.json" | awk '{print $1}')
incomplete_semantic_assets=$(jq -cn \
  --argjson size "$registry_size" \
  --arg digest "sha256:$registry_sha" \
  '[{name:"semantic-models.json",size:$size,digest:$digest}]')
reset_staging draft "$public_sha" "$incomplete_semantic_assets"
if run_downloader >/dev/null 2>&1; then
  echo "expected incomplete semantic release assets to fail validation" >&2
  exit 1
fi

printf 'model artifact\n' > "$state_dir/downloads/model.zip"
model_sha=$(sha256sum "$state_dir/downloads/model.zip" | awk '{print $1}')
model_size=$(wc -c < "$state_dir/downloads/model.zip" | tr -d '[:space:]')
printf '%s  model.zip\n' "$model_sha" > "$state_dir/downloads/model.zip.sha256"
printf '{}\n' > "$state_dir/downloads/model.metadata.json"
printf '%s\n' '{"spdxVersion":"SPDX-2.3"}' > "$state_dir/downloads/model.spdx.json"
printf 'runtime artifact\n' > "$state_dir/downloads/runtime.zip"
runtime_sha=$(sha256sum "$state_dir/downloads/runtime.zip" | awk '{print $1}')
runtime_size=$(wc -c < "$state_dir/downloads/runtime.zip" | tr -d '[:space:]')
printf '%s  runtime.zip\n' "$runtime_sha" > "$state_dir/downloads/runtime.zip.sha256"
printf '{}\n' > "$state_dir/downloads/runtime.metadata.json"
printf '%s\n' '{"spdxVersion":"SPDX-2.3"}' > "$state_dir/downloads/runtime.spdx.json"
jq -n \
  --arg model_sha "$model_sha" \
  --argjson model_size "$model_size" \
  --arg runtime_sha "$runtime_sha" \
  --argjson runtime_size "$runtime_size" \
  '{schemaVersion:1,product:"timich-semantic-models",recommended:"model",recommendedRuntimePack:"runtime",models:[{id:"model",name:"Model",version:"1.0.0",vectorSpaceId:"model/d4",embeddingDim:4,inputKind:"image",runtime:"onnxruntime",artifacts:{default:{filename:"model.zip",url:"https://github.com/rsahara/timich-agent/releases/download/v0.4.0-rc.2/model.zip",sha256:$model_sha,sizeBytes:$model_size}}}],runtimePacks:[{id:"runtime",name:"Runtime",version:"1.0.0",runtime:"onnxruntime",artifacts:{"linux-amd64":{filename:"runtime.zip",url:"https://github.com/rsahara/timich-agent/releases/download/v0.4.0-rc.2/runtime.zip",sha256:$runtime_sha,sizeBytes:$runtime_size}}}]}' \
  > "$state_dir/downloads/semantic-models.json"

semantic_assets_json='[]'
for semantic_name in semantic-models.json model.zip model.zip.sha256 model.metadata.json model.spdx.json runtime.zip runtime.zip.sha256 runtime.metadata.json runtime.spdx.json; do
  semantic_size=$(wc -c < "$state_dir/downloads/$semantic_name" | tr -d '[:space:]')
  semantic_sha=$(sha256sum "$state_dir/downloads/$semantic_name" | awk '{print $1}')
  semantic_asset=$(jq -cn \
    --arg name "$semantic_name" \
    --argjson size "$semantic_size" \
    --arg digest "sha256:$semantic_sha" \
    '{name:$name,size:$size,digest:$digest}')
  semantic_assets_json=$(jq -cn \
    --argjson assets "$semantic_assets_json" \
    --argjson asset "$semantic_asset" \
    '$assets + [$asset]')
done
reset_staging draft "$public_sha" "$semantic_assets_json"
if run_downloader >/dev/null 2>&1; then
  echo "expected non-ZIP semantic packs to fail validation" >&2
  exit 1
fi

python3 - "$state_dir/downloads" <<'PY'
from __future__ import annotations

import hashlib
import json
from pathlib import Path
import sys
import zipfile

root = Path(sys.argv[1])
base_url = "https://github.com/rsahara/timich-agent/releases/download/v0.4.0-rc.2"


def write_json(path: Path, payload: dict) -> bytes:
    raw = (json.dumps(payload, separators=(",", ":"), sort_keys=True) + "\n").encode()
    path.write_bytes(raw)
    return raw


def write_zip(path: Path, files: dict[str, bytes]) -> None:
    with zipfile.ZipFile(path, "w", compression=zipfile.ZIP_DEFLATED) as archive:
        for name, payload in files.items():
            info = zipfile.ZipInfo(name)
            info.external_attr = (0o755 if name == "python/bin/python3" else 0o644) << 16
            archive.writestr(info, payload)


model_zip = root / "model.zip"
write_zip(model_zip, {
    "timich-model.json": (json.dumps({
        "schemaVersion": 1,
        "product": "timich-semantic-model-pack",
        "modelId": "model",
        "vectorSpaceId": "model/d4",
        "embeddingDim": 4,
        "inputKind": "image",
        "runtime": "onnxruntime",
        "imageModel": "image.onnx",
        "textModel": "text.onnx",
        "tokenizer": "tokenizer.json",
    }) + "\n").encode(),
    "image.onnx": b"image-model\n",
    "text.onnx": b"text-model\n",
    "tokenizer.json": b"{}\n",
})
model_sha = hashlib.sha256(model_zip.read_bytes()).hexdigest()
(root / "model.zip.sha256").write_text(f"{model_sha}  model.zip\n")
model_sbom = write_json(root / "model.spdx.json", {
    "spdxVersion": "SPDX-2.3",
    "SPDXID": "SPDXRef-DOCUMENT",
    "packages": [{"SPDXID": "SPDXRef-Package-ModelPack", "name": "model"}],
})
write_json(root / "model.metadata.json", {
    "schemaVersion": 1,
    "product": "timich-semantic-model-pack-artifact",
    "modelPack": {
        "id": "model",
        "name": "Model",
        "version": "1.0.0",
        "vectorSpaceId": "model/d4",
        "embeddingDim": 4,
        "inputKind": "image",
        "runtime": "onnxruntime",
        "artifact": {"filename": "model.zip", "sha256": model_sha, "sizeBytes": model_zip.stat().st_size},
        "sbom": {"filename": "model.spdx.json", "sha256": hashlib.sha256(model_sbom).hexdigest(), "sizeBytes": len(model_sbom)},
    },
})

runtime_zip = root / "runtime.zip"
write_zip(runtime_zip, {
    "timich-runtime.json": (json.dumps({
        "schemaVersion": 1,
        "product": "timich-semantic-runtime-pack",
        "runtime": "onnxruntime",
        "serverPath": "server.py",
        "pythonPath": "python/bin/python3",
    }) + "\n").encode(),
    "server.py": b"print('runtime')\n",
    "python/bin/python3": b"#!/bin/sh\nexit 0\n",
    "python/pyvenv.cfg": b"home = bundled\n",
    "python/lib/python3.12/encodings/__init__.py": b"# bundled stdlib marker\n",
})
runtime_sha = hashlib.sha256(runtime_zip.read_bytes()).hexdigest()
(root / "runtime.zip.sha256").write_text(f"{runtime_sha}  runtime.zip\n")
runtime_sbom = write_json(root / "runtime.spdx.json", {
    "spdxVersion": "SPDX-2.3",
    "SPDXID": "SPDXRef-DOCUMENT",
    "packages": [{"SPDXID": "SPDXRef-Package-RuntimePack", "name": "runtime"}],
})
write_json(root / "runtime.metadata.json", {
    "schemaVersion": 1,
    "product": "timich-semantic-runtime-pack-artifact",
    "runtimePack": {
        "id": "runtime",
        "name": "Runtime",
        "version": "1.0.0",
        "runtime": "onnxruntime",
        "platform": "linux-amd64",
        "artifact": {"filename": "runtime.zip", "sha256": runtime_sha, "sizeBytes": runtime_zip.stat().st_size},
        "sbom": {"filename": "runtime.spdx.json", "sha256": hashlib.sha256(runtime_sbom).hexdigest(), "sizeBytes": len(runtime_sbom)},
    },
})

write_json(root / "semantic-models.json", {
    "schemaVersion": 1,
    "product": "timich-semantic-models",
    "recommended": "model",
    "recommendedRuntimePack": "runtime",
    "models": [{
        "id": "model",
        "name": "Model",
        "version": "1.0.0",
        "vectorSpaceId": "model/d4",
        "embeddingDim": 4,
        "inputKind": "image",
        "runtime": "onnxruntime",
        "artifacts": {"default": {"filename": "model.zip", "url": f"{base_url}/model.zip", "sha256": model_sha, "sizeBytes": model_zip.stat().st_size}},
    }],
    "runtimePacks": [{
        "id": "runtime",
        "name": "Runtime",
        "version": "1.0.0",
        "runtime": "onnxruntime",
        "artifacts": {"linux-amd64": {"filename": "runtime.zip", "url": f"{base_url}/runtime.zip", "sha256": runtime_sha, "sizeBytes": runtime_zip.stat().st_size}},
    }],
})
PY

semantic_assets_json='[]'
for semantic_name in semantic-models.json model.zip model.zip.sha256 model.metadata.json model.spdx.json runtime.zip runtime.zip.sha256 runtime.metadata.json runtime.spdx.json; do
  semantic_size=$(wc -c < "$state_dir/downloads/$semantic_name" | tr -d '[:space:]')
  semantic_sha=$(sha256sum "$state_dir/downloads/$semantic_name" | awk '{print $1}')
  semantic_asset=$(jq -cn \
    --arg name "$semantic_name" \
    --argjson size "$semantic_size" \
    --arg digest "sha256:$semantic_sha" \
    '{name:$name,size:$size,digest:$digest}')
  semantic_assets_json=$(jq -cn \
    --argjson assets "$semantic_assets_json" \
    --argjson asset "$semantic_asset" \
    '$assets + [$asset]')
done

registry_asset=$(jq -c '.[] | select(.name == "semantic-models.json")' <<<"$semantic_assets_json")
valid_fake_digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa

cp "$state_dir/downloads/semantic-models.json" "$state_dir/semantic-models.original.json"
jq '.models[0].artifacts["linux-amd64"] = .models[0].artifacts.default' \
  "$state_dir/semantic-models.original.json" > "$state_dir/downloads/semantic-models.json"
alias_registry_size=$(wc -c < "$state_dir/downloads/semantic-models.json" | tr -d '[:space:]')
alias_registry_sha=$(sha256sum "$state_dir/downloads/semantic-models.json" | awk '{print $1}')
alias_registry_asset=$(jq -cn \
  --argjson size "$alias_registry_size" \
  --arg digest "sha256:$alias_registry_sha" \
  '{name:"semantic-models.json",size:$size,digest:$digest}')
alias_semantic_assets_json=$(jq -cn \
  --argjson assets "$semantic_assets_json" \
  --argjson registry "$alias_registry_asset" \
  '$assets | map(if .name == "semantic-models.json" then $registry else . end)')
reset_staging draft "$public_sha" "$alias_semantic_assets_json"
if run_downloader >/dev/null 2>&1; then
  echo "expected same-owner platform alias to fail before artifact downloads" >&2
  exit 1
fi
if download_was_called model.zip || download_was_called runtime.zip; then
  echo "same-owner platform alias triggered an artifact download" >&2
  exit 1
fi
mv "$state_dir/semantic-models.original.json" "$state_dir/downloads/semantic-models.json"

reset_staging draft "$public_sha" "$semantic_assets_json"
FAKE_GH_OVERSIZE_DOWNLOAD=semantic-models.json
if run_downloader >/dev/null 2>&1; then
  echo "expected overlong registry payload to fail during download" >&2
  exit 1
fi
unset FAKE_GH_OVERSIZE_DOWNLOAD
if download_was_called oversize-semantic-models.json; then
  echo "overlong registry exceeded the declared GNU Bash file limit" >&2
  exit 1
fi

oversized_staging_assets=$(jq -cn \
  --argjson registry "$registry_asset" \
  --arg digest "$valid_fake_digest" \
  '[$registry, {name:"oversized-debug.zip",size:8589934593,digest:$digest}]')
reset_staging draft "$public_sha" "$oversized_staging_assets"
if run_downloader >/dev/null 2>&1; then
  echo "expected oversized staging asset metadata to fail before download" >&2
  exit 1
fi
if any_download_was_called; then
  echo "oversized staging asset triggered a download" >&2
  exit 1
fi

excessive_staging_assets=$(jq -cn \
  --argjson registry "$registry_asset" \
  --arg digest "$valid_fake_digest" \
  '[$registry] + [range(0; 64) | {name:("debug-" + tostring + ".zip"),size:1,digest:$digest}]')
reset_staging draft "$public_sha" "$excessive_staging_assets"
if run_downloader >/dev/null 2>&1; then
  echo "expected excessive staging asset count to fail before download" >&2
  exit 1
fi
if any_download_was_called; then
  echo "excessive staging asset count triggered a download" >&2
  exit 1
fi

over_budget_staging_assets=$(jq -cn \
  --argjson registry "$registry_asset" \
  --arg digest "$valid_fake_digest" \
  '[$registry, {name:"debug-a.zip",size:6442450944,digest:$digest}, {name:"debug-b.zip",size:6442450944,digest:$digest}]')
reset_staging draft "$public_sha" "$over_budget_staging_assets"
if run_downloader >/dev/null 2>&1; then
  echo "expected staging total-size budget to fail before download" >&2
  exit 1
fi
if any_download_was_called; then
  echo "over-budget staging assets triggered a download" >&2
  exit 1
fi

reset_staging draft "$public_sha" "$semantic_assets_json"
if python3 "$script_dir/../semantic/smoke_semantic_release.py" \
  --assets-dir "$state_dir/downloads" \
  --helper-path "$(command -v true)" \
  --output "$state_dir/semantic-smoke-attestation.json" \
  --timeout 2 >/dev/null 2>&1; then
  echo "expected fake bundled Python/runtime fixture to fail executable smoke" >&2
  exit 1
fi
python3 - "$state_dir/downloads" "$state_dir/semantic-smoke-attestation.json" <<'PY'
from __future__ import annotations

import hashlib
import json
from pathlib import Path
import sys

root = Path(sys.argv[1])
output = Path(sys.argv[2])
ignored = {"semantic-asset-snapshot.json", "semantic-smoke-attestation.json", "obsolete-model.zip"}


def sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


assets = [
    {"name": path.name, "size": path.stat().st_size, "sha256": sha256(path)}
    for path in sorted(root.iterdir())
    if path.is_file() and path.name not in ignored
]
registry = json.loads((root / "semantic-models.json").read_text())
output.write_text(json.dumps({
    "schemaVersion": 1,
    "product": "timich-semantic-release-smoke",
    "platform": "linux-amd64",
    "recommendedModel": "model",
    "recommendedRuntimePack": "runtime",
    "registrySha256": sha256(root / "semantic-models.json"),
    "assets": assets,
    "interpreter": {
        "implementation": "CPython",
        "version": "3.12.0",
        "executable": "/isolated/runtime/python",
        "imports": {"numpy": "test", "onnxruntime": "test", "PIL": "test", "transformers": "test"},
    },
    "runtime": {
        "health": "ok",
        "modelId": "model",
        "vectorSpaceId": "model/d4",
        "embeddingDim": 4,
        "inputKind": "image",
        "runtime": "onnxruntime",
        "textEmbedding": "ok",
        "imageEmbedding": "ok",
    },
}, sort_keys=True) + "\n")
PY
run_downloader >/dev/null
verify_attestation >/dev/null
download_was_called semantic-models.json

cp "$state_dir/semantic-smoke-attestation.json" "$state_dir/semantic-smoke-attestation.valid.json"
jq '.registrySha256 = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"' \
  "$state_dir/semantic-smoke-attestation.valid.json" > "$state_dir/semantic-smoke-attestation.json"
reset_staging draft "$public_sha" "$semantic_assets_json"
if verify_attestation >/dev/null 2>&1; then
  echo "expected semantic smoke attestation digest mismatch to fail validation" >&2
  exit 1
fi
mv "$state_dir/semantic-smoke-attestation.valid.json" "$state_dir/semantic-smoke-attestation.json"

identity_mismatch_dir="$test_root/semantic-identity-mismatch"
mkdir -p "$identity_mismatch_dir"
cp "$state_dir/downloads"/semantic-models.json \
  "$state_dir/downloads"/model.zip \
  "$state_dir/downloads"/model.zip.sha256 \
  "$state_dir/downloads"/model.metadata.json \
  "$state_dir/downloads"/model.spdx.json \
  "$state_dir/downloads"/runtime.zip \
  "$state_dir/downloads"/runtime.zip.sha256 \
  "$state_dir/downloads"/runtime.metadata.json \
  "$state_dir/downloads"/runtime.spdx.json \
  "$identity_mismatch_dir/"
jq '.models[0].vectorSpaceId = "different/d4"' \
  "$identity_mismatch_dir/semantic-models.json" > "$identity_mismatch_dir/semantic-models.next.json"
mv "$identity_mismatch_dir/semantic-models.next.json" "$identity_mismatch_dir/semantic-models.json"
if SEMANTIC_MODEL_REGISTRY="$identity_mismatch_dir/semantic-models.json" \
  SEMANTIC_MODEL_PACK_DIR="$identity_mismatch_dir" \
  SEMANTIC_RUNTIME_PACK_DIR="$identity_mismatch_dir" \
  SEMANTIC_RELEASE_BASE_URL="https://github.com/rsahara/timich-agent/releases/download/v0.4.0-rc.2" \
  python3 "$script_dir/../semantic/validate_semantic_release.py" --validate-pack-layouts >/dev/null 2>&1; then
  echo "expected registry/metadata model identity mismatch to fail" >&2
  exit 1
fi

shared_owner_dir="$test_root/semantic-shared-owner"
cp -R "$state_dir/downloads" "$shared_owner_dir"
jq '
  .recommended = "model-b" |
  .models += [(.models[0] |
    .id = "model-b" |
    .name = "Model B" |
    .version = "2.0.0" |
    .vectorSpaceId = "model-b/d8" |
    .embeddingDim = 8)]
' "$state_dir/downloads/semantic-models.json" > "$shared_owner_dir/semantic-models.json"
if SEMANTIC_MODEL_REGISTRY="$shared_owner_dir/semantic-models.json" \
  SEMANTIC_MODEL_PACK_DIR="$shared_owner_dir" \
  SEMANTIC_RUNTIME_PACK_DIR="$shared_owner_dir" \
  SEMANTIC_RELEASE_BASE_URL="https://github.com/rsahara/timich-agent/releases/download/v0.4.0-rc.2" \
  python3 "$script_dir/../semantic/validate_semantic_release.py" --validate-pack-layouts >/dev/null 2>&1; then
  echo "expected cross-owner semantic artifact filename sharing to fail" >&2
  exit 1
fi

platform_collision_dir="$test_root/semantic-platform-collision"
mkdir -p "$platform_collision_dir"
jq '
  .runtimePacks[0].artifacts[" linux-amd64 "] = (
    .runtimePacks[0].artifacts["linux-amd64"] |
    .filename = "runtime-shadow.zip" |
    .url = "https://github.com/rsahara/timich-agent/releases/download/v0.4.0-rc.2/runtime-shadow.zip"
  )
' "$state_dir/downloads/semantic-models.json" > "$platform_collision_dir/semantic-models.json"
if platform_collision_error=$(SEMANTIC_MODEL_REGISTRY="$platform_collision_dir/semantic-models.json" \
  SEMANTIC_MODEL_PACK_DIR="$platform_collision_dir" \
  SEMANTIC_RUNTIME_PACK_DIR="$platform_collision_dir" \
  SEMANTIC_RELEASE_BASE_URL="https://github.com/rsahara/timich-agent/releases/download/v0.4.0-rc.2" \
  python3 "$script_dir/../semantic/validate_semantic_release.py" 2>&1); then
  echo "expected normalized semantic artifact platform collision to fail release validation" >&2
  exit 1
fi
if ! grep -Fq "artifact platform 'linux-amd64' is duplicated after normalization" <<<"$platform_collision_error"; then
  echo "normalized semantic artifact platform collision failed for the wrong reason: $platform_collision_error" >&2
  exit 1
fi
if merge_collision_error=$(python3 "$script_dir/../semantic/merge_semantic_model_registry.py" \
  --output "$platform_collision_dir/merged.json" \
  "$platform_collision_dir/semantic-models.json" 2>&1); then
  echo "expected normalized semantic artifact platform collision to fail registry merge" >&2
  exit 1
fi
if ! grep -Fq "artifact platform 'linux-amd64' is duplicated after normalization" <<<"$merge_collision_error"; then
  echo "normalized semantic artifact platform merge failed for the wrong reason: $merge_collision_error" >&2
  exit 1
fi

version_mismatch_dir="$test_root/semantic-version-mismatch"
mkdir -p "$version_mismatch_dir"
cp "$state_dir/downloads"/semantic-models.json \
  "$state_dir/downloads"/model.zip \
  "$state_dir/downloads"/model.zip.sha256 \
  "$state_dir/downloads"/model.metadata.json \
  "$state_dir/downloads"/model.spdx.json \
  "$state_dir/downloads"/runtime.zip \
  "$state_dir/downloads"/runtime.zip.sha256 \
  "$state_dir/downloads"/runtime.metadata.json \
  "$state_dir/downloads"/runtime.spdx.json \
  "$version_mismatch_dir/"
jq '.models[0].version = "2.0.0"' \
  "$version_mismatch_dir/semantic-models.json" > "$version_mismatch_dir/semantic-models.next.json"
mv "$version_mismatch_dir/semantic-models.next.json" "$version_mismatch_dir/semantic-models.json"
if SEMANTIC_MODEL_REGISTRY="$version_mismatch_dir/semantic-models.json" \
  SEMANTIC_MODEL_PACK_DIR="$version_mismatch_dir" \
  SEMANTIC_RUNTIME_PACK_DIR="$version_mismatch_dir" \
  SEMANTIC_RELEASE_BASE_URL="https://github.com/rsahara/timich-agent/releases/download/v0.4.0-rc.2" \
  python3 "$script_dir/../semantic/validate_semantic_release.py" --validate-pack-layouts >/dev/null 2>&1; then
  echo "expected registry/metadata model version mismatch to fail" >&2
  exit 1
fi

missing_version_dir="$test_root/semantic-missing-version"
cp -R "$version_mismatch_dir" "$missing_version_dir"
jq 'del(.models[0].version) | .models[0].vectorSpaceId = "model/d4"' \
  "$state_dir/downloads/semantic-models.json" > "$missing_version_dir/semantic-models.json"
if SEMANTIC_MODEL_REGISTRY="$missing_version_dir/semantic-models.json" \
  SEMANTIC_MODEL_PACK_DIR="$missing_version_dir" \
  SEMANTIC_RUNTIME_PACK_DIR="$missing_version_dir" \
  SEMANTIC_RELEASE_BASE_URL="https://github.com/rsahara/timich-agent/releases/download/v0.4.0-rc.2" \
  python3 "$script_dir/../semantic/validate_semantic_release.py" --validate-pack-layouts >/dev/null 2>&1; then
  echo "expected missing registry model version to fail" >&2
  exit 1
fi

missing_size_dir="$test_root/semantic-missing-artifact-size"
cp -R "$state_dir/downloads" "$missing_size_dir"
jq 'del(.models[0].artifacts.default.sizeBytes)' \
  "$state_dir/downloads/semantic-models.json" > "$missing_size_dir/semantic-models.json"
if SEMANTIC_MODEL_REGISTRY="$missing_size_dir/semantic-models.json" \
  SEMANTIC_MODEL_PACK_DIR="$missing_size_dir" \
  SEMANTIC_RUNTIME_PACK_DIR="$missing_size_dir" \
  SEMANTIC_RELEASE_BASE_URL="https://github.com/rsahara/timich-agent/releases/download/v0.4.0-rc.2" \
  python3 "$script_dir/../semantic/validate_semantic_release.py" --validate-pack-layouts >/dev/null 2>&1; then
  echo "expected missing registry artifact sizeBytes to fail" >&2
  exit 1
fi

oversized_pack_dir="$test_root/semantic-oversized-artifact"
cp -R "$state_dir/downloads" "$oversized_pack_dir"
jq '.runtimePacks[0].artifacts["linux-amd64"].sizeBytes = 8589934593' \
  "$state_dir/downloads/semantic-models.json" > "$oversized_pack_dir/semantic-models.json"
if SEMANTIC_MODEL_REGISTRY="$oversized_pack_dir/semantic-models.json" \
  SEMANTIC_MODEL_PACK_DIR="$oversized_pack_dir" \
  SEMANTIC_RUNTIME_PACK_DIR="$oversized_pack_dir" \
  SEMANTIC_RELEASE_BASE_URL="https://github.com/rsahara/timich-agent/releases/download/v0.4.0-rc.2" \
  python3 "$script_dir/../semantic/validate_semantic_release.py" --validate-pack-layouts >/dev/null 2>&1; then
  echo "expected oversized registry artifact sizeBytes to fail" >&2
  exit 1
fi

missing_python_dir="$test_root/semantic-missing-python"
mkdir -p "$missing_python_dir"
cp "$state_dir/downloads"/semantic-models.json \
  "$state_dir/downloads"/model.zip \
  "$state_dir/downloads"/model.zip.sha256 \
  "$state_dir/downloads"/model.metadata.json \
  "$state_dir/downloads"/model.spdx.json \
  "$state_dir/downloads"/runtime.zip \
  "$state_dir/downloads"/runtime.zip.sha256 \
  "$state_dir/downloads"/runtime.metadata.json \
  "$state_dir/downloads"/runtime.spdx.json \
  "$missing_python_dir/"
python3 - "$missing_python_dir" <<'PY'
from __future__ import annotations

import hashlib
import json
from pathlib import Path
import sys
import zipfile

root = Path(sys.argv[1])
runtime_zip = root / "runtime.zip"
with zipfile.ZipFile(runtime_zip, "w", compression=zipfile.ZIP_DEFLATED) as archive:
    archive.writestr("timich-runtime.json", json.dumps({
        "schemaVersion": 1,
        "product": "timich-semantic-runtime-pack",
        "runtime": "onnxruntime",
        "serverPath": "server.py",
    }) + "\n")
    archive.writestr("server.py", "print('runtime')\n")
runtime_sha = hashlib.sha256(runtime_zip.read_bytes()).hexdigest()
(root / "runtime.zip.sha256").write_text(f"{runtime_sha}  runtime.zip\n")
metadata = json.loads((root / "runtime.metadata.json").read_text())
metadata["runtimePack"]["artifact"]["sha256"] = runtime_sha
metadata["runtimePack"]["artifact"]["sizeBytes"] = runtime_zip.stat().st_size
(root / "runtime.metadata.json").write_text(json.dumps(metadata) + "\n")
registry = json.loads((root / "semantic-models.json").read_text())
artifact = registry["runtimePacks"][0]["artifacts"]["linux-amd64"]
artifact["sha256"] = runtime_sha
artifact["sizeBytes"] = runtime_zip.stat().st_size
(root / "semantic-models.json").write_text(json.dumps(registry) + "\n")
PY
if SEMANTIC_MODEL_REGISTRY="$missing_python_dir/semantic-models.json" \
  SEMANTIC_MODEL_PACK_DIR="$missing_python_dir" \
  SEMANTIC_RUNTIME_PACK_DIR="$missing_python_dir" \
  SEMANTIC_RELEASE_BASE_URL="https://github.com/rsahara/timich-agent/releases/download/v0.4.0-rc.2" \
  python3 "$script_dir/../semantic/validate_semantic_release.py" --validate-pack-layouts >/dev/null 2>&1; then
  echo "expected linux-amd64 runtime without bundled Python to fail" >&2
  exit 1
fi

printf 'obsolete\n' > "$state_dir/downloads/obsolete-model.zip"
obsolete_size=$(wc -c < "$state_dir/downloads/obsolete-model.zip" | tr -d '[:space:]')
obsolete_sha=$(sha256sum "$state_dir/downloads/obsolete-model.zip" | awk '{print $1}')
stale_semantic_assets_json=$(jq -cn \
  --argjson assets "$semantic_assets_json" \
  --argjson size "$obsolete_size" \
  --arg digest "sha256:$obsolete_sha" \
  '$assets + [{name:"obsolete-model.zip",size:$size,digest:$digest}]')
reset_staging draft "$public_sha" "$stale_semantic_assets_json"
if run_downloader >/dev/null 2>&1; then
  echo "expected unreferenced staging assets to fail validation" >&2
  exit 1
fi
if download_was_called obsolete-model.zip; then
  echo "unreferenced semantic asset was downloaded before namespace validation" >&2
  exit 1
fi

test ! -e "$state_dir/unexpected-operation"
printf '%s\n' 'semantic staging reader and validation tests passed'
