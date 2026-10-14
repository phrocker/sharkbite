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
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// citedEvidence is what a recording's cited assertions add to its revision
// binding (#695): every document revision their evidence cites, and every
// node of their evidence paths.
type citedEvidence struct {
	revisions []RevisionReference
	nodeIDs   []shoal.ID
}

// verifyInteractionCitations refuses a recording that cites anything its
// recorder may not read, before anything else looks at what was cited (#714):
//
//   - every cited document revision, under revisionAllows. A span node ID
//     does not depend on the revision (it hashes document, section, text and
//     occurrence), so the touched-node check passes for a hidden older
//     revision that shares a span with the current one. Without this check
//     the trusted validator's accept-or-refuse told the recorder whether the
//     hidden revision has that text at that range;
//   - every cited non-derived assertion, under the evidence gate reads use
//     (#700): each revision its evidence cites and each node of its evidence
//     paths, with path edges limited to the session's own authorized edges.
//     A derived assertion is skipped. Assertion.Validate requires it to carry
//     exactly one derivation evidence and no citation, so there is nothing
//     to check, and a derived (latent) edge has no policy edge registration,
//     so the touched-edge check already refuses any session touching one.
//
// Every refusal is auth.ObjectNotFound, whether the revision or assertion is
// hidden, unregistered, or never existed, so the outcome does not depend on
// anything the recorder cannot read. It runs before the retry lookup and the
// trusted validator, as the touched-node check does: a retry by a recorder
// that can still read everything it cited is unaffected.
func (c *Client) verifyInteractionCitations(
	ctx context.Context,
	references []interaction.EvidenceReference,
	touchedEdgeIDs []shoal.ID,
	decision auth.Decision,
	operation auth.Operation,
	now time.Time,
) (citedEvidence, error) {
	var revisions []RevisionReference
	cited := make(map[shoal.ID]interaction.AssertionReference)
	for _, reference := range references {
		switch reference.Kind {
		case interaction.EvidenceDocument:
			revisions = append(revisions, RevisionReference{
				DocumentID: reference.Citation.DocumentID,
				RevisionID: reference.Citation.RevisionID,
			})
		case interaction.EvidenceGraph:
			// Keyed by assertion ID alone: the ID hashes the assertion's
			// content, evidence included, so one ID is one evidence set to
			// check whichever edge a reference names. The trusted validator
			// still checks each reference against its own edge.
			for _, assertion := range reference.Assertions {
				if assertion.Origin != ontology.AssertionDerived {
					cited[assertion.AssertionID] = assertion
				}
			}
		}
	}
	revisions, err := canonicalRevisionReferences(revisions)
	if err != nil {
		return citedEvidence{}, auth.ObjectNotFound()
	}
	admitted := make(map[shoal.ID]graph.Edge, len(touchedEdgeIDs))
	for _, edgeID := range touchedEdgeIDs {
		admitted[edgeID] = graph.Edge{ID: edgeID}
	}
	gate := c.newEvidenceGate(decision, operation, now, nil, admitted)
	if err := gate.revisions.cache.loadReferences(ctx, revisions); err != nil {
		return citedEvidence{}, err
	}
	for _, revision := range revisions {
		allowed, err := gate.revisions.referenceAllows(revision)
		if err != nil {
			return citedEvidence{}, err
		}
		if !allowed {
			return citedEvidence{}, auth.ObjectNotFound()
		}
	}
	if len(cited) == 0 {
		return citedEvidence{}, nil
	}
	assertions, err := c.citedInteractionAssertions(ctx, cited)
	if err != nil {
		return citedEvidence{}, err
	}
	if err := gate.load(ctx, assertions); err != nil {
		return citedEvidence{}, err
	}
	var evidence citedEvidence
	for _, assertion := range assertions {
		readable, err := gate.readable(assertion)
		if err != nil {
			return citedEvidence{}, err
		}
		if !readable {
			return citedEvidence{}, auth.ObjectNotFound()
		}
		for _, item := range assertion.Evidence() {
			citation := item.Citation()
			if citation.DocumentID != "" {
				evidence.revisions = append(evidence.revisions, RevisionReference{
					DocumentID: citation.DocumentID, RevisionID: citation.RevisionID,
				})
			}
			if path, ok := item.Path(); ok {
				for _, node := range path.Nodes {
					evidence.nodeIDs = append(evidence.nodeIDs, node.ID)
				}
			}
		}
	}
	evidence.revisions, err = canonicalRevisionReferences(evidence.revisions)
	if err != nil {
		return citedEvidence{}, inconsistentBase()
	}
	return evidence, nil
}

