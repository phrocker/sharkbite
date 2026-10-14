// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	testAudience = "shoal-api"
	testKID      = "test-signing-key-1"
	testSubject  = "user-123"
)

type fakeOIDCIssuer struct {
	server *httptest.Server
	keys   map[string]*rsa.PrivateKey

	mu                sync.Mutex
	discoveryHits     int
	jwksHits          int
	discoveryStatus   int
	jwksStatus        int
	discoveryIssuer   string
	discoveryJWKSURI  string
	authorizationPath string
	tokenPath         string
	// subjectTypes is the discovery subject_types_supported; nil omits it.
	subjectTypes []string
	jwksStarted  chan<- struct{}
	jwksRelease  <-chan struct{}
}

func newFakeOIDCIssuer(t *testing.T) *fakeOIDCIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate signing key: %v", err)
	}
	issuer := &fakeOIDCIssuer{
		keys:              map[string]*rsa.PrivateKey{testKID: key},
		discoveryStatus:   http.StatusOK,
		jwksStatus:        http.StatusOK,
		authorizationPath: "/authorize",
		tokenPath:         "/token",
		subjectTypes:      []string{"public"},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", issuer.serveDiscovery)
	mux.HandleFunc("/metadata", issuer.serveDiscovery)
	mux.HandleFunc("/keys", issuer.serveJWKS)
	issuer.server = httptest.NewServer(mux)
	t.Cleanup(issuer.server.Close)
	return issuer
}

func (f *fakeOIDCIssuer) serveDiscovery(
	writer http.ResponseWriter,
	_ *http.Request,
) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.discoveryHits++
	if f.discoveryStatus != http.StatusOK {
		writer.WriteHeader(f.discoveryStatus)
		return
	}
	issuer := f.discoveryIssuer
	if issuer == "" {
		issuer = f.server.URL
	}
	jwksURI := f.discoveryJWKSURI
	if jwksURI == "" {
		jwksURI = f.server.URL + "/keys"
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(oidcMetadata{
		Issuer:                issuer,
		JWKSURI:               jwksURI,
		AuthorizationEndpoint: f.server.URL + f.authorizationPath,
		TokenEndpoint:         f.server.URL + f.tokenPath,
		SubjectTypesSupported: f.subjectTypes,
	})
}

func (f *fakeOIDCIssuer) serveJWKS(
	writer http.ResponseWriter,
	_ *http.Request,
) {
	f.mu.Lock()
	f.jwksHits++
	status := f.jwksStatus
	body := f.jwksJSONLocked()
	started := f.jwksStarted
	release := f.jwksRelease
	f.mu.Unlock()
	if started != nil {
		select {
		case started <- struct{}{}:
		default:
		}
	}
	if release != nil {
		<-release
	}
	if status != http.StatusOK {
		writer.WriteHeader(status)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	_, _ = writer.Write(body)
}

func (f *fakeOIDCIssuer) jwksJSONLocked() []byte {
	type webKey struct {
		Kty string `json:"kty"`
		Kid string `json:"kid"`
		Use string `json:"use"`
		Alg string `json:"alg"`
		N   string `json:"n"`
		E   string `json:"e"`
	}
	document := struct {
		Keys []webKey `json:"keys"`
	}{}
	for kid, key := range f.keys {
		document.Keys = append(document.Keys, webKey{
			Kty: "RSA",
			Kid: kid,
			Use: "sig",
			N:   base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
			E: base64.RawURLEncoding.EncodeToString(
				big.NewInt(int64(key.PublicKey.E)).Bytes()),
		})
	}
	raw, _ := json.Marshal(document)
	return raw
}

func (f *fakeOIDCIssuer) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.discoveryHits, f.jwksHits
}

func (f *fakeOIDCIssuer) defaultClaims(now time.Time) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":    f.server.URL,
		"aud":    []string{testAudience, "another-audience"},
		"sub":    testSubject,
		"access": []string{"reader"},
		"iat":    now.Add(-time.Minute).Unix(),
		"nbf":    now.Add(-time.Minute).Unix(),
		"exp":    now.Add(time.Hour).Unix(),
	}
}

