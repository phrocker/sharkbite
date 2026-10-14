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
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/reasoning"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	ontologyRelationshipIDProperty  = "ontology_relationship_id"
	ontologyAssertionIDProperty     = "ontology.assertion.id"
	ontologyAssertionOriginProperty = "ontology.assertion.origin"

	// MaxCitationEnvelopeBytes bounds one encoded provider request/response.
	// It covers the native context and inference budgets plus base64url and
	// JSON framing overhead.
	MaxCitationEnvelopeBytes = 64 * 1024 * 1024
)

// CitationEnvelope is the transport value for a durably captured reasoning
// response. Custom JSON encoding preserves every opaque ID as unpadded
// base64url, including IDs containing arbitrary non-UTF-8 bytes.
type CitationEnvelope struct {
	Finalized                 bool
	DurablyRecorded           bool
	Verification              reasoning.VerificationStatus
	OutputVisibility          string
	WorkspaceSettingsID       shoal.ID
	WorkspaceSettingsRevision uint64
	OntologyInterpretation    *OntologyInterpretation
	ID                        shoal.ID
	SessionID                 shoal.ID
	RecordedAt                time.Time
	ContextPackID             shoal.ID
	ResultID                  shoal.ID
	PolicyID                  shoal.ID
	RequestID                 shoal.ID
	SnapshotID                shoal.ID
	SnapshotAsOf              time.Time
	AuthorizationFingerprint  shoal.ID
	AuthorizationExpiresAt    time.Time
	EmbeddingSpaces           interaction.EmbeddingSpaceSet
	EmbeddingSpaceID          shoal.ID
	EmbeddingSpaceIDs         []shoal.ID
	GeneratedAt               time.Time
	EffectiveVisibility       []string
	RetrievedSourceIDs        []shoal.ID
	CitedSourceIDs            []shoal.ID
	Sources                   []CitationSource
	Evidence                  []CitationEvidence
	Claims                    []CitationClaim
	Issues                    []CitationIssue

	recordedSession interaction.Session
}

type OntologyInterpretation struct {
	Status    string
	SchemaID  shoal.ID
	VersionID shoal.ID
}

type wireEmbeddingSpaceSet struct {
	Identities []string `json:"identities"`
	Digest     string   `json:"digest"`
}

type CitationSource struct {
	ID         shoal.ID
	AnchorIDs  []shoal.ID
	Visibility []string
}

type CitationEvidence struct {
	AnchorID     shoal.ID
	SnapshotID   shoal.ID
	SnapshotAsOf time.Time
	Status       reasoning.VerificationStatus
	Use          reasoning.EvidenceUse
	Origin       reasoning.EvidenceOrigin
	SourceIDs    []shoal.ID
	SectionID    shoal.ID
	SpanID       shoal.ID
	Visibility   []string
	FromAddition bool
	Citation     *document.Citation
	Quote        string
	SourceURI    string
	Path         *graph.Path
	Assertions   []CitationAssertion
}

type CitationAssertion struct {
	AssertionID shoal.ID
	EdgeID      shoal.ID
	Origin      ontology.AssertionOrigin
}

type CitationModel struct {
	Provider   string
	Model      string
	Version    string
	Parameters shoal.Metadata
	Seed       *int64
}

type CitationPrompt struct {
	TemplateID string
	Version    string
	Hash       string
}

type CitationClaim struct {
	ID                       shoal.ID
	Subject                  shoal.ID
	Predicate                shoal.ID
	Object                   ontology.Value
	Confidence               shoal.Score
	Status                   inference.ClaimStatus
	Model                    CitationModel
	Prompt                   CitationPrompt
	Metadata                 shoal.Metadata
	CitationAnchorIDs        []shoal.ID
	DerivedEvidenceAnchorIDs []shoal.ID
}

type CitationIssue struct {
	Kind              reasoning.IssueKind
	OutcomeType       reasoning.IssueOutcomeType
	OutcomeID         shoal.ID
	Input             string
	Reason            string
	EvidenceAnchorIDs []shoal.ID
	Claim             *CitationClaim
}

// NewCitationEnvelope adapts an immutable, captured reasoning response to the
// web API contract without changing citation or provenance cardinality.
func NewCitationEnvelope(response reasoning.Response) CitationEnvelope {
	snapshot := response.Snapshot()
	authorization := response.Authorization()
	envelope := CitationEnvelope{
		ID:                       response.ID(),
		SessionID:                response.SessionID(),
		RecordedAt:               response.RecordedAt(),
		ContextPackID:            response.ContextPackID(),
		ResultID:                 response.ResultID(),
		PolicyID:                 response.PolicyID(),
		RequestID:                response.RequestID(),
		SnapshotID:               snapshot.ID(),
		SnapshotAsOf:             snapshot.AsOf(),
		AuthorizationFingerprint: authorization.Fingerprint(),
		AuthorizationExpiresAt:   authorization.ExpiresAt(),
		EmbeddingSpaces:          response.EmbeddingSpaces(),
		GeneratedAt:              response.GeneratedAt(),
		EffectiveVisibility: append(
			[]string(nil), response.EffectiveOutputVisibility()...),
		RetrievedSourceIDs: response.RetrievedSourceIDs(),
		CitedSourceIDs:     response.CitedSourceIDs(),
		recordedSession:    response.RecordedSession(),
	}
	for _, source := range response.Sources() {
		envelope.Sources = append(envelope.Sources, CitationSource{
			ID: source.ID(), AnchorIDs: source.AnchorIDs(),
			Visibility: source.Visibility(),
		})
	}
	for _, evidence := range response.Evidence() {
		envelope.Evidence = append(
			envelope.Evidence, citationEvidenceValue(evidence))
	}
	for _, grounded := range response.Claims() {
		item := citationClaimValue(grounded.Value())
		for _, evidence := range grounded.Citations() {
			item.CitationAnchorIDs = append(
				item.CitationAnchorIDs, evidence.Anchor().ID())
		}
		for _, evidence := range grounded.DerivedEvidence() {
			item.DerivedEvidenceAnchorIDs = append(
				item.DerivedEvidenceAnchorIDs, evidence.Anchor().ID())
		}
		envelope.Claims = append(envelope.Claims, item)
	}
	for _, issue := range response.Issues() {
		item := CitationIssue{
			Kind: issue.Kind(), OutcomeType: issue.OutcomeType(),
			OutcomeID: issue.OutcomeID(), Input: issue.Input(),
			Reason: issue.Reason(),
		}
		for _, evidence := range issue.Evidence() {
			item.EvidenceAnchorIDs = append(
				item.EvidenceAnchorIDs, evidence.Anchor().ID())
		}
		if claim, ok := issue.Claim(); ok {
			value := citationClaimValue(claim)
			for _, evidence := range issue.Evidence() {
				switch evidence.Use() {
				case reasoning.EvidenceCited:
					value.CitationAnchorIDs = append(
						value.CitationAnchorIDs, evidence.Anchor().ID())
				case reasoning.EvidenceDerived:
					value.DerivedEvidenceAnchorIDs = append(
						value.DerivedEvidenceAnchorIDs, evidence.Anchor().ID())
				}
			}
			item.Claim = &value
		}
		envelope.Issues = append(envelope.Issues, item)
	}
	return envelope
}

func citationClaimValue(claim inference.Claim) CitationClaim {
	model := claim.ModelProvenance()
	seed, hasSeed := model.Seed()
	var seedValue *int64
	if hasSeed {
		seedValue = new(int64)
		*seedValue = seed
	}
	prompt := claim.PromptProvenance()
	return CitationClaim{
		ID: claim.ID(), Subject: claim.Subject(), Predicate: claim.Predicate(),
		Object: claim.Object(), Confidence: claim.Confidence(),
		Status: claim.Status(),
		Model: CitationModel{
			Provider: model.Provider(), Model: model.Model(),
			Version: model.Version(), Parameters: model.Parameters(),
			Seed: seedValue,
		},
		Prompt: CitationPrompt{
			TemplateID: prompt.TemplateID(), Version: prompt.Version(),
			Hash: prompt.Hash(),
		},
		Metadata: claim.Metadata(),
	}
}

func citationEvidenceValue(value reasoning.Evidence) CitationEvidence {
	snapshot := value.Snapshot()
	result := CitationEvidence{
		AnchorID: value.Anchor().ID(), SnapshotID: snapshot.ID(),
		SnapshotAsOf: snapshot.AsOf(), Status: value.Status(),
		Use: value.Use(), Origin: value.Origin(), SourceIDs: value.SourceIDs(),
		SectionID:  value.ResolvedSectionID(),
		SpanID:     value.ResolvedSpanID(),
		Visibility: value.Visibility(), FromAddition: value.FromAddition(),
	}
	if citation, quote, ok := value.Anchor().Document(); ok {
		result.Citation = &citation
		result.Quote = quote
	}
	if path, ok := value.Anchor().Path(); ok {
		result.Path = &path
	}
	for _, assertion := range value.Reference().Assertions {
		result.Assertions = append(result.Assertions, CitationAssertion{
			AssertionID: assertion.AssertionID,
			EdgeID:      assertion.EdgeID,
			Origin:      assertion.Origin,
		})
	}
	return result
}

