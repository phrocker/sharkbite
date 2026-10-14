# Local authorization decision models: measured findings

The [V10 prospective-assessment follow-up](runs/prospective-v10/README.md)
retains all 24 sampled functions from the only eligible new family, PR415:
15/15 relevant in each of two blinded Claude passes, with zero proposed savings.
Ten functions overlap exact development bodies/diffs; on the remaining 14,
each pass labels ten relevant. The PR existed before protocol registration.
Four calls cost $1.046898; three family slots remain unfilled. The gate is on
hold for insufficient families and novel positives. No model was retuned.

The [V9 experiment](runs/operations-v9/README.md) establishes a stronger shadow
candidate: code-only SVM retains 65/67 frontier and 64/64 Claude relevant
functions, versus 61/67 and 59/64 for the older adapted Laya. Four complete-PR
counterfactual pairs reduce actual input tokens by 16.4%. This is encouraging,
but does not establish defect-detection parity or safe source exclusion.

Generic AST operation features did not beat code alone. The new
[learning-loop prototype](../../experiments/decision-learning/README.md) uses
988 manifest-selected training examples to reproduce all 188 frozen scores.
It preserves nineteen held-out assessor disagreements and returns hold under
its new retrospective 98% gate. Full review remains mandatory; PR #410 is unmerged.

The newly observed PR399 delta retains all 25 frontier and nine Claude relevant
functions, lowering only one of 26 functions. Its family was seen in development;
this is live same-family evidence, not independent future-PR validation.

The earlier V8 results below remain historical evidence rather than the latest
candidate assessment.

## Supervised classifier comparison: V8

All previous V1–V6 labels were explicitly retired to development: 801 functions.
Before fitting, 17 training rows were removed because their target or contextual
bodies exactly matched reserved target bodies, leaving 784. Five whole-PR folds
also purge matching held-out target bodies from training targets and context.
Near-duplicates and shared contextual code remain possible; the
[overlap audit](runs/context-v8/overlap-audit.json) records these limits.

Ten matched supervised recipes compare code-only and contextual representations,
using character, word and combined TF-IDF features with logistic classifiers.
Context includes bounded callers/callees found by same-directory syntax, with
pinned revisions, evidence hashes, omission counts and unresolved package/build,
receiver, interface and cross-directory edges. This is a measured partial graph
snapshot, not a resolved program graph. Revision and source hashes are receipts,
not learned features.

At a 98% development recall target, the best code-only recipe proposed 3.5% of
source bytes for reduction; the best contextual recipe proposed 3.0%. Context improved the matched C=0.3 variant from 2.6% to 3.0%, but no
context variant beat the best code-only candidate. These are selection
estimates, not held-out performance. The selected code-only mixed-feature C=3
model and its operating threshold were frozen before reserved labels were opened.

| Reserved reference / measurement | CPU classifier | Earlier frozen Laya |
| --- | ---: | ---: |
| Frontier relevance retained | 27/28 (96.4%) | 28/28 (100%) |
| Claude relevance retained | 24/25 (96%) | 24/25 (96%) |
| Proposed lower-priority functions | 50/194 | 62/194 |
| Proposed complete source-byte reduction | 7.1% | 13.7% |
| Proposed function-diff-byte reduction | 12.2% | 22.9% |

Both CPU references identify the missed `CausalInferenceIterator.Seek` in #302:
it chooses the visibility label on derived output. This shows the need to
recognize policy-bearing data propagation, not merely authorization calls.
No unknown reference labels were lowered by the CPU classifier. These are
proposed task-relevance labels, not verified defects. The small retrospective
sample does not validate future exclusion safety.

The CPU model took 206 ms to score the 194 inputs as a batch (about 1.06 ms
amortized); Laya's median GPU inference was 20.49 ms per eligible input. These
are different execution modes, not an end-to-end speed benchmark. Source
extraction/context construction and downstream reviews are excluded. The
selected classifier does not consume contextual features. Its 5 MiB JSON
bundle contains numerical parameters and vocabulary, with class, shape,
finite-value, recipe and evidence-contract validation. Export/restore parity
and a separate 194-input replay are exact.

