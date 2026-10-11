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

// Command shoal-explore-web serves the optional local Explorer workspace.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/phrocker/shoal-oss/internal/executorattest"
	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/internal/explorerfleet"
	"github.com/phrocker/shoal-oss/internal/explorerfleetevents"
	"github.com/phrocker/shoal-oss/internal/healthsurface"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/transaction"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/mcp"
	"github.com/phrocker/shoal-oss/pkg/explorer/teamoverview"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/explorer/workspace"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/model"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// transitionSkipReporter is the part of the dispatch service the health
// surface reads. Declared here rather than widened into
// webapi.FleetDispatchProvider, which is the API boundary and has no business
// carrying an observability accessor.
type transitionSkipReporter interface {
	SkippedTransitionPublications() uint64
}

// dispatchMetrics renders the dispatch counters the health port may carry, or
// nil when there is no dispatch service to read — a nil writer leaves the
// surface exactly as it was, with /metrics a 404.
//
// One counter, and deliberately not a gauge over pending work. The health port
// is unauthenticated, so it carries counters of this process's own behaviour
// and nothing that tracks tenant workload: an oldest-pending age or a queue
// depth names no object and still reports how much work a tenant has
// outstanding (#642). The durable pending signal waits for #647 and belongs
// behind authentication.
func dispatchMetrics(provider webapi.FleetDispatchProvider) healthsurface.MetricsWriter {
	reporter, ok := provider.(transitionSkipReporter)
	if !ok {
		return nil
	}
	return func(b *strings.Builder) {
		fmt.Fprintf(b,
			"# HELP shoal_fleet_transition_publications_skipped_total "+
				"Pending transition rows left in place because the "+
				"reconciling caller was not entitled to publish them.\n"+
				"# TYPE shoal_fleet_transition_publications_skipped_total counter\n"+
				"shoal_fleet_transition_publications_skipped_total %d\n",
			reporter.SkippedTransitionPublications())
	}
}

func main() {
	ctx, stop := signal.NotifyContext(
		context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "shoal-explore-web: %v\n", err)
		os.Exit(1)
	}
}

// listenTCP opens the workspace listener. It is a variable so tests can prove
// that a refused address is never bound, and that a listener whose resolved
// address is wider than the requested one is still refused and closed.
var listenTCP = net.Listen

