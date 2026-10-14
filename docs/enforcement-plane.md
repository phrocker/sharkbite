# An enforcement plane that can stop an LLM

Design note. The parts described as existing were read in this repository and
are cited. The parts described as missing have issues; the pre-call admission
surface among them has since been built and is described in
`docs/admission-seam.md`, and the "What is missing" section below says exactly
which half of it that covers.

## The constraint that shapes the design

A prompt cannot be un-sent. Once content reaches a hosted model the egress has
happened: the provider has it, has logged it, has billed for it, and retains it
under their policy rather than yours. No downstream control recovers that.

So two things that get called the same thing are not:

**Stop** refuses before the call. It is total, and it is only available from
risk computable out of the request plus whatever state has accumulated about
this identity.

**Withhold** lets the call happen and refuses to deliver the result. The
provider already saw the prompt. This protects your user, not your content.

Almost every "guardrail" product sells the second as the first. The distinction
is the whole design, because it decides where the risk signal has to come from:
a stop cannot depend on reading the model's output, because by then the
disclosure that mattered already occurred.

There is no streaming interface to intervene in either. `model.TextGenerator`
(`pkg/model/model.go:27`) is a single `Generate` call, request in, result out.
Adding one would allow aborting mid-generation, but aborting at token 400 has
still disclosed the prompt and the first 399 tokens. Mid-stream abort is a
latency and cost mitigation, not a stop, and should not be described as one.

## What already exists

More than it looks like.

**A durable per-identity risk accumulator that enforces.**
`authorized.MosaicBudget` (`pkg/explorer/authorized/mosaic.go:33`) bounds how
many distinct sensitivity domains one identity may observe within a window.
`MaxDomains` is the bound, `Window` the period. Once an identity has locked in
that many domains, results from a not-yet-observed domain are withheld until
the window elapses. State persists across restarts through `CoOccurrenceLedger`
(`mosaic.go:89`), which the client requires whenever the budget is enabled, so
a misconfiguration fails rather than silently degrading to in-memory.

This is already the shape wanted: accumulated exposure per identity, durable,
and enforcing rather than advisory. What it is not is general. The signal is
hard-coded to sensitivity-domain co-occurrence.

**Admission that excludes rather than filters.** The authorized client removes
documents an identity may not read from the candidate set before scoring
(`pkg/explorer/authorized/retrieve.go`), so content that is not permitted
cannot influence a result. Refusal is structural, not a post-filter.

**Post-generation withholding that already works correctly.** `pkg/reasoning`
promotes only claims backed by verified citations; the rest are recorded as
issues. That is a withhold, correctly placed: it protects the caller from
ungrounded output and makes no claim about what the provider saw.

**A bounded execution loop.** `harness.Budgets`
(`pkg/inference/harness/harness.go:63`) caps steps, elapsed time, input and
output tokens, evidence anchors, graph hops, graph nodes, fanout and repeated
actions. These are per-request ceilings, set at construction.

## The two loops

The feedback loop people usually draw is response to decision within one
request. That loop cannot stop anything, because the response only exists after
the egress.

The loop that works runs at two different latencies.

**Loop one is synchronous and pre-call.** Before a prompt is built, the
enforcement plane answers a single question: may this principal ask this, given
its authorization, the labels of what would be retrieved, and everything it has
already been shown in this window? A refusal here is a real stop. Nothing left
the host.

**Loop two is asynchronous and post-call.** After a response is recorded, the
plane asks what actually happened: what evidence grounded it, which labels were
touched, whether anything egressed, how much. That result updates the
accumulator.

The feedback edge is loop two into loop one's *state*, not into the request
loop two came from. That is why the accumulator has to be durable, and why
`MosaicBudget` already having a ledger matters more than it appears: the
persistence is the hard part and it is done.

Concretely, per request:

1. Resolve the decision. Reject unauthenticated or unauthorized outright.
2. Ask the accumulator whether this identity is within budget. If not, stop.
   No prompt is built and nothing egresses.
3. Assemble the context pack under the authorized client, so unpermitted
   content never enters it.
