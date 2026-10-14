# Local decision architecture: validation report

Date: 2026-09-29. Shoal baseline: `463c934` (main after #397).
Architecture: [Local decisions](local-decisions.md).

This report validates integration assumptions and existing Shoal seams. It is
not a code-analysis benchmark, quality gate, or completed production integration.
All model inputs here are synthetic snippets, not repository/private source.

## Environment and reproducibility

- Linux, NVIDIA GeForce RTX 4070 Ti, 12,282 MiB, driver 580.159.03.
- Isolated Python 3.11.14 environment; Laya 0.3.21; PyTorch 2.8.0+cu128;
  Transformers 5.17.0. The host default Python is 3.12.3; `uv venv` selected its
  available 3.11.14 runtime, recorded here rather than assumed.
- Upstream inspected at
  [`9d955671415fc19f069b9cc998928075c1f255ec`](https://github.com/NandhaKishorM/laya/tree/9d955671415fc19f069b9cc998928075c1f255ec).
- Executed published PyPI package `laya==0.3.21`; the source checkout was used for
  contract inspection, not substituted for the installed package.
- English checkpoint `convaiinnovations/laya`, immutable model revision
  [`55cf4c4ebb4ebe31b2550e8bdf3bd21b99753851`](https://huggingface.co/convaiinnovations/laya/tree/55cf4c4ebb4ebe31b2550e8bdf3bd21b99753851).
- Effective artifacts include weights, encoder config, tokenizer data/config,
  and RL agent configuration. SHA-256 values are in each probe report, after
  runtime initialization (which can normalize tokenizer configuration).
- [Observed dependency versions](testdata/local-decisions/runtime-requirements.txt),
  [online-mode output](testdata/local-decisions/online-probe.json), and
  [offline-mode output](testdata/local-decisions/offline-probe.json) are retained.
  The requirements file pins versions; it is not a supply-chain hash lock.
- [Reproduction script](../scripts/laya_decision_probe.py) uses an in-process ASGI
  TestClient against the real Laya worker and real GPU weights. It does not test
  a network listener, TLS, queue saturation, multiple processes, or deployment.

Reproduce in a disposable environment with an explicitly selected Python version:

```bash
uv venv --python 3.11 /tmp/shoal-laya-probe-venv
uv pip install --python /tmp/shoal-laya-probe-venv/bin/python \
  -r docs/testdata/local-decisions/runtime-requirements.txt
/tmp/shoal-laya-probe-venv/bin/python scripts/laya_decision_probe.py \
  --revision 55cf4c4ebb4ebe31b2550e8bdf3bd21b99753851 \
  --device cuda --cache /tmp/shoal-laya-probe-cache \
  --output /tmp/laya-online-probe.json
/tmp/shoal-laya-probe-venv/bin/python scripts/laya_decision_probe.py \
  --revision 55cf4c4ebb4ebe31b2550e8bdf3bd21b99753851 \
  --device cuda --cache /tmp/shoal-laya-probe-cache --offline \
  --output /tmp/laya-offline-probe.json
```

Online provisioning downloads the checkpoint into the named cache. The second
run starts a fresh process with `HF_HUB_OFFLINE=1` and
`TRANSFORMERS_OFFLINE=1`, using cached artifacts. This verifies those libraries'
offline path, not an OS-level egress sandbox. GPU-required execution checks both
availability and the actual loaded device. No serving fallback to a hosted model
is configured.

## Results

Both completed probes passed. Three synthetic inputs were each requested twice,
with three question types per request; results are retained without declaring an
accuracy score. Effective artifact hashes matched between runs, as did the
reported sample probabilities in these runs. This does not promise bitwise
reproducibility across hardware, runtime revisions, or batching.

| Check | Observed behavior | Architectural consequence |
| --- | --- | --- |
| Choice | Native label-keyed probability map and selected label. | Validate exact task labels; retain the native distribution. |
| Ordinal score | Native expected score, index-keyed probability map, and index-to-rubric legend. | Preserve rubric ordering and validate the returned legend. |
| Proposition (`noul`) | Native `P(true)` scalar; this probe supplies explicit true/false criteria. | Validate the scalar; any constructed false/true pair is a documented derivation, not a native distribution field. |
| Probability normalization | Upstream rounds each probability to four decimal places; sums can differ from one by 0.0001. | Use an explicit rounding bound (`0.00005 * number_of_labels` plus numeric epsilon). Do not silently renormalize malformed output. |
| Confidence fields | Choice/score `confidence` differs from `answer_confidence`; proposition probability differs from confidence in its selected polarity. | Never conflate entropy-derived confidence, maximum probability, and P(true). Calibrate on the actual task. |
| Missing bearer | HTTP 401. | Worker auth is available; Shoal still owns task/evidence authorization. |
| Missing questions / null state | HTTP 400. | Malformed input has an explicit failure, not a negative label. |
| Zero token budget / invalid question type | HTTP 422. | Preserve validation-failure semantics in the adapter. |
| Unknown model alias | HTTP 200 with automatic routing. | Shoal must reject unknown/unpinned identities before calling upstream. |
| Over-token input | 1,207 raw state tokens accepted with model `max_len=512`; HTTP 200 and no explicit state-truncation flag. | Byte caps do not prove evidence fits. Preflight exact state/question/option encoding, and make any truncation ineligible for optimization. |
| Larger byte/character input during probe development | The initial 52k-character input was rejected (HTTP 413). | HTTP size rejection and tokenizer truncation are different limits; the final probe intentionally isolates the latter. |
| Health after inference | Exact revision, actual CUDA device, and zero CPU fallback count. | Check effective device and revision, not requested preference alone. |
| Cached reload with offline flags | A new process loaded the same artifacts and completed the same checks. | Provisioning and serving can be separated; enforce actual deployment egress policy separately. |

The checkpoint emitted a calibration warning: the shipped `choice:11+`
temperature `0.10058280825614929` was clamped to `0.5`. This bucket is not the
three-option choice exercised here. Record effective calibration and warnings;
no claim follows that the other buckets are calibrated for code review.

The first recorded prediction request took about 345 ms; subsequent short
requests in the online-mode report took about 16–18 ms. Cached model construction
took about 1.66 seconds. These are individual in-process observations from a tiny
synthetic workload, not service p95, throughput, cold-download latency, or a cost
projection. The raw reports include both runs' timings.

The choice examples returned `review` for removal of an explicit check,
`routine` for a comment-only change, and `insufficient` for a missing helper.
The ordinal output favored the lowest review-priority level even for the removed
check. This contrast illustrates why the question/rubric and task evaluation
matter; neither three plausible choices nor a typed output establishes a useful
review policy. No frontier-review recall or exclusion threshold was tested.

Probe development corrected two assumptions against observed upstream behavior:
invalid token budgets use 422, and probability maps require rounding tolerance.
The over-token fixture was shortened to stay below the separate HTTP character
cap. These corrections are retained here to explain why the final probe's bounds
and status expectations differ from a generic strict-JSON classifier.

## Existing Shoal conformance

The following passed on the recorded baseline:

```text
go test ./pkg/code ./pkg/contextpack ./pkg/inference/... ./pkg/explorer/authorized -count=1
go test ./pkg/explorer -run 'Snapshot|Interaction.*Snapshot' -count=1
```

These verify the existing code/evidence/inference/authorized contracts, including
source snapshot behavior. They do not test proposed `pkg/decision` code, which
does not yet exist. The probe script was also checked for Python syntax.

## Adversarial architecture review

A Claude CLI review ran with tools disabled, no session persistence, and a $5
budget cap. The reported model was `claude-opus-5-5`; reported cost was about
$0.41. The model received only the architecture draft and review instructions.
This is an asserted provider/model identity from the CLI result, not independent
model attestation. A separate read-only repository review checked the existing
seams and then the revised document.

Material corrections incorporated into the architecture and issue acceptance:

- Coverage starts with all changed files/declarations, including policy/config,
  and separates collection denominators from unknown dependency completeness.
- Prediction, label, and action units and aggregation rules are explicit.
- Relevance witnesses and confirmed-defect evidence have different rubrics;
  challengers inspect randomized negatives as well as positive findings.
- Full review has unknown recall. Blinding, prospective/reconstructed cohorts,
  duplicate/future-label isolation, and later seeded-fault tests are explicit.
- Authenticated submitting principals differ from asserted model provenance;
  label/adjudication authority differs from observation-submission permission.
- Idempotency reserves a canonical request before inference, so nondeterministic
  recomputation does not create conflicting result identities after a crash.
- Retained source/artifact manifests, not graph snapshot IDs alone, enable replay;
  current access is intersected with the original scope.
- A valid regression reproducer needs an independently checked oracle and
  head/base-or-fix comparison, with dependencies provisioned before isolated runs.
- Paired full/subset reviewer executions measure actual downstream detection,
  separately from merely retaining evidence associated with a finding.
- A prediction-dependent optimization needs a durable receipt; conservative
  full review remains available when prediction or recording is unavailable.

Open empirical questions remain: whether this checkpoint beats simple baselines,
whether source-code context fits reliably, whether labels/positive-case volume
are sufficient, and whether measured savings survive actual downstream review.
Those are implementation/evaluation work, not resolved by architecture review.
