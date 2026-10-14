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
	"bytes"
	"context"
	"fmt"
	"sort"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// ParseRequest supplies the exact source identity and bytes to a parser.
type ParseRequest struct {
	source  Source
	content []byte
}

// NewParseRequest creates an immutable parse request and verifies that the
// supplied bytes match the source's length and content hash.
func NewParseRequest(source Source, content []byte) (ParseRequest, error) {
	request := ParseRequest{
		source:  source,
		content: append([]byte(nil), content...),
	}
	if err := request.Validate(); err != nil {
		return ParseRequest{}, err
	}
	return request, nil
}

func (r ParseRequest) Validate() error {
	if err := r.source.Validate(); err != nil {
		return err
	}
	if uint64(len(r.content)) != r.source.SizeBytes() {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "source size does not match parse content")
	}
	if HashContent(r.content) != r.source.ContentHash() {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "source hash does not match parse content")
	}
	return nil
}

func (r ParseRequest) Source() Source {
	return r.source
}

// Content returns a copy of the exact source bytes.
func (r ParseRequest) Content() []byte {
	return append([]byte(nil), r.content...)
}

// ContentSize returns the exact source byte length without copying content.
func (r ParseRequest) ContentSize() uint64 {
	return uint64(len(r.content))
}

// ContentEqual compares exact source bytes without exposing or copying the
// request's retained content.
func (r ParseRequest) ContentEqual(content []byte) bool {
	return bytes.Equal(r.content, content)
}

// ContentStringEqual compares exact source bytes with a string without
// exposing or copying the request's retained content.
func (r ParseRequest) ContentStringEqual(content string) bool {
	if len(r.content) != len(content) {
		return false
	}
	for index, value := range r.content {
		if value != content[index] {
			return false
		}
	}
	return true
}

func (r ParseRequest) clone() ParseRequest {
	r.content = append([]byte(nil), r.content...)
	return r
}

type parseResultOptions struct {
	roots         []ID
	nodes         []SyntaxNode
	symbols       []SemanticSymbol
	externals     []ExternalEntity
	relationships []Relationship
	diagnostics   []Diagnostic
}

// ParseResultOption extends parse-result construction without exposing a
// source-breaking aggregate specification struct.
type ParseResultOption func(*parseResultOptions)

// WithSyntaxRoots adds typed root nodes in source order. Roots with identical
// ranges are ordered by kind and then occurrence.
func WithSyntaxRoots(roots ...SyntaxNode) ParseResultOption {
	ids := make([]ID, len(roots))
	for index, root := range roots {
		ids[index] = root.ID()
	}
	return func(options *parseResultOptions) {
		options.roots = append(options.roots, ids...)
	}
}

// WithSyntaxNodes adds syntax nodes to the result.
func WithSyntaxNodes(nodes ...SyntaxNode) ParseResultOption {
	cloned := cloneNodes(nodes)
	return func(options *parseResultOptions) {
		options.nodes = append(options.nodes, cloned...)
	}
}

// WithSemanticSymbols adds semantic symbols in canonical source order.
// Identical ranges are ordered by kind and then occurrence.
func WithSemanticSymbols(symbols ...SemanticSymbol) ParseResultOption {
	cloned := cloneSymbols(symbols)
	return func(options *parseResultOptions) {
		options.symbols = append(options.symbols, cloned...)
	}
}

// WithExternalEntities adds external relationship endpoints to the result.
func WithExternalEntities(externals ...ExternalEntity) ParseResultOption {
	cloned := cloneExternals(externals)
	return func(options *parseResultOptions) {
		options.externals = append(options.externals, cloned...)
	}
}

// WithRelationships adds semantic relationships to the result.
func WithRelationships(relationships ...Relationship) ParseResultOption {
	cloned := cloneRelationships(relationships)
	return func(options *parseResultOptions) {
		options.relationships = append(options.relationships, cloned...)
	}
}

// WithDiagnostics adds parser or semantic-analysis diagnostics to the result.
func WithDiagnostics(diagnostics ...Diagnostic) ParseResultOption {
	cloned := append([]Diagnostic(nil), diagnostics...)
	return func(options *parseResultOptions) {
		options.diagnostics = append(options.diagnostics, cloned...)
	}
}

