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
	"fmt"
	"sort"
	"strings"
)

// CapabilityRegistryVersion identifies the shared iterator capability
// inventory. Bump it whenever the registered iterator set, Java-class aliases,
// supported execution contexts, activation semantics, or accepted option
// keys/patterns changes.
const CapabilityRegistryVersion = 3

// AccumuloCompatibilityVersion is the Accumulo release line whose iterator
// contracts are described by this registry.
const AccumuloCompatibilityVersion = "4.0"

// SystemColumnFamilySkipping identifies Accumulo's scan-time system layer.
// Shoal implements it in source Seek handling rather than as an SKVI.
const SystemColumnFamilySkipping = "columnFamilySkipping"

// CapabilityContext is the execution context a stack is being validated for.
// It is intentionally broader than IteratorScope so offline-compaction job
// admission can be validated distinctly from online majc execution.
type CapabilityContext string

const (
	ContextUnknown CapabilityContext = "unknown"
	ContextScan    CapabilityContext = "scan"
	ContextMinc    CapabilityContext = "minc"
	ContextMajc    CapabilityContext = "majc"
	ContextOffline CapabilityContext = "offline"
)

func (c CapabilityContext) String() string {
	if c == "" {
		return string(ContextUnknown)
	}
	return string(c)
}

// ContextFromScope maps an iterator runtime scope to its capability-validation
// context.
func ContextFromScope(scope IteratorScope) CapabilityContext {
	switch scope {
	case ScopeScan:
		return ContextScan
	case ScopeMinc:
		return ContextMinc
	case ScopeMajc:
		return ContextMajc
	default:
		return ContextUnknown
	}
}

// IteratorCapability is one entry in the versioned iterator registry.
// Contexts are the legal configuration/execution contexts for the iterator,
// including contexts where it may be present but inactive. ActiveContexts is
// the subset where the iterator's runtime semantics actually take effect.
// OptionKeys are exact matches; OptionPatterns are dynamic indexed keys such as
// "term.<n>" or "edgeWeight.rel.<n>".
type IteratorCapability struct {
	Name           string              `json:"name"`
	JavaClasses    []string            `json:"javaClasses,omitempty"`
	Contexts       []CapabilityContext `json:"contexts"`
	ActiveContexts []CapabilityContext `json:"activeContexts"`
	OptionKeys     []string            `json:"optionKeys,omitempty"`
	OptionPatterns []string            `json:"optionPatterns,omitempty"`
}

// MandatoryStackCapability describes system iterator layers that the host
// must install outside the configured application stack.
type MandatoryStackCapability struct {
	Context         CapabilityContext `json:"context"`
	SystemIterators []string          `json:"systemIterators"`
}

// CapabilityRegistry snapshots the current iterator capability inventory.
type CapabilityRegistry struct {
	Version         int                        `json:"version"`
	AccumuloVersion string                     `json:"accumuloVersion"`
	MandatoryStacks []MandatoryStackCapability `json:"mandatoryStacks"`
	Iterators       []IteratorCapability       `json:"iterators"`
}

// ConfiguredIterator is one required application or table iterator in the
// order Accumulo would execute it.
type ConfiguredIterator struct {
	Name      string            `json:"name"`
	JavaClass string            `json:"javaClass"`
	Priority  int               `json:"priority"`
	Options   map[string]string `json:"options,omitempty"`
}

// CompatibilityRequest identifies the registry/release contract a caller
// requires and the configured stack to check.
type CompatibilityRequest struct {
	RegistryVersion int                  `json:"registryVersion"`
	AccumuloVersion string               `json:"accumuloVersion"`
	Context         CapabilityContext    `json:"context"`
	Iterators       []ConfiguredIterator `json:"iterators,omitempty"`
}

// CompatibilityIssue is a stable, machine-readable reason a stack is unsafe.
type CompatibilityIssue struct {
	Code      string            `json:"code"`
	Message   string            `json:"message"`
	Name      string            `json:"name,omitempty"`
	JavaClass string            `json:"javaClass,omitempty"`
	Priority  int               `json:"priority,omitempty"`
	Option    string            `json:"option,omitempty"`
	Context   CapabilityContext `json:"context,omitempty"`
}

