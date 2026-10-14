// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package iterrt

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRegistrySnapshot(t *testing.T) {
	want := CapabilityRegistry{
		Version:         CapabilityRegistryVersion,
		AccumuloVersion: AccumuloCompatibilityVersion,
		MandatoryStacks: []MandatoryStackCapability{
			{Context: ContextScan, SystemIterators: []string{IterDeleting, SystemColumnFamilySkipping, IterVisibility}},
			{Context: ContextMinc, SystemIterators: []string{IterDeleting}},
			{Context: ContextMajc, SystemIterators: []string{IterDeleting}},
			{Context: ContextOffline, SystemIterators: []string{IterDeleting}},
		},
		Iterators: []IteratorCapability{
			{Name: IterVersioning, JavaClasses: []string{"org.apache.accumulo.core.iterators.user.VersioningIterator", "org.apache.accumulo.core.iterators.VersioningIterator"}, Contexts: []CapabilityContext{ContextScan, ContextMinc, ContextMajc, ContextOffline}, ActiveContexts: []CapabilityContext{ContextScan, ContextMinc, ContextMajc, ContextOffline}, OptionKeys: []string{VersioningOption}},
			{Name: IterVisibility, JavaClasses: []string{"org.apache.accumulo.core.iteratorsImpl.system.VisibilityFilter", "org.apache.accumulo.core.iterators.system.VisibilityFilter"}, Contexts: []CapabilityContext{ContextScan, ContextMinc, ContextMajc, ContextOffline}, ActiveContexts: []CapabilityContext{ContextScan}},
			{Name: IterDeleting, JavaClasses: []string{"org.apache.accumulo.core.iteratorsImpl.system.DeletingIterator", "org.apache.accumulo.core.iterators.system.DeletingIterator", "org.apache.accumulo.core.iterators.DeletingIterator"}, Contexts: []CapabilityContext{ContextScan, ContextMinc, ContextMajc, ContextOffline}, ActiveContexts: []CapabilityContext{ContextScan, ContextMinc, ContextMajc, ContextOffline}, OptionKeys: []string{DeletingOptionPropagate, DeletingOptionBehavior}},
			{Name: IterLatentEdgeDiscovery, JavaClasses: []string{"org.apache.accumulo.core.graph.LatentEdgeDiscoveryIterator"}, Contexts: []CapabilityContext{ContextMajc, ContextOffline}, ActiveContexts: []CapabilityContext{ContextMajc, ContextOffline}, OptionKeys: []string{LatentEdgeSimilarityThreshold, LatentEdgeMaxPairsPerCell, LatentEdgeMaxCellBuffer, LatentEdgeEdgeCF, LatentEdgeEmbeddingCF, LatentEdgeEmbeddingCQ, LatentEdgeSemanticMode, LatentEdgeMaxEdgesPerVertex, LatentEdgeMaxVectors, LatentEdgeDirection, LatentEdgeInverseEdgeCF}},
			{Name: IterSemanticEdge, Contexts: []CapabilityContext{ContextMajc, ContextOffline}, ActiveContexts: []CapabilityContext{ContextMajc, ContextOffline}, OptionKeys: []string{LatentEdgeSimilarityThreshold, LatentEdgeMaxPairsPerCell, LatentEdgeMaxCellBuffer, LatentEdgeEdgeCF, LatentEdgeEmbeddingCF, LatentEdgeEmbeddingCQ, LatentEdgeSemanticMode, LatentEdgeMaxEdgesPerVertex, LatentEdgeMaxVectors, LatentEdgeDirection, LatentEdgeInverseEdgeCF}},
			{Name: IterGraphRank, JavaClasses: []string{"org.apache.accumulo.core.graph.GraphRankIterator"}, Contexts: []CapabilityContext{ContextMajc, ContextOffline}, ActiveContexts: []CapabilityContext{ContextMajc, ContextOffline}, OptionKeys: []string{GraphRankDampingFactor, GraphRankMaxIterations, GraphRankEdgeType, GraphRankMaxVertices, GraphRankConvergenceThreshold, GraphRankVertexCF, GraphRankEdgeCFPrefix, GraphRankLabelCQ, GraphRankRankCQ}},
			{Name: IterCausalInference, JavaClasses: []string{"org.apache.accumulo.core.graph.CausalInferenceEngine"}, Contexts: []CapabilityContext{ContextScan}, ActiveContexts: []CapabilityContext{ContextScan}, OptionKeys: []string{CausalInferenceQuery, CausalInferenceStartVertex, CausalInferenceDirection, CausalInferenceMaxDepth, CausalInferenceThreshold, CausalInferenceEdgeType, CausalInferenceMaxVertices, CausalInferenceVertexCF, CausalInferenceEmbeddingCQ, CausalInferenceEdgeCFPrefix, CausalInferenceInverseEdgeCFPrefix, CausalInferenceResultCF, CausalInferenceResultCQ}},
			{Name: IterTermIndex, Contexts: []CapabilityContext{ContextScan}, ActiveContexts: []CapabilityContext{ContextScan}, OptionKeys: []string{TermIndexCount, TermIndexPrimaryPrefix, TermIndexIDSource, TermIndexPostingCF, TermIndexPhrase, TermIndexNumericLower, TermIndexNumericLowerSet, TermIndexNumericUpper, TermIndexNumericUpperSet, TermIndexNumericLowerInclusive, TermIndexNumericUpperInclusive}, OptionPatterns: []string{"term.<n>"}},
			{Name: IterVectorKNN, Contexts: []CapabilityContext{ContextScan}, ActiveContexts: []CapabilityContext{ContextScan}, OptionKeys: []string{VectorKNNQuery, VectorKNNEmbeddingSpace, VectorKNNTopK, VectorKNNEmbeddingCF, VectorKNNMetric, VectorKNNMinScore}},
			{Name: IterEdgeExpand, Contexts: []CapabilityContext{ContextScan}, ActiveContexts: []CapabilityContext{ContextScan}, OptionKeys: []string{EdgeExpandAnchorCount, EdgeExpandEdgeCF, EdgeExpandEdgeField, EdgeExpandFieldSep, EdgeExpandIDIndex, EdgeExpandRelIndex, EdgeExpandRelCount, EdgeExpandPrimaryPrefix, EdgeExpandIncludeAnchors, EdgeExpandMaxHops, EdgeExpandWeightCount}, OptionPatterns: []string{"anchor.<n>", "rel.<n>", "edgeWeight.rel.<n>", "edgeWeight.weight.<n>"}},
			{Name: IterScoreFilter, Contexts: []CapabilityContext{ContextScan}, ActiveContexts: []CapabilityContext{ContextScan}, OptionKeys: []string{ScoreFilterScoreCF, ScoreFilterMethod, ScoreFilterQuery, ScoreFilterTopK, ScoreFilterParamCount, ScoreFilterTimestampAnchorMs, ScoreFilterHalfLifeMs}, OptionPatterns: []string{"param.<n>"}},
			{Name: IterGraphAggregation, Contexts: []CapabilityContext{ContextScan}, ActiveContexts: []CapabilityContext{ContextScan}, OptionKeys: []string{GraphAggregationOp, GraphAggregationGroupBy, GraphAggregationRowPrefixSep, GraphAggregationValueCF, GraphAggregationValueCQ, GraphAggregationResultRow, GraphAggregationResultCF}},
			{Name: IterAnomalyDetect, Contexts: []CapabilityContext{ContextScan}, ActiveContexts: []CapabilityContext{ContextScan}, OptionKeys: []string{AnomalyDetectValueCF, AnomalyDetectValueCQ, AnomalyDetectMin, AnomalyDetectMax}},
			{Name: IterVisibilityStamp, Contexts: []CapabilityContext{ContextMajc, ContextOffline}, ActiveContexts: []CapabilityContext{ContextMajc, ContextOffline}, OptionKeys: []string{VisibilityStampLabelOption, VisibilityStampModeOption}},
			{Name: IterAsOf, Contexts: []CapabilityContext{ContextScan}, ActiveContexts: []CapabilityContext{ContextScan}, OptionKeys: []string{AsOfOption}},
			{Name: IterDocumentIndex, Contexts: []CapabilityContext{ContextScan}, ActiveContexts: []CapabilityContext{ContextScan}, OptionKeys: []string{DocumentIndexShardCount, DocumentIndexTermCount, DocumentIndexBoolOp}, OptionPatterns: []string{"shard.<n>", "term.<n>.field", "term.<n>.value"}},
		},
	}

	if got := RegistrySnapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("registry drifted\n got=%#v\nwant=%#v", got, want)
	}
}

