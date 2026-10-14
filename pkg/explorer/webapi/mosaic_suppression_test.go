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

// mosaicServiceFixture wires an EmbeddedService over a mosaic-enabled authorized
// client that reads through the durable, engine-backed policy store — the exact
// path the deployed binary takes. Two documents live in two distinct
// sensitivity compartments; a budget of one distinct domain forces a
// co-occurrence restriction for any identity authorized for both.
type mosaicServiceFixture struct {
	service   webapi.Service
	adminCtx  context.Context
	grantCtx  context.Context
	alpha     explorer.IngestResult
	beta      explorer.IngestResult
	alphaMark string
	betaMark  string
}

func newMosaicServiceFixture(t *testing.T) *mosaicServiceFixture {
	t.Helper()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = corpus.Close() })

	store, err := authorized.OpenDurablePolicyStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	authority := auth.NewAuthority()
	scorer, _ := any(corpus).(authorized.VectorScorer)

	newClient := func(source, policy []byte, budget authorized.MosaicBudget) *authorized.Client {
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
			Mosaic:           budget,
		})
		if err != nil {
			t.Fatal(err)
		}
		return client
	}

	// Ingest clients run without the budget so the two compartments load
	// cleanly; the read client the service wraps enforces the budget.
	alphaClient := newClient(authnSourceGranted, authnPolicyGranted, authorized.MosaicBudget{})
	betaClient := newClient(authnSourceOther, authnPolicyOther, authorized.MosaicBudget{})

	grantCtx, err := authority.Binder().Bind(
		context.Background(), authnPrincipal(t, "granted"))
	if err != nil {
		t.Fatal(err)
	}
	betaCtx, err := authority.Binder().Bind(
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

	const alphaMark = "alphacompartmentmarker"
	const betaMark = "betacompartmentmarker"
	alpha, err := alphaClient.Ingest(grantCtx, explorer.Source{
		URI:       "file:///alpha-compartment.md",
		Title:     alphaMark,
		MediaType: explorer.MediaTypeMarkdown,
		Content:   "# alpha\n\n" + alphaMark + " shared inference token\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	beta, err := betaClient.Ingest(betaCtx, explorer.Source{
		URI:       "file:///beta-compartment.md",
		Title:     betaMark,
		MediaType: explorer.MediaTypeMarkdown,
		Content:   "# beta\n\n" + betaMark + " shared inference token\n",
	})
	if err != nil {
		t.Fatal(err)
	}

	// The service reads through a budget of one distinct domain.
	readClient := newClient(
		authnSourceGranted, authnPolicyGranted,
		authorized.MosaicBudget{MaxDomains: 1, Window: time.Hour})
	service, err := webapi.NewEmbeddedService(readClient)
	if err != nil {
		t.Fatal(err)
	}
	return &mosaicServiceFixture{
		service:   service,
		adminCtx:  adminCtx,
		grantCtx:  grantCtx,
		alpha:     alpha,
		beta:      beta,
		alphaMark: alphaMark,
		betaMark:  betaMark,
	}
}

// withheldMarker returns the sentinel of whichever compartment the budget
// withheld, given the single document that survived.
func (f *mosaicServiceFixture) withheldMarker(shownID string) string {
	if shownID == string(f.alpha.Document.ID) {
		return f.betaMark
	}
	return f.alphaMark
}

