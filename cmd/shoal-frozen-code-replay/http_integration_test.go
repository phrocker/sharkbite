// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/decisionhttp"
	"github.com/phrocker/shoal-oss/internal/decisionservice"
	"github.com/phrocker/shoal-oss/internal/decisionstore"
	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/decision/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Unrelated workspace methods are deliberately not implemented: decision routes
// must stay on the mounted provider. Production supplies its existing service.
type decisionWorkspace struct{ webapi.Service }

func serveDecisionTest(t *testing.T, s *serviceSession, sources []sourceEvidence, revoked *atomic.Bool) (*httptest.Server, func(string) *api.Client) {
	t.Helper()
	server := httptest.NewUnstartedServer(nil)
	provider, e := decisionhttp.New(s.service)
	if e != nil {
		t.Fatal(e)
	}
	authenticator := webapi.AuthenticatorFunc(func(r *http.Request) (auth.Decision, error) {
		switch r.Header.Get("Authorization") {
		case "Bearer test-alice":
			denied := map[string]bool{}
			if revoked.Load() {
				denied[sources[0].ID] = true
			}
			return localDecision(sources, denied)
		case "Bearer test-bob":
			d := s.decision
			return auth.NewDecision(auth.DecisionConfig{Subject: "other-principal", Actor: d.Actor(), ClientID: d.ClientID(), AuthorizationDomain: d.AuthorizationDomain(), AllowedOperations: d.AllowedOperations(), PermittedSourceIDs: d.PermittedSourceIDs(), PermittedPolicyIDs: d.PermittedPolicyIDs(), PolicyGeneration: 1, AuthenticationExpires: time.Now().Add(time.Hour), RequestID: "other-call"})
		default:
			return auth.Decision{}, errors.New("unknown test credential")
		}
	})
	handler, e := webapi.NewAuthenticatedHandler(&decisionWorkspace{}, authenticator, s.authority.Binder(), server.Listener.Addr().String())
	if e != nil {
		t.Fatal(e)
	}
	if e = handler.MountDecisions(provider, s.authority.Resolver()); e != nil {
		t.Fatal(e)
	}
	server.Config.Handler = handler
	server.Start()
	t.Cleanup(server.Close)
	client := func(token string) *api.Client {
		c, e := api.NewClient(api.Config{BaseURL: server.URL, HTTPClient: server.Client(), Token: func(context.Context) (string, error) { return token, nil }})
		if e != nil {
			t.Fatal(e)
		}
		return c
	}
	return server, client
}

func TestDecisionHTTPRealServiceAuthAndDurableSDKReplay(t *testing.T) {
	dir, mh, ph, sh := serviceFixture(t)
	task, _, _, e := serviceDefinitions()
	if e != nil {
		t.Fatal(e)
	}
	bundle, e := loadBundle(filepath.Join(dir, "numeric"), mh, ph, task, 1)
	if e != nil {
		t.Fatal(e)
	}
	defer bundle.Close()
	sources, e := loadSources(dir, sh, bundle)
	if e != nil {
		t.Fatal(e)
	}
	stateDir := t.TempDir()
	saved, e := loadServiceState(stateDir, mh, ph, sh, bundle.Provider.Identity().ID())
	if e != nil {
		t.Fatal(e)
	}
	session, e := openServiceSession(bundle, sources, saved, stateDir)
	if e != nil {
		t.Fatal(e)
	}
	closed := false
	defer func() {
		if !closed {
			session.engine.Close()
		}
	}()
	ctx, e := session.authority.Binder().Bind(context.Background(), session.decision)
	if e != nil {
		t.Fatal(e)
	}
	record := session.records[0]
	if e = session.catalog.Retain(ctx, record); e != nil {
		t.Fatal(e)
	}
	var revoked atomic.Bool
	server, client := serveDecisionTest(t, session, sources, &revoked)
	id := record.Bundle.Request.ID()
	key := []byte("http-shared-key")
	alice := client("test-alice")
	first, e := alice.Evaluate(context.Background(), id, key)
	if e != nil {
		t.Fatal(e)
	}
	second, e := alice.Evaluate(context.Background(), id, key)
	if e != nil {
		t.Fatal(e)
	}
	read, e := alice.Read(context.Background(), id, key)
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(first, second) || !reflect.DeepEqual(first, read) {
		t.Fatal("HTTP retry/read changed original receipt")
	}
	if first.Receipt.Result == nil || first.Receipt.Result.Answers[0].Label != "retain" || first.Ranking == nil {
		t.Fatalf("incomplete wire result %+v", first)
	}
	for _, tc := range []struct {
		token string
		id    shoal.ID
	}{{"test-bob", id}, {"test-alice", "missing-request"}, {"invalid", id}} {
		if _, e := client(tc.token).Read(context.Background(), tc.id, key); e == nil {
			t.Fatal("unauthorized/missing request disclosed")
		}
	}
	revoked.Store(true)
	if _, e := alice.Read(context.Background(), id, key); e == nil {
		t.Fatal("revoked source receipt disclosed")
	}
	if _, e := alice.Evaluate(context.Background(), id, key); e == nil {
		t.Fatal("revoked source retry admitted")
	}
	revoked.Store(false)
	// Identity or permission claims in caller JSON cannot extend the registered task.
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/v1/decisions", strings.NewReader(`{"request_id":"`+api.EncodeID(id)+`","principal_id":"test-alice","grants":["*"]}`))
	req.Header.Set("Authorization", "Bearer test-alice")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", api.EncodeKey(key))
	resp, e := server.Client().Do(req)
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("caller grants status %d", resp.StatusCode)
	}
	server.Close()
	if session.provider.calls != 1 {
		t.Fatalf("provider called %d times", session.provider.calls)
	}
	if e = session.engine.Close(); e != nil {
		t.Fatal(e)
	}
	closed = true
	// Reopen the real engine and authenticate fresh HTTP requests through a new
	// capability authority. Only persisted receipts may satisfy the SDK read/retry.
	again, e := openServiceSession(bundle, sources, saved, stateDir)
	if e != nil {
		t.Fatal(e)
	}
	defer again.engine.Close()
	server2, client2 := serveDecisionTest(t, again, sources, &revoked)
	replay, e := client2("test-alice").Evaluate(context.Background(), id, key)
	if e != nil {
		t.Fatal(e)
	}
	replayRead, e := client2("test-alice").Read(context.Background(), id, key)
	if e != nil {
		t.Fatal(e)
	}
	server2.Close()
	if !reflect.DeepEqual(first, replay) || !reflect.DeepEqual(first, replayRead) || again.provider.calls != 0 {
		t.Fatal("HTTP restart did not return original durable result")
	}
}

