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
	"bytes"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	LatentLinkDefaultColumnFamily = "link"
	LatentLinkDefaultIteratorName = "LatentEdgeDiscoveryIterator"
	LatentLinkSimilarityMetric    = "cosine"
)

const (
	latentLinkOptionSimilarityThreshold = "similarityThreshold"
	latentLinkOptionMaxPairsPerCell     = "maxPairsPerCell"
	latentLinkOptionMaxCellBuffer       = "maxCellBuffer"
	latentLinkOptionEdgeCF              = "edgeCF"
	latentLinkOptionEmbeddingCF         = "embeddingCF"
	latentLinkOptionEmbeddingCQ         = "embeddingCQ"
)

// LatentLinkCell is the storage-neutral shape of a link cell emitted by the
// latent-edge iterator into the graph index.
type LatentLinkCell struct {
	Row             []byte
	ColumnFamily    []byte
	ColumnQualifier []byte
	// ColumnVisibility is the compound cell visibility the latent-edge
	// iterator derived from the two embeddings. ProjectLatentLinkAssertions
	// refuses to project a cell that carries one, because nothing downstream
	// of the projection can enforce it (#569).
	ColumnVisibility []byte
	Timestamp        int64
	Deleted          bool
	Value            []byte
}

// LatentLinkAssertionProjection supplies the run-level provenance needed to
// turn latent link cells into derived ontology relationship assertions.
type LatentLinkAssertionProjection struct {
	EmbeddingModel        string
	EmbeddingModelVersion string
	SimilarityThreshold   shoal.Score
	Predicate             shoal.ID
	SubjectType           shoal.ID
	ObjectType            shoal.ID
	IteratorName          string
	IteratorOptions       shoal.Metadata
	LinkColumnFamily      string
	Provenance            ontology.ExtractionProvenance
	Ontology              ontology.OntologyIdentity
	AssertionMetadata     shoal.Metadata
	EvidenceMetadata      shoal.Metadata
}

