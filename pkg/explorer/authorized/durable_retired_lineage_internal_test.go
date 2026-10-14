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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/cclient"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/iterrt"
	"github.com/phrocker/shoal-oss/pkg/explorer"
)

// testdata/lineage-catalog-667 is a durable policy catalog written by main at
// #667's merge (6142b897): two labelled documents, one narrowing completed
// (its lineage binding and closure rows) and one lagging behind a failed
// pre-commit audit (an open binding row), plus their intent and pre_commit
// records.
const (
	lineageFixtureCompleted = "file:///fixture/completed.txt"
	lineageFixtureLagging   = "file:///fixture/lagging.txt"
)

func copyLineageFixture(t *testing.T) string {
	t.Helper()
	source := filepath.Join("testdata", "lineage-catalog-667")
	target := t.TempDir()
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
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
	})
	if err != nil {
		t.Fatal(err)
	}
	return target
}

// retiredLineageRows returns the rows under the retired lineage prefix.
func retiredLineageRows(t *testing.T, dir string) [][]byte {
	t.Helper()
	eng, err := engine.Open(dir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	scanner, err := eng.Scan(policyTable, iterrt.InfiniteRange(), engine.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer scanner.Close()
	var rows [][]byte
	for scanner.Next() {
		row := append([]byte(nil), scanner.Key().Row...)
		if bytes.HasPrefix(row, []byte(retiredPolicyRowDeclassificationClaim)) &&
			(len(rows) == 0 || !bytes.Equal(rows[len(rows)-1], row)) {
			rows = append(rows, row)
		}
		if err := scanner.Advance(); err != nil {
			t.Fatal(err)
		}
	}
	return rows
}

// copyWithoutRetiredRows rebuilds the catalog at source into a fresh one,
// cell by cell, leaving out every row under the retired lineage prefix.
// (Engine deletes do not hide cells from the raw scan the loader uses.)
func copyWithoutRetiredRows(t *testing.T, source string) string {
	t.Helper()
	target := t.TempDir()
	created, err := OpenDurablePolicyStore(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
	from, err := engine.Open(source, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer from.Close()
	scanner, err := from.Scan(policyTable, iterrt.InfiniteRange(), engine.ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer scanner.Close()
	var mutations []*cclient.Mutation
	for scanner.Next() {
		key := scanner.Key()
		if !bytes.HasPrefix(key.Row, []byte(retiredPolicyRowDeclassificationClaim)) {
			mutation, err := cclient.NewMutation(append([]byte(nil), key.Row...))
			if err != nil {
				t.Fatal(err)
			}
			mutation.PutLatest(append([]byte(nil), key.ColumnFamily...),
				append([]byte(nil), key.ColumnQualifier...), nil,
				append([]byte(nil), scanner.Value()...))
			mutations = append(mutations, mutation)
		}
		if err := scanner.Advance(); err != nil {
			t.Fatal(err)
		}
	}
	to, err := engine.Open(target, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := to.Write(policyTable, mutations); err != nil {
		t.Fatal(err)
	}
	if err := to.Close(); err != nil {
		t.Fatal(err)
	}
	return target
}

type lineageCatalogView struct {
	Declassifications []string
	Claims            map[string]string
	Current           map[string]string
	Registrations     bool
}

func viewLineageCatalog(t *testing.T, store *DurablePolicyStore) lineageCatalogView {
	t.Helper()
	ctx := context.Background()
	view := lineageCatalogView{
		Claims:        map[string]string{},
		Current:       map[string]string{},
		Registrations: store.HasRegistrations(),
	}
	records, err := store.Declassifications(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		view.Declassifications = append(view.Declassifications,
			string(record.AttemptID)+"/"+string(record.Phase)+"/"+
				string(record.RevisionID))
	}
	for _, uri := range []string{lineageFixtureCompleted, lineageFixtureLagging} {
		claim, ok, err := store.SourceClaim(ctx, uri)
		if err != nil || !ok {
			t.Fatalf("claim %s: %v %v", uri, ok, err)
		}
		view.Claims[uri] = claim.Rule.String() + "/" +
			map[bool]string{true: "pending", false: "committed"}[claim.Pending]
		current, ok, err := store.CurrentRevision(ctx, explorer.SourceDocumentID(uri))
		if err != nil || !ok {
			t.Fatalf("registration %s: %v %v", uri, ok, err)
		}
		view.Current[uri] = string(current.RevisionID) + "/" + current.Rule.String()
	}
	return view
}

// TestRetiredLineageRowsAreSkippedOnLoad pins the compatibility promise for
// catalogs written by #667's lineage design: such a catalog opens and reads
// exactly like the same catalog with those rows removed, and stays writable.
func TestRetiredLineageRowsAreSkippedOnLoad(t *testing.T) {
	withRows := copyLineageFixture(t)
	retired := retiredLineageRows(t, withRows)
	if len(retired) < 3 {
		t.Fatalf("fixture holds %d retired lineage rows, want the binding, closure and open binding", len(retired))
	}
	withoutRows := copyWithoutRetiredRows(t, withRows)
	if remaining := retiredLineageRows(t, withoutRows); len(remaining) != 0 {
		t.Fatalf("rows survived deletion: %q", remaining)
	}

	with, err := OpenDurablePolicyStore(withRows)
	if err != nil {
		t.Fatalf("catalog with retired lineage rows does not open: %v", err)
	}
	t.Cleanup(func() { _ = with.Close() })
	without, err := OpenDurablePolicyStore(withoutRows)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = without.Close() })
	viewWith, viewWithout := viewLineageCatalog(t, with), viewLineageCatalog(t, without)
	if !reflect.DeepEqual(viewWith, viewWithout) {
		t.Fatalf("retired rows change what the catalog reads:\nwith    %+v\nwithout %+v",
			viewWith, viewWithout)
	}
	// As main left it: both claims committed under the narrow rule, and the
	// lagging document's registration still under the labelled one.
	if len(viewWith.Declassifications) != 3 || !viewWith.Registrations ||
		viewWith.Current[lineageFixtureLagging] == viewWith.Current[lineageFixtureCompleted] {
		t.Fatalf("fixture did not load as written: %+v", viewWith)
	}
	// A write after the skip still persists and survives a reopen.
	record := DeclassificationRecord{
		AttemptID: "after-skip", Phase: DeclassificationIntent,
		OccurredAt: time.Date(2026, time.August, 26, 18, 0, 0, 0, time.UTC),
		SourceURI:  lineageFixtureLagging,
	}
	claim, _, err := with.SourceClaim(context.Background(), lineageFixtureLagging)
	if err != nil {
		t.Fatal(err)
	}
	lagging, _, err := with.CurrentRevision(context.Background(),
		explorer.SourceDocumentID(lineageFixtureLagging))
	if err != nil {
		t.Fatal(err)
	}
	record.PriorRules = []AccessRule{lagging.Rule}
	record.Rule = claim.Rule
	if err := with.RecordDeclassification(context.Background(), record); err != nil {
		t.Fatalf("write after the skip: %v", err)
	}
	if err := with.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenDurablePolicyStore(withRows)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if records, err := reopened.Declassifications(context.Background()); err != nil || len(records) != 4 {
		t.Fatalf("records after a write and reopen = %d %v", len(records), err)
	}
}

// TestRetiredLineagePrefixStillFailsClosed pins that the skip is exact: a row
// under the retired prefix that is not a well-formed record of the retired
// kind still refuses to open the catalog, as any corrupt row does. Each case
// is stored under the row key its record would have, so only the check it
// names can reject it; a well-formed control row under the same scheme loads.
func TestRetiredLineagePrefixStillFailsClosed(t *testing.T) {
	rule := mustPersistRuleForTest(t)
	binding := func(uri string, version uint64, attempt string, closed bool) retiredPersistedDeclassificationClaim {
		return retiredPersistedDeclassificationClaim{
			Seq: 1, SourceURI: uri, Version: version, AttemptID: attempt, Closed: closed,
		}
	}
	rowFor := func(record retiredPersistedDeclassificationClaim) []byte {
		digest := sha256.Sum256([]byte(record.SourceURI))
		row := fmt.Sprintf("%s%s/%020d", retiredPolicyRowDeclassificationClaim,
			hex.EncodeToString(digest[:]), record.Version)
		if record.Closed {
			row += "/closed"
		}
		return []byte(row)
	}
	valid := binding("file:///x", 7, "a", false)
	for _, test := range []struct {
		name     string
		row      []byte
		value    []byte
		accepted bool
	}{
		{"well-formed control", rowFor(valid),
			mustEncodePolicyRecord(t, retiredPolicyKindDeclassificationClaim, valid), true},
		{"wrong kind", rowFor(valid),
			mustEncodePolicyRecord(t, policyKindDeclassification, valid), false},
		{"other record shape", rowFor(binding("", 0, "", false)),
			mustEncodePolicyRecord(t, retiredPolicyKindDeclassificationClaim,
				persistedRevision{Seq: 1, DocumentID: "d", RevisionID: "r", Rule: rule}), false},
		{"truncated", rowFor(valid), []byte("SHOALP1\x00"), false},
		{"row does not match the record", rowFor(binding("file:///other", 7, "a", false)),
			mustEncodePolicyRecord(t, retiredPolicyKindDeclassificationClaim, valid), false},
		{"version zero", rowFor(binding("file:///x", 0, "a", false)),
			mustEncodePolicyRecord(t, retiredPolicyKindDeclassificationClaim,
				binding("file:///x", 0, "a", false)), false},
		{"closure with an attempt", rowFor(binding("file:///x", 7, "a", true)),
			mustEncodePolicyRecord(t, retiredPolicyKindDeclassificationClaim,
				binding("file:///x", 7, "a", true)), false},
		{"binding without an attempt", rowFor(binding("file:///x", 7, "", false)),
			mustEncodePolicyRecord(t, retiredPolicyKindDeclassificationClaim,
				binding("file:///x", 7, "", false)), false},
		{"invalid source URI", rowFor(binding("", 7, "a", false)),
			mustEncodePolicyRecord(t, retiredPolicyKindDeclassificationClaim,
				binding("", 7, "a", false)), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := copyLineageFixture(t)
			corruptPolicyRow(t, dir, test.row, test.value)
			store, err := OpenDurablePolicyStore(dir)
			if err == nil {
				_ = store.Close()
			}
			if (err == nil) != test.accepted {
				t.Fatalf("open = %v, want accepted=%v", err, test.accepted)
			}
		})
	}
}

func mustEncodePolicyRecord(t *testing.T, kind byte, value any) []byte {
	t.Helper()
	encoded, err := encodePolicyRecord(kind, value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
