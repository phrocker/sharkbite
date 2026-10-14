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

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestOntologyEvidenceOptionalQuotesUseImmutableSource(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, time.September, 6, 0, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	corpus, err := explorer.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subject", Actor: "actor",
		AuthorizationDomain:   []byte("domain"),
		AllowedOperations:     []auth.Operation{auth.OperationIngest},
		PermittedSourceIDs:    [][]byte{[]byte("source")},
		PermittedPolicyIDs:    [][]byte{[]byte("policy")},
		PolicyGeneration:      1,
		AuthenticationExpires: at.Add(time.Hour),
		RequestID:             "request",
	})
	if err != nil {
		t.Fatal(err)
	}
	selector, err := NewStaticPolicySelector([]byte("source"), []byte("policy"))
	if err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(Config{
		Base: corpus, OntologyProposalStore: corpus,
		Resolver: resolverFunc(func(context.Context) (auth.Decision, error) {
			return decision, nil
		}),
		PolicySelector: selector, PolicyStore: NewMemoryPolicyStore(),
		GenerationReader: generationReaderFunc(
			func(context.Context, []byte) (int64, error) { return 1, nil }),
		Clock: func() time.Time { return at.Add(time.Minute) },
	})
	if err != nil {
		t.Fatal(err)
	}
	const content = "alpha beta gamma"
	result, err := client.Ingest(ctx, explorer.Source{
		URI: "memory://ontology-evidence", Title: "Evidence",
		MediaType: explorer.MediaTypeText, Content: content,
	})
	if err != nil {
		t.Fatal(err)
	}
	view, err := corpus.Document(ctx, result.Document.ID, result.Revision.ID)
	if err != nil {
		t.Fatal(err)
	}
	schema, base := ontologyEvidenceVersions(t, at)
	accepted := make(map[shoal.ID]string)
	for index, test := range []struct {
		name  string
		start int64
		end   int64
		quote string
		deny  bool
	}{
		{"omitted full quote", 0, int64(len(content)), "", false},
		{"exact full quote", 0, int64(len(content)), content, false},
		{"omitted subrange quote", 6, 10, "", false},
		{"exact subrange quote", 6, 10, "beta", false},
		{"fabricated quote", 6, 10, "forged", true},
		{"quote from another range", 0, 5, "beta", true},
		{"invalid range without quote", 0, int64(len(content)) + 1, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			citation := document.Citation{
				DocumentID: view.Document.ID, RevisionID: view.Revision.ID,
				SectionID: view.Root.Section.ID,
				Range: document.SourceRange{
					Start: document.SourcePosition{Offset: test.start},
					End:   document.SourcePosition{Offset: test.end},
				},
			}
			evidence, err := ontology.NewEvidenceRef(citation, test.quote, nil)
			if err != nil {
				t.Fatal(err)
			}
			proposal, _ := ontologyEvidenceProposal(
				t, schema, base, index+2, evidence, at)
			err = client.CreateOntologyProposal(ctx, proposal, base)
			if test.deny {
				if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
					t.Fatalf("invalid evidence create = %v, want not found", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("valid evidence create = %v", err)
			}
			submitted, err := client.TransitionOntologyProposal(
				ctx, proposal.ID(), ontology.ProposalSubmitted,
				"actor", "submit evidence", at.Add(2*time.Minute))
			if err != nil {
				t.Fatalf("ingest-only evidence transition = %v", err)
			}
			if submitted.Morphisms()[0].Evidence()[0].Quote() != test.quote {
				t.Fatal("transition changed the accepted quote")
			}
			accepted[proposal.ID()] = test.quote
		})
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := explorer.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	proposals, err := reopened.OntologyProposals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(accepted) != 4 || len(proposals) != len(accepted) {
		t.Fatalf("accepted = %d, persisted = %d, want 4 each", len(accepted), len(proposals))
	}
	for _, proposal := range proposals {
		quote, ok := accepted[proposal.ID()]
		if !ok || proposal.State() != ontology.ProposalSubmitted ||
			proposal.Morphisms()[0].Evidence()[0].Quote() != quote {
			t.Fatalf("unexpected durable proposal: %q", proposal.ID())
		}
	}
}
