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
	"bytes"
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// SnapshotValidator verifies corpus frontiers pinned into interaction records.
type SnapshotValidator interface {
	ValidateSnapshot(context.Context, shoal.ID, time.Time, []shoal.ID) error
}

// EvidenceSnapshotValidator additionally binds exact source evidence to the
// pinned corpus frontier.
type EvidenceSnapshotValidator interface {
	SnapshotValidator
	ValidateEvidenceSnapshot(
		context.Context, shoal.ID, time.Time, []shoal.ID, []shoal.ID,
		[]interaction.EvidenceReference,
	) error
}

// DerivedAssertionReader supplies canonical derived assertions independently
// of the untrusted graph response being authorized.
type DerivedAssertionReader interface {
	DerivedAssertions(
		context.Context, []shoal.ID,
	) (map[shoal.ID]ontology.Assertion, error)
}

// FoldStore is the explicitly trusted durable source for provenance folds.
// It is separate from Base because fold acknowledgements and rehydrated
// provenance are authorization evidence.
type FoldStore interface {
	FoldInteractions(
		context.Context, explorer.FoldRequest,
	) (explorer.FoldResult, error)
	RehydrateFold(context.Context, shoal.ID) (interaction.Fold, error)
	Folds(context.Context) ([]explorer.FoldSummary, error)
}

// Config supplies the trusted dependencies for an authorization-enforcing
// Explorer client.
type Config struct {
	Base explorer.Client
	// VectorScorer is an optional explicitly trusted scorer for authorized
	// vector retrieval validation. It is intentionally separate from Base:
	// Base responses are treated as untrusted and validated canonically.
	VectorScorer VectorScorer
	// InteractionWriter is the explicitly trusted durable sink for
	// interaction records. It is separate from Base because Base responses
	// and mutation acknowledgements are not authorization evidence.
	InteractionWriter explorer.InteractionWriter
	// InteractionReader is the explicitly trusted source for durable
	// interaction envelopes. It is intentionally separate from Base because
	// authorization decisions for derived views depend on the stored source
	// set and authorization fingerprint.
	InteractionReader explorer.InteractionReader
	// SnapshotValidator is the explicitly trusted verifier for historical
	// corpus frontiers pinned into interaction records.
	SnapshotValidator SnapshotValidator
	// DerivedAssertionReader is the explicitly trusted source used to
	// reconstruct producer provenance returned by an untrusted graph reader.
	// When omitted, NewClient may use SnapshotValidator if that independently
	// trusted dependency also implements DerivedAssertionReader. Base is never
	// promoted implicitly.
	DerivedAssertionReader DerivedAssertionReader
	// FoldStore is the explicitly trusted durable fold source. When omitted,
	// NewClient may use SnapshotValidator if that separately trusted
	// dependency also implements FoldStore. Base is never promoted implicitly.
	FoldStore FoldStore
	// OntologyInterpreter is an optional explicitly trusted read-time
	// interpreter. It is separate from Base because Base graph responses are
	// untrusted and must never be allowed to inject interpretations.
	OntologyInterpreter explorer.OntologyInterpreter
	// OntologyProposalStore is the optional explicitly trusted governance
	// store. It is separate from Base because proposal state, evidence, and
	// citation bytes participate in authorization decisions.
	OntologyProposalStore explorer.OntologyProposalStore
	Resolver              auth.Resolver
	PolicySelector        PolicySelector
	EdgePolicySelector    EdgePolicySelector
	PolicyStore           PolicyStore
	GenerationReader      auth.GenerationReader
	Clock                 func() time.Time
	// Mosaic optionally enables the sensitivity-domain co-occurrence budget
	// that defends against the mosaic effect. A zero MaxDomains disables it; a
	// nonzero MaxDomains requires PolicyStore to implement CoOccurrenceLedger
	// and a positive Window, so an enabled-but-unbacked control fails closed at
	// construction.
	Mosaic MosaicBudget
	// CeilingResolver resolves a trusted service's configured ceiling for
	// the reader label evaluator (#564). Nil means no service account has
	// one, so every trusted service is refused labelled data.
	CeilingResolver CeilingResolver
	// RequireDeclassificationReason refuses an ingest that narrows a
	// document's rule (removes a visibility label or any other rule
	// component, #630) unless its context carries a reason code from
	// WithDeclassificationReason. Every narrowing is audited regardless.
	// It is enforced by the Client making the narrowing, so every Client
	// sharing one PolicyStore should be configured alike.
	RequireDeclassificationReason bool
}

