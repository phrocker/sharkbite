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
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type parsedSource struct {
	document document.Document
	revision document.Revision
	source   Source
	sections []document.Section
	spans    []document.Span
	nodes    []graph.Node
	edges    []graph.Edge
}

type sourceLine struct {
	start      int
	contentEnd int
	text       string
}

type headingRecord struct {
	lineIndex int
	level     int
	section   document.Section
}

type orderedChild struct {
	start     int64
	isSection bool
	index     int
}

type parserLimits struct {
	maxSourceBytes int
	maxSourceLines int
	maxSections    int
	maxSpans       int
}

var defaultParserLimits = parserLimits{
	maxSourceBytes: document.MaxRevisionSourceBytes,
	maxSourceLines: document.MaxSourceLinesPerRevision,
	maxSections:    document.MaxSectionsPerRevision,
	maxSpans:       document.MaxSpansPerRevision,
}

func parseSource(source Source, createdAt time.Time) (parsedSource, error) {
	return parseSourceWithLimits(source, createdAt, defaultParserLimits)
}

func parseSourceWithLimits(
	source Source, createdAt time.Time, limits parserLimits,
) (parsedSource, error) {
	if !utf8.ValidString(source.URI) || strings.TrimSpace(source.URI) == "" {
		return parsedSource{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "source URI is required and must be valid UTF-8")
	}
	if len(source.Content) > limits.maxSourceBytes {
		return parsedSource{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "source content exceeds the public byte bound")
	}
	if !utf8.ValidString(source.Title) || !utf8.ValidString(source.Content) {
		return parsedSource{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "source title and content must be valid UTF-8")
	}
	if strings.TrimSpace(source.Content) == "" {
		return parsedSource{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "source content is required")
	}
	switch source.MediaType {
	case "", MediaTypeMarkdown:
		source.MediaType = MediaTypeMarkdown
	case MediaTypeText, MediaTypeSource:
	default:
		return parsedSource{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "source media type is not supported")
	}
	if err := validateMetadata(source.Metadata); err != nil {
		return parsedSource{}, err
	}
	if _, err := interaction.ParseVisibility(
		source.Metadata[interaction.PropertyVisibility],
	); err != nil {
		return parsedSource{}, err
	}

	lines, err := splitSourceLines(source.Content, limits.maxSourceLines)
	if err != nil {
		return parsedSource{}, err
	}
	title := strings.TrimSpace(source.Title)
	if title == "" {
		title = inferredTitle(source.URI, lines, source.MediaType)
	}
	source.Title = title
	source.Metadata = cloneMetadata(source.Metadata)
	documentID := shoal.ID(stableID(documentIDKind, source.URI))
	revisionID := shoal.ID(stableID(
		"rev", source.URI, source.MediaType, title, source.Content,
		canonicalMetadata(source.Metadata)))

	rootID := shoal.ID(stableID("section", string(documentID), "root"))
	fullRange := document.SourceRange{
		Start: document.SourcePosition{Offset: 0},
		End:   document.SourcePosition{Offset: int64(len(source.Content))},
	}
	root := document.Section{
		ID:         rootID,
		DocumentID: documentID,
		RevisionID: revisionID,
		Heading:    title,
		Range:      fullRange,
	}
	sections := []document.Section{root}
	var headings []headingRecord
	if source.MediaType == MediaTypeMarkdown {
		headings, err = parseHeadings(
			lines, documentID, revisionID, rootID, len(source.Content), limits.maxSections)
		if err != nil {
			return parsedSource{}, err
		}
		for _, heading := range headings {
			sections = append(sections, heading.section)
		}
	}
	spans, err := parseParagraphs(
		source.Content, lines, headings, documentID, revisionID, rootID, limits.maxSpans)
	if err != nil {
		return parsedSource{}, err
	}
	assignChildOrder(sections, spans)

	doc := document.Document{
		ID:            documentID,
		RevisionID:    revisionID,
		Title:         title,
		RootSectionID: rootID,
		Metadata:      cloneMetadata(source.Metadata),
	}
	revision := document.Revision{
		ID:            revisionID,
		DocumentID:    documentID,
		CreatedAt:     createdAt.UTC(),
		SourceVersion: hexDigest(source.Content),
		Metadata:      cloneMetadata(source.Metadata),
	}
	nodes, edges := materializeGraph(doc, sections, spans)
	if err := document.ValidateRevisionContent(
		source.Content, doc, revision, sections, spans,
	); err != nil {
		return parsedSource{}, err
	}
	for _, node := range nodes {
		if err := node.Validate(); err != nil {
			return parsedSource{}, err
		}
	}
	for _, edge := range edges {
		if err := edge.Validate(); err != nil {
			return parsedSource{}, err
		}
	}
	return parsedSource{
		document: doc,
		revision: revision,
		source:   source,
		sections: sections,
		spans:    spans,
		nodes:    nodes,
		edges:    edges,
	}, nil
}

