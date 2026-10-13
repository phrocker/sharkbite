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

package webapi_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestStaticWorkspaceVectorModeBehavior(t *testing.T) {
	runNodeUITest(t, `
const unavailable = await runScenario({
  documents: true, document: true, retrieve: true, neighborhood: true, path: true,
});
assert.strictEqual(unavailable.ids["mode-vector"].disabled, true);
assert.strictEqual(unavailable.ids["mode-vector"].checked, false);
assert.match(unavailable.ids["vector-mode-status"].textContent, /did not advertise vector support/);
unavailable.ids["mode-vector"].checked = true;
unavailable.ids.query.value = "knowledge graph";
await unavailable.ids.search.onsubmit({preventDefault() {}});
assert.deepStrictEqual(unavailable.retrieveBody.query.modes, ["lexical", "tree", "graph"]);

const advertised = await runScenario({
  documents: true, document: true, retrieve: true, neighborhood: true, path: true,
  vector_retrieval: true,
});
assert.strictEqual(advertised.ids["mode-vector"].disabled, false);
assert.strictEqual(advertised.ids["vector-mode-status"].hidden, true);
advertised.ids["mode-vector"].checked = true;
advertised.ids.query.value = "knowledge graph";
await advertised.ids.search.onsubmit({preventDefault() {}});
assert.ok(advertised.retrieveBody.query.modes.includes("vector"));

const aliasOnly = await runScenario({
  documents: true, document: true, retrieve: true, neighborhood: true, path: true,
  vector: true, retrieve_vector: true,
});
assert.strictEqual(aliasOnly.ids["mode-vector"].disabled, true);
`)
}

func TestStaticWorkspaceAvoidsMarkupInjectionAPIs(t *testing.T) {
	app := readStaticAsset(t, "static/app.js")
	for _, forbidden := range []string{"innerHTML", "insertAdjacentHTML", "eval("} {
		if strings.Contains(app, forbidden) {
			t.Fatalf("app.js must not use %s with untrusted workspace data", forbidden)
		}
	}
}

func TestStaticWorkspaceUploadBehavior(t *testing.T) {
	runNodeUITest(t, `
const hidden = await runScenario({documents: true, document: true});
assert.strictEqual(hidden.ids["upload-section"].hidden, true);

const uploadedDocs = [{
  document: {id: "doc-upload", title: "guide.md"},
  revision: {id: "rev-upload"},
  source_uri: "upload://workspace/guide.md",
  source_media_type: "text/markdown",
}];
const uploaded = await runScenario(
  {documents: true, document: true, ingest: true},
  [],
  {
    documentSnapshot: {id: "uploadsnapshot", as_of: "2026-08-29T01:00:00Z", frontier: 2},
    documentResponses: [[], uploadedDocs],
    ingestResponse: {
      snapshot: {id: "uploadsnapshot", as_of: "2026-08-29T01:00:00Z", frontier: 2},
      files: [
        {name: "guide.md", disposition: "applied", media_type: "text/markdown", span_count: 1},
        {name: "main.go", disposition: "unchanged", media_type: "text/x-source-code", span_count: 1},
      ],
    },
  },
);
uploaded.ids["upload-files"].files = [{name: "guide.md"}, {name: "main.go"}];
await uploaded.ids.upload.onsubmit({preventDefault() {}});
assert.strictEqual(uploaded.uploadRequest.url, "/api/v1/ingest");
assert.strictEqual(uploaded.uploadRequest.options.method, "POST");
assert.strictEqual(uploaded.uploadRequest.options.headers["x-shoal-workspace-request"], "1");
assert.deepStrictEqual(uploaded.uploadRequest.options.body.parts.map((part) => part.filename), ["guide.md", "main.go"]);
assert.ok(!uploaded.uploadRequest.options.body.parts.some((part) => part.name === "declassification_reason"),
  "an empty reason field must send no reason");
assert.match(uploaded.ids["upload-status"].textContent, /Uploaded 2 file/);
assert.match(uploaded.ids["upload-results"].children[0].textContent, /guide\.md: applied/);
assert.match(uploaded.ids["upload-results"].children[1].textContent, /main\.go: unchanged/);
assert.strictEqual(uploaded.ids.documents.children.length, 1);
assert.match(uploaded.ids.snapshot.textContent, /uploadsnap/);

// #664: a reason typed into the upload form is sent as its own field.
const reasoned = await runScenario(
  {documents: true, document: true, ingest: true},
  [],
  {ingestResponse: {
    snapshot: {id: "reasonsnapshot", as_of: "2026-08-29T01:00:00Z", frontier: 2},
    files: [{name: "secret.md", disposition: "applied", media_type: "text/markdown", span_count: 1}],
  }},
);
reasoned.ids["upload-files"].files = [{name: "secret.md"}];
reasoned.ids["upload-reason"].value = " ticket:SEC-1 ";
await reasoned.ids.upload.onsubmit({preventDefault() {}});
assert.deepStrictEqual(
  reasoned.uploadRequest.options.body.parts.filter((part) => part.name === "declassification_reason")
    .map((part) => part.file),
  ["ticket:SEC-1"]);
assert.deepStrictEqual(
  reasoned.uploadRequest.options.body.parts.filter((part) => part.name === "files")
    .map((part) => part.filename),
  ["secret.md"]);

const extracted = await runScenario(
  {documents: true, document: true, extraction: true},
  [{document: {id: "skill-doc", title: "SKILL.md"}, revision: {id: "skill-rev"}}],
  {extractResponse: {
    snapshot: {id: "extractsnap", as_of: "2026-08-29T00:00:01Z", frontier: 2},
    entity_count: 3,
    relation_count: 2,
    graph_edge_count: 5,
    entity_node_ids: ["entity-1"],
  }, documentSnapshot: {id: "extractsnap", as_of: "2026-08-29T00:00:01Z", frontier: 2}},
);
const extractCard = extracted.ids.documents.children[0];
const extractButton = extractCard.children[1];
assert.strictEqual(extractButton.textContent, "Extract skills");
await extractButton.onclick();
assert.ok(extracted.extractRequest, "extract button should POST /api/v1/extract");
assert.strictEqual(extracted.extractRequest.document_id, "skill-doc");
assert.strictEqual(extracted.extractRequest.revision_id, "skill-rev");
assert.match(renderedText(extractCard), /Extracted 3 entit\(ies\), 2 ontology relation\(s\), and 5 graph edge\(s\)/);
assert.match(extracted.ids.snapshot.textContent, /extractsna/);

const failed = await runScenario(
  {documents: true, document: true, ingest: true},
  [],
  {ingestError: {statusText: "Bad Request", body: {message: "upload failed"}}},
);
failed.ids["upload-files"].files = [{name: "bad.exe"}];
await failed.ids.upload.onsubmit({preventDefault() {}});
assert.match(failed.ids["upload-status"].textContent, /upload failed/);
`)
}

func TestStaticWorkspaceDirectoryPickerMarkup(t *testing.T) {
	index := readStaticAsset(t, "static/index.html")
	if !strings.Contains(index, `id="upload-directory" type="file" webkitdirectory multiple`) {
		t.Fatalf("directory picker input must use webkitdirectory for browser directory selection")
	}
}

func TestStaticWorkspaceDirectoryUploadRenamesSkillCollisions(t *testing.T) {
	runNodeUITest(t, `
const scenario = await runScenario(
  {documents: true, document: true, ingest: true},
  [],
  {
    ingestResponse: {
      snapshot: {id: "skillupload", as_of: "2026-08-29T01:00:00Z", frontier: 3},
      files: [
        {
          name: "skills__alpha__SKILL.md", disposition: "applied", media_type: "text/markdown", span_count: 1,
          skill_file: {expected: true, recognized: true, name: "alpha", description: "Alpha skill", message: "Recognized agent skills file."},
        },
        {
          name: "skills__beta__SKILL.md", disposition: "applied", media_type: "text/markdown", span_count: 1,
          skill_file: {expected: true, recognized: true, name: "beta", description: "Beta skill", message: "Recognized agent skills file."},
        },
      ],
    },
  },
);
scenario.ids["upload-directory"].files = [
  {name: "SKILL.md", webkitRelativePath: "skills/beta/SKILL.md", size: 10},
  {name: "SKILL.md", webkitRelativePath: "skills/alpha/SKILL.md", size: 10},
];
await scenario.ids.upload.onsubmit({preventDefault() {}});
assert.deepStrictEqual(
  scenario.uploadRequest.options.body.parts.map((part) => part.filename),
  ["skills__alpha__SKILL.md", "skills__beta__SKILL.md"],
  "directory SKILL.md filenames must be unique and deterministically sorted",
);
assert.match(
  renderedText(scenario.ids["upload-results"]),
  /Agent skill file recognized: alpha/,
  "recognized skill metadata must be surfaced in upload results",
);
`)
}

