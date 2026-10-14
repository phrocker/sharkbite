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

package code_test

import (
	"context"
	"strings"
	"testing"

	codeast "github.com/phrocker/shoal-oss/pkg/code"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestCanonicalTypedIDsAndReservedNamespaces(t *testing.T) {
	first, err := codeast.NewStableID("vendor.syntax", "repo", "ab", "c")
	if err != nil {
		t.Fatalf("create extension ID: %v", err)
	}
	again, err := codeast.NewStableID("vendor.syntax", "repo", "ab", "c")
	if err != nil {
		t.Fatalf("repeat extension ID: %v", err)
	}
	if first != again {
		t.Fatalf("stable ID changed: %q != %q", first, again)
	}
	differentBoundaries, err := codeast.NewStableID("vendor.syntax", "repo", "a", "bc")
	if err != nil {
		t.Fatalf("create boundary-safe ID: %v", err)
	}
	if first == differentBoundaries {
		t.Fatal("length-delimited identity components produced the same ID")
	}

	for _, namespace := range []string{
		"source", "syntax", "symbol", "external", "relationship", "ingest",
		"parse-result",
	} {
		t.Run(namespace, func(t *testing.T) {
			if _, err := codeast.NewStableID(namespace, "caller-value"); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
				t.Fatalf("expected reserved namespace rejection, got %v", err)
			}
		})
	}

	fixture := newFixture(t, "commit-1")
	if fixture.root.ID().Namespace() != "syntax" {
		t.Fatalf("unexpected root namespace %q", fixture.root.ID().Namespace())
	}
	sameRoot := syntaxNode(
		t, fixture.source, "file", exactRange(t, fixture.content, 0, 10), 0,
		codeast.WithSyntaxChildren(fixture.child))
	if sameRoot.ID() != fixture.root.ID() {
		t.Fatal("equal typed node inputs produced different canonical IDs")
	}
	otherOccurrence := syntaxNode(
		t, fixture.source, "file", exactRange(t, fixture.content, 0, 10), 1,
		codeast.WithSyntaxChildren(fixture.child))
	if otherOccurrence.ID() == fixture.root.ID() {
		t.Fatal("distinct typed occurrences produced the same canonical ID")
	}
}

func TestRangeContainmentRequiresConsistentCoordinates(t *testing.T) {
	outerStart, err := codeast.NewPosition(0, 10, 1)
	if err != nil {
		t.Fatal(err)
	}
	outerEnd, err := codeast.NewPosition(10, 20, 1)
	if err != nil {
		t.Fatal(err)
	}
	innerStart, err := codeast.NewPosition(1, 5, 1)
	if err != nil {
		t.Fatal(err)
	}
	innerEnd, err := codeast.NewPosition(2, 6, 1)
	if err != nil {
		t.Fatal(err)
	}
	outer, err := codeast.NewRange(outerStart, outerEnd)
	if err != nil {
		t.Fatal(err)
	}
	inner, err := codeast.NewRange(innerStart, innerEnd)
	if err != nil {
		t.Fatal(err)
	}

	if outer.Contains(inner) {
		t.Fatal("range with contradictory boundary coordinates was contained")
	}
}

func TestSourceIdentityIncludesByteLength(t *testing.T) {
	content := []byte("abcdefgh")
	repository := testRepository(t)
	contentHash := codeast.HashContent(content)
	first := sourceWithIdentity(
		t, repository, "commit-1", contentHash, uint64(len(content)))
	differentSize := sourceWithIdentity(
		t, repository, "commit-1", contentHash, uint64(len(content)+1))

	if first.ID() == differentSize.ID() {
		t.Fatal("same hash with a different byte length collided")
	}
	if _, err := codeast.NewParseRequest(differentSize, content); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("expected parse-request size defense, got %v", err)
	}
}

func TestSourceRequiresRevisionAndContentHash(t *testing.T) {
	content := []byte("abcdefgh")
	repository := testRepository(t)
	if _, err := codeast.NewSource(
		repository, "refs/heads/main", "pkg/sample.go", "",
		codeast.HashContent(content), uint64(len(content)),
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("expected missing revision failure, got %v", err)
	}
	if _, err := codeast.NewSource(
		repository, "refs/heads/main", "pkg/sample.go", "commit-1",
		codeast.ContentHash{}, uint64(len(content)),
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("expected missing content hash failure, got %v", err)
	}
}

func TestSourceRejectsRepositoryTraversalPaths(t *testing.T) {
	content := []byte("package sample")
	repository := testRepository(t)
	for _, sourcePath := range []string{
		"..",
		"../outside.go",
		"../../outside.go",
		"C:/outside.go",
		"C:outside.go",
		"z:/outside.go",
	} {
		t.Run(sourcePath, func(t *testing.T) {
			_, err := codeast.NewSource(
				repository, "refs/heads/main", sourcePath, "commit-1",
				codeast.HashContent(content), uint64(len(content)),
			)
			if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
				t.Fatalf("NewSource(%q) error = %v, want invalid argument", sourcePath, err)
			}
		})
	}
}