// Client enforces trusted-context authorization around an Explorer client.
type Client struct {
	base                explorer.Client
	vectorScorer        VectorScorer
	interactionSink     explorer.InteractionWriter
	interactionSource   explorer.InteractionReader
	snapshotValidator   SnapshotValidator
	derivedAssertions   DerivedAssertionReader
	foldSource          FoldStore
	ontologyInterpreter explorer.OntologyInterpreter
	ontologyProposals   explorer.OntologyProposalStore
	resolver            auth.Resolver
	policySelector      PolicySelector
	edgePolicySelector  EdgePolicySelector
	policyStore         PolicyStore
	generationReader    auth.GenerationReader
	clock               func() time.Time
	mosaic              MosaicBudget
	accumulator         *Accumulator[[]string]
	labelVisibility     *LabelVisibility
	// requireDeclassificationReason is Config.RequireDeclassificationReason.
	requireDeclassificationReason bool
	mutationMu                    sync.Mutex
	vectorMu                      sync.Mutex
	vectorAvailability            authorizedVectorAvailabilityCache
}

type authorizedVectorAvailabilityCache struct {
	key       string
	checkedAt time.Time
	available bool
}

// NewClient validates every dependency and constructs an additive wrapper.
// An edge selector may be supplied explicitly or implemented by PolicySelector.
func NewClient(config Config) (*Client, error) {
	if isNilDependency(config.Base) {
		return nil, dependencyRequired("base Explorer client")
	}
	if isNilDependency(config.Resolver) {
		return nil, dependencyRequired("authorization resolver")
	}
	if isNilDependency(config.PolicySelector) {
		return nil, dependencyRequired("policy selector")
	}
	if isNilDependency(config.PolicyStore) {
		return nil, dependencyRequired("policy store")
	}
	if isNilDependency(config.GenerationReader) {
		return nil, dependencyRequired("generation reader")
	}
	if config.Clock == nil {
		return nil, dependencyRequired("clock")
	}
	hasInteractionWriter := !isNilDependency(config.InteractionWriter)
	hasInteractionReader := !isNilDependency(config.InteractionReader)
	hasSnapshotValidator := !isNilDependency(config.SnapshotValidator)
	if hasInteractionWriter && (!hasInteractionReader || !hasSnapshotValidator) {
		return nil, dependencyRequired(
			"trusted interaction writer, reader, and snapshot validator")
	}
	if hasSnapshotValidator && !hasInteractionWriter {
		return nil, dependencyRequired("trusted interaction writer")
	}
	derivedAssertions := config.DerivedAssertionReader
	if isNilDependency(derivedAssertions) && hasSnapshotValidator {
		derivedAssertions, _ =
			config.SnapshotValidator.(DerivedAssertionReader)
	}
	foldStore := config.FoldStore
	if isNilDependency(foldStore) && hasSnapshotValidator {
		foldStore, _ = config.SnapshotValidator.(FoldStore)
	}
	edgeSelector := config.EdgePolicySelector
	if isNilDependency(edgeSelector) {
		var ok bool
		edgeSelector, ok = config.PolicySelector.(EdgePolicySelector)
		if !ok || isNilDependency(edgeSelector) {
			return nil, dependencyRequired("edge policy selector")
		}
	}
	accumulator, err := config.Mosaic.accumulator(config.PolicyStore)
	if err != nil {
		return nil, err
	}
	labelVisibility, err := NewLabelVisibility(LabelVisibilityConfig{
		Resolver: config.Resolver, Ceilings: config.CeilingResolver,
		Clock: config.Clock,
	})
	if err != nil {
		return nil, err
	}
	return &Client{
		base:                config.Base,
		vectorScorer:        config.VectorScorer,
		interactionSink:     config.InteractionWriter,
		interactionSource:   config.InteractionReader,
		snapshotValidator:   config.SnapshotValidator,
		derivedAssertions:   derivedAssertions,
		foldSource:          foldStore,
		ontologyInterpreter: config.OntologyInterpreter,
		ontologyProposals:   config.OntologyProposalStore,
		resolver:            config.Resolver,
		policySelector:      config.PolicySelector,
		edgePolicySelector:  edgeSelector,
		policyStore:         config.PolicyStore,
		generationReader:    config.GenerationReader,
		clock:               config.Clock,
		mosaic:              config.Mosaic,
		accumulator:         accumulator,
		labelVisibility:     labelVisibility,

		requireDeclassificationReason: config.RequireDeclassificationReason,
	}, nil
}

