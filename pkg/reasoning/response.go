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

// Package reasoning builds citation-bearing product responses from verified
// inference results. It performs no model invocation and owns no persistence.
package reasoning

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"time"

	"github.com/phrocker/shoal-oss/pkg/contextpack"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const derivedAssertionOriginProperty = "ontology.assertion.origin"

// VerificationStatus states whether an evidence item is fit to be presented
// as a verified citation.
type VerificationStatus string

const (
	VerificationVerified   VerificationStatus = "verified"
	VerificationUnverified VerificationStatus = "unverified"
)

// EvidenceUse keeps exact source citations separate from graph explanations.
type EvidenceUse string

const (
	EvidenceCited   EvidenceUse = "cited"
	EvidenceDerived EvidenceUse = "derived"
)

// EvidenceOrigin distinguishes source graph material from explicitly derived
// graph material. Both remain separate from citation-backed document evidence.
type EvidenceOrigin string

const (
	OriginSource  EvidenceOrigin = "source"
	OriginDerived EvidenceOrigin = "derived"
)

// IssueKind is a response-level non-success outcome.
type IssueKind string

const (
	IssueUnsupported IssueKind = "unsupported"
	IssueUnverified  IssueKind = "unverified"
)

// IssueOutcomeType identifies the canonical inference outcome retained by a
// response issue.
type IssueOutcomeType string

const (
	IssueOutcomeClaim          IssueOutcomeType = "claim"
	IssueOutcomeInferenceIssue IssueOutcomeType = "inference_issue"

	// UnverifiedClaimReason is the canonical explanation for a claim that was
	// not promoted because it lacked verified citation-backed source evidence.
	UnverifiedClaimReason = "claim has no verified citation-backed source evidence"
)

// Policy binds response construction to the context policy and optionally
// narrows the output with extra visibility terms. Derived graph material is
// disabled by default and, when enabled, still never becomes a citation.
type Policy struct {
	ID                    shoal.ID
	ExtraOutputVisibility []string
	AllowDerivedEvidence  bool
}

// BuildInput is the complete successful generator output to verify. Generator
// errors are returned by the producer and are never converted into a response.
type BuildInput struct {
	ContextPack     inference.ContextPack
	Result          inference.InferenceResult
	Policy          Policy
	EmbeddingSpaces interaction.EmbeddingSpaceSet
}

// Builder verifies result evidence through the same contextpack hydration
// seam that created the model context.
type Builder struct {
	verifier contextpack.Builder
}

// Recorder is the result-returning durable recorder boundary used by Capture.
// interaction.Recorder satisfies this interface.
type Recorder interface {
	Record(context.Context, interaction.Session) (interaction.Session, error)
}

// NewBuilder constructs a response builder over an authorized,
// snapshot-aware evidence reader.
func NewBuilder(reader contextpack.AuthorizationReader) (*Builder, error) {
	return NewBuilderWithLimits(reader, contextpack.Limits{})
}

// NewBuilderWithLimits constructs a response builder with the same explicit
// hydration limits used to build its context packs.
func NewBuilderWithLimits(
	reader contextpack.AuthorizationReader,
	limits contextpack.Limits,
) (*Builder, error) {
	if reader == nil || isNil(reader) {
		return nil, invalid("reasoning evidence reader is required")
	}
	return &Builder{
		verifier: contextpack.Builder{Reader: reader, Limits: limits},
	}, nil
}

// Evidence is an immutable verified anchor with its original snapshot,
// complete source identities, visibility, use, and origin.
type Evidence struct {
	anchor       inference.EvidenceAnchor
	snapshot     inference.SnapshotPin
	status       VerificationStatus
	use          EvidenceUse
	origin       EvidenceOrigin
	sourceIDs    []shoal.ID
	sectionID    shoal.ID
	spanID       shoal.ID
	visibility   []string
	fromAddition bool
	reference    interaction.EvidenceReference
}

func (e Evidence) Anchor() inference.EvidenceAnchor { return e.anchor }
func (e Evidence) Snapshot() inference.SnapshotPin  { return e.snapshot }
func (e Evidence) Status() VerificationStatus       { return e.status }
func (e Evidence) Use() EvidenceUse                 { return e.use }
func (e Evidence) Origin() EvidenceOrigin           { return e.origin }
func (e Evidence) SourceIDs() []shoal.ID {
	return append([]shoal.ID(nil), e.sourceIDs...)
}
func (e Evidence) ResolvedSectionID() shoal.ID { return e.sectionID }
func (e Evidence) ResolvedSpanID() shoal.ID    { return e.spanID }
func (e Evidence) Visibility() []string {
	return append([]string(nil), e.visibility...)
}
func (e Evidence) FromAddition() bool { return e.fromAddition }
func (e Evidence) Reference() interaction.EvidenceReference {
	canonical, _ := e.reference.Canonical()
	return canonical
}

// SourceReference identifies every exact source touched by the verified
// context, the anchors that referenced it, and its source visibility.
type SourceReference struct {
	id         shoal.ID
	anchorIDs  []shoal.ID
	visibility []string
}

func (s SourceReference) ID() shoal.ID { return s.id }
func (s SourceReference) AnchorIDs() []shoal.ID {
	return append([]shoal.ID(nil), s.anchorIDs...)
}
func (s SourceReference) Visibility() []string {
	return append([]string(nil), s.visibility...)
}

// Claim is a successful grounded claim. Citations contains only verified
// document anchors. Graph paths remain in DerivedEvidence and never become
// citation or interaction.Cited identities.
type Claim struct {
	claim       inference.Claim
	citations   []Evidence
	derivations []Evidence
}

func (c Claim) Value() inference.Claim { return c.claim }
func (c Claim) Citations() []Evidence  { return cloneEvidence(c.citations) }
func (c Claim) DerivedEvidence() []Evidence {
	return cloneEvidence(c.derivations)
}

// Issue represents unsupported generator output or an outcome that could not
// be promoted to a verified citation-backed claim.
type Issue struct {
	kind        IssueKind
	outcomeType IssueOutcomeType
	outcomeID   shoal.ID
	input       string
	reason      string
	claim       inference.Claim
	hasClaim    bool
	evidence    []Evidence
}

func (i Issue) Kind() IssueKind               { return i.kind }
func (i Issue) OutcomeType() IssueOutcomeType { return i.outcomeType }
func (i Issue) OutcomeID() shoal.ID           { return i.outcomeID }
func (i Issue) Input() string                 { return i.input }
func (i Issue) Reason() string                { return i.reason }
func (i Issue) Evidence() []Evidence          { return cloneEvidence(i.evidence) }
func (i Issue) Claim() (inference.Claim, bool) {
	return i.claim, i.hasClaim
}

// ResponseIdentitySource is the source projection used to derive a response
// identity across transport boundaries.
type ResponseIdentitySource struct {
	ID         shoal.ID
	AnchorIDs  []shoal.ID
	Visibility []string
}