func TestStaticWorkspaceDirectoryUploadBatchesAtServerLimit(t *testing.T) {
	runNodeUITest(t, `
function uploadResponse(names, id) {
  return {
    snapshot: {id, as_of: "2026-08-29T01:00:00Z", frontier: Number(id.replace("snap", ""))},
    files: names.map((name) => ({
      name, disposition: "applied", media_type: "text/plain", span_count: 1,
    })),
  };
}

const eightNames = Array.from({length: 8}, (_, index) => "file-" + String(index).padStart(2, "0") + ".txt");
const eight = await runScenario(
  {documents: true, document: true, ingest: true},
  [],
  {ingestResponses: [uploadResponse(eightNames, "snap8")]},
);
eight.ids["upload-directory"].files = eightNames.map((name) => ({name, webkitRelativePath: "dir/" + name, size: 1}));
await eight.ids.upload.onsubmit({preventDefault() {}});
assert.strictEqual(eight.uploadRequests.length, 1, "exactly MaxUploadFiles must stay in one request");
assert.strictEqual(eight.uploadRequests[0].options.body.parts.length, 8, "the full MaxUploadFiles batch must be sent");

const nineNames = Array.from({length: 9}, (_, index) => "file-" + String(index).padStart(2, "0") + ".txt");
const nine = await runScenario(
  {documents: true, document: true, ingest: true},
  [],
  {ingestResponses: [uploadResponse(nineNames.slice(0, 8), "snap8"), uploadResponse(nineNames.slice(8), "snap9")]},
);
nine.ids["upload-directory"].files = nineNames.map((name) => ({name, webkitRelativePath: "dir/" + name, size: 1}));
await nine.ids.upload.onsubmit({preventDefault() {}});
assert.deepStrictEqual(
  nine.uploadRequests.map((request) => request.options.body.parts.length),
  [8, 1],
  "MaxUploadFiles plus one must create a second request rather than truncate",
);
`)
}

func TestStaticWorkspaceDirectoryUploadBatchesRealisticSkillDirectory(t *testing.T) {
	runNodeUITest(t, `
const names = Array.from({length: 24}, (_, index) => "skills__skill-" + String(index).padStart(2, "0") + "__SKILL.md");
function uploadResponse(batchNames, id) {
  return {
    snapshot: {id, as_of: "2026-08-29T01:00:00Z", frontier: Number(id.replace("snap", ""))},
    files: batchNames.map((name, index) => ({
      name,
      disposition: "applied",
      media_type: "text/markdown",
      span_count: 1,
      skill_file: {
        expected: true,
        recognized: true,
        name: "skill-" + String(index).padStart(2, "0"),
        description: "Generated skill",
        message: "Recognized agent skills file.",
      },
    })),
  };
}
const scenario = await runScenario(
  {documents: true, document: true, ingest: true},
  [],
  {
    ingestResponses: [
      uploadResponse(names.slice(0, 8), "snap1"),
      uploadResponse(names.slice(8, 16), "snap2"),
      uploadResponse(names.slice(16), "snap3"),
    ],
  },
);
scenario.ids["upload-directory"].files = Array.from({length: 24}, (_, index) => ({
  name: "SKILL.md",
  webkitRelativePath: "skills/skill-" + String(index).padStart(2, "0") + "/SKILL.md",
  size: 20,
}));
await scenario.ids.upload.onsubmit({preventDefault() {}});
assert.deepStrictEqual(
  scenario.uploadRequests.map((request) => request.options.body.parts.length),
  [8, 8, 8],
  "twenty-four directory files must upload as three bounded requests",
);
assert.strictEqual(
  new Set(scenario.uploadRequests.flatMap((request) => request.options.body.parts.map((part) => part.filename))).size,
  24,
  "all generated directory upload filenames must be unique across batches",
);
assert.match(
  scenario.ids["upload-status"].textContent,
  /Uploaded 24 file\(s\)/,
  "successful realistic skills directory upload must report all files",
);
`)
}

func TestStaticWorkspaceDirectoryUploadSkipsOversizedFiles(t *testing.T) {
	runNodeUITest(t, `
const scenario = await runScenario(
  {documents: true, document: true, ingest: true},
  [],
  {
    meta: {max_upload_files: 8, max_upload_file_bytes: 10, max_upload_total_bytes: 30},
    ingestResponse: {
      snapshot: {id: "smallfiles", as_of: "2026-08-29T01:00:00Z", frontier: 4},
      files: [
        {name: "dir__a.txt", disposition: "applied", media_type: "text/plain", span_count: 1},
        {name: "dir__c.txt", disposition: "applied", media_type: "text/plain", span_count: 1},
      ],
    },
  },
);
scenario.ids["upload-directory"].files = [
  {name: "a.txt", webkitRelativePath: "dir/a.txt", size: 5},
  {name: "b.txt", webkitRelativePath: "dir/b.txt", size: 11},
  {name: "c.txt", webkitRelativePath: "dir/c.txt", size: 5},
];
await scenario.ids.upload.onsubmit({preventDefault() {}});
assert.deepStrictEqual(
  scenario.uploadRequest.options.body.parts.map((part) => part.filename),
  ["dir__a.txt", "dir__c.txt"],
  "oversized file must be skipped without aborting valid siblings",
);
assert.match(
  scenario.ids["upload-status"].textContent,
  /Uploaded 2 of 3 file\(s\); 1 skipped/,
  "partial directory upload must report skipped oversized files",
);
assert.match(
  renderedText(scenario.ids["upload-results"]),
  /Skipped dir\/b\.txt: 11 B exceeds the per-file limit of 10 B/,
  "oversized skipped result must include the actionable size limit",
);
`)
}

func TestStaticWorkspaceUploadReportsMidBatchFailure(t *testing.T) {
	runNodeUITest(t, `
function names(start, count) {
  return Array.from({length: count}, (_, offset) => "dir__file-" + (start + offset) + ".txt");
}
function uploadResponse(batchNames, id) {
  return {
    snapshot: {id, as_of: "2026-08-29T01:00:00Z", frontier: Number(id.replace("snap", ""))},
    files: batchNames.map((name) => ({
      name, disposition: "applied", media_type: "text/plain", span_count: 1,
    })),
  };
}
const scenario = await runScenario(
  {documents: true, document: true, ingest: true},
  [],
  {
    meta: {max_upload_files: 2, max_upload_file_bytes: 100, max_upload_total_bytes: 200},
    ingestResponses: [uploadResponse(names(0, 2), "snap1"), null, uploadResponse(names(4, 2), "snap3")],
    ingestErrors: [null, {statusText: "Bad Request", body: {message: "batch two rejected"}}],
  },
);
scenario.ids["upload-directory"].files = Array.from({length: 6}, (_, index) => ({
  name: "file-" + index + ".txt",
  webkitRelativePath: "dir/file-" + index + ".txt",
  size: 1,
}));
await scenario.ids.upload.onsubmit({preventDefault() {}});
assert.deepStrictEqual(
  scenario.uploadRequests.map((request) => request.options.body.parts.map((part) => part.filename)),
  [names(0, 2), names(2, 2), names(4, 2)],
  "mid-sequence failure must not stop later bounded upload batches",
);
assert.match(
  scenario.ids["upload-status"].textContent,
  /Uploaded 4 of 6 file\(s\); 2 failed/,
  "mid-sequence failure must summarize prior successes and failed batch files",
);
assert.match(
  renderedText(scenario.ids["upload-results"]),
  /Failed dir\/file-2\.txt: batch two rejected/,
  "failed batch files must be visible beside successful batches",
);
const finalDocumentRequest = scenario.documentBodies[scenario.documentBodies.length - 1];
assert.strictEqual(
  finalDocumentRequest.snapshot.id,
  "snap3",
  "document refresh must use the last successful upload snapshot after a later failure",
);
`)
}

