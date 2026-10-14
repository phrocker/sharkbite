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

package code

import (
	"strconv"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type syntaxNodeOptions struct {
	children   []ID
	attributes map[string]string
}

// SyntaxNodeOption extends syntax-node construction without changing the
// constructor signature.
type SyntaxNodeOption func(*syntaxNodeOptions)

// WithSyntaxChildren appends typed children in source order. Children with
// identical ranges are ordered by kind and then occurrence.
func WithSyntaxChildren(children ...SyntaxNode) SyntaxNodeOption {
	ids := make([]ID, len(children))
	for index, child := range children {
		ids[index] = child.ID()
	}
	return func(options *syntaxNodeOptions) {
		options.children = append(options.children, ids...)
	}
}

// WithSyntaxAttribute adds a parser-neutral syntax attribute.
func WithSyntaxAttribute(key, value string) SyntaxNodeOption {
	return func(options *syntaxNodeOptions) {
		if options.attributes == nil {
			options.attributes = make(map[string]string)
		}
		options.attributes[key] = value
	}
}

// SyntaxNode is a parser-neutral syntax-tree node. During actual declared
// root/child preorder traversal, Occurrence is the current zero-based counter
// for nodes with the same source, kind, and range.
type SyntaxNode struct {
	id          ID
	sourceID    ID
	kind        string
	sourceRange Range
	occurrence  uint32
	children    []ID
	attributes  map[string]string
}

// NewSyntaxNode derives a canonical typed ID and creates an immutable syntax
// node.
func NewSyntaxNode(source Source, kind string, sourceRange Range, occurrence uint32,
	options ...SyntaxNodeOption) (SyntaxNode, error) {
	if err := source.Validate(); err != nil {
		return SyntaxNode{}, err
	}
	config := syntaxNodeOptions{}
	for _, option := range options {
		if option != nil {
			option(&config)
		}
	}
	id, err := syntaxNodeID(source.ID(), kind, sourceRange, occurrence)
	if err != nil {
		return SyntaxNode{}, err
	}
	node := SyntaxNode{
		id:          id,
		sourceID:    source.ID(),
		kind:        kind,
		sourceRange: sourceRange,
		occurrence:  occurrence,
		children:    cloneIDs(config.children),
		attributes:  cloneAttributes(config.attributes),
	}
	if err := node.Validate(); err != nil {
		return SyntaxNode{}, err
	}
	return node, nil
}

func (n SyntaxNode) Validate() error {
	if err := validateTypedID(n.id, "syntax"); err != nil {
		return err
	}
	if err := validateTypedID(n.sourceID, "source"); err != nil {
		return err
	}
	if !requiredExact(n.kind) {
		return shoal.NewError(shoal.ErrorInvalidArgument, "syntax node kind is required")
	}
	if err := n.sourceRange.Validate(); err != nil {
		return err
	}
	expected, err := syntaxNodeID(n.sourceID, n.kind, n.sourceRange, n.occurrence)
	if err != nil || n.id != expected {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "syntax node ID is not canonical")
	}
	seen := make(map[ID]struct{}, len(n.children))
	for _, child := range n.children {
		if err := validateTypedID(child, "syntax"); err != nil {
			return err
		}
		if _, exists := seen[child]; exists {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "syntax node has a duplicate child")
		}
		seen[child] = struct{}{}
	}
	return validateAttributes(n.attributes)
}

func (n SyntaxNode) ID() ID {
	return n.id
}

func (n SyntaxNode) SourceID() ID {
	return n.sourceID
}

func (n SyntaxNode) Kind() string {
	return n.kind
}

func (n SyntaxNode) Range() Range {
	return n.sourceRange
}

func (n SyntaxNode) Occurrence() uint32 {
	return n.occurrence
}

func (n SyntaxNode) Children() []ID {
	return cloneIDs(n.children)
}

func (n SyntaxNode) Attributes() map[string]string {
	return cloneAttributes(n.attributes)
}

func (n SyntaxNode) clone() SyntaxNode {
	n.children = cloneIDs(n.children)
	n.attributes = cloneAttributes(n.attributes)
	return n
}

