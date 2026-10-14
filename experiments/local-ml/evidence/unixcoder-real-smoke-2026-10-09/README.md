# Real UniXcoder challenger smoke

This run exercises the actual frozen `microsoft/unixcoder-base` checkpoint
through `FrozenUnixCoder`, the deterministic linear head, and replay. It is a
runtime/integration measurement only; the six labels are synthetic smoke labels
and do not support a quality, calibration, savings, promotion, or review
exclusion claim.

The checkpoint is pinned to revision
`5604afdc964f6c53782a6813140ade5216b99006`. The local artifact identities are:

- model weights: `6732b60037b086557e4c731c3748c99b4fec735d9ffe3c4c70061fd142e6b9d9`
- tokenizer (`vocab.json || merges.txt`):
  `888668b489c64b5627ebec9b788768c7e94eb8a93bfc6c5f4b84170981cfaa74`

The CPU run produced finite 768-dimensional embeddings, a deterministic head
artifact, six complete predictions, and `optimization_enabled: false`. The
recorded run took less than one second for head fitting and prediction after
checkpoint load. `runtime.json` preserves the Python, Torch, and Transformers
versions alongside manifest/artifact identities and raw scores; elapsed time is
informational and will vary by machine.

Replay from the provisioned cache, with no network access:

```sh
HF_HUB_OFFLINE=1 python3 experiments/local-ml/run_unixcoder_real_smoke.py \
  --snapshot /path/to/snapshots/5604afdc964f6c53782a6813140ade5216b99006 \
  --output /tmp/unixcoder-smoke.json
```

The path must name the pinned snapshot directory. The runner reads the model
and tokenizer bytes locally, records their digests, and refuses another
revision. Any future authorized evaluation must replace these synthetic labels
with an independently admitted, family-held-out dataset and add calibration,
drift, cost, and selective-risk measurements.
