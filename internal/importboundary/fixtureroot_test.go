// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package importboundary

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Every fixture must be its own module; that, not a blanket skip, is what
// keeps the deliberately violating fixtures out of the real tree's check.
func TestEveryFixtureIsItsOwnModule(t *testing.T) {
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			t.Errorf("testdata/%s is not a fixture directory", e.Name())
			continue
		}
		info, err := os.Lstat(filepath.Join("testdata", e.Name(), "go.mod"))
		if err != nil || !info.Mode().IsRegular() {
			t.Errorf("testdata/%s has no go.mod", e.Name())
		}
	}
}

// A Go file under FixtureRoot without its own go.mod belongs to the root
// module and is checked; a fixture module beside it stays exempt.
func TestFixtureRootIsNotABlanketExemption(t *testing.T) {
	const m = Module
	dir := t.TempDir()
	copyTree(t, filepath.Join("testdata", "clean"), dir)
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Root-module bridge hidden among fixtures, reached by explicit path.
	write(FixtureRoot+"/bridge/bridge.go", "package bridge\n\nimport _ \""+m+"/extensions/good\"\n")
	write("pkg/zz/zz.go", "package zz\n\nimport _ \""+m+"/"+FixtureRoot+"/bridge\"\n")
	// A nested module inside a non-module fixture-root child is walked too.
	write(FixtureRoot+"/loose/inner/go.mod", "module "+m+"/inner\n\ngo 1.25.0\n")
	write(FixtureRoot+"/loose/inner/x.go", "package inner\n\nimport _ \""+m+"/extensions/good\"\n")
	// A real fixture module stays exempt.
	write(FixtureRoot+"/fixture/go.mod", "module "+m+"\n\ngo 1.25.0\n")
	write(FixtureRoot+"/fixture/cmd/x/x.go", "package main\n\nimport _ \""+m+"/extensions/good\"\n")
	want := []Violation{
		{"A", FixtureRoot + "/bridge/bridge.go", m + "/extensions/good"},
		{"A", FixtureRoot + "/loose/inner/x.go", m + "/extensions/good"},
	}
	if got := check(t, os.DirFS(dir)); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
