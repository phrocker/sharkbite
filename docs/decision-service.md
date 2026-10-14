# Authenticated decision service composition

`internal/decisionservice` composes the decision contracts and receipt store for
#403/#418. Its `Evaluate` and `Read` methods consume an already registered,
retained request ID and idempotency key. The caller cannot supply identity,
weights, runtime overrides or replacement evidence through these methods.
This is an internal service, not yet an HTTP endpoint or a deployed classifier.

## Authorization and execution

The service resolves an `auth.Decision` through Shoal's trusted resolver and checks
operation permission before artifact lookup. Both methods reject malformed
idempotency keys before artifact lookup, independent of request existence. Evaluation requires `invoke`; receipt
reads require `read` on the registered task resource. Artifact authorization is
additional: the mandatory `Artifacts.LoadAuthorized` integration must enforce
current access to every contributing source, anchor and outcome, validate retained
bytes/provenance and return the original bounded serialization and policies.
Source/policy grants or a historical fingerprint alone are insufficient to
implement that integration. The trusted task registry supplies task-resource
mapping; source text never supplies an invocation policy.

The service independently validates the request/policy bindings and exact input
digest. New evaluation also requires the original authorization projection to
match the current decision. Historical receipt reads may use changed grants if
the artifact integration verifies current access to all original evidence. Missing
and revoked artifacts produce the same not-found response; backend outages remain
availability failures. Resolver implementations must revalidate current authority;
a static/context-bound resolver alone does not discover external grant revocation.

Receipt scope includes authorization domain, subject, actor, client and delegation
chain. Mutable grants and expiry do not create a fresh idempotency namespace.
The store reserves before any provider resolution or invocation. A pending retry
returns its receipt without a claim or ranking; a committed retry returns the
original prediction and a deterministic ranking without invoking the provider.
An indeterminate reservation never grants permission to call a model.

Evidence eligibility runs before inference. If any requested subject is ineligible,
the service records whole-request abstention without resolving a provider. Otherwise
it resolves exactly one registered release/predictor pair and validates the runtime
identity. The worker receives a copy of the exact serialized input. Provider
completion time is stamped by the service; returned request/model/device identities
and answer membership are validated without repairing substituted values.

Invocation deadlines are bounded by the request, current authentication, configured
call limit and lease settlement margin. Registered providers must honor context
cancellation and implement their own concurrency, egress and runtime isolation
limits. The service does not hide an uncooperative worker in an unbounded goroutine.
Hosts must also bound request/catalog/registry latency through the request context.
Provider transport errors and malformed output become fixed-code failed receipts;
raw exception text is not returned. Service-generated failures omit unverified
effective-device claims. Cancellation is checked at entry, around registry lookup
and immediately before provider invocation; providers must still handle cancellation
that races with invocation. No hosted fallback is selected implicitly.

Access is checked again after reservation/registry work, after inference and after
commit or receipt lookup. Revocation during inference can leave a pending record;
revocation during commit can leave a committed record whose payload is withheld.
Neither case is described as rollback or a clean negative. Post-commit current
access is required before returning the receipt or ranking. Artifact and storage
reconciliation are still subject to the caller context: canceled requests may
remain pending and need later authorized reconciliation.

## Tested integration and remaining work

Tests use Shoal's real engine/receipt store, trusted auth binder/resolver, a test
artifact catalog and a deterministic fake provider. They cover unbound identity,
missing invoke permission, denied/revoked artifacts, revocation during inference
and commit, pending/committed retries, provider failures/substitution/timeouts,
input digest substitution/copy isolation, changed grants, caller scope identity,
indeterminate reservations and typed-nil dependencies.

The [durable artifact catalog](decision-artifacts.md) now supplies retention and
rehydration with mandatory current authority checks. Its service integration test
replays a committed result after engine restart without reinvoking a model. A
production task/evidence authority, request-registration path and registered
local-model adapter remain outstanding. Tests use a fixed test registry and fake
provider; they do not establish real-world provenance verification. There is no
public path that can omit the trusted dependencies and still invoke a model.

Remaining #403 work is production authority/registration integration, attributed
append-only outcomes, adjudication permissions and lifecycle/audit integration.
#418 still needs HTTP/SDK schemas and host wiring. The code-only classifier and
other models remain offline showcase implementations until their pinned provider
adapters are installed. No training, model promotion or paid inference occurs here.

Validation:

```sh
go test -race ./internal/decisionservice ./pkg/decision ./internal/decisionstore ./pkg/explorer/auth
go vet ./internal/decisionservice ./pkg/decision
```
