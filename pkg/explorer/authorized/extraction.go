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
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type extractionBackend interface {
	PlanExtractDocument(
		context.Context,
		explorer.ExtractionRequest,
	) (explorer.ExtractionResult, error)
	CommitExtraction(
		context.Context,
		explorer.ExtractionResult,
	) (explorer.ExtractionResult, error)
	ExtractDocument(
		context.Context,
		explorer.ExtractionRequest,
	) (explorer.ExtractionResult, error)
}

// ExtractDocument authorizes an explicit extraction action against the source
// document before any derived graph objects are cataloged.
func (c *Client) ExtractDocument(
	ctx context.Context,
	request explorer.ExtractionRequest,
) (explorer.ExtractionResult, error) {
	backend, ok := c.base.(extractionBackend)
	if !ok {
		return explorer.ExtractionResult{}, shoal.NewError(
			shoal.ErrorUnavailable, "extraction is unavailable")
	}
	decision, guard, now, err := c.begin(ctx, auth.OperationIngest)
	if err != nil {
		return explorer.ExtractionResult{}, err
	}

	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()
	lease, err := c.policyStore.AcquireMutation(ctx)
	if err != nil {
		return explorer.ExtractionResult{}, policyCatalogWriteError(ctx, err)
	}
	defer lease.Release()
	if err := guard.Check(ctx); err != nil {
		return explorer.ExtractionResult{}, err
	}
	// This authorization is load-bearing; TestExtractDocumentRejectsUnreadableSource
	// pins that extracted entities from a document the caller cannot read are
	// never published or exposed through the graph.
	source, err := c.authorizedNode(
		ctx, request.DocumentID, decision, auth.OperationRead, now)
	if err != nil {
		return explorer.ExtractionResult{}, err
	}
	if request.RevisionID != "" && source.RevisionID != request.RevisionID {
		return explorer.ExtractionResult{}, auth.ObjectNotFound()
	}
	request.RevisionID = source.RevisionID
	// This namespace is load-bearing; TestAuthorizedExtractDocumentCrossTenantSharedEntityGetsDistinctNodes pins that attacker-chosen entity keys cannot collide across authorization scopes.
	request.EntityNamespace = source.Rule.extractionEntityNamespace()
	// The extraction grounds only on entities this principal may read, so
	// neither its prompt nor what it reports as reused depends on a document
	// the principal cannot read (#700). This filter is load-bearing;
	// TestExtractionContextOmitsUnreadableEntities pins it.
	request.ContextNodes = func(
		ctx context.Context, nodeIDs []shoal.ID,
	) ([]shoal.ID, error) {
		return c.readableNodeIDs(ctx, nodeIDs, decision, auth.OperationRead, now)
	}
	result, err := backend.PlanExtractDocument(ctx, request)
	if err != nil {
		return explorer.ExtractionResult{}, directBaseError(err)
	}
	nodeRegistration := NodeRegistration{
		DocumentID: source.DocumentID,
		RevisionID: source.RevisionID,
		Rule:       mustCloneRule(source.Rule),
		Kind:       RegistrationExtracted,
	}
	for _, node := range result.GraphNodes {
		// This node registration is load-bearing; TestExtractDocumentAuthorizationControlsDerivedGraph pins that extracted entities remain governed by their source document rule.
		nodeRegistration.Node = node
		if err := c.policyStore.PutNode(ctx, node.ID, nodeRegistration); err != nil {
			return explorer.ExtractionResult{}, extractionCatalogWriteError(ctx, err)
		}
	}
	edgeRule := mustCloneRule(source.Rule)
	for _, edge := range result.GraphEdges {
		// This edge registration is load-bearing; TestExtractDocumentAuthorizationControlsDerivedGraph pins authorization filtering for extracted graph edges.
		// Bound to the asserting revision, so a relabel of this document
		// also closes the relations only it asserts (#570).
		if err := c.policyStore.PutEdge(ctx, EdgeRegistration{
			Edge:       edge,
			DocumentID: source.DocumentID,
			RevisionID: source.RevisionID,
			Rule:       edgeRule,
			Kind:       RegistrationExtracted,
		}); err != nil {
			return explorer.ExtractionResult{}, extractionCatalogWriteError(ctx, err)
		}
	}
	if err := guard.Check(ctx); err != nil {
		return explorer.ExtractionResult{}, err
	}
	// This ordering is load-bearing; TestExtractDocumentRegistrationFailureDoesNotCommitGraph pins that policy registration failure cannot leave orphan graph mutations.
	result, err = backend.CommitExtraction(ctx, result)
	if err != nil {
		return explorer.ExtractionResult{}, directBaseError(err)
	}
	c.invalidateAuthorizedVectorAvailability()
	return result, nil
}

// readableNodeIDs returns, in order, the nodes among nodeIDs the decision may
// read, resolved in bounded batches.
func (c *Client) readableNodeIDs(
	ctx context.Context,
	nodeIDs []shoal.ID,
	decision auth.Decision,
	operation auth.Operation,
	now time.Time,
) ([]shoal.ID, error) {
	reader := readerFor(decision, operation, now)
	visible := make([]shoal.ID, 0, len(nodeIDs))
	for start := 0; start < len(nodeIDs); {
		end := chunkEnd(start, len(nodeIDs), maxInteractionAuthorizationIDs)
		chunk := nodeIDs[start:end]
		start = end
		resolved, err := c.resolveNodes(ctx, chunk, reader)
		if err != nil {
			return nil, err
		}
		for _, nodeID := range chunk {
			registration, ok := resolved[nodeID]
			if !ok {
				continue
			}
			allowed, err := ruleAllows(registration.Rule, decision, operation, now)
			if err != nil {
				return nil, err
			}
			if allowed {
				visible = append(visible, nodeID)
			}
		}
	}
	return visible, nil
}

func extractionCatalogWriteError(ctx context.Context, err error) error {
	if shoal.IsErrorCode(err, shoal.ErrorConflict) {
		return auth.ObjectNotFound()
	}
	return policyCatalogWriteError(ctx, err)
}

var _ interface {
	ExtractDocument(
		context.Context,
		explorer.ExtractionRequest,
	) (explorer.ExtractionResult, error)
} = (*Client)(nil)
