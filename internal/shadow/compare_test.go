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

package shadow

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/rfile"
	"github.com/phrocker/shoal-oss/internal/rfile/wire"
)

// mkKey builds a key with empty visibility, the common shape in
// compaction inputs.
func mkKey(row, cf, cq string, ts int64) *wire.Key {
	return &wire.Key{
		Row:             []byte(row),
		ColumnFamily:    []byte(cf),
		ColumnQualifier: []byte(cq),
		Timestamp:       ts,
	}
}

// writeSyntheticRFile turns a key-sorted (key, value) sequence into an
// RFile byte stream. The shadow oracle's tests treat this as an input
// blob OR a synthetic "java output" — when shoal's compaction is an
// identity passthrough, shoal output == java output (synthetic) and
// every tier should pass.
func writeSyntheticRFile(t *testing.T, cells [][2]any) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := rfile.NewWriter(&buf, rfile.WriterOptions{})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for i, c := range cells {
		k := c[0].(*wire.Key)
		v := []byte(c[1].(string))
		if err := w.Append(k, v); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

// TestCompare_IdentityFullParity: shoal's output of identity-compacting a
// single input must match a "java output" that's the same as that input.
// T2 hash digest matches; T3 is vestigial (Attempted=false).
func TestCompare_IdentityFullParity(t *testing.T) {
	cells := [][2]any{
		{mkKey("r1", "cf", "a", 30), "r1a-v30"},
		{mkKey("r1", "cf", "b", 20), "r1b-v20"},
		{mkKey("r2", "cf", "a", 9), "r2a-v9"},
	}
	rfileBytes := writeSyntheticRFile(t, cells)

	spec := CompareSpec{
		Inputs: []InputBlob{{Name: "input1", Bytes: rfileBytes}},
		Stack:  nil, // identity compaction
		Scope:  iterrt.ScopeMajc,
	}
	report, err := Compare(spec, rfileBytes)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !report.JavaCellsSourced {
		t.Fatal("expected JavaCellsSourced=true")
	}
	if report.ShoalCells != 3 || report.JavaCells != 3 {
		t.Fatalf("cell counts: shoal=%d java=%d (want 3/3)", report.ShoalCells, report.JavaCells)
	}
	if !report.T2.Attempted || !report.T2.Passed {
		t.Fatalf("T2 should pass on identity compaction: %+v", report.T2)
	}
	if report.T3.Attempted {
		t.Fatalf("T3 is vestigial under the streaming-hash design; Attempted should stay false: %+v", report.T3)
	}
	if report.ShoalSummary.TotalCells != 3 || report.JavaSummary.TotalCells != 3 {
		t.Fatalf("T4 summaries: shoal=%d java=%d", report.ShoalSummary.TotalCells, report.JavaSummary.TotalCells)
	}
}

// TestCompare_VersioningDivergence: shoal applies VersioningIterator
// max=1; the "java output" is the raw inputs (no versioning). Hash
// digests differ; the second-pass divergence walk pins it to cell 1
// (shoal ended after one cell, java still had more).
func TestCompare_VersioningDivergence(t *testing.T) {
	cells := [][2]any{
		{mkKey("r1", "cf", "a", 30), "r1a-v30"}, // newest
		{mkKey("r1", "cf", "a", 20), "r1a-v20"},
		{mkKey("r1", "cf", "a", 10), "r1a-v10"},
	}
	rfileBytes := writeSyntheticRFile(t, cells)

	spec := CompareSpec{
		Inputs: []InputBlob{{Name: "input1", Bytes: rfileBytes}},
		Stack: []iterrt.IterSpec{
			{Name: iterrt.IterVersioning, Options: map[string]string{
				iterrt.VersioningOption: "1",
			}},
		},
		Scope: iterrt.ScopeMajc,
	}
	report, err := Compare(spec, rfileBytes)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if report.ShoalCells != 1 {
		t.Fatalf("shoal should produce 1 cell after max=1 versioning, got %d", report.ShoalCells)
	}
	if report.JavaCells != 3 {
		t.Fatalf("java synthetic should have 3 cells, got %d", report.JavaCells)
	}
	if report.T2.Passed {
		t.Fatalf("T2 should fail on divergent cell counts: %+v", report.T2)
	}
	if report.T2.FirstDivergenceIndex < 1 {
		t.Fatalf("T2 first divergence index = %d, want >= 1 (shoal kept cell 0, ended at cell 1)",
			report.T2.FirstDivergenceIndex)
	}
	if report.T2.CellSeqDiff == "" {
		t.Errorf("T2.CellSeqDiff should be populated on mismatch")
	}
}

// TestCompare_NoJavaOutput: when caller doesn't supply java bytes, T2/T3
// are skipped but the shoal compaction still runs and T4 summary is
// populated. Useful "shoal-only sanity" mode.
func TestCompare_NoJavaOutput(t *testing.T) {
	cells := [][2]any{
		{mkKey("r1", "cf", "a", 100), "hello"},
	}
	rfileBytes := writeSyntheticRFile(t, cells)
	spec := CompareSpec{
		Inputs: []InputBlob{{Name: "input1", Bytes: rfileBytes}},
		Scope:  iterrt.ScopeMajc,
	}
	report, err := Compare(spec, nil)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if report.JavaCellsSourced {
		t.Fatal("JavaCellsSourced should be false when no java output supplied")
	}
	if report.T2.Attempted {
		t.Fatal("T2 should not be attempted when java output is empty")
	}
	if report.T3.Attempted {
		t.Fatal("T3 is always Attempted=false under the streaming-hash design")
	}
	if report.ShoalSummary.TotalCells != 1 {
		t.Fatalf("shoal summary cells=%d, want 1", report.ShoalSummary.TotalCells)
	}
}

// TestCompare_ValueMismatchPinpointed: when one cell differs only in its
// value (same key), the divergence walk pins it to that exact cell and
// the diff string contains both values for the report.
func TestCompare_ValueMismatchPinpointed(t *testing.T) {
	shoalCells := [][2]any{
		{mkKey("r1", "cf", "a", 30), "before"},
		{mkKey("r1", "cf", "b", 20), "matches"},
		{mkKey("r2", "cf", "a", 9), "matches-too"},
	}
	javaCells := [][2]any{
		{mkKey("r1", "cf", "a", 30), "before"},
		{mkKey("r1", "cf", "b", 20), "DIFFERS"}, // cell 1 differs only in value
		{mkKey("r2", "cf", "a", 9), "matches-too"},
	}
	// Shoal's input == shoal cells (identity compaction yields shoal's
	// output). Java output is the second slice; T2 must catch the value
	// divergence at cell index 1.
	shoalInput := writeSyntheticRFile(t, shoalCells)
	javaOutput := writeSyntheticRFile(t, javaCells)

	spec := CompareSpec{
		Inputs: []InputBlob{{Name: "input1", Bytes: shoalInput}},
		Scope:  iterrt.ScopeMajc,
	}
	report, err := Compare(spec, javaOutput)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if report.T2.Passed {
		t.Fatalf("T2 should fail on value divergence: %+v", report.T2)
	}
	if report.T2.FirstDivergenceIndex != 1 {
		t.Errorf("FirstDivergenceIndex = %d, want 1", report.T2.FirstDivergenceIndex)
	}
	if !contains(report.T2.CellSeqDiff, "matches") || !contains(report.T2.CellSeqDiff, "DIFFERS") {
		t.Errorf("CellSeqDiff should name both values; got: %q", report.T2.CellSeqDiff)
	}
}

func contains(s, sub string) bool { return bytes.Contains([]byte(s), []byte(sub)) }

// TestCompare_T1Skipped: with EnvJavaRFileValidate unset, T1.Attempted is
// false and the rest of the report still populates.
func TestCompare_T1Skipped(t *testing.T) {
	t.Setenv(EnvJavaRFileValidate, "")
	cells := [][2]any{
		{mkKey("r1", "cf", "a", 10), "v"},
	}
	b := writeSyntheticRFile(t, cells)
	report, err := Compare(CompareSpec{
		Inputs: []InputBlob{{Name: "i", Bytes: b}},
		Scope:  iterrt.ScopeMajc,
	}, b)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if report.T1.Attempted {
		t.Fatalf("T1 should be skipped, got %+v", report.T1)
	}
}

// TestCompare_T1Passed: with a SHOAL_JAVA_RFILE_VALIDATE that always
// returns 0, T1.Passed=true.
func TestCompare_T1Passed(t *testing.T) {
	t.Setenv(EnvJavaRFileValidate, validatorCommand(true))
	cells := [][2]any{
		{mkKey("r1", "cf", "a", 10), "v"},
	}
	b := writeSyntheticRFile(t, cells)
	report, err := Compare(CompareSpec{
		Inputs: []InputBlob{{Name: "i", Bytes: b}},
		Scope:  iterrt.ScopeMajc,
	}, b)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !report.T1.Attempted || !report.T1.Passed {
		t.Fatalf("T1 should be attempted and pass, got %+v", report.T1)
	}
}

// TestCompare_T1Failed: a validator that always exits non-zero surfaces
// as T1.Passed=false with the stderr in T1.Error.
func TestCompare_T1Failed(t *testing.T) {
	t.Setenv(EnvJavaRFileValidate, validatorCommand(false))
	cells := [][2]any{
		{mkKey("r1", "cf", "a", 10), "v"},
	}
	b := writeSyntheticRFile(t, cells)
	report, err := Compare(CompareSpec{
		Inputs: []InputBlob{{Name: "i", Bytes: b}},
		Scope:  iterrt.ScopeMajc,
	}, b)
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if !report.T1.Attempted {
		t.Fatal("T1 should have been attempted")
	}
	if report.T1.Passed {
		t.Fatalf("T1 should fail, got %+v", report.T1)
	}
	if report.T1.Error == "" {
		t.Fatal("T1.Error should be populated on failure")
	}
}

func validatorCommand(success bool) string {
	shell, _ := validatorShell()
	switch shell {
	case "powershell":
		if success {
			return "exit 0 # $RFILE"
		}
		return "Write-Error 'simulated-rejection'; exit 7 # $RFILE"
	case "cmd":
		if success {
			return "exit /b 0"
		}
		return "echo simulated-rejection 1>&2 & exit /b 7"
	default:
		if success {
			return "true # $RFILE"
		}
		return fmt.Sprintf("echo simulated-rejection >&2; exit %d # $RFILE", 7)
	}
}