func TestParseRequestCopiesExactContent(t *testing.T) {
	fixture := newFixture(t, "commit-1")
	if fixture.request.ContentSize() != uint64(len(fixture.content)) {
		t.Fatalf("content size = %d", fixture.request.ContentSize())
	}
	if !fixture.request.ContentEqual(fixture.content) ||
		!fixture.request.ContentStringEqual(string(fixture.content)) {
		t.Fatal("non-copying content comparison rejected exact bytes")
	}
	different := append([]byte(nil), fixture.content...)
	different[0] = 'z'
	if fixture.request.ContentEqual(different) ||
		fixture.request.ContentStringEqual(string(different)) {
		t.Fatal("non-copying content comparison accepted different bytes")
	}
	copied := fixture.request.Content()
	copied[0] = 'z'
	if fixture.request.Content()[0] != fixture.content[0] {
		t.Fatal("parse request exposed mutable content")
	}
}

func TestValidateForRejectsEveryIncorrectCoordinateCategory(t *testing.T) {
	tests := map[string]func(*testing.T, fixture) error{
		"syntax node": func(t *testing.T, fixture fixture) error {
			badChild := syntaxNode(
				t, fixture.source, "function", wrongCoordinateRange(t, 3, 9), 0)
			root := syntaxNode(
				t, fixture.source, "file", exactRange(t, fixture.content, 0, 10), 0,
				codeast.WithSyntaxChildren(badChild))
			_, err := codeast.NewParseResult(
				fixture.request, fixture.language, fixture.parser,
				codeast.WithSyntaxRoots(root),
				codeast.WithSyntaxNodes(root, badChild),
			)
			return err
		},
		"semantic symbol": func(t *testing.T, fixture fixture) error {
			badSymbol := semanticSymbol(
				t, fixture.source, "function", "sample",
				wrongCoordinateRange(t, 3, 5), 0,
				codeast.WithSymbolQualifiedName("sample"),
				codeast.WithSymbolSyntaxNode(fixture.child))
			_, err := codeast.NewParseResult(
				fixture.request, fixture.language, fixture.parser,
				codeast.WithSyntaxRoots(fixture.root),
				codeast.WithSyntaxNodes(fixture.root, fixture.child),
				codeast.WithSemanticSymbols(badSymbol),
			)
			return err
		},
		"relationship": func(t *testing.T, fixture fixture) error {
			badRelationship := relationship(
				t, codeast.RelationshipImport,
				codeast.SyntaxEndpoint(fixture.child),
				codeast.ExternalEndpoint(fixture.external),
				codeast.WithRelationshipRange(wrongCoordinateRange(t, 5, 7)))
			_, err := codeast.NewParseResult(
				fixture.request, fixture.language, fixture.parser,
				codeast.WithSyntaxRoots(fixture.root),
				codeast.WithSyntaxNodes(fixture.root, fixture.child),
				codeast.WithExternalEntities(fixture.external),
				codeast.WithRelationships(badRelationship),
			)
			return err
		},
		"diagnostic": func(t *testing.T, fixture fixture) error {
			badDiagnostic := diagnostic(
				t, codeast.DiagnosticWarning, "bad coordinate",
				codeast.WithDiagnosticRange(wrongCoordinateRange(t, 7, 8)))
			_, err := codeast.NewParseResult(
				fixture.request, fixture.language, fixture.parser,
				codeast.WithSyntaxRoots(fixture.root),
				codeast.WithSyntaxNodes(fixture.root, fixture.child),
				codeast.WithDiagnostics(badDiagnostic),
			)
			return err
		},
	}

	for name, run := range tests {
		t.Run(name, func(t *testing.T) {
			err := run(t, newFixture(t, "commit-1"))
			requireInvalid(t, err)
			if !strings.Contains(err.Error(), "coordinates") {
				t.Fatalf("expected coordinate failure, got %v", err)
			}
		})
	}
}

func TestExactCoordinateValidationPrecedesChildOrdering(t *testing.T) {
	fixture := newFixture(t, "commit-1")
	first := syntaxNode(
		t, fixture.source, "first", exactRange(t, fixture.content, 3, 5), 0)
	second := syntaxNode(
		t, fixture.source, "second", wrongCoordinateRange(t, 7, 9), 0)
	root := syntaxNode(
		t, fixture.source, "file", exactRange(t, fixture.content, 0, 10), 0,
		codeast.WithSyntaxChildren(second, first))

	_, err := codeast.NewParseResult(
		fixture.request, fixture.language, fixture.parser,
		codeast.WithSyntaxRoots(root),
		codeast.WithSyntaxNodes(root, first, second),
	)
	requireInvalid(t, err)
	if !strings.Contains(err.Error(), "coordinates") {
		t.Fatalf("expected coordinates before ordering, got %v", err)
	}
}

