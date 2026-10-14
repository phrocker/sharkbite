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
	"strconv"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
)

// authPackageDir holds the grant-policy namespaces. auth.LabelPolicyNamespace
// documents why they live there rather than in authorized: both the decision
// constructor and the ingest selectors must refuse the reserved part, so the
// namespace is declared beside Decision.
const authPackageDir = "../auth"

// TestEveryGrantPolicyNamespaceIsClassifiedForConfinement makes adding a
// grant-policy namespace a decision about refuseUnconfinedRetrieval rather
// than an omission.
//
// withoutOwnLabelPolicies is a filter with a default: it drops canonical
// label policies for the action's own source and compares everything else.
// Comparing is the safe default against widening, and the wrong default
// against narrowing. A namespace whose grants narrow within a source, as
// labels do (#570), is a strict subset of the action's own scope and must be
// dropped — counting one as a wider policy refused every holder of it an
// unconfined executor, which is #564: the hosted ask path was unusable for
// anyone holding a label, and the refusal names no cause because it must not
// (#398), so there is nothing to read in a log and nothing to notice at
// review.
//
// The next narrowing namespace reproduces that silently. This fails instead,
// and whoever adds the namespace classifies it here.
//
// Scope and its limits, stated because a passing test is not coverage: this
// reads the string literals declared in pkg/explorer/auth, which is where the
// convention puts them. A namespace declared elsewhere, or assembled at
// runtime rather than written as a literal, is not seen. A literal that ends
// in "/" and is not a namespace is classified here with that as its reason,
// which costs one line.
func TestEveryGrantPolicyNamespaceIsClassifiedForConfinement(t *testing.T) {
	// Each entry classifies one namespace against the policy comparison in
	// refuseUnconfinedRetrieval, and every entry is checked against the
	// filter below rather than taken on its word: a prose-only table is
	// satisfied by writing "narrows" next to a namespace the filter still
	// compares, which is the omission this test exists to catch.
	classified := map[string]namespaceClass{
		"shoal.label/": {
			effect: namespaceNarrows,
			reason: "every label rule is " +
				"NewAccessRule(sourcePolicy, labelPolicy...) and " +
				"AccessRule.Authorize requires every term, so a label " +
				"policy on source A governs a strict subset of A's " +
				"documents",
			sample: func(t *testing.T, source []byte) []byte {
				t.Helper()
				id, err := auth.LabelPolicyID(source, "secret")
				if err != nil {
					t.Fatal(err)
				}
				return id
			},
		},
	}

	// namespace -> the constants declaring it, for the failure message.
	found := map[string][]string{}
	parsed := 0
	fileSet := token.NewFileSet()
	entries, err := os.ReadDir(authPackageDir)
	if err != nil {
		t.Fatalf("read %s: %v — this test reads the auth package for "+
			"namespace declarations and cannot pin anything without it",
			authPackageDir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(
			fileSet, filepath.Join(authPackageDir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		parsed++
		for _, declaration := range file.Decls {
			general, ok := declaration.(*ast.GenDecl)
			if !ok ||
				(general.Tok != token.CONST && general.Tok != token.VAR) {
				continue
			}
			for _, specification := range general.Specs {
				value, ok := specification.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for index, expression := range value.Values {
					literal, ok := expression.(*ast.BasicLit)
					if !ok || literal.Kind != token.STRING {
						continue
					}
					text, err := strconv.Unquote(literal.Value)
					if err != nil || !namespaceShaped(text) {
						continue
					}
					declaredBy := "an unnamed declaration"
					if index < len(value.Names) {
						declaredBy = value.Names[index].Name
					}
					found[text] = append(found[text], declaredBy)
				}
			}
		}
	}

	// Vacuity guards. A walk that reads nothing reports every namespace as
	// classified, which is the failure this test exists to prevent.
	if parsed == 0 {
		t.Fatalf("no non-test Go file was parsed under %s", authPackageDir)
	}
	if len(found) == 0 {
		t.Fatalf("no namespace-shaped literal was found under %s, so the "+
			"walk is not reading the declarations it means to check",
			authPackageDir)
	}
	// And one known target, so the walk is pinned against something real
	// rather than against whatever it happens to match.
	if len(found["shoal.label/"]) == 0 {
		t.Fatal("the label namespace was not found; auth.LabelPolicyNamespace " +
			"is the declaration this walk is calibrated against, so either it " +
			"moved and this test needs its new home, or the walk is broken")
	}

	var unclassified []string
	for namespace, declarations := range found {
		if _, ok := classified[namespace]; !ok {
			sort.Strings(declarations)
			unclassified = append(unclassified, namespace+
				" (declared by "+strings.Join(declarations, ", ")+")")
		}
	}
	sort.Strings(unclassified)
	if len(unclassified) > 0 {
		t.Fatalf("these namespaces are declared in the auth package and are "+
			"not classified against refuseUnconfinedRetrieval: %s\n\n"+
			"If grants in the namespace NARROW within a source, the way "+
			"label policies do, they are already inside the action's scope "+
			"and must be dropped from the policy comparison for the "+
			"action's own source — see withoutOwnLabelPolicies. Leaving one "+
			"in the comparison refuses every holder an unconfined executor "+
			"and says nothing about why (#564). If grants can WIDEN, leave "+
			"them compared, which is what the filter already does. If the "+
			"literal is not a grant-policy namespace at all, say so. Either "+
			"way, record the decision in this test.",
			strings.Join(unclassified, ", "))
	}

	// The table must not rot either: an entry for a namespace that no longer
	// exists is a classification nobody is holding to, and it would make the
	// next namespace to reuse that string look decided.
	var stale []string
	for namespace := range classified {
		if len(found[namespace]) == 0 {
			stale = append(stale, namespace)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Fatalf("this test classifies namespaces that the auth package no "+
			"longer declares: %s — remove them, or the next namespace to "+
			"take one of those strings is classified by accident",
			strings.Join(stale, ", "))
	}
	// The filter's default is to compare, which is what makes an
	// unclassified namespace fail closed rather than open, and it is the
	// expectation every classification below is read against. A
	// non-canonical ID in the label namespace is included because the
	// namespace alone is not the thing that narrows: only an ID
	// auth.ParseLabelPolicyID accepts, for the action's own source, is.
	ordinary := [][]byte{
		[]byte("policy"),
		[]byte(auth.LabelPolicyIDPrefix + "not-canonical"),
	}
	if kept := withoutOwnLabelPolicies(ordinary, []byte("source")); len(kept) != len(ordinary) {
		t.Fatalf("withoutOwnLabelPolicies kept %d of %d: an ordinary policy "+
			"and a non-canonical ID in the label namespace must both stay "+
			"in the comparison, or a grant nobody has reasoned about widens "+
			"retrieval silently", len(kept), len(ordinary))
	}

	// Every classification is checked against withoutOwnLabelPolicies, so an
	// entry cannot be satisfied by prose. A namespace classified as
	// narrowing must actually be dropped for the action's own source, and
	// kept for any other source; one classified as widening must be kept.
	for _, namespace := range sortedNamespaces(classified) {
		class := classified[namespace]
		// The reason is the entry's justification and the only thing a
		// reviewer can check the classification against, so it is required
		// rather than decorative.
		if strings.TrimSpace(class.reason) == "" {
			t.Fatalf("%s is classified with no reason; the classification "+
				"is a claim about what grants in the namespace can reach, "+
				"and it has to be written down to be reviewable", namespace)
		}
		if class.effect == namespaceNotAPolicy {
			continue
		}
		if class.sample == nil {
			t.Fatalf("%s is classified but carries no sample ID; the "+
				"classification cannot be checked against the filter, "+
				"which is the only thing that makes it true", namespace)
		}
		own, other := []byte("source"), []byte("another-source")
		id := class.sample(t, own)
		if !strings.HasPrefix(string(id), namespace) {
			t.Fatalf("%s: the sample ID %q is not in the namespace it "+
				"classifies, so it checks nothing", namespace, id)
		}
		keptOwn := withoutOwnLabelPolicies([][]byte{id}, own)
		keptOther := withoutOwnLabelPolicies([][]byte{id}, other)
		switch class.effect {
		case namespaceNarrows:
			if len(keptOwn) != 0 {
				t.Fatalf("%s is classified as narrowing within a source, "+
					"but withoutOwnLabelPolicies keeps %q for the action's "+
					"own source — so every holder of this grant is refused "+
					"an unconfined executor, which is the #564 regression. "+
					"Drop the namespace in the filter, or reclassify it.",
					namespace, id)
			}
			if len(keptOther) != 1 {
				t.Fatalf("%s: the filter dropped %q for a source that is "+
					"not the action's. A grant that narrows within source A "+
					"says nothing about source B, and dropping it there "+
					"hides a principal that really is wider.",
					namespace, id)
			}
		case namespaceWidens:
			if len(keptOwn) != 1 {
				t.Fatalf("%s is classified as able to widen, but "+
					"withoutOwnLabelPolicies drops %q — a widening grant "+
					"left out of the comparison is a scope statement the "+
					"record does not keep (#370)", namespace, id)
			}
		}
	}
}

// namespaceClass is how one namespace stands with respect to the policy
// comparison in refuseUnconfinedRetrieval.
type namespaceClass struct {
	effect namespaceEffect
	reason string
	// sample mints a policy ID in this namespace for one source. It is
	// required for anything that is a grant policy: it is what turns the
	// classification from a claim into a check.
	sample func(t *testing.T, source []byte) []byte
}

type namespaceEffect int

const (
	// namespaceNarrows: grants are a strict subset of one source's scope, so
	// they are dropped from the comparison for the action's own source.
	namespaceNarrows namespaceEffect = iota
	// namespaceWidens: grants can reach beyond the action's scope, so they
	// are compared like any other policy.
	namespaceWidens
	// namespaceNotAPolicy: a slash-terminated literal that is not a
	// grant-policy namespace at all.
	namespaceNotAPolicy
)

func sortedNamespaces(classified map[string]namespaceClass) []string {
	keys := make([]string, 0, len(classified))
	for key := range classified {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// namespaceShaped reports whether a string literal looks like a policy
// namespace: a dotted stem and a trailing separator, as in "shoal.label/".
// The dot is what separates a namespace from a version segment such as
// "v1/", which is the other slash-terminated literal in the auth package.
func namespaceShaped(text string) bool {
	if !strings.HasSuffix(text, "/") {
		return false
	}
	return strings.Contains(strings.TrimSuffix(text, "/"), ".")
}
