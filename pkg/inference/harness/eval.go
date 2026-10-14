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

package harness

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const FixtureEvaluationVersion = "shoal-grounded-inference-eval-v1"

type EvaluationCase struct {
	ID             string
	Pack           inference.ContextPack
	ExpectedClaims []ExpectedClaim
	Sources        map[DocumentRevision]string
	GraphPaths     map[shoal.ID]graph.Path
}

type ExpectedClaim struct {
	Subject     shoal.ID
	Predicate   shoal.ID
	Object      ontology.Value
	EvidenceIDs []shoal.ID
}

type DocumentRevision struct {
	DocumentID shoal.ID
	RevisionID shoal.ID
}

type EvaluationReport struct {
	Version   string                 `json:"version"`
	Cases     []EvaluationCaseReport `json:"cases"`
	Summary   EvaluationSummary      `json:"summary"`
	Generated string                 `json:"generated"`
}

type EvaluationCaseReport struct {
	ID                      string      `json:"id"`
	ContextPackID           string      `json:"context_pack_id"`
	ResultID                string      `json:"result_id,omitempty"`
	StopReason              StopReason  `json:"stop_reason"`
	Iterations              int         `json:"iterations"`
	TraceDigest             string      `json:"trace_digest"`
	Budget                  BudgetUsage `json:"budget"`
	Claims                  int         `json:"claims"`
	ExpectedClaims          int         `json:"expected_claims"`
	SupportedClaims         int         `json:"supported_claims"`
	MissingExpectedClaims   int         `json:"missing_expected_claims"`
	UnsupportedIssues       int         `json:"unsupported_issues"`
	CitationReferences      int         `json:"citation_references"`
	InvalidCitationRefs     int         `json:"invalid_citation_references"`
	GraphPathReferences     int         `json:"graph_path_references"`
	InvalidGraphPathRefs    int         `json:"invalid_graph_path_references"`
	ValidEvidenceReferences int         `json:"valid_evidence_references"`
	InvalidEvidenceRefs     int         `json:"invalid_evidence_references"`
	Error                   string      `json:"error,omitempty"`
}

type EvaluationSummary struct {
	CaseCount               int                `json:"case_count"`
	ClaimCount              int                `json:"claim_count"`
	ExpectedClaimCount      int                `json:"expected_claim_count"`
	SupportedClaimCount     int                `json:"supported_claim_count"`
	MissingExpectedClaims   int                `json:"missing_expected_claims"`
	UnsupportedIssueCount   int                `json:"unsupported_issue_count"`
	GroundingSupportRate    float64            `json:"grounding_support_rate"`
	UnsupportedOutcomeRate  float64            `json:"unsupported_outcome_rate"`
	CitationReferenceCount  int                `json:"citation_reference_count"`
	InvalidCitationRefs     int                `json:"invalid_citation_references"`
	GraphPathReferenceCount int                `json:"graph_path_reference_count"`
	InvalidGraphPathRefs    int                `json:"invalid_graph_path_references"`
	ValidEvidenceReferences int                `json:"valid_evidence_references"`
	InvalidEvidenceRefs     int                `json:"invalid_evidence_references"`
	TotalBudget             BudgetUsage        `json:"total_budget"`
	IterationCounts         []int              `json:"iteration_counts"`
	StopReasons             map[StopReason]int `json:"stop_reasons"`
}

