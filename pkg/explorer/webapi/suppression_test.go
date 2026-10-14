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

package webapi_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
)

// hiddenMarkers are strings that only ever appear in the document the granted
// principal is not permitted to read. None of them may appear anywhere in a
// serialized response: the suppressed count is the entire permitted disclosure.
var hiddenMarkers = []string{
	"file:///hidden-secret.md",
	"HIDDENTOPSECRET",
	"zuluhiddentoken",
	"classified withheld marker",
}

// suppressionServiceFixture wires an EmbeddedService over an authorized client
// that has one document the granted principal may read and one it may not.
type suppressionServiceFixture struct {
	service    webapi.Service
	grantedCtx context.Context
	adminCtx   context.Context
}

func newSuppressionServiceFixture(t *testing.T) *suppressionServiceFixture {
	t.Helper()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = corpus.Close() })

	authority := auth.NewAuthority()
	store := authorized.NewMemoryPolicyStore()
	scorer, _ := any(corpus).(authorized.VectorScorer)

	newClient := func(source, policy []byte) *authorized.Client {
		selector, err := authorized.NewStaticPolicySelector(source, policy)
		if err != nil {
			t.Fatal(err)
		}
		client, err := authorized.NewClient(authorized.Config{
			Base:             corpus,
			VectorScorer:     scorer,
			Resolver:         authority.Resolver(),
			PolicySelector:   selector,
			PolicyStore:      store,
			GenerationReader: authnGenerationReader{},
			Clock:            time.Now,
		})
		if err != nil {
			t.Fatal(err)
		}
		return client
	}

	grantedClient := newClient(authnSourceGranted, authnPolicyGranted)
	otherClient := newClient(authnSourceOther, authnPolicyOther)

	grantedCtx, err := authority.Binder().Bind(
		context.Background(), authnPrincipal(t, "granted"))
	if err != nil {
		t.Fatal(err)
	}
	otherCtx, err := authority.Binder().Bind(
		context.Background(), authnPrincipal(t, "other-grant"))
	if err != nil {
		t.Fatal(err)
	}
	adminDecision, err := auth.NewDecision(auth.DecisionConfig{
		Subject:               "admin",
		Actor:                 "admin-actor",
		AuthorizationDomain:   authnDomain,
		AllowedOperations:     authnAllOperations,
		PermittedSourceIDs:    [][]byte{authnSourceGranted, authnSourceOther},
		PermittedPolicyIDs:    [][]byte{authnPolicyGranted, authnPolicyOther},
		PolicyGeneration:      1,
		AuthenticationExpires: time.Now().Add(time.Hour),
		RequestID:             "admin-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	adminCtx, err := authority.Binder().Bind(context.Background(), adminDecision)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := grantedClient.Ingest(grantedCtx, explorer.Source{
		URI:       "file:///granted.md",
		MediaType: explorer.MediaTypeMarkdown,
		Content:   "# Granted\n\nvisible alpha content\n",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := otherClient.Ingest(otherCtx, explorer.Source{
		URI:       "file:///hidden-secret.md",
		Title:     "HIDDENTOPSECRET",
		MediaType: explorer.MediaTypeMarkdown,
		Content:   "# HIDDENTOPSECRET\n\nclassified withheld marker zuluhiddentoken\n",
	}); err != nil {
		t.Fatal(err)
	}

	service, err := webapi.NewEmbeddedService(grantedClient)
	if err != nil {
		t.Fatal(err)
	}
	return &suppressionServiceFixture{
		service:    service,
		grantedCtx: grantedCtx,
		adminCtx:   adminCtx,
	}
}

func assertNoHiddenMarkers(t *testing.T, label string, payload []byte) {
	t.Helper()
	body := string(payload)
	for _, marker := range hiddenMarkers {
		if strings.Contains(body, marker) {
			t.Fatalf("%s response leaked withheld marker %q: %s",
				label, marker, body)
		}
	}
}

func TestServiceDocumentsReportSuppressedWithoutLeaking(t *testing.T) {
	f := newSuppressionServiceFixture(t)

	documents, err := f.service.Documents(f.grantedCtx, webapi.DocumentsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// Exactly one document is withheld from the granted principal.
	if documents.Suppressed != 1 {
		t.Fatalf("documents suppressed = %d, want 1", documents.Suppressed)
	}
	if len(documents.Documents) != 1 {
		t.Fatalf("granted documents = %+v", documents.Documents)
	}

	payload, err := json.Marshal(documents)
	if err != nil {
		t.Fatal(err)
	}
	assertNoHiddenMarkers(t, "documents", payload)
	if !strings.Contains(string(payload), "\"suppressed\":1") {
		t.Fatalf("serialized documents omitted the count: %s", payload)
	}
}

func TestServiceRetrieveReportsSuppressedWithoutLeaking(t *testing.T) {
	f := newSuppressionServiceFixture(t)

	documents, err := f.service.Documents(f.grantedCtx, webapi.DocumentsRequest{})
	if err != nil {
		t.Fatal(err)
	}

	// "zuluhiddentoken" occurs only in the withheld document, so the granted
	// principal matches nothing yet one document is withheld: the no-results
	// case that must still disclose the count.
	response, err := f.service.Retrieve(f.grantedCtx, webapi.RetrievalRequest{
		Snapshot: documents.Snapshot,
		Query: retrieval.Request{
			Text:  "zuluhiddentoken",
			TopK:  5,
			Modes: []retrieval.Mode{retrieval.ModeLexical},
			AsOf:  documents.Snapshot.AsOf,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Retrieval.Results) != 0 {
		t.Fatalf("expected no results, got %+v", response.Retrieval.Results)
	}
	if response.Suppressed != 1 {
		t.Fatalf("retrieve suppressed = %d, want 1", response.Suppressed)
	}

	payload, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	assertNoHiddenMarkers(t, "retrieval", payload)
	if !strings.Contains(string(payload), "\"suppressed\":1") {
		t.Fatalf("serialized retrieval omitted the count: %s", payload)
	}
}

func TestServiceDocumentsReportZeroWhenNothingWithheld(t *testing.T) {
	f := newSuppressionServiceFixture(t)

	// The admin principal is permitted for every source and policy, so the
	// same corpus withholds nothing from it. A mutation that reports a nonzero
	// count when nothing was withheld fails here.
	documents, err := f.service.Documents(f.adminCtx, webapi.DocumentsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(documents.Documents) != 2 {
		t.Fatalf("admin documents = %+v", documents.Documents)
	}
	if documents.Suppressed != 0 {
		t.Fatalf("admin documents suppressed = %d, want 0", documents.Suppressed)
	}

	// A zero count must not appear on the wire, keeping existing golden
	// responses byte-identical.
	payload, err := json.Marshal(documents)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "suppressed") {
		t.Fatalf("zero count must be omitted from the wire form: %s", payload)
	}
}