func TestStaticWorkspaceUploadRequiresMetadataBounds(t *testing.T) {
	runNodeUITest(t, `
const scenario = await runScenario(
  {documents: true, document: true, ingest: true},
  [],
  {meta: {max_upload_files: 0}},
);
scenario.ids["upload-files"].files = [{name: "guide.md", size: 1}];
await scenario.ids.upload.onsubmit({preventDefault() {}});
assert.strictEqual(scenario.uploadRequests.length, 0, "missing upload bounds must not send an unbounded request");
assert.match(
  scenario.ids["upload-status"].textContent,
  /Upload bounds are unavailable from \/api\/v1\/meta/,
  "missing upload bounds must error clearly",
);
`)
}

func TestStaticWorkspaceSourceLabelsCollapseHostPaths(t *testing.T) {
	runNodeUITest(t, `
const fileURI = "file:///C:/Dev/shoal-web/docs/explorer-demo-guide.md";
const unixURI = "file:///home/build/shoal/docs/guide.md";
const webURI = "https://example.test/private/tree/guide.md?token=secret";
const scenario = await runScenario(
  {documents: true, document: true, retrieve: true, neighborhood: true, path: true},
  [{document: {id: "doc1", title: "Demo"}, revision: {id: "rev1"}, source_uri: fileURI}],
);
const card = scenario.ids.documents.children[0];
const button = card.children[0];
const details = card.children[1];
assert.strictEqual(button.children[1].textContent, "explorer-demo-guide.md");
assert.strictEqual(button.title, "explorer-demo-guide.md");
assert.ok(!button.children[1].textContent.includes("C:/Dev"));
assert.ok(!button.title.includes("C:/Dev"));
assert.strictEqual(details.children[1].textContent, fileURI);
assert.strictEqual(scenario.ctx.sourceLabel(unixURI, "doc"), "guide.md");
assert.strictEqual(scenario.ctx.sourceLabel(webURI, "doc"), "example.test / guide.md");
`)
}

func TestStaticWorkspaceIdentityIsSurfaced(t *testing.T) {
	runNodeUITest(t, `
const scenario = await runScenario({documents: true, document: true, retrieve: true});
const badge = scenario.ids["identity-badge"];
assert.strictEqual(badge.hidden, false);
assert.match(badge.textContent, /development-principal@localhost/);
const panel = renderedText(scenario.ids.identity);
assert.match(panel, /You are/);
assert.match(panel, /development-principal@localhost/);
assert.match(panel, /Shared governed workspace/);
assert.match(panel, /retrieve/);
assert.match(panel, /neighborhood/);
assert.match(panel, /no client-side identity switch/);

const anon = await runScenario(
  {documents: true}, [], {identity: {authenticated: false, operations: []}});
assert.strictEqual(anon.ids["identity-badge"].hidden, true);
assert.match(renderedText(anon.ids.identity), /without an established identity/);

const broken = await runScenario(
  {documents: true}, [],
  {identityError: {statusText: "Service Unavailable", body: {code: "unavailable", message: "identity down"}}});
assert.strictEqual(broken.ids["identity-badge"].hidden, true);
assert.strictEqual(broken.ids.identity.className, "identity identity-unknown");
assert.match(renderedText(broken.ids.identity), /Identity is unavailable/);
`)
}

func TestStaticWorkspaceDistinguishesDeniedFromEmpty(t *testing.T) {
	runNodeUITest(t, `
const scenario = await runScenario({documents: true, retrieve: true});
assert.strictEqual(scenario.ctx.isDenied({code: "unauthorized"}), true);
assert.strictEqual(scenario.ctx.isDenied({status: 401}), true);
assert.strictEqual(scenario.ctx.isDenied({code: "not_found", status: 404}), false);
assert.strictEqual(scenario.ctx.isDenied({code: "internal", status: 500}), false);
assert.strictEqual(scenario.ctx.isDenied(null), false);

const identity = {authenticated: true, subject: "alice@localhost", operations: ["retrieve"]};
const denied = scenario.ctx.deniedText("run this retrieval", identity);
const empty = scenario.ctx.emptyRetrievalText(identity);
assert.notStrictEqual(denied, empty);
assert.match(denied, /Access denied/);
assert.match(denied, /not an empty result/);
assert.match(denied, /alice@localhost/);
assert.match(empty, /No evidence matched/);
assert.match(empty, /alice@localhost/);
`)
}

func TestStaticWorkspaceRetrievalDeniedStateRendersDistinctly(t *testing.T) {
	runNodeUITest(t, `
const denied = await runScenario(
  {documents: true, retrieve: true}, [],
  {retrieveError: {statusText: "Unauthorized", body: {code: "unauthorized", message: "operation retrieve is not authorized"}}});
denied.ids.query.value = "classified";
await denied.ids.search.onsubmit({preventDefault() {}});
assert.strictEqual(denied.ids["evidence-status"].className, "denied");
assert.match(denied.ids["evidence-status"].textContent, /Access denied/);
assert.strictEqual(denied.ids["evidence-results"].children.length, 0);

const empty = await runScenario({documents: true, retrieve: true});
empty.ids.query.value = "nothing here";
await empty.ids.search.onsubmit({preventDefault() {}});
assert.strictEqual(empty.ids["evidence-status"].className, "empty-state");
assert.match(empty.ids["evidence-status"].textContent, /No evidence matched/);
assert.notStrictEqual(
  empty.ids["evidence-status"].className, denied.ids["evidence-status"].className);

const failed = await runScenario(
  {documents: true, retrieve: true}, [],
  {retrieveError: {statusText: "Internal", body: {code: "internal", message: "boom"}}});
failed.ids.query.value = "boom";
await failed.ids.search.onsubmit({preventDefault() {}});
assert.strictEqual(failed.ids["evidence-status"].className, "error");
assert.match(failed.ids["evidence-status"].textContent, /boom/);
`)
}

func TestStaticWorkspaceEmptyDocumentsNamesTheIdentity(t *testing.T) {
	runNodeUITest(t, `
const scenario = await runScenario({documents: true, document: true});
assert.strictEqual(scenario.ids["documents-status"].className, "empty-state");
assert.match(scenario.ids["documents-status"].textContent, /No documents are visible/);
assert.match(scenario.ids["documents-status"].textContent, /development-principal@localhost/);
assert.match(scenario.ids["documents-status"].textContent, /No documents were withheld/);
`)
}

func TestStaticWorkspaceSuppressionClauseDistinguishesThreeCounts(t *testing.T) {
	runNodeUITest(t, `
const scenario = await runScenario({documents: true, retrieve: true});
const identity = {authenticated: true, subject: "alice@localhost", operations: ["retrieve"]};

const none = scenario.ctx.suppressionClause(0, identity);
assert.match(none, /No documents were withheld/);
assert.match(none, /alice@localhost/);
assert.doesNotMatch(none, /[1-9]/);

const one = scenario.ctx.suppressionClause(1, identity);
assert.match(one, /1 document is withheld/);
assert.match(one, /not a confirmation that nothing exists/);
assert.match(one, /alice@localhost/);

const many = scenario.ctx.suppressionClause(3, identity);
assert.match(many, /3 documents are withheld/);

// The three states must never render identically.
assert.notStrictEqual(none, one);
assert.notStrictEqual(one, many);
`)
}