// Ingest authorizes and catalogs one immutable revision. A successful base
// commit is never reported as success until its exact policy registration is
// complete.
func (c *Client) Ingest(
	ctx context.Context,
	source explorer.Source,
) (
	returned explorer.IngestResult,
	returnedErr error,
) {
	decision, guard, now, err := c.begin(ctx, auth.OperationIngest)
	if err != nil {
		return explorer.IngestResult{}, err
	}
	ownedSource := cloneSource(source)
	rule, err := c.selectIngestRule(
		ctx, decision, cloneSource(ownedSource), now)
	if err != nil {
		return explorer.IngestResult{}, err
	}

	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()
	lease, err := c.policyStore.AcquireMutation(ctx)
	if err != nil {
		return explorer.IngestResult{}, policyCatalogWriteError(ctx, err)
	}
	defer lease.Release()
	if err := guard.Check(ctx); err != nil {
		return explorer.IngestResult{}, err
	}
	claim, declassification, err := c.claimSourceMutation(
		ctx, ownedSource.URI, decision, rule, now)
	if err != nil {
		return explorer.IngestResult{}, err
	}
	claimOutcome := sourceClaimRollback
	defer func() {
		cleanupContext := context.WithoutCancel(ctx)
		var cleanupErr error
		switch claimOutcome {
		case sourceClaimCommit:
			cleanupErr = c.policyStore.CommitSourceClaim(
				cleanupContext, claim)
		case sourceClaimPend:
			cleanupErr = c.policyStore.PendSourceClaim(
				cleanupContext, claim)
		case sourceClaimRollback:
			cleanupErr = c.policyStore.RollbackSourceClaim(
				cleanupContext, claim)
		}
		if cleanupErr != nil {
			catalogErr := policyCatalogWriteError(
				cleanupContext, cleanupErr)
			returned = explorer.IngestResult{}
			if returnedErr == nil {
				returnedErr = catalogErr
			} else {
				returnedErr = errors.Join(returnedErr, catalogErr)
			}
		}
	}()
	if err := guard.Check(ctx); err != nil {
		return explorer.IngestResult{}, err
	}
	result, err := c.base.Ingest(ctx, cloneSource(ownedSource))
	if err != nil {
		if explorer.IsIndeterminateCommit(err) {
			claimOutcome = sourceClaimPend
		}
		return explorer.IngestResult{}, err
	}
	// An applied base write is not yet a committed claim (#665). The claim
	// commits only once the revision is registered current below. Until
	// then any failure ends it as Pend, keeping both the desired rule and
	// the rule it replaces (PreviousRule), so a retry must be authorized
	// under both. Before #665 the claim committed here, and a failure
	// before registration left a committed claim under the new rule that
	// any holder of that rule alone could finish. That is how a principal
	// lacking the old rule could complete someone else's half-finished
	// relabel, and how a refused attempt could leave a claim behind.
	//
	// Two consequences are intended and fail closed:
	//   - a claim holder without the replaced rule can no longer complete
	//     someone else's half-finished change;
	//   - a pending claim admits only a retry selecting exactly its rule,
	//     so it blocks a different relabel of the URI until someone
	//     authorized under both rules retries it.
	//
	// The pending state is the one a crash already leaves (a held claim
	// recovers as pending), so this needs no new persisted state.
	if result.Disposition == explorer.IngestApplied {
		claimOutcome = sourceClaimPend
	}
	view, err := c.base.Document(
		ctx, result.Document.ID, result.Revision.ID)
	if err != nil {
		return explorer.IngestResult{}, err
	}
	if view.Document.ID != result.Document.ID ||
		view.Document.RevisionID != result.Revision.ID ||
		view.Revision.ID != result.Revision.ID ||
		view.Revision.DocumentID != result.Document.ID {
		return explorer.IngestResult{}, inconsistentBase()
	}
	nodeIDs, err := documentViewNodeIDs(view)
	if err != nil {
		return explorer.IngestResult{}, inconsistentBase()
	}
	summaries, err := c.base.Documents(ctx)
	if err != nil {
		return explorer.IngestResult{}, err
	}
	current, err := exactRevisionIsCurrent(
		summaries, result.Document.ID, result.Revision.ID)
	if err != nil {
		return explorer.IngestResult{}, err
	}
	if !current {
		claimOutcome = sourceClaimRollback
	}
	var intrinsicEdges []graph.Edge
	if current {
		intrinsicEdges, err = c.intrinsicEdges(ctx, nodeIDs)
		if err != nil {
			return explorer.IngestResult{}, err
		}
	}
	digest, err := documentViewDigest(view)
	if err != nil {
		return explorer.IngestResult{}, inconsistentBase()
	}
	// The pre-write check authorized the registration of the document the
	// Explorer derives from the URI, so the base must have written exactly
	// that document; anything else is an inconsistent base, refused before
	// it is registered (#665).
	if result.Document.ID != explorer.SourceDocumentID(ownedSource.URI) {
		return explorer.IngestResult{}, inconsistentBase()
	}
	// The registration below is what lets a narrowed rule's new readers
	// in, so a narrowing is audited immediately before it (#630).
	if current {
		if err := c.declassificationCommit(
			ctx, decision, now, ownedSource.URI, result.Document.ID,
			result.Revision.ID, rule, declassification,
		); err != nil {
			return explorer.IngestResult{}, err
		}
	}
	if err := c.policyStore.PutRevision(ctx, RevisionRegistration{
		DocumentID:     result.Document.ID,
		RevisionID:     result.Revision.ID,
		NodeIDs:        nodeIDs,
		IntrinsicEdges: intrinsicEdges,
		ContentDigest:  digest,
		Rule:           rule,
		Current:        current,
	}); err != nil {
		return explorer.IngestResult{}, policyCatalogWriteError(ctx, err)
	}
	if current {
		claimOutcome = sourceClaimCommit
	}
	sectionCount, spanCount := documentViewCounts(view)
	cloned := explorer.IngestResult{
		Disposition:  result.Disposition,
		Document:     cloneDocument(view.Document),
		Revision:     cloneRevision(view.Revision),
		SectionCount: sectionCount,
		SpanCount:    spanCount,
	}
	if cloned.Disposition != explorer.IngestApplied &&
		cloned.Disposition != explorer.IngestUnchanged {
		return explorer.IngestResult{}, inconsistentBase()
	}
	if err := guard.Check(ctx); err != nil {
		return explorer.IngestResult{}, err
	}
	c.invalidateAuthorizedVectorAvailability()
	return cloned, nil
}

