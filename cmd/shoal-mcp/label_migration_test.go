// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
)

// TestOpenWorkspaceRunsTheLabelMigration pins that shoal-mcp, which also
// serves the authorized store, runs the startup label migration (#570)
// before serving, records its marker, and does not run it again.
func TestOpenWorkspaceRunsTheLabelMigration(t *testing.T) {
	root := t.TempDir()
	identity, err := configureIdentity(identityOptions{
		development:      true,
		policyGeneration: 1,
		lifetime:         time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	policyDir := filepath.Join(root, "policy")
	open := func() string {
		var diagnostics bytes.Buffer
		_, _, closeWorkspace, err := openWorkspace(
			context.Background(),
			commandConfig{
				corpusDir:   filepath.Join(root, "corpus"),
				policyDir:   policyDir,
				identity:    identity,
				diagnostics: &diagnostics,
			},
			auth.NewAuthority(),
			time.Now,
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := closeWorkspace(); err != nil {
			t.Fatal(err)
		}
		return diagnostics.String()
	}
	// Nothing to narrow, so nothing is printed: stdio launchers may treat
	// stderr output as noise.
	if first := open(); first != "" {
		t.Fatalf("first start diagnostics = %q", first)
	}
	store, err := authorized.OpenDurablePolicyStore(policyDir)
	if err != nil {
		t.Fatal(err)
	}
	record, ok, err := store.LabelMigration(context.Background())
	if closeErr := store.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || !ok || record.Version != authorized.LabelMigrationVersion {
		t.Fatalf("marker = %+v, %v, %v", record, ok, err)
	}
	if second := open(); second != "" {
		t.Fatalf("restart diagnostics = %q", second)
	}
}
