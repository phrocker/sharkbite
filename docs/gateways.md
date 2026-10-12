# Three gateways

Design note; roadmap #445. Shoal governs agents at three boundaries, each
reached by a separate gateway, built in this order:

| gateway | governs | principal | state |
| --- | --- | --- | --- |
| LLM gateway | what an agent *discloses* to a model | agent or application | built: `cmd/shoal-llm-gateway`, `docs/llm-gateway-deploy.md` |
| Effects gateway | what an agent *does* to an external system | agent, through dispatch | built (HTTP): `cmd/shoal-gateway`, `docs/effects-gateway-deploy.md`; design in #391 and `docs/gateway-proxy-design.md` |
| Session gateway | what Shoal *learns* from what humans do | human operator | proposed: an extension under #401 (#447–#449) |

The first two enforce. The third improves what they enforce: it turns human
operational activity into attributed observations that the decision work in
#401 can evaluate and, under #419's rules, learn from. The order is priority,
not dependency: the session gateway does not need the effects gateway, and the
dependencies that do exist are listed under "Order".

## Why three, and why separate

`docs/gateway-proxy-design.md` ("The thing that makes this different") already
separates the first two by consequence. A refused disclosure means nothing left
the host. An allowed effect cannot be withheld, compensated or recalled. The
session gateway differs from both again:

| | LLM gateway | Effects gateway | Session gateway |
| --- | --- | --- | --- |
| unit | one request | one action, at most once | a long-lived stream yielding many observations |
| inbound listener | HTTP | none on Path A if its probes are `exec`: the worker pulls | SSH; an HTTPS/WebSocket Guacamole tunnel for RDP, with `guacd` as an internal sidecar |
| writes to Shoal | admission and report | dispatch completion | observations, shadow predictions, outcomes |
| sensitive state | provider credential | one target credential | target credentials and raw session recordings |

Two rows settle that these are separate binaries, not modes of one:

- **Inbound exposure.** A Path A effects worker pulls its work, and with `exec`
  probes it accepts no connections at all (`docs/gateway-proxy-design.md`,
  "Kubernetes reference"). Even with a health listener, putting an interactive
  SSH server or an RDP tunnel endpoint in the pod that holds an irreversible-effect credential
  gives every operator-facing connection a route to that credential.
- **Recordings.** Raw sessions carry typed secrets and customer data, and need
  their own storage, retention and replay access. Nothing about an effects
  worker should hold them.

