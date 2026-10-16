<!--

    Licensed to the Apache Software Foundation (ASF) under one
    or more contributor license agreements.  See the NOTICE file
    distributed with this work for additional information
    regarding copyright ownership.  The ASF licenses this file
    to you under the Apache License, Version 2.0 (the
    "License"); you may not use this file except in compliance
    with the License.  You may obtain a copy of the License at

      https://www.apache.org/licenses/LICENSE-2.0

    Unless required by applicable law or agreed to in writing,
    software distributed under the License is distributed on an
    "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
    KIND, either express or implied.  See the License for the
    specific language governing permissions and limitations
    under the License.

-->
# Shoal Explorer demo walkthrough

This walkthrough is a verified end-to-end path for the `shoal-explore` CLI, the
optional `shoal-explore-web` workspace, and the `shoal-mcp` agent surface over
the same corpus. It starts from a clean checkout and uses only checked-in
Markdown fixtures plus a local `.shoal/explorer-demo` corpus.

Every step gives the command twice: first for bash on Linux or macOS, then for
PowerShell on Windows. Both run from the repository root. The bash commands
need `jq` and `curl`. Two automated tests keep the demo from drifting:
`TestDocumentedDemoWorkflow` runs the same CLI sequence in-process, and
`TestDocumentedDemoWalkthroughShell` extracts the bash blocks from this file
and executes them on a Linux or macOS runner against a temporary corpus,
including the web workspace and the MCP step.

## 1. Start clean

```bash
rm -rf .shoal/explorer-demo .shoal/explorer-demo-policy
```

```powershell
Remove-Item -Recurse -Force .shoal\explorer-demo, .shoal\explorer-demo-policy -ErrorAction SilentlyContinue
```

Expected outcome: the demo corpus directory and its policy catalog are absent,
so the first ingest creates new Explorer records. The policy catalog is the
sibling `.shoal/explorer-demo-policy` directory that step 9 creates; a stale
one would carry registrations for documents that no longer exist.

## 2. Ingest two Markdown sources

```bash
go run ./cmd/shoal-explore ingest \
  -data .shoal/explorer-demo \
  -file docs/explorer-demo-guide.md
```

```powershell
go run .\cmd\shoal-explore ingest `
  -data .shoal\explorer-demo `
  -file docs\explorer-demo-guide.md
```

Expected observable outcome:

- the JSON contains `"Disposition": "applied"`;
- `"Document"."Title"` is `"Shoal Explorer Demo Guide"`;
- `"SectionCount"` is `5` and `"SpanCount"` is `4`;
- the dynamic `"Document"."ID"` and `"Revision"."ID"` values are present.

The engine also logs `INFO` lines such as `table created` and
`flush complete` on standard error. They are not part of the JSON result.

Then ingest the related release checklist:

```bash
go run ./cmd/shoal-explore ingest \
  -data .shoal/explorer-demo \
  -file docs/explorer-demo-release.md
```

```powershell
go run .\cmd\shoal-explore ingest `
  -data .shoal\explorer-demo `
  -file docs\explorer-demo-release.md
```

Expected observable outcome: the JSON contains `"Disposition": "applied"`,
`"Document"."Title"` is `"Explorer Release Checklist"`, `"SectionCount"` is
`3`, and `"SpanCount"` is `2`.

## 3. Re-ingest idempotently

```bash
go run ./cmd/shoal-explore ingest \
  -data .shoal/explorer-demo \
  -file docs/explorer-demo-guide.md
```

```powershell
go run .\cmd\shoal-explore ingest `
  -data .shoal\explorer-demo `
  -file docs\explorer-demo-guide.md
```

Expected observable outcome: the JSON contains `"Disposition": "unchanged"`
and returns the same document and revision IDs as the first guide ingest.

## 4. List documents and keep their IDs

```bash
documents=$(go run ./cmd/shoal-explore list -data .shoal/explorer-demo)
guide=$(jq -r '.[] | select(.Document.Title == "Shoal Explorer Demo Guide") | .Document.ID' <<<"$documents")
release=$(jq -r '.[] | select(.Document.Title == "Explorer Release Checklist") | .Document.ID' <<<"$documents")
echo "$guide"
echo "$release"
```

```powershell
$documents = go run .\cmd\shoal-explore list -data .shoal\explorer-demo |
  ConvertFrom-Json
$guide = @($documents | Where-Object { $_.Document.Title -eq "Shoal Explorer Demo Guide" })[0]
$release = @($documents | Where-Object { $_.Document.Title -eq "Explorer Release Checklist" })[0]
$guide.Document.ID
$release.Document.ID
```

Expected observable outcome: the list contains both demo titles, and the last
two lines print opaque IDs beginning with `doc_`. Titles sort
alphabetically, so the release checklist appears before the guide in the raw
JSON list. The bash variables hold the bare IDs; the PowerShell variables hold
the whole document summaries, which is why later PowerShell steps use
`$guide.Document.ID`.

## 5. Inspect the hierarchy

```bash
go run ./cmd/shoal-explore outline \
  -data .shoal/explorer-demo \
  -document "$guide"
```

