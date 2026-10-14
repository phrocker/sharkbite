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

package auth

import (
	"bytes"
	"encoding/base32"
	"strconv"
	"strings"

	"github.com/phrocker/shoal-oss/accumulo"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

var componentEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// PolicyConfig supplies the immutable logical policy components.
type PolicyConfig struct {
	AuthorizationDomain []byte
	SourceID            []byte
	GrantPolicyID       []byte
	Epoch               int64
}

// Policy is one structured logical visibility policy. A service role is
// present only when it was derived from a trusted service decision or decoded
// from canonical stored policy bytes and subsequently checked against one.
type Policy struct {
	domain      []byte
	source      []byte
	grantPolicy []byte
	epoch       int64
	serviceRole ServiceRole
}

// NewPolicy constructs a normal user-data policy without service visibility.
func NewPolicy(config PolicyConfig) (Policy, error) {
	policy := Policy{
		domain:      cloneBytes(config.AuthorizationDomain),
		source:      cloneBytes(config.SourceID),
		grantPolicy: cloneBytes(config.GrantPolicyID),
		epoch:       config.Epoch,
	}
	if err := policy.validate(); err != nil {
		return Policy{}, err
	}
	return policy, nil
}

// NewServicePolicy constructs a service-only policy whose role is taken from
// a structurally valid trusted decision, never from caller-provided text.
func NewServicePolicy(config PolicyConfig, decision Decision) (Policy, error) {
	cloned, err := decision.cloneValidated()
	if err != nil || !cloned.TrustedService() {
		return Policy{}, shoal.NewError(
			shoal.ErrorUnauthorized, "trusted service decision required")
	}
	if !bytes.Equal(cloned.domain, config.AuthorizationDomain) ||
		!containsBytes(cloned.sources, config.SourceID) ||
		!containsBytes(cloned.policies, config.GrantPolicyID) {
		return Policy{}, shoal.NewError(
			shoal.ErrorUnauthorized, "service policy is outside the trusted decision")
	}
	policy, err := NewPolicy(config)
	if err != nil {
		return Policy{}, err
	}
	policy.serviceRole = cloned.serviceRole
	return policy, nil
}

// AuthorizationDomain returns an independent copy.
func (p Policy) AuthorizationDomain() []byte { return cloneBytes(p.domain) }

// SourceID returns an independent copy.
func (p Policy) SourceID() []byte { return cloneBytes(p.source) }

// GrantPolicyID returns an independent copy.
func (p Policy) GrantPolicyID() []byte { return cloneBytes(p.grantPolicy) }

// Epoch returns the positive signed policy epoch.
func (p Policy) Epoch() int64 { return p.epoch }

// ServiceRole returns the optional service-only visibility role.
func (p Policy) ServiceRole() ServiceRole { return p.serviceRole }

// Validate checks the structured policy invariants.
func (p Policy) Validate() error { return p.validate() }

// String renders only logical and physical digests.
func (p Policy) String() string {
	logical, logicalErr := p.LogicalPolicyDigest()
	visibility, visibilityErr := p.VisibilityDigest()
	if logicalErr != nil || visibilityErr != nil {
		return "policy{invalid}"
	}
	return "policy{logical=" + logical.String() + ",visibility=" +
		visibility.String() + "}"
}

// EncodeComponent injectively encodes opaque bytes as canonical lowercase,
// no-padding base32.
func EncodeComponent(component []byte) (string, error) {
	if err := validatePolicyComponent("policy component", component, true); err != nil {
		return "", err
	}
	return strings.ToLower(componentEncoding.EncodeToString(component)), nil
}

// DecodeComponent decodes canonical lowercase, no-padding base32 while
// enforcing the decoded component bound.
func DecodeComponent(encoded string) ([]byte, error) {
	if encoded == "" {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "encoded component is required")
	}
	if len(encoded) > MaxEncodedPolicyComponentBytes {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "encoded component exceeds the byte bound")
	}
	for _, character := range encoded {
		if !((character >= 'a' && character <= 'z') ||
			(character >= '2' && character <= '7')) {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument, "encoded component is not canonical base32")
		}
	}
	decoded, err := componentEncoding.DecodeString(strings.ToUpper(encoded))
	if err != nil || len(decoded) == 0 || len(decoded) > MaxPolicyComponentBytes {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "encoded component is not canonical base32")
	}
	canonical, err := EncodeComponent(decoded)
	if err != nil || canonical != encoded {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "encoded component is not canonical base32")
	}
	return decoded, nil
}

// Encode returns the canonical flattened Accumulo visibility expression.
func (p Policy) Encode() ([]byte, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	labels, err := p.labels()
	if err != nil {
		return nil, err
	}
	return canonicalVisibility(labels)
}

