package gcs

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	cloudstorage "cloud.google.com/go/storage"
	shstorage "github.com/phrocker/shoal-oss/internal/storage"
	memorystorage "github.com/phrocker/shoal-oss/internal/storage/memory"
	"google.golang.org/api/googleapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func expectedTemporaryObjectComponent(object, token string) string {
	hash := sha256.Sum256([]byte(object))
	hashHex := hex.EncodeToString(hash[:tempObjectHashHexLen/2])
	return tempObjectPrefix + token[:tempObjectRandomHexLen] + hashHex
}

func TestParsePath(t *testing.T) {
	cases := []struct {
		in      string
		bucket  string
		object  string
		wantErr bool
	}{
		{"gs://my-bucket/path/to/file.rf", "my-bucket", "path/to/file.rf", false},
		{"my-bucket/path/to/file.rf", "my-bucket", "path/to/file.rf", false},
		{"gs://b/o", "b", "o", false},
		{"gs://", "", "", true},
		{"no-slash", "", "", true},
		{"gs://b/", "", "", true},          // empty object
		{"/leading-slash/o", "", "", true}, // leading slash → empty bucket → error
		{"", "", "", true},
	}

	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			b, o, err := ParsePath(c.in)
			if c.wantErr {
				if err == nil {
					t.Errorf("expected error, got bucket=%q object=%q", b, o)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			if b != c.bucket || o != c.object {
				t.Errorf("got (%q, %q), want (%q, %q)", b, o, c.bucket, c.object)
			}
		})
	}
}

func TestParseArtifactCleanupPrefixAcceptsBucketRoot(t *testing.T) {
	for _, path := range []string{"gs://bucket", "gs://bucket/", "bucket"} {
		bucket, prefix, err := parseArtifactCleanupPrefix(path)
		if err != nil || bucket != "bucket" || prefix != "" {
			t.Fatalf("parseArtifactCleanupPrefix(%q) = %q, %q, %v", path, bucket, prefix, err)
		}
	}
}

