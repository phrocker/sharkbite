# Executor attestation

> **Status:** #446 slice 3. Part 1 (#522) is the verifier and store,
> `internal/executorattest`. Part 2 is fleet enforcement: the per-action
> requirement, the ATPL `attestation` field, the presentation route, the
> public `pkg/attestation/api` client and `sdk.Attestation()`. See
> "Enforcement" below.

An executor attests that it runs a pinned image by presenting a statement signed
by an operator verifier. The server checks it against a trust file and records
the latest verified result per principal and executor ref.

## Statement

The evidence is a JSON envelope:

```json
{"verifier_id": "operator-key:1", "statement": { ... }, "signature": "<base64url Ed25519>"}
```

The signature is Ed25519 over `"shoal-executor-statement-v1\x00" || statement`.
That signing context differs from collector statements
(`shoal-collector-statement-v1\x00`), so a collector statement is never accepted
as an executor statement, and the reverse.

The statement is canonical JSON with exactly these fields, in this order. A
non-canonical encoding (whitespace, reordered or unknown fields) is refused:

| Field | Meaning |
|---|---|
| `authorization_domain` | base64url (no padding) of the caller's authorization domain |
| `subject` | base64url of the caller's subject |
| `client_id` | base64url of the caller's client |
| `executor_ref` | the fleet executor ref |
| `image_digest` | `sha256:<64 lowercase hex>` |
| `provenance_digest` | optional, same form |
| `issued_at`, `expires_at` | RFC 3339 times |
| `nonce` | `executorattest.Nonce(principal, ref, key)` |

The nonce is the hex SHA-256 of a domain tag, domain, subject, client, ref and
the presentation's idempotency key, each length-prefixed. A statement minted for
one presentation cannot be replayed under another key or by another principal.
The principal always comes from the authentication decision, never from the
request.

## Trust file

```json
{"executors": {
  "executor:deploy": {
    "verifiers": [{"id": "operator-key:1", "public_key": "<std base64, 32 bytes>",
                   "max_validity": "1h", "clock_skew": "1m"}],
    "image_digests": ["sha256:..."],
    "provenance_digests": ["sha256:..."]
  }}}
```

Parsing is strict (`internal/strictjson`). It refuses:

- unknown fields and trailing data;
- duplicate JSON keys at any level, including a repeated executor ref;
- keys that match a field only up to case, because encoding/json would otherwise accept them last-wins and case-insensitively.

Public keys must be canonical encodings of points in the prime-order subgroup
(`internal/ed25519key`). crypto/ed25519's cofactorless `Verify` accepts forged
signatures under a small-order key, such as the identity. collectorattest
applies the same key check. Each ref needs
at least one verifier and one image digest. `max_validity` must be in (0, 1h],
and `clock_skew` must be in [0, 1m]. Verifier IDs and keys are unique within a
ref. Across refs, an ID is always bound to the same key, so the `VerifierID`
names one signer. Duplicate or malformed digests are refused.
`provenance_digests` is optional. When it is set, a statement must carry a
pinned provenance digest.

## Verification

`Verify(trust, Expectation{Principal, ExecutorRef, Key, Now}, evidence)` refuses
on any of these:

- an unknown ref or verifier;
- a bad signature, including a wrong signing context;
- malformed or non-canonical bytes;
- a mismatch on any bound field (domain, subject, client, ref, nonce);
- an unpinned image digest, or an unpinned provenance digest;
- validity over `max_validity`;
- `issued_at` later than now plus skew;
- expiry, judged strictly: `now >= expires_at` is expired.

Skew applies only to `issued_at`. It never extends expiry.

Each refusal carries a typed `Reason` for audit (`ReasonOf`). Presenters only
ever see the one opaque `ErrRefused` (`Public`).

## Store and lapse rule

`Store` keeps one row per (domain, subject, client, ref) on a
`decisionstore.CAS` backend, table `_shoal_executor_attestations`. The row holds
the latest verified result: a content-addressed `AttestationID`, `VerifierID`,
verifier key digest, image and provenance digests, `IssuedAt` and `ExpiresAt`.

