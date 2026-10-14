# V10: first budgeted prospective assessment

Protocol committed in `45efac6` before new PR metadata and source collection.
The user authorized authorization/security triage on Shoal PRs with a **$25
paid labeling/validation cap**. The V9 SVM and threshold remain frozen. No
training, threshold adjustment, source exclusion or external review posting
occurred during this cohort.

## Current result

The frozen open-PR inventory had only one eligible unseen non-draft family,
PR415, at head `8bede7a75c1dce05ab992b3d3675cbcfb01313c2`. Three family slots remain
unfilled. Drafts and previously observed families were not substituted. No
persistent polling service is installed. Continuing collection requires another
recorded inventory and an explicit protocol continuation; the original inventory
and selection are retained without retrospective replacement.

PR415 existed before registration. This run measures **prospective assessment of
a newly observed existing snapshot**, not performance on future-created PRs.

| Measurement | Result |
| --- | --- |
| Changed files / units / functions | 4 / 52 / 43 |
| Deterministically sampled functions | 24 |
| Exact development overlap (body or diff) | 10 |
| Relevant retained, Claude pass A | 15/15 |
| Relevant retained, Claude pass B | 15/15 |
| Relevant retained after exact-overlap exclusion | 10/10 in each pass |
| Proposed lower-priority functions | 0 |
| Proposed source-byte reduction | 0% |
| Label disagreements | 2 |
| Paid calls | 4 |
| Provider-reported total cost | $1.046898 |
| Remaining monetary allowance | $23.953102 |

Both assessments used separate no-tools sessions of the same provider/model;
these are correlated repeated assessments, not independent model-family evidence.
Actual model identity and usage are retained with each response. The full
24-function packet exceeded the frozen 90,000-byte cap, so deterministic
complete-source batches contain 19 and five functions. Each assessor receives
both batches. No function was truncated or silently discarded.

The gate returns **hold**: only one family and ten novel positives per assessor,
below four families and 50 positives required. Retaining everything cannot
demonstrate efficiency. The primary finding is conservative behavior on this
security-sensitive patch, with substantial development-source overlap. These
labels are relevance assessments, not defect truth or full-PR review parity.

## Implemented infrastructure

- `prospective_snapshot.py`: content-addressed source-ontology projection of
  revision pairs, changed files and declarations, with witnessed `observed_in`
  and `declared_in` relationships. Coverage records all dispositions and an
  unknown dependency denominator. It rejects pre-registration acquisitions,
  changed revision pins and previously observed families. This is a public
  source projection, **not authenticated Shoal graph hydration**; callgraph,
  identity-propagation and policy-consumer edges remain absent.
- `prepare_references.py`: source-only deterministic batches, exact unit IDs,
  frozen byte caps and explicit oversized-unit failure. No model output enters
  reference prompts.
- `paid_review.py`: transactional SQLite reservations before network calls,
  protocol binding, retry prevention, actual-cost settlement and retention of
  reservations for unpriced/interrupted calls. Each call reserves $3 and uses
  the CLI's $1.50 cap plus a 6,000-output-token setting. Calls above reservations
  stop further spending for reconciliation. The runner trusts provider-reported
  costs and CLI budget enforcement; it is not a billing-system spending lock.
- `evaluate_prospective.py`: source/prompt/model identity checks, exact label
  coverage, separate-assessor and temporal-cohort metrics, exact development-overlap
  strata and unconditional full-review behavior. Code features and reported sizes
  are reconstructed from sampled source. The pinned CPU replay recomputes scores
  and dispositions before evaluation; a rehashed score file is insufficient.
  Priced calls must have settled ledger entries. Selection is reconciled with
  retained raw PR/file inventories, and the picture/sample are rebuilt from the
  full collected manifest. Empty cohorts remain pending. No consensus label is fabricated.

The original protocol also caps the experiment at eight calls. Four remain,
independently of the unspent dollar allowance. We will not silently increase
that operational cap to fill the cohort. Shared caching can affect billing;
these costs do not establish production inference cost.

## Evidence and replay

`evidence.tar.gz` preserves the raw inventory/file list, collection manifest,
source projection, sampled inputs, frozen predictions, reviewer prompts/raw
responses, calls, SQLite budget ledger and evaluation outputs.
`evidence-manifest.json` binds every file and the archive by SHA-256. Compact
review artifacts remain directly readable: `report.json`, `inventory.json`,
`budget.json`, and `validation.json`.

The V9 numeric model/development corpus remain in the V9 archive; no duplicate
weights are added here. From the repository root, in the pinned CPU environment:

```sh
mkdir /tmp/v10-replay /tmp/v9-replay
tar -xzf showcases/pr-triage/runs/prospective-v10/evidence.tar.gz -C /tmp/v10-replay
tar -xzf showcases/pr-triage/runs/operations-v9/evidence.tar.gz -C /tmp/v9-replay
python showcases/pr-triage/evaluate_prospective.py \
  --run /tmp/v10-replay \
  --protocol showcases/pr-triage/experiments/prospective-v10.json \
  --model /tmp/v9-replay/trained/model.json \
  --development /tmp/v9-replay/development.json \
  --output /tmp/v10-new-evaluation
```

This command recomputes the frozen CPU model and makes no paid calls. Compare
its report identity to `report.json`. The archive retains the original report;
the current report adds temporal strata and verification metadata while preserving
all original measurements and costs.
The mutable budget database is retained for recovery; never create a fresh
budget ledger for another batch in this experiment. The initial local
registration is a Git receipt, not a trusted-server timestamp attestation.

The original V10 builder fingerprint is explicitly recognized for historical
replay, but its code is not executed: current validation reconstructs every
semantic field while preserving that verified historical metadata. Unknown
builder versions fail closed. The trust roots remain retained public-source
observations; local hashes are not authenticated GitHub timestamps or signatures.
