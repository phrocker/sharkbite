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

package authorized_test

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	publicSpaceIdentity = "provider=local/model=public-v1/dimensions=8"
	hiddenSpaceIdentity = "provider=hosted-secret/model=hidden-v9/dimensions=8"
)

type reportingVectorBackend struct {
	explorer.Client
	scorer authorized.VectorScorer

	mu              sync.Mutex
	spaces          map[shoal.ID]string
	unavailable     map[string]bool
	fanoutLimit     int
	invalidIdentity bool
	beforeRetrieve  func(context.Context) error
	untrustedExtra  string
	retrievalScopes [][]shoal.ID
	scoringScopes   [][]shoal.ID
}

func (b *reportingVectorBackend) Retrieve(
	ctx context.Context,
	request retrieval.Request,
) (retrieval.Response, error) {
	event, documents := b.eventForDocuments(request.Scope.DocumentIDs, false)
	b.mu.Lock()
	b.retrievalScopes = append(b.retrievalScopes, documents)
	hook := b.beforeRetrieve
	extra := b.untrustedExtra
	b.mu.Unlock()
	if extra != "" {
		event.SpaceIdentities = append(event.SpaceIdentities, extra)
		sort.Strings(event.SpaceIdentities)
		event.ProviderCalls++
	}
	explorer.ReportEmbeddingQueryEvent(ctx, event)
	if hook != nil {
		if err := hook(ctx); err != nil {
			return retrieval.Response{}, err
		}
	}
	if event.FanoutExceeded {
		return retrieval.Response{}, shoal.NewError(
			shoal.ErrorUnavailable,
			"raw fanout refusal included "+strings.Join(event.SpaceIdentities, ","),
		)
	}
	if len(event.Unavailable) > 0 {
		return retrieval.Response{}, shoal.NewError(
			shoal.ErrorUnavailable,
			"raw unavailable identity "+event.Unavailable[0],
		)
	}
	return b.Client.Retrieve(
		explorer.WithEmbeddingQueryObserver(ctx, nil),
		request,
	)
}

func (b *reportingVectorBackend) VectorScores(
	ctx context.Context,
	request explorer.VectorScoreRequest,
) (map[shoal.ID]shoal.Score, error) {
	documentIDs := make([]shoal.ID, 0, len(request.Citations))
	for _, citation := range request.Citations {
		documentIDs = append(documentIDs, citation.DocumentID)
	}
	event, documents := b.eventForDocuments(documentIDs, false)
	b.mu.Lock()
	b.scoringScopes = append(b.scoringScopes, documents)
	b.mu.Unlock()
	explorer.ReportEmbeddingQueryEvent(ctx, event)
	if event.FanoutExceeded {
		return nil, shoal.NewError(
			shoal.ErrorUnavailable,
			"raw fanout refusal included "+strings.Join(event.SpaceIdentities, ","),
		)
	}
	if len(event.Unavailable) > 0 {
		return nil, shoal.NewError(
			shoal.ErrorUnavailable,
			"raw unavailable identity "+event.Unavailable[0],
		)
	}
	return b.scorer.VectorScores(
		explorer.WithEmbeddingQueryObserver(ctx, nil),
		request,
	)
}

func (b *reportingVectorBackend) eventForDocuments(
	documentIDs []shoal.ID,
	cacheHit bool,
) (explorer.EmbeddingQueryEvent, []shoal.ID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	documents := append([]shoal.ID(nil), documentIDs...)
	sort.Slice(documents, func(left, right int) bool {
		return documents[left] < documents[right]
	})
	spaces := make(map[string]struct{})
	for _, documentID := range documents {
		if identity := b.spaces[documentID]; identity != "" {
			spaces[identity] = struct{}{}
		}
	}
	identities := make([]string, 0, len(spaces))
	for identity := range spaces {
		identities = append(identities, identity)
	}
	sort.Strings(identities)
	if b.invalidIdentity {
		identities = []string{""}
	}
	event := explorer.EmbeddingQueryEvent{
		SpaceIdentities: identities,
		FanoutLimit:     b.fanoutLimit,
	}
	if b.fanoutLimit > 0 && len(identities) > b.fanoutLimit {
		event.FanoutExceeded = true
		return event, documents
	}
	for _, identity := range identities {
		event.Attempted = append(event.Attempted, identity)
		if b.unavailable[identity] {
			event.Unavailable = append(event.Unavailable, identity)
		} else {
			event.Completed = append(event.Completed, identity)
		}
	}
	if cacheHit {
		event.CacheHits = len(identities)
	} else {
		event.ProviderCalls = len(identities)
	}
	return event, documents
}

