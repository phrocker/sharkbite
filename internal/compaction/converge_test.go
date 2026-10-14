// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.
package compaction

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/rfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile"
	"github.com/phrocker/shoal-oss/internal/rfile/bcfile/block"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// fakeConverger records what the composer asked it to do so the tests
// can assert on the calls, not only on the output.
type fakeConverger struct {
	target string
	// beginErr is returned by Begin; nil admits the attempt.
	beginErr error
	// failAfter makes Convert fail once it has converted that many
	// cells. Zero never fails.
	failAfter  int
	convertErr error

	// epoch, when set, is the only epoch Begin admits.
	epoch string

	begins     int
	admitted   int
	epochSeen  string
	inputsSeen int
	converted  int
	ends       int
	endCells   int64
	endOK      bool
}

func (f *fakeConverger) Begin(
	_ context.Context, req ConvergeRequest,
) (ConvergeAttempt, error) {
	f.begins++
	if f.beginErr != nil {
		return nil, f.beginErr
	}
	if req.Target != f.target {
		return nil, fmt.Errorf("%w: wrong target", embeddingspace.ErrConvergenceUnavailable)
	}
	if f.epoch != "" && req.Epoch != f.epoch {
		return nil, fmt.Errorf("%w: wrong epoch", embeddingspace.ErrConvergenceUnavailable)
	}
	if len(req.Inputs) == 0 {
		return nil, fmt.Errorf("%w: no inputs", embeddingspace.ErrConvergenceUnavailable)
	}
	f.admitted++
	f.epochSeen = req.Epoch
	f.inputsSeen = len(req.Inputs)
	return f, nil
}

func (f *fakeConverger) Convert(_ context.Context, _ *iterrt.Key, value []byte) ([]byte, error) {
	if f.failAfter > 0 && f.converted >= f.failAfter {
		return nil, f.convertErr
	}
	f.converted++
	return append([]byte("converted:"), value...), nil
}

func (f *fakeConverger) End(_ context.Context, converted bool, cells int64, _ error) {
	f.ends++
	f.endCells = cells
	f.endOK = converted
}

func TestCompactSkipsConvergenceWhenInputIsAlreadyInTarget(t *testing.T) {
	input := buildRFileInSpace(t, "a", embeddingspace.Has("space-a"))
	converger := &fakeConverger{target: "space-a"}
	result, err := Compact(Spec{
		Inputs:               []Input{{Name: "a.rf", Bytes: input}},
		Scope:                iterrt.ScopeMajc,
		TargetEmbeddingSpace: "space-a",
		Converger:            converger,
	})
	if err != nil {
		t.Fatal(err)
	}
	if converger.begins != 0 || converger.converted != 0 {
		t.Fatalf("converger consulted for an already-converged input: %+v", converger)
	}
	if got := outputSpace(t, result.Output); got != embeddingspace.Has("space-a") {
		t.Fatalf("output embedding = %+v", got)
	}
	if got := outputValues(t, result.Output); len(got) != 1 || got[0] != "v" {
		t.Fatalf("values = %q, want the untouched input value", got)
	}
}

