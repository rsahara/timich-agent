#!/usr/bin/env python3
"""Bound semantic release ZIP metadata and peak disk usage before extraction."""

from __future__ import annotations

from dataclasses import dataclass
import os
from pathlib import Path
import struct
import zipfile


MAX_ARCHIVE_ENTRIES = 100_000
MAX_ARCHIVE_CENTRAL_DIRECTORY_BYTES = 64 * 1024 * 1024
MAX_ARCHIVE_UNCOMPRESSED_BYTES = 8 * 1024 * 1024 * 1024
MAX_RELEASE_WORKING_SET_BYTES = 10 * 1024 * 1024 * 1024
MAX_ZIP_COMMENT_BYTES = 65_535

EOCD_SIGNATURE = b"PK\x05\x06"
ZIP64_EOCD_SIGNATURE = b"PK\x06\x06"
ZIP64_LOCATOR_SIGNATURE = b"PK\x06\x07"
CENTRAL_DIRECTORY_SIGNATURE = b"PK\x01\x02"
EOCD = struct.Struct("<4s4H2LH")
ZIP64_EOCD = struct.Struct("<4sQ2H2L4Q")
ZIP64_LOCATOR = struct.Struct("<4sLQL")
CENTRAL_DIRECTORY = struct.Struct("<4s6H3L5H2L")


class SemanticArchiveBudgetError(ValueError):
    """Raised when accepted semantic assets would exceed a release resource bound."""


@dataclass(frozen=True)
class SemanticArchiveStats:
    entry_count: int
    central_directory_size_bytes: int
    uncompressed_size_bytes: int


@dataclass(frozen=True)
class SemanticArchiveDirectoryStats:
    entry_count: int
    central_directory_size_bytes: int


@dataclass(frozen=True)
class SemanticArchiveDirectoryLayout:
    entry_count: int
    central_directory_size_bytes: int
    central_directory_offset: int
    footer_offset: int
    disk_number: int
    central_directory_disk: int
    entries_on_disk: int


def preflight_semantic_archive_directory(
    path: Path,
    *,
    max_entries: int = MAX_ARCHIVE_ENTRIES,
    max_central_directory_bytes: int = MAX_ARCHIVE_CENTRAL_DIRECTORY_BYTES,
) -> SemanticArchiveDirectoryStats:
    """Validate bounded ZIP footer and directory records before ZipFile allocates."""
    try:
        with path.open("rb") as handle:
            handle.seek(0, os.SEEK_END)
            archive_size = handle.tell()
            tail_size = min(
                archive_size,
                EOCD.size + MAX_ZIP_COMMENT_BYTES + ZIP64_LOCATOR.size,
            )
            handle.seek(archive_size - tail_size)
            tail = handle.read(tail_size)
            if len(tail) != tail_size:
                raise SemanticArchiveBudgetError(f"could not read ZIP footer from {path}")
            eocd_offset, eocd = _find_eocd(path, tail, archive_size - tail_size)
            (
                _,
                disk_number,
                central_directory_disk,
                entries_on_disk,
                entry_count,
                central_directory_size,
                central_directory_offset,
                _,
            ) = eocd
            layout = SemanticArchiveDirectoryLayout(
                entry_count=int(entry_count),
                central_directory_size_bytes=int(central_directory_size),
                central_directory_offset=int(central_directory_offset),
                footer_offset=eocd_offset,
                disk_number=int(disk_number),
                central_directory_disk=int(central_directory_disk),
                entries_on_disk=int(entries_on_disk),
            )
            zip64_layout = _fixed_zip64_directory_layout(handle, path, eocd_offset)
            if zip64_layout is not None:
                layout = zip64_layout

            _validate_semantic_archive_directory_layout(
                path,
                layout,
                max_entries=max_entries,
                max_central_directory_bytes=max_central_directory_bytes,
            )
            actual_entry_count = _scan_semantic_central_directory(
                handle,
                path,
                layout,
                max_entries=max_entries,
            )
    except SemanticArchiveBudgetError:
        raise
    except OSError as error:
        raise SemanticArchiveBudgetError(f"could not inspect semantic ZIP {path}: {error}") from error

    return SemanticArchiveDirectoryStats(
        entry_count=actual_entry_count,
        central_directory_size_bytes=layout.central_directory_size_bytes,
    )


