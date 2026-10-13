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

package authorized

import (
	"context"
	"encoding/binary"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/extraction"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// RevisionRegistration is the immutable policy-catalog record for one exact
// revision. Current controls an atomic current-node/current-edge projection;
// it is not part of immutable content equality.
type RevisionRegistration struct {
	DocumentID     shoal.ID
	RevisionID     shoal.ID
	NodeIDs        []shoal.ID
	IntrinsicEdges []graph.Edge
	ContentDigest  auth.Digest
	Rule           AccessRule
	Current        bool
}

// RegistrationKind records which writer produced a node or edge
// registration. Authorization depends on it: an extracted registration is
// bound to the document revision that asserted it and also needs that
// document's current rule (#570), so the kind is stored, never inferred from
// which fields happen to be empty. The zero value is valid only on input to
// ReserveEdge/PutEdge (where it means RegistrationApplication) and on records
// persisted before the field existed (see legacyNodeKind, legacyEdgeKind).
type RegistrationKind uint8

const (
	RegistrationUnspecified RegistrationKind = iota
	// RegistrationDocument is an intrinsic node or edge of a document's
	// current revision, written by PutRevision.
	RegistrationDocument
	// RegistrationExtracted is ExtractDocument output. DocumentID and
	// RevisionID name the revision that asserted it.
	RegistrationExtracted
	// RegistrationMaterialized is MaterializeGraph output, owned by a
	// materialization rather than a document.
	RegistrationMaterialized
	// RegistrationApplication is an application edge written by Connect.
	RegistrationApplication
)

// NodeRegistration identifies the current revision and rule owning a node.
type NodeRegistration struct {
	DocumentID shoal.ID
	RevisionID shoal.ID
	Node       graph.Node
	Rule       AccessRule
	Kind       RegistrationKind
}

// EdgeRegistration owns an edge and only its edge-local rule. Endpoint rules
// are deliberately not flattened into this record and must be read from the
// current node catalog on every authorization. DocumentID and RevisionID are
// set for revision-intrinsic edges and for extracted edges (the asserting
// revision), and empty for application and materialized edges.
//
// SourceRevisions and SourceRevisionsBound bind an application edge to the
// document revisions its endpoints belonged to when it was connected (#695);
// see revision_binding.go. SourceRevisionsBound distinguishes an edge
// connected with that binding (possibly empty, when neither endpoint belongs
// to a document) from one registered before it existed.
type EdgeRegistration struct {
	Edge                 graph.Edge
	DocumentID           shoal.ID
	RevisionID           shoal.ID
	Rule                 AccessRule
	Kind                 RegistrationKind
	SourceRevisions      []RevisionReference
	SourceRevisionsBound bool
}

// MutationLease serializes base mutations shared by clients using one store.
type MutationLease interface {
	Release()
}

// SourcePolicyClaim reserves one source URI under a policy rule. Pending
// claims represent an unresolved mutation and require authorization under both
// Rule and PreviousRule when the latter is present. Version is an opaque
// compare-and-swap generation owned by the PolicyStore.
type SourcePolicyClaim struct {
	SourceURI     string
	Rule          AccessRule
	PreviousRule  *AccessRule
	Pending       bool
	Version       uint64
	hadPriorClaim bool
	priorPending  bool
	priorVersion  uint64
	acquisitionID uint64
	tokenSeal     auth.Digest
}

// PolicyStore is the non-durable M2 policy-catalog boundary. Implementations
// must make each put and source-claim transition atomic and return independent
// values from reads. A successful source-claim CAS exclusively owns that URI
// until it is committed, pended, or rolled back. For the exact token returned
// by a successful CAS, each finalizer must be local, atomic, idempotent for the
// same outcome, and infallible; errors indicate invalid tokens or store
// invariant violations. That token is a capability: SourceClaim must return
// only observable claim state and never a value a finalizer would accept, so
// that reading a claim cannot finalize one. Fallible durable coordination
// belongs to M3. A write that reports an error must not be observable by any
// later read (DurablePolicyStore fails closed rather than serve a change it
// could not persist): authorization decides on these reads, and a
// registration or claim that did not persist must never stand in for the
// one it replaced (#665).
type PolicyStore interface {
	AcquireMutation(context.Context) (MutationLease, error)
	SourceClaim(context.Context, string) (SourcePolicyClaim, bool, error)
	CompareAndSwapSourceClaim(
		context.Context, string, *SourcePolicyClaim, AccessRule,
	) (SourcePolicyClaim, error)
	CommitSourceClaim(context.Context, SourcePolicyClaim) error
	PendSourceClaim(context.Context, SourcePolicyClaim) error
	RollbackSourceClaim(context.Context, SourcePolicyClaim) error
	PutRevision(context.Context, RevisionRegistration) error
	PutNode(context.Context, shoal.ID, NodeRegistration) error
	Revision(context.Context, shoal.ID, shoal.ID) (RevisionRegistration, bool, error)
	CurrentRevision(context.Context, shoal.ID) (RevisionRegistration, bool, error)
	// CurrentRevisions resolves the current revision registrations for many
	// documents in one round trip under exactly the contract Nodes carries for
	// node registrations.
	CurrentRevisions(
		context.Context, []shoal.ID,
	) (map[shoal.ID]RevisionRegistration, error)
	Node(context.Context, shoal.ID) (NodeRegistration, bool, error)
	// Nodes resolves many node registrations in one round trip. For every
	// identifier it is given it must report exactly what Node would report for
	// that identifier, and it must fail with the error the equivalent Node loop
	// fails with, in the same request order: an earlier identifier's failure
	// takes precedence over a later one's. Presence is reported by map
	// membership exactly as Node reports it by its boolean: an identifier that
	// is absent from the returned map is unregistered, which callers must treat
	// as the fail-closed deny path. Implementations must never report an
	// identifier that was not requested.
	//
	// Implementations may resolve a repeated identifier once, and may check the
	// context and their own availability for an empty request where a Node loop
	// would make no call at all. Callers must not depend on either.
	Nodes(context.Context, []shoal.ID) (map[shoal.ID]NodeRegistration, error)
	ReserveEdge(context.Context, EdgeRegistration) error
	RollbackEdgeReservation(context.Context, EdgeRegistration) error
	PutEdge(context.Context, EdgeRegistration) error
	Edge(context.Context, shoal.ID) (EdgeRegistration, bool, error)
	// Edges resolves many edge registrations in one round trip under exactly
	// the contract Nodes carries for node registrations.
	Edges(context.Context, []shoal.ID) (map[shoal.ID]EdgeRegistration, error)
	// TighteningIndex builds, in one pass, the per-document index a label
	// migration run passes to every TightenRule call (#570).
	TighteningIndex(context.Context) (*TighteningIndex, error)
	// TightenRule narrows a document's catalog rule in place, atomically per
	// call (#570). It accepts only a To whose components are a strict
	// superset of From's and never widens anything; see
	// MemoryPolicyStore.TightenRule for exactly what it rewrites. It reports
	// whether anything changed, so a repeated call is a no-op.
	TightenRule(context.Context, *TighteningIndex, RuleTightening) (bool, error)
	// LabelMigration returns the label-migration marker recorded by
	// PutLabelMigration, if any.
	LabelMigration(context.Context) (LabelMigrationRecord, bool, error)
	// PutLabelMigration records the label-migration report. It is written
	// after every run, once every document has been migrated.
	PutLabelMigration(context.Context, LabelMigrationRecord) error
	// RecordDeclassification durably appends one relabel-narrowing audit
	// record (#630) before returning nil. Ingest refuses to proceed past
	// the step a record guards until it is recorded, so an implementation
	// must not report success for a record it has not made durable.
	RecordDeclassification(context.Context, DeclassificationRecord) error
	// Declassifications returns every recorded declassification audit, in
	// recording order. It is an operator read that no Client method exposes.
	// Each record is written before the step it guards, so it records an
	// attempt; whether the narrowing took effect is the catalog's revision
	// registration (a pre_commit record's RevisionID being current).
	Declassifications(context.Context) ([]DeclassificationRecord, error)
	// DocumentRevisionRules returns, for each requested document that has
	// any registered revision, the rule of every one of its revisions and
	// which of them is current (#695). Absence is the fail-closed signal, as
	// for Nodes; unrequested documents are never reported.
	DocumentRevisionRules(
		context.Context, []shoal.ID,
	) (map[shoal.ID][]RevisionRule, error)
	// RevisionRules returns, for each document the references name, the
	// rules of the referenced revisions that are registered and of the
	// current revision, under DocumentRevisionRules's contract. It is what a
	// bound output needs without a document's whole history.
	RevisionRules(
		context.Context, []RevisionReference,
	) (map[shoal.ID][]RevisionRule, error)
	// PutInteractionRevisions durably records, before returning nil, that
	// the interaction session was derived from the given document revisions
	// (#695). A repeated call unions with what is recorded and never removes
	// a revision, so a recording can only narrow who reads the session.
	PutInteractionRevisions(
		context.Context, shoal.ID, []RevisionReference,
	) error
	// InteractionRevisions returns the revisions recorded for each requested
	// session. A session absent from the result has no recording: it was
	// recorded before #695, or touched no document.
	InteractionRevisions(
		context.Context, []shoal.ID,
	) (map[shoal.ID][]RevisionReference, error)
}

type revisionKey struct {
	documentID shoal.ID
	revisionID shoal.ID
}

// sourceClaimState separates the observable claim from the sealed
// finalization capability. token is populated only while held and is never
// returned by a read, so observing a claim never confers the ability to
// finalize it.
type sourceClaimState struct {
	claim SourcePolicyClaim
	token SourcePolicyClaim
	held  bool
}

// MemoryPolicyStore is a concurrency-safe reference catalog. Reusing the same
// instance across wrapped-client restarts preserves registrations, but process
// exit loses them; it is not durable recovery storage.
type MemoryPolicyStore struct {
	mutationMu     sync.Mutex
	mu             sync.RWMutex
	sourceClaims   map[string]sourceClaimState
	sourceVersion  uint64
	revisions      map[revisionKey]RevisionRegistration
	revisionIDs    map[shoal.ID]revisionKey
	current        map[shoal.ID]revisionKey
	nodes          map[shoal.ID]NodeRegistration
	intrinsicEdges map[shoal.ID]EdgeRegistration
	edgeClaims     map[shoal.ID]EdgeRegistration
	edges          map[shoal.ID]EdgeRegistration
	coOccurrence   map[string]CoOccurrenceRecord
	labelMigration *LabelMigrationRecord
	// declassifications is append-only (#630).
	declassifications []DeclassificationRecord
	// documentRevisions indexes every registered revision ID by document,
	// (#695).
	documentRevisions map[shoal.ID][]shoal.ID
	// interactionRevisions is what PutInteractionRevisions recorded, in
	// canonical order (#695).
	interactionRevisions map[shoal.ID][]RevisionReference
}

// NewMemoryPolicyStore constructs an empty reference catalog.
func NewMemoryPolicyStore() *MemoryPolicyStore {
	return &MemoryPolicyStore{
		sourceClaims:   make(map[string]sourceClaimState),
		revisions:      make(map[revisionKey]RevisionRegistration),
		revisionIDs:    make(map[shoal.ID]revisionKey),
		current:        make(map[shoal.ID]revisionKey),
		nodes:          make(map[shoal.ID]NodeRegistration),
		intrinsicEdges: make(map[shoal.ID]EdgeRegistration),
		edgeClaims:     make(map[shoal.ID]EdgeRegistration),
		edges:          make(map[shoal.ID]EdgeRegistration),
		coOccurrence:   make(map[string]CoOccurrenceRecord),

		documentRevisions:    make(map[shoal.ID][]shoal.ID),
		interactionRevisions: make(map[shoal.ID][]RevisionReference),
	}
}

type memoryMutationLease struct {
	store *MemoryPolicyStore
	once  sync.Once
}

func (l *memoryMutationLease) Release() {
	if l == nil || l.store == nil {
		return
	}
	l.once.Do(l.store.mutationMu.Unlock)
}

// AcquireMutation serializes base mutations across all wrappers sharing this
// policy store so authorization checks remain pinned through the base write.
func (s *MemoryPolicyStore) AcquireMutation(
	ctx context.Context,
) (MutationLease, error) {
	if err := contextFailure(ctx); err != nil {
		return nil, err
	}
	if s == nil {
		return nil, catalogUnavailable()
	}
	s.mutationMu.Lock()
	if err := contextFailure(ctx); err != nil {
		s.mutationMu.Unlock()
		return nil, err
	}
	return &memoryMutationLease{store: s}, nil
}

// SourceClaim returns the current source-URI policy claim without its
// finalization capability, so an observed claim can never be committed,
// pended, or rolled back by a caller that does not hold the CAS token.
func (s *MemoryPolicyStore) SourceClaim(
	ctx context.Context,
	sourceURI string,
) (SourcePolicyClaim, bool, error) {
	if err := contextFailure(ctx); err != nil {
		return SourcePolicyClaim{}, false, err
	}
	if err := validateSourceURI(sourceURI); err != nil {
		return SourcePolicyClaim{}, false, err
	}
	if s == nil {
		return SourcePolicyClaim{}, false, catalogUnavailable()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	state, ok := s.sourceClaims[sourceURI]
	if !ok {
		return SourcePolicyClaim{}, false, nil
	}
	cloned, err := cloneSourcePolicyClaim(state.claim)
	if err != nil {
		return SourcePolicyClaim{}, false, catalogUnavailable()
	}
	return cloned, true, nil
}

// CompareAndSwapSourceClaim atomically acquires exclusive mutation ownership.
// A committed claim may transition to a desired rule. A pending claim may only
// reacquire ownership for its existing desired rule. A nil expected claim
// requires the URI to be unclaimed.
func (s *MemoryPolicyStore) CompareAndSwapSourceClaim(
	ctx context.Context,
	sourceURI string,
	expected *SourcePolicyClaim,
	desired AccessRule,
) (SourcePolicyClaim, error) {
	if err := contextFailure(ctx); err != nil {
		return SourcePolicyClaim{}, err
	}
	if err := validateSourceURI(sourceURI); err != nil {
		return SourcePolicyClaim{}, err
	}
	rule, err := desired.clone()
	if err != nil {
		return SourcePolicyClaim{}, err
	}
	var normalizedExpected *SourcePolicyClaim
	if expected != nil {
		cloned, cloneErr := cloneSourcePolicyClaim(*expected)
		if cloneErr != nil {
			return SourcePolicyClaim{}, cloneErr
		}
		if cloned.SourceURI != sourceURI || cloned.Version == 0 {
			return SourcePolicyClaim{}, shoal.NewError(
				shoal.ErrorInvalidArgument, "source policy claim is invalid")
		}
		normalizedExpected = &cloned
	}
	if s == nil {
		return SourcePolicyClaim{}, catalogUnavailable()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.initialize()
	current, exists := s.sourceClaims[sourceURI]
	if current.held {
		return SourcePolicyClaim{}, catalogConflict()
	}
	if normalizedExpected == nil {
		if exists {
			return SourcePolicyClaim{}, catalogConflict()
		}
	} else if !exists ||
		!sourcePolicyClaimsEqual(current.claim, *normalizedExpected) {
		return SourcePolicyClaim{}, catalogConflict()
	}
	if exists && current.claim.Pending &&
		!current.claim.Rule.equal(rule) {
		return SourcePolicyClaim{}, catalogConflict()
	}
	if s.sourceVersion == ^uint64(0) {
		return SourcePolicyClaim{}, catalogUnavailable()
	}
	s.sourceVersion++
	claim := SourcePolicyClaim{
		SourceURI:     sourceURI,
		Rule:          rule,
		Pending:       true,
		Version:       s.sourceVersion,
		hadPriorClaim: exists,
		acquisitionID: s.sourceVersion,
	}
	state := sourceClaimState{held: true}
	if exists {
		claim.priorPending = current.claim.Pending
		claim.priorVersion = current.claim.Version
		if current.claim.Pending {
			if current.claim.PreviousRule != nil {
				previousRule, ruleErr := current.claim.PreviousRule.clone()
				if ruleErr != nil {
					return SourcePolicyClaim{}, catalogUnavailable()
				}
				claim.PreviousRule = &previousRule
			}
		} else {
			previousRule, ruleErr := current.claim.Rule.clone()
			if ruleErr != nil {
				return SourcePolicyClaim{}, catalogUnavailable()
			}
			claim.PreviousRule = &previousRule
		}
	}
	claim.tokenSeal = sourceClaimTokenSeal(claim)
	state.token = claim
	state.claim = sourceClaimObservable(claim)
	s.sourceClaims[sourceURI] = state
	return cloneSourcePolicyClaim(claim)
}

// CommitSourceClaim releases exclusive mutation ownership, makes the desired
// rule authoritative, and clears pending recovery state.
func (s *MemoryPolicyStore) CommitSourceClaim(
	ctx context.Context,
	claim SourcePolicyClaim,
) error {
	return s.finishSourceClaim(ctx, claim, sourceClaimFinishCommit)
}

// PendSourceClaim releases exclusive mutation ownership while preserving an
// unresolved desired/previous rule pair for an authorized recovery attempt.
func (s *MemoryPolicyStore) PendSourceClaim(
	ctx context.Context,
	claim SourcePolicyClaim,
) error {
	return s.finishSourceClaim(ctx, claim, sourceClaimFinishPending)
}

// RollbackSourceClaim releases exclusive mutation ownership and restores the
// claim that preceded the CAS, deleting a newly-created claim.
func (s *MemoryPolicyStore) RollbackSourceClaim(
	ctx context.Context,
	claim SourcePolicyClaim,
) error {
	return s.finishSourceClaim(ctx, claim, sourceClaimFinishRollback)
}

type sourceClaimFinish uint8

const (
	sourceClaimFinishCommit sourceClaimFinish = iota
	sourceClaimFinishPending
	sourceClaimFinishRollback
)

func (s *MemoryPolicyStore) finishSourceClaim(
	ctx context.Context,
	claim SourcePolicyClaim,
	finish sourceClaimFinish,
) error {
	if err := contextFailure(ctx); err != nil {
		return err
	}
	normalized, err := cloneSourcePolicyClaim(claim)
	if err != nil {
		return err
	}
	if err := validateSourceURI(normalized.SourceURI); err != nil {
		return err
	}
	if normalized.Version == 0 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "source policy claim is invalid")
	}
	if normalized.acquisitionID == 0 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "source policy claim token is invalid")
	}
	if normalized.tokenSeal == (auth.Digest{}) ||
		normalized.tokenSeal != sourceClaimTokenSeal(normalized) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "source policy claim token is invalid")
	}
	if s == nil {
		return catalogUnavailable()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.sourceClaims[normalized.SourceURI]
	if !ok || !state.held ||
		!sourcePolicyClaimsEqual(state.token, normalized) {
		completed, completionErr := sourceClaimCompletion(
			normalized, finish)
		if completionErr != nil {
			return completionErr
		}
		if completed == nil {
			if !ok {
				return nil
			}
		} else if ok && !state.held &&
			sourcePolicyClaimsEqual(state.claim, *completed) {
			return nil
		}
		return catalogConflict()
	}
	switch finish {
	case sourceClaimFinishRollback:
		previous, previousErr := sourceClaimCompletion(
			normalized, sourceClaimFinishRollback)
		if previousErr != nil {
			return previousErr
		}
		if previous == nil {
			delete(s.sourceClaims, normalized.SourceURI)
			return nil
		}
		s.sourceClaims[normalized.SourceURI] = sourceClaimState{
			claim: *previous,
		}
		return nil
	case sourceClaimFinishPending:
		state.held = false
		state.token = SourcePolicyClaim{}
		completed, completionErr := sourceClaimCompletion(
			normalized, sourceClaimFinishPending)
		if completionErr != nil || completed == nil {
			return catalogUnavailable()
		}
		state.claim = *completed
		s.sourceClaims[normalized.SourceURI] = state
		return nil
	case sourceClaimFinishCommit:
		state.held = false
		state.token = SourcePolicyClaim{}
		completed, completionErr := sourceClaimCompletion(
			normalized, sourceClaimFinishCommit)
		if completionErr != nil || completed == nil {
			return catalogUnavailable()
		}
		state.claim = *completed
		s.sourceClaims[normalized.SourceURI] = state
		return nil
	default:
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "source claim finish is invalid")
	}
}

