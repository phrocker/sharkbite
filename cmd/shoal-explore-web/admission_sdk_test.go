// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	admissionapi "github.com/phrocker/shoal-oss/pkg/admission/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/sdk"
)

// admissionCaller is the principal an out-of-process gateway authenticates as:
// it may invoke, and it may retrieve, because the disclosure restrictor charges
// a declared reference exactly as a read of it would.
var admissionCaller = principal{
	subject: "alice", actor: "alice-gateway",
	operations: []auth.Operation{
		auth.OperationDispatch, auth.OperationInvoke, auth.OperationRetrieve,
	},
}

// sdkAdmission serves the real admission handler over the composition
// openService builds, behind a bearer check standing in for the explorer's
// authenticator, and returns the public SDK client pointed at it. Every 200
// body is kept so a test can compare wire shapes byte for byte.
type sdkAdmission struct {
	client *sdk.Client
	mu     sync.Mutex
	bodies map[string][][]byte
}

func newSDKAdmission(t *testing.T, h *approvalHarness, who principal) *sdkAdmission {
	t.Helper()
	handler, err := webapi.NewAdmissionHandler(h.opened.admission)
	if err != nil {
		t.Fatal(err)
	}
	result := &sdkAdmission{bodies: map[string][][]byte{}}
	server := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer admission-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, r.WithContext(h.as(who)))
			result.mu.Lock()
			result.bodies[r.URL.Path] = append(
				result.bodies[r.URL.Path], recorder.Body.Bytes())
			result.mu.Unlock()
			for name, values := range recorder.Header() {
				w.Header()[name] = values
			}
			w.WriteHeader(recorder.Code)
			_, _ = w.Write(recorder.Body.Bytes())
		}))
	t.Cleanup(server.Close)
	result.client, err = sdk.New(sdk.Config{
		BaseURL: server.URL, HTTPClient: server.Client(),
		Token: func(context.Context) (string, error) { return "admission-token", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func (s *sdkAdmission) lastBody(route string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	bodies := s.bodies[route]
	return bodies[len(bodies)-1]
}

func sdkAdmissionRequest(
	h *approvalHarness, id, action string, effects []string, disclosures []string,
) admissionapi.Request {
	return admissionapi.Request{
		Context: admissionapi.RequestContext{
			RequestID: admissionapi.EncodeID([]byte("body-request")), ReasonCode: "test",
			Deadline: h.now().Add(time.Hour),
		},
		ID:              admissionapi.EncodeID([]byte(id)),
		IdempotencyKey:  admissionapi.EncodeID([]byte("key-" + id)),
		TokenID:         admissionapi.EncodeID([]byte("token-" + id)),
		AgentID:         admissionapi.EncodeID([]byte("gateway")),
		AgentGeneration: h.generationOf("gateway"),
		Capability:      "ops", Action: action,
		SourceID: workspaceSourceID, PolicyID: workspaceGrantPolicyID,
		ObjectID: admissionapi.EncodeID([]byte("release-7")),
		Effects:  effects, Input: json.RawMessage(`{"version":"7"}`),
		Disclosures: disclosures, Lease: time.Minute,
	}
}

// newAdmissionHarness is newApprovalHarness's composition with two changes,
// both needed for an allowance to be observable at all: the "local" reference
// is bound as -fleet-external-executor-refs=local binds it, with an
// external-mutation ceiling (the approval harness leaves it a placeholder with
// no ceiling, under which every admission is denied), and the agent's actions
// declare the external effect they are admitted for.
func newAdmissionHarness(t *testing.T) *approvalHarness {
	t.Helper()
	h := &approvalHarness{t: t, root: t.TempDir()}
	h.clock.Store(time.Now().UTC().Add(time.Minute).Truncate(time.Second).UnixNano())
	authority, err := auth.NewAuthorityWithClock(h.now)
	if err != nil {
		t.Fatal(err)
	}
	h.authority = authority
	h.reader = &mutableFleetGeneration{}
	h.reader.value.Store(workspacePolicyGeneration)
	executors, err := newConfiguredFleetExecutors([]string{"local"})
	if err != nil {
		t.Fatal(err)
	}
	if err := bindExternalFleetEffects(executors, externalFleetEffectBindings{
		mutating: []string{"local"},
	}); err != nil {
		t.Fatal(err)
	}
	opened, err := openService(context.Background(), serviceConfig{
		backend: "embedded", data: filepath.Join(h.root, "corpus"),
		policyDir: filepath.Join(h.root, "policy"),
		resolver:  h.authority.Resolver(), clock: h.now,
		executors:        executors,
		generationReader: h.reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	if opened.admission == nil {
		opened.close()
		t.Fatal("embedded service did not compose admission")
	}
	h.opened, h.isOpen = opened, true
	t.Cleanup(h.close)
	// As approvalActions, with the effect each action declares: an action
	// declaring nothing admits nothing.
	external := fleet.Effects{fleet.EffectMutatesExternal}
	if _, err := h.opened.fleetRegistry.Register(
		h.as(registrant), fleet.RegisterRequest{
			Context:         h.context(h.now().Add(time.Minute)),
			RegistrationKey: "admission-registration",
			Spec: fleet.Spec{
				ID: "gateway", AuthorizationDomain: workspaceAuthorizationDomain,
				Scopes: []fleet.Scope{
					{SourceID: workspaceSourceID, PolicyID: workspaceGrantPolicyID},
				},
				ExecutorRef: "local",
				Capabilities: []fleet.Capability{{
					Name: "ops",
					Actions: []fleet.Action{{
						Name:             "deploy",
						InputSchema:      json.RawMessage(`{"type":"object"}`),
						OutputSchema:     json.RawMessage(`{"type":"object"}`),
						Effects:          external,
						RequiresApproval: true,
					}, {
						Name:         "status",
						InputSchema:  json.RawMessage(`{"type":"object"}`),
						OutputSchema: json.RawMessage(`{"type":"object"}`),
						Effects:      external,
					}},
				}},
				LeaseExpiresAt: h.now().Add(20 * time.Hour),
			},
		}); err != nil {
		t.Fatal(err)
	}
	return h
}

// TestSDKAdmissionAgainstTheRealComposition drives the public client,
// sdk.New(...).Admission(), against the handler mounted over the services
// openService composes for the shipped binary. No double stands in for the
// dispatch store, the recorder, the restrictor or the publisher.
func TestSDKAdmissionAgainstTheRealComposition(t *testing.T) {
	t.Parallel()
	h := newAdmissionHarness(t)
	plane := newSDKAdmission(t, h, admissionCaller)
	admission := plane.client.Admission()
	ctx := context.Background()
	options := admissionapi.RequestOptions{
		MinReportWindow: 5 * time.Second, Clock: h.now,
	}

	t.Run("allowed, reported, receipted", func(t *testing.T) {
		request := sdkAdmissionRequest(h, "sdk-allowed", "status",
			[]string{admissionapi.EffectMutatesExternal}, nil)
		grant, err := admission.Request(ctx, request, options)
		if err != nil {
			t.Fatal(err)
		}
		if grant.Outcome != admissionapi.OutcomeAllowed || grant.Token == nil ||
			grant.Token.TokenID != request.TokenID || len(grant.Withhold) != 0 {
			t.Fatalf("grant = %#v", grant)
		}
		receipt, err := admission.Report(ctx, admissionapi.Report{
			Context: request.Context, Token: *grant.Token,
			Outcome: json.RawMessage(`{"version":"7"}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		if receipt.ActionID != grant.Token.ActionID ||
			receipt.State != admissionapi.DispatchSucceeded ||
			receipt.Version <= grant.Token.Version {
			t.Fatalf("receipt = %#v for token %#v", receipt, grant.Token)
		}

		// An exact replay of the report answers from the record, so a caller
		// that lost the receipt can retry safely.
		replayed, err := admission.Report(ctx, admissionapi.Report{
			Context: request.Context, Token: *grant.Token,
			Outcome: json.RawMessage(`{"version":"7"}`),
		})
		if err != nil || replayed.ActionID != receipt.ActionID ||
			replayed.Version != receipt.Version || replayed.State != receipt.State {
			t.Fatalf("replayed report = %#v, %v (first %#v)", replayed, err, receipt)
		}

		// Anything else against the spent token — a different outcome, or a
		// token that was never issued — is the one conflict every spent token
		// answers with, whatever spent it.
		forged := *grant.Token
		forged.TokenID = admissionapi.EncodeID([]byte("token-never-issued"))
		for name, report := range map[string]admissionapi.Report{
			"different outcome": {
				Context: request.Context, Token: *grant.Token,
				Failed: true, ErrorCode: "upstream_unreachable",
			},
			"never issued": {
				Context: request.Context, Token: forged,
				Outcome: json.RawMessage(`{"version":"7"}`),
			},
		} {
			_, err = admission.Report(ctx, report)
			var status *admissionapi.HTTPError
			if !errors.As(err, &status) || status.Status != http.StatusConflict ||
				status.Code != "conflict" ||
				status.Message != "conflict: admission token is not live" ||
				status.Indeterminate {
				t.Fatalf("%s: report = %#v", name, err)
			}
		}
	})

	t.Run("obligated withhold", func(t *testing.T) {
		// A reference with no registration is withheld, indistinguishably from
		// one the caller may not read. The obligation names it back exactly as
		// it was declared.
		unknown := admissionapi.EncodeID([]byte("doc-never-registered"))
		request := sdkAdmissionRequest(h, "sdk-obligated", "status",
			[]string{admissionapi.EffectMutatesExternal}, []string{unknown})
		grant, err := admission.Request(ctx, request, options)
		if err != nil {
			t.Fatal(err)
		}
		if grant.Outcome != admissionapi.OutcomeObligated || grant.Token == nil ||
			len(grant.Withhold) != 1 || grant.Withhold[0] != unknown {
			t.Fatalf("grant = %#v", grant)
		}
		// The caller cannot satisfy it and says so, so it does not stay
		// outstanding.
		if _, err := admission.Report(ctx, admissionapi.Report{
			Context: request.Context, Token: *grant.Token,
			Failed: true, ErrorCode: "obligation_unsatisfiable",
		}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("path B approval required", func(t *testing.T) {
		request := sdkAdmissionRequest(h, "sdk-held", "deploy",
			[]string{admissionapi.EffectMutatesExternal}, nil)
		// The first answer is the denial, which is what it always should have
		// been. This was pinned as a 503 "requires reconciliation" carrying
		// no indeterminate marker, with a note to flip it when the publisher
		// was fixed (#505).
		//
		// What was wrong: the denial commits a cancelled record, and the
		// publisher hardcoded OperationDispatch for action.canceled while the
		// record's AuthorizedOperations is [invoke], so the provenance check
		// refused the publication — and a refused publication is
		// ErrActionCommitted. Every denial therefore reported a failure for a
		// refusal that had committed and granted nothing. It went unnoticed
		// because the retry answered "denied": the second attempt looked
		// correct, which is exactly what the replay loop below asserts.
		//
		// Both gates are fixed. deny records its real transition operation,
		// and both the publisher and lifecyclePublicationPermits admit invoke
		// for action.canceled, because two legitimate writers produce that
		// kind under different operations.
		grant, err := admission.Request(ctx, request, options)
		if !errors.Is(err, admissionapi.ErrDenied) ||
			grant.Outcome != admissionapi.OutcomeDenied || grant.Token != nil ||
			grant.Withhold == nil || len(grant.Withhold) != 0 {
			t.Fatalf("path B first answer = %#v, %#v", grant, err)
		}
		// And the replay answers identically from the record, which is the
		// property that masked the defect and is still worth pinning.
		for attempt := 0; attempt < 2; attempt++ {
			grant, err = admission.Request(ctx, request, options)
			if !errors.Is(err, admissionapi.ErrDenied) ||
				grant.Outcome != admissionapi.OutcomeDenied || grant.Token != nil ||
				grant.Withhold == nil || len(grant.Withhold) != 0 {
				t.Fatalf("path B replay %d = %#v, %v", attempt, grant, err)
			}
			if body := plane.lastBody(admissionapi.RequestRoute); !bytes.Equal(
				body, []byte(`{"outcome":"denied","withhold":[]}`+"\n")) {
				t.Fatalf("path B denial wire = %q", body)
			}
		}
	})

	t.Run("outstanding", func(t *testing.T) {
		request := sdkAdmissionRequest(h, "sdk-outstanding", "status",
			[]string{admissionapi.EffectMutatesExternal}, nil)
		grant, err := admission.Request(ctx, request, options)
		if err != nil {
			t.Fatal(err)
		}
		page, err := admission.Outstanding(ctx, admissionapi.OutstandingRequest{
			Context: request.Context, Limit: 10,
		})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, outstanding := range page.Admissions {
			found = found || (outstanding.TokenID == grant.Token.TokenID &&
				outstanding.ActionID == grant.Token.ActionID)
		}
		if !found {
			t.Fatalf("unreported admission missing from %#v", page)
		}
	})
}
