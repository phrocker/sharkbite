// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
)

// labelGrantsConfig is the issuer's test configuration with a grant file
// holding document.
func labelGrantsConfig(
	t *testing.T, issuer *fakeOIDCIssuer, document any,
) oidcConfig {
	t.Helper()
	config := issuer.testConfig(time.Now)
	config.labelGrantsFile = writeLabelGrants(t, document)
	config.labelGrantSources = [][]byte{workspaceSourceID, labelOtherSource}
	return config
}

// TestLabelGrantsFileRefusals: every malformed or unsafe grant file stops
// the workspace from starting, with a reason that names the problem.
func TestLabelGrantsFileRefusals(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	valid := func() map[string]any { return labelGrantsDocument(issuer.server.URL) }
	grant := func(source, label string) []map[string]string {
		return []map[string]string{{"source": source, "label": label}}
	}
	workspace := string(workspaceSourceID)
	manyGrants := make([]map[string]string, 0, authorized.MaxLabelGrants+1)
	for i := 0; i <= authorized.MaxLabelGrants; i++ {
		manyGrants = append(manyGrants,
			map[string]string{"source": workspace, "label": fmt.Sprintf("l%d", i)})
	}
	manyValues := map[string]any{}
	for i := 0; i <= labelGrantsMaxValues; i++ {
		manyValues[fmt.Sprintf("group-%d", i)] = grant(workspace, "secret")
	}
	for _, refused := range []struct {
		name   string
		edit   func(map[string]any)
		raw    []byte
		reason string
	}{
		{name: "unknown top-level field", edit: func(d map[string]any) {
			d["policies"] = []string{"shoal.label/v1/!untranslatable"}
		}, reason: "does not match a field exactly"},
		{name: "unknown grant field", edit: func(d map[string]any) {
			d["grants"] = map[string]any{"g": []map[string]string{{
				"source": workspace, "label": "secret",
				"policy": "shoal.label/v1/!untranslatable",
			}}}
		}, reason: "does not match a field exactly"},
		{name: "duplicate key", raw: []byte(`{"version":"shoal.label-grants/v1",` +
			`"version":"shoal.label-grants/v1"}`), reason: "duplicate JSON key"},
		{name: "duplicate claim value", raw: []byte(`{"version":"shoal.label-grants/v1",` +
			`"issuer":"` + issuer.server.URL + `","claim":["groups"],"max_values":4,` +
			`"grants":{"g":[{"source":"` + workspace + `","label":"a"}],` +
			`"g":[{"source":"` + workspace + `","label":"b"}]}}`),
			reason: "duplicate JSON key"},
		{name: "trailing data", raw: append(mustJSON(t, valid()), []byte(` {}`)...),
			reason: "trailing JSON data"},
		{name: "not UTF-8", raw: []byte{'{', 0xff, '}'}, reason: "UTF-8"},
		{name: "wrong version", edit: func(d map[string]any) {
			d["version"] = "shoal.label-grants/v2"
		}, reason: "version"},
		{name: "another issuer", edit: func(d map[string]any) {
			d["issuer"] = issuer.server.URL + "/"
		}, reason: "issuer"},
		{name: "no claim", edit: func(d map[string]any) {
			d["claim"] = []string{}
		}, reason: "claim"},
		{name: "padded claim segment", edit: func(d map[string]any) {
			d["claim"] = []string{"groups "}
		}, reason: "claim"},
		{name: "max_values zero", edit: func(d map[string]any) {
			d["max_values"] = 0
		}, reason: "max_values"},
		{name: "max_values over bound", edit: func(d map[string]any) {
			d["max_values"] = labelGrantsMaxClaimValues + 1
		}, reason: "max_values"},
		{name: "no grants", edit: func(d map[string]any) {
			d["grants"] = map[string]any{}
		}, reason: "grants must not be empty"},
		{name: "a value granting nothing", edit: func(d map[string]any) {
			d["grants"] = map[string]any{"g": []map[string]string{}}
		}, reason: "grants nothing"},
		{name: "padded claim value", edit: func(d map[string]any) {
			d["grants"] = map[string]any{"g ": grant(workspace, "secret")}
		}, reason: "claim values"},
		{name: "unknown source", edit: func(d map[string]any) {
			d["grants"] = map[string]any{"g": grant("shoal-explore-web/nowhere", "secret")}
		}, reason: "not a configured source"},
		{name: "source differing in case", edit: func(d map[string]any) {
			d["grants"] = map[string]any{"g": grant(strings.ToUpper(workspace), "secret")}
		}, reason: "not a configured source"},
		{name: "missing label", edit: func(d map[string]any) {
			d["grants"] = map[string]any{"g": []map[string]string{{"source": workspace}}}
		}, reason: "source and a label"},
		{name: "empty label", edit: func(d map[string]any) {
			d["grants"] = map[string]any{"g": grant(workspace, "")}
		}, reason: "not a valid label"},
		{name: "label with a slash", edit: func(d map[string]any) {
			d["grants"] = map[string]any{"g": grant(workspace, "a/b")}
		}, reason: "not a valid label"},
		{name: "the reserved label spelled out", edit: func(d map[string]any) {
			d["grants"] = map[string]any{"g": grant(workspace, "!untranslatable")}
		}, reason: "not a valid label"},
		{name: "over-long label", edit: func(d map[string]any) {
			d["grants"] = map[string]any{"g": grant(workspace, strings.Repeat("x", 257))}
		}, reason: "not a valid label"},
		{name: "label whose ID exceeds the component bound", edit: func(d map[string]any) {
			d["grants"] = map[string]any{"g": grant(workspace, strings.Repeat("x", 120))}
		}, reason: "exceeds the policy component"},
		{name: "a pair granted twice", edit: func(d map[string]any) {
			d["grants"] = map[string]any{"g": append(
				grant(workspace, "secret"), grant(workspace, "secret")...)}
		}, reason: "twice"},
		{name: "more grants than the bound", edit: func(d map[string]any) {
			d["grants"] = map[string]any{"g": manyGrants}
		}, reason: "more than"},
		{name: "more claim values than the bound", edit: func(d map[string]any) {
			d["grants"] = manyValues
		}, reason: "more claim values"},
		{name: "oversize file", raw: append(mustJSON(t, valid()),
			[]byte(strings.Repeat(" ", labelGrantsMaxBytes))...),
			reason: "size bound"},
	} {
		t.Run(refused.name, func(t *testing.T) {
			raw := refused.raw
			if raw == nil {
				document := valid()
				refused.edit(document)
				raw = mustJSON(t, document)
			}
			_, err := newOIDCAuthenticator(
				labelGrantsConfig(t, issuer, raw), time.Now)
			if err == nil || !strings.Contains(err.Error(), refused.reason) ||
				!strings.Contains(err.Error(), "-oidc-label-grants-file") {
				t.Fatalf("= %v, want a refusal citing %q", err, refused.reason)
			}
		})
	}
	if _, err := newOIDCAuthenticator(
		labelGrantsConfig(t, issuer, valid()), time.Now); err != nil {
		t.Fatalf("the valid control was refused: %v", err)
	}
	// In production the only configured source is the workspace's, so a
	// file naming any other source is refused.
	production := labelGrantsConfig(t, issuer, valid())
	production.labelGrantSources = nil
	if _, err := newOIDCAuthenticator(production, time.Now); err == nil ||
		!strings.Contains(err.Error(), "not a configured source") {
		t.Fatalf("a second source without configuration = %v", err)
	}
}

