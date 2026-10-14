# Write-tier operations

This runbook covers the Accumulo-connected `shoal-tserver` and
`shoal-compactor` roles. Live mixed-fleet rollout evidence remains gated by
[#74](https://github.com/phrocker/shoal-oss/issues/74).

## Readiness and startup

Both roles expose `/startupz`, `/readyz`, `/healthz`, and `/metrics` on their
operations listener. `/readyz` is JSON and reports every dependency by name.
The tserver is ready only while its ServiceLock and manager authority are
current, tablet transitions are settled, scan admission is open, and all
advertised ingest/WAL/minor-compaction authorities exist. Lock loss, manager
loss, drain, or an unavailable write authority returns 503.

The compactor requires coordinator discovery/session health, validated storage,
a writable completion journal, its CompactorService listener, and open job
admission. Historical failures remain counters; current dependency state gates
readiness.

Stable metrics include tserver tablet/ingest/backpressure/WAL/minc counters and
compactor active/executed/completed/failed/cancelled jobs, progress entries,
ambiguous completion, and retries. Starter Prometheus rules are in
`deploy/monitoring/write-tier-alerts.yaml`.

## TLS and configuration

`-tls-cert` and `-tls-key` enable TLS 1.2+ on both the role's Thrift listener
and operations listener. `-tls-client-ca` additionally requires and verifies
client certificates on both. The three flags use the shared
`internal/tlsserver` policy. Partial TLS configuration fails startup.

Secrets are read from `SHOAL_PASSWORD`, `ACCUMULO_INSTANCE_SECRET`, and
`SHOAL_SYSTEM_TOKEN_BASE64`; an explicit non-empty CLI value takes precedence.
Kubernetes mounts secrets rather than placing credentials in ConfigMaps.
Certificate and secret rotation is restart-based: update the Secret, then
perform the bounded rolling restart below.

## Drain, restart, upgrade, and rollback

Tserver shutdown closes ingest and scan admission before withdrawing the
ServiceLock. Retained scans drain until `-drain-timeout`; the Thrift and
operations accept loops are then stopped and joined under bounded deadlines.
WAL and minc state live on the StatefulSet PVC and are reconciled on restart.

Compactor cancellation closes job admission and bounds coordinator hand-back
with `-release-timeout`; transport RPCs remain bounded by `-rpc-timeout`.
After job cancellation/hand-back, listener shutdown is bounded by
`-shutdown-timeout`. The 55-second manifest grace period covers the default
15-second release plus 30-second listener budgets with margin. Its completion journal is on a StatefulSet PVC, allowing
an ambiguous `compactionCompleted` call to reconcile after container or pod
restart without publishing twice.

Before an upgrade:

1. Verify all `shoal_dependency_ready` series are 1 and no completion/WAL/minc
   alert is firing.
2. Update one image/config revision. StatefulSet `partition` can canary the
   highest ordinal.
3. Wait for `/startupz`, then semantic `/readyz`, before lowering the partition.
4. Confirm WAL recovery/minc resume or completion-reconciliation counters and
   logs contain no unresolved operation.
5. Roll back with `kubectl rollout undo statefulset/shoal-tserver` or
   `kubectl rollout undo statefulset/shoal-compactor`; PVC state is reused.

PDBs permit one voluntary disruption at a time. They do not replace manager
placement or coordinator dead-job recovery and must not be relaxed during an
active dependency or completion alert.
