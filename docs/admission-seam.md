# The pre-call admission seam

An out-of-process caller with a side effect in hand and no way to ask whether
it may perform it is the gap `docs/enforcement-plane.md` describes. This is the
seam that closes it, and the prerequisite for the LLM gateway.

Every other enforcement surface in Shoal adjudicates work Shoal performs.
`/api/v1/ask` runs the inference. `/api/v1/fleet/actions/invoke` enqueues work
Shoal's own executor runs. The authorized client withholds documents as it
retrieves them. A proxy holding a prompt it has not yet sent has none of these:
it does not assemble the response, it does not run under Shoal's executor, and
by the time content exists the egress has already happened.

## Stop, not withhold

Withholding removes evidence from a response that is produced anyway. A caller
that assembles its own payload cannot be served that way. It needs a decision
before the effect, and a refusal here is a real stop: nothing left the host.

## An admission is a dispatch action

There is no new durable record. An admission is an `ActionRecord`, and the
token is its claim:

| admission | dispatch state | lifecycle event |
| --- | --- | --- |
| granted | `claimed` | `action.claimed` |
| refused | `canceled` | `action.canceled` |
| reported | `succeeded` / `failed` | `action.completed` / `action.failed` |
| outstanding | `claimed`, unreported | — |

This is not an economy. A remote worker holding a claim and a proxy holding an
admission token are the same situation: an out-of-process actor holding
permission Shoal granted, whose outcome Shoal has not yet seen. A second record
type would have given the two different durability and a second place for them
to disagree.

The consequence worth stating: an admission and a claim are the same kind of
thing, so the same machinery serves both. They are still told apart — see the
admission marker below — because the two surfaces close them differently.

### An admission is never queued

The table has no `queued` row, and that is the stop rather than an omission.

Every check runs before anything durable is written, and the answer is then
committed as exactly one record at version one — claimed for a grant, cancelled
for a refusal. Writing a queued record first and deciding afterwards would open
a window in which an ungranted, possibly refused, admission exists as an action
waiting for a worker. `Pull` returns it to the same principal, `Claim` on the
dispatch surface hands out a live claim against it, and under the execution
boundary a claim is permission to perform the declared effect out of process. A
refusal would become permission through a different door.

Cancelling the queued record when a later step fails does not close that
window: the cancel is a second mutation that can fail for the same reasons the
first step did, and when it does the orphan is still there. The only safe
failure mode is having written nothing — the caller gets a transport error and
fails closed, which is what a caller of this surface must do with an
unreachable decision plane anyway.

One integration consequence: `action.enqueued` is never published for an
admission. Anything reading the lifecycle stream — the accumulator in #389 in
particular — must key on `action.claimed` and `action.canceled`.

### The record carries what was admitted

`ActionRecord.AdmittedEffects` holds the declared effect set and
`AdmittedDisclosures` a digest of the declared corpus references, and both are
part of `equivalentEnqueue` — the record's retry identity.

Without that, a caller could ask with corpus references, receive obligations
restricting them, then replay the same action ID, idempotency key and token
with the references removed: the record would be recognised as the same
request, obligations would be recomputed over nothing, and the reply would be
an unrestricted allow for a token that is already live. The declaration has to
be pinned by the record, not merely adjudicated on the way past it. A retry
that changes it is a conflict.

The references are a digest rather than a list because they are corpus
identities the caller supplied, and copying them into a dispatch record would
put a caller's claimed reading list somewhere the team overview reads. Refusing
a changed retry needs only equality.

`AdmittedObligation` holds the obligation the grant returned, as a bitmap over
the canonical order of the declared references. **A replay returns that
obligation; it never recomputes one.** The co-occurrence budget is windowed and
moves as an identity reads, so recomputing would let a caller replay its way
into a weaker obligation while holding the same live token — and would make an
already-granted admission unrecoverable for as long as the restrictor was
unreachable, which is the case a retry exists for. Positions rather than
identities, for the same reason the declaration is a digest: a position
discloses nothing without the list it indexes, and the caller supplies that
list again on the retry, where the digest proves it is the same one.

### The durable identity is derived, not the caller's name

