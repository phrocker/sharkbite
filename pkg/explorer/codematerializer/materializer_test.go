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

package codematerializer_test

import (
	"bytes"
	"encoding/json"
	"testing"

	codeast "github.com/phrocker/shoal-oss/pkg/code"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer/codematerializer"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestMaterializeNestedCodeDeterministically(t *testing.T) {
	fixture := newMaterializerFixture(t)

	first, err := codematerializer.Materialize(
		fixture.ingest, fixture.content)
	if err != nil {
		t.Fatalf("first materialization: %v", err)
	}
	second, err := codematerializer.Materialize(
		fixture.ingest, append([]byte(nil), fixture.content...))
	if err != nil {
		t.Fatalf("second materialization: %v", err)
	}
	if err := first.ValidateFor(fixture.ingest); err != nil {
		t.Fatalf("validate materialization: %v", err)
	}

	firstSnapshot := materializationSnapshot(t, first)
	secondSnapshot := materializationSnapshot(t, second)
	if !bytes.Equal(firstSnapshot, secondSnapshot) {
		t.Fatalf("repeated materialization changed:\n%s\n%s",
			firstSnapshot, secondSnapshot)
	}
	if first.Version() != codematerializer.Version {
		t.Fatalf("version = %q", first.Version())
	}

	expectedDocumentID := stableID(
		t, "shoal.document",
		fixture.source.Repository().Locator(),
		fixture.source.Path(),
	)
	expectedRevisionID := stableID(
		t, "shoal.revision",
		string(expectedDocumentID),
		fixture.source.ID().String(),
		fixture.ingest.IdempotencyKey().String(),
	)
	expectedRootID := stableID(
		t, "shoal.section", string(expectedRevisionID), "root")
	expectedGraphArtifactID := stableID(
		t, "shoal.graph-publication",
		fixture.ingest.IdempotencyKey().String(),
	)
	doc := first.Document()
	revision := first.Revision()
	if doc.ID != expectedDocumentID ||
		doc.RevisionID != expectedRevisionID ||
		doc.RootSectionID != expectedRootID ||
		revision.ID != expectedRevisionID {
		t.Fatalf("canonical identities = doc %q revision %q root %q",
			doc.ID, revision.ID, doc.RootSectionID)
	}
	if revision.CreatedAt.IsZero() == false {
		t.Fatalf("created_at must remain deterministic zero, got %v", revision.CreatedAt)
	}
	if revision.SourceVersion != fixture.source.Revision() {
		t.Fatalf("source version = %q", revision.SourceVersion)
	}

	publicSource := first.Source()
	if publicSource.Content != string(fixture.content) ||
		publicSource.URI !=
			fixture.source.Repository().Locator()+"#/"+fixture.source.Path() ||
		publicSource.Title != "café.go" {
		t.Fatalf("public source = %+v", publicSource)
	}
	if publicSource.Metadata["shoal.code.path"] != fixture.source.Path() {
		t.Fatalf("public source metadata = %#v", publicSource.Metadata)
	}
	if _, present := publicSource.Metadata["shoal.code.ref"]; present {
		t.Fatal("source ref leaked into canonical metadata")
	}

	sections := first.Sections()
	spans := first.Spans()
	expectedSyntax := []codeast.SyntaxNode{
		fixture.fileNode,
		fixture.importNode,
		fixture.functionNode,
		fixture.callNode,
		fixture.literalNode,
	}
	if len(sections) != len(expectedSyntax)+1 || len(spans) != len(expectedSyntax) {
		t.Fatalf("sections/spans = %d/%d", len(sections), len(spans))
	}
	if sections[0].ID != expectedRootID ||
		sections[0].Range.Start.Offset != 0 ||
		sections[0].Range.End.Offset != int64(len(fixture.content)) {
		t.Fatalf("root section = %+v", sections[0])
	}
	for index, syntax := range expectedSyntax {
		expectedSectionID := stableID(
			t, "shoal.section", string(expectedRevisionID), syntax.ID().String())
		expectedSpanID := stableID(
			t, "shoal.span", string(expectedRevisionID), syntax.ID().String())
		section := sections[index+1]
		span := spans[index]
		if section.ID != expectedSectionID || span.ID != expectedSpanID ||
			span.SectionID != expectedSectionID {
			t.Fatalf("syntax %d identities = section %q span %q",
				index, section.ID, span.ID)
		}
		start := syntax.Range().Start().ByteOffset()
		end := syntax.Range().End().ByteOffset()
		if span.Text != string(fixture.content[start:end]) {
			t.Fatalf("syntax %d exact UTF-8 slice = %q", index, span.Text)
		}
	}
	if spans[4].Text != "\"héllo\"" {
		t.Fatalf("UTF-8 literal span = %q", spans[4].Text)
	}
	if sections[1].Order != 0 ||
		sections[2].Order != 1 ||
		sections[3].Order != 2 ||
		sections[4].Order != 1 ||
		sections[5].Order != 1 {
		t.Fatalf("syntax section orders = %d,%d,%d,%d,%d",
			sections[1].Order, sections[2].Order, sections[3].Order,
			sections[4].Order, sections[5].Order)
	}

	nodes := first.Nodes()
	for _, node := range nodes {
		if _, present := node.Properties["shoal.code.ref"]; present {
			t.Fatalf("source ref leaked into graph node %q", node.ID)
		}
	}
	for _, id := range []shoal.ID{
		shoal.ID(fixture.source.ID().String()),
		shoal.ID(fixture.callNode.ID().String()),
		shoal.ID(fixture.symbol.ID().String()),
		shoal.ID(fixture.module.ID().String()),
		shoal.ID(fixture.callee.ID().String()),
	} {
		if _, found := graphNode(nodes, id); !found {
			t.Fatalf("exact code graph node %q was not materialized", id)
		}
	}
	for _, relationship := range []codeast.Relationship{
		fixture.callRelationship, fixture.importRelationship,
	} {
		edge, found := graphEdge(
			first.Edges(), shoal.ID(relationship.ID().String()))
		if !found {
			t.Fatalf("exact relationship edge %q was not materialized",
				relationship.ID())
		}
		if edge.From != shoal.ID(relationship.From().ID().String()) ||
			edge.To != shoal.ID(relationship.To().ID().String()) ||
			edge.Type != string(relationship.Kind()) {
			t.Fatalf("relationship edge changed = %+v", edge)
		}
	}
	expectedContainsID := stableID(
		t, "shoal.graph-edge", codematerializer.Version, "contains",
		string(expectedDocumentID), string(expectedRootID),
	)
	if edge, found := graphEdge(first.Edges(), expectedContainsID); !found ||
		edge.From != expectedDocumentID || edge.To != expectedRootID {
		t.Fatalf("stable document containment edge = %+v, found %t", edge, found)
	}
	expectedCallSectionID := stableID(
		t, "shoal.section", string(expectedRevisionID),
		fixture.callNode.ID().String(),
	)
	expectedAssociationID := stableID(
		t, "shoal.graph-edge", codematerializer.Version, "associated_with",
		string(expectedCallSectionID), fixture.callNode.ID().String(),
	)
	if edge, found := graphEdge(first.Edges(), expectedAssociationID); !found ||
		edge.From != expectedCallSectionID ||
		edge.To != shoal.ID(fixture.callNode.ID().String()) {
		t.Fatalf("stable syntax association edge = %+v, found %t", edge, found)
	}

	associations := first.Associations()
	if len(associations) != len(expectedSyntax)+1+2 {
		t.Fatalf("association count = %d", len(associations))
	}
	if associationFor(
		associations, codematerializer.AssociationNode,
		shoal.ID(fixture.module.ID().String()),
	) != nil {
		t.Fatal("external entity unexpectedly produced citation evidence")
	}
	symbolAssociation := associationFor(
		associations, codematerializer.AssociationNode,
		shoal.ID(fixture.symbol.ID().String()),
	)
	if symbolAssociation == nil {
		t.Fatal("symbol declaration association missing")
	}
	symbolQuote, err := document.ResolveCitationQuote(
		publicSource.Content, doc, revision, sections, spans,
		symbolAssociation.Citation())
	if err != nil {
		t.Fatalf("symbol quote: %v", err)
	}
	if symbolQuote != "café" {
		t.Fatalf("symbol quote = %q", symbolQuote)
	}
	callAssociation := associationFor(
		associations, codematerializer.AssociationEdge,
		shoal.ID(fixture.callRelationship.ID().String()),
	)
	if callAssociation == nil {
		t.Fatal("ranged relationship association missing")
	}
	callQuote, err := document.ResolveCitationQuote(
		publicSource.Content, doc, revision, sections, spans,
		callAssociation.Citation())
	if err != nil {
		t.Fatalf("call relationship quote: %v", err)
	}
	if callQuote != "mód.Call(\"héllo\")" {
		t.Fatalf("call relationship quote = %q", callQuote)
	}

	artifacts := first.Artifacts()
	if len(artifacts) != 2 ||
		artifacts[0].Kind() != codeast.ArtifactDocument ||
		artifacts[0].Identifier() != expectedDocumentID ||
		artifacts[1].Kind() != codeast.ArtifactGraph ||
		artifacts[1].Identifier() != expectedGraphArtifactID {
		t.Fatalf("artifacts = %#v", artifacts)
	}
	applied, err := first.IngestResult(fixture.ingest, codeast.IngestApplied)
	if err != nil {
		t.Fatalf("applied result: %v", err)
	}
	unchanged, err := second.IngestResult(fixture.ingest, codeast.IngestUnchanged)
	if err != nil {
		t.Fatalf("unchanged result: %v", err)
	}
	if err := codeast.ValidateCommittedRetry(applied, unchanged); err != nil {
		t.Fatalf("retry stability: %v", err)
	}

	fixture.content[0] = 'X'
	publicSource.Metadata["shoal.code.path"] = "mutated"
	nodes[0].Labels[0] = "mutated"
	if first.Source().Metadata["shoal.code.path"] != fixture.source.Path() ||
		first.Source().Content[0] != 'i' ||
		first.Nodes()[0].Labels[0] == "mutated" {
		t.Fatal("materialization retained caller- or getter-owned mutable values")
	}
}