func TestCleanupStaleArtifactsListsBucketRoot(t *testing.T) {
	ops := &fakeGCSArtifactOperations{}
	backend := &Backend{artifactOps: ops}
	if _, err := backend.CleanupStaleArtifacts(context.Background(), "gs://bucket/", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ops.listedPrefix != "" {
		t.Fatalf("listed prefix = %q, want bucket root", ops.listedPrefix)
	}
}

func TestIsAmbiguousPromotionErrorUsesStructuredPreconditionSignals(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "googleapi 412 conditionNotMet",
			err:  &googleapi.Error{Code: http.StatusPreconditionFailed, Message: "conditionNotMet"},
			want: false,
		},
		{
			name: "wrapped googleapi 412",
			err:  fmt.Errorf("wrapped: %w", &googleapi.Error{Code: http.StatusPreconditionFailed, Message: "pre-conditions mismatch"}),
			want: false,
		},
		{
			name: "grpc failed precondition",
			err:  status.Error(codes.FailedPrecondition, "condition not met"),
			want: false,
		},
		{
			name: "wrapped grpc failed precondition",
			err:  fmt.Errorf("wrapped: %w", status.Error(codes.FailedPrecondition, "conditionNotMet")),
			want: false,
		},
		{
			name: "misleading googleapi text with unknown code stays ambiguous",
			err:  &googleapi.Error{Code: http.StatusInternalServerError, Message: "precondition failed"},
			want: true,
		},
		{
			name: "misleading grpc text with unknown code stays ambiguous",
			err:  status.Error(codes.Aborted, "conditionNotMet"),
			want: true,
		},
		{
			name: "plain text without structured code stays ambiguous",
			err:  errors.New("precondition failed"),
			want: true,
		},
		{
			name: "wrapped not found stays non ambiguous",
			err:  fmt.Errorf("wrapped: %w", cloudstorage.ErrObjectNotExist),
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isAmbiguousPromotionError(tc.err)
			if got != tc.want {
				t.Fatalf("isAmbiguousPromotionError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestWriter_AbortUsesCloseWithErrorAndRejectsLaterUse(t *testing.T) {
	backend, bucket := newFakeBackend()
	w, err := backend.Create(context.Background(), "bucket/path/to/object.rf")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	gcsWriter := w.(*writer)
	inner := gcsWriter.inner.(*fakeObjectWriter)

	if _, err := w.Write([]byte("hello gcs")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := inner.buf.String(); got != "hello gcs" {
		t.Fatalf("inner wrote %q, want %q", got, "hello gcs")
	}

	if err := w.(shstorage.Aborter).Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if inner.closeCalls != 0 {
		t.Fatalf("Close calls = %d, want 0", inner.closeCalls)
	}
	if inner.closeWithErrorCalls != 1 {
		t.Fatalf("CloseWithError calls = %d, want 1", inner.closeWithErrorCalls)
	}
	if inner.closeWithErrorArg == nil || !strings.Contains(inner.closeWithErrorArg.Error(), "gcs: write aborted") {
		t.Fatalf("CloseWithError arg = %v, want gcs abort error", inner.closeWithErrorArg)
	}
	if _, ok := bucket.objects["path/to/object.rf"]; ok {
		t.Fatal("Abort published the destination object")
	}
	if _, ok := bucket.objects[gcsWriter.temp.(*fakeObject).name]; ok {
		t.Fatal("Abort left the temporary object behind")
	}

	if err := w.(shstorage.Aborter).Abort(); err != nil {
		t.Fatalf("second Abort: %v", err)
	}
	if inner.closeWithErrorCalls != 1 {
		t.Fatalf("CloseWithError calls after second Abort = %d, want 1", inner.closeWithErrorCalls)
	}
	if _, err := w.Write([]byte("late")); err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("Write after Abort error = %v, want aborted state", err)
	}
	if err := w.Close(); err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("Close after Abort error = %v, want aborted state", err)
	}
	if inner.closeCalls != 0 {
		t.Fatalf("Close calls after Abort = %d, want 0", inner.closeCalls)
	}
}

func TestWriter_AbortTreatsReturnedAbortCauseAsSuccess(t *testing.T) {
	backend, _ := newFakeBackend()
	w, err := backend.Create(context.Background(), "bucket/path/to/object.rf")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	gcsWriter := w.(*writer)
	gcsWriter.inner.(*fakeObjectWriter).plan.closeAbortFn = func(err error) error { return err }

	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := gcsWriter.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if !gcsWriter.aborted {
		t.Fatal("Abort did not mark the writer aborted")
	}
}

func TestWriter_AbortTreatsWrappedAbortCauseAsSuccess(t *testing.T) {
	backend, _ := newFakeBackend()
	w, err := backend.Create(context.Background(), "bucket/path/to/object.rf")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	gcsWriter := w.(*writer)
	gcsWriter.inner.(*fakeObjectWriter).plan.closeAbortFn = func(err error) error {
		return fmt.Errorf("wrapped abort: %w", err)
	}

	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := gcsWriter.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if !gcsWriter.aborted {
		t.Fatal("Abort did not mark the writer aborted")
	}
}

func TestWriter_AbortPropagatesDistinctCloseWithErrorFailureAndAllowsRetry(t *testing.T) {
	backend, _ := newFakeBackend()
	w, err := backend.Create(context.Background(), "bucket/path/to/object.rf")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	gcsWriter := w.(*writer)
	distinctErr := errors.New("close aborted stream failed")
	gcsWriter.inner.(*fakeObjectWriter).plan.closeAbortErr = distinctErr

	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := gcsWriter.Abort(); !errors.Is(err, distinctErr) {
		t.Fatalf("first Abort error = %v, want %v", err, distinctErr)
	}
	if gcsWriter.aborted {
		t.Fatal("first Abort unexpectedly marked the writer aborted")
	}
	if err := gcsWriter.Abort(); err != nil {
		t.Fatalf("second Abort: %v", err)
	}
	if !gcsWriter.aborted {
		t.Fatal("second Abort did not mark the writer aborted")
	}
}

func TestWriter_AbortRetriesCleanupAfterExpectedCloseWithErrorCause(t *testing.T) {
	backend, bucket := newFakeBackend()
	w, err := backend.Create(context.Background(), "bucket/path/to/object.rf")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	gcsWriter := w.(*writer)
	gcsWriter.inner.(*fakeObjectWriter).plan.closeAbortFn = func(err error) error { return err }
	tempName := gcsWriter.temp.(*fakeObject).name
	bucket.putObjectWithMetadata(tempName, []byte("owned"), map[string]string{"shoal-write-id": gcsWriter.writeID})

	attempts := 0
	bucket.deleteHook = func(_ context.Context, object *fakeObject) error {
		if object.name == tempName && attempts == 0 {
			attempts++
			return errors.New("delete failed")
		}
		return nil
	}

	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	abortErr := gcsWriter.Abort()
	if abortErr == nil || !strings.Contains(abortErr.Error(), "remove temporary object") {
		t.Fatalf("first Abort error = %v, want cleanup failure", abortErr)
	}
	if strings.Contains(abortErr.Error(), errWriteAborted.Error()) {
		t.Fatalf("first Abort error = %v, want cleanup failure without synthetic abort noise", abortErr)
	}
	if gcsWriter.aborted {
		t.Fatal("first Abort unexpectedly marked the writer aborted")
	}
	if _, ok := bucket.objects[tempName]; !ok {
		t.Fatalf("first Abort removed temp object %q unexpectedly", tempName)
	}
	if err := gcsWriter.Abort(); err != nil {
		t.Fatalf("second Abort: %v", err)
	}
	if !gcsWriter.aborted {
		t.Fatal("second Abort did not mark the writer aborted")
	}
	if _, ok := bucket.objects[tempName]; ok {
		t.Fatalf("second Abort did not remove temp object %q", tempName)
	}
}

func TestWriteAllCancellationDoesNotReportSyntheticAbortError(t *testing.T) {
	backend, bucket := newFakeBackend()
	bucket.writerPlan = func(name string) fakeWriterPlan {
		if isTemporaryObjectName(name) {
			return fakeWriterPlan{closeAbortFn: func(err error) error { return err }}
		}
		return fakeWriterPlan{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	err := shstorage.WriteAll(ctx, cancelAfterWriteBackend{WritableBackend: backend, cancel: cancel}, "bucket/path/to/object.rf", []byte("payload"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("WriteAll error = %v, want context canceled", err)
	}
	if strings.Contains(err.Error(), errWriteAborted.Error()) || strings.Contains(err.Error(), "abort temporary object") {
		t.Fatalf("WriteAll error = %v, want no synthetic abort noise", err)
	}
	if _, ok := bucket.objects["path/to/object.rf"]; ok {
		t.Fatal("WriteAll published the destination object")
	}
}

func TestCopyCancellationDoesNotReportSyntheticAbortError(t *testing.T) {
	src := memorystorage.New()
	src.Put("src.rf", []byte("payload"))

	backend, bucket := newFakeBackend()
	bucket.writerPlan = func(name string) fakeWriterPlan {
		if isTemporaryObjectName(name) {
			return fakeWriterPlan{closeAbortFn: func(err error) error { return err }}
		}
		return fakeWriterPlan{}
	}

	ctx, cancel := context.WithCancel(context.Background())
	written, err := shstorage.Copy(ctx, src, "src.rf", cancelAfterWriteBackend{WritableBackend: backend, cancel: cancel}, "bucket/path/to/object.rf")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Copy error = %v, want context canceled", err)
	}
	if strings.Contains(err.Error(), errWriteAborted.Error()) || strings.Contains(err.Error(), "abort temporary object") {
		t.Fatalf("Copy error = %v, want no synthetic abort noise", err)
	}
	if written != int64(len("payload")) {
		t.Fatalf("Copy wrote %d bytes, want %d", written, len("payload"))
	}
	if _, ok := bucket.objects["path/to/object.rf"]; ok {
		t.Fatal("Copy published the destination object")
	}
}

func TestWriter_WriteAfterCloseReportsClosedState(t *testing.T) {
	w := &writer{closed: true}
	if _, err := w.Write([]byte("late")); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("Write after Close error = %v, want closed state", err)
	}
}

func TestNextTemporaryObjectNamePreservesPrefixAndBoundsUTF8Bytes(t *testing.T) {
	originalToken := randomTempObjectToken
	randomTempObjectToken = func() (string, error) {
		return strings.Repeat("a", tempObjectRandomHexLen), nil
	}
	t.Cleanup(func() {
		randomTempObjectToken = originalToken
	})

	object := "tenant/path/" + strings.Repeat("界", 338)
	tempName, err := nextTemporaryObjectName(object)
	if err != nil {
		t.Fatalf("nextTemporaryObjectName: %v", err)
	}
	if got, want := tempObjectParentPrefix(tempName), tempObjectParentPrefix(object); got != want {
		t.Fatalf("temp prefix = %q, want %q", got, want)
	}
	if len(tempName) > maxObjectNameBytes {
		t.Fatalf("temp object length = %d bytes, want <= %d", len(tempName), maxObjectNameBytes)
	}
	if strings.Contains(tempName, object) {
		t.Fatalf("temp object %q embeds the full target name %q", tempName, object)
	}
}

func TestNextTemporaryObjectNameSupportsMaxUTF8ByteName(t *testing.T) {
	originalToken := randomTempObjectToken
	randomTempObjectToken = func() (string, error) {
		return strings.Repeat("b", tempObjectRandomHexLen), nil
	}
	t.Cleanup(func() {
		randomTempObjectToken = originalToken
	})

	object := strings.Repeat("界", 341) + "a"
	if got := len(object); got != maxObjectNameBytes {
		t.Fatalf("target length = %d bytes, want %d", got, maxObjectNameBytes)
	}
	tempName, err := nextTemporaryObjectName(object)
	if err != nil {
		t.Fatalf("nextTemporaryObjectName: %v", err)
	}
	if len(tempName) > maxObjectNameBytes {
		t.Fatalf("temp object length = %d bytes, want <= %d", len(tempName), maxObjectNameBytes)
	}
	if !strings.HasPrefix(tempName, tempObjectPrefix) {
		t.Fatalf("temp object = %q, want prefix %q", tempName, tempObjectPrefix)
	}
}

func TestNextTemporaryObjectNameRejectsInsteadOfEscapingPrefixScope(t *testing.T) {
	originalToken := randomTempObjectToken
	tokenCalls := 0
	randomTempObjectToken = func() (string, error) {
		tokenCalls++
		return strings.Repeat("d", tempObjectRandomHexLen), nil
	}
	t.Cleanup(func() {
		randomTempObjectToken = originalToken
	})

	object := strings.Repeat("a", 500) + "/" + strings.Repeat("b", 498) + "/x"
	if _, err := nextTemporaryObjectName(object); err == nil || !strings.Contains(err.Error(), "refusing to stage outside the destination prefix") {
		t.Fatalf("nextTemporaryObjectName error = %v, want prefix-scope rejection", err)
	}
	if tokenCalls != 0 {
		t.Fatalf("random token generated %d times before prefix validation", tokenCalls)
	}
}

func TestNextTemporaryObjectNameRejectsPrefixWithoutFullRandomToken(t *testing.T) {
	originalToken := randomTempObjectToken
	randomTempObjectToken = func() (string, error) {
		return strings.Repeat("f", tempObjectRandomHexLen), nil
	}
	t.Cleanup(func() {
		randomTempObjectToken = originalToken
	})

	object := strings.Repeat("a", 999) + "/x"
	if _, err := nextTemporaryObjectName(object); err == nil {
		t.Fatal("nextTemporaryObjectName succeeded without room for the full generated temporary name")
	}
}

func TestNextTemporaryObjectNameKeepsFullRandomTokenAtMinimumSpace(t *testing.T) {
	originalToken := randomTempObjectToken
	tokens := []string{
		strings.Repeat("9", tempObjectRandomHexLen),
		strings.Repeat("8", tempObjectRandomHexLen),
	}
	randomTempObjectToken = func() (string, error) {
		token := tokens[0]
		tokens = tokens[1:]
		return token, nil
	}
	t.Cleanup(func() {
		randomTempObjectToken = originalToken
	})

	object := strings.Repeat("a", 998) + "/x"
	tempName, err := nextTemporaryObjectName(object)
	if err != nil {
		t.Fatalf("nextTemporaryObjectName: %v", err)
	}
	if got, want := tempObjectParentPrefix(tempName), tempObjectParentPrefix(object); got != want {
		t.Fatalf("temp prefix = %q, want %q", got, want)
	}
	component := tempName[len(tempObjectParentPrefix(tempName)):]
	if got, want := component, expectedTemporaryObjectComponent(object, strings.Repeat("9", tempObjectRandomHexLen)); got != want {
		t.Fatalf("temporary component = %q, want %q", got, want)
	}
	if !isTemporaryObjectName(tempName) {
		t.Fatalf("temporary object %q is visible to List", tempName)
	}
	other, err := nextTemporaryObjectName(object)
	if err != nil {
		t.Fatalf("second nextTemporaryObjectName: %v", err)
	}
	if other == tempName {
		t.Fatalf("distinct random tokens produced the same temporary object %q", tempName)
	}
}

func TestNextTemporaryObjectNameSupportsHierarchicalSegmentLimit(t *testing.T) {
	originalToken := randomTempObjectToken
	randomTempObjectToken = func() (string, error) {
		return strings.Repeat("f", tempObjectRandomHexLen), nil
	}
	t.Cleanup(func() {
		randomTempObjectToken = originalToken
	})

	object := strings.Repeat("a", 511) + "/" + strings.Repeat("b", 511)
	tempName, err := nextTemporaryObjectName(object)
	if err != nil {
		t.Fatalf("nextTemporaryObjectName: %v", err)
	}
	component := tempName[len(tempObjectParentPrefix(tempName)):]
	if len(component) > maxObjectSegmentBytes {
		t.Fatalf("temp component length = %d bytes, want <= %d", len(component), maxObjectSegmentBytes)
	}
}

func TestIsTemporaryObjectNameMatchesOnlyReservedFormats(t *testing.T) {
	legacyUUID := "123e4567-e89b-12d3-a456-426614174000"
	if !isTemporaryObjectName("tenant/.shoal-tmp-aaaaaaaaaa1234") {
		t.Fatal("generated temporary object was not detected")
	}
	if isTemporaryObjectName("tenant/.shoal-tmp-AAAAAAAAAA1234") {
		t.Fatal("uppercase user object should remain visible")
	}
	if isTemporaryObjectName("tenant/.shoal-tmp-aaaaaaaaaa") {
		t.Fatal("partial generated temporary object should remain visible")
	}
	if !isTemporaryObjectName("tenant/object.rf" + legacyTempObjectPrefix + legacyUUID) {
		t.Fatal("legacy temporary object was not detected")
	}
	if isTemporaryObjectName("tenant/object.rf" + legacyTempObjectPrefix + strings.ToUpper(legacyUUID)) {
		t.Fatal("uppercase legacy user object should remain visible")
	}
	if isTemporaryObjectName("tenant/.shl-visible-data") {
		t.Fatal("user-visible .shl- object should not be hidden")
	}
	if isTemporaryObjectName("tenant/.shl-aaaaaaaaa") {
		t.Fatal("short .shl- object should remain visible")
	}
	if isTemporaryObjectName("tenant/.shoal-tmp-aaaaaaaaaa12345") {
		t.Fatal("longer .shoal-tmp- object should remain visible")
	}
	if isTemporaryObjectName("tenant/.shoal-tmp-AAAAAAAAAAAAAA") {
		t.Fatal("uppercase-shaped .shoal-tmp- object should remain visible")
	}
	if isTemporaryObjectName("tenant/object.rf" + legacyTempObjectPrefix + "visible") {
		t.Fatal("non-generated legacy-looking object should not be hidden")
	}
}

func TestCleanupStaleArtifactsIsBoundedGenerationPinnedAndExplicit(t *testing.T) {
	now := time.Now()
	removeErr := errors.New("delete failed")
	old := "dir/" + expectedTemporaryObjectComponent("dir/target", strings.Repeat("0", 64))
	failing := "dir/" + expectedTemporaryObjectComponent("dir/target", strings.Repeat("1", 64))
	recent := "dir/" + expectedTemporaryObjectComponent("dir/target", strings.Repeat("2", 64))
	lookalike := "dir/" + expectedTemporaryObjectComponent("dir/user", strings.Repeat("3", 64))
	uppercase := "dir/" + tempObjectPrefix + strings.Repeat("A", tempObjectRandomHexLen+tempObjectHashHexLen)
	ops := &fakeGCSArtifactOperations{
		artifacts: []gcsArtifact{
			{name: recent, updated: now, generation: 3, owned: true},
			{name: lookalike, updated: now.Add(-2 * time.Hour), generation: 4},
			{name: uppercase, updated: now.Add(-2 * time.Hour), generation: 7, owned: true},
			{name: failing, updated: now.Add(-2 * time.Hour), generation: 5, owned: true},
			{name: old, updated: now.Add(-2 * time.Hour), generation: 6, owned: true},
		},
		removeErrors: map[string]error{failing: removeErr},
	}
	backend := &Backend{artifactOps: ops}

	result, err := backend.CleanupStaleArtifacts(context.Background(), "gs://bucket/dir", now.Add(-time.Hour))
	if !errors.Is(err, removeErr) {
		t.Fatalf("error = %v, want delete failure", err)
	}
	if result.Examined != 4 {
		t.Fatalf("Examined = %d, want 4", result.Examined)
	}
	if len(result.Removed) != 1 || result.Removed[0] != "gs://bucket/"+old {
		t.Fatalf("Removed = %v", result.Removed)
	}
	if len(ops.removed) != 2 || ops.removed[0].generation == 0 {
		t.Fatalf("generation-pinned removals = %#v", ops.removed)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := backend.CleanupStaleArtifacts(ctx, "gs://bucket/dir", now); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cleanup error = %v, want context.Canceled", err)
	}
}

func TestCleanupStaleArtifactsSupportsRootPrefix(t *testing.T) {
	now := time.Now()
	old := expectedTemporaryObjectComponent("target", strings.Repeat("0", 64))
	recent := expectedTemporaryObjectComponent("target", strings.Repeat("1", 64))
	uppercase := tempObjectPrefix + strings.Repeat("A", tempObjectRandomHexLen+tempObjectHashHexLen)
	visible := ".shl-visible"
	ops := &fakeGCSArtifactOperations{
		artifacts: []gcsArtifact{
			{name: recent, updated: now, generation: 3, owned: true},
			{name: uppercase, updated: now.Add(-2 * time.Hour), generation: 4, owned: true},
			{name: visible, updated: now.Add(-2 * time.Hour), generation: 5, owned: true},
			{name: old, updated: now.Add(-2 * time.Hour), generation: 6, owned: true},
		},
	}
	backend := &Backend{artifactOps: ops}

	result, err := backend.CleanupStaleArtifacts(context.Background(), "gs://bucket/", now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if ops.listedPrefix != "" {
		t.Fatalf("listed prefix = %q, want root prefix", ops.listedPrefix)
	}
	if result.Examined != 2 {
		t.Fatalf("Examined = %d, want 2", result.Examined)
	}
	if len(result.Removed) != 1 || result.Removed[0] != "gs://bucket/"+old {
		t.Fatalf("Removed = %v, want [gs://bucket/%s]", result.Removed, old)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := backend.CleanupStaleArtifacts(ctx, "gs://bucket/", now); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cleanup error = %v, want context.Canceled", err)
	}
}

func TestBackendCreateRejectsReservedInternalObjectNames(t *testing.T) {
	backend, _ := newFakeBackend()
	for _, object := range []string{
		"tenant/.shoal-tmp-aaaaaaaaaa1234",
		"tenant/nested/.shoal-tmp-aaaaaaaaaa1234",
		"tenant/object.rf" + legacyTempObjectPrefix + "123e4567-e89b-12d3-a456-426614174000",
	} {
		t.Run(object, func(t *testing.T) {
			if _, err := backend.Create(context.Background(), "bucket/"+object); err == nil || !strings.Contains(err.Error(), "reserved internal namespace") {
				t.Fatalf("Create(%q) error = %v, want reserved internal namespace rejection", object, err)
			}
		})
	}
}

func TestBackendCreateAllowsUserObjectsOutsideReservedNamespace(t *testing.T) {
	for _, object := range []string{
		"tenant/.shl-final.rf",
		"tenant/.shl-aaaaaaaaa",
		"tenant/.shoal-tmp-" + strings.Repeat("A", tempObjectRandomHexLen+tempObjectHashHexLen),
		"tenant/.shoal-tmp-aaaaaaaaaa12345",
		"tenant/nested/.shl-final.rf",
	} {
		t.Run(object, func(t *testing.T) {
			backend, bucket := newFakeBackend()
			w, err := backend.Create(context.Background(), "bucket/"+object)
			if err != nil {
				t.Fatalf("Create(%q): %v", object, err)
			}
			if _, err := w.Write([]byte("data")); err != nil {
				t.Fatalf("Write(%q): %v", object, err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("Close(%q): %v", object, err)
			}
			got, ok := bucket.objects[object]
			if !ok || string(got.body) != "data" {
				t.Fatalf("destination object %q = %q, present=%v; want data", object, got.body, ok)
			}
		})
	}
}

func TestWriter_CloseStagesAndPromotesObject(t *testing.T) {
	backend, bucket := newFakeBackend()
	bucket.putObject("path/to/object.rf", []byte("old"))

	w, err := backend.Create(context.Background(), "bucket/path/to/object.rf")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	gcsWriter := w.(*writer)
	tempName := gcsWriter.temp.(*fakeObject).name

	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, ok := bucket.objects["path/to/object.rf"]
	if !ok || string(got.body) != "new" {
		t.Fatalf("destination object = %q, present=%v; want new", got.body, ok)
	}
	if _, ok := bucket.objects[tempName]; ok {
		t.Fatalf("successful Close left temp object %q behind", tempName)
	}
	if len(bucket.copyCalls) != 1 {
		t.Fatalf("copy calls = %v, want 1", bucket.copyCalls)
	}
	if cond := bucket.copyCalls[0].conditions; cond.GenerationMatch == 0 {
		t.Fatalf("promotion conditions = %+v, want GenerationMatch", cond)
	}
}

func TestWriter_CloseErrorPreservesExistingObjectAndCleansTemp(t *testing.T) {
	backend, bucket := newFakeBackend()
	bucket.putObject("path/to/object.rf", []byte("old"))

	closeErr := errors.New("checksum mismatch")
	bucket.writerPlan = func(name string) fakeWriterPlan {
		if isTemporaryObjectName(name) {
			return fakeWriterPlan{closeErr: closeErr, publishOnClose: true}
		}
		return fakeWriterPlan{}
	}

	w, err := backend.Create(context.Background(), "bucket/path/to/object.rf")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	gcsWriter := w.(*writer)
	tempName := gcsWriter.temp.(*fakeObject).name

	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	err = w.Close()
	if !errors.Is(err, closeErr) {
		t.Fatalf("Close error = %v, want checksum mismatch", err)
	}
	if got := string(bucket.objects["path/to/object.rf"].body); got != "old" {
		t.Fatalf("destination contents = %q, want preserved old object", got)
	}
	if _, ok := bucket.objects[tempName]; ok {
		t.Fatalf("Close error left temp object %q behind", tempName)
	}
}

func TestWriter_ClosePreconditionFailurePreservesNewerTarget(t *testing.T) {
	backend, bucket := newFakeBackend()
	bucket.putObject("path/to/object.rf", []byte("old"))

	w, err := backend.Create(context.Background(), "bucket/path/to/object.rf")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	gcsWriter := w.(*writer)
	tempName := gcsWriter.temp.(*fakeObject).name

	bucket.putObject("path/to/object.rf", []byte("newer"))
	if _, err := w.Write([]byte("replacement")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	err = w.Close()
	if !errors.Is(err, errFakePrecondition) {
		t.Fatalf("Close error = %v, want precondition failure", err)
	}
	if got := string(bucket.objects["path/to/object.rf"].body); got != "newer" {
		t.Fatalf("destination contents = %q, want concurrent newer object preserved", got)
	}
	if _, ok := bucket.objects[tempName]; ok {
		t.Fatalf("precondition failure left temp object %q behind", tempName)
	}
}

func TestWriter_TemporaryObjectCollisionPreservesExistingTargetAndTemp(t *testing.T) {
	originalToken := randomTempObjectToken
	randomTempObjectToken = func() (string, error) {
		return strings.Repeat("c", tempObjectRandomHexLen), nil
	}
	t.Cleanup(func() {
		randomTempObjectToken = originalToken
	})

	backend, bucket := newFakeBackend()
	bucket.putObject("path/to/object.rf", []byte("old"))
	tempName, err := nextTemporaryObjectName("path/to/object.rf")
	if err != nil {
		t.Fatalf("nextTemporaryObjectName: %v", err)
	}
	bucket.putObject(tempName, []byte("other-temp"))

	w, err := backend.Create(context.Background(), "bucket/path/to/object.rf")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	err = w.Close()
	if !errors.Is(err, errFakePrecondition) {
		t.Fatalf("Close error = %v, want temporary-object precondition failure", err)
	}
	if got := string(bucket.objects["path/to/object.rf"].body); got != "old" {
		t.Fatalf("destination contents = %q, want preserved old object", got)
	}
	if got := string(bucket.objects[tempName].body); got != "other-temp" {
		t.Fatalf("colliding temp contents = %q, want preserved existing temp object", got)
	}
}

func TestWriter_CloseAmbiguousPromotionErrorDoesNotReplayMutation(t *testing.T) {
	backend, bucket := newFakeBackend()
	bucket.putObject("path/to/object.rf", []byte("old"))
	bucket.copyAfterCommitErr = context.DeadlineExceeded

	w, err := backend.Create(context.Background(), "bucket/path/to/object.rf")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close after committed ambiguous copy response: %v", err)
	}
	if got := string(bucket.objects["path/to/object.rf"].body); got != "new" {
		t.Fatalf("destination contents = %q, want committed new object", got)
	}
	if len(bucket.copyCalls) != 1 {
		t.Fatalf("copy calls = %d, want exactly 1 (no mutation replay)", len(bucket.copyCalls))
	}
}

func TestWriter_CloseIndeterminateCommittedPromotionRetriesVerification(t *testing.T) {
	backend, bucket := newFakeBackend()
	bucket.putObject("path/to/object.rf", []byte("old"))
	bucket.copyAfterCommitErr = context.DeadlineExceeded

	w, err := backend.Create(context.Background(), "bucket/path/to/object.rf")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	gcsWriter := w.(*writer)
	bucket.attrsErrs["path/to/object.rf"] = []error{context.DeadlineExceeded}
	tempName := gcsWriter.temp.(*fakeObject).name
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Close error = %v, want transient verification failure", err)
	}
	if !gcsWriter.promotionIndeterminate {
		t.Fatal("first Close did not retain indeterminate promotion state")
	}
	if _, ok := bucket.objects[tempName]; !ok {
		t.Fatalf("indeterminate Close removed temp object %q", tempName)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if len(bucket.copyCalls) != 1 {
		t.Fatalf("copy calls = %d, want exactly 1", len(bucket.copyCalls))
	}
	if got := string(bucket.objects["path/to/object.rf"].body); got != "new" {
		t.Fatalf("destination contents = %q, want new", got)
	}
	if _, ok := bucket.objects[tempName]; ok {
		t.Fatalf("second Close did not clean temp object %q", tempName)
	}
}

func TestWriter_CloseIndeterminateCommittedCancellationRetriesVerification(t *testing.T) {
	backend, bucket := newFakeBackend()
	bucket.putObject("path/to/object.rf", []byte("old"))
	bucket.copyAfterCommitErr = context.Canceled

	w, err := backend.Create(context.Background(), "bucket/path/to/object.rf")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	gcsWriter := w.(*writer)
	bucket.attrsErrs["path/to/object.rf"] = []error{context.DeadlineExceeded}
	tempName := gcsWriter.temp.(*fakeObject).name
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err == nil || !errors.Is(err, context.Canceled) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Close error = %v, want cancellation joined with transient verification failure", err)
	}
	if !gcsWriter.promotionIndeterminate {
		t.Fatal("first Close did not retain indeterminate promotion state")
	}
	if _, ok := bucket.objects[tempName]; !ok {
		t.Fatalf("indeterminate Close removed temp object %q", tempName)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if len(bucket.copyCalls) != 1 {
		t.Fatalf("copy calls = %d, want exactly 1", len(bucket.copyCalls))
	}
	if got := string(bucket.objects["path/to/object.rf"].body); got != "new" {
		t.Fatalf("destination contents = %q, want new", got)
	}
	if _, ok := bucket.objects[tempName]; ok {
		t.Fatalf("second Close did not clean temp object %q", tempName)
	}
}

func TestWriter_CloseIndeterminateUncommittedPromotionRetriesCopy(t *testing.T) {
	backend, bucket := newFakeBackend()
	bucket.putObject("path/to/object.rf", []byte("old"))
	attempts := 0
	bucket.copyHook = func(_ context.Context, _, _ *fakeObject) error {
		if attempts == 0 {
			attempts++
			return context.DeadlineExceeded
		}
		return nil
	}

	w, err := backend.Create(context.Background(), "bucket/path/to/object.rf")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	gcsWriter := w.(*writer)
	bucket.attrsErrs["path/to/object.rf"] = []error{context.DeadlineExceeded}
	tempName := gcsWriter.temp.(*fakeObject).name
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Close error = %v, want transient verification failure", err)
	}
	if !gcsWriter.promotionIndeterminate {
		t.Fatal("first Close did not retain indeterminate promotion state")
	}
	if got := string(bucket.objects["path/to/object.rf"].body); got != "old" {
		t.Fatalf("destination contents after failed copy = %q, want old", got)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if len(bucket.copyCalls) != 2 {
		t.Fatalf("copy calls = %d, want 2", len(bucket.copyCalls))
	}
	if got := string(bucket.objects["path/to/object.rf"].body); got != "new" {
		t.Fatalf("destination contents after retry = %q, want new", got)
	}
	if _, ok := bucket.objects[tempName]; ok {
		t.Fatalf("retry success did not clean temp object %q", tempName)
	}
}

func TestWriter_CloseCanceledUncommittedPromotionCanAbort(t *testing.T) {
	backend, bucket := newFakeBackend()
	bucket.putObject("path/to/object.rf", []byte("old"))
	bucket.copyHook = func(_ context.Context, _, _ *fakeObject) error {
		return context.Canceled
	}

	w, err := backend.Create(context.Background(), "bucket/path/to/object.rf")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	gcsWriter := w.(*writer)
	tempName := gcsWriter.temp.(*fakeObject).name
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("first Close error = %v, want cancellation", err)
	}
	if !gcsWriter.promotionIndeterminate {
		t.Fatal("first Close did not retain indeterminate promotion state")
	}
	if got := string(bucket.objects["path/to/object.rf"].body); got != "old" {
		t.Fatalf("destination contents after canceled copy = %q, want old", got)
	}
	if err := gcsWriter.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if !gcsWriter.aborted {
		t.Fatal("Abort did not mark the writer aborted")
	}
	if gcsWriter.promotionIndeterminate {
		t.Fatal("Abort left the promotion state indeterminate")
	}
	if _, ok := bucket.objects[tempName]; ok {
		t.Fatalf("Abort did not clean temp object %q", tempName)
	}
	if got := string(bucket.objects["path/to/object.rf"].body); got != "old" {
		t.Fatalf("destination contents after Abort = %q, want old", got)
	}
}

func TestWriter_CloseAmbiguousPromotionRequiresMatchingWriteID(t *testing.T) {
	backend, bucket := newFakeBackend()
	bucket.putObject("path/to/object.rf", []byte("old"))
	bucket.copyHook = func(_ context.Context, dst, src *fakeObject) error {
		srcData, ok := src.lookup()
		if !ok {
			t.Fatal("temporary object missing during competing copy simulation")
		}
		bucket.putObjectWithMetadata(dst.name, srcData.body, map[string]string{"shoal-write-id": "intruder"})
		return context.DeadlineExceeded
	}

	w, err := backend.Create(context.Background(), "bucket/path/to/object.rf")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	tempName := w.(*writer).temp.(*fakeObject).name
	err = w.Close()
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want ambiguous copy error", err)
	}
	if got := bucket.objects["path/to/object.rf"].metadata["shoal-write-id"]; got != "intruder" {
		t.Fatalf("destination write-id = %q, want intruder competing writer", got)
	}
	if _, ok := bucket.objects[tempName]; !ok {
		t.Fatalf("failed promotion should retain temp object %q for retry", tempName)
	}
	if err := w.Close(); err == nil || !strings.Contains(err.Error(), "indeterminate") {
		t.Fatalf("second Close error = %v, want indeterminate competing-writer failure", err)
	}
	if len(bucket.copyCalls) != 1 {
		t.Fatalf("copy calls = %d, want exactly 1 after competing writer", len(bucket.copyCalls))
	}
}

func TestWriter_AmbiguousPromotionDoesNotAcceptConcurrentIdenticalObject(t *testing.T) {
	backend, bucket := newFakeBackend()
	bucket.putObject("path/to/object.rf", []byte("old"))
	bucket.copyHook = func(_ context.Context, dst, _ *fakeObject) error {
		bucket.putObjectWithMetadata(dst.name, []byte("new"), map[string]string{"shoal-write-id": "other-writer"})
		return nil
	}

	w, err := backend.Create(context.Background(), "bucket/path/to/object.rf")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); !errors.Is(err, errFakePrecondition) {
		t.Fatalf("Close error = %v, want concurrent precondition failure", err)
	}
	if got := bucket.objects["path/to/object.rf"].metadata["shoal-write-id"]; got != "other-writer" {
		t.Fatalf("destination write ID = %q, want concurrent writer", got)
	}
}

func TestWriter_CloseAmbiguousPromotionAcceptsMatchingWriteID(t *testing.T) {
	backend, bucket := newFakeBackend()
	bucket.putObject("path/to/object.rf", []byte("old"))

	w, err := backend.Create(context.Background(), "bucket/path/to/object.rf")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	gcsWriter := w.(*writer)
	bucket.copyHook = func(_ context.Context, dst, src *fakeObject) error {
		srcData, ok := src.lookup()
		if !ok {
			t.Fatal("temporary object missing during committed copy simulation")
		}
		bucket.putObjectWithMetadata(dst.name, srcData.body, map[string]string{"shoal-write-id": gcsWriter.writeID})
		return context.DeadlineExceeded
	}
	tempName := gcsWriter.temp.(*fakeObject).name
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close after committed ambiguous copy response: %v", err)
	}
	if got := bucket.objects["path/to/object.rf"].metadata["shoal-write-id"]; got != gcsWriter.writeID {
		t.Fatalf("destination write-id = %q, want %q", got, gcsWriter.writeID)
	}
	if len(bucket.copyCalls) != 1 {
		t.Fatalf("copy calls = %d, want exactly 1", len(bucket.copyCalls))
	}
	if _, ok := bucket.objects[tempName]; ok {
		t.Fatalf("successful committed copy should clean temp object %q", tempName)
	}
}

func TestWriter_AbortPreservesRecoverabilityDuringIndeterminatePromotion(t *testing.T) {
	backend, bucket := newFakeBackend()
	bucket.putObject("path/to/object.rf", []byte("old"))
	bucket.copyAfterCommitErr = context.DeadlineExceeded

	w, err := backend.Create(context.Background(), "bucket/path/to/object.rf")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	gcsWriter := w.(*writer)
	bucket.attrsErrs["path/to/object.rf"] = []error{context.DeadlineExceeded}
	tempName := gcsWriter.temp.(*fakeObject).name
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want transient verification failure", err)
	}
	if err := gcsWriter.Abort(); err == nil || !strings.Contains(err.Error(), "indeterminate") {
		t.Fatalf("Abort error = %v, want indeterminate abort failure", err)
	}
	if gcsWriter.aborted {
		t.Fatal("Abort marked an indeterminate promotion as aborted")
	}
	if _, ok := bucket.objects[tempName]; !ok {
		t.Fatalf("Abort removed temp object %q needed for recovery", tempName)
	}
}

