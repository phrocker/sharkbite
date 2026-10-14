// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package importboundary

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// cgo, assembly, .syso and SWIG files let a package compile or link code
// that no Go import names, and the C preprocessor has too many spellings to
// police by deny-list. The rules are therefore an allowlist.
//
// Extensions (rule B) may not use cgo or SWIG at all, and may not contain any
// source the go command builds besides .go files (extensionFileAllowed).
// Allowing cgo in extensions needs its own design.
//
// Core (rule A) may use cgo only in CgoPackages. Their #cgo directives are an
// exact allowlist (cgoDirective): CFLAGS or CPPFLAGS whose every argument is
// -I${SRCDIR}/<path> into the package directory or CgoIncludeDirs, or a plain
// -D define. Every include the compiler would read, followed from the
// package's preambles and C files in the compiler's search order, must be a
// plain literal resolving inside those directories, or a <system.h> name with
// no path that no repository search directory holds (checkCgoPackage).
// Anything the simple parser cannot read is a violation. Any other core
// package with import "C" or a buildable non-Go source is a violation.
//
// Constructs the checker cannot follow (inline assembly and assembler
// .include/.incbin, raw strings, #embed, __has_include, a lone carriage
// return) are refused outright in that package (refusals).
//
// Threat model: this enforces dependency direction against mistakes and
// casual circumvention in the Go import graph, module wiring and the single
// allowlisted cgo package. It is not a sandbox against a committer
// deliberately smuggling code through C, assembler or toolchain behaviour;
// that residual is bounded by cgo being confined to one allowlisted package
// whose changes require review. Flags and search paths supplied by the build
// environment (CGO_CFLAGS, CGO_LDFLAGS, pkg-config) are outside any source
// check.

// CgoPackages are the only core packages that may use cgo. Today that is the
// C ABI library.
var CgoPackages = []string{"cmd/shoal-capi"}

// CgoIncludeDirs are the only directories, besides a cgo package's own,
// that its #cgo paths and includes may name. These are exactly what
// cmd/shoal-capi uses: the public C headers and the C test seam.
var CgoIncludeDirs = []string{"capi/include", "capi/tests"}

// InertCSourceDirs hold C fixtures read as data by a Go test. The go command
// refuses to build C files in a package without import "C", which the
// cgo rule forbids here, so these files cannot be compiled into core.
// Assembly, .syso and SWIG files stay forbidden even here.
var InertCSourceDirs = []string{"docs/testdata/validate_sharkbite_matrix"}

// buildableExts are the non-Go files the go command compiles or links into
// a package: C, C++, Objective-C, Fortran, headers, assembly, .syso and SWIG.
var buildableExts = []string{".c", ".h", ".cc", ".cpp", ".cxx", ".hh", ".hpp", ".hxx", ".m", ".mm", ".f", ".F", ".for", ".f90", ".s", ".S", ".sx", ".syso", ".swig", ".swigcxx"}

// buildWithoutCgo are buildable files the go command uses even when a
// package does not import "C".
var buildWithoutCgo = []string{".s", ".syso", ".swig", ".swigcxx"}

func hasExt(name string, exts []string) bool {
	return slices.Contains(exts, path.Ext(name))
}

// extensionFileAllowed reports whether an extension module may contain the
// file: Go sources, module files and plain documentation or data. Under a
// testdata directory any non-buildable file is data.
//
// The set is deliberately small, since any extension file is something an
// include elsewhere could try to name: .go, go.mod, go.sum and .md, plus
// .json and .golden under a testdata directory. The example extension needs
// only .go and go.mod.
func extensionFileAllowed(name string) bool {
	base := path.Base(name)
	switch {
	case strings.HasSuffix(base, ".go"), base == "go.mod", base == "go.sum", path.Ext(base) == ".md":
		return true
	case strings.Contains("/"+path.Dir(name)+"/", "/testdata/"):
		return path.Ext(base) == ".json" || path.Ext(base) == ".golden"
	}
	return false
}

// refusedForms are constructs the checker cannot follow and refuses
// outright in the allowlisted cgo package: inline assembly (the assembler
// fetches files itself through .include and .incbin), C++ raw strings
// (which desynchronise comment and string scanning), #embed and
// __has_include. A lone carriage return, which gcc treats as a line end and
// this checker would not, is refused separately.
var refusedForms = []struct {
	re   *regexp.Regexp
	what string
}{
	{regexp.MustCompile(`\b(asm|__asm|__asm__)\b`), "inline assembly"},
	{regexp.MustCompile(`(?i)\.(include|incbin)\b`), "assembler include"},
	{regexp.MustCompile(`(^|[^A-Za-z0-9_])(L|u8|u|U)?R"`), "raw string literal"},
	{regexp.MustCompile(`#\s*embed\b`), "#embed"},
	{regexp.MustCompile(`__has_include(_next)?\b`), "__has_include"},
}

