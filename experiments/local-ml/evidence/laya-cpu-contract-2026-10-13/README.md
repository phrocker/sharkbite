# Pinned Laya CPU contract probe — 2026-10-13

This directory retains a reproducible runtime-contract probe for the pinned
`convaiinnovations/laya` checkpoint at revision
`55cf4c4ebb4ebe31b2550e8bdf3bd21b99753851`. The probe ran fully offline from
the local Hugging Face cache with Laya `0.3.21`, Transformers `5.17.0`, and
PyTorch `2.8.0` on the CPU.

It exercised authenticated typed choice, ordinal score, and proposition
probability requests, malformed-request rejection, bounded oversized input,
unknown-model behavior, health reporting, and two identical calls per sample.
The report records effective artifact digests, load/inference timings, and
native rounded probabilities.

This is runtime and transport evidence only. It makes no GPU, calibration,
accuracy, savings, promotion, or review-exclusion claim. The GPU acceptance
for #404 still requires a stable CUDA device and a separately provisioned
pinned checkpoint run.