type wireCitationEnvelope struct {
	Finalized                 bool                         `json:"finalized"`
	DurablyRecorded           bool                         `json:"durably_recorded"`
	Verification              reasoning.VerificationStatus `json:"verification"`
	OutputVisibility          string                       `json:"output_visibility"`
	WorkspaceSettingsID       string                       `json:"workspace_settings_id,omitempty"`
	WorkspaceSettingsRevision uint64                       `json:"workspace_settings_revision,omitempty"`
	OntologyInterpretation    *wireOntologyInterpretation  `json:"ontology_interpretation"`
	ID                        string                       `json:"id"`
	SessionID                 string                       `json:"session_id"`
	RecordedAt                time.Time                    `json:"recorded_at"`
	ContextPackID             string                       `json:"context_pack_id"`
	ResultID                  string                       `json:"result_id"`
	PolicyID                  string                       `json:"policy_id"`
	RequestID                 string                       `json:"request_id,omitempty"`
	SnapshotID                string                       `json:"snapshot_id"`
	SnapshotAsOf              time.Time                    `json:"snapshot_as_of"`
	AuthorizationFingerprint  string                       `json:"authorization_fingerprint"`
	AuthorizationExpiresAt    time.Time                    `json:"authorization_expires_at"`
	EmbeddingSpaces           *wireEmbeddingSpaceSet       `json:"embedding_spaces,omitempty"`
	GeneratedAt               time.Time                    `json:"generated_at"`
	EffectiveVisibility       []string                     `json:"effective_visibility"`
	RetrievedSourceIDs        []string                     `json:"retrieved_source_ids"`
	CitedSourceIDs            []string                     `json:"cited_source_ids"`
	Sources                   []wireCitationSource         `json:"sources"`
	Evidence                  []wireCitationEvidence       `json:"evidence"`
	Claims                    []wireCitationClaim          `json:"claims"`
	Issues                    []wireCitationIssue          `json:"issues"`
}

type wireOntologyInterpretation struct {
	Status    string `json:"status"`
	SchemaID  string `json:"schema_id,omitempty"`
	VersionID string `json:"version_id,omitempty"`
}

type wireCitationSource struct {
	ID         string   `json:"id"`
	AnchorIDs  []string `json:"anchor_ids"`
	Visibility []string `json:"visibility"`
}

type wireCitationEvidence struct {
	AnchorID     string                       `json:"anchor_id"`
	SnapshotID   string                       `json:"snapshot_id"`
	SnapshotAsOf time.Time                    `json:"snapshot_as_of"`
	Status       reasoning.VerificationStatus `json:"verification_status"`
	Use          reasoning.EvidenceUse        `json:"use"`
	Origin       reasoning.EvidenceOrigin     `json:"origin"`
	SourceIDs    []string                     `json:"source_ids"`
	SectionID    string                       `json:"resolved_section_id,omitempty"`
	SpanID       string                       `json:"resolved_span_id,omitempty"`
	Visibility   []string                     `json:"visibility"`
	FromAddition bool                         `json:"from_addition"`
	Citation     *wireCitation                `json:"citation,omitempty"`
	Quote        string                       `json:"quote,omitempty"`
	SourceURI    string                       `json:"source_uri,omitempty"`
	Path         *wirePath                    `json:"path,omitempty"`
	Assertions   []wireCitationAssertion      `json:"assertions,omitempty"`
}

type wireCitationAssertion struct {
	AssertionID string                   `json:"assertion_id"`
	EdgeID      string                   `json:"edge_id"`
	Origin      ontology.AssertionOrigin `json:"origin"`
}

type wireCitationModel struct {
	Provider   string       `json:"provider"`
	Model      string       `json:"model"`
	Version    string       `json:"version,omitempty"`
	Parameters wireMetadata `json:"parameters,omitempty"`
	Seed       *int64       `json:"seed,omitempty"`
}

type wireCitationPrompt struct {
	TemplateID string `json:"template_id"`
	Version    string `json:"version"`
	Hash       string `json:"hash"`
}

type wireCitationOntologyValue struct {
	Type      ontology.ValueType `json:"type"`
	Text      *string            `json:"text,omitempty"`
	Integer   *int64             `json:"integer,omitempty"`
	Number    *float64           `json:"number,omitempty"`
	Boolean   *bool              `json:"boolean,omitempty"`
	Timestamp *time.Time         `json:"timestamp,omitempty"`
	Reference *string            `json:"reference,omitempty"`
}

type wireCitationClaim struct {
	ID                       string                    `json:"id"`
	Subject                  string                    `json:"subject"`
	Predicate                string                    `json:"predicate"`
	Object                   wireCitationOntologyValue `json:"object"`
	Confidence               shoal.Score               `json:"confidence"`
	Status                   inference.ClaimStatus     `json:"status"`
	Model                    wireCitationModel         `json:"model"`
	Prompt                   wireCitationPrompt        `json:"prompt"`
	Metadata                 wireMetadata              `json:"metadata,omitempty"`
	CitationAnchorIDs        []string                  `json:"citation_anchor_ids"`
	DerivedEvidenceAnchorIDs []string                  `json:"derived_evidence_anchor_ids"`
}

type wireCitationIssue struct {
	Kind              reasoning.IssueKind        `json:"kind"`
	OutcomeType       reasoning.IssueOutcomeType `json:"outcome_type"`
	OutcomeID         string                     `json:"outcome_id"`
	Input             string                     `json:"input"`
	Reason            string                     `json:"reason"`
	EvidenceAnchorIDs []string                   `json:"evidence_anchor_ids"`
	Claim             *wireCitationClaim         `json:"claim,omitempty"`
}

func (e CitationEnvelope) MarshalJSON() ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	wire := wireCitationEnvelope{
		Finalized: e.Finalized, DurablyRecorded: e.DurablyRecorded,
		Verification: e.Verification, OutputVisibility: e.OutputVisibility,
		WorkspaceSettingsID:       encodeOptionalID(e.WorkspaceSettingsID),
		WorkspaceSettingsRevision: e.WorkspaceSettingsRevision,
		ID:                        encodeID(e.ID), SessionID: encodeID(e.SessionID), RecordedAt: e.RecordedAt,
		ContextPackID: encodeID(e.ContextPackID), ResultID: encodeID(e.ResultID),
		PolicyID: encodeID(e.PolicyID), RequestID: encodeOptionalID(e.RequestID),
		SnapshotID: encodeID(e.SnapshotID), SnapshotAsOf: e.SnapshotAsOf,
		AuthorizationFingerprint: encodeID(e.AuthorizationFingerprint),
		AuthorizationExpiresAt:   e.AuthorizationExpiresAt,
		GeneratedAt:              e.GeneratedAt,
		EffectiveVisibility:      append([]string(nil), e.EffectiveVisibility...),
		RetrievedSourceIDs:       encodeIDs(e.RetrievedSourceIDs),
		CitedSourceIDs:           encodeIDs(e.CitedSourceIDs),
	}
	if e.OntologyInterpretation != nil {
		wire.OntologyInterpretation = &wireOntologyInterpretation{
			Status:    e.OntologyInterpretation.Status,
			SchemaID:  encodeOptionalID(e.OntologyInterpretation.SchemaID),
			VersionID: encodeOptionalID(e.OntologyInterpretation.VersionID),
		}
	}
	if len(e.EmbeddingSpaces.Identities) > 0 {
		wire.EmbeddingSpaces = &wireEmbeddingSpaceSet{
			Identities: append(
				[]string(nil), e.EmbeddingSpaces.Identities...),
			Digest: e.EmbeddingSpaces.Digest,
		}
	}
	for _, source := range e.Sources {
		wire.Sources = append(wire.Sources, wireCitationSource{
			ID: encodeID(source.ID), AnchorIDs: encodeIDs(source.AnchorIDs),
			Visibility: append([]string(nil), source.Visibility...),
		})
	}
	for _, evidence := range e.Evidence {
		wire.Evidence = append(
			wire.Evidence, wireCitationEvidenceValue(evidence))
	}
	for _, claim := range e.Claims {
		wire.Claims = append(wire.Claims, wireCitationClaimValue(claim))
	}
	for _, issue := range e.Issues {
		item := wireCitationIssue{
			Kind: issue.Kind, OutcomeType: issue.OutcomeType,
			OutcomeID: encodeID(issue.OutcomeID),
			Input:     issue.Input, Reason: issue.Reason,
		}
		item.EvidenceAnchorIDs = encodeIDs(issue.EvidenceAnchorIDs)
		if issue.Claim != nil {
			claim := wireCitationClaimValue(*issue.Claim)
			item.Claim = &claim
		}
		wire.Issues = append(wire.Issues, item)
	}
	encoded, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}
	if len(encoded) > MaxCitationEnvelopeBytes {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"citation envelope exceeds the provider byte bound")
	}
	return encoded, nil
}

func wireCitationClaimValue(claim CitationClaim) wireCitationClaim {
	return wireCitationClaim{
		ID: encodeID(claim.ID), Subject: encodeID(claim.Subject),
		Predicate:  encodeID(claim.Predicate),
		Object:     wireCitationOntologyValueValue(claim.Object),
		Confidence: claim.Confidence, Status: claim.Status,
		Model: wireCitationModel{
			Provider: claim.Model.Provider, Model: claim.Model.Model,
			Version:    claim.Model.Version,
			Parameters: wireMetadataValue(claim.Model.Parameters),
			Seed:       claim.Model.Seed,
		},
		Prompt: wireCitationPrompt{
			TemplateID: claim.Prompt.TemplateID, Version: claim.Prompt.Version,
			Hash: claim.Prompt.Hash,
		},
		Metadata:                 wireMetadataValue(claim.Metadata),
		CitationAnchorIDs:        encodeIDs(claim.CitationAnchorIDs),
		DerivedEvidenceAnchorIDs: encodeIDs(claim.DerivedEvidenceAnchorIDs),
	}
}

