// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

package service

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestInitJournalIsAtomic: init leaves only journal.log (and the lock),
// never a temporary; a temporary left by a killed init does not block the
// next one; and init never replaces an existing journal.
func TestInitJournalIsAtomic(t *testing.T) {
	dir := t.TempDir()
	leftover := filepath.Join(dir, JournalFileName+".init")
	if err := os.WriteFile(leftover, []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := InitJournal(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if entry.Name() != JournalFileName && entry.Name() != LockFileName {
			t.Errorf("init left %s", entry.Name())
		}
	}
	got, err := ReadJournalID(dir)
	if err != nil || got != id {
		t.Fatalf("ReadJournalID = %q %v, want %q", got, err, id)
	}
	before, _ := os.ReadFile(filepath.Join(dir, JournalFileName))
	if _, err := InitJournal(dir, nil); err == nil {
		t.Fatal("a second init succeeded")
	}
	after, _ := os.ReadFile(filepath.Join(dir, JournalFileName))
	if string(before) != string(after) {
		t.Fatal("a second init changed the journal")
	}
}

// TestReadJournalIDReadsOnlyTheHeader: the header is checked; what follows
// it is not read, so a journal of any size is answered.
func TestReadJournalIDReadsOnlyTheHeader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, JournalFileName)
	header := headerLine(t)
	// A header followed by a line no reader would accept: only the header
	// is read, so the identity is still returned.
	if err := os.WriteFile(path, append(append([]byte{}, header...), []byte("not a record\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if id, err := ReadJournalID(dir); err != nil || id != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("ReadJournalID = %q %v", id, err)
	}
	// A symlinked journal.log is refused, as the server refuses it.
	elsewhere := filepath.Join(t.TempDir(), "real.log")
	_ = os.WriteFile(elsewhere, header, 0o600)
	_ = os.Remove(path)
	if err := os.Symlink(elsewhere, path); err != nil {
		t.Fatal(err)
	}
	if id, err := ReadJournalID(dir); err == nil || id != "" {
		t.Fatalf("a symlinked journal answered %q", id)
	}
	_ = os.Remove(path)
	for name, content := range map[string][]byte{
		"empty":       nil,
		"torn header": header[:len(header)-1],
		"no header":   []byte("not a record\n"),
	} {
		_ = os.WriteFile(path, content, 0o600)
		id, err := ReadJournalID(dir)
		if err == nil || id != "" {
			t.Errorf("%s: %q %v", name, id, err)
		}
		var corrupt *CorruptError
		if name != "no header" && !errors.Is(err, ErrJournalUninitialized) && !errors.As(err, &corrupt) {
			t.Errorf("%s: unexpected error %v", name, err)
		}
	}
}
