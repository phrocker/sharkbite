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
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/contextpack"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/workspace"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/inference/harness"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/reasoning"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const chatPolicyID shoal.ID = "shoal-chat-source-evidence-v1"

type AskRequest struct {
	Question string `json:"question"`
	TopK     uint32 `json:"top_k,omitempty"`
	// Compatibility fields are deliberately ignored. Identity, reason,
	// correlation and operation always come from trusted server state.
	Actor         json.RawMessage `json:"actor,omitempty"`
	Reason        json.RawMessage `json:"reason,omitempty"`
	CorrelationID json.RawMessage `json:"correlation_id,omitempty"`
	Operation     json.RawMessage `json:"operation,omitempty"`
}

// AskProvider returns only verified, durably recorded reasoning responses.
type AskProvider interface {
	Ask(context.Context, AskRequest) (CitationEnvelope, error)
}

type ChatConfig struct {
	Client         *authorized.Client
	Resolver       auth.Resolver
	Generator      model.TextGenerator
	Model          inference.ModelProvenance
	Budgets        harness.Budgets
	Limits         workspace.Limits
	RetrievalModes []retrieval.Mode
	Clock          func() time.Time
}

// ChatService composes the existing inference harness, evidence verifier and
// mandatory interaction sink. Its client is always authorization enforcing.
type ChatService struct {
	client         *authorized.Client
	resolver       auth.Resolver
	generator      model.TextGenerator
	model          inference.ModelProvenance
	budgets        harness.Budgets
	limits         workspace.Limits
	retrievalModes []retrieval.Mode
	clock          func() time.Time
	cache          *harness.MemoryCache
}

// EgressesOffHost reports whether this service's configured model provider
// transmits content off the host.
//
// The answer comes from the generator that was actually configured, because it
// is not a property of this code: the same service is egress-free against a
// loopback provider and egress-bearing against a hosted one. A generator that
// cannot classify itself is treated as egressing, which model.EgressesOffHost
// handles.
//
// It exists so AskExecutor can declare an honest effect ceiling. The executor
// holds an AskProvider rather than a generator, and this is the seam where the
// generator actually lives.
func (s *ChatService) EgressesOffHost() bool {
	if s == nil {
		return true
	}
	return model.EgressesOffHost(s.generator)
}

type citationDocumentKey struct {
	documentID shoal.ID
	revisionID shoal.ID
}

func NewChatService(ctx context.Context, config ChatConfig) (*ChatService, error) {
	if ctx == nil || config.Client == nil ||
		isAbsentInterface(config.Resolver) || isAbsentInterface(config.Generator) {
		return nil, chatInvalid("authorized client, resolver, generator and context are required")
	}
	if config.Budgets == (harness.Budgets{}) {
		config.Budgets = harness.Budgets{
			MaxSteps: 8, MaxElapsed: 30 * time.Second,
			MaxInputTokens: 16384, MaxOutputTokens: 1024, MaxEvidence: 128,
			MaxGraphHops: int(MaxDepth), MaxGraphNodes: int(MaxNodes), MaxFanout: 32,
			MaxRepeatedAction: 2,
		}
	}
	budgets, err := harness.NormalizeBudgets(config.Budgets)
	if err != nil {
		return nil, err
	}
	if config.Limits == (workspace.Limits{}) {
		config.Limits = workspace.Limits{
			RetrievalTopK: 32, GraphDepth: 4, GraphFanout: 32,
			GraphNodes: MaxNodes, OutputBytes: 1 << 20,
		}
	}
	if config.Limits.RetrievalTopK > MaxTopK ||
		config.Limits.GraphDepth > MaxDepth ||
		config.Limits.GraphFanout > MaxFanout ||
		config.Limits.GraphNodes > MaxNodes ||
		config.Limits.OutputBytes > MaxResponseBytes {
		return nil, chatInvalid("chat limits exceed public transport limits")
	}
	if config.Clock == nil {
		config.Clock = time.Now
	}
	if len(config.RetrievalModes) == 0 {
		config.RetrievalModes = []retrieval.Mode{
			retrieval.ModeLexical, retrieval.ModeTree,
		}
	}
	normalizedRetrieval, err := (retrieval.Request{
		Text: "validate", TopK: 1, Modes: config.RetrievalModes,
	}).Normalize()
	if err != nil {
		return nil, err
	}
	if _, err := chatProvenance(config.Model); err != nil {
		return nil, err
	}
	cache, err := harness.NewMemoryCache(harness.MemoryCacheConfig{})
	if err != nil {
		return nil, err
	}
	return &ChatService{
		client: config.Client, resolver: config.Resolver,
		generator: config.Generator, model: config.Model,
		budgets: budgets, limits: config.Limits, clock: config.Clock,
		retrievalModes: append([]retrieval.Mode(nil), normalizedRetrieval.Modes...),
		cache:          cache,
	}, nil
}

