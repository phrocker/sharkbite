// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

// End-to-end tests for the stable identity claim (#526, PR1). As in
// oidc_approver_e2e_test.go, every request is a signed token sent over real
// HTTP to the handler the binary builds and authenticated by the real OIDC
// authenticator; the one counterfactual is labelled.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// stableMappingDocument is the test mapping restating the identity claim.
func stableMappingDocument(claim []string) func(string) map[string]any {
	return func(issuer string) map[string]any {
		document := approverMappingDocument(issuer)
		document["identity_claim"] = claim
		return document
	}
}

func stableEdit(t *testing.T, claim []string) func(*oidcConfig) {
	encoded, err := json.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}
	return func(config *oidcConfig) { config.identityClaim = string(encoded) }
}

// newStableWorld is an approval world under the stable identity claim, on
// an issuer whose discovery advertises pairwise subjects — the issuers the
// claim exists for. registrant is the owner's identity claims.
func newStableWorld(
	t *testing.T, claim []string, registrant jwt.MapClaims,
) *oidcApprovalWorld {
	t.Helper()
	return newOIDCApprovalWorldWith(t, stableEdit(t, claim), registrant,
		[]string{"public", "pairwise"}, stableMappingDocument(claim))
}

func (w *oidcApprovalWorld) stable(value string) shoal.ID {
	return shoal.ID(w.authn.Load().stableIdentityNamespace() + value)
}

// approverTokenWith is a human's approver-audience token with extra claims;
// a nil value deletes the claim.
func (w *oidcApprovalWorld) approverTokenWith(subject string, extra jwt.MapClaims) string {
	claims := approverClaims(w.issuer, w.h.now(), subject)
	for key, value := range extra {
		claims[key] = value
	}
	return w.sign(claims)
}

func oid(value any) jwt.MapClaims { return jwt.MapClaims{"oid": value} }

func (w *oidcApprovalWorld) schemeDigest() auth.Digest {
	return w.authn.Load().identityScheme().digest
}