// ResponseIdentityEvidence is the evidence projection used to derive a
// response identity across transport boundaries.
type ResponseIdentityEvidence struct {
	AnchorID     shoal.ID
	Status       VerificationStatus
	Use          EvidenceUse
	Origin       EvidenceOrigin
	FromAddition bool
	SourceIDs    []shoal.ID
	SectionID    shoal.ID
	SpanID       shoal.ID
	Visibility   []string
}

// ResponseIdentityIssue is the issue projection used to derive a response
// identity across transport boundaries.
type ResponseIdentityIssue struct {
	Kind        IssueKind
	OutcomeType IssueOutcomeType
	OutcomeID   shoal.ID
	Input       string
	Reason      string
}

// ResponseIdentity is the complete verified payload projection that binds a
// durable response ID.
type ResponseIdentity struct {
	ContextPackID            shoal.ID
	ResultID                 shoal.ID
	PolicyID                 shoal.ID
	RequestID                shoal.ID
	SnapshotID               shoal.ID
	SnapshotAsOf             time.Time
	AuthorizationFingerprint shoal.ID
	AuthorizationExpiresAt   time.Time
	EmbeddingSpaces          interaction.EmbeddingSpaceSet
	GeneratedAt              time.Time
	EffectiveVisibility      []string
	RetrievedSourceIDs       []shoal.ID
	CitedSourceIDs           []shoal.ID
	Sources                  []ResponseIdentitySource
	RetrievedEvidence        []interaction.EvidenceReference
	CitedEvidence            []interaction.EvidenceReference
	Evidence                 []ResponseIdentityEvidence
	ClaimIDs                 []shoal.ID
	Issues                   []ResponseIdentityIssue
}

// CaptureMetadata is the exact provenance a producer must put into its
// interaction.Session before attempting durable capture.
type CaptureMetadata struct {
	contextPackID             shoal.ID
	resultID                  shoal.ID
	policyID                  shoal.ID
	requestID                 shoal.ID
	snapshot                  inference.SnapshotPin
	authorization             inference.AuthPin
	embeddingSpaces           interaction.EmbeddingSpaceSet
	generatedAt               time.Time
	retrievedSourceIDs        []shoal.ID
	citedSourceIDs            []shoal.ID
	seedSourceIDs             []shoal.ID
	additionSourceIDs         []shoal.ID
	retrievedEvidence         []interaction.EvidenceReference
	citedEvidence             []interaction.EvidenceReference
	seedEvidence              []interaction.EvidenceReference
	additionEvidence          []interaction.EvidenceReference
	effectiveOutputVisibility []string
}

func (m CaptureMetadata) ContextPackID() shoal.ID { return m.contextPackID }
func (m CaptureMetadata) ResultID() shoal.ID      { return m.resultID }
func (m CaptureMetadata) PolicyID() shoal.ID      { return m.policyID }
func (m CaptureMetadata) RequestID() shoal.ID     { return m.requestID }
func (m CaptureMetadata) Snapshot() inference.SnapshotPin {
	return m.snapshot
}
func (m CaptureMetadata) Authorization() inference.AuthPin {
	return m.authorization
}
func (m CaptureMetadata) EmbeddingSpaces() interaction.EmbeddingSpaceSet {
	result, _ := m.embeddingSpaces.Canonical()
	return result
}
func (m CaptureMetadata) GeneratedAt() time.Time { return m.generatedAt }
func (m CaptureMetadata) RetrievedSourceIDs() []shoal.ID {
	return append([]shoal.ID(nil), m.retrievedSourceIDs...)
}
func (m CaptureMetadata) CitedSourceIDs() []shoal.ID {
	return append([]shoal.ID(nil), m.citedSourceIDs...)
}
func (m CaptureMetadata) SeedSourceIDs() []shoal.ID {
	return append([]shoal.ID(nil), m.seedSourceIDs...)
}
func (m CaptureMetadata) AdditionSourceIDs() []shoal.ID {
	return append([]shoal.ID(nil), m.additionSourceIDs...)
}
func (m CaptureMetadata) SeedEvidence() []interaction.EvidenceReference {
	return cloneInteractionEvidence(m.seedEvidence)
}
func (m CaptureMetadata) AdditionEvidence() []interaction.EvidenceReference {
	return cloneInteractionEvidence(m.additionEvidence)
}
func (m CaptureMetadata) RetrievedEvidence() []interaction.EvidenceReference {
	return cloneInteractionEvidence(m.retrievedEvidence)
}
func (m CaptureMetadata) CitedEvidence() []interaction.EvidenceReference {
	return cloneInteractionEvidence(m.citedEvidence)
}
func (m CaptureMetadata) EffectiveOutputVisibility() []string {
	return append([]string(nil), m.effectiveOutputVisibility...)
}

// NewSession creates the minimal inference/chat session for this response.
// identityTime participates only in OperationSessionID; the trusted Recorder
// assigns RecordedAt. Producers may add actor, reason, provenance, stop reason,
// and turn detail before Capture.
func (m CaptureMetadata) NewSession(
	operation interaction.Operation,
	correlationID shoal.ID,
	identityTime time.Time,
) (interaction.Session, error) {
	if !operation.HasInference() {
		return interaction.Session{}, invalid(
			"reasoning response session requires an inference or chat operation")
	}
	sessionID, err := interaction.OperationSessionID(
		operation, correlationID, identityTime)
	if err != nil {
		return interaction.Session{}, err
	}
	session := interaction.Session{
		ID:                       sessionID,
		Operation:                operation,
		SnapshotID:               m.snapshot.ID(),
		SnapshotAsOf:             m.snapshot.AsOf(),
		AuthorizationFingerprint: m.authorization.Fingerprint(),
		AuthorizationExpiresAt:   m.authorization.ExpiresAt(),
		EmbeddingSpaces:          m.EmbeddingSpaces(),
		RequestID:                m.requestID,
		ContextPackID:            m.contextPackID,
		ResultID:                 m.resultID,
		SeedNodeIDs:              append([]shoal.ID(nil), m.seedSourceIDs...),
		CitedNodeIDs:             append([]shoal.ID(nil), m.citedSourceIDs...),
		SeedEvidence:             cloneInteractionEvidence(m.seedEvidence),
		CitedEvidence:            cloneInteractionEvidence(m.citedEvidence),
	}
	validation := session
	validation.RecordedAt = m.generatedAt
	if err := validation.Validate(); err != nil {
		return interaction.Session{}, err
	}
	return session, nil
}

