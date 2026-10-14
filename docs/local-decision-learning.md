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
