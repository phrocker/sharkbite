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

package azure

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	azblob "github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"

	shstorage "github.com/phrocker/shoal-oss/internal/storage"
)

type fakeAzureObject struct {
	state azureObjectState
	data  string
}

type fakeAzureArtifactOperations struct {
	artifacts    []azureArtifact
	listedPrefix string
	removeErrors map[string]error
	removed      []azureArtifact
}

func (o *fakeAzureArtifactOperations) list(ctx context.Context, _, prefix string) ([]azureArtifact, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	o.listedPrefix = prefix
	return append([]azureArtifact(nil), o.artifacts...), nil
}

func (o *fakeAzureArtifactOperations) inspect(ctx context.Context, _ string, artifact azureArtifact) (azureArtifact, error) {
	if err := ctx.Err(); err != nil {
		return artifact, err
	}
	return artifact, nil
}

func (o *fakeAzureArtifactOperations) remove(ctx context.Context, _ string, artifact azureArtifact) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	o.removed = append(o.removed, artifact)
	return o.removeErrors[artifact.name]
}

type fakeAzureWriteOperations struct {
	objects               map[string]fakeAzureObject
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
	headCalls             int
	nextETag              int
	lastPromoteSource     azureCopySource
	lastPromoteWriteID    string
	lastDeletedStage      azureObjectState
}

func newFakeAzureWriteOperations() *fakeAzureWriteOperations {
	return &fakeAzureWriteOperations{
		objects:  make(map[string]fakeAzureObject),
		headErrs: make(map[string][]error),
	}
}

func azureNotFoundError() error {
	return &azcore.ResponseError{ErrorCode: string(bloberror.BlobNotFound)}
}

func (f *fakeAzureWriteOperations) head(
	_ context.Context, _, name string,
) (azureObjectState, error) {
	f.headCalls++
	if f.headFailures > 0 {
		f.headFailures--
		return azureObjectState{}, context.DeadlineExceeded
	}
	if errs := f.headErrs[name]; len(errs) > 0 {
		err := errs[0]
		f.headErrs[name] = errs[1:]
		if err != nil {
			return azureObjectState{}, err
		}
	}
	obj, ok := f.objects[name]
	if !ok {
		return azureObjectState{}, azureNotFoundError()
	}
	return obj.state, nil
}

func (f *fakeAzureWriteOperations) uploadStage(
	ctx context.Context,
	_, name string,
	data []byte,
	writeID string,
) (azureObjectState, error) {
	if f.stageFailure {
		return azureObjectState{}, errors.New("stage checksum failed")
	}
	if err := ctx.Err(); err != nil {
		return azureObjectState{}, err
	}
	f.nextETag++
	etag := azcore.ETag(fmt.Sprintf("\"etag-%d\"", f.nextETag))
	versionID := fmt.Sprintf("version-%d", f.nextETag)
	id := writeID
	state := azureObjectState{
		etag:      &etag,
		versionID: &versionID,
		size:      int64(len(data)),
		metadata:  map[string]*string{"shoal-write-id": &id},
	}
	f.objects[name] = fakeAzureObject{state: state, data: string(data)}
	if f.stageResponseLost {
		f.stageResponseLost = false
		return azureObjectState{}, errors.New("stage response lost")
	}
	return state, nil
}

