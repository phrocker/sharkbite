// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestOnlyNonClaimantRoutesUseAuthorizedCurrent enforces the convention
// authorizedCurrent's comment describes, rather than leaving it as advice.
//
// authorizedCurrent hardcodes executorPhaseNone, and every toleration a
// claimant route needs is keyed on the phase — so a route that reports on a
// claim and reaches for this wrapper silently gets the strict behaviour. The
// admission report path did exactly that and was broken by it (#577): a
// descriptor lease that lapsed after the admission was granted refused the
// report and lost the record of an egress that had happened.
//
// There is no error and nothing to notice at review, which is why this is a
// pinned allowlist rather than a comment. A new caller fails here, and
// whoever added it reads the reason before deciding their route is genuinely
// not a claimant route.
func TestOnlyNonClaimantRoutesUseAuthorizedCurrent(t *testing.T) {
	// Each entry is a route that reads or cancels rather than reporting on a
	// claim, so executorPhaseNone is the right phase for it.
	allowed := map[string]string{
		// Invoke was here, exempted because its replay branch only reads.
		// It names executorPhaseTerminalReplay now: refusing that read after
		// a lapse strands the effect at the caller rather than in the record
		// (#578). The stale-entry check below is what forced this removal.
		"Cancel": "a cancellation is the enqueuer's lever under dispatch, not " +
			"a report on a claim",
		"Status": "a read",
	}

	callers := map[string]int{}
	found := 0
	fileSet := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(
			fileSet, filepath.Join(".", name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			// The definition itself is not a call site.
			if function.Name.Name == "authorizedCurrent" {
				found++
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "authorizedCurrent" {
					return true
				}
				callers[function.Name.Name]++
				return true
			})
		}
	}

	// Vacuity guards. If the function were renamed or the walk stopped
	// matching, an empty result would read as "the convention holds".
	if found == 0 {
		t.Fatal("authorizedCurrent was not found in this package; this test " +
			"no longer pins anything and should be updated or deleted")
	}
	if len(callers) == 0 {
		t.Fatal("no authorizedCurrent call sites were found, so the walk is " +
			"not reading the calls it means to check")
	}

	var unexpected []string
	for caller := range callers {
		if _, ok := allowed[caller]; !ok {
			unexpected = append(unexpected, caller)
		}
	}
	sort.Strings(unexpected)
	if len(unexpected) > 0 {
		t.Fatalf("these call authorizedCurrent and are not in the allowlist: "+
			"%s\n\nauthorizedCurrent hardcodes executorPhaseNone, so a route "+
			"that reports on a claim already taken gets the strict behaviour "+
			"silently — a lapsed lease or a rebound reference refuses the "+
			"report and the record of an effect that happened is lost "+
			"(#577). If the route reports on a claim, call "+
			"authorizedCurrentBinding with its phase. If it genuinely only "+
			"reads or cancels, add it here with the reason.",
			strings.Join(unexpected, ", "))
	}

	// And the allowlist must not rot: an entry naming a route that no longer
	// calls it is a stale exemption that would hide a real one later.
	var stale []string
	for caller := range allowed {
		if callers[caller] == 0 {
			stale = append(stale, caller)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Fatalf("the allowlist exempts routes that no longer call "+
			"authorizedCurrent: %s — remove them, or the next route to take "+
			"one of those names is exempted by accident",
			strings.Join(stale, ", "))
	}
}
