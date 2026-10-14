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

// Package harness adapts a trusted, provider-neutral tool-using agent runtime
// to the grounded inference Generator contract.
package harness

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	MaxLogicalIDBytes  = 4096
	MaxToolQueryBytes  = 16 * 1024
	MaxTranscriptSteps = 1024
)

var (
	ErrInvalid           = errors.New("invalid agent harness value")
	ErrBudgetExhausted   = errors.New("agent harness budget exhausted")
	ErrRunnerUnavailable = errors.New("agent harness runner unavailable")
)

type ActionKind string

const (
	ActionRetrieve    ActionKind = "retrieve"
	ActionOpenSection ActionKind = "open_section"
	ActionNeighbors   ActionKind = "neighbors"
	ActionStop        ActionKind = "stop"
)

type Budgets struct {
	MaxSteps          int
	MaxElapsed        time.Duration
	MaxInputTokens    int
	MaxOutputTokens   int
	MaxEvidence       int
	MaxGraphHops      int
	MaxGraphNodes     int
	MaxFanout         int
	MaxRepeatedAction int
}

// NormalizeBudgets applies public budget defaults and validates all bounds.
func NormalizeBudgets(b Budgets) (Budgets, error) {
	b = b.normalized()
	if err := b.validate(); err != nil {
		return Budgets{}, err
	}
	return b, nil
}

func validateResultProvenance(result inference.InferenceResult, expected Provenance) error {
	for _, claim := range result.Claims() {
		model := claim.ModelProvenance()
		prompt := claim.PromptProvenance()
		if modelIdentity(model) != modelIdentity(expected.model) ||
			prompt.TemplateID() != expected.prompt.TemplateID() ||
			prompt.Version() != expected.prompt.Version() ||
			prompt.Hash() != expected.prompt.Hash() {
			return invalid("claim provenance does not match the harness session")
		}

	}
	return nil
}

func validateSectionResults(request OpenSectionRequest, anchors []inference.EvidenceAnchor) error {
	for _, anchor := range anchors {
		citation, _, ok := anchor.Document()
		if !ok || citation.DocumentID != request.documentID ||
			citation.RevisionID != request.revisionID || citation.SectionID != request.sectionID {
			return invalid("open_section result does not match the requested section")
		}
	}
	return nil
}

func validateNeighborResults(request NeighborsRequest, anchors []inference.EvidenceAnchor) error {
	for _, anchor := range anchors {
		path, ok := anchor.Path()
		if !ok || len(path.Nodes) == 0 || path.Nodes[0].ID != request.nodeID ||
			len(path.Edges) > request.hops {
			return invalid("neighbors result exceeds or does not match the requested traversal")
		}
	}
	return nil
}

func sectionAllowed(pack inference.ContextPack, request OpenSectionRequest) bool {
	for _, anchor := range pack.Evidence() {
		citation, _, ok := anchor.Document()
		if ok && citation.DocumentID == request.documentID &&
			citation.RevisionID == request.revisionID && citation.SectionID == request.sectionID {
			return true
		}
	}
	return false
}

func nodeAllowed(pack inference.ContextPack, nodeID shoal.ID) bool {
	for _, anchor := range pack.Evidence() {
		path, ok := anchor.Path()
		if !ok {
			continue
		}
		for _, node := range path.Nodes {
			if node.ID == nodeID {
				return true
			}
		}
	}
	return false
}

func (b Budgets) validate() error {
	b = b.normalized()
	if b.MaxSteps <= 0 || b.MaxSteps > MaxTranscriptSteps {
		return invalid("max steps is outside the supported range")
	}
	if b.MaxElapsed <= 0 {
		return invalid("max elapsed must be positive")
	}
	if b.MaxInputTokens < 0 || b.MaxOutputTokens < 0 || b.MaxEvidence <= 0 ||
		b.MaxGraphHops < 0 || b.MaxGraphNodes <= 0 || b.MaxFanout <= 0 ||
		b.MaxRepeatedAction <= 0 {
		return invalid("budget values are outside the supported range")
	}
	if b.MaxEvidence > inference.MaxEvidenceAnchors {
		return invalid("max evidence exceeds the inference evidence anchor bound")
	}
	if !uint32Representable(b.MaxGraphHops) || !uint32Representable(b.MaxGraphNodes) ||
		!uint32Representable(b.MaxFanout) || !uint32RepresentableProduct(b.MaxFanout, b.MaxGraphNodes) {
		return invalid("graph budgets exceed the backend bound")
	}
	return nil
}

func (b Budgets) normalized() Budgets {
	if b.MaxEvidence == 0 && b.MaxFanout > 0 {
		b.MaxEvidence = b.MaxFanout
	}
	if b.MaxGraphNodes == 0 && b.MaxFanout > 0 {
		b.MaxGraphNodes = b.MaxFanout + 1
	}
	return b
}

const maxUint32Value = int64(1<<32 - 1)

func uint32Representable(value int) bool {
	return value >= 0 && int64(value) <= maxUint32Value
}

func uint32RepresentableProduct(left, right int) bool {
	if left < 0 || right < 0 {
		return false
	}
	if left == 0 || right == 0 {
		return true
	}
	return int64(left) <= maxUint32Value/int64(right)
}

type Provenance struct {
	harness    string
	model      inference.ModelProvenance
	prompt     inference.PromptProvenance
	toolPolicy string
}

func NewProvenance(harness string, model inference.ModelProvenance, prompt inference.PromptProvenance, toolPolicy string) (Provenance, error) {
	p := Provenance{
		harness: strings.TrimSpace(harness), model: model,
		prompt: prompt, toolPolicy: strings.TrimSpace(toolPolicy),
	}
	for name, value := range map[string]string{
		"harness": p.harness, "tool policy": p.toolPolicy,
	} {
		if err := boundedString(name, value, shoal.MaxSemanticStringBytes); err != nil {
			return Provenance{}, err
		}
	}
	if err := prompt.Validate(); err != nil {
		return Provenance{}, err
	}
	if err := model.Validate(); err != nil {
		return Provenance{}, err
	}
	return p, nil
}

func (p Provenance) Harness() string                    { return p.harness }
func (p Provenance) Provider() string                   { return p.model.Provider() }
func (p Provenance) Model() inference.ModelProvenance   { return p.model }
func (p Provenance) Prompt() inference.PromptProvenance { return p.prompt }
func (p Provenance) ToolPolicy() string                 { return p.toolPolicy }
func (p Provenance) validate() error {
	_, err := NewProvenance(p.harness, p.model, p.prompt, p.toolPolicy)
	return err
}

type SessionRequest struct {
	id         shoal.ID
	context    inference.ContextPack
	budgets    Budgets
	provenance Provenance
}

func newSessionRequest(pack inference.ContextPack, budgets Budgets, provenance Provenance) (SessionRequest, error) {
	budgets = budgets.normalized()
	if err := pack.Validate(); err != nil {
		return SessionRequest{}, err
	}
	if err := budgets.validate(); err != nil {
		return SessionRequest{}, err
	}
	if err := provenance.validate(); err != nil {
		return SessionRequest{}, err
	}
	id := deriveID("session-request", string(pack.ID()), canonicalBudgets(budgets),
		provenance.harness, modelIdentity(provenance.model),
		provenance.prompt.TemplateID(), provenance.prompt.Version(), provenance.prompt.Hash(),
		provenance.toolPolicy)
	return SessionRequest{id: id, context: pack, budgets: budgets, provenance: provenance}, nil
}

