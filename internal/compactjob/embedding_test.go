// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.
package compactjob

import (
	"context"
	"testing"

	"github.com/phrocker/shoal-oss/internal/compaction"
	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/iterrt"
)

func inputByEntry(t *testing.T, plan *Plan, entry string) InputFile {
	t.Helper()
	for _, in := range plan.Inputs {
		if in.Entry == entry {
			return in
		}
	}
	t.Fatalf("plan has no input for entry %q", entry)
	return InputFile{}
}

func TestTranslateCarriesThePerFileEmbeddingColumn(t *testing.T) {
	t.Parallel()

	job := validJob()
	first := job.Files[0].MetadataFileEntry
	second := job.Files[1].MetadataFileEntry
	states, err := embeddingspace.EncodeFileStates(map[string]embeddingspace.FileState{
		first:  embeddingspace.Has("model-b"),
		second: embeddingspace.NoEmbeddings(),
	})
	if err != nil {
		t.Fatalf("EncodeFileStates: %v", err)
	}
	job.Overrides[embeddingspace.JobFileStatesProperty] = states
	job.Overrides[embeddingspace.TableTargetProperty] = "model-a"

	plan := mustTranslate(t, job, Options{})
	if got := inputByEntry(t, plan, first).Embedding; got != embeddingspace.Has("model-b") {
		t.Fatalf("first input embedding = %+v, want has_embeddings model-b", got)
	}
	if got := inputByEntry(t, plan, second).Embedding; got != embeddingspace.NoEmbeddings() {
		t.Fatalf("second input embedding = %+v, want no_embeddings", got)
	}
	if plan.TargetEmbeddingSpace != "model-a" {
		t.Fatalf("TargetEmbeddingSpace = %q, want %q", plan.TargetEmbeddingSpace, "model-a")
	}
}

func TestTranslateLeavesUnmentionedInputsUnknown(t *testing.T) {
	t.Parallel()

	job := validJob()
	first := job.Files[0].MetadataFileEntry
	second := job.Files[1].MetadataFileEntry
	states, err := embeddingspace.EncodeFileStates(map[string]embeddingspace.FileState{
		first: embeddingspace.Has("model-a"),
	})
	if err != nil {
		t.Fatalf("EncodeFileStates: %v", err)
	}
	job.Overrides[embeddingspace.JobFileStatesProperty] = states

	plan := mustTranslate(t, job, Options{})
	// A file the job says nothing about is *absent* — the zero
	// FileState — not an explicit unknown. Absence is still fail-closed:
	// the composer reads the file footer and never assumes vectors are
	// usable. What it must not do is manufacture a claim, because a
	// claim is cross-checked against the footer and a synthesised
	// "unknown" contradicts every file written before this column
	// existed.
	if got := inputByEntry(t, plan, second).Embedding; got != (embeddingspace.FileState{}) {
		t.Fatalf("unmentioned input embedding = %+v, want the zero FileState", got)
	}
	if got := inputByEntry(t, plan, first).Embedding; got != embeddingspace.Has("model-a") {
		t.Fatalf("first input embedding = %+v, want has_embeddings model-a", got)
	}
}

func TestTranslateWithoutTheColumnLeavesEveryInputAbsent(t *testing.T) {
	t.Parallel()

	plan := mustTranslate(t, validJob(), Options{})
	for _, in := range plan.Inputs {
		if in.Embedding != (embeddingspace.FileState{}) {
			t.Fatalf("input %q embedding = %+v, want the zero FileState", in.Entry, in.Embedding)
		}
	}
	if plan.TargetEmbeddingSpace != "" {
		t.Fatalf("TargetEmbeddingSpace = %q, want empty", plan.TargetEmbeddingSpace)
	}
}

func TestTranslateRefusesAnEmbeddingColumnNamingAForeignFile(t *testing.T) {
	t.Parallel()

	job := validJob()
	states, err := embeddingspace.EncodeFileStates(map[string]embeddingspace.FileState{
		storedFile("hdfs://nn/accumulo/tables/2/t-0001/F9999.rf"): embeddingspace.Has("model-a"),
	})
	if err != nil {
		t.Fatalf("EncodeFileStates: %v", err)
	}
	job.Overrides[embeddingspace.JobFileStatesProperty] = states
	assertRefused(t, job, Options{}, ClassMalformedJob,
		"overrides["+embeddingspace.JobFileStatesProperty+"]")
}

func TestTranslateRefusesAMalformedEmbeddingColumn(t *testing.T) {
	t.Parallel()

	job := validJob()
	job.Overrides[embeddingspace.JobFileStatesProperty] = "{not json"
	assertRefused(t, job, Options{}, ClassMalformedJob,
		"overrides["+embeddingspace.JobFileStatesProperty+"]")

	job = validJob()
	job.Overrides[embeddingspace.JobFileStatesProperty] =
		`{"` + job.Files[0].MetadataFileEntry + `":{"state":"has_embeddings"}}`
	assertRefused(t, job, Options{}, ClassMalformedJob,
		"overrides["+embeddingspace.JobFileStatesProperty+"]")
}

