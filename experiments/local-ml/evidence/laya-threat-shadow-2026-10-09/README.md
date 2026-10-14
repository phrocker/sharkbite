# Laya threat/fault shadow run

This is a four-case local shadow measurement of the pinned Laya English
checkpoint against typed source-risk questions. It is evidence about this
checkpoint and rubric only; it is not a quality estimate, a calibration result,
or permission to exclude human review.

The run used the existing `/tmp/shoal-laya-spike-venv` with Python 3.11.14,
Torch 2.8.0+cu128 and Transformers 5.17.0. Laya resolved
`convaiinnovations/laya` at Hub snapshot
`7b928d828b7b0e022f929d9bd2e44165aa270148`. The model and tokenizer SHA-256
are recorded below so the local cache can be checked before replay:

- `model.safetensors`: `891102d372688fc2a094dac56a384bc537b87c63f21f9f3dac0be2b7cbc8d86c`
- `tokenizer/tokenizer.json`: `6c8aaa9a542084f2457eab775d4eeb51f92a70c0fd9de28d5edb0ddec3c08d30`

`inputs.jsonl` contains four bounded source-change states. `expected.json`
records the analyst's expected labels (routine, critical, critical, routine),
but does not attest that those labels were committed before inference. The
typed risk result was routine for all four: 2/4 against those expectations and
**0/2 on the two expected critical cases**. The fault score stayed in the
none/minor range for all four. This is a negative viability signal for direct
use of this generic checkpoint on this rubric, not evidence that a trained
classifier cannot work.

The checkpoint emitted a warning that one shipped temperature was outside its
valid range and was clamped; confidence for affected entries is therefore
uncalibrated. The results still preserve the raw probabilities and confidence.

Replay, without network access after provisioning:

```sh
HF_HUB_OFFLINE=1 /tmp/shoal-laya-spike-venv/bin/laya \
  --batch inputs.jsonl --predict --model english \
  --questions questions.json --json > replay.jsonl
cmp results.jsonl replay.jsonl
```

This experiment does not authorize promotion, policy mutation, or review
exclusion. The next useful step is a larger authorized labeled corpus and a
trained task-specific head or fine-tune, evaluated on family-held-out snapshots
with calibration and cost/quality gates.