func (r SessionRequest) ID() shoal.ID                   { return r.id }
func (r SessionRequest) Context() inference.ContextPack { return clonePack(r.context) }
func (r SessionRequest) Budgets() Budgets               { return r.budgets }
func (r SessionRequest) Provenance() Provenance         { return r.provenance }

type RetrieveRequest struct {
	query string
	limit int
}

func NewRetrieveRequest(query string, limit int) (RetrieveRequest, error) {
	if err := boundedString("retrieve query", query, MaxToolQueryBytes); err != nil {
		return RetrieveRequest{}, err
	}
	query = strings.Join(strings.Fields(query), " ")
	if err := boundedString("retrieve query", query, MaxToolQueryBytes); err != nil {
		return RetrieveRequest{}, err
	}
	if limit <= 0 {
		return RetrieveRequest{}, invalid("retrieve limit must be positive")
	}
	return RetrieveRequest{query: query, limit: limit}, nil
}
func (r RetrieveRequest) Query() string { return r.query }
func (r RetrieveRequest) Limit() int    { return r.limit }

type OpenSectionRequest struct {
	documentID shoal.ID
	revisionID shoal.ID
	sectionID  shoal.ID
}

func NewOpenSectionRequest(documentID, revisionID, sectionID shoal.ID) (OpenSectionRequest, error) {
	if err := validateLogicalID("document ID", documentID); err != nil {
		return OpenSectionRequest{}, err
	}
	if err := validateLogicalID("revision ID", revisionID); err != nil {
		return OpenSectionRequest{}, err
	}
	if err := validateLogicalID("section ID", sectionID); err != nil {
		return OpenSectionRequest{}, err
	}
	return OpenSectionRequest{documentID: documentID, revisionID: revisionID, sectionID: sectionID}, nil
}
func (r OpenSectionRequest) DocumentID() shoal.ID { return r.documentID }
func (r OpenSectionRequest) RevisionID() shoal.ID { return r.revisionID }
func (r OpenSectionRequest) SectionID() shoal.ID  { return r.sectionID }

type NeighborsRequest struct {
	nodeID shoal.ID
	hops   int
	fanout int
}

func NewNeighborsRequest(nodeID shoal.ID, hops, fanout int) (NeighborsRequest, error) {
	if err := validateLogicalID("node ID", nodeID); err != nil {
		return NeighborsRequest{}, err
	}
	if hops <= 0 || fanout <= 0 {
		return NeighborsRequest{}, invalid("neighbors hops and fanout must be positive")
	}
	return NeighborsRequest{nodeID: nodeID, hops: hops, fanout: fanout}, nil
}
func (r NeighborsRequest) NodeID() shoal.ID { return r.nodeID }
func (r NeighborsRequest) Hops() int        { return r.hops }
func (r NeighborsRequest) Fanout() int      { return r.fanout }

type Usage struct {
	InputTokens  int
	OutputTokens int
}

type Action struct {
	kind        ActionKind
	correlation shoal.ID
	retrieve    RetrieveRequest
	open        OpenSectionRequest
	neighbors   NeighborsRequest
	result      inference.InferenceResult
	usage       Usage
}

func NewRetrieveAction(correlation shoal.ID, request RetrieveRequest, usage Usage) (Action, error) {
	a := Action{kind: ActionRetrieve, correlation: correlation, retrieve: request, usage: usage}
	return validatedAction(a)
}
func NewOpenSectionAction(correlation shoal.ID, request OpenSectionRequest, usage Usage) (Action, error) {
	a := Action{kind: ActionOpenSection, correlation: correlation, open: request, usage: usage}
	return validatedAction(a)
}
func NewNeighborsAction(correlation shoal.ID, request NeighborsRequest, usage Usage) (Action, error) {
	a := Action{kind: ActionNeighbors, correlation: correlation, neighbors: request, usage: usage}
	return validatedAction(a)
}
func NewStopAction(correlation shoal.ID, result inference.InferenceResult, usage Usage) (Action, error) {
	a := Action{kind: ActionStop, correlation: correlation, result: result, usage: usage}
	return validatedAction(a)
}
func validatedAction(a Action) (Action, error) {
	if err := a.validate(); err != nil {
		return Action{}, err
	}
	return a, nil
}
func (a Action) Kind() ActionKind                        { return a.kind }
func (a Action) CorrelationID() shoal.ID                 { return a.correlation }
func (a Action) Usage() Usage                            { return a.usage }
func (a Action) Retrieve() (RetrieveRequest, bool)       { return a.retrieve, a.kind == ActionRetrieve }
func (a Action) OpenSection() (OpenSectionRequest, bool) { return a.open, a.kind == ActionOpenSection }
func (a Action) Neighbors() (NeighborsRequest, bool)     { return a.neighbors, a.kind == ActionNeighbors }
func (a Action) Result() (inference.InferenceResult, bool) {
	return cloneResult(a.result), a.kind == ActionStop
}

func (a Action) validate() error {
	if err := validateLogicalID("correlation ID", a.correlation); err != nil {
		return err
	}
	if a.usage.InputTokens < 0 || a.usage.OutputTokens < 0 {
		return invalid("token usage cannot be negative")
	}
	switch a.kind {
	case ActionRetrieve:
		if _, err := NewRetrieveRequest(a.retrieve.query, a.retrieve.limit); err != nil {
			return err
		}
	case ActionOpenSection:
		if _, err := NewOpenSectionRequest(a.open.documentID, a.open.revisionID, a.open.sectionID); err != nil {
			return err
		}
	case ActionNeighbors:
		if _, err := NewNeighborsRequest(a.neighbors.nodeID, a.neighbors.hops, a.neighbors.fanout); err != nil {
			return err
		}
	case ActionStop:
		if err := a.result.Validate(); err != nil {
			return err
		}
	default:
		return invalid("unknown or forbidden action")
	}
	return nil
}

type ToolResult struct {
	correlation     shoal.ID
	kind            ActionKind
	anchors         []inference.EvidenceAnchor
	snapshot        inference.SnapshotPin
	auth            inference.AuthPin
	embeddingSpaces interaction.EmbeddingSpaceSet
}

func NewToolResult(correlation shoal.ID, kind ActionKind, anchors []inference.EvidenceAnchor, snapshot inference.SnapshotPin, auth inference.AuthPin) (ToolResult, error) {
	return NewToolResultWithEmbeddingSpaces(
		correlation, kind, anchors, snapshot, auth,
		interaction.EmbeddingSpaceSet{},
	)
}

