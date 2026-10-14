// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

package webapi

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	MaxOntologyProposals           uint32 = explorer.MaxOntologyProposals
	MaxOntologyProposalTransitions uint32 = 128
)

type OntologyProposalProvider interface {
	OntologyProposals(context.Context) ([]ontology.GovernedProposal, error)
	CreateOntologyProposal(
		context.Context, CreateOntologyProposalRequest,
	) (ontology.GovernedProposal, error)
	TransitionOntologyProposal(
		context.Context, shoal.ID, TransitionOntologyProposalRequest,
	) (ontology.GovernedProposal, error)
}

type ontologyProposalReadClient interface {
	OntologyProposals(context.Context) ([]ontology.GovernedProposal, error)
}

type ontologyProposalMutationClient interface {
	OntologyProposalMutationState(
		context.Context, ontology.OntologyVersion, shoal.ID,
	) (explorer.OntologyProposalMutationState, error)
	CreateOntologyProposal(
		context.Context, ontology.GovernedProposal, ontology.OntologyVersion,
	) error
	TransitionOntologyProposal(
		context.Context,
		shoal.ID,
		ontology.ProposalState,
		string,
		string,
		time.Time,
	) (ontology.GovernedProposal, error)
}

type OntologyProposalsResponse struct {
	Proposals []OntologyProposalProjection `json:"proposals"`
	Limits    OntologyProposalLimits       `json:"limits"`
}

type OntologyProposalResponse struct {
	Proposal OntologyProposalProjection `json:"proposal"`
}

type OntologyProposalProjection struct {
	ID               string                         `json:"id"`
	BaseSchemaID     string                         `json:"base_schema_id"`
	BaseVersionID    string                         `json:"base_version_id,omitempty"`
	ProposedBy       string                         `json:"proposed_by"`
	Rationale        string                         `json:"rationale"`
	CreatedAt        time.Time                      `json:"created_at"`
	UpdatedAt        time.Time                      `json:"updated_at"`
	State            string                         `json:"state"`
	ProposedOntology OntologyResponse               `json:"proposed_ontology"`
	Transitions      []ProposalTransitionProjection `json:"transitions"`
	Morphisms        []OntologyMorphismProjection   `json:"morphisms,omitempty"`
}

type ProposalTransitionProjection struct {
	From  string    `json:"from"`
	To    string    `json:"to"`
	Actor string    `json:"actor"`
	Note  string    `json:"note"`
	At    time.Time `json:"at"`
}

type OntologyProposalLimits struct {
	MaxProposals              uint32 `json:"max_proposals"`
	MaxTransitions            uint32 `json:"max_transitions"`
	MaxMorphisms              uint32 `json:"max_morphisms"`
	MaxMorphismEvidence       uint32 `json:"max_morphism_evidence"`
	MaxDiscriminatorChoices   uint32 `json:"max_discriminator_choices"`
	MaxConcepts               uint32 `json:"max_concepts"`
	MaxRelationships          uint32 `json:"max_relationships"`
	MaxProperties             uint32 `json:"max_properties"`
	MaxConstraintsPerProperty uint32 `json:"max_constraints_per_property"`
	MaxAllowedValues          uint32 `json:"max_allowed_values"`
}

type CreateOntologyProposalRequest struct {
	Rationale       string                       `json:"rationale"`
	ProposedVersion OntologyProposalVersionDraft `json:"proposed_version"`
	Morphisms       []OntologyMorphismDraft      `json:"morphisms,omitempty"`
}

type OntologyMorphismDraft struct {
	Kind          ontology.MorphismKind              `json:"kind"`
	Sources       []OntologyDefinitionReferenceDraft `json:"sources"`
	Targets       []OntologyDefinitionReferenceDraft `json:"targets"`
	Discriminator *OntologyDiscriminatorDraft        `json:"discriminator,omitempty"`
	Evidence      []OntologyMorphismEvidenceDraft    `json:"evidence"`
	Rationale     string                             `json:"rationale"`
	Metadata      shoal.Metadata                     `json:"metadata,omitempty"`
}

type OntologyDefinitionReferenceDraft struct {
	Namespace string `json:"namespace"`
	Key       string `json:"key"`
}

type OntologyDiscriminatorDraft struct {
	MetadataKey string                                      `json:"metadata_key"`
	Choices     map[string]OntologyDefinitionReferenceDraft `json:"choices"`
}

type OntologyMorphismEvidenceDraft struct {
	Citation document.Citation `json:"citation"`
	Quote    string            `json:"quote,omitempty"`
	Path     *graph.Path       `json:"path,omitempty"`
	Metadata shoal.Metadata    `json:"metadata,omitempty"`
}

type OntologyMorphismEvidenceProjection struct {
	ID       string            `json:"id"`
	Citation document.Citation `json:"citation"`
	Quote    string            `json:"quote,omitempty"`
	Path     *graph.Path       `json:"path,omitempty"`
	Metadata shoal.Metadata    `json:"metadata,omitempty"`
}

