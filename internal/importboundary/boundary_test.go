// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package importboundary

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func check(t *testing.T, fsys fs.FS) []Violation {
	t.Helper()
	got, err := Check(fsys)
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(got, func(i, j int) bool { return got[i].String() < got[j].String() })
	return got
}

// TestRepositoryRespectsBoundary runs the rules over the real tree. It reads
// source files directly, so it behaves the same with GOWORK=off.
func TestRepositoryRespectsBoundary(t *testing.T) {
	root := repoFS(t)
	for _, v := range check(t, root) {
		t.Error(v)
	}
	// The allowlist must name real packages, or rule C passes vacuously.
	for _, pkg := range Allowlist {
		dir := pkg[len(Module)+1:]
		matches, err := fs.Glob(root, path.Join(dir, "*.go"))
		if err != nil || len(matches) == 0 {
			t.Errorf("allowlisted package %s has no Go files", pkg)
		}
	}
	// Rule A must reach root-module packages outside pkg/, internal/, cmd/.
	outside := 0
	if err := walkGo(root, ".", func(dir string) bool { return dir == "extensions" || dir == ".git" }, func(file string, _ []string) error {
		if top, _, _ := strings.Cut(file, "/"); top != "pkg" && top != "internal" && top != "cmd" {
			outside++
		}
		return nil
	}, nil); err != nil || outside == 0 {
		t.Errorf("rule A scanned no files outside pkg/internal/cmd (%v)", err)
	}
	// And the scaffolding the rules exist for must be present.
	if mods, _ := fs.Glob(root, "extensions/*/go.mod"); len(mods) == 0 {
		t.Error("no extension module found; rule B checked nothing")
	}
}