func documentViewCounts(view explorer.DocumentView) (int, int) {
	sections, spans := 0, 0
	var visit func(explorer.SectionView)
	visit = func(section explorer.SectionView) {
		sections++
		spans += len(section.Spans)
		for _, child := range section.Children {
			visit(child)
		}
	}
	visit(view.Root)
	return sections, spans
}

type sourceClaimOutcome uint8

const (
	sourceClaimRollback sourceClaimOutcome = iota
	sourceClaimCommit
	sourceClaimPend
)

// claimSourceMutation acquires shared source-URI mutation ownership before the
// base is changed. Existing claims authorize retries after a base commit even
// when revision registration failed. Legacy registered documents may backfill
// a missing claim only after authorization under their current rule.
func (c *Client) claimSourceMutation(
	ctx context.Context,
	sourceURI string,
	decision auth.Decision,
	selectedRule AccessRule,
	now time.Time,
) (SourcePolicyClaim, *declassificationAttempt, error) {
	if err := validateSourceURI(sourceURI); err != nil {
		return SourcePolicyClaim{}, nil, err
	}
	existingClaim, claimed, err := c.policyStore.SourceClaim(ctx, sourceURI)
	if err != nil {
		return SourcePolicyClaim{}, nil, policyCatalogReadError(ctx, err)
	}
	var expected *SourcePolicyClaim
	var authorized []AccessRule
	var priorDocument, priorRevision shoal.ID
	if claimed {
		allowed, ruleErr := sourceClaimAllowsMutation(
			existingClaim, selectedRule, decision, now)
		if ruleErr != nil {
			return SourcePolicyClaim{}, nil, ruleErr
		}
		if !allowed {
			return SourcePolicyClaim{}, nil, auth.ObjectNotFound()
		}
		expected = &existingClaim
		authorized = append(authorized, existingClaim.Rule)
		if existingClaim.Pending && existingClaim.PreviousRule != nil {
			authorized = append(authorized, *existingClaim.PreviousRule)
		}
	} else {
		legacy, err := c.authorizeLegacySource(
			ctx, sourceURI, decision, now)
		if err != nil {
			return SourcePolicyClaim{}, nil, err
		}
		if legacy != nil {
			authorized = append(authorized, legacy.Rule)
			priorDocument = legacy.DocumentID
			priorRevision = legacy.RevisionID
		}
	}
	// Before anything is written, the registration this ingest would
	// replace must be one the principal is authorized under. The claim
	// above normally shows that, but not for a pending claim with no
	// previous rule, or for a catalog that diverged from its base (a
	// registration with no claim and no base document). So this looks the
	// registration up by the document identity the Explorer derives from
	// the URI. A refused principal therefore writes nothing to the base and
	// leaves no claim behind (#665).
	documentID := explorer.SourceDocumentID(sourceURI)
	replaced, err := c.authorizeReplacedRegistration(
		ctx, decision, now, documentID, authorized)
	if err != nil {
		return SourcePolicyClaim{}, nil, err
	}
	// Every rule this ingest replaces has now authorized the principal, so
	// a narrowing of any of them is audited (and the reason policy applied)
	// before the claim is swapped or the base written (#630).
	if replaced != nil {
		authorized = appendDistinctRule(authorized, replaced.Rule)
		priorDocument = documentID
		priorRevision = replaced.RevisionID
	}
	declassification, err := c.declassificationIntent(
		ctx, decision, now, sourceURI, selectedRule, authorized,
		priorDocument, priorRevision)
	if err != nil {
		return SourcePolicyClaim{}, nil, err
	}
	claim, err := c.policyStore.CompareAndSwapSourceClaim(
		ctx, sourceURI, expected, selectedRule)
	if err == nil {
		return claim, declassification, nil
	}
	if !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		return SourcePolicyClaim{}, nil, policyCatalogWriteError(ctx, err)
	}
	latest, ok, readErr := c.policyStore.SourceClaim(ctx, sourceURI)
	if readErr != nil {
		return SourcePolicyClaim{}, nil, policyCatalogReadError(ctx, readErr)
	}
	if ok {
		allowed, ruleErr := sourceClaimAllowsMutation(
			latest, selectedRule, decision, now)
		if ruleErr != nil {
			return SourcePolicyClaim{}, nil, ruleErr
		}
		if !allowed {
			return SourcePolicyClaim{}, nil, auth.ObjectNotFound()
		}
	}
	return SourcePolicyClaim{}, nil, policyCatalogWriteError(ctx, err)
}

