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

package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	lowmodel "github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const ModelPromptMarker = "shoal-harness-action-json/v1"

const modelPromptInstruction = "Return only one JSON object. Use action retrieve, open_section, neighbors, or stop. " +
	"All JSON ID fields are base64-encoded opaque ID bytes. Final stop claims must cite evidence_ids from this context; " +
	"put unsupported or ungrounded output in unsupported."

func modelPromptTools() []string {
	return []string{
		string(ActionRetrieve), string(ActionOpenSection),
		string(ActionNeighbors), string(ActionStop),
	}
}

func modelPromptSchemas() []promptSchema {
	return []promptSchema{
		{
			Action: ActionRetrieve,
			Required: []string{
				"action=retrieve", "correlation_id", "query", "limit",
			},
			Description: "Retrieve ranked evidence for query with positive limit. correlation_id is base64-encoded opaque ID bytes.",
		},
		{
			Action: ActionOpenSection,
			Required: []string{
				"action=open_section", "correlation_id", "document_id", "revision_id", "section_id",
			},
			Description: "Open exactly one visible section previously present in evidence. All *_id fields are base64-encoded opaque ID bytes.",
		},
		{
			Action: ActionNeighbors,
			Required: []string{
				"action=neighbors", "correlation_id", "node_id", "hops", "fanout",
			},
			Description: "Expand from one visible graph node with positive hops and fanout. node_id is base64-encoded opaque ID bytes.",
		},
		{
			Action: ActionStop,
			Required: []string{
				"action=stop", "correlation_id",
				"claims[].subject", "claims[].predicate",
				"claims[].object.type", "claims[].object.value",
				"claims[].confidence", "claims[].evidence_ids",
				"unresolved[].input", "unresolved[].reason",
				"unsupported[].input", "unsupported[].reason",
			},
			Description: "Stop with at least one claim, unresolved issue, or unsupported issue. " +
				"Ontology object types are string, integer, number, boolean, timestamp, or reference. " +
				"Every claim evidence_ids entry must be copied from evidence[].id.",
		},
	}
}

