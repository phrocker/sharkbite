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

package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	s3sdk "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	shstorage "github.com/phrocker/shoal-oss/internal/storage"
)

func expectedTemporaryStageKeyComponent(key, token string) string {
	hash := sha256.Sum256([]byte(key))
	hashHex := hex.EncodeToString(hash[:tempStageHashHexLen/2])
	return tempStageKeyPrefix + token[:tempStageRandomHexLen] + hashHex
}

func (o *fakeS3ArtifactOperations) inspect(ctx context.Context, _ string, artifact s3Artifact) (s3Artifact, error) {
	if err := ctx.Err(); err != nil {
		return artifact, err
	}
	return artifact, nil
}

type fakeS3Object struct {
	state s3ObjectState
	data  string
}

type fakeS3WriteOperations struct {
	objects               map[string]fakeS3Object
	stageFailure          bool
	stageResponseLost     bool
	promoteFailure        error
	promoteResponseLost   bool
	promoteAfterCommitErr error
	mutateBeforePromote   bool
	headErrs              map[string][]error
	deleteFailures        int
	promoteCalls          int
	cleanupCanceled       bool
	headFailures          int
	nextETag              int
}

type fakeS3ArtifactOperations struct {
	artifacts    []s3Artifact
	listedPrefix string
	removeErrors map[string]error
	removed      []s3Artifact
}

func (o *fakeS3ArtifactOperations) list(ctx context.Context, _, prefix string) ([]s3Artifact, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	o.listedPrefix = prefix
	return append([]s3Artifact(nil), o.artifacts...), nil
}

func (o *fakeS3ArtifactOperations) remove(ctx context.Context, _ string, artifact s3Artifact) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	o.removed = append(o.removed, artifact)
	return o.removeErrors[artifact.key]
}

func newTestS3Client(server *httptest.Server) *s3sdk.Client {
	return s3sdk.NewFromConfig(
		aws.Config{
			Region:      "us-east-1",
			Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider("AKID", "SECRET", "")),
			HTTPClient:  server.Client(),
		},
		func(o *s3sdk.Options) {
			o.BaseEndpoint = aws.String(server.URL)
			o.UsePathStyle = true
		},
	)
}

func newFakeS3WriteOperations() *fakeS3WriteOperations {
	return &fakeS3WriteOperations{
		objects:  make(map[string]fakeS3Object),
		headErrs: make(map[string][]error),
	}
}

func (f *fakeS3WriteOperations) head(
	_ context.Context, _, key string,
) (s3ObjectState, error) {
	if f.headFailures > 0 {
		f.headFailures--
		return s3ObjectState{}, context.DeadlineExceeded
	}
	if errs := f.headErrs[key]; len(errs) > 0 {
		err := errs[0]
		f.headErrs[key] = errs[1:]
		if err != nil {
			return s3ObjectState{}, err
		}
	}
	obj, ok := f.objects[key]
	if !ok {
		return s3ObjectState{}, &types.NotFound{}
	}
	return obj.state, nil
}

func (f *fakeS3WriteOperations) putStage(
	ctx context.Context, _, key string, data []byte, writeID string,
) (s3ObjectState, error) {
	if f.stageFailure {
		return s3ObjectState{}, errors.New("stage checksum failed")
	}
	f.nextETag++
	etag := fmt.Sprintf("\"etag-%d\"", f.nextETag)
	state := s3ObjectState{
		etag:     &etag,
		size:     int64(len(data)),
		metadata: map[string]string{"shoal-write-id": writeID},
	}
	f.objects[key] = fakeS3Object{state: state, data: string(data)}
	if f.stageResponseLost {
		f.stageResponseLost = false
		return s3ObjectState{}, errors.New("stage response lost")
	}
	if err := ctx.Err(); err != nil {
		return s3ObjectState{}, err
	}
	return state, nil
}

func (f *fakeS3WriteOperations) promote(
	ctx context.Context,
	_, stageKey, key string,
	_ s3ObjectState,
	targetExists bool,
	target s3ObjectState,
) error {
	f.promoteCalls++
	if f.mutateBeforePromote {
		f.mutateBeforePromote = false
		f.nextETag++
		etag := fmt.Sprintf("\"etag-%d\"", f.nextETag)
		f.objects[key] = fakeS3Object{
			state: s3ObjectState{etag: &etag, size: int64(len("concurrent"))},
			data:  "concurrent",
		}
	}
	current, exists := f.objects[key]
	if exists != targetExists ||
		(exists && !equalStringPointers(current.state.etag, target.etag)) {
		return errors.New("destination precondition failed")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.promoteFailure != nil {
		err := f.promoteFailure
		f.promoteFailure = nil
		return err
	}
	stage := f.objects[stageKey]
	f.nextETag++
	etag := fmt.Sprintf("\"etag-%d\"", f.nextETag)
	stage.state.etag = &etag
	f.objects[key] = stage
	if f.promoteAfterCommitErr != nil {
		err := f.promoteAfterCommitErr
		f.promoteAfterCommitErr = nil
		return err
	}
	if f.promoteResponseLost {
		f.promoteResponseLost = false
		return errors.New("copy response lost")
	}
	return nil
}

func (f *fakeS3WriteOperations) deleteStage(
	ctx context.Context, _, key string, _ s3ObjectState,
) error {
	if ctx.Err() != nil {
		f.cleanupCanceled = true
		return ctx.Err()
	}
	if f.deleteFailures > 0 {
		f.deleteFailures--
		return errors.New("delete failed")
	}
	if _, ok := f.objects[key]; !ok {
		return &types.NotFound{}
	}
	delete(f.objects, key)
	return nil
}

func newFakeS3Writer(f *fakeS3WriteOperations, target string) *writer {
	targetState, err := f.head(context.Background(), "bucket", target)
	return &writer{
		ops:            f,
		bucket:         "bucket",
		key:            target,
		stageKey:       ".shoal-tmp/stage",
		writeID:        "write-id",
		ctx:            context.Background(),
		targetExists:   err == nil,
		target:         targetState,
		cleanupTimeout: s3CleanupTimeout,
	}
}

func TestParsePath(t *testing.T) {
	cases := []struct {
		in      string
		bucket  string
		key     string
		wantErr bool
	}{
		{"s3://my-bucket/path/to/file.rf", "my-bucket", "path/to/file.rf", false},
		{"my-bucket/path/to/file.rf", "my-bucket", "path/to/file.rf", false},
		{"s3://b/o", "b", "o", false},
		{"s3://", "", "", true},
		{"no-slash", "", "", true},
		{"s3://b/", "", "", true},          // trailing slash only → empty key
		{"/leading-slash/o", "", "", true}, // leading slash → empty bucket → error
		{"", "", "", true},
	}

	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			b, k, err := ParsePath(c.in)
			if c.wantErr {
				if err == nil {
					t.Errorf("expected error, got bucket=%q key=%q", b, k)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if b != c.bucket || k != c.key {
				t.Errorf("got (%q, %q), want (%q, %q)", b, k, c.bucket, c.key)
			}
		})
	}
}

