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
	"encoding/base64"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/explorerfleet"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const reasonTestAuditPurpose = "apply reviewed fleet policy"

var (
	digest3c = "atpl:policy:v1:" + strings.Repeat("3c", 32)
	digestAb = "atpl:policy:v1:" + strings.Repeat("ab", 32)
)

// hostedRegistry is the fleet registry HTTP route over the service exactly as
// openService composes it: authorized client, embedded corpus, real
// lifecycle recorder.
type hostedRegistry struct {
	t         *testing.T
	root      string
	now       time.Time
	authority *auth.Authority
	opened    openedService
	handler   http.Handler
}

func openHostedRegistry(
	t *testing.T, root string, now time.Time,
) *hostedRegistry {
	t.Helper()
	clock := func() time.Time { return now }
	authority, err := auth.NewAuthorityWithClock(clock)
	if err != nil {
		t.Fatal(err)
	}
	h := &hostedRegistry{t: t, root: root, now: now, authority: authority}
	h.open()
	return h
}

func (h *hostedRegistry) config() serviceConfig {
	return serviceConfig{
		backend: "embedded", data: filepath.Join(h.root, "corpus"),
		policyDir: filepath.Join(h.root, "policy"),
		resolver:  h.authority.Resolver(),
		clock:     func() time.Time { return h.now },
		executors: configuredFleetExecutors{
			"local": configuredFleetExecutor{reference: "local"},
		},
	}
}

func (h *hostedRegistry) open() {
	h.t.Helper()
	opened, err := openService(context.Background(), h.config())
	if err != nil {
		h.t.Fatal(err)
	}
	handler, err := webapi.NewFleetHandler(
		opened.fleetRegistry, opened.fleetDispatch)
	if err != nil {
		opened.close()
		h.t.Fatal(err)
	}
	h.opened, h.handler = opened, handler
}

func (h *hostedRegistry) close() { h.opened.close() }

func (h *hostedRegistry) restart() {
	h.close()
	h.open()
}

