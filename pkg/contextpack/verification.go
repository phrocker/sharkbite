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

package contextpack

import (
	"context"
	"encoding/hex"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// SnapshotReader is the authorization-enforcing hydration seam required to
// verify generated evidence immediately before a product response is emitted.
// Snapshot is checked before and after hydration so a response cannot combine
// evidence from different corpus revisions.
type SnapshotReader interface {
	Reader
	Snapshot(context.Context) (explorer.Snapshot, error)
}

// AuthorizationReader is the authorization-enforcing verification seam.
// Validation is performed before and after hydration so a result cannot be
// accepted under a different, expired, or revoked authorization decision.
type AuthorizationReader interface {
	SnapshotReader
	ValidateAuthorization(context.Context, inference.AuthPin) error
}

// VerifiedSource is one exact source-node identity and its declared
// visibility at the verified snapshot.
type VerifiedSource struct {
	id         shoal.ID
	visibility []string
}

func (s VerifiedSource) ID() shoal.ID { return s.id }

func (s VerifiedSource) Visibility() []string {
	return append([]string(nil), s.visibility...)
}

// VerifiedAnchor is an evidence anchor rehydrated from the authorized source
// reader. Sources are the complete, uncapped set of source nodes represented by
// the anchor. Visibility also includes any edge-local visibility on a path.
type VerifiedAnchor struct {
	anchor     inference.EvidenceAnchor
	sources    []VerifiedSource
	assertions []VerifiedAssertion
	visibility []string
	addition   bool
}

// VerifiedAssertion is an authoritative assertion attached to one verified
// path edge.
type VerifiedAssertion struct {
	assertionID shoal.ID
	edgeID      shoal.ID
	origin      ontology.AssertionOrigin
}

func (a VerifiedAssertion) AssertionID() shoal.ID { return a.assertionID }
func (a VerifiedAssertion) EdgeID() shoal.ID      { return a.edgeID }
func (a VerifiedAssertion) Origin() ontology.AssertionOrigin {
	return a.origin
}

func (a VerifiedAnchor) Anchor() inference.EvidenceAnchor { return a.anchor }

func (a VerifiedAnchor) Sources() []VerifiedSource {
	result := make([]VerifiedSource, len(a.sources))
	for index, source := range a.sources {
		result[index] = VerifiedSource{
			id: source.id, visibility: source.Visibility(),
		}
	}
	return result
}

func (a VerifiedAnchor) Visibility() []string {
	return append([]string(nil), a.visibility...)
}

func (a VerifiedAnchor) Assertions() []VerifiedAssertion {
	return append([]VerifiedAssertion(nil), a.assertions...)
}

// EvidenceReference projects this verified anchor onto the complete redacted
// interaction provenance contract.
func (a VerifiedAnchor) EvidenceReference() (
	interaction.EvidenceReference, error,
) {
	reference := interaction.EvidenceReference{AnchorID: a.anchor.ID()}
	for _, source := range a.sources {
		reference.NodeIDs = append(reference.NodeIDs, source.ID())
	}
	switch a.anchor.Kind() {
	case inference.AnchorDocument:
		citation, _, ok := a.anchor.Document()
		if !ok {
			return interaction.EvidenceReference{}, invalid(
				"verified document evidence is unavailable")
		}
		if len(a.sources) != 3 ||
			a.sources[0].ID() != citation.DocumentID {
			return interaction.EvidenceReference{}, invalid(
				"verified document evidence has incomplete resolved source roles")
		}
		reference.Kind = interaction.EvidenceDocument
		reference.Citation = citation
	case inference.AnchorGraph:
		path, ok := a.anchor.Path()
		if !ok {
			return interaction.EvidenceReference{}, invalid(
				"verified graph evidence is unavailable")
		}
		reference.Kind = interaction.EvidenceGraph
		for _, edge := range path.Edges {
			reference.EdgeIDs = append(reference.EdgeIDs, edge.ID)
		}
		for _, assertion := range a.assertions {
			reference.Assertions = append(
				reference.Assertions,
				interaction.AssertionReference{
					AssertionID: assertion.assertionID,
					EdgeID:      assertion.edgeID,
					Origin:      assertion.origin,
				},
			)
		}
	default:
		return interaction.EvidenceReference{}, invalid(
			"verified evidence kind is unsupported")
	}
	return reference.Canonical()
}

// Addition reports whether this evidence was added by the generator rather
// than present in the original context pack.
func (a VerifiedAnchor) Addition() bool { return a.addition }

// ResultVerification is the immutable result of re-verifying one generated
// result against its exact context pack and authorized corpus snapshot.
type ResultVerification struct {
	contextPackID shoal.ID
	resultID      shoal.ID
	snapshot      inference.SnapshotPin
	anchors       []VerifiedAnchor
}

func (v ResultVerification) ContextPackID() shoal.ID { return v.contextPackID }
func (v ResultVerification) ResultID() shoal.ID      { return v.resultID }
func (v ResultVerification) Snapshot() inference.SnapshotPin {
	return v.snapshot
}

func (v ResultVerification) Anchors() []VerifiedAnchor {
	result := make([]VerifiedAnchor, len(v.anchors))
	for index, anchor := range v.anchors {
		result[index] = VerifiedAnchor{
			anchor:     anchor.anchor,
			sources:    anchor.Sources(),
			assertions: anchor.Assertions(),
			visibility: anchor.Visibility(),
			addition:   anchor.addition,
		}
	}
	return result
}

// VerifyResult rehydrates every original and generator-added anchor, performs
// the same exact quote/path checks used by Builder, and requires the source
// snapshot to remain equal to the context pack before and after verification.
func (b Builder) VerifyResult(
	ctx context.Context,
	pack inference.ContextPack,
	result inference.InferenceResult,
) (ResultVerification, error) {
	if err := contextError(ctx); err != nil {
		return ResultVerification{}, err
	}
	if err := result.ValidateFor(pack); err != nil {
		return ResultVerification{}, err
	}
	reader, ok := b.Reader.(AuthorizationReader)
	if !ok || reader == nil || nilSnapshotReader(reader) {
		return ResultVerification{}, invalid(
			"result verification requires an authorization-aware snapshot reader")
	}
	limits, err := normalizeLimits(b.Limits)
	if err != nil {
		return ResultVerification{}, err
	}
	before, err := reader.Snapshot(ctx)
	if err != nil {
		return ResultVerification{}, err
	}
	if err := verifySnapshot(pack.Snapshot(), before); err != nil {
		return ResultVerification{}, err
	}
	if err := reader.ValidateAuthorization(
		ctx, pack.Authorization()); err != nil {
		return ResultVerification{}, err
	}

	verifier, err := newVerifier(ctx, reader, limits, nil, nil)
	if err != nil {
		return ResultVerification{}, err
	}
	original := pack.Evidence()
	additions := result.EvidenceAdditions()
	if len(original)+len(additions) > limits.MaxAnchors {
		return ResultVerification{}, invalid(
			"verified result exceeds the evidence anchor bound")
	}
	anchors := make([]VerifiedAnchor, 0, len(original)+len(additions))
	for _, entry := range []struct {
		values   []inference.EvidenceAnchor
		addition bool
	}{
		{values: original},
		{values: additions, addition: true},
	} {
		for _, anchor := range entry.values {
			verified, err := verifier.verifyAnchor(anchor, entry.addition)
			if err != nil {
				return ResultVerification{}, fmt.Errorf(
					"verify evidence anchor %q: %w", anchor.ID(), err)
			}
			anchors = append(anchors, verified)
		}
	}
	sort.Slice(anchors, func(i, j int) bool {
		return shoal.CompareID(
			anchors[i].anchor.ID(), anchors[j].anchor.ID(),
		) < 0
	})

	after, err := reader.Snapshot(ctx)
	if err != nil {
		return ResultVerification{}, err
	}
	if before.ID != after.ID || !before.AsOf.Equal(after.AsOf) {
		return ResultVerification{}, shoal.NewError(
			shoal.ErrorUnavailable,
			"corpus snapshot changed while evidence was being verified",
		)
	}
	if err := verifySnapshot(pack.Snapshot(), after); err != nil {
		return ResultVerification{}, err
	}
	if err := reader.ValidateAuthorization(
		ctx, pack.Authorization()); err != nil {
		return ResultVerification{}, err
	}
	return ResultVerification{
		contextPackID: pack.ID(),
		resultID:      result.ID(),
		snapshot:      pack.Snapshot(),
		anchors:       anchors,
	}, nil
}

func nilSnapshotReader(reader SnapshotReader) bool {
	value := reflect.ValueOf(reader)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

func (v *verifier) verifyAnchor(
	anchor inference.EvidenceAnchor,
	addition bool,
) (VerifiedAnchor, error) {
	switch anchor.Kind() {
	case inference.AnchorDocument:
		citation, quote, ok := anchor.Document()
		if !ok {
			return VerifiedAnchor{}, invalid(
				"document evidence variant is unavailable")
		}
		if _, err := v.documentAnchor(citation, quote); err != nil {
			return VerifiedAnchor{}, err
		}
		index := v.documents[documentKey{
			documentID: citation.DocumentID,
			revisionID: citation.RevisionID,
		}]
		sources, visibility, err := verifiedDocumentSources(index, citation, quote)
		if err != nil {
			return VerifiedAnchor{}, err
		}
		return VerifiedAnchor{
			anchor:     anchor,
			sources:    sources,
			visibility: visibility,
			addition:   addition,
		}, nil
	case inference.AnchorGraph:
		path, ok := anchor.Path()
		if !ok {
			return VerifiedAnchor{}, invalid(
				"graph evidence variant is unavailable")
		}
		verifiedAnchor, assertions, err := v.verifyResultGraphAnchor(
			anchor, path)
		if err != nil {
			return VerifiedAnchor{}, err
		}
		sources := make([]VerifiedSource, 0, len(path.Nodes))
		visibilitySets := make([][]string, 0, len(path.Nodes)+len(path.Edges))
		for _, node := range path.Nodes {
			labels, err := interaction.NodeVisibility(node)
			if err != nil {
				return VerifiedAnchor{}, err
			}
			sources = append(sources, VerifiedSource{
				id: node.ID, visibility: labels,
			})
			visibilitySets = append(visibilitySets, labels)
		}
		for _, edge := range path.Edges {
			labels, err := interaction.EdgeVisibility(edge)
			if err != nil {
				return VerifiedAnchor{}, err
			}
			visibilitySets = append(visibilitySets, labels)
		}
		visibility, err := interaction.Conjoin(visibilitySets...)
		if err != nil {
			return VerifiedAnchor{}, err
		}
		return VerifiedAnchor{
			anchor:     verifiedAnchor,
			sources:    sources,
			assertions: assertions,
			visibility: visibility,
			addition:   addition,
		}, nil
	default:
		return VerifiedAnchor{}, invalid("unknown evidence anchor kind")
	}
}

func (v *verifier) verifyResultGraphAnchor(
	anchor inference.EvidenceAnchor,
	path graph.Path,
) (inference.EvidenceAnchor, []VerifiedAssertion, error) {
	if err := path.Validate(); err != nil {
		return inference.EvidenceAnchor{}, nil, err
	}
	if len(path.Nodes) > v.limits.MaxPathNodes ||
		len(path.Edges) > v.limits.MaxGraphEdges {
		return inference.EvidenceAnchor{}, nil, invalid(
			"graph evidence path exceeds verification bounds")
	}
	path = clonePath(path)
	nodeIDs := make([]shoal.ID, 0, len(path.Nodes))
	missing := false
	for index, node := range path.Nodes {
		node = canonicalNode(node)
		path.Nodes[index] = node
		nodeIDs = append(nodeIDs, node.ID)
		if existing, ok := v.nodes[node.ID]; !ok ||
			!canonicalEqual(existing, node) {
			missing = true
		}
	}
	for _, edge := range path.Edges {
		if existing, ok := v.edges[edge.ID]; !ok ||
			!canonicalEqual(existing, edge) {
			missing = true
		}
	}
	if missing {
		if v.reader == nil {
			return inference.EvidenceAnchor{}, nil, invalid(
				"graph hydration is required for verification")
		}
		request := explorer.NeighborhoodRequest{NodeIDs: nodeIDs, Depth: 1}
		neighborhood, err := v.reader.Neighborhood(v.ctx, request)
		if err != nil {
			return inference.EvidenceAnchor{}, nil, err
		}
		if err := validateNeighborhoodResponse(
			request, neighborhood, v.limits); err != nil {
			return inference.EvidenceAnchor{}, nil, err
		}
		if err := v.addNeighborhood(neighborhood); err != nil {
			return inference.EvidenceAnchor{}, nil, err
		}
	}
	for _, node := range path.Nodes {
		existing, ok := v.nodes[node.ID]
		if !ok || !canonicalEqual(existing, node) {
			return inference.EvidenceAnchor{}, nil, invalid(
				"graph path node does not match hydrated Explorer data")
		}
	}
	for _, edge := range path.Edges {
		existing, ok := v.edges[edge.ID]
		if !ok || !canonicalEqual(existing, edge) {
			return inference.EvidenceAnchor{}, nil, invalid(
				"graph path edge does not match hydrated Explorer data")
		}
	}
	assertions, err := v.assertionsForPath(path)
	if err != nil {
		return inference.EvidenceAnchor{}, nil, err
	}
	verified, err := inference.NewGraphAnchorWithAssertions(
		path, verifiedAssertionReferences(assertions))
	if err != nil {
		return inference.EvidenceAnchor{}, nil, err
	}
	if verified.ID() != anchor.ID() {
		return inference.EvidenceAnchor{}, nil, invalid(
			"graph evidence assertions do not match authoritative provenance")
	}
	return verified, assertions, nil
}

func verifiedAssertionReferences(
	assertions []VerifiedAssertion,
) []interaction.AssertionReference {
	result := make([]interaction.AssertionReference, len(assertions))
	for index, assertion := range assertions {
		result[index] = interaction.AssertionReference{
			AssertionID: assertion.assertionID,
			EdgeID:      assertion.edgeID,
			Origin:      assertion.origin,
		}
	}
	return result
}

func (v *verifier) assertionsForPath(
	path graph.Path,
) ([]VerifiedAssertion, error) {
	assertions := make([]VerifiedAssertion, 0, len(path.Edges))
	for _, edge := range path.Edges {
		references := v.assertions[edge.ID]
		hasAssertion := len(references) > 0
		hasAssertionMarker :=
			edge.Properties["ontology_relationship_id"] != "" ||
				edge.Properties["ontology.assertion.id"] != "" ||
				edge.Properties["ontology.assertion.origin"] != ""
		if hasAssertionMarker && !hasAssertion {
			return nil, invalid(
				"graph path edge is missing its authoritative assertion")
		}
		if !hasAssertion {
			continue
		}
		for _, reference := range references {
			if reference.EdgeID != edge.ID {
				return nil, invalid(
					"graph path assertion does not match its edge")
			}
			if assertionID := edge.Properties["ontology.assertion.id"]; assertionID != "" &&
				shoal.ID(assertionID) != reference.AssertionID {
				return nil, invalid(
					"graph path assertion ID does not match its authoritative edge")
			}
			if origin := edge.Properties["ontology.assertion.origin"]; origin != "" && origin != string(reference.Origin) {
				return nil, invalid(
					"graph path origin does not match its authoritative assertion")
			}
			assertions = append(assertions, VerifiedAssertion{
				assertionID: reference.AssertionID,
				edgeID:      reference.EdgeID,
				origin:      reference.Origin,
			})
		}
	}
	sort.Slice(assertions, func(i, j int) bool {
		return shoal.CompareID(
			assertions[i].edgeID, assertions[j].edgeID) < 0
	})
	return assertions, nil
}

func verifiedDocumentSources(
	index *documentIndex,
	citation document.Citation,
	quote string,
) ([]VerifiedSource, []string, error) {
	if index == nil {
		return nil, nil, invalid("verified document index is unavailable")
	}
	var span document.Span
	if citation.SpanID != "" {
		var ok bool
		span, ok = index.spans[citation.SpanID]
		if !ok {
			return nil, nil, shoal.NewError(
				shoal.ErrorNotFound, "cited span was not found")
		}
	} else {
		for _, candidate := range index.spansBySection[citation.SectionID] {
			if !rangeContains(candidate.Range, citation.Range) {
				continue
			}
			start := citation.Range.Start.Offset - candidate.Range.Start.Offset
			end := citation.Range.End.Offset - candidate.Range.Start.Offset
			if start < 0 || end < start || end > int64(len(candidate.Text)) {
				continue
			}
			if candidate.Text[start:end] != quote {
				continue
			}
			if span.ID != "" {
				return nil, nil, invalid(
					"section citation resolves to more than one source span")
			}
			span = candidate
		}
		if span.ID == "" {
			return nil, nil, invalid(
				"section citation does not resolve to an exact source span")
		}
	}

	documentVisibility, err := visibilityFromMetadata(index.view.Document.Metadata)
	if err != nil {
		return nil, nil, err
	}
	revisionVisibility, err := visibilityFromMetadata(index.view.Revision.Metadata)
	if err != nil {
		return nil, nil, err
	}
	sectionVisibility, err := visibilityFromMetadata(
		index.sections[span.SectionID].Metadata)
	if err != nil {
		return nil, nil, err
	}
	spanVisibility, err := visibilityFromMetadata(span.Metadata)
	if err != nil {
		return nil, nil, err
	}
	documentVisibility, err = interaction.Conjoin(
		documentVisibility, revisionVisibility)
	if err != nil {
		return nil, nil, err
	}
	sectionVisibility, err = interaction.Conjoin(
		documentVisibility, sectionVisibility)
	if err != nil {
		return nil, nil, err
	}
	spanVisibility, err = interaction.Conjoin(
		sectionVisibility, spanVisibility)
	if err != nil {
		return nil, nil, err
	}
	visibility, err := interaction.Conjoin(
		documentVisibility, sectionVisibility, spanVisibility)
	if err != nil {
		return nil, nil, err
	}
	return []VerifiedSource{
		{id: citation.DocumentID, visibility: documentVisibility},
		{id: span.SectionID, visibility: sectionVisibility},
		{id: span.ID, visibility: spanVisibility},
	}, visibility, nil
}

func visibilityFromMetadata(metadata shoal.Metadata) ([]string, error) {
	if metadata == nil {
		return nil, nil
	}
	return interaction.ParseVisibility(metadata[interaction.PropertyVisibility])
}

func verifySnapshot(pin inference.SnapshotPin, snapshot explorer.Snapshot) error {
	if err := shoal.ValidateRequiredID(
		"verified snapshot ID", shoal.ID(snapshot.ID),
	); err != nil {
		return err
	}
	if snapshot.AsOf.IsZero() {
		return invalid("verified snapshot time is required")
	}
	if shoal.ID(snapshot.ID) != pin.ID() ||
		!snapshot.AsOf.UTC().Equal(pin.AsOf()) {
		return shoal.NewError(
			shoal.ErrorUnavailable,
			"verified source snapshot does not match the context pack",
		)
	}
	return nil
}

// PolicyID returns the trusted opaque policy identity pinned by Builder.
func PolicyID(pack inference.ContextPack) (shoal.ID, error) {
	id, present, err := metadataID(
		pack, metadataPolicyKey, "context policy identity")
	if err != nil {
		return "", err
	}
	if !present {
		return "", invalid("context policy identity is missing")
	}
	return id, nil
}

// RetrievalRequestID returns the optional opaque retrieval request identity
// pinned by Builder.
func RetrievalRequestID(
	pack inference.ContextPack,
) (shoal.ID, bool, error) {
	return metadataID(
		pack, metadataRequestKey, "context retrieval request identity")
}

func metadataID(
	pack inference.ContextPack,
	key string,
	name string,
) (shoal.ID, bool, error) {
	value := pack.Metadata()[key]
	if value == "" {
		return "", false, nil
	}
	const prefix = "hex:"
	if !strings.HasPrefix(value, prefix) {
		return "", false, invalid(name + " is invalid")
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, prefix))
	if err != nil {
		return "", false, invalid(name + " is invalid")
	}
	id := shoal.ID(decoded)
	if id == "" {
		return "", false, nil
	}
	if err := shoal.ValidateRequiredID(name, id); err != nil {
		return "", false, err
	}
	return id, true, nil
}