// ParseResult is an immutable parser-neutral AST and semantic snapshot for one
// exact parse request.
type ParseResult struct {
	source        Source
	language      Language
	parser        ParserProvenance
	roots         []ID
	nodes         []SyntaxNode
	symbols       []SemanticSymbol
	externals     []ExternalEntity
	relationships []Relationship
	diagnostics   []Diagnostic
}

// ParseResultCounts reports collection and explicit source-attribution
// cardinalities without copying parse-result values.
type ParseResultCounts struct {
	SyntaxNodes         uint64
	Symbols             uint64
	Externals           uint64
	Relationships       uint64
	DeclaredSymbols     uint64
	RangedRelationships uint64
}

// NewParseResult constructs a result with functional options and validates it
// against the exact request bytes.
func NewParseResult(request ParseRequest, language Language, parser ParserProvenance,
	options ...ParseResultOption) (ParseResult, error) {
	if err := request.Validate(); err != nil {
		return ParseResult{}, err
	}
	config := parseResultOptions{}
	for _, option := range options {
		if option != nil {
			option(&config)
		}
	}
	result := ParseResult{
		source:        request.Source(),
		language:      language,
		parser:        parser,
		roots:         cloneIDs(config.roots),
		nodes:         cloneNodes(config.nodes),
		symbols:       cloneSymbols(config.symbols),
		externals:     cloneExternals(config.externals),
		relationships: cloneRelationships(config.relationships),
		diagnostics:   append([]Diagnostic(nil), config.diagnostics...),
	}
	if err := result.ValidateFor(request); err != nil {
		return ParseResult{}, err
	}
	return result, nil
}

// Validate checks canonical identities and structural invariants. Use
// ValidateFor when exact source bytes are available; ingestion requires it.
func (r ParseResult) Validate() error {
	index, err := r.validateValues()
	if err != nil {
		return err
	}
	return r.validateStructure(index)
}

// ValidateFor verifies the result against the exact parse request. Exact
// byte/line/UTF-8-byte-column coordinates are checked for every ranged value
// before any parent containment, child ordering, declaration containment, or
// relationship endpoint checks.
func (r ParseResult) ValidateFor(request ParseRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if !r.source.Equal(request.Source()) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "parse result source does not match request")
	}
	index, err := r.validateValues()
	if err != nil {
		return err
	}
	if err := r.validateExactCoordinates(request.content); err != nil {
		return err
	}
	return r.validateStructure(index)
}

func (r ParseResult) Source() Source {
	return r.source
}

func (r ParseResult) Language() Language {
	return r.language
}

func (r ParseResult) Parser() ParserProvenance {
	return r.parser
}

func (r ParseResult) Roots() []ID {
	return cloneIDs(r.roots)
}

func (r ParseResult) Nodes() []SyntaxNode {
	return cloneNodes(r.nodes)
}

func (r ParseResult) Symbols() []SemanticSymbol {
	return cloneSymbols(r.symbols)
}

func (r ParseResult) Externals() []ExternalEntity {
	return cloneExternals(r.externals)
}

func (r ParseResult) Relationships() []Relationship {
	return cloneRelationships(r.relationships)
}

func (r ParseResult) Diagnostics() []Diagnostic {
	return append([]Diagnostic(nil), r.diagnostics...)
}

// Counts returns collection and explicit source-attribution cardinalities
// without copying parser-owned values.
func (r ParseResult) Counts() ParseResultCounts {
	counts := ParseResultCounts{
		SyntaxNodes:   uint64(len(r.nodes)),
		Symbols:       uint64(len(r.symbols)),
		Externals:     uint64(len(r.externals)),
		Relationships: uint64(len(r.relationships)),
	}
	for _, symbol := range r.symbols {
		if _, present := symbol.SyntaxNodeID(); present {
			counts.DeclaredSymbols++
		}
	}
	for _, relationship := range r.relationships {
		if _, present := relationship.Range(); present {
			counts.RangedRelationships++
		}
	}
	return counts
}

// Node returns a copy of the syntax node with id.
func (r ParseResult) Node(id ID) (SyntaxNode, bool) {
	for _, node := range r.nodes {
		if node.ID() == id {
			return node.clone(), true
		}
	}
	return SyntaxNode{}, false
}

