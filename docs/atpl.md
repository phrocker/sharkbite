# ATPL policy files

`pkg/atpl` compiles reviewable, versioned policy files to fleet registry
registrations, and `shoalctl policy` reconciles them with a live registry. This
is the first slice of #452. Design context: `docs/gateways.md`, "What comes from
ATPL".

The format is derived from the Agent Trust Policy Language
(`github.com/SentriusLLC/atpl`, Apache-2.0). It keeps the name and the idea,
policy reviewed as a diff and reproduced from source, but not ATPL's schema.
Every file carries that credit in its required `origin` field.

## Format

A policy is a directory of `*.atpl.json` files, read non-recursively. Each file
is one JSON object:

```json
{
  "atpl": "shoal.atpl/v1",
  "origin": "Derived from the Agent Trust Policy Language, github.com/SentriusLLC/atpl (Apache-2.0)",
  "executors": [
    {"ref": "remote-exec", "max_effects": ["external", "reads-corpus"], "min_effects": []}
  ],
  "agents": [
    {
      "id": "planner",
      "authorization_domain": "domain",
      "scopes": [{"source_id": "source-a", "policy_id": "policy"}],
      "executor_ref": "remote-exec",
      "lease_ttl": "12h",
      "capabilities": [
        {"name": "search", "actions": [
          {"name": "query", "effects": ["reads-corpus"],
           "input_schema": {"type": "object"}, "output_schema": {"type": "object"}}
        ]},
        {"name": "ops", "actions": [
          {"name": "deploy", "effects": ["external"], "approval": {"required": true},
           "input_schema": {"type": "object"}, "output_schema": {"type": "object"}}
        ]}
      ]
    }
  ]
}
```

- **Agents** compile one-to-one to `fleet.Spec` (`pkg/explorer/fleet/model.go`).
  IDs, the authorization domain and scope identities are UTF-8 strings in the
  file and the registry's opaque bytes unchanged on the wire.
- **Effects** use the registry's wire spellings: `reads-corpus`,
  `egresses-content`, `external`. Omitted means the empty set.
