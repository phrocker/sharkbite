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
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSkillsOntologyFileLoads(t *testing.T) {
	version, err := loadOntologyVersionFile(
		filepath.Join("..", "..", "docs", "skills-ontology.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(version.Concepts()) != 3 || len(version.Relationships()) != 3 {
		t.Fatalf("skills ontology shape = %d concepts, %d relationships; want 3 and 3",
			len(version.Concepts()), len(version.Relationships()))
	}
}

func TestOntologyFileConfiguresWebWorkspaceEndpoint(t *testing.T) {
	data := t.TempDir()
	ontologyPath := writeWorkspaceOntologyFile(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := &lockedBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{
			"-data", data,
			"-listen", "127.0.0.1:0",
			"-dev-auth",
			"-ontology-file", ontologyPath,
		}, output)
	}()

	baseURL := waitForListeningURL(t, output)
	getMeta(t, baseURL)
	body := getOntologyJSON(t, baseURL+"/api/v1/ontology")
	var ontology map[string]any
	if err := json.Unmarshal(body, &ontology); err != nil {
		t.Fatalf("decode ontology %s: %v", string(body), err)
	}
	if configured, _ := ontology["configured"].(bool); !configured {
		t.Fatalf("ontology endpoint was not configured by -ontology-file: %s", string(body))
	}
	if concepts, ok := ontology["concepts"].([]any); !ok || len(concepts) != 2 {
		t.Fatalf("ontology concepts were not served from -ontology-file: %s", string(body))
	}
	if relationships, ok := ontology["relationships"].([]any); !ok || len(relationships) != 1 {
		t.Fatalf("ontology relationships were not served from -ontology-file: %s", string(body))
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down")
	}
}

func TestOntologyProposalLifecycleUsesStartedEmbeddedWorkspace(t *testing.T) {
	data := t.TempDir()
	ontologyPath := writeWorkspaceOntologyFile(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := &lockedBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{
			"-data", data,
			"-listen", "127.0.0.1:0",
			"-dev-auth",
			"-ontology-file", ontologyPath,
		}, output)
	}()
	baseURL := waitForListeningURL(t, output)
	getMeta(t, baseURL)

	created := postOntologyProposalJSON(t, baseURL, "/api/v1/ontology/proposals", []byte(`{
  "rationale": "exercise startup proposal wiring",
  "proposed_version": {
    "version": "v2",
    "properties": [
      {"key": "name", "name": "Name", "value_type": "string"},
      {"key": "role", "name": "Role", "value_type": "string"}
    ],
    "concepts": [
      {"key": "person", "name": "Person", "properties": ["name"]},
      {"key": "organization", "name": "Organization", "properties": ["name"]}
    ],
    "relationships": [
      {
        "key": "member_of",
        "name": "Member of",
        "from_concepts": ["person"],
        "to_concepts": ["organization"],
        "properties": ["role"],
        "directed": true
      }
    ]
  }
}`))
	proposal, ok := created["proposal"].(map[string]any)
	if !ok || proposal["state"] != "draft" {
		t.Fatalf("started workspace proposal creation = %v", created)
	}
	proposalID, _ := proposal["id"].(string)
	submitted := postOntologyProposalJSON(
		t, baseURL, "/api/v1/ontology/proposals/"+proposalID+"/transition",
		[]byte(`{"state":"submitted","note":"ready for review"}`),
	)
	updated, ok := submitted["proposal"].(map[string]any)
	if !ok || updated["state"] != "submitted" {
		t.Fatalf("started workspace proposal transition = %v", submitted)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down")
	}
}

