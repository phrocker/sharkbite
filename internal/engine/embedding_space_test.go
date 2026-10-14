// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.
package engine

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/embeddingspace"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/internal/tablet"
)

func TestTableTargetEmbeddingSpacePersists(t *testing.T) {
	dir := t.TempDir()
	eng, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.CreateTable("graph", TableOptions{TargetEmbeddingSpace: "space-a"}); err != nil {
		t.Fatal(err)
	}
	got, err := eng.TableTargetEmbeddingSpace("graph")
	if err != nil {
		t.Fatal(err)
	}
	if got != "space-a" {
		t.Fatalf("target = %q", got)
	}
	if err := eng.SetTableTargetEmbeddingSpace("graph", "space-b"); err != nil {
		t.Fatal(err)
	}
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err = reopened.TableTargetEmbeddingSpace("graph")
	if err != nil {
		t.Fatal(err)
	}
	if got != "space-b" {
		t.Fatalf("reopened target = %q", got)
	}
}

func TestTableDefaultEmbeddingPersistsPerFile(t *testing.T) {
	for _, format := range []tablet.FileFormat{
		tablet.FormatRFile,
		tablet.FormatParquet,
	} {
		t.Run(string(format), func(t *testing.T) {
			dir := t.TempDir()
			eng, err := Open(dir, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if err := eng.CreateTable("graph", TableOptions{
				TabletOptions:    tablet.Options{FileFormat: format},
				DefaultEmbedding: embeddingspace.Has("space-a"),
			}); err != nil {
				t.Fatal(err)
			}
			write := func(row string) {
				mutation, _ := cclient.NewMutation([]byte(row))
				mutation.PutLatest([]byte("vec"), nil, nil, []byte("value"))
				if err := eng.Write("graph", []*cclient.Mutation{mutation}); err != nil {
					t.Fatal(err)
				}
				if err := eng.Flush("graph"); err != nil {
					t.Fatal(err)
				}
			}
			write("a")
			if err := eng.SetTableDefaultEmbedding(
				"graph", embeddingspace.Has("space-b")); err != nil {
				t.Fatal(err)
			}
			write("b")
			if err := eng.Close(); err != nil {
				t.Fatal(err)
			}

			reopened, err := Open(dir, Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			snapshot, err := reopened.TableEmbeddingStateSnapshot(
				context.Background(), "graph")
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.UnflushedCells != 0 || len(snapshot.Files) != 2 {
				t.Fatalf("snapshot = %+v", snapshot)
			}
			got := map[string]int{}
			for _, file := range snapshot.Files {
				got[file.State.String()]++
			}
			if got[embeddingspace.Has("space-a").String()] != 1 ||
				got[embeddingspace.Has("space-b").String()] != 1 ||
				len(got) != 2 {
				t.Fatalf("states = %v", got)
			}
			if err := reopened.Compact("graph", nil); err != nil {
				t.Fatalf("compact mixed embedding spaces: %v", err)
			}
			snapshot, err = reopened.TableEmbeddingStateSnapshot(
				context.Background(), "graph")
			if err != nil {
				t.Fatal(err)
			}
			got = map[string]int{}
			for _, file := range snapshot.Files {
				got[file.State.String()]++
			}
			if len(snapshot.Files) != 2 ||
				got[embeddingspace.Has("space-a").String()] != 1 ||
				got[embeddingspace.Has("space-b").String()] != 1 {
				t.Fatalf("compacted mixed states = %+v", snapshot)
			}
			if err := reopened.Compact("graph", []iterrt.IterSpec{{
				Name: iterrt.IterGraphRank,
			}}); !errors.Is(err, tablet.ErrMixedEmbeddingCompactionStack) {
				t.Fatalf("mixed-space global compaction error = %v", err)
			}
		})
	}
}

func TestValidateExactVectorSpaceFailsClosed(t *testing.T) {
	eng, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if err := eng.CreateTable("graph", TableOptions{
		DefaultEmbedding: embeddingspace.Has("space-a"),
	}); err != nil {
		t.Fatal(err)
	}
	mutation, _ := cclient.NewMutation([]byte("a"))
	mutation.PutLatest([]byte("vec"), nil, nil, []byte("value"))
	if err := eng.Write("graph", []*cclient.Mutation{mutation}); err != nil {
		t.Fatal(err)
	}
	if err := eng.SetTableDefaultEmbedding(
		"graph", embeddingspace.Has("space-b"),
	); !errors.Is(err, ErrEmbeddingStateChangeWithUnflushedData) {
		t.Fatalf(
			"default change error = %v, want ErrEmbeddingStateChangeWithUnflushedData",
			err)
	}
	if err := eng.ValidateExactVectorSpace(
		context.Background(), "graph", "space-a",
	); err != nil {
		t.Fatalf("known unflushed state rejected: %v", err)
	}
	if err := eng.Flush("graph"); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.ScanHosted(
		"graph", iterrt.InfiniteRange(), ScanOptions{},
		[]iterrt.IterSpec{{
			Name: iterrt.IterVectorKNN,
			Options: map[string]string{
				iterrt.VectorKNNQuery: base64.StdEncoding.EncodeToString(
					[]byte{0, 0, 0, 0}),
			},
		}},
	); !errors.Is(err, embeddingspace.ErrQueryIdentityRequired) {
		t.Fatalf("missing query identity error = %v", err)
	}
	if err := eng.ValidateExactVectorSpace(
		context.Background(), "graph", "space-b",
	); !errors.Is(err, embeddingspace.ErrMismatch) {
		t.Fatalf("mismatch error = %v, want ErrMismatch", err)
	}
	if _, err := eng.Scan(
		"graph", iterrt.InfiniteRange(), ScanOptions{
			Stack: []iterrt.IterSpec{{
				Name: iterrt.IterVectorKNN,
				Options: map[string]string{
					iterrt.VectorKNNQuery: base64.StdEncoding.EncodeToString(
						[]byte{0, 0, 0, 0}),
					iterrt.VectorKNNEmbeddingSpace: "space-b",
				},
			}},
		},
	); !errors.Is(err, embeddingspace.ErrQueryMetadataMissing) {
		t.Fatalf(
			"direct scan error = %v, want ErrQueryMetadataMissing",
			err)
	}
}

func TestOrdinaryWritesShareEmbeddingStateGate(t *testing.T) {
	eng, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if err := eng.CreateTable("graph", TableOptions{
		DefaultEmbedding: embeddingspace.Has("space-a"),
	}); err != nil {
		t.Fatal(err)
	}
	eng.mu.RLock()
	tbl := eng.tables["graph"]
	eng.mu.RUnlock()
	tbl.formatMu.RLock()
	mutation, _ := cclient.NewMutation([]byte("a"))
	mutation.PutLatest([]byte("vec"), nil, nil, []byte("value"))
	done := make(chan error, 1)
	go func() {
		done <- eng.Write("graph", []*cclient.Mutation{mutation})
	}()
	select {
	case err := <-done:
		tbl.formatMu.RUnlock()
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		tbl.formatMu.RUnlock()
		t.Fatal("ordinary write waited for a peer read lock")
	}
}

func TestEmbeddingStateSnapshotExcludesWrites(t *testing.T) {
	eng, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if err := eng.CreateTable("graph", TableOptions{
		DefaultEmbedding: embeddingspace.Has("space-a"),
	}); err != nil {
		t.Fatal(err)
	}
	eng.mu.RLock()
	tbl := eng.tables["graph"]
	eng.mu.RUnlock()
	tbl.formatMu.RLock()
	done := make(chan error, 1)
	go func() {
		_, err := eng.TableEmbeddingStateSnapshot(
			context.Background(), "graph")
		done <- err
	}()
	select {
	case err := <-done:
		tbl.formatMu.RUnlock()
		t.Fatalf("snapshot did not exclude a writer: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	tbl.formatMu.RUnlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestTableDefaultEmbeddingRejectsPartialState(t *testing.T) {
	eng, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	partial := embeddingspace.FileState{Identity: "space-a"}
	if err := eng.CreateTable("partial", TableOptions{
		DefaultEmbedding: partial,
	}); !errors.Is(err, embeddingspace.ErrInvalidState) {
		t.Fatalf("CreateTable partial state error = %v", err)
	}
	if err := eng.CreateTable("valid", TableOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := eng.SetTableDefaultEmbedding(
		"valid", partial,
	); !errors.Is(err, embeddingspace.ErrInvalidState) {
		t.Fatalf("SetTableDefaultEmbedding partial state error = %v", err)
	}
}
