// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"unicode/utf8"

	"github.com/golang-jwt/jwt/v5"
	"github.com/phrocker/shoal-oss/internal/strictjson"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Label grants (#570, PR3) are an operator file, like the approver mapping,
// and for the same reason: ATPL is written by registrants, and a registrant
// must never be able to grant itself clearance.
//
// A label grant adds visibility and nothing else. It appends label policy IDs
// to the PermittedPolicyIDs of a token that a workspace role mapping
// (-oidc-reader-values, -oidc-contributor-values, -oidc-fleet-values) has
// already minted; it never adds an operation, never adds a source, and never
// makes an unmapped token mapped. A labelled document conjoins one policy per
// (source, label) into its AccessRule, so holding the source alone opens
// nothing labelled, and an ungranted label is visible to nobody — the
// principal that ingested it included.
//
// Approver tokens get no labels: the approver branch mints approve and
// nothing else, and approve reads nothing. Executor-bound decisions (#391)
// get none either: the executor branch mints execute alone, under the
// workspace policy and no label policy, whatever the token's claims say.
const (
	labelGrantsVersion = "shoal.label-grants/v1"
	// labelGrantsMaxBytes bounds the file read at startup.
	labelGrantsMaxBytes = 256 << 10
	// labelGrantsMaxValues bounds the claim values the file maps.
	labelGrantsMaxValues = 256
	// labelGrantsMaxClaimValues bounds max_values, as the approver mapping
	// does.
	labelGrantsMaxClaimValues = approverMappingMaxValues
	// labelGrantsDigestTag domain-separates the grant digest.
	labelGrantsDigestTag = "shoal.label-grants/v1-digest"
)

// labelGrantsFile is the strict on-disk shape. Unknown fields, duplicate
// keys and keys matching a field only up to case (at any depth), and
// trailing data, are refused.
type labelGrantsFile struct {
	Version   string                      `json:"version"`
	Issuer    string                      `json:"issuer"`
	Claim     []string                    `json:"claim"`
	MaxValues int                         `json:"max_values"`
	Grants    map[string][]labelGrantJSON `json:"grants"`
}

// labelGrantJSON is one (source, label) pair. The source is the configured
// source ID, byte for byte; the label is a free-form visibility label exactly
// as documents carry it.
type labelGrantJSON struct {
	Source *string `json:"source"`
	Label  *string `json:"label"`
}

// labelGrants is the validated, in-force grant file.
type labelGrants struct {
	issuer    string
	claim     []string
	maxValues int
	// grants maps an exact claim value to its sorted label policy IDs.
	grants map[string][][]byte
	total  int
	digest auth.Digest
}

func labelGrantsInvalid(reason string) error {
	return shoal.NewError(
		shoal.ErrorInvalidArgument,
		"-oidc-label-grants-file is invalid: "+reason)
}

// loadLabelGrants reads and validates the operator file. issuer is the
// trimmed -oidc-issuer and sources the source IDs this command configures.
func loadLabelGrants(
	path string, issuer string, sources [][]byte,
) (*labelGrants, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read -oidc-label-grants-file: %w", err)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, labelGrantsMaxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read -oidc-label-grants-file: %w", err)
	}
	if len(raw) > labelGrantsMaxBytes {
		return nil, labelGrantsInvalid("the file exceeds its size bound")
	}
	return parseLabelGrants(raw, issuer, sources, authorized.LabelPolicyID)
}

// parseLabelGrants validates the file. mint is authorized.LabelPolicyID in
// production; a test injects a misbehaving constructor to show that the
// reserved-namespace and canonical-ID refusals hold on their own.
func parseLabelGrants(
	raw []byte, issuer string, sources [][]byte,
	mint func([]byte, string) ([]byte, error),
) (*labelGrants, error) {
	if !utf8.Valid(raw) {
		return nil, labelGrantsInvalid("the file is not UTF-8")
	}
	// strictjson, not encoding/json: the latter matches field names
	// case-insensitively, so {"label": "secret", "LABEL": "other"} would be
	// read as a grant of "other" while a reviewer reads "secret".
	var file labelGrantsFile
	if err := strictjson.Decode(raw, &file); err != nil {
		return nil, labelGrantsInvalid(err.Error())
	}
	if file.Version != labelGrantsVersion {
		return nil, labelGrantsInvalid("version must be " + labelGrantsVersion)
	}
	if file.Issuer == "" || file.Issuer != issuer {
		return nil, labelGrantsInvalid(
			"issuer must equal -oidc-issuer byte for byte")
	}
	if len(file.Claim) == 0 || len(file.Claim) > auth.MaxGrantClaimPathSegments {
		return nil, labelGrantsInvalid(fmt.Sprintf(
			"claim must be a list of 1 to %d path segments",
			auth.MaxGrantClaimPathSegments))
	}
	for _, segment := range file.Claim {
		if !approverText(segment) {
			return nil, labelGrantsInvalid("claim has an invalid segment")
		}
	}
	if file.MaxValues < 1 || file.MaxValues > labelGrantsMaxClaimValues {
		return nil, labelGrantsInvalid(fmt.Sprintf(
			"max_values must be between 1 and %d", labelGrantsMaxClaimValues))
	}
	if len(file.Grants) == 0 {
		return nil, labelGrantsInvalid("grants must not be empty")
	}
	if len(file.Grants) > labelGrantsMaxValues {
		return nil, labelGrantsInvalid("grants map more claim values than its bound")
	}
	grants := &labelGrants{
		issuer:    file.Issuer,
		claim:     append([]string(nil), file.Claim...),
		maxValues: file.MaxValues,
		grants:    make(map[string][][]byte, len(file.Grants)),
	}
	for value, pairs := range file.Grants {
		if !approverText(value) {
			return nil, labelGrantsInvalid(
				"grant claim values must be non-empty, with no surrounding " +
					"whitespace and no control characters")
		}
		if len(pairs) == 0 {
			return nil, labelGrantsInvalid(
				fmt.Sprintf("claim value %q grants nothing", value))
		}
		grants.total += len(pairs)
		if grants.total > authorized.MaxLabelGrants {
			return nil, labelGrantsInvalid(fmt.Sprintf(
				"the file holds more than %d grants", authorized.MaxLabelGrants))
		}
		ids := make([][]byte, 0, len(pairs))
		seen := make(map[string]struct{}, len(pairs))
		for _, pair := range pairs {
			if pair.Source == nil || pair.Label == nil {
				return nil, labelGrantsInvalid(
					"every grant needs a source and a label")
			}
			id, err := admitFileLabelGrant(*pair.Source, *pair.Label, sources, mint)
			if err != nil {
				return nil, labelGrantsInvalid(fmt.Sprintf(
					"claim value %q: %v", value, err))
			}
			if _, duplicate := seen[string(id)]; duplicate {
				return nil, labelGrantsInvalid(fmt.Sprintf(
					"claim value %q grants (%q, %q) twice",
					value, *pair.Source, *pair.Label))
			}
			seen[string(id)] = struct{}{}
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return bytes.Compare(ids[i], ids[j]) < 0 })
		grants.grants[value] = ids
	}
	grants.digest = grants.computeDigest()
	return grants, nil
}

