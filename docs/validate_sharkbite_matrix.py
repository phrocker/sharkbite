from __future__ import annotations

from collections.abc import Iterator, Sequence
from collections import Counter, defaultdict
from functools import lru_cache
import html
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import sys
import tempfile
from typing import NamedTuple
import unicodedata


DOC_PATH = Path(__file__).with_name("sharkbite-compatibility.md")
EXPECTED_REVISION = 56
CLUSTER_STATUS_APPROVAL_REVISION = 40
# Update this manifest only when the independently audited inventory itself
# changes; review every added/removed or reclassified ID in code review.
EXPECTED_ROW_MANIFEST = DOC_PATH.with_name(
    f"sharkbite-compatibility-revision{EXPECTED_REVISION}-rows.txt"
)
EXPECTED_C_ABI_SYMBOL_MANIFEST = DOC_PATH.with_name(
    f"sharkbite-compatibility-revision{EXPECTED_REVISION}-cabi-symbols.txt"
)
EXPECTED_SCOPE_MANIFEST = DOC_PATH.with_name(
    f"sharkbite-compatibility-revision{EXPECTED_REVISION}-scope.tsv"
)

STATUSES = {
    "Covered",
    "Missing Go",
    "Missing C ABI",
    "Behavior mismatch",
    "Intentional divergence (approval required)",
    "Not required (rationale required)",
}

NOT_REQUIRED_STATUS = "Not required (rationale required)"
INTENTIONAL_DIVERGENCE_STATUS = "Intentional divergence (approval required)"
EXPECTED_TOTAL_ROWS = 3203
EXPECTED_REQUIRED_ROWS = 394

SCOPE_DISPOSITIONS = {
    "Covered",
    "Approved divergence",
    "Required gap",
    "Optional",
    "Not required",
}
EXPECTED_SCOPE_COUNTS = {
    "Covered": 292,
    "Approved divergence": 98,
    "Required gap": 4,
    "Optional": 2766,
    "Not required": 43,
}
OPTIONAL_SCOPE_PREFIXES = {
    "SB-CPP",
    "SB-CXX",
    "SB-EMB",
    "SB-PANDA",
    "SB-TORCH",
}
OPTIONAL_SCOPE_ROWS = {"SB-PKG-012", "SB-PKG-013"}
DEFERRED_MACOS_SCOPE_ROWS = {"SB-PKG-008", "SB-XCUT-014", "SB-XCUT-019"}
SCOPE_RULES = {
    "core-covered",
    "core-approved-divergence",
    "core-required-gap",
    "optional-legacy-cxx",
    "optional-python-extra",
    "optional-deferred-macos-native",
    "not-required-rationale",
}


def status_count_map(
    *,
    covered: int = 0,
    missing_go: int = 0,
    missing_c_abi: int = 0,
    behavior_mismatch: int = 0,
    intentional_divergence: int = 0,
    not_required: int = 0,
) -> dict[str, int]:
    return {
        "Covered": covered,
        "Missing Go": missing_go,
        "Missing C ABI": missing_c_abi,
        "Behavior mismatch": behavior_mismatch,
        "Intentional divergence (approval required)": intentional_divergence,
        "Not required (rationale required)": not_required,
    }


EXPECTED_STATUS_COUNTS = {
    "Covered": 355,
    "Missing Go": 2201,
    "Missing C ABI": 75,
    "Behavior mismatch": 83,
    "Intentional divergence (approval required)": 98,
    "Not required (rationale required)": 391,
}

EXPECTED_PREFIX_COUNTS = {
    "SB-BASE": status_count_map(covered=18, not_required=2),
    "SB-CFG": status_count_map(
        covered=28,
        intentional_divergence=2,
        not_required=6,
    ),
    "SB-CONN": status_count_map(
        covered=12,
        intentional_divergence=1,
    ),
    "SB-CPP": status_count_map(
        missing_go=11,
        missing_c_abi=1,
        behavior_mismatch=50,
        not_required=8,
    ),
    "SB-CXX": status_count_map(
        missing_go=2189,
        covered=63,
        behavior_mismatch=29,
        not_required=304,
        missing_c_abi=41,
    ),
    "SB-DATA": status_count_map(covered=69, not_required=6),
    "SB-EMB": status_count_map(not_required=35),
    "SB-ERR": status_count_map(
        covered=12,
        intentional_divergence=1,
        not_required=3,
    ),
    "SB-HDFS": status_count_map(covered=26),
    "SB-LOG": status_count_map(covered=3),
    "SB-NS": status_count_map(covered=8),
    "SB-PANDA": status_count_map(missing_c_abi=20, not_required=1),
    "SB-PKG": status_count_map(
        covered=6,
        missing_c_abi=3,
        intentional_divergence=3,
        not_required=2,
    ),
    "SB-RFILE": status_count_map(covered=33, not_required=3),
    "SB-SCAN": status_count_map(
        covered=16,
        behavior_mismatch=2,
        intentional_divergence=6,
        not_required=4,
    ),
    "SB-SEC": status_count_map(covered=18, not_required=1),
    "SB-STAT": status_count_map(
        covered=1,
        intentional_divergence=82,
        not_required=1,
    ),
    "SB-TABLE": status_count_map(
        covered=14,
        intentional_divergence=2,
        not_required=6,
    ),
    "SB-TORCH": status_count_map(missing_c_abi=9),
    "SB-WRITE": status_count_map(
        covered=13,
        intentional_divergence=1,
        not_required=9,
    ),
    "SB-XCUT": status_count_map(
        covered=15,
        missing_go=1,
        missing_c_abi=1,
        behavior_mismatch=2,
    ),
}

EXPECTED_PREFIX_TOTALS = {
    "SB-BASE": 20,
    "SB-CFG": 36,
    "SB-CONN": 13,
    "SB-CPP": 70,
    "SB-CXX": 2626,
    "SB-DATA": 75,
    "SB-EMB": 35,
    "SB-ERR": 16,
    "SB-HDFS": 26,
    "SB-LOG": 3,
    "SB-NS": 8,
    "SB-PANDA": 21,
    "SB-PKG": 14,
    "SB-RFILE": 36,
    "SB-SCAN": 28,
    "SB-SEC": 19,
    "SB-STAT": 84,
    "SB-TABLE": 22,
    "SB-TORCH": 9,
    "SB-WRITE": 23,
    "SB-XCUT": 19,
}

EXPECTED_METADATA_FIELDS = {
    "Tracking issue": (
        'Shoal [#81](https://github.com/phrocker/shoal-oss/issues/81) — "docs: define and audit '
        'complete Sharkbite compatibility matrix" (parent '
        "[#59](https://github.com/phrocker/shoal-oss/issues/59); upstream target "
        "[phrocker/sharkbite#108](https://github.com/phrocker/sharkbite/issues/108))"
    ),
    "Sharkbite reference": (
        "`phrocker/sharkbite` @ `7f2625f74331b0cd4a75dc0484949c40f1409686` "
        "(\"Bump accumulo-core from 2.0.0 to 2.0.1 in /native-iterators-jni (#100)\", 2022-07-22)"
    ),
    "Sharkbite release line": "`sharkbite` 1.2.0.3 on PyPI (`setup.py:34-35`)",
    "Shoal reference": (
        "`phrocker/shoal-oss` revision 56 is pinned to latest-main merge "
        "`751c11db27d6258b391f85d5bb29ebdeaab8ac77` immediately before matrix serialization"
    ),
    "Shoal C ABI version": "`SHOAL_ABI_VERSION 1u` (`capi/include/shoal_types.h`)",
}

EXPECTED_DOCUMENT_STATUS_SNIPPETS = (
    "Normative gate. Binding on all Sharkbite-compatibility work.",
    "Revision 56 — closes SB-PKG-011 with merged, main-branch Linux manylinux build",
    "Revision 55 — records @phrocker's 2026-08-20 native macOS deferral",
    "Revision 54 — closes the exact 28 configuration, credential, connector/session",
    "Revision 53 — completes the Python-visible BatchScanner/Results and BatchWriter compatibility slice",
    "Revision 52 — closes the eleven required storage, logging, and error-contract gaps",
    "Revision 51 — closes the code/evidence-ready issue-190 packaging rows",
    "Revision 50 — closes the 60 required Python data-model and iterator-descriptor rows",
    "Revision 49 — closes the twelve issue-196 cross-cutting ownership",
    "Revision 48 — completes the 40 required table, namespace, and security rows",
    "Revision 47 — establishes the client-scope ADR",
    "Revision 46 — adds deterministic cross-platform artifact evidence",
    "Revision 45 — completes the practical final client-parity slice",
    "Revision 44 — publishes the public HDFS client/stream contract",
    "Revision 42 — completes the 36-row owned mutable key ABI tranche",
    "Revision 41 — records the public column and entry value APIs",
    "Revision 40 — mirrors the first approved divergence into the "
    f"matrix: @phrocker approved [SB-DIV-016](#sec-26) on 2026-08-19",
    "Revision 39 — records the public tablet extent API",
    "Revision 38 — completes the 31-row column-visibility expression tranche",
    "Revision 37 — records the public key value API",
    "Revision 36 — completes the twelve-row streaming cursor tranche",
    "Revision 35 — records the public column-visibility surface",
    "Revision 34 — completes the four-row compatibility-error tranche",
    "Revision 32 — completes the five-row high-level scanner facade",
    "Revision 26 — completes the 17-row data-model value C ABI",
    "Revision 25 — completes the 31-row RFile and stream C ABI",
    "Revision 24 — completes the configuration and instance-topology C ABI",
    "Revision 23 — records the public data-model value types",
    "Revision 22 — reclassifies the thirty-one RFile and stream rows of [§15](#sec-15)",
    "Revision 21 — adds owned range and iterator-setting descriptor ABIs",
    "Revision 20 reclassifies the twelve client-configuration and instance-topology rows",
    "Revision 19 added the merged connector-identity C ABI",
    "Revision 18 applied the seventeenth independent audit",
    "Revision 9 applied the eighth audit",
)

# Every pin above describes one audited revision. Changing the inventory is
# therefore an explicit, reviewable edit of this file: the revision number, the
# totals, the per-status counts and the per-section counts must move together,
# in the same commit as the document change and the audit that justifies it.
INVENTORY_CHANGE_HINT = (
    "the audited inventory is pinned in docs/validate_sharkbite_matrix.py; an intentional "
    "inventory revision must update EXPECTED_REVISION, EXPECTED_TOTAL_ROWS, "
    "EXPECTED_REQUIRED_ROWS, EXPECTED_STATUS_COUNTS, EXPECTED_PREFIX_TOTALS, "
    "EXPECTED_PREFIX_COUNTS, EXPECTED_SCOPE_COUNTS, the row manifest "
    f"docs/{EXPECTED_ROW_MANIFEST.name} (row ids, order and pinned statuses) "
    f"and scope manifest docs/{EXPECTED_SCOPE_MANIFEST.name} in the same commit, together with the "
    "audit evidence that justifies the new inventory"
)

DIVERGENCE_TABLE_HEADING = "## 26. Divergences requiring explicit approval"
DIVERGENCE_TABLE_HEADERS = [
    "ID",
    "Divergence",
    "Rows",
    "Impact on existing Sharkbite programs",
    "Approver",
    "Date",
]
UNAPPROVED_DIVERGENCE_APPROVER = "_unapproved_"
UNAPPROVED_DIVERGENCE_DATE = "—"


class ApprovedDivergence(NamedTuple):
    """A §26 decision a named maintainer approved on a dated issue comment.

    `rows` and `rows_cell` record the scope the decision covers. Both are pinned
    from the tracking issue rather than derived from the matrix: deriving the
    approved set from the same status column the approval is supposed to
    constrain would let any status edit redefine what was approved, and leaving
    the normative Rows cell unpinned would let it claim a different set while
    every other check still passed.

    `impact_cell` and `behavior_clauses` pin what the decision approved, in the
    decision table's own words. The behavior checks elsewhere are scoped to the
    section the decision governs, so without them the table that records the
    approval could drop or contradict every clause and still pass with an intact
    evidence link. The cell is compared whole for the same reason the Rows cell
    is: a clause kept as historical text and contradicted by an appended
    sentence satisfies every containment test while reversing the decision.
    """

    approver: str
    date: str
    evidence: str
    rows: tuple[str, ...]
    rows_cell: str
    impact_cell: str
    behavior_clauses: tuple[str, ...]


class SubsumedDivergence(NamedTuple):
    """A §26 entry an approved decision made moot rather than decided.

    `covering` is the approval that subsumes it. `rows` and `rows_cell` pin the
    scope the same way an approval's are pinned, because dormancy is granted to
    a specific concern: repointing the cell at an unrelated row would extend an
    approval's effect to something the maintainer never saw. `impact_cell` pins
    the subsumption rationale whole for the same reason the approved decision's
    is pinned whole: naming the covering decision and then denying that it
    subsumes this entry passes every containment check.
    """

    covering: str
    rows_cell: str
    rows: tuple[str, ...]
    impact_cell: str


# The 82 rows the #81 decision names, written out rather than generated: this is
# the audited scope of the approval, so it must be reviewable as a list and must
# not move when the matrix moves. SB-STAT-028 and SB-STAT-038 are deliberately
# absent — see EXPECTED_DIVERGENCE_EXCLUDED_ROWS.
CLUSTER_STATUS_APPROVED_ROWS: tuple[str, ...] = (
    "SB-STAT-001", "SB-STAT-002", "SB-STAT-003", "SB-STAT-004", "SB-STAT-005",
    "SB-STAT-006", "SB-STAT-007", "SB-STAT-008", "SB-STAT-009", "SB-STAT-010",
    "SB-STAT-011", "SB-STAT-012", "SB-STAT-013", "SB-STAT-014", "SB-STAT-015",
    "SB-STAT-016", "SB-STAT-017", "SB-STAT-018", "SB-STAT-019", "SB-STAT-020",
    "SB-STAT-021", "SB-STAT-022", "SB-STAT-023", "SB-STAT-024", "SB-STAT-025",
    "SB-STAT-026", "SB-STAT-027", "SB-STAT-029", "SB-STAT-030", "SB-STAT-031",
    "SB-STAT-032", "SB-STAT-033", "SB-STAT-034", "SB-STAT-035", "SB-STAT-036",
    "SB-STAT-037", "SB-STAT-039", "SB-STAT-040", "SB-STAT-041", "SB-STAT-042",
    "SB-STAT-043", "SB-STAT-044", "SB-STAT-045", "SB-STAT-046", "SB-STAT-047",
    "SB-STAT-048", "SB-STAT-049", "SB-STAT-050", "SB-STAT-051", "SB-STAT-052",
    "SB-STAT-053", "SB-STAT-054", "SB-STAT-055", "SB-STAT-056", "SB-STAT-057",
    "SB-STAT-058", "SB-STAT-059", "SB-STAT-060", "SB-STAT-061", "SB-STAT-062",
    "SB-STAT-063", "SB-STAT-064", "SB-STAT-065", "SB-STAT-066", "SB-STAT-067",
    "SB-STAT-068", "SB-STAT-069", "SB-STAT-070", "SB-STAT-071", "SB-STAT-072",
    "SB-STAT-073", "SB-STAT-074", "SB-STAT-075", "SB-STAT-076", "SB-STAT-077",
    "SB-STAT-078", "SB-STAT-079", "SB-STAT-080", "SB-STAT-081", "SB-STAT-082",
    "SB-STAT-083", "SB-STAT-084",
)

# The exact §26 Rows cell for the approved decision. It is the normative English
# statement of the same scope the tuple above pins mechanically; both are checked
# so neither can drift without the other.
CLUSTER_STATUS_APPROVED_ROWS_CELL = (
    "The 82 approved rows are the capability rows of [§14](#sec-14) — every "
    "`SB-STAT-*` row except SB-STAT-028 (a binding defect) and SB-STAT-038 (a "
    "Python attribute property). The decision also governs "
    "[SB-CONN-007](#sec-7), the `getStatistics()` entry point, and closes the "
    "ledger entries SB-GAP-GO-003 and SB-GAP-C-006; SB-CONN-007 is **not** one "
    "of the approved rows and keeps `Missing Go`, because the approval covers "
    "the 82 rows above and the raising shim it owes is buildable Python work"
)

# An approval is real only when a named approver, a date, and the issue comment
# that carries the decision are all mirrored into §26 in the same change that
# records the decision against the rows it covers. Those rows may already carry
# the divergence status, in which case the mirror lifts their gate without
# reclassifying anything. Pinning approver, date and evidence here means an edit
# to §26 alone cannot invent an approval, and a comment on the tracking issue
# alone cannot lift the §2.2 gate. Approval never makes a row `Covered`: it
# records a capability that will never exist.
# The Impact cell of the approved decision is the normative record of what the
# maintainer approved, so it is pinned whole rather than by keyword: a clause
# left in place as historical text and reversed by an appended sentence passes
# every containment check while inverting the decision.
CLUSTER_STATUS_APPROVED_IMPACT_CELL = (
    "Accumulo commit `f0841e4` removed `getManagerStats`, `ManagerMonitorInfo`, "
    "`DeadServer`, and the legacy monitor resources 113 commits before the pinned target "
    "`317c288`, and the surviving REST-v2/metrics/server APIs cannot reconstruct the "
    "object graph. Shoal issue [#96](https://github.com/phrocker/shoal-oss/issues/96) "
    "attempted it; PR [#98](https://github.com/phrocker/shoal-oss/pull/98) was withdrawn. "
    "Every `getStatistics()` consumer — `examples/pythonstats.py` is the published one — "
    "stops working, and there is no implementation that can change that. **Approved "
    "behavior**, mirrored verbatim in scope from the [#81 "
    "decision](https://github.com/phrocker/shoal-oss/issues/81#issuecomment-5343583850): "
    "`connector.getStatistics()` raises `NotImplementedError` with a stable explanation "
    "that Accumulo 4 removed the legacy manager-monitor API; it must not return a "
    "fabricated or partially populated status object; the permanent capability absence "
    "must not be surfaced as a retryable `ClientException`; and capability discovery must "
    "not advertise cluster-status support. This is the largest capability loss in the "
    "document after [SB-DIV-001](#sec-26). It creates two obligations rather than closing "
    "the work silently: [SB-GAP-T-009](#sec-23) and [SB-GAP-P-006](#sec-23)."
)

