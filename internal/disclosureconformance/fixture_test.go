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

	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
)

type retrievalProbeResult struct {
	Response   retrieval.Response
	Disclosure authorized.Disclosure
}

func retrieveTerm(
	t *testing.T, corpus *Corpus, ctx context.Context, term string,
) retrievalProbeResult {
	t.Helper()
	response, disclosure, err := corpus.Client.RetrieveWithDisclosure(
		ctx, retrieval.Request{
			Text: term, TopK: 8,
			Modes: []retrieval.Mode{retrieval.ModeLexical}, Explain: true,
		})
	if err != nil {
		t.Fatalf("retrieve %q: %v", term, err)
	}
	return retrievalProbeResult{Response: response, Disclosure: disclosure}
}

// TestRetrievalIsUniformForWithheldAndAbsent is the measurement that should
// have come first. It proves directly, against a real two-compartment corpus,
// that an uncleared principal cannot tell a term whose matches exist and were
// withheld from a term that matches nothing.
//
// The property holds by construction rather than by care: the authorized layer
// removes documents the identity may not read from the search projection
// before scoring, so retrieval never sees them and cannot report on them. The
// disclosure counts are computed over the whole corpus and are therefore
// identical for every query by this identity, including one that returns a
// result. This test exists so that a future change which makes suppression
// query-dependent, and turns a volume signal into a per-term oracle, fails
// here.
func TestRetrievalIsUniformForWithheldAndAbsent(t *testing.T) {
	corpus := NewCorpus(t)
	ctx := corpus.Uncleared(t)
	Run(t, Probe{
		Name:     "retrieve/withheld-vs-absent",
		Withheld: retrieveTerm(t, corpus, ctx, RestrictedTerm),
		Control:  retrieveTerm(t, corpus, ctx, AbsentTerm),
	})
}

// TestDisclosureCountIsQueryIndependent pins the mechanism the uniformity
// rests on. The count reports how much of the corpus this identity may not
// read, not what matched, so a term that returns a result reports the same
// count as one that returns nothing.
func TestDisclosureCountIsQueryIndependent(t *testing.T) {
	corpus := NewCorpus(t)
	ctx := corpus.Uncleared(t)
	matched := retrieveTerm(t, corpus, ctx, OpenTerm)
	if len(matched.Response.Results) == 0 {
		t.Fatal("the open term must match, or the fixture proves nothing")
	}
	for _, term := range []string{RestrictedTerm, AbsentTerm} {
		other := retrieveTerm(t, corpus, ctx, term)
		if other.Disclosure != matched.Disclosure {
			t.Fatalf("disclosure varies by query: %q reports %#v, %q reports %#v",
				OpenTerm, matched.Disclosure, term, other.Disclosure)
		}
	}
}

// TestClearedPrincipalSeesRestrictedContent guards the fixture itself. If the
// restricted document were unreadable by everyone, or missing, the uniformity
// tests above would pass trivially.
func TestClearedPrincipalSeesRestrictedContent(t *testing.T) {
	corpus := NewCorpus(t)
	cleared := retrieveTerm(t, corpus, corpus.Cleared(t), RestrictedTerm)
	if len(cleared.Response.Results) == 0 {
		t.Fatal("the cleared principal must see the restricted term")
	}
	if cleared.Disclosure.Suppressed != 0 {
		t.Fatalf("nothing should be withheld from the cleared principal: %#v",
			cleared.Disclosure)
	}
	uncleared := retrieveTerm(t, corpus, corpus.Uncleared(t), RestrictedTerm)
	if len(uncleared.Response.Results) != 0 {
		t.Fatal("the uncleared principal must not see the restricted term")
	}
}
