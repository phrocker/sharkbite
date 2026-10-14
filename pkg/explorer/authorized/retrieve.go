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

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Retrieve always supplies an authorized current-document projection to the
// base scorer, then validates every returned citation and path.
func (c *Client) Retrieve(
	ctx context.Context,
	request retrieval.Request,
) (retrieval.Response, error) {
	var report RetrievalReport
	return c.retrieve(ctx, request, &report)
}

// RetrieveWithSuppressed performs the identical authorized retrieval as
// Retrieve and additionally reports how many current documents this identity
// was denied and therefore never searched. The count is derived from the exact
// same authorization gate Retrieve enforces; it is reporting only and never
// changes which results are returned. See the counting site in retrieve and the
// webapi emission point for the amplification risk this disclosure carries.
func (c *Client) RetrieveWithSuppressed(
	ctx context.Context,
	request retrieval.Request,
) (retrieval.Response, uint32, error) {
	var report RetrievalReport
	response, err := c.retrieve(ctx, request, &report)
	if err != nil {
		return retrieval.Response{}, 0, err
	}
	return response, report.Disclosure.Suppressed, nil
}

// RetrieveWithDisclosure performs the identical authorized retrieval as
// Retrieve and additionally reports both withholding reason classes:
// authorization denials (Suppressed) and mosaic co-occurrence restrictions
// (Restricted). The two counts are kept apart so a caller can tell a plain
// denial from a co-occurrence restriction; neither ever names what was
// withheld.
func (c *Client) RetrieveWithDisclosure(
	ctx context.Context,
	request retrieval.Request,
) (retrieval.Response, Disclosure, error) {
	var report RetrievalReport
	response, err := c.retrieve(ctx, request, &report)
	if err != nil {
		return retrieval.Response{}, Disclosure{}, err
	}
	return response, report.Disclosure, nil
}

// RetrieveWithReport performs authorized retrieval and returns request-local
// embedding-space observability alongside the existing disclosure counts.
// Unlike the older helpers, report is retained when retrieval fails so an
// unavailable authorized provider or fan-out refusal is not silently lost.
func (c *Client) RetrieveWithReport(
	ctx context.Context,
	request retrieval.Request,
) (retrieval.Response, RetrievalReport, error) {
	var report RetrievalReport
	response, err := c.retrieve(ctx, request, &report)
	return response, report, err
}