func TestWriter_IndeterminatePromotionRetainsTempAcrossRepeatedVerificationFailures(t *testing.T) {
	backend, bucket := newFakeBackend()
	bucket.putObject("path/to/object.rf", []byte("old"))
	bucket.copyAfterCommitErr = context.DeadlineExceeded

	w, err := backend.Create(context.Background(), "bucket/path/to/object.rf")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	gcsWriter := w.(*writer)
	bucket.attrsErrs["path/to/object.rf"] = []error{context.DeadlineExceeded, context.DeadlineExceeded}
	tempName := gcsWriter.temp.(*fakeObject).name
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Close error = %v, want transient verification failure", err)
	}
	if err := w.Close(); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Close error = %v, want repeated transient verification failure", err)
	}
	if !gcsWriter.promotionIndeterminate {
		t.Fatal("repeated verification failures cleared the indeterminate promotion state")
	}
	if len(bucket.copyCalls) != 1 {
		t.Fatalf("copy calls = %d, want 1", len(bucket.copyCalls))
	}
	if _, ok := bucket.objects[tempName]; !ok {
		t.Fatalf("repeated failures removed temp object %q", tempName)
	}
}

func TestWriter_AbortRetriesUnknownTemporaryObjectVerification(t *testing.T) {
	backend, bucket := newFakeBackend()
	w, err := backend.Create(context.Background(), "bucket/path/to/object.rf")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tempName := w.(*writer).temp.(*fakeObject).name
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	bucket.writerPlan = func(name string) fakeWriterPlan {
		if name == tempName {
			return fakeWriterPlan{closeErr: context.DeadlineExceeded, publishOnClose: true}
		}
		return fakeWriterPlan{}
	}
	bucket.lastWriter.plan = bucket.writerPlan(tempName)
	bucket.attrsFailures = 2
	if err := w.Close(); err == nil {
		t.Fatal("Close succeeded despite ambiguous upload and verification failures")
	}
	if _, ok := bucket.objects[tempName]; !ok {
		t.Fatal("ambiguous temporary object unexpectedly absent before retry")
	}
	if err := w.(*writer).Abort(); err != nil {
		t.Fatalf("Abort retry: %v", err)
	}
	if _, ok := bucket.objects[tempName]; ok {
		t.Fatal("Abort did not discover and remove temporary object")
	}
}

