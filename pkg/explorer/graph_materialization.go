// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
package explorer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	MaxGraphMaterializationNodes     = 256
	MaxGraphMaterializationRelations = 2048
	MaxGraphMaterializationBytes     = 2 << 20
)

type GraphNodeSpec struct {
	Key        []byte
	Kind       string
	Properties shoal.Metadata
}

type GraphRelationSpec struct {
	Key        []byte
	From       []byte
	To         []byte
	Type       string
	Properties shoal.Metadata
}

// GraphMaterializationRequest is an immutable graph publication. Namespace is
// caller supplied; IdentityNamespace must be supplied by a trusted adapter.
type GraphMaterializationRequest struct {
	Namespace         []byte
	IdentityNamespace []byte
	SourceID          []byte
	PolicyID          []byte
	MutationID        shoal.ID
	ExpectedSnapshot  Snapshot
	Nodes             []GraphNodeSpec
	Relations         []GraphRelationSpec
}

type GraphIdentity struct {
	Key []byte
	ID  shoal.ID
}

type GraphMaterializationResult struct {
	MaterializationID shoal.ID
	MutationID        shoal.ID
	Disposition       IngestDisposition
	Snapshot          Snapshot
	Nodes             []GraphIdentity
	Relations         []GraphIdentity
	GraphNodes        []graph.Node
	GraphEdges        []graph.Edge
	record            *persistedGraphMaterialization
	expectedSnapshot  Snapshot
}

type persistedGraphMaterialization struct {
	ID           shoal.ID
	MutationID   shoal.ID
	Digest       [sha256.Size]byte
	Nodes        []graph.Node
	Edges        []graph.Edge
	NodeKeys     [][]byte
	RelationKeys [][]byte
	PublishedAt  time.Time
}

func (e *Explorer) MaterializeGraph(
	ctx context.Context,
	request GraphMaterializationRequest,
) (GraphMaterializationResult, error) {
	planned, err := e.PlanGraphMaterialization(ctx, request)
	if err != nil {
		return GraphMaterializationResult{}, err
	}
	return e.CommitGraphMaterialization(ctx, planned)
}

// PlanGraphMaterialization validates and derives stable graph identities
// without mutating storage.
func (e *Explorer) PlanGraphMaterialization(
	ctx context.Context,
	request GraphMaterializationRequest,
) (GraphMaterializationResult, error) {
	if err := contextError(ctx); err != nil {
		return GraphMaterializationResult{}, err
	}
	planned, err := planGraphMaterialization(request)
	if err != nil {
		return GraphMaterializationResult{}, err
	}
	result := graphMaterializationResult(
		planned, IngestApplied, request.ExpectedSnapshot)
	result.expectedSnapshot = request.ExpectedSnapshot
	return result, nil
}