func run(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("shoal-explore-web", flag.ContinueOnError)
	backend := flags.String("backend", "embedded", "Explorer backend: embedded or remote")
	stateDir := flags.String(
		"state-dir", "",
		"Recommended workspace state root. The corpus (including workspace "+
			"settings) and durable policy catalog are created as corpus/ and policy/ "+
			"inside it, so mounting "+
			"this one directory as a volume persists everything a restart "+
			"needs. Overrides -data when set",
	)
	data := flags.String(
		"data", ".shoal/explorer",
		"Legacy Explorer corpus directory (used when -state-dir is unset). The "+
			"workspace settings share this corpus engine; the durable policy catalog "+
			"is placed in a sibling directory and both must be persisted across restart",
	)
	policyDirFlag := flags.String(
		"policy-dir", "",
		"Durable policy catalog directory. Overrides the location derived from "+
			"-state-dir or -data; must be persisted for registrations to "+
			"survive a restart",
	)
	listen := flags.String("listen", "127.0.0.1:8080", "HTTP listen address")
	healthAddress := flags.String(
		"health-address", os.Getenv("SHOAL_HEALTH_ADDRESS"),
		"Optional separate listen address serving GET /healthz and GET /readyz "+
			"for orchestrator probes. Empty disables it. This is a second "+
			"listener on purpose: the workspace listener refuses any request "+
			"whose Host is not an exactly configured authority, which a probe "+
			"addressing the pod by its runtime-assigned IP can never satisfy. "+
			"The surface answers with a status code and a fixed string and "+
			"reads no corpus, policy or identity state, so it is safe to bind "+
			"where the workspace port is not. /readyz reports not-ready until "+
			"the workspace is serving and again as soon as shutdown begins, so "+
			"a draining instance leaves Service endpoints before it stops "+
			"accepting; /healthz stays ok throughout a drain. Environment "+
			"fallback SHOAL_HEALTH_ADDRESS",
	)
	allowedHost := flags.String(
		"allowed-host", "",
		"Comma-separated exact-match allow-list of external authorities (host "+
			"or host:port) an inbound request's Host/:authority must match; the "+
			"hostname compares case-insensitively and the port exactly. No "+
			"wildcard or suffix matching, and X-Forwarded-Host is never "+
			"trusted. Required for a non-loopback or wildcard -listen, whose "+
			"resolved socket address real client Host headers never carry; a "+
			"public bind refuses every request until this names the external "+
			"authority. Defaults to the resolved listen address; environment "+
			"fallback SHOAL_ALLOWED_HOST",
	)
	remote := flags.String("remote", "", "Remote Explorer web API URL for -backend remote")
	embeddingProvider := flags.String(
		"embedding-provider", "",
		"Optional embedded vector provider: fake, lexical, ollama, openai, or voyage",
	)
	embeddingModel := flags.String(
		"embedding-model", "",
		"Embedding model name for -embedding-provider",
	)
	embeddingBaseURL := flags.String(
		"embedding-base-url", "",
		"Embedding provider base URL for ollama/openai/voyage",
	)
	embeddingAPIKeyEnv := flags.String(
		"embedding-api-key-env", "OPENAI_API_KEY",
		"Environment variable read at request time for openai/voyage credentials",
	)
	embeddingDimensions := flags.Int(
		"embedding-dimensions", 0,
		"Embedding dimensions; required for ollama/openai, zero uses fake default",
	)
	chatProvider := flags.String(
		"chat-provider", firstNonEmpty(os.Getenv("SHOAL_CHAT_PROVIDER"), "ollama"),
		"Grounded chat model provider: ollama or openai-compatible",
	)
	chatModel := flags.String(
		"chat-model", os.Getenv("SHOAL_CHAT_MODEL"),
		"Grounded chat model name",
	)
	chatBaseURL := flags.String(
		"chat-base-url",
		firstNonEmpty(os.Getenv("SHOAL_CHAT_BASE_URL"), model.DefaultOllamaBaseURL),
		"Grounded chat provider base URL",
	)
	chatAPIKeyEnv := flags.String(
		"chat-api-key-env", "SHOAL_OPENAI_API_KEY",
		"Environment variable containing the OpenAI-compatible chat credential",
	)
	chatOrganization := flags.String(
		"chat-organization", os.Getenv("SHOAL_OPENAI_ORGANIZATION"),
		"Optional OpenAI-compatible organization header",
	)
	chatProject := flags.String(
		"chat-project", os.Getenv("SHOAL_OPENAI_PROJECT"),
		"Optional OpenAI-compatible project header",
	)
	fleetExecutorRefs := flags.String(
		"fleet-executor-refs", os.Getenv("SHOAL_FLEET_EXECUTOR_REFS"),
		"Comma-separated opaque executor references accepted by the durable "+
			"agent registry; empty keeps registration fail-closed",
	)
	fleetAskExecutorRef := flags.String(
		"fleet-ask-executor-ref", os.Getenv("SHOAL_FLEET_ASK_EXECUTOR_REF"),
		"Executor reference bound to the built-in grounded-reasoning "+
			"executor; must also appear in -fleet-executor-refs and requires "+
			"a configured chat provider. An agent principal invoking this "+
			"executor needs the retrieve grant in addition to invoke, because "+
			"the reasoning path authorizes retrieval on its own terms",
	)
	fleetExternalExecutorRefs := flags.String(
		"fleet-external-executor-refs",
		os.Getenv("SHOAL_FLEET_EXTERNAL_EXECUTOR_REFS"),
		"Comma-separated executor references bound with a ceiling of "+
			"{external} and no floor, so a descriptor may register an action "+
			"whose consequences land outside Shoal. Each must also appear in "+
			"-fleet-executor-refs. Nothing is executed in process against "+
			"these references: they implement no action execution, so work "+
			"reaches them over the dispatch queue and the completion report "+
			"is what Shoal records. Empty by default, and no other setting "+
			"produces this ceiling — a reference that is merely allowlisted "+
			"still permits nothing at all",
	)
	fleetExternalEgressExecutorRefs := flags.String(
		"fleet-external-egress-executor-refs",
		os.Getenv("SHOAL_FLEET_EXTERNAL_EGRESS_EXECUTOR_REFS"),
		"Comma-separated executor references bound with a ceiling of "+
			"{egresses-content, external} and no floor, for an external "+
			"operation that also transmits corpus content off this host. "+
			"Otherwise identical to -fleet-external-executor-refs, which a "+
			"reference may not also appear in: transmission is a separate "+
			"declaration so that it is never acquired by naming a reference "+
			"for mutation alone",
	)
	fleetExecutorAttestation := flags.String(
		"fleet-executor-attestation",
		os.Getenv("SHOAL_FLEET_EXECUTOR_ATTESTATION"),
		"Path to the executor attestation trust file (docs/executor-"+
			"attestation.md): per executor reference, the operator verifiers "+
			"and pinned image digests an executor's attestation must match. "+
			"Each reference must also be bound by -fleet-external-executor-refs "+
			"or -fleet-external-egress-executor-refs. Required before a "+
			"descriptor may register an action that requires attestation; "+
			"empty configures no trust root, so such registrations are refused",
	)
	concealWithholding := flags.Bool(
		"conceal-withholding",
		concealWithholdingDefault(),
		"Remove the withheld-document counts from responses. Off by default: "+
			"the counts are emitted deliberately so a short answer is never "+
			"silently mistaken for an empty corpus. The counts are "+
			"corpus-wide and identical for every query, so they disclose how "+
			"much a caller cannot read and signal when that changes, not "+
			"which terms match. Turn this on for a compartmented deployment "+
			"that declines to disclose either. Audit records both counts "+
			"either way",
	)
	developmentAuth := flags.Bool(
		"dev-auth", false,
		"Authenticate every request as a fixed development principal; "+
			"refused unless the resolved listen address is loopback-only",
	)
	developmentLabels := flags.String(
		"dev-auth-labels", "",
		"Comma-separated label grants for the -dev-auth principal, each "+
			"<source>=<label> with the source exactly as configured ("+
			string(workspaceSourceID)+"), for example "+
			string(workspaceSourceID)+"=secret. Without it the development "+
			"principal holds no label and can neither ingest nor read "+
			"labelled content (#570). Requires -dev-auth",
	)
	mosaicBudget := flags.Uint(
		"mosaic-budget", 0,
		"Sensitivity-domain co-occurrence budget defending against the mosaic "+
			"effect: the maximum number of distinct sensitivity domains one "+
			"identity may observe together within -mosaic-window before further "+
			"cross-domain results are withheld. Zero disables the control",
	)
	mosaicWindow := flags.Duration(
		"mosaic-window", time.Hour,
		"Window over which the -mosaic-budget distinct-domain count accumulates "+
			"for an identity before it resets",
	)
	oidcIssuer := flags.String(
		"oidc-issuer", "",
		"Exact OIDC token issuer. Required for OIDC authentication; "+
			"environment fallback SHOAL_OIDC_ISSUER",
	)
	oidcDiscoveryURL := flags.String(
		"oidc-discovery-url", "",
		"OIDC discovery document URL. Defaults to <issuer>/.well-known/"+
			"openid-configuration; environment fallback SHOAL_OIDC_DISCOVERY_URL",
	)
	oidcAudiences := flags.String(
		"oidc-audience", "",
		"Comma-separated token audiences; at least one exact match is required. "+
			"Environment fallback SHOAL_OIDC_AUDIENCE",
	)
	oidcJWKSURI := flags.String(
		"oidc-jwks-uri", "",
		"JWKS URI override. When empty, jwks_uri is read from OIDC discovery; "+
			"environment fallback SHOAL_OIDC_JWKS_URI",
	)
	oidcAllowedAlgs := flags.String(
		"oidc-allowed-algs", "",
		"Comma-separated allowlist of asymmetric signing algorithms "+
			"(for example RS256,ES256). Defaults to RS256. HS* and none are "+
			"always rejected",
	)
	oidcClockSkew := flags.Duration(
		"oidc-clock-skew", 0,
		"Tolerated clock skew for token expiry/not-before checks; defaults to "+
			"60s and is capped at 5m",
	)
	oidcSubjectClaim := flags.String(
		"oidc-subject-claim", "",
		"Top-level claim used as the subject identity. Defaults to sub; "+
			"environment fallback SHOAL_OIDC_SUBJECT_CLAIM",
	)
	oidcActorClaim := flags.String(
		"oidc-actor-claim", "",
		"Optional required claim mapped to the decision actor; environment "+
			"fallback SHOAL_OIDC_ACTOR_CLAIM",
	)
	oidcClientIDClaim := flags.String(
		"oidc-client-id-claim", "",
		"Optional required claim mapped to the decision client identity; "+
			"environment fallback SHOAL_OIDC_CLIENT_ID_CLAIM",
	)
	oidcDelegationClaim := flags.String(
		"oidc-delegation-claim", "",
		"Optional required string or string-array claim mapped in order to the "+
			"decision delegation chain; environment fallback "+
			"SHOAL_OIDC_DELEGATION_CLAIM",
	)
	oidcAuthorizationClaim := flags.String(
		"oidc-authorization-claim", "",
		"Required string or string-array claim whose values are mapped to "+
			"workspace authority; environment fallback "+
			"SHOAL_OIDC_AUTHORIZATION_CLAIM",
	)
	oidcReaderValues := flags.String(
		"oidc-reader-values", "",
		"Comma-separated authorization-claim values granted read-only "+
			"workspace access; environment fallback SHOAL_OIDC_READER_VALUES",
	)
	oidcContributorValues := flags.String(
		"oidc-contributor-values", "",
		"Comma-separated authorization-claim values granted read and ingest "+
			"access; environment fallback SHOAL_OIDC_CONTRIBUTOR_VALUES",
	)
	oidcFleetValues := flags.String(
		"oidc-fleet-values", "",
		"Comma-separated authorization-claim values granted Fleet control-plane "+
			"access; environment fallback SHOAL_OIDC_FLEET_VALUES",
	)
	oidcBrowserClientID := flags.String(
		"oidc-browser-client-id", "",
		"Optional public-client ID enabling browser Authorization Code + PKCE; "+
			"environment fallback SHOAL_OIDC_BROWSER_CLIENT_ID",
	)
	oidcBrowserScope := flags.String(
		"oidc-browser-scope", "",
		"Space-delimited scope requested by browser login; required with "+
			"-oidc-browser-client-id; environment fallback "+
			"SHOAL_OIDC_BROWSER_SCOPE",
	)
	oidcAuthorizationEndpoint := flags.String(
		"oidc-authorization-endpoint", "",
		"Authorization endpoint override for browser login; otherwise read "+
			"from discovery. Environment fallback "+
			"SHOAL_OIDC_AUTHORIZATION_ENDPOINT",
	)
	oidcApproverMappingFile := flags.String(
		"oidc-approver-mapping-file", "",
		"Operator file (shoal.approvers/v1) mapping OIDC humans to the "+
			"approver role on an audience of its own; without it no token "+
			"may approve. Environment fallback SHOAL_OIDC_APPROVER_MAPPING_FILE")
	oidcIdentityClaim := flags.String(
		"oidc-identity-claim", "",
		"Stable identity claim (#526) as a JSON array of path segments, such "+
			"as '[\"oid\"]'. Requesters and approvers are then named "+
			"oidcid:<iss>#<tag>#<value> on both branches; the approver mapping must "+
			"restate it as identity_claim. Environment fallback "+
			"SHOAL_OIDC_IDENTITY_CLAIM")
	oidcLabelGrantsFile := flags.String(
		"oidc-label-grants-file", "",
		"Operator file (shoal.label-grants/v1) granting free-form visibility "+
			"labels, per source, to OIDC claim values (#570). A grant adds "+
			"visibility to a token a role mapping already grants, never an "+
			"operation; without it no token holds any label, and labelled "+
			"content is visible to nobody. Environment fallback "+
			"SHOAL_OIDC_LABEL_GRANTS_FILE")
	oidcExecutorMappingFile := flags.String(
		"oidc-executor-mapping-file", "",
		"Operator file (shoal.executors/v1) mapping service credentials of "+
			"an issuer it names (such as the cluster's ServiceAccount issuer) "+
			"to one executor reference each, on an audience of its own (#391). "+
			"It is the only source of the execute operation; without it no "+
			"token may pull, claim or complete queued work. Environment "+
			"fallback SHOAL_OIDC_EXECUTOR_MAPPING_FILE")
	oidcIdentitySchemeMigrate := flags.String(
		"oidc-identity-scheme-migrate", "",
		"One-shot identity scheme switch: the digest (64 hex digits, as the "+
			"startup refusal prints it) of the scheme recorded for -oidc-issuer "+
			"that this rollout replaces. Startup proceeds only if the recorded "+
			"scheme is that one, or already this replica's. Without it a "+
			"replica whose scheme differs from the recorded one refuses to start")
	oidcTokenEndpoint := flags.String(
		"oidc-token-endpoint", "",
		"Token endpoint override for browser login; otherwise read from "+
			"discovery. Environment fallback SHOAL_OIDC_TOKEN_ENDPOINT",
	)
	entraTenant := flags.String(
		"entra-tenant", "",
		"Deprecated compatibility alias for the former Entra tenant setting; "+
			"environment fallback SHOAL_ENTRA_TENANT",
	)
	entraIssuer := flags.String(
		"entra-issuer", "",
		"Deprecated compatibility alias for -oidc-issuer; environment fallback "+
			"SHOAL_ENTRA_ISSUER",
	)
	entraClientID := flags.String(
		"entra-client-id", "",
		"Deprecated compatibility alias for the OIDC audience and browser "+
			"client ID; environment fallback SHOAL_ENTRA_CLIENT_ID",
	)
	entraJWKSURI := flags.String(
		"entra-jwks-uri", "",
		"Deprecated compatibility alias for -oidc-jwks-uri; environment "+
			"fallback SHOAL_ENTRA_JWKS_URI",
	)
	entraAllowedAlgs := flags.String(
		"entra-allowed-algs", "",
		"Deprecated compatibility alias for -oidc-allowed-algs",
	)
	entraClockSkew := flags.Duration(
		"entra-clock-skew", 0,
		"Deprecated compatibility alias for -oidc-clock-skew",
	)
	entraReaderRoles := flags.String(
		"entra-reader-roles", "",
		"Deprecated compatibility alias mapping the roles claim to reader "+
			"authority; environment fallback SHOAL_ENTRA_READER_ROLES",
	)
	entraContributorRoles := flags.String(
		"entra-contributor-roles", "",
		"Deprecated compatibility alias mapping the roles claim to contributor "+
			"authority; environment fallback SHOAL_ENTRA_CONTRIBUTOR_ROLES",
	)
	entraScope := flags.String(
		"entra-scope", "",
		"Deprecated browser-scope compatibility alias; environment fallback "+
			"SHOAL_ENTRA_SCOPE",
	)
	ontologyFile := flags.String(
		"ontology-file", "",
		"Optional JSON file containing the active ontology schema, version, "+
			"concepts, relationships, properties, and constraints to describe "+
			"read-only at /api/v1/ontology; environment fallback "+
			"SHOAL_ONTOLOGY_FILE",
	)
	listUntranslatable := flags.Bool(
		"list-untranslatable-labels", false,
		"Print the startup label migration's report of documents whose "+
			"visibility labels could not be translated into label policies "+
			"(and so are readable by nobody), from the policy catalog that "+
			"-state-dir, -data and -policy-dir select, then exit without "+
			"serving. Run it with the workspace stopped (#570)",
	)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *listUntranslatable {
		_, policyDir := resolveWorkspacePaths(*stateDir, *data, *policyDirFlag)
		return listUntranslatableLabels(ctx, output, policyDir)
	}
	executors, err := newConfiguredFleetExecutors(
		splitCommaList(*fleetExecutorRefs))
	if err != nil {
		return err
	}
	// Bound here rather than beside the ask executor further down, because
	// this binding waits on nothing: it declares a ceiling and performs no
	// work, so it needs neither workspace settings nor a chat provider. The
	// collision check is the reason it must not be deferred — a reference
	// named by both this and -fleet-ask-executor-ref has to be refused
	// whether or not the ask binding ever gets as far as being attempted.
	externalBindings := externalFleetEffectBindings{
		mutating:     splitCommaList(*fleetExternalExecutorRefs),
		transmitting: splitCommaList(*fleetExternalEgressExecutorRefs),
		// Trimmed, because splitCommaList trims the entries of the two lists
		// above and comparing a trimmed entry against an untrimmed reference
		// would miss a collision that newConfiguredFleetExecutors then
		// reports as an unrelated allow-list failure.
		askReference: strings.TrimSpace(*fleetAskExecutorRef),
	}
	if err := bindExternalFleetEffects(executors, externalBindings); err != nil {
		return err
	}
	attestationTrust, err := loadExecutorAttestationTrust(
		*fleetExecutorAttestation, externalBindings)
	if err != nil {
		return err
	}

	oidc := applyLegacyEntraCompatibility(oidcConfig{
		issuer: firstNonEmpty(
			*oidcIssuer, os.Getenv("SHOAL_OIDC_ISSUER")),
		discoveryURL: firstNonEmpty(
			*oidcDiscoveryURL, os.Getenv("SHOAL_OIDC_DISCOVERY_URL")),
		audiences: splitCommaList(firstNonEmpty(
			*oidcAudiences, os.Getenv("SHOAL_OIDC_AUDIENCE"))),
		jwksURI: firstNonEmpty(
			*oidcJWKSURI, os.Getenv("SHOAL_OIDC_JWKS_URI")),
		allowedAlgorithms: splitCommaList(firstNonEmpty(
			*oidcAllowedAlgs, os.Getenv("SHOAL_OIDC_ALLOWED_ALGS"))),
		clockSkew: *oidcClockSkew,
		subjectClaim: firstNonEmpty(
			*oidcSubjectClaim, os.Getenv("SHOAL_OIDC_SUBJECT_CLAIM")),
		actorClaim: firstNonEmpty(
			*oidcActorClaim, os.Getenv("SHOAL_OIDC_ACTOR_CLAIM")),
		clientIDClaim: firstNonEmpty(
			*oidcClientIDClaim, os.Getenv("SHOAL_OIDC_CLIENT_ID_CLAIM")),
		delegationClaim: firstNonEmpty(
			*oidcDelegationClaim, os.Getenv("SHOAL_OIDC_DELEGATION_CLAIM")),
		authorizationClaim: firstNonEmpty(
			*oidcAuthorizationClaim,
			os.Getenv("SHOAL_OIDC_AUTHORIZATION_CLAIM")),
		readerClaimValues: splitCommaList(firstNonEmpty(
			*oidcReaderValues, os.Getenv("SHOAL_OIDC_READER_VALUES"))),
		contributorValues: splitCommaList(firstNonEmpty(
			*oidcContributorValues,
			os.Getenv("SHOAL_OIDC_CONTRIBUTOR_VALUES"))),
		fleetValues: splitCommaList(firstNonEmpty(
			*oidcFleetValues, os.Getenv("SHOAL_OIDC_FLEET_VALUES"))),
		browserClientID: firstNonEmpty(
			*oidcBrowserClientID, os.Getenv("SHOAL_OIDC_BROWSER_CLIENT_ID")),
		browserScope: firstNonEmpty(
			*oidcBrowserScope, os.Getenv("SHOAL_OIDC_BROWSER_SCOPE")),
		authorizationEndpoint: firstNonEmpty(
			*oidcAuthorizationEndpoint,
			os.Getenv("SHOAL_OIDC_AUTHORIZATION_ENDPOINT")),
		tokenEndpoint: firstNonEmpty(
			*oidcTokenEndpoint, os.Getenv("SHOAL_OIDC_TOKEN_ENDPOINT")),
		approverMappingFile: firstNonEmpty(
			*oidcApproverMappingFile,
			os.Getenv("SHOAL_OIDC_APPROVER_MAPPING_FILE")),
		identityClaim: firstNonEmpty(
			*oidcIdentityClaim, os.Getenv("SHOAL_OIDC_IDENTITY_CLAIM")),
		labelGrantsFile: firstNonEmpty(
			*oidcLabelGrantsFile, os.Getenv("SHOAL_OIDC_LABEL_GRANTS_FILE")),
		executorMappingFile: firstNonEmpty(
			*oidcExecutorMappingFile,
			os.Getenv("SHOAL_OIDC_EXECUTOR_MAPPING_FILE")),
	}, legacyEntraConfig{
		tenantID: firstNonEmpty(
			*entraTenant, os.Getenv("SHOAL_ENTRA_TENANT")),
		issuer: firstNonEmpty(
			*entraIssuer, os.Getenv("SHOAL_ENTRA_ISSUER")),
		audience: firstNonEmpty(
			*entraClientID, os.Getenv("SHOAL_ENTRA_CLIENT_ID")),
		jwksURI: firstNonEmpty(
			*entraJWKSURI, os.Getenv("SHOAL_ENTRA_JWKS_URI")),
		allowedAlgorithms: splitCommaList(firstNonEmpty(
			*entraAllowedAlgs, os.Getenv("SHOAL_ENTRA_ALLOWED_ALGS"))),
		clockSkew: *entraClockSkew,
		readerRoles: splitCommaList(firstNonEmpty(
			*entraReaderRoles, os.Getenv("SHOAL_ENTRA_READER_ROLES"))),
		contributorRoles: splitCommaList(firstNonEmpty(
			*entraContributorRoles,
			os.Getenv("SHOAL_ENTRA_CONTRIBUTOR_ROLES"))),
		scope: firstNonEmpty(
			*entraScope, os.Getenv("SHOAL_ENTRA_SCOPE")),
	})

	embedding, err := embeddingConfig{
		provider:   *embeddingProvider,
		model:      *embeddingModel,
		baseURL:    *embeddingBaseURL,
		apiKeyEnv:  *embeddingAPIKeyEnv,
		dimensions: *embeddingDimensions,
	}.embedder()
	if err != nil {
		return err
	}
	var activeOntology *ontology.OntologyVersion
	if path := firstNonEmpty(*ontologyFile, os.Getenv("SHOAL_ONTOLOGY_FILE")); path != "" {
		version, err := loadOntologyVersionFile(path)
		if err != nil {
			return err
		}
		activeOntology = &version
	}

	// The requested address is classified and refused before anything is
	// bound, so an address the workspace may not serve never opens a socket
	// and never prompts an operator to approve exposure the program has
	// already decided against.
	if _, err := selectAuthenticator(
		*developmentAuth, *developmentLabels, oidc, *listen, time.Now); err != nil {
		return err
	}
	listener, err := listenTCP("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", *listen, err)
	}
	// Defence in depth. The resolved listener address is authoritative: it may
	// be wider than the requested one, and the check above may be incomplete.
	// Identity is decided from it, and a refusal closes the listener here,
	// before the corpus is opened and before any request can be served.
	authenticator, err := selectAuthenticator(
		*developmentAuth, *developmentLabels, oidc,
		listener.Addr().String(), time.Now)
	if err != nil {
		listener.Close()
		return err
	}
	var browserAuth *webapi.BrowserAuthConfig
	// The approver mapping in force. Zero when none is configured; the
	// approval service pins every decision to it.
	var approverMapping auth.Digest
	// The identity scheme the OIDC authenticator mints under (#526); nil
	// for any other authenticator.
	var identityScheme *identitySchemeConfig
	// The label grant file in force (#570): its digest and grant count.
	// Zero when none is configured.
	var labelGrantsDigest auth.Digest
	var labelGrantCount int
	// The executor mapping in force (#391): its digest and entry count.
	// Zero when none is configured.
	var executorMappingDigest auth.Digest
	var executorCount int
	if oidcAuthenticator, ok := authenticator.(*oidcAuthenticator); ok {
		// An approver mapping needs an issuer that states public subject
		// identifiers only, unless a stable identity claim is configured;
		// refuse to start otherwise, including when discovery cannot be
		// read.
		if err := oidcAuthenticator.verifyApproverDiscovery(ctx); err != nil {
			listener.Close()
			return fmt.Errorf(
				"refusing to serve %s with -oidc-approver-mapping-file: the "+
					"issuer's discovery must be readable and, without "+
					"-oidc-identity-claim, state subject_types_supported "+
					"[\"public\"] only (pairwise subjects, as Entra issues, "+
					"would let one human approve their own request): %w",
				listener.Addr(), err)
		}
		// Every mapped executor reference must be one this host configures.
		if err := oidcAuthenticator.refuseUnconfiguredExecutorRefs(executors); err != nil {
			listener.Close()
			return err
		}
		approverMapping = oidcAuthenticator.approverMappingDigest()
		labelGrantsDigest, labelGrantCount = oidcAuthenticator.labelGrantsDigest()
		executorMappingDigest, executorCount = oidcAuthenticator.executorMappingDigest()
		migrateFrom, err := parseIdentitySchemeMigrate(
			strings.TrimSpace(*oidcIdentitySchemeMigrate))
		if err != nil {
			listener.Close()
			return err
		}
		scheme := oidcAuthenticator.identityScheme()
		identityScheme = &identitySchemeConfig{
			scheme: scheme, migrateFrom: migrateFrom,
		}
		fmt.Fprintf(output, "OIDC identity scheme for %s: %s\n",
			scheme.issuer, coordination.Digest(scheme.digest))
		browserAuth, err = oidcAuthenticator.browserAuthConfig(ctx)
		if err != nil {
			listener.Close()
			return fmt.Errorf(
				"refusing to serve %s with OIDC browser login: %w",
				listener.Addr(), err)
		}
	}
	authority := auth.NewAuthority()
	// The policy catalog is durable, so documents ingested by this build stay
	// authorized across restarts (issue #284). The gate below still allows a
	// development-only, one-time migration for a corpus whose documents were
	// ingested before the catalog was durable, for -dev-auth on loopback and
	// nothing else.
	backfill := newDevelopmentBackfill(
		authenticator, listener.Addr().String(), authority.Binder())
	corpusDir, policyDir := resolveWorkspacePaths(*stateDir, *data, *policyDirFlag)
	opened, err := openService(ctx, serviceConfig{
		backend:   *backend,
		data:      corpusDir,
		policyDir: policyDir,
		remote:    *remote,
		embedder:  embedding,
		resolver:  authority.Resolver(),
		clock:     time.Now,
		backfill:  backfill,
		ontology:  activeOntology,
		executors: executors,
		approverMapping: func(context.Context) (auth.Digest, error) {
			return approverMapping, nil
		},
		identityScheme: identityScheme,

		executorAttestation: attestationTrust,

		concealWithholding: *concealWithholding,
		mosaic: authorized.MosaicBudget{
			MaxDomains: uint32(*mosaicBudget),
			Window:     *mosaicWindow,
		},
	})
	if err != nil {
		listener.Close()
		return err
	}
	service, cleanup := opened.service, opened.close
	defer cleanup()

	// The host-authority allow-list defaults to the resolved listen address,
	// preserving the local-first posture: a loopback bind serves only requests
	// whose Host is that loopback authority. A non-loopback or wildcard bind
	// resolves to an address (for example 0.0.0.0:<port>) that real client Host
	// headers never carry, so such a deployment must name its external
	// authority with -allowed-host or every request is refused — a fail-closed
	// default, safe but requiring explicit configuration behind a proxy.
	allowedHostConfigured := firstNonEmpty(*allowedHost, os.Getenv("SHOAL_ALLOWED_HOST"))
	allowedAuthorities := splitCommaList(allowedHostConfigured)
	if len(allowedAuthorities) == 0 {
		allowedAuthorities = []string{listener.Addr().String()}
	}
	// Surface the most common way an operator trips over this gate — a public
	// bind with no -allowed-host, which fails closed on every request — as a
	// startup warning before any traffic arrives. A per-refusal log is
	// deliberately avoided: the Host is attacker-controlled, so logging each
	// refusal invites a log flood, and it would not reach an operator any
	// sooner than this line.
	if warning := hostAuthorityStartupWarning(
		listener.Addr().String(), allowedHostConfigured); warning != "" {
		fmt.Fprintln(output, warning)
	}

	handler, err := webapi.NewAuthenticatedHandler(
		service, authenticator, authority.Binder(), allowedAuthorities...)
	if err != nil {
		listener.Close()
		return err
	}
	var chat webapi.AskProvider
	var provenance webapi.InteractionProvider
	if opened.settings != nil {
		if err := handler.SetWorkspaceSettingsProvider(opened.settings); err != nil {
			listener.Close()
			return err
		}
		if opened.client != nil {
			chat, provenance, err = newChatProviders(
				ctx, opened.client, authority.Resolver(), chatModelConfig{
					provider: *chatProvider, model: *chatModel, baseURL: *chatBaseURL,
					apiKeyEnv: *chatAPIKeyEnv, organization: *chatOrganization,
					project: *chatProject, retrievalModes: chatRetrievalModes(embedding),
				})
			if err != nil {
				listener.Close()
				return err
			}
			if err := handler.SetChatProvider(chat); err != nil {
				listener.Close()
				return err
			}
			if err := handler.SetInteractionProvider(provenance); err != nil {
				listener.Close()
				return err
			}
		}
	}
	if *fleetAskExecutorRef != "" {
		if chat == nil {
			listener.Close()
			return errors.New(
				"-fleet-ask-executor-ref requires workspace settings and a " +
					"configured chat provider")
		}
		askExecutor, err := webapi.NewAskExecutor(
			webapi.AskExecutorConfig{Provider: chat})
		if err != nil {
			listener.Close()
			return err
		}
		if err := executors.bind(*fleetAskExecutorRef, askExecutor); err != nil {
			listener.Close()
			return err
		}
	}
	var mcpTools []mcp.OptionalToolProvider
	if chat != nil {
		askTool, err := mcp.NewAskTool(chat)
		if err != nil {
			listener.Close()
			return err
		}
		mcpTools = append(mcpTools, askTool)
	}
	if provenance != nil {
		provenanceTools, err := mcp.NewInteractionTools(provenance)
		if err != nil {
			listener.Close()
			return err
		}
		mcpTools = append(mcpTools, provenanceTools...)
	}
	if opened.fleetDispatch != nil {
		fleetTools, err := mcp.NewFleetDispatchTools(
			opened.fleetDispatch, authority.Resolver())
		if err != nil {
			listener.Close()
			return err
		}
		mcpTools = append(mcpTools, fleetTools...)
	}
	mcpServer, err := mcp.NewServer(mcp.Config{
		Service:           service,
		Authority:         authority,
		Decisions:         mcp.DecisionProviderFunc(authority.Resolver().Resolve),
		InteractionSink:   opened.client,
		Snapshots:         opened.client,
		WorkspaceSettings: opened.settings,
		OptionalTools:     mcpTools,
		ServerInfo: mcp.Implementation{
			Name:        "shoal-explore-web",
			Title:       "Shoal Explorer MCP",
			Version:     "1",
			Description: "Authenticated Shoal Explorer over Streamable HTTP",
		},
		Instructions: "Every HTTP request is independently authenticated. " +
			"Session IDs retain protocol state only and never carry authority.",
	})
	if err != nil {
		listener.Close()
		return err
	}
	mcpHTTP, err := mcp.NewHTTPHandler(mcp.HTTPConfig{
		Server:                   mcpServer,
		AllowedOrigins:           mcp.OriginsForAuthorities(allowedAuthorities),
		RequireWorkspaceSettings: opened.settings != nil,
	})
	if err != nil {
		listener.Close()
		return err
	}
	if err := handler.MountAuthenticated("/mcp", mcpHTTP); err != nil {
		listener.Close()
		return err
	}
	if opened.fleetRegistry != nil || opened.fleetDispatch != nil ||
		opened.fleetEvents != nil {
		if opened.fleetRegistry == nil || opened.fleetDispatch == nil ||
			opened.fleetEvents == nil {
			listener.Close()
			return shoal.NewError(
				shoal.ErrorUnavailable,
				"fleet HTTP dependencies are incomplete",
			)
		}
		var fleetHandler http.Handler
		if opened.attestation != nil {
			fleetHandler, err = webapi.NewFleetHandlerWithAttestation(
				opened.fleetRegistry, opened.fleetDispatch, opened.attestation)
		} else {
			fleetHandler, err = webapi.NewFleetHandler(
				opened.fleetRegistry, opened.fleetDispatch)
		}
		if err != nil {
			listener.Close()
			return err
		}
		if err := handler.MountAuthenticated(
			webapi.FleetRoutePrefix, fleetHandler,
		); err != nil {
			listener.Close()
			return err
		}
		if err := handler.MountFleetEvents(opened.fleetEvents); err != nil {
			listener.Close()
			return err
		}
	}
	if opened.admission != nil {
		admissionHandler, err := webapi.NewAdmissionHandler(opened.admission)
		if err != nil {
			listener.Close()
			return err
		}
		if err := handler.MountAuthenticated(
			webapi.AdmissionRoutePrefix, admissionHandler,
		); err != nil {
			listener.Close()
			return err
		}
	}
	if opened.approvals != nil {
		approvalHandler, err := webapi.NewFleetApprovalHandler(opened.approvals)
		if err != nil {
			listener.Close()
			return err
		}
		if err := handler.MountAuthenticated(
			webapi.FleetApprovalRoutePrefix, approvalHandler,
		); err != nil {
			listener.Close()
			return err
		}
	}
	if opened.teamOverview != nil {
		teamHandler, err := webapi.NewTeamOverviewHandler(opened.teamOverview)
		if err != nil {
			listener.Close()
			return err
		}
		if err := handler.MountAuthenticated(
			webapi.TeamOverviewRoute, teamHandler,
		); err != nil {
			listener.Close()
			return err
		}
	}
	// Browser login is optional and publishes only non-secret OIDC parameters.
	// With -dev-auth, or an API-only OIDC configuration, auth-config reports
	// unconfigured and the UI renders no login flow.
	if browserAuth != nil {
		handler.SetBrowserAuthConfig(browserAuth)
	}
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	if *developmentAuth {
		fmt.Fprintf(
			output,
			"Authenticating every request as development principal %s\n",
			developmentSubject,
		)
		if *developmentLabels != "" {
			fmt.Fprintf(output,
				"The development principal holds the label grants %s\n",
				*developmentLabels)
		}
	}
	if oidc.configured() {
		fmt.Fprintf(
			output,
			"Validating OIDC bearer tokens for audience(s) %s; "+
				"unmapped authorization claims are denied\n",
			strings.Join(oidc.audiences, ","),
		)
		if approverMapping != (auth.Digest{}) {
			fmt.Fprintf(output,
				"OIDC approver mapping is in force (%s); approvals are "+
					"pinned to it\n", approverMapping)
		}
		if labelGrantsDigest != (auth.Digest{}) {
			fmt.Fprintf(output,
				"OIDC label grants are in force (%s): %d grant(s); a label "+
					"no grant names is visible to nobody\n",
				labelGrantsDigest, labelGrantCount)
		} else {
			fmt.Fprintf(output,
				"No OIDC label grants are configured: labelled content is "+
					"visible to nobody\n")
		}
		if executorMappingDigest != (auth.Digest{}) {
			fmt.Fprintf(output,
				"OIDC executor mapping is in force (%s): %d executor "+
					"credential(s), each bound to one executor reference\n",
				executorMappingDigest, executorCount)
		} else {
			fmt.Fprintf(output,
				"No OIDC executor mapping is configured: no token may "+
					"execute queued work\n")
		}
	}
	if *backend == "embedded" {
		printLabelMigration(output, opened.labelMigration)
		if activeOntology != nil {
			fmt.Fprintf(
				output,
				"Active ontology %s / %s is configured for read-only description\n",
				activeOntology.Schema().ID(), activeOntology.ID(),
			)
		}
		if backfill != nil {
			fmt.Fprintf(
				output,
				"Granted %d pre-existing document(s) in %s to %s: a "+
					"development-only, one-time migration for -dev-auth on "+
					"loopback of documents ingested before the policy catalog "+
					"was durable. The catalog now persists, so these "+
					"registrations survive restarts (issue #284)\n",
				opened.backfilled, corpusDir, developmentSubject,
			)
		} else {
			fmt.Fprintf(
				output,
				"Policy catalog is durable in %s: documents this build "+
					"ingests stay authorized across restarts. Persist both the "+
					"corpus (%s) and this policy directory for the workspace to "+
					"survive a restart. Documents ingested before the catalog "+
					"was durable stay hidden until re-registered (issue #284)\n",
				policyDir, corpusDir,
			)
		}
	}
	fmt.Fprintf(output, "Shoal Explorer listening at http://%s\n", listener.Addr())
	// Bound here, after the corpus and policy catalog are open and the handler
	// is built, so that the health port accepting a connection already means
	// construction finished. A bind failure is fatal rather than degraded: an
	// operator who asked for a probe surface and silently did not get one would
	// read every probe failure as the workspace being down.
	health := (*healthsurface.Server)(nil)
	state := &healthsurface.State{}
	if address := strings.TrimSpace(*healthAddress); address != "" {
		health, err = healthsurface.StartWithConfig(address, state,
			healthsurface.Config{Metrics: dispatchMetrics(opened.fleetDispatch)})
		if err != nil {
			listener.Close()
			return fmt.Errorf("listen on %s: %w", address, err)
		}
		fmt.Fprintf(output, "Health surface listening at http://%s\n", health.Address())
	}
	shutdownDone := make(chan error, 1)
	go func() {
		<-ctx.Done()
		// drain drops readiness before the workspace stops accepting, so the
		// instance keeps answering requests already routed to it while the
		// endpoints controller stops routing new ones.
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownDone <- healthsurface.Drain(shutdown, state, server, health)
	}()
	state.MarkReady()
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return <-shutdownDone
	}
	// The workspace serve loop failed on its own. Nothing signalled the
	// shutdown goroutine, so close the health listener here rather than leaking
	// it past the returning process.
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = healthsurface.Drain(shutdown, state, server, health)
	return err
}