func TestWriter_CanceledPromotionUsesIndependentCleanupContext(t *testing.T) {
	backend, bucket := newFakeBackend()
	bucket.putObject("path/to/object.rf", []byte("old"))
	cleanupSawCanceledContext := false
	bucket.deleteHook = func(ctx context.Context, _ *fakeObject) error {
		cleanupSawCanceledContext = ctx.Err() != nil
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())

	w, err := backend.Create(ctx, "bucket/path/to/object.rf")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tempName := w.(*writer).temp.(*fakeObject).name
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	cancel()
	err = w.Close()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Close error = %v, want context.Canceled", err)
	}
	if got := string(bucket.objects["path/to/object.rf"].body); got != "old" {
		t.Fatalf("destination contents = %q, want preserved old object", got)
	}
	if _, ok := bucket.objects[tempName]; !ok {
		t.Fatalf("canceled promotion did not retain temp object %q for retry", tempName)
	}
	if cleanupSawCanceledContext {
		t.Fatal("temporary cleanup reused the canceled operation context")
	}
	if err := w.(shstorage.Aborter).Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if cleanupSawCanceledContext {
		t.Fatal("Abort cleanup reused the canceled operation context")
	}
	if _, ok := bucket.objects[tempName]; ok {
		t.Fatalf("Abort did not remove temp object %q", tempName)
	}
}