// revokeAfterCommit keeps the authenticated principal and fingerprint unchanged;
// only current source access disappears after the real durable commit succeeds.
type revokeAfterCommit struct {
	decisionservice.Receipts
	revoked   *atomic.Bool
	committed chan decisionstore.Receipt
}

func (r *revokeAfterCommit) Commit(ctx context.Context, scope decisionstore.Scope, key []byte, request decision.DecisionRequest, claim decisionstore.Claim, result decision.ResultConfig) (decisionstore.Receipt, error) {
	receipt, err := r.Receipts.Commit(ctx, scope, key, request, claim, result)
	if err == nil {
		r.committed <- receipt
		r.revoked.Store(true)
	}
	return receipt, err
}

type sourceAccessAfterCommit struct {
	decisionservice.Artifacts
	revoked *atomic.Bool
}

func (a sourceAccessAfterCommit) LoadAuthorized(ctx context.Context, d auth.Decision, id shoal.ID) (decisionservice.Bundle, error) {
	if a.revoked.Load() {
		return decisionservice.Bundle{}, shoal.NewError(shoal.ErrorNotFound, "request not found")
	}
	return a.Artifacts.LoadAuthorized(ctx, d, id)
}
func TestDecisionHTTPPostCommitRevocationIsIndeterminateAndReplayable(t *testing.T) {
	dir, mh, ph, sh := serviceFixture(t)
	task, _, _, err := serviceDefinitions()
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := loadBundle(filepath.Join(dir, "numeric"), mh, ph, task, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer bundle.Close()
	sources, err := loadSources(dir, sh, bundle)
	if err != nil {
		t.Fatal(err)
	}
	stateDir := t.TempDir()
	saved, err := loadServiceState(stateDir, mh, ph, sh, bundle.Provider.Identity().ID())
	if err != nil {
		t.Fatal(err)
	}
	session, err := openServiceSession(bundle, sources, saved, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer session.engine.Close()
	ctx, err := session.authority.Binder().Bind(context.Background(), session.decision)
	if err != nil {
		t.Fatal(err)
	}
	record := session.records[0]
	if err = session.catalog.Retain(ctx, record); err != nil {
		t.Fatal(err)
	}
	backend, err := explorercoord.NewEngineStore(session.engine, decisionstore.Table)
	if err != nil {
		t.Fatal(err)
	}
	receipts, err := decisionstore.New(backend, nil, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	var sourceRevoked atomic.Bool
	committing := &revokeAfterCommit{Receipts: receipts, revoked: &sourceRevoked, committed: make(chan decisionstore.Receipt, 1)}
	session.service, err = decisionservice.New(decisionservice.Config{Resolver: session.authority.Resolver(), Artifacts: sourceAccessAfterCommit{session.catalog, &sourceRevoked}, Providers: session.provider, Receipts: committing, Clock: time.Now, Lease: 2 * time.Minute, MaxCall: 30 * time.Second, Settlement: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	// The authenticator never changes grants: this failure occurs only in the
	// post-commit artifact authorization, after the durable write is acknowledged.
	var grantsRevoked atomic.Bool
	_, client := serveDecisionTest(t, session, sources, &grantsRevoked)
	sdk := client("test-alice")
	id := record.Bundle.Request.ID()
	key := []byte("post-commit-revocation")
	response, err := sdk.Evaluate(context.Background(), id, key)
	if !errors.Is(err, api.ErrIndeterminate) {
		t.Fatalf("post-commit revocation lost uncertainty: %v", err)
	}
	var httpError *api.HTTPError
	if !errors.As(err, &httpError) || !httpError.Indeterminate {
		t.Fatalf("missing wire uncertainty: %v", err)
	}
	if !reflect.DeepEqual(response, api.Response{}) {
		t.Fatalf("revoked result disclosed: %+v", response)
	}
	committed := <-committing.committed
	if committed.State != decisionstore.Committed || committed.Result == nil {
		t.Fatal("fixture did not commit durable receipt")
	}
	sourceRevoked.Store(false)
	replay, err := sdk.Evaluate(context.Background(), id, key)
	if err != nil {
		t.Fatal(err)
	}
	read, err := sdk.Read(context.Background(), id, key)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replay, read) || replay.Receipt.ID != committed.ID || replay.Receipt.PredictionID != api.EncodeID(committed.PredictionID) || session.provider.calls != 1 {
		t.Fatal("restored access did not replay original durable receipt without reinference")
	}
}
