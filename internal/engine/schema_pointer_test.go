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
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
	"github.com/phrocker/shoal-oss/internal/tablet"
)

func readManifest(t *testing.T, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "graph", "table.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestSchemaPointerPersistsWithoutAManifestVersionBump(t *testing.T) {
	dir := t.TempDir()
	eng, err := engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.CreateTable("graph", engine.TableOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := eng.TableSchema("graph"); err != nil || ok {
		t.Fatalf("fresh table is described: %v, %v", ok, err)
	}
	if _, ok := readManifest(t, dir)["schema"]; ok {
		t.Fatal("fresh manifest carries a schema pointer")
	}
	if err := eng.SetTableSchema("graph", engine.SchemaPointer{ID: "x"}); err == nil {
		t.Fatal("pointer without a version accepted")
	}
	if err := eng.SetTableSchema("graph", engine.SchemaPointer{Version: 1}); err == nil {
		t.Fatal("pointer without an id accepted")
	}
	if err := eng.SetTableSchema("absent", engine.SchemaPointer{ID: "x", Version: 1}); err == nil {
		t.Fatal("pointer on an absent table accepted")
	}
	want := engine.SchemaPointer{ID: "foreman.graph", Version: 7}
	if err := eng.SetTableSchema("graph", want); err != nil {
		t.Fatal(err)
	}
	manifest := readManifest(t, dir)
	if manifest["version"] != float64(1) {
		t.Fatalf("manifest version changed: %v", manifest["version"])
	}
	if pointer, _ := manifest["schema"].(map[string]any); pointer["id"] != "foreman.graph" || pointer["version"] != float64(7) {
		t.Fatalf("manifest pointer = %v", manifest["schema"])
	}
	// Other manifest rewrites keep the pointer.
	if err := eng.SetTableTargetEmbeddingSpace("graph", "space:v1"); err != nil {
		t.Fatal(err)
	}
	if err := eng.SetTableFileFormat("graph", tablet.FormatParquet); err != nil {
		t.Fatal(err)
	}
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}
	eng, err = engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	got, ok, err := eng.TableSchema("graph")
	if err != nil || !ok || got != want {
		t.Fatalf("pointer after reopen = %+v, %v, %v", got, ok, err)
	}
	if space, _ := eng.TableTargetEmbeddingSpace("graph"); space != "space:v1" {
		t.Fatalf("target embedding space after reopen = %q", space)
	}
	var stat engine.TableStat
	for _, st := range eng.Stats() {
		if st.Name == "graph" {
			stat = st
		}
	}
	if stat.Schema == nil || *stat.Schema != want {
		t.Fatalf("stat = %+v", stat)
	}
}

func TestManifestRefusesAnInvalidSchemaPointer(t *testing.T) {
	dir := t.TempDir()
	eng, err := engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.CreateTable("graph", engine.TableOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "graph", "table.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	broken := strings.Replace(string(data), `"version": 1,`, `"version": 1, "schema": {"id": "", "version": 0},`, 1)
	if broken == string(data) {
		t.Fatalf("manifest shape changed: %s", data)
	}
	if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Open(dir, engine.Options{}); err == nil || !strings.Contains(err.Error(), "schema pointer") {
		t.Fatalf("invalid pointer accepted: %v", err)
	}
}

// A file-format change used to rewrite the manifest without the backend
// root, so a rooted table could not be reopened afterwards.
func TestManifestRewritesKeepTheBackendRoot(t *testing.T) {
	dir := t.TempDir()
	rooted, err := storage.NewRooted(memory.New(), dir, "bucket/prefix")
	if err != nil {
		t.Fatal(err)
	}
	eng, err := engine.Open(dir, engine.Options{Backend: rooted})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.CreateTable("graph", engine.TableOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := eng.SetTableFileFormat("graph", tablet.FormatParquet); err != nil {
		t.Fatal(err)
	}
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}
	if root := readManifest(t, dir)["backend_root"]; root != "bucket/prefix" {
		t.Fatalf("backend_root after format change = %v", root)
	}
	eng, err = engine.Open(dir, engine.Options{Backend: rooted})
	if err != nil {
		t.Fatalf("reopen after format change: %v", err)
	}
	eng.Close()
}
