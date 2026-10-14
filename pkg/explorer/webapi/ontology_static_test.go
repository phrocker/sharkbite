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
	"testing"
)

func TestStaticWorkspaceOntologyPanelStates(t *testing.T) {
	runOntologyUITest(t, `
const none = await runOntologyScenario({
  configured: false,
  identity: {known: false, reading: "unresolved"},
  schema: null,
  version: null,
  concepts: [],
  relationships: [],
  properties: [],
  limits: {},
});
assert.strictEqual(none.ids["ontology-status"].className, "empty-state");
assert.match(none.ids["ontology-status"].textContent, /No active ontology is configured/);
assert.match(none.ids["ontology-status"].textContent, /not an empty ontology/);
assert.strictEqual(none.ids["ontology-details"].children.length, 0);

const empty = await runOntologyScenario({
  configured: true,
  identity: {known: true, schema_id: "schema-empty", version_id: "version-empty", reading: "same_version"},
  schema: {id: "schema-empty", key: "empty", name: "Empty"},
  version: {id: "version-empty", version: "v1", created_at: "2026-09-04T00:00:00Z"},
  concepts: [],
  relationships: [],
  properties: [],
  limits: {},
});
assert.strictEqual(empty.ids["ontology-status"].className, "muted");
assert.match(empty.ids["ontology-status"].textContent, /0 concept/);
assert.match(renderedText(empty.ids["ontology-details"]), /Active identity/);
assert.match(renderedText(empty.ids["ontology-details"]), /no relationships/);
assert.notStrictEqual(empty.ids["ontology-status"].textContent, none.ids["ontology-status"].textContent);

const denied = await runOntologyScenario(null, {
  ontologyError: {
    status: 401,
    statusText: "Unauthorized",
    body: {code: "unauthorized", message: "operation describe ontology is not authorized"},
  },
});
assert.strictEqual(denied.ids["ontology-status"].className, "denied");
assert.match(denied.ids["ontology-status"].textContent, /Access denied/);
assert.match(denied.ids["ontology-status"].textContent, /not an empty result/);
assert.strictEqual(denied.ids["ontology-details"].children.length, 0);

const proposalDenied = await runOntologyScenario({
  configured: true,
  identity: {known: true, schema_id: "schema-empty", version_id: "version-empty", reading: "same_version"},
  schema: {id: "schema-empty", key: "empty", name: "Empty"},
  version: {id: "version-empty", version: "v1", created_at: "2026-09-04T00:00:00Z"},
  concepts: [],
  relationships: [],
  properties: [],
  limits: {},
}, {
  proposalsError: {
    status: 401,
    statusText: "Unauthorized",
    body: {code: "unauthorized", message: "operation list ontology proposals is not authorized"},
  },
});
assert.match(renderedText(proposalDenied.ids["ontology-details"]), /Access denied/);
assert.match(renderedText(proposalDenied.ids["ontology-details"]), /not an empty result/);
`)
}

func TestStaticWorkspaceOntologyRendersDeclaredJoins(t *testing.T) {
	runOntologyUITest(t, `
const rich = await runOntologyScenario({
  configured: true,
  identity: {known: true, schema_id: "schema-rich", version_id: "version-rich", reading: "same_version"},
  schema: {id: "schema-rich", key: "workspace", name: "Workspace"},
  version: {id: "version-rich", version: "v1", created_at: "2026-09-04T00:00:00Z"},
  concepts: [
    {id: "concept-person", key: "person", name: "Person", properties: ["property-name"]},
    {id: "concept-org", key: "organization", name: "Organization", properties: ["property-name"]},
  ],
  relationships: [{
    id: "relationship-member-of",
    key: "member_of",
    name: "Member of",
    directed: true,
    from_concepts: ["concept-person"],
    to_concepts: ["concept-org"],
    properties: ["property-role"],
  }],
  properties: [
    {id: "property-name", key: "name", name: "Name", value_type: "string", constraints: [
      {kind: "required"},
      {kind: "allowed_values", allowed_values: [
        {type: "string", value: "Alice"},
        {type: "string", value: "Bob"},
      ]},
    ]},
    {id: "property-role", key: "role", name: "Role", value_type: "string", constraints: [
      {kind: "pattern", pattern: "^[A-Za-z ]+$"},
    ]},
  ],
  limits: {},
});
const text = renderedText(rich.ids["ontology-details"]);
assert.match(text, /Active identity/);
assert.match(text, /Declared joins/);
assert.match(text, /Person → Organization/);
assert.match(text, /Directed relationship/);
assert.match(text, /Role/);
assert.match(text, /required/);
assert.match(text, /allowed_values: string Alice, string Bob/);
`)
}