func Evaluate(ctx context.Context, generator *Generator, cases []EvaluationCase, generatedAt time.Time) (EvaluationReport, error) {
	if generator == nil {
		return EvaluationReport{}, invalid("evaluation generator is required")
	}
	ordered := append([]EvaluationCase(nil), cases...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	seen := make(map[string]struct{}, len(ordered))
	for _, tc := range ordered {
		id := strings.TrimSpace(tc.ID)
		if id == "" {
			return EvaluationReport{}, invalid("evaluation case ID is required")
		}
		if _, ok := seen[id]; ok {
			return EvaluationReport{}, invalid("evaluation case ID must be unique")
		}
		seen[id] = struct{}{}
	}
	report := EvaluationReport{
		Version:   FixtureEvaluationVersion,
		Generated: generatedAt.UTC().Format(time.RFC3339Nano),
		Summary:   EvaluationSummary{StopReasons: make(map[StopReason]int)},
	}
	for _, tc := range ordered {
		record, err := generator.Run(ctx, tc.Pack)
		caseReport := evaluateRecord(tc, record, err)
		report.Cases = append(report.Cases, caseReport)
		report.Summary.CaseCount++
		report.Summary.ClaimCount += caseReport.Claims
		report.Summary.ExpectedClaimCount += caseReport.ExpectedClaims
		report.Summary.SupportedClaimCount += caseReport.SupportedClaims
		report.Summary.MissingExpectedClaims += caseReport.MissingExpectedClaims
		report.Summary.UnsupportedIssueCount += caseReport.UnsupportedIssues
		report.Summary.CitationReferenceCount += caseReport.CitationReferences
		report.Summary.InvalidCitationRefs += caseReport.InvalidCitationRefs
		report.Summary.GraphPathReferenceCount += caseReport.GraphPathReferences
		report.Summary.InvalidGraphPathRefs += caseReport.InvalidGraphPathRefs
		report.Summary.ValidEvidenceReferences += caseReport.ValidEvidenceReferences
		report.Summary.InvalidEvidenceRefs += caseReport.InvalidEvidenceRefs
		report.Summary.TotalBudget = addBudgetUsage(report.Summary.TotalBudget, caseReport.Budget)
		report.Summary.IterationCounts = append(report.Summary.IterationCounts, caseReport.Iterations)
		report.Summary.StopReasons[caseReport.StopReason]++
		if err != nil {
			continue
		}
	}
	report.Summary.GroundingSupportRate = ratio(report.Summary.SupportedClaimCount, report.Summary.ClaimCount)
	report.Summary.UnsupportedOutcomeRate = ratio(report.Summary.UnsupportedIssueCount, report.Summary.ClaimCount+report.Summary.UnsupportedIssueCount)
	return report, nil
}

func (r EvaluationReport) CanonicalJSON() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}

func evaluateRecord(tc EvaluationCase, record Record, runErr error) EvaluationCaseReport {
	resultID := ""
	if runErr == nil {
		resultID = string(record.Result.ID())
	}
	out := EvaluationCaseReport{
		ID:             tc.ID,
		ContextPackID:  string(tc.Pack.ID()),
		ResultID:       resultID,
		StopReason:     record.Trace.StopReason,
		Iterations:     len(record.Trace.Iterations),
		TraceDigest:    traceDigest(record.Trace),
		Budget:         record.Trace.Usage,
		ExpectedClaims: len(tc.ExpectedClaims),
	}
	if runErr != nil {
		out.Error = runErr.Error()
		out.MissingExpectedClaims = len(tc.ExpectedClaims)
		return out
	}
	available := evidenceIndex(tc.Pack.Evidence(), record.Result.EvidenceAdditions())
	matchedExpected := make([]bool, len(tc.ExpectedClaims))
	for _, claim := range record.Result.Claims() {
		out.Claims++
		matchIndex := expectedClaimIndex(claim, tc.ExpectedClaims)
		ok := matchIndex >= 0 && !matchedExpected[matchIndex]
		for _, evidenceID := range claim.EvidenceIDs() {
			anchor, found := available[evidenceID]
			if !found {
				out.InvalidEvidenceRefs++
				ok = false
				continue
			}
			out.ValidEvidenceReferences++
			if citation, quote, cited := anchor.Document(); cited {
				out.CitationReferences++
				if err := validateFixtureCitation(citation, quote, tc.Sources); err != nil {
					out.InvalidCitationRefs++
					ok = false
				}
			}
			if path, cited := anchor.Path(); cited {
				out.GraphPathReferences++
				if err := validateFixtureGraphPath(anchor.ID(), path, tc.GraphPaths); err != nil {
					out.InvalidGraphPathRefs++
					ok = false
				}
			}
		}

		if ok {
			out.SupportedClaims++
			matchedExpected[matchIndex] = true
		}
	}
	for _, matched := range matchedExpected {
		if !matched {
			out.MissingExpectedClaims++
		}
	}
	unsupported := record.Result.Unsupported()
	out.UnsupportedIssues = len(unsupported)
	for _, issue := range unsupported {
		validateIssueEvidenceReferences(issue, available, tc, &out)
	}
	for _, issue := range record.Result.Unresolved() {
		validateIssueEvidenceReferences(issue, available, tc, &out)
	}
	return out
}

