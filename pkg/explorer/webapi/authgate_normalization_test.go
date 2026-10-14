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

package webapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestPubliclyReachableNormalizesTraversal pins the load-bearing path.Clean in
// publiclyReachable directly, at the level where it lives. net/http delivers the
// raw request path to ServeHTTP, so a traversal that escapes /assets and lands
// on an /api/v1/* route must be judged as that API route — protected — and not
// as a public asset. Deleting or neutralising the Clean (for example
// `_ = path.Clean(cleaned)`) makes the traversal rows below flip to true, which
// this test catches. The percent-encoded row additionally proves that URL.Path
// is already the decoded form, so %2e%2e collapses to .. before matching without
// any extra decoding here.
func TestPubliclyReachableNormalizesTraversal(t *testing.T) {
	handler := &Handler{}
	cases := []struct {
		name   string
		method string
		target string
		want   bool
	}{
		{"root shell", http.MethodGet, "/", true},
		{"asset", http.MethodGet, "/assets/app.js", true},
		{"asset root", http.MethodGet, "/assets/", true},
		{"auth config", http.MethodGet, "/api/v1/auth-config", true},
		{"api meta protected", http.MethodGet, "/api/v1/meta", false},
		{"api identity protected", http.MethodGet, "/api/v1/identity", false},

		// Traversals out of /assets resolve to the API route they name and must
		// stay protected. These are the rows a lost path.Clean flips to true.
		{"traversal to meta", http.MethodGet, "/assets/../api/v1/meta", false},
		{"traversal to identity", http.MethodGet, "/assets/../api/v1/identity", false},
		{"deep traversal to meta", http.MethodGet, "/assets/../../api/v1/meta", false},
		{"traversal to documents", http.MethodGet, "/assets/../api/v1/documents", false},
		{"percent-encoded traversal", http.MethodGet, "/assets/%2e%2e/api/v1/meta", false},

		// A POST can never be public regardless of path — the independent
		// GET-only axis of the same allowlist.
		{"post auth-config", http.MethodPost, "/api/v1/auth-config", false},
		{"post traversal to asset", http.MethodPost, "/assets/app.js", false},
	}
	for _, testCase := range cases {
		request := httptest.NewRequest(testCase.method, testCase.target, nil)
		if got := handler.publiclyReachable(request); got != testCase.want {
			t.Fatalf("%s: publiclyReachable(%s %s) = %v, want %v (URL.Path=%q)",
				testCase.name, testCase.method, testCase.target, got, testCase.want,
				request.URL.Path)
		}
	}
}
