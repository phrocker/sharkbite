(() => {
  "use strict";

  const $ = (id) => document.getElementById(id);
  if (!$("inbox")) return;

  function setStatus(text, error = false) {
    const status = $("inbox-status");
    if (!status) return;
    status.textContent = text;
    status.className = error ? "decision-status-error" : "muted";
  }

  function show(value) {
    const output = $("inbox-output");
    if (!output) return;
    output.textContent = value === "" ? "" : (typeof value === "string" ? value : JSON.stringify(value, null, 2));
  }

  function responseError(value, response) {
    if (typeof apiError === "function") return apiError(value, response);
    const error = new Error((value && value.message) || (response && response.statusText) || "request failed");
    error.status = response && response.status || 0;
    return error;
  }

  async function requestJSON(path, options = {}) {
    const headers = {
      accept: "application/json",
      ...(typeof authHeaders === "function" ? authHeaders() : {}),
      ...(options.headers || {}),
    };
    const response = await fetch(path, {...options, headers});
    let value = {};
    try {
      value = await response.json();
    } catch (_) {
      // Keep the HTTP status in the error when a deployment returns no JSON.
    }
    if (!response.ok) throw responseError(value, response);
    return value;
  }

  function post(path, body) {
    return requestJSON(path, {method: "POST", headers: {"content-type": "application/json"}, body: JSON.stringify(body)});
  }

  function newKey() {
    const bytes = new Uint8Array(18);
    if (!window.crypto || !window.crypto.getRandomValues) {
      throw new Error("this browser cannot create a request id safely");
    }
    window.crypto.getRandomValues(bytes);
    let value = "";
    for (const byte of bytes) value += String.fromCharCode(byte);
    return btoa(value).replace(/=/g, "").replace(/\+/g, "-").replace(/\//g, "_");
  }

  // Every fleet route records the context it was called under; the inbox
  // names itself and gives each read a short deadline.
  function context(reason) {
    return {request_id: newKey(), reason_code: reason, deadline: new Date(Date.now() + 5 * 60 * 1000).toISOString()};
  }

  function el(tag, className, text) {
    const node = document.createElement(tag);
    if (className) node.className = className;
    if (text !== undefined) node.textContent = String(text);
    return node;
  }

  function kv(pairs) {
    const grid = el("div", "kv");
    for (const [key, value] of pairs) {
      grid.appendChild(el("span", "muted", key));
      grid.appendChild(el("span", "", value === undefined || value === null ? "" : value));
    }
    return grid;
  }

  function table(headers, rows) {
    const node = el("table");
    const head = el("tr");
    for (const h of headers) head.appendChild(el("th", "", h));
    node.appendChild(head);
    for (const row of rows) {
      const tr = el("tr");
      for (const cell of row) tr.appendChild(el("td", "", cell));
      node.appendChild(tr);
    }
    return node;
  }

  function stamp(value) {
    return value ? String(value).replace("T", " ").replace(/\.\d+Z$/, "Z") : "";
  }

  // A refused action is shown as the route said it; a bearer challenge goes
  // through the same sign-in-again path as the sibling consoles.
  function failed(label, error) {
    show({error: error.message || String(error), code: error.code || "", status: error.status || 0});
    if (typeof needsReauthentication === "function" && needsReauthentication(error)) {
      if (typeof handleReauthentication === "function") handleReauthentication();
      setStatus(`${label} requires sign-in again.`, true);
    } else {
      setStatus(`${label} refused: ${error.message || error}`, true);
    }
  }

  // Selecting an item selects it everywhere: the inspectors read this.
  const selection = {kind: "", id: "", object: ""};
  function select(kind, id, object) {
    selection.kind = kind;
    selection.id = id;
    selection.object = object || "";
    document.querySelectorAll(".inbox-item").forEach((node) => {
      node.classList.toggle("selected", node.dataset.itemId === id);
    });
    window.explorerSelection = {...selection};
  }

  function openInEvidence(object) {
    const query = $("query");
    if (query && object) query.value = object;
    if (typeof activate === "function") activate("evidence");
  }

  function openInLearning(candidateID) {
    const input = $("learning-evaluations-candidate");
    if (input) input.value = candidateID;
    if (typeof activate === "function") activate("learning");
  }

  // The chain behind a held request: its current status (which carries the
  // input and the effective state beside the stored one) and the declared
  // effects of the action from the agent's current descriptor.
  async function expandApproval(item, body, controls, isCurrent) {
    body.textContent = "";
    body.appendChild(el("p", "muted", "Reading the request's status and the agent's descriptor…"));
    const row = item.approval;
    let status;
    try {
      status = await post(`/api/v1/fleet/approvals/${encodeURIComponent(item.id)}/status`, context("inbox-inspect"));
    } catch (error) {
      if (!isCurrent()) return;
      body.textContent = "";
      body.appendChild(el("p", "error", `Status unavailable: ${error.message || error}`));
      return;
    }
    if (!isCurrent()) return;
    let effects = "(descriptor unavailable)";
    let requiresApproval = "";
    try {
      const descriptor = await post(`/api/v1/fleet/agents/${encodeURIComponent(row.agent_id)}/resolve`, context("inbox-inspect"));
      for (const capability of descriptor.capabilities || []) {
        if (capability.name !== row.capability) continue;
        for (const action of capability.actions || []) {
          if (action.name !== row.action) continue;
          effects = (action.effects || []).length ? (action.effects || []).join(", ") : "none declared";
          requiresApproval = action.requires_approval ? "yes" : "no";
        }
      }
      if (descriptor.generation !== row.agent_generation) {
        effects += ` (descriptor generation ${descriptor.generation}; the request was made under ${row.agent_generation})`;
      }
    } catch (_) {
      // The chain still shows without the descriptor; the row says so.
    }
    if (!isCurrent()) return;
    body.textContent = "";
    const stale = status.state !== "pending" || status.policy_generation !== row.policy_generation || status.request_digest !== row.request_digest;
    body.appendChild(kv([
      ["state", status.condition ? `${status.state} (${status.condition})` : status.state],
      ["requester", `${row.requester} via ${row.requester_actor}`],
      ["agent", `${row.agent_id} generation ${row.agent_generation}`],
      ["action", `${row.capability} / ${row.action}`],
      ["declared effects", effects],
      ["requires approval", requiresApproval],
      ["object", row.object_id],
      ["requested", stamp(row.requested_at)],
      ["deadline", stamp(row.deadline)],
      ["expires", stamp(row.expires_at)],
      ["policy generation", row.policy_generation],
    ]));
    const input = el("pre", "decision-output");
    input.textContent = status.input === undefined ? "(no input served)" : JSON.stringify(status.input, null, 2);
    body.appendChild(el("h3", "", "Input"));
    body.appendChild(input);
    controls.textContent = "";
    if (stale) {
      controls.appendChild(el("p", "error", "This request no longer matches the listing; reload the inbox before deciding."));
      return;
    }
    let deciding = false;
    for (const verdict of ["approve", "refuse"]) {
      const button = el("button", "", verdict === "approve" ? "Approve" : "Refuse");
      button.type = "button";
      button.onclick = async () => {
        if (deciding || !isCurrent()) return;
        deciding = true;
        controls.querySelectorAll("button").forEach((control) => { control.disabled = true; });
        setStatus(`${verdict === "approve" ? "Approving" : "Refusing"} ${item.id}…`);
        try {
          const record = await post(`/api/v1/fleet/approvals/${encodeURIComponent(item.id)}/decide`, {
            context: context("inbox-decide"), request_digest: row.request_digest, policy_generation: row.policy_generation, verdict,
          });
          show(record);
          setStatus(`Request ${item.id} is now ${record.state}.`);
          await load();
        } catch (error) {
          // A conflict can mean the policy or approval moved. A transport
          // failure can mean the transition committed before the response
          // failed. In both cases a fresh list is required before another
          // human decision; never re-enable the old controls.
          if (isCurrent()) {
            controls.textContent = "";
            controls.appendChild(el("p", "error", "Decision result needs a fresh inbox read before another action."));
          }
          await load();
          failed("Decision", error);
        }
      };
      controls.appendChild(button);
    }
    const inspect = el("button", "", "Open object in Evidence");
    inspect.type = "button";
    inspect.onclick = () => openInEvidence(row.object_id);
    controls.appendChild(inspect);
  }

  function renderClassRecords(container, classes, classesError) {
    if (classesError) {
      container.appendChild(el("p", "error", `No measured record: ${classesError}.`));
      return;
    }
    if (!classes || !classes.length) {
      container.appendChild(el("p", "muted", "No measured record is served beside this candidate."));
      return;
    }
    for (const entry of classes) {
      container.appendChild(el("h3", "", `Cohort ${entry.cohort_id}`));
      if (entry.error) {
        container.appendChild(el("p", "muted", `${entry.error} (window from ${stamp(entry.from)})`));
        continue;
      }
      const records = entry.records;
      container.appendChild(el("p", "muted", `Cutoff ${stamp(records.cutoff)}; window ${stamp(records.from)} to ${stamp(records.to)}; ${records.in_window} of ${records.members} members in the window.`));
      container.appendChild(table(["label", "targets", "agree", "disagree", "disputed", "unresolved", "none", "precision"], (records.classes || []).map((c) => {
        const a = c.adjudication || {};
        return [c.class.abstained ? "(abstained)" : c.class.label, c.targets, a.verified_agree, a.verified_disagree, a.disputed, a.unresolved, a.none, c.precision === null || c.precision === undefined ? "n/a" : c.precision.toFixed(3)];
      })));
    }
  }

  function expandCandidate(item, body, controls) {
    body.textContent = "";
    const cand = item.candidate;
    body.appendChild(kv([
      ["disposition", cand.disposition],
      ["tasks", JSON.stringify(cand.task_ids || [])],
      ["dataset", cand.dataset_id || ""],
      ["training manifest", cand.training_manifest_sha256 || ""],
      ["evaluation cited by promotion", cand.promotable_evaluation_id],
    ]));
    body.appendChild(el("h3", "", "Evaluations"));
    body.appendChild(table(["id", "disposition", "permitted action", "reasons"], (cand.evaluations || []).map((e) => [e.id, e.disposition, e.permitted_action || "", JSON.stringify(e.reasons || [])])));
    body.appendChild(el("h3", "", "Measured record of the cohorts it was trained on"));
    renderClassRecords(body, item.classes, item.classes_error);
    controls.textContent = "";
    const promote = el("button", "", "Promote to shadow release");
    promote.type = "button";
    promote.onclick = async () => {
      promote.disabled = true;
      setStatus(`Promoting ${item.id}…`);
      try {
        const record = await post("/api/v1/learning/releases/promote", {candidate_id: item.id, evaluation_id: cand.promotable_evaluation_id});
        show(record);
        setStatus(`Candidate ${item.id} promoted.`);
        await load();
      } catch (error) {
        // A failed response does not prove promotion was not committed.
        // Reload before offering another action on this candidate.
        await load();
        failed("Promotion", error);
      }
    };
    controls.appendChild(promote);
    const inspect = el("button", "", "Open in Learning");
    inspect.type = "button";
    inspect.onclick = () => openInLearning(item.id);
    controls.appendChild(inspect);
  }

  function renderItem(item, summaryText, expand) {
    const details = el("details", "inbox-item");
    details.dataset.itemId = item.id;
    const summary = el("summary", "", summaryText);
    details.appendChild(summary);
    const body = el("div", "inbox-item-body");
    const controls = el("div", "button-row inbox-controls");
    details.appendChild(body);
    details.appendChild(controls);
    let inspection = 0;
    details.addEventListener("toggle", () => {
      const current = ++inspection;
      if (!details.open) return;
      select(item.kind, item.id, item.approval ? item.approval.object_id : "");
      expand(item, body, controls, () => details.open && inspection === current);
    });
    return details;
  }

  function render(inbox) {
    const approvals = $("inbox-approvals-list");
    const candidates = $("inbox-candidates-list");
    approvals.textContent = "";
    candidates.textContent = "";
    const a = inbox.approvals || {};
    $("inbox-approvals-note").textContent = a.note || (a.truncated ? "More held requests exist than are listed; decide these first." : "");
    let held = 0;
    for (const group of a.groups || []) {
      const section = el("div", "inbox-group");
      section.appendChild(el("h3", "", `${group.capability} / ${group.action} (${group.items.length})`));
      for (const item of group.items) {
        held++;
        const row = item.approval;
        section.appendChild(renderItem(item, `${row.requester} asks to ${group.action} ${row.object_id}, requested ${stamp(row.requested_at)}, deadline ${stamp(row.deadline)}`, expandApproval));
      }
      approvals.appendChild(section);
    }
    if (a.available && held === 0) approvals.appendChild(el("p", "empty-state", "No held requests await your decision."));
    const c = inbox.candidates || {};
    $("inbox-candidates-note").textContent = c.note || (c.truncated ? "More candidates exist than are listed." : "");
    for (const item of c.items || []) {
      candidates.appendChild(renderItem(item, `Candidate ${item.id}, sealed as ${item.candidate.disposition}, tasks ${JSON.stringify(item.candidate.task_ids || [])}`, expandCandidate));
    }
    if (c.available && !(c.items || []).length) candidates.appendChild(el("p", "empty-state", "No candidate awaits promotion."));
    setStatus(`Inbox read at ${stamp(inbox.generated_at)}: ${held} held request${held === 1 ? "" : "s"}, ${(c.items || []).length} candidate${(c.items || []).length === 1 ? "" : "s"}.`);
  }

  let loading = null;
  async function load() {
    if (loading) return loading;
    setStatus("Reading what awaits you…");
    show("");
    loading = (async () => {
      try {
        render(await post("/api/v1/inbox", {context: context("inbox-read")}));
      } catch (error) {
        show({error: error.message || String(error), code: error.code || "", status: error.status || 0});
        if (typeof needsReauthentication === "function" && needsReauthentication(error)) {
          if (typeof handleReauthentication === "function") handleReauthentication();
          setStatus("The inbox requires sign-in again.", true);
        } else {
          setStatus(`Inbox unavailable: ${error.message || error}`, true);
        }
      } finally {
        loading = null;
      }
    })();
    return loading;
  }

  $("inbox-refresh").onclick = () => load();
  document.addEventListener("explorer:view", (event) => {
    if (event.detail && event.detail.id === "inbox") load();
  });
})();