```powershell
go run .\cmd\shoal-explore outline `
  -data .shoal\explorer-demo `
  -document $guide.Document.ID
```

Expected observable outcome: the JSON root is titled
`"Shoal Explorer Demo Guide"` and its child hierarchy includes
`"Ingested knowledge"`, `"Promotion gate"`, and nested
`"Relationship target"` sections. Each section carries its revision-specific
ID and byte range.

## 6. Retrieve exact cited evidence with explanations

```bash
go run ./cmd/shoal-explore query \
  -data .shoal/explorer-demo \
  -text "canary validation exact citation" \
  -top 2 \
  -modes lexical,tree,graph \
  -explain=true
```

```powershell
go run .\cmd\shoal-explore query `
  -data .shoal\explorer-demo `
  -text "canary validation exact citation" `
  -top 2 `
  -modes lexical,tree,graph `
  -explain=true
```

Expected observable outcome: the first result quotes exactly:

```text
Run the canary validation before promotion and keep the exact citation in the
release note.
```

The result also includes a `"Citation"` with the guide document ID, revision
ID, section ID, span ID, and byte range. `"Explanation"` is present with
`"Modes"` containing `"lexical"`, `"tree"`, and `"graph"`, and `"Scores"` for
those modes.

## 7. Ask a grounded question

```bash
go run ./cmd/shoal-explore ask \
  -data .shoal/explorer-demo \
  -provider fake \
  -question "What keeps release notes grounded?"
```

```powershell
go run .\cmd\shoal-explore ask `
  -data .shoal\explorer-demo `
  -provider fake `
  -question "What keeps release notes grounded?"
```

Expected observable outcome: the deterministic offline fake provider returns
`"Answer": "grounded"` with one grounded claim, `"StopReason": "stop"`, exact
evidence entries containing citations, quotes, byte ranges, and graph paths,
redacted provenance, local execution semantics, budget limits, and a concise
`"Trace"` summary. Add `-trace` to include the detailed per-iteration tool
trace or `-format markdown` for a readable Markdown report. For live model
experiments, select `-provider ollama` or `-provider openai-compatible`;
API-key credentials are read from the environment named by `-api-key-env` and
are not printed. Opaque Shoal IDs in `ask` output are base64-encoded so
document, graph, and evidence identifiers round-trip losslessly; graph
metadata keys and values use the same lossless encoding.

## 8. Create a relationship and traverse neighbors

```bash
go run ./cmd/shoal-explore connect \
  -data .shoal/explorer-demo \
  -id demo-edge-guide-supports-release \
  -from "$guide" \
  -to "$release" \
  -type supports
```

```powershell
go run .\cmd\shoal-explore connect `
  -data .shoal\explorer-demo `
  -id demo-edge-guide-supports-release `
  -from $guide.Document.ID `
  -to $release.Document.ID `
  -type supports
```

Expected observable outcome: the JSON edge echoes
`"ID": "demo-edge-guide-supports-release"`, `"Type": "supports"`,
`"Weight": 1`, and the guide/release document IDs.

```bash
go run ./cmd/shoal-explore neighbors \
  -data .shoal/explorer-demo \
  -node "$guide" \
  -depth 1 \
  -edge-types supports
```

```powershell
go run .\cmd\shoal-explore neighbors `
  -data .shoal\explorer-demo `
  -node $guide.Document.ID `
  -depth 1 `
  -edge-types supports
```

Expected observable outcome: the neighborhood contains one `"supports"` edge
and two document nodes. The node properties show the guide and release titles,
confirming that traversal crossed the relationship created above.

## 9. Start the optional web workspace

```bash
go run ./cmd/shoal-explore-web \
  -data .shoal/explorer-demo \
  -listen 127.0.0.1:8080 \
  -dev-auth
```

```powershell
go run .\cmd\shoal-explore-web `
  -data .shoal\explorer-demo `
  -listen 127.0.0.1:8080 `
  -dev-auth
```

Expected observable outcome on standard output, between the engine's `INFO`
lines:

```text
Authenticating every request as development principal development-principal@localhost
Label migration v1: examined 2 document(s): 0 tightened, 0 already labelled, 0 unlabelled, 2 unregistered; 0 historical revision(s) narrowed by their own labels; 0 untranslatable
Granted 2 pre-existing document(s) in .shoal/explorer-demo to development-principal@localhost: a development-only, one-time migration for -dev-auth on loopback of documents ingested before the policy catalog was durable. The catalog now persists, so these registrations survive restarts (issue #284)
Shoal Explorer listening at http://127.0.0.1:8080
```

Every request must carry an identity. `-dev-auth` mints one fixed development
principal for callers on this host, and the server refuses to start with
`-dev-auth` on any listen address that is not loopback-only, so `:8080`,
`0.0.0.0:8080`, and `[::]:8080` are rejected. Without `-dev-auth` and without a
real authenticator the server refuses to start at all rather than serve
anonymously.