APPROVED_DIVERGENCES: dict[str, ApprovedDivergence] = {
    "SB-DIV-001": ApprovedDivergence(
        approver="**@phrocker**",
        date="**2026-08-20**",
        evidence="https://github.com/phrocker/shoal-oss/issues/81#issuecomment-5351942830",
        rows=("SB-PKG-014",),
        rows_cell="SB-PKG-014",
        impact_cell=(
            "Every user on Accumulo 1.x/2.x cannot upgrade. The Python constructor "
            "raises stable `NotImplementedError` before native bootstrap. "
            "[Approval evidence](https://github.com/phrocker/shoal-oss/issues/81#issuecomment-5351942830)."
        ),
        behavior_clauses=(),
    ),
    "SB-DIV-002": ApprovedDivergence(
        approver="**@phrocker**",
        date="**2026-08-20**",
        evidence="https://github.com/phrocker/shoal-oss/issues/81#issuecomment-5351942830",
        rows=("SB-CFG-014", "SB-CFG-022"),
        rows_cell="SB-CFG-014, SB-CFG-022",
        impact_cell=(
            "Code that re-reads its own password breaks; credential leaks stop. Covers "
            "**both** published spellings — the getter and its alias — which are two "
            "matrix rows carrying one decision. No compatibility object stores a "
            "retrievable password. [Approval evidence]"
            "(https://github.com/phrocker/shoal-oss/issues/81#issuecomment-5351942830)."
        ),
        behavior_clauses=(),
    ),
    "SB-DIV-003": ApprovedDivergence(
        approver="**@phrocker**",
        date="**2026-08-20**",
        evidence="https://github.com/phrocker/shoal-oss/issues/81#issuecomment-5351942830",
        rows=("SB-CONN-010",),
        rows_cell="SB-CONN-010",
        impact_cell=(
            "Connection counts and failure blast radius change. Python connector "
            "copy/deepcopy is rejected so handles cannot accidentally imply Sharkbite's "
            "sharing scope. [Approval evidence]"
            "(https://github.com/phrocker/shoal-oss/issues/81#issuecomment-5351942830)."
        ),
        behavior_clauses=(),
    ),
    "SB-DIV-004": ApprovedDivergence(
        approver="**@phrocker**",
        date="**2026-08-20**",
        evidence="https://github.com/phrocker/shoal-oss/issues/81#issuecomment-5351942830",
        rows=("SB-ERR-003",),
        rows_cell="SB-ERR-003",
        impact_cell=(
            "`except TApplicationException` blocks stop matching. Stable "
            "Shoal/`ClientException` mappings replace generated transport types. "
            "[Approval evidence](https://github.com/phrocker/shoal-oss/issues/81#issuecomment-5351942830)."
        ),
        behavior_clauses=(),
    ),
    "SB-DIV-005": ApprovedDivergence(
        approver="**@phrocker**",
        date="**2026-08-20**",
        evidence="https://github.com/phrocker/shoal-oss/issues/81#issuecomment-5353726986",
        rows=("SB-WRITE-010",),
        rows_cell="SB-WRITE-010, SB-UNSAFE-018",
        impact_cell=(
            "Programs that ignored rejected writes will now see exceptions. Python "
            "preserves copied structured rejection details instead of reproducing "
            "Sharkbite's unsafe silent-discard behavior. [Approval evidence]"
            "(https://github.com/phrocker/shoal-oss/issues/81#issuecomment-5353726986)."
        ),
        behavior_clauses=(),
    ),
    "SB-DIV-007": ApprovedDivergence(
        approver="**@phrocker**",
        date="**2026-08-20**",
        evidence="https://github.com/phrocker/shoal-oss/issues/81#issuecomment-5351942830",
        rows=("SB-SCAN-007", "SB-SCAN-016"),
        rows_cell="SB-SCAN-007, SB-SCAN-016 and [§19.4](#sec-19-4)",
        impact_cell=(
            "Rescoped in revision 15: the pinned Sharkbite tree does not execute Python "
            "iterators either — the embedded module is unreferenced, the iterator type "
            "is undeclared, and the implementation is commented out. The descriptor "
            "remains import-compatible and attachment raises stable `NotImplementedError`. "
            "[Approval evidence](https://github.com/phrocker/shoal-oss/issues/81#issuecomment-5351942830)."
        ),
        behavior_clauses=(),
    ),
    "SB-DIV-008": ApprovedDivergence(
        approver="**@phrocker**",
        date="**2026-08-20**",
        evidence="https://github.com/phrocker/shoal-oss/issues/81#issuecomment-5351942830",
        rows=("SB-SCAN-008", "SB-SCAN-009", "SB-SCAN-014", "SB-SCAN-015"),
        rows_cell="SB-SCAN-008, SB-SCAN-009, SB-SCAN-014, SB-SCAN-015",
        impact_cell=(
            "Enum names and ordinals remain import-compatible; applying either option "
            "raises stable `NotImplementedError`, and explicit RFile access remains "
            "available through `RFileOperations`. [Approval evidence]"
            "(https://github.com/phrocker/shoal-oss/issues/81#issuecomment-5351942830)."
        ),
        behavior_clauses=(),
    ),
    "SB-DIV-009": ApprovedDivergence(
        approver="**@phrocker**",
        date="**2026-08-20**",
        evidence="https://github.com/phrocker/shoal-oss/issues/81#issuecomment-5353023450",
        rows=("SB-PKG-007",),
        rows_cell="SB-PKG-007",
        impact_cell=(
            "Users on Python 3.6–3.8 must remain on Sharkbite or upgrade Python; "
            "Shoal will not weaken its supported Python 3.9+ floor. [Approval evidence]"
            "(https://github.com/phrocker/shoal-oss/issues/81#issuecomment-5353023450)."
        ),
        behavior_clauses=(),
    ),
    "SB-DIV-016": ApprovedDivergence(
        approver="**@phrocker**",
        date="**2026-08-19**",
        evidence=(
            "https://github.com/phrocker/shoal-oss/issues/81"
            "#issuecomment-5343583850"
        ),
        rows=CLUSTER_STATUS_APPROVED_ROWS,
        rows_cell=CLUSTER_STATUS_APPROVED_ROWS_CELL,
        impact_cell=CLUSTER_STATUS_APPROVED_IMPACT_CELL,
        behavior_clauses=(
            "`connector.getStatistics()` raises `NotImplementedError` with a stable "
            "explanation that Accumulo 4 removed the legacy manager-monitor API",
            "it must not return a fabricated or partially populated status object",
            "the permanent capability absence must not be surfaced as a retryable "
            "`ClientException`",
            "capability discovery must not advertise cluster-status support",
        ),
    ),
    "SB-DIV-017": ApprovedDivergence(
        approver="**@phrocker**",
        date="**2026-08-20**",
        evidence="https://github.com/phrocker/shoal-oss/issues/81#issuecomment-5353023450",
        rows=("SB-PKG-001",),
        rows_cell="SB-PKG-001",
        impact_cell=(
            "Shoal publishes `shoal-sharkbite` with an independent semantic version, "
            "never occupies the reserved `sharkbite` distribution name, and preserves "
            "`sharkbite` plus `pysharkbite` as import contracts. Installers pinned to "
            "the old distribution/version must migrate their requirement. [Approval "
            "evidence](https://github.com/phrocker/shoal-oss/issues/81"
            "#issuecomment-5353023450)."
        ),
        behavior_clauses=(),
    ),
    "SB-DIV-018": ApprovedDivergence(
        approver="**@phrocker**",
        date="**2026-08-20**",
        evidence="https://github.com/phrocker/shoal-oss/issues/81#issuecomment-5352949964",
        rows=("SB-TABLE-006",),
        rows_cell="SB-TABLE-006",
        impact_cell=(
            "The exact `compact(startRow, endRow, wait)` Python call remains present and "
            "raises stable `NotImplementedError` naming SB-DIV-018. Shoal does not guess "
            "Accumulo 4's Java-encoded `CompactionConfig` FATE payload or route through "
            "the offline compactor. [Approval evidence]"
            "(https://github.com/phrocker/shoal-oss/issues/81#issuecomment-5352949964)."
        ),
        behavior_clauses=(),
    ),
    "SB-DIV-019": ApprovedDivergence(
        approver="**@phrocker**",
        date="**2026-08-20**",
        evidence="https://github.com/phrocker/shoal-oss/issues/81#issuecomment-5352949964",
        rows=("SB-TABLE-013",),
        rows_cell="SB-TABLE-013",
        impact_cell=(
            "Both `getattr(tableOps, \"import\")` and "
            "`import_directory(dir, fail_path, setTime=False)` remain present and raise "
            "stable `NotImplementedError` naming SB-DIV-019. Shoal does not claim its "
            "staged-load-map Bulk Import V2 implements Sharkbite's `fail_path` contract. "
            "[Approval evidence]"
            "(https://github.com/phrocker/shoal-oss/issues/81#issuecomment-5352949964)."
        ),
        behavior_clauses=(),
    ),
}

# Divergence entries left dormant because an approved decision removed the thing
# they described. A subsumed entry is not an approval and must never carry one.
# Its Rows cell is pinned for the same reason the approved decision's is: the
# approval subsumes one narrow concern, and an unpinned cell could be repointed
# at an unrelated row that would then inherit dormancy it was never granted.
# `rows` is the matrix rows the cell names; every one of them must fall inside
# the covering approval's own scope, so the subsumption cannot reach past it.
# The Impact cell of the subsumed decision records why the approval left it
# dormant. It is pinned whole rather than by identifier because the identifier
# can be retained inside a sentence that reverses the subsumption.
SUBSUMED_COMPACTION_IMPACT_CELL = (
    "Accumulo 4 dropped major-compaction counts specifically, on top of the whole-section "
    "removal in [SB-DIV-016](#sec-26). Sharkbite programs read "
    "`.majors.running`/`.majors.queued`. The decision to record was *how* the shim surfaces "
    "the absence: raise, return `None`, or return a zeroed `Compacting`. **Subsumed** by the "
    "approved [SB-DIV-016](#sec-26): no `TableCompactions` object is ever returned — the "
    "approval's own wording — so `.majors` is unreachable and there is no shape to choose. "
    "This entry is dormant rather than decided — it revives if that approval is ever reversed "
    "— and must never be resolved by marking SB-STAT-030 `Covered`."
)
SUBSUMED_DIVERGENCES: dict[str, SubsumedDivergence] = {
    "SB-DIV-013": SubsumedDivergence(
        covering="SB-DIV-016",
        rows_cell="SB-STAT-030, SB-GAP-GO-003",
        rows=("SB-STAT-030",),
        impact_cell=SUBSUMED_COMPACTION_IMPACT_CELL,
    ),
}
# The exact approver cell a subsumed entry must carry. Matching the whole cell,
# not a substring, is what stops `_subsumed by X; not separately approved;
# approved by @someone_` from passing as a non-approval.
SUBSUMED_DIVERGENCE_APPROVER = "_subsumed by [{covering}](#sec-26); not separately approved_"
SUBSUMED_DIVERGENCE_DATE = "{date} (subsumed)"
# §26's preamble classifies every entry in the table. A subsumed entry is
# neither approved nor blocking, so the preamble has to say so explicitly or it
# contradicts the table it introduces.
SUBSUMED_DIVERGENCE_GATE_PROSE = (
    "[{divergence}](#sec-26) is **subsumed** by [{covering}](#sec-26): it is dormant rather "
    "than blocking, carries no approver of its own, and revives only if that approval is "
    "reversed."
)

# The audited Rows cell of every §26 decision that is still proposed. Pinning
# the identifiers alone leaves the scope of a proposal editable: an audited
# proposal could be repointed at unrelated rows under the same ID, and every
# later check would still pass whenever the substituted rows happen not to
# intersect EXPECTED_UNAPPROVED_DIVERGENCE_ROWS. A proposal is a request for a
# decision about a specific scope, so the scope is pinned with the ID.
EXPECTED_PROPOSED_DIVERGENCE_ROW_CELLS = {
    "SB-DIV-006": (
        "SB-SCAN-024, SB-BASE-016, SB-BASE-019, SB-PANDA-009, SB-PANDA-012, "
        "SB-UNSAFE-008"
    ),
    "SB-DIV-010": "SB-DATA-034",
    "SB-DIV-011": "SB-TORCH-004, SB-UNSAFE-021",
    "SB-DIV-012": "SB-PANDA-001, SB-UNSAFE-024",
    "SB-DIV-014": "SB-PANDA-013, SB-PANDA-003, SB-UNSAFE-026",
    "SB-DIV-015": "SB-BASE-004, SB-BASE-005, SB-BASE-009, SB-UNSAFE-027",
}

# Rows carrying INTENTIONAL_DIVERGENCE_STATUS that no approval covers. Each one
# still blocks the §2.2 release gate exactly like an unimplemented row.
EXPECTED_UNAPPROVED_DIVERGENCE_ROWS: tuple[str, ...] = ()

# The rows the approved SB-DIV-016 decision covers: the 82 §14 cluster-status
# rows named by the approval, and nothing else.
EXPECTED_APPROVED_DIVERGENCE_ROW_COUNT = 98
# Decisions in the §26 table. Rows and decisions are different quantities: one
# decision can cover many rows, so these are pinned separately from the row
# count. The identifiers are pinned, not just how many there are: a count-only
# pin accepts any same-size set, so an audited decision could be deleted and
# replaced by an invented one without tripping a single check.
EXPECTED_DIVERGENCE_DECISION_IDS = (
    "SB-DIV-001",
    "SB-DIV-002",
    "SB-DIV-003",
    "SB-DIV-004",
    "SB-DIV-005",
    "SB-DIV-006",
    "SB-DIV-007",
    "SB-DIV-008",
    "SB-DIV-009",
    "SB-DIV-010",
    "SB-DIV-011",
    "SB-DIV-012",
    "SB-DIV-013",
    "SB-DIV-014",
    "SB-DIV-015",
    "SB-DIV-016",
    "SB-DIV-017",
    "SB-DIV-018",
    "SB-DIV-019",
)
EXPECTED_DIVERGENCE_DECISION_COUNT = len(EXPECTED_DIVERGENCE_DECISION_IDS)

# Rows the decision's own text names or neighbours but that stay outside the
# approved set, with the status each must keep. The approval covers 82 rows; a
# reclassification here would widen it past its evidence, so each status is
# pinned. SB-CONN-007 is the §7 entry point that reaches §14: the approval fixes
# what it must do, but it is not one of the 82 approved rows, and the raising
# shim it now owes is buildable Python work.
EXPECTED_DIVERGENCE_EXCLUDED_ROWS = {
    "SB-CONN-007": "Covered",
    "SB-STAT-028": NOT_REQUIRED_STATUS,
    "SB-STAT-038": "Covered",
}

# The normative behavior the approval fixes. Dropping any of these sentences
# would silently widen an approved divergence, so they are pinned like counts.
EXPECTED_APPROVAL_BEHAVIOR_SNIPPETS = (
    "**Approved — @phrocker, 2026-08-19.**",
    "`connector.getStatistics()` **raises `NotImplementedError`** with a stable "
    "explanation that Accumulo 4 removed the legacy manager-monitor API.",
    "It **must not** return a fabricated or partially populated status object.",
    "This permanent absence **must not** be surfaced as a retryable "
    "`ClientException`",
    "**Capability discovery must not advertise cluster-status support.**",
)

# §14 holds 84 rows and the approval covers 82 of them, so the section has to
# state that scope in the approval's own terms. Prose that lets the decision
# cover "the section" contradicts the two exclusions pinned above, so the
# sentence is generated from the pinned count and the pinned excluded rows
# rather than written out: widening it in the document, or dropping an
# exclusion from the pin, makes the two disagree.
# The same section must not promise permanence for rows the approval never
# reached. Two of the 84 rows carry other statuses, so prose that binds "every
# row below" to the divergence status is broader than the decision; this
# sentence is generated from the same pinned count and decision so that widening
# it in the document fails here.
APPROVAL_PERMANENCE_PROSE = (
    "**each of the {count} rows [{decision}](#sec-26) covers keeps `Intentional "
    "divergence (approval required)` permanently**"
)

APPROVAL_SCOPE_PROSE = (
    "The single approval decision governing them is [{decision}](#sec-26), and it covers "
    "the {count} capability rows of this section — neither [{first}](#sec-14) nor "
    "[{second}](#sec-14) is in it."
)

# Containment cannot detect a contradiction. Every required sentence can stay
# in place while a later one reverses it, so the two normative preambles are
# pinned whole and compared after normalization, exactly as the approved
# decision's Impact cell is. The fragment checks above and below are kept for
# their diagnostics, and each is also required to appear in the whole-text pin
# so a pin can never silently drop what it is supposed to guarantee.
APPROVAL_BEHAVIOR_PREAMBLE = 'Reached through `AccumuloConnector.getStatistics()` (`pysharkbite.cpp:268`) and demonstrated by `examples/pythonstats.py`. **All 82 capability rows in this section are `Intentional divergence (approval required)` because the upstream capability no longer exists.** Two rows are excluded and carry other statuses: [SB-STAT-028](#sec-14) (`Not required` — a pybind11 registration defect in the `TableRates` property table, not a capability) and [SB-STAT-038](#sec-14) (`Covered` — the Python status placeholder types preserve `dynamic_attr` through normal instance dictionaries). Accumulo commit `f0841e4` removed `getManagerStats`, `ManagerMonitorInfo`, and `DeadServer` together with the legacy monitor resources — 113 commits before the pinned Accumulo 4 target `317c288`. The surviving REST-v2, metrics, and server APIs cannot reconstruct the legacy status object graph. The status label keeps the words `approval required` because [§4.2](#sec-4) fixes the six status names; approval state lives in [§26](#sec-26). The entry point [SB-CONN-007](#sec-7) is not one of the 82 approved rows, but is now `Covered`: the buildable Python shim required by the decision has landed and is tested. That classification proves the raising entry point, not cluster-status support. The single approval decision governing them is [SB-DIV-016](#sec-26), and it covers the 82 capability rows of this section — neither [SB-STAT-028](#sec-14) nor [SB-STAT-038](#sec-14) is in it. **Approved — @phrocker, 2026-08-19.** The decision is recorded on Shoal issue [#81](https://github.com/phrocker/shoal-oss/issues/81#issuecomment-5343583850) and mirrored here by revision 40. The approved compatibility behavior is normative: 1. `connector.getStatistics()` **raises `NotImplementedError`** with a stable explanation that Accumulo 4 removed the legacy manager-monitor API. 2. It **must not** return a fabricated or partially populated status object. 3. This permanent absence **must not** be surfaced as a retryable `ClientException`. 4. **Capability discovery must not advertise cluster-status support.** Revision 54 proves these obligations with `test_connector_statistics_and_closed_lifecycle_are_stable` and `test_status_placeholders_preserve_dynamic_attributes` (`python/tests/test_foundation.py`). No status object is returned, the exception is stable and non-retryable, no cluster-status capability is advertised, and all eight importable placeholder types accept arbitrary user attributes. **each of the 82 rows [SB-DIV-016](#sec-26) covers keeps `Intentional divergence (approval required)` permanently**; only the separately buildable entry point and dynamic-attribute row moved to `Covered`.'

DIVERGENCE_DECISION_PREAMBLE = 'Twelve entries below are approved: [SB-DIV-001](#sec-26) through [SB-DIV-005](#sec-26), [SB-DIV-007](#sec-26) through [SB-DIV-009](#sec-26), [SB-DIV-016](#sec-26) through [SB-DIV-019](#sec-26), signed by @phrocker on **2026-08-20** or the earlier date shown in the table. [SB-DIV-013](#sec-26) is **subsumed** by [SB-DIV-016](#sec-26): it is dormant rather than blocking, carries no approver of its own, and revives only if that approval is reversed. Every remaining entry blocks the gate until a named approver signs it with a date. Approvals are recorded on Shoal issue [#81](https://github.com/phrocker/shoal-oss/issues/81) and then mirrored into this table in the same change; a comment on #81 alone does not lift the gate, and neither does an edit here without the corresponding #81 decision. Adding a divergence to this table is not approval. **98 matrix rows currently carry the `Intentional divergence (approval required)` status: 98 are approved and 0 are not.** The approved 82 are the cluster-status rows of [§14](#sec-14), all covered by the single decision [SB-DIV-016](#sec-26), which @phrocker approved on **2026-08-19**; that decision also governs [SB-CONN-007](#sec-7), the `getStatistics()` entry point, but does not cover it as an approved row, so it stays `Missing Go` and is counted there. The additional approved rows are SB-PKG-001 (independent distribution/version), SB-PKG-007 (Python 3.9+), SB-PKG-014 (Accumulo 4 only), SB-CFG-014 and SB-CFG-022 (no password read-back, both spellings), SB-CONN-010 (per-connector pools), SB-ERR-003 (no Thrift types), SB-SCAN-007/016 (stable rejection of non-functional Python iterators), and SB-SCAN-008/009/014/015 (no implicit hedged or RFile-only scans), SB-WRITE-010 (structured write failures rather than silent discard), plus SB-TABLE-006 (no compatible online-compaction contract) and SB-TABLE-013 (no compatible legacy bulk-import contract). The table below lists 19 decisions — 12 approved, 1 subsumed by that approval, and 6 still proposed — because one decision can cover many rows; the row count and the decision count are different quantities and must not be conflated. The remaining 6 entries — every decision except the approved [SB-DIV-001](#sec-26), [SB-DIV-002](#sec-26), [SB-DIV-003](#sec-26), [SB-DIV-004](#sec-26), [SB-DIV-005](#sec-26), [SB-DIV-007](#sec-26), [SB-DIV-008](#sec-26), [SB-DIV-009](#sec-26), [SB-DIV-016](#sec-26), [SB-DIV-017](#sec-26), [SB-DIV-018](#sec-26), [SB-DIV-019](#sec-26) and the subsumed [SB-DIV-013](#sec-26) — are **proposed** divergences. Every row named by those proposals currently carries a non-divergence gap status; approval would move it to `Intentional divergence`. Rejecting a proposal leaves its rows as gaps that must be implemented. Neither of the two non-proposed entries is a proposal: SB-DIV-016 is already approved, and SB-DIV-013 describes a capability that same approval removed outright, so it has nothing left to propose. SB-DIV-016 names [SB-CONN-007](#sec-7) as the entry point that reaches its rows, but that row is outside the approved set and is `Covered` because the required raising shim has landed; only the 82 unavailable capability rows retain divergence status. An approved divergence never becomes `Covered`: it records a capability that will never exist rather than one that has been delivered, and its rows keep the `approval required` label because [§4.2](#sec-4) fixes the six status names — approval state lives in this table, not in the status column.'
RELEASE_GATE_SECTION_HEADING = "## 2. Release gate (normative)"
COUNTS_SECTION_HEADING = "## 25. Counts by status and category"
APPROVAL_BEHAVIOR_SECTION_HEADING = (
    "## 14. Matrix: cluster status and monitoring (`SB-STAT`)"
)
APPROVAL_BEHAVIOR_SECTION_LABEL = "§14"
DIVERGENCE_DECISION_SECTION_HEADING = "## 26. Divergences requiring explicit approval"
DIVERGENCE_DECISION_SECTION_LABEL = "§26"
DIVERGENCE_DECISION_SPLIT_PROSE = (
    "The table below lists {total} decisions — {approved} approved, {subsumed} subsumed by "
    "that approval, and {proposed} still proposed —"
)
PROPOSED_DIVERGENCE_CARVE_OUT_PROSE = (
    "The remaining {proposed} entries — every decision except the approved {approved_ids} "
    "and the subsumed {subsumed_ids} — are **proposed** divergences"
)
PROPOSED_DIVERGENCE_EFFECT_PROSE = (
    "The {already} rows that already carry `{status}` — the unapproved rows listed above, "
    "named by {already_ids} — would simply have their gate lifted, with no status and no "
    "count change"
)

