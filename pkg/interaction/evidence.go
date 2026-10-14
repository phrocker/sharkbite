/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package interaction

import (
	"sort"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// EvidenceKind distinguishes exact document citations from graph paths.
type EvidenceKind string

const (
	EvidenceDocument EvidenceKind = "document"
	EvidenceGraph    EvidenceKind = "graph"
)

// AssertionReference binds an authoritative ontology assertion and origin to
// the exact graph edge that materializes it.
type AssertionReference struct {
	AssertionID shoal.ID
	EdgeID      shoal.ID
	Origin      ontology.AssertionOrigin
}

// EvidenceReference is the complete redacted identity of one verified anchor.
// Quotes and generated text are deliberately absent.
type EvidenceReference struct {
	AnchorID   shoal.ID
	Kind       EvidenceKind
	Citation   document.Citation
	NodeIDs    []shoal.ID
	EdgeIDs    []shoal.ID
	Assertions []AssertionReference
}

func (r EvidenceReference) Validate() error {
	if err := shoal.ValidateRequiredID(
		"interaction evidence anchor ID", r.AnchorID); err != nil {
		return err
	}
	for _, id := range r.NodeIDs {
		if err := shoal.ValidateRequiredID(
			"interaction evidence node ID", id); err != nil {
			return err
		}
		if IsInteractionID(id) {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"interaction evidence cannot reference an interaction node")
		}
	}
	for _, id := range r.EdgeIDs {
		if err := shoal.ValidateRequiredID(
			"interaction evidence edge ID", id); err != nil {
			return err
		}
	}
	switch r.Kind {
	case EvidenceDocument:
		if err := r.Citation.Validate(); err != nil {
			return err
		}
		if len(r.EdgeIDs) != 0 || len(r.Assertions) != 0 {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"document evidence cannot contain graph edge references")
		}
		required := []shoal.ID{r.Citation.DocumentID}
		if r.Citation.SectionID != "" {
			required = append(required, r.Citation.SectionID)
		}
		if r.Citation.SpanID != "" {
			required = append(required, r.Citation.SpanID)
		}
		required = dedupeIDs(required)
		actual := dedupeIDs(r.NodeIDs)
		if len(actual) > 3 ||
			(len(actual) != len(required) && len(actual) != 3) {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"document evidence nodes do not match its citation source roles")
		}
		for _, id := range required {
			if !containsEvidenceID(actual, id) {
				return shoal.NewError(
					shoal.ErrorInvalidArgument,
					"document evidence nodes do not match its citation source roles")
			}
		}
	case EvidenceGraph:
		if r.Citation != (document.Citation{}) || len(r.NodeIDs) == 0 {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"graph evidence has an invalid variant")
		}
		if len(r.EdgeIDs) != len(r.NodeIDs)-1 {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"graph evidence path has inconsistent edges")
		}
	default:
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "interaction evidence kind is invalid")
	}
	for _, assertion := range r.Assertions {
		if err := shoal.ValidateRequiredID(
			"interaction assertion ID", assertion.AssertionID); err != nil {
			return err
		}
		if err := shoal.ValidateRequiredID(
			"interaction assertion edge ID", assertion.EdgeID); err != nil {
			return err
		}
		if !containsEvidenceID(r.EdgeIDs, assertion.EdgeID) {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"interaction assertion does not name a referenced edge")
		}
		switch assertion.Origin {
		case ontology.AssertionExplicit,
			ontology.AssertionInferred,
			ontology.AssertionDerived:
		default:
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"interaction assertion origin is invalid")
		}
	}
	return nil
}

