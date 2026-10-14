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
	"os"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/golang-jwt/jwt/v5"
	"github.com/phrocker/shoal-oss/internal/strictjson"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The approver role (#451) is granted by an operator file, never by ATPL and
// never by a workspace claim mapping.
//
// ATPL is written by registrants, and letting a registrant name who approves
// its own agents' work is the conflict of interest #419 forbids. The workspace
// mappings (-oidc-reader-values and friends) are granted verbatim to every
// token on the workspace audiences; an approver is instead a token on an
// audience of its own, so a workspace token can never become one and an
// approver token can never be anything else.
const (
	approverMappingVersion = "shoal.approvers/v1"
	// approverMappingMaxBytes bounds the file read at startup.
	approverMappingMaxBytes = 64 << 10
	// approverMappingMaxEntries bounds client_ids and values.
	approverMappingMaxEntries = 256
	// approverMappingMaxValues bounds max_values.
	approverMappingMaxValues = 1024
	// approverMappingMaxText bounds every string in the file.
	approverMappingMaxText = 1024
	// oidcApproverAuditPurpose records why an approver decision exists.
	oidcApproverAuditPurpose = "oidc-mapped approver"
	// approverMappingDigestTag domain-separates the mapping digest.
	approverMappingDigestTag = "shoal.approvers/v1-mapping-digest"
)

// oidcApproverOperations is the whole authority of a mapped approver. It is
// the only list in this command that grants OperationActionApprove, and it
// grants nothing else: no read, no dispatch, and above all none of invoke,
// dispatch or execute, which the approval service refuses an approver for
// holding. oidc_approve_grant_test.go asserts both.
var oidcApproverOperations = []auth.Operation{auth.OperationActionApprove}

// approverDelegationClaims are the claims whose presence refuses an approver
// token outright. act and may_act are RFC 8693 delegation; _claim_names,
// _claim_sources and hasgroups are the overage indicators issuers emit in
// place of a group list too long for the token, so the list the token does
// carry is not the whole truth about the principal.
var approverDelegationClaims = []string{
	"act", "may_act", "_claim_names", "_claim_sources", "hasgroups",
}

// approverClientClaims mark a token issued to a client rather than a human.
// Keycloak puts client_id (clientId before 24) on a service account's token;
// Auth0 sets gty to client-credentials. None of them is the guard — the
// positive human assertion is — but a token carrying one is refused
// whatever else it says. sub == azp is the only other client shape this code
// recognizes, and it is caught only in that literal form: Keycloak, Entra and
// Auth0 client-credentials tokens all have sub != azp.
var approverClientClaims = []string{"client_id", "clientId"}

// approverClientGrantTypes are gty values that mark a client-credentials
// token.
var approverClientGrantTypes = map[string]struct{}{
	"client-credentials": {}, "client_credentials": {},
}

var (
	// errApproverAudienceConfusion is returned for a token carrying both the
	// approver audience and a workspace audience, or neither.
	errApproverAudienceConfusion = shoal.NewError(
		shoal.ErrorUnauthorized, "token audience is ambiguous")
	// errApproverDelegated is returned for an approver token carrying a
	// delegation or claim-overage indicator.
	errApproverDelegated = shoal.NewError(
		shoal.ErrorUnauthorized, "approver token carries delegation or overage")
	// errApproverClient is returned when azp is not an allowed client, or
	// names the subject itself.
	errApproverClient = shoal.NewError(
		shoal.ErrorUnauthorized, "approver token client is not allowed")
	// errApproverNotHuman is returned when the human assertion fails.
	errApproverNotHuman = shoal.NewError(
		shoal.ErrorUnauthorized, "approver token is not a human's")
	// errApproverClientToken is returned for a token carrying a client
	// identifier claim or a client-credentials grant marker.
	errApproverClientToken = shoal.NewError(
		shoal.ErrorUnauthorized, "approver token was issued to a client")
	// errApproverSubjectTypes is returned while the issuer's discovery does
	// not state public subject identifiers only.
	errApproverSubjectTypes = shoal.NewError(
		shoal.ErrorUnauthorized,
		"issuer does not state public subject identifiers only")
	// errApproverUnmapped is returned when no claim value matches exactly.
	errApproverUnmapped = shoal.NewError(
		shoal.ErrorUnauthorized, "approver token claim is not mapped")
	// errApproverHoldsFleet is returned when the approver token itself shows
	// the principal holds a fleet mapping, which grants dispatch.
	errApproverHoldsFleet = shoal.NewError(
		shoal.ErrorUnauthorized, "approver token also maps to fleet authority")
)

// approverMappingFile is the strict on-disk shape. Unknown fields, duplicate
// keys and trailing data are refused.
type approverMappingFile struct {
	Version        string                   `json:"version"`
	Issuer         string                   `json:"issuer"`
	Audience       string                   `json:"audience"`
	ClientIDs      []string                 `json:"client_ids"`
	Claim          []string                 `json:"claim"`
	Values         []string                 `json:"values"`
	MaxValues      int                      `json:"max_values"`
	HumanAssertion *approverHumanAssertJSON `json:"human_assertion"`
	// IdentityClaim restates -oidc-identity-claim (#526). It must equal the
	// flag segment for segment, byte for byte, whenever either is set, so
	// the mapping digest pins the identity scheme approvers are named under.
	IdentityClaim []string `json:"identity_claim"`
}

// approverHumanAssertJSON says what makes a token a human's: a claim that
// must equal a value exactly, on a claim only human tokens carry (for
// example one an Auth0 post-login Action adds, which never runs for a
// client-credentials grant).
//
// It is positive by design. An earlier draft also accepted "this claim is
// absent", and the documented example — idtyp absent — passed a Keycloak
// service-account token, because Keycloak never emits idtyp. Absence proves
// nothing about who a token was issued to, so it is not a form this file
// accepts; an "absent" key is an unknown field and refused.
type approverHumanAssertJSON struct {
	Claim  []string `json:"claim"`
	Equals *string  `json:"equals"`
}

// approverMapping is the validated, in-force mapping.
type approverMapping struct {
	issuer    string
	audience  string
	clientIDs map[string]struct{}
	claim     []string
	values    map[string]struct{}
	maxValues int
	human     approverHumanAssertion
	// identityClaim is the stable identity claim path (#526), or nil.
	identityClaim []string
	digest        auth.Digest
}

type approverHumanAssertion struct {
	claim  []string
	equals string
}

// loadApproverMapping reads and validates the operator file. issuer is the
// trimmed -oidc-issuer, audiences the workspace audiences and identityClaim
// the parsed -oidc-identity-claim, or nil.
func loadApproverMapping(
	path string, issuer string, audiences []string, identityClaim []string,
) (*approverMapping, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read -oidc-approver-mapping-file: %w", err)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, approverMappingMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read -oidc-approver-mapping-file: %w", err)
	}
	if len(raw) > approverMappingMaxBytes {
		return nil, approverMappingInvalid("the file exceeds its size bound")
	}
	return parseApproverMapping(raw, issuer, audiences, identityClaim)
}

