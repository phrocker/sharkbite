// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
package webapi_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestStaticTeamOverviewDashboardContract(t *testing.T) {
	index := readStaticAsset(t, "static/index.html")
	for _, marker := range []string{
		`id="tab-team"`,
		`id="team-form"`,
		`id="team-summary"`,
		`id="team-people"`,
		`id="team-agents"`,
		`id="team-work-items"`,
		`id="team-trends"`,
		`id="team-actors"`,
		`id="team-bottlenecks"`,
		`id="team-improvements"`,
		`id="team-automation"`,
		`id="team-activity"`,
		`id="team-evidence"`,
		`id="team-more"`,
		`id="team-export"`,
		`id="team-print"`,
	} {
		if !strings.Contains(index, marker) {
			t.Fatalf("team dashboard HTML missing %s", marker)
		}
	}
	appIndex := strings.Index(index, `<script src="/assets/app.js"></script>`)
	teamIndex := strings.Index(
		index, `<script src="/assets/team-overview.js"></script>`)
	if appIndex < 0 || teamIndex < appIndex {
		t.Fatal("team overview script must load after app.js auth helpers")
	}

	script := readStaticAsset(t, "static/team-overview.js")
	for _, forbidden := range []string{
		"innerHTML", "insertAdjacentHTML", "document.write", "eval(",
		"localStorage", "sessionStorage",
	} {
		if strings.Contains(script, forbidden) {
			t.Fatalf("team-overview.js must not use %s", forbidden)
		}
	}
	for _, required := range []string{
		`"/api/v1/team/overview"`,
		`history_days`,
		`next_cursor`,
		"`Heuristic signal · ",
		`"Team data is unavailable or withheld.`,
		`JSON.stringify(payload, null, 2)`,
		`window.print()`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("team-overview.js missing %q", required)
		}
	}

	style := readStaticAsset(t, "static/style.css")
	for _, marker := range []string{
		".team-card-grid",
		".team-trend-row",
		".team-insight",
		"@media print",
		".panel#team{display:block!important}",
	} {
		if !strings.Contains(style, marker) {
			t.Fatalf("team dashboard CSS missing %q", marker)
		}
	}
}

func TestStaticTeamOverviewLoadsPagesExportsAndPrints(t *testing.T) {
	runTeamOverviewNodeTest(t, `
const first = overviewResponse({
  activities: [{
    id: "activity-1", kind: "interaction",
    recorded_at: "2026-09-07T12:00:00Z", actor_id: "person-a",
    operation: "review", manual: true,
    evidence: [{kind: "interaction", id: "session-1", source_id: "source-wire", policy_id: "policy-wire"}],
  }],
  next_cursor: "cursor-2",
  bounds: {...baseBounds, complete: false, actions_truncated: true},
});
const second = overviewResponse({
  activities: [{
    id: "activity-2", kind: "fleet_action",
    recorded_at: "2026-09-07T13:00:00Z", actor_id: "agent-a",
    operation: "dispatch", state: "succeeded",
    evidence: [{kind: "fleet_action", id: "action-2", source_id: "source-wire", policy_id: "policy-wire"}],
  }],
  next_cursor: "",
});
const refreshed = overviewResponse({
  activities: [{
    id: "activity-3", kind: "graph_activity",
    recorded_at: "2026-09-07T14:00:00Z", actor_id: "person-a",
    operation: "refresh",
    evidence: [{kind: "graph_node", id: "activity-node-3"}],
  }],
  next_cursor: "",
});
const scenario = await runScenario({responses: [first, second, refreshed]});
scenario.ids["team-id"].value = "team-a";
scenario.ids["team-source-id"].value = "c291cmNl";
scenario.ids["team-policy-id"].value = "cG9saWN5";
scenario.ids["team-history-days"].value = "14";
scenario.ids["team-page-size"].value = "25";
await scenario.ids["team-form"].listeners.submit({preventDefault() {}});

assert.strictEqual(scenario.requests.length, 1);
assert.strictEqual(scenario.requests[0].url, "/api/v1/team/overview");
assert.strictEqual(scenario.requests[0].options.method, "POST");
assert.strictEqual(
  scenario.requests[0].options.headers.authorization, "Bearer unit-token");
assert.deepStrictEqual(JSON.parse(scenario.requests[0].options.body), {
  team_id: "team-a", source_id: "c291cmNl", policy_id: "cG9saWN5",
  history_days: 14, limit: 25,
});
assert.strictEqual(scenario.ids["team-dashboard"].hidden, false);
assert.match(renderedText(scenario.ids["team-summary"]), /Active agents 1/);
assert.match(renderedText(scenario.ids["team-people"]), /Alex/);
assert.match(renderedText(scenario.ids["team-agents"]), /Planning Agent/);
assert.match(renderedText(scenario.ids["team-work-items"]), /Review queue/);
assert.match(renderedText(scenario.ids["team-trends"]), /2026-09-07/);
assert.match(renderedText(scenario.ids["team-actors"]), /Alex 3/);
assert.match(renderedText(scenario.ids["team-bottlenecks"]), /Heuristic signal/);
assert.match(renderedText(scenario.ids["team-improvements"]), /Seven Day Improvement/);
assert.match(renderedText(scenario.ids["team-automation"]), /Repeated Manual Operation/);
assert.match(renderedText(scenario.ids["team-evidence"]), /session-1/);
assert.match(renderedText(scenario.ids["team-evidence"]), /source-wire/);
assert.match(scenario.ids["team-bounds"].textContent, /Bounded partial response/);
assert.match(scenario.ids["team-bounds"].textContent, /actions/);
assert.strictEqual(scenario.ids["team-more"].hidden, false);

await scenario.ids["team-more"].listeners.click();
assert.strictEqual(scenario.requests.length, 2);
assert.strictEqual(JSON.parse(scenario.requests[1].options.body).cursor, "cursor-2");
assert.strictEqual(scenario.window.ShoalTeamOverview.state().activityCount, 2);
assert.match(renderedText(scenario.ids["team-activity"]), /dispatch/);
assert.strictEqual(scenario.ids["team-more"].hidden, true);

scenario.ids["team-export"].listeners.click();
assert.strictEqual(scenario.downloads.length, 1);
assert.strictEqual(scenario.downloads[0].download, "team-a-overview.json");
assert.match(scenario.downloads[0].blobText, /"activity-2"/);
assert.strictEqual(scenario.revoked.length, 1);

await scenario.ids["team-refresh"].listeners.click();
assert.strictEqual(scenario.requests.length, 3);
assert.strictEqual(
  Object.prototype.hasOwnProperty.call(
    JSON.parse(scenario.requests[2].options.body), "cursor"),
  false,
);
assert.strictEqual(scenario.window.ShoalTeamOverview.state().activityCount, 1);
assert.match(renderedText(scenario.ids["team-activity"]), /refresh/);

scenario.ids["team-print"].listeners.click();
assert.strictEqual(scenario.printed, 1);
`)
}