type semanticSymbolOptions struct {
	qualifiedName   string
	syntaxNodeID    ID
	hasSyntaxNodeID bool
	attributes      map[string]string
}

// SemanticSymbolOption extends semantic-symbol construction.
type SemanticSymbolOption func(*semanticSymbolOptions)

// WithSymbolQualifiedName sets the canonical language-qualified name.
func WithSymbolQualifiedName(qualifiedName string) SemanticSymbolOption {
	return func(options *semanticSymbolOptions) {
		options.qualifiedName = qualifiedName
	}
}

// WithSymbolSyntaxNode links a symbol to its typed declaration node.
func WithSymbolSyntaxNode(node SyntaxNode) SemanticSymbolOption {
	return func(options *semanticSymbolOptions) {
		options.syntaxNodeID = node.ID()
		options.hasSyntaxNodeID = true
	}
}

// WithSymbolAttribute adds a parser-neutral symbol attribute.
func WithSymbolAttribute(key, value string) SemanticSymbolOption {
	return func(options *semanticSymbolOptions) {
		if options.attributes == nil {
			options.attributes = make(map[string]string)
		}
		options.attributes[key] = value
	}
}

// SemanticSymbol is a parser-neutral declared semantic entity. Occurrence is
// the zero-based preorder occurrence among symbols with the same source, kind,
// and definition range. ParseResult requires every such group to be contiguous
// starting at zero.
type SemanticSymbol struct {
	id              ID
	sourceID        ID
	kind            string
	name            string
	qualifiedName   string
	definition      Range
	occurrence      uint32
	syntaxNodeID    ID
	hasSyntaxNodeID bool
	attributes      map[string]string
}

// NewSemanticSymbol derives a canonical typed ID and creates an immutable
// semantic symbol.
func NewSemanticSymbol(source Source, kind, name string, definition Range,
	occurrence uint32, options ...SemanticSymbolOption) (SemanticSymbol, error) {
	if err := source.Validate(); err != nil {
		return SemanticSymbol{}, err
	}
	config := semanticSymbolOptions{}
	for _, option := range options {
		if option != nil {
			option(&config)
		}
	}
	id, err := semanticSymbolID(
		source.ID(), kind, name, config.qualifiedName, definition, occurrence)
	if err != nil {
		return SemanticSymbol{}, err
	}
	symbol := SemanticSymbol{
		id:              id,
		sourceID:        source.ID(),
		kind:            kind,
		name:            name,
		qualifiedName:   config.qualifiedName,
		definition:      definition,
		occurrence:      occurrence,
		syntaxNodeID:    config.syntaxNodeID,
		hasSyntaxNodeID: config.hasSyntaxNodeID,
		attributes:      cloneAttributes(config.attributes),
	}
	if err := symbol.Validate(); err != nil {
		return SemanticSymbol{}, err
	}
	return symbol, nil
}

func (s SemanticSymbol) Validate() error {
	if err := validateTypedID(s.id, "symbol"); err != nil {
		return err
	}
	if err := validateTypedID(s.sourceID, "source"); err != nil {
		return err
	}
	if !requiredExact(s.kind) || !requiredExact(s.name) ||
		!optionalExact(s.qualifiedName) {
		return shoal.NewError(shoal.ErrorInvalidArgument, "invalid semantic symbol")
	}
	if err := s.definition.Validate(); err != nil {
		return err
	}
	if s.hasSyntaxNodeID {
		if err := validateTypedID(s.syntaxNodeID, "syntax"); err != nil {
			return err
		}
	}
	expected, err := semanticSymbolID(
		s.sourceID, s.kind, s.name, s.qualifiedName, s.definition, s.occurrence)
	if err != nil || s.id != expected {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "semantic symbol ID is not canonical")
	}
	return validateAttributes(s.attributes)
}

func (s SemanticSymbol) ID() ID {
	return s.id
}

func (s SemanticSymbol) SourceID() ID {
	return s.sourceID
}