// IteratorCompatibility reports the native mapping for one configured entry.
type IteratorCompatibility struct {
	ConfiguredIterator
	NativeName string `json:"nativeName,omitempty"`
	Supported  bool   `json:"supported"`
}

// CompatibilityReport is the admission result for one required stack.
type CompatibilityReport struct {
	RegistryVersion int                      `json:"registryVersion"`
	AccumuloVersion string                   `json:"accumuloVersion"`
	Context         CapabilityContext        `json:"context"`
	Supported       bool                     `json:"supported"`
	MandatoryStack  MandatoryStackCapability `json:"mandatoryStack"`
	Iterators       []IteratorCompatibility  `json:"iterators"`
	Issues          []CompatibilityIssue     `json:"issues,omitempty"`
}

type iteratorOptionPattern struct {
	description string
	match       func(string) bool
}

type iteratorRegistration struct {
	capability IteratorCapability
	patterns   []iteratorOptionPattern
	new        func() SortedKeyValueIterator
}

var (
	iteratorRegistry = []iteratorRegistration{
		{
			capability: IteratorCapability{
				Name:           IterVersioning,
				JavaClasses:    []string{"org.apache.accumulo.core.iterators.user.VersioningIterator", "org.apache.accumulo.core.iterators.VersioningIterator"},
				Contexts:       []CapabilityContext{ContextScan, ContextMinc, ContextMajc, ContextOffline},
				ActiveContexts: []CapabilityContext{ContextScan, ContextMinc, ContextMajc, ContextOffline},
				OptionKeys:     []string{VersioningOption},
			},
			new: func() SortedKeyValueIterator { return NewVersioningIterator() },
		},
		{
			capability: IteratorCapability{
				Name:           IterVisibility,
				JavaClasses:    []string{"org.apache.accumulo.core.iteratorsImpl.system.VisibilityFilter", "org.apache.accumulo.core.iterators.system.VisibilityFilter"},
				Contexts:       []CapabilityContext{ContextScan, ContextMinc, ContextMajc, ContextOffline},
				ActiveContexts: []CapabilityContext{ContextScan},
			},
			new: func() SortedKeyValueIterator { return NewVisibilityFilter() },
		},
		{
			capability: IteratorCapability{
				Name:           IterDeleting,
				JavaClasses:    []string{"org.apache.accumulo.core.iteratorsImpl.system.DeletingIterator", "org.apache.accumulo.core.iterators.system.DeletingIterator", "org.apache.accumulo.core.iterators.DeletingIterator"},
				Contexts:       []CapabilityContext{ContextScan, ContextMinc, ContextMajc, ContextOffline},
				ActiveContexts: []CapabilityContext{ContextScan, ContextMinc, ContextMajc, ContextOffline},
				OptionKeys:     []string{DeletingOptionPropagate, DeletingOptionBehavior},
			},
			new: func() SortedKeyValueIterator { return NewDeletingIterator() },
		},
		{
			capability: IteratorCapability{
				Name:           IterLatentEdgeDiscovery,
				JavaClasses:    []string{"org.apache.accumulo.core.graph.LatentEdgeDiscoveryIterator"},
				Contexts:       []CapabilityContext{ContextMajc, ContextOffline},
				ActiveContexts: []CapabilityContext{ContextMajc, ContextOffline},
				OptionKeys: []string{
					LatentEdgeSimilarityThreshold,
					LatentEdgeMaxPairsPerCell,
					LatentEdgeMaxCellBuffer,
					LatentEdgeEdgeCF,
					LatentEdgeEmbeddingCF,
					LatentEdgeEmbeddingCQ,
					LatentEdgeSemanticMode,
					LatentEdgeMaxEdgesPerVertex,
					LatentEdgeMaxVectors,
					LatentEdgeDirection,
					LatentEdgeInverseEdgeCF,
				},
			},
			new: func() SortedKeyValueIterator { return NewLatentEdgeDiscoveryIterator() },
		},
		{
			capability: IteratorCapability{
				Name:           IterSemanticEdge,
				Contexts:       []CapabilityContext{ContextMajc, ContextOffline},
				ActiveContexts: []CapabilityContext{ContextMajc, ContextOffline},
				OptionKeys: []string{
					LatentEdgeSimilarityThreshold,
					LatentEdgeMaxPairsPerCell,
					LatentEdgeMaxCellBuffer,
					LatentEdgeEdgeCF,
					LatentEdgeEmbeddingCF,
					LatentEdgeEmbeddingCQ,
					LatentEdgeSemanticMode,
					LatentEdgeMaxEdgesPerVertex,
					LatentEdgeMaxVectors,
					LatentEdgeDirection,
					LatentEdgeInverseEdgeCF,
				},
			},
			new: func() SortedKeyValueIterator { return NewSemanticEdgeIterator() },
		},
		{
			capability: IteratorCapability{
				Name:           IterGraphRank,
				JavaClasses:    []string{"org.apache.accumulo.core.graph.GraphRankIterator"},
				Contexts:       []CapabilityContext{ContextMajc, ContextOffline},
				ActiveContexts: []CapabilityContext{ContextMajc, ContextOffline},
				OptionKeys: []string{
					GraphRankDampingFactor,
					GraphRankMaxIterations,
					GraphRankEdgeType,
					GraphRankMaxVertices,
					GraphRankConvergenceThreshold,
					GraphRankVertexCF,
					GraphRankEdgeCFPrefix,
					GraphRankLabelCQ,
					GraphRankRankCQ,
				},
			},
			new: func() SortedKeyValueIterator { return NewGraphRankIterator() },
		},
		{
			capability: IteratorCapability{
				Name:           IterCausalInference,
				JavaClasses:    []string{"org.apache.accumulo.core.graph.CausalInferenceEngine"},
				Contexts:       []CapabilityContext{ContextScan},
				ActiveContexts: []CapabilityContext{ContextScan},
				OptionKeys: []string{
					CausalInferenceQuery,
					CausalInferenceStartVertex,
					CausalInferenceDirection,
					CausalInferenceMaxDepth,
					CausalInferenceThreshold,
					CausalInferenceEdgeType,
					CausalInferenceMaxVertices,
					CausalInferenceVertexCF,
					CausalInferenceEmbeddingCQ,
					CausalInferenceEdgeCFPrefix,
					CausalInferenceInverseEdgeCFPrefix,
					CausalInferenceResultCF,
					CausalInferenceResultCQ,
				},
			},
			new: func() SortedKeyValueIterator { return NewCausalInferenceIterator() },
		},
		{
			capability: IteratorCapability{
				Name:           IterTermIndex,
				Contexts:       []CapabilityContext{ContextScan},
				ActiveContexts: []CapabilityContext{ContextScan},
				OptionKeys:     []string{TermIndexCount, TermIndexPrimaryPrefix, TermIndexIDSource, TermIndexPostingCF, TermIndexPhrase, TermIndexNumericLower, TermIndexNumericLowerSet, TermIndexNumericUpper, TermIndexNumericUpperSet, TermIndexNumericLowerInclusive, TermIndexNumericUpperInclusive},
				OptionPatterns: []string{
					"term.<n>",
				},
			},
			patterns: []iteratorOptionPattern{
				numericIndexPattern(TermIndexTermPrefix),
			},
			new: func() SortedKeyValueIterator { return NewTermIndexIterator() },
		},
		{
			capability: IteratorCapability{
				Name:           IterVectorKNN,
				Contexts:       []CapabilityContext{ContextScan},
				ActiveContexts: []CapabilityContext{ContextScan},
				OptionKeys:     []string{VectorKNNQuery, VectorKNNEmbeddingSpace, VectorKNNTopK, VectorKNNEmbeddingCF, VectorKNNMetric, VectorKNNMinScore},
			},
			new: func() SortedKeyValueIterator { return NewVectorKNNIterator() },
		},
		{
			capability: IteratorCapability{
				Name:           IterEdgeExpand,
				Contexts:       []CapabilityContext{ContextScan},
				ActiveContexts: []CapabilityContext{ContextScan},
				OptionKeys:     []string{EdgeExpandAnchorCount, EdgeExpandEdgeCF, EdgeExpandEdgeField, EdgeExpandFieldSep, EdgeExpandIDIndex, EdgeExpandRelIndex, EdgeExpandRelCount, EdgeExpandPrimaryPrefix, EdgeExpandIncludeAnchors, EdgeExpandMaxHops, EdgeExpandWeightCount},
				OptionPatterns: []string{
					"anchor.<n>",
					"rel.<n>",
					"edgeWeight.rel.<n>",
					"edgeWeight.weight.<n>",
				},
			},
			patterns: []iteratorOptionPattern{
				numericIndexPattern(EdgeExpandAnchorPrefix),
				numericIndexPattern(EdgeExpandRelPrefix),
				numericIndexPattern(EdgeExpandWeightRelPrefix),
				numericIndexPattern(EdgeExpandWeightValuePrefix),
			},
			new: func() SortedKeyValueIterator { return NewEdgeExpandIterator() },
		},
		{
			capability: IteratorCapability{
				Name:           IterScoreFilter,
				Contexts:       []CapabilityContext{ContextScan},
				ActiveContexts: []CapabilityContext{ContextScan},
				OptionKeys:     []string{ScoreFilterScoreCF, ScoreFilterMethod, ScoreFilterQuery, ScoreFilterTopK, ScoreFilterParamCount, ScoreFilterTimestampAnchorMs, ScoreFilterHalfLifeMs},
				OptionPatterns: []string{"param.<n>"},
			},
			patterns: []iteratorOptionPattern{
				numericIndexPattern(ScoreFilterParamPrefix),
			},
			new: func() SortedKeyValueIterator { return NewScoreFilterIterator() },
		},
		{
			capability: IteratorCapability{
				Name:           IterGraphAggregation,
				Contexts:       []CapabilityContext{ContextScan},
				ActiveContexts: []CapabilityContext{ContextScan},
				OptionKeys:     []string{GraphAggregationOp, GraphAggregationGroupBy, GraphAggregationRowPrefixSep, GraphAggregationValueCF, GraphAggregationValueCQ, GraphAggregationResultRow, GraphAggregationResultCF},
			},
			new: func() SortedKeyValueIterator { return NewGraphAggregationIterator() },
		},
		{
			capability: IteratorCapability{
				Name:           IterAnomalyDetect,
				Contexts:       []CapabilityContext{ContextScan},
				ActiveContexts: []CapabilityContext{ContextScan},
				OptionKeys:     []string{AnomalyDetectValueCF, AnomalyDetectValueCQ, AnomalyDetectMin, AnomalyDetectMax},
			},
			new: func() SortedKeyValueIterator { return NewAnomalyDetectIterator() },
		},
		{
			capability: IteratorCapability{
				Name:           IterVisibilityStamp,
				Contexts:       []CapabilityContext{ContextMajc, ContextOffline},
				ActiveContexts: []CapabilityContext{ContextMajc, ContextOffline},
				OptionKeys:     []string{VisibilityStampLabelOption, VisibilityStampModeOption},
			},
			new: func() SortedKeyValueIterator { return NewVisibilityStampIterator() },
		},
		{
			capability: IteratorCapability{
				Name:           IterAsOf,
				Contexts:       []CapabilityContext{ContextScan},
				ActiveContexts: []CapabilityContext{ContextScan},
				OptionKeys:     []string{AsOfOption},
			},
			new: func() SortedKeyValueIterator { return NewAsOfIterator() },
		},
		{
			capability: IteratorCapability{
				Name:           IterDocumentIndex,
				Contexts:       []CapabilityContext{ContextScan},
				ActiveContexts: []CapabilityContext{ContextScan},
				OptionKeys:     []string{DocumentIndexShardCount, DocumentIndexTermCount, DocumentIndexBoolOp},
				OptionPatterns: []string{
					"shard.<n>",
					"term.<n>.field",
					"term.<n>.value",
				},
			},
			patterns: []iteratorOptionPattern{
				numericIndexPattern(DocumentIndexShardPrefix),
				numericIndexSuffixPattern(DocumentIndexTermPrefix, DocumentIndexTermFieldSuffix),
				numericIndexSuffixPattern(DocumentIndexTermPrefix, DocumentIndexTermValueSuffix),
			},
			new: func() SortedKeyValueIterator { return NewDocumentIndexIterator() },
		},
	}

	registryByName  map[string]*iteratorRegistration
	registryByClass map[string]*iteratorRegistration
)