// refusals reports every refused form in raw source text.
// Forms are matched in both the raw and the spliced text, so a
// backslash-newline inside a token (__a\<newline>sm__) cannot hide it.
func refusals(src []byte) []string {
	var out []string
	raw := strings.ReplaceAll(string(src), "\r\n", "\n")
	if strings.Contains(raw, "\r") {
		out = append(out, "(lone carriage return)")
	}
	spliced := splice(src)
	for _, f := range refusedForms {
		if f.re.MatchString(raw) || f.re.MatchString(spliced) {
			out = append(out, "("+f.what+" not allowed)")
		}
	}
	return out
}

// lineSplice is a backslash, optional spaces or tabs, and a newline. gcc
// joins all of these (warning when whitespace precedes the newline).
var lineSplice = regexp.MustCompile(`\\[ \t]*\n`)

// splice normalises CRLF to LF and removes every lineSplice, the line
// joining gcc performs before tokenizing.
func splice(src []byte) string {
	return lineSplice.ReplaceAllString(strings.ReplaceAll(string(src), "\r\n", "\n"), "")
}

// preprocess applies C translation phases 1 to 3: it splices continuations
// (splice: backslash, optional spaces or tabs, newline) and replaces
// comments with a space, so
// "#include \", "#/**/include" and the like read as the directive the
// compiler sees. ok is false for trigraphs, which can splice lines or form
// '#' when a compiler flag enables them.
func preprocess(src []byte) (string, bool) {
	s := strings.ReplaceAll(string(src), "\r\n", "\n")
	if regexp.MustCompile(`\?\?[=/'()!<>-]`).MatchString(s) {
		return "", false
	}
	s = splice([]byte(s))
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '"' || c == '\'':
			// Copy a literal through its closing quote.
			j := i + 1
			for j < len(s) && s[j] != c && s[j] != '\n' {
				if s[j] == '\\' {
					j++
				}
				j++
			}
			if j >= len(s) {
				j = len(s) - 1
			}
			b.WriteString(s[i : j+1])
			i = j
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return "", false
			}
			// Keep newlines so line structure survives.
			b.WriteByte(' ')
			b.WriteString(strings.Repeat("\n", strings.Count(s[i+2:i+2+end], "\n")))
			i += end + 3
		case c == '/' && i+1 < len(s) && s[i+1] == '/':
			for i < len(s) && s[i] != '\n' {
				i++
			}
			if i < len(s) {
				b.WriteByte('\n')
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), true
}

var directiveName = regexp.MustCompile(`^[A-Za-z_]+`)

// include is one inclusion directive: kind is "quote" or "angle".
type include struct {
	kind, path string
}

func (i include) String() string {
	if i.kind == "angle" {
		return "<" + i.path + ">"
	}
	return `"` + i.path + `"`
}

// includes returns the inclusion directives of C source. A directive the
// parser cannot read exactly yields a problem description.
func includes(src []byte) ([]include, []string) {
	text, ok := preprocess(src)
	if !ok {
		return nil, []string{"(unreadable source: trigraph or unterminated comment)"}
	}
	var out []include
	var problems []string
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "%:") {
			problems = append(problems, "(digraph directive "+t+")")
			continue
		}
		if !strings.HasPrefix(t, "#") {
			continue
		}
		rest := strings.TrimSpace(t[1:])
		name := directiveName.FindString(rest)
		if name == "" {
			if rest != "" { // "#" alone is the null directive.
				problems = append(problems, "(unreadable directive "+t+")")
			}
			continue
		}
		switch name {
		case "include", "include_next", "import":
		default:
			continue
		}
		arg := strings.TrimSpace(rest[len(name):])
		switch {
		case len(arg) >= 2 && arg[0] == '"' && strings.Count(arg, `"`) == 2 && arg[len(arg)-1] == '"':
			out = append(out, include{"quote", arg[1 : len(arg)-1]})
		case len(arg) >= 2 && arg[0] == '<' && strings.Count(arg, ">") == 1 && arg[len(arg)-1] == '>':
			out = append(out, include{"angle", arg[1 : len(arg)-1]})
		default:
			problems = append(problems, "include "+arg+" (not a literal path)")
		}
	}
	return out, problems
}

