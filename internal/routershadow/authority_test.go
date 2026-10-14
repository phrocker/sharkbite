// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package routershadow

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The authority property is structural. The guarded packages (everything go
// list finds under pkg/router and internal/routershadow) cannot hold a service,
// because:
//
//  1. they import only the data packages in allowedDirect (deny by default);
//  2. no package they depend on, transitively, holds a service (serviceful),
//     except pkg/explorer/auth and its own dependencies, reached through
//     pkg/decision for one constant (see authExemption);
//  3. the exported API of every package they import mentions no type outside
//     apiAllowed, so no function they can call returns a service, an
//     authority or a client; and
//  4. the only other values they hold arrive through their own ports, which
//     internal/routerwire implements with wrappers whose method sets a test
//     there pins exactly.
//
// A value they cannot name or obtain cannot be asserted, instantiated or
// called, whatever form the code takes. The checks below are therefore about
// imports and API surfaces, not call sites.
//
// The standard library is allowlisted too (stdAllowed): a guarded package may
// import only the standard packages listed there, so process, environment,
// file, network, cgo and reflection access (os, os/*, net, net/*,
// crypto/tls, runtime/debug, syscall, plugin, reflect, unsafe, "C", ...) is
// refused by default. go:linkname is refused as well.
//
// This is a source-level guard against our own code under review, checked on
// the non-test files of the guarded packages. It is not a runtime sandbox:
// it does not constrain test files, the packages the guarded ones depend on,
// or anything at run time.

const module = "github.com/phrocker/shoal-oss/"

// allowedDirect are the module packages a guarded package may import.
var allowedDirect = map[string]bool{
	module + "pkg/decision": true, module + "pkg/document": true, module + "pkg/graph": true,
	module + "pkg/inference": true, module + "pkg/lexicon": true, module + "pkg/ontology": true,
	module + "pkg/shoal": true,
}

// serviceful packages hold services, stores, clients or authority. None may be
// a dependency of a guarded package.
var serviceful = []string{
	module + "pkg/explorer",
	module + "pkg/explorer/authorized",
	module + "pkg/explorer/fleet",
	module + "pkg/explorer/coordination",
	module + "pkg/explorer/webapi",
	module + "internal/decisionservice",
	module + "internal/decisionregistration",
	module + "internal/decisionlinear",
	module + "internal/routerwire",
	module + "internal/explorerfleet",
}

// authExemption: pkg/decision imports pkg/explorer/auth for one constant
// (auth.MaxOnBehalfOfEntries), so auth and its dependencies, the Accumulo
// client among them, are linked into the guarded packages. They are not
// reachable: a guarded package may not import them (rule 1), and no API it
// can call mentions their types (rule 3), so it can never hold an auth
// Decision, Authority or Binder or a client. The test requires that auth is
// reached only through pkg/decision.
const authExemption = module + "pkg/explorer/auth"

// stdAllowed is every standard-library package a guarded package may import,
// each listed exactly (no prefix wildcards): what the guarded packages import
// today. None performs process, environment, file or network access by
// itself. io/fs is the interface package only; the evaluation harness reads
// fixtures from an fs.FS its caller supplies (os.DirFS in tests), so no
// guarded package imports os. Anything else is refused.
var stdAllowed = map[string]bool{
	"bufio": true, "bytes": true, "context": true,
	"crypto/hmac": true, "crypto/sha256": true,
	"encoding/binary": true, "encoding/hex": true, "encoding/json": true,
	"errors": true, "fmt": true, "io": true, "io/fs": true,
	"math": true, "math/big": true, "path": true,
	"sort": true, "strconv": true, "strings": true, "sync": true, "time": true,
	"unicode/utf8": true,
}

type finding struct{ pos, what string }

func goList(t *testing.T, root string, args ...string) []string {
	t.Helper()
	cmd := exec.Command("go", append([]string{"list"}, args...)...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list %v: %v", args, err)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(strings.TrimSpace(string(out)))
}

type listedPackage struct{ path, dir string }

// guardedPackages is the guarded set, from go list, so a new subpackage is
// guarded the moment it exists.
func guardedPackages(t *testing.T, root string) []listedPackage {
	t.Helper()
	var pkgs []listedPackage
	for _, line := range goList(t, root, "-f", "{{.ImportPath}}\t{{.Dir}}", "./pkg/router/...", "./internal/routershadow/...") {
		if path, dir, ok := strings.Cut(line, "\t"); ok {
			pkgs = append(pkgs, listedPackage{path, dir})
		}
	}
	return pkgs
}

func parseDir(fset *token.FileSet, dir string) ([]*ast.File, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no Go files in %s", dir)
	}
	return files, nil
}

// importFindings applies rule 1 and the standard-library allowlist to one
// package's files, and refuses go:linkname.
func importFindings(fset *token.FileSet, files []*ast.File, guarded func(string) bool) []finding {
	var out []finding
	for _, f := range files {
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			pos := fset.Position(imp.Pos()).String()
			first := strings.SplitN(p, "/", 2)[0]
			switch {
			case strings.HasPrefix(p, module):
				if !guarded(p) && !allowedDirect[p] {
					out = append(out, finding{pos, "import of " + p})
				}
			case strings.Contains(first, "."):
				out = append(out, finding{pos, "import of external package " + p})
			case !stdAllowed[p]:
				out = append(out, finding{pos, "import of " + p})
			}
		}
		for _, group := range f.Comments {
			for _, c := range group.List {
				if strings.Contains(c.Text, "go:linkname") {
					out = append(out, finding{fset.Position(c.Pos()).String(), "go:linkname directive"})
				}
			}
		}
	}
	return out
}