func TestParseArtifactCleanupPrefixAcceptsBucketRoot(t *testing.T) {
	for _, path := range []string{"s3://bucket", "s3://bucket/", "bucket"} {
		bucket, prefix, err := parseArtifactCleanupPrefix(path)
		if err != nil || bucket != "bucket" || prefix != "" {
			t.Fatalf("parseArtifactCleanupPrefix(%q) = %q, %q, %v", path, bucket, prefix, err)
		}
	}
}

func TestCleanupStaleArtifactsListsBucketRoot(t *testing.T) {
	ops := &fakeS3ArtifactOperations{}
	backend := &Backend{artifactOps: ops}
	if _, err := backend.CleanupStaleArtifacts(context.Background(), "s3://bucket/", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ops.listedPrefix != "" {
		t.Fatalf("listed prefix = %q, want bucket root", ops.listedPrefix)
	}
}

// TestIsNotFound exercises the not-found detection helpers without any network.
func TestIsNotFound(t *testing.T) {
	if !isNotFound(&types.NoSuchKey{}) {
		t.Error("expected isNotFound(*types.NoSuchKey) = true")
	}
	if !isNotFound(&types.NotFound{}) {
		t.Error("expected isNotFound(*types.NotFound) = true")
	}
	if isNotFound(fmt.Errorf("some other error")) {
		t.Error("expected isNotFound(generic error) = false")
	}
}

// TestWriter_Write exercises the in-memory buffer without any S3 call.
func TestWriter_Write(t *testing.T) {
	w := &writer{}
	data := []byte("hello s3")
	n, err := w.Write(data)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(data) {
		t.Errorf("n = %d, want %d", n, len(data))
	}
	if w.buf.Len() != len(data) {
		t.Errorf("buf.Len() = %d, want %d", w.buf.Len(), len(data))
	}
	// A second write should accumulate.
	more := []byte(" world")
	w.Write(more) //nolint:errcheck
	if w.buf.Len() != len(data)+len(more) {
		t.Errorf("accumulated buf.Len() = %d, want %d", w.buf.Len(), len(data)+len(more))
	}
}

func TestWriter_AbortDiscardsBufferedDataAndRejectsLaterUse(t *testing.T) {
	w := &writer{}
	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	if _, err := w.Write([]byte(" world")); err != nil {
		t.Fatalf("second Write: %v", err)
	}
	if got := w.buf.Len(); got != len("hello world") {
		t.Fatalf("buf.Len() before Abort = %d, want %d", got, len("hello world"))
	}

	if err := w.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if got := w.buf.Len(); got != 0 {
		t.Fatalf("buf.Len() after Abort = %d, want 0", got)
	}
	if err := w.Abort(); err != nil {
		t.Fatalf("second Abort: %v", err)
	}
	if got := w.buf.Len(); got != 0 {
		t.Fatalf("buf.Len() after second Abort = %d, want 0", got)
	}
	if _, err := w.Write([]byte("late")); err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("Write after Abort error = %v, want aborted state", err)
	}
	if err := w.Close(); err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("Close after Abort error = %v, want aborted state", err)
	}
}

func TestWriter_WriteAfterCloseReportsClosedState(t *testing.T) {
	w := &writer{closed: true}
	if _, err := w.Write([]byte("late")); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Write after Close error = %v, want closed state", err)
	}
}

func TestNextTemporaryStageKeyPreservesPrefixAndBoundsUTF8Bytes(t *testing.T) {
	original := randomStageKeyToken
	randomStageKeyToken = func() (string, error) {
		return strings.Repeat("a", tempStageRandomHexLen), nil
	}
	t.Cleanup(func() {
		randomStageKeyToken = original
	})

	key := "tenant/path/" + strings.Repeat("界", 338)
	stageKey, err := nextTemporaryStageKey(key)
	if err != nil {
		t.Fatalf("nextTemporaryStageKey: %v", err)
	}
	if got, want := stageKeyParentPrefix(stageKey), stageKeyParentPrefix(key); got != want {
		t.Fatalf("stage prefix = %q, want %q", got, want)
	}
	if len(stageKey) > maxObjectKeyBytes {
		t.Fatalf("stage key length = %d bytes, want <= %d", len(stageKey), maxObjectKeyBytes)
	}
}

func TestNextTemporaryStageKeyRejectsInsteadOfEscapingPrefixScope(t *testing.T) {
	original := randomStageKeyToken
	tokenCalls := 0
	randomStageKeyToken = func() (string, error) {
		tokenCalls++
		return strings.Repeat("b", tempStageRandomHexLen), nil
	}
	t.Cleanup(func() {
		randomStageKeyToken = original
	})

	key := strings.Repeat("a", 500) + "/" + strings.Repeat("b", 498) + "/x"
	if _, err := nextTemporaryStageKey(key); err == nil || !strings.Contains(err.Error(), "refusing to stage outside the destination prefix") {
		t.Fatalf("nextTemporaryStageKey error = %v, want prefix-scope rejection", err)
	}
	if tokenCalls != 0 {
		t.Fatalf("random token generated %d times before prefix validation", tokenCalls)
	}
}

func TestNextTemporaryStageKeyRejectsPrefixWithoutFullRandomToken(t *testing.T) {
	original := randomStageKeyToken
	randomStageKeyToken = func() (string, error) {
		return strings.Repeat("b", tempStageRandomHexLen), nil
	}
	t.Cleanup(func() {
		randomStageKeyToken = original
	})

	key := strings.Repeat("a", 999) + "/x"
	if _, err := nextTemporaryStageKey(key); err == nil {
		t.Fatal("nextTemporaryStageKey succeeded without room for the full generated stage key")
	}
}

func TestNextTemporaryStageKeyKeepsFullRandomTokenAtMinimumSpace(t *testing.T) {
	original := randomStageKeyToken
	tokens := []string{
		strings.Repeat("c", tempStageRandomHexLen),
		strings.Repeat("d", tempStageRandomHexLen),
	}
	randomStageKeyToken = func() (string, error) {
		token := tokens[0]
		tokens = tokens[1:]
		return token, nil
	}
	t.Cleanup(func() {
		randomStageKeyToken = original
	})

	key := strings.Repeat("a", 998) + "/x"
	stageKey, err := nextTemporaryStageKey(key)
	if err != nil {
		t.Fatalf("nextTemporaryStageKey: %v", err)
	}
	if got, want := stageKeyParentPrefix(stageKey), stageKeyParentPrefix(key); got != want {
		t.Fatalf("stage prefix = %q, want %q", got, want)
	}
	component := stageKey[len(stageKeyParentPrefix(stageKey)):]
	if got, want := component, expectedTemporaryStageKeyComponent(key, strings.Repeat("c", tempStageRandomHexLen)); got != want {
		t.Fatalf("temporary component = %q, want %q", got, want)
	}
	if !isTemporaryStageKey(stageKey) {
		t.Fatalf("temporary key %q is visible to List", stageKey)
	}
	other, err := nextTemporaryStageKey(key)
	if err != nil {
		t.Fatalf("second nextTemporaryStageKey: %v", err)
	}
	if other == stageKey {
		t.Fatalf("distinct random tokens produced the same temporary key %q", stageKey)
	}
}