type embeddingConfig struct {
	provider   string
	model      string
	baseURL    string
	apiKeyEnv  string
	dimensions int
}

func (c embeddingConfig) embedder() (model.Embedder, error) {
	switch c.provider {
	case "":
		return nil, nil
	case "fake":
		return model.FakeEmbedder{Dimensions: c.dimensions, Model: c.model}, nil
	case "lexical":
		if c.dimensions <= 0 {
			return nil, fmt.Errorf("embedding dimensions are required for lexical")
		}
		return model.NewLexicalEmbedder(model.LexicalConfig{
			Dimensions: c.dimensions,
			Model:      c.model,
		})
	case "voyage":
		if c.dimensions <= 0 {
			return nil, fmt.Errorf("embedding dimensions are required for voyage")
		}
		return model.NewVoyageEmbedder(model.VoyageConfig{
			BaseURL:          c.baseURL,
			Model:            c.model,
			Dimensions:       c.dimensions,
			APICredentialEnv: c.apiKeyEnv,
		})
	case "ollama":
		if c.dimensions <= 0 {
			return nil, fmt.Errorf("embedding dimensions are required for ollama")
		}
		return model.NewOllamaEmbedder(model.OllamaConfig{
			BaseURL:    c.baseURL,
			Model:      c.model,
			Dimensions: c.dimensions,
		})
	case "openai":
		if c.dimensions <= 0 {
			return nil, fmt.Errorf("embedding dimensions are required for openai")
		}
		return model.NewOpenAIEmbedder(model.OpenAIConfig{
			BaseURL:             c.baseURL,
			EmbeddingModel:      c.model,
			EmbeddingDimensions: c.dimensions,
			Credentials:         envCredentialResolver(c.apiKeyEnv),
		})
	default:
		return nil, fmt.Errorf("unknown embedding provider %q", c.provider)
	}
}