func TestStaticWorkspaceRestrictionClauseIsDistinctFromSuppression(t *testing.T) {
	runNodeUITest(t, `
const scenario = await runScenario({documents: true, retrieve: true});
const identity = {authenticated: true, subject: "alice@localhost", operations: ["retrieve"]};

// The zero case adds nothing, so an unrestricted read is byte-identical to the
// pre-mosaic wording.
const none = scenario.ctx.restrictionClause(0, identity);
assert.strictEqual(none, "");

const one = scenario.ctx.restrictionClause(1, identity);
assert.match(one, /1 document is restricted/);
assert.match(one, /co-occurrence budget/);
assert.match(one, /restriction, not a denial/);
assert.match(one, /alice@localhost/);

const many = scenario.ctx.restrictionClause(3, identity);
assert.match(many, /3 documents are restricted/);

// A restriction must never read the same as an authorization withholding: the
// two reason classes use different vocabulary for the same count.
const suppressedOne = scenario.ctx.suppressionClause(1, identity);
assert.notStrictEqual(one, suppressedOne);
assert.doesNotMatch(suppressedOne, /co-occurrence budget/);
assert.doesNotMatch(one, /nothing exists/);

// The restriction wording discloses only a count, never a domain or document.
assert.doesNotMatch(one, /file:/);
`)
}

func TestStaticWorkspaceRestrictionRendersDistinctClassName(t *testing.T) {
	runNodeUITest(t, `
const visibleDocs = [{
  document: {id: "doc-1", title: "Visible"},
  revision: {id: "rev-1"},
  source_uri: "file:///visible.txt",
}];

// Documents: a restriction renders the dedicated "restricted" class, distinct
// from a plain authorization withholding ("withheld"), a denial, and empty.
const docs = await runScenario(
  {documents: true, document: true}, [],
  {documentResponses: [visibleDocs], documentsRestricted: 1});
assert.strictEqual(docs.ids["documents-status"].className, "restricted");
assert.match(docs.ids["documents-status"].textContent, /1 document is restricted/);
assert.match(docs.ids["documents-status"].textContent, /co-occurrence budget/);
assert.notStrictEqual(docs.ids["documents-status"].className, "withheld");
assert.notStrictEqual(docs.ids["documents-status"].className, "denied");
assert.notStrictEqual(docs.ids["documents-status"].className, "empty-state");

// A restriction takes visual precedence over a co-occurring suppression so the
// stronger, distinguishing signal is what the operator sees.
const both = await runScenario(
  {documents: true, document: true}, [],
  {documentResponses: [visibleDocs], documentsRestricted: 1, documentsSuppressed: 2});
assert.strictEqual(both.ids["documents-status"].className, "restricted");
assert.match(both.ids["documents-status"].textContent, /1 document is restricted/);

// Evidence: the retrieval panel mirrors the same distinct class.
const evidence = await runScenario(
  {documents: true, retrieve: true}, [],
  {retrievalResults: [{id: "span-1", score: 0.9, evidence: []}], retrievalRestricted: 1});
evidence.ids.query.value = "alpha";
await evidence.ids.search.onsubmit({preventDefault() {}});
assert.strictEqual(evidence.ids["evidence-status"].className, "restricted");
assert.match(evidence.ids["evidence-status"].textContent, /1 document is restricted/);
assert.notStrictEqual(evidence.ids["evidence-status"].className, "withheld");
assert.notStrictEqual(evidence.ids["evidence-status"].className, "denied");
`)
}

func TestStaticWorkspaceRetrievalReportsWithheldContext(t *testing.T) {
	runNodeUITest(t, `
// State 1: results, nothing withheld.
const clean = await runScenario(
  {documents: true, retrieve: true}, [],
  {retrievalResults: [{id: "span-1", score: 0.9, evidence: []}], retrievalSuppressed: 0});
clean.ids.query.value = "alpha";
await clean.ids.search.onsubmit({preventDefault() {}});
assert.strictEqual(clean.ids["evidence-status"].className, "muted");
assert.match(clean.ids["evidence-status"].textContent, /Showing 1 evidence result/);
assert.match(clean.ids["evidence-status"].textContent, /No documents were withheld/);

// State 2: results, some withheld.
const some = await runScenario(
  {documents: true, retrieve: true}, [],
  {retrievalResults: [{id: "span-1", score: 0.9, evidence: []}], retrievalSuppressed: 2});
some.ids.query.value = "alpha";
await some.ids.search.onsubmit({preventDefault() {}});
assert.strictEqual(some.ids["evidence-status"].className, "withheld");
assert.match(some.ids["evidence-status"].textContent, /Showing 1 evidence result/);
assert.match(some.ids["evidence-status"].textContent, /2 documents are withheld/);
assert.strictEqual(some.ids["evidence-results"].children.length, 1);

// State 3: no results, yet withheld — the case that used to read as flat empty.
const only = await runScenario(
  {documents: true, retrieve: true}, [],
  {retrievalResults: [], retrievalSuppressed: 4});
only.ids.query.value = "alpha";
await only.ids.search.onsubmit({preventDefault() {}});
assert.strictEqual(only.ids["evidence-status"].className, "withheld");
assert.match(only.ids["evidence-status"].textContent, /No evidence matched/);
assert.match(only.ids["evidence-status"].textContent, /4 documents are withheld/);
assert.strictEqual(only.ids["evidence-results"].children.length, 0);

// The withheld state must be visually distinct from both a denial and a plain
// empty result.
assert.notStrictEqual(only.ids["evidence-status"].className, "denied");
assert.notStrictEqual(only.ids["evidence-status"].className, "empty-state");
`)
}

func TestStaticWorkspaceDocumentsReportWithheldContext(t *testing.T) {
	runNodeUITest(t, `
// No visible documents but some withheld.
const onlyWithheld = await runScenario(
  {documents: true, document: true}, [], {documentsSuppressed: 5});
assert.strictEqual(onlyWithheld.ids["documents-status"].className, "withheld");
assert.match(onlyWithheld.ids["documents-status"].textContent, /No documents are visible/);
assert.match(onlyWithheld.ids["documents-status"].textContent, /5 documents are withheld/);

// Visible documents plus some withheld.
const visibleDocs = [{
  document: {id: "doc-1", title: "Visible"},
  revision: {id: "rev-1"},
  source_uri: "file:///visible.txt",
}];
const mixed = await runScenario(
  {documents: true, document: true}, [],
  {documentResponses: [visibleDocs], documentsSuppressed: 1});
assert.strictEqual(mixed.ids["documents-status"].className, "withheld");
assert.match(mixed.ids["documents-status"].textContent, /Showing 1 document/);
assert.match(mixed.ids["documents-status"].textContent, /1 document is withheld/);
`)
}

func TestStaticWorkspacePkceUsesS256Challenge(t *testing.T) {
	runNodeUITest(t, `
const scenario = await runScenario({documents: true, retrieve: true});
// RFC 7636 appendix B test vector pins base64url(SHA-256(verifier)).
const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk";
const challenge = await scenario.ctx.pkceChallengeFromVerifier(verifier);
assert.strictEqual(challenge, "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM");

const url = scenario.ctx.buildAuthorizeUrl(
  {authorization_endpoint: "https://identity.example/authorize?prompt=login", client_id: "client-1", scope: "openid profile"},
  {redirectUri: "https://app.example/", state: "state-xyz", codeChallenge: challenge});
assert.match(url, /code_challenge_method=S256/);
assert.match(url, /^https:\/\/identity\.example\/authorize\?prompt=login&/);
assert.match(url, /response_type=code/);
assert.match(url, /state=state-xyz/);
// The UI consumes only the access token and never processes an ID token, so it
// must not send an OIDC nonce that it cannot validate.
assert.strictEqual(/[?&]nonce=/.test(url), false);
assert.match(url, /code_challenge=E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM/);
// The plain method must never be emitted.
assert.strictEqual(/code_challenge_method=plain/.test(url), false);
`)
}