func wireCitationEvidenceValue(
	value CitationEvidence,
) wireCitationEvidence {
	result := wireCitationEvidence{
		AnchorID: encodeID(value.AnchorID), SnapshotID: encodeID(value.SnapshotID),
		SnapshotAsOf: value.SnapshotAsOf, Status: value.Status,
		Use: value.Use, Origin: value.Origin, SourceIDs: encodeIDs(value.SourceIDs),
		SectionID:    encodeOptionalID(value.SectionID),
		SpanID:       encodeOptionalID(value.SpanID),
		Visibility:   append([]string(nil), value.Visibility...),
		FromAddition: value.FromAddition, Quote: value.Quote,
		SourceURI: value.SourceURI,
	}
	if value.Citation != nil {
		citation := wireCitationValue(*value.Citation)
		result.Citation = &citation
	}
	if value.Path != nil {
		path := wirePathValue(*value.Path)
		result.Path = &path
	}
	for _, assertion := range value.Assertions {
		result.Assertions = append(result.Assertions, wireCitationAssertion{
			AssertionID: encodeID(assertion.AssertionID),
			EdgeID:      encodeID(assertion.EdgeID),
			Origin:      assertion.Origin,
		})
	}
	return result
}

func (e *CitationEnvelope) UnmarshalJSON(data []byte) error {
	if len(data) > MaxCitationEnvelopeBytes {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"citation envelope exceeds the provider byte bound")
	}
	if err := rejectDuplicateJSONKeys(
		data, reflect.TypeOf(wireCitationEnvelope{})); err != nil {
		return err
	}
	var wire wireCitationEnvelope
	if err := strictUnmarshal(data, &wire); err != nil {
		return err
	}
	value, err := citationEnvelopeValue(wire)
	if err != nil {
		return err
	}
	*e = value
	return nil
}

func citationEnvelopeValue(
	wire wireCitationEnvelope,
) (CitationEnvelope, error) {
	var result CitationEnvelope
	var err error
	result.Finalized = wire.Finalized
	result.DurablyRecorded = wire.DurablyRecorded
	result.Verification = wire.Verification
	result.OutputVisibility = wire.OutputVisibility
	result.WorkspaceSettingsRevision = wire.WorkspaceSettingsRevision
	result.WorkspaceSettingsID, err = decodeOptionalID(wire.WorkspaceSettingsID)
	if err != nil {
		return CitationEnvelope{}, fmt.Errorf("workspace_settings_id: %w", err)
	}
	if wire.OntologyInterpretation != nil {
		interpretation := &OntologyInterpretation{Status: wire.OntologyInterpretation.Status}
		interpretation.SchemaID, err = decodeOptionalID(wire.OntologyInterpretation.SchemaID)
		if err != nil {
			return CitationEnvelope{}, fmt.Errorf("ontology_interpretation.schema_id: %w", err)
		}
		interpretation.VersionID, err = decodeOptionalID(wire.OntologyInterpretation.VersionID)
		if err != nil {
			return CitationEnvelope{}, fmt.Errorf("ontology_interpretation.version_id: %w", err)
		}
		result.OntologyInterpretation = interpretation
	}
	for name, source := range map[string]string{
		"id": wire.ID, "session_id": wire.SessionID,
		"context_pack_id": wire.ContextPackID, "result_id": wire.ResultID,
		"policy_id": wire.PolicyID, "snapshot_id": wire.SnapshotID,
		"authorization_fingerprint": wire.AuthorizationFingerprint,
	} {
		id, decodeErr := decodeCitationID(source)
		if decodeErr != nil {
			return CitationEnvelope{}, fmt.Errorf("%s: %w", name, decodeErr)
		}
		switch name {
		case "id":
			result.ID = id
		case "session_id":
			result.SessionID = id
		case "context_pack_id":
			result.ContextPackID = id
		case "result_id":
			result.ResultID = id
		case "policy_id":
			result.PolicyID = id
		case "snapshot_id":
			result.SnapshotID = id
		case "authorization_fingerprint":
			result.AuthorizationFingerprint = id
		}
	}
	result.RequestID, err = decodeOptionalCitationID(wire.RequestID)
	if err != nil {
		return CitationEnvelope{}, fmt.Errorf("request_id: %w", err)
	}
	if wire.EmbeddingSpaces != nil {
		result.EmbeddingSpaces, err = interaction.NewEmbeddingSpaceSet(
			wire.EmbeddingSpaces.Identities)
		if err != nil {
			return CitationEnvelope{}, fmt.Errorf("embedding_spaces: %w", err)
		}
		if result.EmbeddingSpaces.Digest != wire.EmbeddingSpaces.Digest {
			return CitationEnvelope{}, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"embedding_spaces digest is not canonical",
			)
		}
	}
	result.RecordedAt = wire.RecordedAt
	result.SnapshotAsOf = wire.SnapshotAsOf
	result.AuthorizationExpiresAt = wire.AuthorizationExpiresAt
	result.GeneratedAt = wire.GeneratedAt
	result.EffectiveVisibility = append(
		[]string(nil), wire.EffectiveVisibility...)
	result.RetrievedSourceIDs, err = decodeCitationIDs(wire.RetrievedSourceIDs)
	if err != nil {
		return CitationEnvelope{}, fmt.Errorf("retrieved_source_ids: %w", err)
	}
	result.CitedSourceIDs, err = decodeCitationIDs(wire.CitedSourceIDs)
	if err != nil {
		return CitationEnvelope{}, fmt.Errorf("cited_source_ids: %w", err)
	}
	for _, source := range wire.Sources {
		id, err := decodeCitationID(source.ID)
		if err != nil {
			return CitationEnvelope{}, fmt.Errorf("sources.id: %w", err)
		}
		anchorIDs, err := decodeCitationIDs(source.AnchorIDs)
		if err != nil {
			return CitationEnvelope{}, fmt.Errorf("sources.anchor_ids: %w", err)
		}
		result.Sources = append(result.Sources, CitationSource{
			ID: id, AnchorIDs: anchorIDs,
			Visibility: append([]string(nil), source.Visibility...),
		})
	}
	for _, evidence := range wire.Evidence {
		value, err := citationEvidenceFromWire(evidence)
		if err != nil {
			return CitationEnvelope{}, fmt.Errorf("evidence: %w", err)
		}
		result.Evidence = append(result.Evidence, value)
	}
	for _, claim := range wire.Claims {
		value, err := citationClaimFromWire(claim)
		if err != nil {
			return CitationEnvelope{}, fmt.Errorf("claims: %w", err)
		}
		result.Claims = append(result.Claims, value)
	}
	for _, issue := range wire.Issues {
		outcomeID, err := decodeCitationID(issue.OutcomeID)
		if err != nil {
			return CitationEnvelope{}, fmt.Errorf("issues.outcome_id: %w", err)
		}
		item := CitationIssue{
			Kind: issue.Kind, OutcomeType: issue.OutcomeType,
			OutcomeID: outcomeID, Input: issue.Input, Reason: issue.Reason,
		}
		item.EvidenceAnchorIDs, err = decodeCitationIDs(
			issue.EvidenceAnchorIDs)
		if err != nil {
			return CitationEnvelope{}, fmt.Errorf(
				"issues.evidence_anchor_ids: %w", err)
		}
		if issue.Claim != nil {
			claim, err := citationClaimFromWire(*issue.Claim)
			if err != nil {
				return CitationEnvelope{}, fmt.Errorf(
					"issues.claim: %w", err)
			}
			item.Claim = &claim
		}
		result.Issues = append(result.Issues, item)
	}
	if err := result.Validate(); err != nil {
		return CitationEnvelope{}, err
	}
	return result, nil
}

func citationClaimFromWire(
	wire wireCitationClaim,
) (CitationClaim, error) {
	id, err := decodeCitationID(wire.ID)
	if err != nil {
		return CitationClaim{}, fmt.Errorf("id: %w", err)
	}
	subject, err := decodeCitationID(wire.Subject)
	if err != nil {
		return CitationClaim{}, fmt.Errorf("subject: %w", err)
	}
	predicate, err := decodeCitationID(wire.Predicate)
	if err != nil {
		return CitationClaim{}, fmt.Errorf("predicate: %w", err)
	}
	object, err := citationOntologyValue(wire.Object)
	if err != nil {
		return CitationClaim{}, fmt.Errorf("object: %w", err)
	}
	parameters, err := citationMetadataValue(wire.Model.Parameters)
	if err != nil {
		return CitationClaim{}, fmt.Errorf("model.parameters: %w", err)
	}
	metadata, err := citationMetadataValue(wire.Metadata)
	if err != nil {
		return CitationClaim{}, fmt.Errorf("metadata: %w", err)
	}
	result := CitationClaim{
		ID: id, Subject: subject, Predicate: predicate, Object: object,
		Confidence: wire.Confidence, Status: wire.Status,
		Model: CitationModel{
			Provider: wire.Model.Provider, Model: wire.Model.Model,
			Version: wire.Model.Version, Parameters: parameters, Seed: wire.Model.Seed,
		},
		Prompt: CitationPrompt{
			TemplateID: wire.Prompt.TemplateID, Version: wire.Prompt.Version,
			Hash: wire.Prompt.Hash,
		},
		Metadata: metadata,
	}
	result.CitationAnchorIDs, err = decodeCitationIDs(
		wire.CitationAnchorIDs)
	if err != nil {
		return CitationClaim{}, fmt.Errorf("citation_anchor_ids: %w", err)
	}
	result.DerivedEvidenceAnchorIDs, err = decodeCitationIDs(
		wire.DerivedEvidenceAnchorIDs)
	if err != nil {
		return CitationClaim{}, fmt.Errorf(
			"derived_evidence_anchor_ids: %w", err)
	}
	return result, nil
}

