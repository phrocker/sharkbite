# Holding an operation for human approval

Issue #451, slice 1. Adopted from ATPL's `on_marginal: require_ztat`; see
`docs/gateways.md`, "What comes from ATPL".

A registered action can require approval: `"requires_approval": true` on the
action in a fleet registration. Every new request for that action is then held
until a principal independent of the requester has approved that exact request.
Nothing is performed before then, across retry and restart.

## The model

**A held request is not an action.** It is an `ApprovalRecord`
(`pkg/explorer/fleet/approval.go`) stored in its own key space, `approval/`
rows in the dispatch table (`internal/explorerfleet/approval_store.go`). No
`ActionRecord` exists for it until it has been approved and the requester has
come back for it. Pull, Claim, Invoke and ExecuteClaim all start from an
`ActionRecord`, so held work cannot be pulled, claimed or performed through any
of them, and none of them needed an edit. This is the property the admission
seam already relies on: an admission never passes through the queued state
(`docs/admission-seam.md`).

A new `DispatchState` was the alternative and was refused. A "held" state falls
through Claim's checks into `applyClaim` unless every check learns about it,
and the approver's transition would land in the requester's outbox, which is
#480's mixed-identity wedge.

The lifecycle is three compare-and-set steps on the approval record:

1. **Request** (requester, `dispatch`). `POST /api/v1/fleet/approvals/request`
   with the same body an enqueue takes. The service builds exactly the
   version-1 record an enqueue would commit, under exactly the checks an
   enqueue makes, and stores it as the request with its `request_digest` and
   the `policy_generation` in force. The request is decidable until
   `expires_at = min(now + window, deadline)`; the window defaults to one hour.
   The answer is `202` with `state: pending`.
2. **Decide** (approver, `action_approve`).
   `POST /api/v1/fleet/approvals/{id}/decide` with `verdict` (`approve` or
   `refuse`) and the `request_digest` and `policy_generation` the approver
   reviewed. Expiry is checked first and written as `expired`. A decision
   identical to one already made replays; any other decision on a decided
   request conflicts.
3. **Materialize** (requester re-requesting). The same request again. The
   rebuilt request must equal the stored one field for field. If something
   other than this approval's work already occupies the identity, the request
   is refused with a conflict and stays `approved`. Otherwise the approval
   moves `approved → enqueued`, and only then is the `ActionRecord` created
   (version 1, from the stored request) through the same audit and publication
   path an enqueue uses, carrying `ApprovalRequestDigest`,
   `ApprovalPolicyGeneration`, `ApproverSubject`/`Actor`/`ClientID` and
   `ApprovedAt`. The answer is `201` with `state: enqueued` and the action.

Re-requesting is idempotent at every step: it answers `pending`, `refused`,
`expired` or `enqueued`, or conflicts if anything about the request changed.
Refused and expired are answers, not errors. Concurrent re-requests of one
approved request all receive the same enqueued action; exactly one
`ActionRecord` is written. A materialization that collides with a concurrent
one still landing is retried internally with a short jittered back-off, because
the durable store reports that case as a conflict or a transient not-found
rather than as the fleet sentinel. Each attempt judges the window at its own
time, so a retried `approved → enqueued` commit cannot land after
`expires_at`.

`POST /api/v1/fleet/approvals/pending` lists what the caller may decide;
`POST /api/v1/fleet/approvals/{id}/status` returns one approval, with its
input, to the requester or to an eligible approver. Pending omits inputs to
keep a page within the workspace output budget.

### Status reports the effective state

Expiry is written lazily, by the next request or decision, and a request whose
target has moved can never progress although its row is unchanged. Status
therefore reports three things: `state`, what the request effectively is now;
`stored_state`, the row; and `condition`, why they differ. Nothing is written.

