# Adjudicated export → local training → durable inquiry

This showcase runs two separate binary tasks through real Shoal services and
storage: retained source → prediction → reported outcome → authenticated
adjudication → authorized numeric export → locally trained challenger → durable
inquiry. The exporter also returns a provenance manifest, and the trainer requires
an independently supplied manifest digest in its authorized-export mode.

The tasks are **synthetic conformance**, not a code-review benchmark:

- `source`: whether a generated Go snippet contains a literal zero divisor. An
  AST-based reference oracle supplies the narrow property label.
- `review`: whether an original review claim names an existing nonempty line in
  its retained source. This does not verify that a review finding is a defect.

The bootstrap predictor always proposes the negative label. Positive labels come
from separately retained oracle witnesses and adjudication, so prediction agreement
cannot silently become training truth. The fixture identities are trusted logical
roles in one local process; they do not establish independent human review.

## Run both tasks

Use the existing pinned Python environment from `requirements.txt`. Training
publication and inquiry locking currently require Linux. From the repository root:

```sh
go build -buildvcs=false -o /tmp/shoal-adjudicated-training-demo \
  ./cmd/shoal-adjudicated-training-demo
"$PYTHON" experiments/local-ml/run_adjudicated_showcase.py \
  --binary /tmp/shoal-adjudicated-training-demo \
  --output-dir /tmp/my-adjudicated-training-run
```

The output directory must be new. The runner prepares both finite cohorts, trains
each twice, requires byte-identical model artifacts for repeated fitting of the
same export, and queries each candidate twice using the same persistent state.
The first query runs the model; the reopened process must return the same request,
receipt and prediction identities with zero model calls. It writes `summary.json`
with the exact digests, counts and measured local timings.

Each cohort has 16 registered examples: 8 train, 2 calibration, 2 validation and
4 test. Fourteen have verified narrow-property labels, one remains unknown and
one disputed. Only the 8 verified training rows enter fitting. No-comment or
unresolved evidence never becomes a negative. The fixture census and inclusion
probability of one apply only to these 16 compiled examples. Template-derived
fixtures do not support population accuracy, calibration, review-savings or
promotion claims; the held-out splits exercise the data paths only.

To run a single export and retain its producer-issued pin:

```sh
/tmp/shoal-adjudicated-training-demo prepare --task source --out /tmp/source-cohort
# Use the manifest SHA printed by this trusted producer as MANIFEST_SHA.
"$PYTHON" experiments/local-ml/train.py \
  --dataset /tmp/source-cohort/dataset.json \
  --authorized-manifest /tmp/source-cohort/manifest.json \
  --authorized-manifest-sha256 MANIFEST_SHA \
  --output-dir /tmp/source-cohort/candidate
# Use the model SHA printed by the trainer as MODEL_SHA.
/tmp/shoal-adjudicated-training-demo inquire --task source \
  --model /tmp/source-cohort/candidate/model.json \
  --model-sha256 MODEL_SHA --state-dir /tmp/source-cohort/inquiry
```

Use `--task review` for the second task. The public files are created exclusively
and synced; errors can leave a partial directory, which must not be treated as a
successful export. An existing output directory is never overwritten. Inquiry
holds a persistent lock inode through engine close and pins the model, runtime
predictor, task, fixture bytes and creation time in durable state. It refuses to
mint new request identity over an engine whose metadata is missing.

## What the exporter checks

`internal/decisiondatasets.Service.Export(ctx, cohortID)` accepts a registered
cohort identity, not caller-supplied labels, membership or permissions. It requires
a trusted resolver and complete authority integration. It checks exact task,
prediction, picture, target, selected adjudication and retained basis identities.
Inputs must retain their original source bytes; current bytes cannot replace an
unavailable historical version. Outcome and prediction sources are excluded from
predictor input in this slice.

The last adjudication available by the registered cutoff supplies the candidate
label. The export also checks a fresh complete target inventory and current
training-purpose permission. A newer unadjudicated report, later adjudication,
withdrawal or missing permission quarantines the label. A new report does not
rewrite its historical adjudication. Unknown/disputed rows remain explicit
exclusions, and unauthorized source material fails the whole export rather than
leaking target IDs or counts through an exclusion report.

All declared families and retained source digests must remain within one split,
including quarantined rows. Feature/source/label availability is bounded by the
cutoff. Sampling policy, known or unknown inclusion probability, reconstructed or
prospective mode, exact retention references and unknown checkpoint overlap remain
in the manifest. This is an audit record, not evidence of unbiased evaluation.

The exporter detaches mutable authority returns, limits material incrementally
before loading the whole cohort, and supplies separate verification copies. After
all reads, authority must jointly recheck current permissions and inventory
generations. Changing the verification material fails closed, including a change
in the IEEE-754 sign bit of zero. No source/storage read follows that final check.
The retained-material budget is 32 MiB, dataset output 16 MiB and manifest 4 MiB.

The demo authority proves completeness only within its sealed compiled registry.
It checks retained role/witness/source bytes, reads actual durable outcome and
adjudication receipts, and rechecks its generation and permissions after IO. It
has no public registration endpoint and does not claim to enumerate all GitHub
reports. The prediction/outcome/adjudication and target inventory stores use the existing
engine and are flushed to RFiles. See [durable inventory training](DURABLE_TRAINING.md)
for admission recovery, historical snapshot retention and measured export cuts. The reference adjudication registry is constructed anew
for a prepare run; this is not a general reopened production authority.

## Training and provenance boundary

Authorized training checks the raw manifest against the independently supplied
pin before parsing, reads each input once, and checks the raw dataset byte binding,
row membership, split/source/family isolation and portable feature commitments.
Feature commitments hash `shoal.numeric.features.v1\0`, a little-endian uint32
width, then little-endian float64 values. Both languages preserve signed zero.
The training receipt binds the manifest, cohort, authority revision, inventory,
training purpose, dataset, recipe and runtime, including the admission code hash.

The trusted exporter/job boundary supplies authorization. A file plus its own
checksum cannot authenticate roles or enforce later revocation. Every subsequent
training job must reauthorize retained exports before reuse. Legacy synthetic and
trusted offline datasets remain accepted without these optional manifest arguments;
that mode makes no authenticated-export claim. Both manifest flags are required
together. Neither mode promotes the resulting model.

Authenticated dataset HTTP/Go SDK export is available through explicit host
wiring; see [the API guide](../../docs/decision-dataset-http-api.md). Deployed
production source/outcome authority, remaining registration APIs, unbiased
operational cohorts, independent quality evaluation, promotion/rollback and
withdrawal of previously trained models remain follow-on work. Full-review
fallback and the paid-inference experiment budget are unchanged.