// TestStableIdentityOneHumanAcrossClientsIsOneIdentity is the case #526
// exists for. Under a pairwise issuer one human has a different sub on the
// workspace client and on the approval console; with the stable claim both
// branches name them by oid, so they cannot approve their own request — as
// the requester, as the agent's registrant, or as an ancestor's — while a
// different human can. Both branches carry the caller's correlation.
func TestStableIdentityOneHumanAcrossClientsIsOneIdentity(t *testing.T) {
	t.Parallel()
	const aliceTrace, bobTrace = "upstream-alice-526", "upstream-bob-526"
	w := newStableWorld(t, []string{"oid"}, oid("oid-owner"))
	h := w.h
	alice := call{
		token:       w.fleetToken("alice-workspace-sub", oid("oid-alice")),
		correlation: aliceTrace,
	}
	request := h.held("stable-1")
	receipt := w.mustHold(alice, request, "gateway")
	held := w.records(request.id)
	if len(held) == 0 || held[0].Request.Subject != w.stable("oid-alice") {
		t.Fatalf("the requester is not named by the stable claim: %+v", held)
	}
	if held[0].IdentityScheme != w.schemeDigest() ||
		held[0].IdentityScheme == (auth.Digest{}) {
		t.Fatalf("the request was not stamped with the scheme in force: %x",
			held[0].IdentityScheme)
	}
	if held[0].Request.CorrelationID != aliceTrace {
		t.Fatalf("the workspace branch lost the correlation: %q",
			held[0].Request.CorrelationID)
	}

	// The same human through the approval console: another sub, the same
	// oid. Under sub-derived identities this was two people.
	assertNotIndependent(t, "the requester under another client's sub",
		w.decide(call{token: w.approverTokenWith(
			"alice-console-sub", oid("oid-alice"))}, receipt))
	// The agent's registrant, likewise.
	assertNotIndependent(t, "the registrant under another client's sub",
		w.decide(call{token: w.approverTokenWith(
			"owner-console-sub", oid("oid-owner"))}, receipt))

	// A different human decides, threading their own trace.
	bob := call{
		token:       w.approverTokenWith("bob-console-sub", oid("oid-bob")),
		correlation: bobTrace,
	}
	decided := w.mustDecide(bob, receipt)
	if decided.Approver != b64([]byte(w.stable("oid-bob"))) ||
		decided.ApproverActor != b64([]byte(w.stable("oid-bob"))) {
		t.Fatalf("decided = %s", decided.raw)
	}
	record := w.decisionRecord(request.id)
	want := auth.GrantProvenance{
		Issuer: w.issuer.server.URL, Subject: "bob-console-sub",
		ClaimPath:         []string{"realm_access", "roles"},
		MatchedValue:      testApproverValue,
		MappingDigest:     w.digest.Load().(auth.Digest),
		IdentityClaimPath: []string{"oid"},
	}
	if !record.ApproverProvenance.Equal(want) {
		t.Fatalf("approver provenance = %+v, want %+v", record.ApproverProvenance, want)
	}
	if record.ApproverClientID != w.identity(testApproverClient) {
		t.Fatalf("the approver client is not azp: %q", record.ApproverClientID)
	}
	if record.Request.CorrelationID != aliceTrace ||
		record.DecisionCorrelationID != bobTrace {
		t.Fatalf("correlations = request %q decision %q",
			record.Request.CorrelationID, record.DecisionCorrelationID)
	}
	again := w.request(call{token: alice.token}, request, "gateway")
	if again.status != http.StatusCreated || again.State != "enqueued" {
		t.Fatalf("materialization = %d %s", again.status, again.raw)
	}

	// Generated correlations, on both branches (#524, #527).
	generated := h.held("stable-generated")
	generatedReceipt := w.mustHold(
		call{token: alice.token}, generated, "gateway")
	w.mustDecide(call{token: bob.token}, generatedReceipt)
	generatedRecord := w.decisionRecord(generated.id)
	if !strings.HasPrefix(string(generatedRecord.Request.CorrelationID), "oidc-correlation-") ||
		!strings.HasPrefix(string(generatedRecord.DecisionCorrelationID), "oidc-correlation-") ||
		generatedRecord.Request.CorrelationID == generatedRecord.DecisionCorrelationID {
		t.Fatalf("generated correlations = request %q decision %q",
			generatedRecord.Request.CorrelationID,
			generatedRecord.DecisionCorrelationID)
	}

	t.Run("ancestor", func(t *testing.T) {
		carol := w.fleetToken("carol-workspace-sub", oid("oid-carol"))
		w.register(carol, "stable-parent", "")
		w.register(carol, "stable-child", "stable-parent")
		child := h.held("stable-child-1")
		childReceipt := w.mustHold(call{token: alice.token}, child, "stable-child")
		assertNotIndependent(t, "the ancestors' registrant under another sub",
			w.decide(call{token: w.approverTokenWith(
				"carol-console-sub", oid("oid-carol"))}, childReceipt))
		w.mustDecide(call{token: bob.token}, childReceipt)
	})
}

// TestStableIdentityClaimShapesFailClosedOnBothBranches: the claim must be
// present and a string of 1-256 bytes with no control character and no
// surrounding whitespace, and it is never trimmed. Every other shape is the
// generic 401 on the workspace branch and on the approver branch, and writes
// nothing. The controls are the same tokens with a valid claim — including
// one of exactly 256 bytes — so each 401 is the claim's.
func TestStableIdentityClaimShapesFailClosedOnBothBranches(t *testing.T) {
	t.Parallel()
	w := newStableWorld(t, []string{"oid"}, oid("oid-owner"))
	h := w.h
	held := h.held("stable-shapes")
	receipt := w.mustHold(
		call{token: w.fleetToken("alice", oid("oid-alice"))}, held, "gateway")
	before := len(w.records(held.id))
	for _, probe := range []struct {
		name  string
		value any
		drop  bool
	}{
		{name: "missing", drop: true},
		{name: "null", value: nil},
		{name: "empty", value: ""},
		{name: "leading space", value: " oid-x"},
		{name: "trailing space", value: "oid-x "},
		{name: "tab padded", value: "oid-x\t"},
		{name: "too long", value: strings.Repeat("o", maxStableIdentityBytes+1)},
		{name: "NUL", value: "oid\x00x"},
		{name: "newline", value: "oid\nx"},
		{name: "C1 control", value: "oid\u0085x"},
		{name: "number", value: 42},
		{name: "boolean", value: true},
		{name: "array", value: []any{"oid-x"}},
		{name: "object", value: map[string]any{"value": "oid-x"}},
	} {
		t.Run(probe.name, func(t *testing.T) {
			extra := oid(probe.value)
			workspace := w.fleetToken("probe-workspace", extra)
			approverClaims := extra
			if probe.drop {
				workspace = w.fleetToken("probe-workspace", nil)
				approverClaims = nil
			}
			request := h.held("stable-shape-" + strings.ReplaceAll(probe.name, " ", "-"))
			if got := w.request(call{token: workspace}, request, "gateway"); got.status != http.StatusUnauthorized {
				t.Fatalf("workspace branch = %d %s, want 401", got.status, got.raw)
			}
			if records := w.records(request.id); len(records) != 0 {
				t.Fatalf("a refused request wrote %d records", len(records))
			}
			approver := w.approverTokenWith("probe-console", approverClaims)
			if got := w.decide(call{token: approver}, receipt); got.status != http.StatusUnauthorized {
				t.Fatalf("approver branch = %d %s, want 401", got.status, got.raw)
			}
			if records := w.records(held.id); len(records) != before {
				t.Fatalf("a refused decision wrote %d records", len(records)-before)
			}
		})
	}

	// Controls: the longest value accepted, on both branches.
	longest := strings.Repeat("o", maxStableIdentityBytes)
	w.mustHold(call{token: w.fleetToken("probe-workspace", oid(longest))},
		h.held("stable-shape-control"), "gateway")
	decided := w.mustDecide(call{token: w.approverTokenWith(
		"probe-console", oid(longest))}, receipt)
	if decided.Approver != b64([]byte(w.stable(longest))) {
		t.Fatalf("the control decision = %s", decided.raw)
	}
}

