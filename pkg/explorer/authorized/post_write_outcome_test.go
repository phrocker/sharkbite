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

// Every error an authorized ingest returns once its base may have written is
// indeterminate (#687): the revision may be applied with the claim pending,
// or registered. The marker only adds; codes and identities are kept. An
// error before any write, or after a base write that changed nothing, stays
// definite.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

var errInjectedRead = errors.New("injected read failure")

// readAfterIngestBase fails the next Documents read after an Ingest it
// passed through, when armed.
type readAfterIngestBase struct {
	explorer.Client
	mu      sync.Mutex
	armed   bool
	pending bool
}

func (b *readAfterIngestBase) Ingest(
	ctx context.Context, source explorer.Source,
) (explorer.IngestResult, error) {
	result, err := b.Client.Ingest(ctx, source)
	b.mu.Lock()
	b.pending = b.armed && err == nil
	b.mu.Unlock()
	return result, err
}

func (b *readAfterIngestBase) Documents(ctx context.Context) ([]explorer.DocumentSummary, error) {
	b.mu.Lock()
	fail := b.pending
	b.pending = false
	b.mu.Unlock()
	if fail {
		return nil, shoal.WrapError(shoal.ErrorUnavailable, "injected", errInjectedRead)
	}
	return b.Client.Documents(ctx)
}

// failingRegistrationStore fails PutRevision when armed.
type failingRegistrationStore struct {
	authorized.PolicyStore
	armed bool
}

func (s *failingRegistrationStore) PutRevision(
	ctx context.Context, registration authorized.RevisionRegistration,
) error {
	if s.armed {
		return shoal.NewError(shoal.ErrorUnavailable, "injected registration failure")
	}
	return s.PolicyStore.PutRevision(ctx, registration)
}

func TestIngestFailureAfterAWriteIsIndeterminate(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, inner authorized.PolicyStore) {
		f := newFixture(t)
		base := &readAfterIngestBase{Client: f.base}
		store := &failingRegistrationStore{PolicyStore: inner}
		client := f.newClient(t, base, store, f.sourceA, f.policyA, nil)
		owner := f.labelIngester(t, "owner")

		// The read after an applied write fails: indeterminate, with the
		// base's code and identity.
		base.armed = true
		_, err := client.Ingest(owner, labelledSource("file:///post/read.txt", "read", ""))
		base.armed = false
		if !explorer.IsIndeterminateCommit(err) ||
			!shoal.IsErrorCode(err, shoal.ErrorUnavailable) || !errors.Is(err, errInjectedRead) {
			t.Fatalf("read failure after an applied write = %v, want it indeterminate", err)
		}

		// The registration fails: indeterminate.
		store.armed = true
		_, err = client.Ingest(owner, labelledSource("file:///post/register.txt", "register", ""))
		store.armed = false
		if !explorer.IsIndeterminateCommit(err) || !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
			t.Fatalf("registration failure = %v, want it indeterminate", err)
		}

		// A read failure after an unchanged base result wrote nothing: the
		// claim rolls back, so it stays definite.
		if _, err := client.Ingest(owner, labelledSource("file:///post/same.txt", "same", "")); err != nil {
			t.Fatal(err)
		}
		base.armed = true
		_, err = client.Ingest(owner, labelledSource("file:///post/same.txt", "same", ""))
		base.armed = false
		if err == nil || explorer.IsIndeterminateCommit(err) || !errors.Is(err, errInjectedRead) {
			t.Fatalf("read failure after an unchanged result = %v, want it definite", err)
		}

		// An unchanged result re-registers its revision, so a failed
		// registration there is indeterminate too: it may have committed.
		store.armed = true
		_, err = client.Ingest(owner, labelledSource("file:///post/same.txt", "same", ""))
		store.armed = false
		if !explorer.IsIndeterminateCommit(err) || !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
			t.Fatalf("re-registration failure after an unchanged result = %v, want it indeterminate", err)
		}

		// A refusal before any write stays definite.
		_, err = client.Ingest(f.labelIngester(t, "outsider-without-ingest"),
			labelledSource("file:///post/same.txt", "other", "secret"))
		if err == nil || explorer.IsIndeterminateCommit(err) {
			t.Fatalf("pre-write refusal = %v, want it definite", err)
		}
	})
}

// TestDeclassificationCommitFailureIsIndeterminate pins the pre-commit audit
// path (declassificationCommit, where a commit-time reason refusal is also
// returned): it runs after the base applied the narrowing, so its failure is
// indeterminate and keeps its code.
func TestDeclassificationCommitFailureIsIndeterminate(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, inner authorized.PolicyStore) {
		f := newFixture(t)
		store := &failingDeclassificationStore{
			PolicyStore: inner, fail: map[int]bool{2: true}}
		client := f.declassificationClient(t, store, false)
		const uri = "file:///post/declassify.txt"
		owner := f.labelIngester(t, "owner", "secret")
		if _, err := client.Ingest(owner, labelledSource(uri, "secret", "secret")); err != nil {
			t.Fatal(err)
		}
		_, err := client.Ingest(owner, labelledSource(uri, "public", ""))
		if !explorer.IsIndeterminateCommit(err) || !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
			t.Fatalf("pre-commit audit failure = %v, want it indeterminate", err)
		}
	})
}
