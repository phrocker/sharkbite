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

// Package document defines Shoal's public hierarchical document contract.
package document

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	MaxRevisionSourceBytes = 512 * 1024 * 1024
	MaxSectionsPerRevision = 100_000
	MaxSpansPerRevision    = 1_000_000
	// MaxSourceLinesPerRevision permits every bounded section and span to
	// occupy its own line with a blank separator while preventing newline-only
	// sources from amplifying into an unbounded parser index.
	MaxSourceLinesPerRevision = 2*(MaxSectionsPerRevision+MaxSpansPerRevision) + 1
)

// SourcePosition identifies a zero-based UTF-8 byte offset and, when known, a
// one-based source page. Page zero means the source has no page information.
type SourcePosition struct {
	Offset int64
	Page   int32
}

// SourceRange is a half-open UTF-8 byte interval [Start, End). Empty ranges,
// including an empty range at end of source, are valid.
type SourceRange struct {
	Start SourcePosition
	End   SourcePosition
}

// Validate checks the ordering and coordinate invariants of a source range.
func (r SourceRange) Validate() error {
	if r.Start.Offset < 0 || r.End.Offset < r.Start.Offset {
		return shoal.NewError(shoal.ErrorInvalidArgument, "invalid source offsets")
	}
	if r.Start.Page < 0 || r.End.Page < 0 {
		return shoal.NewError(shoal.ErrorInvalidArgument, "source pages cannot be negative")
	}
	if r.Start.Page > 0 && r.End.Page > 0 && r.End.Page < r.Start.Page {
		return shoal.NewError(shoal.ErrorInvalidArgument, "invalid source page range")
	}
	return nil
}

// ValidateSource checks this range against canonical UTF-8 source bytes.
func (r SourceRange) ValidateSource(source string) error {
	if !utf8.ValidString(source) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "canonical source must be valid UTF-8")
	}
	return r.validateSourceBounds(source)
}

func (r SourceRange) validateSourceBounds(source string) error {
	if err := r.Validate(); err != nil {
		return err
	}
	sourceLength := int64(len(source))
	if r.End.Offset > sourceLength {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "source range exceeds canonical source")
	}
	for _, offset := range []int64{r.Start.Offset, r.End.Offset} {
		if offset > 0 && offset < sourceLength && !utf8.RuneStart(source[offset]) {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "source offset is not a UTF-8 boundary")
		}
	}
	return nil
}

// Revision identifies an immutable version of a document. CreatedAt and
// SourceVersion describe the source and are not publication order.
type Revision struct {
	ID            shoal.ID
	DocumentID    shoal.ID
	CreatedAt     time.Time
	SourceVersion string
	Metadata      shoal.Metadata
}

// Validate checks revision identity and public static bounds.
func (r Revision) Validate() error {
	if err := shoal.ValidateRequiredID("revision ID", r.ID); err != nil {
		return err
	}
	if err := shoal.ValidateRequiredID("revision document ID", r.DocumentID); err != nil {
		return err
	}
	if !r.CreatedAt.IsZero() {
		year := r.CreatedAt.UTC().Year()
		if year < 1 || year > 9999 {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "revision created_at is outside the supported range")
		}
	}
	if err := shoal.ValidateSemanticString("revision source version", r.SourceVersion); err != nil {
		return err
	}
	return shoal.ValidateMetadata("revision metadata", r.Metadata)
}

// Document is the revision-specific root of a hierarchical source.
type Document struct {
	ID            shoal.ID
	RevisionID    shoal.ID
	Title         string
	RootSectionID shoal.ID
	Metadata      shoal.Metadata
}

// Validate checks document identity and public static bounds.
func (d Document) Validate() error {
	if err := shoal.ValidateRequiredID("document ID", d.ID); err != nil {
		return err
	}
	if err := shoal.ValidateRequiredID("document revision ID", d.RevisionID); err != nil {
		return err
	}
	if err := shoal.ValidateRequiredID("document root section ID", d.RootSectionID); err != nil {
		return err
	}
	if err := shoal.ValidateSemanticString("document title", d.Title); err != nil {
		return err
	}
	return shoal.ValidateMetadata("document metadata", d.Metadata)
}

// Section is an ordered node in a document tree. ParentID is empty only for
// the document's root section.
type Section struct {
	ID         shoal.ID
	DocumentID shoal.ID
	RevisionID shoal.ID
	ParentID   shoal.ID
	Order      uint32
	Heading    string
	Range      SourceRange
	Metadata   shoal.Metadata
}

// Validate checks section identity, range structure, and public static bounds.
func (s Section) Validate() error {
	for name, id := range map[string]shoal.ID{
		"section ID":          s.ID,
		"section document ID": s.DocumentID,
		"section revision ID": s.RevisionID,
	} {
		if err := shoal.ValidateRequiredID(name, id); err != nil {
			return err
		}
	}
	if err := shoal.ValidateOptionalID("section parent ID", s.ParentID); err != nil {
		return err
	}
	if err := shoal.ValidateSemanticString("section heading", s.Heading); err != nil {
		return err
	}
	if err := s.Range.Validate(); err != nil {
		return fmt.Errorf("section: %w", err)
	}
	return shoal.ValidateMetadata("section metadata", s.Metadata)
}

