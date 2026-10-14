# ADR: Sharkbite client compatibility scope

Status: Accepted, normative for `docs/sharkbite-compatibility.md` revision 56.

## Decision

Shoal replaces Sharkbite as an Accumulo 4 client. The release gate covers the
importable and documented `sharkbite`/`pysharkbite` Python API, the Go behavior
needed to implement it, the stable C ABI used by that package, and cross-cutting
ownership, binary-safety, error, lifecycle, packaging, and live-cluster
semantics. It does not require a source-compatible recreation of Sharkbite's
historical C++ implementation.

Every matrix row has exactly one release-scope disposition in
`docs/sharkbite-compatibility-revision56-scope.tsv`:

1. **Covered** — a required client row whose matrix status is `Covered`.
2. **Approved divergence** — a required row with a named, dated decision in
   §26. It is satisfied but does not claim equivalent functionality.
3. **Required gap** — an importable or documented core Python/client, stable
   C ABI, Accumulo 4 semantic, packaging, or cross-cutting row that is not yet
   covered or approved.
4. **Optional** — PyTorch, pandas/pandashark, the dead embedded extension, or
   historical C++ header/test surface, plus an individually named deferred
   platform obligation. These may be delivered separately but do not block
   the core client package.
5. **Not required** — an individually justified superset, duplicate, defect,
   or non-contract row already carrying `Not required (rationale required)`.

`SB-PKG-012` and `SB-PKG-013` follow their optional Torch and pandas modules.
All `SB-CPP`, `SB-CXX`, `SB-EMB`, `SB-TORCH`, and `SB-PANDA` rows are optional.
On 2026-08-20, owner @phrocker decided that native macOS wheel, dylib, archive,
link/runtime, and Mach-O verification are not required for the current client
release. Exactly `SB-PKG-008`, `SB-XCUT-014`, and `SB-XCUT-019` are therefore
`Optional` under `optional-deferred-macos-native`; their underlying matrix
statuses and evidence remain unchanged. `SB-PKG-011` is not included in that
deferral: revision 56 covers it with the controlled Linux manylinux workflow
and verified main-branch run, while macOS publication remains optional. No
other prefix-level or platform bulk exclusion is permitted.

## Evidence and anti-bulk rules

The ordered scope manifest pins the Sharkbite source commit, Shoal source
commit, matrix revision, row order, matrix status, disposition, and governing
rule. The validator recomputes every disposition from the rule set. Editing
counts or thousands of manifest lines cannot authorize a reclassification:
changing a rule requires a reviewed validator and self-test change.

The detailed matrix status remains useful evidence. Optional and not-required
rows retain their underlying implementation status; scope does not rewrite a
missing implementation into `Covered`. Core rows with `Missing Go`, `Missing C ABI`, or `Behavior mismatch` remain
`Required gap` unless they are one of the three individually named deferred
macOS-native rows above.

## Acceptance

The core code/package gate is open only when every required row is `Covered` or
an `Approved divergence`. Live Accumulo evidence tracked by #74 remains
explicit required scope. Revision 56 retains exactly the three named
macOS-native rows as optional without claiming coverage. `SB-PKG-011` is
covered by the merged controlled Linux manylinux workflow and successful
main-branch run 32493148857.