func citationEvidenceFromWire(
	wire wireCitationEvidence,
) (CitationEvidence, error) {
	anchorID, err := decodeCitationID(wire.AnchorID)
	if err != nil {
		return CitationEvidence{}, fmt.Errorf("anchor_id: %w", err)
	}
	snapshotID, err := decodeCitationID(wire.SnapshotID)
	if err != nil {
		return CitationEvidence{}, fmt.Errorf("snapshot_id: %w", err)
	}
	sourceIDs, err := decodeCitationIDs(wire.SourceIDs)
	if err != nil {
		return CitationEvidence{}, fmt.Errorf("source_ids: %w", err)
	}
	sectionID, err := decodeOptionalCitationID(wire.SectionID)
	if err != nil {
		return CitationEvidence{}, fmt.Errorf("resolved_section_id: %w", err)
	}
	spanID, err := decodeOptionalCitationID(wire.SpanID)
	if err != nil {
		return CitationEvidence{}, fmt.Errorf("resolved_span_id: %w", err)
	}
	result := CitationEvidence{
		AnchorID: anchorID, SnapshotID: snapshotID,
		SnapshotAsOf: wire.SnapshotAsOf, Status: wire.Status,
		Use: wire.Use, Origin: wire.Origin, SourceIDs: sourceIDs,
		SectionID: sectionID, SpanID: spanID,
		Visibility:   append([]string(nil), wire.Visibility...),
		FromAddition: wire.FromAddition, Quote: wire.Quote,
		SourceURI: wire.SourceURI,
	}
	if wire.Citation != nil {
		citation, err := citationValueStrict(*wire.Citation)
		if err != nil {
			return CitationEvidence{}, fmt.Errorf("citation: %w", err)
		}
		result.Citation = &citation
	}
	if wire.Path != nil {
		path, err := pathValueStrict(*wire.Path)
		if err != nil {
			return CitationEvidence{}, fmt.Errorf("path: %w", err)
		}
		result.Path = &path
	}
	for _, assertion := range wire.Assertions {
		assertionID, err := decodeCitationID(assertion.AssertionID)
		if err != nil {
			return CitationEvidence{}, fmt.Errorf(
				"assertions.assertion_id: %w", err)
		}
		edgeID, err := decodeCitationID(assertion.EdgeID)
		if err != nil {
			return CitationEvidence{}, fmt.Errorf(
				"assertions.edge_id: %w", err)
		}
		result.Assertions = append(result.Assertions, CitationAssertion{
			AssertionID: assertionID,
			EdgeID:      edgeID,
			Origin:      assertion.Origin,
		})
	}
	return result, nil
}

func citationValueStrict(value wireCitation) (document.Citation, error) {
	documentID, err := decodeCitationID(value.DocumentID)
	if err != nil {
		return document.Citation{}, fmt.Errorf("document_id: %w", err)
	}
	revisionID, err := decodeCitationID(value.RevisionID)
	if err != nil {
		return document.Citation{}, fmt.Errorf("revision_id: %w", err)
	}
	sectionID, err := decodeOptionalCitationID(value.SectionID)
	if err != nil {
		return document.Citation{}, fmt.Errorf("section_id: %w", err)
	}
	spanID, err := decodeOptionalCitationID(value.SpanID)
	if err != nil {
		return document.Citation{}, fmt.Errorf("span_id: %w", err)
	}
	return document.Citation{
		DocumentID: documentID, RevisionID: revisionID,
		SectionID: sectionID, SpanID: spanID,
		Range: document.SourceRange{
			Start: document.SourcePosition{
				Offset: value.Range.Start.Offset, Page: value.Range.Start.Page,
			},
			End: document.SourcePosition{
				Offset: value.Range.End.Offset, Page: value.Range.End.Page,
			},
		},
	}, nil
}

func pathValueStrict(value wirePath) (graph.Path, error) {
	path := graph.Path{
		Nodes: make([]graph.Node, 0, len(value.Nodes)),
		Edges: make([]graph.Edge, 0, len(value.Edges)),
	}
	for _, item := range value.Nodes {
		id, err := decodeCitationID(item.ID)
		if err != nil {
			return graph.Path{}, fmt.Errorf("nodes.id: %w", err)
		}
		properties, err := citationMetadataValue(item.Properties)
		if err != nil {
			return graph.Path{}, fmt.Errorf("nodes.properties: %w", err)
		}
		path.Nodes = append(path.Nodes, graph.Node{
			ID: id, Kind: item.Kind,
			Labels:     append([]string(nil), item.Labels...),
			Properties: properties,
		})
	}
	for _, item := range value.Edges {
		id, err := decodeCitationID(item.ID)
		if err != nil {
			return graph.Path{}, fmt.Errorf("edges.id: %w", err)
		}
		from, err := decodeCitationID(item.From)
		if err != nil {
			return graph.Path{}, fmt.Errorf("edges.from: %w", err)
		}
		to, err := decodeCitationID(item.To)
		if err != nil {
			return graph.Path{}, fmt.Errorf("edges.to: %w", err)
		}
		properties, err := citationMetadataValue(item.Properties)
		if err != nil {
			return graph.Path{}, fmt.Errorf("edges.properties: %w", err)
		}
		path.Edges = append(path.Edges, graph.Edge{
			ID: id, From: from, To: to, Type: item.Type,
			Weight: item.Weight, Properties: properties,
		})
	}
	return path, nil
}

func wireCitationOntologyValueValue(
	value ontology.Value,
) wireCitationOntologyValue {
	wire := wireCitationOntologyValue{Type: value.Type()}
	switch value.Type() {
	case ontology.ValueString:
		got, _ := value.StringValue()
		wire.Text = &got
	case ontology.ValueInteger:
		got, _ := value.IntegerValue()
		wire.Integer = &got
	case ontology.ValueNumber:
		got, _ := value.NumberValue()
		wire.Number = &got
	case ontology.ValueBoolean:
		got, _ := value.BooleanValue()
		wire.Boolean = &got
	case ontology.ValueTimestamp:
		got, _ := value.TimestampValue()
		wire.Timestamp = &got
	case ontology.ValueReference:
		got, _ := value.ReferenceValue()
		encoded := encodeID(got)
		wire.Reference = &encoded
	}
	return wire
}

func citationOntologyValue(
	value wireCitationOntologyValue,
) (ontology.Value, error) {
	active := 0
	for _, present := range []bool{
		value.Text != nil,
		value.Integer != nil,
		value.Number != nil,
		value.Boolean != nil,
		value.Timestamp != nil,
		value.Reference != nil,
	} {
		if present {
			active++
		}
	}
	if active != 1 {
		return ontology.Value{}, fmt.Errorf(
			"ontology value requires exactly one active field")
	}
	switch value.Type {
	case ontology.ValueString:
		if value.Text == nil {
			break
		}
		return ontology.NewStringValue(*value.Text)
	case ontology.ValueInteger:
		if value.Integer == nil {
			break
		}
		return ontology.NewIntegerValue(*value.Integer), nil
	case ontology.ValueNumber:
		if value.Number == nil {
			break
		}
		return ontology.NewNumberValue(*value.Number)
	case ontology.ValueBoolean:
		if value.Boolean == nil {
			break
		}
		return ontology.NewBooleanValue(*value.Boolean), nil
	case ontology.ValueTimestamp:
		if value.Timestamp == nil {
			break
		}
		return ontology.NewTimestampValue(*value.Timestamp)
	case ontology.ValueReference:
		if value.Reference == nil {
			break
		}
		reference, err := decodeCitationID(*value.Reference)
		if err != nil {
			return ontology.Value{}, fmt.Errorf("reference: %w", err)
		}
		return ontology.NewReferenceValue(reference)
	default:
		return ontology.Value{}, fmt.Errorf(
			"unknown value type %q", value.Type)
	}
	return ontology.Value{}, fmt.Errorf(
		"ontology value active field does not match type %q", value.Type)
}

func citationMetadataValue(value wireMetadata) (shoal.Metadata, error) {
	if len(value) == 0 {
		return nil, nil
	}
	metadata := make(shoal.Metadata, len(value))
	for _, item := range value {
		key, err := decodeCanonicalBase64URL(item.Key)
		if err != nil {
			return nil, fmt.Errorf("key must be canonical unpadded base64url")
		}
		decodedValue, err := decodeCanonicalBase64URL(item.Value)
		if err != nil {
			return nil, fmt.Errorf("value must be canonical unpadded base64url")
		}
		if _, duplicate := metadata[string(key)]; duplicate {
			return nil, fmt.Errorf("metadata contains duplicate keys")
		}
		metadata[string(key)] = string(decodedValue)
	}
	return metadata, nil
}