func (f *fakeOIDCIssuer) signRS256(
	t *testing.T,
	kid string,
	claims jwt.MapClaims,
) string {
	t.Helper()
	f.mu.Lock()
	key := f.keys[kid]
	f.mu.Unlock()
	if key == nil {
		var err error
		key, err = rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate ephemeral key: %v", err)
		}
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = kid
	signed, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

func (f *fakeOIDCIssuer) testConfig(clock func() time.Time) oidcConfig {
	return oidcConfig{
		issuer:             f.server.URL,
		audiences:          []string{testAudience},
		authorizationClaim: "access",
		readerClaimValues:  []string{"reader"},
		contributorValues:  []string{"writer"},
		httpClient:         f.server.Client(),
		clock:              clock,
	}
}

func fixedClock(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

func bearerRequest(token string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/api/v1/meta", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	return request
}

func newTestOIDCAuthenticator(
	t *testing.T,
	config oidcConfig,
) *oidcAuthenticator {
	t.Helper()
	authenticator, err := newOIDCAuthenticator(config, time.Now)
	if err != nil {
		t.Fatalf("newOIDCAuthenticator: %v", err)
	}
	return authenticator
}

func TestOIDCValidTokenMintsReaderDecision(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	now := time.Now()
	authenticator := newTestOIDCAuthenticator(
		t, issuer.testConfig(fixedClock(now)))
	token := issuer.signRS256(t, testKID, issuer.defaultClaims(now))

	decision, err := authenticator.Authenticate(bearerRequest(token))
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	wantSubject := "oidc:" + issuer.server.URL + "#" + testSubject
	if decision.Subject() != shoal.ID(wantSubject) {
		t.Fatalf("subject = %q, want %q", decision.Subject(), wantSubject)
	}
	if decision.Actor() != oidcActor {
		t.Fatalf("actor = %q, want %q", decision.Actor(), oidcActor)
	}
	if !grantsWorkspaceSource(decision) {
		t.Fatal("mapped reader was not granted the workspace source")
	}
	if operationsContain(decision.AllowedOperations(), auth.OperationIngest) {
		t.Fatal("reader was granted ingest")
	}
	if !operationsContain(decision.AllowedOperations(), auth.OperationRead) {
		t.Fatal("reader was not granted read")
	}
	if !strings.HasPrefix(string(decision.RequestID()), "oidc-request-") {
		t.Fatalf("request ID = %q", decision.RequestID())
	}
	// A correlation ID too, and not the same value as the request ID. Without
	// one, every dispatch, admission and approval route refused every OIDC
	// request with "dispatch correlation ID is required" (#524) — and nothing
	// caught it, because no test drove an authenticated decision into those
	// routes and this one asserted only the request identity.
	if !strings.HasPrefix(
		string(decision.CorrelationID()), "oidc-correlation-") {
		t.Fatalf("correlation ID = %q, so the dispatch surface is "+
			"unreachable through OIDC", decision.CorrelationID())
	}
	if decision.CorrelationID() == decision.RequestID() {
		t.Fatal("the request and correlation identities are one value, so " +
			"the correlation threads nothing")
	}
	second, err := authenticator.Authenticate(bearerRequest(token))
	if err != nil {
		t.Fatal(err)
	}
	if second.RequestID() == decision.RequestID() {
		t.Fatal("two requests shared one request identity")
	}
	if second.CorrelationID() == decision.CorrelationID() {
		t.Fatal("two unrelated requests shared one correlation identity")
	}

	// And a caller threading an upstream trace is honoured, which is the
	// field's purpose for a gateway or an agent acting for a user.
	threaded := bearerRequest(token)
	threaded.Header.Set(CorrelationIDHeader, "upstream-trace-9")
	carried, err := authenticator.Authenticate(threaded)
	if err != nil {
		t.Fatal(err)
	}
	if carried.CorrelationID() != "upstream-trace-9" {
		t.Fatalf("a supplied correlation was not honoured: %q",
			carried.CorrelationID())
	}
}

func TestLegacyEntraCompatibilityPreservesBehavior(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	now := time.Now()
	config := applyLegacyEntraCompatibility(oidcConfig{
		httpClient: issuer.server.Client(),
		clock:      fixedClock(now),
	}, legacyEntraConfig{
		issuer:           issuer.server.URL,
		audience:         testAudience,
		readerRoles:      []string{"Shoal.Reader"},
		contributorRoles: []string{"Shoal.Contributor"},
	})
	if config.subjectClaim != "oid" || config.subjectFallbackClaim != "sub" ||
		config.authorizationClaim != "roles" ||
		config.identityPrefix != legacyEntraPrefix ||
		config.defaultActor != legacyEntraActor ||
		config.browserClientID != testAudience ||
		config.browserScope != "openid profile "+testAudience+"/.default" {
		t.Fatalf("legacy translation = %+v", config)
	}
	authenticator := newTestOIDCAuthenticator(t, config)
	claims := issuer.defaultClaims(now)
	delete(claims, "access")
	claims["oid"] = " object-123 "
	claims["roles"] = []string{" Shoal.Reader "}
	decision, err := authenticator.Authenticate(bearerRequest(
		issuer.signRS256(t, testKID, claims)))
	if err != nil {
		t.Fatalf("legacy role token rejected: %v", err)
	}
	if decision.Subject() != "entra:object-123" ||
		decision.Actor() != legacyEntraActor ||
		!operationsContain(decision.AllowedOperations(), auth.OperationRead) {
		t.Fatalf(
			"legacy decision = subject %q actor %q operations %v",
			decision.Subject(), decision.Actor(), decision.AllowedOperations())
	}

	delete(claims, "oid")
	delete(claims, "roles")
	decision, err = authenticator.Authenticate(bearerRequest(
		issuer.signRS256(t, testKID, claims)))
	if err != nil {
		t.Fatalf("legacy sub fallback/unmapped token rejected: %v", err)
	}
	if decision.Subject() != "entra:"+testSubject ||
		!operationsContain(decision.AllowedOperations(), auth.OperationList) ||
		grantsWorkspaceSource(decision) {
		t.Fatalf(
			"legacy unmapped decision = subject %q operations %v sources %q",
			decision.Subject(), decision.AllowedOperations(),
			decision.PermittedSourceIDs())
	}
}

func TestLegacyEntraTenantDerivesIssuerAndBrowserDefaults(t *testing.T) {
	config := applyLegacyEntraCompatibility(oidcConfig{}, legacyEntraConfig{
		tenantID:    "tenant-123",
		audience:    "client-456",
		readerRoles: []string{"reader"},
	})
	if config.issuer !=
		"https://login.microsoftonline.com/tenant-123/v2.0" {
		t.Fatalf("derived issuer = %q", config.issuer)
	}
	if config.authorizationEndpoint !=
		"https://login.microsoftonline.com/tenant-123/oauth2/v2.0/authorize" ||
		config.tokenEndpoint !=
			"https://login.microsoftonline.com/tenant-123/oauth2/v2.0/token" ||
		config.legacyTenantID != "tenant-123" ||
		config.legacyAuthority !=
			"https://login.microsoftonline.com/tenant-123" {
		t.Fatalf("legacy browser defaults = %+v", config)
	}
}

func TestLegacyEntraWithoutRoleMappingsRemainsFailClosed(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	now := time.Now()
	config := applyLegacyEntraCompatibility(oidcConfig{
		httpClient: issuer.server.Client(),
		clock:      fixedClock(now),
	}, legacyEntraConfig{
		issuer:   issuer.server.URL,
		audience: testAudience,
	})
	authenticator := newTestOIDCAuthenticator(t, config)
	claims := issuer.defaultClaims(now)
	delete(claims, "access")
	decision, err := authenticator.Authenticate(bearerRequest(
		issuer.signRS256(t, testKID, claims)))
	if err != nil {
		t.Fatalf("legacy token without role mappings rejected: %v", err)
	}
	if !operationsContain(decision.AllowedOperations(), auth.OperationList) ||
		grantsWorkspaceSource(decision) {
		t.Fatalf(
			"legacy no-role decision = operations %v sources %q",
			decision.AllowedOperations(), decision.PermittedSourceIDs())
	}
}

func TestLegacyEntraRejectsScalarRolesClaim(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	now := time.Now()
	config := applyLegacyEntraCompatibility(oidcConfig{
		httpClient: issuer.server.Client(),
		clock:      fixedClock(now),
	}, legacyEntraConfig{
		issuer:      issuer.server.URL,
		audience:    testAudience,
		readerRoles: []string{"Shoal.Reader"},
	})
	authenticator := newTestOIDCAuthenticator(t, config)
	claims := issuer.defaultClaims(now)
	delete(claims, "access")
	claims["roles"] = "Shoal.Reader"
	if _, err := authenticator.authenticate(bearerRequest(
		issuer.signRS256(t, testKID, claims),
	)); !errors.Is(err, errMalformedClaim) {
		t.Fatalf("legacy scalar roles error = %v", err)
	}
}

func TestLegacyOptionAloneDoesNotEnableAuthentication(t *testing.T) {
	config := applyLegacyEntraCompatibility(oidcConfig{}, legacyEntraConfig{
		allowedAlgorithms: []string{"RS256"},
	})
	if config.configured() {
		t.Fatalf("standalone legacy algorithm unexpectedly enabled OIDC: %+v", config)
	}
}

func TestAzureTemplateRetainsDeprecatedEntraParameters(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(
		"..", "..", "deploy", "shoal-explore-web", "azure", "main.bicep"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{
		"param entraTenantId string",
		"param entraClientId string",
		"param entraReaderRoles string = 'Shoal.Reader'",
		"param entraContributorRoles string = 'Shoal.Contributor'",
		"param entraIssuer string",
		"param entraJwksUri string",
		"SHOAL_ENTRA_TENANT: entraTenantId",
		"SHOAL_ENTRA_CLIENT_ID: entraClientId",
		"SHOAL_ENTRA_READER_ROLES: entraReaderRoles",
		"SHOAL_ENTRA_CONTRIBUTOR_ROLES: entraContributorRoles",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("Azure compatibility template missing %q", want)
		}
	}
}

func TestOIDCClaimMappingPreservesIdentityAndDelegation(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	now := time.Now()
	config := issuer.testConfig(fixedClock(now))
	config.subjectClaim = "principal"
	config.actorClaim = "actor"
	config.clientIDClaim = "client"
	config.delegationClaim = "delegation"
	authenticator := newTestOIDCAuthenticator(t, config)

	claims := issuer.defaultClaims(now)
	delete(claims, "sub")
	claims["principal"] = " principal-7 "
	claims["actor"] = " service-4 "
	claims["client"] = " client-9 "
	claims["delegation"] = []string{" delegate-a ", "delegate-b"}
	claims["access"] = "writer"
	decision, err := authenticator.Authenticate(
		bearerRequest(issuer.signRS256(t, testKID, claims)))
	if err != nil {
		t.Fatalf("mapped token rejected: %v", err)
	}
	// A non-sub subject claim has a namespace of its own (#546), and the
	// actor, client and delegation values are minted under the same prefix.
	prefix := "oidc:" + issuer.server.URL + "#" + subjectClaimTag("principal") + "#"
	if decision.Subject() != shoal.ID(prefix+" principal-7 ") ||
		decision.Actor() != shoal.ID(prefix+" service-4 ") ||
		decision.ClientID() != shoal.ID(prefix+" client-9 ") {
		t.Fatalf(
			"mapped identity = subject %q actor %q client %q",
			decision.Subject(), decision.Actor(), decision.ClientID())
	}
	wantDelegation := []shoal.ID{
		shoal.ID(prefix + " delegate-a "),
		shoal.ID(prefix + "delegate-b"),
	}
	gotDelegation := decision.OnBehalfOf()
	if len(gotDelegation) != len(wantDelegation) {
		t.Fatalf("delegation = %q", gotDelegation)
	}
	for index := range wantDelegation {
		if gotDelegation[index] != wantDelegation[index] {
			t.Fatalf("delegation = %q, want %q", gotDelegation, wantDelegation)
		}
	}
	if !operationsContain(decision.AllowedOperations(), auth.OperationIngest) {
		t.Fatal("contributor mapping did not grant ingest")
	}
	if operationsContain(decision.AllowedOperations(), auth.OperationDelegate) {
		t.Fatal("contributor mapping granted agent delegation")
	}
}

func TestOIDCFleetMappingIsSeparateFromContributor(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	now := time.Now()
	config := issuer.testConfig(fixedClock(now))
	config.fleetValues = []string{"fleet"}
	authenticator := newTestOIDCAuthenticator(t, config)

	claims := issuer.defaultClaims(now)
	claims["access"] = "fleet"
	decision, err := authenticator.Authenticate(
		bearerRequest(issuer.signRS256(t, testKID, claims)))
	if err != nil {
		t.Fatalf("fleet token rejected: %v", err)
	}
	for _, operation := range []auth.Operation{
		auth.OperationAgentRegister,
		auth.OperationAgentRevoke,
		auth.OperationDelegate,
		auth.OperationDispatch,
		auth.OperationInvoke,
		auth.OperationSubscriptionCreate,
		auth.OperationEventPublish,
	} {
		if !operationsContain(decision.AllowedOperations(), operation) {
			t.Fatalf("fleet mapping omitted %s", operation)
		}
	}
	if operationsContain(decision.AllowedOperations(), auth.OperationIngest) {
		t.Fatal("fleet mapping granted contributor ingest")
	}

	claims["access"] = []string{"fleet", "writer"}
	decision, err = authenticator.Authenticate(
		bearerRequest(issuer.signRS256(t, testKID, claims)))
	if err != nil {
		t.Fatalf("combined token rejected: %v", err)
	}
	if !operationsContain(decision.AllowedOperations(), auth.OperationDelegate) ||
		!operationsContain(decision.AllowedOperations(), auth.OperationIngest) {
		t.Fatalf("combined mapping operations = %v", decision.AllowedOperations())
	}
}

func TestOIDCRejectsMalformedConfiguredIdentityClaims(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	now := time.Now()
	cases := []struct {
		name       string
		claimName  string
		claimValue any
		configure  func(*oidcConfig)
	}{
		{
			name:       "actor",
			claimName:  "actor",
			claimValue: 7,
			configure: func(config *oidcConfig) {
				config.actorClaim = "actor"
			},
		},
		{
			name:       "client",
			claimName:  "client",
			claimValue: []string{"not-a-string"},
			configure: func(config *oidcConfig) {
				config.clientIDClaim = "client"
			},
		},
		{
			name:       "delegation",
			claimName:  "delegation",
			claimValue: []any{"delegate", 7},
			configure: func(config *oidcConfig) {
				config.delegationClaim = "delegation"
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := issuer.testConfig(fixedClock(now))
			testCase.configure(&config)
			authenticator := newTestOIDCAuthenticator(t, config)
			claims := issuer.defaultClaims(now)
			claims[testCase.claimName] = testCase.claimValue
			_, err := authenticator.authenticate(bearerRequest(
				issuer.signRS256(t, testKID, claims)))
			if !errors.Is(err, errMalformedClaim) {
				t.Fatalf("malformed %s claim error = %v", testCase.name, err)
			}
		})
	}
}

func TestOIDCRejectsUnmappedAndMalformedClaims(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	now := time.Now()
	authenticator := newTestOIDCAuthenticator(
		t, issuer.testConfig(fixedClock(now)))
	cases := []struct {
		name string
		edit func(jwt.MapClaims)
		want error
	}{
		{
			name: "missing authorization claim",
			edit: func(claims jwt.MapClaims) { delete(claims, "access") },
			want: errMissingMappedClaim,
		},
		{
			name: "unmapped authorization value",
			edit: func(claims jwt.MapClaims) { claims["access"] = []string{"unknown"} },
			want: errUnmappedAuthorization,
		},
		{
			name: "authorization value is not whitespace-normalized",
			edit: func(claims jwt.MapClaims) { claims["access"] = []string{" reader "} },
			want: errUnmappedAuthorization,
		},
		{
			name: "malformed authorization claim",
			edit: func(claims jwt.MapClaims) { claims["access"] = []any{"reader", 7} },
			want: errMalformedClaim,
		},
		{
			name: "missing subject",
			edit: func(claims jwt.MapClaims) { delete(claims, "sub") },
			want: errMissingSubject,
		},
		{
			name: "malformed subject",
			edit: func(claims jwt.MapClaims) { claims["sub"] = 42 },
			want: errMalformedClaim,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			claims := issuer.defaultClaims(now)
			testCase.edit(claims)
			token := issuer.signRS256(t, testKID, claims)
			_, err := authenticator.authenticate(bearerRequest(token))
			if !errors.Is(err, testCase.want) {
				t.Fatalf("error = %v, want %v", err, testCase.want)
			}
			if _, err := authenticator.Authenticate(bearerRequest(token)); err == nil {
				t.Fatal("invalid or unmapped claims crossed the public boundary")
			}
		})
	}
}

func TestOIDCRejectsIssuerAudienceAndTimeFailures(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	now := time.Now()
	tests := []struct {
		name string
		edit func(jwt.MapClaims)
		want error
	}{
		{
			name: "issuer",
			edit: func(claims jwt.MapClaims) { claims["iss"] = "https://issuer.invalid" },
			want: jwt.ErrTokenInvalidIssuer,
		},
		{
			name: "audience",
			edit: func(claims jwt.MapClaims) { claims["aud"] = "other-api" },
			want: jwt.ErrTokenInvalidAudience,
		},
		{
			name: "expiry",
			edit: func(claims jwt.MapClaims) {
				claims["exp"] = now.Add(-2 * time.Hour).Unix()
			},
			want: jwt.ErrTokenExpired,
		},
		{
			name: "not before",
			edit: func(claims jwt.MapClaims) {
				claims["nbf"] = now.Add(2 * time.Hour).Unix()
			},
			want: jwt.ErrTokenNotValidYet,
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			authenticator := newTestOIDCAuthenticator(
				t, issuer.testConfig(fixedClock(now)))
			claims := issuer.defaultClaims(now)
			testCase.edit(claims)
			_, err := authenticator.authenticate(bearerRequest(
				issuer.signRS256(t, testKID, claims)))
			if !errors.Is(err, testCase.want) {
				t.Fatalf("error = %v, want %v", err, testCase.want)
			}
		})
	}
}

