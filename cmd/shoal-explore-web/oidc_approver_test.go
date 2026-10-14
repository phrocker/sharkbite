// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	testApproverAudience = "shoal-approvals"
	testApproverClient   = "shoal-console"
	testApproverValue    = "shoal-approvers"
	// testHumanClaim is a user-only claim, such as an Auth0 post-login
	// Action adds and a client-credentials token never carries.
	testHumanClaim = "shoal_principal_type"
	testHumanValue = "human"
)

// approverMappingDocument is the valid mapping the tests start from.
func approverMappingDocument(issuer string) map[string]any {
	return map[string]any{
		"version":    approverMappingVersion,
		"issuer":     issuer,
		"audience":   testApproverAudience,
		"client_ids": []string{testApproverClient},
		"claim":      []string{"realm_access", "roles"},
		"values":     []string{testApproverValue},
		"max_values": 8,
		"human_assertion": map[string]any{
			"claim": []string{testHumanClaim}, "equals": testHumanValue,
		},
	}
}

func writeApproverMapping(t *testing.T, document any) string {
	t.Helper()
	raw, ok := document.([]byte)
	if !ok {
		var err error
		raw, err = json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "approvers.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// approverTestConfig is the issuer's test configuration with a fleet mapping
// and the approver mapping file.
func approverTestConfig(
	t *testing.T, issuer *fakeOIDCIssuer, clock func() time.Time,
	document map[string]any,
) oidcConfig {
	t.Helper()
	config := issuer.testConfig(clock)
	config.fleetValues = []string{"fleet"}
	// A nil document is no mapping file: no approvers (a non-default
	// subject claim refuses one, #546).
	if document != nil {
		config.approverMappingFile = writeApproverMapping(t, document)
	}
	return config
}

// approverClaims is a human's token on the approver audience.
func approverClaims(issuer *fakeOIDCIssuer, now time.Time, subject string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss": issuer.server.URL,
		"aud": []string{testApproverAudience},
		"sub": subject,
		"azp": testApproverClient,
		"realm_access": map[string]any{
			"roles": []any{"offline_access", testApproverValue},
		},
		testHumanClaim: testHumanValue,
		"iat":          now.Add(-time.Minute).Unix(),
		"nbf":          now.Add(-time.Minute).Unix(),
		"exp":          now.Add(time.Hour).Unix(),
	}
}

func TestApproverTokenMintsApproveAndNothingElse(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	now := time.Now()
	authenticator := newTestOIDCAuthenticator(t, approverTestConfig(
		t, issuer, fixedClock(now), approverMappingDocument(issuer.server.URL)))
	claims := approverClaims(issuer, now, "bob")
	// A workspace claim on the approver token grants nothing: the branch
	// is the audience's, and it mints approve or nothing.
	claims["access"] = []string{"reader", "writer"}
	decision, err := authenticator.Authenticate(
		bearerRequest(issuer.signRS256(t, testKID, claims)))
	if err != nil {
		t.Fatalf("a mapped human's approver token: %v", err)
	}
	if got := decision.AllowedOperations(); len(got) != 1 ||
		got[0] != auth.OperationActionApprove {
		t.Fatalf("approver operations = %v, want [action_approve]", got)
	}
	identity := shoal.ID("oidc:" + issuer.server.URL + "#bob")
	// A correlation ID, because the approver branch mints its own decision and
	// an approver's decide route is one of the routes that refuses a request
	// without one (#524). The workspace branch is covered separately, so
	// without this the newer of the two paths would be the uncovered one.
	if !strings.HasPrefix(
		string(decision.CorrelationID()), "oidc-correlation-") {
		t.Fatalf("approver correlation ID = %q, so the decide route refuses "+
			"this approver", decision.CorrelationID())
	}
	if decision.CorrelationID() == decision.RequestID() {
		t.Fatal("the approver's request and correlation identities are one " +
			"value, so the correlation threads nothing")
	}
	if decision.Subject() != identity || decision.Actor() != identity {
		t.Fatalf("approver subject/actor = %q/%q, want %q for both",
			decision.Subject(), decision.Actor(), identity)
	}
	if decision.Actor() == oidcActor {
		t.Fatal("the approver carries the shared OIDC actor")
	}
	if decision.ClientID() != shoal.ID(
		"oidc:"+issuer.server.URL+"#"+testApproverClient) {
		t.Fatalf("approver client = %q", decision.ClientID())
	}
	if len(decision.OnBehalfOf()) != 0 {
		t.Fatalf("approver on-behalf-of = %v", decision.OnBehalfOf())
	}
	if decision.PolicyGeneration() != workspacePolicyGeneration {
		t.Fatalf("approver generation = %d", decision.PolicyGeneration())
	}
	provenance := decision.GrantProvenance()
	want := auth.GrantProvenance{
		Issuer: issuer.server.URL, Subject: "bob",
		ClaimPath:     []string{"realm_access", "roles"},
		MatchedValue:  testApproverValue,
		MappingDigest: authenticator.approverMappingDigest(),
	}
	if !provenance.Equal(want) || want.MappingDigest == (auth.Digest{}) {
		t.Fatalf("approver provenance = %+v, want %+v", provenance, want)
	}
	resource := auth.ResourceRequest{
		AuthorizationDomain: workspaceAuthorizationDomain,
		SourceID:            workspaceSourceID,
		PolicyID:            workspaceGrantPolicyID,
		ObjectID:            "approval",
	}
	if err := decision.AuthorizeObject(
		auth.OperationActionApprove, resource, now); err != nil {
		t.Fatalf("approver cannot approve on the workspace scope: %v", err)
	}
	for _, operation := range []auth.Operation{
		auth.OperationDispatch, auth.OperationInvoke, auth.OperationExecute,
		auth.OperationRead, auth.OperationList,
	} {
		if decision.AuthorizeObject(operation, resource, now) == nil {
			t.Fatalf("approver may also %s", operation)
		}
	}

	// A scalar claim value is a one-element list.
	scalar := approverClaims(issuer, now, "bob")
	scalar["realm_access"] = map[string]any{"roles": testApproverValue}
	if _, err := authenticator.authenticate(
		bearerRequest(issuer.signRS256(t, testKID, scalar))); err != nil {
		t.Fatalf("a scalar mapped value: %v", err)
	}

	// The workspace audience is unchanged by the mapping's presence.
	workspace := issuer.defaultClaims(now)
	workspace["access"] = []string{"fleet"}
	fleetDecision, err := authenticator.Authenticate(
		bearerRequest(issuer.signRS256(t, testKID, workspace)))
	if err != nil {
		t.Fatal(err)
	}
	if fleetDecision.Actor() != oidcActor || fleetDecision.GrantProvenance().Set() {
		t.Fatalf("workspace decision changed: actor %q provenance %+v",
			fleetDecision.Actor(), fleetDecision.GrantProvenance())
	}
}

// TestApproverTokenRefusals is every adversarial token the design lists.
// Each must be denied, with the specific reason where the guard is this
// code's, and the public error must be the single generic denial.
func TestApproverTokenRefusals(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	now := time.Now()
	config := approverTestConfig(
		t, issuer, fixedClock(now), approverMappingDocument(issuer.server.URL))
	config.delegationClaim = "on_behalf_of"
	authenticator := newTestOIDCAuthenticator(t, config)
	other := newFakeOIDCIssuer(t) // a second issuer, same kid, its own key
	sign := issuer.signRS256

	cases := []struct {
		name   string
		edit   func(jwt.MapClaims)
		signer func(*testing.T, string, jwt.MapClaims) string
		want   error // nil: any denial (a parser-level guard)
	}{
		// Agents and clients holding the group.
		{"client credentials: sub equals azp",
			func(c jwt.MapClaims) { c["sub"] = testApproverClient }, nil, errApproverClient},
		{"no human assertion claim (a service account)",
			func(c jwt.MapClaims) { delete(c, testHumanClaim) }, nil, errApproverNotHuman},
		{"human assertion claim with another value",
			func(c jwt.MapClaims) { c[testHumanClaim] = "service" }, nil, errApproverNotHuman},
		{"human assertion claim in another case",
			func(c jwt.MapClaims) { c[testHumanClaim] = "Human" }, nil, errApproverNotHuman},
		{"human assertion claim null",
			func(c jwt.MapClaims) { c[testHumanClaim] = nil }, nil, errApproverNotHuman},
		{"human assertion claim as an array",
			func(c jwt.MapClaims) { c[testHumanClaim] = []any{testHumanValue} }, nil, errApproverNotHuman},
		{"Keycloak service account: client_id",
			func(c jwt.MapClaims) { c["client_id"] = testApproverClient }, nil, errApproverClientToken},
		{"Keycloak service account: clientId",
			func(c jwt.MapClaims) { c["clientId"] = testApproverClient }, nil, errApproverClientToken},
		{"Auth0 client credentials: gty",
			func(c jwt.MapClaims) { c["gty"] = "client-credentials" }, nil, errApproverClientToken},
		{"client credentials: gty with an underscore",
			func(c jwt.MapClaims) { c["gty"] = "client_credentials" }, nil, errApproverClientToken},
		{"gty that is not a string",
			func(c jwt.MapClaims) { c["gty"] = []any{"client-credentials"} }, nil, errApproverClientToken},
		{"client not allowed",
			func(c jwt.MapClaims) { c["azp"] = "other-client" }, nil, errApproverClient},
		{"client case differs",
			func(c jwt.MapClaims) { c["azp"] = "Shoal-Console" }, nil, errApproverClient},
		{"no azp", func(c jwt.MapClaims) { delete(c, "azp") }, nil, errApproverClient},
		{"no sub", func(c jwt.MapClaims) { delete(c, "sub") }, nil, errMissingSubject},
		// Delegation and overage.
		{"act", func(c jwt.MapClaims) {
			c["act"] = map[string]any{"sub": "agent"}
		}, nil, errApproverDelegated},
		{"act null", func(c jwt.MapClaims) { c["act"] = nil }, nil, errApproverDelegated},
		{"may_act", func(c jwt.MapClaims) {
			c["may_act"] = map[string]any{"sub": "agent"}
		}, nil, errApproverDelegated},
		{"_claim_names", func(c jwt.MapClaims) {
			c["_claim_names"] = map[string]any{"groups": "src1"}
		}, nil, errApproverDelegated},
		{"_claim_sources", func(c jwt.MapClaims) {
			c["_claim_sources"] = map[string]any{"src1": map[string]any{}}
		}, nil, errApproverDelegated},
		{"hasgroups", func(c jwt.MapClaims) { c["hasgroups"] = true }, nil, errApproverDelegated},
		{"configured delegation claim", func(c jwt.MapClaims) {
			c["on_behalf_of"] = []string{"alice"}
		}, nil, errApproverDelegated},
		// Issuer confusion.
		{"issuer with a trailing slash",
			func(c jwt.MapClaims) { c["iss"] = issuer.server.URL + "/" }, nil, nil},
		{"issuer in another case",
			func(c jwt.MapClaims) { c["iss"] = strings.ToUpper(issuer.server.URL) }, nil, nil},
		{"second issuer, same kid, its own iss",
			func(c jwt.MapClaims) { c["iss"] = other.server.URL }, other.signRS256, nil},
		{"second issuer's key, same kid, our iss",
			func(jwt.MapClaims) {}, other.signRS256, nil},
		// Audience confusion.
		{"both the approver and a workspace audience",
			func(c jwt.MapClaims) {
				c["aud"] = []string{testApproverAudience, testAudience}
			}, nil, errApproverAudienceConfusion},
		{"an unknown audience only",
			func(c jwt.MapClaims) { c["aud"] = []string{"elsewhere"} }, nil, nil},
		// The path is segments, never a dotted string.
		{"literal dotted top-level key", func(c jwt.MapClaims) {
			delete(c, "realm_access")
			c["realm_access.roles"] = []any{testApproverValue}
		}, nil, errMissingMappedClaim},
		{"path parent is not an object", func(c jwt.MapClaims) {
			c["realm_access"] = "roles"
		}, nil, errMalformedClaim},
		// Exact bytes.
		{"case", approverValue("Shoal-Approvers"), nil, errApproverUnmapped},
		{"leading space", approverValue(" " + testApproverValue), nil, errApproverUnmapped},
		{"trailing space", approverValue(testApproverValue + " "), nil, errApproverUnmapped},
		{"homoglyph (Cyrillic a)", approverValue("shoal-аpprovers"), nil, errApproverUnmapped},
		{"homoglyph (hyphen U+2010)", approverValue("shoal‐approvers"), nil, errApproverUnmapped},
		{"non-string element", func(c jwt.MapClaims) {
			c["realm_access"] = map[string]any{"roles": []any{testApproverValue, 7}}
		}, nil, errMalformedClaim},
		{"object value", func(c jwt.MapClaims) {
			c["realm_access"] = map[string]any{"roles": map[string]any{"a": testApproverValue}}
		}, nil, errMalformedClaim},
		// Overage and absence.
		{"too many values", func(c jwt.MapClaims) {
			roles := []any{testApproverValue}
			for i := 0; i < 8; i++ {
				roles = append(roles, "filler")
			}
			c["realm_access"] = map[string]any{"roles": roles}
		}, nil, errMalformedClaim},
		{"missing claim", func(c jwt.MapClaims) { delete(c, "realm_access") }, nil, errMissingMappedClaim},
		{"empty claim", func(c jwt.MapClaims) {
			c["realm_access"] = map[string]any{"roles": []any{}}
		}, nil, errMissingMappedClaim},
		{"null claim", func(c jwt.MapClaims) {
			c["realm_access"] = map[string]any{"roles": nil}
		}, nil, errMissingMappedClaim},
		// A mapped human whose token shows the fleet mapping too.
		{"also holds fleet", func(c jwt.MapClaims) {
			c["access"] = []string{"reader", "fleet"}
		}, nil, errApproverHoldsFleet},
	}
	for _, probe := range cases {
		t.Run(probe.name, func(t *testing.T) {
			claims := approverClaims(issuer, now, "bob")
			probe.edit(claims)
			signer := probe.signer
			if signer == nil {
				signer = sign
			}
			token := signer(t, testKID, claims)
			decision, err := authenticator.authenticate(bearerRequest(token))
			if err == nil {
				t.Fatalf("minted %v", decision.AllowedOperations())
			}
			if probe.want != nil && !errors.Is(err, probe.want) {
				t.Fatalf("denied for %v, want %v", err, probe.want)
			}
			_, public := authenticator.Authenticate(bearerRequest(token))
			if public == nil || public.Error() != oidcDenied().Error() {
				t.Fatalf("public error = %v, want the generic denial", public)
			}
			for _, segment := range strings.Split(token, ".") {
				if strings.Contains(public.Error(), segment) ||
					strings.Contains(err.Error(), segment) {
					t.Fatal("a denial carries a token segment")
				}
			}
		})
	}
}

func approverValue(value string) func(jwt.MapClaims) {
	return func(c jwt.MapClaims) {
		c["realm_access"] = map[string]any{"roles": []any{value}}
	}
}

// TestApproverPathIsLiteralSegments is the reverse of the dotted-key spoof:
// a mapping naming the single segment "realm_access.roles" matches only a
// literal top-level key of that name, never the nested path.
func TestApproverPathIsLiteralSegments(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	now := time.Now()
	document := approverMappingDocument(issuer.server.URL)
	document["claim"] = []string{"realm_access.roles"}
	authenticator := newTestOIDCAuthenticator(
		t, approverTestConfig(t, issuer, fixedClock(now), document))
	nested := approverClaims(issuer, now, "bob")
	if _, err := authenticator.authenticate(bearerRequest(
		issuer.signRS256(t, testKID, nested))); !errors.Is(err, errMissingMappedClaim) {
		t.Fatalf("nested path under a dotted mapping = %v", err)
	}
	literal := approverClaims(issuer, now, "bob")
	delete(literal, "realm_access")
	literal["realm_access.roles"] = []any{testApproverValue}
	decision, err := authenticator.authenticate(bearerRequest(
		issuer.signRS256(t, testKID, literal)))
	if err != nil {
		t.Fatalf("literal dotted key under a dotted mapping: %v", err)
	}
	if got := decision.GrantProvenance().ClaimPath; len(got) != 1 ||
		got[0] != "realm_access.roles" {
		t.Fatalf("claim path = %v", got)
	}
}

// TestApproverRefusesTheReviewersServiceAccountToken is the #523 review's
// reproduction: a Keycloak service-account token for the console client —
// a UUID sub, azp the allowed client, preferred_username
// service-account-<client>, no idtyp, holding the approver role. Under the
// documented "idtyp absent" assertion it was minted an approver. The
// assertion is now positive, and Keycloak's own client marker refuses it too.
func TestApproverRefusesTheReviewersServiceAccountToken(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	now := time.Now()
	authenticator := newTestOIDCAuthenticator(t, approverTestConfig(
		t, issuer, fixedClock(now), approverMappingDocument(issuer.server.URL)))
	serviceAccount := func() jwt.MapClaims {
		claims := approverClaims(
			issuer, now, "0f8fad5b-d9cb-469f-a165-70867728950e")
		delete(claims, testHumanClaim)
		claims["preferred_username"] = "service-account-" + testApproverClient
		return claims
	}
	if _, err := authenticator.authenticate(bearerRequest(issuer.signRS256(
		t, testKID, serviceAccount()))); !errors.Is(err, errApproverNotHuman) {
		t.Fatalf("the service-account token = %v, want not-human", err)
	}
	withMarker := serviceAccount()
	withMarker["client_id"] = testApproverClient
	if _, err := authenticator.authenticate(bearerRequest(issuer.signRS256(
		t, testKID, withMarker))); err == nil {
		t.Fatal("the service-account token with client_id was minted")
	}
	// Even if a misconfigured mapper gave it the human claim, its client
	// marker refuses it.
	withMarker[testHumanClaim] = testHumanValue
	if _, err := authenticator.authenticate(bearerRequest(issuer.signRS256(
		t, testKID, withMarker))); !errors.Is(err, errApproverClientToken) {
		t.Fatalf("a service account carrying the human claim = %v", err)
	}
}

// TestApproverHumanAssertionEquals covers the equals form.
func TestApproverHumanAssertionEquals(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	now := time.Now()
	document := approverMappingDocument(issuer.server.URL)
	document["human_assertion"] = map[string]any{
		"claim": []string{"ext", "kind"}, "equals": "user",
	}
	authenticator := newTestOIDCAuthenticator(
		t, approverTestConfig(t, issuer, fixedClock(now), document))
	for _, probe := range []struct {
		value any
		ok    bool
	}{
		{"user", true}, {"User", false}, {"app", false}, {nil, false},
		{[]any{"user"}, false},
	} {
		claims := approverClaims(issuer, now, "bob")
		if probe.value != nil {
			claims["ext"] = map[string]any{"kind": probe.value}
		}
		_, err := authenticator.authenticate(bearerRequest(
			issuer.signRS256(t, testKID, claims)))
		if (err == nil) != probe.ok {
			t.Fatalf("idtyp %v = %v, want ok %v", probe.value, err, probe.ok)
		}
	}
}

// TestApproverMappingStartupRefusals is every configuration the design says
// to refuse at startup.
func TestApproverMappingStartupRefusals(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	now := time.Now()
	edit := func(change func(map[string]any)) any {
		document := approverMappingDocument(issuer.server.URL)
		change(document)
		return document
	}
	valid, err := json.Marshal(approverMappingDocument(issuer.server.URL))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		document any
	}{
		{"issuer with a trailing slash", edit(func(d map[string]any) {
			d["issuer"] = issuer.server.URL + "/"
		})},
		{"issuer in another case", edit(func(d map[string]any) {
			d["issuer"] = strings.ToUpper(issuer.server.URL)
		})},
		{"issuer with surrounding space", edit(func(d map[string]any) {
			d["issuer"] = " " + issuer.server.URL
		})},
		{"another issuer than the flag", edit(func(d map[string]any) {
			d["issuer"] = "https://elsewhere.example"
		})},
		{"no issuer", edit(func(d map[string]any) { delete(d, "issuer") })},
		{"wrong version", edit(func(d map[string]any) { d["version"] = "shoal.approvers/v2" })},
		{"unknown field", edit(func(d map[string]any) { d["fallback"] = "reader" })},
		{"unknown nested field", edit(func(d map[string]any) {
			d["human_assertion"] = map[string]any{
				"claim": []string{testHumanClaim}, "equals": testHumanValue, "or": "x",
			}
		})},
		{"duplicate key", []byte(strings.Replace(string(valid),
			`"audience":`, `"audience":"`+testAudience+`","audience":`, 1))},
		{"trailing data", append(append([]byte(nil), valid...), []byte(` {}`)...)},
		{"audience is a workspace audience", edit(func(d map[string]any) {
			d["audience"] = testAudience
		})},
		{"no audience", edit(func(d map[string]any) { d["audience"] = "" })},
		{"audience with whitespace", edit(func(d map[string]any) { d["audience"] = "a " })},
		{"no client_ids", edit(func(d map[string]any) { d["client_ids"] = []string{} })},
		{"empty client id", edit(func(d map[string]any) { d["client_ids"] = []string{""} })},
		{"no claim", edit(func(d map[string]any) { d["claim"] = []string{} })},
		{"claim as a dotted string", edit(func(d map[string]any) {
			d["claim"] = "realm_access.roles"
		})},
		{"empty claim segment", edit(func(d map[string]any) {
			d["claim"] = []string{"realm_access", ""}
		})},
		{"no values", edit(func(d map[string]any) { d["values"] = []string{} })},
		{"value with a leading space", edit(func(d map[string]any) {
			d["values"] = []string{" " + testApproverValue}
		})},
		{"value with a trailing space", edit(func(d map[string]any) {
			d["values"] = []string{testApproverValue + " "}
		})},
		{"value with a control character", edit(func(d map[string]any) {
			d["values"] = []string{"shoal‎" + "\x07approvers"}
		})},
		{"value with a tab", edit(func(d map[string]any) {
			d["values"] = []string{"shoal\tapprovers"}
		})},
		{"duplicate value", edit(func(d map[string]any) {
			d["values"] = []string{testApproverValue, testApproverValue}
		})},
		{"zero max_values", edit(func(d map[string]any) { d["max_values"] = 0 })},
		{"max_values over the bound", edit(func(d map[string]any) {
			d["max_values"] = approverMappingMaxValues + 1
		})},
		{"no human assertion", edit(func(d map[string]any) { delete(d, "human_assertion") })},
		{"human assertion by absence (removed)", edit(func(d map[string]any) {
			d["human_assertion"] = map[string]any{
				"claim": []string{"idtyp"}, "absent": true,
			}
		})},
		{"human assertion with absence and equals", edit(func(d map[string]any) {
			d["human_assertion"] = map[string]any{
				"claim": []string{"idtyp"}, "absent": true, "equals": "user",
			}
		})},
		{"human assertion without equals", edit(func(d map[string]any) {
			d["human_assertion"] = map[string]any{"claim": []string{"idtyp"}}
		})},
		{"human assertion with an empty value", edit(func(d map[string]any) {
			d["human_assertion"] = map[string]any{
				"claim": []string{"idtyp"}, "equals": "",
			}
		})},
		{"oversized file", []byte(`{"version":"` +
			strings.Repeat("x", approverMappingMaxBytes) + `"}`)},
		{"not UTF-8", []byte("{\"version\":\"\xff\"}")},
	}
	for _, probe := range cases {
		config := issuer.testConfig(fixedClock(now))
		config.approverMappingFile = writeApproverMapping(t, probe.document)
		if _, err := newOIDCAuthenticator(config, time.Now); err == nil {
			t.Errorf("%s: the authenticator started", probe.name)
		}
	}

	// A missing file refuses rather than starting without approvers.
	config := issuer.testConfig(fixedClock(now))
	config.approverMappingFile = filepath.Join(t.TempDir(), "absent.json")
	if _, err := newOIDCAuthenticator(config, time.Now); err == nil {
		t.Error("a missing mapping file started the authenticator")
	}
	// The legacy Entra identity mode names principals differently from an
	// approver, so the same human would be two identities.
	legacy := applyLegacyEntraCompatibility(oidcConfig{
		issuer: issuer.server.URL, audiences: []string{testAudience},
		httpClient: issuer.server.Client(), clock: fixedClock(now),
		approverMappingFile: writeApproverMapping(
			t, approverMappingDocument(issuer.server.URL)),
	}, legacyEntraConfig{readerRoles: []string{"reader"}, audience: testAudience})
	if _, err := newOIDCAuthenticator(legacy, time.Now); err == nil {
		t.Error("the approver mapping started under the legacy identity mode")
	}
	// The mapping file alone is OIDC intent, and an incomplete one.
	alone := oidcConfig{approverMappingFile: config.approverMappingFile}
	if !alone.configured() {
		t.Error("a mapping file alone does not count as OIDC configuration")
	}
}

