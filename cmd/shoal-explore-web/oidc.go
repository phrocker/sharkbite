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
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The OIDC authenticator validates standards-based bearer tokens and mints one
// trusted decision per request. A validated token proves identity only;
// authority is derived from an explicit, configurable, fail-closed claim
// mapping.
const (
	oidcActor          shoal.ID = "shoal-explore-web-oidc"
	oidcAuditPurpose            = "oidc-authenticated workspace access"
	oidcIdentityPrefix          = "oidc:"
	legacyEntraActor   shoal.ID = "shoal-explore-web-entra"
	legacyEntraPurpose          = "entra-authenticated workspace access"
	legacyEntraPrefix           = "entra:"

	// defaultOIDCClockSkew tolerates small clock differences between this
	// instance and the issuer without widening the validity window materially.
	defaultOIDCClockSkew = 60 * time.Second
	// maxOIDCClockSkew bounds the configurable skew so a misconfiguration can
	// never turn expiry validation into a rubber stamp.
	maxOIDCClockSkew = 5 * time.Minute

	// oidcJWKSMinRefreshInterval is the shortest interval between JWKS
	// fetches. A cache miss for an unknown key identifier triggers at most one
	// fetch per interval, so a remote attacker presenting tokens with random
	// key identifiers cannot induce an unbounded fetch storm. Legitimate key
	// rotation is still picked up within this interval.
	oidcJWKSMinRefreshInterval = 5 * time.Minute
	// oidcJWKSMaxCacheAge bounds how long a cached signing key remains trusted.
	// Once the set is this old, even a cache hit must refresh successfully
	// before the key can be used, so removing a compromised key at the issuer
	// takes effect within a bounded interval.
	oidcJWKSMaxCacheAge = 15 * time.Minute
	// oidcHTTPTimeout bounds every discovery and JWKS fetch.
	oidcHTTPTimeout = 10 * time.Second
	// oidcMetadataMaxBytes bounds a JWKS or discovery response so a hostile or
	// broken endpoint cannot exhaust memory.
	oidcMetadataMaxBytes = 1 << 20
)

// supportedOIDCAlgorithms is the set of asymmetric signing algorithms the
// authenticator will ever accept. It deliberately excludes "none" and every
// symmetric (HMAC) algorithm: accepting an HMAC algorithm would let an attacker
// present a token signed with the issuer's public key as if it were the shared
// secret (an algorithm-confusion attack), and "none" would accept unsigned
// tokens outright.
var supportedOIDCAlgorithms = map[string]struct{}{
	"RS256": {}, "RS384": {}, "RS512": {},
	"PS256": {}, "PS384": {}, "PS512": {},
	"ES256": {}, "ES384": {}, "ES512": {},
}

// Package-level sentinel errors are returned by the internal validation path so
// a test can assert the *specific* reason a token was rejected, not merely that
// some error occurred. They are stable pointer identities; errors.Is matches
// them by identity because shoal.Error defines no Is method. The public
// Authenticate method never returns these directly: it collapses every failure
// to a generic denial (see oidcDenied) so no reason and no token-derived
// detail crosses the trust boundary. These carry only fixed strings and never
// embed the raw token.
var (
	// errMissingBearer is returned when no usable bearer credential is present.
	errMissingBearer = shoal.NewError(
		shoal.ErrorUnauthorized, "authorization bearer token is required")
	// errMissingKeyID is returned when the token header carries no key id.
	errMissingKeyID = shoal.NewError(
		shoal.ErrorUnauthorized, "token is missing a key identifier")
	// errUnexpectedSigningMethod is returned by the keyfunc's signing-method
	// switch default arm: the resolved method is not one of the accepted
	// asymmetric families. This is one of the two independent algorithm guards
	// (the other is the parser's WithValidMethods allowlist).
	errUnexpectedSigningMethod = shoal.NewError(
		shoal.ErrorUnauthorized, "unexpected token signing method")
	// errKeyMethodMismatch is returned when the resolved key type does not
	// match the token's signing-method family (e.g. an RSA method resolving a
	// non-RSA key).
	errKeyMethodMismatch = shoal.NewError(
		shoal.ErrorUnauthorized, "signing key does not match the signing method")
	// errKeyAlgorithmMismatch is returned when a JWK's optional alg constraint
	// does not match the token header algorithm.
	errKeyAlgorithmMismatch = shoal.NewError(
		shoal.ErrorUnauthorized, "signing key does not allow the token algorithm")
	// errNoMatchingKey is returned when no signing key matches the token's key
	// identifier, including when the fetch-storm guard refuses a fresh fetch.
	errNoMatchingKey = shoal.NewError(
		shoal.ErrorUnauthorized, "no signing key matches the token key identifier")
	// errMissingSubject is returned when a verified token carries no usable
	// subject or object identifier.
	errMissingSubject = shoal.NewError(
		shoal.ErrorUnauthorized, "token has no usable subject")
	// errMissingExpiry is returned when a verified token carries no expiry.
	errMissingExpiry = shoal.NewError(
		shoal.ErrorUnauthorized, "token has no expiry")
	// errMalformedClaim is returned when a configured claim is present in a
	// shape that cannot be mapped safely to a decision.
	errMalformedClaim = shoal.NewError(
		shoal.ErrorUnauthorized, "token claim has an invalid shape")
	// errMissingMappedClaim is returned when a configured required identity or
	// authorization claim is absent.
	errMissingMappedClaim = shoal.NewError(
		shoal.ErrorUnauthorized, "token is missing a required mapped claim")
	// errUnmappedAuthorization is returned when the authorization claim is
	// valid but none of its values is explicitly mapped to workspace authority.
	errUnmappedAuthorization = shoal.NewError(
		shoal.ErrorUnauthorized, "token authorization claim is not mapped")
)

// oidcReaderOperations is the authority granted to a mapped reader. It permits
// discovery and retrieval but not ingestion.
var oidcReaderOperations = []auth.Operation{
	auth.OperationList,
	auth.OperationRead,
	auth.OperationConnect,
	auth.OperationNeighborhood,
	auth.OperationRetrieve,
	auth.OperationWorkspaceSettingsRead,
	auth.OperationAgentResolve,
	auth.OperationTeamOverviewRead,
}

// oidcContributorOperations additionally permits ingestion. It matches the
// ceiling the workspace UI needs.
var oidcContributorOperations = []auth.Operation{
	auth.OperationIngest,
	auth.OperationGraphMaterialize,
	auth.OperationList,
	auth.OperationRead,
	auth.OperationConnect,
	auth.OperationNeighborhood,
	auth.OperationRetrieve,
	auth.OperationWorkspaceSettingsRead,
	auth.OperationWorkspaceSettingsWrite,
	auth.OperationAgentResolve,
	auth.OperationTeamOverviewRead,
}

// oidcFleetOperations is the explicit control-plane ceiling for operators who
// opt in via -oidc-fleet-values.
var oidcFleetOperations = []auth.Operation{
	auth.OperationAgentRegister,
	auth.OperationAgentHeartbeat,
	auth.OperationAgentRevoke,
	auth.OperationAgentResolve,
	auth.OperationDelegate,
	auth.OperationDispatch,
	auth.OperationInvoke,
	// OperationExecute is deliberately NOT here, and that is the whole point
	// rather than an omission.
	//
	// This list is not a ceiling something else narrows: authority() grants it
	// verbatim to any token whose claim matches -oidc-fleet-values. Every fleet
	// principal is also minted with the same workspaceAuthorizationDomain,
	// workspaceSourceID and workspaceGrantPolicyID, and authorizeResource
	// compares only those three — AuthorizeObject validates ObjectID and never
	// consults it. So the only thing separating two fleet principals' queued
	// work is the principal check that execute skips.
	//
	// Adding execute here would therefore have granted every existing
	// fleet-mapped token, on upgrade, the ability to pull another principal's
	// records, claim them, and complete them with a fabricated outcome. That is
	// the exact property the operation exists to prevent, so it stays
	// unreachable through this mapping until it has one of its own.
	//
	// Execute has a mapping of its own since #391: the executor mint
	// (oidc_executor.go), on an audience and possibly an issuer of its own,
	// which binds each credential to one executor reference under
	// ServiceRoleActionExecution. oidc_execute_grant_test.go asserts that it
	// is the only source of execute in this command.
	//
	// OperationActionApprove is absent for a related reason (#451). This list
	// also grants dispatch and invoke, and the approval service refuses any
	// approver holding either on the scope, so approve here would be dead
	// weight; on the reader or contributor list it would mint an approver of
	// every mapped token. Approval has a role mapping of its own — the
	// operator file behind -oidc-approver-mapping-file, on an audience of
	// its own (oidc_approver.go) — and oidc_approve_grant_test.go asserts
	// that oidcApproverOperations is the only list that grants it.
	auth.OperationSubscriptionCreate,
	auth.OperationSubscriptionDelete,
	auth.OperationSubscriptionDeliver,
	auth.OperationEventPublish,
}