func validateIssueEvidenceReferences(
	issue inference.Issue,
	available map[shoal.ID]inference.EvidenceAnchor,
	tc EvaluationCase,
	out *EvaluationCaseReport,
) {
	for _, evidenceID := range issue.EvidenceIDs() {
		anchor, found := available[evidenceID]
		if !found {
			out.InvalidEvidenceRefs++
			continue
		}
		out.ValidEvidenceReferences++
		if citation, quote, cited := anchor.Document(); cited {
			out.CitationReferences++
			if err := validateFixtureCitation(citation, quote, tc.Sources); err != nil {
				out.InvalidCitationRefs++
			}
		}
		if path, cited := anchor.Path(); cited {
			out.GraphPathReferences++
			if err := validateFixtureGraphPath(anchor.ID(), path, tc.GraphPaths); err != nil {
				out.InvalidGraphPathRefs++
			}
		}
	}
}

func validateFixtureGraphPath(anchorID shoal.ID, path graph.Path, expected map[shoal.ID]graph.Path) error {
	if err := path.Validate(); err != nil {
		return err
	}
	want, ok := expected[anchorID]
	if !ok {
		return invalid("graph evidence path is absent from fixture case")
	}
	if len(path.Nodes) != len(want.Nodes) || len(path.Edges) != len(want.Edges) {
		return invalid("graph evidence path does not match fixture oracle")
	}
	for i := range path.Nodes {
		if !fixtureGraphNodesEqual(path.Nodes[i], want.Nodes[i]) {
			return invalid("graph evidence path node does not match fixture oracle")
		}
	}
	for i := range path.Edges {
		if !fixtureGraphEdgesEqual(path.Edges[i], want.Edges[i]) {
			return invalid("graph evidence path edge does not match fixture oracle")
		}
	}
	return nil
}

func fixtureGraphNodesEqual(got, want graph.Node) bool {
	return got.ID == want.ID &&
		got.Kind == want.Kind &&
		stringSlicesEqual(got.Labels, want.Labels) &&
		metadataEqual(got.Properties, want.Properties)
}

func fixtureGraphEdgesEqual(got, want graph.Edge) bool {
	return got.ID == want.ID &&
		got.From == want.From &&
		got.To == want.To &&
		got.Type == want.Type &&
		math.Float64bits(float64(got.Weight)) == math.Float64bits(float64(want.Weight)) &&
		metadataEqual(got.Properties, want.Properties)
}

