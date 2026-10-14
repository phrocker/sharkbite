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
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/phrocker/shoal-oss/internal/metadata"
)

// --- fakes ------------------------------------------------------------

type fakeFence struct {
	verifyErr error
	verifies  int
}

func (f *fakeFence) Fence(context.Context) (FenceToken, error) {
	return FenceToken{Offline: true, Version: 1, Session: 1}, nil
}

func (f *fakeFence) Verify(context.Context, FenceToken) error {
	f.verifies++
	return f.verifyErr
}

type fakeCommitter struct {
	got     []TabletCommit
	failAt  int // 1-based index to fail on; 0 = never
	failErr error
}

func (c *fakeCommitter) Commit(_ context.Context, tc TabletCommit) error {
	c.got = append(c.got, tc)
	if c.failAt != 0 && len(c.got) == c.failAt {
		return c.failErr
	}
	return nil
}

// --- helpers ----------------------------------------------------------

func samplePlan() *Plan {
	return &Plan{
		TableID: "2",
		Results: []TabletResult{
			{
				Tablet: metadata.TabletInfo{TableID: "2", EndRow: []byte("m"), PrevRow: nil},
				Inputs: []metadata.FileEntry{
					{Path: "hdfs:/t/2/a.rf", RawQualifier: []byte(`{"path":"hdfs:/t/2/a.rf","startRow":"","endRow":""}`)},
					{Path: "hdfs:/t/2/b.rf", RawQualifier: []byte(`{"path":"hdfs:/t/2/b.rf","startRow":"","endRow":""}`)},
				},
				OutputPath:     "hdfs:/t/2/Aout.rf",
				OutputSize:     4096,
				EntriesWritten: 120,
			},
			{
				Tablet: metadata.TabletInfo{TableID: "2", EndRow: nil, PrevRow: []byte("m")},
				Inputs: []metadata.FileEntry{
					{Path: "hdfs:/t/2/c.rf", RawQualifier: []byte(`{"path":"hdfs:/t/2/c.rf","startRow":"","endRow":""}`)},
				},
				OutputPath:     "hdfs:/t/2/Aout2.rf",
				OutputSize:     2048,
				EntriesWritten: 55,
			},
		},
	}
}

// --- tests ------------------------------------------------------------

func TestBuildCommitPlan(t *testing.T) {
	cp, err := BuildCommitPlan(samplePlan())
	if err != nil {
		t.Fatalf("BuildCommitPlan: %v", err)
	}
	if cp.TableID != "2" || cp.Mode != "plan" {
		t.Fatalf("header mismatch: %+v", cp)
	}
	if len(cp.Tablets) != 2 {
		t.Fatalf("want 2 tablets, got %d", len(cp.Tablets))
	}
	t0 := cp.Tablets[0]
	if string(t0.EndRow) != "m" || t0.PrevRow != nil {
		t.Fatalf("extent[0] mismatch: %+v", t0)
	}
	if len(t0.DeleteQualifiers) != 2 {
		t.Fatalf("want 2 deletes, got %d", len(t0.DeleteQualifiers))
	}
	if t0.Add.Path != "hdfs:/t/2/Aout.rf" || t0.Add.Size != 4096 || t0.Add.NumEntries != 120 {
		t.Fatalf("add[0] mismatch: %+v", t0.Add)
	}
	// Default tablet: nil EndRow, PrevRow "m".
	t1 := cp.Tablets[1]
	if t1.EndRow != nil || string(t1.PrevRow) != "m" {
		t.Fatalf("extent[1] mismatch: %+v", t1)
	}
}

func TestBuildCommitPlanRejectsMissingRawQualifier(t *testing.T) {
	p := samplePlan()
	p.Results[0].Inputs[1].RawQualifier = nil
	if _, err := BuildCommitPlan(p); err == nil {
		t.Fatal("want error for missing RawQualifier, got nil")
	}
}

func TestCommitPlanIsIndependentCopy(t *testing.T) {
	p := samplePlan()
	cp, err := BuildCommitPlan(p)
	if err != nil {
		t.Fatal(err)
	}
	// Mutating the plan's backing bytes must not change the built plan.
	p.Results[0].Inputs[0].RawQualifier[0] = 'X'
	p.Results[0].Tablet.EndRow[0] = 'Z'
	if cp.Tablets[0].DeleteQualifiers[0][0] == 'X' {
		t.Fatal("delete qualifier aliases the source slice")
	}
	if cp.Tablets[0].EndRow[0] == 'Z' {
		t.Fatal("extent EndRow aliases the source slice")
	}
}