// oidcConfig is the operator-supplied, provider-neutral configuration. A
// client secret is never accepted: this validates bearer tokens and optionally
// publishes public-client browser login metadata; it never issues credentials.
type oidcConfig struct {
	// issuer is the exact token issuer and discovery issuer.
	issuer string
	// discoveryURL overrides the standard metadata location derived from issuer.
	discoveryURL string
	// audiences are accepted token audiences. At least one is required.
	audiences []string
	// jwksURI, when set, overrides OIDC discovery and is fetched directly.
	jwksURI string
	// allowedAlgorithms restricts accepted signing algorithms. Empty means
	// RS256 only.
	allowedAlgorithms []string
	// clockSkew is the tolerated clock difference for expiry/not-before. Zero
	// means defaultOIDCClockSkew.
	clockSkew time.Duration
	// Claim names are exact, top-level JWT claim keys. subjectClaim defaults to
	// the standard "sub" claim. authorizationClaim and at least one mapped
	// value are required.
	subjectClaim         string
	subjectFallbackClaim string
	actorClaim           string
	clientIDClaim        string
	delegationClaim      string
	authorizationClaim   string
	readerClaimValues    []string
	contributorValues    []string
	fleetValues          []string
	// Browser login is optional. When browserClientID is set, browserScope is
	// required and endpoints are either supplied explicitly or read from OIDC
	// discovery metadata.
	browserClientID       string
	browserScope          string
	authorizationEndpoint string
	tokenEndpoint         string
	// Compatibility fields preserve the former Entra configuration and
	// decision contract while routing it through this OIDC implementation.
	identityPrefix             string
	defaultActor               shoal.ID
	auditPurpose               string
	allowUnmappedAuthorization bool
	trimAuthorizationValues    bool
	trimIdentityValues         bool
	authorizationArrayOnly     bool
	legacyTenantID             string
	legacyAuthority            string
	// approverMappingFile names the operator approver mapping (#451). Empty
	// means no approvers: no token mints OperationActionApprove.
	approverMappingFile string
	// identityClaim is -oidc-identity-claim (#526): a JSON array of path
	// segments naming a stable identity claim. Empty means identities are
	// derived from the subject claim, as before.
	identityClaim string
	// labelGrantsFile names the operator label grant file (#570). Empty
	// means no labels: no token holds a label policy, so labelled content is
	// visible to nobody.
	labelGrantsFile string
	// labelGrantSources is the set of source IDs a label grant may name. Nil
	// means this command's one configured source, workspaceSourceID; only a
	// test sets it, to name a second source the server does not ingest into.
	labelGrantSources [][]byte
	// executorMappingFile names the operator executor mapping (#391).
	// Empty means no executors: no token mints OperationExecute.
	executorMappingFile string

	// httpClient and clock are injected by tests; production leaves them nil.
	httpClient *http.Client
	clock      func() time.Time
}

// configured reports whether any OIDC option was supplied. Any single option
// signals intent to run the real authenticator, so a partial configuration is
// reported as an error rather than silently ignored.
func (c oidcConfig) configured() bool {
	return c.issuer != "" || c.discoveryURL != "" || len(c.audiences) > 0 ||
		c.jwksURI != "" || len(c.allowedAlgorithms) > 0 ||
		c.clockSkew != 0 || c.subjectClaim != "" || c.actorClaim != "" ||
		c.subjectFallbackClaim != "" ||
		c.clientIDClaim != "" || c.delegationClaim != "" ||
		c.authorizationClaim != "" || len(c.readerClaimValues) > 0 ||
		len(c.contributorValues) > 0 || len(c.fleetValues) > 0 ||
		c.browserClientID != "" ||
		c.browserScope != "" || c.authorizationEndpoint != "" ||
		c.tokenEndpoint != "" || c.approverMappingFile != "" ||
		c.identityClaim != "" || c.labelGrantsFile != "" ||
		c.executorMappingFile != ""
}

// oidcAuthenticator validates bearer tokens against the issuer's JWKS
// and mints a conservatively-scoped decision per request.
type oidcAuthenticator struct {
	parser                     *jwt.Parser
	keys                       *jwksCache
	expectedIssuer             string
	subjectClaim               string
	subjectFallbackClaim       string
	actorClaim                 string
	clientIDClaim              string
	delegationClaim            string
	authorizationClaim         string
	readerClaimValues          map[string]struct{}
	contributorValues          map[string]struct{}
	fleetValues                map[string]struct{}
	authenticationLeeway       time.Duration
	identityPrefix             string
	defaultActor               shoal.ID
	auditPurpose               string
	allowUnmappedAuthorization bool
	trimAuthorizationValues    bool
	trimIdentityValues         bool
	authorizationArrayOnly     bool
	legacyTenantID             string
	legacyAuthority            string
	browserClientID            string
	browserScope               string
	authorizationEndpoint      string
	tokenEndpoint              string
	// approver is the operator approver mapping, or nil. workspaceAudiences
	// is the set the approver audience must be disjoint from, and that
	// decides which branch a token is minted on.
	approver           *approverMapping
	workspaceAudiences map[string]struct{}
	// identityClaim is the stable identity claim path (#526), or nil. When
	// set, every principal on both branches is oidcid:<iss>#<tag>#<value>, derived
	// by stableIdentity and by nothing else.
	identityClaim []string
	// flatIdentityValues is set when identityPrefix is the sub-derived
	// namespace oidc:<iss># (#546). Every value minted under it must then
	// be free of '#', so that no such identity can be read as one in a
	// non-default subject claim's namespace, oidc:<iss>#<claim tag>#.
	flatIdentityValues bool
	// hashRefusals logs the claims whose values were refused for '#'.
	hashRefusals *hashRefusalLog
	// labelGrants is the operator label grant file (#570), or nil. Only the
	// workspace branch reads it, and only for a token a role mapping has
	// already granted.
	labelGrants *labelGrants
	// executor is the executor branch (#391), or nil. It has its own
	// issuer, parser and key cache, and is the only source of
	// OperationExecute in this command.
	executor *oidcExecutorBranch
}