func decodeCanonicalBase64URL(value string) ([]byte, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil ||
		base64.RawURLEncoding.EncodeToString(decoded) != value {
		return nil, fmt.Errorf("invalid canonical unpadded base64url")
	}
	return decoded, nil
}

// Validate checks the decoded wire shape. It deliberately does not claim to
// re-verify source evidence; server-side reasoning.Builder owns that boundary.
func (e CitationEnvelope) Validate() error {
	for name, id := range map[string]shoal.ID{
		"citation response ID": e.ID, "citation session ID": e.SessionID,
		"citation context pack ID":           e.ContextPackID,
		"citation result ID":                 e.ResultID,
		"citation policy ID":                 e.PolicyID,
		"citation snapshot ID":               e.SnapshotID,
		"citation authorization fingerprint": e.AuthorizationFingerprint,
	} {
		if err := shoal.ValidateRequiredID(name, id); err != nil {
			return err
		}
	}
	if err := shoal.ValidateOptionalID(
		"citation request ID", e.RequestID); err != nil {
		return err
	}
	if err := e.EmbeddingSpaces.Validate(); err != nil {
		return err
	}
	for name, value := range map[string]time.Time{
		"citation recorded time":        e.RecordedAt,
		"citation snapshot time":        e.SnapshotAsOf,
		"citation authorization expiry": e.AuthorizationExpiresAt,
		"citation generation time":      e.GeneratedAt,
	} {
		if value.IsZero() {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, name+" is required")
		}
	}
	if e.GeneratedAt.Before(e.SnapshotAsOf) ||
		e.RecordedAt.Before(e.GeneratedAt) ||
		!e.GeneratedAt.Before(e.AuthorizationExpiresAt) ||
		!e.RecordedAt.Before(e.AuthorizationExpiresAt) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"citation response chronology is invalid")
	}
	if _, err := interactionVisibility(e.EffectiveVisibility); err != nil {
		return err
	}
	if err := validateWireIDs(
		"retrieved source ID", e.RetrievedSourceIDs); err != nil {
		return err
	}
	if err := validateWireIDs(
		"cited source ID", e.CitedSourceIDs); err != nil {
		return err
	}
	if len(e.Sources) == 0 || len(e.Evidence) == 0 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"citation response requires verified source evidence")
	}
	if len(e.Claims) == 0 && len(e.Issues) == 0 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"citation response requires at least one outcome")
	}
	claimOutcomes := len(e.Claims)
	nativeIssues := 0
	for _, issue := range e.Issues {
		switch issue.OutcomeType {
		case reasoning.IssueOutcomeClaim:
			claimOutcomes++
		case reasoning.IssueOutcomeInferenceIssue:
			nativeIssues++
		}
	}
	if claimOutcomes > inference.MaxClaims {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"citation response has too many claims")
	}
	if nativeIssues > inference.MaxIssues {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"citation response has too many issues")
	}
	if len(e.Evidence) > inference.MaxEvidenceAnchors {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"citation response has too many evidence anchors")
	}
	seenSources := make(map[shoal.ID]CitationSource, len(e.Sources))
	sourceAnchors := make(
		map[shoal.ID]map[shoal.ID]struct{}, len(e.Sources))
	sourceIDs := make([]shoal.ID, 0, len(e.Sources))
	visibilitySets := make([][]string, 0, len(e.Sources)+len(e.Evidence))
	for _, source := range e.Sources {
		if err := shoal.ValidateRequiredID(
			"citation source ID", source.ID); err != nil {
			return err
		}
		if _, duplicate := seenSources[source.ID]; duplicate {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"citation response contains duplicate source IDs")
		}
		seenSources[source.ID] = source
		anchors := make(map[shoal.ID]struct{}, len(source.AnchorIDs))
		for _, anchorID := range source.AnchorIDs {
			anchors[anchorID] = struct{}{}
		}
		sourceAnchors[source.ID] = anchors
		sourceIDs = append(sourceIDs, source.ID)
		if err := validateWireIDs(
			"citation source anchor ID", source.AnchorIDs); err != nil {
			return err
		}
		if len(source.AnchorIDs) == 0 {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"citation source requires at least one evidence anchor")
		}
		if _, err := interactionVisibility(source.Visibility); err != nil {
			return err
		}
		visibilitySets = append(visibilitySets, source.Visibility)
	}
	if !equalWireIDs(sourceIDs, e.RetrievedSourceIDs) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"citation sources do not match retrieved source identities")
	}
	evidenceByID := make(map[shoal.ID]CitationEvidence, len(e.Evidence))
	evidenceSources := make(
		map[shoal.ID]map[shoal.ID]struct{}, len(e.Evidence))
	for _, evidence := range e.Evidence {
		if err := validateCitationEvidence(evidence); err != nil {
			return err
		}
		if err := validateCitationEvidenceVisibility(
			evidence, seenSources); err != nil {
			return err
		}
		if evidence.SnapshotID != e.SnapshotID ||
			!evidence.SnapshotAsOf.Equal(e.SnapshotAsOf) {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"citation evidence snapshot does not match response")
		}
		if _, duplicate := evidenceByID[evidence.AnchorID]; duplicate {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"citation response contains duplicate evidence anchors")
		}
		sources := make(map[shoal.ID]struct{}, len(evidence.SourceIDs))
		for _, sourceID := range evidence.SourceIDs {
			source, ok := seenSources[sourceID]
			if !ok {
				return shoal.NewError(
					shoal.ErrorInvalidArgument,
					"citation evidence references an unknown source")
			}
			if _, ok := sourceAnchors[source.ID][evidence.AnchorID]; !ok {
				return shoal.NewError(
					shoal.ErrorInvalidArgument,
					"citation evidence is missing from its source reference")
			}
			sources[sourceID] = struct{}{}
		}
		evidenceByID[evidence.AnchorID] = evidence
		evidenceSources[evidence.AnchorID] = sources
		visibilitySets = append(visibilitySets, evidence.Visibility)
	}
	for _, source := range e.Sources {
		for _, anchorID := range source.AnchorIDs {
			if _, ok := evidenceByID[anchorID]; !ok {
				return shoal.NewError(
					shoal.ErrorInvalidArgument,
					"citation source references an inconsistent evidence anchor")
			}
			if _, ok := evidenceSources[anchorID][source.ID]; !ok {
				return shoal.NewError(
					shoal.ErrorInvalidArgument,
					"citation source references an inconsistent evidence anchor")
			}
		}
	}
	requiredVisibility, err := interaction.Conjoin(visibilitySets...)
	if err != nil {
		return err
	}
	if !visibilityCovers(e.EffectiveVisibility, requiredVisibility) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"effective visibility omits evidence visibility")
	}
	var cited []shoal.ID
	seenOutcomes := make(map[shoal.ID]struct{}, len(e.Claims)+len(e.Issues))
	for _, claim := range e.Claims {
		if _, duplicate := seenOutcomes[claim.ID]; duplicate {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"citation response contains duplicate claim IDs")
		}
		seenOutcomes[claim.ID] = struct{}{}
		claimCited, err := validateCitationClaim(claim, evidenceByID)
		if err != nil {
			return err
		}
		cited = append(cited, claimCited...)
	}
	for _, issue := range e.Issues {
		if issue.Kind != reasoning.IssueUnsupported &&
			issue.Kind != reasoning.IssueUnverified {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"citation issue kind is invalid")
		}
		if err := shoal.ValidateRequiredID(
			"citation issue outcome ID", issue.OutcomeID); err != nil {
			return err
		}
		if _, duplicate := seenOutcomes[issue.OutcomeID]; duplicate {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"citation response contains duplicate outcome IDs")
		}
		seenOutcomes[issue.OutcomeID] = struct{}{}
		if err := validateWireIDs(
			"citation issue evidence anchor ID",
			issue.EvidenceAnchorIDs,
		); err != nil {
			return err
		}
		for _, anchorID := range issue.EvidenceAnchorIDs {
			if _, ok := evidenceByID[anchorID]; !ok {
				return shoal.NewError(
					shoal.ErrorInvalidArgument,
					"citation issue references an unknown evidence anchor")
			}
		}
		switch issue.OutcomeType {
		case reasoning.IssueOutcomeInferenceIssue:
			if issue.Claim != nil {
				return shoal.NewError(
					shoal.ErrorInvalidArgument,
					"inference issue cannot contain a claim payload")
			}
			inferenceKind := inference.IssueUnresolved
			if issue.Kind == reasoning.IssueUnsupported {
				inferenceKind = inference.IssueUnsupported
			}
			canonical, err := inference.NewIssue(
				inferenceKind,
				issue.Input,
				issue.Reason,
				issue.EvidenceAnchorIDs,
			)
			if err != nil {
				return err
			}
			if canonical.ID() != issue.OutcomeID {
				return shoal.NewError(
					shoal.ErrorInvalidArgument,
					"citation issue outcome ID is not canonical")
			}
		case reasoning.IssueOutcomeClaim:
			if issue.Kind != reasoning.IssueUnverified ||
				issue.Claim == nil {
				return shoal.NewError(
					shoal.ErrorInvalidArgument,
					"claim outcome requires an unverified claim payload")
			}
			if issue.Claim.ID != issue.OutcomeID ||
				issue.Input != string(issue.OutcomeID) ||
				issue.Reason != reasoning.UnverifiedClaimReason {
				return shoal.NewError(
					shoal.ErrorInvalidArgument,
					"unverified claim issue does not match its outcome")
			}
			if len(issue.Claim.CitationAnchorIDs) != 0 ||
				!equalWireIDs(
					issue.Claim.DerivedEvidenceAnchorIDs,
					issue.EvidenceAnchorIDs,
				) {
				return shoal.NewError(
					shoal.ErrorInvalidArgument,
					"unverified claim issue evidence does not match its claim")
			}
			if _, err := validateCitationClaimPayload(
				*issue.Claim, evidenceByID, false); err != nil {
				return err
			}
		default:
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"citation issue outcome type is invalid")
		}
	}
	if !equalWireIDs(cited, e.CitedSourceIDs) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"citation claims do not match cited source identities")
	}
	if err := validateCitationAggregateBudgets(e); err != nil {
		return err
	}
	expectedID, err := reasoning.CanonicalResponseID(
		e.SessionID, e.RecordedAt, citationResponseIdentity(e))
	if err != nil {
		return err
	}
	if expectedID != e.ID {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"citation envelope response ID is not canonical")
	}
	return nil
}