type responseData struct {
	contextPackID             shoal.ID
	resultID                  shoal.ID
	policyID                  shoal.ID
	requestID                 shoal.ID
	snapshot                  inference.SnapshotPin
	authorization             inference.AuthPin
	embeddingSpaces           interaction.EmbeddingSpaceSet
	generatedAt               time.Time
	effectiveOutputVisibility []string
	retrievedSourceIDs        []shoal.ID
	citedSourceIDs            []shoal.ID
	seedSourceIDs             []shoal.ID
	additionSourceIDs         []shoal.ID
	retrievedEvidence         []interaction.EvidenceReference
	citedEvidence             []interaction.EvidenceReference
	seedEvidence              []interaction.EvidenceReference
	additionEvidence          []interaction.EvidenceReference
	sources                   []SourceReference
	evidence                  []Evidence
	claims                    []Claim
	issues                    []Issue
	fingerprint               string
}

// PreparedResponse has passed evidence and policy validation but is
// deliberately not serializable. Capture is the only path to a Response.
type PreparedResponse struct {
	builder *Builder
	input   BuildInput
	data    responseData
}

// CaptureMetadata returns the redacted provenance needed to construct and
// validate the interaction.Session that will be durably recorded.
func (p PreparedResponse) CaptureMetadata() CaptureMetadata {
	return captureMetadata(p.data)
}

// Response is the immutable, durably captured product response.
type Response struct {
	id         shoal.ID
	sessionID  shoal.ID
	recordedAt time.Time
	session    interaction.Session
	data       responseData
}

func (r Response) ID() shoal.ID          { return r.id }
func (r Response) SessionID() shoal.ID   { return r.sessionID }
func (r Response) RecordedAt() time.Time { return r.recordedAt }
func (r Response) RecordedSession() interaction.Session {
	canonical, _ := r.session.Canonical()
	return canonical
}
func (r Response) ContextPackID() shoal.ID          { return r.data.contextPackID }
func (r Response) ResultID() shoal.ID               { return r.data.resultID }
func (r Response) PolicyID() shoal.ID               { return r.data.policyID }
func (r Response) RequestID() shoal.ID              { return r.data.requestID }
func (r Response) Snapshot() inference.SnapshotPin  { return r.data.snapshot }
func (r Response) Authorization() inference.AuthPin { return r.data.authorization }
func (r Response) EmbeddingSpaces() interaction.EmbeddingSpaceSet {
	result, _ := r.data.embeddingSpaces.Canonical()
	return result
}
func (r Response) GeneratedAt() time.Time { return r.data.generatedAt }
func (r Response) EffectiveOutputVisibility() []string {
	return append([]string(nil), r.data.effectiveOutputVisibility...)
}
func (r Response) RetrievedSourceIDs() []shoal.ID {
	return append([]shoal.ID(nil), r.data.retrievedSourceIDs...)
}
func (r Response) CitedSourceIDs() []shoal.ID {
	return append([]shoal.ID(nil), r.data.citedSourceIDs...)
}
func (r Response) Sources() []SourceReference {
	return cloneSources(r.data.sources)
}
func (r Response) Evidence() []Evidence { return cloneEvidence(r.data.evidence) }
func (r Response) Claims() []Claim      { return cloneClaims(r.data.claims) }
func (r Response) Issues() []Issue      { return cloneIssues(r.data.issues) }

// Validate checks the captured identity and the canonical verified payload.
func (r Response) Validate() error {
	if err := shoal.ValidateRequiredID(
		"reasoning response ID", r.id); err != nil {
		return err
	}
	if err := shoal.ValidateRequiredID(
		"reasoning response session ID", r.sessionID); err != nil {
		return err
	}
	if r.recordedAt.IsZero() {
		return invalid("reasoning response recording time is required")
	}
	if err := validateCaptureSession(r.session, captureMetadata(r.data)); err != nil {
		return err
	}
	if r.session.ID != r.sessionID ||
		!r.session.RecordedAt.Equal(r.recordedAt) {
		return invalid("reasoning response session identity is inconsistent")
	}
	fingerprint, err := responseFingerprint(r.data)
	if err != nil {
		return err
	}
	if fingerprint != r.data.fingerprint {
		return invalid("reasoning response verification fingerprint is not canonical")
	}
	expected, err := CanonicalResponseID(
		r.sessionID, r.recordedAt, responseIdentity(r.data))
	if err != nil {
		return err
	}
	if expected != r.id {
		return invalid("reasoning response ID is not canonical")
	}
	return nil
}

// Build validates policy identity, re-verifies all context and result evidence,
// and prepares an immutable response that cannot be obtained before capture.
func (b *Builder) Build(
	ctx context.Context,
	input BuildInput,
) (PreparedResponse, error) {
	if b == nil {
		return PreparedResponse{}, invalid("reasoning response builder is required")
	}
	data, err := b.assemble(ctx, input)
	if err != nil {
		return PreparedResponse{}, err
	}
	return PreparedResponse{
		builder: b,
		input: BuildInput{
			ContextPack:     input.ContextPack,
			Result:          input.Result,
			EmbeddingSpaces: data.embeddingSpaces,
			Policy: Policy{
				ID: input.Policy.ID,
				ExtraOutputVisibility: append(
					[]string(nil), input.Policy.ExtraOutputVisibility...),
				AllowDerivedEvidence: input.Policy.AllowDerivedEvidence,
			},
		},
		data: data,
	}, nil
}

// Capture requires the producer's complete interaction session, verifies that
// it records exactly the response's retrieved/cited identities and pins, and
// returns a Response only after interaction.Recorder confirms durable capture.
func (p PreparedResponse) Capture(
	ctx context.Context,
	recorder Recorder,
	session interaction.Session,
) (Response, error) {
	if p.builder == nil || p.data.fingerprint == "" {
		return Response{}, invalid("prepared reasoning response is required")
	}
	if recorder == nil || isNil(recorder) {
		return Response{}, invalid("interaction recorder is required")
	}
	before, err := p.builder.assemble(ctx, p.input)
	if err != nil {
		return Response{}, err
	}
	if before.fingerprint != p.data.fingerprint {
		return Response{}, shoal.NewError(
			shoal.ErrorUnavailable,
			"reasoning evidence changed before durable capture",
		)
	}
	callerSession := session
	callerSession.RecordedAt = before.generatedAt
	callerSession, err = callerSession.Canonical()
	if err != nil {
		return Response{}, err
	}
	if err := validateCaptureSession(
		callerSession, captureMetadata(before)); err != nil {
		return Response{}, err
	}
	callerSession.RecordedAt = time.Time{}
	recorded, err := recorder.Record(ctx, callerSession)
	if err != nil {
		return Response{}, err
	}
	recorded, err = recorded.Canonical()
	if err != nil {
		return Response{}, explorer.MarkCommittedInteraction(
			fmt.Errorf("persisted interaction session: %w", err))
	}
	if err := validateCaptureSession(
		recorded, captureMetadata(before)); err != nil {
		return Response{}, explorer.MarkCommittedInteraction(
			fmt.Errorf("persisted interaction session: %w", err))
	}
	if recorded.ID != callerSession.ID ||
		recorded.Operation != callerSession.Operation {
		return Response{}, explorer.MarkCommittedInteraction(invalid(
			"persisted interaction session identity changed during capture"))
	}
	after, err := p.builder.assemble(ctx, p.input)
	if err != nil {
		return Response{}, explorer.MarkCommittedInteraction(err)
	}
	if after.fingerprint != before.fingerprint {
		return Response{}, explorer.MarkCommittedInteraction(shoal.NewError(
			shoal.ErrorUnavailable,
			"reasoning evidence changed during durable capture",
		))
	}
	id, err := CanonicalResponseID(
		recorded.ID, recorded.RecordedAt, responseIdentity(after))
	if err != nil {
		return Response{}, explorer.MarkCommittedInteraction(err)
	}
	response := Response{
		id:         id,
		sessionID:  recorded.ID,
		recordedAt: recorded.RecordedAt.UTC(),
		session:    recorded,
		data:       cloneResponseData(after),
	}
	if err := response.Validate(); err != nil {
		return Response{}, explorer.MarkCommittedInteraction(err)
	}
	return response, nil
}

