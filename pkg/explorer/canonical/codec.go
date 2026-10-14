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

package canonical

import (
	"bytes"
	"encoding/binary"
	"math"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	minMetadataEntryBytes = 8
	minSectionBytes       = 52
	minSpanBytes          = 52
	minNodeBytes          = 16
	minEdgeBytes          = 28
)

type encoder struct {
	data     []byte
	expected []byte
	offset   int
	compare  bool
	mismatch bool
	err      error
}

func newEncoder() *encoder {
	return &encoder{data: make([]byte, envelopeHeaderBytes)}
}

func newComparingEncoder(expected []byte) *encoder {
	return &encoder{expected: expected, compare: true}
}

func (e *encoder) ensure(size int) bool {
	if e.err != nil {
		return false
	}
	position := len(e.data)
	if e.compare {
		position = envelopeHeaderBytes + e.offset
	}
	if size < 0 ||
		position > MaxCanonicalRecordBytes-envelopeChecksumSize-size {
		e.err = invalid("canonical record exceeds the aggregate byte bound")
		return false
	}
	return true
}

func (e *encoder) writeBytes(value []byte) {
	if !e.ensure(len(value)) {
		return
	}
	if !e.compare {
		e.data = append(e.data, value...)
		return
	}
	start := e.offset
	e.offset += len(value)
	if e.mismatch {
		return
	}
	if start > len(e.expected) || len(value) > len(e.expected)-start {
		e.mismatch = true
		return
	}
	for index, item := range value {
		if e.expected[start+index] != item {
			e.mismatch = true
			return
		}
	}
}

func (e *encoder) writeString(value string) {
	if !e.ensure(len(value)) {
		return
	}
	if !e.compare {
		e.data = append(e.data, value...)
		return
	}
	start := e.offset
	e.offset += len(value)
	if e.mismatch {
		return
	}
	if start > len(e.expected) || len(value) > len(e.expected)-start {
		e.mismatch = true
		return
	}
	for index := 0; index < len(value); index++ {
		if e.expected[start+index] != value[index] {
			e.mismatch = true
			return
		}
	}
}

func (e *encoder) putByte(value byte) {
	var encoded [1]byte
	encoded[0] = value
	e.writeBytes(encoded[:])
}

func (e *encoder) putUint32(value uint32) {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], value)
	e.writeBytes(encoded[:])
}

func (e *encoder) putUint64(value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	e.writeBytes(encoded[:])
}

func (e *encoder) putBytes(name string, value []byte) {
	if len(value) > math.MaxUint32 {
		e.err = invalid(name + " exceeds the codec byte bound")
		return
	}
	e.putUint32(uint32(len(value)))
	e.writeBytes(value)
}

func (e *encoder) putString(name, value string) {
	if len(value) > math.MaxUint32 {
		e.err = invalid(name + " exceeds the codec byte bound")
		return
	}
	e.putUint32(uint32(len(value)))
	e.writeString(value)
}

func (e *encoder) putID(name string, value shoal.ID) {
	e.putString(name, string(value))
}

func (e *encoder) putCount(name string, count int) {
	if count < 0 || uint64(count) > math.MaxUint32 {
		e.err = invalid(name + " exceeds the codec count bound")
		return
	}
	e.putUint32(uint32(count))
}

func encodeRecordV1(e *encoder, record RecordV1) {
	encodePublication(e, record.Publication)
	e.putBytes("canonical source", record.Source)
	encodeDocument(e, record.Document)
	encodeRevision(e, record.Revision)

	e.putCount("section count", len(record.Sections))
	for _, section := range record.Sections {
		encodeSection(e, section)
	}
	e.putCount("span count", len(record.Spans))
	for _, span := range record.Spans {
		encodeSpan(e, span)
	}
	e.putCount("graph node count", len(record.Nodes))
	for _, node := range record.Nodes {
		encodeNode(e, node)
	}
	e.putCount("graph edge count", len(record.Edges))
	for _, edge := range record.Edges {
		encodeEdge(e, edge)
	}
}

func encodePublication(e *encoder, publication *PublicationV1) {
	if publication == nil {
		e.putByte(0)
		return
	}
	e.putByte(1)
	e.putUint64(publication.Sequence)
	encodeTime(e, publication.PublishedAt)
}

