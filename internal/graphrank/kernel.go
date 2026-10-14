// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

// Package graphrank provides the deterministic, storage-independent PageRank
// kernel shared by graph iterators and authorized bounded analytics.
package graphrank

import (
	"context"
	"math"
	"sort"
)

// Edge is one directed PageRank transition.
type Edge struct {
	From string
	To   string
}

// Options controls deterministic PageRank iteration.
type Options struct {
	DampingFactor              float64
	MaxIterations              int
	ConvergenceThreshold       float64
	RedistributeDangling       bool
	DeduplicateIncomingSources bool
}

// Result contains ranks keyed by vertex ID and convergence metadata.
type Result struct {
	Ranks      map[string]float64
	Iterations int
	Converged  bool
}

// Compute runs PageRank over the supplied vertex set. Vertex and contribution
// order are canonicalized so identical inputs produce identical floating-point
// evaluation order.
func Compute(
	ctx context.Context,
	vertices []string,
	edges []Edge,
	options Options,
) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	vertexIDs := append([]string(nil), vertices...)
	sort.Strings(vertexIDs)
	if len(vertexIDs) == 0 {
		return Result{Ranks: map[string]float64{}, Converged: true}, nil
	}

	known := make(map[string]struct{}, len(vertexIDs))
	for _, vertexID := range vertexIDs {
		known[vertexID] = struct{}{}
	}
	outDegree := make(map[string]int, len(vertexIDs))
	incoming := make(map[string][]string, len(vertexIDs))
	incomingSets := make(map[string]map[string]struct{}, len(vertexIDs))
	for _, edge := range edges {
		outDegree[edge.From]++
		if _, ok := known[edge.To]; !ok {
			continue
		}
		if options.DeduplicateIncomingSources {
			sources := incomingSets[edge.To]
			if sources == nil {
				sources = make(map[string]struct{})
				incomingSets[edge.To] = sources
			}
			sources[edge.From] = struct{}{}
			continue
		}
		incoming[edge.To] = append(incoming[edge.To], edge.From)
	}
	if options.DeduplicateIncomingSources {
		for vertexID, sources := range incomingSets {
			values := make([]string, 0, len(sources))
			for source := range sources {
				values = append(values, source)
			}
			sort.Strings(values)
			incoming[vertexID] = values
		}
	} else {
		for vertexID := range incoming {
			sort.Strings(incoming[vertexID])
		}
	}

	n := len(vertexIDs)
	initialRank := 1.0 / float64(n)
	ranks := make(map[string]float64, n)
	for _, vertexID := range vertexIDs {
		ranks[vertexID] = initialRank
	}
	result := Result{Ranks: ranks}
	for iteration := 0; iteration < options.MaxIterations; iteration++ {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		dangling := 0.0
		if options.RedistributeDangling {
			for _, vertexID := range vertexIDs {
				if outDegree[vertexID] == 0 {
					dangling += ranks[vertexID]
				}
			}
		}
		newRanks := make(map[string]float64, n)
		maxDelta := 0.0
		for _, vertexID := range vertexIDs {
			rankSum := 0.0
			for _, source := range incoming[vertexID] {
				sourceRank, ok := ranks[source]
				if !ok {
					continue
				}
				degree := outDegree[source]
				if degree == 0 {
					degree = 1
				}
				rankSum += sourceRank / float64(degree)
			}
			if options.RedistributeDangling {
				rankSum += dangling / float64(n)
			}
			newRank := (1.0-options.DampingFactor)/float64(n) +
				options.DampingFactor*rankSum
			newRanks[vertexID] = newRank
			if delta := math.Abs(newRank - ranks[vertexID]); delta > maxDelta {
				maxDelta = delta
			}
		}
		ranks = newRanks
		result.Ranks = ranks
		result.Iterations = iteration + 1
		if maxDelta < options.ConvergenceThreshold {
			result.Converged = true
			break
		}
	}
	return result, nil
}