Evidence: [training recipe](runs/context-v8/training/config.json),
[code-only winner](runs/context-v8/training/candidate-4.json),
[best context candidate](runs/context-v8/training/candidate-7.json),
[frozen selection](runs/context-v8/selection.json),
[frontier comparison](runs/context-v8/frontier-report.json),
[Claude comparison](runs/context-v8/claude-report.json), and
[replay verification](runs/context-v8/replay-verification.json).

The first four code-bearing PRs in the reserved collection were selected for
complete-PR paired reviews before reading labels. Every changed function was
scored, beyond the smaller relevance sample. Those additional functions were
not included in the reserved-target body purge; no independent relevance claim
is made for them. Thirty declarations were nominally lowered; one complete
helper body remained elsewhere in shared context. The omission audit preserves
that fact.

| Four complete-PR paired reviews | Full evidence | Candidate evidence |
| --- | ---: | ---: |
| Input tokens, including cache reads/writes | 361,689 | 351,706 |
| Output tokens | 15,308 | 16,515 |
| Sum of call duration | 160.2 s | 173.5 s |
| Reported API cost | $3.1323 | $3.0766 |

The [bounded source audit](runs/context-v8/paired/adjudication.json) confirms
two findings shared by both arms: integer narrowing can disable the mosaic
budget, and an extraction staleness check compares an explicitly requested
revision with itself. The authorized wrapper mitigates the latter's ordinary
path. Primary implementations behind four full-only claims remained present
byte-for-byte in candidate packets; their security implications are conditional
or unresolved. Other claims remain explicitly unadjudicated. Finding counts
therefore cannot establish defect recall or quality parity.

The [paired report](runs/context-v8/paired/report.json) measures **2.8% fewer
input tokens**, more output tokens, and no speed gain in these calls. Shared
caching, concurrency and stochastic findings confound cost and latency. This
round supports CPU feasibility, not improved review quality or meaningful
end-to-end savings. Adding unresolved neighboring code alone did not solve the
problem. The next representation needs typed policy consumers and propagation
of identity/visibility, with abstention for missing relationships; it must be
selected on development data and tested on new frozen snapshots.

Replay the shipped classifier without a GPU or hosted model:

```sh
gzip -dc showcases/pr-triage/runs/context-v8/reserved-inputs.json.gz > /tmp/shoal-v8-inputs.json
python showcases/pr-triage/context_classifier.py predict \
  --inputs /tmp/shoal-v8-inputs.json \
  --model showcases/pr-triage/runs/context-v8/training/model.json \
  --selection showcases/pr-triage/runs/context-v8/selection.json \
  --output /tmp/shoal-v8-predictions-new.json
```

Use the pinned CPU dependencies. All outputs retain `action: full_review`.

## Earlier live shadow result

The continued live shadow test does **not** support promoting the current
candidate. On four pinned open PRs, it retained 79/86 frontier-assessed relevant
functions (91.9%), below the provisional 95% research target. Seven misses
survived targeted source-based reconsideration. Full review remains mandatory;
PR #410 remains unmerged.

## Continued shadow: existing open PRs and actual paired reviews

The frozen checkpoint and threshold scored every changed function in open PRs
#399, #361, #360 and #357: 153 functions, with 32 proposed reductions. This is an
existing-open-PR snapshot, not prospective data created after registration.
The [protocol](experiments/live-v6.json) selects all open PRs except this
experiment. Exact rendered diff states have no collisions with development or
V5; shared code families and other correlations remain possible.

The [blinded reference](runs/live-v6/frontier.json) and
[comparison](runs/live-v6/frontier-report.json) identify seven lowered relevant
functions: the bound admission `Outstanding`, `Report` and `requestContext`
methods, admission request decoding, embedded lease `AdvanceEpoch` and
`Release`, and the corrupt-manifest fail-closed test. The
[targeted reconsideration](runs/live-v6/miss-adjudication.json) preserves all
seven, with pinned callers and counterarguments. It is the same assessor
reconsidering selected misses, not an independent defect oracle or a complete
new adjudication.