func (s *ChatService) Ask(ctx context.Context, input AskRequest) (CitationEnvelope, error) {
	if s == nil || ctx == nil {
		return CitationEnvelope{}, chatInvalid("chat service and context are required")
	}
	if strings.TrimSpace(input.Question) == "" || !utf8.ValidString(input.Question) ||
		len(input.Question) > 32*1024 {
		return CitationEnvelope{}, chatInvalid("question must be nonempty UTF-8 within 32768 bytes")
	}
	decision, err := s.resolver.Resolve(ctx)
	if err != nil {
		return CitationEnvelope{}, err
	}
	if err := s.authorize(decision); err != nil {
		return CitationEnvelope{}, err
	}
	productRecorder, err := interaction.NewRecorder(ctx, s.client)
	if err != nil {
		return CitationEnvelope{}, err
	}
	if err := productRecorder.SetClock(s.clock); err != nil {
		return CitationEnvelope{}, err
	}
	limits := s.limits
	var extraVisibility []string
	var settingsID shoal.ID
	var settingsRevision uint64
	var cacheDimensions map[string]uint64
	if effective, ok := EffectiveWorkspaceSettings(ctx); ok {
		configured := effective.Limits()
		limits.RetrievalTopK = min(limits.RetrievalTopK, configured.RetrievalTopK)
		limits.GraphDepth = min(limits.GraphDepth, configured.GraphDepth)
		limits.GraphFanout = min(limits.GraphFanout, configured.GraphFanout)
		limits.GraphNodes = min(limits.GraphNodes, configured.GraphNodes)
		limits.OutputBytes = min(limits.OutputBytes, configured.OutputBytes)
		if len(effective.OutputPolicies()) > 0 {
			label, err := effective.OutputVisibility()
			if err != nil {
				return CitationEnvelope{}, err
			}
			if len(label) > 0 {
				extraVisibility, err = interaction.ParseVisibility(string(label))
				if err != nil {
					return CitationEnvelope{}, err
				}
			}
		}
		settingsID = effective.SettingsID()
		settingsRevision = effective.Revision()
		cacheDimensions = effective.CacheDimensions()
	}
	if limits.RetrievalTopK == 0 || limits.GraphFanout == 0 ||
		limits.GraphNodes == 0 || limits.OutputBytes == 0 {
		return CitationEnvelope{}, shoal.NewError(shoal.ErrorUnauthorized, "workspace disables chat resources")
	}
	if input.TopK == 0 {
		input.TopK = min(uint32(8), limits.RetrievalTopK)
	}
	if input.TopK > limits.RetrievalTopK {
		return CitationEnvelope{}, chatInvalid("top_k exceeds the effective workspace limit")
	}
	budgets := s.budgets
	budgets.MaxGraphHops = min(budgets.MaxGraphHops, int(limits.GraphDepth))
	budgets.MaxGraphNodes = min(budgets.MaxGraphNodes, int(limits.GraphNodes))
	budgets.MaxFanout = min(budgets.MaxFanout, int(limits.GraphFanout))
	ctx, cancel := context.WithTimeout(ctx, budgets.MaxElapsed)
	defer cancel()
	snapshot, err := s.client.Snapshot(ctx)
	if err != nil {
		return CitationEnvelope{}, err
	}
	snapshotPin, err := inference.NewSnapshotPin(shoal.ID(snapshot.ID), snapshot.AsOf)
	if err != nil {
		return CitationEnvelope{}, err
	}
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		return CitationEnvelope{}, err
	}
	authPin, err := inference.NewAuthPin(shoal.ID(fingerprint.String()), decision.AuthenticationExpires())
	if err != nil {
		return CitationEnvelope{}, err
	}
	if err := s.client.ValidateAuthorization(ctx, authPin); err != nil {
		return CitationEnvelope{}, err
	}
	request := retrieval.Request{
		Text: input.Question, TopK: input.TopK,
		Modes: append([]retrieval.Mode(nil), s.retrievalModes...), Explain: true,
	}
	retrieved, err := s.client.Retrieve(ctx, request)
	if err != nil {
		return CitationEnvelope{}, err
	}
	retrieved.RequestID = decision.RequestID()
	retrievalRecorder := &chatRetrievalRecorder{
		recorder: productRecorder, snapshot: snapshotPin, authorization: authPin,
		correlationID: correlationIDForDecision(decision), clock: s.clock,
		requiredVisibility: extraVisibility,
	}
	if err := retrievalRecorder.Record(ctx, request, retrieved); err != nil {
		return CitationEnvelope{}, fmt.Errorf("capture initial retrieval: %w", err)
	}
	reader := chatContextReader{client: s.client, limits: limits}
	builder := contextpack.Builder{Reader: reader, Limits: contextpack.Limits{
		MaxResults: max(int(input.TopK), budgets.MaxFanout),
		MaxAnchors: budgets.MaxEvidence, MaxGraphNodes: budgets.MaxGraphNodes,
		MaxGraphEdges: budgets.MaxGraphNodes * budgets.MaxFanout,
		MaxPathNodes:  min(budgets.MaxGraphNodes, inference.MaxPathNodes),
	}}
	pins := contextpack.Pins{
		Snapshot: snapshotPin, Authorization: authPin, PolicyID: chatPolicyID,
	}
	if selected, ok := decision.SelectedOntology(); ok {
		pins.Ontology = &selected
	}
	request.AsOf = snapshotPin.AsOf()
	pack, err := builder.Build(ctx, contextpack.InitialRequest{
		Request: request, Response: retrieved, Pins: pins,
	})
	if err != nil {
		return CitationEnvelope{}, err
	}
	recorder, err := harness.NewGraphRecorder(ctx, s.client)
	if err != nil {
		return CitationEnvelope{}, err
	}
	if err := recorder.SetClock(s.clock); err != nil {
		return CitationEnvelope{}, err
	}
	runner, err := harness.NewModelRunner(s.generator, harness.ModelRunnerConfig{
		MaxOutputTokens: budgets.MaxOutputTokens,
	})
	if err != nil {
		return CitationEnvelope{}, err
	}
	host, err := harness.NewExplorerToolHost(chatToolClient{
		Client: s.client, limits: limits, recorder: retrievalRecorder,
	}, builder)
	if err != nil {
		return CitationEnvelope{}, err
	}
	host.PolicyID = chatPolicyID
	host.RetrievalModes = request.Modes
	host.RetrievalExplain = true
	cacheIdentity := chatCacheIdentity(settingsID, settingsRevision, cacheDimensions)
	host.ClientIdentity = cacheIdentity
	host.BoundedClientIdentity = cacheIdentity
	host.BuilderReaderIdentity = cacheIdentity
	provenance, err := chatProvenance(s.model)
	if err != nil {
		return CitationEnvelope{}, err
	}
	captureRecorder := &chatEvaluationRecorder{delegate: recorder}
	generator, err := harness.NewCachedGenerator(
		runner, host, budgets, provenance, captureRecorder, s.cache)
	if err != nil {
		return CitationEnvelope{}, err
	}
	if err := generator.SetClock(s.clock); err != nil {
		return CitationEnvelope{}, err
	}
	record, err := generator.Run(ctx, pack)
	if err != nil {
		return CitationEnvelope{}, fmt.Errorf("run recorded chat inference: %w", err)
	}
	verified, err := reasoning.NewBuilderWithLimits(reader, builder.Limits)
	if err != nil {
		return CitationEnvelope{}, err
	}
	prepared, err := verified.Build(ctx, reasoning.BuildInput{
		ContextPack: record.Request.Context(), Result: record.Result,
		Policy: reasoning.Policy{ID: chatPolicyID, ExtraOutputVisibility: extraVisibility},
	})
	if err != nil {
		return CitationEnvelope{}, fmt.Errorf("verify chat evidence: %w", err)
	}
	evaluation, ok := captureRecorder.Evaluation()
	if !ok {
		return CitationEnvelope{}, shoal.NewError(
			shoal.ErrorInternal, "chat execution record was not captured")
	}
	executionSession, err := harness.InteractionSession(evaluation, s.clock().UTC())
	if err != nil {
		return CitationEnvelope{}, err
	}
	correlationID := decision.CorrelationID()
	if correlationID == "" {
		correlationID = decision.RequestID()
	}
	metadata := prepared.CaptureMetadata()
	session, err := metadata.NewSession(
		interaction.OperationChat, correlationID, executionSession.RecordedAt)
	if err != nil {
		return CitationEnvelope{}, err
	}
	session.Provenance = executionSession.Provenance
	session.QueryDigest = executionSession.QueryDigest
	session.StopReason = executionSession.StopReason
	session.Turns = executionSession.Turns
	session.RequiredVisibility = metadata.EffectiveOutputVisibility()
	captured, err := prepared.Capture(ctx, productRecorder, session)
	if err != nil {
		return CitationEnvelope{}, fmt.Errorf("capture verified chat response: %w", err)
	}
	response := NewCitationEnvelope(captured)
	if err := s.hydrateCitationLinks(ctx, &response); err != nil {
		return CitationEnvelope{}, explorer.MarkCommittedInteraction(err)
	}
	if err := s.client.ValidateAuthorization(ctx, authPin); err != nil {
		return CitationEnvelope{}, explorer.MarkCommittedInteraction(err)
	}
	if _, err := verified.Build(ctx, reasoning.BuildInput{
		ContextPack: record.Request.Context(), Result: record.Result,
		Policy: reasoning.Policy{
			ID: chatPolicyID, ExtraOutputVisibility: extraVisibility,
		},
	}); err != nil {
		return CitationEnvelope{}, explorer.MarkCommittedInteraction(
			fmt.Errorf("reauthorize recorded chat evidence: %w", err))
	}
	response.Finalized = true
	response.DurablyRecorded = true
	response.Verification = reasoning.VerificationVerified
	response.OutputVisibility = chatVisibility(response.EffectiveVisibility)
	response.WorkspaceSettingsID = settingsID
	response.WorkspaceSettingsRevision = settingsRevision
	if selected, ok := decision.SelectedOntology(); ok {
		response.OntologyInterpretation = &OntologyInterpretation{
			Status: "selected", SchemaID: selected.SchemaID(),
			VersionID: selected.VersionID(),
		}
	} else {
		response.OntologyInterpretation = &OntologyInterpretation{
			Status: "unresolved",
		}
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return CitationEnvelope{}, explorer.MarkCommittedInteraction(err)
	}
	if uint64(len(encoded)) > limits.OutputBytes {
		return CitationEnvelope{}, explorer.MarkCommittedInteraction(
			shoal.NewError(shoal.ErrorUnavailable, "recorded chat output exceeds workspace byte limit"))
	}
	return response, nil
}

