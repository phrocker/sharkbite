# Learning from measurable ontology snapshots

Status: first offline executable slice, 2026-09-29. This extends
[local decisions](local-decisions.md); it does not close the production
implementation issues or enable review exclusions.

The ontology should organize evidence and feedback. A model learns a bounded
task over a versioned projection of that evidence. Predictions, reviewer
assessments, adjudications and observed outcomes retain different types and
provenance. Repeated predictions never become observations merely by appearing
in the graph.

The implemented [offline loop](../experiments/decision-learning/README.md) binds
picture → example → assessment → dataset → candidate → prediction → evaluation.
A PR-specific adapter actually refits a classifier from the generated manifest
and reproduces its frozen predictions. The generic core has no PR concepts.
The snapshot is allowed to be incomplete: source/builder/ontology identities,
coverage denominators and gaps travel with the evidence. This first import uses
code and unresolved syntax observations; it does not claim an ontology graph
has already been integrated into the learning engine.

## Serving and improvement

The intended serving cascade is exact authorized reuse, a local predictor,
explicit evidence expansion when justified, and an expensive model or human
fallback. Cache identities include task, picture, authorization, model and
policy versions. Missing required evidence, unfamiliar inputs, stale observations
or abstention trigger the consumer's conservative fallback. The serving cascade
is proposed, not implemented by this offline slice.

Automatic training produces challengers. Independent validation determines
where a challenger may be used. Calibration data cannot be reused as final test
evidence. A future release record must bind its exact scope, task loss, minimum
sample requirements, confidence/uncertainty rule, incumbent comparison, immutable
policy registration and rollback target. Live requests never mutate serving
weights. The current gate can grant only shadow-candidate status; even a perfect
run continues full review.

## Weights bundles and release records

A candidate artifact is a **weights bundle**: an ordered manifest
(`bundle.json`) of files, each with a role (`weights`, `tokenizer`,
`preprocessing`, `runtime`, `calibration`), size and SHA-256, plus a bundle
digest over the canonical manifest. Order and roles are part of the identity.
The one-file linear SVM from `experiments/local-ml` is the smallest bundle;
`weights_bundle.py stage` derives it from an unchanged `train.py` output and
`internal/decisionlinear.LoadBundle` serves it. A bundle with a missing,
extra, or altered file fails verification before any load, and the error
names the file. `compare` records whether two trainer runs on the same
manifest agree; when they do not, the diverging files are named and the run
is recorded as non-reproducible rather than as a success.

Release records (`model_release`) bind the bundle digest, the training or
distillation manifest pin, the dataset manifest pin, the evaluation record,
the task scope, and the bound approval. `experiments/decision-learning/learning.py`
and `internal/weightsbundle` seal records identically, so a promotion
decided in either produces the same identity. Rollback appends a new active
release that restores the exact prior bundle digest; the registry's single
pointer advances only after every bound record is durable, so concurrent
requests never observe two releases, and a promotion decided against a stale
pointer is refused. Publication is no-replace on every backend: files first,
manifest last through exclusive creation (or an atomic no-replace rename
locally), so a partial bundle is never visible.

Restricted-data lineage follows the bundle. Promotion records the sources a
dataset manifest included; withdrawing a source marks every release that
included it ineligible for new use, with an event that states no unlearning
is claimed. Rollback to a withdrawn release is refused.

## Selective distillation

A student language model is produced by **selective distillation** and
promoted through the same candidate, shadow, explicit promotion, and
rollback lifecycle. Selection runs along two dimensions and both are
recorded inputs to the job (`internal/distillation`):

- **What** the student learns: the selected tasks, the cohorts and label
  policies admitted per task, the evidence families the student may see,
  the prompt or serialization version, the generation parameters, and the
  held-out example ids excluded from teacher generation, selection, and
  training.
- **Which models** it learns from and is built on: the eligible teachers and
  base checkpoints, each with provider, model, version, cache identity,
  resolved egress posture, and bundle digest where local; the selection
  rule; the development-split measurements the rule used; the chosen
  teacher and base; and every rejected alternative with its reason.

The manifest is content-addressed. Changing any selection changes the pin
and therefore the bundle lineage. A chosen model outside the eligible set,
a choice whose recorded measurements do not satisfy the recorded rule, a
measurement from a non-development split, or an unexplained rejection
refuses the manifest before training. Egress is the resolved posture
(`loopback` or `off_host`), never a declaration: the manifest author writes
one, and the host that records the job resolves every eligible model's
posture from its own configuration (`-learning-model-egress`), refusing a
declaration it does not confirm, a provider it cannot resolve, and an
off-host chosen teacher or base unless the operator opted in
(`-learning-allow-off-host-models`). Two byte strings that decode to the
same typed manifest but differ (an explicit empty optional member) do not
share a pin; the recorder refuses bytes that do not round-trip.