func (b *Builder) assemble(
	ctx context.Context,
	input BuildInput,
) (responseData, error) {
	if err := input.ContextPack.Validate(); err != nil {
		return responseData{}, err
	}
	if err := input.Result.ValidateFor(input.ContextPack); err != nil {
		return responseData{}, err
	}
	policyID, err := contextpack.PolicyID(input.ContextPack)
	if err != nil {
		return responseData{}, err
	}
	if err := shoal.ValidateRequiredID(
		"reasoning response policy ID", input.Policy.ID,
	); err != nil {
		return responseData{}, err
	}
	if input.Policy.ID != policyID {
		return responseData{}, invalid(
			"reasoning response policy does not match the context pack")
	}
	extraVisibility, err := interaction.Conjoin(
		input.Policy.ExtraOutputVisibility)
	if err != nil {
		return responseData{}, err
	}
	if input.Result.GeneratedAt().Before(input.ContextPack.Snapshot().AsOf()) {
		return responseData{}, invalid(
			"inference result predates the verified context snapshot")
	}
	if !input.Result.GeneratedAt().Before(
		input.ContextPack.Authorization().ExpiresAt(),
	) {
		return responseData{}, shoal.NewError(
			shoal.ErrorUnauthorized,
			"inference result was generated after authorization expired",
		)
	}

	verification, err := b.verifier.VerifyResult(
		ctx, input.ContextPack, input.Result)
	if err != nil {
		return responseData{}, err
	}
	evidenceByID := make(map[shoal.ID]Evidence, len(verification.Anchors()))
	sourceByID := make(map[shoal.ID]SourceReference)
	visibilitySets := make([][]string, 0, len(verification.Anchors())+1)
	var retrieved []shoal.ID
	var seedSources []shoal.ID
	var additionSources []shoal.ID
	var retrievedEvidence []interaction.EvidenceReference
	var seedEvidence []interaction.EvidenceReference
	var additionEvidence []interaction.EvidenceReference
	for _, verified := range verification.Anchors() {
		evidence, err := verifiedEvidence(
			verified,
			input.ContextPack.Snapshot(),
			input.Policy.AllowDerivedEvidence,
		)
		if err != nil {
			return responseData{}, err
		}
		evidenceByID[evidence.anchor.ID()] = evidence
		visibilitySets = append(visibilitySets, evidence.visibility)
		reference := evidence.Reference()
		retrievedEvidence = append(retrievedEvidence, reference)
		retrieved = append(retrieved, evidence.sourceIDs...)
		if evidence.fromAddition {
			additionEvidence = append(additionEvidence, reference)
			additionSources = append(additionSources, evidence.sourceIDs...)
		} else {
			seedEvidence = append(seedEvidence, reference)
			seedSources = append(seedSources, evidence.sourceIDs...)
		}
		for _, source := range verified.Sources() {
			current := sourceByID[source.ID()]
			visibility, err := interaction.Conjoin(
				current.visibility, source.Visibility())
			if err != nil {
				return responseData{}, err
			}
			current.id = source.ID()
			current.anchorIDs = append(
				current.anchorIDs, evidence.anchor.ID())
			current.anchorIDs = canonicalIDs(current.anchorIDs)
			current.visibility = visibility
			sourceByID[source.ID()] = current
		}
	}
	visibilitySets = append(visibilitySets, extraVisibility)
	outputVisibility, err := interaction.Conjoin(visibilitySets...)
	if err != nil {
		return responseData{}, err
	}
	retrieved = canonicalIDs(retrieved)
	seedSources = canonicalIDs(seedSources)
	additionSources = canonicalIDs(additionSources)

	claims := make([]Claim, 0, len(input.Result.Claims()))
	issues := make([]Issue, 0)
	var cited []shoal.ID
	var citedEvidence []interaction.EvidenceReference
	citedEvidenceByID := make(map[shoal.ID]interaction.EvidenceReference)
	for _, claim := range input.Result.Claims() {
		citations, derivations, err := outcomeEvidence(
			claim.EvidenceIDs(), evidenceByID)
		if err != nil {
			return responseData{}, err
		}
		if len(citations) == 0 {
			issues = append(issues, Issue{
				kind:        IssueUnverified,
				outcomeType: IssueOutcomeClaim,
				outcomeID:   claim.ID(),
				input:       string(claim.ID()),
				reason:      UnverifiedClaimReason,
				claim:       claim,
				hasClaim:    true,
				evidence:    derivations,
			})
			continue
		}
		for _, citation := range citations {
			cited = append(cited, citation.sourceIDs...)
			reference := citation.Reference()
			if existing, duplicate := citedEvidenceByID[reference.AnchorID]; duplicate {
				if !interactionEvidenceEqual(existing, reference) {
					return responseData{}, invalid(
						"verified citation anchor has conflicting references")
				}
				continue
			}
			citedEvidenceByID[reference.AnchorID] = reference
			citedEvidence = append(citedEvidence, reference)
		}
		claims = append(claims, Claim{
			claim:       claim,
			citations:   citations,
			derivations: derivations,
		})
	}

	inferenceIssues := append(
		input.Result.Unresolved(), input.Result.Unsupported()...)
	sort.Slice(inferenceIssues, func(i, j int) bool {
		return shoal.CompareID(
			inferenceIssues[i].ID(), inferenceIssues[j].ID()) < 0
	})
	for _, issue := range inferenceIssues {
		citations, derivations, err := outcomeEvidence(
			issue.EvidenceIDs(), evidenceByID)
		if err != nil {
			return responseData{}, err
		}
		kind := IssueUnverified
		if issue.Kind() == inference.IssueUnsupported {
			kind = IssueUnsupported
		}
		issues = append(issues, Issue{
			kind:        kind,
			outcomeType: IssueOutcomeInferenceIssue,
			outcomeID:   issue.ID(),
			input:       issue.Input(),
			reason:      issue.Reason(),
			evidence:    append(citations, derivations...),
		})
	}
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].kind != issues[j].kind {
			return issues[i].kind < issues[j].kind
		}
		return shoal.CompareID(
			issues[i].outcomeID, issues[j].outcomeID) < 0
	})

	sources := make([]SourceReference, 0, len(sourceByID))
	for _, source := range sourceByID {
		sources = append(sources, source)
	}
	sort.Slice(sources, func(i, j int) bool {
		return shoal.CompareID(sources[i].id, sources[j].id) < 0
	})
	allEvidence := make([]Evidence, 0, len(evidenceByID))
	for _, evidence := range evidenceByID {
		allEvidence = append(allEvidence, evidence)
	}
	sort.Slice(allEvidence, func(i, j int) bool {
		return shoal.CompareID(
			allEvidence[i].anchor.ID(), allEvidence[j].anchor.ID()) < 0
	})
	cited = canonicalIDs(cited)
	requestID, _, err := contextpack.RetrievalRequestID(input.ContextPack)
	if err != nil {
		return responseData{}, err
	}
	embeddingSpaces, err := input.EmbeddingSpaces.Canonical()
	if err != nil {
		return responseData{}, err
	}
	data := responseData{
		contextPackID:             input.ContextPack.ID(),
		resultID:                  input.Result.ID(),
		policyID:                  policyID,
		requestID:                 requestID,
		snapshot:                  input.ContextPack.Snapshot(),
		authorization:             input.ContextPack.Authorization(),
		embeddingSpaces:           embeddingSpaces,
		generatedAt:               input.Result.GeneratedAt(),
		effectiveOutputVisibility: outputVisibility,
		retrievedSourceIDs:        retrieved,
		citedSourceIDs:            cited,
		seedSourceIDs:             seedSources,
		additionSourceIDs:         additionSources,
		retrievedEvidence:         retrievedEvidence,
		citedEvidence:             citedEvidence,
		seedEvidence:              seedEvidence,
		additionEvidence:          additionEvidence,
		sources:                   sources,
		evidence:                  allEvidence,
		claims:                    claims,
		issues:                    issues,
	}
	data.fingerprint, err = responseFingerprint(data)
	if err != nil {
		return responseData{}, err
	}
	return data, nil
}