func TestStaticWorkspaceLoginRejectsMismatchedState(t *testing.T) {
	runNodeUITest(t, `
const scenario = await runScenario({documents: true, retrieve: true});
const config = {token_endpoint: "https://identity.example/token", client_id: "client-1", scope: "openid"};
const now = 1735689600000;

// verifyCallbackState is exact and refuses a missing stored value.
assert.strictEqual(scenario.ctx.verifyCallbackState("s", "s"), true);
assert.strictEqual(scenario.ctx.verifyCallbackState("s", "other"), false);
assert.strictEqual(scenario.ctx.verifyCallbackState("s", ""), false);
assert.strictEqual(scenario.ctx.verifyCallbackState("s", undefined), false);

// A mismatched callback must be rejected BEFORE the token endpoint is called.
let calls = 0;
const deps = {fetch: async () => { calls++; return {ok: true, json: async () => ({access_token: "leaked"})}; }};
let threw = false;
try {
  await scenario.ctx.exchangeAuthorizationCode(
    config, {code: "c", state: "attacker"},
    {state: "mine", verifier: "v", redirectUri: "https://app/", createdAt: now},
    {...deps, now: () => now});
} catch (_) { threw = true; }
assert.strictEqual(threw, true, "mismatched state must reject");
assert.strictEqual(calls, 0, "token endpoint must not be called on state mismatch");

// A matching callback exchanges the code and returns the token.
let tokenURL = null;
let tokenBody = null;
const okDeps = {fetch: async (url, options) => { tokenURL = url; tokenBody = options.body; return {ok: true, json: async () => ({access_token: "good-token"})}; }};
const token = await scenario.ctx.exchangeAuthorizationCode(
  config, {code: "auth-code", state: "mine"},
  {state: "mine", verifier: "verifier-1", redirectUri: "https://app/", createdAt: now},
  {...okDeps, now: () => now});
assert.strictEqual(token, "good-token");
assert.strictEqual(tokenURL, "https://identity.example/token");
assert.match(tokenBody, /grant_type=authorization_code/);
assert.match(tokenBody, /code_verifier=verifier-1/);
assert.match(tokenBody, /code=auth-code/);

// An expired or legacy timestamp-free flow is rejected before any network call.
for (const stored of [
  {state: "mine", verifier: "v", redirectUri: "https://app/", createdAt: now - (11 * 60 * 1000)},
  {state: "mine", verifier: "v", redirectUri: "https://app/"},
]) {
  let expiredCalls = 0;
  let expired = false;
  try {
    await scenario.ctx.exchangeAuthorizationCode(
      config, {code: "c", state: "mine"}, stored,
      {fetch: async () => { expiredCalls++; }, now: () => now});
  } catch (_) { expired = true; }
  assert.strictEqual(expired, true, "expired login flow must reject");
  assert.strictEqual(expiredCalls, 0, "expired login flow must not reach token endpoint");
}
`)
}

func TestStaticWorkspaceReauthenticationIsDistinctFromDenial(t *testing.T) {
	runNodeUITest(t, `
const scenario = await runScenario({documents: true, retrieve: true});

// The bearer challenge is the sole re-authentication signal.
const challenged = {headers: {get: (n) => n.toLowerCase() === "www-authenticate" ? "Bearer" : null}};
const unchallenged = {headers: {get: () => null}};
assert.strictEqual(scenario.ctx.challengedForBearer(challenged), true);
assert.strictEqual(scenario.ctx.challengedForBearer(unchallenged), false);

// A token expiry re-authenticates and is NOT a denial.
const expiry = {status: 401, reauthenticate: true, code: "unauthorized"};
assert.strictEqual(scenario.ctx.needsReauthentication(expiry), true);
assert.strictEqual(scenario.ctx.isDenied(expiry), false);

// A governance 401 with no challenge is a denial and never re-auth.
const governance = {status: 401, code: "unauthorized"};
assert.strictEqual(scenario.ctx.needsReauthentication(governance), false);
assert.strictEqual(scenario.ctx.isDenied(governance), true);

// Integration: a challenged 401 renders as re-auth, not access denied.
const reauth = await runScenario(
  {documents: true, retrieve: true}, [],
  {retrieveError: {status: 401, wwwAuthenticate: true, statusText: "Unauthorized", body: {code: "unauthorized", message: "authentication required"}}});
reauth.ids.query.value = "classified";
await reauth.ids.search.onsubmit({preventDefault() {}});
assert.strictEqual(reauth.ids["evidence-status"].className, "reauth");
assert.match(reauth.ids["evidence-status"].textContent, /sign in again/);
assert.strictEqual(/Access denied/.test(reauth.ids["evidence-status"].textContent), false);

// A governance 401 with no challenge still renders as access denied.
const denied = await runScenario(
  {documents: true, retrieve: true}, [],
  {retrieveError: {status: 401, statusText: "Unauthorized", body: {code: "unauthorized", message: "authorization denied"}}});
denied.ids.query.value = "classified";
await denied.ids.search.onsubmit({preventDefault() {}});
assert.strictEqual(denied.ids["evidence-status"].className, "denied");
assert.match(denied.ids["evidence-status"].textContent, /Access denied/);
`)
}

func TestStaticWorkspaceAuthHeaderOmittedWithoutToken(t *testing.T) {
	runNodeUITest(t, `
const scenario = await runScenario({documents: true, retrieve: true});
// With no token acquired (the -dev-auth path), no Authorization header is sent.
assert.strictEqual(Object.keys(scenario.ctx.authHeaders()).length, 0);
assert.strictEqual(scenario.ctx.authHeaders().authorization, undefined);
`)
}

func TestStaticWorkspaceTokenIsNeverPersisted(t *testing.T) {
	app := readStaticAsset(t, "static/app.js")
	if strings.Contains(app, "localStorage") {
		t.Fatal("app.js must never touch localStorage; the access token is memory-only")
	}
	if strings.Count(app, "sessionStorage.setItem") != 1 ||
		!strings.Contains(app, "sessionStorage.setItem(LOGIN_FLOW_KEY") {
		t.Fatal("the only sessionStorage write must be the single-use login flow")
	}
	for _, line := range strings.Split(app, "\n") {
		if strings.Contains(line, "setItem") && strings.Contains(line, "accessToken") {
			t.Fatalf("access token must not be written to storage: %q", line)
		}
	}
}

func TestStaticWorkspaceResponsiveHeaderWrapsBeforeTabletWidths(t *testing.T) {
	html := readStaticAsset(t, "static/index.html")
	style := readStaticAsset(t, "static/style.css")

	for _, want := range []string{
		"header{min-height:58px;display:flex",
		"flex-wrap:wrap",
		"form{display:flex",
		"flex:1 1 36rem",
		"@media(max-width:1100px)",
		"form{flex-basis:100%;max-width:none}",
	} {
		if !strings.Contains(style, want) {
			t.Fatalf("style.css missing responsive layout marker %q", want)
		}
	}
	for _, noisy := range []string{
		`id="documents" aria-live`,
		`id="hierarchy" class="tree" aria-live`,
		`id="evidence" class="panel active" role="tabpanel" aria-labelledby="tab-evidence" aria-live`,
	} {
		if strings.Contains(html, noisy) {
			t.Fatalf("interactive content container should not be a broad live region: %q", noisy)
		}
	}
}

func readStaticAsset(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func runNodeUITest(t *testing.T, assertions string) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		// Asymmetric on purpose: a developer laptop without Node may skip these
		// executable UI checks, but CI must never let them silently vanish into
		// a green run. GitHub Actions always sets CI, so there a missing node is
		// a hard failure rather than a skip.
		if os.Getenv("CI") != "" {
			t.Fatal("node is required for executable static UI checks in CI")
		}
		t.Skip("node is not available for executable static UI checks")
	}
	script := uiHarnessScript(assertions)
	command := exec.Command("node", "-e", script)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("node UI check failed: %v\n%s", err, output)
	}
}