// NewToolResultWithEmbeddingSpaces records the stable spaces that participated
// in successful vector-scored output.
func NewToolResultWithEmbeddingSpaces(
	correlation shoal.ID,
	kind ActionKind,
	anchors []inference.EvidenceAnchor,
	snapshot inference.SnapshotPin,
	auth inference.AuthPin,
	embeddingSpaces interaction.EmbeddingSpaceSet,
) (ToolResult, error) {
	if len(anchors) > inference.MaxEvidenceAnchors {
		return ToolResult{}, invalid("tool result anchor count is outside the supported range")
	}
	r := ToolResult{
		correlation: correlation, kind: kind,
		anchors:  append([]inference.EvidenceAnchor(nil), anchors...),
		snapshot: snapshot, auth: auth,
		embeddingSpaces: embeddingSpaces,
	}
	r.embeddingSpaces.Identities = append(
		[]string(nil), embeddingSpaces.Identities...)
	sort.Slice(r.anchors, func(i, j int) bool { return shoal.CompareID(r.anchors[i].ID(), r.anchors[j].ID()) < 0 })
	if err := r.validate(); err != nil {
		return ToolResult{}, err
	}
	return r, nil
}
func (r ToolResult) CorrelationID() shoal.ID { return r.correlation }
func (r ToolResult) Kind() ActionKind        { return r.kind }
func (r ToolResult) Anchors() []inference.EvidenceAnchor {
	return append([]inference.EvidenceAnchor(nil), r.anchors...)
}
func (r ToolResult) Snapshot() inference.SnapshotPin  { return r.snapshot }
func (r ToolResult) Authorization() inference.AuthPin { return r.auth }
func (r ToolResult) EmbeddingSpaces() interaction.EmbeddingSpaceSet {
	cloned := r.embeddingSpaces
	cloned.Identities = append([]string(nil), r.embeddingSpaces.Identities...)
	return cloned
}
func (r ToolResult) validate() error {
	if err := validateLogicalID("tool result correlation ID", r.correlation); err != nil {
		return err
	}
	switch r.kind {
	case ActionRetrieve, ActionOpenSection, ActionNeighbors:
	default:
		return invalid("tool result kind is not a callable action")
	}
	if err := r.snapshot.Validate(); err != nil {
		return err
	}
	if err := r.auth.Validate(); err != nil {
		return err
	}
	if err := r.embeddingSpaces.Validate(); err != nil {
		return err
	}
	if len(r.anchors) > inference.MaxEvidenceAnchors {
		return invalid("tool result anchor count is outside the supported range")
	}
	for i, anchor := range r.anchors {
		if err := anchor.Validate(); err != nil {
			return err
		}
		if i > 0 && shoal.CompareID(r.anchors[i-1].ID(), anchor.ID()) >= 0 {
			return invalid("tool result anchors must be unique and canonical")
		}
	}
	return nil
}

type Exchange struct {
	action Action
	result ToolResult
}

func (e Exchange) Action() Action     { return e.action }
func (e Exchange) Result() ToolResult { return cloneToolResult(e.result) }

type Transcript struct {
	id              shoal.ID
	requestID       shoal.ID
	context         inference.ContextPack
	embeddingSpaces interaction.EmbeddingSpaceSet
	exchanges       []Exchange
	final           *Action
}

func newTranscript(request SessionRequest) Transcript {
	t := Transcript{requestID: request.id, context: request.context}
	t.id = transcriptID(t)
	return t
}
func (t Transcript) ID() shoal.ID                   { return t.id }
func (t Transcript) RequestID() shoal.ID            { return t.requestID }
func (t Transcript) Context() inference.ContextPack { return clonePack(t.context) }
func (t Transcript) Exchanges() []Exchange {
	out := make([]Exchange, len(t.exchanges))
	for i := range t.exchanges {
		out[i] = Exchange{action: t.exchanges[i].action, result: cloneToolResult(t.exchanges[i].result)}
	}
	return out
}
func (t Transcript) Final() (Action, bool) {
	if t.final == nil {
		return Action{}, false
	}
	return *t.final, true
}

type Runner interface {
	// Start must stop promptly when ctx is canceled.
	Start(context.Context, SessionRequest) (Session, error)
}
type Session interface {
	// Next must stop promptly when ctx is canceled.
	Next(context.Context, Transcript) (Action, error)
}

type ToolHost interface {
	// Implementations must stop promptly when ctx is canceled.
	Retrieve(context.Context, ToolContext, RetrieveRequest) (ToolResult, error)
	OpenSection(context.Context, ToolContext, OpenSectionRequest) (ToolResult, error)
	Neighbors(context.Context, ToolContext, NeighborsRequest) (ToolResult, error)
}

// ToolContext identifies the exact session projection for one tool call.
type ToolContext struct {
	requestID     shoal.ID
	contextPackID shoal.ID
	context       inference.ContextPack
	budgets       Budgets
	correlation   shoal.ID
	snapshot      inference.SnapshotPin
	auth          inference.AuthPin
}

func (c ToolContext) RequestID() shoal.ID              { return c.requestID }
func (c ToolContext) ContextPackID() shoal.ID          { return c.contextPackID }
func (c ToolContext) Context() inference.ContextPack   { return clonePack(c.context) }
func (c ToolContext) Budgets() Budgets                 { return c.budgets }
func (c ToolContext) CorrelationID() shoal.ID          { return c.correlation }
func (c ToolContext) Snapshot() inference.SnapshotPin  { return c.snapshot }
func (c ToolContext) Authorization() inference.AuthPin { return c.auth }

type Record struct {
	Request    SessionRequest
	Transcript Transcript
	Result     inference.InferenceResult
	Trace      RunTrace
}

type StopReason string

const (
	StopReasonStop            StopReason = "stop"
	StopReasonBudgetExhausted StopReason = "budget_exhausted"
	StopReasonInvalid         StopReason = "invalid"
	StopReasonUnavailable     StopReason = "unavailable"
	StopReasonCanceled        StopReason = "canceled"
	StopReasonDeadline        StopReason = "deadline"
)

type BudgetUsage struct {
	ModelCalls   int
	InputTokens  int
	OutputTokens int
	Evidence     int
	GraphHops    int
	GraphNodes   int
}

type IterationTrace struct {
	Index         int
	Decision      ActionKind
	ActionKey     string
	CorrelationID shoal.ID
	Usage         Usage
	EvidenceIDs   []shoal.ID
	Budget        BudgetUsage
	Failure       string
}

type FailureTrace struct {
	Iteration int
	Operation string
	Error     string
}

type RunTrace struct {
	Budgets    Budgets
	Usage      BudgetUsage
	Iterations []IterationTrace
	StopReason StopReason
	Failures   []FailureTrace
}

// InteractionTurn is one redacted model decision. RetrievedNodeIDs are the
// source graph nodes the turn's tool call put in front of the model. What the
// model was shown is deliberately recorded alongside what it cited, because
// exposure — and therefore the visibility an interaction record requires — is
// determined by everything it saw.
type InteractionTurn struct {
	Index             int
	Decision          ActionKind
	Usage             Usage
	Failed            bool
	ToolKind          ActionKind
	RetrievedNodeIDs  []shoal.ID
	RetrievedEvidence []interaction.EvidenceReference
}

// EvaluationRecord is a redacted deterministic execution record. It contains
// identities and digests, never raw prompts, questions, answers, evidence
// quotes, authorization grants, tool payloads, or model-chosen correlation
// strings.
type EvaluationRecord struct {
	Provenance  Provenance
	Budgets     Budgets
	ActionKinds []ActionKind
	ActionUsage []Usage

	// TranscriptID, RequestID, ContextPackID, ResultID, SnapshotID, and
	// AuthorizationFingerprint are opaque identities.
	// QueryDigest is a one-way digest of the question, present so records can
	// be correlated without persisting the question itself.
	TranscriptID             shoal.ID
	RequestID                shoal.ID
	ContextPackID            shoal.ID
	ResultID                 shoal.ID
	QueryDigest              string
	StopReason               StopReason
	SnapshotID               shoal.ID
	SnapshotAsOf             time.Time
	AuthorizationFingerprint shoal.ID
	AuthorizationExpiresAt   time.Time
	AuthorizationOperation   string
	EmbeddingSpaces          interaction.EmbeddingSpaceSet

	// SeedNodeIDs are source graph nodes the session was shown before its
	// first turn. CitedNodeIDs are the source graph nodes the final answer
	// actually cited. Both are sorted and deduplicated.
	SeedNodeIDs   []shoal.ID
	SeedEvidence  []interaction.EvidenceReference
	Turns         []InteractionTurn
	CitedNodeIDs  []shoal.ID
	CitedEvidence []interaction.EvidenceReference
}