func TestMaterializeIsIdenticalAcrossSourceRefs(t *testing.T) {
	branch := newMaterializerFixtureWithRef(t, "refs/heads/main")
	tag := newMaterializerFixtureWithRef(t, "refs/tags/v1.0.0")
	if branch.source.Ref() == tag.source.Ref() {
		t.Fatal("fixture refs must differ")
	}
	if branch.source.ID() != tag.source.ID() ||
		branch.ingest.IdempotencyKey() != tag.ingest.IdempotencyKey() {
		t.Fatal("source ref unexpectedly changed canonical ingestion identity")
	}

	branchMaterialization, err := codematerializer.Materialize(
		branch.ingest, branch.content)
	if err != nil {
		t.Fatalf("branch materialization: %v", err)
	}
	tagMaterialization, err := codematerializer.Materialize(
		tag.ingest, tag.content)
	if err != nil {
		t.Fatalf("tag materialization: %v", err)
	}
	branchSnapshot := materializationSnapshot(t, branchMaterialization)
	tagSnapshot := materializationSnapshot(t, tagMaterialization)
	if !bytes.Equal(branchSnapshot, tagSnapshot) {
		t.Fatalf("source ref changed canonical public values:\n%s\n%s",
			branchSnapshot, tagSnapshot)
	}
}