func TestRegistryInventoryJSONIsCurrent(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "iterator-capabilities-v3.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var got CapabilityRegistry
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if want := RegistrySnapshot(); !reflect.DeepEqual(got, want) {
		t.Fatalf("inventory drifted\n got=%#v\nwant=%#v", got, want)
	}
}

func TestBuildStack_AcceptsConfiguredInactiveVisibilityOnMajc(t *testing.T) {
	top, err := BuildStack(
		newSliceSource(kv{k: mk("r", "cf", "cq", "", 1), v: []byte("v")}),
		[]IterSpec{{Name: IterVisibility}},
		IteratorEnvironment{Scope: ScopeMajc},
	)
	if err != nil {
		t.Fatalf("BuildStack: %v", err)
	}
	if err := top.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	got, err := drain(top)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(got) != 1 || string(got[0].v) != "v" {
		t.Fatalf("majc visibility should be passthrough, got %+v", got)
	}
}

func TestBuildStack_RejectsUnsupportedContext(t *testing.T) {
	_, err := BuildStack(
		newSliceSource(kv{k: mk("r", "cf", "cq", "", 1), v: []byte("v")}),
		[]IterSpec{{Name: IterAsOf}},
		IteratorEnvironment{Scope: ScopeMajc},
	)
	if err == nil {
		t.Fatal("expected unsupported-context error")
	}
	var ctxErr *UnsupportedIteratorContextError
	if !errors.As(err, &ctxErr) {
		t.Fatalf("expected UnsupportedIteratorContextError, got %T: %v", err, err)
	}
}