func validateCitationClaim(
	claim CitationClaim,
	evidenceByID map[shoal.ID]CitationEvidence,
) ([]shoal.ID, error) {
	return validateCitationClaimPayload(claim, evidenceByID, true)
}

func validateCitationClaimPayload(
	claim CitationClaim,
	evidenceByID map[shoal.ID]CitationEvidence,
	requireCitation bool,
) ([]shoal.ID, error) {
	for name, id := range map[string]shoal.ID{
		"citation claim ID": claim.ID, "citation claim subject": claim.Subject,
		"citation claim predicate": claim.Predicate,
	} {
		if err := shoal.ValidateRequiredID(name, id); err != nil {
			return nil, err
		}
	}
	if err := claim.Object.Validate(); err != nil {
		return nil, err
	}
	if err := shoal.ValidateFiniteScore(
		"citation claim confidence", claim.Confidence); err != nil {
		return nil, err
	}
	if claim.Status != inference.ClaimObserved &&
		claim.Status != inference.ClaimInferred {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "citation claim status is invalid")
	}
	model, err := inference.NewModelProvenance(
		claim.Model.Provider, claim.Model.Model, claim.Model.Version,
		claim.Model.Parameters, claim.Model.Seed,
	)
	if err != nil {
		return nil, err
	}
	prompt, err := inference.NewPromptProvenance(
		claim.Prompt.TemplateID, claim.Prompt.Version, claim.Prompt.Hash,
	)
	if err != nil {
		return nil, err
	}
	if err := shoal.ValidateMetadata(
		"citation claim metadata", claim.Metadata); err != nil {
		return nil, err
	}
	if err := validateWireIDs(
		"citation claim citation anchor ID",
		claim.CitationAnchorIDs,
	); err != nil {
		return nil, err
	}
	if err := validateWireIDs(
		"citation claim derived evidence anchor ID",
		claim.DerivedEvidenceAnchorIDs,
	); err != nil {
		return nil, err
	}
	if requireCitation && len(claim.CitationAnchorIDs) == 0 {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"successful citation claim requires a verified citation")
	}
	evidenceIDs := append(
		append([]shoal.ID(nil), claim.CitationAnchorIDs...),
		claim.DerivedEvidenceAnchorIDs...,
	)
	var citedSourceIDs []shoal.ID
	for _, anchorID := range claim.CitationAnchorIDs {
		evidence, ok := evidenceByID[anchorID]
		if !ok {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"citation claim references an unknown evidence anchor")
		}
		if evidence.Use != reasoning.EvidenceCited ||
			evidence.Status != reasoning.VerificationVerified {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"successful citation claim contains unverified citation")
		}
		citedSourceIDs = append(citedSourceIDs, evidence.SourceIDs...)
	}
	for _, anchorID := range claim.DerivedEvidenceAnchorIDs {
		evidence, ok := evidenceByID[anchorID]
		if !ok {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"citation claim references an unknown evidence anchor")
		}
		if evidence.Use != reasoning.EvidenceDerived ||
			evidence.Status != reasoning.VerificationVerified {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"successful claim contains unverified derived evidence")
		}
	}
	canonicalClaim, err := inference.NewClaim(
		claim.Subject, claim.Predicate, claim.Object, claim.Confidence,
		evidenceIDs, claim.Status, model, prompt, claim.Metadata,
	)
	if err != nil {
		return nil, err
	}
	if canonicalClaim.ID() != claim.ID {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"citation claim ID is not canonical")
	}
	return citedSourceIDs, nil
}

func citationInferenceClaim(claim CitationClaim) (inference.Claim, error) {
	model, err := inference.NewModelProvenance(
		claim.Model.Provider, claim.Model.Model, claim.Model.Version,
		claim.Model.Parameters, claim.Model.Seed,
	)
	if err != nil {
		return inference.Claim{}, err
	}
	prompt, err := inference.NewPromptProvenance(
		claim.Prompt.TemplateID, claim.Prompt.Version, claim.Prompt.Hash,
	)
	if err != nil {
		return inference.Claim{}, err
	}
	evidenceIDs := append(
		append([]shoal.ID(nil), claim.CitationAnchorIDs...),
		claim.DerivedEvidenceAnchorIDs...,
	)
	return inference.NewClaim(
		claim.Subject, claim.Predicate, claim.Object, claim.Confidence,
		evidenceIDs, claim.Status, model, prompt, claim.Metadata,
	)
}

func validateCitationAggregateBudgets(e CitationEnvelope) error {
	var original []inference.EvidenceAnchor
	var additions []inference.EvidenceAnchor
	for _, evidence := range e.Evidence {
		anchor, err := citationInferenceAnchor(evidence)
		if err != nil {
			return err
		}
		if evidence.FromAddition {
			additions = append(additions, anchor)
		} else {
			original = append(original, anchor)
		}
	}
	snapshot, err := inference.NewSnapshotPin(e.SnapshotID, e.SnapshotAsOf)
	if err != nil {
		return err
	}
	authorization, err := inference.NewAuthPin(
		e.AuthorizationFingerprint, e.AuthorizationExpiresAt)
	if err != nil {
		return err
	}
	pack, err := inference.NewContextPack(
		"citation-envelope-budget",
		original,
		nil,
		snapshot,
		authorization,
		nil,
	)
	if err != nil {
		return err
	}
	claims := make([]inference.Claim, 0, len(e.Claims)+len(e.Issues))
	for _, claim := range e.Claims {
		canonical, err := citationInferenceClaim(claim)
		if err != nil {
			return err
		}
		claims = append(claims, canonical)
	}
	issues := make([]inference.Issue, 0, len(e.Issues))
	for _, issue := range e.Issues {
		switch issue.OutcomeType {
		case reasoning.IssueOutcomeClaim:
			canonical, err := citationInferenceClaim(*issue.Claim)
			if err != nil {
				return err
			}
			claims = append(claims, canonical)
		case reasoning.IssueOutcomeInferenceIssue:
			kind := inference.IssueUnresolved
			if issue.Kind == reasoning.IssueUnsupported {
				kind = inference.IssueUnsupported
			}
			canonical, err := inference.NewIssue(
				kind, issue.Input, issue.Reason, issue.EvidenceAnchorIDs)
			if err != nil {
				return err
			}
			issues = append(issues, canonical)
		}
	}
	_, err = inference.NewExtendedInferenceResult(
		pack, additions, claims, issues, e.GeneratedAt, nil)
	return err
}

