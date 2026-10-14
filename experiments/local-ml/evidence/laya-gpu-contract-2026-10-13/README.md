# Pinned Laya GPU contract probe — 2026-10-13

This directory retains a reproducible runtime-contract probe for the pinned
`convaiinnovations/laya` checkpoint at revision
`55cf4c4ebb4ebe31b2550e8bdf3bd21b99753851`. The probe ran offline from the
local Hugging Face cache on an NVIDIA GeForce RTX 4070 Ti (driver 580.159.03)
using CUDA through PyTorch 2.8.0 and Laya 0.3.21.

It exercised authenticated typed choice, ordinal score, and proposition
probability requests, malformed-request rejection, bounded oversized input,
unknown-model behavior, health reporting, and two identical calls per sample.
The report records effective artifact digests, cold/warm timings, device
identity, and native rounded probabilities. `comparison.json` compares the
same cached checkpoint with the CPU contract probe retained alongside this
report; it is a runtime/device comparison, not a quality evaluation.

This is runtime and transport evidence only. It makes no calibration,
accuracy, savings, promotion, or review-exclusion claim. The checkpoint and
inputs are synthetic contract-probe material; authorized held-out evaluation
remains a separate gate.
