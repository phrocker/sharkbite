// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

// Package storagetest provides shared backend conformance tests.
package storagetest

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/storage"
)

// RunExclusiveCreator checks exclusive publication and abandoned-write cleanup.
// newBackend should return an isolated backend for each subtest. An optional
// prefix scopes paths for backends that require an absolute directory or URI.
func RunExclusiveCreator(t *testing.T, newBackend func(*testing.T) storage.ExclusiveCreator, prefixes ...string) {
	t.Helper()
	ctx := context.Background()
	prefix := ""
	if len(prefixes) != 0 {
		prefix = strings.TrimRight(prefixes[0], "/") + "/"
	}
	read := func(t *testing.T, b storage.Backend, path, want string) {
		t.Helper()
		f, err := b.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		buf := make([]byte, f.Size())
		if _, err := f.ReadAt(buf, 0); err != nil {
			t.Fatal(err)
		}
		if string(buf) != want {
			t.Fatalf("contents = %q, want %q", buf, want)
		}
	}
	write := func(t *testing.T, w storage.Writer, body string) {
		t.Helper()
		t.Cleanup(func() {
			if a, ok := w.(storage.Aborter); ok {
				_ = a.Abort()
			}
		})
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("absent", func(t *testing.T) {
		b := newBackend(t)
		path := prefix + "exclusive-absent"
		w, err := b.CreateExclusive(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		write(t, w, "created")
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		read(t, b, path, "created")
	})
	for _, body := range []string{"different", "original"} {
		t.Run("existing-"+body, func(t *testing.T) {
			b := newBackend(t)
			path := prefix + "exclusive-existing-" + body
			if err := storage.WriteAll(ctx, b, path, []byte("original")); err != nil {
				t.Fatal(err)
			}
			w, err := b.CreateExclusive(ctx, path)
			if err == nil {
				write(t, w, body)
				err = w.Close()
			}
			if !errors.Is(err, storage.ErrExists) {
				t.Fatalf("exclusive create error = %v, want ErrExists", err)
			}
			read(t, b, path, "original")
		})
	}
	t.Run("competing-writers", func(t *testing.T) {
		b := newBackend(t)
		path := prefix + "exclusive-race"
		writers := make([]storage.Writer, 2)
		bodies := []string{"first", "second"}
		for i := range writers {
			w, err := b.CreateExclusive(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			writers[i] = w
			write(t, w, bodies[i])
		}
		start := make(chan struct{})
		results := make(chan struct {
			index int
			err   error
		}, 2)
		for i, w := range writers {
			go func(i int, w storage.Writer) {
				<-start
				results <- struct {
					index int
					err   error
				}{i, w.Close()}
			}(i, w)
		}
		close(start)
		winner := -1
		for range writers {
			result := <-results
			if result.err == nil {
				if winner != -1 {
					t.Fatal("both exclusive writers succeeded")
				}
				winner = result.index
			} else if !errors.Is(result.err, storage.ErrExists) {
				t.Fatalf("loser error = %v, want ErrExists", result.err)
			}
		}
		if winner == -1 {
			t.Fatal("neither exclusive writer succeeded")
		}
		read(t, b, path, bodies[winner])
	})
	t.Run("abort", func(t *testing.T) {
		b := newBackend(t)
		path := prefix + "exclusive-abort"
		var before []string
		if lister, ok := b.(storage.Lister); ok {
			var err error
			before, err = lister.List(ctx, strings.TrimSuffix(prefix, "/"))
			if err != nil {
				t.Fatal(err)
			}
			slices.Sort(before)
		}
		w, err := b.CreateExclusive(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		write(t, w, "abandoned")
		a, ok := w.(storage.Aborter)
		if !ok {
			t.Fatal("exclusive writer does not support abort")
		}
		if err := a.Abort(); err != nil {
			t.Fatal(err)
		}
		if f, err := b.Open(ctx, path); !errors.Is(err, storage.ErrNotFound) {
			if f != nil {
				_ = f.Close()
			}
			t.Fatalf("open after abort = %v, want ErrNotFound", err)
		}
		if lister, ok := b.(storage.Lister); ok {
			paths, err := lister.List(ctx, strings.TrimSuffix(prefix, "/"))
			if err != nil {
				t.Fatal(err)
			}
			slices.Sort(paths)
			if !slices.Equal(paths, before) {
				t.Fatalf("objects left after abort: %v", paths)
			}
		}
		if cleaner, ok := b.(storage.ArtifactCleaner); ok {
			result, err := cleaner.CleanupStaleArtifacts(ctx, strings.TrimSuffix(prefix, "/"), time.Now().Add(-storage.RecommendedArtifactCleanupAge))
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Removed) != 0 || len(result.Recoverable) != 0 {
				t.Fatalf("staging artifacts left after abort: %+v", result)
			}
		}
	})
}