// TestApproverMappingDigestIsSemantic: reordering sets does not move the
// digest; every change in meaning does.
func TestApproverMappingDigestIsSemantic(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	base := approverMappingDocument(issuer.server.URL)
	base["client_ids"] = []string{"a", "b"}
	base["values"] = []string{"x", "y"}
	digestOf := func(document map[string]any) auth.Digest {
		t.Helper()
		raw, err := json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
		mapping, err := parseApproverMapping(
			raw, issuer.server.URL, []string{testAudience}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return mapping.digest
	}
	original := digestOf(base)
	reordered := approverMappingDocument(issuer.server.URL)
	reordered["client_ids"] = []string{"b", "a"}
	reordered["values"] = []string{"y", "x"}
	if digestOf(reordered) != original {
		t.Fatal("reordering a set moved the digest")
	}
	seen := map[auth.Digest]string{original: "original"}
	for name, change := range map[string]func(map[string]any){
		"audience": func(d map[string]any) { d["audience"] = "other" },
		"client":   func(d map[string]any) { d["client_ids"] = []string{"a"} },
		"claim":    func(d map[string]any) { d["claim"] = []string{"realm_access.roles"} },
		"value":    func(d map[string]any) { d["values"] = []string{"x", "z"} },
		"max":      func(d map[string]any) { d["max_values"] = 9 },
		"assertion": func(d map[string]any) {
			d["human_assertion"] = map[string]any{"claim": []string{testHumanClaim}, "equals": "user"}
		},
		"assert claim": func(d map[string]any) {
			d["human_assertion"] = map[string]any{"claim": []string{"typ"}, "equals": testHumanValue}
		},
	} {
		document := approverMappingDocument(issuer.server.URL)
		document["client_ids"] = []string{"a", "b"}
		document["values"] = []string{"x", "y"}
		change(document)
		digest := digestOf(document)
		if previous, ok := seen[digest]; ok {
			t.Fatalf("changing the %s left the digest equal to %s", name, previous)
		}
		seen[digest] = name
	}
}

// TestNoApproverMappingMeansNoApprovers: without the file, a token on the
// would-be approver audience is refused by the parser, and no workspace
// token mints approve whatever its claims say.
func TestNoApproverMappingMeansNoApprovers(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	now := time.Now()
	config := issuer.testConfig(fixedClock(now))
	config.fleetValues = []string{"fleet"}
	authenticator := newTestOIDCAuthenticator(t, config)
	if authenticator.approverMappingDigest() != (auth.Digest{}) {
		t.Fatal("an authenticator without a mapping reports a digest")
	}
	if _, err := authenticator.authenticate(bearerRequest(issuer.signRS256(
		t, testKID, approverClaims(issuer, now, "bob")))); err == nil {
		t.Fatal("an approver-audience token was accepted with no mapping")
	}
}

// TestBrowserAuthConfigDisclosesNoMapping: the public browser login
// configuration is the same with and without the approver mapping, and none
// of the mapping's values appear in it.
func TestBrowserAuthConfigDisclosesNoMapping(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	now := time.Now()
	plain := issuer.testConfig(fixedClock(now))
	plain.browserClientID, plain.browserScope = "browser", "openid"
	withMapping := plain
	withMapping.fleetValues = []string{"fleet"}
	withMapping.approverMappingFile = writeApproverMapping(
		t, approverMappingDocument(issuer.server.URL))
	encode := func(config oidcConfig) string {
		authenticator := newTestOIDCAuthenticator(t, config)
		browser, err := authenticator.browserAuthConfig(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(browser)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	without, with := encode(plain), encode(withMapping)
	if without != with {
		t.Fatalf("browser config changed with the mapping:\n%s\n%s", without, with)
	}
	for _, secret := range []string{
		testApproverAudience, testApproverClient, testApproverValue,
		"realm_access", testHumanClaim,
	} {
		if strings.Contains(with, secret) {
			t.Fatalf("browser config discloses %q", secret)
		}
	}
}
