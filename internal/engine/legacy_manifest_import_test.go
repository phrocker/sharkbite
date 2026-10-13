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

package engine_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"testing"

	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
)

// TestLegacyNameSortedManifestRanksImportNewer: a tablet flushed its own
// value, then fan-in imported a newer one. A pre-#692 binary (or an older
// binary rolled back to) writes files.json name-sorted, without the
// "ordered" marker, which puts the ".shoal-import-*" file first. On reopen
// that unmarked order is not trusted: imports rank after local files, so
// the imported (newer) value still wins.
func TestLegacyNameSortedManifestRanksImportNewer(t *testing.T) {
	ctx := context.Background()
	dir, backend := filepath.Join(t.TempDir(), "middle"), memory.New()
	middle, err := engine.Open(dir, engine.Options{Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	if err := middle.CreateTable(overwriteTable, engine.TableOptions{}); err != nil {
		t.Fatal(err)
	}
	putLatest(t, middle, "cell", "own-older")
	applyStep(t, middle, stepFlush)
	producer := openOverwriteEngine(t, filepath.Join(t.TempDir(), "producer"), nil)
	defer producer.Close()
	putLatest(t, producer, "cell", "imported-newer")
	m, err := producer.ExportRFiles(ctx, overwriteTable, backend, engine.RFileExportOptions{DestinationRoot: dir, EngineVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := middle.ImportRFileManifest(ctx, m); err != nil {
		t.Fatal(err)
	}
	if got := readLatestOn(t, middle, "cell"); got != "imported-newer" {
		t.Fatalf("live latest = %q, want imported-newer", got)
	}
	if err := middle.Close(); err != nil {
		t.Fatal(err)
	}

	// Rewrite files.json the way an older binary would: known fields only
	// (the marker and high-water mark are dropped), active sorted by name.
	mp := filepath.Join(dir, overwriteTable, "t-0000", "files.json")
	data, err := storage.ReadAll(ctx, backend, mp)
	if err != nil {
		t.Fatal(err)
	}
	var current struct {
		Active   []string `json:"active"`
		Obsolete []string `json:"obsolete"`
	}
	if err := json.Unmarshal(data, &current); err != nil {
		t.Fatal(err)
	}
	sort.Strings(current.Active)
	legacy := map[string]any{"version": 1, "active": current.Active}
	if len(current.Obsolete) > 0 {
		legacy["obsolete"] = current.Obsolete
	}
	data, err = json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := storage.WriteAll(ctx, backend, mp, data); err != nil {
		t.Fatal(err)
	}

	reopened, err := engine.Open(dir, engine.Options{Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := readLatestOn(t, reopened, "cell"); got != "imported-newer" {
		t.Fatalf("after legacy reopen latest = %q, want imported-newer (legacy active %v)", got, current.Active)
	}
	// The next write re-marks the manifest as ordered with the repaired order.
	putLatest(t, reopened, "cell", "local-newest")
	applyStep(t, reopened, stepFlush)
	if got := readLatestOn(t, reopened, "cell"); got != "local-newest" {
		t.Fatalf("after flush latest = %q, want local-newest", got)
	}
	data, err = storage.ReadAll(ctx, backend, mp)
	if err != nil {
		t.Fatal(err)
	}
	var marked struct {
		Ordered bool `json:"ordered"`
	}
	if err := json.Unmarshal(data, &marked); err != nil || !marked.Ordered {
		t.Fatalf("manifest not re-marked ordered: %s", data)
	}
}
