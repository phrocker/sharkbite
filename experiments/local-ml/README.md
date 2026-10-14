# Local CPU training → inquiry → durable replay

This runnable slice connects a real locally fitted binary linear SVM to Shoal's
artifact catalog, authenticated decision service and durable receipts. Dataset
admission, training and serving were built concurrently behind one bounded data
contract. There is no model download, Python subprocess or network call at serving
time: the Go provider loads a data-only coefficient artifact and runs on CPU.

The included task is **synthetic numerical conformance**, not source-code accuracy.
It is not the V9 mixed TF-IDF SVM or its feature extractor. The same numeric
interface can accept versioned measured features from a code/ontology builder,
but verifying that builder and its exported labels remains production integration
work. The frozen V11 experiment and paid-inference budget are unchanged.

The separate [frozen code conformance experiment](FROZEN_CODE.md) imports the
retained V9 mixed TF-IDF model and checks its historical proposals through the
same Go provider. The [source-backed service example](FROZEN_SERVICE.md) adds
local source authorization and durable receipts for that frozen cohort. Neither
example supplies a production registration authority.

The [adjudicated training showcase](ADJUDICATED_TRAINING.md) now connects real
source/review fixture receipts to authorized export, manifest-pinned fitting and
durable inquiry. It exercises poisoning, withdrawal and provenance boundaries
with a sealed synthetic census; it does not establish code-review quality.

## Run

Use a Python environment matching `requirements.txt` (Python's exact version and
numerical library/runtime details are recorded in every build). Training publication
currently requires Linux `renameat2` with no-replace support. Serving is Go-only.
From the repository root, with `PYTHON` pointing to that environment:

```sh
go build -buildvcs=false -o /tmp/shoal-local-ml-demo ./cmd/shoal-local-ml-demo
mkdir -p /tmp/my-local-ml-run
/tmp/shoal-local-ml-demo prepare --output /tmp/my-local-ml-run/dataset.json
"$PYTHON" experiments/local-ml/train.py \
  --dataset /tmp/my-local-ml-run/dataset.json \
  --output-dir /tmp/my-local-ml-run/candidate
```

The trainer prints `model_sha256` and writes `model.json`, `recipe.json`,
`runtime.json` and `training-receipt.json`. Supply that independently obtained
model digest to inquiry (replace `EXPECTED_SHA256` below):

```sh
/tmp/shoal-local-ml-demo inquire \
  --model /tmp/my-local-ml-run/candidate/model.json \
  --model-sha256 EXPECTED_SHA256 \
  --state-dir /tmp/my-local-ml-run/state
```

Run inquiry again with the same arguments. The first invocation calls the real
model once and verifies an immediate retry. The second process reopens the engine
and returns the same request/receipt/prediction IDs with
`provider_calls_this_process: 0`. Both report `replay_matched: true`. The fixed
fixture uses one registered task and trusted synthetic evidence; it is deliberately
not a general public request-registration or provenance-verification endpoint.
Changing the model requires a new state directory. Missing replay metadata for an
existing engine fails closed instead of minting a fresh request identity.

All files/directories are created exclusively. Training fsyncs files and directory
entries before acknowledging publication. If destination-parent sync fails after
the atomic rename, the candidate remains and the CLI exits 2 with
`published-durability-indeterminate`; do not treat this as rollback or overwrite
the candidate on retry.

## Contracts and determinism

`dataset.py` admits schema-v1 `numeric-training-dataset` exports. Each row retains
its identity, family/content digest, source revision, observation/receipt/label
times, split, label state, asserted training permission and finite numeric features.
Feature magnitudes must be at most 1,000,000 in both training and inference; larger
values require an explicit versioned external transform and are never clipped.
This prevents the observed extreme-value native fitting stall. Production jobs
still need a scheduler-enforced wall-clock/resource limit; an iteration bound is
not a training-time SLA.
The loader rejects future receipts, cross-split family/content overlap, malformed
JSON, duplicate keys, inconsistent widths and excessive rows/bytes. Only verified,
permitted `train` rows enter fitting; unknown/disputed rows remain explicit
exclusions, and nontraining splits never fit vocabulary, scaling or coefficients.
There is no implicit feature transformation in this recipe.

