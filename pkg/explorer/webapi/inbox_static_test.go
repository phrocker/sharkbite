// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package webapi_test

import (
	"strings"
	"testing"
)

func TestStaticWorkspaceMountsApprovalInbox(t *testing.T) {
	index := readStaticAsset(t, "static/index.html")
	for _, id := range []string{
		`id="tab-inbox"`, `id="inbox"`, `id="inbox-list"`,
		`id="inbox-detail"`, `id="inbox-refresh"`, `id="inbox-more"`,
		`/assets/inbox.js`,
	} {
		if !strings.Contains(index, id) {
			t.Fatalf("inbox HTML missing %s", id)
		}
	}
	app := readStaticAsset(t, "static/app.js")
	if !strings.Contains(app, `capability("fleet_approvals")`) ||
		!strings.Contains(app, `activate("inbox")`) {
		t.Fatal("inbox is not gated and opened from metadata")
	}
	for _, forbidden := range []string{"innerHTML", "insertAdjacentHTML", "eval("} {
		if strings.Contains(readStaticAsset(t, "static/inbox.js"), forbidden) {
			t.Fatalf("inbox must not use %s with approval data", forbidden)
		}
	}
}

func TestApprovalInboxListsInspectsDecidesAndRefusesStaleRows(t *testing.T) {
	runNodeUITest(t, `
const vmInbox = require("vm");
const inboxSource = fs.readFileSync("static/inbox.js", "utf8") + "\nthis.inbox = inbox;";
function element() {
  return {
    children: [], className: "", textContent: "", disabled: false, hidden: false,
    attributes: {},
    append(...items) { this.children.push(...items); },
    replaceChildren(...items) { this.children = [...items]; },
    setAttribute(key, value) { this.attributes[key] = value; },
    querySelectorAll(selector) {
      if (selector !== ".inbox-actions button") return [];
      const actions = this.children.find((child) => child.className === "button-row inbox-actions");
      return actions ? actions.children : [];
    },
    classList: {toggle() {}},
  };
}
function setup(pages, inspected, available = true) {
  const ids = Object.fromEntries(["tab-inbox", "inbox-status", "inbox-list", "inbox-more", "inbox-detail", "inbox-refresh"]
    .map((id) => [id, element()]));
  const requests = [];
  const context = {
    document: {getElementById: (id) => ids[id], createElement: () => element()},
    window: {crypto: require("crypto").webcrypto},
    TextEncoder, btoa, Date,
    authHeaders: () => ({"Shoal-Workspace-ID": "demo"}),
    apiError: (value, response) => Object.assign(new Error(value.message || "request failed"), {status: response.status}),
    needsReauthentication: () => false, isDenied: () => false,
    handleReauthentication() {}, reauthenticationText: () => "sign in",
    deniedText: () => "denied", state: {identity: {}},
    capability: (name) => name === "fleet_approvals" && available,
    fetch: async (path, options) => {
      requests.push({path, body: JSON.parse(options.body), headers: options.headers});
      let value;
      if (path.endsWith("/pending")) value = pages.shift() || {approvals: []};
      else if (path.endsWith("/status")) value = inspected;
      else if (path.endsWith("/decide")) value = {...inspected, state: "approved"};
      else throw new Error("unexpected path " + path);
      return {ok: true, json: async () => value};
    },
  };
  vmInbox.runInNewContext(inboxSource, context);
  return {ids, requests, inbox: context.inbox};
}
const old = {id: "b2xk", version: 1, state: "pending", request_digest: "old-digest",
  policy_generation: 3, capability: "analyze", action: "run", requested_at: "2026-10-28T00:00:00Z"};
const current = {id: "bmV3", version: 2, state: "pending", request_digest: "current-digest",
  policy_generation: 4, capability: "analyze", action: "run", requested_at: "2026-10-30T00:00:00Z"};
const unavailable = setup([], {}, false);
unavailable.inbox.applyCapability();
await unavailable.inbox.open();
assert.strictEqual(unavailable.ids["tab-inbox"].disabled, true);
assert.strictEqual(unavailable.requests.length, 0);
const live = setup([{approvals: [old, current]}, {approvals: []}], {...current, input: {prompt: "review"}});
live.inbox.applyCapability();
assert.strictEqual(live.ids["tab-inbox"].disabled, false);
await live.inbox.open();
assert.match(live.ids["inbox-list"].children[0].children[1].textContent, /2026-10-30/);
await live.ids["inbox-list"].children[0].onclick();
const actions = live.ids["inbox-detail"].querySelectorAll(".inbox-actions button");
assert.strictEqual(actions.length, 2);
assert.ok(live.ids["inbox-detail"].children.some((item) => item.children.some((child) => child.textContent === '{\n  "prompt": "review"\n}')));
await actions[0].onclick();
const decision = live.requests.find((request) => request.path.endsWith("/decide"));
assert.strictEqual(decision.body.verdict, "approve");
assert.strictEqual(decision.body.request_digest, current.request_digest);
assert.strictEqual(decision.body.policy_generation, current.policy_generation);
assert.strictEqual(decision.headers["Shoal-Workspace-ID"], "demo");
assert.strictEqual(live.ids["inbox-list"].children[0].textContent, "No pending fleet approvals are visible in this workspace.");

const stale = setup([{approvals: [current]}], {...current, policy_generation: 5, condition: "policy_generation_moved"});
await stale.inbox.open();
await stale.ids["inbox-list"].children[0].onclick();
assert.strictEqual(stale.ids["inbox-detail"].querySelectorAll(".inbox-actions button").length, 0);
assert.match(stale.ids["inbox-status"].textContent, /changed/);
assert.strictEqual(stale.requests.filter((request) => request.path.endsWith("/decide")).length, 0);
`)
}