Two independent, tool-free Claude calls per PR compared full and candidate
packets. The first construction was flawed: shared file patches retained 31/32
nominally omitted function bodies. Its 6.1% input-token reduction mostly
measured duplicate removal. Historical prompts, responses, differing hash
conventions and the [audit](runs/live-v6/code-audit.json) remain intact.

The corrected comparison uses complete before/after changed declarations and
source-span separation from shared file context. An
[omission audit](runs/live-v6/paired-v2/omission-audit.json) verifies none of the
32 omitted complete bodies remain. Non-function context stays in both arms;
initializer ordering and rename evidence retain full fallback patches.

| Corrected paired calls | Full evidence | Candidate evidence |
| --- | ---: | ---: |
| Input tokens, including cache reads/writes | 249,461 | 234,798 |
| Output tokens | 12,902 | 10,233 |
| Sum of call duration | 140.8 s | 111.7 s |
| Reported API cost | $2.1864 | $1.9935 |

The measured input reduction is **5.9%**, not the earlier 19.1% function-diff
byte estimate. These four pairs are one stochastic call per arm, with shared
provider caches and concurrent execution. Output, duration and cost differences
cannot be attributed solely to source reduction. The identical doc-only
control prompts also produce different responses and costs. Actual returned
model identities and full usage are preserved in the
[paired report](runs/live-v6/paired-v2/report.json).

The [source adjudication](runs/live-v6/paired-v2/adjudication.json) supports
three manifestations in PR #399: concurrent differing admission reports can be
falsely acknowledged through terminal replay; generic dispatch routes bypass
admission lifecycle checks; and admission APIs expose ordinary same-principal
dispatch claims. The full arm surfaced all three, while the candidate arm
surfaced only the generic-route bypass. These share underlying lifecycle/type
confusion and are not three independent proven vulnerabilities. Core implicated
service code was present identically in both packets, so the difference cannot
be causally attributed to the 32 omissions. Static source adjudication does not
substitute for exploit or concurrency reproduction. Earlier cursor/existence
oracle claims were pre-existing, and the reported Open cleanup leak was refuted.

Paired packet construction uses an explicit one-million-byte prompt bound,
separate from the original collector's 120,000-byte whole-diff packet bound.
Every declaration and context source remains pinned. Hardened replay enforces
frozen selection consistency, preserves pure renames, rejects undecodable paths,
and hashes raw prompt bytes. The [hardening receipt](runs/live-v6/paired-v2/hardening-audit.json)
records byte-identical prompts for this cohort after those repairs.

Rebuild packets and regenerate the corrected report (use new output paths):

```sh
python showcases/pr-triage/paired_review.py \
  --repo . --extractor /tmp/shoal-shadow-extract \
  --manifest showcases/pr-triage/runs/live-v6/manifest.json \
  --predictions showcases/pr-triage/runs/live-v6/predictions.json \
  --selection showcases/pr-triage/runs/live-v6/selection.json \
  --output /tmp/shoal-paired-new
python showcases/pr-triage/paired_report.py \
  --run showcases/pr-triage/runs/live-v6/paired-v2 \
  --output /tmp/shoal-paired-report-new.json
```

The new run therefore fails the relevance gate and has only modest measured
input reduction. Do not describe it as established review-cost viability.
The next candidate needs causal caller/policy-consumer evidence, especially
for small identity-binding and authority-lifecycle helpers, plus explicit
abstention when that evidence is missing. Keep model and threshold selection
separate from new evaluation data. A
[prospective protocol](experiments/prospective-v7.json) is registered for ten
subsequently observed eligible snapshots; no persistent polling service has
been installed and no future results are claimed.

## Earlier reserved result: adapted Laya on changed-code evidence

The frozen candidate uses the pinned English checkpoint with rank-8,
alpha-16 encoder adapters and a trained decision head. Base encoder weights
stay frozen; 29,846,529 parameters are trainable. The input is a five-context-line
function diff with explicit missing caller/dependency context. Four supervised
epochs use three whole-PR development folds to choose an operating threshold;
no evaluation labels are used for fitting or threshold selection.

