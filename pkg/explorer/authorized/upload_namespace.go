/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package authorized

import (
	"context"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Upload source URIs are routed through the uploader's namespace (#712).
//
// The browser upload route and the MCP ingest tool name a document by a
// caller-chosen filename, as UploadSourceURIPrefix + the escaped name. Before
// #712 that URI was stored as it was, so every principal's upload of a name
// was the same document. Ingesting at a URI whose source claim the caller
// cannot read is refused (writing over a hidden document is not acceptable),
// while an absent URI succeeds, so a reader could test whether a hidden
// document exists under a guessed filename.
//
// An upload name is therefore a request, not a URI. Ingest routes it, under
// the mutation lock and before anything is written, to:
//
//  1. the caller's own namespaced URI (UploadSourceURIPrefix, "u-", a digest
//     of the caller's identity, "/", the name) when anything is stored there:
//     a claim, a registration or a base document;
//  2. otherwise the un-namespaced URI of the name, the URI every upload had
//     before #712, when it is eligible: something is recorded for it and the
//     caller is authorized to ingest under every rule recorded for it (its
//     claim's rule, a pending claim's previous rule, and its current
//     registration);
//  3. otherwise the one other identity's namespaced URI of the name that is
//     stored in the base and eligible, when there is exactly one;
//  4. otherwise the caller's own namespaced URI. When two or more other
//     identities' documents of the name are eligible the name is ambiguous,
//     and none of them is written.
//
// Steps 2 and 3 only ever choose an eligible URI, and skip every other one
// exactly as if it were absent. So the route, and so the outcome, never
// depends on another identity's document the caller is not authorized to
// ingest under: for such a caller, the outcome is the one it would be if the
// name had never been uploaded, its own namespace, where only its own
// identity has written. Principals authorized under a document still share
// it by name, so relabelling (#630, #680) and shared editing keep working,
// and documents uploaded before #712 stay re-uploadable by everyone
// authorized under them.
//
// The outcome does depend on the caller's own identity's document of the name
// (step 1), whatever its rule. If someone else relabelled that document to a
// rule the caller is now outside, the caller's uploads of the name are
// refused with NotFound, as any ingest over a document the caller cannot
// read is, until a holder relabels it back or the caller regains the labels;
// the caller can upload under another name meanwhile. Step 1 does not fall
// through to the other steps for such a document: the upload would then
// silently write some other identity's document of the name, or (with
// nothing eligible) still land on the caller's own URI.
//
// Every route is reported (UploadRoute). OtherDocuments counts the eligible
// documents of the name, other than the one written, that the upload did not
// change: an upload that went to the caller's own document while another
// eligible document of the name exists (step 1), or that found the name
// ambiguous (step 4), says so instead of looking like an update of the
// document the caller may have meant. Only eligible documents are counted,
// so the report discloses nothing the caller could not have written anyway.
// To write one of them anyway, a library caller ingests at its source URI:
// an eligible URI in another identity's namespace is accepted as a target.
//
// Namespacing only when the caller cannot read the existing claim would not
// do: an absent name would then route to the un-namespaced URI and a hidden
// one to the namespace, and the two outcomes differ. Here the default, for an
// absent name and for a hidden one alike, is the namespace.
//
// A namespaced URI is accepted as an ingest's source URI in the caller's own
// namespace, and in another identity's namespace only when it is eligible; a
// hidden or absent one there is refused with one InvalidArgument, the same
// either way. Any other URI under UploadSourceURIPrefix that is not a bare
// name is refused with one InvalidArgument before anything is read. Source
// URIs outside UploadSourceURIPrefix (file://, https://, a host's own
// schemes) are not routed: no HTTP or MCP route lets a caller choose one, so
// they come from trusted ingesters (operator configuration, cmd/shoal-explore,
// library hosts), and ingesting one over a hidden document is refused with
// NotFound as before.
//
// The namespace binds the identity, not its current authority (as the
// application edge namespace does, #699): a narrowed session of the same
// identity routes to the same namespace, so it can still tell whether that
// identity's broader sessions uploaded a name it cannot read. The identity
// includes the client and acting application, so one person uploading through
// the browser and through an MCP client has two namespaces.
const UploadSourceURIPrefix = "upload://workspace/"

// uploadNamespaceSegmentPrefix starts the namespace segment of a namespaced
// upload URI. An upload name never contains "/" (the upload route escapes
// it), so an un-namespaced upload URI has exactly one segment after
// UploadSourceURIPrefix, and a namespaced one has two.
const uploadNamespaceSegmentPrefix = "u-"

// uploadNamespaceDigestLength is the length of the hex identity digest in the
// namespace segment.
const uploadNamespaceDigestLength = 32

// uploadNamespaceVersion separates the upload namespace's digest from the
// application edge namespace's digest of the same identity.
const uploadNamespaceVersion = "shoal/authorized/upload-namespace/v1"

// uploadNamespace returns the URI prefix of every upload the decision's
// identity owns.
func uploadNamespace(decision auth.Decision) string {
	return UploadSourceURIPrefix + uploadNamespaceSegmentPrefix +
		identityNamespaceDigest(uploadNamespaceVersion, decision) + "/"
}

// UploadRouteKind says which document an upload was written to.
type UploadRouteKind string

const (
	// UploadRouteOwn is the caller's own namespaced URI of the name.
	UploadRouteOwn UploadRouteKind = "own"
	// UploadRouteLegacy is the name's un-namespaced URI (before #712).
	UploadRouteLegacy UploadRouteKind = "legacy"
	// UploadRouteShared is another identity's namespaced URI of the name.
	UploadRouteShared UploadRouteKind = "shared"
)

// UploadRoute reports where an ingest under UploadSourceURIPrefix was
// written. It is zero for any other source URI.
type UploadRoute struct {
	Kind UploadRouteKind
	// SourceURI is the URI written.
	SourceURI string
	// OtherDocuments counts the documents of the name, other than the one
	// written, that the caller is authorized to ingest under. They were not
	// changed. It is zero for an explicit target URI.
	OtherDocuments int
	// Ambiguous is set when the name went to the caller's own URI because
	// two or more other identities' documents of it are eligible (step 4).
	Ambiguous bool
}

type uploadSourceKind uint8

const (
	// notUploadSource is a URI outside UploadSourceURIPrefix, stored as is.
	notUploadSource uploadSourceKind = iota
	// uploadSourceName is UploadSourceURIPrefix and a bare name, routed.
	uploadSourceName
	// uploadSourceOwn is a URI in the caller's own namespace, stored as is.
	uploadSourceOwn
	// uploadSourceTarget is a well-formed URI in another identity's
	// namespace, written only when eligible.
	uploadSourceTarget
)

// classifyUploadSourceURI says how Ingest treats sourceURI. For a bare
// upload name it returns the name (the URI's segment after
// UploadSourceURIPrefix). It reads nothing: a refusal depends only on the
// caller's identity and the URI it sent.
func classifyUploadSourceURI(
	decision auth.Decision, sourceURI string,
) (uploadSourceKind, string, error) {
	if !strings.HasPrefix(sourceURI, UploadSourceURIPrefix) {
		return notUploadSource, "", nil
	}
	rest := sourceURI[len(UploadSourceURIPrefix):]
	if rest != "" && !strings.Contains(rest, "/") {
		return uploadSourceName, rest, nil
	}
	own := uploadNamespace(decision)
	if len(sourceURI) > len(own) && strings.HasPrefix(sourceURI, own) &&
		!strings.Contains(sourceURI[len(own):], "/") {
		return uploadSourceOwn, "", nil
	}
	if name, ok := namespacedUploadName(sourceURI); ok {
		return uploadSourceTarget, name, nil
	}
	return notUploadSource, "", uploadNamespaceRefused()
}

func uploadNamespaceRefused() error {
	return shoal.NewError(shoal.ErrorInvalidArgument,
		"upload source URI is neither an upload name nor in the caller's "+
			"upload namespace; ingest "+UploadSourceURIPrefix+"<name>")
}

// uploadTargetRefused is the one refusal of a target URI in another
// identity's namespace that is not eligible, whether a document the caller
// is outside is stored there or nothing is.
func uploadTargetRefused() error {
	return shoal.NewError(shoal.ErrorInvalidArgument,
		"upload source URI is in another uploader's namespace and is not a "+
			"document the caller may upload to; ingest "+
			UploadSourceURIPrefix+"<name>")
}

// namespacedUploadName returns the name of a well-formed URI in some
// identity's upload namespace: UploadSourceURIPrefix, "u-", 32 lowercase hex
// digits, "/", one non-empty segment.
func namespacedUploadName(sourceURI string) (string, bool) {
	if !strings.HasPrefix(sourceURI, UploadSourceURIPrefix+uploadNamespaceSegmentPrefix) {
		return "", false
	}
	rest := sourceURI[len(UploadSourceURIPrefix)+len(uploadNamespaceSegmentPrefix):]
	if len(rest) <= uploadNamespaceDigestLength+1 ||
		rest[uploadNamespaceDigestLength] != '/' ||
		strings.Contains(rest[uploadNamespaceDigestLength+1:], "/") {
		return "", false
	}
	for _, digit := range rest[:uploadNamespaceDigestLength] {
		if (digit < '0' || digit > '9') && (digit < 'a' || digit > 'f') {
			return "", false
		}
	}
	return rest[uploadNamespaceDigestLength+1:], true
}

// routeUploadSource returns where an ingest of the upload name writes, as the
// comment on UploadSourceURIPrefix describes. It runs under the mutation
// lock, so the route cannot change before the claim is taken.
func (c *Client) routeUploadSource(
	ctx context.Context,
	decision auth.Decision,
	now time.Time,
	name string,
) (UploadRoute, error) {
	own := uploadNamespace(decision) + name
	summaries, err := c.base.Documents(ctx)
	if err != nil {
		return UploadRoute{}, err
	}
	stored, err := c.sourceURIStored(ctx, own, summaries)
	if err != nil {
		return UploadRoute{}, err
	}
	legacy := UploadSourceURIPrefix + name
	legacyEligible, err := c.uploadRouteEligible(ctx, decision, now, legacy)
	if err != nil {
		return UploadRoute{}, err
	}
	var shared []string
	seen := make(map[string]struct{})
	for _, summary := range summaries {
		uri := summary.SourceURI
		if _, done := seen[uri]; done || uri == own {
			continue
		}
		if other, ok := namespacedUploadName(uri); !ok || other != name {
			continue
		}
		seen[uri] = struct{}{}
		eligible, err := c.uploadRouteEligible(ctx, decision, now, uri)
		if err != nil {
			return UploadRoute{}, err
		}
		if eligible {
			shared = append(shared, uri)
		}
	}
	eligible := len(shared)
	if legacyEligible {
		eligible++
	}
	switch {
	case stored:
		return UploadRoute{Kind: UploadRouteOwn, SourceURI: own, OtherDocuments: eligible}, nil
	case legacyEligible:
		return UploadRoute{Kind: UploadRouteLegacy, SourceURI: legacy,
			OtherDocuments: eligible - 1}, nil
	case len(shared) == 1:
		return UploadRoute{Kind: UploadRouteShared, SourceURI: shared[0]}, nil
	}
	return UploadRoute{Kind: UploadRouteOwn, SourceURI: own,
		OtherDocuments: eligible, Ambiguous: eligible > 1}, nil
}

// routeUploadTarget admits a target URI in another identity's namespace only
// when it is eligible, and otherwise refuses it the same way whether a
// document the caller is outside is stored there or nothing is.
func (c *Client) routeUploadTarget(
	ctx context.Context,
	decision auth.Decision,
	now time.Time,
	sourceURI string,
) (UploadRoute, error) {
	eligible, err := c.uploadRouteEligible(ctx, decision, now, sourceURI)
	if err != nil {
		return UploadRoute{}, err
	}
	if !eligible {
		return UploadRoute{}, uploadTargetRefused()
	}
	return UploadRoute{Kind: UploadRouteShared, SourceURI: sourceURI}, nil
}

// sourceURIStored reports whether a claim, a registration or a base document
// exists for sourceURI. It is asked only of the caller's own namespace.
func (c *Client) sourceURIStored(
	ctx context.Context,
	sourceURI string,
	summaries []explorer.DocumentSummary,
) (bool, error) {
	for _, summary := range summaries {
		if summary.SourceURI == sourceURI {
			return true, nil
		}
	}
	if _, claimed, err := c.policyStore.SourceClaim(ctx, sourceURI); err != nil {
		return false, policyCatalogReadError(ctx, err)
	} else if claimed {
		return true, nil
	}
	_, registered, err := c.policyStore.CurrentRevision(
		ctx, explorer.SourceDocumentID(sourceURI))
	if err != nil {
		return false, policyCatalogReadError(ctx, err)
	}
	return registered, nil
}

// uploadRouteEligible reports whether an upload may route to sourceURI:
// something is recorded for it and the caller is authorized to ingest under
// every rule recorded for it. Anything else, including a rule that fails to
// evaluate, is skipped as if absent, so the route never depends on a rule the
// caller is outside.
func (c *Client) uploadRouteEligible(
	ctx context.Context,
	decision auth.Decision,
	now time.Time,
	sourceURI string,
) (bool, error) {
	claim, claimed, err := c.policyStore.SourceClaim(ctx, sourceURI)
	if err != nil {
		return false, policyCatalogReadError(ctx, err)
	}
	registration, registered, err := c.policyStore.CurrentRevision(
		ctx, explorer.SourceDocumentID(sourceURI))
	if err != nil {
		return false, policyCatalogReadError(ctx, err)
	}
	var rules []AccessRule
	if claimed {
		rules = append(rules, claim.Rule)
		if claim.Pending && claim.PreviousRule != nil {
			rules = append(rules, *claim.PreviousRule)
		}
	}
	if registered {
		rules = append(rules, registration.Rule)
	}
	if len(rules) == 0 {
		return false, nil
	}
	for _, rule := range rules {
		allowed, err := ruleAllows(rule, decision, auth.OperationIngest, now)
		if err != nil || !allowed {
			return false, nil
		}
	}
	return true, nil
}

// IngestUpload is Ingest, and also reports where an upload under
// UploadSourceURIPrefix was written (see UploadRoute). The route is zero
// when the ingest fails, and for any other source URI.
func (c *Client) IngestUpload(
	ctx context.Context,
	source explorer.Source,
) (explorer.IngestResult, UploadRoute, error) {
	var route UploadRoute
	result, err := c.ingest(ctx, source, true, &route)
	if err != nil {
		return explorer.IngestResult{}, UploadRoute{}, err
	}
	return result, route, nil
}

// OwnUploadSourceURI returns the source URI in the authenticated caller's
// upload namespace for the upload name (the segment after
// UploadSourceURIPrefix, escaped as the upload route escapes a filename). An
// ingest at it writes the caller's own document of the name, whatever else is
// stored under the name; an ingest of UploadSourceURIPrefix + name writes
// there unless the caller has no document of the name and is authorized under
// another one (see UploadSourceURIPrefix). The result depends only on the
// caller's identity and the name, and is the same on every call and across
// restarts.
func (c *Client) OwnUploadSourceURI(
	ctx context.Context,
	name string,
) (string, error) {
	if name == "" || strings.Contains(name, "/") {
		return "", shoal.NewError(shoal.ErrorInvalidArgument,
			"upload name must be one non-empty URI segment")
	}
	decision, guard, _, err := c.begin(ctx, auth.OperationIngest)
	if err != nil {
		return "", err
	}
	if err := guard.Check(ctx); err != nil {
		return "", err
	}
	return uploadNamespace(decision) + name, nil
}