func TestStaticTeamOverviewUsesWorkspaceScopeAndHonestFailures(t *testing.T) {
	runTeamOverviewNodeTest(t, `
const scoped = await runScenario({
  workspaceID: "workspace-a",
  workspaceSettings: {
    settings: {
      permitted_source_ids: ["c291cmNl"],
      permitted_policy_ids: ["cG9saWN5"],
    },
  },
  responses: [overviewResponse({})],
});
scoped.ids["team-id"].value = "team-a";
scoped.ids["team-history-days"].value = "14";
scoped.ids["team-page-size"].value = "25";
await scoped.ids["team-form"].listeners.submit({preventDefault() {}});
assert.strictEqual(scoped.ids["team-source-id"].value, "c291cmNl");
assert.strictEqual(scoped.ids["team-policy-id"].value, "cG9saWN5");
assert.strictEqual(scoped.requests[0].url, "/api/v1/workspaces/workspace-a/settings");
assert.strictEqual(scoped.requests[1].url, "/api/v1/team/overview");

const challenged = await runScenario({
  failures: [{status: 401, code: "unauthorized", bearer: true}],
});
challenged.ids["team-id"].value = "team-a";
challenged.ids["team-source-id"].value = "c291cmNl";
challenged.ids["team-policy-id"].value = "cG9saWN5";
await challenged.ids["team-form"].listeners.submit({preventDefault() {}});
assert.strictEqual(challenged.ids["team-status"].className, "reauth");
assert.match(challenged.ids["team-status"].textContent, /Sign in again/);
assert.strictEqual(challenged.reauthenticated, 1);

const denied = await runScenario({
  failures: [{status: 401, code: "unauthorized", bearer: false}],
});
denied.ids["team-id"].value = "team-a";
denied.ids["team-source-id"].value = "c291cmNl";
denied.ids["team-policy-id"].value = "cG9saWN5";
await denied.ids["team-form"].listeners.submit({preventDefault() {}});
assert.strictEqual(denied.ids["team-status"].className, "denied");
assert.match(denied.ids["team-status"].textContent, /Access denied/);
assert.doesNotMatch(denied.ids["team-status"].textContent, /unavailable or withheld/);

const hidden = await runScenario({
  failures: [{status: 404, code: "not_found", bearer: false}],
});
hidden.ids["team-id"].value = "team-a";
hidden.ids["team-source-id"].value = "c291cmNl";
hidden.ids["team-policy-id"].value = "cG9saWN5";
await hidden.ids["team-form"].listeners.submit({preventDefault() {}});
assert.strictEqual(hidden.ids["team-status"].className, "team-withheld");
assert.match(hidden.ids["team-status"].textContent, /unavailable or withheld/);
assert.match(hidden.ids["team-status"].textContent, /does not distinguish/);
assert.strictEqual(hidden.ids["team-dashboard"].hidden, true);
`)
}

