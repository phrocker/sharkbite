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
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// FoldInteractions creates a native provenance fold only after every named
// session and all source evidence it touched have been authorized for the
// current caller.
func (c *Client) FoldInteractions(
	ctx context.Context, request explorer.FoldRequest,
) (explorer.FoldResult, error) {
	store, err := c.foldStore()
	if err != nil {
		return explorer.FoldResult{}, err
	}
	decision, guard, now, err := c.begin(ctx, auth.OperationConnect)
	if err != nil {
		return explorer.FoldResult{}, err
	}
	pending := interaction.Fold{
		Members: make([]interaction.FoldMember, 0, len(request.SessionIDs)),
	}
	for _, sessionID := range request.SessionIDs {
		record, err := c.InteractionRecord(ctx, sessionID)
		if err != nil {
			return explorer.FoldResult{}, err
		}
		if record.Summary.Deleted || record.Session.ID == "" {
			return explorer.FoldResult{}, auth.ObjectNotFound()
		}
		// The member as the store will fold it, from the trusted record
		// rather than the reader's view, whose expression may be cleared.
		stored, err := c.interactionSource.InteractionRecord(ctx, sessionID)
		if err != nil {
			return explorer.FoldResult{}, directBaseError(err)
		}
		pending.Members = append(pending.Members, interaction.FoldMember{
			SessionID:        sessionID,
			RetrievedNodeIDs: stored.TouchedNodeIDs,
			TouchedEdgeIDs:   stored.TouchedEdgeIDs,
			Visibility:       auth.SplitVisibilityConjunction(stored.Summary.Visibility),
		})
	}
	// A fold is shown only to a reader who may see its own visibility, so a
	// caller who could not see the result is refused before it is written
	// rather than after.
	permitted, err := c.foldLabelsPermitted(ctx, decision, now, pending)
	if err != nil {
		return explorer.FoldResult{}, err
	}
	if !permitted {
		return explorer.FoldResult{}, auth.ObjectNotFound()
	}
	if err := guard.Check(ctx); err != nil {
		return explorer.FoldResult{}, err
	}
	result, err := store.FoldInteractions(ctx, request)
	if err != nil {
		return explorer.FoldResult{}, directBaseError(err)
	}
	fold, err := store.RehydrateFold(ctx, result.FoldID)
	if err != nil {
		return committedFoldFailure(directBaseError(err))
	}
	if err := validateDurableFoldResult(result, fold); err != nil {
		return committedFoldFailure(err)
	}
	// The durable winner is authoritative for retries. Reauthorize every one
	// of its canonical members rather than only the caller-supplied request.
	allowed, err := c.foldVisible(ctx, decision, c.clock(), fold)
	if err != nil {
		return committedFoldFailure(err)
	}
	if !allowed {
		return committedFoldFailure(auth.ObjectNotFound())
	}
	if err := guard.Check(ctx); err != nil {
		return committedFoldFailure(err)
	}
	return result, nil
}

func validateDurableFoldResult(
	result explorer.FoldResult,
	fold interaction.Fold,
) error {
	canonical, err := fold.Canonical()
	if err != nil {
		return err
	}
	foldID, err := canonical.ID()
	if err != nil {
		return err
	}
	matchesID := result.FoldID == foldID
	if !matchesID {
		legacy := canonical
		legacy.Members = make([]interaction.FoldMember, len(canonical.Members))
		for index, member := range canonical.Members {
			legacy.Members[index] = member
			legacy.Members[index].TouchedEdgeIDs = nil
		}
		legacyID, legacyErr := legacy.ID()
		if legacyErr != nil {
			return legacyErr
		}
		matchesID = result.FoldID == legacyID
	}
	var retrieved, cited []shoal.ID
	visibilitySets := make([][]string, 0, len(canonical.Members))
	for _, member := range canonical.Members {
		retrieved = append(retrieved, member.RetrievedNodeIDs...)
		cited = append(cited, member.CitedNodeIDs...)
		visibilitySets = append(visibilitySets, member.Visibility)
	}
	visibility, err := interaction.Conjoin(visibilitySets...)
	if err != nil {
		return err
	}
	if !matchesID ||
		!result.FoldedAt.Equal(canonical.FoldedAt) ||
		result.MemberCount != len(canonical.Members) ||
		result.RetrievedCount != countDistinctIDs(retrieved) ||
		result.CitedCount != countDistinctIDs(cited) ||
		result.Visibility != interaction.Expression(visibility) {
		return shoal.NewError(
			shoal.ErrorConflict,
			"fold result does not match its durable record",
		)
	}
	return nil
}

func countDistinctIDs(values []shoal.ID) int {
	unique := make(map[shoal.ID]struct{}, len(values))
	for _, value := range values {
		unique[value] = struct{}{}
	}
	return len(unique)
}

func committedFoldFailure(err error) (explorer.FoldResult, error) {
	return explorer.FoldResult{}, explorer.MarkCommittedInteraction(err)
}

