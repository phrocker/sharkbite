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

//go:build sqlite_bench

// bench_sqlite_test.go is the SQLite half of the comparative benchmark
// suite. It is gated behind the sqlite_bench build tag (and cgo) so the
// default `go test ./...` — which runs with CGO_ENABLED=0 — is never
// affected. Run the comparison with:
//
//	CGO_ENABLED=1 go test -tags sqlite_bench -bench='Shoal|SQLite' \
//	    -benchmem -run='^$' ./internal/engine/
//
// The SQLite schema mirrors shoal's cell model exactly: one row per
// (row, cf, cq) coordinate. Each logical entity expands to the SAME
// three cells shoal's makeMutation writes (props:label = 128B value,
// props:type = "entity", props:salience = "0.85"), under the SAME
// "entity:%08d" key distribution. So both engines store identical data
// and the per-op numbers are directly comparable.
//
// SQLite runs in WAL mode with synchronous=NORMAL — the durability
// profile a production embedded consumer (e.g. better-sqlite3) uses,
// and the closest analogue to shoal's WAL + memtable write path.
package engine_test

import (
	"database/sql"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/engine"
)

// openSQLite creates a fresh on-disk WAL-mode database in a temp dir and
// installs the cell-store schema. The PRAGMAs match a typical embedded
// production config: WAL journaling, NORMAL sync, a generous page cache.
func openSQLite(tb testing.TB) (*sql.DB, func()) {
	tb.Helper()
	dir, err := os.MkdirTemp("", "shoal-sqlite-bench-*")
	if err != nil {
		tb.Fatal(err)
	}
	dsn := filepath.Join(dir, "bench.db") +
		"?_journal_mode=WAL&_synchronous=NORMAL&_busy_timeout=5000"
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		os.RemoveAll(dir)
		tb.Fatal(err)
	}
	// Single connection keeps the WAL writer deterministic for write
	// benchmarks (SQLite serializes writers anyway).
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`
		PRAGMA cache_size = -65536;
		CREATE TABLE cells (
			row TEXT NOT NULL,
			cf  TEXT NOT NULL,
			cq  TEXT NOT NULL,
			ts  INTEGER NOT NULL,
			val BLOB,
			PRIMARY KEY (row, cf, cq)
		) WITHOUT ROWID;
	`); err != nil {
		db.Close()
		os.RemoveAll(dir)
		tb.Fatal(err)
	}
	return db, func() {
		db.Close()
		os.RemoveAll(dir)
	}
}

// entityValue returns the 128-byte label value shoal's makeMutation
// writes (val[i] = byte(i%256)).
func entityValue(valSize int) []byte {
	val := make([]byte, valSize)
	for i := range val {
		val[i] = byte(i % 256)
	}
	return val
}

// insertEntity writes the three cells of one entity through the prepared
// statement — the SQLite analogue of one makeMutation (props:label,
// props:type, props:salience).
func insertEntity(stmt *sql.Stmt, row string, label []byte) error {
	if _, err := stmt.Exec(row, "props", "label", 0, label); err != nil {
		return err
	}
	if _, err := stmt.Exec(row, "props", "type", 0, []byte("entity")); err != nil {
		return err
	}
	if _, err := stmt.Exec(row, "props", "salience", 0, []byte("0.85")); err != nil {
		return err
	}
	return nil
}

const sqliteInsert = `INSERT OR REPLACE INTO cells (row, cf, cq, ts, val) VALUES (?, ?, ?, ?, ?)`

