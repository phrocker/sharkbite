# Authenticated outcome HTTP and Go SDK

A host explicitly mounts `Handler.MountOutcomes(provider, resolver)` with its
trusted authenticator and binder. `internal/decisionoutcomehttp.New(store)` adapts
a configured outcome store. The store's source/reporting authority and optional
durable inventory admission coordinator remain mandatory host decisions; there
is no permissive production authority or public source registration here.

The Go SDK exposes `AppendOutcome(ctx, observation, key)` and
`ReadOutcome(ctx, requestID, predictionID, key)`, also reachable through the SDK
facade's `Decisions()` client. Go DTO identifiers are decoded `shoal.ID` values.
Wire codecs use canonical raw URL base64 for content IDs and opaque authentication
bytes. The receipt ID itself retains its `outcome-receipt:<hex>` form. Asserted
reporter/model/prompt/tool provenance stays separate from authenticated attribution.

## Protocol

- `POST /api/v1/outcomes` accepts schema 1 with an `observation` object and returns
  the original proposed receipt. Requests are bounded to 2 MiB.
- `GET /api/v1/outcomes/{encoded-request-id}/{encoded-prediction-id}` returns that
  principal's original receipt, with no body. It performs no admission or repair.
- Both require a bearer token and canonical `Idempotency-Key` header. Responses
  are bounded to 4 MiB and marked `Cache-Control: no-store`; host workspace limits
  may be smaller. Success is HTTP 200, including exact POST retries.

Use `EncodeOutcomeRequest`/`DecodeOutcomeRequest` and
`EncodeOutcomeReceipt`/`DecodeOutcomeReceipt` for the strict envelopes. Unknown or
duplicate fields, null aliases, malformed IDs, incompatible outcome shapes and
invalid times are rejected. Evidence IDs and observed times are normalized before
binding a successful POST response to the complete submitted observation.

The wire supports correctness and execution reports. A host may allow a narrower
set; the guarded training-inventory showcase admits correctness reports only.
Every accepted report remains proposed. Execution success and reporter attribution
do not establish correctness, independent adjudication or training eligibility.

## Recovery

Keep the exact key and normalized report together. Every failure after invoking
Append is conservatively returned as HTTP 503 with
`Shoal-Commit-Outcome: indeterminate`, including final permission or response
failures. `errors.Is(err, api.ErrIndeterminate)` exposes that state in the SDK.
The SDK never automatically replays POST, follows redirects or sends cookies.

A pending inventory intent may exist even when the outcome write never completed.
Conversely, GET may return an original committed receipt while its inventory
publication is still pending. GET success or absence cannot settle publication.
Retry the exact POST/key/body under the same authenticated principal/delegation
to reconcile it. The original receipt time and authorization attribution survive
exact retries and grant refreshes. A changed body is a conflict, not a correction;
use a new key and an authorized `Supersedes` reference for a correction.

## Example

Set `SHOAL_BEARER_TOKEN`, then run
`go run ./examples/decision-outcome-client --base-url https://HOST --key STABLE_KEY
--observation-file report.json`. The file contains the schema-1 request produced
by `api.EncodeOutcomeRequest`. For a read, replace `--observation-file` with
`--request-id REQUEST --prediction-id PREDICTION` using decoded IDs.

Real source/review integration tests exercise POST, GET, exact replay, reporter
isolation and shared inventories, automatic stale-label quarantine, publication
outage, read-only GET during that outage, exact POST repair and source revocation.