This does not contradict `docs/sentrius-integration.md` §6 ("What must not move
into Shoal"). §6 keeps session *transport* out of Shoal's dispatch and
execution: a session is the wrong shape for an action, and SSH and RDP proxies
effect change by definition. The session gateway is such a proxy: it relays
operator sessions and holds target credentials. It is also a separate binary
that reports what it observes to Shoal through public contracts. The effects are
the operator's, outside Shoal's dispatch and executor; nothing in Shoal's
executor performs a session, and the gateway enforces nothing until shadow
results justify it.

All three follow the deployment rule in `docs/gateway-proxy-design.md` ("The unit
of deployment is the operational surface"): one binary, deployed once per
surface, each with its own ServiceAccount, Secret and egress policy. A session
gateway for a Linux fleet and one for Windows jump hosts hold only their own
host credentials.

## Core and extensions

The LLM gateway and the HTTP effects gateway are core. Everything
protocol-specific beyond HTTP is an extension.

**Core owns protocol-neutral contracts:**

- Admission and report (`docs/admission-seam.md`), effect declarations and
  executor ceilings (`docs/sentrius-integration.md` §6).
- Human approval as an admission outcome (see "What comes from ATPL").
- Pictures, decisions and attributed outcomes (#402, #403, #418).
- Registration of an external collector as an evidence source with declared
  authority, and recorded collector and extractor identity with extraction
  confidence (#419).
- Runtime attestation reported by a gateway, collector or executor, recorded as
  provenance that policy may require.
- A versioned client SDK that all three gateways use for the above.

Admission and report are published as `pkg/admission/api`: the exact wire
types of `/api/v1/admission/*`, the outcome and effect constants, the limits,
and a client (`sdk.New(...).Admission()`, or `api.NewClient` directly where the
plane sits behind a path prefix). `cmd/shoal-llm-gateway` speaks the seam only
through it. Golden fixtures in `pkg/admission/api/testdata/wire` pin the bytes
both the handler and the gateway put on the wire. The client's response decoder
refuses duplicate and case-variant keys and trailing data but ignores unknown
fields, so a deployed gateway keeps working against a newer plane.

The worker-facing dispatch routes are published the same way as
`pkg/dispatch/api`: the wire types of pull, claim, extend, complete, ambiguity,
status and descriptor resolve, the limits, and a client
(`sdk.New(...).Dispatch()`, or `api.NewClient` behind a path prefix) that
carries the rules for a lost answer — a claim that may have committed is a
re-pull, an extension binds on the fence alone, and a completion binds on the
claim fence and resends until a record or an honest "indeterminate" settles
it. `internal/effectsgateway` is a thin adapter over it and links no
`pkg/explorer` package. Golden fixtures in `pkg/dispatch/api/testdata/wire` pin
both sides.

**Extensions use only that public surface:**

- The session gateway: an SSH adapter, then an RDP adapter.
- Versioned extractors: commands from terminal streams; input events, frames
  and UI Automation state from RDP sessions.
- An `effects.ssh` adapter for the effects gateway, so an agent's single SSH
  command is one Path A action.

Extensions live under `extensions/`, each with its own `go.mod` listed in
`go.work`, the pattern `wal-quorum-sidecar` already uses. They ship their own images and are not
rendered by the core chart unless enabled. Core release gates do not cover them.
They stay in this repository while the contracts settle, so a contract change and
the extension that exposed it can land together.

The boundary follows the rule #402 states for decision contracts, "core imports
do not depend on GitHub", and goes further by testing it: a new test (#446)
fails if any package in the root module imports from `extensions/`, or if an
extension imports anything other than the SDK and public `pkg/` contracts.

**What the rule costs.** An extension cannot reach into internals, so core
contracts must cover everything an extension needs. The first extension will
find gaps. Close them in core; a workaround inside an extension is a fork.

**What it buys.** Contracts general enough for Shoal's own session gateway are
general enough for someone else's collector: a Teleport audit-event importer, a
Boundary plugin, an asciinema importer. Their observations enter under the same
#419 authority rules as Shoal's own.

## The session gateway

Agents do not need interactive terminals; one command with one bounded result is
an effects-gateway action. The session gateway exists for humans, and its first
job is observation, not enforcement.

```
transport adapter  ->  raw recording  ->  versioned extractors  ->  observations
   (ssh | rdp)          (retained)         (identity, confidence)        |
                                                                         v
                         outcome attribution  <-  shadow decisions  <----+
                       (incident, TSG step,      (#409 method, no
                        rollback, resolution)     enforcement)
```

**Raw recordings are retained and extraction is replayable.** Extraction of
terminal streams is imperfect (line editing, completion, heredocs, full-screen
programs, multiplexers), and extraction of screens is worse. Retaining the raw
stream and pinning each extractor's identity, as #402 pins predictor identity,
means a better extractor rebuilds datasets from sessions already paid for.

**Observation is not a label.** What an operator did is an observed action, not
ground truth (#419). Labels come from attributed outcomes. A dataset built from
"what people did" teaches that whatever was done is allowed.

**Observation needs no claim.** A recorded session is not a dispatched action,
so `MaxActionClaimTTL` (`pkg/explorer/fleet/dispatch_model.go`) does not bound
it. Per-command enforcement, when it comes, is a short admission per command.

**RDP is paid for deliberately.** Screen capture is expensive and noisy, and it
is the only complete record of GUI operations. The costs are contained, not
avoided:

- the Guacamole stream supplies key, pointer and clipboard events directly;
- frames are sampled around input bursts and changed regions, not on a timer;
- vision extraction runs offline, in batches, on local models where they suffice;
- UI Automation, PowerShell transcription and event logs, where the target
  allows an agent, are higher-confidence evidence alongside pixels;
- cost is reported per eligible labelled example, by protocol, as #409 reports
  total cost.

`guacd` is a C daemon with a history of protocol-parsing vulnerabilities. It
runs per session or in a strict sandbox, with egress to its target and the
gateway only.

**Recordings and disclosure.** Terminal and screen recordings will contain
secrets. Text can be redacted before dataset admission; pixels mostly cannot, so
frame datasets exclude rather than redact. Recordings are encrypted at rest and
replay is an authorized read.

## What comes from ATPL

The Agent Trust Policy Language (`github.com/SentriusLLC/atpl`, Apache-2.0) is
a JSON Schema for per-agent trust policy: identity, provenance, runtime,
behavior, capabilities, a weighted trust score and outcomes on success, marginal
score and failure. It has no evaluator. Most of it already has a stronger
counterpart here: fleet capabilities with declared effects and narrow-only
delegation, `auth.Decision`, #419 source authority and #403 outcome receipts.
Three parts do not, and are adopted:

- **Approval as an admission outcome.** ATPL's `on_marginal: require_ztat`.
  Slice 1 is in `docs/approval.md`: a registered action can require approval,
  and a held request becomes work only once an independent approver has
  approved it.
  Admission today refuses, allows, or allows with obligations, and the only
  obligation is `Withhold` (`pkg/explorer/fleet/admission.go`). A held
  admission, granted by an approver in a role distinct from the requester,
  bounded in time and recorded on the `ActionRecord`, is what the effects
  gateway needs before it enforces anything risky. Approvals and refusals
  attached to exact context are also adjudications for #401, subject to #419's
  role separation. A ZTAT remains an approval record, not a grant
  (`docs/sentrius-integration.md` §3).
- **Runtime attestation.** ATPL's `runtime` section. Source attestation
  references exist in `pkg/decision`, but nothing records whether a gateway or
  executor runs attested code. It becomes provenance that
  policy may require for executors bound for `EffectMutatesExternal`.
- **A declarative policy format.** Policy is today spread across descriptor
  registration, chart route tables and `auth` configuration. A versioned file
  format that compiles to Shoal's primitives (capabilities, effects, scopes,
  obligations, approval rules) makes policy reviewable and diffable. It keeps
  the ATPL name; it does not keep ATPL's schema.

Not adopted: `trust_score`. A hand-weighted sum that gates admission is the
uncalibrated authority #419 forbids: "model scores cannot assign policy
authority". #429 holds the same line for ranking, where low priority never
grants permission to skip review. How far to
trust an agent is a typed decision, evaluated in shadow against attributed
outcomes and promoted explicitly. Fixed `behavior` thresholds are dropped for
the same reason.

## Order

1. **LLM gateway.** Built. The listener serves TLS, or plaintext on loopback
   only (#424). Infrastructural denials are counted on the health
   listener's `/metrics` (#425). Withhold obligations apply to caller-attributed
   content (#426, `docs/llm-gateway-deploy.md`).
2. **Effects gateway, HTTP.** Built (#391): `cmd/shoal-gateway`, its image
   and the chart's `effectsGateways:`, tested end to end as a binary. Every
   blocker is closed: #435 (worker never received input), #436 (no
   external-effect executor ceiling), #437 (claimant had to be the enqueuer),
   #438 and #484 (nowhere to record a lost-fence ambiguity), #430 (claims
   could not be extended), #480 (nothing could hold `OperationExecute`) and
   #486 (a heartbeat moved the descriptor generation). Open, and not
   blocking: #363 (reaping claims a dead gateway left), #633 (a live agent
   briefly reads as not found under concurrent writes), #638 (image CI hits
   Docker Hub's rate limit) and #630 (declassification is not audited).
   Approval as an admission outcome lands before the gateway enforces any
   effect that requires it.
3. **Core extension contracts.** Collector registration and authority, extractor
   identity, runtime attestation, SDK, import boundary test. Rides on #403, #418
   and #419.
4. **Session gateway, SSH.** Subject to the #421 open decision below, and
   tracked with it in #447. Shadow only. Begins emitting observations only once
   step 3 and #418 exist. Transport and recording may be built before those,
   but not before the #421 decision.
5. **Session gateway, RDP.** Same pipeline, `guacd` adapter.
6. **Extractors.** Commands first, then GUI events, vision and UI Automation.
7. **`effects.ssh`.** After #391; needs #430 for long commands.

Enforcement in the session gateway is not on this list. It follows only after
shadow results justify it, under the same gate #409 sets for exclusion.

## Decided

- **Naming.** The LLM gateway was `shoal-llm-proxy`. Its binary, image, chart
  key (`llmGateway`) and Kubernetes resources are renamed to match (#454).
- **ATPL.** The declarative policy format (#452) keeps the ATPL name.

## Open decisions

- **Before building the session gateway.** Whether imported recordings from existing
  tools, admitted as low-authority evidence, should first show that session data
  improves a typed decision in #421, with "it does not" an acceptable result.
- **Repository.** When, if ever, extensions move to their own repositories.
- **ATPL repository.** Whether it is archived with a pointer here once the
  format exists.
- **Consent and retention.** Who may be recorded, for how long, and who may
  replay. This is a deployment policy Shoal must make expressible, not one it
  sets.
