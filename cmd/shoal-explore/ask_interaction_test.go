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

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/interaction"
)

// TestAskRejectsReadOnlyCorpusAtSetup pins binding decision 4: a corpus that
// cannot record the interaction refuses ask outright, with a clear diagnostic,
// before any retrieval or model work happens.
func TestAskRejectsReadOnlyCorpusAtSetup(t *testing.T) {
	data := ingestAskFixture(t)
	var output bytes.Buffer
	err := run(context.Background(), []string{
		"ask", "-data", data, "-read-only",
		"-provider", "fake",
		"-question", "What keeps grounded answers tied to exact quotes?",
	}, &output)
	if err == nil {
		t.Fatal("ask succeeded on a read-only corpus")
	}
	message := err.Error()
	for _, want := range []string{
		"writable interaction sink", "read-only",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("diagnostic %q does not mention %q", message, want)
		}
	}
	if output.Len() != 0 {
		t.Fatalf("read-only ask produced output: %s", output.String())
	}

	corpus, err := explorer.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = corpus.Close() })
	sessions, err := corpus.Interactions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 0 {
		t.Fatalf("refused ask still recorded %d interactions", len(sessions))
	}
}

// TestAskRecordsInteractionInTheGraph pins that the production ask path uses a
// real graph-backed recorder rather than discarding the execution record, and
// that what it records stays inside the reserved namespace.
func TestAskRecordsInteractionInTheGraph(t *testing.T) {
	ctx := context.Background()
	data := ingestAskFixture(t)
	var output bytes.Buffer
	if err := run(ctx, []string{
		"ask", "-data", data,
		"-provider", "fake",
		"-question", "What keeps grounded answers tied to exact quotes?",
	}, &output); err != nil {
		t.Fatal(err)
	}

	corpus, err := explorer.Open(data)
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	sessions, err := corpus.Interactions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 {
		t.Fatalf("recorded interactions = %d, want 1", len(sessions))
	}
	summary := sessions[0]
	if summary.Deleted || summary.NodeCount == 0 || summary.EdgeCount == 0 {
		t.Fatalf("interaction summary = %+v", summary)
	}

	sub, err := corpus.InteractionSubgraph(ctx, summary.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, node := range sub.Nodes {
		if !interaction.IsInteractionKind(node.Kind) {
			t.Fatalf("recorded a non-interaction node: %+v", node)
		}
		kinds[node.Kind]++
	}
	if kinds[interaction.KindSession] != 1 || kinds[interaction.KindTurn] == 0 {
		t.Fatalf("recorded node kinds = %v", kinds)
	}
	var retrieved, cited int
	for _, edge := range sub.Edges {
		switch edge.Type {
		case interaction.EdgeRetrieved:
			retrieved++
		case interaction.EdgeCited:
			cited++
		}
	}
	if retrieved == 0 || cited == 0 {
		t.Fatalf("retrieved edges = %d, cited edges = %d", retrieved, cited)
	}
	if err := corpus.Close(); err != nil {
		t.Fatal(err)
	}

	// The recorded interaction must not resurface as source evidence.
	var queryOutput bytes.Buffer
	if err := run(ctx, []string{
		"query", "-data", data, "-text", "grounded answers",
	}, &queryOutput); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(queryOutput.String(), interaction.KindPrefix) {
		t.Fatalf("query output leaked interaction nodes: %s", queryOutput.String())
	}
}
