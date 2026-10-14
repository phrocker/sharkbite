// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package router

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestRouterIsPure pins the package's direct imports: no clock, file,
// network, process, randomness or model runtime.
func TestRouterIsPure(t *testing.T) {
	allowed := map[string]bool{
		"bytes": true, "crypto/sha256": true, "encoding/hex": true, "encoding/json": true,
		"errors": true, "fmt": true, "io": true, "math": true, "sort": true, "strconv": true,
		"strings": true, "unicode/utf8": true,
		"github.com/phrocker/shoal-oss/pkg/decision": true,
		"github.com/phrocker/shoal-oss/pkg/lexicon":  true,
		"github.com/phrocker/shoal-oss/pkg/shoal":    true,
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if !allowed[path] {
				t.Errorf("%s imports %s", name, path)
			}
		}
	}
}
