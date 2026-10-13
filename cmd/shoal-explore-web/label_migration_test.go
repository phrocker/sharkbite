// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"context"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/labelmigration"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
)

// The startup wiring of the label migration (#570, PR4). What the migration
// does to the catalog is pinned in pkg/explorer/authorized
// (TestLabelMigration*); these tests pin that this command runs it before
// serving, records it, fails closed, and reports it.

func labelMigrationConfig(root string) serviceConfig {
	now := time.Now().UTC()
	return serviceConfig{
		backend: "embedded", data: filepath.Join(root, "corpus"),
		policyDir: filepath.Join(root, "policy"),
		resolver:  auth.NewAuthority().Resolver(),
		clock:     func() time.Time { return now },
	}
}

func TestStartupRunsTheLabelMigrationOnEveryStart(t *testing.T) {
	root := t.TempDir()
	config := labelMigrationConfig(root)
	for start := 0; start < 2; start++ {
		opened, err := openService(context.Background(), config)
		if err != nil {
			t.Fatal(err)
		}
		outcome := opened.labelMigration
		opened.close()
		if outcome.record.Version != authorized.LabelMigrationVersion {
			t.Fatalf("start %d did not run the migration: %+v", start, outcome)
		}
		var log bytes.Buffer
		printLabelMigration(&log, outcome)
		if !strings.Contains(log.String(), "Label migration v1: examined") {
			t.Fatalf("start %d log = %q", start, log.String())
		}
		// The report is in the catalog, so the listing reads it.
		var listing bytes.Buffer
		if err := listUntranslatableLabels(
			context.Background(), &listing, config.policyDir); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(listing.String(), "0 untranslatable") {
			t.Fatalf("listing = %q", listing.String())
		}
	}
}

type failingMigrator struct{}

func (failingMigrator) MigrateLabelledDocuments(
	context.Context, *labelmigration.Capability,
) (authorized.LabelMigrationRecord, error) {
	return authorized.LabelMigrationRecord{}, errors.New("injected store failure")
}

func TestLabelMigrationFailureRefusesToServe(t *testing.T) {
	_, err := runLabelMigration(context.Background(), failingMigrator{})
	if err == nil || !strings.Contains(err.Error(), "refusing to serve") ||
		!strings.Contains(err.Error(), "injected store failure") {
		t.Fatalf("migration failure = %v, want a refusal to serve", err)
	}
	if _, err := runLabelMigration(context.Background(), nil); err == nil {
		t.Fatal("a nil client was accepted")
	}
}

