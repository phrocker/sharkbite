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
	"bytes"
	"encoding/binary"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// AccessRule is a canonical conjunction of structured policies. Its
// components are privately and defensively owned.
type AccessRule struct {
	policies []auth.Policy
	keys     [][]byte
}

// NewAccessRule validates, canonicalizes, and deduplicates a nonempty policy
// conjunction by immutable logical policy identity. One rule cannot span
// authorization domains, but physical policy epochs may differ.
func NewAccessRule(policies ...auth.Policy) (AccessRule, error) {
	if len(policies) == 0 {
		return AccessRule{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "at least one access policy is required")
	}
	if len(policies) > auth.MaxPolicyTerms {
		return AccessRule{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "access rule exceeds the policy bound")
	}
	type component struct {
		policy auth.Policy
		key    []byte
	}
	components := make([]component, 0, len(policies))
	var domain []byte
	for index, policy := range policies {
		cloned, _, err := clonePolicy(policy)
		if err != nil {
			return AccessRule{}, err
		}
		key := logicalPolicyKey(cloned)
		if index == 0 {
			domain = cloned.AuthorizationDomain()
		} else if !bytes.Equal(domain, cloned.AuthorizationDomain()) {
			return AccessRule{}, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"access rule policies must share an authorization domain",
			)
		}
		components = append(components, component{policy: cloned, key: key})
	}
	sort.Slice(components, func(left, right int) bool {
		if compared := bytes.Compare(
			components[left].key, components[right].key,
		); compared != 0 {
			return compared < 0
		}
		return components[left].policy.Epoch() < components[right].policy.Epoch()
	})
	rule := AccessRule{
		policies: make([]auth.Policy, 0, len(components)),
		keys:     make([][]byte, 0, len(components)),
	}
	for _, component := range components {
		if len(rule.keys) > 0 &&
			bytes.Equal(rule.keys[len(rule.keys)-1], component.key) {
			continue
		}
		rule.policies = append(rule.policies, component.policy)
		rule.keys = append(rule.keys, append([]byte(nil), component.key...))
	}
	return rule, nil
}

// Authorize requires every policy component to authorize the exact operation
// under the current decision's logical domain/source/policy grants. Physical
// policy epochs do not participate in immutable object authorization.
func (r AccessRule) Authorize(
	decision auth.Decision,
	operation auth.Operation,
	now time.Time,
) error {
	if len(r.policies) == 0 || len(r.policies) != len(r.keys) {
		return authorizationDenied()
	}
	for _, policy := range r.policies {
		if role := policy.ServiceRole(); role != "" {
			if !decision.TrustedService() || decision.ServiceRole() != role ||
				!role.Allows(operation) {
				return authorizationDenied()
			}
		}
		if err := decision.Authorize(operation, auth.ResourceRequest{
			AuthorizationDomain: policy.AuthorizationDomain(),
			SourceID:            policy.SourceID(),
			PolicyID:            policy.GrantPolicyID(),
		}, now); err != nil {
			return err
		}
	}
	return nil
}

// String renders only a digest of the canonical conjunction.
func (r AccessRule) String() string {
	if len(r.policies) == 0 {
		return "access-rule{invalid}"
	}
	return "access-rule{" +
		auth.DigestBytes(
			"explorer-access-rule-logical-v1", logicalRuleBytes(r.keys),
		).String() + "}"
}

func (r AccessRule) extractionEntityNamespace() string {
	if len(r.keys) == 0 {
		return ""
	}
	return auth.DigestBytes(
		"explorer-extraction-entity-namespace-v1",
		logicalRuleBytes(r.keys),
	).String()
}

func (r AccessRule) clone() (AccessRule, error) {
	return NewAccessRule(r.policies...)
}

func (r AccessRule) components() []auth.Policy {
	cloned, err := r.clone()
	if err != nil {
		return nil
	}
	return cloned.policies
}

func (r AccessRule) equal(other AccessRule) bool {
	if len(r.keys) != len(other.keys) {
		return false
	}
	for index := range r.keys {
		if !bytes.Equal(r.keys[index], other.keys[index]) {
			return false
		}
	}
	return len(r.keys) > 0
}

func clonePolicy(policy auth.Policy) (auth.Policy, []byte, error) {
	encoded, err := policy.Encode()
	if err != nil {
		return auth.Policy{}, nil, err
	}
	cloned, err := auth.DecodePolicy(encoded)
	if err != nil {
		return auth.Policy{}, nil, err
	}
	return cloned, append([]byte(nil), encoded...), nil
}

func logicalPolicyKey(policy auth.Policy) []byte {
	var key bytes.Buffer
	writeLogicalComponent := func(value []byte) {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = key.Write(length[:])
		_, _ = key.Write(value)
	}
	writeLogicalComponent(policy.AuthorizationDomain())
	writeLogicalComponent(policy.SourceID())
	writeLogicalComponent(policy.GrantPolicyID())
	writeLogicalComponent([]byte(policy.ServiceRole()))
	return key.Bytes()
}

func logicalRuleBytes(keys [][]byte) []byte {
	var encoded bytes.Buffer
	for _, key := range keys {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(key)))
		_, _ = encoded.Write(length[:])
		_, _ = encoded.Write(key)
	}
	return encoded.Bytes()
}