func newOIDCAuthenticator(
	config oidcConfig,
	clock func() time.Time,
) (*oidcAuthenticator, error) {
	if config.clock != nil {
		clock = config.clock
	}
	if clock == nil {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "clock is required")
	}
	issuer := strings.TrimSpace(config.issuer)
	if issuer == "" {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"-oidc-issuer is required for the OIDC authenticator")
	}
	if err := validateOIDCEndpoint("issuer", issuer, config.httpClient != nil); err != nil {
		return nil, err
	}
	audiences, err := normalizeRequiredValues("OIDC audience", config.audiences)
	if err != nil {
		return nil, err
	}

	algorithms, err := normalizeOIDCAlgorithms(config.allowedAlgorithms)
	if err != nil {
		return nil, err
	}
	skew := config.clockSkew
	if skew == 0 {
		skew = defaultOIDCClockSkew
	}
	if skew < 0 || skew > maxOIDCClockSkew {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			fmt.Sprintf(
				"-oidc-clock-skew must be between 0 and %s", maxOIDCClockSkew))
	}

	subjectClaim, err := normalizeClaimName(
		"-oidc-subject-claim", config.subjectClaim, "sub", true)
	if err != nil {
		return nil, err
	}
	subjectFallbackClaim, err := normalizeClaimName(
		"legacy subject fallback claim", config.subjectFallbackClaim, "", false)
	if err != nil {
		return nil, err
	}
	actorClaim, err := normalizeClaimName(
		"-oidc-actor-claim", config.actorClaim, "", false)
	if err != nil {
		return nil, err
	}
	clientIDClaim, err := normalizeClaimName(
		"-oidc-client-id-claim", config.clientIDClaim, "", false)
	if err != nil {
		return nil, err
	}
	delegationClaim, err := normalizeClaimName(
		"-oidc-delegation-claim", config.delegationClaim, "", false)
	if err != nil {
		return nil, err
	}
	authorizationClaim, err := normalizeClaimName(
		"-oidc-authorization-claim", config.authorizationClaim, "", true)
	if err != nil {
		return nil, err
	}
	readerClaimValues := normalizeValueSet(config.readerClaimValues)
	contributorValues := normalizeValueSet(config.contributorValues)
	fleetValues := normalizeValueSet(config.fleetValues)
	if len(readerClaimValues) == 0 && len(contributorValues) == 0 &&
		len(fleetValues) == 0 &&
		!config.allowUnmappedAuthorization {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"at least one -oidc-reader-values, "+
				"-oidc-contributor-values, or -oidc-fleet-values mapping is required")
	}

	identityClaim, err := parseIdentityClaimFlag(config.identityClaim)
	if err != nil {
		return nil, err
	}
	if identityClaim != nil {
		if err := refuseIdentityClaimCombination(config, issuer); err != nil {
			return nil, err
		}
	}

	var approver *approverMapping
	if path := strings.TrimSpace(config.approverMappingFile); path != "" {
		approver, err = loadApproverMapping(path, issuer, audiences, identityClaim)
		if err != nil {
			return nil, err
		}
		// The approval service separates approver from requester by
		// identity. An approver is always oidc:<iss>#<sub>, so the workspace
		// principals it is compared against must be named the same way: under
		// a legacy identity mode (entra:<oid>, or another subject claim) the
		// same human would carry two unrelated identities, and could approve
		// their own request.
		if subjectClaim != "sub" || subjectFallbackClaim != "" ||
			config.trimIdentityValues ||
			(config.identityPrefix != "" &&
				config.identityPrefix != oidcIdentityPrefix+issuer+"#") {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"-oidc-approver-mapping-file requires the default OIDC "+
					"identity (subject claim sub, identities oidc:<iss>#<sub>); "+
					"it cannot be combined with the legacy Entra identity mode")
		}
	}
	identityPrefix, flat := subjectIdentityPrefix(
		config.identityPrefix, issuer, subjectClaim)
	var grants *labelGrants
	if path := strings.TrimSpace(config.labelGrantsFile); path != "" {
		sources := config.labelGrantSources
		if sources == nil {
			sources = [][]byte{workspaceSourceID}
		}
		grants, err = loadLabelGrants(path, issuer, sources)
		if err != nil {
			return nil, err
		}
	}
	workspaceAudiences := make(map[string]struct{}, len(audiences))
	for _, audience := range audiences {
		workspaceAudiences[audience] = struct{}{}
	}
	parserAudiences := append([]string(nil), audiences...)
	if approver != nil {
		parserAudiences = append(parserAudiences, approver.audience)
	}

	httpClient := config.httpClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: oidcHTTPTimeout}
	}
	allowLoopbackHTTP := config.httpClient != nil
	discoveryURL := strings.TrimSpace(config.discoveryURL)
	if discoveryURL == "" {
		discoveryURL = strings.TrimRight(issuer, "/") +
			"/.well-known/openid-configuration"
	}
	if err := validateOIDCEndpoint(
		"discovery URL", discoveryURL, allowLoopbackHTTP); err != nil {
		return nil, err
	}
	staticJWKSURI := strings.TrimSpace(config.jwksURI)
	if staticJWKSURI != "" {
		if err := validateOIDCEndpoint(
			"JWKS URI", staticJWKSURI, allowLoopbackHTTP); err != nil {
			return nil, err
		}
	}
	if err := validateBrowserOIDCConfig(config, allowLoopbackHTTP); err != nil {
		return nil, err
	}

	algorithmNames := make([]string, 0, len(algorithms))
	for name := range algorithms {
		algorithmNames = append(algorithmNames, name)
	}

	var executor *oidcExecutorBranch
	if path := strings.TrimSpace(config.executorMappingFile); path != "" {
		// Disjoint from every audience the human issuer's tokens are
		// minted on, so no token is addressed to both branches.
		reserved := append([]string(nil), audiences...)
		if approver != nil {
			reserved = append(reserved, approver.audience)
		}
		mapping, err := loadExecutorMapping(path, reserved, allowLoopbackHTTP)
		if err != nil {
			return nil, err
		}
		executor = newOIDCExecutorBranch(
			mapping, algorithmNames, skew, clock, httpClient, allowLoopbackHTTP)
	}

	parser := jwt.NewParser(
		jwt.WithValidMethods(algorithmNames),
		jwt.WithIssuer(issuer),
		jwt.WithAudience(parserAudiences...),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(skew),
		jwt.WithTimeFunc(clock),
	)

	metadata := &oidcMetadataCache{
		issuer:       issuer,
		discoveryURL: discoveryURL,
		httpClient:   httpClient,
		allowHTTP:    allowLoopbackHTTP,
	}
	return &oidcAuthenticator{
		parser: parser,
		keys: &jwksCache{
			metadata:           metadata,
			staticJWKSURI:      staticJWKSURI,
			httpClient:         httpClient,
			clock:              clock,
			minRefreshInterval: oidcJWKSMinRefreshInterval,
			maxCacheAge:        oidcJWKSMaxCacheAge,
			allowHTTP:          allowLoopbackHTTP,
		},
		expectedIssuer:             issuer,
		subjectClaim:               subjectClaim,
		subjectFallbackClaim:       subjectFallbackClaim,
		actorClaim:                 actorClaim,
		clientIDClaim:              clientIDClaim,
		delegationClaim:            delegationClaim,
		authorizationClaim:         authorizationClaim,
		readerClaimValues:          readerClaimValues,
		contributorValues:          contributorValues,
		fleetValues:                fleetValues,
		authenticationLeeway:       skew,
		identityPrefix:             identityPrefix,
		flatIdentityValues:         flat,
		hashRefusals:               newHashRefusalLog(nil, clock),
		defaultActor:               firstNonZeroID(config.defaultActor, oidcActor),
		auditPurpose:               firstNonEmpty(config.auditPurpose, oidcAuditPurpose),
		allowUnmappedAuthorization: config.allowUnmappedAuthorization,
		trimAuthorizationValues:    config.trimAuthorizationValues,
		trimIdentityValues:         config.trimIdentityValues,
		authorizationArrayOnly:     config.authorizationArrayOnly,
		legacyTenantID:             strings.TrimSpace(config.legacyTenantID),
		legacyAuthority:            strings.TrimSpace(config.legacyAuthority),
		browserClientID:            strings.TrimSpace(config.browserClientID),
		browserScope:               strings.TrimSpace(config.browserScope),
		authorizationEndpoint:      strings.TrimSpace(config.authorizationEndpoint),
		tokenEndpoint:              strings.TrimSpace(config.tokenEndpoint),
		approver:                   approver,
		workspaceAudiences:         workspaceAudiences,
		identityClaim:              identityClaim,
		labelGrants:                grants,
		executor:                   executor,
	}, nil
}

