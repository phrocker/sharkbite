// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
package authorized

import (
	"bytes"
	"context"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type graphMaterializationBackend interface {
	PlanGraphMaterialization(
		context.Context,
		explorer.GraphMaterializationRequest,
	) (explorer.GraphMaterializationResult, error)
	CommitGraphMaterialization(
		context.Context,
		explorer.GraphMaterializationResult,
	) (explorer.GraphMaterializationResult, error)
}

// MaterializeGraph publishes one bounded, immutable graph fixture under the
// exact trusted source and policy selected for the caller.
func (c *Client) MaterializeGraph(
	ctx context.Context,
	request explorer.GraphMaterializationRequest,
) (explorer.GraphMaterializationResult, error) {
	backend, ok := c.base.(graphMaterializationBackend)
	if !ok {
		return explorer.GraphMaterializationResult{}, shoal.NewError(
			shoal.ErrorUnavailable, "graph materialization is unavailable")
	}
	if err := refuseMaterializedVisibility(request); err != nil {
		return explorer.GraphMaterializationResult{}, err
	}
	decision, guard, now, err := c.begin(ctx, auth.OperationGraphMaterialize)
	if err != nil {
		return explorer.GraphMaterializationResult{}, err
	}
	rule, err := c.selectGraphMaterializationRule(ctx, decision, now)
	if err != nil {
		return explorer.GraphMaterializationResult{}, err
	}
	if err := decision.Authorize(
		auth.OperationGraphMaterialize,
		auth.ResourceRequest{
			AuthorizationDomain: decision.AuthorizationDomain(),
			SourceID:            request.SourceID, PolicyID: request.PolicyID,
		},
		now,
	); err != nil {
		return explorer.GraphMaterializationResult{}, authorizationDenied()
	}
	components := rule.components()
	if len(components) != 1 ||
		!bytes.Equal(components[0].SourceID(), request.SourceID) ||
		!bytes.Equal(components[0].GrantPolicyID(), request.PolicyID) {
		return explorer.GraphMaterializationResult{}, authorizationDenied()
	}
	request.IdentityNamespace = auth.DigestBytes(
		"explorer-graph-materialization-identity-v1",
		logicalRuleBytes(rule.keys),
	).Bytes()

	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()
	lease, err := c.policyStore.AcquireMutation(ctx)
	if err != nil {
		return explorer.GraphMaterializationResult{},
			policyCatalogWriteError(ctx, err)
	}
	defer lease.Release()
	if err := guard.Check(ctx); err != nil {
		return explorer.GraphMaterializationResult{}, err
	}
	planned, err := backend.PlanGraphMaterialization(ctx, request)
	if err != nil {
		return explorer.GraphMaterializationResult{}, directBaseError(err)
	}
	nodeRegistration := NodeRegistration{
		DocumentID: planned.MaterializationID,
		RevisionID: planned.MutationID,
		Rule:       mustCloneRule(rule),
		Kind:       RegistrationMaterialized,
	}
	for _, node := range planned.GraphNodes {
		nodeRegistration.Node = node
		if err := c.policyStore.PutNode(
			ctx, node.ID, nodeRegistration,
		); err != nil {
			return explorer.GraphMaterializationResult{},
				policyCatalogWriteError(ctx, err)
		}
	}
	for _, edge := range planned.GraphEdges {
		if err := c.policyStore.PutEdge(ctx, EdgeRegistration{
			Edge: edge, DocumentID: planned.MaterializationID,
			RevisionID: planned.MutationID, Rule: mustCloneRule(rule),
			Kind: RegistrationMaterialized,
		}); err != nil {
			return explorer.GraphMaterializationResult{},
				policyCatalogWriteError(ctx, err)
		}
	}
	if err := guard.Check(ctx); err != nil {
		return explorer.GraphMaterializationResult{}, err
	}
	result, err := backend.CommitGraphMaterialization(ctx, planned)
	if err != nil {
		return explorer.GraphMaterializationResult{}, directBaseError(err)
	}
	if err := guard.Check(ctx); err != nil {
		return explorer.GraphMaterializationResult{},
			explorer.MarkIndeterminateCommit(err)
	}
	return result, nil
}

// refuseMaterializedVisibility refuses a materialization whose nodes or
// relations declare visibility labels. A materialization is registered under
// the bare selected source rule; it reads nothing from the corpus (relations
// may only join its own nodes), so there are no input rules to conjoin, and a
// declared label would be stored as if it were enforced while every holder of
// the source could read it (#570). Labelled content is published through
// Ingest, where labels become rule components.
func refuseMaterializedVisibility(
	request explorer.GraphMaterializationRequest,
) error {
	declares := func(properties shoal.Metadata) bool {
		for _, key := range []string{
			interaction.PropertyVisibility,
			interaction.PropertyVisibilityDigest,
			interaction.PropertyVisibilityCount,
		} {
			if _, ok := properties[key]; ok {
				return true
			}
		}
		return false
	}
	for _, node := range request.Nodes {
		if declares(node.Properties) {
			return materializedVisibilityRefused()
		}
	}
	for _, relation := range request.Relations {
		if declares(relation.Properties) {
			return materializedVisibilityRefused()
		}
	}
	return nil
}

func materializedVisibilityRefused() error {
	return shoal.NewError(
		shoal.ErrorInvalidArgument,
		"graph materialization cannot declare visibility labels: it is "+
			"registered under the bare source rule, which would not enforce them",
	)
}

func (c *Client) selectGraphMaterializationRule(
	ctx context.Context,
	decision auth.Decision,
	now time.Time,
) (AccessRule, error) {
	policy, err := c.policySelector.SelectPolicy(
		ctx, decision, explorer.Source{URI: "graph://materialization"},
	)
	if err != nil {
		return AccessRule{}, policySelectionError(ctx, err)
	}
	return selectedPolicyRule(
		decision, auth.OperationGraphMaterialize, policy, now,
	)
}

var _ interface {
	MaterializeGraph(
		context.Context,
		explorer.GraphMaterializationRequest,
	) (explorer.GraphMaterializationResult, error)
} = (*Client)(nil)
