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

package main

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/offlinecompact"
)

func TestValidateReleaseCommitOptions(t *testing.T) {
	tests := []struct {
		name    string
		dryRun  bool
		mode    offlinecompact.CommitMode
		wantErr bool
	}{
		{name: "plan dry run", dryRun: true, mode: offlinecompact.ModePlan},
		{name: "plan commit disabled", dryRun: false, mode: offlinecompact.ModePlan, wantErr: true},
		{name: "direct disabled", dryRun: true, mode: offlinecompact.ModeDirect, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateReleaseCommitOptions(tt.dryRun, tt.mode)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateReleaseCommitOptions() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestParseRange(t *testing.T) {
	tests := []struct {
		spec             string
		wantStart, wantE []byte
		wantErr          bool
	}{
		{"", nil, nil, false},
		{"a:m", []byte("a"), []byte("m"), false},
		{":m", nil, []byte("m"), false},
		{"a:", []byte("a"), nil, false},
		{":", nil, nil, false},
		{"nocolon", nil, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			s, e, err := parseRange(tt.spec)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error for %q", tt.spec)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !bytes.Equal(s, tt.wantStart) || !bytes.Equal(e, tt.wantE) {
				t.Fatalf("parseRange(%q) = %q,%q want %q,%q", tt.spec, s, e, tt.wantStart, tt.wantE)
			}
		})
	}
}

type stubEnum struct {
	tablets []metadata.TabletInfo
	err     error
}

func (s stubEnum) LocateTable(context.Context, string) ([]metadata.TabletInfo, error) {
	return s.tablets, s.err
}

func TestRangeEnumeratorPassThrough(t *testing.T) {
	tablets := []metadata.TabletInfo{
		{TableID: "2", EndRow: []byte("m")},
		{TableID: "2", EndRow: nil},
	}
	e := rangeEnumerator{inner: stubEnum{tablets: tablets}, apply: false}
	got, err := e.LocateTable(context.Background(), "2")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("pass-through should return all tablets, got %d", len(got))
	}
}

func TestRangeEnumeratorFilters(t *testing.T) {
	tablets := []metadata.TabletInfo{
		{TableID: "2", EndRow: []byte("d")},                       // rows (-inf, d]
		{TableID: "2", EndRow: []byte("m"), PrevRow: []byte("d")}, // (d, m]
		{TableID: "2", EndRow: nil, PrevRow: []byte("m")},         // (m, +inf)
	}
	// Restrict to [e, m]: drops the first tablet (all rows <= d < e) and
	// the last (all rows > m).
	e := rangeEnumerator{inner: stubEnum{tablets: tablets}, start: []byte("e"), end: []byte("m"), apply: true}
	got, err := e.LocateTable(context.Background(), "2")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || string(got[0].EndRow) != "m" {
		t.Fatalf("expected only the (d,m] tablet, got %+v", got)
	}
}

func TestRangeEnumeratorPropagatesError(t *testing.T) {
	want := errors.New("boom")
	e := rangeEnumerator{inner: stubEnum{err: want}, apply: true}
	if _, err := e.LocateTable(context.Background(), "2"); !errors.Is(err, want) {
		t.Fatalf("want propagated error, got %v", err)
	}
}