// TestLabelGrantsRefuseReservedAndNonCanonicalIDs: the file can never grant
// an ID in the reserved namespace or one that does not read back as the pair
// it was written for, even if the ID constructor were wrong. Labels cannot
// spell the reserved prefix, so the constructor is replaced to reach the
// check.
func TestLabelGrantsRefuseReservedAndNonCanonicalIDs(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	raw := mustJSON(t, labelGrantsDocument(issuer.server.URL))
	sources := [][]byte{workspaceSourceID, labelOtherSource}
	reserved := func([]byte, string) ([]byte, error) {
		return []byte(authorized.UntranslatableLabelPolicyID), nil
	}
	if _, err := parseLabelGrants(raw, issuer.server.URL, sources, reserved); err == nil ||
		!strings.Contains(err.Error(), "reserved") {
		t.Fatalf("a reserved ID = %v, want the reserved refusal", err)
	}
	foreign := func(source []byte, _ string) ([]byte, error) {
		return authorized.LabelPolicyID(source, "other")
	}
	if _, err := parseLabelGrants(raw, issuer.server.URL, sources, foreign); err == nil ||
		!strings.Contains(err.Error(), "canonical") {
		t.Fatalf("a foreign ID = %v, want the canonical refusal", err)
	}
	if _, err := parseLabelGrants(
		raw, issuer.server.URL, sources, authorized.LabelPolicyID); err != nil {
		t.Fatalf("the real constructor = %v", err)
	}
}

