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

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/tableschema"
)

const foremanDescriptor = `{
  "table": "graph",
  "schema_id": "foreman.graph",
  "version": 1,
  "description": "foreman entities and events",
  "nodes": [
    {"name": "entity", "row_prefix": "ent:", "kind_sep": "|", "kind_index": 0,
     "attribute_qualifier": "QUALIFIER_ROLE_FIELD", "attribute_encoding": "VALUE_ENCODING_TEXT"},
    {"name": "event", "row_prefix": "evt:", "kind_sep": "|", "kind_index": 0,
     "attribute_qualifier": "QUALIFIER_ROLE_FIELD", "attribute_encoding": "VALUE_ENCODING_TEXT"}
  ],
  "edges": [
    {"name": "edge", "edge_cf": "edge", "edge_field": "qualifier", "field_sep": "\u0000",
     "id_index": 1, "id_index_set": true, "rel_index": 0}
  ]
}`

func TestSchemaCommands(t *testing.T) {
	t.Setenv("SHOAL_BACKEND", "")
	t.Setenv("SHOAL_BACKEND_ROOT", "")
	dir := t.TempDir()
	run := func(args ...string) (string, error) {
		t.Helper()
		out, err := backendCommand(t, append(args, "-data", dir)...).CombinedOutput()
		return string(out), err
	}
	mustRun := func(args ...string) string {
		t.Helper()
		out, err := run(args...)
		if err != nil {
			t.Fatalf("%v failed: %v\n%s", args, err, out)
		}
		return out
	}

	if out, err := run("init", "-table", tableschema.Table); err == nil || !strings.Contains(out, "reserved") {
		t.Fatalf("init of the reserved table: %v\n%s", err, out)
	}
	mustRun("init", "-table", "graph")

	if out := mustRun("status"); !strings.Contains(out, "SCHEMA") || !strings.Contains(out, "graph") || strings.Contains(out, "foreman.graph@") {
		t.Fatalf("status before put:\n%s", out)
	}
	if out := mustRun("schema", "list"); !strings.Contains(out, "no described tables") {
		t.Fatalf("list before put:\n%s", out)
	}
	if out, err := run("schema", "get", "-table", "graph"); err == nil || !strings.Contains(out, "not described") {
		t.Fatalf("get before put: %v\n%s", err, out)
	}

	file := filepath.Join(t.TempDir(), "graph.json")
	if err := os.WriteFile(file, []byte(foremanDescriptor), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := run("schema", "put", "-table", "events", "-file", file); err == nil || !strings.Contains(out, `describes table "graph"`) {
		t.Fatalf("put under the wrong table name: %v\n%s", err, out)
	}
	if out := mustRun("schema", "put", "-table", "graph", "-file", file); !strings.Contains(out, "recorded graph schema foreman.graph version 1") {
		t.Fatalf("put:\n%s", out)
	}
	if out, err := run("schema", "put", "-table", "graph", "-file", file); err != nil || !strings.Contains(out, "version 1") {
		t.Fatalf("identical re-put: %v\n%s", err, out)
	}
	if err := os.WriteFile(file, []byte(strings.Replace(foremanDescriptor, "foreman entities", "revised", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := run("schema", "put", "-table", "graph", "-file", file); err == nil || !strings.Contains(out, "different document") {
		t.Fatalf("changed re-put of version 1: %v\n%s", err, out)
	}
	if err := os.WriteFile(file, []byte(strings.Replace(foremanDescriptor, `"version": 1`, `"version": 2`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRun("schema", "put", "-table", "graph", "-file", file)

	out := mustRun("schema", "get", "-table", "graph")
	if !strings.Contains(out, `"version": 2`) || !strings.Contains(out, `"field_sep": "\u0000"`) {
		t.Fatalf("get newest:\n%s", out)
	}
	if out := mustRun("schema", "get", "-table", "graph", "-version", "1"); !strings.Contains(out, `"version": 1`) {
		t.Fatalf("get version 1:\n%s", out)
	}
	if out := mustRun("schema", "list"); !strings.Contains(out, "foreman.graph") || !strings.Contains(out, "2") {
		t.Fatalf("list:\n%s", out)
	}
	if out := mustRun("status"); !strings.Contains(out, "foreman.graph@2") || !strings.Contains(out, tableschema.Table) {
		t.Fatalf("status after put:\n%s", out)
	}
	// The document is plain cells of the reserved table.
	if out := mustRun("scan", "-table", tableschema.Table); !strings.Contains(out, "foreman.graph") || !strings.Contains(out, "00000000000000000002") {
		t.Fatalf("scan of the reserved table:\n%s", out)
	}
	// Opening the store with a binary that has never heard of the pointer is
	// covered in internal/engine; here the pointer must survive reopen.
	if out := mustRun("schema", "list"); !strings.Contains(out, "foreman.graph") {
		t.Fatalf("list after reopen:\n%s", out)
	}
}

// A usage error creates no store: the engine is opened only after the
// command's inputs are validated.
func TestSchemaPutValidatesBeforeOpeningTheStore(t *testing.T) {
	t.Setenv("SHOAL_BACKEND", "")
	t.Setenv("SHOAL_BACKEND_ROOT", "")
	dir := filepath.Join(t.TempDir(), "unopened")
	if out, err := backendCommand(t, "schema", "put", "-data", dir).CombinedOutput(); err == nil || !strings.Contains(string(out), "--table is required") {
		t.Fatalf("missing table: %v\n%s", err, out)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("store created by a usage error: %v", err)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`{"table":"graph"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := backendCommand(t, "schema", "put", "-data", dir, "-table", "graph", "-file", bad).CombinedOutput(); err == nil || !strings.Contains(string(out), "schema_id") {
		t.Fatalf("invalid descriptor: %v\n%s", err, out)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("store created by an invalid descriptor: %v", err)
	}
}
