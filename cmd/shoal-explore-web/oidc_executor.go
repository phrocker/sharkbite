// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/golang-jwt/jwt/v5"
	"github.com/phrocker/shoal-oss/internal/strictjson"
	"github.com/phrocker/shoal-oss/pkg/executorref"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The executor mint (#391, PR3) is the only path in this command that grants
// OperationExecute. It is an operator file, like the approver mapping and the
// label grants, and for the same reason: ATPL is written by registrants, and
// a registrant must never be able to name who performs its agents' work.
//
// A worker credential differs from every human credential in three ways, and
// the file encodes each:
//
//   - It is issued by its own issuer. A projected Kubernetes ServiceAccount
//     token is signed by the cluster's service-account issuer, not the human
//     IdP. The file names that issuer, and the branch keeps a discovery and
//     JWKS cache of its own: a key one issuer publishes can never verify a
//     token the other's parser accepts, even when both use the same kid.
//   - It is on an audience of its own, disjoint from the workspace and
//     approver audiences. A token carrying both is refused, not minted as
//     either.
//   - It is positively a service's. The file names a claim that must equal a
//     value exactly (the Kubernetes namespace claim, or gty =
//     client-credentials), and a token without it is refused whatever else
//     it says. Absence proves nothing, so absence is not a form the file
//     accepts.
//
// Each entry maps one exact subject to one executor reference; both are
// unique, so one credential is bound to one surface and one surface has one
// credential. The minted decision executes and resolves its own descriptor
// (ServiceRoleActionExecution) for exactly that reference, acts only as
// itself (no OnBehalfOf), holds no label, and is named
// oidcexec:<iss>#<sub> — outside the human identity family of #553, so an
// executor is never compared with, and never mistaken for, a requester or an
// approver.
const (
	executorMappingVersion = "shoal.executors/v1"
	// executorMappingMaxBytes bounds the file read at startup.
	executorMappingMaxBytes = 64 << 10
	// executorMappingMaxEntries bounds the executors list.
	executorMappingMaxEntries = 256
	// executorMappingDigestTag domain-separates the mapping digest.
	executorMappingDigestTag = "shoal.executors/v1-mapping-digest"
	// oidcExecutorPrefix names every executor identity. It is deliberately
	// not in humanIdentityFamily: "oidcexec:" begins with neither "oidc:"
	// nor "oidcid:".
	oidcExecutorPrefix = "oidcexec:"
	// oidcExecutorAuditPurpose records why an executor decision exists.
	oidcExecutorAuditPurpose = "oidc-mapped executor"
	// oidcExecutorCeiling is the service ceiling identity every executor
	// decision names; ServiceRoleActionExecution requires one.
	oidcExecutorCeiling shoal.ID = "shoal-explore-web-oidc-executor"
	// oidcExecutorCorrelationPrefix prefixes a minted correlation ID.
	oidcExecutorCorrelationPrefix = "oidcexec-correlation-"
)

// oidcExecutorOperations is the whole authority of a mapped executor, and the
// only list in this command that grants OperationExecute.
// oidc_execute_grant_test.go asserts both.
//
// agent_resolve lets the worker read its own descriptor at startup (#391:
// action_execution may resolve only its own bound ref). The fleet confines
// it for this role to the descriptor whose executor ref is the binding
// (resolvableUnderBinding, applied to resolve, list and delivery
// validation), so another ref's descriptor answers not_found. No heartbeat
// and no register: a worker cannot truthfully assert a descriptor's liveness.
var oidcExecutorOperations = []auth.Operation{
	auth.OperationExecute, auth.OperationAgentResolve,
}

var (
	// errExecutorAudienceConfusion is returned for a token carrying the
	// executor audience and any workspace or approver audience.
	errExecutorAudienceConfusion = shoal.NewError(
		shoal.ErrorUnauthorized, "token audience is ambiguous")
	// errExecutorDelegated is returned for an executor token carrying a
	// delegation or claim-overage indicator.
	errExecutorDelegated = shoal.NewError(
		shoal.ErrorUnauthorized, "executor token carries delegation or overage")
	// errExecutorNotService is returned when the service assertion fails.
	errExecutorNotService = shoal.NewError(
		shoal.ErrorUnauthorized, "executor token is not a service's")
	// errExecutorOnHumanBranch is returned for a token on a workspace or
	// approver audience that satisfies the executor service assertion or
	// names a mapped executor subject of the executor issuer.
	errExecutorOnHumanBranch = shoal.NewError(
		shoal.ErrorUnauthorized, "an executor credential is not a human's")
	// errExecutorUnmapped is returned when the subject maps to no executor.
	errExecutorUnmapped = shoal.NewError(
		shoal.ErrorUnauthorized, "executor token subject is not mapped")
)

// executorMappingFile is the strict on-disk shape. Unknown fields, duplicate
// keys, keys matching a field only up to case (at any depth) and trailing
// data are refused.
type executorMappingFile struct {
	Version  string `json:"version"`
	Issuer   string `json:"issuer"`
	Audience string `json:"audience"`
	// JWKSURI optionally overrides the issuer's discovery, as -oidc-jwks-uri
	// does for the human issuer.
	JWKSURI          string                     `json:"jwks_uri"`
	ServiceAssertion *executorServiceAssertJSON `json:"service_assertion"`
	Executors        []executorMappingEntryJSON `json:"executors"`
}

// executorServiceAssertJSON says what makes a token a service's: a claim that
// must equal a value exactly.
type executorServiceAssertJSON struct {
	Claim  []string `json:"claim"`
	Equals *string  `json:"equals"`
}

// executorMappingEntryJSON maps one subject to one executor reference.
type executorMappingEntryJSON struct {
	Subject     *string `json:"subject"`
	ExecutorRef *string `json:"executor_ref"`
}

// executorMapping is the validated, in-force mapping.
type executorMapping struct {
	issuer    string
	audience  string
	jwksURI   string
	assertion executorServiceAssertion
	// refs maps an exact subject to its executor reference.
	refs map[string]string
	// order is the references in file order, so a startup refusal can name
	// an entry's position.
	order  []string
	digest auth.Digest
}

type executorServiceAssertion struct {
	claim  []string
	equals string
}

func executorMappingInvalid(reason string) error {
	return shoal.NewError(
		shoal.ErrorInvalidArgument,
		"-oidc-executor-mapping-file is invalid: "+reason)
}

// loadExecutorMapping reads and validates the operator file. reserved is
// every audience the human issuer's tokens are minted on (the workspace
// audiences and, when configured, the approver audience).
func loadExecutorMapping(
	path string, reserved []string, allowLoopbackHTTP bool,
) (*executorMapping, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read -oidc-executor-mapping-file: %w", err)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, executorMappingMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read -oidc-executor-mapping-file: %w", err)
	}
	if len(raw) > executorMappingMaxBytes {
		return nil, executorMappingInvalid("the file exceeds its size bound")
	}
	return parseExecutorMapping(raw, reserved, allowLoopbackHTTP)
}