// Span is an ordered, directly attributable piece of source content.
type Span struct {
	ID         shoal.ID
	DocumentID shoal.ID
	RevisionID shoal.ID
	SectionID  shoal.ID
	Order      uint32
	Range      SourceRange
	Text       string
	Metadata   shoal.Metadata
}

// Validate checks span identity, range structure, and public static bounds.
func (s Span) Validate() error {
	for name, id := range map[string]shoal.ID{
		"span ID":          s.ID,
		"span document ID": s.DocumentID,
		"span revision ID": s.RevisionID,
		"span section ID":  s.SectionID,
	} {
		if err := shoal.ValidateRequiredID(name, id); err != nil {
			return err
		}
	}
	if err := s.Range.Validate(); err != nil {
		return fmt.Errorf("span: %w", err)
	}
	if !utf8.ValidString(s.Text) {
		return shoal.NewError(shoal.ErrorInvalidArgument, "span text must be valid UTF-8")
	}
	return shoal.ValidateMetadata("span metadata", s.Metadata)
}

// Citation is an exact reference to evidence in one document revision.
type Citation struct {
	DocumentID shoal.ID
	RevisionID shoal.ID
	SectionID  shoal.ID
	SpanID     shoal.ID
	Range      SourceRange
}

// Validate checks that the citation can identify immutable source evidence.
// Existence, ownership, containment, and quote equality require revision
// context and are checked by ResolveCitationQuote.
func (c Citation) Validate() error {
	if err := shoal.ValidateRequiredID("citation document ID", c.DocumentID); err != nil {
		return err
	}
	if err := shoal.ValidateRequiredID("citation revision ID", c.RevisionID); err != nil {
		return err
	}
	if c.SectionID == "" && c.SpanID == "" {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "citation requires a section or span ID")
	}
	if err := shoal.ValidateOptionalID("citation section ID", c.SectionID); err != nil {
		return err
	}
	if err := shoal.ValidateOptionalID("citation span ID", c.SpanID); err != nil {
		return err
	}
	if err := c.Range.Validate(); err != nil {
		return fmt.Errorf("citation: %w", err)
	}
	return nil
}

// ValidateRevisionContent validates a complete revision bundle without
// assuming persistence, publication, latest-selection, or hydration behavior.
func ValidateRevisionContent(
	source string,
	document Document,
	revision Revision,
	sections []Section,
	spans []Span,
) error {
	if !utf8.ValidString(source) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "canonical source must be valid UTF-8")
	}
	if len(source) > MaxRevisionSourceBytes {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "canonical source exceeds the public byte bound")
	}
	if sourceLineFragmentsExceed(source, MaxSourceLinesPerRevision) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "canonical source has too many line fragments")
	}
	if len(sections) > MaxSectionsPerRevision {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "revision has too many sections")
	}
	if len(spans) > MaxSpansPerRevision {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "revision has too many spans")
	}
	if err := document.Validate(); err != nil {
		return err
	}
	if err := revision.Validate(); err != nil {
		return err
	}
	if document.RevisionID != revision.ID || revision.DocumentID != document.ID {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "document and revision ownership do not match")
	}

	sectionByID := make(map[shoal.ID]Section, len(sections))
	children := make(map[shoal.ID][]shoal.ID)
	orders := make(map[shoal.ID]map[uint32]struct{})
	rootCount := 0
	for _, section := range sections {
		if err := section.Validate(); err != nil {
			return err
		}
		if section.DocumentID != document.ID || section.RevisionID != revision.ID {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "section ownership does not match revision")
		}
		if err := section.Range.validateSourceBounds(source); err != nil {
			return fmt.Errorf("section %q: %w", section.ID, err)
		}
		if _, duplicate := sectionByID[section.ID]; duplicate {
			return shoal.NewError(shoal.ErrorInvalidArgument, "duplicate section ID")
		}
		sectionByID[section.ID] = section
		if section.ParentID == "" {
			rootCount++
			continue
		}
		children[section.ParentID] = append(children[section.ParentID], section.ID)
		if err := addSiblingOrder(orders, section.ParentID, section.Order); err != nil {
			return err
		}
	}
	if rootCount != 1 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "revision requires exactly one root section")
	}
	root, ok := sectionByID[document.RootSectionID]
	if !ok || root.ParentID != "" {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "document root section is missing or has a parent")
	}
	for _, section := range sections {
		if section.ParentID == "" {
			if section.ID != document.RootSectionID {
				return shoal.NewError(
					shoal.ErrorInvalidArgument, "revision contains a disconnected root section")
			}
			continue
		}
		parent, ok := sectionByID[section.ParentID]
		if !ok {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "section parent does not exist")
		}
		if !rangeContains(parent.Range, section.Range) {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "section range is not contained by its parent")
		}
	}

	spanByID := make(map[shoal.ID]Span, len(spans))
	for _, span := range spans {
		if err := span.Validate(); err != nil {
			return err
		}
		if span.DocumentID != document.ID || span.RevisionID != revision.ID {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "span ownership does not match revision")
		}
		section, ok := sectionByID[span.SectionID]
		if !ok {
			return shoal.NewError(shoal.ErrorInvalidArgument, "span section does not exist")
		}
		if err := span.Range.validateSourceBounds(source); err != nil {
			return fmt.Errorf("span %q: %w", span.ID, err)
		}
		if !rangeContains(section.Range, span.Range) {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "span range is not contained by its section")
		}
		if span.Text != source[span.Range.Start.Offset:span.Range.End.Offset] {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "span text does not match canonical source bytes")
		}
		if _, duplicate := spanByID[span.ID]; duplicate {
			return shoal.NewError(shoal.ErrorInvalidArgument, "duplicate span ID")
		}
		spanByID[span.ID] = span
		if err := addSiblingOrder(orders, span.SectionID, span.Order); err != nil {
			return err
		}
	}

	visited := make(map[shoal.ID]bool, len(sections))
	visiting := make(map[shoal.ID]bool, len(sections))
	var visit func(shoal.ID) error
	visit = func(id shoal.ID) error {
		if visiting[id] {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "section hierarchy contains a cycle")
		}
		if visited[id] {
			return nil
		}
		visiting[id] = true
		for _, child := range children[id] {
			if err := visit(child); err != nil {
				return err
			}
		}
		delete(visiting, id)
		visited[id] = true
		return nil
	}
	if err := visit(document.RootSectionID); err != nil {
		return err
	}
	if len(visited) != len(sections) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "section hierarchy is not connected")
	}
	return nil
}