func (h *hostedRegistry) register(
	requestID, key, agentID shoal.ID, detail string,
) *httptest.ResponseRecorder {
	h.t.Helper()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "owner", Actor: "operator", ClientID: "shoalctl",
		AuthorizationDomain:   workspaceAuthorizationDomain,
		AllowedOperations:     []auth.Operation{auth.OperationAgentRegister},
		PermittedSourceIDs:    [][]byte{workspaceSourceID},
		PermittedPolicyIDs:    [][]byte{workspaceGrantPolicyID},
		PolicyGeneration:      workspacePolicyGeneration,
		AuthenticationExpires: h.now.Add(time.Hour),
		AuditPurpose:          reasonTestAuditPurpose,
		RequestID:             requestID,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	ctx, err := h.authority.Binder().Bind(context.Background(), decision)
	if err != nil {
		h.t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{
		"context": map[string]any{
			"request_id":    encodeTestID(requestID),
			"reason_code":   fleet.ReasonCodeATPLApply,
			"reason_detail": detail,
			"deadline":      h.now.Add(time.Minute),
		},
		"registration_key":    encodeTestID(key),
		"expected_generation": 0,
		"descriptor": map[string]any{
			"id":                   encodeTestID(agentID),
			"authorization_domain": workspaceAuthorizationDomain,
			"scopes": []fleet.Scope{{
				SourceID: workspaceSourceID, PolicyID: workspaceGrantPolicyID,
			}},
			"executor_ref": "local",
			"capabilities": []fleet.Capability{{
				Name: "documents",
				Actions: []fleet.Action{{
					Name:         "list",
					InputSchema:  json.RawMessage(`{"type":"object"}`),
					OutputSchema: json.RawMessage(`{"type":"object"}`),
				}},
			}},
			"lease_expires_at": h.now.Add(10 * time.Minute),
		},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	request := httptest.NewRequest(
		http.MethodPost, "/api/v1/fleet/agents", bytes.NewReader(body),
	).WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	h.handler.ServeHTTP(response, request)
	return response
}

func (h *hostedRegistry) expectStatus(
	response *httptest.ResponseRecorder, status int, what string,
) {
	h.t.Helper()
	if response.Code != status {
		h.close()
		h.t.Fatalf("%s: status %d, want %d: %s",
			what, response.Code, status, response.Body.String())
	}
}

// receipts closes the service and reads every durable receipt from disk.
func (h *hostedRegistry) receipts() (
	map[shoal.ID]interaction.Session, func(),
) {
	h.t.Helper()
	h.close()
	corpus, err := explorer.Open(filepath.Join(h.root, "corpus"))
	if err != nil {
		h.t.Fatal(err)
	}
	summaries, err := corpus.Interactions(context.Background())
	if err != nil {
		corpus.Close()
		h.t.Fatal(err)
	}
	result := make(map[shoal.ID]interaction.Session, len(summaries))
	for _, summary := range summaries {
		session, err := corpus.Interaction(
			context.Background(), summary.SessionID)
		if err != nil {
			corpus.Close()
			h.t.Fatal(err)
		}
		result[summary.SessionID] = session
	}
	return result, func() { corpus.Close() }
}

func registerReceiptID(requestID, agentID shoal.ID) shoal.ID {
	return explorerfleet.LifecycleReceiptID(
		auth.OperationAgentRegister, requestID, agentID)
}

// TestHostedATPLRegistrationRecordsAssertedPolicyDigestDurably drives the
// registry HTTP route against the real recorder, restarts, and reads the
// receipt back from disk.
func TestHostedATPLRegistrationRecordsAssertedPolicyDigestDurably(t *testing.T) {
	h := openHostedRegistry(t, t.TempDir(), time.Now().UTC().Add(time.Minute))
	const agentID = shoal.ID("policy-agent")
	h.expectStatus(h.register("atpl-malformed-request", "atpl-key", agentID,
		"atpl:policy:v1:NOT-A-DIGEST"), http.StatusBadRequest, "malformed digest")
	h.expectStatus(h.register("atpl-apply-request", "atpl-key", agentID,
		digest3c), http.StatusCreated, "policy registration")

	// Restart on the same state: the registration and its receipt survive.
	h.restart()
	resolveDecision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "owner", Actor: "operator",
		AuthorizationDomain:   workspaceAuthorizationDomain,
		AllowedOperations:     []auth.Operation{auth.OperationAgentResolve},
		PermittedSourceIDs:    [][]byte{workspaceSourceID},
		PermittedPolicyIDs:    [][]byte{workspaceGrantPolicyID},
		PolicyGeneration:      workspacePolicyGeneration,
		AuthenticationExpires: h.now.Add(time.Hour),
		RequestID:             "atpl-resolve-request",
	})
	if err != nil {
		h.close()
		t.Fatal(err)
	}
	resolveContext, err := h.authority.Binder().Bind(
		context.Background(), resolveDecision)
	if err != nil {
		h.close()
		t.Fatal(err)
	}
	resolved, err := h.opened.fleetRegistry.Resolve(resolveContext,
		fleet.ResolveRequest{
			Context: fleet.RequestContext{
				RequestID: "atpl-resolve-request", ReasonCode: "atpl-read",
				Deadline: h.now.Add(time.Minute),
			},
			ID: agentID,
		})
	if err != nil || resolved.Descriptor.Generation != 1 {
		h.close()
		t.Fatalf("restarted resolve = %#v, %v", resolved.Descriptor, err)
	}

	receipts, done := h.receipts()
	defer done()
	receipt, ok := receipts[registerReceiptID("atpl-apply-request", agentID)]
	if !ok {
		t.Fatalf("durable receipt missing: %#v", receipts)
	}
	if receipt.CallerAssertedReason != (interaction.CallerAssertedReason{
		Code: fleet.ReasonCodeATPLApply, Source: digest3c,
	}) {
		t.Fatalf("caller-asserted reason = %#v", receipt.CallerAssertedReason)
	}
	wantPurpose, err := interaction.NewReason(
		"audit_purpose", reasonTestAuditPurpose)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Actor.SubjectID != "owner" || receipt.Actor.ActorID != "operator" ||
		receipt.Actor.ClientID != "shoalctl" || receipt.Reason != wantPurpose ||
		receipt.ResultID != agentID ||
		receipt.AuthorizationOperation != string(auth.OperationAgentRegister) {
		t.Fatalf("durable receipt = %#v", receipt)
	}
	// The refused request wrote nothing: only the registration and the
	// restart's resolve are recorded.
	if _, ok := receipts[registerReceiptID("atpl-malformed-request", agentID)]; ok ||
		len(receipts) != 2 {
		t.Fatalf("lifecycle receipts = %#v", receipts)
	}
	for _, session := range receipts {
		if strings.Contains(session.CallerAssertedReason.Source, "NOT-A-DIGEST") {
			t.Fatalf("malformed digest persisted: %#v", session)
		}
	}
}