func splitSourceLines(content string, maximum int) ([]sourceLine, error) {
	capacity := len(content)/80 + 1
	if capacity > maximum {
		capacity = maximum
	}
	if capacity > 4096 {
		capacity = 4096
	}
	lines := make([]sourceLine, 0, capacity)
	for start := 0; start < len(content); {
		if len(lines) >= maximum {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument, "source content has too many line fragments")
		}
		newline := strings.IndexByte(content[start:], '\n')
		end := len(content)
		if newline >= 0 {
			end = start + newline + 1
		}
		contentEnd := end
		if contentEnd > start && content[contentEnd-1] == '\n' {
			contentEnd--
		}
		if contentEnd > start && content[contentEnd-1] == '\r' {
			contentEnd--
		}
		lines = append(lines, sourceLine{
			start: start, contentEnd: contentEnd, text: content[start:contentEnd],
		})
		start = end
	}
	return lines, nil
}

func markdownHeading(line string) (int, string, bool) {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 {
		return 0, "", false
	}
	level := 0
	for level < len(trimmed) && level < 6 && trimmed[level] == '#' {
		level++
	}
	if level == 0 || level == len(trimmed) || !unicode.IsSpace(rune(trimmed[level])) {
		return 0, "", false
	}
	title := strings.TrimSpace(trimmed[level:])
	closingStart := len(title)
	for closingStart > 0 && title[closingStart-1] == '#' {
		closingStart--
	}
	if closingStart < len(title) && closingStart > 0 {
		preceding, _ := utf8.DecodeLastRuneInString(title[:closingStart])
		if unicode.IsSpace(preceding) {
			title = strings.TrimSpace(title[:closingStart])
		}
	}
	if title == "" {
		return 0, "", false
	}
	return level, title, true
}

type markdownFence struct {
	marker byte
	length int
}

func parseMarkdownFence(line string) (markdownFence, string, bool) {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 || len(trimmed) < 3 {
		return markdownFence{}, "", false
	}
	marker := trimmed[0]
	if marker != '`' && marker != '~' {
		return markdownFence{}, "", false
	}
	length := 0
	for length < len(trimmed) && trimmed[length] == marker {
		length++
	}
	if length < 3 {
		return markdownFence{}, "", false
	}
	return markdownFence{marker: marker, length: length}, trimmed[length:], true
}

func parseHeadings(
	lines []sourceLine,
	documentID, revisionID, rootID shoal.ID,
	contentLength, maxSections int,
) ([]headingRecord, error) {
	var headings []headingRecord
	var stack []int
	occurrences := make(map[string]int)
	var openFence markdownFence
	for lineIndex, line := range lines {
		if fence, suffix, ok := parseMarkdownFence(line.text); ok {
			if openFence.length == 0 {
				openFence = fence
				continue
			}
			if fence.marker == openFence.marker && fence.length >= openFence.length &&
				strings.TrimSpace(suffix) == "" {
				openFence = markdownFence{}
			}
			continue
		}
		if openFence.length != 0 {
			continue
		}
		level, title, ok := markdownHeading(line.text)
		if !ok {
			continue
		}
		if len(headings)+1 >= maxSections {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument, "revision has too many sections")
		}
		for len(stack) > 0 && headings[stack[len(stack)-1]].level >= level {
			stack = stack[:len(stack)-1]
		}
		parentID := rootID
		if len(stack) > 0 {
			parentID = headings[stack[len(stack)-1]].section.ID
		}
		key := string(parentID) + "\x00" + strings.ToLower(title)
		occurrence := occurrences[key]
		occurrences[key] = occurrence + 1
		id := shoal.ID(stableID(
			"section", string(documentID), string(parentID), title, fmt.Sprint(occurrence)))
		record := headingRecord{
			lineIndex: lineIndex,
			level:     level,
			section: document.Section{
				ID:         id,
				DocumentID: documentID,
				RevisionID: revisionID,
				ParentID:   parentID,
				Heading:    title,
				Range: document.SourceRange{
					Start: document.SourcePosition{Offset: int64(line.start)},
					End:   document.SourcePosition{Offset: int64(contentLength)},
				},
			},
		}
		headings = append(headings, record)
		stack = append(stack, len(headings)-1)
	}
	stack = stack[:0]
	for index := range headings {
		for len(stack) > 0 &&
			headings[stack[len(stack)-1]].level >= headings[index].level {
			closed := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			headings[closed].section.Range.End.Offset =
				int64(lines[headings[index].lineIndex].start)
		}
		stack = append(stack, index)
	}
	return headings, nil
}