func encodeDocument(e *encoder, value document.Document) {
	e.putID("document ID", value.ID)
	e.putID("document revision ID", value.RevisionID)
	e.putString("document title", value.Title)
	e.putID("document root section ID", value.RootSectionID)
	encodeMetadata(e, value.Metadata)
}

func encodeRevision(e *encoder, value document.Revision) {
	e.putID("revision ID", value.ID)
	e.putID("revision document ID", value.DocumentID)
	encodeTime(e, value.CreatedAt)
	e.putString("revision source version", value.SourceVersion)
	encodeMetadata(e, value.Metadata)
}

func encodeSection(e *encoder, value document.Section) {
	e.putID("section ID", value.ID)
	e.putID("section document ID", value.DocumentID)
	e.putID("section revision ID", value.RevisionID)
	e.putID("section parent ID", value.ParentID)
	e.putUint32(value.Order)
	e.putString("section heading", value.Heading)
	encodeRange(e, value.Range)
	encodeMetadata(e, value.Metadata)
}

func encodeSpan(e *encoder, value document.Span) {
	e.putID("span ID", value.ID)
	e.putID("span document ID", value.DocumentID)
	e.putID("span revision ID", value.RevisionID)
	e.putID("span section ID", value.SectionID)
	e.putUint32(value.Order)
	encodeRange(e, value.Range)
	e.putString("span text", value.Text)
	encodeMetadata(e, value.Metadata)
}

func encodeNode(e *encoder, value graph.Node) {
	e.putID("graph node ID", value.ID)
	e.putString("graph node kind", value.Kind)
	e.putCount("graph node label count", len(value.Labels))
	for _, label := range value.Labels {
		e.putString("graph node label", label)
	}
	encodeMetadata(e, value.Properties)
}

func encodeEdge(e *encoder, value graph.Edge) {
	e.putID("graph edge ID", value.ID)
	e.putID("graph edge from", value.From)
	e.putID("graph edge to", value.To)
	e.putString("graph edge type", value.Type)
	e.putUint64(math.Float64bits(float64(value.Weight)))
	encodeMetadata(e, value.Properties)
}

func encodeRange(e *encoder, value document.SourceRange) {
	e.putUint64(uint64(value.Start.Offset))
	e.putUint32(uint32(value.Start.Page))
	e.putUint64(uint64(value.End.Offset))
	e.putUint32(uint32(value.End.Page))
}

func encodeTime(e *encoder, value time.Time) {
	if value.IsZero() {
		e.putByte(0)
		return
	}
	value = value.UTC()
	e.putByte(1)
	e.putUint64(uint64(value.Unix()))
	e.putUint32(uint32(value.Nanosecond()))
}

func encodeMetadata(e *encoder, metadata shoal.Metadata) {
	e.putCount("metadata entry count", len(metadata))
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return bytes.Compare([]byte(keys[i]), []byte(keys[j])) < 0
	})
	for _, key := range keys {
		e.putString("metadata key", key)
		e.putString("metadata value", metadata[key])
	}
}

type decoder struct {
	data   []byte
	offset int
	err    error
}

func newDecoder(data []byte) *decoder {
	return &decoder{data: data}
}

func (d *decoder) remaining() int {
	return len(d.data) - d.offset
}

func (d *decoder) take(name string, size int) []byte {
	if d.err != nil {
		return nil
	}
	if size < 0 || size > d.remaining() {
		d.err = invalid("canonical record is truncated in " + name)
		return nil
	}
	value := d.data[d.offset : d.offset+size]
	d.offset += size
	return value
}

func (d *decoder) readByte(name string) byte {
	value := d.take(name, 1)
	if value == nil {
		return 0
	}
	return value[0]
}

func (d *decoder) readUint32(name string) uint32 {
	value := d.take(name, 4)
	if value == nil {
		return 0
	}
	return binary.BigEndian.Uint32(value)
}

func (d *decoder) readUint64(name string) uint64 {
	value := d.take(name, 8)
	if value == nil {
		return 0
	}
	return binary.BigEndian.Uint64(value)
}

func (d *decoder) readNonNegativeInt64(name string) int64 {
	value := d.readUint64(name)
	if d.err != nil {
		return 0
	}
	if value > math.MaxInt64 {
		d.err = invalid(name + " exceeds the supported signed range")
		return 0
	}
	return int64(value)
}

