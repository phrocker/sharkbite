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

This walkthrough is a verified end-to-end path for the `shoal-explore` CLI and
the optional `shoal-explore-web` workspace. It starts from a clean checkout and
uses only checked-in Markdown fixtures plus a local `.shoal/explorer-demo`
corpus. The automated test `TestDocumentedDemoWorkflow` exercises the same CLI
sequence with a temporary data directory so the demo cannot silently drift.

The commands below are written for PowerShell from the repository root.

## 1. Start clean

```powershell
Remove-Item -Recurse -Force .shoal\explorer-demo -ErrorAction SilentlyContinue
```

Expected outcome: the demo corpus directory is absent, so the first ingest
creates new Explorer records.

## 2. Ingest two Markdown sources

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

Then ingest the related release checklist:

```powershell
go run .\cmd\shoal-explore ingest `
  -data .shoal\explorer-demo `
  -file docs\explorer-demo-release.md
```

Expected observable outcome: the JSON contains `"Disposition": "applied"`,
`"Document"."Title"` is `"Explorer Release Checklist"`, `"SectionCount"` is
`3`, and `"SpanCount"` is `2`.

## 3. Re-ingest idempotently

```powershell
go run .\cmd\shoal-explore ingest `
  -data .shoal\explorer-demo `
  -file docs\explorer-demo-guide.md
```

Expected observable outcome: the JSON contains `"Disposition": "unchanged"`
and returns the same document and revision IDs as the first guide ingest.

## 4. List documents and keep their IDs

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
JSON list.

## 5. Inspect the hierarchy

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

```powershell
go run .\cmd\shoal-explore-web `
  -data .shoal\explorer-demo `
  -listen 127.0.0.1:8080 `
  -dev-auth
```

Expected observable outcome:

```text
Authenticating every request as development principal development-principal@localhost
Granted 2 pre-existing document(s) in .shoal\explorer-demo to development-principal@localhost: a development-only convenience for -dev-auth on loopback, because the policy catalog is in-memory and does not survive this process (issue #284)
Shoal Explorer listening at http://127.0.0.1:8080
```

Every request must carry an identity. `-dev-auth` mints one fixed development
principal for callers on this host, and the server refuses to start with
`-dev-auth` on any listen address that is not loopback-only, so `:8080`,
`0.0.0.0:8080`, and `[::]:8080` are rejected. Without `-dev-auth` and without a
real authenticator the server refuses to start at all rather than serve
anonymously.

The authorized Explorer client enforces the bound decision on every call, and
its policy catalog lives in memory for the lifetime of the process (issue #284).
The documents ingested in steps 2 and 3 through the CLI therefore carry no
catalog registration, which is why `-dev-auth` on a loopback listener grants them
to the development principal at startup and prints the count above. That startup grant
is development-only: it is refused for a real authenticator and for any listener
another host can reach, and it does not persist across restarts.

The default listen address is also `127.0.0.1:8080`, so the explicit
`-listen` value documents the default. Open <http://127.0.0.1:8080> to use the
workspace. In another shell, the metadata endpoint should also respond:

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
the local workspace.