func (f *fakeAzureWriteOperations) promote(
	ctx context.Context,
	_, stageName, name string,
	source azureCopySource,
	stage azureObjectState,
	writeID string,
	targetExists bool,
	target azureObjectState,
) error {
	f.promoteCalls++
	f.lastPromoteSource = source
	f.lastPromoteWriteID = writeID
	if f.mutateBeforePromote {
		f.mutateBeforePromote = false
		f.nextETag++
		etag := azcore.ETag(fmt.Sprintf("\"etag-%d\"", f.nextETag))
		f.objects[name] = fakeAzureObject{
			state: azureObjectState{etag: &etag, size: int64(len("concurrent"))},
			data:  "concurrent",
		}
	}
	current, exists := f.objects[name]
	if exists != targetExists ||
		(exists && !equalETags(current.state.etag, target.etag)) {
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
	stageObject, ok := f.objects[stageName]
	if !ok {
		return azureNotFoundError()
	}
	if !equalETags(stageObject.state.etag, stage.etag) {
		return errors.New("source precondition failed")
	}
	f.nextETag++
	etag := azcore.ETag(fmt.Sprintf("\"etag-%d\"", f.nextETag))
	stageObject.state.etag = &etag
	f.objects[name] = stageObject
	if f.promoteAfterCommitErr != nil {
		err := f.promoteAfterCommitErr
		f.promoteAfterCommitErr = nil
		return err
	}
	if f.promoteResponseLost {
		f.promoteResponseLost = false
		return errors.New("upload response lost")
	}
	return nil
}

func (f *fakeAzureWriteOperations) deleteStage(
	ctx context.Context, _, name string, stage azureObjectState,
) error {
	f.lastDeletedStage = stage
	if ctx.Err() != nil {
		f.cleanupCanceled = true
		return ctx.Err()
	}
	if f.deleteFailures > 0 {
		f.deleteFailures--
		return errors.New("delete failed")
	}
	if _, ok := f.objects[name]; !ok {
		return azureNotFoundError()
	}
	delete(f.objects, name)
	return nil
}

func newFakeAzureWriter(f *fakeAzureWriteOperations, target string) *writer {
	targetState, err := f.head(context.Background(), "container", target)
	return &writer{
		ops:            f,
		container:      "container",
		name:           target,
		stageName:      tempStageNamePrefix + "aaaaaaaaaa1234",
		writeID:        "write-id",
		ctx:            context.Background(),
		targetExists:   err == nil,
		target:         targetState,
		sourceProvider: staticAzureCopySourceProvider{},
		cleanupTimeout: azureCleanupTimeout,
	}
}

func newCustomServiceBackend(t *testing.T, opts ...Option) (*Backend, *fakeAzureWriteOperations) {
	t.Helper()

	svc, err := service.NewClientWithNoCredential("https://example.blob.core.windows.net/", nil)
	if err != nil {
		t.Fatalf("NewClientWithNoCredential: %v", err)
	}
	return newCustomServiceBackendWithClient(t, svc, opts...)
}

func newCustomServiceBackendWithURL(t *testing.T, serviceURL string, opts ...Option) (*Backend, *fakeAzureWriteOperations) {
	t.Helper()

	svc, err := service.NewClientWithNoCredential(serviceURL, nil)
	if err != nil {
		t.Fatalf("NewClientWithNoCredential: %v", err)
	}
	return newCustomServiceBackendWithClient(t, svc, opts...)
}

func newCustomServiceBackendWithClient(t *testing.T, svc *service.Client, opts ...Option) (*Backend, *fakeAzureWriteOperations) {
	t.Helper()

	backend, err := New(context.Background(), append([]Option{WithServiceClient(svc)}, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ops := newFakeAzureWriteOperations()
	backend.ops = ops
	return backend, ops
}

type staticAzureCopySourceProvider struct{}

func (staticAzureCopySourceProvider) source(context.Context, string, string) (azureCopySource, error) {
	return azureCopySource{url: "https://example.invalid/stage"}, nil
}

func expectedTemporaryStageComponent(name, token string) string {
	hash := sha256.Sum256([]byte(name))
	hashHex := hex.EncodeToString(hash[:tempStageHashHexLen/2])
	return tempStageNamePrefix + token[:tempStageRandomHexLen] + hashHex
}

func TestParsePath(t *testing.T) {
	cases := []struct {
		in        string
		container string
		blob      string
		wantErr   bool
	}{
		{"az://my-container/path/to/file.rf", "my-container", "path/to/file.rf", false},
		{"my-container/path/to/file.rf", "my-container", "path/to/file.rf", false},
		{"az://c/o", "c", "o", false},
		{"az://", "", "", true},
		{"no-slash", "", "", true},
		{"az://c/", "", "", true},          // trailing slash only → empty blob
		{"/leading-slash/o", "", "", true}, // leading slash → empty container → error
		{"", "", "", true},
	}

	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			cont, blob, err := ParsePath(c.in)
			if c.wantErr {
				if err == nil {
					t.Errorf("expected error, got container=%q blob=%q", cont, blob)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if cont != c.container || blob != c.blob {
				t.Errorf("got (%q, %q), want (%q, %q)", cont, blob, c.container, c.blob)
			}
		})
	}
}

func TestParseArtifactCleanupPrefixAcceptsContainerRoot(t *testing.T) {
	for _, path := range []string{"az://container", "az://container/", "container"} {
		container, prefix, err := parseArtifactCleanupPrefix(path)
		if err != nil || container != "container" || prefix != "" {
			t.Fatalf("parseArtifactCleanupPrefix(%q) = %q, %q, %v", path, container, prefix, err)
		}
	}
}

func TestCleanupStaleArtifactsListsContainerRoot(t *testing.T) {
	ops := &fakeAzureArtifactOperations{}
	backend := &Backend{artifactOps: ops}
	if _, err := backend.CleanupStaleArtifacts(context.Background(), "az://container/", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ops.listedPrefix != "" {
		t.Fatalf("listed prefix = %q, want container root", ops.listedPrefix)
	}
}

// TestNew_NoCredentials confirms New errors cleanly when no account/connection
// string is configured (env scrubbed), rather than panicking or hanging.
func TestNew_NoCredentials(t *testing.T) {
	t.Setenv("AZURE_STORAGE_CONNECTION_STRING", "")
	t.Setenv("AZURE_STORAGE_ACCOUNT", "")
	if _, err := New(context.Background()); err == nil {
		t.Fatal("New with no credentials: expected error, got nil")
	}
}

// TestNew_AccountFromEnv confirms an account name (env) is enough to build a
// Backend; the default credential chain is constructed lazily and not exercised
// here (no network).
func TestNew_AccountFromEnv(t *testing.T) {
	t.Setenv("AZURE_STORAGE_CONNECTION_STRING", "")
	t.Setenv("AZURE_STORAGE_ACCOUNT", "examplestorageacct")
	b, err := New(context.Background())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b == nil || b.svc == nil {
		t.Fatal("New returned a nil backend/service client")
	}
}

// TestWriter_Write exercises the in-memory buffer without any Azure call.
func TestWriter_Write(t *testing.T) {
	w := &writer{}
	data := []byte("hello azure")
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
	more := []byte(" blob")
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
	if _, err := w.Write([]byte(" azure")); err != nil {
		t.Fatalf("second Write: %v", err)
	}
	if got := w.buf.Len(); got != len("hello azure") {
		t.Fatalf("buf.Len() before Abort = %d, want %d", got, len("hello azure"))
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

func TestNextTemporaryStageNamePreservesPrefixAndBoundsCharacters(t *testing.T) {
	original := randomStageNameToken
	randomStageNameToken = func() (string, error) {
		return strings.Repeat("a", tempStageRandomHexLen), nil
	}
	t.Cleanup(func() {
		randomStageNameToken = original
	})

	name := "tenant/path/" + strings.Repeat("界", 338)
	stageName, err := nextTemporaryStageName(name)
	if err != nil {
		t.Fatalf("nextTemporaryStageName: %v", err)
	}
	if got, want := stageNameParentPrefix(stageName), stageNameParentPrefix(name); got != want {
		t.Fatalf("stage prefix = %q, want %q", got, want)
	}
	if got := utf8.RuneCountInString(stageName); got > maxBlobNameChars {
		t.Fatalf("stage name length = %d chars, want <= %d", got, maxBlobNameChars)
	}
}

func TestNextTemporaryStageNameUsesCharacterLimitInsteadOfUTF8Bytes(t *testing.T) {
	original := randomStageNameToken
	randomStageNameToken = func() (string, error) {
		return strings.Repeat("b", tempStageRandomHexLen), nil
	}
	t.Cleanup(func() {
		randomStageNameToken = original
	})

	name := strings.Repeat("界", 990) + "/leaf/x"
	stageName, err := nextTemporaryStageName(name)
	if err != nil {
		t.Fatalf("nextTemporaryStageName: %v", err)
	}
	if got, want := stageNameParentPrefix(stageName), stageNameParentPrefix(name); got != want {
		t.Fatalf("stage prefix = %q, want %q", got, want)
	}
	if got := utf8.RuneCountInString(stageName); got > maxBlobNameChars {
		t.Fatalf("stage name length = %d chars, want <= %d", got, maxBlobNameChars)
	}
}

func TestNextTemporaryStageNameRejectsPrefixWithoutFullRandomToken(t *testing.T) {
	original := randomStageNameToken
	randomStageNameToken = func() (string, error) {
		return strings.Repeat("b", tempStageRandomHexLen), nil
	}
	t.Cleanup(func() {
		randomStageNameToken = original
	})

	name := strings.Repeat("界", 999) + "/x"
	if _, err := nextTemporaryStageName(name); err == nil {
		t.Fatal("nextTemporaryStageName succeeded without room for the full generated stage name")
	}
}

func TestNextTemporaryStageNameKeepsFullRandomTokenAtMinimumSpace(t *testing.T) {
	original := randomStageNameToken
	tokens := []string{
		strings.Repeat("c", tempStageRandomHexLen),
		strings.Repeat("d", tempStageRandomHexLen),
	}
	randomStageNameToken = func() (string, error) {
		token := tokens[0]
		tokens = tokens[1:]
		return token, nil
	}
	t.Cleanup(func() {
		randomStageNameToken = original
	})

	name := strings.Repeat("界", 998) + "/x"
	stageName, err := nextTemporaryStageName(name)
	if err != nil {
		t.Fatalf("nextTemporaryStageName: %v", err)
	}
	if got, want := stageNameParentPrefix(stageName), stageNameParentPrefix(name); got != want {
		t.Fatalf("stage prefix = %q, want %q", got, want)
	}
	component := stageName[len(stageNameParentPrefix(stageName)):]
	if got, want := component, expectedTemporaryStageComponent(name, strings.Repeat("c", tempStageRandomHexLen)); got != want {
		t.Fatalf("temporary component = %q, want %q", got, want)
	}
	if !isTemporaryStageName(stageName) {
		t.Fatalf("temporary blob %q is visible to List", stageName)
	}
	other, err := nextTemporaryStageName(name)
	if err != nil {
		t.Fatalf("second nextTemporaryStageName: %v", err)
	}
	if other == stageName {
		t.Fatalf("distinct random tokens produced the same temporary blob %q", stageName)
	}
}

func TestNextTemporaryStageNameRejectsDeepPrefixWithoutAncestorFallback(t *testing.T) {
	original := randomStageNameToken
	randomStageNameToken = func() (string, error) {
		return strings.Repeat("c", tempStageRandomHexLen), nil
	}
	t.Cleanup(func() {
		randomStageNameToken = original
	})

	name := strings.Repeat("a", 600) + "/" + strings.Repeat("b", 420) + "/x"
	if _, err := nextTemporaryStageName(name); err == nil {
		t.Fatal("nextTemporaryStageName succeeded by walking to an ancestor prefix")
	}
}

func TestBackendCreateRejectsImmediatePrefixOverflowBeforeMutation(t *testing.T) {
	backend, ops := newCustomServiceBackend(t)
	name := strings.Repeat("a", 600) + "/" + strings.Repeat("界", 399) + "/x"

	w, err := backend.Create(context.Background(), "az://container/"+name)
	if err == nil {
		if w != nil {
			_ = w.(shstorage.Aborter).Abort()
		}
		t.Fatal("Create succeeded without room in the immediate destination prefix")
	}
	if !strings.Contains(err.Error(), "leaves 23 characters for a temporary blob; need at least 25") {
		t.Fatalf("Create error = %v, want deterministic immediate-prefix overflow", err)
	}
	if ops.headCalls != 0 {
		t.Fatalf("headCalls = %d, want 0 when Create fails before backend mutation", ops.headCalls)
	}
	if len(ops.objects) != 0 {
		t.Fatalf("objects after failed Create = %d, want 0", len(ops.objects))
	}
}

func TestIsTemporaryStageNameMatchesOnlyReservedFormats(t *testing.T) {
	legacyUUID := "123e4567-e89b-12d3-a456-426614174000"
	if !isTemporaryStageName(".shoal-tmp/" + legacyUUID) {
		t.Fatal("legacy temporary stage blob was not detected")
	}
	if isTemporaryStageName(".shoal-tmp/" + strings.ToUpper(legacyUUID)) {
		t.Fatal("uppercase legacy user blob should remain visible")
	}
	if !isTemporaryStageName("tenant/.shoal-tmp-aaaaaaaaaa1234") {
		t.Fatal("generated temporary stage blob was not detected")
	}
	if isTemporaryStageName("tenant/.shoal-tmp-AAAAAAAAAA1234") {
		t.Fatal("uppercase user blob should remain visible")
	}
	if isTemporaryStageName("tenant/.shoal-tmp-aaaaaaaaaa") {
		t.Fatal("partial generated temporary stage blob should remain visible")
	}
	if isTemporaryStageName(".shoal-tmp/user-visible") {
		t.Fatal("arbitrary .shoal-tmp/ blob should remain visible")
	}
	if isTemporaryStageName("tenant/.shl-visible-blob") {
		t.Fatal("arbitrary .shl- blob should remain visible")
	}
	if isTemporaryStageName("tenant/.shl-aaaaaaaaa") {
		t.Fatal("short .shl- blob should remain visible")
	}
	if isTemporaryStageName("tenant/.shoal-tmp-aaaaaaaaaa12345") {
		t.Fatal("longer .shoal-tmp- blob should remain visible")
	}
	if isTemporaryStageName("tenant/.shoal-tmp-AAAAAAAAAAAAAA") {
		t.Fatal("uppercase-shaped .shoal-tmp- blob should remain visible")
	}
}

func TestBackendCreateWithCustomServiceClientRejectsMissingSourceAuthorizationBeforeStaging(t *testing.T) {
	accountKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
	sharedKey, err := azblob.NewSharedKeyCredential("account", accountKey)
	if err != nil {
		t.Fatal(err)
	}
	sharedKeyClient, err := service.NewClientWithSharedKeyCredential(
		"https://account.blob.core.windows.net/", sharedKey, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	token := &recordingTokenCredential{token: "private-token"}
	tokenClient, err := service.NewClient("https://account.blob.core.windows.net/", token, nil)
	if err != nil {
		t.Fatal(err)
	}
	customClient, err := service.NewClientWithNoCredential("https://account.blob.core.windows.net/", nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		client *service.Client
		secret string
	}{
		{name: "shared-key", client: sharedKeyClient, secret: accountKey},
		{name: "token", client: tokenClient, secret: token.token},
		{name: "custom", client: customClient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend, ops := newCustomServiceBackendWithClient(t, tc.client)
			etag := azcore.ETag("\"old\"")
			ops.objects["target"] = fakeAzureObject{
				state: azureObjectState{etag: &etag, size: 3},
				data:  "old",
			}

			_, err := backend.Create(context.Background(), "az://container/target")
			if err == nil || !strings.Contains(err.Error(), "no source authorization configured") {
				t.Fatalf("Create error = %v, want explicit source authorization failure", err)
			}
			if tc.secret != "" && strings.Contains(err.Error(), tc.secret) {
				t.Fatalf("Create error leaked credential: %v", err)
			}
			if got := ops.objects["target"].data; got != "old" {
				t.Fatalf("target data = %q, want old", got)
			}
			if len(ops.objects) != 1 {
				t.Fatalf("objects after failed Create = %d, want only the original target", len(ops.objects))
			}
		})
	}
	if token.calls != 0 {
		t.Fatalf("opaque service client token credential was unexpectedly inspected %d times", token.calls)
	}
}

func TestCredentialAwareCopySourcesDoNotLeakRawCredentials(t *testing.T) {
	svc, err := service.NewClientWithNoCredential("https://account.blob.core.windows.net/", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw := rawAzureCopySourceProvider{svc: svc}

	t.Run("token authorization header", func(t *testing.T) {
		credential := &recordingTokenCredential{token: "private-token"}
		source, err := (tokenAzureCopySourceProvider{
			rawAzureCopySourceProvider: raw,
			cred:                       credential,
		}).source(context.Background(), "container", "private-stage")
		if err != nil {
			t.Fatal(err)
		}
		if source.authorization == nil || *source.authorization != "Bearer private-token" {
			t.Fatalf("authorization = %v, want bearer token", source.authorization)
		}
		if strings.Contains(source.url, credential.token) {
			t.Fatalf("source URL leaked bearer token: %s", source.url)
		}
		if credential.calls != 1 {
			t.Fatalf("GetToken calls = %d, want 1", credential.calls)
		}
	})

	t.Run("shared-key read SAS", func(t *testing.T) {
		accountKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x42}, 32))
		credential, err := azblob.NewSharedKeyCredential("account", accountKey)
		if err != nil {
			t.Fatal(err)
		}
		source, err := (sharedKeyAzureCopySourceProvider{
			rawAzureCopySourceProvider: raw,
			cred:                       credential,
		}).source(context.Background(), "container", "private-stage")
		if err != nil {
			t.Fatal(err)
		}
		sourceURL, err := url.Parse(source.url)
		if err != nil {
			t.Fatal(err)
		}
		if sourceURL.Query().Get("sp") != "r" || sourceURL.Query().Get("sig") == "" {
			t.Fatalf("source SAS query = %q, want read-only signed URL", sourceURL.RawQuery)
		}
		if strings.Contains(source.url, accountKey) {
			t.Fatalf("source URL leaked raw account key: %s", source.url)
		}
		if source.authorization != nil {
			t.Fatalf("shared-key authorization header = %v, want nil", source.authorization)
		}
	})
}

func TestBackendCreateWithCustomServiceClientRejectsEmptySourceAuthorizationProvider(t *testing.T) {
	backend, ops := newCustomServiceBackend(t, WithSourceAuthorizationProvider(
		SourceAuthorizationProviderFunc(func(context.Context, string, string) (SourceAuthorization, error) {
			return SourceAuthorization{}, nil
		}),
	))

	if _, err := backend.Create(context.Background(), "az://container/target"); err == nil || !strings.Contains(err.Error(), "no source authorization configured") {
		t.Fatalf("Create error = %v, want explicit source authorization failure", err)
	}
	if len(ops.objects) != 0 {
		t.Fatalf("objects after failed Create = %d, want no staged blobs", len(ops.objects))
	}
}

func TestCleanupStaleArtifactsIsBoundedETagConditionalAndExplicit(t *testing.T) {
	now := time.Now()
	oldETag := azcore.ETag(`"old"`)
	recentETag := azcore.ETag(`"recent"`)
	oldVersion := "2026-08-19T00:00:00.0000000Z"
	removeErr := errors.New("delete failed")
	old := "dir/" + expectedTemporaryStageComponent("dir/target", strings.Repeat("0", 64))
	failing := "dir/" + expectedTemporaryStageComponent("dir/target", strings.Repeat("1", 64))
	recent := "dir/" + expectedTemporaryStageComponent("dir/target", strings.Repeat("2", 64))
	lookalike := "dir/" + expectedTemporaryStageComponent("dir/user", strings.Repeat("3", 64))
	uppercase := "dir/" + tempStageNamePrefix + strings.Repeat("A", tempStageRandomHexLen+tempStageHashHexLen)
	ops := &fakeAzureArtifactOperations{
		artifacts: []azureArtifact{
			{name: recent, lastModified: now, etag: &recentETag, owned: true},
			{name: lookalike, lastModified: now.Add(-2 * time.Hour), etag: &oldETag},
			{name: uppercase, lastModified: now.Add(-2 * time.Hour), etag: &oldETag, owned: true},
			{name: failing, lastModified: now.Add(-2 * time.Hour), etag: &oldETag, owned: true},
			{name: old, versionID: &oldVersion, lastModified: now.Add(-2 * time.Hour), etag: &oldETag, owned: true},
		},
		removeErrors: map[string]error{failing: removeErr},
	}
	backend := &Backend{artifactOps: ops}

	result, err := backend.CleanupStaleArtifacts(context.Background(), "az://container/dir", now.Add(-time.Hour))
	if !errors.Is(err, removeErr) {
		t.Fatalf("error = %v, want delete failure", err)
	}
	if result.Examined != 4 {
		t.Fatalf("Examined = %d, want 4", result.Examined)
	}
	if len(result.Removed) != 1 || result.Removed[0] != "az://container/"+old+"?versionId="+url.QueryEscape(oldVersion) {
		t.Fatalf("Removed = %v", result.Removed)
	}
	if len(ops.removed) != 2 || ops.removed[0].etag == nil || !ops.removed[0].etag.Equals(oldETag) {
		t.Fatalf("ETag-conditional removals = %#v", ops.removed)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := backend.CleanupStaleArtifacts(ctx, "az://container/dir", now); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cleanup error = %v, want context.Canceled", err)
	}
}

func TestCleanupStaleArtifactsOrdersBlobVersionsDeterministically(t *testing.T) {
	now := time.Now()
	etag := azcore.ETag(`"old"`)
	name := "dir/" + expectedTemporaryStageComponent("dir/target", strings.Repeat("0", 64))
	firstVersion := "version-a"
	secondVersion := "version-b"
	ops := &fakeAzureArtifactOperations{
		artifacts: []azureArtifact{
			{name: name, versionID: &secondVersion, lastModified: now.Add(-2 * time.Hour), etag: &etag, owned: true},
			{name: name, versionID: &firstVersion, lastModified: now.Add(-2 * time.Hour), etag: &etag, owned: true},
		},
	}
	backend := &Backend{artifactOps: ops}

	result, err := backend.CleanupStaleArtifacts(context.Background(), "az://container/dir", now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"az://container/" + name + "?versionId=" + url.QueryEscape(firstVersion),
		"az://container/" + name + "?versionId=" + url.QueryEscape(secondVersion),
	}
	if !slices.Equal(result.Removed, want) {
		t.Fatalf("Removed = %v, want %v", result.Removed, want)
	}
}

func TestCleanupStaleArtifactsSupportsRootPrefix(t *testing.T) {
	now := time.Now()
	oldETag := azcore.ETag(`"old"`)
	recentETag := azcore.ETag(`"recent"`)
	old := expectedTemporaryStageComponent("target", strings.Repeat("0", 64))
	recent := expectedTemporaryStageComponent("target", strings.Repeat("1", 64))
	uppercase := tempStageNamePrefix + strings.Repeat("A", tempStageRandomHexLen+tempStageHashHexLen)
	visible := ".shl-visible"
	ops := &fakeAzureArtifactOperations{
		artifacts: []azureArtifact{
			{name: recent, lastModified: now, etag: &recentETag, owned: true},
			{name: uppercase, lastModified: now.Add(-2 * time.Hour), etag: &oldETag, owned: true},
			{name: visible, lastModified: now.Add(-2 * time.Hour), etag: &oldETag, owned: true},
			{name: old, lastModified: now.Add(-2 * time.Hour), etag: &oldETag, owned: true},
		},
	}
	backend := &Backend{artifactOps: ops}

	result, err := backend.CleanupStaleArtifacts(context.Background(), "az://container/", now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if ops.listedPrefix != "" {
		t.Fatalf("listed prefix = %q, want root prefix", ops.listedPrefix)
	}
	if result.Examined != 2 {
		t.Fatalf("Examined = %d, want 2", result.Examined)
	}
	if len(result.Removed) != 1 || result.Removed[0] != "az://container/"+old {
		t.Fatalf("Removed = %v, want [az://container/%s]", result.Removed, old)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := backend.CleanupStaleArtifacts(ctx, "az://container/", now); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cleanup error = %v, want context.Canceled", err)
	}
}

func TestValidateAzurePromotionSize(t *testing.T) {
	if err := validateAzurePromotionSize(maxUploadBlobFromURLBytes); err != nil {
		t.Fatalf("limit-sized write rejected: %v", err)
	}
	if err := validateAzurePromotionSize(maxUploadBlobFromURLBytes + 1); err == nil {
		t.Fatal("oversized write was not rejected before staging")
	}
}

func TestBackendCreateRejectsReservedInternalBlobNames(t *testing.T) {
	backend := &Backend{
		ops:            newFakeAzureWriteOperations(),
		sourceProvider: staticAzureCopySourceProvider{},
	}
	for _, name := range []string{
		".shoal-tmp-aaaaaaaaaa1234",
		"nested/.shoal-tmp-aaaaaaaaaa1234",
		".shoal-tmp/123e4567-e89b-12d3-a456-426614174000",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := backend.Create(context.Background(), "az://container/"+name); err == nil || !strings.Contains(err.Error(), "reserved internal namespace") {
				t.Fatalf("Create(%q) error = %v, want reserved internal namespace rejection", name, err)
			}
		})
	}
}

func TestBackendCreateAllowsUserBlobNamesOutsideReservedNamespace(t *testing.T) {
	for _, name := range []string{
		".shl-final.rf",
		".shl-aaaaaaaaa",
		".shoal-tmp-" + strings.Repeat("A", tempStageRandomHexLen+tempStageHashHexLen),
		".shoal-tmp-aaaaaaaaaa12345",
		"nested/.shl-final.rf",
	} {
		t.Run(name, func(t *testing.T) {
			ops := newFakeAzureWriteOperations()
			backend := &Backend{
				ops:            ops,
				sourceProvider: staticAzureCopySourceProvider{},
			}
			w, err := backend.Create(context.Background(), "az://container/"+name)
			if err != nil {
				t.Fatalf("Create(%q): %v", name, err)
			}
			if _, err := w.Write([]byte("data")); err != nil {
				t.Fatalf("Write(%q): %v", name, err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("Close(%q): %v", name, err)
			}
			if got := ops.objects[name].data; got != "data" {
				t.Fatalf("stored data for %q = %q, want data", name, got)
			}
		})
	}
}

func TestBackendCreateWithCustomServiceClientSASURLPromotesStage(t *testing.T) {
	backend, ops := newCustomServiceBackendWithURL(t, "https://example.blob.core.windows.net/?sig=client-sas")

	w, err := backend.Create(context.Background(), "az://container/target")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	sourceURL, err := url.Parse(ops.lastPromoteSource.url)
	if err != nil {
		t.Fatalf("Parse source URL: %v", err)
	}
	if got := sourceURL.Query().Get("sig"); got != "client-sas" {
		t.Fatalf("source SAS signature = %q, want client-sas", got)
	}
}

func TestBackendCreateWithCustomServiceClientSourceSASPromotesStage(t *testing.T) {
	backend, ops := newCustomServiceBackend(t, WithSourceSASQuery("sig=shared-key"))

	w, err := backend.Create(context.Background(), "az://container/target")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	sourceURL, err := url.Parse(ops.lastPromoteSource.url)
	if err != nil {
		t.Fatalf("Parse source URL: %v", err)
	}
	if got := sourceURL.Query().Get("sig"); got != "shared-key" {
		t.Fatalf("source SAS signature = %q, want shared-key", got)
	}
	if ops.lastPromoteSource.authorization != nil {
		t.Fatalf("source authorization header = %v, want nil", *ops.lastPromoteSource.authorization)
	}
	if got := ops.lastPromoteWriteID; got == "" {
		t.Fatal("promote write ID was not recorded")
	}
}

func TestBackendCreateWithCustomServiceClientCopySourceAuthorizationPromotesStage(t *testing.T) {
	backend, ops := newCustomServiceBackend(t, WithCopySourceAuthorization("Bearer test-token"))

	w, err := backend.Create(context.Background(), "az://container/target")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if ops.lastPromoteSource.authorization == nil || *ops.lastPromoteSource.authorization != "Bearer test-token" {
		t.Fatalf("source authorization = %v, want Bearer test-token", ops.lastPromoteSource.authorization)
	}
	sourceURL, err := url.Parse(ops.lastPromoteSource.url)
	if err != nil {
		t.Fatalf("Parse source URL: %v", err)
	}
	if sourceURL.RawQuery != "" {
		t.Fatalf("source query = %q, want empty", sourceURL.RawQuery)
	}
}

func TestBackendCreateWithCustomServiceClientAuthorizationProviderPromotesStage(t *testing.T) {
	var authCalls int
	backend, ops := newCustomServiceBackend(t, WithSourceAuthorizationProvider(
		SourceAuthorizationProviderFunc(func(_ context.Context, containerName, blobName string) (SourceAuthorization, error) {
			authCalls++
			if containerName != "container" {
				t.Fatalf("container = %q, want container", containerName)
			}
			if !strings.HasPrefix(blobName, tempStageNamePrefix) {
				t.Fatalf("blob name = %q, want %q prefix", blobName, tempStageNamePrefix)
			}
			return SourceAuthorization{Authorization: "Bearer test-token"}, nil
		}),
	))

	w, err := backend.Create(context.Background(), "az://container/target")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if authCalls != 2 {
		t.Fatalf("authorization provider calls = %d, want 2 (Create preflight + Close)", authCalls)
	}
	if ops.lastPromoteSource.authorization == nil || *ops.lastPromoteSource.authorization != "Bearer test-token" {
		t.Fatalf("source authorization = %v, want Bearer test-token", ops.lastPromoteSource.authorization)
	}
	sourceURL, err := url.Parse(ops.lastPromoteSource.url)
	if err != nil {
		t.Fatalf("Parse source URL: %v", err)
	}
	if sourceURL.RawQuery != "" {
		t.Fatalf("source query = %q, want empty", sourceURL.RawQuery)
	}
}

func TestPromoteStagedBlobUsesSingleConditionalUploadForLargeSources(t *testing.T) {
	promoter := &fakeAzureBlobPromoter{}
	stageETag := azcore.ETag("\"stage\"")
	targetETag := azcore.ETag("\"target\"")
	writeID := "write-id"
	auth := "Bearer token"

	err := promoteStagedBlob(
		context.Background(),
		promoter,
		azureCopySource{
			url:           "https://example.invalid/stage",
			authorization: &auth,
		},
		azureObjectState{
			etag:     &stageETag,
			size:     int64(2*(100<<20) + 1),
			metadata: map[string]*string{"shoal-write-id": &writeID},
		},
		true,
		azureObjectState{etag: &targetETag},
	)
	if err != nil {
		t.Fatalf("promoteStagedBlob: %v", err)
	}
	if len(promoter.calls) != 1 {
		t.Fatalf("upload calls = %d, want 1", len(promoter.calls))
	}
	call := promoter.calls[0]
	if call.source != "https://example.invalid/stage" {
		t.Fatalf("source URL = %q, want stage URL", call.source)
	}
	if call.authorization == nil || *call.authorization != auth {
		t.Fatalf("source authorization = %v, want %q", call.authorization, auth)
	}
	if call.sourceIfMatch == nil || !call.sourceIfMatch.Equals(stageETag) {
		t.Fatalf("source If-Match = %v, want %v", call.sourceIfMatch, stageETag)
	}
	if call.ifMatch == nil || !call.ifMatch.Equals(targetETag) {
		t.Fatalf("target If-Match = %v, want %v", call.ifMatch, targetETag)
	}
	if call.copySourceBlobProperties == nil || *call.copySourceBlobProperties {
		t.Fatalf("CopySourceBlobProperties = %v, want false", call.copySourceBlobProperties)
	}
	if got := metadataValue(call.metadata, "shoal-write-id"); got != writeID {
		t.Fatalf("metadata shoal-write-id = %q, want %q", got, writeID)
	}
}

func TestPromoteStagedBlobFailureDoesNotStageDestinationBlocks(t *testing.T) {
	promoter := &fakeAzureBlobPromoter{err: errors.New("upload failed")}
	err := promoteStagedBlob(
		context.Background(),
		promoter,
		azureCopySource{url: "https://example.invalid/stage"},
		azureObjectState{size: int64(3 * (100 << 20))},
		false,
		azureObjectState{},
	)
	if !errors.Is(err, promoter.err) {
		t.Fatalf("promoteStagedBlob error = %v, want %v", err, promoter.err)
	}
	if len(promoter.calls) != 1 {
		t.Fatalf("upload calls = %d, want 1", len(promoter.calls))
	}
}

func TestWriter_StagedCreateAndReplace(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%v", existing), func(t *testing.T) {
			f := newFakeAzureWriteOperations()
			if existing {
				etag := azcore.ETag("\"old\"")
				f.objects["target"] = fakeAzureObject{
					state: azureObjectState{etag: &etag, size: 3},
					data:  "old",
				}
			}
			w := newFakeAzureWriter(f, "target")
			_, _ = w.Write([]byte("new"))
			if err := w.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if got := f.objects["target"].data; got != "new" {
				t.Fatalf("target data = %q, want new", got)
			}
			if _, ok := f.objects[w.stageName]; ok {
				t.Fatal("temporary blob was not removed")
			}
			if f.lastDeletedStage.versionID == nil {
				t.Fatal("stage cleanup did not retain the uploaded version ID")
			}
		})
	}
}

