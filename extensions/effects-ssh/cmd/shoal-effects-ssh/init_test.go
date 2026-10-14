// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/extensions/effects-ssh/service"
)

// TestJournalInitPrintExisting: with -print-existing, a second init prints
// the identity the first wrote and changes nothing; a headerless or corrupt
// journal is still an error, never replaced; without the flag a second init
// is refused as before.
func TestJournalInitPrintExisting(t *testing.T) {
	dir := t.TempDir()
	var first bytes.Buffer
	if code := run([]string{"journal", "init", "-print-existing", "-journal-dir", dir}, &first, io.Discard, nil); code != exitOK {
		t.Fatalf("first init exit %d", code)
	}
	id := strings.TrimSpace(first.String())
	if !service.ValidJournalID(id) {
		t.Fatalf("first init printed %q", id)
	}
	before, _ := os.ReadFile(filepath.Join(dir, service.JournalFileName))
	var second bytes.Buffer
	if code := run([]string{"journal", "init", "-print-existing", "-journal-dir", dir}, &second, io.Discard, nil); code != exitOK {
		t.Fatalf("second init exit %d", code)
	}
	if strings.TrimSpace(second.String()) != id {
		t.Fatalf("second init printed %q, not %q", second.String(), id)
	}
	after, _ := os.ReadFile(filepath.Join(dir, service.JournalFileName))
	if !bytes.Equal(before, after) {
		t.Fatal("the journal was changed")
	}
	if code := run([]string{"journal", "init", "-journal-dir", dir}, io.Discard, io.Discard, nil); code != exitRuntime {
		t.Fatalf("init without -print-existing on an existing journal exit %d", code)
	}

	for name, content := range map[string][]byte{
		"headerless": nil,
		"corrupt":    []byte("{\"op\":\"header\"}\n"),
	} {
		bad := t.TempDir()
		path := filepath.Join(bad, service.JournalFileName)
		_ = os.WriteFile(path, content, 0o600)
		var out bytes.Buffer
		if code := run([]string{"journal", "init", "-print-existing", "-journal-dir", bad}, &out, io.Discard, nil); code != exitRuntime || out.Len() != 0 {
			t.Errorf("%s: exit %d, printed %q", name, code, out.String())
		}
		if got, _ := os.ReadFile(path); !bytes.Equal(got, content) {
			t.Errorf("%s: the journal was changed", name)
		}
	}
}
