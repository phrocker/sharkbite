// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package webapi_test

import (
	"strings"
	"testing"
)

func TestOperatorInboxScriptLoadsBeforeWorkspace(t *testing.T) {
	index := readStaticAsset(t, "static/index.html")
	if strings.Index(index, `/assets/inbox.js`) > strings.Index(index, `/assets/app.js`) {
		t.Fatal("inbox listener must be installed before workspace activation")
	}
}

func TestOperatorInboxCannotSubmitTwoVerdictsOrRetryUncertainDecision(t *testing.T) {
	runNodeUITest(t, `
const source = fs.readFileSync("static/inbox.js", "utf8");
class InboxElement {
  constructor(tag = "div") {
    this.tag = tag; this.children = []; this.dataset = {}; this.listeners = {};
    this.className = ""; this.disabled = false; this.open = false; this._text = "";
    this.classList = {toggle() {}};
  }
  set textContent(value) { this._text = String(value); this.children = []; }
  get textContent() { return this._text; }
  appendChild(child) { this.children.push(child); return child; }
  addEventListener(name, fn) { this.listeners[name] = fn; }
  querySelectorAll(selector) {
    const all = [];
    const visit = (node) => { for (const child of node.children) { all.push(child); visit(child); } };
    visit(this);
    if (selector === "button") return all.filter((node) => node.tag === "button");
    if (selector === ".inbox-item") return all.filter((node) => node.className === "inbox-item");
    return [];
  }
}
const ids = Object.fromEntries([
  "inbox", "inbox-status", "inbox-output", "inbox-refresh",
  "inbox-approvals-list", "inbox-approvals-note",
  "inbox-candidates-list", "inbox-candidates-note",
].map((id) => [id, new InboxElement()]));
const row = {id: "YXBwcm92YWw", version: 1, state: "pending", request_digest: "ZGlnZXN0",
  policy_generation: 3, agent_id: "YWdlbnQ", agent_generation: 1,
  capability: "tools", action: "deploy", object_id: "b2JqZWN0", requester: "alice",
  requester_actor: "agent", requested_at: "2026-10-31T12:00:00Z", deadline: "2026-11-01T12:00:00Z"};
const listing = {generated_at: "2026-10-31T12:00:00Z", approvals: {available: true,
  groups: [{capability: "tools", action: "deploy", items: [{kind: "approval", id: row.id, approval: row}]}]},
  candidates: {available: false, items: []}};
let decideCalls = 0;
let inboxCalls = 0;
let statusCalls = 0;
let finishDecision;
const pendingDecision = new Promise((resolve) => { finishDecision = resolve; });
let finishFirstStatus;
const firstStatus = new Promise((resolve) => { finishFirstStatus = resolve; });
const response = (body, ok = true, status = 200) => ({ok, status, statusText: "Conflict", json: async () => body});
const mock = {
  document: {
    getElementById: (id) => ids[id], createElement: (tag) => new InboxElement(tag),
    querySelectorAll: (selector) => ids["inbox"].querySelectorAll(selector),
    addEventListener() {},
  },
  window: {crypto: require("crypto").webcrypto}, Date, btoa, Uint8Array,
  authHeaders: () => ({}), needsReauthentication: () => false,
  apiError: (body, reply) => Object.assign(new Error(body.message), {status: reply.status, code: body.code}),
  fetch: async (path) => {
    if (path === "/api/v1/inbox") { inboxCalls++; return response(listing); }
    if (path.endsWith("/status")) {
      statusCalls++;
      return statusCalls === 1 ? firstStatus : response({...row, input: {target: "preview"}});
    }
    if (path.endsWith("/resolve")) return response({generation: 1, capabilities: []});
    if (path.endsWith("/decide")) { decideCalls++; return pendingDecision; }
    throw new Error("unexpected inbox request " + path);
  },
};
vm.runInNewContext(source, mock);
await ids["inbox-refresh"].onclick();
const group = ids["inbox-approvals-list"].children[0];
const details = group.children[1];
ids["inbox"].children.push(group);
details.open = true;
details.listeners.toggle();
details.open = false;
details.listeners.toggle();
details.open = true;
details.listeners.toggle();
await new Promise(setImmediate);
const controls = details.children[2];
const [approve, refuse] = controls.querySelectorAll("button");
assert.ok(approve && refuse);
finishFirstStatus(response({...row, state: "expired"}));
await new Promise(setImmediate);
assert.strictEqual(controls.querySelectorAll("button").length, 3);
const first = approve.onclick();
await refuse.onclick();
assert.strictEqual(decideCalls, 1);
assert.strictEqual(approve.disabled, true);
assert.strictEqual(refuse.disabled, true);
finishDecision(response({message: "policy generation moved", code: "conflict"}, false, 409));
await first;
assert.strictEqual(inboxCalls, 2);
assert.strictEqual(controls.querySelectorAll("button").length, 0);
assert.match(ids["inbox-status"].textContent, /policy generation moved/);
`)
}