func TestOIDCAcceptsAnyConfiguredAudience(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	now := time.Now()
	config := issuer.testConfig(fixedClock(now))
	config.audiences = []string{"unused-audience", testAudience}
	authenticator := newTestOIDCAuthenticator(t, config)
	if _, err := authenticator.Authenticate(bearerRequest(
		issuer.signRS256(t, testKID, issuer.defaultClaims(now)))); err != nil {
		t.Fatalf("configured audience in aud array was rejected: %v", err)
	}
}

func TestOIDCAcceptsWithinClockSkew(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	now := time.Now()
	config := issuer.testConfig(fixedClock(now))
	config.clockSkew = time.Minute
	authenticator := newTestOIDCAuthenticator(t, config)
	claims := issuer.defaultClaims(now)
	claims["exp"] = now.Add(-30 * time.Second).Unix()
	decision, err := authenticator.Authenticate(bearerRequest(
		issuer.signRS256(t, testKID, claims)))
	if err != nil {
		t.Fatalf("token within clock skew was rejected: %v", err)
	}
	authority, err := auth.NewAuthorityWithClock(fixedClock(now))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authority.Binder().Bind(context.Background(), decision); err != nil {
		t.Fatalf("decision within clock skew was rejected by binder: %v", err)
	}
}

func TestOIDCRejectsSignatureAlgorithmAndKeyFailures(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	now := time.Now()
	authenticator := newTestOIDCAuthenticator(
		t, issuer.testConfig(fixedClock(now)))

	attacker, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	badSignature := jwt.NewWithClaims(
		jwt.SigningMethodRS256, issuer.defaultClaims(now))
	badSignature.Header["kid"] = testKID
	forged, err := badSignature.SignedString(attacker)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authenticator.authenticate(
		bearerRequest(forged)); !errors.Is(err, jwt.ErrTokenSignatureInvalid) {
		t.Fatalf("bad signature error = %v", err)
	}

	missingKID := jwt.NewWithClaims(
		jwt.SigningMethodRS256, issuer.defaultClaims(now))
	unsignedKid, err := missingKID.SignedString(issuer.keys[testKID])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authenticator.authenticate(
		bearerRequest(unsignedKid)); !errors.Is(err, errMissingKeyID) {
		t.Fatalf("missing kid error = %v", err)
	}

	none := jwt.NewWithClaims(jwt.SigningMethodNone, issuer.defaultClaims(now))
	none.Header["kid"] = testKID
	unsigned, err := none.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authenticator.authenticate(
		bearerRequest(unsigned)); !errors.Is(err, jwt.ErrTokenSignatureInvalid) {
		t.Fatalf("alg:none error = %v", err)
	}

	issuer.mu.Lock()
	publicN := issuer.keys[testKID].PublicKey.N.Bytes()
	issuer.mu.Unlock()
	hmac := jwt.NewWithClaims(jwt.SigningMethodHS256, issuer.defaultClaims(now))
	hmac.Header["kid"] = testKID
	confused, err := hmac.SignedString(publicN)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := authenticator.authenticate(
		bearerRequest(confused)); !errors.Is(err, jwt.ErrTokenSignatureInvalid) {
		t.Fatalf("algorithm-confusion error = %v", err)
	}
}

