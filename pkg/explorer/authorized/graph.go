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
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type canonicalDocumentIndexKey struct{}

func (c *Client) withCanonicalDocumentIndex(
	ctx context.Context,
) (context.Context, error) {
	if _, ok := ctx.Value(canonicalDocumentIndexKey{}).(map[shoal.ID]shoal.ID); ok {
		return ctx, nil
	}
	summaries, err := c.base.Documents(ctx)
	if err != nil {
		return nil, err
	}
	currentBase := make(map[shoal.ID]shoal.ID, len(summaries))
	for _, summary := range summaries {
		if err := validateSummary(summary); err != nil {
			return nil, inconsistentBase()
		}
		if _, duplicate := currentBase[summary.Document.ID]; duplicate {
			return nil, inconsistentBase()
		}
		currentBase[summary.Document.ID] = summary.Revision.ID
	}
	return context.WithValue(ctx, canonicalDocumentIndexKey{}, currentBase), nil
}

// Connect authorizes both current endpoints and stores only the trusted
// edge-local policy. Endpoint rules are re-evaluated dynamically on every use.
//
// The edge ID must be in the caller's application edge namespace
// (ApplicationEdgeID); any other ID is InvalidArgument, before anything is
// read. An ID outside it could collide with an edge the caller cannot read,
// and whether Connect then failed would disclose that edge, or through an
// intrinsic edge ID a hidden document at a guessed URI (#699).
func (c *Client) Connect(ctx context.Context, edge graph.Edge) error {
	if err := edge.Validate(); err != nil {
		return err
	}
	ownedEdge := cloneGraphEdge(edge)
	decision, guard, now, err := c.begin(ctx, auth.OperationConnect)
	if err != nil {
		return err
	}
	if err := requireApplicationEdgeNamespace(decision, ownedEdge.ID); err != nil {
		return err
	}

	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()
	lease, err := c.policyStore.AcquireMutation(ctx)
	if err != nil {
		return policyCatalogWriteError(ctx, err)
	}
	defer lease.Release()
	if err := guard.Check(ctx); err != nil {
		return err
	}
	from, err := c.authorizedNode(
		ctx, ownedEdge.From, decision, auth.OperationConnect, now)
	if err != nil {
		return err
	}
	to, err := c.authorizedNode(
		ctx, ownedEdge.To, decision, auth.OperationConnect, now)
	if err != nil {
		return err
	}
	if !rulesShareDomain(from.Rule, to.Rule) {
		return auth.ObjectNotFound()
	}
	edgeRule, err := c.selectEdgeRule(
		ctx, decision, cloneGraphEdge(ownedEdge), now)
	if err != nil {
		return err
	}
	if !rulesShareDomain(edgeRule, from.Rule) ||
		!rulesShareDomain(edgeRule, to.Rule) {
		return authorizationDenied()
	}
	if err := edgeRule.Authorize(
		decision, auth.OperationConnect, now); err != nil {
		return authorizationDenied()
	}
	if err := guard.Check(ctx); err != nil {
		return err
	}
	// The edge is bound to the revisions its endpoints belong to now, so a
	// later relabel of either cannot open it (#695).
	sources, err := connectSourceRevisions(from, to)
	if err != nil {
		return inconsistentBase()
	}
	registration := EdgeRegistration{
		Edge: ownedEdge, Rule: edgeRule, Kind: RegistrationApplication,
		SourceRevisions: sources, SourceRevisionsBound: true,
	}
	if err := c.policyStore.ReserveEdge(ctx, registration); err != nil {
		return policyCatalogWriteError(ctx, err)
	}
	if err := c.base.Connect(ctx, cloneGraphEdge(ownedEdge)); err != nil {
		if !explorer.IsIndeterminateCommit(err) {
			if rollbackErr := c.policyStore.RollbackEdgeReservation(
				context.WithoutCancel(ctx), registration,
			); rollbackErr != nil {
				return policyCatalogWriteError(ctx, rollbackErr)
			}
		}
		return directBaseError(err)
	}
	if err := c.policyStore.PutEdge(ctx, registration); err != nil {
		return policyCatalogWriteError(ctx, err)
	}
	return guard.Check(ctx)
}