func approverMappingInvalid(reason string) error {
	return shoal.NewError(
		shoal.ErrorInvalidArgument,
		"-oidc-approver-mapping-file is invalid: "+reason)
}

func parseApproverMapping(
	raw []byte, issuer string, audiences []string, identityClaim []string,
) (*approverMapping, error) {
	if !utf8.Valid(raw) {
		return nil, approverMappingInvalid("the file is not UTF-8")
	}
	// strictjson refuses duplicate keys, keys that match a field only up to
	// case, unknown fields and trailing data, at every level. encoding/json
	// alone matches field names case-insensitively, so {"values": [...],
	// "VALUES": [...]} passed a duplicate-key check that compares exactly
	// and the later key won: the file said one thing to a reviewer and
	// another to this program.
	var file approverMappingFile
	if err := strictjson.Decode(raw, &file); err != nil {
		return nil, approverMappingInvalid(err.Error())
	}
	if file.Version != approverMappingVersion {
		return nil, approverMappingInvalid(
			"version must be " + approverMappingVersion)
	}
	// Byte-equal to the trimmed flag: no trimming, case folding or trailing
	// slash normalization of the file's value. A mismatch is an operator
	// error worth refusing, not a difference worth papering over.
	if file.Issuer == "" || file.Issuer != issuer {
		return nil, approverMappingInvalid(
			"issuer must equal -oidc-issuer byte for byte")
	}
	if !approverText(file.Audience) {
		return nil, approverMappingInvalid("audience is required")
	}
	for _, audience := range audiences {
		if audience == file.Audience {
			return nil, approverMappingInvalid(
				"audience must be disjoint from the workspace audiences")
		}
	}
	clientIDs, err := approverSet("client_ids", file.ClientIDs)
	if err != nil {
		return nil, err
	}
	claim, err := approverPath("claim", file.Claim)
	if err != nil {
		return nil, err
	}
	values, err := approverSet("values", file.Values)
	if err != nil {
		return nil, err
	}
	if file.MaxValues < 1 || file.MaxValues > approverMappingMaxValues {
		return nil, approverMappingInvalid(fmt.Sprintf(
			"max_values must be between 1 and %d", approverMappingMaxValues))
	}
	if file.HumanAssertion == nil {
		return nil, approverMappingInvalid("human_assertion is required")
	}
	humanClaim, err := approverPath(
		"human_assertion.claim", file.HumanAssertion.Claim)
	if err != nil {
		return nil, err
	}
	if file.HumanAssertion.Equals == nil ||
		!approverText(*file.HumanAssertion.Equals) {
		return nil, approverMappingInvalid(
			"human_assertion.equals is required: the assertion is a claim " +
				"only human tokens carry, equal to a value")
	}
	human := approverHumanAssertion{
		claim: humanClaim, equals: *file.HumanAssertion.Equals,
	}
	var mappedIdentityClaim []string
	if file.IdentityClaim != nil {
		mappedIdentityClaim, err = approverPath("identity_claim", file.IdentityClaim)
		if err != nil {
			return nil, err
		}
	}
	if !sameClaimPath(mappedIdentityClaim, identityClaim) {
		return nil, approverMappingInvalid(
			"identity_claim must restate -oidc-identity-claim exactly, and " +
				"be present exactly when the flag is set")
	}
	mapping := &approverMapping{
		issuer: file.Issuer, audience: file.Audience,
		clientIDs: clientIDs, claim: claim, values: values,
		maxValues: file.MaxValues, human: human,
		identityClaim: mappedIdentityClaim,
	}
	mapping.digest = mapping.computeDigest()
	return mapping, nil
}