- **`approval: {"required": true}`** on an action compiles to
  `fleet.Action.RequiresApproval` (#451, `docs/approval.md`): every new
  request for the action is held for a human decision. It is accepted only on
  an action (`agents[].capabilities[].actions[]`); anywhere else `approval` is
  refused by name. The object is exactly `{"required": true}`. Omitting
  `approval` means not required, and that is the only spelling for it:
  `{"required": false}` is refused rather than read as absent, so no file
  carries a line that reads as switching a requirement off, and every meaning
  has one form for the digest and for review. `{}`, `null`, a bare boolean,
  any other key and case-folded keys (`Approval`, `Required`) are refused, as
  everywhere in the format.
- **`attestation: {"required": true}`** on an action compiles to
  `fleet.Action.RequiresAttestation` (#446, `docs/executor-attestation.md`):
  a claim on the action (dispatch claim, extension, or the admission grant)
  requires the claimant to hold a current verified executor attestation that
  covers the lease. It has exactly approval's strictness and refusals: only on
  an action, exactly `{"required": true}`, `{"required": false}`, `{}`, `null`,
  a bare boolean, any other key and case-folded keys (`Attestation`,
  `Required`) refused. It is refused on an action whose effects lack
  `external` (at `...actions[name=X].attestation`), as the registry refuses
  it. Inheritance carries it; an inherited action may add it; a delegated
  action declared in full without it under a parent that requires it is
  refused at `...actions[name=X].attestation`. Whether the host has a trust
  root for the executor reference is host configuration the file cannot see;
  the registry checks it at apply.
- **`inherit: true`** on an action copies the same-named action of the parent's
  same-named capability, schemas, effects and approval requirement exactly. It
  is refused if the parent has no such action, or if the action also declares
  its own effects or schemas. It may carry `approval: {"required": true}` to
  add the requirement to what it inherits, which narrows. A delegated action
  can add approval but never drop its parent's, as the registry's
  `capabilitiesSubset` requires: a child that declares the action in full
  without `approval` under a parent whose action requires it is refused at
  `...actions[name=X].approval`. A parity test runs the same cases through
  `fleet.Service.Register`.
- **`lease_ttl`** is a Go duration in (0, 23h55m]. Compile adds it to one shared
  compile time, so a child and its parent compare TTLs exactly as the registry
  compares absolute leases. Leases are absolute and at most 24h ahead
  (`Spec.canonical`), which is why the file holds a TTL. The 5 minutes below
  the registry's bound (`SkewMargin`) absorb a client clock running ahead of
  the server's: apply computes the lease on the client, the registry checks it
  on the server.
- **Scopes** bound which actions an agent may be asked to perform, and — for
  an executor that says so — what one may reach while performing it. The
  second half is not free: `fleet.Invocation` carries the action's source,
  policy and object, and an executor is only taken to honour them if it
  implements `fleet.RetrievalConfiner`. One that does not is refused at
  invocation when the invoking principal may retrieve beyond the action's
  scope, rather than run and record evidence from outside it (#370). Where
  the principal can reach no more than the scope — which is how
  `cmd/shoal-explore-web` mints fleet principals — the bound constrains
  nothing and nothing is refused. Scoped retrieval itself is not implemented:
  `retrieval.Scope` is document and node identifiers and cannot express a
  source/policy bound, so a scoped reasoning agent currently refuses rather
  than reaching wider.
- **Executors** assert what the host binds a reference to. Ceilings and floors
  are host Go objects with no read API (`executorCeiling`, `executorFloor`,
  `cmd/shoal-explore-web/fleet.go`), so the file declares them and compile
  checks every action against them. The server stays authoritative and refuses
  on its own binding. `min_effects` must be within `max_effects`.
- **Declaring nothing** is refused on an action whose executor reference has a
  ceiling permitting `external` or `egresses-content` and an empty
  `min_effects` — the dispatch-only shape, where the host has said work may
  reach outside and not said when. Any one class satisfies it, `reads-corpus`
  included: the rule does not require an external class, because an action on
  such a reference may legitimately reach outside nothing, and forcing the
  claim would make `effect_possible` true where it should be false. What is
  refused is silence (#510, #514). Compile and `Register` refuse it alike;
  already-stored descriptors are not rewritten and keep resolving, so an
  operator meets this in a `plan`, not mid-flight. See the migration note in
  `shoal-explore-web-deploy.md`.

Strictness, in `Decode`: UTF-8 only; `\u` escapes of unpaired UTF-16
surrogates refused (they decode to U+FFFD, so two spellings would read as one);
one top-level object; duplicate keys refused at any depth, schemas included;
trailing data refused; the version gate before any other field; every unknown
field refused. Fields ATPL defines that this version does not compile are
refused by name with the reason:

| Field | Refusal |
| --- | --- |
| `approval` (anywhere but an action) | declared per action only |
| `obligations` | not declared in policy: admission obligations are computed per request; deferred |
| `attestation` (anywhere but an action) | declared per action only |
| `runtime` | requires a later ATPL version (runtime attestation, #446) |
| `trust_score` | not part of ATPL in Shoal; trust is a typed decision |
| `behavior` | not part of ATPL in Shoal; fixed thresholds are not gates |

Bounds: at most 4096 agents and 1024 executors per policy; at most 4097 files
(one per agent plus executors, the layout export writes, so a later export
diffs agent by agent); at most 4 MiB per file and 256 MiB per policy directory.
4 MiB is what any one agent the registry accepts can need: the registry bounds
an agent at `fleet.MaxDescriptorBytes` of its own JSON, where byte fields are
base64, and here those fields are strings that escape to at most 6 bytes per
byte. Schemas are written compact (`Encode`), so indentation does not grow
them.

## Validation

Compile refuses anything Register would refuse, before apply, with the file and
path: `agents.atpl.json: agents[id=searcher].lease_ttl: 13h0m0s outlasts parent
agents[id=planner]'s lease_ttl 12h0m0s`. It calls the registry's own validators
through thin wrappers (`pkg/explorer/fleet/policy_export.go`) one action at a
time, so a refusal names the action, then once over the whole spec. Delegation
conditions (`service.go`, the delegated-agent check) are re-composed per item:
same domain, scopes a subset, every action present in the parent with identical
schemas and no wider effects, lease no later. A parity test runs every refusal
fixture through `fleet.Service.Register` as well.

A parent must be declared in the policy or, under `plan` and `apply`, live in
the registry. Offline `compile` refuses an undeclared parent. A live parent is
checked the way Register's `activeChain` checks it: compile walks the live
registrations to the root, refusing an ancestor that is not visible, revoked or
expired, a link that no longer narrows its parent, and a chain that would reach
`fleet.MaxDelegationDepth` (64 registrations) counting live ancestors and the
policy's own.

Compile accepts documents built in code as well as decoded ones, so it refuses
every string that is not UTF-8 itself: encoding/json would otherwise replace
the invalid bytes and two different policies would share a digest.

An executor reference must pass `executorref.ValidExecutorRef`
(`pkg/executorref`, #391). Every place that accepts one applies this same rule,
and parity tests hold them all to the same verdict:
- fleet registration;
- the ATPL compiler (an `executors[].ref` and an agent's `executor_ref`);
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

## Identity

`Policy.Digest` is `atpl:policy:v1:<sha256>` over the Go `encoding/json` form of
the version, executors and every registration without its absolute lease but
with its TTL, as `pkg/decision` identities are computed. The same files give the
same digest at any time and in any file, list or key order. `policy compile`
prints those exact bytes. An action that requires approval carries
`"approval":{"required":true}` in them, and one that requires attestation
`"attestation":{"required":true}`; every other action is encoded as it was
before either could be declared, so a policy without them keeps its
digest (a golden test pins it), while two policies that differ only in a
requirement never share one.

## Commands

```
shoalctl policy compile DIR
shoalctl policy plan DIR -endpoint URL -token-file F
shoalctl policy apply DIR -endpoint URL -token-file F -plan-digest D
shoalctl policy export -out DIR -endpoint URL -token-file F [-executors manifest.atpl.json]
```

`compile` is offline: it prints the policy digest and canonical JSON, or exits
non-zero with the refusal.

`plan` reads the registry through the list route (`POST
/api/v1/fleet/agents/resolve`, `pkg/explorer/webapi/fleet_registry.go`), 16 per
page, compiles against it and prints the registry, then one line per agent:
`+` create, `~` narrow, `*` executor change, `=` unchanged, `!` refused, `?`
unmanaged, with field-level `+`/`-`/`~` lines beneath. An executor change is an
update the registry accepts like a narrowing, but it changes what runs the
agent's actions, so it is marked apart. It exits non-zero if anything is
refused. Refusals are: widening a live agent (the registry only narrows,
`service.go`), moving it to another parent, and a write that would leave a
delegation link exceeding its parent at any point during or after apply,
including a live child the plan does not rewrite that would stop resolving.
Leases are not compared; heartbeats maintain them, and apply does not renew
them.

Approval is compared per action. A policy that requires approval on an action
the live agent holds without it plans as a narrowing (`+ ...approval:
required`): the registry accepts it as an update, because adding the
requirement narrows authority, and the new generation stops records queued
before it from resolving (`docs/approval.md`). A policy that omits approval on
an action that requires it live plans as `refused-widening` (`-
...approval`): the registry refuses dropping it, and so does plan. Adding a
requirement to a parent that a live child this plan does not rewrite lacks is
`refused-delegation`, since the child would stop resolving. Apply orders an
approval change like any other narrowing. The `refused-approval` kind of
#489, which refused any managed agent with a live approval requirement because
the format could not express it, no longer exists.

Attestation is compared per action in exactly the same way. Adding it plans as
a narrowing (`+ ...attestation: required`), omitting it on an action that
requires it live plans as `refused-widening` (`- ...attestation`), and adding
it to a parent whose live child this plan does not rewrite is
`refused-delegation`.

**Flipping attestation on is a generation change.** Registering the
requirement moves the agent's generation, and a dispatch record names the
generation it was enqueued against, so records queued or claimed under the old
generation stop resolving — the same stranding a heartbeat causes today
(#486). Until #486 lands, flip with no live claims. Keep the two apart when
reasoning about it: the stranding is the heartbeat/generation issue, not
attestation. The requirement itself affects only *new* claims and extensions;
a live claim is never re-checked, and an attestation cannot lapse inside a
claim it covers.

The plan digest (`atpl:plan:v2:`) binds the policy digest, the normalized
endpoint (lower-case scheme and host, default port and trailing slash dropped),
each managed agent's kind and `ContentDigest` of its live registration, and the
IDs of unmanaged agents, and the write order below, two-step writes included.
`ContentDigest` covers parent, domain, scopes, executor
and capabilities, approval and attestation requirements included (each
appended only for an action that requires it, so other registrations keep
their earlier digests),
and leaves out generation, subject, actor, lease and update
time, which heartbeats move. A requirement added or removed between plan and
apply therefore invalidates the reviewed plan, and stops apply's retry. A heartbeat between plan and apply therefore does
not invalidate a reviewed plan, unless it changes the write order; a plan
reviewed against one registry does not apply to another. The write order
depends on leases, so on time and heartbeats: a plan reviewed long before apply
can order differently by then, and apply refuses it rather than write an order
nobody reviewed.

`apply` recomputes the plan and refuses unless its digest equals
`-plan-digest` and nothing is refused. It then registers with reason code
`atpl-apply` and the policy digest as reason detail, in an order where every
intermediate state is one the registry accepts and resolves
(`pkg/atpl/order.go`). Creates come last, parents first. Updates are ordered
for each child updated together with its parent:

1. the child's new registration, lease included, fits the parent's live one:
   the child first;
2. else the child's live registration fits the parent's new one: the parent
   first;
3. else only the lease prevents (1), because a narrowed child always fits its
   parent's content: the child is written twice, first with its lease clamped
   to the parent's live lease, then, after the parent, with its full lease.
   Plan output lists the apply order and marks both steps;
4. otherwise the child is refused.

The resulting sequence is then replayed against the live state, checking every
link each write touches, both to its parent and to its children; a plan
whose replay leaves any link exceeding its parent, even between two writes, is
refused. Chains of any depth are ordered the same way. Each write expects the
generation apply read, or for a second step the one the first step wrote. If a heartbeat moves it before the
write lands, the registry's compare-and-swap refuses; apply re-reads the agent
through the resolve route and retries once if its `ContentDigest` is unchanged,
and stops otherwise. The registration key hashes the policy digest, agent,
expected generation and lease. Apply stops at the first refusal and is not
atomic across agents.

`export` writes `executors.atpl.json` and one file per live agent, named
`agent.<id>.atpl.json` for plain lower-case IDs and by hash otherwise. Actions
are written in full, schemas compact. Each TTL is the lease remaining at one
export time, rounded to the second and clamped to the policy's maximum and to
the parent's exported TTL: export reflects what the live fleet holds at that
moment, not the TTL each agent was first registered with. The clamp absorbs
rounding and also a real inversion, where a parent's own heartbeat left it with
less time than its child: the child then exports with the parent's TTL, shorter
than what it holds live, and the export still compiles. A lease with under a
second remaining, measured before rounding, is refused. Without `-executors`, each executor's `max_effects` is the union of
its actions' effects and `min_effects` is empty, and the command warns that this
is not what the host binds. A live ID, domain, scope or executor reference that
is not UTF-8 is refused, naming the field. A live action that requires approval
(`docs/approval.md`) is written with `"approval": {"required": true}`, and only
such an action; export no longer refuses it, and the export recompiles to the
same registrations and digest. An action that requires attestation is written
with `"attestation": {"required": true}` likewise (tested through a real `fleet.Service`, and
end to end through `shoalctl`). Export refuses a directory that
already holds policy files, read with `os.ReadDir` so a directory name with glob
metacharacters cannot defeat the check, and refuses before writing anything if
the files together would exceed the 256 MiB a policy directory may hold.

Endpoints must be `https`, or `http` on loopback, with no credentials, query or
fragment; a bare trailing `?` or `#` is refused too. Requests go to the
normalized endpoint, built from scheme, host, port and path. Redirects are not
followed.

## Limits

- The registry lists only active registrations. A revoked or expired agent is
  invisible to plan, which shows its ID as a create, and the registry then
  refuses it: a revoked or expired ID cannot be registered again.
- If apply stops after the first step of a two-step write, that child keeps
  its clamped lease, its parent's old lease, rather than its full TTL. A
  re-plan does not compare leases, so it shows the child unchanged and does not
  raise the lease; only a heartbeat or a later content change does. The
  "stopped after N of M writes" error names each child left clamped and the
  time its clamped lease ends.
- `policy plan` does not yet show which policy produced each live
  registration. The hosted registry records it (see below), but there is no
  HTTP route that reads a registration's lifecycle receipt.

## Recorded policy source

A hosted registry (`internal/explorerfleet`'s durable lifecycle recorder)
records each registration's reason code and detail in its lifecycle receipt as
`interaction.Session.CallerAssertedReason`: what the authenticated caller
asserted, attributed to the receipt's trusted `Actor`, not verified by Shoal and
never used to authorize anything. It sits beside the trusted `Reason`, which
remains the decision's audit purpose. For reason code `atpl-apply` the detail
must be exactly `atpl:policy:v1:<64 lowercase hex>` and is kept verbatim as
`Source`; anything else is refused before the receipt or the registration is
written. Other reason codes keep their detail only as a SHA-256 `DetailDigest`,
and every code must match `[A-Za-z0-9_.:-]`; this code rule applies to every
registry route, reads (resolve, list) included.

The assertion is bound into the receipt's query digest, so a retry of the same
request with a different assertion conflicts (HTTP 409). That digest is an
unkeyed SHA-256: it detects a divergent retry, not tampering by someone who can
write the store. A register that replays an existing registration key is still
recorded: under its original request ID it is checked against the original
receipt, and under a new request ID it gets its own receipt with its own
assertion, attributed to its caller, over the same mutation. The receipt that
admitted the generation, the earliest one, is never changed. To find which
policy produced a generation, read that earliest receipt; a later replay
receipt records only what that later caller asserted.

Receipts are written under the identity `fleet.lifecycle.v4`. Receipts written
before the asserted reason was recorded have v1 or v2 identities and no
`CallerAssertedReason`. The first retry of such a request reconciles with them,
whatever it asserts, because they never recorded an assertion to compare. That
retry also writes a v4 receipt, which does record its assertion, so later
retries are held to it (see below). A v3 or v4
receipt without an asserted reason, or a v1/v2 receipt with one, is a conflict.

The query digest binds the registry mutation digest, which is versioned
(#521). v4 receipts carry the v2 digest, which length-prefixes every field,
writes a count ahead of every list (scopes, capabilities, actions, effects),
and writes each per-action flag explicitly. v1, v2 and v3 receipts carry the
v1 digest, which wrote no list counts and appended optional per-action fields
only when set, so two different descriptors could hash the same. The receipt's
identity says which version it holds, because a 32-byte digest cannot. A retry
that finds a v1–v3 receipt is compared using the v1 digest of its mutation, so
a request admitted before the upgrade still reconciles after it, and a changed
descriptor still conflicts. New receipts are only ever written as v4 with the
v2 digest.

Because the v1 digest can be shared by two different descriptors, matching a
v1–v3 receipt is not enough. A retry that matches one also writes the v4
receipt, recording the mutation actually being applied by its v2 digest. A
repeat of that retry reconciles with the v4 receipt, and any other mutation
under the same request ID conflicts with it, even one the v1 receipt cannot
tell apart. A consequence for v1/v2 receipts is that the first retry's
asserted reason is recorded in that v4 receipt, so a later retry asserting a
different reason conflicts.

During a rolling upgrade or a rollback, an old-build replica does not look for
v4 receipts. A retry it handles writes a duplicate v3 receipt, and it will not
flag a changed descriptor under the same request ID. That is the same gap the
earlier v2→v3 switch had.

The receipt's ID is `explorerfleet.LifecycleReceiptID(operation, request ID,
agent ID)`, and it is read with the corpus's `InteractionRecord`.

## Deferred

- `runtime` (ATPL's runtime attestation declaration, #446): refused by name.
  The per-action `attestation` requirement compiles; what a runtime must
  attest to is host trust configuration (`-fleet-executor-attestation`).
- Approval rules beyond the per-action requirement (approver sets, quorum,
  conditions). The format holds only `approval: {"required": true}`, which is
  everything the registry stores today; any other key in `approval` is refused.
- Admission obligations. They are computed per request at admission, not
  declared per agent; how a policy would constrain them is undecided.
- YAML. The repository has no YAML library, and every strict decoder here is
  JSON.
- Atomic multi-agent apply, revoking unmanaged agents, renewing leases.
- A server read API for executor ceilings, so export need not derive them.
- Whether the ATPL repository is archived with a pointer here
  (`docs/gateways.md`, open decisions).
