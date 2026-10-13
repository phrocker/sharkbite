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

package authorized

import (
	"context"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// engineBreakingBase closes the durable catalog's engine right after the
// armed base write, so the registration that follows fails to persist.
type engineBreakingBase struct {
	explorer.Client
	store *DurablePolicyStore
	armed bool
}

func (b *engineBreakingBase) Ingest(
	ctx context.Context, source explorer.Source,
) (explorer.IngestResult, error) {
	result, err := b.Client.Ingest(ctx, source)
	if err == nil && b.armed {
		b.armed = false
		_ = b.store.engine.Close()
	}
	return result, err
}

// TestIngestOnADivergingCatalogRecoversPending runs a relabel whose
// registration fails to persist on a durable catalog. The store diverges, so
// even the claim's finalize cannot be persisted. After a reopen, the claim the
// CAS persisted comes back pending with the replaced rule, and the
// registration is the labelled one, so finishing the relabel still needs the
// replaced rule.
func TestIngestOnADivergingCatalogRecoversPending(t *testing.T) {
	now := time.Date(2026, time.September, 2, 18, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	store, err := OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	explorerBase, err := explorer.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = explorerBase.Close() })
	base := &engineBreakingBase{Client: explorerBase, store: store}
	secretPolicy, err := LabelPolicyID([]byte("source"), "secret")
	if err != nil {
		t.Fatal(err)
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "subject", Actor: "actor",
		AuthorizationDomain: []byte("domain"),
		AllowedOperations: []auth.Operation{
			auth.OperationIngest, auth.OperationList, auth.OperationRead,
		},
		PermittedSourceIDs:    [][]byte{[]byte("source")},
		PermittedPolicyIDs:    [][]byte{[]byte("policy"), secretPolicy},
		PolicyGeneration:      1,
		AuthenticationExpires: now.Add(time.Hour),
		RequestID:             "request",
	})
	if err != nil {
		t.Fatal(err)
	}
	selector, err := NewStaticPolicySelector([]byte("source"), []byte("policy"))
	if err != nil {
		t.Fatal(err)
	}
	newClient := func(policyStore PolicyStore) *Client {
		client, err := NewClient(Config{
			Base: base,
			Resolver: resolverFunc(func(context.Context) (auth.Decision, error) {
				return decision, nil
			}),
			PolicySelector: selector,
			PolicyStore:    policyStore,
			GenerationReader: generationReaderFunc(
				func(context.Context, []byte) (int64, error) { return 1, nil }),
			Clock: func() time.Time { return now },
		})
		if err != nil {
			t.Fatal(err)
		}
		return client
	}
	const uri = "file:///diverging/relabel.txt"
	ctx := context.Background()
	first, err := newClient(store).Ingest(ctx, explorer.Source{
		URI: uri, MediaType: explorer.MediaTypeText, Content: "secret",
		Metadata: shoal.Metadata{interaction.PropertyVisibility: "secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	base.armed = true
	if _, err := newClient(store).Ingest(ctx, explorer.Source{
		URI: uri, MediaType: explorer.MediaTypeText, Content: "public",
	}); !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("relabel on a diverging catalog = %v, want unavailable", err)
	}
	if !store.Diverged() {
		t.Fatal("the store did not diverge")
	}
	_ = store.lock.Close()
	store.closed = true

	reopened, err := OpenDurablePolicyStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	claim, ok, err := reopened.SourceClaim(ctx, uri)
	if err != nil || !ok || !claim.Pending || claim.PreviousRule == nil {
		t.Fatalf("reopened claim = %+v %v %v, want pending with the replaced rule", claim, ok, err)
	}
	current, ok, err := reopened.CurrentRevision(ctx, first.Document.ID)
	if err != nil || !ok || current.RevisionID != first.Revision.ID ||
		!current.Rule.equal(*claim.PreviousRule) {
		t.Fatalf("reopened registration = %+v %v %v, want the labelled revision", current, ok, err)
	}
}