func TestMaterializeRejectsMismatchedAndInvalidSourceBytes(t *testing.T) {
	fixture := newMaterializerFixture(t)
	sameLength := append([]byte(nil), fixture.content...)
	sameLength[len(sameLength)-2] = 'x'
	for name, source := range map[string][]byte{
		"length": fixture.content[:len(fixture.content)-1],
		"hash":   sameLength,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := codematerializer.Materialize(
				fixture.ingest, source,
			); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
				t.Fatalf("error = %v", err)
			}
		})
	}

	invalid := []byte{0xff, '\n'}
	invalidIngest := ingestForContent(t, invalid)
	if _, err := codematerializer.Materialize(
		invalidIngest, invalid,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("invalid UTF-8 error = %v", err)
	}
}

func TestMaterializeRejectsSyntaxRangeInsideUTF8Encoding(t *testing.T) {
	content := []byte("é\n")
	source := testSource(t, content)
	parseRequest := mustParseRequest(t, source, content)
	node := mustSyntaxNode(
		t, source, "broken",
		exactRange(t, content, 1, 2),
		0,
	)
	language, parser := languageAndParser(t)
	result, err := codeast.NewParseResult(
		parseRequest, language, parser,
		codeast.WithSyntaxRoots(node),
		codeast.WithSyntaxNodes(node),
	)
	if err != nil {
		t.Fatalf("parse result permits byte coordinates: %v", err)
	}
	ingest, err := codeast.NewIngestRequest(parseRequest, result)
	if err != nil {
		t.Fatalf("ingest request: %v", err)
	}
	if _, err := codematerializer.Materialize(
		ingest, content,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("UTF-8 boundary error = %v", err)
	}
}