func (s *ChatService) hydrateCitationLinks(
	ctx context.Context, response *CitationEnvelope,
) error {
	uris := make(map[citationDocumentKey]string)
	hydrate := func(evidence []CitationEvidence) error {
		for index := range evidence {
			citation := evidence[index].Citation
			if citation == nil {
				continue
			}
			key := citationDocumentKey{
				documentID: citation.DocumentID,
				revisionID: citation.RevisionID,
			}
			uri, ok := uris[key]
			if !ok {
				document, err := s.client.Document(
					ctx, citation.DocumentID, citation.RevisionID)
				if err != nil {
					return fmt.Errorf("resolve citation source link: %w", err)
				}
				uri = document.SourceURI
				uris[key] = uri
			}
			evidence[index].SourceURI = uri
		}
		return nil
	}
	if err := hydrate(response.Evidence); err != nil {
		return err
	}
	return nil
}

type chatEvaluationRecorder struct {
	delegate harness.Recorder
	mu       sync.Mutex
	value    harness.EvaluationRecord
	set      bool
}

func (r *chatEvaluationRecorder) Record(
	ctx context.Context, value harness.EvaluationRecord,
) error {
	if err := r.delegate.Record(ctx, value); err != nil {
		return err
	}
	r.mu.Lock()
	r.value = value
	r.set = true
	r.mu.Unlock()
	return nil
}