func stringSlicesEqual(got, want []string) bool {
	got = append([]string(nil), got...)
	want = append([]string(nil), want...)
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func metadataEqual(got, want shoal.Metadata) bool {
	if len(got) != len(want) {
		return false
	}
	for key, gotValue := range got {
		if wantValue, ok := want[key]; !ok || wantValue != gotValue {
			return false
		}
	}
	return true
}

func expectedClaimIndex(claim inference.Claim, expected []ExpectedClaim) int {
	for index, item := range expected {
		if claim.Subject() != item.Subject || claim.Predicate() != item.Predicate ||
			!ontologyValuesEqual(claim.Object(), item.Object) {
			continue
		}
		claimEvidence := claim.EvidenceIDs()
		expectedEvidence := append([]shoal.ID(nil), item.EvidenceIDs...)
		sort.Slice(expectedEvidence, func(i, j int) bool {
			return shoal.CompareID(expectedEvidence[i], expectedEvidence[j]) < 0
		})
		if len(claimEvidence) != len(expectedEvidence) {
			continue
		}
		matches := true
		for i := range claimEvidence {
			if claimEvidence[i] != expectedEvidence[i] {
				matches = false
				break
			}
		}
		if matches {
			return index
		}
	}
	return -1
}

func ontologyValuesEqual(left, right ontology.Value) bool {
	if left.Type() != right.Type() {
		return false
	}
	switch left.Type() {
	case ontology.ValueString:
		l, _ := left.StringValue()
		r, _ := right.StringValue()
		return l == r
	case ontology.ValueInteger:
		l, _ := left.IntegerValue()
		r, _ := right.IntegerValue()
		return l == r
	case ontology.ValueNumber:
		l, _ := left.NumberValue()
		r, _ := right.NumberValue()
		return l == r
	case ontology.ValueBoolean:
		l, _ := left.BooleanValue()
		r, _ := right.BooleanValue()
		return l == r
	case ontology.ValueTimestamp:
		l, _ := left.TimestampValue()
		r, _ := right.TimestampValue()
		return l.Equal(r)
	case ontology.ValueReference:
		l, _ := left.ReferenceValue()
		r, _ := right.ReferenceValue()
		return l == r
	default:
		return false
	}
}

func validateFixtureCitation(citation document.Citation, quote string, sources map[DocumentRevision]string) error {
	if err := citation.Validate(); err != nil {
		return err
	}
	source, ok := sources[DocumentRevision{DocumentID: citation.DocumentID, RevisionID: citation.RevisionID}]
	if !ok {
		return invalid("citation source is absent from fixture case")
	}
	if err := citation.Range.ValidateSource(source); err != nil {
		return err
	}
	start, end := citation.Range.Start.Offset, citation.Range.End.Offset
	if source[int(start):int(end)] != quote {
		return invalid("citation quote does not match fixture source")
	}
	return nil
}

func traceDigest(trace RunTrace) string {
	digest := sha256.New()
	for _, iteration := range trace.Iterations {
		writeDigestPart(digest, strconv.Itoa(iteration.Index))
		writeDigestPart(digest, string(iteration.Decision))
		writeDigestPart(digest, iteration.ActionKey)
		writeDigestPart(digest, string(iteration.CorrelationID))
		writeDigestPart(digest, strconv.Itoa(iteration.Usage.InputTokens))
		writeDigestPart(digest, strconv.Itoa(iteration.Usage.OutputTokens))
		writeDigestPart(digest, strconv.Itoa(iteration.Budget.ModelCalls))
		writeDigestPart(digest, strconv.Itoa(iteration.Budget.InputTokens))
		writeDigestPart(digest, strconv.Itoa(iteration.Budget.OutputTokens))
		writeDigestPart(digest, strconv.Itoa(iteration.Budget.Evidence))
		writeDigestPart(digest, strconv.Itoa(iteration.Budget.GraphHops))
		writeDigestPart(digest, strconv.Itoa(iteration.Budget.GraphNodes))
		writeDigestPart(digest, iteration.Failure)
		for _, id := range iteration.EvidenceIDs {
			writeDigestPart(digest, string(id))
		}
	}
	writeDigestPart(digest, string(trace.StopReason))
	for _, failure := range trace.Failures {
		writeDigestPart(digest, strconv.Itoa(failure.Iteration))
		writeDigestPart(digest, failure.Operation)
		writeDigestPart(digest, failure.Error)
	}
	return "sha256:" + hex.EncodeToString(digest.Sum(nil))
}

type digestWriter interface {
	Write([]byte) (int, error)
}

func writeDigestPart(digest digestWriter, part string) {
	_, _ = digest.Write([]byte(strconv.Itoa(len(part))))
	_, _ = digest.Write([]byte{':'})
	_, _ = digest.Write([]byte(part))
}

func evidenceIndex(anchors ...[]inference.EvidenceAnchor) map[shoal.ID]inference.EvidenceAnchor {
	index := make(map[shoal.ID]inference.EvidenceAnchor)
	for _, set := range anchors {
		for _, anchor := range set {
			index[anchor.ID()] = anchor
		}
	}
	return index
}

func addBudgetUsage(left, right BudgetUsage) BudgetUsage {
	return BudgetUsage{
		ModelCalls:   left.ModelCalls + right.ModelCalls,
		InputTokens:  left.InputTokens + right.InputTokens,
		OutputTokens: left.OutputTokens + right.OutputTokens,
		Evidence:     left.Evidence + right.Evidence,
		GraphHops:    left.GraphHops + right.GraphHops,
		GraphNodes:   left.GraphNodes + right.GraphNodes,
	}
}

func ratio(numerator, denominator int) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}

