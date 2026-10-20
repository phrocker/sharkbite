// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hdfs

import (
	"context"
	"errors"
	"testing"

	"github.com/phrocker/shoal-oss/internal/storage"
)

type immutableTestClient struct{ *fakeClient }

func (c *immutableTestClient) CreateNoOverwrite(name string) (storage.Writer, error) {
	return c.Create(name)
}

func TestPublishUnreferencedReadbackAndNoOverwrite(t *testing.T) {
	client := &immutableTestClient{fakeClient: newFakeClient()}
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	const target = "/accumulo/wal/shoal+9997/00000000-0000-4000-8000-000000000001"
	if err := backend.PublishUnreferenced(context.Background(), target, []byte("native wal")); err != nil {
		t.Fatal(err)
	}
	if got := string(client.files[target]); got != "native wal" {
		t.Fatalf("published bytes = %q", got)
	}
	if err := backend.PublishUnreferenced(context.Background(), target, []byte("replacement")); !errors.Is(err, storage.ErrExists) {
		t.Fatalf("duplicate publish = %v, want ErrExists", err)
	}
	if got := string(client.files[target]); got != "native wal" {
		t.Fatalf("duplicate changed published bytes to %q", got)
	}
}

func TestPublishUnreferencedRefusesUnprovenClient(t *testing.T) {
	client := newFakeClient()
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	if err := backend.PublishUnreferenced(context.Background(), "/accumulo/wal/new", []byte("wal")); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("publish = %v, want unsupported", err)
	}
	if client.lastCreatePath != "" {
		t.Fatal("unproven client wrote before refusing")
	}
}

func TestPublishUnreferencedCloseFailureNeverReportsSuccess(t *testing.T) {
	client := &immutableTestClient{fakeClient: newFakeClient()}
	client.failWriterClose = true
	backend, err := New("nn:8020", WithClient(client))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	if err := backend.PublishUnreferenced(context.Background(), "/accumulo/wal/new", []byte("wal")); err == nil {
		t.Fatal("failed close reported an immutable publication")
	}
}
