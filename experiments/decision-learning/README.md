# Offline learning receipts

This executable prototype implements the learning-record boundary proposed in
[the architecture](../../docs/local-decisions.md). It has no GitHub concepts,
LLM provider dependency, serving API, scheduler, authorization enforcement, or
production release mutation. Inputs are trusted local exports; content hashes
check identity, not the authority or truth of a submitting assessor.

`learning.py` provides three commands:

- `dataset`: freeze explicit family splits and label references; quarantine
  disagreements, unknown labels, feature-ineligible training rows and exact
  training/held-out content overlap.
- `audit`: select a reproducible uniform hash sample, then disjoint targeted
  cases. Record the random stratum's inclusion probability. The caller must
  commit an independently chosen seed before observing labels; the function
  cannot prove this chronology. Targeted results are not a population estimate.
- `evaluate`: check model/dataset/prediction lineage, exact evaluation coverage,
  reserved test membership and train/validation/test leakage; evaluate every
  assessor independently. Emit `hold` or `shadow_candidate`. Both require full
  review and disable optimization. No result permits source exclusion.

The library also exposes `promote_candidate` and `rollback_release`. Promotion
requires a sealed candidate artifact, a clean `shadow_candidate` evaluation,
and a separately bound explicit approval; it emits an immutable active-release
record without changing a serving pointer. Rollback appends a new release that
points at a prior complete release and requires a bound approval. Neither
function authenticates the caller or enables live traffic by itself; a service
must enforce those boundaries around the records.

`poisoning.py` runs a versioned deterministic attack corpus covering duplicate
flooding, prediction-as-label injection, held-out overlap, failed evaluation,
artifact substitution, unauthorized promotion, an unlisted assessor, replayed
assessment identity, and a targeted training-membership trigger. It reports
rejection and quarantine denominators with a clean control. The retained v2
receipt is [evidence/poisoning-v2.json](evidence/poisoning-v2.json). The report
is structural attack evidence only; it does not estimate model quality, ground
truth, influence, or machine unlearning.

`verify_poisoning.py` verifies a saved receipt without rerunning the attacks. It
checks the pinned corpus/attack identities, fail-closed statuses, denominators,
clean control, structural limitation, and content digest. A verified receipt is
evidence that the recorded corpus run was not edited after the fact; it is not a
claim that the boundary resists an unrepresented attack.

`quality.py` produces a sealed provider-neutral quality report for a predeclared
test cohort. It keeps unknown/disputed rows out of resolved metrics while
recording their denominators, reports confusion metrics, Brier score, ECE,
Wilson recall intervals, family count, and bounded training/inference/labeling
costs. Targeted samples cannot make a population claim, and every report has
`promotion_eligible: false`. Reports also bind canonical label, score, and
group-map digests so the measured inputs can be independently retrieved.
The verifier also checks denominator, confusion, calibration, uncertainty,
sampling, cost, and population-claim consistency before accepting a sealed
report.

`calibration.py` fits a deterministic temperature artifact on validation scores
only and applies it only to disjoint test/inference IDs. The artifact binds the
source model/runtime and well-formed validation score/label digests, accepts exact 0/1
probabilities with bounded logits, and cannot mutate a serving pointer or
authorize promotion.

Every output is content-addressed, schema-versioned JSON and created with
exclusive file creation. A new batch produces a new ledger snapshot, never an
in-place label correction. Conflicts stay excluded until a later explicitly
adjudicated export/policy resolves them. Predictions are rejected as assessment
kinds. This is a local convention, not protection against a dishonest exporter
misrepresenting a prediction as human evidence.

The picture schema binds source/builder identities and ontology version, plus
coverage with unknown denominators represented as `null`. V9 imports explicitly
say no resolved ontology is present. The core does not yet hydrate or authorize
an actual Shoal graph snapshot. Dataset receipts are explicitly reconstructed;
there is no temporal availability validator yet.

## Executed real-data cycle

The [PR showcase adapter](../../showcases/pr-triage/learning_loop.py) imports the
frozen V9 artifacts, derives a fitting manifest, optionally refits its winning
model, reproduces held-out scores, records separate assessments and emits the
gate result. See [V9 reproduction](../../showcases/pr-triage/runs/operations-v9/README.md).

Observed: 995 development examples, seven exact held-out overlaps removed,
988 fitted examples. Nineteen held-out assessor disagreements are quarantined
from consensus-label dataset rows but **all 188 held-out examples remain in
separate-assessor evaluation**. The refit reproduces every frozen score exactly.
The new gate emits `hold`: frontier retention is 65/67, below its 98% target,
and this imported policy is post hoc. It cannot retroactively become predeclared.

The audit plan is a replay demonstration (12 random plus 12 targeted), not a
claim that it selected the already collected labels. Inference margin distance
is a targeting heuristic, not calibrated uncertainty.

The sealed `influence.py` primitive measures leave-one-out score deltas and
decision flips between two explicitly identified artifacts on the same test
IDs, with canonical digests for both score maps. It is a structural sensitivity report only: it makes no truth, quality,
causal, or machine-unlearning claim, and cannot change a serving or promotion
pointer.

```sh
python3 -m unittest discover -s experiments/decision-learning -v
python3 experiments/decision-learning/learning.py dataset \
  --ledger /path/to/ledger.json --policy /path/to/dataset_policy.json \
  --output /tmp/new-dataset.json
python3 experiments/decision-learning/learning.py evaluate \
  --ledger /path/to/ledger.json --dataset /path/to/dataset.json \
  --policy /path/to/evaluation_policy.json --candidate /path/to/candidate.json \
  --predictions /path/to/predictions.json --output /tmp/new-promotion.json
```

## Next integration boundaries

1. Implement authenticated export/retention and typed task/picture records through
   issues #402/#403/#405. Replace client assertions about receipt times and
   authority with server records. Preserve visibility on learned artifacts.
2. Add temporal membership and immutable pre-experiment policy registration.
   Separate development, calibration and future test families; record dependency
   and near-duplicate overlap, which exact hashing cannot detect.
3. Add task-specific training adapters behind manifest input/model artifact
   output. The current adapter refits only the frozen V9 winner. Candidate search,
   scheduling, rollback and serving pointers remain separate work under #406.
4. Compare ontology features with code-only evidence on the same snapshots and
   splits: entry-point reachability, identity/policy/persistence relationships,
   change impact, and coverage/freshness. Unknown links are not negative facts.
   Measure each feature family's incremental utility before adopting it.
5. Validate end-to-end task quality, error severity, family-level uncertainty,
   calibration, drift, and total labeling/training/serving expense. Point recall
   and a small sample do not authorize exclusion. Random audits must continue
   after any future task-scoped deployment.