func parseExecutorMapping(
	raw []byte, reserved []string, allowLoopbackHTTP bool,
) (*executorMapping, error) {
	if !utf8.Valid(raw) {
		return nil, executorMappingInvalid("the file is not UTF-8")
	}
	var file executorMappingFile
	if err := strictjson.Decode(raw, &file); err != nil {
		return nil, executorMappingInvalid(err.Error())
	}
	if file.Version != executorMappingVersion {
		return nil, executorMappingInvalid(
			"version must be " + executorMappingVersion)
	}
	if err := validateExecutorIssuer(file.Issuer, allowLoopbackHTTP); err != nil {
		return nil, executorMappingInvalid(err.Error())
	}
	if !approverText(file.Audience) {
		return nil, executorMappingInvalid("audience is required")
	}
	for _, audience := range reserved {
		if audience == file.Audience {
			return nil, executorMappingInvalid(
				"audience must be disjoint from the workspace and approver " +
					"audiences")
		}
	}
	if file.JWKSURI != "" {
		if err := validateOIDCEndpoint(
			"jwks_uri", file.JWKSURI, allowLoopbackHTTP); err != nil {
			return nil, executorMappingInvalid(endpointReason(err))
		}
	}
	if file.ServiceAssertion == nil {
		return nil, executorMappingInvalid("service_assertion is required")
	}
	claim, err := approverPath(
		"service_assertion.claim", file.ServiceAssertion.Claim)
	if err != nil {
		// approverPath names the approver file; restate it for this one.
		return nil, executorMappingInvalid(
			"service_assertion.claim must be a list of 1 to " +
				fmt.Sprint(auth.MaxGrantClaimPathSegments) +
				" non-empty path segments")
	}
	if file.ServiceAssertion.Equals == nil ||
		!approverText(*file.ServiceAssertion.Equals) {
		return nil, executorMappingInvalid(
			"service_assertion.equals is required: the assertion is a claim " +
				"only service tokens carry, equal to a value")
	}
	if len(file.Executors) == 0 {
		return nil, executorMappingInvalid("executors must not be empty")
	}
	if len(file.Executors) > executorMappingMaxEntries {
		return nil, executorMappingInvalid("executors exceeds its bound")
	}
	refs := make(map[string]string, len(file.Executors))
	order := make([]string, 0, len(file.Executors))
	bound := make(map[string]struct{}, len(file.Executors))
	for index, entry := range file.Executors {
		// Positions, never values: a refusal must not echo a subject or a
		// reference, which may hold any bytes.
		position := fmt.Sprintf("executors[%d]", index)
		if entry.Subject == nil || !approverText(*entry.Subject) {
			return nil, executorMappingInvalid(position +
				".subject must be non-empty, with no surrounding whitespace " +
				"and no control characters")
		}
		if entry.ExecutorRef == nil {
			return nil, executorMappingInvalid(position +
				".executor_ref is required")
		}
		if err := executorref.ValidExecutorRef(*entry.ExecutorRef); err != nil {
			return nil, executorMappingInvalid(position +
				".executor_ref is not a valid executor reference")
		}
		subject, ref := *entry.Subject, *entry.ExecutorRef
		identity := executorIdentity(file.Issuer, subject)
		if err := shoal.ValidateRequiredID("executor identity", identity); err != nil {
			return nil, executorMappingInvalid(position +
				".subject is too long to name an executor under this issuer")
		}
		if _, duplicate := refs[subject]; duplicate {
			return nil, executorMappingInvalid(position +
				".subject is mapped twice: one credential maps to one executor")
		}
		if _, duplicate := bound[ref]; duplicate {
			return nil, executorMappingInvalid(position +
				".executor_ref is mapped twice: one executor has one credential")
		}
		refs[subject] = ref
		bound[ref] = struct{}{}
		order = append(order, ref)
	}
	mapping := &executorMapping{
		issuer: file.Issuer, audience: file.Audience, jwksURI: file.JWKSURI,
		assertion: executorServiceAssertion{
			claim: claim, equals: *file.ServiceAssertion.Equals,
		},
		refs: refs, order: order,
	}
	mapping.digest = mapping.computeDigest()
	return mapping, nil
}