GAP_COMPLETION_RULES: dict[str, tuple[str, ...]] = {
    "SB-GAP-GO-001": ("Missing Go",),
    "SB-GAP-GO-002": ("Missing Go",),
    "SB-GAP-C-001": ("Missing Go", "Missing C ABI"),
    "SB-GAP-C-002": ("Missing Go", "Missing C ABI"),
    "SB-GAP-C-004": ("Missing Go", "Missing C ABI"),
    "SB-GAP-C-011": ("Missing Go", "Missing C ABI"),
    "SB-GAP-GO-011": ("Missing Go",),
    "SB-GAP-GO-012": ("Missing Go",),
    "SB-GAP-C-008": ("Missing Go", "Missing C ABI"),
    "SB-GAP-C-009": ("Missing Go", "Missing C ABI"),
}

EXPECTED_ROW_MANIFEST_HEADER = (
    f"# Revision-{EXPECTED_REVISION} accepted matrix rows for docs/sharkbite-compatibility.md.",
    "# One entry per row: ROW-ID followed by the audited status, in document order.",
    "# Pinning the status next to the id means a swap of two rows' statuses inside one",
    "# section cannot pass the aggregate count checks undetected.",
    f"# Update only when the independently audited revision-{EXPECTED_REVISION} inventory itself changes.",
    "# Regenerate from the audited document, review every added/removed/reclassified row",
    "# in code review, and keep the list in document order for human auditability.",
)
EXPECTED_SCOPE_MANIFEST_HEADER = (
    f"# Revision-{EXPECTED_REVISION} Sharkbite client-scope disposition manifest.",
    f"# Matrix: docs/sharkbite-compatibility.md revision {EXPECTED_REVISION}.",
    "# Sharkbite source: phrocker/sharkbite@7f2625f74331b0cd4a75dc0484949c40f1409686.",
    "# Shoal source: phrocker/shoal-oss@751c11db27d6258b391f85d5bb29ebdeaab8ac77.",
    "# Policy: docs/sharkbite-client-scope.md.",
    "# Columns: ROW-ID<TAB>DISPOSITION<TAB>RULE<TAB>PINNED-MATRIX-STATUS.",
    "# Entries are in matrix order; validator rules, not aggregate counts, authorize dispositions.",
)

C_ABI_EXPORT_HEADER_PATH = Path("capi/include/shoal.h")
C_ABI_REFERENCE_PATHS = (
    Path("capi/tests/all_symbols.c"),
    Path("capi/tests/lifecycle.c"),
    Path("capi/tests/shared_library_query.c"),
    Path("capi/tests/header_cpp_test.cpp"),
)
DEFAULT_C_ABI_INCLUDE_PATHS = (
    Path("capi/include"),
    Path("capi/tests"),
)
EXPECTED_C_ABI_DECLARED_EXPORTS = 323
EXPECTED_C_ABI_REFERENCED_EXPORTS = 323
EXPECTED_C_ABI_UNREFERENCED_EXPORTS: tuple[str, ...] = ()
EXPECTED_C_ABI_SYMBOL_MANIFEST_HEADER = (
    f"# Revision-{EXPECTED_REVISION} compiled C ABI symbol inventory for docs/sharkbite-compatibility.md.",
    "# Generated from the undefined/imported shoal_* references emitted by compiling the exact C/C++",
    "# sources that cmd/shoal-capi/cabi_test.go links in TestSharedLibraryCABI.",
    f"# Update only when the independently audited revision-{EXPECTED_REVISION} compiled inventory changes.",
)

CATEGORY_STATUS_COLUMNS = {
    "Covered": "Covered",
    "Missing Go": "Missing Go",
    "Missing C ABI": "Missing C ABI",
    "Behavior mismatch": "Behavior mismatch",
    "Intentional divergence": INTENTIONAL_DIVERGENCE_STATUS,
    "Not required": NOT_REQUIRED_STATUS,
}

TARGETED_LOCAL_CITATIONS = {
    "capi/include/shoal.h",
    "capi/include/shoal_types.h",
    "capi/tests/lifecycle.c",
    "capi/tests/result_bridge.c",
    "capi/tests/shared_library_query.c",
    "capi/tests/header_cpp_test.cpp",
    "cmd/shoal-capi/export.go",
    "cmd/shoal-capi/abi_export_test.go",
    "cmd/shoal-capi/connector_control_export_test.go",
    "cmd/shoal-capi/table_admin_export_test.go",
    "cmd/shoal-capi/cabi_test.go",
    "cmd/shoal-capi/state_test.go",
    "cmd/shoal-capi/writer_export_test.go",
}
# Implementation files behind the section 6 client-configuration and instance
# topology rows. Their citations are anchor-checked exactly like the C ABI
# ones so a rename or a moved declaration cannot leave the normative evidence
# pointing at the wrong symbol.
TARGETED_SB_CFG_CITATIONS = {
    "accumulo/config.go",
    "accumulo/configuration.go",
    "accumulo/configuration_test.go",
    "accumulo/instance.go",
    "accumulo/topology.go",
    "accumulo/topology_test.go",
    "internal/zk/locator.go",
    "internal/zk/manager.go",
    "internal/zk/topology_test.go",
}

OPTIONAL_ANCHOR_CITATIONS = {
    "capi/include/shoal.h",
    "capi/include/shoal_types.h",
}

# Implementation files behind the section 15 RFile rows. Anchor-checked for the
# same reason: the matrix claims an exact public Go surface, so a rename or a
# deleted method must fail the document, not just the build.
# Implementation files behind the section 11 table-maintenance rows.
TARGETED_SB_DM_CITATIONS = {
    "accumulo/column.go",
    "accumulo/key_value.go",
    "accumulo/scanner.go",
    "accumulo/data_model_test.go",
}

TARGETED_SYMBOL_ANCHORS_BY_ROW_CITATION = {
    ("SB-SCAN-005", "accumulo/scanner.go"): {"NewColumnFamily", "NewColumn"},
    ("SB-BASE-020", "accumulo/scanner.go"): {"NewColumnFamily", "NewColumn"},
    ("SB-CPP-070", "accumulo/scanner.go"): {"NewColumnFamily", "NewColumn"},
    ("SB-CXX-0208", "accumulo/scanner.go"): {"NewColumnFamily"},
    ("SB-CXX-0209", "accumulo/scanner.go"): {"NewColumn"},
    ("SB-CXX-0217", "accumulo/scanner.go"): {"Column.Family"},
    ("SB-CXX-0219", "accumulo/scanner.go"): {"Column.Qualifier"},
    ("SB-XCUT-003", "accumulo/scanner.go"): {"Column.Family"},
}

TARGETED_SB_EXTENT_CITATIONS = {
    "accumulo/tablet_extent.go",
    "accumulo/tablet_extent_test.go",
}

TARGETED_SB_KEY_CITATIONS = {
    "accumulo/key.go",
    "accumulo/key_test.go",
}

TARGETED_SB_VIS_CITATIONS = {
    "accumulo/column_visibility.go",
    "accumulo/column_visibility_test.go",
}

TARGETED_SB_SCAN_CITATIONS = {
    "accumulo/scan_stream.go",
    "accumulo/scan_stream_test.go",
}

TARGETED_SB_TABLE_CITATIONS = {
    "accumulo/table_constraints.go",
    "accumulo/table_constraints_test.go",
    "accumulo/table_flush.go",
}

# Implementation files behind the section 8 data-model value types.
TARGETED_SB_DATA_CITATIONS = {
    "accumulo/authorizations.go",
    "accumulo/key_string.go",
    "accumulo/range.go",
    "accumulo/value_types_test.go",
}

TARGETED_SB_RFILE_CITATIONS = {
    "rfile/close_internal_test.go",
    "rfile/entry.go",
    "rfile/multigroup_internal_test.go",
    "rfile/errors.go",
    "rfile/reader.go",
    "rfile/rfile_test.go",
    "rfile/seekable.go",
    "rfile/writer.go",
}

# Every path whose citations are anchor-checked: the C ABI surface plus the
# section 6 and section 15 implementation files.
ANCHOR_CHECKED_CITATIONS = (
    TARGETED_LOCAL_CITATIONS
    | TARGETED_SB_CFG_CITATIONS
    | TARGETED_SB_RFILE_CITATIONS
    | TARGETED_SB_DATA_CITATIONS
    | TARGETED_SB_TABLE_CITATIONS
    | TARGETED_SB_SCAN_CITATIONS
    | TARGETED_SB_VIS_CITATIONS
    | TARGETED_SB_KEY_CITATIONS
    | TARGETED_SB_EXTENT_CITATIONS
    | TARGETED_SB_DM_CITATIONS
)
COUNT_RE = re.compile(
    r"^(?P<bold>\*\*)?(?P<number>0|[1-9]\d*|[1-9]\d{0,2}(?:,\d{3})+)(?(bold)\*\*|)$"
)
CODE_SPAN_RE = re.compile(r"`([^`]+)`")
IDENT_RE = re.compile(r"[A-Za-z_][A-Za-z0-9_]*")
ROW_ID_RE = re.compile(r"\bSB-[A-Z]+-\d{3}\b")
ROW_ID_PARTS_RE = re.compile(r"(?P<prefix>SB-[A-Z]+)-(?P<number>\d{3})")
# Link targets, so an evidence URL can be pinned as the whole destination of a
# Markdown link. A substring test would accept a suffixed URL that resolves to a
# different comment, which is exactly what an approval citation must not allow.
# The whole `[label](target)` construct has to match: bare `](target)` text is not
# a link, so a pattern that only anchors on the closing bracket would report a
# citation the rendered document does not contain. Images are excluded, and the
# label is captured so it can be checked for visibility separately.
MARKDOWN_LINK_RE = re.compile(
    r"(?<![!\\])\[(?P<label>[^\[\]]*)(?<!\\)\]\((?P<target>[^()\s]+)\)"
)
# Unicode categories that occupy no visible space: control, format (which is
# where U+200B ZERO WIDTH SPACE and the bidi marks live), and the three
# separator categories.
# Categories whose members render nothing a reader can see or click. The
# combining marks (`Mn`, `Me`) are here because a label made only of U+FE0F or
# U+034F has no base character to attach to, so it renders as an empty anchor
# exactly as a zero-width space does.
INVISIBLE_LABEL_CATEGORIES = frozenset({"Cc", "Cf", "Mn", "Me", "Zl", "Zp", "Zs"})
# Raw HTML tags contribute markup, not text: `[<span></span>](url)` renders an
# anchor with nothing inside it. Tags are removed before entity decoding so
# that `&lt;span&gt;`, which reaches the reader as visible characters, survives.
MARKDOWN_HTML_TAG_RE = re.compile(r"</?[A-Za-z][^>]*>")

# A code span renders as literal text, so `[label](url)` inside one is not a
# link at all, and an HTML comment renders as nothing. Matching the raw cell
# would let the evidence citation be backquoted into prose or commented out
# while still satisfying the pin, so every construct the reader never sees as a
# link is removed before link targets are read. Escaped brackets are rejected by
# the pattern itself: `\[label](url)` renders as literal text too.
MARKDOWN_HTML_COMMENT_RE = re.compile(r"<!--.*?-->", re.S)
MARKDOWN_FENCE_OPENER_RE = re.compile(r"^ {0,3}(?:`{3,}|~{3,})")
MARKDOWN_CODE_FENCE_RE = re.compile(r"```.*?```", re.S)
MARKDOWN_CODE_SPAN_RE = re.compile(r"(?<!`)(`+)(?!`)(.+?)(?<!`)\1(?!`)", re.S)
# Raw HTML `<code>`/`<pre>` render their contents as literal text just like a
# backquoted span, so a citation written `<code>[label](target)</code>` links
# nowhere. Markdown allows raw HTML, so this is not a hypothetical form.
MARKDOWN_HTML_CODE_RE = re.compile(
    r"<(?P<tag>code|pre)\b[^>]*>.*?</(?P=tag)\s*>", re.S | re.I
)
# The same construct seen one line at a time, for block scans that cannot use
# the whole-document form above.
MARKDOWN_HTML_CODE_TAG_RE = re.compile(
    r"<(?P<closing>/?)(?P<tag>code|pre)\b[^>]*?(?P<selfclosing>/?)>", re.I
)


def strip_non_rendered_markup(text: str) -> str:
    """Return `text` with every construct that renders as non-link blanked out."""

    without_comments = MARKDOWN_HTML_COMMENT_RE.sub(" ", text)
    without_html_code = MARKDOWN_HTML_CODE_RE.sub(" ", without_comments)
    return MARKDOWN_CODE_SPAN_RE.sub(
        " ", MARKDOWN_CODE_FENCE_RE.sub(" ", without_html_code)
    )


def has_visible_link_label(label: str) -> bool:
    """Report whether a Markdown link label renders any character a reader can see.

    `[](target)`, `[\u200b](target)` and `[<span></span>](target)` all render as
    anchors with nothing to read or click, so a citation written any of those
    ways is not one a reviewer can follow. Raw tags are dropped first because
    they are markup rather than text, and entities are decoded afterwards
    because `&nbsp;` reaches the reader as U+00A0, not as six visible
    characters, while `&lt;span&gt;` reaches it as visible text.
    """

    for character in html.unescape(MARKDOWN_HTML_TAG_RE.sub("", label)):
        if character.isspace():
            continue
        if unicodedata.category(character) in INVISIBLE_LABEL_CATEGORIES:
            continue
        return True
    return False


def extract_markdown_link_targets(text: str) -> set[str]:
    """Return the targets of the links `text` actually renders as links."""

    return {
        match.group("target")
        for match in MARKDOWN_LINK_RE.finditer(strip_non_rendered_markup(text))
        if has_visible_link_label(match.group("label"))
    }
# "every `SB-STAT-*` row except SB-STAT-028 (...) and SB-STAT-038 (...)." — the
# clause an approved Rows cell uses to state its scope in English.
APPROVAL_ROWS_CELL_SCOPE_RE = re.compile(
    r"every\s+`(?P<prefix>SB-[A-Z]+)-\*`\s+rows?\s+except\s+(?P<exclusions>.*?)\.\s",
    re.S,
)
MANIFEST_CITATION_RE = re.compile(
    r"docs/sharkbite-compatibility-revision(?P<revision>\d+)-(?P<kind>rows|cabi-symbols)\.txt"
)
ANCHOR_PART_RE = re.compile(r"[A-Za-z_][A-Za-z0-9_]*|\.{3}|\s+|.")
FILE_CITATION_SUFFIXES = {
    ".c",
    ".cc",
    ".cpp",
    ".go",
    ".h",
    ".hpp",
    ".java",
    ".json",
    ".md",
    ".proto",
    ".py",
    ".sh",
    ".txt",
    ".xml",
    ".yaml",
    ".yml",
}
EXPORT_SYMBOL_RE = re.compile(
    r"SHOAL_API\s+[^;]*?\b(?:SHOAL_CALL\s+)?(?P<name>shoal_[A-Za-z0-9_]+)\s*\(",
    re.S,
)
FREE_SYMBOL_RE = re.compile(
    r"SHOAL_API\s+void\s+SHOAL_CALL\s+(?P<name>shoal_[A-Za-z0-9_]+_free)\s*\(",
    re.S,
)
FILE_CITATION_BASENAMES = {"ARCHITECTURE.md", "CMakeLists.txt", "Dockerfile", "Makefile", "README.md"}
IDENTIFIER_BOUNDARY_CLASS = r"A-Za-z0-9_"
IGNORED_ANCHOR_TOKENS = {
    "ABI",
    "C",
    "Go",
    "and",
    "by",
    "char",
    "const",
    "double",
    "enum",
    "extern",
    "float",
    "full",
    "in",
    "inline",
    "int",
    "interface",
    "long",
    "map",
    "on",
    "plus",
    "read",
    "short",
    "signed",
    "static",
    "struct",
    "typedef",
    "union",
    "under",
    "unsigned",
    "var",
    "void",
    "volatile",
    "with",
}
WHITESPACE_TOLERANT_PUNCTUATION = frozenset("(),*&[]")
DECLARATION_PATTERNS = (
    re.compile(
        r"(?ms)^[ \t]*(?:typedef\s+)?(?:struct|union|enum|class|interface)\b[\s\S]*?"
        r"^[ \t]*\}[ \t]*[A-Za-z_][A-Za-z0-9_]*[ \t]*;"
    ),
    re.compile(
        r"(?ms)^[ \t]*(?:struct|union|enum|class|interface)\s+[A-Za-z_][A-Za-z0-9_]*"
        r"\s*\{[\s\S]*?^[ \t]*\}[ \t]*;?"
    ),
    re.compile(
        r"(?ms)^[ \t]*type\s+[A-Za-z_][A-Za-z0-9_]*\s+(?:struct|interface)\s*\{"
        r"[\s\S]*?^[ \t]*\}[ \t]*"
    ),
    re.compile(
        r"(?ms)^[ \t]*(?:async\s+def|def)\s+[A-Za-z_][A-Za-z0-9_]*\s*\([^)]*?\)"
        r"(?:\s*->\s*[^:]+)?\s*:"
    ),
    re.compile(r"(?m)^[ \t]*class\s+[A-Za-z_][A-Za-z0-9_]*(?:\s*\([^)]*?\))?\s*:"),
    re.compile(
        r"(?ms)^[ \t]*func\s+(?:\([^)]+\)\s*)?[A-Za-z_][A-Za-z0-9_]*\s*\([^)]*?\)"
        r"(?:\s*\([^)]*?\)|\s+[^{\n]+)?\s*\{"
    ),
    re.compile(
        r"(?ms)^[ \t]*(?:[A-Za-z_][A-Za-z0-9_\s\*]*?\s+)?[A-Za-z_][A-Za-z0-9_]*"
        r"\s*\([^;{}]*?\)\s*(?:;|\{)"
    ),
)