func TestOntologyProposalEditorDraftPreservesStartedEmbeddedWorkspaceOntology(t *testing.T) {
	data := t.TempDir()
	ontologyPath := writeWorkspaceOntologyFile(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := &lockedBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{
			"-data", data,
			"-listen", "127.0.0.1:0",
			"-dev-auth",
			"-ontology-file", ontologyPath,
		}, output)
	}()
	baseURL := waitForListeningURL(t, output)
	getMeta(t, baseURL)

	body := getOntologyJSON(t, baseURL+"/api/v1/ontology")
	var active map[string]any
	if err := json.Unmarshal(body, &active); err != nil {
		t.Fatalf("decode active ontology %s: %v", body, err)
	}
	draft := editorDraftFromActiveOntology(t, active)
	concepts := draft["concepts"].([]any)
	concepts[0].(map[string]any)["name"] = "Human"
	request, err := json.Marshal(map[string]any{
		"rationale":        "editor changed one field without removing elements",
		"proposed_version": draft,
	})
	if err != nil {
		t.Fatal(err)
	}
	created := postOntologyProposalJSON(
		t, baseURL, "/api/v1/ontology/proposals", request)
	proposal, ok := created["proposal"].(map[string]any)
	if !ok || proposal["state"] != "draft" {
		t.Fatalf("started workspace editor proposal creation = %v", created)
	}
	proposed, _ := proposal["proposed_ontology"].(map[string]any)
	if got := len(proposed["concepts"].([]any)); got != 2 {
		t.Fatalf("editor-shaped proposal lost concepts: %v", proposed["concepts"])
	}
	if got := len(proposed["relationships"].([]any)); got != 1 {
		t.Fatalf("editor-shaped proposal lost relationships: %v", proposed["relationships"])
	}
	if got := len(proposed["properties"].([]any)); got != 2 {
		t.Fatalf("editor-shaped proposal lost properties: %v", proposed["properties"])
	}
	if proposed["concepts"].([]any)[0].(map[string]any)["name"] != "Human" {
		t.Fatalf("editor-shaped proposal did not apply the field edit: %v", proposed["concepts"])
	}

	proposalID, _ := proposal["id"].(string)
	blastBody := getOntologyJSON(
		t, baseURL+"/api/v1/ontology/proposals/"+proposalID+"/blast-radius")
	var blastEnvelope map[string]any
	if err := json.Unmarshal(blastBody, &blastEnvelope); err != nil {
		t.Fatalf("decode blast radius %s: %v", blastBody, err)
	}
	blast := blastEnvelope["blast_radius"].(map[string]any)
	summary := blast["summary"].(map[string]any)
	for _, field := range []string{"removed_concepts", "removed_relationships", "removed_properties"} {
		if summary[field] != float64(0) {
			t.Fatalf("editor-shaped one-field proposal reported %s: %s", field, blastBody)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down")
	}
}

func TestOntologyProposalBlastRadiusUsesStartedEmbeddedWorkspace(t *testing.T) {
	data := t.TempDir()
	ontologyPath := writeWorkspaceOntologyFile(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	output := &lockedBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{
			"-data", data,
			"-listen", "127.0.0.1:0",
			"-dev-auth",
			"-ontology-file", ontologyPath,
		}, output)
	}()
	baseURL := waitForListeningURL(t, output)
	getMeta(t, baseURL)

	created := postOntologyProposalJSON(t, baseURL, "/api/v1/ontology/proposals", []byte(`{
  "rationale": "exercise startup blast radius wiring",
  "proposed_version": {
    "version": "v2",
    "properties": [
      {"key": "name", "name": "Name", "value_type": "string"}
    ],
    "concepts": [
      {"key": "person", "name": "Person", "properties": ["name"]}
    ],
    "relationships": []
  }
}`))
	proposal, ok := created["proposal"].(map[string]any)
	if !ok || proposal["state"] != "draft" {
		t.Fatalf("started workspace proposal creation = %v", created)
	}
	proposalID, _ := proposal["id"].(string)
	body := getOntologyJSON(
		t, baseURL+"/api/v1/ontology/proposals/"+proposalID+"/blast-radius")
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode blast radius %s: %v", body, err)
	}
	blast, ok := decoded["blast_radius"].(map[string]any)
	if !ok {
		t.Fatalf("started workspace blast radius response = %s", body)
	}
	removed, ok := blast["removed_relationships"].([]any)
	if !ok || len(removed) != 1 {
		t.Fatalf("started workspace blast radius did not report removed relationship: %s", body)
	}
	summary, _ := blast["summary"].(map[string]any)
	if _, exists := summary["counts_computed"]; exists {
		t.Fatalf("started workspace blast radius exposed unimplemented counts: %v", summary)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not shut down")
	}
}

