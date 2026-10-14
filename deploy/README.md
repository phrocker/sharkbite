# Shoal platform deployment

This directory contains platform artifacts for running Shoal as four Kubernetes roles:

- **Write tier**: one `shoal-embed` StatefulSet per shard. Each pod has stable identity and a persistent local data directory for WAL/tablet state. The deployment manifests explicitly opt into the HTTP observability listener with `shoal-embed serve --data <dir> --address 0.0.0.0:<port> --metrics-address 0.0.0.0:<port> --quiesce-delay 10s`.
- **Read fleet**: stateless `shoal` pods behind a Service. They serve the Accumulo-compatible Thrift scan surface and open immutable RFiles through the configured storage backend.
- **Accumulo tablet server**: `shoal-tserver` StatefulSet members acquire
  Accumulo ServiceLocks, accept manager assignments, and serve scans and
  WAL-backed ingest only for tablets whose metadata ownership they claimed by
  conditional CAS. They advertise `TABLET_INGEST` only after all write
  authorities initialize and the release-gated `-enable-ingest` flag is set.
  Each ordinal has a persistent `/var/lib/shoal`
  volume for referenced WALs and minc checkpoints; an assignment that cannot
  access a referenced WAL fails closed rather than publishing incomplete data.
- **Accumulo compactor**: `shoal-compactor` StatefulSet members discover the
  coordinator through manager ServiceLock data, execute external compactions,
  persist ambiguous completion state on PVCs, and reconcile it after restart.

For local development these collapse into a single `shoal-embed` process. The intended platform shape is: writes land in a shard-local `shoal-embed`, flush/compaction emits RFiles to a shared object-store prefix, and read-fleet pods open those same RFiles for hedged scans.

Shoal's accepted
[coordination authority model](../docs/coordination-authority.md) deliberately
separates this Shoal-only pod topology from Accumulo-connected ownership:
Kubernetes Leases may coordinate a Shoal-only writer group, while
Accumulo-connected roles use ZooKeeper ServiceLocks and manager/coordinator
authority. The same logical table must never be writable under both domains.
The current manifests do not yet implement the Kubernetes Lease adapter; that
work is ordered under #128 rather than implied by the StatefulSet alone.

## Artifacts

- `../Dockerfile`: multi-stage image containing `/shoal-embed`, `/shoal`, and `/shoal-tserver`.
- `k8s/`: plain Kubernetes YAML for the write tier, read fleet, shared ConfigMap, and placeholder Secret.
- `helm/shoal/`: minimal Helm chart wrapping the same resources.
- `../Dockerfile.shoal-explore-web`: the authorized plane image (`shoal-explore-web`).
- `helm/validate-chart.sh`: chart checks — lint, render, schema, and every configuration guard.

## Build and push

```bash
docker build -t ghcr.io/YOUR_ORG/shoal:TAG .
docker push ghcr.io/YOUR_ORG/shoal:TAG
```

If you cannot run Docker locally, validate the same Go code path with:

```bash
go build ./...
```

## Deploy with plain manifests

Edit `deploy/k8s/configmap.yaml` and image names in `write-tier.yaml` / `read-fleet.yaml`, then apply:

```bash
kubectl apply -f deploy/k8s/configmap.yaml
kubectl apply -f deploy/k8s/secret.yaml
kubectl apply -f deploy/k8s/write-tier.yaml
kubectl apply -f deploy/k8s/read-fleet.yaml
kubectl apply -f deploy/k8s/tserver.yaml
kubectl apply -f deploy/k8s/compactor.yaml
```

The Secret is a template only. Replace `key.json` with a real GCS service-account key if not using Workload Identity / node Application Default Credentials, and replace `accumulo-password` with the metadata-walk password used by `cmd/shoal`, `shoal-tserver`, and `shoal-compactor`.
Both `shoal-tserver` and `shoal-compactor` require
`accumulo-instance-secret` and `accumulo-system-token-base64`; never store
production values in Git.

## Deploy with Helm

Choose one canonical deployment mode:

- `single`: one Shoal-only write tier.
- `distributed`: one Shoal-only write tier plus the read fleet.
- `accumulo`: Accumulo tservers, read fleet, and compactors; no Shoal-only
  write tier is rendered.

Each example values file is installable with one command:

```bash
helm upgrade --install shoal deploy/helm/shoal -f deploy/helm/shoal/values-single.yaml
helm upgrade --install shoal deploy/helm/shoal -f deploy/helm/shoal/values-distributed.yaml
helm upgrade --install shoal deploy/helm/shoal -f deploy/helm/shoal/values-accumulo.yaml
```

