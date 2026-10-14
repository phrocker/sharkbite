# Authorization triage shadow experiment

This external showcase tests the local-decision architecture in
[the design](../../docs/local-decisions.md). It preserves full review for every
change. It does not implement a Shoal review feature or production decision API.

The [budgeted V10 follow-up](runs/prospective-v10/README.md) has started with
one eligible newly observed PR. Four blinded assessment calls cost $1.05 of the
$25 allowance. The frozen classifier retained all 24 sampled functions, so no
savings were proposed. Three family slots remain unfilled; the gate is on hold.

The [V9 comparison](runs/operations-v9/README.md) now favors a code-only CPU SVM:
65/67 frontier-assessed and 64/64 Claude-assessed relevant functions retained,
versus Laya's 61/67 and 59/64. Four complete-PR counterfactual pairs used 16.4%
fewer input tokens. Quality parity and safe exclusion remain unproven.

The [offline learning loop](../../experiments/decision-learning/README.md)
builds training manifests, quarantines conflicting labels and exact overlaps,
refits the winner, and reproduces all 188 held-out scores exactly. Its promotion
record correctly says hold: the new 98% gate is unmet and retrospective.
Full review remains mandatory and PR #410 remains unmerged.

The [first measured run](runs/pilot-v1/report.md) covers four historical PRs,
selected by the frozen [protocol](protocol.json). Of 93 changed Go functions,
55 complete before/after inputs fit the pinned Laya model's 512-token budget;
38 were withheld from inference. Median local inference time was 14.35 ms.
The 53 other units remain explicit full-review dispositions.

Laya's 31 `routine` answers include 21 units proposed for authorization review
by the independent frontier assessor and 12 by Claude. The assessors disagree
on 41 of all 146 units. These are relevance proposals with witnesses, not
verified defects or accuracy measurements. No exclusion or savings claim is
supported by this run.

## Reproduce collection and inference

Run from the repository root, with the historical anchor available in Git:

```sh
go build -o /tmp/shoal-shadow-extract ./showcases/pr-triage/extract
python3 showcases/pr-triage/shadow.py collect \
  --repo . --extractor /tmp/shoal-shadow-extract \
  --output /tmp/shoal-shadow-new-run
```

Use the pinned runtime in
[runtime-requirements.txt](../../docs/testdata/local-decisions/runtime-requirements.txt)
and a CUDA GPU. The measured machine used an RTX 4070 Ti. Prepare the pinned
English checkpoint cache following [the probe](../../scripts/laya_decision_probe.py).
Then use that environment's Python:

```sh
python showcases/pr-triage/shadow.py predict \
  --run /tmp/shoal-shadow-new-run --cache /path/to/laya-model-cache
```

Inference requires the protocol's runtime, device and model revision, runs
offline, and preflights the complete rendered input before calling Laya.
Oversized inputs receive explicit dispositions. Source parsing uses only the
Go standard library; it does not execute reviewed code. Dependency bodies and
caller context are absent. Every unit has `optimization_eligible: false` and
`action: full_review` regardless of the prediction.

## Assessments and report

Give independent assessors only `review-packets/*.json`, without predictions
or historical GitHub reviews. Each assessment must contain a `proposed`
status and exactly one label and witness per unit. The archived Claude prompt
records its instructions. Claude ran with tools, MCP servers, hooks and
session persistence disabled. Its reported model and usage are CLI metadata;
the other assessor's exact model revision and cost were not exposed.

```sh
python3 showcases/pr-triage/report.py --run /tmp/shoal-shadow-new-run \
  --assessment /path/to/first-assessment.json \
  --assessment /path/to/second-assessment.json
python3 -m unittest discover -s showcases/pr-triage -p 'test_*.py'
go test ./showcases/pr-triage/extract
```

Writers refuse to overwrite existing artifacts. To rebuild the archived
report, copy its input JSON files into a fresh directory and pass the two
archived assessments. Report construction validates protocol, manifest and
prediction digests and complete assessment coverage. Cached model file hashes
are recorded separately in `predictor-artifacts.json`.

## What remains

The initial pilot above remains development evidence. Follow-up experiments
add separate retrospective cohorts and adjudication, but there is still no
human-adjudicated defect oracle, prospective quality estimate, paired
downstream review, or measured LLM token savings. The next promotion gate is
a prospective shadow cohort and paired downstream reviews. Caller and policy
consumer evidence is a concrete context improvement for further Laya work.

The files are local experiment artifacts, not production Shoal receipts or a
fully materialized operational graph. The collector, adjudication workflow
and end-to-end integration remain tracked in
[#407](https://github.com/phrocker/shoal-oss/issues/407),
[#408](https://github.com/phrocker/shoal-oss/issues/408) and
[#409](https://github.com/phrocker/shoal-oss/issues/409).
