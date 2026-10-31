# Governed ontology discovery

`EmbeddedService.DiscoverOntology` is an opt-in Go entry point for proposing
ontology changes from authorized historical usage and bounded model-guided
graph walks. It persists a **draft**, returns an existing equivalent candidate,
or records an explained abstention. It never approves or publishes a proposal.

This first slice is a library API and executable reference fixture, not a
background learner, HTTP endpoint, MCP tool, or browser discovery workflow.
It does not retrain a model or assume that training data contains your corpus.

## Run the reference fixture

Use a separate, protected directory for each run. The command writes a small
synthetic corpus, historical interaction, governed proposals, and
`discovery-reports.jsonl`; it does not open an existing application corpus.

From the repository root in PowerShell:

```powershell
go run ./cmd/shoal-ontology-discovery-demo -dir .\.shoal\discovery-draft
```

The default **scripted** provider exercises the real authorization, retrieval,
graph-walk, evidence-verification, validation, persistence, and governance
boundaries. It is a deterministic conformance test, not an LLM quality result.
Expected milestones:

```text
provider=scripted status=draft evaluation=no_improvement
retry=duplicate; no additional draft created
restart=preserved state=draft selected_version_resolved=0/2
```

Other scripted scenarios:

```powershell
go run ./cmd/shoal-ontology-discovery-demo -dir .\.shoal\discovery-additive -scenario additive
go run ./cmd/shoal-ontology-discovery-demo -dir .\.shoal\discovery-abstain -scenario abstain
go run ./cmd/shoal-ontology-discovery-demo -dir .\.shoal\discovery-unchanged -scenario unchanged
```

Explicit publication is a separate demonstration flag:

```powershell
go run ./cmd/shoal-ontology-discovery-demo -dir .\.shoal\discovery-published -publish
```

This executes submitted, approved, and published transitions with an explicit
demo reviewer, then reopens the corpus and verifies selected-version
interpretation of the original assertions. The assertions are not rewritten.
This single-process demonstration is not a separation-of-duties policy.

To try an already-installed model with a local Ollama server:

```powershell
go run ./cmd/shoal-ontology-discovery-demo -dir .\.shoal\discovery-model -provider ollama -model YOUR_INSTALLED_MODEL
```

The model must follow the existing action protocol. Invalid output fails
explicitly; there is no fallback to the scripted provider. This path does not
install a model or establish that any particular model produces useful changes.
Do not send private corpus data to an unapproved model endpoint.

## Application integration

Use an authorization-aware embedded client, configure the service's active
ontology, and build a verified context pack using the existing
`contextpack.Builder`. The pack must pin that ontology, the current snapshot,
and the caller's authorization. Select 1-16 exact historical interaction IDs
whose entire exposed evidence and touched nodes are covered by the pack.

```go
result, err := service.DiscoverOntology(ctx, webapi.OntologyDiscoveryRequest{
    Context:         verifiedPack,
    UsageSessionIDs: selectedSessionIDs,
    SelectionPolicy: "reviewed-failure-sample-v1",
    SelectionReason: "Explicitly selected recurring unresolved interactions",
    PopulationSize:  knownPopulation,
    CoverageGaps:    []string{"Only this principal's authorized interactions"},
}, webapi.OntologyDiscoveryConfig{
    Generator: approvedGenerator,
    Model:     modelProvenance,
    Recorder:  durableDiscoveryRecorder,
    Evaluator: heldOutEvaluator, // Optional trusted host component.
})
```

Handle errors before treating the result as successful. A recorder error after
draft creation is returned with an indeterminate-commit marker; inspect the
returned proposal and durable proposal history before retrying. Rejected input
fails before model execution. Once an attempt starts, the required recorder
receives its success, abstention, or failure report.

The recorder must durably store reports under the **complete input
visibility**, not ordinary application-log permissions. Reports contain model
text, source identities, selection information, and execution traces. Failed
walks retain references for the initial context and graph additions actually
made available to the model, even when the model never returns a valid stop.
The demo appends and syncs JSONL; its `0600` creation mode is not a substitute
for protected directory ACLs on Windows or a production authorization policy.

Historical sessions contain digests and structured interaction metadata, not
raw user questions or answers. Discovery does not learn unrecorded query
vocabulary. Incomplete legacy exposure records, foreign-principal sessions,
forged evidence, stale snapshots, and independent extra output-visibility
restrictions are rejected rather than silently weakened.

## Candidate contract and governance

The model receives the base definitions, selected usage metadata, explicit
selection/coverage information, exact visible evidence, and bounded graph
tools. Source text is evidence, not authority to publish.

It must produce one inferred stop claim with the supplied base-version subject
and discovery predicate, whose string object is strict candidate JSON, or an
explained unresolved/unsupported abstention. Candidates contain a rationale,
proposed definitions, supported morphisms, counterevidence, and open questions.
Unknown/duplicate fields, trailing JSON, oversized/deep output, invalid
definitions, unsupported evolution, ambiguous claims, and uncited morphism
evidence are rejected by deterministic checks.

Definition references use keys. Model-action and morphism evidence IDs use
the harness's padded standard base64 encoding; reference-valued allowed
constraints and projected audit IDs use the web API's unpadded base64url
encoding. Unchanged constraints and host-owned schema/definition metadata are
preserved. The host assigns target version labels and candidate identities.

All exposed source evidence is retained on the governed proposal, including
inputs the model did not cite. Proposal visibility therefore cannot be widened
by selecting only convenient citations. Candidate creation verifies the
complete proposal against the existing governance projection bounds, including
the 32-evidence-reference limit, before persistence.

Equivalent base/target candidates are deduplicated across service instances at
the durable create-at-base boundary. Different rationale, author, audit time,
or exposure cannot create another proposal for that same target. Authorized
callers receive an existing candidate when visible. Rejected or withdrawn
candidates are not silently resurrected; selecting a candidate does not change
its lifecycle. A newly published successor rejects stale-base creation.

Publication remains the existing explicit governed lifecycle described in
[read-dynamic ontology](read-dynamic-ontology.md). A draft cannot become an
interpretation lens. Proposals and evidence survive reopening the corpus.

## Budgets and evaluation

Default limits are eight steps, one minute, 65,536 input tokens, 16,384 output
tokens, 64 evidence anchors, four graph hops, 128 graph nodes, fanout 16, and one
repetition of an action. A nonzero custom `harness.Budgets` must supply all
required positive fields; partial structs do not inherit ordinary defaults.
Additional service ceilings prevent an effectively unbounded walk. The full
base/selection prompt is bounded at 16 KiB; oversized scopes fail explicitly,
not by dropping definitions or evidence.

An optional trusted evaluator receives the base and candidate after proposing.
It must identify held-out cases, artifacts, policy, denominator, correct and
unsupported counts, and limitations. Supplied duplicate/overlapping case or
artifact identities are rejected. Status is derived from baseline/candidate
counts; increased unsupported counts are a regression. Scores never promote
or publish a candidate.

Artifact identity checks cannot prove source-document-level independence.
The application must keep held-out cases out of proposing context and enforce
the dataset split. The fixture's one held-out endpoint-compatibility case
scores 1/1 for both versions and reports `no_improvement`. That establishes
protocol compatibility only, not better retrieval, agent quality, or a
statistical outcome gain. A real deployment still needs a representative,
independent task-quality benchmark before claiming utility.