// Recorder durably captures an execution record. Recording is part of serving
// an inference: a Generator cannot be constructed without one, and a recording
// failure fails the request.
type Recorder interface {
	Record(context.Context, EvaluationRecord) error
}

type Generator struct {
	runner     Runner
	tools      ToolHost
	budgets    Budgets
	provenance Provenance
	recorder   Recorder
	cache      Cache
	now        func() time.Time
}

func NewGenerator(runner Runner, tools ToolHost, budgets Budgets, provenance Provenance, recorder Recorder) (*Generator, error) {
	if runner == nil || tools == nil {
		return nil, invalid("runner and tool host are required")
	}
	if recorder == nil {
		return nil, invalid(
			"recorder is required: capture is part of serving an inference")
	}
	budgets = budgets.normalized()
	if err := budgets.validate(); err != nil {
		return nil, err
	}
	if err := provenance.validate(); err != nil {
		return nil, err
	}
	return &Generator{runner: runner, tools: tools, budgets: budgets, provenance: provenance, recorder: recorder, now: time.Now}, nil
}

func NewCachedGenerator(runner Runner, tools ToolHost, budgets Budgets, provenance Provenance, recorder Recorder, cache Cache) (*Generator, error) {
	if cache == nil {
		return nil, invalid("cache is required")
	}
	g, err := NewGenerator(runner, tools, budgets, provenance, recorder)
	if err != nil {
		return nil, err
	}
	g.cache = cache
	return g, nil
}

// SetClock configures the generator clock. Callers that need reproducible
// fixture evaluation can set this before the generator is used.
func (g *Generator) SetClock(now func() time.Time) error {
	if g == nil {
		return invalid("generator is required")
	}
	if now == nil {
		return invalid("clock is required")
	}
	g.now = now
	return nil
}

func (g *Generator) Generate(ctx context.Context, pack inference.ContextPack) (inference.InferenceResult, error) {
	record, err := g.Run(ctx, pack)
	return record.Result, err
}