// LoadFixtureEvaluationCases builds a small, deterministic grounded inference
// suite from the public fixture corpus. It reads local fixture files only.
func LoadFixtureEvaluationCases(root string, generatedAt time.Time) ([]EvaluationCase, error) {
	if strings.TrimSpace(root) == "" {
		return nil, invalid("fixture root is required")
	}
	records, err := loadFixtureExpectations(filepath.Join(root, "expectations.jsonl"))
	if err != nil {
		return nil, err
	}
	cases := make([]EvaluationCase, 0, len(records))
	for _, record := range records {
		if record.Applicability.State != "current_evaluable" && record.CaseID != "q11-graph-native-navigation" {
			continue
		}
		tc, err := evaluationCaseFromExpectation(root, record, generatedAt)
		if err != nil {
			return nil, err
		}
		cases = append(cases, tc)
	}
	return cases, nil
}

type fixtureExpectation struct {
	CaseID        string `json:"case_id"`
	Query         string `json:"query"`
	Request       fixtureExpectationRequest
	Expected      fixtureExpected
	Applicability struct {
		State string `json:"state"`
	} `json:"applicability"`
}

type fixtureExpectationRequest struct {
	AsOf      string `json:"as_of"`
	Principal struct {
		Scopes []string `json:"scopes"`
	} `json:"principal"`
	Filters struct {
		GraphSnapshotID string `json:"graph_snapshot_id"`
	} `json:"filters"`
}

type fixtureExpected struct {
	Evidence []fixtureEvidence `json:"evidence_exact"`
	Facts    []fixtureFact     `json:"facts_exact"`
}

type fixtureEvidence struct {
	EvidenceID      string           `json:"evidence_id"`
	EvidenceType    string           `json:"evidence_type"`
	EvaluationState string           `json:"evaluation_state"`
	Citation        *fixtureCitation `json:"citation"`
	GraphRecordID   shoal.ID         `json:"graph_record_id"`
}

type fixtureCitation struct {
	DocumentID shoal.ID `json:"document_id"`
	RevisionID shoal.ID `json:"revision_id"`
	Path       string   `json:"path"`
	SectionID  shoal.ID `json:"section_id"`
	SpanID     shoal.ID `json:"span_id"`
	ByteStart  int64    `json:"byte_start"`
	ByteEnd    int64    `json:"byte_end"`
	Quote      string   `json:"quote"`
}

type fixtureFact struct {
	SubjectID shoal.ID        `json:"subject_id"`
	Predicate shoal.ID        `json:"predicate"`
	Value     json.RawMessage `json:"value"`
	Qualifier string          `json:"qualifier"`
}