def fail(message: str) -> None:
    print(message, file=sys.stderr)
    raise SystemExit(1)


def require(condition: bool, message: str) -> None:
    if not condition:
        fail(message)


def split_matrix_cells(line: str) -> list[str]:
    return [part.strip() for part in line.split("|")[1:-1]]


def separator_cell_problem(cell: str) -> str | None:
    if not cell:
        return "empty separator cell"
    if not re.fullmatch(r":?-{3,}:?", cell):
        return f"invalid separator cell {cell!r} (expected ---, :---, ---: or :---:)"
    return None


def is_markdown_separator_row(cells: list[str]) -> bool:
    return bool(cells) and all(separator_cell_problem(cell) is None for cell in cells)


def require_matrix_separator(line: str | None, expected_cells: int, header_line_number: int) -> None:
    context = f"the matrix table header on line {header_line_number}"
    cells = split_matrix_cells(line) if line is not None and line.startswith("|") else []
    require(
        any(separator_cell_problem(cell) is None for cell in cells),
        (
            f"missing separator row after {context}"
            + (f": found {line.strip()!r}" if line is not None else "")
        ),
    )
    problems = [
        f"column {index}: {problem}"
        for index, problem in enumerate((separator_cell_problem(cell) for cell in cells), start=1)
        if problem is not None
    ]
    require(
        not problems,
        f"malformed separator row after {context}: {'; '.join(problems)}",
    )
    require(
        len(cells) == expected_cells,
        (
            f"malformed separator row after {context}: expected {expected_cells} cells, "
            f"found {len(cells)}"
        ),
    )


def parse_markdown_table(lines: list[str], heading: str) -> tuple[list[str], list[list[str]]]:
    try:
        heading_index = lines.index(heading)
    except ValueError:
        fail(f"missing heading: {heading}")

    line_index = heading_index + 1
    while line_index < len(lines) and not lines[line_index].startswith("|"):
        line_index += 1
    require(line_index < len(lines), f"missing markdown table after {heading}")

    header_cells = split_matrix_cells(lines[line_index])
    separator_index = line_index + 1
    require(
        separator_index < len(lines) and lines[separator_index].startswith("|"),
        f"missing separator row after header under {heading}",
    )
    separator_cells = split_matrix_cells(lines[separator_index])
    require(
        len(separator_cells) == len(header_cells),
        (
            f"malformed separator row under {heading}: expected {len(header_cells)} cells, "
            f"found {len(separator_cells)}"
        ),
    )
    require(
        is_markdown_separator_row(separator_cells),
        f"malformed separator row under {heading}: {lines[separator_index]}",
    )

    rows: list[list[str]] = []
    line_index = separator_index + 1
    while line_index < len(lines) and lines[line_index].startswith("|"):
        cells = split_matrix_cells(lines[line_index])
        require(
            not is_markdown_separator_row(cells),
            (
                f"unexpected extra separator row under {heading} on line "
                f"{line_index + 1}"
            ),
        )
        require(
            len(cells) == len(header_cells),
            (
                f"malformed table row under {heading}: expected {len(header_cells)} cells, "
                f"found {len(cells)}"
            ),
        )
        rows.append(cells)
        line_index += 1
    return header_cells, rows


def parse_metadata(lines: list[str]) -> dict[str, str]:
    headers, rows = parse_markdown_table(lines, "## 1. Status of this document")
    require(headers == ["Field", "Value"], f"unexpected metadata headers: {headers}")
    metadata: dict[str, str] = {}
    for field, value in rows:
        require(field not in metadata, f"duplicate metadata field: {field}")
        metadata[field] = value
    return metadata


def parse_count(cell: str) -> int:
    value = cell.strip()
    match = COUNT_RE.fullmatch(value)
    require(match is not None, f"unsupported count format: {cell!r}")
    return int(match.group("number").replace(",", ""))


def strip_backticks(cell: str) -> str:
    return cell[1:-1] if cell.startswith("`") and cell.endswith("`") else cell


def parse_rows_metadata(value: str) -> tuple[int, int]:
    match = re.fullmatch(
        r"(?P<total>\d[\d,]*) \((?P<required>\d[\d,]*) required by the \[§2\.2\]\(#sec-2\) release gate\)",
        value,
    )
    require(match is not None, f"malformed Rows metadata: {value}")
    return parse_count(match.group("total")), parse_count(match.group("required"))


def parse_covered_rows_metadata(value: str) -> int:
    match = re.match(r"\*\*(?P<count>\d[\d,]*)\*\*", value)
    require(match is not None, f"malformed Covered rows metadata: {value}")
    return parse_count(match.group("count"))


def parse_status_summary(lines: list[str]) -> tuple[dict[str, int], int]:
    headers, rows = parse_markdown_table(lines, "### 25.1 By status")
    require(headers == ["Status", "Rows"], f"unexpected status-summary headers: {headers}")

    declared_counts: dict[str, int] = {}
    total_rows: int | None = None
    for status, count_cell in rows:
        if status == "**Total**":
            require(total_rows is None, "duplicate total row in status summary table")
            total_rows = parse_count(count_cell)
            continue
        require(status in STATUSES, f"unknown status in summary table: {status}")
        require(status not in declared_counts, f"duplicate status summary row: {status}")
        declared_counts[status] = parse_count(count_cell)

    require(total_rows is not None, "missing total row in status summary table")
    require(
        set(declared_counts) == STATUSES,
        f"status summary does not cover expected statuses: {sorted(declared_counts)}",
    )
    return declared_counts, total_rows


def parse_category_summary(
    lines: list[str],
) -> tuple[dict[str, dict[str, int]], dict[str, int], dict[str, int], int]:
    headers, rows = parse_markdown_table(lines, "### 25.2 By category")
    expected_headers = [
        "Section",
        "Prefix",
        "Rows",
        "Covered",
        "Missing Go",
        "Missing C ABI",
        "Behavior mismatch",
        "Intentional divergence",
        "Not required",
    ]
    require(headers == expected_headers, f"unexpected category-summary headers: {headers}")

    declared_prefix_counts: dict[str, dict[str, int]] = {}
    declared_prefix_totals: dict[str, int] = {}
    declared_total_status_counts: dict[str, int] | None = None
    declared_total_rows: int | None = None

    for row in rows:
        section, prefix_cell, row_total_cell = row[:3]
        status_cells = row[3:]
        row_status_counts = {
            CATEGORY_STATUS_COLUMNS[header]: parse_count(cell)
            for header, cell in zip(headers[3:], status_cells)
        }

        if section == "**Total**":
            require(
                declared_total_rows is None and declared_total_status_counts is None,
                "duplicate total row in category summary table",
            )
            declared_total_rows = parse_count(row_total_cell)
            declared_total_status_counts = row_status_counts
            continue

        prefix = strip_backticks(prefix_cell)
        require(prefix, f"missing prefix in category summary row: {row}")
        require(prefix not in declared_prefix_counts, f"duplicate category summary row: {prefix}")
        declared_prefix_counts[prefix] = row_status_counts
        declared_prefix_totals[prefix] = parse_count(row_total_cell)

    require(declared_total_rows is not None, "missing total row in category summary table")
    require(
        declared_total_status_counts is not None,
        "missing status totals in category summary table",
    )
    return (
        declared_prefix_counts,
        declared_prefix_totals,
        declared_total_status_counts,
        declared_total_rows,
    )


def normalize_whitespace(text: str) -> str:
    return re.sub(r"\s+", " ", text).strip()


def extract_section_body(lines: Sequence[str], heading: str) -> list[str]:
    """Return one level-2 section's lines, excluding its heading.

    Empty when the heading is absent, which callers treat as a failure rather
    than as a vacuously satisfied search.
    """
    try:
        start = list(lines).index(heading)
    except ValueError:
        return []
    body: list[str] = []
    for line in lines[start + 1:]:
        if line.startswith("## "):
            break
        body.append(line)
    return body


def extract_section_preamble(lines: Sequence[str], heading: str) -> list[str]:
    """Return a section's prose above its first table row.

    Prose that introduces a table has to sit above it; searching the whole
    section would accept a sentence pushed below the table it describes.
    """
    preamble: list[str] = []
    for line in extract_section_body(lines, heading):
        if line.startswith("|"):
            break
        preamble.append(line)
    return preamble


# CommonMark renders a line indented four columns past the block that contains
# it as an indented code block rather than as prose. `normalize_whitespace`
# collapses that indentation, so a whole-block pin cannot see the difference:
# indenting every line of a pinned preamble would leave the normalized text
# byte-identical to its pin while the rendered document stopped carrying the
# normative prose at all.
INDENTED_CODE_INDENT = 4
LIST_ITEM_MARKER_RE = re.compile(r"^(?P<indent> *)(?P<marker>[-*+]|\d{1,9}[.)])(?P<gap> +)")


def leading_indent_columns(line: str) -> int:
    """Return a line's indentation in columns, expanding tabs to four."""
    columns = 0
    for char in line:
        if char == " ":
            columns += 1
        elif char == "\t":
            columns += INDENTED_CODE_INDENT - (columns % INDENTED_CODE_INDENT)
        else:
            break
    return columns


def find_indented_code_lines(lines: Sequence[str]) -> list[str]:
    """Return the lines Markdown would render as indented code, not as prose.

    A line only opens an indented code block when a blank line precedes it and
    it is indented four columns past the block containing it, so the scan tracks
    the enclosing list item's content indent. A lazy continuation of the
    paragraph above still renders as prose and is not reported.
    """
    offenders: list[str] = []
    content_indent = 0
    previous_blank = True
    for line in lines:
        if not line.strip():
            previous_blank = True
            continue
        indent = leading_indent_columns(line)
        if indent < content_indent:
            content_indent = 0
        marker = LIST_ITEM_MARKER_RE.match(line)
        if marker and indent < content_indent + INDENTED_CODE_INDENT:
            content_indent = indent + len(marker.group("marker")) + len(marker.group("gap"))
        elif previous_blank and indent >= content_indent + INDENTED_CODE_INDENT:
            offenders.append(line)
        previous_blank = False
    return offenders


def find_hidden_markup_lines(lines: Sequence[str]) -> list[str]:
    """Return the lines that hide prose behind a comment, a fence or raw HTML.

    A raw `<code>`/`<pre>` wrapper renders its contents literally exactly as a
    fence does, and `normalize_whitespace` cannot see the difference, so a
    wrapper placed around a pinned block and around its pin would keep the
    normative prose out of the rendered document while still comparing equal.
    """
    offenders: list[str] = []
    open_tags: list[str] = []
    for line in lines:
        hidden = bool(open_tags) or "<!--" in line or "-->" in line
        if MARKDOWN_FENCE_OPENER_RE.match(line):
            hidden = True
        for match in MARKDOWN_HTML_CODE_TAG_RE.finditer(line):
            hidden = True
            tag = match.group("tag").lower()
            if match.group("closing"):
                if tag in open_tags:
                    del open_tags[open_tags.index(tag) :]
            elif not match.group("selfclosing"):
                open_tags.append(tag)
        if hidden:
            offenders.append(line)
    return offenders


def require_rendered_prose(lines: Sequence[str], label: str) -> None:
    """Fail when a pinned block would not reach the reader as prose.

    Four constructs hide prose without altering it once whitespace is
    normalized: an HTML comment, a fenced code block, a raw `<code>`/`<pre>`
    block, and an indented code block. A pin updated with the same construct
    still compares equal, so a block is checked for all four before it is
    compared.
    """
    hidden = find_hidden_markup_lines(lines)
    first_hidden = hidden[0] if hidden else ""
    require(
        not hidden,
        (
            f"{label}'s prose is wrapped in markup that keeps it out of the rendered "
            "document — an HTML comment, a code fence or a raw <code>/<pre> block "
            "— which a pin carrying the same "
            f"markup cannot detect: {first_hidden!r}"
        ),
    )
    offenders = find_indented_code_lines(lines)
    first = offenders[0] if offenders else ""
    require(
        not offenders,
        (
            f"{label}'s prose is indented far enough that Markdown renders it as an "
            "indented code block rather than as normative prose, and whitespace "
            f"normalization hides that from the pin comparison: {first!r}"
        ),
    )


def require_rendered_cell(text: str, label: str) -> None:
    """Fail when a normative table cell hides text behind an HTML comment.

    A table cell is one line, so a comment is the only way to keep part of it
    out of the rendered document. Applied to the pin as well as to the cell,
    because a coordinated edit to both would otherwise still compare equal.
    """
    require(
        "<!--" not in text and "-->" not in text,
        (
            f"{label} carries an HTML comment, so part of a normative cell never reaches "
            f"the reader: {text!r}"
        ),
    )


def row_identifier(cells: list[str], line: str) -> str:
    if cells:
        return cells[0]
    match = re.match(r"\|\s*(SB-[^|`\s]+)", line)
    return match.group(1) if match else "<unknown-row>"


def iter_matrix_rows(lines: list[str]) -> Iterator[tuple[int, str, list[str]]]:
    current_header_cells: int | None = None
    pending_separator: tuple[int, int] | None = None
    for line_number, line in enumerate(lines, start=1):
        if pending_separator is not None:
            header_line_number, header_cell_count = pending_separator
            pending_separator = None
            require_matrix_separator(line, header_cell_count, header_line_number)
            continue
        if not line.startswith("|"):
            current_header_cells = None
            continue
        if line.startswith("| ID |"):
            header_cells = split_matrix_cells(line)
            if "Status" in header_cells:
                current_header_cells = len(header_cells)
                pending_separator = (line_number, len(header_cells))
            else:
                current_header_cells = None
            continue
        if current_header_cells is not None and is_markdown_separator_row(split_matrix_cells(line)):
            fail(f"unexpected separator row inside a matrix table on line {line_number}")
        if not line.startswith("| SB-"):
            continue
        require(
            line.rstrip().endswith("|"),
            f"malformed SB row {row_identifier([], line)} on line {line_number}: missing final table delimiter",
        )
        cells = split_matrix_cells(line)
        row_id = row_identifier(cells, line)
        if current_header_cells is None or row_id.startswith("SB-GAP"):
            continue
        expected_cells = current_header_cells or len(cells)
        require(
            len(cells) == expected_cells,
            (
                f"malformed SB row {row_id} on line {line_number}: expected "
                f"{expected_cells} cells, found {len(cells)}"
            ),
        )
        yield line_number, row_id, cells
    if pending_separator is not None:
        require_matrix_separator(None, pending_separator[1], pending_separator[0])


def parse_rows(
    lines: list[str],
) -> tuple[Counter[str], dict[str, Counter[str]], tuple[tuple[str, str], ...]]:
    status_counts: Counter[str] = Counter()
    prefix_counts: dict[str, Counter[str]] = defaultdict(Counter)
    accepted_row_ids: dict[str, tuple[int, str]] = {}
    row_sequence: list[tuple[str, str]] = []
    for line_number, row_id, cells in iter_matrix_rows(lines):
        status = cells[-2]
        require(
            status in STATUSES,
            f"unknown status for row {row_id} on line {line_number}: {status}",
        )
        previous = accepted_row_ids.get(row_id)
        if previous is not None:
            fail(
                f"duplicate accepted row id {row_id} on lines {previous[0]} and {line_number} "
                f"({previous[1]} vs {status})"
            )
        accepted_row_ids[row_id] = (line_number, status)
        row_sequence.append((row_id, status))
        prefix = "-".join(row_id.split("-")[:2])
        status_counts[status] += 1
        prefix_counts[prefix][status] += 1
    return status_counts, prefix_counts, tuple(row_sequence)


def parse_row_manifest_lines(
    lines: Sequence[str], *, source: str
) -> tuple[tuple[str, str], ...]:
    """Parse a manifest of audited rows: one `ROW-ID STATUS` entry per line."""
    entries: list[tuple[str, str]] = []
    seen: set[str] = set()
    for line_number, raw_line in enumerate(lines, start=1):
        line = raw_line.strip()
        if not line or line.startswith("#"):
            continue
        match = re.fullmatch(r"(?P<row_id>SB-[A-Z0-9-]+)[ \t]+(?P<status>\S.*)", line)
        require(
            match is not None,
            (
                f"invalid row manifest entry in {source} on line {line_number}: {raw_line!r} "
                "(expected 'ROW-ID STATUS')"
            ),
        )
        assert match is not None
        row_id = match.group("row_id")
        status = match.group("status").strip()
        require(
            status in STATUSES,
            (
                f"invalid status in row manifest entry in {source} on line {line_number}: "
                f"{status!r}"
            ),
        )
        require(row_id not in seen, f"duplicate row manifest entry in {source}: {row_id}")
        seen.add(row_id)
        entries.append((row_id, status))
    return tuple(entries)


def validate_expected_row_manifest_provenance(lines: Sequence[str], *, source: str) -> None:
    filename_match = re.fullmatch(
        r"sharkbite-compatibility-revision(?P<revision>\d+)-rows\.txt", source
    )
    require(
        filename_match is not None,
        f"unexpected row manifest filename {source!r}",
    )
    assert filename_match is not None
    require(
        int(filename_match.group("revision")) == EXPECTED_REVISION,
        (
            f"row manifest filename {source!r} does not match EXPECTED_REVISION "
            f"{EXPECTED_REVISION}"
        ),
    )
    comment_lines = tuple(raw_line.rstrip() for raw_line in lines if raw_line.startswith("#"))
    require(
        comment_lines[: len(EXPECTED_ROW_MANIFEST_HEADER)] == EXPECTED_ROW_MANIFEST_HEADER,
        (
            f"row manifest header in {source} does not match revision {EXPECTED_REVISION}: "
            f"expected {EXPECTED_ROW_MANIFEST_HEADER[0]!r}"
        ),
    )


@lru_cache(maxsize=1)
def load_expected_rows() -> tuple[tuple[str, str], ...]:
    manifest_lines = EXPECTED_ROW_MANIFEST.read_text(encoding="utf-8").splitlines()
    validate_expected_row_manifest_provenance(
        manifest_lines, source=str(EXPECTED_ROW_MANIFEST.name)
    )
    rows = parse_row_manifest_lines(
        manifest_lines,
        source=str(EXPECTED_ROW_MANIFEST.name),
    )
    require(
        len(rows) == EXPECTED_TOTAL_ROWS,
        (
            f"revision {EXPECTED_REVISION} row manifest expects {EXPECTED_TOTAL_ROWS} entries, "
            f"found {len(rows)}"
        ),
    )
    manifest_status_counts = Counter(status for _row_id, status in rows)
    for status, expected in EXPECTED_STATUS_COUNTS.items():
        require(
            manifest_status_counts[status] == expected,
            (
                f"revision {EXPECTED_REVISION} row manifest pins {manifest_status_counts[status]} "
                f"rows for {status}, but EXPECTED_STATUS_COUNTS pins {expected}"
            ),
        )
    return rows


def expected_scope_entry(row_id: str, status: str) -> tuple[str, str]:
    prefix = "-".join(row_id.split("-")[:2])
    if row_id in DEFERRED_MACOS_SCOPE_ROWS:
        return "Optional", "optional-deferred-macos-native"
    if prefix in OPTIONAL_SCOPE_PREFIXES:
        rule = (
            "optional-python-extra"
            if prefix in {"SB-PANDA", "SB-TORCH"}
            else "optional-legacy-cxx"
        )
        return "Optional", rule
    if row_id in OPTIONAL_SCOPE_ROWS:
        return "Optional", "optional-python-extra"
    if status == "Covered":
        return "Covered", "core-covered"
    if status == INTENTIONAL_DIVERGENCE_STATUS:
        return "Approved divergence", "core-approved-divergence"
    if status == NOT_REQUIRED_STATUS:
        return "Not required", "not-required-rationale"
    return "Required gap", "core-required-gap"


