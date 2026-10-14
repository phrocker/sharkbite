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
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
)

// withholdingDeployment starts the real composition with the concealment
// option set as a deployment would set it, ingests one document, and returns a
// principal that is authorized to act but holds no grant for that document.
// Its reads are therefore withheld, which is the only state in which the
// counts are observable at all.
func withholdingDeployment(
	t *testing.T, conceal bool,
) (webapi.Service, context.Context, func()) {
	t.Helper()
	root := t.TempDir()
	now := time.Now().UTC().Add(time.Minute)
	authority := auth.NewAuthority()
	opened, err := openService(context.Background(), serviceConfig{
		backend: "embedded", data: filepath.Join(root, "corpus"),
		policyDir: filepath.Join(root, "policy"),
		resolver:  authority.Resolver(), clock: func() time.Time { return now },
		concealWithholding: conceal,
	})
	if err != nil {
		t.Fatal(err)
	}
	if opened.client == nil {
		opened.close()
		t.Skip("embedded service exposes no authorized client")
	}
	ingestCtx, err := authority.Binder().Bind(context.Background(), askDecision(
		t, now, "wiring-ingest",
		auth.OperationIngest, auth.OperationRead, auth.OperationList))
	if err != nil {
		opened.close()
		t.Fatal(err)
	}
	if _, err := opened.client.Ingest(ingestCtx, explorer.Source{
		URI: "shoal://test/withheld.md", Title: "Withheld",
		MediaType: "text/markdown", Content: "# Withheld\n\nrestricted body\n",
	}); err != nil {
		opened.close()
		t.Fatalf("ingest: %v", err)
	}
	// Same domain and operations, but a grant for a source that owns nothing.
	// The ingested document is then withheld from this principal by
	// authorization rather than by absence.
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "outsider", Actor: "outsider",
		AuthorizationDomain: workspaceAuthorizationDomain,
		AllowedOperations: []auth.Operation{
			auth.OperationList, auth.OperationRead, auth.OperationRetrieve,
		},
		PermittedSourceIDs:    [][]byte{[]byte("shoal-explore-web/other")},
		PermittedPolicyIDs:    [][]byte{[]byte("shoal-explore-web/other-grant")},
		PolicyGeneration:      workspacePolicyGeneration,
		AuthenticationExpires: now.Add(time.Hour),
		RequestID:             "outsider-request",
		CorrelationID:         "outsider-correlation",
	})
	if err != nil {
		opened.close()
		t.Fatal(err)
	}
	ctx, err := authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		opened.close()
		t.Fatal(err)
	}
	return opened.service, ctx, opened.close
}

// TestConcealWithholdingReachesTheDeployedService proves the option survives
// composition. Every other test for it calls ConcealWithholding directly, so a
// regression in openService could leave the advertised deployment option
// ineffective while those stay green.
//
// The default case is asserted in the same test rather than separately,
// because the option is only meaningful as a difference: a concealed response
// that happens to match an unconfigured one proves nothing.
func TestConcealWithholdingReachesTheDeployedService(t *testing.T) {
	disclosing, disclosingCtx, closeDisclosing := withholdingDeployment(t, false)
	defer closeDisclosing()
	disclosed, err := disclosing.Documents(
		disclosingCtx, webapi.DocumentsRequest{})
	if err != nil {
		t.Fatalf("documents with disclosure: %v", err)
	}
	if disclosed.Suppressed == 0 {
		t.Fatal("the fixture must actually withhold something, " +
			"or concealment cannot be observed")
	}
	if len(disclosed.Documents) != 0 {
		t.Fatalf("the outsider must read nothing: %#v", disclosed.Documents)
	}

	concealing, concealingCtx, closeConcealing := withholdingDeployment(t, true)
	defer closeConcealing()
	concealed, err := concealing.Documents(
		concealingCtx, webapi.DocumentsRequest{})
	if err != nil {
		t.Fatalf("documents with concealment: %v", err)
	}
	if concealed.Suppressed != 0 || concealed.Restricted != 0 {
		t.Fatalf("-conceal-withholding did not reach the deployed service: %#v",
			concealed)
	}
	if len(concealed.Documents) != 0 {
		t.Fatalf("concealment changed what the outsider reads: %#v",
			concealed.Documents)
	}
}

// TestConcealWithholdingEnvironmentDefault covers the environment half of the
// wiring. Only the documented value enables the option, so a typo cannot
// silently switch a security control on or off.
func TestConcealWithholdingEnvironmentDefault(t *testing.T) {
	for value, want := range map[string]bool{
		"":      false,
		"1":     true,
		"0":     false,
		"true":  false,
		"yes":   false,
		" 1":    false,
		"1 ":    false,
		"TRUE":  false,
		"enabl": false,
	} {
		t.Run("env="+value, func(t *testing.T) {
			t.Setenv("SHOAL_CONCEAL_WITHHOLDING", value)
			if got := concealWithholdingDefault(); got != want {
				t.Fatalf("SHOAL_CONCEAL_WITHHOLDING=%q resolved to %v, want %v",
					value, got, want)
			}
		})
	}
}

// TestConcealWithholdingFlagIsRegistered proves the flag reaches the real
// parser. A flag that is documented but never defined fails here rather than
// at a deployment that quietly ignores it.
func TestConcealWithholdingFlagIsRegistered(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// The context is already cancelled, so the command shuts down as soon as
	// it has parsed. An undefined flag fails before that with a parse error.
	err := run(ctx, []string{
		"-data", t.TempDir(), "-listen", "127.0.0.1:0", "-dev-auth",
		"-conceal-withholding",
	}, &lockedBuffer{})
	if err != nil && strings.Contains(err.Error(), "not defined") {
		t.Fatalf("-conceal-withholding is not registered: %v", err)
	}
}

// A fully end-to-end HTTP test of this flag is not achievable under -dev-auth,
// and the attempt is recorded here rather than shipped as a test that passes
// without exercising anything.
//
// The counts are only observable when something is withheld, and under
// -dev-auth nothing ever is: developmentBackfill exists specifically to grant
// the documents already on disk to the development principal, so a corpus
// served under an empty policy catalog is repaired at startup rather than
// withheld. The mosaic route is closed too, because the command uses a single
// static policy selector and therefore has one sensitivity domain, which no
// co-occurrence budget can restrict. Producing a withheld response over HTTP
// would require real OIDC authentication with a narrowed principal.
//
// What remains uncovered is one assignment, from the parsed flag into
// serviceConfig. TestConcealWithholdingFlagIsRegistered covers the parse,
// TestConcealWithholdingReachesTheDeployedService covers everything after the
// config field, and neither sees that line. Closing it honestly needs either
// non-development authentication in the test, or extracting the flag-to-config
// mapping so both halves meet; it is not closed by a test that cannot fail.