func citationResponseIdentity(e CitationEnvelope) reasoning.ResponseIdentity {
	identity := reasoning.ResponseIdentity{
		ContextPackID: e.ContextPackID, ResultID: e.ResultID,
		PolicyID: e.PolicyID, RequestID: e.RequestID,
		SnapshotID: e.SnapshotID, SnapshotAsOf: e.SnapshotAsOf,
		AuthorizationFingerprint: e.AuthorizationFingerprint,
		AuthorizationExpiresAt:   e.AuthorizationExpiresAt,
		EmbeddingSpaces:          e.EmbeddingSpaces,
		GeneratedAt:              e.GeneratedAt,
		EffectiveVisibility: append(
			[]string(nil), e.EffectiveVisibility...),
		RetrievedSourceIDs: append(
			[]shoal.ID(nil), e.RetrievedSourceIDs...),
		CitedSourceIDs: append([]shoal.ID(nil), e.CitedSourceIDs...),
	}
	for _, source := range e.Sources {
		identity.Sources = append(
			identity.Sources, reasoning.ResponseIdentitySource{
				ID: source.ID,
				AnchorIDs: append(
					[]shoal.ID(nil), source.AnchorIDs...),
				Visibility: append(
					[]string(nil), source.Visibility...),
			})
	}
	evidenceByID := make(
		map[shoal.ID]interaction.EvidenceReference, len(e.Evidence))
	for _, evidence := range e.Evidence {
		reference := citationInteractionReference(evidence)
		evidenceByID[evidence.AnchorID] = reference
		identity.RetrievedEvidence = append(
			identity.RetrievedEvidence, reference)
		identity.Evidence = append(
			identity.Evidence, reasoning.ResponseIdentityEvidence{
				AnchorID: evidence.AnchorID, Status: evidence.Status,
				Use: evidence.Use, Origin: evidence.Origin,
				FromAddition: evidence.FromAddition,
				SourceIDs: append(
					[]shoal.ID(nil), evidence.SourceIDs...),
				SectionID: evidence.SectionID,
				SpanID:    evidence.SpanID,
				Visibility: append(
					[]string(nil), evidence.Visibility...),
			})
	}
	cited := make(map[shoal.ID]struct{})
	for _, claim := range e.Claims {
		identity.ClaimIDs = append(identity.ClaimIDs, claim.ID)
		for _, anchorID := range claim.CitationAnchorIDs {
			if _, duplicate := cited[anchorID]; duplicate {
				continue
			}
			cited[anchorID] = struct{}{}
			identity.CitedEvidence = append(
				identity.CitedEvidence, evidenceByID[anchorID])
		}
	}
	for _, issue := range e.Issues {
		identity.Issues = append(
			identity.Issues, reasoning.ResponseIdentityIssue{
				Kind: issue.Kind, OutcomeType: issue.OutcomeType,
				OutcomeID: issue.OutcomeID,
				Input:     issue.Input, Reason: issue.Reason,
			})
	}
	return identity
}

func citationInteractionReference(
	evidence CitationEvidence,
) interaction.EvidenceReference {
	reference := interaction.EvidenceReference{
		AnchorID: evidence.AnchorID,
		NodeIDs:  append([]shoal.ID(nil), evidence.SourceIDs...),
	}
	if evidence.Citation != nil {
		reference.Kind = interaction.EvidenceDocument
		reference.Citation = *evidence.Citation
	}
	if evidence.Path != nil {
		reference.Kind = interaction.EvidenceGraph
		reference.NodeIDs = nil
		for _, node := range evidence.Path.Nodes {
			reference.NodeIDs = append(reference.NodeIDs, node.ID)
		}
		for _, edge := range evidence.Path.Edges {
			reference.EdgeIDs = append(reference.EdgeIDs, edge.ID)
		}
	}
	for _, assertion := range evidence.Assertions {
		reference.Assertions = append(
			reference.Assertions, interaction.AssertionReference{
				AssertionID: assertion.AssertionID,
				EdgeID:      assertion.EdgeID, Origin: assertion.Origin,
			})
	}
	canonical, _ := reference.Canonical()
	return canonical
}

func citationInferenceAnchor(
	evidence CitationEvidence,
) (inference.EvidenceAnchor, error) {
	switch evidence.Use {
	case reasoning.EvidenceCited:
		return inference.NewDocumentAnchor(
			*evidence.Citation, evidence.Quote)
	case reasoning.EvidenceDerived:
		assertions := make(
			[]interaction.AssertionReference, len(evidence.Assertions))
		for index, assertion := range evidence.Assertions {
			assertions[index] = interaction.AssertionReference{
				AssertionID: assertion.AssertionID,
				EdgeID:      assertion.EdgeID, Origin: assertion.Origin,
			}
		}
		return inference.NewGraphAnchorWithAssertions(
			*evidence.Path, assertions)
	default:
		return inference.EvidenceAnchor{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "citation evidence use is invalid")
	}
}

func validateCitationEvidence(evidence CitationEvidence) error {
	if err := shoal.ValidateRequiredID(
		"citation evidence anchor ID", evidence.AnchorID); err != nil {
		return err
	}
	if err := shoal.ValidateRequiredID(
		"citation evidence snapshot ID", evidence.SnapshotID); err != nil {
		return err
	}
	if evidence.SnapshotAsOf.IsZero() {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"citation evidence snapshot time is required")
	}
	if evidence.Status != reasoning.VerificationVerified {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"citation envelope evidence must be verified")
	}
	if err := validateWireIDs(
		"citation evidence source ID", evidence.SourceIDs); err != nil {
		return err
	}
	for _, sourceID := range evidence.SourceIDs {
		if interaction.IsInteractionID(sourceID) {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"interaction-derived identity cannot be source evidence")
		}
	}
	if len(evidence.SourceIDs) == 0 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"citation evidence requires source identities")
	}
	if _, err := interactionVisibility(evidence.Visibility); err != nil {
		return err
	}
	switch evidence.Use {
	case reasoning.EvidenceCited:
		if evidence.Citation == nil || evidence.Path != nil ||
			len(evidence.Assertions) != 0 ||
			evidence.Origin != reasoning.OriginSource {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"cited evidence requires source-origin document citation")
		}
		if err := evidence.Citation.Validate(); err != nil {
			return err
		}
		if len(evidence.SourceIDs) != 3 {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"citation evidence must retain document, section, and span sources")
		}
		if err := shoal.ValidateRequiredID(
			"resolved citation section ID", evidence.SectionID); err != nil {
			return err
		}
		if err := shoal.ValidateRequiredID(
			"resolved citation span ID", evidence.SpanID); err != nil {
			return err
		}
		if evidence.Citation.SectionID != "" &&
			evidence.Citation.SectionID != evidence.SectionID ||
			evidence.Citation.SpanID != "" &&
				evidence.Citation.SpanID != evidence.SpanID ||
			!equalWireIDs(evidence.SourceIDs, []shoal.ID{
				evidence.Citation.DocumentID,
				evidence.SectionID,
				evidence.SpanID,
			}) {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"citation evidence source roles do not match its citation")
		}
		if evidence.Quote == "" ||
			evidence.Citation.Range.End.Offset-
				evidence.Citation.Range.Start.Offset != int64(len(evidence.Quote)) {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"citation quote does not match its source range")
		}
		anchor, err := inference.NewDocumentAnchor(
			*evidence.Citation, evidence.Quote)
		if err != nil {
			return err
		}
		if anchor.ID() != evidence.AnchorID {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"citation evidence anchor ID is not canonical")
		}
	case reasoning.EvidenceDerived:
		if evidence.Path == nil || evidence.Citation != nil ||
			evidence.Quote != "" ||
			evidence.SectionID != "" || evidence.SpanID != "" {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"derived evidence requires only a graph path")
		}
		if err := evidence.Path.Validate(); err != nil {
			return err
		}
		pathNodeIDs := make([]shoal.ID, 0, len(evidence.Path.Nodes))
		for _, node := range evidence.Path.Nodes {
			pathNodeIDs = append(pathNodeIDs, node.ID)
		}
		if !equalWireIDs(evidence.SourceIDs, pathNodeIDs) {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"derived evidence sources do not match path nodes")
		}
		assertions := make(
			[]interaction.AssertionReference, len(evidence.Assertions))
		for index, assertion := range evidence.Assertions {
			assertions[index] = interaction.AssertionReference{
				AssertionID: assertion.AssertionID,
				EdgeID:      assertion.EdgeID, Origin: assertion.Origin,
			}
		}
		anchor, err := inference.NewGraphAnchorWithAssertions(
			*evidence.Path, assertions)
		if err != nil {
			return err
		}
		if anchor.ID() != evidence.AnchorID {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"derived evidence anchor ID is not canonical")
		}
	default:
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "citation evidence use is invalid")
	}
	if evidence.Origin != reasoning.OriginSource &&
		evidence.Origin != reasoning.OriginDerived {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "citation evidence origin is invalid")
	}
	type assertionKey struct {
		assertionID shoal.ID
		edgeID      shoal.ID
	}
	seenAssertions := make(
		map[assertionKey]struct{}, len(evidence.Assertions))
	assertionsByEdge := make(
		map[shoal.ID][]CitationAssertion, len(evidence.Assertions))
	pathEdges := make(map[shoal.ID]struct{})
	if evidence.Path != nil {
		pathEdges = make(map[shoal.ID]struct{}, len(evidence.Path.Edges))
		for _, edge := range evidence.Path.Edges {
			pathEdges[edge.ID] = struct{}{}
		}
	}
	derivedPath := false
	for _, assertion := range evidence.Assertions {
		if err := shoal.ValidateRequiredID(
			"citation assertion ID", assertion.AssertionID); err != nil {
			return err
		}
		if err := shoal.ValidateRequiredID(
			"citation assertion edge ID", assertion.EdgeID); err != nil {
			return err
		}
		key := assertionKey{
			assertionID: assertion.AssertionID,
			edgeID:      assertion.EdgeID,
		}
		if _, duplicate := seenAssertions[key]; duplicate {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"citation evidence contains duplicate assertions")
		}
		seenAssertions[key] = struct{}{}
		assertionsByEdge[assertion.EdgeID] = append(
			assertionsByEdge[assertion.EdgeID], assertion)
		if _, ok := pathEdges[assertion.EdgeID]; !ok {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"citation assertion does not match a path edge")
		}
		switch assertion.Origin {
		case ontology.AssertionExplicit:
		case ontology.AssertionInferred, ontology.AssertionDerived:
			derivedPath = true
		default:
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"citation assertion origin is invalid")
		}
	}
	if evidence.Path != nil {
		for _, node := range evidence.Path.Nodes {
			if interaction.IsInteractionKind(node.Kind) {
				return shoal.NewError(
					shoal.ErrorInvalidArgument,
					"interaction-derived node cannot be source evidence")
			}
			if graph.IsProvenanceKind(node.Kind) {
				derivedPath = true
			}
		}
		for _, edge := range evidence.Path.Edges {
			if interaction.IsInteractionEdgeType(edge.Type) {
				return shoal.NewError(
					shoal.ErrorInvalidArgument,
					"interaction-derived edge cannot be source evidence")
			}
			if graph.IsProvenanceEdgeType(edge.Type) ||
				edge.Properties[ontologyAssertionOriginProperty] ==
					string(ontology.AssertionDerived) {
				derivedPath = true
			}
			assertions := assertionsByEdge[edge.ID]
			hasAssertion := len(assertions) > 0
			hasAssertionMarker :=
				edge.Properties[ontologyRelationshipIDProperty] != "" ||
					edge.Properties[ontologyAssertionIDProperty] != "" ||
					edge.Properties[ontologyAssertionOriginProperty] != ""
			if hasAssertionMarker && !hasAssertion {
				return shoal.NewError(
					shoal.ErrorInvalidArgument,
					"graph path edge is missing its assertion reference")
			}
			if origin := edge.Properties[ontologyAssertionOriginProperty]; hasAssertion && origin != "" {
				for _, assertion := range assertions {
					if origin != string(assertion.Origin) {
						return shoal.NewError(
							shoal.ErrorInvalidArgument,
							"graph path assertion origin does not match its edge")
					}
				}
			}
			if assertionID := edge.Properties[ontologyAssertionIDProperty]; hasAssertion && assertionID != "" {
				for _, assertion := range assertions {
					if shoal.ID(assertionID) != assertion.AssertionID {
						return shoal.NewError(
							shoal.ErrorInvalidArgument,
							"graph path assertion ID does not match its edge")
					}
				}
			}
		}
		expectedOrigin := reasoning.OriginSource
		if derivedPath {
			expectedOrigin = reasoning.OriginDerived
		}
		if evidence.Origin != expectedOrigin {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"graph evidence origin does not match its path provenance")
		}
	}
	return nil
}