func (s SemanticSymbol) Kind() string {
	return s.kind
}

func (s SemanticSymbol) Name() string {
	return s.name
}

func (s SemanticSymbol) QualifiedName() string {
	return s.qualifiedName
}

func (s SemanticSymbol) Definition() Range {
	return s.definition
}

func (s SemanticSymbol) Occurrence() uint32 {
	return s.occurrence
}

func (s SemanticSymbol) SyntaxNodeID() (ID, bool) {
	return s.syntaxNodeID, s.hasSyntaxNodeID
}

func (s SemanticSymbol) Attributes() map[string]string {
	return cloneAttributes(s.attributes)
}

func (s SemanticSymbol) clone() SemanticSymbol {
	s.attributes = cloneAttributes(s.attributes)
	return s
}

type externalEntityOptions struct {
	attributes map[string]string
}

// ExternalEntityOption extends external-entity construction.
type ExternalEntityOption func(*externalEntityOptions)

// WithExternalAttribute adds a parser-neutral external-entity attribute.
func WithExternalAttribute(key, value string) ExternalEntityOption {
	return func(options *externalEntityOptions) {
		if options.attributes == nil {
			options.attributes = make(map[string]string)
		}
		options.attributes[key] = value
	}
}

// ExternalEntity is a canonical relationship endpoint outside the parsed
// source, such as an imported module or library symbol.
type ExternalEntity struct {
	id            ID
	kind          string
	canonicalName string
	attributes    map[string]string
}

// NewExternalEntity derives a canonical typed ID from an external entity kind
// and fully qualified canonical name.
func NewExternalEntity(kind, canonicalName string,
	options ...ExternalEntityOption) (ExternalEntity, error) {
	config := externalEntityOptions{}
	for _, option := range options {
		if option != nil {
			option(&config)
		}
	}
	id, err := externalEntityID(kind, canonicalName)
	if err != nil {
		return ExternalEntity{}, err
	}
	entity := ExternalEntity{
		id:            id,
		kind:          kind,
		canonicalName: canonicalName,
		attributes:    cloneAttributes(config.attributes),
	}
	if err := entity.Validate(); err != nil {
		return ExternalEntity{}, err
	}
	return entity, nil
}

func (e ExternalEntity) Validate() error {
	if err := validateTypedID(e.id, "external"); err != nil {
		return err
	}
	if !requiredExact(e.kind) || !requiredExact(e.canonicalName) {
		return shoal.NewError(shoal.ErrorInvalidArgument, "invalid external entity")
	}
	expected, err := externalEntityID(e.kind, e.canonicalName)
	if err != nil || e.id != expected {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "external entity ID is not canonical")
	}
	return validateAttributes(e.attributes)
}

func (e ExternalEntity) ID() ID {
	return e.id
}

func (e ExternalEntity) Kind() string {
	return e.kind
}

func (e ExternalEntity) CanonicalName() string {
	return e.canonicalName
}

func (e ExternalEntity) Attributes() map[string]string {
	return cloneAttributes(e.attributes)
}

func (e ExternalEntity) clone() ExternalEntity {
	e.attributes = cloneAttributes(e.attributes)
	return e
}

// EndpointKind identifies the typed namespace of a relationship endpoint.
type EndpointKind string

const (
	EndpointSource   EndpointKind = "source"
	EndpointSyntax   EndpointKind = "syntax"
	EndpointSymbol   EndpointKind = "symbol"
	EndpointExternal EndpointKind = "external"
)

// Endpoint identifies one exact typed source, syntax, symbol, or external
// entity. Endpoints can only be created from typed values.
type Endpoint struct {
	kind EndpointKind
	id   ID
}

// SourceEndpoint returns the endpoint for an immutable source value.
func SourceEndpoint(source Source) Endpoint {
	return Endpoint{kind: EndpointSource, id: source.ID()}
}

// SyntaxEndpoint returns the endpoint for a syntax node.
func SyntaxEndpoint(node SyntaxNode) Endpoint {
	return Endpoint{kind: EndpointSyntax, id: node.ID()}
}

