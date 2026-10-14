//go:build !embed

package cclient

import (
	"bytes"
	"testing"

	"github.com/phrocker/shoal-oss/internal/thrift/gen/data"
)

func TestNewKeyExtent_Validation(t *testing.T) {
	tests := []struct {
		name       string
		tableID    string
		endRow     []byte
		prevEndRow []byte
		wantErr    string
	}{
		{"ok-bounded", "1", []byte("z"), []byte("a"), ""},
		{"ok-no-end", "1", nil, []byte("a"), ""},
		{"ok-no-prev", "1", []byte("z"), nil, ""},
		{"ok-empty-prev-coerced-to-nil", "1", []byte("z"), []byte{}, ""},
		{"ok-empty-end-coerced-to-nil", "1", []byte{}, []byte("a"), ""},
		{"ok-fully-open", "1", nil, nil, ""},
		{"err-empty-table", "", nil, nil, "tableID must be non-empty"},
		{"err-prev-equal-end", "1", []byte("a"), []byte("a"), "prevEndRow"},
		{"err-prev-greater", "1", []byte("a"), []byte("b"), "prevEndRow"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ke, err := NewKeyExtent(tc.tableID, tc.endRow, tc.prevEndRow)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if ke == nil {
					t.Fatal("nil KeyExtent on success")
				}
				return
			}
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.wantErr)
			}
			if !contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestKeyExtent_NormalizeEmptyToNil(t *testing.T) {
	ke, err := NewKeyExtent("1", []byte{}, []byte{})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ke.EndRow() != nil {
		t.Errorf("EndRow not normalized to nil: %v", ke.EndRow())
	}
	if ke.PrevEndRow() != nil {
		t.Errorf("PrevEndRow not normalized to nil: %v", ke.PrevEndRow())
	}
}

func TestKeyExtent_ToThrift_NilsArePreserved(t *testing.T) {
	ke, err := NewKeyExtent("metadata", nil, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	got := ke.ToThrift()
	if !bytes.Equal(got.Table, []byte("metadata")) {
		t.Errorf("Table = %q", got.Table)
	}
	if got.EndRow != nil {
		t.Errorf("EndRow = %v, want nil (not zero []byte)", got.EndRow)
	}
	if got.PrevEndRow != nil {
		t.Errorf("PrevEndRow = %v, want nil (not zero []byte)", got.PrevEndRow)
	}
}

func TestKeyExtent_ToThrift_BoundedRoundtrip(t *testing.T) {
	src, err := NewKeyExtent("3", []byte("zzz"), []byte("aaa"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	tk := src.ToThrift()
	if string(tk.Table) != "3" || string(tk.EndRow) != "zzz" || string(tk.PrevEndRow) != "aaa" {
		t.Fatalf("Thrift round-trip lost data: %+v", tk)
	}
	back, err := KeyExtentFromThrift(tk)
	if err != nil {
		t.Fatalf("FromThrift: %v", err)
	}
	if !back.Equal(src) {
		t.Errorf("roundtrip not equal: %s vs %s", back, src)
	}
}

func TestKeyExtentFromThrift_Errors(t *testing.T) {
	if _, err := KeyExtentFromThrift(nil); err == nil {
		t.Error("nil TKeyExtent should error")
	}
	// Empty Table (length zero) re-fails validation.
	if _, err := KeyExtentFromThrift(&data.TKeyExtent{}); err == nil {
		t.Error("empty Table should error")
	}
}

func TestKeyExtent_String(t *testing.T) {
	ke, _ := NewKeyExtent("t", []byte("z"), []byte("a"))
	if got := ke.String(); got != "tableId:t end:z prev:a" {
		t.Errorf("String = %q", got)
	}
	open, _ := NewKeyExtent("t", nil, nil)
	if got := open.String(); got != "tableId:t end:< prev:<" {
		t.Errorf("open String = %q", got)
	}
}