func validateCitationEvidenceVisibility(
	evidence CitationEvidence,
	sources map[shoal.ID]CitationSource,
) error {
	visibilitySets := make([][]string, 0, len(evidence.SourceIDs))
	if evidence.Path == nil {
		for _, sourceID := range evidence.SourceIDs {
			visibilitySets = append(
				visibilitySets, sources[sourceID].Visibility)
		}
	} else {
		for _, node := range evidence.Path.Nodes {
			visibility, err := interaction.NodeVisibility(node)
			if err != nil {
				return err
			}
			source, ok := sources[node.ID]
			if !ok || !equalCitationStrings(source.Visibility, visibility) {
				return shoal.NewError(
					shoal.ErrorInvalidArgument,
					"graph source visibility does not match its path node")
			}
			visibilitySets = append(visibilitySets, visibility)
		}
		for _, edge := range evidence.Path.Edges {
			visibility, err := interaction.EdgeVisibility(edge)
			if err != nil {
				return err
			}
			visibilitySets = append(visibilitySets, visibility)
		}
	}
	required, err := interaction.Conjoin(visibilitySets...)
	if err != nil {
		return err
	}
	if !equalCitationStrings(evidence.Visibility, required) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"citation evidence visibility does not match its sources")
	}
	return nil
}

func equalCitationStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func validateWireIDs(name string, values []shoal.ID) error {
	seen := make(map[shoal.ID]struct{}, len(values))
	for _, id := range values {
		if err := shoal.ValidateRequiredID(name, id); err != nil {
			return err
		}
		if _, duplicate := seen[id]; duplicate {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, name+" is duplicated")
		}
		seen[id] = struct{}{}
	}
	return nil
}

func interactionVisibility(values []string) ([]string, error) {
	normalized, err := interaction.Conjoin(values)
	if err != nil {
		return nil, err
	}
	if len(normalized) != len(values) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"citation visibility contains duplicate labels")
	}
	for index := range normalized {
		if normalized[index] != values[index] {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"citation visibility is not canonically ordered")
		}
	}
	return normalized, nil
}

func encodeIDs(values []shoal.ID) []string {
	result := make([]string, 0, len(values))
	for _, id := range values {
		result = append(result, encodeID(id))
	}
	return result
}

func decodeCitationID(value string) (shoal.ID, error) {
	id, err := decodeID(value)
	if err != nil {
		return "", err
	}
	if base64.RawURLEncoding.EncodeToString([]byte(id)) != value {
		return "", fmt.Errorf("must be canonical unpadded base64url")
	}
	return id, nil
}

func decodeOptionalCitationID(value string) (shoal.ID, error) {
	if value == "" {
		return "", nil
	}
	return decodeCitationID(value)
}

func decodeCitationIDs(values []string) ([]shoal.ID, error) {
	result := make([]shoal.ID, 0, len(values))
	for _, value := range values {
		id, err := decodeCitationID(value)
		if err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, nil
}

func rejectDuplicateJSONKeys(data []byte, root reflect.Type) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var visit func(reflect.Type) error
	visit = func(expected reflect.Type) error {
		for expected != nil && expected.Kind() == reflect.Pointer {
			expected = expected.Elem()
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delimiter {
		case '{':
			seen := make(map[string]struct{})
			fields, dynamic := jsonFields(expected)
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return fmt.Errorf("JSON object key must be a string")
				}
				canonicalKey := key
				var valueType reflect.Type
				if !dynamic {
					if field, ok := fields.exact[key]; ok {
						canonicalKey = field.name
						valueType = field.value
					} else if field, ok := fields.folded[foldJSONName(key)]; ok {
						canonicalKey = field.name
						valueType = field.value
					}
				} else if expected != nil {
					valueType = expected.Elem()
				}
				if _, duplicate := seen[canonicalKey]; duplicate {
					return fmt.Errorf("duplicate JSON key %q", key)
				}
				seen[canonicalKey] = struct{}{}
				if err := visit(valueType); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			var element reflect.Type
			if expected != nil &&
				(expected.Kind() == reflect.Slice ||
					expected.Kind() == reflect.Array) {
				element = expected.Elem()
			}
			for decoder.More() {
				if err := visit(element); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
		}
	}
	if err := visit(root); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("JSON contains multiple values")
		}
		return err
	}
	return nil
}

type jsonField struct {
	name  string
	value reflect.Type
}

type jsonFieldSet struct {
	exact  map[string]jsonField
	folded map[string]jsonField
}

func jsonFields(value reflect.Type) (jsonFieldSet, bool) {
	if value == nil || value.Kind() != reflect.Struct {
		if value != nil && value.Kind() == reflect.Map {
			return jsonFieldSet{}, true
		}
		return jsonFieldSet{}, false
	}
	fields := jsonFieldSet{
		exact:  make(map[string]jsonField),
		folded: make(map[string]jsonField),
	}
	for index := 0; index < value.NumField(); index++ {
		field := value.Field(index)
		if !field.IsExported() {
			continue
		}
		name := field.Name
		if tag := field.Tag.Get("json"); tag != "" {
			if comma := bytes.IndexByte([]byte(tag), ','); comma >= 0 {
				tag = tag[:comma]
			}
			if tag == "-" {
				continue
			}
			if tag != "" {
				name = tag
			}
		}
		entry := jsonField{name: name, value: field.Type}
		fields.exact[name] = entry
		folded := foldJSONName(name)
		if _, exists := fields.folded[folded]; !exists {
			fields.folded[folded] = entry
		}
	}
	return fields, false
}

func foldJSONName(name string) string {
	output := make([]byte, 0, len(name))
	for len(name) > 0 {
		if name[0] < utf8.RuneSelf {
			character := name[0]
			if 'a' <= character && character <= 'z' {
				character -= 'a' - 'A'
			}
			output = append(output, character)
			name = name[1:]
			continue
		}
		value, size := utf8.DecodeRuneInString(name)
		output = utf8.AppendRune(output, foldJSONRune(value))
		name = name[size:]
	}
	return string(output)
}

func foldJSONRune(value rune) rune {
	for {
		next := unicode.SimpleFold(value)
		if next <= value {
			return next
		}
		value = next
	}
}

func equalWireIDs(left, right []shoal.ID) bool {
	left = canonicalWireIDs(left)
	right = canonicalWireIDs(right)
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func canonicalWireIDs(values []shoal.ID) []shoal.ID {
	seen := make(map[shoal.ID]struct{}, len(values))
	for _, id := range values {
		seen[id] = struct{}{}
	}
	result := make([]shoal.ID, 0, len(seen))
	for id := range seen {
		result = append(result, id)
	}
	sort.Slice(result, func(i, j int) bool {
		return shoal.CompareID(result[i], result[j]) < 0
	})
	return result
}

func visibilityCovers(actual, required []string) bool {
	actualSet := make(map[string]struct{}, len(actual))
	for _, label := range actual {
		actualSet[label] = struct{}{}
	}
	for _, label := range required {
		if _, ok := actualSet[label]; !ok {
			return false
		}
	}
	return true
}

func containsWireID(values []shoal.ID, target shoal.ID) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
