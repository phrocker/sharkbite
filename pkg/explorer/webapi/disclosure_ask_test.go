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
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// askOutcome is what a caller can observe from one grounded question: the
// envelope if it succeeded, and the error class and message if it did not.
// Both halves matter. An error that names why a question could not be answered
// is as much a response as a body is.
type askOutcome struct {
	Envelope  *CitationEnvelope `json:"envelope,omitempty"`
	ErrorCode shoal.ErrorCode   `json:"error_code,omitempty"`
	ErrorText string            `json:"error_text,omitempty"`
}

func askTerm(
	t *testing.T, service *ChatService, ctx context.Context, term string,
) askOutcome {
	t.Helper()
	envelope, err := service.Ask(ctx, AskRequest{Question: term, TopK: 8})
	if err != nil {
		return askOutcome{
			ErrorCode: primaryErrorCode(err), ErrorText: err.Error(),
		}
	}
	return askOutcome{Envelope: &envelope}
}

// TestGroundedAskIsUniformForWithheldAndAbsent answers the question the
// disclosure-count analysis left open. The counts are corpus-wide, so they
// cannot be a per-term oracle. The grounded-ask path runs per request, so if a
// "matches exist but were withheld" outcome were distinguishable from "nothing
// matched" anywhere, this is where it would be.
//
// Nothing here is a test double except the model, which is the deterministic
// offline generator: a real two-compartment corpus, the real authorized
// client, and the real reasoning path.
func TestGroundedAskIsUniformForWithheldAndAbsent(t *testing.T) {
	corpus := disclosureconformance.NewCorpus(t)
	provenance, err := inference.NewModelProvenance("fake", "fake", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewChatService(context.Background(), ChatConfig{
		Client: corpus.Client, Resolver: corpus.Resolver(),
		Generator: model.FakeGenerator{Model: "fake"}, Model: provenance,
		RetrievalModes: []retrieval.Mode{retrieval.ModeLexical},
		Clock:          corpus.Clock(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Guard the probe. If both sides failed for an unrelated reason the
	// comparison would pass while proving nothing, so first prove the path
	// works: the cleared principal must actually get an answer grounded in the
	// restricted document.
	cleared := askTerm(
		t, service, corpus.Cleared(t), disclosureconformance.RestrictedTerm)
	if cleared.Envelope == nil {
		t.Fatalf("the cleared principal must be answerable, got %q: %s",
			cleared.ErrorCode, cleared.ErrorText)
	}

	ctx := corpus.Uncleared(t)
	withheld := askTerm(t, service, ctx, disclosureconformance.RestrictedTerm)
	absent := askTerm(t, service, ctx, disclosureconformance.AbsentTerm)
	t.Logf("withheld outcome: code=%q text=%q", withheld.ErrorCode, withheld.ErrorText)
	t.Logf("absent   outcome: code=%q text=%q", absent.ErrorCode, absent.ErrorText)
	disclosureconformance.Run(t, disclosureconformance.Probe{
		Name: "ask/withheld-vs-absent", Withheld: withheld, Control: absent,
	})
}