// preamble returns the cgo preamble of a Go file, the comment on import "C".
func preamble(name string, src []byte) (string, bool, error) {
	file, err := parser.ParseFile(token.NewFileSet(), name, src, parser.ImportsOnly|parser.ParseComments)
	if err != nil {
		return "", false, err
	}
	var b strings.Builder
	usesC := false
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.IMPORT {
			continue
		}
		for _, spec := range gen.Specs {
			imp := spec.(*ast.ImportSpec)
			if p, _ := strconv.Unquote(imp.Path.Value); p != "C" {
				continue
			}
			usesC = true
			for _, cg := range []*ast.CommentGroup{imp.Doc, gen.Doc} {
				if cg == nil {
					continue
				}
				for _, c := range cg.List {
					switch t := c.Text; {
					case strings.HasPrefix(t, "//"):
						b.WriteString(t[2:] + "\n")
					case strings.HasPrefix(t, "/*"):
						b.WriteString(strings.TrimSuffix(t[2:], "*/") + "\n")
					}
				}
			}
		}
	}
	return b.String(), usesC, nil
}

// cgoDefine is the only -D form allowed: an identifier, optionally with a
// value free of paths, quotes and spaces.
var cgoDefine = regexp.MustCompile(`^-D[A-Za-z_][A-Za-z0-9_]*(=[A-Za-z0-9_.+-]*)?$`)

// cgoConstraint matches the optional build-constraint words before a verb.
var cgoConstraint = regexp.MustCompile(`^[A-Za-z0-9_,!]+$`)

// cgoDirective checks one #cgo line against the exact allowlist and returns
// the -I directories it adds, in order. Allowed: CFLAGS or CPPFLAGS whose
// every argument is -I${SRCDIR}/<path> resolving into an allowed directory,
// or -D<IDENT>[=<plain value>]. Everything else is refused: other verbs
// (LDFLAGS, pkg-config, noescape), other flags (-include, -iquote, -Wp,
// -Xpreprocessor, @file), separate-argument forms, and -I paths not anchored
// on ${SRCDIR}.
func cgoDirective(pkg, line string, inAllowed func(string) bool) ([]string, []string) {
	var dirs, problems []string
	head, args, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(line, "#cgo")), ":")
	words := strings.Fields(head)
	if !ok || len(words) == 0 {
		return nil, []string{"(#cgo directive not allowed: " + line + ")"}
	}
	verb := words[len(words)-1]
	for _, w := range words[:len(words)-1] {
		if !cgoConstraint.MatchString(w) {
			return nil, []string{"(#cgo directive not allowed: " + line + ")"}
		}
	}
	if verb != "CFLAGS" && verb != "CPPFLAGS" {
		return nil, []string{"(#cgo " + verb + " not allowed)"}
	}
	for _, arg := range strings.Fields(args) {
		if cgoDefine.MatchString(arg) {
			continue
		}
		if rest, ok := strings.CutPrefix(arg, "-I${SRCDIR}"); ok && (rest == "" || strings.HasPrefix(rest, "/")) && !strings.ContainsAny(rest, `$\"'`) {
			p := path.Join(pkg, "."+rest)
			if p != ".." && !strings.HasPrefix(p, "../") && inAllowed(p) {
				dirs = append(dirs, p)
				continue
			}
		}
		problems = append(problems, "cgo flag "+arg)
	}
	return dirs, problems
}