func TestWriter_AbortSkipsDeletingUncommittedTempObject(t *testing.T) {
	backend, bucket := newFakeBackend()

	attempts := 0
	deleteErr := errors.New("delete failed")
	bucket.deleteHook = func(_ context.Context, object *fakeObject) error {
		if isTemporaryObjectName(object.name) && attempts == 0 {
			attempts++
			return deleteErr
		}
		return nil
	}

	w, err := backend.Create(context.Background(), "bucket/path/to/object.rf")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	gcsWriter := w.(*writer)
	tempName := gcsWriter.temp.(*fakeObject).name

	if _, err := w.Write([]byte("abort")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	err = w.(shstorage.Aborter).Abort()
	if err != nil {
		t.Fatalf("Abort error = %v, want nil for an uncommitted temporary object", err)
	}
	if _, err := w.Write([]byte("late")); err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("Write after failed Abort error = %v, want aborted state", err)
	}
	if err := w.Close(); err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("Close after failed Abort error = %v, want aborted state", err)
	}
	if _, ok := bucket.objects[tempName]; ok {
		t.Fatal("Abort should not publish or retain an uncommitted temp object")
	}
	if attempts != 0 {
		t.Fatalf("temporary delete attempts = %d, want 0 for an uncommitted temp object", attempts)
	}

	if err := w.(shstorage.Aborter).Abort(); err != nil {
		t.Fatalf("second Abort: %v", err)
	}
	if gcsWriter.inner.(*fakeObjectWriter).closeWithErrorCalls != 1 {
		t.Fatalf("CloseWithError calls after retry = %d, want 1", gcsWriter.inner.(*fakeObjectWriter).closeWithErrorCalls)
	}
}

