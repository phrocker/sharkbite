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

package inference

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func deriveID(namespace string, parts ...string) shoal.ID {
	digest := sha256.New()
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(parts)))
	_, _ = digest.Write(length[:])
	for _, part := range parts {
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		_, _ = digest.Write(length[:])
		_, _ = digest.Write([]byte(part))
	}
	return shoal.ID(namespace + ":" + hex.EncodeToString(digest.Sum(nil)))
}

func canonicalParts(parts ...string) string {
	var builder strings.Builder
	for _, part := range parts {
		builder.WriteString(strconv.Itoa(len(part)))
		builder.WriteByte(':')
		builder.WriteString(part)
	}
	return builder.String()
}

func canonicalCitation(citation document.Citation) string {
	return canonicalParts(
		string(citation.DocumentID),
		string(citation.RevisionID),
		string(citation.SectionID),
		string(citation.SpanID),
		strconv.FormatInt(citation.Range.Start.Offset, 10),
		strconv.FormatInt(int64(citation.Range.Start.Page), 10),
		strconv.FormatInt(citation.Range.End.Offset, 10),
		strconv.FormatInt(int64(citation.Range.End.Page), 10),
	)
}

func canonicalPath(path graph.Path) string {
	nodes := make([]string, len(path.Nodes))
	for index, node := range path.Nodes {
		nodes[index] = canonicalParts(
			string(node.ID),
			node.Kind,
			canonicalParts(node.Labels...),
			canonicalMetadata(node.Properties),
		)
	}
	edges := make([]string, len(path.Edges))
	for index, edge := range path.Edges {
		edges[index] = canonicalParts(
			string(edge.ID),
			string(edge.From),
			string(edge.To),
			edge.Type,
			canonicalScore(edge.Weight),
			canonicalMetadata(edge.Properties),
		)
	}
	return canonicalParts(canonicalParts(nodes...), canonicalParts(edges...))
}

func canonicalMetadata(metadata shoal.Metadata) string {
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys)*2)
	for _, key := range keys {
		parts = append(parts, key, metadata[key])
	}
	return canonicalParts(parts...)
}

func canonicalScore(score shoal.Score) string {
	return strconv.FormatFloat(float64(score), 'g', -1, 64)
}

func canonicalTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func canonicalValue(value ontology.Value) string {
	switch value.Type() {
	case ontology.ValueString:
		item, _ := value.StringValue()
		return canonicalParts(string(value.Type()), item)
	case ontology.ValueInteger:
		item, _ := value.IntegerValue()
		return canonicalParts(string(value.Type()), strconv.FormatInt(item, 10))
	case ontology.ValueNumber:
		item, _ := value.NumberValue()
		return canonicalParts(
			string(value.Type()),
			strconv.FormatFloat(item, 'g', -1, 64),
		)
	case ontology.ValueBoolean:
		item, _ := value.BooleanValue()
		return canonicalParts(string(value.Type()), strconv.FormatBool(item))
	case ontology.ValueTimestamp:
		item, _ := value.TimestampValue()
		return canonicalParts(string(value.Type()), canonicalTime(item))
	case ontology.ValueReference:
		item, _ := value.ReferenceValue()
		return canonicalParts(string(value.Type()), string(item))
	default:
		return ""
	}
}

func canonicalModel(model ModelProvenance) string {
	seed := ""
	if model.hasSeed {
		seed = strconv.FormatInt(model.seed, 10)
	}
	return canonicalParts(
		model.provider,
		model.model,
		model.version,
		canonicalMetadata(model.parameters),
		strconv.FormatBool(model.hasSeed),
		seed,
	)
}

func canonicalPrompt(prompt PromptProvenance) string {
	return canonicalParts(prompt.templateID, prompt.version, prompt.hash)
}

func validateCitation(citation document.Citation) error {
	if err := citation.Validate(); err != nil {
		return err
	}
	return nil
}

func validatePath(path graph.Path) error {
	if len(path.Nodes) > MaxPathNodes || len(path.Edges) > MaxPathEdges {
		return invalid("graph evidence path exceeds the public count bound")
	}
	if err := path.Validate(); err != nil {
		return err
	}
	for _, node := range path.Nodes {
		if !utf8.ValidString(node.Kind) {
			return invalid("graph path node kind must be valid UTF-8")
		}
		for index, label := range node.Labels {
			if !utf8.ValidString(label) {
				return invalid("graph path node label must be valid UTF-8")
			}
			if index > 0 && node.Labels[index-1] >= label {
				return invalid("graph path node labels must be unique and canonically ordered")
			}
		}
		if err := validateMetadata("graph path node properties", node.Properties); err != nil {
			return err
		}
	}
	for _, edge := range path.Edges {
		if !utf8.ValidString(edge.Type) {
			return invalid("graph path edge type must be valid UTF-8")
		}
		if err := validateMetadata("graph path edge properties", edge.Properties); err != nil {
			return err
		}
	}
	if pathPayloadBytes(path) > MaxContextPackBytes {
		return invalid("graph evidence path exceeds the public byte bound")
	}
	return nil
}

func validateMetadata(name string, metadata shoal.Metadata) error {
	if err := shoal.ValidateMetadata(name, metadata); err != nil {
		return err
	}
	for key, value := range metadata {
		if !utf8.ValidString(key) || !utf8.ValidString(value) {
			return invalid(name + " must contain valid UTF-8")
		}
	}
	return nil
}

