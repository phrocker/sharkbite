// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
// The inbox renders only approvals returned by the authenticated fleet API.
const inbox = (() => {
  "use strict";
  const $ = (id) => document.getElementById(id);
  const pageSize = 25;
  let busy = false;
  let generation = 0;
  let next = "";
  let selected = null;
  let rows = [];

  function status(message, error = false) {
    const element = $("inbox-status");
    element.textContent = message;
    element.className = error ? "error" : "muted";
  }

  function wireID(value) {
    const bytes = new TextEncoder().encode(value);
    let binary = "";
    for (const byte of bytes) binary += String.fromCharCode(byte);
    return btoa(binary).replace(/=/g, "").replace(/\+/g, "-").replace(/\//g, "_");
  }

  function requestContext() {
    if (!window.crypto || !window.crypto.getRandomValues) {
      throw new Error("this browser cannot create a request ID safely");
    }
    const bytes = new Uint8Array(16);
    window.crypto.getRandomValues(bytes);
    const id = [...bytes].map((byte) => byte.toString(16).padStart(2, "0")).join("");
    return {
      request_id: wireID(`inbox:${id}`),
      reason_code: "operator_inbox",
      deadline: new Date(Date.now() + 120000).toISOString(),
    };
  }

  async function post(path, body) {
    const response = await fetch(path, {
      method: "POST",
      headers: {accept: "application/json", "content-type": "application/json", ...authHeaders()},
      body: JSON.stringify(body),
    });
    let value = {};
    try { value = await response.json(); } catch (_) { /* Preserve HTTP status. */ }
    if (!response.ok) throw apiError(value, response);
    return value;
  }

  function failure(action, error) {
    if (needsReauthentication(error)) {
      handleReauthentication();
      status(reauthenticationText(), true);
    } else if (isDenied(error)) {
      status(deniedText(action, state.identity), true);
    } else {
      status(`${action} failed: ${error.message || String(error)}`, true);
    }
  }

  function fact(parent, label, value) {
    const dt = document.createElement("dt");
    dt.textContent = label;
    const dd = document.createElement("dd");
    dd.textContent = value === null || value === undefined || value === "" ? "—" : String(value);
    parent.append(dt, dd);
  }

  function renderRows() {
    const list = $("inbox-list");
    list.replaceChildren();
    if (!rows.length) {
      const empty = document.createElement("p");
      empty.className = "empty-state";
      empty.textContent = "No pending fleet approvals are visible in this workspace.";
      list.append(empty);
    }
    for (const row of rows) {
      const button = document.createElement("button");
      button.type = "button";
      button.className = "inbox-item";
      button.disabled = busy;
      button.setAttribute("aria-pressed", String(selected && selected.id === row.id));
      const title = document.createElement("strong");
      title.textContent = `${row.capability || "Fleet action"} · ${row.action || "approval"}`;
      const detail = document.createElement("span");
      detail.textContent = `${row.state} · requested ${row.requested_at || "unknown time"}`;
      button.append(title, detail);
      button.onclick = () => inspect(row);
      list.append(button);
    }
    $("inbox-more").hidden = !next;
  }

  function renderDetail(record, stale = false) {
    const detail = $("inbox-detail");
    detail.replaceChildren();
    const heading = document.createElement("h2");
    heading.id = "inbox-detail-heading";
    heading.textContent = "Review approval";
    const stateText = document.createElement("p");
    stateText.className = record.state === "pending" && !stale && !record.condition ? "inbox-state" : "error";
    stateText.textContent = stale
      ? "This approval changed since the list loaded. Refresh before deciding."
      : `${record.state}${record.condition ? ` · ${record.condition}` : ""}`;
    const facts = document.createElement("dl");
    facts.className = "inbox-facts";
    fact(facts, "Capability", record.capability);
    fact(facts, "Action", record.action);
    fact(facts, "Agent", record.agent_id);
    fact(facts, "Object", record.object_id);
    fact(facts, "Requester", record.requester);
    fact(facts, "Requested", record.requested_at);
    fact(facts, "Expires", record.expires_at);
    fact(facts, "Request digest", record.request_digest);
    fact(facts, "Policy generation", record.policy_generation);
    detail.append(heading, stateText, facts);
    if (record.input !== undefined) {
      const input = document.createElement("details");
      const summary = document.createElement("summary");
      summary.textContent = "Request input";
      const pre = document.createElement("pre");
      pre.className = "decision-output";
      pre.textContent = JSON.stringify(record.input, null, 2);
      input.append(summary, pre);
      detail.append(input);
    }
    if (record.state !== "pending" || stale || record.condition) return;
    const actions = document.createElement("div");
    actions.className = "button-row inbox-actions";
    for (const [verdict, label] of [["approve", "Approve"], ["refuse", "Refuse"]]) {
      const button = document.createElement("button");
      button.type = "button";
      button.textContent = label;
      button.onclick = () => decide(record, verdict);
      actions.append(button);
    }
    detail.append(actions);
  }

  async function inspect(row) {
    if (busy) return;
    const ticket = ++generation;
    selected = row;
    renderRows();
    status("Checking current approval status…");
    try {
      const record = await post(`/api/v1/fleet/approvals/${encodeURIComponent(row.id)}/status`,
        {context: requestContext()});
      if (ticket !== generation) return;
      const stale = record.version !== row.version ||
        record.request_digest !== row.request_digest ||
        record.policy_generation !== row.policy_generation;
      selected = record;
      renderRows();
      renderDetail(record, stale);
      status(stale ? "Approval changed. Refresh the inbox before deciding." : "Approval inspected.", stale);
    } catch (error) {
      if (ticket !== generation) return;
      selected = null;
      renderRows();
      $("inbox-detail").replaceChildren();
      failure("Inspect approval", error);
    }
  }

  async function decide(record, verdict) {
    if (busy || !selected || selected.id !== record.id || selected.version !== record.version ||
        selected.request_digest !== record.request_digest ||
        selected.policy_generation !== record.policy_generation) return;
    busy = true;
    renderRows();
    $("inbox-refresh").disabled = true;
    $("inbox-more").disabled = true;
    $("inbox-detail").querySelectorAll(".inbox-actions button").forEach((button) => { button.disabled = true; });
    status(`${verdict === "approve" ? "Approving" : "Refusing"} approval…`);
    try {
      // The service compares digest and policy generation in the same
      // transition. A conflicting or indeterminate response is never retried.
      await post(`/api/v1/fleet/approvals/${encodeURIComponent(record.id)}/decide`, {
        context: requestContext(), request_digest: record.request_digest,
        policy_generation: record.policy_generation, verdict,
      });
      status("Decision recorded. Refreshing pending approvals…");
      await refresh();
    } catch (error) {
      try { await refresh(); } catch (_) { /* Keep the original error visible. */ }
      failure("Decide approval", error);
    } finally {
      busy = false;
      renderRows();
      $("inbox-refresh").disabled = false;
      $("inbox-more").disabled = false;
    }
  }

  async function loadPage(reset) {
    if (!capability("fleet_approvals")) return;
    const ticket = ++generation;
    if (reset) {
      rows = [];
      next = "";
      selected = null;
      renderRows();
      $("inbox-detail").replaceChildren();
      status("Loading pending approvals…");
    }
    $("inbox-refresh").disabled = true;
    $("inbox-more").disabled = true;
    try {
      const page = await post("/api/v1/fleet/approvals/pending", {
        context: requestContext(), after: reset ? "" : next, limit: pageSize,
      });
      if (ticket !== generation) return;
      rows = rows.concat(page.approvals || []).sort((a, b) =>
        String(b.requested_at || "").localeCompare(String(a.requested_at || "")) ||
        String(a.id || "").localeCompare(String(b.id || "")));
      next = page.next || "";
      renderRows();
      status(`${rows.length} pending approval${rows.length === 1 ? "" : "s"} loaded.`);
    } catch (error) {
      if (ticket === generation) failure("Load pending approvals", error);
    } finally {
      if (ticket === generation) {
        $("inbox-refresh").disabled = busy;
        $("inbox-more").disabled = busy;
      }
    }
  }

  function refresh() { return loadPage(true); }

  function applyCapability() {
    const available = capability("fleet_approvals");
    const tab = $("tab-inbox");
    tab.disabled = !available;
    tab.setAttribute("aria-disabled", String(!available));
    tab.classList.toggle("unavailable", !available);
    tab.title = available ? "Review pending fleet approvals" : "Fleet approvals are not mounted on this host";
    if (!available) status("Fleet approvals are unavailable on this Explorer host.");
  }

  function open() { return refresh(); }
  $("inbox-refresh").onclick = refresh;
  $("inbox-more").onclick = () => { if (next) loadPage(false); };
  return {applyCapability, open};
})();