func TestMaterializeRejectsIncompatibleCodeProperties(t *testing.T) {
	content := []byte("x\n")
	source := testSource(t, content)
	parseRequest := mustParseRequest(t, source, content)
	node := mustSyntaxNode(
		t, source, "identifier", exactRange(t, content, 0, 1), 0,
		codeast.WithSyntaxAttribute("shoal.code.entity_kind", "collision"),
	)
	language, parser := languageAndParser(t)
	result, err := codeast.NewParseResult(
		parseRequest, language, parser,
		codeast.WithSyntaxRoots(node),
		codeast.WithSyntaxNodes(node),
	)
	if err != nil {
		t.Fatalf("parse result: %v", err)
	}
	ingest, err := codeast.NewIngestRequest(parseRequest, result)
	if err != nil {
		t.Fatalf("ingest request: %v", err)
	}
	if _, err := codematerializer.Materialize(
		ingest, content,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("property compatibility error = %v", err)
	}
}

type materializerFixture struct {
	content            []byte
	source             codeast.Source
	fileNode           codeast.SyntaxNode
	importNode         codeast.SyntaxNode
	functionNode       codeast.SyntaxNode
	callNode           codeast.SyntaxNode
	literalNode        codeast.SyntaxNode
	symbol             codeast.SemanticSymbol
	module             codeast.ExternalEntity
	callee             codeast.ExternalEntity
	importRelationship codeast.Relationship
	callRelationship   codeast.Relationship
	ingest             codeast.IngestRequest
}

func newMaterializerFixture(t *testing.T) materializerFixture {
	t.Helper()
	return newMaterializerFixtureWithRef(t, "refs/heads/main")
}