func validateRequiredString(name, value string, maximum int) error {
	if !utf8.ValidString(value) {
		return invalid(name + " must be valid UTF-8")
	}
	if strings.TrimSpace(value) == "" {
		return invalid(name + " is required")
	}
	if len(value) > maximum {
		return invalid(name + " exceeds the public byte bound")
	}
	return nil
}

func validateOptionalString(name, value string, maximum int) error {
	if !utf8.ValidString(value) {
		return invalid(name + " must be valid UTF-8")
	}
	if len(value) > maximum {
		return invalid(name + " exceeds the public byte bound")
	}
	return nil
}

func validateSHA256(name, value string) error {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) {
		return invalid(name + " must be a canonical SHA-256 digest")
	}
	encoded := strings.TrimPrefix(value, prefix)
	if len(encoded) != sha256.Size*2 {
		return invalid(name + " must be a canonical SHA-256 digest")
	}
	decoded, err := hex.DecodeString(encoded)
	if err != nil || hex.EncodeToString(decoded) != encoded {
		return invalid(name + " must be a canonical SHA-256 digest")
	}
	return nil
}

func validateTime(name string, value time.Time) error {
	if value.IsZero() {
		return invalid(name + " is required")
	}
	year := value.UTC().Year()
	if year < 1 || year > 9999 {
		return invalid(name + " is outside the supported range")
	}
	return nil
}

func normalizeTime(value time.Time) time.Time {
	if value.IsZero() {
		return value
	}
	return value.UTC()
}

func normalizeQuery(query string) string {
	return strings.Join(strings.Fields(query), " ")
}

func cloneMetadata(metadata shoal.Metadata) shoal.Metadata {
	if metadata == nil {
		return nil
	}
	cloned := make(shoal.Metadata, len(metadata))
	for key, value := range metadata {
		cloned[key] = value
	}
	return cloned
}

func metadataBytes(metadata shoal.Metadata) int {
	total := 0
	for key, value := range metadata {
		total += len(key) + len(value)
	}
	return total
}

func pathPayloadBytes(path graph.Path) int {
	total := 0
	for _, node := range path.Nodes {
		total += len(node.ID) + len(node.Kind) + metadataBytes(node.Properties)
		for _, label := range node.Labels {
			total += len(label)
		}
	}
	for _, edge := range path.Edges {
		total += len(edge.ID) + len(edge.From) + len(edge.To) + len(edge.Type) + 8
		total += metadataBytes(edge.Properties)
	}
	return total
}

func anchorPayloadBytes(anchor EvidenceAnchor) int {
	if anchor.kind == AnchorDocument {
		return len(anchor.id) + len(anchor.quote) +
			len(anchor.citation.DocumentID) + len(anchor.citation.RevisionID) +
			len(anchor.citation.SectionID) + len(anchor.citation.SpanID) + 24
	}
	return len(anchor.id) + pathPayloadBytes(anchor.path)
}

func claimPayloadBytes(claim Claim) int {
	total := len(claim.id) + len(claim.subject) + len(claim.predicate)
	total += len(canonicalValue(claim.object)) + 8
	for _, id := range claim.evidenceIDs {
		total += len(id)
	}
	total += len(canonicalModel(claim.model)) + len(canonicalPrompt(claim.prompt))
	return total + metadataBytes(claim.metadata)
}

func issuePayloadBytes(issue Issue) int {
	total := len(issue.id) + len(issue.kind) + len(issue.input) + len(issue.reason)
	for _, id := range issue.evidenceIDs {
		total += len(id)
	}
	return total
}

func clonePath(path graph.Path) graph.Path {
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

func canonicalizePath(path graph.Path) graph.Path {
	cloned := clonePath(path)
	for index := range cloned.Nodes {
		sort.Strings(cloned.Nodes[index].Labels)
	}
	return cloned
}

func cloneAnchors(anchors []EvidenceAnchor) []EvidenceAnchor {
	if anchors == nil {
		return nil
	}
	cloned := make([]EvidenceAnchor, len(anchors))
	for index, anchor := range anchors {
		cloned[index] = anchor.clone()
	}
	return cloned
}

func cloneClaims(claims []Claim) []Claim {
	if claims == nil {
		return nil
	}
	cloned := make([]Claim, len(claims))
	for index, claim := range claims {
		cloned[index] = claim.clone()
	}
	return cloned
}

func cloneIssues(issues []Issue) []Issue {
	if issues == nil {
		return nil
	}
	cloned := make([]Issue, len(issues))
	for index, issue := range issues {
		cloned[index] = issue
		cloned[index].evidenceIDs = append([]shoal.ID(nil), issue.evidenceIDs...)
	}
	return cloned
}

func filterIssues(issues []Issue, kind IssueKind) []Issue {
	filtered := make([]Issue, 0, len(issues))
	for _, issue := range issues {
		if issue.kind == kind {
			cloned := issue
			cloned.evidenceIDs = append([]shoal.ID(nil), issue.evidenceIDs...)
			filtered = append(filtered, cloned)
		}
	}
	return filtered
}

func citationPresent(citation document.Citation) bool {
	return citation.DocumentID != "" ||
		citation.RevisionID != "" ||
		citation.SectionID != "" ||
		citation.SpanID != "" ||
		citation.Range.Start.Offset != 0 ||
		citation.Range.Start.Page != 0 ||
		citation.Range.End.Offset != 0 ||
		citation.Range.End.Page != 0
}

func pathPresent(path graph.Path) bool {
	return len(path.Nodes) != 0 || len(path.Edges) != 0
}
