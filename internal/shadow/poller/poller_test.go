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

package poller

import (
	"sort"
	"testing"

	"github.com/phrocker/shoal-oss/internal/metadata"
)

func TestDiffFileSets(t *testing.T) {
	old := map[string]metadata.FileEntry{
		"gs://b/a.rf": {Path: "gs://b/a.rf", Size: 100},
		"gs://b/b.rf": {Path: "gs://b/b.rf", Size: 200},
		"gs://b/c.rf": {Path: "gs://b/c.rf", Size: 300},
	}
	new := map[string]metadata.FileEntry{
		"gs://b/a.rf": {Path: "gs://b/a.rf", Size: 100}, // unchanged
		"gs://b/d.rf": {Path: "gs://b/d.rf", Size: 600}, // compaction output
	}
	removed, added := diffFileSets(old, new)
	sort.Slice(removed, func(i, j int) bool { return removed[i].Path < removed[j].Path })
	sort.Slice(added, func(i, j int) bool { return added[i].Path < added[j].Path })

	wantRemoved := []string{"gs://b/b.rf", "gs://b/c.rf"}
	wantAdded := []string{"gs://b/d.rf"}

	if len(removed) != len(wantRemoved) {
		t.Fatalf("removed = %+v, want %+v", removed, wantRemoved)
	}
	for i, e := range removed {
		if e.Path != wantRemoved[i] {
			t.Errorf("removed[%d] = %q, want %q", i, e.Path, wantRemoved[i])
		}
	}
	if len(added) != len(wantAdded) {
		t.Fatalf("added = %+v, want %+v", added, wantAdded)
	}
	if added[0].Path != wantAdded[0] {
		t.Errorf("added[0] = %q, want %q", added[0].Path, wantAdded[0])
	}
}

func TestClassify(t *testing.T) {
	in := []metadata.FileEntry{{Path: "x"}}
	out := []metadata.FileEntry{{Path: "y"}}
	if got := classify(in, out); got != KindCompaction {
		t.Errorf("compaction shape: got %v, want %v", got, KindCompaction)
	}
	if got := classify(nil, out); got != KindFlush {
		t.Errorf("flush shape: got %v, want %v", got, KindFlush)
	}
	// File shedding (split): inputs removed, no output. Not a
	// compaction.
	if got := classify(in, nil); got != -1 {
		t.Errorf("split-shed shape: got %v, want -1", got)
	}
	// No change: shouldn't reach classify, but be defensive.
	if got := classify(nil, nil); got != -1 {
		t.Errorf("no-change shape: got %v, want -1", got)
	}
}
