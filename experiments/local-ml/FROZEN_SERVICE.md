# Frozen code: authorized service inquiry and durable replay

This local integration registers the retained V9 cached source representations,
numeric inputs and model, then runs the complete 188-subject cohort through the
artifact catalog, decision service and engine-backed receipt store. It extends
[frozen provider conformance](FROZEN_CODE.md) to retained source bytes, current
access checks and durable replay. The service remains general; source-review
semantics live in this command and exporter.

The Python wrapper produces one exclusively published bundle containing the
existing numeric export, all cached code-text source files, and an independently
pinned source manifest. It verifies the retained archive and binds source order,
subject identities, byte lengths and digests to the numeric manifest. It records
its own builder hash. No model fitting or paid inference occurs.

These sources are the original model's cached `text.code` representations. They
are not complete repository files and have no independently verified historical
observation or receipt timestamps. Pictures use the current local import time;
revision identifiers bind cached content. The task is historical priority replay,
not a current assessment of those repositories. Every subject remains full review.

The Go command checks external model, numeric-manifest and source-manifest pins.
A trusted local registration authority admits only the exact registered record
and checks current task/source access before artifact disclosure and inference.
The example binds a fixed local principal through the real authentication resolver;
it is not a network login, a multi-tenant registration endpoint or a production
source-permission exporter. Manifest hashes establish integrity for this trusted
producer, not authorization or authenticated historical provenance.

Each subject has a source-backed picture, request and durable receipt. Immediate
retry and process restart return the original stored prediction without another
provider call. State metadata binds all three external pins, the effective predictor/runtime
identity and the import time;
missing/corrupt metadata or changed pins cannot silently create new request
identities. Publication errors leave explicit uncertainty rather than claiming
that a completed filesystem rename was rolled back.

The numeric provider and persisted core record formats are unchanged. This slice
does not implement outcomes/adjudication, training rights, poisoning-resistant
label governance, promotion or rollback, source-to-model withdrawal, or HTTP/SDK
transport. Those remain separate roadmap work. Frozen V11 is unchanged.

## Run

Use the same pinned Python environment as [the numeric training example](README.md).
From the repository root:

```sh
go build -buildvcs=false -o /tmp/shoal-frozen-code-replay ./cmd/shoal-frozen-code-replay
/tmp/shoal-frozen-code-replay describe-service
"$PYTHON" experiments/local-ml/frozen_service_bundle.py \
  --task-id TASK_ID_FROM_DESCRIBE_SERVICE --output-dir /tmp/my-code-service-bundle
/tmp/shoal-frozen-code-replay inquire \
  --bundle /tmp/my-code-service-bundle \
  --model-sha256 MODEL_SHA256_FROM_EXPORT \
  --manifest-sha256 MANIFEST_SHA256_FROM_EXPORT \
  --source-manifest-sha256 SOURCE_MANIFEST_SHA256_FROM_EXPORT \
  --state-dir /tmp/my-code-service-state
```

Repeat the inquiry command with the same pins and state directory. The first
process should report 188 provider calls; the second should report zero, with
identical per-subject picture/request/receipt/prediction identities and labels.
Each inquiry also checks immediate retry and authorized receipt retrieval.
The original `describe` and `replay` commands continue to support the provider-only
experiment; `describe-service` defines a distinct task with actual evidence-policy
and ranking-plan identities. Bundles for the two tasks are not interchangeable.

Generated source/numeric bundles and the engine state live outside the repository.
The original compressed evidence and compact run reports are retained. State is a
single local registration session, not a shared multi-user service deployment.


## Executed evidence and independent review

At `db39142`, the [retained run](evidence/frozen-service-2026-10-08/summary.json)
completed all **188/188** historical proposals through the real service. The first
process made 188 provider calls. A second process reopened the engine's RFiles
and returned identical per-subject picture, request, receipt, prediction and label
records with **zero provider calls**. Both runs also verified immediate retry and
receipt reads. The retained source representations total 340,577 bytes.

Two separate exports produced byte-identical contents across all 381 files.
The original provider-only replay also remained byte-identical to its prior
retained report. All 33 Python tests, targeted Go race tests across the command,
provider, artifact catalog, service and receipt store, and command vet passed.
Tests cover source revocation before admission and after completion, renewed
current authentication, source/model/state substitution, missing/corrupt state,
source quote bounds, and publication failures. No paid classifier calls occurred.

Two independent adversarial code-review rounds preceded publication. Round 1
found a durability gap: existing valid metadata could be accepted on retry after
an earlier file-sync failure without ever confirming that the file contents were
durable. The fix reopens the metadata, verifies the exact bytes and successfully
syncs the file before engine use; failure-injection tests exercise retry. A fresh
round 2 at `db39142` found **no actionable findings**. Review did not simulate
power loss or concurrent filesystem replacement. That was the initial publication review; the additional review findings and
fixes below supersede its clean verdict for the current head.


## Additional adversarial review and session ownership

A subsequent user-requested review found two more correctness issues. Two live
engine handles could each reserve and execute the same request/key, because the
embedded engine does not coordinate independent processes. Inquiry now holds a
nonblocking exclusive Linux file lock from before metadata admission until after
engine close. The lock file remains in place so all cooperating invocations use
the same inode. Another inquiry against that directory is rejected before state
or engine mutation; process exit releases the lock. This is a local advisory lock,
not coordination across machines or protection against a privileged process
replacing files in the state directory. Service inquiry fails closed on unsupported
platforms; the provider-only replay remains available independently.

The second issue was runtime drift: unchanged bundle pins could rebuild different
request identities with a new Go/runtime identity, then fail on the old receipt
key after inserting new artifacts. State schema 2 now persists the effective
predictor identity and rejects drift before opening the engine. Exact replay
requires the same predictor/runtime identity. Schema-1 state is rejected with an
explicit recovery requirement; there is no silent migration or regeneration of
historical request identities. Preserve old state for recovery rather than deleting
it. A new state directory creates a separate registration session.

Regression tests cover cross-process lock contention, release on process exit,
symlink refusal, rejection before state mutation, runtime drift, and legacy state
admission. The original evidence above remains the initial implementation's run;
final revised execution evidence is recorded separately below.


A fresh full-PR review of `bfa5ee0..b203200` found **no actionable findings**
after those fixes. Uncached targeted race tests covered the new contention,
process-exit, state rejection and authorization cases. Requests that have not
completed retain their original one-hour deadline; the command does not extend
that deadline on retry. Committed receipts remain replayable under the matching
runtime and current authorization. No power-loss simulation or hostile filesystem
replacement test was performed.

The [revised execution evidence](evidence/frozen-service-2026-10-08/reviewed-v2-summary.json)
at `b203200` again records 188/188 matching decisions, 188 calls initially and
zero after restart with identical per-subject records. A simultaneous full CLI
invocation was rejected by the session lock. These reports use a new schema-2
state directory; the original schema-1 evidence remains retained above. Only
documentation and these execution reports changed after the fresh clean review.
