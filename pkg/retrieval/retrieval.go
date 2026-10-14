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

// Package retrieval defines transport-neutral knowledge retrieval contracts.
package retrieval

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	DefaultTopK         uint32 = 20
	MaxTopK             uint32 = 1000
	MaxQueryBytes              = 16 * 1024
	MaxScopeIDs                = 10_000
	MaxModes                   = 4
	MaxExplanationBytes        = 16 * 1024

	// MaxIdempotencyTokenBytes is reserved for future additive generic
	// mutation APIs. Retrieval requests do not carry such a token.
	MaxIdempotencyTokenBytes = 128
)

// Mode selects a retrieval strategy. Multiple modes request a hybrid plan.
type Mode string

const (
	ModeLexical Mode = "lexical"
	ModeVector  Mode = "vector"
	ModeTree    Mode = "tree"
	ModeGraph   Mode = "graph"
)

// Scope bounds retrieval to known documents or graph nodes. IDs are ORed
// within each list. When both lists are nonempty, candidates must satisfy both
// dimensions through a canonical document/graph association.
type Scope struct {
	DocumentIDs []shoal.ID
	NodeIDs     []shoal.ID
}

// Request describes one coarse knowledge retrieval operation.
type Request struct {
	Text    string
	TopK    uint32
	Modes   []Mode
	Scope   Scope
	AsOf    time.Time
	Explain bool
}

// Normalize returns an independently owned, canonical request and never
// mutates caller slices. It applies public defaults, removes duplicate modes
// and scope IDs while preserving first occurrence, and normalizes AsOf to UTC.
func (r Request) Normalize() (Request, error) {
	normalized := r
	if normalized.TopK == 0 {
		normalized.TopK = DefaultTopK
	}
	if len(r.Modes) == 0 {
		normalized.Modes = []Mode{ModeLexical}
	} else {
		normalized.Modes = deduplicateModes(r.Modes)
	}
	normalized.Scope = Scope{
		DocumentIDs: deduplicateIDs(r.Scope.DocumentIDs),
		NodeIDs:     deduplicateIDs(r.Scope.NodeIDs),
	}
	if !r.AsOf.IsZero() {
		normalized.AsOf = r.AsOf.UTC()
	}
	if err := normalized.validateNormalized(); err != nil {
		return Request{}, err
	}
	return normalized, nil
}

// Validate checks transport-independent request invariants after public
// normalization.
func (r Request) Validate() error {
	_, err := r.Normalize()
	return err
}

func (r Request) validateNormalized() error {
	if !utf8.ValidString(r.Text) {
		return shoal.NewError(shoal.ErrorInvalidArgument, "retrieval text must be valid UTF-8")
	}
	if strings.TrimSpace(r.Text) == "" {
		return shoal.NewError(shoal.ErrorInvalidArgument, "retrieval text is required")
	}
	if len(r.Text) > MaxQueryBytes {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "retrieval text exceeds the public byte bound")
	}
	if r.TopK == 0 || r.TopK > MaxTopK {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "retrieval top_k is outside the public bound")
	}
	if len(r.Modes) == 0 || len(r.Modes) > MaxModes {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "retrieval mode count is outside the public bound")
	}
	for _, mode := range r.Modes {
		if !validMode(mode) {
			return shoal.NewError(shoal.ErrorInvalidArgument, "unknown retrieval mode")
		}
	}
	if len(r.Scope.DocumentIDs)+len(r.Scope.NodeIDs) > MaxScopeIDs {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "retrieval scope has too many IDs")
	}
	for _, id := range r.Scope.DocumentIDs {
		if err := shoal.ValidateRequiredID("retrieval document ID", id); err != nil {
			return err
		}
	}
	for _, id := range r.Scope.NodeIDs {
		if err := shoal.ValidateRequiredID("retrieval node ID", id); err != nil {
			return err
		}
	}
	if !r.AsOf.IsZero() {
		year := r.AsOf.Year()
		if year < 1 || year > 9999 {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "retrieval as_of is outside the supported range")
		}
	}
	return nil
}