// SymbolEndpoint returns the endpoint for a semantic symbol.
func SymbolEndpoint(symbol SemanticSymbol) Endpoint {
	return Endpoint{kind: EndpointSymbol, id: symbol.ID()}
}

// ExternalEndpoint returns the endpoint for an external entity.
func ExternalEndpoint(entity ExternalEntity) Endpoint {
	return Endpoint{kind: EndpointExternal, id: entity.ID()}
}

func (e Endpoint) Validate() error {
	namespace, valid := endpointNamespace(e.kind)
	if !valid {
		return shoal.NewError(shoal.ErrorInvalidArgument, "invalid endpoint kind")
	}
	return validateTypedID(e.id, namespace)
}

func (e Endpoint) Kind() EndpointKind {
	return e.kind
}

func (e Endpoint) ID() ID {
	return e.id
}

// RelationshipKind identifies a supported parser-neutral semantic
// relationship.
type RelationshipKind string

const (
	RelationshipImport    RelationshipKind = "import"
	RelationshipCall      RelationshipKind = "call"
	RelationshipReference RelationshipKind = "reference"
	RelationshipContains  RelationshipKind = "contains"
)

type relationshipOptions struct {
	sourceRange Range
	hasRange    bool
	attributes  map[string]string
}

// RelationshipOption extends relationship construction.
type RelationshipOption func(*relationshipOptions)

// WithRelationshipRange sets exact source evidence for a relationship.
func WithRelationshipRange(sourceRange Range) RelationshipOption {
	return func(options *relationshipOptions) {
		options.sourceRange = sourceRange
		options.hasRange = true
	}
}

// WithRelationshipAttribute adds a parser-neutral relationship attribute.
func WithRelationshipAttribute(key, value string) RelationshipOption {
	return func(options *relationshipOptions) {
		if options.attributes == nil {
			options.attributes = make(map[string]string)
		}
		options.attributes[key] = value
	}
}

// Relationship is a directed semantic relationship. The supported endpoint
// matrix is:
//
//	import:    source|syntax -> external
//	call:      syntax|symbol -> symbol|external
//	reference: syntax|symbol -> symbol|external
//	contains:  source|syntax|symbol -> syntax|symbol
type Relationship struct {
	id          ID
	kind        RelationshipKind
	from        Endpoint
	to          Endpoint
	sourceRange Range
	hasRange    bool
	attributes  map[string]string
}

// NewRelationship validates the declared kind and endpoint matrix, then
// derives a canonical typed ID.
func NewRelationship(kind RelationshipKind, from, to Endpoint,
	options ...RelationshipOption) (Relationship, error) {
	config := relationshipOptions{}
	for _, option := range options {
		if option != nil {
			option(&config)
		}
	}
	id, err := relationshipID(
		kind, from, to, config.sourceRange, config.hasRange)
	if err != nil {
		return Relationship{}, err
	}
	relationship := Relationship{
		id:          id,
		kind:        kind,
		from:        from,
		to:          to,
		sourceRange: config.sourceRange,
		hasRange:    config.hasRange,
		attributes:  cloneAttributes(config.attributes),
	}
	if err := relationship.Validate(); err != nil {
		return Relationship{}, err
	}
	return relationship, nil
}

func (r Relationship) Validate() error {
	if err := validateTypedID(r.id, "relationship"); err != nil {
		return err
	}
	if !validRelationshipKind(r.kind) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "unsupported relationship kind")
	}
	if err := r.from.Validate(); err != nil {
		return err
	}
	if err := r.to.Validate(); err != nil {
		return err
	}
	if !validRelationshipEndpoints(r.kind, r.from.Kind(), r.to.Kind()) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "relationship endpoints are invalid for kind")
	}
	if r.hasRange {
		if err := r.sourceRange.Validate(); err != nil {
			return err
		}
	}
	expected, err := relationshipID(
		r.kind, r.from, r.to, r.sourceRange, r.hasRange)
	if err != nil || r.id != expected {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "relationship ID is not canonical")
	}
	return validateAttributes(r.attributes)
}