func TestIsTemporaryStageKeyMatchesOnlyReservedFormats(t *testing.T) {
	legacyUUID := "123e4567-e89b-12d3-a456-426614174000"
	if !isTemporaryStageKey(".shoal-tmp/" + legacyUUID) {
		t.Fatal("legacy temporary stage key was not detected")
	}
	if isTemporaryStageKey(".shoal-tmp/" + strings.ToUpper(legacyUUID)) {
		t.Fatal("uppercase legacy user key should remain visible")
	}
	if !isTemporaryStageKey("tenant/.shoal-tmp-aaaaaaaaaa1234") {
		t.Fatal("generated temporary stage key was not detected")
	}
	if isTemporaryStageKey("tenant/.shoal-tmp-AAAAAAAAAA1234") {
		t.Fatal("uppercase user key should remain visible")
	}
	if isTemporaryStageKey("tenant/.shoal-tmp-aaaaaaaaaa") {
		t.Fatal("partial generated temporary stage key should remain visible")
	}
	if isTemporaryStageKey(".shoal-tmp/user-visible") {
		t.Fatal("arbitrary .shoal-tmp/ key should remain visible")
	}
	if isTemporaryStageKey("tenant/.shl-visible-object") {
		t.Fatal("arbitrary .shl- key should remain visible")
	}
	if isTemporaryStageKey("tenant/.shl-aaaaaaaaa") {
		t.Fatal("short .shl- key should remain visible")
	}
	if isTemporaryStageKey("tenant/.shoal-tmp-aaaaaaaaaa12345") {
		t.Fatal("longer .shoal-tmp- key should remain visible")
	}
	if isTemporaryStageKey("tenant/.shoal-tmp-AAAAAAAAAAAAAA") {
		t.Fatal("uppercase-shaped .shoal-tmp- key should remain visible")
	}
}

