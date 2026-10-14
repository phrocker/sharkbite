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

package localwal

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/cclient"
)

func TestWAL_AppendReplay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.wal")

	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	// Write two mutations
	m1, _ := cclient.NewMutation([]byte("row1"))
	m1.Put([]byte("cf"), []byte("cq1"), nil, 100, []byte("val1"))

	m2, _ := cclient.NewMutation([]byte("row2"))
	m2.Put([]byte("cf"), []byte("cq2"), []byte("vis"), 200, []byte("val2"))
	m2.Delete([]byte("cf"), []byte("cq3"), nil, 300)

	if _, err := w.Append([]*cclient.Mutation{m1, m2}); err != nil {
		t.Fatal(err)
	}
	w.Close()

	// Reopen and replay
	w2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()

	var replayed []*cclient.Mutation
	count, err := w2.Replay(func(m *cclient.Mutation) error {
		replayed = append(replayed, m)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("expected 2 mutations, got %d", count)
	}
	if string(replayed[0].Row()) != "row1" {
		t.Errorf("mutation 0 row = %q, want row1", replayed[0].Row())
	}
	if replayed[0].Size() != 1 {
		t.Errorf("mutation 0 size = %d, want 1", replayed[0].Size())
	}
	if string(replayed[1].Row()) != "row2" {
		t.Errorf("mutation 1 row = %q, want row2", replayed[1].Row())
	}
	if replayed[1].Size() != 2 {
		t.Errorf("mutation 1 size = %d, want 2", replayed[1].Size())
	}
}

// TestWAL_SyncModes verifies that under the deferred-fsync durability
// tiers (SyncNormal with and without a group-commit interval, and
// SyncOff) every appended mutation is still written and replays
// correctly. The fsync timing changes; the on-disk bytes do not.
func TestWAL_SyncModes(t *testing.T) {
	cases := []struct {
		name string
		opts []Option
	}{
		{name: "normal", opts: []Option{WithSyncMode(SyncNormal)}},
		{name: "normal_interval", opts: []Option{WithSyncMode(SyncNormal), WithSyncInterval(2 * time.Millisecond)}},
		{name: "off", opts: []Option{WithSyncMode(SyncOff)}},
	}
	const appends = 5
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "test.wal")

			w, err := Open(path, tc.opts...)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < appends; i++ {
				m, _ := cclient.NewMutation([]byte{byte('a' + i)})
				m.Put([]byte("cf"), []byte("cq"), nil, int64(i), []byte("v"))
				if _, err := w.Append([]*cclient.Mutation{m}); err != nil {
					t.Fatal(err)
				}
			}
			// Close flushes any deferred bytes and stops the ticker.
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}

			w2, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer w2.Close()
			count, err := w2.Replay(func(*cclient.Mutation) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			if count != appends {
				t.Fatalf("replayed %d mutations, want %d", count, appends)
			}
		})
	}
}

func TestWAL_Truncate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.wal")

	w, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	m, _ := cclient.NewMutation([]byte("row"))
	m.Put([]byte("cf"), []byte("cq"), nil, 1, []byte("v"))
	w.Append([]*cclient.Mutation{m})

	if err := w.Truncate(); err != nil {
		t.Fatal(err)
	}

	info, _ := os.Stat(path)
	if info.Size() != 0 {
		t.Errorf("WAL should be empty after truncate, size = %d", info.Size())
	}
}
