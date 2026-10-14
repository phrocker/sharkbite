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

package ontology

import (
	"context"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// ExtractionContractVersion identifies the public extraction envelope.
type ExtractionContractVersion string

const (
	// ExtractionContractV1 is the fail-closed grounded extraction contract.
	ExtractionContractV1 ExtractionContractVersion = "v1"
)

const (
	hardMaxEvidence         = 4096
	hardMaxAssertions       = 16384
	hardMaxProposals        = 1024
	hardMaxQuoteBytes       = 1 << 20
	hardMaxInstructionBytes = 1 << 20
	hardMaxPathNodes        = 16384
	hardMaxPathEdges        = 32768
	hardMaxSchemaMembers    = 32768
	hardMaxMetadataEntries  = 4096
	hardMaxMetadataBytes    = 1 << 20
	hardMaxPayloadBytes     = 64 << 20
)

// ExtractionLimits bounds one v1 extraction envelope. Zero assertions or
// proposals disables that output category.
type ExtractionLimits struct {
	MaxEvidence         uint32
	MaxAssertions       uint32
	MaxProposals        uint32
	MaxQuoteBytes       uint32
	MaxInstructionBytes uint32
	MaxPathNodes        uint32
	MaxPathEdges        uint32
	MaxSchemaMembers    uint32
	MaxMetadataEntries  uint32
	MaxMetadataBytes    uint32
	MaxPayloadBytes     uint32
}

// DefaultExtractionLimits returns conservative v1 limits.
func DefaultExtractionLimits() ExtractionLimits {
	return ExtractionLimits{
		MaxEvidence: 256, MaxAssertions: 1024, MaxProposals: 64,
		MaxQuoteBytes: 64 << 10, MaxInstructionBytes: 64 << 10,
		MaxPathNodes: 1024, MaxPathEdges: 2048, MaxSchemaMembers: 4096,
		MaxMetadataEntries: 256, MaxMetadataBytes: 64 << 10,
		MaxPayloadBytes: 4 << 20,
	}
}

// Validate checks non-zero input bounds and fixed v1 safety ceilings.
func (l ExtractionLimits) Validate() error {
	if l.MaxEvidence == 0 || l.MaxEvidence > hardMaxEvidence ||
		l.MaxAssertions > hardMaxAssertions ||
		l.MaxProposals > hardMaxProposals ||
		l.MaxQuoteBytes == 0 || l.MaxQuoteBytes > hardMaxQuoteBytes ||
		l.MaxInstructionBytes == 0 ||
		l.MaxInstructionBytes > hardMaxInstructionBytes ||
		l.MaxPathNodes == 0 || l.MaxPathNodes > hardMaxPathNodes ||
		l.MaxPathEdges == 0 || l.MaxPathEdges > hardMaxPathEdges ||
		l.MaxSchemaMembers == 0 || l.MaxSchemaMembers > hardMaxSchemaMembers ||
		l.MaxMetadataEntries == 0 ||
		l.MaxMetadataEntries > hardMaxMetadataEntries ||
		l.MaxMetadataBytes == 0 || l.MaxMetadataBytes > hardMaxMetadataBytes ||
		l.MaxPayloadBytes == 0 || l.MaxPayloadBytes > hardMaxPayloadBytes {
		return invalid("extraction limits are outside v1 safety bounds")
	}
	return nil
}

func (l ExtractionLimits) canonical() string {
	return canonicalParts(
		strconv.FormatUint(uint64(l.MaxEvidence), 10),
		strconv.FormatUint(uint64(l.MaxAssertions), 10),
		strconv.FormatUint(uint64(l.MaxProposals), 10),
		strconv.FormatUint(uint64(l.MaxQuoteBytes), 10),
		strconv.FormatUint(uint64(l.MaxInstructionBytes), 10),
		strconv.FormatUint(uint64(l.MaxPathNodes), 10),
		strconv.FormatUint(uint64(l.MaxPathEdges), 10),
		strconv.FormatUint(uint64(l.MaxSchemaMembers), 10),
		strconv.FormatUint(uint64(l.MaxMetadataEntries), 10),
		strconv.FormatUint(uint64(l.MaxMetadataBytes), 10),
		strconv.FormatUint(uint64(l.MaxPayloadBytes), 10),
	)
}

// ExtractionRequest supplies an immutable schema snapshot and cited evidence
// to an extractor.
type ExtractionRequest struct {
	id              shoal.ID
	contractVersion ExtractionContractVersion
	version         OntologyVersion
	evidence        []EvidenceRef
	instructions    string
	provenance      ExtractionProvenance
	limits          ExtractionLimits
	metadata        shoal.Metadata
}

// NewExtractionRequest creates a canonical extraction request.
func NewExtractionRequest(
	version OntologyVersion,
	evidence []EvidenceRef,
	instructions string,
	provenance ExtractionProvenance,
	limits ExtractionLimits,
	metadata shoal.Metadata,
) (ExtractionRequest, error) {
	if err := preflightExtractionRequest(
		version, evidence, instructions, provenance, limits, metadata,
	); err != nil {
		return ExtractionRequest{}, err
	}
	request := ExtractionRequest{
		contractVersion: ExtractionContractV1,
		version:         version.clone(),
		evidence:        cloneEvidence(evidence),
		instructions:    instructions,
		provenance:      provenance.clone(),
		limits:          limits,
		metadata:        cloneMetadata(metadata),
	}
	sort.Slice(request.evidence, func(left, right int) bool {
		return string(request.evidence[left].ID()) <
			string(request.evidence[right].ID())
	})
	id, err := extractionRequestID(request)
	if err != nil {
		return ExtractionRequest{}, err
	}
	request.id = id
	if err := request.Validate(); err != nil {
		return ExtractionRequest{}, err
	}
	return request, nil
}

// Validate checks request identity, schema, evidence, and wire values.
func (r ExtractionRequest) Validate() error {
	if err := validateTypedID(r.id, "extraction"); err != nil {
		return err
	}
	if r.contractVersion != ExtractionContractV1 {
		return invalid("unsupported extraction contract version")
	}
	if err := r.version.Validate(); err != nil {
		return err
	}
	if err := r.provenance.Validate(); err != nil {
		return err
	}
	if err := r.limits.Validate(); err != nil {
		return err
	}
	if err := validateBoundedVersion(r.version, r.limits); err != nil {
		return err
	}
	if extractionRequestPayloadBytes(r) > uint64(r.limits.MaxPayloadBytes) {
		return invalid("extraction request payload exceeds request limit")
	}
	if len(r.evidence) == 0 {
		return invalid("extraction request requires evidence")
	}
	if uint64(len(r.evidence)) > uint64(r.limits.MaxEvidence) {
		return invalid("extraction evidence exceeds request limit")
	}
	for index, evidence := range r.evidence {
		if err := evidence.Validate(); err != nil {
			return err
		}
		// Load-bearing: TestExtractionRequestRejectsDerivationEvidence keeps
		// extraction prompts document-cited even though assertions can be derived.
		if evidence.hasDerivation {
			return invalid("extraction request requires cited evidence")
		}
		if uint64(len(evidence.Quote())) > uint64(r.limits.MaxQuoteBytes) {
			return invalid("evidence quote exceeds request limit")
		}
		if path, present := evidence.Path(); present {
			if uint64(len(path.Nodes)) > uint64(r.limits.MaxPathNodes) ||
				uint64(len(path.Edges)) > uint64(r.limits.MaxPathEdges) {
				return invalid("evidence graph path exceeds request limit")
			}
			for _, node := range path.Nodes {
				if err := validateBoundedMetadata(node.Properties, r.limits); err != nil {
					return err
				}
			}
			for _, edge := range path.Edges {
				if err := validateBoundedMetadata(edge.Properties, r.limits); err != nil {
					return err
				}
			}
		}
		if err := validateBoundedMetadata(evidence.Metadata(), r.limits); err != nil {
			return err
		}
		if index > 0 &&
			string(r.evidence[index-1].ID()) >= string(evidence.ID()) {
			return invalid("request evidence must be unique and canonically ordered")
		}
	}
	if !requiredWire(r.instructions) {
		return invalid("extraction instructions are required")
	}
	if uint64(len(r.instructions)) > uint64(r.limits.MaxInstructionBytes) {
		return invalid("extraction instructions exceed request limit")
	}
	if err := validateMetadata(r.metadata); err != nil {
		return err
	}
	if err := validateBoundedMetadata(r.metadata, r.limits); err != nil {
		return err
	}
	if err := validateBoundedMetadata(r.provenance.Metadata(), r.limits); err != nil {
		return err
	}
	expected, err := extractionRequestID(r)
	if err != nil || expected != r.id {
		return invalid("extraction request ID is not canonical")
	}
	return nil
}

func (r ExtractionRequest) ID() shoal.ID {
	return r.id
}

func (r ExtractionRequest) ContractVersion() ExtractionContractVersion {
	return r.contractVersion
}

func (r ExtractionRequest) Version() OntologyVersion {
	return r.version.clone()
}

func (r ExtractionRequest) Evidence() []EvidenceRef {
	return cloneEvidence(r.evidence)
}

func (r ExtractionRequest) Instructions() string {
	return r.instructions
}

func (r ExtractionRequest) Provenance() ExtractionProvenance {
	return r.provenance.clone()
}

func (r ExtractionRequest) Limits() ExtractionLimits {
	return r.limits
}

func (r ExtractionRequest) Metadata() shoal.Metadata {
	return cloneMetadata(r.metadata)
}

func (r ExtractionRequest) clone() ExtractionRequest {
	r.version = r.version.clone()
	r.evidence = cloneEvidence(r.evidence)
	r.provenance = r.provenance.clone()
	r.metadata = cloneMetadata(r.metadata)
	return r
}

func extractionRequestID(request ExtractionRequest) (shoal.ID, error) {
	if err := request.version.Validate(); err != nil {
		return "", err
	}
	if request.contractVersion != ExtractionContractV1 {
		return "", invalid("unsupported extraction contract version")
	}
	if err := request.provenance.Validate(); err != nil {
		return "", err
	}
	if err := request.limits.Validate(); err != nil {
		return "", err
	}
	if !requiredWire(request.instructions) {
		return "", invalid("extraction instructions are required")
	}
	if err := validateMetadata(request.metadata); err != nil {
		return "", err
	}
	evidenceIDs := make([]string, len(request.evidence))
	for index, evidence := range request.evidence {
		if err := evidence.Validate(); err != nil {
			return "", err
		}
		if evidence.hasDerivation {
			return "", invalid("extraction request requires cited evidence")
		}
		evidenceIDs[index] = canonicalParts(
			string(evidence.ID()), canonicalMetadata(evidence.metadata))
	}
	return deriveID(
		"extraction",
		"request",
		string(request.contractVersion),
		string(request.version.ID()),
		canonicalParts(evidenceIDs...),
		request.instructions,
		request.provenance.canonical(),
		request.limits.canonical(),
		canonicalMetadata(request.metadata),
	)
}

// ExtractionResult is the immutable output of one exact request.
type ExtractionResult struct {
	id              shoal.ID
	contractVersion ExtractionContractVersion
	requestID       shoal.ID
	assertions      []Assertion
	proposals       []GovernedProposal
	provenance      ExtractionProvenance
	limits          ExtractionLimits
	completedAt     time.Time
	metadata        shoal.Metadata
}

// NewExtractionResult creates a request-bound extraction result.
func NewExtractionResult(
	request ExtractionRequest,
	assertions []Assertion,
	proposals []GovernedProposal,
	completedAt time.Time,
	metadata shoal.Metadata,
) (ExtractionResult, error) {
	if err := request.Validate(); err != nil {
		return ExtractionResult{}, err
	}
	if err := preflightExtractionResult(request, assertions, proposals, metadata); err != nil {
		return ExtractionResult{}, err
	}
	result := ExtractionResult{
		contractVersion: request.ContractVersion(),
		requestID:       request.ID(),
		assertions:      cloneAssertions(assertions),
		proposals:       cloneProposals(proposals),
		provenance:      request.Provenance(),
		limits:          request.Limits(),
		completedAt:     normalizeTime(completedAt),
		metadata:        cloneMetadata(metadata),
	}
	sort.Slice(result.assertions, func(left, right int) bool {
		return string(result.assertions[left].ID()) <
			string(result.assertions[right].ID())
	})
	sort.Slice(result.proposals, func(left, right int) bool {
		return string(result.proposals[left].ID()) <
			string(result.proposals[right].ID())
	})
	id, err := extractionResultID(result)
	if err != nil {
		return ExtractionResult{}, err
	}
	result.id = id
	if err := result.ValidateFor(request); err != nil {
		return ExtractionResult{}, err
	}
	return result, nil
}

// Validate checks result-local invariants. ValidateFor additionally verifies
// request evidence and schema membership.
func (r ExtractionResult) Validate() error {
	if err := validateTypedID(r.id, "extraction"); err != nil {
		return err
	}
	if err := validateTypedID(r.requestID, "extraction"); err != nil {
		return err
	}
	if r.contractVersion != ExtractionContractV1 {
		return invalid("unsupported extraction contract version")
	}
	if err := r.provenance.Validate(); err != nil {
		return err
	}
	if err := r.limits.Validate(); err != nil {
		return err
	}
	if uint64(len(r.assertions)) > uint64(r.limits.MaxAssertions) {
		return invalid("extraction assertions exceed result limit")
	}
	if uint64(len(r.proposals)) > uint64(r.limits.MaxProposals) {
		return invalid("extraction proposals exceed result limit")
	}
	if err := validateTime(r.completedAt, "extraction completion time"); err != nil {
		return err
	}
	if err := validateMetadata(r.metadata); err != nil {
		return err
	}
	if err := validateBoundedMetadata(r.metadata, r.limits); err != nil {
		return err
	}
	if err := validateBoundedMetadata(r.provenance.Metadata(), r.limits); err != nil {
		return err
	}
	if extractionResultPayloadBytes(r) > uint64(r.limits.MaxPayloadBytes) {
		return invalid("extraction result payload exceeds result limit")
	}
	for index, assertion := range r.assertions {
		if err := assertion.Validate(); err != nil {
			return err
		}
		if assertion.Provenance().canonical() != r.provenance.canonical() {
			return invalid("assertion provenance does not match extraction result")
		}
		if uint64(len(assertion.Evidence())) > uint64(r.limits.MaxEvidence) {
			return invalid("assertion evidence exceeds result limit")
		}
		for _, evidence := range assertion.Evidence() {
			if err := validateBoundedEvidenceMetadata(evidence, r.limits); err != nil {
				return err
			}
		}
		if err := validateBoundedMetadata(assertion.Metadata(), r.limits); err != nil {
			return err
		}
		if assertionPayloadBytes(assertion) > uint64(r.limits.MaxPayloadBytes) {
			return invalid("assertion payload exceeds result limit")
		}
		if index > 0 &&
			string(r.assertions[index-1].ID()) >= string(assertion.ID()) {
			return invalid("result assertions must be unique and canonically ordered")
		}
	}
	for index, proposal := range r.proposals {
		if err := proposal.Validate(); err != nil {
			return err
		}
		if proposal.State() != ProposalDraft {
			return invalid("extracted ontology proposals must be drafts")
		}
		if err := validateBoundedVersion(proposal.ProposedVersion(), r.limits); err != nil {
			return err
		}
		if err := validateBoundedMetadata(proposal.schema.metadata, r.limits); err != nil {
			return err
		}
		if err := validateBoundedMetadata(proposal.Metadata(), r.limits); err != nil {
			return err
		}
		for _, morphism := range proposal.Morphisms() {
			if uint64(len(morphism.Evidence())) > uint64(r.limits.MaxEvidence) {
				return invalid("morphism evidence exceeds result limit")
			}
			if err := validateBoundedMetadata(morphism.Metadata(), r.limits); err != nil {
				return err
			}
			for _, evidence := range morphism.Evidence() {
				if err := validateBoundedEvidenceMetadata(evidence, r.limits); err != nil {
					return err
				}
			}
		}
		if proposalPayloadBytes(proposal) > uint64(r.limits.MaxPayloadBytes) {
			return invalid("proposal payload exceeds result limit")
		}
		if index > 0 &&
			string(r.proposals[index-1].ID()) >= string(proposal.ID()) {
			return invalid("result proposals must be unique and canonically ordered")
		}
	}
	expected, err := extractionResultID(r)
	if err != nil || expected != r.id {
		return invalid("extraction result ID is not canonical")
	}
	return nil
}

// ValidateFor verifies that the result only cites request evidence and only
// uses definitions from the request's immutable ontology version.
func (r ExtractionResult) ValidateFor(request ExtractionRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if err := r.Validate(); err != nil {
		return err
	}
	if r.requestID != request.ID() {
		return invalid("extraction result does not match request")
	}
	if r.contractVersion != request.ContractVersion() ||
		r.limits != request.Limits() ||
		r.provenance.canonical() != request.Provenance().canonical() {
		return invalid("extraction result envelope does not match request")
	}
	requestEvidence := make(map[shoal.ID]string, len(request.evidence))
	for _, evidence := range request.evidence {
		requestEvidence[evidence.ID()] = canonicalMetadata(evidence.metadata)
	}
	properties := make(map[shoal.ID]PropertyDefinition, len(request.version.properties))
	for _, property := range request.version.properties {
		properties[property.ID()] = property
	}
	concepts := make(map[shoal.ID]ConceptDefinition, len(request.version.concepts))
	for _, concept := range request.version.concepts {
		concepts[concept.ID()] = concept
	}
	relationships := make(
		map[shoal.ID]RelationshipDefinition, len(request.version.relationships))
	for _, relationship := range request.version.relationships {
		relationships[relationship.ID()] = relationship
	}
	propertyOwners := make(map[shoal.ID]map[shoal.ID]struct{})
	propertyPatterns := make(map[shoal.ID]*regexp.Regexp)
	for _, concept := range request.version.concepts {
		for _, property := range concept.properties {
			if propertyOwners[property] == nil {
				propertyOwners[property] = make(map[shoal.ID]struct{})
			}
			propertyOwners[property][concept.ID()] = struct{}{}
		}
	}
	for _, relationship := range request.version.relationships {
		for _, property := range relationship.properties {
			if propertyOwners[property] == nil {
				propertyOwners[property] = make(map[shoal.ID]struct{})
			}
			propertyOwners[property][relationship.ID()] = struct{}{}
		}
	}
	for _, property := range request.version.properties {
		for _, constraint := range property.constraints {
			if constraint.Kind() == ConstraintPattern {
				pattern, _ := constraint.Pattern()
				propertyPatterns[property.ID()] = regexp.MustCompile(pattern)
			}
		}
	}
	type assertionGroup struct {
		subject   shoal.ID
		predicate shoal.ID
	}
	relationshipInstances := make(map[shoal.ID]shoal.ID)
	for _, assertion := range r.assertions {
		if IDNamespace(assertion.Predicate()) == "relationship" {
			relationshipInstances[assertion.ID()] = assertion.Predicate()
		}
	}
	counts := make(map[assertionGroup]uint32)
	subjects := make(map[shoal.ID]struct{})
	subjectTypes := make(map[shoal.ID]shoal.ID)
	subjectTypeCounts := make(map[shoal.ID]uint32)
	uniqueValues := make(map[string]struct{})
	requestOntology := OntologyIdentity{
		schemaID:  request.version.Schema().ID(),
		versionID: request.version.ID(),
	}
	for _, assertion := range r.assertions {
		switch assertion.ReadUnder(requestOntology) {
		case OntologySameVersion:
		case OntologyUnresolved:
			// The assertion recorded no ontology identity. That is an explicit
			// unknown, reported by UnresolvedOntologyAssertions, and it is not
			// filled in from the request here: stamping the pinned version onto
			// a value that never declared it would manufacture the evidence
			// this field exists to carry.
		default:
			return invalid(
				"assertion was made under a different ontology version than the request")
		}
		subjects[assertion.Subject()] = struct{}{}
		subjectType, present := assertion.SubjectType()
		if !present {
			return invalid("assertion subject type is required")
		}
		if existing, exists := subjectTypes[assertion.Subject()]; exists {
			if existing != subjectType {
				return invalid("assertion subject has inconsistent ontology types")
			}
		} else {
			subjectTypes[assertion.Subject()] = subjectType
			subjectTypeCounts[subjectType]++
		}
		for _, evidence := range assertion.Evidence() {
			metadata, exists := requestEvidence[evidence.ID()]
			if !exists || metadata != canonicalMetadata(evidence.metadata) {
				return invalid("assertion cites evidence outside the extraction request")
			}
		}
		switch IDNamespace(assertion.Predicate()) {
		case "property":
			property, exists := properties[assertion.Predicate()]
			if !exists {
				return invalid("assertion references an unknown property")
			}
			if err := validatePropertyValue(
				property, assertion.Object(), propertyPatterns[property.ID()],
			); err != nil {
				return err
			}
			if IDNamespace(subjectType) == "concept" {
				if _, exists := concepts[subjectType]; !exists {
					return invalid("assertion subject concept is not in the ontology")
				}
			} else if IDNamespace(subjectType) == "relationship" {
				if _, exists := relationships[subjectType]; !exists {
					return invalid("assertion subject relationship is not in the ontology")
				}
				if instanceType, exists := relationshipInstances[assertion.Subject()]; !exists || instanceType != subjectType {
					return invalid(
						"relationship property must target an asserted relationship instance")
				}
			} else {
				return invalid("assertion subject type is not in the ontology")
			}
			if owners := propertyOwners[assertion.Predicate()]; owners != nil {
				if _, allowed := owners[subjectType]; !allowed {
					return invalid("property does not apply to assertion subject type")
				}
			}
			counts[assertionGroup{
				subject: assertion.Subject(), predicate: assertion.Predicate(),
			}]++
			for _, constraint := range property.constraints {
				if constraint.Kind() != ConstraintUnique {
					continue
				}
				key := canonicalParts(
					string(assertion.Predicate()),
					propertyValueKey(property, assertion.Object()),
				)
				if _, duplicate := uniqueValues[key]; duplicate {
					return invalid("assertion value violates property uniqueness")
				}
				uniqueValues[key] = struct{}{}
			}
		case "relationship":
			relationship, exists := relationships[assertion.Predicate()]
			if !exists {
				return invalid("assertion references an unknown relationship")
			}
			if assertion.Object().Type() != ValueReference {
				return invalid("relationship assertion requires a reference object")
			}
			objectType, present := assertion.ObjectType()
			if IDNamespace(subjectType) != "concept" || !present {
				return invalid("relationship assertion requires concept endpoint types")
			}
			forward := containsID(relationship.fromConcepts, subjectType) &&
				containsID(relationship.toConcepts, objectType)
			reverse := !relationship.directed &&
				containsID(relationship.toConcepts, subjectType) &&
				containsID(relationship.fromConcepts, objectType)
			if !forward && !reverse {
				return invalid("relationship does not allow the assertion endpoint concepts")
			}
			instanceID := assertion.ID()
			subjects[instanceID] = struct{}{}
			if existing, exists := subjectTypes[instanceID]; exists {
				if existing != assertion.Predicate() {
					return invalid("relationship instance has inconsistent ontology type")
				}
			} else {
				subjectTypes[instanceID] = assertion.Predicate()
				subjectTypeCounts[assertion.Predicate()]++
			}
			objectID, _ := assertion.Object().ReferenceValue()
			subjects[objectID] = struct{}{}
			if existing, exists := subjectTypes[objectID]; exists {
				if existing != objectType {
					return invalid("referenced entity has inconsistent ontology types")
				}
			} else {
				subjectTypes[objectID] = objectType
				subjectTypeCounts[objectType]++
			}
		default:
			return invalid("assertion predicate is not in the ontology")
		}
	}
	predicateSubjects := make(map[shoal.ID]uint32)
	predicateMinimums := make(map[shoal.ID]uint32)
	for group, count := range counts {
		property := properties[group.predicate]
		predicateSubjects[group.predicate]++
		minimum, exists := predicateMinimums[group.predicate]
		if !exists || count < minimum {
			predicateMinimums[group.predicate] = count
		}
		for _, constraint := range property.constraints {
			switch constraint.Kind() {
			case ConstraintMaximumCount:
				maximum, _ := constraint.Count()
				if count > maximum {
					return invalid("assertion count exceeds property maximum")
				}
			}
		}
	}
	for predicate, property := range properties {
		targetSubjects := uint32(len(subjects))
		if owners := propertyOwners[predicate]; owners != nil {
			targetSubjects = 0
			for owner := range owners {
				targetSubjects += subjectTypeCounts[owner]
			}
		}
		for _, constraint := range property.constraints {
			switch constraint.Kind() {
			case ConstraintRequired:
				if targetSubjects != 0 &&
					predicateSubjects[predicate] != targetSubjects {
					return invalid("required property assertion is missing")
				}
			case ConstraintMinimumCount:
				minimum, _ := constraint.Count()
				if targetSubjects != 0 &&
					(predicateSubjects[predicate] != targetSubjects ||
						predicateMinimums[predicate] < minimum) {
					return invalid("assertion count is below property minimum")
				}
			}
		}
	}
	for _, proposal := range r.proposals {
		if proposal.Schema().ID() != request.version.Schema().ID() {
			return invalid("extracted proposal belongs to a different schema")
		}
		baseVersionID, present := proposal.BaseVersionID()
		if !present || baseVersionID != request.version.ID() {
			return invalid("extracted proposal does not use the request ontology version")
		}
	}
	return nil
}

func validatePropertyValue(
	property PropertyDefinition, value Value, pattern *regexp.Regexp,
) error {
	if !valueMatchesType(value, property.ValueType()) {
		return invalid("assertion value does not match property type")
	}
	if !valueSatisfiesConstraints(value, property.constraints, true, pattern) {
		return invalid("assertion value violates property constraints")
	}
	return nil
}

func containsID(values []shoal.ID, target shoal.ID) bool {
	index := sort.Search(len(values), func(index int) bool {
		return string(values[index]) >= string(target)
	})
	return index < len(values) && values[index] == target
}

func propertyValueKey(property PropertyDefinition, value Value) string {
	if property.ValueType() == ValueNumber {
		return canonicalParts(string(ValueNumber), numericRat(value).RatString())
	}
	return value.canonical()
}

func (r ExtractionResult) ID() shoal.ID {
	return r.id
}

func (r ExtractionResult) ContractVersion() ExtractionContractVersion {
	return r.contractVersion
}

func (r ExtractionResult) RequestID() shoal.ID {
	return r.requestID
}

func (r ExtractionResult) Assertions() []Assertion {
	return cloneAssertions(r.assertions)
}

// UnresolvedOntologyAssertions returns, in canonical order, the IDs of
// assertions that record no ontology identity. ValidateFor permits them
// because the request pins a version the caller can still recover, but it does
// not stamp that version onto them, so an unresolved assertion is marked and
// reported rather than quietly adopted.
func (r ExtractionResult) UnresolvedOntologyAssertions() []shoal.ID {
	var unresolved []shoal.ID
	for _, assertion := range r.assertions {
		if _, known := assertion.Ontology(); !known {
			unresolved = append(unresolved, assertion.ID())
		}
	}
	return unresolved
}

func (r ExtractionResult) Proposals() []GovernedProposal {
	return cloneProposals(r.proposals)
}

func (r ExtractionResult) Provenance() ExtractionProvenance {
	return r.provenance.clone()
}

func (r ExtractionResult) Limits() ExtractionLimits {
	return r.limits
}

func (r ExtractionResult) CompletedAt() time.Time {
	return r.completedAt
}

func (r ExtractionResult) Metadata() shoal.Metadata {
	return cloneMetadata(r.metadata)
}

func extractionResultID(result ExtractionResult) (shoal.ID, error) {
	if err := validateTypedID(result.requestID, "extraction"); err != nil {
		return "", err
	}
	if result.contractVersion != ExtractionContractV1 {
		return "", invalid("unsupported extraction contract version")
	}
	if err := result.provenance.Validate(); err != nil {
		return "", err
	}
	if err := result.limits.Validate(); err != nil {
		return "", err
	}
	if err := validateTime(result.completedAt, "extraction completion time"); err != nil {
		return "", err
	}
	if err := validateMetadata(result.metadata); err != nil {
		return "", err
	}
	assertionIDs := make([]string, len(result.assertions))
	for index, assertion := range result.assertions {
		if err := assertion.Validate(); err != nil {
			return "", err
		}
		assertionIDs[index] = canonicalParts(
			string(assertion.ID()), canonicalMetadata(assertion.metadata))
	}
	proposalIDs := make([]string, len(result.proposals))
	for index, proposal := range result.proposals {
		if err := proposal.Validate(); err != nil {
			return "", err
		}
		proposalIDs[index] = canonicalParts(
			string(proposal.ID()), proposal.schema.canonical(),
			canonicalMetadata(proposal.metadata),
		)
	}
	return deriveID(
		"extraction",
		"result",
		string(result.contractVersion),
		string(result.requestID),
		canonicalParts(assertionIDs...),
		canonicalParts(proposalIDs...),
		result.provenance.canonical(),
		result.limits.canonical(),
		canonicalTime(result.completedAt),
		canonicalMetadata(result.metadata),
	)
}

func validateBoundedVersion(version OntologyVersion, limits ExtractionLimits) error {
	memberCount := uint64(len(version.Concepts())) +
		uint64(len(version.Relationships())) +
		uint64(len(version.Properties()))
	if memberCount > uint64(limits.MaxSchemaMembers) {
		return invalid("ontology schema members exceed extraction limit")
	}
	if err := validateBoundedMetadata(version.Schema().Metadata(), limits); err != nil {
		return err
	}
	if err := validateBoundedMetadata(version.Metadata(), limits); err != nil {
		return err
	}
	for _, concept := range version.Concepts() {
		if err := validateBoundedMetadata(concept.Metadata(), limits); err != nil {
			return err
		}
	}
	for _, relationship := range version.Relationships() {
		if err := validateBoundedMetadata(relationship.Metadata(), limits); err != nil {
			return err
		}
	}
	for _, property := range version.Properties() {
		if err := validateBoundedMetadata(property.Metadata(), limits); err != nil {
			return err
		}
	}
	return nil
}

func extractionRequestPayloadBytes(request ExtractionRequest) uint64 {
	size := ontologyVersionPayloadBytes(request.version) +
		uint64(len(request.instructions)) +
		provenancePayloadBytes(request.provenance) +
		uint64(len(canonicalMetadata(request.metadata)))
	for _, evidence := range request.evidence {
		size += evidencePayloadBytes(evidence)
	}
	return size
}

func extractionResultPayloadBytes(result ExtractionResult) uint64 {
	size := provenancePayloadBytes(result.provenance) +
		uint64(len(canonicalMetadata(result.metadata)))
	for _, assertion := range result.assertions {
		size += assertionPayloadBytes(assertion)
	}
	for _, proposal := range result.proposals {
		size += proposalPayloadBytes(proposal)
	}
	return size
}

func ontologyVersionPayloadBytes(version OntologyVersion) uint64 {
	size := uint64(len(version.schema.key) + len(version.schema.name) +
		len(version.schema.description) + len(version.version) +
		len(canonicalMetadata(version.schema.metadata)) +
		len(canonicalMetadata(version.metadata)))
	for _, concept := range version.concepts {
		size += uint64(len(concept.canonical()))
	}
	for _, relationship := range version.relationships {
		size += uint64(len(relationship.canonical()))
	}
	for _, property := range version.properties {
		size += uint64(len(property.canonical()))
	}
	return size
}

func evidencePayloadBytes(evidence EvidenceRef) uint64 {
	if evidence.hasDerivation {
		return uint64(len(evidence.derivation.canonical()) +
			len(canonicalMetadata(evidence.metadata)))
	}
	return uint64(len(canonicalCitation(evidence.citation)) +
		len(evidence.quote) + len(canonicalGraphPath(evidence.path)) +
		len(canonicalMetadata(evidence.metadata)))
}

func provenancePayloadBytes(provenance ExtractionProvenance) uint64 {
	return uint64(len(provenance.canonical()))
}

func assertionPayloadBytes(assertion Assertion) uint64 {
	size := uint64(len(assertion.subject) + len(assertion.predicate) +
		len(assertion.subjectType) + len(assertion.objectType) +
		len(assertion.object.canonical()) + len(assertion.provenance.canonical()) +
		len(assertion.ontologyIdentity.schemaID) +
		len(assertion.ontologyIdentity.versionID) +
		len(canonicalMetadata(assertion.metadata)))
	for _, evidence := range assertion.evidence {
		size += evidencePayloadBytes(evidence)
	}
	return size
}

func preflightExtractionRequest(
	version OntologyVersion,
	evidence []EvidenceRef,
	instructions string,
	provenance ExtractionProvenance,
	limits ExtractionLimits,
	metadata shoal.Metadata,
) error {
	if err := limits.Validate(); err != nil {
		return err
	}
	memberCount := uint64(len(version.concepts)) +
		uint64(len(version.relationships)) + uint64(len(version.properties))
	if memberCount > uint64(limits.MaxSchemaMembers) {
		return invalid("ontology schema members exceed extraction limit")
	}
	if len(evidence) == 0 || uint64(len(evidence)) > uint64(limits.MaxEvidence) {
		return invalid("extraction evidence exceeds request limit")
	}
	if uint64(len(instructions)) > uint64(limits.MaxInstructionBytes) {
		return invalid("extraction instructions exceed request limit")
	}
	if err := validateBoundedMetadata(metadata, limits); err != nil {
		return err
	}
	if err := validateBoundedMetadata(provenance.metadata, limits); err != nil {
		return err
	}
	counter := payloadCounter{limit: uint64(limits.MaxPayloadBytes)}
	counter.addVersion(version)
	counter.addString(instructions)
	counter.addProvenance(provenance)
	counter.addMetadata(metadata)
	for _, item := range evidence {
		// Load-bearing: TestExtractionRequestRejectsDerivationEvidence keeps
		// extraction prompts document-cited even though assertions can be derived.
		if item.hasDerivation {
			return invalid("extraction request requires cited evidence")
		}
		if uint64(len(item.quote)) > uint64(limits.MaxQuoteBytes) {
			return invalid("evidence quote exceeds request limit")
		}
		if uint64(len(item.path.Nodes)) > uint64(limits.MaxPathNodes) ||
			uint64(len(item.path.Edges)) > uint64(limits.MaxPathEdges) {
			return invalid("evidence graph path exceeds request limit")
		}
		if err := validateBoundedEvidenceMetadata(item, limits); err != nil {
			return err
		}
		counter.addEvidence(item)
	}
	if counter.exceeded {
		return invalid("extraction request payload exceeds request limit")
	}
	return nil
}

func preflightExtractionResult(
	request ExtractionRequest,
	assertions []Assertion,
	proposals []GovernedProposal,
	metadata shoal.Metadata,
) error {
	limits := request.limits
	if uint64(len(assertions)) > uint64(limits.MaxAssertions) {
		return invalid("extraction assertions exceed result limit")
	}
	if uint64(len(proposals)) > uint64(limits.MaxProposals) {
		return invalid("extraction proposals exceed result limit")
	}
	if err := validateBoundedMetadata(metadata, limits); err != nil {
		return err
	}
	counter := payloadCounter{limit: uint64(limits.MaxPayloadBytes)}
	counter.addProvenance(request.provenance)
	counter.addMetadata(metadata)
	for _, assertion := range assertions {
		counter.addAssertion(assertion)
	}
	for _, proposal := range proposals {
		if err := validateBoundedMetadata(proposal.schema.metadata, limits); err != nil {
			return err
		}
		counter.addProposal(proposal)
	}
	if counter.exceeded {
		return invalid("extraction result payload exceeds result limit")
	}
	return nil
}

type payloadCounter struct {
	size     uint64
	limit    uint64
	exceeded bool
}

func (c *payloadCounter) addLength(length int) {
	if c.exceeded {
		return
	}
	value := uint64(length)
	if value > c.limit-c.size {
		c.exceeded = true
		return
	}
	c.size += value
}

func (c *payloadCounter) addString(value string) {
	c.addLength(len(value))
}

func (c *payloadCounter) addMetadata(metadata shoal.Metadata) {
	for key, value := range metadata {
		c.addString(key)
		c.addString(value)
	}
}

func (c *payloadCounter) addValue(value Value) {
	switch value.Type() {
	case ValueString:
		text, _ := value.StringValue()
		c.addString(text)
	case ValueReference:
		reference, _ := value.ReferenceValue()
		c.addString(string(reference))
	}
}

func (c *payloadCounter) addVersion(version OntologyVersion) {
	c.addString(version.schema.key)
	c.addString(version.schema.name)
	c.addString(version.schema.description)
	c.addMetadata(version.schema.metadata)
	c.addString(version.version)
	c.addMetadata(version.metadata)
	for _, concept := range version.concepts {
		c.addString(string(concept.id))
		c.addString(concept.key)
		c.addString(concept.name)
		c.addString(concept.description)
		for _, property := range concept.properties {
			c.addString(string(property))
		}
		c.addMetadata(concept.metadata)
	}
	for _, relationship := range version.relationships {
		c.addString(string(relationship.id))
		c.addString(relationship.key)
		c.addString(relationship.name)
		c.addString(relationship.description)
		for _, concept := range relationship.fromConcepts {
			c.addString(string(concept))
		}
		for _, concept := range relationship.toConcepts {
			c.addString(string(concept))
		}
		for _, property := range relationship.properties {
			c.addString(string(property))
		}
		c.addMetadata(relationship.metadata)
	}
	for _, property := range version.properties {
		c.addString(string(property.id))
		c.addString(property.key)
		c.addString(property.name)
		c.addString(property.description)
		for _, constraint := range property.constraints {
			c.addString(constraint.pattern)
			c.addValue(constraint.value)
			for _, allowed := range constraint.allowed {
				c.addValue(allowed)
			}
		}
		c.addMetadata(property.metadata)
	}
}

func (c *payloadCounter) addEvidence(evidence EvidenceRef) {
	if evidence.hasDerivation {
		c.addDerivation(evidence.derivation)
		c.addMetadata(evidence.metadata)
		return
	}
	c.addString(string(evidence.citation.DocumentID))
	c.addString(string(evidence.citation.RevisionID))
	c.addString(string(evidence.citation.SectionID))
	c.addString(string(evidence.citation.SpanID))
	c.addString(evidence.quote)
	c.addMetadata(evidence.metadata)
	for _, node := range evidence.path.Nodes {
		c.addString(string(node.ID))
		c.addString(node.Kind)
		for _, label := range node.Labels {
			c.addString(label)
		}
		c.addMetadata(node.Properties)
	}
	for _, edge := range evidence.path.Edges {
		c.addString(string(edge.ID))
		c.addString(string(edge.From))
		c.addString(string(edge.To))
		c.addString(edge.Type)
		c.addMetadata(edge.Properties)
	}
}

func (c *payloadCounter) addDerivation(derivation AssertionDerivation) {
	c.addString(derivation.embeddingModel)
	c.addString(derivation.embeddingModelVersion)
	c.addString(derivation.similarityMetric)
	c.addString(canonicalFloat(float64(derivation.threshold)))
	c.addString(derivation.tessellationCell)
	c.addString(canonicalFloat(float64(derivation.score)))
	c.addString(string(derivation.sourceEndpoint))
	c.addString(string(derivation.targetEndpoint))
	c.addString(derivation.iteratorName)
	c.addMetadata(derivation.iteratorOptions)
}

func (c *payloadCounter) addProvenance(provenance ExtractionProvenance) {
	c.addString(provenance.provider)
	c.addString(provenance.model)
	c.addString(provenance.modelVersion)
	c.addString(provenance.prompt)
	c.addString(provenance.promptVersion)
	c.addString(provenance.extractor)
	c.addString(provenance.extractorVersion)
	c.addMetadata(provenance.metadata)
}

func (c *payloadCounter) addAssertion(assertion Assertion) {
	c.addString(string(assertion.subject))
	c.addString(string(assertion.subjectType))
	c.addString(string(assertion.predicate))
	c.addString(string(assertion.objectType))
	c.addValue(assertion.object)
	c.addProvenance(assertion.provenance)
	c.addString(string(assertion.ontologyIdentity.schemaID))
	c.addString(string(assertion.ontologyIdentity.versionID))
	c.addMetadata(assertion.metadata)
	for _, evidence := range assertion.evidence {
		c.addEvidence(evidence)
	}
}

func (c *payloadCounter) addProposal(proposal GovernedProposal) {
	c.addString(proposal.schema.key)
	c.addString(proposal.schema.name)
	c.addString(proposal.schema.description)
	c.addMetadata(proposal.schema.metadata)
	c.addString(string(proposal.baseSchemaID))
	c.addString(string(proposal.baseVersionID))
	c.addVersion(proposal.proposedVersion)
	c.addString(proposal.proposedBy)
	c.addString(proposal.rationale)
	c.addMetadata(proposal.metadata)
	for _, transition := range proposal.transitions {
		c.addString(transition.actor)
		c.addString(transition.note)
	}
	for _, morphism := range proposal.morphisms {
		c.addString(string(morphism.kind))
		c.addString(string(morphism.safety))
		c.addString(string(morphism.source.schemaID))
		c.addString(string(morphism.source.versionID))
		c.addString(string(morphism.target.schemaID))
		c.addString(string(morphism.target.versionID))
		for _, id := range morphism.sources {
			c.addString(string(id))
		}
		for _, id := range morphism.targets {
			c.addString(string(id))
		}
		c.addString(morphism.discriminator.metadataKey)
		for _, choice := range morphism.discriminator.choices {
			c.addString(choice.value)
			c.addString(string(choice.target))
		}
		for _, evidence := range morphism.evidence {
			c.addEvidence(evidence)
		}
		c.addString(morphism.rationale)
		c.addMetadata(morphism.metadata)
	}
}

func proposalPayloadBytes(proposal GovernedProposal) uint64 {
	size := uint64(len(proposal.schema.key)+len(proposal.schema.name)+
		len(proposal.schema.description)+len(proposal.baseSchemaID)+
		len(proposal.baseVersionID)+
		len(proposal.proposedBy)+len(proposal.rationale)+
		len(canonicalMetadata(proposal.schema.metadata))+
		len(canonicalMetadata(proposal.metadata))) +
		ontologyVersionPayloadBytes(proposal.proposedVersion)
	for _, transition := range proposal.transitions {
		size += uint64(len(transition.actor) + len(transition.note))
	}
	for _, morphism := range proposal.morphisms {
		counter := payloadCounter{limit: ^uint64(0)}
		counter.addString(string(morphism.kind))
		counter.addString(string(morphism.safety))
		counter.addString(string(morphism.source.schemaID))
		counter.addString(string(morphism.source.versionID))
		counter.addString(string(morphism.target.schemaID))
		counter.addString(string(morphism.target.versionID))
		for _, id := range morphism.sources {
			counter.addString(string(id))
		}
		for _, id := range morphism.targets {
			counter.addString(string(id))
		}
		counter.addString(morphism.discriminator.metadataKey)
		for _, choice := range morphism.discriminator.choices {
			counter.addString(choice.value)
			counter.addString(string(choice.target))
		}
		for _, evidence := range morphism.evidence {
			counter.addEvidence(evidence)
		}
		counter.addString(morphism.rationale)
		counter.addMetadata(morphism.metadata)
		if counter.exceeded || counter.size > ^uint64(0)-size {
			return ^uint64(0)
		}
		size += counter.size
	}
	return size
}

func validateBoundedMetadata(metadata shoal.Metadata, limits ExtractionLimits) error {
	if uint64(len(metadata)) > uint64(limits.MaxMetadataEntries) {
		return invalid("metadata entries exceed extraction limit")
	}
	var size uint64
	for key, value := range metadata {
		size += uint64(len(key)) + uint64(len(value))
		if size > uint64(limits.MaxMetadataBytes) {
			return invalid("metadata bytes exceed extraction limit")
		}
	}
	return nil
}

func validateBoundedEvidenceMetadata(
	evidence EvidenceRef, limits ExtractionLimits,
) error {
	if err := validateBoundedMetadata(evidence.metadata, limits); err != nil {
		return err
	}
	if evidence.hasDerivation {
		return validateBoundedMetadata(evidence.derivation.iteratorOptions, limits)
	}
	if !evidence.hasPath {
		return nil
	}
	for _, node := range evidence.path.Nodes {
		if err := validateBoundedMetadata(node.Properties, limits); err != nil {
			return err
		}
	}
	for _, edge := range evidence.path.Edges {
		if err := validateBoundedMetadata(edge.Properties, limits); err != nil {
			return err
		}
	}
	return nil
}

func cloneAssertions(values []Assertion) []Assertion {
	cloned := make([]Assertion, len(values))
	for index, value := range values {
		cloned[index] = value.clone()
	}
	return cloned
}

func cloneProposals(values []GovernedProposal) []GovernedProposal {
	cloned := make([]GovernedProposal, len(values))
	for index, value := range values {
		cloned[index] = value.clone()
	}
	return cloned
}

// Extractor is implemented by provider adapters without exposing provider SDK
// values. Implementations must not retain or mutate requests and must return
// results that pass ValidateFor. Compiling or persisting a validated result is
// a separate operation outside this interface.
type Extractor interface {
	Extract(context.Context, ExtractionRequest) (ExtractionResult, error)
}
