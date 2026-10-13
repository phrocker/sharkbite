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
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const lineageFixtureLaggingURI = "file:///fixture/lagging.txt"

func openLineageFixture(t *testing.T) *authorized.DurablePolicyStore {
	t.Helper()
	source := filepath.Join("testdata", "lineage-catalog-667")
	target := t.TempDir()
	if err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		destination := filepath.Join(target, relative)
		if entry.IsDir() {
			return os.MkdirAll(destination, 0o755)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(destination, content, 0o644)
	}); err != nil {
		t.Fatal(err)
	}
	store, err := authorized.OpenDurablePolicyStore(target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// TestLaggingNarrowingFromLineageCatalog drives ingests against the catalog
// #667 left behind (testdata/lineage-catalog-667). There, a narrowing's claim
// is committed under the narrow rule while its registration still carries
// the labelled one. That state used to be finished by any holder of the
// narrow rule. Now only a holder of the registration's rule finishes it, as
// an ordinary audited narrowing under the reason policy. Everyone else is
// refused as absent, and writes neither the base nor the claim.
func TestLaggingNarrowingFromLineageCatalog(t *testing.T) {
	for _, test := range []struct {
		name   string
		labels []string
		source explorer.Source
	}{
		{"narrow-rule holder, same content", nil, labelledSource(lineageFixtureLaggingURI, "public", "")},
		{"narrow-rule holder, other content", nil, labelledSource(lineageFixtureLaggingURI, "other", "")},
		{"holder of another label", []string{"x"}, labelledSource(lineageFixtureLaggingURI, "x", "x")},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			store := openLineageFixture(t)
			claim, _, err := store.SourceClaim(context.Background(), lineageFixtureLaggingURI)
			if err != nil {
				t.Fatal(err)
			}
			client := f.newClient(t, f.base, store, f.sourceA, f.policyA, nil)
			if _, err := client.Ingest(f.labelIngester(t, "reader", test.labels...), test.source); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
				t.Fatalf("ingest = %v, want not found", err)
			}
			if documents, err := f.base.Documents(context.Background()); err != nil || len(documents) != 0 {
				t.Fatalf("a refused principal wrote the base: %+v %v", documents, err)
			}
			after, _, err := store.SourceClaim(context.Background(), lineageFixtureLaggingURI)
			if err != nil || after.Version != claim.Version || after.Pending != claim.Pending {
				t.Fatalf("a refused principal changed the claim: %+v %v", after, err)
			}
		})
	}

	t.Run("registration holder, reason policy", func(t *testing.T) {
		f := newFixture(t)
		store := openLineageFixture(t)
		before, err := store.Declassifications(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		strict := f.declassificationClient(t, store, true)
		public := labelledSource(lineageFixtureLaggingURI, "public", "")
		if _, err := strict.Ingest(f.labelIngester(t, "owner", "secret"), public); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
			t.Fatalf("finishing without a reason = %v, want invalid argument", err)
		}
		if _, err := strict.Ingest(withReason(t, f.labelIngester(t, "owner", "secret"), "finish"), public); err != nil {
			t.Fatalf("finishing with a reason: %v", err)
		}
		after, err := store.Declassifications(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != len(before)+2 ||
			after[len(before)].Phase != authorized.DeclassificationIntent ||
			after[len(before)+1].Phase != authorized.DeclassificationPreCommit ||
			after[len(before)].ReasonCode != "finish" {
			t.Fatalf("finishing was not audited as an intent and a pre-commit: %+v", after[len(before):])
		}
	})
}
