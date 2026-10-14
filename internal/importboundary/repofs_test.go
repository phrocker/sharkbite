// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package importboundary

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// trackedFS exposes only the files git tracks in a checkout. The real-tree
// tests check what the repository contains, not whatever a build or a
// developer left in the working tree (CI unpacks a Thrift source tarball,
// editors create nested worktrees).
type trackedFS struct {
	root    string
	files   map[string]bool          // tracked paths present on disk
	entries map[string][]fs.DirEntry // directory -> tracked children
	infos   map[string]fs.FileInfo   // Lstat of every exposed path
}

// repoFS returns the repository two levels up as a trackedFS. It fails the
// test when git is unavailable rather than silently walking everything.
func repoFS(t *testing.T) fs.FS {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	return trackedFSAt(t, root)
}

func trackedFSAt(t *testing.T, root string) fs.FS {
	t.Helper()
	raw, err := exec.Command("git", "-C", root, "ls-files", "-z").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v (the real-tree boundary tests need a git checkout)", err)
	}
	tfs := &trackedFS{root: root, files: map[string]bool{}, entries: map[string][]fs.DirEntry{}, infos: map[string]fs.FileInfo{}}
	info, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	tfs.infos["."] = info
	for _, name := range bytes.Split(raw, []byte{0}) {
		if len(name) == 0 {
			continue
		}
		p := string(name)
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil {
			continue // Tracked but deleted in the working tree.
		}
		tfs.add(p, info)
		for dir := path.Dir(p); ; dir = path.Dir(dir) {
			if _, ok := tfs.infos[dir]; ok {
				break
			}
			di, err := os.Lstat(filepath.Join(root, filepath.FromSlash(dir)))
			if err != nil {
				t.Fatal(err)
			}
			tfs.add(dir, di)
		}
	}
	for _, list := range tfs.entries {
		sort.Slice(list, func(i, j int) bool { return list[i].Name() < list[j].Name() })
	}
	if !tfs.files["go.mod"] {
		t.Fatal("git ls-files listed no root go.mod")
	}
	return tfs
}

func (t *trackedFS) add(p string, info fs.FileInfo) {
	t.infos[p] = info
	if !info.IsDir() {
		t.files[p] = true
	}
	parent := path.Dir(p)
	t.entries[parent] = append(t.entries[parent], fs.FileInfoToDirEntry(info))
}

func (t *trackedFS) check(op, name string) (fs.FileInfo, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: op, Path: name, Err: fs.ErrInvalid}
	}
	info, ok := t.infos[name]
	if !ok {
		return nil, &fs.PathError{Op: op, Path: name, Err: fs.ErrNotExist}
	}
	return info, nil
}

func (t *trackedFS) Open(name string) (fs.File, error) {
	info, err := t.check("open", name)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return &trackedDir{info: info, entries: t.entries[name]}, nil
	}
	return os.Open(filepath.Join(t.root, filepath.FromSlash(name)))
}

func (t *trackedFS) ReadDir(name string) ([]fs.DirEntry, error) {
	info, err := t.check("readdir", name)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrInvalid}
	}
	return append([]fs.DirEntry(nil), t.entries[name]...), nil
}

func (t *trackedFS) Stat(name string) (fs.FileInfo, error) {
	if _, err := t.check("stat", name); err != nil {
		return nil, err
	}
	return os.Stat(filepath.Join(t.root, filepath.FromSlash(name)))
}

func (t *trackedFS) Lstat(name string) (fs.FileInfo, error) { return t.check("lstat", name) }

func (t *trackedFS) ReadLink(name string) (string, error) {
	if _, err := t.check("readlink", name); err != nil {
		return "", err
	}
	return os.Readlink(filepath.Join(t.root, filepath.FromSlash(name)))
}

var _ fs.ReadLinkFS = (*trackedFS)(nil)

type trackedDir struct {
	info    fs.FileInfo
	entries []fs.DirEntry
	offset  int
}

func (d *trackedDir) Stat() (fs.FileInfo, error) { return d.info, nil }
func (d *trackedDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.info.Name(), Err: fs.ErrInvalid}
}
func (d *trackedDir) Close() error { return nil }
func (d *trackedDir) ReadDir(n int) ([]fs.DirEntry, error) {
	rest := d.entries[d.offset:]
	if n > 0 && len(rest) > n {
		rest = rest[:n]
	}
	d.offset += len(rest)
	if n > 0 && len(rest) == 0 {
		return nil, io.EOF
	}
	return rest, nil
}

// Untracked files never reach the checker: an untracked nested module with a
// bad replace and an extension import (as an unpacked build dependency would
// be) is invisible, while the same files on a plain directory FS, as in a
// committed fixture, are violations.
func TestRepoFSExposesOnlyTrackedFiles(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, filepath.Join("testdata", "clean"), dir)
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q")
	run("add", "-A")
	stray := filepath.Join(dir, "thrift-0.17.0", "lib", "go", "test", "fuzz")
	if err := os.MkdirAll(filepath.Join(stray, "bridge"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stray, "go.mod"), []byte("module fuzz\n\nreplace shared => ./gen-go/shared\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stray, "bridge", "b.go"), []byte("package bridge\n\nimport _ \""+Module+"/extensions/good\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tracked := trackedFSAt(t, dir)
	if _, err := fs.Stat(tracked, "thrift-0.17.0"); err == nil {
		t.Fatal("untracked directory exposed")
	}
	if got := check(t, tracked); len(got) != 0 {
		t.Fatalf("tracked tree: %v", got)
	}
	want := []Violation{
		{"A", "thrift-0.17.0/lib/go/test/fuzz/bridge/b.go", Module + "/extensions/good"},
		{"A", "thrift-0.17.0/lib/go/test/fuzz/go.mod", "replace shared => ./gen-go/shared"},
	}
	if got := check(t, os.DirFS(dir)); !reflect.DeepEqual(got, want) {
		t.Fatalf("plain FS: got %v, want %v", got, want)
	}
	// Once added to the index, the same module is checked through the
	// tracked FS too.
	run("add", "-A")
	if got := check(t, trackedFSAt(t, dir)); !reflect.DeepEqual(got, want) {
		t.Fatalf("tracked after add: got %v, want %v", got, want)
	}
}
