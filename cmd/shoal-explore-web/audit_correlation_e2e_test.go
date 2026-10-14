// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

// End-to-end tests for #532: the interaction audit carries the decision's
// correlation. Every request here goes through the real OIDC authenticator
// over real HTTP (oidcApprovalWorld), per docs/approval.md's testing note — a
// correlation that only an injected decision carries proves nothing about
// what the shipped authenticator mints.

import (
	"bytes"
	"context"
	"encoding/hex"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// auditedSession is one interaction session, as read back from the corpus
// after the service closed, with the summary's view of its correlation.
type auditedSession struct {
	session interaction.Session
	summary explorer.InteractionSummary
}

// auditedSessions closes the service and reads every interaction session it
// recorded, keyed by tool kind ("fleet.<operation>.<phase>").
func auditedSessions(t *testing.T, w *oidcApprovalWorld) map[string][]auditedSession {
	t.Helper()
	w.h.close()
	corpus, err := explorer.Open(filepath.Join(w.h.root, "corpus"))
	if err != nil {
		t.Fatal(err)
	}
	defer corpus.Close()
	summaries, err := corpus.Interactions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result := make(map[string][]auditedSession)
	for _, summary := range summaries {
		session, err := corpus.Interaction(context.Background(), summary.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		kind := ""
		if len(session.Turns) == 1 && session.Turns[0].ToolCall != nil {
			kind = session.Turns[0].ToolCall.Kind
		}
		result[kind] = append(result[kind], auditedSession{session, summary})
	}
	return result
}

// one is the single session of a kind, for one approval record.
func one(
	t *testing.T, sessions map[string][]auditedSession, kind string, id []byte,
) auditedSession {
	t.Helper()
	var found []auditedSession
	for _, candidate := range sessions[kind] {
		if candidate.session.ResultID == shoal.ID(hex.EncodeToString(id)) {
			found = append(found, candidate)
		}
	}
	if len(found) != 1 {
		kinds := make([]string, 0, len(sessions))
		for name := range sessions {
			kinds = append(kinds, name)
		}
		sort.Strings(kinds)
		t.Fatalf("%d %s sessions for %s; recorded kinds: %v",
			len(found), kind, id, kinds)
	}
	return found[0]
}

func assertCorrelation(t *testing.T, got auditedSession, want shoal.ID) {
	t.Helper()
	if got.session.CorrelationID != want || got.summary.CorrelationID != want {
		t.Fatalf("%s session correlation = %q (summary %q), want %q",
			got.session.Turns[0].ToolCall.Kind, got.session.CorrelationID,
			got.summary.CorrelationID, want)
	}
}

// TestSuppliedCorrelationJoinsApprovalToDispatchAudit is the reason for the
// field: a gateway acting for its users threads one Shoal-Correlation-ID
// through the requester's hold, the approver's decision and the requester's
// return, and every interaction session those hops recorded — the approval
// audits and the dispatched action's audit — carries it. Before #532 the only
// join was each hop's own request ID, which names one request and threads
// nothing.
func TestSuppliedCorrelationJoinsApprovalToDispatchAudit(t *testing.T) {
	t.Parallel()
	const trace = "gateway-trace-532"
	w := newOIDCApprovalWorld(t, nil, nil)
	alice := call{token: w.fleetToken("alice", nil), correlation: trace}
	bob := call{token: w.approverToken("bob"), correlation: trace}
	request := w.h.held("correlated-held-1")
	receipt := w.mustHold(alice, request, "gateway")
	w.mustDecide(bob, receipt)
	enqueued := w.request(alice, request, "gateway")
	if enqueued.status != http.StatusCreated || enqueued.State != "enqueued" {
		t.Fatalf("materialization = %d %s", enqueued.status, enqueued.raw)
	}

	sessions := auditedSessions(t, w)
	for _, kind := range []string{
		"fleet.dispatch.approval_request",
		"fleet.action_approve.approval_decision",
		"fleet.dispatch.approval_materialize",
		// The dispatched action's own audit, written by the action recorder.
		"fleet.dispatch.approval_enqueue",
	} {
		assertCorrelation(t, one(t, sessions, kind, request.id), trace)
	}
	// The lifecycle publisher's audit of the action.enqueued event is
	// recorded under the same decision, so it joins the trace too.
	published := 0
	for _, event := range sessions["fleet.dispatch"] {
		if event.session.CorrelationID == trace {
			published++
		}
	}
	if published == 0 {
		t.Fatalf("no lifecycle event audit carries the trace among %d",
			len(sessions["fleet.dispatch"]))
	}
	// The hops are different principals and different requests: the
	// correlation is what they have in common, not the request ID.
	held := one(t, sessions, "fleet.dispatch.approval_request", request.id)
	decided := one(t, sessions, "fleet.action_approve.approval_decision", request.id)
	if held.session.RequestID == decided.session.RequestID ||
		held.session.Actor.SubjectID == decided.session.Actor.SubjectID {
		t.Fatal("the hops are not distinct requests by distinct principals")
	}
}

// TestGeneratedCorrelationAppearsOnTheAudit: a caller that sends no header
// is the root of its own trace, and its authenticator generates the
// correlation. That generated value is what the session carries — the same
// one the durable approval and action records hold for the same decision.
func TestGeneratedCorrelationAppearsOnTheAudit(t *testing.T) {
	t.Parallel()
	w := newOIDCApprovalWorld(t, nil, nil)
	alice := call{token: w.fleetToken("alice", nil)}
	bob := call{token: w.approverToken("bob")}
	request := w.h.held("generated-held-1")
	receipt := w.mustHold(alice, request, "gateway")
	w.mustDecide(bob, receipt)
	enqueued := w.request(alice, request, "gateway")
	if enqueued.status != http.StatusCreated || enqueued.State != "enqueued" {
		t.Fatalf("materialization = %d %s", enqueued.status, enqueued.raw)
	}
	decision := w.decisionRecord(request.id)
	held, decided := decision.Request.CorrelationID, decision.DecisionCorrelationID
	for _, generated := range []shoal.ID{held, decided} {
		if !strings.HasPrefix(string(generated), "oidc-correlation-") ||
			interaction.ValidateCorrelationID(generated) != nil {
			t.Fatalf("correlation %q was not generated, or is not recordable",
				generated)
		}
	}

	sessions := auditedSessions(t, w)
	assertCorrelation(t,
		one(t, sessions, "fleet.dispatch.approval_request", request.id), held)
	assertCorrelation(t,
		one(t, sessions, "fleet.action_approve.approval_decision", request.id),
		decided)
	// The return is its own request with its own generated correlation, and
	// the materialization audit and the dispatched action's audit both carry
	// it, because both were recorded under that one decision.
	materialized := one(t, sessions, "fleet.dispatch.approval_materialize", request.id)
	action := one(t, sessions, "fleet.dispatch.approval_enqueue", request.id)
	returned := materialized.session.CorrelationID
	if !strings.HasPrefix(string(returned), "oidc-correlation-") ||
		returned == held || returned == decided {
		t.Fatalf("the return's correlation %q is not its own generated value",
			returned)
	}
	assertCorrelation(t, action, returned)
}

// TestNonUTF8CorrelationIsRefusedAtTheEdge: net/http admits a header byte
// above 0x7f as obs-text, and #527's printable check ranged over the string,
// where such a byte decodes to U+FFFD and passes. Such a value reached the
// approval and action records raw and was then dropped from the audit
// session. The mint now applies the session boundary's own rule
// (interaction.ValidateCorrelationID), so the header is refused at
// authentication on every hop of the approval route.
//
// A 401 alone would not show the bytes went nowhere, so after the whole route
// has run, the approval records, the action record, every interaction session
// and every file under the data root are searched for them, raw and as the
// U+FFFD a lossy decoder would have made of them. The control threads a valid
// header through the same route end to end — hold, decide, materialize — so
// the tightening has not closed the ordinary path.
func TestNonUTF8CorrelationIsRefusedAtTheEdge(t *testing.T) {
	t.Parallel()
	const (
		invalid = "bad\xffbyte"
		trace   = "upstream-control-532"
	)
	w := newOIDCApprovalWorld(t, nil, nil)
	request := w.h.held("oidc-non-utf8-trace")
	alice := w.fleetToken("alice", nil)
	bob := w.approverToken("bob")

	refused := func(hop string, got answer) {
		t.Helper()
		if got.status != http.StatusUnauthorized {
			t.Fatalf("%s with a non-UTF-8 correlation = %d %s, want 401",
				hop, got.status, got.raw)
		}
	}
	refused("hold", w.request(call{token: alice, correlation: invalid}, request, "gateway"))
	if records := w.records(request.id); len(records) != 0 {
		t.Fatalf("a refused hold wrote %d approval records", len(records))
	}

	receipt := w.mustHold(call{token: alice, correlation: trace}, request, "gateway")
	refused("decide", w.decide(call{token: bob, correlation: invalid}, receipt))
	if records := w.records(request.id); len(records) != 1 {
		t.Fatalf("a refused decision wrote: %d approval records, want 1",
			len(records))
	}
	w.mustDecide(call{token: bob, correlation: trace}, receipt)
	refused("materialize", w.request(call{token: alice, correlation: invalid}, request, "gateway"))
	enqueued := w.request(call{token: alice, correlation: trace}, request, "gateway")
	if enqueued.status != http.StatusCreated || enqueued.State != "enqueued" ||
		enqueued.materialized() == nil ||
		enqueued.materialized().CorrelationID != b64([]byte(trace)) {
		t.Fatalf("the valid header did not thread to the action: %d %s",
			enqueued.status, enqueued.raw)
	}

	forms := [][]byte{[]byte(invalid), []byte("bad�byte")}
	for _, record := range w.records(request.id) {
		for _, value := range []shoal.ID{
			record.Request.CorrelationID, record.Request.TransitionCorrelationID,
			record.DecisionCorrelationID,
		} {
			if value != "" && value != trace {
				t.Fatalf("an approval record holds correlation %q", value)
			}
		}
	}
	if strings.Contains(string(enqueued.raw), "bad") {
		t.Fatalf("the action record names the refused correlation: %s",
			enqueued.raw)
	}

	sessions := auditedSessions(t, w)
	for _, kind := range []string{
		"fleet.dispatch.approval_request",
		"fleet.action_approve.approval_decision",
		"fleet.dispatch.approval_materialize",
		"fleet.dispatch.approval_enqueue",
	} {
		assertCorrelation(t, one(t, sessions, kind, request.id), trace)
	}
	for _, list := range sessions {
		for _, audited := range list {
			if id := audited.session.CorrelationID; id != "" && id != trace &&
				!strings.HasPrefix(string(id), "oidc-correlation-") {
				t.Fatalf("an audit session holds correlation %q", id)
			}
		}
	}
	found := 0
	if err := filepath.WalkDir(w.h.root, func(
		path string, entry fs.DirEntry, err error,
	) error {
		if err != nil || entry.IsDir() {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, form := range forms {
			if bytes.Contains(content, form) {
				t.Fatalf("%s holds the refused correlation %q", path, form)
			}
		}
		if bytes.Contains(content, []byte(trace)) {
			found++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The search reads where correlations land: the valid one is on disk.
	if found == 0 {
		t.Fatal("the valid correlation is in no file; the search proves nothing")
	}
}