func TestStaticWorkspaceOntologyProposalLifecycleControls(t *testing.T) {
	runOntologyUITest(t, `
const rendered = await runOntologyScenario({
  configured: true,
  identity: {known: true, schema_id: "schema-rich", version_id: "version-rich", reading: "same_version"},
  schema: {id: "schema-rich", key: "workspace", name: "Workspace"},
  version: {id: "version-rich", version: "v1", created_at: "2026-09-04T00:00:00Z"},
  concepts: [{id: "concept-person", key: "person", name: "Person", properties: ["property-name"]}],
  relationships: [],
  properties: [{id: "property-name", key: "name", name: "Name", value_type: "string", constraints: []}],
  limits: {},
}, {
  proposals: {
    proposals: [{
      id: "proposal-1",
      base_schema_id: "schema-rich",
      base_version_id: "version-rich",
      proposed_by: "development-principal@localhost",
      rationale: "add organization joins",
      created_at: "2026-09-04T00:01:00Z",
      updated_at: "2026-09-04T00:02:00Z",
      state: "submitted",
      proposed_ontology: {identity: {version_id: "version-v2"}},
      transitions: [{from: "draft", to: "submitted", actor: "author", note: "ready"}],
    }],
    limits: {},
  },
});
const text = renderedText(rendered.ids["ontology-details"]);
assert.match(text, /Governed proposals/);
assert.match(text, /Create draft proposal/);
assert.match(text, /submitted proposal/);
assert.match(text, /draft → submitted/);
assert.match(text, /Approve/);
assert.match(text, /Reject/);
assert.match(text, /Withdraw/);
const jsonEditor = descendants(rendered.ids["ontology-details"])
  .find((node) => node.className === "ontology-proposal-json");
assert.ok(jsonEditor);
assert.match(jsonEditor.value, /"version": "v2"/);
assert.match(jsonEditor.value, /"properties":/);
`)
}

func TestStaticWorkspaceOntologyProposalEditorPrepopulatesActiveOntologyOnSubmit(t *testing.T) {
	runOntologyUITest(t, `
const createdProposals = [];
const rendered = await runOntologyScenario({
  configured: true,
  identity: {known: true, schema_id: "schema-rich", version_id: "version-rich", reading: "same_version"},
  schema: {id: "schema-rich", key: "workspace", name: "Workspace"},
  version: {id: "version-rich", version: "v1", created_at: "2026-09-04T00:00:00Z"},
  concepts: [
    {id: "concept-person", key: "person", name: "Person", properties: ["property-name"]},
    {id: "concept-org", key: "organization", name: "Organization", properties: ["property-name"]},
  ],
  relationships: [{
    id: "relationship-member-of",
    key: "member_of",
    name: "Member of",
    directed: true,
    from_concepts: ["concept-person"],
    to_concepts: ["concept-org"],
    properties: ["property-role"],
  }],
  properties: [
    {id: "property-name", key: "name", name: "Name", value_type: "string", constraints: [{kind: "required"}]},
    {id: "property-role", key: "role", name: "Role", value_type: "string", constraints: []},
  ],
  limits: {},
}, {createdProposals});
const form = descendants(rendered.ids["ontology-details"])
  .find((node) => node.className === "ontology-proposal-form");
assert.ok(form);
const rationale = descendants(form)
  .find((node) => node.tagName === "TEXTAREA" && node.placeholder);
rationale.value = "change one concept name";
const conceptName = descendants(form)
  .filter((node) => node.className === "ontology-proposal-concept-name")[0];
if (conceptName) {
  conceptName.value = "Human";
  conceptName.oninput();
}
await form.onsubmit({preventDefault() {}});
assert.strictEqual(createdProposals.length, 1);
const proposed = createdProposals[0].proposed_version;
assert.strictEqual(proposed.concepts.length, 2);
assert.strictEqual(proposed.relationships.length, 1);
assert.strictEqual(proposed.properties.length, 2);
assert.deepStrictEqual(proposed.concepts.map((concept) => concept.key), ["person", "organization"]);
assert.deepStrictEqual(proposed.relationships[0].from_concepts, ["person"]);
assert.deepStrictEqual(proposed.relationships[0].to_concepts, ["organization"]);
assert.deepStrictEqual(proposed.relationships[0].properties, ["role"]);
assert.deepStrictEqual(proposed.properties.map((property) => property.key), ["name", "role"]);
assert.deepStrictEqual(proposed.properties[0].constraints, [{kind: "required"}]);
assert.strictEqual(proposed.concepts[0].name, "Human");
assert.strictEqual(descendants(form).filter((node) => node.className === "ontology-remove-concept").length, 2);
assert.strictEqual(descendants(form).filter((node) => node.className === "ontology-remove-relationship").length, 1);
assert.strictEqual(descendants(form).filter((node) => node.className === "ontology-remove-property").length, 2);
`)
}