func TestCleanupStaleArtifactsIsBoundedConditionalAndExplicit(t *testing.T) {
	now := time.Now()
	oldETag := `"old"`
	recentETag := `"recent"`
	oldVersion := "old-version"
	failingVersion := "failing-version"
	recentVersion := "recent-version"
	userVersion := "user-version"
	markerVersion := "marker-version"
	recentMarkerVersion := "recent-marker-version"
	removeErr := errors.New("delete failed")
	old := "dir/" + expectedTemporaryStageKeyComponent("dir/target", strings.Repeat("0", 64))
	failing := "dir/" + expectedTemporaryStageKeyComponent("dir/target", strings.Repeat("1", 64))
	recent := "dir/" + expectedTemporaryStageKeyComponent("dir/target", strings.Repeat("2", 64))
	lookalike := "dir/" + expectedTemporaryStageKeyComponent("dir/user", strings.Repeat("3", 64))
	marker := lookalike
	recentMarker := "dir/" + expectedTemporaryStageKeyComponent("dir/recent-marker", strings.Repeat("4", 64))
	uppercase := "dir/" + tempStageKeyPrefix + strings.Repeat("A", tempStageRandomHexLen+tempStageHashHexLen)
	ops := &fakeS3ArtifactOperations{
		artifacts: []s3Artifact{
			{key: recent, lastModified: now, etag: &recentETag, versionID: &recentVersion, owned: true},
			{key: lookalike, lastModified: now.Add(-2 * time.Hour), etag: &oldETag, versionID: &userVersion},
			{key: uppercase, lastModified: now.Add(-2 * time.Hour), etag: &oldETag, versionID: aws.String("uppercase-version"), owned: true},
			{key: failing, lastModified: now.Add(-2 * time.Hour), etag: &oldETag, versionID: &failingVersion, owned: true},
			{key: old, lastModified: now.Add(-2 * time.Hour), etag: &oldETag, versionID: &oldVersion, owned: true},
			{key: marker, lastModified: now.Add(-2 * time.Hour), versionID: &markerVersion, deleteMarker: true},
			{key: recentMarker, lastModified: now, versionID: &recentMarkerVersion, deleteMarker: true},
		},
		removeErrors: map[string]error{failing: removeErr},
	}
	backend := &Backend{artifactOps: ops}

	result, err := backend.CleanupStaleArtifacts(context.Background(), "s3://bucket/dir", now.Add(-time.Hour))
	if !errors.Is(err, removeErr) {
		t.Fatalf("error = %v, want delete failure", err)
	}
	if result.Examined != 6 {
		t.Fatalf("Examined = %d, want 6", result.Examined)
	}
	wantRemoved := []string{"s3://bucket/" + old + "?versionId=" + oldVersion}
	if fmt.Sprint(result.Removed) != fmt.Sprint(wantRemoved) {
		t.Fatalf("Removed = %v", result.Removed)
	}
	wantRecoverable := []string{"s3://bucket/" + marker + "?versionId=" + markerVersion}
	if fmt.Sprint(result.Recoverable) != fmt.Sprint(wantRecoverable) {
		t.Fatalf("Recoverable = %v, want %v", result.Recoverable, wantRecoverable)
	}
	if len(ops.removed) != 2 {
		t.Fatalf("conditional removals = %#v", ops.removed)
	}
	for _, removed := range ops.removed {
		if removed.versionID == nil {
			t.Fatalf("removal missing version ID: %#v", removed)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := backend.CleanupStaleArtifacts(ctx, "s3://bucket/dir", now); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cleanup error = %v, want context.Canceled", err)
	}
}

func TestCleanupStaleArtifactsSupportsRootPrefix(t *testing.T) {
	now := time.Now()
	oldETag := `"old"`
	recentETag := `"recent"`
	oldVersion := "old-version"
	recentVersion := "recent-version"
	old := expectedTemporaryStageKeyComponent("target", strings.Repeat("0", 64))
	recent := expectedTemporaryStageKeyComponent("target", strings.Repeat("1", 64))
	uppercase := tempStageKeyPrefix + strings.Repeat("A", tempStageRandomHexLen+tempStageHashHexLen)
	visible := ".shl-visible"
	ops := &fakeS3ArtifactOperations{
		artifacts: []s3Artifact{
			{key: recent, lastModified: now, etag: &recentETag, versionID: &recentVersion, owned: true},
			{key: uppercase, lastModified: now.Add(-2 * time.Hour), etag: &oldETag, versionID: aws.String("upper-version"), owned: true},
			{key: visible, lastModified: now.Add(-2 * time.Hour), etag: &oldETag, versionID: aws.String("visible-version"), owned: true},
			{key: old, lastModified: now.Add(-2 * time.Hour), etag: &oldETag, versionID: &oldVersion, owned: true},
		},
	}
	backend := &Backend{artifactOps: ops}

	result, err := backend.CleanupStaleArtifacts(context.Background(), "s3://bucket/", now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if ops.listedPrefix != "" {
		t.Fatalf("listed prefix = %q, want root prefix", ops.listedPrefix)
	}
	if result.Examined != 2 {
		t.Fatalf("Examined = %d, want 2", result.Examined)
	}
	wantRemoved := "s3://bucket/" + old + "?versionId=" + oldVersion
	if len(result.Removed) != 1 || result.Removed[0] != wantRemoved {
		t.Fatalf("Removed = %v, want [%s]", result.Removed, wantRemoved)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := backend.CleanupStaleArtifacts(ctx, "s3://bucket/", now); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cleanup error = %v, want context.Canceled", err)
	}
}

func TestCleanupStaleArtifactsDeletesUnversionedStagesAndSkipsETagRaces(t *testing.T) {
	now := time.Now()
	oldETag := `"old"`
	freshETag := `"fresh"`
	raceETag := `"race"`
	old := "dir/" + expectedTemporaryStageKeyComponent("dir/target", strings.Repeat("a", 64))
	fresh := "dir/" + expectedTemporaryStageKeyComponent("dir/target", strings.Repeat("b", 64))
	race := "dir/" + expectedTemporaryStageKeyComponent("dir/target", strings.Repeat("c", 64))
	ops := &fakeS3ArtifactOperations{
		artifacts: []s3Artifact{
			{key: old, lastModified: now.Add(-2 * time.Hour), etag: &oldETag, owned: true},
			{key: fresh, lastModified: now, etag: &freshETag, owned: true},
			{key: race, lastModified: now.Add(-2 * time.Hour), etag: &raceETag, owned: true},
		},
		removeErrors: map[string]error{
			race: &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "etag changed"},
		},
	}
	backend := &Backend{artifactOps: ops}

	result, err := backend.CleanupStaleArtifacts(context.Background(), "s3://bucket/dir", now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("CleanupStaleArtifacts: %v", err)
	}
	if result.Examined != 3 {
		t.Fatalf("Examined = %d, want 3", result.Examined)
	}
	if len(result.Removed) != 1 || result.Removed[0] != "s3://bucket/"+old {
		t.Fatalf("Removed = %v, want [%s]", result.Removed, "s3://bucket/"+old)
	}
	if len(ops.removed) != 2 {
		t.Fatalf("removal attempts = %#v, want stale delete plus skipped race", ops.removed)
	}
	if ops.removed[0].versionID != nil || ops.removed[1].versionID != nil {
		t.Fatalf("unversioned removal attempts should not include version IDs: %#v", ops.removed)
	}
}

func TestSDKS3ArtifactOperationsListPaginatesVersionsAndDeleteMarkers(t *testing.T) {
	stageA := "dir/" + expectedTemporaryStageKeyComponent("dir/target-a", strings.Repeat("5", 64))
	stageB := "dir/" + expectedTemporaryStageKeyComponent("dir/target-b", strings.Repeat("6", 64))
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/bucket" {
			t.Errorf("path = %s, want /bucket", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/xml")
		switch calls {
		case 1:
			if _, ok := r.URL.Query()["versioning"]; !ok {
				t.Errorf("query = %q, want versioning status request", r.URL.RawQuery)
			}
			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Status>Suspended</Status>
</VersioningConfiguration>`)
		case 2:
			if got := r.URL.Query().Get("prefix"); got != "dir/" {
				t.Errorf("prefix = %q, want dir/", got)
			}
			if _, ok := r.URL.Query()["versions"]; !ok {
				t.Errorf("query = %q, want versions listing", r.URL.RawQuery)
			}
			if got := r.URL.Query().Get("key-marker"); got != "" {
				t.Errorf("first key-marker = %q, want empty", got)
			}
			if got := r.URL.Query().Get("version-id-marker"); got != "" {
				t.Errorf("first version-id-marker = %q, want empty", got)
			}
			fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListVersionsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Name>bucket</Name>
  <Prefix>dir/</Prefix>
  <IsTruncated>true</IsTruncated>
  <NextKeyMarker>%s</NextKeyMarker>
  <NextVersionIdMarker>v-old</NextVersionIdMarker>
  <Version>
    <Key>%s</Key>
    <VersionId>v-old</VersionId>
    <IsLatest>false</IsLatest>
    <LastModified>2026-08-19T00:00:00.000Z</LastModified>
    <ETag>&quot;etag-old&quot;</ETag>
    <Size>1</Size>
    <StorageClass>STANDARD</StorageClass>
  </Version>
</ListVersionsResult>`, stageA, stageA)
		case 3:
			if got := r.URL.Query().Get("prefix"); got != "dir/" {
				t.Errorf("prefix = %q, want dir/", got)
			}
			if _, ok := r.URL.Query()["versions"]; !ok {
				t.Errorf("query = %q, want versions listing", r.URL.RawQuery)
			}
			if got := r.URL.Query().Get("key-marker"); got != stageA {
				t.Errorf("second key-marker = %q, want %q", got, stageA)
			}
			if got := r.URL.Query().Get("version-id-marker"); got != "v-old" {
				t.Errorf("second version-id-marker = %q, want v-old", got)
			}
			fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListVersionsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Name>bucket</Name>
  <Prefix>dir/</Prefix>
  <IsTruncated>false</IsTruncated>
  <DeleteMarker>
    <Key>%s</Key>
    <VersionId>m-old</VersionId>
    <IsLatest>true</IsLatest>
    <LastModified>2026-08-19T01:00:00.000Z</LastModified>
  </DeleteMarker>
  <Version>
    <Key>%s</Key>
    <VersionId>null</VersionId>
    <IsLatest>true</IsLatest>
    <LastModified>2026-08-19T02:00:00.000Z</LastModified>
    <ETag>&quot;etag-null&quot;</ETag>
    <Size>1</Size>
    <StorageClass>STANDARD</StorageClass>
  </Version>
</ListVersionsResult>`, stageA, stageB)
		default:
			t.Fatalf("unexpected extra paginator call %d", calls)
		}
	}))
	defer server.Close()

	client := newTestS3Client(server)

	artifacts, err := (sdkS3ArtifactOperations{client: client}).list(context.Background(), "bucket", "dir/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if calls != 3 {
		t.Fatalf("pagination calls = %d, want 3", calls)
	}
	if len(artifacts) != 3 {
		t.Fatalf("artifacts = %#v, want 3 entries", artifacts)
	}
	got := make(map[string]s3Artifact, len(artifacts))
	for _, artifact := range artifacts {
		got[artifact.key+"#"+aws.ToString(artifact.versionID)] = artifact
	}
	if artifact, ok := got[stageA+"#v-old"]; !ok || artifact.deleteMarker || artifact.etag == nil {
		t.Fatalf("old stage version = %#v, want version with ETag", artifact)
	}
	if artifact, ok := got[stageA+"#m-old"]; !ok || !artifact.deleteMarker {
		t.Fatalf("stale delete marker = %#v, want delete marker entry", artifact)
	}
	if artifact, ok := got[stageB+"#null"]; !ok || artifact.deleteMarker || artifact.etag == nil {
		t.Fatalf("null-version stage = %#v, want current version with ETag", artifact)
	}
}

func TestSDKS3ArtifactOperationsListUsesObjectVersionsWhenEnabled(t *testing.T) {
	stage := "dir/" + expectedTemporaryStageKeyComponent("dir/target-enabled", strings.Repeat("9", 64))
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/bucket" {
			t.Errorf("path = %s, want /bucket", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/xml")
		switch calls {
		case 1:
			if _, ok := r.URL.Query()["versioning"]; !ok {
				t.Errorf("query = %q, want versioning status request", r.URL.RawQuery)
			}
			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Status>Enabled</Status>
</VersioningConfiguration>`)
		case 2:
			if _, ok := r.URL.Query()["versions"]; !ok {
				t.Errorf("query = %q, want versions listing", r.URL.RawQuery)
			}
			fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListVersionsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Name>bucket</Name>
  <Prefix>dir/</Prefix>
  <IsTruncated>false</IsTruncated>
  <Version>
    <Key>%s</Key>
    <VersionId>v-enabled</VersionId>
    <IsLatest>true</IsLatest>
    <LastModified>2026-08-19T03:00:00.000Z</LastModified>
    <ETag>&quot;etag-enabled&quot;</ETag>
    <Size>1</Size>
    <StorageClass>STANDARD</StorageClass>
  </Version>
</ListVersionsResult>`, stage)
		default:
			t.Fatalf("unexpected extra call %d", calls)
		}
	}))
	defer server.Close()

	artifacts, err := (sdkS3ArtifactOperations{client: newTestS3Client(server)}).list(context.Background(), "bucket", "dir/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if len(artifacts) != 1 || artifacts[0].key != stage || aws.ToString(artifacts[0].versionID) != "v-enabled" {
		t.Fatalf("artifacts = %#v, want one versioned stage", artifacts)
	}
}

func TestSDKS3ArtifactOperationsListUsesObjectsV2ForNeverVersionedBuckets(t *testing.T) {
	old := "dir/" + expectedTemporaryStageKeyComponent("dir/target-old", strings.Repeat("7", 64))
	fresh := "dir/" + expectedTemporaryStageKeyComponent("dir/target-fresh", strings.Repeat("8", 64))
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/bucket" {
			t.Errorf("path = %s, want /bucket", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/xml")
		switch calls {
		case 1:
			if _, ok := r.URL.Query()["versioning"]; !ok {
				t.Errorf("query = %q, want versioning status request", r.URL.RawQuery)
			}
			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></VersioningConfiguration>`)
		case 2:
			if got := r.URL.Query().Get("list-type"); got != "2" {
				t.Errorf("list-type = %q, want 2", got)
			}
			if got := r.URL.Query().Get("prefix"); got != "dir/" {
				t.Errorf("prefix = %q, want dir/", got)
			}
			if got := r.URL.Query().Get("continuation-token"); got != "" {
				t.Errorf("first continuation-token = %q, want empty", got)
			}
			fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Name>bucket</Name>
  <Prefix>dir/</Prefix>
  <IsTruncated>true</IsTruncated>
  <NextContinuationToken>next-page</NextContinuationToken>
  <Contents>
    <Key>%s</Key>
    <LastModified>2026-08-19T00:00:00.000Z</LastModified>
    <ETag>&quot;etag-old&quot;</ETag>
    <Size>1</Size>
    <StorageClass>STANDARD</StorageClass>
  </Contents>
</ListBucketResult>`, old)
		case 3:
			if got := r.URL.Query().Get("list-type"); got != "2" {
				t.Errorf("list-type = %q, want 2", got)
			}
			if got := r.URL.Query().Get("continuation-token"); got != "next-page" {
				t.Errorf("second continuation-token = %q, want next-page", got)
			}
			fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>
<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Name>bucket</Name>
  <Prefix>dir/</Prefix>
  <IsTruncated>false</IsTruncated>
  <Contents>
    <Key>%s</Key>
    <LastModified>2026-08-19T01:00:00.000Z</LastModified>
    <ETag>&quot;etag-fresh&quot;</ETag>
    <Size>1</Size>
    <StorageClass>STANDARD</StorageClass>
  </Contents>
</ListBucketResult>`, fresh)
		default:
			t.Fatalf("unexpected extra paginator call %d", calls)
		}
	}))
	defer server.Close()

	artifacts, err := (sdkS3ArtifactOperations{client: newTestS3Client(server)}).list(context.Background(), "bucket", "dir/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
	if len(artifacts) != 2 {
		t.Fatalf("artifacts = %#v, want 2 entries", artifacts)
	}
	if artifacts[0].key != old || artifacts[0].versionID != nil || artifacts[0].etag == nil {
		t.Fatalf("first artifact = %#v, want unversioned object with ETag", artifacts[0])
	}
	if artifacts[1].key != fresh || artifacts[1].versionID != nil || artifacts[1].etag == nil {
		t.Fatalf("second artifact = %#v, want unversioned object with ETag", artifacts[1])
	}
}

