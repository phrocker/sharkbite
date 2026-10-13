# Deploying the single-instance Shoal Explorer workspace

`shoal-explore-web` is the optional Explorer workspace: **one binary plus a data
directory**. The UI is compiled in with `go:embed`, and the corpus is served
straight from Shoal's own storage engine through the embedded backend. There is
**no ZooKeeper, no tablet server, no Accumulo, and no external datastore** in
this deployment — Shoal's storage engine is the durable backend.

The goal is a single artifact that runs on a laptop and (once the shared-instance
prerequisites below land) in a shared instance, where the two deployments differ
**only in configuration**, never in code or image.

- [Image](#image)
- [Local: one command](#local-one-command)
- [Volume layout: a single state root](#volume-layout-a-single-state-root)
- [The configuration seam: local vs shared](#the-configuration-seam-local-vs-shared)
- [Provider-neutral OIDC authentication](#provider-neutral-oidc-authentication)
- [Browser login (Authorization Code + PKCE)](#browser-login-authorization-code--pkce)
- [The unsafe-configuration guard](#the-unsafe-configuration-guard)
- [Persistence: corpus and authorization both survive](#persistence-corpus-and-authorization-both-survive)
- [Shared / cloud instance: shape and open gaps](#shared--cloud-instance-shape-and-open-gaps)
- [Azure hosting: App Service for Containers, single instance](#azure-hosting-app-service-for-containers-single-instance)

## Image

`Dockerfile.shoal-explore-web` is a multi-stage build:

- **build stage** `golang:1.25-bookworm`, `CGO_ENABLED=0`, `GOWORK=off` (the
  repo's `go.work` pins a newer toolchain than the base image; the command
  itself builds on 1.25).
- **runtime stage** `gcr.io/distroless/static-debian12:nonroot` — runs as uid/gid
  `65532`, contains only the stripped binary and an empty, `65532`-owned state
  root. `/var/lib/shoal` (the whole state root) is a declared `VOLUME`.

```console
$ docker build -f Dockerfile.shoal-explore-web -t shoal-explore-web:test --build-arg VERSION=deploy-test .
...
 => naming to docker.io/library/shoal-explore-web:test

$ docker image inspect shoal-explore-web:test \
    --format 'Size={{.Size}} User={{.Config.User}} Cmd={{json .Config.Cmd}} Volumes={{json .Config.Volumes}}'
Size=7127891 User=65532:65532 Cmd=["-state-dir","/var/lib/shoal","-listen","127.0.0.1:8098","-dev-auth"] Volumes={"/var/lib/shoal":{}}
```

The image is ~7 MB, runs as non-root, and embeds no secrets: `Config.Env` is only
`PATH` and `SSL_CERT_FILE`, and the exported filesystem contains no keys, tokens,
or `.env` files (only the binary and the distroless `etc/passwd` that defines the
`nonroot` account).

## Local: one command

```console
$ docker compose -f deploy/shoal-explore-web/docker-compose.yml up --build
```

That starts the workspace with the loopback-gated **development authenticator**
(`-dev-auth`), which authenticates every request as a fixed, clearly-named
development principal (`development-principal@localhost`). It is safe only because
the listener is proven loopback-only.

### Why host networking

`-dev-auth` refuses any listener that another host can reach, so the app binds
`127.0.0.1` **inside the container**. Docker port publishing (`-p`) forwards to
the container's *external* interface, which a loopback-only listener never
answers — so publishing a port would leave you with a refused connection. The
compose file therefore uses `network_mode: host`:

- **Linux (and WSL2):** the app is on the host's `127.0.0.1:8098`. Open
  <http://127.0.0.1:8098> in a browser.
- **Docker Desktop (macOS / Windows):** "host" is the Docker Linux VM, so
  `127.0.0.1:8098` is the VM's loopback, not the desktop OS's. Reach it from a
  helper that shares that namespace, e.g.:

  ```console
  $ docker run --rm --network host curlimages/curl -s http://127.0.0.1:8098/api/v1/meta
  ```

  For a browser on Docker Desktop, run the native binary instead
  (`go run ./cmd/shoal-explore-web -dev-auth`), or enable Docker Desktop host
  networking.

The persistent state lives on the named volume `explorer-state` mounted at the
state root `/var/lib/shoal`, so `docker compose down` followed by `up`, or any
container restart, preserves it. `docker compose down -v` deletes it.

## Volume layout: a single state root

Configure the workspace with **`-state-dir /var/lib/shoal`** (the recommended
flag from PR #288) and mount that one directory as the volume. The command places
both persistent directories inside it:

```
/var/lib/shoal/                 <- the declared VOLUME (mount THIS, persist THIS)
├── corpus/                      <- the shared corpus engine
│   ├── _shoal_explorer/         <- the document corpus table
│   └── _shoal_workspace_settings/ <- workspace settings in the same engine
└── policy/                      <- the durable authorization catalog (#284/#288)
    └── _shoal_policy/           <- the policy store's table
```

**One sentence: persist `/var/lib/shoal` — the whole state root — and both the
corpus (including workspace settings) and authorization catalog survive a
restart.**

The authorization catalog is a **sibling** of the corpus, never a child of it:
the corpus engine treats every subdirectory of the corpus directory as a table,
so nesting the catalog there would corrupt table discovery. `-state-dir` keeps
them as siblings under one mount, which removes the earlier string-suffix
coupling entirely. Workspace settings are different: they intentionally use a
dedicated table in the already-open corpus engine, so the host does not open a
second WAL, directory lock, or sidecar settings engine.

Flag precedence (see `resolveWorkspacePaths` and `TestResolveWorkspacePathsPrecedence`):

- **`-state-dir <root>`** (recommended) → corpus `<root>/corpus`, policy `<root>/policy`.
- **`-data <dir>`** (legacy, backwards compatible) → corpus `<dir>`, policy the
  sibling `filepath.Clean(<dir>)+"-policy"`. Both must be persisted.
- **`-policy-dir <dir>`** overrides the policy location and always wins.

**Mounting only the corpus directory** would preserve documents but drop every
authorization registration on restart. On this build that is not a silent
failure: the split-brain guard (below) refuses to start. Mounting the state root
keeps both. The image declares `/var/lib/shoal` (not a subdirectory) as the
volume, owned by `65532`, so the non-root process creates `corpus/` and `policy/`
at runtime.

## The configuration seam: local vs shared

The same image serves both deployments. Everything that varies is a flag or
mounted volume — never code:

| Concern            | Local (laptop)                     | Shared instance                              |
| ------------------ | ---------------------------------- | -------------------------------------------- |
| Bind address       | `-listen 127.0.0.1:8098` (loopback)| `-listen 0.0.0.0:<port>` (see gaps below)    |
| Host authority     | default (the resolved listen address) | `-allowed-host <external-name[:port]>` (see below) |
| Data directory     | `-state-dir /var/lib/shoal` (one mounted volume) | same flag, backed by durable storage |
| Authenticator      | `-dev-auth` (loopback-only)        | OpenID Connect (`-oidc-*`, see below)        |
| TLS termination    | none (loopback)                    | terminated at an ingress/reverse proxy       |
| Log format         | Go `log` text to stderr            | same today (no structured-log flag yet)      |

`deploy/shoal-explore-web/.env.example` documents the one value the local compose
profile parameterises (`SHOAL_EXPLORE_LISTEN`). The remaining columns are the
seam for a shared instance and are covered under [gaps](#shared--cloud-instance-shape-and-open-gaps).

## Provider-neutral OIDC authentication

A shared, non-loopback instance runs the standards-based OIDC authenticator
instead of `-dev-auth`. It validates every bearer token before minting an
`auth.Decision`: asymmetric signature and key identifier, configured algorithm
allow-list, exact issuer, one of the configured audiences, expiry, and
not-before. Signing keys are fetched from `jwks_uri`, cached, and refreshed on a
bounded cadence when an unknown key identifier appears. `alg: none` and all
HMAC algorithms are always refused.

`-dev-auth` and OIDC are mutually exclusive. Missing or partial OIDC
configuration, unavailable or inconsistent discovery metadata, unavailable or
malformed JWKS, malformed claims, and unmapped authorization values all fail
closed. Authentication never falls back to anonymous or development authority.

### Required token validation and claim mapping

| Flag / environment fallback | Purpose |
| --- | --- |
| `-oidc-issuer` / `SHOAL_OIDC_ISSUER` | Exact issuer required in both the discovery document and token. |
| `-oidc-audience` / `SHOAL_OIDC_AUDIENCE` | Comma-separated accepted token audiences; at least one exact match is required. |
| `-oidc-authorization-claim` / `SHOAL_OIDC_AUTHORIZATION_CLAIM` | Exact top-level string or string-array claim whose values are mapped to authority. |
| `-oidc-reader-values` / `SHOAL_OIDC_READER_VALUES` | Comma-separated claim values granting list, read, connect, neighborhood, retrieve, workspace-settings read, and agent resolve. |
| `-oidc-contributor-values` / `SHOAL_OIDC_CONTRIBUTOR_VALUES` | Comma-separated claim values granting ingest and workspace-settings write in addition to reader operations. |
| `-oidc-fleet-values` / `SHOAL_OIDC_FLEET_VALUES` | Comma-separated claim values granting Fleet control-plane access: agent register/heartbeat/revoke, delegation for child-agent registration, dispatch/invoke, subscription create/delete/deliver, and event publication. |
| `-oidc-approver-mapping-file` / `SHOAL_OIDC_APPROVER_MAPPING_FILE` | Operator file (`shoal.approvers/v1`) mapping OIDC humans on a dedicated approver audience to `action_approve` and nothing else. Without it no token may approve. Requires access tokens that carry `azp`, and either an issuer whose discovery states `subject_types_supported: ["public"]` only (expected: Auth0; Okta with a custom `azp` claim) or `-oidc-identity-claim` (Entra, Keycloak); startup and minting enforce both, so a non-qualifying issuer is refused. See `docs/approval.md`, "The OIDC approver mapping". |
| `-oidc-identity-claim` / `SHOAL_OIDC_IDENTITY_CLAIM` | A stable identity claim (#526), as a JSON array of path segments such as `'["oid"]'`. Requesters and approvers are then named `oidcid:<issuer>#<tag>#<value>` (the tag is a digest of the claim path) on both branches, read by one derivation (present, a string of 1–256 bytes, no control characters, never trimmed), and the approver mapping's public-subjects check is waived. The approver mapping must restate it as `identity_claim`. Refused: `["sub"]`; a last segment that does not name one human stably — editable and profile fields (`email`, `preferred_username`, `upn`, `unique_name`, `name`, `nickname`, `given_name`, `family_name`, `locale`, `picture`, `website`, `zoneinfo`), per-session or per-token claims (`sid`, `session_state`, `jti`, `nonce`, `at_hash`, `c_hash`, `auth_time`, `iat`, `exp`, `nbf`, `acr`, `amr`) and per-client claims (`azp`, `client_id`, `cid`); never choose a path under a user-editable parent either; an issuer containing `#`; combination with a non-default `-oidc-subject-claim`, legacy Entra mode, `-oidc-actor-claim` or `-oidc-delegation-claim`; an issuer path naming `common`, `organizations` or `{tenantid}`. Use `oid` on Entra's tenant issuer, or a Keycloak User Property `id` mapper on a client scope shared by both clients. See `docs/approval.md`, "Supported issuers". |
| `-oidc-label-grants-file` / `SHOAL_OIDC_LABEL_GRANTS_FILE` | Operator file (`shoal.label-grants/v1`) granting free-form visibility labels, one source at a time, to claim values. A grant adds visibility to a token a reader, contributor or Fleet mapping already grants, and nothing else. Without it no token holds any label, so labelled content is visible to nobody. See "Granting visibility labels" below. |
| `-oidc-executor-mapping-file` / `SHOAL_OIDC_EXECUTOR_MAPPING_FILE` | Operator file (`shoal.executors/v1`) binding service credentials, such as projected ServiceAccount tokens, to one executor reference each (#391). It names its own issuer, which may be the cluster's service-account issuer, and that issuer gets a key cache of its own. Tokens use an audience of their own, and the mapping requires a positive service assertion. It is the only source of `execute`; the minted decision holds `execute` alone, under the `action_execution` role, bound to the reference, named `oidcexec:<issuer>#<sub>`, with no labels. Without it no token may pull, claim or complete queued work. See `docs/effects-gateway-deploy.md`, "Issuing executor credentials". |
| `-oidc-identity-scheme-migrate` | One-shot identity scheme switch: the digest (64 lowercase hex digits) of the scheme recorded for `-oidc-issuer` that this rollout replaces, as the startup refusal prints it. Every OIDC replica records its scheme (issuer, claim path, identity format) in the coordination store and refuses to start when it differs from the recorded one; with this flag a replica starts only if the recorded scheme is the one named (it then records its own) or already its own. Remove it after the rollout. Changing `-oidc-issuer` (Entra v1 to v2, a hostname move) is a scheme switch too and needs this flag. Upgrade every replica to this build on the current scheme before switching, and never roll back to a build before #553 across a switch. A request made under the previous scheme can then only expire. See `docs/approval.md`, "Switching an issuer's identity scheme". |

At least one reader, contributor, or Fleet value is required. A missing,
malformed, or unmapped authorization claim is denied before a service operation runs.
Authentication alone never grants corpus access, and a Fleet-only mapping is
valid without a reader or contributor mapping.

The subject defaults to the standard `sub` claim. Optional exact top-level
claim mappings preserve richer decision identity and delegation:

| Flag / environment fallback | Decision field |
| --- | --- |
| `-oidc-subject-claim` / `SHOAL_OIDC_SUBJECT_CLAIM` | `Subject` (default `sub`) |
| `-oidc-actor-claim` / `SHOAL_OIDC_ACTOR_CLAIM` | `Actor` |
| `-oidc-client-id-claim` / `SHOAL_OIDC_CLIENT_ID_CLAIM` | `ClientID` |
| `-oidc-delegation-claim` / `SHOAL_OIDC_DELEGATION_CLAIM` | ordered `OnBehalfOf` chain; accepts a string or string array |

When an optional mapping is configured, that claim becomes required and must
have the expected string shape. Token-derived identities are namespaced by the
validated issuer so subjects from different issuers cannot collide.

Under the default subject claim, `sub`, identities are `oidc:<iss>#<sub>`,
as they always were. **No value named under that prefix may contain `#`**
(#546): a token whose `sub`, or whose `-oidc-actor-claim`,
`-oidc-client-id-claim` or `-oidc-delegation-claim` value, contains `#` is
refused with `401` on both the workspace and the approver branch. The
client-ID and actor claims you configure must therefore never carry `#`.
One that routinely does locks out every user, and because the scheme digest
does not change, startup cannot warn you. The first refusal for each claim
is logged at WARN, naming the claim but never the value, and the log
repeats at most once a minute per claim. Check those logs after upgrading. A non-default `-oidc-subject-claim` names
identities `oidc:<iss>#<tag>#<value>`, where the tag is 16 hex digits of a
digest of the claim name (#546), so its identities can never equal a
`sub`-derived identity or another claim's; the actor, client and delegation
values are named under the same prefix. Releases before #546 named them
`oidc:<iss>#<value>`, in the namespace of `sub`, so one human's value and
another human's `sub` could be the same identity. For a stable, cross-client
identity use `-oidc-identity-claim` instead, which has a namespace of its
own (`oidcid:`) and cannot be combined with a non-default subject claim.

#### Upgrading a deployment that sets `-oidc-subject-claim`

A deployment that runs a non-default `-oidc-subject-claim` (anything but
`sub`; legacy Entra mode is not affected) changes identity scheme on upgrade
to this release, although its flags do not change: the scheme digest covers
the identity format. The first upgraded replica therefore **refuses to
start**, saying that the recorded scheme is the same subject claim in the
namespace of `sub` and printing the digest to migrate from. That is the
intended fail-closed path:

1. Roll out this release with
   `-oidc-identity-scheme-migrate=<the printed digest>`
   (`explorer.auth.oidc.identitySchemeMigrateFrom` in the chart), then remove
   the flag.
2. Expect principals to have new identities. Agents registered, and records
   owned, under the old `oidc:<iss>#<value>` identities belong to identities
   the new scheme no longer mints. Wherever such an identity is involved in an
   approval, the approval is refused, naming the namespace, until #526's
   adoption route moves the registration into the namespace in force.
   Requests pending at the upgrade can only expire. Re-register agents, or
   wait for adoption, as you would after any scheme switch (see
   `docs/approval.md`, "Switching an issuer's identity scheme").
3. Do not roll back to a release before #546 after the switch. That release
   would refuse to start, because the recorded scheme is not its own. Passing
   it the migrate flag would put the old shared namespace back.

Deployments on the default `sub`, with or without an approver mapping, and
legacy Entra deployments keep their recorded scheme and start without any
flag. Default `sub` deployments do refuse a `#` in `sub`, actor, client-ID
and delegation values (above). Legacy Entra (`entra:`) does not.

### Granting visibility labels

A document ingested with free-form visibility labels (`metadata["shoal.visibility"]`,
for example `secret` or `secret&pii`) is registered under its source's policy
**and** one policy per label, on that source (#570). Holding the source is no
longer enough to read it: a reader needs every label as well. Labels are
granted only by the operator, in a file; never by ATPL, so a registrant cannot
grant itself clearance, and never by the workspace role mappings.

```json
{
  "version": "shoal.label-grants/v1",
  "issuer": "https://issuer.example.com/",
  "claim": ["groups"],
  "max_values": 64,
  "grants": {
    "secret-readers": [
      {"source": "shoal-explore-web/workspace", "label": "secret"}
    ],
    "privacy-office": [
      {"source": "shoal-explore-web/workspace", "label": "secret"},
      {"source": "shoal-explore-web/workspace", "label": "pii"}
    ]
  }
}
```

- **What a grant gives.** A token whose `claim` holds a value listed under
  `grants` is given the label policies listed for that value, added to its
  `PermittedPolicyIDs`. That is all it gives. It adds no operation (a reader
  holding `secret` still cannot ingest), no source, and does nothing for a
  token no reader, contributor or Fleet value maps: such a token is denied as
  before, label or not. Approver tokens get no labels, whatever their claims
  say: the approver branch mints `action_approve` and nothing else, and approve
  reads nothing. Executor-bound decisions (#391) are not minted by this
  authenticator and get none either.
- **Per source.** A grant is a (source, label) pair, and a label policy names
  its source. A grant for (A, `secret`) does not open `secret` content in
  source B, even to a principal that also holds B. In this workspace every
  grant names `shoal-explore-web/workspace`, the one source it configures; any
  other source is refused at startup.
- **Fail closed.** A label no grant names is visible to nobody. That includes
  the principal that ingested it: ingest is refused unless the ingester holds
  every label it writes, and so is any later relabel, which needs both the
  old labels and the new. The file is read once, at startup; after a
  restart without a grant, its principals stop seeing the content on their
  next request.
- **Matching.** `claim` is a path of literal object keys, as in the approver
  mapping (`["realm_access", "roles"]` is the nested claim, never a key
  containing a dot). The claim may be a string or an array of at most
  `max_values` strings; values are compared byte for byte, never trimmed or
  case-folded. An absent, null or empty claim grants no labels. Any other
  shape, or more than `max_values` values, refuses the token.
- **Refused at startup.** Unknown fields at any depth, duplicate keys,
  trailing data, a file over 256 KiB, a version other than
  `shoal.label-grants/v1`, an `issuer` that is not `-oidc-issuer` byte for
  byte, a source that is not configured, a label outside the label charset
  (`A-Z a-z 0-9 _ . : -`, at most 256 bytes, never folded: `secret` and
  `Secret` are different labels), a label policy ID over 128 bytes, a pair
  listed twice for one value, more than 256 claim values, and more than 1024
  grants in all. A grant can never name the reserved label namespace
  (`shoal.label/v1/!…`), which is how untranslatable labels are kept from
  everyone.
- **Digest.** Startup prints `OIDC label grants are in force (<digest>)`.
  The digest identifies the file's meaning (it ignores ordering), so operators
  can confirm every replica serves the same grants. It is provenance only: it
  is not part of the policy generation or of any decision's fingerprint. A
  grant change already changes the `PermittedPolicyIDs`, and therefore the
  authorization fingerprint, of exactly the principals whose grants changed;
  folding the file's digest into every decision would instead invalidate every
  pinned decision on any edit.

For local development, `-dev-auth-labels` grants labels to the `-dev-auth`
principal: a comma-separated list of `<source>=<label>`, with the source
exactly as configured, for example
`-dev-auth-labels shoal-explore-web/workspace=secret`. `=` separates source
from label because the label charset excludes it (and includes `:`, which
therefore cannot). Without it the development principal holds no label and
can neither ingest nor read labelled content. `shoal-mcp` takes the same form
in `-identity-labels` (`docs/mcp-stdio.md`).

Neither the browser upload route nor the MCP ingest tool lets a caller attach
a visibility label today; labelled content arrives through programmatic
ingest (`authorized.Client.Ingest`) or is already in the corpus.

### Upgrading: labelled documents are tightened at startup

Before this release a label was stored on the document's nodes
(`shoal.visibility`) but its catalog rule was the bare source rule, so every
holder of the source could read it. That data is already in the corpus, so the
upgrade cannot wait for the next write: **refusing at the next write**, the
model #544 used for registration rules, would leave every existing labelled
document readable until someone happened to re-ingest it. Unlike #544, which
tightened what may be *registered* and left stored records resolving, this
migration rewrites stored authorization so the control is on before the first
request is served.

On every start, before anything is served, `shoal-explore-web` (and
`shoal-mcp`, which serves the same authorized store) runs the label migration
under the catalog's mutation lease:

- For each registered revision of each document it reads `shoal.visibility`
  from **that revision's own** metadata (and, when the stored document node
  names the same revision, from the node too, conjoining the two), and narrows
  the rule to the source policy **and** one policy per label. The narrowing
  (`PolicyStore.TightenRule`) only ever adds conjuncts and refuses anything
  else. It covers every revision of the document (not only the current one),
  the current node and edge projections, the entities and relations extracted
  from it, the relations the corpus's extraction records say it asserted
  (see the residuals for why that matters), application edges touching its
  entities, and the source claim, so a source holder without the labels can
  neither read the document nor re-ingest it unlabelled.
- A historical revision is also narrowed by its own labels, which may differ
  from the current revision's.
- A document whose labels cannot be translated (they do not parse, fall
  outside the label charset, exceed 256 bytes, exceed 61 labels or the
  flattened term or byte bounds, or make a label policy ID over 128 bytes) is
  conjoined with the reserved `shoal.label/v1/!untranslatable` policy, which
  no grant can name. It and everything derived from it is readable by
  **nobody**, its ingester included, until relabelled. Its ID, source URI and
  escaped label are recorded in a report, and the run continues. Only a label
  translation failure does this.
- **Drift is not a label failure.** An ingest commits to the corpus before it
  registers the revision in the policy catalog, so a failed registration (or a
  crash between the two) leaves the corpus one revision ahead. The migration
  narrows the registered revision by its own labels, never locks the document
  or its source claim, and lists the document under "newer revision in the
  corpus than in the policy catalog" in the log and the report. **Retry that
  ingest** to repair it; a retry that changes the labels is a relabel and
  needs the old labels and the new. An interrupted ingest's pending source
  claim keeps the rule the retry must select and takes the old labels as the
  rule the retry must also satisfy.
- The run logs its counts (`Label migration v1: examined N document(s): …`)
  and the untranslatable list. Any catalog or corpus error refuses to start:
  the workspace never serves a corpus it did not finish narrowing.
- It runs on **every** start, not once. A rollback to a release before this
  one, followed by a re-upgrade, can leave labelled documents ingested by the
  older binary under the bare source rule; the next start closes them. Every
  step is idempotent, so a run that is interrupted (a crash, a failed write)
  is simply completed by the next start, and a start with nothing to narrow
  only confirms it. The run builds one index of the catalog and then visits
  each document's own registrations: on a catalog of 1M entities and 1M
  relations, about 0.7 s for the index plus 3.6 ms per labelled document the
  first time and 0.1 ms per document afterwards.
- Its report is written to the policy catalog after the last document, for
  `-list-untranslatable-labels`. It is a report, not a gate.

Operator steps:

1. **Upgrade** the image. Keep the corpus and policy directories (one
   `-state-dir` mount).
2. **Write the label grant file** (`-oidc-label-grants-file`, or
   `explorer.auth.oidc.labelGrants` in the chart) for every label in use, as
   described above. Without a grant a label is visible to nobody, so do this
   before users notice their labelled documents disappear.
3. **Start** the workspace. The migration runs before the listener serves;
   read its line in the startup log.
4. **Review the untranslatable list.** It is in the startup log, and
   `shoal-explore-web -list-untranslatable-labels` (with the same
   `-state-dir`, `-data` or `-policy-dir`) prints the stored report and exits
   without serving. It also lists drifted documents to re-ingest. It writes
   nothing: before opening the storage engine (which would otherwise create a
   table, give a bare table a WAL, or replay a WAL), it refuses a directory
   with no policy catalog, a catalog with unflushed writes (a non-empty
   `wal.log`: the workspace is running, or stopped without a clean shutdown),
   and a catalog with no committed records. It also refuses a catalog that a
   running `shoal-explore-web` or `shoal-mcp` (or another listing) has open,
   even an idle one: every process that opens the policy catalog holds an
   exclusive lock on `.shoal-policy-catalog.lock` in its directory for as long
   as it has the catalog open, and the listing tries that lock without
   waiting (#686). The kernel releases the lock when its process exits, so the
   file a crashed workspace leaves behind blocks nothing; do not delete it.
   The listing never creates the lock file, so it refuses a catalog that no
   workspace of this version has opened yet. The file is mode `0600` and
   belongs to the workspace's user, so run the listing as that user; any other
   non-root user gets a permission error. Stop the workspace cleanly and run
   it then.
5. **Relabel** each listed document by ingesting its content again under a
   **new source URI**, with labels inside the charset, as a principal holding
   them. The original cannot be relabelled in place: the untranslatable
   conjunct is on its source claim too, and no grant can satisfy it, so its URI
   refuses every ingest and the original stays readable by nobody. That is
   deliberate: leaving the claim open would let any source holder replace the
   current revision and reopen whatever still keys on the document node.
6. **Rebuild lexicon bundles.** A bundle built before the upgrade may name
   entities of now-labelled documents, and a bundle already shipped cannot be
   recalled; rebuild and redistribute them so new bundles are scoped to the
   narrowed catalog.

Residuals, stated rather than implied:

- A relation written before `RegistrationKind` existed is stored as an
  application edge naming no document, so the policy catalog alone cannot tell
  it from a `Connect` edge. A relation that only a labelled document states,
  between entities first extracted by **other**, public documents, would then
  stay readable: both endpoints are visible. Re-extracting would not fix it
  either, because the labelled rule changes the entity namespace, so
  re-extraction mints new IDs and leaves the old relation where it is. The
  migration therefore reads the corpus's own extraction records, which name
  the document and revision behind every relation, and narrows each legacy
  relation the document asserted. Two consequences: if a public document
  asserts the same relation, it still closes (fail closed); and every
  application edge touching the document's entities, or named by its
  extraction records, takes the labels, so retrying the very same `Connect`
  afterwards conflicts.
- Shared entities and relations belong to the first document that extracted
  them. If that document is labelled, they close for readers without its
  labels even when a public document also mentions them (fail closed), and a
  later extraction of that public document under the bare rule conflicts with
  the narrowed registration.
- Pending edge reservations keep their old rule; committing one conflicts.

### Declassification audit: reason codes, listing, retention

A principal holding every label on a document may upload it again under fewer
labels. A browser upload and the MCP `shoal.ingest` tool carry no labels, so
re-uploading a labelled document's name removes all of them. That is allowed,
but it is never silent: every such relabel leaves declassification audit
records in the policy catalog (#630). One record is an `intent`, written before
the source claim changes. The other is a `pre_commit`, written before the
registration that lets the new readers in. Both name the principal, the
request, the document, the revisions, the replaced rules and the new rule.

**Reason codes (#664).** An upload may carry a reason code, which is recorded
on both records:

| Surface | How the reason is sent |
| --- | --- |
| Browser upload route (`POST /api/v1/ingest`) | a non-file multipart form field named `declassification_reason`, in any position; an empty value means none. The workspace's upload form has an optional "Declassification reason" input that sends it with every batch |
| MCP `shoal.ingest` tool | the optional `declassification_reason` argument |
| `webapi.RemoteService` (remote web API client) | `IngestRequest.DeclassificationReason`, forwarded as the form field above |

A code is 1 to 128 bytes of ASCII letters, digits and `.`, `_`, `-`, `:` and
`/`, for example `ticket:SEC-1234`. This is the authorized client's own rule
(`authorized.ValidateDeclassificationReason`). One reason applies to every
file in the batch. An invalid reason refuses the whole batch before anything
is ingested: a 400 with `invalid_argument` and the message `declassification
reason is invalid` (MCP: `shoal.ingest arguments are invalid`). A form with
two reason fields is refused with `upload declassification reason is
repeated`. A file part keeps file semantics whatever its field name.

`-require-declassification-reason` (chart:
`explorer.declassification.requireReason`) refuses a relabel that removes
labels unless it carries a reason. It is off by default. The refusal is a 400
whose message names `declassification_reason`. It is returned only after the
uploader has been authorized under the rule being replaced and under the new
one, so it tells them nothing they could not already read. Unlike an invalid
reason, it is decided per file, so it is not a pre-batch refusal: the batch
reports it for that file and goes on with the others (#687, see "Per-file
upload outcomes" below). An upload that removes no label never needs a reason.
Records are written whether this flag is on or off. The refusal, like the
refusal of a file whose content matches an earlier revision, is
deterministic: retrying the same file returns the same refusal. Supply a
reason, or change the named file, before retrying.

**Per-file upload outcomes (#687).** The upload route ingests a batch one
file at a time. Two per-file refusals do not stop it: content that matches an
earlier revision (`historical_revision`) and a reasonless relabel under
`-require-declassification-reason` (`declassification_reason_required`). The
authorized client marks each as leaving nothing committed only after it has
rolled the source claim back with nothing registered (or, for the reason,
refused before the claim). Any other failure stops the batch at that file,
including either refusal when that guarantee is missing (a failed claim
rollback, say); the files after it are not tried.

A batch where every file went through is the `200` it always was. Any other
batch is a non-2xx error, so a client that checks only the status never takes
it for success. The body keeps the usual `code`, `message` and
`indeterminate` fields and adds `files`, one entry per file in request order:

| `status` | Meaning |
| --- | --- |
| `applied`, `unchanged`, `historical` | the file's ingest disposition; it went through |
| `refused` | refused with nothing committed; `reason` names why and `message` is the public refusal |
| `failed` | the failure that stopped the batch; the top-level `message` describes it, and `"indeterminate": true` on the entry means this file may have been applied |
| `not_attempted` | after the failure that stopped the batch; upload it again |

The top-level fields describe the batch for a client that ignores `files`.
They are the failure that stopped it or, when every file was tried, the first
refusal (`409 conflict` naming the file, or the reason-required `400`). The
batch is indeterminate (a `503` with `unavailable`, the
`Shoal-Commit-Outcome: indeterminate` header and `"indeterminate": true`,
keeping the message) only when a file's own outcome is unknown, which is when
the failure that stopped the batch was itself indeterminate. The authorized
client marks every failure after its base may have written as indeterminate,
so a file that may have been applied is never reported as definite. A file that
returned a result, `applied` or `unchanged` (even an authorized relabel), has
a known outcome. A batch with such files beside refusals, or before a definite
failure, keeps its definite status and is **partial**: the
`Shoal-Commit-Outcome: partial` header and `"partial": true`. A batch where
nothing took effect (only refused or `historical` files) has neither.
A failed snapshot read after every file went through is reported the same
way, with the files' outcomes (partial when any was applied or unchanged).
`partial` is a new header value, sent only by this route. A client that tests
the header for `indeterminate` is unaffected by it.

Retrying a partial batch is idempotent and converges. Its applied files come
back `unchanged`, and its refused files are refused again, so every retry
gets the same definite answer. Before #687 such a batch was a `503`
indeterminate on every retry, and the batch stopped at its first refusal.

A directory re-uploaded after switching branches back (A to B to A) is the
common case. Each file whose content went back to A is `refused` with
`historical_revision` and keeps serving B. The others are applied or
unchanged. Such a batch is a definite `409` with the partial outcome, and its
`files` say exactly what happened. The workspace UI lists each file's outcome
(applied, unchanged, refused with its reason, failed, not attempted), says
when the files that took effect make a retry safe, and refreshes the document
list.

The MCP `shoal.ingest` tool returns the same `files` list in its error. A
batch in which every file was tried and has a known outcome, partial or not,
is reported with its definite code. A batch stopped by a failure is reported
as `indeterminate`, like every other post-dispatch MCP failure.
`webapi.RemoteService` passes an upstream's `files` on
(`webapi.IngestFileOutcomes`) after checking them against the request. It
then keeps the upstream's verified commit outcome, definite or partial, and
marks it indeterminate anyway if an outcome says a file's result is unknown.
Outcomes that do not match the request, or that come with an unverified
error, are dropped. An upstream built before #687 sends no `files`, so a
failed multi-file upload stays indeterminate there, as before. MCP over a
remote workspace therefore matches an embedded one when the upstream reports
files, and stays indeterminate when it does not.

A `RemoteService` client that talks to an upstream built before #664 has its
reason dropped without an error, because that upstream ignores unknown form
fields. The relabel is still audited there, and that upstream cannot be
configured to require a reason.

**Copy-down is not a declassification here (#663).** An operation can name
a labelled document as its input and write content read from it into
something stored under a weaker rule. That covers an application edge
(`Connect`) to the document, an interaction (a chat answer or an MCP tool
call) that has it as evidence, and what is extracted from it. None of these
writes a declassification record, and `-require-declassification-reason`
never refuses one, because none of them admits a new reader when it is
written. Every read of such an output also requires its inputs' current
rules. An edge needs both endpoints, an interaction needs every node and
edge it touched, and an extracted entity or relation needs its document.
Each also needs the rule of the revision it was derived from, so when the
labelled document is later relabelled to fewer labels (audited as above),
what was derived from the labelled revision stays closed with that revision
(#695). No content of these outputs is served to a reader without the label.
Their existence is not fully hidden yet: see the gaps below.

**Upgrading past #695: back up the policy directory first.** The first
read-write open by a release with #695 writes a format-floor row
(`sourceclaim/\xffformat-floor`, record kind 12) and logs "policy catalog
format floor written". From then on an older `shoal-explore-web` or
`shoal-mcp`, including its `-list-*` listings, refuses the catalog with
`internal: stored policy source claim record is corrupt`. The catalog is not
corrupt; the older binary would otherwise ignore the new bindings. To roll
back, restore the backup taken before the upgrade. Do not delete the row.
The corpus volume need not be restored with it: a document ingested after
the backup has no registration in the restored catalog and is withheld from
every reader (fail closed) until it is ingested again. Restore the corpus
from the same point if you want the two to agree.

Known gaps:

- Interactions and edges recorded before #695 carry no record of the
  revision they were derived from. A session is held to every revision of
  its documents up to the one current at its snapshot (by publication
  sequence) and the current one, so it may close over an earlier labelled
  revision it did not use. An edge is held to every revision its endpoints'
  documents ever had, so an old edge to a once-labelled document stays
  closed to readers without that label.
- A fleet action's `Output` is shown to readers whose view of the action's
  evidence is withheld (#696).
- Existence and count oracles. A bounded neighborhood's scan limit reveals
  the exact number of hidden edges and interactions on a visible node
  (#698). A `Connect` whose edge ID collides with a hidden edge reveals that
  edge, and through intrinsic edge IDs, a hidden document by URI (#699). An
  extracted entity shared with a document that is labelled later reveals the
  entity key (#700).
- Copy-down that names no input is not detected. Examples are labelled text
  pasted into an upload under a new name, or into a graph materialization.
  Detection needs session correlation or content similarity, which can only
  flag a write for an operator to review (#694).

**Listing the records.** `shoal-explore-web -list-declassifications` (with the
same `-state-dir`, `-data` or `-policy-dir`) prints every record in recording
order and exits without serving. Each record shows its attempt, phase, time,
source URI, document and revisions, principal, request, reason, the labels it
removed, and any other removed component (a changed source policy, a dropped
service role). The first line counts the attempts that have no `pre_commit`
record: those never reached the registration. Every caller-supplied string is
Go-quoted. The listing opens the catalog exactly as
`-list-untranslatable-labels` does, so it writes nothing and refuses a
directory with no catalog, with unflushed writes, or open in a running
workspace. Stop the workspace cleanly first.

The listing is **offline only**, by design. The records hold the old and new
label names in the clear, just as the catalog's rules do. An operator who can
open the policy directory can already read them. An online read surface would
have to restrict each record to readers who could read the replaced rule (or
to a dedicated audit role), and must never show one to readers of the new,
weaker rule only. No such surface exists, and none should be added without
that restriction (#664).

**Retention: an open decision.** The records are append-only. Nothing
compacts, expires or deletes them, and every record is loaded into memory
whenever the policy catalog opens. Retention is therefore bounded only by how
often labels are removed. One record encodes to at most about 1.9 KB for a
document with one label, about 3 KB for three labels and about 7 KB for ten.
A relabel writes two records. 10,000 relabels of three-label documents
therefore add up to about 60 MB to the catalog on disk, and a similar amount
is held in memory after every start.
Removing labels is expected to be rare, so this is not a concern at the
expected volume. A retention or compaction policy is still undecided and is
tracked on #664. Decide it before declassification happens at volume, and
watch the policy catalog's size until then.

### Discovery, keys, and validation options

| Flag / environment fallback | Default |
| --- | --- |
| `-oidc-discovery-url` / `SHOAL_OIDC_DISCOVERY_URL` | `<issuer>/.well-known/openid-configuration` |
| `-oidc-jwks-uri` / `SHOAL_OIDC_JWKS_URI` | `jwks_uri` from discovery |
| `-oidc-allowed-algs` / `SHOAL_OIDC_ALLOWED_ALGS` | `RS256`; RS/PS/ES 256/384/512 are supported |
| `-oidc-clock-skew` | `60s`, capped at `5m` |

Issuer and endpoint URLs require HTTPS. Loopback HTTP is available only to the
in-process test seam, not to production command configuration. A client secret
is never accepted.

### Migration from the former provider-specific configuration

The former `-entra-*` flags, `SHOAL_ENTRA_*` settings, Azure Bicep parameters,
and browser `tenant_id` / `authority` response fields remain available as
**deprecated compatibility aliases**. They are translated into the OIDC
implementation rather than selecting a separate authenticator. New `-oidc-*`
values take precedence field-by-field, so configuration can be migrated
incrementally:

| Former setting | Provider-neutral replacement |
| --- | --- |
| tenant | `-oidc-issuer` / `SHOAL_OIDC_ISSUER`, set to the discovery document's exact issuer |
| client ID | `-oidc-audience` / `SHOAL_OIDC_AUDIENCE`; also set `-oidc-browser-client-id` for browser login |
| reader/contributor roles | `-oidc-authorization-claim roles` plus the corresponding reader/contributor values |
| issuer, JWKS URI, allowed algorithms, clock skew | the same-named `-oidc-*` options |
| browser scope | `-oidc-browser-scope` / `SHOAL_OIDC_BROWSER_SCOPE` |

Compatibility mode preserves the old issuer derivation
(`https://login.microsoftonline.com/<tenant>/v2.0`), `oid`-then-`sub` subject
selection, `roles` claim mapping, `entra:` identity namespace, unmapped
list-only/no-corpus decision, browser scope
`openid profile <client-id>/.default`, and v2 authorization/token endpoint
defaults. The deprecated exported `BrowserAuthConfig.TenantID` and `Authority`
fields and their JSON wire fields are retained; generic endpoint fields are
published alongside them.

This changes configuration names, not the authorization path: both modern and
compatibility inputs use the OIDC validator and mint an `auth.Decision` that
must pass the matching binder and resolver before any service operation runs.

### Example

```console
$ docker run --rm --network host -v shoal-explore-state:/var/lib/shoal \
    shoal-explore-web:test \
    -state-dir /var/lib/shoal -listen 0.0.0.0:8098 \
    -allowed-host explorer.example.test \
    -oidc-issuer https://identity.example.test \
    -oidc-audience shoal-api \
    -oidc-authorization-claim access \
    -oidc-reader-values reader \
    -oidc-contributor-values contributor
Validating OIDC bearer tokens for audience(s) shoal-api; unmapped authorization claims are denied
Shoal Explorer listening at http://0.0.0.0:8098
```

Clients call the API with a bearer token issued by the configured issuer and
addressed to one of the configured audiences. Set `-allowed-host` before
exposing a public bind behind a reverse proxy (see below).

## Browser login (Authorization Code + PKCE)

Browser login is optional. Set `-oidc-browser-client-id` and
`-oidc-browser-scope` to enable the embedded UI's OAuth 2.0 Authorization Code
flow with PKCE (S256 only). The access token is held in memory only; it is never
written to `localStorage`, `sessionStorage`, or a cookie.

The UI bootstraps by reading `GET /api/v1/auth-config`, which is unauthenticated
and returns only the non-secret client ID, scope, authorization endpoint, and
token endpoint. Endpoints come from OIDC discovery unless overridden with
`-oidc-authorization-endpoint` / `SHOAL_OIDC_AUTHORIZATION_ENDPOINT` and
`-oidc-token-endpoint` / `SHOAL_OIDC_TOKEN_ENDPOINT`. Under `-dev-auth`, or an
API-only OIDC configuration without a browser client ID, it reports
`{"configured":false}` and renders no login control.

Register the deployed origin's root (for example
`https://explorer.example.test/`) as an allowed redirect URI for a public client
at the chosen identity provider, permit CORS for the token endpoint, and grant
the scopes named by `-oidc-browser-scope`. The browser sends no client secret.
The server validates the resulting access token independently against its
issuer, audience, signature, time, and claim-mapping configuration.

## Streamable HTTP MCP

For a rehearsable two-user scenario, deterministic fixture provisioning, and a
VS Code `.vscode/mcp.json` template, see
[`multi-user-demo.md`](multi-user-demo.md).

The same authenticated workspace exposes MCP `2025-11-25` at `/mcp`. It uses
the documented Streamable HTTP lifecycle: `POST initialize`, the returned
`MCP-Session-Id`, `POST notifications/initialized`, and a
`MCP-Protocol-Version: 2025-11-25` header on every subsequent request. Clients
must advertise both `application/json` and `text/event-stream` in `Accept`;
this implementation returns synchronous JSON responses and answers `GET /mcp`
with `405 Method Not Allowed` because it emits no unsolicited SSE messages.

`/mcp` is mounted inside the existing `webapi.Handler`, not beside it. Host
authority validation and the configured development/OIDC authenticator
therefore run before every MCP request. Every request must also carry exactly
one `Shoal-Workspace-ID` header containing the unpadded base64url encoding of
the owned workspace ID. Workspace settings narrow the issuer decision before
MCP dispatch and lower retrieval, graph, output, and context limits. Session
IDs retain lifecycle state only, are cryptographically random, and are bound to
the caller's authorization fingerprint plus the effective workspace ID,
settings ID, revision, cache dimensions, and limits. Another principal or
workspace cannot reuse one; a policy-generation or settings change makes the
old session absent. The initialize result reports the applied values in
`_meta["shoal.workspace"]`. An `Origin`, when present, must exactly match an
HTTP or HTTPS origin derived from the configured allowed Host authorities.

Generic MCP tool calls are durably recorded as `OperationToolCall` through the
authorized Explorer client. Recording is mandatory and fail-closed. Mutations
receive a durable admission record before dispatch; if their post-effect
outcome record fails, the response is explicitly indeterminate and directs the
caller to inspect current state before considering a retry. The first-party command advertises `shoal.ask`,
`shoal.provenance.{list,inspect,fold,unfold}`, and, when the embedded Fleet
providers are available, `shoal.agent_dispatch` and `shoal.agent_invoke`.
They use the same chat, interaction, registry, and dispatch providers as the
HTTP API; no second reasoning or action pipeline is constructed.
Ask observations preserve the exact complete evidence accepted with the durable
interaction, including citations, nodes, edges, assertions, visibility, and
embedding-space identities. Direct `shoal.retrieve` results also receive an
independent retrieval capture in addition to the generic tool-call record.
Provenance listing is bounded to 100 records by default (1,000 maximum) and
returns an opaque `next_cursor` for both HTTP and MCP callers.
`OperationToolCall` is only a provenance discriminator; the canonical
authorized operation is stored separately, actor/delegation metadata comes only
from the bound decision, and the authorized client derives the hashed
audit-purpose reason. Grounded evidence remains reauthorized with the legacy
`retrieve` operation, while source-free actions need only their exact authorized
action. Fleet action tools must separately enforce that exact `auth.Operation`
before effects.

### Recorded chat and workspace provenance acceptance

`POST /api/v1/ask` and `POST /api/v1/chat/stream` apply the selected
`Shoal-Workspace-ID` settings under the retrieval operation. The stream emits
one structured `complete` SSE event only after durable capture, not speculative
model tokens. Workspace output policies are parsed as conjunctions of labels;
their join with verified evidence is recorded on the chat session and returned
with its structured, verified citations.

The same workspace header applies to provenance list/inspection and unfold
under `read`, and to provenance fold under `connect`. These are provenance
folds, not context compression.

A labelled session or fold is returned only to a reader holding its labels
(#564, #567, #568): to any other reader it is absent from lists and not found
on point reads, exactly as if it had never been written, over HTTP, MCP and
team overview alike. A free-form ingest label is evaluated as its structured
(source, label) policy, granted through the label-grant file (#570). The
`output_visibility` expression is shown only to a reader permitted every term
it names. See "Output labels on interaction and fold reads" in
`docs/explorer-public-contract.md`.

The following local acceptance matrix exercises REST ask, SSE chat, and HTTP
MCP ask with 24 cited documents, a nonpublic workspace output policy, and
provenance inspection/fold/unfold. It verifies complete citation sets and
withholds output when retrieval, inference, chat capture, or the MCP outcome
record fails. It uses embedded Shoal storage and a deterministic test model;
no model service or live cluster is required.

```bash
go test ./pkg/explorer/mcp ./pkg/explorer/webapi -run 'TestHTTPAskAdapterUsesSharedChatAndPersistsCompleteEvidence|TestHTTPChatSurfacesWithholdUnrecordedOutput|TestObservedEvidenceNodeSetsIgnoreOrdering|TestWorkspaceOperationForRequestUsesRouteOperation' -count=1
```

## Durable Fleet registry, dispatch, and events

The embedded command composes the durable registry and dispatch services on
the same transaction runtime, mounts their combined handler once at
`/api/v1/fleet/`, and mounts the more-specific event subtree at
`/api/v1/fleet/events/` through the same authenticated workspace handler.
Register, heartbeat, revoke, resolve, list, enqueue, claim, cancel, status,
pull, invoke, subscription, publication, and delivery operations use the same
bound authorization resolver. Missing recorder, snapshot, generation, cursor,
registry, dispatch, or event dependencies fail startup rather than advertising
partial functionality. Every
privileged mutation first records an `OperationToolCall` interaction carrying
the exact `agent_*` authorization operation, decision fingerprint/expiry, and a
fresh trusted interaction snapshot. The adapter supplies no actor or reason;
the authorized interaction client derives trusted identity fields and validates
the exact persisted receipt.

Registry list requests accept an optional `limit` (default 25, maximum 32) and
opaque `cursor`. Responses return at most that many authorized descriptors and
an optional `next_cursor`; filtering advances through a bounded number of
stored descriptors per request.

Executor references are host-owned opaque capabilities. Configure their
allowlist with `-fleet-executor-refs` or `SHOAL_FLEET_EXECUTOR_REFS` as a
comma-separated list. An empty list is valid but fail-closed: registry reads
remain available while new registrations are rejected because no executor is
known to the host.

Allowlisting a reference does not grant it any effect authority. An allowlisted
reference declares no effect ceiling, and a declaration that is not a subset of
the ceiling is refused — so a descriptor declaring any effect class at all is
rejected against it, at registration and again at resolution. Two settings
raise that ceiling, each per reference and each an explicit opt-in:
`-fleet-external-executor-refs` (`SHOAL_FLEET_EXTERNAL_EXECUTOR_REFS`) binds a
ceiling of `{external}`, and `-fleet-external-egress-executor-refs`
(`SHOAL_FLEET_EXTERNAL_EGRESS_EXECUTOR_REFS`) binds
`{egresses-content, external}` for an external operation that also transmits
corpus content off this host. Neither has a default and nothing else produces
either ceiling.

A reference bound this way carries **no effect floor**, so a descriptor may
declare less than the ceiling permits, and it implements no action execution:
nothing runs in process against it. Work reaches it over the dispatch queue —
enqueue, pull, claim, perform elsewhere, report — and the completion report is
what Shoal records. This is the point at which Shoal stops containing an
effect; it dispatches the work and does not undo it.

Each such reference must also appear in `-fleet-executor-refs`, must appear in
at most one of the two lists, and must not be `-fleet-ask-executor-ref`. The
grounded-reasoning executor's effect floor equals its ceiling and deliberately
excludes external mutation, so one reference cannot carry both bindings; give
the gateway its own reference. The chart refuses all three at render time.

Dispatch transitions publish stable `action.enqueued`, `action.claimed`,
`action.completed`, `action.canceled`, and `action.failed` events. Raw
idempotency, claim, executor, and cancellation keys remain private; envelopes
carry the producer generation and an opaque hashed transition identity.
Lifecycle publication reauthorizes the original exact `dispatch` or `invoke`
operation and retains complete representable evidence. Subscription delivery
uses the dedicated `subscription_deliver` operation and rechecks the shared
policy-generation authority during long polls. Delivery also drops, per
subscriber, every evidence reference that subscriber may not see, whole and
with no count (#562, under #398's rule); the stored event keeps them.

The same per-reference rule (`evidencelabels.FilterReferences`, #564) decides
every dispatch read that returns a record (Status, the Pull and TeamActions
pages, and the enqueue, invoke and approval replays) and every delivery:

- A reference that names nodes or edges is decided by their **current**
  access rules (`authorized.NodeGate`): every node, every edge's effective
  rule (an extracted relation is bound to the document that asserted it), and
  both endpoints of every edge, the same rules interaction reads re-check. A
  document reference also needs its cited revision's own rule (and the
  current revision's, as historical document reads do): section and span
  identities do not change with the revision, so an unlabelled current
  revision must not open a labelled historical one. A loosened document
  therefore does not release evidence citing a revision that was labelled.
  A node, edge or revision the catalog does not know withholds the
  reference. Every reference on a page or delivery is decided in one batch.
  At record time a graph reference is refused as `invalid_executor_evidence`
  unless its edges run between its nodes in sequence and its assertions are
  exactly the corpus's at the executor's pinned snapshot, as the interaction
  recorder requires. A
  document relabelled after the action ran therefore governs its evidence:
  tightened, a holder of only the old labels loses it; loosened, a reader of
  the new rule gains it. The labels stored with the reference are provenance
  only.
- A reference that names no node or edge has nothing current to consult and is
  decided by its stored labels (`authorized.LabelVisibility`).

`shoal-explore-web` wires the authorized client's gate and evaluator into
`fleetevents.Config` and, through `explorerfleet.ComposeDispatchWithAttestations`,
into dispatch. A host that wires no gate withholds every reference naming a
node from every reader. When an executor's evidence is recorded, the free-form
label it reports is replaced by the structured terms of the label policy
enforcing it (`authorized.LabelTranslator`), so the stored provenance names
what was enforced at the time.

Fleet event cursors are AES-GCM-protected, restart-stable, and scoped to the
subscription and authorization identity. Event and subscription storage use
4,096 bounded logical slots with an event-local durable retention floor.
Expired publication retries fail closed. Historical physical cell-version
cleanup remains the responsibility of normal Explorer storage compaction; the
event subsystem does not advance a shared allocator history floor.

## Host authority (required for a public bind)

Every request must present a `Host` header (HTTP/1.1) or `:authority` (HTTP/2)
that matches an allow-list, enforced centrally before routing or handling. The
match is exact: the hostname compares **case-insensitively** and the port
**exactly**. There is **no wildcard, no suffix, and no `X-Forwarded-Host` or
`Forwarded` matching** — each of those is a classic host-authority bypass. A
mismatch is refused with `421 Misdirected Request` and a fixed body that never
echoes the submitted host. This bounds cache poisoning, absolute-URL/redirect
poisoning, virtual-host confusion, and DNS rebinding against a private-network
listener.

| Flag / environment fallback                | Purpose                                                                   |
| ------------------------------------------ | ------------------------------------------------------------------------- |
| `-allowed-host` / `SHOAL_ALLOWED_HOST`     | Comma-separated exact-match list of external authorities (`host` or `host:port`). |

**Default when unset — the resolved listen address (fail-closed).** A loopback
bind (`127.0.0.1:8098`) therefore serves only requests whose `Host` is that
loopback authority, preserving the local-first posture with no configuration.
A **non-loopback or wildcard** bind (`0.0.0.0:<port>`) resolves to a socket
address real client `Host` headers never carry, so such a deployment **refuses
every request until `-allowed-host` names the external authority** the proxy or
client actually sends. That is deliberate: a public bind fails closed rather
than silently accepting any `Host`.

A deployment behind a reverse proxy that legitimately answers to more than one
name lists each explicitly, comma-separated (for example
`-allowed-host explorer.example.test,explorer-internal.example.test`). Because
TLS terminates at the proxy, the value is usually the port-less external name
(for example `explorer.example.test`), matching the `Host` the browser sends on
the default port; include a port only when the client sends one.

`X-Forwarded-Host` is **never** trusted. A proxy story that requires honouring
a forwarded host is out of scope here (it needs an authenticated hop and an
explicit trust boundary) and is not implemented.

A `Host` in FQDN-root form with a single trailing dot (`explorer.example.test.`)
is treated as equal to its non-rooted spelling — the dot is normalised away on
both the configured and the request side, so either form matches either. Without
that normalisation the rooted form would fail closed (a `421`, not a security
hole); it is normalised only to avoid a confusing outage if a client sends it.

When `-listen` binds a non-loopback or wildcard address and `-allowed-host` is
unset, the command prints a one-time startup **WARNING** naming the bound
address and the `421` consequence, before any request is served. This is the
most common way to trip over the gate; refusals themselves are not logged
per-request, because the `Host` is attacker-controlled and would invite a log
flood.

## Kubernetes: the Helm chart

`deploy/helm/shoal` renders the workspace as a StatefulSet with a Service, a
PodDisruptionBudget and a persistent state volume. The `explorer` values block
is off by default and independent of the chart's `mode`, which selects a storage
topology the workspace does not depend on.

```console
$ cp deploy/helm/shoal/values-explorer.yaml my-explorer-values.yaml
$ helm upgrade --install shoal deploy/helm/shoal -f my-explorer-values.yaml \
    --set explorer.image.repository=ghcr.io/YOUR_ORG/shoal-explore-web \
    --set explorer.image.tag=TAG
```

The profile **does not install as shipped**. Every value it leaves empty is one
the chart refuses to render without, because each is a setting whose absence
produces a workspace that starts and then denies or answers nothing — the
failure mode this guide's unsafe-configuration section exists to prevent, moved
from a pod log to `helm template`. Working through the errors in order fills the
profile.

The refusals:

| Setting | Why the chart will not render without it |
| --- | --- |
| `explorer.auth.mode: oidc` | The only valid value. `-dev-auth` is refused on any non-loopback listener, and a pod reached through a Service must bind one, so a chart could only render it into a workspace that cannot start. |
| `explorer.allowedHosts` | An empty allow-list answers every request `421` (see host authority, above). |
| `explorer.auth.oidc.issuer`, `audiences`, `authorizationClaim` | Each is required for token validation; a blank or whitespace-only value is treated as missing, because the workspace drops empty entries and then reports the setting absent. |
| `explorer.auth.oidc.approverMapping.identity_claim` not restating `explorer.auth.oidc.identityClaim` exactly, an `identityClaim` that is not a list of non-blank segments, or an `approverMapping` that is not a map | The workspace refuses to start when the mapping and the flag disagree, and a dotted string is one claim name, never a path. |
| one of `readerValues` / `contributorValues` / `fleetValues` | A claim mapped to nothing denies every authenticated caller, which is fail-closed but indistinguishable from an outage. |
| `explorer.replicas` above 1 | The corpus, workspace settings and policy catalog share one state root on a `ReadWriteOnce` volume, with no coordination protocol between two processes over it. |
| a remote chat or embedding provider without a credential Secret | The credential is read at request time and nothing projects it into the pod. |
| `explorer.auth.oidc.labelGrants` that is not a map, with a version other than `shoal.label-grants/v1`, an `issuer` other than `explorer.auth.oidc.issuer`, no grants, a grant on a source other than `shoal-explore-web/workspace`, or a label outside the label charset | The workspace refuses each of these at startup. |
| `explorer.auth.oidc.executorMapping` that is not a map, with a version other than `shoal.executors/v1`, a non-canonical or non-`https` issuer or one containing `#` or a query, an audience shared with the human audiences, no positive service assertion, or blank, duplicate or out-of-charset entries, or an `executor_ref` missing from `explorer.fleet.executorRefs` | The workspace refuses the first group at startup, and a reference no descriptor can register against binds nothing. |
| any value still containing `REPLACE_ME` | A placeholder is not configuration. |

Two chart choices follow from this document rather than from Kubernetes
convention. It is a **StatefulSet** not for ordinal identity but because its
rolling update stops the old pod before starting the new one, where a Deployment
surges by default and would put two processes on the one state root the
split-brain guard exists to prevent. And **every setting is a container
argument**, not a ConfigMap, so a changed setting rolls the pod on its own; a
ConfigMap without a checksum annotation would leave the running process on the
old disclosure posture while the chart claimed the new one.

The approver mapping is the one exception, because the workspace reads it as a
file. `explorer.auth.oidc.approverMapping` holds the `shoal.approvers/v1`
document as a map; the chart renders it as JSON into a ConfigMap it owns,
mounts it read-only at `/etc/shoal/approvers`, passes
`-oidc-approver-mapping-file`, and puts its checksum on the pod template so a
changed mapping rolls the pod. `explorer.auth.oidc.identityClaim` is a list of
path segments rendered as the `-oidc-identity-claim` argument, and the mapping
must restate it as `identity_claim`. `explorer.auth.oidc.identitySchemeMigrateFrom`
renders `-oidc-identity-scheme-migrate` with the digest of the recorded scheme
being replaced; it can move the record only once, so unset it after the
rollout (`docs/approval.md`).

The label grants follow the same pattern. `explorer.auth.oidc.labelGrants`
holds the `shoal.label-grants/v1` document as a map; the chart renders it as
JSON into a ConfigMap it owns (`<release>-label-grants`), mounts it read-only
at `/etc/shoal/label-grants`, passes `-oidc-label-grants-file`, and puts its
checksum on the pod template so a changed grant rolls the pod.

So does the executor mapping. `explorer.auth.oidc.executorMapping` holds the
`shoal.executors/v1` document as a map; the chart renders it into
`<release>-executors`, mounts it read-only at `/etc/shoal/executors`, passes
`-oidc-executor-mapping-file`, and puts its checksum on the pod template
(`docs/effects-gateway-deploy.md`, "Issuing executor credentials").

Probes address the health port below, never the workspace port.

The same chart renders the LLM gateway, under its own `llmGateway` block and also
off by default — see `docs/llm-gateway-deploy.md`. It is documented separately
because almost every deployment decision in this guide follows from one binary
over one state root served by one replica, and the proxy has the opposite
properties: no state root, several replicas, and a rolling update that is
allowed to surge. It is a **Deployment** for exactly the reason this one is a
StatefulSet.

Before changing the chart, run its checks:

```console
$ deploy/helm/validate-chart.sh
```

They render every profile, schema-check the output, and assert that each guard
above still refuses and each valid configuration still renders. A guard that
silently stops firing is the failure they exist to catch.

## Upgrading: re-register reasoning descriptors

The effect taxonomy became a set of classes — `reads-corpus`,
`egresses-content`, and external mutation, spelled `external` on the wire and
in `effects` — replacing a two-value split that could only describe mutation. Durable records decode without a migration: a
descriptor written before the change keeps its stored meaning, and the HTTP
surface still accepts the superseded `"effect": "external"` spelling alongside
the current `"effects": ["external"]`.

**One thing does not survive the upgrade, deliberately.** A descriptor
registered against the built-in reasoning executor declared the old evidence
value, which decodes to an empty set. That executor now declares what invoking
it *always* does — it reads the corpus, and against a hosted model provider it
transmits what it read — and an action may not declare less than that. So those
descriptors are refused until re-registered with their effects declared.

This is the control working rather than an upgrade defect. Such a descriptor
genuinely understates what running it does, and it says nothing only because
the taxonomy it was written under could not say anything else. Accepting it
would keep exactly the descriptors the check exists to reject: one that reads
as non-transmitting while every call ships corpus content to a third party.

The refusal names the missing classes:

```console
invalid_argument: action omits effects its executor causes on every invocation
(egresses-content, reads-corpus); the declaration must not understate what
running it does
```

Re-register with those classes in `effects`. What to declare depends on where
`-chat-base-url` points, not on the action — a loopback provider transmits
nothing — so take it from the executor rather than hardcoding it.

Descriptors bound to any other executor are mostly unaffected — an executor
that declares no floor imposes none — with one narrow exception, below.

**A second thing does not survive, for the same reason in a different shape.**
An action that declares *nothing at all* is now refused when its executor's
reference is bound to a ceiling that permits reaching outside Shoal
(`external` or `egresses-content`) and declares no floor. That is the
dispatch-only shape: the host has said work on this reference may reach
outside, and declined to say when, so Shoal cannot know. A completion against
such a reference reported `effect_possible: false` for an action that declared
nothing, which reads as "no external effect was declared for work Shoal never
performed" while a remote worker may have done anything the ceiling permits
(#510, #514).

The refusal names the classes the ceiling permits:

```console
invalid_argument: action declares no effects at all and is bound to an executor
that may reach outside Shoal (external) without declaring when; declare what
this action does — any class will do, including reads-corpus alone if it
reaches outside nothing
```

Note what this does **not** require: an external class. A dispatch-only action
may legitimately reach outside nothing — a remote worker reading Shoal's own
corpus is the common case — and forcing it to claim an external effect would
make `effect_possible` true where it should be false, which is the same false
claim in the opposite direction. Any one class satisfies the requirement. What
is refused is silence.

**Already-registered descriptors keep running.** This is a registration
boundary, not a resolution one: a stored descriptor declaring nothing still
resolves and still completes, so an upgrade does not strand in-flight work or
stop a running fleet. It is refused the next time it is registered or updated,
and `shoal-atpl compile`/`plan` refuse the same shape before apply, so an
operator sees it in a plan rather than at apply time. Nothing rewrites stored
records.

Residual gap, stated rather than implied: an action declaring `reads-corpus` on
such a reference still completes with `effect_possible: false` while the worker
may do anything the ceiling permits. Closing that needs either a floor on the
binding or a per-action external declaration in the manifest; neither is in
this change.

## Orchestrator probes: the health surface

The host-authority gate above runs before routing, before authentication, and
before anything else. That is correct for the workspace, and it makes the
workspace port unusable as a probe target under an orchestrator: a Kubernetes
kubelet addresses a pod by its runtime-assigned IP, which no static
`-allowed-host` list can name, so every probe answers `421` and the pod never
becomes ready.

`-health-address` (environment fallback `SHOAL_HEALTH_ADDRESS`) opens a second
listener for exactly this. Empty disables it, so a local or compose deployment
is unchanged.

| Route     | Meaning                                                                 |
| --------- | ----------------------------------------------------------------------- |
| `GET /healthz` | The process is up. Stays `200` throughout a drain.                 |
| `GET /readyz`  | The workspace is serving. `503` before it serves and from the moment shutdown begins. |

Both answer with a status code and a fixed string. Neither reads the corpus,
the policy catalog, the authenticator, or the request, so binding this port
where the workspace port may not be bound discloses nothing beyond the fact
that a Shoal process is listening — which the open socket already says. Any
other path is a `404`; the surface is two routes and cannot grow by accident.

The shared health surface (`internal/healthsurface`) can also serve
`GET /metrics` (Prometheus text format, `no-store`, GET/HEAD only) on this same
listener, but only when the binary supplies a metrics writer through
`healthsurface.Config`. The Explorer does not supply one yet, so `/metrics` is
a `404` here today. A writer that is supplied answers to the same
unauthenticated audience as the probes, so it may carry operational counts
only. A writer that panics answers `500`, and a scrape in flight when shutdown
begins is answered `503` rather than holding the drain.

The split between the two routes is what makes a rolling update safe.
Readiness drops **before** the workspace stops accepting, so the endpoints
controller removes the pod from the Service while it is still finishing
in-flight requests. Liveness does not drop, because a pod shedding traffic on
purpose has not failed, and restarting it would throw away the graceful close.

The listener is opened after the corpus and policy catalog are open. That
ordering matters: a probe that connects at all already means construction
finished, so there is no window in which a TCP check reports ready while the
workspace is still opening its corpus. The workspace listener cannot offer that,
because it is deliberately bound early so an address the workspace may not serve
is refused before any state is touched.

A `-health-address` that cannot be bound is fatal. An operator who asked for a
probe surface and silently did not get one would read every probe failure as
the workspace being down.

```console
$ shoal-explore-web -listen 0.0.0.0:8098 -allowed-host explorer.example.test \
    -health-address 0.0.0.0:8099 ...
Shoal Explorer listening at http://0.0.0.0:8098
Health surface listening at http://0.0.0.0:8099

$ curl -s -o /dev/null -w '%{http_code}\n' -H 'Host: 10.1.2.3:8098' http://10.1.2.3:8098/api/v1/auth-config
421
$ curl -s -o /dev/null -w '%{http_code}\n' -H 'Host: 10.1.2.3:8099' http://10.1.2.3:8099/readyz
200
```

## The unsafe-configuration guard

The dangerous combination — a shared/public bind with the development
authenticator — **fails closed at startup** before any socket is bound. This is
enforced by `selectAuthenticator` / `listenAddressIsLoopback` in
`cmd/shoal-explore-web/authn.go`; it is verified here, not merely asserted.

Public bind + `-dev-auth` is refused:

```console
$ docker run --rm --network host shoal-explore-web:test \
    -state-dir /var/lib/shoal -listen 0.0.0.0:8099 -dev-auth
shoal-explore-web: refusing to serve 0.0.0.0:8099 with -dev-auth: the
development-principal@localhost development principal is granted the whole
workspace corpus and is only safe on a loopback listener; bind 127.0.0.1 or
[::1], or supply a real authenticator
# exit code 1
```

No authenticator at all is also refused (the workspace never serves anonymously):

```console
$ docker run --rm --network host shoal-explore-web:test \
    -state-dir /var/lib/shoal -listen 127.0.0.1:8099
shoal-explore-web: refusing to serve 127.0.0.1:8099 without authentication: no
authenticator is configured; pass -dev-auth to mint the
development-principal@localhost development principal on a loopback listener, or
supply a real authenticator before exposing the Explorer API
# exit code 1
```

The check runs twice: once on the requested flag value before anything binds,
and again on the listener's *resolved* address (which may be wider than
requested), closing the listener before the corpus is opened.

## Persistence: corpus and authorization both survive

Be precise about which directories were mounted and what survived. Three facts
are distinct:

1. **The volume preserved the corpus bytes on disk.**
2. **The workspace served that corpus after restart.**
3. **The authorization registrations survived — not rebuilt from scratch.**

With PR #288 merged the policy catalog is durable, and `-state-dir /var/lib/shoal`
places `corpus/` and `policy/` under one mount, so all three hold.

### Observed under `-dev-auth` (the container / compose profile)

**Mount:** the named volume `shoal-explore-state` at `/var/lib/shoal` (the state
root); `-state-dir /var/lib/shoal`.

```console
$ docker run -d --name shoal-explore-test --network host \
    -v shoal-explore-state:/var/lib/shoal shoal-explore-web:test
# first-start log:
#   Granted 0 pre-existing document(s) in /var/lib/shoal/corpus to development-principal@localhost ...
#   Shoal Explorer listening at http://127.0.0.1:8098

# seed one document
$ docker run --rm --network host -v "$PWD/seed:/seed:ro" curlimages/curl -s \
    -X POST http://127.0.0.1:8098/api/v1/ingest \
    -H "X-Shoal-Workspace-Request: 1" \
    -F "file=@/seed/seed.md;type=text/markdown"
{"snapshot":{"id":"23f7a8e4cc666a53...", ...}}
# before restart: seed.md served

$ docker restart shoal-explore-test
# post-restart log:
#   Granted 0 pre-existing document(s) in /var/lib/shoal/corpus to development-principal@localhost ...
#   Shoal Explorer listening at http://127.0.0.1:8098

# after restart: seed.md still served
$ docker run --rm --network host -v "$PWD/seed:/seed:ro" curlimages/curl -s \
    -X POST http://127.0.0.1:8098/api/v1/documents \
    -H "X-Shoal-Workspace-Request: 1" -H "Content-Type: application/json" \
    -d '{"page":{"limit":10}}'
{ ... "documents":[{"document":{"title":"seed.md", ...}}]}

# both directories are present on the mounted state root:
$ docker run --rm -v shoal-explore-state:/state --entrypoint sh curlimages/curl \
    -c "ls -1 /state; echo ---; ls -1 /state/corpus; echo ---; ls -1 /state/policy"
corpus
policy
---
_shoal_explorer
---
_shoal_policy
```

Note the post-restart log: **`Granted 0 pre-existing document(s)`**. On the
pre-#288 build this line read `Granted 1` — the in-memory catalog had lost the
registration and the backfill re-created it. Now it is `0`: the durable policy
store already held the registration, so the backfill had nothing to migrate. Both
`corpus/` and `policy/` are present on the single mounted volume.

**Caveat, stated plainly:** this profile runs `-dev-auth`, and the split-brain
guard is **bypassed whenever the dev backfill is active**. A green restart test
*under `-dev-auth`* does not by itself demonstrate that a production container
survives a restart, because the backfill would re-register a lost corpus and mask
the loss. The `Granted 0` line is good evidence the durable store carried the
registration, but the authoritative proof of production behaviour is the guard
plus the integration tests below.

### Production-mode proof (backfill disabled)

Production mode uses the OIDC authenticator; running it **without** either
`-dev-auth` or an `-oidc-*` configuration still fails closed before the corpus
is opened, because no authenticator is configured. Observed:

```console
$ docker run --rm --network host -v shoal-explore-state:/var/lib/shoal \
    shoal-explore-web:test -state-dir /var/lib/shoal -listen 127.0.0.1:8098
shoal-explore-web: refusing to serve 127.0.0.1:8098 without authentication: no
authenticator is configured; pass -dev-auth ...
# exit code 1
```

So the durable-persistence and split-brain behaviours are proven where they are
reachable — the package's integration tests, which drive `openService` with
`backfill: nil` (production):

- **`TestStateDirLayoutSharesOneMountPoint`** — ingests under a `-state-dir` root,
  reopens with the backfill disabled, and serves the document after the reopen;
  the guard does not fire because `corpus/` and `policy/` shared the mount. This
  is fact 3 in production mode: **the authorization registrations survived on
  their own.**
- **`TestOpenServiceRefusesSplitBrainStateDirectory`** — ingests, removes the
  policy directory (a lost/unmounted policy volume), reopens in production, and
  asserts the guard refuses.

```console
$ go test ./cmd/shoal-explore-web/ -run 'SplitBrain|StateDir' -v
--- PASS: TestOpenServiceRefusesSplitBrainStateDirectory (0.06s)
--- PASS: TestStateDirLayoutSharesOneMountPoint (0.06s)
```

### The guard message (what a lost policy volume produces)

Captured by driving the real `openService` guard path (ingest under a state root,
delete `policy/`, reopen with `backfill: nil`):

```text
refusing to serve a split-brain workspace: corpus <root>/corpus holds 1
document(s) but the durable policy catalog <root>/policy holds no authorization
registrations. This is the signature of a lost or unmounted policy volume; every
registration was dropped and the workspace would serve an empty or
under-populated corpus. Restore the policy directory from the same volume as the
corpus (use -state-dir so both persist under one mount), or, for a corpus
ingested before the catalog was durable, run once with -dev-auth on a loopback
listener to re-register it (issue #284)
```

### Did the guard fire in the restart test?

**No — and that is expected.** The container restart test runs `-dev-auth`, which
activates the dev backfill and bypasses the guard by design. The guard protects
the production path (`backfill == nil`), which is currently unreachable from the
container CLI (it refuses earlier, above) and is exercised by the two tests. If a
future production deployment mounts only the corpus directory, the guard turns
that silent misconfiguration into a hard startup refusal with the message above.

## Shared / cloud instance: shape and open gaps

The image is shared-instance ready; the *runtime prerequisites* are not all in
place on `main`. Honest gaps, none of which this deployment work should paper
over:

1. **The provider-neutral OIDC authenticator is wired (issue #315).** Supply the
   `-oidc-*` flags (see [Provider-neutral OIDC authentication](#provider-neutral-oidc-authentication))
   and a non-loopback listener is allowed. Without either `-dev-auth` or an
   OIDC configuration, startup still fails closed demanding an authenticator.
2. **`-backend remote` is deliberately refused** because `auth.Decision` has no
   on-the-wire representation (issue #278): forwarding would authenticate at the
   edge and then call upstream with no identity. This means **multi-node scaling
   is out of scope**; the target is single instance, many users.
3. **Host-authority binding is configurable (this change).** The handler
   requires the request `Host`/`:authority` to match an exact allow-list. It
   defaults to the listener's resolved address — so a loopback bind is served
   with no configuration — and a shared instance behind a reverse proxy sets
   `-allowed-host` / `SHOAL_ALLOWED_HOST` to the external name(s), decoupling the
   served authority from the wildcard socket address. See
   [Host authority](#host-authority-required-for-a-public-bind). Until it is set,
   a public bind of `0.0.0.0:<port>` fails closed (every request `421`s),
   because the resolved authority `0.0.0.0:<port>` never matches a real client
   `Host`.
4. **Durable policy store (#284 / PR #288) has landed.** The policy catalog now
   persists, and a split-brain guard refuses to start when the corpus holds
   documents but the policy catalog is empty (a lost policy volume). Use
   `-state-dir` so the corpus and policy always persist under one mount.

A public bind is now end-to-end serviceable behind a proxy: run the OIDC
authenticator and set `-allowed-host` to the external name the proxy forwards.
Omitting `-allowed-host` on a public bind is a deliberate fail-closed posture —
requests are refused until the external authority is declared — not a packaging
defect.

## Azure hosting: App Service for Containers, single instance

The hosting shape **is** decided, and the deciding factor is exactly the one this
guide has been building toward: a **persistent, single-writer volume** that
survives restarts and redeploys without ever running two writers against the
embedded store. The full decision, evidence (tagged verified / documented /
inferred), and the deployable Bicep live under
[`deploy/shoal-explore-web/azure/`](../deploy/shoal-explore-web/azure/README.md).

**Chosen: Azure App Service for Containers, one instance, deploy stop-first.**
The short version of why:

- **Container Apps is rejected.** In single-revision mode it uses zero-downtime
  deployment, so a new revision is brought up healthy **before** the old one is
  torn down — the two overlap — and its only persistent storage is Azure Files,
  which "multiple containers can mount … in another replica, revision, or
  container app." For a single-writer local store that overlap is data
  corruption you cannot switch off. (Microsoft Learn: Container Apps application
  lifecycle; storage mounts.)
- **App Service is chosen.** It also warm-swaps a new container in before
  stopping the old, **but** it has a first-class `az webapp stop` / `start`, so a
  deploy can force the old writer fully down before the new one starts. Its
  bring-your-own Azure Files mount attaches at the state root `/var/lib/shoal`
  (App Service forbids mounting at `/` or `/home`, but `/var/lib/shoal` is fine).
  Single instance is `capacity: 1` + `numberOfWorkers: 1` + no autoscale.
- **AKS is rejected as overkill.** A `replicas: 1` StatefulSet on a
  ReadWriteOnce Azure Disk is the *only* option that enforces one writer at the
  storage layer, and it is the documented escalation path if the Azure Files
  risks below bite — but running a Kubernetes cluster to host one binary is not
  justified for this workload.

Two Azure Files risks are sized honestly in the artifact README. The genuine
gate is the non-root uid `65532` writing an SMB mount (a first-boot check settles
it). The SMB-semantics worry is **largely retired by evidence**. This binary has
no memory-mapped I/O anywhere in the tree. It does take file locks:
`internal/dirlock` (flock on Linux, LockFileEx on Windows) holds an exclusive,
non-blocking lock on the corpus directory (`.shoal-explorer-runtime.lock`, which
also covers the workspace settings sharing its engine) and on the policy catalog
(`.shoal-policy-catalog.lock`, #686) for as long as each is open. (`internal/promotion`'s blocking flock is not
in `go list -deps ./cmd/shoal-explore-web` on either OS.) These locks fail
closed. The Linux CIFS client implements flock as an SMB byte-range lock, which
the file server enforces for every client. On a mount where flock is not
supported, the lock call fails and the workspace refuses to start, reporting
that error itself rather than "already open". So on SMB the risk is a failed
first boot, which the same first-boot check catches, not two writers sharing a
store. Azure Files SMB is a reasonable default, with NFS or AKS + Azure Disk
kept as escalation paths only if a specific problem shows up. TLS terminates at the App Service front end and the
app runs the OIDC authenticator (`-oidc-*` / `SHOAL_OIDC_*`). The
host-authority gate (gap #3) is closed by **PR #295** (merged to `main` at
`3670e00`): the template sets `SHOAL_ALLOWED_HOST` to the App Service hostname
(or your custom domains) so the public bind is serviceable, not refused with 421.
See the artifact README's host-authority section for the wiring and the honest
defence-in-depth caveat.