func TestTranslateRefusesAnInvalidEmbeddingTarget(t *testing.T) {
	t.Parallel()

	job := validJob()
	job.Overrides[embeddingspace.TableTargetProperty] = string([]byte{0xff, 0xfe})
	assertRefused(t, job, Options{}, ClassUnsupportedProperty,
		"overrides["+embeddingspace.TableTargetProperty+"]")

	job = validJob()
	assertRefused(t, job, Options{TargetEmbeddingSpace: string([]byte{0xff, 0xfe})},
		ClassUnsupportedProperty, embeddingspace.TableTargetProperty)
}

func TestTranslateOverrideBeatsTheTableProperty(t *testing.T) {
	t.Parallel()

	job := validJob()
	job.Overrides[embeddingspace.TableTargetProperty] = " model-override "
	plan := mustTranslate(t, job, Options{TargetEmbeddingSpace: "model-table"})
	if plan.TargetEmbeddingSpace != "model-override" {
		t.Fatalf("TargetEmbeddingSpace = %q, want %q", plan.TargetEmbeddingSpace, "model-override")
	}
}

func TestTableOptionsReadTheEmbeddingTargetProperty(t *testing.T) {
	t.Parallel()

	opts, err := OptionsFromTableProperties(map[string]string{
		embeddingspace.TableTargetProperty: "  model-a  ",
	}, Limits{})
	if err != nil {
		t.Fatalf("TableOptions: %v", err)
	}
	if opts.TargetEmbeddingSpace != "model-a" {
		t.Fatalf("TargetEmbeddingSpace = %q, want %q", opts.TargetEmbeddingSpace, "model-a")
	}
}

func TestPlanSpecCarriesTheTargetAndConverger(t *testing.T) {
	t.Parallel()

	job := validJob()
	job.Overrides[embeddingspace.TableTargetProperty] = "model-a"
	plan := mustTranslate(t, job, Options{})

	spec := plan.Spec(nil)
	if spec.TargetEmbeddingSpace != "model-a" {
		t.Fatalf("spec target = %q, want %q", spec.TargetEmbeddingSpace, "model-a")
	}
	if spec.Converger != nil {
		t.Fatal("Spec must leave convergence disabled")
	}

	converger := planTestConverger{}
	spec = plan.SpecWithConverger(nil, converger)
	if spec.Converger == nil {
		t.Fatal("SpecWithConverger must attach the converger")
	}
	if _, ok := spec.Converger.(planTestConverger); !ok {
		t.Fatalf("spec.Converger = %T, want planTestConverger", spec.Converger)
	}
}

var _ compaction.Converger = planTestConverger{}

// planTestConverger is a stand-in used only to prove the plan carries a
// converger through to the spec.
type planTestConverger struct{}

func (planTestConverger) Begin(
	context.Context, compaction.ConvergeRequest,
) (compaction.ConvergeAttempt, error) {
	return planTestAttempt{}, nil
}

type planTestAttempt struct{}

func (planTestAttempt) Convert(_ context.Context, _ *iterrt.Key, value []byte) ([]byte, error) {
	return value, nil
}

func (planTestAttempt) End(context.Context, bool, int64, error) {}

// TestEmbeddingOverridesRoundTripsThroughTranslate is finding 1's
// answer. EncodeFileStates had no producer at all, so the encoder and
// the decoder were only ever tested against each other. This exercises
// the real path: build the overrides the way a coordinator must, put
// them on a job, and translate it.
//
// Note the honest caveat: no shoal binary constructs a
// TExternalCompactionJob today — Accumulo's Java manager does — so this
// is the seam a future coordinator must use, not a live production call
// site. See the doc comment on EmbeddingOverrides.
func TestEmbeddingOverridesRoundTripsThroughTranslate(t *testing.T) {
	t.Parallel()

	job := validJob()
	first := job.Files[0].MetadataFileEntry
	second := job.Files[1].MetadataFileEntry
	job.Overrides["table.file.compress.type"] = "gz"
	want := map[string]embeddingspace.FileState{
		first:  embeddingspace.Has("model-b"),
		second: embeddingspace.Unknown(),
	}
	if err := ApplyEmbeddingOverrides(job, "  model-a  ", " epoch-3 ", want); err != nil {
		t.Fatalf("ApplyEmbeddingOverrides: %v", err)
	}
	// Merging, not replacing: the codec and block size the job already
	// carried must survive, or the output file changes.
	if _, ok := job.Overrides["table.file.compress.type"]; !ok {
		t.Fatalf("overrides lost the pre-existing keys: %v", job.Overrides)
	}

	plan := mustTranslate(t, job, Options{})
	if plan.TargetEmbeddingSpace != "model-a" {
		t.Fatalf("target = %q, want model-a", plan.TargetEmbeddingSpace)
	}
	if plan.EmbeddingEpoch != "epoch-3" {
		t.Fatalf("epoch = %q, want epoch-3", plan.EmbeddingEpoch)
	}
	for entry, state := range want {
		if got := inputByEntry(t, plan, entry).Embedding; got != state {
			t.Fatalf("%s embedding = %+v, want %+v", entry, got, state)
		}
	}
	if spec := plan.Spec(nil); spec.EmbeddingEpoch != "epoch-3" {
		t.Fatalf("spec epoch = %q, want epoch-3", spec.EmbeddingEpoch)
	}
}