func TestBuildStack_RejectsUnsupportedOption(t *testing.T) {
	_, err := BuildStack(
		newSliceSource(kv{k: mk("r", "cf", "cq", "", 1), v: []byte("v")}),
		[]IterSpec{{Name: IterVersioning, Options: map[string]string{"bogus": "1"}}},
		IteratorEnvironment{Scope: ScopeScan},
	)
	if err == nil {
		t.Fatal("expected unsupported-option error")
	}
	var optErr *UnsupportedIteratorOptionError
	if !errors.As(err, &optErr) {
		t.Fatalf("expected UnsupportedIteratorOptionError, got %T: %v", err, err)
	}
}

func TestCheckCompatibilityKnownAccumulo4Stack(t *testing.T) {
	report, err := CheckCompatibility(CompatibilityRequest{
		RegistryVersion: CapabilityRegistryVersion,
		AccumuloVersion: AccumuloCompatibilityVersion,
		Context:         ContextMajc,
		Iterators: []ConfiguredIterator{
			{Name: "del", JavaClass: "org.apache.accumulo.core.iteratorsImpl.system.DeletingIterator", Priority: 5},
			{Name: "vers", JavaClass: "org.apache.accumulo.core.iterators.user.VersioningIterator", Priority: 20, Options: map[string]string{VersioningOption: "3"}},
		},
	})
	if err != nil {
		t.Fatalf("CheckCompatibility: %v", err)
	}
	if !report.Supported || len(report.Iterators) != 2 {
		t.Fatalf("report = %+v", report)
	}
	if got := report.MandatoryStack.SystemIterators; !reflect.DeepEqual(got, []string{IterDeleting}) {
		t.Fatalf("mandatory stack = %v, want deleting", got)
	}
}

