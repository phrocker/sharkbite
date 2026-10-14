// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/golang-jwt/jwt/v5"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// A stable identity claim (#526).
//
// The approval service separates approver from requester by identity. With
// identities derived from sub, that separation is only as good as the
// issuer's promise that sub is the same for one human across clients — the
// public subject type. Entra issues pairwise subjects, and Keycloak
// advertises the pairwise type for every realm, so neither can back an
// approver mapping on sub.
//
// -oidc-identity-claim names another claim — Entra's oid, a Keycloak user ID
// exposed by a mapper — that the operator asserts is one value per human
// across every client. Code cannot prove that; the docs say per issuer how to
// choose it. What the code guarantees is that the claim is read identically
// on both branches, by stableIdentity and nothing else, into a namespace of
// its own (oidcid:) that cannot collide with the sub-derived oidc: or the
// legacy entra: identities.
const (
	// oidcStableIdentityPrefix begins every identity derived from the stable
	// identity claim. It differs from oidcIdentityPrefix ("oidc:") in its
	// first five bytes, so no identity of one form is a prefix of, or equal
	// to, one of the other.
	oidcStableIdentityPrefix = "oidcid:"
	// maxStableIdentityBytes bounds the claim value.
	maxStableIdentityBytes = 256
	// identitySchemeDigestTag domain-separates the identity scheme digest.
	identitySchemeDigestTag = "shoal-explore-web/identity-scheme/v1"
	// identityClaimPathTag domain-separates the claim path tag.
	identityClaimPathTag = "shoal-explore-web/identity-claim-path/v1"
	// subjectClaimTagDomain domain-separates the subject claim tag (#546).
	subjectClaimTagDomain = "shoal-explore-web/subject-claim/v1"
)

// mutableIdentityClaims are claims that do not name one human stably, and
// are refused as the last segment of the path, compared without regard to
// case:
//
//   - claims a human, or an administrator in the ordinary course, can
//     change, including the OIDC profile fields. A changed value would be a
//     new identity for the same human, and a reused one an old identity for
//     a new human;
//   - per-session and per-token claims (sid, session_state, jti, nonce, the
//     hashes and the times). They name a login or a token, so one human
//     would be a new identity every time, and the value is not theirs;
//   - per-client claims (azp, client_id, cid). They name the application,
//     so every human using one client would be one identity, and one human
//     on two clients two.
//
// The list cannot be complete: a custom claim can be any of these. Choosing
// the claim is the operator's assertion (docs/approval.md), and that
// includes never choosing a path under a parent the user can edit.
var mutableIdentityClaims = map[string]struct{}{
	// Editable identifiers and profile fields.
	"email": {}, "preferred_username": {}, "upn": {}, "unique_name": {},
	"name": {}, "nickname": {}, "given_name": {}, "family_name": {},
	"locale": {}, "picture": {}, "website": {}, "zoneinfo": {},
	// Per session and per token.
	"sid": {}, "jti": {}, "session_state": {}, "nonce": {}, "at_hash": {},
	"c_hash": {}, "auth_time": {}, "iat": {}, "exp": {}, "nbf": {},
	"acr": {}, "amr": {},
	// Per client.
	"azp": {}, "client_id": {}, "cid": {},
}

// entraMultiTenantSegments are the issuer path segments of an Entra
// endpoint that is not one tenant's. An object ID is unique only within a
// tenant, so on these an oid would not name one human.
var entraMultiTenantSegments = map[string]struct{}{
	"common": {}, "organizations": {}, "{tenantid}": {},
}

func identityClaimInvalid(reason string) error {
	return shoal.NewError(
		shoal.ErrorInvalidArgument, "-oidc-identity-claim is invalid: "+reason)
}

