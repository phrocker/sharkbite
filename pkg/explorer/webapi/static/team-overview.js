(() => {
  "use strict";

  const overviewPath = "/api/v1/team/overview";
  const state = {
    response: null,
    activities: [],
    nextCursor: "",
    request: null,
    loading: false,
    generation: 0,
    evidence: new Map(),
    sources: new Map(),
  };
  let elements = null;

  function element(tag, text, className) {
    const value = document.createElement(tag);
    if (className) value.className = className;
    if (text !== undefined) value.textContent = String(text);
    return value;
  }

  function setStatus(text, className = "muted") {
    elements.status.className = className;
    elements.status.textContent = text;
  }

  function responseError(value, response) {
    if (typeof apiError === "function") return apiError(value, response);
    const error = new Error(
      value && value.message || response.statusText || "request failed");
    error.code = value && value.code || "";
    error.status = response.status || 0;
    error.reauthenticate = Boolean(
      response.headers && response.headers.get &&
      /bearer/i.test(response.headers.get("WWW-Authenticate") || ""));
    return error;
  }

  async function requestJSON(path, options = {}) {
    const headers = {
      "accept": "application/json",
      ...(typeof authHeaders === "function" ? authHeaders() : {}),
      ...(options.headers || {}),
    };
    const response = await fetch(path, {...options, headers});
    let value = {};
    try {
      value = await response.json();
    } catch (_) {
      // The normal API uses JSON envelopes; retain the HTTP status if it does not.
    }
    if (!response.ok) throw responseError(value, response);
    return value;
  }

  function canonicalRequestBytes(value) {
    const input = String(value || "").trim();
    if (!input) throw new Error("source and policy wire IDs are required");
    let standard = input;
    if (/^[A-Za-z0-9_-]+$/.test(input)) {
      if (input.length % 4 === 1) throw new Error("wire ID is not valid base64url");
      standard = input.replace(/-/g, "+").replace(/_/g, "/") +
        "=".repeat((4 - input.length % 4) % 4);
    }
    try {
      return btoa(atob(standard));
    } catch (_) {
      throw new Error("wire ID is not valid base64/base64url");
    }
  }

  function boundedInteger(input, minimum, maximum, label) {
    const value = Number(input.value);
    if (!Number.isInteger(value) || value < minimum || value > maximum) {
      throw new Error(`${label} must be between ${minimum} and ${maximum}`);
    }
    return value;
  }

  function selectedWorkspaceID() {
    const settings = window.ShoalWorkspaceSettings;
    return settings && typeof settings.selectedWorkspaceID === "function"
      ? settings.selectedWorkspaceID()
      : "";
  }

  function scopeCandidates(settings) {
    const narrowing = settings && settings.settings || {};
    const sourceIDs = [...(narrowing.permitted_source_ids || [])];
    const policyIDs = [...(narrowing.permitted_policy_ids || [])];
    for (const policy of narrowing.output_policies || []) {
      if (policy.source_id && !sourceIDs.includes(policy.source_id)) {
        sourceIDs.push(policy.source_id);
      }
      if (policy.grant_policy_id &&
          !policyIDs.includes(policy.grant_policy_id)) {
        policyIDs.push(policy.grant_policy_id);
      }
    }
    return {sourceIDs, policyIDs};
  }

  async function fillScopeFromWorkspace() {
    if (elements.source.value.trim() && elements.policy.value.trim()) return;
    const workspaceID = selectedWorkspaceID();
    if (!workspaceID) return;
    const settings = await requestJSON(
      `/api/v1/workspaces/${encodeURIComponent(workspaceID)}/settings`);
    const candidates = scopeCandidates(settings);
    if (!elements.source.value.trim() && candidates.sourceIDs.length === 1) {
      elements.source.value = candidates.sourceIDs[0];
    }
    if (!elements.policy.value.trim() && candidates.policyIDs.length === 1) {
      elements.policy.value = candidates.policyIDs[0];
    }
    if ((!elements.source.value.trim() && candidates.sourceIDs.length > 1) ||
        (!elements.policy.value.trim() && candidates.policyIDs.length > 1)) {
      throw new Error(
        "the selected workspace permits multiple scopes; choose exact source and policy IDs");
    }
  }

  function requestFromControls(cursor = "") {
    const teamID = elements.teamID.value.trim();
    if (!teamID) throw new Error("team ID is required");
    return {
      team_id: teamID,
      source_id: canonicalRequestBytes(elements.source.value),
      policy_id: canonicalRequestBytes(elements.policy.value),
      history_days: boundedInteger(
        elements.historyDays, 1, 31, "history days"),
      limit: boundedInteger(elements.pageSize, 1, 100, "page size"),
      ...(cursor ? {cursor} : {}),
    };
  }

  function duration(seconds) {
    const total = Math.max(0, Number(seconds) || 0);
    if (total < 60) return `${Math.round(total)}s`;
    if (total < 3600) return `${Math.round(total / 60)}m`;
    if (total < 86400) return `${(total / 3600).toFixed(1)}h`;
    return `${(total / 86400).toFixed(1)}d`;
  }

  function timestamp(value) {
    const parsed = new Date(value);
    return Number.isNaN(parsed.getTime()) ? String(value || "unknown") :
      parsed.toLocaleString();
  }

  function percent(value) {
    const number = Number(value);
    return Number.isFinite(number) ? `${(number * 100).toFixed(1)}%` : "n/a";
  }

  function empty(container, noun) {
    container.replaceChildren(element(
      "p",
      `No authorized ${noun} were returned. They may be absent or withheld by policy.`,
      "empty-state",
    ));
  }

  function evidenceKey(value) {
    const normalized = normalizeEvidence(value);
    return `${normalized.kind || "evidence"}\u0000${normalized.id || ""}\u0000` +
      `${normalized.source_id || ""}\u0000${normalized.policy_id || ""}`;
  }

  function normalizeEvidence(value) {
    const team = state.response && state.response.team || {};
    return {
      ...value,
      source_id: value.source_id || team.source_id || "",
      policy_id: value.policy_id || team.policy_id || "",
    };
  }

  function collectEvidence(values) {
    for (const value of values || []) {
      if (!value || !value.id) continue;
      const key = evidenceKey(value);
      const normalized = normalizeEvidence(value);
      if (!state.evidence.has(key)) {
        state.evidence.set(key, {
          value: normalized,
          anchor: `team-evidence-${state.evidence.size + 1}`,
        });
      }
      if (normalized.source_id && !state.sources.has(normalized.source_id)) {
        state.sources.set(
          normalized.source_id, `team-source-${state.sources.size + 1}`);
      }
    }
  }

  function evidenceLinks(values) {
    const list = element("ul", undefined, "team-evidence-links");
    for (const value of values || []) {
      const record = state.evidence.get(evidenceKey(value));
      if (!record) continue;
      const item = element("li");
      const link = element("a", `${value.kind}: ${value.id}`);
      link.href = `#${record.anchor}`;
      item.append(link);
      list.append(item);
    }
    return list;
  }

  function metricCard(label, value, detail = "") {
    const card = element("article", undefined, "team-metric-card");
    card.append(
      element("span", label, "team-metric-label"),
      element("strong", value, "team-metric-value"),
    );
    if (detail) card.append(element("small", detail, "muted"));
    return card;
  }

  function renderSummary(value) {
    const metrics = value.metrics || {};
    const queue = metrics.queue || {};
    const states = metrics.action_states || {};
    elements.summary.replaceChildren(
      metricCard("Active agents", metrics.active_agents || 0),
      metricCard("Queued actions", states.queued || 0,
        `${queue.aged_count || 0} aged ≥ ${duration(queue.aged_after_seconds)}`),
      metricCard("Completed", metrics.completed || 0,
        `${states.succeeded || 0} succeeded actions`),
      metricCard("Failed", metrics.failed || 0,
        `${states.failed || 0} failed actions`),
      metricCard("Median cycle time",
        duration(metrics.median_cycle_time_seconds)),
      metricCard("Median queue age", duration(queue.median_age_seconds),
        `oldest ${duration(queue.oldest_age_seconds)}`),
      metricCard(
        "Action states",
        Number(states.queued || 0) + Number(states.claimed || 0) +
          Number(states.succeeded || 0) + Number(states.failed || 0) +
          Number(states.canceled || 0),
        `queued ${states.queued || 0} · claimed ${states.claimed || 0} · ` +
          `succeeded ${states.succeeded || 0} · failed ${states.failed || 0} · ` +
          `canceled ${states.canceled || 0}`,
      ),
    );
  }

  function identityCard(title, details, nodeID) {
    const card = element("article", undefined, "team-entity-card");
    card.append(element("h3", title));
    for (const detail of details) {
      if (detail) card.append(element("p", detail, "team-meta"));
    }
    if (nodeID) {
      const evidence = {kind: "graph_node", id: nodeID};
      collectEvidence([evidence]);
      card.append(evidenceLinks([evidence]));
    }
    return card;
  }

  function renderRoster(value) {
    elements.people.replaceChildren();
    for (const person of value.people || []) {
      elements.people.append(identityCard(
        person.name || person.id,
        [
          `Person: ${person.id}`,
          person.subject_id ? `Subject: ${person.subject_id}` : "",
        ],
        person.node_id,
      ));
    }
    if (!(value.people || []).length) empty(elements.people, "people");

    elements.agents.replaceChildren();
    for (const agent of value.agents || []) {
      const card = identityCard(
        agent.name || agent.id,
        [
          `Agent: ${agent.id}`,
          agent.active ? "Active" : "Inactive",
          agent.generation ? `Generation: ${agent.generation}` : "",
          agent.lease_expires_at
            ? `Lease expires: ${timestamp(agent.lease_expires_at)}`
            : "",
        ],
        agent.node_id,
      );
      card.classList.add(agent.active ? "active-agent" : "inactive-agent");
      elements.agents.append(card);
    }
    if (!(value.agents || []).length) empty(elements.agents, "agents");

    elements.workItems.replaceChildren();
    for (const item of value.work_items || []) {
      elements.workItems.append(identityCard(
        item.title || item.id,
        [
          `Work item: ${item.id}`,
          `Status: ${item.status || "unavailable"}`,
          `Assigned to: ${(item.assigned_to || []).join(", ") || "unassigned"}`,
          `Blocked by: ${(item.blocked_by || []).join(", ") || "none reported"}`,
        ],
        item.node_id,
      ));
    }
    if (!(value.work_items || []).length) {
      empty(elements.workItems, "work items");
    }
  }

  function renderComparison(metrics) {
    const current = metrics && metrics.seven_day_comparison &&
      metrics.seven_day_comparison.current || {};
    const previous = metrics && metrics.seven_day_comparison &&
      metrics.seven_day_comparison.previous || {};
    const card = element("article", undefined, "team-comparison-card");
    card.append(element("h3", "Current seven days compared with previous seven"));
    const table = element("table", undefined, "team-table");
    const head = element("tr");
    for (const label of ["Metric", "Current", "Previous"]) {
      head.append(element("th", label));
    }
    table.append(head);
    const rows = [
      ["Completed", current.completed || 0, previous.completed || 0],
      ["Failed", current.failed || 0, previous.failed || 0],
      ["Failure rate", percent(current.failure_rate), percent(previous.failure_rate)],
      ["Median cycle time", duration(current.median_cycle_time_seconds),
        duration(previous.median_cycle_time_seconds)],
    ];
    for (const values of rows) {
      const row = element("tr");
      for (const value of values) row.append(element("td", value));
      table.append(row);
    }
    card.append(table);
    elements.comparison.replaceChildren(card);
  }

  function renderTrends(metrics) {
    elements.trends.replaceChildren();
    const daily = metrics && metrics.daily || [];
    if (!daily.length) {
      empty(elements.trends, "daily activity buckets");
      return;
    }
    const maximum = Math.max(1, ...daily.map((day) =>
      Number(day.activities || 0)));
    for (const day of daily) {
      const row = element("div", undefined, "team-trend-row");
      const label = element("time", day.date || "unknown", "team-trend-date");
      label.dateTime = day.date || "";
      const track = element("div", undefined, "team-trend-track");
      const bar = element("span", undefined, "team-trend-bar");
      bar.setAttribute(
        "style",
        `width:${Math.round(100 * Number(day.activities || 0) / maximum)}%`);
      track.append(bar);
      row.append(
        label,
        track,
        element(
          "span",
          `${day.activities || 0} activity · ${day.completed || 0} completed · ` +
          `${day.failed || 0} failed · ${day.manual || 0} manual`,
          "team-trend-counts",
        ),
      );
      elements.trends.append(row);
    }
  }

  function renderActors(value) {
    elements.actors.replaceChildren();
    const names = new Map();
    for (const person of value.people || []) {
      names.set(person.id, person.name || person.id);
    }
    for (const agent of value.agents || []) {
      names.set(agent.id, agent.name || agent.id);
    }
    const actors = value.metrics && value.metrics.activity_by_actor || [];
    for (const actor of actors) {
      elements.actors.append(metricCard(
        names.get(actor.actor_id) || actor.actor_id || "Actor unavailable",
        actor.count || 0,
        actor.actor_id ? `Actor ID: ${actor.actor_id}` : "",
      ));
    }
    if (!actors.length) empty(elements.actors, "actor activity totals");
  }

  function insightTitle(value) {
    return String(value.label || value.kind || "signal")
      .replace(/_/g, " ")
      .replace(/\b\w/g, (letter) => letter.toUpperCase());
  }

  function renderInsightGroup(container, values, noun) {
    container.replaceChildren();
    for (const insight of values) {
      collectEvidence(insight.evidence);
      const card = element(
        "article", undefined,
        `team-insight team-severity-${insight.severity || "notice"}`);
      card.append(
        element("h3", insightTitle(insight)),
        element(
          "p",
          insight.heuristic
            ? `Heuristic signal · ${insight.description || ""}`
            : `Reported signal · ${insight.description || ""}`,
        ),
        evidenceLinks(insight.evidence),
      );
      container.append(card);
    }
    if (!values.length) empty(container, noun);
  }

  function renderInsights(value) {
    const insights = value.insights || [];
    renderInsightGroup(
      elements.bottlenecks,
      insights.filter((item) => item.kind === "bottleneck"),
      "bottleneck signals",
    );
    renderInsightGroup(
      elements.improvements,
      insights.filter((item) => item.kind === "improvement"),
      "improvement signals",
    );
    renderInsightGroup(
      elements.automation,
      insights.filter((item) => item.kind === "automation_candidate"),
      "automation candidates",
    );
  }

  function renderActivities() {
    elements.activity.replaceChildren();
    if (!state.activities.length) {
      empty(elements.activity, "activity");
      return;
    }
    for (const activity of state.activities) {
      collectEvidence(activity.evidence);
      const card = element("article", undefined, "team-activity-card");
      const heading = activity.operation || activity.kind || "Activity";
      card.append(
        element("h3", heading),
        element("time", timestamp(activity.recorded_at), "team-meta"),
        element(
          "p",
          [
            activity.actor_id ? `Actor ${activity.actor_id}` : "Actor unavailable",
            activity.state ? `state ${activity.state}` : "",
            activity.manual ? "manual" : "",
            activity.queue_age_seconds
              ? `queue age ${duration(activity.queue_age_seconds)}`
              : "",
          ].filter(Boolean).join(" · "),
          "team-meta",
        ),
        evidenceLinks(activity.evidence),
      );
      elements.activity.append(card);
    }
  }

  function renderEvidenceIndex() {
    elements.evidence.replaceChildren();
    if (!state.evidence.size) {
      empty(elements.evidence, "evidence references");
      return;
    }
    const evidenceHeading = element("h3", "Evidence identifiers");
    elements.evidence.append(evidenceHeading);
    for (const {value, anchor} of state.evidence.values()) {
      const card = element("article", undefined, "team-evidence-card");
      card.id = anchor;
      card.append(
        element("strong", value.kind || "evidence"),
        element("code", value.id),
      );
      if (value.source_id) {
        const link = element("a", `Source: ${value.source_id}`);
        link.href = `#${state.sources.get(value.source_id)}`;
        card.append(link);
      } else {
        card.append(element("span", "Source identifier unavailable", "muted"));
      }
      if (value.policy_id) {
        card.append(element("code", `Policy: ${value.policy_id}`));
      } else {
        card.append(element("span", "Policy identifier unavailable", "muted"));
      }
      elements.evidence.append(card);
    }
    if (state.sources.size) {
      elements.evidence.append(element("h3", "Source identifiers"));
      for (const [sourceID, anchor] of state.sources) {
        const card = element("article", undefined, "team-source-card");
        card.id = anchor;
        card.append(
          element("strong", "Authorized source"),
          element("code", sourceID),
        );
        elements.evidence.append(card);
      }
    }
  }

  function renderBounds(value) {
    const bounds = value.bounds || {};
    const truncated = [];
    for (const [field, label] of [
      ["graph_truncated", "graph"],
      ["agents_truncated", "agents"],
      ["actions_truncated", "actions"],
      ["interactions_truncated", "interactions"],
    ]) {
      if (bounds[field]) truncated.push(label);
    }
    const prefix = bounds.complete
      ? "Complete within configured server bounds."
      : `Bounded partial response${truncated.length
        ? `; truncated: ${truncated.join(", ")}`
        : "; completeness was not asserted"}.`;
    elements.bounds.textContent =
      `${prefix} Window ${timestamp(bounds.since)} to ${timestamp(bounds.as_of)}.`;
    elements.bounds.className = bounds.complete ? "muted" : "team-withheld";
    elements.printWindow.textContent =
      `Authorized window: ${timestamp(bounds.since)} to ${timestamp(bounds.as_of)}. ` +
      (bounds.complete ? "Complete within bounds." : "Bounded partial response.");
  }

  function render(value) {
    state.evidence.clear();
    state.sources.clear();
    renderSummary(value);
    renderRoster(value);
    renderComparison(value.metrics || {});
    renderTrends(value.metrics || {});
    renderActors(value);
    renderInsights(value);
    renderActivities();
    renderEvidenceIndex();
    renderBounds(value);
    elements.dashboard.hidden = false;
    elements.refresh.disabled = false;
    elements.exportButton.disabled = false;
    elements.more.hidden = !state.nextCursor;
    elements.printTitle.textContent =
      `${value.team && (value.team.name || value.team.id) || "Team"} overview`;
  }

  function mergeActivities(values) {
    const seen = new Set(state.activities.map((item) =>
      `${item.kind || ""}\u0000${item.id || ""}`));
    for (const value of values || []) {
      const key = `${value.kind || ""}\u0000${value.id || ""}`;
      if (!seen.has(key)) {
        seen.add(key);
        state.activities.push(value);
      }
    }
    state.activities.sort((left, right) =>
      String(right.recorded_at || "").localeCompare(
        String(left.recorded_at || "")) ||
      String(left.kind || "").localeCompare(String(right.kind || "")) ||
      String(left.id || "").localeCompare(String(right.id || "")));
  }

  function failureText(error) {
    if (typeof needsReauthentication === "function" &&
        needsReauthentication(error)) {
      if (typeof handleReauthentication === "function") {
        handleReauthentication();
      }
      return {
        text: typeof reauthenticationText === "function"
          ? reauthenticationText()
          : "Sign in again to continue.",
        className: "reauth",
      };
    }
    if ((typeof isDenied === "function" && isDenied(error)) ||
        error.code === "unauthorized") {
      return {
        text: "Access denied. The server did not authorize this team overview.",
        className: "denied",
      };
    }
    if (error.status === 404 || error.code === "not_found") {
      return {
        text: "Team data is unavailable or withheld. The server intentionally does not distinguish those cases.",
        className: "team-withheld",
      };
    }
    return {
      text: `Unable to load team overview: ${error.message || String(error)}`,
      className: "error",
    };
  }

  async function load(reset = true) {
    if (state.loading) return;
    state.loading = true;
    const generation = ++state.generation;
    elements.load.disabled = true;
    elements.refresh.disabled = true;
    elements.more.disabled = true;
    setStatus(reset ? "Loading authorized team overview…" :
      "Loading more authorized activity…");
    try {
      if (reset) await fillScopeFromWorkspace();
      const request = reset
        ? requestFromControls()
        : {...state.request, cursor: state.nextCursor};
      const value = await requestJSON(overviewPath, {
        method: "POST",
        headers: {"content-type": "application/json"},
        body: JSON.stringify(request),
      });
      if (generation !== state.generation) return;
      if (reset) {
        state.activities = [];
        state.request = {...request, cursor: undefined};
      }
      mergeActivities(value.activities);
      state.response = value;
      state.nextCursor = value.next_cursor || "";
      render(value);
      setStatus(
        `Loaded ${state.activities.length} authorized activity record(s) for ` +
        `${value.team && (value.team.name || value.team.id) || request.team_id}.`);
    } catch (error) {
      if (generation !== state.generation) return;
      if (reset) resetRenderedData();
      const failure = failureText(error);
      setStatus(failure.text, failure.className);
    } finally {
      if (generation === state.generation) {
        state.loading = false;
        elements.load.disabled = false;
        elements.refresh.disabled = !state.response;
        elements.more.disabled = false;
      }
    }
  }

  function exportJSON() {
    if (!state.response) return;
    const payload = {
      ...state.response,
      activities: state.activities,
      next_cursor: state.nextCursor || undefined,
    };
    const blob = new Blob(
      [JSON.stringify(payload, null, 2) + "\n"],
      {type: "application/json"},
    );
    const href = URL.createObjectURL(blob);
    const link = element("a");
    link.href = href;
    link.download = `${String(payload.team && payload.team.id || "team")
      .replace(/[^A-Za-z0-9._-]+/g, "-")}-overview.json`;
    document.body.append(link);
    link.click();
    link.remove();
    URL.revokeObjectURL(href);
  }

  function resetRenderedData() {
    state.response = null;
    state.activities = [];
    state.nextCursor = "";
    state.request = null;
    state.evidence.clear();
    state.sources.clear();
    if (!elements) return;
    elements.dashboard.hidden = true;
    elements.refresh.disabled = true;
    elements.exportButton.disabled = true;
    elements.more.hidden = true;
    elements.bounds.textContent = "";
  }

  function reset() {
    state.generation++;
    state.loading = false;
    resetRenderedData();
    if (elements) {
      elements.source.value = "";
      elements.policy.value = "";
      setStatus("Enter an authorized team and exact source/policy scope.");
    }
  }

  function initialize() {
    elements = {
      form: document.getElementById("team-form"),
      teamID: document.getElementById("team-id"),
      source: document.getElementById("team-source-id"),
      policy: document.getElementById("team-policy-id"),
      historyDays: document.getElementById("team-history-days"),
      pageSize: document.getElementById("team-page-size"),
      load: document.getElementById("team-load"),
      refresh: document.getElementById("team-refresh"),
      exportButton: document.getElementById("team-export"),
      printButton: document.getElementById("team-print"),
      status: document.getElementById("team-status"),
      bounds: document.getElementById("team-bounds"),
      dashboard: document.getElementById("team-dashboard"),
      summary: document.getElementById("team-summary"),
      people: document.getElementById("team-people"),
      agents: document.getElementById("team-agents"),
      workItems: document.getElementById("team-work-items"),
      comparison: document.getElementById("team-comparison"),
      trends: document.getElementById("team-trends"),
      actors: document.getElementById("team-actors"),
      bottlenecks: document.getElementById("team-bottlenecks"),
      improvements: document.getElementById("team-improvements"),
      automation: document.getElementById("team-automation"),
      activity: document.getElementById("team-activity"),
      more: document.getElementById("team-more"),
      evidence: document.getElementById("team-evidence"),
      printTitle: document.getElementById("team-print-title"),
      printWindow: document.getElementById("team-print-window"),
    };
    if (Object.values(elements).some((value) => !value)) return;
    elements.form.addEventListener("submit", async (event) => {
      event.preventDefault();
      await load(true);
    });
    elements.refresh.addEventListener("click", async () => load(true));
    elements.more.addEventListener("click", async () => load(false));
    elements.exportButton.addEventListener("click", exportJSON);
    elements.printButton.addEventListener("click", () => window.print());
  }

  window.ShoalTeamOverview = Object.freeze({
    reset,
    load: () => load(true),
    state: () => ({
      loaded: Boolean(state.response),
      activityCount: state.activities.length,
      nextCursor: state.nextCursor,
    }),
  });
  window.addEventListener("DOMContentLoaded", initialize, {once: true});
})();
