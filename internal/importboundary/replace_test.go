// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package importboundary

import (
	"testing"
	"testing/fstest"
)

// Every local replace target spelling must resolve to the root or a walked
// nested module; module-path targets are not local and are left alone.
func TestLocalReplaceTargets(t *testing.T) {
	nested := []string{"wal", "a/b"}
	for target, ok := range map[string]bool{
		".":                        true,
		"./wal":                    true,
		"./wal/":                   true,
		"./a/./b":                  true,
		"./x/../wal":               true,
		"./extensions/e":           false,
		"./" + FixtureRoot + "/fx": false,
		"../elsewhere":             false,
		"./unwalked":               false,
		"/abs/path":                false,
		`.\wal`:                    false,
		`C:\evil`:                  false,
		"example.com/m v1.0.0":     true, // a module, not a directory
	} {
		fsys := fstest.MapFS{"go.mod": {Data: []byte("module " + Module + "\nreplace example.com/x => " + target + "\n")}}
		got := checkModuleFile(fsys, ".", nested)
		if (len(got) == 0) != ok {
			t.Errorf("%q: violations %v, want ok=%v", target, got, ok)
		}
	}
	// Relative to a nested module, ../ reaches the root and siblings.
	fsys := fstest.MapFS{"a/b/go.mod": {Data: []byte("module m\nreplace x => ../../wal\nreplace y => ../../../out\n")}}
	if got := checkModuleFile(fsys, "a/b", nested); len(got) != 1 || got[0].Import != "replace y => ../../../out" {
		t.Errorf("nested resolution: %v", got)
	}
}
