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
	"testing"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/iterrt"
)

func TestEngine_StatsAndMetrics(t *testing.T) {
	eng, err := engine.Open(t.TempDir(), engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	if err := eng.CreateTable("graph", engine.TableOptions{Splits: engine.PrefixSplit("evt:", "ent:")}); err != nil {
		t.Fatal(err)
	}
	if err := eng.CreateTable("log", engine.TableOptions{}); err != nil {
		t.Fatal(err)
	}

	// Two Write calls, 3 mutations, 4 cells total.
	m1, _ := cclient.NewMutation([]byte("evt:0001"))
	m1.PutLatest([]byte("content"), []byte("body"), nil, []byte("hello"))
	m1.PutLatest([]byte("attr"), []byte("k"), nil, []byte("v"))
	if err := eng.Write("graph", []*cclient.Mutation{m1}); err != nil {
		t.Fatal(err)
	}
	m2, _ := cclient.NewMutation([]byte("ent:abc"))
	m2.PutLatest([]byte("content"), []byte("label"), nil, []byte("Acme"))
	m3, _ := cclient.NewMutation([]byte("evt:0002"))
	m3.PutLatest([]byte("content"), []byte("body"), nil, []byte("world"))
	if err := eng.Write("graph", []*cclient.Mutation{m2, m3}); err != nil {
		t.Fatal(err)
	}

	// One scan, one flush, one compact.
	sc, err := eng.Scan("graph", iterrt.InfiniteRange(), engine.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for sc.Next() {
		if err := sc.Advance(); err != nil {
			t.Fatal(err)
		}
	}
	sc.Close()
	if err := eng.Flush("graph"); err != nil {
		t.Fatal(err)
	}
	if err := eng.Compact("graph", nil); err != nil {
		t.Fatal(err)
	}

	mx := eng.Metrics()
	if mx.Writes != 2 {
		t.Errorf("Writes = %d, want 2", mx.Writes)
	}
	if mx.Mutations != 3 {
		t.Errorf("Mutations = %d, want 3", mx.Mutations)
	}
	if mx.CellsWritten != 4 {
		t.Errorf("CellsWritten = %d, want 4", mx.CellsWritten)
	}
	if mx.Scans != 1 {
		t.Errorf("Scans = %d, want 1", mx.Scans)
	}
	if mx.Flushes != 1 {
		t.Errorf("Flushes = %d, want 1", mx.Flushes)
	}
	if mx.Compactions != 1 {
		t.Errorf("Compactions = %d, want 1", mx.Compactions)
	}

	stats := eng.Stats()
	if len(stats) != 2 {
		t.Fatalf("Stats len = %d, want 2", len(stats))
	}
	// Ordered by name: graph, log.
	if stats[0].Name != "graph" || stats[1].Name != "log" {
		t.Fatalf("Stats order: %+v", stats)
	}
	if stats[0].Tablets != 3 {
		t.Errorf("graph tablets = %d, want 3", stats[0].Tablets)
	}
	if stats[1].Tablets != 1 {
		t.Errorf("log tablets = %d, want 1", stats[1].Tablets)
	}
	// After flush+compact the graph table has at least one RFile.
	if stats[0].RFiles < 1 {
		t.Errorf("graph rfiles = %d, want >= 1", stats[0].RFiles)
	}
}
