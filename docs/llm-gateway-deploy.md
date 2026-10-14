# Deploying the Shoal LLM gateway

`shoal-llm-gateway` is the enforcement plane: an OpenAI-compatible endpoint a
caller points at instead of the real provider. For each request it asks the
Explorer for admission, applies whatever obligations it can satisfy, forwards or
refuses, and reports the outcome. The value is that it needs no cooperation from
the caller — an agent framework, an IDE plugin, a shell script with `curl`:
anything that speaks the API is governed by changing one base URL.

**A withhold obligation is applied only to content the caller attributes.** A
caller that sends `shoal_attribution` gets every segment it attributed to a
withheld reference replaced by a fixed placeholder; a caller that does not gets
the call refused as `obligation_unsatisfiable`. The guarantee is narrow and is
stated in full under
[Withhold obligations and attribution](#withhold-obligations-and-attribution).
Read anything below about obligations with that in mind.

- [Why this is a separate document](#why-this-is-a-separate-document)
- [Why it is a separate process](#why-it-is-a-separate-process)
- [The flag contract](#the-flag-contract)
- [Withhold obligations and attribution](#withhold-obligations-and-attribution)
- [Listener transport: TLS, or plaintext on loopback](#listener-transport-tls-or-plaintext-on-loopback)
- [Kubernetes: the Helm chart](#kubernetes-the-helm-chart)
- [The render-time refusals](#the-render-time-refusals)
- [Orchestrator probes: the health surface](#orchestrator-probes-the-health-surface)
- [Metrics: what to alert on](#metrics-what-to-alert-on)
- [Stateless, and not a singleton](#stateless-and-not-a-singleton)
- [Secrets: two credentials, both required](#secrets-two-credentials-both-required)
- [A rotating credential: the projected ServiceAccount token](#a-rotating-credential-the-projected-serviceaccount-token)
- [Upgrading](#upgrading)
- [What the chart cannot express yet](#what-the-chart-cannot-express-yet)

## Why this is a separate document

`docs/shoal-explore-web-deploy.md` is organised around one binary with one state
root served by one replica, and nearly every deployment decision in it follows
from that. The gateway has the opposite properties — no state root, several
replicas, a rolling update that is allowed to surge — and a different threat
model. Folding it into that document would make its title untrue and would bury
the one contrast that matters most about the two planes.

The two documents cross-reference instead. Read that one for the Explorer the
gateway asks; read this one for the gateway.

## Why it is a separate process

It must not be inside `shoal-explore-web`. The Explorer holds the policy store
and the corpus; the gateway handles untrusted prompt content from arbitrary
callers and speaks to third-party endpoints. Putting them in one process puts
prompt injection in the same address space as the decision plane.

That is also why the admission seam is an HTTP API rather than a Go interface —
see `docs/admission-seam.md`. The gateway is a different binary and, under the
execution boundary, an out-of-process actor whose effects Shoal declares but
does not contain.

One consequence shapes everything below. The gateway is the first place in Shoal
where a denial means the work does not happen at all; everywhere else,
enforcement withholds from a response that is still produced. So the gateway must
fail closed, and a gateway that fails closed on everything is not a crash — it is
a pod that passes every probe and refuses every request. From outside, that is
indistinguishable from a total outage of whatever is configured to go through
it. Almost every guard in the chart exists to move one way of producing that
from a production incident to `helm template`.

## The flag contract

| Flag | Purpose |
| --- | --- |
| `-listen` | OpenAI-compatible listen address, for example `0.0.0.0:8100`. Plaintext only on loopback; see [Listener transport](#listener-transport-tls-or-plaintext-on-loopback). |
| `-tls-cert-file`, `-tls-key-file` | PEM certificate chain and key the listener serves TLS with. Both or neither, checked at startup, and re-read per handshake so a rotated certificate is served without a restart. |
| `-allow-plaintext-listener` | Accept plaintext on a non-loopback `-listen`, for a mesh sidecar that terminates mTLS. Off by default; refused beside the TLS flags. |
| `-health-address` | Separate plaintext probe listener serving `GET /healthz`, `GET /readyz` and `GET /metrics`. See [Metrics](#metrics-what-to-alert-on). |
| `-allowed-host` | Comma-separated exact-match external authorities — the same gate as the Explorer's. |
| `-admission-url` | Base URL of the Explorer's authenticated API. A path prefix is allowed (the admission routes are joined onto it); user info, a query or a fragment is refused at startup. See below. |
| `-allow-plaintext-admission` | Accept a remote `http://` `-admission-url`. Off by default, and the admission hop only. |
| `-admission-token-env` | Environment variable holding the bearer token the gateway presents to the Explorer. Mutually exclusive with the file form. |
| `-admission-token-file` | File holding that token instead, read per request. The only form a rotating credential has. |
| `-upstream-base-url` | The real OpenAI-compatible provider, including the version segment it documents. |
| `-upstream-api-key-env` | Environment variable holding the upstream credential. Mutually exclusive with the file form. |
| `-model` | Model names this deployment expects, comma-separated. Optional; see below. |
| `-upstream-api-key-file` | File holding that credential instead, read per request. |
| `-agent-id` | Registered descriptor this gateway admits against. |
| `-agent-generation` | `int64`, positive. |
| `-capability` | Registered capability name. |
| `-action` | Registered action name. |
| `-source-id` | Scope source, canonical unpadded base64url. |
| `-policy-id` | Scope policy, canonical unpadded base64url. |
| `-lease` | Admission lease duration. Capped at `MaxActionClaimTTL`, five minutes. |
| `-request-timeout` | Upstream request timeout. |

Every one of these is a container argument on the pod spec rather than a
ConfigMap entry, for the reason the Explorer's are: a changed argument changes
the pod template and rolls the pod on its own. A ConfigMap would need a checksum
annotation to get the same effect, and without one an edited `-admission-url`
would leave the running gateway asking the old decision plane while the chart
claimed the new one.

The gateway speaks to the plane through the public admission client
(`pkg/admission/api`), which is built at startup. Two consequences changed with
that move:

- An `-admission-url` carrying user info (`https://user:pass@host`), a query
  (`?tenant=a`, or a bare `?`) or a fragment is refused at startup with
  `-admission-url invalid admission client configuration`. Before, a query was
  carried silently onto every admission path and user info was accepted; both
  now fail before the pod reports ready rather than shaping every request.
- A bearer token the client cannot send is refused locally, per call, without
  a request: an empty value fails as `invalid bearer token`, as does one
  containing a line break. In this binary an unset or empty variable and an
  empty token file were already refused locally by the credential readers
  (`no credential is configured`, `<path> is empty`), so the new check is the
  client's backstop rather than a change an operator will normally see; had
  an empty value reached the old transport, it would have gone out as
  `Authorization: Bearer ` and come back a remote 401 (`proxy credential
  rejected`). Either way the call is refused `plane_unavailable`.

### Credentials, and what "read per request" does and does not buy

Both credentials are read at the moment they are used rather than captured at
startup. **That is a rotation mechanism for file-backed sources, and nothing
more.** Two earlier versions of this guide claimed more than that, so it is
worth being exact about both halves.

It does not make an environment variable rotatable. A process environment is
fixed once the container starts: updating the Secret behind a `secretKeyRef`
leaves every running gateway on the old value until the pod is replaced, so
re-reading `os.Getenv` per request re-reads the same string forever.

And it does not keep a credential out of a crash dump. An environment value is
process state and stays resident for the process lifetime, so a dump carries it
however often it is read — and a file-backed credential is in memory too while
a request is using it. For the variable forms, per-request reading avoids one
extra cached copy and buys no security property at all.

Rotation needs a source that can change underneath a running process, which is
what the `-file` forms are for:

| Source | Changes under a running pod? |
| --- | --- |
| `secretKeyRef` into an environment variable | No. Requires replacing the pod. |
| A Secret mounted as a volume | Yes. The kubelet updates the file in place. |
| A projected ServiceAccount token | Yes, and it is rewritten on rotation by the kubelet. |
| A CSI driver's volume (SPIFFE, Vault Agent, secrets-store-csi) | Per that driver. |

Each credential takes exactly one form. The binary refuses an explicitly chosen
`-admission-token-env` together with `-admission-token-file` — and the same pair
for the upstream key — rather than ranking them, because picking one silently
makes the effective credential invisible: an operator who adds a file while a
stale variable is still in the manifest cannot tell from the configuration which
one is being presented, and the symptom of the wrong answer is an authentication
failure that names neither. The variable flags ship with non-empty defaults, so
"both set" means the default was left in place rather than that two were chosen
deliberately — which is why the default is what gets compared, and why the file
form works without also blanking the variable.

### The plaintext acknowledgement covers one hop

`-allow-plaintext-admission` accepts a remote `http://` decision plane. It
exists for a mesh that already supplies the transport authentication the scheme
would, and it makes accepting that an explicit act: over plaintext the bearer
token crosses the network in the clear and so does the verdict, and anything on
the path can rewrite a deny into an allow — which removes the enforcement plane
while leaving every sign that it is running.

## No Kubernetes API credential in this pod

The gateway pod sets `automountServiceAccountToken: false`, unconditionally.

It needs no API access: it speaks HTTP to the workspace with a bearer token and
HTTP to the provider with another, and nothing it does touches the API server.
Without this, Kubernetes puts a token for the pod's ServiceAccount on the
filesystem anyway — so a compromise of the one process in this chart that parses
arbitrary caller input would inherit whatever RBAC that account carries. That is
the opposite of the reason the gateway is a separate process from the workspace.

This suppresses only the *automatic* mount. An explicit `serviceAccountToken`
projection is a volume this chart declares, and the kubelet still mints it, so
`llmGateway.admission.tokenFileSource: projected` is unaffected. The two settings
look like they should conflict and do not — `validate-chart.sh` asserts the
projection, its audience and the suppression together, because if that were
wrong the projected-token form would be dead on arrival while every other check
still passed.

There is no values key for it, since one could only ever be used to put back a
credential nothing here consumes. An operator who genuinely needs a token in
this pod — for a sidecar, say — declares it as a volume through
`llmGateway.admission.tokenVolume`, which is explicit and auditable in the values
file rather than implicit in a default.

## Declared models, and why the list exists

`-model` (`llmGateway.models`) names the models this deployment expects. It is
optional, and an empty list is a working configuration.

The reason it exists is not routing — `-model` restricts nothing, and an
unlisted model is still forwarded. It is the declaration. The gateway sends Shoal
a description of what a call would do and never the payload, and `model` is a
caller-controlled free-text field: a prompt or a secret fits in it exactly as
well as `gpt-4o` does. So the declaration reports a **named** model as itself
and anything else as `other`, and the names come from the operator rather than
from the request.

The trade is explicit. With no list, no caller text can reach the plane through
this field, and the plane also cannot write policy about which model was used —
every call reports `other`. Naming models buys that granularity back for the
ones you name. Either way the caller's own `model` value goes upstream
unchanged, because rewriting it would make the gateway the reason an unmodified
client gets a different answer.

A plane that wants to refuse unfamiliar models can deny on `other`. Deciding
that here would make the gateway a model gate, which #390 lists as a non-goal.

## Withhold obligations and attribution

An admission can allow a call with an obligation to withhold some of the
references the caller declared in `shoal_references`. A reference is an ID; the
material it stands for is free text in `messages[].content`, and nothing on an
OpenAI-compatible request connects the two. `shoal_attribution` is the optional
connection (#426).

**Guarantee.** When a caller attributes content to a reference, the gateway
replaces every segment attributed to a withheld reference with a fixed
placeholder. It refuses the call (`obligation_unsatisfiable`, before contacting
the provider) when:

- a withheld reference has no attributed content, or
- a verbatim run of 96 bytes or more of withheld text appears anywhere else in
  the forwarded body.

**What this is not.** Attribution is the caller's own statement, as
`shoal_references` already is, and the gateway does not authenticate inbound
callers. This protects against mistakes by a *cooperating* caller, for example a
RAG pipeline that attributes the chunks it retrieved but includes one of them
twice. It does **not** protect against a caller that intends to send the
material: one that doesn't declare a reference, pastes text without attributing
it, misattributes it, or paraphrases it. To stop that caller, deny it the
document at retrieval.

### The shape

```json
"shoal_attribution": [{"reference":"<id>","message":3,"part":0,"start":120,"end":4210,"sha256":"<b64url>"}]
```

- **Decoding is strict.** When `shoal_attribution` is present the whole body is
  strictly decoded, so a duplicate key at any depth, invalid UTF-8 or trailing
  data is a 400, and edited messages are re-encoded. A body without it is parsed
  exactly as before.
- **`reference`** must be one of the declared `shoal_references`. Attribution
  without `shoal_references` is a 400.
- **`message`** is an index into `messages` and may be any role; the message
  must have a string role. **`part`** is required for array content and must
  point to a `text` part; it must be omitted for string content. Nothing else —
  `tool_calls` arguments, images, names — can be attributed in v1.
- **`start`/`end`** are UTF-8 byte offsets into the decoded string, on rune
  boundaries, selecting a non-empty range.
- **`sha256`** is the unpadded base64url SHA-256 of the segment, recomputed by
  the gateway. It only checks that the offsets select the caller's own bytes. It
  is never forwarded, logged or sent to the plane.
- **Limits:** overlapping segments are refused, whatever their references, and
  there are at most 256 entries.

All of this is validated before admission: a malformed attribution is a 400
that makes no call to the plane. `shoal_attribution`, like `shoal_references`,
is stripped from what the provider receives.

### What the provider receives

- Each withheld segment becomes the constant `[shoal: content withheld]`, with
  no ID, length or index. The segment is replaced, not removed, so surrounding
  text like "see below" doesn't dangle.
- The placeholder reveals to the provider that something was withheld. It
  reveals nothing to the caller that a refusal doesn't already.
- Segments attributed to references the plane allowed are forwarded unchanged.
  Allowed references need no attribution, so an unmodified client keeps the
  allow path, and a withhold against a caller that attributes nothing is
  refused exactly as it was before attribution existed.

### The residue check

Every string the provider would receive — every value and key, in every message
and in fields the gateway does not know — is searched for withheld text.
Withheld segments that are contiguous in the same string are joined first,
whatever their references, so attributing material as many short adjacent
segments does not shrink what is searched for; the check covers any verbatim
run of 96 bytes or more of that union. Each joined run is sampled as 64-byte
windows every 32 bytes; every forwarded
string is scanned with a rolling hash of the same width, and each hit is
confirmed by comparing bytes and extended to see whether it is part of a
verbatim run of 96 bytes or more. Any run that long contains a sampled window,
so none is missed. The work is bounded by the size of the body; a body that
produces more coincidences with withheld text than that bound allows is refused
rather than forwarded unexamined.

**Known gaps:**

- Splitting across messages, normalization changes and runs under 96 bytes all
  get past the residue check. Withheld segments separated by even one
  unattributed byte are not joined, so neither side of that byte counts toward
  the other's 96.
- Assistant turns, `tool_calls` arguments and images are not covered. The
  residue check still scans every string in them for a verbatim run, but nothing
  else about them is withheld.

### Audit and logging

- The admission's report outcome is unchanged; a refusal is reported to the
  plane as the failure `obligation_unsatisfiable`, the same code the plane
  already receives. The plane already records `Withhold`. There is no plane
  change.
- The gateway logs only counts (references, segments, bytes) and the refusal
  class (`undeclared_reference`, `unattributed_reference`, `residue`,
  `residue_bound`). It never logs content, digests, reference IDs or offsets.

### What #390 guarantees

#390's acceptance criterion, revised to describe what is actually guaranteed:

> An admission returning a withhold obligation forwards a request in which every
> segment the caller attributed to a withheld reference is replaced by a fixed
> placeholder. It refuses with `obligation_unsatisfiable`, before contacting the
> upstream, when a withheld reference has no attributed content or its text
> appears verbatim elsewhere (96 bytes or more). This is pinned by a test that
> embeds the material and fails if the material, not the ID, reaches the
> provider. Attribution is asserted by the caller, and the criterion makes no
> claim about unattributed content.

The test is `TestWithheldMaterialNeverReachesTheProvider` in
`cmd/shoal-llm-gateway`; `TestTheMutationsTheAcceptanceTestMustCatch` runs each
known regression — stripping only the ID, skipping the digest check with offsets
one rune off, skipping the residue check — through the same assertion and
requires it to fail.

## No plaintext acknowledgement for the provider hop

There is deliberately no counterpart for `-upstream-base-url`. That request
carries the prompt itself and the operator's provider credential, and a mesh
authenticating the hop to the Explorer says nothing about the hop to a third
party. A remote `http://` upstream is refused by the chart at render and by the
binary at startup, with no way to accept it.

## Listener transport: TLS, or plaintext on loopback

The gateway refuses to send a prompt to a remote provider over plaintext, and
since #424 it applies the same rule to the request it receives: **TLS anywhere,
plaintext on loopback only.** Before, the listener had no rule at all, so the
request the process refused to *make* in the clear was one it would *accept* in
the clear, carrying the same prompt.

| Listener | What the binary does |
| --- | --- |
| `-tls-cert-file` and `-tls-key-file` | Serves TLS (1.2 or later) on any address. |
| plaintext on a loopback address | Serves, as before. The default `-listen 127.0.0.1:8100` needs no flag. |
| plaintext anywhere else | **Refused at startup**, unless `-allow-plaintext-listener` is set. |

The loopback test is applied to the address the kernel actually bound, not the
string passed: `0.0.0.0:8100` and `:8100` are not loopback, and `localhost` is
only if it resolved there.

These are refused at startup rather than discovered per handshake:

- only one of `-tls-cert-file` and `-tls-key-file`;
- a file that cannot be read, or a certificate that is not PEM;
- a key that is not the certificate's (`private key does not match public key`);
- `-allow-plaintext-listener` beside the TLS flags, where it covers nothing and
  would read as though the listener were plaintext.

**The key pair is reloaded, not captured.** Both files are re-read on every
handshake, and a changed pair is adopted once it parses and matches. A
cert-manager `Certificate` reissues on a timer and the kubelet rewrites a
mounted Secret in place, so a renewed certificate is served without restarting
the pod, for the same reason the credential `-file` forms are read per request.
A changed pair that does not parse or does not match is not adopted and the
previous one keeps serving, which is what makes a rotation caught halfway (the
certificate read before the kubelet's symlink swap and the key after it) a
non-event. The refusal is logged once per distinct pair. If the files stay
broken, the old certificate is served until it expires and clients start
refusing it, so that log line is worth alerting on.

`-allow-plaintext-listener` is the one exception, shaped like
`-allow-plaintext-admission`. It is for a pod whose mesh sidecar terminates
mTLS and forwards to the container in plaintext. The container still has to
bind a non-loopback address for the sidecar to reach it, and the process cannot
see whether a sidecar is there, so relying on one is stated rather than
inferred. It opens the listener only. It does not relax `-upstream-base-url`, and
`-allow-plaintext-admission` does not cover the listener.

The health surface (`-health-address`) stays plaintext. It answers `/healthz`,
`/readyz` and `/metrics` (see [Metrics](#metrics-what-to-alert-on)); it carries
no prompt, is not published by the Service, and is addressed by a kubelet — and
a scraper — that would have to be configured to trust the certificate. The
chart's scrape annotations therefore name the plaintext health port and set no
`prometheus.io/scheme`.

### In the chart

The chart serves TLS by default. `llmGateway.tls.secretName` names a
`kubernetes.io/tls` Secret, whose `tls.crt` and `tls.key` are mounted read-only
at `/etc/shoal-llm-gateway/tls` (mode 0440, readable through `fsGroup`) and
passed as the two flags. The container and Service ports are named `https`, so
a mesh does not parse the handshake as an HTTP request. Clients use an
`https://` base URL and must trust the certificate's issuer. The name they dial
still has to be in `allowedHosts`, and it should also be in the certificate.

```yaml
llmGateway:
  tls:
    enabled: true
    secretName: shoal-llm-gateway-tls   # e.g. a cert-manager Certificate's Secret
```

With `tls.enabled: true` the Service may be any type, because the transport is
the process's own.

`tls.enabled: false` is the plaintext choice, made in the values file. The pod
binds `0.0.0.0`, so the chart then renders `-allow-plaintext-listener=true`.
Without it the binary would refuse to start. Use it only where an mTLS mesh
sidecar encrypts and authenticates the hop. The chart cannot see whether one
does. ClusterIP does not encrypt pod-network traffic.

A plaintext listener stays inside the cluster unless you say otherwise. A
non-ClusterIP Service type with `tls.enabled: false` is refused unless
`llmGateway.service.allowPlaintext: true` acknowledges a TLS-terminating load
balancer or ingress in front of it:

```yaml
llmGateway:
  tls:
    enabled: false
  service:
    type: LoadBalancer
    allowPlaintext: true   # TLS terminates at the load balancer, configured separately
```

That acknowledgement configures no TLS, and the chart neither installs nor
verifies the terminating hop. The hop from the load balancer to the pod is
still plaintext, so keep it on a trusted network or behind the mesh.
`service.allowPlaintext` beside `tls.enabled: true` covers nothing and is
refused.

`tls.enabled` and `service.allowPlaintext` must be booleans. A quoted `"false"`
is a non-empty string, which a Helm condition reads as true.

## Kubernetes: the Helm chart

`deploy/helm/shoal` renders the gateway as a **Deployment** with a Service and a
PodDisruptionBudget. The `llmGateway` values block is off by default and
independent of the chart's `mode`, which selects a storage topology the gateway
does not depend on.

```console
$ cp deploy/helm/shoal/values-llm-gateway.yaml my-llm-gateway-values.yaml
$ helm upgrade --install shoal deploy/helm/shoal -f my-llm-gateway-values.yaml \
    --set llmGateway.image.repository=ghcr.io/YOUR_ORG/shoal-llm-gateway \
    --set llmGateway.image.tag=TAG
```

The profile **does not install as shipped**, on the same principle as
`values-explorer.yaml`. Every value it leaves empty is one the chart refuses to
render without. `helm template` names the first missing one, so working through
the errors in order fills the profile.

Both planes compose from one values file. `llmGateway.admission.url` may equally
name an Explorer installed separately, in another namespace, or in another
cluster — the gateway needs one to ask, not one in the same release:

```console
$ helm upgrade --install shoal deploy/helm/shoal \
    -f my-explorer-values.yaml -f my-llm-gateway-values.yaml
```

Before changing the chart, run its checks:

```console
$ deploy/helm/validate-chart.sh
```

They render every profile, schema-check the output, assert that each refusal
below still refuses and each valid configuration still renders, and assert that
the gateway's two listeners are wired end to end. A guard that silently stops
firing is the failure they exist to catch, and the checks themselves are
mutation-tested: each guard is removed in turn and the case that must then fail
is confirmed to fail.

## The render-time refusals

| Setting | Why the chart will not render without it |
| --- | --- |
| `llmGateway.tls.secretName`, while `tls.enabled` (the default) | The listener serves TLS and needs a `kubernetes.io/tls` Secret to serve. Set `tls.enabled: false` only where an mTLS mesh encrypts the hop. |
| `tls.secretName` with `tls.enabled: false` | Nothing mounts or reads it, so the manifest shows a certificate the listener does not serve. |
| a non-ClusterIP `llmGateway.service.type` with `tls.enabled: false` and no `service.allowPlaintext: true` | A plaintext listener carrying prompts would be exposed outside the cluster. The acknowledgement is for a terminating load balancer configured separately; it enables no TLS. |
| `service.allowPlaintext: true` with `tls.enabled: true` | The listener serves TLS, so the acknowledgement covers nothing, and it would start mattering silently the day TLS is turned off. |
| a non-boolean `tls.enabled` or `service.allowPlaintext` | A quoted `"false"` is truthy in a Helm condition, so it would read as the opposite of what it says. |
| a credential file on, under or above `/etc/shoal-llm-gateway/tls` while TLS is on | That is where the key pair is mounted. Two volumes cannot share a mount path or nest inside a read-only one, so the API server refuses the pod. |
| `llmGateway.admission.url` | A gateway that cannot ask must deny. With no decision plane every request behind it is refused while the pod stays healthy. It must also be an absolute `http://` or `https://` URL: a bare host is a transport error on every admission. |
| a plaintext `admission.url` to a non-loopback host | Over `http://` the bearer token crosses the network in the clear, and so does the verdict — anything on the path can rewrite a deny into an allow, which removes the enforcement plane while everything still looks healthy. `admission.allowPlaintext: true` accepts it where a mesh already authenticates the hop. |
| `llmGateway.admission.tokenEnv`, `credentialSecretName`, `credentialSecretKey` | The Explorer's API is authenticated in every deployment this chart can render, so an admission request with no bearer token is a 401 — every time. The gateway then denies every call. Required **in the environment-variable form**; the file form requires other things instead, below. |
| a non-boolean `admission.allowPlaintext` | It is rendered verbatim into `-allow-plaintext-admission`, and Go's boolean flag parser refuses anything it cannot read as one. YAML words like `yes` and `on` are strings here, not booleans, and the pod exits at startup on a flag error. |
| a remote `http://` `upstream.baseURL` | No acknowledgement accepts it, unlike the admission hop: this request carries the prompt and the provider credential. The binary refuses it at startup too, so rendering it is a pod that never serves. |
| `llmGateway.terminationGracePeriodSeconds` not exceeding `admission.lease` by 5s | The gateway drains for the lease on SIGTERM, because the lease is the bound on an admitted call's whole lifetime including the report that closes it. This value is the kubelet's budget for the same window, so a shorter one means SIGKILL arrives mid-drain and kills the calls whose egress has already happened and whose grant has already been spent — an unreported grant produced by every rolling update, on a schedule, while the pod terminates cleanly as far as the kubelet is concerned. The margin is required because the grace period is whole seconds while a lease need not be, and because the process still has to exit after the drain returns. |
| `llmGateway.identity.agentID` that is not canonical unpadded base64url | The workspace decodes this field and the binary refuses an undecodable value at startup. The guard reaches encoding mistakes and **not** the class of wrong values: a `shoal.ID` is opaque and variable-length, so there is no width to validate, and a readable name that happens to decode is indistinguishable here from a registered ID. Of fifteen plausible names, eleven decode cleanly to garbage (`gateway` to five bytes, `my-agent` to six) and only four fail on length (`llm-proxy`, `proxy`, `agent`, each `4n+1`). The eleven are refused by the plane on every request instead, as a descriptor that does not exist. |
| `agentGeneration`, a port, `tokenExpirationSeconds` or `terminationGracePeriodSeconds` that is not a whole number as written | Each is rendered verbatim, and converting before testing is lossy in exactly the cases worth refusing: `int64` of `1.5` is a positive `1`, so a guard that only checked positivity passed it and then rendered `-agent-generation=1.5`, which exits the pod on a flag error. A leading zero is worse than a refusal because it works — `010` is parsed as octal `8`, a generation nothing was registered under. |
| `llmGateway.admission.lease` above `5m` | `pkg/explorer/fleet` caps an admission lease at `MaxActionClaimTTL` and **refuses** a request outside the bound rather than shortening it. A larger value is not a longer lease; it is every call denied as an invalid argument, which the caller cannot act on and an operator cannot tell apart from a policy denial. |
| `llmGateway.upstream.baseURL` | An admitted request has nowhere to go, so the caller sees a failure on exactly the calls policy allowed — and the admission is spent on work that never happened. Must also be absolute. |
| a remote upstream with no credential at all | The credential is read at request time and nothing puts one in the pod, so every admitted call is rejected by the provider after admission has been spent on it — policy allowed work that then did not happen. A Secret or an `apiKeyFile` with `apiKeyFileSource: volume` satisfies it; a loopback provider needs neither. |
| `llmGateway.upstream.requestTimeout` above `admission.lease` | A call that outlives its lease is performed under a token that can no longer be reported against. The fleet record calls that *abandoned* rather than resolved: whether the effect happened is unknown and stays unknown, which breaks the loop the report exists to close. |
| `llmGateway.allowedHosts` | An empty allow-list answers every request `421 Misdirected Request` before admission is even asked. See the Explorer document's host-authority section: the gate is the same, and the resolved listen address of a pod is an authority no real client sends. |
| `llmGateway.identity.*` | Admission resolves the agent, capability, action and scope together. An incomplete identity is not a startup failure — it is a gateway that comes up healthy and is refused on every request, with a denial that carries no reason by design. There is nothing in the pod log to read. `agentGeneration` must be a positive `int64`; `sourceID` and `policyID` must be canonical unpadded base64url. |
| `llmGateway.healthPort` equal to `containerPort` | Two listeners cannot share a port, and which one loses is decided by bind order. If the health listener wins the gateway serves nothing; if the traffic listener wins every probe answers `421` and the pod never becomes ready. |
| either port below `1024` | The container runs as uid 65532 with all capabilities dropped. The bind fails and the pod restarts forever. |
| `llmGateway.podDisruptionBudget.maxUnavailable: 0` | The Explorer uses `0` because it is a singleton over one state root. The gateway is stateless and replaceable, so a `0` budget protects nothing and instead makes `kubectl drain` and cluster-autoscaler consolidation block forever on a pod that could safely have moved. |
| a `tokenFile` or `apiKeyFile` that is relative, or names a file at `/` | The path is both the flag and the mount the chart derives from it. A relative path resolves against a working directory nothing in the pod spec guarantees, so every read fails while the pod stays healthy; a mount at `/` replaces the container's root filesystem and the binary with it. |
| a `tokenFileSource` or `apiKeyFileSource` the chart does not render | The flag would name a path with no volume mounted there, which is an unreadable credential on every request — a pod that passes every probe and governs nothing. |
| `admission.credentialSecretName` with `tokenFileSource: projected` or `volume` | Neither source reads a Secret, so it is a credential the operator believes is being presented and the pod never opens. This is the ambiguity the binary refuses two token flags over, arriving through the chart instead. |
| `admission.tokenAudience` with a source that issues no token | The audience names the verifier a projected token is minted for. With any other source it records a binding that exists nowhere in the deployment. |
| `tokenFileSource: projected` with no `serviceAccountName` | A projected token's subject is the pod's ServiceAccount, and left implicit that is the namespace's `default` — an account every other pod in the namespace can also mint a token for, so authorizing the gateway authorizes the namespace. Naming `default` explicitly is accepted, and is the acknowledgement. |
| `tokenFileSource: projected` with no `tokenAudience` | A token projected with no audience is issued for the cluster's own API server, which the Explorer is not. Every admission request is then a 401. |
| `tokenExpirationSeconds` below `600` | The API server's floor for a token projection. Below it the pod spec is invalid, so the Deployment is accepted and no pod is ever created from it — a rollout that never completes and no pod log at all. |
| an empty or self-naming `tokenVolume`/`apiKeyVolume` | A volume with a name and no source is not a pod spec the API server accepts; a second name renders a duplicate key, so the volume is named something the mount does not reference and the kubelet never mounts the credential. |
| both credential files in one directory | Each is mounted as its own volume at its own file's directory, and a pod cannot mount two volumes at one path. |
| an explicitly chosen `tokenEnv` beside a `tokenFile`, or `apiKeyEnv` beside an `apiKeyFile` | The binary refuses that pair at startup rather than ranking them, so rendering it is a pod that never serves. The variable at its shipped default is not a second choice — that is how the file form stays reachable. |
| any value still containing `REPLACE_ME` | A placeholder is not configuration. The dangerous case is not the one that fails: an identity of literally `REPLACE_ME` renders a gateway refused on every call while the chart reports success. |

A value written and left blank counts as missing, and so does one that is only
whitespace. A key with nothing after it in YAML is `nil`, not `""`, and `nil`
stringifies to `"<nil>"` — non-blank. A required-value check that did not
normalise first would read an absent value as configured and render the pod with
an empty flag, which is exactly the healthy-and-denying gateway these guards
exist for. Every value is therefore normalised before it is tested, and the
`-allowed-host` argument is built from the same template the guard counts, so
the chart cannot pass its own check and then render `" llm.example.test"` — an
authority matched exactly, with a space in it, matching nothing.

## Orchestrator probes: the health surface

The host-authority gate runs before routing and before anything else. That makes
the traffic port unusable as a probe target: a kubelet addresses a pod by its
runtime-assigned IP, which no static `-allowed-host` list can name, so every
probe there answers `421` and the pod never becomes ready.

`-health-address` opens a second listener for exactly this, and behaves as the
Explorer's does:

| Route | Meaning |
| --- | --- |
| `GET /healthz` | The process is up. Stays `200` throughout a drain. |
| `GET /readyz` | The gateway is serving. `503` before it serves and from the moment shutdown begins. |
| `GET /metrics` | Prometheus text: the gateway's refusals and failures by closed reason. See [Metrics](#metrics-what-to-alert-on). |

The split is what makes a rollout safe. Readiness drops **before** the listener
stops accepting, so the endpoints controller removes the pod from the Service
while it is still finishing in-flight requests. Liveness does not drop, because
a pod shedding traffic on purpose has not failed and restarting it would throw
away the graceful close.

The readiness probe is deliberately short — `periodSeconds: 3`,
`failureThreshold: 1`. A failing readiness probe is the only thing that takes
the pod out of the Service, so that interval *is* the window between the drain
starting and traffic stopping; a slower probe spends it answering requests the
gateway has already decided to stop serving.

There is no startup probe, unlike the Explorer. The Explorer needs one because
opening a large corpus and policy catalog takes time. The gateway opens nothing,
so a startup probe would only postpone the first readiness check.

The Service publishes the traffic port only. The probe surface exists for the
kubelet, which reaches the pod directly; publishing it would make an
unauthenticated endpoint routable in-cluster for no reason, and it is the one
endpoint on the pod that answers without passing through admission. A scraper
reaches it the way the kubelet does, by pod IP, through the
`prometheus.io/scrape`, `prometheus.io/port` (the health port) and
`prometheus.io/path: /metrics` annotations on the pod template — the same
convention as the effects gateway.

`validate-chart.sh` asserts all of this end to end rather than in parts — the
probes address the health port, `-health-address` is passed and names the port
the probes and the container agree on, the scrape annotations name `/metrics`
on that same port, `-listen` names the port the Service targets, the Service
does not publish the health port, and its selector actually matches the pod's
labels. Asserting only that the probes *name* a
health port once passed a chart whose probes addressed a port nothing was
listening on.

## Metrics: what to alert on

A gateway failing closed because the decision plane is unreachable is a total
outage for every caller behind it, and before #425 it looked, from every signal
the chart exposes, exactly like a policy tightening: both pass every probe, and
telling them apart meant reading the logs of every replica. `GET /metrics` on
the health listener makes the difference scrapeable.

The health port is unauthenticated, so what it carries is deliberately narrow:
counts of the gateway's own refusals and failures, each by a reason drawn from
a closed set fixed in the binary. Nothing that meters tenant workload or caller
behaviour — no total of calls, no count of allowed or forwarded calls, no bytes
or tokens, nothing in flight, no count of callers hanging up — and no label
derived from a request: no model, reference, compartment, policy, principal,
document or error text. Every series is present at zero from the first scrape.
A caller that hangs up mid-call is counted nowhere, so a client with a short
timeout cannot raise the outage signal. Workload and hang-up counts are a
follow-up for an authenticated surface.

| Family | `reason` | Meaning |
| --- | --- | --- |
| `shoal_llm_gateway_infrastructural_denials_total` | `plane_unreachable` | No answer from the plane: transport failure, timeout, or a 502/503/504 with no structured error body — what whatever fronts the plane answers when it is gone. |
| | `plane_credential_rejected` | The plane answered 401/403 to the **gateway's own** admission token. The caller was never adjudicated. |
| | `plane_reported_unavailable` | The plane itself answered a structured `503` (`code: unavailable`): it is up and refusing to admit right now. The whole-store rollout conditions — an unmigrated admission, an occupied reserved span — answer this, and an operator clears them on the Explorer, not in the network. |
| | `plane_error_status` | Any other non-2xx from the plane, including a 502/504 that carries the plane's own error code. |
| | `plane_answer_unusable` | A 200 the gateway cannot act on: an unknown outcome, an unusable token, or a token naming another claim or expiring inside the report window. |
| | `admission_credential_unavailable` | The gateway's **own** admission token could not be read, or was empty or held a line break. Nothing was sent to the plane. |
| | `grant_window_exhausted` | The grant arrived with too little lease left to make the call and still report it — a slow plane. |
| | `identity_unavailable` | The gateway could not generate an admission identity. Since Go 1.24 `crypto/rand` cannot fail this way, so this stays at zero; it is kept so the refusal path that exists is counted under its own name. |
| `shoal_llm_gateway_policy_refusals_total` | `denied` | The plane denied the call. A policy outcome, not an outage. |
| | `obligation_unsatisfiable` | The plane allowed the call with an obligation the gateway cannot meet. |
| `shoal_llm_gateway_upstream_failures_total` | `credential_unavailable` | A configured provider credential could not be read. |
| | `unreachable` | The provider could not be reached. |
| | `error_status` | The provider answered outside 2xx. This **includes provider `429`s**, so a rate-limited account shows here; the caller receives the provider's status and `Retry-After` unchanged. |
| | `response_truncated` | The provider's response stopped part-way. |
| `shoal_llm_gateway_report_failures_total` | the five `plane_*` reasons and `admission_credential_unavailable` | A report the plane did not acknowledge. Each leaves a grant outstanding on the plane, and this is the only signal of it outside the plane itself. |

What to alert on:

- **Fail-closed outage** — page on any sustained infrastructural denial:
  `sum by (reason) (rate(shoal_llm_gateway_infrastructural_denials_total[5m])) > 0`
  for a few minutes. Every one of these is a call that policy did not refuse
  and that did not happen. The reason says where to look: `plane_unreachable`
  is the Explorer or the network to it, `plane_reported_unavailable` is a
  condition on the Explorer an operator must clear, `plane_credential_rejected`
  and `admission_credential_unavailable` are the gateway's admission token
  (`llmGateway.admission.*`) — rejected by the plane, or not readable on the
  pod — and `grant_window_exhausted` is a plane too slow for the configured
  lease.
- **Unreported grants** — alert on
  `rate(shoal_llm_gateway_report_failures_total[5m]) > 0`. The call's effect
  happened or was refused, and the plane does not know which.
- **Provider trouble** — `rate(shoal_llm_gateway_upstream_failures_total[5m])`
  by reason, with `credential_unavailable` worth a page of its own: it is a
  mounted file the pod cannot read, and it refuses every admitted call.
  `error_status` moves with provider rate limiting (`429`) as well as provider
  errors, so alert on its rate against a threshold tuned to the account rather than on any nonzero value.
- **Policy change** — `shoal_llm_gateway_policy_refusals_total` is not an
  outage signal, and should not page. A step change in it with no matching
  infrastructural denials is what a policy tightening looks like; the reverse
  is an outage. That distinction is the point of the two families.

**Readiness does not follow these counters**, deliberately. A gateway failing
closed is doing what it was built to do, and every replica shares the same
plane: marking them not-ready would empty the Service at once, replacing a
structured, retryable `503 plane_unavailable` with a connection failure that
tells the caller nothing, and no reroute or restart brings the plane back.
`/readyz` stays `200`; the outage is an alert on the counter.

The caller-facing answer is unchanged and still distinguishes the two: a policy
denial is `403` `denied`, which a client must not retry, and every
infrastructural denial is the same fixed `503` `plane_unavailable`, which it
should. The reason is in the metric and the log, never in the response.

## Stateless, and not a singleton

A **Deployment**, where the Explorer is a StatefulSet, and that single
difference is the whole difference between the two planes.

The Explorer needs its rolling update to stop the old pod before starting the
new one, because two processes over one `ReadWriteOnce` state root is a
split-brain it has no protocol for — and `explorer.replicas` above 1 is refused
for the same reason. The gateway has no state root at all: the decision lives in
the Explorer, the admission token travels in the request it was granted for, and
nothing is retained between calls. So replicas are independent askers of one
decision plane, surging is correct, and there is no volume, no ordinal identity
and no governing headless Service to need a StatefulSet for.

The default is two replicas, and raising it is the expected direction. The
rollout is `maxUnavailable: 0`, `maxSurge: 1`: no old pod is removed until a
surge pod has passed readiness, because a gap in capacity here is an outage for
everything behind the gateway rather than a slow read. Replicas carry a soft
anti-affinity across nodes so a single node loss is not the whole plane —
*soft*, because a cluster with fewer nodes than replicas must still schedule,
and a Pending pod is as unavailable as a denied one.

Supplying `llmGateway.affinity` replaces that default outright rather than merging
with it. Merging two affinity trees would produce a scheduling constraint
neither the chart nor the operator wrote.

The pod has no writable path and no `emptyDir` to give one back. The gateway holds
no state root and must not spool payloads: prompt and completion content is
exactly what it is governing the egress of, and a scratch directory is where
that content would end up on a node's disk.

The credential volumes do not change that. Each is mounted `readOnly`, the root
filesystem stays read-only, and the only thing in them is the credential the
kubelet writes from outside the container. `validate-chart.sh` asserts that a
credential mount never arrives alongside a writable root filesystem.

## Secrets: two credentials, both required

The chart projects both from pre-existing Secrets. Neither is created for you.

```console
$ kubectl create secret generic shoal-admission-token --from-literal=token="$EXPLORER_BEARER"
$ kubectl create secret generic shoal-upstream-key --from-literal=api-key="$PROVIDER_KEY"
```

```yaml
llmGateway:
  admission:
    credentialSecretName: shoal-admission-token
    credentialSecretKey: token
  upstream:
    credentialSecretName: shoal-upstream-key
    credentialSecretKey: api-key
```

In this form the admission credential is unconditional — the chart requires a
Secret. The upstream credential is required for any non-loopback provider,
exactly as the Explorer's chat provider is, and is satisfied either by a Secret
or by an operator-supplied volume (below).

Neither of these rotates. Both are environment variables, and a container's
environment is fixed after start: changing either Secret leaves every running
gateway on the old value until the pod is replaced. Where that is not acceptable,
use a file.

## A rotating credential: the projected ServiceAccount token

A projected ServiceAccount token is a file the kubelet rewrites in place when it
rotates it. It never updates an environment variable, so with only the variable
form it could not be used at all — the admission credential had to be a
long-lived static Secret, which is the thing projected tokens exist to avoid.

### What the chart needs from you

```console
$ kubectl create serviceaccount shoal-llm-gateway
```

That is all: the account is not created by the chart, like the Secrets, and no
RBAC is needed for the token itself. The Explorer must be configured to accept
it — its OIDC authenticator has to trust the cluster as an issuer and include
the audience below in `explorer.auth.oidc.audiences` — and the descriptor the
gateway admits against has to be registered to the identity the token carries,
`system:serviceaccount:<namespace>:shoal-llm-gateway`.

### The values file, complete

```yaml
llmGateway:
  enabled: true
  image:
    repository: ghcr.io/YOUR_ORG/shoal-llm-gateway
    tag: TAG
  replicas: 2
  allowedHosts: [llm.internal.example.test]
  # The listener serves TLS by default (#424). A kubernetes.io/tls Secret, for
  # example a cert-manager Certificate's for llm.internal.example.test; it is
  # re-read on renewal without rolling the pod. Clients use https://.
  tls:
    secretName: shoal-llm-gateway-tls
  # The token's subject is this account. Required for the projected form, and
  # not created by the chart.
  serviceAccountName: shoal-llm-gateway
  admission:
    url: https://shoal-explorer.shoal.svc.cluster.local:8098
    # The file form. No credentialSecretName, and naming one here is refused:
    # it would be a credential the pod never opens.
    tokenFile: /var/run/secrets/shoal/token
    tokenFileSource: projected
    # Must be an audience the Explorer's authenticator accepts. Empty is not a
    # default — a token projected with no audience is issued for the cluster's
    # own API server, which the Explorer is not, so every admission request
    # would be a 401.
    tokenAudience: shoal
    # 600 is the API server's floor. 3600 is a reasonable lifetime; see below
    # for why no value above the floor can make the gateway read a stale token.
    tokenExpirationSeconds: 3600
    lease: 60s
  upstream:
    baseURL: https://api.example.test/v1
    credentialSecretName: shoal-upstream-key
    credentialSecretKey: api-key
    requestTimeout: 30s
  # Must exceed the lease by at least 5s, because the gateway drains for the
  # lease. The default is 75 against the default 60s lease.
  terminationGracePeriodSeconds: 75
  identity:
    agentID: Z292ZXJuZWQtcHJveHk
    agentGeneration: 1
    capability: chat.completions
    action: complete
    sourceID: c291cmNl
    policyID: cG9saWN5
```

What that renders, in the part that matters:

```yaml
spec:
  template:
    spec:
      serviceAccountName: shoal-llm-gateway
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        fsGroup: 65532            # not optional; see below
      containers:
        - name: shoal-llm-gateway
          args:
            - -admission-token-file=/var/run/secrets/shoal/token
            # and no -admission-token-env at all
          volumeMounts:
            - name: admission-token
              mountPath: /var/run/secrets/shoal     # the file's directory
              readOnly: true
      volumes:
        - name: admission-token
          projected:
            defaultMode: 0440     # not 0400; see below
            sources:
              - serviceAccountToken:
                  path: token     # the file's base name
                  audience: shoal
                  expirationSeconds: 3600
```

The mount is **derived from the path in the flag**: `mountPath` is its directory
and the projected `path` is its base name, so there is no second value to keep
in step with `-admission-token-file`. That is why the path has to be absolute
and has to name a file inside a directory other than `/` — a volume mounted at
`/` would replace the container's root filesystem, taking the binary with it.

### fsGroup and 0440 are one decision

The kubelet writes projected and Secret volumes owned by root, and this
container runs as uid 65532 with every capability dropped. At `0400` with no
`fsGroup`, the one process that needs the credential cannot open it — and the
failure is not a crash. The gateway starts, passes both probes, and fails every
admission request on an unreadable file, which from outside looks exactly like a
policy problem. `fsGroup: 65532` makes the mount group-owned by the group the
process runs as, and `0440` is then readable and writable by nobody.
`validate-chart.sh` asserts the pair rather than either half.

### Why the per-request read sees a fresh token

The gateway reads the file on every request, and that is what makes rotation work:
a value captured at startup would be the one that has since expired.

Two properties make it correct rather than racy.

**The rewrite is atomic.** The kubelet projects these volumes through an atomic
writer — it writes the new content into a fresh timestamped directory and then
swaps a `..data` symlink — so a reader opening the path gets either the whole
old token or the whole new one, never a half-written file.

**The rewrite happens well before the expiry.** The kubelet refreshes a
projected token when it is 80% of the way through its lifetime, so the margin
between the rewrite and the expiry is a fifth of `expirationSeconds`.

That margin is why `600` is the floor twice over. The API server rejects a
projection below 600 seconds as an invalid pod spec — which is the failure that
does not look like one: the Deployment is accepted and no pod is ever created
from it, so there is no pod log to read. And 600 is also where the margin, two
minutes, stays comfortably clear of the kubelet's own re-projection cadence of
about a minute.

So **no accepted value above the floor can make this gateway read a stale token**:
a larger `expirationSeconds` only widens the margin. The one way the gateway can
present an expired token is the kubelet failing to refresh at all — a node-level
problem — and the result is a 401 on every admission request, which denies every
call. Fail-closed, which is the only direction this component is allowed to
fail.

Longer than a day is accepted and not advised. The kubelet re-projects at 24
hours regardless, so extra validity buys no fewer rotations; it only widens the
window in which a leaked token is still accepted.

### The other two sources

`tokenFileSource` also takes:

**`secret`** — `admission.credentialSecretName` mounted as the token file
instead of projected into the environment. Worth having on its own rather than a
worse version of the variable form: the kubelet updates a mounted Secret's
contents in place, so rotating the Secret's value takes effect without rolling
the pod, which the variable form cannot do at all.

```yaml
admission:
  tokenFile: /var/run/secrets/shoal/token
  tokenFileSource: secret
  credentialSecretName: shoal-admission-token
  credentialSecretKey: token
```

**`volume`** — `admission.tokenVolume`, supplied verbatim, for a CSI driver the
chart does not model (SPIFFE, Vault Agent, secrets-store-csi). Give the volume
*source* only; the chart owns the name, so the volume and its mount cannot name
different things. Set an explicit mode and item path, or the credential lands
wherever the driver's defaults put it:

```yaml
admission:
  tokenFile: /var/run/spiffe/token
  tokenFileSource: volume
  tokenVolume:
    csi:
      driver: csi.spiffe.io
      readOnly: true
```

### The upstream credential takes a file too

`-upstream-api-key-file`, with the same reasoning and a smaller set of sources:

```yaml
upstream:
  baseURL: https://api.example.test/v1
  apiKeyFile: /etc/shoal/upstream/api-key
  apiKeyFileSource: secret        # or volume
  credentialSecretName: shoal-upstream-key
  credentialSecretKey: api-key
```

There is no `projected` source here, because a projected ServiceAccount token is
a cluster-issued credential for a cluster verifier and a third-party provider is
neither. Where a provider does accept one — workload identity federation — the
`volume` source expresses that projection verbatim, audience and expiry
included, which is strictly more than a second copy of those two keys could say.

The two credential files must sit in **different directories**. Each is mounted
as its own volume at its own file's directory, and a pod cannot mount two
volumes at one path: the API server refuses the pod spec, so the Deployment is
created and no pod is ever made from it.

## Upgrading

**To a TLS listener (#424).** The chart now serves the gateway's listener over
TLS by default, and an existing values file stops rendering with
`llmGateway.tls.secretName is required`. Nothing is rolled until you choose
one of these:

- **Serve TLS** (recommended). Create a `kubernetes.io/tls` Secret for the name
  clients dial, for example with a cert-manager `Certificate`, and set
  `llmGateway.tls.secretName`. Then move every caller's base URL from `http://`
  to `https://` and make sure it trusts the issuer. The Service and container
  ports are renamed from `http` to `https`, so update anything that selects the
  port by name (a mesh `DestinationRule`, a `ServiceMonitor`, an `Ingress`
  backend, which must now speak HTTPS to the pod). Callers still on `http://`
  get `400 Client sent an HTTP request to an HTTPS server` and nothing is
  admitted for them. To avoid a gap, run the TLS gateway under a separate
  release until callers have moved.
- **Keep plaintext behind an mTLS mesh.** Set `llmGateway.tls.enabled: false`.
  The chart renders `-allow-plaintext-listener=true`, which the binary now needs
  for a `0.0.0.0` bind. Nothing else changes for callers. A non-ClusterIP
  Service additionally needs `llmGateway.service.allowPlaintext: true`.

Outside the chart, a gateway started with a non-loopback `-listen` and neither
the TLS flags nor `-allow-plaintext-listener` now exits at startup with
`-listen ... is not a loopback address`. The default `127.0.0.1:8100` is
unaffected.

**From the LLM proxy.** This component was `shoal-llm-proxy`, under the chart
key `llmProxy`. Moving to the gateway changes four things, and the chart can
catch only the first:

- Move the values from `llmProxy` to `llmGateway`, and `values-llm-proxy.yaml`
  overlays to `values-llm-gateway.yaml`, then delete the `llmProxy` block. The
  chart refuses to render while `llmProxy` is enabled, or carries settings
  beside an enabled `llmGateway` without saying `enabled: false`. A null or
  empty `llmProxy`, or one whose `enabled` is literally `false` (as in a full
  dump of the old defaults), is ignored.
- Rebuild the image. The Deployment runs `/usr/local/bin/shoal-llm-gateway`; an
  image built before the rename contains only `shoal-llm-proxy` and the pod
  never starts.
- Repoint callers. The Deployment, Service and PodDisruptionBudget are named
  from the chart's full name: `<fullname>-llm-gateway`, which is
  `<release>-shoal-llm-gateway` unless the release name already contains
  `shoal` (`helm install shoal` gives `shoal-llm-gateway`). Helm deletes the old
  objects and creates new ones in the same upgrade, so the old Service stops
  answering at that moment, before the new pods are necessarily ready, and
  `maxUnavailable: 0` does not help because it applies within one Deployment.
  `--wait` does not close the gap, because the old objects are deleted before
  Helm waits; it only makes the upgrade block until the new pods are ready. For
  no gap, run the gateway under a separate release until callers have moved,
  then remove the old one.
- Update anything outside the chart that selects the pods. Both selector labels
  changed: `app.kubernetes.io/name` from `shoal-llm-proxy` to
  `shoal-llm-gateway`, and `app.kubernetes.io/component` from `llm-proxy` to
  `llm-gateway`. A NetworkPolicy or monitor written against either old value
  silently stops matching.

Two things to know before any upgrade.

**`identity.agentGeneration` pins a descriptor generation.** Re-registering the
descriptor bumps it, and this value has to be bumped with it in the same change.
A stale generation is not a startup failure: the gateway comes up, passes its
probes, and is refused on every admission request.

**Changing any setting rolls the pods, by design.** Every setting is an
argument, so there is no case where the chart reports a new posture while a
running process is on the old one. `maxUnavailable: 0` means the rollout keeps
every replica serving while it happens.

**The lease is also the drain window, so it is also the rollout's worst case.**
On SIGTERM the gateway drains for `-lease`, because the lease is the bound on an
admitted call's entire lifetime including the report that closes it — a
ten-second drain abandoned in-flight calls whose egress had already happened and
whose grant had already been spent, which made every rolling update a producer
of unreported grants. `terminationGracePeriodSeconds` must exceed the lease by
at least five seconds for the same reason, and the chart refuses the pair when
it does not.

The cost is explicit rather than hidden. With `maxUnavailable: 0` and
`maxSurge: 1` the controller will not exceed `replicas + 1` pods, so it waits
for a terminating pod to go away before surging the next one: a rollout
serializes on termination. At a five-minute lease and ten replicas that is a
large number if every pod takes its whole window.

In practice it does not, and the distinction matters when choosing a lease.
`Shutdown` returns as soon as the last connection closes, so the drain window is
a **ceiling, not a delay** — a pod with nothing in flight exits immediately, and
a pod with a call in flight waits for that call, which `-request-timeout` already
bounds. So the expected termination time is governed by the request timeout, and
the lease only sets the worst case the kubelet must be prepared to allow. The
knob for faster rollouts is therefore `upstream.requestTimeout`; the lease
follows it, because the chart requires the lease to exceed it by the report
window.

The chart deliberately does **not** cap the lease below the fleet's own
`MaxActionClaimTTL` to make rollouts faster. The lease is a correctness bound —
how long a grant stays reportable, and therefore the longest call this gateway
will admit — and capping it for a deployment-convenience reason would silently
shorten what callers can do, which is the kind of coupling that produces a
denial nobody can explain.

## What the chart cannot express yet

Gaps in the flag contract, recorded rather than worked around.

- **The listener does not verify clients.** It serves TLS but asks for no
  client certificate. Callers are not authenticated by the transport, which is
  why the Host gate and admission exist. There is no `-tls-client-ca`, unlike
  `shoal-tserver`.
- **No unix-socket listener.** Plaintext is allowed on loopback only, and
  `-listen` is always TCP. A sidecar that wants a socket has to use loopback.
- **No quiesce delay.** `readFleet` has `-quiesce-delay`; the gateway does not.
  Readiness drops and the listener stops accepting in the same breath, so
  requests arriving in the few seconds before the endpoints controller removes
  the pod are refused at the connection rather than answered. Nothing is
  admitted in that window, so no grant is stranded by it — the caller sees a
  connection error instead of a 503, which is worse to read and not worse to
  recover from. The drain itself is no longer unbounded or guessed at: it is
  `-lease`, and `terminationGracePeriodSeconds` is required to exceed it.
- **No report timeout.** `-request-timeout` bounds the upstream call. The report
  that follows it has no bound of its own, and an admission that is never
  reported is the outstanding case `docs/admission-seam.md` describes.
- **A withhold obligation is enforced only over attributed content.** Without
  `shoal_attribution` it is refused, as strong as a denial; with it, only what
  the caller attributed is withheld, plus a 96-byte verbatim residue check.
  Attribution is the caller's statement and is not authenticated, so this is no
  defence against a caller that means to send the material. See
  [Withhold obligations and attribution](#withhold-obligations-and-attribution)
  for the guarantee and its gaps (#426).
- **No ServiceAccount is created for the projected token form.** The chart
  references `llmGateway.serviceAccountName` and does not create the account or
  any RBAC, on the same principle as the Secrets. An operator wiring the
  Kubernetes-native credential still has one object to create by hand.
- **The chart cannot check that the Explorer accepts the token it projects.**
  `tokenAudience` has to match an audience the Explorer's authenticator accepts
  and the cluster has to be a trusted issuer there, and neither is visible from
  the gateway's own values — the Explorer may be in another cluster entirely. A
  mismatch is a 401 on every admission request, with a healthy pod.
