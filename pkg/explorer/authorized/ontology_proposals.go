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

package authorized

import (
	"context"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func (c *Client) OntologyProposals(
	ctx context.Context,
) ([]ontology.GovernedProposal, error) {
	store, err := c.ontologyProposalStore()
	if err != nil {
		return nil, err
	}
	decision, guard, now, err := c.begin(ctx, auth.OperationRead)
	if err != nil {
		return nil, err
	}
	proposals, err := store.OntologyProposals(ctx)
	if err != nil {
		return nil, directBaseError(err)
	}
	visible := make([]ontology.GovernedProposal, 0, len(proposals))
	for _, proposal := range proposals {
		allowed, err := c.ontologyProposalEvidenceAllows(
			ctx, proposal, decision, auth.OperationRead, now, true)
		if err != nil {
			return nil, err
		}
		if allowed {
			visible = append(visible, proposal)
		}
	}
	if err := guard.Check(ctx); err != nil {
		return nil, err
	}
	return visible, nil
}

func (c *Client) CreateOntologyProposal(
	ctx context.Context,
	proposal ontology.GovernedProposal,
	baseVersion ontology.OntologyVersion,
) error {
	store, err := c.ontologyProposalStore()
	if err != nil {
		return err
	}

	// This operation check is load-bearing; TestOntologyProposalEndpointDistinguishesAuthorizationDenial
	// pins that proposal mutations require write authority and surface a
	// governance 401 without a bearer challenge when the caller lacks it.
	decision, guard, now, err := c.begin(ctx, auth.OperationIngest)
	if err != nil {
		return err
	}
	if err := proposal.Validate(); err != nil {
		return err
	}
	// A new proposal's evidence must match the catalog exactly (stored
	// false); see ontologyEvidenceAllows.
	allowed, err := c.ontologyProposalEvidenceAllows(
		ctx, proposal, decision, auth.OperationIngest, now, false)
	if err != nil {
		return err
	}
	if !allowed {
		return auth.ObjectNotFound()
	}
	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()
	if err := guard.Check(ctx); err != nil {
		return err
	}
	if err := store.CreateOntologyProposal(ctx, proposal, baseVersion); err != nil {
		return directBaseError(err)
	}
	if err := guard.Check(ctx); err != nil {
		return explorer.MarkIndeterminateCommit(err)
	}
	return nil
}

func (c *Client) OntologyProposalMutationState(
	ctx context.Context,
	configured ontology.OntologyVersion,
	proposalID shoal.ID,
) (explorer.OntologyProposalMutationState, error) {
	store, ok := c.ontologyProposals.(explorer.OntologyProposalMutationStateProvider)
	if !ok {
		return explorer.OntologyProposalMutationState{}, shoal.NewError(
			shoal.ErrorUnavailable,
			"workspace capability \"ontology proposal mutation state\" is unavailable",
		)
	}
	decision, guard, now, err := c.begin(ctx, auth.OperationIngest)
	if err != nil {
		return explorer.OntologyProposalMutationState{}, err
	}
	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()
	if err := guard.Check(ctx); err != nil {
		return explorer.OntologyProposalMutationState{}, err
	}
	if proposalID != "" {
		evidenceStore, ok := c.ontologyProposals.(explorer.OntologyProposalEvidenceProvider)
		if !ok {
			return explorer.OntologyProposalMutationState{}, shoal.NewError(
				shoal.ErrorUnavailable,
				"workspace capability \"ontology proposal evidence\" is unavailable",
			)
		}
		evidence, found, err := evidenceStore.OntologyProposalEvidence(ctx, proposalID)
		if err != nil {
			return explorer.OntologyProposalMutationState{}, directBaseError(err)
		}
		if !found {
			return explorer.OntologyProposalMutationState{}, nil
		}
		for _, item := range evidence {
			allowed, evidenceErr := c.ontologyEvidenceAllows(
				ctx, item, decision, auth.OperationIngest, now, true)
			if evidenceErr != nil {
				return explorer.OntologyProposalMutationState{}, evidenceErr
			}
			if !allowed {
				return explorer.OntologyProposalMutationState{}, nil
			}
		}
		if err := guard.Check(ctx); err != nil {
			return explorer.OntologyProposalMutationState{}, err
		}
	}
	state, err := store.OntologyProposalMutationState(ctx, configured, proposalID)
	if err != nil {
		return explorer.OntologyProposalMutationState{}, directBaseError(err)
	}
	if err := guard.Check(ctx); err != nil {
		return explorer.OntologyProposalMutationState{}, err
	}
	return state, nil
}

func (c *Client) OntologyActiveState(
	ctx context.Context,
	configured ontology.OntologyVersion,
) (ontology.OntologyVersion, error) {
	provider, ok := c.ontologyProposals.(explorer.OntologyActiveStateProvider)
	if !ok {
		return ontology.OntologyVersion{}, shoal.NewError(
			shoal.ErrorUnavailable,
			"workspace capability \"active ontology state\" is unavailable",
		)
	}
	_, guard, _, err := c.begin(ctx, auth.OperationRead)
	if err != nil {
		return ontology.OntologyVersion{}, err
	}
	active, err := provider.OntologyActiveState(ctx, configured)
	if err != nil {
		return ontology.OntologyVersion{}, directBaseError(err)
	}
	if err := guard.Check(ctx); err != nil {
		return ontology.OntologyVersion{}, err
	}
	return active, nil
}

// PublishedOntologyCatalog exposes only the caller-authorized published
// version chain needed by workspace settings. Proposal bodies remain protected
// by OntologyProposals and its generic read permission.
func (c *Client) PublishedOntologyCatalog(
	ctx context.Context,
	configured ontology.OntologyVersion,
) (ontology.PublishedCatalog, error) {
	decision, guard, now, operation, err := c.beginOneOf(
		ctx,
		auth.OperationWorkspaceSettingsRead,
		auth.OperationWorkspaceSettingsWrite,
	)
	if err != nil {
		return ontology.PublishedCatalog{}, err
	}
	return c.publishedOntologyCatalog(
		ctx, configured, decision, guard, now, operation, nil)
}

// AuthorizePublishedOntology checks one selected published identity under the
// exact operation whose decision is being narrowed, without exposing the
// catalog or requiring workspace-settings management authority.
func (c *Client) AuthorizePublishedOntology(
	ctx context.Context,
	configured ontology.OntologyVersion,
	identity ontology.OntologyIdentity,
	operation auth.Operation,
) error {
	if err := identity.Validate(); err != nil {
		return err
	}
	decision, guard, now, err := c.begin(ctx, operation)
	if err != nil {
		return err
	}
	catalog, err := c.publishedOntologyCatalog(
		ctx, configured, decision, guard, now, operation, &identity)
	if err != nil {
		return err
	}
	if !catalog.Contains(identity) {
		return auth.ObjectNotFound()
	}
	return nil
}

func (c *Client) publishedOntologyCatalog(
	ctx context.Context,
	configured ontology.OntologyVersion,
	decision auth.Decision,
	guard auth.GenerationGuard,
	now time.Time,
	operation auth.Operation,
	through *ontology.OntologyIdentity,
) (ontology.PublishedCatalog, error) {
	store, err := c.ontologyProposalStore()
	if err != nil {
		return ontology.PublishedCatalog{}, err
	}
	proposals, err := store.OntologyProposals(ctx)
	if err != nil {
		return ontology.PublishedCatalog{}, directBaseError(err)
	}
	if len(proposals) > int(explorer.MaxOntologyProposals) {
		return ontology.PublishedCatalog{}, shoal.NewError(
			shoal.ErrorUnavailable, "ontology proposals exceed the corpus bound")
	}
	catalog, err := explorer.NewPublishedOntologyCatalog(configured, proposals)
	if err != nil {
		return ontology.PublishedCatalog{}, err
	}
	versions := catalog.Versions()
	if through != nil {
		if !catalog.Contains(*through) {
			return ontology.PublishedCatalog{}, auth.ObjectNotFound()
		}
		for index, version := range versions {
			identity, err := ontology.NewOntologyIdentity(version)
			if err != nil {
				return ontology.PublishedCatalog{}, err
			}
			if identity == *through {
				versions = versions[:index+1]
				break
			}
		}
	}
	reachable := make(map[shoal.ID]shoal.ID, len(versions)-1)
	for index := 1; index < len(versions); index++ {
		reachable[versions[index-1].ID()] = versions[index].ID()
	}
	for _, proposal := range proposals {
		if proposal.State() != ontology.ProposalPublished ||
			proposal.Schema().ID() != configured.Schema().ID() {
			continue
		}
		baseID, hasBase := proposal.BaseVersionID()
		if !hasBase || reachable[baseID] != proposal.ProposedVersion().ID() {
			continue
		}
		allowed, err := c.ontologyProposalEvidenceAllows(
			ctx, proposal, decision, operation, now, true)
		if err != nil {
			return ontology.PublishedCatalog{}, err
		}
		if !allowed {
			return ontology.PublishedCatalog{}, auth.ObjectNotFound()
		}
	}
	if err := guard.Check(ctx); err != nil {
		return ontology.PublishedCatalog{}, err
	}
	freshNow := c.clock()
	if freshNow.IsZero() ||
		decision.Authorize(
			operation,
			auth.ResourceRequest{
				AuthorizationDomain: decision.AuthorizationDomain(),
			},
			freshNow,
		) != nil {
		return ontology.PublishedCatalog{}, authorizationDenied()
	}
	return catalog, nil
}

func (c *Client) TransitionOntologyProposal(
	ctx context.Context,
	proposalID shoal.ID,
	next ontology.ProposalState,
	actor, note string,
	at time.Time,
) (ontology.GovernedProposal, error) {
	return c.transitionOntologyProposal(ctx, proposalID, next, actor, note, at, nil)
}

// TransitionOntologyProposalWithLimits keeps bounded transitions under the
// same mutation and evidence authority as the ordinary domain operation.
func (c *Client) TransitionOntologyProposalWithLimits(
	ctx context.Context,
	proposalID shoal.ID,
	next ontology.ProposalState,
	actor, note string,
	at time.Time,
	limits explorer.OntologyProjectionLimits,
) (ontology.GovernedProposal, error) {
	return c.transitionOntologyProposal(ctx, proposalID, next, actor, note, at, &limits)
}

func (c *Client) transitionOntologyProposal(
	ctx context.Context,
	proposalID shoal.ID,
	next ontology.ProposalState,
	actor, note string,
	at time.Time,
	limits *explorer.OntologyProjectionLimits,
) (ontology.GovernedProposal, error) {
	store, err := c.ontologyProposalStore()
	if err != nil {
		return ontology.GovernedProposal{}, err
	}
	// This operation check is load-bearing; TestOntologyProposalEndpointDistinguishesAuthorizationDenial
	// pins that proposal mutations require write authority and surface a
	// governance 401 without a bearer challenge when the caller lacks it.
	decision, guard, now, err := c.begin(ctx, auth.OperationIngest)
	if err != nil {
		return ontology.GovernedProposal{}, err
	}
	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()
	if err := guard.Check(ctx); err != nil {
		return ontology.GovernedProposal{}, err
	}
	evidenceStore, ok := c.ontologyProposals.(explorer.OntologyProposalEvidenceProvider)
	if !ok {
		return ontology.GovernedProposal{}, shoal.NewError(
			shoal.ErrorUnavailable,
			"workspace capability \"ontology proposal evidence\" is unavailable",
		)
	}
	evidence, found, err := evidenceStore.OntologyProposalEvidence(ctx, proposalID)
	if err != nil {
		return ontology.GovernedProposal{}, directBaseError(err)
	}
	if !found {
		return ontology.GovernedProposal{}, auth.ObjectNotFound()
	}
	for _, item := range evidence {
		allowed, evidenceErr := c.ontologyEvidenceAllows(
			ctx, item, decision, auth.OperationIngest, now, true)
		if evidenceErr != nil {
			return ontology.GovernedProposal{}, evidenceErr
		}
		if !allowed {
			return ontology.GovernedProposal{}, auth.ObjectNotFound()
		}
	}
	if err := guard.Check(ctx); err != nil {
		return ontology.GovernedProposal{}, err
	}
	var proposal ontology.GovernedProposal
	if limits == nil {
		proposal, err = store.TransitionOntologyProposal(
			ctx, proposalID, next, actor, note, at)
	} else {
		bounded, ok := store.(explorer.OntologyProposalBoundedTransitionStore)
		if !ok {
			return ontology.GovernedProposal{}, shoal.NewError(
				shoal.ErrorUnavailable,
				"workspace capability \"bounded ontology proposal transitions\" is unavailable",
			)
		}
		proposal, err = bounded.TransitionOntologyProposalWithLimits(
			ctx, proposalID, next, actor, note, at, *limits)
	}
	if err != nil {
		return ontology.GovernedProposal{}, directBaseError(err)
	}
	if err := guard.Check(ctx); err != nil {
		return ontology.GovernedProposal{}, explorer.MarkIndeterminateCommit(err)
	}
	return proposal, nil
}

func (c *Client) ontologyProposalStore() (explorer.OntologyProposalStore, error) {
	if isNilDependency(c.ontologyProposals) {
		return nil, shoal.NewError(
			shoal.ErrorUnavailable, "workspace capability \"ontology proposals\" is unavailable")
	}
	return c.ontologyProposals, nil
}

func (c *Client) ontologyProposalEvidenceAllows(
	ctx context.Context,
	proposal ontology.GovernedProposal,
	decision auth.Decision,
	operation auth.Operation,
	now time.Time,
	stored bool,
) (bool, error) {
	if err := proposal.Validate(); err != nil {
		return false, inconsistentBase()
	}
	for _, morphism := range proposal.Morphisms() {
		for _, evidence := range morphism.Evidence() {
			allowed, err := c.ontologyEvidenceAllows(
				ctx, evidence, decision, operation, now, stored)
			if err != nil || !allowed {
				return allowed, err
			}
		}
	}
	return true, nil
}

// ontologyEvidenceAllows authorizes one piece of proposal evidence. stored
// marks the re-validation of evidence a proposal already holds: an
// extracted node or relation in its graph path may then be in the rendering
// it had before shared extraction content was canonical (#700), so it is
// compared in canonical form. That evidence matched the catalog exactly when
// it was proposed. New evidence (stored false) must match the catalog's
// canonical content exactly, or a proposer could store and serve any kind,
// labels or properties on an extracted node.
func (c *Client) ontologyEvidenceAllows(
	ctx context.Context,
	evidence ontology.EvidenceRef,
	decision auth.Decision,
	operation auth.Operation,
	now time.Time,
	stored bool,
) (bool, error) {
	if err := evidence.Validate(); err != nil {
		return false, inconsistentBase()
	}
	citation := evidence.Citation()
	if citation.SectionID == "" && citation.SpanID == "" {
		return false, nil
	}
	registration, ok, err := c.policyStore.Revision(
		ctx, citation.DocumentID, citation.RevisionID)
	if err != nil {
		return false, policyCatalogReadError(ctx, err)
	}
	if !ok {
		return false, nil
	}
	allowed, err := c.revisionAllows(
		ctx, registration, decision, operation, now)
	if err != nil || !allowed {
		return allowed, err
	}
	view, err := c.base.Document(ctx, citation.DocumentID, citation.RevisionID)
	if err != nil {
		return false, directBaseError(err)
	}
	if err := verifyDocumentViewRegistration(view, registration); err != nil {
		return false, err
	}
	canonical, err := buildCanonicalRetrievalDocument(view, registration)
	if err != nil {
		return false, inconsistentBase()
	}
	if citation.SectionID != "" {
		section, ok := canonical.sections[citation.SectionID]
		if !ok || section.Range.Start.Offset > citation.Range.Start.Offset ||
			citation.Range.End.Offset > section.Range.End.Offset {
			return false, nil
		}
	}
	if citation.SpanID != "" {
		span, ok := canonical.spans[citation.SpanID]
		if !ok || citation.SectionID != "" && span.SectionID != citation.SectionID ||
			span.Range.Start.Offset > citation.Range.Start.Offset ||
			citation.Range.End.Offset > span.Range.End.Offset {
			return false, nil
		}
	}
	resolver, ok := c.ontologyProposals.(explorer.OntologyEvidenceCitationResolver)
	if !ok {
		return false, shoal.NewError(
			shoal.ErrorUnavailable,
			"workspace capability \"ontology evidence citation\" is unavailable",
		)
	}
	quote, err := resolver.ResolveOntologyEvidenceCitation(ctx, citation)
	if err != nil {
		if shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) ||
			shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			return false, nil
		}
		return false, directBaseError(err)
	}
	if evidence.Quote() != "" && quote != evidence.Quote() {
		return false, nil
	}
	path, hasPath := evidence.Path()
	if !hasPath {
		return true, nil
	}
	nodeIDs := make([]shoal.ID, len(path.Nodes))
	for index, node := range path.Nodes {
		nodeIDs[index] = node.ID
	}
	nodes, err := c.resolveNodes(ctx, nodeIDs, readerFor(decision, operation, now))
	if err != nil {
		return false, err
	}
	for _, node := range path.Nodes {
		nodeRegistration, ok := nodes[node.ID]
		if !ok {
			return false, nil
		}
		allowed, err := ruleAllows(
			nodeRegistration.Rule, decision, operation, now)
		if err != nil || !allowed {
			return allowed, err
		}
	}
	canonicalNodes, err := c.canonicalRegisteredNodes(ctx, nodes)
	if err != nil {
		if shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			return false, nil
		}
		return false, err
	}
	for _, node := range path.Nodes {
		// Load-bearing: TestProposalEvidenceIsExactAtCreateAndCanonicalWhenStored.
		evidenceNode := node
		if stored {
			evidenceNode, _ = explorer.CanonicalExtractedNode(node)
		}
		if !graphNodesEqual(canonicalNodes[node.ID], evidenceNode) {
			return false, nil
		}
	}
	for _, edge := range path.Edges {
		edgeRegistration, ok, err := c.policyStore.Edge(ctx, edge.ID)
		if err != nil {
			return false, policyCatalogReadError(ctx, err)
		}
		evidenceEdge := edge
		if stored {
			evidenceEdge, _ = explorer.CanonicalExtractedRelation(edge)
		}
		if !ok || !graphEdgesEqual(edgeRegistration.Edge, evidenceEdge) {
			return false, nil
		}
		allowed, err := c.edgeAllows(
			ctx, edgeRegistration, decision, operation, now)
		if err != nil || !allowed {
			return allowed, err
		}
	}
	return true, nil
}
