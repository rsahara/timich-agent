#!/usr/bin/env python3
from __future__ import annotations

import argparse
import hashlib
import json
import tarfile
import time
from pathlib import Path


RUNTIME_ROOT_IN_ARCHIVE = "media-runtime/ffmpeg"


def main() -> None:
    parser = argparse.ArgumentParser(description="Build Timich FFmpeg runtime pack sidecars")
    parser.add_argument("--runtime-dir", type=Path, required=True)
    parser.add_argument("--output-dir", type=Path, required=True)
    parser.add_argument("--id", required=True)
    parser.add_argument("--name", required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--platform", required=True)
    parser.add_argument("--base-url", default="")
    args = parser.parse_args()

    runtime_dir = args.runtime_dir.resolve()
    output_dir = args.output_dir.resolve()
    output_dir.mkdir(parents=True, exist_ok=True)

    validate_runtime_dir(runtime_dir)
    build_info = read_json(runtime_dir / "BUILDINFO.json")
    source_sha256 = read_source_sha(runtime_dir / "SOURCE.sha256")

    artifact_name = f"{args.id}_{args.version}_{args.platform}.tar.gz"
    artifact_path = output_dir / artifact_name
    sidecar_base = artifact_name.removesuffix(".tar.gz")
    write_runtime_archive(runtime_dir, artifact_path)
    artifact_sha256 = sha256_file(artifact_path)
    artifact_size = artifact_path.stat().st_size

    checksum_path = artifact_path.with_suffix(artifact_path.suffix + ".sha256")
    checksum_path.write_text(f"{artifact_sha256}  {artifact_name}\n", encoding="utf-8")

    sbom = build_sbom(args, artifact_name, artifact_sha256, artifact_size, build_info, source_sha256)
    sbom_path = output_dir / f"{sidecar_base}.spdx.json"
    write_json(sbom_path, sbom)

    metadata = build_metadata(
        args=args,
        artifact_name=artifact_name,
        artifact_sha256=artifact_sha256,
        artifact_size=artifact_size,
        build_info=build_info,
        source_sha256=source_sha256,
        sbom_path=sbom_path,
    )
    metadata_path = output_dir / f"{sidecar_base}.metadata.json"
    write_json(metadata_path, metadata)

    print(
        json.dumps(
            {
                "artifact": str(artifact_path),
                "sha256": str(checksum_path),
                "metadata": str(metadata_path),
                "sbom": str(sbom_path),
            },
            indent=2,
            sort_keys=True,
        )
    )


def validate_runtime_dir(runtime_dir: Path) -> None:
    required = [
        runtime_dir / "bin" / "ffmpeg",
        runtime_dir / "bin" / "ffprobe",
        runtime_dir / "BUILDINFO.json",
        runtime_dir / "CONFIGURE.txt",
        runtime_dir / "SOURCE.sha256",
        runtime_dir / "LICENSES" / "FFmpeg-LICENSE.md",
        runtime_dir / "LICENSES" / "FFmpeg-COPYING.LGPLv2.1",
        runtime_dir / "THIRD_PARTY_NOTICES" / "FFmpeg.txt",
    ]
    for path in required:
        if not path.exists():
            raise SystemExit(f"missing runtime file: {path}")


def write_runtime_archive(runtime_dir: Path, artifact_path: Path) -> None:
    with tarfile.open(artifact_path, "w:gz") as archive:
        for path in sorted(runtime_dir.rglob("*")):
            if path.is_dir():
                continue
            relative = path.relative_to(runtime_dir)
            arcname = f"{RUNTIME_ROOT_IN_ARCHIVE}/{relative.as_posix()}"
            info = archive.gettarinfo(str(path), arcname=arcname)
            info.uid = 0
            info.gid = 0
            info.uname = ""
            info.gname = ""
            if relative.as_posix() in {"bin/ffmpeg", "bin/ffprobe"}:
                info.mode = 0o755
            else:
                info.mode = 0o644
            with path.open("rb") as handle:
                archive.addfile(info, handle)


def build_metadata(
    *,
    args: argparse.Namespace,
    artifact_name: str,
    artifact_sha256: str,
    artifact_size: int,
    build_info: dict,
    source_sha256: str,
    sbom_path: Path,
) -> dict:
    base_url = args.base_url.rstrip("/")
    artifact_url = f"{base_url}/{artifact_name}" if base_url else ""
    sbom_sha256 = sha256_file(sbom_path)
    return {
        "schemaVersion": 1,
        "id": args.id,
        "name": args.name,
        "version": args.version,
        "platform": args.platform,
        "kind": "media-ffmpeg-runtime",
        "license": "LGPL-2.1-or-later",
        "layout": {
            "runtimeRoot": RUNTIME_ROOT_IN_ARCHIVE,
            "ffmpeg": f"{RUNTIME_ROOT_IN_ARCHIVE}/bin/ffmpeg",
            "ffprobe": f"{RUNTIME_ROOT_IN_ARCHIVE}/bin/ffprobe",
        },
        "source": {
            "project": "FFmpeg",
            "version": str(build_info.get("ffmpegVersion") or args.version),
            "url": str(build_info.get("sourceUrl") or ""),
            "sha256": source_sha256,
            "verifiedBy": "FFmpeg release GPG signature",
        },
        "build": {
            "profile": "lgpl-decode-only",
            "platform": str(build_info.get("platform") or args.platform),
            "builtAt": str(build_info.get("builtAt") or ""),
            "configureFile": f"{RUNTIME_ROOT_IN_ARCHIVE}/CONFIGURE.txt",
        },
        "artifact": {
            "filename": artifact_name,
            "url": artifact_url,
            "sha256": artifact_sha256,
            "sizeBytes": artifact_size,
        },
        "sbom": {
            "filename": sbom_path.name,
            "sha256": sbom_sha256,
            "sizeBytes": sbom_path.stat().st_size,
        },
    }


def build_sbom(
    args: argparse.Namespace,
    artifact_name: str,
    artifact_sha256: str,
    artifact_size: int,
    build_info: dict,
    source_sha256: str,
) -> dict:
    created = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
    document_id = f"{args.id}-{args.version}-{args.platform}"
    return {
        "spdxVersion": "SPDX-2.3",
        "dataLicense": "CC0-1.0",
        "SPDXID": "SPDXRef-DOCUMENT",
        "name": f"{args.name} {args.version} {args.platform}",
        "documentNamespace": f"https://timich.app/spdx/{document_id}",
        "creationInfo": {
            "created": created,
            "creators": ["Tool: timich-agent make_ffmpeg_runtime_pack.py"],
        },
        "packages": [
            {
                "name": artifact_name,
                "SPDXID": "SPDXRef-Package-FFmpegRuntimePack",
                "downloadLocation": "NOASSERTION",
                "filesAnalyzed": False,
                "licenseConcluded": "LGPL-2.1-or-later",
                "licenseDeclared": "LGPL-2.1-or-later",
                "copyrightText": "NOASSERTION",
                "checksums": [
                    {"algorithm": "SHA256", "checksumValue": artifact_sha256},
                ],
                "externalRefs": [
                    {
                        "referenceCategory": "PACKAGE-MANAGER",
                        "referenceType": "purl",
                        "referenceLocator": f"pkg:generic/{args.id}@{args.version}?arch={args.platform}",
                    },
                ],
                "packageVerificationCode": {"packageVerificationCodeValue": "NOASSERTION"},
                "summary": f"Timich FFmpeg runtime pack, {artifact_size} bytes",
            },
            {
                "name": "FFmpeg",
                "SPDXID": "SPDXRef-Package-FFmpegSource",
                "versionInfo": str(build_info.get("ffmpegVersion") or args.version),
                "downloadLocation": str(build_info.get("sourceUrl") or "NOASSERTION"),
                "filesAnalyzed": False,
                "licenseConcluded": "LGPL-2.1-or-later",
                "licenseDeclared": "LGPL-2.1-or-later",
                "copyrightText": "NOASSERTION",
                "checksums": [
                    {"algorithm": "SHA256", "checksumValue": source_sha256},
                ],
            },
        ],
        "relationships": [
            {
                "spdxElementId": "SPDXRef-DOCUMENT",
                "relationshipType": "DESCRIBES",
                "relatedSpdxElement": "SPDXRef-Package-FFmpegRuntimePack",
            },
            {
                "spdxElementId": "SPDXRef-Package-FFmpegRuntimePack",
                "relationshipType": "CONTAINS",
                "relatedSpdxElement": "SPDXRef-Package-FFmpegSource",
            },
        ],
    }


def read_json(path: Path) -> dict:
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as error:
        raise SystemExit(f"{path}: invalid JSON: {error}") from error
    if not isinstance(payload, dict):
        raise SystemExit(f"{path}: expected JSON object")
    return payload


def read_source_sha(path: Path) -> str:
    line = path.read_text(encoding="utf-8").splitlines()[0].strip()
    sha = line.split()[0].lower()
    if len(sha) != 64 or any(c not in "0123456789abcdef" for c in sha):
        raise SystemExit(f"{path}: invalid SHA-256 line")
    return sha


def write_json(path: Path, payload: dict) -> None:
    path.write_text(json.dumps(payload, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def sha256_file(path: Path) -> str:
    hasher = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            hasher.update(chunk)
    return hasher.hexdigest()


if __name__ == "__main__":
    main()