func (c *Client) retrieve(
	ctx context.Context,
	request retrieval.Request,
	report *RetrievalReport,
) (response retrieval.Response, err error) {
	normalized, err := request.Normalize()
	if err != nil {
		return retrieval.Response{}, err
	}
	if err := normalized.ValidateSeedPlan(true); err != nil {
		return retrieval.Response{}, err
	}
	vectorRequest := normalized.HasMode(retrieval.ModeVector)
	collector := newEmbeddingQueryCollector()
	observedCtx := ctx
	if vectorRequest {
		observedCtx = explorer.WithEmbeddingQueryObserver(ctx, collector.observe)
	}
	if normalized.HasMode(retrieval.ModeVector) && !c.authorizedVectorScoringAvailable() {
		embedding := collector.report(
			report.Disclosure, false,
			shoal.NewError(
				shoal.ErrorUnavailable,
				"authorized vector retrieval requires trusted vector validation",
			),
			true,
		)
		report.Embedding = &embedding
		notifyEmbeddingQueryObserver(ctx, embedding)
		return retrieval.Response{}, shoal.NewError(
			shoal.ErrorUnavailable,
			"authorized vector retrieval requires trusted vector validation",
		)
	}
	decision, guard, now, err := c.begin(ctx, auth.OperationRetrieve)
	if err != nil {
		return retrieval.Response{}, err
	}
	authorizedCandidates := false
	defer func() {
		discloseIdentities := true
		if guardErr := guard.Check(ctx); guardErr != nil {
			discloseIdentities = false
			report.Disclosure = Disclosure{}
			if err == nil {
				response = retrieval.Response{}
				err = guardErr
			}
		}
		if !vectorRequest {
			return
		}
		embedding := collector.report(
			report.Disclosure,
			authorizedCandidates,
			err,
			discloseIdentities,
		)
		report.Embedding = &embedding
		notifyEmbeddingQueryObserver(ctx, embedding)
	}()
	summaries, err := c.base.Documents(ctx)
	if err != nil {
		return retrieval.Response{}, err
	}
	visibleOrder := make([]shoal.ID, 0, len(summaries))
	visible := make(map[shoal.ID]RevisionRegistration, len(summaries))
	for _, summary := range summaries {
		if err := validateSummary(summary); err != nil {
			return retrieval.Response{}, inconsistentBase()
		}
	}
	candidateDocumentIDs := make([]shoal.ID, 0, len(summaries))
	for _, summary := range summaries {
		candidateDocumentIDs = append(candidateDocumentIDs, summary.Document.ID)
	}
	currentRevisions, err := c.resolveCurrentRevisions(ctx, candidateDocumentIDs)
	if err != nil {
		return retrieval.Response{}, err
	}
	for _, summary := range summaries {
		registration, ok := currentRevisions[summary.Document.ID]
		if !ok {
			// A document present in the corpus but covered by no policy grant
			// is withheld from every caller: that is an authorization outcome,
			// so it is counted. This is deliberately asymmetric with the
			// stale-revision drop just below, which is an availability lag, not
			// a policy decision about this caller, and is not counted. Counting
			// !ok is also the signal that reveals a lost or empty policy
			// catalog, where the corpus is intact but every document falls
			// through here; without it a fully withheld corpus would read as
			// "nothing withheld". The record is still dropped exactly as before.
			report.Disclosure.Suppressed++
			continue
		}
		if registration.RevisionID != summary.Revision.ID {
			continue
		}
		allowed, err := ruleAllows(
			registration.Rule, decision, auth.OperationRetrieve, now)
		if err != nil {
			return retrieval.Response{}, err
		}
		if !allowed {
			// Accounting beside the enforcement branch, not within it: the
			// record is still dropped exactly as before. Counting a document
			// the identity's rule denies is unambiguous authorization
			// suppression, alongside the missing-grant case counted above.
			report.Disclosure.Suppressed++
			continue
		}
		if _, duplicate := visible[summary.Document.ID]; duplicate {
			return retrieval.Response{}, inconsistentBase()
		}
		visible[summary.Document.ID] = registration
		visibleOrder = append(visibleOrder, summary.Document.ID)
	}

	restricted, err := c.restrictCoOccurrence(
		ctx, decision, now, visibleOrder, func(documentID shoal.ID) string {
			return visible[documentID].Rule.sensitivityDomain()
		})
	if err != nil {
		return retrieval.Response{}, err
	}
	report.Disclosure.Restricted += restricted.restricted
	if len(restricted.allowed) != len(visibleOrder) {
		// Load-bearing: documents whose sensitivity domain exceeds the
		// co-occurrence budget are dropped from the projection so the base
		// scorer never searches them, exactly as an authorization denial is.
		// TestMosaicBudgetRetrieveWithholdsCrossDomain pins this filtering.
		filteredOrder := make([]shoal.ID, 0, len(restricted.allowed))
		filteredVisible := make(
			map[shoal.ID]RevisionRegistration, len(restricted.allowed))
		for _, documentID := range visibleOrder {
			if _, ok := restricted.allowed[documentID]; !ok {
				continue
			}
			filteredOrder = append(filteredOrder, documentID)
			filteredVisible[documentID] = visible[documentID]
		}
		visibleOrder = filteredOrder
		visible = filteredVisible
	}

	documentIDs := visibleOrder
	if len(normalized.Scope.DocumentIDs) > 0 {
		documentIDs = make([]shoal.ID, 0, len(normalized.Scope.DocumentIDs))
		for _, documentID := range normalized.Scope.DocumentIDs {
			if _, ok := visible[documentID]; ok {
				documentIDs = append(documentIDs, documentID)
			}
		}
		if len(documentIDs) == 0 {
			return c.emptyRetrieval(normalized)
		}
	}
	if len(documentIDs) == 0 {
		return c.emptyRetrieval(normalized)
	}
	selected := make(map[shoal.ID]RevisionRegistration, len(documentIDs))
	selectedNodes := make(map[shoal.ID]NodeRegistration)
	for _, documentID := range documentIDs {
		registration := visible[documentID]
		selected[documentID] = registration
		for _, nodeID := range registration.NodeIDs {
			selectedNodes[nodeID] = NodeRegistration{
				DocumentID: documentID,
				RevisionID: registration.RevisionID,
				Rule:       registration.Rule,
				Kind:       RegistrationDocument,
			}
		}
	}

	var nodeIDs []shoal.ID
	if len(normalized.Scope.NodeIDs) > 0 {
		nodeIDs = make([]shoal.ID, 0, len(normalized.Scope.NodeIDs))
		for _, nodeID := range normalized.Scope.NodeIDs {
			if _, ok := selectedNodes[nodeID]; ok {
				nodeIDs = append(nodeIDs, nodeID)
			}
		}
		if len(nodeIDs) == 0 {
			return c.emptyRetrieval(normalized)
		}
	}
	if len(documentIDs)+len(nodeIDs) > retrieval.MaxScopeIDs {
		return retrieval.Response{}, shoal.NewError(
			shoal.ErrorUnavailable,
			"authorized retrieval projection exceeds the public bound",
		)
	}
	projected := normalized
	projected.Scope = retrieval.Scope{
		DocumentIDs: append([]shoal.ID(nil), documentIDs...),
		NodeIDs:     append([]shoal.ID(nil), nodeIDs...),
	}
	authorizedCandidates = vectorRequest
	corpus, err := c.hydrateRetrievalCorpus(
		ctx, documentIDs, selected, decision, now)
	if err != nil {
		return retrieval.Response{}, err
	}
	var vectorScores map[shoal.ID]shoal.Score
	if vectorRequest {
		nodeScope := make(map[shoal.ID]struct{}, len(projected.Scope.NodeIDs))
		for _, nodeID := range projected.Scope.NodeIDs {
			nodeScope[nodeID] = struct{}{}
		}
		vectorScores, err = c.authorizedVectorScores(
			observedCtx, projected, corpus, nodeScope)
		if err != nil {
			return retrieval.Response{}, err
		}
		if err := collector.err(); err != nil {
			return retrieval.Response{}, err
		}
	}
	response, err = c.base.Retrieve(
		explorer.WithEmbeddingQueryObserver(ctx, nil),
		projected,
	)
	if err != nil {
		if vectorRequest {
			return retrieval.Response{}, authorizedVectorError(err)
		}
		return retrieval.Response{}, err
	}
	if err := response.ValidateFor(projected); err != nil {
		return retrieval.Response{}, inconsistentRetrieval()
	}
	if err := c.validateRetrievedResponseWithVectorScores(
		ctx, response, projected, corpus, decision, now, vectorScores,
	); err != nil {
		return retrieval.Response{}, err
	}
	cloned := cloneRetrievalResponse(response)
	cloned.RequestID = ""
	return cloned, nil
}

func (c *Client) emptyRetrieval(
	request retrieval.Request,
) (retrieval.Response, error) {
	response := retrieval.Response{}
	if err := response.ValidateFor(request); err != nil {
		return retrieval.Response{}, inconsistentRetrieval()
	}
	return response, nil
}

func inconsistentRetrieval() error {
	return shoal.NewError(
		shoal.ErrorInternal,
		"underlying Explorer returned unauthorized retrieval data",
	)
}