// CommitGraphMaterialization atomically publishes a previously planned graph.
func (e *Explorer) CommitGraphMaterialization(
	ctx context.Context,
	plannedResult GraphMaterializationResult,
) (GraphMaterializationResult, error) {
	if err := contextError(ctx); err != nil {
		return GraphMaterializationResult{}, err
	}
	if plannedResult.record == nil {
		return GraphMaterializationResult{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "planned graph materialization is required")
	}
	planned := clonePersistedGraphMaterialization(*plannedResult.record)
	if err := validatePlannedGraphMaterialization(planned); err != nil {
		return GraphMaterializationResult{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.requireOpen(); err != nil {
		return GraphMaterializationResult{}, err
	}
	if err := e.requireWritableLocked(); err != nil {
		return GraphMaterializationResult{}, err
	}
	if err := e.ensureGraphLocked(); err != nil {
		return GraphMaterializationResult{}, err
	}
	if existing := e.graphMaterializations[planned.ID]; existing != nil {
		if existing.MutationID != planned.MutationID ||
			existing.Digest != planned.Digest {
			return GraphMaterializationResult{}, shoal.NewError(
				shoal.ErrorConflict,
				"graph materialization namespace already has different content",
			)
		}
		return graphMaterializationResult(
			*existing, IngestUnchanged, e.snapshot), nil
	}
	if !snapshotsEqual(plannedResult.expectedSnapshot, e.snapshot) {
		return GraphMaterializationResult{}, shoal.NewError(
			shoal.ErrorConflict, "expected snapshot is no longer current")
	}
	for _, node := range planned.Nodes {
		if existing, ok := e.graphNodes[node.ID]; ok && !nodesEqual(existing, node) {
			return GraphMaterializationResult{}, shoal.NewError(
				shoal.ErrorConflict, "graph node ID already has different content")
		}
	}
	for _, edge := range planned.Edges {
		if existing, ok := e.graphEdges[edge.ID]; ok && !edgesEqual(existing, edge) {
			return GraphMaterializationResult{}, shoal.NewError(
				shoal.ErrorConflict, "graph edge ID already has different content")
		}
	}
	planned.PublishedAt = time.Now().UTC()
	accepted, err := e.conditionalInteractionRecord(
		graphMaterializationRecordRow(planned.ID),
		embeddedRecordGraphMaterialization,
		planned,
		recordCQV2,
		false,
	)
	if err != nil {
		return GraphMaterializationResult{}, err
	}
	if !accepted {
		var winner persistedGraphMaterialization
		found, readErr := e.lookupEmbeddedRecord(
			graphMaterializationRecordRow(planned.ID),
			embeddedRecordGraphMaterialization,
			&winner,
		)
		if readErr != nil {
			return GraphMaterializationResult{}, readErr
		}
		if !found || winner.MutationID != planned.MutationID ||
			winner.Digest != planned.Digest {
			return GraphMaterializationResult{}, shoal.NewError(
				shoal.ErrorConflict,
				"graph materialization namespace already has different content",
			)
		}
		e.graphMaterializations[winner.ID] = &winner
		return graphMaterializationResult(
			winner, IngestUnchanged, e.snapshot), nil
	}
	copy := planned
	e.graphMaterializations[planned.ID] = &copy
	// The record is durable from here, so a later failure is indeterminate
	// (#687): it keeps its code, but the caller must not read it as nothing
	// written.
	if err := e.rebuildCurrentGraphLocked(); err != nil {
		return GraphMaterializationResult{}, MarkIndeterminateCommit(err)
	}
	e.refreshSnapshotLocked()
	if err := e.registerSnapshotLocked(e.snapshot); err != nil {
		return GraphMaterializationResult{}, MarkIndeterminateCommit(err)
	}
	return graphMaterializationResult(planned, IngestApplied, e.snapshot), nil
}

func validatePlannedGraphMaterialization(
	record persistedGraphMaterialization,
) error {
	if record.PublishedAt.IsZero() {
		record.PublishedAt = time.Unix(1, 0).UTC()
	}
	return validatePersistedGraphMaterialization(record)
}

func planGraphMaterialization(
	request GraphMaterializationRequest,
) (persistedGraphMaterialization, error) {
	if len(request.Namespace) == 0 || len(request.Namespace) > shoal.MaxIDBytes {
		return persistedGraphMaterialization{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "graph namespace is outside its bound")
	}
	if len(request.IdentityNamespace) == 0 ||
		len(request.IdentityNamespace) > shoal.MaxIDBytes {
		return persistedGraphMaterialization{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"trusted graph identity namespace is outside its bound")
	}
	if err := shoal.ValidateRequiredID(
		"graph mutation ID", request.MutationID); err != nil {
		return persistedGraphMaterialization{}, err
	}
	if request.ExpectedSnapshot.ID == "" ||
		request.ExpectedSnapshot.AsOf.IsZero() {
		return persistedGraphMaterialization{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "expected snapshot is required")
	}
	if len(request.Nodes) == 0 ||
		len(request.Nodes) > MaxGraphMaterializationNodes {
		return persistedGraphMaterialization{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"graph materialization node count is outside its bound")
	}
	if len(request.Relations) > MaxGraphMaterializationRelations {
		return persistedGraphMaterialization{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"graph materialization relation count exceeds its bound")
	}
	nodeSpecs := append([]GraphNodeSpec(nil), request.Nodes...)
	sort.Slice(nodeSpecs, func(i, j int) bool {
		return bytes.Compare(nodeSpecs[i].Key, nodeSpecs[j].Key) < 0
	})
	nodes := make([]graph.Node, 0, len(nodeSpecs))
	nodeIDs := make(map[string]shoal.ID, len(nodeSpecs))
	nodeKeys := make([][]byte, 0, len(nodeSpecs))
	total := len(request.Namespace) + len(request.IdentityNamespace) +
		len(request.MutationID)
	for _, spec := range nodeSpecs {
		if err := validateGraphKey("graph node key", spec.Key); err != nil {
			return persistedGraphMaterialization{}, err
		}
		key := string(spec.Key)
		if _, duplicate := nodeIDs[key]; duplicate {
			return persistedGraphMaterialization{}, shoal.NewError(
				shoal.ErrorInvalidArgument, "graph node keys must be unique")
		}
		if err := validateGraphKind("graph node kind", spec.Kind); err != nil {
			return persistedGraphMaterialization{}, err
		}
		if interaction.IsInteractionKind(spec.Kind) ||
			graph.IsProvenanceKind(spec.Kind) {
			return persistedGraphMaterialization{}, shoal.NewError(
				shoal.ErrorInvalidArgument, "graph node kind is reserved")
		}
		if err := shoal.ValidateMetadata(
			"graph node properties", spec.Properties); err != nil {
			return persistedGraphMaterialization{}, err
		}
		id := materializedID(
			"node", request.IdentityNamespace, request.Namespace, spec.Key)
		node := graph.Node{
			ID: id, Kind: spec.Kind, Properties: cloneGraphMetadata(spec.Properties),
		}
		if err := node.Validate(); err != nil {
			return persistedGraphMaterialization{}, err
		}
		nodeIDs[key] = id
		nodeKeys = append(nodeKeys, append([]byte(nil), spec.Key...))
		nodes = append(nodes, node)
		total += len(spec.Key) + len(spec.Kind) + metadataBytes(spec.Properties)
	}
	relationSpecs := append([]GraphRelationSpec(nil), request.Relations...)
	sort.Slice(relationSpecs, func(i, j int) bool {
		return bytes.Compare(relationSpecs[i].Key, relationSpecs[j].Key) < 0
	})
	edges := make([]graph.Edge, 0, len(relationSpecs))
	relationKeys := make([][]byte, 0, len(relationSpecs))
	seenRelations := make(map[string]struct{}, len(relationSpecs))
	for _, spec := range relationSpecs {
		if err := validateGraphKey("graph relation key", spec.Key); err != nil {
			return persistedGraphMaterialization{}, err
		}
		key := string(spec.Key)
		if _, duplicate := seenRelations[key]; duplicate {
			return persistedGraphMaterialization{}, shoal.NewError(
				shoal.ErrorInvalidArgument, "graph relation keys must be unique")
		}
		from, fromOK := nodeIDs[string(spec.From)]
		to, toOK := nodeIDs[string(spec.To)]
		if !fromOK || !toOK {
			return persistedGraphMaterialization{}, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"graph relation references a node outside the materialization")
		}
		if err := validateGraphKind("graph relation type", spec.Type); err != nil {
			return persistedGraphMaterialization{}, err
		}
		if interaction.IsInteractionEdgeType(spec.Type) ||
			graph.IsProvenanceEdgeType(spec.Type) {
			return persistedGraphMaterialization{}, shoal.NewError(
				shoal.ErrorInvalidArgument, "graph relation type is reserved")
		}
		if err := shoal.ValidateMetadata(
			"graph relation properties", spec.Properties); err != nil {
			return persistedGraphMaterialization{}, err
		}
		edge := graph.Edge{
			ID: materializedID(
				"edge", request.IdentityNamespace, request.Namespace, spec.Key),
			From: from, To: to, Type: spec.Type, Weight: 1,
			Properties: cloneGraphMetadata(spec.Properties),
		}
		if err := edge.Validate(); err != nil {
			return persistedGraphMaterialization{}, err
		}
		seenRelations[key] = struct{}{}
		relationKeys = append(relationKeys, append([]byte(nil), spec.Key...))
		edges = append(edges, edge)
		total += len(spec.Key) + len(spec.From) + len(spec.To) + len(spec.Type) +
			metadataBytes(spec.Properties)
	}
	if total > MaxGraphMaterializationBytes {
		return persistedGraphMaterialization{}, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"graph materialization payload exceeds its bound")
	}
	id := materializedID(
		"materialization", request.IdentityNamespace, request.Namespace)
	record := persistedGraphMaterialization{
		ID: id, MutationID: request.MutationID,
		Nodes: nodes, Edges: edges, NodeKeys: nodeKeys,
		RelationKeys: relationKeys,
	}
	record.Digest = graphMaterializationDigest(record)
	return record, nil
}

