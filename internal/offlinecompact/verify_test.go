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

package offlinecompact

import (
	"testing"

	"github.com/phrocker/shoal-oss/internal/compaction"
	"github.com/phrocker/shoal-oss/internal/iterrt"
)

// majcSpec builds an identity-stack full-major spec over one input RFile.
func majcSpec(t *testing.T, cells []kv) (compaction.Spec, *compaction.Result) {
	t.Helper()
	spec := compaction.Spec{
		Inputs:              []compaction.Input{{Name: "in.rf", Bytes: buildRFile(t, cells)}},
		Scope:               iterrt.ScopeMajc,
		FullMajorCompaction: true,
	}
	res, err := compaction.Compact(spec)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	return spec, res
}

func TestVerifyTablet_Pass(t *testing.T) {
	cells := []kv{
		{"r1", "cf", "a", 10, "v1"},
		{"r2", "cf", "b", 10, "v2"},
	}
	spec, res := majcSpec(t, cells)

	rep, err := VerifyTablet(spec, res.Output, res.EntriesWritten)
	if err != nil {
		t.Fatalf("VerifyTablet: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("expected OK, got mismatch=%v selfConsistent=%v", rep.Mismatch, rep.SelfConsistent)
	}
	if rep.CellsCompared != 2 {
		t.Fatalf("CellsCompared=%d, want 2", rep.CellsCompared)
	}
	if rep.Shadow == nil || rep.Shadow.ShoalSummary.TotalCells != 2 {
		t.Fatalf("shadow T4 summary missing or wrong: %+v", rep.Shadow)
	}
	if rep.Shadow.T1.Attempted {
		t.Fatalf("T1 should be skipped without SHOAL_JAVA_RFILE_VALIDATE, got %+v", rep.Shadow.T1)
	}
}

func TestVerifyTablet_ValueDivergencePinned(t *testing.T) {
	cells := []kv{
		{"r1", "cf", "a", 10, "v1"},
		{"r2", "cf", "b", 10, "v2"},
	}
	spec, _ := majcSpec(t, cells)

	// A tampered output: second cell's value differs from the stream.
	wrong := buildRFile(t, []kv{
		{"r1", "cf", "a", 10, "v1"},
		{"r2", "cf", "b", 10, "TAMPERED"},
	})

	rep, err := VerifyTablet(spec, wrong, 2)
	if err != nil {
		t.Fatalf("VerifyTablet: %v", err)
	}
	if rep.OK() {
		t.Fatal("expected verification to fail")
	}
	if rep.Mismatch == nil || rep.Mismatch.Index != 1 {
		t.Fatalf("mismatch should pin cell 1, got %v", rep.Mismatch)
	}
}

func TestVerifyTablet_OutputEndsEarly(t *testing.T) {
	cells := []kv{
		{"r1", "cf", "a", 10, "v1"},
		{"r2", "cf", "b", 10, "v2"},
	}
	spec, _ := majcSpec(t, cells)

	short := buildRFile(t, []kv{{"r1", "cf", "a", 10, "v1"}})

	rep, err := VerifyTablet(spec, short, 2)
	if err != nil {
		t.Fatalf("VerifyTablet: %v", err)
	}
	if rep.OK() || rep.Mismatch == nil || rep.Mismatch.Index != 1 {
		t.Fatalf("want mismatch at cell 1 (output short), got %v", rep.Mismatch)
	}
}

func TestVerifyTablet_OutputHasExtraCells(t *testing.T) {
	cells := []kv{
		{"r1", "cf", "a", 10, "v1"},
		{"r2", "cf", "b", 10, "v2"},
	}
	spec, _ := majcSpec(t, cells)

	extra := buildRFile(t, []kv{
		{"r1", "cf", "a", 10, "v1"},
		{"r2", "cf", "b", 10, "v2"},
		{"r3", "cf", "c", 10, "v3"},
	})

	rep, err := VerifyTablet(spec, extra, 3)
	if err != nil {
		t.Fatalf("VerifyTablet: %v", err)
	}
	if rep.OK() || rep.Mismatch == nil {
		t.Fatalf("want mismatch for extra output cells, got %v", rep.Mismatch)
	}
}

func TestVerifyTablet_CountMismatch(t *testing.T) {
	cells := []kv{
		{"r1", "cf", "a", 10, "v1"},
		{"r2", "cf", "b", 10, "v2"},
	}
	spec, res := majcSpec(t, cells)

	// Output is byte-correct but the reported count is a lie.
	rep, err := VerifyTablet(spec, res.Output, 99)
	if err != nil {
		t.Fatalf("VerifyTablet: %v", err)
	}
	if rep.OK() || rep.Mismatch == nil {
		t.Fatalf("want count-mismatch failure, got %v", rep.Mismatch)
	}
}
