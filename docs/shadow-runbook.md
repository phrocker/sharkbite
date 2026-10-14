<!--

    Licensed to the Apache Software Foundation (ASF) under one
    or more contributor license agreements.  See the NOTICE file
    distributed with this work for additional information
    regarding copyright ownership.  The ASF licenses this file
    to you under the Apache License, Version 2.0 (the
    "License"); you may not use this file except in compliance
    with the License.  You may obtain a copy of the License at

      https://www.apache.org/licenses/LICENSE-2.0

    Unless required by applicable law or agreed to in writing,
    software distributed under the License is distributed on an
    "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
    KIND, either express or implied.  See the License for the
    specific language governing permissions and limitations
    under the License.

-->
# shoal-compactor-shadow — operator runbook

The shadow service runs a passive correctness oracle for Accumulo's
compactor: every time Java compacts a tablet on a watched table, the
shadow re-runs the same compaction in shoal and diffs the outputs at
four tiers (T1 Java cross-read, T2 cell sequence, T3 point lookup, T4
per-file summary).

A 24-hour run with divergence rate < 0.1% and zero T1 failures is the
acceptance gate for the C3.b production cutover (shoal compactors
replacing Java compactors).

## Enable per cluster

```bash
helm upgrade --install <release> charts/accumulo \
  --set shoalShadow.enabled=true \
  --set shoalShadow.tables=graph_vidx,graph \
  --set shoalShadow.reportPrefix=gs://example-bucket/shadow-reports/<cluster>
```

That's the minimum surface. Per-flag defaults live in
`charts/accumulo/values.yaml` under `shoalShadow`. Key tunables:

| Value | Default | When to change |
|---|---|---|
| `tables` | `""` | Comma-separated table NAMES to watch. Empty = pod refuses to start. |
| `pollInterval` | `5s` | Lower for faster detection vs. metadata load; below 1s is rejected. |
| `oracleConcurrency` | `4` | Raise on >8-core nodes if many compactions land at once. |
| `itercfgTTL` | `5m` | Lower to react to table.iterator.* changes faster. |
| `reportPrefix` | `""` | gs:// or local prefix for JSON reports. Empty = log-only. |
| `javaValidator` | `""` | Shell template invoking `accumulo file rfile-info $RFILE`; enables T1. |

## Where reports land

When `reportPrefix` is set, each observed compaction emits a JSON
report at:

```
<reportPrefix>/<table-id>/<unix-nanos>.json
```

Path components:
- `<table-id>` is the Accumulo internal table id (e.g. `2k`), not the
  human-readable name. Look up the mapping via the resolver's first-
  startup log line (`watch table name=graph_vidx id=2k`).
- `<unix-nanos>` makes the report list chronologically sortable.

Inspect the latest 10 reports:

```bash
gsutil ls -l gs://example-bucket/shadow-reports/<cluster>/2k/ \
  | sort -k2 -r | head -10
```

Each report contains the full `shadow.Report` struct: cell counts,
T1/T2/T3 outcomes, first-divergence keys, and shoal/Java per-file
summaries (cells-by-CF).

## Interpreting a divergence

The pod logs INFO for every observation and ERROR for any T1/T2
failure. The single-line log shape is:

```
shadow report table_id=2k table=graph_vidx kind=compaction
  inputs=4 output=gs://.../A001.rf
  shoal_cells=169530 java_cells=169530
  t1_pass=true t2_match=true t3_match=10000/10000
  dur_ms=2270
```

Triage by tier:

| Tier failure | Likely cause | What to look at |
|---|---|---|
| **T1 fail** (`t1_pass=false`) | shoal produced bytes the Java reader can't parse — wire-format regression in shoal's RFile writer or compression codec | `report.T1.Error` (validator stderr), `internal/rfile/parity_*_test.go` parity harness |
| **T2 fail** (`t2_match=false`) | iterator semantic divergence: shoal's port of an iterator emits cells in a different order, drops different cells, or computes different values | `report.T2.CellSeqDiff` gives the first-divergence rendering — search by row/cf to identify which iterator produced it |
| **T3 fail** (`t3_match=N/10000` low) | block-index or bloom-filter divergence: shoal's index points lookups at different blocks than Java's | `report.T3.Divergences` (first 32). If `T1` and `T2` are green but `T3` is red, suspect the relative-key compression or index sampling rate. |