func (r Relationship) ID() ID {
	return r.id
}

func (r Relationship) Kind() RelationshipKind {
	return r.kind
}

func (r Relationship) From() Endpoint {
	return r.from
}

func (r Relationship) To() Endpoint {
	return r.to
}

func (r Relationship) Range() (Range, bool) {
	return r.sourceRange, r.hasRange
}

func (r Relationship) Attributes() map[string]string {
	return cloneAttributes(r.attributes)
}

func (r Relationship) clone() Relationship {
	r.attributes = cloneAttributes(r.attributes)
	return r
}

// DiagnosticSeverity is a parser-neutral diagnostic severity.
type DiagnosticSeverity string

const (
	DiagnosticError       DiagnosticSeverity = "error"
	DiagnosticWarning     DiagnosticSeverity = "warning"
	DiagnosticInformation DiagnosticSeverity = "information"
	DiagnosticHint        DiagnosticSeverity = "hint"
)

type diagnosticOptions struct {
	code        string
	sourceRange Range
	hasRange    bool
}

// DiagnosticOption extends diagnostic construction.
type DiagnosticOption func(*diagnosticOptions)

// WithDiagnosticCode sets the parser-defined diagnostic code.
func WithDiagnosticCode(code string) DiagnosticOption {
	return func(options *diagnosticOptions) {
		options.code = code
	}
}

// WithDiagnosticRange sets the exact source location of a diagnostic.
func WithDiagnosticRange(sourceRange Range) DiagnosticOption {
	return func(options *diagnosticOptions) {
		options.sourceRange = sourceRange
		options.hasRange = true
	}
}

// Diagnostic describes a parser or semantic-analysis finding.
type Diagnostic struct {
	severity    DiagnosticSeverity
	code        string
	message     string
	sourceRange Range
	hasRange    bool
}

// NewDiagnostic creates an immutable diagnostic.
func NewDiagnostic(severity DiagnosticSeverity, message string,
	options ...DiagnosticOption) (Diagnostic, error) {
	config := diagnosticOptions{}
	for _, option := range options {
		if option != nil {
			option(&config)
		}
	}
	diagnostic := Diagnostic{
		severity:    severity,
		code:        config.code,
		message:     message,
		sourceRange: config.sourceRange,
		hasRange:    config.hasRange,
	}
	if err := diagnostic.Validate(); err != nil {
		return Diagnostic{}, err
	}
	return diagnostic, nil
}

func (d Diagnostic) Validate() error {
	switch d.severity {
	case DiagnosticError, DiagnosticWarning, DiagnosticInformation, DiagnosticHint:
	default:
		return shoal.NewError(shoal.ErrorInvalidArgument, "invalid diagnostic severity")
	}
	if !optionalExact(d.code) || !requiredExact(d.message) {
		return shoal.NewError(shoal.ErrorInvalidArgument, "invalid diagnostic")
	}
	if d.hasRange {
		return d.sourceRange.Validate()
	}
	return nil
}

func (d Diagnostic) Severity() DiagnosticSeverity {
	return d.severity
}

func (d Diagnostic) Code() string {
	return d.code
}

func (d Diagnostic) Message() string {
	return d.message
}

func (d Diagnostic) Range() (Range, bool) {
	return d.sourceRange, d.hasRange
}

func syntaxNodeID(sourceID ID, kind string, sourceRange Range,
	occurrence uint32) (ID, error) {
	if !requiredExact(kind) {
		return ID{}, shoal.NewError(shoal.ErrorInvalidArgument, "syntax node kind is required")
	}
	if err := sourceRange.Validate(); err != nil {
		return ID{}, err
	}
	parts := []string{sourceID.String(), kind, strconv.FormatUint(uint64(occurrence), 10)}
	parts = append(parts, rangeIdentityParts(sourceRange)...)
	return deriveID("syntax", parts...)
}

