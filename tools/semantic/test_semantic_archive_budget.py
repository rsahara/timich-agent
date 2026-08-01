#!/usr/bin/env python3

from __future__ import annotations

import io
from pathlib import Path
import struct
import tempfile
import unittest
from unittest import mock
import zipfile

from semantic_archive_budget import (
    EOCD,
    EOCD_SIGNATURE,
    ZIP64_EOCD,
    ZIP64_EOCD_SIGNATURE,
    ZIP64_LOCATOR,
    ZIP64_LOCATOR_SIGNATURE,
    SemanticArchiveBudgetError,
    enforce_semantic_working_set,
    inspect_semantic_archive,
    preflight_semantic_archive_directory,
)


class SemanticArchiveBudgetTests(unittest.TestCase):
    def test_archive_entry_limit_is_checked_from_central_directory(self) -> None:
        with tempfile.TemporaryDirectory() as raw_temp:
            path = Path(raw_temp) / "pack.zip"
            with zipfile.ZipFile(path, "w") as archive:
                archive.writestr("one", b"1")
                archive.writestr("two", b"2")
                archive.writestr("three", b"3")
            with mock.patch("semantic_archive_budget.zipfile.ZipFile", side_effect=AssertionError("allocated")):
                with self.assertRaisesRegex(SemanticArchiveBudgetError, "3 entries"):
                    inspect_semantic_archive(path, max_entries=2)

    def test_archive_central_directory_limit_is_checked_before_zipfile(self) -> None:
        with tempfile.TemporaryDirectory() as raw_temp:
            path = Path(raw_temp) / "pack.zip"
            with zipfile.ZipFile(path, "w") as archive:
                archive.writestr("payload", b"1")
            with mock.patch("semantic_archive_budget.zipfile.ZipFile", side_effect=AssertionError("allocated")):
                with self.assertRaisesRegex(SemanticArchiveBudgetError, "central directory"):
                    inspect_semantic_archive(path, max_central_directory_bytes=1)

    def test_zip64_directory_metadata_is_bounded_before_zipfile(self) -> None:
        with tempfile.TemporaryDirectory() as raw_temp:
            path = Path(raw_temp) / "pack.zip"
            with zipfile.ZipFile(path, "w") as archive:
                archive.writestr("payload", b"1")
            raw = path.read_bytes()
            eocd_offset = raw.rfind(EOCD_SIGNATURE)
            self.assertGreaterEqual(eocd_offset, 0)
            eocd = EOCD.unpack(raw[eocd_offset:eocd_offset + EOCD.size])
            entry_count = eocd[4]
            central_directory_size = eocd[5]
            central_directory_offset = eocd[6]
            zip64_eocd_offset = eocd_offset
            zip64_eocd = ZIP64_EOCD.pack(
                ZIP64_EOCD_SIGNATURE,
                ZIP64_EOCD.size - 12,
                45,
                45,
                0,
                0,
                entry_count,
                entry_count,
                central_directory_size,
                central_directory_offset,
            )
            locator = ZIP64_LOCATOR.pack(
                ZIP64_LOCATOR_SIGNATURE,
                0,
                zip64_eocd_offset,
                1,
            )
            zip64_marker = EOCD.pack(
                EOCD_SIGNATURE,
                0,
                0,
                0xFFFF,
                0xFFFF,
                0xFFFFFFFF,
                0xFFFFFFFF,
                0,
            )
            path.write_bytes(raw[:eocd_offset] + zip64_eocd + locator + zip64_marker)

            stats = inspect_semantic_archive(path)
            self.assertEqual(stats.entry_count, 1)
            with mock.patch("semantic_archive_budget.zipfile.ZipFile", side_effect=AssertionError("allocated")):
                with self.assertRaisesRegex(SemanticArchiveBudgetError, "1 entries"):
                    inspect_semantic_archive(path, max_entries=0)

    def test_zip64_extensible_record_cannot_select_different_zipfile_footer(self) -> None:
        with tempfile.TemporaryDirectory() as raw_temp:
            path = Path(raw_temp) / "pack.zip"
            with zipfile.ZipFile(path, "w") as archive:
                archive.writestr("payload", b"1")
            raw = path.read_bytes()
            eocd_offset = raw.rfind(EOCD_SIGNATURE)
            eocd = EOCD.unpack(raw[eocd_offset:eocd_offset + EOCD.size])
            entry_count = eocd[4]
            central_directory_size = eocd[5]
            central_directory_offset = eocd[6]
            primary = ZIP64_EOCD.pack(
                ZIP64_EOCD_SIGNATURE,
                (ZIP64_EOCD.size - 12) + ZIP64_EOCD.size,
                45,
                45,
                0,
                0,
                entry_count,
                entry_count,
                central_directory_size,
                central_directory_offset,
            )
            alternate = ZIP64_EOCD.pack(
                ZIP64_EOCD_SIGNATURE,
                ZIP64_EOCD.size - 12,
                45,
                45,
                0,
                0,
                entry_count,
                entry_count,
                70 * 1024 * 1024,
                central_directory_offset,
            )
            locator = ZIP64_LOCATOR.pack(
                ZIP64_LOCATOR_SIGNATURE,
                0,
                eocd_offset,
                1,
            )
            zip64_marker = EOCD.pack(
                EOCD_SIGNATURE,
                0,
                0,
                0xFFFF,
                0xFFFF,
                0xFFFFFFFF,
                0xFFFFFFFF,
                0,
            )
            path.write_bytes(raw[:eocd_offset] + primary + alternate + locator + zip64_marker)

            with mock.patch("semantic_archive_budget.zipfile.ZipFile", side_effect=AssertionError("allocated")):
                with self.assertRaisesRegex(SemanticArchiveBudgetError, "locator"):
                    inspect_semantic_archive(path)

    def test_declared_entry_count_is_checked_against_actual_records_before_zipfile(self) -> None:
        with tempfile.TemporaryDirectory() as raw_temp:
            path = Path(raw_temp) / "pack.zip"
            with zipfile.ZipFile(path, "w") as archive:
                archive.writestr("one", b"1")
                archive.writestr("two", b"2")
                archive.writestr("three", b"3")
            raw = bytearray(path.read_bytes())
            eocd_offset = raw.rfind(EOCD_SIGNATURE)
            eocd = list(EOCD.unpack(raw[eocd_offset:eocd_offset + EOCD.size]))
            eocd[3] = 1
            eocd[4] = 1
            raw[eocd_offset:eocd_offset + EOCD.size] = EOCD.pack(*eocd)
            path.write_bytes(raw)

            with mock.patch("semantic_archive_budget.zipfile.ZipFile", side_effect=AssertionError("allocated")):
                with self.assertRaisesRegex(SemanticArchiveBudgetError, "declares 1 entries but contains 3"):
                    inspect_semantic_archive(path)

    def test_comment_embedded_eocd_is_rejected_before_zipfile(self) -> None:
        with tempfile.TemporaryDirectory() as raw_temp:
            path = Path(raw_temp) / "pack.zip"
            inner = io.BytesIO()
            with zipfile.ZipFile(inner, "w") as archive:
                archive.writestr("inner-one", b"1")
                archive.writestr("inner-two", b"2")
            with zipfile.ZipFile(path, "w") as archive:
                archive.writestr("outer-only", b"1")
                archive.comment = inner.getvalue() + b"!"

            with zipfile.ZipFile(path) as archive:
                self.assertEqual(archive.namelist(), ["inner-one", "inner-two"])
            with mock.patch("semantic_archive_budget.zipfile.ZipFile", side_effect=AssertionError("allocated")):
                with self.assertRaisesRegex(SemanticArchiveBudgetError, "last ZIP end record"):
                    inspect_semantic_archive(path, max_entries=1)

    def test_classic_zip_with_65535_entries_is_not_misclassified_as_zip64(self) -> None:
        with tempfile.TemporaryDirectory() as raw_temp:
            path = Path(raw_temp) / "pack.zip"
            with zipfile.ZipFile(path, "w", allowZip64=False) as archive:
                for index in range(65_535):
                    archive.writestr(str(index), b"")

            stats = preflight_semantic_archive_directory(path)
            self.assertEqual(stats.entry_count, 65_535)

    def test_standard_library_zip64_boundary_is_accepted(self) -> None:
        with tempfile.TemporaryDirectory() as raw_temp:
            path = Path(raw_temp) / "pack.zip"
            with zipfile.ZipFile(path, "w") as archive:
                for index in range(65_536):
                    archive.writestr(str(index), b"")

            preflight = preflight_semantic_archive_directory(path)
            inspected = inspect_semantic_archive(path)
            self.assertEqual(preflight.entry_count, 65_536)
            self.assertEqual(inspected.entry_count, 65_536)

    def test_classic_entry_comment_can_start_with_zip64_locator_signature(self) -> None:
        with tempfile.TemporaryDirectory() as raw_temp:
            path = Path(raw_temp) / "pack.zip"
            info = zipfile.ZipInfo("payload")
            info.comment = ZIP64_LOCATOR_SIGNATURE + bytes(ZIP64_LOCATOR.size - 4)
            with zipfile.ZipFile(path, "w") as archive:
                archive.writestr(info, b"1")

            # Python 3.12's ZipFile misclassifies these valid trailing bytes as
            # a Zip64 locator. This regression targets our bounded footer
            # preflight, which must distinguish the central-entry comment
            # without depending on the later standard-library parser.
            stats = preflight_semantic_archive_directory(path)
            self.assertEqual(stats.entry_count, 1)

    def test_archive_uncompressed_limit_is_checked_without_extraction(self) -> None:
        with tempfile.TemporaryDirectory() as raw_temp:
            path = Path(raw_temp) / "pack.zip"
            with zipfile.ZipFile(path, "w", compression=zipfile.ZIP_DEFLATED) as archive:
                archive.writestr("payload", b"0" * 32)
            with self.assertRaisesRegex(SemanticArchiveBudgetError, "expands to 32 bytes"):
                inspect_semantic_archive(path, max_uncompressed_bytes=31)

    def test_download_and_extraction_share_one_working_set(self) -> None:
        enforce_semantic_working_set(6, 4, 4, max_working_set_bytes=10)
        with self.assertRaisesRegex(SemanticArchiveBudgetError, "validation working set"):
            enforce_semantic_working_set(7, 4, 3, max_working_set_bytes=10)
        with self.assertRaisesRegex(SemanticArchiveBudgetError, "smoke working set"):
            enforce_semantic_working_set(6, 2, 5, max_working_set_bytes=10)


if __name__ == "__main__":
    unittest.main()