// validateExecutorIssuer applies the OIDC issuer rule (#553) — an absolute
// https URL with a valid host, no user info, no query, and no '#' anywhere,
// including the empty fragment url.Parse reports for "https://x/#" — and
// additionally requires the issuer to be canonical: exactly what url.Parse
// renders it as, with a lower-case host and no default port. The issuer is
// compared byte for byte with every token's iss and embedded in every
// executor identity, so two spellings of one issuer would be two namespaces.
func validateExecutorIssuer(raw string, allowLoopbackHTTP bool) error {
	if raw == "" {
		return errors.New("issuer is required")
	}
	if err := validateOIDCEndpoint("issuer", raw, allowLoopbackHTTP); err != nil {
		return errors.New(endpointReason(err))
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.String() != raw || strings.TrimSpace(raw) != raw ||
		parsed.RawPath != "" || parsed.Host != strings.ToLower(parsed.Host) ||
		(parsed.Scheme == "https" && parsed.Port() == "443") ||
		(parsed.Scheme == "http" && parsed.Port() == "80") ||
		parsed.Opaque != "" || parsed.ForceQuery {
		return errors.New("issuer must be canonical: a lower-case host, no " +
			"default port, and exactly as it parses")
	}
	for _, segment := range strings.Split(parsed.Path, "/") {
		if segment == "." || segment == ".." {
			return errors.New("issuer must be canonical: no dot segments")
		}
	}
	return nil
}

// endpointReason is validateOIDCEndpoint's refusal without its error code.
func endpointReason(err error) string {
	var typed *shoal.Error
	if errors.As(err, &typed) && typed.Message != "" {
		return typed.Message
	}
	return err.Error()
}

// executorIdentity names an executor: its issuer and its raw subject, under a
// prefix of its own. Issuers never contain '#', so the first '#' separates
// them and no two (issuer, subject) pairs share a name.
func executorIdentity(issuer, subject string) shoal.ID {
	return shoal.ID(oidcExecutorPrefix + issuer + "#" + subject)
}

// computeDigest is a canonical, length-framed digest of everything that
// decides who is an executor for what. Entries are sorted by subject, so
// reordering the file does not move it.
func (m *executorMapping) computeDigest() auth.Digest {
	var buffer bytes.Buffer
	text := func(value string) {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		buffer.Write(length[:])
		buffer.WriteString(value)
	}
	text(executorMappingVersion)
	text(m.issuer)
	text(m.audience)
	text(m.jwksURI)
	text(fmt.Sprint(len(m.assertion.claim)))
	for _, segment := range m.assertion.claim {
		text(segment)
	}
	text("equals")
	text(m.assertion.equals)
	subjects := make([]string, 0, len(m.refs))
	for subject := range m.refs {
		subjects = append(subjects, subject)
	}
	sort.Strings(subjects)
	text(fmt.Sprint(len(subjects)))
	for _, subject := range subjects {
		text(subject)
		text(m.refs[subject])
	}
	return auth.DigestBytes(executorMappingDigestTag, buffer.Bytes())
}

func (a executorServiceAssertion) holds(claims jwt.MapClaims) error {
	value, present, err := approverClaimValue(claims, a.claim)
	if err != nil {
		return errExecutorNotService
	}
	text, ok := value.(string)
	if !present || !ok || text != a.equals {
		return errExecutorNotService
	}
	return nil
}

// oidcExecutorBranch verifies and mints executor tokens. Its parser accepts
// only the mapping's issuer and audience, and its key cache is its own.
type oidcExecutorBranch struct {
	mapping *executorMapping
	parser  *jwt.Parser
	keys    *jwksCache
}

func newOIDCExecutorBranch(
	mapping *executorMapping,
	algorithms []string,
	skew time.Duration,
	clock func() time.Time,
	httpClient *http.Client,
	allowLoopbackHTTP bool,
) *oidcExecutorBranch {
	parser := jwt.NewParser(
		jwt.WithValidMethods(algorithms),
		jwt.WithIssuer(mapping.issuer),
		jwt.WithAudience(mapping.audience),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(skew),
		jwt.WithTimeFunc(clock),
	)
	metadata := &oidcMetadataCache{
		issuer: mapping.issuer,
		discoveryURL: strings.TrimRight(mapping.issuer, "/") +
			"/.well-known/openid-configuration",
		httpClient: httpClient,
		allowHTTP:  allowLoopbackHTTP,
	}
	return &oidcExecutorBranch{
		mapping: mapping,
		parser:  parser,
		keys: &jwksCache{
			metadata:           metadata,
			staticJWKSURI:      mapping.jwksURI,
			httpClient:         httpClient,
			clock:              clock,
			minRefreshInterval: oidcJWKSMinRefreshInterval,
			maxCacheAge:        oidcJWKSMaxCacheAge,
			allowHTTP:          allowLoopbackHTTP,
		},
	}
}

// addressed reports whether a token's unverified audience names the
// executor audience. It decides only which verifier runs; the executor
// verifier then checks the signature against the executor issuer's keys and
// the issuer and audience exactly.
func (e *oidcExecutorBranch) addressed(raw string) bool {
	claims := jwt.MapClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(raw, claims); err != nil {
		return false
	}
	audiences, err := claims.GetAudience()
	if err != nil {
		return false
	}
	for _, audience := range audiences {
		if audience == e.mapping.audience {
			return true
		}
	}
	return false
}

// refuseOnHumanBranch refuses a verified human-issuer token that also carries
// the executor audience, or that is an executor's token by its service
// assertion or its mapped subject.
func (e *oidcExecutorBranch) refuseOnHumanBranch(claims jwt.MapClaims) error {
	audiences, err := claims.GetAudience()
	if err != nil {
		return errMalformedClaim
	}
	for _, audience := range audiences {
		if audience == e.mapping.audience {
			return errExecutorAudienceConfusion
		}
	}
	// One principal cannot be both a human and an executor. A token on a
	// human audience that carries the executor service assertion, or whose
	// issuer is the executor issuer and whose sub is a mapped executor, is
	// refused here, on the workspace and approver branches alike. Without
	// this, a mapped service account of a shared issuer (an Entra workload
	// identity on the tenant issuer) sending its token to the workspace
	// audience would be minted as a reader, and with label grants.
	if e.mapping.assertion.holds(claims) == nil {
		return errExecutorOnHumanBranch
	}
	if issuer, err := claims.GetIssuer(); err == nil && issuer == e.mapping.issuer {
		if subject, ok := claims["sub"].(string); ok {
			if _, mapped := e.mapping.refs[subject]; mapped {
				return errExecutorOnHumanBranch
			}
		}
	}
	return nil
}

// authenticateExecutor verifies a token on the executor audience and mints
// its decision.
func (a *oidcAuthenticator) authenticateExecutor(
	request *http.Request, raw string,
) (auth.Decision, error) {
	ctx := request.Context()
	claims := jwt.MapClaims{}
	if _, err := a.executor.parser.ParseWithClaims(
		raw, claims, a.executorKeyFunc(ctx)); err != nil {
		return auth.Decision{}, err
	}
	// The #527 contract: every dispatch route refuses a decision without a
	// correlation ID, and the execute routes are dispatch routes.
	correlationID, err := correlationIDFor(request, oidcExecutorCorrelationPrefix)
	if err != nil {
		return auth.Decision{}, err
	}
	return a.mintExecutor(claims, correlationID)
}

// executorKeyFunc reads the executor issuer's key cache and no other.
func (a *oidcAuthenticator) executorKeyFunc(ctx context.Context) jwt.Keyfunc {
	return keyFuncFor(ctx, a.executor.keys)
}

// mintExecutor mints the decision for a verified executor token. Every check
// is required and none falls back to anything.
func (a *oidcAuthenticator) mintExecutor(
	claims jwt.MapClaims, correlationID shoal.ID,
) (auth.Decision, error) {
	mapping := a.executor.mapping
	audiences, err := claims.GetAudience()
	if err != nil {
		return auth.Decision{}, errMalformedClaim
	}
	for _, audience := range audiences {
		_, workspace := a.workspaceAudiences[audience]
		approver := a.approver != nil && audience == a.approver.audience
		if workspace || approver {
			return auth.Decision{}, errExecutorAudienceConfusion
		}
	}
	forbidden := approverDelegationClaims
	if a.delegationClaim != "" {
		forbidden = append(append([]string(nil), forbidden...), a.delegationClaim)
	}
	for _, name := range forbidden {
		if _, present := claims[name]; present {
			return auth.Decision{}, errExecutorDelegated
		}
	}
	if err := mapping.assertion.holds(claims); err != nil {
		return auth.Decision{}, err
	}
	subject, err := requiredStringClaim(claims, "sub")
	if err != nil {
		if errors.Is(err, errMissingMappedClaim) {
			return auth.Decision{}, errMissingSubject
		}
		return auth.Decision{}, err
	}
	// Exact bytes: no trimming, case folding or normalization.
	ref, mapped := mapping.refs[subject]
	if !mapped {
		return auth.Decision{}, errExecutorUnmapped
	}
	expiration, err := claims.GetExpirationTime()
	if err != nil {
		return auth.Decision{}, errMalformedClaim
	}
	if expiration == nil {
		return auth.Decision{}, errMissingExpiry
	}
	requestID, err := newOIDCRequestID()
	if err != nil {
		return auth.Decision{}, err
	}
	identity := executorIdentity(mapping.issuer, subject)
	return auth.NewDecision(auth.DecisionConfig{
		// Subject = actor = client: a worker acts as itself and is its own
		// client. Attestation keys a statement by (domain, subject, client)
		// and requires the client, so it is set.
		Subject:             identity,
		Actor:               identity,
		ClientID:            identity,
		AuthorizationDomain: workspaceAuthorizationDomain,
		AllowedOperations:   oidcExecutorOperations,
		PermittedSourceIDs:  [][]byte{workspaceSourceID},
		// No label grants (#570): an executor holds none, whatever its
		// token's claims say.
		PermittedPolicyIDs: [][]byte{workspaceGrantPolicyID},
		PolicyGeneration:   workspacePolicyGeneration,
		AuthenticationExpires: expiration.Time.UTC().Add(
			a.authenticationLeeway),
		RequestID:              requestID,
		CorrelationID:          correlationID,
		AuditPurpose:           oidcExecutorAuditPurpose,
		ServiceRole:            auth.ServiceRoleActionExecution,
		ServiceCeilingIdentity: oidcExecutorCeiling,
		ExecutorBinding:        ref,
		GrantProvenance: auth.GrantProvenance{
			Issuer: mapping.issuer, Subject: subject,
			ClaimPath:     []string{"sub"},
			MatchedValue:  subject,
			MappingDigest: mapping.digest,
		},
	})
}

// refuseUnconfiguredExecutorRefs refuses a mapping that binds a credential to
// a reference this host does not configure (-fleet-executor-refs, which every
// external reference must also appear in). No descriptor can register
// against such a reference, so the credential would be bound to nothing: an
// operator error worth refusing at startup rather than a worker that pulls
// an empty queue forever. The refusal names the entry's position, never the
// reference.
func (a *oidcAuthenticator) refuseUnconfiguredExecutorRefs(
	executors configuredFleetExecutors,
) error {
	if a == nil || a.executor == nil {
		return nil
	}
	for index, ref := range a.executor.mapping.order {
		if _, configured := executors.ResolveExecutor(ref); !configured {
			return executorMappingInvalid(fmt.Sprintf(
				"executors[%d].executor_ref is not configured by "+
					"-fleet-executor-refs: no descriptor can register against "+
					"it, so the credential would be bound to nothing", index))
		}
	}
	return nil
}

// executorMappingDigest returns the in-force executor mapping digest and its
// entry count, or the zero digest when none is configured.
func (a *oidcAuthenticator) executorMappingDigest() (auth.Digest, int) {
	if a == nil || a.executor == nil {
		return auth.Digest{}, 0
	}
	return a.executor.mapping.digest, len(a.executor.mapping.refs)
}