// DecodePolicy accepts only a complete canonical policy expression emitted by
// Encode. It rejects caller expressions, unions, grouping, quoting,
// whitespace, duplicates, alternate ordering, and noncanonical base32.
func DecodePolicy(expression []byte) (Policy, error) {
	if len(expression) == 0 {
		return Policy{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "policy visibility cannot be blank")
	}
	if len(expression) > MaxPolicyExpressionBytes {
		return Policy{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "policy visibility exceeds the byte bound")
	}
	for _, character := range expression {
		switch character {
		case ' ', '\t', '\n', '\r', '|', '(', ')', '"', '\'':
			return Policy{}, shoal.NewError(
				shoal.ErrorInvalidArgument, "policy visibility is not canonical")
		}
	}
	terms := bytes.Split(expression, []byte{'&'})
	if len(terms) < 3 || len(terms) > 4 || len(terms) > MaxPolicyTerms {
		return Policy{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "policy visibility has an invalid term count")
	}
	seenTerms := make(map[string]struct{}, len(terms))
	for _, term := range terms {
		if len(term) == 0 {
			return Policy{}, shoal.NewError(
				shoal.ErrorInvalidArgument, "policy visibility contains an empty term")
		}
		if _, duplicate := seenTerms[string(term)]; duplicate {
			return Policy{}, shoal.NewError(
				shoal.ErrorInvalidArgument, "policy visibility contains duplicate terms")
		}
		seenTerms[string(term)] = struct{}{}
	}
	visibility, err := accumulo.NewColumnVisibility(expression)
	if err != nil || !bytes.Equal(visibility.Flatten(), expression) {
		return Policy{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "policy visibility is not canonical")
	}

	var policy Policy
	var haveDomain, haveSource, haveGrant, haveService bool
	for _, termBytes := range terms {
		term := string(termBytes)
		switch {
		case strings.HasPrefix(term, "d:"):
			if haveDomain {
				return Policy{}, invalidPolicyVisibility()
			}
			policy.domain, err = DecodeComponent(strings.TrimPrefix(term, "d:"))
			haveDomain = true
		case strings.HasPrefix(term, "s:"):
			if haveSource {
				return Policy{}, invalidPolicyVisibility()
			}
			policy.source, err = DecodeComponent(strings.TrimPrefix(term, "s:"))
			haveSource = true
		case strings.HasPrefix(term, "g:"):
			if haveGrant {
				return Policy{}, invalidPolicyVisibility()
			}
			encodedPolicy, encodedEpoch, found := strings.Cut(
				strings.TrimPrefix(term, "g:"), ":e:")
			if !found || encodedEpoch == "" {
				return Policy{}, invalidPolicyVisibility()
			}
			policy.grantPolicy, err = DecodeComponent(encodedPolicy)
			if err == nil {
				policy.epoch, err = strconv.ParseInt(encodedEpoch, 10, 64)
				if err == nil && (policy.epoch <= 0 ||
					strconv.FormatInt(policy.epoch, 10) != encodedEpoch) {
					err = invalidPolicyVisibility()
				}
			}
			haveGrant = true
		case strings.HasPrefix(term, "svc:"):
			if haveService {
				return Policy{}, invalidPolicyVisibility()
			}
			policy.serviceRole = ServiceRole(strings.TrimPrefix(term, "svc:"))
			err = policy.serviceRole.Validate()
			haveService = true
		default:
			return Policy{}, invalidPolicyVisibility()
		}
		if err != nil {
			return Policy{}, invalidPolicyVisibility()
		}
	}
	if !haveDomain || !haveSource || !haveGrant {
		return Policy{}, invalidPolicyVisibility()
	}
	if err := policy.validate(); err != nil {
		return Policy{}, invalidPolicyVisibility()
	}
	canonical, err := policy.Encode()
	if err != nil || !bytes.Equal(canonical, expression) {
		return Policy{}, invalidPolicyVisibility()
	}
	return policy, nil
}

// ConjoinPolicies returns the canonical conjunction of every input policy.
// Shared labels are deduplicated; no union or blank visibility can be emitted.
func ConjoinPolicies(policies ...Policy) ([]byte, error) {
	if len(policies) == 0 {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "at least one policy is required")
	}
	if len(policies) > MaxPolicyTerms {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "policy conjunction exceeds the input bound")
	}
	labels := make([][]byte, 0, len(policies)*4)
	seen := make(map[string]struct{}, len(policies)*4)
	for _, policy := range policies {
		if err := policy.validate(); err != nil {
			return nil, err
		}
		policyLabels, err := policy.labels()
		if err != nil {
			return nil, err
		}
		for _, label := range policyLabels {
			if _, duplicate := seen[string(label)]; duplicate {
				continue
			}
			seen[string(label)] = struct{}{}
			labels = append(labels, label)
		}
	}
	return canonicalVisibility(labels)
}