func semanticSymbolID(sourceID ID, kind, name, qualifiedName string,
	definition Range, occurrence uint32) (ID, error) {
	if !requiredExact(kind) || !requiredExact(name) || !optionalExact(qualifiedName) {
		return ID{}, shoal.NewError(shoal.ErrorInvalidArgument, "invalid semantic symbol")
	}
	if err := definition.Validate(); err != nil {
		return ID{}, err
	}
	parts := []string{
		sourceID.String(),
		kind,
		name,
		qualifiedName,
		strconv.FormatUint(uint64(occurrence), 10),
	}
	parts = append(parts, rangeIdentityParts(definition)...)
	return deriveID("symbol", parts...)
}

func externalEntityID(kind, canonicalName string) (ID, error) {
	if !requiredExact(kind) || !requiredExact(canonicalName) {
		return ID{}, shoal.NewError(shoal.ErrorInvalidArgument, "invalid external entity")
	}
	return deriveID("external", kind, canonicalName)
}

func relationshipID(kind RelationshipKind, from, to Endpoint, sourceRange Range,
	hasRange bool) (ID, error) {
	if !validRelationshipKind(kind) {
		return ID{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "unsupported relationship kind")
	}
	if err := from.Validate(); err != nil {
		return ID{}, err
	}
	if err := to.Validate(); err != nil {
		return ID{}, err
	}
	if !validRelationshipEndpoints(kind, from.Kind(), to.Kind()) {
		return ID{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "relationship endpoints are invalid for kind")
	}
	parts := []string{
		string(kind),
		string(from.Kind()),
		from.ID().String(),
		string(to.Kind()),
		to.ID().String(),
		strconv.FormatBool(hasRange),
	}
	if hasRange {
		if err := sourceRange.Validate(); err != nil {
			return ID{}, err
		}
		parts = append(parts, rangeIdentityParts(sourceRange)...)
	}
	return deriveID("relationship", parts...)
}

func rangeIdentityParts(sourceRange Range) []string {
	return []string{
		strconv.FormatUint(sourceRange.Start().ByteOffset(), 10),
		strconv.FormatUint(uint64(sourceRange.Start().Line()), 10),
		strconv.FormatUint(uint64(sourceRange.Start().Column()), 10),
		strconv.FormatUint(sourceRange.End().ByteOffset(), 10),
		strconv.FormatUint(uint64(sourceRange.End().Line()), 10),
		strconv.FormatUint(uint64(sourceRange.End().Column()), 10),
	}
}

func validateTypedID(id ID, namespace string) error {
	if err := id.Validate(); err != nil {
		return err
	}
	if id.Namespace() != namespace {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "stable ID has the wrong typed namespace")
	}
	return nil
}

func endpointNamespace(kind EndpointKind) (string, bool) {
	switch kind {
	case EndpointSource:
		return "source", true
	case EndpointSyntax:
		return "syntax", true
	case EndpointSymbol:
		return "symbol", true
	case EndpointExternal:
		return "external", true
	default:
		return "", false
	}
}

func validRelationshipKind(kind RelationshipKind) bool {
	switch kind {
	case RelationshipImport, RelationshipCall,
		RelationshipReference, RelationshipContains:
		return true
	default:
		return false
	}
}

func validRelationshipEndpoints(
	kind RelationshipKind, from, to EndpointKind) bool {
	switch kind {
	case RelationshipImport:
		return (from == EndpointSource || from == EndpointSyntax) &&
			to == EndpointExternal
	case RelationshipCall, RelationshipReference:
		return (from == EndpointSyntax || from == EndpointSymbol) &&
			(to == EndpointSymbol || to == EndpointExternal)
	case RelationshipContains:
		return (from == EndpointSource || from == EndpointSyntax ||
			from == EndpointSymbol) &&
			(to == EndpointSyntax || to == EndpointSymbol)
	default:
		return false
	}
}

func cloneIDs(values []ID) []ID {
	return append([]ID(nil), values...)
}

func cloneAttributes(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func validateAttributes(attributes map[string]string) error {
	for key := range attributes {
		if !requiredExact(key) {
			return shoal.NewError(shoal.ErrorInvalidArgument, "attribute key is required")
		}
	}
	return nil
}