// TestStableIdentityClaimPathIsNotADottedString: a nested path is never
// satisfied by a top-level key whose name contains the dot, nor a dotted
// one-segment path by the nested claim; a non-object on the way, or a typed
// value at the end, fails closed.
func TestStableIdentityClaimPathIsNotADottedString(t *testing.T) {
	t.Parallel()
	nested := func(value any) jwt.MapClaims {
		return jwt.MapClaims{"ext": map[string]any{"oid": value}}
	}
	dotted := func(value any) jwt.MapClaims {
		return jwt.MapClaims{"ext.oid": value}
	}
	for _, probe := range []struct {
		name    string
		claim   []string
		owner   jwt.MapClaims
		valid   jwt.MapClaims
		spoofed []jwt.MapClaims
	}{
		{
			name: "nested path", claim: []string{"ext", "oid"},
			owner: nested("oid-owner"), valid: nested("oid-alice"),
			spoofed: []jwt.MapClaims{
				dotted("oid-alice"),
				{"ext": "oid-alice"},
				nested([]any{"oid-alice"}),
				nested(map[string]any{"oid": "oid-alice"}),
			},
		},
		{
			name: "dotted segment", claim: []string{"ext.oid"},
			owner: dotted("oid-owner"), valid: dotted("oid-alice"),
			spoofed: []jwt.MapClaims{nested("oid-alice")},
		},
	} {
		t.Run(probe.name, func(t *testing.T) {
			w := newStableWorld(t, probe.claim, probe.owner)
			h := w.h
			for index, claims := range probe.spoofed {
				request := h.held("spoof-" + string(rune('a'+index)))
				if got := w.request(call{token: w.fleetToken("alice", claims)},
					request, "gateway"); got.status != http.StatusUnauthorized {
					t.Fatalf("spoof %d on the workspace branch = %d %s",
						index, got.status, got.raw)
				}
			}
			receipt := w.mustHold(call{token: w.fleetToken("alice", probe.valid)},
				h.held("spoof-control"), "gateway")
			for index, claims := range probe.spoofed {
				if got := w.decide(call{token: w.approverTokenWith(
					"bob", claims)}, receipt); got.status != http.StatusUnauthorized {
					t.Fatalf("spoof %d on the approver branch = %d %s",
						index, got.status, got.raw)
				}
			}
			// The control approver carries the claim at the configured path.
			bobClaims := jwt.MapClaims{}
			for key := range probe.valid {
				if strings.Contains(key, ".") {
					bobClaims[key] = "oid-bob"
				} else {
					bobClaims[key] = map[string]any{"oid": "oid-bob"}
				}
			}
			w.mustDecide(call{token: w.approverTokenWith("bob", bobClaims)}, receipt)
		})
	}
}

