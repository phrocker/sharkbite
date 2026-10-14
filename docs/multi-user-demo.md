# Deterministic two-user Shoal demo

This guide provisions a repeatable public-OSS demonstration against one shared
`shoal-explore-web` service. Two OIDC-authenticated people use the same HTTP
MCP endpoint, but each uses distinct owner-bound workspace settings.

The seed contains:

- one team;
- two people mapped to configured OIDC `sub` values;
- two simulated Fleet agents registered against host-owned executor references;
- seven work items; and
- one deterministic activity record on each of 14 consecutive UTC dates.

Every fixture has stable IDs, timestamps, filenames, graph keys, and content.
The seed publishes exact `team`, `person`, `agent`, `work_item`, and `activity`
nodes plus direct `member_of`, `assigned_to`, and `blocked_by` relations through
`POST /api/v1/graph/materialize`. IDs are derived from the trusted authorization
scope, configured namespace, and opaque local key. Exact reruns return
`unchanged`; divergent reuse of a namespace or mutation ID fails with a
conflict. The command also refreshes the two Fleet leases and verifies the
result through `POST /api/v1/team/overview`.

## 1. Run one shared service

Use the same image for local and shared deployments. A shared deployment needs
OIDC, an exact allowed host, and one persistent state root:

```console
shoal-explore-web \
  -state-dir=/var/lib/shoal \
  -listen=0.0.0.0:8098 \
  -allowed-host=shoal.example.test
```

Set the provider-neutral OIDC environment variables shown in
`deploy/shoal-explore-web/shared.env.example`. Do not use `-dev-auth` on a
shared listener.

Both demo users need authorization-claim values mapped to:

| Mapping | Operations used by the demo |
| --- | --- |
| reader | list, read, connect, retrieve, neighborhood, workspace-settings read |
| contributor | ingest, graph materialization, and workspace-settings write, in addition to reader |
| fleet | agent registration, heartbeat, and resolution used by the seed |

The seed command verifies each bearer token with `GET /api/v1/identity`. It
refuses a token whose trusted Shoal subject is not
`oidc:<configured-issuer>#<configured-subject>` or which lacks `ingest`, `graph_materialize`, `workspace_settings_read`,
`workspace_settings_write`, `agent_register`, `agent_heartbeat`,
`agent_resolve`, or `team_overview_read`.

Persist the **whole** `-state-dir`. With the path above, the corpus (including
workspace settings) is under `/var/lib/shoal/corpus` and the durable
authorization catalog is under `/var/lib/shoal/policy`. Both directories must
survive restarts, and the service must remain a single writer.

## 2. Configure and run the seed

Copy the example without committing the populated copy:

```console
Copy-Item deploy\shoal-explore-web\demo-scenario.json.example .\demo-scenario.json
```

Replace the public endpoint, exact OIDC issuer, authorization domain, the two
users' raw subject claim values, and non-secret display values. The configured
graph source/policy IDs must match the host's trusted policy selector. Each
`executor_ref` must appear in the host's configured Fleet executor registry.
Keep workspace IDs distinct:
workspace settings are owned by the authenticated subject that creates them,
so one shared workspace ID cannot be provisioned for two owners.

Put short-lived access tokens in the named environment variables, never in the
JSON file:

```powershell
$env:SHOAL_DEMO_TOKEN_ALEX = [System.Net.NetworkCredential]::new(
  "", (Read-Host "First user's access token" -AsSecureString)).Password
$env:SHOAL_DEMO_TOKEN_SAM = [System.Net.NetworkCredential]::new(
  "", (Read-Host "Second user's access token" -AsSecureString)).Password
$env:SHOAL_DEMO_AGENT_KEY_ALEX = [System.Net.NetworkCredential]::new(
  "", (Read-Host "First agent registration key" -AsSecureString)).Password
$env:SHOAL_DEMO_AGENT_KEY_SAM = [System.Net.NetworkCredential]::new(
  "", (Read-Host "Second agent registration key" -AsSecureString)).Password
go run .\cmd\shoal-demo-seed -config .\demo-scenario.json
```

The command provisions each workspace through
`PUT /api/v1/workspaces/{workspace}/settings`, then uploads that user's stable
fixture subset through `POST /api/v1/ingest` with both
`X-Shoal-Workspace-Request: 1` and the user's `Shoal-Workspace-ID`.
It then reads the current snapshot, performs one snapshot-CAS graph
materialization, registers or heartbeats both Fleet agents, and validates the
team overview.
The JSON result prints each raw workspace ID and its canonical unpadded
base64url `workspace_header` value. It never prints a bearer token.

Run the same command again. All workspace mutations replay at revision 1,
every file disposition and the graph disposition are `unchanged`, and Fleet
leases are refreshed without duplicate agent IDs. A graph conflict means the
namespace was previously used with different content; choose a new namespace
rather than overwriting unrelated state.

## 3. Configure VS Code HTTP MCP

Shoal exposes one shared Streamable HTTP endpoint:

```text
https://shoal.example.test/mcp
```

Copy `.vscode/mcp.json.example` to `.vscode/mcp.json`. The checked-in example
contains no endpoint, subject, tenant, token, or workspace value. Each user
starts VS Code with their own process environment:

```powershell
$env:SHOAL_MCP_URL = "https://shoal.example.test/mcp"
$env:SHOAL_MCP_BEARER_TOKEN = [System.Net.NetworkCredential]::new(
  "", (Read-Host "This user's access token" -AsSecureString)).Password
$env:SHOAL_MCP_WORKSPACE_ID = "<this user's workspace_header from the seed>"
code .
```