func TestOIDCAcceptsConfiguredPSSAndECDSAAlgorithms(t *testing.T) {
	tests := []struct {
		name       string
		method     jwt.SigningMethod
		privateKey func(*testing.T) (any, jsonWebKey)
	}{
		{
			name:   "PS256",
			method: jwt.SigningMethodPS256,
			privateKey: func(t *testing.T) (any, jsonWebKey) {
				key, err := rsa.GenerateKey(rand.Reader, 2048)
				if err != nil {
					t.Fatal(err)
				}
				return key, jsonWebKey{
					Kty: "RSA", Kid: testKID, Use: "sig",
					KeyOps: []string{"verify"},
					N:      base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes()),
					E: base64.RawURLEncoding.EncodeToString(
						big.NewInt(int64(key.PublicKey.E)).Bytes()),
				}
			},
		},
		{
			name:   "ES256",
			method: jwt.SigningMethodES256,
			privateKey: func(t *testing.T) (any, jsonWebKey) {
				return testECSigningKey(t, elliptic.P256(), "P-256")
			},
		},
		{
			name:   "ES384",
			method: jwt.SigningMethodES384,
			privateKey: func(t *testing.T) (any, jsonWebKey) {
				return testECSigningKey(t, elliptic.P384(), "P-384")
			},
		},
		{
			name:   "ES512",
			method: jwt.SigningMethodES512,
			privateKey: func(t *testing.T) (any, jsonWebKey) {
				return testECSigningKey(t, elliptic.P521(), "P-521")
			},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			privateKey, key := testCase.privateKey(t)
			document, err := json.Marshal(struct {
				Keys []jsonWebKey `json:"keys"`
			}{Keys: []jsonWebKey{key}})
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(
				func(writer http.ResponseWriter, _ *http.Request) {
					writer.Header().Set("Content-Type", "application/json")
					_, _ = writer.Write(document)
				},
			))
			defer server.Close()

			now := time.Now()
			config := oidcConfig{
				issuer:             server.URL,
				audiences:          []string{testAudience},
				jwksURI:            server.URL,
				allowedAlgorithms:  []string{testCase.method.Alg()},
				authorizationClaim: "access",
				readerClaimValues:  []string{"reader"},
				httpClient:         server.Client(),
				clock:              fixedClock(now),
			}
			claims := jwt.MapClaims{
				"iss": server.URL, "aud": testAudience, "sub": testSubject,
				"access": "reader", "exp": now.Add(time.Hour).Unix(),
			}
			token := jwt.NewWithClaims(testCase.method, claims)
			token.Header["kid"] = testKID
			signed, err := token.SignedString(privateKey)
			if err != nil {
				t.Fatal(err)
			}
			authenticator := newTestOIDCAuthenticator(t, config)
			if _, err := authenticator.Authenticate(
				bearerRequest(signed)); err != nil {
				t.Fatalf("configured %s token rejected: %v", testCase.name, err)
			}

			config.allowedAlgorithms = []string{"RS256"}
			rejecting := newTestOIDCAuthenticator(t, config)
			if _, err := rejecting.Authenticate(bearerRequest(signed)); err == nil {
				t.Fatalf("%s token bypassed the configured allowlist", testCase.name)
			}
		})
	}
}