// admitFileLabelGrant validates one pair: a configured source, a valid label,
// and an ID that is neither reserved nor non-canonical. Each refusal says
// which, so an operator can fix the file from the startup error alone.
func admitFileLabelGrant(
	source, label string, sources [][]byte,
	mint func([]byte, string) ([]byte, error),
) ([]byte, error) {
	known := false
	for _, configured := range sources {
		if bytes.Equal(configured, []byte(source)) {
			known = true
			break
		}
	}
	if !known {
		return nil, fmt.Errorf("source %q is not a configured source", source)
	}
	if err := interaction.ValidateLabel(label); err != nil {
		return nil, fmt.Errorf("label %q is not a valid label: %w", label, err)
	}
	id, err := mint([]byte(source), label)
	if err != nil {
		return nil, fmt.Errorf("label %q on source %q: %w", label, source, err)
	}
	return authorized.AdmitLabelPolicyID(id, []byte(source), label)
}

// computeDigest is a canonical, length-framed digest of everything that
// decides who holds which label. Values and IDs are sorted, so reordering the
// file does not move it; any change to what it grants does.
//
// It is provenance, not authority: it feeds neither the policy generation nor
// the authorization fingerprint. A decision's fingerprint already hashes its
// PermittedPolicyIDs, so a grant change moves the fingerprint of exactly the
// principals whose grants changed and of nobody else; folding the whole
// file's digest in would invalidate every pinned decision on any edit, and
// bumping the generation would do the same to every principal in the domain.
// The digest is printed at startup so an operator can tell which grant file
// every replica is serving.
func (g *labelGrants) computeDigest() auth.Digest {
	var buffer bytes.Buffer
	text := func(value []byte) {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		buffer.Write(length[:])
		buffer.Write(value)
	}
	count := func(n int) { text([]byte(fmt.Sprint(n))) }
	text([]byte(labelGrantsVersion))
	text([]byte(g.issuer))
	count(len(g.claim))
	for _, segment := range g.claim {
		text([]byte(segment))
	}
	count(g.maxValues)
	values := make([]string, 0, len(g.grants))
	for value := range g.grants {
		values = append(values, value)
	}
	sort.Strings(values)
	count(len(values))
	for _, value := range values {
		text([]byte(value))
		count(len(g.grants[value]))
		for _, id := range g.grants[value] {
			text(id)
		}
	}
	return auth.DigestBytes(labelGrantsDigestTag, buffer.Bytes())
}

// policies returns the label policy IDs the token's claim values are granted.
// The claim is resolved like the approver mapping's: a path of literal
// segments, a string or an array of strings, compared byte for byte, at most
// max_values values. An absent, null or empty claim grants nothing; any other
// shape refuses the token, since a claim this file cannot read is not one it
// should guess at.
func (g *labelGrants) policies(claims jwt.MapClaims) ([][]byte, error) {
	if g == nil {
		return nil, nil
	}
	values, err := approverClaimStrings(claims, g.claim, g.maxValues)
	if errors.Is(err, errMissingMappedClaim) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids [][]byte
	seen := make(map[string]struct{})
	for _, value := range values {
		for _, id := range g.grants[value] {
			if _, duplicate := seen[string(id)]; duplicate {
				continue
			}
			seen[string(id)] = struct{}{}
			ids = append(ids, append([]byte(nil), id...))
		}
	}
	return ids, nil
}

// labelGrantsDigest returns the in-force grant digest, or the zero digest
// when no grant file is configured.
func (a *oidcAuthenticator) labelGrantsDigest() (auth.Digest, int) {
	if a == nil || a.labelGrants == nil {
		return auth.Digest{}, 0
	}
	return a.labelGrants.digest, a.labelGrants.total
}
