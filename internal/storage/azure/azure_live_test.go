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

package azure

import (
	"bytes"
	"context"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/google/uuid"

	shstorage "github.com/phrocker/shoal-oss/internal/storage"
)

func TestLiveObjectStore(t *testing.T) {
	if os.Getenv("SHOAL_AZURE_INTEGRATION") != "1" {
		t.Skip("set SHOAL_AZURE_INTEGRATION=1 to run live Azure tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	backend, err := New(ctx, WithSyncCopyPromotion())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Error(err)
		}
	})
	containerName := "shoal-integration-" + uuid.NewString()
	client := backend.svc.NewContainerClient(containerName)
	if _, err := client.Create(ctx, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := client.Delete(ctx, nil); err != nil {
			t.Error(err)
		}
	})
	root := "az://" + containerName + "/"
	prefix := root + "live/"
	objectPath := prefix + "roundtrip"
	want := []byte("shoal live Azure roundtrip")
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
	if err := w.(shstorage.Aborter).Abort(); err != nil {
		t.Fatal(err)
	}
	// Backend.List hides stages; inspect the server's raw namespace instead.
	pager := client.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{Prefix: to.Ptr("live/")})
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Segment.BlobItems) != 0 {
			t.Fatalf("blobs remain after Remove and Abort: %v", page.Segment.BlobItems)
		}
	}
	stage, err := nextTemporaryStageName("live/fresh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.ops.uploadStage(ctx, containerName, stage, want, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	result, err := backend.CleanupStaleArtifacts(ctx, prefix, time.Now().Add(-time.Hour))
	if err != nil || result.Examined != 1 || len(result.Removed) != 0 {
		t.Fatalf("CleanupStaleArtifacts = %+v, %v", result, err)
	}
	if _, err := client.NewBlobClient(stage).GetProperties(ctx, nil); err != nil {
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