func testECSigningKey(
	t *testing.T,
	curve elliptic.Curve,
	name string,
) (any, jsonWebKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key, jsonWebKey{
		Kty: "EC", Kid: testKID, Use: "sig", KeyOps: []string{"verify"},
		Crv: name,
		X:   base64.RawURLEncoding.EncodeToString(key.PublicKey.X.Bytes()),
		Y:   base64.RawURLEncoding.EncodeToString(key.PublicKey.Y.Bytes()),
	}
}

func TestOIDCKeyfuncRejectsMismatchedSigningMethod(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	now := time.Now()
	authenticator := newTestOIDCAuthenticator(
		t, issuer.testConfig(fixedClock(now)))
	if _, err := authenticator.authenticate(bearerRequest(
		issuer.signRS256(t, testKID, issuer.defaultClaims(now)))); err != nil {
		t.Fatal(err)
	}
	keyFunc := authenticator.keyFuncForContext(context.Background())
	hmac := &jwt.Token{
		Header: map[string]any{"kid": testKID},
		Method: jwt.SigningMethodHS256,
	}
	if _, err := keyFunc(hmac); !errors.Is(err, errUnexpectedSigningMethod) {
		t.Fatalf("HMAC method error = %v", err)
	}
	ecdsa := &jwt.Token{
		Header: map[string]any{"kid": testKID},
		Method: jwt.SigningMethodES256,
	}
	if _, err := keyFunc(ecdsa); !errors.Is(err, errKeyMethodMismatch) {
		t.Fatalf("ECDSA/RSA mismatch error = %v", err)
	}
}

func TestOIDCJWKAlgorithmConstraintIsEnforced(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	n := base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(
		big.NewInt(int64(key.PublicKey.E)).Bytes())
	document := `{"keys":[{"kty":"RSA","kid":"` + testKID +
		`","use":"sig","alg":"RS256","n":"` + n + `","e":"` + e + `"}]}`
	server := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(document))
		},
	))
	defer server.Close()

	now := time.Now()
	config := oidcConfig{
		issuer:             server.URL,
		audiences:          []string{testAudience},
		jwksURI:            server.URL,
		allowedAlgorithms:  []string{"RS256", "PS256"},
		authorizationClaim: "access",
		readerClaimValues:  []string{"reader"},
		httpClient:         server.Client(),
		clock:              fixedClock(now),
	}
	claims := jwt.MapClaims{
		"iss": server.URL, "aud": testAudience, "sub": testSubject,
		"access": "reader", "exp": now.Add(time.Hour).Unix(),
	}
	pss := jwt.NewWithClaims(jwt.SigningMethodPS256, claims)
	pss.Header["kid"] = testKID
	signed, err := pss.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	authenticator := newTestOIDCAuthenticator(t, config)
	if _, err := authenticator.authenticate(
		bearerRequest(signed)); !errors.Is(err, errKeyAlgorithmMismatch) {
		t.Fatalf("JWK algorithm mismatch error = %v", err)
	}
}