func verifiedEvidence(
	verified contextpack.VerifiedAnchor,
	snapshot inference.SnapshotPin,
	allowDerived bool,
) (Evidence, error) {
	anchor := verified.Anchor()
	evidence := Evidence{
		anchor:       anchor,
		snapshot:     snapshot,
		status:       VerificationVerified,
		origin:       OriginSource,
		visibility:   verified.Visibility(),
		fromAddition: verified.Addition(),
	}
	reference, err := verified.EvidenceReference()
	if err != nil {
		return Evidence{}, err
	}
	evidence.reference = reference
	for _, source := range verified.Sources() {
		evidence.sourceIDs = append(evidence.sourceIDs, source.ID())
	}
	evidence.sourceIDs = canonicalIDs(evidence.sourceIDs)
	switch anchor.Kind() {
	case inference.AnchorDocument:
		evidence.use = EvidenceCited
		citation, _, _ := anchor.Document()
		evidence.sectionID, evidence.spanID, err =
			resolvedDocumentSourceIDs(citation, evidence.sourceIDs)
		if err != nil {
			return Evidence{}, err
		}
	case inference.AnchorGraph:
		evidence.use = EvidenceDerived
		path, ok := anchor.Path()
		if !ok {
			return Evidence{}, invalid("verified graph evidence is unavailable")
		}
		derived := false
		for _, assertion := range verified.Assertions() {
			switch assertion.Origin() {
			case ontology.AssertionInferred, ontology.AssertionDerived:
				derived = true
			}
		}
		for _, node := range path.Nodes {
			if interaction.IsInteractionKind(node.Kind) {
				return Evidence{}, invalid(
					"interaction-derived content cannot be source evidence")
			}
			if graph.IsProvenanceKind(node.Kind) {
				derived = true
			}
		}
		for _, edge := range path.Edges {
			if interaction.IsInteractionEdgeType(edge.Type) {
				return Evidence{}, invalid(
					"interaction-derived edges cannot be source evidence")
			}
			if graph.IsProvenanceEdgeType(edge.Type) ||
				edge.Properties[derivedAssertionOriginProperty] ==
					string(ontology.AssertionDerived) {
				derived = true
			}
		}
		if derived {
			evidence.origin = OriginDerived
			if !allowDerived {
				return Evidence{}, invalid(
					"derived graph evidence is disabled by source-only policy")
			}
		}
	default:
		return Evidence{}, invalid("verified evidence kind is unsupported")
	}
	return evidence, nil
}

func resolvedDocumentSourceIDs(
	citation document.Citation,
	sourceIDs []shoal.ID,
) (shoal.ID, shoal.ID, error) {
	if len(sourceIDs) != 3 || !containsID(sourceIDs, citation.DocumentID) {
		return "", "", invalid(
			"verified citation sources do not retain document, section, and span")
	}
	sectionID := citation.SectionID
	spanID := citation.SpanID
	for _, sourceID := range sourceIDs {
		if sourceID == citation.DocumentID ||
			sourceID == sectionID || sourceID == spanID {
			continue
		}
		switch {
		case sectionID == "":
			sectionID = sourceID
		case spanID == "":
			spanID = sourceID
		default:
			return "", "", invalid(
				"verified citation contains an unexpected source identity")
		}
	}
	if sectionID == "" || spanID == "" ||
		!containsID(sourceIDs, sectionID) ||
		!containsID(sourceIDs, spanID) {
		return "", "", invalid(
			"verified citation sources do not identify section and span")
	}
	return sectionID, spanID, nil
}

func outcomeEvidence(
	ids []shoal.ID,
	evidenceByID map[shoal.ID]Evidence,
) ([]Evidence, []Evidence, error) {
	citations := make([]Evidence, 0, len(ids))
	derivations := make([]Evidence, 0, len(ids))
	for _, id := range ids {
		evidence, ok := evidenceByID[id]
		if !ok {
			return nil, nil, invalid(
				"outcome references evidence outside verified result")
		}
		switch evidence.use {
		case EvidenceCited:
			if evidence.status != VerificationVerified {
				return nil, nil, invalid(
					"successful citation is not verified")
			}
			citations = append(citations, evidence)
		case EvidenceDerived:
			derivations = append(derivations, evidence)
		default:
			return nil, nil, invalid("verified evidence use is unsupported")
		}
	}
	return citations, derivations, nil
}