func TestWriter_AbortRetriesOwnedCleanupAfterAmbiguousCloseVerification(t *testing.T) {
	backend, bucket := newFakeBackend()
	w, err := backend.Create(context.Background(), "bucket/path/to/object.rf")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	gcsWriter := w.(*writer)
	tempName := gcsWriter.temp.(*fakeObject).name
	bucket.attrsErrs[tempName] = []error{context.DeadlineExceeded, context.DeadlineExceeded}

	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want transient temp verification failure", err)
	}
	if _, ok := bucket.objects[tempName]; !ok {
		t.Fatalf("ambiguous Close lost temp object %q before retry", tempName)
	}

	attempts := 0
	bucket.deleteHook = func(_ context.Context, object *fakeObject) error {
		if object.name == tempName && attempts == 0 {
			attempts++
			return errors.New("delete failed")
		}
		return nil
	}
	if err := w.(shstorage.Aborter).Abort(); err == nil || !strings.Contains(err.Error(), "remove temporary object") {
		t.Fatalf("first Abort error = %v, want retryable cleanup failure", err)
	}
	if _, ok := bucket.objects[tempName]; !ok {
		t.Fatalf("failed Abort removed temp object %q unexpectedly", tempName)
	}
	if err := w.(shstorage.Aborter).Abort(); err != nil {
		t.Fatalf("second Abort: %v", err)
	}
	if _, ok := bucket.objects[tempName]; ok {
		t.Fatalf("second Abort did not remove temp object %q", tempName)
	}
	if err := w.(shstorage.Aborter).Abort(); err != nil {
		t.Fatalf("third Abort: %v", err)
	}
	if err := w.Close(); err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("Close after Abort error = %v, want aborted state", err)
	}
}

func TestWriter_CloseRetriesOwnedVerificationAfterAmbiguousClose(t *testing.T) {
	backend, bucket := newFakeBackend()
	w, err := backend.Create(context.Background(), "bucket/path/to/object.rf")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	gcsWriter := w.(*writer)
	tempName := gcsWriter.temp.(*fakeObject).name
	bucket.attrsErrs[tempName] = []error{context.DeadlineExceeded, context.DeadlineExceeded}

	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want transient temp verification failure", err)
	}
	if _, ok := bucket.objects[tempName]; !ok {
		t.Fatalf("ambiguous Close lost temp object %q before retry", tempName)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := string(bucket.objects["path/to/object.rf"].body); got != "new" {
		t.Fatalf("destination contents = %q, want new", got)
	}
	if _, ok := bucket.objects[tempName]; ok {
		t.Fatalf("second Close did not clean temp object %q", tempName)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("third Close: %v", err)
	}
}

func TestWriter_AbortTreatsUnownedTempAsAlreadyCleanAfterAmbiguousClose(t *testing.T) {
	backend, bucket := newFakeBackend()
	w, err := backend.Create(context.Background(), "bucket/path/to/object.rf")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	gcsWriter := w.(*writer)
	tempName := gcsWriter.temp.(*fakeObject).name
	bucket.attrsErrs[tempName] = []error{context.DeadlineExceeded, context.DeadlineExceeded}

	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close error = %v, want transient temp verification failure", err)
	}

	bucket.putObjectWithMetadata(tempName, []byte("intruder"), map[string]string{"shoal-write-id": "intruder"})
	if err := w.(shstorage.Aborter).Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if got := string(bucket.objects[tempName].body); got != "intruder" {
		t.Fatalf("Abort removed or replaced unowned temp object: %q", got)
	}
	if len(bucket.deleteCalls) != 0 {
		t.Fatalf("delete calls = %+v, want none for unowned temp object", bucket.deleteCalls)
	}
	if err := w.(shstorage.Aborter).Abort(); err != nil {
		t.Fatalf("second Abort: %v", err)
	}
	if err := w.Close(); err == nil || !strings.Contains(err.Error(), "aborted") {
		t.Fatalf("Close after Abort error = %v, want aborted state", err)
	}
}

func TestWriter_CloseSuccessIgnoresTempCleanupFailure(t *testing.T) {
	var logs bytes.Buffer
	restore := captureDefaultLogger(t, &logs)
	defer restore()

	backend, bucket := newFakeBackend()
	cleanupErr := errors.New("cleanup failed")
	bucket.deleteHook = func(_ context.Context, object *fakeObject) error {
		if isTemporaryObjectName(object.name) {
			return cleanupErr
		}
		return nil
	}

	w, err := backend.Create(context.Background(), "bucket/path/to/object.rf")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tempName := w.(*writer).temp.(*fakeObject).name
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close error = %v, want success after committed promotion", err)
	}
	if got := string(bucket.objects["path/to/object.rf"].body); got != "new" {
		t.Fatalf("destination contents = %q, want new", got)
	}
	if _, ok := bucket.objects[tempName]; !ok {
		t.Fatalf("cleanup failure should leave temp object %q for observation", tempName)
	}
	if got := logs.String(); !strings.Contains(got, "temporary object pending cleanup") || !strings.Contains(got, tempName) {
		t.Fatalf("cleanup warning log = %q, want pending cleanup warning for %s", got, tempName)
	}
	bucket.deleteHook = nil
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if _, ok := bucket.objects[tempName]; ok {
		t.Fatalf("second Close did not retry exhausted committed cleanup for %q", tempName)
	}
}

func TestWriter_CloseRetriesTempCleanupAfterCommittedPromotion(t *testing.T) {
	backend, bucket := newFakeBackend()
	attempts := 0
	bucket.deleteHook = func(_ context.Context, object *fakeObject) error {
		if isTemporaryObjectName(object.name) && attempts == 0 {
			attempts++
			return errors.New("delete failed")
		}
		return nil
	}

	w, err := backend.Create(context.Background(), "bucket/path/to/object.rf")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	gcsWriter := w.(*writer)
	tempName := gcsWriter.temp.(*fakeObject).name
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if _, ok := bucket.objects[tempName]; ok {
		t.Fatalf("committed cleanup did not retry and remove temp object %q", tempName)
	}
}

