from __future__ import annotations

from contextlib import redirect_stderr
from io import StringIO
from pathlib import Path
from unittest import mock
import re
import sys
import unittest


DOCS_DIR = Path(__file__).parent
sys.path.insert(0, str(DOCS_DIR))

import validate_sharkbite_matrix as validator


FIXTURE_DIR = DOCS_DIR / "testdata" / "validate_sharkbite_matrix"
CROSS_FILE_FIXTURE_PATHS = {
    "docs/testdata/validate_sharkbite_matrix/fixture_a.go",
    "docs/testdata/validate_sharkbite_matrix/fixture_b.go",
}
ANCHOR_FIXTURE_PATHS = {
    "docs/testdata/validate_sharkbite_matrix/fixture_a.go",
    "docs/testdata/validate_sharkbite_matrix/fixture_go_signature.go",
    "docs/testdata/validate_sharkbite_matrix/fixture_identifier_old.go",
    "docs/testdata/validate_sharkbite_matrix/fixture_main.c",
    "docs/testdata/validate_sharkbite_matrix/fixture_python_signature.py",
    "docs/testdata/validate_sharkbite_matrix/fixture_signature.h",
    "docs/testdata/validate_sharkbite_matrix/fixture_signature_elsewhere.h",
}


def load_fixture_lines(name: str) -> list[str]:
    return (FIXTURE_DIR / name).read_text(encoding="utf-8").splitlines()


def load_document_text() -> str:
    return validator.DOC_PATH.read_text(encoding="utf-8")


def load_row_manifest_fixture(name: str) -> tuple[tuple[str, str], ...]:
    return validator.parse_row_manifest_lines(load_fixture_lines(name), source=name)


def replace_pattern_once(text: str, pattern: str, replacement: str) -> str:
    rewritten, count = re.subn(pattern, replacement, text, count=1, flags=re.MULTILINE)
    if count != 1:
        raise AssertionError(f"expected exactly one match for pattern {pattern!r}")
    return rewritten


def remove_line_starting_once(lines: list[str], prefix: str) -> list[str]:
    removed = False
    rewritten: list[str] = []
    for line in lines:
        if not removed and line.startswith(prefix):
            removed = True
            continue
        rewritten.append(line)
    if not removed:
        raise AssertionError(f"expected to remove a line starting with {prefix!r}")
    return rewritten


def insert_after_line_starting_once(lines: list[str], prefix: str, new_line: str) -> list[str]:
    inserted = False
    rewritten: list[str] = []
    for line in lines:
        rewritten.append(line)
        if not inserted and line.startswith(prefix):
            rewritten.append(new_line)
            inserted = True
    if not inserted:
        raise AssertionError(f"expected to insert after a line starting with {prefix!r}")
    return rewritten


def make_anchor_fixture_lines(evidence: str) -> list[str]:
    return [
        "| ID | Sharkbite | Shoal Go | Shoal C ABI | Evidence | Status | Notes |",
        "| --- | --- | --- | --- | --- | --- | --- |",
        f"| SB-FIXTURE-401 | Example | — | — | {evidence} | Missing Go | |",
    ]


MATRIX_HEADER = "| ID | Sharkbite | Shoal Go | Shoal C ABI | Evidence | Status | Notes |"
MATRIX_SEPARATOR = "| --- | --- | --- | --- | --- | --- | --- |"
MATRIX_ROW = "| SB-FIXTURE-101 | — | — | — | — | Covered | fixture row |"


def matrix_table(header: str, separator: str | None, *rows: str) -> list[str]:
    lines = ["## Fixture heading", "", header]
    if separator is not None:
        lines.append(separator)
    lines.extend(rows)
    lines.append("")
    return lines


def replace_standalone_number(text: str, old: int, new: int) -> str:
    return re.sub(rf"(?<![A-Za-z\d,]){old}(?![A-Za-z\d,])", str(new), text)


def delete_matrix_row_consistently(text: str, row_id: str, prefix: str) -> str:
    """Delete one `Not required` row and restate every number the document derives.

    The result satisfies every internal cross-check — metadata, per-status
    summary, per-category summary and the narrative prose — while holding one
    row fewer than the audited inventory.
    """
    lines = text.splitlines()
    kept = [line for line in lines if not line.startswith(f"| {row_id} |")]
    assert len(kept) == len(lines) - 1, f"row {row_id} was not unique in the document"

    mutated = "\n".join(kept)
    mutated = replace_standalone_number(
        mutated, validator.EXPECTED_TOTAL_ROWS, validator.EXPECTED_TOTAL_ROWS - 1
    )
    mutated = replace_standalone_number(
        mutated,
        validator.EXPECTED_STATUS_COUNTS[validator.NOT_REQUIRED_STATUS],
        validator.EXPECTED_STATUS_COUNTS[validator.NOT_REQUIRED_STATUS] - 1,
    )
    removed_scope = next(
        entry[1] for entry in validator.load_expected_scope() if entry[0] == row_id
    )
    mutated = replace_standalone_number(
        mutated,
        validator.EXPECTED_SCOPE_COUNTS[removed_scope],
        validator.EXPECTED_SCOPE_COUNTS[removed_scope] - 1,
    )

    adjusted: list[str] = []
    for line in mutated.splitlines():
        cells = [cell.strip() for cell in line.split("|")[1:-1]]
        if len(cells) == 9 and cells[1] == f"`{prefix}`":
            cells[2] = str(int(cells[2]) - 1)
            cells[8] = str(int(cells[8]) - 1)
            line = "| " + " | ".join(cells) + " |"
        adjusted.append(line)
    return "\n".join(adjusted) + "\n"


def pinned_constants_for_deleted_not_required_row(prefix: str, row_id: str) -> dict:
    status_counts = dict(validator.EXPECTED_STATUS_COUNTS)
    status_counts[validator.NOT_REQUIRED_STATUS] -= 1
    prefix_totals = dict(validator.EXPECTED_PREFIX_TOTALS)
    prefix_totals[prefix] -= 1
    prefix_counts = {
        name: dict(counts) for name, counts in validator.EXPECTED_PREFIX_COUNTS.items()
    }
    prefix_counts[prefix][validator.NOT_REQUIRED_STATUS] -= 1
    manifest = tuple(entry for entry in validator.load_expected_rows() if entry[0] != row_id)
    scope_counts = dict(validator.EXPECTED_SCOPE_COUNTS)
    removed_scope = next(
        entry[1] for entry in validator.load_expected_scope() if entry[0] == row_id
    )
    scope_counts[removed_scope] -= 1
    scope_manifest = tuple(
        entry for entry in validator.load_expected_scope() if entry[0] != row_id
    )
    return {
        "EXPECTED_TOTAL_ROWS": validator.EXPECTED_TOTAL_ROWS - 1,
        "EXPECTED_REQUIRED_ROWS": validator.EXPECTED_REQUIRED_ROWS,
        "EXPECTED_STATUS_COUNTS": status_counts,
        "EXPECTED_PREFIX_TOTALS": prefix_totals,
        "EXPECTED_PREFIX_COUNTS": prefix_counts,
        "EXPECTED_SCOPE_COUNTS": scope_counts,
        "load_expected_rows": lambda: manifest,
        "load_expected_scope": lambda: scope_manifest,
    }


def fixture_evidence_cell(name: str) -> str:
    for line in load_fixture_lines(name):
        if line.startswith("| SB-"):
            return [cell.strip() for cell in line.split("|")[1:-1]][4]
    raise AssertionError(f"fixture {name} did not contain a matrix row")