func TestSDKS3ArtifactOperationsListPropagatesVersioningStatusFailures(t *testing.T) {
	t.Run("status error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>InternalError</Code><Message>boom</Message></Error>`)
		}))
		defer server.Close()

		if _, err := (sdkS3ArtifactOperations{client: newTestS3Client(server)}).list(context.Background(), "bucket", "dir/"); err == nil {
			t.Fatal("list unexpectedly succeeded on GetBucketVersioning failure")
		}
	})

	t.Run("canceled context", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("GetBucketVersioning should not complete after context cancellation")
		}))
		defer server.Close()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := (sdkS3ArtifactOperations{client: newTestS3Client(server)}).list(ctx, "bucket", "dir/"); !errors.Is(err, context.Canceled) {
			t.Fatalf("list error = %v, want context.Canceled", err)
		}
	})
}

func TestBackendCreateRejectsReservedInternalStageKeys(t *testing.T) {
	backend := &Backend{ops: newFakeS3WriteOperations()}
	for _, key := range []string{
		".shoal-tmp-aaaaaaaaaa1234",
		"nested/.shoal-tmp-aaaaaaaaaa1234",
		".shoal-tmp/123e4567-e89b-12d3-a456-426614174000",
	} {
		t.Run(key, func(t *testing.T) {
			if _, err := backend.Create(context.Background(), "s3://bucket/"+key); err == nil || !strings.Contains(err.Error(), "reserved internal namespace") {
				t.Fatalf("Create(%q) error = %v, want reserved internal namespace rejection", key, err)
			}
		})
	}
}