// parseIdentityClaimFlag reads -oidc-identity-claim: exactly one JSON array
// of path segments, validated as the approver mapping's claim paths are. A
// dotted string is refused (it is not an array); a segment containing a dot
// names a key containing a dot. Empty means not configured.
func parseIdentityClaimFlag(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	var segments []string
	if err := decoder.Decode(&segments); err != nil {
		return nil, identityClaimInvalid(
			"it must be a JSON array of path segments, such as [\"oid\"]")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, identityClaimInvalid("it must hold one JSON array")
	}
	if len(segments) == 0 || len(segments) > auth.MaxGrantClaimPathSegments {
		return nil, identityClaimInvalid(fmt.Sprintf(
			"it must have 1 to %d path segments", auth.MaxGrantClaimPathSegments))
	}
	for _, segment := range segments {
		if !approverText(segment) {
			return nil, identityClaimInvalid("it has an invalid segment")
		}
	}
	if len(segments) == 1 && segments[0] == "sub" {
		return nil, identityClaimInvalid(
			"[\"sub\"] is the subject the stable identity replaces; leave " +
				"the flag unset to name principals by sub")
	}
	last := strings.ToLower(segments[len(segments)-1])
	if _, mutable := mutableIdentityClaims[last]; mutable {
		return nil, identityClaimInvalid(
			"it names a claim that does not name one human stably (" +
				segments[len(segments)-1] + ": editable, per-session, per-token " +
				"or per-client); choose an immutable per-user identifier")
	}
	return segments, nil
}

// refuseIdentityClaimCombination refuses the configurations under which a
// stable identity would not be the only identity a principal carries.
//
// A non-default subject claim, an actor claim and a delegation claim each
// put a second, client-scoped identity into the decision, and the approval
// service counts every one of them as involved: the separation would again
// be judged partly in a pairwise identity space. The legacy Entra mode names
// principals entra:<oid> and is replaced by this one. An Entra issuer that is
// not one tenant's makes oid ambiguous.
func refuseIdentityClaimCombination(config oidcConfig, issuer string) error {
	if subject := strings.TrimSpace(config.subjectClaim); subject != "" && subject != "sub" {
		return identityClaimInvalid(
			"it cannot be combined with -oidc-subject-claim; the stable " +
				"identity is the subject")
	}
	if config.subjectFallbackClaim != "" || config.identityPrefix != "" ||
		config.trimIdentityValues || config.legacyTenantID != "" {
		return identityClaimInvalid(
			"it cannot be combined with the legacy Entra identity mode; " +
				"use -oidc-identity-claim '[\"oid\"]' on the tenant issuer instead")
	}
	if strings.TrimSpace(config.actorClaim) != "" {
		return identityClaimInvalid(
			"it cannot be combined with -oidc-actor-claim")
	}
	if strings.TrimSpace(config.delegationClaim) != "" {
		return identityClaimInvalid(
			"it cannot be combined with -oidc-delegation-claim")
	}
	parsed, err := url.Parse(issuer)
	if err != nil {
		return identityClaimInvalid("the issuer is not a URL")
	}
	for _, segment := range strings.Split(parsed.Path, "/") {
		if _, multiTenant := entraMultiTenantSegments[strings.ToLower(segment)]; multiTenant {
			return identityClaimInvalid(
				"the issuer path names " + segment + ", which is not one " +
					"tenant; an identifier is unique only within a tenant, so " +
					"use the tenant-specific issuer")
		}
	}
	return nil
}

// stableIdentityValue is the one derivation of the stable identity claim,
// shared by the workspace and approver branches. The claim must be present
// and a string of 1 to maxStableIdentityBytes bytes of valid UTF-8, with no
// control character and no leading or trailing whitespace. It is never
// trimmed: a padded value is refused rather than read as another value, so
// " x" can neither become nor impersonate "x". Anything else — absent, null,
// empty, a number, an object, an array — fails closed, and Authenticate
// collapses every failure to the same generic denial.
func stableIdentityValue(claims jwt.MapClaims, path []string) (string, error) {
	value, present, err := approverClaimValue(claims, path)
	if err != nil {
		return "", errMalformedClaim
	}
	if !present || value == nil {
		return "", errMissingSubject
	}
	text, ok := value.(string)
	if !ok {
		return "", errMalformedClaim
	}
	if text == "" {
		return "", errMissingSubject
	}
	if len(text) > maxStableIdentityBytes || !utf8.ValidString(text) ||
		strings.TrimSpace(text) != text {
		return "", errMalformedClaim
	}
	for _, character := range text {
		if unicode.IsControl(character) {
			return "", errMalformedClaim
		}
	}
	return text, nil
}

