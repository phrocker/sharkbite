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

package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized/authorizedtest"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	mcpLabelledSession shoal.ID = "interaction.session_mcp_labelled"
	mcpPlainSession    shoal.ID = "interaction.session_mcp_plain"
)

func mcpLabelID(id shoal.ID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id))
}

// mcpLabelServer is a real MCP server whose provenance tools are backed by the
// real authorized client, acting as subject.
func mcpLabelServer(
	t *testing.T, f *authorizedtest.Fixture, subject string,
) *Server {
	t.Helper()
	service, err := webapi.NewEmbeddedService(f.Client)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := webapi.NewInteractionService(f.Client)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := NewInteractionTools(provider)
	if err != nil {
		t.Fatal(err)
	}
	template := f.Decision(t, subject)
	server, err := newRecordedTestServer(t, Config{
		Service: service, Authority: f.Authority,
		Decisions: DecisionProviderFunc(func(context.Context) (auth.Decision, error) {
			return template, nil
		}),
		Snapshots: f.Client, OptionalTools: tools,
		requestIDFactory: sequentialRequestIDs(),
	})
	if err != nil {
		t.Fatal(err)
	}
	makeReady(t, server)
	return server
}

// mcpLabelCall returns the tool's structured content bytes.
func mcpLabelCall(t *testing.T, server *Server, name, arguments string) []byte {
	t.Helper()
	response := callToolRequest(t, server, name, arguments)
	result := decodeToolResult(t, response)
	if response.Error != nil || result.IsError {
		t.Fatalf("%s failed: %+v / %s", name, response.Error, result.StructuredContent)
	}
	return result.StructuredContent
}

// mcpLabelOutcome is a tool call's whole outcome, error or not, as bytes, so
// a refusal can be compared with the one a never-written identifier gets.
func mcpLabelOutcome(t *testing.T, server *Server, name, arguments string) []byte {
	t.Helper()
	response := callToolRequest(t, server, name, arguments)
	result := decodeToolResult(t, response)
	encoded, err := json.Marshal(struct {
		Error  any
		Result any
	}{response.Error, result})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// TestProvenanceToolsServeLabelledRecordsOnlyToHolders: the MCP provenance
// list, inspect and unfold tools serve a reader holding the labels the
// labelled records with their labels, and serve one that does not exactly
// what it would receive had they never been written (#564, #567, #568).
func TestProvenanceToolsServeLabelledRecordsOnlyToHolders(t *testing.T) {
	f := authorizedtest.New(t)
	f.Record(t, authorizedtest.HolderSubject, mcpLabelledSession,
		authorizedtest.SecretLabels)
	f.Record(t, authorizedtest.HolderSubject, mcpPlainSession, nil)
	labelledFold := f.Fold(t, authorizedtest.HolderSubject, mcpLabelledSession)
	plainFold := f.Fold(t, authorizedtest.HolderSubject, mcpPlainSession)
	outsider := mcpLabelServer(t, f, authorizedtest.OutsiderSubject)
	holder := mcpLabelServer(t, f, authorizedtest.HolderSubject)
	const never shoal.ID = "interaction.session_mcp_never"
	holderSees := func(name string, body []byte) {
		t.Helper()
		if !bytes.Contains(body, []byte(authorizedtest.SecretLabels[0])) {
			t.Fatalf("%s: the holder lost the labels: %s", name, body)
		}
	}

	// List: the outsider's page is the holder's without the labelled items.
	var outsiderPage, holderPage webapi.ProvenanceListResponse
	listed := mcpLabelCall(t, outsider, ToolProvenanceList, `{}`)
	if leaked := authorizedtest.Leaks(listed); len(leaked) != 0 {
		t.Fatalf("list leaked %v: %s", leaked, listed)
	}
	holderListed := mcpLabelCall(t, holder, ToolProvenanceList, `{}`)
	holderSees("list", holderListed)
	if err := json.Unmarshal(listed, &outsiderPage); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(holderListed, &holderPage); err != nil {
		t.Fatal(err)
	}
	if len(holderPage.Interactions) != 2 || len(holderPage.Folds) != 2 {
		t.Fatalf("the holder lost records: %s", holderListed)
	}
	want := holderPage
	want.Interactions, want.Folds = nil, nil
	for _, item := range holderPage.Interactions {
		if item.SessionID == mcpLabelID(mcpPlainSession) {
			want.Interactions = append(want.Interactions, item)
		}
	}
	for _, item := range holderPage.Folds {
		if item.FoldID == mcpLabelID(plainFold.FoldID) {
			want.Folds = append(want.Folds, item)
		}
	}
	wantBytes, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	gotBytes, err := json.Marshal(outsiderPage)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBytes, wantBytes) {
		t.Fatalf("outsider list:\n%s\nwant, as if never written,\n%s", gotBytes, wantBytes)
	}

	// Inspect and unfold: the refusal of a never-written identifier.
	inspect := func(id shoal.ID) string {
		return `{"session_id":"` + mcpLabelID(id) + `"}`
	}
	unfold := func(id shoal.ID) string {
		return `{"fold_id":"` + mcpLabelID(id) + `"}`
	}
	for _, probe := range []struct{ tool, labelled, never string }{
		{ToolProvenanceInspect, inspect(mcpLabelledSession), inspect(never)},
		{ToolProvenanceUnfold, unfold(labelledFold.FoldID), unfold(never)},
	} {
		got := mcpLabelOutcome(t, outsider, probe.tool, probe.labelled)
		absent := mcpLabelOutcome(t, outsider, probe.tool, probe.never)
		if !bytes.Equal(got, absent) {
			t.Fatalf("%s: an outsider can tell the record exists:\n%s\nnever written:\n%s",
				probe.tool, got, absent)
		}
		holderSees(probe.tool, mcpLabelCall(t, holder, probe.tool, probe.labelled))
	}
}