func sourceClaimAllowsMutation(
	claim SourcePolicyClaim,
	selectedRule AccessRule,
	decision auth.Decision,
	now time.Time,
) (bool, error) {
	if claim.Pending && !claim.Rule.equal(selectedRule) {
		return false, nil
	}
	allowed, err := ruleAllows(
		claim.Rule, decision, auth.OperationIngest, now)
	if err != nil || !allowed {
		return allowed, err
	}
	if claim.Pending && claim.PreviousRule != nil {
		return ruleAllows(
			*claim.PreviousRule, decision, auth.OperationIngest, now)
	}
	return true, nil
}

// authorizeLegacySource authorizes a mutation of an unclaimed source URI. It
// returns the current registration of the document already stored under the
// URI, or nil when there is none.
func (c *Client) authorizeLegacySource(
	ctx context.Context,
	sourceURI string,
	decision auth.Decision,
	now time.Time,
) (*RevisionRegistration, error) {
	summaries, err := c.base.Documents(ctx)
	if err != nil {
		return nil, err
	}
	var documentID shoal.ID
	found := false
	for _, summary := range summaries {
		if summary.SourceURI != sourceURI {
			continue
		}
		if err := validateSummary(summary); err != nil {
			return nil, inconsistentBase()
		}
		if found && summary.Document.ID != documentID {
			return nil, inconsistentBase()
		}
		documentID = summary.Document.ID
		found = true
	}
	if !found {
		return nil, nil
	}
	currentRevisions, err := c.resolveCurrentRevisions(
		ctx, []shoal.ID{documentID})
	if err != nil {
		return nil, err
	}
	registration, ok := currentRevisions[documentID]
	if !ok {
		return nil, catalogUnavailable()
	}
	for _, summary := range summaries {
		if summary.Document.ID == documentID &&
			registration.RevisionID != summary.Revision.ID {
			return nil, catalogUnavailable()
		}
	}
	allowed, err := ruleAllows(
		registration.Rule, decision, auth.OperationIngest, now)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, auth.ObjectNotFound()
	}
	return &registration, nil
}

// authorizeReplacedRegistration runs before anything is written, against the
// document the Explorer derives from the source URI (Ingest then refuses a
// base that wrote any other document). Replacing the document's current
// registration requires authorization under that registration's rule. The
// claim-time check normally covered it (it authorized under the claim's rule, a pending
// claim's previous rule, or a legacy document's registration). When it did
// not, this check is the first authorization under that rule, and a principal
// outside it gets NotFound, as at claim time (#665). That happens when the
// catalog and base diverged: a registration exists for a URI with no claim
// and no base document. It also happens when a pending claim recorded no
// previous rule because it was created over an unclaimed, registered
// document.
func (c *Client) authorizeReplacedRegistration(
	ctx context.Context,
	decision auth.Decision,
	now time.Time,
	documentID shoal.ID,
	claimAuthorized []AccessRule,
) (*RevisionRegistration, error) {
	current, ok, err := c.policyStore.CurrentRevision(ctx, documentID)
	if err != nil {
		return nil, policyCatalogReadError(ctx, err)
	}
	if !ok {
		return nil, nil
	}
	for _, authorized := range claimAuthorized {
		if authorized.equal(current.Rule) {
			return &current, nil
		}
	}
	allowed, err := ruleAllows(
		current.Rule, decision, auth.OperationIngest, now)
	if err != nil {
		return nil, err
	}
	if !allowed {
		return nil, auth.ObjectNotFound()
	}
	return &current, nil
}

