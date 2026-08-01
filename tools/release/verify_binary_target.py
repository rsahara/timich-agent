#!/usr/bin/env python3
"""Verify that a release binary matches its declared operating system and architecture."""

from __future__ import annotations

import argparse
import pathlib
import struct
import sys


ELF_MACHINES = {
    62: "amd64",
    183: "arm64",
}

MACHO_CPUS = {
    0x01000007: "amd64",
    0x0100000C: "arm64",
}

PE_MACHINES = {
    0x8664: "amd64",
    0xAA64: "arm64",
}


def detect_binary_target(path: pathlib.Path) -> tuple[str, str]:
    data = path.read_bytes()[:4096]
    if data.startswith(b"\x7fELF") and len(data) >= 20:
        byte_order = {1: "little", 2: "big"}.get(data[5])
        if byte_order is None:
            raise ValueError("ELF byte order is unsupported")
        machine = int.from_bytes(data[18:20], byte_order)
        arch = ELF_MACHINES.get(machine)
        if arch is None:
            raise ValueError(f"ELF machine {machine} is unsupported")
        return "linux", arch

    if len(data) >= 8 and data[:4] in {b"\xcf\xfa\xed\xfe", b"\xfe\xed\xfa\xcf"}:
        byte_order = "little" if data[:4] == b"\xcf\xfa\xed\xfe" else "big"
        cpu = int.from_bytes(data[4:8], byte_order)
        arch = MACHO_CPUS.get(cpu)
        if arch is None:
            raise ValueError(f"Mach-O CPU {cpu:#x} is unsupported")
        return "darwin", arch

    if data.startswith(b"MZ") and len(data) >= 0x40:
        pe_offset = struct.unpack_from("<I", data, 0x3C)[0]
        if pe_offset + 6 > len(data) or data[pe_offset : pe_offset + 4] != b"PE\0\0":
            raise ValueError("PE header is missing")
        machine = struct.unpack_from("<H", data, pe_offset + 4)[0]
        arch = PE_MACHINES.get(machine)
        if arch is None:
            raise ValueError(f"PE machine {machine:#x} is unsupported")
        return "windows", arch

    raise ValueError("binary format is unsupported")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--path", required=True, type=pathlib.Path)
    parser.add_argument("--os", required=True, dest="expected_os")
    parser.add_argument("--arch", required=True, dest="expected_arch")
    args = parser.parse_args()

    try:
        actual_os, actual_arch = detect_binary_target(args.path)
    except (OSError, ValueError) as error:
        print(f"could not verify {args.path}: {error}", file=sys.stderr)
        return 1

    expected = (args.expected_os.strip(), args.expected_arch.strip())
    actual = (actual_os, actual_arch)
    if actual != expected:
        print(
            f"binary target mismatch for {args.path}: "
            f"built {actual_os}/{actual_arch}, expected {expected[0]}/{expected[1]}",
            file=sys.stderr,
        )
        return 1

    print(f"Verified {args.path}: {actual_os}/{actual_arch}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