func (g *Generator) Run(ctx context.Context, pack inference.ContextPack) (Record, error) {
	request, err := newSessionRequest(pack, g.budgets, g.provenance)
	if err != nil {
		return Record{}, err
	}
	initialEvidence := len(pack.Evidence())
	initialGraphNodes := graphNodeSet(pack.Evidence())
	trace := RunTrace{
		Budgets: g.budgets,
		Usage:   BudgetUsage{Evidence: initialEvidence, GraphNodes: len(initialGraphNodes)},
	}
	earlyFinish := func(reason StopReason, operation string, err error) (Record, error) {
		trace.StopReason = reason
		trace.Failures = append(trace.Failures, FailureTrace{
			Iteration: -1,
			Operation: operation,
			Error:     err.Error(),
		})
		return Record{Request: request, Trace: cloneRunTrace(trace)}, err
	}
	if !g.now().Before(pack.Authorization().ExpiresAt()) {
		err := invalid("authorization pin is stale")
		return earlyFinish(StopReasonInvalid, "authorization", err)
	}
	remainingAuth := pack.Authorization().ExpiresAt().Sub(g.now())
	if remainingAuth <= 0 {
		err := invalid("authorization pin is stale")
		return earlyFinish(StopReasonInvalid, "authorization", err)
	}
	if initialEvidence > g.budgets.MaxEvidence {
		err := budget("evidence")
		return earlyFinish(StopReasonBudgetExhausted, "budget", err)
	}
	if len(initialGraphNodes) > g.budgets.MaxGraphNodes {
		err := budget("graph nodes")
		return earlyFinish(StopReasonBudgetExhausted, "budget", err)
	}
	maxElapsed := g.budgets.MaxElapsed
	if remainingAuth < maxElapsed {
		maxElapsed = remainingAuth
	}
	runCtx, cancel := context.WithTimeout(ctx, maxElapsed)
	defer cancel()
	cacheKey, cacheable := CacheKey{}, false
	if g.cache != nil {
		runtimeIdentity, identityErr := runtimeCacheIdentity(g.runner, g.tools)
		key, err := cacheKeyForRequest(request, runtimeIdentity)
		if identityErr == nil && err == nil {
			if cached, ok, err := g.cache.Get(runCtx, key); err == nil && ok {
				if err := runCtx.Err(); err != nil {
					return earlyFinish(stopReasonFor(err), "cache", err)
				}
				if !g.now().Before(pack.Authorization().ExpiresAt()) {
					err := invalid("authorization pin expired during cache lookup")
					return earlyFinish(StopReasonInvalid, "authorization", err)
				}
				if err := validateCachedRecord(cached, request, pack); err == nil {
					evaluation, recordErr := evaluationRecord(cached)
					if recordErr != nil {
						return earlyFinish(
							stopReasonFor(recordErr), "recorder", recordErr)
					}
					if err := g.recorder.Record(runCtx, evaluation); err != nil {
						return earlyFinish(stopReasonFor(err), "recorder", err)
					}
					if err := runCtx.Err(); err != nil {
						return earlyFinish(
							stopReasonFor(err), "recorder",
							explorer.MarkCommittedInteraction(err))
					}
					if !g.now().Before(pack.Authorization().ExpiresAt()) {
						err := invalid("authorization pin expired during cache recording")
						return earlyFinish(
							StopReasonInvalid, "authorization",
							explorer.MarkCommittedInteraction(err))
					}
					if err := runCtx.Err(); err != nil {
						return earlyFinish(
							stopReasonFor(err), "cache",
							explorer.MarkCommittedInteraction(err))
					}
					if !g.now().Before(pack.Authorization().ExpiresAt()) {
						err := invalid("authorization pin expired before cache return")
						return earlyFinish(
							StopReasonInvalid, "authorization",
							explorer.MarkCommittedInteraction(err))
					}
					return cloneRecord(cached), nil
				}
			}
			cacheKey, cacheable = key, true
		}
	}
	session, err := g.runner.Start(runCtx, request)
	if err != nil {
		if runCtx.Err() != nil {
			return earlyFinish(stopReasonFor(runCtx.Err()), "model", runCtx.Err())
		}
		err := fmt.Errorf("%w: %w", ErrRunnerUnavailable, err)
		return earlyFinish(StopReasonUnavailable, "model", err)
	}
	if session == nil {
		return earlyFinish(StopReasonUnavailable, "model", ErrRunnerUnavailable)
	}
	transcript := newTranscript(request)
	seenCorrelation := map[shoal.ID]struct{}{}
	repeats := map[string]int{}
	inputTokens, outputTokens, hops, evidence := 0, 0, 0, len(transcript.context.Evidence())
	graphNodes := graphNodeSet(transcript.context.Evidence())
	currentUsage := func() BudgetUsage {
		return BudgetUsage{
			ModelCalls:   len(trace.Iterations),
			InputTokens:  inputTokens,
			OutputTokens: outputTokens,
			Evidence:     evidence,
			GraphHops:    hops,
			GraphNodes:   len(graphNodes),
		}
	}
	recordCommitted := false
	finish := func(
		reason StopReason,
		iteration int,
		operation string,
		result inference.InferenceResult,
		err error,
	) (Record, error) {
		trace.Usage = currentUsage()
		trace.StopReason = reason
		if err != nil {
			trace.Failures = append(trace.Failures, FailureTrace{
				Iteration: iteration,
				Operation: operation,
				Error:     err.Error(),
			})
			if len(trace.Iterations) > 0 && iteration >= 0 &&
				trace.Iterations[len(trace.Iterations)-1].Index == iteration {
				trace.Iterations[len(trace.Iterations)-1].Failure = err.Error()
			}
		}
		record := Record{
			Request: request, Transcript: cloneTranscript(transcript),
			Result: result, Trace: cloneRunTrace(trace),
		}
		if err == nil && reason == StopReasonStop && cacheable && !unsafeRecordForCache(record) {
			_ = g.cache.Put(runCtx, cacheKey, record)
		}
		if err == nil && reason == StopReasonStop {
			if postErr := runCtx.Err(); postErr != nil {
				trace.StopReason = stopReasonFor(postErr)
				trace.Failures = append(trace.Failures, FailureTrace{
					Iteration: iteration,
					Operation: "cache",
					Error:     postErr.Error(),
				})
				record.Trace = cloneRunTrace(trace)
				if recordCommitted {
					postErr = explorer.MarkCommittedInteraction(postErr)
				}
				return record, postErr
			}
			if !g.now().Before(pack.Authorization().ExpiresAt()) {
				postErr := invalid("authorization pin expired before result return")
				trace.StopReason = StopReasonInvalid
				trace.Failures = append(trace.Failures, FailureTrace{
					Iteration: iteration,
					Operation: "authorization",
					Error:     postErr.Error(),
				})
				record.Trace = cloneRunTrace(trace)
				if recordCommitted {
					postErr = explorer.MarkCommittedInteraction(postErr)
				}
				return record, postErr
			}
		}
		return record, err
	}
	for step := 0; step < g.budgets.MaxSteps; step++ {
		if !g.now().Before(pack.Authorization().ExpiresAt()) {
			err := invalid("authorization pin expired during execution")
			return finish(StopReasonInvalid, step, "authorization", inference.InferenceResult{}, err)
		}
		action, nextErr := session.Next(runCtx, transcript)
		if nextErr != nil {
			if usage, ok := actionErrorUsage(nextErr); ok {
				inputTokens = addSaturating(inputTokens, usage.InputTokens)
				outputTokens = addSaturating(outputTokens, usage.OutputTokens)
				trace.Iterations = append(trace.Iterations, IterationTrace{
					Index: step, Usage: usage, Budget: currentUsage(),
				})
			}
			if runCtx.Err() != nil {
				return finish(stopReasonFor(runCtx.Err()), step, "model", inference.InferenceResult{}, runCtx.Err())
			}
			if errors.Is(nextErr, ErrInvalid) || errors.Is(nextErr, ErrBudgetExhausted) {
				return finish(stopReasonFor(nextErr), step, "model", inference.InferenceResult{}, nextErr)
			}
			err := fmt.Errorf("%w: %w", ErrRunnerUnavailable, nextErr)
			return finish(StopReasonUnavailable, step, "model", inference.InferenceResult{}, err)
		}
		iteration := IterationTrace{
			Index: step, Decision: action.kind, CorrelationID: action.correlation,
			ActionKey: actionKey(action), Usage: action.usage,
		}
		trace.Iterations = append(trace.Iterations, iteration)
		if err := action.validate(); err != nil {
			trace.Iterations[len(trace.Iterations)-1].Budget = currentUsage()
			return finish(StopReasonInvalid, step, "action", inference.InferenceResult{}, err)
		}
		if exceedsRemaining(inputTokens, action.usage.InputTokens, g.budgets.MaxInputTokens) ||
			exceedsRemaining(outputTokens, action.usage.OutputTokens, g.budgets.MaxOutputTokens) {
			inputTokens = addSaturating(inputTokens, action.usage.InputTokens)
			outputTokens = addSaturating(outputTokens, action.usage.OutputTokens)
			trace.Iterations[len(trace.Iterations)-1].Budget = currentUsage()
			err := budget("token")
			return finish(StopReasonBudgetExhausted, step, "budget", inference.InferenceResult{}, err)
		}
		inputTokens += action.usage.InputTokens
		outputTokens += action.usage.OutputTokens
		trace.Iterations[len(trace.Iterations)-1].Budget = currentUsage()
		if err := runCtx.Err(); err != nil {
			return finish(
				stopReasonFor(err), step, "model",
				inference.InferenceResult{}, err,
			)
		}
		if !g.now().Before(pack.Authorization().ExpiresAt()) {
			err := invalid("authorization pin expired during execution")
			return finish(StopReasonInvalid, step, "authorization", inference.InferenceResult{}, err)
		}
		if _, duplicate := seenCorrelation[action.correlation]; duplicate {
			err := invalid("duplicate action correlation ID")
			return finish(StopReasonInvalid, step, "action", inference.InferenceResult{}, err)
		}
		seenCorrelation[action.correlation] = struct{}{}
		key := actionKey(action)
		repeats[key]++
		if repeats[key] > g.budgets.MaxRepeatedAction {
			err := budget("repeated action/cycle")
			return finish(StopReasonBudgetExhausted, step, "budget", inference.InferenceResult{}, err)
		}
		if action.kind == ActionStop {
			runnerResult := cloneResult(action.result)
			if err := runnerResult.ValidateFor(transcript.context); err != nil {
				err := fmt.Errorf("%w: final result: %v", ErrInvalid, err)
				return finish(StopReasonInvalid, step, "final result", inference.InferenceResult{}, err)
			}
			if err := validateResultProvenance(runnerResult, g.provenance); err != nil {
				return finish(StopReasonInvalid, step, "final result", inference.InferenceResult{}, err)
			}
			if transcript.context.Snapshot() != pack.Snapshot() || transcript.context.Authorization() != pack.Authorization() {
				err := invalid("final context pins changed")
				return finish(StopReasonInvalid, step, "final result", inference.InferenceResult{}, err)
			}
			final := action
			transcript.final = &final
			transcript.id = transcriptID(transcript)
			additions := verifiedAdditions(pack, transcript.context)
			issues := append(runnerResult.Unresolved(), runnerResult.Unsupported()...)
			result, err := inference.NewExtendedInferenceResult(
				pack, additions, runnerResult.Claims(), issues,
				runnerResult.GeneratedAt(), runnerResult.Metadata(),
			)
			if err != nil {
				return finish(StopReasonInvalid, step, "final result", inference.InferenceResult{}, err)
			}
			trace.Iterations[len(trace.Iterations)-1].Budget = currentUsage()
			record := Record{
				Request: request, Transcript: cloneTranscript(transcript),
				Result: result, Trace: cloneRunTrace(trace),
			}
			evaluation, recordErr := evaluationRecord(record)
			if recordErr != nil {
				return finish(
					stopReasonFor(recordErr), step, "recorder",
					inference.InferenceResult{}, recordErr,
				)
			}
			if err := g.recorder.Record(runCtx, evaluation); err != nil {
				if explorer.IsCommittedInteraction(err) {
					recordCommitted = true
				}
				return finish(stopReasonFor(err), step, "recorder", inference.InferenceResult{}, err)
			}
			recordCommitted = true
			if err := runCtx.Err(); err != nil {
				return finish(
					stopReasonFor(err), step, "model",
					inference.InferenceResult{},
					explorer.MarkCommittedInteraction(err),
				)
			}
			return finish(StopReasonStop, step, "stop", result, nil)
		}
		var toolResult ToolResult
		toolContext := ToolContext{
			requestID:     request.id,
			contextPackID: transcript.context.ID(),
			context:       transcript.context,
			budgets:       g.budgets,
			correlation:   action.correlation,
			snapshot:      pack.Snapshot(),
			auth:          pack.Authorization(),
		}
		switch action.kind {
		case ActionRetrieve:
			if action.retrieve.limit > g.budgets.MaxFanout {
				err := budget("retrieve fanout")
				return finish(StopReasonBudgetExhausted, step, "budget", inference.InferenceResult{}, err)
			}
			toolResult, err = g.tools.Retrieve(runCtx, toolContext, action.retrieve)
		case ActionOpenSection:
			if !sectionAllowed(transcript.context, action.open) {
				err := invalid("open_section IDs were not issued to this session")
				return finish(StopReasonInvalid, step, "authorization", inference.InferenceResult{}, err)
			}
			toolResult, err = g.tools.OpenSection(runCtx, toolContext, action.open)
		case ActionNeighbors:
			if exceedsRemaining(hops, action.neighbors.hops, g.budgets.MaxGraphHops) ||
				action.neighbors.fanout > g.budgets.MaxFanout {
				err := budget("graph traversal")
				return finish(StopReasonBudgetExhausted, step, "budget", inference.InferenceResult{}, err)
			}
			hops += action.neighbors.hops
			trace.Iterations[len(trace.Iterations)-1].Budget = currentUsage()
			if !nodeAllowed(transcript.context, action.neighbors.nodeID) {
				err := invalid("neighbors node ID was not issued to this session")
				return finish(StopReasonInvalid, step, "authorization", inference.InferenceResult{}, err)
			}
			toolResult, err = g.tools.Neighbors(runCtx, toolContext, action.neighbors)
		default:
			err := invalid("unknown or forbidden action")
			return finish(StopReasonInvalid, step, "action", inference.InferenceResult{}, err)
		}

		if err != nil {
			if runCtx.Err() != nil {
				return finish(stopReasonFor(runCtx.Err()), step, "tool", inference.InferenceResult{}, runCtx.Err())
			}
			return finish(stopReasonFor(err), step, "tool", inference.InferenceResult{}, err)
		}
		if err := runCtx.Err(); err != nil {
			return finish(stopReasonFor(err), step, "tool", inference.InferenceResult{}, err)
		}
		if err := validateToolResult(action, toolResult, pack); err != nil {
			return finish(StopReasonInvalid, step, "tool result", inference.InferenceResult{}, err)
		}
		switch action.kind {
		case ActionRetrieve:
			// The retrieval limit bounds ranked results. A result may add both
			// document and graph evidence anchors, so the global evidence budget
			// accounts for the exact anchor count below.
		case ActionNeighbors:
			if err := validateNeighborResults(action.neighbors, toolResult.anchors); err != nil {
				return finish(StopReasonInvalid, step, "tool result", inference.InferenceResult{}, err)
			}
		case ActionOpenSection:
			if err := validateSectionResults(action.open, toolResult.anchors); err != nil {
				return finish(StopReasonInvalid, step, "tool result", inference.InferenceResult{}, err)
			}
		}
		attemptedEvidenceIDs := anchorIDs(toolResult.anchors)
		newGraphNodes := countNewGraphNodes(graphNodes, toolResult.anchors)
		trace.Iterations[len(trace.Iterations)-1].EvidenceIDs = attemptedEvidenceIDs
		if exceedsRemaining(evidence, len(toolResult.anchors), g.budgets.MaxEvidence) {
			evidence = addSaturating(evidence, len(toolResult.anchors))
			addGraphNodes(graphNodes, toolResult.anchors)
			trace.Iterations[len(trace.Iterations)-1].Budget = currentUsage()
			err := budget("evidence")
			return finish(StopReasonBudgetExhausted, step, "budget", inference.InferenceResult{}, err)
		}
		evidence += len(toolResult.anchors)
		if exceedsRemaining(len(graphNodes), newGraphNodes, g.budgets.MaxGraphNodes) {
			addGraphNodes(graphNodes, toolResult.anchors)
			trace.Iterations[len(trace.Iterations)-1].Budget = currentUsage()
			err := budget("graph nodes")
			return finish(StopReasonBudgetExhausted, step, "budget", inference.InferenceResult{}, err)
		}
		addGraphNodes(graphNodes, toolResult.anchors)
		nextPack, err := addAnchors(
			transcript.context,
			toolResult.anchors,
		)
		if err != nil {
			return finish(StopReasonInvalid, step, "context pack", inference.InferenceResult{}, err)
		}
		transcript.context = nextPack
		transcript.embeddingSpaces, err = mergeEmbeddingSpaces(
			transcript.embeddingSpaces, toolResult.embeddingSpaces)
		if err != nil {
			return finish(
				StopReasonInvalid, step, "embedding provenance",
				inference.InferenceResult{}, err)
		}
		transcript.exchanges = append(transcript.exchanges, Exchange{action: action, result: cloneToolResult(toolResult)})
		transcript.id = transcriptID(transcript)
		trace.Iterations[len(trace.Iterations)-1].Budget = currentUsage()
	}
	stepErr := budget("step")
	return finish(StopReasonBudgetExhausted, g.budgets.MaxSteps, "budget", inference.InferenceResult{}, stepErr)
}