| `state` | `condition` | meaning |
| --- | --- | --- |
| `pending` / `approved` | — | live |
| `expired` | `window_closed` | stored pending or approved, window closed, expiry not yet written |
| `enqueued` | — | the action exists; its own status says where it is |
| `enqueued` | `enqueued_without_action` | the second write was lost; the next re-request writes it |
| `unresolvable` | `identity_taken` | committed to become work, but another route's action holds the identity |
| `unresolvable` | `target_moved` | the agent's generation moved, or the agent is gone |
| `unresolvable` | `policy_generation_moved` | the policy generation in force (read from the generation authority, not the caller's token) moved since the request |
| `unresolvable` | `approver_mapping_moved` | stored approved, but the operator approver mapping in force is not the one the approval was given under; it can never materialize |
| `unresolvable` | `identity_scheme_moved` | stored pending or approved, but made under an identity scheme other than the one in force (#526); it can never be decided or materialize, and only expires |
| `unresolvable` | `deadline_passed` | the action deadline passed; it can no longer be re-requested |
| `refused` / `expired` | — | final |

An action at the identity counts as this approval's work only if it is its
materialization: the same request, digest, approver and decision time — the
predicate a replay applies. `unresolvable` is never stored. An approver sees a request through Status and
Pending only while its target still resolves at the request's generation; the
requester always sees its own.

### Recovery after a crash

The `approved → enqueued` write commits the decision to make the work; the
`ActionRecord` is the second write. A crash between them is recovered by the
requester re-requesting — including after `expires_at` — **but only while the
request can still be re-requested**: before the action deadline, while the
agent is at the generation the request names, and while the policy generation
holds. A re-request after the deadline is refused before it reaches the
approval (`deadline_exceeded`), and one after the agent's generation moved
cannot resolve its target (`not_found`). The approval then stays `enqueued`
with no action, and Status reports it as `unresolvable` with the reason. No
work is created in either case; the requester must make a new request under
the new generation, which needs a new approval.

The record is built from the stored request and the stored materialization
time, so a retry builds the same work.

**An approval is a record, not a grant.** It grants the approver nothing. The
requester gets one queued record, which still runs only through its own
principal and the claim fence. Nothing reads the approval fields to authorize a
claim or an execution.

## Who may approve

All of these, checked on every approver path — decide, pending, and status
asked as an approver:

- Not under a narrowing the host applies, such as workspace settings. The host
  supplies the predicate (`ApprovalConfig.Narrowed`; `cmd/shoal-explore-web`
  answers from `webapi.EffectiveWorkspaceSettings`). A narrowing can only
  remove authority, so a principal holding approve and dispatch could narrow to
  approve alone and pass the next rule; separation is judged on the authority
  it actually holds. Pending refuses a narrowed caller outright (`401`). Decide
  and Status-as-approver run every other rule first and return the narrowing
  refusal only when the caller would otherwise be eligible on that record;
  any other failure answers `404` exactly as a missing ID does, so a narrowed
  caller cannot use the refusal to learn which approval IDs exist.
- `action_approve` authorized on the request's scope, and the request's target
  still resolving at the generation it names. Failing either answers as an
  absent record does.
- No identity in common with the request. The approver's subject and actor
  must not equal any of: the requester's subject, actor or delegation
  identities; the agent's ID; and, for the agent and every ancestor in its
  delegation chain, read from the registry, that agent's ID and its
  registrant's subject and actor. A delegated agent acts with authority its
  parent granted, so the parent agent and the parent's registrant are parties
  to the work. Equality is across fields, as in
  `internal/decisionadjudication`: acting for someone does not establish
  independence from them.

  For an OIDC approver this is **one comparison, not two**. A mapped approver
  is minted with actor = subject (`oidc:<iss>#<sub>`, or
  `oidcid:<iss>#<tag>#<value>` under a stable identity claim; below), so the subject
  and actor checks test the same identity, and the separation margin is
  exactly "the approver's subject must not overlap anyone involved". That is
  not a weakening: before the mapping every OIDC token's actor was the
  literal `shoal-explore-web-oidc`, a constant no principal controlled — and
  because the requester's actor is involved, that constant made every OIDC
  approver overlap every OIDC requester. No OIDC approver could approve any
  OIDC request; the flow failed closed, under a refusal ("approver is not
  independent of the request") that misdescribed the cause.
- The same identity scheme (#526). The approver must be named under the
  scheme the request was stamped with; a request made under another scheme
  cannot be decided (`identity_scheme_moved`). And under every OIDC scheme,
  the default one included, every involved identity in the human OIDC
  family (`oidc:`, `oidcid:`, `entra:`, under any issuer) must be in the
  namespace in force; the refusal names the namespace. Identities outside
  the family are not affected. See "Switching an issuer's identity scheme".
- No delegation. A decision carrying `OnBehalfOf` is refused.
- The approver must **fail** `dispatch`, `invoke` and `execute` on the scope.
  Under shared scopes, which every OIDC-minted principal has, holding approve
  alone separates nothing: two principals with dispatch and approve could each
  approve the other's work. Execute is included because an approver that may
  claim and perform the work it approved has not been separated from the
  effect.
- The same policy generation: the one named in the decision, the one the
  approver's credential is under, and the one in force according to the
  generation authority (`ApprovalConfig.Generations`). A token minted before
  the policy moved still carries the old generation.

What these rules deliberately do not do:

- **Client ID is not compared.** Independence is between people and agents,
  not between client applications; two people using one console are
  independent, and one person using two consoles is caught by subject and
  actor. This matches `internal/decisionadjudication`.
- **Separation is per credential.** An approver is judged on the credential it
  presents. One that holds dispatch under another token is not detected; the
  identity rule is what prevents it approving its own request in that case.

`auth.OperationActionApprove` (`action_approve`) is granted by
`auth.ServiceRoleActionApproval` (`action_approval`) and, in the shipped
binary, by exactly one list: `oidcApproverOperations`, minted only through the
approver mapping below. It is in none of the three workspace OIDC mappings,
the unmapped fallback, or the `-dev-auth` principal. Two tests hold that:
`oidc_approve_grant_test.go` parses every package-level `[]auth.Operation` in
the command and requires approve in the approver list alone (and that list to
be approve alone), and `oidc_approve_mint_test.go` mints a decision through
each authenticator — with and without an approver mapping in force, including
workspace tokens that carry the approver claim and value — and asks it whether
it authorizes approve.

### The OIDC approver mapping

`-oidc-approver-mapping-file` (environment `SHOAL_OIDC_APPROVER_MAPPING_FILE`)
names an operator file mapping OIDC humans to the approver role. **No file
means no approvers.** It is never in ATPL: ATPL is written by registrants,
and letting a registrant name who approves its own agents' work is the
conflict of interest #419 forbids.

**Supported issuers.** Independence is judged by identity, so an approver
mapping is only as sound as the promise that one human has one identity on
the workspace branch and on the approver branch. There are two ways to keep
that promise, and every issuer needs one of them:

1. **Public subjects.** Without `-oidc-identity-claim`, identities are
   `oidc:<iss>#<sub>`, and startup refuses the mapping unless the issuer's
   discovery states `subject_types_supported` as exactly `["public"]` (below).
   Auth0 is expected to qualify this way; Okta too once the operator adds an
   `azp` claim.
2. **A stable identity claim (#526).** With `-oidc-identity-claim`, both
   branches name every principal `oidcid:<iss>#<tag>#<value>`, read from the claim
   the flag names by one shared derivation, and the subject-type statement is
   waived. This is how Microsoft Entra ID and Keycloak are supported.

Either way every approver mint requires `azp`. The per-issuer notes below
describe expected behaviour; they were not verified against each vendor, so
confirm your issuer's discovery document and a sample token before relying
on them.

```json
{
  "version": "shoal.approvers/v1",
  "issuer": "https://shoal.example.auth0.com/",
  "audience": "https://shoal.example.com/approvals",
  "client_ids": ["shoal-console-client-id"],
  "claim": ["https://shoal.example.com/roles"],
  "values": ["shoal-approvers"],
  "max_values": 64,
  "human_assertion": {
    "claim": ["https://shoal.example.com/principal_type"],
    "equals": "human"
  }
}
```

**The human assertion is positive.** It names a claim that only human tokens
carry and the exact value it must have. There is no "claim is absent" form:
absence proves nothing about who a token was issued to. An earlier draft of
this mapping documented `idtyp` absent, and a Keycloak service-account token
passed it — Keycloak never emits `idtyp`, and its client-credentials tokens
(like Entra's and Auth0's) have `sub != azp`. Per issuer:

- **Auth0: expected to qualify on public subjects.** Discovery is expected to
  state `["public"]` and access tokens carry `azp`. Add the human assertion
  with a post-login Action that sets a namespaced claim (as in the example).
  A post-login Action does not run for `client_credentials` grants, which run
  the `credentials-exchange` trigger instead, so machine tokens never carry
  the claim. Those tokens also carry `gty: client-credentials`, which is
  refused anyway.
- **Okta: expected to qualify on public subjects, with an `azp` claim the
  operator adds.** Okta access tokens identify the client as `cid` (and the
  user as `uid`), not `azp`. `cid` is not accepted, so an Okta approver token
  is refused unless the authorization server has a custom claim named `azp`
  whose value is the client ID (`app.clientId`). Put the human assertion on a
  custom claim that only user tokens get, for example one included only for a
  user group.
- **Microsoft Entra ID: supported with `-oidc-identity-claim '["oid"]'` on a
  tenant issuer.** Entra issues pairwise `sub` values — a different `sub` per
  application — and says so in discovery, so on `sub` the same person would be
  two people. `oid` is the user's object ID: one value per user in a tenant,
  the same in every application's token, and not editable by the user. It is
  unique only within a tenant, so the issuer must be the tenant-specific
  `https://login.microsoftonline.com/<tenant-id>/v2.0`; an issuer whose path
  names `common`, `organizations` or `{tenantid}` is refused at startup.
  Entra v2.0 access tokens carry `azp`. Replace legacy Entra mode
  (`-entra-*`), which names principals `entra:<oid>` and cannot be combined
  with an approver mapping or with this flag.
- **Keycloak: supported with a stable claim from a User Property mapper.**
  Keycloak's discovery advertises `subject_types_supported: ["public",
  "pairwise"]` for every realm (`OIDCWellKnownProvider` sets it
  unconditionally), so it can never qualify on public subjects. Add a
  **User Property** protocol mapper for the user's `id` property, to the
  access token, with a token claim name such as `user_id`, on a **client scope
  shared by both clients** — the workspace client and the approval console —
  so both tokens carry the same claim the same way; then set
  `-oidc-identity-claim '["user_id"]'`. The user ID is assigned by Keycloak
  and never changes. **Never use a user attribute a user can edit** (a User
  Attribute mapper over an attribute exposed in the account console, or
  `email`, `preferred_username` and the like): whoever can edit the value can
  choose an identity, including someone else's. The flag refuses the common
  mutable claim names, but cannot know which custom attributes are editable;
  that is the operator's assertion.

**The stable identity claim is an operator assertion.** Code cannot prove a
claim is the same for one human across clients and never reused; the operator
asserts it by choosing the claim, per issuer as above. What the code
guarantees:

- **One derivation, both branches.** The workspace (requester) principal and
  the approver principal are both named `oidcid:<iss>#<tag>#<value>` by the
  same helper, where `<tag>` is the first 16 hex digits of a digest of the
  claim path. The value must be present and be a string of 1 to 256 bytes with no
  control character and no leading or trailing whitespace; it is never
  trimmed. Missing, `null`, empty, padded, too long, a number, an array or an
  object is the generic authentication denial on either branch. The flag is a
  JSON array of path segments, validated like the mapping's `claim`: a dotted
  string is refused, and a segment containing a dot names a key containing a
  dot, never a path.
- **A namespace per scheme.** `oidcid:` cannot equal or prefix an `oidc:` or
  `entra:` identity, so a stable identity never collides with a sub-derived
  one, whatever either value is; and the tag gives each claim path a
  namespace of its own, so `["oid"]` and `["uid"]` identities never collide
  either. The issuer is the one configured and the tag is fixed length, so a
  value containing `#` is still one identity.
- **Approvers.** Actor = subject = the stable identity. The client is still
  `oidc:<iss>#<azp>`, and `sub != azp` is still checked on the raw `sub`.
  `GrantProvenance` keeps the raw `sub` as `Subject` and records the claim
  path as `IdentityClaimPath`, appended to the authorization fingerprint only
  when present, so every existing fingerprint is unchanged.
- **Refused at startup:** `["sub"]` (that is the subject the claim replaces);
  a path whose last segment, in any case, is a claim that does not name one
  human stably — an editable identifier or profile field (`email`,
  `preferred_username`, `upn`, `unique_name`, `name`, `nickname`,
  `given_name`, `family_name`, `locale`, `picture`, `website`, `zoneinfo`),
  a per-session or per-token claim (`sid`, `session_state`, `jti`, `nonce`,
  `at_hash`, `c_hash`, `auth_time`, `iat`, `exp`, `nbf`, `acr`, `amr`), or a
  per-client one (`azp`, `client_id`, `cid`); an issuer containing `#`
  anywhere (it separates the issuer from the value in every identity); and
  the flag together with a non-default
  `-oidc-subject-claim`, legacy Entra mode, `-oidc-actor-claim` or
  `-oidc-delegation-claim`. Each of those would put a second, possibly
  per-client identity into the decision, and the approval service counts
  every identity a request carries as involved.

  The list checks the claim's own name, and cannot be complete. **Never
  choose a path under a parent the user can edit** — for example a claim
  inside a user-attributes object an account console exposes — whatever the
  last segment is called: whoever can edit the parent can choose the value.
- **The mapping restates it.** The approver mapping file's `identity_claim`
  must equal the flag segment for segment, byte for byte, whenever either is
  set, and it is part of the mapping digest (only when present, so a mapping
  without it keeps its digest). A changed claim therefore also moves every
  approval pinned to the old mapping to `approver_mapping_moved`.
- **The subject-type check is waived only with the claim.** Without the flag
  the public-subjects check below applies unchanged. With it, discovery must
  still be readable: an issuer whose discovery cannot be fetched is refused
  either way.

**`-oidc-subject-claim oid` is not this.** `-oidc-subject-claim` changes the
workspace branch only: the approver branch is always named by `sub`, so the
approver mapping still refuses a non-default subject claim. Since #546 a
non-default subject claim names identities `oidc:<iss>#<tag>#<value>`, in a
namespace of its own (the tag is a digest of the claim name), not
`oidc:<iss>#<value>` in the namespace of `sub`. Before that, a deployment
that changed the claim, or two replicas that differed, could give one human's
`oid` and another human's `sub` the same identity.

**The issuer must state public subject identifiers only — without the stable
claim.** Without `-oidc-identity-claim` the approval service separates people
by `oidc:<iss>#<sub>`. Under the pairwise subject type one human has a
different `sub` per client and could approve their own request. So the server
refuses to start with an approver mapping and no stable claim unless the
issuer's discovery document states `subject_types_supported` as exactly
`["public"]`. A missing statement, any other list, or a discovery document
that cannot be read is a refusal; there is no override. Every approver mint
checks the cached discovery document again.

Startup refuses the file, and the server does not start, when: a field is
unknown, a key is repeated, or anything follows the object; `version` is not
`shoal.approvers/v1`; `issuer` is not byte-equal to the trimmed
`-oidc-issuer` (no trailing-slash, case or whitespace leniency); `audience` is
empty or is one of the workspace audiences; `client_ids`, `claim` or `values`
is empty; any string has leading or trailing whitespace or a control
character; `claim` is not a list of path segments (a dotted string is refused,
and a segment containing a dot names a key containing a dot, never a path);
`max_values` is outside 1–1024; `human_assertion` is missing or has no
`equals` (an `absent` key is an unknown field); `identity_claim` does not
restate `-oidc-identity-claim` exactly (above). The mapping also requires the
default subject claim (`sub`) and refuses the legacy Entra identity mode, in
which the same human would carry an `entra:` identity as a requester and an
`oidc:` one as an approver and could approve their own request.

### Switching an issuer's identity scheme

Changing how principals are named changes every identity, and an identity
under one scheme cannot be compared with one under another: the same human
can hold one of each. That is as true of a switch from one stable claim to
another, or back to `sub`, as of the first switch to a stable claim — and of
**changing `-oidc-issuer`**. The issuer is part of every identity, so moving
from Entra's v1 issuer (`https://sts.windows.net/<tenant>/`) to its v2 issuer
(`https://login.microsoftonline.com/<tenant>/v2.0`), or moving a Keycloak
realm to a new hostname, renames every human although `oid` or the user ID
is unchanged. An issuer change is a scheme switch, with the same rules and
the same rollout as any other.
Independence has to hold across every switch, and through the rollout that
makes it, and three things make it hold.

- **Only the namespace in force is comparable.** Under every OIDC scheme,
  the default sub-derived one included, `eligibility` refuses an approval
  when any involved identity — the requester's subject, actor and delegation
  chain, the agent, its registrant, any ancestor's ID or registrant, and the
  approver itself — is in the human OIDC family (`oidc:`, `oidcid:` or
  `entra:`, under **any** issuer) but not in the namespace in force
  (`oidc:<iss>#` under `sub`, `oidc:<iss>#<tag>#` under another
  `-oidc-subject-claim`, `oidcid:<iss>#<tag>#` under a stable claim,
  `entra:` under legacy Entra). The namespace of `sub` is flat: an identity
  in it has no `#` after the issuer's, and the server refuses a `sub`
  containing `#`, so a subject claim's `oidc:<iss>#<tag>#<value>` (which
  begins with `oidc:<iss>#` as a string) is foreign under `sub`, and a `sub`
  identity is foreign under the claim (#546). The refusal names such a
  nested namespace `oidc:<iss>#<nested>#`. The family is not scoped to the issuer in
  force: a deployment has one human issuer, so an identity under another
  issuer was minted before the issuer changed and may be the same human.
  There is no list of previous schemes or issuers to keep complete: any
  identity minted under another scheme is refused, whatever the switch
  history. Executor identities (#391, `oidcexec:`) are not human and are not
  in the family. Otherwise someone who registered an agent as
  `oidc:<iss>#<sub>` could approve work on it as `oidcid:`, and someone who
  registered under one stable claim could approve under another. Identities
  outside the family — other issuers, development, service and MCP
  principals — are not affected. The refusal names the namespace, never the
  identity, and fails closed. **Adoption (#526, PR2) is how approvals
  resume**: an adoption route will move a descriptor subtree into the
  namespace in force, proved by the previous subject the same workspace token
  yields, and audited with both identities. Until it ships, work on an agent
  registered under another scheme cannot be approved; work on an agent
  registered under the scheme in force can.
- **Requests are stamped.** Each request records the scheme its requester
  was named under (`ApprovalRecord.IdentityScheme`, set once at request time;
  the store refuses any write that changes it). Requests under the default
  scheme and legacy Entra are not stamped (requests under a non-default
  `-oidc-subject-claim` are, since #546), and a record written before the stamp existed
  decodes with it empty too, so a deployment that does not switch decides
  every old request exactly as before. After a switch, a request made under
  another scheme cannot be decided — `decide` answers `409`
  (`identity_scheme_moved`, `ErrIdentitySchemeMoved`), Pending skips it, an
  approver's Status answers as for a missing ID, and the requester's Status
  reports `unresolvable` / `identity_scheme_moved`. It can only expire. The
  requester makes a new request under their new identity.
- **The scheme is recorded, and switched once.** Every OIDC deployment
  writes a digest of its identity scheme — the issuer, the claim path and the
  identity format — to the domain's one `identity-scheme` row in the
  coordination store (one row, not one per issuer, so a new issuer finds the
  previous issuer's record rather than an empty row), beside the policy generation, the first time it starts,
  and checks it on every start after; startup prints it. A replica whose
  scheme differs refuses to start, and the refusal prints the recorded
  digest. To switch, start the new configuration with
  `-oidc-identity-scheme-migrate=<recorded digest>`: a replica starts only if
  the row holds exactly that digest (it then records its own) or already
  holds its own (a later replica of the same rollout). The flag is
  therefore one-shot — once the row holds the new scheme it names nothing a
  different configuration could replace — and two replicas on different
  schemes cannot flip the row back and forth, since each would need a flag
  naming the other's. Remove it after the rollout all the same. Switching
  back is another explicit switch, naming the new digest.

**Rolling out a switch.** The row is checked only when a replica starts, so
replicas on the old configuration keep serving until they are replaced. That
window is safe because of the first rule: an approval that would compare an
identity minted by an old replica with one minted by a new one is refused on
either replica, and a request made on one cannot be decided on the other.
The order matters, though:

1. **Upgrade every replica to this build on the scheme you already run**
   (no `-oidc-identity-claim` change). Each records the scheme, if it was not
   recorded, and from then on holds approvals to its namespace.
2. **Then switch**: roll out the new configuration with
   `-oidc-identity-scheme-migrate` naming the recorded digest, and remove the
   flag afterwards.
3. **Never roll back to a build before #553 across a switch.** Older builds
   read neither the row nor the stamp, and do not hold approvals to a
   namespace, so they would compare identities across schemes. Rolling back
   to such a build is safe only while the scheme in force is the one it ran.

**Minting is decided by audience and fails closed.** A token on a workspace
audience is minted by the workspace mappings, which never grant approve. A
token on the approver audience is minted as an approver or denied — there is
no fallback to reader. A token carrying both is denied. On the approver
audience every one of these is required:

- `azp` is one of `client_ids`, and `sub != azp`. This catches exactly one
  shape — a token whose subject is literally its own client — and not
  client-credentials tokens in general, which on Keycloak, Entra and Auth0
  have `sub != azp`. The human assertion is what refuses those;
- none of `client_id` or `clientId` (Keycloak service accounts) is present,
  and `gty` is not `client-credentials` / `client_credentials` (Auth0) —
  defence in depth beside the human assertion, never instead of it;
- none of `act`, `may_act`, `_claim_names`, `_claim_sources`, `hasgroups`, or
  the configured `-oidc-delegation-claim` is present (delegation, or a group
  list the issuer truncated);
- the human assertion holds;
- the value at `claim` is a string or an array of at most `max_values`
  strings, and one of them equals a configured value **byte for byte** — no
  trimming, case folding or Unicode normalization;
- where the token also carries the workspace authorization claim, none of its
  values maps to `-oidc-fleet-values` (the human holds dispatch).

Every failure is the same generic authentication denial. The decision grants
`action_approve` and nothing else, with subject = actor = `oidc:<iss>#<sub>`
(or `oidcid:<iss>#<tag>#<value>` under a stable identity claim), client
`oidc:<iss>#<azp>`, no `OnBehalfOf`, the workspace policy generation, and an
`auth.GrantProvenance` of issuer, raw subject, claim path, matched value, the
mapping digest and, under a stable claim, the identity claim path. The
provenance is appended to the authorization fingerprint only when present, and
the identity claim path within it only when present, so every existing
fingerprint is unchanged.

**The mapping digest is pinned, not the generation.** The workspace policy
generation stays fixed configuration: deriving it from a hash of the mapping
could move it backwards. Instead the digest of the mapping in force
(canonical; reordering a set does not move it) is `ApprovalConfig.ApproverMapping`:

- `decide` requires the approver decision's mapping digest to equal the one in
  force (both zero when no mapping is configured; a decision without
  provenance is refused while one is), and stores it on the record as
  `ApproverMappingDigest` beside `ApproverProvenance`;
- the requester's re-request checks it again before `approved → enqueued`. A
  changed mapping refuses (`409`, `ErrApproverMappingMoved`), the row stays
  approved, and Status reports `unresolvable` / `approver_mapping_moved`;
- a pending request is unaffected until it is decided, and is then decided
  under the mapping in force.

**Audit.** Each approval transition's audit carries the acting principal's
provenance; the trusted session commits to it (its query digest covers it) and
the durable approval record holds the values. No token byte is recorded
anywhere.

**Correlation across hops.** Every interaction session an approval produces
carries the correlation of the decision it was recorded under (#532): the
request's hold, the approver's decision, the requester's return that
materializes it, and the dispatched action's own audit (`approval_enqueue`)
and lifecycle event audit. A gateway that threads one `Shoal-Correlation-ID`
through all three hops can therefore join the whole approval-to-dispatch trail
from the interaction audit alone; a caller that sends none gets a generated
value per request, the same one the approval and action records hold. The
correlation is metadata, never part of a session's identity — see the
interaction contract in `explorer-public-contract.md`.

**The action audit's actor and correlation (#480 items 2–3).** The action
recorder still builds its expected actor from the record's enqueuer, which is
wrong for a claimant that is not the enqueuer (#480 item 2), and the outbox
still publishes under one caller's decision (#480 item 3). #532 deliberately
does not touch either. It does not carry the acting decision on `ActionAudit`;
the action recorder *accepts* the correlation the trusted sink stamps rather
than comparing it with the record's transition correlation, because those can
differ for the same reason the actors can, and correlation must never fail an
audit. When #480 gives `ActionAudit` the acting principal, it can bind the
correlation the same way. Until then a session's correlation and its actor are
both the recording decision's, which for every request-scoped audit is the
decision that caused it; for a reconciled transition it is the reconciler's.

**Disclosure.** The mapping is never served: the browser login configuration
is identical with and without it, and no approval response carries the
provenance or the digest. Non-approvers keep their `404`s. The requester sees
who approved their own request (`approver`, `approver_actor`) — that is
accountability for that request, and it is intended.

**Not detected.** A human who holds the fleet mapping through a group their
approver token does not carry is not detected; separation is per credential,
and the identity rule is what stops them approving their own request. Keep
the approver and fleet groups disjoint.

## What enqueue, invoke and admission do

- **Enqueue and invoke** of an approval-required action return
  `ErrApprovalRequired`: HTTP `409`, code `conflict`. Conflict, deliberately:
  the caller is authorized and the request is well formed, so neither `400` nor
  `401` is true, and a retry of the same request will never succeed, so `503`
  would mislead. The request conflicts with the action's registered state and
  the remedy is a different route. Clients should branch on the `409` from these
  two routes and resubmit through the approval route. A refused enqueue is not
  audited: nothing is written, no action exists for an audit to name, and the
  approval route, which is audited at every transition, is where this work goes.
- **Invoke on an identity an approval already materialized** reaches enqueue's
  replay branch, which returns the existing record before the guard, and Invoke
  then claims and runs it in process if an in-process executor is bound. That
  is the one approved request, run once, by its requester under the claim
  fence — the same work any worker would claim — and not a way round the
  approval. It is the case "synchronous invoke of an approval-required action"
  in Deferred refers to: it works by accident of the replay branch rather than
  by design, and slice 1 neither relies on it nor forbids it.
- **The pre-call admission seam (Path B)** denies an approval-required action
  durably with no reason, like any other denial. Holding is not available
  there: the caller is waiting on the answer with the payload in hand.

### Beside executor attestation

An action may require both approval and executor attestation
(`docs/executor-attestation.md`). They gate different moments and do not
interact: approval holds a request before any record exists; attestation is
judged when a claim is granted (Claim, ExtendClaim and the born-claimed
admission grant), against the claimant. An approved, materialized record is
claimed like any other, so its claimant still needs a current attestation.
Both deny Path B durably with no reason. Both are generation changes when
registered, with the same stranding note below.

## Work that already exists when the flag is registered

`RequiresApproval` gates new work. It does not reach a record already queued,
and the enqueue guard sits after the replay branch for the same reason: a
replay creates nothing, and refusing it would only make a caller's retry fail
where its first attempt succeeded.

What an operator should expect when registering the flag on a live agent:

- Registering it is a new generation of the agent. A queued record names the
  generation it was enqueued against, so every **not-yet-claimed**
  old-generation record stops resolving: Pull omits it and Claim answers not
  found. It is not deleted and does not run.
- A record **already claimed** before the flag is outside this change: its
  claimant still holds the claim it was granted, and completion re-resolves the
  binding, so it too stops resolving once the generation moves. (A claim-time
  generation re-check is deferred to #438/#430.)
- An enqueue retry of an old record replays it as it is; a new enqueue at the
  new generation is refused with `409`.
- An admission granted or denied before the flag replays from its record.
- The flag cannot be removed by re-registration or by a delegate: dropping it
  widens authority and is refused like any other widening. Removing it means
  revoking the agent and registering a new one.

The same generation rule applies to heartbeats: a heartbeat moves the agent's
generation, so a request held across a heartbeat no longer matches its target
and becomes `unresolvable`. Approval windows should be shorter than the
heartbeat interval; this is the same property queued dispatch work already has.

## Storage and compatibility

- The v1 registry mutation digest appended the flag only when it was true, so
  every digest from before the flag existed stayed unchanged. The v2 digest
  (#521), which all new receipts carry, writes the flag explicitly as a 0 or 1
  field on every action; v1 is still computed only to reconcile a retry with a
  receipt written before v2 (see `docs/atpl.md`). The JSON form omits the key
  when false.
- The descriptor codec writes version 4, with one flag byte per action, only
  for a descriptor in which some action requires approval. Every other
  descriptor is still version 3, byte for byte. A previous build refuses to
  decode version 4 rather than reading the action without its flag.
- The new `ActionRecord` fields are additive; gob decodes older records with
  them absent, and `Validate` requires them all or none.
- `ApprovalRecord.ApproverMappingDigest` and `ApproverProvenance` are
  additive gob fields: a record a previous build wrote decodes with both
  zero, and a previous build reads a record that has them by skipping them.
  `Validate` refuses either on an undecided record and a digest that is not
  its provenance's.
- `ApprovalRecord.IdentityScheme` (#526) is an additive gob field, written
  once at request time. A record a previous build wrote decodes with it zero,
  which is the sub-derived scheme, never "a scheme that differs from every
  other"; a previous build reads a stamped record by skipping it. The
  approval store refuses any write that changes it, or the request, its
  digest, generation, request time or expiry, or — once written — the
  decision or the materialization time (`refuseRewrittenApproval`, the
  approval record's counterpart of #461's action invariant).
- The `identity-scheme` coordination row (`coordination.IdentitySchemeRow`,
  value `IdentitySchemeV1`) is new; both encodings are pinned by golden
  fixtures. A build without it ignores the row.
- `approval.*` event kinds are reserved on the public publish route, which
  refuses every one. The trusted route publishes them only as described in
  "Lifecycle events" (#653).
- Approval event outbox rows (#653) are new: `approval-event/<hex id>/<version>`
  rows in the dispatch table, guard kind `V`, magic `SFV1`. A record written
  before them has no row, and its transitions publish nothing. A build without
  them ignores the rows.
- ATPL (#452): a policy file declares the requirement per action as
  `"approval": {"required": true}`, which compiles to `requires_approval`.
  Inheritance carries it and delegation cannot drop it; plan treats adding it
  as a narrowing and omitting a live one as a refused widening; export writes
  it, and the export recompiles to the same policy digest. A policy without
  approval keeps the digest it had before (`docs/atpl.md`). The earlier
  `refused-approval` plan kind and the export refusal are gone.

## Lifecycle events

Issue #653. Each approval transition is published as one fleet event, under
the identity of the principal that made it.

| kind | transition | published by, under |
| --- | --- | --- |
| `approval.requested` | hold (v1, `pending`) | the requester, `dispatch` |
| `approval.approved` / `approval.refused` | decide | the approver who decided, `action_approve` |
| `approval.expired` | expiry written | whoever's call observed it (below) |
| `approval.materialized` | `approved → enqueued` | the requester, `dispatch` |

The materialized action then has its own `action.enqueued`, as before. Each
event's `action_id` is the approval ID, which is also the action's. Its
authorization join is the request's source, policy and target object. It
carries no input, request digest, approver identity, grant provenance,
correlation ID or request reason. Status shows the parties none of the other
party's correlation or the request's reason digest, and the event shows less
than Status. The publication audit still carries the publisher's correlation,
stamped from its live decision (#532). `occurred_at` is when the transition
took effect.

**The outbox is per transition.** This is what #480's mixed-identity wedge
required. The approval store writes one outbox row per transition, keyed by
approval ID and version, **in the same coordination publish as the approval
row**, so a committed transition always has its row. The row names its
publisher: the subject, actor, client, delegation chain, operation, request
and correlation IDs, fingerprint and expiry of the decision that made the
transition. `ApprovalTransition.Validate` binds that publisher to the record,
and the store refuses any other: the requester for requested and
materialized, the approver the record names for a decision, and the observer
for an expiry. A row is published only under its publisher's own live
decision, by the same gate dispatch uses (identity match, plus the operation
on the scope). So nothing can publish the approver's event under the
requester's name, or the other way round.

**Rows are independent, so nothing wedges.** After its own commit, each
Request or Decide publishes its own row. It also drains the other pending rows
of that approval that it is the publisher of, and skips everyone else's.
Whether a row is the caller's is decided by comparing the row's stored
publisher with the caller's decision, never inferred from an error.

- A call that committed a transition answers only for that transition. A
  failure on any earlier row, even the caller's own, is counted
  (`ApprovalService.ApprovalEventCounters`) and left pending. A stranded
  approver row therefore never blocks the requester's materialization or its
  answer.
- A replay that commits nothing answers for any failure on the caller's own
  pending rows, including an outbox read or authorization lookup that failed,
  because a replay is the retry an unpublished event asked for. Answering
  success there would lose the event, since nobody else may publish it. A
  replay never answers for another principal's row. A committed transition
  whose own row is not yet readable (the committed-read lag, #633) is answered
  the same way, not reported as published.
- An own-row failure is `ErrApprovalEventUnpublished`: HTTP `503`, **not**
  marked indeterminate, since nothing external was contacted. The same
  request replays the transition and publishes its event: a re-request for
  the requester, the duplicate decision for an approver.

**A crash between a transition and its event** leaves the row pending. The
next call by its publisher on that approval publishes it. The event's token is
a function of (kind, approval, version), so a publication that landed and was
not acknowledged is deduplicated, not repeated. This holds across restart.

**No row can wedge forever** (#650's review). The backend deduplicates a
token until its `RetryUntil` and refuses a `RetryUntil` more than 24h ahead of
its clock. An approval event's `RetryUntil` is fixed by its row:
`UpdatedAt + 23h`. The row stops being published an hour before that, at
`UpdatedAt + 22h`. Both hours are replica clock skew, one on each side:

- A transition written on a replica behind the previous writer has its
  timestamps clamped forward (see "Clock skew"). With a full 24h,
  `RetryUntil` would exceed the bound on that replica's clock, and its event
  could not publish until the clocks agreed.
- The backend prunes dedup records by the clock of whichever replica next
  appends. A replica running ahead could prune a token before `RetryUntil` by
  the publisher's clock, and a retry in between would be appended twice.

From `UpdatedAt + 22h` a pending row is closed **lapsed** by whoever next
drains the approval, whoever they are. Closing publishes nothing and
attributes nothing, so it needs no identity. Publishing then could duplicate
an event whose first publication landed and whose acknowledgement did not. A
lapsed row is terminal, never retried and never an error.

**The residual is stated, not hidden.** An event is lost only if its own
publisher's publication failed and that publisher made no further call on the
approval within 22h, or if its row stayed unreadable (the committed-read
lag) through both the committing call, which answers `503`, and the retry
that call asked for. A replay drains the rows it can read and cannot tell a
lagging row from one a build before #653 never wrote. A persistent failure on
a caller's own row, such as replica skew beyond the hour allowance, makes that
caller's replays of that one approval answer `503` until the row publishes
or lapses. Status, which publishes nothing, is unaffected. A lapsed row whose publication had in fact landed was
delivered. Either way the approval record, and its audit, remain the durable
truth. Nobody else may publish the row instead, by design (#480). An operator
remedy, as #657 proposes for dispatch rows, is not provided here.

**Expiry has no actor.** Time closes the window. Expiry is written lazily, by
the next Request (the requester, under `dispatch`) or Decide (an approver,
under `action_approve`) that finds the window closed. That writer is the
expiry's publisher. Its event has `occurred_at = expires_at`, and its reason
code is `approval.expiry.observed_by_requester` or
`approval.expiry.observed_by_approver`. The publication audit names the
observer's request. A request nobody touches after its window is never
written as expired, and so publishes no expiry. That is the stranded-rows
residual below, and a sweeper (#655) would publish as itself.

**Who receives them.** The fleet event join admits any subscriber holding
`subscription_create` on the request's source and policy, which every
co-tenant of the scope does. Status answers a co-tenant `404`. So an approval
event is delivered only if `ApprovalService.VisibleTo` also admits the
subscriber, asked under the subscriber's own context at delivery. That is
Status's rule: the requester, or a principal eligible to decide the request
under every approver rule. A narrowed caller is never shown one, where Status
answers it `401` only when otherwise eligible. An event has no response to
vary, so there is nothing to tell it. Every refusal is silent. So is a
failure to establish eligibility at all, such as a target whose executor ref
the host no longer configures: Status answers that `503`, but at delivery an
error would fail the subscriber's whole page behind one approval event and
tell a principal that should see nothing that it is there. A failed read of
the approval itself withholds the event too, for every subscriber, whether
the failure is transient or permanent: that read runs for every subscriber
the scope join admits, co-tenants included, so an error would fail all of
their pages, and a permanent one forever. A party can therefore miss one
notification during a store fault; Status still answers it. The way a record
became permanently unreadable before, another store adopting its guard head
through the shared kind letter 'P', is refused by the guard since the
head-ownership fix. A host that
configures no rule delivers approval events to nobody.

**Forgery.** The public route refuses the `approval.` namespace. The trusted
route admits an approval kind only under the one operation that transition is
made under, and only if the **approval ledger** confirms it. The ledger reads
the durable outbox and requires a row for the approval named whose derived
publication equals the request field for field: token, kind, times, producer,
evidence, operation and receipt. Holding the lifecycle capability
proves only that the caller is host code. An `approval.approved` for a request
nobody approved has no row to match. A host that configures no ledger admits
no approval event.

**Ordering.** Events are in publication order. A late event, such as an
approver's decision published on the approver's retry after the requester had
already materialized, appears after events it preceded. `occurred_at` says when
each happened.

## Clock skew between replicas

A transition written on a replica whose clock is behind the one that wrote the
row clamps its timestamps forward to the row's latest (`RequestedAt`,
`UpdatedAt`, `DecidedAt`, `MaterializedAt`) instead of failing validation. The
clamp moves time forward by at most the skew and never past `expires_at`,
because every transition has already checked the window.

The materialized action's `UpdatedAt` is the clamped materialization time. If
the materializing replica is behind the deciding one, that time is ahead of
its clock, and the lifecycle publisher, which bounds a publication's retry
window from `UpdatedAt` against the local clock, refuses to publish: the
caller is told the action committed and needs reconciliation, and a
re-request once the clock has caught up replays the one record. Every dispatch
transition reconciled on a lagging replica has the same property.

## Testing

New end-to-end tests of the approval path should go through the real
authenticator: a signed token, real HTTP, the authenticated handler, the real
binder and the bound providers, as `oidc_approver_e2e_test.go` does. A test
that injects a bound context (`h.as(...)`) or re-mints the authenticator's
decision cannot see a gap in the authenticator; #524, where neither shipped
authenticator minted the correlation ID every fleet route requires, passed
every such test while the whole surface was unreachable. Injected-context
tests remain the right tool for service-level properties below the transport.

## Residuals

- **Pre-existing: an admission denial cannot be published by the hosted
  publisher.** The denial commits a cancelled record whose authorized
  operations are `[invoke]`, and the hosted publisher permits `action.canceled`
  only under `dispatch`. The first answer to a denied admission is therefore a
  `503` "requires reconciliation" over a denial that did commit; the replay
  answers `denied`. This affects every admission denial on main, including the
  effect-ceiling one, and is in #480's area. It is safe — nothing is granted —
  and it is pinned by the acceptance tests so a fix flips them visibly.
- **Approval identities are caller-chosen**, as dispatch action identities
  are. A principal with dispatch on the scope can enqueue an ordinary action at
  the identity of someone's held request. Found before materialization, the
  approval stays `approved` and the re-request conflicts; landing between that
  check and the commit leaves the approval `enqueued` under someone else's
  action, which Status reports as `unresolvable` / `identity_taken`. A conflict on request also reveals that something exists at
  an identity. Both are the existing dispatch identity properties.
- **An in-scope approver can learn that an approval ID exists.** Without a
  narrowing, a caller that holds `action_approve` on a request's scope but is
  ineligible to decide it — not independent of it, holding dispatch, invoke or
  execute, or acting on someone's behalf — is told why (`401`), where a
  missing ID or a request outside its scope answers `404`. This is kept
  deliberately: such a caller already holds approve on that scope, and the
  specific refusal is what lets it see that it is the wrong approver. Only a
  narrowed caller is concealed, because the narrowing is what it could vary to
  probe.
- **Response timing is an existence oracle for narrowed callers.** The error
  bodies for a missing ID, a request in another scope and a request the caller
  is not independent of are byte-identical, but the work behind them is not:
  measured at roughly 65µs, 650µs and 1.5ms respectively, because each answer
  is reached after a different number of store and registry reads. Closing it
  would need constant-time answers, padded to the slowest path; that is not
  done.
- **A generation move inside the last window.** The in-force policy
  generation is checked immediately before the `approved → enqueued` commit,
  and the agent generation is resolved by the re-request just before that. A
  generation that moves between those checks and the store's compare-and-set
  is not seen. This is the same uncoordinated window a plain enqueue has: the
  registry and the policy authority are not part of the dispatch store's
  transaction.
- **Stranded rows.** A request whose target generation or policy generation
  moves, or whose deadline passes, stays in its stored state forever; nothing
  sweeps it. It also never publishes an expiry event, since nobody writes
  the expiry. Status reports it as `unresolvable`, Pending omits it, and neither
  decide nor re-request can move it. A sweeper is deferred.
- **Audits record attempts.** Approval audits precede their write, and each
  attempt has its own session, keyed on the acting principal and request ID, so
  a lost compare-and-set or a crash cannot wedge the record. An audit whose
  write lost a race describes a decision that was attempted, not made; the
  approval row is the outcome.
- **The materialization audit** uses the dispatch action recorder's session
  identity (phase, action, version). A crash after that audit and before the
  action write is recovered by a retry under the same credential; under a
  different one the audit session conflicts. This is the property every
  dispatch transition already has.

## Deferred

- Dataset export of approvals and refusals as adjudications (#401, #419). Every
  field it needs is stored on the approval record.
- #526 PR2: the adoption route that moves a descriptor subtree into the
  identity namespace in force (until then, registrations under another
  scheme block approval), and retiring legacy Entra mode.
- Approver pools per action or descriptor (a later ATPL version may only
  reference an operator-defined pool), a second approver-only issuer, and
  quorum.
- Synchronous invoke of an approval-required action, by design rather than
  through the replay branch.
- A claim-time generation re-check (after #438/#430).
- Requester withdrawal of a held request, and a sweeper for stranded rows.