type envCredentialResolver string

func (r envCredentialResolver) ResolveCredential(context.Context) ([]byte, error) {
	if r == "" {
		return nil, model.ErrInvalidConfig
	}
	value := os.Getenv(string(r))
	if value == "" {
		return nil, model.ErrCredential
	}
	return []byte(value), nil
}

func (r envCredentialResolver) CacheIdentity() (string, error) {
	if r == "" {
		return "", model.ErrInvalidConfig
	}
	return "env:" + string(r), nil
}

// serviceConfig carries the backend selection together with the trusted
// authorization dependencies every backend must enforce.
type serviceConfig struct {
	backend string
	data    string
	// policyDir is the durable policy catalog directory. When empty it is
	// derived from data as a legacy sibling, so callers that set only data keep
	// working.
	policyDir string
	remote    string
	embedder  model.Embedder
	resolver  auth.Resolver
	clock     func() time.Time
	// backfill is nil unless the development principal is serving a loopback
	// listener. See developmentBackfill and issue #284.
	backfill *developmentBackfill
	// ontology is an optional immutable snapshot configured at startup for the
	// read-only ontology description endpoint.
	ontology *ontology.OntologyVersion
	// concealWithholding removes the withholding counts from responses. See
	// the -conceal-withholding flag.
	concealWithholding bool
	// executors is the host-owned allowlist of opaque fleet executor
	// references. A non-nil empty registry keeps agent registration disabled.
	executors fleet.ExecutorRegistry
	// executorAttestation is the -fleet-executor-attestation trust. Nil
	// configures no trust root: actions requiring attestation cannot
	// register, and the presentation route is not mounted.
	executorAttestation *executorattest.Trust
	// generationReader is the shared current-policy authority used by
	// authorized reads, settings, dispatch, and event delivery. Tests and
	// deployments with a mutable policy lifecycle inject its durable reader.
	generationReader auth.GenerationReader
	// mosaic configures the sensitivity-domain co-occurrence budget. A zero
	// MaxDomains disables the control.
	mosaic authorized.MosaicBudget
	// wrapApprovalStore is a test seam and nothing else: nil in every
	// production path. It lets a test place a fault between the durable
	// approval store and the approval service — a write that lands and then
	// reports failure, which is what a crash between the approval's two
	// compare-and-set writes looks like to the caller — while every other
	// dependency stays the one this function composes.
	wrapApprovalStore func(fleet.ApprovalStore) fleet.ApprovalStore
	// approverMapping returns the operator approver mapping digest in force
	// (#451), or the zero digest. Nil means none.
	approverMapping func(context.Context) (auth.Digest, error)
	// identityScheme is the OIDC identity scheme in force (#526), recorded
	// in, or checked against, the coordination store before anything is
	// served. Nil for an authenticator that is not OIDC.
	identityScheme *identitySchemeConfig
}