func TestCompactConvergesInputIntoTargetSpace(t *testing.T) {
	input := buildRFileInSpace(t, "a", embeddingspace.Has("space-a"))
	converger := &fakeConverger{target: "space-b"}
	result, err := Compact(Spec{
		Inputs:               []Input{{Name: "a.rf", Bytes: input}},
		Scope:                iterrt.ScopeMajc,
		TargetEmbeddingSpace: "space-b",
		Converger:            converger,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := outputSpace(t, result.Output); got != embeddingspace.Has("space-b") {
		t.Fatalf("output embedding = %+v, want space-b", got)
	}
	if got := outputValues(t, result.Output); len(got) != 1 || got[0] != "converted:v" {
		t.Fatalf("values = %q, want the rewritten value", got)
	}
	if converger.ends != 1 || !converger.endOK || converger.endCells != 1 {
		t.Fatalf("End = (%d calls, ok=%v, cells=%d)", converger.ends, converger.endOK, converger.endCells)
	}
}

// A migration is the only reason a compaction ever sees two different
// embedding spaces at once. Without a converger this must stay refused;
// with one, every cell is rewritten, so the single label on the output
// is true.
func TestCompactConvergesMixedSpacesOnlyWithAConverger(t *testing.T) {
	a := buildRFileInSpace(t, "a", embeddingspace.Has("space-a"))
	b := buildRFileInSpace(t, "b", embeddingspace.Has("space-b"))
	inputs := []Input{{Name: "a.rf", Bytes: a}, {Name: "b.rf", Bytes: b}}

	if _, err := Compact(Spec{
		Inputs:               inputs,
		Scope:                iterrt.ScopeMajc,
		TargetEmbeddingSpace: "space-c",
	}); !errors.Is(err, embeddingspace.ErrMismatch) {
		t.Fatalf("Compact without converger error = %v, want ErrMismatch", err)
	}

	result, err := Compact(Spec{
		Inputs:               inputs,
		Scope:                iterrt.ScopeMajc,
		TargetEmbeddingSpace: "space-c",
		Converger:            &fakeConverger{target: "space-c"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := outputSpace(t, result.Output); got != embeddingspace.Has("space-c") {
		t.Fatalf("output embedding = %+v, want space-c", got)
	}
	if got := outputValues(t, result.Output); len(got) != 2 {
		t.Fatalf("values = %q, want both inputs", got)
	}
}

func TestCompactPreservesInputSpaceWhenConvergenceIsRefused(t *testing.T) {
	input := buildRFileInSpace(t, "a", embeddingspace.Has("space-a"))
	converger := &fakeConverger{
		target:   "space-b",
		beginErr: fmt.Errorf("%w: provider down", embeddingspace.ErrConvergenceUnavailable),
	}
	result, err := Compact(Spec{
		Inputs:               []Input{{Name: "a.rf", Bytes: input}},
		Scope:                iterrt.ScopeMajc,
		TargetEmbeddingSpace: "space-b",
		Converger:            converger,
	})
	if err != nil {
		t.Fatalf("a refused convergence must not fail the compaction: %v", err)
	}
	if got := outputSpace(t, result.Output); got != embeddingspace.Has("space-a") {
		t.Fatalf("output embedding = %+v, want the preserved input space", got)
	}
	if got := outputValues(t, result.Output); len(got) != 1 || got[0] != "v" {
		t.Fatalf("values = %q, want the untouched input value", got)
	}
	// Finding 2's contract: a refused Begin creates no obligation. If the
	// composer called End here it would settle a permit the converger
	// never issued, and with a shared governor that refund would come out
	// of some other compaction's reservation.
	if converger.ends != 0 {
		t.Fatalf("End = %d calls, want none after a refused Begin", converger.ends)
	}
	if converger.admitted != 0 {
		t.Fatalf("admitted = %d, want none", converger.admitted)
	}
}

func TestCompactPreservesInputSpaceWhenTheProviderFailsMidStream(t *testing.T) {
	input := buildMultiCellRFile(t, embeddingspace.Has("space-a"), "a", "b", "c")
	converger := &fakeConverger{
		target:     "space-b",
		failAfter:  2,
		convertErr: fmt.Errorf("%w: rate limited", embeddingspace.ErrConvergenceUnavailable),
	}
	result, err := Compact(Spec{
		Inputs:               []Input{{Name: "a.rf", Bytes: input}},
		Scope:                iterrt.ScopeMajc,
		TargetEmbeddingSpace: "space-b",
		Converger:            converger,
	})
	if err != nil {
		t.Fatalf("a mid-stream provider failure must not fail the compaction: %v", err)
	}
	if got := outputSpace(t, result.Output); got != embeddingspace.Has("space-a") {
		t.Fatalf("output embedding = %+v, want the preserved input space", got)
	}
	values := outputValues(t, result.Output)
	if len(values) != 3 {
		t.Fatalf("values = %q, want all three cells recomposed unconverted", values)
	}
	for _, value := range values {
		if value != "v" {
			t.Fatalf("values = %q, want no partially converted cell", values)
		}
	}
	if converger.endCells != 2 || converger.endOK {
		t.Fatalf("End = (cells=%d, ok=%v), want the two converted cells charged",
			converger.endCells, converger.endOK)
	}
}

func TestCompactFailsWhenConvergenceIsAborted(t *testing.T) {
	input := buildMultiCellRFile(t, embeddingspace.Has("space-a"), "a", "b")
	converger := &fakeConverger{
		target:     "space-b",
		failAfter:  1,
		convertErr: fmt.Errorf("%w: corrupt cell", ErrConvergenceAborted),
	}
	_, err := Compact(Spec{
		Inputs:               []Input{{Name: "a.rf", Bytes: input}},
		Scope:                iterrt.ScopeMajc,
		TargetEmbeddingSpace: "space-b",
		Converger:            converger,
	})
	if !errors.Is(err, ErrConvergenceAborted) {
		t.Fatalf("Compact error = %v, want ErrConvergenceAborted", err)
	}
}

func TestCompactConvergesUnknownAndUnembeddedInput(t *testing.T) {
	for _, state := range []embeddingspace.FileState{
		embeddingspace.Unknown(),
		embeddingspace.NoEmbeddings(),
	} {
		input := buildRFileInSpace(t, "a", state)
		result, err := Compact(Spec{
			Inputs:               []Input{{Name: "a.rf", Bytes: input}},
			Scope:                iterrt.ScopeMajc,
			TargetEmbeddingSpace: "space-b",
			Converger:            &fakeConverger{target: "space-b"},
		})
		if err != nil {
			t.Fatalf("%s: %v", state, err)
		}
		if got := outputSpace(t, result.Output); got != embeddingspace.Has("space-b") {
			t.Fatalf("%s: output embedding = %+v", state, got)
		}
	}
}

func TestCompactWithoutTargetIgnoresTheConverger(t *testing.T) {
	input := buildRFileInSpace(t, "a", embeddingspace.Has("space-a"))
	converger := &fakeConverger{target: "space-b"}
	result, err := Compact(Spec{
		Inputs:    []Input{{Name: "a.rf", Bytes: input}},
		Scope:     iterrt.ScopeMajc,
		Converger: converger,
	})
	if err != nil {
		t.Fatal(err)
	}
	if converger.begins != 0 {
		t.Fatalf("converger consulted without a target: %+v", converger)
	}
	if got := outputSpace(t, result.Output); got != embeddingspace.Has("space-a") {
		t.Fatalf("output embedding = %+v", got)
	}
}

func TestCompactRejectsInvalidConvergenceTarget(t *testing.T) {
	input := buildRFileInSpace(t, "a", embeddingspace.Has("space-a"))
	_, err := Compact(Spec{
		Inputs:               []Input{{Name: "a.rf", Bytes: input}},
		Scope:                iterrt.ScopeMajc,
		TargetEmbeddingSpace: strings8k(),
		Converger:            &fakeConverger{},
	})
	if err == nil {
		t.Fatal("an oversized target identity must be refused")
	}
}

func TestCompactConvergesParquetOutput(t *testing.T) {
	input := buildRFileInSpace(t, "a", embeddingspace.Has("space-a"))
	result, err := Compact(Spec{
		Inputs:               []Input{{Name: "a.rf", Bytes: input}},
		Scope:                iterrt.ScopeMajc,
		OutputFormat:         "parquet",
		TargetEmbeddingSpace: "space-b",
		Converger:            &fakeConverger{target: "space-b"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.EntriesWritten != 1 {
		t.Fatalf("entries = %d", result.EntriesWritten)
	}
}

// TestCompactStampsTheEpochOnlyOnConvergedOutput proves the epoch is a
// claim about the migration that produced a file, not a label carried by
// any compaction that happened to run while a migration was configured.
func TestCompactStampsTheEpochOnlyOnConvergedOutput(t *testing.T) {
	input := buildRFileInSpace(t, "a", embeddingspace.Has("space-a"))
	converged, err := Compact(Spec{
		Inputs:               []Input{{Name: "a.rf", Bytes: input}},
		Scope:                iterrt.ScopeMajc,
		TargetEmbeddingSpace: "space-b",
		EmbeddingEpoch:       "epoch-7",
		Converger:            &fakeConverger{target: "space-b", epoch: "epoch-7"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !converged.Converged || converged.EmbeddingEpoch != "epoch-7" {
		t.Fatalf("converged result = (converged=%v, epoch=%q), want epoch-7",
			converged.Converged, converged.EmbeddingEpoch)
	}
	if converged.EmbeddingSpace != embeddingspace.Has("space-b") {
		t.Fatalf("converged space = %+v", converged.EmbeddingSpace)
	}

	// Same epoch, but the converger refuses: the output keeps space-a and
	// must not claim to belong to the migration.
	refused, err := Compact(Spec{
		Inputs:               []Input{{Name: "a.rf", Bytes: input}},
		Scope:                iterrt.ScopeMajc,
		TargetEmbeddingSpace: "space-b",
		EmbeddingEpoch:       "epoch-7",
		Converger: &fakeConverger{
			target:   "space-b",
			beginErr: fmt.Errorf("%w: provider down", embeddingspace.ErrConvergenceUnavailable),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if refused.Converged || refused.EmbeddingEpoch != "" {
		t.Fatalf("unconverged result = (converged=%v, epoch=%q), want no epoch",
			refused.Converged, refused.EmbeddingEpoch)
	}
}

// TestCompactPassesTheEpochToTheConverger is the anti-oscillation
// property: a converger bound to one epoch sees the compaction's epoch
// and can refuse a stale one, in which case the compaction still runs
// unconverged rather than failing.
func TestCompactPassesTheEpochToTheConverger(t *testing.T) {
	input := buildRFileInSpace(t, "a", embeddingspace.Has("space-a"))
	converger := &fakeConverger{target: "space-b", epoch: "epoch-2"}
	result, err := Compact(Spec{
		Inputs:               []Input{{Name: "a.rf", Bytes: input}},
		Scope:                iterrt.ScopeMajc,
		TargetEmbeddingSpace: "space-b",
		EmbeddingEpoch:       "epoch-1",
		Converger:            converger,
	})
	if err != nil {
		t.Fatalf("a stale epoch must not fail the compaction: %v", err)
	}
	if converger.admitted != 0 {
		t.Fatalf("admitted = %d, want the stale epoch refused", converger.admitted)
	}
	if got := outputSpace(t, result.Output); got != embeddingspace.Has("space-a") {
		t.Fatalf("output embedding = %+v, want the input preserved", got)
	}
	if result.Converged || result.EmbeddingEpoch != "" {
		t.Fatalf("result = (converged=%v, epoch=%q)", result.Converged, result.EmbeddingEpoch)
	}
}

func TestCompactRejectsAnEpochWithoutATarget(t *testing.T) {
	input := buildRFileInSpace(t, "a", embeddingspace.Has("space-a"))
	_, err := Compact(Spec{
		Inputs:         []Input{{Name: "a.rf", Bytes: input}},
		Scope:          iterrt.ScopeMajc,
		EmbeddingEpoch: "epoch-1",
	})
	if err == nil {
		t.Fatal("an epoch without a target names a migration that cannot exist")
	}
}

// TestCompactReportsConvergenceRequiredForRefusedMixedInput covers
// finding 4. Two distinct identities cannot be merged under one label,
// so a refused provider leaves the compaction with nothing publishable.
// The requirement is that this is (a) distinguishable as retryable, (b)
// still an identity mismatch for existing handlers, and (c) reported
// before any convergence work is charged.
func TestCompactReportsConvergenceRequiredForRefusedMixedInput(t *testing.T) {
	a := buildRFileInSpace(t, "a", embeddingspace.Has("space-a"))
	b := buildRFileInSpace(t, "b", embeddingspace.Has("space-x"))
	converger := &fakeConverger{
		target:   "space-b",
		beginErr: fmt.Errorf("%w: provider down", embeddingspace.ErrConvergenceUnavailable),
	}
	_, err := Compact(Spec{
		Inputs: []Input{
			{Name: "a.rf", Bytes: a},
			{Name: "b.rf", Bytes: b},
		},
		Scope:                iterrt.ScopeMajc,
		TargetEmbeddingSpace: "space-b",
		Converger:            converger,
	})
	if !errors.Is(err, ErrConvergenceRequired) {
		t.Fatalf("Compact error = %v, want ErrConvergenceRequired", err)
	}
	if !errors.Is(err, embeddingspace.ErrMismatch) {
		t.Fatalf("Compact error = %v, want it to remain an identity mismatch", err)
	}
	if converger.ends != 0 {
		t.Fatalf("End = %d calls, want none for an attempt that was never admitted", converger.ends)
	}
}

// TestCompactMixedInputNeverPublishesOneLabel is the invariant behind
// finding 4: whatever the provider does, two spaces never become one
// labelled file. With no converger at all the compaction must fail
// rather than pick a winner.
func TestCompactMixedInputNeverPublishesOneLabel(t *testing.T) {
	a := buildRFileInSpace(t, "a", embeddingspace.Has("space-a"))
	b := buildRFileInSpace(t, "b", embeddingspace.Has("space-x"))
	_, err := Compact(Spec{
		Inputs: []Input{
			{Name: "a.rf", Bytes: a},
			{Name: "b.rf", Bytes: b},
		},
		Scope: iterrt.ScopeMajc,
	})
	if !errors.Is(err, embeddingspace.ErrMismatch) {
		t.Fatalf("Compact error = %v, want ErrMismatch", err)
	}
}

// TestCompactAbsentMetadataEmbeddingDoesNotForceAnIntegrityCheck covers
// finding 10 at the composer boundary: a zero MetadataEmbedding is
// absence, so the footer alone decides and no cross-check runs. An
// explicit claim that disagrees with the footer must still fail.
func TestCompactAbsentMetadataEmbeddingDoesNotForceAnIntegrityCheck(t *testing.T) {
	input := buildRFileInSpace(t, "a", embeddingspace.Has("space-a"))

	result, err := Compact(Spec{
		// MetadataEmbedding deliberately left at its zero value.
		Inputs: []Input{{Name: "a.rf", Bytes: input}},
		Scope:  iterrt.ScopeMajc,
	})
	if err != nil {
		t.Fatalf("an absent metadata column must not fail the compaction: %v", err)
	}
	if got := outputSpace(t, result.Output); got != embeddingspace.Has("space-a") {
		t.Fatalf("output embedding = %+v, want the footer's space", got)
	}

	_, err = Compact(Spec{
		Inputs: []Input{{
			Name:              "a.rf",
			Bytes:             input,
			MetadataEmbedding: embeddingspace.Unknown(),
		}},
		Scope: iterrt.ScopeMajc,
	})
	if !errors.Is(err, embeddingspace.ErrIntegrity) {
		t.Fatalf("Compact error = %v, want a present claim to be cross-checked", err)
	}
}

func strings8k() string {
	buf := make([]byte, embeddingspace.MaxIdentityBytes+1)
	for i := range buf {
		buf[i] = 'x'
	}
	return string(buf)
}

func buildMultiCellRFile(t *testing.T, state embeddingspace.FileState, rows ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := rfile.NewWriter(&buf, rfile.WriterOptions{EmbeddingSpace: state})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		key := &wire.Key{Row: []byte(row), ColumnFamily: []byte("cf"), Timestamp: 1}
		if err := w.Append(key, []byte("v")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func outputSpace(t *testing.T, image []byte) embeddingspace.FileState {
	t.Helper()
	bc, err := bcfile.NewReader(bytes.NewReader(image), int64(len(image)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := rfile.ReadEmbeddingSpaceMetadata(bc, block.Default())
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func outputValues(t *testing.T, image []byte) []string {
	t.Helper()
	var out []string
	err := StreamCells(Spec{
		Inputs: []Input{{Name: "out.rf", Bytes: image}},
		Scope:  iterrt.ScopeMajc,
	}, func(_ *wire.Key, value []byte) error {
		out = append(out, string(value))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
