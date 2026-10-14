// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package importboundary

import (
	"slices"
	"testing"
	"testing/fstest"
)

func TestParseGoWorkFailsClosed(t *testing.T) {
	parse := func(body string) (goMod, error) {
		return parseGoWork(fstest.MapFS{"go.work": {Data: []byte(body)}}, "go.work")
	}
	good, err := parse("go 1.26.4\n\nuse(\n\t.\n\t./wal\n)\nuse ./x\nreplace a => ./b\n")
	if err != nil || !slices.Equal(good.uses, []string{".", "./wal", "./x"}) || len(good.replaces) != 1 {
		t.Fatalf("valid go.work: %+v %v", good, err)
	}
	for name, body := range map[string]string{
		"module directive": "go 1.26.4\nmodule x\n",
		"require":          "go 1.26.4\nrequire a v1\n",
		"single-line use":  "go 1.26.4\nuse(./x)\n",
		"unterminated":     "go 1.26.4\nuse (\n./x\n",
		"use two dirs":     "go 1.26.4\nuse ./a ./b\n",
	} {
		if _, err := parse(body); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

// The real repository's nested modules and go.work are actually examined, so
// rule A's nested-module and workspace checks are not vacuous.
func TestRepositoryWorkspaceIsExamined(t *testing.T) {
	root := repoFS(t)
	nested, err := NestedModules(root)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(nested, "wal-quorum-sidecar") {
		t.Errorf("nested modules %v omit wal-quorum-sidecar", nested)
	}
	for _, dir := range nested {
		if dir == FixtureRoot || len(dir) > len(FixtureRoot) && dir[:len(FixtureRoot)+1] == FixtureRoot+"/" {
			t.Errorf("fixture %s treated as a nested module", dir)
		}
	}
	work, err := parseGoWork(root, "go.work")
	if err != nil || !slices.Contains(work.uses, "./extensions/example-collector") ||
		!slices.Contains(work.uses, "./extensions/effects-ssh") {
		t.Fatalf("go.work: %+v %v", work, err)
	}
}
