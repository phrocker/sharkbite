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

// Free-form ingest visibility labels (interaction.PropertyVisibility) become
// enforceable by translating each one into an ordinary structured policy that
// is conjoined into the document's AccessRule (issue #570). This file holds
// only the identity scheme and the constructors; ingest, grants and migration
// wire it in separately.
//
// The constructors live here, beside AccessRule and NewAccessRule, because
// the product of translation is an AccessRule: a label policy means something
// only as a conjunct next to the source policy it was derived from, and this
// package is where ingest rules are selected (Client.selectIngestRule).

import (
	"bytes"
	"strconv"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The namespace prefixes (auth.LabelPolicyIDPrefix and
// auth.ReservedLabelPolicyIDPrefix) live in package auth so that NewDecision
// and the ingest selectors can refuse them.
const (
	// UntranslatableLabelPolicyID is the reserved, never-grantable grant
	// policy identity conjoined onto a document whose labels cannot be
	// translated. auth.NewDecision refuses it as a grant, so such a document
	// is readable by nobody until it is relabelled.
	UntranslatableLabelPolicyID = auth.ReservedLabelPolicyIDPrefix + "untranslatable"

	// MaxLabelsPerRule bounds the distinct labels one rule may carry, counted
	// in flattened visibility terms. A rule over one source flattens to the
	// source policy's d:, s: and g: terms plus one g: term per label, so
	// 3+n terms must fit in auth.MaxPolicyTerms. The flattened expression
	// must also fit in auth.MaxPolicyExpressionBytes, which LabelRule checks
	// separately and which binds earlier when sources or labels are long.
	MaxLabelsPerRule = auth.MaxPolicyTerms - 3
)

// LabelPolicyID returns the grant-policy identity for one free-form label on
// one source:
//
//	shoal.label/v1/<decimal len(source)>/<source bytes>/<label>
//
// The encoding is injective. The length field is digits only and canonical
// (no sign, no leading zero, never zero because the source is non-empty), and
// it ends at the first '/' after the prefix, so finding it never looks at the
// source. The source is then taken by count, not by delimiter, so source
// bytes that are digits, '/' or anything else cannot move a boundary. The
// label is everything after the '/' that follows the source. A source that
// starts with digits is therefore unambiguous: in "shoal.label/v1/2/12/x" the
// length is 2 (ended by the first '/'), the source is the next two bytes "12",
// and the label is "x". ParseLabelPolicyID is the inverse.
//
// The label must pass interaction.ValidateLabel exactly as given; it is never
// folded, trimmed or sanitized, so "secret" and "Secret" are distinct
// policies. The source must be non-empty and the whole ID must fit in
// auth.MaxPolicyComponentBytes.
func LabelPolicyID(sourceID []byte, label string) ([]byte, error) {
	return auth.LabelPolicyID(sourceID, label)
}

// ParseLabelPolicyID is the inverse of LabelPolicyID. It accepts only an ID
// LabelPolicyID would emit, byte for byte, and so refuses the reserved
// namespace.
func ParseLabelPolicyID(id []byte) (sourceID []byte, label string, err error) {
	return auth.ParseLabelPolicyID(id)
}

// IsReservedLabelPolicyID reports whether id lies in the reserved part of the
// label namespace, which includes UntranslatableLabelPolicyID. A grant loader
// must refuse any such ID: it can never be granted. auth.NewDecision already
// refuses it as a permitted policy identity.
func IsReservedLabelPolicyID(id []byte) bool {
	return auth.IsReservedLabelPolicyID(id)
}

// newLabelPolicy derives the structured policy for one label on the source
// that sourcePolicy names. It is an ordinary user-data policy (auth.NewPolicy,
// never a service policy) with the source policy's domain, byte-equal source
// identity and epoch, and GrantPolicyID = LabelPolicyID(source, label).
func newLabelPolicy(sourcePolicy auth.Policy, label string) (auth.Policy, error) {
	if err := validateLabelSourcePolicy(sourcePolicy); err != nil {
		return auth.Policy{}, err
	}
	id, err := LabelPolicyID(sourcePolicy.SourceID(), label)
	if err != nil {
		return auth.Policy{}, err
	}
	return deriveLabelPolicy(sourcePolicy, id)
}

// UntranslatablePolicy derives the reserved, never-grantable policy for a
// document on sourcePolicy's source whose labels cannot be translated.
func UntranslatablePolicy(sourcePolicy auth.Policy) (auth.Policy, error) {
	if err := validateLabelSourcePolicy(sourcePolicy); err != nil {
		return auth.Policy{}, err
	}
	return deriveLabelPolicy(sourcePolicy, []byte(UntranslatableLabelPolicyID))
}

// LabelRule returns the AccessRule for a document on sourcePolicy's source
// that carries labels: the conjunction of the source policy and one label
// policy per distinct label, built only through NewAccessRule. Duplicate
// labels collapse. An empty label set yields exactly
// NewAccessRule(sourcePolicy).
//
// The rule must stay flattenable into one canonical visibility
// (auth.ConjoinPolicies), so it is refused, never truncated, when it has more
// than MaxLabelsPerRule distinct labels, more than auth.MaxPolicyTerms
// flattened terms, or more than auth.MaxPolicyExpressionBytes flattened bytes.
// Each refusal names the bound it hit.
func LabelRule(sourcePolicy auth.Policy, labels []string) (AccessRule, error) {
	if err := validateLabelSourcePolicy(sourcePolicy); err != nil {
		return AccessRule{}, err
	}
	policies := make([]auth.Policy, 0, 1+min(len(labels), MaxLabelsPerRule))
	policies = append(policies, sourcePolicy)
	seen := make(map[string]struct{}, min(len(labels), MaxLabelsPerRule))
	for _, label := range labels {
		if _, duplicate := seen[label]; duplicate {
			continue
		}
		if len(seen) == MaxLabelsPerRule {
			return AccessRule{}, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"document visibility labels exceed MaxLabelsPerRule ("+
					strconv.Itoa(MaxLabelsPerRule)+" distinct labels)",
			)
		}
		seen[label] = struct{}{}
		policy, err := newLabelPolicy(sourcePolicy, label)
		if err != nil {
			return AccessRule{}, err
		}
		policies = append(policies, policy)
	}
	rule, err := NewAccessRule(policies...)
	if err != nil {
		return AccessRule{}, err
	}
	if err := checkFlattenable(rule.policies); err != nil {
		return AccessRule{}, err
	}
	return rule, nil
}

// checkFlattenable refuses a conjunction that auth.ConjoinPolicies cannot
// render as one visibility, naming the bound it exceeds. It measures the
// flattened terms itself so the refusal can say which bound was hit, then
// defers to ConjoinPolicies as the authority.
func checkFlattenable(policies []auth.Policy) error {
	terms := make(map[string]struct{}, len(policies)*3)
	expressionBytes := -1
	for _, policy := range policies {
		encoded, err := policy.Encode()
		if err != nil {
			return err
		}
		for _, term := range bytes.Split(encoded, []byte{'&'}) {
			if _, duplicate := terms[string(term)]; duplicate {
				continue
			}
			terms[string(term)] = struct{}{}
			expressionBytes += len(term) + 1
		}
	}
	if len(terms) > auth.MaxPolicyTerms {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"document visibility labels exceed auth.MaxPolicyTerms ("+
				strconv.Itoa(auth.MaxPolicyTerms)+" flattened terms, rule needs "+
				strconv.Itoa(len(terms))+")",
		)
	}
	if expressionBytes > auth.MaxPolicyExpressionBytes {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"document visibility labels exceed auth.MaxPolicyExpressionBytes ("+
				strconv.Itoa(auth.MaxPolicyExpressionBytes)+" flattened bytes, rule needs "+
				strconv.Itoa(expressionBytes)+")",
		)
	}
	if _, err := auth.ConjoinPolicies(policies...); err != nil {
		return err
	}
	return nil
}