// TestStableIdentityFormatCannotCollide: oidcid:<iss>#<tag>#<value> is injective in
// the value and disjoint from every sub-derived identity. A sub that is
// literally a stable identity does not make its holder that identity, an
// oid containing '#' names its own identity, and a sub-derived identity of
// any value never enters the stable namespace.
func TestStableIdentityFormatCannotCollide(t *testing.T) {
	t.Parallel()
	w := newStableWorld(t, []string{"oid"}, oid("oid-owner"))
	h := w.h
	alice := call{token: w.fleetToken("alice", oid("oid-alice"))}

	// An approver whose sub is literally alice's stable identity is named by
	// its own oid, not by its sub: it is not alice, and its identity is not
	// the sub it carried.
	impostor := w.approverTokenWith(string(w.stable("oid-alice")), oid("oid-mallory"))
	receipt := w.mustHold(alice, h.held("collide-sub"), "gateway")
	decided := w.mustDecide(call{token: impostor}, receipt)
	if decided.Approver != b64([]byte(w.stable("oid-mallory"))) {
		t.Fatalf("a stable-looking sub named the approver: %s", decided.raw)
	}

	// '#' in a value: oid-alice#x is not oid-alice, and an oid that is
	// literally alice's stable identity is not alice either.
	for index, value := range []string{"oid-alice#x", string(w.stable("oid-alice"))} {
		receipt := w.mustHold(alice, h.held("collide-hash-"+string(rune('a'+index))), "gateway")
		decided := w.mustDecide(call{token: w.approverTokenWith(
			"console", oid(value))}, receipt)
		if decided.Approver != b64([]byte(w.stable(value))) ||
			w.stable(value) == w.stable("oid-alice") {
			t.Fatalf("value %q decided as %s", value, decided.raw)
		}
	}

	// The sub-derived authenticator on the same issuer: a sub that is
	// literally a stable identity contains '#', which the sub-derived
	// namespace refuses outright (#546). One that begins like a stable
	// identity mints an oidc: identity, outside the stable namespace and
	// inside the legacy one the approval service refuses under the stable
	// scheme.
	legacy := newTestOIDCAuthenticator(t, w.issuer.testConfig(w.h.now))
	claims := w.issuer.defaultClaims(w.h.now())
	claims["sub"] = string(w.stable("oid-alice"))
	if _, err := legacy.Authenticate(bearerRequest(
		w.issuer.signRS256(t, testKID, claims))); err == nil {
		t.Fatal("a sub containing '#' was accepted")
	}
	claims["sub"] = oidcStableIdentityPrefix + w.issuer.server.URL
	decision, err := legacy.Authenticate(
		bearerRequest(w.issuer.signRS256(t, testKID, claims)))
	if err != nil {
		t.Fatal(err)
	}
	subject := string(decision.Subject())
	if strings.HasPrefix(subject, oidcStableIdentityPrefix) ||
		!strings.HasPrefix(subject, oidcIdentityPrefix+w.issuer.server.URL+"#") {
		t.Fatalf("a sub-derived identity entered the stable namespace: %q", subject)
	}
	approvals := w.authn.Load().identityScheme().approvals
	inFamily := false
	for _, namespace := range approvals.Family {
		if strings.HasPrefix(subject, namespace) {
			inFamily = true
		}
	}
	if !inFamily || strings.HasPrefix(subject, approvals.Prefix) {
		t.Fatalf("the sub-derived identity %q is not a foreign family member of %+v",
			subject, approvals)
	}

	// Two stable claim paths never share a namespace, so a value under one
	// can never be read as the same value under the other.
	paths := map[string][]string{
		"oid": {"oid"}, "uid": {"uid"}, "nested": {"ext", "oid"}, "dotted": {"ext.oid"},
	}
	seen := map[string]string{}
	for name, path := range paths {
		tag := claimPathTag(path)
		if len(tag) != 16 {
			t.Fatalf("claim path tag %q is not 16 hex digits", tag)
		}
		if previous, ok := seen[tag]; ok {
			t.Fatalf("%s and %s share a namespace", name, previous)
		}
		seen[tag] = name
	}
	if !strings.HasPrefix(string(w.stable("x")),
		oidcStableIdentityPrefix+w.issuer.server.URL+"#"+claimPathTag([]string{"oid"})+"#") {
		t.Fatalf("the stable identity does not carry its claim path tag: %s", w.stable("x"))
	}
}

func assertSchemeMoved(t *testing.T, name string, got answer) {
	t.Helper()
	if got.status != http.StatusConflict || got.Code != string(shoal.ErrorConflict) ||
		!strings.Contains(got.Message, "identity scheme that is no longer in force") {
		t.Fatalf("%s = %d %s, want identity_scheme_moved", name, got.status, got.raw)
	}
}

// schemeConfig is the authenticator configuration of one identity scheme:
// claim nil is the default, sub-derived scheme.
func (w *oidcApprovalWorld) schemeConfig(claim []string) (func(*oidcConfig), map[string]any) {
	if claim == nil {
		return nil, approverMappingDocument(w.issuer.server.URL)
	}
	return stableEdit(w.t, claim), stableMappingDocument(claim)(w.issuer.server.URL)
}

