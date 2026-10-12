# Effects gateway: deployment

> **Deploying it:** `Dockerfile.shoal-gateway` builds the image, and the
> chart's `effectsGateways:` list deploys it, one entry per surface. See
> [In the chart](#in-the-chart). [Running the gateway](#running-the-gateway)
> is what the chart renders, for a deployment by hand.

This is the HTTP Path A worker from #391: it pulls an action off the fleet
dispatch queue, claims it under a fence, performs one HTTP request against a
configured operational surface, classifies what happened, and completes the
action. The reference design is `docs/gateway-proxy-design.md`; the slice-1
design this implements is the last comment on #391. `docs/gateways.md` places
it among the three gateways.

## Status

**Delivered (#391).** The worker loop, the `shoal-gateway` binary, its image
and its chart entry are built, and the binary is tested end to end (below,
*What exists*). The blockers this design was written against are all closed:

| | what it was | how it was resolved |
|---|---|---|
| **#480** | the lifecycle auditor, action recorder and reconciler refused `OperationExecute`, so the grant could not be turned on | fixed; the execute grant comes only from the executor mint, bound to one executor ref ([Issuing executor credentials](#issuing-executor-credentials)) |
| **#430** | claim renewal (`POST actions/{id}/extend`) | landed; `-renew` turns it on |
| **#484** | the lost-fence ambiguity route (`POST actions/{id}/ambiguity`) | landed; a worker whose claim lapsed mid-effect records what it attempted |
| **#486** | a heartbeat moved the descriptor generation, so `/complete` and `/ambiguity` answered 404 after one heartbeat | fixed; only `Register` moves the generation |

The dispatch client speaks every route it needs (`Extend`, `ReportAmbiguity`).
It has no heartbeat method, and will not get one: a worker cannot truthfully
assert a descriptor's liveness, so the gateway never heartbeats and carries
no registrar credential (#391).

**What remains**, none of which blocks deploying it:

| | what | effect on the gateway |
|---|---|---|
| **#363** | reaping expired claims, and the dispatch retry and dead-letter policy | whoever registers a gateway's descriptor owns its liveness; the gateway never heartbeats, and nothing yet reaps a claim a dead gateway left |
| **#633** | a concurrent claim or extend write can briefly make a live agent read as not found | a completion answered 404 by it is treated as definite: the worker reports `effect_observed` and the action is re-claimed; a keyed route deduplicates, so the cost is delay, not a second effect |
| **#638** | the image jobs pull their base images from Docker Hub anonymously and hit its rate limit | CI only; where the image is published, and where its base images come from, are maintainer decisions |
| **#630** | a label holder's declassification (relabel narrowing, copy-down) is not audited | not the gateway's own: it concerns writes inside the explorer, so an action input built by such a write-down reaches the gateway unflagged |

Out of scope for this slice, as the design says: Path B (a per-caller
principal), protocols other than HTTP, and more than one replica per surface.
SSH commands reach a host through this gateway with the `extensions/effects-ssh`
service as an HTTP target (`docs/effects-ssh.md`, #450).

## What exists

| file | what it is |
|---|---|
| `config.go` | flags, validation, lease arithmetic, credential sources |
| `routes.go` | the route table, the request binder, derived effects, the startup descriptor check |
| `classify.go` | the pure response classifier and the closed `ErrorCode` vocabulary |
| `timing.go` | clock anchoring, PRECHECK predicates, the send gate, the grace-period formula |
| `executorkey.go` | `ExecutorKey` decoding in both platform spellings |
| `dialer.go` | the egress-restricted target transport and the separate explorer client |
| `client.go`, `client_ops.go` | the internal dispatch client: pull, claim, extend, complete (bound on the claim fence), ambiguity, resolve (its own ref only), attestation presentation; every claim-scoped request carries the record's `Shoal-Correlation-ID` |
| `logging.go` | the one logging function, and the policy it enforces |
| `worker.go` | the worker loop: pull, filter, precheck, attest, claim, bind, send, classify, complete; renewal and FENCE_LOST; slots, backoff and drain |
| `unrecorded.go` | the unrecorded-report log (#514) and the directory lock that keeps the gateway to one replica |
| `gatewaycmd/` | the command: `run`, `unrecorded list`/`ack`, `grace-period`; `cmd/shoal-gateway` is its `main` |

The dispatch client is tested against the real explorer composition — the
embedded store, the real recorder and publisher, the authenticated webapi
handler, over a socket — in
`cmd/shoal-explore-web/effects_gateway_client_test.go`. Its principal is the
enqueuer's. A foreign claimant now has a credential: the executor mapping
below (#391). `cmd/shoal-explore-web/oidc_executor_e2e_test.go` drives a mapped
ServiceAccount token from a second issuer through pull, claim, extend and
complete on the real routes.

The gateway itself is tested end to end in
`cmd/shoal-explore-web/effects_gateway_binary_e2e_test.go`: the real
`shoal-gateway` binary, built by the test and run as a child process on the
system clock, against the explorer in-process (the real handlers, the durable
embedded stores, the real OIDC authenticator and executor mint) and a target
that performs one effect per idempotency key. Each scenario asserts the
target's effect count and the record's state: the happy path; a 422; SIGKILL
mid-effect, recovered by a restart under the same `ExecutorKey` with one
effect and the dead fence's completion refused; a SIGTERM drain; a second
SIGTERM (exit 4, with the run in the unrecorded log); a plane outage during
completion, held in the log and cleared by the replay after a restart; a
second instance refused by the lock; a gateway bound to the wrong executor,
which claims nothing; attestation presented before the claim; an
unreachable plane, which refuses the start with nothing sent; work enqueued
while the gateway is already pulling; and
`unrecorded list` and `ack` through the binary.

## Configuration

| flag | default | rule |
|---|---|---|
| `-dispatch-url` | | required; https, or http to loopback |
| `-allow-plaintext-dispatch` | false | admits a remote `http://` explorer behind a mesh that authenticates the hop |
| `-dispatch-token-env` / `-dispatch-token-file` | `SHOAL_DISPATCH_TOKEN` / | exactly one; the file form is read per call, which is how a projected token rotates |
| `-agent-id` | | required; the descriptor ID as unpadded base64url, not a display name |
| `-capability` | `effects.http` | fleet name grammar |
| `-surface-name` | | required; the ambiguity report's `target` |
| `-target-base-url` | | required; https, or http to loopback; **no plaintext opt-out**; no userinfo, query or fragment |
| `-target-credential-env` / `-target-credential-file` | `SHOAL_TARGET_CREDENTIAL` / | exactly one; sent verbatim as the value of `-target-auth-header` |
| `-target-auth-header` | `Authorization` | must not be a header the gateway writes itself |
| `-target-client-cert-file` / `-target-client-key-file` | | both or neither; https target only; a client certificate presented to the target (mTLS, below) |
| `-target-ca-file` | | https target only; the CA bundle the target is verified against instead of the system roots, for the target only |
| `-idempotency-header` | | required with any `key` route, refused without one; distinct from the auth header |
| `-idempotency-retention` | | required with any `key` route, refused without one; must exceed `T + 5s` |
| `-target-allow-private` | false | admits private and loopback resolution; link-local and metadata stay refused |
| `-routes` | | required; the route table as a JSON array |
| `-claim-lease` (L) | 4m | `0 < L ≤ 5m` |
| `-operation-timeout` (T) | 3m | positive |
| `-plane-timeout` | 10s | positive, `≤ L/4` |
| `-max-response-bytes` | 64 KiB | 1 to 1 MiB |
| `-pull-limit` | 32 | 1 to 256 |
| `-pull-interval` | 2s | at least 100ms |
| `-health-address` | | optional `host:port` for `/healthz`, `/readyz` and `/metrics` |
| `-renew` | false | extend the claim every `L/2` (#430); see below |
| `-executor-ref` | | required; the ref the dispatch credential is minted for, and the descriptor names |
| `-unrecorded-dir` | | required; the unrecorded log and the single-replica lock. A persistent volume |
| `-max-in-flight` | 4 | 1 to 64 claims held at once |
| `-pod-name` | host name | names the replica in claim IDs; printable ASCII, no spaces or `\|` |
| `-attestation-statement-file` / `-attestation-key-file` | | both or neither; required when an action requires attestation, refused when none does |

Two credential rules carried over from the LLM gateway: passing the env flag
beside the file flag is refused even when the env flag is typed as its default,
and an unset env variable is *absence* while an unreadable file is *breakage*.
Absence of a target credential is accepted only for a loopback target, or for a
target the gateway presents a client certificate to and no credential flag is
passed, where the certificate is the credential. A credential that is named
(`-target-credential-env FOO` with `FOO` unset, or an unreadable
`-target-credential-file`) is refused either way: it was meant to be sent.
The certificate is presented only when the target's handshake asks for one,
so a target that does not ask for one receives no credential at all; configure
both for a target that needs both.

### Client certificates (mTLS)

The target client can present a client certificate (#662). There is one target
per gateway, so the certificate is per target: every route presents it to the
one surface the egress policy admits.

- **Presented when asked.** The pair is offered whenever the target's handshake
  requests a client certificate, and not otherwise.
- **Reloaded at each handshake.** Both files are read again for every new
  connection, so a rotated Secret takes effect without a restart. A
  connection keeps the certificate it was opened with: an idle one closes
  after 30 seconds, and a busy one keeps its certificate until it closes. A
  pair that cannot be loaded at a handshake (a rotation caught half written)
  keeps the last pair that could, and the gateway says so on stderr at most
  once a minute, naming the flags and nothing from the files. At startup there is no such
  fallback: a missing file, a key that does not match, or a certificate whose
  extended key usage excludes client authentication refuses the start.
- **Never logged.** No error carries a file's contents or the certificate.
- **The target is still verified.** A client certificate changes how the
  gateway proves itself, not whether it checks the target. `-target-ca-file`
  replaces the system roots for the target client only; the explorer client
  keeps its own. It is read once at startup, so a changed bundle needs a
  restart. Verification is never turned off.
- **Egress is unchanged.** The certificate and the roots are TLS settings of
  the one transport the egress policy already restricts. They add no
  destination.

With both a client certificate and a credential, both are sent. Whether the
target needs one or both is the target's decision. effects-ssh accepts
either, or requires both when both are configured (`docs/effects-ssh.md`,
"Caller authentication").

The executor reference the gateway's descriptor registers, and that an
executor-bound dispatch credential names, follows one rule:

An executor reference must pass `executorref.ValidExecutorRef`
(`pkg/executorref`, #391). Every place that accepts one applies this same rule,
and parity tests hold them all to the same verdict:
- fleet registration;
- the ATPL compiler;
- attestation presentation, on the server and in `pkg/attestation/api`;
- an action-execution decision's executor binding;
- the explorer's `-fleet-executor-refs`, `-fleet-external-executor-refs`,
  `-fleet-external-egress-executor-refs` and `-fleet-ask-executor-ref` flags,
  the `-fleet-executor-attestation` trust file, and the chart's
  `explorer.fleet.*ExecutorRef(s)` values.

The rule is a fixed ASCII charset, `^[A-Za-z0-9][A-Za-z0-9._:/@-]*$`, from 1
to 1024 bytes. A reference starts with a letter or digit and continues with
letters, digits and `. _ : / @ -`. It contains no spaces and no non-ASCII
characters. References are compared byte for byte, and inside this charset
two references that look alike are equal.

**Migration.** The rule tightens *registration*, as #544's floor did.
- Stored descriptors keep resolving.
- A descriptor whose reference falls outside the charset is refused with
  `invalid_argument` at its next `Register`, or at the next ATPL
  `plan`/`apply` that declares it.
- A new attestation presentation for such a reference is refused the same way,
  and no executor binding can name it.
- Host configuration naming one is refused at startup. The error names the
  flag and the entry's position.

If this happens to a descriptor that has worked for a long time, the control
is working: rename the executor reference, and its host binding, into the
charset and re-register.

### Lease arithmetic

```
0 < L ≤ MaxActionClaimTTL (5m)
planeTimeout ≤ L/4
without renewal:   L > T + 5s + planeTimeout
with key routes:   retention > T + 5s
```

Without renewal the lease bounds the claim, the operation and the report
together, which limits an operation to about 4.5 minutes.

**`-renew`** lifts that limit. The worker extends the claim every `L/2`
(re-attesting first when the action requires it), so the lease becomes a
silence interval, and the `L > T + 5s + planeTimeout` rule no longer applies;
`planeTimeout ≤ L/4` still does, so one failed extension can be retried before
`L/2` is overdue. The send gate then asks `leaseLocal − now ≥ L/2` instead of
`≥ T + 5s`, and each action's deadline (`deadlineLocal − now ≥ T + 5s`) is the
bound on the operation. The design's example — `L = 60s`, `T = 10m`, plane
timeout 15s — is refused without `-renew` and accepted with it. A refused
extension, or the local lease end arriving, is FENCE_LOST (below). The grace
period does not depend on `-renew`. The retention rule
exists because the two claim-time rules `T + 5s < deadline − now` and
`deadline − created_at ≤ retention` cannot both hold when the retention is
shorter: nothing would ever be claimable, and the gateway would look idle.

### Grace period

`terminationGracePeriodSeconds ≥ T + max(5s, 3×planeTimeout) + 2×5s + 5s`:
the in-flight request, its completion (whose client may send the body three
times, each bounded by the plane timeout), the two fallback ambiguity reports,
and five seconds to exit. `GracePeriod(T, planeTimeout)` computes it from the
same constants the worker spends, and `GracePeriodSeconds` rounds up. For the
defaults (`T = 3m`, plane timeout 10s) that is 225s; for `T = 10m` and a 15s
plane timeout, 660s.

The command computes it three ways, all from the same function:
`shoal-gateway grace-period -operation-timeout T -plane-timeout P` prints the
seconds; `run` logs it at start ("terminationGracePeriodSeconds must be at
least N"); and `/metrics` exports it as
`shoal_effects_gateway_grace_period_seconds`. Set the pod's
`terminationGracePeriodSeconds` to at least that figure. The chart computes it
from the entry's timeouts ([In the chart](#in-the-chart)).

## Issuing executor credentials

A worker claims work it did not enqueue, so it needs `OperationExecute`. In
the explorer exactly one thing grants it: the executor mapping,
`-oidc-executor-mapping-file` (`SHOAL_OIDC_EXECUTOR_MAPPING_FILE`; chart value
`explorer.auth.oidc.executorMapping`). No workspace, Fleet, development or
approver mapping holds execute, and `oidc_execute_grant_test.go` asserts both
halves. Without the file, no token can pull, claim, extend or complete queued
work.

The intended credential is a **projected ServiceAccount token**. The kubelet
mints it for the gateway's pod, signs it with the cluster's service-account
issuer, and rotates it. The gateway reads it from `-dispatch-token-file` on
every call, so a rotated token takes effect without a restart.

### The mapping file

```json
{
  "version": "shoal.executors/v1",
  "issuer": "https://oidc.eks.eu-west-1.amazonaws.com/id/0123456789ABCDEF",
  "audience": "shoal-executors",
  "service_assertion": {
    "claim": ["kubernetes.io", "namespace"],
    "equals": "shoal-gateways"
  },
  "executors": [
    {"subject": "system:serviceaccount:shoal-gateways:stripe", "executor_ref": "stripe"},
    {"subject": "system:serviceaccount:shoal-gateways:ledger", "executor_ref": "ledger"}
  ]
}
```

The file is decoded strictly. Unknown fields, duplicate keys, keys that match
a field only up to case, and trailing data are all refused, at any depth. Any
refusal stops the explorer from starting. A refusal names the entry's
position, never its value.

| field | rule |
|---|---|
| `version` | `shoal.executors/v1` |
| `issuer` | The executor credentials' own issuer, which may differ from `-oidc-issuer`. It is held to the OIDC issuer rule (#553): an absolute `https` URL with a valid host, and no user info, query, or `#` anywhere (an empty fragment, `https://x/#`, included). It must also be canonical: exactly as `url.Parse` renders it, a lower-case host, no default port, no dot segments. A token's `iss` must equal it byte for byte. |
| `jwks_uri` | Optional. Overrides discovery for this issuer, as `-oidc-jwks-uri` does for the human issuer. `https` only. |
| `audience` | Required, and disjoint from `-oidc-audience` and the approver mapping's audience. |
| `service_assertion` | Required, and positive: a claim path (a list of segments; a dotted string is one key, never a path) that must equal `equals` exactly. For projected ServiceAccount tokens, use the namespace claim `["kubernetes.io", "namespace"]`. For an IdP client-credentials token, use a marker such as Auth0's `["gty"]` = `client-credentials`. "Absent" is not an accepted form, because absence proves nothing about who a token was issued to. |
| `executors` | 1 to 256 `{subject, executor_ref}` entries. A subject is compared byte for byte with the token's `sub`: no trimming, case folding or normalization. `executor_ref` must pass `executorref.ValidExecutorRef`. Subjects are unique and references are unique: one credential maps to one surface, and one surface has one credential. |

**Choose a service assertion no human token can ever carry.** Good choices:
- Kubernetes: the ServiceAccount claim `["kubernetes.io", "namespace"]`, or
  `["kubernetes.io", "serviceaccount", "name"]`.
- Entra: `["idtyp"]` = `app`. Entra sets it only on app-only tokens.

Never assert a claim a user can be given, such as a group, a role or a
scope. The assertion separates the two kinds of principal in both
directions.

**One principal is never both a human and an executor.** While the mapping is
configured, the workspace and approver branches refuse any token that either:
- (a) satisfies the service assertion, whichever issuer signed it; or
- (b) is from the executor issuer and has a `sub` the mapping names.

Both refusals are the generic `401`. The rule matters most when the mapping
names `-oidc-issuer` itself, which is allowed on purpose: Entra workload
identities share the tenant issuer with humans. Without the rule, a mapped
service principal's token sent to the workspace audience would be minted as
a reader, with any label grant its claims match. With it, that token is
refused there and works only on the executor audience. A human of a
*different* issuer whose `sub` happens to spell a mapped subject is
unaffected by (b), but is still refused by (a) if their token carries the
assertion.

The second issuer gets its own discovery and JWKS cache, separate from the
human issuer's. Both issuers may publish a key under the same `kid`. A key
from one issuer still never verifies a token the other issuer's parser
accepts. The mapping may also name `-oidc-issuer` itself; the audiences still
keep the two branches apart. The executor branch accepts the same signing
algorithms as `-oidc-allowed-algs`. Kubernetes signs with RS256, the default.

The explorer must be able to fetch the issuer's discovery document
(`<issuer>/.well-known/openid-configuration`) or the `jwks_uri` override over
TLS that it trusts:
- On a managed cluster, the public OIDC issuer URL works (EKS, GKE, AKS).
- On a self-managed cluster, the API server serves discovery only to callers
  the `system:service-account-issuer-discovery` ClusterRole is bound to, and
  its certificate is usually signed by the cluster CA. Publish the issuer's
  discovery and keys somewhere the explorer can reach and trust instead.

### What a mapped token is minted as

| | |
|---|---|
| operations | `execute` and `agent_resolve`, and nothing else. `agent_resolve` is confined to the descriptor the binding names: resolving another reference's descriptor answers `not_found`, and a list holds only the bound descriptor (`resolvableUnderBinding`, #573). The gateway resolves its own descriptor at startup. |
| service role | `action_execution`. It cannot heartbeat or register: a worker cannot truthfully assert a descriptor's liveness. |
| executor binding | the entry's `executor_ref`. The fleet narrows every execute route to it (#573). Another reference's work answers `not_found`. |
| subject, actor, client ID | `oidcexec:<issuer>#<sub>`, one identity. It is outside the human identity family (`oidc:`, `oidcid:`, `entra:`), so it is never compared with a requester or an approver. The client ID is set because attestation requires one. |
| on behalf of | none. A worker acts as itself. |
| sources, policies | the workspace source and grant policy. **No label policy**, whatever the label grants file says, so labelled evidence stays redacted on a pull. |
| provenance | the issuer, the raw `sub`, and the mapping digest |
| correlation ID | taken from `Shoal-Correlation-ID`, or minted (`oidcexec-correlation-…`). Every dispatch route needs one (#527). |

The explorer refuses a token on the executor audience when any of these is
true:
- it carries `act`, `may_act`, a claim-overage indicator (`_claim_names`,
  `_claim_sources`, `hasgroups`) or the configured `-oidc-delegation-claim`;
- it also names a workspace or approver audience (and a human token naming
  the executor audience is refused on the human branch too);
- the service assertion fails;
- its `sub` is not mapped;
- it is signed by any key other than the executor issuer's.

The human branches refuse an executor's credential as described under "The
mapping file". At startup the explorer also refuses a mapping entry whose
`executor_ref` is not among `-fleet-executor-refs`, naming the entry's
position. Every external reference must already be in that list. No
descriptor could register against such a reference, so the credential would
be bound to nothing.

Every refusal is the same generic `401`. At startup the explorer prints the
mapping digest and the number of executor credentials it holds.

### Projecting the token

Give each gateway its own ServiceAccount, so its token's `sub` is
`system:serviceaccount:<namespace>:<name>`. Turn off automounting, and project
a token on the executor audience:

```yaml
spec:
  serviceAccountName: stripe
  automountServiceAccountToken: false
  containers:
    - name: gateway
      args:
        - -dispatch-token-file=/var/run/shoal/dispatch/token
      volumeMounts:
        - name: dispatch-token
          mountPath: /var/run/shoal/dispatch
          readOnly: true
  volumes:
    - name: dispatch-token
      projected:
        sources:
          - serviceAccountToken:
              audience: shoal-executors
              expirationSeconds: 3600
              path: token
```

The kubelet refreshes the file before the token expires (at 80% of its
lifetime). The explorer adds `-oidc-clock-skew` to the token's `exp`, and no
more.

To revoke a credential, remove its entry and roll the explorer. To rotate one
onto a new ServiceAccount, add the new subject under a new reference and
re-register the descriptor; one reference never has two credentials.

### In the chart

Set `explorer.auth.oidc.executorMapping` to the document as a map. The chart:
- renders it as JSON into a ConfigMap it owns (`<release>-executors`);
- mounts it read-only at `/etc/shoal/executors` and passes
  `-oidc-executor-mapping-file`;
- puts its checksum on the pod template, so a changed mapping rolls the pod.

`validate.yaml` refuses, at render time, the shapes the explorer would refuse
at startup:
- a wrong version;
- a non-canonical or non-`https` issuer, or one with a query or a `#`;
- a missing audience, or one shared with the human audiences;
- a missing or non-positive service assertion;
- blank, duplicate or out-of-charset entries;
- a placeholder.

It also refuses an `executor_ref` that is not in `explorer.fleet.executorRefs`:
no descriptor could register against it, so the credential would be bound to
nothing.

## Routes

A route maps one fleet action to one HTTP request. An action with no route is
never performed.

```json
[
  {
    "action": "charge",
    "method": "POST",
    "path": "/v1/accounts/{account}/charges",
    "effects": ["external", "egresses-content"],
    "idempotency": "key",
    "query": ["expand"],
    "conflict": {"status": [400], "pointer": "/error/type", "equals": ["idempotency_error"]},
    "retryable": [429, 503],
    "reference": {"pointer": "/id", "pattern": "ch_[A-Za-z0-9]+"}
  },
  {
    "action": "remove-item",
    "method": "DELETE",
    "path": "/v1/items/{id}",
    "effects": ["external", "egresses-content"],
    "idempotency": "natural",
    "conflict": {"status": [404], "pointer": "/error/code", "equals": ["resource_missing"]}
  }
]
```

Decoding is strict: unknown fields, duplicate keys, trailing data and an empty
table are refused, and every refusal names the route by index and action.
Field names must be spelled exactly. `encoding/json` matches fields
case-insensitively, with Unicode folding (`ſ` folds to `s`), so without this
`{"method":"GET","METHOD":"POST"}` would decode as POST while reading as GET.

- **Methods.** POST, PUT, PATCH, DELETE. GET, HEAD and OPTIONS are refused: a
  read performs no effect.
- **Idempotency.** `key` sends the action's `ExecutorKey` in
  `-idempotency-header`; `natural` requires PUT or DELETE, the only methods
  idempotent by specification; `unprotected` sends nothing and never re-sends
  a request that may have been written.
- **Paths.** A parameter is a whole segment, `{name}`. Values are
  percent-escaped as one segment, so `a/b` is `a%2Fb`, and empty, `.` and `..`
  values are refused.
- **Effects** are derived from the target and the declaration must match:
  `{external}` for a loopback target, `{external, egresses-content}` for
  anything else. Over-declaring egress is not the safe direction — the effect
  floor refuses only understatement, so an overstated set is silently denied by
  a policy forbidding egress.
- **Conflict** is allowed on `key` and `natural` routes, and always needs
  `status`, `pointer` **and** `equals`; a status-only rule is refused. The
  commonest same-key conflict on a byte-identical retry means "the original is
  still in flight" (409 `idempotency_key_in_use` and its relatives), and it
  arrives exactly on the written → timeout → retry path. Reading it as success
  records an effect that may yet fail. **In-progress codes must be
  `retryable`, never in `equals`;** only a body value that means "this exact
  request already succeeded" belongs there. A status may be both a conflict
  status and retryable, which is how a provider that answers 409 for both
  "already done" and "still in flight" is configured: the marker is success,
  anything else under that status retries under the same key until the
  provider replays the original outcome. A conflict status whose body lacks
  the marker and that is *not* listed retryable is recorded `outcome_unknown`,
  never `target_rejected`. For a natural DELETE, a conflict on 404 needs the
  target's not-found marker in the body; a 404 without it is
  `outcome_unknown`.
- **Reference** is a target-side identifier copied into the record. Exactly
  one of `pointer` or `header`; the pattern is anchored by the gateway and must
  not match the empty string; a value over 256 bytes is dropped.

### The action's input

```json
{"path": {"account": "acct_1"}, "query": {"expand": "balance"}, "body": {"amount": 100}}
```

Closed: no other top-level field, each spelled exactly; path and query values
are strings; every template parameter is present and no undeclared one is; a
DELETE route takes no body. `Route.InputSchema()`
renders this as the action's `input_schema`, so the explorer refuses a malformed
input at enqueue rather than after a claim.

### The request is a pure function of the record

`Binder.Bind(route, input, executorKey)` produces the method, URL, headers and
body, and nothing else contributes: no `Date` header, no request ID, no nonce, a
fixed `User-Agent`. The body is `json.Compact(input.body)`, and query parameters
are sorted. A re-claim therefore sends byte-for-byte the request the first claim
sent, which is what lets a target deduplicating on key plus body treat it as
the same request, and what lets a key conflict be read as evidence the effect
happened. The credential is attached per request, outside the binder.

### Startup check

`VerifyDescriptor` compares the resolved descriptor with the table: actions and
routes one to one, each action's effects equal to its route's, each
`input_schema` equal to the route's `InputSchema()`, and each `output_schema`
equal to the canonical closed schema (`OutputSchema()`). Any mismatch fails
closed. The input-schema check matters because the explorer enforces it at
enqueue: a looser one lets work be queued and claimed — setting
`EffectPossible` — before the binder refuses it.

The resolve, `VerifyDescriptor` and the attestation requirements
(`AttestationRequirements`) are read **once, at startup**. A running gateway
does not notice the descriptor being re-registered: a change to an action's
effects, its schemas, or whether it requires attestation takes effect for the
gateway only when it restarts, and is checked against the route table then.
Restart the gateway after re-registering its descriptor.

## Classification

`Classify` is a pure function of the route and one observed attempt.

| observed | key / natural | unprotected |
|---|---|---|
| not written (DNS, dial, TLS, egress refused) | retry | retry |
| 2xx | success | success |
| 2xx with `Idempotent-Replayed: true` | success, `replayed` (key only) | success |
| conflict status, body marker matches | success, `conflict` | refused at config |
| conflict status, no marker (or body unreadable), status listed retryable | retry, same bytes | refused at config |
| conflict status, no marker (or body unreadable), not retryable | `failed/outcome_unknown` | refused at config |
| configured retryable status | retry, same bytes, honouring `Retry-After` seconds | `failed/outcome_unknown` |
| written, then error, timeout or reset | retry, same bytes | `failed/outcome_unknown` |
| 3xx (never followed) | `failed/outcome_unknown` | `failed/outcome_unknown` |
| other 4xx/5xx | `failed/target_rejected_NNN` | same |

"Written" is set from the first moment a byte may have reached a connection,
erring towards written: a write misread as never-sent is a second effect on an
unprotected route, while the reverse is only an honest `outcome_unknown`.

`ErrorCode` is closed: `request_not_sent`, `outcome_unknown`,
`target_rejected_NNN`, `retry_exhausted`, `input_invalid`. The dispatch client
refuses to send any other. A worker that gives up retrying uses
`request_not_sent` if no attempt was ever written and `retry_exhausted`
otherwise.

Success records `{status, idempotency, reference?}` and nothing else; evidence
is always empty. Response bodies are read through an `io.LimitReader` bounded by
`-max-response-bytes`, and an oversize body is success without a reference.

## Timing

Every margin is a server-issued difference applied to a local monotonic
instant; no local wall-clock reading is compared with a server timestamp.

- **PRECHECK**, on pull-page data, before anything is claimed:
  `deadline − created_at ≤ retention` for key routes, and
  `serverNow + T + 5s < deadline`, where `serverNow` is the pull response's
  `Date` plus one second plus the local time elapsed since it arrived. A
  failing record is skipped; it cannot be recorded on the action.
- **Anchoring**, from the claim response: `leaseLocal = t₀ + (claim_lease_until
  − updated_at)` and `deadlineLocal = t₀ + (deadline − updated_at)`, with `t₀`
  the local instant the claim ID was *first* sent. Both err early.
  `deadlineLocalLatest = t₁ + (deadline − updated_at)`, with `t₁` the local
  instant the answer that was read arrived, errs late; it bounds every lease
  the explorer can grant on the claim.
- **Send gate**, before every attempt: not draining,
  `deadlineLocal − now ≥ T + 5s`, and `leaseLocal − now ≥ T + 5s` (or
  `≥ L/2` with `-renew`).

An agent enqueuing with a conventional 30-second request deadline creates a
30-second action, which PRECHECK skips: `Deadline` is the enqueuing request's
own context deadline.

## The worker loop

`Worker.Run` is the loop, in `worker.go`; `WorkerConfig` is what the command
builds from `Config`.

```
PULL → FILTER → PRECHECK → [ATTEST] → CLAIM → BIND → SEND → CLASSIFY → COMPLETE
                                         └──────── FENCE_LOST ────────┘
```

- **PULL** only with a free slot (`-max-in-flight`, default 4) and room
  reserved in the unrecorded log. An empty page or an error backs off,
  jittered over `[d/2, d]`, doubling from the pull interval to 30s.
- **FILTER**: the record's agent, capability, and an action with a route. The
  pull page has no server-side filter, and a claim sets `EffectPossible`.
- **PRECHECK** as above. A skipped record is logged once per version.
- **ATTEST** when the action requires attestation (`AttestationRequirements`
  reads it from the resolved descriptor) and the attestation in hand expires
  before the explorer's now `+ L + 5s`. A presentation that is still too short
  is not claimed under.
- **CLAIM** with a fresh claim ID per attempt. `repull`, 404 and 409 send the
  worker back to PULL; a 409 on an attested action also forgets the
  attestation in hand.
- **BIND** failure sends nothing: `request_not_sent` is reported, then the
  action completes `failed/input_invalid`.
- **SEND** behind the send gate, re-checked before every attempt and once
  more after the credential is read. Retries reuse the bound request (same
  key, same bytes), at most five attempts, waiting the target's
  `Retry-After` capped at 30s or a jittered backoff.
- **COMPLETE** on the claim fence, with `effected` on a failure that may have
  left on an egress route: the request body of every attempt that may have
  been written (#427).

**Renewal.** Every `L/2` from the anchor of the last claim or extension, the
worker extends — bound on the fence (#629), re-attesting first when needed —
and re-anchors on the explorer's returned end, which the explorer clamps to
the deadline; never on the lease it asked for. A lease clamped to the deadline
is not extended again. An extension whose answer is lost keeps the end already
held and is retried after the plane timeout.

**FENCE_LOST** is a refused extension or the local lease end arriving:

| when | the worker |
|---|---|
| nothing sent | sends nothing more, reports `request_not_sent`, lets the claim lapse |
| a request in flight | lets it finish, then reports `effect_observed` (a success) or `outcome_unknown` |

The same report follows a completion the explorer refused (404, 409, …). A
completion whose answer may have committed (`indeterminate`, #506) stops
renewing, waits out the lease, and then reports through the ambiguity route,
which appends at `expected_version` 0. "The lease" there is the latest end any
renewal could have granted. When an extension's answer was never read
(cancelled, timed out, lost), it may still have applied — at any time: the
request's deadline is a local wall reading the explorer judges on its own
clock, and a write already under way can commit after it — so the wait runs
to the action's deadline on its late side, `DeadlineLocalLatest =
answerReceived + (deadline − updated_at)`. The explorer clamps every lease to
the deadline and applied the claim before its answer arrived, so this is
skew-free: server minus server, on the local monotonic clock. The cost is
waiting to the deadline in the rare case of an indeterminate completion
behind an unknown renewal.
Each fence gets at most one report.

**Shutdown** (context cancelled): stop pulling and go not-ready (`draining`).
A claim with nothing sent reports `request_not_sent` and lapses. A request in
flight finishes and completes, falling back to the report and then to the
unrecorded log. The start-up retry of held reports also stops on SIGTERM;
what it did not reach stays on disk. The drain waits at most
`DrainBound` — the grace period less its five-second exit margin, because the
kubelet's clock starts before SIGTERM arrives. If that runs out, every
unfinished run whose request may have reached the target is written to the
unrecorded log as `outcome_unknown`, all of them in one durable rewrite,
before it is abandoned; an `abandoned` event carries the count, and `Run`
returns `ErrDrainAbandoned`. Every bound the worker spends — the operation,
each plane call, the completion budget, each report window — is measured on
the worker's own clock, and stopping a renewal cancels its call in flight
rather than waiting for it, so the honest worst path (request, completion,
both fallback reports) fits inside the drain bound; a test drives it with
every call blocking to its timeout and measures it. `HardStop` (a second
signal) takes the same abandonment path at once: mark, cancel, then the
unrecorded-log write, which `Run` waits for before it returns; a `HardStop`
after `Run` has returned does nothing. Both rely on one invariant, rather than
on winning a race with the runs: a run whose request was handed to the target
leaves the worker's set of runs only once it is settled — its outcome recorded
on the plane or written to the unrecorded log — so a snapshot taken at any
moment holds every effect not yet accounted for. A randomized stop-timing test
(`TestEveryEffectIsAccountedForWhateverTheStopTiming`, 500 iterations) checks
that every effect the target performed is accounted for however the signals
land. `Kill` is SIGKILL itself, for tests: it abandons everything
and writes nothing, and the next instance re-claims after the lapse and
resends under the same `ExecutorKey`.

## The unrecorded log

A report the explorer refused, did not find, or could not confirm after two
attempts is never "nothing to report" (#514). It is appended to
`unrecorded.jsonl` in the unrecorded directory:

- **Entry**: action ID, fence, claim nonce, route action, method, path
  template, outcome, target, the bounded reference, the record's correlation
  (a retry must send it), the explorer's status and the client's error kind,
  first and last times, attempts. No input, filled path, body, header or
  error text.
- **Durability**: every change rewrites the log through a temporary file that
  is fsync'd and renamed, then fsyncs the directory. A torn trailing line is
  tolerated on read; any other unreadable line refuses the start.
- **Bound**: 1024 entries and 1 MiB, kept by reserving room for one report
  before each claim. When no room is left the worker stops claiming and is
  not ready (`unrecorded_log_full`).
- **Retry**: every entry is presented again at start. One the explorer now
  records — an identical replay is accepted (#542) — leaves the log; the rest
  stay.
- **A write that fails**: the run stays held with the entry it could not
  write. When the drain's work is done it retries that entry, once; if the
  write fails again, `Run` returns `ErrUnrecordedUnwritten` (the command exits
  1), never a clean stop with an effect on no record and in no log. The same
  holds for a hard stop whose write fails: `Run` reports the failed write
  ahead of the stop itself.
- **Clearing**: otherwise only `UnrecordedLog.Ack` / `Worker.AckUnrecorded`,
  which the command exposes as `shoal-gateway unrecorded ack`. The ack opens
  the log, so it runs against a stopped gateway's directory (see
  [Operating the unrecorded log](#operating-the-unrecorded-log)).
- **Signals**: the `unrecorded` and `unrecorded_cleared` log events carry the
  entry count (the gauge, also `Worker.UnrecordedEntries`), and `readiness`
  events carry the not-ready reason.

The directory also holds `effects-gateway.lock`, flock'd for the life of the
log: a second gateway on the same directory refuses to start.

## Running the gateway

```
shoal-gateway run [flags]
shoal-gateway unrecorded list -unrecorded-dir DIR
shoal-gateway unrecorded ack -unrecorded-dir DIR (ACTION_ID[:FENCE]... | -all)
shoal-gateway grace-period [-operation-timeout T] [-plane-timeout P]
```

`internal/effectsgateway/gatewaycmd` is the command; `cmd/shoal-gateway` is a
`main` that passes it the process's signals.

**Startup**, in this order, any failure a refused start:

1. Every flag is parsed and validated (exit 2). Nothing has touched the disk
   or the network.
2. The unrecorded log is opened, which takes `effects-gateway.lock` in
   `-unrecorded-dir`. A second gateway on the directory stops here, before
   any network I/O: two replicas never both talk to the explorer.
3. The dispatch client is built with the executor credential, bound to
   `-executor-ref`, and the gateway resolves its own descriptor
   (`-agent-id`). A refused, unavailable or foreign descriptor (one naming
   another executor ref) refuses the start.
4. The descriptor is checked against the route table (`VerifyDescriptor`,
   above): the capability must be declared and its actions must equal the
   routes. If any action requires attestation, the attestation flags are
   required and the attestation is presented now; if none does, the flags
   are refused.
5. The worker runs: it retries the unrecorded log, then pulls.

**Signals.** The first SIGTERM or SIGINT drains (above, *Shutdown*), within
`DrainBound`. A second is a hard stop (`Worker.HardStop`). It does not wait,
but it is not SIGKILL either; it does what a drain that runs out does, in
this order: nothing more is claimed or sent (a claim taken after the stop
began is not registered, and a run the stop has not yet marked refuses to
send), every run is marked abandoned, everything in flight is cancelled, and
then every run whose request may have reached the target, and whose outcome
is not yet on the record or in the log, is written to the unrecorded log as
`outcome_unknown`, all in one durable rewrite. Cancelling does not unsend, so
the write comes after it and still covers those runs. Nothing more is
reported. The write is bounded only by the disk: a stuck fsync holds the
process in it, and the alternative — exiting without the entries — is the
loss the log exists to prevent; the kubelet's SIGKILL at the end of the grace
period is the bound then. Runs that sent nothing
simply lapse, and the next instance re-claims them under the same
`ExecutorKey`. A real SIGKILL (the kubelet after the grace period, or an OOM
kill) still writes nothing; there the re-claim, resending under the same
`ExecutorKey` to a target that deduplicates on it, is what covers the run.

**Exit codes.**

| code | meaning |
|---|---|
| 0 | drained cleanly |
| 1 | startup refused, or the worker failed — including any stop, a hard stop or a drain, that could not write an outcome to the unrecorded log (`ErrUnrecordedUnwritten`): an effect that may have happened is on no record and in no log, and only the gateway's `dispatch_error` events show it |
| 2 | the command line is wrong |
| 3 | the drain bound ran out with work unfinished (`ErrDrainAbandoned`); every abandoned run whose request may have reached the target is in the unrecorded log |
| 4 | a second signal stopped the gateway without draining, and every sent request is accounted for — on the record, or in the unrecorded log. A hard stop whose write failed exits 1, not 4 |

**Health** (`-health-address`):

- `/healthz` is liveness: 200 while the process answers, draining or not.
- `/readyz` is readiness: 200 only while the worker is pulling. Otherwise it
  is 503, and the `worker` dependency's `detail` names why: `starting`,
  `draining`, `unrecorded_log_full` or `stopped`.
- `/metrics`: `shoal_effects_gateway_unrecorded_entries` (the gauge an alert
  should watch: any value above zero is a report awaiting reconciliation),
  `shoal_effects_gateway_in_flight` (claims held),
  `shoal_effects_gateway_grace_period_seconds`, and the house
  `shoal_dependency_ready{name="worker"}`.

The probes need a listener, so "no inbound listener" does not hold for a
gateway with `-health-address`; it serves nothing else.

### Operating the unrecorded log

`unrecorded list` prints one JSON object per held report, with the closed
fields only: `id` (`ACTION_ID:FENCE`, what `ack` takes), the action ID, the
fence, the claim nonce, the route action, method and path template, the
outcome, `has_reference`, the explorer's status and the client's error kind,
first and last times, and attempts. It never prints the reference (text the
target returned, however tightly the route's pattern bounds it), the target
name or the correlation ID; the operator who needs the reference to reconcile
reads `unrecorded.jsonl` itself.

`unrecorded ack` clears the reports named, once the operator has reconciled
each by other means, and logs an `unrecorded_cleared` event per entry. It is
one durable rewrite (`UnrecordedLog.AckAll`): all of the named reports are
cleared, or none. A bare
action ID is accepted when exactly one report holds it; otherwise name the
fence. Every name must match before anything is removed. `-all` clears
everything.

Neither creates the directory: a directory that does not exist is an error
(exit 1), not an empty log, so a mistyped path cannot read as "nothing awaits
reconciliation". Only `run` creates it. Both take the directory's lock, so
both refuse while a gateway runs on the directory: the running gateway owns the log, holds it in memory, and would
write back an entry acknowledged under it. Stop the gateway (or scale it to
zero) first. A running gateway's backlog is visible meanwhile on `/metrics`
and in its `unrecorded` log events.

## In the chart

### The image

`Dockerfile.shoal-gateway` builds `cmd/shoal-gateway` into distroless static,
from the same digest-pinned base images as `Dockerfile.shoal-embed`. The binary
is `/usr/local/bin/shoal-gateway` (the entrypoint), it runs as uid/gid 65532,
and `/var/lib/shoal-gateway` is the unrecorded log's mount point. BuildKit is
required (`docker buildx`).

```
make gateway-container-build GATEWAY_IMAGE=ghcr.io/YOUR_ORG/shoal-gateway:TAG
make gateway-container-smoke                # build, then check user, entrypoint, grace-period
```

The `shoal-gateway image` workflow builds and smoke-tests it on every change
that reaches the binary. It publishes nothing: where the image is pushed, and
where its base images come from (#638), are maintainer decisions.

### What an entry renders

`effectsGateways` is a list, empty by default, with one entry per surface.
Each entry is deep-merged over `effectsGatewayDefaults`, so it names only what
differs. Nothing renders unless an entry has `enabled: true`. An enabled entry
renders these objects, each named `<stem>-gw-<name>`, where the stem is the
release's full name cut to 24 characters:

| object | what it is |
|---|---|
| Deployment | `replicas: 1` (or 0 to stop it), `strategy: Recreate`, `automountServiceAccountToken: false`, and a computed grace period (below) |
| ServiceAccount | Created unless `serviceAccountName` names an existing one. It has no token automount. Its subject, `system:serviceaccount:<namespace>:<stem>-gw-<name>`, is what the executor mapping binds to `executorRef`. |
| ConfigMap `-routes` | The route table as JSON. It reaches `-routes` through the environment, and the pod template carries its checksum, so a changed table rolls the pod. |
| PersistentVolumeClaim `-unrecorded` | ReadWriteOnce, 64Mi, annotated `helm.sh/resource-policy: keep` |
| NetworkPolicy | Only with `networkPolicy.enabled` |

There is no Service, no registrar credential and no heartbeat. The gateway
serves only its probes, and it never asserts its descriptor's liveness (#391).

**At most one replica, Recreate.** `replicas` is 1, or 0 to stop the gateway;
`validate.yaml` refuses anything else. A second replica would share the
executor principal and the unrecorded log: the log's lock refuses the second
process, and two workers on one surface can evict each other's claims (#514).
Recreate means a rollout stops the old pod and waits out its drain before the
new pod mounts the claim. A surge would put two pods on one ReadWriteOnce log.

**One principal per entry.** Two enabled entries may not share an
`executorRef`, an `agentID`, an effective ServiceAccount (a defaulted
`<stem>-gw-<name>` included) or an unrecorded claim (an `existingClaim` that
names another entry's chart-created `<stem>-gw-<name>-unrecorded` included). Each of those makes two
workers on one executor principal (#514), and with separate claims no lock
notices the second.

**The explorer in the same release mints the credential.** When
`explorer.enabled` is true and an entry's token is projected, the chart also
refuses:
- a `dispatch.tokenAudience` other than `explorer.auth.oidc.executorMapping.audience`;
- a mapping that does not bind the pod's subject,
  `system:serviceaccount:<release namespace>:<account>`, to the entry's
  `executorRef`.

A Secret-sourced token is opaque to the chart and is not checked.

**The unrecorded log is on a claim.** `-unrecorded-dir` is
`/var/lib/shoal-gateway/unrecorded`, a subdirectory of the claim's mount. The
claim survives `helm uninstall`. Delete it by hand once `unrecorded list` shows
it empty. `unrecorded.storage` takes one of:
- `persistentVolumeClaim`, the default;
- `existingClaim`, for a claim you own;
- `emptyDir`, which is refused unless `acceptLossOfUnrecordedReports: true`
  is also set. Even then, the install's NOTES print a warning. The log dies with
  the pod, and with it the only record of effects the explorer refused to
  record.

**The grace period is computed.** The chart sets
`terminationGracePeriodSeconds` to `T + max(5s, 3 × planeTimeout) + 2 × 5s + 5s`,
rounded up to whole seconds. This is the figure
`shoal-gateway grace-period -operation-timeout T -plane-timeout P` prints. An
entry's `terminationGracePeriodSeconds` can raise it, and a value below it is
refused.

Computing it, rather than asking for it and checking it, keeps it from going
stale. A timeout change cannot leave the grace period behind, because there is
no second value to forget to update. Checking a supplied value would still need
the formula in the chart, so it would carry the same risk of drift while also
asking the operator for a number. The template's copy of the formula is kept
honest by `deploy/helm/validate-chart.sh`. It builds the binary, renders the
chart for a table of `(T, P)` pairs covering every branch of the formula and
its rounding, and fails on any difference. The Helm chart workflow runs it on
any change to `cmd/shoal-gateway`, `internal/effectsgateway` or the chart.

**Credentials are files.** Each one is mounted read-only at mode 0440, with
`fsGroup: 65532`.
- The executor token, `-dispatch-token-file`, is either a projected
  ServiceAccount token on `dispatch.tokenAudience` (the default) or
  `dispatch.credentialSecretName`.
- The target credential is `target.credentialSecretName`, mounted as
  `-target-credential-file`. It is required unless the target is loopback.
- The attestation statement and key come from `attestation.secretName`
  (optional), mounted as `-attestation-statement-file` and
  `-attestation-key-file`.

The chart never renders the environment forms. A file is re-read on each
call, so a rotated token or an updated Secret takes effect without a restart.

**Probes and metrics.** `-health-address` listens on `healthPort` (8102).
Liveness is `/healthz` and readiness is `/readyz`. Metrics are `/metrics` on
the same port, with `prometheus.io/*` pod annotations. Readiness gates nothing,
since there is no Service, but it is what a rollout waits on, and
`unrecorded_log_full` shows there.

**Security context.** The pod runs as non-root uid/gid 65532 with the
`RuntimeDefault` seccomp profile. The container has a read-only root
filesystem, no privilege escalation and every capability dropped. The only
writable path is the claim.

**NetworkPolicy** (`networkPolicy.enabled`, off by default):
- Ingress is allowed to the health port only.
- Egress is allowed to DNS, to the explorer and to `targetCIDRs`, on the
  target's port or on `targetPorts`.
- The explorer defaults to this release's explorer pods on
  `explorer.containerPort`. Override it with `explorer.podLabels`,
  `explorer.namespaceLabels` and `explorer.port`.
- A policy matches addresses, not names. An FQDN target is held only to the
  CIDRs listed, and a remote target with none listed is refused.
- The gateway's own dialer refuses link-local and metadata addresses whatever
  the policy allows.

### Values

Every key, with its default, is in `effectsGatewayDefaults` in
`deploy/helm/shoal/values.yaml`.

| key | default | flag / object | rule |
|---|---|---|---|
| `name` | | object names | required. A DNS label, at most 24 characters, unique. The entry's identity: never change it on a running entry (see [Renaming an entry](#renaming-an-entry)) |
| `enabled` | false | | renders the entry |
| `image.repository`, `.tag`, `.pullPolicy` | `ghcr.io/example/shoal-gateway`, `dev`, `IfNotPresent` | | |
| `replicas` | 1 | Deployment | 0 or 1 |
| `agentID` | | `-agent-id` | required, unpadded base64url |
| `capability` | `effects.http` | `-capability` | |
| `surfaceName` | | `-surface-name` | required |
| `executorRef` | | `-executor-ref` | required; `executorref.ValidExecutorRef`. With the explorer in the same release, it must be in `explorer.fleet.externalEgressExecutorRefs` (or `externalExecutorRefs` for a loopback target) |
| `serviceAccountName` | chart-created | `serviceAccountName` | |
| `dispatch.url` | | `-dispatch-url` | required. https, or http to loopback |
| `dispatch.allowPlaintext` | false | `-allow-plaintext-dispatch` | admits a remote http:// explorer |
| `dispatch.tokenSource` | `projected` | `-dispatch-token-file` | `projected` or `secret` |
| `dispatch.tokenAudience` | | projected token | required with `projected` |
| `dispatch.tokenExpirationSeconds` | 3600 | projected token | at least 600 |
| `dispatch.credentialSecretName`, `.credentialSecretKey` | , `token` | Secret token | required with `secret`, refused with `projected` |
| `target.baseURL` | | `-target-base-url` | required. https, or http to loopback; no userinfo, query or fragment |
| `target.allowPrivate` | false | `-target-allow-private` | required for loopback |
| `target.authHeader` | `Authorization` | `-target-auth-header` | |
| `target.credentialSecretName`, `.credentialSecretKey` | , `credential` | `-target-credential-file` | required unless loopback |
| `target.idempotencyHeader`, `.idempotencyRetention` | | `-idempotency-header`, `-idempotency-retention` | required with a `key` route, refused without one; retention > T + 5s |
| `routes` | `[]` | `-routes` via ConfigMap | required, a list of [routes](#routes) |
| `timing.claimLease` | 4m | `-claim-lease` | `0 < L ≤ 5m` |
| `timing.operationTimeout` | 3m | `-operation-timeout` | positive |
| `timing.planeTimeout` | 10s | `-plane-timeout` | positive, `≤ L/4` |
| `timing.renew` | false | `-renew` | without it, `L > T + 5s + planeTimeout` |
| `terminationGracePeriodSeconds` | computed | pod spec | may only raise the computed value |
| `maxInFlight` | 4 | `-max-in-flight` | 1 to 64 |
| `pullLimit` | 32 | `-pull-limit` | 1 to 256 |
| `pullInterval` | 2s | `-pull-interval` | |
| `maxResponseBytes` | 65536 | `-max-response-bytes` | 1 to 1048576 |
| `attestation.secretName`, `.statementKey`, `.keyKey` | , `statement`, `key` | `-attestation-*-file` | optional |
| `unrecorded.storage` | `persistentVolumeClaim` | `-unrecorded-dir` volume | or `existingClaim`, or `emptyDir` with `acceptLossOfUnrecordedReports` |
| `unrecorded.size`, `.storageClassName`, `.existingClaim` | `64Mi`, , | claim | |
| `healthPort` | 8102 | `-health-address` | 1 to 65535 |
| `resources`, `nodeSelector`, `tolerations`, `affinity` | small requests, none | pod spec | |
| `networkPolicy.*` | disabled | NetworkPolicy | `targetCIDRs` required with a remote target |

Durations are Go literals built from whole numbers and `ms`, `s`, `m` or `h`
(`90s`, `1m30s`). Every value is rendered as a quoted scalar or as JSON, and a
line break anywhere in an entry is refused (#468).

### A complete entry

The executor mapping must map this gateway's ServiceAccount
(`system:serviceaccount:<namespace>:shoal-gw-stripe` for a release named
`shoal`) to `stripe`. The Secret `stripe-api-key` holds the whole
`Authorization` value under the key `credential`.

```yaml
effectsGateways:
  - name: stripe
    enabled: true
    image:
      repository: ghcr.io/YOUR_ORG/shoal-gateway
      tag: TAG
    agentID: c3RyaXBlLWdhdGV3YXk
    surfaceName: stripe
    executorRef: stripe
    dispatch:
      url: https://shoal-explorer.shoal.svc:8098
      tokenAudience: shoal-executors
    target:
      baseURL: https://api.stripe.com
      credentialSecretName: stripe-api-key
      idempotencyHeader: Idempotency-Key
      idempotencyRetention: 24h
    timing:
      claimLease: 60s
      operationTimeout: 2m
      planeTimeout: 15s
      renew: true
    routes:
      - action: charge
        method: POST
        path: /v1/charges
        effects: [external, egresses-content]
        idempotency: key
        conflict: {status: [400], pointer: /error/type, equals: [idempotency_error]}
        retryable: [429, 503]
        reference: {pointer: /id, pattern: "ch_[A-Za-z0-9]+"}
    networkPolicy:
      enabled: true
      explorer:
        podLabels: {app.kubernetes.io/name: shoal-explore-web}
        namespaceLabels: {kubernetes.io/metadata.name: shoal}
      targetCIDRs: [203.0.113.0/24]
```

The explorer in this example runs in another namespace, `shoal`. When it is
in the same release, also list `stripe` in `explorer.fleet.executorRefs` and
`explorer.fleet.externalEgressExecutorRefs`, or the chart refuses the entry.
`deploy/helm/validate-chart.sh` renders this example.

### Acknowledging unrecorded reports

`unrecorded ack` refuses while the gateway holds the log's lock, so stop the
gateway first: set the entry's `replicas: 0` and `helm upgrade`. (A
`kubectl scale` works too, but the next `helm upgrade` reverts it, and an
upgrade run while you are acknowledging would restart the gateway under you.)
Once the pod is gone, run the command in a pod that mounts the same claim:

```
kubectl run shoal-gw-stripe-ack --rm -it --restart=Never \
  --image=ghcr.io/YOUR_ORG/shoal-gateway:TAG \
  --overrides='{"spec":{"securityContext":{"runAsUser":65532,"runAsGroup":65532,"fsGroup":65532},
    "volumes":[{"name":"u","persistentVolumeClaim":{"claimName":"shoal-gw-stripe-unrecorded"}}],
    "containers":[{"name":"ack","image":"ghcr.io/YOUR_ORG/shoal-gateway:TAG",
      "args":["unrecorded","list","-unrecorded-dir","/var/lib/shoal-gateway/unrecorded"],
      "volumeMounts":[{"name":"u","mountPath":"/var/lib/shoal-gateway"}]}]}}'
```

Replace `list` with `ack ACTION_ID:FENCE` once each report is reconciled. Then
set `replicas: 1` and upgrade again.

### Renaming an entry

An entry's `name` is its identity. It names the Deployment, the ServiceAccount
(and so the executor credential's subject) and the unrecorded log's claim.
Renaming a running entry makes Helm delete the old Deployment while it creates
the new one. The old pod then drains while the new pod claims on the same
executor reference, which is two workers on one principal (#514). The old log
is also left on a claim that nothing mounts. Removing a running entry has the
same problem without the new pod. So do not change a running entry's name.
When you must:

1. Set the entry's `replicas: 0` and `helm upgrade`. Wait for the pod to
   terminate; its drain is bounded by the grace period.
2. Acknowledge the old log (above), or migrate it: copy `unrecorded.jsonl`
   into the new claim, or point the renamed entry at the old claim with
   `unrecorded.storage: existingClaim`.
3. Rename the entry, and re-map the new ServiceAccount subject in the executor
   mapping (or keep the old account with `serviceAccountName`). Set
   `replicas: 1` and upgrade.

The chart enforces step 1. On an install or upgrade it looks up the release's
effects-gateway Deployments, and refuses when one is still running (spec or
status replicas above zero) and no enabled entry names it. `lookup` returns
nothing under `helm template`, a client-side `--dry-run` and `helm lint`, so
those never see this refusal; `helm upgrade --dry-run=server` does.

## Egress

The target client reaches one host and one port. Each connection resolves the
name once and dials the checked address as a literal, and a `Control` hook
re-checks the socket's peer, so DNS rebinding cannot swap the address between
check and connect. Every resolved address must pass, not merely one.

Always refused: `0.0.0.0/8`, `169.254.0.0/16`, `fe80::/10`, `fd00:ec2::254`,
`100.100.100.200`, `168.63.129.16`, `192.0.0.192`, multicast, reserved and
unspecified addresses, and the same addresses embedded in NAT64 (`64:ff9b::/96`)
or 6to4 form. The RFC 8215 local-use NAT64 prefix `64:ff9b:1::/48` is refused
outright, because its embedding depends on a prefix length this code cannot
know. A zone (`%eth0`) is stripped before any check: no prefix contains a
zoned address.
Refused unless `-target-allow-private`: loopback, RFC 1918, RFC 4193,
`100.64.0.0/10` and `198.18.0.0/15`.

No proxy from the environment, no cookie jar, no redirects, no compression.
The explorer client is a separate client with a separate transport, and shares
the no-proxy, no-jar and no-redirect rules. The target's client certificate
and `-target-ca-file` are settings of the target transport only: the explorer
client never presents the certificate, and never trusts the target's roots.

## Completion: lost responses, and the wire as found on main

**Lost or unreadable response (permanent).** The report may have committed
after any of these first answers:

- a transport error, or a 503 with `Shoal-Commit-Outcome: indeterminate`;
- a 502 or 504 — a proxy in front of the explorer can answer either after the
  explorer processed the request;
- a 2xx whose body does not decode, or does not describe this claim's terminal
  record at exactly the reported version plus one — the route answered
  success, so something committed, and `protocol` alone would hide it.

The client resends the identical body once; the completion route's replay
branch answers it with the committed record.

**Every 503 is possibly committed (interim, until #505).** Today
`ErrExecutionAmbiguous` and `ErrActionCommitted` (a durable write whose
publication failed) reach the wire as bare 503s with no
`Shoal-Commit-Outcome` header, so a header-less 503 from `/complete` may hide a
committed write. The client therefore treats *any* 503 from `/complete` like a
lost response. `ErrRecordingUnavailable` is also a bare 503; on the dispatch
surface every audit precedes its store write, so it is in fact a clean
refusal, but nothing on the wire tells it apart and the client never reads
error-message text. (The "committed" in `MarkCommittedInteraction` is about an
interaction record, not the action.) When #505 marks the genuinely
indeterminate sentinels, a header-less 503 means a clean refusal before any
write, and the trigger narrows back to the header; the resend on a genuinely
lost response stays.

**After a possibly-committed answer, only a record is definite.** Once the
first answer was any of the above, any 503, or a #492-shaped 400/500, the
client returns the committed record or `indeterminate`, nothing else:

| resend answered | result |
|---|---|
| 2xx with the record | the record (`recorded_otherwise` rules apply) |
| transport error, any 503, 502 or 504 | `indeterminate` |
| anything else — 409, 404, 400, 500, a 2xx that is not this report's record, … | one third read through the replay branch; a record settles it, anything else is `indeterminate` |

It never returns the first attempt's status, which describes a request whose
outcome the resend was sent to learn. A 409 or 404 answering the resend is not
definite: `ErrExecutionAmbiguous` means the first write may still be in flight
when the resend reads the record, which can then see the claim at the old
version with its lease lapsed and answer 409 for a report that commits a moment
later. A #492 400/500 answering the resend is not definite either: the first
answer may have been a genuine error that committed nothing while the resend
committed and had its record discarded. With nothing possibly committed before
it, a first-attempt 409 or 404 is definite and returned as it is.

**Recorded otherwise (permanent).** The committed record is compared with the
report — state, error code, and for a success the output as a JSON value. Any
difference returns the record with a `recorded_otherwise` error: the record is
final and must not be reported again. The record's version must be exactly
the reported version plus one: the route answers 200 only for a fresh terminal
write or a replay of one, and a terminal record does not move past that.

**#492 workaround (temporary).** Two behaviours of the completion route on
main, pinned as *current* behaviour by the real-handler tests:

- a durably recorded failure is answered HTTP 500;
- a success whose output the explorer refuses is recorded as failed
  (`invalid_executor_output`) and answered HTTP 400.

Both come from the `/complete` handler discarding the committed record
`CompleteClaim` returns alongside an error (#492). Until that is fixed the
client also resends after a 400 or 500, with the same rule for a lost resend.
When #492 lands the two pins flip to 2xx and the 400/500 trigger is removed.

`not-found` from `Claim` means re-pull, never "gone": the loser of a claim race
is told not-found deliberately.

`repull` from `Claim` is every answer after which the claim may have
committed: a transport error, any 503 (with or without the indeterminate
header), or a 502 or 504 that a proxy can answer after the explorer processed
the claim. It is never reported as a definite failure; but `Claim` returns no
action, so the caller holds no claim, executes nothing, and re-pulls. A claim
that did commit lapses at its lease and reappears on the pull page; a
completion against a claim the worker does not hold is refused. The original
error, with its kind, is the cause. The bare-503 case is interim: today
`ErrActionCommitted` reaches the wire as a bare 503, and after #505 a
header-less 503 is a clean refusal and narrows back to `unavailable`.

## Logging policy

One function writes log lines, and its argument has no free-text field.

- **Allowed:** action ID, claim fence, claim nonce (the CSPRNG part of the
  claim ID), route action, method, path *template*, status, classification,
  transport-failure kind, gate refusal, dispatch-error kind, byte counts,
  durations.
- **Never:** credentials, the action's input, the filled path or query, bodies,
  headers, or anything the target said in words.

In particular a target client's `err.Error()` is never logged: Go's
`*url.Error` embeds the full request URL, and the URL carries input-derived
path parameters. A failure is logged as its kind — `dns`, `dial`,
`connection_refused`, `tls`, `timeout`, `canceled`, `reset`, `egress_refused`,
`other` — computed from the error's type and never from its text. A test pins
that a `url.Error` carrying a secret path parameter does not reach the log, and
another fails if a field is added to the log record outside the policy.

## Deviations from the slice-1 design

- **`Idempotent-Replayed` counts only on a 2xx.** The design lists "2xx or
  `Idempotent-Replayed`" as success. Providers replay the original response
  whatever it was, so a replayed 402 is a declined payment repeated back; reading
  the header alone as success would record a decline as a completed effect.
- **A conflict status whose body cannot be read is `outcome_unknown`.** The
  design does not cover it; success and `target_rejected` would each be a guess.
- **3xx is `outcome_unknown`.** Redirects are never followed, and a 303 after a
  POST commonly means "created".
- **The send gate without renewal** requires `leaseLocal − now ≥ T + 5s`; the
  design states only the renewing form (`≥ renewAfter`).
- **Conflict rules need a body value.** The design allows "status + optional
  JSON pointer value"; a status-only rule is refused, because the commonest
  same-key conflict on a retry means "still in flight". A conflict status
  without the marker retries if listed retryable and is otherwise
  `outcome_unknown`, not `target_rejected_NNN`.
- **Field names must be spelled exactly**, in the route table and the action's
  input, and the startup check also compares `input_schema`.
- **A DELETE route takes no body**, matching its `InputSchema()`.
- **The completion resend** recovers a lost or unreadable response — transport
  error, indeterminate 503, 502, 504, or a 2xx that is not this report's
  record (permanent) — and, until #505, any 503, and until #492 a 400 or 500;
  after any of these only a record is definite, and everything else is
  `indeterminate`. Not in the design.