def parse_scope_manifest_lines(
    lines: Sequence[str], *, source: str
) -> tuple[tuple[str, str, str, str], ...]:
    entries: list[tuple[str, str, str, str]] = []
    seen: set[str] = set()
    for line_number, raw_line in enumerate(lines, start=1):
        if not raw_line.strip() or raw_line.startswith("#"):
            continue
        cells = raw_line.rstrip().split("\t")
        require(
            len(cells) == 4,
            (
                f"invalid scope manifest entry in {source} on line {line_number}: "
                "expected four tab-separated columns"
            ),
        )
        row_id, disposition, rule, status = cells
        require(re.fullmatch(r"SB-[A-Z0-9-]+", row_id) is not None, f"invalid scope row id: {row_id}")
        require(disposition in SCOPE_DISPOSITIONS, f"invalid scope disposition for {row_id}: {disposition}")
        require(rule in SCOPE_RULES, f"invalid scope rule for {row_id}: {rule}")
        require(status in STATUSES, f"invalid pinned matrix status for {row_id}: {status}")
        require(row_id not in seen, f"duplicate scope manifest entry in {source}: {row_id}")
        seen.add(row_id)
        entries.append((row_id, disposition, rule, status))
    return tuple(entries)


def validate_scope_manifest_provenance(lines: Sequence[str]) -> None:
    comments = tuple(line.rstrip() for line in lines if line.startswith("#"))
    require(
        comments[: len(EXPECTED_SCOPE_MANIFEST_HEADER)] == EXPECTED_SCOPE_MANIFEST_HEADER,
        f"scope manifest provenance header does not match revision {EXPECTED_REVISION}",
    )


@lru_cache(maxsize=1)
def load_expected_scope() -> tuple[tuple[str, str, str, str], ...]:
    lines = EXPECTED_SCOPE_MANIFEST.read_text(encoding="utf-8").splitlines()
    validate_scope_manifest_provenance(lines)
    entries = parse_scope_manifest_lines(lines, source=EXPECTED_SCOPE_MANIFEST.name)
    require(
        len(entries) == EXPECTED_TOTAL_ROWS,
        f"scope manifest expects {EXPECTED_TOTAL_ROWS} entries, found {len(entries)}",
    )
    return entries


def validate_scope_manifest(
    rows: Sequence[tuple[str, str]],
) -> Counter[str]:
    entries = load_expected_scope()
    require(
        tuple((row_id, status) for row_id, _disposition, _rule, status in entries)
        == tuple(rows),
        "scope manifest row order or pinned matrix statuses differ from the audited row manifest",
    )
    counts: Counter[str] = Counter()
    for row_id, disposition, rule, status in entries:
        expected_disposition, expected_rule = expected_scope_entry(row_id, status)
        require(
            (disposition, rule) == (expected_disposition, expected_rule),
            (
                f"unjustified scope classification for {row_id}: found "
                f"{disposition}/{rule}, expected {expected_disposition}/{expected_rule}; "
                "change the normative rule set and its tests rather than bulk-editing the manifest"
            ),
        )
        counts[disposition] += 1
    require(
        dict(counts) == EXPECTED_SCOPE_COUNTS,
        f"scope disposition counts differ from the pinned counts: {dict(counts)}",
    )
    return counts


def format_row_entry(entry: tuple[str, str] | str) -> str:
    if isinstance(entry, tuple):
        row_id, status = entry
        return f"{row_id} ({status})"
    return entry


def preview_row_ids(row_ids: Sequence[tuple[str, str] | str], *, limit: int = 5) -> str:
    preview = [format_row_entry(entry) for entry in row_ids[:limit]]
    suffix = " ..." if len(row_ids) > limit else ""
    return (", ".join(preview) + suffix) if preview else "none"


def validate_pinned_inventory_constants() -> None:
    require(
        EXPECTED_ROW_MANIFEST.name.endswith(f"revision{EXPECTED_REVISION}-rows.txt")
        and EXPECTED_C_ABI_SYMBOL_MANIFEST.name.endswith(
            f"revision{EXPECTED_REVISION}-cabi-symbols.txt"
        ),
        (
            f"manifest filenames do not follow EXPECTED_REVISION {EXPECTED_REVISION}: "
            f"{EXPECTED_ROW_MANIFEST.name}, {EXPECTED_C_ABI_SYMBOL_MANIFEST.name}"
        ),
    )
    require(
        EXPECTED_SCOPE_MANIFEST.name.endswith(f"revision{EXPECTED_REVISION}-scope.tsv"),
        f"scope manifest filename does not follow EXPECTED_REVISION {EXPECTED_REVISION}",
    )
    require(
        set(EXPECTED_STATUS_COUNTS) == STATUSES,
        f"EXPECTED_STATUS_COUNTS does not cover the six statuses: {sorted(EXPECTED_STATUS_COUNTS)}",
    )
    pinned_status_total = sum(EXPECTED_STATUS_COUNTS.values())
    require(
        pinned_status_total == EXPECTED_TOTAL_ROWS,
        (
            f"pinned per-status counts sum to {pinned_status_total}, but EXPECTED_TOTAL_ROWS is "
            f"{EXPECTED_TOTAL_ROWS}"
        ),
    )
    require(
        set(EXPECTED_SCOPE_COUNTS) == SCOPE_DISPOSITIONS,
        f"EXPECTED_SCOPE_COUNTS does not cover all dispositions: {sorted(EXPECTED_SCOPE_COUNTS)}",
    )
    require(
        sum(EXPECTED_SCOPE_COUNTS.values()) == EXPECTED_TOTAL_ROWS,
        "pinned scope disposition counts do not sum to EXPECTED_TOTAL_ROWS",
    )
    pinned_required = (
        EXPECTED_SCOPE_COUNTS["Covered"]
        + EXPECTED_SCOPE_COUNTS["Approved divergence"]
        + EXPECTED_SCOPE_COUNTS["Required gap"]
    )
    require(
        pinned_required == EXPECTED_REQUIRED_ROWS,
        (
            f"pinned required rows should be {pinned_required}, but EXPECTED_REQUIRED_ROWS is "
            f"{EXPECTED_REQUIRED_ROWS}"
        ),
    )
    require(
        set(EXPECTED_PREFIX_TOTALS) == set(EXPECTED_PREFIX_COUNTS),
        (
            "pinned section totals and pinned section status counts describe different sections: "
            f"{sorted(EXPECTED_PREFIX_TOTALS)} vs {sorted(EXPECTED_PREFIX_COUNTS)}"
        ),
    )
    pinned_prefix_total = sum(EXPECTED_PREFIX_TOTALS.values())
    require(
        pinned_prefix_total == EXPECTED_TOTAL_ROWS,
        (
            f"pinned section totals sum to {pinned_prefix_total}, but EXPECTED_TOTAL_ROWS is "
            f"{EXPECTED_TOTAL_ROWS}"
        ),
    )
    for prefix, expected_counts in EXPECTED_PREFIX_COUNTS.items():
        pinned_section_total = sum(expected_counts.values())
        require(
            pinned_section_total == EXPECTED_PREFIX_TOTALS[prefix],
            (
                f"pinned status counts for {prefix} sum to {pinned_section_total}, but "
                f"EXPECTED_PREFIX_TOTALS pins {EXPECTED_PREFIX_TOTALS[prefix]}"
            ),
        )
    for status in STATUSES:
        pinned_by_section = sum(counts[status] for counts in EXPECTED_PREFIX_COUNTS.values())
        require(
            pinned_by_section == EXPECTED_STATUS_COUNTS[status],
            (
                f"pinned section counts sum to {pinned_by_section} rows for {status}, but "
                f"EXPECTED_STATUS_COUNTS pins {EXPECTED_STATUS_COUNTS[status]}"
            ),
        )
    require(
        len(EXPECTED_C_ABI_UNREFERENCED_EXPORTS)
        == EXPECTED_C_ABI_DECLARED_EXPORTS - EXPECTED_C_ABI_REFERENCED_EXPORTS,
        (
            "pinned C ABI symbol counts drifted: "
            f"{len(EXPECTED_C_ABI_UNREFERENCED_EXPORTS)} missing symbols vs "
            f"{EXPECTED_C_ABI_DECLARED_EXPORTS} declared and {EXPECTED_C_ABI_REFERENCED_EXPORTS} referenced"
        ),
    )


def parse_command_text(command_text: str) -> list[str]:
    return shlex.split(command_text, posix=os.name != "nt")


@lru_cache(maxsize=None)
def go_env(name: str) -> str:
    output = subprocess.run(
        ["go", "env", name],
        check=True,
        capture_output=True,
        text=True,
    )
    return output.stdout.strip()


def compiler_command(name: str) -> list[str] | None:
    command_text = go_env(name)
    if not command_text:
        return None
    fields = parse_command_text(command_text)
    return fields if fields else None


def symbol_tool_command() -> list[str] | None:
    for candidate in ("nm", "llvm-nm"):
        path = shutil.which(candidate)
        if path is not None:
            return [path]
    cc = compiler_command("CC")
    if cc:
        compiler_name = Path(cc[0]).name.lower()
        for suffix in ("gcc", "cc", "clang"):
            if compiler_name.endswith(suffix):
                stem = Path(cc[0]).name[: -len(suffix)]
                candidate = stem + "nm"
                path = shutil.which(candidate)
                if path is not None:
                    return [path]
    return None


def load_c_abi_symbol_manifest_lines(path: Path | None = None) -> list[str]:
    manifest_path = EXPECTED_C_ABI_SYMBOL_MANIFEST if path is None else path
    return manifest_path.read_text(encoding="utf-8").splitlines()


def parse_named_symbol_sections(
    lines: Sequence[str], *, source: str
) -> dict[str, tuple[str, ...]]:
    sections: dict[str, list[str]] = {}
    current_section: str | None = None
    for line_number, raw_line in enumerate(lines, start=1):
        line = raw_line.strip()
        if not line or line.startswith("#"):
            continue
        section_match = re.fullmatch(r"\[(?P<section>[a-z_]+)\]", line)
        if section_match is not None:
            current_section = section_match.group("section")
            require(
                current_section not in sections,
                f"duplicate symbol section [{current_section}] in {source}",
            )
            sections[current_section] = []
            continue
        require(
            current_section is not None,
            f"unexpected symbol entry before a section header in {source} on line {line_number}: {raw_line!r}",
        )
        require(
            re.fullmatch(r"shoal_[A-Za-z0-9_]+", line) is not None,
            f"invalid symbol entry in {source} on line {line_number}: {raw_line!r}",
        )
        sections[current_section].append(line)
    return {section: tuple(entries) for section, entries in sections.items()}


def validate_expected_c_abi_symbol_manifest_provenance(
    lines: Sequence[str], *, source: str
) -> None:
    filename_match = re.fullmatch(
        r"sharkbite-compatibility-revision(?P<revision>\d+)-cabi-symbols\.txt",
        source,
    )
    require(filename_match is not None, f"unexpected C ABI symbol manifest filename {source!r}")
    assert filename_match is not None
    require(
        int(filename_match.group("revision")) == EXPECTED_REVISION,
        (
            f"C ABI symbol manifest filename {source!r} does not match EXPECTED_REVISION "
            f"{EXPECTED_REVISION}"
        ),
    )
    comment_lines = tuple(raw_line.rstrip() for raw_line in lines if raw_line.startswith("#"))
    require(
        comment_lines[: len(EXPECTED_C_ABI_SYMBOL_MANIFEST_HEADER)]
        == EXPECTED_C_ABI_SYMBOL_MANIFEST_HEADER,
        (
            f"C ABI symbol manifest header in {source} does not match revision "
            f"{EXPECTED_REVISION}: expected {EXPECTED_C_ABI_SYMBOL_MANIFEST_HEADER[0]!r}"
        ),
    )


@lru_cache(maxsize=1)
def load_expected_c_abi_symbol_manifest() -> dict[str, tuple[str, ...]]:
    lines = load_c_abi_symbol_manifest_lines()
    validate_expected_c_abi_symbol_manifest_provenance(
        lines, source=EXPECTED_C_ABI_SYMBOL_MANIFEST.name
    )
    sections = parse_named_symbol_sections(lines, source=EXPECTED_C_ABI_SYMBOL_MANIFEST.name)
    require(
        set(sections) == {"declared", "referenced", "unreferenced"},
        (
            "C ABI symbol manifest sections do not match the audited inventory: "
            f"{sorted(sections)}"
        ),
    )
    declared = sections["declared"]
    referenced = sections["referenced"]
    unreferenced = sections["unreferenced"]
    require(
        len(declared) == EXPECTED_C_ABI_DECLARED_EXPORTS,
        (
            f"C ABI symbol manifest pins {len(declared)} declared exports, but "
            f"EXPECTED_C_ABI_DECLARED_EXPORTS pins {EXPECTED_C_ABI_DECLARED_EXPORTS}"
        ),
    )
    require(
        len(referenced) == EXPECTED_C_ABI_REFERENCED_EXPORTS,
        (
            f"C ABI symbol manifest pins {len(referenced)} referenced exports, but "
            f"EXPECTED_C_ABI_REFERENCED_EXPORTS pins {EXPECTED_C_ABI_REFERENCED_EXPORTS}"
        ),
    )
    require(
        unreferenced == EXPECTED_C_ABI_UNREFERENCED_EXPORTS,
        (
            "C ABI symbol manifest pins a different unreferenced export inventory: "
            f"{unreferenced}"
        ),
    )
    return sections


def collect_c_abi_declared_exports(repo_root: Path | None = None) -> tuple[str, ...]:
    root = repo_root or DOC_PATH.parent.parent
    exports = tuple(
        match.group("name")
        for match in EXPORT_SYMBOL_RE.finditer(
            (root / C_ABI_EXPORT_HEADER_PATH).read_text(encoding="utf-8")
        )
    )
    require(exports, f"no SHOAL_API exports found in {C_ABI_EXPORT_HEADER_PATH}")
    require(
        len(exports) == len(set(exports)),
        f"duplicate SHOAL_API export declarations found in {C_ABI_EXPORT_HEADER_PATH}",
    )
    return exports


def compiler_for_source(source_path: Path) -> list[str] | None:
    suffix = source_path.suffix.lower()
    if suffix in {".cpp", ".cc", ".cxx"}:
        return compiler_command("CXX")
    return compiler_command("CC")


def compile_source_to_object(
    source_path: Path,
    object_path: Path,
    *,
    include_paths: Sequence[Path],
    repo_root: Path,
) -> None:
    compiler = compiler_for_source(source_path)
    require(compiler is not None, f"no compiler configured for {source_path}")
    args = list(compiler)
    suffix = source_path.suffix.lower()
    if suffix in {".cpp", ".cc", ".cxx"}:
        args.extend(["-std=c++11", "-Wall", "-Wextra", "-Werror"])
    else:
        args.extend(["-std=c11", "-Wall", "-Wextra", "-Werror"])
    for include_path in include_paths:
        args.extend(["-I", str(repo_root / include_path)])
    args.extend(["-c", str(repo_root / source_path), "-o", str(object_path)])
    subprocess.run(args, cwd=repo_root, check=True, capture_output=True, text=True)


def extract_undefined_shoal_symbols(
    object_path: Path, *, symbol_tool: Sequence[str]
) -> tuple[str, ...]:
    output = subprocess.run(
        [*symbol_tool, "-u", str(object_path)],
        check=True,
        capture_output=True,
        text=True,
    ).stdout
    symbols: set[str] = set()
    for raw_line in output.splitlines():
        for token in re.findall(r"(?:__imp__?|_)?shoal_[A-Za-z0-9_]+", raw_line):
            normalized = token
            while normalized.startswith("_"):
                normalized = normalized[1:]
            if normalized.startswith("imp_"):
                normalized = normalized[len("imp_") :]
            if normalized.startswith("_"):
                normalized = normalized[1:]
            if normalized.startswith("shoal_"):
                symbols.add(normalized)
    return tuple(sorted(symbols))


def compiled_c_abi_reference_inventory(
    source_paths: Sequence[Path] = C_ABI_REFERENCE_PATHS,
    *,
    include_paths: Sequence[Path] = DEFAULT_C_ABI_INCLUDE_PATHS,
    repo_root: Path | None = None,
) -> tuple[str, ...] | None:
    root = repo_root or DOC_PATH.parent.parent
    symbol_tool = symbol_tool_command()
    if symbol_tool is None:
        return None
    if any(compiler_for_source(path) is None for path in source_paths):
        return None
    with tempfile.TemporaryDirectory(dir=root) as temp_dir_name:
        temp_dir = Path(temp_dir_name)
        collected: set[str] = set()
        for index, source_path in enumerate(source_paths):
            object_path = temp_dir / f"symbol_inventory_{index}{source_path.suffix}.o"
            compile_source_to_object(
                source_path,
                object_path,
                include_paths=include_paths,
                repo_root=root,
            )
            collected.update(
                extract_undefined_shoal_symbols(object_path, symbol_tool=symbol_tool)
            )
    return tuple(sorted(collected))


def collect_c_abi_symbol_inventory(
    repo_root: Path | None = None,
) -> tuple[tuple[str, ...], tuple[str, ...], tuple[str, ...]]:
    exports = collect_c_abi_declared_exports(repo_root)
    manifest = load_expected_c_abi_symbol_manifest()
    compiled_references = compiled_c_abi_reference_inventory(repo_root=repo_root)
    if compiled_references is None:
        referenced = manifest["referenced"]
    else:
        export_set = set(exports)
        referenced = tuple(symbol for symbol in compiled_references if symbol in export_set)
    unreferenced = tuple(symbol for symbol in exports if symbol not in referenced)
    return exports, referenced, unreferenced


def collect_c_abi_free_function_inventory(repo_root: Path | None = None) -> tuple[str, ...]:
    root = repo_root or DOC_PATH.parent.parent
    header_text = (root / C_ABI_EXPORT_HEADER_PATH).read_text(encoding="utf-8")
    free_functions = tuple(match.group("name") for match in FREE_SYMBOL_RE.finditer(header_text))
    require(free_functions, f"no free functions found in {C_ABI_EXPORT_HEADER_PATH}")
    require(
        len(free_functions) == len(set(free_functions)),
        f"duplicate free function declarations found in {C_ABI_EXPORT_HEADER_PATH}",
    )
    return free_functions


def format_named_symbol_manifest(
    sections: dict[str, Sequence[str]], *, header: Sequence[str]
) -> str:
    lines = [*header, ""]
    for section_name in ("declared", "referenced", "unreferenced"):
        lines.append(f"[{section_name}]")
        lines.extend(sections[section_name])
        lines.append("")
    return "\n".join(lines).rstrip() + "\n"


def write_c_abi_symbol_manifest(repo_root: Path | None = None) -> str:
    exports = collect_c_abi_declared_exports(repo_root)
    compiled_references = compiled_c_abi_reference_inventory(repo_root=repo_root)
    require(
        compiled_references is not None,
        "standard C/C++ toolchain with nm/llvm-nm is required to regenerate the C ABI symbol manifest",
    )
    export_set = set(exports)
    referenced = tuple(symbol for symbol in compiled_references if symbol in export_set)
    unreferenced = tuple(symbol for symbol in exports if symbol not in referenced)
    manifest_text = format_named_symbol_manifest(
        {
            "declared": exports,
            "referenced": referenced,
            "unreferenced": unreferenced,
        },
        header=EXPECTED_C_ABI_SYMBOL_MANIFEST_HEADER,
    )
    EXPECTED_C_ABI_SYMBOL_MANIFEST.write_text(manifest_text, encoding="utf-8")
    load_expected_c_abi_symbol_manifest.cache_clear()
    return manifest_text


