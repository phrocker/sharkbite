# Frozen source-code classifier serving conformance

This experiment imports the retained V9 code-only mixed TF-IDF linear SVM and
executes its reserved cohort through `internal/decisionlinear`. It tests whether
the real code classifier can cross the Python feature-export / Go numeric-provider
boundary without changing the historical proposals. It does not train a new
classifier or establish new accuracy, review savings, promotion eligibility or
permission to exclude code. Every subject still requires full review.

The exporter verifies the pinned V9 evidence archive and selected member hashes,
content-addressed records, frozen selection, helper source fingerprints and
numerical dependencies. It reads archive members without extracting paths. The
frozen vocabulary and IDF weights transform cached source text; no vocabulary,
coefficient or threshold is fitted to this cohort. This is cached representation
replay, not a rerun of the historical source extractor against current source.

V9 bounded the native SVM margin with a sigmoid and selected `lower_priority`
when that value was below its frozen threshold. The Go provider uses zero as its
classification boundary, so the derived model subtracts `logit(threshold)` from
the intercept. This is the same mathematical decision rule, but floating-point
arithmetic can differ. Cohort parity is measured rather than assumed; equality
on this cohort does not prove bitwise equivalence for arbitrary future inputs.
The sigmoid value is an uncalibrated ranking score, not a probability.

The derived model's `training_runtime_sha256` field binds the recorded **import
runtime**. This adapter does not possess a complete verified runtime inventory
for the historical training execution. Its recipe and runtime record make that
limitation explicit; consumers must not interpret it as a newly trained candidate
with fully authenticated training provenance.

The Go harness uses the actual pinned provider, task, picture and typed result
contracts. Its evidence authority is an offline conformance fixture. This harness
does not register production evidence, expose an authenticated API, or persist
service receipts. The synthetic service/restart demonstration remains separate.
Production use still requires an authorized source/feature builder and exporter,
label lifecycle, registered evaluation and activation gates, and withdrawal support.

Generated dense numeric inputs belong in a temporary output directory. The
repository retains the original compressed evidence and small execution reports;
it does not duplicate the dense cohort.

## Run

From the repository root, use the pinned Python environment described in
[README.md](README.md):

```sh
go build -buildvcs=false -o /tmp/shoal-frozen-code-replay ./cmd/shoal-frozen-code-replay
/tmp/shoal-frozen-code-replay describe
"$PYTHON" experiments/local-ml/frozen_code.py \
  --task-id TASK_ID_FROM_DESCRIBE --output-dir /tmp/my-frozen-code-bundle
/tmp/shoal-frozen-code-replay replay \
  --bundle /tmp/my-frozen-code-bundle \
  --model-sha256 MODEL_SHA256_FROM_EXPORT \
  --manifest-sha256 MANIFEST_SHA256_FROM_EXPORT
```

Use the exporter-produced hashes as the expected pins. The Go harness verifies
exact bytes and their request bindings; it relies on the reviewed exporter to
establish the derivation from the frozen archive. A caller-supplied hash is not a
signature or independent authorization. The fixed picture timestamps belong to
the conformance fixture, not historical source observation times.

The exporter publishes into a new directory exclusively. Run it again into a
different directory to compare reproducibility. Replay can reuse an existing
bundle; repeated execution recomputes predictions rather than reading receipts.
The CLI fails if the cohort is incomplete or any label differs.

## Adversarial review

The design review caught and resolved an exporter/harness schema mismatch and
insufficient checking of the manifest's frozen provenance assertions. The first
independent code review then reproduced a strict-decoding gap: Go's struct decoder
accepted case-insensitive field aliases and converted null scores to zero, even
when the malformed manifest had a matching externally supplied hash. Required,
case-sensitive field and type validation is necessary in addition to byte pins.

The parser fix requires every field with its exact spelling and type, rejects
nulls, and rejects invalid UTF-8 or unpaired Unicode escapes. Regression cases
cover missing, null and aliased fields throughout the manifest. A fresh second
independent code review of `11a42b9` found **no actionable findings**. It also
independently compared retained subject identities, ordering and original scores
to the pinned archive. No power-loss or concurrent filesystem-race simulation
was performed.

## Executed evidence

The [retained summary](evidence/frozen-code-2026-10-06/summary.json) and
[per-subject replay](evidence/frozen-code-2026-10-06/replay.json) record the run at
`11a42b9`: **188/188 proposals matched**, with 47 lower priority and 141 retain.
Two separate exports produced byte-identical artifacts across all 192 files;
two separate Go replay processes produced byte-identical reports. These are
recomputed provider results, not service receipt replay. All 26 Python tests,
targeted Go race tests and vet passed. No paid classifier calls were made.

The retained manifest binds the generated inputs and model. Recipe/runtime
records describe the import. Reproduce the dense inputs with the commands above;
they are intentionally omitted from the repository. This result establishes the
frozen code model's numeric-provider compatibility on this cohort. It does not
repair V9's known quality limitations or authorize source exclusion.