func loadFixtureExpectations(path string) ([]fixtureExpectation, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var records []fixtureExpectation
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var record fixtureExpectation
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

type fixtureGraphIndex struct {
	nodes map[shoal.ID]graph.Node
	edges map[shoal.ID]graph.Edge
	paths map[shoal.ID]struct {
		nodes []shoal.ID
		edges []shoal.ID
	}
}

func loadFixtureGraph(path string) (fixtureGraphIndex, error) {
	index := fixtureGraphIndex{
		nodes: map[shoal.ID]graph.Node{},
		edges: map[shoal.ID]graph.Edge{},
		paths: map[shoal.ID]struct {
			nodes []shoal.ID
			edges []shoal.ID
		}{},
	}
	file, err := os.Open(path)
	if err != nil {
		return index, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var record struct {
			RecordType   string     `json:"record_type"`
			NodeID       shoal.ID   `json:"node_id"`
			NodeType     string     `json:"node_type"`
			Label        string     `json:"label"`
			EdgeID       shoal.ID   `json:"edge_id"`
			EdgeType     string     `json:"edge_type"`
			SourceNodeID shoal.ID   `json:"source_node_id"`
			TargetNodeID shoal.ID   `json:"target_node_id"`
			PathID       shoal.ID   `json:"path_id"`
			NodeIDs      []shoal.ID `json:"node_ids"`
			EdgeIDs      []shoal.ID `json:"edge_ids"`
		}
		if err := json.Unmarshal(line, &record); err != nil {
			return index, err
		}
		switch record.RecordType {
		case "node":
			index.nodes[record.NodeID] = graph.Node{
				ID: record.NodeID, Kind: record.NodeType, Labels: []string{record.Label},
			}
		case "edge":
			index.edges[record.EdgeID] = graph.Edge{
				ID: record.EdgeID, From: record.SourceNodeID, To: record.TargetNodeID, Type: record.EdgeType,
			}
		case "path":
			index.paths[record.PathID] = struct {
				nodes []shoal.ID
				edges []shoal.ID
			}{nodes: append([]shoal.ID(nil), record.NodeIDs...), edges: append([]shoal.ID(nil), record.EdgeIDs...)}
		}
	}
	return index, scanner.Err()
}

func (g fixtureGraphIndex) pathFor(recordID shoal.ID) (graph.Path, error) {
	if node, ok := g.nodes[recordID]; ok {
		return graph.Path{Nodes: []graph.Node{node}}, nil
	}
	if edge, ok := g.edges[recordID]; ok {
		from, fromOK := g.nodes[edge.From]
		to, toOK := g.nodes[edge.To]
		if !fromOK || !toOK {
			return graph.Path{}, invalid("fixture graph edge references unknown nodes")
		}
		return graph.Path{Nodes: []graph.Node{from, to}, Edges: []graph.Edge{edge}}, nil
	}
	if path, ok := g.paths[recordID]; ok {
		nodes := make([]graph.Node, 0, len(path.nodes))
		for _, id := range path.nodes {
			node, ok := g.nodes[id]
			if !ok {
				return graph.Path{}, invalid("fixture graph path references unknown node")
			}
			nodes = append(nodes, node)
		}
		edges := make([]graph.Edge, 0, len(path.edges))
		for _, id := range path.edges {
			edge, ok := g.edges[id]
			if !ok {
				return graph.Path{}, invalid("fixture graph path references unknown edge")
			}
			edges = append(edges, edge)
		}
		return graph.Path{Nodes: nodes, Edges: edges}, nil
	}
	return graph.Path{}, invalid("fixture graph record is absent")
}

func safeFixtureCorpusPath(root, relative string) (string, error) {
	clean := filepath.Clean(relative)
	if filepath.IsAbs(clean) || !strings.HasPrefix(filepath.ToSlash(clean), "corpus/") {
		return "", invalid("fixture path must be corpus-relative")
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	candidate := filepath.Join(rootReal, clean)
	candidateReal, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(rootReal, candidateReal)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", invalid("fixture path escapes root")
	}
	return candidateReal, nil
}

func evaluationCaseFromExpectation(root string, record fixtureExpectation, generatedAt time.Time) (EvaluationCase, error) {
	asOf, err := time.Parse(time.RFC3339Nano, record.Request.AsOf)
	if err != nil {
		return EvaluationCase{}, err
	}
	snapshotID := shoal.ID("fixture-snapshot")
	if strings.TrimSpace(record.Request.Filters.GraphSnapshotID) != "" {
		snapshotID = shoal.ID(record.Request.Filters.GraphSnapshotID)
	}
	snapshot, err := inference.NewSnapshotPin(snapshotID, asOf)
	if err != nil {
		return EvaluationCase{}, err
	}
	scopes := append([]string(nil), record.Request.Principal.Scopes...)
	sort.Strings(scopes)
	authID := shoal.ID("fixture-public")
	if len(scopes) > 0 {
		authID = shoal.ID("fixture-scopes:" + strings.Join(scopes, ","))
	}
	auth, err := inference.NewAuthPin(authID, generatedAt.Add(24*time.Hour))
	if err != nil {
		return EvaluationCase{}, err
	}
	graphs, err := loadFixtureGraph(filepath.Join(root, "graph.jsonl"))
	if err != nil {
		return EvaluationCase{}, err
	}
	anchors, anchorIDs, sources, graphPaths, err := fixtureAnchors(root, record.Expected.Evidence, graphs)
	if err != nil {
		return EvaluationCase{}, err
	}
	if len(anchors) == 0 {
		anchor, err := inference.NewGraphAnchor(graph.Path{
			Nodes: []graph.Node{{ID: shoal.ID("fixture-negative:" + record.CaseID), Kind: "negative-fixture"}},
		})
		if err != nil {
			return EvaluationCase{}, err
		}
		anchors = append(anchors, anchor)
	}
	pack, err := inference.NewContextPack(
		record.Query, anchors, nil, snapshot, auth,
		shoal.Metadata{"fixture": "explorer-eval", "expectation": record.CaseID},
	)
	if err != nil {
		return EvaluationCase{}, err
	}
	expected, err := fixtureExpectedClaims(record, anchorIDs, pack.Evidence())
	if err != nil {
		return EvaluationCase{}, err
	}
	return EvaluationCase{ID: record.CaseID, Pack: pack, ExpectedClaims: expected, Sources: sources, GraphPaths: graphPaths}, nil
}

func fixtureAnchors(root string, evidence []fixtureEvidence, graphs fixtureGraphIndex) ([]inference.EvidenceAnchor, map[string]shoal.ID, map[DocumentRevision]string, map[shoal.ID]graph.Path, error) {
	var anchors []inference.EvidenceAnchor
	anchorIDs := map[string]shoal.ID{}
	sources := map[DocumentRevision]string{}
	graphPaths := map[shoal.ID]graph.Path{}
	for _, item := range evidence {
		if item.EvaluationState != "evaluable" {
			continue
		}
		if item.EvidenceType == "graph_native" {
			path, err := graphs.pathFor(item.GraphRecordID)
			if err != nil {
				return nil, nil, nil, nil, err
			}
			anchor, err := inference.NewGraphAnchor(path)
			if err != nil {
				return nil, nil, nil, nil, err
			}
			anchors = append(anchors, anchor)
			anchorIDs[item.EvidenceID] = anchor.ID()
			graphPaths[anchor.ID()] = path
			continue
		}
		if item.EvidenceType != "citation" || item.Citation == nil {
			continue
		}
		citation := item.Citation
		sourcePath, err := safeFixtureCorpusPath(root, citation.Path)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		content, err := os.ReadFile(sourcePath)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		if bytes.Contains(content, []byte("\r")) {
			return nil, nil, nil, nil, invalid("fixture corpus must use LF line endings")
		}
		docCitation := document.Citation{
			DocumentID: citation.DocumentID,
			RevisionID: citation.RevisionID,
			SectionID:  citation.SectionID,
			SpanID:     citation.SpanID,
			Range: document.SourceRange{
				Start: document.SourcePosition{Offset: citation.ByteStart},
				End:   document.SourcePosition{Offset: citation.ByteEnd},
			},
		}
		if err := validateFixtureCitation(docCitation, citation.Quote, map[DocumentRevision]string{
			{DocumentID: citation.DocumentID, RevisionID: citation.RevisionID}: string(content),
		}); err != nil {
			return nil, nil, nil, nil, err
		}
		anchor, err := inference.NewDocumentAnchor(docCitation, citation.Quote)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		anchors = append(anchors, anchor)
		anchorIDs[item.EvidenceID] = anchor.ID()
		sources[DocumentRevision{DocumentID: citation.DocumentID, RevisionID: citation.RevisionID}] = string(content)
	}
	return anchors, anchorIDs, sources, graphPaths, nil
}

func fixtureExpectedClaims(record fixtureExpectation, anchorIDs map[string]shoal.ID, anchors []inference.EvidenceAnchor) ([]ExpectedClaim, error) {
	fallback := make([]shoal.ID, 0, 1)
	if len(anchors) > 0 {
		fallback = append(fallback, anchors[0].ID())
	}
	expected := make([]ExpectedClaim, 0, len(record.Expected.Facts))
	for _, fact := range record.Expected.Facts {
		value, err := fixtureOntologyValue(fact.Value)
		if err != nil {
			return nil, err
		}
		evidenceIDs := fallback
		if id, ok := anchorIDs[fixtureEvidenceIDForFact(record.CaseID, fact)]; ok {
			evidenceIDs = []shoal.ID{id}
		}
		expected = append(expected, ExpectedClaim{
			Subject: fact.SubjectID, Predicate: fact.Predicate, Object: value,
			EvidenceIDs: evidenceIDs,
		})
	}
	return expected, nil
}

func fixtureEvidenceIDForFact(caseID string, fact fixtureFact) string {
	switch caseID {
	case "q01-current-window":
		return "ev-q01-ack"
	case "q02-historical-window":
		return "ev-q02-ack"
	case "q03-revision-boundary":
		return "ev-q03-ack"
	case "q04-revision-change":
		if fact.Qualifier == "r1" {
			return "ev-q04-r1-ack"
		}
		return "ev-q04-r2-ack"
	case "q05-section-hierarchy":
		return "ev-q05-local-sentence"
	case "q06-cross-document-path":
		if fact.Predicate == "consumer" {
			return "ev-q06-consumer"
		}
		return "ev-q06-buffer"
	case "q07-runbook-mitigation":
		switch fact.Predicate {
		case "leave_online":
			return "ev-q07-online"
		case "restart_only":
			return "ev-q07-restart"
		case "resume_below_age_seconds":
			return "ev-q07-resume"
		default:
			return "ev-q07-pause"
		}
	case "q08-tiered-ranking":
		if fact.Predicate == "restart_policy" {
			return "ev-q08-adr-policy"
		}
		return "ev-q08-buffer"
	case "q11-graph-native-navigation":
		return "ev-q11-public-path"
	case "q17-one-tick-before-boundary":
		return "ev-q17-r1-ack"
	case "q18-one-tick-after-boundary":
		return "ev-q18-r2-ack"
	default:
		return ""
	}
}

func fixtureOntologyValue(raw json.RawMessage) (ontology.Value, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return ontology.Value{}, invalid("fixture fact value is unsupported")
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return ontology.NewStringValue(text)
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err == nil {
		if !strings.ContainsAny(number.String(), ".eE") {
			value, err := strconv.ParseInt(number.String(), 10, 64)
			if err != nil {
				return ontology.Value{}, err
			}
			return ontology.NewIntegerValue(value), nil
		}
		value, err := strconv.ParseFloat(number.String(), 64)
		if err != nil {
			return ontology.Value{}, err
		}
		return ontology.NewNumberValue(value)
	}
	var boolean bool
	if err := json.Unmarshal(raw, &boolean); err == nil {
		return ontology.NewBooleanValue(boolean), nil
	}
	return ontology.Value{}, invalid("fixture fact value is unsupported")
}