// approverText is a configured string: non-empty, bounded, and with no
// leading or trailing whitespace and no control character anywhere. The
// token side is compared byte for byte, so a value the operator cannot see
// (a trailing space, a zero-width control) is refused here rather than
// silently never matching — or matching something it should not.
func approverText(value string) bool {
	if value == "" || len(value) > approverMappingMaxText ||
		!utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func approverSet(name string, configured []string) (map[string]struct{}, error) {
	if len(configured) == 0 {
		return nil, approverMappingInvalid(name + " must not be empty")
	}
	if len(configured) > approverMappingMaxEntries {
		return nil, approverMappingInvalid(name + " exceeds its bound")
	}
	set := make(map[string]struct{}, len(configured))
	for _, value := range configured {
		if !approverText(value) {
			return nil, approverMappingInvalid(
				name + " entries must be non-empty, with no surrounding " +
					"whitespace and no control characters")
		}
		if _, duplicate := set[value]; duplicate {
			return nil, approverMappingInvalid(name + " has a duplicate entry")
		}
		set[value] = struct{}{}
	}
	return set, nil
}

// approverPath validates a claim path: a list of literal segments, each one
// object key. A dotted string is one segment naming a key that contains a
// dot, never a path, so a token with a top-level "realm_access.roles" key
// can never satisfy ["realm_access", "roles"], nor the reverse.
func approverPath(name string, segments []string) ([]string, error) {
	if len(segments) == 0 || len(segments) > auth.MaxGrantClaimPathSegments {
		return nil, approverMappingInvalid(fmt.Sprintf(
			"%s must be a list of 1 to %d path segments",
			name, auth.MaxGrantClaimPathSegments))
	}
	for _, segment := range segments {
		if !approverText(segment) {
			return nil, approverMappingInvalid(name + " has an invalid segment")
		}
	}
	return append([]string(nil), segments...), nil
}

// sameClaimPath compares two claim paths segment for segment, byte for byte.
// nil and nil are the same; nil and any path are not.
func sameClaimPath(left, right []string) bool {
	if (left == nil) != (right == nil) || len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// computeDigest is a canonical, length-framed digest of everything that
// decides who is an approver. Set members are sorted, so reordering the file
// does not move it; any change to what the mapping means does.
func (m *approverMapping) computeDigest() auth.Digest {
	var buffer bytes.Buffer
	text := func(value string) {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		buffer.Write(length[:])
		buffer.WriteString(value)
	}
	list := func(values []string) {
		text(fmt.Sprint(len(values)))
		for _, value := range values {
			text(value)
		}
	}
	sorted := func(set map[string]struct{}) []string {
		values := make([]string, 0, len(set))
		for value := range set {
			values = append(values, value)
		}
		sort.Strings(values)
		return values
	}
	text(approverMappingVersion)
	text(m.issuer)
	text(m.audience)
	list(sorted(m.clientIDs))
	list(m.claim)
	list(sorted(m.values))
	text(fmt.Sprint(m.maxValues))
	list(m.human.claim)
	text("equals")
	text(m.human.equals)
	// Appended only when present, so a mapping without it keeps the digest
	// every approval it decided was pinned to.
	if m.identityClaim != nil {
		text("identity_claim")
		list(m.identityClaim)
	}
	return auth.DigestBytes(approverMappingDigestTag, buffer.Bytes())
}

// approverClaimValue resolves a claim path through nested objects. The
// boolean is false when any segment is absent; a non-object on the way is
// malformed.
func approverClaimValue(
	claims jwt.MapClaims, path []string,
) (any, bool, error) {
	var current map[string]any = claims
	for index, segment := range path {
		value, present := current[segment]
		if !present {
			return nil, false, nil
		}
		if index == len(path)-1 {
			return value, true, nil
		}
		next, ok := value.(map[string]any)
		if !ok {
			return nil, false, errMalformedClaim
		}
		current = next
	}
	return nil, false, nil
}

// approverClaimStrings reads the mapped claim as a string or an array of
// strings, refusing anything else: a null, a number, an object, an array
// with a non-string element, an empty array, or more than maxValues values.
func approverClaimStrings(
	claims jwt.MapClaims, path []string, maxValues int,
) ([]string, error) {
	value, present, err := approverClaimValue(claims, path)
	if err != nil {
		return nil, err
	}
	if !present || value == nil {
		return nil, errMissingMappedClaim
	}
	switch typed := value.(type) {
	case string:
		return []string{typed}, nil
	case []any:
		if len(typed) == 0 {
			return nil, errMissingMappedClaim
		}
		if len(typed) > maxValues {
			return nil, errMalformedClaim
		}
		values := make([]string, 0, len(typed))
		for _, item := range typed {
			text, ok := item.(string)
			if !ok {
				return nil, errMalformedClaim
			}
			values = append(values, text)
		}
		return values, nil
	default:
		return nil, errMalformedClaim
	}
}

// approverAudience classifies a token's audiences. It is called only when an
// approver mapping is configured.
func (a *oidcAuthenticator) approverAudience(
	claims jwt.MapClaims,
) (bool, error) {
	audiences, err := claims.GetAudience()
	if err != nil {
		return false, errMalformedClaim
	}
	approver, workspace := false, false
	for _, audience := range audiences {
		if audience == a.approver.audience {
			approver = true
		}
		if _, ok := a.workspaceAudiences[audience]; ok {
			workspace = true
		}
	}
	if approver == workspace {
		// Both is a token that could be read as either, and the safe reading
		// is neither; with an approver mapping configured there is no
		// fallback to the workspace branch. Neither cannot pass the parser.
		return false, errApproverAudienceConfusion
	}
	return approver, nil
}

// mintApprover mints the decision for a token on the approver audience.
// Every check is required and none falls back to anything: a token that
// fails one is denied, not minted as a reader.
func (a *oidcAuthenticator) mintApprover(
	ctx context.Context, claims jwt.MapClaims, correlationID shoal.ID,
) (auth.Decision, error) {
	mapping := a.approver
	if err := a.verifyApproverDiscovery(ctx); err != nil {
		return auth.Decision{}, err
	}
	forbidden := approverDelegationClaims
	if a.delegationClaim != "" {
		forbidden = append(append([]string(nil), forbidden...), a.delegationClaim)
	}
	for _, name := range forbidden {
		if _, present := claims[name]; present {
			return auth.Decision{}, errApproverDelegated
		}
	}
	for _, name := range approverClientClaims {
		if _, present := claims[name]; present {
			return auth.Decision{}, errApproverClientToken
		}
	}
	if grant, present := claims["gty"]; present {
		text, ok := grant.(string)
		if !ok {
			return auth.Decision{}, errApproverClientToken
		}
		if _, client := approverClientGrantTypes[text]; client {
			return auth.Decision{}, errApproverClientToken
		}
	}
	subject, err := requiredStringClaim(claims, "sub")
	if err != nil {
		if errors.Is(err, errMissingMappedClaim) {
			return auth.Decision{}, errMissingSubject
		}
		return auth.Decision{}, err
	}
	client, err := requiredStringClaim(claims, "azp")
	if err != nil {
		return auth.Decision{}, errApproverClient
	}
	if _, allowed := mapping.clientIDs[client]; !allowed || subject == client {
		// sub == azp catches exactly one shape: a token whose subject is
		// literally its own client. Most client-credentials tokens are not
		// that shape (see approverClientClaims); the human assertion is what
		// refuses them.
		return auth.Decision{}, errApproverClient
	}
	if err := mapping.human.holds(claims); err != nil {
		return auth.Decision{}, err
	}
	values, err := approverClaimStrings(claims, mapping.claim, mapping.maxValues)
	if err != nil {
		return auth.Decision{}, err
	}
	matched := ""
	for _, value := range values {
		// Exact bytes: no trimming, case folding or Unicode normalization.
		if _, ok := mapping.values[value]; ok {
			matched = value
			break
		}
	}
	if matched == "" {
		return auth.Decision{}, errApproverUnmapped
	}
	// #489 refuses an approver that holds dispatch. The decision minted here
	// never does, but the human behind it may hold the fleet mapping through
	// another token; where this token shows it, refuse.
	if workspaceValues, err := requiredStringListClaim(
		claims, a.authorizationClaim); err == nil &&
		hasMappedValue(workspaceValues, a.fleetValues) {
		return auth.Decision{}, errApproverHoldsFleet
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
	// Actor = subject. The workspace branch's shared default actor is the
	// same constant for every OIDC principal, and the approval service
	// counts the request's actor as involved, so an approver minted with it
	// overlapped every OIDC requester and no approval could succeed.
	identity := shoal.ID(oidcIdentityPrefix + mapping.issuer + "#" + subject)
	var identityClaimPath []string
	if a.identityClaim != nil {
		// The stable identity, through the one derivation the workspace
		// branch also uses (#526). The raw sub stays in the provenance.
		identity, _, err = a.stableIdentity(claims)
		if err != nil {
			return auth.Decision{}, err
		}
		identityClaimPath = append([]string(nil), a.identityClaim...)
	} else if strings.Contains(subject, "#") {
		// The sub-derived namespace holds no value containing '#' (#546),
		// on this branch as on the workspace branch (identity).
		a.hashRefusals.note("sub")
		return auth.Decision{}, errMalformedClaim
	}
	return auth.NewDecision(auth.DecisionConfig{
		Subject:             identity,
		Actor:               identity,
		ClientID:            shoal.ID(oidcIdentityPrefix + mapping.issuer + "#" + client),
		AuthorizationDomain: workspaceAuthorizationDomain,
		AllowedOperations:   oidcApproverOperations,
		PermittedSourceIDs:  [][]byte{workspaceSourceID},
		// No label grants (#570), whatever the token's claims say: an
		// approver reads nothing, so a label would be clearance with no use
		// but to widen what a future approver operation could see.
		PermittedPolicyIDs: [][]byte{workspaceGrantPolicyID},
		PolicyGeneration:   workspacePolicyGeneration,
		AuthenticationExpires: expiration.Time.UTC().Add(
			a.authenticationLeeway),
		RequestID: requestID,
		// Without a correlation ID every dispatch, admission and approval
		// route refuses the request (#524), and an approver's decide route is
		// one of them.
		CorrelationID: correlationID,
		AuditPurpose:  oidcApproverAuditPurpose,
		GrantProvenance: auth.GrantProvenance{
			Issuer: mapping.issuer, Subject: subject,
			ClaimPath:         append([]string(nil), mapping.claim...),
			MatchedValue:      matched,
			MappingDigest:     mapping.digest,
			IdentityClaimPath: identityClaimPath,
		},
	})
}

func (h approverHumanAssertion) holds(claims jwt.MapClaims) error {
	value, present, err := approverClaimValue(claims, h.claim)
	if err != nil {
		return errApproverNotHuman
	}
	text, ok := value.(string)
	if !present || !ok || text != h.equals {
		return errApproverNotHuman
	}
	return nil
}

// approverSubjectTypesPublic reports whether discovery states public subject
// identifiers and nothing else.
//
// The approval service separates approver from requester by identity, and
// both are oidc:<iss>#<sub>. Under the pairwise subject type an issuer gives
// one human a different sub per client, so the same person requesting through
// one client and approving through another would look like two people, and
// could approve their own request. Entra is such an issuer, and Keycloak
// advertises ["public", "pairwise"] for every realm. Until a stable identity
// claim is used identically on both branches (#526), an issuer that offers
// pairwise identifiers — or does not say — cannot back an approver mapping.
func approverSubjectTypesPublic(metadata oidcMetadata) bool {
	return len(metadata.SubjectTypesSupported) == 1 &&
		metadata.SubjectTypesSupported[0] == "public"
}

// verifyApproverDiscovery refuses an approver mapping unless the issuer's
// discovery states public subject identifiers only. A missing or failed
// discovery is a refusal. It is called at startup, so the server does not
// start, and again on every approver mint against the cached discovery
// document, so a mint can never rest on a check that did not happen.
//
// The subject-types statement is waived when, and only when, a stable
// identity claim is configured (#526): identities then do not come from sub,
// so whether sub is pairwise no longer decides independence. Discovery must
// still be readable; an issuer whose discovery cannot be read is refused
// either way.
func (a *oidcAuthenticator) verifyApproverDiscovery(ctx context.Context) error {
	if a == nil || a.approver == nil {
		return nil
	}
	metadata, err := a.keys.metadata.get(ctx, false)
	if err != nil {
		return errApproverSubjectTypes
	}
	if a.identityClaim != nil {
		return nil
	}
	if !approverSubjectTypesPublic(metadata) {
		return errApproverSubjectTypes
	}
	return nil
}

// approverMappingDigest returns the in-force mapping digest, or the zero
// digest when no mapping is configured.
func (a *oidcAuthenticator) approverMappingDigest() auth.Digest {
	if a == nil || a.approver == nil {
		return auth.Digest{}
	}
	return a.approver.digest
}