func (r EvidenceReference) Canonical() (EvidenceReference, error) {
	if err := r.Validate(); err != nil {
		return EvidenceReference{}, err
	}
	if r.Kind == EvidenceDocument {
		r.NodeIDs = dedupeIDs(r.NodeIDs)
	} else {
		r.NodeIDs = append([]shoal.ID(nil), r.NodeIDs...)
		r.EdgeIDs = append([]shoal.ID(nil), r.EdgeIDs...)
	}
	r.Assertions = append([]AssertionReference(nil), r.Assertions...)
	sort.Slice(r.Assertions, func(i, j int) bool {
		if compared := shoal.CompareID(
			r.Assertions[i].EdgeID, r.Assertions[j].EdgeID); compared != 0 {
			return compared < 0
		}
		if compared := shoal.CompareID(
			r.Assertions[i].AssertionID,
			r.Assertions[j].AssertionID,
		); compared != 0 {
			return compared < 0
		}
		return r.Assertions[i].Origin < r.Assertions[j].Origin
	})
	for index := 1; index < len(r.Assertions); index++ {
		if r.Assertions[index-1].AssertionID ==
			r.Assertions[index].AssertionID &&
			r.Assertions[index-1].EdgeID == r.Assertions[index].EdgeID {
			return EvidenceReference{}, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"interaction evidence contains duplicate assertion references")
		}
	}
	return r, nil
}

func canonicalEvidenceReferences(
	values []EvidenceReference,
) ([]EvidenceReference, error) {
	byAnchor := make(map[shoal.ID]EvidenceReference, len(values))
	for _, value := range values {
		canonical, err := value.Canonical()
		if err != nil {
			return nil, err
		}
		if existing, duplicate := byAnchor[canonical.AnchorID]; duplicate {
			if !evidenceReferencesEqual(existing, canonical) {
				return nil, shoal.NewError(
					shoal.ErrorInvalidArgument,
					"interaction evidence anchor has conflicting references")
			}
			continue
		}
		byAnchor[canonical.AnchorID] = canonical
	}
	result := make([]EvidenceReference, 0, len(byAnchor))
	for _, value := range byAnchor {
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool {
		return shoal.CompareID(result[i].AnchorID, result[j].AnchorID) < 0
	})
	return result, nil
}

func evidenceNodeIDs(values []EvidenceReference) []shoal.ID {
	var ids []shoal.ID
	for _, value := range values {
		ids = append(ids, value.NodeIDs...)
	}
	return dedupeIDs(ids)
}

func evidenceEdgeIDs(values []EvidenceReference) []shoal.ID {
	var ids []shoal.ID
	for _, value := range values {
		ids = append(ids, value.EdgeIDs...)
	}
	return dedupeIDs(ids)
}

func evidenceAssertions(
	values []EvidenceReference,
) []AssertionReference {
	var references []AssertionReference
	for _, value := range values {
		references = append(references, value.Assertions...)
	}
	sort.Slice(references, func(i, j int) bool {
		if compared := shoal.CompareID(
			references[i].EdgeID, references[j].EdgeID); compared != 0 {
			return compared < 0
		}
		if compared := shoal.CompareID(
			references[i].AssertionID,
			references[j].AssertionID,
		); compared != 0 {
			return compared < 0
		}
		return references[i].Origin < references[j].Origin
	})
	result := references[:0]
	for _, reference := range references {
		if len(result) > 0 && result[len(result)-1] == reference {
			continue
		}
		result = append(result, reference)
	}
	return result
}

func evidenceReferencesEqual(left, right EvidenceReference) bool {
	if left.AnchorID != right.AnchorID || left.Kind != right.Kind ||
		left.Citation != right.Citation ||
		len(left.NodeIDs) != len(right.NodeIDs) ||
		len(left.EdgeIDs) != len(right.EdgeIDs) ||
		len(left.Assertions) != len(right.Assertions) {
		return false
	}
	for index := range left.NodeIDs {
		if left.NodeIDs[index] != right.NodeIDs[index] {
			return false
		}
	}
	for index := range left.EdgeIDs {
		if left.EdgeIDs[index] != right.EdgeIDs[index] {
			return false
		}
	}
	for index := range left.Assertions {
		if left.Assertions[index] != right.Assertions[index] {
			return false
		}
	}
	return true
}

func containsEvidenceID(values []shoal.ID, target shoal.ID) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func equalIDs(left, right []shoal.ID) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
