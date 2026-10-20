# Fleet action discovery over MCP

When the embedded Fleet registry is configured, the hosted MCP server offers
`shoal.agent_discover`. It pages through the same durable, authorized registry
as `POST /api/v1/fleet/agents/resolve`. Each returned agent has a generation,
scopes, and declarative capabilities. Each action includes its input/output
schemas, effect classes, and `requires_approval` / `requires_attestation` flags.
MCP uses the same public descriptor JSON projection as Fleet HTTP. The
`executor_ref` is an opaque host-owned reference, not an execution endpoint.

The tool takes optional `limit` (default 16, maximum 16) and `cursor` fields.
Pass the returned `next_cursor` unchanged to read the next page. Cursor and ID
strings use unpadded URL-safe base64, matching the Fleet HTTP route IDs. The
caller does not supply a request ID, actor, reason, or authorization domain:
the server derives a fresh request identity from the authenticated MCP call.
Fleet applies the `agent_resolve` operation and scope/binding checks, and
records the registry read under its lifecycle receipt. The MCP call also has
the ordinary durable tool-call record.

This lists registered declarations visible to the caller. It does not
guarantee that a host executor is currently bound, that a later generation or
lease will remain current, or that an action will be admitted. Resolve and
dispatch recheck those conditions at use time. This is a discovery step for
issue #146; dynamic per-action MCP tools, registration workflows, and operator
adjudication/export remain separate work.
