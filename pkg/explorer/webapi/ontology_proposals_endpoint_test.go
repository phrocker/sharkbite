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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestOntologyProposalEndpointUsesEmbeddedServiceLifecycleAndPersistence(t *testing.T) {
	data := t.TempDir()
	corpus, err := explorer.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	service, server := ontologyProposalServer(t, corpus, richOntologyVersion(t))

	created := createOntologyProposal(t, server, "v2")
	if created.State != string(ontology.ProposalDraft) ||
		len(created.Transitions) != 0 {
		t.Fatalf("created proposal = %+v", created)
	}
	submitted := transitionOntologyProposal(
		t, server, created.ID, string(ontology.ProposalSubmitted))
	if submitted.State != string(ontology.ProposalSubmitted) ||
		len(submitted.Transitions) != 1 {
		t.Fatalf("submitted proposal = %+v", submitted)
	}
	approved := transitionOntologyProposal(
		t, server, created.ID, string(ontology.ProposalApproved))
	if approved.State != string(ontology.ProposalApproved) ||
		len(approved.Transitions) != 2 {
		t.Fatalf("approved proposal = %+v", approved)
	}
	published := transitionOntologyProposal(
		t, server, created.ID, string(ontology.ProposalPublished))
	if published.State != string(ontology.ProposalPublished) ||
		len(published.Transitions) != 3 {
		t.Fatalf("published proposal = %+v", published)
	}

	active, configured, err := service.ActiveOntology(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !configured || active.Version() != "v2" {
		t.Fatalf("active ontology after publish = configured %v version %q",
			configured, active.Version())
	}
	baseIdentity, err := ontology.NewOntologyIdentity(richOntologyVersion(t))
	if err != nil {
		t.Fatal(err)
	}
	publishedIdentity, err := ontology.NewOntologyIdentity(active)
	if err != nil {
		t.Fatal(err)
	}
	assertion := assertionUnderOntology(t, baseIdentity)
	if reading := assertion.ReadUnder(publishedIdentity); reading != ontology.OntologyOtherVersion {
		t.Fatalf("published active ontology retroactively changed assertion reading = %s", reading)
	}

	proposals := getOntologyProposals(t, server)
	if len(proposals.Proposals) != 1 ||
		proposals.Proposals[0].ID != created.ID ||
		proposals.Proposals[0].State != string(ontology.ProposalPublished) ||
		len(proposals.Proposals[0].Transitions) != 3 {
		t.Fatalf("proposal list after lifecycle = %+v", proposals)
	}
	server.Close()
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := explorer.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopenedService, reopenedServer := ontologyProposalServer(
		t, reopened, richOntologyVersion(t))
	persisted := getOntologyProposals(t, reopenedServer)
	if len(persisted.Proposals) != 1 ||
		persisted.Proposals[0].State != string(ontology.ProposalPublished) ||
		len(persisted.Proposals[0].Transitions) != 3 {
		t.Fatalf("persisted proposal after reopen = %+v", persisted)
	}
	replayed, configured, err := reopenedService.ActiveOntology(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !configured || replayed.Version() != "v2" {
		t.Fatalf("active ontology after reopen = configured %v version %q",
			configured, replayed.Version())
	}
}

func TestOntologyProposalEndpointRejectsIllegalTransition(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	_, server := ontologyProposalServer(t, corpus, richOntologyVersion(t))

	created := createOntologyProposal(t, server, "v2")
	_ = transitionOntologyProposal(
		t, server, created.ID, string(ontology.ProposalSubmitted))
	_ = transitionOntologyProposal(
		t, server, created.ID, string(ontology.ProposalRejected))

	body := bytes.NewBufferString(
		`{"state":"approved","note":"cannot approve rejected proposal"}`)
	response, err := server.Client().Post(
		server.URL+"/api/v1/ontology/proposals/"+created.ID+"/transition",
		"application/json",
		body,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("approve rejected status = %d, want 400: %s",
			response.StatusCode, data)
	}
	proposals := getOntologyProposals(t, server)
	if got := proposals.Proposals[0]; got.State != string(ontology.ProposalRejected) ||
		len(got.Transitions) != 2 {
		t.Fatalf("illegal transition mutated proposal = %+v", got)
	}
}

func TestOntologyProposalEndpointPreservesTransitionHistory(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	_, server := ontologyProposalServer(t, corpus, richOntologyVersion(t))

	created := createOntologyProposal(t, server, "v2")
	_ = transitionOntologyProposal(
		t, server, created.ID, string(ontology.ProposalSubmitted))
	approved := transitionOntologyProposal(
		t, server, created.ID, string(ontology.ProposalApproved))
	if len(approved.Transitions) != 2 ||
		approved.Transitions[0].From != string(ontology.ProposalDraft) ||
		approved.Transitions[0].To != string(ontology.ProposalSubmitted) ||
		approved.Transitions[1].From != string(ontology.ProposalSubmitted) ||
		approved.Transitions[1].To != string(ontology.ProposalApproved) {
		t.Fatalf("transition history was not append-only: %+v", approved.Transitions)
	}
	persisted := getOntologyProposals(t, server)
	if got := persisted.Proposals[0]; len(got.Transitions) != 2 ||
		got.Transitions[0].To != string(ontology.ProposalSubmitted) ||
		got.Transitions[1].To != string(ontology.ProposalApproved) {
		t.Fatalf("persisted transition history was not append-only: %+v", got.Transitions)
	}
}

func TestOntologyProposalEndpointRejectsRepeatedConcurrentTransition(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	_, server := ontologyProposalServer(t, corpus, richOntologyVersion(t))
	created := createOntologyProposal(t, server, "v2")

	const attempts = 8
	var wg sync.WaitGroup
	statuses := make(chan int, attempts)
	for index := 0; index < attempts; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := bytes.NewBufferString(
				`{"state":"submitted","note":"concurrent submit"}`)
			response, err := server.Client().Post(
				server.URL+"/api/v1/ontology/proposals/"+created.ID+"/transition",
				"application/json",
				body,
			)
			if err != nil {
				statuses <- 0
				return
			}
			defer response.Body.Close()
			statuses <- response.StatusCode
		}()
	}
	wg.Wait()
	close(statuses)
	successes := 0
	for status := range statuses {
		if status == http.StatusOK {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent submit successes = %d, want 1", successes)
	}
	proposals := getOntologyProposals(t, server)
	if got := proposals.Proposals[0]; got.State != string(ontology.ProposalSubmitted) ||
		len(got.Transitions) != 1 {
		t.Fatalf("concurrent submit corrupted proposal = %+v", got)
	}
}

func TestOntologyProposalEndpointDistinguishesAuthorizationDenial(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	authority := auth.NewAuthority()
	selector, err := authorized.NewStaticPolicySelector(
		authnSourceGranted, authnPolicyGranted)
	if err != nil {
		t.Fatal(err)
	}
	client, err := authorized.NewClient(authorized.Config{
		Base:                  corpus,
		OntologyProposalStore: corpus,
		Resolver:              authority.Resolver(),
		PolicySelector:        selector,
		PolicyStore:           authorized.NewMemoryPolicyStore(),
		GenerationReader:      authnGenerationReader{},
		Clock:                 time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := webapi.NewEmbeddedService(client)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetOntologyVersion(richOntologyVersion(t)); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	handler, err := webapi.NewAuthenticatedHandler(
		service, webapi.AuthenticatorFunc(authnAuthenticate),
		authority.Binder(), server.Listener.Addr().String())
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	defer server.Close()

	body := mustJSON(t, ontologyProposalRequest("v2"))
	request, err := http.NewRequest(
		http.MethodPost, server.URL+"/api/v1/ontology/proposals",
		bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(authnPrincipalHeader, "no-ingest")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("proposal denial status = %d, want 401", response.StatusCode)
	}
	if got := response.Header.Get("WWW-Authenticate"); got != "" {
		t.Fatalf("proposal authorization denial must not carry bearer challenge, got %q", got)
	}
	var envelope struct {
		Code shoal.ErrorCode `json:"code"`
	}
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Code != shoal.ErrorUnauthorized {
		t.Fatalf("proposal denial code = %q, want unauthorized", envelope.Code)
	}

	create, err := http.NewRequest(
		http.MethodPost, server.URL+"/api/v1/ontology/proposals",
		bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	create.Header.Set("Content-Type", "application/json")
	create.Header.Set(authnPrincipalHeader, "ingest-only")
	createdResponse, err := server.Client().Do(create)
	if err != nil {
		t.Fatal(err)
	}
	defer createdResponse.Body.Close()
	if createdResponse.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(createdResponse.Body)
		t.Fatalf("ingest-only create status = %d, want 201: %s",
			createdResponse.StatusCode, data)
	}
	var created webapi.OntologyProposalResponse
	if err := json.NewDecoder(createdResponse.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	for _, state := range []ontology.ProposalState{
		ontology.ProposalSubmitted,
		ontology.ProposalApproved,
		ontology.ProposalPublished,
	} {
		transition, err := http.NewRequest(
			http.MethodPost,
			server.URL+"/api/v1/ontology/proposals/"+created.Proposal.ID+"/transition",
			bytes.NewReader(mustJSON(t, webapi.TransitionOntologyProposalRequest{
				State: string(state), Note: "ingest-only governance",
			})))
		if err != nil {
			t.Fatal(err)
		}
		transition.Header.Set("Content-Type", "application/json")
		transition.Header.Set(authnPrincipalHeader, "ingest-only")
		transitionResponse, err := server.Client().Do(transition)
		if err != nil {
			t.Fatal(err)
		}
		data, readErr := io.ReadAll(transitionResponse.Body)
		transitionResponse.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if transitionResponse.StatusCode != http.StatusOK {
			t.Fatalf("ingest-only %s status = %d, want 200: %s",
				state, transitionResponse.StatusCode, data)
		}
	}
	list, err := http.NewRequest(
		http.MethodGet, server.URL+"/api/v1/ontology/proposals", nil)
	if err != nil {
		t.Fatal(err)
	}
	list.Header.Set(authnPrincipalHeader, "ingest-only")
	listResponse, err := server.Client().Do(list)
	if err != nil {
		t.Fatal(err)
	}
	defer listResponse.Body.Close()
	if listResponse.StatusCode != http.StatusUnauthorized {
		data, _ := io.ReadAll(listResponse.Body)
		t.Fatalf("ingest-only list status = %d, want 401: %s",
			listResponse.StatusCode, data)
	}
}

func TestOntologyProposalEndpointReturnsStableOrdering(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	_, server := ontologyProposalServer(t, corpus, richOntologyVersion(t))

	first := createOntologyProposal(t, server, "v2")
	time.Sleep(time.Millisecond)
	second := createOntologyProposal(t, server, "v3")
	proposals := getOntologyProposals(t, server)
	if len(proposals.Proposals) != 2 {
		t.Fatalf("proposal count = %d, want 2", len(proposals.Proposals))
	}
	if proposals.Proposals[0].ID != second.ID || proposals.Proposals[1].ID != first.ID {
		t.Fatalf("proposal order = %q then %q, want newest first %q then %q",
			proposals.Proposals[0].ID, proposals.Proposals[1].ID, second.ID, first.ID)
	}
}

func TestOntologyProposalPublishUpdatesActiveOntology(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	service, server := ontologyProposalServer(t, corpus, richOntologyVersion(t))

	created := createOntologyProposal(t, server, "v2")
	_ = transitionOntologyProposal(
		t, server, created.ID, string(ontology.ProposalSubmitted))
	_ = transitionOntologyProposal(
		t, server, created.ID, string(ontology.ProposalApproved))
	_ = transitionOntologyProposal(
		t, server, created.ID, string(ontology.ProposalPublished))

	active, configured, err := service.ActiveOntology(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !configured || active.Version() != "v2" {
		t.Fatalf("published ontology was not activated: configured=%v version=%q",
			configured, active.Version())
	}
}

func TestOntologyProposalPublishRejectsStaleBase(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	service, server := ontologyProposalServer(t, corpus, richOntologyVersion(t))

	first := createOntologyProposal(t, server, "v2")
	second := createOntologyProposal(t, server, "v3")
	for _, proposal := range []webapi.OntologyProposalProjection{first, second} {
		_ = transitionOntologyProposal(
			t, server, proposal.ID, string(ontology.ProposalSubmitted))
		_ = transitionOntologyProposal(
			t, server, proposal.ID, string(ontology.ProposalApproved))
	}
	_ = transitionOntologyProposal(
		t, server, first.ID, string(ontology.ProposalPublished))

	body := bytes.NewBufferString(`{"state":"published","note":"stale publish"}`)
	response, err := server.Client().Post(
		server.URL+"/api/v1/ontology/proposals/"+second.ID+"/transition",
		"application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusConflict {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("stale publish status = %d, want 409: %s",
			response.StatusCode, data)
	}
	active, configured, err := service.ActiveOntology(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !configured || active.Version() != "v2" {
		t.Fatalf("stale proposal changed active ontology: configured=%v version=%q",
			configured, active.Version())
	}
}

func TestOntologyProposalConcurrentPublishHasSingleWinner(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	_, server := ontologyProposalServer(t, corpus, richOntologyVersion(t))
	first := createOntologyProposal(t, server, "v2")
	second := createOntologyProposal(t, server, "v3")
	for _, proposal := range []webapi.OntologyProposalProjection{first, second} {
		_ = transitionOntologyProposal(
			t, server, proposal.ID, string(ontology.ProposalSubmitted))
		_ = transitionOntologyProposal(
			t, server, proposal.ID, string(ontology.ProposalApproved))
	}

	start := make(chan struct{})
	statuses := make(chan int, 2)
	for _, proposalID := range []string{first.ID, second.ID} {
		proposalID := proposalID
		go func() {
			<-start
			body := bytes.NewBufferString(
				`{"state":"published","note":"concurrent publish"}`)
			response, requestErr := server.Client().Post(
				server.URL+"/api/v1/ontology/proposals/"+proposalID+"/transition",
				"application/json", body)
			if requestErr != nil {
				statuses <- 0
				return
			}
			defer response.Body.Close()
			statuses <- response.StatusCode
		}()
	}
	close(start)
	successes := 0
	conflicts := 0
	for range 2 {
		switch status := <-statuses; status {
		case http.StatusOK:
			successes++
		case http.StatusConflict:
			conflicts++
		default:
			t.Fatalf("concurrent publish status = %d", status)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent publishes = %d successes, %d conflicts", successes, conflicts)
	}
	proposals := getOntologyProposals(t, server)
	published := 0
	approved := 0
	for _, proposal := range proposals.Proposals {
		switch proposal.State {
		case string(ontology.ProposalPublished):
			published++
		case string(ontology.ProposalApproved):
			approved++
		}
	}
	if published != 1 || approved != 1 {
		t.Fatalf("concurrent proposal states = %+v", proposals.Proposals)
	}
}

func TestOntologyProposalMorphismUsesKeyReferencesAndWireCodecs(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	active := richOntologyVersion(t)
	_, server := ontologyProposalServer(t, corpus, active)
	request := ontologyProposalRequest("v2")
	request.ProposedVersion.Relationships[0].Key = "belongs_to"
	request.ProposedVersion.Relationships[0].Name = "Belongs to"
	metadata := shoal.Metadata{"source": "test", "unicode": "caf\u00e9", "\ufffd": "literal", "nul": "\x00"}
	evidenceDraft := webapi.OntologyMorphismEvidenceDraft{
		Citation: document.Citation{
			DocumentID: "document:opaque/value", RevisionID: "revision:opaque/value",
			SectionID: "section:opaque/value", SpanID: "span:opaque/value",
			Range: document.SourceRange{
				Start: document.SourcePosition{Offset: 0, Page: 1},
				End:   document.SourcePosition{Offset: 4, Page: 1},
			},
		},
		Quote: "rename evidence",
		Path: &graph.Path{Nodes: []graph.Node{{
			ID: "node:opaque/value", Kind: "evidence",
		}}},
		Metadata: shoal.Metadata{"source": "test"},
	}
	request.Morphisms = []webapi.OntologyMorphismDraft{{
		Kind: ontology.MorphismRename,
		Sources: []webapi.OntologyDefinitionReferenceDraft{{
			Namespace: "relationship", Key: "member_of",
		}},
		Targets: []webapi.OntologyDefinitionReferenceDraft{{
			Namespace: "relationship", Key: "belongs_to",
		}},
		Evidence:  []webapi.OntologyMorphismEvidenceDraft{evidenceDraft},
		Rationale: "rename the relationship",
		Metadata:  metadata,
	}}
	encoded := mustJSON(t, request)
	var decoded webapi.CreateOntologyProposalRequest
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Morphisms) != 1 || !reflect.DeepEqual(decoded.Morphisms[0].Metadata, metadata) {
		t.Fatalf("draft metadata roundtrip = %#v", decoded.Morphisms)
	}
	if bytes.Contains(encoded, []byte(`"document_id":"document:opaque/value"`)) ||
		bytes.Contains(encoded, []byte(`"id":"node:opaque/value"`)) ||
		bytes.Contains(encoded, []byte(`"metadata":{"source"`)) {
		t.Fatalf("morphism evidence bypassed wire codecs: %s", encoded)
	}
	created := createOntologyProposalWithRequest(t, server, request)
	if len(created.Morphisms) != 1 {
		t.Fatalf("morphism count = %d, want 1", len(created.Morphisms))
	}
	var sourceID shoal.ID
	for _, relationship := range active.Relationships() {
		if relationship.Key() == "member_of" {
			sourceID = relationship.ID()
		}
	}
	var targetID string
	for _, relationship := range created.ProposedOntology.Relationships {
		if relationship.Key == "belongs_to" {
			targetID = relationship.ID
		}
	}
	projected := created.Morphisms[0]
	wantSource := base64.RawURLEncoding.EncodeToString([]byte(sourceID))
	var evidenceOptions []ontology.EvidenceOption
	if evidenceDraft.Path != nil {
		evidenceOptions = append(
			evidenceOptions, ontology.WithEvidencePath(*evidenceDraft.Path))
	}
	expectedEvidence, err := ontology.NewEvidenceRef(
		evidenceDraft.Citation, evidenceDraft.Quote,
		evidenceDraft.Metadata, evidenceOptions...)
	if err != nil {
		t.Fatal(err)
	}
	if len(projected.Sources) != 1 || projected.Sources[0] != wantSource ||
		len(projected.Targets) != 1 || projected.Targets[0] != targetID ||
		len(projected.EvidenceIDs) != 1 ||
		projected.EvidenceIDs[0] != base64.RawURLEncoding.EncodeToString(
			[]byte(expectedEvidence.ID())) ||
		len(projected.Evidence) != 0 {
		t.Fatalf("projected morphism IDs = %+v", projected)
	}
	if !reflect.DeepEqual(projected.Metadata, metadata) {
		t.Fatalf("projection metadata = %#v, want %#v", projected.Metadata, metadata)
	}
	persisted, err := corpus.OntologyProposals(context.Background())
	if err != nil || len(persisted) != 1 || len(persisted[0].Morphisms()) != 1 {
		t.Fatalf("persisted proposals = %#v, %v", persisted, err)
	}
	stored := persisted[0].Morphisms()[0]
	expected, err := ontology.NewOntologyMorphism(ontology.MorphismConfig{
		Kind: ontology.MorphismRename, SourceVersion: active,
		TargetVersion: persisted[0].ProposedVersion(),
		Sources:       []shoal.ID{sourceID}, Targets: stored.Targets(),
		Evidence:  []ontology.EvidenceRef{expectedEvidence},
		Rationale: request.Morphisms[0].Rationale, Metadata: metadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stored.Metadata(), metadata) || stored.ID() != expected.ID() ||
		projected.ID != base64.RawURLEncoding.EncodeToString([]byte(expected.ID())) {
		t.Fatalf("morphism metadata or canonical identity changed: stored %q, projected %q, want %q",
			stored.ID(), projected.ID, expected.ID())
	}
	listed := getOntologyProposals(t, server)
	if len(listed.Proposals) != 1 || len(listed.Proposals[0].Morphisms) != 1 ||
		!reflect.DeepEqual(listed.Proposals[0].Morphisms[0].Metadata, metadata) ||
		listed.Proposals[0].Morphisms[0].ID != projected.ID {
		t.Fatalf("listed morphism metadata or identity changed: %#v", listed.Proposals)
	}
	listedEvidence := listed.Proposals[0].Morphisms[0].Evidence
	if len(listedEvidence) != 1 ||
		listedEvidence[0].ID != projected.EvidenceIDs[0] ||
		listedEvidence[0].Citation != evidenceDraft.Citation ||
		listedEvidence[0].Quote != evidenceDraft.Quote ||
		listedEvidence[0].Metadata["source"] != "test" ||
		listedEvidence[0].Path == nil ||
		len(listedEvidence[0].Path.Nodes) != 1 ||
		listedEvidence[0].Path.Nodes[0].ID != "node:opaque/value" {
		t.Fatalf("listed morphism evidence = %+v", listedEvidence)
	}
	for _, invalidMetadata := range []shoal.Metadata{
		{"\xff": "one", "\xfe": "two"},
		{"valid": "\xff"},
	} {
		invalid := request
		invalid.Morphisms = append([]webapi.OntologyMorphismDraft(nil), request.Morphisms...)
		invalid.Morphisms[0].Metadata = invalidMetadata
		response, err := server.Client().Post(
			server.URL+"/api/v1/ontology/proposals", "application/json",
			bytes.NewReader(mustJSON(t, invalid)))
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid UTF-8 metadata status = %d, want 400: %s", response.StatusCode, body)
		}
	}
	if after := getOntologyProposals(t, server); len(after.Proposals) != 1 {
		t.Fatalf("invalid metadata persisted a proposal: %#v", after.Proposals)
	}
}

func TestOntologyProposalMorphismEmptyDefinitionsReturnBadRequest(t *testing.T) {
	for _, missing := range []string{"sources", "targets"} {
		t.Run(missing, func(t *testing.T) {
			corpus, err := explorer.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer corpus.Close()
			_, server := ontologyProposalServer(t, corpus, richOntologyVersion(t))
			request := ontologyProposalRequest("v2")
			source := []webapi.OntologyDefinitionReferenceDraft{{
				Namespace: "relationship", Key: "member_of",
			}}
			target := append([]webapi.OntologyDefinitionReferenceDraft(nil), source...)
			if missing == "sources" {
				source = nil
			} else {
				target = nil
			}
			request.Morphisms = []webapi.OntologyMorphismDraft{{
				Kind: ontology.MorphismRename, Sources: source, Targets: target,
				Evidence: []webapi.OntologyMorphismEvidenceDraft{{
					Citation: document.Citation{
						DocumentID: "doc", RevisionID: "rev",
						SectionID: "section", SpanID: "span",
						Range: document.SourceRange{
							Start: document.SourcePosition{Offset: 0, Page: 1},
							End:   document.SourcePosition{Offset: 4, Page: 1},
						},
					},
				}},
				Rationale: "malformed mapping",
			}}
			response, err := server.Client().Post(
				server.URL+"/api/v1/ontology/proposals", "application/json",
				bytes.NewReader(mustJSON(t, request)))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusBadRequest {
				data, _ := io.ReadAll(response.Body)
				t.Fatalf("empty %s status = %d, want 400: %s",
					missing, response.StatusCode, data)
			}
		})
	}
}

func TestActiveOntologyReplaysPublishedChainAcrossRestart(t *testing.T) {
	data := t.TempDir()
	corpus, err := explorer.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	service, server := ontologyProposalServer(t, corpus, richOntologyVersion(t))
	first := createOntologyProposal(t, server, "v2")
	for _, state := range []ontology.ProposalState{
		ontology.ProposalSubmitted, ontology.ProposalApproved, ontology.ProposalPublished,
	} {
		_ = transitionOntologyProposal(t, server, first.ID, string(state))
	}
	second := createOntologyProposal(t, server, "v3")
	for _, state := range []ontology.ProposalState{
		ontology.ProposalSubmitted, ontology.ProposalApproved, ontology.ProposalPublished,
	} {
		_ = transitionOntologyProposal(t, server, second.ID, string(state))
	}
	active, configured, err := service.ActiveOntology(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !configured || active.Version() != "v3" {
		t.Fatalf("active chain before restart = configured %v version %q",
			configured, active.Version())
	}
	server.Close()
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := explorer.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	reopenedService, reopenedServer := ontologyProposalServer(
		t, reopened, richOntologyVersion(t))
	defer reopenedServer.Close()
	active, configured, err = reopenedService.ActiveOntology(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !configured || active.Version() != "v3" {
		t.Fatalf("active chain after restart = configured %v version %q",
			configured, active.Version())
	}
	catalog, configured, err := reopenedService.OntologyCatalog(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !configured || len(catalog.Identities()) != 3 ||
		catalog.ActiveIdentity() != mustOntologyIdentity(t, active) {
		t.Fatalf("governed choice catalog after restart = %#v", catalog.Identities())
	}
}

func mustOntologyIdentity(
	t *testing.T,
	version ontology.OntologyVersion,
) ontology.OntologyIdentity {
	t.Helper()
	identity, err := ontology.NewOntologyIdentity(version)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}

func TestOntologyProposalBlastRadiusReportsStructuralDiffWithoutDecorativeCounts(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	_, server := ontologyProposalServer(t, corpus, blastRadiusOntologyVersion(t))

	created := createOntologyProposalWithRequest(
		t, server, blastRadiusProposalRequest())
	report, encoded := getOntologyProposalBlastRadius(t, server, created.ID)
	if report.ProposalID != created.ID ||
		report.Summary.DestructiveChanges != 6 ||
		report.Summary.AdditiveChanges != 3 {
		t.Fatalf("blast radius summary = %+v for proposal %q", report.Summary, report.ProposalID)
	}
	if bytes.Contains(encoded, []byte("asserted_count")) ||
		bytes.Contains(encoded, []byte("derived_count")) ||
		bytes.Contains(encoded, []byte(`"impact"`)) ||
		bytes.Contains(encoded, []byte("counts_computed")) {
		t.Fatalf("blast radius emitted unimplemented assertion counts: %s", encoded)
	}
	if got := conceptByKey(report.RemovedConcepts, "case_file"); got == nil {
		t.Fatalf("removed case_file = %+v, want structural removal", report.RemovedConcepts)
	}
	if got := relationByKey(report.RemovedRelationships, "referenced_in"); got == nil {
		t.Fatalf("removed referenced_in = %+v, want structural removal", report.RemovedRelationships)
	}
	if got := propertyByKey(report.RemovedProperties, "role"); got == nil {
		t.Fatalf("removed role = %+v, want structural removal", report.RemovedProperties)
	}
	if got := conceptChangeByKey(report.ChangedConcepts, "person"); got == nil ||
		!equalStrings(got.Fields, []string{"properties"}) {
		t.Fatalf("changed person = %+v, want property-set change", got)
	}
	if got := relationChangeByKey(report.ChangedRelationships, "member_of"); got == nil ||
		!equalStrings(got.Fields, []string{"to_concepts", "directed", "properties"}) {
		t.Fatalf("changed member_of = %+v, want endpoint/direction/property change", got)
	}
	if got := propertyChangeByKey(report.ChangedProperties, "name"); got == nil ||
		!equalStrings(got.Fields, []string{"value_type"}) {
		t.Fatalf("changed name = %+v, want value-type change", got)
	}
	if conceptByKey(report.AddedConcepts, "vessel") == nil ||
		relationByKey(report.AddedRelationships, "port_call") == nil ||
		propertyByKey(report.AddedProperties, "imo") == nil {
		t.Fatalf("added blast radius entries = concepts %+v relationships %+v properties %+v",
			report.AddedConcepts, report.AddedRelationships, report.AddedProperties)
	}
}

func TestOntologyProposalBlastRadiusHasNoUnimplementedImpactCountScaffold(t *testing.T) {
	source, err := os.ReadFile("ontology_blast_radius.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range [][]byte{
		[]byte("OntologyAssertionImpactCounts"),
		[]byte("ontologyAssertionImpactCounter"),
		[]byte("changedOntologyElementIDs"),
		[]byte("OntologyAssertionImpactProjection"),
		[]byte("CountsComputed"),
		[]byte("asserted_count"),
		[]byte("derived_count"),
		[]byte(`json:"impact"`),
	} {
		if bytes.Contains(source, forbidden) {
			t.Fatalf("blast-radius source contains unimplemented assertion-count scaffold %q", forbidden)
		}
	}
}

func TestOntologyProposalBlastRadiusRejectsOversizedActiveOntology(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	_, server := ontologyProposalServer(t, corpus, oversizedOntologyVersion(t))
	created := createOntologyProposalWithRequest(t, server, webapi.CreateOntologyProposalRequest{
		Rationale: "replace oversized active ontology with a bounded proposal",
		ProposedVersion: webapi.OntologyProposalVersionDraft{
			Version: "v2",
		},
	})

	response, err := server.Client().Get(
		server.URL + "/api/v1/ontology/proposals/" + created.ID + "/blast-radius")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("oversized active blast radius status = %d, want 503: %s",
			response.StatusCode, data)
	}
}

func TestOntologyProposalBlastRadiusRejectsOverLimitReport(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	active := disjointPropertyOntologyVersion(t, "active", webapi.MaxOntologyProperties)
	_, server := ontologyProposalServer(t, corpus, active)
	created := createOntologyProposalWithRequest(
		t, server,
		disjointPropertyProposalRequest("proposed", webapi.MaxOntologyProperties),
	)

	response, err := server.Client().Get(
		server.URL + "/api/v1/ontology/proposals/" + created.ID + "/blast-radius")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("over-limit blast radius status = %d, want 503: %s",
			response.StatusCode, data)
	}
}

func ontologyProposalServer(
	t *testing.T,
	corpus *explorer.Explorer,
	active ontology.OntologyVersion,
) (*webapi.EmbeddedService, *httptest.Server) {
	t.Helper()
	service, err := webapi.NewEmbeddedService(corpus)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.SetOntologyVersion(active); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(nil)
	handler, err := webapi.NewHandler(service, server.Listener.Addr().String())
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	server.Config.Handler = handler
	server.Start()
	t.Cleanup(server.Close)
	return service, server
}

func createOntologyProposal(
	t *testing.T,
	server *httptest.Server,
	version string,
) webapi.OntologyProposalProjection {
	t.Helper()
	return createOntologyProposalWithRequest(t, server, ontologyProposalRequest(version))
}

func createOntologyProposalWithRequest(
	t *testing.T,
	server *httptest.Server,
	request webapi.CreateOntologyProposalRequest,
) webapi.OntologyProposalProjection {
	t.Helper()
	body := mustJSON(t, request)
	response, err := server.Client().Post(
		server.URL+"/api/v1/ontology/proposals",
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("create proposal status = %d, want 201: %s",
			response.StatusCode, data)
	}
	var out webapi.OntologyProposalResponse
	if err := json.NewDecoder(response.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Proposal
}

func getOntologyProposalBlastRadius(
	t *testing.T,
	server *httptest.Server,
	proposalID string,
) (webapi.OntologyBlastRadiusReport, []byte) {
	t.Helper()
	response, err := server.Client().Get(
		server.URL + "/api/v1/ontology/proposals/" + proposalID + "/blast-radius")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("blast radius status = %d, want 200: %s", response.StatusCode, data)
	}
	var out webapi.OntologyBlastRadiusResponse
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out.BlastRadius, data
}

func transitionOntologyProposal(
	t *testing.T,
	server *httptest.Server,
	proposalID string,
	state string,
) webapi.OntologyProposalProjection {
	t.Helper()
	body := bytes.NewBufferString(
		`{"state":` + strconvQuote(state) + `,"note":"test transition"}`)
	response, err := server.Client().Post(
		server.URL+"/api/v1/ontology/proposals/"+proposalID+"/transition",
		"application/json",
		body,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("transition proposal to %s status = %d, want 200: %s",
			state, response.StatusCode, data)
	}
	var out webapi.OntologyProposalResponse
	if err := json.NewDecoder(response.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out.Proposal
}

func getOntologyProposals(
	t *testing.T,
	server *httptest.Server,
) webapi.OntologyProposalsResponse {
	t.Helper()
	response, err := server.Client().Get(server.URL + "/api/v1/ontology/proposals")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("proposal list status = %d, want 200: %s", response.StatusCode, data)
	}
	var out webapi.OntologyProposalsResponse
	if err := json.NewDecoder(response.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func ontologyProposalRequest(version string) webapi.CreateOntologyProposalRequest {
	return webapi.CreateOntologyProposalRequest{
		Rationale: "refine the workspace ontology",
		ProposedVersion: webapi.OntologyProposalVersionDraft{
			Version: version,
			Properties: []webapi.OntologyProposalPropertyDraft{
				{Key: "name", Name: "Name", ValueType: ontology.ValueString},
				{Key: "role", Name: "Role", ValueType: ontology.ValueString},
			},
			Concepts: []webapi.OntologyProposalConceptDraft{
				{Key: "person", Name: "Person", Properties: []string{"name"}},
				{Key: "organization", Name: "Organization", Properties: []string{"name"}},
			},
			Relationships: []webapi.OntologyProposalRelationshipDraft{
				{
					Key:          "member_of",
					Name:         "Member of",
					FromConcepts: []string{"person"},
					ToConcepts:   []string{"organization"},
					Properties:   []string{"role"},
					Directed:     true,
				},
			},
		},
	}
}

func blastRadiusProposalRequest() webapi.CreateOntologyProposalRequest {
	return webapi.CreateOntologyProposalRequest{
		Rationale: "show destructive ontology consequences",
		ProposedVersion: webapi.OntologyProposalVersionDraft{
			Version: "v2",
			Properties: []webapi.OntologyProposalPropertyDraft{
				{Key: "name", Name: "Name", ValueType: ontology.ValueNumber},
				{Key: "age", Name: "Age", ValueType: ontology.ValueInteger},
				{Key: "imo", Name: "IMO", ValueType: ontology.ValueString},
			},
			Concepts: []webapi.OntologyProposalConceptDraft{
				{Key: "person", Name: "Person", Properties: []string{"name"}},
				{Key: "organization", Name: "Organization", Properties: []string{"name"}},
				{Key: "vessel", Name: "Vessel", Properties: []string{"name", "imo"}},
			},
			Relationships: []webapi.OntologyProposalRelationshipDraft{
				{
					Key:          "member_of",
					Name:         "Member of",
					FromConcepts: []string{"person"},
					ToConcepts:   []string{"vessel"},
					Directed:     false,
				},
				{
					Key:          "port_call",
					Name:         "Port call",
					FromConcepts: []string{"vessel"},
					ToConcepts:   []string{"organization"},
					Directed:     true,
				},
			},
		},
	}
}

func blastRadiusOntologyVersion(t *testing.T) ontology.OntologyVersion {
	t.Helper()
	name, err := ontology.NewPropertyDefinition(
		"name", "Name", "", ontology.ValueString, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	age, err := ontology.NewPropertyDefinition(
		"age", "Age", "", ontology.ValueInteger, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	role, err := ontology.NewPropertyDefinition(
		"role", "Role", "", ontology.ValueString, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	person, err := ontology.NewConceptDefinition(
		"person", "Person", "", []shoal.ID{name.ID(), age.ID()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	organization, err := ontology.NewConceptDefinition(
		"organization", "Organization", "", []shoal.ID{name.ID()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	caseFile, err := ontology.NewConceptDefinition(
		"case_file", "Case file", "", []shoal.ID{name.ID()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	memberOf, err := ontology.NewRelationshipDefinition(
		"member_of", "Member of", "",
		[]shoal.ID{person.ID()}, []shoal.ID{organization.ID()},
		[]shoal.ID{role.ID()}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	referencedIn, err := ontology.NewRelationshipDefinition(
		"referenced_in", "Referenced in", "",
		[]shoal.ID{person.ID()}, []shoal.ID{caseFile.ID()},
		[]shoal.ID{role.ID()}, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := ontology.NewOntologySchema("workspace", "Workspace", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	version, err := ontology.NewOntologyVersion(
		schema, "v1", time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC),
		[]ontology.ConceptDefinition{person, organization, caseFile},
		[]ontology.RelationshipDefinition{memberOf, referencedIn},
		[]ontology.PropertyDefinition{name, age, role},
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	return version
}

func disjointPropertyOntologyVersion(
	t *testing.T,
	prefix string,
	propertyCount uint32,
) ontology.OntologyVersion {
	t.Helper()
	properties := make([]ontology.PropertyDefinition, 0, propertyCount)
	for index := uint32(0); index < propertyCount; index++ {
		key := prefix + "_property_" + strconvUint(index)
		property, err := ontology.NewPropertyDefinition(
			key, "Property "+strconvUint(index), "", ontology.ValueString, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		properties = append(properties, property)
	}
	concept, err := ontology.NewConceptDefinition(
		prefix+"_concept", "Concept", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := ontology.NewOntologySchema("workspace", "Workspace", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	version, err := ontology.NewOntologyVersion(
		schema, "v1", time.Date(2026, 9, 4, 0, 0, 0, 0, time.UTC),
		[]ontology.ConceptDefinition{concept}, nil, properties, nil)
	if err != nil {
		t.Fatal(err)
	}
	return version
}

func disjointPropertyProposalRequest(
	prefix string,
	propertyCount uint32,
) webapi.CreateOntologyProposalRequest {
	properties := make([]webapi.OntologyProposalPropertyDraft, 0, propertyCount)
	for index := uint32(0); index < propertyCount; index++ {
		properties = append(properties, webapi.OntologyProposalPropertyDraft{
			Key:       prefix + "_property_" + strconvUint(index),
			Name:      "Property " + strconvUint(index),
			ValueType: ontology.ValueString,
		})
	}
	return webapi.CreateOntologyProposalRequest{
		Rationale: "exercise blast radius report bounds",
		ProposedVersion: webapi.OntologyProposalVersionDraft{
			Version:    "v2",
			Properties: properties,
			Concepts: []webapi.OntologyProposalConceptDraft{
				{Key: prefix + "_concept", Name: "Concept"},
			},
		},
	}
}

func assertionUnderOntology(
	t *testing.T,
	identity ontology.OntologyIdentity,
) ontology.Assertion {
	t.Helper()
	property, err := ontology.NewPropertyDefinition(
		"title", "Title", "", ontology.ValueString, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	citation := document.Citation{
		DocumentID: "document-1", RevisionID: "revision-1",
		SectionID: "section-1", SpanID: "span-1",
		Range: document.SourceRange{
			Start: document.SourcePosition{Offset: 0, Page: 1},
			End:   document.SourcePosition{Offset: 7, Page: 1},
		},
	}
	evidence, err := ontology.NewEvidenceRef(citation, "subject", nil)
	if err != nil {
		t.Fatal(err)
	}
	provenance, err := ontology.NewExtractionProvenance(
		"test-provider", "test-model", "2026-08", "ontology-v1", "3",
		"fake-extractor", "1.2.0", nil)
	if err != nil {
		t.Fatal(err)
	}
	value, err := ontology.NewStringValue("subject")
	if err != nil {
		t.Fatal(err)
	}
	assertion, err := ontology.NewAssertion(
		shoal.ID("entity:person-1"), property.ID(), value,
		ontology.AssertionExplicit, 0.9, []ontology.EvidenceRef{evidence},
		provenance, nil, ontology.WithAssertionOntology(identity))
	if err != nil {
		t.Fatal(err)
	}
	return assertion
}

func conceptChangeByKey(
	values []webapi.OntologyConceptChangeProjection,
	key string,
) *webapi.OntologyConceptChangeProjection {
	for index := range values {
		if values[index].Before.Key == key {
			return &values[index]
		}
	}
	return nil
}

func relationChangeByKey(
	values []webapi.OntologyRelationChangeProjection,
	key string,
) *webapi.OntologyRelationChangeProjection {
	for index := range values {
		if values[index].Before.Key == key {
			return &values[index]
		}
	}
	return nil
}

func propertyChangeByKey(
	values []webapi.OntologyPropertyChangeProjection,
	key string,
) *webapi.OntologyPropertyChangeProjection {
	for index := range values {
		if values[index].Before.Key == key {
			return &values[index]
		}
	}
	return nil
}

func conceptByKey(
	values []webapi.OntologyConceptProjection,
	key string,
) *webapi.OntologyConceptProjection {
	for index := range values {
		if values[index].Key == key {
			return &values[index]
		}
	}
	return nil
}

func relationByKey(
	values []webapi.OntologyRelationProjection,
	key string,
) *webapi.OntologyRelationProjection {
	for index := range values {
		if values[index].Key == key {
			return &values[index]
		}
	}
	return nil
}

func propertyByKey(
	values []webapi.OntologyPropertyProjection,
	key string,
) *webapi.OntologyPropertyProjection {
	for index := range values {
		if values[index].Key == key {
			return &values[index]
		}
	}
	return nil
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func strconvQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
