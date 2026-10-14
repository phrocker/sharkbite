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
	"errors"
	"io"
	"testing"

	"github.com/phrocker/shoal-oss/internal/cache"
	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/metadata"
	"github.com/phrocker/shoal-oss/internal/qwal"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
)

// fakeWALSegment is an in-memory walEntrySource — a slice of WAL entries
// drained one-by-one, then io.EOF. Sealed and ready for the W2 read path
// without a real sidecar.
type fakeWALSegment struct {
	entries []*qwal.Entry
	idx     int
	closed  bool
}

func (f *fakeWALSegment) Next() (*qwal.Entry, error) {
	if f.idx >= len(f.entries) {
		return nil, io.EOF
	}
	e := f.entries[f.idx]
	f.idx++
	return e, nil
}

func (f *fakeWALSegment) Close() error { f.closed = true; return nil }

// walPut builds a single-mutation MUTATION WAL entry with one Put.
func walPut(tabletID int32, seq int64, row, cf, cq string, ts int64, val string) *qwal.Entry {
	m, err := cclient.NewMutation([]byte(row))
	if err != nil {
		panic(err)
	}
	m.Put([]byte(cf), []byte(cq), nil, ts, []byte(val))
	return &qwal.Entry{
		Key:   qwal.LogFileKey{Event: qwal.EventMutation, TabletID: tabletID, Seq: seq},
		Value: qwal.LogFileValue{Mutations: []*cclient.Mutation{m}},
	}
}