func uiHarnessScript(assertions string) string {
	return `
const assert = require("assert");
const fs = require("fs");
const vm = require("vm");
const app = fs.readFileSync("static/app.js", "utf8") +
  "\nthis.sourceLabel = sourceLabel;" +
  "\nthis.isDenied = isDenied;" +
  "\nthis.deniedText = deniedText;" +
  "\nthis.emptyRetrievalText = emptyRetrievalText;" +
  "\nthis.emptyDocumentsText = emptyDocumentsText;" +
  "\nthis.suppressionClause = suppressionClause;" +
  "\nthis.needsReauthentication = needsReauthentication;" +
  "\nthis.challengedForBearer = challengedForBearer;" +
  "\nthis.authHeaders = authHeaders;" +
  "\nthis.base64UrlEncode = base64UrlEncode;" +
  "\nthis.pkceChallengeFromVerifier = pkceChallengeFromVerifier;" +
  "\nthis.buildAuthorizeUrl = buildAuthorizeUrl;" +
  "\nthis.verifyCallbackState = verifyCallbackState;" +
  "\nthis.callbackParams = callbackParams;" +
  "\nthis.exchangeAuthorizationCode = exchangeAuthorizationCode;" +
  "\nthis.base64UrlDecode = base64UrlDecode;" +
  "\nthis.mergeGraph = mergeGraph;" +
  "\nthis.selectEdge = selectEdge;" +
  "\nthis.edgeIsDerived = edgeIsDerived;" +
  "\nthis.edgeDerivationScore = edgeDerivationScore;" +
  "\nthis.renderGraphList = renderGraphList;" +
  "\nthis.state = state;";

class ClassList {
  constructor(element) { this.element = element; }
  add(name) { this.set(name, true); }
  remove(name) { this.set(name, false); }
  toggle(name, force) { this.set(name, force === undefined ? !this.has(name) : force); }
  has(name) { return this.element.className.split(/\s+/).includes(name); }
  set(name, enabled) {
    const values = new Set(this.element.className.split(/\s+/).filter(Boolean));
    if (enabled) values.add(name); else values.delete(name);
    this.element.className = [...values].join(" ");
  }
}

class Element {
  constructor(tag, id = "") {
    this.tagName = tag.toUpperCase();
    this.id = id;
    this.children = [];
    this.dataset = {};
    this.attributes = {};
    this.className = "";
    this.classList = new ClassList(this);
    this.textContent = "";
    this.hidden = false;
    this.disabled = false;
    this.checked = false;
    this.value = "";
    this.type = "";
    this.title = "";
    this.onclick = null;
    this.onsubmit = null;
    this.onkeydown = null;
  }
  append(...nodes) {
    for (const node of nodes.flat()) {
      if (node && node.tagName === "FRAGMENT") this.children.push(...node.children);
      else this.children.push(node);
    }
  }
  replaceChildren(...nodes) { this.children = []; this.append(...nodes); }
  setAttribute(name, value) { this.attributes[name] = String(value); if (name === "aria-pressed") this.ariaPressed = String(value); }
  getAttribute(name) { return this.attributes[name]; }
  querySelector(selector) { return descendants(this).find((node) => matches(node, selector)) || null; }
  querySelectorAll(selector) { return descendants(this).filter((node) => matches(node, selector)); }
  getBoundingClientRect() { return {width: 0, height: 0, left: 0, top: 0}; }
  getContext() { return canvasContext; }
}

const canvasContext = {
  setTransform() {}, clearRect() {}, beginPath() {}, moveTo() {}, lineTo() {},
  stroke() {}, fillText() {}, arc() {}, fill() {},
  save() {}, restore() {}, setLineDash() {},
};

function descendants(element) {
  const out = [];
  for (const child of element.children || []) {
    if (!child) continue;
    out.push(child, ...descendants(child));
  }
  return out;
}

function renderedText(element) {
  return [element, ...descendants(element)]
    .map((node) => (node && node.textContent) || "")
    .join(" ");
}

function matches(element, selector) {
  if (selector === ".doc") return element.className.split(/\s+/).includes("doc");
  if (selector === ".doc-card") return element.className.split(/\s+/).includes("doc-card");
  if (selector === "[data-section]") return Boolean(element.dataset.section);
  if (selector === "button") return element.tagName === "BUTTON";
  return false;
}

function response(value, options = {}) {
  const headers = options.headers || {};
  return {
    ok: options.ok !== undefined ? options.ok : true,
    status: options.status !== undefined
      ? options.status
      : (options.ok === false ? 400 : 200),
    statusText: options.statusText || "OK",
    headers: {
      get(name) {
        const found = headers[String(name).toLowerCase()];
        return found === undefined ? null : found;
      },
      has(name) {
        return Object.prototype.hasOwnProperty.call(headers, String(name).toLowerCase());
      },
    },
    json: async () => value,
  };
}

class TestFormData {
  constructor() { this.parts = []; }
  append(name, file, filename) { this.parts.push({name, file, filename}); }
}

function makeDocument(capabilities, documents) {
  const ids = {};
  const pathFromLabel = new Element("label");
  const pathToLabel = new Element("label");
  pathFromLabel.attributes.for = "path-from";
  pathToLabel.attributes.for = "path-to";
  const panels = {};
  const tabs = {};
  const inputs = [];
  const document = {
    getElementById(id) { return ids[id]; },
    createElement(tag) { return new Element(tag); },
    createDocumentFragment() { return new Element("fragment"); },
    querySelectorAll(selector) {
      if (selector === ".tab") return Object.values(tabs);
      if (selector === ".tab,.panel") return [...Object.values(tabs), ...Object.values(panels)];
      if (selector === "[for='path-from'],[for='path-to']") return [pathFromLabel, pathToLabel];
      if (selector === "#modes input:checked:not(:disabled)") {
        return inputs.filter((input) => input.checked && !input.disabled && !ids.modes.disabled);
      }
      if (selector === ".doc-card") return descendants(ids.documents).filter((node) => matches(node, selector));
      return [];
    },
    querySelector(selector) {
      const panel = selector.match(/^\[data-panel="(.+)"\]$/);
      if (panel) return tabs[panel[1]];
      return null;
    },
  };
  for (const id of [
    "query", "search", "search-button", "modes", "mode-vector", "mode-vector-control",
    "vector-mode-status", "evidence", "evidence-status", "evidence-results", "expand",
    "continue-expansion", "path-from", "path-to", "find-path", "graph-status",
    "upload-section", "upload", "upload-drop", "upload-files", "upload-directory", "upload-reason", "upload-button",
    "upload-status", "upload-results",
    "documents", "documents-status", "more", "graph-nodes", "graph-edges", "canvas",
    "selection", "hierarchy", "hierarchy-status", "snapshot", "identity", "identity-badge",
  ]) ids[id] = new Element(id === "canvas" ? "canvas" : "div", id);
  for (const id of ["documents-status", "hierarchy-status", "evidence-status", "vector-mode-status", "upload-status", "identity"]) {
    ids[id].setAttribute("role", "status");
  }
  ids.query.value = "";
  ids.search = new Element("form", "search");
  ids.upload = new Element("form", "upload");
  ids["upload-files"] = new Element("input", "upload-files");
  ids["upload-directory"] = new Element("input", "upload-directory");
  ids["upload-button"] = new Element("button", "upload-button");
  ids["upload-drop"] = new Element("label", "upload-drop");
  ids["search-button"] = new Element("button", "search-button");
  ids.modes = new Element("fieldset", "modes");
  for (const value of ["lexical", "tree", "graph", "vector"]) {
    const input = value === "vector" ? ids["mode-vector"] : new Element("input");
    input.value = value;
    input.checked = value !== "vector";
    inputs.push(input);
  }
  ids["mode-vector-control"] = new Element("label", "mode-vector-control");
  ids["vector-mode-status"] = new Element("span", "vector-mode-status");
  for (const name of ["evidence", "graph"]) panels[name] = new Element("div");
  for (const name of ["evidence", "graph"]) {
    tabs[name] = new Element("button");
    tabs[name].dataset.panel = name;
  }
  return {document, ids};
}

async function runScenario(capabilities, documents = [], scenarioOptions = {}) {
  const dom = makeDocument(capabilities, documents);
  let retrieveBody = null;
  const uploadRequests = [];
  const documentBodies = [];
  const neighborhoodRequests = [];
  const recomputeRequests = [];
  let extractRequest = null;
  let documentCalls = 0;
  const snapshot = {id: "snapshot123456", as_of: "2026-08-29T00:00:00Z", frontier: 1};
  const ctx = {
    document: dom.document,
    FormData: TestFormData,
    ResizeObserver: class { constructor(callback) { this.callback = callback; } observe() {} },
    devicePixelRatio: 1,
    URL,
    crypto: require("crypto").webcrypto,
    TextEncoder,
    fetch: async (url, requestOptions = {}) => {
      if (url === "/api/v1/meta") return response({
        max_upload_files: 8,
        max_upload_file_bytes: 1048576,
        max_upload_total_bytes: 9437184,
        capabilities,
        ...(scenarioOptions.meta || {}),
      });
      if (url === "/api/v1/auth-config") {
        return response("authConfig" in scenarioOptions
          ? scenarioOptions.authConfig
          : {configured: false});
      }
      if (url === "/api/v1/identity") {
        if (scenarioOptions.identityError) {
          return response(scenarioOptions.identityError.body || {}, {
            ok: false,
            statusText: scenarioOptions.identityError.statusText || "Error",
          });
        }
        return response("identity" in scenarioOptions ? scenarioOptions.identity : {
          authenticated: true,
          subject: "development-principal@localhost",
          actor: "shoal-explore-web-dev-auth",
          authorization_domain: "shoal-explore-web",
          operations: ["ingest", "list", "read", "connect", "neighborhood", "retrieve"],
          policy_generation: 1,
          audit_purpose: "localhost development workspace",
          request_id: "dev-request-abc123",
        });
      }
      if (url.endsWith("/documents")) {
        if (requestOptions.body) documentBodies.push(JSON.parse(requestOptions.body));
        const sequence = scenarioOptions.documentResponses || [documents];
        const docs = sequence[Math.min(documentCalls, sequence.length - 1)];
        const documentSnapshot = scenarioOptions.documentSnapshot || snapshot;
        documentCalls++;
        return response({
          snapshot: documentSnapshot, documents: docs, next_cursor: "",
          suppressed: scenarioOptions.documentsSuppressed || 0,
          restricted: scenarioOptions.documentsRestricted || 0,
        });
      }
      if (url === "/api/v1/ingest") {
        uploadRequests.push({url, options: requestOptions});
        const index = uploadRequests.length - 1;
        const errors = scenarioOptions.ingestErrors || [];
        const error = errors[index] || scenarioOptions.ingestError;
        if (error) {
          return response(error.body, {
            ok: false,
            statusText: error.statusText,
          });
        }
        const responses = scenarioOptions.ingestResponses || [];
        return response(responses[index] || scenarioOptions.ingestResponse);
      }
      if (url.endsWith("/extract")) {
        extractRequest = JSON.parse(requestOptions.body);
        if (scenarioOptions.extractError) {
          return response(scenarioOptions.extractError.body || {}, {
            ok: false,
            statusText: scenarioOptions.extractError.statusText || "Error",
          });
        }
        return response(scenarioOptions.extractResponse || {snapshot});
      }
      if (url.endsWith("/retrieve")) {
        retrieveBody = JSON.parse(requestOptions.body);
        if (scenarioOptions.retrieveError) {
          const failure = scenarioOptions.retrieveError;
          return response(failure.body || {}, {
            ok: false,
            status: failure.status,
            statusText: failure.statusText || "Error",
            headers: failure.wwwAuthenticate ? {"www-authenticate": "Bearer"} : {},
          });
        }
        return response({
          snapshot,
          retrieval: {results: scenarioOptions.retrievalResults || []},
          suppressed: scenarioOptions.retrievalSuppressed || 0,
          restricted: scenarioOptions.retrievalRestricted || 0,
        });
      }
      if (url.endsWith("/neighborhood")) {
        neighborhoodRequests.push(JSON.parse(requestOptions.body));
        if (scenarioOptions.neighborhoodError) {
          return response(scenarioOptions.neighborhoodError.body || {}, {
            ok: false,
            statusText: scenarioOptions.neighborhoodError.statusText || "Error",
          });
        }
        return response(scenarioOptions.neighborhoodResponse || {snapshot, neighborhood: {nodes: [], edges: []}});
      }
      if (url.endsWith("/derivation/recompute")) {
        recomputeRequests.push(JSON.parse(requestOptions.body));
        const index = recomputeRequests.length - 1;
        const errors = scenarioOptions.recomputeErrors || [];
        const error = errors[index] || scenarioOptions.recomputeError;
        if (error) {
          return response(error.body || {}, {
            ok: false,
            status: error.status,
            statusText: error.statusText || "Error",
          });
        }
        const responses = scenarioOptions.recomputeResponses || [];
        return response(responses[index] || scenarioOptions.recomputeResponse || {snapshot, unchanged: true, digest: "digest", detail: {}});
      }
      throw new Error("unexpected fetch " + url);
    },
  };
  vm.createContext(ctx);
  vm.runInContext(app, ctx);
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
  return {
    ctx, ids: dom.ids,
    get retrieveBody() { return retrieveBody; },
    get uploadRequest() { return uploadRequests[0] || null; },
    get uploadRequests() { return uploadRequests; },
    get documentBodies() { return documentBodies; },
    get extractRequest() { return extractRequest; },
    get neighborhoodRequests() { return neighborhoodRequests; },
    get recomputeRequests() { return recomputeRequests; },
  };
}

(async () => {
` + assertions + `
})().catch((error) => { console.error(error.stack || error); process.exit(1); });
`
}

