// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Unit tests for the executor mint (#391, PR3): the operator file, the
// second issuer and its isolated key cache, and the decision. The end-to-end
// tests over the real dispatch routes are in oidc_executor_e2e_test.go.

const (
	testExecutorAudience = "shoal-executors"
	// The subjects a Kubernetes service-account issuer gives projected
	// tokens.
	testExecutorSubject      = "system:serviceaccount:shoal:stripe-gateway"
	testExecutorRef          = "stripe"
	testOtherExecutorSubject = "system:serviceaccount:shoal:ledger-gateway"
	testOtherExecutorRef     = "ledger"
	testExecutorNamespace    = "shoal"
)

// executorMappingDocument is the test executor mapping: two service
// accounts of one namespace, one executor reference each, asserted by the
// Kubernetes namespace claim.
func executorMappingDocument(issuer string) map[string]any {
	return map[string]any{
		"version":  executorMappingVersion,
		"issuer":   issuer,
		"audience": testExecutorAudience,
		"service_assertion": map[string]any{
			"claim":  []string{"kubernetes.io", "namespace"},
			"equals": testExecutorNamespace,
		},
		"executors": []map[string]string{
			{"subject": testExecutorSubject, "executor_ref": testExecutorRef},
			{"subject": testOtherExecutorSubject, "executor_ref": testOtherExecutorRef},
		},
	}
}