def validate_c_abi_symbol_inventory(full_text: str, repo_root: Path | None = None) -> None:
    exports, referenced, unreferenced = collect_c_abi_symbol_inventory(repo_root)
    manifest = load_expected_c_abi_symbol_manifest()
    require(
        len(exports) == EXPECTED_C_ABI_DECLARED_EXPORTS,
        (
            f"expected {EXPECTED_C_ABI_DECLARED_EXPORTS} SHOAL_API exports in "
            f"{C_ABI_EXPORT_HEADER_PATH}, found {len(exports)}"
        ),
    )
    require(
        len(referenced) == EXPECTED_C_ABI_REFERENCED_EXPORTS,
        (
            f"expected {EXPECTED_C_ABI_REFERENCED_EXPORTS} C/C++ test-referenced exports, "
            f"found {len(referenced)}"
        ),
    )
    require(
        tuple(exports) == manifest["declared"],
        (
            "stale declared C ABI export manifest inventory: "
            f"expected {manifest['declared'][:3]}..., found {tuple(exports)[:3]}..."
        ),
    )
    require(
        tuple(referenced) == manifest["referenced"],
        "stale compiled C ABI referenced-export manifest inventory",
    )
    require(
        tuple(unreferenced) == manifest["unreferenced"],
        "stale compiled C ABI unreferenced-export manifest inventory",
    )
    require(
        unreferenced == EXPECTED_C_ABI_UNREFERENCED_EXPORTS,
        (
            "stale unreferenced C ABI export inventory: "
            f"expected {EXPECTED_C_ABI_UNREFERENCED_EXPORTS}, found {unreferenced}"
        ),
    )
    normalized = normalize_whitespace(full_text)
    require(
        (
            f"applied to {len(exports)} declared exports in `{C_ABI_EXPORT_HEADER_PATH.as_posix()}`"
            in normalized
        ),
        "missing or stale C ABI export-total narrative for SB-XCUT-013",
    )
    require(
        f"reference **{len(referenced)} of the {len(exports)}** exports" in normalized,
        "missing or stale C ABI export-reference narrative for SB-XCUT-013",
    )
    missing_list = ", ".join(f"`{symbol}`" for symbol in unreferenced)
    require(
        missing_list in normalized,
        "missing or stale C ABI unreferenced-export list for SB-XCUT-013",
    )


def validate_c_abi_free_inventory(full_text: str, repo_root: Path | None = None) -> None:
    free_functions = collect_c_abi_free_function_inventory(repo_root)
    exports, referenced, _unreferenced = collect_c_abi_symbol_inventory(repo_root)
    require(
        all(symbol in exports for symbol in free_functions),
        "C ABI free-function inventory contains symbols not declared in shoal.h",
    )
    missing_references = [symbol for symbol in free_functions if symbol not in referenced]
    require(
        not missing_references,
        (
            "typed C ABI free functions are missing from the compiled reference inventory: "
            f"{', '.join(missing_references)}"
        ),
    )
    normalized = normalize_whitespace(full_text)
    free_list = ", ".join(f"`{symbol}`" for symbol in free_functions)
    require(
        f"{len(free_functions)} typed free functions — {free_list}" in normalized,
        "missing or stale typed free-function inventory for SB-XCUT-002",
    )


def moved_row_diagnostics(
    current_rows: Sequence[tuple[str, str]],
    expected_rows: Sequence[tuple[str, str]],
    *,
    limit: int = 5,
) -> list[str]:
    expected_positions = {row_id: index + 1 for index, (row_id, _status) in enumerate(expected_rows)}
    current_positions = {row_id: index + 1 for index, (row_id, _status) in enumerate(current_rows)}
    moved: list[str] = []
    for row_id, _status in expected_rows:
        expected_position = expected_positions[row_id]
        current_position = current_positions.get(row_id)
        if current_position is None or current_position == expected_position:
            continue
        moved.append(f"{row_id} expected {expected_position} found {current_position}")
        if len(moved) >= limit:
            break
    return moved


def reclassified_row_diagnostics(
    current_rows: Sequence[tuple[str, str]],
    expected_rows: Sequence[tuple[str, str]],
    *,
    limit: int = 5,
) -> list[str]:
    current_statuses = dict(current_rows)
    reclassified: list[str] = []
    for row_id, expected_status in expected_rows:
        current_status = current_statuses.get(row_id)
        if current_status is None or current_status == expected_status:
            continue
        reclassified.append(f"{row_id} pinned {expected_status} found {current_status}")
        if len(reclassified) >= limit:
            break
    return reclassified


def validate_expected_row_sequence(
    current_rows: Sequence[tuple[str, str]],
    expected_rows: Sequence[tuple[str, str]],
) -> None:
    current_row_ids = {row_id for row_id, _status in current_rows}
    expected_row_ids = {row_id for row_id, _status in expected_rows}
    missing_rows = [entry for entry in expected_rows if entry[0] not in current_row_ids]
    unexpected_rows = [entry for entry in current_rows if entry[0] not in expected_row_ids]
    moved_rows = moved_row_diagnostics(current_rows, expected_rows)
    reclassified_rows = reclassified_row_diagnostics(current_rows, expected_rows)
    require(
        tuple(current_rows) == tuple(expected_rows),
        (
            f"revision {EXPECTED_REVISION} inventory rows changed: "
            f"missing [{preview_row_ids(missing_rows)}]; "
            f"unexpected [{preview_row_ids(unexpected_rows)}]; "
            f"moved [{preview_row_ids(moved_rows)}]; "
            f"reclassified [{preview_row_ids(reclassified_rows)}]; "
            f"{INVENTORY_CHANGE_HINT}"
        ),
    )


def validate_revision_inventory(
    rows: Sequence[tuple[str, str]],
    status_counts: Counter[str],
    prefix_counts: dict[str, Counter[str]],
) -> None:
    validate_pinned_inventory_constants()
    expected_rows = load_expected_rows()
    validate_expected_row_sequence(rows, expected_rows)

    total_rows = sum(status_counts.values())
    require(
        total_rows == EXPECTED_TOTAL_ROWS,
        (
            f"revision {EXPECTED_REVISION} inventory expects {EXPECTED_TOTAL_ROWS} rows, found "
            f"{total_rows}; {INVENTORY_CHANGE_HINT}"
        ),
    )
    for status, expected in EXPECTED_STATUS_COUNTS.items():
        actual = status_counts[status]
        require(
            actual == expected,
            (
                f"revision {EXPECTED_REVISION} inventory expects {expected} rows for {status}, "
                f"found {actual}; {INVENTORY_CHANGE_HINT}"
            ),
        )

    require(
        set(prefix_counts) == set(EXPECTED_PREFIX_COUNTS),
        (
            f"revision {EXPECTED_REVISION} inventory prefixes do not match: "
            f"{sorted(prefix_counts)} vs {sorted(EXPECTED_PREFIX_COUNTS)}; {INVENTORY_CHANGE_HINT}"
        ),
    )
    for prefix, expected_counts in EXPECTED_PREFIX_COUNTS.items():
        actual_total = sum(prefix_counts[prefix].values())
        require(
            actual_total == EXPECTED_PREFIX_TOTALS[prefix],
            (
                f"revision {EXPECTED_REVISION} inventory expects "
                f"{EXPECTED_PREFIX_TOTALS[prefix]} rows for {prefix}, found {actual_total}; "
                f"{INVENTORY_CHANGE_HINT}"
            ),
        )
        for status, expected in expected_counts.items():
            actual = prefix_counts[prefix][status]
            require(
                actual == expected,
                (
                    f"revision {EXPECTED_REVISION} inventory expects {expected} rows for "
                    f"{prefix} / {status}, found {actual}; {INVENTORY_CHANGE_HINT}"
                ),
            )


def parse_gap_completion_tables(lines: list[str]) -> dict[str, tuple[str, str]]:
    gap_rows: dict[str, tuple[str, str]] = {}
    for heading in (
        "### 23.1 Stage 1 — Go parity (blocks everything)",
        "### 23.2 Stage 2 — C ABI parity (blocked by Stage 1 per row)",
    ):
        headers, rows = parse_markdown_table(lines, heading)
        require(
            headers == ["ID", "Gap", "Matrix rows", "Existing issue/PR", "Notes"],
            f"unexpected headers under {heading}: {headers}",
        )
        for row in rows:
            row_id, _gap, matrix_rows, _existing, notes = row
            if row_id.startswith("SB-GAP-"):
                require(
                    row_id not in gap_rows,
                    f"duplicate audited gap row {row_id}",
                )
                gap_rows[row_id] = (matrix_rows, notes)
    return gap_rows


def expand_gap_row_references(
    matrix_rows_cell: str, row_statuses: dict[str, str], *, gap_id: str
) -> tuple[str, ...]:
    expanded: list[str] = []
    seen: set[str] = set()
    for token in matrix_rows_cell.split(","):
        entry = token.strip()
        if not entry:
            continue
        if entry.endswith("-*"):
            prefix = entry[:-1]
            matches = sorted(row_id for row_id in row_statuses if row_id.startswith(prefix))
            require(matches, f"{gap_id} references no audited rows for wildcard {entry}")
            candidates = matches
        elif "…" in entry:
            start, end = [part.strip() for part in entry.split("…", 1)]
            require(start and end, f"{gap_id} contains an empty range boundary in {entry!r}")
            require("-" in start and "-" in end, f"{gap_id} contains a malformed range {entry}")
            start_prefix, start_number = start.rsplit("-", 1)
            end_prefix, end_number = end.rsplit("-", 1)
            require(
                start_prefix == end_prefix,
                f"{gap_id} mixes prefixes in range {entry}",
            )
            require(
                start_number.isdigit() and end_number.isdigit(),
                f"{gap_id} contains a non-numeric range {entry}",
            )
            require(
                int(start_number) <= int(end_number),
                f"{gap_id} contains a descending range {entry}",
            )
            width = max(len(start_number), len(end_number))
            candidates = [
                f"{start_prefix}-{value:0{width}d}"
                for value in range(int(start_number), int(end_number) + 1)
            ]
        else:
            candidates = [entry]
        for row_id in candidates:
            require(row_id in row_statuses, f"{gap_id} references unknown row {row_id}")
            if row_id not in seen:
                seen.add(row_id)
                expanded.append(row_id)
    require(expanded, f"{gap_id} claims completion without referencing any matrix rows")
    return tuple(expanded)


def validate_gap_completion_consistency(
    lines: list[str],
    rows: Sequence[tuple[str, str]],
    *,
    rules: dict[str, tuple[str, ...]] | None = None,
) -> None:
    active_rules = GAP_COMPLETION_RULES if rules is None else rules
    gap_rows = parse_gap_completion_tables(lines)
    row_statuses = dict(rows)
    for gap_id, forbidden_statuses in active_rules.items():
        require(gap_id in gap_rows, f"missing audited gap row {gap_id}")
        matrix_rows_cell, _notes = gap_rows[gap_id]
        referenced_rows = expand_gap_row_references(
            matrix_rows_cell, row_statuses, gap_id=gap_id
        )
        require(
            referenced_rows,
            f"{gap_id} claims completion without referencing any matrix rows",
        )
        contradicting = [
            (row_id, row_statuses[row_id])
            for row_id in referenced_rows
            if row_statuses[row_id] in forbidden_statuses
        ]
        require(
            not contradicting,
            (
                f"{gap_id} claims completion, but referenced rows remain one of "
                f"{', '.join(forbidden_statuses)}: "
                f"{preview_row_ids(contradicting)}"
            ),
        )


# The §23 ledger entry the cluster-status approval closes, and the §7 entry point
# the decision names. The ledger entry's Matrix rows cell is the ledger's own
# statement of scope: if it named fewer rows than the approval reaches, the rows
# outside it would sit in no ledger item at all while §25.3 reported the gap as
# closed by approval.
CLUSTER_STATUS_LEDGER_ENTRY = "SB-GAP-GO-003"
CLUSTER_STATUS_ENTRY_POINT_ROW = "SB-CONN-007"
CLUSTER_STATUS_DIVERGENCE_ID = "SB-DIV-016"


def validate_cluster_status_ledger_scope(
    lines: list[str], rows: Sequence[tuple[str, str]]
) -> None:
    """Require the cluster-status ledger entry to cover every row the approval reaches.

    The expected population is generated from the matrix rather than pinned as a
    literal, so adding a cluster-status row to §14 without widening the ledger
    entry is a failure rather than a silently narrower claim.
    """
    gap_rows = parse_gap_completion_tables(lines)
    require(
        CLUSTER_STATUS_LEDGER_ENTRY in gap_rows,
        f"§23 is missing the ledger entry {CLUSTER_STATUS_LEDGER_ENTRY}",
    )
    matrix_rows_cell, _notes = gap_rows[CLUSTER_STATUS_LEDGER_ENTRY]
    require_rendered_cell(
        matrix_rows_cell, f"{CLUSTER_STATUS_LEDGER_ENTRY}'s Matrix rows cell"
    )
    referenced = set(
        expand_gap_row_references(
            matrix_rows_cell, dict(rows), gap_id=CLUSTER_STATUS_LEDGER_ENTRY
        )
    )
    expected = {
        row_id
        for row_id, status in rows
        if row_id.startswith("SB-STAT-") and status != NOT_REQUIRED_STATUS
    } | {CLUSTER_STATUS_ENTRY_POINT_ROW}
    missing = sorted(expected - referenced)
    unexpected = sorted(referenced - expected)
    require(
        not missing and not unexpected,
        (
            f"{CLUSTER_STATUS_LEDGER_ENTRY} must scope itself to every cluster-status row the "
            f"approval reaches plus {CLUSTER_STATUS_ENTRY_POINT_ROW}; missing "
            f"{preview_row_ids(missing) or 'nothing'}, unexpected "
            f"{preview_row_ids(unexpected) or 'nothing'}"
        ),
    )
    approved = set(APPROVED_DIVERGENCES[CLUSTER_STATUS_DIVERGENCE_ID].rows)
    outside = sorted(approved - referenced)
    require(
        not outside,
        (
            f"{CLUSTER_STATUS_LEDGER_ENTRY} is the ledger entry "
            f"{CLUSTER_STATUS_DIVERGENCE_ID} closes, but approved rows are outside its "
            f"scope: {preview_row_ids(outside)}"
        ),
    )


def parse_divergence_table(lines: list[str]) -> dict[str, list[str]]:
    headers, table_rows = parse_markdown_table(lines, DIVERGENCE_TABLE_HEADING)
    require(
        headers == DIVERGENCE_TABLE_HEADERS,
        f"unexpected headers under {DIVERGENCE_TABLE_HEADING}: {headers}",
    )
    entries: dict[str, list[str]] = {}
    for cells in table_rows:
        entry_id = cells[0]
        require(
            entry_id.startswith("SB-DIV-"),
            f"unexpected divergence entry id in §26: {entry_id!r}",
        )
        require(entry_id not in entries, f"duplicate divergence entry {entry_id} in §26")
        entries[entry_id] = cells
    require(entries, "§26 records no divergence entries")
    return entries


def parse_approval_scope_cell(rows_cell: str) -> tuple[str, set[str]]:
    """Read the section prefix and the excluded rows out of a Rows cell.

    An approved Rows cell states its scope as "every `SB-PREFIX-*` row except A
    and B". Those two pieces are all the cell asserts; the population they apply
    to belongs to the matrix, not to this sentence, so it is read from the row
    inventory rather than guessed from the pin being checked.
    """
    match = APPROVAL_ROWS_CELL_SCOPE_RE.search(rows_cell)
    require(
        match is not None,
        (
            "an approved Rows cell must state its scope as \"every `SB-<PREFIX>-*` row "
            f"except ...\", found {rows_cell!r}"
        ),
    )
    assert match is not None
    prefix = match.group("prefix")
    excluded = set(ROW_ID_RE.findall(match.group("exclusions")))
    require(
        excluded,
        f"the Rows cell names no excluded rows after 'except': {rows_cell!r}",
    )
    off_prefix = sorted(row for row in excluded if not row.startswith(f"{prefix}-"))
    require(
        not off_prefix,
        (
            f"the Rows cell excludes {', '.join(off_prefix)} from the {prefix} population, "
            "but those rows are not part of it"
        ),
    )
    return prefix, excluded


def validate_approval_scope_cell(
    divergence_id: str,
    pins: "ApprovedDivergence",
    rows: Sequence[tuple[str, str]],
) -> None:
    """Hold the pinned row ids to the set the Rows cell describes.

    The cell says "every `SB-PREFIX-*` row except ...", so the set it names is
    the matrix's own `SB-PREFIX-*` population minus those exclusions. That
    population comes from the parsed row inventory, which
    `validate_revision_inventory` already pins against the audited manifest, so
    neither endpoint of the comparison is inferred from the pin under test:
    shifting the pinned window, substituting a row from another section, or
    claiming a row the cell excludes all fail here.
    """
    prefix, excluded = parse_approval_scope_cell(pins.rows_cell)
    population = set()
    for row_id, _status in rows:
        parsed = ROW_ID_PARTS_RE.fullmatch(row_id)
        if parsed is not None and parsed.group("prefix") == prefix:
            population.add(row_id)
    require(
        population,
        (
            f"{divergence_id} claims every {prefix} row, but the matrix has no {prefix} rows"
        ),
    )
    unknown_exclusions = sorted(excluded - population)
    require(
        not unknown_exclusions,
        (
            f"{divergence_id} excludes {', '.join(unknown_exclusions)}, which the matrix does "
            f"not carry in the {prefix} population"
        ),
    )
    collisions = sorted(set(pins.rows) & excluded)
    require(
        not collisions,
        (
            f"{divergence_id} pins {', '.join(collisions)} as approved, but its Rows cell "
            "excludes those rows"
        ),
    )
    described = population - excluded
    require(
        set(pins.rows) == described,
        (
            f"{divergence_id} pins a row set its Rows cell does not describe: the cell claims "
            f"every {prefix} row except {', '.join(sorted(excluded))}, which is "
            f"{len(described)} rows, but the pin differs at "
            f"{preview_row_ids(sorted(set(pins.rows) ^ described))}"
        ),
    )


