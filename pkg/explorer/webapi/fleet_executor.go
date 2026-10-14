/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package webapi

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// AskExecutor is the production fleet.ActionExecutor for grounded reasoning.
// It runs one registered agent action through the authorized reasoning path
// that already backs recorded chat, so tool use, retrieval, visibility, and
// interaction recording remain bound to the request's trusted decision. The
// executor adds no model, tool, or storage authority of its own: it decodes a
// bounded question, delegates, and translates the verified citation envelope
// into dispatch evidence.
//
// It deliberately refuses any response that is not finalized, durably
// recorded, and verified, so a dispatch action can never commit a successful
// effect over an answer the reasoning path did not ground.
//
// What the record carries is bounded separately from whether the answer is
// grounded. The dispatch evidence bound counts members rather than anchors,
// and a cited document anchor costs three, so a record holds at most 85 of
// them while the reasoning harness allows far more. Anchors a claim cites are
// packed first and the receipt reports what was left out. Failing the action
// instead would discard a verified answer that the chat path returns happily,
// after the model call was already billed; the complete grounding stays
// durably recorded in the interaction session the receipt names.
//
// The invoking decision must carry auth.OperationRetrieve in addition to
// auth.OperationInvoke: the reasoning service authorizes retrieval on its own
// terms. A principal granted only invoke and dispatch fails every action with
// AskErrorReasoningUnauthorized, which separates an authorization refusal from
// a model or transport failure but does not identify which refusal it was.
//
// Action output is a receipt, not the answer. DispatchService.Status, Pull and
// TeamActions authorize a reader on (domain, source, policy, object) and apply
// no visibility-label check before serializing an ActionRecord, so anything
// this executor writes there is readable by a principal who does not hold the
// source labels. Claim values, issue reasons, ontology identifiers, and the
// effective output label expression are all derived from retrieved documents,
// so none of them belong in Output. The receipt reports only what the
// execution did; the verified claims stay reachable through the
// visibility-enforcing interaction path using the recorded session ID.
//
// ActionRecord.Evidence is the larger surface and is not addressed here: it
// carries citation identifiers, offsets, and label expressions past the same
// unenforced read path. Each reference records its own Visibility, but no
// reader applies it. That gap belongs to the dispatch read path rather than to
// this executor and is tracked by issue #369.
const (
	// AskCapability and AskAction name the single capability this executor
	// serves. A descriptor must declare both for resolution to select it.
	AskCapability = "explorer.reason"
	AskAction     = "ask"

	// MaxAskQuestionBytes bounds one decoded question independently of the
	// action's declared input schema.
	MaxAskQuestionBytes = 4096

	defaultAskRecentResults = 256
)

// Executor error codes. They are recorded verbatim on a failed ActionRecord
// and are stable wire values.
const (
	AskErrorUnsupportedAction = "unsupported_action"
	AskErrorInvalidInput      = "invalid_input"
	AskErrorReasoningFailed   = "reasoning_failed"
	AskErrorUnverified        = "unverified_response"
	AskErrorUngroundedClaims  = "ungrounded_claims"
	// AskErrorInvalidEnvelope separates a structurally invalid response from
	// one that is merely not finalized. Neither code carries the underlying
	// reason: a validation message can name document identifiers, and the
	// action record is durable and read without a visibility-label check, so
	// the rule that keeps content out of the receipt keeps it out of the error
	// code too. A composition owner that needs the detail has it at the
	// provider boundary.
	AskErrorInvalidEnvelope = "invalid_envelope"
	// AskErrorReasoningUnauthorized covers an authorization refusal the
	// reasoning path reports as unauthorized. In practice that is an agent
	// principal granted invoke and dispatch but not retrieve, and a workspace
	// whose chat resources are disabled. Object-level denial is reported as
	// not-found rather than unauthorized, so it lands in
	// AskErrorReasoningFailed; the code does not claim to name which refusal
	// occurred.
	AskErrorReasoningUnauthorized = "reasoning_unauthorized"
	AskErrorEvidenceSkew          = "evidence_snapshot_skew"
	AskErrorEvidenceUnpinned      = "evidence_unpinned"
	AskErrorEvidenceUnrecordable  = "evidence_unrecordable"
	AskErrorOutputEncoding        = "output_encoding_failed"
)

