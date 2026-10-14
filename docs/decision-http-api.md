# Authenticated decision HTTP API

This transport lets an external agent evaluate an **already registered, immutable
request** and retrieve its durable receipt. It composes the existing decision
service with Explorer authentication and exposes a public Go client at
`github.com/phrocker/shoal-oss/pkg/decision/api`.

The host must construct and mount the service explicitly; adding these packages
does not enable an endpoint in existing deployed binaries. Public picture,
request, model, outcome, and collector registration remain future work under
#418 and #446. The frozen code showcase supplies trusted local registration,
not a production source authority.

## Host integration

Within a Shoal host, construct `internal/decisionservice.Service` with its trusted
artifact catalog, registered providers, receipt store, and authentication
resolver. Wrap it using `internal/decisionhttp.New(service)`, then call
`handler.MountDecisions(adapter, authority.Resolver())` on an Explorer handler
created with `webapi.NewAuthenticatedHandler`. The handler's binder and service's
resolver must belong to the same `auth.Authority`. Complete mounting before
serving requests. Mounting on an unauthenticated handler is rejected.

Keep the host's verified authenticator and host allowlist. The service must check
current access to the task and **every contributing source** on execution and
historical reads. Content hashes and saved authorization fingerprints do not
replace those checks. The public `api.Provider` interface permits another host
adapter, but does not implement source authorization itself.

The routes reject cross-origin browser requests, query parameters, encoded path
aliases, caller identity/grant fields, and arbitrary evidence/model inputs.
Non-browser clients may omit Origin and Fetch Metadata. An Origin, when present,
must match the request's scheme and Host; do not infer trusted scheme or identity
from caller-supplied forwarded headers. Responses use `Cache-Control: no-store`.

## Protocol

| Method and path | Body | Result |
| --- | --- | --- |
| `POST /api/v1/decisions` | `{"request_id":"<encoded ID>"}` | Evaluate or replay the original receipt |
| `GET /api/v1/decisions/<encoded ID>` | Empty | Read without recomputation |

Both require authenticated access and one `Idempotency-Key` header. IDs and keys
use canonical, unpadded URL-safe base64 (`api.EncodeID`, `api.EncodeKey`). Decoded
IDs are nonempty UTF-8, bounded by `shoal.MaxIDBytes`; keys are bounded nonempty
bytes. The SDK accepts decoded IDs and keys. Receipt IDs themselves retain their
original `receipt:<hex>` representation. Reuse the **same request and key** for
retry or reconciliation. A different key represents a different reservation.

POST requires `Content-Type: application/json` (optional UTF-8 charset) and a body
of at most 4096 bytes with exactly the `request_id` field. Duplicate fields,
case aliases, nulls, unknown fields, and trailing JSON are rejected. No content
encoding is accepted. GET accepts no body. Responses are bounded to 8 MiB and
may have a smaller host workspace limit.

The schema-1 response is defined by `api.Response`: `schema`, `receipt`, and an
optional `ranking`. The receipt binds request, task, picture, predictor, version,
state, and timestamps. A committed receipt also carries its prediction identity
and typed result. A pending receipt has no prediction or ranking. No source text,
execution claim, or caller authorization material is returned.

`200` means a committed receipt; `202` means a pending reservation, **not a
completed decision**. A committed result can still be failed or abstained.
Ranking scores are inspection priorities; they do not establish calibrated
safety probabilities. All subjects remain candidates for full review. This API
does not authorize a risky operation or enable code exclusion.

Errors are flat JSON containing `code`, `message`, and optional `indeterminate`.
Invalid requests, authentication failures, absent/inaccessible records, conflicts,
and service failures use sanitized errors. After entering evaluation, an error
can follow a successful commit, including a final source-access revocation.
Such errors return `503`, `indeterminate: true`, and
`Shoal-Commit-Outcome: indeterminate` without disclosing the result.

An uncertain response does not prove a commit or its absence. Keep the same
request and key, retain the conservative full-review fallback, and reconcile
using an authorized read. Continued revocation may prevent reconciliation. The
SDK preserves uncertainty for lost, malformed, oversized, or substituted POST
responses and does not automatically retry evaluation or follow redirects.

## Runnable external client

Obtain a registered request ID and a stable key from your workflow. Supply a
token accepted by the host's authenticator through `SHOAL_TOKEN`, then run:

```sh
go run ./examples/decision-client -url https://shoal.example \
  -request "$REGISTERED_REQUEST_BASE64URL" -key "$IDEMPOTENCY_KEY_BASE64URL" -evaluate

# Read the original receipt using the same identity and key.
go run ./examples/decision-client -url https://shoal.example \
  -request "$REGISTERED_REQUEST_BASE64URL" -key "$IDEMPOTENCY_KEY_BASE64URL"
```

The example imports only public packages, sets a 30-second deadline, and reports
uncertainty without inventing a new key. A production caller can supply its own
token-refresh callback and HTTP transport to `api.NewClient`. Custom transports
must preserve TLS verification and must not add automatic mutation retries.

## Conformance evidence

`cmd/shoal-frozen-code-replay/http_integration_test.go` exercises the public client
through the mounted HTTP handler, real decision service, CPU provider, artifact
catalog, and engine-backed receipt store. It checks authorized replay/read,
another principal, source revocation, caller grant injection, and reopening the
engine with zero additional inference calls. Unit tests cover strict decoding,
response bindings, output bounds, cancellation, and uncertain writes.

These tests establish transport and persistence behavior. They add no new model
quality measurement or training evidence; frozen V11 and full-review fallback
remain unchanged.