type OntologyMorphismProjection struct {
	ID            string                               `json:"id"`
	Kind          ontology.MorphismKind                `json:"kind"`
	Safety        ontology.MorphismSafety              `json:"safety"`
	SourceSchema  string                               `json:"source_schema_id"`
	SourceVersion string                               `json:"source_version_id"`
	TargetSchema  string                               `json:"target_schema_id"`
	TargetVersion string                               `json:"target_version_id"`
	Sources       []string                             `json:"sources"`
	Targets       []string                             `json:"targets"`
	Discriminator *OntologyDiscriminatorProjection     `json:"discriminator,omitempty"`
	EvidenceIDs   []string                             `json:"evidence_ids"`
	Evidence      []OntologyMorphismEvidenceProjection `json:"evidence"`
	Rationale     string                               `json:"rationale"`
	Metadata      shoal.Metadata                       `json:"metadata,omitempty"`
}

type OntologyDiscriminatorProjection struct {
	MetadataKey string            `json:"metadata_key"`
	Choices     map[string]string `json:"choices"`
}

func (d OntologyMorphismDraft) MarshalJSON() ([]byte, error) {
	type fields OntologyMorphismDraft
	return json.Marshal(struct {
		fields
		Metadata wireMetadata `json:"metadata,omitempty"`
	}{fields: fields(d), Metadata: wireMetadataValue(d.Metadata)})
}

func (d *OntologyMorphismDraft) UnmarshalJSON(data []byte) error {
	type fields OntologyMorphismDraft
	var wire struct {
		fields
		Metadata wireMetadata `json:"metadata,omitempty"`
	}
	if err := strictUnmarshal(data, &wire); err != nil {
		return err
	}
	metadata, err := metadataValue(wire.Metadata)
	if err != nil {
		return fmt.Errorf("metadata: %w", err)
	}
	decoded := OntologyMorphismDraft(wire.fields)
	decoded.Metadata = metadata
	*d = decoded
	return nil
}

func (p OntologyMorphismProjection) MarshalJSON() ([]byte, error) {
	type fields OntologyMorphismProjection
	return json.Marshal(struct {
		fields
		Metadata wireMetadata `json:"metadata,omitempty"`
	}{fields: fields(p), Metadata: wireMetadataValue(p.Metadata)})
}

func (p *OntologyMorphismProjection) UnmarshalJSON(data []byte) error {
	type fields OntologyMorphismProjection
	var wire struct {
		fields
		Metadata wireMetadata `json:"metadata,omitempty"`
	}
	if err := strictUnmarshal(data, &wire); err != nil {
		return err
	}
	metadata, err := metadataValue(wire.Metadata)
	if err != nil {
		return fmt.Errorf("metadata: %w", err)
	}
	decoded := OntologyMorphismProjection(wire.fields)
	decoded.Metadata = metadata
	*p = decoded
	return nil
}

func (d OntologyMorphismEvidenceDraft) MarshalJSON() ([]byte, error) {
	var path *wirePath
	if d.Path != nil {
		value := wirePathValue(*d.Path)
		path = &value
	}
	return json.Marshal(struct {
		Citation wireCitation `json:"citation"`
		Quote    string       `json:"quote,omitempty"`
		Path     *wirePath    `json:"path,omitempty"`
		Metadata wireMetadata `json:"metadata,omitempty"`
	}{
		Citation: wireCitationValue(d.Citation), Quote: d.Quote,
		Path: path, Metadata: wireMetadataValue(d.Metadata),
	})
}

func (d *OntologyMorphismEvidenceDraft) UnmarshalJSON(data []byte) error {
	var wire struct {
		Citation wireCitation `json:"citation"`
		Quote    string       `json:"quote,omitempty"`
		Path     *wirePath    `json:"path,omitempty"`
		Metadata wireMetadata `json:"metadata,omitempty"`
	}
	if err := strictUnmarshal(data, &wire); err != nil {
		return err
	}
	citation, err := citationValue(wire.Citation)
	if err != nil {
		return fmt.Errorf("citation: %w", err)
	}
	metadata, err := metadataValue(wire.Metadata)
	if err != nil {
		return fmt.Errorf("metadata: %w", err)
	}
	var path *graph.Path
	if wire.Path != nil {
		value, err := pathValue(*wire.Path)
		if err != nil {
			return fmt.Errorf("path: %w", err)
		}
		path = &value
	}
	*d = OntologyMorphismEvidenceDraft{
		Citation: citation, Quote: wire.Quote, Path: path, Metadata: metadata,
	}
	return nil
}

func (p OntologyMorphismEvidenceProjection) MarshalJSON() ([]byte, error) {
	var path *wirePath
	if p.Path != nil {
		value := wirePathValue(*p.Path)
		path = &value
	}
	return json.Marshal(struct {
		ID       string       `json:"id"`
		Citation wireCitation `json:"citation"`
		Quote    string       `json:"quote,omitempty"`
		Path     *wirePath    `json:"path,omitempty"`
		Metadata wireMetadata `json:"metadata,omitempty"`
	}{
		ID: p.ID, Citation: wireCitationValue(p.Citation), Quote: p.Quote,
		Path: path, Metadata: wireMetadataValue(p.Metadata),
	})
}

