// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.
package embeddingspace

import (
	"errors"
	"testing"
)

// TestCompatibleReportsMismatchRegardlessOfInputOrder pins the property that
// two genuinely incompatible embedding spaces are always reported, even when
// an unknown or embedding-free input is merged between them. Unknown is an
// absorbing state, so a single-pass merge would quietly return unknown here
// and a real misconfiguration would surface or hide purely on input order.
func TestCompatibleReportsMismatchRegardlessOfInputOrder(t *testing.T) {
	cases := [][]FileState{
		{Has("X"), Has("Y")},
		{Has("X"), Unknown(), Has("Y")},
		{Unknown(), Has("X"), Has("Y")},
		{Has("X"), NoEmbeddings(), Has("Y")},
		{Has("X"), Unknown(), NoEmbeddings(), Has("Y")},
		{Has("Y"), Unknown(), Has("X")},
	}
	for _, states := range cases {
		got, err := Compatible("merge", states...)
		if err == nil {
			t.Errorf("Compatible(%v) = %s, want mismatch error", states, got)
			continue
		}
		if !errors.Is(err, ErrMismatch) {
			t.Errorf("Compatible(%v) error = %v, want ErrMismatch", states, err)
		}
	}
}

func TestCompatibleMergesCompatibleStates(t *testing.T) {
	cases := []struct {
		name   string
		states []FileState
		want   FileState
	}{
		{"no inputs", nil, Unknown()},
		{"all embedding free", []FileState{NoEmbeddings(), NoEmbeddings()}, NoEmbeddings()},
		{"single unknown", []FileState{Unknown()}, Unknown()},
		{"unknown poisons embedding free", []FileState{NoEmbeddings(), Unknown()}, Unknown()},
		{"identical identities", []FileState{Has("X"), Has("X")}, Has("X")},
		{"identity with embedding free", []FileState{Has("X"), NoEmbeddings()}, Unknown()},
		{"identity with unknown", []FileState{Has("X"), Unknown()}, Unknown()},
		{"embedding free before identity", []FileState{NoEmbeddings(), Has("X")}, Unknown()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Compatible("merge", tc.states...)
			if err != nil {
				t.Fatalf("Compatible(%v) error = %v", tc.states, err)
			}
			if got != tc.want {
				t.Fatalf("Compatible(%v) = %s, want %s", tc.states, got, tc.want)
			}
		})
	}
}

// TestCompatibleIsOrderIndependent checks that merging is commutative for the
// compatible cases too, so callers cannot get a different answer by presenting
// the same set of files in a different order.
func TestCompatibleIsOrderIndependent(t *testing.T) {
	states := []FileState{Has("X"), NoEmbeddings(), Unknown(), Has("X")}
	forward, err := Compatible("merge", states...)
	if err != nil {
		t.Fatalf("forward error = %v", err)
	}
	reversed := make([]FileState, 0, len(states))
	for i := len(states) - 1; i >= 0; i-- {
		reversed = append(reversed, states[i])
	}
	backward, err := Compatible("merge", reversed...)
	if err != nil {
		t.Fatalf("backward error = %v", err)
	}
	if forward != backward {
		t.Fatalf("forward = %s, backward = %s", forward, backward)
	}
}

func TestCompatibleRejectsInvalidState(t *testing.T) {
	if _, err := Compatible("merge", FileState{State: "bogus"}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("error = %v, want ErrInvalidState", err)
	}
	if _, err := Compatible("merge", FileState{State: StateNoEmbeddings, Identity: "x"}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("identity on embedding-free state must be rejected, got %v", err)
	}
}

func TestValidateQueryStatesFailsClosed(t *testing.T) {
	const query = "provider:model-a:2:l2"
	if err := ValidateQueryStates("exact query", query,
		Has(query), Has(query)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		identity string
		err      error
		in       []FileState
	}{
		{"missing identity", "", ErrQueryIdentityRequired, []FileState{Has(query)}},
		{"non-canonical query identity", " " + query, ErrInvalidState, []FileState{Has(query)}},
		{"unknown", query, ErrQuerySpaceUnknown, []FileState{Unknown()}},
		{"zero is unknown", query, ErrQuerySpaceUnknown, []FileState{{}}},
		{"partial state", query, ErrInvalidState, []FileState{{Identity: query}}},
		{"no embeddings", query, ErrQueryNoEmbeddings, []FileState{NoEmbeddings()}},
		{"same dimensions different model", query, ErrMismatch, []FileState{
			Has(query), Has("provider:model-b:2:l2"),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateQueryStates(
				"exact query", tc.identity, tc.in...); !errors.Is(err, tc.err) {
				t.Fatalf("error = %v, want %v", err, tc.err)
			}
		})
	}
}
