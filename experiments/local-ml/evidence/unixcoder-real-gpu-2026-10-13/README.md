# Real UniXcoder CUDA challenger smoke

This run exercises the actual pinned `microsoft/unixcoder-base` checkpoint on
CUDA through `FrozenUnixCoder`, the deterministic linear head, and replay. It
is runtime/integration evidence only: the six labels are synthetic smoke labels
and do not support a quality, calibration, savings, promotion, or review
exclusion claim.

The local runtime was an NVIDIA GeForce RTX 4070 Ti with driver `580.159.03`,
Torch `2.8.0+cu128`, and Transformers `5.17.0`. The checkpoint is pinned to
revision `5604afdc964f6c53782a6813140ade5216b99006`; weights and tokenizer
identities are retained in `report.json`. The run produced finite 768-dimensional
embeddings, six complete predictions, `optimization_enabled: false`, and an
elapsed fit/prediction time of about 1.9 seconds after checkpoint loading.

Replay command, using only the provisioned cache:

```sh
HF_HUB_OFFLINE=1 python3 experiments/local-ml/run_unixcoder_real_smoke.py \
  --snapshot /path/to/snapshots/5604afdc964f6c53782a6813140ade5216b99006 \
  --device cuda --output /tmp/unixcoder-gpu-smoke.json
```

Any future authorized evaluation must replace these synthetic labels with an
independently admitted, family-held-out dataset and add calibration, drift,
cost, and selective-risk measurements.