// openedService is the constructed workspace service together with what the
// startup backfill registered, so the operator can be told exactly what the
// development principal was granted.
type openedService struct {
	service       webapi.Service
	settings      webapi.WorkspaceSettingsProvider
	fleetRegistry webapi.FleetRegistryProvider
	fleetDispatch webapi.FleetDispatchProvider
	fleetEvents   webapi.FleetEventService
	admission     webapi.AdmissionProvider
	approvals     webapi.FleetApprovalProvider
	attestation   webapi.FleetAttestationProvider
	teamOverview  webapi.TeamOverviewProvider
	client        *authorized.Client
	backfilled    int
	// labelMigration is what the startup label migration did (#570).
	labelMigration labelMigrationOutcome
	close          func()
}

var (
	workspacePublicationDomain = coordination.DomainID("shoal-explore-web/publication")
	workspaceRuntimeOwner      = coordination.OwnerID("shoal-explore-web/runtime")
)

// concealWithholdingDefault resolves the environment default for
// -conceal-withholding. Only the documented value enables it, so a stray value
// cannot switch concealment on.
//
// The converse does not hold and is worth stating plainly: an unrecognized
// value such as "true" or "1 " leaves concealment off. Exact matching cannot
// prevent accidental disablement, so an operator who depends on concealment
// should set the flag, where a typo is rejected by the parser, rather than the
// environment variable, where it is silently ignored.
func concealWithholdingDefault() bool {
	return os.Getenv("SHOAL_CONCEAL_WITHHOLDING") == "1"
}