// Symbol returns a copy of the semantic symbol with id.
func (r ParseResult) Symbol(id ID) (SemanticSymbol, bool) {
	for _, symbol := range r.symbols {
		if symbol.ID() == id {
			return symbol.clone(), true
		}
	}
	return SemanticSymbol{}, false
}

// Parse is implemented by parser adapters. Implementations must not retain or
// mutate request values and must return a result that passes ValidateFor.
type Parse interface {
	Parse(context.Context, ParseRequest) (ParseResult, error)
}

// Parser is the conventional name for a Parse implementation.
type Parser = Parse

type validationIndex struct {
	nodes     map[ID]SyntaxNode
	symbols   map[ID]SemanticSymbol
	externals map[ID]ExternalEntity
}

func (r ParseResult) validateValues() (validationIndex, error) {
	if err := r.source.Validate(); err != nil {
		return validationIndex{}, fmt.Errorf("source: %w", err)
	}
	if err := validateTypedID(r.source.ID(), "source"); err != nil {
		return validationIndex{}, fmt.Errorf("source ID: %w", err)
	}
	if err := r.language.Validate(); err != nil {
		return validationIndex{}, fmt.Errorf("language: %w", err)
	}
	if err := r.parser.Validate(); err != nil {
		return validationIndex{}, fmt.Errorf("parser: %w", err)
	}

	allIDs := map[ID]string{r.source.ID(): "source"}
	nodes := make(map[ID]SyntaxNode, len(r.nodes))
	for _, node := range r.nodes {
		if err := node.Validate(); err != nil {
			return validationIndex{}, fmt.Errorf("syntax node: %w", err)
		}
		if node.SourceID() != r.source.ID() {
			return validationIndex{}, shoal.NewError(
				shoal.ErrorInvalidArgument, "syntax node belongs to a different source")
		}
		if err := registerID(allIDs, node.ID(), "syntax node"); err != nil {
			return validationIndex{}, err
		}
		if err := validateRangeInSource(node.Range(), r.source); err != nil {
			return validationIndex{}, fmt.Errorf("syntax node %s: %w", node.ID(), err)
		}
		nodes[node.ID()] = node
	}

	symbols := make(map[ID]SemanticSymbol, len(r.symbols))
	for _, symbol := range r.symbols {
		if err := symbol.Validate(); err != nil {
			return validationIndex{}, fmt.Errorf("semantic symbol: %w", err)
		}
		if symbol.SourceID() != r.source.ID() {
			return validationIndex{}, shoal.NewError(
				shoal.ErrorInvalidArgument, "semantic symbol belongs to a different source")
		}
		if err := registerID(allIDs, symbol.ID(), "semantic symbol"); err != nil {
			return validationIndex{}, err
		}
		if err := validateRangeInSource(symbol.Definition(), r.source); err != nil {
			return validationIndex{}, fmt.Errorf("semantic symbol %s: %w", symbol.ID(), err)
		}
		symbols[symbol.ID()] = symbol
	}

	externals := make(map[ID]ExternalEntity, len(r.externals))
	for _, external := range r.externals {
		if err := external.Validate(); err != nil {
			return validationIndex{}, fmt.Errorf("external entity: %w", err)
		}
		if err := registerID(allIDs, external.ID(), "external entity"); err != nil {
			return validationIndex{}, err
		}
		externals[external.ID()] = external
	}

	for _, relationship := range r.relationships {
		if err := relationship.Validate(); err != nil {
			return validationIndex{}, fmt.Errorf("relationship: %w", err)
		}
		if err := registerID(allIDs, relationship.ID(), "relationship"); err != nil {
			return validationIndex{}, err
		}
		if sourceRange, present := relationship.Range(); present {
			if err := validateRangeInSource(sourceRange, r.source); err != nil {
				return validationIndex{}, fmt.Errorf(
					"relationship %s: %w", relationship.ID(), err)
			}
		}
	}

	for _, diagnostic := range r.diagnostics {
		if err := diagnostic.Validate(); err != nil {
			return validationIndex{}, fmt.Errorf("diagnostic: %w", err)
		}
		if sourceRange, present := diagnostic.Range(); present {
			if err := validateRangeInSource(sourceRange, r.source); err != nil {
				return validationIndex{}, fmt.Errorf("diagnostic: %w", err)
			}
		}
	}
	return validationIndex{
		nodes: nodes, symbols: symbols, externals: externals,
	}, nil
}

