// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

// Tests for #546: a non-default -oidc-subject-claim has a namespace of its
// own, oidc:<iss>#<claim tag>#<value>, and the sub-derived namespace
// oidc:<iss># holds no value containing '#'. As in oidc_approver_e2e_test.go
// every request is a signed token sent over real HTTP and authenticated by
// the real OIDC authenticator. A non-default subject claim refuses the
// approver mapping (#523), so no shipped authenticator mints an approver
// under one; where a test needs one, it is the labelled counterfactual: the
// real authenticator's workspace decision for that human, with approve as
// its only operation.

import (
	"bytes"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// pinnedIssuer is the issuer the pre-#546 literals below were computed for,
// by the code this change replaces.
const pinnedIssuer = "https://issuer.example/realms/a"

const (
	// preDefaultDigest is the default (sub) scheme's digest before #546.
	preDefaultDigest = "1e6a20788f5f7a7c6e51f13c94ceb9048fd8f2dd985a3d2c8cbfb21f04af1968"
	// preOidDigest is -oidc-subject-claim oid's digest before #546, when
	// it minted oidc:<iss>#<oid>.
	preOidDigest = "3b9fbebe3b7413339fc2dca339583e9acde7cbd9753277f5c4e993c0ebccaf33"
	// preLegacyEntraDigest is the legacy Entra mode's digest before #546.
	preLegacyEntraDigest = "26eb67c317f03e68e3e39c9a35f83d606b5d27f604750e6eaf26c71884116ee0"
)

func subjectClaimEdit(claim string) func(*oidcConfig) {
	if claim == "sub" {
		return nil
	}
	return func(config *oidcConfig) { config.subjectClaim = claim }
}

// subjectClaimDocument is the approver mapping for a subject claim: the
// default one under sub, and none under any other claim, which refuses it.
func (w *oidcApprovalWorld) subjectClaimDocument(claim string) map[string]any {
	if claim == "sub" {
		return approverMappingDocument(w.issuer.server.URL)
	}
	return nil
}

// newSubjectClaimWorld is an approval world on -oidc-subject-claim claim,
// with no approver mapping.
func newSubjectClaimWorld(
	t *testing.T, claim string, registrant jwt.MapClaims,
) *oidcApprovalWorld {
	t.Helper()
	return newOIDCApprovalWorldWith(t, subjectClaimEdit(claim), registrant, nil,
		func(string) map[string]any { return nil })
}

// subjectClaimNamespace is oidc:<iss>#<claim tag>#.
func (w *oidcApprovalWorld) subjectClaimNamespace(claim string) string {
	return oidcIdentityPrefix + w.issuer.server.URL + "#" + subjectClaimTag(claim) + "#"
}

// subjectClaimReplica is a real authenticator on another subject claim over
// the same issuer: what a replica configured for it runs.
func (w *oidcApprovalWorld) subjectClaimReplica(claim string) webapi.Authenticator {
	w.t.Helper()
	config := approverTestConfig(w.t, w.issuer, w.h.now, w.subjectClaimDocument(claim))
	if edit := subjectClaimEdit(claim); edit != nil {
		edit(&config)
	}
	return webapi.AuthenticatorFunc(newTestOIDCAuthenticator(w.t, config).Authenticate)
}

// switchSubjectClaim is the operator changing -oidc-subject-claim and
// restarting, with -oidc-identity-scheme-migrate naming the recorded scheme
// when migrate is set; it returns the open error.
func (w *oidcApprovalWorld) switchSubjectClaim(claim string, migrate bool) error {
	w.t.Helper()
	previous := coordination.Digest(w.h.recorded.scheme.digest)
	w.edit = subjectClaimEdit(claim)
	w.migrateFrom = coordination.Digest{}
	if migrate {
		w.migrateFrom = previous
	}
	w.configure(w.subjectClaimDocument(claim))
	w.h.close()
	return w.h.tryOpen()
}

// workspaceApprover is the COUNTERFACTUAL approver under a non-default
// subject claim: the real authenticator's decision for the workspace token,
// named exactly as the scheme in force names that human, holding approve
// and nothing else, with actor = subject. Two fields changed: the
// operations and the actor.
func (w *oidcApprovalWorld) workspaceApprover(subject string, extra jwt.MapClaims) call {
	return call{token: w.fleetToken(subject, extra), authn: w.counterfactual(
		func(config *auth.DecisionConfig) {
			config.AllowedOperations = append([]auth.Operation(nil),
				oidcApproverOperations...)
			// Actor = subject, as every minted approver has: the
			// workspace branch's shared default actor overlaps every
			// requester.
			config.Actor = config.Subject
		})}
}

func pinnedScheme(t *testing.T, edit func(*oidcConfig)) (*oidcAuthenticator, oidcIdentityScheme) {
	t.Helper()
	issuer := newFakeOIDCIssuer(t)
	config := issuer.testConfig(fixedClock(time.Now()))
	config.issuer = pinnedIssuer
	if edit != nil {
		edit(&config)
	}
	authenticator, err := newOIDCAuthenticator(config, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return authenticator, authenticator.identityScheme()
}

// TestSubjectClaimDefaultSchemeIsUnchanged: the default sub deployment and
// the legacy Entra mode keep the scheme digest the previous release
// recorded (an old-code literal), so they start without a migrate flag, and
// keep their stamps and namespaces. A non-default claim's digest moves, and
// the one it had is recognised.
func TestSubjectClaimDefaultSchemeIsUnchanged(t *testing.T) {
	t.Parallel()
	_, standard := pinnedScheme(t, nil)
	if got := hex.EncodeToString(standard.digest[:]); got != preDefaultDigest {
		t.Fatalf("the default scheme digest moved: %s, want %s", got, preDefaultDigest)
	}
	if standard.approvals.Digest != (auth.Digest{}) ||
		standard.approvals.Prefix != oidcIdentityPrefix+pinnedIssuer+"#" ||
		!standard.approvals.Flat || standard.sharedNamespaceDigest != (auth.Digest{}) {
		t.Fatalf("the default scheme tells approvals %+v", standard.approvals)
	}
	_, legacy := pinnedScheme(t, func(c *oidcConfig) {
		c.subjectClaim = "oid"
		c.subjectFallbackClaim = "sub"
		c.identityPrefix = legacyEntraPrefix
		c.trimIdentityValues = true
	})
	if got := hex.EncodeToString(legacy.digest[:]); got != preLegacyEntraDigest {
		t.Fatalf("the legacy Entra scheme digest moved: %s", got)
	}
	if legacy.approvals.Digest != (auth.Digest{}) || legacy.approvals.Prefix != legacyEntraPrefix ||
		legacy.approvals.Flat {
		t.Fatalf("the legacy Entra scheme tells approvals %+v", legacy.approvals)
	}
	authenticator, claimed := pinnedScheme(t, subjectClaimEdit("oid"))
	if got := hex.EncodeToString(claimed.digest[:]); got == preOidDigest {
		t.Fatal("-oidc-subject-claim oid kept the digest of the shared namespace")
	}
	if got := hex.EncodeToString(claimed.sharedNamespaceDigest[:]); got != preOidDigest {
		t.Fatalf("the pre-#546 digest of the oid claim is %s, want %s", got, preOidDigest)
	}
	namespace := oidcIdentityPrefix + pinnedIssuer + "#" + subjectClaimTag("oid") + "#"
	if claimed.approvals.Prefix != namespace || claimed.approvals.Flat ||
		claimed.approvals.Digest != claimed.digest || authenticator.identityPrefix != namespace {
		t.Fatalf("the oid subject claim tells approvals %+v", claimed.approvals)
	}
	_, other := pinnedScheme(t, subjectClaimEdit("uid"))
	if other.digest == claimed.digest || other.approvals.Prefix == claimed.approvals.Prefix {
		t.Fatal("two subject claims share a scheme")
	}

	// Through the real authenticator: sub names oidc:<iss>#<sub>, as before.
	issuer := newFakeOIDCIssuer(t)
	now := time.Now()
	decision, err := newTestOIDCAuthenticator(t, issuer.testConfig(fixedClock(now))).
		Authenticate(bearerRequest(issuer.signRS256(t, testKID, issuer.defaultClaims(now))))
	if err != nil {
		t.Fatal(err)
	}
	if want := shoal.ID("oidc:" + issuer.server.URL + "#" + testSubject); decision.Subject() != want {
		t.Fatalf("the default subject is %q, want %q", decision.Subject(), want)
	}
}

// TestSubjectClaimMintsItsOwnNamespace: -oidc-subject-claim X names a
// principal oidc:<iss>#<tag>#<value>, where the tag is 16 hex digits of a
// digest of X, and a value containing '#' stays one value in X's namespace.
func TestSubjectClaimMintsItsOwnNamespace(t *testing.T) {
	t.Parallel()
	issuer := newFakeOIDCIssuer(t)
	now := time.Now()
	config := issuer.testConfig(fixedClock(now))
	config.subjectClaim = "oid"
	authenticator := newTestOIDCAuthenticator(t, config)
	namespace := "oidc:" + issuer.server.URL + "#" + subjectClaimTag("oid") + "#"
	for _, value := range []string{"oid-alice", "a#b", "#", subjectClaimTag("uid") + "#x"} {
		claims := issuer.defaultClaims(now)
		claims["oid"] = value
		decision, err := authenticator.Authenticate(
			bearerRequest(issuer.signRS256(t, testKID, claims)))
		if err != nil {
			t.Fatalf("oid %q: %v", value, err)
		}
		if decision.Subject() != shoal.ID(namespace+value) {
			t.Fatalf("oid %q named %q, want %q", value, decision.Subject(), namespace+value)
		}
	}
	// The tag: 16 lowercase hex digits, distinct per claim, whatever the
	// claim name contains.
	seen := map[string]string{}
	for _, claim := range []string{"oid", "uid", "a#b", "a:b", "oid#", "sub "} {
		tag := subjectClaimTag(claim)
		if len(tag) != 16 || strings.Trim(tag, "0123456789abcdef") != "" {
			t.Fatalf("the tag of %q is %q", claim, tag)
		}
		if previous, ok := seen[tag]; ok {
			t.Fatalf("%q and %q share a tag", claim, previous)
		}
		seen[tag] = claim
	}
}

// TestSubjectClaimPrefixIsUnambiguous: under the sub-derived namespace a
// '#' in a sub, on either branch, or in an actor value, is refused — with it,
// sub "<tag>#<value>" would mint exactly the identity the claim's scheme
// gives <value>. And the approval rule reads the namespaces apart: under
// sub, a claim-tagged identity is foreign; under a claim, a sub identity is.
func TestSubjectClaimPrefixIsUnambiguous(t *testing.T) {
	t.Parallel()
	issuer := newFakeOIDCIssuer(t)
	now := time.Now()
	config := approverTestConfig(t, issuer, fixedClock(now),
		approverMappingDocument(issuer.server.URL))
	config.actorClaim = "act_as"
	authenticator := newTestOIDCAuthenticator(t, config)
	collision := subjectClaimTag("oid") + "#oid-alice"
	workspace := func(sub, actor string) error {
		claims := issuer.defaultClaims(now)
		claims["sub"], claims["act_as"] = sub, actor
		_, err := authenticator.Authenticate(bearerRequest(issuer.signRS256(t, testKID, claims)))
		return err
	}
	for _, sub := range []string{collision, "a#b", "#", "x#"} {
		if err := workspace(sub, "service"); err == nil {
			t.Errorf("workspace sub %q was accepted", sub)
		}
		if _, err := authenticator.Authenticate(bearerRequest(issuer.signRS256(
			t, testKID, approverClaims(issuer, now, sub)))); err == nil {
			t.Errorf("approver sub %q was accepted", sub)
		}
	}
	if err := workspace("alice", collision); err == nil {
		t.Error("an actor value containing '#' was accepted")
	}
	// Controls: the same tokens without '#'.
	if err := workspace("alice", "service"); err != nil {
		t.Fatalf("a plain workspace token was refused: %v", err)
	}
	if _, err := authenticator.Authenticate(bearerRequest(issuer.signRS256(
		t, testKID, approverClaims(issuer, now, "bob")))); err != nil {
		t.Fatalf("a plain approver token was refused: %v", err)
	}

	// The namespaces as the approval service is told them: the claim's lies
	// inside sub's as a string, so sub's must be flat, and a sub identity
	// lies outside the claim's. (The rule itself: fleet's
	// TestIdentitySchemeFlatNamespace, and the mixed-rollout test below.)
	standard := authenticator.identityScheme().approvals
	claimConfig := issuer.testConfig(fixedClock(now))
	claimConfig.subjectClaim = "oid"
	oidScheme := newTestOIDCAuthenticator(t, claimConfig).identityScheme().approvals
	if !strings.HasPrefix(oidScheme.Prefix, standard.Prefix) || !standard.Flat ||
		strings.Contains(oidScheme.Prefix[len(standard.Prefix):], "#") == false {
		t.Fatalf("sub's namespace %+v does not exclude the claim's %+v", standard, oidScheme)
	}
	if strings.HasPrefix(standard.Prefix+"alice", oidScheme.Prefix) || oidScheme.Flat {
		t.Fatalf("a sub identity lies in the claim's namespace %+v", oidScheme)
	}
}

// TestHashRefusalIsLoggedByClaim: a '#' refusal is a 401 like any malformed
// claim, and an issuer whose client-ID or actor claim routinely carries '#'
// would refuse every user with nothing at startup to say why. So the first
// refusal for each claim is logged, naming the claim and never the value,
// and then at most once per claim per interval — on both branches.
func TestHashRefusalIsLoggedByClaim(t *testing.T) {
	t.Parallel()
	issuer := newFakeOIDCIssuer(t)
	now := time.Now()
	config := approverTestConfig(t, issuer, fixedClock(now),
		approverMappingDocument(issuer.server.URL))
	config.actorClaim = "act_as"
	config.clientIDClaim = "client_ref"
	authenticator := newTestOIDCAuthenticator(t, config)
	var logs bytes.Buffer
	var clock atomic.Int64
	clock.Store(now.UnixNano())
	authenticator.hashRefusals = newHashRefusalLog(
		slog.New(slog.NewTextHandler(&logs, nil)),
		func() time.Time { return time.Unix(0, clock.Load()) })
	workspace := func(sub, actor, client string) {
		t.Helper()
		claims := issuer.defaultClaims(now)
		claims["sub"], claims["act_as"], claims["client_ref"] = sub, actor, client
		if _, err := authenticator.Authenticate(bearerRequest(
			issuer.signRS256(t, testKID, claims))); err == nil {
			t.Fatalf("a '#' in sub %q, actor %q or client %q was accepted", sub, actor, client)
		}
	}
	lines := func() []string {
		return strings.FieldsFunc(logs.String(), func(r rune) bool { return r == '\n' })
	}

	workspace("alice", "svc", "secret-client#1")
	workspace("alice", "svc", "secret-client#2")
	workspace("bob", "svc", "secret-client#3")
	if got := lines(); len(got) != 1 || !strings.Contains(got[0], "claim=client_ref") {
		t.Fatalf("after three client-ID refusals the log is %q, want one line naming client_ref", got)
	}
	workspace("alice", "secret-actor#1", "client")
	workspace("secret-sub#1", "svc", "client")
	if _, err := authenticator.Authenticate(bearerRequest(issuer.signRS256(
		t, testKID, approverClaims(issuer, now, "secret-approver#1")))); err == nil {
		t.Fatal("an approver sub containing '#' was accepted")
	}
	got := lines()
	if len(got) != 3 || !strings.Contains(got[1], "claim=act_as") ||
		!strings.Contains(got[2], "claim=sub") {
		t.Fatalf("one line per claim, the approver's sub sharing sub's: %q", got)
	}
	clock.Add(int64(hashRefusalInterval))
	workspace("alice", "svc", "secret-client#4")
	if got := lines(); len(got) != 4 || !strings.Contains(got[3], "claim=client_ref") {
		t.Fatalf("after the interval the client-ID refusal is not logged again: %q", got)
	}
	if strings.Contains(logs.String(), "secret") {
		t.Fatalf("the log discloses a claim value: %s", logs.String())
	}
	// Control: no '#', no log line.
	claims := issuer.defaultClaims(now)
	claims["act_as"], claims["client_ref"] = "svc", "client"
	if _, err := authenticator.Authenticate(bearerRequest(
		issuer.signRS256(t, testKID, claims))); err != nil {
		t.Fatal(err)
	}
	if len(lines()) != 4 {
		t.Fatalf("an accepted token was logged: %q", lines())
	}
}

// TestSubjectClaimSwitchFromSubRefusesSelfApproval: Bob registered his
// agent under sub; the deployment switches to -oidc-subject-claim oid. The
// switch needs the migrate flag, and afterwards Bob, now named by his oid,
// cannot approve work on the agent he registered as oidc:<iss>#<sub> — the
// refusal names oidc:<iss># and not Bob. A request made under sub is
// stranded. Control: an agent registered under the oid claim.
func TestSubjectClaimSwitchFromSubRefusesSelfApproval(t *testing.T) {
	t.Parallel()
	w := newOIDCApprovalWorld(t, nil, oid("oid-owner"))
	h := w.h
	alice := call{token: w.fleetToken("alice", oid("oid-alice"))}
	w.register(w.fleetToken("bob", oid("oid-bob")), "bobs-agent", "")
	stranded := w.mustHold(alice, h.held("sub-request"), "gateway")

	if err := w.switchSubjectClaim("oid", false); !errors.Is(err, errIdentitySchemeMismatch) {
		t.Fatalf("a replica on the oid claim started without migrate: %v", err)
	}
	if err := w.switchSubjectClaim("oid", true); err != nil {
		t.Fatal(err)
	}
	alice = call{token: w.fleetToken("alice", oid("oid-alice"))}
	bob := w.workspaceApprover("bob", oid("oid-bob"))
	receipt := w.mustHold(alice, h.held("sub-to-oid"), "bobs-agent")
	assertForeignNamespace(t, "self-approval after switching sub to oid",
		w.decide(bob, receipt), oidcIdentityPrefix+w.issuer.server.URL+"#", "bob")
	assertSchemeMoved(t, "a request made under sub", w.decide(bob, stranded))

	w.register(w.fleetToken("carol", oid("oid-carol")), "carols-agent", "")
	decided := w.mustDecide(bob, w.mustHold(alice, h.held("oid-control"), "carols-agent"))
	if decided.Approver != b64([]byte(w.subjectClaimNamespace("oid")+"oid-bob")) {
		t.Fatalf("the approver is not named in the oid namespace: %s", decided.raw)
	}
}

// TestSubjectClaimSwitchBetweenClaimsRefusesSelfApproval: from
// -oidc-subject-claim oid to uid. Each claim has a namespace of its own, so
// Bob's oid registration is foreign under uid.
func TestSubjectClaimSwitchBetweenClaimsRefusesSelfApproval(t *testing.T) {
	t.Parallel()
	both := func(subject string) jwt.MapClaims {
		return jwt.MapClaims{"oid": "oid-" + subject, "uid": "uid-" + subject}
	}
	w := newSubjectClaimWorld(t, "oid", both("owner"))
	h := w.h
	w.register(w.fleetToken("bob-ws", both("bob")), "bobs-agent", "")
	alice := call{token: w.fleetToken("alice-ws", both("alice"))}
	stranded := w.mustHold(alice, h.held("oid-request"), "gateway")
	oidDigest := w.authn.Load().identityScheme().digest
	if err := w.switchSubjectClaim("uid", false); !errors.Is(err, errIdentitySchemeMismatch) {
		t.Fatalf("a replica on the uid claim started without migrate: %v", err)
	}
	if err := w.switchSubjectClaim("uid", true); err != nil {
		t.Fatal(err)
	}
	alice = call{token: w.fleetToken("alice-ws", both("alice"))}
	bob := w.workspaceApprover("bob-console", both("bob"))
	receipt := w.mustHold(alice, h.held("oid-to-uid"), "bobs-agent")
	assertForeignNamespace(t, "self-approval across subject claims",
		w.decide(bob, receipt), oidcIdentityPrefix+w.issuer.server.URL+"#", "bob")

	w.register(w.fleetToken("carol-ws", both("carol")), "carols-agent", "")
	w.mustDecide(bob, w.mustHold(alice, h.held("uid-control"), "carols-agent"))

	// A request made under the oid claim was stamped with that scheme, and
	// cannot be decided under uid.
	if records := w.records([]byte("oid-request")); len(records) == 0 ||
		records[0].IdentityScheme != oidDigest {
		t.Fatal("a request under a subject claim was not stamped with its scheme")
	}
	assertSchemeMoved(t, "a request made under the oid claim", w.decide(bob, stranded))
}

// TestSubjectClaimMixedRolloutFailsClosed is the switch from sub to a
// claim seen from a replica still on sub, with no counterfactual: Bob
// registers through a replica already on the oid claim, and approves
// through the sub replica's real approver mapping. The claim's namespace
// lies inside oidc:<iss># as a string, so only the flat rule keeps it out.
func TestSubjectClaimMixedRolloutFailsClosed(t *testing.T) {
	t.Parallel()
	w := newOIDCApprovalWorld(t, nil, oid("oid-owner"))
	h := w.h
	newReplica := w.subjectClaimReplica("oid")
	w.registerWith(call{token: w.fleetToken("bob-ws", oid("oid-bob")), authn: newReplica},
		"bobs-new-agent", "")
	alice := call{token: w.fleetToken("alice", oid("oid-alice"))}
	bob := call{token: w.approverToken("bob")}
	receipt := w.mustHold(alice, h.held("mixed"), "bobs-new-agent")
	assertForeignNamespace(t, "the sub replica, a registrant from the oid replica",
		w.decide(bob, receipt), oidcIdentityPrefix+w.issuer.server.URL+"#<nested>#", "oid-bob")

	w.register(w.fleetToken("carol", oid("oid-carol")), "carols-agent", "")
	w.mustDecide(bob, w.mustHold(alice, h.held("mixed-control"), "carols-agent"))
}

// TestSubjectClaimUpgradeRefusesToStartWithoutMigrate: a deployment that
// ran -oidc-subject-claim oid under the previous release recorded that
// release's scheme, in the namespace of sub. Upgraded with the same flags it
// refuses to start, explaining that this release moved the claim's
// namespace and naming the digest to migrate from; with the flag it starts,
// and the identities the previous release minted are refused in approvals.
func TestSubjectClaimUpgradeRefusesToStartWithoutMigrate(t *testing.T) {
	t.Parallel()
	w := newSubjectClaimWorld(t, "oid", oid("oid-owner"))
	h := w.h
	current := w.authn.Load().identityScheme()

	// The previous release's record: its digest for this configuration,
	// written by the one-shot migrate as a replica of that release would
	// have written it on first start.
	h.close()
	h.scheme = &identitySchemeConfig{
		scheme: oidcIdentityScheme{
			issuer: w.issuer.server.URL, digest: current.sharedNamespaceDigest,
		},
		migrateFrom: coordination.Digest(current.digest),
	}
	h.open()
	// Bob's agent as the previous release registered it: it named oid
	// oid-bob oidc:<iss>#oid-bob, the bytes a sub of oid-bob gives today.
	w.registerWith(call{token: w.fleetToken("oid-bob", nil),
		authn: w.subjectClaimReplica("sub")}, "bobs-old-agent", "")

	h.close()
	w.migrateFrom = coordination.Digest{}
	w.configure(nil)
	err := h.tryOpen()
	old := hex.EncodeToString(current.sharedNamespaceDigest[:])
	if !errors.Is(err, errIdentitySchemeMismatch) ||
		!strings.Contains(err.Error(), "#546") ||
		!strings.Contains(err.Error(), "-oidc-identity-scheme-migrate="+old) {
		t.Fatalf("the upgraded replica's refusal = %v", err)
	}
	w.migrateFrom = coordination.Digest(current.sharedNamespaceDigest)
	w.configure(nil)
	h.open()

	alice := call{token: w.fleetToken("alice", oid("oid-alice"))}
	bob := w.workspaceApprover("bob", oid("oid-bob"))
	receipt := w.mustHold(alice, h.held("upgraded"), "bobs-old-agent")
	assertForeignNamespace(t, "a registration from before the upgrade",
		w.decide(bob, receipt), oidcIdentityPrefix+w.issuer.server.URL+"#", "oid-bob")
	w.mustDecide(bob, w.mustHold(alice, h.held("upgraded-control"), "gateway"))
}
