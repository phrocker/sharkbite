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

package webapi

import (
	"sort"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/reasoning"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// CitationEvidenceProjection is the canonical, transport-independent evidence
// view shared by HTTP and MCP adapters. It deliberately contains domain IDs,
// not presentation links or encoded wire IDs.
type CitationEvidenceProjection struct {
	RetrievedSourceIDs  []shoal.ID
	CitedSourceIDs      []shoal.ID
	EdgeIDs             []shoal.ID
	AssertionIDs        []shoal.ID
	Anchors             []CitationEvidenceAnchor
	EffectiveVisibility []string
	OutputVisibility    string
	EmbeddingSpaceIDs   []shoal.ID
}

// CitationEvidenceAnchor preserves one complete verified evidence anchor.
type CitationEvidenceAnchor struct {
	AnchorID   shoal.ID
	Status     reasoning.VerificationStatus
	Use        reasoning.EvidenceUse
	Origin     reasoning.EvidenceOrigin
	SourceIDs  []shoal.ID
	EdgeIDs    []shoal.ID
	Assertions []CitationEvidenceAssertion
	Citation   *document.Citation
	Visibility []string
	SourceURI  string
}

type CitationEvidenceAssertion struct {
	AssertionID shoal.ID
	EdgeID      shoal.ID
	Origin      ontology.AssertionOrigin
}

// EvidenceProjection returns complete evidence without applying presentation
// truncation. Validation prevents adapters from projecting a forged or
// internally inconsistent envelope.
func (e CitationEnvelope) EvidenceProjection() (CitationEvidenceProjection, error) {
	if err := e.Validate(); err != nil {
		return CitationEvidenceProjection{}, err
	}
	return projectCitationEvidence(e), nil
}

// InteractionEvidence returns complete canonical evidence from the exact
// durable interaction session accepted while constructing this envelope.
// Wire-decoded envelopes intentionally do not regain this trusted capability.
func (e CitationEnvelope) InteractionEvidence() (
	retrieved []interaction.EvidenceReference,
	cited []interaction.EvidenceReference,
	err error,
) {
	session, err := e.recordedSession.Canonical()
	if err != nil {
		return nil, nil, err
	}
	retrieved = sessionRetrievedEvidence(session)
	cited = append([]interaction.EvidenceReference(nil), session.CitedEvidence...)
	return retrieved, cited, nil
}

func sessionRetrievedEvidence(session interaction.Session) []interaction.EvidenceReference {
	values := append([]interaction.EvidenceReference(nil), session.SeedEvidence...)
	for _, turn := range session.Turns {
		if turn.ToolCall != nil {
			values = append(values, turn.ToolCall.RetrievedEvidence...)
		}
	}
	return values
}

func projectCitationEvidence(e CitationEnvelope) CitationEvidenceProjection {
	result := CitationEvidenceProjection{
		RetrievedSourceIDs:  canonicalProjectionIDs(e.RetrievedSourceIDs),
		CitedSourceIDs:      canonicalProjectionIDs(e.CitedSourceIDs),
		EffectiveVisibility: append([]string(nil), e.EffectiveVisibility...),
		OutputVisibility:    e.OutputVisibility,
		EmbeddingSpaceIDs:   canonicalProjectionIDs(e.EmbeddingSpaceIDs),
		Anchors:             make([]CitationEvidenceAnchor, 0, len(e.Evidence)),
	}

	var edgeIDs []shoal.ID
	var assertionIDs []shoal.ID
	for _, evidence := range e.Evidence {
		anchor := CitationEvidenceAnchor{
			AnchorID:   evidence.AnchorID,
			Status:     evidence.Status,
			Use:        evidence.Use,
			Origin:     evidence.Origin,
			SourceIDs:  canonicalProjectionIDs(evidence.SourceIDs),
			Visibility: append([]string(nil), evidence.Visibility...),
			SourceURI:  evidence.SourceURI,
		}
		if evidence.Citation != nil {
			citation := *evidence.Citation
			anchor.Citation = &citation
		}
		if evidence.Path != nil {
			for _, edge := range evidence.Path.Edges {
				anchor.EdgeIDs = append(anchor.EdgeIDs, edge.ID)
			}
		}
		for _, assertion := range evidence.Assertions {
			anchor.Assertions = append(
				anchor.Assertions,
				CitationEvidenceAssertion{
					AssertionID: assertion.AssertionID,
					EdgeID:      assertion.EdgeID,
					Origin:      assertion.Origin,
				},
			)
			anchor.EdgeIDs = append(anchor.EdgeIDs, assertion.EdgeID)
			assertionIDs = append(assertionIDs, assertion.AssertionID)
		}
		anchor.EdgeIDs = canonicalProjectionIDs(anchor.EdgeIDs)
		sort.Slice(anchor.Assertions, func(i, j int) bool {
			if comparison := shoal.CompareID(
				anchor.Assertions[i].AssertionID,
				anchor.Assertions[j].AssertionID,
			); comparison != 0 {
				return comparison < 0
			}
			return shoal.CompareID(
				anchor.Assertions[i].EdgeID,
				anchor.Assertions[j].EdgeID,
			) < 0
		})
		edgeIDs = append(edgeIDs, anchor.EdgeIDs...)
		result.Anchors = append(result.Anchors, anchor)
	}
	sort.Slice(result.Anchors, func(i, j int) bool {
		return shoal.CompareID(
			result.Anchors[i].AnchorID,
			result.Anchors[j].AnchorID,
		) < 0
	})
	result.EdgeIDs = canonicalProjectionIDs(edgeIDs)
	result.AssertionIDs = canonicalProjectionIDs(assertionIDs)
	return result
}

func canonicalProjectionIDs(values []shoal.ID) []shoal.ID {
	if len(values) == 0 {
		return nil
	}
	result := append([]shoal.ID(nil), values...)
	sort.Slice(result, func(i, j int) bool {
		return shoal.CompareID(result[i], result[j]) < 0
	})
	unique := result[:0]
	for _, value := range result {
		if len(unique) == 0 || unique[len(unique)-1] != value {
			unique = append(unique, value)
		}
	}
	return unique
}

type InteractionProviderMethod string

const (
	InteractionMethodList    InteractionProviderMethod = "list"
	InteractionMethodInspect InteractionProviderMethod = "inspect"
	InteractionMethodFold    InteractionProviderMethod = "fold"
	InteractionMethodUnfold  InteractionProviderMethod = "unfold"
)

type InteractionProviderSemantics struct {
	Operation  auth.Operation
	ReadOnly   bool
	Mutating   bool
	Idempotent bool
}

// InteractionSemantics gives transports the exact preauthorization and tool
// annotations for each InteractionProvider method. All provenance access uses
// read authority; fold is a content-addressed durable mutation and repeated
// equivalent requests return the original fold. Unfold only rehydrates it.
func InteractionSemantics(
	method InteractionProviderMethod,
) (InteractionProviderSemantics, error) {
	switch method {
	case InteractionMethodList, InteractionMethodInspect,
		InteractionMethodUnfold:
		return InteractionProviderSemantics{
			Operation: auth.OperationRead, ReadOnly: true, Idempotent: true,
		}, nil
	case InteractionMethodFold:
		return InteractionProviderSemantics{
			Operation:  auth.OperationRead,
			Mutating:   true,
			Idempotent: true,
		}, nil
	default:
		return InteractionProviderSemantics{}, chatInvalid(
			"unknown interaction provider method")
	}
}