func TestFixturesDetectEachDirection(t *testing.T) {
	const m = Module
	cases := map[string][]Violation{
		"clean":                  nil,
		"core-imports-extension": {{"A", "cmd/tool/main.go", m + "/extensions/good/lines"}},
		"extension-imports-internal": {
			{"B", "extensions/bad/main.go", m + "/internal/engine"},
			{"B", "extensions/bad/main.go", m + "/pkg/explorer/webapi"},
		},
		"sdk-leaks-internal": {{"C", "pkg/helper/helper.go", m + "/internal/secret"}},
		// The admission contracts are allowlisted: an extension may import
		// them, and they may not reach internal/ either.
		"extension-imports-admission-api": nil,
		"admission-api-leaks-internal":    {{"C", "pkg/admission/api/api.go", m + "/internal/secret"}},
		// So are the dispatch contracts, and rule C follows their closure:
		// pkg/executorref, which they import, may not reach internal/.
		"extension-imports-dispatch-api": nil,
		"dispatch-api-leaks-internal":    {{"C", "pkg/executorref/executorref.go", m + "/internal/secret"}},
		// A module naming itself into the repository cannot exempt its imports.
		"extension-module-spoof": {{"B", "extensions/spoof/go.mod", "module " + m + "/internal (want " + m + "/extensions/spoof)"}},
		"extension-bad-replace": {
			{"B", "extensions/r/go.mod", "replace " + m + " => ../../../elsewhere"},
			{"B", "extensions/r/go.mod", "replace golang.org/x/crypto => ./fork"},
		},
		"nested-extension-module":        {{"B", "extensions/grp/evil/main.go", m + "/internal/engine"}},
		"root-package-imports-extension": {{"A", "showcases/demo/main.go", m + "/extensions/good/lines"}},
		"loose-extension-file":           {{"B", "extensions/loose/loose.go", "(file outside an extension module)"}},
		// Go compiles _, ., testdata and vendor directories when imported by
		// explicit path, so the walk must not skip them.
		"extension-underscore-impl":  {{"B", "extensions/e/_impl/impl.go", m + "/internal/engine"}},
		"extension-testdata-package": {{"B", "extensions/e/testdata/x/x.go", m + "/internal/engine"}},
		"core-underscore-package":    {{"A", "pkg/_x/x.go", m + "/extensions/good/lines"}},
		"gomod-glued-paren":          {{"B", "extensions/e/go.mod", "replace " + m + " => ../../../evil"}},
		"gomod-unknown-directive":    {{"B", "extensions/e/go.mod", "(malformed go.mod: extensions/e/go.mod:5: unknown directive substitute)"}},
		// Core links an extension through a nested module outside
		// extensions/, wired by the root go.mod or by go.work alone.
		"nested-module-bridge": {
			{"A", "go.mod", "replace example.com/shim => ./extensions/e"},
			{"A", "go.mod", "replace " + m + "/extensions/e => ./extensions/e"},
			{"A", "go.mod", "require " + m + "/extensions/e"},
			{"A", "hidden/bridge/bridge.go", m + "/extensions/e"},
		},
		// A local replace may only point at code rule A walks; a fixture
		// module under FixtureRoot and an absolute path are not.
		"root-replace-to-fixture": {
			{"A", "go.mod", "replace example.com/abs => /tmp/evil"},
			{"A", "go.mod", "replace example.com/fx => ./" + FixtureRoot + "/fx"},
		},
		"nested-replace-to-fixture": {
			{"A", "hidden/go.mod", "replace example.com/fx => ../" + FixtureRoot + "/fx"},
		},
		// cgo compiles or links paths no Go import names. Each tree also
		// carries the accepted shoal-capi pattern, which must pass.
		// The cgo allowlist: the compliant shoal-capi shape passes, and each
		// preprocessor probe that beat the old deny-list is caught.
		"cgo-allowed": nil,
		"cgo-probe-angle": {
			{"A", "cmd/shoal-capi/probe.go", "cgo flag -I${SRCDIR}/../.."},
			{"A", "cmd/shoal-capi/probe.go", "include <extensions/e/leak.h> (system header with a path)"},
		},
		"cgo-probe-macro":        {{"A", "cmd/shoal-capi/probe.go", "include H (not a literal path)"}},
		"cgo-probe-inc":          {{"A", "cmd/shoal-capi/p.inc", `include "../../extensions/e/leak.h" (not found in allowed directories)`}},
		"cgo-probe-continuation": {{"A", "cmd/shoal-capi/probe.go", `include "../../extensions/e/leak.h" (not found in allowed directories)`}},
		"cgo-probe-comment":      {{"A", "cmd/shoal-capi/probe.go", `include "../../extensions/e/leak.h" (not found in allowed directories)`}},
		// #cgo arguments are an exact allowlist: -I${SRCDIR}/... into allowed
		// directories and plain -D only. An angle include found through an
		// allowed -I is scanned, so a header in a package subdirectory cannot
		// pull in an extension's .md file.
		"cgo-probe-iinc-md": {
			{"A", "cmd/shoal-capi/inc/pwn.h", `include "../../../extensions/e/payload.md" (resolves outside allowed directories)`},
			{"A", "cmd/shoal-capi/probe.go", "cgo flag -Iinc"},
		},
		// Every existing candidate is checked, so a benign decoy on a later
		// search path cannot hide the file the compiler takes first.
		"cgo-probe-decoy": {{"A", "cmd/shoal-capi/probe.go", `include "../tests/x.h" (resolves outside allowed directories)`}},
		// Forms the checker cannot follow are refused outright: inline
		// assembly (.include/.incbin), C++ raw strings, a lone CR.
		"cgo-probe-asm": {
			{"A", "cmd/shoal-capi/probe.c", "(assembler include not allowed)"},
			{"A", "cmd/shoal-capi/probe.c", "(inline assembly not allowed)"},
		},
		"cgo-probe-rawstring": {
			{"A", "cmd/shoal-capi/probe.c", "(raw string literal not allowed)"},
			{"A", "cmd/shoal-capi/probe.c", "(unreadable source: trigraph or unterminated comment)"},
		},
		"cgo-probe-lonecr": {{"A", "cmd/shoal-capi/probe.c", "(lone carriage return)"}},
		// The same forms split by backslash-newline (gcc joins lines before
		// tokenizing), and a splice with whitespace before the newline.
		"cgo-probe-splice-asm": {
			{"A", "cmd/shoal-capi/probe.c", "(assembler include not allowed)"},
			{"A", "cmd/shoal-capi/probe.c", "(inline assembly not allowed)"},
		},
		"cgo-probe-splice-rawstring": {
			{"A", "cmd/shoal-capi/probe.c", "(raw string literal not allowed)"},
			{"A", "cmd/shoal-capi/probe.c", "(unreadable source: trigraph or unterminated comment)"},
		},
		"cgo-probe-splice-space": {{"A", "cmd/shoal-capi/probe.c", `include "../../extensions/e/notes.md" (resolves outside allowed directories)`}},
		"cgo-probe-flagforms": {
			{"A", "cmd/shoal-capi/probe.go", "cgo flag -Wp,-include,${SRCDIR}/x.h"},
			{"A", "cmd/shoal-capi/probe.go", "cgo flag -Xpreprocessor"},
			{"A", "cmd/shoal-capi/probe.go", "cgo flag -iquote${SRCDIR}"},
			{"A", "cmd/shoal-capi/probe.go", "cgo flag @${SRCDIR}/flags.rsp"},
		},
		// -include with a separate argument; an angle include through -I
		// ${SRCDIR} is scanned whatever its extension; no #cgo continuation.
		"cgo-probe-forced": {
			{"A", "cmd/shoal-capi/hidden.inc", `include "../../extensions/e/leak.h" (not found in allowed directories)`},
			{"A", "cmd/shoal-capi/probe.go", "(#cgo line continuation)"},
			{"A", "cmd/shoal-capi/probe.go", "cgo flag ${SRCDIR}/forced.inc"},
			{"A", "cmd/shoal-capi/probe.go", "cgo flag -include"},
		},
		"cgo-probe-flags": {
			{"A", "cmd/shoal-capi/probe.go", "(#cgo LDFLAGS not allowed)"},
			{"A", "cmd/shoal-capi/probe.go", "cgo flag -I/usr/include"},
		},
		"cgo-outside-allowlist": {
			{"A", "pkg/asm/asm_amd64.s", "(non-Go source outside the cgo allowlist)"},
			{"A", "pkg/blob/rsrc.syso", "(non-Go source outside the cgo allowlist)"},
			{"A", "pkg/core/core.go", "(cgo outside the allowlist)"},
			{"A", "pkg/core/shim.c", "(non-Go source outside the cgo allowlist)"},
			{"A", "pkg/swig/lib.swigcxx", "(non-Go source outside the cgo allowlist)"},
		},
		"extension-native-sources": {
			{"B", "extensions/e/cgo.go", "(cgo in an extension)"},
			{"B", "extensions/e/leak.h", "(non-Go source in an extension)"},
			{"B", "extensions/e/testdata/x.s", "(non-Go source in an extension)"},
			{"B", "extensions/e/x.swig", "(non-Go source in an extension)"},
			{"B", "extensions/e/x.syso", "(non-Go source in an extension)"},
		},
		"gomod-tool-directive": {
			{"A", "go.mod", "tool " + m + "/extensions/e"},
			{"B", "extensions/e/go.mod", "tool " + m + "/internal/engine"},
		},
		"gowork-bridge": {
			{"A", "go.work", "replace " + m + "/extensions/e => ./extensions/e"},
			{"A", "go.work", "use ../outside"},
			{"A", "hidden/bridge/bridge.go", m + "/extensions/e"},
		},
	}
	// Every fixture is exercised: by this table or by a named test.
	entries, err := os.ReadDir("testdata")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if _, ok := cases[e.Name()]; !ok && e.Name() != "symlinked-extension-dir" {
			t.Errorf("fixture %s is not exercised", e.Name())
		}
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			sub, err := fs.Sub(os.DirFS("testdata"), name)
			if err != nil {
				t.Fatal(err)
			}
			if got := check(t, sub); !reflect.DeepEqual(got, want) {
				t.Fatalf("got %v, want %v", got, want)
			}
		})
	}
}

