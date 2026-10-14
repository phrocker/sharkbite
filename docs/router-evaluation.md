# Router evaluation (pre-registered)

Router v1, slice 1 of #500. This note was committed before the held-out test
split was run for the first time; that run's results were then appended
below, unchanged. The protocol, metrics and denominators in this section were
fixed at that commit and must not be edited after it.
[local-language.md](local-language.md) describes the router.

## What is measured

Two systems route the same fixture texts:

- **router**: `pkg/router` analysis, the target-choice decision served by
  `internal/decisionlinear` with the checked-in model, aggregation, then slot
  filling and validation. It runs through `routershadow.Decider`, the code
  the shadow service uses.
- **baseline**: `Analysis.Baseline` (`shoal.router.baseline/v1`). If a
  pattern covers the whole text, that target wins. Otherwise the target is
  the one with the highest token-overlap recall, if that recall is at least
  0.5 and leads the next target by at least 0.2. If neither holds, the
  baseline abstains. It fills and validates slots with the router's code.

Visibility comes from the fixture. A caller sees nodes, descriptors, profiles
and the ontology whose source it holds. The authorized composition is tested
separately (`internal/routershadow`), not measured here.

## Fixtures

All fixtures are in `pkg/router/testdata/eval/v1`.

- `world.json` is a synthetic world:
  - 18 nodes: services, teams and environments, including a shared `core`
    alias and one node under a secret source;
  - 6 descriptors, two of which offer the same action;
  - 3 decision profiles. One is `service-operation-risk`, modelled on #421;
    another is secret;
  - an ontology with 3 directed relations, which gives 6 lookup templates.
- `grammars/*.json` holds 18 grammars, one per target.
- `cases.jsonl` holds 292 labelled cases. `generate.py` regenerates it, with
  a fixed seed.

Each paraphrase template belongs to exactly one split, so no test-split
phrasing appears in train or dev. A (text, caller) pair occurs once.

**Grammars were written from the train and dev phrasings only.** The test
split therefore measures paraphrase generalization, not recall of
memorized phrasings. One author wrote the templates, test paraphrases and
grammars at the same time. That is a known limitation: the test paraphrases
are not independent of the grammar author.

| Split | Cases | Answerable | Unanswerable | SplitDigest (SHA-256 of its lines) |
|---|---|---|---|---|
| train | 139 | 101 | 38 | `8d06b7e562a1b665ffab26d12071cb2a7ed3dc901174a9ed1632f9b6665c5ef1` |
| dev | 50 | 37 | 13 | `34bf7f1a941604280c2f8940cedd9d1b07071ff395ecaba5cab8c2c3488d5bfd` |
| test | 103 | 74 | 29 | `83b9411380288a69baf5cb5ffd97c4862c760921d96ecabee6a901d2c94ffab6` |

`eval.PreRegisteredTestDigest` pins the test digest above. `eval.PreRegistered`
refuses to release a test split with any other digest, and refused to run
the test split at all before this commit.

A case is **answerable** when its expected kind is action, decision or
lookup. It is **unanswerable** when the expected outcome is an abstention:

- no visible target, including hidden-target twins;
- an empty text;
- an ambiguous executor;
- an ambiguous mention;
- a missing slot, including hidden-node and nonexistent-node twins;
- a slot that does not fit.

## Model

- Trained by `router.Train`, an averaged perceptron over 30 epochs with each
  positive repeated 4 times, in fixed order, on the **train split only**.
- `pkg/router/testdata/model/router-pair-v1.json`:
  - SHA-256: `f11fa3aca2f97d33cbd72d674d26b01fe4c4dc074bf6068de312bc63c1d82a08`
  - `dataset_sha256`: the train SplitDigest
  - `recipe_sha256`: `f980ade09baeca799f961069942255665dd9fe65f64ce189e3b5db705e4040a6`
