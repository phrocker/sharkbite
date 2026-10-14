// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/retrieval"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// retrievingExecutor is an unconfined in-process executor that really
// retrieves, under the invoking principal's own context, through a real
// authorized client. It records which documents its retrieval returned.
type retrievingExecutor struct {
	client *authorized.Client
	mu     sync.Mutex
	docs   map[shoal.ID]bool
}

func (e *retrievingExecutor) Execute(
	ctx context.Context, _ Invocation,
) (ExecutionResult, error) {
	// The fixture's fixed clock puts the execution deadline in the real
	// past; the bound decision is what matters here, so keep it and drop
	// the deadline.
	response, err := e.client.Retrieve(context.WithoutCancel(ctx), retrieval.Request{
		Text: "fenced handoff promote", TopK: 20,
		Modes: []retrieval.Mode{retrieval.ModeLexical},
	})
	if err != nil {
		return ExecutionResult{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, result := range response.Results {
		for _, evidence := range result.Evidence {
			e.docs[evidence.Citation.DocumentID] = true
		}
	}
	return ExecutionResult{Output: json.RawMessage(`{"ok":true}`)}, nil
}

// TestALabelGrantNeverWidensAnUnconfinedExecution pins why label-namespace
// policies are left out of refuseUnconfinedRetrieval's policy comparison:
// a label only narrows. Each case invokes a real, unconfined executor that
// retrieves through the authorized client over a corpus holding a document
// labelled "secret" in the action's own source A ("source") and one in B.
func TestALabelGrantNeverWidensAnUnconfinedExecution(t *testing.T) {
	type outcome struct {
		admitted bool
		docs     map[shoal.ID]bool
	}
	setup := func(t *testing.T) (*executorClaimFixture, *retrievingExecutor, shoal.ID, shoal.ID) {
		t.Helper()
		fixture := newExecutorClaimFixture(t)
		corpus, err := explorer.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = corpus.Close() })
		store := authorized.NewMemoryPolicyStore()
		client := func(source, policy string) *authorized.Client {
			selector, err := authorized.NewStaticPolicySelector([]byte(source), []byte(policy))
			if err != nil {
				t.Fatal(err)
			}
			c, err := authorized.NewClient(authorized.Config{
				Base: corpus, Resolver: fixture.authority.Resolver(),
				PolicySelector: selector, PolicyStore: store,
				GenerationReader: fixedGeneration{},
				Clock:            func() time.Time { return *fixture.clock },
			})
			if err != nil {
				t.Fatal(err)
			}
			return c
		}
		clientA, clientB := client("source", "policy"), client("source-b", "policy-b")
		admin := bindDecision(t, fixture.authority, confinementDecision(t,
			[][]byte{[]byte("source"), []byte("source-b")},
			[][]byte{[]byte("policy"), []byte("policy-b"),
				labelPolicyOn(t, "source", "secret"), labelPolicyOn(t, "source-b", "secret")},
			auth.OperationIngest, auth.OperationRead))
		ingest := func(c *authorized.Client, uri string) shoal.ID {
			t.Helper()
			receipt, err := c.Ingest(admin, explorer.Source{
				URI: uri, MediaType: explorer.MediaTypeMarkdown,
				Content:  "# Promotion\n\nLocal tables promote under a fenced handoff.\n",
				Metadata: shoal.Metadata{interaction.PropertyVisibility: "secret"},
			})
			if err != nil {
				t.Fatal(err)
			}
			return receipt.Document.ID
		}
		docA := ingest(clientA, "file:///a/closed.md")
		docB := ingest(clientB, "file:///b/closed.md")
		executor := &retrievingExecutor{client: clientA, docs: map[shoal.ID]bool{}}
		fixture.bindExecutor(t, executor)
		return fixture, executor, docA, docB
	}
	invoke := func(t *testing.T, sources, policies [][]byte) (outcome, shoal.ID, shoal.ID) {
		t.Helper()
		fixture, executor, docA, docB := setup(t)
		caller := bindDecision(t, fixture.authority, confinementDecision(t, sources, policies,
			auth.OperationDispatch, auth.OperationInvoke, auth.OperationRetrieve))
		enqueue := dispatchEnqueue(fixture.now, "request")
		enqueue.ID = []byte("label-confinement")
		enqueue.IdempotencyKey = []byte("label-confinement-key")
		record, err := fixture.service.Invoke(caller, InvokeRequest{
			Enqueue: enqueue, ClaimID: []byte("label-claim"), Lease: time.Minute,
		})
		if err != nil {
			if !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
				t.Fatalf("invoke failed for another reason: %v", err)
			}
			return outcome{docs: executor.docs}, docA, docB
		}
		if record.State != DispatchSucceeded {
			t.Fatalf("invoke = %q / %q", record.State, record.ErrorCode)
		}
		return outcome{admitted: true, docs: executor.docs}, docA, docB
	}

	// (a) The regression: a holder of the action's own source's label was
	// refused. It is admitted and retrieves the labelled document.
	t.Run("a label on the action's own source", func(t *testing.T) {
		got, docA, docB := invoke(t, [][]byte{[]byte("source")},
			[][]byte{[]byte("policy"), labelPolicyOn(t, "source", "secret")})
		if !got.admitted || !got.docs[docA] {
			t.Fatalf("admitted = %v, retrieved %v; want admitted and the "+
				"labelled document in A retrieved", got.admitted, got.docs)
		}
		if got.docs[docB] {
			t.Fatal("retrieval reached B")
		}
	})

	// (b) A label on B without B's source. Only a canonical label policy on
	// the action's own source is left out of the policy comparison, so this
	// grant is compared like any other policy and refused before the
	// executor runs. Behind that, the AccessRule conjunction alone keeps B's
	// labelled document out of reach: every term of B's rule names source B,
	// which this principal does not hold. That is retrieved directly here,
	// under the same decision, so it is pinned whatever the fleet check does.
	t.Run("a label on another source without that source", func(t *testing.T) {
		policies := [][]byte{[]byte("policy"), labelPolicyOn(t, "source-b", "secret")}
		got, _, _ := invoke(t, [][]byte{[]byte("source")}, policies)
		if got.admitted || len(got.docs) != 0 {
			t.Fatalf("admitted = %v, retrieved %v; want refused before the "+
				"executor ran", got.admitted, got.docs)
		}
		fixture, executor, _, docB := setup(t)
		direct := bindDecision(t, fixture.authority, confinementDecision(t,
			[][]byte{[]byte("source")}, policies, auth.OperationRetrieve))
		if _, err := executor.Execute(direct, Invocation{}); err != nil {
			t.Fatal(err)
		}
		if executor.docs[docB] {
			t.Fatal("a label grant without its source reached B's document")
		}
	})

	// (c) A label on B with B's source: the principal is genuinely wider, and
	// the sources check refuses it before the executor runs.
	t.Run("a label on another source with that source", func(t *testing.T) {
		got, _, docB := invoke(t, [][]byte{[]byte("source"), []byte("source-b")},
			[][]byte{[]byte("policy"), labelPolicyOn(t, "source-b", "secret")})
		if got.admitted {
			t.Fatal("a principal holding another source ran unconfined")
		}
		if got.docs[docB] || len(got.docs) != 0 {
			t.Fatalf("the refused executor ran and retrieved %v", got.docs)
		}
	})
}

func confinementDecision(
	t *testing.T, sources, policies [][]byte, operations ...auth.Operation,
) auth.Decision {
	t.Helper()
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject: "owner", Actor: "actor", AuthorizationDomain: []byte("domain"),
		AllowedOperations:     operations,
		PermittedSourceIDs:    sources,
		PermittedPolicyIDs:    policies,
		PolicyGeneration:      1,
		AuthenticationExpires: time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC),
		RequestID:             "request", CorrelationID: "correlation",
	})
	if err != nil {
		t.Fatal(err)
	}
	return decision
}

type fixedGeneration struct{}

func (fixedGeneration) CurrentPolicyGeneration(context.Context, []byte) (int64, error) {
	return 1, nil
}