// ValidateSeedPlan rejects tree or graph shapes that have no bounded seed
// source under the fixed lexical, tree, then graph planner. The boolean states
// whether document-to-graph association seeds are implemented by the target.
func (r Request) ValidateSeedPlan(documentAssociationSeeds bool) error {
	normalized, err := r.Normalize()
	if err != nil {
		return err
	}
	hasLexical := normalized.HasMode(ModeLexical)
	hasVector := normalized.HasMode(ModeVector)
	hasTree := normalized.HasMode(ModeTree)
	hasGraph := normalized.HasMode(ModeGraph)

	treeSeeded := len(normalized.Scope.DocumentIDs) > 0 || hasLexical || hasVector
	if hasTree && !treeSeeded {
		return shoal.NewError(
			shoal.ErrorUnavailable,
			"tree retrieval requires document scope or earlier lexical/vector seeds",
		)
	}
	graphSeeded := len(normalized.Scope.NodeIDs) > 0 ||
		(documentAssociationSeeds && len(normalized.Scope.DocumentIDs) > 0) ||
		hasLexical || hasVector || (hasTree && treeSeeded)
	if hasGraph && !graphSeeded {
		return shoal.NewError(
			shoal.ErrorUnavailable,
			"graph retrieval requires node scope, document associations, or earlier seeds",
		)
	}
	return nil
}

// HasMode reports whether a request contains a mode.
func (r Request) HasMode(mode Mode) bool {
	for _, candidate := range r.Modes {
		if candidate == mode {
			return true
		}
	}
	return false
}

// Evidence ties a result to immutable source and, when applicable, the graph
// path used to reach it.
type Evidence struct {
	Citation document.Citation
	Quote    string
	Path     graph.Path
	Score    shoal.Score
}

// Validate checks transport-neutral evidence structure. Quote equality is a
// document-context check performed against canonical retained source bytes.
func (e Evidence) Validate() error {
	if err := e.Citation.Validate(); err != nil {
		return err
	}
	if err := shoal.ValidateFiniteScore("evidence score", e.Score); err != nil {
		return err
	}
	if pathPresent(e.Path) {
		if err := e.Path.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// Explanation describes why a result was selected without exposing an
// execution engine or storage plan.
type Explanation struct {
	Modes   []Mode
	Summary string
	Scores  map[string]shoal.Score
}

// Result is one ranked, evidence-addressable retrieval result.
type Result struct {
	ID          shoal.ID
	Score       shoal.Score
	Evidence    []Evidence
	Explanation *Explanation
}

// Response is the complete, all-or-error result of one retrieval request.
type Response struct {
	RequestID         shoal.ID
	EmbeddingSpaceID  shoal.ID
	EmbeddingSpaceIDs []shoal.ID
	Results           []Result
}

// ValidateFor checks result bounds, uniqueness, finite scores, and normative
// deterministic result/evidence ordering for a normalized request.
func (r Response) ValidateFor(request Request) error {
	normalized, err := request.Normalize()
	if err != nil {
		return err
	}
	if err := shoal.ValidateOptionalID("retrieval request ID", r.RequestID); err != nil {
		return err
	}
	if err := shoal.ValidateOptionalID(
		"retrieval embedding space ID", r.EmbeddingSpaceID,
	); err != nil {
		return err
	}
	if normalized.HasMode(ModeVector) {
		if (r.EmbeddingSpaceID == "") != (len(r.EmbeddingSpaceIDs) == 0) {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"vector retrieval embedding space aggregate and constituents "+
					"must be present together",
			)
		}
		if len(normalized.Modes) == 1 && len(r.Results) > 0 &&
			r.EmbeddingSpaceID == "" {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"vector-only retrieval results require embedding space provenance",
			)
		}
	} else if r.EmbeddingSpaceID != "" || len(r.EmbeddingSpaceIDs) > 0 {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"non-vector retrieval response cannot carry embedding space provenance",
		)
	}
	if len(r.EmbeddingSpaceIDs) > MaxScopeIDs {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"retrieval embedding space constituents exceed the public bound",
		)
	}
	for index, id := range r.EmbeddingSpaceIDs {
		if err := shoal.ValidateRequiredID(
			"retrieval embedding space constituent ID", id,
		); err != nil {
			return err
		}
		if index > 0 &&
			shoal.CompareID(r.EmbeddingSpaceIDs[index-1], id) >= 0 {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"retrieval embedding space constituent IDs must be unique "+
					"and canonically ordered",
			)
		}
	}
	if len(r.EmbeddingSpaceIDs) > 0 {
		expected, err := EmbeddingSpaceSetID(r.EmbeddingSpaceIDs...)
		if err != nil {
			return err
		}
		if expected != r.EmbeddingSpaceID {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"retrieval embedding space set identity is not canonical",
			)
		}
	}
	if uint32(len(r.Results)) > normalized.TopK {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "retrieval response exceeds normalized top_k")
	}

	seen := make(map[shoal.ID]struct{}, len(r.Results))
	for index, result := range r.Results {
		if err := shoal.ValidateRequiredID("retrieval result ID", result.ID); err != nil {
			return err
		}
		if _, duplicate := seen[result.ID]; duplicate {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "retrieval response has duplicate result IDs")
		}
		seen[result.ID] = struct{}{}
		if err := shoal.ValidateFiniteScore("retrieval result score", result.Score); err != nil {
			return err
		}
		if index > 0 && CompareResult(r.Results[index-1], result) > 0 {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "retrieval results are not deterministically ordered")
		}
		for evidenceIndex, evidence := range result.Evidence {
			if err := evidence.Validate(); err != nil {
				return err
			}
			if evidenceIndex > 0 {
				compared := CompareEvidence(result.Evidence[evidenceIndex-1], evidence)
				if compared > 0 {
					return shoal.NewError(
						shoal.ErrorInvalidArgument,
						"retrieval evidence is not deterministically ordered",
					)
				}
				if compared == 0 {
					return shoal.NewError(
						shoal.ErrorInvalidArgument,
						"retrieval evidence has a duplicate ordering key",
					)
				}
			}
		}
		if result.Explanation != nil {
			if err := validateExplanation(*result.Explanation); err != nil {
				return err
			}
		}
	}
	return nil
}