// copyTree copies a fixture into dir. Symlinks are not committed, because a
// checkout without symlink support turns them into text files; tests create
// them here instead.
func copyTree(t *testing.T, src, dir string) {
	t.Helper()
	err := fs.WalkDir(os.DirFS(src), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		raw, err := os.ReadFile(filepath.Join(src, filepath.FromSlash(name)))
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSymlinksAreViolations(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, filepath.Join("testdata", "symlinked-extension-dir"), dir)
	for link, target := range map[string]string{
		"extensions/e/impl": "../../internal/secret",
		"pkg/alias":         "../internal/secret",
	} {
		if err := os.Symlink(target, filepath.Join(dir, filepath.FromSlash(link))); err != nil {
			t.Skipf("platform refuses symlinks: %v", err)
		}
	}
	want := []Violation{
		{"A", "pkg/alias", "(symlink)"},
		{"B", "extensions/e/impl", "(symlink)"},
	}
	if got := check(t, os.DirFS(dir)); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// An unparseable Go file is an error from Check, which every caller (these
// tests, and so CI) treats as a failure rather than as a clean result.
func TestUnparseableSourceFailsCheck(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, filepath.Join("testdata", "clean"), dir)
	if err := os.WriteFile(filepath.Join(dir, "pkg", "sdk", "broken.go"), []byte("package sdk\nimport (\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Check(os.DirFS(dir)); err == nil {
		t.Fatal("unparseable source passed")
	}
}