// TestLabelGrantsDigestIsProvenanceNotAuthority: the digest pins what the
// file grants and ignores its layout; it is not in any decision. A grant
// change moves the fingerprint of exactly the principals it changes.
func TestLabelGrantsDigestIsProvenanceNotAuthority(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	parse := func(document any) *labelGrants {
		t.Helper()
		grants, err := parseLabelGrants(mustJSON(t, document), issuer.server.URL,
			[][]byte{workspaceSourceID, labelOtherSource}, authorized.LabelPolicyID)
		if err != nil {
			t.Fatal(err)
		}
		return grants
	}
	base := labelGrantsDocument(issuer.server.URL)
	reordered := labelGrantsDocument(issuer.server.URL)
	reordered["grants"].(map[string]any)[labelGroupSecret] = []map[string]string{
		{"source": string(workspaceSourceID), "label": "pii"},
		{"source": string(workspaceSourceID), "label": "secret"},
	}
	widened := labelGrantsDocument(issuer.server.URL)
	widened["grants"].(map[string]any)[labelGroupSecret] = []map[string]string{
		{"source": string(workspaceSourceID), "label": "secret"},
		{"source": string(workspaceSourceID), "label": "pii"},
	}
	if parse(reordered).digest != parse(widened).digest {
		t.Fatal("reordering one value's grants moved the digest")
	}
	if parse(base).digest == parse(widened).digest {
		t.Fatal("a grant change did not move the digest")
	}
	if parse(base).digest == (auth.Digest{}) {
		t.Fatal("the digest is zero")
	}

	fingerprint := func(document any, groups []string) auth.Fingerprint {
		t.Helper()
		authenticator := newTestOIDCAuthenticator(t, labelGrantsConfig(t, issuer, document))
		claims := issuer.defaultClaims(time.Now())
		claims["groups"] = groups
		decision, err := authenticator.Authenticate(
			bearerRequest(issuer.signRS256(t, testKID, claims)))
		if err != nil {
			t.Fatal(err)
		}
		value, err := auth.AuthorizationFingerprint(decision)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	bystander := []string{labelGroupOtherSecret}
	if fingerprint(base, bystander) != fingerprint(widened, bystander) {
		t.Fatal("a grant to another group moved a bystander's fingerprint")
	}
	affected := []string{labelGroupSecret}
	if fingerprint(base, affected) == fingerprint(widened, affected) {
		t.Fatal("a grant change did not move the affected principal's fingerprint")
	}
}

// TestDevelopmentAuthLabels: -dev-auth-labels grants the development
// principal label policies on the workspace source and nothing else.
func TestDevelopmentAuthLabels(t *testing.T) {
	secret, err := authorized.LabelPolicyID(workspaceSourceID, "secret")
	if err != nil {
		t.Fatal(err)
	}
	authenticator, err := selectAuthenticator(true,
		string(workspaceSourceID)+"=secret", oidcConfig{}, "127.0.0.1:8080", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := authenticator.Authenticate(bearerRequest("unused"))
	if err != nil {
		t.Fatal(err)
	}
	policies := decision.PermittedPolicyIDs()
	if len(policies) != 2 || !containsBytes(policies, secret) {
		t.Fatalf("the development principal holds %q", policies)
	}
	if !sameOperations(decision.AllowedOperations(), workspaceOperations) {
		t.Fatalf("labels changed the development operations: %v",
			decision.AllowedOperations())
	}
	plain, err := selectAuthenticator(true, "", oidcConfig{}, "127.0.0.1:8080", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	unlabelled, err := plain.Authenticate(bearerRequest("unused"))
	if err != nil {
		t.Fatal(err)
	}
	if hasLabelPolicy(unlabelled.PermittedPolicyIDs()) {
		t.Fatalf("-dev-auth without labels holds %q", unlabelled.PermittedPolicyIDs())
	}

	for _, refused := range []struct{ labels, reason string }{
		{string(workspaceSourceID) + ":secret", "form"},
		{"shoal-explore-web/other=secret", "not configured"},
		{string(workspaceSourceID) + "=", "label"},
		{string(workspaceSourceID) + "=a/b", "unsupported character"},
		{string(workspaceSourceID) + "=secret,", "empty entry"},
	} {
		if _, err := selectAuthenticator(true, refused.labels, oidcConfig{},
			"127.0.0.1:8080", time.Now); err == nil ||
			!strings.Contains(err.Error(), "-dev-auth-labels") ||
			!strings.Contains(err.Error(), refused.reason) {
			t.Fatalf("-dev-auth-labels %q = %v, want %q", refused.labels, err, refused.reason)
		}
	}
	if _, err := selectAuthenticator(false, string(workspaceSourceID)+"=secret",
		oidcConfig{}, "127.0.0.1:8080", time.Now); err == nil ||
		!strings.Contains(err.Error(), "-dev-auth-labels") {
		t.Fatalf("-dev-auth-labels without -dev-auth = %v", err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
