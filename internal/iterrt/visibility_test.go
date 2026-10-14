// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package iterrt

import "testing"

// visCells: a mix of public, single-label, and AND-expression cells.
func visCells() []kv {
	return []kv{
		{mk("r1", "cf", "a", "", 10), []byte("public")},
		{mk("r2", "cf", "a", "alpha", 10), []byte("alpha-only")},
		{mk("r3", "cf", "a", "beta", 10), []byte("beta-only")},
		{mk("r4", "cf", "a", "alpha&beta", 10), []byte("both")},
	}
}

func initVis(t *testing.T, src SortedKeyValueIterator, env IteratorEnvironment) *VisibilityFilter {
	t.Helper()
	vf := NewVisibilityFilter()
	if err := vf.Init(src, nil, env); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if err := vf.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	return vf
}

// TestVisibility_FiltersByAuths: at ScopeScan, only cells the auth set
// satisfies survive.
func TestVisibility_FiltersByAuths(t *testing.T) {
	env := IteratorEnvironment{
		Scope:          ScopeScan,
		Authorizations: [][]byte{[]byte("alpha")},
	}
	vf := initVis(t, newSliceSource(visCells()...), env)
	got, err := drain(vf)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	// public (empty CV) + alpha-only. beta-only and alpha&beta fail.
	if gs := valStrings(got); !eqStr(gs, []string{"public", "alpha-only"}) {
		t.Fatalf("got %v", gs)
	}
}

// TestVisibility_AndExpressionNeedsBoth: an AND expression passes only
// when every label is held.
func TestVisibility_AndExpressionNeedsBoth(t *testing.T) {
	env := IteratorEnvironment{
		Scope:          ScopeScan,
		Authorizations: [][]byte{[]byte("alpha"), []byte("beta")},
	}
	vf := initVis(t, newSliceSource(visCells()...), env)
	got, err := drain(vf)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if gs := valStrings(got); !eqStr(gs, []string{"public", "alpha-only", "beta-only", "both"}) {
		t.Fatalf("got %v, want all four", gs)
	}
}

// TestVisibility_NilAuthsIsSystemContext: nil Authorizations means
// system context — every cell visible, no filtering.
func TestVisibility_NilAuthsIsSystemContext(t *testing.T) {
	env := IteratorEnvironment{Scope: ScopeScan, Authorizations: nil}
	vf := initVis(t, newSliceSource(visCells()...), env)
	got, err := drain(vf)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("nil auths should pass all 4 cells, got %d", len(got))
	}
}

// TestVisibility_InactiveOnCompactionScopes: at minc/majc the filter is a
// transparent passthrough even with a restrictive auth set — a compaction
// must never drop cells by visibility.
func TestVisibility_InactiveOnCompactionScopes(t *testing.T) {
	for _, sc := range []IteratorScope{ScopeMinc, ScopeMajc} {
		env := IteratorEnvironment{Scope: sc, Authorizations: [][]byte{[]byte("nothing")}}
		vf := initVis(t, newSliceSource(visCells()...), env)
		got, err := drain(vf)
		if err != nil {
			t.Fatalf("scope %v drain: %v", sc, err)
		}
		if len(got) != 4 {
			t.Fatalf("scope %v: filter must be passthrough, got %d cells want 4", sc, len(got))
		}
	}
}

// TestVisibility_EmptyAuthsFiltersAll: a non-nil but empty auth set at
// scan scope is active and drops every labeled cell (only public survives).
func TestVisibility_EmptyAuthsFiltersAll(t *testing.T) {
	env := IteratorEnvironment{Scope: ScopeScan, Authorizations: [][]byte{}}
	vf := initVis(t, newSliceSource(visCells()...), env)
	got, err := drain(vf)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if gs := valStrings(got); !eqStr(gs, []string{"public"}) {
		t.Fatalf("got %v, want only public", gs)
	}
}

// TestVisibility_StackedOverVersioning: visibility over versioning over a
// source — the scan-time stack shape. Filtering happens after version
// capping, matching the Java scan stack order.
func TestVisibility_StackedOverVersioning(t *testing.T) {
	cells := []kv{
		{mk("r1", "cf", "a", "alpha", 30), []byte("v30")},
		{mk("r1", "cf", "a", "alpha", 20), []byte("v20")},
		{mk("r2", "cf", "a", "secret", 10), []byte("secret")},
	}
	v := NewVersioningIterator()
	if err := v.Init(newSliceSource(cells...), map[string]string{VersioningOption: "1"}, IteratorEnvironment{Scope: ScopeScan}); err != nil {
		t.Fatalf("vers Init: %v", err)
	}
	vf := NewVisibilityFilter()
	env := IteratorEnvironment{Scope: ScopeScan, Authorizations: [][]byte{[]byte("alpha")}}
	if err := vf.Init(v, nil, env); err != nil {
		t.Fatalf("vis Init: %v", err)
	}
	if err := vf.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	got, err := drain(vf)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if gs := valStrings(got); !eqStr(gs, []string{"v30"}) {
		t.Fatalf("got %v, want [v30] (newest alpha cell, secret filtered)", gs)
	}
}

// TestVisibility_DeepCopyRederivesEnv: DeepCopy's env replaces the
// original — a copy made with a permissive env sees more cells.
func TestVisibility_DeepCopyRederivesEnv(t *testing.T) {
	restrictive := IteratorEnvironment{Scope: ScopeScan, Authorizations: [][]byte{[]byte("alpha")}}
	vf := initVis(t, newSliceSource(visCells()...), restrictive)

	permissive := IteratorEnvironment{Scope: ScopeScan, Authorizations: [][]byte{[]byte("alpha"), []byte("beta")}}
	cp := vf.DeepCopy(permissive)
	if err := cp.Seek(InfiniteRange(), nil, false); err != nil {
		t.Fatalf("copy Seek: %v", err)
	}
	got, err := drain(cp)
	if err != nil {
		t.Fatalf("copy drain: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("DeepCopy with permissive env should see all 4, got %d", len(got))
	}
}