// Neighborhood filters by current node and edge rules, then recomputes
// reachability so hidden intermediates cannot disclose or bridge.
func (c *Client) Neighborhood(
	ctx context.Context,
	request explorer.NeighborhoodRequest,
) (explorer.Neighborhood, error) {
	normalized, err := request.Normalize()
	if err != nil {
		return explorer.Neighborhood{}, err
	}
	decision, guard, now, err := c.begin(ctx, auth.OperationNeighborhood)
	if err != nil {
		return explorer.Neighborhood{}, err
	}
	for _, nodeID := range normalized.NodeIDs {
		if possibleProvenanceNodeID(nodeID) {
			continue
		}
		if _, err := c.authorizedNode(
			ctx, nodeID, decision, auth.OperationNeighborhood, now); err != nil {
			return explorer.Neighborhood{}, err
		}
	}
	raw, err := c.base.Neighborhood(ctx, normalized)
	if err != nil {
		return explorer.Neighborhood{}, directBaseError(err)
	}
	result, err := c.filterNeighborhood(
		ctx, raw, normalized, explorer.GraphDirectionBoth, decision, now, false,
		auth.OperationNeighborhood)
	if err != nil {
		return explorer.Neighborhood{}, err
	}
	result, err = c.applyOntologyLens(ctx, result, decision)
	if err != nil {
		return explorer.Neighborhood{}, err
	}
	if err := guard.Check(ctx); err != nil {
		return explorer.Neighborhood{}, err
	}
	return result, nil
}

func (c *Client) applyOntologyLens(
	ctx context.Context,
	result explorer.Neighborhood,
	decision auth.Decision,
) (explorer.Neighborhood, error) {
	selected, ok := decision.SelectedOntology()
	if !ok {
		return result, nil
	}
	interpreter := c.ontologyInterpreter
	if isNilDependency(interpreter) {
		for _, assertion := range result.Assertions {
			result.Interpretations = append(result.Interpretations,
				ontology.UnresolvedInterpretation(
					assertion, selected, "ontology interpretation is unavailable"))
		}
		return result, nil
	}
	interpretations, err := interpreter.InterpretAssertions(
		ctx, result.Assertions, selected)
	if err != nil {
		return explorer.Neighborhood{}, directBaseError(err)
	}
	result.Interpretations = interpretations
	return result, nil
}

func (c *Client) edgeAllows(
	ctx context.Context,
	registration EdgeRegistration,
	decision auth.Decision,
	operation auth.Operation,
	now time.Time,
) (bool, error) {
	reader := readerFor(decision, operation, now)
	effective, err := c.effectiveEdgeRegistrations(
		ctx, map[shoal.ID]EdgeRegistration{registration.Edge.ID: registration},
		reader)
	if err != nil {
		return false, err
	}
	registration, ok := effective[registration.Edge.ID]
	if !ok {
		return false, nil
	}
	allowed, err := ruleAllows(registration.Rule, decision, operation, now)
	if err != nil || !allowed {
		return allowed, err
	}
	resolved, err := c.resolveNodes(
		ctx, []shoal.ID{registration.Edge.From, registration.Edge.To}, reader)
	if err != nil {
		return false, err
	}
	return edgeEndpointsAllow(resolved, registration, decision, operation, now)
}

// registeredNodes holds the node registrations resolved for one page in a
// single batch lookup. Membership is the only presence signal, exactly as the
// boolean returned by PolicyStore.Node is: an identifier that is absent was not
// registered and therefore denies. A partial result therefore authorizes only
// the identifiers it does carry, and an empty one authorizes nothing.
type registeredNodes map[shoal.ID]NodeRegistration

type currentRevisions map[shoal.ID]RevisionRegistration

// resolveCurrentRevisions resolves distinct document identifiers in one
// policy-store round trip. Omitted registrations retain the point lookup's
// fail-closed !ok meaning, and results for unrequested identifiers are ignored.
func (c *Client) resolveCurrentRevisions(
	ctx context.Context,
	documentIDs []shoal.ID,
) (currentRevisions, error) {
	distinct := make([]shoal.ID, 0, len(documentIDs))
	requested := make(map[shoal.ID]struct{}, len(documentIDs))
	for _, documentID := range documentIDs {
		if _, duplicate := requested[documentID]; duplicate {
			continue
		}
		requested[documentID] = struct{}{}
		distinct = append(distinct, documentID)
	}
	if len(distinct) == 0 {
		return currentRevisions{}, nil
	}
	batch, err := c.policyStore.CurrentRevisions(ctx, distinct)
	if err != nil {
		return nil, policyCatalogReadError(ctx, err)
	}
	resolved := make(currentRevisions, len(distinct))
	for _, documentID := range distinct {
		registration, ok := batch[documentID]
		if !ok {
			continue
		}
		resolved[documentID] = registration
	}
	return resolved, nil
}

