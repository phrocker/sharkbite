// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package guard

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// entityKinds is every guard entity kind in use, with the store that owns it.
// The head key is (domain, kind, entity ID) and names no store, so two stores
// that pick one letter share a key space. ownedBy keeps either from adopting
// the other's heads, but a caller who can choose an entity ID in one can
// still occupy an ID the other needs, which is a denial of service on that
// entity. A new kind takes a letter nobody holds.
//
// Keys are the letter; values are the owning file, relative to the module
// root.
var entityKinds = map[byte][]string{
	'A': {"internal/explorerfleet/store.go"},
	'C': {"internal/explorerfleetevents/adapter.go"},
	'D': {
		"internal/explorerconformance/fixture.go",
		"internal/explorerfleet/dispatch_store.go",
		"pkg/explorer/publication.go",
	},
	'E': {"internal/explorerfleetevents/adapter.go"},
	'F': {"internal/explorerfleetevents/adapter.go"},
	'G': {"internal/explorerfleetevents/adapter.go"},
	'P': {
		"internal/explorerfleet/approval_store.go",
		"internal/explorerfleetevents/adapter.go",
	},
	'S': {"internal/explorerfleetevents/adapter.go"},
	'T': {"internal/explorerfleet/dispatch_store.go"},
	'U': {"internal/explorerfleetevents/adapter.go"},
	'V': {"internal/explorerfleet/approval_event_store.go"},
	'e': {"internal/explorerfleetevents/adapter.go"},
}

// knownSharedKinds are the letters more than one store already uses. They
// predate this registry, and moving one means migrating live heads, so they
// are recorded rather than fixed. Each is safe from adoption by ownedBy and
// keeps the denial-of-service residual described on entityKinds.
//
//   - 'P': fleet approvals (raw, caller-chosen approval ID) and fleet-event
//     publications (hex of an event ID the publisher can precompute). For a
//     lifecycle event, anyone who can read the committed record can compute
//     the event ID too. So while that event is unpublished, an approval
//     request at its hex occupies it: the event is suppressed, and the
//     action's outbox wedges (#681).
//
//   - 'D': fleet dispatch (raw, caller-chosen action ID), explorer documents
//     (document ID), and the conformance fixture (its own domain). Dispatch
//     and documents share one domain in the shipped binary. Their ID spaces
//     are kept disjoint rather than their letters: a document ID always has
//     the shape explorer.IsDocumentID describes, and dispatch enqueue and
//     approval requests refuse new action IDs of that shape (fleet's
//     reservedActionID, #669). An action stored at such an ID before #669
//     is the only residual.
//
//     Fleet-event subscription delete receipts also used 'D' before #669. Any
//     principal holding dispatch could occupy all 4096 of their entity IDs and
//     so deny every subscription Delete, which is revocation. Receipts now
//     use 'U' at a new row prefix. The adapter still reads legacy receipt
//     rows for replay, but it never builds a 'D' entity, so it is no longer
//     registered here. Its old 'D' heads stay in storage under
//     fleet-events/default, unread and unwritten.
var knownSharedKinds = map[byte]bool{'P': true, 'D': true}

// storePolicies is the logical policy each store that holds a shared letter
// writes its heads under. ownedBy treats the policy as the head's owner, so
// stores that share a letter must not share a policy, or the ownership rule
// stops separating them.
var storePolicies = map[string]string{
	"internal/explorerconformance/fixture.go":  "policy-id",
	"internal/explorerfleet/approval_store.go": "fleet/approval/v1",
	"internal/explorerfleet/dispatch_store.go": "fleet/dispatch/v1",
	"internal/explorerfleetevents/adapter.go":  "fleet-events/default",
	"pkg/explorer/publication.go":              "embedded/default",
}

var (
	repositoryScanOnce sync.Once
	repositoryScan     struct {
		root     string
		found    map[byte]map[string]bool
		problems []string
		exempted map[string]bool
		universe *universe // the root module's, for the fixtures
		listing  *moduleListing
	}
)

// scanRepository runs the kind analysis (kindscan_test.go) over every module
// in the repository once, for every test that needs it.
func scanRepository(t *testing.T) {
	t.Helper()
	repositoryScanOnce.Do(func() {
		root := moduleRoot(t)
		repositoryScan.root = root
		repositoryScan.found = map[byte]map[string]bool{}
		repositoryScan.exempted = map[string]bool{}
		for _, dir := range repositoryModules(t, root) {
			listing := listModule(t, root, dir)
			u, scan := scanModule(t, listing)
			if dir == root {
				repositoryScan.universe, repositoryScan.listing = u, listing
			}
			if u == nil {
				continue
			}
			mergeKinds(repositoryScan.found, scan.found)
			for key := range scan.exempted {
				repositoryScan.exempted[key] = true
			}
			repositoryScan.problems = append(repositoryScan.problems, scan.problems...)
		}
	})
	if repositoryScan.universe == nil || repositoryScan.universe.guardPackage() == nil {
		t.Fatal("the repository scan did not load the guard")
	}
}

