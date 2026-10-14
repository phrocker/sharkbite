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

package document_test

import (
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestValidateRevisionContentRejectsExcessiveLineFragmentation(t *testing.T) {
	source := strings.Repeat("\n", document.MaxSourceLinesPerRevision+1)
	if err := document.ValidateRevisionContent(
		source,
		document.Document{},
		document.Revision{},
		nil,
		nil,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("fragmentation error = %v", err)
	}
}

func TestCitationRequiresRevisionSpecificSource(t *testing.T) {
	citation := document.Citation{
		DocumentID: "doc-1",
		RevisionID: "rev-2",
		SectionID:  "section-3",
		Range: document.SourceRange{
			Start: document.SourcePosition{Offset: 12, Page: 2},
			End:   document.SourcePosition{Offset: 24, Page: 2},
		},
	}
	if err := citation.Validate(); err != nil {
		t.Fatalf("expected valid citation: %v", err)
	}

	citation.RevisionID = ""
	if err := citation.Validate(); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("expected invalid argument, got %v", err)
	}
}

func TestSourceRangeUsesUTF8ByteBoundaries(t *testing.T) {
	source := "aé中"
	valid := []document.SourceRange{
		sourceRange(0, 0),
		sourceRange(1, 3),
		sourceRange(3, int64(len(source))),
		sourceRange(int64(len(source)), int64(len(source))),
	}
	for _, sourceRange := range valid {
		if err := sourceRange.ValidateSource(source); err != nil {
			t.Fatalf("valid range %+v: %v", sourceRange, err)
		}
	}
	for _, sourceRange := range []document.SourceRange{
		sourceRange(2, 3),
		sourceRange(1, 2),
		sourceRange(0, int64(len(source)+1)),
	} {
		if err := sourceRange.ValidateSource(source); !shoal.IsErrorCode(
			err, shoal.ErrorInvalidArgument,
		) {
			t.Fatalf("range %+v error = %v", sourceRange, err)
		}
	}
	if err := sourceRange(0, 0).ValidateSource(string([]byte{0xff})); !shoal.IsErrorCode(
		err, shoal.ErrorInvalidArgument,
	) {
		t.Fatalf("invalid source error = %v", err)
	}
}

func TestValidateRevisionContentAndExactQuotes(t *testing.T) {
	source, doc, revision, sections, spans := validRevision()
	if err := document.ValidateRevisionContent(
		source, doc, revision, sections, spans,
	); err != nil {
		t.Fatalf("valid revision content: %v", err)
	}
	citation := document.Citation{
		DocumentID: doc.ID,
		RevisionID: revision.ID,
		SectionID:  sections[1].ID,
		SpanID:     spans[0].ID,
		Range:      spans[0].Range,
	}
	quote, err := document.ResolveCitationQuote(
		source, doc, revision, sections, spans, citation)
	if err != nil {
		t.Fatalf("resolve citation: %v", err)
	}
	if quote != "éclair" {
		t.Fatalf("quote = %q", quote)
	}
	if err := document.ValidateCitationQuote(
		source, doc, revision, sections, spans, citation, quote,
	); err != nil {
		t.Fatalf("validate quote: %v", err)
	}
	if err := document.ValidateCitationQuote(
		source, doc, revision, sections, spans, citation, "eclair",
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("inexact quote error = %v", err)
	}
	citation.SpanID = ""
	sectionQuote, err := document.ResolveCitationQuote(
		source, doc, revision, sections, spans, citation)
	if err != nil {
		t.Fatalf("resolve section-only citation: %v", err)
	}
	if sectionQuote != "éclair" {
		t.Fatalf("section-only quote = %q", sectionQuote)
	}
}

func TestValidateRevisionContentRejectsOwnershipAndHierarchyFailures(t *testing.T) {
	tests := map[string]func(*document.Document, *document.Revision, []document.Section, []document.Span){
		"revision ownership": func(
			_ *document.Document, revision *document.Revision,
			_ []document.Section, _ []document.Span,
		) {
			revision.DocumentID = "other"
		},
		"section ownership": func(
			_ *document.Document, _ *document.Revision,
			sections []document.Section, _ []document.Span,
		) {
			sections[1].RevisionID = "other"
		},
		"span ownership": func(
			_ *document.Document, _ *document.Revision,
			_ []document.Section, spans []document.Span,
		) {
			spans[0].DocumentID = "other"
		},
		"cycle": func(
			_ *document.Document, _ *document.Revision,
			sections []document.Section, _ []document.Span,
		) {
			sections[0].ParentID = sections[1].ID
			sections[1].ParentID = sections[0].ID
		},
		"outside parent": func(
			_ *document.Document, _ *document.Revision,
			sections []document.Section, _ []document.Span,
		) {
			sections[1].Range = sourceRange(0, 2)
		},
		"duplicate shared order": func(
			_ *document.Document, _ *document.Revision,
			sections []document.Section, spans []document.Span,
		) {
			spans[0].SectionID = sections[0].ID
			spans[0].Order = sections[1].Order
		},
		"inexact span": func(
			_ *document.Document, _ *document.Revision,
			_ []document.Section, spans []document.Span,
		) {
			spans[0].Text = "eclair"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			source, doc, revision, sections, spans := validRevision()
			mutate(&doc, &revision, sections, spans)
			if err := document.ValidateRevisionContent(
				source, doc, revision, sections, spans,
			); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestResolveCitationChecksOwnership(t *testing.T) {
	source, doc, revision, sections, spans := validRevision()
	citation := document.Citation{
		DocumentID: doc.ID,
		RevisionID: revision.ID,
		SectionID:  sections[0].ID,
		SpanID:     spans[0].ID,
		Range:      spans[0].Range,
	}
	if _, err := document.ResolveCitationQuote(
		source, doc, revision, sections, spans, citation,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("mismatched section/span error = %v", err)
	}
	citation.SectionID = sections[1].ID
	citation.DocumentID = "other"
	if _, err := document.ResolveCitationQuote(
		source, doc, revision, sections, spans, citation,
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("mismatched document error = %v", err)
	}
}

func validRevision() (
	string,
	document.Document,
	document.Revision,
	[]document.Section,
	[]document.Span,
) {
	source := "Title\néclair"
	doc := document.Document{
		ID: "doc", RevisionID: "rev", RootSectionID: "root", Title: "Title",
	}
	revision := document.Revision{ID: "rev", DocumentID: "doc"}
	sections := []document.Section{
		{
			ID: "root", DocumentID: "doc", RevisionID: "rev",
			Range: sourceRange(0, int64(len(source))),
		},
		{
			ID: "child", DocumentID: "doc", RevisionID: "rev", ParentID: "root",
			Order: 0, Range: sourceRange(6, int64(len(source))),
		},
	}
	spans := []document.Span{{
		ID: "span", DocumentID: "doc", RevisionID: "rev", SectionID: "child",
		Order: 0, Range: sourceRange(6, int64(len(source))), Text: "éclair",
	}}
	return source, doc, revision, sections, spans
}

func sourceRange(start, end int64) document.SourceRange {
	return document.SourceRange{
		Start: document.SourcePosition{Offset: start},
		End:   document.SourcePosition{Offset: end},
	}
}
