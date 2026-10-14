# Collector-backed decision request registration

Decision evaluation currently consumes an immutable registered request. The next admission layer connects already enrolled collectors to operator-approved task profiles, while preserving exact source bytes and separating registration from execution authority.

## Intended public boundary

A registration selects an operator-provisioned immutable profile and registered observation IDs, with bounded original artifact bytes. The profile pins the task, evidence policy, builder/preprocessor, model release, source-resource policy, and the explicitly permitted collector acquisition modes (`imported` and/or `server_observed`). The server derives subjects, citations, picture, numeric input and request identity. Caller-selected observations define a measured selected-source set; they do not prove repository, PR or service completeness.

The collector registry retains artifact references, not original bytes. Admission must compare supplied bytes against the recorded artifact digest and size and retain exact originals through `decisionartifacts`. Missing or mismatched bytes fail closed. It must not fetch collector-named URIs or substitute extractor payloads for original artifact content. Neither code comments nor observation payloads can select execution policy or run a builder.

The initial material resolver accepts only the current enrollment. It validates collector, generation, exact enrollment, stored row domains, current quarantine and the original artifact reference. Superseded enrollment records are unavailable through this boundary until a trustworthy historical index exists. A mandatory authority separately checks source access, task purpose and combined disclosure; domain-level Read permission alone is insufficient.

## Durable preparation and readiness

The internal registration primitive freezes a preparation under the authenticated domain, original attribution and idempotency key. Its request lookup uses domain, subject and request ID, matching the existing artifact catalog's namespace. Neither namespace confers authorization.

Preparation retains the exact existing canonical artifact-record bytes and their digest before publishing the primary descriptor and request alias. The descriptor pins profile revision, builder, selected-source commitments, derived identities, original authentication fingerprint and accepted times. A interrupted write may leave an orphan preparation blob or a preparing registration. Exact retries reconcile those states; no timeout, deletion or invented rollback converts them into a successful ready request.

Readiness records exact durable artifact retention, not perpetual authority. A mandatory trusted guard validates the original record and catalog retention before the ready transition and on ready retries. The orchestration layer must perform a final joint current authorization check after all store IO before returning or serving the request. Storage success followed by denial still does not imply rollback.

Use separate catalog authority paths: the admission instance may recognize a preparing request solely to retain its original bytes; the execution instance requires a ready request. General `LoadAuthorized` or evaluation must not gain access merely because a preparing alias exists.

## Retry constraints

The existing catalog requires the current admission fingerprint to equal the fingerprint frozen in the picture. If grants change after preparation, an old pending request may no longer be retainable through that admission path. The first integration must fail closed, preserve its preparation and require a new key for a newly authorized request. It must not refresh the old request's timestamps or pins, forge the old decision, or replace its source/model identity. Delegated callers are rejected until a separate delegation purpose and source-authority contract exists. A future recovery API would need an explicit authority contract for retaining the exact original request under refreshed current rights.

An already retained ready request is still subject to current source/task permissions and quarantine on every read and execution. Valid hashes and durable bytes do not override revocation, establish source truth, grant training permission or permit promotion.

## Delivery boundaries

The preparation store and canonical codec helpers are internal building blocks. They do not supply the operator profile registry, source-purpose authority, builder integration, public registration endpoint or deployed host wiring. Those layers require integration tests with real collector enrollment, exact source bytes, the existing decision HTTP API, engine restart and durable receipt replay.

Required adversarial cases include digest/size substitution, collector-generation and enrollment changes, revocation during IO, preparing requests reaching execution, changed profile selection under one key, lost acknowledgements between each durable stage, refreshed grants during retry, and quarantine after successful inference. The source and review exemplars should share these contracts while retaining full-review fallback until operational evaluation justifies a change.

## Internal service integration

`internal/decisionregistration.New` accepts an immutable host profile set, a trusted deterministic builder for each profile, an actual authentication resolver, the collector material resolver, the preparation store and an artifact backend. Its mandatory `AccessAuthority` remains responsible for current profile/source-purpose and joint-disclosure authorization. Profiles are host-provisioned; callers select an explicit profile ID and revision. The effective profile configuration is committed into the derived selected-source inventory identity.

`Register` accepts the profile selection, up to 64 registered observations and at most 8 MiB of original bytes. It checks original artifact digest and size, uses the collector Source mapper, and derives the picture and request with the actual caller fingerprint and server times. The initial builder contract retains one subject per selected observation, including explicit unsupported dispositions. It pins tokenizer and ontology-projection identities in the profile. Selected-input measurements must not be presented as complete repository or service coverage.

`Read` returns the original ready registration under current authority. `Artifacts()` is the ready-only facade for the existing decision service. Per-call private catalog instances keep preparation admission separate from execution. Full canonical retained record commitments are checked inside the catalog's verifier; an outer final joint authorization check follows all catalog and registry IO. No shared mutable authorization session is reused between calls.

Every later read reruns the trusted builder against retained originals at the original cutoff and compares the derived record commitments, including each source's acquisition mode. Changed resulting bytes, mode, or profile configuration under the same revision fail verification; use a new immutable revision for intentional changes. The host must keep the trusted builder implementation pinned; these checks do not independently attest its executable. Retrying may rerun that builder for validation, but never replaces the frozen registration or reruns committed inference. Candidate comments, extractor assertions and proposed labels remain source data, with no route to replace profile policy or model identity.

Profiles have no implicit acquisition-mode default. Stored pins without a recognized mode fail closed; they are not silently interpreted as imported or server-observed. Any migration must recover the authoritative mode from collector history before rewriting a registration.

Integration tests use real collector enrollment and row-CAS storage, source/review documents, a hand-authored local linear CPU model, engine/RFile restart and durable decision replay. The model runs once on the initial inquiry and zero times for its committed replay. These tests establish operational conformance, not trained-model accuracy, calibration or savings. Public registration HTTP/SDK and deployed profile administration remain separate work.