func (c *Client) begin(
	ctx context.Context,
	operation auth.Operation,
) (auth.Decision, auth.GenerationGuard, time.Time, error) {
	if err := contextFailure(ctx); err != nil {
		return auth.Decision{}, auth.GenerationGuard{}, time.Time{}, err
	}
	decision, err := c.resolver.Resolve(ctx)
	if err != nil {
		return auth.Decision{}, auth.GenerationGuard{}, time.Time{},
			resolverFailure(ctx, err)
	}
	now := c.clock()
	if now.IsZero() {
		return auth.Decision{}, auth.GenerationGuard{}, time.Time{},
			authorizationDenied()
	}
	if err := decision.Authorize(operation, auth.ResourceRequest{
		AuthorizationDomain: decision.AuthorizationDomain(),
	}, now); err != nil {
		if contextErr := contextFailure(ctx); contextErr != nil {
			return auth.Decision{}, auth.GenerationGuard{}, time.Time{}, contextErr
		}
		return auth.Decision{}, auth.GenerationGuard{}, time.Time{},
			authorizationDenied()
	}
	guard, err := auth.NewGenerationGuard(decision, c.generationReader)
	if err != nil {
		return auth.Decision{}, auth.GenerationGuard{}, time.Time{},
			authorizationDenied()
	}
	if err := guard.Check(ctx); err != nil {
		return auth.Decision{}, auth.GenerationGuard{}, time.Time{}, err
	}
	return decision, guard, now, nil
}

func (c *Client) beginOneOf(
	ctx context.Context,
	operations ...auth.Operation,
) (
	auth.Decision,
	auth.GenerationGuard,
	time.Time,
	auth.Operation,
	error,
) {
	if err := contextFailure(ctx); err != nil {
		return auth.Decision{}, auth.GenerationGuard{}, time.Time{}, "", err
	}
	if len(operations) == 0 {
		return auth.Decision{}, auth.GenerationGuard{}, time.Time{}, "",
			authorizationDenied()
	}
	decision, err := c.resolver.Resolve(ctx)
	if err != nil {
		return auth.Decision{}, auth.GenerationGuard{}, time.Time{}, "",
			resolverFailure(ctx, err)
	}
	now := c.clock()
	if now.IsZero() {
		return auth.Decision{}, auth.GenerationGuard{}, time.Time{}, "",
			authorizationDenied()
	}
	request := auth.ResourceRequest{
		AuthorizationDomain: decision.AuthorizationDomain(),
	}
	var selected auth.Operation
	for _, operation := range operations {
		if err := operation.Validate(); err != nil {
			return auth.Decision{}, auth.GenerationGuard{}, time.Time{}, "", err
		}
		if err := decision.Authorize(operation, request, now); err == nil {
			selected = operation
			break
		}
	}
	if selected == "" {
		if contextErr := contextFailure(ctx); contextErr != nil {
			return auth.Decision{}, auth.GenerationGuard{}, time.Time{}, "", contextErr
		}
		return auth.Decision{}, auth.GenerationGuard{}, time.Time{}, "",
			authorizationDenied()
	}
	guard, err := auth.NewGenerationGuard(decision, c.generationReader)
	if err != nil {
		return auth.Decision{}, auth.GenerationGuard{}, time.Time{}, "",
			authorizationDenied()
	}
	if err := guard.Check(ctx); err != nil {
		return auth.Decision{}, auth.GenerationGuard{}, time.Time{}, "", err
	}
	return decision, guard, now, selected, nil
}

// beginAny authorizes a setup-time operation that is not tied to one specific
// operation. It still requires a live, in-domain credential and a policy
// generation guard, but accepts any operation the decision itself grants, so
// an action-only grant is not rejected the way pinning setup to Retrieve
// would reject it. A decision that grants nothing is denied.
func (c *Client) beginAny(ctx context.Context) (auth.GenerationGuard, error) {
	if err := contextFailure(ctx); err != nil {
		return auth.GenerationGuard{}, err
	}
	decision, err := c.resolver.Resolve(ctx)
	if err != nil {
		return auth.GenerationGuard{}, resolverFailure(ctx, err)
	}
	now := c.clock()
	if now.IsZero() {
		return auth.GenerationGuard{}, authorizationDenied()
	}
	request := auth.ResourceRequest{
		AuthorizationDomain: decision.AuthorizationDomain(),
	}
	granted := false
	for _, operation := range decision.AllowedOperations() {
		if err := decision.Authorize(operation, request, now); err == nil {
			granted = true
			break
		}
	}
	if !granted {
		if contextErr := contextFailure(ctx); contextErr != nil {
			return auth.GenerationGuard{}, contextErr
		}
		return auth.GenerationGuard{}, authorizationDenied()
	}
	guard, err := auth.NewGenerationGuard(decision, c.generationReader)
	if err != nil {
		return auth.GenerationGuard{}, authorizationDenied()
	}
	if err := guard.Check(ctx); err != nil {
		return auth.GenerationGuard{}, err
	}
	return guard, nil
}

