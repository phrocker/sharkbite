package accumulo

import (
	"bytes"
	"testing"
)

func TestMutationPublicModel(t *testing.T) {
	row := []byte("row")
	mutation, err := NewMutation(row)
	if err != nil {
		t.Fatal(err)
	}
	row[0] = 'x'

	mutation.PutLatest([]byte("cf"), []byte("cq"), nil, []byte("value"))
	mutation.Delete([]byte("cf"), []byte("old"), []byte("private"), 42)

	if got := mutation.Row(); !bytes.Equal(got, []byte("row")) {
		t.Fatalf("Row = %q, want row", got)
	}
	if mutation.Size() != 2 {
		t.Fatalf("Size = %d, want 2", mutation.Size())
	}
}

func TestNewMutationRejectsEmptyRow(t *testing.T) {
	if _, err := NewMutation(nil); err == nil {
		t.Fatal("expected empty row error")
	}
}
