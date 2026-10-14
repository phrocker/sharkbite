# Durable inventory training evidence

Produced from code commit `f65260cf699f296b65c2d73d226ef152ad91930b` on 2026-10-08,
using the pinned local Python environment (NumPy 2.4.6, SciPy 1.17.1,
scikit-learn 1.7.2, threadpoolctl 3.7.0) and the current
`run_adjudicated_showcase.py` runner. No paid inference or model promotion occurred.

Both finite synthetic cohorts contain 16 targets: 14 verified, one unknown and
one disputed, with eight eligible training rows. Two fits of each pinned export
produced byte-identical models. Each first durable inquiry used one provider call;
a separate process replayed it with zero calls and identical receipt/prediction
identities. Initial and final inventory vectors matched for all 16 targets.

The source run took 1.23 seconds to prepare and about 0.53–0.55 seconds per fit;
the review run took 1.18 seconds to prepare and about 0.55–0.57 seconds per fit.
These are single-run observations, not latency estimates for operational workloads.
Exact windows, hashes and identities are in `summary.json`.

Retained files include datasets, manifests, capture proofs, canonical historical
snapshot captures, source/witness/role fixtures, models and training provenance.
Engine/RFile directories and duplicate fit outputs are omitted; the integration
tests separately exercise physical RFile restart and pending-publication repair.
The earlier `adjudicated-training-2026-10-08` evidence remains unchanged.

The accompanying adversarial tests show that a new ordinary outcome Append alone
quarantines a previously verified label (14→13 verified), pending publication
blocks export, exact retry repairs the original receipt, and concurrent exports
use isolated capture sessions. Historical snapshot bytes round-trip through the
canonical codec, including opaque reporter identities.

This evidence establishes pipeline behavior for compiled fixtures. It does not
measure threat-analysis quality, human independence, calibration, LLM savings,
GitHub-wide completeness, production authority deployment or automatic promotion.
