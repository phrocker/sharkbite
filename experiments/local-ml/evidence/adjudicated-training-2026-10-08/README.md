# Adjudicated training conformance evidence

Executed on 2026-10-08 at code commit `be99cd82366c08dc4eea1f323f9588cd46a11c13` using the pinned Python environment and `run_adjudicated_showcase.py`. The source and review cohorts each contain 16 synthetic registered examples: 14 verified, one unknown and one disputed. Eight verified training rows enter each fit. Both repeated fits produced byte-identical model artifacts. Each model ran once on its registered inquiry and reopened with zero model calls and identical request/receipt/prediction IDs.

`summary.json` records exact pins and measured wall times (including process startup). Per-task exports, provenance manifests, candidate models, recipes, runtime records and training receipts are retained here. The full RFile-backed run is at `/tmp/shoal-adjudicated-training-final-20261008`; engine files are not part of this repository evidence. Re-run the documented command to produce a fresh durable session.

These are synthetic protocol and numerical-conformance measurements. They do not establish defect-detection quality, calibration, unbiased population metrics, savings or human independence. No paid inference or promotion occurred; source review remains mandatory.

Independent adversarial review found and fixed mutable verification material, loading all targets before aggregate bounds, and signed-zero commitment mutation. A fresh full integration review, separate Python review and separate fixture/inquiry review were clean. Targeted race tests/vet and 29 Python tests passed.