func (b *reportingVectorBackend) scopes() ([][]shoal.ID, [][]shoal.ID) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return cloneIDMatrix(b.retrievalScopes), cloneIDMatrix(b.scoringScopes)
}

func cloneIDMatrix(values [][]shoal.ID) [][]shoal.ID {
	cloned := make([][]shoal.ID, len(values))
	for index := range values {
		cloned[index] = append([]shoal.ID(nil), values[index]...)
	}
	return cloned
}

type embeddingReportFixture struct {
	*fixture
	base    *explorer.Explorer
	backend *reportingVectorBackend
	clientA *authorized.Client
	clientB *authorized.Client
}

func newEmbeddingReportFixture(t *testing.T) *embeddingReportFixture {
	t.Helper()
	f := newFixture(t)
	base, err := explorer.OpenWithOptions(t.TempDir(), explorer.Options{
		Embedder: model.FakeEmbedder{
			Model: "authorized-report", Dimensions: 8,
		},
		MaxEmbeddingSpaceFanout: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	scorer, ok := any(base).(authorized.VectorScorer)
	if !ok {
		t.Fatal("embedded explorer does not implement VectorScorer")
	}
	backend := &reportingVectorBackend{
		Client: base, scorer: scorer,
		spaces:      make(map[shoal.ID]string),
		unavailable: make(map[string]bool),
		fanoutLimit: 8,
	}
	return &embeddingReportFixture{
		fixture: f,
		base:    base,
		backend: backend,
		clientA: f.newClient(t, backend, f.store, f.sourceA, f.policyA, nil),
		clientB: f.newClient(t, backend, f.store, f.sourceB, f.policyB, nil),
	}
}

func (f *embeddingReportFixture) ingest(
	t *testing.T,
	client *authorized.Client,
	ctx context.Context,
	uri, content, space string,
) explorer.IngestResult {
	t.Helper()
	result, err := client.Ingest(ctx, explorer.Source{
		URI: uri, MediaType: explorer.MediaTypeText, Content: content,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.backend.mu.Lock()
	f.backend.spaces[result.Document.ID] = space
	f.backend.mu.Unlock()
	return result
}

func vectorQuery(text string) retrieval.Request {
	return retrieval.Request{
		Text: text, TopK: 10,
		Modes: []retrieval.Mode{retrieval.ModeVector},
	}
}

func TestAuthorizedEmbeddingReportDoesNotExposeHiddenSpace(t *testing.T) {
	f := newEmbeddingReportFixture(t)
	visible := f.ingest(
		t, f.clientA, f.admin(t),
		"file:///visible-vector.txt", "visible vector evidence",
		publicSpaceIdentity,
	)

	var callbacks []authorized.EmbeddingQueryReport
	aliceCtx := authorized.WithEmbeddingQueryObserver(
		f.alice(t),
		func(report authorized.EmbeddingQueryReport) {
			callbacks = append(callbacks, report)
		},
	)
	_, before, err := f.clientA.RetrieveWithReport(
		aliceCtx, vectorQuery("visible vector"))
	if err != nil {
		t.Fatal(err)
	}
	if before.Embedding == nil ||
		len(before.Embedding.Spaces) != 1 ||
		before.Embedding.Spaces[0].Status != authorized.EmbeddingSpaceAvailable {
		t.Fatalf("initial embedding report = %+v", before.Embedding)
	}
	publicID := before.Embedding.Spaces[0].ID

	hidden := f.ingest(
		t, f.clientB, f.admin(t),
		"file:///hidden-vector.txt", "hidden vector evidence",
		hiddenSpaceIdentity,
	)
	f.backend.mu.Lock()
	f.backend.untrustedExtra = hiddenSpaceIdentity
	f.backend.mu.Unlock()
	_, after, err := f.clientA.RetrieveWithReport(
		aliceCtx, vectorQuery("visible vector"))
	if err != nil {
		t.Fatal(err)
	}
	if after.Disclosure.Suppressed != 1 ||
		after.Embedding == nil || !after.Embedding.Suppressed {
		t.Fatalf("post-hidden report = %+v", after)
	}
	if len(after.Embedding.Spaces) != 1 ||
		after.Embedding.Spaces[0].ID != publicID ||
		after.Embedding.Spaces[0].Status != authorized.EmbeddingSpaceAvailable ||
		after.Embedding.Degraded {
		t.Fatalf("hidden space changed public report: before=%+v after=%+v",
			before.Embedding, after.Embedding)
	}
	for _, marker := range []string{publicSpaceIdentity, hiddenSpaceIdentity} {
		if strings.Contains(string(publicID), marker) ||
			strings.Contains(fmt.Sprint(after.Embedding), marker) {
			t.Fatalf("authorized report exposed raw embedding identity %q", marker)
		}
	}
	if len(callbacks) != 2 ||
		!reflect.DeepEqual(callbacks[1], *after.Embedding) {
		t.Fatalf("request callbacks = %+v, report = %+v", callbacks, after.Embedding)
	}

	retrievalScopes, scoringScopes := f.backend.scopes()
	for _, scopes := range [][][]shoal.ID{retrievalScopes, scoringScopes} {
		for _, scope := range scopes {
			if containsID(scope, hidden.Document.ID) {
				t.Fatalf("unauthorized document reached vector backend: %v", scope)
			}
			if !containsID(scope, visible.Document.ID) {
				t.Fatalf("authorized document missing from vector backend: %v", scope)
			}
		}
	}

	_, adminReport, err := f.clientA.RetrieveWithReport(
		f.admin(t), vectorQuery("visible hidden vector"))
	if err != nil {
		t.Fatal(err)
	}
	if adminReport.Disclosure.Suppressed != 0 ||
		adminReport.Embedding == nil ||
		len(adminReport.Embedding.Spaces) != 2 ||
		adminReport.Embedding.CacheHits != 0 ||
		adminReport.Embedding.ProviderCalls != 0 {
		t.Fatalf("admin mixed-space report = %+v", adminReport)
	}
	if adminReport.Embedding.Spaces[0].ID == adminReport.Embedding.Spaces[1].ID {
		t.Fatalf("distinct authorized spaces collapsed: %+v", adminReport.Embedding)
	}
}

func TestHiddenOnlyVectorScopeDoesNotInvokeProvider(t *testing.T) {
	f := newEmbeddingReportFixture(t)
	hidden := f.ingest(
		t, f.clientB, f.admin(t),
		"file:///hidden-only-vector.txt", "hidden only vector evidence",
		hiddenSpaceIdentity,
	)
	beforeRetrieval, beforeScoring := f.backend.scopes()
	response, report, err := f.clientA.RetrieveWithReport(
		f.alice(t),
		retrieval.Request{
			Text:  "hidden only",
			TopK:  5,
			Modes: []retrieval.Mode{retrieval.ModeVector},
			Scope: retrieval.Scope{DocumentIDs: []shoal.ID{
				hidden.Document.ID,
			}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Results) != 0 ||
		report.Disclosure.Suppressed != 1 ||
		report.Embedding == nil ||
		len(report.Embedding.Spaces) != 0 ||
		report.Embedding.Observed ||
		report.Embedding.Degraded ||
		!report.Embedding.Suppressed {
		t.Fatalf("hidden-only response/report = %+v / %+v", response, report)
	}
	afterRetrieval, afterScoring := f.backend.scopes()
	if len(afterRetrieval) != len(beforeRetrieval) ||
		len(afterScoring) != len(beforeScoring) {
		t.Fatalf("hidden-only query invoked vector backend: retrieve=%v score=%v",
			afterRetrieval, afterScoring)
	}
}

func TestAuthorizedUnavailableAndFanoutReportsAreHonest(t *testing.T) {
	t.Run("unavailable", func(t *testing.T) {
		f := newEmbeddingReportFixture(t)
		f.ingest(
			t, f.clientA, f.admin(t),
			"file:///unavailable-vector.txt", "unavailable vector evidence",
			publicSpaceIdentity,
		)
		f.backend.unavailable[publicSpaceIdentity] = true
		var callback authorized.EmbeddingQueryReport
		ctx := authorized.WithEmbeddingQueryObserver(
			f.alice(t),
			func(report authorized.EmbeddingQueryReport) { callback = report },
		)
		_, report, err := f.clientA.RetrieveWithReport(
			ctx, vectorQuery("unavailable vector"))
		if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
			t.Fatalf("unavailable error = %v", err)
		}
		if strings.Contains(err.Error(), publicSpaceIdentity) {
			t.Fatalf("error leaked raw authorized space identity: %v", err)
		}
		if report.Embedding == nil ||
			!report.Embedding.Degraded ||
			!report.Embedding.Observed ||
			len(report.Embedding.Spaces) != 1 ||
			report.Embedding.Spaces[0].Status != authorized.EmbeddingSpaceUnavailable {
			t.Fatalf("unavailable report = %+v", report.Embedding)
		}
		if !reflect.DeepEqual(callback, *report.Embedding) {
			t.Fatalf("unavailable callback = %+v, report = %+v", callback, report.Embedding)
		}
	})

	t.Run("partial completion", func(t *testing.T) {
		f := newEmbeddingReportFixture(t)
		f.ingest(
			t, f.clientA, f.admin(t),
			"file:///available-vector.txt", "available vector evidence",
			publicSpaceIdentity,
		)
		f.ingest(
			t, f.clientB, f.admin(t),
			"file:///later-unavailable-vector.txt", "later unavailable vector evidence",
			hiddenSpaceIdentity,
		)
		f.backend.unavailable[hiddenSpaceIdentity] = true
		_, report, err := f.clientA.RetrieveWithReport(
			f.admin(t), vectorQuery("partial vector"))
		if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
			t.Fatalf("partial completion error = %v", err)
		}
		statuses := map[authorized.EmbeddingSpaceStatus]int{}
		for _, space := range report.Embedding.Spaces {
			statuses[space.Status]++
		}
		if statuses[authorized.EmbeddingSpaceAvailable] != 1 ||
			statuses[authorized.EmbeddingSpaceUnavailable] != 1 ||
			len(statuses) != 2 {
			t.Fatalf("partial completion statuses = %+v", report.Embedding)
		}
	})

	t.Run("fanout", func(t *testing.T) {
		f := newEmbeddingReportFixture(t)
		f.ingest(
			t, f.clientA, f.admin(t),
			"file:///fanout-a.txt", "fanout a",
			publicSpaceIdentity,
		)
		f.ingest(
			t, f.clientA, f.admin(t),
			"file:///fanout-b.txt", "fanout b",
			hiddenSpaceIdentity,
		)
		f.backend.fanoutLimit = 1
		_, report, err := f.clientA.RetrieveWithReport(
			f.admin(t), vectorQuery("fanout"))
		if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
			t.Fatalf("fanout error = %v", err)
		}
		if report.Embedding == nil ||
			!report.Embedding.Degraded ||
			!report.Embedding.FanoutExceeded ||
			report.Embedding.FanoutLimit != 1 ||
			report.Embedding.ProviderCalls != 0 ||
			len(report.Embedding.Spaces) != 2 {
			t.Fatalf("fanout report = %+v", report.Embedding)
		}
		for _, space := range report.Embedding.Spaces {
			if space.Status != authorized.EmbeddingSpaceNotAttempted {
				t.Fatalf("fanout space = %+v", space)
			}
		}
	})
}

func TestAuthorizedEmbeddingReportFailsClosedForUnknownObservation(t *testing.T) {
	f := newEmbeddingReportFixture(t)
	f.ingest(
		t, f.clientA, f.admin(t),
		"file:///unknown-vector.txt", "unknown vector evidence",
		publicSpaceIdentity,
	)
	f.backend.invalidIdentity = true
	response, report, err := f.clientA.RetrieveWithReport(
		f.alice(t), vectorQuery("unknown vector"))
	if !shoal.IsErrorCode(err, shoal.ErrorInternal) {
		t.Fatalf("unknown identity error = %v", err)
	}
	if len(response.Results) != 0 ||
		report.Embedding == nil ||
		!report.Embedding.Degraded ||
		!report.Embedding.Observed ||
		len(report.Embedding.Spaces) != 0 {
		t.Fatalf("unknown identity response/report = %+v / %+v", response, report)
	}
}

func TestAuthorizedEmbeddingReportScrubsAfterCancellationAndRevocation(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(*testing.T, *embeddingReportFixture, context.Context) (authorized.RetrievalReport, error)
		code shoal.ErrorCode
	}{
		{
			name: "cancellation",
			run: func(t *testing.T, f *embeddingReportFixture, ctx context.Context) (authorized.RetrievalReport, error) {
				entered := make(chan struct{})
				f.backend.beforeRetrieve = func(ctx context.Context) error {
					close(entered)
					<-ctx.Done()
					return ctx.Err()
				}
				cancelCtx, cancel := context.WithCancel(ctx)
				type result struct {
					report authorized.RetrievalReport
					err    error
				}
				done := make(chan result, 1)
				go func() {
					_, report, err := f.clientA.RetrieveWithReport(
						cancelCtx, vectorQuery("cancel vector"))
					done <- result{report: report, err: err}
				}()
				<-entered
				cancel()
				got := <-done
				return got.report, got.err
			},
			code: shoal.ErrorCanceled,
		},
		{
			name: "revocation",
			run: func(t *testing.T, f *embeddingReportFixture, ctx context.Context) (authorized.RetrievalReport, error) {
				entered := make(chan struct{})
				release := make(chan struct{})
				f.backend.beforeRetrieve = func(context.Context) error {
					close(entered)
					<-release
					return nil
				}
				type result struct {
					report authorized.RetrievalReport
					err    error
				}
				done := make(chan result, 1)
				go func() {
					_, report, err := f.clientA.RetrieveWithReport(
						ctx, vectorQuery("revoke vector"))
					done <- result{report: report, err: err}
				}()
				<-entered
				f.reader.Set(f.domain, 2)
				close(release)
				got := <-done
				return got.report, got.err
			},
			code: shoal.ErrorUnavailable,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newEmbeddingReportFixture(t)
			f.ingest(
				t, f.clientA, f.admin(t),
				"file:///guarded-vector.txt", "guarded vector evidence",
				publicSpaceIdentity,
			)
			var callback authorized.EmbeddingQueryReport
			ctx := authorized.WithEmbeddingQueryObserver(
				f.alice(t),
				func(report authorized.EmbeddingQueryReport) { callback = report },
			)
			report, err := test.run(t, f, ctx)
			if !shoal.IsErrorCode(err, test.code) {
				t.Fatalf("guarded error = %v, want %s", err, test.code)
			}
			if report.Disclosure != (authorized.Disclosure{}) ||
				report.Embedding == nil ||
				report.Embedding.Suppressed ||
				report.Embedding.Restricted {
				t.Fatalf("guarded return report leaked disclosure: %+v", report)
			}
			if !callback.Degraded ||
				callback.Observed ||
				len(callback.Spaces) != 0 ||
				callback.CacheHits != 0 ||
				callback.ProviderCalls != 0 ||
				callback.FanoutExceeded ||
				callback.Suppressed ||
				callback.Restricted {
				t.Fatalf("guarded callback leaked pre-check report: %+v", callback)
			}
		})
	}
}

func TestAuthorizedEmbeddingCallbacksAreRequestLocalUnderConcurrency(t *testing.T) {
	f := newEmbeddingReportFixture(t)
	f.ingest(
		t, f.clientA, f.admin(t),
		"file:///concurrent-public-vector.txt", "public concurrent vector",
		publicSpaceIdentity,
	)
	f.ingest(
		t, f.clientB, f.admin(t),
		"file:///concurrent-hidden-vector.txt", "hidden concurrent vector",
		hiddenSpaceIdentity,
	)

	const requests = 12
	type outcome struct {
		admin  bool
		report authorized.EmbeddingQueryReport
		err    error
	}
	outcomes := make(chan outcome, requests)
	var wait sync.WaitGroup
	aliceCtx := f.alice(t)
	adminCtx := f.admin(t)
	for index := 0; index < requests; index++ {
		admin := index%2 == 0
		wait.Add(1)
		go func(index int, admin bool) {
			defer wait.Done()
			ctx := aliceCtx
			if admin {
				ctx = adminCtx
			}
			var callback authorized.EmbeddingQueryReport
			ctx = authorized.WithEmbeddingQueryObserver(
				ctx,
				func(report authorized.EmbeddingQueryReport) {
					callback = report
				},
			)
			_, _, err := f.clientA.RetrieveWithReport(
				ctx, vectorQuery(fmt.Sprintf("concurrent-%d", index)))
			outcomes <- outcome{admin: admin, report: callback, err: err}
		}(index, admin)
	}
	wait.Wait()
	close(outcomes)
	for outcome := range outcomes {
		if outcome.err != nil {
			t.Fatal(outcome.err)
		}
		wantSpaces := 1
		wantSuppressed := true
		if outcome.admin {
			wantSpaces = 2
			wantSuppressed = false
		}
		if len(outcome.report.Spaces) != wantSpaces ||
			outcome.report.Suppressed != wantSuppressed ||
			outcome.report.Degraded {
			t.Fatalf("request-local outcome admin=%v report=%+v",
				outcome.admin, outcome.report)
		}
	}
}

func containsID(values []shoal.ID, target shoal.ID) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

var _ authorized.VectorScorer = (*reportingVectorBackend)(nil)