Selection is enforced at export. The authorized cohort exporter produces
only the selected slice; the trainer refuses an export the manifest did not
pin, a cohort or label policy outside the task's admission, or an export
containing a held-out example. A job must supply every admitted cohort.
Teacher outputs are recorded as a sealed prediction set with provenance
(teacher, egress at generation, prompt version) and the kind `prediction`;
an output recorded as an assessment or observation is refused, as is an
example outside the job's admitted exports and an empty set. The set is
submitted to the host before the receipt, and a completed receipt must name
exactly the recorded set.

The trainer contract is provider-neutral. A job consumes the manifest pin
and the exports and emits a weights bundle plus a receipt naming requested
versus actual parameters, cohorts, teacher, and base; runtime identities;
and the executor. Deviations are structured differences (parameter,
requested, actual) that the verifier recomputes and requires to match
exactly; a prose deviation, a parameter the manifest never requested, a
cohort outside the selection, an improvement claim, or an evaluation inside
the receipt is refused. Completion is not improvement. The reference worker is
`experiments/local-ml/distill.py`: a deterministic CPU student with no
model-hub access; a checkpoint fine-tune student plugs into the same
contract. The evaluation policy a run will be judged under (reserved split, positive
label, minimum recall, minimum specificity, minimum positives, minimum
families) is declared on the job before teacher predictions or a receipt: the host seals it,
records the declaration against the manifest pin, refuses a completed
receipt or teacher predictions until one is declared, and
refuses a declaration once either is recorded, so the host's accepted outputs
cannot be used to choose the gates. A completed run becomes a candidate through one
sealing step (`internal/distillation.BuildCandidateSet`, reached from the
console's "seal run" form, `shoalctl learning candidate`, or
`experiments/local-ml/seal_candidate.py`): the candidate, evaluation, and
artifact are built from the recorded manifest, receipt, prediction set, the
declared policy, and every export the run consumed plus the bundle, so the
three paths seal byte-identical records and the host accepts a candidate
only under the policy it declared. The evaluation measures the student on
the evaluation cohort's reserved split against the adjudicated labels (recall
over verified positives and specificity over verified negatives, so a
constant answer clears at most one gate), names the teacher it is compared
with on the rows the teacher labelled, and records every unmet gate and
every leak between the reserved rows and any training row of any cohort as
a reason; its disposition is shadow_candidate or hold. Evaluation compares students against their teachers and the
incumbent on the same picture, and the comparison names each teacher, so an
extraction change and a model change stay distinguishable. A release records
the selection's task scope; a request for a task outside it is refused.

Random audits and targeted labeling are complementary. Targeted cases include
assessor disagreement, uncertain margins, novel relationships and consequential
outcomes. The random sample covers confidently wrong decisions that uncertainty
sampling misses. Store selection reasons and propensities. Population claims
cannot be calculated from targeted labels as though they were a random sample.

## What the measurements change

V9's code-only SVM retained 65/67 frontier-assessed and 64/64 Claude-assessed
relevant functions. Laya retained 61/67 and 59/64 on the same cohort. Generic
operation facts and combined features lost to code-only in development; ontology
features must improve measured decisions rather than simply increase context.
Four complete-PR counterfactual pairs used 16.4% fewer input tokens. This is
promising efficiency evidence, not established defect-detection parity.

The reconstructed learning loop fits 988 examples, quarantines seven exact
train/test overlaps and nineteen held-out label disagreements, and exactly
reproduces 188 frozen predictions. Disputed examples remain in each assessor's
evaluation. The gate reports hold, preserving the two frontier misses and the
post-hoc policy limitation.

Next ontology experiments should compare entry-point reachability, connections
to identity/policy/secrets/persistence, dependency impact and evidence freshness
against the same code-only baseline. Each experiment pins the graph projection
and its gaps. Evaluate at fixed review budget and fixed required retention, then
measure whole-task token use and quality. Keep Laya, linear models and future
predictors interchangeable: task evidence decides which earns deployment.

See the [runnable prototype](../experiments/decision-learning/README.md) for
implemented checks and outstanding production integration boundaries.