func sourceClaimCompletion(
	token SourcePolicyClaim,
	finish sourceClaimFinish,
) (*SourcePolicyClaim, error) {
	stable := SourcePolicyClaim{
		SourceURI: token.SourceURI,
		Version:   token.Version,
	}
	switch finish {
	case sourceClaimFinishCommit:
		rule, err := token.Rule.clone()
		if err != nil {
			return nil, err
		}
		stable.Rule = rule
		return &stable, nil
	case sourceClaimFinishPending:
		rule, err := token.Rule.clone()
		if err != nil {
			return nil, err
		}
		stable.Rule = rule
		stable.Pending = true
		if token.PreviousRule != nil {
			previous, previousErr := token.PreviousRule.clone()
			if previousErr != nil {
				return nil, previousErr
			}
			stable.PreviousRule = &previous
		}
		return &stable, nil
	case sourceClaimFinishRollback:
		if !token.hadPriorClaim {
			return nil, nil
		}
		stable.Version = token.priorVersion
		stable.Pending = token.priorPending
		if token.priorPending {
			rule, err := token.Rule.clone()
			if err != nil {
				return nil, err
			}
			stable.Rule = rule
			if token.PreviousRule != nil {
				previous, previousErr := token.PreviousRule.clone()
				if previousErr != nil {
					return nil, previousErr
				}
				stable.PreviousRule = &previous
			}
			return &stable, nil
		}
		if token.PreviousRule == nil {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument, "source policy claim is invalid")
		}
		rule, err := token.PreviousRule.clone()
		if err != nil {
			return nil, err
		}
		stable.Rule = rule
		return &stable, nil
	default:
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "source claim finish is invalid")
	}
}

