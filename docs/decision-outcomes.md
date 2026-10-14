# Attributed outcome observations

Outcome observations connect a retained prediction to a reported event without
turning that report into a verified training label. `pkg/decision` defines the
immutable observation; `internal/decisionoutcomes` admits and retains it through
trusted authentication, a mandatory evidence authority, and the engine's row-CAS
store. Records follow the engine's WAL/RFile persistence path.

There are two distinct kinds:

| Kind | Assertion | Meaning |
| --- | --- | --- |
| Execution | Action reference and succeeded/failed/unknown status | A reporter claims an execution result |
| Correctness | Subject/question and task label, or observed proposition truth | A reporter proposes what the answer should be |

Execution success does not imply correctness. Unknown execution does not become
a negative label. A correctness report does not become verified because it names
a trusted reviewer, agrees with a model, or has been submitted many times.

## Content and attribution

An observation binds its request, prediction, subject, evidence IDs, asserted
observation time, and optional predecessor receipt. The constructor checks the
request/prediction binding, subject and question membership, and the task's answer
type. Choice and ordinal observations use registered labels. A proposition
observation asserts true or false rather than borrowing a model's confidence.

The content identity includes the complete canonical assertion. Constructors
copy mutable input, normalize timestamps, reject duplicate evidence, and bound
input sizes. These structural checks do not authenticate evidence or establish
truth.

`AssertedProvenance` holds reporter/model/prompt/tool references supplied by the
caller. The store derives the submitter from its trusted resolver and stamps the
receipt time using its own clock. The caller cannot set either authenticated
field through the append operation. Client observation time must not lie after
receipt time. Historical assertion time is retained as an assertion, not treated
as proof that a label was available for a historical evaluation.

All retained observations are **proposed**. There is no verified/adjudicated
state transition or dataset-admission flag in this slice. Offline training
producers must not map these records directly to `verified` or
`training_allowed`. Independent adjudication, conflict-of-interest policy,
temporal eligibility, duplicate provenance accounting, and an authorized
dataset exporter remain prerequisites for learning from these records.

Authentication identifiers are opaque bytes. Storage preserves their exact bytes
separately from the UTF-8 identifiers used in decision content; JSON replacement
characters must not change authenticated attribution during persistence.

## Trusted authority boundary

The store requires an authority that resolves the exact registered prediction,
checks the registered label policy's reporting/read permissions, and verifies
current access to every original and newly cited input. Verification must cover
joint disclosure restrictions as well as individual grants. Hashes and the
original prediction's authentication fingerprint are insufficient.

After reading correction ancestry, the store passes the complete bounded chain
to one authority verification call. The authority must check the combined inputs
under its current policy. Verification happens after the final history read so
revocation during that read cannot bypass the disclosure check. No production
authority is inferred from the fixture's simpler single-source rules.

The authority must resolve action references against retained execution evidence
when admitting an execution report. Naming an action is not proof that it ran.
The observation store introduces no dispatcher and grants no execution rights.

Existing decision receipts and artifacts remain scoped to their authenticated
principal. Independent adjudicators will need an explicit, separately authorized
resolution contract; this store does not broaden historical decision reads to
make cross-principal adjudication possible. Tests use a trusted fixed registry
and the real source catalog to demonstrate the boundary. They do not supply a
production evidence/adjudication authority.

## Durable append and correction

An authenticated domain/principal/request scope and caller idempotency key select
one immutable receipt. An exact retry returns its original attribution and
receipt time; a changed observation under the same key conflicts. Retrying under
another key does not establish independent corroboration.

Corrections append an observation naming the prior receipt in `Supersedes`.
The prior record remains retained. Correction links must stay within the same
scope and prediction/subject/question/kind; execution corrections also preserve
their action target. Current authorization applies to contributing ancestry.
Cycles and excessive ancestry are rejected. Competing corrections are preserved;
there is no implicit last-writer-wins adjudication.

Authorization is checked before and after storage. A mutation may have persisted
even if final authorization fails. The caller receives no inaccessible record,
and an indeterminate result remains indeterminate until an authorized read or
exact retry confirms the receipt. Revocation may prevent that reconciliation.
Storage errors never justify fabricating a rollback or a negative outcome.

The new outcome record family has its own versioned encoding. Existing prediction
receipts, retained picture/source artifacts, and graph/co-occurrence formats are
unchanged. Public outcome HTTP/SDK routes and production registration are future
integration work under #418; this slice advances #403 and #419.

## Verification

The store tests exercise competing exact/conflicting submissions, acknowledgement
loss, cancellation and revocation during mutation, immutable corrections and
bounded ancestry, corrupt records, and physical RFile flush/reopen. The source
showcase integration uses the real prediction service and retained source catalog
to check separate reporting permission, asserted versus authenticated reporter
identity, unknown evidence rejection, correction retention, and source revocation
without another inference call.

```sh
go test -race ./pkg/decision ./internal/decisionoutcomes ./cmd/shoal-frozen-code-replay
go vet ./pkg/decision ./internal/decisionoutcomes ./cmd/shoal-frozen-code-replay
```

These are boundary and persistence tests. They measure neither classifier quality
under poisoning nor adjudicator accuracy; no model is trained or promoted by this
slice, and full-review fallback remains enabled.