func isGuardedPath(p string) bool {
	return p == module+"pkg/router" || strings.HasPrefix(p, module+"pkg/router/") ||
		p == module+"internal/routershadow" || strings.HasPrefix(p, module+"internal/routershadow/")
}

func checkImports(t *testing.T, pkgs []listedPackage) []finding {
	t.Helper()
	fset := token.NewFileSet()
	var all []finding
	for _, p := range pkgs {
		files, err := parseDir(fset, p.dir)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, importFindings(fset, files, isGuardedPath)...)
	}
	return all
}

// TestGuardedPackagesImportNoService applies rules 1 and 2.
func TestGuardedPackagesImportNoService(t *testing.T) {
	root := moduleRoot(t)
	pkgs := guardedPackages(t, root)
	if len(pkgs) < 3 {
		t.Fatalf("guarded packages = %v", pkgs)
	}
	for _, f := range checkImports(t, pkgs) {
		t.Errorf("%s: %s", f.pos, f.what)
	}
	var paths []string
	for _, p := range pkgs {
		paths = append(paths, p.path)
	}
	deps := map[string]bool{}
	for _, d := range goList(t, root, append([]string{"-deps"}, paths...)...) {
		deps[d] = true
	}
	for _, s := range serviceful {
		if deps[s] {
			t.Errorf("a guarded package depends on %s", s)
		}
	}
	for d := range deps {
		if strings.HasPrefix(d, module+"internal/explorer") || strings.HasPrefix(d, module+"pkg/explorer/") && d != authExemption {
			t.Errorf("a guarded package depends on %s", d)
		}
	}
	// auth is reached only through pkg/decision.
	var others []string
	for p := range allowedDirect {
		if p != module+"pkg/decision" {
			others = append(others, p)
		}
	}
	for _, d := range goList(t, root, append([]string{"-deps"}, others...)...) {
		if d == authExemption {
			t.Errorf("%s is reached other than through pkg/decision", authExemption)
		}
	}
	for _, d := range goList(t, root, "-f", "{{join .Imports \"\\n\"}}", "./pkg/decision") {
		if d == authExemption {
			return
		}
	}
	t.Log("pkg/decision no longer imports auth; the exemption can be removed")
}

// apiAllowed are the module packages whose types may appear anywhere in the
// exported API of a package a guarded package imports.
var apiAllowed = map[string]bool{
	module + "pkg/decision": true, module + "pkg/document": true, module + "pkg/graph": true,
	module + "pkg/inference": true, module + "pkg/lexicon": true, module + "pkg/ontology": true,
	module + "pkg/shoal": true, module + "pkg/interaction": true,
}

// TestImportedAPIsExposeNoService applies rule 3: walking every exported
// function, method, field, variable and constant of each allowed import,
// through every type they mention, finds no type from a package outside
// apiAllowed or stdAllowed, and no unsafe.Pointer.
func TestImportedAPIsExposeNoService(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	var direct []string
	for p := range allowedDirect {
		direct = append(direct, p)
	}
	sort.Strings(direct)
	imp := exportImporter(t, root, fset, nil, direct...)
	for typ, via := range apiFindings(t, imp, direct) {
		t.Errorf("an allowed import's API exposes %s (via %s)", typ, via)
	}
}

// TestAPIWalkerFlagsExposure is the walker's positive control: a fixture
// package whose API returns an *os.File, an unsafe.Pointer and a fleet
// service is flagged for each.
func TestAPIWalkerFlagsExposure(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	dir := filepath.Join(root, "internal", "routershadow", "testdata", "round3", "api")
	imp := exportImporter(t, root, fset, map[string]string{"example.test/round3/api": dir}, "./pkg/explorer/fleet", "os")
	bad := apiFindings(t, imp, []string{"example.test/round3/api"})
	for _, want := range []string{"os.File", "unsafe.Pointer", module + "pkg/explorer/fleet.Service"} {
		if _, ok := bad[want]; !ok {
			t.Errorf("walker did not flag %s (flagged %v)", want, bad)
		}
	}
}