func TestOIDCUnknownKIDDoesNotStormAndRotationRefreshes(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	base := time.Now()
	clockValue := base
	config := issuer.testConfig(func() time.Time { return clockValue })
	authenticator := newTestOIDCAuthenticator(t, config)

	unknownToken := issuer.signRS256(
		t, "unknown-kid", issuer.defaultClaims(clockValue))
	start := make(chan struct{})
	errorsSeen := make(chan error, 20)
	var wait sync.WaitGroup
	for range 20 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := authenticator.authenticate(bearerRequest(unknownToken))
			errorsSeen <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if !errors.Is(err, errNoMatchingKey) {
			t.Fatalf("unknown kid error = %v", err)
		}
	}
	discoveryHits, jwksHits := issuer.counts()
	if discoveryHits != 1 || jwksHits != 1 {
		t.Fatalf("metadata fetches = discovery %d JWKS %d", discoveryHits, jwksHits)
	}

	rotated, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	issuer.mu.Lock()
	issuer.keys["rotated-key"] = rotated
	issuer.mu.Unlock()
	clockValue = base.Add(oidcJWKSMinRefreshInterval + time.Second)
	if _, err := authenticator.Authenticate(bearerRequest(
		issuer.signRS256(t, "rotated-key", issuer.defaultClaims(clockValue)))); err != nil {
		t.Fatalf("rotated key not accepted after refresh: %v", err)
	}
	discoveryHits, jwksHits = issuer.counts()
	if discoveryHits != 2 || jwksHits != 2 {
		t.Fatalf(
			"rotation fetches = discovery %d JWKS %d, want 2 and 2",
			discoveryHits, jwksHits)
	}
}

func TestOIDCCachedKeyRemovalTakesEffectWithinBound(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	base := time.Now()
	clockValue := base
	config := issuer.testConfig(func() time.Time { return clockValue })
	authenticator := newTestOIDCAuthenticator(t, config)
	token := issuer.signRS256(t, testKID, issuer.defaultClaims(base))

	if _, err := authenticator.Authenticate(bearerRequest(token)); err != nil {
		t.Fatalf("initial token rejected: %v", err)
	}
	issuer.mu.Lock()
	delete(issuer.keys, testKID)
	replacement, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		issuer.mu.Unlock()
		t.Fatal(err)
	}
	issuer.keys["replacement-key"] = replacement
	issuer.mu.Unlock()

	clockValue = base.Add(oidcJWKSMaxCacheAge - time.Second)
	if _, err := authenticator.Authenticate(bearerRequest(token)); err != nil {
		t.Fatalf("fresh cached key rejected before refresh bound: %v", err)
	}
	clockValue = base.Add(oidcJWKSMaxCacheAge + time.Second)
	if _, err := authenticator.Authenticate(bearerRequest(token)); err == nil {
		t.Fatal("removed signing key remained trusted past the cache bound")
	}
	discoveryHits, jwksHits := issuer.counts()
	if discoveryHits != 2 || jwksHits != 2 {
		t.Fatalf(
			"bounded refresh fetches = discovery %d JWKS %d, want 2 and 2",
			discoveryHits, jwksHits)
	}
}

func TestOIDCSharedRefreshOutlivesTriggeringRequest(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseRefresh := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseRefresh)
	issuer.mu.Lock()
	issuer.jwksStarted = started
	issuer.jwksRelease = release
	issuer.mu.Unlock()

	now := time.Now()
	authenticator := newTestOIDCAuthenticator(
		t, issuer.testConfig(fixedClock(now)))
	token := issuer.signRS256(t, testKID, issuer.defaultClaims(now))
	ctx, cancel := context.WithCancel(context.Background())
	request := bearerRequest(token).WithContext(ctx)
	firstDone := make(chan error, 1)
	go func() {
		_, err := authenticator.authenticate(request)
		firstDone <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("JWKS refresh did not start")
	}
	cancel()
	select {
	case err := <-firstDone:
		if err == nil {
			t.Fatal("canceled triggering request was accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled triggering request remained blocked on shared refresh")
	}

	secondDone := make(chan error, 1)
	go func() {
		_, err := authenticator.Authenticate(bearerRequest(token))
		secondDone <- err
	}()
	releaseRefresh()
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatalf("shared refresh was poisoned by caller cancellation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("legitimate request did not receive shared refresh result")
	}
	discoveryHits, jwksHits := issuer.counts()
	if discoveryHits != 1 || jwksHits != 1 {
		t.Fatalf(
			"shared refresh fetches = discovery %d JWKS %d, want 1 and 1",
			discoveryHits, jwksHits)
	}
}

func TestOIDCDiscoveryAndJWKSFailuresFailClosed(t *testing.T) {
	now := time.Now()
	t.Run("discovery unavailable", func(t *testing.T) {
		issuer := newFakeOIDCIssuer(t)
		issuer.mu.Lock()
		issuer.discoveryStatus = http.StatusServiceUnavailable
		issuer.mu.Unlock()
		authenticator := newTestOIDCAuthenticator(
			t, issuer.testConfig(fixedClock(now)))
		_, err := authenticator.authenticate(bearerRequest(
			issuer.signRS256(t, testKID, issuer.defaultClaims(now))))
		if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
			t.Fatalf("discovery failure = %v", err)
		}
	})
	t.Run("discovery issuer mismatch", func(t *testing.T) {
		issuer := newFakeOIDCIssuer(t)
		issuer.mu.Lock()
		issuer.discoveryIssuer = "https://issuer.invalid"
		issuer.mu.Unlock()
		authenticator := newTestOIDCAuthenticator(
			t, issuer.testConfig(fixedClock(now)))
		_, err := authenticator.authenticate(bearerRequest(
			issuer.signRS256(t, testKID, issuer.defaultClaims(now))))
		if !shoal.IsErrorCode(err, shoal.ErrorUnauthorized) {
			t.Fatalf("discovery issuer mismatch = %v", err)
		}
	})
	t.Run("JWKS unavailable", func(t *testing.T) {
		issuer := newFakeOIDCIssuer(t)
		issuer.mu.Lock()
		issuer.jwksStatus = http.StatusServiceUnavailable
		issuer.mu.Unlock()
		authenticator := newTestOIDCAuthenticator(
			t, issuer.testConfig(fixedClock(now)))
		_, err := authenticator.authenticate(bearerRequest(
			issuer.signRS256(t, testKID, issuer.defaultClaims(now))))
		if !shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
			t.Fatalf("JWKS failure = %v", err)
		}
	})
}