A caller names its own admission, and the durable namespace is global. Those
two facts together used to mean that two principals naming the same admission
landed on one record — and, worse, that the collision was *visible*: an unheld
name produced a grant and a name another principal held produced a conflict, a
different status code, so probing names enumerated other principals'
admissions.

Two things do the work, and an earlier version of this section credited the
wrong one.

**The derivation keeps principals apart.** The durable ID is
`digest(authorization domain, subject, actor, client, delegation chain,
caller's name)`, each component length-framed, so two principals choosing the
same name hold two different records. That is all it does. It is unkeyed over a
tuple of a workspace and some identities — knowable in most deployments — so it
makes nothing unguessable, and the previous claim that "neither can address the
other's" was false. Addressing a record never required this surface: a caller
could compute the victim's derived ID, submit it to dispatch enqueue as an
ordinary action ID, and read occupancy off conflict versus success. It could
also squat an unheld one and deny the victim its own admission by name.

**A reserved namespace makes them unreachable.** `admissionIDPrefix` marks a
span of the action identity space that no caller may name. `enqueue` refuses it
unconditionally — before the store is read, and regardless of what is there —
and that is the only place the refusal is needed, because enqueue is the one
entry point at which a caller names a durable action it does not already own.
Every other path takes an identity it must already hold and answers a foreign
or absent one identically.

**Every byte of the span is `0xff`, and that is a correctness requirement.**
The listings exclude admissions by their durable marker and keep no scan budget
off them by position, which is only affordable if a scan meets every ordinary
record before the first admission. A prefix delivers that if and only if it is
*maximal* — every byte `0xff` — because only then does any identity outside the
span differ from it at a byte that is necessarily smaller, and so sort below
the whole span.

Two earlier versions each got this wrong in the opposite direction. The first
put the span at the *bottom* of the key space and had the listings jump over
the key range to keep their scan budget off it — a jump that necessarily
skipped anything *else* in the range, including an ordinary action stored there
before the span was reserved at all. The second moved the span to
`\xffshoal.admission\x00`, which stopped admissions sorting first without
making them sort last: `'s'` leaves every byte above it free, so an ordinary
`\xff\xff` sorted *after* every admission in the store. `Pull` does not refill
a page its filter empties, so a worker got an empty page and a cursor for as
long as the admission tail lasted, and `TeamActions` spent its bounded
discovery budget before reaching the action at all — the same listing outage
the move to `0xff` was meant to end, reached from the other end of the span.
One byte of `0xff` was enough to stop admissions sorting first. It was not
enough to make them sort last.

**The span is four bytes wide, and the width is a rollout budget, not a
secret.** Maximality fixes the ordering at any width, so width trades only
against the second rollout condition below: a pre-existing ordinary action
inside the span blocks the issuing of new admissions until an operator clears
it. Action identities are opaque bytes with no charset rule, so a client
minting random ones puts 1-in-256 of them under a one-byte span — enough that
admission would refuse to start on a sizable store, and live dispatch records
cannot always be deleted to unblock it. Four bytes makes that 1-in-2^32, under
one expected collision in a store of a billion random identities. A longer span
would not be more private, only less likely to be already occupied.

The span also carries no readable tag inside it. A tag invites reading meaning
back out of the identifier, which is the mistake three review rounds were.

**The span says nothing about what a record is.** It is a forward rule about
what may be created, not a classifier. Reachability, ownership and identity
scheme are all read from recorded state:

| question | answered by |
| --- | --- |
| is this an admission? | `AdmittedEffects` is non-empty |
| which identity scheme produced its key? | `AdmittedIdentityScheme` |
| what was admitted? | `AdmittedEffects`, `AdmittedDisclosures` |
| what was the caller obliged to withhold? | `AdmittedObligation` |

Three consecutive review rounds found the same error here — a property inferred
from data that did not carry it. Ownership inferred from a record with no
domain. Identity scheme inferred from an identity that was caller-chosen.
Reachability inferred from a span that predates the reservation. The table is
the answer to the class: every decision reads a marker that was written when
the fact was known, and the only remaining identity-derived rule is enqueue's
refusal, which cannot be wrong about an existing record because it never looks
at one.

A keyed digest was the other option and is the weaker one. It hides an
identifier without making it unreachable, and secrecy of an identifier is not
access control: anything that ever leaks one — a log line, an expired token, a
record read through some other surface — hands the reachability back. It also
needs a durable secret with a rotation story, and rotation re-derives every live
admission's identity, orphaning outstanding grants whose tokens no longer
resolve. The namespace costs none of that.