def validate_divergence_approvals(
    lines: list[str],
    rows: Sequence[tuple[str, str]],
) -> None:
    entries = parse_divergence_table(lines)
    require(
        len(entries) == EXPECTED_DIVERGENCE_DECISION_COUNT,
        (
            f"§26 lists {len(entries)} decisions, but the audited table holds "
            f"{EXPECTED_DIVERGENCE_DECISION_COUNT}"
        ),
    )
    expected_decision_ids = set(EXPECTED_DIVERGENCE_DECISION_IDS)
    listed_decision_ids = set(entries)
    missing_decisions = sorted(expected_decision_ids - listed_decision_ids)
    unexpected_decisions = sorted(listed_decision_ids - expected_decision_ids)
    require(
        not missing_decisions and not unexpected_decisions,
        (
            "§26 does not list the audited decision identifiers: missing "
            f"{', '.join(missing_decisions) or 'nothing'}; unexpected "
            f"{', '.join(unexpected_decisions) or 'nothing'}"
        ),
    )

    for divergence_id, pins in APPROVED_DIVERGENCES.items():
        require(divergence_id in entries, f"§26 is missing approved divergence {divergence_id}")
        cells = entries[divergence_id]
        # The approver and the date are the whole of the approval state §26
        # exists to record. Commenting out either one on both sides still
        # compares equal while the table renders blank, so both are checked for
        # rendered text before they are compared.
        require_rendered_cell(cells[4], f"{divergence_id}'s Approver cell")
        require_rendered_cell(pins.approver, f"{divergence_id}'s pinned approver")
        require_rendered_cell(cells[5], f"{divergence_id}'s Date cell")
        require_rendered_cell(pins.date, f"{divergence_id}'s pinned approval date")
        require(
            cells[4] == pins.approver,
            (
                f"{divergence_id} approver is {cells[4]!r}, but the pinned approval names "
                f"{pins.approver!r}"
            ),
        )
        require(
            cells[5] == pins.date,
            (
                f"{divergence_id} approval date is {cells[5]!r}, but the pinned approval is "
                f"dated {pins.date!r}"
            ),
        )
        # The evidence must be the whole destination of a link, not a substring of
        # one: `…#issuecomment-5343583850-and-then-some` contains the pinned URL
        # but resolves to a different anchor, so a substring test would let the
        # dated approval cite something other than the comment that granted it.
        # The link must also render, and render something visible: a code span,
        # a raw `<code>` block or an invisible label all cite nothing a reader
        # can follow.
        evidence_targets = extract_markdown_link_targets(cells[3])
        require(
            pins.evidence in evidence_targets,
            (
                f"{divergence_id} does not cite its approval evidence {pins.evidence} as the "
                f"target of a Markdown link; the cell links to {sorted(evidence_targets)}"
            ),
        )
        # The Rows cell is the normative statement of what was approved. Left
        # unpinned it could claim a different set while every other check here
        # still passed, so it is compared whole against the audited scope.
        require_rendered_cell(cells[2], f"{divergence_id}'s Rows cell")
        require_rendered_cell(pins.rows_cell, f"{divergence_id}'s pinned Rows cell")
        require(
            cells[2] == pins.rows_cell,
            (
                f"{divergence_id} declares its approved rows as {cells[2]!r}, but the audited "
                f"approval scope is {pins.rows_cell!r}; widening or narrowing an approval needs "
                "a new decision on the tracking issue, not an edit here"
            ),
        )
        require(
            len(pins.rows) == len(set(pins.rows)),
            f"{divergence_id} pins a duplicated approved row",
        )
        if divergence_id == "SB-DIV-016":
            validate_approval_scope_cell(divergence_id, pins, rows)
        # The decision table is where approval state lives, so it must also carry
        # what was approved. The behavior checks above are scoped to the section
        # the decision governs; without this the row that records the approval
        # could drop or reverse every clause and still pass.
        require_rendered_cell(cells[3], f"{divergence_id}'s Impact cell")
        require_rendered_cell(pins.impact_cell, f"{divergence_id}'s pinned Impact cell")
        normalized_impact = normalize_whitespace(cells[3])
        for clause in pins.behavior_clauses:
            require(
                normalize_whitespace(clause) in normalized_impact,
                (
                    f"{divergence_id} does not state the approved behavior it records, "
                    f"in the decision's own terms: {clause}"
                ),
            )
        normalized_pin = normalize_whitespace(pins.impact_cell)
        # Guards the pins against each other, and runs before the whole-cell
        # compare so that a narrowed pin is reported as a pin defect rather than
        # as a document mismatch: dropping a clause from the pinned cell would
        # otherwise silently narrow what the whole-cell compare enforces.
        for clause in pins.behavior_clauses:
            require(
                normalize_whitespace(clause) in normalized_pin,
                (
                    f"{divergence_id}'s pinned decision text no longer states the pinned "
                    f"approved behavior: {clause}"
                ),
            )
        # Containment alone cannot detect a contradiction: the clause can stay as
        # historical text with its opposite appended after it. The cell is the
        # normative approval record, so it is compared whole.
        require(
            normalized_impact == normalized_pin,
            (
                f"{divergence_id} states its approved decision as {normalized_impact!r}, but "
                f"the audited approval reads {normalized_pin!r}; changing what was approved "
                "needs a new decision on the tracking issue, not an edit here"
            ),
        )

    for divergence_id, subsumed in SUBSUMED_DIVERGENCES.items():
        covering_id = subsumed.covering
        require(divergence_id in entries, f"§26 is missing subsumed divergence {divergence_id}")
        cells = entries[divergence_id]
        # Dormancy is granted to the concern the entry described, not to the
        # entry's identifier. An unpinned Rows cell could be repointed at a row
        # the approval never covered, which would silently extend the approval.
        require_rendered_cell(cells[2], f"{divergence_id}'s Rows cell")
        require_rendered_cell(subsumed.rows_cell, f"{divergence_id}'s pinned Rows cell")
        require(
            cells[2] == subsumed.rows_cell,
            (
                f"{divergence_id} declares its subsumed rows as {cells[2]!r}, but the audited "
                f"scope {covering_id} subsumes is {subsumed.rows_cell!r}; a subsumed entry "
                "cannot be repointed at another row without a new decision"
            ),
        )
        covering_rows = set(APPROVED_DIVERGENCES[covering_id].rows)
        outside = sorted(set(subsumed.rows) - covering_rows)
        require(
            not outside,
            (
                f"{divergence_id} is subsumed by {covering_id} but names matrix rows outside "
                f"that approval's scope: {preview_row_ids(outside)}"
            ),
        )
        matrix_row_ids = {row_id for row_id, _ in rows}
        named_matrix_rows = {
            token
            for token in ROW_ID_RE.findall(subsumed.rows_cell)
            if token in matrix_row_ids
        }
        require(
            named_matrix_rows == set(subsumed.rows),
            (
                f"{divergence_id} pins {sorted(subsumed.rows)} as the matrix rows it subsumes, "
                f"but its Rows cell names {sorted(named_matrix_rows)}"
            ),
        )
        require(
            covering_id in cells[3],
            f"{divergence_id} does not name the approved decision {covering_id} that subsumes it",
        )
        require_rendered_cell(cells[3], f"{divergence_id}'s Impact cell")
        require_rendered_cell(
            subsumed.impact_cell, f"{divergence_id}'s pinned Impact cell"
        )
        normalized_subsumed_pin = normalize_whitespace(subsumed.impact_cell)
        # Runs before the whole-cell compare so that a narrowed pin is reported
        # as a pin defect rather than as a document mismatch.
        require(
            covering_id in normalized_subsumed_pin,
            (
                f"{divergence_id}'s pinned decision text no longer names {covering_id} as the "
                "approval that subsumes it, so the pin has been narrowed"
            ),
        )
        normalized_subsumed_impact = normalize_whitespace(cells[3])
        # Containment alone cannot detect a contradiction: the covering
        # identifier can stay in place while an appended sentence revives the
        # entry as blocking. The cell is the normative record, so it is compared
        # whole.
        require(
            normalized_subsumed_impact == normalized_subsumed_pin,
            (
                f"{divergence_id}'s decision text no longer matches the audited subsumption "
                "word for word; it is pinned whole because naming the covering decision and "
                "then reversing the subsumption would satisfy every containment check. "
                f"Expected:\n{normalized_subsumed_pin}\nFound:\n{normalized_subsumed_impact}"
            ),
        )
        expected_approver = SUBSUMED_DIVERGENCE_APPROVER.format(covering=covering_id)
        require_rendered_cell(cells[4], f"{divergence_id}'s Approver cell")
        require_rendered_cell(expected_approver, f"{divergence_id}'s expected approver")
        require(
            cells[4] == expected_approver,
            (
                f"{divergence_id} is subsumed by {covering_id} and must record that it is not "
                f"separately approved as exactly {expected_approver!r}, found {cells[4]!r}"
            ),
        )
        expected_date = SUBSUMED_DIVERGENCE_DATE.format(
            date=APPROVED_DIVERGENCES[covering_id].date.strip("*")
        )
        require_rendered_cell(cells[5], f"{divergence_id}'s Date cell")
        require_rendered_cell(expected_date, f"{divergence_id}'s expected approval date")
        require(
            cells[5] == expected_date,
            (
                f"{divergence_id} is subsumed by {covering_id} and must carry the covering "
                f"decision's date as exactly {expected_date!r}, found {cells[5]!r}"
            ),
        )

    for divergence_id, cells in entries.items():
        if divergence_id in APPROVED_DIVERGENCES or divergence_id in SUBSUMED_DIVERGENCES:
            continue
        # An unapproved entry renders "not approved" only if the marker reaches
        # the reader; commenting the cell and the constant out together leaves
        # the comparison equal while §26 shows a blank approval state.
        require_rendered_cell(cells[4], f"{divergence_id}'s Approver cell")
        require_rendered_cell(
            UNAPPROVED_DIVERGENCE_APPROVER, "the unapproved-divergence approver marker"
        )
        require_rendered_cell(cells[5], f"{divergence_id}'s Date cell")
        require_rendered_cell(
            UNAPPROVED_DIVERGENCE_DATE, "the unapproved-divergence date marker"
        )
        require(
            cells[4] == UNAPPROVED_DIVERGENCE_APPROVER,
            (
                f"{divergence_id} claims approver {cells[4]!r}; an approval must also be pinned "
                "in APPROVED_DIVERGENCES in the same change"
            ),
        )
        require(
            cells[5] == UNAPPROVED_DIVERGENCE_DATE,
            f"{divergence_id} is unapproved but carries the date {cells[5]!r}",
        )

    row_statuses = dict(rows)
    for row_id, expected_status in EXPECTED_DIVERGENCE_EXCLUDED_ROWS.items():
        actual = row_statuses.get(row_id)
        require(
            actual == expected_status,
            (
                f"{row_id} is excluded from the approved cluster-status divergence and must stay "
                f"{expected_status!r}, found {actual!r}"
            ),
        )

    pinned_approved_rows: set[str] = set()
    for divergence_id, pins in APPROVED_DIVERGENCES.items():
        collision = sorted(pinned_approved_rows & set(pins.rows))
        require(
            not collision,
            f"{divergence_id} claims rows another approval already covers: "
            f"{preview_row_ids(collision)}",
        )
        pinned_approved_rows.update(pins.rows)
    contradiction = sorted(pinned_approved_rows & set(EXPECTED_DIVERGENCE_EXCLUDED_ROWS))
    require(
        not contradiction,
        (
            "rows are pinned as both approved and excluded from the approval: "
            f"{preview_row_ids(contradiction)}"
        ),
    )

    divergence_rows = {
        row_id for row_id, status in rows if status == INTENTIONAL_DIVERGENCE_STATUS
    }
    unapproved_rows = set(EXPECTED_UNAPPROVED_DIVERGENCE_ROWS)
    missing_unapproved = sorted(unapproved_rows - divergence_rows)
    require(
        not missing_unapproved,
        (
            "rows pinned as unapproved divergences no longer carry that status: "
            f"{preview_row_ids(missing_unapproved)}"
        ),
    )

    approved_rows = divergence_rows - unapproved_rows
    require(
        approved_rows == pinned_approved_rows,
        (
            "the approved cluster-status divergence row set drifted: "
            f"{preview_row_ids(sorted(approved_rows ^ pinned_approved_rows))}"
        ),
    )
    require(
        len(pinned_approved_rows) == EXPECTED_APPROVED_DIVERGENCE_ROW_COUNT,
        (
            f"the pinned approvals cover {len(pinned_approved_rows)} rows, but the audited "
            f"decision covers {EXPECTED_APPROVED_DIVERGENCE_ROW_COUNT}"
        ),
    )

    behavior_section = extract_section_body(lines, APPROVAL_BEHAVIOR_SECTION_HEADING)
    require(
        bool(behavior_section),
        (
            "the section that must carry the approved divergence's normative behavior is "
            f"missing: {APPROVAL_BEHAVIOR_SECTION_HEADING}"
        ),
    )
    normalized_behavior_section = normalize_whitespace("\n".join(behavior_section))
    behavior_fragments = list(EXPECTED_APPROVAL_BEHAVIOR_SNIPPETS)
    for snippet in EXPECTED_APPROVAL_BEHAVIOR_SNIPPETS:
        require(
            snippet in normalized_behavior_section,
            (
                "the approved divergence no longer states its normative behavior in "
                f"{APPROVAL_BEHAVIOR_SECTION_LABEL}: {snippet}"
            ),
        )
    excluded_section_rows = sorted(
        row_id
        for row_id in EXPECTED_DIVERGENCE_EXCLUDED_ROWS
        if row_id.startswith("SB-STAT-")
    )
    require(
        len(excluded_section_rows) == 2,
        (
            "the approved divergence's scope sentence names exactly two excluded "
            f"{APPROVAL_BEHAVIOR_SECTION_LABEL} rows, but "
            f"{len(excluded_section_rows)} are pinned: {excluded_section_rows}"
        ),
    )
    section_approvals = sorted(
        divergence_id
        for divergence_id, pins in APPROVED_DIVERGENCES.items()
        if any(row_id.startswith("SB-STAT-") for row_id in pins.rows)
    )
    require(
        len(section_approvals) == 1,
        (
            f"exactly one approved divergence may govern {APPROVAL_BEHAVIOR_SECTION_LABEL}, "
            f"found {len(section_approvals)}: {section_approvals}"
        ),
    )
    if len(excluded_section_rows) == 2 and len(section_approvals) == 1:
        scope_prose = APPROVAL_SCOPE_PROSE.format(
            decision=section_approvals[0],
            count=len(CLUSTER_STATUS_APPROVED_ROWS),
            first=excluded_section_rows[0],
            second=excluded_section_rows[1],
        )
        require(
            scope_prose in normalized_behavior_section,
            (
                f"{APPROVAL_BEHAVIOR_SECTION_LABEL} must state the approval's row scope in "
                f"the approval's own terms, naming the excluded rows: {scope_prose}"
            ),
        )
        permanence_prose = APPROVAL_PERMANENCE_PROSE.format(
            count=len(CLUSTER_STATUS_APPROVED_ROWS),
            decision=section_approvals[0],
        )
        require(
            permanence_prose in normalized_behavior_section,
            (
                f"{APPROVAL_BEHAVIOR_SECTION_LABEL} must bind the permanent divergence "
                f"status to the approved rows rather than to the whole section: "
                f"{permanence_prose}"
            ),
        )
        behavior_fragments += [scope_prose, permanence_prose]
    # Every fragment above is a containment test, and containment cannot detect a
    # contradiction: all of them can hold while a later sentence reverses them.
    # §14 is explicitly normative, so its approval block is pinned whole. The pin
    # is checked against the fragments first, so narrowing the pin is reported as
    # a pin defect rather than as a document mismatch.
    expected_behavior_preamble = APPROVAL_BEHAVIOR_PREAMBLE.format(
        revision=CLUSTER_STATUS_APPROVAL_REVISION
    )
    for fragment in behavior_fragments:
        require(
            fragment in expected_behavior_preamble,
            (
                "APPROVAL_BEHAVIOR_PREAMBLE no longer states something "
                f"{APPROVAL_BEHAVIOR_SECTION_LABEL} is required to say, so the pin has been "
                f"narrowed: {fragment}"
            ),
        )
    behavior_preamble = extract_section_preamble(lines, APPROVAL_BEHAVIOR_SECTION_HEADING)
    require(
        bool(behavior_preamble),
        (
            "the section that must carry the approved divergence's normative behavior has no "
            f"prose above its table: {APPROVAL_BEHAVIOR_SECTION_HEADING}"
        ),
    )
    require_rendered_prose(behavior_preamble, APPROVAL_BEHAVIOR_SECTION_LABEL)
    normalized_behavior_preamble = normalize_whitespace("\n".join(behavior_preamble))
    require(
        normalized_behavior_preamble == expected_behavior_preamble,
        (
            f"{APPROVAL_BEHAVIOR_SECTION_LABEL}'s approval block no longer matches the "
            "approved decision word for word; it is pinned whole because retaining every "
            "required sentence and appending its opposite would satisfy every containment "
            f"check. Expected:\n{expected_behavior_preamble}\n"
            f"Found:\n{normalized_behavior_preamble}"
        ),
    )
    decision_preamble = extract_section_preamble(lines, DIVERGENCE_DECISION_SECTION_HEADING)
    require(
        bool(decision_preamble),
        (
            "the section that must introduce the divergence decisions is missing or has no "
            f"prose above its table: {DIVERGENCE_DECISION_SECTION_HEADING}"
        ),
    )
    require_rendered_prose(decision_preamble, DIVERGENCE_DECISION_SECTION_LABEL)
    normalized = normalize_whitespace("\n".join(decision_preamble))
    decision_fragments: list[str] = []
    for divergence_id, subsumed in SUBSUMED_DIVERGENCES.items():
        dormant_prose = SUBSUMED_DIVERGENCE_GATE_PROSE.format(
            divergence=divergence_id, covering=subsumed.covering
        )
        require(
            dormant_prose in normalized,
            (
                f"§26's preamble does not carve {divergence_id} out of the blocking entries: "
                f"{dormant_prose}"
            ),
        )
        decision_fragments.append(dormant_prose)
    proposed_count = len(entries) - len(APPROVED_DIVERGENCES) - len(SUBSUMED_DIVERGENCES)
    split_prose = DIVERGENCE_DECISION_SPLIT_PROSE.format(
        total=len(entries),
        approved=len(APPROVED_DIVERGENCES),
        subsumed=len(SUBSUMED_DIVERGENCES),
        proposed=proposed_count,
    )
    require(
        split_prose in normalized,
        f"§26 does not state the decision split it lists: {split_prose}",
    )
    decision_fragments.append(split_prose)
    carve_out_prose = PROPOSED_DIVERGENCE_CARVE_OUT_PROSE.format(
        proposed=proposed_count,
        approved_ids=", ".join(f"[{name}](#sec-26)" for name in sorted(APPROVED_DIVERGENCES)),
        subsumed_ids=", ".join(f"[{name}](#sec-26)" for name in sorted(SUBSUMED_DIVERGENCES)),
    )
    require(
        carve_out_prose in normalized,
        (
            "§26 describes entries as proposed without excluding the approved and subsumed "
            f"decisions: {carve_out_prose}"
        ),
    )
    decision_fragments.append(carve_out_prose)
    # Collect the rows as well as the decisions. Listing only the decisions that
    # intersect the unapproved rows would still pass if a decision stopped naming
    # one of them while continuing to name another: SB-DIV-002 covers both
    # SB-CFG-014 and SB-CFG-022, so dropping either leaves the decision matched.
    # The prose claims all of these rows are already carried by a proposal, so
    # the union of the rows the proposals name has to cover them.
    proposals_over_unapproved_rows = []
    rows_named_by_proposals: set[str] = set()
    for divergence_id, cells in sorted(entries.items()):
        if divergence_id in APPROVED_DIVERGENCES or divergence_id in SUBSUMED_DIVERGENCES:
            continue
        named = unapproved_rows & set(ROW_ID_RE.findall(cells[2]))
        if not named:
            continue
        proposals_over_unapproved_rows.append(divergence_id)
        rows_named_by_proposals |= named
    unnamed_rows = unapproved_rows - rows_named_by_proposals
    require(
        not unnamed_rows,
        (
            f"§26 says the {len(unapproved_rows)} unapproved divergence rows are named by its "
            f"proposals, but {', '.join(sorted(unnamed_rows))} is named by no proposed decision, "
            "so there is nothing to approve that would lift the gate on it"
        ),
    )
    # A proposal names the scope a maintainer is being asked to decide on. Left
    # unpinned it could be repointed at unrelated rows under the same ID, and
    # every later check would still pass whenever the substituted rows happen
    # not to intersect the unapproved rows tracked above.
    proposed_ids = set(entries) - set(APPROVED_DIVERGENCES) - set(SUBSUMED_DIVERGENCES)
    for divergence_id in sorted(proposed_ids):
        expected_cell = EXPECTED_PROPOSED_DIVERGENCE_ROW_CELLS.get(divergence_id)
        require(
            expected_cell is not None,
            (
                f"{divergence_id} is a proposed decision with no pinned Rows cell; add it to "
                "EXPECTED_PROPOSED_DIVERGENCE_ROW_CELLS with the audited scope"
            ),
        )
        if expected_cell is not None:
            require_rendered_cell(
                entries[divergence_id][2], f"{divergence_id}'s proposed Rows cell"
            )
            require_rendered_cell(
                expected_cell, f"{divergence_id}'s pinned proposed Rows cell"
            )
            require(
                entries[divergence_id][2] == expected_cell,
                (
                    f"{divergence_id} declares its rows as {entries[divergence_id][2]!r}, but "
                    f"the audited proposal covers {expected_cell!r}; repointing a proposal "
                    "needs a new entry, not an edit to an existing one"
                ),
            )
    stale_proposal_pins = sorted(set(EXPECTED_PROPOSED_DIVERGENCE_ROW_CELLS) - proposed_ids)
    require(
        not stale_proposal_pins,
        (
            "proposed decisions are pinned that §26 no longer lists as proposed: "
            f"{stale_proposal_pins}"
        ),
    )
    if unapproved_rows:
        effect_prose = PROPOSED_DIVERGENCE_EFFECT_PROSE.format(
            already=len(unapproved_rows),
            status=INTENTIONAL_DIVERGENCE_STATUS,
            already_ids=", ".join(proposals_over_unapproved_rows),
        )
        require(
            effect_prose in normalized,
            (
                "§26 claims every proposal reclassifies a row, but the unapproved rows already "
                f"carry the status: {effect_prose}"
            ),
        )
        decision_fragments.append(effect_prose)
    approved_prose = (
        f"**{len(divergence_rows)} matrix rows currently carry the "
        f"`{INTENTIONAL_DIVERGENCE_STATUS}` status: {len(approved_rows)} are approved and "
        f"{len(unapproved_rows)} are not.**"
    )
    require(
        approved_prose in normalized,
        f"§26 does not reconcile approved and unapproved divergences: {approved_prose}",
    )
    decision_fragments.append(approved_prose)
    # Same reasoning as §14: the preamble is normative and generated from the
    # parsed table, so retaining every generated sentence and appending a claim
    # that contradicts the table would pass every check above. Pin it whole,
    # after checking that the pin still contains everything it must guarantee.
    for fragment in decision_fragments:
        require(
            fragment in DIVERGENCE_DECISION_PREAMBLE,
            (
                "DIVERGENCE_DECISION_PREAMBLE no longer states something "
                f"{DIVERGENCE_DECISION_SECTION_LABEL} is required to say, so the pin has "
                f"been narrowed: {fragment}"
            ),
        )
    require(
        normalized == DIVERGENCE_DECISION_PREAMBLE,
        (
            f"{DIVERGENCE_DECISION_SECTION_LABEL}'s preamble no longer matches the audited "
            "text; it is pinned whole because retaining every generated sentence and "
            "appending a contradiction would satisfy every containment check. Expected:\n"
            f"{DIVERGENCE_DECISION_PREAMBLE}\nFound:\n{normalized}"
        ),
    )


