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
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// seedProvenanceCorpus ingests one source and records two sessions over its
// spans, returning the data directory and the span the sessions shared.
func seedProvenanceCorpus(t *testing.T) (string, shoal.ID) {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "guide.md")
	if err := os.WriteFile(source, []byte(
		"# Guide\n\nUse exponential backoff for retries.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "data")
	var output bytes.Buffer
	if err := run(context.Background(), []string{
		"ingest", "-data", data, "-file", source,
	}, &output); err != nil {
		t.Fatal(err)
	}

	corpus, err := explorer.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	documents, err := corpus.Documents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	view, err := corpus.Neighborhood(
		context.Background(), explorer.NeighborhoodRequest{
			NodeIDs: []shoal.ID{documents[0].Document.ID},
			Depth:   16,
		})
	if err != nil {
		t.Fatal(err)
	}
	var span shoal.ID
	for _, node := range view.Nodes {
		if node.Kind == "span" {
			span = node.ID
			break
		}
	}
	if span == "" {
		t.Fatal("ingest produced no span node")
	}
	for index, id := range []shoal.ID{"interaction.session_cli-one", "interaction.session_cli-two"} {
		if err := corpus.RecordInteraction(
			context.Background(), interaction.Session{
				ID:           id,
				RecordedAt:   time.Unix(1700000000+int64(index), 0).UTC(),
				SeedNodeIDs:  []shoal.ID{span},
				CitedNodeIDs: []shoal.ID{span},
			}); err != nil {
			t.Fatal(err)
		}
	}
	return data, span
}

// TestFoldAndProvenanceCommands exercises the operator-facing traversal
// surface: fold, unfold, and cross-session provenance walks.
func TestFoldAndProvenanceCommands(t *testing.T) {
	data, span := seedProvenanceCorpus(t)
	ctx := context.Background()

	var output bytes.Buffer
	if err := run(ctx, []string{
		"fold", "-data", data,
		"-session", "interaction.session_cli-one", "-session", "interaction.session_cli-two",
		"-summary-digest", interaction.Digest("a summary held out of band"),
	}, &output); err != nil {
		t.Fatal(err)
	}
	var result explorer.FoldResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("fold output = %s: %v", output.String(), err)
	}
	if !result.Created || result.MemberCount != 2 {
		t.Fatalf("fold result = %+v", result)
	}
	if !strings.HasPrefix(string(result.FoldID), interaction.KindFold+"_") {
		t.Fatalf("fold ID %q is outside the reserved namespace", result.FoldID)
	}

	output.Reset()
	if err := run(ctx, []string{
		"unfold", "-data", data, "-fold", string(result.FoldID),
	}, &output); err != nil {
		t.Fatal(err)
	}
	var fold interaction.Fold
	if err := json.Unmarshal(output.Bytes(), &fold); err != nil {
		t.Fatalf("unfold output = %s: %v", output.String(), err)
	}
	if len(fold.Members) != 2 {
		t.Fatalf("unfold returned %d members, want 2", len(fold.Members))
	}
	for _, member := range fold.Members {
		if len(member.RetrievedNodeIDs) != 1 || len(member.CitedNodeIDs) != 1 {
			t.Fatalf("unfold lost provenance for %s: %+v",
				member.SessionID, member)
		}
	}

	output.Reset()
	if err := run(ctx, []string{
		"provenance", "-data", data, "-node", string(span),
	}, &output); err != nil {
		t.Fatal(err)
	}
	var touches []explorer.InteractionTouch
	if err := json.Unmarshal(output.Bytes(), &touches); err != nil {
		t.Fatalf("provenance output = %s: %v", output.String(), err)
	}
	if len(touches) != 3 {
		t.Fatalf("provenance walked %d interactions, want 3: %+v",
			len(touches), touches)
	}

	output.Reset()
	if err := run(ctx, []string{
		"provenance", "-data", data, "-interaction", "interaction.session_cli-one",
	}, &output); err != nil {
		t.Fatal(err)
	}
	var overlaps []explorer.InteractionOverlap
	if err := json.Unmarshal(output.Bytes(), &overlaps); err != nil {
		t.Fatalf("overlap output = %s: %v", output.String(), err)
	}
	if len(overlaps) != 2 {
		t.Fatalf("cross-session walk found %d interactions, want 2: %+v",
			len(overlaps), overlaps)
	}
}

// TestProvenanceRefusesDerivedNode pins that the operator surface will not
// walk provenance from a derived node as though it were source evidence.
func TestProvenanceRefusesDerivedNode(t *testing.T) {
	data, _ := seedProvenanceCorpus(t)
	ctx := context.Background()

	var output bytes.Buffer
	if err := run(ctx, []string{
		"fold", "-data", data, "-session", "interaction.session_cli-one",
	}, &output); err != nil {
		t.Fatal(err)
	}
	var result explorer.FoldResult
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := run(ctx, []string{
		"provenance", "-data", data, "-node", string(result.FoldID),
	}, &output); err == nil {
		t.Fatal("expected walking from a fold node to be refused")
	}
	if err := run(ctx, []string{
		"provenance", "-data", data,
		"-node", "span-x", "-interaction", "interaction.session_cli-one",
	}, &output); err == nil {
		t.Fatal("expected -node with -interaction to be refused")
	}
	if err := run(ctx, []string{"fold", "-data", data}, &output); err == nil {
		t.Fatal("expected fold with no sessions to be refused")
	}
}