func TestStaticWorkspaceOntologyProposalEditorRoundTripsRawAndForms(t *testing.T) {
	runOntologyUITest(t, `
const rendered = await runOntologyScenario({
  configured: true,
  identity: {known: true, schema_id: "schema-rich", version_id: "version-rich", reading: "same_version"},
  schema: {id: "schema-rich", key: "workspace", name: "Workspace"},
  version: {id: "version-rich", version: "v1", created_at: "2026-09-04T00:00:00Z"},
  concepts: [{
    id: "concept-person",
    key: "person",
    name: "Person",
    description: "A known person",
    properties: ["property-name"],
  }],
  relationships: [{
    id: "relationship-ref",
    key: "referenced_in",
    name: "Referenced in",
    description: "A citation join",
    directed: true,
    from_concepts: ["concept-person"],
    to_concepts: ["concept-person"],
    properties: ["property-name"],
  }],
  properties: [{
    id: "property-name",
    key: "name",
    name: "Name",
    description: "Display name",
    value_type: "string",
    constraints: [{kind: "allowed_values", allowed_values: [
      {type: "string", value: "Alice"},
      {type: "string", value: "Bob"},
    ]}],
  }],
  limits: {},
});
const form = descendants(rendered.ids["ontology-details"])
  .find((node) => node.className === "ontology-proposal-form");
const rawButton = descendants(form)
  .find((node) => node.tagName === "BUTTON" && node.textContent === "Raw JSON");
const formButton = descendants(form)
  .find((node) => node.tagName === "BUTTON" && node.textContent === "Form editor");
const propertyDescription = descendants(form)
  .find((node) => node.className === "ontology-proposal-property-description");
propertyDescription.value = "Edited display name";
propertyDescription.oninput();
rawButton.onclick();
const raw = descendants(form).find((node) => node.className === "ontology-proposal-json");
let draft = JSON.parse(raw.value);
assert.strictEqual(draft.properties[0].description, "Edited display name");
assert.deepStrictEqual(draft.properties[0].constraints, [{kind: "allowed_values", allowed_values: [
  {type: "string", value: "Alice"},
  {type: "string", value: "Bob"},
]}]);
assert.strictEqual(draft.concepts[0].description, "A known person");
assert.deepStrictEqual(draft.concepts[0].properties, ["name"]);
assert.strictEqual(draft.relationships[0].description, "A citation join");
assert.deepStrictEqual(draft.relationships[0].from_concepts, ["person"]);
draft.concepts[0].description = "Raw edited person";
draft.relationships[0].properties = ["name"];
raw.value = JSON.stringify(draft, null, 2);
formButton.onclick();
const conceptDescription = descendants(form)
  .find((node) => node.className === "ontology-proposal-concept-description");
assert.strictEqual(conceptDescription.value, "Raw edited person");
rawButton.onclick();
draft = JSON.parse(raw.value);
assert.strictEqual(draft.concepts[0].description, "Raw edited person");
assert.deepStrictEqual(draft.properties[0].constraints[0].allowed_values.map((value) => value.value), ["Alice", "Bob"]);
assert.deepStrictEqual(draft.relationships[0].properties, ["name"]);
`)
}

func TestStaticWorkspaceOntologyProposalEditorBoundsErrorDoesNotTruncate(t *testing.T) {
	runOntologyUITest(t, `
const createdProposals = [];
const rendered = await runOntologyScenario({
  configured: true,
  identity: {known: true, schema_id: "schema-rich", version_id: "version-rich", reading: "same_version"},
  schema: {id: "schema-rich", key: "workspace", name: "Workspace"},
  version: {id: "version-rich", version: "v1", created_at: "2026-09-04T00:00:00Z"},
  concepts: [],
  relationships: [],
  properties: [{id: "property-name", key: "name", name: "Name", value_type: "string", constraints: []}],
  limits: {max_properties: 1},
}, {createdProposals});
const form = descendants(rendered.ids["ontology-details"])
  .find((node) => node.className === "ontology-proposal-form");
const addProperty = descendants(form)
  .find((node) => node.tagName === "BUTTON" && node.textContent === "Add property");
addProperty.onclick();
await form.onsubmit({preventDefault() {}});
assert.strictEqual(createdProposals.length, 0);
assert.match(renderedText(form), /property count 2 exceeds limit 1/);
`)
}