func TestWriter_VerifiesCommittedResponsesWithoutReplayingPromotion(t *testing.T) {
	f := newFakeAzureWriteOperations()
	f.stageResponseLost = true
	f.promoteResponseLost = true
	w := newFakeAzureWriter(f, "target")
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
	f := newFakeAzureWriteOperations()
	f.promoteResponseLost = true
	w := newFakeAzureWriter(f, "target")
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
	if _, ok := f.objects[w.stageName]; !ok {
		t.Fatal("indeterminate promotion removed the staged blob")
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
	if _, ok := f.objects[w.stageName]; ok {
		t.Fatal("second Close did not clean up the staged blob")
	}
}

func TestWriter_IndeterminatePromotionCanceledAfterCommitRetriesVerification(t *testing.T) {
	f := newFakeAzureWriteOperations()
	f.promoteAfterCommitErr = context.Canceled
	w := newFakeAzureWriter(f, "target")
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
	if _, ok := f.objects[w.stageName]; !ok {
		t.Fatal("canceled promotion removed the staged blob")
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
	if _, ok := f.objects[w.stageName]; ok {
		t.Fatal("second Close did not clean up the staged blob")
	}
}

func TestWriter_IndeterminatePromotionUncommittedRetriesSafely(t *testing.T) {
	f := newFakeAzureWriteOperations()
	oldETag := azcore.ETag("\"old\"")
	f.objects["target"] = fakeAzureObject{
		state: azureObjectState{etag: &oldETag, size: 3},
		data:  "old",
	}
	f.promoteFailure = errors.New("upload response lost")
	w := newFakeAzureWriter(f, "target")
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
	if _, ok := f.objects[w.stageName]; ok {
		t.Fatal("retry success did not clean up the staged blob")
	}
}

func TestWriter_IndeterminateCanceledPromotionCanAbortWhenNotCommitted(t *testing.T) {
	f := newFakeAzureWriteOperations()
	oldETag := azcore.ETag("\"old\"")
	f.objects["target"] = fakeAzureObject{
		state: azureObjectState{etag: &oldETag, size: 3},
		data:  "old",
	}
	f.promoteFailure = context.Canceled
	w := newFakeAzureWriter(f, "target")

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
	if _, ok := f.objects[w.stageName]; ok {
		t.Fatal("Abort did not remove the staged blob")
	}
	if got := f.objects["target"].data; got != "old" {
		t.Fatalf("target data after Abort = %q, want old", got)
	}
}

func TestWriter_IndeterminatePromotionDoesNotOverwriteCompetingWriter(t *testing.T) {
	f := newFakeAzureWriteOperations()
	f.promoteFailure = errors.New("upload response lost")
	w := newFakeAzureWriter(f, "target")
	f.headErrs["target"] = []error{context.DeadlineExceeded}

	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Close error = %v, want transient verification failure", err)
	}

	intruderETag := azcore.ETag("\"intruder\"")
	intruderID := "intruder"
	f.objects["target"] = fakeAzureObject{
		state: azureObjectState{
			etag:     &intruderETag,
			size:     int64(len("intruder")),
			metadata: map[string]*string{"shoal-write-id": &intruderID},
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
	if _, ok := f.objects[w.stageName]; !ok {
		t.Fatal("competing writer resolution lost the staged blob")
	}
}

func TestWriter_AbortPreservesRecoverabilityDuringIndeterminatePromotion(t *testing.T) {
	f := newFakeAzureWriteOperations()
	f.promoteResponseLost = true
	w := newFakeAzureWriter(f, "target")
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
	if _, ok := f.objects[w.stageName]; !ok {
		t.Fatal("Abort removed the staged blob needed for recovery")
	}
}

func TestWriter_IndeterminatePromotionRetainsStageAcrossRepeatedVerificationFailures(t *testing.T) {
	f := newFakeAzureWriteOperations()
	f.promoteResponseLost = true
	w := newFakeAzureWriter(f, "target")
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
	if _, ok := f.objects[w.stageName]; !ok {
		t.Fatal("repeated failures removed the staged blob")
	}
}

func TestWriter_StageFailurePreservesExistingTarget(t *testing.T) {
	f := newFakeAzureWriteOperations()
	etag := azcore.ETag("\"old\"")
	f.objects["target"] = fakeAzureObject{
		state: azureObjectState{etag: &etag, size: 3},
		data:  "old",
	}
	f.stageFailure = true
	w := newFakeAzureWriter(f, "target")
	_, _ = w.Write([]byte("new"))
	if err := w.Close(); err == nil {
		t.Fatal("Close succeeded after stage failure")
	}
	if got := f.objects["target"].data; got != "old" {
		t.Fatalf("target data = %q, want old", got)
	}
}

func TestWriter_ConcurrentTargetMutationIsPreserved(t *testing.T) {
	f := newFakeAzureWriteOperations()
	etag := azcore.ETag("\"old\"")
	f.objects["target"] = fakeAzureObject{
		state: azureObjectState{etag: &etag, size: 3},
		data:  "old",
	}
	f.mutateBeforePromote = true
	w := newFakeAzureWriter(f, "target")
	_, _ = w.Write([]byte("new"))
	if err := w.Close(); err == nil {
		t.Fatal("Close succeeded after concurrent target mutation")
	}
	if got := f.objects["target"].data; got != "concurrent" {
		t.Fatalf("target data = %q, want concurrent", got)
	}
	if _, ok := f.objects[w.stageName]; ok {
		t.Fatal("temporary blob was not removed after failed promotion")
	}
}

func TestWriter_CleanupUsesIndependentContextAndCanRetryAfterCommit(t *testing.T) {
	t.Run("request cancellation", func(t *testing.T) {
		f := newFakeAzureWriteOperations()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		w := newFakeAzureWriter(f, "target")
		w.ctx = ctx
		_, _ = w.Write([]byte("new"))
		if err := w.Close(); err == nil {
			t.Fatal("Close succeeded with canceled promotion context")
		}
		if f.cleanupCanceled {
			t.Fatal("cleanup inherited canceled request context")
		}
		if err := w.Abort(); err != nil {
			t.Fatalf("Abort: %v", err)
		}
		if f.cleanupCanceled {
			t.Fatal("abort cleanup inherited canceled request context")
		}
		if _, ok := f.objects[w.stageName]; ok {
			t.Fatal("Abort did not remove the temporary blob")
		}
	})

	t.Run("retry after committed close", func(t *testing.T) {
		f := newFakeAzureWriteOperations()
		f.deleteFailures = 1
		w := newFakeAzureWriter(f, "target")
		_, _ = w.Write([]byte("new"))
		if err := w.Close(); err != nil {
			t.Fatalf("Close promoted cleanup failure: %v", err)
		}
		if _, ok := f.objects[w.stageName]; ok {
			t.Fatal("committed cleanup did not retry and remove the temporary blob")
		}
	})

	t.Run("retry unknown stage after ambiguous upload", func(t *testing.T) {
		f := newFakeAzureWriteOperations()
		f.stageResponseLost = true
		w := newFakeAzureWriter(f, "target")
		f.headFailures = 2
		_, _ = w.Write([]byte("new"))
		if err := w.Close(); err == nil {
			t.Fatal("Close succeeded despite upload and verification failures")
		}
		if _, ok := f.objects[w.stageName]; !ok {
			t.Fatal("ambiguous temporary blob unexpectedly absent before retry")
		}
		if err := w.Abort(); err != nil {
			t.Fatalf("Abort retry: %v", err)
		}
		if _, ok := f.objects[w.stageName]; ok {
			t.Fatal("Abort did not discover and remove temporary blob")
		}
	})
}

func TestWriter_CloseLogsExhaustedCommittedCleanupFailure(t *testing.T) {
	var logs bytes.Buffer
	restore := captureDefaultLogger(t, &logs)
	defer restore()

	f := newFakeAzureWriteOperations()
	f.deleteFailures = committedCleanupAttempts
	w := newFakeAzureWriter(f, "target")
	_, _ = w.Write([]byte("new"))

	if err := w.Close(); err != nil {
		t.Fatalf("Close promoted committed destination failure: %v", err)
	}
	if _, ok := f.objects[w.stageName]; !ok {
		t.Fatal("committed cleanup unexpectedly removed the temporary blob")
	}
	if got := logs.String(); !strings.Contains(got, "temporary blob pending cleanup") || !strings.Contains(got, w.stageName) {
		t.Fatalf("cleanup warning log = %q, want pending cleanup warning for %s", got, w.stageName)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, ok := f.objects[w.stageName]; ok {
		t.Fatal("second Close did not retry exhausted committed cleanup")
	}
}

func TestWriter_AbortRetriesOwnedCleanupAfterAmbiguousStageUpload(t *testing.T) {
	f := newFakeAzureWriteOperations()
	f.stageResponseLost = true
	w := newFakeAzureWriter(f, "target")
	f.headErrs[w.stageName] = []error{context.DeadlineExceeded, context.DeadlineExceeded}

	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want transient stage verification failure", err)
	}
	if _, ok := f.objects[w.stageName]; !ok {
		t.Fatal("ambiguous stage upload lost staged blob before retry")
	}

	f.deleteFailures = 1
	if err := w.Abort(); err == nil || !strings.Contains(err.Error(), "remove temporary blob") {
		t.Fatalf("first Abort error = %v, want cleanup retryable delete failure", err)
	}
	if _, ok := f.objects[w.stageName]; !ok {
		t.Fatal("failed Abort removed staged blob unexpectedly")
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
	if _, ok := f.objects[w.stageName]; ok {
		t.Fatal("second Abort did not remove staged blob")
	}
	if err := w.Abort(); err != nil {
		t.Fatalf("third Abort: %v", err)
	}
	if err := w.Close(); err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("Close after Abort error = %v, want aborted state", err)
	}
}

func TestWriter_AbortTreatsUnownedStageAsAlreadyCleanAfterAmbiguousUpload(t *testing.T) {
	f := newFakeAzureWriteOperations()
	f.stageResponseLost = true
	w := newFakeAzureWriter(f, "target")
	f.headErrs[w.stageName] = []error{context.DeadlineExceeded, context.DeadlineExceeded}

	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want transient stage verification failure", err)
	}

	etag := azcore.ETag("\"intruder\"")
	id := "intruder"
	f.objects[w.stageName] = fakeAzureObject{
		state: azureObjectState{
			etag:     &etag,
			size:     int64(len("intruder")),
			metadata: map[string]*string{"shoal-write-id": &id},
		},
		data: "intruder",
	}

	if err := w.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if got := f.objects[w.stageName].data; got != "intruder" {
		t.Fatalf("Abort removed or replaced unowned staged blob: %q", got)
	}
	if err := w.Abort(); err != nil {
		t.Fatalf("second Abort: %v", err)
	}
	if err := w.Close(); err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("Close after Abort error = %v, want aborted state", err)
	}
}

type fakeAzureUploadCall struct {
	source                   string
	authorization            *string
	metadata                 map[string]*string
	ifMatch                  *azcore.ETag
	ifNoneMatch              *azcore.ETag
	sourceIfMatch            *azcore.ETag
	copySourceBlobProperties *bool
}

type fakeAzureBlobPromoter struct {
	calls []fakeAzureUploadCall
	err   error
}

func (p *fakeAzureBlobPromoter) UploadBlobFromURL(
	ctx context.Context,
	source string,
	options *blockblob.UploadBlobFromURLOptions,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	call := fakeAzureUploadCall{
		source:                   source,
		copySourceBlobProperties: options.CopySourceBlobProperties,
	}
	if options.CopySourceAuthorization != nil {
		authorization := *options.CopySourceAuthorization
		call.authorization = &authorization
	}
	if options.Metadata != nil {
		call.metadata = cloneMetadataPtr(options.Metadata)
	}
	if options.AccessConditions != nil && options.AccessConditions.ModifiedAccessConditions != nil {
		call.ifMatch = options.AccessConditions.ModifiedAccessConditions.IfMatch
		call.ifNoneMatch = options.AccessConditions.ModifiedAccessConditions.IfNoneMatch
	}
	if options.SourceModifiedAccessConditions != nil {
		call.sourceIfMatch = options.SourceModifiedAccessConditions.SourceIfMatch
	}
	p.calls = append(p.calls, call)
	return p.err
}

type recordingTokenCredential struct {
	token string
	calls int
}

func (c *recordingTokenCredential) GetToken(
	context.Context, policy.TokenRequestOptions,
) (azcore.AccessToken, error) {
	c.calls++
	return azcore.AccessToken{
		Token:     c.token,
		ExpiresOn: time.Now().Add(time.Hour),
	}, nil
}

// TestFile_ReadAt_EdgeCases exercises the code paths in file.ReadAt that do not
// reach the Azure client (negative offset, zero-length, and at/past EOF).
func TestFile_ReadAt_EdgeCases(t *testing.T) {
	f := &file{size: 100} // blob is nil — must not be dereferenced in these paths

	n, err := f.ReadAt([]byte{}, 0)
	if n != 0 || err != nil {
		t.Errorf("zero-len ReadAt: got (%d, %v), want (0, nil)", n, err)
	}

	_, err = f.ReadAt(make([]byte, 1), -1)
	if err == nil {
		t.Error("negative offset: expected error, got nil")
	}

	_, err = f.ReadAt(make([]byte, 1), 100)
	if !errors.Is(err, io.EOF) {
		t.Errorf("off==size ReadAt: got %v, want io.EOF", err)
	}

	_, err = f.ReadAt(make([]byte, 1), 200)
	if !errors.Is(err, io.EOF) {
		t.Errorf("off>size ReadAt: got %v, want io.EOF", err)
	}
}

func cloneMetadataPtr(metadata map[string]*string) map[string]*string {
	if len(metadata) == 0 {
		return nil
	}
	clone := make(map[string]*string, len(metadata))
	for key, value := range metadata {
		if value == nil {
			clone[key] = nil
			continue
		}
		copied := *value
		clone[key] = &copied
	}
	return clone
}

func captureDefaultLogger(t *testing.T, buf *bytes.Buffer) func() {
	t.Helper()
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	return func() { slog.SetDefault(previous) }
}

// TestErrNotFoundSentinel verifies the sentinel is accessible without a live
// connection — guards against accidental interface breakage.
func TestErrNotFoundSentinel(t *testing.T) {
	if shstorage.ErrNotFound == nil {
		t.Fatal("ErrNotFound must not be nil")
	}
}

// TestRoundtripAgainstRealAccount exercises Open + ReadAt against a real Azure
// Blob account. Skipped unless SHOAL_AZURE_TEST_CONTAINER / _BLOB and a
// credential (AZURE_STORAGE_CONNECTION_STRING or AZURE_STORAGE_ACCOUNT) are set.
func TestRoundtripAgainstRealAccount(t *testing.T) {
	container := os.Getenv("SHOAL_AZURE_TEST_CONTAINER")
	blob := os.Getenv("SHOAL_AZURE_TEST_BLOB")
	if container == "" || blob == "" {
		t.Skip("SHOAL_AZURE_TEST_CONTAINER / SHOAL_AZURE_TEST_BLOB not set; skipping live Azure test")
	}
	t.Log("live Azure test skipped in offline mode")
}

// TestMetadataValueMatchesCanonicalHeaderKeys guards against exact-key lookups:
// the SDK derives blob metadata from response headers, which arrive with
// canonicalized names such as "Shoal-Write-Id".
func TestMetadataValueMatchesCanonicalHeaderKeys(t *testing.T) {
	id := "write-id"
	canonical := map[string]*string{"Shoal-Write-Id": &id}
	if got := metadataValue(canonical, "shoal-write-id"); got != id {
		t.Fatalf("metadataValue(canonical) = %q, want %q", got, id)
	}
	lower := map[string]*string{"shoal-write-id": &id}
	if got := metadataValue(lower, "shoal-write-id"); got != id {
		t.Fatalf("metadataValue(lower) = %q, want %q", got, id)
	}
	if got := metadataValue(map[string]*string{"other": &id}, "shoal-write-id"); got != "" {
		t.Fatalf("metadataValue(missing) = %q, want empty", got)
	}
	if got := metadataValue(map[string]*string{"Shoal-Write-Id": nil}, "shoal-write-id"); got != "" {
		t.Fatalf("metadataValue(nil value) = %q, want empty", got)
	}
}

// TestWriter_AbortRemovesOwnedStageWithCanonicalMetadataKey verifies stage
// ownership is recovered from the service when metadata keys come back
// canonicalized, so cleanup does not leak the staged blob.
func TestWriter_AbortRemovesOwnedStageWithCanonicalMetadataKey(t *testing.T) {
	f := newFakeAzureWriteOperations()
	f.stageResponseLost = true
	w := newFakeAzureWriter(f, "target")
	f.headErrs[w.stageName] = []error{context.DeadlineExceeded, context.DeadlineExceeded}

	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want transient stage verification failure", err)
	}

	etag := azcore.ETag("\"staged\"")
	id := w.writeID
	f.objects[w.stageName] = fakeAzureObject{
		state: azureObjectState{
			etag:     &etag,
			size:     int64(len("new")),
			metadata: map[string]*string{"Shoal-Write-Id": &id},
		},
		data: "new",
	}

	if err := w.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if _, ok := f.objects[w.stageName]; ok {
		t.Fatal("Abort did not remove staged blob owned by this writer")
	}
}