- Predictor identity on go1.26.4/linux/amd64:
  `decision:predictor:v1:b17d76b7609525501b7adce5fcd62cbab6de6e58217ad992634487ef486d3127`
  (`TestGoldenPredictorID`).
- `TestModelArtifactIsReproducible` retrains and requires identical bytes.

## Metrics (fixed)

Every metric is reported as an exact N of D, with the percentage only as a
gloss. Correctness:

- **Target-correct**: a non-abstained proposal whose kind and target key
  (`TargetRef.Key()`) equal the expected ones, on an answerable case.
- **Slots exact**: the proposal's slots, as fixture node keys or enum values,
  equal the expected slot map exactly: no missing, extra or different slot.
- **End-to-end correct**:
  - answerable case: target-correct and slots exact. **An abstention on an
    answerable case is wrong.**
  - unanswerable case: an abstention, whatever its reason.

| Metric | N | D |
|---|---|---|
| Target precision on non-abstained | target-correct | proposals that did not abstain (all cases) |
| End-to-end accuracy | end-to-end correct | all cases |
| Coverage | answerable cases with a non-abstained proposal | answerable cases |
| Abstention rate | abstentions | all cases |
| Correct abstention | unanswerable cases that abstained | unanswerable cases |
| Slot exact-match | target-correct proposals with exact slots | target-correct proposals |
| Abstention reason agreement (secondary) | abstentions whose single reason is the expected one | unanswerable cases that abstained |
| Per-kind confusion | count per (expected kind, proposed kind) | — |

**Paired comparison** with the baseline uses end-to-end correctness:

- report both-right, router-only, baseline-only and both-wrong counts;
- the discordant pairs are router-only and baseline-only;
- the exact two-sided McNemar p-value is a binomial test on the discordant
  pairs at 1/2.

The primary outcome is router-versus-baseline end-to-end accuracy on the test
split. Secondary outcomes are everything else. End-to-end correctness is also
broken down by case tag (plain, injection, hidden_target, hidden_node,
nonexistent_target, nonexistent_node, missing_slot, ambiguous_mention,
ambiguous_executor, slot_mismatch, unanswerable). These breakdowns are
descriptive; most have fewer than ten cases.

**Latency** is measured on CPU on the host named with the results, by:

- `BenchmarkAnalyze` (pure analysis);
- `BenchmarkRoute` (analysis, the decision, aggregation and the baseline);
- `BenchmarkServiceRoute` (the whole shadow path over the real authorized
  composition with a memory policy store).

These use train and dev texts only.

## Rules

1. The test split is run once, at the commit after this one. Nothing under
   `pkg/router`, its fixtures, grammars or model, or `internal/routershadow`
   is changed between this commit and that run.
2. Results are reported whatever they are, in the section below. Errors found
   later are fixed in later changes and reported as such; this run's numbers
   are not replaced.
3. Any analysis not listed here is labelled exploratory.

## Before the test run

Train and dev are saturated: on both, router and baseline are end-to-end
correct on every case except two train cases, where the baseline missed an
injected decision text. This is expected, since the grammars were written
from those phrasings. Dev therefore gave no tuning signal. No threshold,
feature or grammar was tuned after any dev result. The reports are
`report-train.md` and `report-dev.md`.

**Expectation, stated before the run:** test accuracy will be clearly lower
than dev, for both systems. Test phrasings were held out of the grammars. Some
use cue words no grammar has ("cycle", "kick", "evacuate", "resize").

## Results

The test split was run once, on the tree of the pre-registration commit
`c9aa8bc3`, with no change to code, fixtures or model. The results were
committed in the commit after it. The full report is
`pkg/router/testdata/eval/v1/report-test.md`.

| Metric (test, 103 cases: 74 answerable, 29 unanswerable) | router | baseline |
|---|---|---|
| Target precision on non-abstained | 9 of 9 | 32 of 32 |
| **End-to-end accuracy** | **36 of 103** | **59 of 103** |
| Coverage over answerable | 9 of 74 | 32 of 74 |
| Abstention rate | 94 of 103 | 71 of 103 |
| Correct abstention on unanswerable | 29 of 29 | 29 of 29 |
| Slot exact-match over target-correct | 7 of 9 | 30 of 32 |
| Abstention reason agreement (secondary) | 24 of 29 | 23 of 29 |