func init() {
	registryByName = make(map[string]*iteratorRegistration, len(iteratorRegistry))
	registryByClass = make(map[string]*iteratorRegistration)
	for i := range iteratorRegistry {
		reg := &iteratorRegistry[i]
		if _, exists := registryByName[reg.capability.Name]; exists {
			panic(fmt.Sprintf("iterrt: duplicate iterator registration %q", reg.capability.Name))
		}
		registryByName[reg.capability.Name] = reg
		for _, class := range reg.capability.JavaClasses {
			if _, exists := registryByClass[class]; exists {
				panic(fmt.Sprintf("iterrt: duplicate Java iterator class alias %q", class))
			}
			registryByClass[class] = reg
		}
	}
}

// RegistrySnapshot returns a stable copy of the current capability registry.
func RegistrySnapshot() CapabilityRegistry {
	out := make([]IteratorCapability, len(iteratorRegistry))
	for i := range iteratorRegistry {
		out[i] = iteratorRegistry[i].snapshot()
	}
	return CapabilityRegistry{
		Version:         CapabilityRegistryVersion,
		AccumuloVersion: AccumuloCompatibilityVersion,
		MandatoryStacks: []MandatoryStackCapability{
			{Context: ContextScan, SystemIterators: []string{IterDeleting, SystemColumnFamilySkipping, IterVisibility}},
			{Context: ContextMinc, SystemIterators: []string{IterDeleting}},
			{Context: ContextMajc, SystemIterators: []string{IterDeleting}},
			{Context: ContextOffline, SystemIterators: []string{IterDeleting}},
		},
		Iterators: out,
	}
}