func runTeamOverviewNodeTest(t *testing.T, assertions string) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("node is required for executable static UI checks in CI")
		}
		t.Skip("node is not available for executable static UI checks")
	}
	script := `
const assert = require("assert");
const fs = require("fs");
const vm = require("vm");
const source = fs.readFileSync("static/team-overview.js", "utf8");

class ClassList {
  constructor(element) { this.element = element; }
  add(name) {
    const values = new Set(this.element.className.split(/\s+/).filter(Boolean));
    values.add(name);
    this.element.className = [...values].join(" ");
  }
}
class Element {
  constructor(tag, id = "") {
    this.tagName = tag.toUpperCase();
    this.id = id;
    this.children = [];
    this.parent = null;
    this.listeners = {};
    this.attributes = {};
    this.className = "";
    this.classList = new ClassList(this);
    this.textContent = "";
    this.hidden = false;
    this.disabled = false;
    this.value = "";
    this.href = "";
    this.download = "";
    this.dateTime = "";
  }
  append(...values) {
    for (const value of values.flat()) {
      if (value === undefined || value === null) continue;
      value.parent = this;
      this.children.push(value);
    }
  }
  replaceChildren(...values) { this.children = []; this.append(...values); }
  setAttribute(name, value) { this.attributes[name] = String(value); }
  getAttribute(name) { return this.attributes[name]; }
  addEventListener(name, listener) { this.listeners[name] = listener; }
  click() {
    this.clicked = true;
    if (this.listeners.click) return this.listeners.click({preventDefault() {}});
  }
  remove() {
    if (!this.parent) return;
    this.parent.children = this.parent.children.filter((value) => value !== this);
  }
}
function descendants(value) {
  const result = [];
  for (const child of value.children || []) {
    result.push(child, ...descendants(child));
  }
  return result;
}
function renderedText(value) {
  return [value, ...descendants(value)]
    .map((item) => item.textContent || "").join(" ");
}
function makeDocument() {
  const ids = {};
  for (const id of [
    "team-form", "team-id", "team-source-id", "team-policy-id",
    "team-history-days", "team-page-size", "team-load", "team-refresh",
    "team-export", "team-print", "team-status", "team-bounds",
    "team-dashboard", "team-summary", "team-people", "team-agents",
    "team-work-items", "team-comparison", "team-trends", "team-bottlenecks",
    "team-actors",
    "team-improvements", "team-automation", "team-activity", "team-more",
    "team-evidence", "team-print-title", "team-print-window",
  ]) ids[id] = new Element(id === "team-form" ? "form" : "div", id);
  ids["team-dashboard"].hidden = true;
  ids["team-refresh"].disabled = true;
  ids["team-export"].disabled = true;
  ids["team-more"].hidden = true;
  ids["team-history-days"].value = "14";
  ids["team-page-size"].value = "25";
  const body = new Element("body", "body");
  return {
    ids, body,
    getElementById(id) { return ids[id] || null; },
    createElement(tag) { return new Element(tag); },
  };
}
function httpResponse(value, options = {}) {
  return {
    ok: options.ok === undefined ? true : options.ok,
    status: options.status || 200,
    statusText: options.statusText || "OK",
    headers: {get(name) {
      return String(name).toLowerCase() === "www-authenticate" && options.bearer
        ? "Bearer"
        : null;
    }},
    json: async () => value,
  };
}
class TestBlob {
  constructor(parts, options = {}) {
    this.content = parts.map(String).join("");
    this.type = options.type || "";
  }
}
const baseBounds = {
  as_of: "2026-09-07T14:00:00Z", since: "2026-08-24T14:00:00Z",
  complete: true, graph_truncated: false, agents_truncated: false,
  actions_truncated: false, interactions_truncated: false,
};
function overviewResponse(overrides = {}) {
  return {
    team: {id: "team-a", name: "Delivery Team", node_id: "team-node", source_id: "source-wire", policy_id: "policy-wire"},
    people: [{id: "person-a", name: "Alex", subject_id: "subject-a", node_id: "person-node"}],
    agents: [{id: "agent-a", name: "Planning Agent", node_id: "agent-node", active: true, generation: 2}],
    work_items: [{id: "work-a", title: "Review queue", status: "blocked", node_id: "work-node", assigned_to: ["person-a"], blocked_by: ["work-b"]}],
    metrics: {
      active_agents: 1,
      action_states: {queued: 2, claimed: 1, succeeded: 4, failed: 1, canceled: 0},
      completed: 4, failed: 1, median_cycle_time_seconds: 7200,
      queue: {count: 2, median_age_seconds: 1800, oldest_age_seconds: 7200, aged_count: 1, aged_after_seconds: 3600},
      activity_by_actor: [{actor_id: "person-a", count: 3}],
      daily: [{date: "2026-09-07", activities: 3, manual: 1, interactions: 1, actions: 1, completed: 1, failed: 0}],
      seven_day_comparison: {
        current: {completed: 4, failed: 1, failure_rate: .2, median_cycle_time_seconds: 7200},
        previous: {completed: 2, failed: 2, failure_rate: .5, median_cycle_time_seconds: 10800},
      },
    },
    activities: [],
    insights: [
      {kind: "bottleneck", label: "aged_queue", severity: "warning", heuristic: true, description: "one queue is aged", evidence: [{kind: "fleet_action", id: "action-1"}]},
      {kind: "improvement", label: "seven_day_improvement", severity: "notice", heuristic: true, description: "current window improved", evidence: [{kind: "fleet_action", id: "action-3"}]},
      {kind: "automation_candidate", label: "repeated_manual_operation", severity: "notice", heuristic: true, description: "review repeats", evidence: [{kind: "interaction", id: "session-1", source_id: "source-wire", policy_id: "policy-wire"}]},
    ],
    bounds: baseBounds,
    next_cursor: "",
    ...overrides,
  };
}
async function runScenario(options = {}) {
  const document = makeDocument();
  const requests = [];
  const downloads = [];
  const revoked = [];
  let printed = 0;
  let reauthenticated = 0;
  let responseIndex = 0;
  const window = {
    ShoalWorkspaceSettings: {
      selectedWorkspaceID: () => options.workspaceID || "",
    },
    addEventListener(name, callback) {
      if (name === "DOMContentLoaded") callback();
    },
    print() { printed++; },
  };
  const objectURLs = new Map();
  const URLValue = {
    createObjectURL(blob) {
      const value = "blob:unit-" + (objectURLs.size + 1);
      objectURLs.set(value, blob);
      return value;
    },
    revokeObjectURL(value) { revoked.push(value); },
  };
  document.body.append = function(...values) {
    Element.prototype.append.call(this, ...values);
    for (const value of values) {
      const originalClick = value.click.bind(value);
      value.click = function() {
        originalClick();
        const blob = objectURLs.get(value.href);
        downloads.push({
          download: value.download,
          blobText: blob ? blob.content : null,
          blob,
        });
      };
    }
  };
  const ctx = {
    assert, document, window, URL: URLValue, Blob: TestBlob, console,
    btoa, atob, encodeURIComponent, setTimeout, clearTimeout,
    authHeaders: () => ({authorization: "Bearer unit-token"}),
    apiError(value, response) {
      const error = new Error(value.message || response.statusText);
      error.code = value.code || "";
      error.status = response.status;
      error.reauthenticate = Boolean(response.headers.get("WWW-Authenticate"));
      return error;
    },
    needsReauthentication: (error) =>
      error.status === 401 && error.reauthenticate === true,
    isDenied: (error) =>
      error.status === 401 && error.reauthenticate !== true,
    handleReauthentication: () => { reauthenticated++; },
    reauthenticationText: () => "Sign in again to continue.",
    fetch: async (url, requestOptions = {}) => {
      requests.push({url, options: requestOptions});
      if (String(url).includes("/workspaces/")) {
        return httpResponse(options.workspaceSettings || {settings: {}});
      }
      const failure = (options.failures || [])[responseIndex];
      if (failure) {
        responseIndex++;
        return httpResponse(
          {code: failure.code, message: failure.code},
          {ok: false, status: failure.status, bearer: failure.bearer});
      }
      const value = (options.responses || [overviewResponse({})])[
        Math.min(responseIndex, (options.responses || []).length - 1)];
      responseIndex++;
      return httpResponse(value);
    },
  };
  vm.createContext(ctx);
  vm.runInContext(source, ctx);
  return {
    ids: document.ids, requests, downloads, revoked, window,
    get printed() { return printed; },
    get reauthenticated() { return reauthenticated; },
  };
}

(async () => {
` + assertions + `
})().catch((error) => { console.error(error.stack || error); process.exit(1); });
`
	command := exec.Command("node", "-e", script)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("team overview node UI check failed: %v\n%s", err, output)
	}
}