func mergeKinds(into, from map[byte]map[string]bool) {
	for kind, files := range from {
		if into[kind] == nil {
			into[kind] = map[string]bool{}
		}
		for file := range files {
			into[kind][file] = true
		}
	}
}

// registryErrors holds found to entityKinds. An unregistered letter, a
// registered letter that moved or is no longer used, and a letter held by two
// files that is not one of knownSharedKinds are each an error.
func registryErrors(found map[byte]map[string]bool) []string {
	var errs []string
	for kind, files := range found {
		owners := sortedKeys(files)
		registered := append([]string(nil), entityKinds[kind]...)
		sort.Strings(registered)
		if strings.Join(owners, ",") != strings.Join(registered, ",") {
			errs = append(errs, fmt.Sprintf(
				"guard entity kind %q is used by %v, registered to %v", kind, owners, registered))
		}
		if len(owners) > 1 && !knownSharedKinds[kind] {
			errs = append(errs, fmt.Sprintf(
				"guard entity kind %q is shared by %v; take an unused letter", kind, owners))
		}
	}
	for kind := range entityKinds {
		if found[kind] == nil {
			errs = append(errs, fmt.Sprintf(
				"guard entity kind %q is registered but no longer used", kind))
		}
	}
	sort.Strings(errs)
	return errs
}

// TestEveryGuardEntityKindIsRegistered holds every kind the code can write to
// guard.Entity.Kind, found by type checking every package that depends on the
// guard, to entityKinds. A value the analysis cannot resolve to a constant
// fails too, so how a kind is spelled cannot hide it, and how it is passed
// can hide it only in the ways kindscan_test.go lists as uncovered.
func TestEveryGuardEntityKindIsRegistered(t *testing.T) {
	scanRepository(t)
	for _, problem := range repositoryScan.problems {
		t.Errorf("unresolved guard entity kind: %s", problem)
	}
	if len(repositoryScan.found) == 0 {
		t.Fatal("the analysis found no guard entity kinds")
	}
	for _, err := range registryErrors(repositoryScan.found) {
		t.Error(err)
	}
	// An exemption nothing uses any more would silently cover whatever
	// next lands at its file and function.
	for _, exemptions := range []map[string]string{
		decodedKinds, decodedEntities, exportedKindMethods,
	} {
		for key := range exemptions {
			if !repositoryScan.exempted[key] {
				t.Errorf("exemption %s is no longer used; remove it", key)
			}
		}
	}
}

// kindFixtures are packages under testdata/kindspelling, each writing a kind
// in a way the regular-expression scan this analysis replaced did not see
// (#671). A fixture is type checked against the real guard and analysed on
// its own. The analysis must attribute each listed letter to the fixture or
// report a problem matching each listed fragment, and the letters must fail
// the registry.
var kindFixtures = map[string]struct {
	kinds    []byte
	problems []string
}{
	// const kind = 'Q', untyped.
	"untyped": {kinds: []byte{'Q'}},
	// byte(0x41), 'A', which explorerfleet's registry store already holds.
	"converted": {kinds: []byte{'A'}},
	// The kind is a parameter of a helper and of an interface method, or a
	// field of the caller's own request type.
	"helper": {kinds: []byte{'H', 'I', 'R'}},
	// Kinds the analysis cannot resolve are reported, not skipped.
	"nonconstant": {problems: []string{
		"non-constant *ast.IndexExpr",
		"which takes a kind, is used as a value",
		"compound assignment",
		"from which reflection can set a kind field, becomes any",
	}},
	// Dodges an independent review of #671 found in a first version of the
	// analysis, each passing it unreported.
	"generic":       {problems: []string{"is identical to github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard.Entity"}},
	"unnamed":       {problems: []string{"is identical to github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard.Entity"}},
	"ifacevalue":    {problems: []string{"Make, which takes a kind, is used as a value", "exported method"}},
	"jsonany":       {problems: []string{"*github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard.Entity, from which reflection can set a kind field, becomes any"}},
	"genericdecode": {problems: []string{"decode is instantiated with github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard.Entity"}},
	"jsonfield":     {problems: []string{"*kindspelling/jsonfield.req, from which reflection can set a kind field, becomes any"}},
	"reflectw":      {problems: []string{"from which reflection can set a kind field, becomes any"}},
	// From the second review.
	"localiface":  {problems: []string{"kindspelling/localiface.holder, which can hold a value from which reflection can set a kind field, becomes any"}},
	"tuplecall":   {problems: []string{"*github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard.Entity, from which reflection can set a kind field, becomes any"}},
	"tuplereturn": {problems: []string{"tuplereturn/fixture.go:17:", "tuplereturn/fixture.go:21:"}},
	"rangeassign": {problems: []string{"*github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard.Entity, from which reflection can set a kind field, becomes any"}},
	"errorf":      {problems: []string{"*kindspelling/errorf.carrier, from which reflection can set a kind field, becomes any"}},
	// Ordinary code the analysis must not report: slices and log.
	"quiet": {kinds: []byte{'B'}},
}

