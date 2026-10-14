# Authenticated adjudication HTTP and Go SDK

A host explicitly mounts `MountAdjudications(provider, resolver)` on an authenticated Explorer handler. `internal/decisionadjudicationhttp.New(service)` adapts the existing adjudication service; that service requires a trusted evidence authority, retained bases and a durable journal. Mounting this API supplies no permissive authority or automatic training eligibility.

External clients use `pkg/decision/api.Client` (also available through `pkg/sdk.Client.Decisions()`). `Adjudicate(ctx, proposal, key)` submits a proposed disposition. `AdjudicationHistory(ctx, targetID)` retrieves the complete currently authorized target history. These operations support source and review tasks through the same general contracts. The runnable `examples/decision-adjudication-client` accepts either `--proposal-file` plus `--key`, or `--target-id`, with `SHOAL_BEARER_TOKEN` authentication.

## Protocol

- `POST /api/v1/adjudications` takes a schema-versioned proposal and a canonical raw-URL-base64 `Idempotency-Key`. The caller supplies request/prediction/subject/question references, observation receipt and witness references, disposition, label or truth, reason, and expected head/version.
- `GET /api/v1/adjudications/{encodedTargetID}` takes a canonical raw-URL-base64 target ID and no idempotency key. It returns the complete history or an error, never a partial authorized prefix.
- Use the public `EncodeAdjudicationRequest`, receipt and history codecs for exact wire representation. Authentication attribution uses encoded original bytes, including opaque identities that cannot safely be represented as UTF-8 strings.
- The host resolves the pinned policy, roles, evidence, inventory completeness and current source permissions. Clients cannot supply bases, role grants, completeness assertions, authenticated attribution or training permission.

Receipts retain the original proposal, immutable basis reference, target/task/picture/policy/proposal identities, journal version, authenticated adjudicator attribution and server receipt time. The API does not disclose hydrated evidence bases. A verified disposition records admission under the host's authority; training export still separately checks current permissions, inventory and eligibility.

## Recovery and history

Any failure after invoking adjudication is conservatively reported as HTTP 503 with `Shoal-Commit-Outcome: indeterminate`. The Go client returns `api.ErrIndeterminate`. A basis or journal entry may already be durable; this error does not mean rollback. Transport errors, malformed successful responses and response substitution after POST are also uncertain. The SDK does not automatically replay requests or follow redirects.

Retry with the exact original key and proposal, including its original expected head and version. An authorized retry returns the original receipt even when the journal has advanced. Changing the expected head changes the proposal; it is not recovery of the original operation.

History is read-only, does not repair or perform adjudication, and never reports write uncertainty. The service rechecks the entire retained chain and current combined disclosure authority before returning it. Revocation, missing retained evidence or bounds failures return no partial history. An absent history is not a claim that the target has an approved label.

## Limits and verification

Requests are bounded at 2 MiB, single-receipt responses at 4 MiB, and history responses at 32 MiB. A history contains at most 128 receipts. Lower host response budgets may reject an otherwise valid history as a whole. Reference counts, text, identities and delegation are separately bounded. Histories must bind the requested target, contiguous versions, predecessor chain, stable task/picture/policy and subject/question identities, and nondecreasing receipt times. Public codecs perform structural validation; they do not independently authenticate the host or reconstruct core content hashes. The trusted service validates those identities and evidence authority.

The source and review fixture integration tests exercise authenticated submission, original retries after the head advances, whole-history reads, role denial, stale-head rejection and source revocation through the real service, journal, retained inventory bases, HTTP handler and SDK. These synthetic tests establish transport and authority conformance; they do not establish operational model accuracy or independent human ground truth.