The authorized Explorer client enforces the bound decision on every call, and
its policy catalog is durable in `.shoal/explorer-demo-policy`, next to the
corpus. The documents ingested in steps 2 and 3 through the CLI carry no
catalog registration, which is why `-dev-auth` on a loopback listener grants
them to the development principal at startup and prints the count above. That
grant is development-only: it is refused for a real authenticator and for any
listener another host can reach. It is also one-time: on the next start the
`Granted` line is replaced by a `Policy catalog is durable in ...` line, because
the registrations were persisted. Persist the corpus and the policy directory
together.

The default listen address is also `127.0.0.1:8080`, so the explicit
`-listen` value documents the default. Open <http://127.0.0.1:8080> to use the
workspace. In another shell, the metadata endpoint should also respond:

```bash
curl -fsS http://127.0.0.1:8080/api/v1/meta
```

```powershell
Invoke-RestMethod http://127.0.0.1:8080/api/v1/meta
```

When the backend advertises the `ingest` capability, the workspace shows an
upload area above the corpus list. Use the file picker or drag and drop bounded
Markdown, plain-text, or source-code files. Each successful upload reports
whether the revision was `applied` or already `unchanged`, or `historical`
when the file matches an earlier revision that was not made current again
(the current revision is left as it was), and then refreshes the snapshot and
corpus list.

Expected observable outcome: `/api/v1/meta` publishes the server bounds
(`max_page_size`, `max_top_k`, `max_depth`, `max_fanout`, `max_nodes`,
`max_upload_files`, `max_upload_file_bytes`, and `max_upload_total_bytes`).

Snapshot-bound API calls also carry snapshot identity. For example:

```bash
curl -fsS -X POST \
  -H 'Content-Type: application/json' \
  -d '{"page":{"limit":2}}' \
  http://127.0.0.1:8080/api/v1/documents
```

```powershell
Invoke-RestMethod `
  -Method Post `
  -ContentType "application/json" `
  -Body '{"page":{"limit":2}}' `
  http://127.0.0.1:8080/api/v1/documents
```

Expected observable outcome: the response has a `snapshot` object with `id`,
`as_of`, and `frontier` fields, and its `documents` array contains the two
demo documents. API request bodies reject unknown fields rather than silently
ignoring them. Press <kbd>Ctrl</kbd>+<kbd>C</kbd> in the server shell to stop
the local workspace before the next step.

## 10. Serve the same corpus to an agent over MCP

`shoal-mcp` speaks the Model Context Protocol over standard input and output,
so an MCP client such as an IDE launches it as a subprocess. It serves the same
authorized Explorer; nothing is re-ingested. This smoke test performs the
initialization handshake, lists the tools, and lists the documents:

```bash
printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"demo","version":"1"}}}' \
  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
  '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"shoal.documents","arguments":{}}}' |
  go run ./cmd/shoal-mcp -data .shoal/explorer-demo -dev-auth
```

```powershell
@(
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"demo","version":"1"}}}',
  '{"jsonrpc":"2.0","method":"notifications/initialized"}',
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}',
  '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"shoal.documents","arguments":{}}}'
) | go run .\cmd\shoal-mcp -data .shoal\explorer-demo -dev-auth
```

Expected observable outcome: three JSON-RPC responses, one per line, on
standard output. The `tools/list` response advertises `shoal.documents`,
`shoal.document`, `shoal.retrieve`, `shoal.neighborhood`, `shoal.path`,
`shoal.changes`, `shoal.ingest`, and `shoal.recompute`. The `shoal.documents`
response carries a `snapshot` and the two demo documents with base64-encoded
IDs. The process exits when standard input closes.

This step must follow step 9. Before the web workspace has granted the
CLI-ingested documents, `shoal-mcp` refuses to start:

```text
shoal-mcp: refusing to serve corpus .shoal/explorer-demo with 2 document(s) because policy catalog .shoal/explorer-demo-policy has no authorization registrations; restore the matching policy directory or ingest through an authorized workspace
```

That refusal is the fail-closed rule from `docs/mcp-stdio.md`: an unregistered
corpus is never silently granted to the process identity. The same document
describes the `-state-dir` layout for a corpus that is created through MCP
from the start, the explicit (non-development) identity flags, and the
mandatory tool-call recording. A `shoal.retrieve` call over the embedded
Explorer is currently withheld by that recording (issue #137), which is why
this step lists documents rather than retrieving evidence.

## 11. Where the gateways fit

Steps 1 to 10 are the evidence side: ingest, cite, ask, relate, serve. The
enforcement side is three separate binaries, described in `docs/gateways.md`,
and none of them is needed for this demo:

- The **LLM gateway** (`cmd/shoal-llm-gateway`) is an OpenAI-compatible
  endpoint that asks Shoal for admission before every model call. An existing
  client is governed by changing one base URL. See
  `docs/llm-gateway-deploy.md`.
- The **effects gateway** (`cmd/shoal-gateway`) is a worker that pulls
  approved HTTP actions from the dispatch queue and performs each at most once
  under a fenced claim. See `docs/effects-gateway-deploy.md`.
- The **MCP surface** shown in step 10 is the agent-facing read path. Its
  authenticated multi-user form is the Streamable HTTP `/mcp` endpoint mounted
  by `shoal-explore-web`, walked through in `docs/multi-user-demo.md`.