func TestEmbeddingOverridesOmitsWhatItWasNotGiven(t *testing.T) {
	t.Parallel()

	out, err := EmbeddingOverrides("", "", nil)
	if err != nil {
		t.Fatalf("EmbeddingOverrides: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("overrides = %v, want none", out)
	}

	if _, err := EmbeddingOverrides(string([]byte{0xff, 0xfe}), "", nil); err == nil {
		t.Fatal("an invalid target must be refused by the producer, not only by Translate")
	}
	longEpoch := string(make([]byte, embeddingspace.MaxJobEpochBytes+1))
	if _, err := EmbeddingOverrides("model-a", longEpoch, nil); err == nil {
		t.Fatal("an oversized epoch must be refused")
	}
	if err := ApplyEmbeddingOverrides(nil, "model-a", "", nil); err == nil {
		t.Fatal("a nil job must be refused")
	}
}

// TestApplyEmbeddingOverridesClearsStaleKeys proves a caller that stops
// converging actually stops: a stale target left behind would keep
// stamping outputs with a migration the table has abandoned.
func TestApplyEmbeddingOverridesClearsStaleKeys(t *testing.T) {
	t.Parallel()

	job := validJob()
	if err := ApplyEmbeddingOverrides(job, "model-a", "epoch-1", nil); err != nil {
		t.Fatalf("ApplyEmbeddingOverrides: %v", err)
	}
	if err := ApplyEmbeddingOverrides(job, "", "", nil); err != nil {
		t.Fatalf("ApplyEmbeddingOverrides: %v", err)
	}
	for _, key := range []string{
		embeddingspace.TableTargetProperty,
		embeddingspace.JobEpochProperty,
		embeddingspace.JobFileStatesProperty,
	} {
		if _, ok := job.Overrides[key]; ok {
			t.Fatalf("override %q survived a cleared assignment", key)
		}
	}
	plan := mustTranslate(t, job, Options{})
	if plan.TargetEmbeddingSpace != "" || plan.EmbeddingEpoch != "" {
		t.Fatalf("plan = (target=%q, epoch=%q), want both cleared",
			plan.TargetEmbeddingSpace, plan.EmbeddingEpoch)
	}
}

func TestTranslateEpochOverrideBeatsTheTableProperty(t *testing.T) {
	t.Parallel()

	job := validJob()
	job.Overrides[embeddingspace.TableTargetProperty] = "model-a"
	job.Overrides[embeddingspace.JobEpochProperty] = " epoch-override "
	plan := mustTranslate(t, job, Options{EmbeddingEpoch: "epoch-table"})
	if plan.EmbeddingEpoch != "epoch-override" {
		t.Fatalf("EmbeddingEpoch = %q, want %q", plan.EmbeddingEpoch, "epoch-override")
	}

	job = validJob()
	job.Overrides[embeddingspace.TableTargetProperty] = "model-a"
	plan = mustTranslate(t, job, Options{EmbeddingEpoch: "  epoch-table  "})
	if plan.EmbeddingEpoch != "epoch-table" {
		t.Fatalf("EmbeddingEpoch = %q, want %q", plan.EmbeddingEpoch, "epoch-table")
	}
}

func TestTranslateRefusesAnOversizedEpoch(t *testing.T) {
	t.Parallel()

	job := validJob()
	job.Overrides[embeddingspace.JobEpochProperty] = string(make([]byte, embeddingspace.MaxJobEpochBytes+1))
	assertRefused(t, job, Options{}, ClassUnsupportedProperty,
		"overrides["+embeddingspace.JobEpochProperty+"]")
}

func TestTableOptionsReadTheEmbeddingEpochProperty(t *testing.T) {
	t.Parallel()

	opts, err := OptionsFromTableProperties(map[string]string{
		embeddingspace.TableTargetProperty: "model-a",
		embeddingspace.JobEpochProperty:    "  epoch-5  ",
	}, Limits{})
	if err != nil {
		t.Fatalf("OptionsFromTableProperties: %v", err)
	}
	if opts.EmbeddingEpoch != "epoch-5" {
		t.Fatalf("EmbeddingEpoch = %q, want %q", opts.EmbeddingEpoch, "epoch-5")
	}
}