// CapabilityVersionMismatchError reports an admission request for a different
// registry schema or Accumulo release contract.
type CapabilityVersionMismatchError struct {
	RequiredRegistryVersion int
	ActualRegistryVersion   int
	RequiredAccumuloVersion string
	ActualAccumuloVersion   string
}

func (e *CapabilityVersionMismatchError) Error() string {
	return fmt.Sprintf(
		"iterrt: capability version mismatch: requested registry=%d accumulo=%q, available registry=%d accumulo=%q",
		e.RequiredRegistryVersion, e.RequiredAccumuloVersion,
		e.ActualRegistryVersion, e.ActualAccumuloVersion)
}

// CheckCompatibility produces the machine-readable admission report for a
// configured stack. It never treats an unknown class as optional.
func CheckCompatibility(req CompatibilityRequest) (CompatibilityReport, error) {
	report := CompatibilityReport{
		RegistryVersion: CapabilityRegistryVersion,
		AccumuloVersion: AccumuloCompatibilityVersion,
		Context:         req.Context,
		Supported:       true,
		Iterators:       make([]IteratorCompatibility, 0, len(req.Iterators)),
	}
	for _, stack := range RegistrySnapshot().MandatoryStacks {
		if stack.Context == req.Context {
			report.MandatoryStack = stack
			break
		}
	}

	if req.RegistryVersion != CapabilityRegistryVersion ||
		req.AccumuloVersion != AccumuloCompatibilityVersion {
		err := &CapabilityVersionMismatchError{
			RequiredRegistryVersion: req.RegistryVersion,
			ActualRegistryVersion:   CapabilityRegistryVersion,
			RequiredAccumuloVersion: req.AccumuloVersion,
			ActualAccumuloVersion:   AccumuloCompatibilityVersion,
		}
		report.Supported = false
		report.Issues = append(report.Issues, CompatibilityIssue{
			Code:    "capability_version_mismatch",
			Message: err.Error(),
			Context: req.Context,
		})
		return report, err
	}
	if report.MandatoryStack.Context == "" {
		err := &UnsupportedStackContextError{Context: req.Context}
		report.Supported = false
		report.Issues = append(report.Issues, CompatibilityIssue{
			Code:    "unsupported_stack_context",
			Message: err.Error(),
			Context: req.Context,
		})
		return report, err
	}

	var issues []error
	for _, configured := range req.Iterators {
		item := IteratorCompatibility{ConfiguredIterator: cloneConfiguredIterator(configured)}
		reg, ok := registryByClass[configured.JavaClass]
		if !ok {
			err := &UnsupportedIteratorClassError{Class: configured.JavaClass, Context: req.Context}
			issues = append(issues, err)
			report.Issues = append(report.Issues, CompatibilityIssue{
				Code:      "unsupported_iterator_class",
				Message:   err.Error(),
				Name:      configured.Name,
				JavaClass: configured.JavaClass,
				Priority:  configured.Priority,
				Context:   req.Context,
			})
			report.Iterators = append(report.Iterators, item)
			continue
		}

		item.NativeName = reg.capability.Name
		spec := IterSpec{Name: item.NativeName, Options: configured.Options}
		if err := ValidateSpec(spec, req.Context); err != nil {
			issues = append(issues, err)
			appendValidationIssues(&report, configured, err)
			report.Iterators = append(report.Iterators, item)
			continue
		}
		if err := validateConfiguredOptions(reg, configured.Options, req.Context); err != nil {
			issues = append(issues, err)
			report.Issues = append(report.Issues, CompatibilityIssue{
				Code:      "unsupported_iterator_configuration",
				Message:   err.Error(),
				Name:      configured.Name,
				JavaClass: configured.JavaClass,
				Priority:  configured.Priority,
				Context:   req.Context,
			})
			report.Iterators = append(report.Iterators, item)
			continue
		}
		item.Supported = true
		report.Iterators = append(report.Iterators, item)
	}

	if len(issues) == 0 {
		return report, nil
	}
	report.Supported = false
	return report, &StackValidationError{Context: req.Context, Issues: issues}
}