func TestParseResultValidatesRangeContainment(t *testing.T) {
	fixture := newFixture(t, "commit-1")
	child := syntaxNode(
		t, fixture.source, "outside", exactRange(t, fixture.content, 8, 9), 0)
	root := syntaxNode(
		t, fixture.source, "file", exactRange(t, fixture.content, 0, 8), 0,
		codeast.WithSyntaxChildren(child))
	_, err := codeast.NewParseResult(
		fixture.request, fixture.language, fixture.parser,
		codeast.WithSyntaxRoots(root),
		codeast.WithSyntaxNodes(root, child),
	)
	requireInvalid(t, err)
}

func TestParseResultValidatesChildOrdering(t *testing.T) {
	fixture := newFixture(t, "commit-1")
	first := syntaxNode(
		t, fixture.source, "first", exactRange(t, fixture.content, 3, 5), 0)
	second := syntaxNode(
		t, fixture.source, "second", exactRange(t, fixture.content, 7, 9), 0)
	root := syntaxNode(
		t, fixture.source, "file", exactRange(t, fixture.content, 0, 10), 0,
		codeast.WithSyntaxChildren(second, first))
	_, err := codeast.NewParseResult(
		fixture.request, fixture.language, fixture.parser,
		codeast.WithSyntaxRoots(root),
		codeast.WithSyntaxNodes(root, first, second),
	)
	requireInvalid(t, err)
}

func TestSyntaxOccurrencesAreCanonicalContiguousAndZeroBased(t *testing.T) {
	fixture := newFixture(t, "commit-1")
	nodeRange := exactRange(t, fixture.content, 3, 9)
	occurrence0 := syntaxNode(
		t, fixture.source, "wrapper", nodeRange, 0)
	occurrence1 := syntaxNode(
		t, fixture.source, "wrapper", nodeRange, 1)
	occurrence2 := syntaxNode(
		t, fixture.source, "wrapper", nodeRange, 2)

	t.Run("canonical", func(t *testing.T) {
		root := syntaxNode(
			t, fixture.source, "file", exactRange(t, fixture.content, 0, 10), 0,
			codeast.WithSyntaxChildren(occurrence0, occurrence1))
		if _, err := codeast.NewParseResult(
			fixture.request, fixture.language, fixture.parser,
			codeast.WithSyntaxRoots(root),
			codeast.WithSyntaxNodes(root, occurrence0, occurrence1),
		); err != nil {
			t.Fatalf("expected canonical occurrences: %v", err)
		}
	})

	t.Run("reversed", func(t *testing.T) {
		root := syntaxNode(
			t, fixture.source, "file", exactRange(t, fixture.content, 0, 10), 0,
			codeast.WithSyntaxChildren(occurrence1, occurrence0))
		_, err := codeast.NewParseResult(
			fixture.request, fixture.language, fixture.parser,
			codeast.WithSyntaxRoots(root),
			codeast.WithSyntaxNodes(root, occurrence0, occurrence1),
		)
		requireInvalid(t, err)
	})

	t.Run("duplicate", func(t *testing.T) {
		duplicate0 := syntaxNode(
			t, fixture.source, "wrapper", nodeRange, 0)
		root := syntaxNode(
			t, fixture.source, "file", exactRange(t, fixture.content, 0, 10), 0,
			codeast.WithSyntaxChildren(occurrence0))
		_, err := codeast.NewParseResult(
			fixture.request, fixture.language, fixture.parser,
			codeast.WithSyntaxRoots(root),
			codeast.WithSyntaxNodes(root, occurrence0, duplicate0),
		)
		requireInvalid(t, err)
	})

	t.Run("gapped", func(t *testing.T) {
		root := syntaxNode(
			t, fixture.source, "file", exactRange(t, fixture.content, 0, 10), 0,
			codeast.WithSyntaxChildren(occurrence0, occurrence2))
		_, err := codeast.NewParseResult(
			fixture.request, fixture.language, fixture.parser,
			codeast.WithSyntaxRoots(root),
			codeast.WithSyntaxNodes(root, occurrence0, occurrence2),
		)
		requireInvalid(t, err)
	})
}

func TestEqualRangeParentChildOccurrencesFollowDeclaredPreorder(t *testing.T) {
	fixture := newFixture(t, "commit-1")
	nodeRange := exactRange(t, fixture.content, 3, 9)
	build := func(parent, child codeast.SyntaxNode) error {
		_, err := codeast.NewParseResult(
			fixture.request, fixture.language, fixture.parser,
			codeast.WithSyntaxRoots(parent),
			codeast.WithSyntaxNodes(parent, child),
		)
		return err
	}

	t.Run("valid preorder", func(t *testing.T) {
		child := syntaxNode(
			t, fixture.source, "wrapper", nodeRange, 1)
		parent := syntaxNode(
			t, fixture.source, "wrapper", nodeRange, 0,
			codeast.WithSyntaxChildren(child))
		if err := build(parent, child); err != nil {
			t.Fatalf("expected valid parent/child preorder: %v", err)
		}
	})

	t.Run("reversed", func(t *testing.T) {
		child := syntaxNode(
			t, fixture.source, "wrapper", nodeRange, 0)
		parent := syntaxNode(
			t, fixture.source, "wrapper", nodeRange, 1,
			codeast.WithSyntaxChildren(child))
		err := build(parent, child)
		requireInvalid(t, err)
		if !strings.Contains(err.Error(), "declared preorder") {
			t.Fatalf("expected preorder occurrence failure, got %v", err)
		}
	})

	t.Run("duplicate", func(t *testing.T) {
		child := syntaxNode(
			t, fixture.source, "wrapper", nodeRange, 0)
		parent := syntaxNode(
			t, fixture.source, "wrapper", nodeRange, 0,
			codeast.WithSyntaxChildren(child))
		requireInvalid(t, build(parent, child))
	})

	t.Run("gap", func(t *testing.T) {
		child := syntaxNode(
			t, fixture.source, "wrapper", nodeRange, 2)
		parent := syntaxNode(
			t, fixture.source, "wrapper", nodeRange, 0,
			codeast.WithSyntaxChildren(child))
		err := build(parent, child)
		requireInvalid(t, err)
		if !strings.Contains(err.Error(), "declared preorder") {
			t.Fatalf("expected preorder occurrence failure, got %v", err)
		}
	})
}