// ModelPromptTemplateHash returns the canonical SHA-256 digest for the
// model-action prompt template used by ModelRunner.
func ModelPromptTemplateHash() string {
	wireExample, err := modelPromptWireExample()
	if err != nil {
		panic(err)
	}
	payload := struct {
		Protocol    string          `json:"protocol"`
		Tools       []string        `json:"tools"`
		Schemas     []promptSchema  `json:"schemas"`
		Instruction string          `json:"instruction"`
		WireSchema  []string        `json:"wire_schema"`
		WireExample json.RawMessage `json:"wire_example"`
	}{
		Protocol:    ModelPromptMarker,
		Tools:       modelPromptTools(),
		Schemas:     modelPromptSchemas(),
		Instruction: modelPromptInstruction,
		WireSchema:  wireSchemaDescriptors(reflect.TypeOf(promptEnvelope{})),
		WireExample: wireExample,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func modelPromptWireExample() (json.RawMessage, error) {
	example := promptEnvelope{
		Protocol: ModelPromptMarker,
		Tools:    modelPromptTools(),
		Budgets: Budgets{
			MaxSteps:          1,
			MaxElapsed:        time.Second,
			MaxInputTokens:    2,
			MaxOutputTokens:   3,
			MaxEvidence:       4,
			MaxGraphHops:      5,
			MaxGraphNodes:     6,
			MaxFanout:         7,
			MaxRepeatedAction: 8,
		},
		Consumed: BudgetUsage{ModelCalls: 1, InputTokens: 2, OutputTokens: 3, Evidence: 4, GraphHops: 5, GraphNodes: 6},
		Remaining: BudgetUsage{
			ModelCalls: 7, InputTokens: 8, OutputTokens: 9, Evidence: 10, GraphHops: 11, GraphNodes: 12,
		},
		Snapshot:  promptSnapshot{ID: newProtocolID("snapshot"), AsOf: "2000-01-02T03:04:05Z"},
		Auth:      promptAuth{Fingerprint: newProtocolID("auth"), ExpiresAt: "2000-01-02T03:04:05Z"},
		Query:     "question",
		ContextID: newProtocolID("context"),
		Evidence: []promptEvidence{{
			ID:   newProtocolID("evidence"),
			Kind: inference.AnchorDocument,
			Citation: &promptCitation{
				DocumentID: newProtocolID("document"),
				RevisionID: newProtocolID("revision"),
				SectionID:  newProtocolID("section"),
				SpanID:     newProtocolID("span"),
				Range: document.SourceRange{
					Start: document.SourcePosition{Offset: 13, Page: 14},
					End:   document.SourcePosition{Offset: 15, Page: 16},
				},
			},
			Quote: "quote",
			Path: &promptPath{
				Nodes: []promptNode{{
					ID:         newProtocolID("node"),
					Kind:       "kind",
					Labels:     []string{"label"},
					Properties: []promptMetadataEntry{{Key: newProtocolBytes("node-key"), Value: newProtocolBytes("node-value")}},
				}},
				Edges: []promptEdge{{
					ID:     newProtocolID("edge"),
					From:   newProtocolID("from"),
					To:     newProtocolID("to"),
					Type:   "edge-type",
					Weight: shoal.Score(0.5),
					Properties: []promptMetadataEntry{{
						Key: newProtocolBytes("edge-key"), Value: newProtocolBytes("edge-value"),
					}},
				}},
			},
		}},
		Transcript: []promptExchange{{
			Action:      ActionRetrieve,
			Correlation: newProtocolID("correlation"),
			Query:       "query",
			Limit:       1,
			DocumentID:  newProtocolID("document"),
			RevisionID:  newProtocolID("revision"),
			SectionID:   newProtocolID("section"),
			NodeID:      newProtocolID("node"),
			Hops:        2,
			Fanout:      3,
			Usage:       Usage{InputTokens: 4, OutputTokens: 5},
			EvidenceIDs: []protocolID{newProtocolID("evidence")},
		}},
		Schemas:     modelPromptSchemas(),
		Instruction: modelPromptInstruction,
	}
	return json.Marshal(example)
}

func wireSchemaDescriptors(roots ...reflect.Type) []string {
	seen := make(map[reflect.Type]struct{})
	descriptors := make([]string, 0, len(roots))
	var walk func(reflect.Type)
	walk = func(t reflect.Type) {
		if t == nil {
			return
		}
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		switch t.Kind() {
		case reflect.Array, reflect.Slice:
			walk(t.Elem())
			return
		case reflect.Map:
			walk(t.Key())
			walk(t.Elem())
			return
		case reflect.Struct:
		default:
			return
		}
		if _, ok := seen[t]; ok {
			return
		}
		seen[t] = struct{}{}
		descriptors = append(descriptors, typeDescriptor(t))
		for i := 0; i < t.NumField(); i++ {
			walk(t.Field(i).Type)
		}
	}
	for _, root := range roots {
		walk(root)
	}
	sort.Strings(descriptors)
	return descriptors
}

func typeDescriptor(t reflect.Type) string {
	var builder strings.Builder
	builder.WriteString(t.PkgPath())
	builder.WriteByte('.')
	builder.WriteString(t.Name())
	builder.WriteByte('{')
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		builder.WriteString(field.Name)
		builder.WriteByte(':')
		builder.WriteString(field.Type.String())
		builder.WriteByte(':')
		builder.WriteString(string(field.Tag))
		builder.WriteByte(';')
	}
	builder.WriteByte('}')
	return builder.String()
}

type ModelRunnerConfig struct {
	MaxOutputTokens int
	TokenEstimator  TextTokenEstimator
	Now             func() time.Time
	ClockIdentity   string
}

type TextTokenEstimator interface {
	EstimateTextTokens(context.Context, string) (int, error)
}

type ModelRunner struct {
	generator lowmodel.TextGenerator
	cfg       ModelRunnerConfig
}

func NewModelRunner(generator lowmodel.TextGenerator, cfg ModelRunnerConfig) (*ModelRunner, error) {
	if generator == nil {
		return nil, invalid("model text generator is required")
	}
	if cfg.MaxOutputTokens < 0 {
		return nil, invalid("model max output tokens cannot be negative")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &ModelRunner{generator: generator, cfg: cfg}, nil
}

func (r *ModelRunner) Start(ctx context.Context, request SessionRequest) (Session, error) {
	if r == nil || r.generator == nil {
		return nil, ErrRunnerUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := request.context.Validate(); err != nil {
		return nil, err
	}
	return &modelSession{runner: r, request: request}, nil
}

func (r *ModelRunner) CacheIdentity() (string, error) {
	if r == nil || r.generator == nil {
		return "", ErrCacheIdentityUnsafe
	}
	generatorIdentity, err := dependencyCacheIdentity(r.generator, "model generator")
	if err != nil {
		return "", err
	}
	estimatorIdentity := "byte-estimator-v1"
	if r.cfg.TokenEstimator != nil {
		estimatorIdentity, err = dependencyCacheIdentity(r.cfg.TokenEstimator, "token estimator")
		if err != nil {
			return "", err
		}
	}
	clockIdentity := strings.TrimSpace(r.cfg.ClockIdentity)
	if clockIdentity == "" || unsafeCacheText(clockIdentity) {
		return "", ErrCacheIdentityUnsafe
	}
	identity := framed(
		"model-runner-v1",
		generatorIdentity,
		strconv.Itoa(r.cfg.MaxOutputTokens),
		estimatorIdentity,
		clockIdentity,
	)
	if unsafeCacheText(identity) {
		return "", ErrCacheIdentityUnsafe
	}
	return identity, nil
}

type modelSession struct {
	runner  *ModelRunner
	request SessionRequest
}

func (s *modelSession) Next(ctx context.Context, transcript Transcript) (Action, error) {
	if err := ctx.Err(); err != nil {
		return Action{}, err
	}
	prompt, err := modelPrompt(s.request, transcript)
	if err != nil {
		return Action{}, err
	}
	remainingInput := s.request.budgets.MaxInputTokens - transcriptInputTokens(transcript)
	if remainingInput <= 0 {
		return Action{}, budget("input token")
	}
	estimatedInput, err := s.runner.estimateInputTokens(ctx, prompt)
	if err != nil {
		return Action{}, err
	}
	if estimatedInput < 0 {
		return Action{}, invalid("input token estimate cannot be negative")
	}
	if estimatedInput > remainingInput {
		return Action{}, budget("input token")
	}
	remainingOutput := s.request.budgets.MaxOutputTokens - transcriptOutputTokens(transcript)
	if remainingOutput <= 0 {
		return Action{}, budget("output token")
	}
	maxOutput := remainingOutput
	if s.runner.cfg.MaxOutputTokens > 0 && s.runner.cfg.MaxOutputTokens < maxOutput {
		maxOutput = s.runner.cfg.MaxOutputTokens
	}
	generated, err := s.runner.generator.Generate(ctx, lowmodel.GenerateRequest{
		Prompt:          prompt,
		MaxOutputTokens: maxOutput,
	})
	if err != nil {
		if errors.Is(err, lowmodel.ErrOversizedResponse) {
			return Action{}, newActionError(budget("output token"), Usage{})
		}
		return Action{}, newActionError(err, Usage{})
	}
	usage := Usage{
		InputTokens:  generated.Usage.InputTokens,
		OutputTokens: generated.Usage.OutputTokens,
	}
	if usage.InputTokens < 0 || usage.OutputTokens < 0 {
		return Action{}, newActionError(invalid("model token usage cannot be negative"), Usage{})
	}
	if err := validateGeneratedProvenance(generated.Provenance, s.request.provenance.model); err != nil {
		return Action{}, newActionError(err, usage)
	}
	action, err := parseModelAction(generated.Text, transcript.context, s.request.provenance, usage, s.runner.cfg.Now())
	if err != nil {
		return Action{}, newActionError(err, usage)
	}
	return action, nil
}

func (r *ModelRunner) estimateInputTokens(ctx context.Context, prompt string) (int, error) {
	if r.cfg.TokenEstimator != nil {
		return r.cfg.TokenEstimator.EstimateTextTokens(ctx, prompt)
	}
	return len([]byte(prompt)), nil
}

func transcriptInputTokens(transcript Transcript) int {
	total := 0
	for _, exchange := range transcript.exchanges {
		total += exchange.action.usage.InputTokens
	}
	if transcript.final != nil {
		total += transcript.final.usage.InputTokens
	}
	return total
}

func validateGeneratedProvenance(generated lowmodel.Provenance, expected inference.ModelProvenance) error {
	if strings.TrimSpace(generated.Provider) == "" || strings.TrimSpace(generated.Model) == "" {
		return invalid("model generation omitted provider provenance")
	}
	if generated.Provider != expected.Provider() || generated.Model != expected.Model() {
		return invalid("model generation provenance does not match the harness session")
	}
	return nil
}

type actionError struct {
	err   error
	usage Usage
}

func newActionError(err error, usage Usage) error {
	return actionError{err: err, usage: usage}
}

func (e actionError) Error() string { return e.err.Error() }
func (e actionError) Unwrap() error { return e.err }

func actionErrorUsage(err error) (Usage, bool) {
	var actionErr actionError
	if errors.As(err, &actionErr) {
		return actionErr.usage, true
	}
	return Usage{}, false
}

func transcriptOutputTokens(transcript Transcript) int {
	total := 0
	for _, exchange := range transcript.exchanges {
		total += exchange.action.usage.OutputTokens
	}
	if transcript.final != nil {
		total += transcript.final.usage.OutputTokens
	}
	return total
}

type promptEnvelope struct {
	Protocol    string           `json:"protocol"`
	Tools       []string         `json:"tools"`
	Budgets     Budgets          `json:"budgets"`
	Consumed    BudgetUsage      `json:"consumed"`
	Remaining   BudgetUsage      `json:"remaining"`
	Snapshot    promptSnapshot   `json:"snapshot"`
	Auth        promptAuth       `json:"authorization"`
	Query       string           `json:"query"`
	ContextID   protocolID       `json:"context_id"`
	Evidence    []promptEvidence `json:"evidence"`
	Transcript  []promptExchange `json:"transcript"`
	Schemas     []promptSchema   `json:"schemas"`
	Instruction string           `json:"instruction"`
}

type promptSnapshot struct {
	ID   protocolID `json:"id"`
	AsOf string     `json:"as_of"`
}

type promptAuth struct {
	Fingerprint protocolID `json:"fingerprint"`
	ExpiresAt   string     `json:"expires_at"`
}

type promptEvidence struct {
	ID       protocolID           `json:"id"`
	Kind     inference.AnchorKind `json:"kind"`
	Citation *promptCitation      `json:"citation,omitempty"`
	Quote    string               `json:"quote,omitempty"`
	Path     *promptPath          `json:"path,omitempty"`
}

type promptExchange struct {
	Action      ActionKind   `json:"action"`
	Correlation protocolID   `json:"correlation_id"`
	Query       string       `json:"query,omitempty"`
	Limit       int          `json:"limit,omitempty"`
	DocumentID  protocolID   `json:"document_id,omitempty"`
	RevisionID  protocolID   `json:"revision_id,omitempty"`
	SectionID   protocolID   `json:"section_id,omitempty"`
	NodeID      protocolID   `json:"node_id,omitempty"`
	Hops        int          `json:"hops,omitempty"`
	Fanout      int          `json:"fanout,omitempty"`
	Usage       Usage        `json:"usage"`
	EvidenceIDs []protocolID `json:"evidence_ids"`
}

type promptCitation struct {
	DocumentID protocolID           `json:"document_id"`
	RevisionID protocolID           `json:"revision_id"`
	SectionID  protocolID           `json:"section_id,omitempty"`
	SpanID     protocolID           `json:"span_id,omitempty"`
	Range      document.SourceRange `json:"range"`
}

type promptPath struct {
	Nodes []promptNode `json:"nodes"`
	Edges []promptEdge `json:"edges"`
}

type promptNode struct {
	ID         protocolID            `json:"id"`
	Kind       string                `json:"kind,omitempty"`
	Labels     []string              `json:"labels,omitempty"`
	Properties []promptMetadataEntry `json:"properties,omitempty"`
}

type promptEdge struct {
	ID         protocolID            `json:"id"`
	From       protocolID            `json:"from"`
	To         protocolID            `json:"to"`
	Type       string                `json:"type"`
	Weight     shoal.Score           `json:"weight"`
	Properties []promptMetadataEntry `json:"properties,omitempty"`
}

type promptMetadataEntry struct {
	Key   protocolBytes `json:"key"`
	Value protocolBytes `json:"value"`
}

type promptSchema struct {
	Action      ActionKind `json:"action"`
	Required    []string   `json:"required"`
	Description string     `json:"description"`
}

type protocolID shoal.ID

type protocolBytes string

func newProtocolID(id shoal.ID) protocolID {
	return protocolID(id)
}

func (id protocolID) shoalID() shoal.ID {
	return shoal.ID(id)
}

func (id protocolID) MarshalJSON() ([]byte, error) {
	return json.Marshal(base64.StdEncoding.EncodeToString([]byte(id)))
}

func newProtocolBytes(value string) protocolBytes {
	return protocolBytes(value)
}

func (value protocolBytes) MarshalJSON() ([]byte, error) {
	return json.Marshal(base64.StdEncoding.EncodeToString([]byte(value)))
}

func (id *protocolID) UnmarshalJSON(data []byte) error {
	var encoded string
	if err := json.Unmarshal(data, &encoded); err != nil {
		return err
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return invalid("protocol ID is not valid base64")
	}
	if len(decoded) > shoal.MaxIDBytes {
		return invalid("protocol ID exceeds the public byte bound")
	}
	*id = protocolID(shoal.ID(string(decoded)))
	return nil
}

func protocolIDs(ids []shoal.ID) []protocolID {
	if len(ids) == 0 {
		return nil
	}
	result := make([]protocolID, 0, len(ids))
	for _, id := range ids {
		result = append(result, newProtocolID(id))
	}
	return result
}

func shoalIDs(ids []protocolID) []shoal.ID {
	if len(ids) == 0 {
		return nil
	}
	result := make([]shoal.ID, 0, len(ids))
	for _, id := range ids {
		result = append(result, id.shoalID())
	}
	return result
}

func promptCitationFrom(citation document.Citation) *promptCitation {
	return &promptCitation{
		DocumentID: newProtocolID(citation.DocumentID),
		RevisionID: newProtocolID(citation.RevisionID),
		SectionID:  newProtocolID(citation.SectionID),
		SpanID:     newProtocolID(citation.SpanID),
		Range:      citation.Range,
	}
}

func promptPathFrom(path graph.Path) *promptPath {
	result := promptPath{
		Nodes: make([]promptNode, 0, len(path.Nodes)),
		Edges: make([]promptEdge, 0, len(path.Edges)),
	}
	for _, node := range path.Nodes {
		result.Nodes = append(result.Nodes, promptNode{
			ID:         newProtocolID(node.ID),
			Kind:       node.Kind,
			Labels:     append([]string(nil), node.Labels...),
			Properties: promptMetadataFrom(node.Properties),
		})
	}
	for _, edge := range path.Edges {
		result.Edges = append(result.Edges, promptEdge{
			ID:         newProtocolID(edge.ID),
			From:       newProtocolID(edge.From),
			To:         newProtocolID(edge.To),
			Type:       edge.Type,
			Weight:     edge.Weight,
			Properties: promptMetadataFrom(edge.Properties),
		})
	}
	return &result
}

func promptMetadataFrom(metadata shoal.Metadata) []promptMetadataEntry {
	if len(metadata) == 0 {
		return nil
	}
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(left, right int) bool {
		return shoal.CompareID(shoal.ID(keys[left]), shoal.ID(keys[right])) < 0
	})
	entries := make([]promptMetadataEntry, 0, len(keys))
	for _, key := range keys {
		entries = append(entries, promptMetadataEntry{
			Key:   newProtocolBytes(key),
			Value: newProtocolBytes(metadata[key]),
		})
	}
	return entries
}

func modelPrompt(request SessionRequest, transcript Transcript) (string, error) {
	contextEvidence := transcript.context.Evidence()
	evidence := make([]promptEvidence, 0, len(contextEvidence))
	for _, anchor := range contextEvidence {
		item := promptEvidence{ID: newProtocolID(anchor.ID()), Kind: anchor.Kind()}
		if citation, quote, ok := anchor.Document(); ok {
			item.Citation = promptCitationFrom(citation)
			item.Quote = quote
		}
		if path, ok := anchor.Path(); ok {
			item.Path = promptPathFrom(path)
		}
		evidence = append(evidence, item)
	}
	exchanges := make([]promptExchange, 0, len(transcript.exchanges))
	for _, exchange := range transcript.exchanges {
		item := promptExchange{
			Action:      exchange.action.kind,
			Correlation: newProtocolID(exchange.action.correlation),
			Usage:       exchange.action.usage,
			EvidenceIDs: protocolIDs(anchorIDs(exchange.result.anchors)),
		}
		switch exchange.action.kind {
		case ActionRetrieve:
			item.Query = exchange.action.retrieve.query
			item.Limit = exchange.action.retrieve.limit
		case ActionOpenSection:
			item.DocumentID = newProtocolID(exchange.action.open.documentID)
			item.RevisionID = newProtocolID(exchange.action.open.revisionID)
			item.SectionID = newProtocolID(exchange.action.open.sectionID)
		case ActionNeighbors:
			item.NodeID = newProtocolID(exchange.action.neighbors.nodeID)
			item.Hops = exchange.action.neighbors.hops
			item.Fanout = exchange.action.neighbors.fanout
		}
		exchanges = append(exchanges, item)
	}
	consumed := promptConsumed(request.context, transcript)
	envelope := promptEnvelope{
		Protocol:    ModelPromptMarker,
		Tools:       modelPromptTools(),
		Budgets:     request.budgets,
		Consumed:    consumed,
		Remaining:   promptRemaining(request.budgets, consumed),
		Snapshot:    promptSnapshot{ID: newProtocolID(request.context.Snapshot().ID()), AsOf: request.context.Snapshot().AsOf().Format(time.RFC3339Nano)},
		Auth:        promptAuth{Fingerprint: newProtocolID(request.context.Authorization().Fingerprint()), ExpiresAt: request.context.Authorization().ExpiresAt().Format(time.RFC3339Nano)},
		Query:       request.context.Query(),
		ContextID:   newProtocolID(transcript.context.ID()),
		Evidence:    evidence,
		Transcript:  exchanges,
		Schemas:     modelPromptSchemas(),
		Instruction: modelPromptInstruction,
	}
	payload, err := json.Marshal(envelope)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

func promptConsumed(initial inference.ContextPack, transcript Transcript) BudgetUsage {
	usage := BudgetUsage{
		ModelCalls: len(transcript.exchanges),
		Evidence:   len(initial.Evidence()),
		GraphNodes: len(graphNodeSet(transcript.context.Evidence())),
	}
	for _, exchange := range transcript.exchanges {
		usage.InputTokens += exchange.action.usage.InputTokens
		usage.OutputTokens += exchange.action.usage.OutputTokens
		usage.Evidence += len(exchange.result.anchors)
		if exchange.action.kind == ActionNeighbors {
			usage.GraphHops += exchange.action.neighbors.hops
		}
	}
	return usage
}

func promptRemaining(budgets Budgets, consumed BudgetUsage) BudgetUsage {
	return BudgetUsage{
		ModelCalls:   nonnegative(budgets.MaxSteps - consumed.ModelCalls),
		InputTokens:  nonnegative(budgets.MaxInputTokens - consumed.InputTokens),
		OutputTokens: nonnegative(budgets.MaxOutputTokens - consumed.OutputTokens),
		Evidence:     nonnegative(budgets.MaxEvidence - consumed.Evidence),
		GraphHops:    nonnegative(budgets.MaxGraphHops - consumed.GraphHops),
		GraphNodes:   nonnegative(budgets.MaxGraphNodes - consumed.GraphNodes),
	}
}

func nonnegative(value int) int {
	if value < 0 {
		return 0
	}
	return value
}

type modelActionPayload struct {
	Action        ActionKind          `json:"action"`
	CorrelationID protocolID          `json:"correlation_id"`
	Query         string              `json:"query"`
	Limit         int                 `json:"limit"`
	DocumentID    protocolID          `json:"document_id"`
	RevisionID    protocolID          `json:"revision_id"`
	SectionID     protocolID          `json:"section_id"`
	NodeID        protocolID          `json:"node_id"`
	Hops          int                 `json:"hops"`
	Fanout        int                 `json:"fanout"`
	Claims        []modelClaimPayload `json:"claims"`
	Unresolved    []modelIssuePayload `json:"unresolved"`
	Unsupported   []modelIssuePayload `json:"unsupported"`
}

type modelClaimPayload struct {
	Subject     protocolID      `json:"subject"`
	Predicate   protocolID      `json:"predicate"`
	Object      json.RawMessage `json:"object"`
	Confidence  *shoal.Score    `json:"confidence"`
	EvidenceIDs []protocolID    `json:"evidence_ids"`
}

type modelIssuePayload struct {
	Input       string       `json:"input"`
	Reason      string       `json:"reason"`
	EvidenceIDs []protocolID `json:"evidence_ids"`
}

func parseModelAction(
	text string,
	pack inference.ContextPack,
	provenance Provenance,
	usage Usage,
	generatedAt time.Time,
) (Action, error) {
	var payload modelActionPayload
	decoder := json.NewDecoder(bytes.NewReader([]byte(strings.TrimSpace(text))))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return Action{}, invalid("model output is not a valid action JSON object")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Action{}, invalid("model output contains multiple JSON values")
	}
	switch payload.Action {
	case ActionRetrieve:
		request, err := NewRetrieveRequest(payload.Query, payload.Limit)
		if err != nil {
			return Action{}, err
		}
		return NewRetrieveAction(payload.CorrelationID.shoalID(), request, usage)
	case ActionOpenSection:
		request, err := NewOpenSectionRequest(
			payload.DocumentID.shoalID(), payload.RevisionID.shoalID(), payload.SectionID.shoalID())
		if err != nil {
			return Action{}, err
		}
		return NewOpenSectionAction(payload.CorrelationID.shoalID(), request, usage)
	case ActionNeighbors:
		request, err := NewNeighborsRequest(payload.NodeID.shoalID(), payload.Hops, payload.Fanout)
		if err != nil {
			return Action{}, err
		}
		return NewNeighborsAction(payload.CorrelationID.shoalID(), request, usage)
	case ActionStop:
		result, err := resultFromPayload(pack, provenance, payload, generatedAt)
		if err != nil {
			return Action{}, fmt.Errorf("%w: model stop result: %v", ErrInvalid, err)
		}
		return NewStopAction(payload.CorrelationID.shoalID(), result, usage)
	default:
		return Action{}, invalid("model selected an unknown or forbidden action")
	}
}

func resultFromPayload(
	pack inference.ContextPack,
	provenance Provenance,
	payload modelActionPayload,
	generatedAt time.Time,
) (inference.InferenceResult, error) {
	claims := make([]inference.Claim, 0, len(payload.Claims))
	for _, item := range payload.Claims {
		if item.Confidence == nil {
			return inference.InferenceResult{}, invalid("claim confidence is required")
		}
		value, err := parseOntologyValue(item.Object)
		if err != nil {
			return inference.InferenceResult{}, err
		}
		claim, err := inference.NewClaim(
			item.Subject.shoalID(), item.Predicate.shoalID(), value, *item.Confidence, shoalIDs(item.EvidenceIDs),
			inference.ClaimInferred, provenance.model, provenance.prompt, nil)
		if err != nil {
			return inference.InferenceResult{}, err
		}
		claims = append(claims, claim)
	}
	issues := make([]inference.Issue, 0, len(payload.Unresolved)+len(payload.Unsupported))
	for _, item := range payload.Unresolved {
		issue, err := inference.NewIssue(
			inference.IssueUnresolved, item.Input, item.Reason, shoalIDs(item.EvidenceIDs))
		if err != nil {
			return inference.InferenceResult{}, err
		}
		issues = append(issues, issue)
	}
	for _, item := range payload.Unsupported {
		issue, err := inference.NewIssue(
			inference.IssueUnsupported, item.Input, item.Reason, shoalIDs(item.EvidenceIDs))
		if err != nil {
			return inference.InferenceResult{}, err
		}
		issues = append(issues, issue)
	}
	return inference.NewInferenceResult(pack, claims, issues, generatedAt, nil)
}

type ontologyValuePayload struct {
	Type  ontology.ValueType `json:"type"`
	Value json.RawMessage    `json:"value"`
}

func parseOntologyValue(raw json.RawMessage) (ontology.Value, error) {
	var payload ontologyValuePayload
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return ontology.Value{}, invalid("claim object is not a valid ontology value")
	}
	switch payload.Type {
	case ontology.ValueString:
		var value string
		if err := json.Unmarshal(payload.Value, &value); err != nil {
			return ontology.Value{}, invalid("string claim object has invalid value")
		}
		return ontology.NewStringValue(value)
	case ontology.ValueInteger:
		var number json.Number
		if err := json.Unmarshal(payload.Value, &number); err != nil {
			return ontology.Value{}, invalid("integer claim object has invalid value")
		}
		value, err := strconv.ParseInt(number.String(), 10, 64)
		if err != nil {
			return ontology.Value{}, invalid("integer claim object is out of range")
		}
		return ontology.NewIntegerValue(value), nil
	case ontology.ValueNumber:
		var number json.Number
		if err := json.Unmarshal(payload.Value, &number); err != nil {
			return ontology.Value{}, invalid("number claim object has invalid value")
		}
		value, err := strconv.ParseFloat(number.String(), 64)
		if err != nil {
			return ontology.Value{}, invalid("number claim object is invalid")
		}
		return ontology.NewNumberValue(value)
	case ontology.ValueBoolean:
		var value bool
		if err := json.Unmarshal(payload.Value, &value); err != nil {
			return ontology.Value{}, invalid("boolean claim object has invalid value")
		}
		return ontology.NewBooleanValue(value), nil
	case ontology.ValueTimestamp:
		var value string
		if err := json.Unmarshal(payload.Value, &value); err != nil {
			return ontology.Value{}, invalid("timestamp claim object has invalid value")
		}
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return ontology.Value{}, invalid("timestamp claim object is invalid")
		}
		return ontology.NewTimestampValue(parsed)
	case ontology.ValueReference:
		var value protocolID
		if err := json.Unmarshal(payload.Value, &value); err != nil {
			return ontology.Value{}, invalid("reference claim object has invalid value")
		}
		return ontology.NewReferenceValue(value.shoalID())
	default:
		return ontology.Value{}, invalid(fmt.Sprintf("unsupported claim object type %q", payload.Type))
	}
}