// directoryEntries lists every path under dir with its modification time
// and, for a file, its bytes, for proving a read leaves it untouched: a file
// rewritten with the same bytes still moves its mtime, and an entry created
// or removed moves its directory's.
func directoryEntries(t *testing.T, dir string) []string {
	t.Helper()
	var entries []string
	if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		modified := info.ModTime().Format(time.RFC3339Nano)
		if entry.IsDir() {
			entries = append(entries, path+"/ "+modified)
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		entries = append(entries, path+" "+modified+" "+string(content))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return entries
}

// listUnchanged runs the listing and fails unless the policy directory is
// byte-for-byte what it was before.
func listUnchanged(t *testing.T, root string, output *bytes.Buffer) error {
	t.Helper()
	policy := filepath.Join(root, "policy")
	before := directoryEntries(t, policy)
	output.Reset()
	err := run(context.Background(),
		[]string{"-list-untranslatable-labels", "-state-dir", root}, output)
	if after := directoryEntries(t, policy); !reflect.DeepEqual(before, after) {
		t.Fatalf("the listing changed the policy directory:\n%q\n->\n%q", before, after)
	}
	return err
}

func TestListUntranslatableLabelsFlag(t *testing.T) {
	root := t.TempDir()
	policy := filepath.Join(root, "policy")
	var output bytes.Buffer
	// No catalog: refused, and none is created.
	err := run(context.Background(),
		[]string{"-list-untranslatable-labels", "-state-dir", root}, &output)
	if err == nil || !strings.Contains(err.Error(), "no policy catalog") {
		t.Fatalf("listing without a catalog = %v", err)
	}
	if _, statErr := os.Stat(policy); !os.IsNotExist(statErr) {
		t.Fatalf("the listing created a policy catalog: %v", statErr)
	}
	// An existing but empty directory: refused, and left empty. The storage
	// engine would create the catalog table in it.
	if err := os.Mkdir(policy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := listUnchanged(t, root, &output); err == nil ||
		!strings.Contains(err.Error(), "no policy catalog") {
		t.Fatalf("listing an empty directory = %v", err)
	}
	// A bare table directory: the engine would give it a WAL.
	if err := os.Mkdir(filepath.Join(policy, "_shoal_policy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := listUnchanged(t, root, &output); err == nil ||
		!strings.Contains(err.Error(), "no committed records") {
		t.Fatalf("listing a bare table directory = %v", err)
	}
	if err := os.Remove(filepath.Join(policy, "_shoal_policy")); err != nil {
		t.Fatal(err)
	}

	// A catalog still open in a "running workspace": its writes are in the
	// WAL, which opening the engine would replay into a new file.
	store, err := authorized.OpenDurablePolicyStore(policy)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := store.CompareAndSwapSourceClaim(
		context.Background(), "file:///claimed.md", nil, labelMigrationProbeRule(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitSourceClaim(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	if err := listUnchanged(t, root, &output); err == nil ||
		!strings.Contains(err.Error(), "unflushed writes") {
		t.Fatalf("listing a catalog with an unflushed WAL = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// A running workspace that is idle, so its WAL is empty, is refused too:
	// it holds the catalog's directory lock (#686).
	store, err = authorized.OpenDurablePolicyStore(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := listUnchanged(t, root, &output); err == nil ||
		!strings.Contains(err.Error(), "is already open") {
		store.Close()
		t.Fatalf("listing an idle open catalog = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Cleanly closed, but the migration never ran.
	if err := listUnchanged(t, root, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "has not completed") {
		t.Fatalf("listing before the migration = %q", output.String())
	}

	// A completed migration with an untranslatable document and a drift.
	store, err = authorized.OpenDurablePolicyStore(policy)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutLabelMigration(context.Background(), authorized.LabelMigrationRecord{
		Version: authorized.LabelMigrationVersion, Documents: 1, Tightened: 1,
		Untranslatable: []authorized.UntranslatableLabel{{
			DocumentID: "doc-1", RevisionID: "rev-1", SourceURI: "file:///old.md",
			EscapedLabel: `"top secret!"`, Reason: "document node visibility does not parse",
		}},
		Drift: []authorized.RevisionDrift{{
			DocumentID: "doc-2", SourceURI: "file:///drifted.md",
			CatalogRevisionID: "rev-a", BaseRevisionID: "rev-b",
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := listUnchanged(t, root, &output); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"1 untranslatable", "document doc-1 revision rev-1",
		`source "file:///old.md"`, `label "top secret!"`, "does not parse",
		`document doc-2 source "file:///drifted.md": catalog revision rev-a, corpus revision rev-b`,
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("listing %q lacks %q", output.String(), want)
		}
	}
}

// labelMigrationProbeRule is any valid rule, for writing a claim.
func labelMigrationProbeRule(t *testing.T) authorized.AccessRule {
	t.Helper()
	policy, err := auth.NewPolicy(auth.PolicyConfig{
		AuthorizationDomain: []byte("domain"), SourceID: []byte("source"),
		GrantPolicyID: []byte("policy"), Epoch: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	rule, err := authorized.NewAccessRule(policy)
	if err != nil {
		t.Fatal(err)
	}
	return rule
}

// TestLabelMigrationCapabilityMintSites pins that the migration capability is
// minted only by the two startup paths that serve the authorized store, and
// called there rather than taken as a value.
func TestLabelMigrationCapabilityMintSites(t *testing.T) {
	const (
		capabilityPackage = "github.com/phrocker/shoal-oss/internal/labelmigration"
		constructor       = "NewCapability"
	)
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var sites []mintSite
	fileSet := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if strings.HasPrefix(entry.Name(), ".") || entry.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			return err
		}
		locals, unnamed := importedNames(parsed, capabilityPackage)
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if unnamed {
			t.Errorf("%s dot- or blank-imports %s", relative, capabilityPackage)
		}
		if len(locals) > 0 {
			sites = append(sites, mintSites(parsed, relative, locals, constructor)...)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(sites, func(left, right int) bool { return sites[left].file < sites[right].file })
	want := []mintSite{
		{file: filepath.Join("cmd", "shoal-explore-web", "label_migration.go"),
			function: "runLabelMigration", called: true},
		{file: filepath.Join("cmd", "shoal-mcp", "workspace.go"),
			function: "migrateLabels", called: true},
	}
	if len(sites) != len(want) {
		t.Fatalf("the label migration capability is minted at %+v, want %+v", sites, want)
	}
	for index := range want {
		if sites[index] != want[index] {
			t.Errorf("mint site %d = %+v, want %+v", index, sites[index], want[index])
		}
	}
}