Permissions, `verified` labels and `trusted-export` are assertions from an offline
trusted producer. They are **not authenticated by this file format**. The production
export service must enforce training rights, provenance, temporal visibility,
source withdrawal and adjudicator roles before issuing an eligible export.

`train.py` uses a fixed seeded `LinearSVC` recipe, sorted fitting rows, pinned package
versions and one numerical thread. Artifacts bind the canonical dataset, recipe,
trainer/dataset source hashes and actual training runtime. Model artifacts contain
finite coefficients and label ordering, never pickle or executable code. Convergence
failure cannot produce a completed candidate. Repeat builds are checked for exact
bytes in the same environment; cross-platform/cross-library bit identity is not
promised. Production deployment also needs an independently trusted runtime/package
artifact inventory; version strings are not package signatures.

`internal/decisionlinear` validates an external SHA-256 before model load, resolves
one immutable release/predictor pair, and accepts strict bounded numeric input with
exact task/question/label/subject/feature bindings. It uses serial float64 CPU
arithmetic, checks cancellation and nonfinite accumulation, and allows one active
call per provider. Input bytes must match the picture's digest. Outputs are labels:
uncalibrated margins are not probabilities. Zero margin selects the positive label.
The versioned serving identity is distinct from the training environment; runtime
or formatter behavior changes require their version IDs to change.

Receipt retrieval returns the exact stored original, including its timestamps.
Recomputation is a new execution and has a new completion time even when its label
matches. Deterministic output does not establish a correct decision.

## Validation and remaining work

```sh
"$PYTHON" -m unittest discover -s experiments/local-ml -v
go test -race ./internal/decisionlinear ./cmd/shoal-local-ml-demo \
  ./internal/decisionartifacts ./internal/decisionservice ./internal/decisionstore ./pkg/decision
go vet ./internal/decisionlinear ./cmd/shoal-local-ml-demo
```

Tests exercise quarantine/leakage, training-only fitting, separate-process artifact
reproducibility, wrong dependency/model identities, malformed input, cancellation,
exclusive publication races and durability failures, and real-engine restart replay.

This advances #405/#406/#418 without closing them. Remaining work includes the
production picture/export authority and label lifecycle; real source-code and
review-feature adapters; independently registered evaluation/promotion/rollback;
source-to-model withdrawal; HTTP/SDK wiring; and the pinned Laya worker. Automatic
jobs may create candidates, but this slice never promotes one or permits source
exclusion or risky operations. A successful synthetic run makes no model-quality
claim about code review.

## Executed evidence and independent review

The [retained synthetic run](evidence/smoke-2026-10-06/smoke.json) was executed on
`af1bb3e`: two separate training processes produced byte-identical model, recipe,
runtime and receipt artifacts. Eight rows were fitted; four held-out synthetic sign
labels matched the exported linear decision rule. The real Go inquiry made one
model call in the first process and zero after restart, with identical stored
request/receipt/prediction IDs. These are conformance observations, not code-review
quality or an approval to promote the model. The tiny dataset and complete candidate
artifacts are retained alongside the report.

Three independent reviewer-agent rounds preceded publication:

| Round | Result | Resolution |
|---|---|---|
| 1, initial working implementation | P2: missing directory fsync for trained artifacts and demo-state publication. | File → staging-directory → atomic rename → parent sync; explicit post-rename uncertainty. Demo syncs state-file parent. Included in `3bad915`. |
| 2, `3bad915` | P2: newly created state ancestors were not durable; P2: extreme finite feature values stalled native fitting beyond the iteration bound. | Sync all state ancestors, including retries; reject feature magnitudes above 1,000,000 on training and serving, with no clipping. Fixed in `af1bb3e`. |
| 3, `af1bb3e` | No actionable findings in a fresh full review. | No implementation changes after the clean round. |

All 20 Python tests, targeted Go race tests and vet passed. Review included static
analysis, syscall-order instrumentation and an externally timed numerical probe;
it did not simulate a power loss or perform exhaustive fuzzing. No Copilot review
or paid classifier experiment was requested.
The same-picture challenger comparison persists both artifacts and a
measurement-only report. `verify_comparison.py` independently checks the
report digest, disjoint predeclared splits, candidate set, metric bounds,
pinned threshold, and measurement-only disposition before a downstream
evaluator consumes it.