func TestSemanticSymbolOccurrencesAreCanonicalContiguousAndZeroBased(t *testing.T) {
	fixture := newFixture(t, "commit-1")
	definition := exactRange(t, fixture.content, 3, 5)
	symbol := func(name string, occurrence uint32) codeast.SemanticSymbol {
		return semanticSymbol(
			t, fixture.source, "function", name, definition, occurrence,
			codeast.WithSymbolQualifiedName(name),
			codeast.WithSymbolSyntaxNode(fixture.child))
	}
	build := func(symbols ...codeast.SemanticSymbol) error {
		_, err := codeast.NewParseResult(
			fixture.request, fixture.language, fixture.parser,
			codeast.WithSyntaxRoots(fixture.root),
			codeast.WithSyntaxNodes(fixture.root, fixture.child),
			codeast.WithSemanticSymbols(symbols...),
		)
		return err
	}

	occurrence0 := symbol("zeta", 0)
	occurrence1 := symbol("alpha", 1)
	occurrence2 := symbol("beta", 2)

	t.Run("canonical", func(t *testing.T) {
		if err := build(occurrence0, occurrence1); err != nil {
			t.Fatalf("expected canonical occurrences: %v", err)
		}
	})
	t.Run("reversed", func(t *testing.T) {
		requireInvalid(t, build(occurrence1, occurrence0))
	})
	t.Run("duplicate", func(t *testing.T) {
		requireInvalid(t, build(occurrence0, symbol("other", 0)))
	})
	t.Run("gapped", func(t *testing.T) {
		requireInvalid(t, build(occurrence0, occurrence2))
	})
}

func TestIdenticalRangeOrderingUsesKindAndOccurrenceNotIDHash(t *testing.T) {
	fixture := newFixture(t, "commit-1")
	candidates := []string{
		"kind00", "kind01", "kind02", "kind03", "kind04",
		"kind05", "kind06", "kind07", "kind08", "kind09",
	}
	nodeRange := exactRange(t, fixture.content, 3, 9)
	var firstNode, secondNode codeast.SyntaxNode
	for left := 0; left < len(candidates) && firstNode.ID().String() == ""; left++ {
		for right := left + 1; right < len(candidates); right++ {
			leftNode := syntaxNode(
				t, fixture.source, candidates[left], nodeRange, 0)
			rightNode := syntaxNode(
				t, fixture.source, candidates[right], nodeRange, 0)
			if leftNode.ID().String() > rightNode.ID().String() {
				firstNode, secondNode = leftNode, rightNode
				break
			}
		}
	}
	if firstNode.ID().String() == "" {
		t.Fatal("test candidates did not produce an inverse ID hash order")
	}
	root := syntaxNode(
		t, fixture.source, "file", exactRange(t, fixture.content, 0, 10), 0,
		codeast.WithSyntaxChildren(firstNode, secondNode))
	if _, err := codeast.NewParseResult(
		fixture.request, fixture.language, fixture.parser,
		codeast.WithSyntaxRoots(root),
		codeast.WithSyntaxNodes(root, firstNode, secondNode),
	); err != nil {
		t.Fatalf("syntax ordering followed ID hash instead of semantic tuple: %v", err)
	}

	definition := exactRange(t, fixture.content, 3, 5)
	var firstSymbol, secondSymbol codeast.SemanticSymbol
	for left := 0; left < len(candidates) && firstSymbol.ID().String() == ""; left++ {
		for right := left + 1; right < len(candidates); right++ {
			leftSymbol := semanticSymbol(
				t, fixture.source, candidates[left], "left", definition, 0,
				codeast.WithSymbolSyntaxNode(fixture.child))
			rightSymbol := semanticSymbol(
				t, fixture.source, candidates[right], "right", definition, 0,
				codeast.WithSymbolSyntaxNode(fixture.child))
			if leftSymbol.ID().String() > rightSymbol.ID().String() {
				firstSymbol, secondSymbol = leftSymbol, rightSymbol
				break
			}
		}
	}
	if firstSymbol.ID().String() == "" {
		t.Fatal("test candidates did not produce an inverse symbol ID hash order")
	}
	if _, err := codeast.NewParseResult(
		fixture.request, fixture.language, fixture.parser,
		codeast.WithSyntaxRoots(fixture.root),
		codeast.WithSyntaxNodes(fixture.root, fixture.child),
		codeast.WithSemanticSymbols(firstSymbol, secondSymbol),
	); err != nil {
		t.Fatalf("symbol ordering followed ID hash instead of semantic tuple: %v", err)
	}
}