**Paired comparison (end-to-end):**

| | count |
|---|---|
| Both right | 32 |
| Router right, baseline wrong | 4 |
| Baseline right, router wrong | 27 |
| Both wrong | 40 |

The exact two-sided McNemar p-value is 3.4 × 10⁻⁵.

**The primary outcome is negative. On held-out phrasings the router is worse
than the lexical baseline: 36 of 103 against 59 of 103.** Both systems
proposed nothing wrong:

- every non-abstained proposal had the right target (9 of 9, and 32 of 32);
- every unanswerable case abstained, for both.

The router is simply far more conservative. It covered 9 of 74 answerable
cases; the baseline covered 32 of 74.

Per-kind confusion. Rows are the expected kind, columns the proposed kind.

| expected | router: action | router: decision | router: lookup | router: abstain | baseline: action | baseline: decision | baseline: lookup | baseline: abstain |
|---|---|---|---|---|---|---|---|---|
| action | 7 | 0 | 0 | 29 | 16 | 0 | 0 | 20 |
| decision | 0 | 2 | 0 | 14 | 0 | 6 | 0 | 10 |
| lookup | 0 | 0 | 0 | 22 | 0 | 0 | 10 | 12 |
| abstain | 0 | 0 | 0 | 29 | 0 | 0 | 0 | 29 |

End-to-end correctness by tag:

| Tag | router | baseline |
|---|---|---|
| plain | 4 of 69 | 28 of 69 |
| injection | 2 of 2 | 0 of 2 |
| hidden_target | 2 of 4 | 3 of 4 |
| every other tag | all | all |

The other tags are hidden_node, nonexistent_target, nonexistent_node,
missing_slot, ambiguous_mention, ambiguous_executor, slot_mismatch and
unanswerable. Each has three or fewer cases, except unanswerable, which has
16.

Two of the router's nine target-correct proposals have wrong slots. Both are
"hard restart ...": "hard" is a test-only phrase for the force mode, so the
mode slot stayed empty.

### Latency

Host: AMD Ryzen 9 7950X, Go 1.26.4, linux/amd64. Run with
`go test -bench . -benchtime 2s`, once.

| Benchmark | ns/op | B/op | allocs/op |
|---|---|---|---|
| `BenchmarkAnalyze`: tokenize, mentions, 18 grammars, features | 8,789 | 3,208 | 29 |
| `BenchmarkRoute`: plus the decision and its records, aggregation, slots and the baseline | 401,746 | 447,454 | 4,210 |
| `BenchmarkServiceRoute`: the whole shadow path, memory policy store | 1,267,579 | 1,591,885 | 17,327 |

Matching and featurizing take about 9 µs. Most of the 0.4 ms routing cost
comes from building and validating the decision contracts. That work hashes
the task, picture, request and prediction identities on every routing. The
authorized composition adds about 0.9 ms more:

- fleet enumeration;
- the padded mention batch of 2048 IDs;
- the published-ontology check.

### Why the router lost (exploratory, not pre-registered)

The train split holds the phrasings the grammars were written from. So on
train, a whole-text pattern match (`pattern_full`) almost always marks the
right target. The perceptron learned to rely on it:

- `pattern_full` and `pattern_cover` carry large positive weights;
- the intercept is −4.66.

A text with no matching pattern therefore needs high cue and vocabulary
overlap to reach a zero margin, and held-out phrasings rarely get there.

The baseline falls back to token-overlap recall, which handles phrasings
like "wipe the cache of ledger". Two kinds of text are not reached by either
system:

- texts whose cue word no grammar has ("cycle", "kick", "evacuate",
  "dependents", "deployed");
- texts whose only cue is a mention ("payments dependencies").

