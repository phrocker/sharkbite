// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
package webapi

import (
	"context"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type GraphMaterializeNode struct {
	Key        string         `json:"key"`
	Kind       string         `json:"kind"`
	Properties shoal.Metadata `json:"properties,omitempty"`
}

type GraphMaterializeRelation struct {
	Key        string         `json:"key"`
	From       string         `json:"from"`
	To         string         `json:"to"`
	Type       string         `json:"type"`
	Properties shoal.Metadata `json:"properties,omitempty"`
}

type GraphMaterializeRequest struct {
	Namespace  string                     `json:"namespace"`
	SourceID   string                     `json:"source_id"`
	PolicyID   string                     `json:"policy_id"`
	MutationID string                     `json:"mutation_id"`
	Snapshot   Snapshot                   `json:"snapshot"`
	Nodes      []GraphMaterializeNode     `json:"nodes"`
	Relations  []GraphMaterializeRelation `json:"relations,omitempty"`
}

type GraphMaterializeIdentity struct {
	Key string `json:"key"`
	ID  string `json:"id"`
}

type GraphMaterializeResponse struct {
	MaterializationID string                     `json:"materialization_id"`
	MutationID        string                     `json:"mutation_id"`
	Disposition       explorer.IngestDisposition `json:"disposition"`
	Snapshot          Snapshot                   `json:"snapshot"`
	Nodes             []GraphMaterializeIdentity `json:"nodes"`
	Relations         []GraphMaterializeIdentity `json:"relations,omitempty"`
}

type GraphMaterializationProvider interface {
	MaterializeGraph(
		context.Context,
		GraphMaterializeRequest,
	) (GraphMaterializeResponse, error)
}

func (s *EmbeddedService) MaterializeGraph(
	ctx context.Context,
	request GraphMaterializeRequest,
) (GraphMaterializeResponse, error) {
	backend, ok := s.client.(interface {
		MaterializeGraph(
			context.Context,
			explorer.GraphMaterializationRequest,
		) (explorer.GraphMaterializationResult, error)
	})
	if !ok {
		return GraphMaterializeResponse{}, shoal.NewError(
			shoal.ErrorUnavailable, "graph materialization is unavailable")
	}
	namespace, err := decodeOpaqueGraphValue("namespace", request.Namespace)
	if err != nil {
		return GraphMaterializeResponse{}, err
	}
	mutationID, err := decodeID(request.MutationID)
	if err != nil {
		return GraphMaterializeResponse{}, shoal.WrapError(
			shoal.ErrorInvalidArgument, "graph mutation ID", err)
	}
	sourceID, err := decodeOpaqueGraphValue("source ID", request.SourceID)
	if err != nil {
		return GraphMaterializeResponse{}, err
	}
	policyID, err := decodeOpaqueGraphValue("policy ID", request.PolicyID)
	if err != nil {
		return GraphMaterializeResponse{}, err
	}
	nodes := make([]explorer.GraphNodeSpec, len(request.Nodes))
	for index, node := range request.Nodes {
		key, err := decodeOpaqueGraphValue("graph node key", node.Key)
		if err != nil {
			return GraphMaterializeResponse{}, err
		}
		nodes[index] = explorer.GraphNodeSpec{
			Key: key, Kind: node.Kind, Properties: node.Properties,
		}
	}
	relations := make([]explorer.GraphRelationSpec, len(request.Relations))
	for index, relation := range request.Relations {
		key, err := decodeOpaqueGraphValue("graph relation key", relation.Key)
		if err != nil {
			return GraphMaterializeResponse{}, err
		}
		from, err := decodeOpaqueGraphValue("graph relation source", relation.From)
		if err != nil {
			return GraphMaterializeResponse{}, err
		}
		to, err := decodeOpaqueGraphValue("graph relation target", relation.To)
		if err != nil {
			return GraphMaterializeResponse{}, err
		}
		relations[index] = explorer.GraphRelationSpec{
			Key: key, From: from, To: to, Type: relation.Type,
			Properties: relation.Properties,
		}
	}
	result, err := backend.MaterializeGraph(ctx, explorer.GraphMaterializationRequest{
		Namespace: namespace, SourceID: sourceID, PolicyID: policyID,
		MutationID: mutationID,
		ExpectedSnapshot: explorer.Snapshot{
			ID: request.Snapshot.ID, AsOf: request.Snapshot.AsOf,
			Frontier: request.Snapshot.Frontier,
		},
		Nodes: nodes, Relations: relations,
	})
	if err != nil {
		return GraphMaterializeResponse{}, err
	}
	return graphMaterializeResponse(result), nil
}

func decodeOpaqueGraphValue(name, value string) ([]byte, error) {
	decoded, err := decodeID(value)
	if err != nil {
		return nil, shoal.WrapError(
			shoal.ErrorInvalidArgument, name, err)
	}
	return append([]byte(nil), []byte(decoded)...), nil
}

func graphMaterializeResponse(
	result explorer.GraphMaterializationResult,
) GraphMaterializeResponse {
	nodes := make([]GraphMaterializeIdentity, len(result.Nodes))
	for index, identity := range result.Nodes {
		nodes[index] = GraphMaterializeIdentity{
			Key: encodeID(shoal.ID(identity.Key)), ID: encodeID(identity.ID),
		}
	}
	relations := make([]GraphMaterializeIdentity, len(result.Relations))
	for index, identity := range result.Relations {
		relations[index] = GraphMaterializeIdentity{
			Key: encodeID(shoal.ID(identity.Key)), ID: encodeID(identity.ID),
		}
	}
	return GraphMaterializeResponse{
		MaterializationID: encodeID(result.MaterializationID),
		MutationID:        encodeID(result.MutationID),
		Disposition:       result.Disposition,
		Snapshot:          fromExplorerSnapshot(result.Snapshot),
		Nodes:             nodes,
		Relations:         relations,
	}
}