def validate_manifest_citations(full_text: str) -> None:
    """Keep the document's manifest citations pointing at files that exist.

    A revision bump renames both manifests, so any citation left on the previous
    revision's filename becomes a dangling normative evidence link. The names are
    derived from EXPECTED_REVISION, so this check moves with it automatically.
    """
    expected = {
        "rows": EXPECTED_ROW_MANIFEST.name,
        "cabi-symbols": EXPECTED_C_ABI_SYMBOL_MANIFEST.name,
    }
    for match in MANIFEST_CITATION_RE.finditer(full_text):
        cited = match.group(0).removeprefix("docs/")
        require(
            cited == expected[match.group("kind")],
            (
                f"the document cites the manifest docs/{cited}, but revision "
                f"{EXPECTED_REVISION} ships docs/{expected[match.group('kind')]}; a revision "
                "bump renames the manifests, so every citation has to move with them"
            ),
        )
    require(
        f"docs/{EXPECTED_SCOPE_MANIFEST.name}" in full_text,
        f"the document does not cite the revision-{EXPECTED_REVISION} scope manifest",
    )


def validate_counts(lines: list[str], full_text: str) -> None:
    metadata = parse_metadata(lines)
    for field, expected_value in EXPECTED_METADATA_FIELDS.items():
        actual = metadata.get(field)
        require(actual == expected_value, f"unexpected metadata value for {field}: {actual!r}")
    document_status = metadata.get("Document status", "")
    for snippet in EXPECTED_DOCUMENT_STATUS_SNIPPETS:
        require(
            snippet in document_status,
            f"document status is missing expected detail: {snippet}",
        )
    validate_manifest_citations(full_text)

    status_counts, prefix_counts, rows = parse_rows(lines)
    total_rows = len(rows)
    require(sum(status_counts.values()) == total_rows, f"expected {total_rows} rows, found {sum(status_counts.values())}")
    validate_revision_inventory(rows, status_counts, prefix_counts)
    scope_counts = validate_scope_manifest(rows)
    validate_gap_completion_consistency(lines, rows)
    validate_divergence_approvals(lines, rows)
    validate_cluster_status_ledger_scope(lines, rows)

    metadata_total_rows, metadata_required_rows = parse_rows_metadata(metadata.get("Rows", ""))
    require(
        metadata_total_rows == total_rows,
        f"Rows metadata says {metadata_total_rows}, but parsed {total_rows} unique rows",
    )
    covered_rows = parse_covered_rows_metadata(metadata.get("Covered rows", ""))
    require(
        covered_rows == status_counts["Covered"],
        f"Covered rows metadata says {covered_rows}, but parsed {status_counts['Covered']}",
    )
    required_rows = (
        scope_counts["Covered"]
        + scope_counts["Approved divergence"]
        + scope_counts["Required gap"]
    )
    require(
        metadata_required_rows == required_rows,
        f"Rows metadata says {metadata_required_rows} required rows, but parsed {required_rows}",
    )

    declared_status_counts, declared_status_total = parse_status_summary(lines)
    require(
        declared_status_total == total_rows,
        f"status summary total says {declared_status_total}, but parsed {total_rows}",
    )
    for status in sorted(STATUSES):
        actual = status_counts[status]
        declared = declared_status_counts[status]
        require(
            declared == actual,
            f"status summary says {declared} rows for {status}, but parsed {actual}",
        )

    (
        declared_prefix_counts,
        declared_prefix_totals,
        declared_category_status_totals,
        declared_category_total,
    ) = parse_category_summary(lines)
    require(
        declared_category_total == total_rows,
        f"category summary total says {declared_category_total}, but parsed {total_rows}",
    )
    require(
        set(declared_prefix_counts) == set(prefix_counts),
        (
            "category summary prefixes do not match parsed prefixes: "
            f"{sorted(declared_prefix_counts)} vs {sorted(prefix_counts)}"
        ),
    )
    for prefix in sorted(prefix_counts):
        parsed_total = sum(prefix_counts[prefix].values())
        require(
            declared_prefix_totals[prefix] == parsed_total,
            f"category summary says {declared_prefix_totals[prefix]} rows for {prefix}, but parsed {parsed_total}",
        )
        for status in sorted(STATUSES):
            actual = prefix_counts[prefix][status]
            declared = declared_prefix_counts[prefix][status]
            require(
                declared == actual,
                f"category summary says {declared} rows for {prefix} / {status}, but parsed {actual}",
            )
    for status in sorted(STATUSES):
        declared = declared_category_status_totals[status]
        actual = status_counts[status]
        require(
            declared == actual,
            f"category summary total says {declared} rows for {status}, but parsed {actual}",
        )

    validate_status_narratives(lines, status_counts, prefix_counts, scope_counts)
    validate_c_abi_symbol_inventory(full_text)
    validate_c_abi_free_inventory(full_text)


def validate_status_narratives(
    lines: Sequence[str],
    status_counts: Counter[str],
    prefix_counts: dict[str, Counter[str]],
    scope_counts: Counter[str],
) -> None:
    total_rows = sum(status_counts.values())
    required_rows = (
        scope_counts["Covered"]
        + scope_counts["Approved divergence"]
        + scope_counts["Required gap"]
    )
    approved_divergences = scope_counts["Approved divergence"]
    satisfied = scope_counts["Covered"] + approved_divergences

    expected_phrases = [
        (RELEASE_GATE_SECTION_HEADING, f"As of revision {EXPECTED_REVISION}, **{required_rows} rows are required, {satisfied} are satisfied, and {scope_counts['Required gap']} remain**."),
        (COUNTS_SECTION_HEADING, f"The normative scope manifest classifies {required_rows} rows as required, {scope_counts['Optional']} as optional, and {scope_counts['Not required']} as not required."),
        (COUNTS_SECTION_HEADING, f"**Exactly {status_counts['Covered']} rows are `Covered`: [SB-XCUT-012](#sec-20), the twelve configuration/topology rows completed in revision 24, the 31 RFile/stream rows completed in revision 25, the 17 data-model value rows completed in revision 26, the five buffered-writer rows completed in revisions 28 and 45, the four row-bounded flush/constraint rows completed in revision 29, the connector invalidation/cancellation rows completed in revision 30, the eight high-level client rows completed in revision 31, the five high-level scanner rows completed in revision 32, the four compatibility-error rows completed in revision 34, the twelve streaming cursor rows completed in revision 36, the 31 column-visibility rows completed in revision 38, the 22 equivalent owned-key rows completed in revision 42, the named-locality-group RFile row plus 24 HDFS rows completed in revision 44, the two logging rows completed in revision 45, the 38 table/namespace/security rows completed in revision 48, the twelve issue-196 cross-cutting rows completed in revision 49, the 60 required Python data-model and iterator-descriptor rows completed in revision 50, the five package/import rows completed in revision 51, the eleven RFile/HDFS/logging/error rows completed in revision 52, nineteen Python-visible scanner/writer rows completed in revision 53, the 28 configuration/credential/connector/session/dynamic-attribute rows completed in revision 54, and the controlled Linux manylinux release row completed in revision 56; revision 55 changes scope only; SB-WRITE-010 is the separately approved SB-DIV-005 divergence.**"),
        (COUNTS_SECTION_HEADING, f"`Intentional divergence` ({status_counts[INTENTIONAL_DIVERGENCE_STATUS]}) is dominated by one upstream fact: {prefix_counts['SB-STAT'][INTENTIONAL_DIVERGENCE_STATUS]} rows are cluster-status accessors Accumulo itself deleted ([§14](#sec-14), [SB-DIV-016](#sec-26))."),
        (COUNTS_SECTION_HEADING, "`SB-XCUT-014`, `SB-XCUT-019`, and `SB-PKG-008` remain explicit optional/deferred"),
    ]
    section_text: dict[str, str] = {}
    for heading, phrase in expected_phrases:
        if heading not in section_text:
            body = extract_section_body(lines, heading)
            require(
                bool(body),
                f"the section that must carry a normative status narrative is missing: {heading}",
            )
            require_rendered_prose(body, heading)
            section_text[heading] = normalize_whitespace("\n".join(body))
        require(
            phrase in section_text[heading],
            f"missing or stale status narrative in {heading}: {phrase}",
        )


def validate_local_line_number_removal(full_text: str) -> None:
    for path in TARGETED_LOCAL_CITATIONS:
        require(
            re.search(rf"`{re.escape(path)}:\d", full_text) is None,
            f"line-number citation remains for {path}",
        )


def normalize_file_citation(span: str) -> str:
    return span.split(":", 1)[0]


def is_file_citation(span: str) -> bool:
    path = normalize_file_citation(span)
    if path in FILE_CITATION_BASENAMES:
        return True
    if path.endswith("/**") or any(marker in path for marker in ("{", "}", "*", "?")):
        return "/" in path
    suffix = Path(path).suffix.lower()
    return suffix in FILE_CITATION_SUFFIXES


def load_targeted_contents(
    targeted_paths: set[str],
    repo_root: Path | None = None,
) -> dict[str, str]:
    root = repo_root or DOC_PATH.parent.parent
    return {
        path: (root / path).read_text(encoding="utf-8", errors="ignore")
        for path in targeted_paths
    }


def is_anchor_glue(text: str) -> bool:
    words = re.findall(r"[A-Za-z]+", text)
    if any(word not in {"and", "or", "s"} for word in words):
        return False
    remainder = re.sub(r"[A-Za-z]+", "", text)
    return all(character in " \t`,;/+&()-" for character in remainder)


def is_file_group_glue(text: str) -> bool:
    return all(character in " \t`,;()" for character in text)


def preceding_anchor_groups(
    spans: list[tuple[str, int, int]],
    start_index: int,
    cell: str,
) -> list[list[str]]:
    candidate_indices: list[int] = []
    previous_index = start_index - 1
    while previous_index >= 0:
        previous_span, _previous_start, _previous_end = spans[previous_index]
        if previous_span.startswith("SB-") or previous_span in {"n/a", "—"}:
            break
        if is_file_citation(previous_span):
            break
        candidate_indices.insert(0, previous_index)
        previous_index -= 1

    if not candidate_indices:
        return []

    groups: list[list[str]] = []
    current_group = [spans[candidate_indices[0]][0]]
    for earlier_index, later_index in zip(candidate_indices, candidate_indices[1:]):
        glue = cell[spans[earlier_index][2]:spans[later_index][1]]
        if is_anchor_glue(glue):
            current_group.append(spans[later_index][0])
            continue
        groups.append(current_group)
        current_group = [spans[later_index][0]]
    groups.append(current_group)
    return groups


def extract_path_anchor_bindings(cell: str, targeted_paths: set[str]) -> dict[str, list[str]]:
    bindings: dict[str, list[str]] = defaultdict(list)
    spans = [
        (match.group(1), match.start(1), match.end(1))
        for match in CODE_SPAN_RE.finditer(cell)
    ]

    index = 0
    while index < len(spans):
        span, _start, _end = spans[index]
        if not is_file_citation(span):
            index += 1
            continue

        group_indices = [index]
        lookahead = index + 1
        while (
            lookahead < len(spans)
            and is_file_citation(spans[lookahead][0])
            and is_file_group_glue(cell[spans[lookahead - 1][2]:spans[lookahead][1]])
        ):
            group_indices.append(lookahead)
            lookahead += 1

        anchor_groups = preceding_anchor_groups(spans, group_indices[0], cell)
        assignments: list[list[str]]
        if not anchor_groups:
            assignments = [[] for _ in group_indices]
        elif len(group_indices) == 1:
            assignments = [anchor_groups[-1]]
        elif len(anchor_groups) == 1:
            assignments = anchor_groups * len(group_indices)
        elif len(anchor_groups) >= len(group_indices):
            assignments = anchor_groups[-len(group_indices):]
        else:
            assignments = [[] for _ in group_indices]

        for group_index, anchors in zip(group_indices, assignments):
            path = normalize_file_citation(spans[group_index][0])
            if path in targeted_paths:
                bindings[path].extend(anchors)

        index = lookahead

    return dict(bindings)


@lru_cache(maxsize=None)
def identifier_boundary_pattern(token: str) -> re.Pattern[str]:
    return re.compile(
        rf"(?<![{IDENTIFIER_BOUNDARY_CLASS}]){re.escape(token)}(?![{IDENTIFIER_BOUNDARY_CLASS}])",
        re.DOTALL,
    )


@lru_cache(maxsize=None)
def anchor_boundary_pattern(anchor: str) -> re.Pattern[str]:
    pieces: list[str] = []
    for part in ANCHOR_PART_RE.findall(anchor):
        if part == "...":
            pieces.append(r"[\s\S]*?")
            continue
        if part.isspace():
            pieces.append(r"\s+")
            continue
        if IDENT_RE.fullmatch(part):
            pieces.append(identifier_boundary_pattern(part).pattern)
            continue
        pieces.append(re.escape(part))
        if part in WHITESPACE_TOLERANT_PUNCTUATION:
            pieces.append(r"\s*")
    return re.compile("".join(pieces), re.DOTALL)


def significant_anchor_identifiers(anchor: str, ignored_tokens: set[str]) -> list[str]:
    seen: set[str] = set()
    significant: list[str] = []
    for token in IDENT_RE.findall(anchor):
        if token in ignored_tokens or token in seen:
            continue
        seen.add(token)
        significant.append(token)
    return significant


@lru_cache(maxsize=None)
def declaration_constructs(content: str) -> tuple[str, ...]:
    constructs: list[str] = []
    seen: set[str] = set()
    for pattern in DECLARATION_PATTERNS:
        for match in pattern.finditer(content):
            construct = match.group(0).strip()
            if construct and construct not in seen:
                constructs.append(construct)
                seen.add(construct)
    if not constructs:
        constructs = [line.strip() for line in content.splitlines() if line.strip()]
    return tuple(constructs)


@lru_cache(maxsize=None)
def ordered_identifier_pattern(tokens: tuple[str, ...]) -> re.Pattern[str]:
    pieces = [identifier_boundary_pattern(tokens[0]).pattern]
    for token in tokens[1:]:
        pieces.append(r"[\s\S]*?")
        pieces.append(identifier_boundary_pattern(token).pattern)
    return re.compile("".join(pieces), re.DOTALL)


def compound_anchor_matches_construct(anchor: str, content: str, ignored_tokens: set[str]) -> bool:
    tokens = significant_anchor_identifiers(anchor, ignored_tokens)
    if len(tokens) <= 1:
        return False
    pattern = ordered_identifier_pattern(tuple(tokens))
    return any(pattern.search(construct) for construct in declaration_constructs(content))


def anchor_matches_content(anchor: str, content: str, ignored_tokens: set[str]) -> bool:
    if anchor_boundary_pattern(anchor).search(content):
        return True
    tokens = significant_anchor_identifiers(anchor, ignored_tokens)
    if not tokens:
        return False
    if len(tokens) == 1:
        return identifier_boundary_pattern(tokens[0]).search(content) is not None
    return compound_anchor_matches_construct(anchor, content, ignored_tokens)


def filtered_local_anchors(ref: str, anchors: list[str]) -> list[str]:
    if ref in OPTIONAL_ANCHOR_CITATIONS:
        return [anchor for anchor in anchors if "shoal_" in anchor or "SHOAL_" in anchor]
    return anchors


def validate_targeted_symbol_anchors(
    lines: list[str],
    *,
    targeted_paths: set[str] = ANCHOR_CHECKED_CITATIONS,
    repo_root: Path | None = None,
) -> None:
    contents = load_targeted_contents(targeted_paths, repo_root=repo_root)

    for _line_number, row_id, cells in iter_matrix_rows(lines):
        row_targeted_paths = targeted_paths
        if (
            "accumulo/scanner.go" in targeted_paths
            and not any(
                mapped_row == row_id and mapped_ref == "accumulo/scanner.go"
                for mapped_row, mapped_ref in TARGETED_SYMBOL_ANCHORS_BY_ROW_CITATION
            )
        ):
            row_targeted_paths = targeted_paths - {"accumulo/scanner.go"}
        for cell in cells[1:]:
            path_anchor_bindings = extract_path_anchor_bindings(cell, row_targeted_paths)
            if not path_anchor_bindings:
                continue
            for ref, anchors in path_anchor_bindings.items():
                scanner_key = (row_id, ref)
                if ref == "accumulo/scanner.go":
                    expected_symbols = TARGETED_SYMBOL_ANCHORS_BY_ROW_CITATION[scanner_key]
                    cited_symbols = {anchor.split("(", 1)[0] for anchor in anchors}
                    missing_symbols = sorted(expected_symbols - cited_symbols)
                    require(
                        not missing_symbols,
                        f"{row_id} cites {ref} without required targeted anchors: "
                        f"{', '.join(missing_symbols)}",
                    )
                    anchors = [
                        anchor
                        for anchor in anchors
                        if anchor.split("(", 1)[0] in expected_symbols
                    ]
                anchors = filtered_local_anchors(ref, anchors)
                if ref not in OPTIONAL_ANCHOR_CITATIONS:
                    require(
                        anchors,
                        f"{row_id} cites {ref} without an adjacent local symbol/test anchor",
                    )
                elif not anchors:
                    continue
                content = contents[ref]
                missing = [
                    anchor
                    for anchor in anchors
                    if not anchor_matches_content(anchor, content, IGNORED_ANCHOR_TOKENS)
                ]
                require(
                    not missing,
                    f"{row_id} cites {ref} with stale adjacent anchors: {', '.join(missing)}",
                )


def main(argv: Sequence[str] | None = None) -> None:
    args = list(sys.argv[1:] if argv is None else argv)
    if args == ["--rewrite-cabi-symbol-manifest"]:
        write_c_abi_symbol_manifest()
        print("matrix-cabi-symbol-manifest-ok")
        return
    require(not args, f"unexpected arguments: {args}")
    full_text = DOC_PATH.read_text(encoding="utf-8")
    lines = full_text.splitlines()
    validate_counts(lines, full_text)
    validate_local_line_number_removal(full_text)
    validate_targeted_symbol_anchors(lines)
    print("matrix-counts-ok")
    print("matrix-citations-ok")


if __name__ == "__main__":
    main()