func (r ParseResult) validateExactCoordinates(content []byte) error {
	lineStarts := deriveLineStarts(content)
	for _, node := range r.nodes {
		if err := validateExactRange(node.Range(), lineStarts, uint64(len(content))); err != nil {
			return fmt.Errorf("syntax node %s coordinates: %w", node.ID(), err)
		}
	}
	for _, symbol := range r.symbols {
		if err := validateExactRange(
			symbol.Definition(), lineStarts, uint64(len(content))); err != nil {
			return fmt.Errorf("semantic symbol %s coordinates: %w", symbol.ID(), err)
		}
	}
	for _, relationship := range r.relationships {
		if sourceRange, present := relationship.Range(); present {
			if err := validateExactRange(
				sourceRange, lineStarts, uint64(len(content))); err != nil {
				return fmt.Errorf("relationship %s coordinates: %w", relationship.ID(), err)
			}
		}
	}
	for _, diagnostic := range r.diagnostics {
		if sourceRange, present := diagnostic.Range(); present {
			if err := validateExactRange(
				sourceRange, lineStarts, uint64(len(content))); err != nil {
				return fmt.Errorf("diagnostic coordinates: %w", err)
			}
		}
	}
	return nil
}

func (r ParseResult) validateStructure(index validationIndex) error {
	if err := validateSemanticSymbolOccurrences(r.symbols); err != nil {
		return err
	}
	if err := validateSemanticSymbolOrder(r.symbols); err != nil {
		return err
	}
	if err := validateSyntaxTree(r.roots, index.nodes); err != nil {
		return err
	}
	if err := validateSyntaxPreorderOccurrences(r.roots, index.nodes); err != nil {
		return err
	}
	for _, symbol := range r.symbols {
		if syntaxNodeID, present := symbol.SyntaxNodeID(); present {
			node, exists := index.nodes[syntaxNodeID]
			if !exists {
				return shoal.NewError(
					shoal.ErrorInvalidArgument, "semantic symbol references unknown syntax node")
			}
			if !node.Range().Contains(symbol.Definition()) {
				return shoal.NewError(
					shoal.ErrorInvalidArgument,
					"semantic symbol definition is outside its syntax node")
			}
		}
	}
	for _, relationship := range r.relationships {
		if !endpointExists(
			relationship.From(), r.source, index.nodes, index.symbols, index.externals) ||
			!endpointExists(
				relationship.To(), r.source, index.nodes, index.symbols, index.externals) {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "relationship endpoint does not exist")
		}
	}
	return nil
}

func registerID(ids map[ID]string, id ID, kind string) error {
	if previous, exists := ids[id]; exists {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			fmt.Sprintf("%s ID collides with %s ID", kind, previous))
	}
	ids[id] = kind
	return nil
}

func validateRangeInSource(sourceRange Range, source Source) error {
	if err := sourceRange.Validate(); err != nil {
		return err
	}
	if sourceRange.End().ByteOffset() > source.SizeBytes() {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "source range exceeds source content")
	}
	return nil
}

func deriveLineStarts(content []byte) []uint64 {
	lineStarts := []uint64{0}
	for index, value := range content {
		if value == '\n' {
			lineStarts = append(lineStarts, uint64(index+1))
		}
	}
	return lineStarts
}

func validateExactRange(sourceRange Range, lineStarts []uint64, size uint64) error {
	if err := validateExactPosition(sourceRange.Start(), lineStarts, size); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	if err := validateExactPosition(sourceRange.End(), lineStarts, size); err != nil {
		return fmt.Errorf("end: %w", err)
	}
	return nil
}