func (c *Client) selectIngestRule(
	ctx context.Context,
	decision auth.Decision,
	source explorer.Source,
	now time.Time,
) (AccessRule, error) {
	policy, err := c.policySelector.SelectPolicy(ctx, decision, source)
	if err != nil {
		return AccessRule{}, policySelectionError(ctx, err)
	}
	// A source policy is never a label policy (#570). StaticPolicySelector
	// refuses one at construction; a host PolicySelectorFunc is checked here,
	// because a label policy selected as the source would anchor further
	// label policies and could stand in for a label grant.
	if auth.IsLabelPolicyID(policy.GrantPolicyID()) {
		return AccessRule{}, shoal.NewError(
			shoal.ErrorUnavailable,
			"trusted policy selection returned a policy in the label namespace "+
				auth.LabelPolicyNamespace,
		)
	}
	// The source policy alone must authorize ingest before labels are read,
	// so a caller outside the source learns nothing from a label refusal.
	if _, err := selectedPolicyRule(
		decision, auth.OperationIngest, policy, now); err != nil {
		return AccessRule{}, err
	}
	// Free-form ingest labels are enforced by conjoining one structured
	// policy per label into the document's rule (#570). They are parsed with
	// exactly the parser the base uses (parse.go), so the labels that reach
	// the rule are the labels stored on the document's nodes.
	labels, err := interaction.ParseVisibility(
		source.Metadata[interaction.PropertyVisibility])
	if err != nil {
		return AccessRule{}, err
	}
	// LabelRule refuses, never truncates, a label set over MaxLabelsPerRule or
	// the flattened term/byte bounds, and names the bound it hit.
	rule, err := LabelRule(policy, labels)
	if err != nil {
		return AccessRule{}, err
	}
	// The ingester must hold every label it writes: each label policy is a
	// component the decision has to authorize for ingest, so an unknown or
	// ungranted label fails closed.
	if err := rule.Authorize(decision, auth.OperationIngest, now); err != nil {
		return AccessRule{}, authorizationDenied()
	}
	return rule, nil
}

func (c *Client) selectEdgeRule(
	ctx context.Context,
	decision auth.Decision,
	edge graph.Edge,
	now time.Time,
) (AccessRule, error) {
	policy, err := c.edgePolicySelector.SelectEdgePolicy(ctx, decision, edge)
	if err != nil {
		return AccessRule{}, policySelectionError(ctx, err)
	}
	// An edge rule is never a label policy alone (#570), as a document's
	// source policy is not (selectIngestRule). A label policy stands for one
	// label conjoined onto its source's policy; a rule of it alone would let
	// a label grant stand in for the source grant, and would break the
	// premise the dispatch confinement check relies on (label rules only
	// narrow their source).
	if auth.IsLabelPolicyID(policy.GrantPolicyID()) {
		return AccessRule{}, shoal.NewError(
			shoal.ErrorUnavailable,
			"trusted edge policy selection returned a policy in the label namespace "+
				auth.LabelPolicyNamespace,
		)
	}
	return selectedPolicyRule(decision, auth.OperationConnect, policy, now)
}

func selectedPolicyRule(
	decision auth.Decision,
	operation auth.Operation,
	policy auth.Policy,
	now time.Time,
) (AccessRule, error) {
	if policy.Epoch() != decision.PolicyGeneration() {
		return AccessRule{}, authorizationDenied()
	}
	rule, err := NewAccessRule(policy)
	if err != nil {
		return AccessRule{}, shoal.NewError(
			shoal.ErrorUnavailable, "trusted policy selection is invalid")
	}
	if err := rule.Authorize(decision, operation, now); err != nil {
		return AccessRule{}, authorizationDenied()
	}
	return rule, nil
}

func (c *Client) intrinsicEdges(
	ctx context.Context,
	nodeIDs []shoal.ID,
) ([]graph.Edge, error) {
	neighborhood, err := c.base.Neighborhood(ctx, explorer.NeighborhoodRequest{
		NodeIDs: append([]shoal.ID(nil), nodeIDs...),
		Depth:   1,
		EdgeTypes: []string{
			"contains",
		},
	})
	if err != nil {
		return nil, err
	}
	nodes := make(map[shoal.ID]struct{}, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		nodes[nodeID] = struct{}{}
	}
	edges := make([]graph.Edge, 0, len(neighborhood.Edges))
	for _, edge := range neighborhood.Edges {
		if edge.Type != "contains" {
			continue
		}
		if _, ok := nodes[edge.From]; !ok {
			continue
		}
		if _, ok := nodes[edge.To]; !ok {
			continue
		}
		edges = append(edges, cloneGraphEdge(edge))
	}
	sort.Slice(edges, func(left, right int) bool {
		return shoal.CompareID(edges[left].ID, edges[right].ID) < 0
	})
	return edges, nil
}