// AskExecutorConfig configures one executor. Provider is required and is
// always an authorization-enforcing reasoning service.
type AskExecutorConfig struct {
	Provider   AskProvider
	Capability string
	Action     string
	// MaxRecentResults bounds the in-process result cache that makes a retried
	// action idempotent. Zero uses the default.
	MaxRecentResults int
	// Clock defaults to time.Now and exists so snapshot-skew detection is
	// testable. It is never used to stamp provenance.
	Clock func() time.Time
}

// AskExecutor implements fleet.ActionExecutor.
//
// It does not implement fleet.RetrievalConfiner, and that is accurate rather
// than an omission: it passes only the question and a top-K to its provider,
// so the invocation's source, policy and object reach nothing, and
// ChatService authorizes OperationRetrieve domain-wide. So fleet refuses an
// invocation whose principal may retrieve beyond the action's own scope,
// rather than letting the record carry evidence the descriptor's scope does
// not cover (#370). Implementing the interface is a promise about retrieval;
// it must not be added before the provider seam can carry a source/policy
// bound, which retrieval.Scope cannot express today.
type AskExecutor struct {
	provider   AskProvider
	capability string
	action     string
	clock      func() time.Time

	// recent makes a retried action idempotent within this process. The
	// dispatch service threads a stable executor key into every invocation for
	// exactly this purpose: an ambiguous execution leaves the action claimed,
	// and once the claim lease expires it is re-claimed and executed again.
	// Without this, that retry bills a second model call and records a second
	// durable interaction session for one action.
	//
	// It is in-process only, so it does not survive restart. Durable
	// idempotency belongs with the retry and reaping work in #363.
	mu     sync.Mutex
	recent map[string]fleet.ExecutionResult
	order  []string
	limit  int
}

var _ fleet.ActionExecutor = (*AskExecutor)(nil)

// NewAskExecutor validates the configuration and returns a ready executor.
func NewAskExecutor(config AskExecutorConfig) (*AskExecutor, error) {
	if isAbsentInterface(config.Provider) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "ask executor requires a reasoning provider")
	}
	capability := config.Capability
	if capability == "" {
		capability = AskCapability
	}
	action := config.Action
	if action == "" {
		action = AskAction
	}
	if strings.TrimSpace(capability) != capability ||
		strings.TrimSpace(action) != action {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "ask executor capability and action must be canonical")
	}
	clock := config.Clock
	if clock == nil {
		clock = time.Now
	}
	limit := config.MaxRecentResults
	if limit == 0 {
		limit = defaultAskRecentResults
	}
	if limit < 0 {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "ask executor result cache bound is negative")
	}
	return &AskExecutor{
		provider: config.Provider, capability: capability,
		action: action, clock: clock,
		recent: make(map[string]fleet.ExecutionResult, limit), limit: limit,
	}, nil
}

// Capability and Action report what this executor serves, so a composition
// owner can register a descriptor that matches without restating literals.
func (e *AskExecutor) Capability() string { return e.capability }
func (e *AskExecutor) Action() string     { return e.action }

// MaxEffects declares what this executor may do, derived from the provider it
// was actually configured with.
//
// It always reads the corpus: that is the whole of what it does inside Shoal.
//
// Whether it also transmits is not a property of this code. The executor hands
// retrieved passages to a configured model provider, and the web composition
// builds that from Ollama or an OpenAI-compatible generator against whatever
// base URL the operator supplied. The same executor is egress-free against a
// loopback provider and egress-bearing against a hosted one, so a constant
// would be wrong for one of those deployments — which is why this returned
// nothing at all until the taxonomy could express transmission (#385).
//
// A provider that cannot report its posture is treated as egressing. Silence
// is not evidence that content stays put, and the permissive reading is the
// wrong default for exactly the class that exists to gate transmission.
//
// It never declares external mutation. This executor changes nothing outside
// Shoal, and declaring it would raise the ceiling enough for genuinely
// external actions to resolve here.
func (e *AskExecutor) MaxEffects() fleet.Effects {
	if e == nil {
		return AskActionEffects(nil)
	}
	return AskActionEffects(e.provider)
}

