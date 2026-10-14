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
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// PolicySelector is trusted host configuration that derives exactly one
// structured ingest policy. Source fields are descriptive input, never grants.
type PolicySelector interface {
	SelectPolicy(context.Context, auth.Decision, explorer.Source) (auth.Policy, error)
}

// PolicySelectorFunc adapts a trusted host function to PolicySelector.
type PolicySelectorFunc func(
	context.Context, auth.Decision, explorer.Source,
) (auth.Policy, error)

// SelectPolicy calls the trusted selector function.
func (f PolicySelectorFunc) SelectPolicy(
	ctx context.Context,
	decision auth.Decision,
	source explorer.Source,
) (auth.Policy, error) {
	return f(ctx, decision, source)
}

// EdgePolicySelector is trusted host configuration for application edges.
type EdgePolicySelector interface {
	SelectEdgePolicy(context.Context, auth.Decision, graph.Edge) (auth.Policy, error)
}

// EdgePolicySelectorFunc adapts a trusted host function to EdgePolicySelector.
type EdgePolicySelectorFunc func(
	context.Context, auth.Decision, graph.Edge,
) (auth.Policy, error)

// SelectEdgePolicy calls the trusted edge selector function.
func (f EdgePolicySelectorFunc) SelectEdgePolicy(
	ctx context.Context,
	decision auth.Decision,
	edge graph.Edge,
) (auth.Policy, error) {
	return f(ctx, decision, edge)
}

// StaticPolicySelector is a safe embedded selector whose source and grant
// identities come only from trusted construction-time configuration.
type StaticPolicySelector struct {
	sourceID      []byte
	grantPolicyID []byte
}

// NewStaticPolicySelector constructs a selector that combines trusted static
// source/grant identities with the resolved decision's exact domain and
// generation. It deliberately ignores Source metadata, URI, and edge values.
func NewStaticPolicySelector(
	sourceID, grantPolicyID []byte,
) (*StaticPolicySelector, error) {
	// A source policy must never be a label policy: label policies are only
	// derived from a source policy (LabelRule), never selected as one.
	if auth.IsLabelPolicyID(grantPolicyID) {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"static selector grant policy identity is in the label namespace "+
				auth.LabelPolicyNamespace,
		)
	}
	if _, err := auth.NewPolicy(auth.PolicyConfig{
		AuthorizationDomain: []byte("static-selector-validation"),
		SourceID:            sourceID,
		GrantPolicyID:       grantPolicyID,
		Epoch:               1,
	}); err != nil {
		return nil, err
	}
	return &StaticPolicySelector{
		sourceID:      append([]byte(nil), sourceID...),
		grantPolicyID: append([]byte(nil), grantPolicyID...),
	}, nil
}

// SelectPolicy derives an ingest policy from trusted configuration and the
// resolved decision only.
func (s *StaticPolicySelector) SelectPolicy(
	ctx context.Context,
	decision auth.Decision,
	_ explorer.Source,
) (auth.Policy, error) {
	if err := contextFailure(ctx); err != nil {
		return auth.Policy{}, err
	}
	return s.policy(decision)
}

// SelectEdgePolicy derives the corresponding trusted edge policy.
func (s *StaticPolicySelector) SelectEdgePolicy(
	ctx context.Context,
	decision auth.Decision,
	_ graph.Edge,
) (auth.Policy, error) {
	if err := contextFailure(ctx); err != nil {
		return auth.Policy{}, err
	}
	return s.policy(decision)
}

func (s *StaticPolicySelector) policy(decision auth.Decision) (auth.Policy, error) {
	return auth.NewPolicy(auth.PolicyConfig{
		AuthorizationDomain: decision.AuthorizationDomain(),
		SourceID:            append([]byte(nil), s.sourceID...),
		GrantPolicyID:       append([]byte(nil), s.grantPolicyID...),
		Epoch:               decision.PolicyGeneration(),
	})
}