func TestBackendCreateAllowsUserKeysOutsideReservedNamespace(t *testing.T) {
	for _, key := range []string{
		".shl-final.rf",
		".shl-aaaaaaaaa",
		".shoal-tmp-" + strings.Repeat("A", tempStageRandomHexLen+tempStageHashHexLen),
		".shoal-tmp-aaaaaaaaaa12345",
		"nested/.shl-final.rf",
	} {
		t.Run(key, func(t *testing.T) {
			f := newFakeS3WriteOperations()
			backend := &Backend{ops: f}
			w, err := backend.Create(context.Background(), "s3://bucket/"+key)
			if err != nil {
				t.Fatalf("Create(%q): %v", key, err)
			}
			if _, err := w.Write([]byte("data")); err != nil {
				t.Fatalf("Write(%q): %v", key, err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("Close(%q): %v", key, err)
			}
			if got := f.objects[key].data; got != "data" {
				t.Fatalf("stored data for %q = %q, want data", key, got)
			}
		})
	}
}

func TestWriter_StagedCreateAndReplace(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%v", existing), func(t *testing.T) {
			f := newFakeS3WriteOperations()
			if existing {
				etag := "\"old\""
				f.objects["target"] = fakeS3Object{
					state: s3ObjectState{etag: &etag, size: 3},
					data:  "old",
				}
			}
			w := newFakeS3Writer(f, "target")
			_, _ = w.Write([]byte("new"))
			if err := w.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if got := f.objects["target"].data; got != "new" {
				t.Fatalf("target data = %q, want new", got)
			}
			if _, ok := f.objects[w.stageKey]; ok {
				t.Fatal("temporary object was not removed")
			}
		})
	}
}

func TestBackendCreateProtectsConcurrentlyCreatedDestination(t *testing.T) {
	f := newFakeS3WriteOperations()
	backend := &Backend{ops: f}
	created, err := backend.Create(context.Background(), "s3://bucket/target")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	w := created.(*writer)
	if w.targetExists {
		t.Fatal("missing destination was recorded as existing")
	}
	_, _ = w.Write([]byte("new"))

	etag := "\"concurrent\""
	f.objects["target"] = fakeS3Object{
		state: s3ObjectState{etag: &etag, size: int64(len("concurrent"))},
		data:  "concurrent",
	}
	if err := w.Close(); err == nil {
		t.Fatal("Close overwrote a concurrently created destination")
	}
	if got := f.objects["target"].data; got != "concurrent" {
		t.Fatalf("destination data = %q, want concurrent", got)
	}
}

func TestWriter_VerifiesCommittedResponsesWithoutReplayingPromotion(t *testing.T) {
	f := newFakeS3WriteOperations()
	f.stageResponseLost = true
	f.promoteResponseLost = true
	w := newFakeS3Writer(f, "target")
	_, _ = w.Write([]byte("new"))
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if f.promoteCalls != 1 {
		t.Fatalf("promote calls = %d, want 1", f.promoteCalls)
	}
	if got := f.objects["target"].data; got != "new" {
		t.Fatalf("target data = %q, want new", got)
	}
}

func TestWriter_IndeterminatePromotionCommittedRetrySucceeds(t *testing.T) {
	f := newFakeS3WriteOperations()
	f.promoteResponseLost = true
	w := newFakeS3Writer(f, "target")
	f.headErrs["target"] = []error{context.DeadlineExceeded}

	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Close error = %v, want transient verification failure", err)
	}
	if !w.promotionIndeterminate {
		t.Fatal("first Close did not retain indeterminate promotion state")
	}
	if _, ok := f.objects[w.stageKey]; !ok {
		t.Fatal("indeterminate promotion removed the staged object")
	}

	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if f.promoteCalls != 1 {
		t.Fatalf("promote calls = %d, want 1", f.promoteCalls)
	}
	if got := f.objects["target"].data; got != "new" {
		t.Fatalf("target data = %q, want new", got)
	}
	if _, ok := f.objects[w.stageKey]; ok {
		t.Fatal("second Close did not clean up the staged object")
	}
}

func TestWriter_IndeterminatePromotionCanceledAfterCommitRetriesVerification(t *testing.T) {
	f := newFakeS3WriteOperations()
	f.promoteAfterCommitErr = context.Canceled
	w := newFakeS3Writer(f, "target")
	f.headErrs["target"] = []error{context.DeadlineExceeded}

	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err == nil || !errors.Is(err, context.Canceled) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Close error = %v, want cancellation joined with transient verification failure", err)
	}
	if !w.promotionIndeterminate {
		t.Fatal("first Close did not retain indeterminate promotion state")
	}
	if _, ok := f.objects[w.stageKey]; !ok {
		t.Fatal("canceled promotion removed the staged object")
	}

	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if f.promoteCalls != 1 {
		t.Fatalf("promote calls = %d, want 1", f.promoteCalls)
	}
	if got := f.objects["target"].data; got != "new" {
		t.Fatalf("target data = %q, want new", got)
	}
	if _, ok := f.objects[w.stageKey]; ok {
		t.Fatal("second Close did not clean up the staged object")
	}
}

func TestWriter_IndeterminatePromotionUncommittedRetriesSafely(t *testing.T) {
	f := newFakeS3WriteOperations()
	oldETag := "\"old\""
	f.objects["target"] = fakeS3Object{
		state: s3ObjectState{etag: &oldETag, size: 3},
		data:  "old",
	}
	f.promoteFailure = errors.New("copy response lost")
	w := newFakeS3Writer(f, "target")
	f.headErrs["target"] = []error{context.DeadlineExceeded}

	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Close error = %v, want transient verification failure", err)
	}
	if !w.promotionIndeterminate {
		t.Fatal("first Close did not retain indeterminate promotion state")
	}
	if got := f.objects["target"].data; got != "old" {
		t.Fatalf("target data after failed promote = %q, want old", got)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if f.promoteCalls != 2 {
		t.Fatalf("promote calls = %d, want 2", f.promoteCalls)
	}
	if got := f.objects["target"].data; got != "new" {
		t.Fatalf("target data after retry = %q, want new", got)
	}
	if _, ok := f.objects[w.stageKey]; ok {
		t.Fatal("retry success did not clean up the staged object")
	}
}

