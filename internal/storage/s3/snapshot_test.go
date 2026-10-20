package s3

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestOpenSnapshotPinsVersionAcrossReplacement(t *testing.T) {
	var mu sync.Mutex
	current := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		selected := current
		mu.Unlock()
		if r.Method == http.MethodGet {
			switch r.URL.Query().Get("versionId") {
			case "v1":
				selected = 0
			case "v2":
				selected = 1
			default:
				t.Errorf("GetObject lacks pinned version: %s", r.URL.String())
			}
		}
		body, etag, version := "first", `"old"`, "v1"
		if selected == 1 {
			body, etag, version = "later", `"new"`, "v2"
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("x-amz-version-id", version)
		if r.Method == http.MethodGet {
			if got := r.Header.Get("If-Match"); got != etag {
				t.Errorf("If-Match = %q, want %q", got, etag)
			}
			w.Header().Set("Content-Range", "bytes 0-4/5")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, body)
			return
		}
		w.Header().Set("Content-Length", "5")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	backend := &Backend{client: newTestS3Client(server)}
	snapshot, err := backend.OpenSnapshot(context.Background(), "s3://bucket/file.rf")
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	mu.Lock()
	current = 1
	mu.Unlock()
	buf := make([]byte, 5)
	if n, err := snapshot.ReadAt(buf, 0); n != 5 || err != nil || string(buf) != "first" {
		t.Fatalf("pinned read = %d, %q, %v", n, buf, err)
	}
}

func TestOpenSnapshotETagRefusesReplacement(t *testing.T) {
	var mu sync.Mutex
	current := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		selected := current
		mu.Unlock()
		body, etag := "first", `"old"`
		if selected == 1 {
			body, etag = "later", `"new"`
		}
		w.Header().Set("ETag", etag)
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "5")
			return
		}
		if got := r.Header.Get("If-Match"); got != "" && got != etag {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		w.Header().Set("Content-Range", "bytes 0-4/5")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	backend := &Backend{client: newTestS3Client(server)}
	snapshot, err := backend.OpenSnapshot(context.Background(), "s3://bucket/file.rf")
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	mu.Lock()
	current = 1
	mu.Unlock()
	buf := make([]byte, 5)
	if n, err := snapshot.ReadAt(buf, 0); n != 0 || err == nil || string(buf) != "\x00\x00\x00\x00\x00" {
		t.Fatalf("replacement read = %d, %q, %v", n, buf, err)
	}
	ordinary, err := backend.Open(context.Background(), "s3://bucket/file.rf")
	if err != nil {
		t.Fatal(err)
	}
	defer ordinary.Close()
	if n, err := ordinary.ReadAt(buf, 0); n != 5 || err != nil || string(buf) != "later" {
		t.Fatalf("ordinary read = %d, %q, %v", n, buf, err)
	}
}

func TestOpenSnapshotRejectsMissingTokensWhileOpenRemainsCompatible(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "5")
			return
		}
		w.Header().Set("Content-Range", "bytes 0-4/5")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "first")
	}))
	defer server.Close()
	backend := &Backend{client: newTestS3Client(server)}
	if _, err := backend.OpenSnapshot(context.Background(), "s3://bucket/file.rf"); err == nil || !strings.Contains(err.Error(), "no version ID or ETag") {
		t.Fatalf("snapshot without token = %v", err)
	}
	f, err := backend.Open(context.Background(), "s3://bucket/file.rf")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 5)
	if n, err := f.ReadAt(buf, 0); n != 5 || err != nil || string(buf) != "first" {
		t.Fatalf("ordinary tokenless read = %d, %q, %v", n, buf, err)
	}
}

func TestOpenSnapshotRejectsMissingSize(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"old"`)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	backend := &Backend{client: newTestS3Client(server)}
	if _, err := backend.OpenSnapshot(context.Background(), "s3://bucket/file.rf"); err == nil || !strings.Contains(err.Error(), "no valid size") {
		t.Fatalf("snapshot without size = %v", err)
	}
	if _, err := backend.Open(context.Background(), "s3://bucket/file.rf"); err != nil {
		t.Fatalf("ordinary Open without size changed: %v", err)
	}
}

func TestSnapshotReadRejectsSubstitutedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "5")
			w.Header().Set("ETag", `"old"`)
			return
		}
		// A nonconforming endpoint ignored If-Match but reported the new ETag.
		w.Header().Set("ETag", `"new"`)
		w.Header().Set("Content-Range", "bytes 0-4/5")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "later")
	}))
	defer server.Close()
	backend := &Backend{client: newTestS3Client(server)}
	f, err := backend.OpenSnapshot(context.Background(), "s3://bucket/file.rf")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 5)
	if n, err := f.ReadAt(buf, 0); n != 0 || err == nil || !strings.Contains(err.Error(), "different ETag") {
		t.Fatalf("substituted response = %d, %q, %v", n, buf, err)
	}
}

func TestSnapshotReadRejectsSubstitutedRange(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"old"`)
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "5")
			return
		}
		w.Header().Set("Content-Range", "bytes 1-5/5")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "first")
	}))
	defer server.Close()
	backend := &Backend{client: newTestS3Client(server)}
	f, err := backend.OpenSnapshot(context.Background(), "s3://bucket/file.rf")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 5)
	if n, err := f.ReadAt(buf, 0); n != 0 || err == nil || !strings.Contains(err.Error(), "different range") || string(buf) != "\x00\x00\x00\x00\x00" {
		t.Fatalf("substituted range read = %d, %q, %v", n, buf, err)
	}
}