func (p *OntologyMorphismEvidenceProjection) UnmarshalJSON(data []byte) error {
	var wire struct {
		ID       string       `json:"id"`
		Citation wireCitation `json:"citation"`
		Quote    string       `json:"quote,omitempty"`
		Path     *wirePath    `json:"path,omitempty"`
		Metadata wireMetadata `json:"metadata,omitempty"`
	}
	if err := strictUnmarshal(data, &wire); err != nil {
		return err
	}
	id, err := decodeID(wire.ID)
	if err != nil {
		return fmt.Errorf("id: %w", err)
	}
	citation, err := citationValue(wire.Citation)
	if err != nil {
		return fmt.Errorf("citation: %w", err)
	}
	metadata, err := metadataValue(wire.Metadata)
	if err != nil {
		return fmt.Errorf("metadata: %w", err)
	}
	var (
		path    *graph.Path
		options []ontology.EvidenceOption
	)
	if wire.Path != nil {
		value, pathErr := pathValue(*wire.Path)
		if pathErr != nil {
			return fmt.Errorf("path: %w", pathErr)
		}
		path = &value
		options = append(options, ontology.WithEvidencePath(value))
	}
	evidence, err := ontology.NewEvidenceRef(
		citation, wire.Quote, metadata, options...)
	if err != nil {
		return err
	}
	if evidence.ID() != id {
		return fmt.Errorf("id does not match evidence content")
	}
	*p = OntologyMorphismEvidenceProjection{
		ID: wire.ID, Citation: citation, Quote: wire.Quote,
		Path: path, Metadata: metadata,
	}
	return nil
}

type TransitionOntologyProposalRequest struct {
	State string `json:"state"`
	Note  string `json:"note"`
}

type OntologyProposalVersionDraft struct {
	Version       string                              `json:"version"`
	Concepts      []OntologyProposalConceptDraft      `json:"concepts"`
	Relationships []OntologyProposalRelationshipDraft `json:"relationships"`
	Properties    []OntologyProposalPropertyDraft     `json:"properties"`
}