// resolveNodes collapses the identifiers it is given into a single policy-store
// round trip, or none at all when there is nothing to resolve, deduplicating
// first so the round trip carries distinct nodes rather than one entry per edge
// endpoint. Identifiers the store does not know are omitted from the result,
// preserving the fail-closed path a point lookup takes when it reports !ok.
// Registrations for identifiers that were not requested are discarded so a
// misbehaving store cannot widen a page. A shared extracted node is projected
// onto an owner reader can read (#700); a nil reader is a trusted,
// reader-free use.
func (c *Client) resolveNodes(
	ctx context.Context,
	nodeIDs []shoal.ID,
	reader *registrationReader,
) (registeredNodes, error) {
	distinct := make([]shoal.ID, 0, len(nodeIDs))
	requested := make(map[shoal.ID]struct{}, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		if _, duplicate := requested[nodeID]; duplicate {
			continue
		}
		requested[nodeID] = struct{}{}
		distinct = append(distinct, nodeID)
	}
	if len(distinct) == 0 {
		return registeredNodes{}, nil
	}
	batch, err := c.policyStore.Nodes(ctx, distinct)
	if err != nil {
		return nil, policyCatalogReadError(ctx, err)
	}
	resolved := make(registeredNodes, len(distinct))
	for _, nodeID := range distinct {
		registration, ok := batch[nodeID]
		if !ok {
			continue
		}
		resolved[nodeID] = registration
	}
	effective, err := c.effectiveNodeRegistrations(ctx, resolved, reader)
	if err != nil {
		return nil, err
	}
	return effective, nil
}

// registeredEdges holds the edge registrations resolved for one page in a
// single batch lookup, under the same rule as registeredNodes: membership is
// the only presence signal, so an identifier that is absent was not registered
// and denies.
type registeredEdges map[shoal.ID]EdgeRegistration

// resolveEdges collapses a page's candidate edges into one policy-store round
// trip under the same discipline as resolveNodes: deduplicate first, skip the
// round trip entirely when there is nothing to resolve, treat omission from the
// result as the fail-closed !ok path, and discard registrations for identifiers
// that were not requested. Shared relations are projected for reader as
// resolveNodes projects shared entities.
func (c *Client) resolveEdges(
	ctx context.Context,
	edgeIDs []shoal.ID,
	reader *registrationReader,
) (registeredEdges, error) {
	distinct := make([]shoal.ID, 0, len(edgeIDs))
	requested := make(map[shoal.ID]struct{}, len(edgeIDs))
	for _, edgeID := range edgeIDs {
		if _, duplicate := requested[edgeID]; duplicate {
			continue
		}
		requested[edgeID] = struct{}{}
		distinct = append(distinct, edgeID)
	}
	if len(distinct) == 0 {
		return registeredEdges{}, nil
	}
	batch, err := c.policyStore.Edges(ctx, distinct)
	if err != nil {
		return nil, policyCatalogReadError(ctx, err)
	}
	resolved := make(registeredEdges, len(distinct))
	for _, edgeID := range distinct {
		registration, ok := batch[edgeID]
		if !ok {
			continue
		}
		resolved[edgeID] = registration
	}
	effective, err := c.effectiveEdgeRegistrations(ctx, resolved, reader)
	if err != nil {
		return nil, err
	}
	return effective, nil
}

// edgeAllowsResolved authorizes one edge against endpoint registrations that
// were already batched for the page, issuing no further policy-store round
// trips.
func edgeAllowsResolved(
	resolved registeredNodes,
	registration EdgeRegistration,
	decision auth.Decision,
	operation auth.Operation,
	now time.Time,
) (bool, error) {
	allowed, err := ruleAllows(registration.Rule, decision, operation, now)
	if err != nil || !allowed {
		return allowed, err
	}
	return edgeEndpointsAllow(resolved, registration, decision, operation, now)
}

// edgeEndpointsAllow requires both endpoints to be present in the resolved page
// and allowed. An endpoint missing from resolved denies, exactly as a point
// lookup reporting !ok denies.
func edgeEndpointsAllow(
	resolved registeredNodes,
	registration EdgeRegistration,
	decision auth.Decision,
	operation auth.Operation,
	now time.Time,
) (bool, error) {
	from, ok := resolved[registration.Edge.From]
	if !ok {
		return false, nil
	}
	to, ok := resolved[registration.Edge.To]
	if !ok {
		return false, nil
	}
	// Load-bearing: TestAuthorizedLatentAssertionWithholdsUnauthorizedSource
	// pins that a derived assertion cannot reveal an unauthorized source
	// endpoint through an incoming graph read.
	fromAllowed, err := ruleAllows(from.Rule, decision, operation, now)
	if err != nil || !fromAllowed {
		return fromAllowed, err
	}
	// Load-bearing: TestAuthorizedLatentAssertionWithholdsUnauthorizedTarget
	// pins that a derived assertion cannot reveal an unauthorized target
	// endpoint through an outgoing graph read.
	toAllowed, err := ruleAllows(to.Rule, decision, operation, now)
	if err != nil || !toAllowed {
		return toAllowed, err
	}
	return true, nil
}