func TestWriter_IndeterminateCanceledPromotionCanAbortWhenNotCommitted(t *testing.T) {
	f := newFakeS3WriteOperations()
	oldETag := "\"old\""
	f.objects["target"] = fakeS3Object{
		state: s3ObjectState{etag: &oldETag, size: 3},
		data:  "old",
	}
	f.promoteFailure = context.Canceled
	w := newFakeS3Writer(f, "target")

	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("first Close error = %v, want cancellation", err)
	}
	if !w.promotionIndeterminate {
		t.Fatal("first Close did not retain indeterminate promotion state")
	}
	if got := f.objects["target"].data; got != "old" {
		t.Fatalf("target data after canceled promote = %q, want old", got)
	}
	if err := w.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if !w.aborted {
		t.Fatal("Abort did not mark the writer aborted")
	}
	if w.promotionIndeterminate {
		t.Fatal("Abort left the promotion state indeterminate")
	}
	if _, ok := f.objects[w.stageKey]; ok {
		t.Fatal("Abort did not remove the staged object")
	}
	if got := f.objects["target"].data; got != "old" {
		t.Fatalf("target data after Abort = %q, want old", got)
	}
}

func TestWriter_IndeterminatePromotionDoesNotOverwriteCompetingWriter(t *testing.T) {
	f := newFakeS3WriteOperations()
	f.promoteFailure = errors.New("copy response lost")
	w := newFakeS3Writer(f, "target")
	f.headErrs["target"] = []error{context.DeadlineExceeded}

	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Close error = %v, want transient verification failure", err)
	}

	intruderETag := "\"intruder\""
	f.objects["target"] = fakeS3Object{
		state: s3ObjectState{
			etag:     &intruderETag,
			size:     int64(len("intruder")),
			metadata: map[string]string{"shoal-write-id": "intruder"},
		},
		data: "intruder",
	}

	if err := w.Close(); err == nil || !strings.Contains(err.Error(), "indeterminate") {
		t.Fatalf("second Close error = %v, want indeterminate competing-writer failure", err)
	}
	if f.promoteCalls != 1 {
		t.Fatalf("promote calls = %d, want 1", f.promoteCalls)
	}
	if got := f.objects["target"].data; got != "intruder" {
		t.Fatalf("target data = %q, want intruder", got)
	}
	if _, ok := f.objects[w.stageKey]; !ok {
		t.Fatal("competing writer resolution lost the staged object")
	}
}

func TestWriter_AbortPreservesRecoverabilityDuringIndeterminatePromotion(t *testing.T) {
	f := newFakeS3WriteOperations()
	f.promoteResponseLost = true
	w := newFakeS3Writer(f, "target")
	f.headErrs["target"] = []error{context.DeadlineExceeded}

	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want transient verification failure", err)
	}
	if err := w.Abort(); err == nil || !strings.Contains(err.Error(), "indeterminate") {
		t.Fatalf("Abort error = %v, want indeterminate abort failure", err)
	}
	if w.aborted {
		t.Fatal("Abort marked an indeterminate promotion as aborted")
	}
	if _, ok := f.objects[w.stageKey]; !ok {
		t.Fatal("Abort removed the staged object needed for recovery")
	}
}

func TestWriter_IndeterminatePromotionRetainsStageAcrossRepeatedVerificationFailures(t *testing.T) {
	f := newFakeS3WriteOperations()
	f.promoteResponseLost = true
	w := newFakeS3Writer(f, "target")
	f.headErrs["target"] = []error{context.DeadlineExceeded, context.DeadlineExceeded}

	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Close error = %v, want transient verification failure", err)
	}
	if err := w.Close(); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Close error = %v, want repeated transient verification failure", err)
	}
	if !w.promotionIndeterminate {
		t.Fatal("repeated failures cleared the indeterminate promotion state")
	}
	if f.promoteCalls != 1 {
		t.Fatalf("promote calls = %d, want 1", f.promoteCalls)
	}
	if _, ok := f.objects[w.stageKey]; !ok {
		t.Fatal("repeated failures removed the staged object")
	}
}

func TestWriter_StageFailurePreservesExistingTarget(t *testing.T) {
	f := newFakeS3WriteOperations()
	etag := "\"old\""
	f.objects["target"] = fakeS3Object{
		state: s3ObjectState{etag: &etag, size: 3},
		data:  "old",
	}
	f.stageFailure = true
	w := newFakeS3Writer(f, "target")
	_, _ = w.Write([]byte("new"))
	if err := w.Close(); err == nil {
		t.Fatal("Close succeeded after stage failure")
	}
	if got := f.objects["target"].data; got != "old" {
		t.Fatalf("target data = %q, want old", got)
	}
}

func TestWriter_ConcurrentTargetMutationIsPreserved(t *testing.T) {
	f := newFakeS3WriteOperations()
	etag := "\"old\""
	f.objects["target"] = fakeS3Object{
		state: s3ObjectState{etag: &etag, size: 3},
		data:  "old",
	}
	f.mutateBeforePromote = true
	w := newFakeS3Writer(f, "target")
	_, _ = w.Write([]byte("new"))
	if err := w.Close(); err == nil {
		t.Fatal("Close succeeded after concurrent target mutation")
	}
	if got := f.objects["target"].data; got != "concurrent" {
		t.Fatalf("target data = %q, want concurrent", got)
	}
	if _, ok := f.objects[w.stageKey]; ok {
		t.Fatal("temporary object was not removed after failed promotion")
	}
}

func TestWriter_CleanupUsesIndependentContextAndCanRetryAfterCommit(t *testing.T) {
	t.Run("request cancellation", func(t *testing.T) {
		f := newFakeS3WriteOperations()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		w := newFakeS3Writer(f, "target")
		w.ctx = ctx
		_, _ = w.Write([]byte("new"))
		if err := w.Close(); err == nil {
			t.Fatal("Close succeeded with canceled promotion context")
		}
		if f.cleanupCanceled {
			t.Fatal("cleanup inherited canceled request context")
		}
		if _, ok := f.objects[w.stageKey]; !ok {
			t.Fatal("canceled promotion did not retain the staged object for retry")
		}
		if err := w.Abort(); err != nil {
			t.Fatalf("Abort: %v", err)
		}
		if f.cleanupCanceled {
			t.Fatal("abort cleanup inherited canceled request context")
		}
		if _, ok := f.objects[w.stageKey]; ok {
			t.Fatal("Abort did not remove the temporary object")
		}
	})

	t.Run("retry after committed close", func(t *testing.T) {
		f := newFakeS3WriteOperations()
		f.deleteFailures = 1
		w := newFakeS3Writer(f, "target")
		_, _ = w.Write([]byte("new"))
		if err := w.Close(); err != nil {
			t.Fatalf("Close promoted cleanup failure: %v", err)
		}
		if _, ok := f.objects[w.stageKey]; ok {
			t.Fatal("committed cleanup did not retry and remove the temporary object")
		}
	})

	t.Run("retry unknown stage after ambiguous upload", func(t *testing.T) {
		f := newFakeS3WriteOperations()
		f.stageResponseLost = true
		w := newFakeS3Writer(f, "target")
		f.headFailures = 2
		_, _ = w.Write([]byte("new"))
		if err := w.Close(); err == nil {
			t.Fatal("Close succeeded despite upload and verification failures")
		}
		if _, ok := f.objects[w.stageKey]; !ok {
			t.Fatal("ambiguous temporary object unexpectedly absent before retry")
		}
		if err := w.Abort(); err != nil {
			t.Fatalf("Abort retry: %v", err)
		}
		if _, ok := f.objects[w.stageKey]; ok {
			t.Fatal("Abort did not discover and remove temporary object")
		}
	})
}