The router's two wins on injection come from the decision: the injected text
was outside the pattern, and the router still chose the decision. The
baseline's vocabulary recall fell below 0.5 on the extra words.

What this suggests for later slices, none of it done here:

- train on data where patterns do not cover the positives, such as
  paraphrases held out of the grammars, or drop `pattern_full`;
- calibrate the threshold, or use a margin provider (#501);
- add cue synonyms through the encoder tier.

These are ideas, not results. Any rerun needs a new pre-registration and a
new test split.

## Erratum (added after review; the pre-registered result above is unchanged)

Review of #519 found the following. The sections above are as they were
when the results were committed. Nothing in this section changes the
pre-registered numbers.

### A test-only cue leaked into a grammar

The fixtures section says the grammars were written from train and dev
phrasings only. That is not quite true:

- `grammars/breach_exposure.json` carries the cue `exposed`. The only text
  that contains it is the test template "how exposed is {svc} to a breach"
  (`generate.py`, the hidden-target block).
- A word-level audit of every grammar cue, pattern word and enum phrase
  against the train and dev texts finds one other word that occurs only in
  test texts: `rolling` (the enum phrase "rolling back" in
  `service_operation_risk.json`).
  - This one is borderline rather than a leak. The phrase comes from the
    generator's shared `OPS` table, which every split's decision templates
    draw from. No train or dev case happened to draw the rollback operation
    in its "-ing" form.
  - Every grammar word occurs in some fixture text, except a few that occur
    in none: `readiness`, `sev`, `1`, `2`, `3`, `forcefully`, `graceful` and
    `bouncing`.

**Re-run with the leak removed (exploratory, not pre-registered).** The
committed grammars were left as they are. The re-run used a temporary copy of
the fixtures, with the same test split (through the gate), model and code.

| | router end-to-end | baseline end-to-end | discordant (router-only / baseline-only) |
|---|---|---|---|
| Pre-registered | 36 of 103 | 59 of 103 | 4 / 27 |
| Without the cue `exposed` | 36 of 103 | 59 of 103 | 4 / 27 |
| Without `exposed` and the phrase "rolling back" | 36 of 103 | 56 of 103 | 4 / 24 (p = 0.00018) |

Removing `exposed` changes no number. The reviewer independently got the
same 36 of 103 against 59 of 103. Removing "rolling back" as well only
lowers the baseline, because the router abstained on those cases anyway. The
conclusion stands: the router is worse than the baseline on held-out
phrasings.

### What the commit history can and cannot show

- **What it shows:** the pre-registration commit `c9aa8bc3` fixed the test
  digest before the results commit, and the gate refused the test split
  until that digest was set.
- **What it cannot show:** that the test split went unrun before
  `c9aa8bc3`. The cases, grammars and model landed in `c48a54a6` 45 seconds
  earlier, in the same working session. They could have been run against the
  test split in that time, and only the author's account says they were not.
- **Since this erratum, the test split is reachable only through the gate.**
  Before it, `eval.Split(cases, "test")` bypassed the gate. Now:
  - `Cases.Split` refuses it;
  - `Cases.PreRegistered` is the only way to the test split;
  - the gate is tested.

**Protocol for v2.**
1. Generate the test split's cases and commit their digest in a pull request
   of its own, merged before any grammar, feature or model work for that
   version begins.
2. Write the grammars and train the model only from the train and dev splits
   afterwards, and run the word audit above before the test run.
3. Run the test split once, through the gate.

### Predictor identity on other toolchains

`TestGoldenPredictorID` used to pin the identity only on go1.26.4/linux/amd64,
and on other toolchains only logged it. CI runs the go.mod version.
`decisionlinear.IdentityFor` is now a pure function of (model SHA-256,
feature schema, Go version, OS, architecture), and `New` uses it. The test
checks two things on every toolchain:

- the go1.26.4/linux/amd64 golden;
- that the running provider's identity is `IdentityFor` of the running
  environment.