// citedInteractionAssertions resolves each cited assertion from the trusted
// corpus: the assertion the base graph attaches to its edge or, for a
// relation several extractions assert, one of theirs (#700), matched by ID
// and origin as the trusted validator matches them (an assertion ID hashes
// its content, evidence included, and an assertion the graph attaches
// through an edge property names no edge in its metadata). One the corpus
// does not hold is refused exactly as an unreadable one is; whether it fits
// its edge is left to the trusted validator.
func (c *Client) citedInteractionAssertions(
	ctx context.Context,
	cited map[shoal.ID]interaction.AssertionReference,
) ([]ontology.Assertion, error) {
	edgeIDs := make([]shoal.ID, 0, len(cited))
	seenEdges := make(map[shoal.ID]struct{}, len(cited))
	for _, reference := range cited {
		if _, seen := seenEdges[reference.EdgeID]; !seen {
			seenEdges[reference.EdgeID] = struct{}{}
			edgeIDs = append(edgeIDs, reference.EdgeID)
		}
	}
	sortIDs(edgeIDs)
	found := make(map[shoal.ID]ontology.Assertion, len(cited))
	match := func(edgeID shoal.ID, assertion ontology.Assertion) error {
		reference, ok := cited[assertion.ID()]
		if !ok || (edgeID != "" && reference.EdgeID != edgeID) ||
			reference.Origin != assertion.Origin() {
			return nil
		}
		if err := assertion.Validate(); err != nil {
			return inconsistentBase()
		}
		found[assertion.ID()] = assertion
		return nil
	}
	for start := 0; start < len(edgeIDs); {
		end := chunkEnd(start, len(edgeIDs), maxInteractionAuthorizationIDs)
		chunk := edgeIDs[start:end]
		start = end
		if err := c.graphAttachedAssertions(ctx, chunk, match); err != nil {
			return nil, err
		}
		if isNilDependency(c.relationAssertions) {
			continue
		}
		alternatives, err := c.relationAssertions.ExtractionAssertions(ctx, chunk)
		if err != nil {
			return nil, directBaseError(err)
		}
		for _, edgeID := range chunk {
			for _, alternative := range alternatives[edgeID] {
				if err := match(edgeID, alternative); err != nil {
					return nil, err
				}
			}
		}
	}
	assertions := make([]ontology.Assertion, 0, len(cited))
	for id := range cited {
		assertion, ok := found[id]
		if !ok {
			return nil, auth.ObjectNotFound()
		}
		assertions = append(assertions, assertion)
	}
	return assertions, nil
}

// EdgeAssertionReader is the explicitly trusted source of the assertion the
// base graph attaches to each edge, read by edge ID (#714). NewClient
// promotes it from SnapshotValidator, never from Base.
type EdgeAssertionReader interface {
	EdgeAssertions(
		context.Context, []shoal.ID,
	) (map[shoal.ID]ontology.Assertion, error)
}

// graphAttachedAssertions passes match the assertion the base graph attaches
// to each edge. The trusted EdgeAssertionReader reads exactly those edges
// under the corpus's shared lock. Without one, it falls back to the base's
// depth-1 neighborhood of each edge's source, filtered to the edges' types,
// which reads the source's whole adjacency.
func (c *Client) graphAttachedAssertions(
	ctx context.Context,
	edgeIDs []shoal.ID,
	match func(shoal.ID, ontology.Assertion) error,
) error {
	if !isNilDependency(c.edgeAssertions) {
		attached, err := c.edgeAssertions.EdgeAssertions(ctx, edgeIDs)
		if err != nil {
			return directBaseError(err)
		}
		for _, edgeID := range edgeIDs {
			if assertion, ok := attached[edgeID]; ok {
				if err := match(edgeID, assertion); err != nil {
					return err
				}
			}
		}
		return nil
	}
	registrations, err := c.policyStore.Edges(ctx, edgeIDs)
	if err != nil {
		return policyCatalogReadError(ctx, err)
	}
	sources := make([]shoal.ID, 0, len(edgeIDs))
	types := make([]string, 0, len(edgeIDs))
	for _, edgeID := range edgeIDs {
		registration, ok := registrations[edgeID]
		if !ok {
			return auth.ObjectNotFound()
		}
		sources = append(sources, registration.Edge.From)
		types = append(types, registration.Edge.Type)
	}
	raw, err := c.base.Neighborhood(ctx, explorer.NeighborhoodRequest{
		NodeIDs: sources, Depth: 1, EdgeTypes: types,
	})
	if err != nil {
		return directBaseError(err)
	}
	for _, assertion := range raw.Assertions {
		edgeID := shoal.ID(assertion.Metadata()[graphAssertionEdgeIDMetadata])
		if err := match(edgeID, assertion); err != nil {
			return err
		}
	}
	return nil
}