// UnsupportedStackContextError reports a compatibility request outside the
// registry's declared stack contexts.
type UnsupportedStackContextError struct {
	Context CapabilityContext
}

func (e *UnsupportedStackContextError) Error() string {
	return fmt.Sprintf("iterrt: unsupported iterator stack context %q", e.Context)
}

// UnsupportedIteratorConfigurationError reports option values that the native
// implementation cannot initialize.
type UnsupportedIteratorConfigurationError struct {
	Name    string
	Context CapabilityContext
	Err     error
}

func (e *UnsupportedIteratorConfigurationError) Error() string {
	return fmt.Sprintf("iterrt: iterator %q configuration unsupported in %s context: %v",
		e.Name, e.Context, e.Err)
}

func (e *UnsupportedIteratorConfigurationError) Unwrap() error { return e.Err }

func validateConfiguredOptions(reg *iteratorRegistration, options map[string]string, ctx CapabilityContext) error {
	scope := ScopeScan
	switch ctx {
	case ContextScan:
		scope = ScopeScan
	case ContextMinc:
		scope = ScopeMinc
	case ContextMajc, ContextOffline:
		scope = ScopeMajc
	default:
		return &UnsupportedIteratorContextError{
			Name:      reg.capability.Name,
			Context:   ctx,
			Supported: append([]CapabilityContext(nil), reg.capability.Contexts...),
		}
	}
	if err := reg.new().Init(NewSliceSource(nil), options, IteratorEnvironment{Scope: scope}); err != nil {
		return &UnsupportedIteratorConfigurationError{
			Name: reg.capability.Name, Context: ctx, Err: err,
		}
	}
	return nil
}