// checkCgoPackage applies the allowlist rules to one cgo package.
//
// Scanning follows what the compiler reads rather than walking directories.
// The go command compiles the package's preambles and the C-family files
// directly in its directory. With #cgo limited to -I and -D, the compiler
// reads nothing else except what those files include, so every included
// repository file is scanned too, transitively and whatever its extension.
// (A directory walk would also judge files that other toolchains build with
// other search paths, such as the C tests in capi/tests.)
//
// A quoted include is searched beside the including file, then in the
// package directory and the -I directories; an angle include only in the
// package directory and the -I directories. Every candidate that exists, not
// just the first, must lie in the allowed directories and is scanned, so the
// checker and the compiler cannot disagree about which one is used. A quoted
// include found nowhere is a violation; an angle include found nowhere is a
// system header and may not contain a path or "..".
func checkCgoPackage(fsys fs.FS, pkg string) ([]Violation, error) {
	var out []Violation
	allowed := append([]string{pkg}, CgoIncludeDirs...)
	inAllowed := func(p string) bool {
		for _, dir := range allowed {
			if within(p, dir) {
				return true
			}
		}
		return false
	}
	regular := func(p string) bool {
		info, err := fs.Stat(fsys, p)
		return err == nil && info.Mode().IsRegular()
	}
	type unit struct {
		file string
		src  []byte
	}
	var queue []unit
	seen := map[string]bool{}
	enqueue := func(file string) error {
		if seen[file] {
			return nil
		}
		seen[file] = true
		src, err := fs.ReadFile(fsys, file)
		if err != nil {
			return err
		}
		queue = append(queue, unit{file, src})
		return nil
	}

	// The package's preambles, in file name order as cgo reads them. Their
	// -I directories form one search list for the whole package.
	searchDirs := []string{pkg}
	entries, err := fs.ReadDir(fsys, pkg)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		name := path.Join(pkg, e.Name())
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		src, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		text, usesC, err := preamble(name, src)
		if err != nil {
			return nil, err
		}
		if !usesC {
			continue
		}
		for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
			t := strings.TrimSpace(line)
			if !strings.HasPrefix(t, "#cgo") {
				continue
			}
			if strings.HasSuffix(t, `\`) {
				out = append(out, Violation{"A", name, "(#cgo line continuation)"})
				continue
			}
			dirs, problems := cgoDirective(pkg, t, inAllowed)
			for _, p := range problems {
				out = append(out, Violation{"A", name, p})
			}
			for _, d := range dirs {
				if !slices.Contains(searchDirs, d) {
					searchDirs = append(searchDirs, d)
				}
			}
		}
		queue = append(queue, unit{name, []byte(text)})
	}

	// C-family files the go command compiles: those directly in the package.
	for _, e := range entries {
		name := path.Join(pkg, e.Name())
		switch {
		case e.IsDir() || strings.HasSuffix(name, ".go"):
		case hasExt(name, buildWithoutCgo):
			out = append(out, Violation{"A", name, "(assembly, .syso or SWIG source)"})
		case hasExt(name, buildableExts):
			if err := enqueue(name); err != nil {
				return nil, err
			}
		}
	}

	for len(queue) > 0 {
		u := queue[0]
		queue = queue[1:]
		for _, r := range refusals(u.src) {
			out = append(out, Violation{"A", u.file, r})
		}
		incs, problems := includes(u.src)
		for _, p := range problems {
			out = append(out, Violation{"A", u.file, p})
		}
		for _, inc := range incs {
			if strings.ContainsAny(inc.path, `\$`) || strings.HasPrefix(inc.path, "/") || inc.path == "" {
				out = append(out, Violation{"A", u.file, "include " + inc.String() + " (not a plain relative path)"})
				continue
			}
			candidates := searchDirs
			if inc.kind == "quote" {
				candidates = append([]string{path.Dir(u.file)}, searchDirs...)
			}
			found, outside := false, false
			for _, dir := range candidates {
				p := path.Join(dir, inc.path)
				if !regular(p) {
					continue
				}
				found = true
				if !inAllowed(p) {
					outside = true
					continue
				}
				if err := enqueue(p); err != nil {
					return nil, err
				}
			}
			switch {
			case outside:
				out = append(out, Violation{"A", u.file, "include " + inc.String() + " (resolves outside allowed directories)"})
			case found:
			case inc.kind == "angle" && !strings.Contains(inc.path, "/") && !strings.Contains(inc.path, ".."):
				// A system header: found in no repository -I directory.
			case inc.kind == "angle":
				out = append(out, Violation{"A", u.file, "include " + inc.String() + " (system header with a path)"})
			default:
				out = append(out, Violation{"A", u.file, "include " + inc.String() + " (not found in allowed directories)"})
			}
		}
	}
	return out, nil
}

// cgoAudit applies the cgo allowlist to every package outside CgoPackages.
// visit is called by the rule A walk for every file it sees.
type cgoAudit struct {
	fsys     fs.FS
	packages map[string]bool // directories holding .go files
	pending  []string        // buildable non-Go files, judged in finish
	out      []Violation
}

func (a *cgoAudit) visit(file string, imports []string) {
	dir := path.Dir(file)
	if slices.Contains(CgoPackages, dir) {
		return // Checked in full by checkCgoPackage.
	}
	if strings.HasSuffix(file, ".go") {
		a.packages[dir] = true
		if slices.Contains(imports, "C") {
			a.out = append(a.out, Violation{"A", file, "(cgo outside the allowlist)"})
		}
		return
	}
	if hasExt(file, buildableExts) {
		a.pending = append(a.pending, file)
	}
}

// finish reports buildable non-Go files in Go package directories. They
// are recorded until the walk has seen every .go file.
func (a *cgoAudit) finish() []Violation {
	sort.Strings(a.pending)
	for _, file := range a.pending {
		dir := path.Dir(file)
		if !a.packages[dir] && !a.hasGo(dir) {
			continue // Not a Go package; the go command never builds it.
		}
		if slices.Contains(InertCSourceDirs, dir) && !hasExt(file, buildWithoutCgo) {
			continue
		}
		a.out = append(a.out, Violation{"A", file, "(non-Go source outside the cgo allowlist)"})
	}
	return a.out
}

func (a *cgoAudit) hasGo(dir string) bool {
	entries, _ := fs.ReadDir(a.fsys, dir)
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			return true
		}
	}
	return false
}