func verifiedAdditions(original, expanded inference.ContextPack) []inference.EvidenceAnchor {
	seen := make(map[shoal.ID]struct{}, len(original.Evidence()))
	for _, anchor := range original.Evidence() {
		seen[anchor.ID()] = struct{}{}
	}
	var additions []inference.EvidenceAnchor
	for _, anchor := range expanded.Evidence() {
		if _, exists := seen[anchor.ID()]; !exists {
			additions = append(additions, anchor)
		}
	}
	return additions
}

func anchorIDs(anchors []inference.EvidenceAnchor) []shoal.ID {
	ids := make([]shoal.ID, len(anchors))
	for i, anchor := range anchors {
		ids[i] = anchor.ID()
	}
	sort.Slice(ids, func(i, j int) bool { return shoal.CompareID(ids[i], ids[j]) < 0 })
	return ids
}

func graphNodeSet(anchors []inference.EvidenceAnchor) map[shoal.ID]struct{} {
	nodes := make(map[shoal.ID]struct{})
	addGraphNodes(nodes, anchors)
	return nodes
}

func countNewGraphNodes(seen map[shoal.ID]struct{}, anchors []inference.EvidenceAnchor) int {
	count := 0
	pending := make(map[shoal.ID]struct{})
	for _, anchor := range anchors {
		path, ok := anchor.Path()
		if !ok {
			continue
		}
		for _, node := range path.Nodes {
			if _, exists := seen[node.ID]; exists {
				continue
			}
			if _, exists := pending[node.ID]; exists {
				continue
			}
			pending[node.ID] = struct{}{}
			count++
		}
	}
	return count
}

func addGraphNodes(seen map[shoal.ID]struct{}, anchors []inference.EvidenceAnchor) {
	for _, anchor := range anchors {
		path, ok := anchor.Path()
		if !ok {
			continue
		}
		for _, node := range path.Nodes {
			seen[node.ID] = struct{}{}
		}
	}
}

