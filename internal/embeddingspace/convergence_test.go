// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.
package embeddingspace

import (
	"errors"
	"strings"
	"testing"
)

func TestParseTargetNormalizesAndValidates(t *testing.T) {
	t.Parallel()

	got, err := ParseTarget("  model-a  ")
	if err != nil {
		t.Fatalf("ParseTarget: %v", err)
	}
	if got != "model-a" {
		t.Fatalf("target = %q, want %q", got, "model-a")
	}

	got, err = ParseTarget("   ")
	if err != nil {
		t.Fatalf("blank target must not be an error: %v", err)
	}
	if got != "" {
		t.Fatalf("blank target = %q, want empty", got)
	}

	if _, err := ParseTarget(strings.Repeat("x", MaxIdentityBytes+1)); err == nil {
		t.Fatal("an over-long identity must be refused")
	}
}

func TestPlanConvergenceSkipsOnlyTheTargetSpace(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		target  string
		current FileState
		want    Decision
	}{
		{"no target is never convergence", "", Has("model-a"), DecisionNone},
		{"already in target", "model-a", Has("model-a"), DecisionSkip},
		{"another space is a re-embed", "model-a", Has("model-b"), DecisionRewrite},
		{"no embeddings is a backfill", "model-a", NoEmbeddings(), DecisionRewrite},
		{"unknown is never assumed usable", "model-a", Unknown(), DecisionRewrite},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PlanConvergence(tc.target, tc.current)
			if err != nil {
				t.Fatalf("PlanConvergence: %v", err)
			}
			if got != tc.want {
				t.Fatalf("decision = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPlanConvergenceRefusesInvalidInput(t *testing.T) {
	t.Parallel()

	if _, err := PlanConvergence("model-a", FileState{State: "bogus"}); err == nil {
		t.Fatal("an invalid current state must be refused")
	}
	if _, err := PlanConvergence(strings.Repeat("x", MaxIdentityBytes+1), NoEmbeddings()); err == nil {
		t.Fatal("an invalid target must be refused")
	}
}

func TestEnsureMonotonicAcceptsOnlyMovementTowardTheTarget(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		target  string
		before  FileState
		after   FileState
		wantErr bool
	}{
		{"unchanged is the provider-failure outcome", "model-a", Has("model-b"), Has("model-b"), false},
		{"reaching the target", "model-a", Has("model-b"), Has("model-a"), false},
		{"backfill reaching the target", "model-a", NoEmbeddings(), Has("model-a"), false},
		// no_embeddings is a positive assertion that the file holds no
		// vectors, not a weaker claim than has_embeddings. Accepting it as a
		// degradation would let a failed convergence mislabel a file that
		// still holds model-b vectors and hide them from planning forever.
		{"no_embeddings is not a legal degradation", "model-a", Has("model-b"), NoEmbeddings(), true},
		{"unknown to no_embeddings is not a legal degradation", "model-a", Unknown(), NoEmbeddings(), true},
		{"no_embeddings to unknown is legal", "model-a", NoEmbeddings(), Unknown(), false},
		{"degrading to unknown is fail-closed", "model-a", Has("model-b"), Unknown(), false},
		{"dropping out of the target", "model-a", Has("model-a"), NoEmbeddings(), true},
		{"target to a third space", "model-a", Has("model-a"), Has("model-c"), true},
		{"landing in a third space", "model-a", Has("model-b"), Has("model-c"), true},
		{"inventing an identity", "model-a", Unknown(), Has("model-c"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := EnsureMonotonic(tc.target, tc.before, tc.after)
			if tc.wantErr {
				if !errors.Is(err, ErrNotMonotonic) {
					t.Fatalf("err = %v, want ErrNotMonotonic", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("EnsureMonotonic: %v", err)
			}
		})
	}
}

func TestEnsureMonotonicWithoutATargetStillRefusesANewIdentity(t *testing.T) {
	t.Parallel()

	if err := EnsureMonotonic("", Has("model-b"), Has("model-c")); !errors.Is(err, ErrNotMonotonic) {
		t.Fatalf("err = %v, want ErrNotMonotonic", err)
	}
	if err := EnsureMonotonic("", Has("model-b"), Unknown()); err != nil {
		t.Fatalf("degrading to unknown without a target is legal: %v", err)
	}
	if err := EnsureMonotonic("", Has("model-b"), NoEmbeddings()); !errors.Is(err, ErrNotMonotonic) {
		t.Fatalf("err = %v, want ErrNotMonotonic: no_embeddings asserts the vectors are gone", err)
	}
}

func TestConvergedIsAPositiveClaim(t *testing.T) {
	t.Parallel()

	if !Converged("model-a", Has("model-a")) {
		t.Fatal("a file in the target space has converged")
	}
	if Converged("model-a", Has("model-b")) {
		t.Fatal("another space has not converged")
	}
	if Converged("model-a", Unknown()) {
		t.Fatal("unknown must never count as converged")
	}
	if Converged("model-a", NoEmbeddings()) {
		t.Fatal("no_embeddings must never count as converged")
	}
	if Converged("", Has("model-a")) {
		t.Fatal("without a target nothing has converged")
	}
}

func TestFileStatesRoundTripAndAreDeterministic(t *testing.T) {
	t.Parallel()

	states := map[string]FileState{
		"hdfs://a/t/f1.rf":             Has("model-a"),
		"hdfs://a/t/f2.rf":             NoEmbeddings(),
		"hdfs://a/t/f3.rf":             Unknown(),
		"hdfs://a/t/f4.rf R:(a,b,c,d)": Has("model-b"),
	}
	encoded, err := EncodeFileStates(states)
	if err != nil {
		t.Fatalf("EncodeFileStates: %v", err)
	}
	again, err := EncodeFileStates(states)
	if err != nil {
		t.Fatalf("EncodeFileStates (again): %v", err)
	}
	if encoded != again {
		t.Fatalf("encoding is not deterministic:\n%s\n%s", encoded, again)
	}

	decoded, err := DecodeFileStates(encoded)
	if err != nil {
		t.Fatalf("DecodeFileStates: %v", err)
	}
	if len(decoded) != len(states) {
		t.Fatalf("decoded %d entries, want %d", len(decoded), len(states))
	}
	for entry, want := range states {
		if decoded[entry] != want {
			t.Fatalf("entry %q = %+v, want %+v", entry, decoded[entry], want)
		}
	}
}

func TestFileStatesEmptyIsNotAnError(t *testing.T) {
	t.Parallel()

	encoded, err := EncodeFileStates(nil)
	if err != nil || encoded != "" {
		t.Fatalf("EncodeFileStates(nil) = %q, %v", encoded, err)
	}
	decoded, err := DecodeFileStates("   ")
	if err != nil {
		t.Fatalf("DecodeFileStates(blank): %v", err)
	}
	if len(decoded) != 0 {
		t.Fatalf("blank decoded to %d entries, want 0", len(decoded))
	}
}

func TestFileStatesRefuseMalformedInput(t *testing.T) {
	t.Parallel()

	if _, err := EncodeFileStates(map[string]FileState{"": Has("model-a")}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("empty entry: err = %v, want ErrInvalidState", err)
	}
	if _, err := EncodeFileStates(map[string]FileState{"f": {State: "bogus"}}); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("invalid state: err = %v, want ErrInvalidState", err)
	}
	if _, err := DecodeFileStates("not json"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("garbage: err = %v, want ErrInvalidState", err)
	}
	if _, err := DecodeFileStates("null"); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("null: err = %v, want ErrInvalidState", err)
	}
	if _, err := DecodeFileStates(`{"f":{"state":"has_embeddings"}}`); err == nil {
		t.Fatal("has_embeddings without an identity must be refused")
	}
	oversized := `{"f":"` + strings.Repeat("x", MaxJobFileStatesBytes) + `"}`
	if _, err := DecodeFileStates(oversized); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("oversized: err = %v, want ErrInvalidState", err)
	}
}