func TestStaticWorkspaceDecodesWireMetadataWithoutBrowserGlobals(t *testing.T) {
	runNodeUITest(t, `
const scenario = await runScenario({documents: true, neighborhood: true});
const decode = scenario.ctx.base64UrlDecode;
const encode = scenario.ctx.base64UrlEncode;
const encoder = new TextEncoder();
for (const sample of ["", "derived", "ontology.assertion.origin", "0.91", "latent-similar-to", "café ☕"]) {
  const roundTrip = decode(encode(encoder.encode(sample)));
  assert.strictEqual(roundTrip, sample, "base64UrlDecode must invert base64UrlEncode for " + JSON.stringify(sample));
}
`)
}

func TestStaticWorkspaceDistinguishesDerivedEdgesInGraphList(t *testing.T) {
	runNodeUITest(t, `
const scenario = await runScenario({documents: true, neighborhood: true});
const enc = (value) => scenario.ctx.base64UrlEncode(new TextEncoder().encode(value));
const prop = (key, value) => ({key: enc(key), value: enc(value)});

const derivedEdge = {
  id: "assertion-wire-id", from: "node-a", to: "node-b", type: "latent-similar-to",
  properties: [
    prop("ontology.assertion.origin", "derived"),
    prop("ontology.assertion.id", "assertion:abc"),
    prop("ontology.assertion.derivation.id", "derivation:xyz"),
    prop("ontology.assertion.derivation.score", "0.91"),
  ],
};
const assertedEdge = {
  id: "asserted-edge", from: "node-a", to: "node-b", type: "mentions",
  properties: [prop("ontology.assertion.origin", "asserted")],
};

// The edge property is authoritative, so detection must work from the edge
// alone even before any assertion detail is delivered.
assert.strictEqual(scenario.ctx.edgeIsDerived(derivedEdge), true);
assert.strictEqual(scenario.ctx.edgeIsDerived(assertedEdge), false);
assert.strictEqual(scenario.ctx.edgeDerivationScore(derivedEdge), 0.91);
assert.strictEqual(scenario.ctx.edgeDerivationScore(assertedEdge), null);

scenario.ctx.mergeGraph({
  nodes: [{id: "node-a", labels: ["A"]}, {id: "node-b", labels: ["B"]}],
  edges: [derivedEdge, assertedEdge],
});
const buttons = scenario.ids["graph-edges"].querySelectorAll("button");
assert.strictEqual(buttons.length, 2, "each edge renders as a selectable button");
const derivedButton = buttons.find((button) => button.className.split(/\s+/).includes("derived"));
const assertedButton = buttons.find((button) => !button.className.split(/\s+/).includes("derived"));
assert.ok(derivedButton, "the derived edge must carry the derived class");
assert.match(derivedButton.textContent, /^◆/, "the derived edge must be marked distinctly");
assert.match(derivedButton.getAttribute("aria-label"), /^Derived edge,/);
assert.doesNotMatch(assertedButton.textContent, /^◆/, "the asserted edge must not be marked derived");
`)
}

