package embeddingsidecar

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testKeyringFile(t *testing.T, data string, mode os.FileMode) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "keys.json")
	if err := os.WriteFile(filename, []byte(data), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filename, mode); err != nil {
		t.Fatal(err)
	}
	return filename
}

func testKeyringJSON(namespace, current string, keys ...string) string {
	items := make([]string, 0, len(keys))
	for _, id := range keys {
		material := base64.StdEncoding.EncodeToString([]byte(strings.Repeat(id, 32)))
		items = append(items, `{"id":"`+id+`","material_base64":"`+material+`"}`)
	}
	return `{"version":1,"namespace":"` + namespace + `","current":"` + current +
		`","keys":[` + strings.Join(items, ",") + `]}`
}

func TestHMACKeyringScopesSignaturesAndRetainsHistoricalKeys(t *testing.T) {
	ctx := context.Background()
	old, err := LoadHMACKeyring(testKeyringFile(t,
		testKeyringJSON("cluster-a", "old", "old"), 0o600), "cluster-a")
	if err != nil {
		t.Fatal(err)
	}
	keyID, err := old.KeyID(ctx)
	if err != nil || keyID != "old" {
		t.Fatalf("old key ID = %q, %v", keyID, err)
	}
	payload := []byte("sharkbite.embedding-publication.v1\x00marker")
	signature, err := old.Sign(ctx, keyID, payload)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := LoadHMACKeyring(testKeyringFile(t,
		testKeyringJSON("cluster-a", "new", "old", "new"), 0o400), "cluster-a")
	if err != nil {
		t.Fatal(err)
	}
	verified, err := rotated.Verify(ctx, "old", payload, signature)
	if err != nil || !verified {
		t.Fatalf("historical signature = %t, %v", verified, err)
	}
	if _, err := rotated.Sign(ctx, "old", payload); !errors.Is(err, ErrInvalid) {
		t.Fatalf("sign with retired key = %v", err)
	}
	newSignature, err := rotated.Sign(ctx, "new", payload)
	if err != nil || newSignature == signature {
		t.Fatalf("new signature reused old key: %v", err)
	}
	verified, err = rotated.Verify(ctx, "old", append(payload, '!'), signature)
	if err != nil || verified {
		t.Fatalf("tampered payload = %t, %v", verified, err)
	}
	other, err := LoadHMACKeyring(testKeyringFile(t,
		testKeyringJSON("cluster-b", "old", "old"), 0o600), "cluster-b")
	if err != nil {
		t.Fatal(err)
	}
	verified, err = other.Verify(ctx, "old", payload, signature)
	if err != nil || verified {
		t.Fatalf("cross-namespace signature = %t, %v", verified, err)
	}
}

func TestHMACKeyringRejectsUnsafeConfiguration(t *testing.T) {
	for _, item := range []struct {
		name, document, namespace string
		mode                      os.FileMode
	}{
		{"world-readable", testKeyringJSON("cluster", "one", "one"), "cluster", 0o644},
		{"namespace-mismatch", testKeyringJSON("cluster", "one", "one"), "another", 0o600},
		{"missing-current", testKeyringJSON("cluster", "missing", "one"), "cluster", 0o600},
		{"duplicate-id", testKeyringJSON("cluster", "one", "one", "one"), "cluster", 0o600},
		{"unknown-field", strings.Replace(testKeyringJSON("cluster", "one", "one"),
			`"version":1`, `"version":1,"extra":true`, 1), "cluster", 0o600},
		{"duplicate-top-level-field", strings.Replace(testKeyringJSON("cluster", "one", "one"),
			`"version":1`, `"version":1,"version":1`, 1), "cluster", 0o600},
		{"duplicate-key-field", strings.Replace(testKeyringJSON("cluster", "one", "one"),
			`"id":"one"`, `"id":"one","id":"one"`, 1), "cluster", 0o600},
		{"weak-key", `{"version":1,"namespace":"cluster","current":"one",` +
			`"keys":[{"id":"one","material_base64":"YQ=="}]}`, "cluster", 0o600},
		{"invalid-key-id", testKeyringJSON("cluster", "bad/id", "bad/id"), "cluster", 0o600},
		{"trailing-json", testKeyringJSON("cluster", "one", "one") + `{}`, "cluster", 0o600},
	} {
		t.Run(item.name, func(t *testing.T) {
			filename := testKeyringFile(t, item.document, item.mode)
			if _, err := LoadHMACKeyring(filename, item.namespace); !errors.Is(err, ErrInvalid) {
				t.Fatalf("LoadHMACKeyring = %v", err)
			}
		})
	}
}

func TestHMACKeyringHonorsCancelledContext(t *testing.T) {
	ring, err := LoadHMACKeyring(testKeyringFile(t,
		testKeyringJSON("cluster", "one", "one"), 0o600), "cluster")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ring.KeyID(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("KeyID after cancel = %v", err)
	}
	if _, err := ring.Sign(ctx, "one", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sign after cancel = %v", err)
	}
	if _, err := ring.Verify(ctx, "one", nil, strings.Repeat("0", 64)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Verify after cancel = %v", err)
	}
}
