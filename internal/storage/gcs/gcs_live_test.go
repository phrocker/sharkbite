//go:build integration

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

package gcs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/google/uuid"
	"google.golang.org/api/iterator"

	shstorage "github.com/phrocker/shoal-oss/internal/storage"
)

func TestLiveObjectStore(t *testing.T) {
	if os.Getenv("SHOAL_GCS_INTEGRATION") != "1" {
		t.Skip("set SHOAL_GCS_INTEGRATION=1 to run live GCS tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	backend, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Error(err)
		}
	})
	bucketName := "shoal-integration-" + uuid.NewString()
	bucket := backend.client.Bucket(bucketName)
	if err := bucket.Create(ctx, "shoal-emulator", nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		objects := bucket.Objects(ctx, nil)
		for {
			object, err := objects.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				t.Error(err)
				return
			}
			if err := bucket.Object(object.Name).Delete(ctx); err != nil {
				t.Error(err)
			}
		}
		if err := bucket.Delete(ctx); err != nil {
			t.Error(err)
		}
	})
	root := "gs://" + bucketName + "/"
	prefix := root + "live/"
	objectPath := prefix + "roundtrip"
	want := []byte("shoal live GCS roundtrip")
	create := func(path string) {
		t.Helper()
		w, err := backend.Create(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		defer w.(shstorage.Aborter).Abort()
		if _, err := w.Write(want); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	create(objectPath)
	create(root + "outside")
	f, err := backend.Open(ctx, objectPath)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(want))
	_, readErr := f.ReadAt(got, 0)
	closeErr := f.Close()
	if readErr != nil || closeErr != nil || f.Size() != int64(len(want)) || !bytes.Equal(got, want) {
		t.Fatalf("round trip = %q, read=%v close=%v size=%d", got, readErr, closeErr, f.Size())
	}
	entries, err := backend.List(ctx, prefix)
	if err != nil || !slices.Equal(entries, []string{objectPath}) {
		t.Fatalf("List = %v, %v", entries, err)
	}
	if err := backend.Remove(ctx, objectPath); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Open(ctx, objectPath); !errors.Is(err, shstorage.ErrNotFound) {
		t.Fatalf("Open after Remove = %v, want ErrNotFound", err)
	}
	w, err := backend.Create(ctx, prefix+"aborted")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(want); err != nil {
		t.Fatal(err)
	}
	// Write only buffers inside the SDK writer; the temp object is first
	// created by Close. Create it directly under this writer's identity so
	// Abort has something real to remove, then prove it is gone.
	gw := w.(*writer)
	tempName := strings.TrimPrefix(gw.tempPath, "gs://"+bucketName+"/")
	tempWriter := bucket.Object(tempName).NewWriter(ctx)
	tempWriter.Metadata = map[string]string{"shoal-write-id": gw.writeID}
	if _, err := tempWriter.Write(want); err != nil {
		t.Fatal(err)
	}
	if err := tempWriter.Close(); err != nil {
		t.Fatal(err)
	}
	tempAttrs, err := bucket.Object(tempName).Attrs(ctx)
	if err != nil {
		t.Fatalf("temp missing before Abort: %v", err)
	}
	gw.tempGeneration = tempAttrs.Generation
	gw.tempAttrs = tempAttrs
	if err := w.(shstorage.Aborter).Abort(); err != nil {
		t.Fatal(err)
	}
	if _, err := bucket.Object(tempName).Attrs(ctx); err == nil {
		t.Fatalf("temp %s survived Abort", tempName)
	}
	// Backend.List hides stages; inspect the server's raw namespace instead.
	if object, err := bucket.Objects(ctx, &storage.Query{Prefix: "live/"}).Next(); err != iterator.Done {
		t.Fatalf("object remains after Remove and Abort: %v, %v", object, err)
	}
	stage, err := nextTemporaryObjectName("live/fresh")
	if err != nil {
		t.Fatal(err)
	}
	stageWriter := bucket.Object(stage).NewWriter(ctx)
	stageWriter.Metadata = map[string]string{"shoal-write-id": uuid.NewString()}
	if _, err := stageWriter.Write(want); err != nil {
		t.Fatal(err)
	}
	if err := stageWriter.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := backend.CleanupStaleArtifacts(ctx, prefix, time.Now().Add(-time.Hour))
	if err != nil || result.Examined != 1 || len(result.Removed) != 0 {
		t.Fatalf("CleanupStaleArtifacts = %+v, %v", result, err)
	}
	artifacts, err := backend.artifactOps.list(ctx, bucketName, "live/")
	if err != nil {
		t.Fatal(err)
	}
	ownedSeen := false
	for _, a := range artifacts {
		if a.name == stage {
			ownedSeen = a.owned
		}
	}
	if !ownedSeen {
		t.Fatalf("fresh stage %s not recognized as owned by artifact listing: %+v", stage, artifacts)
	}
	if _, err := bucket.Object(stage).Attrs(ctx); err != nil {
		t.Fatalf("fresh stage removed: %v", err)
	}
	outside, err := backend.Open(ctx, root+"outside")
	if err != nil {
		t.Fatalf("outside object removed: %v", err)
	}
	if err := outside.Close(); err != nil {
		t.Fatal(err)
	}
}
