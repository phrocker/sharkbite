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

package webapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// fakeChangeBackend is an authorized backing client that serves only the change
// feed. It embeds the wide BoundedClient interface (left nil) purely to satisfy
// the method set NewEmbeddedService requires; only Changes is ever called on
// the feed path, so the nil methods are never invoked. The cursor is opaque to
// this service, so the fake simply echoes a canned sealed token and records the
// token it was handed.
type fakeChangeBackend struct {
	explorer.BoundedClient
	page        authorized.ChangeFeedPage
	err         error
	lastRequest authorized.ChangeFeedRequest
	calls       int
}

func (f *fakeChangeBackend) Changes(
	_ context.Context, request authorized.ChangeFeedRequest,
) (authorized.ChangeFeedPage, error) {
	f.calls++
	f.lastRequest = request
	if f.err != nil {
		return authorized.ChangeFeedPage{}, f.err
	}
	return f.page, nil
}

// realChanges opens a throwaway corpus, ingests count documents, and returns
// their change records so wire marshaling operates on genuine documents.
func realChanges(t *testing.T, count int) []explorer.DocumentChange {
	t.Helper()
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = corpus.Close() })
	ctx := context.Background()
	for i := 1; i <= count; i++ {
		if _, err := corpus.Ingest(ctx, explorer.Source{
			URI:       "file:///wire-" + string(rune('a'+i-1)) + ".md",
			MediaType: explorer.MediaTypeMarkdown,
			Content:   "# Wire\n\nSynthetic body for the change feed wire test.\n",
		}); err != nil {
			t.Fatal(err)
		}
	}
	feed, err := corpus.Changes(ctx, explorer.ChangeRequest{Since: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(feed.Changes) != count {
		t.Fatalf("seeded %d changes, want %d", len(feed.Changes), count)
	}
	return feed.Changes
}

func TestServiceChangesForwardsOpaqueCursorBothWays(t *testing.T) {
	changes := realChanges(t, 3)
	backend := &fakeChangeBackend{page: authorized.ChangeFeedPage{
		Changes: changes, Cursor: "sealed-token-1", More: false,
	}}
	service, err := webapi.NewEmbeddedService(backend)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	first, err := service.Changes(ctx, webapi.ChangesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	// The service is a pass-through: an empty request cursor reaches the backend
	// verbatim, and the backend's sealed cursor is returned unmodified. The
	// service never reads or rewrites the token.
	if backend.lastRequest.Cursor != "" {
		t.Fatalf("first backend cursor = %q, want empty", backend.lastRequest.Cursor)
	}
	if len(first.Changes) != 3 {
		t.Fatalf("changes = %d, want 3", len(first.Changes))
	}
	if first.Changes[0].Kind != string(explorer.ChangeKindDocumentPublished) {
		t.Fatalf("kind = %q", first.Changes[0].Kind)
	}
	if first.NextCursor != "sealed-token-1" {
		t.Fatalf("NextCursor = %q, want the backend's sealed token verbatim",
			first.NextCursor)
	}

	// Replaying the opaque cursor forwards it to the backend byte-for-byte.
	if _, err := service.Changes(ctx, webapi.ChangesRequest{Cursor: "sealed-token-1"}); err != nil {
		t.Fatal(err)
	}
	if backend.lastRequest.Cursor != "sealed-token-1" {
		t.Fatalf("resume cursor = %q, want it forwarded verbatim",
			backend.lastRequest.Cursor)
	}
}

func TestServiceChangesTruncatedPagePassesCursorAndLimit(t *testing.T) {
	changes := realChanges(t, 3)
	// The backend delivered only the first two changes, reports More, and hands
	// back the sealed resume token for the first undelivered change.
	backend := &fakeChangeBackend{page: authorized.ChangeFeedPage{
		Changes: changes[:2], Cursor: "sealed-resume", More: true,
	}}
	service, err := webapi.NewEmbeddedService(backend)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	page, err := service.Changes(ctx, webapi.ChangesRequest{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !page.More {
		t.Fatal("More = false, want true for a truncated page")
	}
	if backend.lastRequest.Limit != 2 {
		t.Fatalf("backend limit = %d, want 2", backend.lastRequest.Limit)
	}
	if page.NextCursor != "sealed-resume" {
		t.Fatalf("NextCursor = %q, want the backend's resume token", page.NextCursor)
	}

	// Resuming forwards the truncated page's sealed cursor unmodified.
	if _, err := service.Changes(ctx, webapi.ChangesRequest{Cursor: page.NextCursor}); err != nil {
		t.Fatal(err)
	}
	if backend.lastRequest.Cursor != "sealed-resume" {
		t.Fatalf("resume cursor = %q, want it forwarded verbatim",
			backend.lastRequest.Cursor)
	}
}

func TestServiceChangesRejectsOverLargeLimit(t *testing.T) {
	backend := &fakeChangeBackend{}
	service, err := webapi.NewEmbeddedService(backend)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Changes(context.Background(), webapi.ChangesRequest{
		Limit: webapi.MaxChangePageSize + 1,
	})
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("error = %v, want invalid_argument", err)
	}
	if backend.calls != 0 {
		t.Fatalf("backend was called %d times for a rejected limit", backend.calls)
	}
}

// TestServiceChangesPropagatesInvalidCursor confirms the service does not try
// to interpret the opaque cursor itself: an unopenable token is rejected by the
// backend (which holds the seal key) and the service forwards both the token
// and the resulting invalid-argument error without swallowing or reshaping it.
func TestServiceChangesPropagatesInvalidCursor(t *testing.T) {
	backend := &fakeChangeBackend{
		err: shoal.NewError(shoal.ErrorInvalidArgument,
			"change cursor is invalid; resynchronise"),
	}
	service, err := webapi.NewEmbeddedService(backend)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Changes(context.Background(), webapi.ChangesRequest{
		Cursor: "not-a-valid-token",
	})
	if !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("error = %v, want invalid_argument", err)
	}
	if backend.calls != 1 {
		t.Fatalf("backend calls = %d, want 1 (cursor forwarded, not pre-judged)",
			backend.calls)
	}
	if backend.lastRequest.Cursor != "not-a-valid-token" {
		t.Fatalf("backend cursor = %q, want it forwarded verbatim",
			backend.lastRequest.Cursor)
	}
}

func TestServiceChangesPropagatesResynchroniseConflict(t *testing.T) {
	backend := &fakeChangeBackend{
		err: shoal.NewError(shoal.ErrorConflict,
			"change cursor is ahead of the corpus; resynchronise"),
	}
	service, err := webapi.NewEmbeddedService(backend)
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Changes(context.Background(), webapi.ChangesRequest{})
	if !shoal.IsErrorCode(err, shoal.ErrorConflict) {
		t.Fatalf("error = %v, want conflict", err)
	}
}

func TestServiceChangesUnavailableWithoutFeedCapableBackend(t *testing.T) {
	// testService backs the embedded service with a raw explorer, which has no
	// authorization filtering and therefore does not implement the feed
	// backend. The feed must fail closed rather than serve an unfiltered feed.
	service, corpus, _, _ := testService(t)
	defer corpus.Close()
	_, err := service.Changes(context.Background(), webapi.ChangesRequest{})
	if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("error = %v, want unavailable", err)
	}
}

func TestHTTPChangesRoundTripAndGating(t *testing.T) {
	changes := realChanges(t, 2)
	backend := &fakeChangeBackend{page: authorized.ChangeFeedPage{
		Changes: changes, Cursor: "sealed-http", More: false,
	}}
	service, err := webapi.NewEmbeddedService(backend)
	if err != nil {
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
	defer server.Close()

	response, err := http.Post(
		server.URL+"/api/v1/changes", "application/json",
		bytes.NewBufferString(`{}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("changes status = %s", response.Status)
	}
	var decoded webapi.ChangesResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Changes) != 2 {
		t.Fatalf("http changes = %d, want 2", len(decoded.Changes))
	}
	if decoded.Changes[0].Document.Document.ID != changes[0].Document.ID {
		t.Fatalf("http change doc = %s, want %s",
			decoded.Changes[0].Document.Document.ID, changes[0].Document.ID)
	}
	if decoded.NextCursor != "sealed-http" {
		t.Fatalf("http NextCursor = %q, want the backend's sealed token",
			decoded.NextCursor)
	}

	// A raw-explorer-backed service must report the capability as unavailable
	// over HTTP rather than serving an unfiltered feed.
	rawService, corpus, _, _ := testService(t)
	defer corpus.Close()
	rawServer := httptest.NewUnstartedServer(nil)
	rawHandler, err := webapi.NewHandler(rawService, rawServer.Listener.Addr().String())
	if err != nil {
		rawServer.Close()
		t.Fatal(err)
	}
	rawServer.Config.Handler = rawHandler
	rawServer.Start()
	defer rawServer.Close()

	rawResponse, err := http.Post(
		rawServer.URL+"/api/v1/changes", "application/json",
		bytes.NewBufferString(`{}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer rawResponse.Body.Close()
	if rawResponse.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("raw-backed changes status = %s, want 503", rawResponse.Status)
	}
}
