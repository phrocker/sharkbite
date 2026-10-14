# Shoal Explorer on Azure — single shared instance

This directory holds the Azure deployment artifacts for a **single shared,
multi-user** Shoal Explorer workspace: one Linux container, one persistent
volume, no external datastore. It extends the platform-neutral packaging in
[`../README.md`](../README.md) and the full guide in
[`../../../docs/shoal-explore-web-deploy.md`](../../../docs/shoal-explore-web-deploy.md).

- `main.bicep` — the deployment (App Service plan, Linux container web app,
  Azure Files share for the state root, user-assigned managed identity, the
  Azure Files mount at `/var/lib/shoal`).
- `main.bicepparam` — example parameters, all obvious placeholders.

## How to read the claims in this document

Deployment docs are the easiest place in a repo to write something that has
never run. Every non-trivial claim below is tagged:

- **[Verified]** — I checked it against this repository (source, image, or an
  offline template build) on the machine that produced this PR.
- **[Docs]** — taken from Microsoft's documentation; a link is given.
- **[Inference]** — my reasoning from the above. Useful, but not tested end to
  end, because there are no Azure credentials in the environment that produced
  this PR. **No Azure resource was created or deployed.**

I could not deploy to Azure from here. Treat every **[Docs]** and **[Inference]**
claim as needing a first-run smoke test in your subscription.

## The deciding question: a persistent, single-writer volume

Shoal's Explorer is an embedded store on a local directory. Two facts drive the
whole hosting decision:

1. It must run as **exactly one writer.** Two processes writing the same corpus
   or policy store concurrently is data corruption, not a race you can retry.
2. PR #288's **split-brain startup guard** means durability is not optional: if
   the corpus holds documents but the policy catalog is empty (a lost or
   swapped volume), the server **refuses to start**. A platform that gives you
   ephemeral or silently re-created storage turns every restart into an outage.
   **[Verified]** — see `TestOpenServiceRefusesSplitBrainStateDirectory` in
   `cmd/shoal-explore-web/`.

So the question is not "can it run a container" — all three candidates can — but
"can it guarantee one writer against one durable volume, including across a
deploy."

### The three candidates, on that one question

| | Persistent volume | Storage sharing model | Deploy overlap (two writers?) |
| --- | --- | --- | --- |
| **App Service for Containers** | Bring-your-own Azure Files mount at a custom path | Azure Files (SMB/NFS), multi-mount | Warm-swap: old container serves until new is ready **[Docs]** — *avoidable* with a stop-first deploy |
| **Container Apps** | Azure Files (SMB/NFS) env storage | Multi-mount by design | Single-revision mode = zero-downtime = **overlap you cannot turn off** **[Docs]** |
| **AKS** | RWO Azure Disk PVC on a `replicas: 1` StatefulSet | Block volume, one node at a time | StatefulSet recreates (terminate-before-start); RWO disk blocks a second attach **[Inference]** |

Evidence for each row:

- **Container Apps overlaps on every deploy, and cannot be stopped from doing
  so.** "When you use single revision mode, Container Apps automatically
  switches between revisions to support **zero downtime deployment**." **[Docs]**
  ([application lifecycle](https://learn.microsoft.com/azure/container-apps/application-lifecycle-management)).
  Zero-downtime means the new revision is brought up healthy **before** the old
  one is torn down — the two overlap. Its only persistent storage is Azure
  Files, and "**Multiple containers can mount the same file share, including
  ones that are in another replica, revision, or container app**" **[Docs]**
  ([storage mounts](https://learn.microsoft.com/azure/container-apps/storage-mounts)).
  There is no ReadWriteOnce block option. So during any image or config change
  two replicas can, and by design do, write the same embedded store at once.
  **For a single-writer local store this is disqualifying** — you cannot deploy
  a new image without briefly running two writers. **[Inference, from two
  [Docs] facts]**

- **App Service also warm-swaps, but you can defeat it.** "While the new
  container is pulled and started, **App Service continues to serve requests
  from the old container.** App Service only sends requests to the new container
  after it starts and is ready" **[Docs]**
  ([custom container](https://learn.microsoft.com/azure/app-service/configure-custom-container#use-an-image-from-a-network-protected-registry)).
  So a naive image change overlaps too. The difference that decides this
  document: App Service has a first-class **stop / start** primitive, so an
  operator can force *old-fully-stopped-before-new-starts* — see
  [Single-writer deploys](#single-writer-deploys). Persistent storage is a
  bring-your-own Azure Files mount at a custom path (**"Mount path … Don't use
  `/` or `/home`"** — `/var/lib/shoal` is fine) **[Docs]**
  ([mount Azure Storage](https://learn.microsoft.com/azure/app-service/configure-connect-to-azure-storage?pivots=container-linux)).

- **AKS is the only one that enforces single-writer at the storage layer**, via
  a ReadWriteOnce Azure Disk that cannot attach to two nodes, plus StatefulSet
  terminate-before-recreate update semantics. **[Inference]** It is also, for
  one binary and one volume, **the wrong tool**: you would run and patch a
  Kubernetes cluster to host a single process. The operational overhead is not
  justified by this workload. If you ever need a storage-*enforced* guarantee or
  proper block-device POSIX semantics, this is the escalation path — not the
  starting point.

### Decision

**Azure App Service for Containers, one instance, deploy stop-first.** It is the
simplest managed option that can host a persistent volume **and** be pinned to a
verifiable single writer. Container Apps is rejected on the single-writer
question above; AKS is rejected as overkill for one binary. This matches the
prior guide's instinct (Container Apps "runs multiple replicas by default";
App Service "single-instance") but makes the *why* explicit and evidence-backed.

## Single-instance enforcement

`main.bicep` pins one writer three ways **[Verified — present in the template]**:

- App Service plan `sku.capacity: 1`.
- Site `siteConfig.numberOfWorkers: 1`.
- **No `Microsoft.Insights/autoscaleSettings` resource exists in the template.**
  Do not add one, and do not enable autoscale in the portal. Scaling this app
  out to two instances would point two writers at one Azure Files share.

**[Inference]** App Service will not run a second worker on its own with
autoscale absent and capacity 1; the residual overlap risk is deploy-time and
platform-maintenance events, addressed next.

## Single-writer deploys

A plain image swap warm-overlaps (above). To deploy a new image **without two
concurrent writers**, stop first:

```console
az webapp stop  --name <app> --resource-group <rg>
az webapp config container set --name <app> --resource-group <rg> \
    --docker-custom-image-name <registry>/shoal-explore-web:<new-pinned-tag>
az webapp start --name <app> --resource-group <rg>
```

This trades a short **downtime window** for a guaranteed single writer — which is
the correct trade for this store, and consistent with "zero-downtime deploy is
not supported" below. **[Docs]** for the warm-swap behavior; **[Inference]** that
`stop` fully releases the mount before `start` re-attaches it (App Service stops
the container on `stop`). Verify on first deploy by confirming the old container
logs stop before the new container logs begin.

- Do **not** use deployment slots with swap here: a slot runs a second container
  against the same data before the swap completes. Slots are a two-writer
  pattern for this workload. **[Inference]**
- Platform-initiated moves (plan scale, infra maintenance) can still briefly
  recycle the container. On Azure Files (RWX) nothing at the storage layer
  blocks a transient second writer. This residual risk is inherent to any
  RWX-backed PaaS and is the honest reason AKS + RWO disk exists as the
  escalation path. **[Inference]**

## Identity and secrets

- **No client secret anywhere.** The OIDC authenticator validates inbound
  bearer tokens and **"A client secret is never accepted."** **[Verified]** —
  `cmd/shoal-explore-web/main.go`. There is no token-issuing flow, so there is
  no application credential to store, rotate, or leak.
- **OIDC config travels as environment variables, not command-line args.** The
  template sets `SHOAL_OIDC_ISSUER`, `SHOAL_OIDC_AUDIENCE`,
  `SHOAL_OIDC_AUTHORIZATION_CLAIM`, `SHOAL_OIDC_READER_VALUES`, and
  `SHOAL_OIDC_CONTRIBUTOR_VALUES` as app settings; `main.go` reads each as a
  fallback for the matching `-oidc-*` flag. Optional discovery, JWKS, and
  browser-login settings use the same generic prefix. **[Verified]** — the app
  command line carries only `-state-dir` and `-listen`, so OIDC identifiers do
  not land in shell history.
- **Existing Entra-named deployments remain accepted during migration.** The
  deprecated `entraTenantId`, `entraClientId`, `entraReaderRoles`,
  `entraContributorRoles`, `entraIssuer`, `entraJwksUri`, and `entraScope`
  Bicep parameters still emit their matching `SHOAL_ENTRA_*` settings. The
  command translates those settings into the same OIDC validator, preserving
  the former issuer, role, identity, and browser defaults. Prefer the
  `oidc*` parameters for new deployments.
- **Managed identity for platform access.** A user-assigned managed identity is
  created and attached to the site; set `useAcrManagedIdentity=true` and grant
  it `AcrPull` on your registry so image pulls need no registry password:

  ```console
  az role assignment create \
    --assignee <identityClientId-output> \
    --role AcrPull \
    --scope <acr-resource-id>
  ```

  (Kept out of the template because the registry is usually in another resource
  group or subscription; cross-scope role assignment belongs in an explicit
  command.) **[Docs]**/**[Inference]**.
- **The one credential — the Azure Files key — is never committed.** The mount
  reads it with `listKeys()` at deploy time. If you prefer it in Key Vault,
  store the key as a secret and use a Key Vault reference app setting
  (`@Microsoft.KeyVault(SecretUri=…)`) with `keyVaultReferenceIdentity` set to
  the managed identity (already wired in the template). **[Docs]**
  ([Key Vault references](https://learn.microsoft.com/azure/app-service/app-service-key-vault-references)).

## The two Azure Files risks, sized honestly

Whether Azure Files is safe or corrupting for this store is *the* question, so it
gets a sized answer rather than a shrug. One risk is now largely retired by
evidence from this repository; the other is the genuine gating unknown.

### Risk 1 (the real gate): non-root user vs. the SMB mount

The image runs as uid/gid `65532` (non-root) **[Verified]** — `USER 65532:65532`
in `Dockerfile.shoal-explore-web` (at the repository root, not under `deploy/`).
App Service mounts Azure Files over SMB/CIFS with a fixed ownership; if the
mounted `/var/lib/shoal` is not writable by `65532`, the container cannot create
`corpus/` and `policy/` and will fail to start. **[Inference]** — I could not
test the mount's uid/gid from here, so treat this as the one unknown that gates
the first deploy.

**Settle it in under a minute after first start** with either check:

- **From the app's log stream** — a healthy first boot prints the durable-catalog
  line and the listening line; a mount-permission failure prints an
  open/create error instead and the container never reaches "listening":

  ```console
  az webapp log tail --name <app> --resource-group <rg>
  # expect, on success:
  #   Policy catalog is durable in /var/lib/shoal/policy: ... Persist both the
  #   corpus (/var/lib/shoal/corpus) and this policy directory ...
  #   Shoal Explorer listening at http://0.0.0.0:8098
  # must NOT appear (host-authority app setting missing — see that section):
  #   WARNING: bound 0.0.0.0:8098 but -allowed-host/SHOAL_ALLOWED_HOST is unset ...
  ```

  (Note: the distroless image has no shell, so `az webapp ssh` is not available —
  the log stream and the share listing below are the checks that work.)

- **From the storage side** — confirm the non-root process actually wrote to the
  share. Because the share is mounted at the state root, `corpus/` and `policy/`
  appear at the share root:

  ```console
  az storage file list --account-name <storageAccount> --share-name shoal-state \
      --path . -o table
  # expect two directories: corpus  policy
  ```

If neither directory appears, the mount is not writable by `65532`: this is a
mount-ownership problem, not an application bug. Escalate to AKS + Azure Disk
(where `securityContext.fsGroup` sets mount ownership) rather than changing app
code.

### Risk 2 (largely retired): SMB semantics under the store

The classic reasons an embedded store corrupts on SMB/CIFS are **memory-mapped
files** and **POSIX/BSD advisory locks** — the mechanisms SQLite, BoltDB and
LevelDB all cite when they document network shares as unsafe. **Neither is
present on this binary's path.** **[Verified]** — properties of this repository,
checkable offline:

- **No memory-mapped I/O anywhere in the tree.** A repo-wide search for
  `Mmap`/`syscall.Mmap`/`unix.Mmap` returns nothing; `internal/engine` in
  particular has none. The store's durability path is instead
  fsync-then-atomic-rename-over-temp (`tmp.Sync()` then `os.Rename(tmp, path)` at
  `internal/engine/table.go:385,392` and `internal/engine/rfile_sync.go:245`) —
  the single most network-filesystem-tolerant write pattern there is.
- **No advisory file locking on this binary's path.** The only `flock`/
  `LockFileEx` call sites in the entire repository are in
  `internal/promotion/intent_lock_unix.go` and `intent_lock_windows.go`, and
  `internal/promotion` **is not in this binary's dependency closure**.

Reproduce both offline:

```console
# 1) The lock code lives only in internal/promotion, and this binary never
#    imports it (empty output = not reachable), on either target OS:
go list -deps ./cmd/shoal-explore-web | Select-String 'internal/promotion|internal/tserver'
$env:GOOS='linux'; go list -deps ./cmd/shoal-explore-web | Select-String 'internal/promotion'; Remove-Item Env:GOOS

# 2) No memory-mapped I/O anywhere in the repo:
Get-ChildItem -Recurse -Filter *.go | Select-String 'syscall\.Mmap|unix\.Mmap|\bMmap\b'
```

One honesty note so a future reader is not misled: on the **Linux** deploy target
`golang.org/x/sys/unix` *is* in the closure (the Go runtime and other packages
pull it in), so "`x/sys/unix` is absent" is a Windows-only artifact and is **not**
the proof. The durable proof is that `internal/promotion` — the only package that
actually calls `flock` — is absent from the closure on both Windows and Linux, so
the `flock` code is unreachable regardless of whether `x/sys/unix` is linked.

**Net:** the two mechanisms that make embedded stores unsafe on SMB are not on
this path, so Azure Files SMB is a reasonable default for this binary. Keep
**Azure Files NFS** (Premium `FileStorage` + VNet integration; Key Vault mount
auth is not applicable with NFS) **[Docs]** and AKS + Azure Disk as escalation
paths **only if a specific problem is observed** — not as the expected remedy.

## Operational reality

### Backup and restore — this is the entire durability story

There is **no replication**. Durability is: the Azure Files share, and your
backups of it. **[Inference — architectural, follows from a single local store.]**

- **Backup:** snapshot the file share on a schedule.

  ```console
  az storage share snapshot --name <share> --account-name <storageAccount>
  ```

  Or enable Azure Backup for Azure Files for scheduled, retained snapshots.
  **[Docs]** ([Azure Files backup](https://learn.microsoft.com/azure/backup/azure-file-share-backup-overview)).
  A crash-consistent snapshot is fine for restart; for a clean backup, take the
  snapshot while the app is stopped so no write is mid-flight. **[Inference]**
- **Restore:** stop the app, restore the share (or repoint the mount to a
  restored share), start the app. Because the corpus and policy live under **one**
  share, they restore together and stay consistent — which is exactly what the
  split-brain guard checks for. **[Inference]**
- **Redundancy:** `Standard_LRS` is single-region, single-datacenter redundancy.
  For zone or region resilience choose `Standard_ZRS`/`Standard_GRS`, understanding
  that cross-region durability is still not application replication. **[Docs]**.

### What a restart looks like

The container restarts against the same share; the corpus and durable policy
catalog are already there, so the workspace serves the same data. **[Verified]**
that persistence works given one shared mount — the container/compose restart
evidence and `TestStateDirLayoutSharesOneMountPoint` in
`../../../docs/shoal-explore-web-deploy.md`. **[Inference]** that App Service
re-attaches the *same* Azure Files share on restart (it does, given the mount
config is unchanged).

### The split-brain refusal — what an operator sees, and recovery

If the policy directory is lost or a different share is mounted, startup fails
closed (production mode) with:

```text
refusing to serve a split-brain workspace: corpus <root>/corpus holds N
document(s) but the durable policy catalog <root>/policy holds no authorization
registrations. This is the signature of a lost or unmounted policy volume ...
Restore the policy directory from the same volume as the corpus (use -state-dir
so both persist under one mount) ...
```

**[Verified]** — captured from the real `openService` guard path; see the guide.

**Recover by fixing the mount, not the app:**

1. Confirm the site's Azure Storage mount still points at the correct share and
   mount path `/var/lib/shoal`. A changed/empty share is the usual cause.
2. If the share was lost, **restore it from a snapshot** (above) so `corpus/`
   and `policy/` return together, then `az webapp start`.
3. Only for a corpus first ingested **before** the policy catalog was durable
   is a re-registration needed — and that path is loopback + `-dev-auth` only,
   not something you run on the shared instance. **[Verified]** — guard message
   and `main.go`.

Never "fix" this by deleting the corpus or pointing at a fresh share to make
startup succeed: that discards data the guard is protecting.

### Host-authority: naming the external authority (required for a public bind)

A public bind of `-listen 0.0.0.0:<port>` means the socket's resolved authority
is `0.0.0.0:<port>`, which no real client `Host` header ever carries. **PR #295**
adds a central host-authority gate (`pkg/explorer/webapi`) enforced on **every
route before authentication**: it compares the request `Host`/`:authority`
against an exact-match allow-list, and answers **`421 Misdirected Request`** to
anything that does not match. Its default, when unconfigured, is the resolved
listen address — so on a public bind **every request is refused until you name
the external authority**. This is a deliberate, correct fail-closed default; it
is also why the naive template would 421 on every request if we did nothing.

The template configures it for you. **[Verified]** against merged `main`
(`origin/main` at `3670e00`, where PR #295 landed) —
`cmd/shoal-explore-web/main.go` and `pkg/explorer/webapi/hostauthority.go` — the
flag is:

- `-allowed-host` — comma-separated, exact-match allow-list of authorities
  (host or `host:port`). Host compares case-insensitively; **port compares
  exactly**, and an entry with no port matches a request whose `Host` has no
  port. No wildcard, no suffix, and `X-Forwarded-Host` is never trusted.
- Environment fallback **`SHOAL_ALLOWED_HOST`** — which is what the template
  sets, keeping the value off the command line for the same reason the OIDC
  identifiers are app settings.

**Why the entry is the bare hostname, not `hostname:8098`.** Reasoned through,
not guessed: a browser reaches App Service over HTTPS on port **443**, the
default for the scheme, so it sends `Host: myapp.azurewebsites.net` with **no
port**. `8098` is only the internal container port App Service forwards to; it
never appears in the public `Host` header. PR #295 normalizes a portless
authority to an empty port and matches empty-to-empty, so the correct allow-list
entry is the **bare host**. Putting `:8098` in the list would match nothing.
**[Verified]** — `normalizeAuthority`/`permits` in `hostauthority.go`.

**What the template does:**

- `allowedHosts` **empty (default)** → `SHOAL_ALLOWED_HOST` is set to the App
  Service **default hostname** (`site.properties.defaultHostName`, e.g.
  `myapp.azurewebsites.net`). The built-in `*.azurewebsites.net` endpoint works
  with no extra configuration.
- `allowedHosts` **set** → passed through verbatim, so a custom domain is named
  explicitly, comma-separated and bare:

  ```bicep
  param allowedHosts = 'explorer.example.test,myapp.azurewebsites.net'
  ```

  Include the `azurewebsites.net` name too if you still want the default
  endpoint to answer. The effective value is surfaced as the
  `effectiveAllowedHosts` output.

**Honest caveat — this is defence-in-depth, not a bug fix.** I confirmed against
merged `main` (PR #295) that **nothing in `pkg/explorer/webapi/` derives a URL,
redirect, or cookie domain from the request `Host` today** — the gate's own
documentation says it bounds these attacks "regardless of whether any individual
handler derives a URL from the request host today." So this hardens the edge
against DNS rebinding, cache poisoning, and virtual-host confusion; it does not
patch a live data-leak. Do not oversell it. **[Verified]** — `hostauthority.go`
doc comment and a search of the package.

**One residual [Inference] to smoke-test:** the gate matches whatever `Host`
App Service forwards to the container. App Service's front end preserves the
public `Host` header **[Inference — I could not test it from here]**; if a
platform quirk rewrote it, requests would 421 despite correct config. Verify on
first deploy: hit `https://<host>/` and a **200/401 means the authority matched;
a 421 means the forwarded `Host` is not in `SHOAL_ALLOWED_HOST`** — set it to the
name the client actually sends.

**A useful negative signal (converts part of the inference into something
observable).** PR #295 also emits a **one-time startup WARNING** — before any
request is served — when the bind is non-loopback **and** `-allowed-host` /
`SHOAL_ALLOWED_HOST` is unset. Its exact text (**[Verified]** — merged `main`,
`hostAuthorityStartupWarning` in `cmd/shoal-explore-web/main.go`) is:

```text
WARNING: bound 0.0.0.0:8098 but -allowed-host/SHOAL_ALLOWED_HOST is unset, so
the host-authority allow-list defaults to the bind address, which real client
Host headers do not carry; every request will be refused with 421 Misdirected
Request. Set -allowed-host to the external name(s) clients use to reach this
workspace.
```

Because this template **always** sets the `SHOAL_ALLOWED_HOST` app setting, that
warning should **never** appear. So it is a precise diagnostic: if you *do* see
it in `az webapp log tail`, the app setting did not reach the container (a
misapplied template, a config resource that failed, or a stale revision), and
you have found the cause of the 421s before debugging anything else.

> Sequencing note (historical): `-allowed-host` / `SHOAL_ALLOWED_HOST` shipped in
> **PR #295**, which merged into `main` at `3670e00`. This branch is rebased onto
> that commit, so the flag exists in the base source; every `[Verified]` tag in
> this section was re-checked against merged `main`, not the feature branch.

## Explicitly not supported

- **Zero-downtime deploy** — deploys stop the single writer first (above).
- **Horizontal scale-out** — one writer only; more instances corrupt the store.
- **Multi-region / active-active** — no replication; a single local store.
- **`-backend remote` fan-out** — refused in code: `auth.Decision` has no wire
  form. **[Verified]** — gap #2 in the guide.

## Growth path — what actually has to change to go past one instance

Do not imply this scales horizontally; with one local store it does not. To go
beyond a single instance you would have to change the **architecture**, not the
template:

1. **A wire-serializable auth decision** so a front tier can authenticate and
   call a backend with identity intact (blocks `-backend remote` today —
   gap #2). **[Verified]**
2. **A shared/coordinated storage tier** instead of one embedded local store —
   either an external datastore or Shoal's own distributed tablet path (the
   `deploy/k8s` + `deploy/helm` distributed manifests target that world; this
   single-binary path deliberately does not). Adding an external datastore is an
   **owner decision**, not something this deployment should introduce.
3. **Multi-host / multi-name serving.** The host-authority gate (PR #295) is
   already the seam: it takes a comma-separated allow-list, so serving several
   names is configuration, not code. Fronting several *instances* still needs
   items 1 and 2 first. **[Verified]** — the flag is comma-separated.

Until all three land, the honest ceiling is: **one shared instance, many users**,
backed up by share snapshots. That is what these artifacts deliver.