func newMaterializerFixtureWithRef(
	t *testing.T, ref string,
) materializerFixture {
	t.Helper()
	content := []byte(
		"import \"mód\"\n" +
			"func café() {\n" +
			"\tmód.Call(\"héllo\")\n" +
			"}\n")
	source := testSourceWithRef(t, content, ref)
	parseRequest := mustParseRequest(t, source, content)

	importStart := bytes.Index(content, []byte("import"))
	importEnd := bytes.IndexByte(content, '\n')
	functionStart := bytes.Index(content, []byte("func"))
	callStart := bytes.Index(content, []byte("mód.Call"))
	callEnd := callStart + len([]byte("mód.Call(\"héllo\")"))
	literalStart := bytes.Index(content, []byte("\"héllo\""))
	literalEnd := literalStart + len([]byte("\"héllo\""))
	nameStart := bytes.Index(content, []byte("café"))
	nameEnd := nameStart + len([]byte("café"))
	importNameStart := bytes.Index(content, []byte("mód"))
	importNameEnd := importNameStart + len([]byte("mód"))

	literal := mustSyntaxNode(
		t, source, "string_literal",
		exactRange(t, content, uint64(literalStart), uint64(literalEnd)), 0,
	)
	call := mustSyntaxNode(
		t, source, "call_expression",
		exactRange(t, content, uint64(callStart), uint64(callEnd)), 0,
		codeast.WithSyntaxChildren(literal),
		codeast.WithSyntaxAttribute("role", "invocation"),
	)
	function := mustSyntaxNode(
		t, source, "function_declaration",
		exactRange(t, content, uint64(functionStart), uint64(len(content))), 0,
		codeast.WithSyntaxChildren(call),
	)
	importNode := mustSyntaxNode(
		t, source, "import_declaration",
		exactRange(t, content, uint64(importStart), uint64(importEnd)), 0,
	)
	file := mustSyntaxNode(
		t, source, "file",
		exactRange(t, content, 0, uint64(len(content))), 0,
		codeast.WithSyntaxChildren(importNode, function),
	)
	symbol, err := codeast.NewSemanticSymbol(
		source,
		"function",
		"café",
		exactRange(t, content, uint64(nameStart), uint64(nameEnd)),
		0,
		codeast.WithSymbolQualifiedName("pkg.café"),
		codeast.WithSymbolSyntaxNode(function),
		codeast.WithSymbolAttribute("visibility", "public"),
	)
	if err != nil {
		t.Fatalf("symbol: %v", err)
	}
	module, err := codeast.NewExternalEntity(
		"module", "example/mód",
		codeast.WithExternalAttribute("origin", "fixture"),
	)
	if err != nil {
		t.Fatalf("module external: %v", err)
	}
	callee, err := codeast.NewExternalEntity(
		"function", "example/mód.Call",
		codeast.WithExternalAttribute("package", "mód"),
	)
	if err != nil {
		t.Fatalf("callee external: %v", err)
	}
	importRelationship, err := codeast.NewRelationship(
		codeast.RelationshipImport,
		codeast.SourceEndpoint(source),
		codeast.ExternalEndpoint(module),
		codeast.WithRelationshipRange(exactRange(
			t, content, uint64(importNameStart), uint64(importNameEnd))),
		codeast.WithRelationshipAttribute("form", "direct"),
	)
	if err != nil {
		t.Fatalf("import relationship: %v", err)
	}
	callRelationship, err := codeast.NewRelationship(
		codeast.RelationshipCall,
		codeast.SyntaxEndpoint(call),
		codeast.ExternalEndpoint(callee),
		codeast.WithRelationshipRange(exactRange(
			t, content, uint64(callStart), uint64(callEnd))),
		codeast.WithRelationshipAttribute("dispatch", "static"),
	)
	if err != nil {
		t.Fatalf("call relationship: %v", err)
	}
	language, parser := languageAndParser(t)
	result, err := codeast.NewParseResult(
		parseRequest, language, parser,
		codeast.WithSyntaxRoots(file),
		codeast.WithSyntaxNodes(call, file, literal, importNode, function),
		codeast.WithSemanticSymbols(symbol),
		codeast.WithExternalEntities(callee, module),
		codeast.WithRelationships(callRelationship, importRelationship),
	)
	if err != nil {
		t.Fatalf("parse result: %v", err)
	}
	ingest, err := codeast.NewIngestRequest(parseRequest, result)
	if err != nil {
		t.Fatalf("ingest request: %v", err)
	}
	return materializerFixture{
		content:            content,
		source:             source,
		fileNode:           file,
		importNode:         importNode,
		functionNode:       function,
		callNode:           call,
		literalNode:        literal,
		symbol:             symbol,
		module:             module,
		callee:             callee,
		importRelationship: importRelationship,
		callRelationship:   callRelationship,
		ingest:             ingest,
	}
}

func ingestForContent(t *testing.T, content []byte) codeast.IngestRequest {
	t.Helper()
	source := testSource(t, content)
	parseRequest := mustParseRequest(t, source, content)
	language, parser := languageAndParser(t)
	result, err := codeast.NewParseResult(parseRequest, language, parser)
	if err != nil {
		t.Fatalf("parse result: %v", err)
	}
	ingest, err := codeast.NewIngestRequest(parseRequest, result)
	if err != nil {
		t.Fatalf("ingest request: %v", err)
	}
	return ingest
}

func testSource(t *testing.T, content []byte) codeast.Source {
	t.Helper()
	return testSourceWithRef(t, content, "refs/heads/main")
}

func testSourceWithRef(
	t *testing.T, content []byte, ref string,
) codeast.Source {
	t.Helper()
	repository, err := codeast.NewRepository("https://example.test/acme/repository")
	if err != nil {
		t.Fatalf("repository: %v", err)
	}
	source, err := codeast.NewSource(
		repository,
		ref,
		"pkg/café.go",
		"0123456789abcdef",
		codeast.HashContent(content),
		uint64(len(content)),
	)
	if err != nil {
		t.Fatalf("source: %v", err)
	}
	return source
}

func mustParseRequest(
	t *testing.T, source codeast.Source, content []byte,
) codeast.ParseRequest {
	t.Helper()
	request, err := codeast.NewParseRequest(source, content)
	if err != nil {
		t.Fatalf("parse request: %v", err)
	}
	return request
}

