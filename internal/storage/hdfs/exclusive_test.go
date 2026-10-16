// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

package hdfs

import (
	"context"
	"errors"
	"io/fs"
	"path"
	"sync"
	"testing"

	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/storagetest"
)

type exclusiveTestClient struct {
	*fakeClient
	mu         sync.Mutex
	publishErr error
}

func (c *exclusiveTestClient) RenameNoOverwriteContext(ctx context.Context, src, dst string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.publishErr != nil {
		return c.publishErr
	}
	if c.dirs[dst] {
		return fs.ErrExist
	}
	return c.fakeClient.Rename(src, dst)
}

func (c *exclusiveTestClient) Create(name string) (storage.Writer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	writer, err := c.fakeClient.Create(name)
	if err != nil {
		return nil, err
	}
	return &exclusiveTestWriter{Writer: writer, mu: &c.mu}, nil
}

func (c *exclusiveTestClient) RemoveContext(ctx context.Context, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fakeClient.RemoveContext(ctx, name)
}

type exclusiveTestWriter struct {
	storage.Writer
	mu *sync.Mutex
}

func (w *exclusiveTestWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.Writer.Close()
}

func TestCreateExclusiveConformance(t *testing.T) {
	storagetest.RunExclusiveCreator(t, func(t *testing.T) storage.ExclusiveCreator {
		client := &exclusiveTestClient{fakeClient: newFakeClient()}
		backend, err := New("nn:8020", WithClient(client))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = backend.Close() })
		t.Cleanup(func() {
			for name := range client.files {
				if isReplacementArtifactName(path.Base(name)) {
					t.Errorf("staging artifact remains: %s", name)
				}
			}
		})
		return backend
	}, "/tables")
}

func TestCreateExclusivePublishesOnlyOnClose(t *testing.T) {
	client := &exclusiveTestClient{fakeClient: newFakeClient()}
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	writers := make([]storage.Writer, 2)
	for i := range writers {
		writers[i], err = backend.CreateExclusive(context.Background(), "/tables/target")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writers[i].Write([]byte{byte('a' + i)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, exists := client.files["/tables/target"]; exists {
		t.Fatal("destination visible before Close")
	}
	if err := writers[0].Close(); err != nil {
		t.Fatal(err)
	}
	if err := writers[0].Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := writers[1].Close(); !errors.Is(err, storage.ErrExists) {
		t.Fatalf("losing Close = %v, want ErrExists", err)
	}
	if err := writers[1].(storage.Aborter).Abort(); err != nil {
		t.Fatalf("Abort losing writer: %v", err)
	}
	if got := string(client.files["/tables/target"]); got != "a" {
		t.Fatalf("destination = %q, want a", got)
	}
	if len(client.files) != 1 || len(client.renameCalls) != 2 {
		t.Fatalf("files = %v, renames = %v; want only target and two publish attempts", client.files, client.renameCalls)
	}
}

func TestCreateExclusivePreservesExistingDestination(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(map[bool]string{false: "file", true: "directory"}[directory], func(t *testing.T) {
			client := &exclusiveTestClient{fakeClient: newFakeClient()}
			if directory {
				client.dirs["/tables/target"] = true
			} else {
				client.files["/tables/target"] = []byte("same bytes")
			}
			backend, err := New("nn:8020", WithClient(client))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = backend.Close() })
			writer, err := backend.CreateExclusive(context.Background(), "/tables/target")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := writer.Write([]byte("same bytes")); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); !errors.Is(err, storage.ErrExists) {
				t.Fatalf("Close = %v, want ErrExists even for identical contents", err)
			}
			if _, exists := client.files[client.lastCreatePath]; exists {
				t.Fatal("temporary file remains after failed publish")
			}
			for _, removed := range client.removeCalls {
				if removed == "/tables/target" {
					t.Fatal("removed existing destination")
				}
			}
			if directory && !client.dirs["/tables/target"] {
				t.Fatal("removed existing directory")
			}
			if !directory && string(client.files["/tables/target"]) != "same bytes" {
				t.Fatal("changed existing destination")
			}
		})
	}
}

func TestCreateExclusivePublishFailureAndAbort(t *testing.T) {
	for _, abort := range []bool{false, true} {
		t.Run(map[bool]string{false: "publish failure", true: "abort"}[abort], func(t *testing.T) {
			publishErr := errors.New("publish failure")
			client := &exclusiveTestClient{fakeClient: newFakeClient(), publishErr: publishErr}
			backend, err := New("nn:8020", WithClient(client))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = backend.Close() })
			writer, err := backend.CreateExclusive(context.Background(), "/tables/target")
			if err != nil {
				t.Fatal(err)
			}
			if abort {
				if err := writer.(storage.Aborter).Abort(); err != nil {
					t.Fatal(err)
				}
			} else if err := writer.Close(); !errors.Is(err, publishErr) {
				t.Fatalf("Close = %v, want publish failure", err)
			}
			if len(client.files) != 0 {
				t.Fatalf("files remain: %v", client.files)
			}
		})
	}
}

func TestCreateExclusiveRequiresAtomicClientPrimitive(t *testing.T) {
	client := newFakeClient()
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	if _, err := backend.CreateExclusive(context.Background(), "/tables/target"); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("CreateExclusive = %v, want unsupported", err)
	}
	if client.lastCreatePath != "" || len(client.renameCalls) != 0 {
		t.Fatal("unsupported client created or renamed files")
	}
}

var _ storage.ExclusiveCreator = (*Backend)(nil)