func validateCaptureSession(
	session interaction.Session,
	metadata CaptureMetadata,
) error {
	canonical, err := session.Canonical()
	if err != nil {
		return err
	}
	if !canonical.Operation.HasInference() {
		return invalid(
			"reasoning response capture requires an inference or chat operation")
	}
	if canonical.ContextPackID != metadata.contextPackID ||
		canonical.ResultID != metadata.resultID {
		return invalid(
			"interaction session does not match the reasoning result")
	}
	if canonical.SnapshotID != metadata.snapshot.ID() ||
		!canonical.SnapshotAsOf.Equal(metadata.snapshot.AsOf()) {
		return invalid(
			"interaction session snapshot does not match the reasoning result")
	}
	if canonical.AuthorizationFingerprint !=
		metadata.authorization.Fingerprint() ||
		!canonical.AuthorizationExpiresAt.Equal(
			metadata.authorization.ExpiresAt()) {
		return invalid(
			"interaction session authorization does not match the reasoning result")
	}
	if !reflect.DeepEqual(
		canonical.EmbeddingSpaces, metadata.embeddingSpaces,
	) {
		return invalid(
			"interaction session embedding spaces do not match the reasoning result")
	}
	if canonical.RecordedAt.Before(metadata.snapshot.AsOf()) ||
		canonical.RecordedAt.Before(metadata.generatedAt) {
		return invalid(
			"interaction session recording time predates verified input")
	}
	if !canonical.RecordedAt.Before(
		metadata.authorization.ExpiresAt()) {
		return shoal.NewError(
			shoal.ErrorUnauthorized,
			"interaction session recording time is outside authorization")
	}
	if canonical.RequestID != metadata.requestID {
		return invalid(
			"interaction session request does not match the reasoning result")
	}
	if !equalIDs(
		canonical.CitedNodeIDs, metadata.citedSourceIDs,
	) {
		return invalid(
			"interaction session cited sources do not match verified citations")
	}
	if !equalIDs(canonical.SeedNodeIDs, metadata.seedSourceIDs) {
		return invalid(
			"interaction session seed sources do not match initial context")
	}
	if !equalIDs(
		canonical.TouchedNodeIDs(), metadata.retrievedSourceIDs,
	) {
		return invalid(
			"interaction session touched sources do not match verified retrieval")
	}
	if !equalInteractionEvidence(
		canonical.SeedEvidence, metadata.seedEvidence,
	) {
		return invalid(
			"interaction session seed evidence does not match initial context")
	}
	retrievedEvidence, err := retrievedSessionEvidence(canonical)
	if err != nil {
		return err
	}
	if !equalInteractionEvidence(
		retrievedEvidence, metadata.retrievedEvidence,
	) {
		return invalid(
			"interaction session retrieved evidence does not match verification")
	}
	if !equalInteractionEvidence(
		canonical.CitedEvidence, metadata.citedEvidence,
	) {
		return invalid(
			"interaction session cited evidence does not match verification")
	}
	return nil
}

func retrievedSessionEvidence(
	session interaction.Session,
) ([]interaction.EvidenceReference, error) {
	values := append(
		[]interaction.EvidenceReference(nil), session.SeedEvidence...)
	for _, turn := range session.Turns {
		if turn.ToolCall != nil {
			values = append(values, turn.ToolCall.RetrievedEvidence...)
		}
	}
	byAnchor := make(map[shoal.ID]interaction.EvidenceReference, len(values))
	for _, value := range values {
		canonical, err := value.Canonical()
		if err != nil {
			return nil, err
		}
		if existing, duplicate := byAnchor[canonical.AnchorID]; duplicate {
			if !interactionEvidenceEqual(existing, canonical) {
				return nil, invalid(
					"interaction session contains conflicting retrieved evidence")
			}
			continue
		}
		byAnchor[canonical.AnchorID] = canonical
	}
	result := make([]interaction.EvidenceReference, 0, len(byAnchor))
	for _, value := range byAnchor {
		result = append(result, value)
	}
	return canonicalInteractionEvidence(result)
}

func captureMetadata(data responseData) CaptureMetadata {
	return CaptureMetadata{
		contextPackID:      data.contextPackID,
		resultID:           data.resultID,
		policyID:           data.policyID,
		requestID:          data.requestID,
		snapshot:           data.snapshot,
		authorization:      data.authorization,
		embeddingSpaces:    cloneEmbeddingSpaces(data.embeddingSpaces),
		generatedAt:        data.generatedAt,
		retrievedSourceIDs: append([]shoal.ID(nil), data.retrievedSourceIDs...),
		citedSourceIDs:     append([]shoal.ID(nil), data.citedSourceIDs...),
		seedSourceIDs:      append([]shoal.ID(nil), data.seedSourceIDs...),
		additionSourceIDs:  append([]shoal.ID(nil), data.additionSourceIDs...),
		retrievedEvidence:  cloneInteractionEvidence(data.retrievedEvidence),
		citedEvidence:      cloneInteractionEvidence(data.citedEvidence),
		seedEvidence:       cloneInteractionEvidence(data.seedEvidence),
		additionEvidence:   cloneInteractionEvidence(data.additionEvidence),
		effectiveOutputVisibility: append(
			[]string(nil), data.effectiveOutputVisibility...),
	}
}

func responseFingerprint(data responseData) (string, error) {
	return ResponseFingerprint(responseIdentity(data))
}

func responseIdentity(data responseData) ResponseIdentity {
	identity := ResponseIdentity{
		ContextPackID: data.contextPackID, ResultID: data.resultID,
		PolicyID: data.policyID, RequestID: data.requestID,
		SnapshotID: data.snapshot.ID(), SnapshotAsOf: data.snapshot.AsOf(),
		AuthorizationFingerprint: data.authorization.Fingerprint(),
		AuthorizationExpiresAt:   data.authorization.ExpiresAt(),
		EmbeddingSpaces:          cloneEmbeddingSpaces(data.embeddingSpaces),
		GeneratedAt:              data.generatedAt,
		EffectiveVisibility: append(
			[]string(nil), data.effectiveOutputVisibility...),
		RetrievedSourceIDs: append(
			[]shoal.ID(nil), data.retrievedSourceIDs...),
		CitedSourceIDs: append(
			[]shoal.ID(nil), data.citedSourceIDs...),
		RetrievedEvidence: cloneInteractionEvidence(data.retrievedEvidence),
		CitedEvidence:     cloneInteractionEvidence(data.citedEvidence),
	}
	for _, source := range data.sources {
		identity.Sources = append(identity.Sources, ResponseIdentitySource{
			ID: source.id, AnchorIDs: append(
				[]shoal.ID(nil), source.anchorIDs...),
			Visibility: append([]string(nil), source.visibility...),
		})
	}
	for _, evidence := range data.evidence {
		identity.Evidence = append(identity.Evidence, ResponseIdentityEvidence{
			AnchorID: evidence.anchor.ID(), Status: evidence.status,
			Use: evidence.use, Origin: evidence.origin,
			FromAddition: evidence.fromAddition,
			SourceIDs:    append([]shoal.ID(nil), evidence.sourceIDs...),
			SectionID:    evidence.sectionID,
			SpanID:       evidence.spanID,
			Visibility:   append([]string(nil), evidence.visibility...),
		})
	}
	for _, claim := range data.claims {
		identity.ClaimIDs = append(identity.ClaimIDs, claim.claim.ID())
	}
	for _, issue := range data.issues {
		identity.Issues = append(identity.Issues, ResponseIdentityIssue{
			Kind: issue.kind, OutcomeType: issue.outcomeType,
			OutcomeID: issue.outcomeID, Input: issue.input, Reason: issue.reason,
		})
	}
	return identity
}