func possibleProvenanceNodeID(nodeID shoal.ID) bool {
	return strings.HasPrefix(string(nodeID), "producer_") ||
		ontology.IDNamespace(nodeID) == "assertion"
}

func (c *Client) authorizedNode(
	ctx context.Context,
	nodeID shoal.ID,
	decision auth.Decision,
	operation auth.Operation,
	now time.Time,
) (NodeRegistration, error) {
	registration, ok, err := c.policyStore.Node(ctx, nodeID)
	if err != nil {
		return NodeRegistration{}, policyCatalogReadError(ctx, err)
	}
	if !ok {
		return NodeRegistration{}, auth.ObjectNotFound()
	}
	effective, err := c.effectiveNodeRegistrations(
		ctx, map[shoal.ID]NodeRegistration{nodeID: registration},
		readerFor(decision, operation, now))
	if err != nil {
		return NodeRegistration{}, err
	}
	registration, ok = effective[nodeID]
	if !ok {
		return NodeRegistration{}, auth.ObjectNotFound()
	}
	allowed, err := ruleAllows(registration.Rule, decision, operation, now)
	if err != nil {
		return NodeRegistration{}, err
	}
	if !allowed {
		return NodeRegistration{}, auth.ObjectNotFound()
	}
	if _, err := c.canonicalRegisteredNodes(
		ctx, map[shoal.ID]NodeRegistration{nodeID: registration},
	); err != nil {
		return NodeRegistration{}, err
	}
	return registration, nil
}

func (c *Client) canonicalRegisteredNodes(
	ctx context.Context,
	registrations map[shoal.ID]NodeRegistration,
) (map[shoal.ID]graph.Node, error) {
	return c.canonicalRegisteredNodesCached(ctx, registrations, nil)
}

func (c *Client) canonicalRegisteredNodesCached(
	ctx context.Context,
	registrations map[shoal.ID]NodeRegistration,
	canonicalDocuments map[shoal.ID]*canonicalRetrievalDocument,
) (map[shoal.ID]graph.Node, error) {
	currentBase, ok := ctx.Value(
		canonicalDocumentIndexKey{}).(map[shoal.ID]shoal.ID)
	if !ok {
		indexed, err := c.withCanonicalDocumentIndex(ctx)
		if err != nil {
			return nil, err
		}
		currentBase = indexed.Value(
			canonicalDocumentIndexKey{}).(map[shoal.ID]shoal.ID)
	}
	if canonicalDocuments == nil {
		canonicalDocuments = make(map[shoal.ID]*canonicalRetrievalDocument)
	}
	required := make(map[shoal.ID]shoal.ID)
	for _, registration := range registrations {
		if registration.Node.ID != "" {
			continue
		}
		if cached, ok := canonicalDocuments[registration.DocumentID]; ok {
			if cached.registration.RevisionID != registration.RevisionID {
				return nil, inconsistentBase()
			}
			continue
		}
		if revisionID, ok := required[registration.DocumentID]; ok &&
			revisionID != registration.RevisionID {
			return nil, inconsistentBase()
		}
		required[registration.DocumentID] = registration.RevisionID
	}
	documentIDs := make([]shoal.ID, 0, len(required))
	for documentID, revisionID := range required {
		if currentBase[documentID] != revisionID {
			return nil, auth.ObjectNotFound()
		}
		documentIDs = append(documentIDs, documentID)
	}
	currentRevisions, err := c.resolveCurrentRevisions(ctx, documentIDs)
	if err != nil {
		return nil, err
	}
	for documentID, revisionID := range required {
		current, ok := currentRevisions[documentID]
		if !ok || current.RevisionID != revisionID {
			return nil, auth.ObjectNotFound()
		}
		view, err := c.base.Document(ctx, documentID, revisionID)
		if err != nil {
			return nil, directBaseError(err)
		}
		if err := verifyDocumentViewRegistration(view, current); err != nil {
			return nil, err
		}
		canonical, err := buildCanonicalRetrievalDocument(view, current)
		if err != nil {
			return nil, inconsistentBase()
		}
		canonicalDocuments[documentID] = canonical
	}
	nodes := make(map[shoal.ID]graph.Node, len(registrations))
	for nodeID, registration := range registrations {
		if registration.Node.ID != "" {
			// This registered-node branch is load-bearing; TestExtractDocumentAuthorizationControlsDerivedGraph pins neighborhood filtering for extracted nodes outside document-section canonical nodes.
			nodes[nodeID] = cloneGraphNode(registration.Node)
			continue
		}
		canonical := canonicalDocuments[registration.DocumentID]
		node, ok := canonical.nodes[nodeID]
		if !ok {
			return nil, inconsistentBase()
		}
		nodes[nodeID] = cloneGraphNode(node)
	}
	return nodes, nil
}
