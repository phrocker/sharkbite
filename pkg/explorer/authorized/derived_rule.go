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

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Extracted registrations (RegistrationExtracted) are bound to the document
// revision that asserted them. Their effective rule is their own rule AND the
// asserting document's current rule, exactly as revisionAllows governs an
// older revision: when a document is relabelled, the entities and relations
// extracted from it close with it (#570). An extracted registration whose
// document has no current registration, or whose conjoined rule cannot be
// built, is dropped, which every caller treats as unregistered and denied.
//
// Every other kind keeps its registered rule: intrinsic (document) nodes and
// edges always name the current revision, and materialized and application
// registrations are not asserted by any document. An application edge is,
// however, bound to the revisions its endpoints belonged to when it was
// connected (#695, revision_binding.go). The decision is made on
// the stored RegistrationKind, never on which fields happen to be empty.

// effectiveNodeRegistrations applies the extracted-registration rule to node
// registrations. It runs where node registrations are resolved (resolveNodes,
// authorizedNode, the lexicon batch).
func (c *Client) effectiveNodeRegistrations(
	ctx context.Context,
	registrations map[shoal.ID]NodeRegistration,
) (map[shoal.ID]NodeRegistration, error) {
	return effectiveRegistrations(c, ctx, registrations,
		func(registration NodeRegistration) (RegistrationKind, shoal.ID, shoal.ID, AccessRule) {
			return registration.Kind, registration.DocumentID,
				registration.RevisionID, registration.Rule
		},
		func(registration NodeRegistration, rule AccessRule) NodeRegistration {
			registration.Rule = rule
			return registration
		})
}

// effectiveEdgeRegistrations applies the same rule to edge registrations. It
// runs where edge registrations are resolved (resolveEdges, edgeAllows), so a
// relation asserted only by a relabelled document closes even when both of
// its endpoint entities are shared with, and owned by, public documents.
func (c *Client) effectiveEdgeRegistrations(
	ctx context.Context,
	registrations map[shoal.ID]EdgeRegistration,
) (map[shoal.ID]EdgeRegistration, error) {
	effective, err := effectiveRegistrations(c, ctx, registrations,
		func(registration EdgeRegistration) (RegistrationKind, shoal.ID, shoal.ID, AccessRule) {
			return registration.Kind, registration.DocumentID,
				registration.RevisionID, registration.Rule
		},
		func(registration EdgeRegistration, rule AccessRule) EdgeRegistration {
			registration.Rule = rule
			return registration
		})
	if err != nil {
		return nil, err
	}
	// Application edges are bound to their endpoints' revisions (#695).
	return c.bindEdgeSourceRevisions(ctx, effective)
}

func effectiveRegistrations[R any](
	c *Client,
	ctx context.Context,
	registrations map[shoal.ID]R,
	fields func(R) (RegistrationKind, shoal.ID, shoal.ID, AccessRule),
	withRule func(R, AccessRule) R,
) (map[shoal.ID]R, error) {
	documentIDs := make([]shoal.ID, 0, len(registrations))
	seen := make(map[shoal.ID]struct{}, len(registrations))
	for _, registration := range registrations {
		kind, documentID, _, _ := fields(registration)
		if kind != RegistrationExtracted {
			continue
		}
		if _, duplicate := seen[documentID]; duplicate {
			continue
		}
		seen[documentID] = struct{}{}
		documentIDs = append(documentIDs, documentID)
	}
	if len(documentIDs) == 0 {
		return registrations, nil
	}
	currents, err := c.policyStore.CurrentRevisions(ctx, documentIDs)
	if err != nil {
		return nil, policyCatalogReadError(ctx, err)
	}
	effective := make(map[shoal.ID]R, len(registrations))
	for id, registration := range registrations {
		kind, documentID, revisionID, rule := fields(registration)
		if kind != RegistrationExtracted {
			effective[id] = registration
			continue
		}
		current, ok := currents[documentID]
		if !ok || current.DocumentID != documentID {
			continue
		}
		if current.RevisionID == revisionID {
			effective[id] = registration
			continue
		}
		conjoined, err := conjoinRules(rule, current.Rule)
		if err != nil {
			continue
		}
		effective[id] = withRule(registration, conjoined)
	}
	return effective, nil
}

// conjoinRules is the AND of two rules, built only through NewAccessRule.
func conjoinRules(left, right AccessRule) (AccessRule, error) {
	leftPolicies, rightPolicies := left.components(), right.components()
	if len(leftPolicies) == 0 || len(rightPolicies) == 0 {
		return AccessRule{}, inconsistentBase()
	}
	return NewAccessRule(append(leftPolicies, rightPolicies...)...)
}