func writeWorkspaceOntologyFile(t *testing.T) string {
	t.Helper()
	ontologyPath := filepath.Join(t.TempDir(), "ontology.json")
	if err := os.WriteFile(ontologyPath, []byte(`{
  "schema": {"key": "workspace", "name": "Workspace"},
  "version": {"version": "v1", "created_at": "2026-09-04T00:00:00Z"},
  "properties": [
    {"key": "name", "name": "Name", "value_type": "string", "constraints": [
      {"kind": "required"}
    ]},
    {"key": "role", "name": "Role", "value_type": "string"}
  ],
  "concepts": [
    {"key": "person", "name": "Person", "properties": ["name"]},
    {"key": "organization", "name": "Organization", "properties": ["name"]}
  ],
  "relationships": [
    {
      "key": "member_of",
      "name": "Member of",
      "from_concepts": ["person"],
      "to_concepts": ["organization"],
      "properties": ["role"],
      "directed": true
    }
  ]
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return ontologyPath
}

func editorDraftFromActiveOntology(t *testing.T, active map[string]any) map[string]any {
	t.Helper()
	propertyKeys := map[string]string{}
	for _, item := range active["properties"].([]any) {
		property := item.(map[string]any)
		propertyKeys[property["id"].(string)] = property["key"].(string)
	}
	conceptKeys := map[string]string{}
	for _, item := range active["concepts"].([]any) {
		concept := item.(map[string]any)
		conceptKeys[concept["id"].(string)] = concept["key"].(string)
	}
	version := active["version"].(map[string]any)
	draft := map[string]any{
		"version":       version["version"].(string) + "-editor",
		"properties":    []any{},
		"concepts":      []any{},
		"relationships": []any{},
	}
	for _, item := range active["properties"].([]any) {
		property := item.(map[string]any)
		draft["properties"] = append(draft["properties"].([]any), map[string]any{
			"key":         property["key"],
			"name":        property["name"],
			"description": property["description"],
			"value_type":  property["value_type"],
			"constraints": property["constraints"],
		})
	}
	for _, item := range active["concepts"].([]any) {
		concept := item.(map[string]any)
		draft["concepts"] = append(draft["concepts"].([]any), map[string]any{
			"key":         concept["key"],
			"name":        concept["name"],
			"description": concept["description"],
			"properties":  remapOntologyKeys(concept["properties"].([]any), propertyKeys),
		})
	}
	for _, item := range active["relationships"].([]any) {
		relationship := item.(map[string]any)
		draft["relationships"] = append(draft["relationships"].([]any), map[string]any{
			"key":           relationship["key"],
			"name":          relationship["name"],
			"description":   relationship["description"],
			"from_concepts": remapOntologyKeys(relationship["from_concepts"].([]any), conceptKeys),
			"to_concepts":   remapOntologyKeys(relationship["to_concepts"].([]any), conceptKeys),
			"properties":    remapOntologyKeys(relationship["properties"].([]any), propertyKeys),
			"directed":      relationship["directed"],
		})
	}
	return draft
}

func remapOntologyKeys(ids []any, keys map[string]string) []string {
	remapped := make([]string, 0, len(ids))
	for _, id := range ids {
		value := id.(string)
		if key, ok := keys[value]; ok {
			remapped = append(remapped, key)
		} else {
			remapped = append(remapped, value)
		}
	}
	return remapped
}

func getOntologyJSON(t *testing.T, url string) []byte {
	t.Helper()
	client := http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s returned %s: %s", url, response.Status, data)
	}
	return data
}

func postOntologyProposalJSON(
	t *testing.T,
	baseURL string,
	path string,
	body []byte,
) map[string]any {
	t.Helper()
	client := http.Client{Timeout: 5 * time.Second}
	response, err := client.Post(
		baseURL+path, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusCreated && response.StatusCode != http.StatusOK {
		t.Fatalf("POST %s returned %s: %s", path, response.Status, data)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode %s response %s: %v", path, data, err)
	}
	return decoded
}