The token carries the derived ID, which is opaque to the caller and needs to be:
it is only ever handed back.

### Rollout: records written before the identity was derived

**This section is empty of consequence for anyone upgrading from a release.**
The only release is `v1.3.0` (2026-08-26); the admission surface merged
2026-09-29 and is not an ancestor of that tag. A record written under the
superseded identity scheme can therefore only exist in a deployment running an
unreleased `main` build.

Where one does exist, the derivation moved its durable key: it lives at the
caller's own name, not the derived one, so a request for it misses. Granting on
that miss would issue a second live token for work already permitted, and for a
record that is already terminal it is worse — a durable *denial* would be
re-adjudicated and could come back granted, inverting a refusal that is on the
record.

So this build will not adjudicate at all while such a record exists.
`Request` reaches a whole-store verdict before anything else and refuses with
`ErrAdmissionUnmigrated` — an unavailable, not a denial, because the caller has
done nothing wrong and can do nothing about it. The verdict is cached once
proven clean, since this build cannot write an unprefixed admission; a dirty or
unreachable verdict is never cached, so clearing the records needs no restart.

**Why whole-store rather than per-record.** A per-record check was tried and
cannot be made correct. The derivation treats the authorization domain as part
of the principal, but an `ActionRecord` does not carry its domain — so a
predicate over a legacy record can establish ownership only two ways, and both
are wrong. `sameActionPrincipal` omits the domain, which hands the distinctive
unmigrated error to an identically-named identity in another domain and reopens
the existence oracle this work exists to close. Routing through
`authorizedCurrent` to recover the domain from the agent descriptor is blind to
any record whose agent generation has moved — and `Heartbeat` bumps the
generation on every lease renewal, so it is blind to essentially all of them,
and grants over them instead. Under-refusing double-grants, over-refusing
enumerates, and the information needed to do neither is not in the record. A
whole-store verdict has no ownership predicate to get wrong.

**The scheme is read from a marker, not from the identity.** A legacy
admission's key was caller-supplied opaque bytes, so it may lie anywhere —
inside the reserved span included, and in the exact prefix-plus-digest shape a
derived key has. An earlier verdict inferred "new scheme" from the span and so
certified precisely that record as new: the retry derived a different key and
issued a second live grant. `AdmittedIdentityScheme` settles it instead, and is
sound for any identity, adversarial ones included. Its zero value is the
superseded scheme, which is what a record written before the field existed
decodes to — so the default is the safe reading rather than a lucky one.
`equivalentEnqueue` compares it too, which refuses such a record as a conflict
even if the verdict were somehow passed.

**The second rollout condition: ordinary actions inside the span.** Action
identities are arbitrary non-empty bytes and always have been, so the span was
a legal place to store an ordinary action long before it was reserved — and
unlike legacy *admissions*, which no release contains, such a record can exist
in a store upgraded from `v1.3.0`. The collision surface is every action
identity beginning with four `0xff` bytes. An operator can enumerate it with a
single range scan, and for a client minting text or UUID identities the set is
empty by construction, because `0xff` is not a byte either produces.

Nothing silently mishandles such a record: the listings exclude admissions by
marker rather than by range, and `ExecuteClaim` no longer refuses an identity
for its shape, so the action stays listed, claimable and executable. Two things
are narrower than full service. `enqueue` refuses the span unconditionally, so
the action cannot be enqueue-replayed and its idempotent retry fails. And
because it sorts *among* the admissions rather than below them, a listing
reaches it only after paging through every grant in the store — within
`TeamActions`' discovery budget while the admission tail is small, and not
beyond it. That is the one case the maximality argument above does not cover,
and it is exactly the deployment `ErrAdmissionSpanOccupied` refuses to issue
new admissions to until an operator clears the span. It is reported separately
from the legacy condition because the remedy differs.

**What still works while the verdict is dirty.** `Report` and `Outstanding`
deliberately do not consult it. A grant already issued must stay reportable, or
upgrading strands the audit record for an effect that may already have
happened, and those records are exactly what an operator has to find. So
in-flight grants complete and remain visible; what stops is the issuing of new
ones.