### The authorized plane

The three modes above describe a storage topology. The **explorer** —
`shoal-explore-web`, the process that holds a decision, enforces the disclosure
controls, and owns the Fleet registry and execution boundary — is orthogonal to
all three and composes with any of them, so it is not derived from `mode`. It is
off by default and enabled explicitly.

```bash
cp deploy/helm/shoal/values-explorer.yaml my-explorer-values.yaml
# fill in allowedHosts, issuer, audiences, authorizationClaim and at least one
# role mapping, then:
helm upgrade --install shoal deploy/helm/shoal -f my-explorer-values.yaml \
  --set explorer.image.repository=ghcr.io/YOUR_ORG/shoal-explore-web \
  --set explorer.image.tag=TAG
```

`values-explorer.yaml` is the authorized plane on its own, with no
Accumulo-replacement roles. **It does not install as shipped**: every required
value is left empty so the guards below fire, and `helm template` names the
first one missing. Fill them in, then install. To add the explorer to a storage
deployment instead, set `explorer.enabled=true` alongside any other profile.

The chart **refuses to render** a configuration that would start and then deny
or answer nothing. Authentication is a required decision with one valid value
(`explorer.auth.mode: oidc`); `-dev-auth` is not offered, because it is refused
on any non-loopback listener and a pod behind a Service must bind one.
`explorer.allowedHosts` is required, because an empty allow-list answers every
request with `421`. Each authority is trimmed; a blank-after-trim entry is
refused even beside valid hosts, and so is an entry holding a comma or interior
whitespace, since the gate matches each authority exactly. Each entry must be
an authority (`host`, `host:port`, `[ipv6]` or `[ipv6]:port`): a URL such as
`https://shoal.example.test` is refused, since it matches nothing, and so is an
unbracketed IPv6 address such as `::1`, which the workspace refuses at
startup. Write `[::1]` or `[::1]:8443` instead. Explorer chat and
network embedding URLs must be absolute HTTP(S) URLs with a host and no
userinfo, query, fragment or path beyond `/` (the provider appends its own API
path, so `https://api.example.test/v1` is refused at startup and here);
plaintext HTTP is allowed only for loopback hosts, not names such as
`localhost.example`. Voyage retains its built-in URL default when none is
supplied. Explorer, embed (`writeTier`), read fleet (`readFleet.listen` and
`readFleet.metricsPort`), tserver, compactor and effects gateway bind ports must
be at least 1024. Their containers run as uid 65532 with all capabilities
dropped, so they may be unable to bind on runtimes that restrict ports below
1024; some runtimes (containerd 2.x, Docker 20.10+) allow unprivileged low
ports, so the floor is the chart's policy rather than a hard limit. Use a port
of 1024 or above. This does not restrict a separate Service port.
`explorer.replicas` above one is refused: the corpus, workspace settings and durable policy catalog share one state root on a
ReadWriteOnce volume, with no coordination protocol between two processes over
it. A remote chat or embedding provider without a credential Secret is refused,
and so is an ask executor reference the allow-list does not name.

Probes address a **separate health port** (`-health-address`), never the
workspace port. The workspace refuses any request whose `Host` is not an exactly
configured authority, and a kubelet addresses the pod by its runtime-assigned
IP, which no static allow-list can name — a probe there answers `421` and the
pod never becomes ready. See the health-surface section of
[`docs/shoal-explore-web-deploy.md`](../docs/shoal-explore-web-deploy.md).

### The enforcement plane

The **LLM gateway** — `shoal-llm-gateway`, an OpenAI-compatible endpoint a caller
points at instead of the real provider — asks the explorer for admission before
each call and reports the outcome after it. It is a separate process on purpose:
it handles untrusted prompt content from arbitrary callers and speaks to
third-party endpoints, and co-locating that with the policy store would put
prompt injection in the decision plane's address space. Like the explorer it is
off by default and not derived from `mode`.

```bash
cp deploy/helm/shoal/values-llm-gateway.yaml my-llm-gateway-values.yaml
# fill in allowedHosts, tls.secretName (a kubernetes.io/tls Secret; the
# listener serves TLS by default), admission.url and credential,
# upstream.baseURL and credential, and the six identity fields, then:
helm upgrade --install shoal deploy/helm/shoal -f my-llm-gateway-values.yaml \
  --set llmGateway.image.repository=ghcr.io/YOUR_ORG/shoal-llm-gateway \
  --set llmGateway.image.tag=TAG
```

