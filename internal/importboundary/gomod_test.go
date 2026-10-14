// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package importboundary

import (
	"testing"
	"testing/fstest"
)

func TestParseGoModFailsClosed(t *testing.T) {
	const m = Module
	parse := func(body string) (goMod, error) {
		return parseGoMod(fstest.MapFS{"go.mod": {Data: []byte(body)}}, "go.mod")
	}
	good, err := parse("module \"" + m + "/extensions/e\"\n\nrequire (\n\t" + m + " v0.0.0 // indirect\n)\nreplace (\n\t" + m + " v0.0.0 => ../..\n)\nreplace(\n" + m + " => ./x\n)\n")
	if err != nil || good.module != m+"/extensions/e" || len(good.replaces) != 2 || good.replaces[0] != [2]string{m, "../.."} || good.replaces[1] != [2]string{m, "./x"} {
		t.Fatalf("valid go.mod: %+v %v", good, err)
	}
	for name, body := range map[string]string{
		"single-line paren":   "module x\nreplace(" + m + " => ../evil)\n",
		"unterminated block":  "module x\nreplace (\n" + m + " => ../evil\n",
		"stray close":         "module x\n)\n",
		"nested block":        "module x\nreplace (\nrequire (\n)\n)\n",
		"unknown directive":   "module x\nsubstitute a => b\n",
		"unknown block":       "module x\nsubstitute (\na => b\n)\n",
		"partial quote":       "module x\nreplace \"" + m + " => ../evil\n",
		"two modules":         "module x\nmodule y\n",
		"no module":           "go 1.25.0\n",
		"replace without new": "module x\nreplace " + m + " =>\n",
	} {
		if _, err := parse(body); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