**Draining means removing.** Reporting a legacy grant records its outcome but
leaves a record written under the superseded scheme, so serving resumes only
once the records are gone. That is deliberate: the alternative is a rule about
which terminal states are safe to ignore, which is the per-record reasoning
that just failed.

**If the verdict cannot be reached** — the scan errors, or exceeds its page
bound — the request refuses. Serving while unable to prove no unmigrated record
exists *is* the double grant. The bound is set far above any plausible store
and exists so the loop terminates rather than as an operational limit.

**The residual, stated plainly:** while a legacy admission exists, its name
remains enumerable through dispatch enqueue, because the reserved span cannot
recognise an identity that predates it. That is #398's exposure over a
caller-chosen name. It is not closable in `enqueue` — that path must tell
occupied from absent to be idempotent at all — so the remedy is to hold no such
records, which is the default for anyone upgrading from a release and which the
refusal above forces for anyone else.

### An admission is distinguishable from a dispatch action

`AdmittedEffects` is the marker — an admission is refused before it reaches a
record unless it declares an effect, and no dispatch enqueue ever sets one.

Both read paths check it. `report` refuses a dispatch action as not-found:
without that, a claimed dispatch action owned by the same caller passes every
other check and gets completed under admission's semantics rather than its own,
and those differ where it matters — a reported failure is a receipt here and
the executor's error there, so a worker would be told its failed work had
succeeded. `outstanding` filters on it too, because listing a record this
surface refuses to close would name work the caller cannot act on.

This narrows a claim made earlier in this document. Before the marker existed,
an outstanding admission and an outstanding claim genuinely were
indistinguishable; they are not any more, and the surface no longer pretends
otherwise.

The dispatch surface checks the same marker, in the other direction, at every
path that takes or returns an action: `Pull` and `TeamActions` skip admissions,
and `Claim`, `Cancel`, `CompleteClaim`, `ExecuteClaim` and `Status` refuse them
as not-found.

That list grew twice under review, one function at a time, which is worth
recording. `ExecuteClaim` was missed because it takes the record rather than an
ID, so a grant holder could synthesise the argument from what its own token told
it — the guard therefore reads the *stored* record, not the one passed in, or
omitting the marker would dodge it. `TeamActions` was missed because it is the
one path that deliberately does not require the reader to be the action's
principal, so an admission there is handed to *another* caller along with what
the owner declared and was obliged to withhold. `Status` discloses nothing
across principals and is guarded anyway, so that every dispatch entry point
gives one answer for an admission-region identity whether it is absent, another
principal's, or the caller's own.

Absence and foreignness are normalised to one answer in `authorizedCurrent`
rather than at each caller. They were different messages under one status, which
was cosmetic while identities were opaque and stops being cosmetic once they are
derivable: a caller could compute a victim's admission ID and read existence off
the message, which is the enumeration the namespace closes at enqueue reopened
through every other door.

That is the price of sharing one claim transition: merging the paths was right,
and it gave dispatch's reclaim semantics reach over admission records. **A
reclaim means nothing for an admission.** The grant was made to one caller which
was told it may perform an effect; nobody else can finish that, and only the
original caller knows whether the effect happened, so a second party taking the
record and reporting an outcome would be recording a fiction. An expired
admission is abandoned, and a claimed record with a lapsed lease is the honest
statement of that — exactly what `outstanding` reports as `expired`. Cancelling
one would be worse still, rewriting an abandoned grant as a refusal, which is
the opposite statement.

The completion guard is not only about expiry. An admission token carries the
action ID, the claim ID and the version — everything dispatch completion needs —
so a caller holding a live grant could always have completed it there instead of
reporting, skipping the one-shot rule, the exact-replay comparison and the
malformed-report rejection. That route needs no expiry at all.

## The three answers

`POST /api/v1/admission/request` returns one of:

- `denied` — the call must not happen. No token is issued, because a token is
  something a caller can report an effect against.
- `allowed` — it may happen as declared.
- `allowed_with_obligations` — it may happen with the returned references
  withheld from the payload.

The middle case is the point. Refusing a whole call is blunt; "you may proceed
without these" is a decision a caller can comply with.