Training uses 459 adjudicated functions from 20 code-bearing PRs, with all
earlier evaluation sets explicitly retired to development. The next twelve
earlier PRs were frozen separately: 1,312 functions, of which 189 were sampled
uniformly within PR, at most 20 per PR, before scores or labels. The candidate,
weights, renderer and threshold were frozen before opening those labels.

| Reference for the new sample | Laya relevant retained | CPU comparator relevant retained |
| --- | ---: | ---: |
| Blinded frontier | 31/32 (96.9%) | 30/32 (93.8%) |
| Blinded Claude | 29/31 (93.5%) | 29/31 (93.5%) |
| Post-evaluation blinded-to-scores adjudication | 33/34 (97.1%) | 32/34 (94.1%) |

Laya proposes 56/189 functions, containing 41,441/217,271 function-diff bytes
(19.1%) or 40,657/392,928 complete before/after source bytes (10.3%), for reduced
review. The CPU comparator proposes 21.3% of diff bytes but misses two
adjudicated relevant functions. Its five-fold development estimate is not
directly comparable to Laya's three-fold estimate; the final comparison uses
the same reserved sample and frozen thresholds.

The Laya miss is `configureIdentity` in PR #317, an actual identity policy
configuration function. It remains a failure in the record. Claude also marked
the constant `entraDenied` error helper relevant and left three proposed
reductions unknown. Adjudication inspected their pinned callers, preserved
both original judgments, and applied the fixed distinction between policy or
disclosure decisions and generic constant error helpers. This resolves proposed
task relevance; it does not establish a defect oracle or erase reviewer dissent.

187 inputs fit the exact 2,048-token budget; two oversized inputs retain full
review. Median GPU inference was 20.66 ms on the local RTX 4070 Ti. Training
used a low-rank configuration after a full-parameter memory preflight refused
to start; its allocator was capped at half the GPU. The threshold is not a
calibrated probability or an approved exclusion loss budget.

The evaluation scores come from the exported adapter reloaded on its pinned
base. Zero-initialized adapter parity was checked before training. The original
training process did not save final-fit reference logits, so trained in-memory
versus exported/reloaded parity is **not separately established**. Quality
claims here refer to the actual reloaded checkpoint used for evaluation.
An independent replay from the preserved local checkpoint reproduced all 189
scores and proposals exactly; its [verification receipt](runs/reserved-v5/checkpoint-replay-verification.json)
does not substitute for the missing training-process comparison.

Latest evidence:

- [Development recipe](runs/encoder-v4/config.json),
  [PR-separated results](runs/encoder-v4/report.json), and
  [checkpoint identity](runs/encoder-v4/model-identity.json).
- [Frozen Laya selection](runs/reserved-v5/laya-selection.json),
  [sampled manifest](runs/reserved-v5/sampled-manifest.json),
  [actual predictions](runs/reserved-v5/laya-predictions.json).
- [Frontier comparison](runs/reserved-v5/frontier-report.json),
  [Claude comparison](runs/reserved-v5/claude-report.json), and
  [adjudicated comparison](runs/reserved-v5/adjudicated-report.json).

The 114 MiB adapter is preserved in this workspace at
`showcases/pr-triage/local-artifacts/laya-v4/`, outside Git. Its digest and
recipe are versioned; it has not been published to a model registry. With the
pinned GPU runtime and cached base checkpoint, replay the actual candidate:

```sh
python showcases/pr-triage/predict_adapter.py \
  --selection showcases/pr-triage/runs/reserved-v5/laya-selection.json \
  --training showcases/pr-triage/local-artifacts/laya-v4 \
  --manifest showcases/pr-triage/runs/reserved-v5/sampled-manifest.json \
  --cache /tmp/shoal-laya-model-cache \
  --output /tmp/shoal-laya-replay-new.json
```

No generic Shoal production API or PR-review product feature is added. The
next promotion gate is prospective shadow data plus paired downstream reviews
that measure actual inspection cost and missed findings. Small, correlated,
retrospective samples and agent references cannot authorize automatic exclusion.

## Earlier candidates and what changed