// normalizeOIDCAlgorithms validates and de-duplicates the configured
// algorithm allowlist, rejecting any symmetric algorithm and "none".
func normalizeOIDCAlgorithms(configured []string) (map[string]struct{}, error) {
	algorithms := make(map[string]struct{})
	for _, raw := range configured {
		name := strings.ToUpper(strings.TrimSpace(raw))
		if name == "" {
			continue
		}
		if _, ok := supportedOIDCAlgorithms[name]; !ok {
			return nil, shoal.NewError(
				shoal.ErrorInvalidArgument,
				fmt.Sprintf(
					"signing algorithm %q is not an accepted asymmetric "+
						"algorithm; only RS/PS/ES family algorithms are allowed "+
						"(never HS* or none)", name))
		}
		algorithms[name] = struct{}{}
	}
	if len(algorithms) == 0 {
		algorithms["RS256"] = struct{}{}
	}
	return algorithms, nil
}

func normalizeRequiredValues(name string, configured []string) ([]string, error) {
	values := normalizeValueSet(configured)
	if len(values) == 0 {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, strings.ToLower(name)+" is required")
	}
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	return result, nil
}

func normalizeValueSet(configured []string) map[string]struct{} {
	values := make(map[string]struct{})
	for _, raw := range configured {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		values[value] = struct{}{}
	}
	return values
}

func normalizeClaimName(
	flagName string,
	configured string,
	defaultName string,
	required bool,
) (string, error) {
	name := strings.TrimSpace(configured)
	if name == "" {
		name = defaultName
	}
	if required && name == "" {
		return "", shoal.NewError(
			shoal.ErrorInvalidArgument, flagName+" is required")
	}
	if len(name) > 256 || strings.ContainsAny(name, "\x00\r\n") {
		return "", shoal.NewError(
			shoal.ErrorInvalidArgument, flagName+" is invalid")
	}
	return name, nil
}

func validateBrowserOIDCConfig(config oidcConfig, allowLoopbackHTTP bool) error {
	clientID := strings.TrimSpace(config.browserClientID)
	scope := strings.TrimSpace(config.browserScope)
	authorizationEndpoint := strings.TrimSpace(config.authorizationEndpoint)
	tokenEndpoint := strings.TrimSpace(config.tokenEndpoint)
	if clientID == "" && scope == "" &&
		authorizationEndpoint == "" && tokenEndpoint == "" {
		return nil
	}
	if clientID == "" {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"-oidc-browser-client-id is required when browser login is configured")
	}
	if scope == "" {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"-oidc-browser-scope is required when browser login is configured")
	}
	if authorizationEndpoint != "" {
		if err := validateOIDCEndpoint(
			"authorization endpoint", authorizationEndpoint, allowLoopbackHTTP); err != nil {
			return err
		}
	}
	if tokenEndpoint != "" {
		if err := validateOIDCEndpoint(
			"token endpoint", tokenEndpoint, allowLoopbackHTTP); err != nil {
			return err
		}
	}
	return nil
}

func validateOIDCEndpoint(name, raw string, allowLoopbackHTTP bool) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil ||
		parsed.Fragment != "" {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, name+" must be an absolute URL")
	}
	if name == "issuer" && parsed.RawQuery != "" {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "issuer must not contain a query")
	}
	// Not even an empty fragment: url.Parse reports "https://x/#" with an
	// empty Fragment, and the issuer is embedded in every identity as
	// oidc:<iss>#<value>, where '#' is the separator. An issuer containing
	// one would let identities of two issuers, or an issuer and a value,
	// run into each other.
	if name == "issuer" && strings.Contains(raw, "#") {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "issuer must not contain '#'")
	}
	if !validOIDCEndpointHost(parsed) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, name+" must use a valid IP or DNS host")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	host := parsed.Hostname()
	if allowLoopbackHTTP && parsed.Scheme == "http" {
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
		if strings.EqualFold(host, "localhost") {
			return nil
		}
	}
	return shoal.NewError(
		shoal.ErrorInvalidArgument, name+" must use https")
}

func validOIDCEndpointHost(parsed *url.URL) bool {
	if parsed == nil || strings.ContainsAny(parsed.Host, "*'\"; \t\r\n\\") {
		return false
	}
	host := parsed.Hostname()
	if host == "" {
		return false
	}
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return false
		}
	}
	if net.ParseIP(host) != nil {
		return true
	}
	host = strings.TrimSuffix(host, ".")
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 ||
			label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, character := range label {
			if (character < 'a' || character > 'z') &&
				(character < 'A' || character > 'Z') &&
				(character < '0' || character > '9') &&
				character != '-' {
				return false
			}
		}
	}
	return true
}

func (a *oidcAuthenticator) browserAuthConfig(
	ctx context.Context,
) (*webapi.BrowserAuthConfig, error) {
	if a.browserClientID == "" {
		return nil, nil
	}
	authorizationEndpoint := a.authorizationEndpoint
	tokenEndpoint := a.tokenEndpoint
	if authorizationEndpoint == "" || tokenEndpoint == "" {
		metadata, err := a.keys.metadata.get(ctx, false)
		if err != nil {
			return nil, err
		}
		if authorizationEndpoint == "" {
			authorizationEndpoint = metadata.AuthorizationEndpoint
		}
		if tokenEndpoint == "" {
			tokenEndpoint = metadata.TokenEndpoint
		}
	}
	if authorizationEndpoint == "" || tokenEndpoint == "" {
		return nil, shoal.NewError(
			shoal.ErrorUnavailable,
			"OIDC discovery document has no browser authorization endpoints")
	}
	if err := validateOIDCEndpoint(
		"authorization endpoint",
		authorizationEndpoint,
		a.keys.allowHTTP,
	); err != nil {
		return nil, err
	}
	if err := validateOIDCEndpoint(
		"token endpoint",
		tokenEndpoint,
		a.keys.allowHTTP,
	); err != nil {
		return nil, err
	}
	return &webapi.BrowserAuthConfig{
		TenantID:              a.legacyTenantID,
		ClientID:              a.browserClientID,
		Scope:                 a.browserScope,
		Authority:             a.legacyAuthority,
		AuthorizationEndpoint: authorizationEndpoint,
		TokenEndpoint:         tokenEndpoint,
	}, nil
}

// Authenticate validates the request's bearer token and mints one decision. It
// is the trust boundary: it collapses every validation failure to a single
// generic denial so no rejection reason and no token-derived detail (least of
// all the raw token) can cross into an error, log line, or HTTP response. The
// specific, token-free reason is available to in-package tests via authenticate.
func (a *oidcAuthenticator) Authenticate(
	request *http.Request,
) (auth.Decision, error) {
	if request == nil {
		return auth.Decision{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "request is required")
	}
	decision, err := a.authenticate(request)
	if err != nil {
		return auth.Decision{}, oidcDenied()
	}
	return decision, nil
}

// authenticate performs the real validation and returns the specific,
// token-free reason on failure. It is unexported and used by Authenticate and
// by in-package tests that assert *which* guard rejected a token. None of the
// errors it returns embed the raw token.
func (a *oidcAuthenticator) authenticate(
	request *http.Request,
) (auth.Decision, error) {
	if request == nil {
		return auth.Decision{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "request is required")
	}
	raw, err := bearerToken(request)
	if err != nil {
		return auth.Decision{}, err
	}
	// A token addressed to the executor audience is validated by the
	// executor branch alone: its issuer, its keys, its audience. Nothing
	// here trusts the unverified audience beyond choosing which verifier
	// runs, and each verifier accepts only its own issuer's signatures.
	if a.executor != nil && a.executor.addressed(raw) {
		return a.authenticateExecutor(request, raw)
	}
	ctx := request.Context()
	claims := jwt.MapClaims{}
	keyFunc := a.keyFuncForContext(ctx)
	if _, err := a.parser.ParseWithClaims(raw, claims, keyFunc); err != nil {
		return auth.Decision{}, err
	}
	// The executor audience is unreachable here through the routing above,
	// and is refused anyway; an executor's credential sent to a human
	// audience (by its service assertion, or its mapped subject of the
	// executor issuer) is refused on both human branches.
	if a.executor != nil {
		if err := a.executor.refuseOnHumanBranch(claims); err != nil {
			return auth.Decision{}, err
		}
	}
	correlationID, err := correlationIDFor(request, "oidc-correlation-")
	if err != nil {
		return auth.Decision{}, err
	}
	return a.mint(ctx, claims, correlationID)
}

