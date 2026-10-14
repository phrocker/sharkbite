// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sourceFixture(t *testing.T) (string, *loadedBundle, sourceManifest) {
	t.Helper()
	dir, mh, ph, m := fixture(t)
	task, e := taskSpec()
	if e != nil {
		t.Fatal(e)
	}
	b, e := loadBundle(dir, mh, ph, task, 1)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { b.Close() })
	if e = os.Mkdir(filepath.Join(dir, "sources"), 0700); e != nil {
		t.Fatal(e)
	}
	raw := []byte("func authorize() bool { return false }")
	write(t, dir, "sources/0000.txt", raw)
	sm := sourceManifest{Schema: 1, Kind: "frozen-code-source-registration", NumericManifestSHA256: ph, NumericModelSHA256: mh, ArchiveSHA256: archiveHash, OriginalInputsSHA256: originalMembers["reserved/inputs-frozen.json"], BuilderSHA256: strings.Repeat("a", 64), Rows: []sourceRow{{ID: m.Rows[0].ID, SourceFile: "sources/0000.txt", SourceSHA256: digest(raw), Bytes: len(raw), Path: "pkg/auth.go", PR: 1, Representation: "cached-v9-code-text"}}, Action: "full_review"}
	return dir, b, sm
}
func TestSourceRegistrationRejectsSubstitutions(t *testing.T) {
	for name, change := range map[string]func(*sourceManifest){
		"none":         func(m *sourceManifest) {},
		"numeric":      func(m *sourceManifest) { m.NumericManifestSHA256 = strings.Repeat("b", 64) },
		"model":        func(m *sourceManifest) { m.NumericModelSHA256 = strings.Repeat("b", 64) },
		"subject":      func(m *sourceManifest) { m.Rows[0].ID = "other" },
		"traversal":    func(m *sourceManifest) { m.Rows[0].SourceFile = "../outside" },
		"digest":       func(m *sourceManifest) { m.Rows[0].SourceSHA256 = strings.Repeat("b", 64) },
		"length":       func(m *sourceManifest) { m.Rows[0].Bytes++ },
		"oversize":     func(m *sourceManifest) { m.Rows[0].Bytes = maxSourceBytes + 1 },
		"historical":   func(m *sourceManifest) { m.HistoricalTimestampsVerified = true },
		"optimization": func(m *sourceManifest) { m.OptimizationEnabled = true },
		"missing":      func(m *sourceManifest) { m.Rows = nil },
	} {
		t.Run(name, func(t *testing.T) {
			dir, b, m := sourceFixture(t)
			change(&m)
			raw, _ := json.Marshal(m)
			write(t, dir, "source-manifest.json", raw)
			got, e := loadSources(dir, digest(raw), b)
			if name == "none" {
				if e != nil || len(got) != 1 || got[0].Digest != m.Rows[0].SourceSHA256 {
					t.Fatalf("valid source: %v", e)
				}
			} else if e == nil {
				t.Fatal("accepted substituted source")
			}
		})
	}
}
func TestSourceManifestStrictFieldsAndPins(t *testing.T) {
	dir, b, m := sourceFixture(t)
	raw, _ := json.Marshal(m)
	for _, bad := range [][]byte{[]byte(strings.Replace(string(raw), `"historical_timestamps_verified":false`, `"historical_timestamps_verified":null`, 1)), []byte(strings.Replace(string(raw), `"schema":1`, `"Schema":1`, 1)), []byte(strings.Replace(string(raw), `"schema":1`, `"schema":1,"schema":1`, 1)), []byte(strings.Replace(string(raw), `"bytes":`, `"Bytes":`, 1))} {
		write(t, dir, "source-manifest.json", bad)
		if _, e := loadSources(dir, digest(bad), b); e == nil {
			t.Fatal("accepted malformed source manifest")
		}
	}
	write(t, dir, "source-manifest.json", raw)
	if _, e := loadSources(dir, strings.Repeat("0", 64), b); e == nil {
		t.Fatal("accepted incorrect source manifest pin")
	}
	if e := os.Remove(filepath.Join(dir, "sources/0000.txt")); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink("../model.json", filepath.Join(dir, "sources/0000.txt")); e != nil {
		t.Fatal(e)
	}
	if _, e := loadSources(dir, digest(raw), b); e == nil {
		t.Fatal("accepted source symlink")
	}
}