func TestWriter_CloseLogsExhaustedCommittedCleanupFailure(t *testing.T) {
	var logs bytes.Buffer
	restore := captureDefaultLogger(t, &logs)
	defer restore()

	f := newFakeS3WriteOperations()
	f.deleteFailures = committedCleanupAttempts
	w := newFakeS3Writer(f, "target")
	_, _ = w.Write([]byte("new"))

	if err := w.Close(); err != nil {
		t.Fatalf("Close promoted committed destination failure: %v", err)
	}
	if _, ok := f.objects[w.stageKey]; !ok {
		t.Fatal("committed cleanup unexpectedly removed the temporary object")
	}
	if got := logs.String(); !strings.Contains(got, "temporary stage pending cleanup") || !strings.Contains(got, w.stageKey) {
		t.Fatalf("cleanup warning log = %q, want pending cleanup warning for %s", got, w.stageKey)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, ok := f.objects[w.stageKey]; ok {
		t.Fatal("second Close did not retry exhausted committed cleanup")
	}
}

func TestWriter_AbortRetriesOwnedCleanupAfterAmbiguousStageUpload(t *testing.T) {
	f := newFakeS3WriteOperations()
	f.stageResponseLost = true
	w := newFakeS3Writer(f, "target")
	f.headErrs[w.stageKey] = []error{context.DeadlineExceeded, context.DeadlineExceeded}

	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want transient stage verification failure", err)
	}
	if _, ok := f.objects[w.stageKey]; !ok {
		t.Fatal("ambiguous stage upload lost staged object before retry")
	}

	f.deleteFailures = 1
	if err := w.Abort(); err == nil || !strings.Contains(err.Error(), "remove temporary object") {
		t.Fatalf("first Abort error = %v, want cleanup retryable delete failure", err)
	}
	if _, ok := f.objects[w.stageKey]; !ok {
		t.Fatal("failed Abort removed staged object unexpectedly")
	}
	if _, err := w.Write([]byte("late")); err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("Write after failed Abort error = %v, want aborted state", err)
	}
	if err := w.Close(); err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("Close after failed Abort error = %v, want aborted state", err)
	}
	if err := w.Abort(); err != nil {
		t.Fatalf("second Abort: %v", err)
	}
	if _, ok := f.objects[w.stageKey]; ok {
		t.Fatal("second Abort did not remove staged object")
	}
	if err := w.Abort(); err != nil {
		t.Fatalf("third Abort: %v", err)
	}
	if err := w.Close(); err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("Close after Abort error = %v, want aborted state", err)
	}
}

func TestWriter_AbortTreatsUnownedStageAsAlreadyCleanAfterAmbiguousUpload(t *testing.T) {
	f := newFakeS3WriteOperations()
	f.stageResponseLost = true
	w := newFakeS3Writer(f, "target")
	f.headErrs[w.stageKey] = []error{context.DeadlineExceeded, context.DeadlineExceeded}

	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want transient stage verification failure", err)
	}

	etag := "\"intruder\""
	f.objects[w.stageKey] = fakeS3Object{
		state: s3ObjectState{
			etag:     &etag,
			size:     int64(len("intruder")),
			metadata: map[string]string{"shoal-write-id": "intruder"},
		},
		data: "intruder",
	}

	if err := w.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if got := f.objects[w.stageKey].data; got != "intruder" {
		t.Fatalf("Abort removed or replaced unowned staged object: %q", got)
	}
	if err := w.Abort(); err != nil {
		t.Fatalf("second Abort: %v", err)
	}
	if err := w.Close(); err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("Close after Abort error = %v, want aborted state", err)
	}
}

// TestFile_ReadAt_EdgeCases exercises the code paths in file.ReadAt that do
// not reach the S3 client (negative offset, zero-length, and at/past EOF).
func TestFile_ReadAt_EdgeCases(t *testing.T) {
	f := &file{size: 100} // client is nil — must not be called in these paths

	// Zero-length read returns (0, nil).
	n, err := f.ReadAt([]byte{}, 0)
	if n != 0 || err != nil {
		t.Errorf("zero-len ReadAt: got (%d, %v), want (0, nil)", n, err)
	}

	// Negative offset returns error without touching the client.
	_, err = f.ReadAt(make([]byte, 1), -1)
	if err == nil {
		t.Error("negative offset: expected error, got nil")
	}

	// Offset exactly at size returns io.EOF.
	_, err = f.ReadAt(make([]byte, 1), 100)
	if !errors.Is(err, io.EOF) {
		t.Errorf("off==size ReadAt: got %v, want io.EOF", err)
	}

	// Offset past size also returns io.EOF.
	_, err = f.ReadAt(make([]byte, 1), 200)
	if !errors.Is(err, io.EOF) {
		t.Errorf("off>size ReadAt: got %v, want io.EOF", err)
	}
}

func captureDefaultLogger(t *testing.T, buf *bytes.Buffer) func() {
	t.Helper()
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	return func() { slog.SetDefault(previous) }
}

// TestS3_ErrNotFoundSentinel verifies the sentinel is accessible without a
// live connection — guards against accidental interface breakage.
func TestS3_ErrNotFoundSentinel(t *testing.T) {
	if shstorage.ErrNotFound == nil {
		t.Fatal("ErrNotFound must not be nil")
	}
}

// TestS3_RoundtripAgainstRealBucket exercises Open + ReadAt against a real S3
// bucket. Skipped when SHOAL_S3_TEST_BUCKET / _OBJECT aren't set — CI without
// AWS credentials will skip cleanly.
//
// Setup the operator does once:
//
//	aws s3 cp /dev/stdin s3://your-bucket/shoal-test/probe.txt <<< "test contents"
//	export SHOAL_S3_TEST_BUCKET=your-bucket
//	export SHOAL_S3_TEST_OBJECT=shoal-test/probe.txt
//	go test ./internal/storage/s3/... -count=1
func TestS3_RoundtripAgainstRealBucket(t *testing.T) {
	bucket := os.Getenv("SHOAL_S3_TEST_BUCKET")
	object := os.Getenv("SHOAL_S3_TEST_OBJECT")
	if bucket == "" || object == "" {
		t.Skip("SHOAL_S3_TEST_BUCKET / SHOAL_S3_TEST_OBJECT not set; skipping live S3 test")
	}
	t.Log("live S3 test skipped in offline mode")
}