`values-llm-gateway.yaml` **does not install as shipped**, on the same principle.
`llmGateway.admission.url` may name an explorer installed separately — the gateway
needs one to ask, not one in the same release — and both planes compose from one
values file.

It is a **Deployment**, not a StatefulSet, and that single difference is the
whole difference between the two planes. The gateway holds no state root, so
replicas are independent askers of one decision plane: more than one is correct,
the default is two, and `llmGateway.podDisruptionBudget.maxUnavailable: 0` is
refused — the posture the explorer requires because it is a singleton would only
wedge drains here.

The refusals, the probe surface and the flag contract are in
[`docs/llm-gateway-deploy.md`](../docs/llm-gateway-deploy.md). The short version:
nearly every misconfiguration of this component produces a pod that passes every
probe and denies every call, because it is required to fail closed — which from
outside is a total outage of whatever is configured to go through it. That is
what the render-time guards move to `helm template`.

Run the chart checks before changing any of this:

```bash
deploy/helm/validate-chart.sh
```

It renders every profile, schema-checks the output, asserts that each guard
still refuses and each valid configuration still renders, and asserts that each
plane's two listeners are wired end to end. A guard that silently stops firing
is the failure this exists to catch, so the assertions themselves are
mutation-tested: each guard is removed in turn and the case that must then fail
is confirmed to fail.

Replace the example image and environment-specific storage, ZooKeeper,
credentials, and HDFS values before deployment. Role-level `enabled` values
remain optional compatibility overrides; when omitted, `mode` selects the safe
resource set above. Helm fails immediately if both Shoal-only `writeTier` and
Accumulo `tserver` write authority are enabled.

In `accumulo` mode, the Secret named by `tserver.credentialsSecretName` and
`compactor.credentialsSecretName` must provide `accumulo-password`,
`accumulo-instance-secret`, and `accumulo-system-token-base64`.

**Not production-ready:** distributed Shoal-only write coordination remains
limited to one writable replica. Multiple writable replicas must not be used
until the Kubernetes Lease adapter exists; the chart rejects that
configuration.

## Wired binary flags and environment

Confirmed from source:

- `cmd/shoal-embed/main.go`: `serve` supports `--data`, `--address` (gRPC bind address, e.g. `0.0.0.0:9876`; defaults to `127.0.0.1:<port>` when only the legacy `--port` is set), `--metrics-port` / `--metrics-address` (HTTP health/readiness/metrics bind; **opt-in** — the HTTP surface only starts if one of these is explicitly passed, so a bare `shoal-embed serve --port N` keeps behaving as a single-port process; when enabled, defaults to `127.0.0.1:9877`), `--quiesce-delay` (default `5s`, but only applied when the HTTP readiness surface is enabled; the manifests override it to `10s`), `--drain-timeout` (default `30s`, bounds graceful shutdown; see below), and `--tls-cert` / `--tls-key` / `--tls-client-ca` (optional TLS material for both listeners together, each with a `SHOAL_EMBED_TLS_*` environment fallback; see the TLS section below).
- `cmd/shoal/main.go`: read fleet additionally supports `-metrics-address`, `-readiness-interval`, `-quiesce-delay`, `-drain-timeout`, and shared-listener `-tls-cert` / `-tls-key` / `-tls-client-ca` settings (`SHOAL_TLS_*` fallbacks).
- `cmd/shoal-tserver/main.go`: requires `-advertise`, an authenticated
  ZooKeeper instance secret, metadata/configuration credentials, and the
  manager system token. `/readyz` becomes healthy only while a ServiceLock is
  held, manager authority is current, scan admission is open, and every
  advertised write authority is initialized; `/metrics` exports tablet,
  ingest, WAL, minc, fencing, and backpressure counters.
- `cmd/shoal-compactor/main.go`: discovers the coordinator, persists completion
  reconciliation, exposes semantic dependency readiness and job/progress/retry
  metrics, and bounds shutdown with `-shutdown-timeout`.
- `internal/storage/gcs/gcs.go`: the GCS backend uses Application Default Credentials and accepts paths like `gs://bucket/object` or `bucket/object`.

The manifests pass only supported process flags to the containers. `GOOGLE_APPLICATION_CREDENTIALS` is set for the GCS client when a key Secret is mounted.

## Health, metrics, and graceful drain

