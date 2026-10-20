package azure

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"
)

func snapshotTestBackend(t *testing.T, handler http.HandlerFunc) *Backend {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	svc, err := service.NewClientWithNoCredential(server.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &Backend{svc: svc}
}

func TestOpenSnapshotPinsVersionAcrossReplacement(t *testing.T) {
	var mu sync.Mutex
	current := 0
	backend := snapshotTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		selected := current
		mu.Unlock()
		if r.Method == http.MethodGet {
			switch r.URL.Query().Get("versionid") {
			case "v1":
				selected = 0
			case "v2":
				selected = 1
			default:
				t.Errorf("DownloadStream lacks pinned version: %s", r.URL.String())
			}
		}
		body, etag, version := "first", `"old"`, "v1"
		if selected == 1 {
			body, etag, version = "later", `"new"`, "v2"
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("x-ms-version-id", version)
		w.Header().Set("x-ms-blob-type", "BlockBlob")
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "5")
			return
		}
		if got := r.Header.Get("If-Match"); got != etag {
			t.Errorf("If-Match = %q, want %q", got, etag)
		}
		w.Header().Set("Content-Range", "bytes 0-4/5")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, body)
	})
	snapshot, err := backend.OpenSnapshot(context.Background(), "az://container/file.rf")
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
	backend := snapshotTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		selected := current
		mu.Unlock()
		body, etag := "first", `"old"`
		if selected == 1 {
			body, etag = "later", `"new"`
		}
		w.Header().Set("ETag", etag)
		w.Header().Set("x-ms-blob-type", "BlockBlob")
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
	})
	snapshot, err := backend.OpenSnapshot(context.Background(), "az://container/file.rf")
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
	ordinary, err := backend.Open(context.Background(), "az://container/file.rf")
	if err != nil {
		t.Fatal(err)
	}
	defer ordinary.Close()
	if n, err := ordinary.ReadAt(buf, 0); n != 5 || err != nil || string(buf) != "later" {
		t.Fatalf("ordinary read = %d, %q, %v", n, buf, err)
	}
}

func TestOpenSnapshotRejectsMissingTokensWhileOpenRemainsCompatible(t *testing.T) {
	backend := snapshotTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ms-blob-type", "BlockBlob")
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "5")
			return
		}
		w.Header().Set("Content-Range", "bytes 0-4/5")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "first")
	})
	if _, err := backend.OpenSnapshot(context.Background(), "az://container/file.rf"); err == nil || !strings.Contains(err.Error(), "no version ID or ETag") {
		t.Fatalf("snapshot without token = %v", err)
	}
	f, err := backend.Open(context.Background(), "az://container/file.rf")
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
	backend := snapshotTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"old"`)
		w.Header().Set("x-ms-blob-type", "BlockBlob")
		w.WriteHeader(http.StatusOK)
	})
	if _, err := backend.OpenSnapshot(context.Background(), "az://container/file.rf"); err == nil || !strings.Contains(err.Error(), "no valid size") {
		t.Fatalf("snapshot without size = %v", err)
	}
	if _, err := backend.Open(context.Background(), "az://container/file.rf"); err != nil {
		t.Fatalf("ordinary Open without size changed: %v", err)
	}
}

func TestSnapshotReadRejectsSubstitutedResponse(t *testing.T) {
	backend := snapshotTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ms-blob-type", "BlockBlob")
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
	})
	f, err := backend.OpenSnapshot(context.Background(), "az://container/file.rf")
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
	backend := snapshotTestBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ms-blob-type", "BlockBlob")
		w.Header().Set("ETag", `"old"`)
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "5")
			return
		}
		w.Header().Set("Content-Range", "bytes 1-5/5")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "first")
	})
	f, err := backend.OpenSnapshot(context.Background(), "az://container/file.rf")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 5)
	if n, err := f.ReadAt(buf, 0); n != 0 || err == nil || !strings.Contains(err.Error(), "different range") || string(buf) != "\x00\x00\x00\x00\x00" {
		t.Fatalf("substituted range read = %d, %q, %v", n, buf, err)
	}
}