// validateLabelSourcePolicy refuses a source policy that cannot anchor label
// policies: an invalid one, or one that is itself in the label namespace.
func validateLabelSourcePolicy(sourcePolicy auth.Policy) error {
	if err := sourcePolicy.Validate(); err != nil {
		return err
	}
	if auth.IsLabelPolicyID(sourcePolicy.GrantPolicyID()) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"a label policy cannot anchor further label policies",
		)
	}
	return nil
}

func deriveLabelPolicy(sourcePolicy auth.Policy, grantPolicyID []byte) (auth.Policy, error) {
	policy, err := auth.NewPolicy(auth.PolicyConfig{
		AuthorizationDomain: sourcePolicy.AuthorizationDomain(),
		SourceID:            sourcePolicy.SourceID(),
		GrantPolicyID:       grantPolicyID,
		Epoch:               sourcePolicy.Epoch(),
	})
	if err != nil {
		return auth.Policy{}, err
	}
	if err := checkLabelPolicy(sourcePolicy, grantPolicyID, policy); err != nil {
		return auth.Policy{}, err
	}
	return policy, nil
}

// checkLabelPolicy asserts every field of a constructed label policy against
// the source policy it was derived from, so a defaulted or mismatched field is
// refused rather than silently changing who can read.
func checkLabelPolicy(sourcePolicy auth.Policy, grantPolicyID []byte, policy auth.Policy) error {
	mismatch := func(field string) error {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"label policy "+field+" does not match its source policy",
		)
	}
	if err := policy.Validate(); err != nil {
		return err
	}
	if !bytes.Equal(policy.AuthorizationDomain(), sourcePolicy.AuthorizationDomain()) {
		return mismatch("authorization domain")
	}
	source := policy.SourceID()
	if len(source) == 0 || !bytes.Equal(source, sourcePolicy.SourceID()) {
		return mismatch("source identity")
	}
	if !bytes.Equal(policy.GrantPolicyID(), grantPolicyID) {
		return mismatch("grant policy identity")
	}
	if policy.Epoch() != sourcePolicy.Epoch() {
		return mismatch("epoch")
	}
	if policy.ServiceRole() != "" {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "label policy must not carry a service role")
	}
	if IsReservedLabelPolicyID(grantPolicyID) {
		if string(grantPolicyID) != UntranslatableLabelPolicyID {
			return mismatch("reserved grant policy identity")
		}
		return nil
	}
	// The source named inside the ID must be the policy's own source, so a
	// grant for a label on one source can never stand for another source.
	encodedSource, _, err := ParseLabelPolicyID(grantPolicyID)
	if err != nil {
		return err
	}
	if !bytes.Equal(encodedSource, source) {
		return mismatch("encoded source identity")
	}
	return nil
}