// authenticatorFor is a real authenticator on another scheme over the same
// issuer: what a replica configured for that scheme runs.
func (w *oidcApprovalWorld) authenticatorFor(claim []string) webapi.Authenticator {
	w.t.Helper()
	edit, document := w.schemeConfig(claim)
	config := approverTestConfig(w.t, w.issuer, w.h.now, document)
	if edit != nil {
		edit(&config)
	}
	return webapi.AuthenticatorFunc(newTestOIDCAuthenticator(w.t, config).Authenticate)
}

// switchScheme is the operator switching the issuer's identity scheme and
// restarting. With migrate the restart names the recorded scheme being
// replaced, as -oidc-identity-scheme-migrate does; it returns the open error.
func (w *oidcApprovalWorld) switchScheme(claim []string, migrate bool) error {
	w.t.Helper()
	previous := coordination.Digest(w.h.recorded.scheme.digest)
	w.edit, _ = w.schemeConfig(claim)
	w.migrateFrom = coordination.Digest{}
	if migrate {
		w.migrateFrom = previous
	}
	_, document := w.schemeConfig(claim)
	w.configure(document)
	w.h.close()
	return w.h.tryOpen()
}

func (w *oidcApprovalWorld) switchToStable(migrate bool) error {
	return w.switchScheme([]string{"oid"}, migrate)
}

// assertForeignNamespace is the namespace rule's refusal, naming the
// namespace and never the identity.
func assertForeignNamespace(t *testing.T, name string, got answer, namespace, hidden string) {
	t.Helper()
	if got.status != http.StatusUnauthorized ||
		!strings.Contains(got.Message, "in namespace "+namespace+",") {
		t.Fatalf("%s = %d %s, want the refusal naming %s", name, got.status, got.raw, namespace)
	}
	if hidden != "" && strings.Contains(string(got.raw), hidden) {
		t.Fatalf("%s: the refusal discloses the identity %q: %s", name, hidden, got.raw)
	}
}

// TestStableIdentitySwitchStrandsPreSwitchRequests: a request made under the
// sub-derived scheme was stamped with the legacy scheme (zero, which is also
// what every record written before the stamp decodes as). Under that scheme
// it is decidable as before; after the switch nobody can decide it
// (identity_scheme_moved), Pending skips it and an approver's Status does
// not show it. And the switch itself needs the migrate flag: a replica on
// another scheme refuses to start, in either direction.
func TestStableIdentitySwitchStrandsPreSwitchRequests(t *testing.T) {
	t.Parallel()
	w := newOIDCApprovalWorld(t, nil, oid("oid-owner"))
	h := w.h
	alice := call{token: w.fleetToken("alice", oid("oid-alice"))}
	stranded, control := h.held("pre-switch"), h.held("pre-switch-control")
	strandedReceipt := w.mustHold(alice, stranded, "gateway")
	controlReceipt := w.mustHold(alice, control, "gateway")
	if records := w.records(stranded.id); records[0].IdentityScheme != (auth.Digest{}) {
		t.Fatalf("a sub-derived request was stamped %x", records[0].IdentityScheme)
	}
	// Under the legacy scheme a zero-stamped pending request is decidable.
	w.mustDecide(call{token: w.approverTokenWith("bob", oid("oid-bob"))}, controlReceipt)
	legacyAuthenticator := w.authn.Load()
	legacyScheme := w.h.scheme

	// A replica configured for the stable claim, without migrate, refuses.
	if err := w.switchToStable(false); !errors.Is(err, errIdentitySchemeMismatch) {
		t.Fatalf("a replica on another scheme started: %v", err)
	}
	if err := w.switchToStable(true); err != nil {
		t.Fatalf("migrating the scheme: %v", err)
	}
	// Once recorded, a restart under the same scheme needs no flag — and one
	// left set, naming the scheme it replaced, is harmless on this scheme.
	h.reopen()
	w.migrateFrom = coordination.Digest{}
	w.configure(stableMappingDocument([]string{"oid"})(w.issuer.server.URL))
	h.reopen()

	bob := call{token: w.approverTokenWith("bob-console", oid("oid-bob"))}
	assertSchemeMoved(t, "deciding a pre-switch request", w.decide(bob, strandedReceipt))
	pending := w.post(bob, "/api/v1/fleet/approvals/pending", map[string]any{
		"context": w.contextWire(h.now().Add(time.Minute)), "limit": 16,
	})
	if pending.status != http.StatusOK || strings.Contains(string(pending.raw), b64(stranded.id)) {
		t.Fatalf("pending after the switch = %d %s", pending.status, pending.raw)
	}
	if got := w.post(bob, "/api/v1/fleet/approvals/"+b64(stranded.id)+"/status",
		w.contextWire(h.now().Add(time.Minute))); got.status != http.StatusNotFound {
		t.Fatalf("an approver's status of a stranded request = %d %s", got.status, got.raw)
	}
	// COUNTERFACTUAL: the requester's real decision with its pre-switch
	// subject, which no authenticator on this scheme mints — the only
	// principal Status answers as the requester of a pre-switch record.
	requester := w.post(call{token: w.fleetToken("alice", oid("oid-alice")),
		authn: w.counterfactual(func(config *auth.DecisionConfig) {
			config.Subject = w.identity("alice")
		})}, "/api/v1/fleet/approvals/"+b64(stranded.id)+"/status",
		w.contextWire(h.now().Add(time.Minute)))
	if requester.status != http.StatusOK ||
		requester.State != string(fleet.ApprovalUnresolvable) ||
		requester.Condition != string(fleet.ApprovalConditionIdentitySchemeMoved) ||
		requester.StoredState != string(fleet.ApprovalPending) {
		t.Fatalf("the requester's status = %d %s", requester.status, requester.raw)
	}

	// The previous replica — the sub-derived scheme — now refuses too.
	h.close()
	h.scheme = legacyScheme
	w.authn.Store(legacyAuthenticator)
	if err := h.tryOpen(); !errors.Is(err, errIdentitySchemeMismatch) {
		t.Fatalf("a replica on the previous scheme started: %v", err)
	}
}