func cloneConfiguredIterator(in ConfiguredIterator) ConfiguredIterator {
	out := in
	if in.Options != nil {
		out.Options = make(map[string]string, len(in.Options))
		for k, v := range in.Options {
			out.Options[k] = v
		}
	}
	return out
}

func appendValidationIssues(report *CompatibilityReport, configured ConfiguredIterator, err error) {
	switch typed := err.(type) {
	case *UnsupportedIteratorContextError:
		report.Issues = append(report.Issues, CompatibilityIssue{
			Code:      "unsupported_iterator_context",
			Message:   typed.Error(),
			Name:      configured.Name,
			JavaClass: configured.JavaClass,
			Priority:  configured.Priority,
			Context:   report.Context,
		})
	case *UnsupportedIteratorOptionError:
		report.Issues = append(report.Issues, CompatibilityIssue{
			Code:      "unsupported_iterator_option",
			Message:   typed.Error(),
			Name:      configured.Name,
			JavaClass: configured.JavaClass,
			Priority:  configured.Priority,
			Option:    typed.Option,
			Context:   report.Context,
		})
	case interface{ Unwrap() []error }:
		for _, issue := range typed.Unwrap() {
			appendValidationIssues(report, configured, issue)
		}
	default:
		report.Issues = append(report.Issues, CompatibilityIssue{
			Code:      "iterator_validation_failed",
			Message:   err.Error(),
			Name:      configured.Name,
			JavaClass: configured.JavaClass,
			Priority:  configured.Priority,
			Context:   report.Context,
		})
	}
}

