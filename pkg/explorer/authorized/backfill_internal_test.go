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

package authorized

import (
	"context"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/devbackfill"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// sourceKeyedSelector declares source independence and then breaks the claim
// by keying its grant on the source it is handed. Only a selector inside this
// package can make that declaration, so this is the shape of mistake the
// run-time probe exists to catch: the declaration is a human assertion about
// code, and the probe checks it against behaviour.
type sourceKeyedSelector struct {
	domain      []byte
	sourceID    []byte
	grantPolicy []byte
	otherPolicy []byte
}

func (s sourceKeyedSelector) SelectPolicy(
	_ context.Context,
	decision auth.Decision,
	source explorer.Source,
) (auth.Policy, error) {
	grant := s.grantPolicy
	if source.URI == backfillSourceProbe.URI {
		grant = s.otherPolicy
	}
	return auth.NewPolicy(auth.PolicyConfig{
		AuthorizationDomain: decision.AuthorizationDomain(),
		SourceID:            append([]byte(nil), s.sourceID...),
		GrantPolicyID:       append([]byte(nil), grant...),
		Epoch:               decision.PolicyGeneration(),
	})
}

func (s sourceKeyedSelector) ignoresIngestSource() {}

// TestBackfillRefusesSelectorThatObservesSource proves the declaration is
// verified rather than trusted. The selector below is permitted to register
// either rule -- the decision allows both policies -- so the refusal comes
// from the probe detecting divergence, not from an authorization failure.
func TestBackfillRefusesSelectorThatObservesSource(t *testing.T) {
	client, base := backfillProbeClient(t, sourceKeyedSelector{
		domain:      []byte("domain"),
		sourceID:    []byte("source"),
		grantPolicy: []byte("policy"),
		otherPolicy: []byte("other-policy"),
	})
	if _, err := client.BackfillExistingDocumentsForDevelopment(
		context.Background(),
		devbackfill.NewCapability(),
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("backfill with a source-observing selector error = %v", err)
	}
	summaries, err := client.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 0 {
		t.Fatalf("a refused backfill still registered content: %d", len(summaries))
	}
	if _, err := base.Documents(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestBackfillAcceptsSourceIndependentSelector is the positive control for the
// test above: the same corpus and the same decision, with a selector that
// really does ignore the source, is backfilled.
func TestBackfillAcceptsSourceIndependentSelector(t *testing.T) {
	selector, err := NewStaticPolicySelector([]byte("source"), []byte("policy"))
	if err != nil {
		t.Fatal(err)
	}
	client, _ := backfillProbeClient(t, selector)
	registered, err := client.BackfillExistingDocumentsForDevelopment(
		context.Background(), devbackfill.NewCapability())
	if err != nil {
		t.Fatal(err)
	}
	if registered != 1 {
		t.Fatalf("registered = %d, want 1", registered)
	}
	summaries, err := client.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 {
		t.Fatalf("backfilled document was not visible: %d", len(summaries))
	}
}

// backfillProbeClient opens a corpus holding one document that was never
// registered, and returns a client whose decision permits both the grant the
// selector normally derives and the one it derives for the probe source.
func backfillProbeClient(
	t *testing.T,
	selector PolicySelector,
) (*Client, *explorer.Explorer) {
	t.Helper()
	now := time.Date(2026, time.September, 2, 18, 0, 0, 0, time.UTC)
	base, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	if _, err := base.Ingest(context.Background(), explorer.Source{
		URI:       "file:///pre-existing.txt",
		Title:     "Pre-existing",
		MediaType: explorer.MediaTypeText,
		Content:   "content written before the catalog existed",
	}); err != nil {
		t.Fatal(err)
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject:             "subject",
		Actor:               "actor",
		AuthorizationDomain: []byte("domain"),
		AllowedOperations: []auth.Operation{
			auth.OperationIngest, auth.OperationList, auth.OperationRead,
		},
		PermittedSourceIDs: [][]byte{[]byte("source")},
		PermittedPolicyIDs: [][]byte{
			[]byte("policy"), []byte("other-policy"),
		},
		PolicyGeneration:      1,
		AuthenticationExpires: now.Add(time.Hour),
		RequestID:             "request",
	})
	if err != nil {
		t.Fatal(err)
	}
	edges, err := NewStaticPolicySelector([]byte("source"), []byte("policy"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(Config{
		Base: base,
		Resolver: resolverFunc(func(context.Context) (auth.Decision, error) {
			return decision, nil
		}),
		PolicySelector:     selector,
		EdgePolicySelector: edges,
		PolicyStore:        NewMemoryPolicyStore(),
		GenerationReader: generationReaderFunc(
			func(context.Context, []byte) (int64, error) { return 1, nil }),
		Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return client, base
}