// TestStableIdentityLegacyRegistrationBlocksApproval: an agent registered
// under the sub-derived scheme names its registrant oidc:<iss>#<sub>, an
// identity no stable approver can be compared with — the registrant may be
// the approver under oidcid:. Approval is refused, naming the namespace and
// not the identity, until adoption (#526 PR2). An agent registered under the
// stable scheme is approvable, and so is one registered by a principal
// outside the OIDC family, so the refusal is the namespace's.
func TestStableIdentityLegacyRegistrationBlocksApproval(t *testing.T) {
	t.Parallel()
	w := newOIDCApprovalWorld(t, nil, oid("oid-owner"))
	h := w.h
	if err := w.switchToStable(true); err != nil {
		t.Fatal(err)
	}
	alice := call{token: w.fleetToken("alice", oid("oid-alice"))}
	receipt := w.mustHold(alice, h.held("legacy-agent"), "gateway")
	bob := call{token: w.approverTokenWith("bob", oid("oid-bob"))}
	assertForeignNamespace(t, "approving work on a legacy registration",
		w.decide(bob, receipt), oidcIdentityPrefix+w.issuer.server.URL+"#", "owner")

	// Control: an agent registered under the stable scheme.
	w.register(w.fleetToken("carol", oid("oid-carol")), "stable-agent", "")
	w.mustDecide(bob, w.mustHold(alice, h.held("stable-agent-request"), "stable-agent"))

	// Outside the family: a service principal's registration ("owner" /
	// "operator", not an OIDC identity) is not this rule's business, and
	// neither is an executor identity (#391's oidcexec: — not human, and
	// deliberately not in the family although it begins with "oidc").
	if _, err := h.register("service-agent", "service-registration", 0, true, ""); err != nil {
		t.Fatal(err)
	}
	w.mustDecide(bob, w.mustHold(alice, h.held("service-agent-request"), "service-agent"))
	executor := principal{
		subject: shoal.ID("oidcexec:" + w.issuer.server.URL + "#runner-1"),
		actor:   shoal.ID("oidcexec:" + w.issuer.server.URL + "#runner-1"),
		operations: []auth.Operation{
			auth.OperationAgentRegister, auth.OperationDelegate,
		},
	}
	if _, err := h.opened.fleetRegistry.Register(h.as(executor), fleet.RegisterRequest{
		Context:         h.context(h.now().Add(time.Minute)),
		RegistrationKey: "executor-registration",
		Spec: fleet.Spec{
			ID: "executor-agent", AuthorizationDomain: workspaceAuthorizationDomain,
			Scopes: []fleet.Scope{
				{SourceID: workspaceSourceID, PolicyID: workspaceGrantPolicyID},
			},
			ExecutorRef: "local", Capabilities: approvalActions(true),
			LeaseExpiresAt: h.now().Add(20 * time.Hour),
		},
	}); err != nil {
		t.Fatal(err)
	}
	w.mustDecide(bob, w.mustHold(alice, h.held("executor-agent-request"), "executor-agent"))
}