4. Classify the call. If the configured provider egresses off-host, that is a
   different risk class from a loopback model and has to be charged differently.
   This is the piece that does not exist yet.
5. Call, verify, and emit only citation-backed claims.
6. Record what was observed and charge the accumulator.

Step two is the stop. Everything after it is withholding.

## What is missing

**Egress cannot be classified, and this is the critical path.** Nothing on
`TextGenerator` reports whether a configured provider leaves the host.
`isLoopbackHost` exists for Ollama (`pkg/model/ollama.go:403`) but is not
surfaced, and an OpenAI-compatible generator posts to whatever base URL it was
given. Without this, "did this leave the host" is unanswerable, and it is the
input the entire design turns on: a loopback call and a hosted call carry
categorically different disclosure risk and must not be charged the same.

Tracked as #385, currently framed as a follow-on to #381. **That framing is
wrong for this design.** It is the prerequisite.

**An external enforcement gateway cannot report back.** The dispatch surface
exposes enqueue, invoke, pull, claim, cancel and status, and after a claim the
only terminal paths are in-process execution or cancellation. A gateway outside
the process can take work and has nowhere to report an `ExecutionResult`.
Tracked as #384.

**There is no pre-call admission hook *inside the harness*.**
`harness.Budgets` are static ceilings fixed at construction. There is still no
seam where the in-process loop asks an accumulator whether to proceed, and no
way for a budget to be a function of accumulated state rather than a constant.

The out-of-process half of this now exists: `/api/v1/admission/request` and
`/api/v1/admission/report` (#388, `docs/admission-seam.md`) let an external
caller ask before the effect and report after it. That is loop one and loop two
for callers Shoal does not contain. It does not give the harness the same seam,
and the two should not be conflated: the harness runs inside this process and
can be asked directly, while the admission surface exists precisely because the
caller cannot be.

**The accumulator is not general.** `MosaicBudget` charges distinct sensitivity
domains. A risk plane wants the same durable, windowed, per-identity machinery
over a pluggable signal: egress volume, label sensitivity, query rate,
unattributed residue (#374), or a composite. The enforcement, windowing and
persistence are reusable; the signal is not.

## Where the decision lives

The gateway asks and enforces. The plane decides and records.

Putting the risk decision in the LLM gateway is the obvious shortcut and the
wrong one. Every gateway then needs the policy, the accumulated state and the
audit trail, and they will drift: two gateways will disagree about whether an
identity is over budget, and the disagreement will be invisible because each is
internally consistent. Accumulated state that is replicated is not accumulated
state.

This is the same boundary #381 draws for execution, and it is why an
enforcement gateway is an external-effect executor rather than something inside
Shoal. Shoal decides and holds the record. The gateway terminates connections,
which is work whose effect lands outside Shoal's evidence record and therefore
must be dispatched rather than performed.

## What this does not give you

It does not stop a model disclosing something from its own training data. That
is parametric, it is not in your corpus, and no admission control sees it.
Recognition leakage is a different problem with a different treatment
(#373, #374).

It does not make a hosted provider trustworthy. It decides whether to hand them
content, and records what was handed over. What they do with it is contractual,
not technical.

It does not produce a number that means "risk". It produces an accumulated,
auditable quantity of a signal you chose, with a bound you set. Whether that
quantity corresponds to risk is a policy judgment, and the design should not
pretend the machinery supplies it.

## Order

1. #385, egress classification. Nothing else is meaningful without it, and it
   is the smallest of these.
2. Generalize the accumulator: keep `MosaicBudget`'s windowing, persistence and
   enforcement, make the charged signal pluggable.
3. A pre-call admission seam in the harness, so step two above exists. The
   out-of-process seam is done (#388); the in-process one is not.
4. #384, completion reporting, when an out-of-process gateway is actually
   built.

`MosaicBudget` is already reachable pre-call from outside the process:
`authorized.Client.RestrictDisclosure` charges it over a caller-declared
reference set and reports what survives, so admission can express the budget as
an obligation rather than only as an internal withholding. The signal is still
hard-coded to sensitivity-domain co-occurrence; that is what step two changes.

Streaming abort is deliberately last and optional. It improves cost and latency
and does not change what was disclosed.