func TestWriter_CloseUsesVerifiedTempObjectWhenWriterAttrsAreUnreliable(t *testing.T) {
	backend, bucket := newFakeBackend()
	bucket.putObject("path/to/object.rf", []byte("old"))
	closeErr := errors.New("checksum mismatch")
	bucket.writerPlan = func(name string) fakeWriterPlan {
		if isTemporaryObjectName(name) {
			return fakeWriterPlan{
				closeErr:       closeErr,
				publishOnClose: true,
				attrsOverride:  &cloudstorage.ObjectAttrs{Name: name, Generation: 999},
			}
		}
		return fakeWriterPlan{}
	}

	w, err := backend.Create(context.Background(), "bucket/path/to/object.rf")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tempName := w.(*writer).temp.(*fakeObject).name
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	err = w.Close()
	if !errors.Is(err, closeErr) {
		t.Fatalf("Close error = %v, want checksum mismatch", err)
	}
	if got := string(bucket.objects["path/to/object.rf"].body); got != "old" {
		t.Fatalf("destination contents = %q, want old", got)
	}
	if _, ok := bucket.objects[tempName]; ok {
		t.Fatalf("verified cleanup left temp object %q behind", tempName)
	}
}

func TestWriter_CloseErrorDeletesOnlyVerifiedTempGeneration(t *testing.T) {
	backend, bucket := newFakeBackend()
	bucket.putObject("path/to/object.rf", []byte("old"))
	closeErr := errors.New("checksum mismatch")
	bucket.writerPlan = func(name string) fakeWriterPlan {
		if isTemporaryObjectName(name) {
			return fakeWriterPlan{closeErr: closeErr, publishOnClose: true}
		}
		return fakeWriterPlan{}
	}
	bucket.deleteHook = func(_ context.Context, object *fakeObject) error {
		if !isTemporaryObjectName(object.name) {
			return nil
		}
		bucket.putObjectWithMetadata(object.name, []byte("intruder"), map[string]string{"shoal-write-id": "intruder"})
		return nil
	}

	w, err := backend.Create(context.Background(), "bucket/path/to/object.rf")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	tempName := w.(*writer).temp.(*fakeObject).name
	if _, err := w.Write([]byte("new")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	err = w.Close()
	if !errors.Is(err, closeErr) {
		t.Fatalf("Close error = %v, want checksum mismatch", err)
	}
	if got := string(bucket.objects["path/to/object.rf"].body); got != "old" {
		t.Fatalf("destination contents = %q, want old", got)
	}
	if len(bucket.deleteCalls) != 1 || bucket.deleteCalls[0].generation == 0 {
		t.Fatalf("delete calls = %+v, want generation-scoped cleanup", bucket.deleteCalls)
	}
	if got := string(bucket.objects[tempName].body); got != "intruder" {
		t.Fatalf("temporary object %q = %q, want preserved replacement object", tempName, got)
	}
}

// TestGCS_RoundtripAgainstRealBucket exercises Open + ReadAt against a
// real GCS bucket. Skipped when SHOAL_GCS_TEST_BUCKET / _OBJECT aren't
// set — CI without GCS creds will skip cleanly.
//
// Setup the operator does once:
//
//	echo "test contents" | gsutil cp - gs://your-bucket/shoal-test/probe.txt
//	export SHOAL_GCS_TEST_BUCKET=your-bucket
//	export SHOAL_GCS_TEST_OBJECT=shoal-test/probe.txt
//	export SHOAL_GCS_TEST_EXPECT="test contents\n"
//	go test -tags gcs ./internal/storage/gcs/... -count=1
//
// Note: ADC must be configured (gcloud auth application-default login
// or workload identity in-cluster).
func TestGCS_RoundtripAgainstRealBucket(t *testing.T) {
	bucket := os.Getenv("SHOAL_GCS_TEST_BUCKET")
	object := os.Getenv("SHOAL_GCS_TEST_OBJECT")
	if bucket == "" || object == "" {
		t.Skip("SHOAL_GCS_TEST_BUCKET / SHOAL_GCS_TEST_OBJECT not set; skipping live GCS test")
	}
	expect := os.Getenv("SHOAL_GCS_TEST_EXPECT")

	ctx := context.Background()
	be, err := New(ctx)
	if err != nil {
		t.Fatalf("gcs.New: %v", err)
	}
	defer be.Close()

	f, err := be.Open(ctx, "gs://"+bucket+"/"+object)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close()

	if f.Size() <= 0 {
		t.Errorf("Size = %d, want > 0", f.Size())
	}

	// Read the whole file.
	body := make([]byte, f.Size())
	n, err := f.ReadAt(body, 0)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("ReadAt: %v", err)
	}
	if int64(n) != f.Size() {
		t.Errorf("read %d bytes, want %d", n, f.Size())
	}
	if expect != "" && !strings.Contains(string(body), strings.TrimSpace(expect)) {
		t.Errorf("contents mismatch: got %q (first 64), want substring %q",
			string(body[:min(64, len(body))]), expect)
	}

	// Random partial read in the middle.
	if f.Size() > 10 {
		mid := make([]byte, 5)
		_, err := f.ReadAt(mid, 1)
		if err != nil && !errors.Is(err, io.EOF) {
			t.Errorf("partial ReadAt: %v", err)
		}
	}
}

func TestGCS_NotFoundIsSentinel(t *testing.T) {
	bucket := os.Getenv("SHOAL_GCS_TEST_BUCKET")
	if bucket == "" {
		t.Skip("SHOAL_GCS_TEST_BUCKET not set; skipping live GCS test")
	}
	ctx := context.Background()
	be, err := New(ctx)
	if err != nil {
		t.Fatalf("gcs.New: %v", err)
	}
	defer be.Close()

	_, err = be.Open(ctx, "gs://"+bucket+"/this/path/does/not/exist/in/any/world.rf")
	if !errors.Is(err, shstorage.ErrNotFound) {
		t.Errorf("err = %v, want chain to storage.ErrNotFound", err)
	}
}

