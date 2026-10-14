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

package explorer

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"reflect"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// MaxOntologyProposals bounds the total durable proposals in one corpus,
// including proposals that have reached a terminal state.
const MaxOntologyProposals uint32 = 256

// OntologyProposalStore is the optional durable proposal lifecycle held by a
// Shoal Explorer corpus. New proposals must be admitted atomically against
// MaxOntologyProposals; identical retries do not consume another slot.
type OntologyProposalStore interface {
	OntologyProposals(context.Context) ([]ontology.GovernedProposal, error)
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

// OntologyProposalMutationStateProvider supplies the narrow preflight view
// needed by proposal mutations without exposing governed proposal bodies.
type OntologyProposalMutationStateProvider interface {
	OntologyProposalMutationState(
		context.Context, ontology.OntologyVersion, shoal.ID,
	) (OntologyProposalMutationState, error)
}

// OntologyActiveStateProvider returns the global durable active tip without
// exposing proposal bodies or evidence.
type OntologyActiveStateProvider interface {
	OntologyActiveState(
		context.Context, ontology.OntologyVersion,
	) (ontology.OntologyVersion, error)
}

// PublishedOntologyCatalogProvider returns the bounded published ontology
// history rooted at a configured version without exposing proposal bodies.
type PublishedOntologyCatalogProvider interface {
	PublishedOntologyCatalog(
		context.Context, ontology.OntologyVersion,
	) (ontology.PublishedCatalog, error)
}

// OntologyProposalEvidenceProvider returns only the immutable evidence needed
// to authorize a requested proposal mutation.
type OntologyProposalEvidenceProvider interface {
	OntologyProposalEvidence(
		context.Context, shoal.ID,
	) ([]ontology.EvidenceRef, bool, error)
}

// OntologyEvidenceCitationResolver resolves exact immutable source bytes for
// evidence validation without exposing a complete document revision.
type OntologyEvidenceCitationResolver interface {
	ResolveOntologyEvidenceCitation(
		context.Context, document.Citation,
	) (string, error)
}

// OntologyProposalMutationState is the narrow preflight view needed by
// proposal mutations. It intentionally excludes proposal authors, rationale,
// metadata, evidence, and unrelated proposal bodies.
type OntologyProposalMutationState struct {
	active                ontology.OntologyVersion
	proposalBaseVersionID shoal.ID
	proposalFound         bool
	proposalHasBase       bool
}

// Active returns the currently published tip rooted at the configured
// ontology version supplied to OntologyProposalMutationState.
func (s OntologyProposalMutationState) Active() ontology.OntologyVersion {
	return s.active
}

// ProposalFound reports whether the requested proposal ID exists.
func (s OntologyProposalMutationState) ProposalFound() bool {
	return s.proposalFound
}

// ProposalBaseVersionID returns the requested proposal's immutable base.
func (s OntologyProposalMutationState) ProposalBaseVersionID() (shoal.ID, bool) {
	return s.proposalBaseVersionID, s.proposalHasBase
}

type persistedOntologyProposal struct {
	ProposalID      shoal.ID
	Schema          persistedOntologySchema
	BaseVersion     *persistedOntologyVersion
	ProposedVersion persistedOntologyVersion
	ProposedBy      string
	Rationale       string
	CreatedAt       time.Time
	Metadata        shoal.Metadata
	Morphisms       []persistedOntologyMorphism
	transitions     []persistedProposalTransition
}

type persistedOntologyMorphism struct {
	Kind             ontology.MorphismKind
	Sources          []shoal.ID
	Targets          []shoal.ID
	DiscriminatorKey string
	Discriminator    map[string]shoal.ID
	Evidence         []persistedEvidenceRef
	Rationale        string
	Metadata         shoal.Metadata
}

type persistedOntologySchema struct {
	Key         string
	Name        string
	Description string
	Metadata    shoal.Metadata
}

type persistedOntologyVersion struct {
	Version       string
	CreatedAt     time.Time
	Concepts      []persistedConceptDefinition
	Relationships []persistedRelationshipDefinition
	Properties    []persistedPropertyDefinition
	Metadata      shoal.Metadata
}

type persistedConceptDefinition struct {
	Key         string
	Name        string
	Description string
	Properties  []shoal.ID
	Metadata    shoal.Metadata
}

type persistedRelationshipDefinition struct {
	Key          string
	Name         string
	Description  string
	FromConcepts []shoal.ID
	ToConcepts   []shoal.ID
	Properties   []shoal.ID
	Directed     bool
	Metadata     shoal.Metadata
}

type persistedPropertyDefinition struct {
	Key         string
	Name        string
	Description string
	ValueType   ontology.ValueType
	Constraints []persistedOntologyConstraint
	Metadata    shoal.Metadata
}

type persistedOntologyConstraint struct {
	Kind          ontology.ConstraintKind
	Count         uint32
	HasCount      bool
	Value         persistedOntologyValue
	HasValue      bool
	Pattern       string
	HasPattern    bool
	AllowedValues []persistedOntologyValue
}

type persistedOntologyValue struct {
	Type      ontology.ValueType
	Text      string
	Integer   int64
	Number    float64
	Boolean   bool
	Timestamp time.Time
	Reference shoal.ID
}

type persistedProposalTransition struct {
	ProposalID shoal.ID
	Sequence   uint32
	From       ontology.ProposalState
	To         ontology.ProposalState
	Actor      string
	Note       string
	At         time.Time
}

// OntologyProposals lists persisted governed proposals in a deterministic
// newest-first order.
func (e *Explorer) OntologyProposals(
	ctx context.Context,
) ([]ontology.GovernedProposal, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if err := e.requireOpen(); err != nil {
		return nil, err
	}
	if err := e.requireCertainOntologyMutationLocked(); err != nil {
		return nil, err
	}
	proposals := make([]ontology.GovernedProposal, 0, len(e.ontologyProposals))
	for _, record := range e.ontologyProposals {
		proposal, err := record.proposal()
		if err != nil {
			return nil, shoal.WrapError(
				shoal.ErrorInternal, "stored ontology proposal is invalid", err)
		}
		proposals = append(proposals, proposal)
	}
	// This sort is load-bearing; TestOntologyProposalEndpointReturnsStableOrdering
	// pins newest-first order with an ID tiebreaker so map iteration cannot leak
	// nondeterminism to the browser.
	sort.Slice(proposals, func(left, right int) bool {
		if proposals[left].UpdatedAt().Equal(proposals[right].UpdatedAt()) {
			return shoal.CompareID(proposals[left].ID(), proposals[right].ID()) < 0
		}
		return proposals[left].UpdatedAt().After(proposals[right].UpdatedAt())
	})
	return proposals, nil
}

// OntologyProposalMutationState returns only the active ontology and the
// requested proposal's base identity. It supports least-privilege mutation
// preflight without exposing the governed proposal corpus.
func (e *Explorer) OntologyProposalMutationState(
	ctx context.Context,
	configured ontology.OntologyVersion,
	proposalID shoal.ID,
) (OntologyProposalMutationState, error) {
	if err := contextError(ctx); err != nil {
		return OntologyProposalMutationState{}, err
	}
	if err := configured.Validate(); err != nil {
		return OntologyProposalMutationState{}, err
	}
	if err := shoal.ValidateOptionalID("ontology proposal ID", proposalID); err != nil {
		return OntologyProposalMutationState{}, err
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if err := e.requireOpen(); err != nil {
		return OntologyProposalMutationState{}, err
	}
	if err := e.requireCertainOntologyMutationLocked(); err != nil {
		return OntologyProposalMutationState{}, err
	}
	if len(e.ontologyProposals) > int(MaxOntologyProposals) {
		return OntologyProposalMutationState{}, shoal.NewError(
			shoal.ErrorUnavailable, "ontology proposals exceed the corpus bound")
	}
	proposals := make([]ontology.GovernedProposal, 0, len(e.ontologyProposals))
	state := OntologyProposalMutationState{}
	for id, record := range e.ontologyProposals {
		proposal, err := record.proposal()
		if err != nil {
			return OntologyProposalMutationState{}, shoal.WrapError(
				shoal.ErrorInternal, "stored ontology proposal is invalid", err)
		}
		proposals = append(proposals, proposal)
		if proposalID != "" && id == proposalID {
			state.proposalFound = true
			state.proposalBaseVersionID, state.proposalHasBase =
				proposal.BaseVersionID()
		}
	}
	catalog, err := NewPublishedOntologyCatalog(configured, proposals)
	if err != nil {
		return OntologyProposalMutationState{}, err
	}
	state.active = catalog.Active()
	return state, nil
}

// OntologyActiveState returns the active tip rooted at configured without
// exposing the proposal corpus used to derive it.
func (e *Explorer) OntologyActiveState(
	ctx context.Context,
	configured ontology.OntologyVersion,
) (ontology.OntologyVersion, error) {
	state, err := e.OntologyProposalMutationState(ctx, configured, "")
	if err != nil {
		return ontology.OntologyVersion{}, err
	}
	return state.Active(), nil
}

// PublishedOntologyCatalog returns the bounded durable published history
// without exposing proposal authors, rationale, metadata, or evidence.
func (e *Explorer) PublishedOntologyCatalog(
	ctx context.Context,
	configured ontology.OntologyVersion,
) (ontology.PublishedCatalog, error) {
	if err := contextError(ctx); err != nil {
		return ontology.PublishedCatalog{}, err
	}
	if err := configured.Validate(); err != nil {
		return ontology.PublishedCatalog{}, err
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if err := e.requireOpen(); err != nil {
		return ontology.PublishedCatalog{}, err
	}
	if err := e.requireCertainOntologyMutationLocked(); err != nil {
		return ontology.PublishedCatalog{}, err
	}
	proposals := make([]ontology.GovernedProposal, 0, len(e.ontologyProposals))
	for _, record := range e.ontologyProposals {
		proposal, err := record.proposal()
		if err != nil {
			return ontology.PublishedCatalog{}, shoal.WrapError(
				shoal.ErrorInternal, "stored ontology proposal is invalid", err)
		}
		proposals = append(proposals, proposal)
	}
	return NewPublishedOntologyCatalog(configured, proposals)
}

// NewPublishedOntologyCatalog applies the corpus proposal bound before
// delegating all publication-chain semantics to ontology.NewPublishedCatalog.
func NewPublishedOntologyCatalog(
	configured ontology.OntologyVersion,
	proposals []ontology.GovernedProposal,
) (ontology.PublishedCatalog, error) {
	if len(proposals) > int(MaxOntologyProposals) {
		return ontology.PublishedCatalog{}, shoal.NewError(
			shoal.ErrorUnavailable, "ontology proposals exceed the corpus bound")
	}
	return ontology.NewPublishedCatalog(configured, proposals)
}

// OntologyProposalEvidence returns independent evidence values for one
// proposal without exposing unrelated governed proposal bodies.
func (e *Explorer) OntologyProposalEvidence(
	ctx context.Context,
	proposalID shoal.ID,
) ([]ontology.EvidenceRef, bool, error) {
	if err := contextError(ctx); err != nil {
		return nil, false, err
	}
	if err := shoal.ValidateRequiredID("ontology proposal ID", proposalID); err != nil {
		return nil, false, err
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if err := e.requireOpen(); err != nil {
		return nil, false, err
	}
	if err := e.requireCertainOntologyMutationLocked(); err != nil {
		return nil, false, err
	}
	record := e.ontologyProposals[proposalID]
	if record == nil {
		return nil, false, nil
	}
	proposal, err := record.proposal()
	if err != nil {
		return nil, false, shoal.WrapError(
			shoal.ErrorInternal, "stored ontology proposal is invalid", err)
	}
	var evidence []ontology.EvidenceRef
	for _, morphism := range proposal.Morphisms() {
		evidence = append(evidence, morphism.Evidence()...)
	}
	return evidence, true, nil
}

// ResolveOntologyEvidenceCitation validates and resolves an exact citation
// against the immutable stored source revision.
func (e *Explorer) ResolveOntologyEvidenceCitation(
	ctx context.Context,
	citation document.Citation,
) (string, error) {
	if err := contextError(ctx); err != nil {
		return "", err
	}
	if err := citation.Validate(); err != nil {
		return "", err
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if err := e.requireOpen(); err != nil {
		return "", err
	}
	record := e.documents[citation.DocumentID][citation.RevisionID]
	if record == nil {
		return "", shoal.NewError(
			shoal.ErrorNotFound, "cited document revision not found")
	}
	return document.ResolveCitationQuote(
		record.Source.Content,
		record.Document,
		record.Revision,
		record.Sections,
		record.Spans,
		citation,
	)
}

// CreateOntologyProposal durably records a new draft proposal. The lifecycle is
// immutable: later governance decisions are stored as separate transition rows.
func (e *Explorer) CreateOntologyProposal(
	ctx context.Context,
	proposal ontology.GovernedProposal,
	baseVersion ontology.OntologyVersion,
) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if err := proposal.Validate(); err != nil {
		return err
	}
	if proposal.State() != ontology.ProposalDraft || len(proposal.Transitions()) != 0 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "only draft proposals can be created")
	}
	record, err := persistOntologyProposal(proposal, baseVersion)
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.requireOpen(); err != nil {
		return err
	}
	if err := e.requireWritableLocked(); err != nil {
		return err
	}
	if err := e.requireCertainOntologyMutationLocked(); err != nil {
		return err
	}
	if existing := e.ontologyProposals[proposal.ID()]; existing != nil {
		if existing.sameBase(record) {
			return nil
		}
		return shoal.NewError(
			shoal.ErrorUnavailable, "ontology proposal ID collision")
	}
	if len(e.ontologyProposals) >= int(MaxOntologyProposals) {
		return shoal.NewError(
			shoal.ErrorUnavailable, "ontology proposals exceed the corpus bound")
	}
	if err := e.writeRecord(
		ontologyProposalRecordRow(record.ProposalID),
		embeddedRecordOntologyProposal,
		record,
	); err != nil {
		if IsIndeterminateCommit(err) {
			e.ontologyMutationIndeterminate = true
		}
		return err
	}
	copy := record
	e.ontologyProposals[record.ProposalID] = &copy
	return nil
}

// TransitionOntologyProposal appends exactly one governed transition by
// delegating the edge validation to ontology.GovernedProposal.
func (e *Explorer) TransitionOntologyProposal(
	ctx context.Context,
	proposalID shoal.ID,
	next ontology.ProposalState,
	actor, note string,
	at time.Time,
) (ontology.GovernedProposal, error) {
	return e.transitionOntologyProposal(ctx, proposalID, next, actor, note, at, nil)
}

// TransitionOntologyProposalWithLimits validates the prospective response
// under the same lock as the transition, without narrowing the domain API.
func (e *Explorer) TransitionOntologyProposalWithLimits(
	ctx context.Context,
	proposalID shoal.ID,
	next ontology.ProposalState,
	actor, note string,
	at time.Time,
	limits OntologyProjectionLimits,
) (ontology.GovernedProposal, error) {
	return e.transitionOntologyProposal(ctx, proposalID, next, actor, note, at, &limits)
}

func (e *Explorer) transitionOntologyProposal(
	ctx context.Context,
	proposalID shoal.ID,
	next ontology.ProposalState,
	actor, note string,
	at time.Time,
	limits *OntologyProjectionLimits,
) (ontology.GovernedProposal, error) {
	if err := contextError(ctx); err != nil {
		return ontology.GovernedProposal{}, err
	}
	if err := shoal.ValidateRequiredID("ontology proposal ID", proposalID); err != nil {
		return ontology.GovernedProposal{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.requireOpen(); err != nil {
		return ontology.GovernedProposal{}, err
	}
	if err := e.requireWritableLocked(); err != nil {
		return ontology.GovernedProposal{}, err
	}
	if err := e.requireCertainOntologyMutationLocked(); err != nil {
		return ontology.GovernedProposal{}, err
	}
	record := e.ontologyProposals[proposalID]
	if record == nil {
		return ontology.GovernedProposal{}, shoal.NewError(
			shoal.ErrorNotFound, "ontology proposal not found")
	}
	current, err := record.proposal()
	if err != nil {
		return ontology.GovernedProposal{}, shoal.WrapError(
			shoal.ErrorInternal, "stored ontology proposal is invalid", err)
	}
	if next == ontology.ProposalPublished {
		publishedVersions := make(map[shoal.ID]struct{})
		for otherID, otherRecord := range e.ontologyProposals {
			if otherID == proposalID {
				continue
			}
			other, restoreErr := otherRecord.proposal()
			if restoreErr != nil {
				return ontology.GovernedProposal{}, shoal.WrapError(
					shoal.ErrorInternal, "stored ontology proposal is invalid", restoreErr)
			}
			otherBase, otherHasBase := other.BaseVersionID()
			currentBase, currentHasBase := current.BaseVersionID()
			if other.State() != ontology.ProposalPublished ||
				other.Schema().ID() != current.Schema().ID() {
				continue
			}
			publishedVersions[other.ProposedVersion().ID()] = struct{}{}
			if otherHasBase {
				publishedVersions[otherBase] = struct{}{}
			}
			if otherHasBase == currentHasBase &&
				otherBase == currentBase {
				return ontology.GovernedProposal{}, shoal.NewError(
					shoal.ErrorConflict,
					"another proposal already advanced this ontology base version",
				)
			}
		}
		if _, cycle := publishedVersions[current.ProposedVersion().ID()]; cycle {
			return ontology.GovernedProposal{}, shoal.NewError(
				shoal.ErrorConflict,
				"ontology publication target already exists in published history",
			)
		}
	}
	// This advance is load-bearing; TestOntologyProposalTransitionsSurviveCoarseClockGranularity
	// pins that back-to-back transitions still record strictly increasing times
	// on platforms whose wall clock does not tick between two reads.
	at = advanceProposalTransitionTime(current, at)
	// This call is load-bearing; TestOntologyProposalEndpointRejectsIllegalTransition
	// pins that the web API drives ontology.GovernedProposal.Transition instead
	// of carrying a second state machine in the transport or storage layer.
	updated, err := current.Transition(next, actor, note, at)
	if err != nil {
		return ontology.GovernedProposal{}, err
	}
	if limits != nil {
		if err := limits.ValidateProposal(updated); err != nil {
			return ontology.GovernedProposal{}, err
		}
	}
	if len(record.transitions) == math.MaxUint32 {
		return ontology.GovernedProposal{}, shoal.NewError(
			shoal.ErrorUnavailable, "ontology proposal transition sequence is exhausted")
	}
	transition := updated.Transitions()[len(updated.Transitions())-1]
	persisted := persistProposalTransition(
		proposalID, uint32(len(record.transitions)+1), transition)
	// This append is load-bearing; TestOntologyProposalEndpointPreservesTransitionHistory
	// pins that each decision is added after the existing history instead of
	// replacing or rewriting prior transitions.
	nextTransitions := append(
		append([]persistedProposalTransition(nil), record.transitions...),
		persisted,
	)
	if err := e.writeRecord(
		ontologyProposalTransitionRecordRow(proposalID, persisted.Sequence),
		embeddedRecordProposalTransition,
		persisted,
	); err != nil {
		if IsIndeterminateCommit(err) {
			e.ontologyMutationIndeterminate = true
		}
		return ontology.GovernedProposal{}, err
	}
	record.transitions = nextTransitions
	return updated, nil
}

func (e *Explorer) requireCertainOntologyMutationLocked() error {
	if !e.ontologyMutationIndeterminate {
		return nil
	}
	return shoal.NewError(
		shoal.ErrorUnavailable,
		"ontology mutation outcome is indeterminate; reopen the corpus before retrying",
	)
}

// advanceProposalTransitionTime returns the earliest time that keeps a
// proposal's lifecycle strictly ordered. Callers derive at from the wall
// clock, whose granularity on some platforms is coarser than the interval
// between two consecutive transitions, so at can equal the preceding
// timestamp even though real time moved forward.
func advanceProposalTransitionTime(
	current ontology.GovernedProposal, at time.Time,
) time.Time {
	previous := current.CreatedAt()
	if transitions := current.Transitions(); len(transitions) > 0 {
		previous = transitions[len(transitions)-1].At()
	}
	if at.After(previous) {
		return at
	}
	return previous.Add(time.Nanosecond)
}

func (e *Explorer) loadOntologyProposalRecord(row, qualifier, encoded []byte) error {
	if !bytes.Equal(qualifier, []byte(recordCQV2)) {
		return nil
	}
	var record persistedOntologyProposal
	if err := decodeEmbeddedRecord(
		encoded, embeddedRecordOntologyProposal, &record,
	); err != nil {
		return shoal.WrapError(shoal.ErrorInternal, "decode ontology proposal", err)
	}
	if !bytes.Equal(row, ontologyProposalRecordRow(record.ProposalID)) {
		return shoal.NewError(
			shoal.ErrorInternal, "stored ontology proposal row is invalid")
	}
	if _, err := record.proposalBase(); err != nil {
		return shoal.WrapError(
			shoal.ErrorInternal, "stored ontology proposal base is invalid", err)
	}
	if existing := e.ontologyProposals[record.ProposalID]; existing != nil {
		record.transitions = append([]persistedProposalTransition(nil), existing.transitions...)
	}
	copy := record
	e.ontologyProposals[record.ProposalID] = &copy
	return nil
}

func (e *Explorer) loadOntologyProposalTransitionRecord(
	row, qualifier, encoded []byte,
) error {
	if !bytes.Equal(qualifier, []byte(recordCQV2)) {
		return nil
	}
	var transition persistedProposalTransition
	if err := decodeEmbeddedRecord(
		encoded, embeddedRecordProposalTransition, &transition,
	); err != nil {
		return shoal.WrapError(shoal.ErrorInternal, "decode ontology proposal transition", err)
	}
	if !bytes.Equal(
		row,
		ontologyProposalTransitionRecordRow(transition.ProposalID, transition.Sequence),
	) {
		return shoal.NewError(
			shoal.ErrorInternal, "stored ontology proposal transition row is invalid")
	}
	record := e.ontologyProposals[transition.ProposalID]
	if record == nil {
		record = &persistedOntologyProposal{ProposalID: transition.ProposalID}
		e.ontologyProposals[transition.ProposalID] = record
	}
	for _, existing := range record.transitions {
		if existing.Sequence == transition.Sequence {
			if reflect.DeepEqual(existing, transition) {
				return nil
			}
			return shoal.NewError(
				shoal.ErrorInternal, "stored ontology proposal transition was rewritten")
		}
	}
	record.transitions = append(record.transitions, transition)
	return nil
}

func (p *persistedOntologyProposal) proposal() (ontology.GovernedProposal, error) {
	if p == nil {
		return ontology.GovernedProposal{}, fmt.Errorf("proposal is missing")
	}
	proposal, err := p.proposalBase()
	if err != nil {
		return ontology.GovernedProposal{}, err
	}
	transitions := append([]persistedProposalTransition(nil), p.transitions...)
	sort.Slice(transitions, func(left, right int) bool {
		return transitions[left].Sequence < transitions[right].Sequence
	})
	for index, transition := range transitions {
		if transition.Sequence != uint32(index+1) {
			return ontology.GovernedProposal{}, fmt.Errorf(
				"proposal transition sequence %d is missing", index+1)
		}
		if transition.ProposalID != proposal.ID() {
			return ontology.GovernedProposal{}, fmt.Errorf(
				"proposal transition belongs to another proposal")
		}
		if transition.From != proposal.State() {
			return ontology.GovernedProposal{}, fmt.Errorf(
				"proposal transition does not continue lifecycle")
		}
		var err error
		proposal, err = proposal.Transition(
			transition.To, transition.Actor, transition.Note, transition.At)
		if err != nil {
			return ontology.GovernedProposal{}, err
		}
	}
	if proposal.ID() != p.ProposalID {
		return ontology.GovernedProposal{}, fmt.Errorf("proposal ID is not canonical")
	}
	return proposal, nil
}

func (p *persistedOntologyProposal) proposalBase() (ontology.GovernedProposal, error) {
	if p.ProposalID == "" {
		return ontology.GovernedProposal{}, fmt.Errorf("proposal ID is missing")
	}
	schema, err := restoreOntologySchema(p.Schema)
	if err != nil {
		return ontology.GovernedProposal{}, err
	}
	var base ontology.OntologyVersion
	if p.BaseVersion != nil {
		base, err = restoreOntologyVersion(schema, *p.BaseVersion)
		if err != nil {
			return ontology.GovernedProposal{}, err
		}
	}
	proposed, err := restoreOntologyVersion(schema, p.ProposedVersion)
	if err != nil {
		return ontology.GovernedProposal{}, err
	}
	morphisms := make([]ontology.OntologyMorphism, 0, len(p.Morphisms))
	for _, item := range p.Morphisms {
		morphism, err := restoreOntologyMorphism(item, base, proposed)
		if err != nil {
			return ontology.GovernedProposal{}, err
		}
		morphisms = append(morphisms, morphism)
	}
	proposal, err := ontology.NewGovernedProposalWithMorphisms(
		schema, base, proposed, morphisms,
		p.ProposedBy, p.Rationale, p.CreatedAt, p.Metadata)
	if err != nil {
		return ontology.GovernedProposal{}, err
	}
	if proposal.ID() != p.ProposalID {
		return ontology.GovernedProposal{}, fmt.Errorf("proposal ID is not canonical")
	}
	return proposal, nil
}

func (p *persistedOntologyProposal) sameBase(other persistedOntologyProposal) bool {
	left := *p
	left.transitions = nil
	other.transitions = nil
	return reflect.DeepEqual(left, other)
}

func persistOntologyProposal(
	proposal ontology.GovernedProposal,
	baseVersion ontology.OntologyVersion,
) (persistedOntologyProposal, error) {
	if err := proposal.Validate(); err != nil {
		return persistedOntologyProposal{}, err
	}
	var base *persistedOntologyVersion
	if baseID, ok := proposal.BaseVersionID(); ok {
		if err := baseVersion.Validate(); err != nil {
			return persistedOntologyProposal{}, err
		}
		if baseVersion.ID() != baseID || baseVersion.Schema().ID() != proposal.Schema().ID() {
			return persistedOntologyProposal{}, shoal.NewError(
				shoal.ErrorInvalidArgument, "proposal base version does not match")
		}
		persisted := persistOntologyVersion(baseVersion)
		base = &persisted
	}
	record := persistedOntologyProposal{
		ProposalID:      proposal.ID(),
		Schema:          persistOntologySchema(proposal.Schema()),
		BaseVersion:     base,
		ProposedVersion: persistOntologyVersion(proposal.ProposedVersion()),
		ProposedBy:      proposal.ProposedBy(),
		Rationale:       proposal.Rationale(),
		CreatedAt:       proposal.CreatedAt(),
		Metadata:        proposal.Metadata(),
	}
	for _, morphism := range proposal.Morphisms() {
		persisted, err := persistOntologyMorphism(morphism)
		if err != nil {
			return persistedOntologyProposal{}, err
		}
		record.Morphisms = append(record.Morphisms, persisted)
	}
	return record, nil
}

func persistOntologyMorphism(
	morphism ontology.OntologyMorphism,
) (persistedOntologyMorphism, error) {
	record := persistedOntologyMorphism{
		Kind: morphism.Kind(), Sources: morphism.Sources(), Targets: morphism.Targets(),
		DiscriminatorKey: morphism.Discriminator().MetadataKey(),
		Discriminator:    morphism.Discriminator().Choices(),
		Rationale:        morphism.Rationale(), Metadata: morphism.Metadata(),
	}
	for _, evidence := range morphism.Evidence() {
		if _, derived := evidence.Derivation(); derived {
			return persistedOntologyMorphism{}, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"ontology morphism persistence requires citation evidence",
			)
		}
		path, hasPath := evidence.Path()
		record.Evidence = append(record.Evidence, persistedEvidenceRef{
			Citation: evidence.Citation(), Quote: evidence.Quote(),
			Path: path, HasPath: hasPath, Metadata: evidence.Metadata(),
		})
	}
	return record, nil
}

func restoreOntologyMorphism(
	record persistedOntologyMorphism,
	source, target ontology.OntologyVersion,
) (ontology.OntologyMorphism, error) {
	evidence := make([]ontology.EvidenceRef, 0, len(record.Evidence))
	for _, item := range record.Evidence {
		var options []ontology.EvidenceOption
		if item.HasPath {
			options = append(options, ontology.WithEvidencePath(item.Path))
		}
		ref, err := ontology.NewEvidenceRef(
			item.Citation, item.Quote, item.Metadata, options...)
		if err != nil {
			return ontology.OntologyMorphism{}, err
		}
		evidence = append(evidence, ref)
	}
	var discriminator ontology.MorphismDiscriminator
	var err error
	if record.Kind == ontology.MorphismSplit {
		discriminator, err = ontology.NewMorphismDiscriminator(
			record.DiscriminatorKey, record.Discriminator)
		if err != nil {
			return ontology.OntologyMorphism{}, err
		}
	}
	return ontology.NewOntologyMorphism(ontology.MorphismConfig{
		Kind: record.Kind, SourceVersion: source, TargetVersion: target,
		Sources: record.Sources, Targets: record.Targets,
		Discriminator: discriminator, Evidence: evidence,
		Rationale: record.Rationale, Metadata: record.Metadata,
	})
}

func persistOntologySchema(schema ontology.OntologySchema) persistedOntologySchema {
	return persistedOntologySchema{
		Key:         schema.Key(),
		Name:        schema.Name(),
		Description: schema.Description(),
		Metadata:    schema.Metadata(),
	}
}

func persistOntologyVersion(version ontology.OntologyVersion) persistedOntologyVersion {
	concepts := version.Concepts()
	relationships := version.Relationships()
	properties := version.Properties()
	record := persistedOntologyVersion{
		Version:       version.Version(),
		CreatedAt:     version.CreatedAt(),
		Concepts:      make([]persistedConceptDefinition, 0, len(concepts)),
		Relationships: make([]persistedRelationshipDefinition, 0, len(relationships)),
		Properties:    make([]persistedPropertyDefinition, 0, len(properties)),
		Metadata:      version.Metadata(),
	}
	for _, concept := range concepts {
		record.Concepts = append(record.Concepts, persistedConceptDefinition{
			Key:         concept.Key(),
			Name:        concept.Name(),
			Description: concept.Description(),
			Properties:  concept.Properties(),
			Metadata:    concept.Metadata(),
		})
	}
	for _, relationship := range relationships {
		record.Relationships = append(record.Relationships, persistedRelationshipDefinition{
			Key:          relationship.Key(),
			Name:         relationship.Name(),
			Description:  relationship.Description(),
			FromConcepts: relationship.FromConcepts(),
			ToConcepts:   relationship.ToConcepts(),
			Properties:   relationship.Properties(),
			Directed:     relationship.Directed(),
			Metadata:     relationship.Metadata(),
		})
	}
	for _, property := range properties {
		constraints := property.Constraints()
		persisted := persistedPropertyDefinition{
			Key:         property.Key(),
			Name:        property.Name(),
			Description: property.Description(),
			ValueType:   property.ValueType(),
			Constraints: make([]persistedOntologyConstraint, 0, len(constraints)),
			Metadata:    property.Metadata(),
		}
		for _, constraint := range constraints {
			persisted.Constraints = append(
				persisted.Constraints, persistOntologyConstraint(constraint))
		}
		record.Properties = append(record.Properties, persisted)
	}
	return record
}

func persistOntologyConstraint(
	constraint ontology.Constraint,
) persistedOntologyConstraint {
	record := persistedOntologyConstraint{Kind: constraint.Kind()}
	if count, ok := constraint.Count(); ok {
		record.Count = count
		record.HasCount = true
	}
	if value, ok := constraint.Value(); ok {
		record.Value = persistOntologyValue(value)
		record.HasValue = true
	}
	if pattern, ok := constraint.Pattern(); ok {
		record.Pattern = pattern
		record.HasPattern = true
	}
	for _, value := range constraint.AllowedValues() {
		record.AllowedValues = append(record.AllowedValues, persistOntologyValue(value))
	}
	return record
}

func persistOntologyValue(value ontology.Value) persistedOntologyValue {
	record := persistedOntologyValue{Type: value.Type()}
	switch value.Type() {
	case ontology.ValueString:
		record.Text, _ = value.StringValue()
	case ontology.ValueInteger:
		record.Integer, _ = value.IntegerValue()
	case ontology.ValueNumber:
		record.Number, _ = value.NumberValue()
	case ontology.ValueBoolean:
		record.Boolean, _ = value.BooleanValue()
	case ontology.ValueTimestamp:
		record.Timestamp, _ = value.TimestampValue()
	case ontology.ValueReference:
		record.Reference, _ = value.ReferenceValue()
	}
	return record
}

func persistProposalTransition(
	proposalID shoal.ID,
	sequence uint32,
	transition ontology.ProposalTransition,
) persistedProposalTransition {
	return persistedProposalTransition{
		ProposalID: proposalID,
		Sequence:   sequence,
		From:       transition.From(),
		To:         transition.To(),
		Actor:      transition.Actor(),
		Note:       transition.Note(),
		At:         transition.At(),
	}
}

func restoreOntologySchema(
	record persistedOntologySchema,
) (ontology.OntologySchema, error) {
	return ontology.NewOntologySchema(
		record.Key, record.Name, record.Description, record.Metadata)
}

func restoreOntologyVersion(
	schema ontology.OntologySchema,
	record persistedOntologyVersion,
) (ontology.OntologyVersion, error) {
	properties := make([]ontology.PropertyDefinition, 0, len(record.Properties))
	for _, item := range record.Properties {
		constraints := make([]ontology.Constraint, 0, len(item.Constraints))
		for _, persisted := range item.Constraints {
			constraint, err := restoreOntologyConstraint(persisted)
			if err != nil {
				return ontology.OntologyVersion{}, err
			}
			constraints = append(constraints, constraint)
		}
		property, err := ontology.NewPropertyDefinition(
			item.Key, item.Name, item.Description, item.ValueType,
			constraints, item.Metadata)
		if err != nil {
			return ontology.OntologyVersion{}, err
		}
		properties = append(properties, property)
	}
	concepts := make([]ontology.ConceptDefinition, 0, len(record.Concepts))
	for _, item := range record.Concepts {
		concept, err := ontology.NewConceptDefinition(
			item.Key, item.Name, item.Description, item.Properties, item.Metadata)
		if err != nil {
			return ontology.OntologyVersion{}, err
		}
		concepts = append(concepts, concept)
	}
	relationships := make([]ontology.RelationshipDefinition, 0, len(record.Relationships))
	for _, item := range record.Relationships {
		relationship, err := ontology.NewRelationshipDefinition(
			item.Key, item.Name, item.Description, item.FromConcepts,
			item.ToConcepts, item.Properties, item.Directed, item.Metadata)
		if err != nil {
			return ontology.OntologyVersion{}, err
		}
		relationships = append(relationships, relationship)
	}
	return ontology.NewOntologyVersion(
		schema, record.Version, record.CreatedAt,
		concepts, relationships, properties, record.Metadata)
}

func restoreOntologyConstraint(
	record persistedOntologyConstraint,
) (ontology.Constraint, error) {
	switch record.Kind {
	case ontology.ConstraintRequired, ontology.ConstraintUnique:
		return ontology.NewFlagConstraint(record.Kind)
	case ontology.ConstraintMinimumCount, ontology.ConstraintMaximumCount:
		if !record.HasCount {
			return ontology.Constraint{}, fmt.Errorf("%s constraint count is missing", record.Kind)
		}
		return ontology.NewCountConstraint(record.Kind, record.Count)
	case ontology.ConstraintMinimumValue, ontology.ConstraintMaximumValue:
		if !record.HasValue {
			return ontology.Constraint{}, fmt.Errorf("%s constraint value is missing", record.Kind)
		}
		value, err := restoreOntologyValue(record.Value)
		if err != nil {
			return ontology.Constraint{}, err
		}
		return ontology.NewValueConstraint(record.Kind, value)
	case ontology.ConstraintPattern:
		if !record.HasPattern {
			return ontology.Constraint{}, fmt.Errorf("pattern constraint pattern is missing")
		}
		return ontology.NewPatternConstraint(record.Pattern)
	case ontology.ConstraintAllowedValues:
		values := make([]ontology.Value, 0, len(record.AllowedValues))
		for _, item := range record.AllowedValues {
			value, err := restoreOntologyValue(item)
			if err != nil {
				return ontology.Constraint{}, err
			}
			values = append(values, value)
		}
		return ontology.NewAllowedValuesConstraint(values)
	default:
		return ontology.Constraint{}, fmt.Errorf("unknown constraint kind %q", record.Kind)
	}
}

func restoreOntologyValue(record persistedOntologyValue) (ontology.Value, error) {
	switch record.Type {
	case ontology.ValueString:
		return ontology.NewStringValue(record.Text)
	case ontology.ValueInteger:
		return ontology.NewIntegerValue(record.Integer), nil
	case ontology.ValueNumber:
		return ontology.NewNumberValue(record.Number)
	case ontology.ValueBoolean:
		return ontology.NewBooleanValue(record.Boolean), nil
	case ontology.ValueTimestamp:
		return ontology.NewTimestampValue(record.Timestamp)
	case ontology.ValueReference:
		return ontology.NewReferenceValue(record.Reference)
	default:
		return ontology.Value{}, fmt.Errorf("unknown ontology value type %q", record.Type)
	}
}