`shoal-embed serve` can expose an HTTP surface alongside the gRPC write port, but only when asked: it is off by default and only starts if `--metrics-port` or `--metrics-address` is explicitly passed — a bare `shoal-embed serve --data ... --port 9876` (the top-level README's quick-start) stays a single-port process rather than silently gaining a second listening port. These manifests explicitly opt in via `--metrics-address 0.0.0.0:<port>` (default `9877`), so this section describes what's actually running in the write tier:

- `/healthz`: unconditional liveness — 200 once the process has started serving. Used for `livenessProbe` in both manifests.
- `/readyz`: readiness — 200 once startup finishes, 503 during startup and during shutdown drain. Used for `readinessProbe`, so a draining pod can be removed from Service endpoints before its gRPC port stops accepting new work.
- `/metrics`: Prometheus text-format metrics (via `internal/obs`). Not yet wired to a `ServiceMonitor`/scrape annotation in these manifests; add one for your Prometheus setup if needed.

`cmd/shoal` exposes a separate read-role operations listener when
`-metrics-address` is set. `/healthz` is process liveness. `/readyz` is
semantic and returns 200 only when the Thrift listener is serving, new scan
sessions are accepted, the most recent ZooKeeper root-tablet lookup succeeded,
the metadata walk succeeded, and the configured storage backend could open and
read a discovered RFile (an empty instance has no object to probe). During
drain, session admission flips false immediately and readiness returns 503
before the Thrift listener closes.

The read role exports these stable Prometheus names:

- `shoal_read_accepting_sessions`
- `shoal_scan_sessions_active{kind}`
- `shoal_scan_sessions_expired_total{kind}`
- `shoal_scan_sessions_canceled_total{kind}`
- `shoal_scan_continuations_total{kind}`
- `shoal_scan_failures_total{operation,kind}`
- `shoal_scan_rpc_latency_seconds_count{operation}`
- `shoal_scan_rpc_latency_seconds_sum{operation}`
- `shoal_scan_backpressure_total{reason}`

The manifests add Prometheus scrape annotations and
`deploy/monitoring/read-fleet-alerts.yaml` provides starter rules for lost
admission, backpressure, failures, and expiry.

On SIGTERM the read role rejects new `StartScan`/`StartMultiScan` calls with
`ScanServerBusyException`, waits `-quiesce-delay`, then allows retained sessions
to continue for at most `-drain-timeout`. A deadline force-cancels and releases
the remaining bounded session state before the Thrift listener stops.
`terminationGracePeriodSeconds: 45` covers the default 5s quiesce + 30s drain
plus shutdown margin.

The write-tier headless Service no longer sets `publishNotReadyAddresses: true`, so readiness now gates which pod IPs are published to clients.

On `SIGINT`/`SIGTERM`, `cmdServe` (via `serveHandle.RunUntilSignal`) flips `/readyz` to not-ready immediately. If the HTTP readiness surface is enabled, it then waits `--quiesce-delay` so a readiness-polling consumer has a chance to react before anything stops accepting work; if the HTTP surface is disabled, legacy single-port `serve` skips that quiesce and begins shutdown immediately. The manifests set `--quiesce-delay=10s`, `readinessProbe.periodSeconds=5`, and `readinessProbe.failureThreshold=1`, so a draining pod gets a real ready-to-not-ready withdrawal window before gRPC shutdown starts. After quiesce, HTTP and gRPC shut down concurrently under the same `--drain-timeout` budget (default `30s`), so a slow `/stats`/`/metrics` request can't eat into the budget meant for draining writes.

If that deadline passes, the server force-stops transport (`grpc.Server.Stop()`). Transport-cancellable RPCs (for example a streaming `Scan`) unwind quickly after that, so shutdown can still close the engine and exit cleanly. A unary RPC already inside a synchronous engine call (`Write`/`Flush`/`Compact`/`CreateTable`) is different: it ignores request cancellation today, so force-stopping transport cannot interrupt it safely. In that case shutdown returns an explicit timeout/in-flight error, skips engine close rather than racing a close against still-running engine work, and exits without claiming a completed close. Making those engine calls themselves cancellation-aware would close this gap fully; that's a larger engine-layer change and remains open (see gaps below).

The write-tier `terminationGracePeriodSeconds` (`50s`) covers the manifest's `--quiesce-delay=10s` + `--drain-timeout=30s` plus a `10s` margin for engine close and process exit before Kubernetes sends `SIGKILL`; if you raise either flag, raise `terminationGracePeriodSeconds` to match.

## TLS for the write-tier listeners

`shoal-embed serve` can terminate TLS on both its listeners — the gRPC write port and the HTTP `/healthz`/`/readyz`/`/stats`/`/metrics` surface — but it is **off by default**: existing plaintext deployments keep working unchanged.

Enable it by supplying a PEM certificate and private key, via flag or environment variable. A flag always wins over its same-purpose environment variable — including an explicitly-set empty flag (e.g. `--tls-cert=""`), which clears the setting rather than falling back to the environment variable (see `flagOrEnv`/`flagWasSet` in `cmd/shoal-embed/main.go`). There is no other precedence and no implicit fallback (for example, TLS is never auto-enabled, and a partially-specified configuration is a startup error, not a silent switch to plaintext):

| Setting | Flag | Environment variable |
| --- | --- | --- |
| Server certificate | `--tls-cert` | `SHOAL_EMBED_TLS_CERT` |
| Server private key | `--tls-key` | `SHOAL_EMBED_TLS_KEY` |
| Client CA bundle (enables mutual TLS) | `--tls-client-ca` | `SHOAL_EMBED_TLS_CLIENT_CA` |

`--tls-cert` and `--tls-key` (or their environment equivalents) must be set together — a cert without a key, or vice versa, fails startup with a descriptive error rather than falling back to plaintext. Adding `--tls-client-ca` turns on mutual TLS: both listeners then require and verify a client certificate against that CA bundle before completing the handshake. One certificate/key pair covers both listeners; they cannot be given independent TLS material. The resulting `tls.Config` pins `MinVersion` to TLS 1.2.

To enable TLS in the plain write-tier manifest: mount a Secret containing `tls.crt` / `tls.key` (and, for mutual TLS, `ca.crt`) at `/var/run/secrets/shoal/tls`, then uncomment the matching `--tls-*` args, volumeMount, and volume already spelled out as comments in `deploy/k8s/write-tier.yaml`. With Helm, set `writeTier.tls.enabled=true` and `writeTier.tls.secretName=<your secret>` (add `writeTier.tls.requireClientCert=true` for mutual TLS); the chart wires the args/volume for you and fails the render with a clear error if `secretName` is left unset while `enabled=true`.

With Helm TLS enabled, kubelet `httpGet` probes use `scheme: HTTPS` for `/readyz` and `/healthz`. When Helm mutual TLS is enabled, the chart switches those probes to `tcpSocket` checks against the metrics port instead, because kubelet HTTP probes cannot present a client certificate. That keeps mTLS pods schedulable/ready from Kubernetes' perspective, but the probe then proves only listener reachability rather than `/readyz`'s startup/shutdown-drain state; use a custom probe path if you need both kubelet-visible readiness semantics and client-certificate enforcement on the HTTP listener.

The plain write-tier manifest has no templating, so that probe switch isn't automatic there: if you uncomment the TLS example in `deploy/k8s/write-tier.yaml`, also add `scheme: HTTPS` to its `readinessProbe`/`livenessProbe` `httpGet` blocks, or replace them with a `tcpSocket: {port: metrics}` check if you additionally uncomment `--tls-client-ca` (mutual TLS). The manifest carries an inline comment with these exact steps next to the probes.

Certificate/key rotation is out of scope for this slice: `shoal-embed` loads the key pair (and CA bundle) once at startup, so rotating any of them requires a pod restart today — see the gaps list below.

The read role uses the same TLS policy and shared implementation: one
certificate/key pair covers both Thrift and the HTTP operations listener,
TLS 1.2 is the minimum, and a client CA enables mutual TLS on both. Helm values
are `readFleet.tls.enabled`, `secretName`, and `requireClientCert`. HTTPS probes
are used for server-only TLS; mutual TLS falls back to TCP probes because
kubelet cannot present a client certificate. The plain manifest keeps TLS
flags commented out so plaintext remains the default until an operator mounts
the corresponding Secret.

The tserver and compactor use that same `internal/tlsserver` policy for both
their Thrift and operations listeners. Their Helm sections are `tserver.tls`
and `compactor.tls`; mTLS automatically changes kubelet probes to TCP because
kubelet cannot supply a client certificate. Operational upgrade and rollback
steps are documented in
[`docs/write-tier-operations.md`](../docs/write-tier-operations.md).

## Rolling upgrades, rollback, and voluntary disruption

Both tiers now carry an explicit rollout contract and a `PodDisruptionBudget`, instead of relying on whatever the `apps/v1`/`policy/v1` defaults happen to be:

- **Read fleet** (`shoal-read` Deployment): `strategy.rollingUpdate` sets `maxUnavailable: 0`, `maxSurge: 1` — a rollout keeps all 3 replicas available throughout: the controller cannot remove an old pod until the one extra surge pod has passed readiness, since it isn't allowed any unavailable pods in between. Its separate `PodDisruptionBudget` allows `maxUnavailable: 1` for voluntary disruptions (node drains, cluster-autoscaler consolidation) outside of a rollout — a different, more permissive budget than the rollout strategy above, since those disruptions aren't gated on a replacement pod being ready first.
- **Write tier** (`shoal-embed` StatefulSet, one replica per shard today): `updateStrategy` is pinned to the StatefulSet default (`RollingUpdate`, `partition: 0`) explicitly rather than implicitly, leaving `partition: N` available as a lever for a staged/canary rollout later. Its `PodDisruptionBudget` sets `maxUnavailable: 0` — a **deliberate, restrictive** policy: it blocks the Kubernetes Eviction API (`kubectl drain`, cluster-autoscaler consolidation, and similar voluntary-disruption paths) from evicting the sole write-tier replica for a shard, because there is no live tablet hand-off/migration path yet and evicting it today would simply be an unplanned outage for that shard with no failover. This PDB does **not** block operator-initiated rolling updates: `kubectl rollout restart` / `helm upgrade` act through the StatefulSet controller directly, which replaces pods without going through the Eviction API, so PDBs never apply to them.

Recommended operator commands:

```bash
# Roll a new image/config to the read fleet (respects the strategy above)
kubectl set image deployment/shoal-read shoal=ghcr.io/YOUR_ORG/shoal:NEW_TAG
kubectl rollout status deployment/shoal-read
kubectl rollout undo deployment/shoal-read   # rollback to the previous revision

# Roll a new image/config to a write-tier shard (StatefulSet; bypasses the PDB, as above)
kubectl set image statefulset/shoal-embed shoal-embed=ghcr.io/YOUR_ORG/shoal:NEW_TAG
kubectl rollout status statefulset/shoal-embed
kubectl rollout undo statefulset/shoal-embed

# Same via Helm, plus its own rollback path
helm upgrade shoal deploy/helm/shoal --reuse-values --set image.tag=NEW_TAG
helm rollback shoal   # back to the previous release revision
```

A StatefulSet rolling update replaces the container but reuses the same PVC, so on-disk data for that shard is preserved across the restart; there is still no live migration of in-flight writes to another shard during the restart window itself. `terminationGracePeriodSeconds` (`50s`) and the drain sequence described above apply the same way during a rollout as during any other pod termination.

## Current platform gaps / TODOs

These are deliberately documented, not papered over with invented flags:

1. `shoal-embed serve` has no CLI/env for `engine.Options.Backend`, storage backend, bucket, or prefix. The engine/tablet layers support a backend, but the CLI does not wire it yet; therefore the write-tier manifest keeps WAL/RFiles on the PVC today.
2. `cmd/shoal` reads RFile paths from Accumulo/Shoal metadata and has `-storage=gs|local`, but no bucket/prefix override flag. The shared bucket/prefix ConfigMap values are operator intent/future wiring.
3. S3 is mentioned in high-level docs, but this repository currently exposes local, memory, and GCS storage packages; no S3 backend package or binary flag is available.
4. Starter read-fleet alerts are provided, but no complete dashboard is included. Tablet migration and data-loss-safe write coordination remain the Accumulo manager/coordinator's responsibility.
5. `Write`/`Flush`/`Compact`/`CreateTable` discard the request context in `internal/embedstore`, so an in-flight call cannot be cancelled once dispatched. Shutdown now stays bounded by force-stopping transport at `--drain-timeout`, but if one of those unary engine calls is still running at that point the process returns a timeout/in-flight error and intentionally skips engine close rather than closing the engine unsafely underneath it. Only the streaming `Scan` RPC observes transport cancellation today.
6. TLS material has no rotation story: `shoal-embed serve` loads the certificate/key/client-CA once at startup, so rotating any of them requires a pod restart (a rolling one, using the mechanics above) rather than a live reload/SIGHUP-style refresh.
7. The write-tier `PodDisruptionBudget` (`maxUnavailable: 0`) is a deliberate policy, not an oversight — see "Rolling upgrades, rollback, and voluntary disruption" above. It blocks voluntary eviction of the sole write-tier replica for a shard because there is no live tablet hand-off path yet; it does not block operator-driven rollouts.
8. The read fleet uses `readOnlyRootFilesystem: true`; the write tier still needs writable engine state and remains backed by its PVC.
