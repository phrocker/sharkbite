/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package lexiconscope

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/graph"
)

const importPath = "github.com/phrocker/shoal-oss/internal/lexiconscope"

// allowedImporters are the only directories, relative to the module root,
// whose Go files (tests included) may import this package.
var allowedImporters = map[string]bool{
	"pkg/lexicon":             true,
	"pkg/explorer/authorized": true,
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("module root not found")
		}
		dir = parent
	}
}

// importers lists the module-relative directories of every Go file under
// root that imports this package. Only .git and nested modules (directories
// with their own go.mod, which cannot import a module-internal package) are
// skipped: Go builds packages under testdata/, node_modules/, vendor/ and dot
// directories when they are imported by explicit path, so those are scanned.
// A file whose header does not fully parse still has its parsed imports
// checked.
func importers(t *testing.T, root string) []string {
	t.Helper()
	found := map[string]bool{}
	fileSet := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			if path != root && name == ".git" {
				return filepath.SkipDir
			}
			if path != root {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, _ := parser.ParseFile(fileSet, path, nil, parser.ImportsOnly)
		if file == nil {
			return nil
		}
		for _, spec := range file.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				// A spec the parser could not read names no package.
				continue
			}
			if imported == importPath {
				rel, err := filepath.Rel(root, filepath.Dir(path))
				if err != nil {
					return err
				}
				found[filepath.ToSlash(rel)] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var dirs []string
	for dir := range found {
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
}

func TestOnlyLexiconAndAuthorizedImportLexiconScope(t *testing.T) {
	dirs := importers(t, moduleRoot(t))
	for _, dir := range dirs {
		if !allowedImporters[dir] {
			t.Errorf("%s imports %s; only pkg/lexicon and pkg/explorer/authorized may",
				dir, importPath)
		}
	}
	// The scan has teeth: both allowed importers are found.
	for dir := range allowedImporters {
		if !slices.Contains(dirs, dir) {
			t.Errorf("scan did not find the import in %s; found %q", dir, dirs)
		}
	}
}

// TestImporterWalkCoversDirectoriesGoBuildsByPath runs the walk over a
// synthetic module with importers where a skip list would miss them.
func TestImporterWalkCoversDirectoriesGoBuildsByPath(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	importer := func(pkg string) string {
		return "package " + pkg + "\n\nimport _ \"" + importPath + "\"\n"
	}
	write("go.mod", "module example.test\n\ngo 1.25\n")
	write("pkg/lexicon/ok.go", importer("lexicon"))
	write("pkg/graph/testdata/evil/evil.go", importer("evil"))
	write(".hidden/evil/evil.go", importer("evil"))
	write("web/node_modules/evil/evil.go", importer("evil"))
	write("vendor/evil/evil.go", importer("evil"))
	// The import header itself is malformed after the import that matters;
	// the parser still reports the imports it read.
	write("broken/evil.go", "package evil\n\nimport (\n\t_ \""+importPath+"\"\n\t!!\n")
	// Not scanned: .git, and a nested module, which cannot import this
	// module's internal packages.
	write(".git/evil/evil.go", importer("evil"))
	write("nested/go.mod", "module example.test/nested\n")
	write("nested/evil/evil.go", importer("evil"))

	got := importers(t, root)
	want := []string{
		".hidden/evil", "broken", "pkg/graph/testdata/evil", "pkg/lexicon",
		"vendor/evil", "web/node_modules/evil",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("importers = %q, want %q", got, want)
	}
}

func TestInstallIsSetOnce(t *testing.T) {
	saved := sealer
	t.Cleanup(func() { sealer = saved })
	sealer = nil
	first := Sealer(func([]graph.Node, string, time.Time, uint64, [32]byte) any { return 1 })
	Install(first)
	if got := Seal(nil, "", time.Time{}, 0, [32]byte{}); got != 1 {
		t.Fatalf("Seal = %v", got)
	}
	for name, seal := range map[string]Sealer{"second": first, "nil": nil} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("%s Install did not panic", name)
				}
			}()
			Install(seal)
		}()
	}
	sealer = nil
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("Seal without a sealer did not panic")
			}
		}()
		Seal(nil, "", time.Time{}, 0, [32]byte{})
	}()
}