func parseParagraphs(
	content string,
	lines []sourceLine,
	headings []headingRecord,
	documentID, revisionID, rootID shoal.ID,
	maxSpans int,
) ([]document.Span, error) {
	headingLines := make(map[int]struct{}, len(headings))
	for _, heading := range headings {
		headingLines[heading.lineIndex] = struct{}{}
	}
	var spans []document.Span
	occurrences := make(map[string]int)
	for lineIndex := 0; lineIndex < len(lines); {
		line := lines[lineIndex]
		if _, heading := headingLines[lineIndex]; heading ||
			strings.TrimSpace(line.text) == "" {
			lineIndex++
			continue
		}
		startLine := lineIndex
		endLine := lineIndex
		for endLine+1 < len(lines) {
			if _, heading := headingLines[endLine+1]; heading ||
				strings.TrimSpace(lines[endLine+1].text) == "" {
				break
			}
			endLine++
		}
		start := lines[startLine].start
		end := lines[endLine].contentEnd
		for start < end {
			r, size := utf8.DecodeRuneInString(content[start:end])
			if !unicode.IsSpace(r) {
				break
			}
			start += size
		}
		for end > start {
			r, size := utf8.DecodeLastRuneInString(content[start:end])
			if !unicode.IsSpace(r) {
				break
			}
			end -= size
		}
		if start < end {
			if len(spans) >= maxSpans {
				return nil, shoal.NewError(
					shoal.ErrorInvalidArgument, "revision has too many spans")
			}
			sectionID := rootID
			for i := range headings {
				section := headings[i].section
				if int64(start) >= section.Range.Start.Offset &&
					int64(end) <= section.Range.End.Offset {
					sectionID = section.ID
				}
			}
			text := content[start:end]
			key := string(sectionID) + "\x00" + text
			occurrence := occurrences[key]
			occurrences[key] = occurrence + 1
			spans = append(spans, document.Span{
				ID: shoal.ID(stableID(
					"span", string(documentID), string(sectionID), text, fmt.Sprint(occurrence))),
				DocumentID: documentID,
				RevisionID: revisionID,
				SectionID:  sectionID,
				Range: document.SourceRange{
					Start: document.SourcePosition{Offset: int64(start)},
					End:   document.SourcePosition{Offset: int64(end)},
				},
				Text: text,
			})
		}
		lineIndex = endLine + 1
	}
	return spans, nil
}

func assignChildOrder(sections []document.Section, spans []document.Span) {
	children := make(map[shoal.ID][]orderedChild)
	for i := 1; i < len(sections); i++ {
		children[sections[i].ParentID] = append(children[sections[i].ParentID], orderedChild{
			start: sections[i].Range.Start.Offset, isSection: true, index: i,
		})
	}
	for i := range spans {
		children[spans[i].SectionID] = append(children[spans[i].SectionID], orderedChild{
			start: spans[i].Range.Start.Offset, index: i,
		})
	}
	for _, siblings := range children {
		sort.Slice(siblings, func(i, j int) bool {
			if siblings[i].start == siblings[j].start {
				return siblings[i].isSection
			}
			return siblings[i].start < siblings[j].start
		})
		for order, child := range siblings {
			if child.isSection {
				sections[child.index].Order = uint32(order)
			} else {
				spans[child.index].Order = uint32(order)
			}
		}
	}
}