func openService(
	ctx context.Context,
	config serviceConfig,
) (openedService, error) {
	closed := openedService{close: func() {}}
	switch config.backend {
	case "embedded":
		runtimeConfig := explorercoord.Config{
			Directory: config.data,
			Domain:    workspacePublicationDomain,
			Owner:     workspaceRuntimeOwner,
			Authority: transaction.Authority{
				Generation:          1,
				Fence:               1,
				Holder:              workspaceRuntimeOwner,
				Mode:                coordination.WriterModeEmbeddedPrimary,
				RetentionGeneration: 1,
				HistoryFloor:        1,
			},
			Clock: config.clock,
		}
		explorerfleetevents.ConfigureHostedRuntime(&runtimeConfig)
		embedded, err := explorercoord.OpenExplorer(
			runtimeConfig, explorer.Options{Embedder: config.embedder})
		if err != nil {
			return closed, err
		}
		corpus := embedded.Explorer
		// Before anything else reads or writes: a replica naming principals
		// under another identity scheme than the one recorded refuses to
		// start (#526).
		if err := stampIdentityScheme(
			ctx, embedded.Runtime.EmbeddedEngine(), runtimeConfig.CoordinationTable,
			config.identityScheme); err != nil {
			embedded.Close()
			return closed, err
		}
		// The policy catalog is durable and lives in its own directory, not a
		// subdirectory of the corpus: the corpus engine treats every
		// subdirectory as a table, so nesting the store there would corrupt
		// table discovery. A caller that sets only data keeps the legacy
		// sibling layout; -state-dir/-policy-dir place both under one mount.
		policyDir := config.policyDir
		if policyDir == "" {
			policyDir = policyStoreDir(config.data)
		}
		store, err := authorized.OpenDurablePolicyStore(policyDir)
		if err != nil {
			embedded.Close()
			return closed, err
		}
		// A non-empty corpus paired with an empty policy catalog is the
		// signature of a lost or unmounted policy volume: the documents
		// survived but every authorization registration is gone. Refuse rather
		// than serve a silently under-authorized workspace. The development
		// backfill is the sanctioned way to (re)register a pre-durability
		// corpus, so this guard applies only in production (backfill == nil),
		// where the two situations are otherwise indistinguishable.
		if config.backfill == nil {
			if err := refuseSplitBrainStateDirectory(
				ctx, corpus, store, config.data, policyDir); err != nil {
				store.Close()
				embedded.Close()
				return closed, err
			}
		}
		generationReader := config.generationReader
		if isNilFleetDependency(generationReader) {
			generationReader = fixedGenerationReader{
				domain:     workspaceAuthorizationDomain,
				generation: workspacePolicyGeneration,
			}
		}
		client, err := authorizedClient(
			corpus, store, config.resolver, generationReader,
			config.clock, config.mosaic)
		if err != nil {
			store.Close()
			embedded.Close()
			return closed, err
		}
		// Documents labelled before labels were enforced still carry the
		// bare source rule, readable by every holder of the source, until
		// this narrows them (#570). It runs before anything is served, and
		// any failure refuses to start.
		labelMigration, err := runLabelMigration(ctx, client)
		if err != nil {
			store.Close()
			embedded.Close()
			return closed, err
		}
		if err := corpus.EnsureInteractionSink(ctx); err != nil {
			store.Close()
			embedded.Close()
			return closed, err
		}
		interactionRecorder, err := interaction.NewRecorder(
			ctx, fleetInteractionSink{durable: corpus, authorized: client})
		if err != nil {
			store.Close()
			embedded.Close()
			return closed, err
		}
		if err := interactionRecorder.SetClock(config.clock); err != nil {
			store.Close()
			embedded.Close()
			return closed, err
		}
		fleetLifecycleRecorder, err := explorerfleet.NewLifecycleRecorderWithReader(
			fleetInteractionSink{durable: corpus, authorized: client}, corpus)
		if err != nil {
			store.Close()
			embedded.Close()
			return closed, err
		}
		executors := config.executors
		if executors == nil {
			executors = configuredFleetExecutors{}
		}
		snapshots, err := explorerfleet.NewInteractionSnapshotProvider(corpus)
		if err != nil {
			store.Close()
			embedded.Close()
			return closed, err
		}
		// Executor attestation (#446). Without a trust file nothing is
		// composed: the registry refuses attested actions and dispatch has no
		// attestation to read, so every attested claim is refused.
		var (
			attestationTrust   fleet.AttestationTrust
			attestationReader  fleet.ExecutorAttestations
			attestationService *fleet.AttestationService
		)
		if config.executorAttestation != nil {
			adapter, err := openExecutorAttestations(
				embedded.Runtime.EmbeddedEngine(), config.executorAttestation)
			if err != nil {
				store.Close()
				embedded.Close()
				return closed, err
			}
			attestationTrust, attestationReader = adapter, adapter
			attestationRecorder, err := explorerfleet.NewAttestationRecorder(
				interactionRecorder, snapshots)
			if err != nil {
				store.Close()
				embedded.Close()
				return closed, err
			}
			attestationService, err = fleet.NewAttestationService(fleet.AttestationConfig{
				Presenter: adapter, Resolver: config.resolver,
				Recorder: attestationRecorder, Clock: config.clock,
			})
			if err != nil {
				store.Close()
				embedded.Close()
				return closed, err
			}
		}
		fleetRegistry, err := explorerfleet.ComposeWithAttestationTrust(
			embedded.Runtime, config.resolver, fleetLifecycleRecorder, snapshots,
			executors, nil, config.clock, attestationTrust)
		if err != nil {
			store.Close()
			embedded.Close()
			return closed, err
		}
		cursorKey, err := explorerfleetevents.LoadOrCreateCursorKey(ctx, corpus)
		if err != nil {
			store.Close()
			embedded.Close()
			return closed, err
		}
		fleetEvents, actionEvents, err :=
			explorerfleetevents.ComposeWithPublisherAndReader(
				embedded.Runtime, workspacePublicationDomain, config.resolver,
				generationReader, func(operation auth.Operation) interaction.ResultSink {
					if sink := client.FleetActionInteractionSink(operation); sink != nil {
						return sink
					}
					return client
				}, corpus, snapshots, fleetRegistry, cursorKey, config.clock,
				// The reader label evaluator (#564): the same one the
				// dispatch reads below and the client's own interaction
				// and fold reads use, so no plane answers differently.
				client.LabelVisibility(), client.NodeGate(),
			)
		if err != nil {
			store.Close()
			embedded.Close()
			return closed, err
		}
		actionRecorder, err := explorerfleet.NewActionRecorderWithSnapshots(
			interactionRecorder, snapshots)
		if err != nil {
			store.Close()
			embedded.Close()
			return closed, err
		}
		fleetDispatch, err := explorerfleet.ComposeDispatchWithAttestations(
			embedded.Runtime, fleetRegistry, config.resolver, actionRecorder,
			actionEvents, nil, config.clock, attestationReader,
			explorerfleet.DispatchLabels{
				Visibility: client.LabelVisibility(),
				Translator: client.LabelTranslator(),
				Nodes:      client.NodeGate(),
			},
		)
		if err != nil {
			store.Close()
			embedded.Close()
			return closed, err
		}
		teamOverview, err := teamoverview.NewService(teamoverview.Config{
			Graph: client, Agents: fleetRegistry, Actions: fleetDispatch,
			Interactions: client, Resolver: config.resolver, Clock: config.clock,
		})
		if err != nil {
			store.Close()
			embedded.Close()
			return closed, err
		}
		boundFleetDispatch, err := newBoundFleetDispatch(
			fleetDispatch, config.resolver)
		if err != nil {
			store.Close()
			embedded.Close()
			return closed, err
		}
		boundFleetRegistry, err := newBoundFleetRegistry(
			fleetRegistry, config.resolver)
		if err != nil {
			store.Close()
			embedded.Close()
			return closed, err
		}
		// The authorized client is wired as the restrictor so the
		// co-occurrence budget an operator configured is the budget an
		// out-of-process caller is obliged by. Leaving it out would not
		// disable the control — the read path would still withhold — it
		// would make admission narrower than the plane it speaks for, and
		// a proxy would be told to send content the corpus would have held
		// back.
		admissionService, err := fleet.NewAdmissionService(fleet.AdmissionConfig{
			Dispatch: fleetDispatch, Restrictor: client,
		})
		if err != nil {
			store.Close()
			embedded.Close()
			return closed, err
		}
		boundAdmissionService, err := newBoundAdmission(
			admissionService, config.resolver)
		if err != nil {
			store.Close()
			embedded.Close()
			return closed, err
		}
		// Held requests (#451). The approval rows share the dispatch table
		// under their own prefix; the recorder is the same interaction
		// recorder the action audits use, attributing each transition to
		// the principal that made it rather than to the requester.
		var approvalStore fleet.ApprovalStore
		approvalStore, err = explorerfleet.NewApprovalStore(
			embedded.Runtime, nil)
		if err != nil {
			store.Close()
			embedded.Close()
			return closed, err
		}
		if config.wrapApprovalStore != nil {
			approvalStore = config.wrapApprovalStore(approvalStore)
		}
		approvalRecorder, err := explorerfleet.NewApprovalRecorder(
			interactionRecorder, snapshots)
		if err != nil {
			store.Close()
			embedded.Close()
			return closed, err
		}
		approvalService, err := fleet.NewApprovalService(fleet.ApprovalConfig{
			Dispatch: fleetDispatch, Store: approvalStore,
			Recorder: approvalRecorder,
			// Workspace settings are the one narrowing this host applies.
			// The approval service refuses an approver whose decision went
			// through one, on every approver path.
			Narrowed: func(ctx context.Context) bool {
				_, narrowed := webapi.EffectiveWorkspaceSettings(ctx)
				return narrowed
			},
			// The same current-policy authority the authorized client and
			// workspace settings use.
			Generations: generationReader,
			// The operator approver mapping in force; decisions and
			// materializations are pinned to it.
			ApproverMapping: config.approverMapping,
			// How principals are named (#526); the zero value is the
			// sub-derived scheme every host ran before.
			IdentityScheme: config.identityScheme.approvals(),
		})
		if err != nil {
			store.Close()
			embedded.Close()
			return closed, err
		}
		boundApprovalService, err := newBoundApproval(
			approvalService, config.resolver)
		if err != nil {
			store.Close()
			embedded.Close()
			return closed, err
		}
		// The development-only backfill migrates a corpus whose documents were
		// ingested before the policy catalog was durable: their authorization
		// registrations are absent until re-registered once. A failure here is
		// fatal by design: the workspace must not serve a corpus it could not
		// finish authorizing.
		backfilled, err := config.backfill.run(ctx, client)
		if err != nil {
			store.Close()
			embedded.Close()
			return closed, err
		}
		service, err := webapi.NewEmbeddedService(client)
		if err != nil {
			store.Close()
			embedded.Close()
			return closed, err
		}
		service.ConcealWithholding(config.concealWithholding)
		if config.ontology != nil {
			// This call is load-bearing; TestOntologyProposalLifecycleUsesStartedEmbeddedWorkspace
			// pins that startup wires -ontology-file into the real EmbeddedService
			// path used by proposal creation, not only into an injected test double.
			if err := service.SetOntologyVersion(*config.ontology); err != nil {
				store.Close()
				embedded.Close()
				return closed, err
			}
		}
		settingsStore, err := workspace.NewDurableStoreWithEngine(
			embedded.Runtime.EmbeddedEngine())
		if err != nil {
			store.Close()
			embedded.Close()
			return closed, err
		}
		choices, err := webapi.NewGovernedOntologyChoices(service)
		if err != nil {
			settingsStore.Close()
			store.Close()
			embedded.Close()
			return closed, err
		}
		settingsProvider, err := workspace.NewProvider(
			settingsStore,
			workspace.ProviderOptions{
				Resolver:         config.resolver,
				GenerationReader: generationReader,
				OntologyChoices:  choices,
				Clock:            config.clock,
			},
		)
		if err != nil {
			settingsStore.Close()
			store.Close()
			embedded.Close()
			return closed, err
		}
		return openedService{
			service:        service,
			settings:       settingsProvider,
			fleetRegistry:  boundFleetRegistry,
			fleetDispatch:  boundFleetDispatch,
			fleetEvents:    fleetEvents,
			admission:      boundAdmissionService,
			approvals:      boundApprovalService,
			attestation:    optionalAttestation(attestationService),
			teamOverview:   teamOverview,
			client:         client,
			backfilled:     backfilled,
			labelMigration: labelMigration,
			close: func() {
				settingsStore.Close()
				store.Close()
				embedded.Close()
			},
		}, nil
	case "remote":
		// The remote backend forwards workspace calls to an upstream Explorer
		// web API over HTTP and has no way to carry the caller's decision
		// across that hop: webapi.RemoteService is a workspace service, not an
		// explorer.Client, so authorized.Client cannot wrap it, and no
		// on-the-wire representation of auth.Decision exists yet. Serving it
		// would mean authenticating at this edge and then calling upstream
		// with no identity at all. Refuse rather than leave that path open.
		return closed, fmt.Errorf(
			"backend remote is unavailable: forwarding the caller's " +
				"authorization decision to an upstream Explorer is not " +
				"implemented, so the upstream call would carry no identity " +
				"(see issue #278, edge identity)")
	default:
		return closed, fmt.Errorf("unknown backend %q", config.backend)
	}
}