func validateGraphKey(name string, value []byte) error {
	if len(value) == 0 || len(value) > shoal.MaxIDBytes {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, name+" is outside its bound")
	}
	return nil
}

func validateGraphKind(name, value string) error {
	if strings.TrimSpace(value) == "" || !utf8.ValidString(value) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, name+" must be valid nonblank UTF-8")
	}
	return shoal.ValidateSemanticString(name, value)
}

func materializedID(kind string, parts ...[]byte) shoal.ID {
	digest := sha256.New()
	writeGraphDigestPart(digest, []byte("shoal.graph-materialization.v1"))
	writeGraphDigestPart(digest, []byte(kind))
	for _, part := range parts {
		writeGraphDigestPart(digest, part)
	}
	return shoal.ID("materialized-" + kind + "-" +
		hex.EncodeToString(digest.Sum(nil)))
}

func writeGraphDigestPart(digest interface{ Write([]byte) (int, error) }, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = digest.Write(size[:])
	_, _ = digest.Write(value)
}

func graphMaterializationDigest(record persistedGraphMaterialization) [sha256.Size]byte {
	digest := sha256.New()
	writeGraphDigestPart(digest, []byte(record.ID))
	writeGraphDigestPart(digest, []byte(record.MutationID))
	for index, node := range record.Nodes {
		writeGraphDigestPart(digest, record.NodeKeys[index])
		writeGraphDigestPart(digest, []byte(node.ID))
		writeGraphDigestPart(digest, []byte(node.Kind))
		writeGraphMetadataDigest(digest, node.Properties)
	}
	for index, edge := range record.Edges {
		writeGraphDigestPart(digest, record.RelationKeys[index])
		writeGraphDigestPart(digest, []byte(edge.ID))
		writeGraphDigestPart(digest, []byte(edge.From))
		writeGraphDigestPart(digest, []byte(edge.To))
		writeGraphDigestPart(digest, []byte(edge.Type))
		writeGraphMetadataDigest(digest, edge.Properties)
	}
	var result [sha256.Size]byte
	copy(result[:], digest.Sum(nil))
	return result
}