func TestRelationshipKindsAndEndpointMatrixAreStrict(t *testing.T) {
	fixture := newFixture(t, "commit-1")
	source := codeast.SourceEndpoint(fixture.source)
	syntax := codeast.SyntaxEndpoint(fixture.child)
	symbol := codeast.SymbolEndpoint(fixture.symbol)
	external := codeast.ExternalEndpoint(fixture.external)

	valid := []struct {
		name string
		kind codeast.RelationshipKind
		from codeast.Endpoint
		to   codeast.Endpoint
	}{
		{"import source external", codeast.RelationshipImport, source, external},
		{"import syntax external", codeast.RelationshipImport, syntax, external},
		{"call syntax symbol", codeast.RelationshipCall, syntax, symbol},
		{"call syntax external", codeast.RelationshipCall, syntax, external},
		{"call symbol symbol", codeast.RelationshipCall, symbol, symbol},
		{"call symbol external", codeast.RelationshipCall, symbol, external},
		{"reference syntax symbol", codeast.RelationshipReference, syntax, symbol},
		{"reference syntax external", codeast.RelationshipReference, syntax, external},
		{"reference symbol symbol", codeast.RelationshipReference, symbol, symbol},
		{"reference symbol external", codeast.RelationshipReference, symbol, external},
		{"contains source syntax", codeast.RelationshipContains, source, syntax},
		{"contains source symbol", codeast.RelationshipContains, source, symbol},
		{"contains syntax syntax", codeast.RelationshipContains, syntax, syntax},
		{"contains syntax symbol", codeast.RelationshipContains, syntax, symbol},
		{"contains symbol syntax", codeast.RelationshipContains, symbol, syntax},
		{"contains symbol symbol", codeast.RelationshipContains, symbol, symbol},
	}
	for _, test := range valid {
		t.Run("valid "+test.name, func(t *testing.T) {
			relationship, err := codeast.NewRelationship(test.kind, test.from, test.to)
			if err != nil {
				t.Fatalf("construct relationship: %v", err)
			}
			if _, err := codeast.NewParseResult(
				fixture.request, fixture.language, fixture.parser,
				codeast.WithSyntaxRoots(fixture.root),
				codeast.WithSyntaxNodes(fixture.root, fixture.child),
				codeast.WithSemanticSymbols(fixture.symbol),
				codeast.WithExternalEntities(fixture.external),
				codeast.WithRelationships(relationship),
			); err != nil {
				t.Fatalf("represent relationship in parse result: %v", err)
			}
		})
	}

	invalid := []struct {
		name string
		kind codeast.RelationshipKind
		from codeast.Endpoint
		to   codeast.Endpoint
	}{
		{"custom kind", codeast.RelationshipKind("custom"), source, external},
		{"import from symbol", codeast.RelationshipImport, symbol, external},
		{"import to symbol", codeast.RelationshipImport, syntax, symbol},
		{"import to source from source", codeast.RelationshipImport, source, source},
		{"import to source from syntax", codeast.RelationshipImport, syntax, source},
		{"call from source", codeast.RelationshipCall, source, symbol},
		{"call to source", codeast.RelationshipCall, syntax, source},
		{"reference from source", codeast.RelationshipReference, source, symbol},
		{"reference to source", codeast.RelationshipReference, symbol, source},
		{"contains from external", codeast.RelationshipContains, external, syntax},
		{"contains to external", codeast.RelationshipContains, source, external},
	}
	for _, test := range invalid {
		t.Run("invalid "+test.name, func(t *testing.T) {
			_, err := codeast.NewRelationship(test.kind, test.from, test.to)
			requireInvalid(t, err)
		})
	}
}

func TestParseResultRejectsMissingTypedEndpoint(t *testing.T) {
	fixture := newFixture(t, "commit-1")
	missing := externalEntity(t, "module", "example.test/missing")
	importRelationship := relationship(
		t, codeast.RelationshipImport,
		codeast.SyntaxEndpoint(fixture.child),
		codeast.ExternalEndpoint(missing))

	_, err := codeast.NewParseResult(
		fixture.request, fixture.language, fixture.parser,
		codeast.WithSyntaxRoots(fixture.root),
		codeast.WithSyntaxNodes(fixture.root, fixture.child),
		codeast.WithRelationships(importRelationship),
	)
	requireInvalid(t, err)
}

