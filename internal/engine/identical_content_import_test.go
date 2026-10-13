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
	"path/filepath"
	"testing"

	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
)

// TestFanInImportOfByteIdenticalFileKeepsRecency: a producer toggles a cell
// on, off, on. Its third flush is byte-identical to its first, so the
// importer's content-addressed identity matches the file it already holds.
// That copy must move to the newest position; deduping it in place would
// leave the importer reading "off" while the producer reads "on". No salt
// cell: the identical content is the point.
func TestFanInImportOfByteIdenticalFileKeepsRecency(t *testing.T) {
	ctx := context.Background()
	srcDir := filepath.Join(t.TempDir(), "src")
	dstDir := filepath.Join(t.TempDir(), "dst")
	src := openOverwriteEngine(t, srcDir, nil)
	defer src.Close()
	dstBackend := memory.New()
	export := func() *engine.RFileExportManifest {
		t.Helper()
		m, err := src.ExportRFiles(ctx, overwriteTable, dstBackend, engine.RFileExportOptions{DestinationRoot: dstDir, EngineVersion: "test"})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	putLatest(t, src, "cell", "on")
	applyStep(t, src, stepFlush)
	putLatest(t, src, "cell", "off")
	applyStep(t, src, stepFlush)
	dst, err := engine.Open(dstDir, engine.Options{Backend: dstBackend})
	if err != nil {
		t.Fatal(err)
	}
	if err := dst.ImportRFileManifest(ctx, export()); err != nil {
		t.Fatal(err)
	}
	if got := readLatestOn(t, dst, "cell"); got != "off" {
		t.Fatalf("first import = %q, want off", got)
	}
	putLatest(t, src, "cell", "on")
	applyStep(t, src, stepFlush)
	if got := readLatestOn(t, src, "cell"); got != "on" {
		t.Fatalf("source = %q", got)
	}
	m := export()
	if m.RFiles[0].SHA256 != m.RFiles[2].SHA256 {
		t.Fatalf("test premise: first and third flush should be byte-identical")
	}
	if err := dst.ImportRFileManifest(ctx, m); err != nil {
		t.Fatal(err)
	}
	if got := readLatestOn(t, dst, "cell"); got != "on" {
		t.Fatalf("fan-in import latest = %q, want on (source reads on)", got)
	}
	// Re-importing the unchanged manifest is order-stable.
	if err := dst.ImportRFileManifest(ctx, export()); err != nil {
		t.Fatal(err)
	}
	if got := readLatestOn(t, dst, "cell"); got != "on" {
		t.Fatalf("re-import latest = %q, want on", got)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := engine.Open(dstDir, engine.Options{Backend: dstBackend})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := readLatestOn(t, reopened, "cell"); got != "on" {
		t.Fatalf("after reopen latest = %q, want on", got)
	}
}

// TestReimportAfterLocalWriteKeepsLocalNewest: the consumer imports a
// producer's on/off/on history, then writes and flushes its own value.
// Re-importing the UNCHANGED manifest must not replay the history above the
// local write; a grown manifest appends only its new file (#692 review).
func TestReimportAfterLocalWriteKeepsLocalNewest(t *testing.T) {
	ctx := context.Background()
	for _, variant := range []string{"unchanged", "grown", "after-local-compaction"} {
		t.Run(variant, func(t *testing.T) {
			srcDir := filepath.Join(t.TempDir(), "src")
			dstDir := filepath.Join(t.TempDir(), "dst")
			src := openOverwriteEngine(t, srcDir, nil)
			defer src.Close()
			dstBackend := memory.New()
			export := func() *engine.RFileExportManifest {
				t.Helper()
				m, err := src.ExportRFiles(ctx, overwriteTable, dstBackend, engine.RFileExportOptions{DestinationRoot: dstDir, EngineVersion: "test"})
				if err != nil {
					t.Fatal(err)
				}
				return m
			}
			for _, v := range []string{"on", "off", "on"} {
				putLatest(t, src, "cell", v)
				applyStep(t, src, stepFlush)
			}
			dst, err := engine.Open(dstDir, engine.Options{Backend: dstBackend})
			if err != nil {
				t.Fatal(err)
			}
			defer dst.Close()
			if err := dst.ImportRFileManifest(ctx, export()); err != nil {
				t.Fatal(err)
			}
			if got := readLatestOn(t, dst, "cell"); got != "on" {
				t.Fatalf("import = %q, want on", got)
			}
			putLatest(t, dst, "cell", "local")
			if err := dst.Flush(overwriteTable); err != nil {
				t.Fatal(err)
			}
			if variant == "after-local-compaction" {
				if err := dst.Compact(overwriteTable, nil); err != nil {
					t.Fatal(err)
				}
				putLatest(t, dst, "cell", "local")
				if err := dst.Flush(overwriteTable); err != nil {
					t.Fatal(err)
				}
			}
			if got := readLatestOn(t, dst, "cell"); got != "local" {
				t.Fatalf("after local write = %q, want local", got)
			}
			want := "local"
			if variant == "grown" {
				putLatest(t, src, "other", "z")
				applyStep(t, src, stepFlush)
			}
			if err := dst.ImportRFileManifest(ctx, export()); err != nil {
				t.Fatal(err)
			}
			if got := readLatestOn(t, dst, "cell"); got != want {
				t.Fatalf("re-import latest = %q, want %q", got, want)
			}
			if variant == "grown" {
				if got := readLatestOn(t, dst, "other"); got != "z" {
					t.Fatalf("grown file not imported: other = %q", got)
				}
			}
			if err := dst.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := engine.Open(dstDir, engine.Options{Backend: dstBackend})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if err := reopened.ImportRFileManifest(ctx, export()); err != nil {
				t.Fatal(err)
			}
			if got := readLatestOn(t, reopened, "cell"); got != want {
				t.Fatalf("re-import after reopen latest = %q, want %q", got, want)
			}
		})
	}
}