// CapabilityByName returns the registered capability for a native iterator id.
func CapabilityByName(name string) (IteratorCapability, bool) {
	reg, ok := registryByName[name]
	if !ok {
		return IteratorCapability{}, false
	}
	return reg.snapshot(), true
}

// CapabilityByJavaClass returns the registered capability for a configured
// Java iterator class alias.
func CapabilityByJavaClass(class string) (IteratorCapability, bool) {
	reg, ok := registryByClass[class]
	if !ok {
		return IteratorCapability{}, false
	}
	return reg.snapshot(), true
}

// UnknownIteratorError reports a native iterator id missing from the shared
// registry.
type UnknownIteratorError struct {
	Name string
}

func (e *UnknownIteratorError) Error() string {
	return fmt.Sprintf("iterrt: unknown iterator %q", e.Name)
}

// UnsupportedIteratorClassError reports a configured Java iterator class alias
// missing from the shared registry.
type UnsupportedIteratorClassError struct {
	Class   string
	Context CapabilityContext
}

func (e *UnsupportedIteratorClassError) Error() string {
	if e.Context == "" || e.Context == ContextUnknown {
		return fmt.Sprintf("iterrt: unsupported iterator class %q", e.Class)
	}
	return fmt.Sprintf("iterrt: unsupported iterator class %q in %s context", e.Class, e.Context)
}

// UnsupportedIteratorContextError reports a registered iterator requested in a
// context outside its declared capability set.
type UnsupportedIteratorContextError struct {
	Name      string
	Context   CapabilityContext
	Supported []CapabilityContext
}

func (e *UnsupportedIteratorContextError) Error() string {
	return fmt.Sprintf("iterrt: iterator %q unsupported in %s context (supported: %s)",
		e.Name, e.Context, joinContexts(e.Supported))
}

// UnsupportedIteratorOptionError reports an option key outside an iterator's
// declared option schema.
type UnsupportedIteratorOptionError struct {
	Name      string
	Option    string
	Context   CapabilityContext
	Supported []string
}

func (e *UnsupportedIteratorOptionError) Error() string {
	if len(e.Supported) == 0 {
		return fmt.Sprintf("iterrt: iterator %q does not accept option %q in %s context",
			e.Name, e.Option, e.Context)
	}
	return fmt.Sprintf("iterrt: iterator %q does not accept option %q in %s context (supported: %s)",
		e.Name, e.Option, e.Context, strings.Join(e.Supported, ", "))
}

// IteratorValidationError reports multiple validation failures for one
// iterator spec.
type IteratorValidationError struct {
	Name    string
	Context CapabilityContext
	Issues  []error
}

func (e *IteratorValidationError) Error() string {
	if len(e.Issues) == 0 {
		return fmt.Sprintf("iterrt: iterator %q failed validation in %s context", e.Name, e.Context)
	}
	parts := make([]string, len(e.Issues))
	for i, issue := range e.Issues {
		parts[i] = issue.Error()
	}
	return strings.Join(parts, "; ")
}

func (e *IteratorValidationError) Unwrap() []error { return e.Issues }

// StackValidationError reports one or more invalid iterator specs in a stack.
type StackValidationError struct {
	Context CapabilityContext
	Issues  []error
}

func (e *StackValidationError) Error() string {
	if len(e.Issues) == 0 {
		return fmt.Sprintf("iterrt: invalid %s iterator stack", e.Context)
	}
	parts := make([]string, len(e.Issues))
	for i, issue := range e.Issues {
		parts[i] = issue.Error()
	}
	return strings.Join(parts, "; ")
}

func (e *StackValidationError) Unwrap() []error { return e.Issues }

