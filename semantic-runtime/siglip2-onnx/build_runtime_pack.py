#!/usr/bin/env python3
"""Build a platform-specific Timich semantic runtime pack."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
import platform as py_platform
import shutil
import stat
import subprocess
import sys
import uuid
import zipfile
from email.parser import Parser
from pathlib import Path


RUNTIME = "onnxruntime"
PRODUCT = "timich-semantic-runtime-pack"
ARTIFACT_PRODUCT = "timich-semantic-runtime-pack-artifact"
REGISTRY_PRODUCT = "timich-semantic-models"
DEFAULT_PACK_ID = "timich-siglip2-onnxruntime-runtime"
DEFAULT_PACK_NAME = "Timich SigLIP 2 ONNX Runtime"
DEFAULT_LICENSE = "Apache-2.0"
DEFAULT_ZIP_MTIME = (2026, 1, 1, 0, 0, 0)


def main() -> int:
    args = parse_args()
    source_dir = Path(__file__).resolve().parent
    output_dir = args.output_dir.resolve()
    work_dir = args.work_dir.resolve() if args.work_dir else output_dir / "_work" / artifact_stem(args)
    stage_dir = work_dir / "stage"

    if not (source_dir / "server.py").is_file():
        raise SystemExit("server.py is missing next to build_runtime_pack.py")
    if not args.requirements.is_file():
        raise SystemExit(f"requirements file is missing: {args.requirements}")

    if work_dir.exists():
        shutil.rmtree(work_dir)
    stage_dir.mkdir(parents=True, exist_ok=True)
    output_dir.mkdir(parents=True, exist_ok=True)

    runtime_dir = stage_dir / "semantic-runtime" / "siglip2-onnx"
    runtime_dir.mkdir(parents=True, exist_ok=True)
    shutil.copy2(source_dir / "server.py", runtime_dir / "server.py")
    shutil.copy2(args.requirements, runtime_dir / "requirements.txt")
    if (source_dir / "README.md").is_file():
        shutil.copy2(source_dir / "README.md", runtime_dir / "README.md")
    make_executable(runtime_dir / "server.py")

    venv_dir = stage_dir / ".venv"
    if args.python_runtime_root:
        copy_python_runtime(args.python_runtime_root, venv_dir)
    else:
        create_venv(args.python, venv_dir)
    python_bin = venv_python(venv_dir, args.platform)
    install_requirements(python_bin, args.requirements, args.allow_source_builds)
    python_path = to_zip_path(python_bin.relative_to(stage_dir))
    write_requirements_lock(python_bin, stage_dir / "requirements.lock")
    runtime_info = python_runtime_info(str(python_bin))
    copy_python_stdlib(runtime_info, venv_dir)
    copy_python_shared_libraries(runtime_info, venv_dir, args.platform)
    copy_linux_shared_object_dependencies(venv_dir, args.platform)
    materialize_symlinks(venv_dir)
    copy_macos_python_framework_runtime_bits(runtime_info, venv_dir, args.platform)

    layout = {
        "schemaVersion": 1,
        "product": PRODUCT,
        "runtime": RUNTIME,
        "serverPath": "semantic-runtime/siglip2-onnx/server.py",
    }
    layout["pythonPath"] = python_path
    write_json(stage_dir / "timich-runtime.json", layout)

    build_info = {
        "schemaVersion": 1,
        "product": ARTIFACT_PRODUCT,
        "id": args.pack_id,
        "name": args.pack_name,
        "version": args.version,
        "runtime": RUNTIME,
        "platform": args.platform,
        "license": args.license,
        "createdAt": now_iso(),
        "pythonPath": python_path,
        "source": "semantic-runtime/siglip2-onnx",
    }
    write_json(stage_dir / "BUILDINFO.json", build_info)
    write_json(stage_dir / "SBOM.spdx.json", build_spdx(args, stage_dir))

    ensure_no_symlinks(stage_dir)

    artifact_path = output_dir / f"{artifact_stem(args)}.zip"
    if artifact_path.exists():
        artifact_path.unlink()
    write_zip(stage_dir, artifact_path)

    artifact_sha = sha256_file(artifact_path)
    sha_path = artifact_path.with_suffix(artifact_path.suffix + ".sha256")
    sha_path.write_text(f"{artifact_sha}  {artifact_path.name}\n", encoding="utf-8")

    sbom_path = output_dir / f"{artifact_stem(args)}.spdx.json"
    shutil.copy2(stage_dir / "SBOM.spdx.json", sbom_path)

    signature = None
    if args.signing_key:
        signature_path = artifact_path.with_suffix(artifact_path.suffix + ".sig")
        sign_artifact(args.signing_key, artifact_path, signature_path)
        signature = {
            "algorithm": "openssl-dgst-sha256-rsa",
            "filename": signature_path.name,
            "sizeBytes": signature_path.stat().st_size,
            "sha256": sha256_file(signature_path),
        }

    metadata = build_artifact_metadata(args, artifact_path, artifact_sha, sbom_path, signature)
    metadata_path = output_dir / f"{artifact_stem(args)}.metadata.json"
    write_json(metadata_path, metadata)

    if args.artifact_base_url:
        registry_path = output_dir / f"{artifact_stem(args)}.registry.json"
        write_json(registry_path, build_registry_fragment(args, artifact_path, artifact_sha))

    if not args.keep_work:
        shutil.rmtree(work_dir)
        try:
            work_dir.parent.rmdir()
        except OSError:
            pass

    print(json.dumps({
        "artifact": str(artifact_path),
        "sha256": artifact_sha,
        "sbom": str(sbom_path),
        "metadata": str(metadata_path),
        "signed": signature is not None,
    }, indent=2, sort_keys=True))
    return 0


def parse_args() -> argparse.Namespace:
    source_dir = Path(__file__).resolve().parent
    parser = argparse.ArgumentParser(description="Build a Timich semantic runtime pack zip.")
    parser.add_argument("--output-dir", type=Path, default=Path(env("TIMICH_RUNTIME_PACK_OUTPUT_DIR", "dist/semantic-runtime-packs")))
    parser.add_argument("--work-dir", type=Path, default=path_env("TIMICH_RUNTIME_PACK_WORK_DIR"))
    parser.add_argument("--pack-id", default=env("TIMICH_RUNTIME_PACK_ID", DEFAULT_PACK_ID))
    parser.add_argument("--pack-name", default=env("TIMICH_RUNTIME_PACK_NAME", DEFAULT_PACK_NAME))
    parser.add_argument("--version", default=env("TIMICH_RUNTIME_PACK_VERSION", "0.3.0"))
    parser.add_argument("--platform", default=env("TIMICH_RUNTIME_PACK_PLATFORM", detect_platform()))
    parser.add_argument("--license", default=env("TIMICH_RUNTIME_PACK_LICENSE", DEFAULT_LICENSE))
    parser.add_argument("--requirements", type=Path, default=Path(env("TIMICH_RUNTIME_PACK_REQUIREMENTS", str(source_dir / "requirements.txt"))))
    parser.add_argument("--python", default=env("TIMICH_RUNTIME_PACK_PYTHON", sys.executable))
    parser.add_argument("--python-runtime-root", type=Path, default=path_env("TIMICH_RUNTIME_PACK_PYTHON_RUNTIME_ROOT"))
    parser.add_argument("--artifact-base-url", default=env("TIMICH_RUNTIME_PACK_ARTIFACT_BASE_URL", ""))
    parser.add_argument("--signing-key", type=Path, default=path_env("TIMICH_RUNTIME_PACK_SIGNING_KEY"))
    parser.add_argument("--allow-source-builds", action="store_true", default=bool_env("TIMICH_RUNTIME_PACK_ALLOW_SOURCE_BUILDS"))
    parser.add_argument("--keep-work", action="store_true", default=bool_env("TIMICH_RUNTIME_PACK_KEEP_WORK"))
    return parser.parse_args()


def env(name: str, default: str) -> str:
    value = os.environ.get(name, "").strip()
    return value if value else default


def path_env(name: str) -> Path | None:
    value = os.environ.get(name, "").strip()
    return Path(value) if value else None


def bool_env(name: str) -> bool:
    return os.environ.get(name, "").strip().lower() in {"1", "true", "yes", "on"}


def detect_platform() -> str:
    goos = {
        "darwin": "darwin",
        "linux": "linux",
        "win32": "windows",
        "cygwin": "windows",
        "msys": "windows",
    }.get(sys.platform, sys.platform)
    machine = py_platform.machine().lower()
    goarch = {
        "x86_64": "amd64",
        "amd64": "amd64",
        "aarch64": "arm64",
        "arm64": "arm64",
    }.get(machine, machine)
    return f"{goos}-{goarch}"


def artifact_stem(args: argparse.Namespace) -> str:
    return f"{args.pack_id}_{args.version}_{args.platform}"


def now_iso() -> str:
    return dt.datetime.now(dt.timezone.utc).replace(microsecond=0).isoformat().replace("+00:00", "Z")


def create_venv(python: str, venv_dir: Path) -> bool:
    try:
        subprocess.run([python, "-m", "venv", "--copies", str(venv_dir)], check=True)
        return False
    except subprocess.CalledProcessError:
        if venv_dir.exists():
            shutil.rmtree(venv_dir)
    subprocess.run([python, "-m", "venv", str(venv_dir)], check=True)
    return True


def copy_python_runtime(source_root: Path, venv_dir: Path) -> None:
    if not source_root.is_dir():
        raise SystemExit(f"python runtime root is missing: {source_root}")
    shutil.copytree(source_root, venv_dir, symlinks=False)


def materialize_symlinks(root: Path) -> None:
    for path in sorted(root.rglob("*")):
        if not path.is_symlink():
            continue
        target = path.resolve(strict=True)
        copy_macos_python_framework_runtime(root, target)
        temp_path = path.with_name(path.name + ".copy-tmp")
        if temp_path.exists():
            if temp_path.is_dir():
                shutil.rmtree(temp_path)
            else:
                temp_path.unlink()
        if target.is_dir():
            shutil.copytree(target, temp_path, symlinks=False)
            path.unlink()
            temp_path.rename(path)
        else:
            shutil.copy2(target, temp_path)
            mode = stat.S_IMODE(target.stat().st_mode)
            path.unlink()
            temp_path.rename(path)
            path.chmod(mode)


def copy_macos_python_framework_runtime(root: Path, target: Path) -> None:
    library = target.parent.parent / "Python3"
    if not library.is_file():
        return
    destination = root / "Python3"
    if destination.exists():
        return
    shutil.copy2(library, destination)
    destination.chmod(stat.S_IMODE(library.stat().st_mode))


def copy_macos_python_framework_runtime_bits(runtime_info: dict[str, str], venv_dir: Path, target_platform: str) -> None:
    if not target_platform.startswith("darwin-"):
        return
    base_value = runtime_info.get("basePrefix", "")
    base = Path(base_value) if base_value else None
    if base is None:
        return
    library = base / "Python3"
    if library.is_file():
        destination = venv_dir / "Python3"
        if not destination.exists():
            shutil.copy2(library, destination)
            destination.chmod(stat.S_IMODE(library.stat().st_mode))
    app_destination = venv_dir / "Resources" / "Python.app"
    if not app_destination.exists():
        for app_source in (
            base / "Resources" / "Python.app",
            base.parent.parent / "Resources" / "Python.app",
        ):
            if not app_source.is_dir():
                continue
            app_destination.parent.mkdir(parents=True, exist_ok=True)
            shutil.copytree(app_source, app_destination, symlinks=False)
            break


def copy_python_stdlib(runtime_info: dict[str, str], venv_dir: Path) -> None:
    source_value = runtime_info.get("stdlib", "")
    version = runtime_info.get("version", "")
    if not source_value or not version:
        return
    source = Path(source_value)
    destination = venv_dir / "lib" / f"python{version}"
    if not source.is_dir() or not destination.is_dir():
        return
    for child in source.iterdir():
        if child.name in {"site-packages", "__pycache__"}:
            continue
        target = destination / child.name
        if child.is_dir():
            shutil.copytree(child, target, symlinks=False, dirs_exist_ok=True)
        elif not target.exists():
            shutil.copy2(child, target)
            target.chmod(stat.S_IMODE(child.stat().st_mode))


def copy_python_shared_libraries(runtime_info: dict[str, str], venv_dir: Path, target_platform: str) -> None:
    if target_platform.startswith(("darwin-", "windows-")):
        return
    version = runtime_info.get("version", "")
    names = {
        runtime_info.get("ldLibrary", ""),
        runtime_info.get("instSoName", ""),
        f"libpython{version}.so" if version else "",
        f"libpython{version}.so.1.0" if version else "",
    }
    names = {name for name in names if ".so" in name}
    search_roots = []
    for value in (runtime_info.get("libDir", ""), runtime_info.get("basePrefix", "")):
        if value:
            search_roots.append(Path(value))
            search_roots.append(Path(value) / "lib")
    destination_dir = venv_dir / "lib"
    destination_dir.mkdir(parents=True, exist_ok=True)
    copied = set()
    for name in sorted(item for item in names if item):
        for root in search_roots:
            source = root / name
            if not source.is_file():
                continue
            destination = destination_dir / name
            if destination in copied:
                break
            shutil.copy2(source, destination)
            destination.chmod(stat.S_IMODE(source.stat().st_mode))
            copied.add(destination)
            break


def copy_linux_shared_object_dependencies(venv_dir: Path, target_platform: str) -> None:
    if not target_platform.startswith("linux-"):
        return
    destination_dir = venv_dir / "lib"
    destination_dir.mkdir(parents=True, exist_ok=True)
    for shared_object in sorted(venv_dir.rglob("*.so*")):
        if not shared_object.is_file():
            continue
        for dependency in linux_shared_object_dependencies(shared_object):
            if not should_bundle_linux_shared_library(dependency):
                continue
            destination = destination_dir / dependency.name
            if destination.exists():
                continue
            shutil.copy2(dependency, destination)
            destination.chmod(stat.S_IMODE(dependency.stat().st_mode))


def linux_shared_object_dependencies(shared_object: Path) -> list[Path]:
    try:
        result = subprocess.run(
            ["ldd", str(shared_object)],
            check=False,
            capture_output=True,
            text=True,
        )
    except OSError:
        return []
    dependencies: list[Path] = []
    for line in result.stdout.splitlines():
        dependency = parse_ldd_dependency(line)
        if dependency is not None and dependency.is_file():
            dependencies.append(dependency)
    return dependencies


def parse_ldd_dependency(line: str) -> Path | None:
    line = line.strip()
    if not line:
        return None
    if "=>" in line:
        _, _, right = line.partition("=>")
        candidate = right.strip().split(maxsplit=1)[0] if right.strip() else ""
    else:
        candidate = line.split(maxsplit=1)[0]
    if not candidate.startswith("/"):
        return None
    return Path(candidate)


def should_bundle_linux_shared_library(path: Path) -> bool:
    raw = str(path)
    return raw.startswith("/opt/_internal/") or raw.startswith("/opt/python/")


def python_runtime_info(python: str) -> dict[str, str]:
    try:
        result = subprocess.run(
            [
                python,
                "-c",
                "import json, sys, sysconfig; print(json.dumps({"
                "'basePrefix': sys.base_prefix or sysconfig.get_config_var('base') or '',"
                "'stdlib': sysconfig.get_path('stdlib') or '',"
                "'version': f'{sys.version_info.major}.{sys.version_info.minor}',"
                "'libDir': sysconfig.get_config_var('LIBDIR') or '',"
                "'ldLibrary': sysconfig.get_config_var('LDLIBRARY') or '',"
                "'instSoName': sysconfig.get_config_var('INSTSONAME') or '',"
                "}))",
            ],
            check=True,
            capture_output=True,
            text=True,
        )
    except (OSError, subprocess.CalledProcessError):
        return {}
    try:
        payload = json.loads(result.stdout)
    except json.JSONDecodeError:
        return {}
    return {str(key): str(value) for key, value in payload.items()}


def venv_python(venv_dir: Path, target_platform: str) -> Path:
    if target_platform.startswith("windows-"):
        candidate = venv_dir / "Scripts" / "python.exe"
    else:
        candidate = venv_dir / "bin" / "python"
    if not candidate.is_file():
        raise SystemExit(f"venv python is missing: {candidate}")
    make_executable(candidate)
    return candidate


def install_requirements(python_bin: Path, requirements: Path, allow_source_builds: bool) -> None:
    command = [
        str(python_bin),
        "-m",
        "pip",
        "install",
        "--disable-pip-version-check",
    ]
    if not allow_source_builds:
        command.append("--only-binary=:all:")
    command.extend(["-r", str(requirements)])
    subprocess.run(command, check=True)


def write_requirements_lock(python_bin: Path, path: Path) -> None:
    result = subprocess.run(
        [str(python_bin), "-m", "pip", "freeze", "--all"],
        check=True,
        capture_output=True,
        text=True,
    )
    path.write_text(result.stdout, encoding="utf-8")


def ensure_no_symlinks(root: Path) -> None:
    for path in root.rglob("*"):
        if path.is_symlink():
            raise SystemExit(f"runtime pack cannot contain symlinks: {path.relative_to(root)}")


def write_zip(stage_dir: Path, artifact_path: Path) -> None:
    with zipfile.ZipFile(artifact_path, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9) as archive:
        for path in sorted(stage_dir.rglob("*")):
            if path.is_dir():
                continue
            relative = to_zip_path(path.relative_to(stage_dir))
            info = zipfile.ZipInfo(relative, DEFAULT_ZIP_MTIME)
            mode = stat.S_IMODE(path.stat().st_mode)
            info.external_attr = (mode & 0o777) << 16
            info.compress_type = zipfile.ZIP_DEFLATED
            with path.open("rb") as source:
                archive.writestr(info, source.read())


def build_spdx(args: argparse.Namespace, stage_dir: Path) -> dict:
    created = now_iso()
    document_id = f"SPDXRef-DOCUMENT-{uuid.uuid4()}"
    packages = [
        {
            "name": args.pack_id,
            "SPDXID": "SPDXRef-Package-RuntimePack",
            "versionInfo": args.version,
            "downloadLocation": "NOASSERTION",
            "filesAnalyzed": False,
            "licenseConcluded": "NOASSERTION",
            "licenseDeclared": args.license or "NOASSERTION",
            "copyrightText": "NOASSERTION",
        }
    ]
    python_runtime = bundled_python_runtime_package(stage_dir)
    if python_runtime:
        packages.append(python_runtime)
    for package in installed_python_packages(stage_dir):
        packages.append(package)
    relationships = [
        {
            "spdxElementId": "SPDXRef-DOCUMENT",
            "relationshipType": "DESCRIBES",
            "relatedSpdxElement": "SPDXRef-Package-RuntimePack",
        }
    ]
    for package in packages[1:]:
        relationships.append({
            "spdxElementId": "SPDXRef-Package-RuntimePack",
            "relationshipType": "CONTAINS",
            "relatedSpdxElement": package["SPDXID"],
        })
    return {
        "spdxVersion": "SPDX-2.3",
        "dataLicense": "CC0-1.0",
        "SPDXID": "SPDXRef-DOCUMENT",
        "name": f"{args.pack_id}-{args.version}-{args.platform}",
        "documentNamespace": f"https://timich.app/spdx/{document_id}",
        "creationInfo": {
            "created": created,
            "creators": ["Tool: timich-semantic-runtime-pack-builder"],
        },
        "packages": packages,
        "relationships": relationships,
    }


def bundled_python_runtime_package(stage_dir: Path) -> dict | None:
    pyvenv_config = stage_dir / ".venv" / "pyvenv.cfg"
    if not pyvenv_config.is_file():
        return None
    version = "NOASSERTION"
    for line in pyvenv_config.read_text(encoding="utf-8", errors="replace").splitlines():
        key, _, value = line.partition("=")
        if key.strip().lower() == "version" and value.strip():
            version = value.strip()
            break
    return {
        "name": "Python Runtime",
        "SPDXID": "SPDXRef-Package-PythonRuntime",
        "versionInfo": version,
        "downloadLocation": "NOASSERTION",
        "filesAnalyzed": False,
        "licenseConcluded": "NOASSERTION",
        "licenseDeclared": "NOASSERTION",
        "copyrightText": "NOASSERTION",
    }


def installed_python_packages(stage_dir: Path) -> list[dict]:
    packages = []
    site_packages_dirs = [path for path in (stage_dir / ".venv").rglob("site-packages") if path.is_dir()]
    for site_packages in site_packages_dirs:
        for metadata_path in sorted(site_packages.glob("*.dist-info/METADATA")):
            metadata = Parser().parsestr(metadata_path.read_text(encoding="utf-8", errors="replace"))
            name = metadata.get("Name", "").strip()
            version = metadata.get("Version", "").strip()
            if not name:
                continue
            license_declared = metadata.get("License", "").strip() or "NOASSERTION"
            packages.append({
                "name": name,
                "SPDXID": f"SPDXRef-PythonPackage-{spdx_id(name)}",
                "versionInfo": version or "NOASSERTION",
                "downloadLocation": "NOASSERTION",
                "filesAnalyzed": False,
                "licenseConcluded": "NOASSERTION",
                "licenseDeclared": license_declared,
                "copyrightText": "NOASSERTION",
                "externalRefs": purl_refs(name, version),
            })
    return packages


def purl_refs(name: str, version: str) -> list[dict]:
    if not version:
        return []
    normalized = name.lower().replace("_", "-")
    return [{
        "referenceCategory": "PACKAGE-MANAGER",
        "referenceType": "purl",
        "referenceLocator": f"pkg:pypi/{normalized}@{version}",
    }]


def spdx_id(value: str) -> str:
    return "".join(character if character.isalnum() else "-" for character in value)


def build_artifact_metadata(
    args: argparse.Namespace,
    artifact_path: Path,
    artifact_sha: str,
    sbom_path: Path,
    signature: dict | None,
) -> dict:
    metadata = {
        "schemaVersion": 1,
        "product": ARTIFACT_PRODUCT,
        "runtimePack": {
            "id": args.pack_id,
            "name": args.pack_name,
            "version": args.version,
            "runtime": RUNTIME,
            "platform": args.platform,
            "license": args.license,
            "artifact": {
                "filename": artifact_path.name,
                "sha256": artifact_sha,
                "sizeBytes": artifact_path.stat().st_size,
            },
            "sbom": {
                "format": "spdx-json",
                "filename": sbom_path.name,
                "sha256": sha256_file(sbom_path),
                "sizeBytes": sbom_path.stat().st_size,
            },
        },
    }
    if signature:
        metadata["runtimePack"]["signature"] = signature
    return metadata


def build_registry_fragment(args: argparse.Namespace, artifact_path: Path, artifact_sha: str) -> dict:
    base_url = args.artifact_base_url.rstrip("/")
    return {
        "schemaVersion": 1,
        "product": REGISTRY_PRODUCT,
        "recommendedRuntimePack": args.pack_id,
        "runtimePacks": [{
            "id": args.pack_id,
            "name": args.pack_name,
            "version": args.version,
            "runtime": RUNTIME,
            "license": args.license,
            "artifacts": {
                args.platform: {
                    "filename": artifact_path.name,
                    "url": f"{base_url}/{artifact_path.name}",
                    "sha256": artifact_sha,
                    "sizeBytes": artifact_path.stat().st_size,
                },
            },
        }],
    }


def sign_artifact(signing_key: Path, artifact_path: Path, signature_path: Path) -> None:
    if not signing_key.is_file():
        raise SystemExit(f"signing key is missing: {signing_key}")
    subprocess.run(
        [
            "openssl",
            "dgst",
            "-sha256",
            "-sign",
            str(signing_key),
            "-out",
            str(signature_path),
            str(artifact_path),
        ],
        check=True,
    )


def sha256_file(path: Path) -> str:
    hasher = hashlib.sha256()
    with path.open("rb") as file:
        for chunk in iter(lambda: file.read(1024 * 1024), b""):
            hasher.update(chunk)
    return hasher.hexdigest()


def write_json(path: Path, payload: dict) -> None:
    path.write_text(json.dumps(payload, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def make_executable(path: Path) -> None:
    if path.is_symlink():
        return
    mode = path.stat().st_mode
    path.chmod(mode | stat.S_IXUSR)


def to_zip_path(path: Path) -> str:
    return str(path).replace(os.sep, "/")


if __name__ == "__main__":
    raise SystemExit(main())