func TestOIDCBrowserConfigUsesDiscoveryEndpoints(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	config := issuer.testConfig(time.Now)
	config.discoveryURL = issuer.server.URL + "/metadata"
	config.browserClientID = "browser-client"
	config.browserScope = "openid profile shoal.read"
	authenticator := newTestOIDCAuthenticator(t, config)
	browser, err := authenticator.browserAuthConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if browser.ClientID != config.browserClientID ||
		browser.Scope != config.browserScope ||
		browser.AuthorizationEndpoint != issuer.server.URL+"/authorize" ||
		browser.TokenEndpoint != issuer.server.URL+"/token" {
		t.Fatalf("browser config = %+v", browser)
	}
}

func TestOIDCStaticJWKSOverrideSkipsDiscovery(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	now := time.Now()
	config := issuer.testConfig(fixedClock(now))
	config.jwksURI = issuer.server.URL + "/keys"
	authenticator := newTestOIDCAuthenticator(t, config)
	if _, err := authenticator.Authenticate(bearerRequest(
		issuer.signRS256(t, testKID, issuer.defaultClaims(now)))); err != nil {
		t.Fatalf("static JWKS token rejected: %v", err)
	}
	discoveryHits, jwksHits := issuer.counts()
	if discoveryHits != 0 || jwksHits != 1 {
		t.Fatalf(
			"static JWKS fetches = discovery %d JWKS %d, want 0 and 1",
			discoveryHits, jwksHits)
	}
}

func TestParseJWKSRejectsMalformedOrAmbiguousSets(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	n := base64.RawURLEncoding.EncodeToString(key.PublicKey.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(
		big.NewInt(int64(key.PublicKey.E)).Bytes())
	for _, testCase := range []struct {
		name string
		body string
	}{
		{"malformed JSON", `{`},
		{"no usable keys", `{"keys":[{"kty":"oct","kid":"symmetric"}]}`},
		{
			"non-signing key",
			`{"keys":[{"kty":"RSA","kid":"key","use":"enc","n":"` +
				n + `","e":"` + e + `"}]}`,
		},
		{
			"duplicate key identifier",
			`{"keys":[` +
				`{"kty":"RSA","kid":"duplicate","n":"` + n + `","e":"` + e + `"},` +
				`{"kty":"RSA","kid":"duplicate","n":"` + n + `","e":"` + e + `"}` +
				`]}`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := parseJWKS([]byte(testCase.body)); err == nil {
				t.Fatal("malformed or ambiguous JWKS was accepted")
			}
		})
	}
}

func TestOIDCAuthenticatorValidatesConfig(t *testing.T) {
	valid := oidcConfig{
		issuer:             "https://issuer.example",
		audiences:          []string{testAudience},
		authorizationClaim: "access",
		readerClaimValues:  []string{"reader"},
	}
	cases := []struct {
		name   string
		change func(*oidcConfig)
	}{
		{"missing issuer", func(config *oidcConfig) { config.issuer = "" }},
		{"insecure issuer", func(config *oidcConfig) { config.issuer = "http://issuer.example" }},
		{"wildcard issuer", func(config *oidcConfig) { config.issuer = "https://*" }},
		{"invalid DNS issuer", func(config *oidcConfig) {
			config.issuer = "https://bad_host.example"
		}},
		{"missing audience", func(config *oidcConfig) { config.audiences = nil }},
		{"missing authorization claim", func(config *oidcConfig) {
			config.authorizationClaim = ""
		}},
		{"missing authorization values", func(config *oidcConfig) {
			config.readerClaimValues = nil
		}},
		{"symmetric algorithm", func(config *oidcConfig) {
			config.allowedAlgorithms = []string{"HS256"}
		}},
		{"none algorithm", func(config *oidcConfig) {
			config.allowedAlgorithms = []string{"none"}
		}},
		{"excessive skew", func(config *oidcConfig) {
			config.clockSkew = time.Hour
		}},
		{"browser scope without client", func(config *oidcConfig) {
			config.browserScope = "openid"
		}},
		{"browser client without scope", func(config *oidcConfig) {
			config.browserClientID = "browser-client"
		}},
		{"malformed discovery URL", func(config *oidcConfig) {
			config.discoveryURL = "relative"
		}},
		{"malformed claim name", func(config *oidcConfig) {
			config.subjectClaim = "bad\nclaim"
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			config := valid
			testCase.change(&config)
			if _, err := newOIDCAuthenticator(config, time.Now); err == nil {
				t.Fatal("invalid OIDC configuration was accepted")
			}
		})
	}
}

func TestOIDCBoundaryCollapsesErrorsAndNeverLeaksToken(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	now := time.Now()
	authenticator := newTestOIDCAuthenticator(
		t, issuer.testConfig(fixedClock(now)))
	expired := issuer.defaultClaims(now)
	expired["exp"] = now.Add(-2 * time.Hour).Unix()
	unmapped := issuer.defaultClaims(now)
	unmapped["access"] = "unknown"
	tokens := []string{
		"secret-garbage-token",
		issuer.signRS256(t, testKID, expired),
		issuer.signRS256(t, testKID, unmapped),
		issuer.signRS256(t, "unknown-kid", issuer.defaultClaims(now)),
	}
	for _, token := range tokens {
		_, err := authenticator.Authenticate(bearerRequest(token))
		if err == nil {
			t.Fatal("invalid token was accepted")
		}
		if err.Error() != oidcDenied().Error() {
			t.Fatalf("boundary error = %q", err)
		}
		if strings.Contains(err.Error(), token) {
			t.Fatal("boundary error disclosed raw token")
		}
	}
}