// policyStoreDir derives the durable policy catalog's directory from the corpus
// data directory. It is a sibling rather than a child because the corpus engine
// treats every subdirectory of the data directory as a table.
func policyStoreDir(data string) string {
	return filepath.Clean(data) + "-policy"
}

// workspaceSettingsStoreDir keeps settings outside the corpus engine while
// placing them under the same recommended state root.
func workspaceSettingsStoreDir(data string) string {
	clean := filepath.Clean(data)
	if filepath.Base(clean) == "corpus" {
		return filepath.Join(filepath.Dir(clean), "settings")
	}
	return clean + "-settings"
}

// firstNonEmpty returns the first argument whose trimmed value is non-empty.
// It gives command-line flags precedence over their environment fallbacks.
// hostAuthorityStartupWarning returns an operator warning when the workspace
// will refuse every request because it bound a non-loopback address but no
// external host authority was configured — so the host-authority allow-list
// defaults to the bind address, which real client Host headers never carry. It
// returns "" for a configuration that will serve: an explicitly configured
// authority, or the loopback default. The check is on the resolved listen
// address so a wildcard bind ([::]:port, 0.0.0.0:port) is caught too.
func hostAuthorityStartupWarning(resolvedListenAddr, configuredAllowedHost string) string {
	if strings.TrimSpace(configuredAllowedHost) != "" {
		return ""
	}
	if listenAddressIsLoopback(resolvedListenAddr) {
		return ""
	}
	return fmt.Sprintf(
		"WARNING: bound %s but -allowed-host/SHOAL_ALLOWED_HOST is unset, so "+
			"the host-authority allow-list defaults to the bind address, which "+
			"real client Host headers do not carry; every request will be "+
			"refused with 421 Misdirected Request. Set -allowed-host to the "+
			"external name(s) clients use to reach this workspace.",
		resolvedListenAddr)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// splitCommaList splits a comma-separated flag value into trimmed, non-empty
// items. An empty input yields a nil slice.
func splitCommaList(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	items := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			items = append(items, trimmed)
		}
	}
	return items
}