// LogicalPolicyDigest is stable across epoch/generation changes. It includes
// the domain, source, grant-policy identity, and optional service role.
func (p Policy) LogicalPolicyDigest() (Digest, error) {
	if err := p.validate(); err != nil {
		return Digest{}, err
	}
	encoder := newDigestEncoder("explorer-policy-logical-v1")
	encoder.bytes(p.domain)
	encoder.bytes(p.source)
	encoder.bytes(p.grantPolicy)
	encoder.text(string(p.serviceRole))
	return encoder.sum(), nil
}

// VisibilityDigest hashes the canonical physical visibility, including epoch.
func (p Policy) VisibilityDigest() (Digest, error) {
	expression, err := p.Encode()
	if err != nil {
		return Digest{}, err
	}
	return DigestBytes("explorer-policy-visibility-v1", expression), nil
}

func (p Policy) validate() error {
	if err := validatePolicyComponent("authorization domain", p.domain, true); err != nil {
		return err
	}
	if err := validatePolicyComponent("source identity", p.source, true); err != nil {
		return err
	}
	if err := validatePolicyComponent("grant policy identity", p.grantPolicy, true); err != nil {
		return err
	}
	if p.epoch <= 0 {
		return shoal.NewError(shoal.ErrorInvalidArgument, "policy epoch must be positive")
	}
	if p.serviceRole != "" {
		return p.serviceRole.Validate()
	}
	return nil
}

func (p Policy) labels() ([][]byte, error) {
	domain, err := EncodeComponent(p.domain)
	if err != nil {
		return nil, err
	}
	source, err := EncodeComponent(p.source)
	if err != nil {
		return nil, err
	}
	grant, err := EncodeComponent(p.grantPolicy)
	if err != nil {
		return nil, err
	}
	labels := [][]byte{
		[]byte("d:" + domain),
		[]byte("s:" + source),
		[]byte("g:" + grant + ":e:" + strconv.FormatInt(p.epoch, 10)),
	}
	if p.serviceRole != "" {
		labels = append(labels, []byte("svc:"+string(p.serviceRole)))
	}
	return labels, nil
}

func canonicalVisibility(labels [][]byte) ([]byte, error) {
	if len(labels) == 0 {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "policy visibility cannot be blank")
	}
	if len(labels) > MaxPolicyTerms {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "policy visibility exceeds the term bound")
	}
	owned := cloneByteSlices(labels)
	seen := make(map[string]struct{}, len(owned))
	total := len(owned) - 1
	for _, label := range owned {
		if len(label) == 0 {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument, "policy visibility contains an empty term")
		}
		for _, character := range label {
			if !accumulo.ValidAuthorizationCharacter(character) {
				return nil, shoal.NewError(
					shoal.ErrorInvalidArgument, "policy visibility contains an invalid term")
			}
		}
		if _, duplicate := seen[string(label)]; duplicate {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument, "policy visibility contains duplicate terms")
		}
		seen[string(label)] = struct{}{}
		total += len(label)
	}
	if total > MaxPolicyExpressionBytes {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "policy visibility exceeds the byte bound")
	}
	expression := bytes.Join(owned, []byte{'&'})
	visibility, err := accumulo.NewColumnVisibility(expression)
	if err != nil {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "generated policy visibility is invalid")
	}
	flattened := visibility.Flatten()
	if len(flattened) == 0 || len(flattened) > MaxPolicyExpressionBytes {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "generated policy visibility is outside bounds")
	}
	return flattened, nil
}

func invalidPolicyVisibility() error {
	return shoal.NewError(shoal.ErrorInvalidArgument, "policy visibility is not canonical")
}

func validateStructuredLabel(label []byte) error {
	if len(label) == 0 {
		return invalidPolicyVisibility()
	}
	term := string(label)
	switch {
	case strings.HasPrefix(term, "d:"):
		_, err := DecodeComponent(strings.TrimPrefix(term, "d:"))
		return err
	case strings.HasPrefix(term, "s:"):
		_, err := DecodeComponent(strings.TrimPrefix(term, "s:"))
		return err
	case strings.HasPrefix(term, "g:"):
		encodedPolicy, encodedEpoch, found := strings.Cut(
			strings.TrimPrefix(term, "g:"), ":e:")
		if !found {
			return invalidPolicyVisibility()
		}
		if _, err := DecodeComponent(encodedPolicy); err != nil {
			return err
		}
		epoch, err := strconv.ParseInt(encodedEpoch, 10, 64)
		if err != nil || epoch <= 0 || strconv.FormatInt(epoch, 10) != encodedEpoch {
			return invalidPolicyVisibility()
		}
		return nil
	case strings.HasPrefix(term, "svc:"):
		role := ServiceRole(strings.TrimPrefix(term, "svc:"))
		return role.Validate()
	default:
		return invalidPolicyVisibility()
	}
}