func (d *decoder) readNonNegativeInt32(name string) int32 {
	value := d.readUint32(name)
	if d.err != nil {
		return 0
	}
	if value > math.MaxInt32 {
		d.err = invalid(name + " exceeds the supported signed range")
		return 0
	}
	return int32(value)
}

func (d *decoder) readInt64(name string) int64 {
	return int64(d.readUint64(name))
}

func (d *decoder) readBytes(name string, maximum int) []byte {
	length := uint64(d.readUint32(name + " length"))
	if d.err != nil {
		return nil
	}
	if length > uint64(maximum) {
		d.err = invalid(name + " exceeds its byte bound")
		return nil
	}
	if length > uint64(d.remaining()) {
		d.err = invalid("canonical record is truncated in " + name)
		return nil
	}
	value := make([]byte, int(length))
	copy(value, d.take(name, int(length)))
	return value
}

func (d *decoder) readString(name string, maximum int) string {
	return string(d.readBytes(name, maximum))
}

func (d *decoder) readID(name string) shoal.ID {
	return shoal.ID(d.readString(name, shoal.MaxIDBytes))
}

func (d *decoder) readCount(
	name string,
	maximum int,
	minimumEncodedBytes int,
) int {
	count := uint64(d.readUint32(name))
	if d.err != nil {
		return 0
	}
	if count > uint64(maximum) {
		d.err = invalid(name + " exceeds its count bound")
		return 0
	}
	if count > 0 &&
		count > uint64(d.remaining())/uint64(minimumEncodedBytes) {
		d.err = invalid("canonical record is truncated in " + name)
		return 0
	}
	return int(count)
}

func decodeRecordV1(d *decoder) RecordV1 {
	record := RecordV1{
		Publication: decodePublication(d),
		Source: d.readBytes(
			"canonical source", document.MaxRevisionSourceBytes),
		Document: decodeDocument(d),
		Revision: decodeRevision(d),
	}

	sectionCount := d.readCount(
		"section count", document.MaxSectionsPerRevision, minSectionBytes)
	if d.err == nil && sectionCount > 0 {
		record.Sections = make([]document.Section, sectionCount)
		for i := range record.Sections {
			record.Sections[i] = decodeSection(d)
		}
	}

	spanCount := d.readCount(
		"span count", document.MaxSpansPerRevision, minSpanBytes)
	if d.err == nil && spanCount > 0 {
		record.Spans = make([]document.Span, spanCount)
		for i := range record.Spans {
			record.Spans[i] = decodeSpan(d)
		}
	}

	nodeCount := d.readCount(
		"graph node count", MaxGraphNodesPerRecord, minNodeBytes)
	if d.err == nil && nodeCount > 0 {
		record.Nodes = make([]graph.Node, nodeCount)
		for i := range record.Nodes {
			record.Nodes[i] = decodeNode(d)
		}
	}

	edgeCount := d.readCount(
		"graph edge count", MaxGraphEdgesPerRecord, minEdgeBytes)
	if d.err == nil && edgeCount > 0 {
		record.Edges = make([]graph.Edge, edgeCount)
		for i := range record.Edges {
			record.Edges[i] = decodeEdge(d)
		}
	}
	return record
}

func decodePublication(d *decoder) *PublicationV1 {
	switch marker := d.readByte("publication presence"); marker {
	case 0:
		return nil
	case 1:
		return &PublicationV1{
			Sequence:    d.readUint64("publication sequence"),
			PublishedAt: decodeTime(d, "publication time"),
		}
	default:
		d.err = invalid("canonical record has invalid publication presence")
		return nil
	}
}

func decodeDocument(d *decoder) document.Document {
	return document.Document{
		ID:         d.readID("document ID"),
		RevisionID: d.readID("document revision ID"),
		Title: d.readString(
			"document title", shoal.MaxSemanticStringBytes),
		RootSectionID: d.readID("document root section ID"),
		Metadata:      decodeMetadata(d, "document metadata"),
	}
}

func decodeRevision(d *decoder) document.Revision {
	return document.Revision{
		ID:         d.readID("revision ID"),
		DocumentID: d.readID("revision document ID"),
		CreatedAt:  decodeTime(d, "revision created_at"),
		SourceVersion: d.readString(
			"revision source version", shoal.MaxSemanticStringBytes),
		Metadata: decodeMetadata(d, "revision metadata"),
	}
}