class ValidateSharkbiteMatrixTests(unittest.TestCase):
    def assert_validation_fails(self, func, *expected_parts: str) -> str:
        stderr = StringIO()
        with redirect_stderr(stderr):
            with self.assertRaises(SystemExit):
                func()
        message = stderr.getvalue()
        for part in expected_parts:
            self.assertIn(part, message)
        return message

    def test_extract_path_anchor_bindings_keeps_adjacent_pairs(self) -> None:
        bindings = validator.extract_path_anchor_bindings(
            fixture_evidence_cell("multi_file_ok.md"),
            CROSS_FILE_FIXTURE_PATHS,
        )
        self.assertEqual(
            bindings["docs/testdata/validate_sharkbite_matrix/fixture_a.go"],
            ["TestAlpha"],
        )
        self.assertEqual(
            bindings["docs/testdata/validate_sharkbite_matrix/fixture_b.go"],
            ["TestBeta"],
        )

    def test_extract_path_anchor_bindings_preserves_ordered_multi_path_citations(self) -> None:
        bindings = validator.extract_path_anchor_bindings(
            "`TestAlpha` via `TestBeta` "
            "(`docs/testdata/validate_sharkbite_matrix/fixture_a.go`; "
            "`docs/testdata/validate_sharkbite_matrix/fixture_b.go`)",
            CROSS_FILE_FIXTURE_PATHS,
        )
        self.assertEqual(
            bindings["docs/testdata/validate_sharkbite_matrix/fixture_a.go"],
            ["TestAlpha"],
        )
        self.assertEqual(
            bindings["docs/testdata/validate_sharkbite_matrix/fixture_b.go"],
            ["TestBeta"],
        )

    def test_parse_rows_accepts_unique_row_ids(self) -> None:
        status_counts, prefix_counts, row_ids = validator.parse_rows(load_fixture_lines("unique_rows.md"))
        self.assertEqual(row_ids, (("SB-FIXTURE-101", "Covered"), ("SB-FIXTURE-102", "Missing Go")))
        self.assertEqual(status_counts["Covered"], 1)
        self.assertEqual(status_counts["Missing Go"], 1)
        self.assertEqual(prefix_counts["SB-FIXTURE"]["Covered"], 1)
        self.assertEqual(prefix_counts["SB-FIXTURE"]["Missing Go"], 1)

    def test_parse_rows_rejects_duplicate_ids_with_same_status(self) -> None:
        self.assert_validation_fails(
            lambda: validator.parse_rows(load_fixture_lines("duplicate_same_status.md")),
            "duplicate accepted row id SB-FIXTURE-101",
            "Covered vs Covered",
        )

    def test_parse_rows_rejects_duplicate_ids_with_different_status(self) -> None:
        self.assert_validation_fails(
            lambda: validator.parse_rows(load_fixture_lines("duplicate_different_status.md")),
            "duplicate accepted row id SB-FIXTURE-101",
            "Covered vs Missing Go",
        )

    def test_parse_rows_rejects_truncated_rows(self) -> None:
        self.assert_validation_fails(
            lambda: validator.parse_rows(load_fixture_lines("schema_truncated.md")),
            "malformed SB row SB-FIXTURE-301",
            "expected 7 cells, found 6",
        )

    def test_parse_rows_rejects_extra_cell_rows(self) -> None:
        self.assert_validation_fails(
            lambda: validator.parse_rows(load_fixture_lines("schema_extra_cell.md")),
            "malformed SB row SB-FIXTURE-302",
            "expected 7 cells, found 8",
        )

    def test_parse_rows_rejects_malformed_delimiter_rows(self) -> None:
        self.assert_validation_fails(
            lambda: validator.parse_rows(load_fixture_lines("schema_malformed.md")),
            "malformed SB row SB-FIXTURE-303",
            "expected 7 cells, found 5",
        )

    def test_parse_markdown_table_reads_rows_after_heading(self) -> None:
        headers, rows = validator.parse_markdown_table(load_fixture_lines("table_separator_ok.md"), "## Example heading")
        self.assertEqual(headers, ["Field", "Value"])
        self.assertEqual(rows, [["Alpha", "Beta"], ["Gamma", "Delta"]])

    def test_parse_markdown_table_requires_immediate_separator(self) -> None:
        self.assert_validation_fails(
            lambda: validator.parse_markdown_table(
                load_fixture_lines("table_separator_missing.md"),
                "## Example heading",
            ),
            "missing separator row after header under ## Example heading",
        )

    def test_parse_markdown_table_rejects_malformed_separator(self) -> None:
        self.assert_validation_fails(
            lambda: validator.parse_markdown_table(
                load_fixture_lines("table_separator_malformed.md"),
                "## Example heading",
            ),
            "malformed separator row under ## Example heading",
            "| nope | --- |",
        )

    def test_parse_markdown_table_rejects_wrong_width_separator(self) -> None:
        self.assert_validation_fails(
            lambda: validator.parse_markdown_table(
                load_fixture_lines("table_separator_wrong_width.md"),
                "## Example heading",
            ),
            "malformed separator row under ## Example heading",
            "expected 2 cells, found 3",
        )

    def test_parse_rows_metadata_extracts_total_and_required_counts(self) -> None:
        self.assertEqual(
            validator.parse_rows_metadata("3203 (2811 required by the [§2.2](#sec-2) release gate)"),
            (3203, 2811),
        )

    def test_parse_count_accepts_supported_plain_and_bold_integer_formats(self) -> None:
        self.assertEqual(validator.parse_count("0"), 0)
        self.assertEqual(validator.parse_count("3203"), 3203)
        self.assertEqual(validator.parse_count("3,203"), 3203)
        self.assertEqual(validator.parse_count("**3,203**"), 3203)

    def test_parse_count_rejects_unsupported_integer_formats(self) -> None:
        for cell in ("24x47", "-1", "2,44,7", "161.0", "392 rows"):
            with self.subTest(cell=cell):
                self.assert_validation_fails(
                    lambda value=cell: validator.parse_count(value),
                    f"unsupported count format: {cell!r}",
                )

    def test_parse_status_summary_reads_declared_totals(self) -> None:
        declared_counts, total = validator.parse_status_summary(load_fixture_lines("status_summary_valid.md"))
        self.assertEqual(total, 3203)
        self.assertEqual(declared_counts["Covered"], 1)
        self.assertEqual(declared_counts["Missing Go"], 2420)
        self.assertEqual(declared_counts["Not required (rationale required)"], 392)

    def test_parse_status_summary_rejects_unsupported_count_cells(self) -> None:
        for fixture_name, expected_cell in (
            ("status_summary_invalid_alphanumeric.md", "'24x47'"),
            ("status_summary_invalid_negative.md", "'-1'"),
            ("status_summary_invalid_bad_commas.md", "'2,44,7'"),
            ("status_summary_invalid_decimal.md", "'161.0'"),
            ("status_summary_invalid_extra_text.md", "'392 rows'"),
        ):
            with self.subTest(fixture_name=fixture_name):
                self.assert_validation_fails(
                    lambda name=fixture_name: validator.parse_status_summary(load_fixture_lines(name)),
                    "unsupported count format",
                    expected_cell,
                )

    def test_parse_status_summary_rejects_duplicate_total_rows(self) -> None:
        for fixture_name in (
            "status_summary_duplicate_total_same.md",
            "status_summary_duplicate_total_conflict.md",
        ):
            with self.subTest(fixture_name=fixture_name):
                self.assert_validation_fails(
                    lambda name=fixture_name: validator.parse_status_summary(load_fixture_lines(name)),
                    "duplicate total row in status summary table",
                )

    def test_parse_category_summary_reads_prefix_rows_and_totals(self) -> None:
        (
            declared_prefix_counts,
            declared_prefix_totals,
            declared_total_status_counts,
            declared_total_rows,
        ) = validator.parse_category_summary(
            [
                "### 25.2 By category",
                "",
                "| Section | Prefix | Rows | Covered | Missing Go | Missing C ABI | Behavior mismatch | Intentional divergence | Not required |",
                "| --- | --- | --- | --- | --- | --- | --- | --- | --- |",
                "| [§5](#sec-5) Packaging and imports | `SB-PKG` | 14 | 0 | 0 | 10 | 1 | 1 | 2 |",
                "| **Total** | | **14** | **0** | **0** | **10** | **1** | **1** | **2** |",
            ]
        )
        self.assertEqual(declared_total_rows, 14)
        self.assertEqual(declared_prefix_totals["SB-PKG"], 14)
        self.assertEqual(declared_prefix_counts["SB-PKG"]["Missing C ABI"], 10)
        self.assertEqual(
            declared_total_status_counts["Intentional divergence (approval required)"],
            1,
        )

    def test_parse_category_summary_rejects_duplicate_total_rows(self) -> None:
        for fixture_name in (
            "category_summary_duplicate_total_same.md",
            "category_summary_duplicate_total_conflict.md",
        ):
            with self.subTest(fixture_name=fixture_name):
                self.assert_validation_fails(
                    lambda name=fixture_name: validator.parse_category_summary(load_fixture_lines(name)),
                    "duplicate total row in category summary table",
                )

    def test_validate_expected_row_sequence_accepts_valid_fixture_sequence(self) -> None:
        validator.validate_expected_row_sequence(
            load_row_manifest_fixture("row_sequence_valid.txt"),
            load_row_manifest_fixture("row_sequence_expected.txt"),
        )

    def test_validate_expected_row_sequence_rejects_swapped_fixture_sequence(self) -> None:
        self.assert_validation_fails(
            lambda: validator.validate_expected_row_sequence(
                load_row_manifest_fixture("row_sequence_swap.txt"),
                load_row_manifest_fixture("row_sequence_expected.txt"),
            ),
            f"revision {validator.EXPECTED_REVISION} inventory rows changed",
            "missing [none]",
            "unexpected [none]",
            "moved [SB-FIXTURE-102 expected 2 found 3, SB-FIXTURE-103 expected 3 found 2]",
        )

    def test_validate_expected_row_sequence_rejects_reordered_fixture_sequence(self) -> None:
        self.assert_validation_fails(
            lambda: validator.validate_expected_row_sequence(
                load_row_manifest_fixture("row_sequence_reordered.txt"),
                load_row_manifest_fixture("row_sequence_expected.txt"),
            ),
            f"revision {validator.EXPECTED_REVISION} inventory rows changed",
            "missing [none]",
            "unexpected [none]",
            "moved [SB-FIXTURE-101 expected 1 found 2",
        )

    def test_validate_expected_row_sequence_rejects_missing_fixture_row(self) -> None:
        self.assert_validation_fails(
            lambda: validator.validate_expected_row_sequence(
                load_row_manifest_fixture("row_sequence_missing.txt"),
                load_row_manifest_fixture("row_sequence_expected.txt"),
            ),
            f"revision {validator.EXPECTED_REVISION} inventory rows changed",
            "missing [SB-FIXTURE-103 (Behavior mismatch)]",
        )

    def test_validate_expected_row_sequence_rejects_added_fixture_row(self) -> None:
        self.assert_validation_fails(
            lambda: validator.validate_expected_row_sequence(
                load_row_manifest_fixture("row_sequence_added.txt"),
                load_row_manifest_fixture("row_sequence_expected.txt"),
            ),
            f"revision {validator.EXPECTED_REVISION} inventory rows changed",
            "unexpected [SB-FIXTURE-999 (Missing Go)]",
        )

    def test_validate_expected_row_sequence_rejects_status_swap_within_a_section(self) -> None:
        self.assert_validation_fails(
            lambda: validator.validate_expected_row_sequence(
                load_row_manifest_fixture("row_sequence_reclassified.txt"),
                load_row_manifest_fixture("row_sequence_expected.txt"),
            ),
            f"revision {validator.EXPECTED_REVISION} inventory rows changed",
            "missing [none]",
            "unexpected [none]",
            "moved [none]",
            "reclassified [SB-FIXTURE-101 pinned Missing C ABI found Missing Go, "
            "SB-FIXTURE-102 pinned Missing Go found Missing C ABI]",
        )

    def test_parse_row_manifest_lines_requires_a_pinned_status(self) -> None:
        self.assert_validation_fails(
            lambda: load_row_manifest_fixture("row_sequence_missing_status.txt"),
            "invalid row manifest entry",
            "expected 'ROW-ID STATUS'",
        )

    def test_parse_row_manifest_lines_rejects_unknown_status(self) -> None:
        self.assert_validation_fails(
            lambda: load_row_manifest_fixture("row_sequence_invalid_status.txt"),
            "invalid status in row manifest entry",
            "'Totally Covered'",
        )

    def test_row_manifest_pins_the_status_of_every_audited_row(self) -> None:
        rows = validator.load_expected_rows()
        self.assertEqual(len(rows), validator.EXPECTED_TOTAL_ROWS)
        self.assertEqual(len({row_id for row_id, _status in rows}), validator.EXPECTED_TOTAL_ROWS)
        for _row_id, status in rows:
            self.assertIn(status, validator.STATUSES)
        document_rows = validator.parse_rows(load_document_text().splitlines())[2]
        self.assertEqual(rows, document_rows)

    def test_row_manifest_provenance_matches_expected_revision(self) -> None:
        manifest_lines = validator.EXPECTED_ROW_MANIFEST.read_text(encoding="utf-8").splitlines()
        validator.validate_expected_row_manifest_provenance(
            manifest_lines, source=validator.EXPECTED_ROW_MANIFEST.name
        )

    def test_row_manifest_provenance_rejects_stale_revision_header(self) -> None:
        manifest_lines = validator.EXPECTED_ROW_MANIFEST.read_text(encoding="utf-8").splitlines()
        mutated = list(manifest_lines)
        mutated[0] = mutated[0].replace(
            f"Revision-{validator.EXPECTED_REVISION}", "Revision-18"
        )
        self.assertNotEqual(mutated[0], manifest_lines[0])
        self.assert_validation_fails(
            lambda: validator.validate_expected_row_manifest_provenance(
                mutated, source=validator.EXPECTED_ROW_MANIFEST.name
            ),
            "row manifest header",
            f"revision {validator.EXPECTED_REVISION}",
        )

    def test_gap_completion_consistency_accepts_complete_pairs(self) -> None:
        lines = load_fixture_lines("gap_completion_valid.md")
        rows = validator.parse_rows(lines)[2]
        validator.validate_gap_completion_consistency(lines, rows)

    def test_gap_completion_tables_reject_duplicate_gap_ids(self) -> None:
        text = "\n".join(load_fixture_lines("gap_completion_valid.md"))
        row = (
            "| SB-GAP-C-001 | Table administration on the ABI | "
            "SB-TABLE-001, SB-CPP-016, SB-CPP-017 | merged | "
            "**Complete on the ABI for the listed connector-scoped entry points.** |"
        )
        text = text.replace(row, f"{row}\n{row}")
        self.assert_validation_fails(
            lambda: validator.parse_gap_completion_tables(text.splitlines()),
            "duplicate audited gap row SB-GAP-C-001",
        )

    def test_gap_completion_consistency_rejects_c_stage_missing_c_abi_drift(self) -> None:
        lines = load_fixture_lines("gap_completion_drift.md")
        rows = validator.parse_rows(lines)[2]
        self.assert_validation_fails(
            lambda: validator.validate_gap_completion_consistency(lines, rows),
            "SB-GAP-C-001 claims completion, but referenced rows remain one of Missing Go, Missing C ABI",
            "SB-CPP-016 (Missing C ABI)",
        )

    def test_gap_completion_consistency_rejects_go_stage_missing_go_drift(self) -> None:
        lines = load_fixture_lines("gap_completion_go_drift.md")
        rows = validator.parse_rows(lines)[2]
        self.assert_validation_fails(
            lambda: validator.validate_gap_completion_consistency(lines, rows),
            "SB-GAP-GO-001 claims completion, but referenced rows remain one of Missing Go",
            "SB-CONN-004 (Missing Go)",
        )

    def test_gap_completion_consistency_rejects_c_stage_missing_go_drift(self) -> None:
        lines = load_fixture_lines("gap_completion_c_stage_missing_go_drift.md")
        rows = validator.parse_rows(lines)[2]
        self.assert_validation_fails(
            lambda: validator.validate_gap_completion_consistency(lines, rows),
            "SB-GAP-C-004 claims completion, but referenced rows remain one of Missing Go, Missing C ABI",
            "SB-TABLE-010 (Missing Go)",
        )

    def test_gap_completion_consistency_rejects_empty_row_scope(self) -> None:
        text = "\n".join(load_fixture_lines("gap_completion_valid.md")).replace(
            "| SB-GAP-C-001 | Table administration on the ABI | SB-TABLE-001, SB-CPP-016, SB-CPP-017 |",
            "| SB-GAP-C-001 | Table administration on the ABI | |",
        )
        rows = validator.parse_rows(text.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_gap_completion_consistency(text.splitlines(), rows),
            "SB-GAP-C-001 claims completion without referencing any matrix rows",
        )

    def test_gap_completion_consistency_rejects_descending_ranges(self) -> None:
        text = "\n".join(load_fixture_lines("gap_completion_valid.md")).replace(
            "SB-SEC-001…SB-SEC-002",
            "SB-SEC-002…SB-SEC-001",
            1,
        )
        rows = validator.parse_rows(text.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_gap_completion_consistency(text.splitlines(), rows),
            "SB-GAP-GO-001 contains a descending range SB-SEC-002…SB-SEC-001",
        )

    def test_gap_completion_consistency_rejects_empty_range_boundaries(self) -> None:
        text = "\n".join(load_fixture_lines("gap_completion_valid.md")).replace(
            "SB-TABLE-001, SB-CPP-016, SB-CPP-017",
            "SB-TABLE-001…",
            1,
        )
        rows = validator.parse_rows(text.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_gap_completion_consistency(text.splitlines(), rows),
            "SB-GAP-C-001 contains an empty range boundary in 'SB-TABLE-001…'",
        )

    # ---- approved divergences ----------------------------------------------

    def test_divergence_approvals_accept_the_audited_document(self) -> None:
        text = load_document_text()
        rows = validator.parse_rows(text.splitlines())[2]
        validator.validate_divergence_approvals(text.splitlines(), rows)

    def test_divergence_approval_requires_the_pinned_approver(self) -> None:
        text = replace_pattern_once(
            load_document_text(),
            re.escape("| **@phrocker** | **2026-08-19** |"),
            "| **@someone-else** | **2026-08-19** |",
        )
        rows = validator.parse_rows(text.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
            "SB-DIV-016 approver is '**@someone-else**'",
        )

    def test_divergence_approval_requires_the_pinned_date(self) -> None:
        text = replace_pattern_once(
            load_document_text(),
            re.escape("| **@phrocker** | **2026-08-19** |"),
            "| **@phrocker** | **2026-09-01** |",
        )
        rows = validator.parse_rows(text.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
            "SB-DIV-016 approval date is '**2026-09-01**'",
        )

    def test_divergence_approval_requires_evidence_link(self) -> None:
        text = load_document_text().replace(
            "https://github.com/phrocker/shoal-oss/issues/81#issuecomment-5343583850",
            "https://github.com/phrocker/shoal-oss/issues/81",
        )
        rows = validator.parse_rows(text.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
            "SB-DIV-016 does not cite its approval evidence",
        )

    def test_divergence_approval_rejects_a_suffixed_evidence_link(self) -> None:
        text = load_document_text().replace(
            "[#81 decision](https://github.com/phrocker/shoal-oss/issues/81"
            "#issuecomment-5343583850)",
            "[#81 decision](https://github.com/phrocker/shoal-oss/issues/81"
            "#issuecomment-5343583850-not-the-approved-comment)",
        )
        rows = validator.parse_rows(text.splitlines())[2]
        message = self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
            "SB-DIV-016 does not cite its approval evidence",
        )
        self.assertIn("as the target of a Markdown link", message)

    def test_divergence_approval_rejects_evidence_mentioned_without_a_link(self) -> None:
        text = load_document_text().replace(
            "[#81 decision](https://github.com/phrocker/shoal-oss/issues/81"
            "#issuecomment-5343583850)",
            "#81 decision https://github.com/phrocker/shoal-oss/issues/81"
            "#issuecomment-5343583850",
        )
        rows = validator.parse_rows(text.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
            "SB-DIV-016 does not cite its approval evidence",
        )

    def test_unpinned_divergence_cannot_claim_an_approver(self) -> None:
        text = replace_pattern_once(
            load_document_text(),
            re.escape("| _unapproved_ | — |"),
            "| **@someone-else** | **2026-08-19** |",
        )
        rows = validator.parse_rows(text.splitlines())[2]
        message = self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
            "claims approver '**@someone-else**'",
        )
        self.assertIn("APPROVED_DIVERGENCES", message)

    def test_subsumed_divergence_cannot_claim_an_approver(self) -> None:
        text = replace_pattern_once(
            load_document_text(),
            re.escape("| _subsumed by [SB-DIV-016](#sec-26); not separately approved_ |"),
            "| **@phrocker** |",
        )
        rows = validator.parse_rows(text.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
            "SB-DIV-013 is subsumed by SB-DIV-016 and must record that it is not separately approved",
        )

    def test_subsumed_divergence_cannot_be_repointed_at_an_unrelated_row(self) -> None:
        text = replace_pattern_once(
            load_document_text(),
            re.escape("| SB-STAT-030, SB-GAP-GO-003 |"),
            "| SB-CFG-014, SB-GAP-GO-003 |",
        )
        rows = validator.parse_rows(text.splitlines())[2]
        message = self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
            "SB-DIV-013 declares its subsumed rows as",
            "SB-CFG-014",
        )
        self.assertIn("cannot be repointed at another row", message)

    def test_proposed_divergence_cannot_be_repointed_under_the_same_id(self) -> None:
        text = replace_pattern_once(
            load_document_text(),
            re.escape(
                "| SB-SCAN-024, SB-BASE-016, SB-BASE-019, SB-PANDA-009, "
                "SB-PANDA-012, SB-UNSAFE-008 |"
            ),
            "| SB-DATA-034, SB-BASE-016, SB-BASE-019, SB-PANDA-009, "
            "SB-PANDA-012, SB-UNSAFE-008 |",
        )
        rows = validator.parse_rows(text.splitlines())[2]
        message = self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
            "SB-DIV-006 declares its rows as",
            "SB-DATA-034",
        )
        self.assertIn("repointing a proposal needs a new entry", message)

    def test_proposed_divergence_without_a_pinned_row_cell_is_rejected(self) -> None:
        text = load_document_text()
        rows = validator.parse_rows(text.splitlines())[2]
        without = dict(validator.EXPECTED_PROPOSED_DIVERGENCE_ROW_CELLS)
        without.pop("SB-DIV-006")
        with mock.patch.object(
            validator, "EXPECTED_PROPOSED_DIVERGENCE_ROW_CELLS", without
        ):
            self.assert_validation_fails(
                lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
                "SB-DIV-006 is a proposed decision with no pinned Rows cell",
            )

    def test_stale_proposal_pin_for_a_removed_decision_is_rejected(self) -> None:
        text = load_document_text()
        rows = validator.parse_rows(text.splitlines())[2]
        extra = dict(validator.EXPECTED_PROPOSED_DIVERGENCE_ROW_CELLS)
        extra["SB-DIV-099"] = "SB-DATA-034"
        with mock.patch.object(
            validator, "EXPECTED_PROPOSED_DIVERGENCE_ROW_CELLS", extra
        ):
            self.assert_validation_fails(
                lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
                "§26 no longer lists as proposed",
                "SB-DIV-099",
            )

    def test_subsumed_scope_outside_the_covering_approval_is_rejected(self) -> None:
        text = replace_pattern_once(
            load_document_text(),
            re.escape("| SB-STAT-030, SB-GAP-GO-003 |"),
            "| SB-STAT-028, SB-GAP-GO-003 |",
        )
        rows = validator.parse_rows(text.splitlines())[2]
        repointed = {
            "SB-DIV-013": validator.SUBSUMED_DIVERGENCES["SB-DIV-013"]._replace(
                rows_cell="SB-STAT-028, SB-GAP-GO-003",
                rows=("SB-STAT-028",),
            )
        }
        self.assertNotIn(
            "SB-STAT-028", validator.APPROVED_DIVERGENCES["SB-DIV-016"].rows
        )
        with mock.patch.object(validator, "SUBSUMED_DIVERGENCES", repointed):
            self.assert_validation_fails(
                lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
                "names matrix rows outside that approval's scope",
                "SB-STAT-028",
            )

    def test_subsumed_rows_cell_must_name_exactly_the_pinned_matrix_rows(self) -> None:
        text = replace_pattern_once(
            load_document_text(),
            re.escape("| SB-STAT-030, SB-GAP-GO-003 |"),
            "| SB-STAT-030, SB-STAT-031, SB-GAP-GO-003 |",
        )
        rows = validator.parse_rows(text.splitlines())[2]
        widened = {
            "SB-DIV-013": validator.SUBSUMED_DIVERGENCES["SB-DIV-013"]._replace(
                rows_cell="SB-STAT-030, SB-STAT-031, SB-GAP-GO-003",
                rows=("SB-STAT-030",),
            )
        }
        with mock.patch.object(validator, "SUBSUMED_DIVERGENCES", widened):
            self.assert_validation_fails(
                lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
                "but its Rows cell names",
                "SB-STAT-031",
            )

    def test_subsumed_divergence_cannot_append_an_approval_claim(self) -> None:
        text = replace_pattern_once(
            load_document_text(),
            re.escape("| _subsumed by [SB-DIV-016](#sec-26); not separately approved_ |"),
            "| _subsumed by [SB-DIV-016](#sec-26); not separately approved; approved by @phrocker_ |",
        )
        rows = validator.parse_rows(text.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
            "must record that it is not separately approved as exactly",
        )

    def test_subsumed_divergence_must_carry_the_covering_decision_date(self) -> None:
        text = replace_pattern_once(
            load_document_text(),
            re.escape("| 2026-08-19 (subsumed) |"),
            "| 2026-09-01 (subsumed) |",
        )
        rows = validator.parse_rows(text.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
            "must carry the covering decision's date as exactly '2026-08-19 (subsumed)'",
        )

    def test_entry_point_row_cannot_join_the_approved_set(self) -> None:
        text = load_document_text()
        rows = list(validator.parse_rows(text.splitlines())[2])
        index = next(i for i, (row_id, _status) in enumerate(rows) if row_id == "SB-CONN-007")
        rows[index] = ("SB-CONN-007", validator.INTENTIONAL_DIVERGENCE_STATUS)
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
            "SB-CONN-007 is excluded from the approved cluster-status divergence and must stay 'Covered'",
        )

    def test_excluded_cluster_status_row_cannot_join_the_approval(self) -> None:
        text = load_document_text()
        rows = list(validator.parse_rows(text.splitlines())[2])
        index = next(i for i, (row_id, _status) in enumerate(rows) if row_id == "SB-STAT-038")
        rows[index] = ("SB-STAT-038", validator.INTENTIONAL_DIVERGENCE_STATUS)
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
            "SB-STAT-038 is excluded from the approved cluster-status divergence",
        )

    def test_approved_rows_cell_cannot_claim_a_different_set(self) -> None:
        text = replace_pattern_once(
            load_document_text(),
            re.escape("and closes the ledger entries SB-GAP-GO-003 and SB-GAP-C-006"),
            "and closes the ledger entries SB-GAP-GO-003 and SB-GAP-C-006, and covers "
            "SB-CONN-007",
        )
        rows = validator.parse_rows(text.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
            "SB-DIV-016 declares its approved rows as",
            "widening or narrowing an approval needs a new decision on the tracking issue",
        )

    def test_approved_row_set_must_match_the_pinned_rows(self) -> None:
        text = load_document_text()
        rows = list(validator.parse_rows(text.splitlines())[2])
        index = next(i for i, (row_id, _status) in enumerate(rows) if row_id == "SB-STAT-001")
        rows[index] = ("SB-STAT-001", "Missing Go")
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
            "the approved cluster-status divergence row set drifted: SB-STAT-001",
        )

    def test_unpinned_row_cannot_join_the_approved_set(self) -> None:
        text = load_document_text()
        rows = list(validator.parse_rows(text.splitlines())[2])
        index = next(i for i, (row_id, _status) in enumerate(rows) if row_id == "SB-SCAN-001")
        rows[index] = ("SB-SCAN-001", validator.INTENTIONAL_DIVERGENCE_STATUS)
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
            "the approved cluster-status divergence row set drifted: SB-SCAN-001",
        )

    def test_pinned_rows_must_agree_with_the_pinned_rows_cell(self) -> None:
        pins = validator.APPROVED_DIVERGENCES["SB-DIV-016"]
        narrowed = {"SB-DIV-016": pins._replace(rows=pins.rows[:-1])}
        text = load_document_text()
        rows = validator.parse_rows(text.splitlines())[2]
        with mock.patch.object(validator, "APPROVED_DIVERGENCES", narrowed):
            self.assert_validation_fails(
                lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
                "SB-DIV-016 pins a row set its Rows cell does not describe",
            )

    def test_pinned_approval_row_count_is_independently_pinned(self) -> None:
        text = load_document_text()
        rows = validator.parse_rows(text.splitlines())[2]
        with mock.patch.object(validator, "EXPECTED_APPROVED_DIVERGENCE_ROW_COUNT", 83):
            self.assert_validation_fails(
                lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
                "the pinned approvals cover 98 rows, but the audited decision covers 83",
            )

    def test_same_cardinality_substitution_from_another_section_is_rejected(self) -> None:
        pins = validator.APPROVED_DIVERGENCES["SB-DIV-016"]
        swapped = tuple(row for row in pins.rows if row != "SB-STAT-045") + ("SB-SEC-001",)
        self.assertEqual(len(swapped), len(pins.rows))
        substituted = {"SB-DIV-016": pins._replace(rows=swapped)}
        text = load_document_text()
        rows = validator.parse_rows(text.splitlines())[2]
        with mock.patch.object(validator, "APPROVED_DIVERGENCES", substituted):
            self.assert_validation_fails(
                lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
                "SB-DIV-016 pins a row set its Rows cell does not describe",
                "the pin differs at SB-SEC-001, SB-STAT-045",
            )

    def test_same_cardinality_substitution_within_the_section_is_rejected(self) -> None:
        pins = validator.APPROVED_DIVERGENCES["SB-DIV-016"]
        swapped = tuple(row for row in pins.rows if row != "SB-STAT-045") + ("SB-STAT-085",)
        self.assertEqual(len(swapped), len(pins.rows))
        substituted = {"SB-DIV-016": pins._replace(rows=swapped)}
        text = load_document_text()
        rows = validator.parse_rows(text.splitlines())[2]
        with mock.patch.object(validator, "APPROVED_DIVERGENCES", substituted):
            self.assert_validation_fails(
                lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
                "SB-DIV-016 pins a row set its Rows cell does not describe",
            )

    def test_pinning_a_row_the_rows_cell_excludes_is_rejected(self) -> None:
        pins = validator.APPROVED_DIVERGENCES["SB-DIV-016"]
        swapped = tuple(row for row in pins.rows if row != "SB-STAT-045") + ("SB-STAT-028",)
        substituted = {"SB-DIV-016": pins._replace(rows=swapped)}
        text = load_document_text()
        rows = validator.parse_rows(text.splitlines())[2]
        with mock.patch.object(validator, "APPROVED_DIVERGENCES", substituted):
            self.assert_validation_fails(
                lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
                "SB-DIV-016 pins SB-STAT-028 as approved",
                "but its Rows cell excludes those rows",
            )

    def test_rows_cell_must_state_its_scope_in_the_audited_form(self) -> None:
        pins = validator.APPROVED_DIVERGENCES["SB-DIV-016"]
        vague = pins.rows_cell.replace(
            "every `SB-STAT-*` row except SB-STAT-028 (a binding defect) and "
            "SB-STAT-038 (a Python attribute property).",
            "the cluster-status rows.",
        )
        self.assertNotEqual(vague, pins.rows_cell)
        with self.assertRaises(SystemExit):
            validator.parse_approval_scope_cell(vague)

    def test_rows_cell_cannot_exclude_a_row_outside_the_population(self) -> None:
        pins = validator.APPROVED_DIVERGENCES["SB-DIV-016"]
        foreign = pins.rows_cell.replace("SB-STAT-038 (a Python", "SB-SEC-038 (a Python")
        self.assertNotEqual(foreign, pins.rows_cell)
        with self.assertRaises(SystemExit):
            validator.parse_approval_scope_cell(foreign)

    def test_rows_cell_scope_is_parsed_from_the_cell_not_the_pin(self) -> None:
        pins = validator.APPROVED_DIVERGENCES["SB-DIV-016"]
        prefix, excluded = validator.parse_approval_scope_cell(pins.rows_cell)
        self.assertEqual(prefix, "SB-STAT")
        self.assertEqual(excluded, {"SB-STAT-028", "SB-STAT-038"})

    def test_shifting_the_pinned_window_is_rejected(self) -> None:
        """A same-cardinality pin that slides off the population's edge must fail.

        Deriving the population from the pin's own extremes would accept this:
        dropping the first row and appending one past the last keeps the count
        and the contiguity intact. The population comes from the matrix instead,
        so the shift is visible at both ends.
        """
        pins = validator.APPROVED_DIVERGENCES["SB-DIV-016"]
        shifted = tuple(row for row in pins.rows if row != "SB-STAT-001") + ("SB-STAT-085",)
        self.assertEqual(len(shifted), len(pins.rows))
        substituted = {"SB-DIV-016": pins._replace(rows=shifted)}
        text = load_document_text()
        rows = validator.parse_rows(text.splitlines())[2]
        with mock.patch.object(validator, "APPROVED_DIVERGENCES", substituted):
            self.assert_validation_fails(
                lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
                "SB-DIV-016 pins a row set its Rows cell does not describe",
                "the pin differs at SB-STAT-001, SB-STAT-085",
            )

    def test_dropping_a_row_from_the_matrix_population_is_rejected(self) -> None:
        pins = validator.APPROVED_DIVERGENCES["SB-DIV-016"]
        text = load_document_text()
        rows = [row for row in validator.parse_rows(text.splitlines())[2] if row[0] != "SB-STAT-050"]
        self.assertNotIn("SB-STAT-050", {row_id for row_id, _status in rows})
        self.assert_validation_fails(
            lambda: validator.validate_approval_scope_cell("SB-DIV-016", pins, rows),
            "SB-DIV-016 pins a row set its Rows cell does not describe",
            "the pin differs at SB-STAT-050",
        )

    def test_an_empty_population_is_rejected(self) -> None:
        pins = validator.APPROVED_DIVERGENCES["SB-DIV-016"]
        text = load_document_text()
        rows = [
            row for row in validator.parse_rows(text.splitlines())[2]
            if not row[0].startswith("SB-STAT-")
        ]
        self.assert_validation_fails(
            lambda: validator.validate_approval_scope_cell("SB-DIV-016", pins, rows),
            "SB-DIV-016 claims every SB-STAT row, but the matrix has no SB-STAT rows",
        )

    def test_excluding_a_row_the_matrix_does_not_carry_is_rejected(self) -> None:
        pins = validator.APPROVED_DIVERGENCES["SB-DIV-016"]
        moved = pins._replace(
            rows_cell=pins.rows_cell.replace("SB-STAT-038 (a Python", "SB-STAT-938 (a Python"),
        )
        self.assertNotEqual(moved.rows_cell, pins.rows_cell)
        text = load_document_text()
        rows = validator.parse_rows(text.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_approval_scope_cell("SB-DIV-016", moved, rows),
            "SB-DIV-016 excludes SB-STAT-938, which the matrix does not carry",
        )

    def test_row_cannot_be_pinned_as_both_approved_and_excluded(self) -> None:
        excluded = dict(validator.EXPECTED_DIVERGENCE_EXCLUDED_ROWS)
        excluded["SB-STAT-001"] = validator.INTENTIONAL_DIVERGENCE_STATUS
        text = load_document_text()
        rows = validator.parse_rows(text.splitlines())[2]
        with mock.patch.object(validator, "EXPECTED_DIVERGENCE_EXCLUDED_ROWS", excluded):
            self.assert_validation_fails(
                lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
                "rows are pinned as both approved and excluded from the approval: SB-STAT-001",
            )

    def test_decision_count_is_pinned(self) -> None:
        text = load_document_text()
        rows = validator.parse_rows(text.splitlines())[2]
        with mock.patch.object(validator, "EXPECTED_DIVERGENCE_DECISION_COUNT", 20):
            self.assert_validation_fails(
                lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
                "§26 lists 19 decisions, but the audited table holds 20",
            )

    def test_decision_count_is_derived_from_the_pinned_identifiers(self) -> None:
        self.assertEqual(
            validator.EXPECTED_DIVERGENCE_DECISION_COUNT,
            len(validator.EXPECTED_DIVERGENCE_DECISION_IDS),
        )
        self.assertEqual(
            len(set(validator.EXPECTED_DIVERGENCE_DECISION_IDS)),
            len(validator.EXPECTED_DIVERGENCE_DECISION_IDS),
        )

    def test_substituting_a_decision_for_a_new_one_of_the_same_count_is_rejected(self) -> None:
        text = load_document_text().replace("| SB-DIV-005 |", "| SB-DIV-999 |", 1)
        self.assertIn("| SB-DIV-999 |", text)
        rows = validator.parse_rows(text.splitlines())[2]
        entries = validator.parse_divergence_table(text.splitlines())
        self.assertEqual(len(entries), validator.EXPECTED_DIVERGENCE_DECISION_COUNT)
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
            "§26 does not list the audited decision identifiers",
            "missing SB-DIV-005",
            "unexpected SB-DIV-999",
        )

    def test_dropping_an_audited_decision_is_rejected(self) -> None:
        pinned = validator.EXPECTED_DIVERGENCE_DECISION_IDS + ("SB-DIV-020",)
        text = load_document_text()
        rows = validator.parse_rows(text.splitlines())[2]
        with mock.patch.object(validator, "EXPECTED_DIVERGENCE_DECISION_IDS", pinned), \
                mock.patch.object(validator, "EXPECTED_DIVERGENCE_DECISION_COUNT", len(pinned)):
            self.assert_validation_fails(
                lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
                "§26 lists 19 decisions, but the audited table holds 20",
            )

    def test_subsumed_entry_must_be_carved_out_of_the_blocking_entries(self) -> None:
        text = load_document_text().replace(
            "it is dormant rather than blocking, carries no approver of its own",
            "it blocks the gate, carries no approver of its own",
        )
        rows = validator.parse_rows(text.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
            "§26's preamble does not carve SB-DIV-013 out of the blocking entries",
        )

    def test_decision_split_prose_must_match_the_table(self) -> None:
        text = load_document_text().replace(
            "The table below lists 19 decisions — 12 approved, 1 subsumed by that approval,",
            "The table below lists 19 decisions — 13 approved, 0 subsumed by that approval,",
        )
        rows = validator.parse_rows(text.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
            "§26 does not state the decision split it lists",
        )

    def test_proposed_entries_must_exclude_the_approved_and_subsumed_decisions(self) -> None:
        text = load_document_text().replace(
            "The remaining 6 entries — every decision except the approved",
            "The remaining entries — every decision except the approved",
        )
        rows = validator.parse_rows(text.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
            "§26 describes entries as proposed without excluding the approved and subsumed",
        )

    def test_proposals_over_already_divergent_rows_must_be_called_out(self) -> None:
        text = load_document_text().replace(
            "Every row named by those proposals currently carries a non-divergence gap "
            "status; approval would move it to `Intentional divergence`.",
            "Some proposed rows already carry `Intentional divergence`.",
        )
        rows = validator.parse_rows(text.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
            "§26's preamble no longer matches the audited text",
        )

    def test_proposals_over_already_divergent_rows_must_name_every_such_decision(self) -> None:
        text = load_document_text().replace(
            "Every row named by those proposals currently carries a non-divergence gap "
            "status",
            "Every row except SB-DIV-006 currently carries a non-divergence gap status",
        )
        rows = validator.parse_rows(text.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
            "§26's preamble no longer matches the audited text",
        )

    def test_approved_row_dropped_from_its_decision_is_rejected(self) -> None:
        text = load_document_text().replace(
            "| SB-CFG-014, SB-CFG-022 |",
            "| SB-CFG-014 |",
        )
        rows = validator.parse_rows(text.splitlines())[2]
        message = self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
            "SB-DIV-002 declares its approved rows as 'SB-CFG-014'",
        )
        self.assertIn("widening or narrowing an approval needs a new decision", message)

    def test_dropping_the_approved_behavior_is_rejected(self) -> None:
        text = replace_pattern_once(
            load_document_text(),
            re.escape("**Capability discovery must not advertise cluster-status support.**"),
            "Capability discovery may advertise partial cluster-status support.",
        )
        rows = validator.parse_rows(text.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
            "the approved divergence no longer states its normative behavior",
        )

    def test_approved_behavior_must_stay_in_the_section_it_governs(self) -> None:
        moved = "**Capability discovery must not advertise cluster-status support.**"
        text = load_document_text()
        self.assertEqual(text.count(moved), 1)
        relocated = text.replace(moved, "", 1) + f"\n{moved}\n"
        self.assertIn(moved, relocated)
        self.assertIn(
            moved,
            validator.normalize_whitespace(relocated),
            "the sentence must still be somewhere in the document for this to test relocation",
        )
        rows = validator.parse_rows(relocated.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(relocated.splitlines(), rows),
            f"no longer states its normative behavior in {validator.APPROVAL_BEHAVIOR_SECTION_LABEL}",
            moved,
        )

    def test_missing_behavior_section_is_rejected(self) -> None:
        heading = validator.APPROVAL_BEHAVIOR_SECTION_HEADING
        text = load_document_text()
        self.assertEqual(text.count(heading), 1)
        renamed = text.replace(heading, "## 14. Matrix: cluster status (renamed)", 1)
        rows = validator.parse_rows(renamed.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(renamed.splitlines(), rows),
            "the section that must carry the approved divergence's normative behavior is missing",
        )

    def test_behavior_section_extraction_stops_at_the_next_section(self) -> None:
        text = load_document_text()
        body = validator.extract_section_body(
            text.splitlines(), validator.APPROVAL_BEHAVIOR_SECTION_HEADING
        )
        self.assertTrue(body)
        self.assertFalse([line for line in body if line.startswith("## ")])
        self.assertIn("SB-STAT-001", "\n".join(body))
        self.assertNotIn("SB-RFILE-001", "\n".join(body))
        self.assertEqual(validator.extract_section_body(text.splitlines(), "## nope"), [])

    def test_widening_the_approval_scope_to_the_whole_section_is_rejected(self) -> None:
        text = load_document_text()
        widened = replace_pattern_once(
            text,
            re.escape(
                "The single approval decision governing them is\n[SB-DIV-016](#sec-26), and "
                "it covers the 82 capability rows of this section —\nneither "
                "[SB-STAT-028](#sec-14) nor [SB-STAT-038](#sec-14) is in it."
            ),
            "The single approval decision covering the whole section is "
            "[SB-DIV-016](#sec-26).",
        )
        rows = validator.parse_rows(widened.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(widened.splitlines(), rows),
            "must state the approval's row scope in the approval's own terms",
        )

    def test_approval_scope_prose_tracks_the_pinned_row_count(self) -> None:
        inflated = replace_pattern_once(
            load_document_text(),
            re.escape("it covers the 82 capability rows of this section"),
            "it covers the 84 capability rows of this section",
        )
        rows = validator.parse_rows(inflated.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(inflated.splitlines(), rows),
            "must state the approval's row scope in the approval's own terms",
            str(len(validator.CLUSTER_STATUS_APPROVED_ROWS)),
        )

    def test_approval_scope_prose_names_the_pinned_excluded_rows(self) -> None:
        swapped = replace_pattern_once(
            load_document_text(),
            re.escape("nor [SB-STAT-038](#sec-14) is in it."),
            "nor [SB-STAT-039](#sec-14) is in it.",
        )
        rows = validator.parse_rows(swapped.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(swapped.splitlines(), rows),
            "must state the approval's row scope in the approval's own terms",
            "SB-STAT-038",
        )

    def test_permanence_claim_widened_to_the_whole_section_is_rejected(self) -> None:
        widened = replace_pattern_once(
            load_document_text(),
            re.escape(
                "**each of the 82 rows [SB-DIV-016](#sec-26) covers keeps `Intentional divergence\n"
                "(approval required)` permanently**"
            ),
            "**every row below keeps `Intentional divergence (approval required)` "
            "permanently**",
        )
        rows = validator.parse_rows(widened.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(widened.splitlines(), rows),
            "must bind the permanent divergence status to the approved rows",
        )

    def test_permanence_claim_tracks_the_pinned_row_count(self) -> None:
        inflated = replace_pattern_once(
            load_document_text(),
            re.escape("**each of the 82 rows [SB-DIV-016](#sec-26) covers keeps"),
            "**each of the 84 rows [SB-DIV-016](#sec-26) covers keeps",
        )
        rows = validator.parse_rows(inflated.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(inflated.splitlines(), rows),
            "must bind the permanent divergence status to the approved rows",
            str(len(validator.CLUSTER_STATUS_APPROVED_ROWS)),
        )

    def test_permanence_claim_must_name_the_governing_decision(self) -> None:
        repointed = replace_pattern_once(
            load_document_text(),
            re.escape("**each of the 82 rows [SB-DIV-016](#sec-26) covers keeps"),
            "**each of the 82 rows [SB-DIV-013](#sec-26) covers keeps",
        )
        rows = validator.parse_rows(repointed.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(repointed.splitlines(), rows),
            "must bind the permanent divergence status to the approved rows",
            "SB-DIV-016",
        )

    def test_approved_decision_dropping_an_approved_behavior_clause_is_rejected(self) -> None:
        stripped = replace_pattern_once(
            load_document_text(),
            re.escape(
                "; and capability discovery must not advertise cluster-status support."
            ),
            ".",
        )
        rows = validator.parse_rows(stripped.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(stripped.splitlines(), rows),
            "does not state the approved behavior it records",
            "capability discovery must not advertise cluster-status support",
        )

    def test_approved_decision_reversing_an_approved_behavior_clause_is_rejected(self) -> None:
        reversed_clause = replace_pattern_once(
            load_document_text(),
            re.escape("it must not return a fabricated or partially populated status object"),
            "it may return a partially populated status object",
        )
        rows = validator.parse_rows(reversed_clause.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(reversed_clause.splitlines(), rows),
            "does not state the approved behavior it records",
            "must not return a fabricated or partially populated status object",
        )

    def test_evidence_citation_inside_a_code_span_is_not_a_link(self) -> None:
        backquoted = replace_pattern_once(
            load_document_text(),
            re.escape(
                "[#81 decision](https://github.com/phrocker/shoal-oss/issues/81"
                "#issuecomment-5343583850)"
            ),
            "`[#81 decision](https://github.com/phrocker/shoal-oss/issues/81"
            "#issuecomment-5343583850)`",
        )
        rows = validator.parse_rows(backquoted.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(backquoted.splitlines(), rows),
            "does not cite its approval evidence",
        )

    def test_approved_decision_contradicted_by_an_appended_sentence_is_rejected(self) -> None:
        contradicted = replace_pattern_once(
            load_document_text(),
            re.escape(
                "It creates two obligations rather than closing the work silently: "
                "[SB-GAP-T-009](#sec-23) and [SB-GAP-P-006](#sec-23)."
            ),
            "It creates two obligations rather than closing the work silently: "
            "[SB-GAP-T-009](#sec-23) and [SB-GAP-P-006](#sec-23). Current behavior may "
            "return a partially populated status object.",
        )
        self.assertIn(
            "it must not return a fabricated or partially populated status object",
            contradicted,
        )
        rows = validator.parse_rows(contradicted.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(contradicted.splitlines(), rows),
            "but the audited approval reads",
        )

    def test_evidence_citation_without_a_link_label_is_rejected(self) -> None:
        unlabelled = replace_pattern_once(
            load_document_text(),
            re.escape(
                "[#81 decision](https://github.com/phrocker/shoal-oss/issues/81"
                "#issuecomment-5343583850)"
            ),
            "](https://github.com/phrocker/shoal-oss/issues/81#issuecomment-5343583850)",
        )
        rows = validator.parse_rows(unlabelled.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(unlabelled.splitlines(), rows),
            "does not cite its approval evidence",
        )

    def test_markdown_link_target_pattern_requires_a_complete_link(self) -> None:
        self.assertEqual(
            validator.extract_markdown_link_targets("[label](real) ](bare)"),
            {"real"},
        )
        self.assertEqual(validator.extract_markdown_link_targets("![alt](img)"), set())

    def test_pinned_decision_text_must_state_every_pinned_behavior(self) -> None:
        pins = validator.APPROVED_DIVERGENCES["SB-DIV-016"]
        narrowed = pins._replace(
            impact_cell=pins.impact_cell.replace(
                "; and capability discovery must not advertise cluster-status support",
                "",
            )
        )
        self.assertNotEqual(narrowed.impact_cell, pins.impact_cell)
        text = load_document_text()
        rows = validator.parse_rows(text.splitlines())[2]
        with mock.patch.dict(
            validator.APPROVED_DIVERGENCES, {"SB-DIV-016": narrowed}, clear=False
        ):
            self.assert_validation_fails(
                lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
                "pinned decision text no longer states the pinned approved behavior",
            )

    def test_section_14_approval_block_rejects_an_appended_contradiction(self) -> None:
        contradicted = replace_pattern_once(
            load_document_text(),
            re.escape("No status object is returned, the exception"),
            "A partially populated status object may nevertheless be returned. The exception",
        )
        self.assertIn(
            "It **must not** return a fabricated or partially populated status object",
            contradicted,
        )
        rows = validator.parse_rows(contradicted.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(contradicted.splitlines(), rows),
            "approval block no longer matches the approved decision word for word",
        )

    def test_pinned_section_14_preamble_must_state_every_required_sentence(self) -> None:
        narrowed = validator.APPROVAL_BEHAVIOR_PREAMBLE.replace(
            "**Capability discovery must not advertise cluster-status support.**",
            "Capability discovery is out of scope.",
        )
        self.assertNotEqual(narrowed, validator.APPROVAL_BEHAVIOR_PREAMBLE)
        text = load_document_text()
        rows = validator.parse_rows(text.splitlines())[2]
        with mock.patch.object(validator, "APPROVAL_BEHAVIOR_PREAMBLE", narrowed):
            self.assert_validation_fails(
                lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
                "APPROVAL_BEHAVIOR_PREAMBLE no longer states something",
            )

    def test_section_26_preamble_rejects_an_appended_contradiction(self) -> None:
        contradicted = replace_pattern_once(
            load_document_text(),
            re.escape(
                "approval state lives in this table, not in the status column."
            ),
            "approval state lives in this table, not in the status column. SB-DIV-001 "
            "is also approved.",
        )
        rows = validator.parse_rows(contradicted.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(contradicted.splitlines(), rows),
            "preamble no longer matches the audited text",
        )

    def test_pinned_section_26_preamble_must_state_every_generated_sentence(self) -> None:
        narrowed = validator.DIVERGENCE_DECISION_PREAMBLE.replace(
            "The table below lists 19 decisions", "The table below lists 20 decisions"
        )
        self.assertNotEqual(narrowed, validator.DIVERGENCE_DECISION_PREAMBLE)
        text = load_document_text()
        rows = validator.parse_rows(text.splitlines())[2]
        with mock.patch.object(validator, "DIVERGENCE_DECISION_PREAMBLE", narrowed):
            self.assert_validation_fails(
                lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
                "DIVERGENCE_DECISION_PREAMBLE no longer states something",
            )

    def test_commented_out_evidence_citation_is_rejected(self) -> None:
        commented = replace_pattern_once(
            load_document_text(),
            re.escape(
                "[#81 decision](https://github.com/phrocker/shoal-oss/issues/81"
                "#issuecomment-5343583850)"
            ),
            "<!-- [#81 decision](https://github.com/phrocker/shoal-oss/issues/81"
            "#issuecomment-5343583850) -->",
        )
        rows = validator.parse_rows(commented.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(commented.splitlines(), rows),
            "does not cite its approval evidence",
        )

    def test_escaped_evidence_citation_is_rejected(self) -> None:
        escaped = replace_pattern_once(
            load_document_text(),
            re.escape(
                "[#81 decision](https://github.com/phrocker/shoal-oss/issues/81"
                "#issuecomment-5343583850)"
            ),
            "\\[#81 decision](https://github.com/phrocker/shoal-oss/issues/81"
            "#issuecomment-5343583850)",
        )
        rows = validator.parse_rows(escaped.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(escaped.splitlines(), rows),
            "does not cite its approval evidence",
        )

    def test_strip_non_rendered_markup_removes_comments_and_code(self) -> None:
        self.assertNotIn("](url)", validator.strip_non_rendered_markup("`[a](url)` text"))
        self.assertIn("](real)", validator.strip_non_rendered_markup("`[a](url)` [b](real)"))
        self.assertNotIn("](url)", validator.strip_non_rendered_markup("```\n[a](url)\n```"))
        self.assertNotIn("](url)", validator.strip_non_rendered_markup("``[a](url)``"))
        self.assertNotIn(
            "](url)", validator.strip_non_rendered_markup("<!-- [a](url) --> text")
        )
        self.assertNotIn(
            "](url)", validator.strip_non_rendered_markup("<code>[a](url)</code> text")
        )
        self.assertNotIn(
            "](url)", validator.strip_non_rendered_markup("<PRE>\n[a](url)\n</pre>")
        )
        self.assertIn(
            "](real)",
            validator.strip_non_rendered_markup("<code>[a](url)</code> [b](real)"),
        )

    def test_markdown_link_target_pattern_rejects_escaped_brackets(self) -> None:
        self.assertEqual(validator.extract_markdown_link_targets("\\[a](url)"), set())
        self.assertEqual(validator.extract_markdown_link_targets("[a\\](url)"), set())
        self.assertEqual(validator.extract_markdown_link_targets("[a](url)"), {"url"})

    @staticmethod
    def _wrap_section_preamble(text: str, heading: str, opener: str, closer: str) -> str:
        """Wrap a section's prose in markup that keeps it out of the rendered page."""
        lines = text.splitlines()
        start = lines.index(heading)
        first = last = None
        for index in range(start + 1, len(lines)):
            line = lines[index]
            if line.startswith("## ") or line.startswith("|"):
                break
            if line.strip():
                first = index if first is None else first
                last = index
        if first is None or last is None:
            raise AssertionError(f"no preamble prose to wrap under {heading!r}")
        wrapped = lines[:first] + [opener] + lines[first : last + 1] + [closer] + lines[last + 1 :]
        return "\n".join(wrapped) + "\n"

    @staticmethod
    def _comment_out_paragraph(text: str, phrase: str) -> str:
        """Wrap the paragraph holding `phrase` in an HTML comment."""
        lines = text.splitlines()
        matches = [i for i, line in enumerate(lines) if phrase in line]
        if len(matches) != 1:
            raise AssertionError(f"expected one line holding {phrase!r}, found {len(matches)}")
        start = end = matches[0]
        while start > 0 and lines[start - 1].strip():
            start -= 1
        while end + 1 < len(lines) and lines[end + 1].strip():
            end += 1
        wrapped = lines[:start] + ["<!--"] + lines[start : end + 1] + ["-->"] + lines[end + 1 :]
        return "\n".join(wrapped) + "\n"

    def test_markdown_link_target_pattern_rejects_invisible_labels(self) -> None:
        self.assertEqual(validator.extract_markdown_link_targets("[](url)"), set())
        self.assertEqual(validator.extract_markdown_link_targets("[   ](url)"), set())
        self.assertEqual(validator.extract_markdown_link_targets("[a](url)"), {"url"})

    def test_link_labels_that_render_nothing_visible_are_rejected(self) -> None:
        for label in ("\u200b", "\u200b\u200c", "&nbsp;", "&#8203;", "\u00a0", "\ufeff"):
            with self.subTest(label=label):
                self.assertFalse(validator.has_visible_link_label(label))
                self.assertEqual(
                    validator.extract_markdown_link_targets(f"[{label}](url)"), set()
                )
        for label in ("a", " a ", "&amp;", "\u200b0"):
            with self.subTest(label=label):
                self.assertTrue(validator.has_visible_link_label(label))
                self.assertEqual(
                    validator.extract_markdown_link_targets(f"[{label}](url)"), {"url"}
                )

    def test_link_labels_of_only_raw_html_or_combining_marks_are_rejected(self) -> None:
        # `<span></span>` is markup, not text, and a combining mark with no base
        # character has nothing to attach to; both render an empty anchor.
        for label in ("<span></span>", "<b><i></i></b>", "\ufe0f", "\u034f", "\u0301"):
            with self.subTest(label=label):
                self.assertFalse(validator.has_visible_link_label(label))
                self.assertEqual(
                    validator.extract_markdown_link_targets(f"[{label}](url)"), set()
                )
        # Escaped tags reach the reader as visible text, and a mark that follows
        # a base character renders on top of something readable.
        for label in ("&lt;span&gt;", "<b>x</b>", "e\u0301"):
            with self.subTest(label=label):
                self.assertTrue(validator.has_visible_link_label(label))
                self.assertEqual(
                    validator.extract_markdown_link_targets(f"[{label}](url)"), {"url"}
                )

    def test_evidence_citation_with_an_unreadable_label_is_rejected(self) -> None:
        for label in ("<span></span>", "\ufe0f", "\u034f"):
            with self.subTest(label=label):
                invisible = replace_pattern_once(
                    load_document_text(),
                    re.escape(
                        "[#81 decision](https://github.com/phrocker/shoal-oss/issues/81"
                        "#issuecomment-5343583850)"
                    ),
                    f"[{label}](https://github.com/phrocker/shoal-oss/issues/81"
                    "#issuecomment-5343583850)",
                )
                rows = validator.parse_rows(invisible.splitlines())[2]
                self.assert_validation_fails(
                    lambda: validator.validate_divergence_approvals(
                        invisible.splitlines(), rows
                    ),
                    "does not cite its approval evidence",
                )

    def test_evidence_citation_with_a_zero_width_label_is_rejected(self) -> None:
        for label in ("\u200b", "&nbsp;"):
            with self.subTest(label=label):
                invisible = replace_pattern_once(
                    load_document_text(),
                    re.escape(
                        "[#81 decision](https://github.com/phrocker/shoal-oss/issues/81"
                        "#issuecomment-5343583850)"
                    ),
                    f"[{label}](https://github.com/phrocker/shoal-oss/issues/81"
                    "#issuecomment-5343583850)",
                )
                rows = validator.parse_rows(invisible.splitlines())[2]
                self.assert_validation_fails(
                    lambda: validator.validate_divergence_approvals(
                        invisible.splitlines(), rows
                    ),
                    "does not cite its approval evidence",
                )

    def test_evidence_citation_inside_raw_html_code_is_rejected(self) -> None:
        for opener, closer in (("<code>", "</code>"), ("<pre>", "</pre>")):
            with self.subTest(tag=opener):
                literal = replace_pattern_once(
                    load_document_text(),
                    re.escape(
                        "[#81 decision](https://github.com/phrocker/shoal-oss/issues/81"
                        "#issuecomment-5343583850)"
                    ),
                    f"{opener}[#81 decision]"
                    "(https://github.com/phrocker/shoal-oss/issues/81"
                    f"#issuecomment-5343583850){closer}",
                )
                rows = validator.parse_rows(literal.splitlines())[2]
                self.assert_validation_fails(
                    lambda: validator.validate_divergence_approvals(
                        literal.splitlines(), rows
                    ),
                    "does not cite its approval evidence",
                )

    def test_evidence_citation_with_an_empty_label_is_rejected(self) -> None:
        invisible = replace_pattern_once(
            load_document_text(),
            re.escape(
                "[#81 decision](https://github.com/phrocker/shoal-oss/issues/81"
                "#issuecomment-5343583850)"
            ),
            "[](https://github.com/phrocker/shoal-oss/issues/81"
            "#issuecomment-5343583850)",
        )
        rows = validator.parse_rows(invisible.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(invisible.splitlines(), rows),
            "does not cite its approval evidence",
        )

    def test_section_14_preamble_hidden_in_a_comment_is_rejected(self) -> None:
        commented = self._wrap_section_preamble(
            load_document_text(),
            validator.APPROVAL_BEHAVIOR_SECTION_HEADING,
            "<!--",
            "-->",
        )
        pinned = validator.APPROVAL_BEHAVIOR_PREAMBLE
        with mock.patch.object(
            validator, "APPROVAL_BEHAVIOR_PREAMBLE", f"<!-- {pinned} -->"
        ):
            preamble = validator.extract_section_preamble(
                commented.splitlines(), validator.APPROVAL_BEHAVIOR_SECTION_HEADING
            )
            self.assertEqual(
                validator.normalize_whitespace("\n".join(preamble)),
                validator.APPROVAL_BEHAVIOR_PREAMBLE.format(
                    revision=validator.CLUSTER_STATUS_APPROVAL_REVISION
                ),
                "the coordinated pin must still match for this to test rendering",
            )
            rows = validator.parse_rows(commented.splitlines())[2]
            self.assert_validation_fails(
                lambda: validator.validate_divergence_approvals(commented.splitlines(), rows),
                "wrapped in markup that keeps it out of the rendered document",
            )

    def test_section_26_preamble_hidden_in_a_fence_is_rejected(self) -> None:
        fenced = self._wrap_section_preamble(
            load_document_text(),
            validator.DIVERGENCE_DECISION_SECTION_HEADING,
            "```",
            "```",
        )
        pinned = validator.DIVERGENCE_DECISION_PREAMBLE
        with mock.patch.object(
            validator, "DIVERGENCE_DECISION_PREAMBLE", f"``` {pinned} ```"
        ):
            preamble = validator.extract_section_preamble(
                fenced.splitlines(), validator.DIVERGENCE_DECISION_SECTION_HEADING
            )
            self.assertEqual(
                validator.normalize_whitespace("\n".join(preamble)),
                validator.DIVERGENCE_DECISION_PREAMBLE,
                "the coordinated pin must still match for this to test rendering",
            )
            rows = validator.parse_rows(fenced.splitlines())[2]
            self.assert_validation_fails(
                lambda: validator.validate_divergence_approvals(fenced.splitlines(), rows),
                "wrapped in markup that keeps it out of the rendered document",
            )

    def test_commented_behavior_clause_in_the_impact_cell_is_rejected(self) -> None:
        commented = replace_pattern_once(
            load_document_text(),
            re.escape("it must not return a fabricated or partially populated status object;"),
            "<!-- it must not return a fabricated or partially populated status object; -->",
        )
        rows = validator.parse_rows(commented.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(commented.splitlines(), rows),
            "carries an HTML comment",
        )

    def test_commented_behavior_clause_in_the_pinned_impact_cell_is_rejected(self) -> None:
        pinned = validator.APPROVED_DIVERGENCES["SB-DIV-016"]
        hidden = pinned._replace(
            impact_cell=pinned.impact_cell.replace(
                "it must not return a fabricated or partially populated status object;",
                "<!-- it must not return a fabricated or partially populated status object; -->",
            )
        )
        self.assertIn("<!--", hidden.impact_cell)
        text = load_document_text()
        rows = validator.parse_rows(text.splitlines())[2]
        with mock.patch.dict(
            validator.APPROVED_DIVERGENCES, {"SB-DIV-016": hidden}, clear=False
        ):
            self.assert_validation_fails(
                lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
                "pinned Impact cell carries an HTML comment",
            )

    def test_commented_approval_scope_cell_is_rejected(self) -> None:
        commented = replace_pattern_once(
            load_document_text(),
            re.escape("| " + validator.CLUSTER_STATUS_APPROVED_ROWS_CELL + " |"),
            "| <!-- " + validator.CLUSTER_STATUS_APPROVED_ROWS_CELL + " --> |",
        )
        rows = validator.parse_rows(commented.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(commented.splitlines(), rows),
            "Rows cell carries an HTML comment",
        )

    @staticmethod
    def _comment_entry_cell(text: str, entry_id: str, cell: str) -> str:
        """Hide one \u00a726 cell behind an HTML comment, leaving the row otherwise intact."""
        lines = text.splitlines()
        matches = [i for i, line in enumerate(lines) if line.startswith(f"| {entry_id} |")]
        if len(matches) != 1:
            raise AssertionError(f"expected one {entry_id} row, found {len(matches)}")
        index = matches[0]
        target = f"| {cell} |"
        if lines[index].count(target) != 1:
            raise AssertionError(f"expected one {target!r} in {entry_id}'s row")
        lines[index] = lines[index].replace(target, f"| <!-- {cell} --> |", 1)
        return "\n".join(lines) + "\n"

    def test_commented_approver_and_date_cells_are_rejected(self) -> None:
        # Commenting a cell out on both sides leaves the comparison equal, so
        # only a rendering check can catch it.
        pinned = validator.APPROVED_DIVERGENCES["SB-DIV-016"]
        for field, cell, label in (
            ("approver", pinned.approver, "Approver cell carries an HTML comment"),
            ("date", pinned.date, "Date cell carries an HTML comment"),
        ):
            with self.subTest(field=field):
                commented = self._comment_entry_cell(
                    load_document_text(), "SB-DIV-016", cell
                )
                hidden = pinned._replace(**{field: f"<!-- {cell} -->"})
                rows = validator.parse_rows(commented.splitlines())[2]
                with mock.patch.dict(
                    validator.APPROVED_DIVERGENCES, {"SB-DIV-016": hidden}, clear=False
                ):
                    self.assert_validation_fails(
                        lambda: validator.validate_divergence_approvals(
                            commented.splitlines(), rows
                        ),
                        label,
                    )

    def test_commented_subsumed_rows_cell_is_rejected(self) -> None:
        subsumed = validator.SUBSUMED_DIVERGENCES["SB-DIV-013"]
        commented = self._comment_entry_cell(
            load_document_text(), "SB-DIV-013", subsumed.rows_cell
        )
        hidden = subsumed._replace(rows_cell=f"<!-- {subsumed.rows_cell} -->")
        rows = validator.parse_rows(commented.splitlines())[2]
        with mock.patch.dict(
            validator.SUBSUMED_DIVERGENCES, {"SB-DIV-013": hidden}, clear=False
        ):
            self.assert_validation_fails(
                lambda: validator.validate_divergence_approvals(
                    commented.splitlines(), rows
                ),
                "Rows cell carries an HTML comment",
            )

    def _replace_ledger_rows_cell(self, text: str, replacement: str) -> str:
        """Rewrite the cluster-status ledger entry's Matrix rows cell."""
        lines = text.splitlines()
        matches = [
            i
            for i, line in enumerate(lines)
            if line.startswith(f"| {validator.CLUSTER_STATUS_LEDGER_ENTRY} |")
        ]
        if len(matches) != 1:
            raise AssertionError(
                f"expected one {validator.CLUSTER_STATUS_LEDGER_ENTRY} ledger row, "
                f"found {len(matches)}"
            )
        index = matches[0]
        cells = lines[index].split("|")
        cells[3] = f" {replacement} "
        lines[index] = "|".join(cells)
        return "\n".join(lines) + "\n"

    def test_commented_subsumed_approver_and_date_cells_are_rejected(self) -> None:
        # The subsumed approver and date are generated from templates rather than
        # pinned literally, so the coordinated edit is to comment the template out
        # alongside the cell; the comparison stays equal either way.
        subsumed_id = "SB-DIV-013"
        covering_id = validator.SUBSUMED_DIVERGENCES[subsumed_id].covering
        approver_cell = validator.SUBSUMED_DIVERGENCE_APPROVER.format(
            covering=covering_id
        )
        date_cell = validator.SUBSUMED_DIVERGENCE_DATE.format(
            date=validator.APPROVED_DIVERGENCES[covering_id].date.strip("*")
        )
        for attribute, cell, label in (
            (
                "SUBSUMED_DIVERGENCE_APPROVER",
                approver_cell,
                f"{subsumed_id}'s Approver cell carries an HTML comment",
            ),
            (
                "SUBSUMED_DIVERGENCE_DATE",
                date_cell,
                f"{subsumed_id}'s Date cell carries an HTML comment",
            ),
        ):
            with self.subTest(attribute=attribute):
                template = getattr(validator, attribute)
                commented = self._comment_entry_cell(
                    load_document_text(), subsumed_id, cell
                )
                rows = validator.parse_rows(commented.splitlines())[2]
                with mock.patch.object(validator, attribute, f"<!-- {template} -->"):
                    self.assert_validation_fails(
                        lambda: validator.validate_divergence_approvals(
                            commented.splitlines(), rows
                        ),
                        label,
                    )

    def test_commented_unapproved_approver_and_date_cells_are_rejected(self) -> None:
        # "_unapproved_" only warns a reader who can see it. Commenting the marker
        # out on both sides leaves the equality intact and the approval state blank.
        entries = validator.parse_divergence_table(load_document_text().splitlines())
        unapproved = sorted(
            entry_id
            for entry_id in entries
            if entry_id not in validator.APPROVED_DIVERGENCES
            and entry_id not in validator.SUBSUMED_DIVERGENCES
        )
        self.assertTrue(unapproved, "expected at least one unapproved §26 entry")
        entry_id = unapproved[0]
        for attribute, label in (
            (
                "UNAPPROVED_DIVERGENCE_APPROVER",
                f"{entry_id}'s Approver cell carries an HTML comment",
            ),
            (
                "UNAPPROVED_DIVERGENCE_DATE",
                f"{entry_id}'s Date cell carries an HTML comment",
            ),
        ):
            with self.subTest(attribute=attribute):
                marker = getattr(validator, attribute)
                commented = self._comment_entry_cell(
                    load_document_text(), entry_id, marker
                )
                rows = validator.parse_rows(commented.splitlines())[2]
                with mock.patch.object(validator, attribute, f"<!-- {marker} -->"):
                    self.assert_validation_fails(
                        lambda: validator.validate_divergence_approvals(
                            commented.splitlines(), rows
                        ),
                        label,
                    )

    def test_cluster_status_ledger_entry_must_cover_every_approved_row(self) -> None:
        # §25.3 reports the ledger entry as closed by approval, so a scope narrower
        # than the approval would leave the remaining rows in no ledger item at all.
        narrowed = self._replace_ledger_rows_cell(
            load_document_text(),
            "SB-STAT-001…SB-STAT-027, SB-STAT-029…SB-STAT-038, SB-CONN-007",
        )
        rows = validator.parse_rows(narrowed.splitlines())[2]
        message = self.assert_validation_fails(
            lambda: validator.validate_cluster_status_ledger_scope(
                narrowed.splitlines(), rows
            ),
            validator.CLUSTER_STATUS_LEDGER_ENTRY,
            "must scope itself to every cluster-status row",
        )
        self.assertIn("SB-STAT-039", message)

    def test_cluster_status_ledger_entry_rejects_rows_outside_the_surface(self) -> None:
        # The expected population is generated from the matrix, so padding the cell
        # with an unrelated row is a failure rather than a harmless superset.
        padded = self._replace_ledger_rows_cell(
            load_document_text(),
            "SB-STAT-001…SB-STAT-027, SB-STAT-029…SB-STAT-084, SB-CONN-007, SB-CONN-001",
        )
        rows = validator.parse_rows(padded.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_cluster_status_ledger_scope(
                padded.splitlines(), rows
            ),
            "unexpected SB-CONN-001",
        )

    def test_commented_cluster_status_ledger_rows_cell_is_rejected(self) -> None:
        commented = self._replace_ledger_rows_cell(
            load_document_text(),
            "<!-- SB-STAT-001…SB-STAT-027, SB-STAT-029…SB-STAT-084, SB-CONN-007 -->",
        )
        rows = validator.parse_rows(commented.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_cluster_status_ledger_scope(
                commented.splitlines(), rows
            ),
            f"{validator.CLUSTER_STATUS_LEDGER_ENTRY}'s Matrix rows cell carries an HTML comment",
        )

    def test_commented_proposed_rows_cell_is_rejected(self) -> None:
        entry_id = sorted(validator.EXPECTED_PROPOSED_DIVERGENCE_ROW_CELLS)[0]
        cell = validator.EXPECTED_PROPOSED_DIVERGENCE_ROW_CELLS[entry_id]
        commented = self._comment_entry_cell(load_document_text(), entry_id, cell)
        pins = dict(validator.EXPECTED_PROPOSED_DIVERGENCE_ROW_CELLS)
        pins[entry_id] = f"<!-- {cell} -->"
        rows = validator.parse_rows(commented.splitlines())[2]
        with mock.patch.object(
            validator, "EXPECTED_PROPOSED_DIVERGENCE_ROW_CELLS", pins
        ):
            self.assert_validation_fails(
                lambda: validator.validate_divergence_approvals(
                    commented.splitlines(), rows
                ),
                "proposed Rows cell carries an HTML comment",
            )

    def test_commented_subsumption_rationale_is_rejected(self) -> None:
        commented = replace_pattern_once(
            load_document_text(),
            re.escape("**Subsumed** by the approved [SB-DIV-016](#sec-26):"),
            "<!-- **Subsumed** by the approved [SB-DIV-016](#sec-26): -->",
        )
        rows = validator.parse_rows(commented.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(commented.splitlines(), rows),
            "carries an HTML comment",
        )

    def test_commented_release_gate_narrative_is_rejected(self) -> None:
        commented = self._comment_out_paragraph(load_document_text(), "As of revision")
        self.assertIn(
            "The remaining required gaps are 1",
         validator.normalize_whitespace(commented),
            "the sentence must survive for this to test rendering, not deletion",
        )
        self.assert_validation_fails(
            lambda: validator.validate_counts(commented.splitlines(), commented),
            "wrapped in markup that keeps it out of the rendered document",
        )

    @staticmethod
    def _wrap_paragraph_in_raw_html(text: str, phrase: str, tag: str) -> str:
        """Wrap the paragraph holding `phrase` in a raw HTML `tag` block."""
        lines = text.splitlines()
        matches = [i for i, line in enumerate(lines) if phrase in line]
        if len(matches) != 1:
            raise AssertionError(f"expected one line holding {phrase!r}, found {len(matches)}")
        start = end = matches[0]
        while start > 0 and lines[start - 1].strip():
            start -= 1
        while end + 1 < len(lines) and lines[end + 1].strip():
            end += 1
        wrapped = (
            lines[:start]
            + [f"<{tag}>"]
            + lines[start : end + 1]
            + [f"</{tag}>"]
            + lines[end + 1 :]
        )
        return "\n".join(wrapped) + "\n"

    def test_hidden_markup_scan_reports_raw_html_code_blocks(self) -> None:
        for tag in ("pre", "code", "PRE"):
            with self.subTest(tag=tag):
                block = [f"<{tag}>", "normative prose", f"</{tag}>"]
                self.assertEqual(validator.find_hidden_markup_lines(block), block)
        # A closed block does not leak into the prose that follows it, and a
        # self-closing tag opens nothing.
        self.assertEqual(
            validator.find_hidden_markup_lines(["<pre>", "a", "</pre>", "b"]),
            ["<pre>", "a", "</pre>"],
        )
        self.assertEqual(validator.find_hidden_markup_lines(["<code/>", "b"]), ["<code/>"])
        self.assertEqual(validator.find_hidden_markup_lines(["plain prose"]), [])

    def test_raw_html_wrapped_release_gate_narrative_is_rejected(self) -> None:
        wrapped = self._wrap_paragraph_in_raw_html(
            load_document_text(), "As of revision", "pre"
        )
        self.assertIn(
            "The remaining required gaps are 1",
         validator.normalize_whitespace(wrapped),
            "the sentence must survive for this to test rendering, not deletion",
        )
        self.assert_validation_fails(
            lambda: validator.validate_counts(wrapped.splitlines(), wrapped),
            "wrapped in markup that keeps it out of the rendered document",
        )

    @staticmethod
    def _indent_section_preamble(text: str, heading: str) -> str:
        """Indent a section's prose so Markdown renders it as a code block."""
        lines = text.splitlines()
        start = lines.index(heading)
        indented = False
        for index in range(start + 1, len(lines)):
            line = lines[index]
            if line.startswith("## ") or line.startswith("|"):
                break
            if line.strip():
                lines[index] = "    " + line
                indented = True
        if not indented:
            raise AssertionError(f"no preamble prose to indent under {heading!r}")
        return "\n".join(lines) + "\n"

    def test_section_14_preamble_indented_into_a_code_block_is_rejected(self) -> None:
        indented = self._indent_section_preamble(
            load_document_text(), validator.APPROVAL_BEHAVIOR_SECTION_HEADING
        )
        preamble = validator.extract_section_preamble(
            indented.splitlines(), validator.APPROVAL_BEHAVIOR_SECTION_HEADING
        )
        self.assertEqual(
            validator.normalize_whitespace("\n".join(preamble)),
            validator.APPROVAL_BEHAVIOR_PREAMBLE.format(
                revision=validator.CLUSTER_STATUS_APPROVAL_REVISION
            ),
            "the pin must still match for this to test rendering, not wording",
        )
        rows = validator.parse_rows(indented.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(indented.splitlines(), rows),
            "Markdown renders it as an indented code block",
        )

    def test_section_26_preamble_indented_into_a_code_block_is_rejected(self) -> None:
        indented = self._indent_section_preamble(
            load_document_text(), validator.DIVERGENCE_DECISION_SECTION_HEADING
        )
        preamble = validator.extract_section_preamble(
            indented.splitlines(), validator.DIVERGENCE_DECISION_SECTION_HEADING
        )
        self.assertEqual(
            validator.normalize_whitespace("\n".join(preamble)),
            validator.DIVERGENCE_DECISION_PREAMBLE,
            "the pin must still match for this to test rendering, not wording",
        )
        rows = validator.parse_rows(indented.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(indented.splitlines(), rows),
            "Markdown renders it as an indented code block",
        )

    def test_find_indented_code_lines_separates_prose_from_code(self) -> None:
        prose_with_a_list = [
            "",
            "A paragraph that introduces a list.",
            "",
            "1. A numbered clause that wraps onto",
            "   a continuation line aligned under it.",
            "",
            "A closing paragraph.",
        ]
        self.assertEqual(validator.find_indented_code_lines(prose_with_a_list), [])
        self.assertEqual(
            validator.find_indented_code_lines(["", "    indented prose"]),
            ["    indented prose"],
        )
        self.assertEqual(
            validator.find_indented_code_lines(["", "\tindented prose"]),
            ["\tindented prose"],
        )
        self.assertEqual(
            validator.find_indented_code_lines(["", "prose", "    lazy continuation"]),
            [],
            "an indented continuation of a paragraph still renders as prose",
        )
        self.assertEqual(validator.leading_indent_columns("\tx"), 4)
        self.assertEqual(validator.leading_indent_columns("  \tx"), 4)

    def test_release_gate_narrative_relocated_out_of_its_section_is_rejected(self) -> None:
        text = load_document_text()
        relocated = self._relocate_paragraph(text, "As of revision")
        self.assertIn(
            "The remaining required gaps are 1",
         validator.normalize_whitespace(relocated),
            "the sentence must survive somewhere for this to test relocation, not deletion",
        )
        self.assert_validation_fails(
            lambda: validator.validate_counts(relocated.splitlines(), relocated),
            "missing or stale status narrative in",
            validator.RELEASE_GATE_SECTION_HEADING,
        )

    def test_subsumed_decision_text_rejects_a_retained_id_contradiction(self) -> None:
        contradicted = replace_pattern_once(
            load_document_text(),
            re.escape("must never be resolved by marking SB-STAT-030 `Covered`."),
            "must never be resolved by marking SB-STAT-030 `Covered`. SB-DIV-016 does not "
            "subsume this decision; it remains blocking.",
        )
        self.assertIn("SB-DIV-016", contradicted)
        rows = validator.parse_rows(contradicted.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(contradicted.splitlines(), rows),
            "no longer matches the audited subsumption word for word",
        )

    def test_pinned_subsumed_decision_text_must_name_the_covering_decision(self) -> None:
        pinned = validator.SUBSUMED_DIVERGENCES["SB-DIV-013"]
        narrowed = pinned._replace(
            impact_cell=pinned.impact_cell.replace("SB-DIV-016", "SB-DIV-999")
        )
        self.assertNotIn("SB-DIV-016", narrowed.impact_cell)
        text = load_document_text()
        rows = validator.parse_rows(text.splitlines())[2]
        with mock.patch.dict(
            validator.SUBSUMED_DIVERGENCES, {"SB-DIV-013": narrowed}, clear=False
        ):
            self.assert_validation_fails(
                lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
                "pinned decision text no longer names SB-DIV-016",
            )

    @staticmethod
    def _relocate_paragraph(text: str, phrase: str) -> str:
        """Move the paragraph holding `phrase` to the end of the document."""
        lines = text.splitlines()
        matches = [i for i, line in enumerate(lines) if phrase in line]
        if len(matches) != 1:
            raise AssertionError(f"expected one line holding {phrase!r}, found {len(matches)}")
        start = end = matches[0]
        while start > 0 and lines[start - 1].strip():
            start -= 1
        while end + 1 < len(lines) and lines[end + 1].strip():
            end += 1
        paragraph = lines[start : end + 1]
        remaining = lines[:start] + lines[end + 1 :]
        return "\n".join(remaining + [""] + paragraph) + "\n"

    def test_decision_preamble_prose_must_stay_above_the_table_it_introduces(self) -> None:
        text = load_document_text()
        relocated = self._relocate_paragraph(text, "The table below lists")
        split_prose = validator.DIVERGENCE_DECISION_SPLIT_PROSE.format(
            total=19, approved=12, subsumed=1, proposed=6
        )
        self.assertIn(
            split_prose,
            validator.normalize_whitespace(relocated),
            "the sentence must survive somewhere for this to test relocation, not deletion",
        )
        self.assertNotIn(
            split_prose,
            validator.normalize_whitespace(
                "\n".join(
                    validator.extract_section_preamble(
                        relocated.splitlines(), validator.DIVERGENCE_DECISION_SECTION_HEADING
                    )
                )
            ),
        )
        rows = validator.parse_rows(relocated.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(relocated.splitlines(), rows),
            "§26 does not state the decision split it lists",
        )

    def test_reconciliation_prose_must_stay_above_the_table_it_introduces(self) -> None:
        text = load_document_text()
        sentence = (
            "**98 matrix rows currently carry the `Intentional divergence (approval\n"
            "required)` status: 98 are approved and 0 are not.**"
        )
        self.assertEqual(text.count(sentence), 1)
        relocated = text.replace(sentence, "", 1) + "\n" + " ".join(sentence.split()) + "\n"
        self.assertIn(
            validator.normalize_whitespace(sentence),
            validator.normalize_whitespace(relocated),
            "the sentence must survive somewhere for this to test relocation, not deletion",
        )
        rows = validator.parse_rows(relocated.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(relocated.splitlines(), rows),
            "§26 does not reconcile approved and unapproved divergences",
        )

    def test_missing_decision_section_preamble_is_rejected(self) -> None:
        heading = validator.DIVERGENCE_DECISION_SECTION_HEADING
        text = load_document_text()
        self.assertEqual(text.count(heading), 1)
        lines = text.splitlines()
        start = lines.index(heading)
        table = next(i for i in range(start + 1, len(lines)) if lines[i].startswith("|"))
        stripped = "\n".join(lines[: start + 1] + lines[table:]) + "\n"
        self.assertEqual(
            validator.extract_section_preamble(stripped.splitlines(), heading), []
        )
        rows = validator.parse_rows(stripped.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(stripped.splitlines(), rows),
            "the section that must introduce the divergence decisions is missing",
        )

    def test_section_preamble_extraction_stops_at_the_table(self) -> None:
        text = load_document_text()
        preamble = validator.extract_section_preamble(
            text.splitlines(), validator.DIVERGENCE_DECISION_SECTION_HEADING
        )
        self.assertTrue(preamble)
        self.assertFalse([line for line in preamble if line.startswith("|")])
        self.assertIn("The table below lists", "\n".join(preamble))
        self.assertNotIn("SB-DIV-016 |", "\n".join(preamble))
        self.assertEqual(validator.extract_section_preamble(text.splitlines(), "## nope"), [])

    def test_divergence_prose_must_reconcile_approved_and_unapproved_rows(self) -> None:
        text = load_document_text().replace(
            "status: 98 are approved and 0 are not.**",
            "status: 97 are approved and 1 is not.**",
        )
        rows = validator.parse_rows(text.splitlines())[2]
        self.assert_validation_fails(
            lambda: validator.validate_divergence_approvals(text.splitlines(), rows),
            "§26 does not reconcile approved and unapproved divergences",
        )

    def test_pinned_inventory_rejects_status_swap_between_rows_in_one_section(self) -> None:
        text = load_document_text()
        first = "| SB-PKG-002 |"
        second = "| SB-PKG-008 |"
        lines = text.splitlines()
        first_index = next(i for i, line in enumerate(lines) if line.startswith(first))
        second_index = next(i for i, line in enumerate(lines) if line.startswith(second))
        first_cells = lines[first_index].split("|")
        second_cells = lines[second_index].split("|")
        self.assertNotEqual(first_cells[-3].strip(), second_cells[-3].strip())
        first_cells[-3], second_cells[-3] = second_cells[-3], first_cells[-3]
        lines[first_index] = "|".join(first_cells)
        lines[second_index] = "|".join(second_cells)
        mutated = "\n".join(lines) + "\n"

        self.assert_validation_fails(
            lambda: validator.validate_counts(mutated.splitlines(), mutated),
            f"revision {validator.EXPECTED_REVISION} inventory rows changed",
            "reclassified [SB-PKG-002 pinned Covered found Missing C ABI, "
            "SB-PKG-008 pinned Missing C ABI found Covered]",
        )

    def test_validate_revision_inventory_rejects_same_prefix_row_id_substitution(self) -> None:
        rewritten_text = replace_pattern_once(
            load_document_text(),
            r"^\| SB-PKG-001 \|",
            "| SB-PKG-999 |",
        )
        self.assert_validation_fails(
            lambda: validator.validate_counts(rewritten_text.splitlines(), rewritten_text),
            f"revision {validator.EXPECTED_REVISION} inventory rows changed",
            "missing [SB-PKG-001 (Intentional divergence (approval required))]",
            "unexpected [SB-PKG-999 (Intentional divergence (approval required))]",
        )

    def test_validate_revision_inventory_rejects_row_deletion(self) -> None:
        rewritten_text = "\n".join(remove_line_starting_once(load_document_text().splitlines(), "| SB-PKG-001 |"))
        self.assert_validation_fails(
            lambda: validator.validate_counts(rewritten_text.splitlines(), rewritten_text),
            f"revision {validator.EXPECTED_REVISION} inventory rows changed",
            "missing [SB-PKG-001 (Intentional divergence (approval required))]",
        )

    def test_validate_revision_inventory_rejects_row_addition(self) -> None:
        original_line = (
            "| SB-PKG-001 | Distribution `sharkbite`, version `1.2.0.3` (`setup.py:34-35`) | — | — | — | Missing C ABI | "
            "No Python packaging exists in Shoal. `Makefile` has `build`, `capi`, `test`, `test-hdfs`, `vet`, `clean` only — no wheel/sdist target. |"
        )
        added_line = original_line.replace("| SB-PKG-001 |", "| SB-PKG-999 |", 1)
        rewritten_text = "\n".join(
            insert_after_line_starting_once(load_document_text().splitlines(), "| SB-PKG-001 |", added_line)
        )
        self.assert_validation_fails(
            lambda: validator.validate_counts(rewritten_text.splitlines(), rewritten_text),
            f"revision {validator.EXPECTED_REVISION} inventory rows changed",
            "unexpected [SB-PKG-999 (Missing C ABI)]",
        )

    def test_manifest_citation_left_on_the_previous_revision_is_rejected(self) -> None:
        """A revision bump renames the manifests; citations have to move with them."""
        stale = f"docs/sharkbite-compatibility-revision{validator.EXPECTED_REVISION - 1}-cabi-symbols.txt"
        rewritten_text = load_document_text().replace(
            f"docs/{validator.EXPECTED_C_ABI_SYMBOL_MANIFEST.name}",
            stale,
        )
        self.assertIn(stale, rewritten_text)
        self.assert_validation_fails(
            lambda: validator.validate_counts(rewritten_text.splitlines(), rewritten_text),
            f"the document cites the manifest {stale}",
            f"but revision {validator.EXPECTED_REVISION} ships "
            f"docs/{validator.EXPECTED_C_ABI_SYMBOL_MANIFEST.name}",
        )

    def test_manifest_citation_pointing_past_the_current_revision_is_rejected(self) -> None:
        ahead = f"docs/sharkbite-compatibility-revision{validator.EXPECTED_REVISION + 1}-cabi-symbols.txt"
        rewritten_text = load_document_text().replace(
            f"docs/{validator.EXPECTED_C_ABI_SYMBOL_MANIFEST.name}",
            ahead,
        )
        self.assert_validation_fails(
            lambda: validator.validate_counts(rewritten_text.splitlines(), rewritten_text),
            f"the document cites the manifest {ahead}",
        )

    def test_the_document_cites_the_manifest_this_revision_ships(self) -> None:
        text = load_document_text()
        cited = {
            match.group(0).removeprefix("docs/")
            for match in validator.MANIFEST_CITATION_RE.finditer(text)
        }
        self.assertTrue(cited, "the document cites no manifest at all")
        self.assertLessEqual(
            cited,
            {
                validator.EXPECTED_ROW_MANIFEST.name,
                validator.EXPECTED_C_ABI_SYMBOL_MANIFEST.name,
            },
        )
        for name in cited:
            self.assertTrue((DOCS_DIR / name).is_file(), f"cited manifest {name} does not exist")

    def test_manifest_filenames_track_the_pinned_revision(self) -> None:
        with mock.patch.object(validator, "EXPECTED_REVISION", validator.EXPECTED_REVISION + 1):
            self.assert_validation_fails(
                validator.validate_pinned_inventory_constants,
                "manifest filenames do not follow EXPECTED_REVISION",
            )

    def test_validate_targeted_symbol_anchors_rejects_cross_file_anchor_leakage(self) -> None:
        self.assert_validation_fails(
            lambda: validator.validate_targeted_symbol_anchors(
                load_fixture_lines("multi_file_stale.md"),
                targeted_paths=CROSS_FILE_FIXTURE_PATHS,
            ),
            "StaleTest",
            "fixture_b.go",
        )

    def test_validate_targeted_symbol_anchors_accepts_adjacent_multi_file_fixture(self) -> None:
        validator.validate_targeted_symbol_anchors(
            load_fixture_lines("multi_file_ok.md"),
            targeted_paths=CROSS_FILE_FIXTURE_PATHS,
        )

    def test_validate_targeted_symbol_anchors_accepts_exact_identifier_boundaries(self) -> None:
        validator.validate_targeted_symbol_anchors(
            load_fixture_lines("identifier_boundary_ok.md"),
            targeted_paths=ANCHOR_FIXTURE_PATHS,
        )

    def test_validate_targeted_symbol_anchors_rejects_identifier_substrings(self) -> None:
        self.assert_validation_fails(
            lambda: validator.validate_targeted_symbol_anchors(
                load_fixture_lines("identifier_boundary_stale.md"),
                targeted_paths=ANCHOR_FIXTURE_PATHS,
            ),
            "TestAlpha",
            "fixture_identifier_old.go",
        )

    def test_validate_targeted_symbol_anchors_accepts_multiline_signature_anchor(self) -> None:
        validator.validate_targeted_symbol_anchors(
            load_fixture_lines("signature_anchor_ok.md"),
            targeted_paths=ANCHOR_FIXTURE_PATHS,
        )

    def test_validate_targeted_symbol_anchors_accepts_full_multiline_signature_anchor(self) -> None:
        validator.validate_targeted_symbol_anchors(
            load_fixture_lines("signature_full_multiline_ok.md"),
            targeted_paths=ANCHOR_FIXTURE_PATHS,
        )

    def test_validate_targeted_symbol_anchors_rejects_partial_signature_with_stale_type(self) -> None:
        self.assert_validation_fails(
            lambda: validator.validate_targeted_symbol_anchors(
                load_fixture_lines("signature_anchor_partial_stale.md"),
                targeted_paths=ANCHOR_FIXTURE_PATHS,
            ),
            "fixture_signature(stale_type row, ...)",
            "fixture_signature.h",
        )

    def test_validate_targeted_symbol_anchors_rejects_stale_type_from_other_construct(self) -> None:
        self.assert_validation_fails(
            lambda: validator.validate_targeted_symbol_anchors(
                load_fixture_lines("signature_anchor_stale_elsewhere.md"),
                targeted_paths=ANCHOR_FIXTURE_PATHS,
            ),
            "fixture_signature(stale_type row, ...)",
            "fixture_signature_elsewhere.h",
        )

    def test_validate_targeted_symbol_anchors_accepts_go_signature_shorthand(self) -> None:
        validator.validate_targeted_symbol_anchors(
            load_fixture_lines("go_signature_anchor_ok.md"),
            targeted_paths=ANCHOR_FIXTURE_PATHS,
        )

    def test_validate_targeted_symbol_anchors_accepts_python_signature_shorthand(self) -> None:
        validator.validate_targeted_symbol_anchors(
            load_fixture_lines("python_signature_anchor_ok.md"),
            targeted_paths=ANCHOR_FIXTURE_PATHS,
        )

    def test_validate_targeted_symbol_anchors_accepts_main_void_shorthand(self) -> None:
        validator.validate_targeted_symbol_anchors(
            load_fixture_lines("main_anchor_ok.md"),
            targeted_paths=ANCHOR_FIXTURE_PATHS,
        )

    def test_validate_targeted_symbol_anchors_resets_after_non_target_file_citation(self) -> None:
        validator.validate_targeted_symbol_anchors(
            load_fixture_lines("non_target_separator.md"),
            targeted_paths=CROSS_FILE_FIXTURE_PATHS,
        )

    def test_validate_targeted_symbol_anchors_explicit_optional_path_allows_bare_citation(self) -> None:
        validator.validate_targeted_symbol_anchors(
            make_anchor_fixture_lines("`capi/include/shoal.h`"),
            targeted_paths={"capi/include/shoal.h"},
        )

    def test_validate_targeted_symbol_anchors_custom_target_path_requires_anchor(self) -> None:
        self.assert_validation_fails(
            lambda: validator.validate_targeted_symbol_anchors(
                make_anchor_fixture_lines("`docs/testdata/validate_sharkbite_matrix/fixture_a.go`"),
                targeted_paths={"docs/testdata/validate_sharkbite_matrix/fixture_a.go"},
            ),
            "SB-FIXTURE-401 cites docs/testdata/validate_sharkbite_matrix/fixture_a.go without an adjacent local symbol/test anchor",
        )

    def test_validate_targeted_symbol_anchors_custom_target_path_accepts_anchor(self) -> None:
        validator.validate_targeted_symbol_anchors(
            make_anchor_fixture_lines(
                "`TestAlpha` (`docs/testdata/validate_sharkbite_matrix/fixture_a.go`)"
            ),
            targeted_paths={"docs/testdata/validate_sharkbite_matrix/fixture_a.go"},
        )

    def test_scanner_symbol_rename_invalidates_matrix(self) -> None:
        contents = validator.load_targeted_contents(validator.ANCHOR_CHECKED_CITATIONS)
        scanner_path = "accumulo/scanner.go"
        scanner = contents[scanner_path]
        renamed = scanner.replace("func NewColumnFamily(", "func RenamedColumnFamily(", 1)
        self.assertNotEqual(renamed, scanner)
        contents[scanner_path] = renamed

        with mock.patch.object(validator, "load_targeted_contents", return_value=contents):
            self.assert_validation_fails(
                lambda: validator.validate_targeted_symbol_anchors(
                    load_document_text().splitlines()
                ),
                "NewColumnFamily",
                scanner_path,
            )

    def test_scanner_symbol_anchor_typo_invalidates_matrix(self) -> None:
        text = load_document_text()
        mutated = text.replace(
            "with `NewColumnFamily` / `NewColumn` (`accumulo/scanner.go:",
            "with `TypoColumnFamily` / `NewColumn` (`accumulo/scanner.go:",
            1,
        )
        self.assertNotEqual(mutated, text)
        self.assert_validation_fails(
            lambda: validator.validate_targeted_symbol_anchors(mutated.splitlines()),
            "SB-SCAN-005 cites accumulo/scanner.go without required targeted anchors: NewColumnFamily",
        )

    # ---- pinned audited inventory ------------------------------------------

    def test_pinned_inventory_constants_are_internally_consistent(self) -> None:
        validator.validate_pinned_inventory_constants()
        self.assertEqual(validator.EXPECTED_REVISION, 56)
        self.assertEqual(validator.EXPECTED_TOTAL_ROWS, 3203)
        self.assertEqual(validator.EXPECTED_REQUIRED_ROWS, 394)
        self.assertEqual(
            validator.EXPECTED_SCOPE_COUNTS,
            {
                "Covered": 292,
                "Approved divergence": 98,
                "Required gap": 4,
                "Optional": 2766,
                "Not required": 43,
            },
        )
        self.assertEqual(
            validator.EXPECTED_STATUS_COUNTS,
            {
                "Covered": 355,
                "Missing Go": 2201,
                "Missing C ABI": 75,
                "Behavior mismatch": 83,
                validator.INTENTIONAL_DIVERGENCE_STATUS: 98,
                validator.NOT_REQUIRED_STATUS: 391,
            },
        )
        self.assertEqual(validator.EXPECTED_C_ABI_DECLARED_EXPORTS, 323)
        self.assertEqual(validator.EXPECTED_C_ABI_REFERENCED_EXPORTS, 323)
        self.assertEqual(validator.EXPECTED_C_ABI_UNREFERENCED_EXPORTS, ())

    def test_collect_c_abi_symbol_inventory_matches_pinned_values(self) -> None:
        exports, referenced, unreferenced = validator.collect_c_abi_symbol_inventory()
        self.assertEqual(len(exports), validator.EXPECTED_C_ABI_DECLARED_EXPORTS)
        self.assertEqual(len(referenced), validator.EXPECTED_C_ABI_REFERENCED_EXPORTS)
        self.assertEqual(unreferenced, validator.EXPECTED_C_ABI_UNREFERENCED_EXPORTS)
        self.assertIn("shoal_versioned_properties_get", referenced)
        self.assertIn("shoal_connector_get_identity", referenced)
        self.assertIn("shoal_connector_identity_get", referenced)
        self.assertIn("shoal_range_create", referenced)
        self.assertIn("shoal_iterator_setting_get", referenced)
        self.assertIn("shoal_mutation_delete", referenced)
        self.assertIn("shoal_rfile_reader_open_many", referenced)
        self.assertIn("shoal_rfile_entry_result_get", referenced)
        self.assertIn("shoal_client_create", referenced)
        self.assertIn("shoal_client_create_batch_writer", referenced)
        self.assertIn("shoal_client_select_column", referenced)
        self.assertIn("shoal_client_scan_range_with_cancellation", referenced)
        self.assertIn("shoal_client_scan_ranges_with_cancellation", referenced)
        self.assertIn("shoal_scanner_stream_with_cancellation", referenced)
        self.assertIn("shoal_client_stream_ranges_with_cancellation", referenced)
        self.assertIn("shoal_scan_cursor_next", referenced)
        self.assertIn("shoal_scan_cursor_free", referenced)
        self.assertIn("shoal_column_visibility_create", referenced)
        self.assertIn("shoal_visibility_evaluator_evaluate_tree", referenced)
        self.assertIn("shoal_error_visibility_parse", referenced)
        self.assertIn("shoal_error_compatibility_code", referenced)
        self.assertIn("shoal_hdfs_client_mkdir", referenced)
        self.assertIn("shoal_hdfs_client_chown", referenced)
        self.assertIn("shoal_logging_set_callback", referenced)

    def test_scope_manifest_matches_normative_rules_and_counts(self) -> None:
        rows = validator.parse_rows(load_document_text().splitlines())[2]
        counts = validator.validate_scope_manifest(rows)
        self.assertEqual(dict(counts), validator.EXPECTED_SCOPE_COUNTS)
        entries = {entry[0]: entry for entry in validator.load_expected_scope()}
        for row_id in ("SB-XCUT-014", "SB-XCUT-019", "SB-PKG-008"):
            self.assertEqual(
                entries[row_id][1:3],
                ("Optional", "optional-deferred-macos-native"),
            )
        self.assertEqual(entries["SB-PKG-011"][1:3], ("Covered", "core-covered"))
        for row_id in ("SB-PKG-012", "SB-PKG-013", "SB-TORCH-001", "SB-PANDA-001"):
            self.assertEqual(entries[row_id][1], "Optional")

    def test_scope_manifest_rejects_unjustified_bulk_reclassification(self) -> None:
        entries = list(validator.load_expected_scope())
        index = next(i for i, entry in enumerate(entries) if entry[0] == "SB-PKG-011")
        row_id, _disposition, _rule, status = entries[index]
        entries[index] = (row_id, "Optional", "optional-deferred-macos-native", status)
        with mock.patch.object(validator, "load_expected_scope", return_value=tuple(entries)):
            self.assert_validation_fails(
                lambda: validator.validate_scope_manifest(
                    validator.parse_rows(load_document_text().splitlines())[2]
                ),
                "unjustified scope classification for SB-PKG-011",
                "change the normative rule set and its tests",
            )

    def test_scope_manifest_provenance_header_is_pinned(self) -> None:
        lines = validator.EXPECTED_SCOPE_MANIFEST.read_text(encoding="utf-8").splitlines()
        lines[2] = "# Sharkbite source: unreviewed."
        self.assert_validation_fails(
            lambda: validator.validate_scope_manifest_provenance(lines),
            "scope manifest provenance header does not match revision 56",
        )

    def test_collect_c_abi_free_function_inventory_matches_header(self) -> None:
        free_functions = validator.collect_c_abi_free_function_inventory()
        self.assertEqual(len(free_functions), 42)
        self.assertIn("shoal_owned_key_free", free_functions)
        self.assertIn("shoal_key_value_result_free", free_functions)
        self.assertIn("shoal_authorizations_free", free_functions)
        self.assertIn("shoal_column_visibility_free", free_functions)
        self.assertIn("shoal_visibility_node_free", free_functions)
        self.assertIn("shoal_node_expression_free", free_functions)
        self.assertIn("shoal_visibility_evaluator_free", free_functions)
        self.assertIn("shoal_accumulo_writer_free", free_functions)
        self.assertIn("shoal_versioned_properties_free", free_functions)
        self.assertIn("shoal_bytes_list_free", free_functions)
        self.assertIn("shoal_connector_identity_free", free_functions)
        self.assertIn("shoal_range_free", free_functions)
        self.assertIn("shoal_iterator_setting_free", free_functions)
        self.assertIn("shoal_rfile_writer_free", free_functions)
        self.assertIn("shoal_rfile_entry_result_free", free_functions)
        self.assertIn("shoal_client_free", free_functions)
        self.assertIn("shoal_scan_cursor_free", free_functions)
        self.assertIn("shoal_hdfs_client_free", free_functions)
        self.assertIn("shoal_hdfs_input_stream_free", free_functions)
        self.assertIn("shoal_hdfs_output_stream_free", free_functions)
        self.assertIn("shoal_hdfs_dir_list_free", free_functions)
        self.assertIn("shoal_hdfs_dir_entry_result_free", free_functions)

    def test_compiled_c_abi_reference_inventory_ignores_non_linking_mentions(self) -> None:
        references = validator.compiled_c_abi_reference_inventory(
            source_paths=(Path("docs/testdata/validate_sharkbite_matrix/cabi_symbol_fixture.c"),),
            include_paths=(Path("docs/testdata/validate_sharkbite_matrix"),),
            repo_root=DOCS_DIR.parent,
        )
        self.assertIsNotNone(references)
        assert references is not None
        self.assertIn("shoal_live_call", references)
        self.assertIn("shoal_live_address", references)
        self.assertNotIn("shoal_comment_only", references)
        self.assertNotIn("shoal_string_only", references)
        self.assertNotIn("shoal_disabled_only", references)

    def test_stale_typed_free_inventory_narrative_is_rejected(self) -> None:
        text = load_document_text()
        mutated = text.replace("42 typed free functions", "8 typed free functions", 1)
        self.assertNotEqual(mutated, text)
        self.assert_validation_fails(
            lambda: validator.validate_counts(mutated.splitlines(), mutated),
            "missing or stale typed free-function inventory for SB-XCUT-002",
        )

    def test_pinned_inventory_constants_reject_incoherent_edit(self) -> None:
        with mock.patch.object(validator, "EXPECTED_TOTAL_ROWS", 3202):
            self.assert_validation_fails(
                validator.validate_pinned_inventory_constants,
                "pinned per-status counts sum to 3203",
                "EXPECTED_TOTAL_ROWS is 3202",
            )

    def test_pinned_inventory_constants_reject_section_count_drift(self) -> None:
        drifted = {
            prefix: dict(counts) for prefix, counts in validator.EXPECTED_PREFIX_COUNTS.items()
        }
        drifted["SB-EMB"][validator.NOT_REQUIRED_STATUS] -= 1
        with mock.patch.object(validator, "EXPECTED_PREFIX_COUNTS", drifted):
            self.assert_validation_fails(
                validator.validate_pinned_inventory_constants,
                "pinned status counts for SB-EMB sum to 34",
                "EXPECTED_PREFIX_TOTALS pins 35",
            )

    def test_pinned_inventory_rejects_row_deletion_with_consistent_prose(self) -> None:
        mutated = delete_matrix_row_consistently(load_document_text(), "SB-EMB-035", "SB-EMB")
        message = self.assert_validation_fails(
            lambda: validator.validate_counts(mutated.splitlines(), mutated),
            f"revision {validator.EXPECTED_REVISION} inventory rows changed: missing [SB-EMB-035 (Not required (rationale required))]",
        )
        self.assertIn("must update EXPECTED_REVISION", message)
        self.assertIn("row manifest", message)

    def test_pinned_counts_reject_row_deletion_when_the_manifest_is_relaxed(self) -> None:
        mutated = delete_matrix_row_consistently(load_document_text(), "SB-EMB-035", "SB-EMB")
        manifest = tuple(
            entry
            for entry in validator.load_expected_rows()
            if entry[0] != "SB-EMB-035"
        )
        with mock.patch.object(
            validator, "load_expected_rows", lambda: manifest
        ):
            self.assert_validation_fails(
                lambda: validator.validate_counts(mutated.splitlines(), mutated),
                f"revision {validator.EXPECTED_REVISION} inventory expects 3203 rows, found 3202",
            )

    def test_row_deletion_with_consistent_prose_satisfies_only_internal_cross_checks(self) -> None:
        mutated = delete_matrix_row_consistently(load_document_text(), "SB-EMB-035", "SB-EMB")
        with mock.patch.multiple(
            validator,
            **pinned_constants_for_deleted_not_required_row("SB-EMB", "SB-EMB-035"),
        ):
            validator.validate_counts(mutated.splitlines(), mutated)

    def test_pinned_inventory_rejects_section_total_shift(self) -> None:
        status_counts, prefix_counts, row_ids = validator.parse_rows(
            load_document_text().splitlines()
        )
        shifted = {prefix: counts.copy() for prefix, counts in prefix_counts.items()}
        shifted["SB-EMB"]["Missing Go"] += 1
        shifted["SB-XCUT"]["Missing Go"] -= 1
        self.assert_validation_fails(
            lambda: validator.validate_revision_inventory(row_ids, status_counts, shifted),
            f"revision {validator.EXPECTED_REVISION} inventory expects 35 rows for SB-EMB, found 36",
        )

    def test_pinned_inventory_rejects_status_reclassification(self) -> None:
        status_counts, prefix_counts, row_ids = validator.parse_rows(
            load_document_text().splitlines()
        )
        reclassified = status_counts.copy()
        reclassified["Missing Go"] -= 1
        reclassified["Covered"] += 1
        self.assert_validation_fails(
            lambda: validator.validate_revision_inventory(
                row_ids, reclassified, prefix_counts
            ),
            f"revision {validator.EXPECTED_REVISION} inventory expects 355 rows for Covered, found 356",
        )

    def test_declared_count_edit_still_fails_internal_cross_check(self) -> None:
        text = load_document_text()
        mutated = replace_pattern_once(
            text, re.escape("| Missing Go | 2201 |"), "| Missing Go | 2200 |"
        )
        self.assert_validation_fails(
            lambda: validator.validate_counts(mutated.splitlines(), mutated),
            "status summary says 2200 rows for Missing Go, but parsed 2201",
        )

    def test_stale_c_abi_symbol_inventory_narrative_is_rejected(self) -> None:
        text = load_document_text()
        mutated = text.replace(
            "applied to 323 declared exports in `capi/include/shoal.h`",
            "applied to 44 declared exports in `capi/include/shoal.h`",
            1,
        )
        self.assertNotEqual(mutated, text)
        self.assert_validation_fails(
            lambda: validator.validate_counts(mutated.splitlines(), mutated),
            "missing or stale C ABI export-total narrative for SB-XCUT-013",
        )

    def test_revision_bump_requires_validator_constant_update(self) -> None:
        text = load_document_text()
        status_snippet = validator.EXPECTED_DOCUMENT_STATUS_SNIPPETS[1]
        mutated = text.replace(
            status_snippet,
            f"Revision {validator.EXPECTED_REVISION + 1} — adds the next audited scope",
        ).replace(
            f"As of revision {validator.EXPECTED_REVISION},",
            f"As of revision {validator.EXPECTED_REVISION + 1},",
        )
        self.assertNotEqual(mutated, text)
        self.assert_validation_fails(
            lambda: validator.validate_counts(mutated.splitlines(), mutated),
            f"document status is missing expected detail: {status_snippet}",
        )

    # ---- matrix table separators -------------------------------------------

    def test_matrix_row_without_final_delimiter_is_rejected(self) -> None:
        text = load_document_text()
        lines = text.splitlines()
        for index, line in enumerate(lines):
            if line.startswith("| SB-CXX-1056 "):
                self.assertTrue(line.endswith("|"))
                lines[index] = line[:-1]
                break
        else:
            self.fail("missing SB-CXX-1056 fixture row")
        self.assert_validation_fails(
            lambda: validator.parse_rows(lines),
            "malformed SB row SB-CXX-1056",
            "missing final table delimiter",
        )

    def test_parse_rows_accepts_matrix_table_with_separator(self) -> None:
        status_counts, prefix_counts, row_ids = validator.parse_rows(
            matrix_table(MATRIX_HEADER, MATRIX_SEPARATOR, MATRIX_ROW)
        )
        self.assertEqual(row_ids, (("SB-FIXTURE-101", "Covered"),))
        self.assertEqual(status_counts["Covered"], 1)
        self.assertEqual(prefix_counts["SB-FIXTURE"]["Covered"], 1)

    def test_parse_rows_requires_separator_after_matrix_header(self) -> None:
        self.assert_validation_fails(
            lambda: validator.parse_rows(matrix_table(MATRIX_HEADER, None, MATRIX_ROW)),
            "missing separator row after the matrix table header on line 3",
        )

    def test_parse_rows_requires_separator_when_matrix_header_ends_the_document(self) -> None:
        self.assert_validation_fails(
            lambda: validator.parse_rows([MATRIX_HEADER]),
            "missing separator row after the matrix table header on line 1",
        )

    def test_parse_rows_rejects_malformed_separator_after_matrix_header(self) -> None:
        self.assert_validation_fails(
            lambda: validator.parse_rows(
                matrix_table(
                    MATRIX_HEADER,
                    "| --- | --- | -- | --- | --- | --- | --- |",
                    MATRIX_ROW,
                )
            ),
            "malformed separator row after the matrix table header on line 3",
            "column 3: invalid separator cell '--'",
        )

    def test_parse_rows_rejects_wrong_width_separator_after_matrix_header(self) -> None:
        self.assert_validation_fails(
            lambda: validator.parse_rows(
                matrix_table(MATRIX_HEADER, "| --- | --- | --- |", MATRIX_ROW)
            ),
            "malformed separator row after the matrix table header on line 3",
            "expected 7 cells, found 3",
        )

    def test_parse_rows_rejects_separator_inside_matrix_table(self) -> None:
        self.assert_validation_fails(
            lambda: validator.parse_rows(
                matrix_table(MATRIX_HEADER, MATRIX_SEPARATOR, MATRIX_ROW, MATRIX_SEPARATOR)
            ),
            "unexpected separator row inside a matrix table on line 6",
        )

    def test_matrix_rows_are_scoped_to_their_own_table(self) -> None:
        _status_counts, _prefix_counts, row_ids = validator.parse_rows(
            [
                MATRIX_HEADER,
                MATRIX_SEPARATOR,
                MATRIX_ROW,
                "",
                "| Other | Table |",
                "| --- | --- |",
                "| value | value |",
            ]
        )
        self.assertEqual(row_ids, (("SB-FIXTURE-101", "Covered"),))

    def test_parse_markdown_table_accepts_alignment_separators(self) -> None:
        headers, rows = validator.parse_markdown_table(
            [
                "## Example heading",
                "",
                "| Field | Value | Extra |",
                "| :--- | ---: | :---: |",
                "| Alpha | Beta | Gamma |",
            ],
            "## Example heading",
        )
        self.assertEqual(headers, ["Field", "Value", "Extra"])
        self.assertEqual(rows, [["Alpha", "Beta", "Gamma"]])

    def test_parse_markdown_table_rejects_extra_separator_row(self) -> None:
        self.assert_validation_fails(
            lambda: validator.parse_markdown_table(
                [
                    "## Example heading",
                    "",
                    "| Field | Value |",
                    "| --- | --- |",
                    "| Alpha | Beta |",
                    "| --- | --- |",
                    "| Gamma | Delta |",
                ],
                "## Example heading",
            ),
            "unexpected extra separator row under ## Example heading on line 6",
        )


if __name__ == "__main__":
    unittest.main()