The earlier small CPU classifier met the provisional relevance target,
but its proposed reductions contain only **1.4% of before/after source bytes**
or **3.0% of function-diff bytes**. It was a useful baseline, not a viable
review-cost reduction model. Earlier Laya configurations did not meet the
provisional 95% authorization-relevance recall target on separate PRs. No model may exclude
source from review. These measurements concern
agent-proposed task relevance, not verified vulnerabilities, general fault
coverage, or downstream review savings.

## Frozen comparisons

| Candidate and evaluation | Relevant functions retained | Proposed lower-priority functions |
| --- | ---: | ---: |
| Best zero-shot Laya, separate eight-PR cohort, frontier reference | 60/72 (83.3%) | 29/156 (18.6%) |
| Same Laya, Claude reference | 50/58 (86.2%) | 29/156 (18.6%) |
| Lexical baseline, exploratory eight-PR cohort, frontier reference | 70/72 (97.2%) | 29/156 (18.6%) |
| Frozen lexical model, new twelve-PR sample, frontier reference | 69/70 (98.6%) | 31/210 (14.8%) |
| Same frozen lexical model, Claude reference | 45/46 (97.8%) | 31/210 (14.8%) |
| Adapted Laya scorer, new twelve-PR sample, frontier reference | 62/70 (88.6%) | 51/210 (24.3%) |
| Same adapted Laya scorer, Claude reference | 42/46 (91.3%) | 51/210 (24.3%) |
| Frozen lexical model, post-evaluation adjudication | 57/58 (98.3%) | 31/210 (14.8%) |
| Adapted Laya scorer, post-evaluation adjudication | 52/58 (89.7%) | 51/210 (24.3%) |

Both reviewers identify the lexical model's same missed helper,
`loopbackHost` in PR #349. Its caller uses it to decide whether an endpoint
may receive credentials over plain HTTP. The model sees the helper's changed
body but not that caller. Neither reviewer marks any of the lexical model's
31 proposed reductions as unknown. Claude marks two of Laya's proposed
reductions unknown; those are reported separately, never counted as negatives.

The lexical model is character-ngram TF-IDF plus logistic regression. It was
trained on the original 93 functions (three code-bearing PRs within the
four-PR pilot). Its threshold comes from leave-one-PR-out development
predictions. Vocabulary fitting happens inside each fold. The subsequent
twelve-PR assessment changed neither weights nor threshold.

Function counts overstate its utility: most lower-priority functions are tiny
helpers. The [source-volume audit](runs/reserved-v3/source-volume-v2.json) measures
the actual bytes represented by those proposals. The next development round
must rank candidates by meaningful code-volume reduction at high relevance
recall. These byte counts still exclude whole-PR context and are not measured
LLM token savings.

The adapted Laya candidate trains only its existing option scorer, with its
encoder and transformer head frozen. It uses 249 adjudicated development
functions from eight code-bearing PRs, including the retired eight-PR
validation cohort. Ten epochs, learning rate, seed and representation were
fixed before the new sample's labels were opened. Its threshold comes from
leave-one-PR-out development predictions. This is supervised cross entropy,
not the upstream RLCD recipe or full encoder fine-tuning. The negative result
does not rule out a better-trained Laya model.

The CPU model's exported JSON bundle reproduces every one of its 210 measured
scores exactly. Median per-function CPU inference was **0.86 ms**, with about
518 ms to import dependencies and load the bundle in that process. These are
local measurements, not service throughput or an LLM token-savings estimate.

Post-evaluation adjudication preserved both original reviewer judgments and
remained blinded to model scores. It retained the same missed helper for the
lexical candidate. The adjudicated labels supplement the independent results;
they do not replace them with verified defect truth.

## Cohorts, evidence and limitations

The original four-PR pilot remains development data. Five zero-shot
checkpoint/input/task combinations were compared there. Increasing the input
budget to 2,048 tokens and using diffs improved inference coverage from 55/93
to 92/93; better coverage did not imply better ranking. The best development
ranking used complete before/after bodies and the original question.

That candidate and threshold were frozen before inference on eight earlier
PRs. After it failed, those PRs were explicitly retired to development. A
scorer trained on the original pilot alone produced zero reduction on that
cohort. The lexical baseline was introduced after the failure, so its
eight-PR result is exploratory.

