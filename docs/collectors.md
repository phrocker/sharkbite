# External collectors

This is slice 1 of #446. It lets an out-of-process collector register as an
evidence source, report raw artifact references, and attribute each
observation to the extractor version that produced it, through public
contracts only. It follows "Core and extensions" in `docs/gateways.md` and the
authority rules of #419.

Nothing here is mounted in an existing binary. A host constructs the registry
and calls `MountCollectors` itself, as for decisions (`docs/decision-http-api.md`).
Collector administration over HTTP (slice 5, [Administration](#administration))
is a separate, opt-in mount, `MountCollectorAdmin`.

## Packages

| Package | Role | May import |
| --- | --- | --- |
| `pkg/collector` | Contracts: `Registration`, `EnrollRequest`, `ExtractorRef`, `ArtifactRef`, `Observation`, `Confidence`, `AttestationReport`, `AttestationResult` | stdlib, `pkg/shoal` |
| `pkg/collector/api` | Wire types, strict decoding, Go client | `pkg/collector`, `pkg/shoal` |
| `pkg/admission/api` | Admission seam wire types (request, grant, report, receipt, outstanding page, error), limits and a Go client that accepts a path-prefixed base URL; see `docs/admission-seam.md` | stdlib, `pkg/shoal` |
| `pkg/attestation/api` | Executor attestation presentation wire types, limits and a Go client; see `docs/executor-attestation.md` | stdlib, `pkg/shoal` |
| `pkg/dispatch/api` | Worker-facing dispatch wire types (pull, claim, extend, complete, ambiguity, status, descriptor resolve), limits and a Go client that accepts a path-prefixed base URL and carries the resend rules for lost answers | stdlib, `pkg/shoal`, `pkg/executorref` |
| `pkg/sdk` | Versioned facade: `sdk.New(...).Collectors()`, `.Decisions()`, `.Admission()`, `.Attestation()` and `.Dispatch()`; `sdk.ProtocolVersion = 1` | `pkg/shoal`, `*/api` |
| `internal/collectorregistry` | Engine-backed registry, `Provider` wire adapter, `Source` adapter | core |
| `internal/collectorattest` | Attestation verifiers | core |
| `pkg/explorer/webapi` (`collectors.go`, `collector_admin.go`) | HTTP routes | core |

`pkg/decision` itself is not an extension dependency: it pulls `internal/`
packages transitively. Extensions use `pkg/decision/api`.

## Authority

Authority is assigned only by provisioning: `Registry.Provision`, a trusted Go
API, or its HTTP counterpart `Registry.AdminProvision` behind
`auth.OperationCollectorAdmin` ([Administration](#administration)). Either
binds a collector ID to one authenticated principal (subject, client ID and
authorization domain), an authority ceiling (a set of authority policy IDs), a
`Control` and a `Mode`.

- `Control` mirrors `decision.Control` by value (`candidate_controlled`,
  `external_controlled`, `registry_controlled`, `unknown`). It says who
  controls the observed content; it is not a trust level.
- `Mode` is `server_observed` or `imported`: whether the collector saw the
  activity happen or imported records produced elsewhere.

Enrollment (`POST /api/v1/collectors/enroll`) states the authority subset the
collector wants, the extractors it runs and optionally an attestation report.
A request for any authority ID outside the ceiling is **refused**
(`403 permission_denied`) and nothing is written; it is never trimmed to fit.
Control, mode and the ceiling are not request fields; a body carrying them is
rejected as unknown fields. No field carries a model score, and extraction
confidence never maps to authority.

A collector's identity (authorization domain, subject and client ID) is fixed
for the life of its collector ID. Provisioning an existing ID with a different
identity is a conflict, even after revocation; use a new collector ID. Revoked
collectors may be re-provisioned with new authority, control or mode under the
same identity.

Only the provisioned principal can enroll or submit for a collector. Any other
principal, including a delegated decision for the right subject, gets
`404 not_found`, as if the collector did not exist. Writes require
`auth.OperationIngest`; reads require `auth.OperationRead`. Collector admin
implies neither: an admin token cannot enroll or submit.

## Artifacts and observations

`POST /api/v1/collectors/artifacts` records an `ArtifactRef`: collector-chosen
ID, SHA-256 digest, size, media type and observed time. Shoal records the
reference only; retaining the bytes is #447.

`POST /api/v1/collectors/observations` records an `Observation`. Its ID is
derived (`collector.NewObservation`) from the collector, the artifact, the
extractor ID and version, and the content (subject, kind, payload, confidence,
observed time). Consequences:

- A new extractor version over the same artifact is a new observation with
  the same artifact lineage. Earlier observations are never overwritten.
- Resubmitting identical content returns the original receipt.

An observation is accepted only from the collector's provisioned principal,
only for an extractor its current enrollment declares, and only over an
artifact that same collector recorded in its current generation.

Artifacts are stored per generation: an artifact ID used in an earlier
generation is invisible to the next one, which records its own reference.
Every artifact and observation row also stores the authorization domain it was
written in.

`Confidence.Disposition` is `extracted`, `low_confidence` or `unextractable`,
with an optional value in [0, 1]. It is how sure the extractor is that it read
its input, not a judgement about the content.

## Revocation and quarantine

`Registry.Revoke` (trusted Go API) or `Registry.AdminRevoke` (HTTP, below)
increments the collector's generation and marks it revoked. Every artifact and observation records the generation it was
written under. `GET /api/v1/collectors/observations/{id}` computes
`status` from the collector's current registration: `quarantined` when the
collector is revoked or the generation differs, otherwise `active`. Rows are
never rewritten. Revoked collectors cannot enroll or submit. Re-provisioning
starts the new generation; the old generation stays quarantined, its
artifacts cannot carry new observations, and resubmitting an identical
observation first recorded in it is refused rather than re-activated.

Any caller with `OperationRead` in the authorization domain stored with an
observation may read it. Reads are authorized against that stored domain, never
against a later registration. Finer read scoping (by authority policy) is not
implemented in this slice.

## Retries

Every write is idempotent and reports `indeterminate` (`503` with
`Shoal-Commit-Outcome: indeterminate`; `api.ErrIndeterminate` in the client)
when the durable outcome is unknown. Retry with the identical request.

- Enroll takes an `Idempotency-Key`. Same key and request return the original
  receipt, including after a later enrollment superseded it and after its
  attestation expired (expiry is applied when observations are mapped, below). A different
  request under the same key, or a key first used in an earlier generation, is
  a conflict. Re-enrolling (new key) within the ceiling replaces the current
  enrollment without changing the generation.
- Artifacts are keyed by collector and artifact ID, observations by their
  derived ID; neither takes an `Idempotency-Key`. A different artifact under an
  existing ID is a conflict.

The three write routes are listed in `requestMayCommit`
(`pkg/explorer/webapi/workspace_settings.go`), so an over-budget response is
reported as indeterminate rather than as a failure. Their receipts carry IDs,
generation, state and receipt time, never submitted payloads or evidence, so
their size does not grow with what was submitted. With every ID at
`shoal.MaxIDBytes` the largest receipts measure 1600 bytes (enroll), 2860
(artifact) and 2982 (observation)
(`pkg/collector/api/receipt_size_test.go`). A workspace `OutputBytes` budget of
at least `api.MaxReceiptBytes` (3072) therefore always returns a readable
receipt. Below the size of a particular receipt, a write that committed
reports indeterminate: safe, because a retry returns the same receipt, but
unreadable under that budget. Content is read back with the GET route, which
commits nothing and is not listed.

## Administration

Slice 5 adds HTTP provisioning, revocation and reads, for hosts that mount
them with `MountCollectorAdmin(collectorregistry.NewAdminProvider(registry),
resolver)`. Without that call the admin paths answer `404` from the
collector handler, exactly as before, and no collector route changes. Through
the SDK: `sdk.New(...).Collectors()` gains `ProvisionCollector`,
`RevokeCollector` and `CollectorRegistration`.

| Route | Body / result |
| --- | --- |
| `POST /api/v1/collectors/admin/provision` | `ProvisionRequest` (collector ID, subject, optional client ID, domain, authority policy IDs, control, mode, generation) → `AdminReceipt` |
| `POST /api/v1/collectors/admin/revoke` | `RevokeRequest` (collector ID, generation) → `AdminReceipt` |
| `GET /api/v1/collectors/admin/registrations/{id}` | `RegistrationRecord`: the registration, its current enrollment ID, and the change log of its last 8 generations (entries newest first, gaps, anomalies) |

IDs and the domain are base64url without padding, as on the collector routes;
bodies are decoded with the same strict rules.

### The operation, and how an operator obtains it

Every route requires `auth.OperationCollectorAdmin` (`collector_admin`) in the
caller's own authorization domain. It assigns every collector's authority, so
nothing that grants anything else grants it:

- no OIDC mapping (reader, contributor, fleet), the approver audience, the
  executor mint or the `-dev-auth` principal of `shoal-explore-web` mints it;
- `shoal-mcp`'s `-identity-operations` refuses it explicitly (that process
  serves no collector route);
- no service role allows it, including migration's legacy wildcard, so no
  service decision can hold it;
- workspace narrowing can only remove operations, so a workspace setting
  cannot add it;
- a delegated decision (any on-behalf-of chain), a service decision and an
  executor-bound decision are refused as admins even if they hold it.

**The operator path** is the host itself. A host that calls
`MountCollectorAdmin` authenticates its admins with an authenticator it
configures and mints their decisions with the operation. That is the same
trust boundary as the Go `Provision` API: operator-controlled code, not a
claim value an identity provider or a token holder can choose.
`cmd/shoal-explore-web/oidc_collector_admin_grant_test.go` pins all of the
above: an inventory of every operation list the command defines, a go/types
scan proving the command's code denotes the operation nowhere, and a mint of
every token kind with `collector_admin` smuggled into its claims. The auth
inventory tests, `cmd/shoal-mcp` and `pkg/explorer/workspace` each assert
their part.

### Rules

- **Own domain only.** `domain` must be the admin's authorization domain
  (`403` otherwise, decided before anything is read). Revoking or reading a
  collector in another domain answers exactly like a collector that does not
  exist (`404`). Collector IDs are a single namespace, so provisioning an ID
  already taken in another domain is a `409`, answered exactly like an identity
  mismatch in the admin's own domain: it reveals only that the ID is taken.
- **No self-provisioning.** A request whose subject is the admin's subject or
  actor is refused (`403`) before anything is read, under any client ID. So an
  admin can neither make itself a collector nor raise, confirm or reactivate a
  registration bound to itself. The check compares identities as the admin's
  own decision states them; a host whose admin and collector authenticators
  can map one person to different subject strings must rule that out itself.
- **The request decides the outcome; nothing is merged.** `generation` is the
  generation the registration holds afterwards. Provisioning creates only at
  generation 1, reactivates a revoked registration only at its current
  generation (with new terms allowed, as in the Go API), and otherwise must
  match an active registration exactly, which is a no-op. Revocation names the
  generation being revoked; revoking it again is a no-op. Everything else is
  `409`: different terms on an active registration (revoke first, which
  quarantines), a different identity, a stale or future generation. So a
  retried provisioning cannot undo a revocation that happened in between.
- **Concurrency** is the registry's existing compare-and-set on the
  registration row; concurrent admins serialize, and the loser of a race
  re-evaluates against what won.
- **Quarantine stays read-time.** Revocation bumps the generation exactly as
  `Revoke` does. Observation status is still computed from the current
  registration at each observation read; nothing caches it, and the admin
  read carries no status.

### Audit

Every provisioning and revocation that changes a registration writes one
immutable entry to that collector's change log. It does so in the **same
one-row compare-and-set mutation** as the registration write, through either
API, so neither is durable without the other.

An entry records:
- the action;
- the registration as written;
- the time;
- its source: `collector_admin` with the admin's subject, actor, client ID,
  domain, request ID, correlation ID and authorization fingerprint, or
  `operator_api` for the Go API, which has no caller identity.

No-op retries and refused requests change nothing and write no entry.
`Registry.Changes` is a trusted Go read of the log. The HTTP read covers the
last 8 generations.

**Slots, not a sequence.** The log has no head and no global sequence. Each
entry has a slot, (generation, action):
- a provisioning that creates a collector writes (1, provision);
- a revocation moves generation g to g+1 and writes (g+1, revoke);
- a provisioning that reactivates a revoked collector at g writes
  (g, provision).

So a registration at generation G implies exactly (1, provision), then
(g, revoke) and (g, provision) for each g from 2 to G. The exception is
(G, provision), which is absent while the collector is revoked. At most one
change can land in a slot, because the registration compare-and-set admits
one.

The entry's qualifier is its slot plus the write attempt's random nonce. The
write is conditioned only on that exact cell being absent. A write therefore
reads nothing else in the log, and no other cell can block it, damaged or
stray. A revocation, and a provisioning, depends only on the collector's own
registration row.

A lost acknowledgement is reconciled by reading back that attempt's exact
cell. An attempt that did not commit wrote nothing, since the entry and the
registration are one atomic mutation, so lost-ack races leave no stray
entries behind.

**Reads check the log against the registration.** A read takes the
registration, scans that collector's entries for the requested generations,
and reads the registration again. If its logged state (generation,
revocation, terms) moved in between, the read retries, and answers `409` if
it keeps moving. So the entries and the registration always describe the
same moment. Enrollment changes the registration without a logged change, so
it never causes a retry. The domain is checked before anything in the log is
read.

The read reports:
- **gaps**: slots the registration implies that hold no entry. Examples are a
  registration from before the log existed, or an entry deleted or hidden by
  a delete marker out of band.
- **anomalies**: cells that are not exactly one valid entry for their slot.
  - `undecodable`;
  - `unknown_version`;
  - `misplaced`: the content does not match the qualifier, or the qualifier
    is not one the registry writes;
  - `unexpected`: a slot the registration does not imply, such as a
    generation above it;
  - `duplicate`: several entries in one slot, none of which is presented;
  - `inconsistent`: the newest slot's entry describes a registration other
    than the current one.

The HTTP read lists at most 64 anomalies, `inconsistent` and `duplicate`
first, and counts the rest in `more_anomalies`. An entry is accepted only
within the wire's own bounds (timestamps in years 1 to 9999, fingerprints of
at most 128 bytes), so anything the registry presents, the HTTP read can
carry.

A lost acknowledgement is resolved by the attempt's own cell even if the
registration has since moved on (an enrollment, say): the cell's presence
proves the change committed.

Gaps and anomalies are reported, never repaired, and they never block a
write. A delete marker cannot pre-empt a write either, because the qualifier
is unknown until the write happens.

**Recorded identities.** An admin decision is refused as an admin (`401`)
before anything is read if any of these fails:
- its subject, actor, client ID or request ID is valid, printable UTF-8
  within `shoal.MaxIDBytes`; spaces are allowed, since OIDC-mapped subjects
  contain them;
- its correlation ID passes `interaction.ValidateCorrelationID`.

Independently, the registry decodes every entry it is about to write, under
the readers' rules, and refuses to write one that would not read back as
itself. So the log never holds an entry its own read API refuses.

**Versioned entries.** Every entry carries a format version. The decoder
reads the version first, then decodes strictly into that version's own
structure. A version it does not know is refused explicitly and reported as
`unknown_version`, never guessed at. Each version decodes into its own frozen storage structure, not into the
live types, so changing those types cannot move a stored entry. The
version 1 bytes are pinned by
`internal/collectorregistry/testdata/change_v1.golden.json`. A later build
adds fields by adding a version, and entries written in an earlier version
stay readable.

**Requirements and limits.**
- Reads need a store with row scans; the engine and Accumulo stores have
  them. Writes need only exact reads and the compare-and-set.
- A read examines at most 512 cells, so a range flooded with stray cells
  reads as unavailable rather than partially.
- On a store that applies a scan's limit before filtering by visibility, a
  page holding only another visibility's cells reads as empty and ends the
  scan early. That needs a second registry configuration with a different
  visibility sharing the table.

The log lives in its own column family (`a`) of the registration's row. The
cells the existing flows write are unchanged byte for byte
(`internal/collectorregistry/testdata/persisted_rows.golden.json`, captured
before this slice).

### Retries and budgets

Provision and revoke are idempotent and listed in `requestMayCommit`, so an
over-budget or uncertain response reports `indeterminate`; retry with the
identical request. `AdminReceipt.Changed` says whether this call made the
change, so a retry of a change that already applied reports `false` with the
same registration. Receipts carry only the collector ID, generation, state
and `changed`: at most 1467 bytes with a maximal ID, within
`api.MaxAdminReceiptBytes` (2048). The largest registration record is about
172 KB, within `api.MaxResponseBytes`.

## Attestation

`internal/collectorattest.Set` dispatches a report by kind:

- A kind with a configured verifier is **verified or refused**. A refusal
  fails the enrollment with nothing written; it is never downgraded to a claim.
- A kind with no verifier is recorded as a **claim**: the evidence digest is
  kept, no verifier is named, and `AttestationResult.ID()` is empty, so a claim
  cannot become a `decision.Source.AttestationID`.

The one verifier, `ed25519-statement`, checks a statement signed by an
operator-configured Ed25519 key over the collector ID, image digest, issue and
expiry times, and a nonce `collector.EnrollNonce(collectorID, key)` bound to the
enrollment's idempotency key. Forged signatures, expired or not-yet-valid
statements, validity windows above the configured maximum, a different
collector and a nonce minted for another key are refused. Because an
idempotency key cannot be reused across generations, a captured statement
cannot be replayed after re-provisioning either.

**Residual trust.** This verifier proves only that the holder of the operator
key vouched for this enrollment. It does not prove which code runs: the
operator's signing process and key custody are trusted. Nitro, SGX/TDX and
cosign verification are later verifiers behind the same interface. Results
are stored as provenance; no policy consumes them yet.

## Mapping to decision sources

`collectorregistry.Source` maps an active observation onto `decision.Source` by
value: `OriginID` is the collector, `AuthorityPolicyID` must be granted by the
observation's enrollment and still provisioned, `Control` is the provisioned
control. `AttestationID` is set only for verified attestation, and only when
the observation's observed and received times both fall within the
statement's `[IssuedAt, ExpiresAt)`. An enrollment lasts until revocation but
its attestation does not; outside that window the attestation has lapsed
(`collectorregistry.AttestationLapsed`) and the source carries none. This is an
**internal adapter, not an agreed contract**; how pictures consume collector
observations belongs to the decision track (#418).

## Boundary

`internal/importboundary` parses source files (it does not need the go command,
so it works with `GOWORK=off`) and fails when:

- A. any package in the root module (not only `pkg/`, `internal/` and
  `cmd/`) or in any nested module outside `extensions/` (such as
  `wal-quorum-sidecar`) imports `extensions/`; or the root `go.mod` or a
  nested module's `go.mod` requires or replaces an extension module, or has a
  local replace (`./`, `../`, absolute or backslash path) that does not
  resolve to the root or a nested module the check walks. That rejects
  replaces onto `extensions/`, onto fixture modules, onto untracked
  directories and onto paths outside the repository. Or `go.work` has any `replace`, or a
  `use` other than
  the root, an extension module or a nested module. Nested modules are
  covered because a `require`/`replace` or a `go.work` `use` can link them,
  and through them extension code, into core. Using an extension module in
  `go.work` is harmless only because no core or nested-module package may
  import it;
- B. an extension module (any `go.mod` under `extensions/`, at any depth)
  imports a repository package outside `pkg/sdk`, `pkg/collector`,
  `pkg/collector/api`, `pkg/decision/api`, `pkg/admission/api`,
  `pkg/attestation/api`, `pkg/dispatch/api`, `pkg/shoal`;
  or declares a module
  path other than the repository module plus its directory (a module naming
  itself `.../internal` would otherwise exempt its own imports, and Go's
  `internal` rule does not protect this tree from such a module); or replaces
  anything other than the repository module with a relative path to this
  tree; or a Go file under `extensions/` sits outside every extension module;
- C. the in-repository import closure of those packages contains `internal/`.

The scan descends into every directory, including `testdata`, `vendor` and
names beginning with `_` or `.`. `go build ./...` skips those, but Go still
compiles them when a package imports them by explicit path. Each module is
walked on its own, stopping at the next module boundary. Two things are
skipped:

- the repository's `.git`;
- the checker's own fixtures: only the direct children of
  `internal/importboundary/testdata` (`importboundary.FixtureRoot`) that
  have their own `go.mod`. Any other Go file under that directory belongs to
  the root module, and Go builds it when it is imported by explicit path, so
  it is checked like any other code. A test asserts that every fixture
  directory is its own module.

The real-tree tests check what the repository contains: they read an FS of
the files `git ls-files` lists, and fail if git is unavailable. Untracked
content in a working tree, such as the Thrift source tarball CI unpacks or an
editor's nested worktree, is therefore never checked, and a tracked copy of
the same content would be. CI also unpacks Thrift under `$RUNNER_TEMP`, outside
the checkout. Fixture tests use plain directory trees.

Any symlink in a checked module or under `extensions/` is a violation. The
walk does not follow symlinks, but the go command and `go.work` do. The
repository contains none. The symlink fixture's links are created at test
time in a temporary copy rather than committed, so a checkout without
symlink support still runs the test, or skips it if the platform refuses.

cgo, assembly, `.syso` and SWIG files let a package compile or link code that
no Go import names. The C preprocessor has too many spellings to police with
a deny-list, so these rules are an allowlist (`internal/importboundary/cgo.go`):

- **Extensions use no cgo or SWIG in this slice.** An extension module may
  not contain `import "C"`. It may not contain any source the go command
  builds besides `.go` files: C, C++, Objective-C, Fortran, headers,
  assembly, `.syso`, `.swig` or `.swigcxx`, including under `testdata/`.
  In fact an extension may contain only `.go`, `go.mod`, `go.sum` and `.md`
  files, plus `.json` and `.golden` files under a `testdata/` directory.
  Any extension file is something an include elsewhere could try to name, so
  the set is kept small. The example extension needs only `.go` and
  `go.mod`. Allowing cgo in an extension later needs its own design.
- **Core may use cgo only in `CgoPackages`**, which today is `cmd/shoal-capi`,
  the only cgo package in the tree. For that package:
  - `#cgo` directives are an exact allowlist. The only verbs are `CFLAGS` and
    `CPPFLAGS`, and every argument must be either
    `-I${SRCDIR}/<path>` resolving into the package directory or
    `CgoIncludeDirs` (`capi/include`, `capi/tests`), or `-D<IDENT>` with an
    optional value containing no path characters, quotes or spaces. Anything
    else is a violation: `LDFLAGS`, `pkg-config`, `-include`, `-iquote`,
    `-Wp,`, `-Xpreprocessor`, `@file`, a separate-argument form, or an `-I`
    not anchored on `${SRCDIR}`. `cmd/shoal-capi` uses only
    `-I${SRCDIR}/../../capi/include`, `-I${SRCDIR}/../../capi/tests` and
    `-DSHOAL_CAPI_TEST`.
  - Scanning follows what the compiler reads. That means the package's
    preambles, the C-family files directly in its directory (the files the
    go command compiles), and every repository file they include,
    transitively, whatever its extension. With flags limited to `-I` and
    `-D`, the compiler reads nothing else. A directory walk would also judge
    files that other toolchains build with other search paths, such as the C
    tests in `capi/tests`.
  - A quoted include is searched beside the including file, then in the
    package directory and the `-I` directories. An angle include is searched
    in the package directory and the `-I` directories only. Every candidate
    that exists, not just the first, must lie in the allowed directories and
    is scanned, so the checker and the compiler cannot disagree about which
    file is used. A quoted include found nowhere is a violation. An angle
    include found nowhere is a system header and may not contain `/` or
    `..`. `#include_next` and `#import` are treated the same way.
  - Constructs the checker cannot follow are refused outright in the
    package's preambles, its C files and every repository file they include:
    - inline assembly (`asm`, `__asm`, `__asm__`), and any `.include` or
      `.incbin` text, because the assembler fetches files itself;
    - C++ raw strings (`R"`, `LR"`, `uR"`, `UR"`, `u8R"`), which desynchronise
      comment and string scanning;
    - `#embed`, and `__has_include` / `__has_include_next`;
    - a carriage return not followed by a line feed, which gcc treats as a
      line end and this checker does not (CRLF is normalised).
- Before matching, the checker normalises CRLF to LF and removes every
  backslash followed by optional spaces or tabs and a newline
  (`\\[ \t]*\n`). gcc joins all of these lines, warning when whitespace
  precedes the newline. The refused forms above are matched in both the raw
  and the spliced text, so `__a\` + newline + `sm__` is still inline
  assembly. Include parsing then also replaces comments with a space. So
  `#include \` followed by a new line, `#inc\ ` + newline + `lude`, and
  `#/**/include` read as the directive the compiler sees. Trigraphs are
  refused rather than translated. Macros as include targets,
  digraphs, trigraphs, unterminated comments, line markers, `#cgo` line
  continuations and any other form the parser cannot read are violations.
- Any other core package with `import "C"`, or with a buildable non-Go file,
  is a violation. A directory with no Go files is not a package, and the go
  command never builds it. `InertCSourceDirs`
  (`docs/testdata/validate_sharkbite_matrix`) holds C fixtures that a Go test
  reads as data. Its C files are allowed because the go command refuses to
  build C in a package without `import "C"`. Assembly, `.syso` and SWIG stay
  forbidden even there.

Residual: flags and search paths supplied by the build environment
(`CGO_CFLAGS`, `CGO_LDFLAGS` and the like, `pkg-config` search paths, system
include directories) are outside a source check.

**Threat model.** The checker keeps the dependency direction in place
against mistakes and casual circumvention. It covers the Go import graph,
module wiring (go.mod, go.work, replace, tool, nested modules, symlinks) and
the one allowlisted cgo package. It is not a sandbox against a committer who
deliberately smuggles code through C, the assembler or toolchain behaviour;
static analysis of C does not converge on that. Two things bound the
residual:

- cgo is confined to a single allowlisted package (`cmd/shoal-capi`), whose
  changes require review;
- build-environment flags are outside any source check.

Classes closed by fixtures in `internal/importboundary/testdata`:

- **Imports in both directions:** imports of `extensions/` from anywhere in
  the root module, including `_`, `testdata` and other skipped-by-`./...`
  directories; and extension imports of anything beyond the allowlist.
- **Transitive leaks** of `internal/` through the allowlist.
- **Module wiring:**
  - module-path spoofing and stray replaces in extension modules;
  - nested extension modules, and loose files under `extensions/`;
  - bridges through nested modules wired by go.mod or go.work;
  - local replaces onto fixtures or unchecked directories;
  - `tool` directives;
  - glued-parenthesis and unknown go.mod directives;
  - symlinks.
- **cgo outside the allowlist,** including C, assembly, `.syso` and SWIG.
- **Inside the allowlisted package:**
  - disallowed `#cgo` flags (`-Iinc`, `-include`, `@file`, `-Wp`,
    `-Xpreprocessor`, `-iquote`, `LDFLAGS`, absolute paths);
  - include spellings: angle includes with paths, macro includes, `.inc`
    chains, line continuations, comments inside the directive, forced
    includes;
  - decoy resolution;
  - inline assembly `.include`, raw strings, and a lone carriage return.

A go.mod `tool` directive puts its package in the module's build graph. Core
`go.mod` files may not name a tool under `extensions/`. An extension may
name only its own packages or allowlisted ones.

The root `go.mod`, `go.work` and every extension `go.mod` are parsed with every known directive recognized and
parentheses split from adjacent tokens (`replace(` counts the same as
`replace (`). An unknown directive, an unbalanced or nested block, or any
other line the parser cannot read exactly is reported as a violation, never
skipped.

Fixtures under `internal/importboundary/testdata` prove each rule detects a
violation. A source file that does not parse makes `Check` return an error,
which the test (and so CI) reports as a failure, not a pass. CI also vets and tests every module under `extensions/` on its own
`go.mod`, and fails if a core package links `golang.org/x/crypto/ssh`, an RDP
library or Guacamole (a failing `go list` fails that check rather than passing
it).

`extensions/example-collector` is a synthetic file-tail collector that imports
from Shoal only `pkg/sdk` and the allowlisted `pkg/collector` and `pkg/shoal`
(its tests also ask for admission through `sdk.New(...).Admission()` and
`pkg/admission/api`, to prove that seam is reachable from an extension). Run it
against a host that provisioned it:

```sh
SHOAL_TOKEN=... go run ./extensions/example-collector \
  -base-url https://shoal.example -collector collector:tail \
  -authority authority:logs -file /var/log/app.log
```

## Deferred

- Public admission and report contracts in the SDK, and moving
  `cmd/shoal-llm-gateway` onto it (after #443/#451).
- Policy requiring verified attestation for `EffectMutatesExternal` executors
  (with ATPL `runtime`, #457); hardware and code attestation verifiers.
- Raw artifact byte retention (#447).
- Building picture sources from observations (#418) and propagating quarantine
  into datasets, calibrations and releases (#419).