func (r *chatEvaluationRecorder) Evaluation() (harness.EvaluationRecord, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.value, r.set
}

func chatCacheIdentity(
	settingsID shoal.ID,
	revision uint64,
	dimensions map[string]uint64,
) string {
	keys := make([]string, 0, len(dimensions))
	for key := range dimensions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var value strings.Builder
	value.WriteString("web-chat-v1:")
	value.WriteString(encodeOptionalID(settingsID))
	value.WriteByte(':')
	value.WriteString(strconv.FormatUint(revision, 10))
	for _, key := range keys {
		value.WriteByte(':')
		value.WriteString(key)
		value.WriteByte('=')
		value.WriteString(strconv.FormatUint(dimensions[key], 10))
	}
	return value.String()
}

func validateFinalizedChatResponse(response CitationEnvelope) error {
	if !response.Finalized || !response.DurablyRecorded ||
		response.Verification != reasoning.VerificationVerified {
		return chatInvalid("chat response is not verified and durably finalized")
	}
	if response.OutputVisibility != chatVisibility(response.EffectiveVisibility) {
		return chatInvalid("chat output visibility does not match verified evidence")
	}
	if response.WorkspaceSettingsRevision == 0 &&
		response.WorkspaceSettingsID != "" ||
		response.WorkspaceSettingsRevision != 0 &&
			response.WorkspaceSettingsID == "" {
		return chatInvalid("chat workspace settings identity is incomplete")
	}
	if response.OntologyInterpretation == nil {
		return chatInvalid("chat ontology interpretation status is required")
	}
	switch response.OntologyInterpretation.Status {
	case "unresolved":
		if response.OntologyInterpretation.SchemaID != "" ||
			response.OntologyInterpretation.VersionID != "" {
			return chatInvalid("unresolved ontology interpretation has an identity")
		}
	case "selected":
		if err := shoal.ValidateRequiredID(
			"chat ontology schema ID",
			response.OntologyInterpretation.SchemaID,
		); err != nil {
			return err
		}
		if err := shoal.ValidateRequiredID(
			"chat ontology version ID",
			response.OntologyInterpretation.VersionID,
		); err != nil {
			return err
		}
	default:
		return chatInvalid("chat ontology interpretation status is invalid")
	}
	return nil
}