func TestIngestionIsBoundToParseRequestAndResult(t *testing.T) {
	fixture := newFixture(t, "commit-1")
	first, err := codeast.NewIngestRequest(fixture.request, fixture.result)
	if err != nil {
		t.Fatalf("create ingest request: %v", err)
	}
	if first.Source() != fixture.source ||
		first.ContentSize() != uint64(len(fixture.content)) ||
		!first.ContentEqual(fixture.content) ||
		!first.ContentStringEqual(string(fixture.content)) {
		t.Fatal("ingest request non-copying source/content accessors changed exact input")
	}
	counts := first.ParseResultCounts()
	if counts.SyntaxNodes != 2 ||
		counts.Symbols != 1 ||
		counts.Externals != 1 ||
		counts.Relationships != 1 ||
		counts.DeclaredSymbols != 1 ||
		counts.RangedRelationships != 1 {
		t.Fatalf("parse result counts = %+v", counts)
	}

	again, err := codeast.NewIngestRequest(fixture.request, fixture.result)
	if err != nil {
		t.Fatalf("create repeated ingest request: %v", err)
	}
	if first.IdempotencyKey() != again.IdempotencyKey() {
		t.Fatal("identical bound parse values produced different idempotency keys")
	}

	changedDiagnostic := diagnostic(
		t, codeast.DiagnosticHint, "different result payload",
		codeast.WithDiagnosticCode("example"),
		codeast.WithDiagnosticRange(exactRange(t, fixture.content, 7, 8)))
	changedResult, err := codeast.NewParseResult(
		fixture.request, fixture.language, fixture.parser,
		codeast.WithSyntaxRoots(fixture.root),
		codeast.WithSyntaxNodes(fixture.root, fixture.child),
		codeast.WithSemanticSymbols(fixture.symbol),
		codeast.WithExternalEntities(fixture.external),
		codeast.WithRelationships(fixture.relationship),
		codeast.WithDiagnostics(changedDiagnostic),
	)
	if err != nil {
		t.Fatalf("create changed parse result: %v", err)
	}
	changed, err := codeast.NewIngestRequest(fixture.request, changedResult)
	if err != nil {
		t.Fatalf("create changed ingest request: %v", err)
	}
	if first.IdempotencyKey() == changed.IdempotencyKey() {
		t.Fatal("different validated parse results produced the same idempotency key")
	}

	other := newFixture(t, "commit-2")
	if _, err := codeast.NewIngestRequest(other.request, fixture.result); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("expected request/result mismatch, got %v", err)
	}

	copied := first.ParseRequest().Content()
	copied[0] = 'z'
	if err := first.Validate(); err != nil {
		t.Fatalf("ingest request retained mutable parse bytes: %v", err)
	}

	documentRef, err := codeast.NewArtifactRef(codeast.ArtifactDocument, "document-1")
	if err != nil {
		t.Fatalf("create document artifact: %v", err)
	}
	graphRef, err := codeast.NewArtifactRef(codeast.ArtifactGraph, "graph-1")
	if err != nil {
		t.Fatalf("create graph artifact: %v", err)
	}
	ingestResult, err := codeast.NewIngestResult(
		first, codeast.IngestApplied, []codeast.ArtifactRef{documentRef, graphRef})
	if err != nil {
		t.Fatalf("create ingest result: %v", err)
	}
	if err := ingestResult.ValidateFor(first); err != nil {
		t.Fatalf("validate ingest result: %v", err)
	}
}