// Folds lists only folds whose complete member provenance is currently
// authorized. A revoked member makes the fold disappear rather than leaking
// its existence or stale visibility.
func (c *Client) Folds(ctx context.Context) ([]explorer.FoldSummary, error) {
	store, err := c.foldStore()
	if err != nil {
		return nil, err
	}
	decision, guard, now, err := c.begin(ctx, auth.OperationRead)
	if err != nil {
		return nil, err
	}
	values, err := store.Folds(ctx)
	if err != nil {
		return nil, directBaseError(err)
	}
	visible := make([]explorer.FoldSummary, 0, len(values))
	for _, value := range values {
		fold, readErr := store.RehydrateFold(ctx, value.FoldID)
		if readErr != nil {
			if shoal.IsErrorCode(readErr, shoal.ErrorNotFound) ||
				shoal.IsErrorCode(readErr, shoal.ErrorUnavailable) ||
				shoal.IsErrorCode(readErr, shoal.ErrorConflict) {
				continue
			}
			return nil, directBaseError(readErr)
		}
		allowed, authorizationErr := c.foldVisible(ctx, decision, now, fold)
		if authorizationErr != nil {
			return nil, authorizationErr
		}
		if !allowed {
			continue
		}
		visible = append(visible, value)
	}
	if err := guard.Check(ctx); err != nil {
		return nil, err
	}
	return visible, nil
}

func (c *Client) FoldsPage(
	ctx context.Context, after shoal.ID, limit uint32,
) (explorer.FoldSummaryPage, error) {
	if err := shoal.ValidateOptionalID("fold page cursor", after); err != nil {
		return explorer.FoldSummaryPage{}, err
	}
	if limit == 0 || limit > explorer.MaxFoldSummaryPageSize {
		return explorer.FoldSummaryPage{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "fold page limit is outside its bound")
	}
	folds, err := c.Folds(ctx)
	if err != nil {
		return explorer.FoldSummaryPage{}, err
	}
	page := explorer.FoldSummaryPage{
		Folds: make([]explorer.FoldSummary, 0, limit),
	}
	for _, fold := range folds {
		if shoal.CompareID(fold.FoldID, after) <= 0 {
			continue
		}
		if len(page.Folds) == int(limit) {
			page.NextAfter = page.Folds[len(page.Folds)-1].FoldID
			break
		}
		page.Folds = append(page.Folds, fold)
	}
	return page, nil
}

// RehydrateFold returns the exact retrieved/cited split only when every folded
// session remains visible to the current caller.
func (c *Client) RehydrateFold(
	ctx context.Context, foldID shoal.ID,
) (interaction.Fold, error) {
	store, err := c.foldStore()
	if err != nil {
		return interaction.Fold{}, err
	}
	decision, guard, now, err := c.begin(ctx, auth.OperationRead)
	if err != nil {
		return interaction.Fold{}, err
	}
	fold, err := store.RehydrateFold(ctx, foldID)
	if err != nil {
		if shoal.IsErrorCode(err, shoal.ErrorNotFound) ||
			shoal.IsErrorCode(err, shoal.ErrorUnavailable) ||
			shoal.IsErrorCode(err, shoal.ErrorConflict) {
			return interaction.Fold{}, auth.ObjectNotFound()
		}
		return interaction.Fold{}, directBaseError(err)
	}
	allowed, err := c.foldVisible(ctx, decision, now, fold)
	if err != nil {
		return interaction.Fold{}, err
	}
	if !allowed {
		return interaction.Fold{}, auth.ObjectNotFound()
	}
	if err := guard.Check(ctx); err != nil {
		return interaction.Fold{}, err
	}
	return fold, nil
}

// foldVisible reauthorizes every member session of a fold for the current
// caller, each through the interaction read's own label check, and then
// requires the caller to see the fold's own visibility (#568). A fold cannot
// be partially unfolded, because its summary digest covers every member, so
// one member or one label the caller may not see hides the whole fold.
func (c *Client) foldVisible(
	ctx context.Context, decision auth.Decision, now time.Time,
	fold interaction.Fold,
) (bool, error) {
	for _, member := range fold.Members {
		if _, err := c.Interaction(ctx, member.SessionID); err != nil {
			if shoal.IsErrorCode(err, shoal.ErrorNotFound) {
				return false, nil
			}
			return false, err
		}
	}
	return c.foldLabelsPermitted(ctx, decision, now, fold)
}

// foldLabelsPermitted evaluates a fold's own visibility, translated over its
// members' touched provenance.
func (c *Client) foldLabelsPermitted(
	ctx context.Context, decision auth.Decision, now time.Time,
	fold interaction.Fold,
) (bool, error) {
	labels, nodeIDs, edgeIDs := foldVisibility(fold)
	translated, err := c.structuredLabels(
		ctx, readerFor(decision, auth.OperationRead, now), labels, nodeIDs, edgeIDs)
	if err != nil {
		return false, err
	}
	return c.labelVisibility.permits(ctx, decision, translated, now)
}

func (c *Client) foldStore() (FoldStore, error) {
	if isNilDependency(c.foldSource) {
		return nil, shoal.NewError(
			shoal.ErrorUnavailable,
			"trusted provenance fold store is unavailable",
		)
	}
	return c.foldSource, nil
}