// seedSQLite bulk-loads count entities under prefix in one transaction —
// the SQLite analogue of seedEntries. Used to prime scan benchmarks.
func seedSQLite(tb testing.TB, db *sql.DB, prefix string, count, valSize int) {
	tb.Helper()
	label := entityValue(valSize)
	tx, err := db.Begin()
	if err != nil {
		tb.Fatal(err)
	}
	stmt, err := tx.Prepare(sqliteInsert)
	if err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < count; i++ {
		if err := insertEntity(stmt, fmt.Sprintf("%s%08d", prefix, i), label); err != nil {
			tb.Fatal(err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
}

// seedSQLiteGraph bulk-loads g.nodes nodes (3 prop cells + one edge cell
// per out-neighbor) in one transaction — the SQLite analogue of
// seedGraph, using identical adjacency so the traversal compares equal
// work.
func seedSQLiteGraph(tb testing.TB, db *sql.DB, g graphSpec, valSize int) {
	tb.Helper()
	label := entityValue(valSize)
	tx, err := db.Begin()
	if err != nil {
		tb.Fatal(err)
	}
	stmt, err := tx.Prepare(sqliteInsert)
	if err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < g.nodes; i++ {
		row := fmt.Sprintf("entity:%08d", i)
		if err := insertEntity(stmt, row, label); err != nil {
			tb.Fatal(err)
		}
		for _, t := range g.neighbors(i) {
			if _, err := stmt.Exec(row, "edge", fmt.Sprintf("entity:%08d", t), 0, edgeWeight); err != nil {
				tb.Fatal(err)
			}
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		tb.Fatal(err)
	}
}

// traverseSQLite runs the same hops-deep BFS as traverseShoal, expanding
// each frontier node via a prepared "edges of row" query.
func traverseSQLite(b *testing.B, stmt *sql.Stmt, start, hops int) int {
	b.Helper()
	visited := map[int]bool{start: true}
	frontier := []int{start}
	for h := 0; h < hops && len(frontier) > 0; h++ {
		var next []int
		for _, node := range frontier {
			rows, err := stmt.Query(fmt.Sprintf("entity:%08d", node))
			if err != nil {
				b.Fatal(err)
			}
			for rows.Next() {
				var cq string
				if err := rows.Scan(&cq); err != nil {
					b.Fatal(err)
				}
				if t := entityID([]byte(cq)); t >= 0 && !visited[t] {
					visited[t] = true
					next = append(next, t)
				}
			}
			if err := rows.Err(); err != nil {
				b.Fatal(err)
			}
			rows.Close()
		}
		frontier = next
	}
	return len(visited)
}

// BenchmarkSQLite_Write_SingleTablet commits one entity (3 cells) per op
// in autocommit mode — the durable-write-per-call analogue of shoal's
// per-call WAL append.
func BenchmarkSQLite_Write_SingleTablet(b *testing.B) {
	db, cleanup := openSQLite(b)
	defer cleanup()

	stmt, err := db.Prepare(sqliteInsert)
	if err != nil {
		b.Fatal(err)
	}
	defer stmt.Close()
	label := entityValue(128)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		if err := insertEntity(stmt, fmt.Sprintf("entity:%08d", i), label); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSQLite_WriteBatch commits 100 entities per transaction —
// mirrors BenchmarkShoal_WriteBatch (100-mutation batches).
func BenchmarkSQLite_WriteBatch(b *testing.B) {
	db, cleanup := openSQLite(b)
	defer cleanup()

	const batchSize = 100
	label := entityValue(128)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		tx, err := db.Begin()
		if err != nil {
			b.Fatal(err)
		}
		stmt, err := tx.Prepare(sqliteInsert)
		if err != nil {
			b.Fatal(err)
		}
		for j := 0; j < batchSize; j++ {
			row := fmt.Sprintf("entity:%08d", i*batchSize+j)
			if err := insertEntity(stmt, row, label); err != nil {
				b.Fatal(err)
			}
		}
		stmt.Close()
		if err := tx.Commit(); err != nil {
			b.Fatal(err)
		}
	}
}

// --- Scan benchmarks (mirror BenchmarkShoal_Scan_*) ---

// drainRows iterates a *sql.Rows of (row, cf, cq, ts, val) to EOF,
// returning the cell count. Mirrors shoal's scan-iterate loop.
func drainRows(b *testing.B, rows *sql.Rows) int {
	b.Helper()
	count := 0
	for rows.Next() {
		var r, cf, cq string
		var ts int64
		var val []byte
		if err := rows.Scan(&r, &cf, &cq, &ts, &val); err != nil {
			b.Fatal(err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		b.Fatal(err)
	}
	rows.Close()
	return count
}

// BenchmarkSQLite_Scan_FullTable scans every cell after seeding 10K
// entities (30K cells) — mirrors BenchmarkShoal_Scan_FullTable.
func BenchmarkSQLite_Scan_FullTable(b *testing.B) {
	db, cleanup := openSQLite(b)
	defer cleanup()

	seedSQLite(b, db, "entity:", 10_000, 128)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		rows, err := db.Query(`SELECT row, cf, cq, ts, val FROM cells ORDER BY row, cf, cq`)
		if err != nil {
			b.Fatal(err)
		}
		if n := drainRows(b, rows); n == 0 {
			b.Fatal("scan returned 0 results")
		}
	}
}

// BenchmarkSQLite_Scan_PrefixRange scans the entity: prefix out of a
// table also holding event:/knowledge: rows — mirrors
// BenchmarkShoal_Scan_PrefixRange.
func BenchmarkSQLite_Scan_PrefixRange(b *testing.B) {
	db, cleanup := openSQLite(b)
	defer cleanup()

	for _, p := range []string{"entity:", "event:", "knowledge:"} {
		seedSQLite(b, db, p, 3_000, 128)
	}
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		// [entity:, entity;) — ':'+1 is ';', the same half-open bound
		// shoal uses for its prefix range.
		rows, err := db.Query(
			`SELECT row, cf, cq, ts, val FROM cells WHERE row >= ? AND row < ? ORDER BY row, cf, cq`,
			"entity:", "entity;",
		)
		if err != nil {
			b.Fatal(err)
		}
		drainRows(b, rows)
	}
}

// BenchmarkSQLite_Graph_KHopTraversal mirrors
// BenchmarkShoal_Graph_KHopTraversal: identical random graph, graphHops
// BFS from a random seed per op, neighbors fetched via an indexed
// "edges of row" query.
func BenchmarkSQLite_Graph_KHopTraversal(b *testing.B) {
	db, cleanup := openSQLite(b)
	defer cleanup()

	g := graphSpec{nodes: 10_000, degree: 8, seed: 1}
	seedSQLiteGraph(b, db, g, 128)

	stmt, err := db.Prepare(`SELECT cq FROM cells WHERE row = ? AND cf = 'edge'`)
	if err != nil {
		b.Fatal(err)
	}
	defer stmt.Close()

	rng := rand.New(rand.NewSource(42))
	b.ResetTimer()

	var totalVisited int
	for i := 0; i < b.N; i++ {
		totalVisited += traverseSQLite(b, stmt, rng.Intn(g.nodes), graphHops)
	}
	b.StopTimer()
	if totalVisited == 0 {
		b.Fatal("traversal visited no nodes")
	}
}

// TestFileSize_ShoalVsSQLite seeds the SAME realistic, text-shaped
// dataset (10k entities × 3 cells, 128-byte natural-language label values)
// into a real shoal engine and into SQLite, then reports the on-disk
// footprint of each. Shoal flushes snappy-compressed RFiles; SQLite stores
// the cells uncompressed in a WITHOUT ROWID B-tree. Run with:
//
//	CGO_ENABLED=1 go test -tags sqlite_bench -run TestFileSize_ShoalVsSQLite -v ./internal/engine/
func TestFileSize_ShoalVsSQLite(t *testing.T) {
	const rows = 10_000

	// Generate the label values once so both engines store identical bytes.
	rng := rand.New(rand.NewSource(1))
	labels := make([][]byte, rows)
	for i := range labels {
		labels[i] = realisticLabel(rng, 128)
	}

	// --- shoal ---
	shoalDir, err := os.MkdirTemp("", "shoal-size-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(shoalDir)
	eng, err := engine.Open(shoalDir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.CreateTable("bench", engine.TableOptions{}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < rows; i += 500 {
		end := i + 500
		if end > rows {
			end = rows
		}
		batch := make([]*cclient.Mutation, 0, end-i)
		for j := i; j < end; j++ {
			m, _ := cclient.NewMutation([]byte(fmt.Sprintf("entity:%08d", j)))
			m.Put([]byte("props"), []byte("label"), nil, cclient.MutationLatestTimestamp, labels[j])
			m.Put([]byte("props"), []byte("type"), nil, cclient.MutationLatestTimestamp, []byte("entity"))
			m.Put([]byte("props"), []byte("salience"), nil, cclient.MutationLatestTimestamp, []byte("0.85"))
			batch = append(batch, m)
		}
		if err := eng.Write("bench", batch); err != nil {
			t.Fatal(err)
		}
	}
	if err := eng.Flush("bench"); err != nil {
		t.Fatal(err)
	}
	shoalBytes := dirRFileBytes(t, shoalDir)
	eng.Close()

	// --- SQLite ---
	sqlDir, err := os.MkdirTemp("", "shoal-sqlite-size-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(sqlDir)
	dbPath := filepath.Join(sqlDir, "size.db")
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL&_synchronous=NORMAL")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE TABLE cells (row TEXT NOT NULL, cf TEXT NOT NULL, cq TEXT NOT NULL, ts INTEGER NOT NULL, val BLOB, PRIMARY KEY (row, cf, cq)) WITHOUT ROWID;`); err != nil {
		t.Fatal(err)
	}
	tx, _ := db.Begin()
	stmt, _ := tx.Prepare(sqliteInsert)
	for i := 0; i < rows; i++ {
		row := fmt.Sprintf("entity:%08d", i)
		if _, err := stmt.Exec(row, "props", "label", 0, labels[i]); err != nil {
			t.Fatal(err)
		}
		stmt.Exec(row, "props", "type", 0, []byte("entity"))
		stmt.Exec(row, "props", "salience", 0, []byte("0.85"))
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Checkpoint the WAL back into the main db and compact so the on-disk
	// size reflects the settled database, not transient WAL pages.
	db.Exec(`PRAGMA wal_checkpoint(TRUNCATE);`)
	db.Exec(`VACUUM;`)
	db.Close()

	var sqlBytes int64
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if fi, err := os.Stat(dbPath + suffix); err == nil {
			sqlBytes += fi.Size()
		}
	}

	cells := rows * 3
	t.Logf("On-disk size for %d entities (%d cells, realistic 128-byte text values):", rows, cells)
	t.Logf("  shoal RFile (snappy) %9d bytes  %6.2f bytes/cell", shoalBytes, float64(shoalBytes)/float64(cells))
	t.Logf("  SQLite  (.db+wal)    %9d bytes  %6.2f bytes/cell", sqlBytes, float64(sqlBytes)/float64(cells))
	t.Logf("  shoal is %.2fx the size of SQLite", float64(shoalBytes)/float64(sqlBytes))
}
func BenchmarkSQLite_Scan_PointLookup(b *testing.B) {
	db, cleanup := openSQLite(b)
	defer cleanup()

	seedSQLite(b, db, "entity:", 10_000, 128)

	stmt, err := db.Prepare(`SELECT row, cf, cq, ts, val FROM cells WHERE row = ? ORDER BY cf, cq`)
	if err != nil {
		b.Fatal(err)
	}
	defer stmt.Close()

	rng := rand.New(rand.NewSource(42))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		row := fmt.Sprintf("entity:%08d", rng.Intn(10_000))
		rows, err := stmt.Query(row)
		if err != nil {
			b.Fatal(err)
		}
		drainRows(b, rows)
	}
}