func TestSourceRefIsExcludedFromSourceAndIngestionIdentity(t *testing.T) {
	content := []byte("package sample\n")
	repository := testRepository(t)
	hash := codeast.HashContent(content)
	mainSource, err := codeast.NewSource(
		repository, "refs/heads/main", "pkg/sample.go", "commit-1",
		hash, uint64(len(content)),
	)
	if err != nil {
		t.Fatal(err)
	}

	tagSource, err := codeast.NewSource(
		repository, "refs/tags/v1", "pkg/sample.go", "commit-1",
		hash, uint64(len(content)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if mainSource.ID() != tagSource.ID() {
		t.Fatal("Source.Ref changed immutable source identity")
	}
	language, err := codeast.NewLanguage("go", "1.25", "")
	if err != nil {
		t.Fatal(err)
	}
	parser, err := codeast.NewParserProvenance(
		"test-parser", "1.0.0", codeast.HashContent(nil))
	if err != nil {
		t.Fatal(err)
	}
	keyFor := func(source codeast.Source) codeast.ID {
		request, requestErr := codeast.NewParseRequest(source, content)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		result, resultErr := codeast.NewParseResult(request, language, parser)
		if resultErr != nil {
			t.Fatal(resultErr)
		}
		ingest, ingestErr := codeast.NewIngestRequest(request, result)
		if ingestErr != nil {
			t.Fatal(ingestErr)
		}
		return ingest.IdempotencyKey()
	}
	if keyFor(mainSource) != keyFor(tagSource) {
		t.Fatal("Source.Ref changed ingestion idempotency key")
	}
}

func TestArtifactRefPreservesOpaqueIdentifierBytes(t *testing.T) {
	identifier := shoal.ID(" \x00artifact ")
	reference, err := codeast.NewArtifactRef(codeast.ArtifactDocument, identifier)
	if err != nil {
		t.Fatalf("opaque artifact identifier: %v", err)
	}
	if reference.Identifier() != identifier {
		t.Fatalf("identifier = %q, want exact %q", reference.Identifier(), identifier)
	}
	if _, err := codeast.NewArtifactRef(
		codeast.ArtifactDocument,
		shoal.ID(strings.Repeat("x", shoal.MaxIDBytes+1)),
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("oversized artifact identifier error = %v", err)
	}
}

func TestCommittedRetryKeepsArtifactIDsAcrossMaterializerVersions(t *testing.T) {
	fixture := newFixture(t, "commit-1")
	request, err := codeast.NewIngestRequest(fixture.request, fixture.result)
	if err != nil {
		t.Fatal(err)
	}
	documentRef, err := codeast.NewArtifactRef(codeast.ArtifactDocument, "document-1")
	if err != nil {
		t.Fatal(err)
	}
	graphRef, err := codeast.NewArtifactRef(codeast.ArtifactGraph, "graph-1")
	if err != nil {
		t.Fatal(err)
	}
	committed, err := codeast.NewIngestResult(
		request, codeast.IngestApplied, []codeast.ArtifactRef{documentRef, graphRef})
	if err != nil {
		t.Fatal(err)
	}
	retry, err := codeast.NewIngestResult(
		request, codeast.IngestUnchanged, []codeast.ArtifactRef{documentRef, graphRef})
	if err != nil {
		t.Fatal(err)
	}
	if err := codeast.ValidateCommittedRetry(committed, retry); err != nil {
		t.Fatalf("identical retry: %v", err)
	}

	reinterpretedRef, err := codeast.NewArtifactRef(
		codeast.ArtifactDocument, "materializer-v2-document")
	if err != nil {
		t.Fatal(err)
	}
	reinterpreted, err := codeast.NewIngestResult(
		request, codeast.IngestUnchanged,
		[]codeast.ArtifactRef{reinterpretedRef, graphRef},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := codeast.ValidateCommittedRetry(
		committed, reinterpreted,
	); !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("reinterpreted retry error = %v", err)
	}
}

func TestSyntaxNodeCopiesOptionValuesAndAccessors(t *testing.T) {
	fixture := newFixture(t, "commit-1")
	node := syntaxNode(
		t, fixture.source, "parent", exactRange(t, fixture.content, 0, 10), 0,
		codeast.WithSyntaxChildren(fixture.child),
		codeast.WithSyntaxAttribute("role", "declaration"))

	returnedChildren := node.Children()
	returnedChildren[0] = fixture.root.ID()
	returnedAttributes := node.Attributes()
	returnedAttributes["role"] = "changed"
	if node.Children()[0] != fixture.child.ID() {
		t.Fatal("syntax node returned a mutable child slice")
	}
	if node.Attributes()["role"] != "declaration" {
		t.Fatal("syntax node returned a mutable attribute map")
	}
}

type fixture struct {
	content      []byte
	source       codeast.Source
	request      codeast.ParseRequest
	language     codeast.Language
	parser       codeast.ParserProvenance
	root         codeast.SyntaxNode
	child        codeast.SyntaxNode
	symbol       codeast.SemanticSymbol
	external     codeast.ExternalEntity
	relationship codeast.Relationship
	diagnostic   codeast.Diagnostic
	result       codeast.ParseResult
}

func newFixture(t *testing.T, revision string) fixture {
	t.Helper()
	content := []byte("ab\ncdéfg\n")
	source := testSource(t, content, revision)
	request, err := codeast.NewParseRequest(source, content)
	if err != nil {
		t.Fatalf("create parse request: %v", err)
	}
	language, err := codeast.NewLanguage("go", "1.25", "")
	if err != nil {
		t.Fatalf("create language: %v", err)
	}
	parser, err := codeast.NewParserProvenance(
		"test-parser", "1.0.0", codeast.HashContent(nil))
	if err != nil {
		t.Fatalf("create parser provenance: %v", err)
	}
	child := syntaxNode(
		t, source, "function", exactRange(t, content, 3, 9), 0,
		codeast.WithSyntaxAttribute("role", "declaration"))
	root := syntaxNode(
		t, source, "file", exactRange(t, content, 0, 10), 0,
		codeast.WithSyntaxChildren(child))
	symbol := semanticSymbol(
		t, source, "function", "sample", exactRange(t, content, 3, 5), 0,
		codeast.WithSymbolQualifiedName("sample"),
		codeast.WithSymbolSyntaxNode(child))
	external := externalEntity(t, "module", "example.test/module")
	relationship := relationship(
		t, codeast.RelationshipImport,
		codeast.SyntaxEndpoint(child),
		codeast.ExternalEndpoint(external),
		codeast.WithRelationshipRange(exactRange(t, content, 5, 7)))
	diagnostic := diagnostic(
		t, codeast.DiagnosticHint, "example",
		codeast.WithDiagnosticCode("example"),
		codeast.WithDiagnosticRange(exactRange(t, content, 7, 8)))
	result, err := codeast.NewParseResult(
		request, language, parser,
		codeast.WithSyntaxRoots(root),
		codeast.WithSyntaxNodes(root, child),
		codeast.WithSemanticSymbols(symbol),
		codeast.WithExternalEntities(external),
		codeast.WithRelationships(relationship),
		codeast.WithDiagnostics(diagnostic),
	)
	if err != nil {
		t.Fatalf("create parse result: %v", err)
	}
	return fixture{
		content:      content,
		source:       source,
		request:      request,
		language:     language,
		parser:       parser,
		root:         root,
		child:        child,
		symbol:       symbol,
		external:     external,
		relationship: relationship,
		diagnostic:   diagnostic,
		result:       result,
	}
}

type parserStub struct {
	result codeast.ParseResult
}

func (p parserStub) Parse(
	_ context.Context, _ codeast.ParseRequest) (codeast.ParseResult, error) {
	return p.result, nil
}

var _ codeast.Parse = parserStub{}

func testRepository(t *testing.T) codeast.Repository {
	t.Helper()
	repository, err := codeast.NewRepository("https://example.test/acme/repository")
	if err != nil {
		t.Fatalf("create repository: %v", err)
	}
	return repository
}

func testSource(t *testing.T, content []byte, revision string) codeast.Source {
	t.Helper()
	return sourceWithIdentity(
		t, testRepository(t), revision, codeast.HashContent(content), uint64(len(content)))
}

func sourceWithIdentity(t *testing.T, repository codeast.Repository, revision string,
	contentHash codeast.ContentHash, size uint64) codeast.Source {
	t.Helper()
	source, err := codeast.NewSource(
		repository,
		"refs/heads/main",
		"pkg/sample.go",
		revision,
		contentHash,
		size,
	)
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	return source
}

func syntaxNode(t *testing.T, source codeast.Source, kind string,
	sourceRange codeast.Range, occurrence uint32,
	options ...codeast.SyntaxNodeOption) codeast.SyntaxNode {
	t.Helper()
	node, err := codeast.NewSyntaxNode(source, kind, sourceRange, occurrence, options...)
	if err != nil {
		t.Fatalf("create syntax node: %v", err)
	}
	return node
}

func semanticSymbol(t *testing.T, source codeast.Source, kind, name string,
	definition codeast.Range, occurrence uint32,
	options ...codeast.SemanticSymbolOption) codeast.SemanticSymbol {
	t.Helper()
	symbol, err := codeast.NewSemanticSymbol(
		source, kind, name, definition, occurrence, options...)
	if err != nil {
		t.Fatalf("create semantic symbol: %v", err)
	}
	return symbol
}

func externalEntity(
	t *testing.T, kind, canonicalName string) codeast.ExternalEntity {
	t.Helper()
	external, err := codeast.NewExternalEntity(kind, canonicalName)
	if err != nil {
		t.Fatalf("create external entity: %v", err)
	}
	return external
}

func relationship(t *testing.T, kind codeast.RelationshipKind,
	from, to codeast.Endpoint,
	options ...codeast.RelationshipOption) codeast.Relationship {
	t.Helper()
	relationship, err := codeast.NewRelationship(kind, from, to, options...)
	if err != nil {
		t.Fatalf("create relationship: %v", err)
	}
	return relationship
}

func diagnostic(t *testing.T, severity codeast.DiagnosticSeverity, message string,
	options ...codeast.DiagnosticOption) codeast.Diagnostic {
	t.Helper()
	diagnostic, err := codeast.NewDiagnostic(severity, message, options...)
	if err != nil {
		t.Fatalf("create diagnostic: %v", err)
	}
	return diagnostic
}

func exactRange(t *testing.T, content []byte, start, end uint64) codeast.Range {
	t.Helper()
	startPosition := exactPosition(t, content, start)
	endPosition := exactPosition(t, content, end)
	sourceRange, err := codeast.NewRange(startPosition, endPosition)
	if err != nil {
		t.Fatalf("create source range: %v", err)
	}
	return sourceRange
}

func exactPosition(t *testing.T, content []byte, offset uint64) codeast.Position {
	t.Helper()
	if offset > uint64(len(content)) {
		t.Fatalf("test offset %d exceeds content", offset)
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
		t.Fatalf("create exact position: %v", err)
	}
	return position
}

func wrongCoordinateRange(t *testing.T, start, end uint64) codeast.Range {
	t.Helper()
	startPosition, err := codeast.NewPosition(start, 2, uint32(start-3+2))
	if err != nil {
		t.Fatalf("create wrong start: %v", err)
	}
	endPosition, err := codeast.NewPosition(end, 2, uint32(end-3+2))
	if err != nil {
		t.Fatalf("create wrong end: %v", err)
	}
	sourceRange, err := codeast.NewRange(startPosition, endPosition)
	if err != nil {
		t.Fatalf("create wrong-coordinate range: %v", err)
	}
	return sourceRange
}

func requireInvalid(t *testing.T, err error) {
	t.Helper()
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("expected invalid argument, got %v", err)
	}
}