func writeGraphMetadataDigest(
	digest interface{ Write([]byte) (int, error) },
	metadata shoal.Metadata,
) {
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		writeGraphDigestPart(digest, []byte(key))
		writeGraphDigestPart(digest, []byte(metadata[key]))
	}
}

func metadataBytes(metadata shoal.Metadata) int {
	total := 0
	for key, value := range metadata {
		total += len(key) + len(value)
	}
	return total
}

func cloneGraphMetadata(input shoal.Metadata) shoal.Metadata {
	if input == nil {
		return nil
	}
	result := make(shoal.Metadata, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func snapshotsEqual(left, right Snapshot) bool {
	return left.ID == right.ID && left.AsOf.Equal(right.AsOf) &&
		left.Frontier == right.Frontier
}

func graphMaterializationResult(
	record persistedGraphMaterialization,
	disposition IngestDisposition,
	snapshot Snapshot,
) GraphMaterializationResult {
	nodes := make([]GraphIdentity, len(record.Nodes))
	for index := range record.Nodes {
		nodes[index] = GraphIdentity{
			Key: append([]byte(nil), record.NodeKeys[index]...),
			ID:  record.Nodes[index].ID,
		}
	}
	relations := make([]GraphIdentity, len(record.Edges))
	for index := range record.Edges {
		relations[index] = GraphIdentity{
			Key: append([]byte(nil), record.RelationKeys[index]...),
			ID:  record.Edges[index].ID,
		}
	}
	return GraphMaterializationResult{
		MaterializationID: record.ID, MutationID: record.MutationID,
		Disposition: disposition, Snapshot: snapshot,
		Nodes: nodes, Relations: relations,
		GraphNodes: cloneNodes(record.Nodes), GraphEdges: cloneEdges(record.Edges),
		record: func() *persistedGraphMaterialization {
			cloned := clonePersistedGraphMaterialization(record)
			return &cloned
		}(),
	}
}

func clonePersistedGraphMaterialization(
	record persistedGraphMaterialization,
) persistedGraphMaterialization {
	record.ID = shoal.ID(append([]byte(nil), []byte(record.ID)...))
	record.MutationID = shoal.ID(
		append([]byte(nil), []byte(record.MutationID)...))
	record.Nodes = cloneNodes(record.Nodes)
	record.Edges = cloneEdges(record.Edges)
	record.NodeKeys = cloneByteSlices(record.NodeKeys)
	record.RelationKeys = cloneByteSlices(record.RelationKeys)
	return record
}

func cloneByteSlices(values [][]byte) [][]byte {
	result := make([][]byte, len(values))
	for index := range values {
		result[index] = append([]byte(nil), values[index]...)
	}
	return result
}

func validatePersistedGraphMaterialization(
	record persistedGraphMaterialization,
) error {
	if err := shoal.ValidateRequiredID(
		"graph materialization ID", record.ID); err != nil {
		return err
	}
	if err := shoal.ValidateRequiredID(
		"graph mutation ID", record.MutationID); err != nil {
		return err
	}
	if record.PublishedAt.IsZero() ||
		len(record.Nodes) == 0 ||
		len(record.Nodes) != len(record.NodeKeys) ||
		len(record.Edges) != len(record.RelationKeys) {
		return fmt.Errorf("graph materialization record is incomplete")
	}
	for _, node := range record.Nodes {
		if err := node.Validate(); err != nil {
			return err
		}
	}
	for _, edge := range record.Edges {
		if err := edge.Validate(); err != nil {
			return err
		}
	}
	if graphMaterializationDigest(record) != record.Digest {
		return fmt.Errorf("graph materialization digest is invalid")
	}
	return nil
}

func (e *Explorer) loadGraphMaterializationRecord(
	row, qualifier, encoded []byte,
) error {
	if !bytes.Equal(qualifier, []byte(recordCQV2)) {
		return nil
	}
	var record persistedGraphMaterialization
	if err := decodeEmbeddedRecord(
		encoded, embeddedRecordGraphMaterialization, &record,
	); err != nil {
		return shoal.WrapError(
			shoal.ErrorInternal, "decode graph materialization", err)
	}
	if err := validatePersistedGraphMaterialization(record); err != nil {
		return shoal.WrapError(
			shoal.ErrorInternal, "stored graph materialization is invalid", err)
	}
	if !bytes.Equal(row, graphMaterializationRecordRow(record.ID)) {
		return shoal.NewError(
			shoal.ErrorInternal, "stored graph materialization row is invalid")
	}
	if existing := e.graphMaterializations[record.ID]; existing != nil &&
		(existing.MutationID != record.MutationID ||
			existing.Digest != record.Digest) {
		return shoal.NewError(
			shoal.ErrorInternal,
			"stored graph materialization has conflicting content")
	}
	copy := record
	e.graphMaterializations[record.ID] = &copy
	return nil
}