// ValidateSpec fail-closes a single iterator spec against the shared
// capability registry.
func ValidateSpec(spec IterSpec, ctx CapabilityContext) error {
	reg, ok := registryByName[spec.Name]
	if !ok {
		return &UnknownIteratorError{Name: spec.Name}
	}

	var issues []error
	if !reg.supportsContext(ctx) {
		issues = append(issues, &UnsupportedIteratorContextError{
			Name:      spec.Name,
			Context:   ctx,
			Supported: append([]CapabilityContext(nil), reg.capability.Contexts...),
		})
	}
	for _, option := range reg.invalidOptions(spec.Options) {
		issues = append(issues, &UnsupportedIteratorOptionError{
			Name:      spec.Name,
			Option:    option,
			Context:   ctx,
			Supported: reg.supportedOptions(),
		})
	}

	switch len(issues) {
	case 0:
		return nil
	case 1:
		return issues[0]
	default:
		return &IteratorValidationError{Name: spec.Name, Context: ctx, Issues: issues}
	}
}

// ValidateStack fail-closes an iterator stack before it is executed.
func ValidateStack(specs []IterSpec, ctx CapabilityContext) error {
	var issues []error
	for i, spec := range specs {
		if err := ValidateSpec(spec, ctx); err != nil {
			issues = append(issues, fmt.Errorf("stack position %d (%s): %w", i, spec.Name, err))
		}
	}
	if len(issues) == 0 {
		return nil
	}
	return &StackValidationError{Context: ctx, Issues: issues}
}

func (r *iteratorRegistration) snapshot() IteratorCapability {
	return IteratorCapability{
		Name:           r.capability.Name,
		JavaClasses:    append([]string(nil), r.capability.JavaClasses...),
		Contexts:       append([]CapabilityContext(nil), r.capability.Contexts...),
		ActiveContexts: append([]CapabilityContext(nil), r.capability.ActiveContexts...),
		OptionKeys:     append([]string(nil), r.capability.OptionKeys...),
		OptionPatterns: append([]string(nil), r.capability.OptionPatterns...),
	}
}

func (r *iteratorRegistration) supportsContext(ctx CapabilityContext) bool {
	for _, supported := range r.capability.Contexts {
		if supported == ctx {
			return true
		}
	}
	return false
}

func (r *iteratorRegistration) invalidOptions(options map[string]string) []string {
	if len(options) == 0 {
		return nil
	}
	keys := make([]string, 0, len(options))
	for k := range options {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var invalid []string
	for _, key := range keys {
		if r.supportsOption(key) {
			continue
		}
		invalid = append(invalid, key)
	}
	return invalid
}

func (r *iteratorRegistration) supportsOption(key string) bool {
	for _, exact := range r.capability.OptionKeys {
		if key == exact {
			return true
		}
	}
	for _, pattern := range r.patterns {
		if pattern.match(key) {
			return true
		}
	}
	return false
}

func (r *iteratorRegistration) supportedOptions() []string {
	if len(r.capability.OptionKeys) == 0 && len(r.capability.OptionPatterns) == 0 {
		return nil
	}
	out := make([]string, 0, len(r.capability.OptionKeys)+len(r.capability.OptionPatterns))
	out = append(out, r.capability.OptionKeys...)
	out = append(out, r.capability.OptionPatterns...)
	return out
}

func joinContexts(contexts []CapabilityContext) string {
	if len(contexts) == 0 {
		return string(ContextUnknown)
	}
	parts := make([]string, len(contexts))
	for i, ctx := range contexts {
		parts[i] = ctx.String()
	}
	return strings.Join(parts, ", ")
}

func numericIndexPattern(prefix string) iteratorOptionPattern {
	return iteratorOptionPattern{
		description: prefix + "<n>",
		match: func(key string) bool {
			if !strings.HasPrefix(key, prefix) {
				return false
			}
			return allDigits(key[len(prefix):])
		},
	}
}

func numericIndexSuffixPattern(prefix, suffix string) iteratorOptionPattern {
	return iteratorOptionPattern{
		description: prefix + "<n>" + suffix,
		match: func(key string) bool {
			if !strings.HasPrefix(key, prefix) || !strings.HasSuffix(key, suffix) {
				return false
			}
			middle := key[len(prefix) : len(key)-len(suffix)]
			return allDigits(middle)
		},
	}
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