// AskActionEffects is the effect set a descriptor must register for this
// action against the given provider.
//
// It is exported for the same reason AskActionInputSchema is: registration and
// execution must not drift apart. A registrant cannot guess this — the answer
// depends on where the operator pointed the model provider, not on the action
// — and a descriptor that guesses low is refused at registration rather than
// silently accepted.
//
// A nil provider is not "no egress". There is no configured provider to reason
// about, so it reports the wider set; the executor cannot run without one
// anyway.
func AskActionEffects(provider AskProvider) fleet.Effects {
	if provider == nil || model.EgressesOffHost(provider) {
		return fleet.Effects{
			fleet.EffectEgressesContent, fleet.EffectReadsCorpus,
		}
	}
	return fleet.Effects{fleet.EffectReadsCorpus}
}

// MinEffects is the same set, because for this executor the ceiling is not a
// permission envelope — it is a description. Every invocation reads the corpus,
// and every invocation against a hosted provider transmits what it read.
//
// Declaring only the ceiling would leave the gap that a ceiling cannot close:
// subset semantics permit an action to declare less than the truth, so an
// action declaring only {EffectReadsCorpus} would resolve to a transmitting
// executor and leave a descriptor that reads as non-transmitting while every
// call transmits. Egress leaves no trace in Shoal's own record, so that
// descriptor would be the only place anyone could have noticed.
//
// This is a breaking change for descriptors registered before it, and
// deliberately so. A descriptor written under the superseded taxonomy declared
// the evidence zero value, which decodes to the empty set — so it omits this
// floor and stops resolving until it is re-registered. That is not an upgrade
// bug to paper over: such a descriptor genuinely understates what invoking it
// does, and it says nothing only because the taxonomy it was written under
// could not say anything else. Grandfathering it would keep exactly the
// descriptors this floor exists to reject.
//
// The refusal names the missing classes, so an operator sees what to add.
// docs/shoal-explore-web-deploy.md carries the re-registration note.
func (e *AskExecutor) MinEffects() fleet.Effects { return e.MaxEffects() }

// AskActionInputSchema is the declarative schema a descriptor must register
// for this action. Registering it from here keeps admission and execution from
// drifting apart.
func AskActionInputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"question":{"type":"string"},` +
		`"top_k":{"type":"integer"}},` +
		`"required":["question"],"additionalProperties":false}`)
}

// AskActionOutputSchema is the matching output schema.
func AskActionOutputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"verification":{"type":"string"},` +
		`"snapshot_id":{"type":"string"},` +
		`"session_id":{"type":"string"},` +
		`"evidence_count":{"type":"integer"},` +
		`"evidence_considered":{"type":"integer"},` +
		`"evidence_unusable":{"type":"integer"},` +
		`"evidence_truncated":{"type":"boolean"},` +
		`"cited_evidence_truncated":{"type":"boolean"},` +
		`"claim_count":{"type":"integer"},` +
		`"issue_count":{"type":"integer"}},` +
		`"required":["verification","evidence_count","evidence_considered",` +
		`"evidence_unusable","evidence_truncated",` +
		`"cited_evidence_truncated","claim_count","issue_count"],` +
		`"additionalProperties":false}`)
}

type askExecutorInput struct {
	Question string `json:"question"`
	TopK     uint32 `json:"top_k,omitempty"`
}