// TestStableIdentityStableToStableSwitchIsRefused is review finding (a):
// stable ["oid"] then stable ["uid"]. Bob registered his agent under oid;
// after the switch he approves as his uid. Each stable claim path has a
// namespace of its own, and only the one in force is comparable, so this is
// refused — naming oidcid:<iss>#, never Bob.
func TestStableIdentityStableToStableSwitchIsRefused(t *testing.T) {
	t.Parallel()
	both := func(subject string) jwt.MapClaims {
		return jwt.MapClaims{"oid": "oid-" + subject, "uid": "uid-" + subject}
	}
	w := newStableWorld(t, []string{"oid"}, both("owner"))
	h := w.h
	w.register(w.fleetToken("bob-ws", both("bob")), "bobs-agent", "")
	if err := w.switchScheme([]string{"uid"}, true); err != nil {
		t.Fatal(err)
	}
	alice := call{token: w.fleetToken("alice-ws", both("alice"))}
	bob := call{token: w.approverTokenWith("bob-console", both("bob"))}
	receipt := w.mustHold(alice, h.held("oid-to-uid"), "bobs-agent")
	assertForeignNamespace(t, "self-approval across stable claim paths",
		w.decide(bob, receipt), oidcStableIdentityPrefix+w.issuer.server.URL+"#", "bob")

	// Control: the same approver on an agent registered under uid.
	w.register(w.fleetToken("carol-ws", both("carol")), "carols-agent", "")
	w.mustDecide(bob, w.mustHold(alice, h.held("uid-control"), "carols-agent"))
}

// TestStableIdentityStableToDefaultSwitchIsRefused is review finding (b):
// stable ["oid"] back to the default sub-derived scheme. The default scheme
// is held to the same rule: an oidcid: registrant is foreign to it.
func TestStableIdentityStableToDefaultSwitchIsRefused(t *testing.T) {
	t.Parallel()
	w := newStableWorld(t, []string{"oid"}, oid("oid-owner"))
	h := w.h
	w.register(w.fleetToken("bob", oid("oid-bob")), "bobs-agent", "")
	// The default scheme needs public subjects again.
	w.issuer.mu.Lock()
	w.issuer.subjectTypes = []string{"public"}
	w.issuer.mu.Unlock()
	if err := w.switchScheme(nil, true); err != nil {
		t.Fatal(err)
	}
	alice := call{token: w.fleetToken("alice", nil)}
	bob := call{token: w.approverToken("bob")}
	receipt := w.mustHold(alice, h.held("stable-to-sub"), "bobs-agent")
	assertForeignNamespace(t, "self-approval after switching back to sub",
		w.decide(bob, receipt), oidcStableIdentityPrefix+w.issuer.server.URL+"#", "oid-bob")

	w.register(w.fleetToken("carol", nil), "carols-agent", "")
	// The approver is held to the namespace too: Bob through an authenticator
	// still on the stable scheme, on work otherwise wholly in the default
	// namespace, is refused. (The stamp cannot catch this: the default scheme
	// does not stamp.)
	controlReceipt := w.mustHold(alice, h.held("sub-control"), "carols-agent")
	assertForeignNamespace(t, "an approver minted under another scheme",
		w.decide(call{token: w.approverTokenWith("bob", oid("oid-bob")),
			authn: w.authenticatorFor([]string{"oid"})}, controlReceipt),
		oidcStableIdentityPrefix+w.issuer.server.URL+"#", "oid-bob")
	w.mustDecide(bob, controlReceipt)
}