For the next test, twelve earlier PRs were frozen by chronological rule.
They contain 1,146 changed functions. To bound assessment work, a seeded
uniform sample of at most 20 functions per PR was selected **before labels
or predictions**, giving 210 functions. Reported percentages describe this
sample; they are not population-weighted estimates or whole-PR savings.
Unsampled and nonfunction units retain full review. The two assessors worked
without candidate scores or each other's labels; Claude had complete sampled
function bodies and whole-PR diffs only where the explicit byte budget allowed.

There are no exact rendered-state duplicates across the three sets. Shared
paths, related change families, repository-specific vocabulary, and overlap
with public model pretraining remain possible. Functions within a PR are
correlated. Small retrospective cohorts and agent judgments do not establish
a statistical lower bound on future defect recall. Score thresholds are
operating points; neither model's scores are calibrated probabilities.

Evidence:

- [Development candidate selection](runs/candidates-v2/selection.json),
  [adjudication](runs/candidates-v2/adjudication.json), and
  [original lexical baseline](runs/candidates-v2/lexical-v1.json).
- Eight-PR [frontier comparison](runs/validation-v2-predictions/frontier-report.json)
  and [Claude comparison](runs/validation-v2-predictions/claude-report.json).
- Twelve-PR [sampling manifest](runs/reserved-v3/sampled-manifest.json),
  [frozen lexical selection](runs/reserved-v3/lexical-selection.json),
  [frontier comparison](runs/reserved-v3/frontier-report.json),
  [Claude comparison](runs/reserved-v3/claude-report.json),
  [post-evaluation adjudication](runs/reserved-v3/adjudicated-report.json), and
  [replay verification](runs/reserved-v3/replay-verification.json).
- [Adapted Laya recipe](runs/scorer-v2/config.json) and
  [PR-separated predictions and final scores](runs/scorer-v2/result.json).

## Run the frozen candidate locally

Use Python 3.11 and the pinned
[CPU dependencies](experiments/lexical-requirements.txt). No GPU or hosted
model is required for replay:

```sh
python showcases/pr-triage/local_model.py \
  --bundle showcases/pr-triage/runs/reserved-v3/lexical-model-v2.json \
  --manifest showcases/pr-triage/runs/reserved-v3/sampled-manifest.json \
  --output /tmp/shoal-local-replay-new.json
```

The bundle contains numerical parameters and vocabulary, not pickle or
executable code. The loader validates content identity, model contract,
training recipe and renderer identity, parameter shapes and finite values.
Every output retains `action: full_review`
and `optimization_eligible: false`; `lower_priority` is only a proposal.
Use a new output path for each observation.

Adversarial code review found that the original frozen selection did not
mechanically enforce its training-recipe identity. The schema-2 bundle and
replay selection add that enforcement. The
[post-run attestation](runs/reserved-v3/recipe-binding-audit.json) records this
limitation and exact score parity; it does not claim enforcement existed
before the original evaluation. Historical selections and outputs remain
unchanged. Initializer-order coverage was also strengthened; recollecting all
three populations confirmed unchanged case contents for these actual cohorts.

The full population manifest is archived as deterministic gzip alongside its
uncompressed digest. Training configs preserve original experiment paths;
the archived records identify the exact source manifests, labels and inputs.
Binary Laya scorer weights remain local experiment artifacts, identified by
hash in each result. They are reproducible from the pinned training recipe,
but are not the shipped CPU candidate.

## Implications for Shoal

Keep the generic decision contract independent of Laya. A model bundle needs
its task/rubric, input representation, training and calibration lineage,
evaluation evidence, operating threshold and limitations. CPU classifiers and
Laya should use the same observation/proposal/action separation.

The operational picture should carry pinned caller and policy-consumer
evidence, plus explicit missing edges. The missed transport helper is a
concrete context-acquisition case. Do not hide incomplete context behind a
high model score or treat a graph snapshot as complete authorization evidence.

The next promotion gate is a prospective shadow cohort and paired downstream
reviews measuring actual inspection cost and missed findings. Keep reference
adjudication separate from model outputs; retraining consumes retired
development data and reserves new evaluation data. PR triage stays an external
showcase of these generic capabilities.
