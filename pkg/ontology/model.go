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

// Package ontology defines transport-neutral ontology and extraction
// contracts.
package ontology

import (
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// ValueType identifies the wire-safe type of an assertion or property value.
type ValueType string

const (
	ValueString    ValueType = "string"
	ValueInteger   ValueType = "integer"
	ValueNumber    ValueType = "number"
	ValueBoolean   ValueType = "boolean"
	ValueTimestamp ValueType = "timestamp"
	ValueReference ValueType = "reference"
)

// Value is an immutable typed ontology value.
type Value struct {
	valueType ValueType
	text      string
	integer   int64
	number    float64
	boolean   bool
	timestamp time.Time
	reference shoal.ID
}

// NewStringValue creates a string value.
func NewStringValue(value string) (Value, error) {
	result := Value{valueType: ValueString, text: value}
	if err := result.Validate(); err != nil {
		return Value{}, err
	}
	return result, nil
}

// NewIntegerValue creates an integer value.
func NewIntegerValue(value int64) Value {
	return Value{valueType: ValueInteger, integer: value}
}

// NewNumberValue creates a finite floating-point value.
func NewNumberValue(value float64) (Value, error) {
	result := Value{valueType: ValueNumber, number: value}
	if err := result.Validate(); err != nil {
		return Value{}, err
	}
	return result, nil
}

// NewBooleanValue creates a boolean value.
func NewBooleanValue(value bool) Value {
	return Value{valueType: ValueBoolean, boolean: value}
}

// NewTimestampValue creates a canonical UTC timestamp value.
func NewTimestampValue(value time.Time) (Value, error) {
	result := Value{valueType: ValueTimestamp, timestamp: normalizeTime(value)}
	if err := result.Validate(); err != nil {
		return Value{}, err
	}
	return result, nil
}

// NewReferenceValue creates a reference to a caller-visible entity.
func NewReferenceValue(value shoal.ID) (Value, error) {
	result := Value{valueType: ValueReference, reference: value}
	if err := result.Validate(); err != nil {
		return Value{}, err
	}
	return result, nil
}

// Validate checks that the active value is safe for transport.
func (v Value) Validate() error {
	switch v.valueType {
	case ValueString:
		if !validOpaqueWire(v.text) {
			return invalid("string value must be valid UTF-8")
		}
	case ValueInteger:
	case ValueNumber:
		return validateFinite(v.number, "number value")
	case ValueBoolean:
	case ValueTimestamp:
		return validateTime(v.timestamp, "timestamp value")
	case ValueReference:
		return validateReference(v.reference, "reference value")
	default:
		return invalid("invalid ontology value type")
	}
	return nil
}

func (v Value) Type() ValueType {
	return v.valueType
}

func (v Value) StringValue() (string, bool) {
	return v.text, v.valueType == ValueString
}

func (v Value) IntegerValue() (int64, bool) {
	return v.integer, v.valueType == ValueInteger
}

func (v Value) NumberValue() (float64, bool) {
	return v.number, v.valueType == ValueNumber
}

func (v Value) BooleanValue() (bool, bool) {
	return v.boolean, v.valueType == ValueBoolean
}

func (v Value) TimestampValue() (time.Time, bool) {
	return v.timestamp, v.valueType == ValueTimestamp
}

func (v Value) ReferenceValue() (shoal.ID, bool) {
	return v.reference, v.valueType == ValueReference
}

func (v Value) canonical() string {
	switch v.valueType {
	case ValueString:
		return canonicalParts(string(v.valueType), v.text)
	case ValueInteger:
		return canonicalParts(string(v.valueType), strconv.FormatInt(v.integer, 10))
	case ValueNumber:
		return canonicalParts(string(v.valueType), canonicalFloat(v.number))
	case ValueBoolean:
		return canonicalParts(string(v.valueType), strconv.FormatBool(v.boolean))
	case ValueTimestamp:
		return canonicalParts(string(v.valueType), canonicalTime(v.timestamp))
	case ValueReference:
		return canonicalParts(string(v.valueType), string(v.reference))
	default:
		return ""
	}
}

// ConstraintKind identifies a supported property constraint.
type ConstraintKind string

const (
	ConstraintRequired      ConstraintKind = "required"
	ConstraintUnique        ConstraintKind = "unique"
	ConstraintMinimumCount  ConstraintKind = "minimum_count"
	ConstraintMaximumCount  ConstraintKind = "maximum_count"
	ConstraintMinimumValue  ConstraintKind = "minimum_value"
	ConstraintMaximumValue  ConstraintKind = "maximum_value"
	ConstraintPattern       ConstraintKind = "pattern"
	ConstraintAllowedValues ConstraintKind = "allowed_values"
)

// Constraint is an immutable property constraint. Construct it with the
// kind-specific constructors.
type Constraint struct {
	kind       ConstraintKind
	count      uint32
	value      Value
	pattern    string
	allowed    []Value
	hasCount   bool
	hasValue   bool
	hasPattern bool
}

// NewFlagConstraint creates a required or unique constraint.
func NewFlagConstraint(kind ConstraintKind) (Constraint, error) {
	constraint := Constraint{kind: kind}
	if err := constraint.Validate(); err != nil {
		return Constraint{}, err
	}
	return constraint, nil
}

// NewCountConstraint creates a minimum-count or maximum-count constraint.
func NewCountConstraint(kind ConstraintKind, count uint32) (Constraint, error) {
	constraint := Constraint{kind: kind, count: count, hasCount: true}
	if err := constraint.Validate(); err != nil {
		return Constraint{}, err
	}
	return constraint, nil
}

// NewValueConstraint creates a minimum-value or maximum-value constraint.
func NewValueConstraint(kind ConstraintKind, value Value) (Constraint, error) {
	constraint := Constraint{kind: kind, value: value, hasValue: true}
	if err := constraint.Validate(); err != nil {
		return Constraint{}, err
	}
	return constraint, nil
}

// NewPatternConstraint creates a regular-expression string constraint.
func NewPatternConstraint(pattern string) (Constraint, error) {
	constraint := Constraint{
		kind: ConstraintPattern, pattern: pattern, hasPattern: true,
	}
	if err := constraint.Validate(); err != nil {
		return Constraint{}, err
	}
	return constraint, nil
}

// NewAllowedValuesConstraint creates a non-empty canonical value-set
// constraint.
func NewAllowedValuesConstraint(values []Value) (Constraint, error) {
	constraint := Constraint{
		kind:    ConstraintAllowedValues,
		allowed: append([]Value(nil), values...),
	}
	sort.Slice(constraint.allowed, func(left, right int) bool {
		return constraint.allowed[left].canonical() < constraint.allowed[right].canonical()
	})
	if err := constraint.Validate(); err != nil {
		return Constraint{}, err
	}
	return constraint, nil
}

// Validate checks the kind-specific constraint representation.
func (c Constraint) Validate() error {
	switch c.kind {
	case ConstraintRequired, ConstraintUnique:
		if c.hasCount || c.hasValue || c.hasPattern || len(c.allowed) != 0 {
			return invalid("flag constraint contains an operand")
		}
	case ConstraintMinimumCount, ConstraintMaximumCount:
		if !c.hasCount || c.hasValue || c.hasPattern || len(c.allowed) != 0 {
			return invalid("count constraint requires exactly one count")
		}
	case ConstraintMinimumValue, ConstraintMaximumValue:
		if !c.hasValue || c.hasCount || c.hasPattern || len(c.allowed) != 0 {
			return invalid("value constraint requires exactly one value")
		}
		if err := c.value.Validate(); err != nil {
			return err
		}
		if c.value.Type() != ValueInteger && c.value.Type() != ValueNumber {
			return invalid("value bounds require a numeric value")
		}
	case ConstraintPattern:
		if !c.hasPattern || c.hasCount || c.hasValue || len(c.allowed) != 0 ||
			!requiredWire(c.pattern) {
			return invalid("pattern constraint requires a canonical pattern")
		}
		if _, err := regexp.Compile(c.pattern); err != nil {
			return invalid("pattern constraint is not a valid regular expression")
		}
	case ConstraintAllowedValues:
		if c.hasCount || c.hasValue || c.hasPattern || len(c.allowed) == 0 {
			return invalid("allowed-values constraint requires values")
		}
		for index, value := range c.allowed {
			if err := value.Validate(); err != nil {
				return err
			}
			if index > 0 &&
				c.allowed[index-1].canonical() >= value.canonical() {
				return invalid("allowed values must be unique and canonically ordered")
			}
		}
	default:
		return invalid("invalid constraint kind")
	}
	return nil
}

func (c Constraint) Kind() ConstraintKind {
	return c.kind
}

func (c Constraint) Count() (uint32, bool) {
	return c.count, c.hasCount
}

func (c Constraint) Value() (Value, bool) {
	return c.value, c.hasValue
}

func (c Constraint) Pattern() (string, bool) {
	return c.pattern, c.hasPattern
}

func (c Constraint) AllowedValues() []Value {
	return append([]Value(nil), c.allowed...)
}

func (c Constraint) clone() Constraint {
	c.allowed = append([]Value(nil), c.allowed...)
	return c
}

func (c Constraint) canonical() string {
	allowed := make([]string, len(c.allowed))
	for index, value := range c.allowed {
		allowed[index] = value.canonical()
	}
	return canonicalParts(
		string(c.kind),
		strconv.FormatUint(uint64(c.count), 10),
		strconv.FormatBool(c.hasCount),
		c.value.canonical(),
		strconv.FormatBool(c.hasValue),
		c.pattern,
		strconv.FormatBool(c.hasPattern),
		canonicalParts(allowed...),
	)
}

// AssertionDerivation records how a derived assertion can be recomputed.
type AssertionDerivation struct {
	id                    shoal.ID
	embeddingModel        string
	embeddingModelVersion string
	similarityMetric      string
	threshold             shoal.Score
	tessellationCell      string
	score                 shoal.Score
	sourceEndpoint        shoal.ID
	targetEndpoint        shoal.ID
	iteratorName          string
	iteratorOptions       shoal.Metadata
}

// NewAssertionDerivation creates a reproducible derivation record.
func NewAssertionDerivation(
	embeddingModel, embeddingModelVersion, similarityMetric string,
	threshold shoal.Score,
	tessellationCell string,
	score shoal.Score,
	sourceEndpoint, targetEndpoint shoal.ID,
	iteratorName string,
	iteratorOptions shoal.Metadata,
) (AssertionDerivation, error) {
	derivation := AssertionDerivation{
		embeddingModel:        embeddingModel,
		embeddingModelVersion: embeddingModelVersion,
		similarityMetric:      similarityMetric,
		threshold:             threshold,
		tessellationCell:      tessellationCell,
		score:                 score,
		sourceEndpoint:        sourceEndpoint,
		targetEndpoint:        targetEndpoint,
		iteratorName:          iteratorName,
		// Load-bearing: TestDerivationConstructorDoesNotAliasCallerOptions
		// pins that caller map mutations cannot alter derivation provenance.
		iteratorOptions: cloneMetadata(iteratorOptions),
	}
	id, err := derivationID(derivation)
	if err != nil {
		return AssertionDerivation{}, err
	}
	derivation.id = id
	if err := derivation.Validate(); err != nil {
		return AssertionDerivation{}, err
	}
	return derivation, nil
}

// Validate checks derivation identity and recomputation inputs.
func (d AssertionDerivation) Validate() error {
	if err := validateTypedID(d.id, "derivation"); err != nil {
		return err
	}
	if err := validateDerivationFields(d); err != nil {
		return err
	}
	expected, err := derivationID(d)
	if err != nil || expected != d.id {
		return invalid("derivation ID is not canonical")
	}
	return nil
}

func (d AssertionDerivation) ID() shoal.ID {
	return d.id
}

func (d AssertionDerivation) EmbeddingModel() string {
	return d.embeddingModel
}

func (d AssertionDerivation) EmbeddingModelVersion() string {
	return d.embeddingModelVersion
}

func (d AssertionDerivation) SimilarityMetric() string {
	return d.similarityMetric
}

func (d AssertionDerivation) Threshold() shoal.Score {
	return d.threshold
}

func (d AssertionDerivation) TessellationCell() string {
	return d.tessellationCell
}

func (d AssertionDerivation) Score() shoal.Score {
	return d.score
}

func (d AssertionDerivation) SourceEndpoint() shoal.ID {
	return d.sourceEndpoint
}

func (d AssertionDerivation) TargetEndpoint() shoal.ID {
	return d.targetEndpoint
}

func (d AssertionDerivation) IteratorName() string {
	return d.iteratorName
}

func (d AssertionDerivation) IteratorOptions() shoal.Metadata {
	// Load-bearing: TestDerivationAccessorDoesNotLeakInternalOptions pins
	// that callers cannot mutate the derivation provenance through accessors.
	return cloneMetadata(d.iteratorOptions)
}

func (d AssertionDerivation) clone() AssertionDerivation {
	// Load-bearing: TestAssertionDerivationCloneDoesNotAliasOptions pins
	// that cloned derivations do not share mutable provenance options.
	d.iteratorOptions = cloneMetadata(d.iteratorOptions)
	return d
}

func (d AssertionDerivation) canonical() string {
	// Load-bearing: TestAssertionDerivationIDIsCanonicalAndContentDerived
	// pins that every recomputation field below participates in identity.
	return canonicalParts(
		d.embeddingModel,
		d.embeddingModelVersion,
		d.similarityMetric,
		canonicalFloat(float64(d.threshold)),
		d.tessellationCell,
		canonicalFloat(float64(d.score)),
		string(d.sourceEndpoint),
		string(d.targetEndpoint),
		d.iteratorName,
		canonicalMetadata(d.iteratorOptions),
	)
}

func derivationID(derivation AssertionDerivation) (shoal.ID, error) {
	if err := validateDerivationFields(derivation); err != nil {
		return "", err
	}
	return deriveID("derivation", derivation.canonical())
}

func validateDerivationFields(derivation AssertionDerivation) error {
	if !requiredWire(derivation.embeddingModel) {
		return invalid("derivation embedding model is required")
	}
	if !requiredWire(derivation.embeddingModelVersion) {
		return invalid("derivation embedding model version is required")
	}
	if !requiredWire(derivation.similarityMetric) {
		return invalid("derivation similarity metric is required")
	}
	if err := validateFinite(float64(derivation.threshold), "derivation threshold"); err != nil {
		return err
	}
	if !requiredWire(derivation.tessellationCell) {
		return invalid("derivation tessellation cell is required")
	}
	if err := validateFinite(float64(derivation.score), "derivation score"); err != nil {
		return err
	}
	// Load-bearing: TestDerivationRejectsScoreBelowThreshold pins that a
	// derivation only represents an edge emitted at or above its threshold.
	if derivation.score < derivation.threshold {
		return invalid("derivation score must meet or exceed threshold")
	}
	if err := validateReference(derivation.sourceEndpoint, "derivation source endpoint"); err != nil {
		return err
	}
	if err := validateReference(derivation.targetEndpoint, "derivation target endpoint"); err != nil {
		return err
	}
	if !requiredWire(derivation.iteratorName) {
		return invalid("derivation iterator name is required")
	}
	return validateMetadata(derivation.iteratorOptions)
}

// EvidenceRef identifies exact immutable source evidence or a derivation.
type EvidenceRef struct {
	id            shoal.ID
	citation      document.Citation
	quote         string
	path          graph.Path
	hasPath       bool
	derivation    AssertionDerivation
	hasDerivation bool
	metadata      shoal.Metadata
}

type evidenceOptions struct {
	path    graph.Path
	hasPath bool
}

// EvidenceOption extends evidence construction.
type EvidenceOption func(*evidenceOptions)

// WithEvidencePath records the graph explanation associated with cited
// evidence.
func WithEvidencePath(path graph.Path) EvidenceOption {
	cloned := canonicalizeGraphPath(path)
	return func(options *evidenceOptions) {
		options.path = cloned
		options.hasPath = true
	}
}

// NewEvidenceRef creates a citation-backed evidence reference.
func NewEvidenceRef(
	citation document.Citation, quote string, metadata shoal.Metadata,
	options ...EvidenceOption,
) (EvidenceRef, error) {
	config := evidenceOptions{}
	for _, option := range options {
		if option != nil {
			option(&config)
		}
	}
	evidence := EvidenceRef{
		citation: citation,
		quote:    quote,
		path:     cloneGraphPath(config.path),
		hasPath:  config.hasPath,
		metadata: cloneMetadata(metadata),
	}
	id, err := evidenceID(citation, quote, evidence.path, evidence.hasPath)
	if err != nil {
		return EvidenceRef{}, err
	}
	evidence.id = id
	if err := evidence.Validate(); err != nil {
		return EvidenceRef{}, err
	}
	return evidence, nil
}

// NewDerivationEvidenceRef creates derivation-backed evidence.
func NewDerivationEvidenceRef(
	derivation AssertionDerivation, metadata shoal.Metadata,
) (EvidenceRef, error) {
	evidence := EvidenceRef{
		derivation:    derivation.clone(),
		hasDerivation: true,
		metadata:      cloneMetadata(metadata),
	}
	id, err := derivedEvidenceID(evidence.derivation)
	if err != nil {
		return EvidenceRef{}, err
	}
	evidence.id = id
	if err := evidence.Validate(); err != nil {
		return EvidenceRef{}, err
	}
	return evidence, nil
}

// Validate checks the evidence payload, metadata, and canonical identity.
func (e EvidenceRef) Validate() error {
	if err := validateTypedID(e.id, "evidence"); err != nil {
		return err
	}
	if e.hasDerivation {
		if e.citation != (document.Citation{}) || e.quote != "" || e.hasPath {
			return invalid("derivation evidence cannot include citation fields")
		}
		if err := e.derivation.Validate(); err != nil {
			return err
		}
		if err := validateMetadata(e.metadata); err != nil {
			return err
		}
		expected, err := derivedEvidenceID(e.derivation)
		if err != nil || expected != e.id {
			return invalid("evidence ID is not canonical")
		}
		return nil
	}
	if err := validateCitation(e.citation); err != nil {
		return err
	}
	if !validOpaqueWire(e.quote) {
		return invalid("evidence quote must be valid UTF-8")
	}
	if e.hasPath {
		if err := validateGraphPath(e.path); err != nil {
			return err
		}
	}
	if err := validateMetadata(e.metadata); err != nil {
		return err
	}
	expected, err := evidenceID(e.citation, e.quote, e.path, e.hasPath)
	if err != nil || expected != e.id {
		return invalid("evidence ID is not canonical")
	}
	return nil
}

func (e EvidenceRef) ID() shoal.ID {
	return e.id
}

func (e EvidenceRef) Citation() document.Citation {
	return e.citation
}

func (e EvidenceRef) Quote() string {
	return e.quote
}

func (e EvidenceRef) Path() (graph.Path, bool) {
	return cloneGraphPath(e.path), e.hasPath
}

func (e EvidenceRef) Derivation() (AssertionDerivation, bool) {
	return e.derivation.clone(), e.hasDerivation
}

func (e EvidenceRef) Metadata() shoal.Metadata {
	return cloneMetadata(e.metadata)
}

func (e EvidenceRef) clone() EvidenceRef {
	e.path = cloneGraphPath(e.path)
	e.derivation = e.derivation.clone()
	e.metadata = cloneMetadata(e.metadata)
	return e
}

func derivedEvidenceID(derivation AssertionDerivation) (shoal.ID, error) {
	if err := derivation.Validate(); err != nil {
		return "", err
	}
	return deriveID("evidence", "derivation", string(derivation.ID()))
}

func evidenceID(
	citation document.Citation, quote string, path graph.Path, hasPath bool,
) (shoal.ID, error) {
	if err := validateCitation(citation); err != nil {
		return "", err
	}
	if !validOpaqueWire(quote) {
		return "", invalid("evidence quote must be valid UTF-8")
	}
	if hasPath {
		if err := validateGraphPath(path); err != nil {
			return "", err
		}
	}
	return deriveID(
		"evidence",
		canonicalCitation(citation),
		quote,
		canonicalOptional(hasPath, canonicalGraphPath(path)),
	)
}

func validateCitation(citation document.Citation) error {
	if err := citation.Validate(); err != nil {
		return fmt.Errorf("evidence citation: %w", err)
	}
	for name, value := range map[string]shoal.ID{
		"citation document ID": citation.DocumentID,
		"citation revision ID": citation.RevisionID,
		"citation section ID":  citation.SectionID,
		"citation span ID":     citation.SpanID,
	} {
		if value != "" && !requiredWire(string(value)) {
			return invalid(name + " must be canonical UTF-8")
		}
	}
	return nil
}

func canonicalCitation(citation document.Citation) string {
	return canonicalParts(
		string(citation.DocumentID),
		string(citation.RevisionID),
		canonicalOptional(citation.SectionID != "", string(citation.SectionID)),
		canonicalOptional(citation.SpanID != "", string(citation.SpanID)),
		strconv.FormatInt(citation.Range.Start.Offset, 10),
		strconv.FormatInt(int64(citation.Range.Start.Page), 10),
		strconv.FormatInt(citation.Range.End.Offset, 10),
		strconv.FormatInt(int64(citation.Range.End.Page), 10),
	)
}

func validateGraphPath(path graph.Path) error {
	if err := path.Validate(); err != nil {
		return fmt.Errorf("evidence graph path: %w", err)
	}
	for _, node := range path.Nodes {
		if err := validateReference(node.ID, "graph path node ID"); err != nil {
			return err
		}
		if !requiredWire(node.Kind) {
			return invalid("graph path node kind is required")
		}
		for index, label := range node.Labels {
			if !requiredWire(label) {
				return invalid("graph path node labels must be exact UTF-8")
			}
			if index > 0 && node.Labels[index-1] >= label {
				return invalid("graph path node labels must be unique and canonically ordered")
			}
		}
		if err := validateMetadata(node.Properties); err != nil {
			return err
		}
	}
	for _, edge := range path.Edges {
		if err := validateReference(edge.ID, "graph path edge ID"); err != nil {
			return err
		}
		if err := validateReference(edge.From, "graph path edge source"); err != nil {
			return err
		}
		if err := validateReference(edge.To, "graph path edge target"); err != nil {
			return err
		}
		if !requiredWire(edge.Type) {
			return invalid("graph path edge type is required")
		}
		if err := validateFinite(float64(edge.Weight), "graph path edge weight"); err != nil {
			return err
		}
		if err := validateMetadata(edge.Properties); err != nil {
			return err
		}
	}
	return nil
}

func canonicalGraphPath(path graph.Path) string {
	nodes := make([]string, len(path.Nodes))
	for index, node := range path.Nodes {
		nodes[index] = canonicalParts(
			string(node.ID), node.Kind, canonicalParts(node.Labels...),
			canonicalMetadata(node.Properties),
		)
	}
	edges := make([]string, len(path.Edges))
	for index, edge := range path.Edges {
		edges[index] = canonicalParts(
			string(edge.ID), string(edge.From), string(edge.To), edge.Type,
			canonicalFloat(float64(edge.Weight)), canonicalMetadata(edge.Properties),
		)
	}
	return canonicalParts(canonicalParts(nodes...), canonicalParts(edges...))
}

func canonicalizeGraphPath(path graph.Path) graph.Path {
	cloned := cloneGraphPath(path)
	for index := range cloned.Nodes {
		sort.Strings(cloned.Nodes[index].Labels)
	}
	return cloned
}

func cloneGraphPath(path graph.Path) graph.Path {
	cloned := graph.Path{
		Nodes: make([]graph.Node, len(path.Nodes)),
		Edges: make([]graph.Edge, len(path.Edges)),
	}
	for index, node := range path.Nodes {
		cloned.Nodes[index] = node
		cloned.Nodes[index].Labels = append([]string(nil), node.Labels...)
		cloned.Nodes[index].Properties = cloneMetadata(node.Properties)
	}
	for index, edge := range path.Edges {
		cloned.Edges[index] = edge
		cloned.Edges[index].Properties = cloneMetadata(edge.Properties)
	}
	return cloned
}

// ExtractionProvenance records the provider, model, prompt, and extractor
// identities that can affect extracted assertions.
type ExtractionProvenance struct {
	provider         string
	model            string
	modelVersion     string
	prompt           string
	promptVersion    string
	extractor        string
	extractorVersion string
	metadata         shoal.Metadata
}

// NewExtractionProvenance creates complete extraction provenance.
func NewExtractionProvenance(
	provider, model, modelVersion, prompt, promptVersion,
	extractor, extractorVersion string,
	metadata shoal.Metadata,
) (ExtractionProvenance, error) {
	provenance := ExtractionProvenance{
		provider: provider, model: model, modelVersion: modelVersion,
		prompt: prompt, promptVersion: promptVersion,
		extractor: extractor, extractorVersion: extractorVersion,
		metadata: cloneMetadata(metadata),
	}
	if err := provenance.Validate(); err != nil {
		return ExtractionProvenance{}, err
	}
	return provenance, nil
}

// Validate checks all provenance fields and metadata.
func (p ExtractionProvenance) Validate() error {
	if !requiredWire(p.provider) || !requiredWire(p.model) ||
		!requiredWire(p.modelVersion) || !requiredWire(p.prompt) ||
		!requiredWire(p.promptVersion) || !requiredWire(p.extractor) ||
		!requiredWire(p.extractorVersion) {
		return invalid("complete model, prompt, and extractor provenance is required")
	}
	return validateMetadata(p.metadata)
}

func (p ExtractionProvenance) Provider() string {
	return p.provider
}

func (p ExtractionProvenance) Model() string {
	return p.model
}

func (p ExtractionProvenance) ModelVersion() string {
	return p.modelVersion
}

func (p ExtractionProvenance) Prompt() string {
	return p.prompt
}

func (p ExtractionProvenance) PromptVersion() string {
	return p.promptVersion
}

func (p ExtractionProvenance) Extractor() string {
	return p.extractor
}

func (p ExtractionProvenance) ExtractorVersion() string {
	return p.extractorVersion
}

func (p ExtractionProvenance) Metadata() shoal.Metadata {
	return cloneMetadata(p.metadata)
}

func (p ExtractionProvenance) clone() ExtractionProvenance {
	p.metadata = cloneMetadata(p.metadata)
	return p
}

func (p ExtractionProvenance) canonical() string {
	return canonicalParts(
		p.provider, p.model, p.modelVersion, p.prompt, p.promptVersion,
		p.extractor, p.extractorVersion, canonicalMetadata(p.metadata),
	)
}

// AssertionOrigin distinguishes stated, inferred, and reproducibly derived facts.
type AssertionOrigin string

const (
	AssertionExplicit AssertionOrigin = "explicit"
	AssertionInferred AssertionOrigin = "inferred"
	AssertionDerived  AssertionOrigin = "derived"
)

// Assertion is one immutable, attributable ontology fact.
//
// Explicit and inferred assertions remain document-cited. Derived assertions
// are backed by exactly one reproducible derivation record.
//
// ontologyIdentity names the ontology snapshot the assertion was made under.
// Definition IDs are stable across versions by design, so the identity is the
// only record of which meaning applied. It is unresolved unless a caller
// supplies it; it is never inferred from ambient state.
type Assertion struct {
	id               shoal.ID
	subject          shoal.ID
	subjectType      shoal.ID
	predicate        shoal.ID
	object           Value
	objectType       shoal.ID
	origin           AssertionOrigin
	confidence       shoal.Score
	evidence         []EvidenceRef
	provenance       ExtractionProvenance
	ontologyIdentity OntologyIdentity
	metadata         shoal.Metadata
}

type assertionOptions struct {
	subjectType      shoal.ID
	objectType       shoal.ID
	ontologyIdentity OntologyIdentity
}

// AssertionOption binds ontology type context to an assertion.
type AssertionOption func(*assertionOptions)

// WithAssertionSubjectType identifies the subject's concept or relationship type.
func WithAssertionSubjectType(subjectType shoal.ID) AssertionOption {
	return func(options *assertionOptions) {
		options.subjectType = subjectType
	}
}

// WithAssertionObjectType identifies a relationship object's concept type.
func WithAssertionObjectType(objectType shoal.ID) AssertionOption {
	return func(options *assertionOptions) {
		options.objectType = objectType
	}
}

// WithAssertionOntology records the ontology snapshot the assertion was made
// under. Omitting it leaves the assertion explicitly unresolved rather than
// binding it to whatever version happens to be current. Callers holding an
// OntologyVersion build the identity with NewOntologyIdentity, which validates
// the version rather than silently degrading a malformed one to unresolved.
func WithAssertionOntology(identity OntologyIdentity) AssertionOption {
	return func(options *assertionOptions) {
		options.ontologyIdentity = identity
	}
}

// NewAssertion creates an explicit, inferred, or derived assertion.
func NewAssertion(
	subject shoal.ID,
	predicate shoal.ID,
	object Value,
	origin AssertionOrigin,
	confidence shoal.Score,
	evidence []EvidenceRef,
	provenance ExtractionProvenance,
	metadata shoal.Metadata,
	options ...AssertionOption,
) (Assertion, error) {
	config := assertionOptions{}
	for _, option := range options {
		if option != nil {
			option(&config)
		}
	}
	assertion := Assertion{
		subject: subject, subjectType: config.subjectType,
		predicate: predicate, object: object, objectType: config.objectType, origin: origin,
		confidence: confidence, evidence: cloneEvidence(evidence),
		provenance: provenance.clone(), ontologyIdentity: config.ontologyIdentity,
		metadata: cloneMetadata(metadata),
	}
	sort.Slice(assertion.evidence, func(left, right int) bool {
		return string(assertion.evidence[left].ID()) <
			string(assertion.evidence[right].ID())
	})
	id, err := assertionID(assertion)
	if err != nil {
		return Assertion{}, err
	}
	assertion.id = id
	if err := assertion.Validate(); err != nil {
		return Assertion{}, err
	}
	return assertion, nil
}

// Validate checks assertion identity, provenance, evidence, and confidence.
func (a Assertion) Validate() error {
	if err := validateTypedID(a.id, "assertion"); err != nil {
		return err
	}
	if err := validateReference(a.subject, "assertion subject"); err != nil {
		return err
	}
	if a.subjectType != "" {
		if err := ValidateID(a.subjectType); err != nil {
			return err
		}
		namespace := IDNamespace(a.subjectType)
		if namespace != "concept" && namespace != "relationship" {
			return invalid("assertion subject type must be a concept or relationship")
		}
	}
	if err := ValidateID(a.predicate); err != nil {
		return err
	}
	if IDNamespace(a.predicate) != "property" &&
		IDNamespace(a.predicate) != "relationship" {
		return invalid("assertion predicate must be a property or relationship")
	}
	if err := a.object.Validate(); err != nil {
		return err
	}
	if a.objectType != "" {
		if IDNamespace(a.predicate) != "relationship" {
			return invalid("assertion object type requires a relationship predicate")
		}
		if err := validateTypedID(a.objectType, "concept"); err != nil {
			return err
		}
	}
	switch a.origin {
	case AssertionExplicit, AssertionInferred, AssertionDerived:
	default:
		return invalid("invalid assertion origin")
	}
	if err := validateFinite(float64(a.confidence), "assertion confidence"); err != nil {
		return err
	}
	if a.confidence < 0 || a.confidence > 1 {
		return invalid("assertion confidence must be between zero and one")
	}
	if len(a.evidence) == 0 {
		if a.origin == AssertionDerived {
			return invalid("derived assertion requires derivation evidence")
		}
		return invalid("assertion requires cited evidence")
	}
	// Load-bearing: TestDerivedAssertionRejectsMultipleDerivations pins that a
	// derived assertion is attributable to one reproducible derivation record.
	if a.origin == AssertionDerived && len(a.evidence) != 1 {
		return invalid("derived assertion requires exactly one derivation evidence")
	}
	for index, evidence := range a.evidence {
		if err := evidence.Validate(); err != nil {
			return err
		}
		// Load-bearing: TestDerivedAssertionRejectsCitationEvidence and
		// TestCitedAssertionsRejectDerivationEvidence pin that assertion
		// origins cannot silently swap or mix evidence kinds.
		if a.origin == AssertionDerived && !evidence.hasDerivation {
			return invalid("derived assertion requires derivation evidence")
		}
		if a.origin != AssertionDerived && evidence.hasDerivation {
			return invalid("cited assertion cannot use derivation evidence")
		}
		if index > 0 &&
			string(a.evidence[index-1].ID()) >= string(evidence.ID()) {
			return invalid("assertion evidence must be unique and canonically ordered")
		}
	}
	if err := a.provenance.Validate(); err != nil {
		return err
	}
	if a.ontologyIdentity.Known() {
		if err := a.ontologyIdentity.Validate(); err != nil {
			return err
		}
	}
	if err := validateMetadata(a.metadata); err != nil {
		return err
	}
	expected, err := assertionID(a)
	if err != nil || expected != a.id {
		return invalid("assertion ID is not canonical")
	}
	return nil
}

func (a Assertion) ID() shoal.ID {
	return a.id
}

func (a Assertion) Subject() shoal.ID {
	return a.subject
}

func (a Assertion) SubjectType() (shoal.ID, bool) {
	return a.subjectType, a.subjectType != ""
}

func (a Assertion) Predicate() shoal.ID {
	return a.predicate
}

func (a Assertion) Object() Value {
	return a.object
}

func (a Assertion) ObjectType() (shoal.ID, bool) {
	return a.objectType, a.objectType != ""
}

func (a Assertion) Origin() AssertionOrigin {
	return a.origin
}

func (a Assertion) Confidence() shoal.Score {
	return a.confidence
}

func (a Assertion) Evidence() []EvidenceRef {
	return cloneEvidence(a.evidence)
}

func (a Assertion) Provenance() ExtractionProvenance {
	return a.provenance.clone()
}

// Ontology returns the ontology snapshot the assertion was made under. The
// boolean is false when no identity was recorded, which is an explicit unknown
// and not a claim about any particular version.
func (a Assertion) Ontology() (OntologyIdentity, bool) {
	return a.ontologyIdentity, a.ontologyIdentity.Known()
}

// ReadUnder reports how the assertion's recorded ontology relates to the
// ontology a reader holds, so reinterpretation under a different version is
// detectable rather than silent. An assertion that recorded nothing reads as
// OntologyUnresolved under every reader.
func (a Assertion) ReadUnder(reader OntologyIdentity) OntologyReading {
	return ReadOntologyUnder(a.ontologyIdentity, reader)
}

func (a Assertion) Metadata() shoal.Metadata {
	return cloneMetadata(a.metadata)
}

func (a Assertion) clone() Assertion {
	a.evidence = cloneEvidence(a.evidence)
	a.provenance = a.provenance.clone()
	a.metadata = cloneMetadata(a.metadata)
	return a
}

func assertionID(assertion Assertion) (shoal.ID, error) {
	if err := validateReference(assertion.subject, "assertion subject"); err != nil {
		return "", err
	}
	if err := ValidateID(assertion.predicate); err != nil {
		return "", err
	}
	if err := assertion.object.Validate(); err != nil {
		return "", err
	}
	if err := assertion.provenance.Validate(); err != nil {
		return "", err
	}
	// A half-populated ontology identity is refused by Assertion.Validate,
	// which every constructed assertion passes through. It is deliberately not
	// re-checked here, so that guard has a single reachable enforcement point.
	evidenceIDs := make([]string, len(assertion.evidence))
	for index, evidence := range assertion.evidence {
		if err := evidence.Validate(); err != nil {
			return "", err
		}
		evidenceIDs[index] = string(evidence.ID())
	}
	return deriveID(
		"assertion",
		string(assertion.subject),
		string(assertion.subjectType),
		string(assertion.predicate),
		assertion.object.canonical(),
		string(assertion.objectType),
		string(assertion.origin),
		canonicalFloat(float64(assertion.confidence)),
		canonicalParts(evidenceIDs...),
		assertion.provenance.canonical(),
		assertion.ontologyIdentity.canonical(),
	)
}

func cloneEvidence(values []EvidenceRef) []EvidenceRef {
	cloned := make([]EvidenceRef, len(values))
	for index, value := range values {
		cloned[index] = value.clone()
	}
	return cloned
}

// ConceptDefinition declares one ontology concept and its properties.
type ConceptDefinition struct {
	id          shoal.ID
	key         string
	name        string
	description string
	properties  []shoal.ID
	metadata    shoal.Metadata
}

// NewConceptDefinition creates a concept definition.
func NewConceptDefinition(
	key, name, description string, properties []shoal.ID, metadata shoal.Metadata,
) (ConceptDefinition, error) {
	id, err := deriveDefinitionID("concept", key)
	if err != nil {
		return ConceptDefinition{}, err
	}
	concept := ConceptDefinition{
		id: id, key: key, name: name, description: description,
		properties: canonicalizeIDs(properties), metadata: cloneMetadata(metadata),
	}
	if err := concept.Validate(); err != nil {
		return ConceptDefinition{}, err
	}
	return concept, nil
}

// Validate checks the definition and canonical property order.
func (d ConceptDefinition) Validate() error {
	if err := validateDefinition(d.id, "concept", d.key, d.name, d.description, d.metadata); err != nil {
		return err
	}
	return validateCanonicalIDs(d.properties, "property", "concept properties", false)
}

func (d ConceptDefinition) ID() shoal.ID {
	return d.id
}

func (d ConceptDefinition) Key() string {
	return d.key
}

func (d ConceptDefinition) Name() string {
	return d.name
}

func (d ConceptDefinition) Description() string {
	return d.description
}

func (d ConceptDefinition) Properties() []shoal.ID {
	return cloneIDs(d.properties)
}

func (d ConceptDefinition) Metadata() shoal.Metadata {
	return cloneMetadata(d.metadata)
}

func (d ConceptDefinition) clone() ConceptDefinition {
	d.properties = cloneIDs(d.properties)
	d.metadata = cloneMetadata(d.metadata)
	return d
}

func (d ConceptDefinition) canonical() string {
	return canonicalParts(
		string(d.id), d.key, d.name, d.description,
		canonicalIDs(d.properties), canonicalMetadata(d.metadata),
	)
}

// RelationshipDefinition declares a directed or undirected relationship.
type RelationshipDefinition struct {
	id           shoal.ID
	key          string
	name         string
	description  string
	fromConcepts []shoal.ID
	toConcepts   []shoal.ID
	properties   []shoal.ID
	directed     bool
	metadata     shoal.Metadata
}

// NewRelationshipDefinition creates a relationship definition.
func NewRelationshipDefinition(
	key, name, description string,
	fromConcepts, toConcepts, properties []shoal.ID,
	directed bool,
	metadata shoal.Metadata,
) (RelationshipDefinition, error) {
	id, err := deriveDefinitionID("relationship", key)
	if err != nil {
		return RelationshipDefinition{}, err
	}
	relationship := RelationshipDefinition{
		id: id, key: key, name: name, description: description,
		fromConcepts: canonicalizeIDs(fromConcepts),
		toConcepts:   canonicalizeIDs(toConcepts),
		properties:   canonicalizeIDs(properties),
		directed:     directed, metadata: cloneMetadata(metadata),
	}
	if err := relationship.Validate(); err != nil {
		return RelationshipDefinition{}, err
	}
	return relationship, nil
}

// Validate checks endpoints, properties, and canonical ordering.
func (d RelationshipDefinition) Validate() error {
	if err := validateDefinition(
		d.id, "relationship", d.key, d.name, d.description, d.metadata,
	); err != nil {
		return err
	}
	if err := validateCanonicalIDs(
		d.fromConcepts, "concept", "relationship source concepts", true,
	); err != nil {
		return err
	}
	if err := validateCanonicalIDs(
		d.toConcepts, "concept", "relationship target concepts", true,
	); err != nil {
		return err
	}
	return validateCanonicalIDs(
		d.properties, "property", "relationship properties", false)
}

func (d RelationshipDefinition) ID() shoal.ID {
	return d.id
}

func (d RelationshipDefinition) Key() string {
	return d.key
}

func (d RelationshipDefinition) Name() string {
	return d.name
}

func (d RelationshipDefinition) Description() string {
	return d.description
}

func (d RelationshipDefinition) FromConcepts() []shoal.ID {
	return cloneIDs(d.fromConcepts)
}

func (d RelationshipDefinition) ToConcepts() []shoal.ID {
	return cloneIDs(d.toConcepts)
}

func (d RelationshipDefinition) Properties() []shoal.ID {
	return cloneIDs(d.properties)
}

func (d RelationshipDefinition) Directed() bool {
	return d.directed
}

func (d RelationshipDefinition) Metadata() shoal.Metadata {
	return cloneMetadata(d.metadata)
}

func (d RelationshipDefinition) clone() RelationshipDefinition {
	d.fromConcepts = cloneIDs(d.fromConcepts)
	d.toConcepts = cloneIDs(d.toConcepts)
	d.properties = cloneIDs(d.properties)
	d.metadata = cloneMetadata(d.metadata)
	return d
}

func (d RelationshipDefinition) canonical() string {
	fromConcepts := canonicalIDs(d.fromConcepts)
	toConcepts := canonicalIDs(d.toConcepts)
	if !d.directed && fromConcepts > toConcepts {
		fromConcepts, toConcepts = toConcepts, fromConcepts
	}
	return canonicalParts(
		string(d.id), d.key, d.name, d.description,
		fromConcepts, toConcepts,
		canonicalIDs(d.properties), strconv.FormatBool(d.directed),
		canonicalMetadata(d.metadata),
	)
}

// PropertyDefinition declares one typed ontology property.
type PropertyDefinition struct {
	id          shoal.ID
	key         string
	name        string
	description string
	valueType   ValueType
	constraints []Constraint
	metadata    shoal.Metadata
}

// NewPropertyDefinition creates a typed property definition.
func NewPropertyDefinition(
	key, name, description string,
	valueType ValueType,
	constraints []Constraint,
	metadata shoal.Metadata,
) (PropertyDefinition, error) {
	id, err := deriveDefinitionID("property", key)
	if err != nil {
		return PropertyDefinition{}, err
	}
	property := PropertyDefinition{
		id: id, key: key, name: name, description: description,
		valueType: valueType, constraints: cloneConstraints(constraints),
		metadata: cloneMetadata(metadata),
	}
	sort.Slice(property.constraints, func(left, right int) bool {
		return property.constraints[left].canonical() <
			property.constraints[right].canonical()
	})
	if err := property.Validate(); err != nil {
		return PropertyDefinition{}, err
	}
	return property, nil
}

// Validate checks property type and mutually consistent constraints.
func (d PropertyDefinition) Validate() error {
	if err := validateDefinition(d.id, "property", d.key, d.name, d.description, d.metadata); err != nil {
		return err
	}
	if !validValueType(d.valueType) {
		return invalid("invalid property value type")
	}
	seen := make(map[ConstraintKind]struct{}, len(d.constraints))
	var minimumCount, maximumCount *uint32
	var minimumValue, maximumValue *Value
	var allowedValues []Value
	var compiledPattern *regexp.Regexp
	for index, constraint := range d.constraints {
		if err := constraint.Validate(); err != nil {
			return err
		}
		if index > 0 &&
			d.constraints[index-1].canonical() >= constraint.canonical() {
			return invalid("property constraints must be unique and canonically ordered")
		}
		if _, duplicate := seen[constraint.Kind()]; duplicate {
			return invalid("property contains a duplicate constraint kind")
		}
		seen[constraint.Kind()] = struct{}{}
		switch constraint.Kind() {
		case ConstraintMinimumCount:
			value, _ := constraint.Count()
			minimumCount = &value
		case ConstraintMaximumCount:
			value, _ := constraint.Count()
			maximumCount = &value
		case ConstraintMinimumValue:
			value, _ := constraint.Value()
			if !valueMatchesType(value, d.valueType) {
				return invalid("minimum value does not match property type")
			}
			minimumValue = &value
		case ConstraintMaximumValue:
			value, _ := constraint.Value()
			if !valueMatchesType(value, d.valueType) {
				return invalid("maximum value does not match property type")
			}
			maximumValue = &value
		case ConstraintPattern:
			if d.valueType != ValueString {
				return invalid("pattern constraint requires a string property")
			}
			pattern, _ := constraint.Pattern()
			compiledPattern = regexp.MustCompile(pattern)
		case ConstraintAllowedValues:
			allowedValues = constraint.AllowedValues()
			for _, value := range allowedValues {
				if !valueMatchesType(value, d.valueType) {
					return invalid("allowed value does not match property type")
				}
			}
		}
	}
	if minimumCount != nil && maximumCount != nil &&
		*minimumCount > *maximumCount {
		return invalid("minimum count exceeds maximum count")
	}
	if _, required := seen[ConstraintRequired]; required &&
		maximumCount != nil && *maximumCount == 0 {
		return invalid("required property cannot have a maximum count of zero")
	}
	if minimumValue != nil && maximumValue != nil &&
		compareNumericValues(*minimumValue, *maximumValue) > 0 {
		return invalid("minimum value exceeds maximum value")
	}
	for _, value := range allowedValues {
		if !valueSatisfiesConstraints(value, d.constraints, false, compiledPattern) {
			return invalid("allowed value contradicts property constraints")
		}
	}
	return nil
}

func (d PropertyDefinition) ID() shoal.ID {
	return d.id
}

func (d PropertyDefinition) Key() string {
	return d.key
}

func (d PropertyDefinition) Name() string {
	return d.name
}

func (d PropertyDefinition) Description() string {
	return d.description
}

func (d PropertyDefinition) ValueType() ValueType {
	return d.valueType
}

func (d PropertyDefinition) Constraints() []Constraint {
	return cloneConstraints(d.constraints)
}

func (d PropertyDefinition) Metadata() shoal.Metadata {
	return cloneMetadata(d.metadata)
}

func (d PropertyDefinition) clone() PropertyDefinition {
	d.constraints = cloneConstraints(d.constraints)
	d.metadata = cloneMetadata(d.metadata)
	return d
}

func (d PropertyDefinition) canonical() string {
	constraints := make([]string, len(d.constraints))
	for index, constraint := range d.constraints {
		constraints[index] = constraint.canonical()
	}
	return canonicalParts(
		string(d.id), d.key, d.name, d.description, string(d.valueType),
		canonicalParts(constraints...), canonicalMetadata(d.metadata),
	)
}

func deriveDefinitionID(namespace, key string) (shoal.ID, error) {
	if !requiredWire(key) {
		return "", invalid("definition key is required")
	}
	return deriveID(namespace, key)
}

func validateDefinition(
	id shoal.ID, namespace, key, name, description string, metadata shoal.Metadata,
) error {
	if err := validateTypedID(id, namespace); err != nil {
		return err
	}
	if !requiredWire(key) || !requiredWire(name) || !optionalWire(description) {
		return invalid("definition key, name, or description is invalid")
	}
	expected, err := deriveDefinitionID(namespace, key)
	if err != nil || expected != id {
		return invalid("definition ID is not canonical")
	}
	return validateMetadata(metadata)
}

func validValueType(valueType ValueType) bool {
	switch valueType {
	case ValueString, ValueInteger, ValueNumber, ValueBoolean,
		ValueTimestamp, ValueReference:
		return true
	default:
		return false
	}
}

func valueMatchesType(value Value, valueType ValueType) bool {
	if valueType == ValueNumber {
		return value.Type() == ValueNumber || value.Type() == ValueInteger
	}
	return value.Type() == valueType
}

func numericRat(value Value) *big.Rat {
	if integer, ok := value.IntegerValue(); ok {
		return new(big.Rat).SetInt64(integer)
	}
	number, _ := value.NumberValue()
	result := new(big.Rat)
	result.SetFloat64(number)
	return result
}

func compareNumericValues(left, right Value) int {
	return numericRat(left).Cmp(numericRat(right))
}

func valueSatisfiesConstraints(
	value Value, constraints []Constraint, includeAllowed bool,
	compiledPattern *regexp.Regexp,
) bool {
	for _, constraint := range constraints {
		switch constraint.Kind() {
		case ConstraintMinimumValue:
			minimum, _ := constraint.Value()
			if compareNumericValues(value, minimum) < 0 {
				return false
			}
		case ConstraintMaximumValue:
			maximum, _ := constraint.Value()
			if compareNumericValues(value, maximum) > 0 {
				return false
			}
		case ConstraintPattern:
			text, _ := value.StringValue()
			if compiledPattern == nil {
				pattern, _ := constraint.Pattern()
				compiledPattern = regexp.MustCompile(pattern)
			}
			if !compiledPattern.MatchString(text) {
				return false
			}
		case ConstraintAllowedValues:
			if !includeAllowed {
				continue
			}
			matched := false
			for _, allowed := range constraint.AllowedValues() {
				numeric := (allowed.Type() == ValueInteger ||
					allowed.Type() == ValueNumber) &&
					(value.Type() == ValueInteger || value.Type() == ValueNumber)
				if numeric && compareNumericValues(allowed, value) == 0 ||
					!numeric && allowed.canonical() == value.canonical() {
					matched = true
					break
				}
			}
			if !matched {
				return false
			}
		}
	}
	return true
}

func cloneConstraints(values []Constraint) []Constraint {
	cloned := make([]Constraint, len(values))
	for index, value := range values {
		cloned[index] = value.clone()
	}
	return cloned
}

// OntologySchema identifies a logical ontology independently of its versions.
type OntologySchema struct {
	id          shoal.ID
	key         string
	name        string
	description string
	metadata    shoal.Metadata
}

// NewOntologySchema creates a logical ontology schema.
func NewOntologySchema(
	key, name, description string, metadata shoal.Metadata,
) (OntologySchema, error) {
	id, err := deriveDefinitionID("schema", key)
	if err != nil {
		return OntologySchema{}, err
	}
	schema := OntologySchema{
		id: id, key: key, name: name, description: description,
		metadata: cloneMetadata(metadata),
	}
	if err := schema.Validate(); err != nil {
		return OntologySchema{}, err
	}
	return schema, nil
}

// Validate checks logical schema identity and metadata.
func (s OntologySchema) Validate() error {
	return validateDefinition(s.id, "schema", s.key, s.name, s.description, s.metadata)
}

func (s OntologySchema) ID() shoal.ID {
	return s.id
}

func (s OntologySchema) Key() string {
	return s.key
}

func (s OntologySchema) Name() string {
	return s.name
}

func (s OntologySchema) Description() string {
	return s.description
}

func (s OntologySchema) Metadata() shoal.Metadata {
	return cloneMetadata(s.metadata)
}

func (s OntologySchema) canonical() string {
	return canonicalParts(
		string(s.id), s.key, s.name, s.description, canonicalMetadata(s.metadata))
}

func (s OntologySchema) clone() OntologySchema {
	s.metadata = cloneMetadata(s.metadata)
	return s
}

// OntologyVersion is an immutable, canonical schema snapshot.
type OntologyVersion struct {
	id            shoal.ID
	schema        OntologySchema
	version       string
	createdAt     time.Time
	concepts      []ConceptDefinition
	relationships []RelationshipDefinition
	properties    []PropertyDefinition
	metadata      shoal.Metadata
}

// NewOntologyVersion creates an immutable version. Definition order is
// canonicalized before identity derivation.
func NewOntologyVersion(
	schema OntologySchema,
	version string,
	createdAt time.Time,
	concepts []ConceptDefinition,
	relationships []RelationshipDefinition,
	properties []PropertyDefinition,
	metadata shoal.Metadata,
) (OntologyVersion, error) {
	result := OntologyVersion{
		schema: schema.clone(), version: version, createdAt: normalizeTime(createdAt),
		concepts:      cloneConcepts(concepts),
		relationships: cloneRelationships(relationships),
		properties:    cloneProperties(properties),
		metadata:      cloneMetadata(metadata),
	}
	sortDefinitions(&result)
	id, err := ontologyVersionID(result)
	if err != nil {
		return OntologyVersion{}, err
	}
	result.id = id
	if err := result.Validate(); err != nil {
		return OntologyVersion{}, err
	}
	return result, nil
}

// Validate checks the complete snapshot, member references, ordering, and
// content-derived identity.
func (v OntologyVersion) Validate() error {
	if err := validateTypedID(v.id, "ontology-version"); err != nil {
		return err
	}
	if err := v.schema.Validate(); err != nil {
		return err
	}
	if !requiredWire(v.version) {
		return invalid("ontology version is required")
	}
	if err := validateTime(v.createdAt, "ontology version creation time"); err != nil {
		return err
	}
	if err := validateMetadata(v.metadata); err != nil {
		return err
	}
	properties := make(map[shoal.ID]PropertyDefinition, len(v.properties))
	for index, property := range v.properties {
		if err := property.Validate(); err != nil {
			return fmt.Errorf("property definition: %w", err)
		}
		if index > 0 && string(v.properties[index-1].ID()) >= string(property.ID()) {
			return invalid("property definitions must be unique and canonically ordered")
		}
		properties[property.ID()] = property
	}
	concepts := make(map[shoal.ID]ConceptDefinition, len(v.concepts))
	for index, concept := range v.concepts {
		if err := concept.Validate(); err != nil {
			return fmt.Errorf("concept definition: %w", err)
		}
		if index > 0 && string(v.concepts[index-1].ID()) >= string(concept.ID()) {
			return invalid("concept definitions must be unique and canonically ordered")
		}
		for _, propertyID := range concept.Properties() {
			if _, exists := properties[propertyID]; !exists {
				return invalid("concept references an unknown property")
			}
		}
		concepts[concept.ID()] = concept
	}
	for index, relationship := range v.relationships {
		if err := relationship.Validate(); err != nil {
			return fmt.Errorf("relationship definition: %w", err)
		}
		if index > 0 &&
			string(v.relationships[index-1].ID()) >= string(relationship.ID()) {
			return invalid("relationship definitions must be unique and canonically ordered")
		}
		for _, conceptID := range append(
			relationship.FromConcepts(), relationship.ToConcepts()...,
		) {
			if _, exists := concepts[conceptID]; !exists {
				return invalid("relationship references an unknown concept")
			}
		}
		for _, propertyID := range relationship.Properties() {
			if _, exists := properties[propertyID]; !exists {
				return invalid("relationship references an unknown property")
			}
		}
	}
	expected, err := ontologyVersionID(v)
	if err != nil || expected != v.id {
		return invalid("ontology version ID is not canonical")
	}
	return nil
}

func (v OntologyVersion) ID() shoal.ID {
	return v.id
}

func (v OntologyVersion) Schema() OntologySchema {
	return v.schema.clone()
}

func (v OntologyVersion) Version() string {
	return v.version
}

func (v OntologyVersion) CreatedAt() time.Time {
	return v.createdAt
}

func (v OntologyVersion) Concepts() []ConceptDefinition {
	return cloneConcepts(v.concepts)
}

func (v OntologyVersion) Relationships() []RelationshipDefinition {
	return cloneRelationships(v.relationships)
}

func (v OntologyVersion) Properties() []PropertyDefinition {
	return cloneProperties(v.properties)
}

func (v OntologyVersion) Metadata() shoal.Metadata {
	return cloneMetadata(v.metadata)
}

func (v OntologyVersion) clone() OntologyVersion {
	v.schema = v.schema.clone()
	v.concepts = cloneConcepts(v.concepts)
	v.relationships = cloneRelationships(v.relationships)
	v.properties = cloneProperties(v.properties)
	v.metadata = cloneMetadata(v.metadata)
	return v
}

func (v OntologyVersion) property(id shoal.ID) (PropertyDefinition, bool) {
	for _, property := range v.properties {
		if property.ID() == id {
			return property.clone(), true
		}
	}
	return PropertyDefinition{}, false
}

func (v OntologyVersion) relationship(id shoal.ID) (RelationshipDefinition, bool) {
	for _, relationship := range v.relationships {
		if relationship.ID() == id {
			return relationship.clone(), true
		}
	}
	return RelationshipDefinition{}, false
}

func ontologyVersionID(version OntologyVersion) (shoal.ID, error) {
	if err := version.schema.Validate(); err != nil {
		return "", err
	}
	if !requiredWire(version.version) {
		return "", invalid("ontology version is required")
	}
	if err := validateTime(version.createdAt, "ontology version creation time"); err != nil {
		return "", err
	}
	concepts := make([]string, len(version.concepts))
	for index, concept := range version.concepts {
		concepts[index] = concept.canonical()
	}
	relationships := make([]string, len(version.relationships))
	for index, relationship := range version.relationships {
		relationships[index] = relationship.canonical()
	}
	properties := make([]string, len(version.properties))
	for index, property := range version.properties {
		properties[index] = property.canonical()
	}
	return deriveID(
		"ontology-version",
		version.schema.canonical(),
		version.version,
		canonicalTime(version.createdAt),
		canonicalParts(concepts...),
		canonicalParts(relationships...),
		canonicalParts(properties...),
		canonicalMetadata(version.metadata),
	)
}

func sortDefinitions(version *OntologyVersion) {
	sort.Slice(version.concepts, func(left, right int) bool {
		return string(version.concepts[left].ID()) <
			string(version.concepts[right].ID())
	})
	sort.Slice(version.relationships, func(left, right int) bool {
		return string(version.relationships[left].ID()) <
			string(version.relationships[right].ID())
	})
	sort.Slice(version.properties, func(left, right int) bool {
		return string(version.properties[left].ID()) <
			string(version.properties[right].ID())
	})
}

func cloneConcepts(values []ConceptDefinition) []ConceptDefinition {
	cloned := make([]ConceptDefinition, len(values))
	for index, value := range values {
		cloned[index] = value.clone()
	}
	return cloned
}

func cloneRelationships(values []RelationshipDefinition) []RelationshipDefinition {
	cloned := make([]RelationshipDefinition, len(values))
	for index, value := range values {
		cloned[index] = value.clone()
	}
	return cloned
}

func cloneProperties(values []PropertyDefinition) []PropertyDefinition {
	cloned := make([]PropertyDefinition, len(values))
	for index, value := range values {
		cloned[index] = value.clone()
	}
	return cloned
}

// ProposalState is the governed lifecycle state of an ontology proposal.
type ProposalState string

const (
	ProposalDraft        ProposalState = "draft"
	ProposalSubmitted    ProposalState = "submitted"
	ProposalApproved     ProposalState = "approved"
	ProposalPublished    ProposalState = "published"
	ProposalRejected     ProposalState = "rejected"
	ProposalWithdrawn    ProposalState = "withdrawn"
	MaxProposalMorphisms               = 128
)

// ProposalTransition records one immutable governance decision.
type ProposalTransition struct {
	from  ProposalState
	to    ProposalState
	actor string
	note  string
	at    time.Time
}

// NewProposalTransition creates a validated lifecycle transition.
func NewProposalTransition(
	from, to ProposalState, actor, note string, at time.Time,
) (ProposalTransition, error) {
	transition := ProposalTransition{
		from: from, to: to, actor: actor, note: note, at: normalizeTime(at),
	}
	if err := transition.Validate(); err != nil {
		return ProposalTransition{}, err
	}
	return transition, nil
}

// Validate checks the state edge, actor, note, and timestamp.
func (t ProposalTransition) Validate() error {
	if !validProposalTransition(t.from, t.to) {
		return invalid("invalid proposal state transition")
	}
	if !requiredWire(t.actor) || !requiredWire(t.note) {
		return invalid("proposal transition actor and note are required")
	}
	return validateTime(t.at, "proposal transition time")
}

func (t ProposalTransition) From() ProposalState {
	return t.from
}

func (t ProposalTransition) To() ProposalState {
	return t.to
}

func (t ProposalTransition) Actor() string {
	return t.actor
}

func (t ProposalTransition) Note() string {
	return t.note
}

func (t ProposalTransition) At() time.Time {
	return t.at
}

// GovernedProposal is an immutable proposed ontology version plus its
// replayable lifecycle.
type GovernedProposal struct {
	id              shoal.ID
	schema          OntologySchema
	baseSchemaID    shoal.ID
	baseVersionID   shoal.ID
	proposedVersion OntologyVersion
	proposedBy      string
	rationale       string
	createdAt       time.Time
	state           ProposalState
	transitions     []ProposalTransition
	morphisms       []OntologyMorphism
	metadata        shoal.Metadata
}

// NewGovernedProposal creates a draft proposal. baseVersion may be zero for
// the first version of a schema.
func NewGovernedProposal(
	schema OntologySchema,
	baseVersion OntologyVersion,
	proposedVersion OntologyVersion,
	proposedBy, rationale string,
	createdAt time.Time,
	metadata shoal.Metadata,
) (GovernedProposal, error) {
	return NewGovernedProposalWithMorphisms(
		schema, baseVersion, proposedVersion, nil,
		proposedBy, rationale, createdAt, metadata,
	)
}

// NewGovernedProposalWithMorphisms creates a draft proposal whose immutable
// semantic change set is governed by the same lifecycle as its version.
func NewGovernedProposalWithMorphisms(
	schema OntologySchema,
	baseVersion OntologyVersion,
	proposedVersion OntologyVersion,
	morphisms []OntologyMorphism,
	proposedBy, rationale string,
	createdAt time.Time,
	metadata shoal.Metadata,
) (GovernedProposal, error) {
	proposal := GovernedProposal{
		schema:          schema.clone(),
		proposedVersion: proposedVersion.clone(),
		morphisms:       cloneMorphisms(morphisms),
		proposedBy:      proposedBy, rationale: rationale,
		createdAt: normalizeTime(createdAt), state: ProposalDraft,
		metadata: cloneMetadata(metadata),
	}
	if baseVersion.ID() != "" {
		if err := baseVersion.Validate(); err != nil {
			return GovernedProposal{}, err
		}
		proposal.baseSchemaID = baseVersion.Schema().ID()
		proposal.baseVersionID = baseVersion.ID()
	}
	id, err := proposalID(proposal)
	if err != nil {
		return GovernedProposal{}, err
	}
	proposal.id = id
	if err := proposal.Validate(); err != nil {
		return GovernedProposal{}, err
	}
	return proposal, nil
}

// Validate replays the proposal lifecycle and checks immutable identity.
func (p GovernedProposal) Validate() error {
	if err := validateTypedID(p.id, "proposal"); err != nil {
		return err
	}
	if err := p.schema.Validate(); err != nil {
		return err
	}
	if (p.baseSchemaID == "") != (p.baseVersionID == "") {
		return invalid("proposal base schema and version must both be present")
	}
	if p.baseVersionID != "" {
		if err := validateTypedID(p.baseSchemaID, "schema"); err != nil {
			return err
		}
		if err := validateTypedID(p.baseVersionID, "ontology-version"); err != nil {
			return err
		}
		if p.baseSchemaID != p.schema.ID() {
			return invalid("proposal base version belongs to a different schema")
		}
	}
	if err := p.proposedVersion.Validate(); err != nil {
		return err
	}
	if p.proposedVersion.Schema().ID() != p.schema.ID() {
		return invalid("proposal version belongs to a different schema")
	}
	if p.baseVersionID == p.proposedVersion.ID() {
		return invalid("proposal base and proposed versions must differ")
	}
	sourceIdentity := OntologyIdentity{
		schemaID: p.baseSchemaID, versionID: p.baseVersionID,
	}
	targetIdentity, _ := NewOntologyIdentity(p.proposedVersion)
	if len(p.morphisms) > MaxProposalMorphisms {
		return invalid("proposal morphisms exceed the public bound")
	}
	for index, morphism := range p.morphisms {
		if err := morphism.Validate(); err != nil {
			return err
		}
		if p.baseVersionID == "" ||
			morphism.Source() != sourceIdentity || morphism.Target() != targetIdentity {
			return invalid("proposal morphism does not link its base and proposed versions")
		}
		if index > 0 && p.morphisms[index-1].ID() >= morphism.ID() {
			return invalid("proposal morphisms must be unique and canonically ordered")
		}
	}
	if !requiredWire(p.proposedBy) || !requiredWire(p.rationale) {
		return invalid("proposal author and rationale are required")
	}
	if err := validateTime(p.createdAt, "proposal creation time"); err != nil {
		return err
	}
	if err := validateMetadata(p.metadata); err != nil {
		return err
	}
	state := ProposalDraft
	previousAt := p.createdAt
	for _, transition := range p.transitions {
		if err := transition.Validate(); err != nil {
			return err
		}
		if transition.From() != state {
			return invalid("proposal transition does not continue lifecycle")
		}
		if !transition.At().After(previousAt) {
			return invalid("proposal transition times must increase")
		}
		state = transition.To()
		previousAt = transition.At()
	}
	if p.state != state {
		return invalid("proposal state does not match lifecycle")
	}
	expected, err := proposalID(p)
	if err != nil || expected != p.id {
		return invalid("proposal ID is not canonical")
	}
	return nil
}

// Transition returns a new proposal with a governance transition appended.
func (p GovernedProposal) Transition(
	next ProposalState, actor, note string, at time.Time,
) (GovernedProposal, error) {
	if err := p.Validate(); err != nil {
		return GovernedProposal{}, err
	}
	transition, err := NewProposalTransition(p.state, next, actor, note, at)
	if err != nil {
		return GovernedProposal{}, err
	}
	updated := p.clone()
	updated.state = next
	updated.transitions = append(updated.transitions, transition)
	if err := updated.Validate(); err != nil {
		return GovernedProposal{}, err
	}
	return updated, nil
}

func (p GovernedProposal) ID() shoal.ID {
	return p.id
}

func (p GovernedProposal) Schema() OntologySchema {
	return p.schema.clone()
}

func (p GovernedProposal) BaseVersionID() (shoal.ID, bool) {
	return p.baseVersionID, p.baseVersionID != ""
}

func (p GovernedProposal) ProposedVersion() OntologyVersion {
	return p.proposedVersion.clone()
}

func (p GovernedProposal) ProposedBy() string {
	return p.proposedBy
}

func (p GovernedProposal) Rationale() string {
	return p.rationale
}

func (p GovernedProposal) CreatedAt() time.Time {
	return p.createdAt
}

func (p GovernedProposal) UpdatedAt() time.Time {
	if len(p.transitions) == 0 {
		return p.createdAt
	}
	return p.transitions[len(p.transitions)-1].At()
}

func (p GovernedProposal) State() ProposalState {
	return p.state
}

func (p GovernedProposal) Transitions() []ProposalTransition {
	return append([]ProposalTransition(nil), p.transitions...)
}

func (p GovernedProposal) Morphisms() []OntologyMorphism {
	return cloneMorphisms(p.morphisms)
}

func (p GovernedProposal) Metadata() shoal.Metadata {
	return cloneMetadata(p.metadata)
}

func (p GovernedProposal) clone() GovernedProposal {
	p.schema = p.schema.clone()
	p.proposedVersion = p.proposedVersion.clone()
	p.transitions = append([]ProposalTransition(nil), p.transitions...)
	p.morphisms = cloneMorphisms(p.morphisms)
	p.metadata = cloneMetadata(p.metadata)
	return p
}

func proposalID(proposal GovernedProposal) (shoal.ID, error) {
	if err := proposal.schema.Validate(); err != nil {
		return "", err
	}
	if proposal.baseVersionID != "" {
		if err := validateTypedID(proposal.baseSchemaID, "schema"); err != nil {
			return "", err
		}
		if err := validateTypedID(proposal.baseVersionID, "ontology-version"); err != nil {
			return "", err
		}
	}
	if err := proposal.proposedVersion.Validate(); err != nil {
		return "", err
	}
	if !requiredWire(proposal.proposedBy) || !requiredWire(proposal.rationale) {
		return "", invalid("proposal author and rationale are required")
	}
	if err := validateTime(proposal.createdAt, "proposal creation time"); err != nil {
		return "", err
	}
	parts := []string{
		string(proposal.schema.ID()),
		canonicalOptional(
			proposal.baseVersionID != "",
			canonicalParts(string(proposal.baseSchemaID), string(proposal.baseVersionID)),
		),
		string(proposal.proposedVersion.ID()),
		proposal.proposedBy,
		proposal.rationale,
		canonicalTime(proposal.createdAt),
	}
	if len(proposal.morphisms) > 0 {
		ids := make([]string, len(proposal.morphisms))
		for i, morphism := range proposal.morphisms {
			ids[i] = string(morphism.ID())
		}
		parts = append(parts, canonicalParts(ids...))
	}
	return deriveID(
		"proposal",
		parts...,
	)
}

func cloneMorphisms(values []OntologyMorphism) []OntologyMorphism {
	cloned := make([]OntologyMorphism, len(values))
	for i, value := range values {
		cloned[i] = value.clone()
	}
	sort.Slice(cloned, func(i, j int) bool { return cloned[i].ID() < cloned[j].ID() })
	return cloned
}

func validProposalTransition(from, to ProposalState) bool {
	switch from {
	case ProposalDraft:
		return to == ProposalSubmitted || to == ProposalWithdrawn
	case ProposalSubmitted:
		return to == ProposalApproved || to == ProposalRejected ||
			to == ProposalWithdrawn
	case ProposalApproved:
		return to == ProposalPublished || to == ProposalWithdrawn
	default:
		return false
	}
}