func validateExactPosition(position Position, lineStarts []uint64, size uint64) error {
	if err := position.Validate(); err != nil {
		return err
	}
	if position.ByteOffset() > size {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "position exceeds source content")
	}
	lineIndex := sort.Search(len(lineStarts), func(index int) bool {
		return lineStarts[index] > position.ByteOffset()
	}) - 1
	expectedLine := uint64(lineIndex + 1)
	expectedColumn := position.ByteOffset() - lineStarts[lineIndex] + 1
	if uint64(position.Line()) != expectedLine ||
		uint64(position.Column()) != expectedColumn {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"byte offset does not match line and UTF-8 byte column")
	}
	return nil
}

func validateSyntaxTree(roots []ID, nodes map[ID]SyntaxNode) error {
	if len(nodes) == 0 {
		if len(roots) != 0 {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "syntax roots require syntax nodes")
		}
		return nil
	}
	if len(roots) == 0 {
		return shoal.NewError(shoal.ErrorInvalidArgument, "syntax tree requires a root")
	}

	parentCount := make(map[ID]int, len(nodes))
	for id := range nodes {
		parentCount[id] = 0
	}
	for _, node := range nodes {
		children := node.Children()
		if err := validateNodeOrder(children, nodes, "syntax children"); err != nil {
			return err
		}
		for _, childID := range children {
			child, exists := nodes[childID]
			if !exists {
				return shoal.NewError(
					shoal.ErrorInvalidArgument, "syntax node references unknown child")
			}
			if childID == node.ID() {
				return shoal.NewError(
					shoal.ErrorInvalidArgument, "syntax node cannot contain itself")
			}
			parentCount[childID]++
			if parentCount[childID] > 1 {
				return shoal.NewError(
					shoal.ErrorInvalidArgument, "syntax node has multiple parents")
			}
			if !node.Range().Contains(child.Range()) {
				return shoal.NewError(
					shoal.ErrorInvalidArgument, "syntax child range is outside its parent")
			}
		}
	}

	rootSet := make(map[ID]struct{}, len(roots))
	for _, rootID := range roots {
		if err := validateTypedID(rootID, "syntax"); err != nil {
			return err
		}
		if _, exists := nodes[rootID]; !exists {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "syntax root does not exist")
		}
		if _, duplicate := rootSet[rootID]; duplicate {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "duplicate syntax root")
		}
		rootSet[rootID] = struct{}{}
	}
	if err := validateNodeOrder(roots, nodes, "syntax roots"); err != nil {
		return err
	}
	for id, count := range parentCount {
		_, declaredRoot := rootSet[id]
		if (count == 0) != declaredRoot {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "syntax roots do not match parentless nodes")
		}
	}

	state := make(map[ID]uint8, len(nodes))
	var visit func(ID) error
	visit = func(id ID) error {
		switch state[id] {
		case 1:
			return shoal.NewError(shoal.ErrorInvalidArgument, "syntax tree contains a cycle")
		case 2:
			return nil
		}
		state[id] = 1
		for _, childID := range nodes[id].Children() {
			if err := visit(childID); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for _, rootID := range roots {
		if err := visit(rootID); err != nil {
			return err
		}
	}
	if len(state) != len(nodes) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "syntax tree contains unreachable nodes")
	}
	return nil
}

func validateNodeOrder(ids []ID, nodes map[ID]SyntaxNode, label string) error {
	for index, id := range ids {
		node, exists := nodes[id]
		if !exists {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, label+" contain an unknown node")
		}
		if index == 0 {
			continue
		}
		previous := nodes[ids[index-1]]
		if previous.Range() == node.Range() {
			if previous.Kind() > node.Kind() ||
				(previous.Kind() == node.Kind() &&
					previous.Occurrence() >= node.Occurrence()) {
				return shoal.NewError(
					shoal.ErrorInvalidArgument,
					label+" have a non-canonical kind and occurrence order")
			}
			continue
		}
		if previous.Range().End().ByteOffset() > node.Range().Start().ByteOffset() {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, label+" overlap or are not in source order")
		}
	}
	return nil
}

type occurrenceGroup struct {
	sourceID    ID
	kind        string
	sourceRange Range
}