def _fixed_zip64_directory_layout(handle, path: Path, eocd_offset: int):
    locator_offset = eocd_offset - ZIP64_LOCATOR.size
    locator = _read_exact_at(handle, locator_offset, ZIP64_LOCATOR.size)
    if len(locator) != ZIP64_LOCATOR.size or not locator.startswith(ZIP64_LOCATOR_SIGNATURE):
        return None

    zip64_record_offset = locator_offset - ZIP64_EOCD.size
    zip64_raw = _read_exact_at(handle, zip64_record_offset, ZIP64_EOCD.size)
    if len(zip64_raw) != ZIP64_EOCD.size or not zip64_raw.startswith(ZIP64_EOCD_SIGNATURE):
        # These bytes can be the tail of a valid classic central-directory
        # entry comment. They are a ZIP64 locator only when the complete fixed
        # record is contiguous immediately before them.
        return None

    _, zip64_disk, declared_record_offset, total_disks = ZIP64_LOCATOR.unpack(locator)
    (
        _,
        zip64_record_size,
        _,
        _,
        disk_number,
        central_directory_disk,
        entries_on_disk,
        entry_count,
        central_directory_size,
        central_directory_offset,
    ) = ZIP64_EOCD.unpack(zip64_raw)
    if zip64_record_size != ZIP64_EOCD.size - 12:
        raise SemanticArchiveBudgetError(
            f"semantic ZIP {path.name} uses unsupported ZIP64 extensible data"
        )
    if declared_record_offset != zip64_record_offset:
        raise SemanticArchiveBudgetError(
            f"semantic ZIP {path.name} ZIP64 locator does not identify its fixed end record"
        )
    if zip64_disk != 0 or total_disks != 1:
        raise SemanticArchiveBudgetError("multi-disk semantic ZIPs are not supported")

    return SemanticArchiveDirectoryLayout(
        entry_count=int(entry_count),
        central_directory_size_bytes=int(central_directory_size),
        central_directory_offset=int(central_directory_offset),
        footer_offset=zip64_record_offset,
        disk_number=int(disk_number),
        central_directory_disk=int(central_directory_disk),
        entries_on_disk=int(entries_on_disk),
    )


def _validate_semantic_archive_directory_layout(
    path: Path,
    layout: SemanticArchiveDirectoryLayout,
    *,
    max_entries: int,
    max_central_directory_bytes: int,
) -> None:
    if max_entries < 0 or max_central_directory_bytes < 0:
        raise SemanticArchiveBudgetError("semantic ZIP limits must not be negative")
    if (
        layout.disk_number != 0
        or layout.central_directory_disk != 0
        or layout.entries_on_disk != layout.entry_count
    ):
        raise SemanticArchiveBudgetError("multi-disk semantic ZIPs are not supported")
    if layout.entry_count > max_entries:
        raise SemanticArchiveBudgetError(
            f"semantic ZIP {path.name} has {layout.entry_count} entries; maximum is {max_entries}"
        )
    if layout.central_directory_size_bytes > max_central_directory_bytes:
        raise SemanticArchiveBudgetError(
            f"semantic ZIP {path.name} central directory is {layout.central_directory_size_bytes} bytes; "
            f"maximum is {max_central_directory_bytes}"
        )
    if layout.entry_count * CENTRAL_DIRECTORY.size > layout.central_directory_size_bytes:
        raise SemanticArchiveBudgetError(f"semantic ZIP {path.name} has inconsistent directory metadata")
    if (
        layout.central_directory_offset + layout.central_directory_size_bytes
        != layout.footer_offset
    ):
        raise SemanticArchiveBudgetError(
            f"semantic ZIP {path.name} central directory does not end at its footer"
        )


def _scan_semantic_central_directory(
    handle,
    path: Path,
    layout: SemanticArchiveDirectoryLayout,
    *,
    max_entries: int,
) -> int:
    position = layout.central_directory_offset
    directory_end = position + layout.central_directory_size_bytes
    actual_entry_count = 0
    while position < directory_end:
        if actual_entry_count >= max_entries:
            raise SemanticArchiveBudgetError(
                f"semantic ZIP {path.name} contains more than {max_entries} entries"
            )
        header = _read_exact_at(handle, position, CENTRAL_DIRECTORY.size)
        if len(header) != CENTRAL_DIRECTORY.size:
            raise SemanticArchiveBudgetError(
                f"semantic ZIP {path.name} has a truncated central-directory record"
            )
        fields = CENTRAL_DIRECTORY.unpack(header)
        if fields[0] != CENTRAL_DIRECTORY_SIGNATURE:
            raise SemanticArchiveBudgetError(
                f"semantic ZIP {path.name} has an invalid central-directory record"
            )
        variable_size = int(fields[10]) + int(fields[11]) + int(fields[12])
        next_position = position + CENTRAL_DIRECTORY.size + variable_size
        if next_position > directory_end:
            raise SemanticArchiveBudgetError(
                f"semantic ZIP {path.name} central-directory record exceeds its declared boundary"
            )
        position = next_position
        actual_entry_count += 1

    if actual_entry_count != layout.entry_count:
        raise SemanticArchiveBudgetError(
            f"semantic ZIP {path.name} declares {layout.entry_count} entries "
            f"but contains {actual_entry_count} central-directory records"
        )
    return actual_entry_count