To drill into a specific divergent compaction:

```bash
# Pull the report
gsutil cat gs://example-bucket/shadow-reports/<table-id>/<unix-nanos>.json \
  | jq '.report.T2.CellSeqDiff, .inputs, .java_output'

# Re-run the oracle locally on the same inputs/output for live debugging
# (single-shot Phase 1 mode):
shoal-compactor-shadow \
  --inputs "$(jq -r '.inputs | join(",")' < report.json)" \
  --java-output "$(jq -r '.java_output' < report.json)" \
  --iterators 'latentEdgeDiscovery:similarityThreshold=0.85;versioning:maxVersions=10' \
  --scope majc --report-out report-local.json
```

## Metrics + alerting

The pod exposes Prometheus exposition at `:9810/metrics`. Key series:

- `shadow_compactions_observed_total{table=...}` — should be non-zero
  within `pollInterval` after enabling.
- `shadow_t1_failures_total{table=...}` — any non-zero value is an
  immediate page (wire-format regression).
- `shadow_t2_mismatches_total{table=...}` — divergence rate. Alert if
  `rate(t2_mismatches[10m]) / rate(compactions_observed[10m]) > 0.001`
  for 10 minutes (the 0.1% gate from the Phase 3 acceptance criteria).
- `shadow_inputs_missing_total{table=...}` — high counts mean the
  Accumulo GC window is shorter than oracle latency. Lower
  `pollInterval` or raise the GC hold-time.
- `shadow_oracle_errors_total` — non-zero is the pod's own bug
  signal; investigate via logs.

For clusters without Prometheus, the same data lives at
`:9810/metrics-json` as a single JSON snapshot — wire it to GCP
Cloud Logging via a sidecar curl on a cron, or rely on the ERROR
log lines for `severity=ERROR` log-based alerts.

## Common operational scenarios

### "shoal-shadow pod won't start"

1. `kubectl logs deploy/<release>-accumulo-shoal-shadow` — usually
   `--tables` is empty or names a table that doesn't exist.
2. The first thing logged after `zk connected` is `watch table
   name=<n> id=<id>`. If you don't see those lines, table resolution
   failed.

### "no compactions observed, but Java is compacting"

1. Confirm with `kubectl exec` + accumulo shell:
   `compact -t graph_vidx -w` then watch the pod logs for the
   `compaction observed` line within `pollInterval + 5s`.
2. If the line appears but no `shadow report` follows, check
   `itercfg: class not in shoal allowlist` warnings — the table has
   iterators whose Java class isn't yet ported. The compaction was
   observed, but the oracle declined to run on partial coverage.

### "T2 mismatch on every compaction"

A pattern, not a bug — look at `report.T2.CellSeqDiff`:
- If divergences are all CF=`link` and timestamps differ, that's
  `LatentEdgeDiscoveryIterator`'s wall-clock timestamps; pre-existing
  link cells are still expected to match. Look at non-link CF first.
- If divergences are spread across CFs with timestamp differences,
  iterator semantic divergence — escalate.

### "lots of `inputs missing` events"

Accumulo's GC nuked the input files before the oracle fetched them.
Two knobs:
- Lower `shoalShadow.pollInterval` (default 5s → 2s).
- Raise the cluster's GC hold-time (`gc.cycle.delay`).

## The path to C3.b (production cutover)

The shadow service is the gating signal for replacing Java compactors
with shoal compactors via the new `CompactionCommit` manager RPC
(design decision #1). Cutover prerequisites:

1. Shadow runs green for **24h** on `graph_vidx` and `graph` with
   `t2_mismatches_total == 0` and `t1_failures_total == 0`.
2. No `class not in shoal allowlist` warnings — full iterator
   coverage for the target tables.
3. `inputs_missing_total` rate is steady (no growth) — the GC race
   is not silently masking divergence.

After all three, cut over per the Phase C3.b plan (shoal compactor
replacement via the `CompactionCommit` manager RPC).