A denial is an HTTP 200 carrying `"outcome": "denied"`. An unreachable decision
plane is a transport failure. The two must never be confusable, because a proxy
has to treat them oppositely: the first is the answer, the second is an outage
it must fail closed on itself.

## What is checked

1. The request context and the caller's `auth.Decision`, through the same
   `begin` every dispatch call uses. The request and correlation identity come
   from the decision, not the body.
2. The declared effect set against the action's declared effects, resolved
   under that decision. The executor ceiling is already enforced inside
   `resolveActionBinding`, so an action can neither declare more than its
   executor may do nor be admitted for more than it declares.
3. An empty effect set is refused as invalid, not read as "nothing". An
   admission that declares nothing would be the cheapest request and the most
   permissive answer.
4. An unrecognised effect class is refused. The transport passes an unknown
   class through verbatim rather than dropping it, so an unclassifiable request
   stays unclassifiable rather than becoming an empty one.

## Obligations

`Disclosures` on the request are the corpus references the caller says its
payload would carry. Obligations are computed over exactly that set, and
expressed in exactly that set, so a response never names anything the caller
did not name first.

Two stages, both narrowing:

1. `decision.AuthorizeObject(OperationRetrieve, …)` per reference — the same
   object-scoped check the authorized read path applies before a document
   enters a candidate set.
2. `authorized.Client.RestrictDisclosure`, which resolves each reference's
   registration rule and charges the co-occurrence budget exactly as a read of
   the same documents would, and reports what survives.

Stage one is honestly a request-level gate, not a per-reference one. An
admission names one source and one policy; only the object identity varies, so
every reference in a request gets the same answer. Per-reference authorization
is the registration rule attached to each document, which is what stage two
resolves. A deployment that does not wire a restrictor therefore gets weaker
obligations than one that does, and nothing in the service can detect that —
the wiring is a deployment invariant. Hosted startup wires it.

The second is how an existing withholding control becomes an obligation instead
of a refusal. It is not a new policy language: it is `MosaicBudget`, charged
pre-call, answering over a caller-supplied set instead of over a result this
process assembled.

A restrictor that returns a reference authorization already withheld does not
admit it — the result is intersected, so an implementation can only narrow. A
restrictor that errors stops the request; it is not read as "withhold nothing".

**Obligations say nothing about why.** Separating "you are not authorized for
this" from "your budget is spent" would turn admission into an authorization
oracle over document identities: a caller would learn which of a guessed set it
may read by reading the reason. The authorized read path reports those two
classes apart because there the counts describe a corpus the caller is already
reading; here they would describe identities the caller merely named.

## Reporting

`POST /api/v1/admission/report` closes the loop. Without it admission is a
stateless gate: permission granted and no record of whether the call happened.

A token is one-shot. Dispatch's completion path treats a terminal action at the
reported version under the same claim as the reporter's own lost response and
replays it, which is right for a worker retrying a delivery and wrong here — it
would let a caller report one outcome, then report a different one against the
same token and be told the second was recorded. So the admission path resolves
the terminal case first and accepts only a byte-identical replay of what is
already committed. Everything else is `conflict`.

A report is either an outcome or a failure, never both, and all three ways of
violating that are refused: a failure without an error code, an outcome
carrying one, and a failure carrying an outcome. The last matters most — the
completion path discards a failed report's outcome, so the record would say
nothing about it and the replay comparison would read any two failures with the
same error code as the same report.

The error code and the outcome are both validated *before* anything is
completed. The completion path is written for an executor that has already
performed the work, so it records a malformed result as
`invalid_executor_error` or `invalid_executor_output` rather than refusing it.
Reaching that from here would spend a live one-shot token on a failure the
caller was never told about and leave it unable to report the outcome it
actually has. Nothing about the underlying call is known from a malformed
report, so it is refused with no durable transition and the token stays usable.

**Reporting a failure is a successful report.** The failure is the news, not an
error in delivering it: a committed record that matches the report is a
receipt. If the first response to a reported failure were an error and the
identical retry a receipt, what the caller saw would depend on whether its own
report had committed — the exact confusion the one-shot token exists to remove.

