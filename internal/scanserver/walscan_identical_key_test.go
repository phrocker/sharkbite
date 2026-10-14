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

package scanserver

import (
	"context"
	"testing"

	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/qwal"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
)

// TestWALLeafWinsIdenticalKey: on the WAL route, a cell in the WAL memtable
// and an RFile cell with the identical key (same timestamp) resolve to the
// WAL's — the memtable is the newest source and goes first in the merge
// (#692).
func TestWALLeafWinsIdenticalKey(t *testing.T) {
	mem := memory.New()
	const path = "gs://test/a.rf"
	writeRFileToMemory(t, mem, path, []cellSpec{
		{row: "r02", cf: "cf", cq: "cq", value: "rfile-old", ts: 5},
	})
	loc := &stubLocator{tablets: map[string][]metadata.TabletInfo{"1": {{
		TableID: "1", Files: []metadata.FileEntry{{Path: path}},
		Logs: []metadata.LogEntry{{UUID: "seg-A", Path: "/wal/seg-A", WALPath: "/wal/seg-A", Peers: []string{"p:1"}}},
	}}}}
	srv := newWALTestServer(t, loc, mem, map[string][]*qwal.Entry{
		"seg-A": {walPut(1, 1, "r02", "cf", "cq", 5, "wal-new")},
	})
	resp, err := srv.StartScan(context.Background(), nil, nil,
		&data.TKeyExtent{Table: []byte("1")},
		&data.TRange{InfiniteStartKey: true, InfiniteStopKey: true},
		nil, 0, nil, nil, nil, false, false, 0, nil, 0, "",
		map[string]string{RouteHintKey: RouteWAL}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Result_.Results) != 1 || string(resp.Result_.Results[0].Value) != "wal-new" {
		for _, kv := range resp.Result_.Results {
			t.Logf("%s=%s", kv.Key.Row, kv.Value)
		}
		t.Fatalf("want single wal-new")
	}
}