// ProjectLatentLinkAssertions turns latent-edge link cells into derived
// ontology assertions. Non-link, deleted and labelled cells are ignored so
// callers can pass a mixed graph-index scan without pre-filtering.
func ProjectLatentLinkAssertions(
	cells []LatentLinkCell,
	projection LatentLinkAssertionProjection,
) ([]ontology.Assertion, error) {
	normalized, err := normalizeLatentProjection(projection)
	if err != nil {
		return nil, err
	}
	ordered := append([]LatentLinkCell(nil), cells...)
	// Load-bearing: TestProjectLatentLinkAssertionsDeterministicOrder pins that
	// caller insertion order cannot leak into assertion ordering.
	sort.SliceStable(ordered, func(i, j int) bool {
		return compareLatentLinkCells(ordered[i], ordered[j]) < 0
	})

	assertions := make([]ontology.Assertion, 0, len(ordered))
	for _, cell := range ordered {
		if !bytes.Equal(cell.ColumnFamily, []byte(normalized.LinkColumnFamily)) {
			// Load-bearing: TestProjectLatentLinkAssertionsSkipsNonLinkCells pins
			// that mixed graph scans do not try to project embedding cells.
			continue
		}
		if cell.Deleted {
			// Load-bearing: TestProjectLatentLinkAssertionsSkipsDeletedLinkCells
			// pins that tombstoned link cells do not become positive assertions.
			continue
		}
		if len(cell.ColumnVisibility) != 0 {
			// A labelled relationship must not become an unlabelled assertion
			// (#569). LatentEdgeDiscoveryIterator writes the compound
			// visibility of the two embeddings onto the link cell, and nothing
			// past this point carries it: the derivation, the evidence ref and
			// the assertion have no visibility, and computeCurrentGraph adds
			// the result as a graph edge gated only by its endpoints'
			// AccessRules. So a reader authorized on both endpoints would
			// otherwise see the labelled relationship and its similarity
			// score, derived from embeddings whose labels it does not hold.
			//
			// Dropped rather than enforced, and the difference is not a
			// preference. Enforcing means scanning under the reader's
			// authorizations, and the derived graph is cached process-wide in
			// Explorer.graphNodes/graphEdges with no authorization in the key,
			// so the first reader's label set would decide what every later
			// reader sees. Keying that cache per label set is #569's other
			// option and a real design change.
			//
			// Note for whoever revisits this: a cell visibility expression is
			// not the same representation as the structured label policies
			// #570 introduced, which live in node properties and are evaluated
			// through AccessRules. The #623 label evaluator does not apply
			// here, which is why this is a refusal and not a filter.
			//
			// Two costs, both accepted. A reader that does hold the labels
			// also loses the edge, and an operator who attaches the latent
			// iterator over labelled embeddings gets no latent edges and no
			// explanation. Both are over-withholding, which is the safe
			// direction, and PutLatentLinkCells has no production caller today
			// (counted, not sampled), so nothing shipped changes behaviour.
			//
			// Load-bearing: TestProjectLatentLinkAssertionsSkipsLabelledCells
			// and TestLatentLinkGraphReadDropsLabelledCells.
			continue
		}
		tessellationCell, source, target, err := latentLinkEndpoints(cell)
		if err != nil {
			return nil, err
		}
		score, err := latentLinkScore(cell.Value)
		if err != nil {
			return nil, err
		}
		derivation, err := ontology.NewAssertionDerivation(
			normalized.EmbeddingModel,
			normalized.EmbeddingModelVersion,
			LatentLinkSimilarityMetric,
			normalized.SimilarityThreshold,
			tessellationCell,
			score,
			source,
			target,
			normalized.IteratorName,
			normalized.IteratorOptions,
		)
		if err != nil {
			return nil, err
		}
		// Load-bearing: TestProjectLatentLinkAssertionsPreservesDerivationEvidence
		// pins that derived link assertions never fabricate citation evidence.
		evidence, err := ontology.NewDerivationEvidenceRef(
			derivation, normalized.EvidenceMetadata)
		if err != nil {
			return nil, err
		}
		value, err := ontology.NewReferenceValue(target)
		if err != nil {
			return nil, err
		}
		options := []ontology.AssertionOption{}
		if normalized.SubjectType != "" {
			options = append(options, ontology.WithAssertionSubjectType(normalized.SubjectType))
		}
		if normalized.ObjectType != "" {
			options = append(options, ontology.WithAssertionObjectType(normalized.ObjectType))
		}
		if normalized.Ontology.Known() {
			options = append(options, ontology.WithAssertionOntology(normalized.Ontology))
		}
		assertion, err := ontology.NewAssertion(
			source,
			normalized.Predicate,
			value,
			ontology.AssertionDerived,
			score,
			[]ontology.EvidenceRef{evidence},
			normalized.Provenance,
			normalized.AssertionMetadata,
			options...,
		)
		if err != nil {
			return nil, err
		}
		assertions = append(assertions, assertion)
	}
	return assertions, nil
}

func normalizeLatentProjection(
	projection LatentLinkAssertionProjection,
) (LatentLinkAssertionProjection, error) {
	if projection.IteratorName == "" {
		projection.IteratorName = LatentLinkDefaultIteratorName
	}
	projection.IteratorOptions = cloneLatentMetadata(projection.IteratorOptions)
	if projection.LinkColumnFamily == "" {
		projection.LinkColumnFamily = projection.IteratorOptions[latentLinkOptionEdgeCF]
	}
	if projection.LinkColumnFamily == "" {
		projection.LinkColumnFamily = LatentLinkDefaultColumnFamily
	}
	if !utf8.ValidString(projection.LinkColumnFamily) ||
		strings.TrimSpace(projection.LinkColumnFamily) != projection.LinkColumnFamily {
		return LatentLinkAssertionProjection{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "latent link column family is invalid")
	}
	if err := validateLatentOptionConsistency(projection); err != nil {
		return LatentLinkAssertionProjection{}, err
	}
	projection.IteratorOptions = completeLatentIteratorOptions(projection)
	if err := projection.Provenance.Validate(); err != nil {
		return LatentLinkAssertionProjection{}, err
	}
	return projection, nil
}

func validateLatentOptionConsistency(projection LatentLinkAssertionProjection) error {
	// Load-bearing: TestProjectLatentLinkAssertionsRejectsConflictingIteratorOptions
	// pins that selected link cells and rerun options use the same edge CF.
	if projection.IteratorOptions[latentLinkOptionEdgeCF] != "" &&
		projection.IteratorOptions[latentLinkOptionEdgeCF] != projection.LinkColumnFamily {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "latent link column family conflicts with iterator options")
	}
	if projection.IteratorOptions[latentLinkOptionSimilarityThreshold] == "" {
		return nil
	}
	parsed, err := strconv.ParseFloat(
		projection.IteratorOptions[latentLinkOptionSimilarityThreshold], 64)
	if err != nil {
		return shoal.WrapError(
			shoal.ErrorInvalidArgument, "parse latent iterator threshold", err)
	}
	// Load-bearing: TestProjectLatentLinkAssertionsRejectsConflictingIteratorOptions
	// pins that derivation threshold and rerun options cannot contradict.
	if shoal.Score(parsed) != projection.SimilarityThreshold {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "latent iterator threshold conflicts with projection")
	}
	return nil
}