func validateSyntaxPreorderOccurrences(roots []ID, nodes map[ID]SyntaxNode) error {
	counters := make(map[occurrenceGroup]uint32)
	visited := make(map[ID]struct{}, len(nodes))
	var visit func(ID) error
	visit = func(id ID) error {
		if _, duplicate := visited[id]; duplicate {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"syntax node appears more than once in declared preorder")
		}
		node, exists := nodes[id]
		if !exists {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"declared syntax preorder references an unknown node")
		}
		key := occurrenceGroup{
			sourceID: node.SourceID(), kind: node.Kind(), sourceRange: node.Range(),
		}
		expected := counters[key]
		if node.Occurrence() != expected {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"syntax node occurrence does not match declared preorder")
		}
		counters[key] = expected + 1
		visited[id] = struct{}{}
		for _, childID := range node.Children() {
			if err := visit(childID); err != nil {
				return err
			}
		}
		return nil
	}
	for _, rootID := range roots {
		if err := visit(rootID); err != nil {
			return err
		}
	}
	if len(visited) != len(nodes) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"declared syntax preorder does not include every node")
	}
	return nil
}

func validateSemanticSymbolOccurrences(symbols []SemanticSymbol) error {
	groups := make(map[occurrenceGroup][]uint32)
	for _, symbol := range symbols {
		key := occurrenceGroup{
			sourceID:    symbol.SourceID(),
			kind:        symbol.Kind(),
			sourceRange: symbol.Definition(),
		}
		groups[key] = append(groups[key], symbol.Occurrence())
	}
	return validateContiguousOccurrences(groups, "semantic symbols")
}

func validateContiguousOccurrences(
	groups map[occurrenceGroup][]uint32, label string) error {
	for _, occurrences := range groups {
		sort.Slice(occurrences, func(left, right int) bool {
			return occurrences[left] < occurrences[right]
		})
		for expected, actual := range occurrences {
			if actual != uint32(expected) {
				return shoal.NewError(
					shoal.ErrorInvalidArgument,
					label+" require contiguous zero-based occurrences")
			}
		}
	}
	return nil
}

func validateSemanticSymbolOrder(symbols []SemanticSymbol) error {
	for index := 1; index < len(symbols); index++ {
		previous := symbols[index-1]
		current := symbols[index]
		previousRange := previous.Definition()
		currentRange := current.Definition()
		if previousRange.Start().ByteOffset() > currentRange.Start().ByteOffset() ||
			(previousRange.Start().ByteOffset() == currentRange.Start().ByteOffset() &&
				previousRange.End().ByteOffset() > currentRange.End().ByteOffset()) {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"semantic symbols are not in canonical source order")
		}
		if previousRange == currentRange &&
			(previous.Kind() > current.Kind() ||
				(previous.Kind() == current.Kind() &&
					previous.Occurrence() >= current.Occurrence())) {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"semantic symbols have a non-canonical kind and occurrence order")
		}
	}
	return nil
}

func endpointExists(endpoint Endpoint, source Source,
	nodes map[ID]SyntaxNode, symbols map[ID]SemanticSymbol,
	externals map[ID]ExternalEntity) bool {
	switch endpoint.Kind() {
	case EndpointSource:
		return endpoint.ID() == source.ID()
	case EndpointSyntax:
		_, exists := nodes[endpoint.ID()]
		return exists
	case EndpointSymbol:
		_, exists := symbols[endpoint.ID()]
		return exists
	case EndpointExternal:
		_, exists := externals[endpoint.ID()]
		return exists
	default:
		return false
	}
}

func cloneNodes(values []SyntaxNode) []SyntaxNode {
	cloned := make([]SyntaxNode, len(values))
	for index, value := range values {
		cloned[index] = value.clone()
	}
	return cloned
}

func cloneSymbols(values []SemanticSymbol) []SemanticSymbol {
	cloned := make([]SemanticSymbol, len(values))
	for index, value := range values {
		cloned[index] = value.clone()
	}
	return cloned
}

func cloneExternals(values []ExternalEntity) []ExternalEntity {
	cloned := make([]ExternalEntity, len(values))
	for index, value := range values {
		cloned[index] = value.clone()
	}
	return cloned
}

func cloneRelationships(values []Relationship) []Relationship {
	cloned := make([]Relationship, len(values))
	for index, value := range values {
		cloned[index] = value.clone()
	}
	return cloned
}