// EmbeddingSpaceIdentityID hashes one provider-defined embedding-space
// identity into a stable opaque constituent ID.
func EmbeddingSpaceIdentityID(identity string) (shoal.ID, error) {
	if !utf8.ValidString(identity) || strings.TrimSpace(identity) == "" ||
		len(identity) > shoal.MaxSemanticStringBytes {
		return "", shoal.NewError(
			shoal.ErrorInvalidArgument,
			"embedding space identity is invalid",
		)
	}
	hash := sha256.New()
	var length [8]byte
	write := func(value string) {
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(value))
	}
	write("shoal-retrieval-embedding-space-v1")
	write(identity)
	return shoal.ID(
		"embedding-space_" + hex.EncodeToString(hash.Sum(nil)[:16]),
	), nil
}

// EmbeddingSpaceSetID returns a stable opaque identity for an exact set of
// canonical constituent embedding-space IDs.
func EmbeddingSpaceSetID(identities ...shoal.ID) (shoal.ID, error) {
	if len(identities) > MaxScopeIDs {
		return "", shoal.NewError(
			shoal.ErrorInvalidArgument,
			"embedding space constituents exceed the public bound",
		)
	}
	normalized := append([]shoal.ID(nil), identities...)
	for _, identity := range normalized {
		if err := shoal.ValidateRequiredID(
			"embedding space constituent ID", identity,
		); err != nil {
			return "", err
		}
	}
	sort.Slice(normalized, func(i, j int) bool {
		return shoal.CompareID(normalized[i], normalized[j]) < 0
	})
	unique := normalized[:0]
	for _, identity := range normalized {
		if len(unique) > 0 && unique[len(unique)-1] == identity {
			continue
		}
		unique = append(unique, identity)
	}
	if len(unique) == 0 {
		return "", nil
	}
	hash := sha256.New()
	var length [8]byte
	write := func(value string) {
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(value))
	}
	write("shoal-retrieval-embedding-space-set-v1")
	for _, identity := range unique {
		write(string(identity))
	}
	return shoal.ID(
		"embedding-space_" + hex.EncodeToString(hash.Sum(nil)[:16]),
	), nil
}

// CompareResult returns a negative value when left sorts before right under
// score-descending, raw-result-ID-ascending order.
func CompareResult(left, right Result) int {
	switch {
	case left.Score > right.Score:
		return -1
	case left.Score < right.Score:
		return 1
	default:
		return shoal.CompareID(left.ID, right.ID)
	}
}