func TestStaticWorkspaceOntologyProposalBlastRadius(t *testing.T) {
	runOntologyUITest(t, `
const rendered = await runOntologyScenario({
  configured: true,
  identity: {known: true, schema_id: "schema-rich", version_id: "version-rich", reading: "same_version"},
  schema: {id: "schema-rich", key: "workspace", name: "Workspace"},
  version: {id: "version-rich", version: "v1", created_at: "2026-09-04T00:00:00Z"},
  concepts: [{id: "concept-person", key: "person", name: "Person", properties: ["property-name"]}],
  relationships: [],
  properties: [{id: "property-name", key: "name", name: "Name", value_type: "string", constraints: []}],
  limits: {},
}, {
  proposals: {
    proposals: [{
      id: "proposal-1",
      base_schema_id: "schema-rich",
      base_version_id: "version-rich",
      proposed_by: "development-principal@localhost",
      rationale: "remove case files",
      created_at: "2026-09-04T00:01:00Z",
      updated_at: "2026-09-04T00:02:00Z",
      state: "submitted",
      proposed_ontology: {identity: {version_id: "version-v2"}},
      transitions: [{from: "draft", to: "submitted", actor: "author", note: "ready"}],
    }],
    limits: {},
  },
  blastRadii: {
    "proposal-1": {
      proposal_id: "proposal-1",
      active_version_id: "version-rich",
      proposed_version_id: "version-v2",
      summary: {destructive_changes: 3, additive_changes: 1},
      removed_concepts: [{id: "concept-case", key: "case_file", name: "Case file", properties: []}],
      removed_relationships: [{id: "relationship-referenced", key: "referenced_in", name: "Referenced in"}],
      removed_properties: [],
      changed_concepts: [],
      changed_relationships: [{before: {id: "relationship-member", key: "member_of", name: "Member of"}, after: {id: "relationship-member", key: "member_of", name: "Member of"}, fields: ["to_concepts"]}],
      changed_properties: [],
      added_concepts: [{id: "concept-vessel", key: "vessel", name: "Vessel", properties: []}],
      added_relationships: [],
      added_properties: [],
      limits: {},
    },
  },
});
const text = renderedText(rendered.ids["ontology-details"]);
assert.match(text, /Blast radius/);
assert.match(text, /3 destructive change/);
assert.match(text, /Removed concepts/);
assert.match(text, /Case file \(case_file\) removed/);
assert.match(text, /Changed relationships/);
assert.match(text, /Member of \(member_of\) changed to_concepts/);
assert.match(text, /Added concepts/);
assert.match(text, /Vessel \(vessel\) added/);
assert.doesNotMatch(text, /assertion impact counts/);
assert.doesNotMatch(text, /0 asserted/);
`)
}

func runOntologyUITest(t *testing.T, assertions string) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node is required for executable ontology UI checks in CI")
		}
		t.Skip("node is not available for executable static UI checks")
	}
	command := exec.Command("node", "-e", ontologyHarnessScript(assertions))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("node ontology UI check failed: %v\n%s", err, output)
	}
}