func TestOIDCRejectsMissingOrMalformedBearer(t *testing.T) {
	issuer := newFakeOIDCIssuer(t)
	authenticator := newTestOIDCAuthenticator(
		t, issuer.testConfig(fixedClock(time.Now())))
	for _, header := range []string{"", "opaque-token", "Basic abc", "Bearer "} {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/meta", nil)
		if header != "" {
			request.Header.Set("Authorization", header)
		}
		if _, err := authenticator.Authenticate(request); err == nil {
			t.Fatalf("header %q was accepted", header)
		}
	}
}

func TestSelectAuthenticatorOIDCAllowsNonLoopbackAndRejectsConflict(t *testing.T) {
	config := oidcConfig{
		issuer:             "https://issuer.example",
		audiences:          []string{testAudience},
		authorizationClaim: "access",
		readerClaimValues:  []string{"reader"},
	}
	for _, address := range []string{"0.0.0.0:8080", "10.0.0.5:8080", "[::]:8080"} {
		authenticator, err := selectAuthenticator(false, "", config, address, time.Now)
		if err != nil {
			t.Fatalf("OIDC authenticator refused on %s: %v", address, err)
		}
		if _, ok := authenticator.(*oidcAuthenticator); !ok {
			t.Fatalf("authenticator = %T", authenticator)
		}
	}
	if authenticator, err := selectAuthenticator(
		true, "", config, "127.0.0.1:8080", time.Now,
	); err == nil || authenticator != nil {
		t.Fatal("-dev-auth and OIDC were not rejected as mutually exclusive")
	}
}

func TestOIDCAuthenticatorNeverGetsDevelopmentBackfill(t *testing.T) {
	config := oidcConfig{
		issuer:             "https://issuer.example",
		audiences:          []string{testAudience},
		authorizationClaim: "access",
		readerClaimValues:  []string{"reader"},
	}
	authenticator, err := selectAuthenticator(
		false, "", config, "0.0.0.0:8080", time.Now)
	if err != nil {
		t.Fatal(err)
	}
	backfill := newDevelopmentBackfill(
		authenticator, "127.0.0.1:8080", auth.NewAuthority().Binder())
	if backfill != nil {
		t.Fatal("the OIDC authenticator was granted a development backfill")
	}
}

func TestOIDCRequestFlowsThroughBinderResolverAndUnmappedStopsAtGate(
	t *testing.T,
) {
	issuer := newFakeOIDCIssuer(t)
	now := time.Now()
	authenticator := newTestOIDCAuthenticator(
		t, issuer.testConfig(fixedClock(now)))

	authority := auth.NewAuthority()
	root := t.TempDir()
	opened, err := openService(context.Background(), serviceConfig{
		backend:   "embedded",
		data:      filepath.Join(root, "corpus"),
		policyDir: filepath.Join(root, "policy"),
		resolver:  authority.Resolver(),
		clock:     fixedClock(now),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer opened.close()
	handler, err := webapi.NewAuthenticatedHandler(
		opened.service, authenticator, authority.Binder(), "workspace.test")
	if err != nil {
		t.Fatal(err)
	}
	token := issuer.signRS256(t, testKID, issuer.defaultClaims(now))
	request := httptest.NewRequest(
		http.MethodPost,
		"http://workspace.test/api/v1/documents",
		strings.NewReader(`{"page":{"limit":10}}`),
	)
	request.Host = "workspace.test"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("verified OIDC request status = %d body = %s",
			response.Code, response.Body.String())
	}

	counter := &countingService{}
	gated, err := webapi.NewAuthenticatedHandler(
		counter, authenticator, authority.Binder(), "workspace.test")
	if err != nil {
		t.Fatal(err)
	}
	unmapped := issuer.defaultClaims(now)
	unmapped["access"] = "unknown"
	request = httptest.NewRequest(
		http.MethodPost,
		"http://workspace.test/api/v1/documents",
		strings.NewReader(`{"page":{"limit":10}}`),
	)
	request.Host = "workspace.test"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+
		issuer.signRS256(t, testKID, unmapped))
	response = httptest.NewRecorder()
	gated.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unmapped token status = %d", response.Code)
	}
	if counter.calls != 0 {
		t.Fatalf("unmapped token reached %d service operations", counter.calls)
	}

	request = httptest.NewRequest(
		http.MethodPost,
		"http://workspace.test/api/v1/documents",
		strings.NewReader(`{"page":{"limit":10}}`),
	)
	request.Host = "workspace.test"
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer malformed-token")
	response = httptest.NewRecorder()
	gated.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("malformed token status = %d", response.Code)
	}
	if counter.calls != 0 {
		t.Fatalf("malformed token reached %d service operations", counter.calls)
	}
}

type countingService struct {
	calls int
}

func (s *countingService) Documents(
	context.Context,
	webapi.DocumentsRequest,
) (webapi.DocumentsResponse, error) {
	s.calls++
	return webapi.DocumentsResponse{}, nil
}

func (s *countingService) Document(
	context.Context,
	webapi.DocumentRequest,
) (webapi.DocumentResponse, error) {
	s.calls++
	return webapi.DocumentResponse{}, nil
}

func (s *countingService) Retrieve(
	context.Context,
	webapi.RetrievalRequest,
) (webapi.RetrievalResponse, error) {
	s.calls++
	return webapi.RetrievalResponse{}, nil
}

func (s *countingService) Neighborhood(
	context.Context,
	webapi.NeighborhoodRequest,
) (webapi.NeighborhoodResponse, error) {
	s.calls++
	return webapi.NeighborhoodResponse{}, nil
}

func (s *countingService) Path(
	context.Context,
	webapi.PathRequest,
) (webapi.PathResponse, error) {
	s.calls++
	return webapi.PathResponse{}, nil
}

func operationsContain(operations []auth.Operation, target auth.Operation) bool {
	for _, operation := range operations {
		if operation == target {
			return true
		}
	}
	return false
}

func grantsWorkspaceSource(decision auth.Decision) bool {
	for _, source := range decision.PermittedSourceIDs() {
		if bytes.Equal(source, workspaceSourceID) {
			return true
		}
	}
	return false
}

var _ webapi.Authenticator = (*oidcAuthenticator)(nil)
var _ webapi.Service = (*countingService)(nil)
