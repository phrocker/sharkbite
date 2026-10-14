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

package explorer

import (
	"fmt"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const graphAssertionEdgeIDMetadata = "shoal.graph.edge_id"

type persistedExtractionAssertion struct {
	EdgeID            shoal.ID
	Subject           shoal.ID
	SubjectType       shoal.ID
	Predicate         shoal.ID
	Object            persistedExtractionValue
	ObjectType        shoal.ID
	Origin            string
	Confidence        float64
	Evidence          []persistedEvidenceRef
	Provenance        persistedExtractionProvenance
	OntologySchemaID  shoal.ID
	OntologyVersionID shoal.ID
	Metadata          shoal.Metadata
}

type persistedExtractionValue struct {
	Type      string
	Text      string
	Integer   int64
	Number    float64
	Boolean   bool
	Timestamp time.Time
	Reference shoal.ID
}

type persistedEvidenceRef struct {
	Citation      document.Citation
	Quote         string
	Path          graph.Path
	HasPath       bool
	HasDerivation bool
	Metadata      shoal.Metadata
}

type persistedExtractionProvenance struct {
	Provider         string
	Model            string
	ModelVersion     string
	Prompt           string
	PromptVersion    string
	Extractor        string
	ExtractorVersion string
	Metadata         shoal.Metadata
}

func persistAssertion(
	edgeID shoal.ID,
	assertion ontology.Assertion,
) (persistedExtractionAssertion, error) {
	identity, _ := assertion.Ontology()
	subjectType, _ := assertion.SubjectType()
	objectType, _ := assertion.ObjectType()
	evidence := assertion.Evidence()
	persistedEvidence := make([]persistedEvidenceRef, 0, len(evidence))
	for _, item := range evidence {
		if _, ok := item.Derivation(); ok {
			return persistedExtractionAssertion{}, fmt.Errorf(
				"extraction relationship assertion cannot persist derivation evidence")
		}
		path, hasPath := item.Path()
		persistedEvidence = append(persistedEvidence, persistedEvidenceRef{
			Citation: item.Citation(), Quote: item.Quote(), Path: path,
			HasPath: hasPath, Metadata: item.Metadata(),
		})
	}
	metadata := assertion.Metadata()
	if metadata == nil {
		metadata = shoal.Metadata{}
	}
	// This edge metadata is load-bearing; TestExtractDocumentAuthorizationControlsDerivedGraph pins that authorized reads can retain extracted relationship assertions after edge filtering.
	metadata[graphAssertionEdgeIDMetadata] = string(edgeID)
	return persistedExtractionAssertion{
		EdgeID: edgeID, Subject: assertion.Subject(), SubjectType: subjectType,
		Predicate: assertion.Predicate(), Object: persistValue(assertion.Object()),
		ObjectType: objectType, Origin: string(assertion.Origin()),
		Confidence: float64(assertion.Confidence()), Evidence: persistedEvidence,
		Provenance:        persistProvenance(assertion.Provenance()),
		OntologySchemaID:  identity.SchemaID(),
		OntologyVersionID: identity.VersionID(),
		Metadata:          metadata,
	}, nil
}

func restoreAssertion(
	persisted persistedExtractionAssertion,
) (ontology.Assertion, error) {
	value, err := restoreValue(persisted.Object)
	if err != nil {
		return ontology.Assertion{}, err
	}
	evidence := make([]ontology.EvidenceRef, 0, len(persisted.Evidence))
	for _, item := range persisted.Evidence {
		var options []ontology.EvidenceOption
		if item.HasPath {
			options = append(options, ontology.WithEvidencePath(item.Path))
		}
		ref, err := ontology.NewEvidenceRef(
			item.Citation, item.Quote, item.Metadata, options...)
		if err != nil {
			return ontology.Assertion{}, err
		}
		evidence = append(evidence, ref)
	}
	provenance, err := ontology.NewExtractionProvenance(
		persisted.Provenance.Provider,
		persisted.Provenance.Model,
		persisted.Provenance.ModelVersion,
		persisted.Provenance.Prompt,
		persisted.Provenance.PromptVersion,
		persisted.Provenance.Extractor,
		persisted.Provenance.ExtractorVersion,
		persisted.Provenance.Metadata,
	)
	if err != nil {
		return ontology.Assertion{}, err
	}
	identity, err := ontology.NewOntologyIdentityFromIDs(
		persisted.OntologySchemaID, persisted.OntologyVersionID)
	if err != nil {
		return ontology.Assertion{}, err
	}
	options := []ontology.AssertionOption{
		ontology.WithAssertionOntology(identity),
	}
	if persisted.SubjectType != "" {
		options = append(options, ontology.WithAssertionSubjectType(persisted.SubjectType))
	}
	if persisted.ObjectType != "" {
		options = append(options, ontology.WithAssertionObjectType(persisted.ObjectType))
	}
	assertion, err := ontology.NewAssertion(
		persisted.Subject,
		persisted.Predicate,
		value,
		ontology.AssertionOrigin(persisted.Origin),
		shoal.Score(persisted.Confidence),
		evidence,
		provenance,
		persisted.Metadata,
		options...,
	)
	if err != nil {
		return ontology.Assertion{}, err
	}
	return assertion, nil
}

func persistValue(value ontology.Value) persistedExtractionValue {
	out := persistedExtractionValue{Type: string(value.Type())}
	switch value.Type() {
	case ontology.ValueString:
		out.Text, _ = value.StringValue()
	case ontology.ValueInteger:
		out.Integer, _ = value.IntegerValue()
	case ontology.ValueNumber:
		out.Number, _ = value.NumberValue()
	case ontology.ValueBoolean:
		out.Boolean, _ = value.BooleanValue()
	case ontology.ValueTimestamp:
		out.Timestamp, _ = value.TimestampValue()
	case ontology.ValueReference:
		out.Reference, _ = value.ReferenceValue()
	}
	return out
}

func restoreValue(value persistedExtractionValue) (ontology.Value, error) {
	switch ontology.ValueType(value.Type) {
	case ontology.ValueString:
		return ontology.NewStringValue(value.Text)
	case ontology.ValueInteger:
		return ontology.NewIntegerValue(value.Integer), nil
	case ontology.ValueNumber:
		return ontology.NewNumberValue(value.Number)
	case ontology.ValueBoolean:
		return ontology.NewBooleanValue(value.Boolean), nil
	case ontology.ValueTimestamp:
		return ontology.NewTimestampValue(value.Timestamp)
	case ontology.ValueReference:
		return ontology.NewReferenceValue(value.Reference)
	default:
		return ontology.Value{}, fmt.Errorf("unknown persisted ontology value type %q", value.Type)
	}
}

func persistProvenance(value ontology.ExtractionProvenance) persistedExtractionProvenance {
	return persistedExtractionProvenance{
		Provider: value.Provider(), Model: value.Model(),
		ModelVersion: value.ModelVersion(), Prompt: value.Prompt(),
		PromptVersion: value.PromptVersion(), Extractor: value.Extractor(),
		ExtractorVersion: value.ExtractorVersion(), Metadata: value.Metadata(),
	}
}
