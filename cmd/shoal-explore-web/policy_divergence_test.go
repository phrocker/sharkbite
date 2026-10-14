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

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/healthsurface"
)

type fakeDivergence struct{ hook func() }

func (f *fakeDivergence) OnDiverged(hook func()) { f.hook = hook }

// TestPolicyCatalogDivergenceDropsReadinessAndLogs pins the operator signal
// for a diverged policy catalog (#665): one stderr line naming no corpus or
// policy state, and /readyz turning not-ready while /healthz stays ok.
func TestPolicyCatalogDivergenceDropsReadinessAndLogs(t *testing.T) {
	state := &healthsurface.State{}
	state.MarkReady()
	handler := healthsurface.NewHandler(state)
	probe := func(path string) int {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		return recorder.Code
	}
	store := &fakeDivergence{}
	var log strings.Builder
	watchPolicyCatalogDivergence(store, state, &log)
	if store.hook == nil {
		t.Fatal("no divergence hook registered")
	}
	if probe("/readyz") != http.StatusOK || log.Len() != 0 {
		t.Fatal("ready before divergence")
	}
	store.hook()
	if code := probe("/readyz"); code == http.StatusOK {
		t.Fatalf("/readyz after divergence = %d, want not ready", code)
	}
	if code := probe("/healthz"); code != http.StatusOK {
		t.Fatalf("/healthz after divergence = %d, want ok", code)
	}
	if !strings.Contains(log.String(), "policy catalog") ||
		strings.Count(log.String(), "\n") != 1 {
		t.Fatalf("log = %q, want one line", log.String())
	}
	// A nil store is not watched and does not panic.
	watchPolicyCatalogDivergence(nil, state, &log)
	var typedNil *fakeDivergence
	watchPolicyCatalogDivergence(typedNil, state, &log)
}