// ResponseFingerprint derives the canonical verification fingerprint shared
// by immutable responses and strict transport adapters.
func ResponseFingerprint(identity ResponseIdentity) (string, error) {
	embeddingSpaces, err := identity.EmbeddingSpaces.Canonical()
	if err != nil {
		return "", fmt.Errorf("embedding spaces: %w", err)
	}
	retrievedEvidence, err := canonicalInteractionEvidence(
		identity.RetrievedEvidence)
	if err != nil {
		return "", fmt.Errorf("retrieved evidence: %w", err)
	}
	citedEvidence, err := canonicalInteractionEvidence(identity.CitedEvidence)
	if err != nil {
		return "", fmt.Errorf("cited evidence: %w", err)
	}
	retrievedSourceIDs := append(
		[]shoal.ID(nil), identity.RetrievedSourceIDs...)
	sort.Slice(retrievedSourceIDs, func(i, j int) bool {
		return shoal.CompareID(retrievedSourceIDs[i], retrievedSourceIDs[j]) < 0
	})
	citedSourceIDs := append([]shoal.ID(nil), identity.CitedSourceIDs...)
	sort.Slice(citedSourceIDs, func(i, j int) bool {
		return shoal.CompareID(citedSourceIDs[i], citedSourceIDs[j]) < 0
	})
	sources := append([]ResponseIdentitySource(nil), identity.Sources...)
	sort.Slice(sources, func(i, j int) bool {
		return shoal.CompareID(sources[i].ID, sources[j].ID) < 0
	})
	evidenceItems := append(
		[]ResponseIdentityEvidence(nil), identity.Evidence...)
	sort.Slice(evidenceItems, func(i, j int) bool {
		return shoal.CompareID(
			evidenceItems[i].AnchorID, evidenceItems[j].AnchorID) < 0
	})
	claimIDs := append([]shoal.ID(nil), identity.ClaimIDs...)
	sort.Slice(claimIDs, func(i, j int) bool {
		return shoal.CompareID(claimIDs[i], claimIDs[j]) < 0
	})
	issues := append([]ResponseIdentityIssue(nil), identity.Issues...)
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Kind != issues[j].Kind {
			return issues[i].Kind < issues[j].Kind
		}
		return shoal.CompareID(
			issues[i].OutcomeID, issues[j].OutcomeID) < 0
	})
	parts := []string{
		string(identity.ContextPackID),
		string(identity.ResultID),
		string(identity.PolicyID),
		string(identity.RequestID),
		string(identity.SnapshotID),
		identity.SnapshotAsOf.UTC().Format(time.RFC3339Nano),
		string(identity.AuthorizationFingerprint),
		identity.AuthorizationExpiresAt.UTC().Format(time.RFC3339Nano),
		embeddingSpaces.Digest,
		identity.GeneratedAt.UTC().Format(time.RFC3339Nano),
	}
	for _, embeddingSpace := range embeddingSpaces.Identities {
		parts = append(parts, "embedding-space", embeddingSpace)
	}
	for _, visibility := range identity.EffectiveVisibility {
		parts = append(parts, "visibility", visibility)
	}
	for _, id := range retrievedSourceIDs {
		parts = append(parts, "retrieved", string(id))
	}
	for _, id := range citedSourceIDs {
		parts = append(parts, "cited", string(id))
	}
	for _, source := range sources {
		parts = append(parts, "source", string(source.ID))
		anchorIDs := append([]shoal.ID(nil), source.AnchorIDs...)
		sort.Slice(anchorIDs, func(i, j int) bool {
			return shoal.CompareID(anchorIDs[i], anchorIDs[j]) < 0
		})
		for _, anchorID := range anchorIDs {
			parts = append(parts, "source-anchor", string(anchorID))
		}
		for _, visibility := range source.Visibility {
			parts = append(parts, "source-visibility", visibility)
		}
	}
	for _, reference := range retrievedEvidence {
		parts = append(parts, interactionEvidenceParts("retrieved-evidence", reference)...)
	}
	for _, reference := range citedEvidence {
		parts = append(parts, interactionEvidenceParts("cited-evidence", reference)...)
	}
	for _, evidence := range evidenceItems {
		parts = append(
			parts,
			"evidence",
			string(evidence.AnchorID),
			string(evidence.Status),
			string(evidence.Use),
			string(evidence.Origin),
			strconv.FormatBool(evidence.FromAddition),
			string(evidence.SectionID),
			string(evidence.SpanID),
		)
		sourceIDs := append([]shoal.ID(nil), evidence.SourceIDs...)
		sort.Slice(sourceIDs, func(i, j int) bool {
			return shoal.CompareID(sourceIDs[i], sourceIDs[j]) < 0
		})
		for _, sourceID := range sourceIDs {
			parts = append(parts, "evidence-source", string(sourceID))
		}
		for _, visibility := range evidence.Visibility {
			parts = append(parts, "evidence-visibility", visibility)
		}
	}
	for _, claimID := range claimIDs {
		parts = append(parts, "claim", string(claimID))
	}
	for _, issue := range issues {
		parts = append(
			parts, "issue", string(issue.Kind), string(issue.OutcomeType),
			string(issue.OutcomeID),
			issue.Input, issue.Reason)
	}
	return string(deriveID("reasoning-verification", parts...)), nil
}

// CanonicalResponseID derives the stable ID for a durably captured response.
func CanonicalResponseID(
	sessionID shoal.ID,
	recordedAt time.Time,
	identity ResponseIdentity,
) (shoal.ID, error) {
	if err := shoal.ValidateRequiredID(
		"reasoning response session ID", sessionID); err != nil {
		return "", err
	}
	if recordedAt.IsZero() {
		return "", invalid("reasoning response recording time is required")
	}
	fingerprint, err := ResponseFingerprint(identity)
	if err != nil {
		return "", err
	}
	return deriveID(
		"reasoning-response",
		string(sessionID),
		recordedAt.UTC().Format(time.RFC3339Nano),
		fingerprint,
	), nil
}

func deriveID(namespace string, parts ...string) shoal.ID {
	digest := sha256.New()
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(parts)))
	_, _ = digest.Write(length[:])
	for _, part := range parts {
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		_, _ = digest.Write(length[:])
		_, _ = digest.Write([]byte(part))
	}
	return shoal.ID(namespace + ":" + hex.EncodeToString(digest.Sum(nil)))
}