// TestGCS_CloseIdempotent doesn't need a real bucket — it just exercises
// the client-construction + double-Close path.
func TestGCS_CloseIdempotent(t *testing.T) {
	if os.Getenv("SHOAL_GCS_TEST_BUCKET") == "" {
		// Even without creds, NewClient(ADC) succeeds for the storage
		// client's metadata-only construction. But that's flaky in
		// hermetic environments — gate this whole test on creds presence.
		t.Skip("skipping GCS-client construction test without test bucket")
	}
	ctx := context.Background()
	be, err := New(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := be.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := be.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

func newFakeBackend() (*Backend, *fakeBucket) {
	bucket := newFakeBucket()
	return &Backend{
		bucketFactory: func(string) bucketHandle { return bucket },
	}, bucket
}

var errFakePrecondition = errors.New("fake gcs: precondition failed")

type cancelAfterWriteBackend struct {
	shstorage.WritableBackend
	cancel func()
}

func (b cancelAfterWriteBackend) Create(ctx context.Context, path string) (shstorage.Writer, error) {
	writer, err := b.WritableBackend.Create(ctx, path)
	if err != nil {
		return nil, err
	}
	return cancelAfterWriteWriter{Writer: writer, cancel: b.cancel}, nil
}

type cancelAfterWriteWriter struct {
	shstorage.Writer
	cancel func()
}

func (w cancelAfterWriteWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if w.cancel != nil {
		w.cancel()
	}
	return n, err
}

type fakeGCSArtifactOperations struct {
	artifacts    []gcsArtifact
	listedPrefix string
	removeErrors map[string]error
	removed      []gcsArtifact
}

func (o *fakeGCSArtifactOperations) list(ctx context.Context, _, prefix string) ([]gcsArtifact, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	o.listedPrefix = prefix
	return append([]gcsArtifact(nil), o.artifacts...), nil
}

func (o *fakeGCSArtifactOperations) remove(ctx context.Context, _ string, artifact gcsArtifact) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	o.removed = append(o.removed, artifact)
	return o.removeErrors[artifact.name]
}

type fakeBucket struct {
	objects            map[string]fakeObjectData
	nextGen            int64
	writerPlan         func(string) fakeWriterPlan
	attrsErrs          map[string][]error
	deleteHook         func(context.Context, *fakeObject) error
	copyHook           func(context.Context, *fakeObject, *fakeObject) error
	copyAfterCommitErr error
	attrsFailures      int
	lastWriter         *fakeObjectWriter
	copyCalls          []fakeCopyCall
	deleteCalls        []fakeDeleteCall
}

type fakeObjectData struct {
	body           []byte
	generation     int64
	metageneration int64
	metadata       map[string]string
}

type fakeWriterPlan struct {
	closeErr       error
	publishOnClose bool
	closeAbortErr  error
	closeAbortFn   func(error) error
	attrsOverride  *cloudstorage.ObjectAttrs
}

type fakeCopyCall struct {
	src        string
	dst        string
	srcGen     int64
	conditions cloudstorage.Conditions
}

type fakeDeleteCall struct {
	name       string
	generation int64
}

func newFakeBucket() *fakeBucket {
	return &fakeBucket{
		objects:   make(map[string]fakeObjectData),
		nextGen:   1,
		attrsErrs: make(map[string][]error),
		writerPlan: func(string) fakeWriterPlan {
			return fakeWriterPlan{}
		},
	}
}

func (b *fakeBucket) Object(name string) objectHandle {
	return &fakeObject{bucket: b, name: name}
}

func (b *fakeBucket) putObject(name string, body []byte) *cloudstorage.ObjectAttrs {
	return b.putObjectWithMetadata(name, body, nil)
}

func (b *fakeBucket) putObjectWithMetadata(name string, body []byte, metadata map[string]string) *cloudstorage.ObjectAttrs {
	attrs := &cloudstorage.ObjectAttrs{
		Name:           name,
		Generation:     b.nextGen,
		Metageneration: 1,
		Size:           int64(len(body)),
		CRC32C:         crc32.Checksum(body, crc32.MakeTable(crc32.Castagnoli)),
		Metadata:       cloneMetadata(metadata),
	}
	md5Sum := md5.Sum(body)
	attrs.MD5 = append([]byte(nil), md5Sum[:]...)
	b.nextGen++
	b.objects[name] = fakeObjectData{
		body:           append([]byte(nil), body...),
		generation:     attrs.Generation,
		metageneration: attrs.Metageneration,
		metadata:       cloneMetadata(metadata),
	}
	return attrs
}

func (b *fakeBucket) currentObject(name string) (fakeObjectData, bool) {
	data, ok := b.objects[name]
	return data, ok
}

type fakeObject struct {
	bucket     *fakeBucket
	name       string
	conditions cloudstorage.Conditions
	generation int64
}

func (o *fakeObject) NewWriter(context.Context) objectWriter {
	plan := fakeWriterPlan{}
	if o.bucket.writerPlan != nil {
		plan = o.bucket.writerPlan(o.name)
	}
	writer := &fakeObjectWriter{
		bucket:     o.bucket,
		name:       o.name,
		conditions: o.conditions,
		plan:       plan,
	}
	o.bucket.lastWriter = writer
	return writer
}

func (o *fakeObject) Attrs(ctx context.Context) (*cloudstorage.ObjectAttrs, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if o.bucket.attrsFailures > 0 {
		o.bucket.attrsFailures--
		return nil, context.DeadlineExceeded
	}
	if errs := o.bucket.attrsErrs[o.name]; len(errs) > 0 {
		err := errs[0]
		o.bucket.attrsErrs[o.name] = errs[1:]
		if err != nil {
			return nil, err
		}
	}
	data, ok := o.lookup()
	if !ok {
		return nil, cloudstorage.ErrObjectNotExist
	}
	return &cloudstorage.ObjectAttrs{
		Name:           o.name,
		Generation:     data.generation,
		Metageneration: data.metageneration,
		Size:           int64(len(data.body)),
		CRC32C:         crc32.Checksum(data.body, crc32.MakeTable(crc32.Castagnoli)),
		MD5:            md5Bytes(data.body),
		Metadata:       cloneMetadata(data.metadata),
	}, nil
}

func (o *fakeObject) If(conditions cloudstorage.Conditions) objectHandle {
	clone := *o
	clone.conditions = conditions
	return &clone
}

func (o *fakeObject) Generation(generation int64) objectHandle {
	clone := *o
	clone.generation = generation
	return &clone
}

func (o *fakeObject) CopierFrom(src objectHandle) objectCopier {
	return &fakeObjectCopier{
		dst: o,
		src: src.(*fakeObject),
	}
}

func (o *fakeObject) Delete(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	o.bucket.deleteCalls = append(o.bucket.deleteCalls, fakeDeleteCall{name: o.name, generation: o.generation})
	if o.bucket.deleteHook != nil {
		if err := o.bucket.deleteHook(ctx, o); err != nil {
			return err
		}
	}
	data, ok := o.lookup()
	if !ok {
		return cloudstorage.ErrObjectNotExist
	}
	if o.generation != 0 && data.generation != o.generation {
		return cloudstorage.ErrObjectNotExist
	}
	delete(o.bucket.objects, o.name)
	return nil
}

func (o *fakeObject) lookup() (fakeObjectData, bool) {
	data, ok := o.bucket.currentObject(o.name)
	if !ok {
		return fakeObjectData{}, false
	}
	if o.generation != 0 && data.generation != o.generation {
		return fakeObjectData{}, false
	}
	return data, true
}

type fakeObjectCopier struct {
	dst *fakeObject
	src *fakeObject
}

func (c *fakeObjectCopier) Run(ctx context.Context) (*cloudstorage.ObjectAttrs, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.dst.bucket.copyCalls = append(c.dst.bucket.copyCalls, fakeCopyCall{
		src:        c.src.name,
		dst:        c.dst.name,
		srcGen:     c.src.generation,
		conditions: c.dst.conditions,
	})
	if c.dst.bucket.copyHook != nil {
		if err := c.dst.bucket.copyHook(ctx, c.dst, c.src); err != nil {
			return nil, err
		}
	}
	srcData, ok := c.src.lookup()
	if !ok {
		return nil, cloudstorage.ErrObjectNotExist
	}
	if err := c.dst.checkConditions(); err != nil {
		return nil, err
	}
	attrs := c.dst.bucket.putObjectWithMetadata(c.dst.name, srcData.body, srcData.metadata)
	if c.dst.bucket.copyAfterCommitErr != nil {
		return nil, c.dst.bucket.copyAfterCommitErr
	}
	return attrs, nil
}

func (o *fakeObject) checkConditions() error {
	return checkObjectConditions(o.bucket, o.name, o.conditions)
}

type fakeObjectWriter struct {
	bucket              *fakeBucket
	name                string
	conditions          cloudstorage.Conditions
	plan                fakeWriterPlan
	buf                 bytes.Buffer
	attrs               *cloudstorage.ObjectAttrs
	metadata            map[string]string
	closeCalls          int
	closeWithErrorCalls int
	closeWithErrorArg   error
}

func (w *fakeObjectWriter) Write(p []byte) (int, error) {
	return w.buf.Write(p)
}

func (w *fakeObjectWriter) Close() error {
	w.closeCalls++
	if err := checkObjectConditions(w.bucket, w.name, w.conditions); err != nil {
		return err
	}
	if w.plan.publishOnClose || w.plan.closeErr == nil {
		w.attrs = w.bucket.putObjectWithMetadata(w.name, w.buf.Bytes(), w.metadata)
		if w.plan.attrsOverride != nil {
			w.attrs = cloneObjectAttrs(w.plan.attrsOverride)
		}
	}
	return w.plan.closeErr
}

func (w *fakeObjectWriter) CloseWithError(err error) error {
	w.closeWithErrorCalls++
	w.closeWithErrorArg = err
	if w.plan.closeAbortFn != nil {
		return w.plan.closeAbortFn(err)
	}
	return w.plan.closeAbortErr
}

func (w *fakeObjectWriter) Attrs() *cloudstorage.ObjectAttrs {
	return w.attrs
}

func (w *fakeObjectWriter) SetMetadata(metadata map[string]string) {
	w.metadata = cloneMetadata(metadata)
}

func checkObjectConditions(bucket *fakeBucket, name string, conditions cloudstorage.Conditions) error {
	current, exists := bucket.currentObject(name)
	switch {
	case conditions.DoesNotExist:
		if exists {
			return fakePreconditionError()
		}
	case conditions.GenerationMatch != 0:
		if !exists ||
			current.generation != conditions.GenerationMatch ||
			current.metageneration != conditions.MetagenerationMatch {
			return fakePreconditionError()
		}
	case conditions.MetagenerationMatch != 0:
		if !exists || current.metageneration != conditions.MetagenerationMatch {
			return fakePreconditionError()
		}
	}
	return nil
}

func fakePreconditionError() error {
	return errors.Join(
		errFakePrecondition,
		&googleapi.Error{Code: http.StatusPreconditionFailed, Message: "conditionNotMet"},
	)
}

func md5Bytes(data []byte) []byte {
	sum := md5.Sum(data)
	return append([]byte(nil), sum[:]...)
}

func cloneMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}
	clone := make(map[string]string, len(metadata))
	for key, value := range metadata {
		clone[key] = value
	}
	return clone
}

func cloneObjectAttrs(attrs *cloudstorage.ObjectAttrs) *cloudstorage.ObjectAttrs {
	if attrs == nil {
		return nil
	}
	clone := *attrs
	clone.MD5 = append([]byte(nil), attrs.MD5...)
	clone.Metadata = cloneMetadata(attrs.Metadata)
	return &clone
}

func captureDefaultLogger(t *testing.T, buf *bytes.Buffer) func() {
	t.Helper()
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	return func() { slog.SetDefault(previous) }
}
