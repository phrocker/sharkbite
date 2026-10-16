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

package s3

import (
	"bytes"
	"context"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	s3sdk "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"

	shstorage "github.com/phrocker/shoal-oss/internal/storage"
)

func TestLiveObjectStore(t *testing.T) {
	if os.Getenv("SHOAL_S3_INTEGRATION") != "1" {
		t.Skip("set SHOAL_S3_INTEGRATION=1 to run live S3 tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	backend, err := New(ctx, WithClientOptions(func(o *s3sdk.Options) {
		o.UsePathStyle = os.Getenv("SHOAL_S3_PATH_STYLE") == "1"
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Error(err)
		}
	})
	bucket := "shoal-integration-" + uuid.NewString()
	if _, err := backend.client.CreateBucket(ctx, &s3sdk.CreateBucketInput{Bucket: aws.String(bucket)}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		objects, err := backend.client.ListObjectsV2(ctx, &s3sdk.ListObjectsV2Input{Bucket: aws.String(bucket)})
		if err != nil {
			t.Error(err)
			return
		}
		for _, object := range objects.Contents {
			if _, err := backend.client.DeleteObject(ctx, &s3sdk.DeleteObjectInput{Bucket: aws.String(bucket), Key: object.Key}); err != nil {
				t.Error(err)
			}
		}
		if _, err := backend.client.DeleteBucket(ctx, &s3sdk.DeleteBucketInput{Bucket: aws.String(bucket)}); err != nil {
			t.Error(err)
		}
	})
	root := "s3://" + bucket + "/"
	prefix := root + "live/"
	objectPath := prefix + "roundtrip"
	want := []byte("shoal live S3 roundtrip")
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
	objects, err := backend.client.ListObjectsV2(ctx, &s3sdk.ListObjectsV2Input{
		Bucket: aws.String(bucket), Prefix: aws.String("live/"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(objects.Contents) != 0 {
		t.Fatalf("objects remain after Remove and Abort: %v", objects.Contents)
	}
	stage, err := nextTemporaryStageKey("live/fresh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.ops.putStage(ctx, bucket, stage, want, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	result, err := backend.CleanupStaleArtifacts(ctx, prefix, time.Now().Add(-time.Hour))
	if err != nil || result.Examined != 1 || len(result.Removed) != 0 {
		t.Fatalf("CleanupStaleArtifacts = %+v, %v", result, err)
	}
	if _, err := backend.client.HeadObject(ctx, &s3sdk.HeadObjectInput{Bucket: aws.String(bucket), Key: aws.String(stage)}); err != nil {
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