func sourceLineFragmentsExceed(source string, maximum int) bool {
	fragments := 0
	for start := 0; start < len(source); {
		fragments++
		if fragments > maximum {
			return true
		}
		newline := strings.IndexByte(source[start:], '\n')
		if newline < 0 {
			break
		}
		start += newline + 1
	}
	return false
}

// ResolveCitationQuote validates exact retained revision content and returns
// the canonical source bytes named by citation. It performs no storage read.
func ResolveCitationQuote(
	source string,
	document Document,
	revision Revision,
	sections []Section,
	spans []Span,
	citation Citation,
) (string, error) {
	if err := ValidateRevisionContent(source, document, revision, sections, spans); err != nil {
		return "", err
	}
	if err := citation.Validate(); err != nil {
		return "", err
	}
	if citation.DocumentID != document.ID || citation.RevisionID != revision.ID {
		return "", shoal.NewError(
			shoal.ErrorInvalidArgument, "citation ownership does not match revision")
	}
	if err := citation.Range.validateSourceBounds(source); err != nil {
		return "", fmt.Errorf("citation: %w", err)
	}

	sectionByID := make(map[shoal.ID]Section, len(sections))
	for _, section := range sections {
		sectionByID[section.ID] = section
	}
	spanByID := make(map[shoal.ID]Span, len(spans))
	for _, span := range spans {
		spanByID[span.ID] = span
	}

	if citation.SectionID != "" {
		section, ok := sectionByID[citation.SectionID]
		if !ok {
			return "", shoal.NewError(shoal.ErrorNotFound, "cited section was not found")
		}
		if !rangeContains(section.Range, citation.Range) {
			return "", shoal.NewError(
				shoal.ErrorInvalidArgument, "citation range is outside cited section")
		}
	}
	if citation.SpanID != "" {
		span, ok := spanByID[citation.SpanID]
		if !ok {
			return "", shoal.NewError(shoal.ErrorNotFound, "cited span was not found")
		}
		if citation.SectionID != "" && span.SectionID != citation.SectionID {
			return "", shoal.NewError(
				shoal.ErrorInvalidArgument, "cited span does not belong to cited section")
		}
		if !rangeContains(span.Range, citation.Range) {
			return "", shoal.NewError(
				shoal.ErrorInvalidArgument, "citation range is outside cited span")
		}
	}
	return source[citation.Range.Start.Offset:citation.Range.End.Offset], nil
}

// ValidateCitationQuote checks a supplied quote against canonical retained
// revision source bytes.
func ValidateCitationQuote(
	source string,
	document Document,
	revision Revision,
	sections []Section,
	spans []Span,
	citation Citation,
	quote string,
) error {
	canonical, err := ResolveCitationQuote(
		source, document, revision, sections, spans, citation)
	if err != nil {
		return err
	}
	if quote != canonical {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "citation quote does not match canonical source bytes")
	}
	return nil
}

func addSiblingOrder(
	orders map[shoal.ID]map[uint32]struct{},
	parentID shoal.ID,
	order uint32,
) error {
	if orders[parentID] == nil {
		orders[parentID] = make(map[uint32]struct{})
	}
	if _, duplicate := orders[parentID][order]; duplicate {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "section children and spans have duplicate order")
	}
	orders[parentID][order] = struct{}{}
	return nil
}

func rangeContains(outer, inner SourceRange) bool {
	return outer.Start.Offset <= inner.Start.Offset &&
		inner.End.Offset <= outer.End.Offset
}
