# V9: syntax ablation, stronger CPU candidate, first learning-loop replay

All actions remain full review. PR #410 remains unmerged.

Six recipes compared code, generic AST operations, and combined views with
logistic and linear-SVM learners. 995 development functions include the retired
V8 evaluation. Seven exact held-out body/diff overlaps were purged, leaving 988.
Five whole-PR folds also purge test-body/diff overlap. The code-only SVM wins:
6.66% development source-byte reduction at 98.10% proposed relevance retention,
compared with 6.03% for matched code-only logistic. Operations and combined views
did not improve this tradeoff. SVM scores are bounded margins, not probabilities.

The reserved cohort contains 188 sampled functions from twelve older PRs:
303, 298, 300, 299, 297, 296, 294, 295, 293, 292, 289, 288. Sampling preceded
fitting/labels (seed 20260929, maximum 20 functions/PR). This is retrospective,
not future-PR validation. The primary model/threshold were frozen before
assessments. The matched logistic artifact was materialized after primary
inference, before the selecting agent read reference outputs; do not interpret
its metadata as proof it preceded label generation.

| Model | Frontier retained | Claude retained | Proposed full-source byte reduction |
| --- | ---: | ---: | ---: |
| Code SVM | 65/67 | 64/64 | 11.44% |
| Matched code logistic | 64/67 | 64/64 | 17.18% |
| Earlier adapted Laya | 61/67 | 59/64 | 18.16% |

The SVM lowers 47/188 functions, no unknowns. Its two frontier misses are
`RetrievalResponse.MarshalJSON` and `(*Explorer).loadCursorKey`; Claude disagrees
on their relevance. The original reference and selection-conditioned source
reconsideration remain separate. One malformed Claude unit ID was normalized
using the unique missing same-PR record, exact prefix and witness; raw output,
normalization and original labels are preserved. No label was changed.

One 188-function CPU scoring batch took about 195.5 ms, excluding cold load and
feature construction. This is not single-request latency or a service benchmark.

## Complete-PR and newly observed live checks

The predeclared paired gate used at least 95% retention on both references and
no unknowns lowered. The first four code PRs in frozen order passed to paired
review: 303, 298, 300, 299. Full-population scoring covers 448 functions; additional
functions beyond the 188-function sample were not part of the training-overlap
reservation. Paired cost evidence is distinct from held-out relevance evidence.

Eight Claude calls used 184,247 full-arm versus 154,058 candidate-arm input tokens
(including cache): **16.39% reduction**. Packet bytes fell 17.40%. Reported costs
were $1.598 versus $1.336, but caching and stochastic outputs confound costs and
time. One call per arm is insufficient for quality parity.

[Source adjudication](paired-adjudication.json) found no demonstrated
omission-caused finding loss: PR303 differs in categorization, and PR300 adds a
full-arm subcase despite shared core evidence. This is not defect ground truth.
The [omission audit](omission-audit.json) found no complete before/after bodies
reintroduced among 81 nominal omissions; partial line overlap remains.

A newly observed PR399 delta from `b7360cabad1cbeb3450bb783ba774eb531e50d36`
to `32843c4e683f86a751584c1933b55f1ccf187022` was fetched after the SVM froze.
Its earlier version is in development, so this is **same-family live evidence**,
not independent future-family validation. Of 26 functions, 25 were retained:
25/25 frontier and 9/9 Claude relevant functions, zero unknowns lowered.
The assessors differ substantially in rubric application. Only 0.33% of source
bytes were proposed lower priority; this security-dense change offers little
savings. The raw live Claude assessment includes all 34 changed units; the
function-only report filters exact IDs without changing labels.

## Reproduce from retained evidence

`evidence.tar.gz` contains cached inputs, frozen numeric models, recipes/folds,
raw assessment prompts/responses, references/disputes, full manifests, paired
packets/results and the live delta. `evidence-manifest.json` binds every member
and the archive digest. Original `/tmp` paths inside receipts are historical
provenance, not prerequisites for the commands below.

Use Python 3.11 and the pinned
[CPU dependencies](../../experiments/lexical-requirements.txt). No GPU, GitHub,
model downloads, or compiled Go extractor are needed for cached replay/refit.

```sh
mkdir /tmp/shoal-v9-evidence
tar -xzf showcases/pr-triage/runs/operations-v9/evidence.tar.gz -C /tmp/shoal-v9-evidence
python showcases/pr-triage/replay_operation_model.py \
  --inputs /tmp/shoal-v9-evidence/reserved/inputs-frozen.json \
  --model /tmp/shoal-v9-evidence/trained/model.json \
  --selection /tmp/shoal-v9-evidence/reserved/selection.json \
  --output /tmp/shoal-v9-replay.json
python showcases/pr-triage/learning_loop.py \
  --artifacts /tmp/shoal-v9-evidence --output /tmp/shoal-v9-learning --refit
```

The new [learning receipts](learning/promotion.json) freeze 988 training IDs,
quarantine seven exact overlaps and nineteen held-out disagreements, refit the
winner, and reproduce all 188 scores with maximum delta zero. All held-out
examples—including disputed ones—remain in independent-assessor evaluation.
Its stricter 98% gate is newly reconstructed and returns **hold**, both for the
65/67 frontier result and post-hoc policy. It does not replace the original 95%
paired-experiment gate. The 12-random/12-targeted audit plan is a retrospective
replay demonstration, not the selection process used to acquire these labels.

The Go operation extractor is untyped syntax: aliases, interface dispatch and
interprocedural flow are unresolved. Facts are capped at 512 and 1200 bytes;
errors/omissions make operations-based models retain affected examples. The
code-only winner is unaffected. Rebuilding the extractor may change its binary
digest through Go build metadata; cached replay deliberately executes no extractor.
