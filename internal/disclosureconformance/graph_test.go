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

package disclosureconformance

import (
	"context"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// graphOutcome is everything a caller observes from one traversal: the result
// if it succeeded, and the error class and message if it did not. An error
// that names why a node could not be expanded is as much a response as a
// neighborhood is.
type graphOutcome struct {
	Neighborhood explorer.Neighborhood `json:"neighborhood"`
	ErrorCode    shoal.ErrorCode       `json:"error_code,omitempty"`
	ErrorText    string                `json:"error_text,omitempty"`
}

func neighborhoodOf(
	t *testing.T, corpus *Corpus, ctx context.Context, node shoal.ID,
) graphOutcome {
	t.Helper()
	result, err := corpus.Client.Neighborhood(ctx, explorer.NeighborhoodRequest{
		NodeIDs: []shoal.ID{node}, Depth: 2,
	})
	if err != nil {
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
	return graphOutcome{Neighborhood: result}
}

// TestNeighborhoodIsUniformForWithheldAndAbsent closes the graph half of the
// disclosure question. Unlike the withholding counts, which are computed over
// the whole corpus and are therefore identical for every request by
// construction, traversal runs per request against a caller-supplied node. If
// a per-request oracle existed anywhere it would be here: expanding a node
// that exists and is forbidden could plausibly differ from expanding one that
// was never there.
//
// It does not. Both are refused identically.
func TestNeighborhoodIsUniformForWithheldAndAbsent(t *testing.T) {
	corpus := NewCorpus(t)
	ctx := corpus.Uncleared(t)
	Run(t, Probe{
		Name:     "neighborhood/withheld-vs-absent",
		Withheld: neighborhoodOf(t, corpus, ctx, corpus.RestrictedNodeID()),
		Control:  neighborhoodOf(t, corpus, ctx, corpus.AbsentNodeID()),
	})
}

// TestClearedPrincipalCanExpandTheRestrictedNode guards the probe above. If
// the restricted node were unreachable for everyone, or the fixture's node ID
// were wrong, the uniformity test would pass while comparing two failures that
// have nothing to do with authorization.
func TestClearedPrincipalCanExpandTheRestrictedNode(t *testing.T) {
	corpus := NewCorpus(t)
	cleared := neighborhoodOf(
		t, corpus, corpus.Cleared(t), corpus.RestrictedNodeID())
	if cleared.ErrorText != "" {
		t.Fatalf("the cleared principal must expand it: %s", cleared.ErrorText)
	}
	if len(cleared.Neighborhood.Nodes) == 0 {
		t.Fatal("the restricted node must have a neighborhood to withhold")
	}
	uncleared := neighborhoodOf(
		t, corpus, corpus.Uncleared(t), corpus.RestrictedNodeID())
	if uncleared.ErrorText == "" {
		t.Fatal("the uncleared principal must not expand it")
	}
}