// TestHostedRegisterReplayChecksAssertedReason is the regression for a
// register replay bypassing the receipt: a same-key replay used to return the
// stored registration before the recorder ran, so a divergent asserted reason
// succeeded and a new request ID left no receipt.
func TestHostedRegisterReplayChecksAssertedReason(t *testing.T) {
	h := openHostedRegistry(t, t.TempDir(), time.Now().UTC().Add(time.Minute))
	const agentID = shoal.ID("replay-agent")
	h.expectStatus(h.register("replay-request", "replay-key", agentID, digest3c),
		http.StatusCreated, "registration")
	h.expectStatus(h.register("replay-request", "replay-key", agentID, digest3c),
		http.StatusCreated, "exact replay")
	h.expectStatus(h.register("replay-request", "replay-key", agentID, digestAb),
		http.StatusConflict, "same-request replay asserting another digest")
	h.expectStatus(h.register("replay-request-2", "replay-key", agentID, digestAb),
		http.StatusCreated, "new-request replay")
	h.expectStatus(h.register("replay-request-3", "replay-key", agentID,
		"atpl:policy:v1:short"), http.StatusBadRequest, "malformed replay")

	receipts, done := h.receipts()
	defer done()
	original := receipts[registerReceiptID("replay-request", agentID)]
	replayed := receipts[registerReceiptID("replay-request-2", agentID)]
	if original.CallerAssertedReason.Source != digest3c {
		t.Fatalf("original receipt = %#v", original.CallerAssertedReason)
	}
	// The new request has its own receipt, over the identical mutation,
	// recording its own assertion; the admitting receipt is untouched.
	if replayed.CallerAssertedReason.Source != digestAb ||
		replayed.RecordedAt.Before(original.RecordedAt) ||
		replayed.ResultID != original.ResultID {
		t.Fatalf("replay receipt = %#v", replayed)
	}
	if _, ok := receipts[registerReceiptID("replay-request-3", agentID)]; ok ||
		len(receipts) != 2 {
		t.Fatalf("lifecycle receipts = %#v", receipts)
	}
}

// TestPre477LifecycleReceiptReconcilesAcrossUpgrade opens state written by the
// code before #477 (testdata/pre477-lifecycle, see its README) with this code.
func TestPre477LifecycleReceiptReconcilesAcrossUpgrade(t *testing.T) {
	root := t.TempDir()
	copyTestTree(t, filepath.Join("testdata", "pre477-lifecycle"), root)
	h := openHostedRegistry(t, root, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC))
	const (
		agentID  = shoal.ID("pre477-agent")
		key      = shoal.ID("pre477-key")
		previous = shoal.ID("pre477-apply-request")
	)
	// The same request retried after the upgrade reconciles with its v2
	// receipt, which an old build wrote with a v1 registry digest. Because the
	// v1 digest is not injective (#521), the reconciled retry also writes the
	// current (v4) receipt, recording the mutation by its v2 digest and the
	// retry's assertion.
	h.expectStatus(h.register(previous, key, agentID, digest3c),
		http.StatusCreated, "pre-#477 retry")
	// A v2 receipt never recorded the assertion, but the v4 receipt the first
	// retry wrote did, so a later retry asserting a different digest conflicts.
	h.expectStatus(h.register(previous, key, agentID, digestAb),
		http.StatusConflict, "pre-#477 retry asserting another digest")
	h.expectStatus(h.register(previous, key, agentID, digest3c),
		http.StatusCreated, "pre-#477 retry repeated")
	// A post-#477 request gets a current receipt, which binds its assertion.
	h.expectStatus(h.register("post477-request", key, agentID, digest3c),
		http.StatusCreated, "post-#477 replay")
	h.expectStatus(h.register("post477-request", key, agentID, digestAb),
		http.StatusConflict, "post-#477 retry asserting another digest")

	receipts, done := h.receipts()
	defer done()
	current, ok := receipts[registerReceiptID(previous, agentID)]
	if !ok || current.CallerAssertedReason.Source != digest3c {
		t.Fatalf("pre-#477 retry did not write the current receipt: %#v", current)
	}
	if len(receipts) != 3 {
		t.Fatalf("lifecycle receipts = %#v", receipts)
	}
	var legacy interaction.Session
	for id, session := range receipts {
		if id != registerReceiptID("post477-request", agentID) &&
			id != registerReceiptID(previous, agentID) {
			legacy = session
		}
	}
	if legacy.RequestID != previous || !legacy.CallerAssertedReason.IsZero() {
		t.Fatalf("pre-#477 receipt = %#v", legacy)
	}
	post := receipts[registerReceiptID("post477-request", agentID)]
	if post.CallerAssertedReason.Source != digest3c ||
		post.ResultID != legacy.ResultID {
		t.Fatalf("post-#477 receipt = %#v", post)
	}
}

func copyTestTree(t *testing.T, source, destination string) {
	t.Helper()
	err := filepath.WalkDir(source, func(
		path string, entry fs.DirEntry, err error,
	) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if relative == "README" || strings.HasSuffix(relative, ".txt") {
			return nil
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
}

func encodeTestID(id shoal.ID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id))
}
