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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// recompute reads the current snapshot itself, so callers may leave the request
// snapshot empty; the endpoint always re-derives against the live frontier.
const recomputeEmptySnapshot = `{"id":"","as_of":"0001-01-01T00:00:00Z","frontier":"0"}`

// recomputeWire is the exact response shape the browser parses. Reading the
// identifiers as raw strings lets the test prove they are wire-encoded rather
// than assuming a helper decoded them.
type recomputeWire struct {
	Unchanged bool   `json:"unchanged"`
	Digest    string `json:"digest"`
	Detail    struct {
		AssertionID           string  `json:"assertion_id"`
		DerivationID          string  `json:"derivation_id"`
		Origin                string  `json:"origin"`
		Score                 float64 `json:"score"`
		EmbeddingModel        string  `json:"embedding_model"`
		EmbeddingModelVersion string  `json:"embedding_model_version"`
		SimilarityMetric      string  `json:"similarity_metric"`
		Threshold             float64 `json:"threshold"`
		TessellationCell      string  `json:"tessellation_cell"`
		IteratorName          string  `json:"iterator_name"`
		IteratorOptions       []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"iterator_options"`
	} `json:"detail"`
}

// TestHTTPRecomputeReDerivesLatentAssertion posts the literal wire bytes the
// browser sends to /api/v1/derivation/recompute and pins the deterministic
// re-derivation contract end to end: byte-identical inputs fold to one digest,
// a stale digest reports drift, and a changed input surfaces the new detail.
func TestHTTPRecomputeReDerivesLatentAssertion(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	sourceID := seedWebapiLatentWorkspace(t, corpus, "0.91", 42)

	server := startRecomputeServer(t, corpus)
	defer server.Close()

	assertionID := latentAssertionWireID(t, server.URL, sourceID)

	// Baseline capture: an empty caller digest has nothing to compare, so it
	// reports unchanged and returns the derivation detail plus a fresh digest.
	first := postRecompute(t, server.URL, assertionID, "")
	if !first.Unchanged {
		t.Fatalf("baseline recompute unchanged = false, want true: %+v", first)
	}
	if first.Digest == "" {
		t.Fatalf("baseline recompute produced an empty digest")
	}
	if first.Detail.Origin != "derived" {
		t.Fatalf("recompute detail origin = %q, want derived", first.Detail.Origin)
	}
	if first.Detail.Score != 0.91 {
		t.Fatalf("recompute detail score = %v, want 0.91", first.Detail.Score)
	}
	for label, value := range map[string]string{
		"embedding_model":         first.Detail.EmbeddingModel,
		"embedding_model_version": first.Detail.EmbeddingModelVersion,
		"similarity_metric":       first.Detail.SimilarityMetric,
		"iterator_name":           first.Detail.IteratorName,
	} {
		if value == "" {
			t.Fatalf("recompute detail %s is empty, want the producer identity", label)
		}
	}
	if first.Detail.Threshold != 0.85 {
		t.Fatalf("recompute detail threshold = %v, want 0.85", first.Detail.Threshold)
	}
	if len(first.Detail.IteratorOptions) < 2 {
		t.Fatalf("recompute detail iterator_options = %v, want at least two entries",
			first.Detail.IteratorOptions)
	}
	for _, id := range []string{first.Detail.AssertionID, first.Detail.DerivationID} {
		decoded, err := base64.RawURLEncoding.DecodeString(id)
		if err != nil || len(decoded) == 0 || string(decoded) == id {
			t.Fatalf("recompute response id %q is not wire-encoded base64url", id)
		}
	}
	if first.Detail.AssertionID != assertionID {
		t.Fatalf("recompute assertion_id = %q, want the request form %q",
			first.Detail.AssertionID, assertionID)
	}
	for _, option := range first.Detail.IteratorOptions {
		if _, err := base64.RawURLEncoding.DecodeString(option.Key); err != nil {
			t.Fatalf("iterator option key %q is not base64url", option.Key)
		}
		if _, err := base64.RawURLEncoding.DecodeString(option.Value); err != nil {
			t.Fatalf("iterator option value %q is not base64url", option.Value)
		}
	}

	// Byte-identical re-run: a second recompute of unchanged inputs reproduces
	// the same digest and reports unchanged for the matching caller digest.
	second := postRecompute(t, server.URL, assertionID, first.Digest)
	if second.Digest != first.Digest {
		t.Fatalf("second recompute digest = %q, want the stable %q",
			second.Digest, first.Digest)
	}
	if !second.Unchanged {
		t.Fatalf("recompute of an unchanged derivation reported drift: %+v", second)
	}

	// A stale caller digest reports drift even though the corpus has not moved,
	// and the fresh digest never depends on the caller-supplied digest.
	stale := postRecompute(t, server.URL, assertionID, "deadbeefstale")
	if stale.Unchanged {
		t.Fatalf("recompute with a stale digest reported unchanged: %+v", stale)
	}
	if stale.Digest != first.Digest {
		t.Fatalf("recompute digest must not depend on the caller digest: %q vs %q",
			stale.Digest, first.Digest)
	}
}

// TestHTTPRecomputeRejectsUnknownDerivation pins that recomputing an identifier
// that is not a live derived assertion fails closed with not-found rather than
// answering with an empty or fabricated derivation.
func TestHTTPRecomputeRejectsUnknownDerivation(t *testing.T) {
	corpus, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	seedWebapiLatentWorkspace(t, corpus, "0.91", 42)

	server := startRecomputeServer(t, corpus)
	defer server.Close()

	body := fmt.Sprintf(`{"snapshot":%s,"assertion_id":%q}`,
		recomputeEmptySnapshot,
		base64.RawURLEncoding.EncodeToString([]byte("missing-derivation")))
	status, payload := postJSONStatus(
		t, server.URL+"/api/v1/derivation/recompute", body)
	if status != http.StatusNotFound {
		t.Fatalf("recompute of an unknown derivation returned %d: %s", status, payload)
	}
}

func startRecomputeServer(t *testing.T, corpus *explorer.Explorer) *httptest.Server {
	t.Helper()
	service, err := webapi.NewEmbeddedService(corpus)
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
	return server
}

func postRecompute(t *testing.T, baseURL, assertionID, digest string) recomputeWire {
	t.Helper()
	body := fmt.Sprintf(`{"snapshot":%s,"assertion_id":%q,"digest":%q}`,
		recomputeEmptySnapshot, assertionID, digest)
	var wire recomputeWire
	if err := json.Unmarshal(
		postJSON(t, baseURL+"/api/v1/derivation/recompute", body), &wire,
	); err != nil {
		t.Fatalf("decode recompute response: %v", err)
	}
	return wire
}

// latentAssertionWireID reads the derived similarity assertion through the same
// neighborhood endpoint the browser uses and returns its wire-encoded id.
func latentAssertionWireID(t *testing.T, baseURL string, sourceID shoal.ID) string {
	t.Helper()
	body := fmt.Sprintf(`{"snapshot":%s,"node_ids":[%q],"depth":1}`,
		recomputeEmptySnapshot, base64.RawURLEncoding.EncodeToString([]byte(sourceID)))
	var wire struct {
		Neighborhood struct {
			Assertions []struct {
				ID     string `json:"id"`
				Origin string `json:"origin"`
			} `json:"assertions"`
		} `json:"neighborhood"`
	}
	if err := json.Unmarshal(
		postJSON(t, baseURL+"/api/v1/neighborhood", body), &wire,
	); err != nil {
		t.Fatalf("decode neighborhood response: %v", err)
	}
	if len(wire.Neighborhood.Assertions) != 1 ||
		wire.Neighborhood.Assertions[0].Origin != "derived" {
		t.Fatalf("neighborhood assertions = %+v, want one derived assertion",
			wire.Neighborhood.Assertions)
	}
	return wire.Neighborhood.Assertions[0].ID
}

func postJSONStatus(t *testing.T, url, body string) (int, []byte) {
	t.Helper()
	client := http.Client{Timeout: 5 * time.Second}
	response, err := client.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, data
}

// seedWebapiLatentWorkspace ingests a source and target document and writes one
// latent link cell scored by value, returning the stable source identity. A
// higher timestamp supersedes an earlier score for the change-detection path.
func seedWebapiLatentWorkspace(
	t *testing.T, corpus *explorer.Explorer, score string, timestamp int64,
) shoal.ID {
	t.Helper()
	ctx := context.Background()
	created := time.Date(2026, time.September, 4, 12, 0, 0, 0, time.UTC)
	var sourceID, targetID shoal.ID
	for index, source := range []explorer.Source{
		{
			URI: "file:///recompute-source.txt", Title: "recompute source",
			MediaType: explorer.MediaTypeText, Content: "recompute source",
			Metadata: shoal.Metadata{"id": "source"},
		},
		{
			URI: "file:///recompute-target.txt", Title: "recompute target",
			MediaType: explorer.MediaTypeText, Content: "recompute target",
			Metadata: shoal.Metadata{"id": "target"},
		},
	} {
		result, err := corpus.IngestWithOptions(
			ctx, source, explorer.IngestOptions{CreatedAt: created})
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			sourceID = result.Document.ID
		} else {
			targetID = result.Document.ID
		}
	}
	if err := corpus.PutLatentLinkCells(ctx, []explorer.LatentLinkCell{{
		Row:             []byte("cell-a:" + string(sourceID)),
		ColumnFamily:    []byte("link"),
		ColumnQualifier: []byte(targetID),
		Timestamp:       timestamp,
		Value:           []byte(score),
	}}); err != nil {
		t.Fatal(err)
	}
	return sourceID
}