func chatVisibility(labels []string) string {
	if value := interaction.Expression(labels); value != "" {
		return value
	}
	return "public"
}

func (s *ChatService) authorize(decision auth.Decision) error {
	return decision.Authorize(auth.OperationRetrieve, auth.ResourceRequest{
		AuthorizationDomain: decision.AuthorizationDomain(),
	}, s.clock())
}

func chatProvenance(modelProvenance inference.ModelProvenance) (harness.Provenance, error) {
	prompt, err := inference.NewPromptProvenance("shoal-recorded-chat", "v1", harness.ModelPromptTemplateHash())
	if err != nil {
		return harness.Provenance{}, err
	}
	return harness.NewProvenance("shoal-recorded-chat-v1", modelProvenance, prompt, string(chatPolicyID))
}

type chatToolClient struct {
	*authorized.Client
	limits   workspace.Limits
	recorder *chatRetrievalRecorder
}

func (c chatToolClient) Retrieve(ctx context.Context, request retrieval.Request) (retrieval.Response, error) {
	if request.TopK == 0 || request.TopK > c.limits.RetrievalTopK {
		return retrieval.Response{}, chatInvalid("tool retrieval exceeds workspace limit")
	}
	response, err := c.Client.Retrieve(ctx, request)
	if err != nil {
		return retrieval.Response{}, err
	}
	if response.RequestID == "" && c.recorder != nil {
		response.RequestID = c.recorder.requestID()
	}
	if c.recorder != nil {
		if err := c.recorder.Record(ctx, request, response); err != nil {
			return retrieval.Response{}, fmt.Errorf("capture tool retrieval: %w", err)
		}
	}
	return response, nil
}

func (c chatToolClient) BoundedNeighborhood(ctx context.Context, request explorer.BoundedNeighborhoodRequest) (explorer.BoundedNeighborhood, error) {
	if request.Depth > c.limits.GraphDepth || request.Fanout > c.limits.GraphFanout ||
		request.MaxNodes > c.limits.GraphNodes ||
		request.MaxScannedEdges > c.limits.GraphNodes*c.limits.GraphFanout {
		return explorer.BoundedNeighborhood{}, chatInvalid("tool graph expansion exceeds workspace limits")
	}
	return c.Client.BoundedNeighborhood(ctx, request)
}

func (c chatToolClient) Neighborhood(ctx context.Context, request explorer.NeighborhoodRequest) (explorer.Neighborhood, error) {
	return (chatContextReader{client: c.Client, limits: c.limits}).Neighborhood(ctx, request)
}

type chatContextReader struct {
	client *authorized.Client
	limits workspace.Limits
}

func (r chatContextReader) Snapshot(ctx context.Context) (explorer.Snapshot, error) {
	return r.client.Snapshot(ctx)
}

func (r chatContextReader) ValidateAuthorization(
	ctx context.Context, pin inference.AuthPin,
) error {
	return r.client.ValidateAuthorization(ctx, pin)
}