- Re-presenting the recorded statement is idempotent.
- A different statement must have a strictly later `IssuedAt`. Otherwise it is
  refused as a rollback.

`Current(ctx, principal, ref, now, needUntil)` is ok only if all of these hold:

- a row exists;
- its verifier is still configured with the same key;
- its digests are still pinned;
- its validity still fits the verifier's current `max_validity`;
- `now < ExpiresAt`;
- `ExpiresAt >= needUntil`.

Trust is read on every check, so removing trust takes effect for the next
check. A store failure is returned as an error, which callers map to
unavailable. It is never "not attested".

**A claim never outlives its attestation.** A claim or extension is granted
only if the attestation's expiry is at or after the new lease end (judged once,
in the fleet gate; the adapter calls `Current` with `needUntil` equal to now). A lapse then coincides with a lease lapse, and a worker
re-attests before it extends. Removing trust lets live claims run to their lease
end, but refuses new claims and extensions.

## Enforcement

### Requiring it

`fleet.Action.RequiresAttestation` (JSON `requires_attestation`, omitted when
false) marks an action whose claims need a current attestation. ATPL declares
it as `"attestation": {"required": true}` on an action (`docs/atpl.md`).

- It is accepted only on an action whose effects include `external`.
- `Register` refuses it when the host has no trust root for the descriptor's
  executor ref (`fleet.Config.AttestationTrust`; nil configures none).
- Delegation and re-registration may add it and never drop it
  (`capabilitiesSubset`).