// keyFuncForContext returns a jwt.Keyfunc bound to the request context so JWKS
// fetches honour request cancellation.
//
// Algorithm safety rests on two independent, observable layers and neither is
// this keyfunc re-checking the header algorithm string (a third, redundant
// check would silently absorb the loss of either real layer, which is exactly
// how an algorithm-confusion test can pass while proving nothing):
//
//  1. The parser's jwt.WithValidMethods allowlist rejects any token whose
//     algorithm is outside the configured asymmetric set (HS*, none, ...)
//     before this keyfunc is ever called.
//  2. The signing-method switch below binds the resolved key type to the
//     method family and its default arm rejects anything else. If the
//     allowlist were removed, a forged HS256 token would reach here and this
//     switch would reject it via the default arm — a different, assertable
//     reason, so removing either layer changes an observable outcome.
func (a *oidcAuthenticator) keyFuncForContext(ctx context.Context) jwt.Keyfunc {
	return keyFuncFor(ctx, a.keys)
}

// keyFuncFor is keyFuncForContext over one issuer's key cache. Each issuer
// has its own cache, and a keyfunc reads exactly one, so a key published by
// one issuer can never verify a token the other issuer's parser accepts.
func keyFuncFor(ctx context.Context, keys *jwksCache) jwt.Keyfunc {
	return func(token *jwt.Token) (interface{}, error) {
		kid, _ := token.Header["kid"].(string)
		if strings.TrimSpace(kid) == "" {
			return nil, errMissingKeyID
		}
		var rsaMethod bool
		switch token.Method.(type) {
		case *jwt.SigningMethodRSA, *jwt.SigningMethodRSAPSS:
			rsaMethod = true
		case *jwt.SigningMethodECDSA:
		default:
			return nil, errUnexpectedSigningMethod
		}
		key, err := keys.keyForID(ctx, kid, token.Method.Alg())
		if err != nil {
			return nil, err
		}
		if rsaMethod {
			if _, ok := key.(*rsa.PublicKey); !ok {
				return nil, errKeyMethodMismatch
			}
		} else {
			if _, ok := key.(*ecdsa.PublicKey); !ok {
				return nil, errKeyMethodMismatch
			}
		}
		return key, nil
	}
}

// mint maps validated claims to a conservatively-scoped decision.
//
// With an approver mapping configured, the audience decides the branch and
// nothing else does: a token on the approver audience is minted as an
// approver or denied, and a token on a workspace audience is minted by the
// workspace mappings, which never grant approve. A token on both is denied.
//
// The correlation ID is resolved once by the caller and threaded into both
// branches, so an approver's decision and a workspace caller's carry one the
// same way. Without it every dispatch, admission and approval route refuses
// the request (#524).
func (a *oidcAuthenticator) mint(
	ctx context.Context, claims jwt.MapClaims, correlationID shoal.ID,
) (auth.Decision, error) {
	if a.approver != nil {
		approver, err := a.approverAudience(claims)
		if err != nil {
			return auth.Decision{}, err
		}
		if approver {
			return a.mintApprover(ctx, claims, correlationID)
		}
	}
	return a.mintWorkspace(claims, correlationID)
}

// mintWorkspace mints a token on a workspace audience.
func (a *oidcAuthenticator) mintWorkspace(
	claims jwt.MapClaims, correlationID shoal.ID,
) (auth.Decision, error) {
	subject, err := a.workspaceSubject(claims)
	if err != nil {
		return auth.Decision{}, err
	}
	expiration, err := claims.GetExpirationTime()
	if err != nil {
		return auth.Decision{}, errMalformedClaim
	}
	if expiration == nil {
		return auth.Decision{}, errMissingExpiry
	}

	authorizationValues, err := requiredStringListClaim(
		claims, a.authorizationClaim)
	if err != nil {
		if !(a.allowUnmappedAuthorization &&
			errors.Is(err, errMissingMappedClaim)) {
			return auth.Decision{}, err
		}
		authorizationValues = nil
	}
	if a.authorizationArrayOnly {
		if _, scalar := claims[a.authorizationClaim].(string); scalar {
			return auth.Decision{}, errMalformedClaim
		}
	}
	operations, sources, policies, mapped, err := a.authority(
		authorizationValues, claims)
	if err != nil {
		return auth.Decision{}, err
	}
	if !mapped {
		return auth.Decision{}, errUnmappedAuthorization
	}

	actor := a.defaultActor
	if a.actorClaim != "" {
		value, err := requiredStringClaim(claims, a.actorClaim)
		if err != nil {
			return auth.Decision{}, err
		}
		if actor, err = a.identity(a.actorClaim, value); err != nil {
			return auth.Decision{}, err
		}
	}
	var clientID shoal.ID
	if a.clientIDClaim != "" {
		value, err := requiredStringClaim(claims, a.clientIDClaim)
		if err != nil {
			return auth.Decision{}, err
		}
		if clientID, err = a.identity(a.clientIDClaim, value); err != nil {
			return auth.Decision{}, err
		}
	}
	var onBehalfOf []shoal.ID
	if a.delegationClaim != "" {
		values, err := requiredStringListClaim(claims, a.delegationClaim)
		if err != nil {
			return auth.Decision{}, err
		}
		onBehalfOf = make([]shoal.ID, 0, len(values))
		for _, value := range values {
			identity, err := a.identity(a.delegationClaim, value)
			if err != nil {
				return auth.Decision{}, err
			}
			onBehalfOf = append(onBehalfOf, identity)
		}
	}

	requestID, err := newOIDCRequestID()
	if err != nil {
		return auth.Decision{}, err
	}
	decision, err := auth.NewDecision(auth.DecisionConfig{
		Subject:             subject,
		Actor:               actor,
		ClientID:            clientID,
		OnBehalfOf:          onBehalfOf,
		AuthorizationDomain: workspaceAuthorizationDomain,
		AllowedOperations:   operations,
		PermittedSourceIDs:  sources,
		PermittedPolicyIDs:  policies,
		PolicyGeneration:    workspacePolicyGeneration,
		// The parser accepts through exp+leeway. Give the request-scoped
		// decision the same boundary so the binder does not immediately reject
		// a token the parser accepted within the configured clock skew.
		AuthenticationExpires: expiration.Time.UTC().Add(a.authenticationLeeway),
		RequestID:             requestID,
		CorrelationID:         correlationID,
		AuditPurpose:          a.auditPurpose,
	})
	if err != nil {
		return auth.Decision{}, err
	}
	return decision, nil
}

// workspaceSubject is the workspace principal's subject: the stable identity
// when one is configured (#526), and otherwise the subject claim (or the
// legacy fallback) under the configured identity prefix, as before.
func (a *oidcAuthenticator) workspaceSubject(claims jwt.MapClaims) (shoal.ID, error) {
	if a.identityClaim != nil {
		identity, _, err := a.stableIdentity(claims)
		return identity, err
	}
	claim := a.subjectClaim
	subject, err := requiredStringClaim(claims, claim)
	if errors.Is(err, errMissingMappedClaim) && a.subjectFallbackClaim != "" {
		claim = a.subjectFallbackClaim
		subject, err = requiredStringClaim(claims, claim)
	}
	if err != nil {
		if err == errMissingMappedClaim {
			return "", errMissingSubject
		}
		return "", err
	}
	return a.identity(claim, subject)
}