func materializeGraph(doc document.Document, sections []document.Section,
	spans []document.Span) ([]graph.Node, []graph.Edge) {
	// A source declares its visibility once, in ingest metadata. Propagating
	// it onto every content node is what lets an interaction record derive its
	// own visibility as the conjunction over the nodes it touched, using the
	// graph alone. A source with no declared visibility is public.
	visibility := canonicalVisibility(doc.Metadata[interaction.PropertyVisibility])
	nodes := []graph.Node{{
		ID: doc.ID, Kind: "document", Labels: []string{"document"},
		Properties: shoal.Metadata{"title": doc.Title, "revision_id": string(doc.RevisionID)},
	}}
	setVisibility(nodes[0].Properties, visibility)
	edges := make([]graph.Edge, 0, len(sections)+len(spans))
	for _, section := range sections {
		properties := shoal.Metadata{
			"heading": section.Heading, "document_id": string(doc.ID),
			"revision_id": string(doc.RevisionID),
		}
		setVisibility(properties, visibility)
		nodes = append(nodes, graph.Node{
			ID: section.ID, Kind: "section", Labels: []string{"section"},
			Properties: properties,
		})
		parent := section.ParentID
		if parent == "" {
			parent = doc.ID
		}
		edges = append(edges, containsEdge(parent, section.ID))
	}
	for _, span := range spans {
		properties := shoal.Metadata{
			"document_id": string(doc.ID), "revision_id": string(doc.RevisionID),
			"section_id": string(span.SectionID),
		}
		setVisibility(properties, visibility)
		nodes = append(nodes, graph.Node{
			ID: span.ID, Kind: "span", Labels: []string{"evidence"},
			Properties: properties,
		})
		edges = append(edges, containsEdge(span.SectionID, span.ID))
	}
	return nodes, edges
}

func setVisibility(properties shoal.Metadata, expression string) {
	if expression == "" {
		return
	}
	properties[interaction.PropertyVisibility] = expression
}

// canonicalVisibility normalizes a declared visibility expression. parseSource
// has already rejected malformed declarations, so an error here is impossible
// and is treated as public.
func canonicalVisibility(expression string) string {
	labels, err := interaction.ParseVisibility(expression)
	if err != nil {
		return ""
	}
	return interaction.Expression(labels)
}

func containsEdge(from, to shoal.ID) graph.Edge {
	return graph.Edge{
		ID:   shoal.ID(stableID("edge", string(from), "contains", string(to))),
		From: from, To: to, Type: "contains", Weight: 1,
	}
}

func inferredTitle(uri string, lines []sourceLine, mediaType string) string {
	if mediaType == MediaTypeMarkdown {
		var openFence markdownFence
		for _, line := range lines {
			if fence, suffix, ok := parseMarkdownFence(line.text); ok {
				if openFence.length == 0 {
					openFence = fence
					continue
				}
				if fence.marker == openFence.marker && fence.length >= openFence.length &&
					strings.TrimSpace(suffix) == "" {
					openFence = markdownFence{}
				}
				continue
			}
			if openFence.length != 0 {
				continue
			}
			if _, title, ok := markdownHeading(line.text); ok {
				return title
			}
		}
	}
	trimmed := strings.TrimRight(uri, `/\`)
	if separator := strings.LastIndexAny(trimmed, `/\`); separator >= 0 {
		trimmed = trimmed[separator+1:]
	}
	if trimmed == "" {
		return "Untitled"
	}
	return trimmed
}

func stableID(kind string, parts ...string) string {
	hash := sha256.New()
	var length [8]byte
	writeComponent := func(value string) {
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(value))
	}
	writeComponent(kind)
	for _, part := range parts {
		writeComponent(part)
	}
	return kind + "_" + hex.EncodeToString(hash.Sum(nil)[:16])
}

func hexDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func validateMetadata(metadata shoal.Metadata) error {
	return shoal.ValidateMetadata("source metadata", metadata)
}

func cloneMetadata(metadata shoal.Metadata) shoal.Metadata {
	if metadata == nil {
		return nil
	}
	clone := make(shoal.Metadata, len(metadata))
	for key, value := range metadata {
		clone[key] = value
	}
	return clone
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

func canonicalMetadata(metadata shoal.Metadata) string {
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var canonical strings.Builder
	var length [8]byte
	for _, key := range keys {
		binary.BigEndian.PutUint64(length[:], uint64(len(key)))
		_, _ = canonical.Write(length[:])
		canonical.WriteString(key)
		value := metadata[key]
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = canonical.Write(length[:])
		canonical.WriteString(value)
	}
	return canonical.String()
}
