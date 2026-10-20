// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"github.com/phrocker/shoal-oss/internal/adjudicatedfixture"
	"os"
	"path/filepath"
	"testing"
)

func TestInquiryDurableReplayAndStateBinding(t *testing.T) {
	for _, task := range []string{"source", "review"} {
		t.Run(task, func(t *testing.T) {
			def, e := adjudicatedfixture.Definitions(task)
			if e != nil {
				t.Fatal(e)
			}
			_, model, e := adjudicatedfixture.Baseline(def)
			if e != nil {
				t.Fatal(e)
			}
			dir := t.TempDir()
			path := filepath.Join(dir, "model.json")
			if e = os.WriteFile(path, model, 0600); e != nil {
				t.Fatal(e)
			}
			state := filepath.Join(dir, "state")
			first, e := inquireCandidate(task, path, adjudicatedfixture.Hash(model), state)
			if e != nil {
				t.Fatal(e)
			}
			second, e := inquireCandidate(task, path, adjudicatedfixture.Hash(model), state)
			if e != nil {
				t.Fatal(e)
			}
			if first.ProviderCalls != 1 || second.ProviderCalls != 0 || first.ReceiptID != second.ReceiptID || first.PredictionID != second.PredictionID || !second.ReplayMatched {
				t.Fatalf("replay mismatch: %+v %+v", first, second)
			}
			other := "review"
			if task == other {
				other = "source"
			}
			if _, e = inquireCandidate(other, path, adjudicatedfixture.Hash(model), state); e == nil {
				t.Fatal("cross-task state accepted")
			}
			if e = os.Remove(filepath.Join(state, "inquiry-state.json")); e != nil {
				t.Fatal(e)
			}
			if _, e = inquireCandidate(task, path, adjudicatedfixture.Hash(model), state); e == nil {
				t.Fatal("minted identity over old engine")
			}
		})
	}
}
func TestInquiryRejectsModelSubstitutionBeforeState(t *testing.T) {
	def, e := adjudicatedfixture.Definitions("source")
	if e != nil {
		t.Fatal(e)
	}
	_, model, e := adjudicatedfixture.Baseline(def)
	if e != nil {
		t.Fatal(e)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "model.json")
	if e = os.WriteFile(path, append(model, ' '), 0600); e != nil {
		t.Fatal(e)
	}
	state := filepath.Join(dir, "state")
	if _, e = inquireCandidate("source", path, adjudicatedfixture.Hash(model), state); e == nil {
		t.Fatal("substituted bytes accepted")
	}
	if _, e = os.Stat(state); !os.IsNotExist(e) {
		t.Fatal("model rejection mutated state")
	}
}
