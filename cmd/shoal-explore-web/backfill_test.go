// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

package main

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/devbackfill"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
)

// otherAuthenticator stands in for a real authentication integration. It must
// never be given the development-only corpus backfill.
type otherAuthenticator struct{}

func (otherAuthenticator) Authenticate(*http.Request) (auth.Decision, error) {
	return auth.Decision{}, errors.New("not authenticated")
}

// TestDevelopmentBackfillGate pins the exact conditions under which the
// development corpus backfill may run: the development authenticator, which
// only -dev-auth produces, and a loopback listener.
func TestDevelopmentBackfillGate(t *testing.T) {
	t.Parallel()
	authority := auth.NewAuthority()
	development, err := selectAuthenticator(true, "", oidcConfig{}, "127.0.0.1:8080", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name          string
		authenticator webapi.Authenticator
		address       string
		binder        auth.Binder
		allowed       bool
	}{
		{"development on loopback", development, "127.0.0.1:8080", authority.Binder(), true},
		{"development on IPv6 loopback", development, "[::1]:8080", authority.Binder(), true},
		{"development on localhost", development, "localhost:8080", authority.Binder(), true},
		{"development on all interfaces", development, "0.0.0.0:8080", authority.Binder(), false},
		{"development on IPv6 wildcard", development, "[::]:8080", authority.Binder(), false},
		{"development on a bare port", development, ":8080", authority.Binder(), false},
		{"development on a routable address", development, "10.0.0.5:8080", authority.Binder(), false},
		{"real authenticator on loopback", otherAuthenticator{}, "127.0.0.1:8080", authority.Binder(), false},
		{"no authenticator on loopback", nil, "127.0.0.1:8080", authority.Binder(), false},
		{"development without a binder", development, "127.0.0.1:8080", nil, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			backfill := newDevelopmentBackfill(
				testCase.authenticator, testCase.address, testCase.binder)
			if allowed := backfill != nil; allowed != testCase.allowed {
				t.Fatalf(
					"newDevelopmentBackfill(%s) allowed = %t, want %t",
					testCase.address, allowed, testCase.allowed)
			}
		})
	}
}

// recordingBackfiller records the context it was called with so the test can
// prove the backfill runs under a bound development decision.
type recordingBackfiller struct {
	registered int
	err        error
	ctx        context.Context
	capability *devbackfill.Capability
}

func (b *recordingBackfiller) BackfillExistingDocumentsForDevelopment(
	ctx context.Context,
	capability *devbackfill.Capability,
) (int, error) {
	b.ctx = ctx
	b.capability = capability
	if b.err != nil {
		return 0, b.err
	}
	return b.registered, nil
}