def _find_eocd(path: Path, tail: bytes, tail_offset: int) -> tuple[int, tuple]:
    relative_offset = tail.rfind(EOCD_SIGNATURE)
    if relative_offset < 0 or relative_offset + EOCD.size > len(tail):
        raise SemanticArchiveBudgetError(f"ZIP end record is missing or invalid in {path}")
    eocd = EOCD.unpack(tail[relative_offset:relative_offset + EOCD.size])
    comment_size = eocd[-1]
    if relative_offset + EOCD.size + comment_size != len(tail):
        raise SemanticArchiveBudgetError(
            f"last ZIP end record is not an exact footer in {path}"
        )
    return tail_offset + relative_offset, eocd


def _read_exact_at(handle, offset: int, size: int) -> bytes:
    if offset < 0:
        return b""
    handle.seek(offset)
    return handle.read(size)


def inspect_semantic_archive(
    path: Path,
    *,
    max_entries: int = MAX_ARCHIVE_ENTRIES,
    max_central_directory_bytes: int = MAX_ARCHIVE_CENTRAL_DIRECTORY_BYTES,
    max_uncompressed_bytes: int = MAX_ARCHIVE_UNCOMPRESSED_BYTES,
) -> SemanticArchiveStats:
    """Read one ZIP central directory without extracting archive bytes."""
    directory = preflight_semantic_archive_directory(
        path,
        max_entries=max_entries,
        max_central_directory_bytes=max_central_directory_bytes,
    )
    try:
        with zipfile.ZipFile(path) as archive:
            entries = archive.infolist()
    except (OSError, zipfile.BadZipFile) as error:
        raise SemanticArchiveBudgetError(f"invalid semantic ZIP {path}: {error}") from error
    if len(entries) != directory.entry_count:
        raise SemanticArchiveBudgetError(f"semantic ZIP {path.name} entry count changed during validation")
    uncompressed_size = sum(int(entry.file_size) for entry in entries if not entry.is_dir())
    if uncompressed_size > max_uncompressed_bytes:
        raise SemanticArchiveBudgetError(
            f"semantic ZIP {path.name} expands to {uncompressed_size} bytes; "
            f"maximum is {max_uncompressed_bytes}"
        )
    return SemanticArchiveStats(
        entry_count=len(entries),
        central_directory_size_bytes=directory.central_directory_size_bytes,
        uncompressed_size_bytes=uncompressed_size,
    )


def enforce_semantic_working_set(
    downloaded_size_bytes: int,
    validation_extract_bytes: int,
    smoke_extract_bytes: int,
    *,
    max_working_set_bytes: int = MAX_RELEASE_WORKING_SET_BYTES,
) -> None:
    """Reject inputs whose retained downloads plus extraction exceed one budget."""
    for label, value in (
        ("downloaded semantic assets", downloaded_size_bytes),
        ("validation extraction", validation_extract_bytes),
        ("smoke extraction", smoke_extract_bytes),
    ):
        if value < 0:
            raise SemanticArchiveBudgetError(f"{label} size must not be negative")
    validation_peak = downloaded_size_bytes + validation_extract_bytes
    smoke_peak = downloaded_size_bytes + smoke_extract_bytes
    if validation_peak > max_working_set_bytes:
        raise SemanticArchiveBudgetError(
            f"semantic validation working set is {validation_peak} bytes; "
            f"maximum is {max_working_set_bytes}"
        )
    if smoke_peak > max_working_set_bytes:
        raise SemanticArchiveBudgetError(
            f"semantic smoke working set is {smoke_peak} bytes; "
            f"maximum is {max_working_set_bytes}"
        )
