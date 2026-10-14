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
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/workspace"
	"github.com/phrocker/shoal-oss/pkg/ontology"
)

func TestOpenServiceWiresDurableWorkspaceSettings(t *testing.T) {
	now := time.Date(2026, 9, 5, 18, 0, 0, 0, time.UTC)
	root := t.TempDir()
	corpus := filepath.Join(root, "corpus")
	policy := filepath.Join(root, "policy")
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "owner", Actor: "actor",
		AuthorizationDomain: workspaceAuthorizationDomain,
		AllowedOperations: []auth.Operation{
			auth.OperationRead,
			auth.OperationWorkspaceSettingsRead,
			auth.OperationWorkspaceSettingsWrite,
		},
		PermittedSourceIDs:    [][]byte{workspaceSourceID},
		PermittedPolicyIDs:    [][]byte{workspaceGrantPolicyID},
		PolicyGeneration:      workspacePolicyGeneration,
		AuthenticationExpires: now.Add(time.Hour),
		RequestID:             "request",
	})
	if err != nil {
		t.Fatal(err)
	}
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	adminContext, err := authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := ontology.NewOntologySchema(
		"workspace", "Workspace", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	version, err := ontology.NewOntologyVersion(
		schema, "1", now, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	config := serviceConfig{
		backend: "embedded", data: corpus, policyDir: policy,
		resolver: authority.Resolver(), clock: func() time.Time { return now },
		ontology: &version,
	}
	opened, err := openService(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	if opened.settings == nil {
		t.Fatal("embedded startup did not expose workspace settings")
	}
	topK := uint32(5)
	created, err := opened.settings.Update(
		adminContext, "started-workspace", workspace.UpdateRequest{
			MutationID: "started-mutation",
			Narrowing: workspace.UpdateNarrowing{
				Budgets: workspace.Budgets{RetrievalTopK: &topK},
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := ontology.NewOntologyIdentity(version)
	if err != nil {
		t.Fatal(err)
	}
	created, err = opened.settings.SelectOntology(
		adminContext, created.WorkspaceID, created.Revision,
		"select-ontology", identity)
	if err != nil {
		t.Fatal(err)
	}
	narrowDecision, err := auth.NewDecision(auth.DecisionConfig{
		Subject:               "owner",
		Actor:                 "actor",
		AuthorizationDomain:   workspaceAuthorizationDomain,
		AllowedOperations:     []auth.Operation{auth.OperationRetrieve},
		PermittedSourceIDs:    [][]byte{workspaceSourceID},
		PermittedPolicyIDs:    [][]byte{workspaceGrantPolicyID},
		PolicyGeneration:      workspacePolicyGeneration,
		AuthenticationExpires: now.Add(time.Hour),
		RequestID:             "narrow-request",
	})
	if err != nil {
		t.Fatal(err)
	}
	narrowContext, err := authority.Binder().Bind(
		context.Background(), narrowDecision)
	if err != nil {
		t.Fatal(err)
	}
	effective, err := opened.settings.ApplyForOperation(
		narrowContext, created.WorkspaceID,
		auth.OperationRetrieve, workspace.MaximumLimits(), nil)
	if err != nil {
		t.Fatalf("apply selected lens for narrow service operation: %v", err)
	}
	selected, selectedSet := effective.Decision().SelectedOntology()
	if !selectedSet || selected != identity {
		t.Fatalf("effective selected ontology = %#v, %v", selected, selectedSet)
	}
	opened.close()
	if _, err := os.Stat(filepath.Join(root, "settings")); !os.IsNotExist(err) {
		t.Fatalf("settings opened a separate engine directory: %v", err)
	}

	reopened, err := openService(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.close()
	loaded, err := reopened.settings.Get(
		adminContext, "started-workspace")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Revision != created.Revision ||
		loaded.SettingsID != created.SettingsID ||
		loaded.Narrowing.Budgets.RetrievalTopK == nil ||
		*loaded.Narrowing.Budgets.RetrievalTopK != topK ||
		!loaded.Narrowing.SelectedOntology.Present ||
		loaded.Narrowing.SelectedOntology.Identity != identity {
		t.Fatalf("restarted settings = %#v", loaded)
	}
}