// AskExecutionOutput is the executor's action output receipt. It deliberately
// carries no document-derived content: see the AskExecutor doc comment.
type AskExecutionOutput struct {
	Verification  string `json:"verification"`
	SnapshotID    string `json:"snapshot_id,omitempty"`
	SessionID     string `json:"session_id,omitempty"`
	EvidenceCount int    `json:"evidence_count"`
	// EvidenceConsidered and EvidenceTruncated report when the dispatch member
	// bound dropped usable anchors. The bound counts members, not anchors, so
	// a fully-cited document anchor costs three and an evidence-rich answer
	// truncates well before the reasoning harness anchor limit. Without this a
	// reader cannot tell a complete grounding from a clipped one.
	EvidenceConsidered int `json:"evidence_considered"`
	// EvidenceUnusable counts anchors the envelope offered that could not be
	// expressed as a dispatch evidence reference at all. It is separate from
	// truncation because the two mean different things: an unusable anchor is
	// a correctness problem upstream, a truncated one is a bounds problem.
	EvidenceUnusable  int  `json:"evidence_unusable"`
	EvidenceTruncated bool `json:"evidence_truncated"`
	// CitedEvidenceTruncated reports that an anchor one of the answer's own
	// claims rests on is absent from this record. Anchors a claim cites are
	// packed first, so ordinary truncation drops retrieved-but-uncited
	// evidence and leaves this false. When it is true the complete grounding
	// is still durably recorded in the interaction session named by SessionID;
	// it is this record that is bounded, not the answer.
	CitedEvidenceTruncated bool `json:"cited_evidence_truncated"`
	ClaimCount             int  `json:"claim_count"`
	IssueCount             int  `json:"issue_count"`
}

// Execute runs one claimed action. The dispatch service owns the claim fence,
// the execution deadline, output schema validation, evidence validation, and
// the terminal state transition; this method owns only the translation between
// a fleet invocation and the authorized reasoning path.
func (e *AskExecutor) Execute(
	ctx context.Context, invocation fleet.Invocation,
) (fleet.ExecutionResult, error) {
	if invocation.Capability != e.capability || invocation.Action != e.action {
		return fleet.ExecutionResult{ErrorCode: AskErrorUnsupportedAction},
			shoal.NewError(shoal.ErrorInvalidArgument,
				"ask executor does not serve the invoked action")
	}
	input, err := decodeAskExecutorInput(invocation.Input)
	if err != nil {
		return fleet.ExecutionResult{ErrorCode: AskErrorInvalidInput}, err
	}
	if cached, ok := e.recall(invocation.IdempotencyKey); ok {
		return cached, nil
	}
	// An absent top_k is forwarded as zero so the reasoning service applies
	// its own workspace-aware default. Substituting a constant here would
	// exceed a workspace whose retrieval limit is lower and fail the action
	// for a question the same principal can ask over chat.
	envelope, err := e.provider.Ask(
		ctx, AskRequest{Question: input.Question, TopK: input.TopK})
	if err != nil {
		if shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
			// Distinguish an authorization refusal from a model or transport
			// failure. The error itself carries which refusal it was.
			return fleet.ExecutionResult{
				ErrorCode: AskErrorReasoningUnauthorized}, err
		}
		return fleet.ExecutionResult{ErrorCode: AskErrorReasoningFailed}, err
	}
	// Both consumers take the same AskProvider, so the executor applies the
	// chat transport's own contract rather than a weaker subset of it, in the
	// same order. Validate is the check that matters most here: it enforces
	// per-anchor verification status, that source roles match the citation,
	// that the quote matches its range, and that the anchor is the canonical
	// content-derived identity. This path writes those anchors into a durable
	// attestation-bearing record, so it is the path that most needs them.
	if err := envelope.Validate(); err != nil {
		return failAfterAsk(envelope, askEvidenceTally{}, AskErrorInvalidEnvelope)
	}
	if err := validateFinalizedChatResponse(envelope); err != nil {
		return failAfterAsk(envelope, askEvidenceTally{}, AskErrorUnverified)
	}
	evidence, tally := askExecutorEvidence(envelope)
	// The guard is on what the envelope offered, not on what survived
	// translation: an answer whose anchors were all unusable is exactly the
	// case that must not commit.
	if (tally.considered > 0 || len(envelope.Claims) > 0) && len(evidence) == 0 {
		// Nothing could be recorded. Succeeding would commit a record
		// asserting claims that the record itself does not ground. Name which
		// of the two causes it was, because they need different responses: an
		// unusable anchor is a correctness problem in the reasoning path,
		// while an anchor too large to pin is a bounds problem.
		code := AskErrorUngroundedClaims
		if tally.usable > 0 {
			code = AskErrorEvidenceUnrecordable
		}
		return failAfterAsk(envelope, tally, code)
	}
	result := fleet.ExecutionResult{
		Output: nil, Evidence: evidence,
	}
	if len(evidence) > 0 {
		// A snapshot pin is only meaningful alongside evidence, and the
		// dispatch service rejects one without it.
		asOf := envelope.SnapshotAsOf.UTC()
		if envelope.SnapshotID == "" || asOf.IsZero() {
			// The dispatch service requires a pin alongside evidence and would
			// otherwise reject the whole batch under a code naming nothing.
			return failAfterAsk(envelope, tally, AskErrorEvidenceUnpinned)
		}
		if asOf.After(e.clock().UTC()) {
			// The service would reject this as outside execution bounds. Fail
			// with a code that names the cause instead of a generic evidence
			// rejection; the pin is provenance and is never rewritten.
			return failAfterAsk(envelope, tally, AskErrorEvidenceSkew)
		}
		result.EvidenceSnapshotID = envelope.SnapshotID
		result.EvidenceSnapshotAsOf = asOf
	}
	output, err := json.Marshal(
		askExecutorOutput(envelope, len(evidence), tally))
	if err != nil {
		return fleet.ExecutionResult{ErrorCode: AskErrorOutputEncoding}, err
	}
	result.Output = output
	e.remember(invocation.IdempotencyKey, result)
	return result, nil
}