// PutRevision atomically registers immutable revision content and, when
// Current is true, replaces that document's current node/edge projection.
func (s *MemoryPolicyStore) PutRevision(
	ctx context.Context,
	registration RevisionRegistration,
) error {
	if err := contextFailure(ctx); err != nil {
		return err
	}
	normalized, err := normalizeRevisionRegistration(registration)
	if err != nil {
		return err
	}
	if s == nil {
		return catalogUnavailable()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.initialize()
	key := revisionKey{
		documentID: normalized.DocumentID,
		revisionID: normalized.RevisionID,
	}
	if owner, ok := s.revisionIDs[normalized.RevisionID]; ok && owner != key {
		return catalogConflict()
	}
	if existing, ok := s.revisions[key]; ok {
		if !revisionContentEqual(existing, normalized) {
			return catalogConflict()
		}
		if !normalized.Current {
			normalized.IntrinsicEdges = cloneRevisionRegistration(existing).IntrinsicEdges
		} else if len(existing.IntrinsicEdges) > 0 &&
			!intrinsicEdgesEqual(existing.IntrinsicEdges, normalized.IntrinsicEdges) {
			return catalogConflict()
		}
	}
	if normalized.Current {
		for _, nodeID := range normalized.NodeIDs {
			if owner, ok := s.nodes[nodeID]; ok &&
				owner.DocumentID != normalized.DocumentID {
				return catalogConflict()
			}
		}
		for _, edge := range normalized.IntrinsicEdges {
			if _, custom := s.edges[edge.ID]; custom {
				return catalogConflict()
			}
			if owner, ok := s.intrinsicEdges[edge.ID]; ok &&
				owner.DocumentID != normalized.DocumentID {
				return catalogConflict()
			}
		}
	}

	stored := cloneRevisionRegistration(normalized)
	stored.Current = false
	if _, known := s.revisions[key]; !known {
		s.documentRevisions[key.documentID] = append(
			s.documentRevisions[key.documentID], key.revisionID)
	}
	s.revisions[key] = stored
	s.revisionIDs[normalized.RevisionID] = key
	if normalized.Current {
		s.replaceCurrent(key, normalized)
	}
	return nil
}

// PutNode registers an extracted graph node under the source revision rule
// that authorized its publication. The node registration is idempotent for the
// same owner and rule, but it never widens or rewrites a previously registered
// node.
func (s *MemoryPolicyStore) PutNode(
	ctx context.Context,
	nodeID shoal.ID,
	registration NodeRegistration,
) error {
	if err := contextFailure(ctx); err != nil {
		return err
	}
	normalized, err := normalizeNodeRegistration(nodeID, registration)
	if err != nil {
		return err
	}
	if s == nil {
		return catalogUnavailable()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initialize()
	if existing, ok := s.nodes[nodeID]; ok {
		if nodeRegistrationsEqual(existing, normalized) {
			return nil
		}
		// This same-rule merge is load-bearing; TestAuthorizedExtractDocumentSameTenantSharedEntityCollapses pins that shared derived entities collapse across a tenant's skill files without widening access.
		if existing.Kind == normalized.Kind &&
			existing.Rule.equal(normalized.Rule) &&
			derivedEntityNodesEqual(existing.Node, normalized.Node) {
			return nil
		}
		return catalogConflict()
	}
	s.nodes[nodeID] = normalized
	return nil
}

func derivedEntityNodesEqual(left, right graph.Node) bool {
	return left.ID != "" &&
		left.Properties[extraction.GraphPropertyEntityKey] != "" &&
		left.Properties[extraction.GraphPropertyOntologyConceptID] != "" &&
		graphNodesEqual(left, right)
}

// Revision returns one exact immutable revision registration.
func (s *MemoryPolicyStore) Revision(
	ctx context.Context,
	documentID, revisionID shoal.ID,
) (RevisionRegistration, bool, error) {
	if err := contextFailure(ctx); err != nil {
		return RevisionRegistration{}, false, err
	}
	if err := shoal.ValidateRequiredID("document ID", documentID); err != nil {
		return RevisionRegistration{}, false, err
	}
	if err := shoal.ValidateRequiredID("revision ID", revisionID); err != nil {
		return RevisionRegistration{}, false, err
	}
	if s == nil {
		return RevisionRegistration{}, false, catalogUnavailable()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := revisionKey{documentID: documentID, revisionID: revisionID}
	registration, ok := s.revisions[key]
	if !ok {
		return RevisionRegistration{}, false, nil
	}
	cloned := cloneRevisionRegistration(registration)
	cloned.Current = s.current[documentID] == key
	return cloned, true, nil
}

// CurrentRevision returns the exact current registration for a document.
func (s *MemoryPolicyStore) CurrentRevision(
	ctx context.Context,
	documentID shoal.ID,
) (RevisionRegistration, bool, error) {
	if err := contextFailure(ctx); err != nil {
		return RevisionRegistration{}, false, err
	}
	if err := shoal.ValidateRequiredID("document ID", documentID); err != nil {
		return RevisionRegistration{}, false, err
	}
	if s == nil {
		return RevisionRegistration{}, false, catalogUnavailable()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	key, ok := s.current[documentID]
	if !ok {
		return RevisionRegistration{}, false, nil
	}
	registration, ok := s.revisions[key]
	if !ok {
		return RevisionRegistration{}, false, catalogUnavailable()
	}
	cloned := cloneRevisionRegistration(registration)
	cloned.Current = true
	return cloned, true, nil
}

// CurrentRevisions returns the exact current registrations for the requested
// documents in one round trip. Each identifier is processed in request order
// with the same validation, lookup, and clone semantics as CurrentRevision.
// Repeated identifiers are attempted once after their first successful
// validation, and unregistered identifiers are omitted from the result.
func (s *MemoryPolicyStore) CurrentRevisions(
	ctx context.Context,
	documentIDs []shoal.ID,
) (map[shoal.ID]RevisionRegistration, error) {
	if s == nil || len(documentIDs) == 0 {
		if err := contextFailure(ctx); err != nil {
			return nil, err
		}
		if s == nil {
			if len(documentIDs) > 0 {
				if err := shoal.ValidateRequiredID(
					"document ID", documentIDs[0],
				); err != nil {
					return nil, err
				}
			}
			return nil, catalogUnavailable()
		}
		return map[shoal.ID]RevisionRegistration{}, nil
	}
	attempted := make(map[shoal.ID]struct{}, len(documentIDs))
	resolved := make(map[shoal.ID]RevisionRegistration, len(documentIDs))
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, documentID := range documentIDs {
		if err := contextFailure(ctx); err != nil {
			return nil, err
		}
		if err := shoal.ValidateRequiredID("document ID", documentID); err != nil {
			return nil, err
		}
		if _, done := attempted[documentID]; done {
			continue
		}
		attempted[documentID] = struct{}{}
		key, ok := s.current[documentID]
		if !ok {
			continue
		}
		registration, ok := s.revisions[key]
		if !ok {
			return nil, catalogUnavailable()
		}
		cloned := cloneRevisionRegistration(registration)
		cloned.Current = true
		resolved[documentID] = cloned
	}
	return resolved, nil
}

// Node returns the current registration owning a graph node.
func (s *MemoryPolicyStore) Node(
	ctx context.Context,
	nodeID shoal.ID,
) (NodeRegistration, bool, error) {
	if err := contextFailure(ctx); err != nil {
		return NodeRegistration{}, false, err
	}
	if err := shoal.ValidateRequiredID("graph node ID", nodeID); err != nil {
		return NodeRegistration{}, false, err
	}
	if s == nil {
		return NodeRegistration{}, false, catalogUnavailable()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	registration, ok := s.nodes[nodeID]
	if !ok {
		return NodeRegistration{}, false, nil
	}
	cloned, err := cloneNodeRegistration(registration)
	if err != nil {
		return NodeRegistration{}, false, catalogUnavailable()
	}
	return cloned, true, nil
}

// Nodes returns the current registrations owning the requested graph nodes in
// one round trip. Each identifier is processed in request order and takes
// exactly the steps Node takes for it — context check, argument validation,
// then lookup and clone — so both the per-identifier result and the error the
// batch fails with are the ones the equivalent Node loop produces, and each
// identifier is charged exactly one context check. A context cancelled partway
// through the batch, a malformed identifier, and a registration that fails to
// clone therefore all surface in the same order they would one call at a time.
// Unregistered identifiers are omitted from the result rather than reported as
// an error, so absence is the same fail-closed signal Node reports with a false
// boolean.
//
// Three deliberate differences from a literal Node loop remain. Repeated
// identifiers are looked up once, though the context is still checked for every
// occurrence. An empty request checks the context and the receiver, where a
// Node loop would make no call at all, so a cancelled context is still reported
// rather than answered with an empty result. The read lock is held for the
// whole batch, so the result is a single consistent snapshot rather than a
// sequence of independently locked reads.
func (s *MemoryPolicyStore) Nodes(
	ctx context.Context,
	nodeIDs []shoal.ID,
) (map[shoal.ID]NodeRegistration, error) {
	if s == nil || len(nodeIDs) == 0 {
		// The per-identifier context check below cannot run for these two
		// cases, so it happens here instead. Doing it unconditionally would
		// charge the first identifier of a live batch two checks where a Node
		// loop charges one.
		if err := contextFailure(ctx); err != nil {
			return nil, err
		}
		if s == nil {
			// A Node loop over a nil receiver fails on its first identifier,
			// but only after that identifier has been validated.
			if len(nodeIDs) > 0 {
				if err := shoal.ValidateRequiredID(
					"graph node ID", nodeIDs[0],
				); err != nil {
					return nil, err
				}
			}
			return nil, catalogUnavailable()
		}
		return map[shoal.ID]NodeRegistration{}, nil
	}
	attempted := make(map[shoal.ID]struct{}, len(nodeIDs))
	resolved := make(map[shoal.ID]NodeRegistration, len(nodeIDs))
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, nodeID := range nodeIDs {
		if err := contextFailure(ctx); err != nil {
			return nil, err
		}
		if err := shoal.ValidateRequiredID("graph node ID", nodeID); err != nil {
			return nil, err
		}
		if _, done := attempted[nodeID]; done {
			continue
		}
		attempted[nodeID] = struct{}{}
		registration, ok := s.nodes[nodeID]
		if !ok {
			continue
		}
		cloned, err := cloneNodeRegistration(registration)
		if err != nil {
			return nil, catalogUnavailable()
		}
		resolved[nodeID] = cloned
	}
	return resolved, nil
}

// ReserveEdge atomically reserves an application edge identity and rule before
// the base mutation. Existing identical reservations are retryable; any
// identity, content, or rule mismatch conflicts.
func (s *MemoryPolicyStore) ReserveEdge(
	ctx context.Context,
	registration EdgeRegistration,
) error {
	if err := contextFailure(ctx); err != nil {
		return err
	}
	normalized, err := normalizeApplicationEdgeRegistration(registration)
	if err != nil {
		return err
	}
	if s == nil {
		return catalogUnavailable()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initialize()
	if _, intrinsic := s.intrinsicEdges[normalized.Edge.ID]; intrinsic {
		return catalogConflict()
	}
	if existing, ok := s.edges[normalized.Edge.ID]; ok &&
		!edgeRegistrationsEqual(existing, normalized) {
		return catalogConflict()
	}
	if existing, ok := s.edgeClaims[normalized.Edge.ID]; ok {
		if edgeRegistrationsEqual(existing, normalized) {
			return nil
		}
		return catalogConflict()
	}
	s.edgeClaims[normalized.Edge.ID] = cloneEdgeRegistration(normalized)
	return nil
}

// RollbackEdgeReservation removes an uncommitted matching reservation after a
// definite base failure. Reservations survive ambiguous or catalog failures.
func (s *MemoryPolicyStore) RollbackEdgeReservation(
	ctx context.Context,
	registration EdgeRegistration,
) error {
	if err := contextFailure(ctx); err != nil {
		return err
	}
	normalized, err := normalizeApplicationEdgeRegistration(registration)
	if err != nil {
		return err
	}
	if s == nil {
		return catalogUnavailable()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initialize()
	if _, committed := s.edges[normalized.Edge.ID]; committed {
		return nil
	}
	existing, ok := s.edgeClaims[normalized.Edge.ID]
	if !ok {
		return nil
	}
	if !edgeRegistrationsEqual(existing, normalized) {
		return catalogConflict()
	}
	delete(s.edgeClaims, normalized.Edge.ID)
	return nil
}

// PutEdge atomically registers an application edge and its edge-local rule.
// Identical content and canonical rule are idempotent; identity or policy
// reuse conflicts.
func (s *MemoryPolicyStore) PutEdge(
	ctx context.Context,
	registration EdgeRegistration,
) error {
	if err := contextFailure(ctx); err != nil {
		return err
	}
	normalized, err := normalizeApplicationEdgeRegistration(registration)
	if err != nil {
		return err
	}
	if s == nil {
		return catalogUnavailable()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initialize()
	if _, intrinsic := s.intrinsicEdges[normalized.Edge.ID]; intrinsic {
		return catalogConflict()
	}
	if claim, ok := s.edgeClaims[normalized.Edge.ID]; ok &&
		!edgeRegistrationsEqual(claim, normalized) {
		return catalogConflict()
	}
	if existing, ok := s.edges[normalized.Edge.ID]; ok {
		switch extractedEdgeMerge(existing, normalized) {
		case edgeMergeKeep:
			delete(s.edgeClaims, normalized.Edge.ID)
			return nil
		case edgeMergeReplace:
			// The replaced application edge's revision binding (#695)
			// is kept: replacing may only narrow who reads the edge.
			normalized.SourceRevisions = append(
				[]RevisionReference(nil), existing.SourceRevisions...)
			normalized.SourceRevisionsBound = existing.SourceRevisionsBound
			s.edges[normalized.Edge.ID] = cloneEdgeRegistration(normalized)
			delete(s.edgeClaims, normalized.Edge.ID)
			return nil
		}
		return catalogConflict()
	}
	s.edges[normalized.Edge.ID] = cloneEdgeRegistration(normalized)
	delete(s.edgeClaims, normalized.Edge.ID)
	return nil
}

// Edge returns a current intrinsic edge or a registered application edge.
func (s *MemoryPolicyStore) Edge(
	ctx context.Context,
	edgeID shoal.ID,
) (EdgeRegistration, bool, error) {
	if err := contextFailure(ctx); err != nil {
		return EdgeRegistration{}, false, err
	}
	if err := shoal.ValidateRequiredID("graph edge ID", edgeID); err != nil {
		return EdgeRegistration{}, false, err
	}
	if s == nil {
		return EdgeRegistration{}, false, catalogUnavailable()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	registration, ok := s.edges[edgeID]
	if !ok {
		registration, ok = s.intrinsicEdges[edgeID]
	}
	if !ok {
		return EdgeRegistration{}, false, nil
	}
	cloned, err := cloneEdgeRegistrationChecked(registration)
	if err != nil {
		return EdgeRegistration{}, false, catalogUnavailable()
	}
	return cloned, true, nil
}

// Edges returns the requested edge registrations in one round trip under
// exactly the contract Nodes carries for node registrations, with Edge in place
// of Node: each identifier is processed in request order, takes the same steps
// Edge takes for it, and is charged exactly one context check, so the
// per-identifier result and the failing error are the ones the equivalent Edge
// loop produces, including a context cancelled partway through the batch and a
// registration that fails to clone. Unregistered identifiers are omitted rather
// than reported as an error, and the same three deliberate differences apply:
// repeated identifiers are looked up once, an empty request still checks the
// context and the receiver, and the read lock is held for the whole batch.
func (s *MemoryPolicyStore) Edges(
	ctx context.Context,
	edgeIDs []shoal.ID,
) (map[shoal.ID]EdgeRegistration, error) {
	if s == nil || len(edgeIDs) == 0 {
		if err := contextFailure(ctx); err != nil {
			return nil, err
		}
		if s == nil {
			if len(edgeIDs) > 0 {
				if err := shoal.ValidateRequiredID(
					"graph edge ID", edgeIDs[0],
				); err != nil {
					return nil, err
				}
			}
			return nil, catalogUnavailable()
		}
		return map[shoal.ID]EdgeRegistration{}, nil
	}
	attempted := make(map[shoal.ID]struct{}, len(edgeIDs))
	resolved := make(map[shoal.ID]EdgeRegistration, len(edgeIDs))
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, edgeID := range edgeIDs {
		if err := contextFailure(ctx); err != nil {
			return nil, err
		}
		if err := shoal.ValidateRequiredID("graph edge ID", edgeID); err != nil {
			return nil, err
		}
		if _, done := attempted[edgeID]; done {
			continue
		}
		attempted[edgeID] = struct{}{}
		registration, ok := s.edges[edgeID]
		if !ok {
			registration, ok = s.intrinsicEdges[edgeID]
		}
		if !ok {
			continue
		}
		cloned, err := cloneEdgeRegistrationChecked(registration)
		if err != nil {
			return nil, catalogUnavailable()
		}
		resolved[edgeID] = cloned
	}
	return resolved, nil
}

// LoadCoOccurrence returns the persisted mosaic co-occurrence state for an
// identity key, reporting membership by its boolean exactly as the graph reads
// do. The returned slice is an independent copy.
func (s *MemoryPolicyStore) LoadCoOccurrence(
	ctx context.Context,
	key string,
) (CoOccurrenceRecord, bool, error) {
	if err := contextFailure(ctx); err != nil {
		return CoOccurrenceRecord{}, false, err
	}
	if s == nil {
		return CoOccurrenceRecord{}, false, catalogUnavailable()
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.coOccurrence[key]
	if !ok {
		return CoOccurrenceRecord{}, false, nil
	}
	return cloneCoOccurrenceRecord(record), true, nil
}

// StoreCoOccurrence overwrites the identity's mosaic co-occurrence state
// atomically with an independent copy of the record.
func (s *MemoryPolicyStore) StoreCoOccurrence(
	ctx context.Context,
	key string,
	record CoOccurrenceRecord,
) error {
	if err := contextFailure(ctx); err != nil {
		return err
	}
	if s == nil {
		return catalogUnavailable()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.initialize()
	s.coOccurrence[key] = cloneCoOccurrenceRecord(record)
	return nil
}

func (s *MemoryPolicyStore) snapshotCoOccurrence(
	key string,
) (CoOccurrenceRecord, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.coOccurrence[key]
	if !ok {
		return CoOccurrenceRecord{}, false
	}
	return cloneCoOccurrenceRecord(record), true
}

func cloneCoOccurrenceRecord(record CoOccurrenceRecord) CoOccurrenceRecord {
	return CoOccurrenceRecord{
		WindowStart: record.WindowStart,
		Domains:     append([]string(nil), record.Domains...),
	}
}

func (s *MemoryPolicyStore) initialize() {
	if s.sourceClaims == nil {
		s.sourceClaims = make(map[string]sourceClaimState)
	}
	if s.revisions == nil {
		s.revisions = make(map[revisionKey]RevisionRegistration)
	}
	if s.revisionIDs == nil {
		s.revisionIDs = make(map[shoal.ID]revisionKey)
	}
	if s.current == nil {
		s.current = make(map[shoal.ID]revisionKey)
	}
	if s.nodes == nil {
		s.nodes = make(map[shoal.ID]NodeRegistration)
	}
	if s.intrinsicEdges == nil {
		s.intrinsicEdges = make(map[shoal.ID]EdgeRegistration)
	}
	if s.edgeClaims == nil {
		s.edgeClaims = make(map[shoal.ID]EdgeRegistration)
	}
	if s.edges == nil {
		s.edges = make(map[shoal.ID]EdgeRegistration)
	}
	if s.coOccurrence == nil {
		s.coOccurrence = make(map[string]CoOccurrenceRecord)
	}
	if s.documentRevisions == nil {
		s.documentRevisions = make(map[shoal.ID][]shoal.ID)
	}
	if s.interactionRevisions == nil {
		s.interactionRevisions = make(map[shoal.ID][]RevisionReference)
	}
}

func validateSourceURI(sourceURI string) error {
	if !utf8.ValidString(sourceURI) || strings.TrimSpace(sourceURI) == "" {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"source URI is required and must be valid UTF-8",
		)
	}
	return nil
}

func cloneSourcePolicyClaim(
	claim SourcePolicyClaim,
) (SourcePolicyClaim, error) {
	rule, err := claim.Rule.clone()
	if err != nil {
		return SourcePolicyClaim{}, err
	}
	claim.Rule = rule
	if claim.PreviousRule != nil {
		previous, previousErr := claim.PreviousRule.clone()
		if previousErr != nil {
			return SourcePolicyClaim{}, previousErr
		}
		claim.PreviousRule = &previous
	}
	if !claim.Pending && claim.PreviousRule != nil {
		return SourcePolicyClaim{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "source policy claim is invalid")
	}
	if !claim.Pending || claim.Version == 0 {
		if claim.hadPriorClaim || claim.priorPending ||
			claim.priorVersion != 0 {
			return SourcePolicyClaim{}, shoal.NewError(
				shoal.ErrorInvalidArgument, "source policy claim is invalid")
		}
	} else if claim.hadPriorClaim {
		if claim.priorVersion == 0 ||
			(!claim.priorPending && claim.PreviousRule == nil) {
			return SourcePolicyClaim{}, shoal.NewError(
				shoal.ErrorInvalidArgument, "source policy claim is invalid")
		}
	} else if claim.priorPending || claim.priorVersion != 0 {
		return SourcePolicyClaim{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "source policy claim is invalid")
	}
	return claim, nil
}

func sourcePolicyClaimsEqual(left, right SourcePolicyClaim) bool {
	return left.SourceURI == right.SourceURI &&
		left.Version == right.Version &&
		left.Pending == right.Pending &&
		left.hadPriorClaim == right.hadPriorClaim &&
		left.priorPending == right.priorPending &&
		left.priorVersion == right.priorVersion &&
		left.acquisitionID == right.acquisitionID &&
		left.tokenSeal == right.tokenSeal &&
		left.Rule.equal(right.Rule) &&
		optionalRulesEqual(left.PreviousRule, right.PreviousRule)
}

// sourceClaimObservable strips the sealed finalization capability from a claim
// token, leaving only the state readers are allowed to observe. Reading a
// claim must never confer the ability to finalize it.
func sourceClaimObservable(token SourcePolicyClaim) SourcePolicyClaim {
	observable := token
	observable.hadPriorClaim = false
	observable.priorPending = false
	observable.priorVersion = 0
	observable.acquisitionID = 0
	observable.tokenSeal = auth.Digest{}
	return observable
}

func sourceClaimTokenSeal(claim SourcePolicyClaim) auth.Digest {
	encoded := make([]byte, 0, len(claim.SourceURI)+128)
	appendUint64 := func(value uint64) {
		var buffer [8]byte
		binary.BigEndian.PutUint64(buffer[:], value)
		encoded = append(encoded, buffer[:]...)
	}
	appendText := func(value string) {
		appendUint64(uint64(len(value)))
		encoded = append(encoded, value...)
	}
	appendText(claim.SourceURI)
	appendText(claim.Rule.String())
	if claim.PreviousRule == nil {
		encoded = append(encoded, 0)
	} else {
		encoded = append(encoded, 1)
		appendText(claim.PreviousRule.String())
	}
	if claim.Pending {
		encoded = append(encoded, 1)
	} else {
		encoded = append(encoded, 0)
	}
	appendUint64(claim.Version)
	if claim.hadPriorClaim {
		encoded = append(encoded, 1)
	} else {
		encoded = append(encoded, 0)
	}
	if claim.priorPending {
		encoded = append(encoded, 1)
	} else {
		encoded = append(encoded, 0)
	}
	appendUint64(claim.priorVersion)
	appendUint64(claim.acquisitionID)
	return auth.DigestBytes("explorer-source-claim-token-v1", encoded)
}

func optionalRulesEqual(left, right *AccessRule) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.equal(*right)
}

func (s *MemoryPolicyStore) replaceCurrent(
	key revisionKey,
	registration RevisionRegistration,
) {
	if previousKey, ok := s.current[registration.DocumentID]; ok {
		if previous, exists := s.revisions[previousKey]; exists {
			for _, nodeID := range previous.NodeIDs {
				if owner, present := s.nodes[nodeID]; present &&
					owner.DocumentID == previous.DocumentID &&
					owner.RevisionID == previous.RevisionID {
					delete(s.nodes, nodeID)
				}
			}
			for _, edge := range previous.IntrinsicEdges {
				if owner, present := s.intrinsicEdges[edge.ID]; present &&
					owner.DocumentID == previous.DocumentID &&
					owner.RevisionID == previous.RevisionID {
					delete(s.intrinsicEdges, edge.ID)
				}
			}
		}
	}
	for _, nodeID := range registration.NodeIDs {
		s.nodes[nodeID] = NodeRegistration{
			DocumentID: registration.DocumentID,
			RevisionID: registration.RevisionID,
			Rule:       mustCloneRule(registration.Rule),
			Kind:       RegistrationDocument,
		}
	}
	for _, edge := range registration.IntrinsicEdges {
		s.intrinsicEdges[edge.ID] = EdgeRegistration{
			Kind:       RegistrationDocument,
			Edge:       cloneGraphEdge(edge),
			DocumentID: registration.DocumentID,
			RevisionID: registration.RevisionID,
			Rule:       mustCloneRule(registration.Rule),
		}
	}
	s.current[registration.DocumentID] = key
}

func normalizeRevisionRegistration(
	registration RevisionRegistration,
) (RevisionRegistration, error) {
	if err := shoal.ValidateRequiredID(
		"document ID", registration.DocumentID,
	); err != nil {
		return RevisionRegistration{}, err
	}
	if err := shoal.ValidateRequiredID(
		"revision ID", registration.RevisionID,
	); err != nil {
		return RevisionRegistration{}, err
	}
	rule, err := registration.Rule.clone()
	if err != nil {
		return RevisionRegistration{}, err
	}
	if registration.ContentDigest == (auth.Digest{}) {
		return RevisionRegistration{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "revision content digest is required")
	}
	nodeIDs := append([]shoal.ID(nil), registration.NodeIDs...)
	if len(nodeIDs) == 0 {
		return RevisionRegistration{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "revision graph nodes are required")
	}
	sort.Slice(nodeIDs, func(left, right int) bool {
		return shoal.CompareID(nodeIDs[left], nodeIDs[right]) < 0
	})
	nodes := make(map[shoal.ID]struct{}, len(nodeIDs))
	deduplicatedNodes := nodeIDs[:0]
	for _, nodeID := range nodeIDs {
		if err := shoal.ValidateRequiredID("graph node ID", nodeID); err != nil {
			return RevisionRegistration{}, err
		}
		if _, duplicate := nodes[nodeID]; duplicate {
			continue
		}
		nodes[nodeID] = struct{}{}
		deduplicatedNodes = append(deduplicatedNodes, nodeID)
	}
	if _, ok := nodes[registration.DocumentID]; !ok {
		return RevisionRegistration{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "revision graph lacks its document node")
	}

	edges := make([]graph.Edge, len(registration.IntrinsicEdges))
	for index, edge := range registration.IntrinsicEdges {
		if err := edge.Validate(); err != nil {
			return RevisionRegistration{}, err
		}
		if _, ok := nodes[edge.From]; !ok {
			return RevisionRegistration{}, shoal.NewError(
				shoal.ErrorInvalidArgument, "intrinsic edge source is outside the revision")
		}
		if _, ok := nodes[edge.To]; !ok {
			return RevisionRegistration{}, shoal.NewError(
				shoal.ErrorInvalidArgument, "intrinsic edge target is outside the revision")
		}
		edges[index] = cloneGraphEdge(edge)
	}
	sort.Slice(edges, func(left, right int) bool {
		return shoal.CompareID(edges[left].ID, edges[right].ID) < 0
	})
	deduplicatedEdges := edges[:0]
	for _, edge := range edges {
		if len(deduplicatedEdges) > 0 &&
			deduplicatedEdges[len(deduplicatedEdges)-1].ID == edge.ID {
			if !graphEdgesEqual(deduplicatedEdges[len(deduplicatedEdges)-1], edge) {
				return RevisionRegistration{}, shoal.NewError(
					shoal.ErrorInvalidArgument,
					"revision graph reuses an edge identity",
				)
			}
			continue
		}
		deduplicatedEdges = append(deduplicatedEdges, edge)
	}
	return RevisionRegistration{
		DocumentID:     registration.DocumentID,
		RevisionID:     registration.RevisionID,
		NodeIDs:        deduplicatedNodes,
		IntrinsicEdges: deduplicatedEdges,
		ContentDigest:  registration.ContentDigest,
		Rule:           rule,
		Current:        registration.Current,
	}, nil
}

func normalizeNodeRegistration(
	nodeID shoal.ID,
	registration NodeRegistration,
) (NodeRegistration, error) {
	if err := shoal.ValidateRequiredID("graph node ID", nodeID); err != nil {
		return NodeRegistration{}, err
	}
	if err := shoal.ValidateRequiredID(
		"document ID", registration.DocumentID,
	); err != nil {
		return NodeRegistration{}, err
	}
	if err := shoal.ValidateRequiredID(
		"revision ID", registration.RevisionID,
	); err != nil {
		return NodeRegistration{}, err
	}
	switch registration.Kind {
	case RegistrationExtracted, RegistrationMaterialized:
	default:
		return NodeRegistration{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"node registration kind must be extracted or materialized")
	}
	rule, err := registration.Rule.clone()
	if err != nil {
		return NodeRegistration{}, err
	}
	node := graph.Node{}
	if registration.Node.ID != "" {
		if registration.Node.ID != nodeID {
			return NodeRegistration{}, catalogConflict()
		}
		if err := registration.Node.Validate(); err != nil {
			return NodeRegistration{}, err
		}
		node = cloneGraphNode(registration.Node)
	}
	return NodeRegistration{
		DocumentID: registration.DocumentID,
		RevisionID: registration.RevisionID,
		Node:       node,
		Rule:       rule,
		Kind:       registration.Kind,
	}, nil
}

func normalizeApplicationEdgeRegistration(
	registration EdgeRegistration,
) (EdgeRegistration, error) {
	if err := registration.Edge.Validate(); err != nil {
		return EdgeRegistration{}, err
	}
	rule, err := registration.Rule.clone()
	if err != nil {
		return EdgeRegistration{}, err
	}
	normalized := EdgeRegistration{
		Edge: cloneGraphEdge(registration.Edge), Rule: rule,
		Kind: registration.Kind,
	}
	sources, err := canonicalRevisionReferences(registration.SourceRevisions)
	if err != nil {
		return EdgeRegistration{}, err
	}
	if len(sources) > 0 && !registration.SourceRevisionsBound {
		return EdgeRegistration{}, shoal.NewError(shoal.ErrorInvalidArgument,
			"edge source revisions require a bound registration")
	}
	if registration.SourceRevisionsBound {
		// Only Connect (application) binds; an extracted edge carries the
		// binding of the application edge it replaced (extractedEdgeMerge).
		switch registration.Kind {
		case RegistrationApplication, RegistrationUnspecified, RegistrationExtracted:
		default:
			return EdgeRegistration{}, shoal.NewError(shoal.ErrorInvalidArgument,
				"only application and extracted edges carry source revisions")
		}
		normalized.SourceRevisions = sources
		normalized.SourceRevisionsBound = true
	}
	switch registration.Kind {
	case RegistrationUnspecified:
		// The pre-kind PutEdge/ReserveEdge contract registered Connect edges.
		normalized.Kind = RegistrationApplication
	case RegistrationApplication, RegistrationMaterialized:
	case RegistrationExtracted:
		// An extracted edge is bound to the revision that asserted it.
		if err := shoal.ValidateRequiredID(
			"extracted edge document ID", registration.DocumentID); err != nil {
			return EdgeRegistration{}, err
		}
		if err := shoal.ValidateRequiredID(
			"extracted edge revision ID", registration.RevisionID); err != nil {
			return EdgeRegistration{}, err
		}
		normalized.DocumentID = registration.DocumentID
		normalized.RevisionID = registration.RevisionID
	default:
		return EdgeRegistration{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"edge registration kind cannot be registered through PutEdge")
	}
	return normalized, nil
}

func revisionContentEqual(left, right RevisionRegistration) bool {
	if left.DocumentID != right.DocumentID ||
		left.RevisionID != right.RevisionID ||
		left.ContentDigest != right.ContentDigest ||
		!left.Rule.equal(right.Rule) ||
		len(left.NodeIDs) != len(right.NodeIDs) {
		return false
	}
	for index := range left.NodeIDs {
		if left.NodeIDs[index] != right.NodeIDs[index] {
			return false
		}
	}
	return true
}

func intrinsicEdgesEqual(left, right []graph.Edge) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !graphEdgesEqual(left[index], right[index]) {
			return false
		}
	}
	return true
}

type edgeMerge uint8

const (
	edgeMergeConflict edgeMerge = iota
	edgeMergeKeep
	edgeMergeReplace
)

// extractedEdgeMerge decides a PutEdge over an existing registration of the
// same edge ID. Identical registrations are idempotent. Relation edge IDs do
// not depend on the asserting document, so two documents under the same rule
// can extract the same edge: the registration stays with the FIRST asserter,
// like a shared extracted node. If that first asserter is later labelled, the
// edge closes for readers without the label even though another, public
// document also asserts it. That fails closed and is accepted (#570). A
// matching edge that was registered without a document (an application edge,
// or an extracted edge persisted before RegistrationKind existed) is replaced
// by the extracted registration, which only adds the asserting document's
// current rule and so can only narrow who reads it.
func extractedEdgeMerge(existing, incoming EdgeRegistration) edgeMerge {
	if edgeRegistrationsEqual(existing, incoming) {
		return edgeMergeKeep
	}
	if incoming.Kind != RegistrationExtracted ||
		!graphEdgesEqual(existing.Edge, incoming.Edge) ||
		!existing.Rule.equal(incoming.Rule) {
		return edgeMergeConflict
	}
	switch existing.Kind {
	case RegistrationExtracted:
		return edgeMergeKeep
	case RegistrationApplication:
		return edgeMergeReplace
	default:
		return edgeMergeConflict
	}
}

func edgeRegistrationsEqual(left, right EdgeRegistration) bool {
	return left.Kind == right.Kind &&
		graphEdgesEqual(left.Edge, right.Edge) &&
		left.DocumentID == right.DocumentID &&
		left.RevisionID == right.RevisionID &&
		left.Rule.equal(right.Rule) &&
		left.SourceRevisionsBound == right.SourceRevisionsBound &&
		revisionReferencesEqual(left.SourceRevisions, right.SourceRevisions)
}

func nodeRegistrationsEqual(left, right NodeRegistration) bool {
	return left.Kind == right.Kind &&
		left.DocumentID == right.DocumentID &&
		left.RevisionID == right.RevisionID &&
		graphNodesEqual(left.Node, right.Node) &&
		left.Rule.equal(right.Rule)
}

func cloneRevisionRegistration(
	registration RevisionRegistration,
) RevisionRegistration {
	cloned := registration
	cloned.NodeIDs = append([]shoal.ID(nil), registration.NodeIDs...)
	cloned.IntrinsicEdges = make([]graph.Edge, len(registration.IntrinsicEdges))
	for index, edge := range registration.IntrinsicEdges {
		cloned.IntrinsicEdges[index] = cloneGraphEdge(edge)
	}
	cloned.Rule = mustCloneRule(registration.Rule)
	return cloned
}

func cloneNodeRegistration(
	registration NodeRegistration,
) (NodeRegistration, error) {
	rule, err := registration.Rule.clone()
	if err != nil {
		return NodeRegistration{}, err
	}
	registration.Rule = rule
	registration.Node = cloneGraphNode(registration.Node)
	return registration, nil
}

func cloneEdgeRegistration(
	registration EdgeRegistration,
) EdgeRegistration {
	cloned, _ := cloneEdgeRegistrationChecked(registration)
	return cloned
}

func cloneEdgeRegistrationChecked(
	registration EdgeRegistration,
) (EdgeRegistration, error) {
	rule, err := registration.Rule.clone()
	if err != nil {
		return EdgeRegistration{}, err
	}
	registration.Edge = cloneGraphEdge(registration.Edge)
	registration.Rule = rule
	registration.SourceRevisions = append(
		[]RevisionReference(nil), registration.SourceRevisions...)
	return registration, nil
}

func cloneGraphEdge(edge graph.Edge) graph.Edge {
	edge.Properties = cloneMetadata(edge.Properties)
	return edge
}

func graphEdgesEqual(left, right graph.Edge) bool {
	if left.ID != right.ID || left.From != right.From || left.To != right.To ||
		left.Type != right.Type || !scoresEqual(left.Weight, right.Weight) ||
		len(left.Properties) != len(right.Properties) {
		return false
	}
	for key, value := range left.Properties {
		if rightValue, ok := right.Properties[key]; !ok || rightValue != value {
			return false
		}
	}
	return true
}

func mustCloneRule(rule AccessRule) AccessRule {
	cloned, err := rule.clone()
	if err != nil {
		return AccessRule{}
	}
	return cloned
}

func catalogConflict() error {
	return shoal.NewError(shoal.ErrorConflict, "authorization policy catalog conflict")
}

func catalogUnavailable() error {
	return shoal.NewError(
		shoal.ErrorUnavailable, "authorization policy catalog unavailable")
}

var _ PolicyStore = (*MemoryPolicyStore)(nil)
