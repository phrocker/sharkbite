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
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
)

// oldBinaryRewrite rewrites files.json the way an older binary does:
// drops ordered/sources/absorbed_sources/name_high_water, sorts active.
func oldBinaryRewrite(t *testing.T, b *memory.Backend, dstDir string) {
	t.Helper()
	p := filepath.Join(dstDir, overwriteTable, "t-0000", "files.json")
	data, err := storage.ReadAll(context.Background(), b, p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"ordered", "sources", "absorbed_sources", "name_high_water"} {
		delete(m, k)
	}
	var active []string
	for _, a := range m["active"].([]any) {
		active = append(active, a.(string))
	}
	sort.Strings(active)
	m["active"] = active
	out, _ := json.Marshal(m)
	b.Put(p, out)
}

func exportForImport(t *testing.T, src *engine.Engine, b *memory.Backend, dstDir string) *engine.RFileExportManifest {
	t.Helper()
	m, err := src.ExportRFiles(context.Background(), overwriteTable, b, engine.RFileExportOptions{DestinationRoot: dstDir, EngineVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestLegacyImportCompactedThenReimport: a legacy import (no producer
// identity recorded) is merged by a local compaction after a local write;
// re-importing the unchanged manifest must not append the producer's old
// value above the local one. The compaction keeps the legacy import's
// content digest in the absorbed set, and the re-imported file claims it.
//
// Known limitation, not tested: if an OLDER binary rewrites files.json
// after this binary's compaction, it drops the absorbed set, and that
// re-import appends the producer file again (a rollback after a compaction
// loses re-import idempotence for the files that compaction merged).
func TestLegacyImportCompactedThenReimport(t *testing.T) {
	for _, variant := range []string{"legacy-before-compaction"} {
		t.Run(variant, func(t *testing.T) {
			ctx := context.Background()
			srcDir := filepath.Join(t.TempDir(), "src")
			dstDir := filepath.Join(t.TempDir(), "dst")
			src := openOverwriteEngine(t, srcDir, nil)
			defer src.Close()
			b := memory.New()
			putLatest(t, src, "cell", "producer")
			applyStep(t, src, stepFlush)
			dst, err := engine.Open(dstDir, engine.Options{Backend: b})
			if err != nil {
				t.Fatal(err)
			}
			if err := dst.ImportRFileManifest(ctx, exportForImport(t, src, b, dstDir)); err != nil {
				t.Fatal(err)
			}
			reopen := func() {
				if err := dst.Close(); err != nil {
					t.Fatal(err)
				}
				oldBinaryRewrite(t, b, dstDir)
				dst, err = engine.Open(dstDir, engine.Options{Backend: b})
				if err != nil {
					t.Fatal(err)
				}
			}
			if variant == "legacy-before-compaction" {
				reopen()
			}
			putLatest(t, dst, "cell", "local")
			if err := dst.Flush(overwriteTable); err != nil {
				t.Fatal(err)
			}
			if err := dst.Compact(overwriteTable, nil); err != nil {
				t.Fatal(err)
			}
			defer dst.Close()
			if got := readLatestOn(t, dst, "cell"); got != "local" {
				t.Fatalf("before re-import = %q", got)
			}
			if err := dst.ImportRFileManifest(ctx, exportForImport(t, src, b, dstDir)); err != nil {
				t.Fatal(err)
			}
			if got := readLatestOn(t, dst, "cell"); got != "local" {
				t.Fatalf("re-import of unchanged manifest = %q, want local (stale read)", got)
			}
		})
	}
}

// TestLegacyClaimNotByFileAfterAppend: legacy import of producer file
// P1(on). The producer then writes off, compacts (P1 leaves its manifest),
// writes on again (P3, byte-identical to P1). The upgraded consumer imports
// [C(off), P3(on)]: C is appended, so P3 is newer than C and must not claim
// the legacy P1 copy in place below it; it is appended, and the consumer
// reads on like the producer.
func TestLegacyClaimNotByFileAfterAppend(t *testing.T) {
	ctx := context.Background()
	srcDir := filepath.Join(t.TempDir(), "src")
	dstDir := filepath.Join(t.TempDir(), "dst")
	src := openOverwriteEngine(t, srcDir, nil)
	defer src.Close()
	b := memory.New()
	putLatest(t, src, "cell", "on")
	applyStep(t, src, stepFlush)
	dst, err := engine.Open(dstDir, engine.Options{Backend: b})
	if err != nil {
		t.Fatal(err)
	}
	first := exportForImport(t, src, b, dstDir)
	if err := dst.ImportRFileManifest(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	oldBinaryRewrite(t, b, dstDir)
	dst, err = engine.Open(dstDir, engine.Options{Backend: b})
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	putLatest(t, src, "cell", "off")
	applyStep(t, src, stepCompact)
	putLatest(t, src, "cell", "on")
	applyStep(t, src, stepFlush)
	if got := readLatestOn(t, src, "cell"); got != "on" {
		t.Fatalf("src = %q", got)
	}
	m := exportForImport(t, src, b, dstDir)
	if len(m.RFiles) != 2 || m.RFiles[1].SHA256 != first.RFiles[0].SHA256 {
		t.Fatalf("premise: want [C, P3==P1], got %d files", len(m.RFiles))
	}
	if err := dst.ImportRFileManifest(ctx, m); err != nil {
		t.Fatal(err)
	}
	if got := readLatestOn(t, dst, "cell"); got != "on" {
		t.Fatalf("import = %q, want on (producer reads on)", got)
	}
}

// TestProducerDirMoveKeepsImportIdentity: a producer file's identity is its
// path relative to the producer's engine directory, so moving the
// producer's data directory (remount, new --dir) leaves it unchanged, and a
// re-import of the same files does not append them above the consumer's
// local writes.
func TestProducerDirMoveKeepsImportIdentity(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	srcDir := filepath.Join(base, "src")
	dstDir := filepath.Join(t.TempDir(), "dst")
	src := openOverwriteEngine(t, srcDir, nil)
	b := memory.New()
	putLatest(t, src, "cell", "producer")
	applyStep(t, src, stepFlush)
	dst, err := engine.Open(dstDir, engine.Options{Backend: b})
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	if err := dst.ImportRFileManifest(ctx, exportForImport(t, src, b, dstDir)); err != nil {
		t.Fatal(err)
	}
	putLatest(t, dst, "cell", "local")
	if err := dst.Flush(overwriteTable); err != nil {
		t.Fatal(err)
	}
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(base, "moved")
	if err := os.Rename(srcDir, moved); err != nil {
		t.Fatal(err)
	}
	src, err = engine.Open(moved, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	if err := dst.ImportRFileManifest(ctx, exportForImport(t, src, b, dstDir)); err != nil {
		t.Fatal(err)
	}
	if got := readLatestOn(t, dst, "cell"); got != "local" {
		t.Fatalf("re-import of the same producer file after dir move = %q, want local", got)
	}
}