func stopReasonFor(err error) StopReason {
	switch {
	case errors.Is(err, context.Canceled):
		return StopReasonCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return StopReasonDeadline
	case errors.Is(err, ErrBudgetExhausted):
		return StopReasonBudgetExhausted
	case errors.Is(err, ErrInvalid):
		return StopReasonInvalid
	default:
		return StopReasonUnavailable
	}
}

func cloneRunTrace(trace RunTrace) RunTrace {
	trace.Iterations = append([]IterationTrace(nil), trace.Iterations...)
	for i := range trace.Iterations {
		trace.Iterations[i].EvidenceIDs = append([]shoal.ID(nil), trace.Iterations[i].EvidenceIDs...)
	}
	trace.Failures = append([]FailureTrace(nil), trace.Failures...)
	return trace
}

func evaluationRecord(record Record) (EvaluationRecord, error) {
	var err error
	evaluation := EvaluationRecord{
		Provenance:  record.Request.provenance,
		Budgets:     record.Request.budgets,
		ActionKinds: make([]ActionKind, 0, len(record.Transcript.exchanges)+1),
		ActionUsage: make([]Usage, 0, len(record.Transcript.exchanges)+1),

		TranscriptID:             record.Transcript.id,
		RequestID:                record.Request.id,
		ContextPackID:            record.Request.context.ID(),
		ResultID:                 record.Result.ID(),
		QueryDigest:              digestString(record.Request.context.Query()),
		StopReason:               record.Trace.StopReason,
		SnapshotID:               record.Request.context.Snapshot().ID(),
		SnapshotAsOf:             record.Request.context.Snapshot().AsOf(),
		AuthorizationFingerprint: record.Request.context.Authorization().Fingerprint(),
		AuthorizationExpiresAt:   record.Request.context.Authorization().ExpiresAt(),
		EmbeddingSpaces:          record.Transcript.embeddingSpaces,
	}
	if evaluation.StopReason == "" && record.Transcript.final != nil {
		evaluation.StopReason = StopReasonStop
	}
	add := func(action Action) {
		evaluation.ActionKinds = append(evaluation.ActionKinds, action.kind)
		evaluation.ActionUsage = append(evaluation.ActionUsage, action.usage)
	}
	failedIterations := make(map[int]struct{}, len(record.Trace.Failures))
	for _, failure := range record.Trace.Failures {
		if failure.Iteration >= 0 {
			failedIterations[failure.Iteration] = struct{}{}
		}
	}
	evaluation.SeedEvidence, err = evidenceReferences(
		record.Request.context.Evidence())
	if err != nil {
		return EvaluationRecord{}, err
	}
	evaluation.SeedNodeIDs = sourceNodeIDs(record.Request.context.Evidence())
	for index, exchange := range record.Transcript.exchanges {
		add(exchange.action)
		turn := InteractionTurn{
			Index:            index,
			Decision:         exchange.action.kind,
			Usage:            exchange.action.usage,
			ToolKind:         exchange.result.kind,
			RetrievedNodeIDs: sourceNodeIDs(exchange.result.anchors),
		}
		turn.RetrievedEvidence, err = evidenceReferences(
			exchange.result.anchors)
		if err != nil {
			return EvaluationRecord{}, err
		}
		if _, failed := failedIterations[index]; failed {
			turn.Failed = true
		}
		evaluation.Turns = append(evaluation.Turns, turn)
	}
	if record.Transcript.final != nil {
		add(*record.Transcript.final)
		index := len(record.Transcript.exchanges)
		turn := InteractionTurn{
			Index:    index,
			Decision: record.Transcript.final.kind,
			Usage:    record.Transcript.final.usage,
		}
		if _, failed := failedIterations[index]; failed {
			turn.Failed = true
		}
		evaluation.Turns = append(evaluation.Turns, turn)
	}
	evaluation.CitedNodeIDs = citedSourceNodeIDs(
		record.Result, record.Transcript.context.Evidence())
	evaluation.CitedEvidence, err = citedEvidenceReferences(
		record.Result, record.Transcript.context.Evidence())
	if err != nil {
		return EvaluationRecord{}, err
	}
	return evaluation, nil
}

func evidenceReferences(
	anchors []inference.EvidenceAnchor,
) ([]interaction.EvidenceReference, error) {
	if len(anchors) == 0 {
		return nil, nil
	}
	result := make([]interaction.EvidenceReference, 0, len(anchors))
	for _, anchor := range anchors {
		reference, err := anchor.EvidenceReference()
		if err != nil {
			return nil, err
		}
		result = append(result, reference)
	}
	sort.Slice(result, func(i, j int) bool {
		return shoal.CompareID(
			result[i].AnchorID, result[j].AnchorID) < 0
	})
	return result, nil
}

func citedEvidenceReferences(
	result inference.InferenceResult,
	available []inference.EvidenceAnchor,
) ([]interaction.EvidenceReference, error) {
	cited := make(map[shoal.ID]struct{})
	for _, claim := range result.Claims() {
		for _, id := range claim.EvidenceIDs() {
			cited[id] = struct{}{}
		}
	}
	for _, issue := range append(result.Unresolved(), result.Unsupported()...) {
		for _, id := range issue.EvidenceIDs() {
			cited[id] = struct{}{}
		}
	}
	anchors := append(append([]inference.EvidenceAnchor(nil), available...),
		result.EvidenceAdditions()...)
	selected := make([]inference.EvidenceAnchor, 0, len(cited))
	for _, anchor := range anchors {
		if _, ok := cited[anchor.ID()]; ok {
			selected = append(selected, anchor)
		}
	}
	return evidenceReferences(selected)
}

// sourceNodeIDs projects evidence anchors onto the source graph nodes they
// expose: the document, section, and span behind a document citation, and
// every node on a graph anchor's path.
func sourceNodeIDs(anchors []inference.EvidenceAnchor) []shoal.ID {
	seen := make(map[shoal.ID]struct{})
	for _, anchor := range anchors {
		addSourceNodeIDs(seen, anchor)
	}
	return sortedIDs(seen)
}

// citedSourceNodeIDs projects the anchors a result's claims and issues
// actually referenced onto their source graph nodes. available is the full
// anchor set the session ended with, because a claim may cite an anchor the
// session was seeded with rather than one it added.
func citedSourceNodeIDs(
	result inference.InferenceResult, available []inference.EvidenceAnchor,
) []shoal.ID {
	cited := make(map[shoal.ID]struct{})
	for _, claim := range result.Claims() {
		for _, id := range claim.EvidenceIDs() {
			cited[id] = struct{}{}
		}
	}
	for _, issue := range append(result.Unresolved(), result.Unsupported()...) {
		for _, id := range issue.EvidenceIDs() {
			cited[id] = struct{}{}
		}
	}
	seen := make(map[shoal.ID]struct{})
	anchors := append(append([]inference.EvidenceAnchor(nil), available...),
		result.EvidenceAdditions()...)
	for _, anchor := range anchors {
		if _, ok := cited[anchor.ID()]; !ok {
			continue
		}
		addSourceNodeIDs(seen, anchor)
	}
	return sortedIDs(seen)
}

func addSourceNodeIDs(seen map[shoal.ID]struct{}, anchor inference.EvidenceAnchor) {
	if citation, _, ok := anchor.Document(); ok {
		for _, id := range []shoal.ID{
			citation.DocumentID, citation.SectionID, citation.SpanID,
		} {
			if id != "" {
				seen[id] = struct{}{}
			}
		}
	}
	if path, ok := anchor.Path(); ok {
		for _, node := range path.Nodes {
			if node.ID != "" {
				seen[node.ID] = struct{}{}
			}
		}
	}
}