func decodeSection(d *decoder) document.Section {
	return document.Section{
		ID:         d.readID("section ID"),
		DocumentID: d.readID("section document ID"),
		RevisionID: d.readID("section revision ID"),
		ParentID:   d.readID("section parent ID"),
		Order:      d.readUint32("section order"),
		Heading: d.readString(
			"section heading", shoal.MaxSemanticStringBytes),
		Range:    decodeRange(d, "section range"),
		Metadata: decodeMetadata(d, "section metadata"),
	}
}

func decodeSpan(d *decoder) document.Span {
	return document.Span{
		ID:         d.readID("span ID"),
		DocumentID: d.readID("span document ID"),
		RevisionID: d.readID("span revision ID"),
		SectionID:  d.readID("span section ID"),
		Order:      d.readUint32("span order"),
		Range:      decodeRange(d, "span range"),
		Text: d.readString(
			"span text", document.MaxRevisionSourceBytes),
		Metadata: decodeMetadata(d, "span metadata"),
	}
}

func decodeNode(d *decoder) graph.Node {
	node := graph.Node{
		ID: d.readID("graph node ID"),
		Kind: d.readString(
			"graph node kind", shoal.MaxSemanticStringBytes),
	}
	labelCount := d.readCount(
		"graph node label count", graph.MaxNodeLabels, 4)
	if d.err == nil && labelCount > 0 {
		node.Labels = make([]string, labelCount)
		for i := range node.Labels {
			node.Labels[i] = d.readString(
				"graph node label", graph.MaxNodeLabelBytes)
		}
	}
	node.Properties = decodeMetadata(d, "graph node properties")
	return node
}

func decodeEdge(d *decoder) graph.Edge {
	return graph.Edge{
		ID:   d.readID("graph edge ID"),
		From: d.readID("graph edge from"),
		To:   d.readID("graph edge to"),
		Type: d.readString(
			"graph edge type", shoal.MaxSemanticStringBytes),
		Weight: shoal.Score(math.Float64frombits(
			d.readUint64("graph edge weight"))),
		Properties: decodeMetadata(d, "graph edge properties"),
	}
}

func decodeRange(d *decoder, name string) document.SourceRange {
	return document.SourceRange{
		Start: document.SourcePosition{
			Offset: d.readNonNegativeInt64(name + " start offset"),
			Page:   d.readNonNegativeInt32(name + " start page"),
		},
		End: document.SourcePosition{
			Offset: d.readNonNegativeInt64(name + " end offset"),
			Page:   d.readNonNegativeInt32(name + " end page"),
		},
	}
}

func decodeTime(d *decoder, name string) time.Time {
	switch marker := d.readByte(name + " presence"); marker {
	case 0:
		return time.Time{}
	case 1:
		// Unix seconds are a signed two's-complement field. Negative values
		// are required for supported timestamps before 1970.
		seconds := d.readInt64(name + " seconds")
		nanoseconds := d.readUint32(name + " nanoseconds")
		if d.err != nil {
			return time.Time{}
		}
		if nanoseconds >= 1_000_000_000 {
			d.err = invalid(name + " has invalid nanoseconds")
			return time.Time{}
		}
		value := time.Unix(seconds, int64(nanoseconds)).UTC()
		if err := validateTime(name, value); err != nil {
			d.err = err
			return time.Time{}
		}
		return value
	default:
		d.err = invalid(name + " has invalid presence")
		return time.Time{}
	}
}

func decodeMetadata(d *decoder, name string) shoal.Metadata {
	count := d.readCount(
		name+" entry count", shoal.MaxMetadataEntries, minMetadataEntryBytes)
	if d.err != nil || count == 0 {
		return nil
	}
	metadata := make(shoal.Metadata, count)
	total := 0
	for range count {
		key := d.readString(name+" key", shoal.MaxMetadataKeyBytes)
		value := d.readString(name+" value", shoal.MaxMetadataValueBytes)
		if d.err != nil {
			return nil
		}
		if _, duplicate := metadata[key]; duplicate {
			d.err = invalid(name + " contains a duplicate key")
			return nil
		}
		total += len(key) + len(value)
		if total > shoal.MaxMetadataBytes {
			d.err = invalid(name + " exceeds its aggregate byte bound")
			return nil
		}
		metadata[key] = value
	}
	return metadata
}