The two users set the same `SHOAL_MCP_URL`, but different bearer tokens and
different `SHOAL_MCP_WORKSPACE_ID` values. The required configured headers are:

- `Authorization: Bearer ...` — authenticates the caller; and
- `Shoal-Workspace-ID: ...` — selects only settings owned by that caller.

VS Code supplies the MCP `Accept`, content type, protocol-version, and session
headers as part of the Streamable HTTP lifecycle. For direct protocol
diagnostics, clients must advertise both `application/json` and
`text/event-stream`; after initialization they must return the
`MCP-Session-Id` and `MCP-Protocol-Version` headers.

### Bearer-token workaround and refresh limitation

Shoal currently validates bearer tokens but does not publish the MCP OAuth
discovery/challenge flow that would let VS Code acquire and refresh them. The
environment-backed `Authorization` header is therefore a workaround, not an
automatic sign-in flow. When a token expires, close the VS Code process that
inherited it, obtain a new token, update `SHOAL_MCP_BEARER_TOKEN`, relaunch VS
Code, and restart the Shoal MCP server. Do not put a live token in
`.vscode/mcp.json`, user settings, source control, terminal history, or the
scenario JSON.

The shared service's `-allowed-host` value must exactly match the authority in
`SHOAL_MCP_URL` (hostname comparison is case-insensitive; ports are exact).
`X-Forwarded-Host` is not trusted.

## 4. Two-user walkthrough

1. Start the shared service and confirm its state root is on persistent
   storage.
2. Run the seed once with both users' tokens, then rerun it and confirm all
   fixture dispositions are `unchanged`.
3. User A launches VS Code with User A's token and workspace header. User B
   launches a separate VS Code process with User B's values. Both point to the
   same `/mcp` URL.
4. In each window, run **MCP: List Servers**, start `shoal`, and inspect the
   server output if initialization fails.
5. Ask each user to retrieve the demo team, work items, and 14-day activity.
   The shared corpus is visible under the user's own narrowing settings, while
   recorded MCP operations are attributed to that user's trusted principal.
6. Confirm the overview reports two active simulated agents. Registration does
   not launch a process: each descriptor only refers to a host-owned executor.
   Leases are at most 24 hours; rerun the seed before expiry to heartbeat them.
7. Restart the service without replacing the state volume. Reconnect both
   users and confirm the fixtures and workspace settings remain available.

## 5. Team dashboard

Open the Explorer web UI and select **Team**. Enter the configured team ID and
the exact authorized source and grant-policy wire IDs, then choose a bounded
history window (maximum 31 days) and activity page size (maximum 100). If the
selected workspace narrows to exactly one source and policy, **Load team**
fills those IDs from the workspace settings.

The dashboard calls `POST /api/v1/team/overview` and presents:

- bounded overview metrics, roster, agents, and work items;
- a unified activity timeline with explicit pagination;
- daily activity and current-versus-previous seven-day trends;
- panels clearly labeled as heuristic bottleneck, improvement, and automation
  signals; and
- exact evidence, source, and policy identifiers.

**Refresh** starts a new bounded snapshot. **More activity** continues only the
current snapshot cursor. **Export JSON** downloads the authorized response and
the currently loaded activity pages. **Print** uses a report layout that omits
navigation and controls.

The UI does not make authorization decisions. A challenged `401` asks the user
to sign in again, an authorization denial remains an explicit denial, and a
non-disclosing `404` is described only as “unavailable or withheld.” Empty or
truncated sections do not claim that hidden data is absent.

The team endpoint consumes graph nodes and direct `member_of`, `assigned_to`,
and `blocked_by` relations. Corpus fixture ingestion alone does not invent
those relationships; the demo environment must materialize the team graph
using the schema published by `pkg/explorer/teamoverview`.

## 6. Visibility boundary

Shoal records interactions only when a request reaches a Shoal HTTP/MCP
operation. It does **not** observe arbitrary editor activity.

To demonstrate the boundary:

1. Call `shoal.provenance.list` and note the latest interaction.
2. Edit and save a file directly in VS Code.
3. Run a terminal command and `git status` without invoking a Shoal tool.
4. Call `shoal.provenance.list` again.

Steps 2 and 3 produce **no Shoal interaction** because no request crossed the
Shoal MCP boundary. The second provenance call records its own MCP tool use, so
compare the records immediately before that final call (or filter out the two
list calls). Shoal must not be presented as capturing file edits, terminal
commands, Git operations, complete Copilot conversations, or reasoning that
occurred outside its tools.

## 6. Automated acceptance coverage

Run the deterministic two-user acceptance harness without OIDC credentials:

```console
go test ./cmd/shoal-explore-web -run TestTwoUserHTTPMCPDemoAcceptance -count=1
```

The test starts one shared authenticated HTTP service with injected Alice and
Bob tokens, separate owner-bound workspaces, isolated MCP sessions, and a
simulated Fleet executor. It verifies durable MCP ingestion and listing,
server-generated correlation, Fleet dispatch and invocation, cross-user
evidence visibility, team-overview changes, and persistence across restart. It
also performs editor-, terminal-, and Git-like filesystem fixture operations
and verifies that they create no interaction records.

The injected authenticator, static team graph, and executor are test fixtures;
the harness does not claim live OIDC sign-in, external agent execution, or
visibility into activity that does not cross the Shoal HTTP/MCP boundary.