- The v1 registry mutation digest appended the tag
  `shoal.fleet.requires-attestation.v1` only when it was set, so every earlier
  digest stayed byte-identical. The v2 digest (#521), which all new receipts
  carry, writes the requirement explicitly as a 0 or 1 field on every action;
  v1 is still computed only to reconcile a retry with a receipt written before
  v2. The durable descriptor codec writes version 5 only
  for a descriptor that requires it; an earlier build refuses version 5 rather
  than reading the action without the requirement.

### The gate

There is one gate, `attestationGate`, applied in `applyClaim` — the single
place a claim is written — and by `ExtendClaim` to its renewal. Callers read
the store (`DispatchService.claimAttestation`, through the
`fleet.ExecutorAttestations` interface, which `cmd/shoal-explore-web` adapts
from `internal/executorattest`) and hand the result in. A claim or extension
is granted only if the attestation is current and its expiry is at or after
the lease end the record will carry. For `ExtendClaim` that is the *clamped*
end (bounded by the action's deadline).

It applies wherever a claim is granted:

| Path | Unattested answer |
| --- | --- |
| dispatch `Claim` (Path A), and `Invoke` through it | `ErrAttestationRequired`: HTTP 409, code `conflict` |
| `ExtendClaim` | 409 as above; the claim is untouched and runs to its current lease end |
| admission grant (Path B, born claimed) | the existing durable `denied`, no reason, no admission wire change |

`CompleteClaim`, `ReportAmbiguity`, `Status`, `Cancel` and `ExecuteClaim` are
never gated. `ExecuteClaim` needs no re-check: an attestation cannot expire
while a claim it granted is live.

The read happens after every existing authorization, concealment and state
branch, so a caller without standing gets the same not-found whether or not
the action requires attestation, and never reaches the store. A store failure
answers `unavailable` (503, unmarked: nothing was written), never "not
attested". The 409 sits above the indeterminate arm in
`webapi.fleetDispatchError`; that is safe because it is a pre-commit refusal,
never joined with `ErrExecutionAmbiguous` or `ErrActionCommitted`. No refusal
names a verifier, digest, attestation ID or expiry.

Every refused claim, extension and admission grant is audited as phase
`claim_refused_attestation`. The audited record is a copy that names the
refused caller (claimant subject, actor, client and delegation chain, and its
request and correlation IDs); the copy is never stored. That phase's session
identity includes the refusing request ID, so two refusals at one record
version are two entries. If the audit cannot be recorded, the refusal stands.

**Stricter wins, independent of generation pinning.** The requirement is read
from the action's *current* registration (`resolveActionBinding` returns the
action from the stored descriptor, not from the record), ORed with the
record's own side — an attestation its current claim was granted under
(`effectiveClaimRequirements`; #486 can add approval to the same shape). So a
record enqueued before the requirement was registered is governed by it
whenever it resolves at all. Today generation pinning makes such a record stop
resolving; if #486 lifts pinning, the record is refused with
`ErrAttestationRequired` instead. Neither ordering grants an unattested claim.

**A live claim when the requirement is registered** is not revoked. It runs to
its current lease end — the end it was granted, already bounded by the action
deadline — and no extension is granted unless the holder is attested (the
extension itself would be clamped to the deadline, never to
now + `MaxActionClaimTTL`).

An attestation is for the executor process that presented it as itself. A
claimant acting on behalf of others (a non-empty delegation chain), or one
without a client ID, is never attested.

### The record

`ActionRecord.ClaimAttestationID` names the attestation the current claim (or
its latest extension) was granted under. It is claim-scoped — a re-claim moves
it, and the outgoing holder's ID is kept in `ClaimHolder.AttestationID` — and
it belongs on #461's mutable list with the claimant fields, outside the
store's immutability invariant (#525). It is bounded at 128 bytes where it
enters the fleet, not only in `Validate`. Records written before it decode
with it empty, and an earlier build reads records carrying it (gob ignores the
field).

### Presenting

`POST /api/v1/fleet/executors/attestation` with
`{"executor_ref", "idempotency_key", "report"}` (key and report are unpadded
base64url; the report is the signed envelope byte for byte) answers
`{"attestation_id", "expires_at"}`, or for any verification failure one opaque
`unauthorized` "attestation refused". The caller needs `OperationExecute`,
gated exactly as the other execute routes (including the correlation ID,
which the built-in authenticators mint or take from `Shoal-Correlation-ID`
since #527). The only credential that holds `OperationExecute` is one minted
by the executor mapping (#391; `docs/effects-gateway-deploy.md`, "Issuing
executor credentials"). The development principal holds no execute, has no
client ID and can never be attested. The row key comes from the authentication
decision; the body cannot name a principal. Every presentation is audited
(`attestation_presented`, or `attestation_refused` with the typed reason); an
accepted one is audited before it is stored, and a refused one stays refused
if its audit fails. `pkg/attestation/api` (standard library, `pkg/shoal` and
the standard-library-only `pkg/executorref`, on the import-boundary allowlist)
is the wire contract and client; `sdk.New(...).Attestation()` returns it.

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
A presentation naming a reference the rule refuses is
`invalid_argument` before verification, and `attestationapi.NewRequest`
refuses it on the client.

Note for Path B: an admission caller authenticates under invoke, but presents
under execute. The same (domain, subject, client) must hold both.

### Executor credentials

A credential minted by the executor mapping is bound to one executor
reference, and presentation requires `executor_ref` to equal that binding
(#573). The statement's principal is the minted identity:
- domain: the workspace authorization domain;
- subject: `oidcexec:<issuer>#<sub>`;
- client: the same identity.

Here `<issuer>` is the mapping's issuer and `<sub>` is the projected token's
raw subject, for example `system:serviceaccount:shoal-gateways:stripe`. A
statement signed for any other subject, client or reference is refused. The
subject does not depend on which pod or token presents, so a rotated
projected token keeps presenting under the same principal.

### Host configuration

`-fleet-executor-attestation` (or `SHOAL_FLEET_EXECUTOR_ATTESTATION`) names
the trust file. Every ref in it must already be bound by
`-fleet-external-executor-refs` or `-fleet-external-egress-executor-refs`.
Without it nothing is composed: attested actions cannot register and the route
is not mounted.

### Policy flip

Registering the requirement is a generation change, like approval. Until #486
lands, flip it with no live claims: records queued or claimed under the old
generation stop resolving. That stranding is the heartbeat/generation issue,
not attestation; the requirement itself affects only new claims and
extensions.

## Residual trust

As with collector statements, a verified statement proves only that the holder
of an operator key vouched for these bindings. It does not prove what code
runs. The operator's signing process and key custody are trusted. Hardware and
code-signing verifiers are deferred.