func completeLatentIteratorOptions(
	projection LatentLinkAssertionProjection,
) shoal.Metadata {
	options := cloneLatentMetadata(projection.IteratorOptions)
	if options == nil {
		options = shoal.Metadata{}
	}
	defaults := map[string]string{
		latentLinkOptionSimilarityThreshold: strconv.FormatFloat(
			float64(projection.SimilarityThreshold), 'g', -1, 64),
		latentLinkOptionMaxPairsPerCell: "500",
		latentLinkOptionMaxCellBuffer:   "200",
		latentLinkOptionEdgeCF:          projection.LinkColumnFamily,
		latentLinkOptionEmbeddingCF:     "V",
		latentLinkOptionEmbeddingCQ:     "_embedding",
	}
	// Load-bearing: TestProjectLatentLinkAssertionsPreservesDerivationEvidence and
	// TestProjectLatentLinkAssertionsPreservesCustomLinkColumnFamily pin the
	// iterator options that make the derivation independently rerunnable.
	for key, value := range defaults {
		if options[key] == "" {
			options[key] = value
		}
	}
	return options
}

func latentLinkEndpoints(
	cell LatentLinkCell,
) (string, shoal.ID, shoal.ID, error) {
	if !utf8.Valid(cell.Row) || !utf8.Valid(cell.ColumnQualifier) {
		return "", "", "", shoal.NewError(
			shoal.ErrorInvalidArgument, "latent link endpoint is invalid UTF-8")
	}
	// Load-bearing: TestProjectLatentLinkAssertionsPreservesDerivationEvidence
	// pins first-colon parsing because endpoint IDs themselves may contain ':'.
	tessellationCell, source, found := strings.Cut(string(cell.Row), ":")
	if !found || tessellationCell == "" || source == "" {
		return "", "", "", shoal.NewError(
			shoal.ErrorInvalidArgument, "latent link row is malformed")
	}
	target := string(cell.ColumnQualifier)
	if target == "" {
		return "", "", "", shoal.NewError(
			shoal.ErrorInvalidArgument, "latent link target is required")
	}
	return tessellationCell, shoal.ID(source), shoal.ID(target), nil
}

func latentLinkScore(value []byte) (shoal.Score, error) {
	if !utf8.Valid(value) {
		return 0, shoal.NewError(
			shoal.ErrorInvalidArgument, "latent link score is invalid UTF-8")
	}
	parsed, err := strconv.ParseFloat(string(value), 64)
	if err != nil {
		return 0, shoal.WrapError(
			shoal.ErrorInvalidArgument, "parse latent link score", err)
	}
	// Load-bearing: TestProjectLatentLinkAssertionsPreservesExactScore pins
	// that projection parses the stored score without lossy reformatting.
	score := shoal.Score(parsed)
	if err := shoal.ValidateFiniteScore("latent link score", score); err != nil {
		return 0, err
	}
	return score, nil
}

func compareLatentLinkCells(left, right LatentLinkCell) int {
	if c := bytes.Compare(left.Row, right.Row); c != 0 {
		return c
	}
	if c := bytes.Compare(left.ColumnFamily, right.ColumnFamily); c != 0 {
		return c
	}
	if c := bytes.Compare(left.ColumnQualifier, right.ColumnQualifier); c != 0 {
		return c
	}
	if c := bytes.Compare(left.ColumnVisibility, right.ColumnVisibility); c != 0 {
		return c
	}
	if left.Timestamp != right.Timestamp {
		if right.Timestamp < left.Timestamp {
			return -1
		}
		return 1
	}
	switch {
	case left.Deleted && !right.Deleted:
		return -1
	case !left.Deleted && right.Deleted:
		return 1
	}
	return bytes.Compare(left.Value, right.Value)
}

func cloneLatentMetadata(metadata shoal.Metadata) shoal.Metadata {
	if metadata == nil {
		return nil
	}
	cloned := make(shoal.Metadata, len(metadata))
	for key, value := range metadata {
		cloned[key] = value
	}
	return cloned
}