func (r chatContextReader) Document(ctx context.Context, documentID, revisionID shoal.ID) (explorer.DocumentView, error) {
	return r.client.Document(ctx, documentID, revisionID)
}

func (r chatContextReader) Neighborhood(ctx context.Context, request explorer.NeighborhoodRequest) (explorer.Neighborhood, error) {
	if request.Depth > r.limits.GraphDepth {
		return explorer.Neighborhood{}, chatInvalid("graph expansion exceeds workspace depth")
	}
	page, err := r.client.BoundedNeighborhood(ctx, explorer.BoundedNeighborhoodRequest{
		NodeIDs: request.NodeIDs, Depth: request.Depth, EdgeTypes: request.EdgeTypes,
		Fanout: r.limits.GraphFanout, MaxNodes: r.limits.GraphNodes,
		MaxScannedEdges: r.limits.GraphNodes * r.limits.GraphFanout,
		Direction:       explorer.GraphDirectionOutgoing,
	})
	if err != nil {
		return explorer.Neighborhood{}, err
	}
	if page.Truncated || page.Continuation || page.NextAfterEdgeID != "" {
		return explorer.Neighborhood{}, shoal.NewError(shoal.ErrorUnavailable, "chat evidence graph is incomplete within workspace limits")
	}
	return page.Neighborhood, nil
}

func chatInvalid(message string) error {
	return shoal.NewError(shoal.ErrorInvalidArgument, message)
}

type chatRetrievalRecorder struct {
	recorder           *interaction.Recorder
	snapshot           inference.SnapshotPin
	authorization      inference.AuthPin
	correlationID      shoal.ID
	requiredVisibility []string
	clock              func() time.Time
	mu                 sync.Mutex
	ordinal            uint64
}

func (r *chatRetrievalRecorder) requestID() shoal.ID {
	if r == nil {
		return ""
	}
	return r.correlationID
}

func (r *chatRetrievalRecorder) Record(
	ctx context.Context,
	request retrieval.Request,
	response retrieval.Response,
) error {
	if r == nil || r.recorder == nil {
		return chatInvalid("chat retrieval recorder is required")
	}
	r.mu.Lock()
	r.ordinal++
	ordinal := r.ordinal
	r.mu.Unlock()
	recordedAt := r.clock().UTC()
	operationCorrelation := interaction.DerivedID(
		"chat_retrieval",
		string(r.correlationID),
		strconv.FormatUint(ordinal, 10),
	)
	sessionID, err := interaction.OperationSessionID(
		interaction.OperationRetrieval, operationCorrelation, recordedAt)
	if err != nil {
		return err
	}
	session := interaction.Session{
		ID: sessionID, RecordedAt: recordedAt,
		Operation:  interaction.OperationRetrieval,
		SnapshotID: r.snapshot.ID(), SnapshotAsOf: r.snapshot.AsOf(),
		AuthorizationFingerprint: r.authorization.Fingerprint(),
		AuthorizationExpiresAt:   r.authorization.ExpiresAt(),
		EmbeddingSpaceID:         response.EmbeddingSpaceID,
		QueryDigest:              interaction.Digest(request.Text),
		RequestID:                response.RequestID,
		RequiredVisibility: append(
			[]string(nil), r.requiredVisibility...),
		SeedNodeIDs: retrievalSourceIDs(response),
	}
	_, err = r.recorder.Record(ctx, session)
	return err
}

func retrievalSourceIDs(response retrieval.Response) []shoal.ID {
	var ids []shoal.ID
	for _, result := range response.Results {
		for _, evidence := range result.Evidence {
			if evidence.Citation.DocumentID != "" {
				ids = append(ids, evidence.Citation.DocumentID)
			}
			if evidence.Citation.SectionID != "" {
				ids = append(ids, evidence.Citation.SectionID)
			}
			if evidence.Citation.SpanID != "" {
				ids = append(ids, evidence.Citation.SpanID)
			}
			for _, node := range evidence.Path.Nodes {
				ids = append(ids, node.ID)
			}
			for _, edge := range evidence.Path.Edges {
				ids = append(ids, edge.From, edge.To)
			}
		}
	}
	return dedupeOpaqueIDs(ids)
}

func correlationIDForDecision(decision auth.Decision) shoal.ID {
	if correlationID := decision.CorrelationID(); correlationID != "" {
		return correlationID
	}
	return decision.RequestID()
}