func sortedIDs(seen map[shoal.ID]struct{}) []shoal.ID {
	if len(seen) == 0 {
		return nil
	}
	ids := make([]shoal.ID, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return shoal.CompareID(ids[i], ids[j]) < 0 })
	return ids
}

func digestString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func validateToolResult(action Action, result ToolResult, original inference.ContextPack) error {
	if err := result.validate(); err != nil {
		return err
	}
	if result.correlation != action.correlation || result.kind != action.kind {
		return invalid("tool result does not match action")
	}
	if result.snapshot != original.Snapshot() || result.auth != original.Authorization() {
		return invalid("tool result snapshot or authorization pin is stale")
	}
	return nil
}

func addAnchors(
	pack inference.ContextPack,
	additions []inference.EvidenceAnchor,
) (inference.ContextPack, error) {
	anchors := pack.Evidence()
	seen := make(map[shoal.ID]struct{}, len(anchors)+len(additions))
	for _, a := range anchors {
		seen[a.ID()] = struct{}{}
	}
	for _, a := range additions {
		if _, duplicate := seen[a.ID()]; duplicate {
			continue
		}
		seen[a.ID()] = struct{}{}
		anchors = append(anchors, a)
	}
	ontology, ok := pack.Ontology()
	var ontologyPtr *inference.OntologyIdentity
	if ok {
		ontologyPtr = &ontology
	}
	return inference.NewContextPack(
		pack.Query(), anchors, ontologyPtr,
		pack.Snapshot(), pack.Authorization(), pack.Metadata())
}

func mergeEmbeddingSpaces(
	left, right interaction.EmbeddingSpaceSet,
) (interaction.EmbeddingSpaceSet, error) {
	if err := left.Validate(); err != nil {
		return interaction.EmbeddingSpaceSet{}, err
	}
	if err := right.Validate(); err != nil {
		return interaction.EmbeddingSpaceSet{}, err
	}
	identities := append(
		append([]string(nil), left.Identities...),
		right.Identities...,
	)
	return interaction.NewEmbeddingSpaceSet(identities)
}

func actionKey(a Action) string {
	switch a.kind {
	case ActionRetrieve:
		return framed(string(a.kind), a.retrieve.query, strconv.Itoa(a.retrieve.limit))
	case ActionOpenSection:
		return framed(string(a.kind), string(a.open.documentID), string(a.open.revisionID), string(a.open.sectionID))
	case ActionNeighbors:
		return framed(string(a.kind), string(a.neighbors.nodeID), strconv.Itoa(a.neighbors.hops), strconv.Itoa(a.neighbors.fanout))
	default:
		return framed(string(a.kind))
	}
}

func framed(parts ...string) string {
	var builder strings.Builder
	for _, part := range parts {
		builder.WriteString(strconv.Itoa(len(part)))
		builder.WriteByte(':')
		builder.WriteString(part)
	}
	return builder.String()
}

func modelIdentity(model inference.ModelProvenance) string {
	parameters := model.Parameters()
	keys := make([]string, 0, len(parameters))
	for key := range parameters {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := []string{model.Provider(), model.Model(), model.Version()}
	for _, key := range keys {
		parts = append(parts, key, parameters[key])
	}
	if seed, ok := model.Seed(); ok {
		parts = append(parts, "seed", strconv.FormatInt(seed, 10))
	} else {
		parts = append(parts, "no-seed")
	}
	return framed(parts...)
}

func exceedsRemaining(current, delta, maximum int) bool {
	return delta > maximum-current
}
func addSaturating(left, right int) int {
	if right > int(^uint(0)>>1)-left {
		return int(^uint(0) >> 1)
	}
	return left + right
}

func transcriptID(t Transcript) shoal.ID {
	parts := []string{string(t.requestID), string(t.context.ID())}
	for _, e := range t.exchanges {
		parts = append(parts, string(e.action.kind), string(e.action.correlation), actionKey(e.action),
			strconv.Itoa(e.action.usage.InputTokens), strconv.Itoa(e.action.usage.OutputTokens),
			e.result.embeddingSpaces.Digest)
		for _, a := range e.result.anchors {
			parts = append(parts, string(a.ID()))
		}
	}
	if t.final != nil {
		parts = append(parts, string(t.final.kind), string(t.final.correlation), actionKey(*t.final),
			strconv.Itoa(t.final.usage.InputTokens), strconv.Itoa(t.final.usage.OutputTokens), string(t.final.result.ID()))
	}
	return deriveID("transcript", parts...)
}
func deriveID(namespace string, parts ...string) shoal.ID {
	h := sha256.New()
	for _, part := range append([]string{namespace}, parts...) {
		fmt.Fprintf(h, "%d:", len(part))
		h.Write([]byte(part))
	}
	return shoal.ID(namespace + ":" + hex.EncodeToString(h.Sum(nil)))
}
func canonicalBudgets(b Budgets) string {
	return fmt.Sprintf("%d/%d/%d/%d/%d/%d/%d/%d/%d",
		b.MaxSteps, b.MaxElapsed.Nanoseconds(), b.MaxInputTokens, b.MaxOutputTokens,
		b.MaxEvidence, b.MaxGraphHops, b.MaxGraphNodes, b.MaxFanout, b.MaxRepeatedAction)
}
func validateLogicalID(name string, id shoal.ID) error {
	if len(id) > MaxLogicalIDBytes {
		return invalid(name + " exceeds the byte bound")
	}
	if err := shoal.ValidateRequiredID(name, id); err != nil {
		return err
	}
	return nil
}
func boundedString(name, value string, limit int) error {
	if !utf8.ValidString(value) || strings.TrimSpace(value) == "" {
		return invalid(name + " is required and must be valid UTF-8")
	}
	if len(value) > limit {
		return invalid(name + " exceeds the byte bound")
	}
	return nil
}
func invalid(message string) error { return fmt.Errorf("%w: %s", ErrInvalid, message) }
func budget(name string) error     { return fmt.Errorf("%w: %s", ErrBudgetExhausted, name) }
func clonePack(pack inference.ContextPack) inference.ContextPack {
	ontology, ok := pack.Ontology()
	var ptr *inference.OntologyIdentity
	if ok {
		ptr = &ontology
	}
	cloned, _ := inference.NewContextPack(pack.Query(), pack.Evidence(), ptr, pack.Snapshot(), pack.Authorization(), pack.Metadata())
	return cloned
}
func cloneResult(result inference.InferenceResult) inference.InferenceResult { return result }
func cloneToolResult(r ToolResult) ToolResult {
	r.anchors = append([]inference.EvidenceAnchor(nil), r.anchors...)
	r.embeddingSpaces.Identities = append(
		[]string(nil), r.embeddingSpaces.Identities...)
	return r
}
func cloneTranscript(t Transcript) Transcript {
	t.context = clonePack(t.context)
	t.embeddingSpaces.Identities = append(
		[]string(nil), t.embeddingSpaces.Identities...)
	t.exchanges = append([]Exchange(nil), t.exchanges...)
	for i := range t.exchanges {
		t.exchanges[i].result = cloneToolResult(t.exchanges[i].result)
	}

	var _ inference.Generator = (*Generator)(nil)
	if t.final != nil {
		final := *t.final
		t.final = &final
	}
	return t
}
