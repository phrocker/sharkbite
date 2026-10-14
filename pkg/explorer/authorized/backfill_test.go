/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package authorized_test

import (
	"context"
	"testing"

	"github.com/phrocker/shoal-oss/internal/devbackfill"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestBackfillRegistersUnregisteredBaseDocuments proves the repair for a
// corpus that outlived its in-memory policy catalog (issue #284) makes content
// visible only under the rule the trusted selector derives.
func TestBackfillRegistersUnregisteredBaseDocuments(t *testing.T) {
	f := newFixture(t)
	if _, err := f.base.Ingest(context.Background(), explorer.Source{
		URI:       "file:///pre-existing.txt",
		Title:     "Pre-existing",
		MediaType: explorer.MediaTypeText,
		Content:   "content written before the catalog existed",
	}); err != nil {
		t.Fatal(err)
	}
	summaries, err := f.clientA.Documents(f.alice(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 0 {
		t.Fatalf("unregistered document was visible: %d", len(summaries))
	}

	registered, err := f.clientA.BackfillExistingDocumentsForDevelopment(
		f.alice(t), devbackfill.NewCapability())
	if err != nil {
		t.Fatal(err)
	}
	if registered != 1 {
		t.Fatalf("registered = %d, want 1", registered)
	}
	summaries, err = f.clientA.Documents(f.alice(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 {
		t.Fatalf("backfilled document was not visible: %d", len(summaries))
	}
	view, err := f.clientA.Document(f.alice(t), summaries[0].Document.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if view.SourceURI != "file:///pre-existing.txt" {
		t.Fatalf("backfilled source URI = %q", view.SourceURI)
	}

	// The backfill grants exactly the selector's rule, so a decision outside
	// that rule still sees nothing.
	hidden, err := f.clientB.Documents(f.bob(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(hidden) != 0 {
		t.Fatalf("backfill widened visibility to another principal: %d", len(hidden))
	}

	// Re-running registers nothing further and leaves the catalog intact.
	again, err := f.clientA.BackfillExistingDocumentsForDevelopment(
		f.alice(t), devbackfill.NewCapability())
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Fatalf("second backfill registered = %d, want 0", again)
	}
}

// TestBackfillRequiresDevelopmentCapability proves the module-internal
// capability is load-bearing. Code outside this module cannot construct one at
// all, so nil is the only argument it could supply, and nil is refused before
// anything is resolved or registered.
func TestBackfillRequiresDevelopmentCapability(t *testing.T) {
	f := newFixture(t)
	if _, err := f.base.Ingest(context.Background(), explorer.Source{
		URI:       "file:///pre-existing.txt",
		Title:     "Pre-existing",
		MediaType: explorer.MediaTypeText,
		Content:   "content written before the catalog existed",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.clientA.BackfillExistingDocumentsForDevelopment(
		f.alice(t), nil); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("backfill without the capability error = %v", err)
	}
	summaries, err := f.clientA.Documents(f.alice(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 0 {
		t.Fatalf("a refused backfill still registered content: %d", len(summaries))
	}
}

// TestBackfillRefusesUndeclaredSelector proves the backfill will not guess.
// The source it can reconstruct for a stored document is lossy -- no content,
// no metadata, and the stored title rather than the submitted one -- so a
// selector that has not been shown to ignore the source could be handed input
// it never saw at ingest and derive a different, possibly wider, rule. Only a
// selector defined inside that package, which declares source independence, is
// accepted; every other one is refused and the corpus stays hidden.
func TestBackfillRefusesUndeclaredSelector(t *testing.T) {
	f := newFixture(t)
	if _, err := f.base.Ingest(context.Background(), explorer.Source{
		URI:       "file:///pre-existing.txt",
		Title:     "Pre-existing",
		MediaType: explorer.MediaTypeText,
		Content:   "content written before the catalog existed",
	}); err != nil {
		t.Fatal(err)
	}
	static, err := authorized.NewStaticPolicySelector(f.sourceA, f.policyA)
	if err != nil {
		t.Fatal(err)
	}
	// The wrapper forwards to a source-independent selector and so derives an
	// identical rule, but it is opaque to this package and is refused anyway.
	wrapped := authorized.PolicySelectorFunc(func(
		ctx context.Context,
		decision auth.Decision,
		source explorer.Source,
	) (auth.Policy, error) {
		return static.SelectPolicy(ctx, decision, source)
	})
	client, err := authorized.NewClient(authorized.Config{
		Base:               f.base,
		Resolver:           f.authority.Resolver(),
		PolicySelector:     wrapped,
		EdgePolicySelector: static,
		PolicyStore:        f.store,
		GenerationReader:   f.reader,
		Clock:              f.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.BackfillExistingDocumentsForDevelopment(
		f.alice(t),
		devbackfill.NewCapability(),
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("backfill with an undeclared selector error = %v", err)
	}
	summaries, err := f.clientA.Documents(f.alice(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 0 {
		t.Fatalf("a refused backfill still registered content: %d", len(summaries))
	}
}

// TestBackfillRequiresAuthorization proves the backfill is not a side door: it
// resolves and authorizes exactly like ingest and fails closed otherwise.
func TestBackfillRequiresAuthorization(t *testing.T) {
	f := newFixture(t)
	if _, err := f.base.Ingest(context.Background(), explorer.Source{
		URI:       "file:///pre-existing.txt",
		Title:     "Pre-existing",
		MediaType: explorer.MediaTypeText,
		Content:   "content written before the catalog existed",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := f.clientA.BackfillExistingDocumentsForDevelopment(
		context.Background(), devbackfill.NewCapability()); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("unbound context error = %v", err)
	}
	readOnly := f.context(t, f.decision(
		t,
		"reader",
		[][]byte{f.sourceA},
		[][]byte{f.policyA},
		[]auth.Operation{auth.OperationList, auth.OperationRead},
	))
	if _, err := f.clientA.BackfillExistingDocumentsForDevelopment(
		readOnly, devbackfill.NewCapability()); !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
		t.Fatalf("decision without ingest error = %v", err)
	}
	summaries, err := f.clientA.Documents(f.alice(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 0 {
		t.Fatalf("a refused backfill still registered content: %d", len(summaries))
	}
}
