// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package guard

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
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
		"internal/explorerfleetevents/adapter.go",
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
	'V': {"internal/explorerfleet/approval_event_store.go"},
	'e': {"internal/explorerfleetevents/adapter.go"},
}

// knownSharedKinds are the letters more than one store already uses. They
// predate this registry, and moving one means migrating live heads, so they
// are recorded rather than fixed. Each is safe from adoption by ownedBy and
// keeps the denial-of-service residual described on entityKinds.
//
//   - 'P': fleet approvals (raw, caller-chosen approval ID) and fleet-event
//     publications (hex of an event ID the publisher can precompute).
//   - 'D': fleet dispatch (raw, caller-chosen action ID), fleet-event
//     subscription delete receipts (a digest of one of 4096 slot rows, which
//     anyone can compute), explorer documents (document ID), and the
//     conformance fixture (its own domain). The residual here is serious: a
//     principal holding dispatch can enqueue actions at every delete-receipt
//     ID first and so deny every subscription Delete, which is revocation.
//     Closing it needs a new letter for one side and a migration of its
//     heads; it is tracked separately.
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

var kindPatterns = []*regexp.Regexp{
	regexp.MustCompile(`Kind:\s*'(.)'`),
	regexp.MustCompile(`EntityKind:\s*'(.)'`),
	regexp.MustCompile(`[A-Za-z]*[kK]ind\s+byte\s*=\s*'(.)'`),
	regexp.MustCompile(`ReceiptEntity\(\s*'(.)'`),
}

// TestEveryGuardEntityKindIsRegistered scans the module's non-test sources
// for guard entity kinds and holds them to entityKinds. An unregistered
// letter fails, a registered letter that moved fails, and a letter held by
// two stores fails unless it is one of knownSharedKinds.
func TestEveryGuardEntityKindIsRegistered(t *testing.T) {
	root := moduleRoot(t)
	found := map[byte]map[string]bool{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			if path != root && (strings.HasPrefix(name, ".") || name == "vendor" ||
				name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !strings.Contains(string(source), "coordination/guard\"") &&
			!strings.Contains(string(source), "EntityKind:") {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		for _, pattern := range kindPatterns {
			for _, match := range pattern.FindAllStringSubmatch(string(source), -1) {
				kind := match[1][0]
				if found[kind] == nil {
					found[kind] = map[string]bool{}
				}
				found[kind][relative] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) == 0 {
		t.Fatal("the scan found no guard entity kinds; the patterns are stale")
	}
	for kind, files := range found {
		owners := sortedKeys(files)
		registered := append([]string(nil), entityKinds[kind]...)
		sort.Strings(registered)
		if strings.Join(owners, ",") != strings.Join(registered, ",") {
			t.Errorf("guard entity kind %q is used by %v, registered to %v",
				kind, owners, registered)
		}
		if len(owners) > 1 && !knownSharedKinds[kind] {
			t.Errorf("guard entity kind %q is shared by %v; take an unused letter",
				kind, owners)
		}
	}
	for kind := range entityKinds {
		if found[kind] == nil {
			t.Errorf("guard entity kind %q is registered but no longer used", kind)
		}
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