func writeExecutorMapping(t *testing.T, document any) string {
	t.Helper()
	raw, ok := document.([]byte)
	if !ok {
		var err error
		raw, err = json.Marshal(document)
		if err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(t.TempDir(), "executors.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// executorTestConfig is every OIDC mapping this command has, at once: the
// human issuer's workspace and fleet mappings, the approver mapping, label
// grants, and the executor mapping naming a second issuer.
func executorTestConfig(
	t *testing.T, human, executor *fakeOIDCIssuer, clock func() time.Time,
	document map[string]any,
) oidcConfig {
	t.Helper()
	config := approverTestConfig(t, human, clock, approverMappingDocument(human.server.URL))
	config.labelGrantsFile = writeLabelGrants(t, labelGrantsDocument(human.server.URL))
	config.labelGrantSources = [][]byte{workspaceSourceID, labelOtherSource}
	config.executorMappingFile = writeExecutorMapping(t, document)
	return config
}

// executorClaims is a projected service-account token for subject.
func executorClaims(issuer *fakeOIDCIssuer, now time.Time, subject string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss": issuer.server.URL,
		"aud": []string{testExecutorAudience},
		"sub": subject,
		"kubernetes.io": map[string]any{
			"namespace": testExecutorNamespace,
			"serviceaccount": map[string]any{
				"name": subject[strings.LastIndex(subject, ":")+1:],
				"uid":  "0d6c1c51-4c25-4a33-9c4e-4c3f2f9f8a1e",
			},
		},
		"iat": now.Add(-time.Minute).Unix(),
		"nbf": now.Add(-time.Minute).Unix(),
		"exp": now.Add(time.Hour).Unix(),
	}
}

type executorFixture struct {
	human, executor *fakeOIDCIssuer
	now             time.Time
	authn           *oidcAuthenticator
}

func newExecutorFixture(t *testing.T) *executorFixture {
	t.Helper()
	f := &executorFixture{
		human: newFakeOIDCIssuer(t), executor: newFakeOIDCIssuer(t), now: time.Now(),
	}
	f.authn = newTestOIDCAuthenticator(t, executorTestConfig(
		t, f.human, f.executor, fixedClock(f.now),
		executorMappingDocument(f.executor.server.URL)))
	return f
}

func (f *executorFixture) executorToken(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	return f.executor.signRS256(t, testKID, claims)
}

// mustMint authenticates and returns the decision, failing on the generic
// denial.
func (f *executorFixture) mustMint(t *testing.T, request *http.Request) auth.Decision {
	t.Helper()
	decision, err := f.authn.Authenticate(request)
	if err != nil {
		t.Fatalf("a mapped executor token was refused: %v", err)
	}
	return decision
}

func TestExecutorTokenMintsExecuteBoundToItsReference(t *testing.T) {
	t.Parallel()
	f := newExecutorFixture(t)
	claims := executorClaims(f.executor, f.now, testExecutorSubject)
	// Claims a human's token would be granted by: none of them reaches an
	// executor. The branch is the audience's, and it mints execute or
	// nothing.
	claims["access"] = []string{"reader", "writer", "fleet"}
	claims["groups"] = []string{labelGroupSecret}
	decision := f.mustMint(t, bearerRequest(f.executorToken(t, claims)))

	// The decision holds its operations sorted.
	if got := decision.AllowedOperations(); len(got) != 2 || got[0] != auth.OperationAgentResolve ||
		got[1] != auth.OperationExecute {
		t.Fatalf("executor operations = %v, want [execute agent_resolve]", got)
	}
	if decision.ServiceRole() != auth.ServiceRoleActionExecution {
		t.Fatalf("role = %q, want action_execution", decision.ServiceRole())
	}
	if decision.ExecutorBinding() != testExecutorRef {
		t.Fatalf("binding = %q, want %q", decision.ExecutorBinding(), testExecutorRef)
	}
	identity := shoal.ID("oidcexec:" + f.executor.server.URL + "#" + testExecutorSubject)
	if decision.Subject() != identity || decision.Actor() != identity {
		t.Fatalf("subject, actor = %q, %q; want both %q",
			decision.Subject(), decision.Actor(), identity)
	}
	// Attestation keys a statement by (domain, subject, client) and refuses
	// an empty client.
	if decision.ClientID() == "" {
		t.Fatal("the executor decision carries no client ID")
	}
	if len(decision.OnBehalfOf()) != 0 {
		t.Fatalf("an executor acts on behalf of %v", decision.OnBehalfOf())
	}
	// No labels (#570): the grant file maps "groups" and this token carries
	// a granted group, and the executor still holds the workspace policy
	// alone.
	if policies := decision.PermittedPolicyIDs(); len(policies) != 1 ||
		string(policies[0]) != string(workspaceGrantPolicyID) {
		t.Fatalf("executor policies = %q, want the workspace policy alone", policies)
	}
	provenance := decision.GrantProvenance()
	digest, count := f.authn.executorMappingDigest()
	if count != 2 || digest == (auth.Digest{}) ||
		provenance.MappingDigest != digest ||
		provenance.Issuer != f.executor.server.URL ||
		provenance.Subject != testExecutorSubject {
		t.Fatalf("provenance = %+v, mapping digest %s (%d)", provenance, digest, count)
	}
	// #527: a correlation ID, minted when the caller supplies none...
	if !strings.HasPrefix(string(decision.CorrelationID()), oidcExecutorCorrelationPrefix) {
		t.Fatalf("correlation ID = %q, want a minted %s value",
			decision.CorrelationID(), oidcExecutorCorrelationPrefix)
	}
	// ...and the caller's, threaded, when it supplies one.
	request := bearerRequest(f.executorToken(t, claims))
	request.Header.Set(CorrelationIDHeader, "upstream-trace-391")
	if got := f.mustMint(t, request).CorrelationID(); got != "upstream-trace-391" {
		t.Fatalf("supplied correlation ID = %q", got)
	}
	// Executor identities are outside the human identity family of #553.
	for _, namespace := range humanIdentityFamily {
		if strings.HasPrefix(string(decision.Subject()), namespace) ||
			strings.HasPrefix(string(decision.ClientID()), namespace) {
			t.Fatalf("executor identity %q is in the human family via %q",
				decision.Subject(), namespace)
		}
	}

	// The second entry is bound to its own reference.
	other := f.mustMint(t, bearerRequest(f.executorToken(t,
		executorClaims(f.executor, f.now, testOtherExecutorSubject))))
	if other.ExecutorBinding() != testOtherExecutorRef {
		t.Fatalf("second binding = %q", other.ExecutorBinding())
	}
}

// TestExecutorTokenRefusals: every check is required, and each refusal is
// the generic denial at the trust boundary.
func TestExecutorTokenRefusals(t *testing.T) {
	t.Parallel()
	f := newExecutorFixture(t)
	f.authn.delegationClaim = "delegated_by"
	valid := func() jwt.MapClaims { return executorClaims(f.executor, f.now, testExecutorSubject) }
	for _, refused := range []struct {
		name  string
		token func() string
		// want is the specific sentinel; nil accepts any refusal (parser
		// errors are the library's).
		want error
	}{
		{name: "human issuer, executor audience", token: func() string {
			claims := valid()
			claims["iss"] = f.human.server.URL
			return f.human.signRS256(t, testKID, claims)
		}},
		{name: "a human's workspace token re-addressed to the executor audience", token: func() string {
			claims := f.human.defaultClaims(f.now)
			claims["aud"] = []string{testExecutorAudience}
			claims["sub"] = testExecutorSubject
			return f.human.signRS256(t, testKID, claims)
		}},
		{name: "service assertion missing", token: func() string {
			claims := valid()
			delete(claims, "kubernetes.io")
			return f.executorToken(t, claims)
		}, want: errExecutorNotService},
		{name: "service assertion wrong", token: func() string {
			claims := valid()
			claims["kubernetes.io"] = map[string]any{"namespace": "default"}
			return f.executorToken(t, claims)
		}, want: errExecutorNotService},
		{name: "service assertion not a string", token: func() string {
			claims := valid()
			claims["kubernetes.io"] = map[string]any{"namespace": []any{testExecutorNamespace}}
			return f.executorToken(t, claims)
		}, want: errExecutorNotService},
		{name: "service assertion as a dotted top-level key", token: func() string {
			claims := valid()
			delete(claims, "kubernetes.io")
			claims["kubernetes.io.namespace"] = testExecutorNamespace
			return f.executorToken(t, claims)
		}, want: errExecutorNotService},
		{name: "act", token: func() string {
			claims := valid()
			claims["act"] = map[string]any{"sub": "someone"}
			return f.executorToken(t, claims)
		}, want: errExecutorDelegated},
		{name: "may_act", token: func() string {
			claims := valid()
			claims["may_act"] = map[string]any{"sub": "someone"}
			return f.executorToken(t, claims)
		}, want: errExecutorDelegated},
		{name: "overage", token: func() string {
			claims := valid()
			claims["_claim_names"] = map[string]any{"groups": "src1"}
			return f.executorToken(t, claims)
		}, want: errExecutorDelegated},
		{name: "hasgroups", token: func() string {
			claims := valid()
			claims["hasgroups"] = true
			return f.executorToken(t, claims)
		}, want: errExecutorDelegated},
		{name: "configured delegation claim", token: func() string {
			claims := valid()
			claims["delegated_by"] = []string{"alice"}
			return f.executorToken(t, claims)
		}, want: errExecutorDelegated},
		{name: "executor and workspace audiences", token: func() string {
			claims := valid()
			claims["aud"] = []string{testExecutorAudience, testAudience}
			return f.executorToken(t, claims)
		}, want: errExecutorAudienceConfusion},
		{name: "executor and approver audiences", token: func() string {
			claims := valid()
			claims["aud"] = []string{testApproverAudience, testExecutorAudience}
			return f.executorToken(t, claims)
		}, want: errExecutorAudienceConfusion},
		{name: "unmapped subject", token: func() string {
			return f.executorToken(t, executorClaims(f.executor, f.now,
				"system:serviceaccount:shoal:intruder"))
		}, want: errExecutorUnmapped},
		{name: "subject differing only in case", token: func() string {
			return f.executorToken(t, executorClaims(f.executor, f.now,
				strings.ToUpper(testExecutorSubject)))
		}, want: errExecutorUnmapped},
		{name: "subject with surrounding whitespace", token: func() string {
			return f.executorToken(t, executorClaims(f.executor, f.now,
				" "+testExecutorSubject))
		}, want: errExecutorUnmapped},
		{name: "no subject", token: func() string {
			claims := valid()
			delete(claims, "sub")
			return f.executorToken(t, claims)
		}, want: errMissingSubject},
		{name: "no expiry", token: func() string {
			claims := valid()
			delete(claims, "exp")
			return f.executorToken(t, claims)
		}},
		{name: "expired", token: func() string {
			claims := valid()
			claims["exp"] = f.now.Add(-time.Hour).Unix()
			return f.executorToken(t, claims)
		}},
		{name: "another audience", token: func() string {
			claims := valid()
			claims["aud"] = []string{"kubernetes"}
			return f.executorToken(t, claims)
		}},
	} {
		t.Run(refused.name, func(t *testing.T) {
			request := bearerRequest(refused.token())
			_, err := f.authn.authenticate(request)
			if err == nil {
				t.Fatal("the token was minted")
			}
			if refused.want != nil && !errors.Is(err, refused.want) {
				t.Fatalf("refusal = %v, want %v", err, refused.want)
			}
			if _, err := f.authn.Authenticate(request); err == nil ||
				err.Error() != oidcDenied().Error() {
				t.Fatalf("Authenticate = %v, want the generic denial", err)
			}
		})
	}
}

// TestBothAudiencesAreRefusedOnTheHumanBranch: a human-issuer token naming a
// workspace audience and the executor audience is refused, not minted as the
// human it otherwise is. The routing sends it to the executor branch, which
// refuses the human issuer; the human branch's own check is exercised
// directly, since the routing makes it unreachable.
func TestBothAudiencesAreRefusedOnTheHumanBranch(t *testing.T) {
	t.Parallel()
	f := newExecutorFixture(t)
	claims := f.human.defaultClaims(f.now)
	claims["aud"] = []string{testAudience, testExecutorAudience}
	if _, err := f.authn.authenticate(
		bearerRequest(f.human.signRS256(t, testKID, claims))); err == nil {
		t.Fatal("a human token on the workspace and executor audiences was minted")
	}
	if err := f.authn.executor.refuseOnHumanBranch(claims); !errors.Is(
		err, errExecutorAudienceConfusion) {
		t.Fatalf("the human branch's check = %v", err)
	}
	// Control: the same token on the workspace audience alone is a reader.
	claims["aud"] = []string{testAudience}
	decision, err := f.authn.authenticate(bearerRequest(f.human.signRS256(t, testKID, claims)))
	if err != nil {
		t.Fatalf("a reader token: %v", err)
	}
	for _, operation := range decision.AllowedOperations() {
		if operation == auth.OperationExecute {
			t.Fatal("a reader holds execute")
		}
	}
}

// TestExecutorAndHumanKeyCachesAreIsolated: both fake issuers publish a key
// under the same kid. A token claiming one issuer, signed with the other's
// key, is refused in both directions; each correctly signed token passes; and
// each branch fetched only its own issuer's JWKS.
func TestExecutorAndHumanKeyCachesAreIsolated(t *testing.T) {
	t.Parallel()
	f := newExecutorFixture(t)
	if f.human.keys[testKID] == f.executor.keys[testKID] {
		t.Fatal("the fixture's issuers share a key")
	}

	// Controls first, so both caches are warm: a refusal below is the
	// signature's, not a cold cache's.
	f.mustMint(t, bearerRequest(f.executorToken(t,
		executorClaims(f.executor, f.now, testExecutorSubject))))
	_, humanJWKSBefore := f.human.counts()
	_, executorJWKSBefore := f.executor.counts()
	if _, err := f.authn.Authenticate(bearerRequest(
		f.human.signRS256(t, testKID, f.human.defaultClaims(f.now)))); err != nil {
		t.Fatalf("a human reader token: %v", err)
	}
	_, humanJWKS := f.human.counts()
	_, executorJWKS := f.executor.counts()
	if humanJWKSBefore != 0 || executorJWKSBefore != 1 ||
		humanJWKS != 1 || executorJWKS != 1 {
		t.Fatalf("JWKS fetches: human %d then %d, executor %d then %d; "+
			"want each branch to fetch its own issuer's keys once",
			humanJWKSBefore, humanJWKS, executorJWKSBefore, executorJWKS)
	}

	// An executor-issuer token signed with the human issuer's key.
	forged := f.human.signRS256(t, testKID,
		executorClaims(f.executor, f.now, testExecutorSubject))
	if _, err := f.authn.authenticate(bearerRequest(forged)); err == nil {
		t.Fatal("a human-issuer key validated an executor-issuer token")
	}
	// A human-issuer token signed with the executor issuer's key.
	forged = f.executor.signRS256(t, testKID, f.human.defaultClaims(f.now))
	if _, err := f.authn.authenticate(bearerRequest(forged)); err == nil {
		t.Fatal("an executor-issuer key validated a human-issuer token")
	}
	// The executor issuer's key on an approver token.
	forged = f.executor.signRS256(t, testKID,
		approverClaims(f.human, f.now, "bob"))
	if _, err := f.authn.authenticate(bearerRequest(forged)); err == nil {
		t.Fatal("an executor-issuer key validated an approver token")
	}
	if f.authn.executor.keys == f.authn.keys ||
		f.authn.executor.keys.metadata == f.authn.keys.metadata {
		t.Fatal("the branches share a key or discovery cache")
	}
}

// TestExecutorIssuerMayEqualTheHumanIssuer: the mapping may name -oidc-issuer
// itself. Audiences still separate the branches, and the executor branch
// still has a cache of its own.
func TestExecutorIssuerMayEqualTheHumanIssuer(t *testing.T) {
	t.Parallel()
	human := newFakeOIDCIssuer(t)
	now := time.Now()
	authenticator := newTestOIDCAuthenticator(t, executorTestConfig(
		t, human, human, fixedClock(now), executorMappingDocument(human.server.URL)))
	decision, err := authenticator.Authenticate(bearerRequest(human.signRS256(
		t, testKID, executorClaims(human, now, testExecutorSubject))))
	if err != nil {
		t.Fatalf("an executor token from the human issuer: %v", err)
	}
	if decision.ExecutorBinding() != testExecutorRef {
		t.Fatalf("binding = %q", decision.ExecutorBinding())
	}
	// A human's reader token from the same issuer is still a reader.
	reader, err := authenticator.Authenticate(bearerRequest(
		human.signRS256(t, testKID, human.defaultClaims(now))))
	if err != nil || reader.ServiceRole() != "" || reader.ExecutorBinding() != "" {
		t.Fatalf("reader = %v role %q binding %q", err, reader.ServiceRole(),
			reader.ExecutorBinding())
	}
	// And a human token on the executor audience is not an executor: the
	// service assertion is what refuses it.
	claims := human.defaultClaims(now)
	claims["aud"] = []string{testExecutorAudience}
	claims["sub"] = testExecutorSubject
	if _, err := authenticator.authenticate(bearerRequest(
		human.signRS256(t, testKID, claims))); !errors.Is(err, errExecutorNotService) {
		t.Fatalf("a human token on the executor audience = %v", err)
	}
	if authenticator.executor.keys == authenticator.keys {
		t.Fatal("one issuer, one shared cache")
	}
}

// TestExecutorMappingFileRefusals: every malformed or unsafe mapping stops the
// workspace from starting, with a reason that names the problem and never a
// configured value.
func TestExecutorMappingFileRefusals(t *testing.T) {
	t.Parallel()
	const issuer = "https://cluster.example.test"
	valid := func() map[string]any { return executorMappingDocument(issuer) }
	edit := func(change func(map[string]any)) map[string]any {
		document := valid()
		change(document)
		return document
	}
	entries := func(values ...[2]any) []map[string]any {
		result := make([]map[string]any, 0, len(values))
		for _, value := range values {
			entry := map[string]any{}
			if value[0] != nil {
				entry["subject"] = value[0]
			}
			if value[1] != nil {
				entry["executor_ref"] = value[1]
			}
			result = append(result, entry)
		}
		return result
	}
	many := make([]map[string]string, 0, executorMappingMaxEntries+1)
	for i := 0; i <= executorMappingMaxEntries; i++ {
		many = append(many, map[string]string{
			"subject":      "sa-" + string(rune('a'+i%26)) + strings.Repeat("x", i),
			"executor_ref": "ref-" + strings.Repeat("y", i+1),
		})
	}
	for _, refused := range []struct {
		name   string
		doc    map[string]any
		raw    []byte
		reason string
	}{
		{name: "unknown field", doc: edit(func(d map[string]any) {
			d["operations"] = []string{"execute", "dispatch"}
		}), reason: "does not match a field exactly"},
		{name: "unknown entry field", doc: edit(func(d map[string]any) {
			d["executors"] = []map[string]string{{
				"subject": testExecutorSubject, "executor_ref": testExecutorRef,
				"on_behalf_of": "alice",
			}}
		}), reason: "does not match a field exactly"},
		{name: "an absence assertion", doc: edit(func(d map[string]any) {
			d["service_assertion"] = map[string]any{
				"claim": []string{"idtyp"}, "absent": true,
			}
		}), reason: "does not match a field exactly"},
		{name: "duplicate key", raw: []byte(`{"version":"shoal.executors/v1",` +
			`"version":"shoal.executors/v1"}`), reason: "duplicate JSON key"},
		{name: "case-variant key", raw: []byte(`{"version":"shoal.executors/v1",` +
			`"Audience":"x"}`), reason: "does not match a field exactly"},
		{name: "trailing data", raw: append(mustJSON(t, valid()), []byte(` {}`)...),
			reason: "trailing JSON data"},
		{name: "not UTF-8", raw: []byte("{\"version\":\"\xff\"}"), reason: "not UTF-8"},
		{name: "wrong version", doc: edit(func(d map[string]any) {
			d["version"] = "shoal.executors/v2"
		}), reason: "version must be shoal.executors/v1"},
		{name: "no issuer", doc: edit(func(d map[string]any) { delete(d, "issuer") }),
			reason: "issuer is required"},
		{name: "issuer with an empty fragment", doc: edit(func(d map[string]any) {
			d["issuer"] = "https://x/#"
		}), reason: "must not contain '#'"},
		{name: "issuer with a fragment", doc: edit(func(d map[string]any) {
			d["issuer"] = "https://x/#tenant"
		}), reason: "issuer"},
		{name: "issuer with a query", doc: edit(func(d map[string]any) {
			d["issuer"] = "https://x/?tenant=a"
		}), reason: "must not contain a query"},
		{name: "http issuer", doc: edit(func(d map[string]any) {
			d["issuer"] = "http://cluster.example.test"
		}), reason: "must use https"},
		{name: "issuer with user info", doc: edit(func(d map[string]any) {
			d["issuer"] = "https://user@cluster.example.test"
		}), reason: "absolute URL"},
		{name: "upper-case issuer host", doc: edit(func(d map[string]any) {
			d["issuer"] = "https://Cluster.example.test"
		}), reason: "canonical"},
		{name: "issuer with the default port", doc: edit(func(d map[string]any) {
			d["issuer"] = "https://cluster.example.test:443"
		}), reason: "canonical"},
		{name: "issuer with a dot segment", doc: edit(func(d map[string]any) {
			d["issuer"] = "https://cluster.example.test/a/../b"
		}), reason: "dot segments"},
		{name: "issuer with an escaped character", doc: edit(func(d map[string]any) {
			d["issuer"] = "https://cluster.example.test/%7Eid"
		}), reason: "canonical"},
		{name: "issuer with surrounding whitespace", doc: edit(func(d map[string]any) {
			d["issuer"] = issuer + " "
		}), reason: "issuer"},
		{name: "relative issuer", doc: edit(func(d map[string]any) {
			d["issuer"] = "cluster.example.test"
		}), reason: "absolute URL"},
		{name: "no audience", doc: edit(func(d map[string]any) { delete(d, "audience") }),
			reason: "audience is required"},
		{name: "the workspace audience", doc: edit(func(d map[string]any) {
			d["audience"] = testAudience
		}), reason: "disjoint from the workspace and approver audiences"},
		{name: "the approver audience", doc: edit(func(d map[string]any) {
			d["audience"] = testApproverAudience
		}), reason: "disjoint from the workspace and approver audiences"},
		{name: "http jwks_uri", doc: edit(func(d map[string]any) {
			d["jwks_uri"] = "http://cluster.example.test/openid/v1/jwks"
		}), reason: "jwks_uri must use https"},
		{name: "no service assertion", doc: edit(func(d map[string]any) {
			delete(d, "service_assertion")
		}), reason: "service_assertion is required"},
		{name: "assertion without equals", doc: edit(func(d map[string]any) {
			d["service_assertion"] = map[string]any{"claim": []string{"gty"}}
		}), reason: "service_assertion.equals is required"},
		{name: "assertion equal to nothing", doc: edit(func(d map[string]any) {
			d["service_assertion"] = map[string]any{"claim": []string{"gty"}, "equals": ""}
		}), reason: "service_assertion.equals is required"},
		{name: "assertion without a claim", doc: edit(func(d map[string]any) {
			d["service_assertion"] = map[string]any{"claim": []string{}, "equals": "x"}
		}), reason: "service_assertion.claim must be a list"},
		{name: "assertion claim with an empty segment", doc: edit(func(d map[string]any) {
			d["service_assertion"] = map[string]any{"claim": []string{"a", ""}, "equals": "x"}
		}), reason: "service_assertion.claim must be a list"},
		{name: "no executors", doc: edit(func(d map[string]any) { d["executors"] = []any{} }),
			reason: "executors must not be empty"},
		{name: "too many executors", doc: edit(func(d map[string]any) { d["executors"] = many }),
			reason: "executors exceeds its bound"},
		{name: "entry without a subject", doc: edit(func(d map[string]any) {
			d["executors"] = entries([2]any{nil, testExecutorRef})
		}), reason: "executors[0].subject must be non-empty"},
		{name: "entry with a padded subject", doc: edit(func(d map[string]any) {
			d["executors"] = entries([2]any{" secret-subject", testExecutorRef})
		}), reason: "executors[0].subject must be non-empty"},
		{name: "entry with a control character", doc: edit(func(d map[string]any) {
			d["executors"] = entries([2]any{"secret\u0007subject", testExecutorRef})
		}), reason: "executors[0].subject must be non-empty"},
		{name: "entry without a reference", doc: edit(func(d map[string]any) {
			d["executors"] = entries([2]any{testExecutorSubject, nil})
		}), reason: "executors[0].executor_ref is required"},
		{name: "reference with a space", doc: edit(func(d map[string]any) {
			d["executors"] = entries([2]any{testExecutorSubject, "secret ref"})
		}), reason: "executors[0].executor_ref is not a valid executor reference"},
		{name: "reference outside ASCII", doc: edit(func(d map[string]any) {
			d["executors"] = entries([2]any{testExecutorSubject, "secrét"})
		}), reason: "executors[0].executor_ref is not a valid executor reference"},
		{name: "empty reference", doc: edit(func(d map[string]any) {
			d["executors"] = entries([2]any{testExecutorSubject, ""})
		}), reason: "executors[0].executor_ref is not a valid executor reference"},
		{name: "one subject, two references", doc: edit(func(d map[string]any) {
			d["executors"] = entries(
				[2]any{testExecutorSubject, testExecutorRef},
				[2]any{testExecutorSubject, testOtherExecutorRef})
		}), reason: "executors[1].subject is mapped twice"},
		{name: "two subjects, one reference", doc: edit(func(d map[string]any) {
			d["executors"] = entries(
				[2]any{testExecutorSubject, testExecutorRef},
				[2]any{testOtherExecutorSubject, testExecutorRef})
		}), reason: "executors[1].executor_ref is mapped twice"},
		{name: "subject too long for an identity", doc: edit(func(d map[string]any) {
			d["executors"] = entries([2]any{strings.Repeat("s", 1020), testExecutorRef})
		}), reason: "too long"},
	} {
		t.Run(refused.name, func(t *testing.T) {
			raw := refused.raw
			if raw == nil {
				raw = mustJSON(t, refused.doc)
			}
			_, err := parseExecutorMapping(raw, []string{testAudience, testApproverAudience}, false)
			if err == nil {
				t.Fatal("the mapping was accepted")
			}
			if !strings.Contains(err.Error(), "-oidc-executor-mapping-file is invalid") ||
				!strings.Contains(err.Error(), refused.reason) {
				t.Fatalf("refusal = %q, want %q", err, refused.reason)
			}
			for _, secret := range []string{"secret", "secrét"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("refusal %q echoes a configured value", err)
				}
			}
		})
	}
	// The valid document parses, and a size bound applies to the file.
	if _, err := parseExecutorMapping(mustJSON(t, valid()),
		[]string{testAudience}, false); err != nil {
		t.Fatalf("the valid mapping: %v", err)
	}
	huge := writeExecutorMapping(t, append(mustJSON(t, valid()),
		[]byte(strings.Repeat(" ", executorMappingMaxBytes))...))
	if _, err := loadExecutorMapping(huge, nil, false); err == nil ||
		!strings.Contains(err.Error(), "exceeds its size bound") {
		t.Fatalf("an oversized file = %v", err)
	}
	// And the authenticator refuses to construct over a refused file.
	human := newFakeOIDCIssuer(t)
	config := human.testConfig(time.Now)
	config.executorMappingFile = writeExecutorMapping(t, edit(func(d map[string]any) {
		d["audience"] = testAudience
	}))
	if _, err := newOIDCAuthenticator(config, time.Now); err == nil {
		t.Fatal("the authenticator started over a mapping on the workspace audience")
	}
	config.executorMappingFile = filepath.Join(t.TempDir(), "missing.json")
	if _, err := newOIDCAuthenticator(config, time.Now); err == nil {
		t.Fatal("the authenticator started without its mapping file")
	}
}

// TestExecutorMappingDigestPinsTheMapping: reordering entries keeps the
// digest; changing who maps to what moves it.
func TestExecutorMappingDigestPinsTheMapping(t *testing.T) {
	t.Parallel()
	const issuer = "https://cluster.example.test"
	parse := func(document map[string]any) auth.Digest {
		t.Helper()
		mapping, err := parseExecutorMapping(mustJSON(t, document), nil, false)
		if err != nil {
			t.Fatal(err)
		}
		return mapping.digest
	}
	base := executorMappingDocument(issuer)
	reordered := executorMappingDocument(issuer)
	reordered["executors"] = []map[string]string{
		{"subject": testOtherExecutorSubject, "executor_ref": testOtherExecutorRef},
		{"subject": testExecutorSubject, "executor_ref": testExecutorRef},
	}
	if parse(base) != parse(reordered) {
		t.Fatal("reordering entries moved the digest")
	}
	swapped := executorMappingDocument(issuer)
	swapped["executors"] = []map[string]string{
		{"subject": testExecutorSubject, "executor_ref": testOtherExecutorRef},
		{"subject": testOtherExecutorSubject, "executor_ref": testExecutorRef},
	}
	assertion := executorMappingDocument(issuer)
	assertion["service_assertion"] = map[string]any{
		"claim": []string{"kubernetes.io", "namespace"}, "equals": "default",
	}
	for name, document := range map[string]map[string]any{
		"swapped references": swapped, "another assertion": assertion,
	} {
		if parse(document) == parse(base) {
			t.Fatalf("%s did not move the digest", name)
		}
	}
}

// TestAnExecutorCredentialIsNeverAHuman is the #626 review finding: with the
// executor issuer equal to the human issuer (an Entra workload identity shares
// the tenant issuer), a mapped service account's token sent to the workspace
// audience was minted as a reader. Both human branches now refuse a token
// that (a) satisfies the executor service assertion, or (b) names a mapped
// executor subject of the executor issuer. Each rule is exercised alone.
func TestAnExecutorCredentialIsNeverAHuman(t *testing.T) {
	t.Parallel()
	human := newFakeOIDCIssuer(t)
	now := time.Now()
	shared := newTestOIDCAuthenticator(t, executorTestConfig(
		t, human, human, fixedClock(now), executorMappingDocument(human.server.URL)))
	sign := func(claims jwt.MapClaims) *http.Request {
		return bearerRequest(human.signRS256(t, testKID, claims))
	}
	refused := func(t *testing.T, authenticator *oidcAuthenticator, name string, request *http.Request) {
		t.Helper()
		if _, err := authenticator.authenticate(request); !errors.Is(err, errExecutorOnHumanBranch) {
			t.Fatalf("%s = %v, want the executor-on-a-human-branch refusal", name, err)
		}
		if _, err := authenticator.Authenticate(request); err == nil ||
			err.Error() != oidcDenied().Error() {
			t.Fatalf("%s: Authenticate = %v, want the generic denial", name, err)
		}
	}
	// workspace is the reviewer's scratch case: the executor's own claims,
	// re-addressed to the workspace audience, with a reader mapping and a
	// granted label group.
	workspace := func(subject string, assertion bool) jwt.MapClaims {
		claims := executorClaims(human, now, subject)
		claims["aud"] = []string{testAudience}
		claims["access"] = []string{"reader"}
		claims["groups"] = []string{labelGroupSecret}
		if !assertion {
			delete(claims, "kubernetes.io")
		}
		return claims
	}
	approver := func(subject string, assertion bool) jwt.MapClaims {
		claims := approverClaims(human, now, subject)
		if assertion {
			claims["kubernetes.io"] = map[string]any{"namespace": testExecutorNamespace}
		}
		return claims
	}
	const stranger = "system:serviceaccount:shoal:not-mapped"

	refused(t, shared, "the scratch case: assertion and mapped sub, workspace audience",
		sign(workspace(testExecutorSubject, true)))
	refused(t, shared, "the same on the approver audience",
		sign(approver(testExecutorSubject, true)))
	// (a) alone: the assertion, an unmapped sub.
	refused(t, shared, "the assertion alone, workspace", sign(workspace(stranger, true)))
	refused(t, shared, "the assertion alone, approver", sign(approver(stranger, true)))
	// (b) alone: a mapped sub of the shared issuer, no assertion.
	refused(t, shared, "a mapped sub alone, workspace", sign(workspace(testExecutorSubject, false)))
	refused(t, shared, "a mapped sub alone, approver", sign(approver(testExecutorSubject, false)))

	// A normal human still works on both branches.
	if decision, err := shared.Authenticate(sign(human.defaultClaims(now))); err != nil ||
		decision.Subject() != shoal.ID("oidc:"+human.server.URL+"#"+testSubject) {
		t.Fatalf("a reader = %v", err)
	}
	if _, err := shared.Authenticate(sign(approverClaims(human, now, "bob"))); err != nil {
		t.Fatalf("an approver = %v", err)
	}

	// (b) is per issuer: with a separate executor issuer, a human of the
	// human issuer whose sub happens to spell a mapped subject is a human.
	// (a) still holds across issuers.
	f := newExecutorFixture(t)
	claims := f.human.defaultClaims(f.now)
	claims["sub"] = testExecutorSubject
	if _, err := f.authn.Authenticate(bearerRequest(f.human.signRS256(t, testKID, claims))); err != nil {
		t.Fatalf("a human whose sub spells another issuer's executor = %v", err)
	}
	claims["kubernetes.io"] = map[string]any{"namespace": testExecutorNamespace}
	refused(t, f.authn, "the assertion on a human-issuer token",
		bearerRequest(f.human.signRS256(t, testKID, claims)))
}

// TestRunRefusesAnExecutorMappingToAnUnconfiguredReference: the binary, like
// the chart, refuses at startup a mapping entry whose executor_ref the host
// does not configure, naming its position and not its value. The same
// mapping starts once every reference is configured.
func TestRunRefusesAnExecutorMappingToAnUnconfiguredReference(t *testing.T) {
	mapping := writeExecutorMapping(t, executorMappingDocument("https://cluster.example.test"))
	args := func(refs string) []string {
		return []string{
			"-listen", "127.0.0.1:0", "-data", t.TempDir(),
			"-oidc-issuer", "https://issuer.example.test",
			"-oidc-audience", testAudience,
			"-oidc-authorization-claim", "access",
			"-oidc-reader-values", "reader",
			"-oidc-executor-mapping-file", mapping,
			"-fleet-executor-refs", refs,
		}
	}
	err := runBounded(t, args(testExecutorRef))
	if err == nil || !strings.Contains(err.Error(),
		"executors[1].executor_ref is not configured by -fleet-executor-refs") {
		t.Fatalf("a mapping naming an unconfigured reference = %v", err)
	}
	if strings.Contains(err.Error(), testOtherExecutorRef) {
		t.Fatalf("the refusal %q echoes the reference", err)
	}
	if err := runBounded(t, args(testExecutorRef+","+testOtherExecutorRef)); err != nil {
		t.Fatalf("a mapping naming configured references = %v", err)
	}
}