func canonicalIDs(input []shoal.ID) []shoal.ID {
	seen := make(map[shoal.ID]struct{}, len(input))
	for _, id := range input {
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

func containsID(values []shoal.ID, target shoal.ID) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func equalIDs(left, right []shoal.ID) bool {
	left = canonicalIDs(left)
	right = canonicalIDs(right)
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

func equalStrings(left, right []string) bool {
	left, leftErr := interaction.Conjoin(left)
	right, rightErr := interaction.Conjoin(right)
	if leftErr != nil || rightErr != nil || len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func cloneEvidence(input []Evidence) []Evidence {
	result := make([]Evidence, len(input))
	for index, evidence := range input {
		result[index] = evidence
		result[index].sourceIDs = append(
			[]shoal.ID(nil), evidence.sourceIDs...)
		result[index].visibility = append(
			[]string(nil), evidence.visibility...)
		result[index].reference, _ = evidence.reference.Canonical()
	}
	return result
}

func cloneSources(input []SourceReference) []SourceReference {
	result := make([]SourceReference, len(input))
	for index, source := range input {
		result[index] = SourceReference{
			id:         source.id,
			anchorIDs:  append([]shoal.ID(nil), source.anchorIDs...),
			visibility: append([]string(nil), source.visibility...),
		}
	}
	return result
}

func cloneClaims(input []Claim) []Claim {
	result := make([]Claim, len(input))
	for index, claim := range input {
		result[index] = Claim{
			claim:       claim.claim,
			citations:   cloneEvidence(claim.citations),
			derivations: cloneEvidence(claim.derivations),
		}
	}
	return result
}

func cloneIssues(input []Issue) []Issue {
	result := make([]Issue, len(input))
	for index, issue := range input {
		result[index] = Issue{
			kind:        issue.kind,
			outcomeType: issue.outcomeType,
			outcomeID:   issue.outcomeID,
			input:       issue.input,
			reason:      issue.reason,
			claim:       issue.claim,
			hasClaim:    issue.hasClaim,
			evidence:    cloneEvidence(issue.evidence),
		}
	}
	return result
}

func cloneResponseData(data responseData) responseData {
	data.embeddingSpaces = cloneEmbeddingSpaces(data.embeddingSpaces)
	data.effectiveOutputVisibility = append(
		[]string(nil), data.effectiveOutputVisibility...)
	data.retrievedSourceIDs = append(
		[]shoal.ID(nil), data.retrievedSourceIDs...)
	data.citedSourceIDs = append(
		[]shoal.ID(nil), data.citedSourceIDs...)
	data.seedSourceIDs = append(
		[]shoal.ID(nil), data.seedSourceIDs...)
	data.additionSourceIDs = append(
		[]shoal.ID(nil), data.additionSourceIDs...)
	data.retrievedEvidence = cloneInteractionEvidence(data.retrievedEvidence)
	data.citedEvidence = cloneInteractionEvidence(data.citedEvidence)
	data.seedEvidence = cloneInteractionEvidence(data.seedEvidence)
	data.additionEvidence = cloneInteractionEvidence(data.additionEvidence)
	data.sources = cloneSources(data.sources)
	data.evidence = cloneEvidence(data.evidence)
	data.claims = cloneClaims(data.claims)
	data.issues = cloneIssues(data.issues)
	return data
}

func cloneEmbeddingSpaces(
	value interaction.EmbeddingSpaceSet,
) interaction.EmbeddingSpaceSet {
	result, _ := value.Canonical()
	return result
}

func cloneInteractionEvidence(
	values []interaction.EvidenceReference,
) []interaction.EvidenceReference {
	result := make([]interaction.EvidenceReference, len(values))
	for index, value := range values {
		result[index] = value
		result[index].NodeIDs = append([]shoal.ID(nil), value.NodeIDs...)
		result[index].EdgeIDs = append([]shoal.ID(nil), value.EdgeIDs...)
		result[index].Assertions = append(
			[]interaction.AssertionReference(nil), value.Assertions...)
	}
	return result
}

func canonicalInteractionEvidence(
	values []interaction.EvidenceReference,
) ([]interaction.EvidenceReference, error) {
	result := make([]interaction.EvidenceReference, len(values))
	for index, value := range values {
		canonical, err := value.Canonical()
		if err != nil {
			return nil, err
		}
		result[index] = canonical
	}
	sort.Slice(result, func(i, j int) bool {
		return shoal.CompareID(
			result[i].AnchorID, result[j].AnchorID) < 0
	})
	return result, nil
}

func equalInteractionEvidence(
	left, right []interaction.EvidenceReference,
) bool {
	var err error
	left, err = canonicalInteractionEvidence(left)
	if err != nil {
		return false
	}
	right, err = canonicalInteractionEvidence(right)
	if err != nil {
		return false
	}
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !interactionEvidenceEqual(left[index], right[index]) {
			return false
		}
	}
	return true
}

func interactionEvidenceEqual(
	left, right interaction.EvidenceReference,
) bool {
	if left.AnchorID != right.AnchorID || left.Kind != right.Kind ||
		left.Citation != right.Citation ||
		!reflect.DeepEqual(left.NodeIDs, right.NodeIDs) ||
		!reflect.DeepEqual(left.EdgeIDs, right.EdgeIDs) ||
		len(left.Assertions) != len(right.Assertions) {
		return false
	}
	for index := range left.Assertions {
		if left.Assertions[index] != right.Assertions[index] {
			return false
		}
	}
	return true
}

func interactionEvidenceParts(
	prefix string,
	reference interaction.EvidenceReference,
) []string {
	canonical, _ := reference.Canonical()
	parts := []string{
		prefix,
		string(canonical.AnchorID),
		string(canonical.Kind),
		string(canonical.Citation.DocumentID),
		string(canonical.Citation.RevisionID),
		string(canonical.Citation.SectionID),
		string(canonical.Citation.SpanID),
		strconv.FormatInt(canonical.Citation.Range.Start.Offset, 10),
		strconv.FormatInt(int64(canonical.Citation.Range.Start.Page), 10),
		strconv.FormatInt(canonical.Citation.Range.End.Offset, 10),
		strconv.FormatInt(int64(canonical.Citation.Range.End.Page), 10),
	}
	for _, id := range canonical.NodeIDs {
		parts = append(parts, "node", string(id))
	}
	for _, id := range canonical.EdgeIDs {
		parts = append(parts, "edge", string(id))
	}
	for _, assertion := range canonical.Assertions {
		parts = append(
			parts,
			"assertion",
			string(assertion.AssertionID),
			string(assertion.EdgeID),
			string(assertion.Origin),
		)
	}
	return parts
}

func invalid(message string) error {
	return shoal.NewError(shoal.ErrorInvalidArgument, message)
}

func isNil(value any) bool {
	reflection := reflect.ValueOf(value)
	switch reflection.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return reflection.IsNil()
	default:
		return false
	}
}
