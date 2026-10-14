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

package webapi

import (
	"context"
	"testing"

	"github.com/phrocker/shoal-oss/internal/disclosureconformance"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// graphOutcome is what a caller observes from one traversal on the deployed
// surface: the response if it succeeded, the error class and text if not.
type graphOutcome struct {
	Neighborhood *NeighborhoodResponse `json:"neighborhood,omitempty"`
	Path         *PathResponse         `json:"path,omitempty"`
	ErrorCode    shoal.ErrorCode       `json:"error_code,omitempty"`
	ErrorText    string                `json:"error_text,omitempty"`
}

func graphError(err error) graphOutcome {
	code := shoal.ErrorCode("")
	for _, candidate := range []shoal.ErrorCode{
		shoal.ErrorNotFound, shoal.ErrorUnauthorized,
		shoal.ErrorInvalidArgument, shoal.ErrorInternal,
	} {
		if shoal.IsErrorCode(err, candidate) {
			code = candidate
			break
		}
	}
	return graphOutcome{ErrorCode: code, ErrorText: err.Error()}
}

func servedNeighborhood(
	t *testing.T, service *EmbeddedService, ctx context.Context, node shoal.ID,
) graphOutcome {
	t.Helper()
	response, err := service.Neighborhood(ctx, NeighborhoodRequest{
		NodeIDs: []shoal.ID{node}, Depth: 2,
	})
	if err != nil {
		return graphError(err)
	}
	return graphOutcome{Neighborhood: &response}
}

func servedPath(
	t *testing.T, service *EmbeddedService, ctx context.Context,
	from, to shoal.ID,
) graphOutcome {
	t.Helper()
	response, err := service.Path(ctx, PathRequest{
		From: from, To: to, MaxDepth: 3,
	})
	if err != nil {
		return graphError(err)
	}
	return graphOutcome{Path: &response}
}

// TestServedGraphSurfacesAreUniform probes the surfaces a deployment actually
// exposes. The conformance package tests authorized.Client.Neighborhood, but
// EmbeddedService.Neighborhood uses BoundedNeighborhood and Path adds its own
// target and path resolution on top, so neither was covered by that probe.
//
// Path matters most here: it takes a target node, so a restricted target is a
// second place a caller could learn that something exists.
func TestServedGraphSurfacesAreUniform(t *testing.T) {
	corpus := disclosureconformance.NewCorpus(t)
	service, err := NewEmbeddedService(corpus.Client)
	if err != nil {
		t.Fatal(err)
	}
	ctx := corpus.Uncleared(t)
	restricted := corpus.RestrictedNodeID()
	absent := corpus.AbsentNodeID()

	disclosureconformance.Run(t,
		disclosureconformance.Probe{
			Name:     "served-neighborhood/withheld-vs-absent",
			Withheld: servedNeighborhood(t, service, ctx, restricted),
			Control:  servedNeighborhood(t, service, ctx, absent),
		},
		// Only the target varies. Changing the source as well would be
		// rejected at seed authorization inside BoundedNeighborhood before
		// target resolution runs, which merely repeats the neighborhood
		// negative path and cannot catch a target-resolution oracle.
		disclosureconformance.Probe{
			Name:     "served-path/withheld-target-vs-absent-target",
			Withheld: servedPath(t, service, ctx, corpus.OpenNodeID(), restricted),
			Control:  servedPath(t, service, ctx, corpus.OpenNodeID(), absent),
		},
	)
}

// TestClearedPrincipalReachesTheServedGraph guards the probes above. If the
// restricted node were unreachable for everyone, the uniformity assertions
// would compare two failures unrelated to authorization and pass for the wrong
// reason.
func TestClearedPrincipalReachesTheServedGraph(t *testing.T) {
	corpus := disclosureconformance.NewCorpus(t)
	service, err := NewEmbeddedService(corpus.Client)
	if err != nil {
		t.Fatal(err)
	}
	cleared := servedNeighborhood(
		t, service, corpus.Cleared(t), corpus.RestrictedNodeID())
	if cleared.ErrorText != "" {
		t.Fatalf("the cleared principal must expand it: %s", cleared.ErrorText)
	}
	if cleared.Neighborhood == nil ||
		len(cleared.Neighborhood.Neighborhood.Nodes) == 0 {
		t.Fatal("the restricted node must have a neighborhood to withhold")
	}
	uncleared := servedNeighborhood(
		t, service, corpus.Uncleared(t), corpus.RestrictedNodeID())
	if uncleared.ErrorText == "" {
		t.Fatal("the uncleared principal must not expand it")
	}
}

// TestClearedPrincipalResolvesThePathToTheRestrictedTarget guards the path
// probe specifically. Without it, comparing two unresolvable paths would pass
// while proving nothing about target resolution: the cleared principal must be
// able to resolve the very path the uncleared principal is refused.
func TestClearedPrincipalResolvesThePathToTheRestrictedTarget(t *testing.T) {
	corpus := disclosureconformance.NewCorpus(t)
	service, err := NewEmbeddedService(corpus.Client)
	if err != nil {
		t.Fatal(err)
	}
	cleared := servedPath(t, service, corpus.Cleared(t),
		corpus.OpenNodeID(), corpus.RestrictedNodeID())
	if cleared.ErrorText != "" {
		t.Fatalf("the cleared principal must resolve the path: %s",
			cleared.ErrorText)
	}
	if cleared.Path == nil || len(cleared.Path.Path.Nodes) == 0 {
		t.Fatalf("the fixture must connect the open node to the restricted "+
			"one, or the path probe varies a target that is unreachable "+
			"either way: %#v", cleared.Path)
	}
}
