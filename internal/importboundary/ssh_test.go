// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package importboundary

import (
	"io/fs"
	"strings"
	"testing"
)

// SSHModules are the only directories whose Go files may import
// golang.org/x/crypto/ssh. docs/gateways.md puts every protocol adapter
// outside core; CI's "Core links no SSH" step checks the root package set's
// dependency graph, and this checks every tracked file, nested modules and
// other extensions included, so a second SSH dependency is a decision made
// here rather than discovered. The session gateway (#447) adds its module, or
// the shared client's, to this list.
var SSHModules = []string{"extensions/effects-ssh"}

func TestSSHIsImportedOnlyByItsModules(t *testing.T) {
	root := repoFS(t)
	found := 0
	err := fs.WalkDir(root, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (name == ".git" || strings.HasPrefix(name, FixtureRoot)) {
			return fs.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(name, ".go") {
			return nil
		}
		list, err := imports(root, name)
		if err != nil {
			return err
		}
		for _, p := range list {
			if !within(p, "golang.org/x/crypto/ssh") {
				continue
			}
			allowed := false
			for _, dir := range SSHModules {
				allowed = allowed || strings.HasPrefix(name, dir+"/")
			}
			if !allowed {
				t.Errorf("%s imports %s outside %v", name, p, SSHModules)
			}
			found++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == 0 {
		t.Error("no file imports golang.org/x/crypto/ssh; the check found nothing to confine")
	}
}