// CompareEvidence compares the public evidence ordering key: score descending,
// citation tuple, then directed path tuple. A response must not contain two
// evidence values with the same ordering key.
func CompareEvidence(left, right Evidence) int {
	switch {
	case left.Score > right.Score:
		return -1
	case left.Score < right.Score:
		return 1
	}
	if compared := compareCitation(left.Citation, right.Citation); compared != 0 {
		return compared
	}
	return comparePath(left.Path, right.Path)
}

// SortResults orders results and each result's evidence by the public total
// orders. It mutates the supplied response slices but not nested values.
func SortResults(response *Response) {
	if response == nil {
		return
	}
	sort.SliceStable(response.Results, func(i, j int) bool {
		return CompareResult(response.Results[i], response.Results[j]) < 0
	})
	for index := range response.Results {
		sort.SliceStable(response.Results[index].Evidence, func(i, j int) bool {
			return CompareEvidence(
				response.Results[index].Evidence[i],
				response.Results[index].Evidence[j],
			) < 0
		})
	}
}

// Retriever is implemented by embedded and remote Shoal clients.
type Retriever interface {
	Retrieve(context.Context, Request) (Response, error)
}

func deduplicateModes(modes []Mode) []Mode {
	result := make([]Mode, 0, len(modes))
	seen := make(map[Mode]struct{}, len(modes))
	for _, mode := range modes {
		if _, duplicate := seen[mode]; duplicate {
			continue
		}
		seen[mode] = struct{}{}
		result = append(result, mode)
	}
	return result
}

func deduplicateIDs(ids []shoal.ID) []shoal.ID {
	if len(ids) == 0 {
		return nil
	}
	result := make([]shoal.ID, 0, len(ids))
	seen := make(map[shoal.ID]struct{}, len(ids))
	for _, id := range ids {
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result
}

func validMode(mode Mode) bool {
	switch mode {
	case ModeLexical, ModeVector, ModeTree, ModeGraph:
		return true
	default:
		return false
	}
}

func validateExplanation(explanation Explanation) error {
	if len(explanation.Summary) > MaxExplanationBytes {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "explanation summary exceeds the public byte bound")
	}
	for _, mode := range explanation.Modes {
		if !validMode(mode) {
			return shoal.NewError(
				shoal.ErrorInvalidArgument, "explanation contains an unknown mode")
		}
	}
	for name, score := range explanation.Scores {
		if err := shoal.ValidateSemanticString("explanation score name", name); err != nil {
			return err
		}
		if err := shoal.ValidateFiniteScore("explanation score", score); err != nil {
			return err
		}
	}
	return nil
}

func compareCitation(left, right document.Citation) int {
	for _, pair := range [][2]shoal.ID{
		{left.DocumentID, right.DocumentID},
		{left.RevisionID, right.RevisionID},
		{left.SectionID, right.SectionID},
		{left.SpanID, right.SpanID},
	} {
		if compared := shoal.CompareID(pair[0], pair[1]); compared != 0 {
			return compared
		}
	}
	switch {
	case left.Range.Start.Offset < right.Range.Start.Offset:
		return -1
	case left.Range.Start.Offset > right.Range.Start.Offset:
		return 1
	case left.Range.End.Offset < right.Range.End.Offset:
		return -1
	case left.Range.End.Offset > right.Range.End.Offset:
		return 1
	default:
		return 0
	}
}

func comparePath(left, right graph.Path) int {
	for index := 0; index < len(left.Nodes) && index < len(right.Nodes); index++ {
		if compared := shoal.CompareID(left.Nodes[index].ID, right.Nodes[index].ID); compared != 0 {
			return compared
		}
	}
	if len(left.Nodes) < len(right.Nodes) {
		return -1
	}
	if len(left.Nodes) > len(right.Nodes) {
		return 1
	}
	for index := 0; index < len(left.Edges) && index < len(right.Edges); index++ {
		if compared := shoal.CompareID(left.Edges[index].ID, right.Edges[index].ID); compared != 0 {
			return compared
		}
	}
	switch {
	case len(left.Edges) < len(right.Edges):
		return -1
	case len(left.Edges) > len(right.Edges):
		return 1
	default:
		return 0
	}
}

func pathPresent(path graph.Path) bool {
	return len(path.Nodes) > 0 || len(path.Edges) > 0
}