func TestKindAnalysisCatchesKnownDodges(t *testing.T) {
	scanRepository(t)
	u := repositoryScan.universe
	directory := filepath.Join(repositoryScan.root,
		"pkg", "explorer", "coordination", "guard", "testdata", "kindspelling")
	for name, want := range kindFixtures {
		t.Run(name, func(t *testing.T) {
			entries, err := filepath.Glob(filepath.Join(directory, name, "*.go"))
			if err != nil || len(entries) == 0 {
				t.Fatalf("fixture %s has no sources: %v", name, err)
			}
			fixture := u.check(t, "kindspelling/"+name, entries, nil)
			scan := scanKinds(t, repositoryScan.root, u,
				[]*scannedPackage{u.guardPackage(), fixture})
			file := "pkg/explorer/coordination/guard/testdata/kindspelling/" + name + "/fixture.go"
			merged := map[byte]map[string]bool{}
			mergeKinds(merged, repositoryScan.found)
			mergeKinds(merged, scan.found)
			errs := strings.Join(registryErrors(merged), "\n")
			for _, kind := range want.kinds {
				if !scan.found[kind][file] {
					t.Errorf("the analysis missed kind %q in %s; found %v, problems %v",
						kind, file, scan.found, scan.problems)
				}
				if !strings.Contains(errs, fmt.Sprintf("%q", kind)) {
					t.Errorf("kind %q from %s passed the registry:\n%s", kind, file, errs)
				}
			}
			if len(want.kinds) != 0 && len(scan.problems) != 0 {
				t.Errorf("problems in a fixture of constants: %v", scan.problems)
			}
			all := strings.Join(scan.problems, "\n")
			for _, fragment := range want.problems {
				if !strings.Contains(all, fragment) {
					t.Errorf("no problem matching %q; problems:\n%s", fragment, all)
				}
			}
		})
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test")
		}
		dir = parent
	}
}

func sortedKeys(values map[string]bool) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

// TestStoresSharingAKindLetterHaveDistinctPolicies holds storePolicies to the
// sources and to each other, for every shared letter.
func TestStoresSharingAKindLetterHaveDistinctPolicies(t *testing.T) {
	root := moduleRoot(t)
	for kind := range knownSharedKinds {
		seen := map[string]string{}
		for _, owner := range entityKinds[kind] {
			policy, ok := storePolicies[owner]
			if !ok {
				t.Fatalf("store %s shares kind %q but names no policy here", owner, kind)
			}
			source, err := os.ReadFile(filepath.Join(root, owner))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(source), `"`+policy+`"`) {
				t.Fatalf("store %s no longer writes policy %q", owner, policy)
			}
			if other, dup := seen[policy]; dup {
				t.Fatalf("stores %s and %s share kind %q and policy %q, so "+
					"the guard cannot tell their heads apart", other, owner, kind, policy)
			}
			seen[policy] = owner
		}
	}
}

// TestKindAnalysisSeesAPackageTheBuildExcludes: go list ./... does not list a
// directory whose every file the build excludes, so a package that builds
// only elsewhere (on Windows, say) would never be type checked. The
// analysis finds such a directory itself and holds its files to the
// syntactic rule for excluded files. Found in review of #671.
func TestKindAnalysisSeesAPackageTheBuildExcludes(t *testing.T) {
	scanRepository(t)
	file := filepath.Join(repositoryScan.root, "pkg", "explorer", "coordination", "guard",
		"testdata", "kindspelling", "winonly", "fixture.go")
	listing := *repositoryScan.listing
	listing.unlisted = append(append([]unlistedFiles(nil), listing.unlisted...),
		unlistedFiles{files: []string{file}, imports: importsOf(t, file)})
	u := listing.load(t, map[string]bool{guardImportPath: true})
	scan := scanKinds(t, repositoryScan.root, u, u.packages)
	want := "pkg/explorer/coordination/guard/testdata/kindspelling/winonly/fixture.go: " +
		"excluded from this build and imports the guard"
	if !strings.Contains(strings.Join(scan.problems, "\n"), want) {
		t.Fatalf("no problem %q; problems: %v", want, scan.problems)
	}
}