// authority maps configured claim values to operations and corpus grants. It
// is fail-closed: only an explicitly configured contributor or reader value
// grants visibility. An unmapped value produces no decision.
//
// Label grants (#570) are appended here and only here, and only to a token a
// role mapping has already granted the workspace source: they add policy
// IDs, never an operation or a source, and a token that holds no role gets
// none — including the legacy unmapped-list branch below.
func (a *oidcAuthenticator) authority(
	values []string, claims jwt.MapClaims,
) ([]auth.Operation, [][]byte, [][]byte, bool, error) {
	if a.trimAuthorizationValues {
		trimmed := make([]string, 0, len(values))
		for _, value := range values {
			trimmed = append(trimmed, strings.TrimSpace(value))
		}
		values = trimmed
	}
	operationSet := make(
		map[auth.Operation]struct{},
		len(oidcFleetOperations)+len(oidcContributorOperations)+len(oidcReaderOperations),
	)
	var operations []auth.Operation
	if hasMappedValue(values, a.fleetValues) {
		operations = appendUniqueOperations(
			operations, operationSet, oidcFleetOperations)
	}
	if hasMappedValue(values, a.contributorValues) {
		operations = appendUniqueOperations(
			operations, operationSet, oidcContributorOperations)
	}
	if hasMappedValue(values, a.readerClaimValues) {
		operations = appendUniqueOperations(
			operations, operationSet, oidcReaderOperations)
	}
	if len(operations) > 0 {
		labels, err := a.labelGrants.policies(claims)
		if err != nil {
			return nil, nil, nil, false, err
		}
		policies := make([][]byte, 0, 1+len(labels))
		policies = append(policies, workspaceGrantPolicyID)
		policies = append(policies, labels...)
		return operations,
			[][]byte{workspaceSourceID},
			policies,
			true, nil
	}
	if a.allowUnmappedAuthorization {
		return []auth.Operation{auth.OperationList}, nil, nil, true, nil
	}
	return nil, nil, nil, false, nil
}

func appendUniqueOperations(
	operations []auth.Operation,
	seen map[auth.Operation]struct{},
	values []auth.Operation,
) []auth.Operation {
	for _, operation := range values {
		if _, ok := seen[operation]; ok {
			continue
		}
		seen[operation] = struct{}{}
		operations = append(operations, operation)
	}
	return operations
}

// hasMappedValue reports whether any token claim value is explicitly mapped.
func hasMappedValue(tokenValues []string, configured map[string]struct{}) bool {
	if len(configured) == 0 {
		return false
	}
	for _, value := range tokenValues {
		if _, ok := configured[value]; ok {
			return true
		}
	}
	return false
}

func requiredStringClaim(claims jwt.MapClaims, name string) (string, error) {
	value, ok := claims[name]
	if !ok || value == nil {
		return "", errMissingMappedClaim
	}
	text, ok := value.(string)
	if !ok {
		return "", errMalformedClaim
	}
	if strings.TrimSpace(text) == "" {
		return "", errMissingMappedClaim
	}
	return text, nil
}

func requiredStringListClaim(claims jwt.MapClaims, name string) ([]string, error) {
	value, ok := claims[name]
	if !ok || value == nil {
		return nil, errMissingMappedClaim
	}
	var raw []string
	switch typed := value.(type) {
	case string:
		raw = []string{typed}
	case []string:
		raw = typed
	case []interface{}:
		raw = make([]string, 0, len(typed))
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, errMalformedClaim
			}
			raw = append(raw, text)
		}
	default:
		return nil, errMalformedClaim
	}
	result := make([]string, 0, len(raw))
	for _, item := range raw {
		if strings.TrimSpace(item) != "" {
			result = append(result, item)
		}
	}
	if len(result) == 0 {
		return nil, errMissingMappedClaim
	}
	return result, nil
}

// identity names a claim value under the identity prefix in force. Under the
// sub-derived namespace oidc:<iss># a value containing '#' is refused
// (#546): oidc:<iss>#<tag>#<value> is the form of a non-default subject
// claim's identities, so a sub (or actor, client or delegation value) of
// "<tag>#<value>" would otherwise mint exactly another scheme's identity.
// OIDC Core leaves sub opaque, but no common issuer puts '#' in one.
//
// The refusal is a 401 like any malformed claim, so it is also logged (by
// hashRefusals, naming the claim and never the value): an issuer whose
// client-ID or actor claim routinely carries '#' refuses every user, and the
// scheme digest does not change, so nothing at startup would say why.
func (a *oidcAuthenticator) identity(claim, value string) (shoal.ID, error) {
	if a.trimIdentityValues {
		value = strings.TrimSpace(value)
	}
	if a.flatIdentityValues && strings.Contains(value, "#") {
		a.hashRefusals.note(claim)
		return "", errMalformedClaim
	}
	return shoal.ID(a.identityPrefix + value), nil
}

type legacyEntraConfig struct {
	tenantID          string
	issuer            string
	audience          string
	jwksURI           string
	allowedAlgorithms []string
	clockSkew         time.Duration
	readerRoles       []string
	contributorRoles  []string
	scope             string
}

func (c legacyEntraConfig) configured() bool {
	return c.authConfigured() || strings.TrimSpace(c.scope) != ""
}

func (c legacyEntraConfig) authConfigured() bool {
	return strings.TrimSpace(c.tenantID) != "" ||
		strings.TrimSpace(c.issuer) != "" ||
		strings.TrimSpace(c.audience) != "" ||
		strings.TrimSpace(c.jwksURI) != "" ||
		len(c.readerRoles) > 0 || len(c.contributorRoles) > 0
}

// applyLegacyEntraCompatibility translates the deprecated provider-specific
// configuration into the provider-neutral OIDC model. New OIDC settings win
// field-by-field, allowing an operator to migrate without a flag-day while
// preserving the old issuer, claims, identity, role, and browser defaults.
func applyLegacyEntraCompatibility(
	config oidcConfig,
	legacy legacyEntraConfig,
) oidcConfig {
	if !legacy.configured() {
		return config
	}
	modernConfigured := config.configured()
	tenant := strings.TrimSpace(legacy.tenantID)
	issuer := strings.TrimSpace(legacy.issuer)
	if issuer == "" && tenant != "" {
		issuer = "https://login.microsoftonline.com/" + tenant + "/v2.0"
	}
	if strings.TrimSpace(config.issuer) == "" {
		config.issuer = issuer
	}
	if len(config.audiences) == 0 && strings.TrimSpace(legacy.audience) != "" {
		config.audiences = []string{strings.TrimSpace(legacy.audience)}
	}
	if strings.TrimSpace(config.jwksURI) == "" {
		config.jwksURI = strings.TrimSpace(legacy.jwksURI)
	}
	if len(config.allowedAlgorithms) == 0 &&
		(legacy.authConfigured() || modernConfigured) {
		config.allowedAlgorithms = legacy.allowedAlgorithms
	}
	if config.clockSkew == 0 && (legacy.authConfigured() || modernConfigured) {
		config.clockSkew = legacy.clockSkew
	}
	if legacy.authConfigured() && strings.TrimSpace(config.subjectClaim) == "" {
		config.subjectClaim = "oid"
		config.subjectFallbackClaim = "sub"
		config.identityPrefix = legacyEntraPrefix
		config.defaultActor = legacyEntraActor
		config.auditPurpose = legacyEntraPurpose
		config.trimIdentityValues = true
	}
	if legacy.authConfigured() &&
		strings.TrimSpace(config.authorizationClaim) == "" {
		config.authorizationClaim = "roles"
		config.allowUnmappedAuthorization = true
		config.trimAuthorizationValues = true
		config.authorizationArrayOnly = true
	}
	if len(config.readerClaimValues) == 0 {
		config.readerClaimValues = legacy.readerRoles
	}
	if len(config.contributorValues) == 0 {
		config.contributorValues = legacy.contributorRoles
	}
	clientID := strings.TrimSpace(legacy.audience)
	if strings.TrimSpace(config.browserClientID) == "" {
		config.browserClientID = clientID
	}
	if strings.TrimSpace(config.browserScope) == "" && clientID != "" {
		config.browserScope = strings.TrimSpace(legacy.scope)
		if config.browserScope == "" {
			config.browserScope = "openid profile " + clientID + "/.default"
		}
	}
	authority := legacyEntraAuthority(tenant, issuer)
	if strings.TrimSpace(config.authorizationEndpoint) == "" && authority != "" {
		config.authorizationEndpoint = authority + "/oauth2/v2.0/authorize"
	}
	if strings.TrimSpace(config.tokenEndpoint) == "" && authority != "" {
		config.tokenEndpoint = authority + "/oauth2/v2.0/token"
	}
	config.legacyTenantID = tenant
	config.legacyAuthority = authority
	return config
}

