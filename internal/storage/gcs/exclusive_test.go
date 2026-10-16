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

package gcs

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/storagetest"
	"google.golang.org/api/googleapi"
)

type exclusiveTestBackend struct {
	backend *Backend
	bucket  *fakeBucket
}

func (b *exclusiveTestBackend) Create(ctx context.Context, path string) (storage.Writer, error) {
	return b.backend.Create(ctx, "bucket/"+path)
}

func (b *exclusiveTestBackend) CreateExclusive(ctx context.Context, path string) (storage.Writer, error) {
	return b.backend.CreateExclusive(ctx, "bucket/"+path)
}

func (b *exclusiveTestBackend) Open(_ context.Context, path string) (storage.File, error) {
	object, ok := b.bucket.objects[path]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return &exclusiveTestFile{bytes.NewReader(object.body)}, nil
}

func (b *exclusiveTestBackend) List(_ context.Context, prefix string) ([]string, error) {
	var paths []string
	for path := range b.bucket.objects {
		if prefix == "" || strings.HasPrefix(path, strings.TrimRight(prefix, "/")+"/") {
			paths = append(paths, path)
		}
	}
	return paths, nil
}

type exclusiveTestFile struct{ *bytes.Reader }

func (*exclusiveTestFile) Close() error { return nil }

func TestExclusiveCreator(t *testing.T) {
	storagetest.RunExclusiveCreator(t, func(t *testing.T) storage.ExclusiveCreator {
		backend, bucket := newFakeBackend()
		t.Cleanup(func() {
			for name := range bucket.objects {
				if isTemporaryObjectName(name) {
					t.Errorf("temporary object remains: %s", name)
				}
			}
		})
		return &exclusiveTestBackend{backend: backend, bucket: bucket}
	})
}

func TestExclusiveCreatorConflictCleansStageWhenVerificationFails(t *testing.T) {
	b, bucket := newFakeBackend()
	w, err := b.CreateExclusive(context.Background(), "bucket/dir/target")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("loser")); err != nil {
		t.Fatal(err)
	}
	bucket.copyHook = func(context.Context, *fakeObject, *fakeObject) error {
		return &googleapi.Error{Code: http.StatusPreconditionFailed}
	}
	bucket.attrsErrs["dir/target"] = []error{context.DeadlineExceeded}
	if err := w.Close(); !errors.Is(err, storage.ErrExists) {
		t.Fatalf("Close = %v, want ErrExists", err)
	}
	if len(bucket.objects) != 0 {
		t.Fatalf("objects left after conflict: %v", bucket.objects)
	}
}
