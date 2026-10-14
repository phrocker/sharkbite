// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package atpl

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	attestationapi "github.com/phrocker/shoal-oss/pkg/attestation/api"
	"github.com/phrocker/shoal-oss/pkg/executorref"
	"github.com/phrocker/shoal-oss/pkg/executorref/executorreftest"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type acceptingPresenter struct{}

func (acceptingPresenter) Verify(
	fleet.AttestationPrincipal, fleet.AttestationPresentation, time.Time,
) (fleet.AttestationReceipt, error) {
	return fleet.AttestationReceipt{AttestationID: "exattest:1"}, nil
}

func (acceptingPresenter) Record(
	_ context.Context, _ fleet.AttestationPrincipal,
	_ fleet.AttestationPresentation, now time.Time,
) (fleet.AttestationReceipt, error) {
	return fleet.AttestationReceipt{
		AttestationID: "exattest:1", ExpiresAt: now.Add(time.Hour),
	}, nil
}

type discardAttestationAudit struct{}

func (discardAttestationAudit) RecordAttestation(
	context.Context, fleet.AttestationAudit,
) error {
	return nil
}

// TestExecutorRefParity: every place an executor reference enters the system
// (fleet registration, attestation presentation on the server and in the
// public client, the ATPL compiler, and an action-execution decision's
// binding) agrees with executorref.ValidExecutorRef on the whole probe table
// (#391). A site with its own local check would accept a look-alike the
// others refuse, or refuse a worker that could never then be bound. The host
// configuration sites (the explorer's executor-ref flags and the attestation
// trust file) run the same table in cmd/shoal-explore-web's
// TestExecutorRefConfigParity.
func TestExecutorRefParity(t *testing.T) {
	now := testNow
	authority, err := auth.NewAuthorityWithClock(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	attestations, err := fleet.NewAttestationService(fleet.AttestationConfig{
		Presenter: acceptingPresenter{}, Resolver: authority.Resolver(),
		Recorder: discardAttestationAudit{}, Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	// A worker presents only for the executor it is bound to (#391), so the
	// presenting decision is bound to the reference under test. A reference
	// the binding refuses gets a decision bound elsewhere, which Present
	// must still refuse on the reference's shape before the binding.
	presentCtx := func(ref string) context.Context {
		config := auth.DecisionConfig{
			Subject: "worker", Actor: "worker", ClientID: "worker-client",
			AuthorizationDomain:    []byte("domain"),
			AllowedOperations:      []auth.Operation{auth.OperationExecute},
			PolicyGeneration:       1,
			AuthenticationExpires:  now.Add(time.Hour),
			RequestID:              "present",
			CorrelationID:          "present-correlation",
			ServiceRole:            auth.ServiceRoleActionExecution,
			ServiceCeilingIdentity: "ceiling-execute",
			ExecutorBinding:        ref,
		}
		executor, err := auth.NewDecision(config)
		if err != nil {
			config.ExecutorBinding = "worker"
			if executor, err = auth.NewDecision(config); err != nil {
				t.Fatal(err)
			}
		}
		ctx, err := authority.Binder().Bind(context.Background(), executor)
		if err != nil {
			t.Fatal(err)
		}
		return ctx
	}

	sites := map[string]func(ref string) error{
		"fleet registration": func(ref string) error {
			spec := fleet.Spec{
				ID: "agent", AuthorizationDomain: []byte("domain"),
				Scopes: []fleet.Scope{{
					SourceID: []byte("source"), PolicyID: []byte("policy"),
				}},
				ExecutorRef: ref, LeaseExpiresAt: now.Add(time.Hour),
				Capabilities: []fleet.Capability{{
					Name: "search", Actions: []fleet.Action{{
						Name:         "query",
						InputSchema:  json.RawMessage(`{"type":"object"}`),
						OutputSchema: json.RawMessage(`{"type":"object"}`),
					}},
				}},
			}
			_, err := spec.Canonical(now)
			return err
		},
		"attestation presentation": func(ref string) error {
			_, err := attestations.Present(presentCtx(ref), fleet.AttestationPresentation{
				ExecutorRef: ref, IdempotencyKey: []byte("key"),
				Report: []byte(`{}`),
			})
			return err
		},
		"attestation client": func(ref string) error {
			_, err := attestationapi.NewRequest(ref, []byte("key"), []byte(`{}`))
			return err
		},
		// The executor declaration alone: a whole Compile would also run the
		// agent's executor_ref through fleet.Spec.Canonical, which would mask
		// a divergent check in compileExecutors.
		"ATPL executors": func(ref string) error {
			document := base(t)
			document.Executors[0].Ref = ref
			_, err := compileExecutors([]Document{document})
			return err
		},
		"ATPL compile": func(ref string) error {
			document := base(t)
			for i := range document.Executors {
				if document.Executors[i].Ref == "search-exec" {
					document.Executors[i].Ref = ref
				}
			}
			for i := range document.Agents {
				if document.Agents[i].ExecutorRef == "search-exec" {
					document.Agents[i].ExecutorRef = ref
				}
			}
			_, err := Compile([]Document{document}, now, nil)
			return err
		},
		"executor binding": func(ref string) error {
			_, err := auth.NewDecision(auth.DecisionConfig{
				Subject: "worker", Actor: "worker",
				AuthorizationDomain:    []byte("domain"),
				AllowedOperations:      []auth.Operation{auth.OperationExecute},
				PolicyGeneration:       1,
				AuthenticationExpires:  now.Add(time.Hour),
				RequestID:              "bind",
				ServiceRole:            auth.ServiceRoleActionExecution,
				ServiceCeilingIdentity: "ceiling-execute",
				ExecutorBinding:        ref,
			})
			return err
		},
	}
	for _, probe := range executorreftest.Probes() {
		if (executorref.ValidExecutorRef(probe.Ref) == nil) != probe.Valid {
			t.Fatalf("%s: the table disagrees with the rule", probe.Name)
		}
		for site, check := range sites {
			err := check(probe.Ref)
			if probe.Valid && err != nil {
				t.Errorf("%s refuses %s (%q): %v", site, probe.Name, probe.Ref, err)
			}
			if !probe.Valid && !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) &&
				!(site == "attestation client" && err != nil) {
				t.Errorf("%s accepts %s (%q): %v", site, probe.Name, probe.Ref, err)
			}
		}
	}
}