// TestServiceDocumentsReportRestrictedThroughDeployedPath proves the mosaic
// restriction is enforced and surfaced where production actually reads: through
// EmbeddedService over the durable policy store. The admin identity is
// authorized for both compartments, so the withholding is a Restricted count,
// not a Suppressed one, and the withheld compartment never appears on the wire.
func TestServiceDocumentsReportRestrictedThroughDeployedPath(t *testing.T) {
	f := newMosaicServiceFixture(t)

	documents, err := f.service.Documents(f.adminCtx, webapi.DocumentsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if documents.Restricted != 1 {
		t.Fatalf("documents restricted = %d, want 1", documents.Restricted)
	}
	if documents.Suppressed != 0 {
		t.Fatalf("documents suppressed = %d, want 0", documents.Suppressed)
	}
	if len(documents.Documents) != 1 {
		t.Fatalf("admin documents = %+v", documents.Documents)
	}

	payload, err := json.Marshal(documents)
	if err != nil {
		t.Fatal(err)
	}
	body := string(payload)
	// No-oracle: the withheld compartment's identifying content is absent. The
	// count reveals only that a restriction happened, never which domain.
	withheld := f.withheldMarker(string(documents.Documents[0].Document.ID))
	if strings.Contains(body, withheld) {
		t.Fatalf("restricted response leaked withheld compartment %q: %s",
			withheld, body)
	}
	// The distinguishable signal reaches the wire through the hand-written
	// marshaler as a plain count.
	if !strings.Contains(body, "\"restricted\":1") {
		t.Fatalf("serialized documents omitted the restriction count: %s", body)
	}
	// Reason-class distinction on the wire: a restriction is not a denial.
	if strings.Contains(body, "\"suppressed\"") {
		t.Fatalf("restriction must not emit a suppression count: %s", body)
	}

	// The hand-written unmarshaler round-trips the count back.
	var decoded webapi.DocumentsResponse
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Restricted != 1 {
		t.Fatalf("round-tripped documents restricted = %d, want 1",
			decoded.Restricted)
	}
}

// TestServiceRetrieveReportsRestrictedThroughDeployedPath proves the same for
// retrieval: the restricted compartment is dropped from the searched
// projection, the count is reported, and no withheld content leaks.
func TestServiceRetrieveReportsRestrictedThroughDeployedPath(t *testing.T) {
	f := newMosaicServiceFixture(t)

	documents, err := f.service.Documents(f.adminCtx, webapi.DocumentsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	response, err := f.service.Retrieve(f.adminCtx, webapi.RetrievalRequest{
		Snapshot: documents.Snapshot,
		Query: retrieval.Request{
			Text:  "shared inference token",
			TopK:  5,
			Modes: []retrieval.Mode{retrieval.ModeLexical},
			AsOf:  documents.Snapshot.AsOf,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Restricted != 1 {
		t.Fatalf("retrieve restricted = %d, want 1", response.Restricted)
	}
	if response.Suppressed != 0 {
		t.Fatalf("retrieve suppressed = %d, want 0", response.Suppressed)
	}

	payload, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	body := string(payload)
	withheld := f.withheldMarker(string(documents.Documents[0].Document.ID))
	if strings.Contains(body, withheld) {
		t.Fatalf("restricted retrieval leaked withheld compartment %q: %s",
			withheld, body)
	}
	if !strings.Contains(body, "\"restricted\":1") {
		t.Fatalf("serialized retrieval omitted the restriction count: %s", body)
	}

	var decoded webapi.RetrievalResponse
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Restricted != 1 {
		t.Fatalf("round-tripped retrieval restricted = %d, want 1",
			decoded.Restricted)
	}
}

// TestServiceRestrictionDistinctFromDenialOnTheWire proves the two reason
// classes are separable in the serialized response a browser receives: the
// granted identity's denial emits a suppression count with no restriction,
// while the admin identity's co-occurrence limit emits a restriction count with
// no suppression. A caller can tell the two apart without learning what was
// withheld.
func TestServiceRestrictionDistinctFromDenialOnTheWire(t *testing.T) {
	f := newMosaicServiceFixture(t)

	// Granted identity: authorized for one compartment only. The other is a
	// plain denial, and one compartment stays within the budget of one.
	denial, err := f.service.Documents(f.grantCtx, webapi.DocumentsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if denial.Suppressed != 1 || denial.Restricted != 0 {
		t.Fatalf("denial: suppressed=%d restricted=%d, want 1 and 0",
			denial.Suppressed, denial.Restricted)
	}
	denialBody, err := json.Marshal(denial)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(denialBody), "\"suppressed\":1") ||
		strings.Contains(string(denialBody), "\"restricted\"") {
		t.Fatalf("denial wire form wrong: %s", denialBody)
	}

	// Admin identity: authorized for both compartments, restricted by the
	// budget. Same store, same corpus, opposite reason class.
	restriction, err := f.service.Documents(f.adminCtx, webapi.DocumentsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if restriction.Restricted != 1 || restriction.Suppressed != 0 {
		t.Fatalf("restriction: suppressed=%d restricted=%d, want 0 and 1",
			restriction.Suppressed, restriction.Restricted)
	}
	restrictionBody, err := json.Marshal(restriction)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(restrictionBody), "\"restricted\":1") ||
		strings.Contains(string(restrictionBody), "\"suppressed\"") {
		t.Fatalf("restriction wire form wrong: %s", restrictionBody)
	}
}
