package cclient

import (
	"bytes"
	"testing"
)

func TestNewAuthorizations_DedupAndSort(t *testing.T) {
	a, err := NewAuthorizations("SECRET", "PUBLIC", "PUBLIC", "RESTRICTED")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	got := a.Strings()
	want := []string{"PUBLIC", "RESTRICTED", "SECRET"}
	if len(got) != len(want) {
		t.Fatalf("got %d auths, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("auths[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestNewAuthorizations_TrimsWhitespace(t *testing.T) {
	a, err := NewAuthorizations("  PUBLIC  ", "\tSECRET\n")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !a.Contains("PUBLIC") || !a.Contains("SECRET") {
		t.Errorf("trim failed: %v", a.Strings())
	}
}

func TestNewAuthorizations_Validation(t *testing.T) {
	tests := []struct {
		name    string
		labels  []string
		wantErr string
	}{
		{"empty-string", []string{""}, "empty"},
		{"whitespace-only", []string{"   "}, "empty"},
		{"bad-char-space", []string{"HAS SPACE"}, "invalid authorization character"},
		{"bad-char-comma", []string{"A,B"}, "invalid authorization character"},
		{"bad-char-paren", []string{"(secret)"}, "invalid authorization character"},
		{"ok-allowed-symbols", []string{"a-b_c.d:e/f"}, ""},
		{"ok-mixed-case-digits", []string{"ABCxyz123"}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewAuthorizations(tc.labels...)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("err: %v", err)
				}
				return
			}
			if err == nil || !contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestParseAuthorizations(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"empty-string", "", nil},
		{"whitespace-only", "   ", nil},
		{"single", "PUBLIC", []string{"PUBLIC"}},
		{"multi-with-spaces", "PUBLIC, SECRET ,RESTRICTED", []string{"PUBLIC", "RESTRICTED", "SECRET"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, err := ParseAuthorizations(tc.in)
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			got := a.Strings()
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d want %d (%v vs %v)", len(got), len(tc.want), got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("[%d] %q vs %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestAuthorizations_ToThrift_NilForEmpty(t *testing.T) {
	if got := EmptyAuthorizations().ToThrift(); got != nil {
		t.Errorf("ToThrift on empty = %v, want nil", got)
	}
}

func TestAuthorizations_ToThrift_Roundtrip(t *testing.T) {
	a, err := NewAuthorizations("ZULU", "ALPHA", "MIKE")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	wire := a.ToThrift()
	if len(wire) != 3 {
		t.Fatalf("wire len = %d", len(wire))
	}
	// Wire must be sorted (ALPHA, MIKE, ZULU).
	if !bytes.Equal(wire[0], []byte("ALPHA")) || !bytes.Equal(wire[1], []byte("MIKE")) || !bytes.Equal(wire[2], []byte("ZULU")) {
		t.Errorf("wire not sorted: %q %q %q", wire[0], wire[1], wire[2])
	}
	back, err := AuthorizationsFromThrift(wire)
	if err != nil {
		t.Fatalf("FromThrift: %v", err)
	}
	if back.String() != a.String() {
		t.Errorf("roundtrip mismatch: %q vs %q", back, a)
	}
}

func TestAuthorizations_StringsCopy(t *testing.T) {
	a, _ := NewAuthorizations("A", "B")
	got := a.Strings()
	got[0] = "MUTATED"
	if a.Strings()[0] == "MUTATED" {
		t.Error("Strings() must return a defensive copy")
	}
}

func TestAuthorizations_String(t *testing.T) {
	a, _ := NewAuthorizations("PUBLIC", "SECRET")
	if got := a.String(); got != "PUBLIC,SECRET" {
		t.Errorf("String() = %q", got)
	}
	if got := EmptyAuthorizations().String(); got != "" {
		t.Errorf("empty String() = %q", got)
	}
}

func TestAuthorizationsFromThrift_Empty(t *testing.T) {
	a, err := AuthorizationsFromThrift(nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !a.Empty() {
		t.Error("nil wire should produce Empty()")
	}
}
