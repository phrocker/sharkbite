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

package replay_test

import (
	"context"
	"testing"

	"github.com/phrocker/shoal-oss/internal/embedstore"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/replay"
)

func newLedger(t *testing.T) (*replay.Ledger, *embedstore.EngineStore, string) {
	t.Helper()
	eng, err := engine.Open(t.TempDir(), engine.Options{})
	if err != nil {
		t.Fatalf("open engine: %v", err)
	}
	t.Cleanup(func() { eng.Close() })
	store := embedstore.New(eng)
	const table = "log"
	if err := store.CreateTable(context.Background(), table, []string{"replay:"}); err != nil {
		t.Fatalf("create table: %v", err)
	}
	return replay.NewLedger(store, table), store, table
}

func TestAppendAutoSeqAndReplayOrder(t *testing.T) {
	ctx := context.Background()
	l, store, table := newLedger(t)

	if _, err := l.Append(ctx, "corr-1", replay.Step{Action: "plan"}); err != nil {
		t.Fatalf("append 1: %v", err)
	}
	if _, err := l.Append(ctx, "corr-1", replay.Step{Action: "search", Hash: "abc"}); err != nil {
		t.Fatalf("append 2: %v", err)
	}
	if _, err := l.Append(ctx, "corr-1", replay.Step{Action: "write", Meta: map[string]string{"file": "x.go"}}); err != nil {
		t.Fatalf("append 3: %v", err)
	}
	// A different correlation must not interleave.
	if _, err := l.Append(ctx, "corr-2", replay.Step{Action: "other"}); err != nil {
		t.Fatalf("append other: %v", err)
	}
	if err := store.Flush(ctx, table); err != nil {
		t.Fatalf("flush: %v", err)
	}

	rep, err := l.Replay(ctx, "corr-1", replay.ReplayOptions{DryRun: true})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !rep.DryRun {
		t.Error("DryRun should be echoed")
	}
	if len(rep.Steps) != 3 {
		t.Fatalf("corr-1 should have 3 steps, got %d", len(rep.Steps))
	}
	wantSeq := []int64{1, 2, 3}
	wantAction := []string{"plan", "search", "write"}
	for i, s := range rep.Steps {
		if s.Seq != wantSeq[i] || s.Action != wantAction[i] {
			t.Errorf("step %d: got seq=%d action=%q want seq=%d action=%q", i, s.Seq, s.Action, wantSeq[i], wantAction[i])
		}
	}
	if rep.Steps[1].Hash != "abc" {
		t.Errorf("hash not preserved: %q", rep.Steps[1].Hash)
	}
	if rep.Steps[2].Meta["file"] != "x.go" {
		t.Errorf("meta not preserved: %+v", rep.Steps[2].Meta)
	}

	other, _ := l.Replay(ctx, "corr-2", replay.ReplayOptions{})
	if len(other.Steps) != 1 || other.Steps[0].Action != "other" {
		t.Errorf("corr-2 isolation broken: %+v", other.Steps)
	}
}

// TestReplayAsOf bounds the replay to a timestamp ceiling: steps appended with
// explicit TimestampMs after the ceiling are excluded.
func TestReplayAsOf(t *testing.T) {
	ctx := context.Background()
	l, store, table := newLedger(t)

	if _, err := l.Append(ctx, "c", replay.Step{Seq: 1, Action: "a", TimestampMs: 100}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := l.Append(ctx, "c", replay.Step{Seq: 2, Action: "b", TimestampMs: 200}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := l.Append(ctx, "c", replay.Step{Seq: 3, Action: "c", TimestampMs: 300}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := store.Flush(ctx, table); err != nil {
		t.Fatalf("flush: %v", err)
	}

	rep, err := l.Replay(ctx, "c", replay.ReplayOptions{AsOf: 250})
	if err != nil {
		t.Fatalf("replay as-of: %v", err)
	}
	if len(rep.Steps) != 2 {
		t.Fatalf("as-of 250 should include 2 steps, got %d (%+v)", len(rep.Steps), rep.Steps)
	}
	if rep.Steps[0].Action != "a" || rep.Steps[1].Action != "b" {
		t.Errorf("as-of steps wrong: %+v", rep.Steps)
	}
}

func TestAppendValidation(t *testing.T) {
	ctx := context.Background()
	l, _, _ := newLedger(t)
	if _, err := l.Append(ctx, "", replay.Step{Action: "x"}); err == nil {
		t.Error("empty correlationID should error")
	}
	if _, err := l.Append(ctx, "c", replay.Step{}); err == nil {
		t.Error("empty action should error")
	}
}