This surface used to convert that itself, because the completion path answered
a durably recorded failure with an error. It no longer does (#492): the
completion path returns the committed record with no error, since recording
the report is what the operation was asked to do and the outcome lives in the
record's `state` and `error_code`. So the conversion here was removed rather
than kept as belt-and-braces — a check nothing can reach implies a guarantee
that is no longer held at that point.

### How much escaped

A failure may carry `effected`, a bounded volume saying how much of an
irreversible egress happened before it (#427):

```json
{"failed": true, "error_code": "response_truncated",
 "effected": {"bytes": 2097152, "chunks": 64}}
```

This exists for one boundary. A streamed completion that breaks halfway has
already put bytes in front of the caller and cannot recall them, so volume is
the only thing left to report about an egress that could not be prevented.
Without it, nothing left, fifty bytes left and two megabytes left all arrived
as `response_truncated`, and the loop that consumes these reports could not
tell a non-event from a near-complete disclosure.

**It is an upper bound, never a receipt.** It counts bytes handed to the
transport, which is the most any sender can know — kernel buffers and
intermediate proxies sit between the write and the reader. Read it as *"at
most N bytes may have reached the caller"*, not *"N bytes were received"*.

It does not weaken the either-or rule above. The outcome was refused on a
failure because the completion path *discarded* it; `effected` is committed
and **part of the replay comparison**, so two failures reporting different
volumes are different reports and a second one is `conflict` rather than an
accepted correction. That also makes it write-once: the number a caller
reports first is the number the record keeps, which both gateways can satisfy
because each knows its final count when it reports.

Refused rather than recorded in four ways, so it cannot become a free-form
field on a durable record: on a success, on an action that does not declare
`egresses-content`, outside its bounds, and for chunks with no bytes — a chunk
that left carried something. On the dispatch completion route a refusal is
adjudicated as `invalid_executor_effected` instead, because by then the effect
has happened; that code means *the volume is unknown*, not zero.

The same object is accepted and returned on
`POST /api/v1/fleet/actions/{action}/complete`, which is the route a gateway
performing external effects completes through.

`shoal-llm-gateway` fills it. Every byte the caller can receive passes through
a counting wrapper around the ResponseWriter: `bytes` is what each write
accepted, `chunks` the flushes that pushed some of it — one per relayed SSE
event. The count is read once, after the handler has stopped writing, and
only for a failure that followed it: a truncated stream, a caller that
disconnected, an upstream that broke mid-body, or a non-2xx body relayed in
full. It is omitted when nothing left and when the gateway declares no
`egresses-content` (a loopback provider), since the plane would refuse it
there and leave the grant unreported. A report whose answer was lost is
resent once, as the identical report.

## Outstanding

`POST /api/v1/admission/outstanding` lists admissions this caller was granted
and has not reported. An expired token is reported as `expired`, which is not a
resolution: it is the point at which Shoal permitted an effect and will never
learn what happened. The record stays claimed — it never quietly becomes
something else.

## Actions that require approval

An action registered with `requires_approval` (`docs/approval.md`) is denied
here, durably and with no reason, exactly as any other denial: a caller cannot
tell it from the effect ceiling. Holding is not available on this path. The
caller is waiting on the answer with the payload in hand, and a grant issued
after a human decided would be a grant for a call whose moment had passed.
Approval-required work goes through `/api/v1/fleet/approvals/`, where nothing
is performed until an approver has decided.

The check sits after the replay branch, so an admission granted or denied
before the flag was registered keeps answering from its record.

The first answer to such a denial in the hosted build is currently a `503`
"requires reconciliation", not the denial: the cancelled record carries
`[invoke]` as its authorized operations and the hosted publisher admits
`action.canceled` only under `dispatch`. The denial is committed and the replay
answers `denied`. This is pre-existing — the effect-ceiling denial takes the
same path — and is recorded in `docs/approval.md`, "Residuals".

## What this is not

It does not enforce that a caller honours an obligation. No plane outside the
caller's own process can. This is a declaration seam of the same kind as the
execution boundary: it stops a mismatch between what a caller claims and what
it is permitted, and cannot stop a caller that lies about what it did.

It does not accumulate risk across calls. The report is the input such an
accumulator would charge, and it is durable and published as a lifecycle event
so one can be attached without changing this surface.

It is not the proxy. The proxy is a separate binary, and that is why this is an
HTTP surface rather than a Go interface: a process that handles untrusted
prompt content from arbitrary callers must not share an address space with the
policy store and the corpus.