// apiFindings walks the exported API of each package and returns every type
// outside apiAllowed and stdAllowed it reaches, with the path it was reached by.
func apiFindings(t *testing.T, imp types.Importer, paths []string) map[string]string {
	t.Helper()
	seen := map[types.Type]bool{}
	bad := map[string]string{}
	var walk func(types.Type, string)
	walk = func(typ types.Type, via string) {
		if typ == nil || seen[typ] {
			return
		}
		seen[typ] = true
		switch t := typ.(type) {
		case *types.Basic:
			if t.Kind() == types.UnsafePointer {
				bad["unsafe.Pointer"] = via
			}
		case *types.Named:
			if p := t.Obj().Pkg(); p != nil {
				path := p.Path()
				switch {
				case strings.HasPrefix(path, module) && !apiAllowed[path]:
					bad[path+"."+t.Obj().Name()] = via
				case !strings.HasPrefix(path, module) && !stdAllowed[path]:
					bad[path+"."+t.Obj().Name()] = via
				}
			}
			for i := 0; i < t.NumMethods(); i++ {
				if m := t.Method(i); m.Exported() {
					walk(m.Type(), via+"."+m.Name())
				}
			}
			for i := 0; i < t.TypeArgs().Len(); i++ {
				walk(t.TypeArgs().At(i), via)
			}
			walk(t.Underlying(), via)
		case *types.Pointer:
			walk(t.Elem(), via)
		case *types.Slice:
			walk(t.Elem(), via)
		case *types.Array:
			walk(t.Elem(), via)
		case *types.Map:
			walk(t.Key(), via)
			walk(t.Elem(), via)
		case *types.Chan:
			walk(t.Elem(), via)
		case *types.Signature:
			for _, tuple := range []*types.Tuple{t.Params(), t.Results()} {
				for i := 0; i < tuple.Len(); i++ {
					walk(tuple.At(i).Type(), via)
				}
			}
		case *types.Struct:
			for i := 0; i < t.NumFields(); i++ {
				if f := t.Field(i); f.Exported() || f.Embedded() {
					walk(f.Type(), via+"."+f.Name())
				}
			}
		case *types.Interface:
			for i := 0; i < t.NumMethods(); i++ {
				walk(t.Method(i).Type(), via+"."+t.Method(i).Name())
			}
		}
	}
	for _, path := range paths {
		pkg, err := imp.Import(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range pkg.Scope().Names() {
			obj := pkg.Scope().Lookup(name)
			if obj.Exported() {
				walk(obj.Type(), path+"."+name)
			}
		}
	}
	return bad
}

// exportImporter resolves imports from compiled export data located with go
// list. sources maps import paths to directories type-checked from source
// instead (fixtures).
func exportImporter(t *testing.T, root string, fset *token.FileSet, sources map[string]string, patterns ...string) types.Importer {
	t.Helper()
	exports := map[string]string{}
	for _, line := range goList(t, root, append([]string{"-export", "-deps", "-f", "{{.ImportPath}}\t{{.Export}}"}, patterns...)...) {
		if path, file, ok := strings.Cut(line, "\t"); ok && file != "" {
			exports[path] = file
		}
	}
	gc := importer.ForCompiler(fset, "gc", func(path string) (io.ReadCloser, error) {
		file, ok := exports[path]
		if !ok {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(file)
	})
	return &sourceImporter{fset: fset, sources: sources, fallback: gc, done: map[string]*types.Package{}}
}

type sourceImporter struct {
	fset     *token.FileSet
	sources  map[string]string
	fallback types.Importer
	done     map[string]*types.Package
}

func (s *sourceImporter) Import(path string) (*types.Package, error) {
	if path == "unsafe" {
		return types.Unsafe, nil
	}
	if p, ok := s.done[path]; ok {
		return p, nil
	}
	dir, ok := s.sources[path]
	if !ok {
		return s.fallback.Import(path)
	}
	files, err := parseDir(s.fset, dir)
	if err != nil {
		return nil, err
	}
	p, err := (&types.Config{Importer: s}).Check(path, s.fset, files, nil)
	if err != nil {
		return nil, err
	}
	s.done[path] = p
	return p, nil
}

// fixture is one violating package, checked as if it were guarded.
type fixture struct {
	dir string
	// want are the findings that must appear; compileError, when set, is a
	// substring of the type-check error that must occur.
	want         []string
	compileError string
	// parseOnly fixtures are not type-checked (cgo may be unavailable).
	parseOnly bool
	// src, when set, is the fixture's only file, held here rather than in
	// testdata: a committed file importing "C" would itself break the
	// repository's cgo allowlist (internal/importboundary).
	src string
}

// cgoProbe is the cgo half of the round-3 probe; see fixture.src.
const cgoProbe = `package cgo

// #include <stdlib.h>
import "C"

func Probe() { C.system(C.CString("true")) }
`

// TestViolationsAreImpossible proves the rules can fail: each fixture is
// either refused by the import rules or does not compile against the
// router's types.
func TestViolationsAreImpossible(t *testing.T) {
	root := moduleRoot(t)
	fleetPkg := module + "pkg/explorer/fleet"
	fixtures := []fixture{
		// Round 1: Enqueue, Invoke, Claim, ExecuteClaim, Cancel, Register,
		// Heartbeat, Revoke, a stored func variable, a local interface, a
		// variable bound elsewhere, reflection and linkname. All need fleet.
		{dir: "violating", want: []string{"import of " + fleetPkg, "import of reflect", "import of unsafe",
			"import of os/exec", "import of external package example.test/elsewhere", "go:linkname directive"}},
		// A subpackage of a fixture: CompleteClaim.
		{dir: "violating/sub", want: []string{"import of " + fleetPkg}},
		// Round 2: an assertion on the authorized client the Config held.
		{dir: "round2/connect", compileError: "config.Client undefined"},
		// Round 2: generics instantiated with the registry and the dispatcher.
		{dir: "round2/register", want: []string{"import of " + fleetPkg}},
		{dir: "round2/enqueue", want: []string{"import of " + fleetPkg}},
		// Round 3: process, environment, file and network access through
		// standard packages a denylist did not name.
		{dir: "round3/probe", want: []string{"import of os", "import of crypto/tls", "import of net/smtp",
			"import of net/http/httputil", "import of runtime/debug"}},
		// Round 3: cgo, parsed only.
		{dir: "round3/cgo", want: []string{"import of C"}, parseOnly: true, src: cgoProbe},
	}
	fset := token.NewFileSet()
	base := filepath.Join(root, "internal", "routershadow", "testdata")
	sources := map[string]string{"example.test/elsewhere": filepath.Join(base, "elsewhere")}
	imp := exportImporter(t, root, fset, sources, "./pkg/explorer/fleet", "./internal/routershadow", "./pkg/graph", "context", "reflect", "os/exec", "os", "crypto/tls", "net/smtp", "net/http/httputil", "runtime/debug")
	for _, fx := range fixtures {
		var files []*ast.File
		var err error
		if fx.src != "" {
			var f *ast.File
			f, err = parser.ParseFile(fset, fx.dir+"/probe.go", fx.src, parser.ParseComments)
			files = []*ast.File{f}
		} else {
			files, err = parseDir(fset, filepath.Join(base, fx.dir))
		}
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, f := range importFindings(fset, files, isGuardedPath) {
			got[f.what] = true
		}
		for _, w := range fx.want {
			if !got[w] {
				t.Errorf("%s: %q not flagged (findings %v)", fx.dir, w, got)
			}
		}
		if fx.parseOnly {
			continue
		}
		_, err = (&types.Config{Importer: imp}).Check("example.test/"+fx.dir, fset, files, nil)
		switch {
		case fx.compileError != "" && (err == nil || !strings.Contains(err.Error(), fx.compileError)):
			t.Errorf("%s: compiled against the router's types (err %v)", fx.dir, err)
		case fx.compileError == "" && err != nil:
			// The fixture must otherwise compile, so the import rule is
			// what refuses it.
			t.Errorf("%s: does not compile: %v", fx.dir, err)
		}
	}
}

// TestNewSubpackageIsGuarded: a package added under pkg/router is in the
// guarded set without editing this test, and its import of fleet is refused.
func TestNewSubpackageIsGuarded(t *testing.T) {
	root := moduleRoot(t)
	name := fmt.Sprintf("guardprobe%d", os.Getpid())
	dir := filepath.Join(root, "pkg", "router", name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	src := "package " + name + "\n\nimport _ \"github.com/phrocker/shoal-oss/pkg/explorer/fleet\"\n"
	if err := os.WriteFile(filepath.Join(dir, "probe.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	pkgs := guardedPackages(t, root)
	found := false
	for _, p := range pkgs {
		found = found || p.path == module+"pkg/router/"+name
	}
	if !found {
		t.Fatalf("new subpackage not guarded: %v", pkgs)
	}
	flagged := false
	for _, f := range checkImports(t, pkgs) {
		flagged = flagged || (strings.Contains(f.pos, name) && f.what == "import of "+module+"pkg/explorer/fleet")
	}
	if !flagged {
		t.Fatal("the new subpackage's fleet import was not flagged")
	}
}