func legacyEntraAuthority(tenant, issuer string) string {
	if tenant != "" {
		return "https://login.microsoftonline.com/" + tenant
	}
	return strings.TrimSuffix(strings.TrimSpace(issuer), "/v2.0")
}

func firstNonZeroID(value, fallback shoal.ID) shoal.ID {
	if value != "" {
		return value
	}
	return fallback
}

// bearerToken extracts the bearer credential from the Authorization header. It
// returns a token-free sentinel that never contains the credential.
func bearerToken(request *http.Request) (string, error) {
	header := request.Header.Get("Authorization")
	if header == "" {
		return "", errMissingBearer
	}
	const prefix = "bearer "
	if len(header) <= len(prefix) ||
		!strings.EqualFold(header[:len(prefix)], prefix) {
		return "", errMissingBearer
	}
	token := strings.TrimSpace(header[len(prefix):])
	if token == "" {
		return "", errMissingBearer
	}
	return token, nil
}

// oidcDenied is the single generic authentication failure. It carries no
// token-derived detail so a raw token can never appear in an error, log, or
// response.
func oidcDenied() error {
	return shoal.NewError(shoal.ErrorUnauthorized, "authentication failed")
}

func newOIDCRequestID() (shoal.ID, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", shoal.WrapError(
			shoal.ErrorUnavailable, "request identity unavailable", err)
	}
	return shoal.ID("oidc-request-" + hex.EncodeToString(raw)), nil
}

type oidcMetadata struct {
	Issuer                string `json:"issuer"`
	JWKSURI               string `json:"jwks_uri"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	// SubjectTypesSupported decides whether the issuer can back an
	// approver mapping; see approverSubjectTypesPublic.
	SubjectTypesSupported []string `json:"subject_types_supported,omitempty"`
}

// oidcMetadataCache resolves the issuer's discovery document once and shares it
// between token validation and the browser's public-client configuration.
type oidcMetadataCache struct {
	issuer       string
	discoveryURL string
	httpClient   *http.Client
	allowHTTP    bool

	mu       sync.Mutex
	document *oidcMetadata
}

func (c *oidcMetadataCache) get(
	ctx context.Context,
	refresh bool,
) (oidcMetadata, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.document != nil && !refresh {
		return *c.document, nil
	}
	body, err := getOIDCDocument(
		ctx, c.httpClient, c.discoveryURL, c.allowHTTP)
	if err != nil {
		return oidcMetadata{}, err
	}
	var document oidcMetadata
	if err := json.Unmarshal(body, &document); err != nil {
		return oidcMetadata{}, shoal.WrapError(
			shoal.ErrorUnavailable, "invalid OIDC discovery document", err)
	}
	if document.Issuer != c.issuer {
		return oidcMetadata{}, shoal.NewError(
			shoal.ErrorUnauthorized,
			"OIDC discovery issuer does not match the configured issuer")
	}
	document.JWKSURI = strings.TrimSpace(document.JWKSURI)
	if document.JWKSURI == "" {
		return oidcMetadata{}, shoal.NewError(
			shoal.ErrorUnavailable, "OIDC discovery document has no jwks_uri")
	}
	if err := validateOIDCEndpoint(
		"discovered JWKS URI", document.JWKSURI, c.allowHTTP); err != nil {
		return oidcMetadata{}, err
	}
	document.AuthorizationEndpoint = strings.TrimSpace(document.AuthorizationEndpoint)
	if document.AuthorizationEndpoint != "" {
		if err := validateOIDCEndpoint(
			"discovered authorization endpoint",
			document.AuthorizationEndpoint,
			c.allowHTTP,
		); err != nil {
			return oidcMetadata{}, err
		}
	}
	document.TokenEndpoint = strings.TrimSpace(document.TokenEndpoint)
	if document.TokenEndpoint != "" {
		if err := validateOIDCEndpoint(
			"discovered token endpoint",
			document.TokenEndpoint,
			c.allowHTTP,
		); err != nil {
			return oidcMetadata{}, err
		}
	}
	c.document = &document
	return document, nil
}

// jwksCache resolves and caches the issuer's signing keys by key identifier.
// It refreshes on rotation but rate-limits fetches so an unknown key identifier
// cannot induce a fetch storm.
type jwksCache struct {
	metadata      *oidcMetadataCache
	staticJWKSURI string
	httpClient    *http.Client
	clock         func() time.Time
	allowHTTP     bool
	// minRefreshInterval is the shortest time between fetch attempts.
	minRefreshInterval time.Duration
	// maxCacheAge is the longest time a fetched key set remains trusted.
	maxCacheAge time.Duration

	mu          sync.Mutex
	keys        map[string]jwksSigningKey
	fetched     bool
	lastAttempt time.Time
	lastSuccess time.Time
	refreshing  bool
	refreshDone chan struct{}
	refreshErr  error
}

// keyForID returns the public key for a key identifier. A miss refreshes at
// most once per minRefreshInterval, while a hit refreshes once maxCacheAge has
// elapsed so issuer-side key removal takes effect within a bounded interval.
// The mutex protects refresh state but is released before network I/O;
// refreshing and refreshDone collapse concurrent callers onto one shared fetch.
func (c *jwksCache) keyForID(
	ctx context.Context,
	kid string,
	algorithm string,
) (crypto.PublicKey, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c.mu.Lock()
		now := c.clock()
		key, keyFound := c.keys[kid]
		cacheAge := now.Sub(c.lastSuccess)
		cacheFresh := !c.lastSuccess.IsZero() &&
			cacheAge >= 0 && cacheAge < c.maxCacheAge
		if keyFound && cacheFresh {
			c.mu.Unlock()
			if key.algorithm != "" && key.algorithm != algorithm {
				return nil, errKeyAlgorithmMismatch
			}
			return key.publicKey, nil
		}
		if c.refreshing {
			done := c.refreshDone
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-done:
				continue
			}
		}
		sinceAttempt := now.Sub(c.lastAttempt)
		if c.fetched && sinceAttempt >= 0 &&
			sinceAttempt < c.minRefreshInterval {
			// Refuse rather than fetch: a recent attempt already ran, so an
			// unknown key or stale cached key fails closed instead of
			// triggering a storm or continuing to trust a removed key.
			err := c.refreshErr
			c.mu.Unlock()
			if err != nil {
				return nil, err
			}
			return nil, errNoMatchingKey
		}
		refreshMetadata := c.fetched
		c.lastAttempt = now
		c.fetched = true
		c.refreshing = true
		c.refreshDone = make(chan struct{})
		done := c.refreshDone
		c.mu.Unlock()

		// Refresh is shared process state, so it must not inherit the first
		// untrusted caller's cancellation. A cache-owned timeout bounds the
		// work, while each caller can stop waiting through its own context.
		go c.refresh(refreshMetadata, done)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-done:
		}
	}
}

func (c *jwksCache) refresh(refreshMetadata bool, done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), oidcHTTPTimeout)
	keys, err := c.fetch(ctx, refreshMetadata)
	cancel()

	c.mu.Lock()
	defer c.mu.Unlock()
	if err == nil {
		c.keys = keys
		c.lastSuccess = c.clock()
	}
	c.refreshErr = err
	c.refreshing = false
	close(done)
}

// fetch resolves the JWKS URI (via static override or OIDC discovery) and loads
// the current signing keys.
func (c *jwksCache) fetch(
	ctx context.Context,
	refreshMetadata bool,
) (map[string]jwksSigningKey, error) {
	jwksURI := c.staticJWKSURI
	if jwksURI == "" {
		metadata, err := c.metadata.get(ctx, refreshMetadata)
		if err != nil {
			return nil, err
		}
		jwksURI = metadata.JWKSURI
	}
	body, err := getOIDCDocument(ctx, c.httpClient, jwksURI, c.allowHTTP)
	if err != nil {
		return nil, err
	}
	return parseJWKS(body)
}

// getOIDCDocument performs a bounded HTTP GET and returns the response body.
func getOIDCDocument(
	ctx context.Context,
	httpClient *http.Client,
	endpoint string,
	allowLoopbackHTTP bool,
) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, shoal.WrapError(
			shoal.ErrorInvalidArgument, "invalid issuer metadata URL", err)
	}
	client := *httpClient
	previousRedirect := client.CheckRedirect
	client.CheckRedirect = func(
		redirected *http.Request,
		via []*http.Request,
	) error {
		if err := validateOIDCEndpoint(
			"redirected issuer metadata URL",
			redirected.URL.String(),
			allowLoopbackHTTP,
		); err != nil {
			return err
		}
		if previousRedirect != nil {
			return previousRedirect(redirected, via)
		}
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		return nil
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, shoal.WrapError(
			shoal.ErrorUnavailable, "issuer metadata is unavailable", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, shoal.NewError(
			shoal.ErrorUnavailable, "issuer metadata request failed")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, oidcMetadataMaxBytes+1))
	if err != nil {
		return nil, shoal.WrapError(
			shoal.ErrorUnavailable, "reading issuer metadata failed", err)
	}
	if len(body) > oidcMetadataMaxBytes {
		return nil, shoal.NewError(
			shoal.ErrorUnavailable, "issuer metadata response is too large")
	}
	return body, nil
}

// jsonWebKey is one entry in a JWKS. Only the fields needed to build an RSA or
// EC public key are decoded.
type jsonWebKey struct {
	Kty    string   `json:"kty"`
	Kid    string   `json:"kid"`
	Use    string   `json:"use"`
	KeyOps []string `json:"key_ops"`
	Alg    string   `json:"alg"`
	N      string   `json:"n"`
	E      string   `json:"e"`
	Crv    string   `json:"crv"`
	X      string   `json:"x"`
	Y      string   `json:"y"`
}

// parseJWKS builds a key-identifier-keyed map of public keys from a JWKS
// document. Entries without a key identifier or of an unsupported type are
// skipped rather than failing the whole set.
type jwksSigningKey struct {
	publicKey crypto.PublicKey
	algorithm string
}

func parseJWKS(body []byte) (map[string]jwksSigningKey, error) {
	var set struct {
		Keys []jsonWebKey `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, shoal.WrapError(
			shoal.ErrorUnavailable, "invalid JWKS document", err)
	}
	keys := make(map[string]jwksSigningKey, len(set.Keys))
	seenKeyIDs := make(map[string]struct{}, len(set.Keys))
	for _, entry := range set.Keys {
		if strings.TrimSpace(entry.Kid) == "" {
			continue
		}
		if _, duplicate := seenKeyIDs[entry.Kid]; duplicate {
			return nil, shoal.NewError(
				shoal.ErrorUnavailable,
				"JWKS document contains a duplicate key identifier")
		}
		seenKeyIDs[entry.Kid] = struct{}{}
		if entry.Use != "" && entry.Use != "sig" {
			continue
		}
		if len(entry.KeyOps) > 0 && !containsString(entry.KeyOps, "verify") {
			continue
		}
		switch entry.Kty {
		case "RSA":
			key, err := entry.rsaKey()
			if err != nil {
				continue
			}
			keys[entry.Kid] = jwksSigningKey{
				publicKey: key,
				algorithm: strings.TrimSpace(entry.Alg),
			}
		case "EC":
			key, err := entry.ecKey()
			if err != nil {
				continue
			}
			keys[entry.Kid] = jwksSigningKey{
				publicKey: key,
				algorithm: strings.TrimSpace(entry.Alg),
			}
		default:
			continue
		}
	}
	if len(keys) == 0 {
		return nil, shoal.NewError(
			shoal.ErrorUnavailable, "JWKS document has no usable signing keys")
	}
	return keys, nil
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (k jsonWebKey) rsaKey() (*rsa.PublicKey, error) {
	modulus, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil || len(modulus) == 0 {
		return nil, fmt.Errorf("invalid RSA modulus")
	}
	exponent, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil || len(exponent) == 0 {
		return nil, fmt.Errorf("invalid RSA exponent")
	}
	e := new(big.Int).SetBytes(exponent)
	if !e.IsInt64() || e.Int64() < 3 || e.Int64() > (1<<31-1) ||
		e.Int64()%2 == 0 {
		return nil, fmt.Errorf("invalid RSA exponent range")
	}
	n := new(big.Int).SetBytes(modulus)
	if n.BitLen() < 2048 {
		return nil, fmt.Errorf("RSA modulus is too small")
	}
	return &rsa.PublicKey{
		N: n,
		E: int(e.Int64()),
	}, nil
}