func exactRevisionIsCurrent(
	summaries []explorer.DocumentSummary,
	documentID, revisionID shoal.ID,
) (bool, error) {
	found := false
	current := false
	for _, summary := range summaries {
		if summary.Document.ID != documentID {
			continue
		}
		if found {
			return false, inconsistentBase()
		}
		found = true
		current = summary.Revision.ID == revisionID &&
			summary.Document.RevisionID == revisionID
	}
	if !found {
		return false, inconsistentBase()
	}
	return current, nil
}

func policySelectionError(ctx context.Context, err error) error {
	if contextErr := contextFailure(ctx); contextErr != nil {
		return contextErr
	}
	if shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		return authorizationDenied()
	}
	return shoal.NewError(
		shoal.ErrorUnavailable, "trusted policy selection unavailable")
}

func resolverFailure(ctx context.Context, err error) error {
	if contextErr := contextFailure(ctx); contextErr != nil {
		return contextErr
	}
	var shoalErr *shoal.Error
	if !errors.As(err, &shoalErr) || shoalErr == nil {
		return shoal.NewError(
			shoal.ErrorUnavailable, "authorization resolution unavailable")
	}
	switch shoalErr.Code {
	case shoal.ErrorCanceled:
		return shoal.NewError(shoal.ErrorCanceled, "operation canceled")
	case shoal.ErrorDeadline:
		return shoal.NewError(
			shoal.ErrorDeadline, "operation deadline exceeded")
	case shoal.ErrorUnavailable:
		return shoal.NewError(
			shoal.ErrorUnavailable, "authorization resolution unavailable")
	case shoal.ErrorInternal:
		return shoal.NewError(
			shoal.ErrorInternal, "authorization resolution failed")
	case shoal.ErrorUnauthorized, shoal.ErrorNotFound, shoal.ErrorInvalidArgument:
		return authorizationDenied()
	default:
		return shoal.NewError(
			shoal.ErrorUnavailable, "authorization resolution unavailable")
	}
}

func policyCatalogReadError(ctx context.Context, _ error) error {
	if contextErr := contextFailure(ctx); contextErr != nil {
		return contextErr
	}
	return catalogUnavailable()
}

func policyCatalogWriteError(ctx context.Context, err error) error {
	if contextErr := contextFailure(ctx); contextErr != nil {
		return contextErr
	}
	// A record the catalog refused before changing anything is the caller's
	// input, and retrying it cannot succeed: report it as such.
	if errors.Is(err, errPolicyRecordTooLarge) {
		return shoal.NewError(shoal.ErrorInvalidArgument,
			"this write is too large for the authorization policy catalog")
	}
	if shoal.IsErrorCode(err, shoal.ErrorConflict) {
		return catalogConflict()
	}
	return catalogUnavailable()
}

func authorizationDenied() error {
	return shoal.NewError(shoal.ErrorUnauthorized, "authorization denied")
}

func inconsistentBase() error {
	return shoal.NewError(
		shoal.ErrorInternal, "underlying Explorer returned inconsistent data")
}

func dependencyRequired(name string) error {
	return shoal.NewError(shoal.ErrorInvalidArgument, name+" is required")
}

func isNilDependency(value any) bool {
	if value == nil {
		return true
	}
	reflection := reflect.ValueOf(value)
	switch reflection.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return reflection.IsNil()
	default:
		return false
	}
}

func contextFailure(ctx context.Context) error {
	if ctx == nil {
		return shoal.NewError(shoal.ErrorInvalidArgument, "context is required")
	}
	switch err := ctx.Err(); {
	case errors.Is(err, context.Canceled):
		return shoal.WrapError(shoal.ErrorCanceled, "operation canceled", err)
	case errors.Is(err, context.DeadlineExceeded):
		return shoal.WrapError(
			shoal.ErrorDeadline, "operation deadline exceeded", err)
	default:
		return nil
	}
}

func rulesShareDomain(left, right AccessRule) bool {
	if len(left.policies) == 0 || len(right.policies) == 0 {
		return false
	}
	return bytes.Equal(
		left.policies[0].AuthorizationDomain(),
		right.policies[0].AuthorizationDomain(),
	)
}

var (
	_ explorer.Client        = (*Client)(nil)
	_ explorer.BoundedClient = (*Client)(nil)
)