// TestStableIdentityMixedRolloutFailsClosed is review finding (c): the row is
// checked only at startup, so during a rollout a replica on the old scheme
// and one on the new serve the same store. Two real authenticators on
// different schemes act against one store — one as registrant, the other as
// approver — with the service first on the old scheme (the old replica's
// view) and then on the new (the new replica's). Both views refuse.
func TestStableIdentityMixedRolloutFailsClosed(t *testing.T) {
	t.Parallel()
	both := func(subject string) jwt.MapClaims {
		return jwt.MapClaims{"oid": "oid-" + subject, "uid": "uid-" + subject}
	}
	w := newStableWorld(t, []string{"oid"}, both("owner"))
	h := w.h
	oldReplica := w.authenticatorFor([]string{"oid"})
	newReplica := w.authenticatorFor([]string{"uid"})
	family := oidcStableIdentityPrefix + w.issuer.server.URL + "#"

	// The old replica's view: Bob registered through the new replica, and
	// approves through the old one under his oid.
	w.registerWith(call{token: w.fleetToken("bob-ws", both("bob")), authn: newReplica},
		"bobs-new-agent", "")
	receipt := w.mustHold(call{token: w.fleetToken("alice-ws", both("alice")),
		authn: oldReplica}, h.held("mixed-old"), "bobs-new-agent")
	assertForeignNamespace(t, "the old replica, a registrant from the new",
		w.decide(call{token: w.approverTokenWith("bob-console", both("bob")),
			authn: oldReplica}, receipt), family, "bob")

	// The new replica's view: Bob registered through the old replica, and
	// approves through the new one under his uid.
	w.registerWith(call{token: w.fleetToken("bob-ws", both("bob")), authn: oldReplica},
		"bobs-old-agent", "")
	if err := w.switchScheme([]string{"uid"}, true); err != nil {
		t.Fatal(err)
	}
	receipt = w.mustHold(call{token: w.fleetToken("alice-ws", both("alice")),
		authn: newReplica}, h.held("mixed-new"), "bobs-old-agent")
	assertForeignNamespace(t, "the new replica, a registrant from the old",
		w.decide(call{token: w.approverTokenWith("bob-console", both("bob")),
			authn: newReplica}, receipt), family, "bob")
	// And a request the old replica made is stamped with the old scheme, so
	// the new replica cannot decide it at all.
	assertSchemeMoved(t, "an old replica's request on the new replica",
		w.decide(call{token: w.approverTokenWith("carol-console", both("carol")),
			authn: newReplica}, w.mustHold(call{
			token: w.fleetToken("alice-ws", both("alice")), authn: oldReplica},
			h.held("mixed-stamp"), "bobs-old-agent")))
}

// TestStableIdentityIssuerChangeIsASchemeSwitch is review round 2: changing
// -oidc-issuer (Entra v1 to v2, a Keycloak hostname move) keeps oid or the
// user id but renames every human. The new issuer finds the previous one's
// record and refuses to start without the one-shot migrate; after the switch,
// a registrant under the old issuer approving under the new one is refused,
// naming the old issuer's namespace and never the identity — under the
// default scheme and under a stable claim alike.
func TestStableIdentityIssuerChangeIsASchemeSwitch(t *testing.T) {
	t.Parallel()
	for _, probe := range []struct {
		name  string
		claim []string
	}{
		{"sub-derived", nil},
		{"stable oid", []string{"oid"}},
	} {
		t.Run(probe.name, func(t *testing.T) {
			var w *oidcApprovalWorld
			if probe.claim == nil {
				w = newOIDCApprovalWorld(t, nil, oid("oid-owner"))
			} else {
				w = newStableWorld(t, probe.claim, oid("oid-owner"))
			}
			h := w.h
			w.register(w.fleetToken("bob", oid("oid-bob")), "bobs-agent", "")
			previous := w.issuer.server.URL
			family := oidcIdentityPrefix
			if probe.claim != nil {
				family = oidcStableIdentityPrefix
			}

			// The new issuer: same humans, same oid and sub values.
			w.issuer = newFakeOIDCIssuer(t)
			if err := w.switchScheme(probe.claim, false); !errors.Is(err, errIdentitySchemeMismatch) ||
				!strings.Contains(err.Error(), "changing the issuer is a scheme change") {
				t.Fatalf("a new issuer started without migrate: %v", err)
			}
			if err := w.switchScheme(probe.claim, true); err != nil {
				t.Fatalf("the issuer switch with migrate: %v", err)
			}

			alice := call{token: w.fleetToken("alice", oid("oid-alice"))}
			bob := call{token: w.approverTokenWith("bob", oid("oid-bob"))}
			receipt := w.mustHold(alice, h.held("issuer-change"), "bobs-agent")
			assertForeignNamespace(t, "self-approval across an issuer change",
				w.decide(bob, receipt), family+previous+"#", "#bob")

			// Control: an agent registered under the new issuer.
			w.register(w.fleetToken("carol", oid("oid-carol")), "carols-agent", "")
			w.mustDecide(bob, w.mustHold(alice, h.held("issuer-control"), "carols-agent"))
		})
	}
}