func TestCheckCompatibilityUnknownClassFailsClosed(t *testing.T) {
	report, err := CheckCompatibility(CompatibilityRequest{
		RegistryVersion: CapabilityRegistryVersion,
		AccumuloVersion: AccumuloCompatibilityVersion,
		Context:         ContextScan,
		Iterators: []ConfiguredIterator{
			{Name: "custom", JavaClass: "com.example.UnknownIterator", Priority: 50},
		},
	})
	if err == nil {
		t.Fatal("expected unsupported class error")
	}
	var classErr *UnsupportedIteratorClassError
	if !errors.As(err, &classErr) {
		t.Fatalf("expected UnsupportedIteratorClassError, got %T: %v", err, err)
	}
	if report.Supported || len(report.Issues) != 1 ||
		report.Issues[0].Code != "unsupported_iterator_class" {
		t.Fatalf("report = %+v", report)
	}
}

func TestCheckCompatibilityVersionMismatch(t *testing.T) {
	report, err := CheckCompatibility(CompatibilityRequest{
		RegistryVersion: CapabilityRegistryVersion + 1,
		AccumuloVersion: "4.1",
		Context:         ContextMinc,
	})
	if err == nil {
		t.Fatal("expected version mismatch")
	}
	var versionErr *CapabilityVersionMismatchError
	if !errors.As(err, &versionErr) {
		t.Fatalf("expected CapabilityVersionMismatchError, got %T: %v", err, err)
	}
	if report.Supported || len(report.Issues) != 1 ||
		report.Issues[0].Code != "capability_version_mismatch" {
		t.Fatalf("report = %+v", report)
	}
}

func TestCheckCompatibilityRejectsInvalidOptionValue(t *testing.T) {
	report, err := CheckCompatibility(CompatibilityRequest{
		RegistryVersion: CapabilityRegistryVersion,
		AccumuloVersion: AccumuloCompatibilityVersion,
		Context:         ContextMajc,
		Iterators: []ConfiguredIterator{{
			Name:      "vers",
			JavaClass: "org.apache.accumulo.core.iterators.user.VersioningIterator",
			Priority:  20,
			Options:   map[string]string{VersioningOption: "not-an-integer"},
		}},
	})
	if err == nil {
		t.Fatal("expected invalid option value error")
	}
	var configErr *UnsupportedIteratorConfigurationError
	if !errors.As(err, &configErr) {
		t.Fatalf("expected UnsupportedIteratorConfigurationError, got %T: %v", err, err)
	}
	if report.Supported || report.Issues[0].Code != "unsupported_iterator_configuration" {
		t.Fatalf("report = %+v", report)
	}
}

func TestCheckCompatibilityRejectsUnknownStackContext(t *testing.T) {
	report, err := CheckCompatibility(CompatibilityRequest{
		RegistryVersion: CapabilityRegistryVersion,
		AccumuloVersion: AccumuloCompatibilityVersion,
		Context:         ContextUnknown,
	})
	if err == nil {
		t.Fatal("expected unsupported stack context")
	}
	var contextErr *UnsupportedStackContextError
	if !errors.As(err, &contextErr) {
		t.Fatalf("expected UnsupportedStackContextError, got %T: %v", err, err)
	}
	if report.Supported || report.Issues[0].Code != "unsupported_stack_context" {
		t.Fatalf("report = %+v", report)
	}
}

func TestCheckCompatibilityStackContexts(t *testing.T) {
	class := "org.apache.accumulo.core.graph.LatentEdgeDiscoveryIterator"
	for _, tc := range []struct {
		context CapabilityContext
		ok      bool
	}{
		{ContextScan, false},
		{ContextMinc, false},
		{ContextMajc, true},
		{ContextOffline, true},
	} {
		t.Run(tc.context.String(), func(t *testing.T) {
			report, err := CheckCompatibility(CompatibilityRequest{
				RegistryVersion: CapabilityRegistryVersion,
				AccumuloVersion: AccumuloCompatibilityVersion,
				Context:         tc.context,
				Iterators: []ConfiguredIterator{
					{Name: "latent", JavaClass: class, Priority: 10},
				},
			})
			if (err == nil) != tc.ok {
				t.Fatalf("err = %v, want supported=%v; report=%+v", err, tc.ok, report)
			}
			if report.Supported != tc.ok {
				t.Fatalf("Supported = %v, want %v", report.Supported, tc.ok)
			}
		})
	}
}
