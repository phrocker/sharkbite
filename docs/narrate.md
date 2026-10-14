# Rendering records as text

`pkg/narrate` renders Shoal records as English sentences, deterministically and
without a model (#498). It is the renderer described in
[local-language.md](local-language.md#renderer-records-to-text). It narrates:

| Record | Function | What it says |
| --- | --- | --- |
| `fleet.ApprovalStatus` | `Approval` | the effective state, the stored state when it differs, the condition, the history, what the request is waiting on, what can happen next |
| `fleet.ActionRecord` | `Action` | the dispatch state, the error code of a failure, evidence returned, what the record cannot establish, how it got here, what blocks the next step, what can happen next |
| `[]fleet.ActionTransition` | `ActionHistory` | one sentence per committed transition, with consecutive claims aggregated |
| `decision.PredictionRecord` | `Prediction` | the result, why a whole request abstained or failed, each answer, the picture's evidence gaps, the next step |
| `decision.EvidenceEligibility`, `decision.InspectionRanking` | `Eligibility`, `Ranking` | which subjects may be scored, and every reason a subject is held for ordinary inspection |
| `interaction.Session` | `AssertedReason` | the caller-asserted reason, quoted and attributed |
| `router.Proposal` | `Proposal` | what the router proposes (action, decision or lookup, with its slots) or why it abstained, always stating that nothing ran, and what a caller would do next |

Nothing else is narrated. The package reads no clock, network or model; a
narration is a function of the record and `Options`.

## Output

Each function returns `[]Sentence`:

```go
type Sentence struct {
    Role     Role     // outcome, reason, gap, history, blocked, next, detail
    Key      string   // the catalog message it was realized from
    Text     string
    RecordID string   // "<kind>:<id>"
    Refs     []Ref    // evidence, records and principals it was derived from
    Quotes   []Quote  // the untrusted spans inside Text, in order
    Spans    []Span   // byte offsets of every quoted span and bare identifier
}
```

Sentences are ordered by role, which is the content-selection rule: what
happened, why, what is missing, how it got here, what blocks the next step,
what can happen next, then quoted detail. Lists are capped
(`Options.MaxListItems`, then "and N more"), and so are a prediction's
per-answer sentences (`Options.MaxAnswers`).

`RecordID` and every `Ref.ID` are plain tokens (`[A-Za-z0-9]` then
`[A-Za-z0-9._:@/+-]`, ending alphanumeric). An identity that is not one, such
as caller-chosen action-ID bytes, is written as `hex:` and its bytes, so a
reference can be printed safely and still resolved.

A sentence that names a principal or a claim carries it as a reference: the
requester (`principal`, `request`), the approver (`principal`, and the
decision's `request`), the claimant (`principal`, `claim`), the decision
requester (`principal`, `release`, `correlation`) and the asserting caller.

`Spans` marks every bare identifier (`identifier`) and every quoted span
(`quote`) by byte offset, so a reader can style record data apart from the
catalog's words — an agent named `approved` is then visibly an identifier.

## The message catalog

The catalog is data: [`pkg/narrate/catalog/en.json`](../pkg/narrate/catalog/en.json),
embedded at build time. A translation is another file of the same shape loaded
with `ParseCatalog`; no Go changes.

```json
{
  "locale": "en",
  "formats": { "time": "2006-01-02 15:04:05 UTC", "group": ",", "decimal": "." },
  "messages": { "dispatch.error.outcome_unknown.executor": "The failure was reported as …" }
}
```

Keys are `<record kind>.<part>.<outcome>[.<condition>]`, for example
`approval.outcome.unresolvable.target_moved`, `dispatch.error.target_rejected.service`
(one per error code origin) and `dispatch.error.target_rejected.next`,
`decision.reason.failed.deadline_exceeded`. Messages use a
subset of ICU MessageFormat, implemented in the package (no new dependency;
plural categories come from `golang.org/x/text/feature/plural`, already in
`go.mod`):

| Syntax | Value |
| --- | --- |
| `{name}` | a rendered fragment, number, time or duration |
| `{n, number}` | integer (grouped per locale) or a probability |
| `{t, time}` | always converted to UTC and formatted with the locale's layout; a zero time is "an unrecorded time" |
| `{d, duration}` | the two most significant units, truncated toward zero; a negative duration is refused, not printed as its magnitude |
| `{xs, list}`, `{xs, list, or}` | joined with the locale's list patterns |
| `{n, plural, =0 {…} one {# …} other {# …}}` | CLDR plural rules of the locale |
| `{s, select, a {…} other {…}}` | a closed-set selector |

Apostrophes follow ICU's optional-quoting rule: `''` is an apostrophe and `'{`
starts a quoted literal; any other apostrophe is literal.

A probability is rounded to three places unless that would print 0 or 1 for a
value that is neither; then it is printed exactly, so 0.9996 never reads as
certainty. Coverage counts are always exact `N of D`, never percentages.

## The coverage rule

Every outcome, error code, condition, state, transition and decision result has
a template. It is enforced three ways:

- **At load.** `ParseCatalog` refuses a catalog that lacks any key in
  `RequiredKeys()`, carries an extra key, has a pattern that does not parse, or
  reads an argument the English message does not read, or reads it as a kind
  the renderer does not supply. A plural is allowed only where English reads
  that argument as a plural, because `number` also carries probabilities and a
  float cannot select a plural form. Select arms are scoped too: a
  translation may read an argument only under every select arm English reads
  it under, because outside them the renderer omits it (`{left}` exists only
  when `timed=yes`) or passes a placeholder (a probability is 0 unless
  `reported=yes`). An argument English reads, with the same kind, in every arm
  of a select is not restricted by that select, so a translation may hoist it
  out (`{action}` in `approval.outcome.expired`). A refusal names the select
  arm the argument is restricted to, or the kind English reads it as.
  A translation therefore cannot fall back to an unreviewed
  sentence, fail at run time, or print a value the record does not hold.
- **Parity with source.** The vocabularies are read from their owners' source
  by AST, never from this package's lists: `DispatchState`, `ApprovalState`,
  `ApprovalCondition`, `ErrorCodeOrigin`, the kinds `NewActionTransition` accepts and every
  value assigned to an `ErrorCode` in `pkg/explorer/fleet` (literals and
  constants; any other expression fails the test, except a copy of another
  `ErrorCode` field); `ResultStatus`,
  `AnswerStatus`, `AnswerKind`, `Disposition` and `InspectionReason` in
  `pkg/decision`; the error-code constants of `internal/effectsgateway`; and the
  `terminal(…)` reasons of `internal/decisionservice`. The status table in
  [approval.md](approval.md#status-reports-the-effective-state) is parsed too,
  and so is `ApprovalService.effectiveState` itself: the (state, condition)
  pairs it can return, with `current.State` and the `unreachable` conditions
  resolved, must equal `EffectiveApprovals`. Every state fleet sets a record to
  must be the destination of a `DispatchEdges` edge, and every edge's kind must
  be the one `actionEventKind` gives its destination. Constants are read whether
  declared with their type (`X T = "x"`) or converted (`X = T("x")`).
  Every reader fails closed: a form it cannot resolve — a typed constant it
  cannot evaluate, a `return s.helper()` or a returned variable in
  `effectiveState` or `unreachable`, a `State` set from a variable, call or
  expression by assignment or in an `ActionRecord` (or type-elided) literal —
  is reported as a problem and fails the test rather than being skipped.
  Adding a value in any of those places without a template here fails
  `TestParity*` and `TestCoverage*`.
- **By rendering.** `TestCoverage*` renders a record for every value read from
  source — every dispatch state, with and without admission and at five
  clock positions; every effects-gateway code including `target_rejected_400`
  to `_599`; every fleet-written code, each under every `ErrorCodeOrigin` in
  source and under one no build writes; every dispatch transition edge; every
  approval status row from every stored state it can arise from; every result
  status, service reason, answer kind, abstention, disposition and inspection
  reason — and fails if any falls back. `TestCoverageEveryKeyIsReachable`
  fails if the catalog has a template nothing can say.

The effects gateway's closed `ErrorCode` set is mirrored as constants
(`GatewayErrorCodes`, `TargetRejectedStatus`) rather than imported, so this
public package does not depend on `internal/`; the parity test imports the
gateway and checks `ValidErrorCode` agrees on every candidate code.

Golden files under `pkg/narrate/testdata/golden` fix the wording. The
scenario goldens cover representative records; the value goldens under
`testdata/golden/values` (`TestGoldenValues`) render every value of every
enumerated set — each dispatch state, error code under each error code origin,
transition, approval status
and condition, decision result and abstention reason, disposition and
inspection reason — from the same source-read lists the coverage tests use, one
file per family. Coverage proves which key a value renders; the value goldens
prove the sentence under that key is the one written for that value, so
swapping the templates of two sibling keys fails. Change them with
`go test ./pkg/narrate -run 'TestGolden' -update` and review the diff.

### Values the renderer does not know

A fleet record is decoded from storage a newer build may have written, so an
unknown dispatch state, transition kind or approval state is rendered by an
`.unrecognized` template that quotes the value. A decision record can only be
built by `pkg/decision`'s validating constructors, so an unknown value there
means the two packages are out of step, and the renderer returns an error.

An `ErrorCode` outside the gateway and fleet sets is free text: an executor may
record any bounded string. It is quoted, attributed to whoever the record's
`ErrorCodeOrigin` says assigned it (`executor`, `service`, or
`executor_or_service` when the record does not say), and its next step is to
reconcile with the target; with `EffectPossible` clear, reconcile if the
action reached any external system (see "Whether the action declared an
external effect"). The same holds for a predictor's own whole-request
or per-answer reason.

### Who assigned an error code

Since #529 an action record says who decided its `ErrorCode`:
`ErrorCodeOrigin` (`error_code_origin` on the wire) is `service` when the
dispatch service adjudicated — it refused the executor's output, evidence or
error code, or the executor failed without a code — and `executor` when the
code is the executor's own account. Fleet derives it from the code path that
wrote the field, never from the request, and refuses the codes it assigns
itself (`invalid_executor_output`, `invalid_executor_evidence`,
`invalid_executor_error`, `executor_error`) when an executor reports them. A
record written before #529 has no origin.

The renderer keys **only on the origin**, never on the code: the reserved-code
list is closed, the origin field is not. Each code's reason has one template
per origin rendering, `dispatch.error.<code>.<service|executor|either>`:

| Origin | Reason | Example |
| --- | --- | --- |
| `service` | Shoal's determination, stated as such | "Shoal recorded the failure as invalid_executor_output, which means the executor’s output did not match the action’s output schema." |
| `executor` | the executor's account: a report, its meaning conditional | "The failure was reported as outcome_unknown, which, if accurate, means …" |
| omitted, or any value this build does not know | attributed to neither, its meaning conditional | "The failure was recorded as outcome_unknown, which either the executor reported or Shoal assigned; the record does not say which. If accurate, it means …" |

An omitted origin is never read as the executor's: a pre-#529
`invalid_executor_output` may be Shoal's adjudication, and attributing it to
the worker is the error #508 describes. An origin this build does not know —
a record from a newer build — renders exactly as an omitted one, so the
fail-safe claims nothing. `errorCodeOrigins` (vocab.go) records the decision for
each origin; `TestParityErrorCodeOrigins` reads fleet's `ErrorCodeOrigin`
constants from source, so a new one fails until it is given a row and its
templates are decided.

The origin is credited with the code, and only the code:

- **The outcome** reads "was reported as failed by {reporter}" only for an
  executor's code. Otherwise it reads "was recorded as failed at …, after
  {reporter} reported its outcome": under a service origin the executor may
  have reported *success* (its output then refused as
  `invalid_executor_output`), and the failure itself may still be the
  executor's (`executor_error` is Shoal's code for a failure reported without
  one), so the outcome names neither Shoal nor a reported failure.
- **The failing transition** reads "{reporter} reported failure with {code}"
  for an executor's code, "Shoal recorded the failure as {code} after
  {reporter} reported its outcome" for Shoal's, and otherwise says the record
  does not say whether the reporter or Shoal assigned the code.
- **The next step** is to reconcile with the target before requesting the
  work again, whatever the code and whoever assigned it. With
  `EffectPossible` clear and a code that is not the gateway's, it reconciles
  if the action reached any external system (see "Whether the action declared
  an external effect" below); no failure drops reconciliation. Three change with
  the origin, because their wording presumed who assigned the code:
  `request_not_sent` and `input_invalid` said "the report says nothing was
  sent, but the record cannot establish that". For an executor's code that
  stands; for an unknown origin it says "the code says", since it may not be a
  report; for Shoal's own code the clause is dropped, because it would call
  Shoal's determination a report and then doubt it. `unrecognized` said "Shoal
  cannot interpret the code", which is false if Shoal assigned it; under a
  service or unknown origin it says this renderer cannot interpret it. Every
  other next step means the same whoever assigned the code and is unchanged.

The service rendering is chosen by the origin even for a code fleet does not
assign today (a gateway code, say): if fleet ever records one as its own, the
sentence says so, and the parity tests on error codes flag the new
assignment.

### Whether the action declared an external effect

`EffectPossible` is set when a claim is taken, and only for an action that
declares `EffectMutatesExternal` or `EffectEgressesContent`. The store lets it
rise and never fall (#461), and since #538 completion carries it forward
instead of setting it on every completion (#510).

**A clear flag says what the action declared, not what happened.**
`ExternalEffectBinding` has no floor: a descriptor may declare less than its
binding permits, so a remote worker on a non-declaring action can still do
real work at a target. Every sentence a clear flag produces is therefore
attributed to the declaration and conditional on it, and a code on the record
that implies a target overrides it.

| State | `EffectPossible` | Code | Sentence | Next step |
| --- | --- | --- | --- | --- |
| claimed | true | — | "The action declares an external or egress effect and is claimed, so an effect may already have happened." (gap) | unchanged |
| succeeded, failed, canceled | true | any | "An external effect may have occurred; reconcile with the target." (gap) | unchanged: a failure, and a canceled record that was claimed, reconcile with the target |
| succeeded | false | none | `dispatch.effect.declared_none`: "This action declares no external or egress effect, so if that declaration is accurate its whole outcome is in this record." (reason) | unchanged (a success never advised reconciling) |
| failed | false | a gateway code (`request_not_sent`, `outcome_unknown`, `retry_exhausted`, `input_invalid`, `target_rejected_NNN`) | `dispatch.gap.effect_contradicted`: "The failure’s code implies an external target was involved, although the action declares no external or egress effect; the declaration may be wrong, and an external effect may have occurred." (gap) | unchanged: reconcile with the target |
| failed | false | anything else, fleet's own codes at any origin included | `dispatch.effect.declared_none_unsure`: "This action declares no external or egress effect, but a declaration does not establish what the action did, so it cannot show that the failure left nothing to reconcile outside this record." (reason) | "Reconcile with the target if the action reached any external system before requesting the work again …" |
| canceled | false | — | none | unchanged: decided by the claim fence |

Reconciliation is never dropped on a failure:

- **A gateway code contradicts the declaration.** The effects gateway assigns
  its codes only once a request to a target was being bound or attempted, so
  a page that said "target_rejected_409" and "no reconciliation needed" would
  contradict itself. Membership is the gateway's closed set
  (`GatewayErrorCodes` and `TargetRejectedStatus`), the same family
  `errorCodeKey` uses, not string matching.
- **Fleet's own codes are assigned after the worker acted.** In
  `applyExecutionResult`, `invalid_executor_output` follows a reported
  success whose output failed the schema (a gateway worker that got a 2xx
  ends there), `invalid_executor_error` overwrites a worker's own code that
  was malformed (`"target_rejected_409 "`, with a trailing space, say), and
  `executor_error` replaces a failure reported without a code. None of them
  says the work did not reach a target, whoever the origin says assigned it.
- **A code the renderer cannot interpret** cannot be placed either.

So a clear flag on a failure says only what the action declared, and
reconciles if the action reached any external system. Only a **success with
no code** says "its whole outcome is in this record", and only on the
declaration's condition. No failure says it even then:
`invalid_executor_output` discards the output and `invalid_executor_evidence`
the evidence, so neither failure's outcome is wholly in the record.

The rest of the rule:

- **A clear flag on a succeeded or failed record comes from #538 or later.**
  `ActionRecord.Validate` required the flag on both states from the first
  build that stored one until #538, so no era marker is needed. Never build a
  fixture with a succeeded or failed state and a false flag and call it
  pre-#538: it tests a shape the old code could never store.
- **True means "may", whatever the era.** Before #538 every succeeded or
  failed record read true, including actions that declare no effect, and the
  record does not carry a dispatched action's declared effects, so the two
  cannot be told apart. Both read "may", which is fail-safe. And since the
  flag is monotonic, a declaring action whose request demonstrably never left
  still reads true: the claim set it, and a worker could have acted at any
  time after.
- **No sentence says an effect happened, or that one could not have.** True
  is "may", never "did"; false is "declares", never "could not".
  `TestNoSentenceSaysAnEffectHappened` scans every rendered sentence for an
  unhedged affirmation and for an unconditional "could not have", "no
  external effect" or "no reconciliation".
  `TestNoPageBothContactsATargetAndDropsReconciliation` checks that no page
  carries a gateway code and "no reconciliation", that every failure's next
  step reconciles with the target, and that only a success with no code says
  its whole outcome is in the record.
- **The flag is never inferred.** Not from the declaration (an admission's
  admitted effects are on the record, but a declaration is not the flag), and
  not from the outcome. The code only ever widens it: `request_not_sent`
  with the flag set still reconciles.
- **A canceled record says nothing about a clear flag.** It may never have
  been claimed, so clear there says only that no claim set it; and claims
  before #396 did not set it for egress, so a canceled-after-lapse record of
  that era could read clear for an egressing action. A canceled record
  advises reconciliation when it was claimed before it was canceled, which the
  claim fence says.

`dispatch_effects` in the value goldens pins every (state × flag)
combination, and `dispatch_errors` pins every failure code under every origin
with the flag clear as well as set, including each gateway code with the flag
clear (the shape #541's review reproduced).

### Reports are not findings

The decision service writes its own whole-request reasons and passes a
predictor's through, and the result does not say which. Since #556 the
service's reasons (`decision.ServiceReasons()`) are reserved: a predictor that
returns one is adjudicated as `invalid_predictor_response`, so a build with
#556 never stores a service reason a predictor set. A result written before
#556 could carry one — nothing refused it then (#509) — and a result records
nothing that shows which build wrote it: no build version, and its
`ReleaseID` is the predictor's release, not the service's. So a whole-request
abstention or failure reason, a service reason included, is still attributed
to "the predictor or the service" (`predictor_or_service` on a quote), its
meaning is conditional ("if accurate"), and its next step starts "If …".
Stating the service fact outright would assert, for every historical record,
a finding the record cannot support. A per-answer abstention is the
predictor's own and is attributed to it.

A success, or a failure whose code the executor gave, is "reported as
succeeded (failed) by" whoever reported it: the claimant, or, for an admission,
the identity that requested it, which is also who an admission's history,
references and output quote name. Any other failure "was recorded as failed …
after" that identity "reported its outcome" (see above).

### Router proposals

`Proposal` narrates a `pkg/router` proposal (#500). The outcome sentence is
`router.proposal.<kind>` for an action, decision or lookup, or
`router.abstain.<reason>` for each abstention reason, and every one of them
says the proposal is only a proposal and that nothing ran: "Proposed, not run:
… Nothing has been executed, enqueued or decided", or "Nothing was proposed or
run". An action's sentence says when it would still require approval, a
decision's that its own predictor decides over its own evidence, a lookup's
that its template is answered over the graph. The next step
(`router.next.<kind>`, `router.next.abstain`) names the route a caller would
take; the router takes none of them.

A proposal holds no input text. Its target and slot values (agent,
capability, action, profile, template, node IDs, enum values) are registry or
grammar identifiers and are rendered as identifiers, quoted when they are not
plain tokens. The receipt is in the outcome's references: `proposal`,
`router`, `catalog`, `grammar_set`, and, when a target-choice decision was
made, `feature_schema`, `router_task`, `picture`, `decision_request`,
`predictor` and `prediction`. A proposal that does not validate
(`Proposal.Validate`: an unknown kind or reason, or an ID that does not match
its content) is refused with an error, as an unknown decision value is. Kinds
and reasons are read from `pkg/router`'s source by the parity test, and the
value goldens (`testdata/golden/values/router.txt`) pin the sentence of every
kind and every reason.

## State machines

`DispatchEdges` and `ApprovalEdges` are the transition tables. Each edge has a
history template (`dispatch.transition.<edge>`, `approval.transition.<edge>`)
and, if it leaves a state, a next-step fragment (`….next.edge.<edge>`).

- **What next** is the list of outgoing edges from the current state.
- **Why not yet** comes from the guard blocking an edge. With `Options.Now`
  set, the dispatch guards are evaluated as `pkg/explorer/fleet` evaluates
  them — completion needs a live lease, a re-claim or cancel needs a lapsed
  one, a claim needs the deadline ahead — and blocked edges become `blocked`
  sentences. Without `Now`, nothing time-dependent is asserted and each option
  states its condition. Approval conditions are their own guard: the condition
  is the reason and `approval.next.<condition>` the remedy.
- **Histories aggregate.** An `ActionRecord` carries no per-claim history, but
  its claim fence counts claims, and a re-claim is only possible after the
  previous lease lapsed, so "claimed 3 times; the first 2 leases lapsed without
  a report" is derived from the record. In `ActionHistory`, a run of
  consecutive claims becomes one sentence.

## Untrusted text

The renderer must not become an injection channel. Message arguments are typed:
a bare Go `string` is refused, so text reaches a sentence only through one of
three constructors:

- catalog text, numbers, times and durations;
- **identifiers** (agent, action and principal IDs, labels, units): shown bare
  only if they are short plain tokens, otherwise quoted;
- **quoted spans** for everything a caller, executor, target, predictor or
  evidence builder supplied: action input and output, unrecognized error
  codes, predictor reasons, subject reasons, caller-asserted reason codes and
  sources.

A quoted span is attributed in the sentence ("Input supplied by alice: “…”",
"alice asserted the reason code “…”; Shoal records this but does not verify
it") and in its `Quote` (`Attribution`, `By`). `By` is a principal ID from the
record, so it is escaped by the same rules and bounded to 64 runes. Inside it a backslash is
doubled; the quotation marks, the ASCII quote and the ellipsis are escaped;
every non-printable rune (controls, newlines, bidirectional and other format
characters, separators other than the ASCII space, private-use and unassigned
code points) and a combining mark at the start are written as `\u{…}`;
invalid UTF-8 is written as `\x…`. At most `Options.MaxQuoteRunes` (default
120, ceiling 2048) runes are shown, followed by an unescaped `…` and
`Truncated`.

Patterns are parsed once at load and values are inserted into the parsed tree,
so braces, `#` or apostrophes in a value are text, not syntax.
`TestUntrustedTextCannotAlterStructure` renders every untrusted field with
newlines, carriage returns, closing quotes, template syntax, bidi overrides,
zero-width and tag characters, separators, controls, invalid UTF-8, combining
marks, fake ellipses and escape look-alikes, private-use code points and
28 KB strings, and requires the same sentences, roles and keys, identical text
outside the quoted spans, no unprintable character anywhere (including in
`Quote.By`), spans that cut the text where they say, and token-only record and
reference IDs.

The renderer reads no clock. `TestNoModelNetworkOrClock` pins the package's
direct imports, refuses `time` imported under any other name, and resolves
selectors by import path to deny `Now`, `Since`, `Until`, `After`, `Tick`,
`NewTimer`, `NewTicker`, `AfterFunc`, `Sleep`, `LoadLocation`, `Local` and
`t.Local()`.

## Follow-ups

- Per-claim attempt history (claimant, lease, outcome per attempt) is not on
  `ActionRecord` on main; aggregation uses the claim fence. When attempt
  history lands (#438/#430), add an `ActionRecord` history sentence per
  attempt.
- Since #556 a predictor cannot set a service reason, but a decision result
  still carries nothing that tells a pre-#556 result from a later one (see
  "Reports are not findings"). Once a result carries such a marker — that the
  service wrote its reason, or that it was written by a build that reserves
  the service's reasons — reasons on marked results can be narrated as Shoal's
  determinations, as error codes with an origin are, and unmarked results keep
  the hedge, as error codes without one do. The `ErrorCode` half of this note
  (#508) is resolved by #529's `ErrorCodeOrigin`; see "Who assigned an error
  code".
- The source state of each `DispatchEdges` edge is checked against fleet's
  guards by review, not by test: the guards are spread through fleet's
  services. A transition table exported by fleet would let the test check them.
- `EffectPossible` on a terminal record is resolved by #538 (fleet #510):
  false on a succeeded or failed record means the action declared no external
  or egress effect — not that none happened, since a descriptor may declare
  less than its binding permits — and is rendered conditionally, overridden by
  a gateway code; true means an effect may have occurred and is monotonic, so
  it stays true even when the request never left. See "Whether the action
  declared an external effect". A floor on `ExternalEffectBinding` (a
  descriptor that cannot declare less than its binding permits) would let a
  clear flag be stated without the condition.
- Ambiguity reports (#484) are not on main, so nothing here reads them. They
  are the only way to narrow a true `EffectPossible`. When
  they land, a failure's next step can say the target was not reached only
  where a report's outcome is `request_not_sent`. Rendering them:
  - the reporter is trustworthy: its Subject and Actor come from the
    authorization decision, not from the request;
  - several reports are ordered by `ClaimFence`, not `ReportedAt`;
  - `Reference` is surfaced most prominently on `request_not_sent` and
    `outcome_unknown` reports, as the handle an operator takes to the target;
  - `Target` and `Reference` are target-controlled. They are bounded and
    printable-only, but are still rendered as attributed untrusted quotes.
  - **A report's absence is not evidence.** A co-tenant in the same execute
    scope can exhaust the per-record report budget or evict the genuine
    holder from the claim history, after which the holder's report is refused
    as not found (#514). So a missing `request_not_sent` report must never be
    rendered as "the request was sent"; the default advice stays
    "reconcile with the target".
  - Claim holders are ordered by `ClaimFence`. Retained holder timestamps were
    wrong before #484's final round (they recorded the successor's claim
    time), so no holder timestamp is shown.
- Decision-service reasons are string literals in `internal/decisionservice`;
  exporting them as constants would let this package reference them directly.
- Only English ships. Wording should be reviewed by someone outside the
  project before a translation is made from it.