func TestStaticWorkspaceInspectsDerivedEdgeProducerAndScore(t *testing.T) {
	runNodeUITest(t, `
const scenario = await runScenario({documents: true, neighborhood: true});
const enc = (value) => scenario.ctx.base64UrlEncode(new TextEncoder().encode(value));
const prop = (key, value) => ({key: enc(key), value: enc(value)});

const edge = {
  id: "assertion-wire-id", from: "node-a", to: "node-b", type: "latent-similar-to",
  properties: [
    prop("ontology.assertion.origin", "derived"),
    prop("ontology.assertion.derivation.id", "derivation:xyz"),
    prop("ontology.assertion.derivation.score", "0.91"),
  ],
};
const assertion = {
  id: "assertion-wire-id", origin: "derived",
  evidence: [{derivation: {
    id: "derivation:xyz", embedding_model: "unit-embed", embedding_model_version: "v1",
    similarity_metric: "cosine", threshold: 0.85, tessellation_cell: "cell-a",
    iterator_name: "latent-similar-to", score: 0.91,
    iterator_options: [prop("similarityThreshold", "0.85"), prop("maxPairsPerCell", "128")],
  }}],
  provenance: {provider: "unit-provider", model: "unit-model", model_version: "v2"},
};
scenario.ctx.mergeGraph({
  nodes: [{id: "node-a", labels: ["A"]}, {id: "node-b", labels: ["B"]}],
  edges: [edge], assertions: [assertion],
});
scenario.ctx.selectEdge("assertion-wire-id");
const panel = renderedText(scenario.ids.selection);
assert.match(panel, /derived \(latent similarity\)/);
assert.match(panel, /0\.91/, "the similarity score must be shown");
assert.match(panel, /unit-embed \(v1\)/, "the embedding model and version must be shown");
assert.match(panel, /cosine/, "the similarity metric must be shown");
assert.match(panel, /cell-a/, "the tessellation cell must be shown");
assert.match(panel, /latent-similar-to/, "the iterator name must be shown");
assert.match(panel, /similarityThreshold=0\.85/, "iterator options must be decoded and shown");
assert.match(panel, /derivation:xyz/, "the derivation id must be shown");
assert.match(panel, /unit-provider/, "the producer identity must be shown");
assert.strictEqual(scenario.ctx.state.selectedEdge, "assertion-wire-id");
assert.strictEqual(scenario.ctx.state.selected, null, "selecting an edge clears the node selection");

// An asserted edge exposes no producer and no recompute affordance.
const assertedEdge = {
  id: "asserted-edge", from: "node-a", to: "node-b", type: "mentions",
  properties: [prop("ontology.assertion.origin", "asserted")],
};
scenario.ctx.mergeGraph({nodes: [], edges: [assertedEdge]});
scenario.ctx.selectEdge("asserted-edge");
assert.strictEqual(
  scenario.ids.selection.querySelectorAll("button").length, 0,
  "an asserted edge must not offer a recompute button");
`)
}

func TestStaticWorkspaceRecomputeConfirmsDeterministicReproduction(t *testing.T) {
	runNodeUITest(t, `
const snapshot = {id: "recomputesnap", as_of: "2026-09-04T12:00:00Z", frontier: 7};
const scenario = await runScenario(
  {documents: true, neighborhood: true},
  [],
  {recomputeResponses: [
    {snapshot, unchanged: true, digest: "digest-one", detail: {origin: "derived", score: 0.91}},
    {snapshot, unchanged: true, digest: "digest-one", detail: {origin: "derived", score: 0.91}},
  ]},
);
const enc = (value) => scenario.ctx.base64UrlEncode(new TextEncoder().encode(value));
const prop = (key, value) => ({key: enc(key), value: enc(value)});
const edge = {
  id: "assertion-wire-id", from: "node-a", to: "node-b", type: "latent-similar-to",
  properties: [
    prop("ontology.assertion.origin", "derived"),
    prop("ontology.assertion.derivation.score", "0.91"),
  ],
};
scenario.ctx.mergeGraph({
  nodes: [{id: "node-a"}, {id: "node-b"}], edges: [edge],
});
scenario.ctx.selectEdge("assertion-wire-id");
const recomputeButton = scenario.ids.selection.querySelectorAll("button")[0];
assert.ok(recomputeButton, "a derived edge must offer a recompute button");

await recomputeButton.onclick();
assert.strictEqual(scenario.recomputeRequests.length, 1);
assert.strictEqual(
  scenario.recomputeRequests[0].assertion_id, "assertion-wire-id",
  "the browser must post the wire assertion id verbatim, not a decoded form");
assert.strictEqual(
  scenario.recomputeRequests[0].digest, "",
  "the first recompute captures a baseline with no prior digest");
assert.match(renderedText(scenario.ids.selection), /Captured digest/);

await recomputeButton.onclick();
assert.strictEqual(
  scenario.recomputeRequests[1].digest, "digest-one",
  "the second recompute posts the captured digest to verify reproduction");
assert.match(renderedText(scenario.ids.selection), /Reproduced byte-identically/);
`)
}

func TestStaticWorkspaceRecomputeSurfacesChangedInputs(t *testing.T) {
	runNodeUITest(t, `
const snapshot = {id: "recomputesnap", as_of: "2026-09-04T12:00:00Z", frontier: 7};

const changed = await runScenario(
  {documents: true, neighborhood: true},
  [],
  {recomputeResponses: [
    {snapshot, unchanged: true, digest: "digest-one", detail: {origin: "derived", score: 0.91}},
    {snapshot, unchanged: false, digest: "digest-two", detail: {origin: "derived", score: 0.42}},
  ]},
);
const enc = (value) => changed.ctx.base64UrlEncode(new TextEncoder().encode(value));
const prop = (key, value) => ({key: enc(key), value: enc(value)});
const edge = {
  id: "assertion-wire-id", from: "node-a", to: "node-b", type: "latent-similar-to",
  properties: [prop("ontology.assertion.origin", "derived"), prop("ontology.assertion.derivation.score", "0.91")],
};
changed.ctx.mergeGraph({nodes: [{id: "node-a"}, {id: "node-b"}], edges: [edge]});
changed.ctx.selectEdge("assertion-wire-id");
const changedButton = changed.ids.selection.querySelectorAll("button")[0];
await changedButton.onclick();
await changedButton.onclick();
const changedText = renderedText(changed.ids.selection);
assert.match(changedText, /Inputs changed/);
assert.match(changedText, /0\.42/, "the changed similarity score must be reported");

// A derivation whose content-addressed identity no longer reproduces fails
// closed with not-found, which the inspector reports as a changed input.
const gone = await runScenario(
  {documents: true, neighborhood: true},
  [],
  {recomputeError: {status: 404, statusText: "Not Found", body: {code: "not_found", message: "gone"}}},
);
const goneEnc = (value) => gone.ctx.base64UrlEncode(new TextEncoder().encode(value));
const goneProp = (key, value) => ({key: goneEnc(key), value: goneEnc(value)});
const goneEdge = {
  id: "assertion-wire-id", from: "node-a", to: "node-b", type: "latent-similar-to",
  properties: [goneProp("ontology.assertion.origin", "derived")],
};
gone.ctx.mergeGraph({nodes: [{id: "node-a"}, {id: "node-b"}], edges: [goneEdge]});
gone.ctx.selectEdge("assertion-wire-id");
await gone.ids.selection.querySelectorAll("button")[0].onclick();
assert.match(renderedText(gone.ids.selection), /no longer reproduces/);
`)
}