func (k jsonWebKey) ecKey() (*ecdsa.PublicKey, error) {
	var curve elliptic.Curve
	switch k.Crv {
	case "P-256":
		curve = elliptic.P256()
	case "P-384":
		curve = elliptic.P384()
	case "P-521":
		curve = elliptic.P521()
	default:
		return nil, fmt.Errorf("unsupported EC curve")
	}
	x, err := base64.RawURLEncoding.DecodeString(k.X)
	if err != nil || len(x) == 0 {
		return nil, fmt.Errorf("invalid EC x coordinate")
	}
	y, err := base64.RawURLEncoding.DecodeString(k.Y)
	if err != nil || len(y) == 0 {
		return nil, fmt.Errorf("invalid EC y coordinate")
	}
	key := &ecdsa.PublicKey{
		Curve: curve,
		X:     new(big.Int).SetBytes(x),
		Y:     new(big.Int).SetBytes(y),
	}
	if !curve.IsOnCurve(key.X, key.Y) {
		return nil, fmt.Errorf("EC point is not on the curve")
	}
	return key, nil
}

// hashRefusalInterval bounds how often one claim's '#' refusal is logged.
const hashRefusalInterval = time.Minute

// hashRefusalLog logs a token refused because a claim value minted under
// the sub-derived namespace contains '#' (#546): the first refusal for each
// claim, then at most one per claim per hashRefusalInterval. It names the
// claim and the rule, never the value, which is an identity.
type hashRefusalLog struct {
	logger *slog.Logger
	clock  func() time.Time
	mu     sync.Mutex
	last   map[string]time.Time
}

// newHashRefusalLog logs to logger, or to slog.Default() when it is nil.
func newHashRefusalLog(logger *slog.Logger, clock func() time.Time) *hashRefusalLog {
	if clock == nil {
		clock = time.Now
	}
	return &hashRefusalLog{logger: logger, clock: clock, last: map[string]time.Time{}}
}

func (l *hashRefusalLog) note(claim string) {
	if l == nil {
		return
	}
	now := l.clock()
	l.mu.Lock()
	previous, seen := l.last[claim]
	if seen && now.Sub(previous) < hashRefusalInterval {
		l.mu.Unlock()
		return
	}
	l.last[claim] = now
	l.mu.Unlock()
	logger := l.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("oidc: refused a token because a claim value contains '#'; "+
		"under the default sub-derived identity namespace (oidc:<iss>#<value>) "+
		"no sub, actor, client-ID or delegation value may contain '#' (#546). "+
		"If this claim routinely carries '#', every such token is refused: "+
		"choose another claim. Further refusals for this claim are logged at "+
		"most once a minute",
		"claim", claim)
}