// newWALTestServer builds a Server pre-wired with an in-memory walOpener
// that maps each segment UUID to its fixture entries. Avoids spinning up
// a real qwal.Reader / sidecar.
func newWALTestServer(t *testing.T, loc *stubLocator, mem *memory.Backend, fixtures map[string][]*qwal.Entry) *Server {
	t.Helper()
	srv, err := NewServer(Options{
		Locator:    loc,
		BlockCache: cache.NewBlockCache(1 << 20),
		Storage:    mem,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Install the in-memory opener directly. Mirrors the production
	// QWALReader → walSegmentOpener wiring in NewServer.
	srv.walOpener = func(_ context.Context, peers []string, uuid, _ string) (walEntrySource, error) {
		ents, ok := fixtures[uuid]
		if !ok {
			return nil, errors.New("test: unknown WAL segment " + uuid)
		}
		return &fakeWALSegment{entries: ents}, nil
	}
	return srv
}

// TestStartScan_DefaultRouteUnchangedByWALConfig is the regression guard:
// even when a Server is wired with a walOpener AND the tablet has log:
// entries, a scan WITHOUT the WAL-route executionHint must produce the
// exact same result as a server with no WAL support at all. The default
// path is byte-for-byte unchanged.
func TestStartScan_DefaultRouteUnchangedByWALConfig(t *testing.T) {
	mem := memory.New()
	const path = "gs://test/a.rf"
	writeRFileToMemory(t, mem, path, []cellSpec{
		{row: "r01", cf: "cf", cq: "cq", value: "rfile-01", ts: 1},
		{row: "r02", cf: "cf", cq: "cq", value: "rfile-02", ts: 1},
	})
	loc := &stubLocator{
		tablets: map[string][]metadata.TabletInfo{
			"1": {{
				TableID: "1",
				Files:   []metadata.FileEntry{{Path: path}},
				Logs: []metadata.LogEntry{{
					UUID: "seg-A", Path: "/wal/seg-A", WALPath: "/wal/seg-A",
					Peers: []string{"peer-0:9710"},
				}},
			}},
		},
	}
	// WAL fixture would add r03; if the default route accidentally merged
	// the WAL it would surface in the result.
	fixtures := map[string][]*qwal.Entry{
		"seg-A": {walPut(1, 1, "r03", "cf", "cq", 100, "wal-only")},
	}
	srv := newWALTestServer(t, loc, mem, fixtures)

	// Default route: NO executionHints (or one without shoal.route=wal).
	resp, err := srv.StartScan(context.Background(), nil, nil,
		&data.TKeyExtent{Table: []byte("1")},
		&data.TRange{InfiniteStartKey: true, InfiniteStopKey: true},
		nil, 0, nil, nil, nil, false, false, 0, nil, 0, "", nil, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(resp.Result_.Results); got != 2 {
		t.Fatalf("default route returned %d cells, want 2 (RFile only)", got)
	}
	for _, kv := range resp.Result_.Results {
		if string(kv.Key.Row) == "r03" {
			t.Errorf("default route leaked WAL cell r03=%q", kv.Value)
		}
	}
}

// TestStartScan_WALRouteMergesRFileAndWAL drives the opt-in path: the
// scan must surface r01 + r02 from the RFile AND r03 from the WAL, all
// in sorted order, with version collapse applied by the iterrt stack.
func TestStartScan_WALRouteMergesRFileAndWAL(t *testing.T) {
	mem := memory.New()
	const path = "gs://test/a.rf"
	writeRFileToMemory(t, mem, path, []cellSpec{
		{row: "r01", cf: "cf", cq: "cq", value: "rfile-01", ts: 1},
		{row: "r02", cf: "cf", cq: "cq", value: "rfile-old", ts: 1},
	})
	loc := &stubLocator{
		tablets: map[string][]metadata.TabletInfo{
			"1": {{
				TableID: "1",
				Files:   []metadata.FileEntry{{Path: path}},
				Logs: []metadata.LogEntry{{
					UUID: "seg-A", Path: "/wal/seg-A", WALPath: "/wal/seg-A",
					Peers: []string{"peer-0:9710"},
				}},
			}},
		},
	}
	// WAL has a NEWER r02 (ts=5) and a brand-new r03. Versioning iterator
	// (maxVersions=1) must surface the newer r02 and drop the RFile one.
	fixtures := map[string][]*qwal.Entry{
		"seg-A": {
			walPut(1, 1, "r02", "cf", "cq", 5, "wal-new"),
			walPut(1, 2, "r03", "cf", "cq", 7, "wal-only"),
		},
	}
	srv := newWALTestServer(t, loc, mem, fixtures)

	resp, err := srv.StartScan(context.Background(), nil, nil,
		&data.TKeyExtent{Table: []byte("1")},
		&data.TRange{InfiniteStartKey: true, InfiniteStopKey: true},
		nil, 0, nil, nil, nil, false, false, 0, nil, 0, "",
		map[string]string{RouteHintKey: RouteWAL}, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(resp.Result_.Results); got != 3 {
		t.Fatalf("WAL route returned %d cells, want 3 (r01+r02+r03)", got)
	}
	want := map[string]string{
		"r01": "rfile-01",
		"r02": "wal-new", // WAL ts=5 beats RFile ts=1
		"r03": "wal-only",
	}
	for _, kv := range resp.Result_.Results {
		row := string(kv.Key.Row)
		if got := string(kv.Value); got != want[row] {
			t.Errorf("row=%s value=%q, want %q", row, got, want[row])
		}
	}
}

// TestStartScan_WALRouteNoLogsIsFlushedOnly: a tablet with zero log:
// entries serves the same cells the default path would — no fabricated
// memtable leaf, no spurious entries.
func TestStartScan_WALRouteNoLogsIsFlushedOnly(t *testing.T) {
	mem := memory.New()
	const path = "gs://test/a.rf"
	writeRFileToMemory(t, mem, path, []cellSpec{
		{row: "r01", cf: "cf", cq: "cq", value: "v1", ts: 1},
	})
	loc := &stubLocator{
		tablets: map[string][]metadata.TabletInfo{
			"1": {{TableID: "1", Files: []metadata.FileEntry{{Path: path}}}},
		},
	}
	srv := newWALTestServer(t, loc, mem, nil)

	resp, err := srv.StartScan(context.Background(), nil, nil,
		&data.TKeyExtent{Table: []byte("1")},
		&data.TRange{InfiniteStartKey: true, InfiniteStopKey: true},
		nil, 0, nil, nil, nil, false, false, 0, nil, 0, "",
		map[string]string{RouteHintKey: RouteWAL}, 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(resp.Result_.Results); got != 1 {
		t.Fatalf("got %d cells, want 1", got)
	}
}

// TestStartScan_WALRouteRequiresOpener: a Server with no walOpener that
// receives a WAL-route request errors loudly rather than silently
// degrading. Catches a misconfigured deploy (operator forgot -wal-route).
func TestStartScan_WALRouteRequiresOpener(t *testing.T) {
	mem := memory.New()
	const path = "gs://test/a.rf"
	writeRFileToMemory(t, mem, path, []cellSpec{
		{row: "r01", cf: "cf", cq: "cq", value: "v1", ts: 1},
	})
	loc := &stubLocator{
		tablets: map[string][]metadata.TabletInfo{
			"1": {{
				TableID: "1",
				Files:   []metadata.FileEntry{{Path: path}},
				Logs: []metadata.LogEntry{{
					UUID: "seg-A", Path: "/wal/seg-A", WALPath: "/wal/seg-A",
					Peers: []string{"peer-0:9710"},
				}},
			}},
		},
	}
	srv, err := NewServer(Options{Locator: loc, Storage: mem})
	if err != nil {
		t.Fatal(err)
	}
	_, err = srv.StartScan(context.Background(), nil, nil,
		&data.TKeyExtent{Table: []byte("1")},
		&data.TRange{InfiniteStartKey: true, InfiniteStopKey: true},
		nil, 0, nil, nil, nil, false, false, 0, nil, 0, "",
		map[string]string{RouteHintKey: RouteWAL}, 0,
	)
	if err == nil {
		t.Fatal("WAL-route request on a server without walOpener must error")
	}
}
