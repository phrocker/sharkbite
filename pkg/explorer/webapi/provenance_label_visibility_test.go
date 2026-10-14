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
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized/authorizedtest"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const labelSubjectHeader = "X-Test-Label-Subject"

const (
	httpLabelledSession shoal.ID = "interaction.session_http_labelled"
	httpPlainSession    shoal.ID = "interaction.session_http_plain"
)

func labelID(id shoal.ID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id))
}

type labelHTTP struct {
	t       *testing.T
	handler *webapi.Handler
}

func newLabelHTTP(t *testing.T, f *authorizedtest.Fixture) labelHTTP {
	t.Helper()
	embedded, err := webapi.NewEmbeddedService(f.Client)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := webapi.NewAuthenticatedHandler(
		embedded,
		webapi.AuthenticatorFunc(func(request *http.Request) (auth.Decision, error) {
			return f.Decision(t, request.Header.Get(labelSubjectHeader)), nil
		}),
		f.Authority.Binder(), "example.test",
	)
	if err != nil {
		t.Fatal(err)
	}
	provenance, err := webapi.NewInteractionService(f.Client)
	if err != nil {
		t.Fatal(err)
	}
	if err := handler.SetInteractionProvider(provenance); err != nil {
		t.Fatal(err)
	}
	return labelHTTP{t: t, handler: handler}
}

// do returns the exact response bytes the handler wrote.
func (h labelHTTP) do(subject, method, path string, body any) []byte {
	h.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	} else {
		reader = bytes.NewReader(nil)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Host = "example.test"
	request.Header.Set(labelSubjectHeader, subject)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK && response.Code != http.StatusCreated {
		h.t.Fatalf("%s %s as %s = %d: %s",
			method, path, subject, response.Code, response.Body.String())
	}
	return response.Body.Bytes()
}

// raw returns the status and exact bytes of any response, refusals included.
func (h labelHTTP) raw(subject, method, path string, body any) (int, []byte) {
	h.t.Helper()
	reader := bytes.NewReader(nil)
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			h.t.Fatal(err)
		}
		reader = bytes.NewReader(encoded)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Host = "example.test"
	request.Header.Set(labelSubjectHeader, subject)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response, request)
	return response.Code, response.Body.Bytes()
}

// TestProvenanceHTTPServesLabelledRecordsOnlyToHolders drives list, inspect,
// fold and unfold over HTTP through the real authenticated handler, the real
// authorized client and the real corpus. A reader holding the labels receives
// the labelled records with their labels; one that does not receives exactly
// the bytes it would had they never been written (#564, #567, #568).
func TestProvenanceHTTPServesLabelledRecordsOnlyToHolders(t *testing.T) {
	f := authorizedtest.New(t)
	f.Record(t, authorizedtest.HolderSubject, httpLabelledSession,
		authorizedtest.SecretLabels)
	f.Record(t, authorizedtest.HolderSubject, httpPlainSession, nil)
	labelledFold := f.Fold(t, authorizedtest.HolderSubject, httpLabelledSession)
	plainFold := f.Fold(t, authorizedtest.HolderSubject, httpPlainSession)
	h := newLabelHTTP(t, f)
	holder, outsider := authorizedtest.HolderSubject, authorizedtest.OutsiderSubject
	const never shoal.ID = "interaction.session_http_never"
	holderSees := func(name string, body []byte) {
		t.Helper()
		if !bytes.Contains(body, []byte(authorizedtest.SecretLabels[0])) {
			t.Fatalf("%s: the holder lost the labels: %s", name, body)
		}
	}

	// List: the outsider's page is the holder's without the labelled items.
	listed := h.do(outsider, http.MethodGet, "/api/v1/provenance", nil)
	assertNoLeak(t, "provenance list", listed)
	holderListed := h.do(holder, http.MethodGet, "/api/v1/provenance", nil)
	holderSees("provenance list", holderListed)
	var page map[string]json.RawMessage
	if err := json.Unmarshal(holderListed, &page); err != nil {
		t.Fatal(err)
	}
	keep := func(name, field string, id shoal.ID) {
		var items []json.RawMessage
		if err := json.Unmarshal(page[name], &items); err != nil {
			t.Fatal(err)
		}
		if len(items) != 2 {
			t.Fatalf("the holder's %s lost records: %s", name, holderListed)
		}
		var kept []json.RawMessage
		for _, item := range items {
			var object map[string]json.RawMessage
			if err := json.Unmarshal(item, &object); err != nil {
				t.Fatal(err)
			}
			if string(object[field]) == `"`+labelID(id)+`"` {
				kept = append(kept, item)
			}
		}
		encoded, err := json.Marshal(kept)
		if err != nil {
			t.Fatal(err)
		}
		page[name] = encoded
	}
	keep("interactions", "session_id", httpPlainSession)
	keep("folds", "fold_id", plainFold.FoldID)
	want, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	var outsiderPage map[string]json.RawMessage
	if err := json.Unmarshal(listed, &outsiderPage); err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(outsiderPage)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("outsider list:\n%s\nwant, as if never written,\n%s", got, want)
	}

	// Inspect, unfold and fold: the refusal a never-written identifier gets.
	for _, probe := range []struct {
		name           string
		method, path   string
		body           any
		neverPath      string
		neverBody      any
		holderResponds bool
	}{
		{name: "inspect", method: http.MethodGet,
			path:      "/api/v1/provenance/" + labelID(httpLabelledSession),
			neverPath: "/api/v1/provenance/" + labelID(never)},
		{name: "unfold", method: http.MethodPost,
			path:      "/api/v1/provenance/unfold",
			body:      webapi.ProvenanceUnfoldRequest{FoldID: labelID(labelledFold.FoldID)},
			neverPath: "/api/v1/provenance/unfold",
			neverBody: webapi.ProvenanceUnfoldRequest{FoldID: labelID(never)}},
		{name: "fold", method: http.MethodPost,
			path:      "/api/v1/provenance/fold",
			body:      webapi.ProvenanceFoldRequest{SessionIDs: []string{labelID(httpLabelledSession)}},
			neverPath: "/api/v1/provenance/fold",
			neverBody: webapi.ProvenanceFoldRequest{SessionIDs: []string{labelID(never)}}},
	} {
		code, body := h.raw(outsider, probe.method, probe.path, probe.body)
		neverCode, neverBody := h.raw(outsider, probe.method, probe.neverPath, probe.neverBody)
		if code != neverCode || !bytes.Equal(body, neverBody) {
			t.Fatalf("%s: an outsider can tell the record exists: %d %s; "+
				"never written: %d %s", probe.name, code, body, neverCode, neverBody)
		}
		if code == http.StatusOK || code == http.StatusCreated {
			t.Fatalf("%s served an outsider: %s", probe.name, body)
		}
		holderSees(probe.name, h.do(holder, probe.method, probe.path, probe.body))
	}
}

func assertNoLeak(t *testing.T, name string, body []byte) {
	t.Helper()
	if leaked := authorizedtest.Leaks(body); len(leaked) != 0 {
		t.Fatalf("%s leaked %v to a reader without the labels: %s", name, leaked, body)
	}
}
