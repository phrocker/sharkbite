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

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// MaxRestrictedReferences bounds one restriction request. It matches the
// evidence bound a single action may record, which is the largest set of
// anchors one call can legitimately carry.
const MaxRestrictedReferences = 256

// RestrictDisclosure reports which of a caller-declared set of document
// references that caller may still disclose, charging the co-occurrence budget
// exactly as a read of the same documents would.
//
// It exists because every control in this package can only withhold. A result
// this client assembles can have documents removed from it; a payload some
// other process assembled cannot, and that process is the one about to transmit
// it. Without this the co-occurrence budget can protect a Shoal response and
// nothing else, and an external caller can only ever be refused outright where
// an internal one would have been quietly narrowed.
//
// The charge happens here, before the call, and that is the point: the budget
// is a pre-call control and the disclosure it is charged for is the one the
// caller is about to make. Repeating an identical request does not double
// charge, because the budget's update is a set union over observed domains — so
// a caller that retries an admission after a lost response is not penalised for
// the retry.
//
// A reference with no current registration, or one this identity's rule denies,
// is withheld with no distinction between the two. The caller supplied the
// reference, so naming it back discloses nothing; saying *why* would separate
// "you may not read this" from "this does not exist", which is the disclosure
// this package refuses everywhere else.
func (c *Client) RestrictDisclosure(
	ctx context.Context,
	references []shoal.ID,
) ([]shoal.ID, error) {
	if len(references) == 0 {
		return nil, nil
	}
	if len(references) > MaxRestrictedReferences {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"restricted references exceed their bound")
	}
	// OperationRetrieve, not OperationList. The caller is about to put this
	// content into a payload, which is the disclosure retrieve governs; list
	// would authorize knowing a document exists and admit a caller that may
	// enumerate the corpus but not read it.
	decision, guard, now, err := c.begin(ctx, auth.OperationRetrieve)
	if err != nil {
		return nil, err
	}
	distinct := make([]shoal.ID, 0, len(references))
	seen := make(map[shoal.ID]struct{}, len(references))
	for _, reference := range references {
		if err := shoal.ValidateRequiredID(
			"restricted reference", reference); err != nil {
			return nil, err
		}
		if _, duplicate := seen[reference]; duplicate {
			continue
		}
		seen[reference] = struct{}{}
		distinct = append(distinct, reference)
	}
	registrations, err := c.resolveCurrentRevisions(ctx, distinct)
	if err != nil {
		return nil, err
	}
	order := make([]shoal.ID, 0, len(distinct))
	for _, reference := range distinct {
		registration, found := registrations[reference]
		// The two guards below overlap, and no single mutation of either is
		// observable: an unregistered reference yields a zero AccessRule, whose
		// Authorize denies, so the rule check alone already withholds it.
		//
		// This one is kept anyway because dropping it would make the fail-closed
		// outcome for an unregistered reference depend on the zero value of a
		// type declared elsewhere. That is a real invariant today and a silent
		// hole the day someone gives AccessRule a permissive zero value.
		// TestRestrictDisclosureWithholdsUnauthorizedAndUnknownAlike catches
		// removing both.
		if !found {
			continue
		}
		allowed, err := ruleAllows(
			registration.Rule, decision, auth.OperationRetrieve, now)
		if err != nil {
			return nil, err
		}
		if !allowed {
			continue
		}
		order = append(order, reference)
	}
	restricted, err := c.restrictCoOccurrence(
		ctx, decision, now, order, func(reference shoal.ID) string {
			return registrations[reference].Rule.sensitivityDomain()
		})
	if err != nil {
		return nil, err
	}
	result := make([]shoal.ID, 0, len(order))
	for _, reference := range order {
		if _, ok := restricted.allowed[reference]; !ok {
			// Load-bearing: the identity is individually authorized for this
			// reference, but admitting it would exceed the identity's
			// distinct-domain budget. Dropping the branch would hand the caller
			// permission to transmit exactly what the budget exists to stop.
			continue
		}
		result = append(result, reference)
	}
	// Checked after the budget has been charged and the answer computed, in the
	// same order the read paths use: a generation that moved under this request
	// invalidates the answer, and returning a set computed against a superseded
	// policy generation would admit content the current policy may not.
	if err := guard.Check(ctx); err != nil {
		return nil, err
	}
	return result, nil
}