// resolveWorkspacePaths determines the corpus and policy directories from the
// three location flags. The recommended configuration is a single -state-dir
// that can be mounted as one volume: the corpus and durable policy catalog live
// as siblings inside it, so persisting the state root persists both. -data
// remains for backwards compatibility and derives a sibling policy directory;
// -policy-dir overrides the policy location explicitly and always wins.
func resolveWorkspacePaths(stateDir, dataDir, policyDir string) (corpus, policy string) {
	if stateDir != "" {
		corpus = filepath.Join(stateDir, "corpus")
		policy = filepath.Join(stateDir, "policy")
	} else {
		corpus = dataDir
		policy = policyStoreDir(dataDir)
	}
	if policyDir != "" {
		policy = policyDir
	}
	return corpus, policy
}

// refuseSplitBrainStateDirectory rejects a workspace whose corpus holds
// documents while the durable policy catalog holds no registrations. That pair
// is what a deployment sees after the policy volume is lost or left unmounted:
// the documents are present but nobody is authorized to see them. Serving it
// would present an empty or under-populated corpus and hide the cause, so the
// program refuses with the paths and remediation an operator needs.
func refuseSplitBrainStateDirectory(
	ctx context.Context,
	corpus *explorer.Explorer,
	store *authorized.DurablePolicyStore,
	corpusDir string,
	policyDir string,
) error {
	if store.HasRegistrations() {
		return nil
	}
	documents, err := corpus.Documents(ctx)
	if err != nil {
		return err
	}
	if len(documents) == 0 {
		return nil
	}
	return fmt.Errorf(
		"refusing to serve a split-brain workspace: corpus %s holds %d "+
			"document(s) but the durable policy catalog %s holds no "+
			"authorization registrations. This is the signature of a lost or "+
			"unmounted policy volume; every registration was dropped and the "+
			"workspace would serve an empty or under-populated corpus. Restore "+
			"the policy directory from the same volume as the corpus (use "+
			"-state-dir so both persist under one mount), or, for a corpus "+
			"ingested before the catalog was durable, run once with -dev-auth "+
			"on a loopback listener to re-register it (issue #284)",
		corpusDir, len(documents), policyDir)
}

// authorizedClient wraps the corpus in the decision-enforcing Explorer client.
// The resolver reads the decision bound by the HTTP transport for the request
// being served, so authorization is per request rather than per process. The
// policy store is supplied by the caller so its lifetime is owned alongside the
// corpus.
func authorizedClient(
	corpus *explorer.Explorer,
	store authorized.PolicyStore,
	resolver auth.Resolver,
	generationReader auth.GenerationReader,
	clock func() time.Time,
	mosaic authorized.MosaicBudget,
) (*authorized.Client, error) {
	selector, err := authorized.NewStaticPolicySelector(
		workspaceSourceID, workspaceGrantPolicyID)
	if err != nil {
		return nil, err
	}
	// No authenticator here mints a trusted-service decision, so no service
	// account has a ceiling: a trusted service, were one ever to reach this
	// process, would be refused every labelled stored record (#564).
	ceilings, err := authorized.NewStaticCeilingResolver()
	if err != nil {
		return nil, err
	}
	scorer, _ := any(corpus).(authorized.VectorScorer)
	return authorized.NewClient(authorized.Config{
		Base:                   corpus,
		VectorScorer:           scorer,
		InteractionWriter:      corpus,
		InteractionReader:      corpus,
		OntologyInterpreter:    corpus,
		OntologyProposalStore:  corpus,
		SnapshotValidator:      corpus,
		DerivedAssertionReader: corpus,
		Resolver:               resolver,
		PolicySelector:         selector,
		PolicyStore:            store,
		GenerationReader:       generationReader,
		Clock:                  clock,
		Mosaic:                 mosaic,
		CeilingResolver:        ceilings,
	})
}
