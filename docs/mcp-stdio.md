# Shoal MCP stdio server

`shoal-mcp` exposes an authorized embedded Explorer corpus as a
newline-delimited JSON-RPC MCP server on standard input and standard output.
It does not require a live Accumulo cluster or any network service.

This command implements the handshake-era MCP `2025-11-25` protocol: clients
must send `initialize`, receive the server's negotiated version, and then send
`notifications/initialized`. It supports no other protocol version. When a
client proposes an unsupported version, the initialize response selects
`2025-11-25`; a client that does not support that version must disconnect.
The later sessionless `2026-07-28` protocol is intentionally not claimed and
requires a separate future transport implementation.

```bash
go run ./cmd/shoal-mcp \
  -state-dir .shoal/mcp \
  -dev-auth
```

An MCP client should launch the command as a subprocess and exchange one
JSON-RPC message per line. Standard output is reserved exclusively for protocol
messages. Flag help, startup failures, storage failures, and other diagnostics
are written to standard error. A single inbound JSON-RPC message is limited to
16 MiB and is rejected whole if it exceeds that bound; messages are never
truncated and interpreted as partial requests.

This shell smoke test performs the MCP initialization handshake and lists the
tools against a local embedded workspace:

```bash
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"smoke","version":"1"}}}' \
  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' |
  go run ./cmd/shoal-mcp -state-dir .shoal/mcp-smoke -dev-auth
```

## Storage and policy configuration

The recommended `-state-dir` layout stores the embedded corpus in `corpus/`
and its durable authorization catalog in `policy/` under one root. Persist both
directories together.

| Flag | Environment | Purpose |
|---|---|---|
| `-state-dir` | `SHOAL_MCP_STATE_DIR` | Recommended state root containing `corpus/` and `policy/` |
| `-data` | `SHOAL_MCP_DATA` | Corpus directory when `-state-dir` is unset; defaults to `.shoal/explorer` |
| `-policy-dir` | `SHOAL_MCP_POLICY_DIR` | Explicit durable policy directory |
| `-context-budget-bytes` | `SHOAL_MCP_CONTEXT_BUDGET_BYTES` | Compatibility-text context budget; defaults to 1 MiB |
| `-tool-calls-per-minute` | `SHOAL_MCP_TOOL_CALLS_PER_MINUTE` | Per-process tool-call limit; defaults to 120 |

If an existing corpus contains documents but the selected policy catalog has
no registrations, startup is refused. Restore the matching policy directory or
ingest through an authorized workspace; the command does not silently grant an
unregistered corpus to its process identity.

## Process identity

`-dev-auth` (or `SHOAL_MCP_DEV_AUTH=true`) selects the fixed local development
process identity. Its domain, source, and policy defaults match the local
Explorer workspace defaults, allowing the commands to use the same persisted
corpus and policy catalog.

For an explicitly configured process identity, omit `-dev-auth` and provide:

| Flag | Environment |
|---|---|
| `-identity-subject` | `SHOAL_MCP_IDENTITY_SUBJECT` |
| `-identity-actor` | `SHOAL_MCP_IDENTITY_ACTOR` |
| `-identity-client-id` | `SHOAL_MCP_IDENTITY_CLIENT_ID` |
| `-identity-domain` | `SHOAL_MCP_IDENTITY_DOMAIN` |
| `-identity-source` | `SHOAL_MCP_IDENTITY_SOURCE` |
| `-identity-policy` | `SHOAL_MCP_IDENTITY_POLICY` |
| `-identity-operations` | `SHOAL_MCP_IDENTITY_OPERATIONS` |
| `-identity-generation` | `SHOAL_MCP_IDENTITY_GENERATION` |
| `-identity-lifetime` | `SHOAL_MCP_IDENTITY_LIFETIME` |
| `-identity-audit-purpose` | `SHOAL_MCP_IDENTITY_AUDIT_PURPOSE` |
| `-identity-labels` | `SHOAL_MCP_IDENTITY_LABELS` |

Subject, actor, domain, source, and policy are required. The default explicit
operation set is read-only:
`list,read,neighborhood,retrieve,validation`. Generation defaults to `1`, and
each decision lifetime defaults to `15m`.

`-identity-labels` grants the process identity free-form visibility labels
(#570): a comma-separated list of `<source>=<label>`, where the source must be
the identity's own source exactly (with `-dev-auth`,
`shoal-explore-web/workspace`). A labelled document is readable only by a
principal holding every one of its labels on its source, so without this flag
the identity can neither read nor ingest labelled content. The labels add
policy grants only; operations are unchanged. A label policy can never be
named through `-identity-policy`, which refuses the whole `shoal.label/`
namespace.

Before serving the first tool call, `shoal-mcp` runs the same startup label
migration as `shoal-explore-web`: documents labelled before labels were
enforced are narrowed to their label policies, untranslatable ones become
readable by nobody, and any error refuses to start. stderr stays silent unless
the migration narrowed something or left documents unreadable. See "Upgrading:
labelled documents are tightened at startup" in
`docs/shoal-explore-web-deploy.md`.

Generic interaction evidence is reauthorized with the legacy `retrieve`
operation. An explicit identity therefore needs `retrieve` for tools whose
records touch grounded source nodes. A source-free action record needs only
the exact action operation that its provider enforces. The interaction
`tool_call` value is provenance metadata, not an `auth.Operation`, and is never
translated wholesale to fleet `invoke` or `dispatch`.

The launcher configuration is the trusted identity source in stdio v1. The MCP
server mints and binds a fresh authorization decision with a new request ID for
every `tools/call`, but every caller connected to the same process uses the same
configured identity. Stdio cannot independently authenticate remote callers.
Independently authenticated remote callers use the Streamable HTTP `/mcp`
endpoint mounted by `shoal-explore-web`.

The server enforces a fixed-window, per-process tool-call limit before
authorization and dispatch. Exceeding the configured per-minute limit returns
a bounded `unavailable` tool error without invoking the workspace.

## Tool surface and recording

The command constructs a real embedded `Explorer`, wraps it with the authorized
client, and serves it through `webapi.EmbeddedService`. Optional tools are
advertised only when that service implements their public capability
interfaces and any required runtime configuration is active. The embedded
service currently supplies ingestion, recomputation, and the authorized
changes feed in addition to the read tools. Extraction is not advertised by
this launcher because it has no active ontology configuration. The standalone
stdio launcher does not configure the web command's chat and provenance
providers, so it does not advertise `shoal.ask` or
`shoal.provenance.{list,inspect,fold,unfold}`. Those first-party adapters are
available from the authenticated `/mcp` endpoint in `shoal-explore-web`.

Tool-call recording is mandatory and fail-closed. The command verifies the
durable interaction sink during startup, then records each generic invocation
as `OperationToolCall` under the same fresh decision used by the tool. Records
carry the authorization fingerprint and expiry, canonical authorized operation,
observed snapshot, hashed arguments, and complete retrieved source set. The MCP
adapter does not supply actor or delegation metadata; the authorized client
overwrites those fields from the bound decision and derives a hashed
`AuditPurpose` reason when configured. Generic calls never synthesize inference
nodes. A mutating tool is admitted to the recorder before dispatch; if its
post-effect outcome cannot be recorded, the returned error is explicitly
indeterminate and tells the client to inspect current state before deciding
whether a retry is safe.

Successful tool results always carry the complete web API wire object in
`structuredContent`. The server also runs the compatibility text mirror through
the native context compressor with a 1 MiB budget. Results above that budget
omit only the duplicate text block; they do not truncate structured data,
source identities, citations, visibility, or errors. This context compression
is separate from Shoal's provenance `fold`.