func TestMarshalCommitPlanRoundTrip(t *testing.T) {
	cp, err := BuildCommitPlan(samplePlan())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := MarshalCommitPlan(cp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back CommitPlan
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.TableID != cp.TableID || len(back.Tablets) != len(cp.Tablets) {
		t.Fatalf("round-trip mismatch: %+v", back)
	}
	if string(back.Tablets[0].DeleteQualifiers[0]) != string(cp.Tablets[0].DeleteQualifiers[0]) {
		t.Fatal("delete qualifier bytes not preserved through JSON")
	}
}

func TestCommitPlanModeNoWrites(t *testing.T) {
	f := &fakeFence{}
	cp, err := Commit(context.Background(), samplePlan(), f, FenceToken{}, ModePlan, false, nil)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if f.verifies != 1 {
		t.Fatalf("want 1 fence verify, got %d", f.verifies)
	}
	if cp.Mode != "plan" || len(cp.Tablets) != 2 {
		t.Fatalf("plan mismatch: %+v", cp)
	}
}

func TestCommitFenceTripAborts(t *testing.T) {
	f := &fakeFence{verifyErr: &FenceTrip{Reason: "back ONLINE"}}
	c := &fakeCommitter{}
	_, err := Commit(context.Background(), samplePlan(), f, FenceToken{}, ModeDirect, false, c)
	if err == nil {
		t.Fatal("want fence-trip error, got nil")
	}
	if len(c.got) != 0 {
		t.Fatalf("committer must not run after fence trip; ran %d", len(c.got))
	}
}

func TestCommitDirectApplies(t *testing.T) {
	f := &fakeFence{}
	c := &fakeCommitter{}
	cp, err := Commit(context.Background(), samplePlan(), f, FenceToken{}, ModeDirect, false, c)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if len(c.got) != 2 {
		t.Fatalf("want 2 tablet commits, got %d", len(c.got))
	}
	if cp.Mode != "direct" {
		t.Fatalf("want mode direct, got %q", cp.Mode)
	}
}

func TestCommitDirectRequiresCommitter(t *testing.T) {
	f := &fakeFence{}
	_, err := Commit(context.Background(), samplePlan(), f, FenceToken{}, ModeDirect, false, nil)
	if !errors.Is(err, ErrDirectCommitUnavailable) {
		t.Fatalf("want ErrDirectCommitUnavailable, got %v", err)
	}
}

func TestCommitDryRunSkipsWrites(t *testing.T) {
	f := &fakeFence{}
	c := &fakeCommitter{}
	cp, err := Commit(context.Background(), samplePlan(), f, FenceToken{}, ModeDirect, true, c)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if f.verifies != 1 {
		t.Fatalf("dry-run must still verify the fence; verifies=%d", f.verifies)
	}
	if len(c.got) != 0 {
		t.Fatalf("dry-run must not write; wrote %d", len(c.got))
	}
	if cp == nil || len(cp.Tablets) != 2 {
		t.Fatalf("dry-run should still return the plan: %+v", cp)
	}
}

func TestCommitDirectPartialFailureNamesTablet(t *testing.T) {
	f := &fakeFence{}
	c := &fakeCommitter{failAt: 2, failErr: errors.New("conditional rejected")}
	_, err := Commit(context.Background(), samplePlan(), f, FenceToken{}, ModeDirect, false, c)
	if err == nil {
		t.Fatal("want error on second tablet, got nil")
	}
	if len(c.got) != 2 {
		t.Fatalf("first tablet should have committed before the failure; got %d", len(c.got))
	}
}

func TestParseCommitMode(t *testing.T) {
	for in, want := range map[string]CommitMode{"": ModePlan, "plan": ModePlan, "direct": ModeDirect} {
		got, err := ParseCommitMode(in)
		if err != nil || got != want {
			t.Fatalf("ParseCommitMode(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseCommitMode("bogus"); err == nil {
		t.Fatal("want error for bogus mode")
	}
}