type OntologyProposalConceptDraft struct {
	Key         string   `json:"key"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Properties  []string `json:"properties,omitempty"`
}

type OntologyProposalRelationshipDraft struct {
	Key          string   `json:"key"`
	Name         string   `json:"name"`
	Description  string   `json:"description,omitempty"`
	FromConcepts []string `json:"from_concepts"`
	ToConcepts   []string `json:"to_concepts"`
	Properties   []string `json:"properties,omitempty"`
	Directed     bool     `json:"directed"`
}

type OntologyProposalPropertyDraft struct {
	Key         string                            `json:"key"`
	Name        string                            `json:"name"`
	Description string                            `json:"description,omitempty"`
	ValueType   ontology.ValueType                `json:"value_type"`
	Constraints []OntologyProposalConstraintDraft `json:"constraints,omitempty"`
}

type OntologyProposalConstraintDraft struct {
	Kind          ontology.ConstraintKind      `json:"kind"`
	Count         *uint32                      `json:"count,omitempty"`
	Value         *OntologyProposalValueDraft  `json:"value,omitempty"`
	Pattern       string                       `json:"pattern,omitempty"`
	AllowedValues []OntologyProposalValueDraft `json:"allowed_values,omitempty"`
}

type OntologyProposalValueDraft struct {
	Type  ontology.ValueType `json:"type"`
	Value json.RawMessage    `json:"value"`
}

func (s *EmbeddedService) OntologyProposals(
	ctx context.Context,
) ([]ontology.GovernedProposal, error) {
	store, ok := s.client.(ontologyProposalReadClient)
	if !ok {
		return nil, shoal.NewError(
			shoal.ErrorUnavailable, "workspace capability \"ontology proposals\" is unavailable")
	}
	return store.OntologyProposals(ctx)
}

func (s *EmbeddedService) CreateOntologyProposal(
	ctx context.Context,
	request CreateOntologyProposalRequest,
) (ontology.GovernedProposal, error) {
	store, ok := s.client.(ontologyProposalMutationClient)
	if !ok {
		return ontology.GovernedProposal{}, shoal.NewError(
			shoal.ErrorUnavailable, "workspace capability \"ontology proposals\" is unavailable")
	}
	state, configured, err := s.ontologyMutationSnapshot(ctx, store, "")
	if err != nil {
		return ontology.GovernedProposal{}, err
	}
	if !configured {
		return ontology.GovernedProposal{}, shoal.NewError(
			shoal.ErrorUnavailable, "an active ontology is required to propose a refinement")
	}
	base := state.Active()
	now := s.now()
	proposed, err := ontologyVersionFromProposalDraft(
		base.Schema(), request.ProposedVersion, now)
	if err != nil {
		return ontology.GovernedProposal{}, err
	}
	if err := enforceOntologyBounds(proposed); err != nil {
		return ontology.GovernedProposal{}, err
	}
	morphisms, err := ontologyMorphismsFromDraft(base, proposed, request.Morphisms)
	if err != nil {
		return ontology.GovernedProposal{}, err
	}
	proposal, err := ontology.NewGovernedProposalWithMorphisms(
		base.Schema(), base, proposed, morphisms,
		ontologyActor(ctx), request.Rationale, now, nil)
	if err != nil {
		return ontology.GovernedProposal{}, err
	}
	if err := store.CreateOntologyProposal(ctx, proposal, base); err != nil {
		return ontology.GovernedProposal{}, err
	}
	return proposal, nil
}

func ontologyMorphismsFromDraft(
	base, proposed ontology.OntologyVersion,
	drafts []OntologyMorphismDraft,
) ([]ontology.OntologyMorphism, error) {
	if len(drafts) > ontology.MaxProposalMorphisms {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "proposal morphisms exceed the public bound")
	}
	out := make([]ontology.OntologyMorphism, 0, len(drafts))
	for _, draft := range drafts {
		if len(draft.Sources) > int(MaxOntologyProperties) ||
			len(draft.Targets) > int(MaxOntologyProperties) {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument, "morphism definitions exceed the service bound")
		}
		if len(draft.Evidence) > int(MaxEvidencePerResult) {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument, "morphism evidence exceeds the service bound")
		}
		if draft.Discriminator != nil &&
			len(draft.Discriminator.Choices) > int(MaxOntologyConcepts) {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"morphism discriminator choices exceed the service bound",
			)
		}
		evidence := make([]ontology.EvidenceRef, 0, len(draft.Evidence))
		for _, item := range draft.Evidence {
			var options []ontology.EvidenceOption
			if item.Path != nil {
				options = append(options, ontology.WithEvidencePath(*item.Path))
			}
			ref, err := ontology.NewEvidenceRef(
				item.Citation, item.Quote, item.Metadata, options...)
			if err != nil {
				return nil, err
			}
			evidence = append(evidence, ref)
		}
		sources, err := resolveOntologyDefinitionReferences(base, draft.Sources)
		if err != nil {
			return nil, fmt.Errorf("morphism sources: %w", err)
		}
		targets, err := resolveOntologyDefinitionReferences(proposed, draft.Targets)
		if err != nil {
			return nil, fmt.Errorf("morphism targets: %w", err)
		}
		var discriminator ontology.MorphismDiscriminator
		if draft.Discriminator != nil {
			choices := make(map[string]shoal.ID, len(draft.Discriminator.Choices))
			for value, reference := range draft.Discriminator.Choices {
				resolved, resolveErr := resolveOntologyDefinitionReference(
					proposed, reference)
				if resolveErr != nil {
					return nil, fmt.Errorf("morphism discriminator: %w", resolveErr)
				}
				choices[value] = resolved
			}
			discriminator, err = ontology.NewMorphismDiscriminator(
				draft.Discriminator.MetadataKey, choices)
			if err != nil {
				return nil, err
			}
		}
		morphism, err := ontology.NewOntologyMorphism(ontology.MorphismConfig{
			Kind: draft.Kind, SourceVersion: base, TargetVersion: proposed,
			Sources: sources, Targets: targets,
			Discriminator: discriminator, Evidence: evidence,
			Rationale: draft.Rationale, Metadata: draft.Metadata,
		})
		if err != nil {
			return nil, err
		}
		out = append(out, morphism)
	}
	return out, nil
}

func resolveOntologyDefinitionReferences(
	version ontology.OntologyVersion,
	references []OntologyDefinitionReferenceDraft,
) ([]shoal.ID, error) {
	ids := make([]shoal.ID, 0, len(references))
	for _, reference := range references {
		id, err := resolveOntologyDefinitionReference(version, reference)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func resolveOntologyDefinitionReference(
	version ontology.OntologyVersion,
	reference OntologyDefinitionReferenceDraft,
) (shoal.ID, error) {
	namespace := strings.TrimSpace(reference.Namespace)
	key := strings.TrimSpace(reference.Key)
	if namespace == "" || key == "" ||
		namespace != reference.Namespace || key != reference.Key {
		return "", shoal.NewError(
			shoal.ErrorInvalidArgument,
			"ontology definition reference requires canonical namespace and key",
		)
	}
	switch namespace {
	case "concept":
		for _, definition := range version.Concepts() {
			if definition.Key() == key {
				return definition.ID(), nil
			}
		}
	case "relationship":
		for _, definition := range version.Relationships() {
			if definition.Key() == key {
				return definition.ID(), nil
			}
		}
	case "property":
		for _, definition := range version.Properties() {
			if definition.Key() == key {
				return definition.ID(), nil
			}
		}
	default:
		return "", shoal.NewError(
			shoal.ErrorInvalidArgument, "unknown ontology definition namespace")
	}
	return "", shoal.NewError(
		shoal.ErrorInvalidArgument, "ontology definition reference was not found")
}

func (s *EmbeddedService) TransitionOntologyProposal(
	ctx context.Context,
	proposalID shoal.ID,
	request TransitionOntologyProposalRequest,
) (ontology.GovernedProposal, error) {
	store, ok := s.client.(ontologyProposalMutationClient)
	if !ok {
		return ontology.GovernedProposal{}, shoal.NewError(
			shoal.ErrorUnavailable, "workspace capability \"ontology proposals\" is unavailable")
	}
	next := ontology.ProposalState(strings.TrimSpace(request.State))
	if next == ontology.ProposalPublished {
		s.ontologyPublishMu.Lock()
		defer s.ontologyPublishMu.Unlock()
		state, configured, snapshotErr := s.ontologyMutationSnapshot(
			ctx, store, proposalID)
		if snapshotErr != nil {
			return ontology.GovernedProposal{}, snapshotErr
		}
		if !state.ProposalFound() {
			return ontology.GovernedProposal{}, shoal.NewError(
				shoal.ErrorNotFound, "ontology proposal not found")
		}
		baseID, hasBase := state.ProposalBaseVersionID()
		if !configured || !hasBase || state.Active().ID() != baseID {
			return ontology.GovernedProposal{}, shoal.NewError(
				shoal.ErrorConflict,
				"ontology proposal base is not the active version",
			)
		}
	}
	bounded, ok := store.(explorer.OntologyProposalBoundedTransitionStore)
	if !ok {
		return ontology.GovernedProposal{}, shoal.NewError(
			shoal.ErrorUnavailable,
			"workspace capability \"bounded ontology proposal transitions\" is unavailable",
		)
	}
	proposal, err := bounded.TransitionOntologyProposalWithLimits(
		ctx, proposalID, next, ontologyActor(ctx), request.Note, s.now(),
		ontologyProjectionLimits())
	if err != nil {
		return ontology.GovernedProposal{}, err
	}
	return proposal, nil
}

func (s *EmbeddedService) ontologyMutationSnapshot(
	ctx context.Context,
	store ontologyProposalMutationClient,
	proposalID shoal.ID,
) (
	explorer.OntologyProposalMutationState,
	bool,
	error,
) {
	s.ontologyMu.RLock()
	var configured ontology.OntologyVersion
	present := s.ontologyVersion != nil
	if present {
		configured = *s.ontologyVersion
	}
	s.ontologyMu.RUnlock()
	if !present {
		return explorer.OntologyProposalMutationState{}, false, nil
	}
	state, err := store.OntologyProposalMutationState(
		ctx, configured, proposalID)
	if err != nil {
		return explorer.OntologyProposalMutationState{}, false, err
	}
	return state, true, nil
}

func (s *EmbeddedService) activeOntologyForMutation(
	ctx context.Context,
) (ontology.OntologyVersion, bool, error) {
	s.ontologyMu.RLock()
	if s.ontologyVersion == nil {
		s.ontologyMu.RUnlock()
		return ontology.OntologyVersion{}, false, nil
	}
	configured := *s.ontologyVersion
	s.ontologyMu.RUnlock()
	provider, ok := s.client.(explorer.OntologyProposalMutationStateProvider)
	if !ok {
		return configured, true, nil
	}
	state, err := provider.OntologyProposalMutationState(ctx, configured, "")
	if err != nil {
		return ontology.OntologyVersion{}, false, err
	}
	return state.Active(), true, nil
}

func ontologyProposalsFor(
	ctx context.Context,
	service Service,
) (OntologyProposalsResponse, error) {
	provider, ok := service.(OntologyProposalProvider)
	if !ok {
		return OntologyProposalsResponse{
			Proposals: []OntologyProposalProjection{},
			Limits:    ontologyProposalLimits(),
		}, nil
	}
	proposals, err := provider.OntologyProposals(ctx)
	if err != nil {
		return OntologyProposalsResponse{}, err
	}
	return projectOntologyProposals(proposals)
}

func createOntologyProposalFor(
	ctx context.Context,
	service Service,
	request CreateOntologyProposalRequest,
) (OntologyProposalResponse, error) {
	provider, ok := service.(OntologyProposalProvider)
	if !ok {
		return OntologyProposalResponse{}, shoal.NewError(
			shoal.ErrorUnavailable, "workspace capability \"ontology proposals\" is unavailable")
	}
	proposal, err := provider.CreateOntologyProposal(ctx, request)
	if err != nil {
		return OntologyProposalResponse{}, err
	}
	projected, err := projectOntologyProposalForMutation(proposal)
	if err != nil {
		return OntologyProposalResponse{}, explorer.MarkIndeterminateCommit(err)
	}
	return OntologyProposalResponse{Proposal: projected}, nil
}

func transitionOntologyProposalFor(
	ctx context.Context,
	service Service,
	proposalID shoal.ID,
	request TransitionOntologyProposalRequest,
) (OntologyProposalResponse, error) {
	provider, ok := service.(OntologyProposalProvider)
	if !ok {
		return OntologyProposalResponse{}, shoal.NewError(
			shoal.ErrorUnavailable, "workspace capability \"ontology proposals\" is unavailable")
	}
	proposal, err := provider.TransitionOntologyProposal(ctx, proposalID, request)
	if err != nil {
		return OntologyProposalResponse{}, err
	}
	projected, err := projectOntologyProposalForMutation(proposal)
	if err != nil {
		return OntologyProposalResponse{}, explorer.MarkIndeterminateCommit(err)
	}
	return OntologyProposalResponse{Proposal: projected}, nil
}

func projectOntologyProposals(
	proposals []ontology.GovernedProposal,
) (OntologyProposalsResponse, error) {
	if uint32(len(proposals)) > MaxOntologyProposals {
		return OntologyProposalsResponse{}, ontologyBoundError(
			"proposal", len(proposals), MaxOntologyProposals)
	}
	ordered := append([]ontology.GovernedProposal(nil), proposals...)
	// This sort is load-bearing; TestOntologyProposalEndpointReturnsStableOrdering
	// pins deterministic newest-first JSON even when a provider returns an
	// unordered slice.
	sort.Slice(ordered, func(left, right int) bool {
		if ordered[left].UpdatedAt().Equal(ordered[right].UpdatedAt()) {
			return shoal.CompareID(ordered[left].ID(), ordered[right].ID()) < 0
		}
		return ordered[left].UpdatedAt().After(ordered[right].UpdatedAt())
	})
	response := OntologyProposalsResponse{
		Proposals: []OntologyProposalProjection{},
		Limits:    ontologyProposalLimits(),
	}
	for _, proposal := range ordered {
		projected, err := projectOntologyProposal(proposal)
		if err != nil {
			return OntologyProposalsResponse{}, err
		}
		response.Proposals = append(response.Proposals, projected)
	}
	return response, nil
}

func projectOntologyProposal(
	proposal ontology.GovernedProposal,
) (OntologyProposalProjection, error) {
	return projectOntologyProposalWithEvidence(proposal, true)
}

func projectOntologyProposalForMutation(
	proposal ontology.GovernedProposal,
) (OntologyProposalProjection, error) {
	return projectOntologyProposalWithEvidence(proposal, false)
}

func projectOntologyProposalWithEvidence(
	proposal ontology.GovernedProposal,
	includeEvidence bool,
) (OntologyProposalProjection, error) {
	if err := proposal.Validate(); err != nil {
		return OntologyProposalProjection{}, err
	}
	if err := ontologyProjectionLimits().ValidateProposal(proposal); err != nil {
		return OntologyProposalProjection{}, err
	}
	transitions := proposal.Transitions()
	proposed, err := projectOntology(proposal.ProposedVersion())
	if err != nil {
		return OntologyProposalProjection{}, err
	}
	baseVersionID, _ := proposal.BaseVersionID()
	projected := OntologyProposalProjection{
		ID:               encodeID(proposal.ID()),
		BaseSchemaID:     encodeID(proposal.Schema().ID()),
		BaseVersionID:    encodeOptionalID(baseVersionID),
		ProposedBy:       proposal.ProposedBy(),
		Rationale:        proposal.Rationale(),
		CreatedAt:        proposal.CreatedAt(),
		UpdatedAt:        proposal.UpdatedAt(),
		State:            string(proposal.State()),
		ProposedOntology: proposed,
		Transitions:      []ProposalTransitionProjection{},
	}
	for _, transition := range transitions {
		projected.Transitions = append(projected.Transitions, ProposalTransitionProjection{
			From:  string(transition.From()),
			To:    string(transition.To()),
			Actor: transition.Actor(),
			Note:  transition.Note(),
			At:    transition.At(),
		})
	}
	for _, morphism := range proposal.Morphisms() {
		evidence := morphism.Evidence()
		evidenceIDs := make([]string, len(evidence))
		evidenceProjected := make([]OntologyMorphismEvidenceProjection, 0)
		for index, item := range evidence {
			evidenceIDs[index] = encodeID(item.ID())
			if !includeEvidence {
				continue
			}
			path, hasPath := item.Path()
			var projectedPath *graph.Path
			if hasPath {
				projectedPath = &path
			}
			evidenceProjected = append(
				evidenceProjected, OntologyMorphismEvidenceProjection{
					ID: encodeID(item.ID()), Citation: item.Citation(),
					Quote: item.Quote(), Path: projectedPath,
					Metadata: item.Metadata(),
				})
		}
		var discriminator *OntologyDiscriminatorProjection
		if morphism.Kind() == ontology.MorphismSplit {
			value := morphism.Discriminator()
			choices := value.Choices()
			projectedChoices := make(map[string]string, len(choices))
			for choice, id := range choices {
				projectedChoices[choice] = encodeID(id)
			}
			discriminator = &OntologyDiscriminatorProjection{
				MetadataKey: value.MetadataKey(), Choices: projectedChoices,
			}
		}
		projected.Morphisms = append(projected.Morphisms, OntologyMorphismProjection{
			ID: encodeID(morphism.ID()), Kind: morphism.Kind(), Safety: morphism.Safety(),
			SourceSchema:  encodeID(morphism.Source().SchemaID()),
			SourceVersion: encodeID(morphism.Source().VersionID()),
			TargetSchema:  encodeID(morphism.Target().SchemaID()),
			TargetVersion: encodeID(morphism.Target().VersionID()),
			Sources:       encodeOntologyIDs(morphism.Sources()),
			Targets:       encodeOntologyIDs(morphism.Targets()),
			Discriminator: discriminator, EvidenceIDs: evidenceIDs,
			Evidence:  evidenceProjected,
			Rationale: morphism.Rationale(), Metadata: morphism.Metadata(),
		})
	}
	return projected, nil
}

func ontologyProposalLimits() OntologyProposalLimits {
	return OntologyProposalLimits{
		MaxProposals:              MaxOntologyProposals,
		MaxTransitions:            MaxOntologyProposalTransitions,
		MaxMorphisms:              ontology.MaxProposalMorphisms,
		MaxMorphismEvidence:       MaxEvidencePerResult,
		MaxDiscriminatorChoices:   MaxOntologyConcepts,
		MaxConcepts:               MaxOntologyConcepts,
		MaxRelationships:          MaxOntologyRelationships,
		MaxProperties:             MaxOntologyProperties,
		MaxConstraintsPerProperty: MaxOntologyConstraintsPerProperty,
		MaxAllowedValues:          MaxOntologyAllowedValues,
	}
}

func ontologyVersionFromProposalDraft(
	schema ontology.OntologySchema,
	draft OntologyProposalVersionDraft,
	createdAt time.Time,
) (ontology.OntologyVersion, error) {
	properties, propertyIDs, err := proposalPropertiesFromDraft(draft.Properties)
	if err != nil {
		return ontology.OntologyVersion{}, err
	}
	concepts, conceptIDs, err := proposalConceptsFromDraft(
		draft.Concepts, propertyIDs)
	if err != nil {
		return ontology.OntologyVersion{}, err
	}
	relationships, err := proposalRelationshipsFromDraft(
		draft.Relationships, conceptIDs, propertyIDs)
	if err != nil {
		return ontology.OntologyVersion{}, err
	}
	return ontology.NewOntologyVersion(
		schema, draft.Version, createdAt, concepts, relationships, properties, nil)
}

func proposalPropertiesFromDraft(
	drafts []OntologyProposalPropertyDraft,
) ([]ontology.PropertyDefinition, map[string]shoal.ID, error) {
	properties := make([]ontology.PropertyDefinition, 0, len(drafts))
	ids := make(map[string]shoal.ID, len(drafts))
	for _, draft := range drafts {
		constraints, err := proposalConstraintsFromDraft(draft.Constraints)
		if err != nil {
			return nil, nil, fmt.Errorf("ontology property %q constraints: %w", draft.Key, err)
		}
		property, err := ontology.NewPropertyDefinition(
			draft.Key, draft.Name, draft.Description, draft.ValueType, constraints, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("ontology property %q: %w", draft.Key, err)
		}
		if _, duplicate := ids[draft.Key]; duplicate {
			return nil, nil, fmt.Errorf("ontology property key %q is duplicated", draft.Key)
		}
		ids[draft.Key] = property.ID()
		properties = append(properties, property)
	}
	return properties, ids, nil
}

func proposalConceptsFromDraft(
	drafts []OntologyProposalConceptDraft,
	propertyIDs map[string]shoal.ID,
) ([]ontology.ConceptDefinition, map[string]shoal.ID, error) {
	concepts := make([]ontology.ConceptDefinition, 0, len(drafts))
	ids := make(map[string]shoal.ID, len(drafts))
	for _, draft := range drafts {
		properties, err := resolveProposalDefinitionIDs(
			"property", draft.Properties, propertyIDs)
		if err != nil {
			return nil, nil, fmt.Errorf("ontology concept %q: %w", draft.Key, err)
		}
		concept, err := ontology.NewConceptDefinition(
			draft.Key, draft.Name, draft.Description, properties, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("ontology concept %q: %w", draft.Key, err)
		}
		if _, duplicate := ids[draft.Key]; duplicate {
			return nil, nil, fmt.Errorf("ontology concept key %q is duplicated", draft.Key)
		}
		ids[draft.Key] = concept.ID()
		concepts = append(concepts, concept)
	}
	return concepts, ids, nil
}

func proposalRelationshipsFromDraft(
	drafts []OntologyProposalRelationshipDraft,
	conceptIDs map[string]shoal.ID,
	propertyIDs map[string]shoal.ID,
) ([]ontology.RelationshipDefinition, error) {
	relationships := make([]ontology.RelationshipDefinition, 0, len(drafts))
	seen := make(map[string]struct{}, len(drafts))
	for _, draft := range drafts {
		from, err := resolveProposalDefinitionIDs(
			"concept", draft.FromConcepts, conceptIDs)
		if err != nil {
			return nil, fmt.Errorf("ontology relationship %q sources: %w", draft.Key, err)
		}
		to, err := resolveProposalDefinitionIDs(
			"concept", draft.ToConcepts, conceptIDs)
		if err != nil {
			return nil, fmt.Errorf("ontology relationship %q targets: %w", draft.Key, err)
		}
		properties, err := resolveProposalDefinitionIDs(
			"property", draft.Properties, propertyIDs)
		if err != nil {
			return nil, fmt.Errorf("ontology relationship %q properties: %w", draft.Key, err)
		}
		relationship, err := ontology.NewRelationshipDefinition(
			draft.Key, draft.Name, draft.Description, from, to,
			properties, draft.Directed, nil)
		if err != nil {
			return nil, fmt.Errorf("ontology relationship %q: %w", draft.Key, err)
		}
		if _, duplicate := seen[draft.Key]; duplicate {
			return nil, fmt.Errorf("ontology relationship key %q is duplicated", draft.Key)
		}
		seen[draft.Key] = struct{}{}
		relationships = append(relationships, relationship)
	}
	return relationships, nil
}

func proposalConstraintsFromDraft(
	drafts []OntologyProposalConstraintDraft,
) ([]ontology.Constraint, error) {
	constraints := make([]ontology.Constraint, 0, len(drafts))
	for _, draft := range drafts {
		constraint, err := proposalConstraintFromDraft(draft)
		if err != nil {
			return nil, err
		}
		constraints = append(constraints, constraint)
	}
	return constraints, nil
}

func proposalConstraintFromDraft(
	draft OntologyProposalConstraintDraft,
) (ontology.Constraint, error) {
	switch draft.Kind {
	case ontology.ConstraintRequired, ontology.ConstraintUnique:
		return ontology.NewFlagConstraint(draft.Kind)
	case ontology.ConstraintMinimumCount, ontology.ConstraintMaximumCount:
		if draft.Count == nil {
			return ontology.Constraint{}, fmt.Errorf("%s constraint requires count", draft.Kind)
		}
		return ontology.NewCountConstraint(draft.Kind, *draft.Count)
	case ontology.ConstraintMinimumValue, ontology.ConstraintMaximumValue:
		if draft.Value == nil {
			return ontology.Constraint{}, fmt.Errorf("%s constraint requires value", draft.Kind)
		}
		value, err := proposalValueFromDraft(*draft.Value)
		if err != nil {
			return ontology.Constraint{}, err
		}
		return ontology.NewValueConstraint(draft.Kind, value)
	case ontology.ConstraintPattern:
		return ontology.NewPatternConstraint(draft.Pattern)
	case ontology.ConstraintAllowedValues:
		values := make([]ontology.Value, 0, len(draft.AllowedValues))
		for _, item := range draft.AllowedValues {
			value, err := proposalValueFromDraft(item)
			if err != nil {
				return ontology.Constraint{}, err
			}
			values = append(values, value)
		}
		return ontology.NewAllowedValuesConstraint(values)
	default:
		return ontology.Constraint{}, fmt.Errorf("unknown constraint kind %q", draft.Kind)
	}
}

func proposalValueFromDraft(draft OntologyProposalValueDraft) (ontology.Value, error) {
	switch draft.Type {
	case ontology.ValueString:
		var value string
		if err := json.Unmarshal(draft.Value, &value); err != nil {
			return ontology.Value{}, fmt.Errorf("string ontology value: %w", err)
		}
		return ontology.NewStringValue(value)
	case ontology.ValueInteger:
		integer, err := jsonNumberInt64(draft.Value)
		if err != nil {
			return ontology.Value{}, fmt.Errorf("integer ontology value: %w", err)
		}
		return ontology.NewIntegerValue(integer), nil
	case ontology.ValueNumber:
		number, err := jsonNumberFloat64(draft.Value)
		if err != nil {
			return ontology.Value{}, fmt.Errorf("number ontology value: %w", err)
		}
		return ontology.NewNumberValue(number)
	case ontology.ValueBoolean:
		var value bool
		if err := json.Unmarshal(draft.Value, &value); err != nil {
			return ontology.Value{}, fmt.Errorf("boolean ontology value: %w", err)
		}
		return ontology.NewBooleanValue(value), nil
	case ontology.ValueTimestamp:
		var value time.Time
		if err := json.Unmarshal(draft.Value, &value); err != nil {
			return ontology.Value{}, fmt.Errorf("timestamp ontology value: %w", err)
		}
		return ontology.NewTimestampValue(value)
	case ontology.ValueReference:
		var value string
		if err := json.Unmarshal(draft.Value, &value); err != nil {
			return ontology.Value{}, fmt.Errorf("reference ontology value: %w", err)
		}
		return ontology.NewReferenceValue(shoal.ID(value))
	default:
		return ontology.Value{}, fmt.Errorf("unknown ontology value type %q", draft.Type)
	}
}

func jsonNumberInt64(raw json.RawMessage) (int64, error) {
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		var text string
		if textErr := json.Unmarshal(raw, &text); textErr != nil {
			return 0, err
		}
		number = json.Number(text)
	}
	return number.Int64()
}

func jsonNumberFloat64(raw json.RawMessage) (float64, error) {
	var number json.Number
	if err := json.Unmarshal(raw, &number); err != nil {
		var text string
		if textErr := json.Unmarshal(raw, &text); textErr != nil {
			return 0, err
		}
		number = json.Number(text)
	}
	return number.Float64()
}

func resolveProposalDefinitionIDs(
	kind string,
	keys []string,
	ids map[string]shoal.ID,
) ([]shoal.ID, error) {
	resolved := make([]shoal.ID, 0, len(keys))
	for _, key := range keys {
		id, ok := ids[key]
		if !ok {
			return nil, fmt.Errorf("unknown %s key %q", kind, key)
		}
		resolved = append(resolved, id)
	}
	return resolved, nil
}

func ontologyActor(ctx context.Context) string {
	if identity, ok := identityFromContext(ctx); ok {
		if strings.TrimSpace(identity.Subject) != "" {
			return identity.Subject
		}
		if strings.TrimSpace(identity.Actor) != "" {
			return identity.Actor
		}
	}
	return "anonymous"
}

func (s *EmbeddedService) now() time.Time {
	if s.clock != nil {
		return s.clock().UTC()
	}
	return time.Now().UTC()
}