func languageAndParser(
	t *testing.T,
) (codeast.Language, codeast.ParserProvenance) {
	t.Helper()
	language, err := codeast.NewLanguage("go", "1.25", "")
	if err != nil {
		t.Fatalf("language: %v", err)
	}
	parser, err := codeast.NewParserProvenance(
		"fixture-parser", "1.0.0", codeast.HashContent(nil))
	if err != nil {
		t.Fatalf("parser: %v", err)
	}
	return language, parser
}

func mustSyntaxNode(
	t *testing.T,
	source codeast.Source,
	kind string,
	sourceRange codeast.Range,
	occurrence uint32,
	options ...codeast.SyntaxNodeOption,
) codeast.SyntaxNode {
	t.Helper()
	node, err := codeast.NewSyntaxNode(
		source, kind, sourceRange, occurrence, options...)
	if err != nil {
		t.Fatalf("syntax node: %v", err)
	}
	return node
}

func exactRange(
	t *testing.T, content []byte, start, end uint64,
) codeast.Range {
	t.Helper()
	sourceRange, err := codeast.NewRange(
		exactPosition(t, content, start),
		exactPosition(t, content, end),
	)
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	return sourceRange
}

func exactPosition(
	t *testing.T, content []byte, offset uint64,
) codeast.Position {
	t.Helper()
	if offset > uint64(len(content)) {
		t.Fatalf("offset %d exceeds source length", offset)
	}
	line := uint32(1)
	lineStart := uint64(0)
	for index := uint64(0); index < offset; index++ {
		if content[index] == '\n' {
			line++
			lineStart = index + 1
		}
	}
	position, err := codeast.NewPosition(
		offset, line, uint32(offset-lineStart+1))
	if err != nil {
		t.Fatalf("position: %v", err)
	}
	return position
}

func stableID(t *testing.T, namespace string, parts ...string) shoal.ID {
	t.Helper()
	id, err := codeast.NewStableID(namespace, parts...)
	if err != nil {
		t.Fatalf("stable ID: %v", err)
	}
	return shoal.ID(id.String())
}

func graphNode(nodes []graph.Node, id shoal.ID) (graph.Node, bool) {
	for _, node := range nodes {
		if node.ID == id {
			return node, true
		}
	}
	return graph.Node{}, false
}

func graphEdge(edges []graph.Edge, id shoal.ID) (graph.Edge, bool) {
	for _, edge := range edges {
		if edge.ID == id {
			return edge, true
		}
	}
	return graph.Edge{}, false
}

func associationFor(
	associations []codematerializer.Association,
	target codematerializer.AssociationTarget,
	id shoal.ID,
) *codematerializer.Association {
	for index := range associations {
		if associations[index].Target() == target &&
			associations[index].TargetID() == id {
			return &associations[index]
		}
	}
	return nil
}

type associationSnapshot struct {
	Target   codematerializer.AssociationTarget
	TargetID shoal.ID
	Citation document.Citation
}

type artifactSnapshot struct {
	Kind codeast.ArtifactKind
	ID   shoal.ID
}

func materializationSnapshot(
	t *testing.T, materialization codematerializer.Materialization,
) []byte {
	t.Helper()
	associations := materialization.Associations()
	associationValues := make([]associationSnapshot, len(associations))
	for index, association := range associations {
		associationValues[index] = associationSnapshot{
			Target: association.Target(), TargetID: association.TargetID(),
			Citation: association.Citation(),
		}
	}
	artifacts := materialization.Artifacts()
	artifactValues := make([]artifactSnapshot, len(artifacts))
	for index, artifact := range artifacts {
		artifactValues[index] = artifactSnapshot{
			Kind: artifact.Kind(), ID: artifact.Identifier(),
		}
	}
	value := struct {
		Version      string
		Source       any
		Document     document.Document
		Revision     document.Revision
		Sections     []document.Section
		Spans        []document.Span
		Nodes        []graph.Node
		Edges        []graph.Edge
		Associations []associationSnapshot
		Artifacts    []artifactSnapshot
	}{
		Version:      materialization.Version(),
		Source:       materialization.Source(),
		Document:     materialization.Document(),
		Revision:     materialization.Revision(),
		Sections:     materialization.Sections(),
		Spans:        materialization.Spans(),
		Nodes:        materialization.Nodes(),
		Edges:        materialization.Edges(),
		Associations: associationValues,
		Artifacts:    artifactValues,
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return encoded
}