// claimPathTag is the namespace segment of a stable identity: the first 16
// hex digits of a digest of the claim path. Two stable schemes on one issuer
// therefore never share a namespace, so an identity minted under one cannot
// be read as one minted under another.
func claimPathTag(path []string) string {
	var buffer bytes.Buffer
	for _, segment := range path {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(segment)))
		buffer.Write(length[:])
		buffer.WriteString(segment)
	}
	digest := auth.DigestBytes(identityClaimPathTag, buffer.Bytes())
	return hex.EncodeToString(digest[:8])
}

// subjectClaimTag is the namespace segment of a non-default subject claim
// (#546): the first 16 hex digits of a digest of the claim name.
//
// A digest, not the name. -oidc-subject-claim accepts any name up to 256
// bytes without NUL, CR or LF, so a name may itself contain '#' or ':'; a
// name used as a tag would need a charset of its own and would still leave a
// boundary to argue about. Sixteen lowercase hex digits are fixed length and
// '#'-free, so in oidc:<iss>#<tag>#<value> the issuer ends at the first '#'
// (#553 refuses '#' in the issuer), the tag at the second, and the value is
// everything after it — '#' included. It is the shape #553 gave the stable
// identity's claim path tag, under a domain of its own.
func subjectClaimTag(claim string) string {
	digest := auth.DigestBytes(subjectClaimTagDomain, []byte(claim))
	return hex.EncodeToString(digest[:8])
}

// subjectIdentityPrefix is the identity prefix for the subject claim, and
// whether it is flat — the sub-derived namespace oidc:<iss>#, under which no
// value may contain '#'.
//
// sub keeps oidc:<iss>#<sub>, so default deployments are unchanged. Any other
// subject claim mints oidc:<iss>#<claim tag>#<value> (#546): before, it
// minted oidc:<iss>#<value>, the namespace of sub, so after a switch between
// sub and the claim, or between two claims, the identities of the scheme
// replaced sat inside the namespace in force and the approval service could
// not tell them apart from current ones. A configured prefix — the legacy
// Entra mode's entra: — is kept as it is.
func subjectIdentityPrefix(
	configured, issuer, subjectClaim string,
) (string, bool) {
	base := oidcIdentityPrefix + issuer + "#"
	if configured != "" && configured != base {
		return configured, false
	}
	if subjectClaim == "sub" {
		return base, true
	}
	return base + subjectClaimTag(subjectClaim) + "#", false
}

// stableIdentityNamespace is oidcid:<iss>#<path tag>#, the namespace every
// identity of the scheme in force begins with.
func (a *oidcAuthenticator) stableIdentityNamespace() string {
	return oidcStableIdentityPrefix + a.expectedIssuer + "#" +
		claimPathTag(a.identityClaim) + "#"
}

// stableIdentity returns oidcid:<iss>#<path tag>#<value> and the raw value.
// The issuer is this deployment's one configured issuer and the tag is fixed
// length, so the value is everything after the tag's '#'; a value containing
// '#' is still one identity.
func (a *oidcAuthenticator) stableIdentity(
	claims jwt.MapClaims,
) (shoal.ID, string, error) {
	value, err := stableIdentityValue(claims, a.identityClaim)
	if err != nil {
		return "", "", err
	}
	return shoal.ID(a.stableIdentityNamespace() + value), value, nil
}