// TestDevelopmentBackfillBindsAndFailsClosed proves the backfill runs under a
// bound decision and that a failure is surfaced instead of swallowed.
func TestDevelopmentBackfillBindsAndFailsClosed(t *testing.T) {
	authority := auth.NewAuthority()
	development, err := selectAuthenticator(true, "", oidcConfig{}, "127.0.0.1:8080", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	backfill := newDevelopmentBackfill(
		development, "127.0.0.1:8080", authority.Binder())
	if backfill == nil {
		t.Fatal("the development backfill was refused on loopback")
	}

	succeeding := &recordingBackfiller{registered: 3}
	registered, err := backfill.run(context.Background(), succeeding)
	if err != nil {
		t.Fatal(err)
	}
	if registered != 3 {
		t.Fatalf("registered = %d, want 3", registered)
	}
	decision, err := authority.Resolver().Resolve(succeeding.ctx)
	if err != nil {
		t.Fatalf("the backfill ran without a bound decision: %v", err)
	}
	if decision.Subject() != developmentSubject {
		t.Fatalf("backfill subject = %q", decision.Subject())
	}

	if !succeeding.capability.Granted() {
		t.Fatal("the backfill ran without the development capability")
	}

	failing := &recordingBackfiller{err: errors.New("catalog write failed")}
	registered, err = backfill.run(context.Background(), failing)
	if err == nil {
		t.Fatal("a failed backfill was reported as success")
	}
	if registered != 0 {
		t.Fatalf("a failed backfill registered = %d, want 0", registered)
	}
	if !strings.Contains(err.Error(), "refusing to serve") {
		t.Fatalf("unclear diagnostic: %v", err)
	}

	var absent *developmentBackfill
	if registered, err := absent.run(context.Background(), succeeding); err != nil ||
		registered != 0 {
		t.Fatalf("an absent backfill ran: registered = %d err = %v", registered, err)
	}
}

// TestBackfillGrantsPreExistingCorpusOnlyWhenGated proves both halves of the
// bargain for a pre-durability corpus (documents ingested through the raw
// explorer, so nothing is registered). With the gate open the backfill
// registers the corpus and it is served. With the gate closed the corpus has
// documents but the durable policy catalog is empty — indistinguishable from a
// lost policy volume — so the workspace now refuses to open rather than serve a
// silently under-authorized corpus.
//
// Before the split-brain guard the ungated half opened successfully and served
// 0 documents: the corpus was silently hidden. After the guard the ungated half
// fails openService with a diagnostic, which is the behaviour asserted here.
func TestBackfillGrantsPreExistingCorpusOnlyWhenGated(t *testing.T) {
	t.Parallel()
	t.Run("backfilled", func(t *testing.T) {
		data := preExistingCorpus(t)
		ctx := context.Background()
		authority := auth.NewAuthority()
		development, err := selectAuthenticator(true, "", oidcConfig{}, "127.0.0.1:8080", time.Now)
		if err != nil {
			t.Fatal(err)
		}
		backfill := newDevelopmentBackfill(
			development, "127.0.0.1:8080", authority.Binder())
		if backfill == nil {
			t.Fatal("the development backfill was refused on loopback")
		}
		opened, err := openService(ctx, serviceConfig{
			backend:  "embedded",
			data:     data,
			resolver: authority.Resolver(),
			clock:    time.Now,
			backfill: backfill,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer opened.close()
		if opened.backfilled != 1 {
			t.Fatalf("backfilled = %d, want 1", opened.backfilled)
		}

		authenticator, err := newDevelopmentAuthenticator(time.Now)
		if err != nil {
			t.Fatal(err)
		}
		decision, err := authenticator.mint(nil)
		if err != nil {
			t.Fatal(err)
		}
		bound, err := authority.Binder().Bind(ctx, decision)
		if err != nil {
			t.Fatal(err)
		}
		response, err := opened.service.Documents(
			bound, webapi.DocumentsRequest{Page: webapi.PageRequest{Limit: 10}})
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Documents) != 1 {
			t.Fatalf("visible documents = %d, want 1", len(response.Documents))
		}
	})

	t.Run("not backfilled", func(t *testing.T) {
		data := preExistingCorpus(t)
		opened, err := openService(context.Background(), serviceConfig{
			backend:  "embedded",
			data:     data,
			resolver: auth.NewAuthority().Resolver(),
			clock:    time.Now,
			backfill: nil,
		})
		if err == nil {
			opened.close()
			t.Fatal(
				"a corpus with no registrations opened without the gate; " +
					"a lost policy volume would be served silently")
		}
		if opened.service != nil {
			t.Fatal("a refused split-brain workspace still produced a service")
		}
		if !strings.Contains(err.Error(), "split-brain workspace") {
			t.Fatalf("unclear diagnostic: %v", err)
		}
	})
}

// TestDurableCorpusServedAfterRestartWithoutBackfill proves the startup
// backfill is not load-bearing once registrations persist. A document is
// ingested through the authorized service of one process — so the durable
// policy catalog records its registration — and then a second process opens the
// same corpus with the backfill explicitly disabled and still serves the
// document. Contrast preExistingCorpus, which ingests through the raw explorer
// (registering nothing) and therefore does need the backfill: that is exactly
// the pre-durability corpus the backfill exists to migrate. The two processes
// use independent authorities, so this is a genuine restart rather than shared
// in-memory state.
func TestDurableCorpusServedAfterRestartWithoutBackfill(t *testing.T) {
	t.Parallel()
	data := t.TempDir()
	ctx := context.Background()

	ingestAuthority := auth.NewAuthority()
	first, err := openService(ctx, serviceConfig{
		backend:  "embedded",
		data:     data,
		resolver: ingestAuthority.Resolver(),
		clock:    time.Now,
		backfill: nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.backfilled != 0 {
		t.Fatalf("a nil backfill ran during ingest: %d", first.backfilled)
	}

	ingestDecision, err := mintDevelopmentDecision(t)
	if err != nil {
		t.Fatal(err)
	}
	ingestCtx, err := ingestAuthority.Binder().Bind(ctx, ingestDecision)
	if err != nil {
		t.Fatal(err)
	}
	provider, ok := first.service.(webapi.IngestProvider)
	if !ok {
		first.close()
		t.Fatal("embedded service does not provide ingest")
	}
	ingested, err := provider.Ingest(ingestCtx, webapi.IngestRequest{
		Files: []webapi.UploadFile{{
			Name:    "durable.md",
			Content: []byte("# Durable\n\nIngested by a durable-backed process.\n"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ingested.Files) != 1 {
		t.Fatalf("ingest result files = %#v, want one", ingested.Files)
	}
	first.close()

	// Restart: a fresh authority and the backfill explicitly disabled. If the
	// backfill were papering over a non-durable catalog, the corpus would be
	// invisible here.
	readAuthority := auth.NewAuthority()
	second, err := openService(ctx, serviceConfig{
		backend:  "embedded",
		data:     data,
		resolver: readAuthority.Resolver(),
		clock:    time.Now,
		backfill: nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer second.close()
	if second.backfilled != 0 {
		t.Fatalf("a nil backfill ran after restart: %d", second.backfilled)
	}

	readDecision, err := mintDevelopmentDecision(t)
	if err != nil {
		t.Fatal(err)
	}
	readCtx, err := readAuthority.Binder().Bind(ctx, readDecision)
	if err != nil {
		t.Fatal(err)
	}
	response, err := second.service.Documents(
		readCtx, webapi.DocumentsRequest{Page: webapi.PageRequest{Limit: 10}})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Documents) != 1 {
		t.Fatalf(
			"documents served after restart without backfill = %d, want 1",
			len(response.Documents))
	}
}

// mintDevelopmentDecision mints a development-principal decision carrying the
// workspace policy the authorized client enforces.
func mintDevelopmentDecision(t *testing.T) (auth.Decision, error) {
	t.Helper()
	authenticator, err := newDevelopmentAuthenticator(time.Now)
	if err != nil {
		return auth.Decision{}, err
	}
	return authenticator.mint(nil)
}

// ingestOneThroughService ingests a single document through the authorized
// service so the durable policy catalog records its registration, then closes
// the process. It is the setup a lost-volume test needs: a corpus whose
// documents are genuinely registered, so removing the policy directory
// afterwards reproduces the split-brain signature rather than a pre-durability
// corpus.
func ingestOneThroughService(t *testing.T, config serviceConfig) {
	t.Helper()
	ctx := context.Background()
	authority := auth.NewAuthority()
	config.resolver = authority.Resolver()
	config.clock = time.Now
	config.backfill = nil
	opened, err := openService(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := mintDevelopmentDecision(t)
	if err != nil {
		opened.close()
		t.Fatal(err)
	}
	ingestCtx, err := authority.Binder().Bind(ctx, decision)
	if err != nil {
		opened.close()
		t.Fatal(err)
	}
	provider, ok := opened.service.(webapi.IngestProvider)
	if !ok {
		opened.close()
		t.Fatal("embedded service does not provide ingest")
	}
	if _, err := provider.Ingest(ingestCtx, webapi.IngestRequest{
		Files: []webapi.UploadFile{{
			Name:    "registered.md",
			Content: []byte("# Registered\n\nIngested through the service.\n"),
		}},
	}); err != nil {
		opened.close()
		t.Fatal(err)
	}
	opened.close()
}

// TestOpenServiceRefusesSplitBrainStateDirectory reproduces a lost policy
// volume: a document is ingested through the service (so the durable catalog
// records its registration), the process closes, the policy directory is
// removed, and the process reopens the same corpus in production (backfill
// nil).
//
// Before the split-brain guard this reopened successfully and served 0
// documents — the corpus was present but every registration was gone, so the
// workspace silently served an under-populated corpus and the backfill could
// partially mask the loss. After the guard openService refuses with a
// diagnostic naming the corpus, the document count, and the empty policy
// directory, so the lost volume is diagnosed instead of hidden.
func TestOpenServiceRefusesSplitBrainStateDirectory(t *testing.T) {
	t.Parallel()
	data := t.TempDir()
	ingestOneThroughService(t, serviceConfig{backend: "embedded", data: data})

	// The lost volume: the corpus survives, the policy catalog does not.
	if err := os.RemoveAll(policyStoreDir(data)); err != nil {
		t.Fatal(err)
	}

	opened, err := openService(context.Background(), serviceConfig{
		backend:  "embedded",
		data:     data,
		resolver: auth.NewAuthority().Resolver(),
		clock:    time.Now,
		backfill: nil,
	})
	if err == nil {
		opened.close()
		t.Fatal(
			"a corpus whose policy volume was lost opened without refusal; " +
				"it would serve a silently under-authorized corpus")
	}
	if opened.service != nil {
		t.Fatal("a refused split-brain workspace still produced a service")
	}
	if !strings.Contains(err.Error(), "split-brain workspace") {
		t.Fatalf("unclear diagnostic: %v", err)
	}
}

// TestStateDirLayoutSharesOneMountPoint proves the recommended layout keeps the
// corpus and durable policy catalog as siblings inside a single state root, so
// mounting that one directory as a volume persists both. A document is ingested
// under the state root and served after a restart with the backfill disabled;
// the split-brain guard does not fire because both directories share the mount.
func TestStateDirLayoutSharesOneMountPoint(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	corpus, policy := resolveWorkspacePaths(root, ".shoal/explorer", "")
	if corpus != filepath.Join(root, "corpus") {
		t.Fatalf("corpus = %q, want %q", corpus, filepath.Join(root, "corpus"))
	}
	if policy != filepath.Join(root, "policy") {
		t.Fatalf("policy = %q, want %q", policy, filepath.Join(root, "policy"))
	}

	ingestOneThroughService(t, serviceConfig{
		backend: "embedded", data: corpus, policyDir: policy})

	ctx := context.Background()
	authority := auth.NewAuthority()
	opened, err := openService(ctx, serviceConfig{
		backend:   "embedded",
		data:      corpus,
		policyDir: policy,
		resolver:  authority.Resolver(),
		clock:     time.Now,
		backfill:  nil,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer opened.close()

	decision, err := mintDevelopmentDecision(t)
	if err != nil {
		t.Fatal(err)
	}
	readCtx, err := authority.Binder().Bind(ctx, decision)
	if err != nil {
		t.Fatal(err)
	}
	response, err := opened.service.Documents(
		readCtx, webapi.DocumentsRequest{Page: webapi.PageRequest{Limit: 10}})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Documents) != 1 {
		t.Fatalf(
			"documents served from the state root after restart = %d, want 1",
			len(response.Documents))
	}
}

// TestResolveWorkspacePathsPrecedence pins the three-flag precedence: -state-dir
// nests corpus/ and policy/ under one mount, -data keeps the legacy sibling
// layout, and -policy-dir overrides the policy location in either case.
func TestResolveWorkspacePathsPrecedence(t *testing.T) {
	t.Parallel()
	for _, testCase := range []struct {
		name       string
		stateDir   string
		dataDir    string
		policyDir  string
		wantCorpus string
		wantPolicy string
	}{
		{
			name:       "legacy data sibling",
			dataDir:    filepath.Join("var", "data"),
			wantCorpus: filepath.Join("var", "data"),
			wantPolicy: policyStoreDir(filepath.Join("var", "data")),
		},
		{
			name:       "state root nests both",
			stateDir:   filepath.Join("var", "state"),
			dataDir:    filepath.Join("var", "data"),
			wantCorpus: filepath.Join("var", "state", "corpus"),
			wantPolicy: filepath.Join("var", "state", "policy"),
		},
		{
			name:       "policy dir overrides state root",
			stateDir:   filepath.Join("var", "state"),
			policyDir:  filepath.Join("mnt", "policy"),
			wantCorpus: filepath.Join("var", "state", "corpus"),
			wantPolicy: filepath.Join("mnt", "policy"),
		},
		{
			name:       "policy dir overrides data sibling",
			dataDir:    filepath.Join("var", "data"),
			policyDir:  filepath.Join("mnt", "policy"),
			wantCorpus: filepath.Join("var", "data"),
			wantPolicy: filepath.Join("mnt", "policy"),
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			corpus, policy := resolveWorkspacePaths(
				testCase.stateDir, testCase.dataDir, testCase.policyDir)
			if corpus != testCase.wantCorpus {
				t.Fatalf("corpus = %q, want %q", corpus, testCase.wantCorpus)
			}
			if policy != testCase.wantPolicy {
				t.Fatalf("policy = %q, want %q", policy, testCase.wantPolicy)
			}
		})
	}
}

// TestRunBackfillsPreExistingCorpusOverHTTP is the end-to-end proof that a
// restarted workspace serves the corpus it already holds.
func TestRunBackfillsPreExistingCorpusOverHTTP(t *testing.T) {
	data := preExistingCorpus(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader, writer := io.Pipe()
	defer writer.Close()
	done := make(chan error, 1)
	go func() {
		done <- run(
			ctx,
			[]string{"-listen", "127.0.0.1:0", "-dev-auth", "-data", data},
			writer,
		)
	}()
	address := awaitListenAddress(t, reader)
	go func() { _, _ = io.Copy(io.Discard, reader) }()

	listed := postJSON(
		t, "http://"+address+"/api/v1/documents", `{"page":{"limit":10}}`)
	if !strings.Contains(string(listed), "file:///pre-existing.md") {
		t.Fatalf("the pre-existing corpus was not served: %s", listed)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("server exit = %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the server did not shut down")
	}
}

// TestOpenServiceRefusesWhenBackfillFails proves a failed backfill stops the
// workspace from opening at all, rather than serving a corpus it did not
// finish authorizing. The backfill is bound by a different authority than the
// client resolves with, so the registration attempt fails closed.
func TestOpenServiceRefusesWhenBackfillFails(t *testing.T) {
	t.Parallel()
	data := preExistingCorpus(t)
	serving := auth.NewAuthority()
	foreign := auth.NewAuthority()
	development, err := selectAuthenticator(true, "", oidcConfig{}, "127.0.0.1:8080", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	backfill := newDevelopmentBackfill(
		development, "127.0.0.1:8080", foreign.Binder())
	if backfill == nil {
		t.Fatal("the development backfill was refused on loopback")
	}
	opened, err := openService(context.Background(), serviceConfig{
		backend:  "embedded",
		data:     data,
		resolver: serving.Resolver(),
		clock:    time.Now,
		backfill: backfill,
	})
	if err == nil {
		opened.close()
		t.Fatal("the workspace opened despite a failed backfill")
	}
	if opened.service != nil {
		t.Fatal("a failed backfill still produced a service")
	}
	if !strings.Contains(err.Error(), "refusing to serve") {
		t.Fatalf("unclear diagnostic: %v", err)
	}
}

// mintSite is one reference to the capability constructor, with the function
// it appears in. Package scope is recorded as "package scope".
type mintSite struct {
	file     string
	function string
	called   bool
}

// TestBackfillCapabilityHasOneMintSite enforces, across every non-test .go
// file in the repository, that there is exactly one reference to
// devbackfill.NewCapability, that it is a call rather than a value taken, that
// it is in cmd/shoal-explore-web/backfill.go, and that it is inside
// (*developmentBackfill).run -- the body reached only after
// newDevelopmentBackfill has established the development authenticator and a
// loopback listener.
//
// Package scope is rejected specifically. A package-level var would mint a
// valid capability at process init, before any gate runs, in every build.
//
// Import forms are handled as follows. A qualified import is resolved to every
// local name the file binds it to -- a file may import the same path more than
// once, so all of the names are collected, not just the first. A dot import is
// rejected outright, because it puts NewCapability into file scope where an
// unqualified call would evade a scan for qualified references; nothing in
// this module has any reason to dot-import the package, so a flat prohibition
// is easier to keep true than a second scanner. A blank import is rejected for
// the same reason: it binds no name of its own, so on its own it cannot reach
// the constructor -- `_ "…/devbackfill"` plus an unqualified NewCapability()
// fails to compile with `undefined: NewCapability` -- but permitting it lets a
// file import the package twice and hide the usable name behind the unusable
// one.
//
// Test files are excluded: they legitimately mint capabilities, and they are
// not production reachability. Outside the module the barrier is the compiler
// -- the capability type cannot be named at all -- and this test is the
// in-module half of that.
//
// TEMPORARY (issue #284): delete with the backfill.
func TestBackfillCapabilityHasOneMintSite(t *testing.T) {
	t.Parallel()
	const (
		capabilityPackage = "github.com/phrocker/shoal-oss/internal/devbackfill"
		constructor       = "NewCapability"
		gateFunction      = "(*developmentBackfill).run"
	)
	gateFile := filepath.Join("cmd", "shoal-explore-web", "backfill.go")
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}

	var sites []mintSite
	var unqualified []string
	fileSet := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			// Every dot-directory is skipped, not just .git. A git worktree
			// created under .claude puts a second copy of this repository
			// inside the tree this walk covers, so the scan finds the same
			// mint site once per worktree and the count is whatever happens to
			// be checked out locally. Naming .git alone made the test pass or
			// fail on something no source change can affect.
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
		if len(locals) == 0 && !unnamed {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if unnamed {
			unqualified = append(unqualified, relative)
		}
		sites = append(sites, mintSites(parsed, relative, locals, constructor)...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(unqualified) != 0 {
		t.Fatalf(
			"%v dot- or blank-import %s, which this test refuses: a dot "+
				"import makes %s callable unqualified, and a blank import "+
				"can mask a second, usable import of the same path",
			unqualified, capabilityPackage, constructor)
	}
	if len(sites) != 1 {
		t.Fatalf(
			"the development backfill capability is minted %d times, want "+
				"exactly 1: %+v", len(sites), sites)
	}
	site := sites[0]
	if !site.called {
		t.Fatalf(
			"the capability constructor is referenced without calling it at "+
				"%s in %s, which can defer minting past the gate",
			site.file, site.function)
	}
	if site.file != gateFile || site.function != gateFunction {
		t.Fatalf(
			"the capability is minted at %s in %s, want %s in %s",
			site.file, site.function, gateFile, gateFunction)
	}
}

// importedNames returns every local name the file binds path to, and whether
// the file imports path in a form that binds no usable name of its own: a dot
// import, which puts the package's exported identifiers into file scope, or a
// blank import, which binds nothing. Both are reported so the caller can
// refuse them rather than scan a file whose references it cannot see.
//
// A file may import the same path more than once, so every spec is examined.
func importedNames(file *ast.File, path string) (locals []string, unnamed bool) {
	for _, spec := range file.Imports {
		value, err := strconv.Unquote(spec.Path.Value)
		if err != nil || value != path {
			continue
		}
		if spec.Name == nil {
			locals = append(locals, filepath.Base(path))
			continue
		}
		if spec.Name.Name == "_" || spec.Name.Name == "." {
			unnamed = true
			continue
		}
		locals = append(locals, spec.Name.Name)
	}
	return locals, unnamed
}

// mintSites reports every reference to constructor qualified by any of locals
// in the file, whether called or merely taken as a value, together with the
// enclosing function.
func mintSites(
	file *ast.File,
	relative string,
	locals []string,
	constructor string,
) []mintSite {
	qualifier := make(map[string]bool, len(locals))
	for _, local := range locals {
		qualifier[local] = true
	}
	var sites []mintSite
	record := func(function string, node ast.Node) {
		called := map[ast.Node]bool{}
		ast.Inspect(node, func(inner ast.Node) bool {
			call, ok := inner.(*ast.CallExpr)
			if ok {
				called[call.Fun] = true
			}
			selector, ok := inner.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != constructor {
				return true
			}
			ident, ok := selector.X.(*ast.Ident)
			if !ok || !qualifier[ident.Name] {
				return true
			}
			sites = append(sites, mintSite{
				file:     relative,
				function: function,
				called:   called[ast.Expr(selector)],
			})
			return true
		})
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok {
			// A var, const, or type declaration: minted at process init,
			// before any gate has run.
			record("package scope", declaration)
			continue
		}
		record(functionName(function), function)
	}
	return sites
}

// functionName renders a function as Name or (Receiver).Name.
func functionName(function *ast.FuncDecl) string {
	if function.Recv == nil || len(function.Recv.List) != 1 {
		return function.Name.Name
	}
	var receiver bytes.Buffer
	if err := printer.Fprint(
		&receiver, token.NewFileSet(), function.Recv.List[0].Type,
	); err != nil {
		return function.Name.Name
	}
	return "(" + receiver.String() + ")." + function.Name.Name
}

// preExistingCorpus writes one document straight to a corpus directory and
// closes it, standing in for content ingested before this process started.
func preExistingCorpus(t *testing.T) string {
	t.Helper()
	data := t.TempDir()
	corpus, err := explorer.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := corpus.Ingest(context.Background(), explorer.Source{
		URI:       "file:///pre-existing.md",
		MediaType: explorer.MediaTypeMarkdown,
		Content:   "# Pre-existing\n\nIngested before this process started.\n",
	}); err != nil {
		corpus.Close()
		t.Fatal(err)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}
	return data
}