func ontologyHarnessScript(assertions string) string {
	return `
const assert = require("assert");
const fs = require("fs");
const vm = require("vm");
const app = fs.readFileSync("static/app.js", "utf8");

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
  setAttribute(name, value) { this.attributes[name] = String(value); }
  getAttribute(name) { return this.attributes[name]; }
  querySelector(selector) { return descendants(this).find((node) => matches(node, selector)) || null; }
  querySelectorAll(selector) { return descendants(this).filter((node) => matches(node, selector)); }
  getBoundingClientRect() { return {width: 0, height: 0, left: 0, top: 0}; }
  getContext() { return canvasContext; }
}

const canvasContext = {
  setTransform() {}, clearRect() {}, beginPath() {}, moveTo() {}, lineTo() {},
  stroke() {}, fillText() {}, arc() {}, fill() {},
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
    headers: {get(name) {
      const found = headers[String(name).toLowerCase()];
      return found === undefined ? null : found;
    }},
    json: async () => value,
  };
}

class TestFormData {
  constructor() { this.parts = []; }
  append(name, file, filename) { this.parts.push({name, file, filename}); }
}

function makeDocument() {
  const ids = {};
  const pathFromLabel = new Element("label");
  const pathToLabel = new Element("label");
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
    "upload-section", "upload", "upload-drop", "upload-files", "upload-directory", "upload-button",
    "upload-status", "upload-results", "documents", "documents-status", "more",
    "graph-nodes", "graph-edges", "canvas", "selection", "hierarchy",
    "hierarchy-status", "snapshot", "identity", "identity-badge", "ontology",
    "ontology-status", "ontology-details", "login",
  ]) ids[id] = new Element(id === "canvas" ? "canvas" : "div", id);
  for (const id of [
    "documents-status", "hierarchy-status", "evidence-status", "vector-mode-status",
    "upload-status", "identity", "ontology-status",
  ]) ids[id].setAttribute("role", "status");
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
  for (const name of ["evidence", "graph", "ontology"]) panels[name] = ids[name];
  for (const name of ["evidence", "graph", "ontology"]) {
    tabs[name] = new Element("button");
    tabs[name].dataset.panel = name;
  }
  return {document, ids};
}

async function runOntologyScenario(ontology, options = {}) {
  const dom = makeDocument();
  const snapshot = {id: "snapshot123456", as_of: "2026-09-04T00:00:00Z", frontier: 1};
  const ctx = {
    document: dom.document,
    FormData: TestFormData,
    ResizeObserver: class { observe() {} },
    devicePixelRatio: 1,
    URL,
    crypto: require("crypto").webcrypto,
    TextEncoder,
    fetch: async (url, request = {}) => {
      const method = String(request.method || "GET").toUpperCase();
      if (url === "/api/v1/auth-config") return response({configured: false});
      if (url === "/api/v1/identity") return response({
        authenticated: true,
        subject: "development-principal@localhost",
        operations: ["retrieve"],
      });
      if (url === "/api/v1/ontology") {
        if (options.ontologyError) {
          const failure = options.ontologyError;
          return response(failure.body || {}, {
            ok: false,
            status: failure.status,
            statusText: failure.statusText || "Error",
            headers: failure.wwwAuthenticate ? {"www-authenticate": "Bearer"} : {},
          });
        }
        return response(ontology);
      }
      if (url === "/api/v1/ontology/proposals" && method === "POST") {
        const body = JSON.parse(request.body || "{}");
        if (options.createdProposals) options.createdProposals.push(body);
        const proposal = {
          id: "proposal-created",
          base_schema_id: ontology && ontology.identity && ontology.identity.schema_id,
          base_version_id: ontology && ontology.identity && ontology.identity.version_id,
          proposed_by: "development-principal@localhost",
          rationale: body.rationale || "",
          created_at: "2026-09-04T00:03:00Z",
          updated_at: "2026-09-04T00:03:00Z",
          state: "draft",
          proposed_ontology: {identity: {version_id: "version-created"}},
          transitions: [],
        };
        return response({proposal}, {status: 201});
      }
      if (url === "/api/v1/ontology/proposals") {
        if (options.proposalsError) {
          const failure = options.proposalsError;
          return response(failure.body || {}, {
            ok: false,
            status: failure.status,
            statusText: failure.statusText || "Error",
            headers: failure.wwwAuthenticate ? {"www-authenticate": "Bearer"} : {},
          });
        }
        return response(options.proposals || {proposals: [], limits: {}});
      }
      const blastRadius = String(url).match(/^\/api\/v1\/ontology\/proposals\/([^/]+)\/blast-radius$/);
      if (blastRadius) {
        const proposalID = decodeURIComponent(blastRadius[1]);
        const value = options.blastRadii && options.blastRadii[proposalID];
        if (!value) return response({code: "not_found", message: "not found"}, {ok: false, status: 404});
        return response({blast_radius: value});
      }
      if (url === "/api/v1/meta") return response({
        max_upload_files: 8,
        max_upload_file_bytes: 1048576,
        max_upload_total_bytes: 9437184,
        capabilities: {documents: true},
      });
      if (url.endsWith("/documents")) return response({snapshot, documents: [], next_cursor: ""});
      throw new Error("unexpected fetch " + url);
    },
  };
  vm.createContext(ctx);
  vm.runInContext(app, ctx);
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
  await new Promise((resolve) => setImmediate(resolve));
  return {ctx, ids: dom.ids};
}

(async () => {
` + assertions + `
})().catch((error) => { console.error(error.stack || error); process.exit(1); });
`
}