// oidcIdentityScheme is the identity scheme this authenticator mints under.
type oidcIdentityScheme struct {
	// issuer keys the coordination store's identity-scheme row.
	issuer string
	// digest is the scheme: issuer, claim path and identity format. Every
	// replica serving the issuer must agree on it.
	digest auth.Digest
	// approvals is what the approval service is told: the namespace in
	// force, the identity family it belongs to, and the stamp (zero under
	// the default sub-derived scheme, which is what records written before
	// the stamp existed decode as).
	approvals fleet.IdentityScheme
	// sharedNamespaceDigest, for a non-default subject claim, is the digest
	// the same configuration had before #546 gave the claim a namespace of
	// its own; zero otherwise. It only explains a startup refusal.
	sharedNamespaceDigest auth.Digest
}

// humanIdentityFamily are the prefixes of every identity this command mints
// for a human, under any scheme and any issuer: sub-derived (oidc:), stable
// (oidcid:) and legacy Entra (entra:). It is deliberately issuer-agnostic. A
// deployment has exactly one human OIDC issuer, so an identity under another
// issuer was minted before the issuer changed (Entra v1 sts.windows.net to
// v2 login.microsoftonline.com, a Keycloak hostname move), and the same human
// may hold it; scoping the family to the issuer in force would make it an
// unrelated principal. The executor identities of #391 (oidcexec:) are not
// human and are not in the family: "oidcexec:" begins with neither "oidc:"
// nor "oidcid:", and must never be added here.
var humanIdentityFamily = []string{
	oidcIdentityPrefix, oidcStableIdentityPrefix, legacyEntraPrefix,
}

// identityFamily is the family, plus the namespace in force should it lie
// outside it (a custom identity prefix).
func (a *oidcAuthenticator) identityFamily(current string) []string {
	family := append([]string(nil), humanIdentityFamily...)
	for _, namespace := range family {
		if strings.HasPrefix(current, namespace) {
			return family
		}
	}
	return append(family, current)
}

// identityScheme describes the scheme in force.
func (a *oidcAuthenticator) identityScheme() oidcIdentityScheme {
	var buffer bytes.Buffer
	text := func(value string) {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		buffer.Write(length[:])
		buffer.WriteString(value)
	}
	scheme := oidcIdentityScheme{issuer: a.expectedIssuer}
	if a.identityClaim != nil {
		prefix := a.stableIdentityNamespace()
		text("stable")
		text(a.expectedIssuer)
		text(fmt.Sprint(len(a.identityClaim)))
		for _, segment := range a.identityClaim {
			text(segment)
		}
		text(prefix)
		scheme.digest = auth.DigestBytes(identitySchemeDigestTag, buffer.Bytes())
		scheme.approvals = fleet.IdentityScheme{
			Digest: scheme.digest, Prefix: prefix,
			Family: a.identityFamily(prefix),
		}
		return scheme
	}
	subjectDigest := func(prefix string) auth.Digest {
		buffer.Reset()
		text("subject")
		text(a.expectedIssuer)
		text(a.subjectClaim)
		text(a.subjectFallbackClaim)
		text(prefix)
		text(fmt.Sprint(a.trimIdentityValues))
		return auth.DigestBytes(identitySchemeDigestTag, buffer.Bytes())
	}
	// The inputs and their encoding are those of every release before
	// #546, so the default and legacy Entra schemes keep their digests. A
	// non-default subject claim's digest moves with its new prefix.
	scheme.digest = subjectDigest(a.identityPrefix)
	scheme.approvals = fleet.IdentityScheme{
		Prefix: a.identityPrefix, Family: a.identityFamily(a.identityPrefix),
		Flat: a.flatIdentityValues,
	}
	if base := oidcIdentityPrefix + a.expectedIssuer + "#"; a.identityPrefix !=
		base && strings.HasPrefix(a.identityPrefix, base) {
		// A non-default subject claim. Before #546 it minted under the sub
		// namespace; that scheme's digest is recognised at startup so the
		// refusal can say why an unchanged configuration is a switch.
		scheme.sharedNamespaceDigest = subjectDigest(base)
		// And it stamps its requests, as a stable scheme does: a request
		// made under one subject claim cannot be decided under another.
		scheme.approvals.Digest = scheme.digest
	}
	// Otherwise the stamp stays zero: sub-derived and legacy Entra requests
	// were always unstamped.
	return scheme
}