// recall returns a result already produced for this executor key. The key is
// stable across retries of one action, so returning the first result is what
// makes a retry idempotent rather than a second billed model call.
func (e *AskExecutor) recall(key []byte) (fleet.ExecutionResult, bool) {
	if len(key) == 0 || e.limit == 0 {
		return fleet.ExecutionResult{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	result, ok := e.recent[string(key)]
	if !ok {
		return fleet.ExecutionResult{}, false
	}
	return cloneAskExecutionResult(result), true
}

func (e *AskExecutor) remember(key []byte, result fleet.ExecutionResult) {
	if len(key) == 0 || e.limit == 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	identity := string(key)
	if _, exists := e.recent[identity]; exists {
		return
	}
	if len(e.order) >= e.limit {
		delete(e.recent, e.order[0])
		e.order = e.order[1:]
	}
	e.recent[identity] = cloneAskExecutionResult(result)
	e.order = append(e.order, identity)
}

func cloneAskExecutionResult(result fleet.ExecutionResult) fleet.ExecutionResult {
	clone := result
	clone.Output = append(json.RawMessage(nil), result.Output...)
	clone.Evidence = append([]fleet.EvidenceRef(nil), result.Evidence...)
	return clone
}

// failAfterAsk records a failure that happened after the reasoning call ran.
// The model call has been made and the interaction durably recorded, so the
// failed record must still carry the session identity: it is the only route
// back to a reasoning session that was already performed and paid for.
//
// Returning a nil error is deliberate. The dispatch service skips output
// entirely when the executor returns an error, so returning one here would
// discard the receipt and leave the operator with a bare code. The non-empty
// error code still commits the action as failed.
func failAfterAsk(
	envelope CitationEnvelope, tally askEvidenceTally, code string,
) (fleet.ExecutionResult, error) {
	output, err := json.Marshal(askExecutorOutput(envelope, 0, tally))
	if err != nil {
		return fleet.ExecutionResult{ErrorCode: AskErrorOutputEncoding}, err
	}
	return fleet.ExecutionResult{ErrorCode: code, Output: output}, nil
}

func decodeAskExecutorInput(raw json.RawMessage) (askExecutorInput, error) {
	if len(raw) == 0 {
		return askExecutorInput{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "ask executor input is required")
	}
	var input askExecutorInput
	if err := json.Unmarshal(raw, &input); err != nil {
		return askExecutorInput{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "ask executor input is invalid JSON")
	}
	if strings.TrimSpace(input.Question) == "" {
		return askExecutorInput{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "ask executor question is required")
	}
	if len(input.Question) > MaxAskQuestionBytes {
		return askExecutorInput{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "ask executor question exceeds its byte bound")
	}
	if input.TopK > MaxTopK {
		return askExecutorInput{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "ask executor top_k exceeds the transport limit")
	}
	return input, nil
}

func askExecutorOutput(
	envelope CitationEnvelope, evidenceCount int, tally askEvidenceTally,
) AskExecutionOutput {
	output := AskExecutionOutput{
		Verification:           string(envelope.Verification),
		EvidenceCount:          evidenceCount,
		EvidenceConsidered:     tally.considered,
		EvidenceUnusable:       tally.considered - tally.usable,
		EvidenceTruncated:      tally.truncated,
		CitedEvidenceTruncated: tally.citedDropped,
		ClaimCount:             len(envelope.Claims),
		IssueCount:             len(envelope.Issues),
	}
	if envelope.SnapshotID != "" {
		// Reported whenever it is known, not only on success: the pin is the
		// subject of the unpinned and skew failures, and an operator cannot
		// diagnose either without seeing it.
		output.SnapshotID = encodeID(envelope.SnapshotID)
	}
	if envelope.SessionID != "" {
		output.SessionID = encodeID(envelope.SessionID)
	}
	return output
}

// askExecutorEvidence translates verified citation evidence into dispatch
// evidence references. Every candidate is canonicalized through the same
// interaction validation the dispatch service applies, and one that does not
// satisfy an evidence variant is skipped rather than failing the batch: a
// single malformed anchor must not discard a verified answer. Execute refuses
// to succeed if nothing survives.
// askEvidenceTally separates the two ways an offered anchor fails to reach the
// record. Collapsing them would let a partially grounded answer report as a
// complete one, which is the distinction the receipt exists to preserve.
type askEvidenceTally struct {
	// considered counts every distinct anchor the envelope offered.
	considered int
	// usable counts those that translated into a valid reference, whether or
	// not the member bound then left room to record them.
	usable int
	// truncated records that the member bound dropped a usable reference.
	// Deriving this from the recorded count instead would report every
	// post-translation failure as a truncation, because those paths report no
	// recorded evidence at all.
	truncated bool
	// citedDropped records that the bound dropped an anchor a recorded claim
	// cites. Anchors are packed cited-first so this is not reachable by
	// ordinary truncation; when it does happen the answer's own claims are
	// ungrounded in the record and the action must not commit.
	citedDropped bool
}

func askExecutorEvidence(
	envelope CitationEnvelope,
) ([]fleet.EvidenceRef, askEvidenceTally) {
	var tally askEvidenceTally
	cited := askCitedAnchors(envelope)
	usable := make([]fleet.EvidenceRef, 0, len(envelope.Evidence))
	seen := make(map[shoal.ID]struct{}, len(envelope.Evidence))
	for _, evidence := range envelope.Evidence {
		if evidence.AnchorID == "" {
			// Counted, never usable. Skipping it before the tally would hide
			// the loss and let the ungrounded guard pass on an envelope whose
			// anchors were all anonymous.
			tally.considered++
			continue
		}
		if _, duplicate := seen[evidence.AnchorID]; duplicate {
			continue
		}
		seen[evidence.AnchorID] = struct{}{}
		tally.considered++
		ref, ok := askExecutorEvidenceRef(evidence)
		if !ok {
			if _, isCited := cited[evidence.AnchorID]; isCited {
				// As lost to the record as one the bound could not fit.
				// Setting this only in the packing loop would leave the
				// guard depending on an invariant owned by another file.
				tally.citedDropped = true
			}
			continue
		}
		tally.usable++
		usable = append(usable, ref)
	}
	// Anchors a claim cites are packed first. The dispatch bound counts
	// evidence members rather than anchors, so an evidence-rich answer
	// truncates well before the reasoning harness anchor limit; without this
	// ordering the bound could drop exactly the anchors the recorded claims
	// depend on and still commit as succeeded.
	refs := make([]fleet.EvidenceRef, 0, len(usable))
	members := 0
	for _, preferCited := range []bool{true, false} {
		for _, ref := range usable {
			_, isCited := cited[ref.AnchorID]
			if isCited != preferCited {
				continue
			}
			cost := len(ref.NodeIDs) + len(ref.EdgeIDs) + len(ref.Assertions)
			if len(refs) >= fleet.MaxActionEvidence ||
				members+cost > fleet.MaxActionEvidence {
				// One oversized path must not discard every smaller anchor
				// after it, so keep packing what still fits.
				tally.truncated = true
				if isCited {
					tally.citedDropped = true
				}
				continue
			}
			members += cost
			refs = append(refs, ref)
		}
	}
	return refs, tally
}

// askCitedAnchors collects the anchors the response's own claims rest on.
func askCitedAnchors(envelope CitationEnvelope) map[shoal.ID]struct{} {
	cited := make(map[shoal.ID]struct{})
	for _, claim := range envelope.Claims {
		for _, anchorID := range claim.CitationAnchorIDs {
			cited[anchorID] = struct{}{}
		}
		for _, anchorID := range claim.DerivedEvidenceAnchorIDs {
			cited[anchorID] = struct{}{}
		}
	}
	return cited
}

func askExecutorEvidenceRef(
	evidence CitationEvidence,
) (fleet.EvidenceRef, bool) {
	// An empty label set is public. A source that declares no visibility is
	// unrestricted, so its evidence legitimately carries no labels and must be
	// recorded rather than dropped; dropping it made every action against an
	// unlabeled corpus fail.
	visibility, err := interaction.Conjoin(evidence.Visibility)
	if err != nil {
		return fleet.EvidenceRef{}, false
	}
	var ref fleet.EvidenceRef
	switch {
	case evidence.Citation != nil:
		citation := *evidence.Citation
		// The verifier resolves a citation's document, section, and span
		// identities into SourceIDs even when the model cited at document
		// granularity, and the interaction record for this anchor uses those
		// three roles. Rebuilding the roles from the raw citation would commit
		// a narrower grounding than the interaction record asserts for the
		// same anchor.
		nodes := append([]shoal.ID(nil), evidence.SourceIDs...)
		if len(nodes) != 3 {
			nodes = []shoal.ID{citation.DocumentID}
			if citation.SectionID != "" {
				nodes = append(nodes, citation.SectionID)
			}
			if citation.SpanID != "" {
				nodes = append(nodes, citation.SpanID)
			}
		}
		ref = fleet.EvidenceRef{
			AnchorID:   evidence.AnchorID,
			Kind:       interaction.EvidenceDocument,
			Citation:   citation,
			NodeIDs:    dedupeAskEvidenceIDs(nodes),
			Visibility: visibility,
		}
	case evidence.Path != nil && len(evidence.Path.Nodes) > 0:
		path := *evidence.Path
		nodes := make([]shoal.ID, 0, len(path.Nodes))
		for _, node := range path.Nodes {
			nodes = append(nodes, node.ID)
		}
		edges := make([]shoal.ID, 0, len(path.Edges))
		for _, edge := range path.Edges {
			edges = append(edges, edge.ID)
		}
		assertions := make([]interaction.AssertionReference, 0, len(evidence.Assertions))
		for _, assertion := range evidence.Assertions {
			// An assertion must name an edge this path actually references.
			if !containsAskEvidenceID(edges, assertion.EdgeID) {
				continue
			}
			assertions = append(assertions, interaction.AssertionReference{
				AssertionID: assertion.AssertionID,
				EdgeID:      assertion.EdgeID,
				Origin:      assertion.Origin,
			})
		}
		ref = fleet.EvidenceRef{
			AnchorID:   evidence.AnchorID,
			Kind:       interaction.EvidenceGraph,
			NodeIDs:    nodes,
			EdgeIDs:    edges,
			Assertions: assertions,
			Visibility: visibility,
		}
	default:
		return fleet.EvidenceRef{}, false
	}
	canonical, err := interaction.EvidenceReference{
		AnchorID: ref.AnchorID, Kind: ref.Kind, Citation: ref.Citation,
		NodeIDs: ref.NodeIDs, EdgeIDs: ref.EdgeIDs, Assertions: ref.Assertions,
	}.Canonical()
	if err != nil {
		return fleet.EvidenceRef{}, false
	}
	ref.NodeIDs = canonical.NodeIDs
	ref.EdgeIDs = canonical.EdgeIDs
	ref.Assertions = canonical.Assertions
	return ref, true
}

func containsAskEvidenceID(values []shoal.ID, value shoal.ID) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}

func dedupeAskEvidenceIDs(values []shoal.ID) []shoal.ID {
	result := make([]shoal.ID, 0, len(values))
	for _, value := range values {
		if !containsAskEvidenceID(result, value) {
			result = append(result, value)
		}
	}
	return result
}
