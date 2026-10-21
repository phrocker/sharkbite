// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
//
// The learning console is a thin client over the authenticated learning
// routes under /api/v1/learning. Every action here has an equivalent route
// and produces the same record. It never treats a response as a promotion,
// a verified label, or permission to omit review: shadow evidence cannot
// promote a model or exclude review, and this console creates no authority.
(() => {
  "use strict";

  const $ = (id) => document.getElementById(id);
  const ROOT = "/api/v1/learning";

  function setStatus(text, error = false) {
    const status = $("learning-console-status");
    if (!status) return;
    status.textContent = text;
    status.className = error ? "decision-status-error" : "muted";
  }

  function show(value) {
    const output = $("learning-console-output");
    if (!output) return;
    output.textContent = typeof value === "string" ? value : JSON.stringify(value, null, 2);
  }

  function cell(text) {
    const td = document.createElement("td");
    td.textContent = text === undefined || text === null ? "" : String(text);
    return td;
  }

  function table(headers, rows) {
    const el = document.createElement("table");
    const head = document.createElement("tr");
    for (const h of headers) {
      const th = document.createElement("th");
      th.textContent = h;
      head.appendChild(th);
    }
    el.appendChild(head);
    for (const row of rows) {
      const tr = document.createElement("tr");
      for (const value of row) {
        if (value instanceof HTMLElement) {
          const td = document.createElement("td");
          td.appendChild(value);
          tr.appendChild(td);
        } else {
          tr.appendChild(cell(value));
        }
      }
      el.appendChild(tr);
    }
    return el;
  }

  function mark(ok, yes, no) {
    const span = document.createElement("span");
    span.className = ok ? "learning-verified" : "learning-refused";
    span.textContent = ok ? yes : no;
    return span;
  }

  function summarize(kind, value) {
    const summary = $("learning-console-summary");
    if (!summary) return;
    summary.replaceChildren();
    const note = (text) => {
      const p = document.createElement("p");
      p.className = "learning-note";
      p.textContent = text;
      summary.appendChild(p);
    };
    if (!value || typeof value !== "object") return;
    if (value.disclaimer) note(value.disclaimer);
    if (value.note) note(value.note);
    if (value.serving_note) note(value.serving_note);
    switch (kind) {
      case "datasets": {
        const rows = (value.cohorts || []).map((row) => {
          const c = row.cohort || {};
          const e = row.latest_export;
          return [c.cohort_id, c.task_id, c.label_policy_id, c.members, e ? e.manifest_sha256 : "(no export)", e ? JSON.stringify(e.quarantine) : ""];
        });
        for (const e of value.exports || []) {
          if (!(value.cohorts || []).some((row) => row.cohort && row.cohort.cohort_id === e.cohort_id)) {
            rows.push([e.cohort_id, e.task_id, e.label_policy_id, e.rows, e.manifest_sha256, JSON.stringify(e.quarantine)]);
          }
        }
        summary.appendChild(table(["cohort", "task", "label policy", "members/rows", "manifest pin", "quarantine"], rows));
        break;
      }
      case "jobs": {
        const rows = (value.jobs || []).map((j) => {
          const job = j.job || {};
          const actual = j.actual || {};
          return [job.manifest_pin, job.status, j.receipt ? actual.status : "(no receipt)", j.executor ? j.executor.id : "", actual.bundle_digest || "", (j.deviations || []).length];
        });
        summary.appendChild(table(["manifest pin", "job", "run status", "executor", "bundle digest", "deviations"], rows));
        break;
      }
      case "job": {
        const requested = value.requested || {};
        const actual = value.actual || {};
        summary.appendChild(table(["", "requested", "actual"], [
          ["parameters", JSON.stringify(requested.generation_parameters), JSON.stringify(actual.parameters)],
          ["teacher", requested.teacher ? requested.teacher.alias : "", actual.teacher ? `${actual.teacher.provider}/${actual.teacher.model}` : ""],
          ["base", requested.base ? requested.base.alias : "", actual.base ? `${actual.base.provider}/${actual.base.model}` : ""],
          ["cohorts", JSON.stringify((requested.tasks || []).flatMap((t) => t.cohort_ids)), JSON.stringify(actual.cohorts)],
          ["executor", "", value.executor ? `${value.executor.id} (${value.executor.attestation_id || "no attestation"})` : "(no receipt)"],
          ["deviations", "", JSON.stringify(value.deviations || [])],
        ]));
        break;
      }
      case "candidates": {
        const rows = [];
        for (const c of value.candidates || []) {
          const candidate = c.candidate || {};
          const digests = (c.artifacts || []).map((a) => a.bundle_digest).join(", ");
          const verdicts = (c.evaluations || []).map((e) => e.disposition).join(", ") || "(no evaluation)";
          rows.push([candidate.id, digests, JSON.stringify(c.lineage && c.lineage.task_ids), c.lineage && c.lineage.training_manifest_sha256, verdicts]);
        }
        summary.appendChild(table(["candidate", "bundle digests", "task scope", "training manifest", "verdicts"], rows));
        break;
      }
      case "evaluations": {
        for (const e of value.evaluations || []) {
          const rows = (e.verdicts || []).map((m) => [m.assessor, m.positives, m.recall, JSON.stringify(m.missed_ids), JSON.stringify(m.unknown_lowered_ids)]);
          const heading = document.createElement("p");
          heading.className = "learning-note";
          heading.appendChild(mark(e.disposition === "shadow_candidate", `${e.id}: ${e.disposition}`, `${e.id}: ${e.disposition}`));
          heading.appendChild(document.createTextNode(` — reasons: ${JSON.stringify(e.reasons || [])}; families: ${e.families}; permitted action: ${e.permitted_action}`));
          summary.appendChild(heading);
          summary.appendChild(table(["assessor", "positives", "recall", "missed", "unknown lowered"], rows));
        }
        break;
      }
      case "releases": {
        const active = value.active;
        note(active ? `Active release ${active.release.id} bundle ${active.bundle_digest || "(none)"}; eligibility ${active.eligibility.status}` : "No active release.");
        const rows = (value.history || []).map((h) => [h.release.id, h.release.bundle_digest, h.release.rollback_of || "", h.release.approval_id, h.eligibility.status]);
        summary.appendChild(table(["release", "bundle digest", "rollback of", "approval", "eligibility"], rows));
        if (value.serving) {
          const serving = value.serving;
          const line = document.createElement("p");
          line.className = "learning-note";
          const servesActive = serving.serves_active_release === true;
          line.appendChild(mark(servesActive, `Runtime serves the active release (bundle ${serving.bundle_digest || "(none)"}, device ${serving.actual_device || "?"})`,
            serving.error ? `Runtime report failed: ${serving.error}` : `Runtime pinned to bundle ${serving.bundle_digest || "(none)"} is not serving the active release or is not ready${serving.health_error ? `: ${serving.health_error}` : ""}`));
          summary.appendChild(line);
          if (serving.recording_errors && serving.recording_errors.length) note(`Event recording failed: ${serving.recording_errors.join("; ")}`);
        } else if (value.serving_note) {
          note(value.serving_note);
        }
        break;
      }
      case "evidence": {
        const rows = (value.reports || []).map((r) => [r.file, r.kind || "", r.id || "", r.error ? mark(false, "", r.error) : mark(r.sealed, "sealed", "unsealed")]);
        summary.appendChild(table(["file", "kind", "id", "status"], rows));
        break;
      }
      case "bundles": {
        for (const b of value.bundles || []) {
          const heading = document.createElement("p");
          heading.className = "learning-note";
          heading.appendChild(mark(b.verified, `${b.name}: verified`, `${b.name}: ${b.error || "failed"}`));
          summary.appendChild(heading);
          summary.appendChild(table(["entry", "kind", "result"], (b.entries || []).map((e) => [e.path, e.kind, mark(e.verified, "verified", e.detail || "failed")])));
        }
        break;
      }
      case "bundle": {
        summary.appendChild(table(["entry", "kind", "result"], (value.entries || []).map((e) => [e.path, e.kind, mark(e.verified, "verified", e.detail || "failed")])));
        if (value.error) note(value.error);
        break;
      }
      default:
        break;
    }
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
    if (!response.ok) {
      const error = responseError(value, response);
      error.record = value;
      throw error;
    }
    return value;
  }

  function post(path, body) {
    return requestJSON(path, {method: "POST", headers: {"content-type": "application/json"}, body: JSON.stringify(body)});
  }

  function required(id) {
    const input = $(id);
    const value = input && input.value.trim();
    if (!value) throw new Error(`${id} is required`);
    return value;
  }

  function optional(id) {
    const input = $(id);
    return input ? input.value.trim() : "";
  }

  function list(id, allowEmpty = false) {
    const raw = allowEmpty ? optional(id) : required(id);
    return raw ? raw.split(",").map((s) => s.trim()).filter(Boolean) : [];
  }

  function number(id) {
    const n = Number(required(id));
    if (!Number.isFinite(n)) throw new Error(`${id} must be a finite number`);
    return n;
  }

  function integer(id) {
    const n = number(id);
    if (!Number.isInteger(n)) throw new Error(`${id} must be an integer`);
    return n;
  }

  function fileBase64(file) {
    return new Promise((resolve, reject) => {
      const reader = new FileReader();
      reader.onerror = () => reject(new Error(`${file.name} could not be read`));
      reader.onload = () => {
        const url = String(reader.result || "");
        const comma = url.indexOf(",");
        resolve(comma >= 0 ? url.slice(comma + 1) : url);
      };
      reader.readAsDataURL(file);
    });
  }

  function jsonValue(id) {
    const raw = required(id);
    try {
      return JSON.parse(raw);
    } catch (error) {
      throw new Error(`${id} must contain valid JSON: ${error.message || String(error)}`);
    }
  }

  async function run(label, kind, request) {
    setStatus(`${label}…`);
    show("");
    const summary = $("learning-console-summary");
    if (summary) summary.replaceChildren();
    try {
      const value = await request();
      summarize(kind, value);
      show(value);
      setStatus(`${label} complete.`);
    } catch (error) {
      // A refusal is shown verbatim from the record; nothing is reworded.
      show(error.record && error.record.message ? error.record : {error: error.message || String(error), code: error.code || "", status: error.status || 0});
      if (typeof needsReauthentication === "function" && needsReauthentication(error)) {
        if (typeof handleReauthentication === "function") handleReauthentication();
        setStatus(`${label} requires sign-in again.`, true);
      } else {
        setStatus(`${label} refused: ${error.message || String(error)}`, true);
      }
    }
  }

  function installLoaders() {
    const labels = {datasets: "Datasets", jobs: "Distillation jobs", candidates: "Candidates", releases: "Releases", bundles: "Offline release bundles"};
    document.querySelectorAll("button[data-learning-load]").forEach((button) => {
      const what = button.dataset.learningLoad;
      button.onclick = () => run(labels[what] || what, what, () => requestJSON(`${ROOT}/${what}`));
    });
  }

  function onSubmit(formID, label, kind, request) {
    const form = $(formID);
    if (!form) return;
    form.onsubmit = async (event) => {
      event.preventDefault();
      await run(label, kind, request);
    };
  }

  installLoaders();
  onSubmit("learning-export-form", "Authorized export", "export", () => post(`${ROOT}/datasets/exports`, {cohort_id: required("learning-cohort-id")}));
  onSubmit("learning-job-inspect-form", "Job", "job", () => requestJSON(`${ROOT}/jobs/${encodeURIComponent(required("learning-job-pin"))}`));
  onSubmit("learning-job-compose-form", "Manifest submission", "job-submit", () => {
    const cohorts = list("learning-job-cohorts");
    const exports = list("learning-job-export-pins").map((pair) => {
      const [cohort, pin] = pair.split("=").map((s) => s.trim());
      if (!cohort || !pin) throw new Error("export pins must be cohort=sha256");
      return {cohort_id: cohort, manifest_sha256: pin};
    });
    const teacher = jsonValue("learning-job-teacher");
    const chosen = (teacher.eligible || []).find((c) => c.alias === teacher.chosen);
    const manifest = {
      schema: 1,
      kind: "distillation-manifest",
      training_manifest_sha256: required("learning-job-training-pin"),
      teacher,
      base: jsonValue("learning-job-base"),
      teacher_egress_at_generation: chosen && chosen.identity ? chosen.identity.egress : "",
      data: {
        tasks: [{task_id: required("learning-job-task"), cohort_ids: cohorts, label_policy_ids: list("learning-job-label-policies")}],
        evidence_families: list("learning-job-families"),
        prompt_version: required("learning-job-prompt"),
        generation_parameters: jsonValue("learning-job-parameters"),
        held_out_example_ids: list("learning-job-held-out", true).sort(),
      },
      exports,
    };
    return post(`${ROOT}/jobs`, manifest);
  });
  onSubmit("learning-predictions-form", "Teacher predictions", "predictions", () => post(`${ROOT}/jobs/${encodeURIComponent(required("learning-predictions-pin"))}/predictions`, jsonValue("learning-predictions-json")));
  onSubmit("learning-receipt-form", "Receipt submission", "receipt", () => post(`${ROOT}/jobs/${encodeURIComponent(required("learning-receipt-pin"))}/receipt`, jsonValue("learning-receipt-json")));
  onSubmit("learning-evaluations-form", "Evaluations", "evaluations", () => requestJSON(`${ROOT}/candidates/${encodeURIComponent(required("learning-evaluations-candidate"))}/evaluations`));
  onSubmit("learning-candidate-form", "Candidate record", "candidate", () => post(`${ROOT}/candidates`, jsonValue("learning-candidate-json")));
  onSubmit("learning-seal-form", "Seal run", "seal", async () => {
    const input = $("learning-seal-bundle");
    const files = input && input.files ? Array.from(input.files) : [];
    if (!files.length) throw new Error("learning-seal-bundle requires the bundle's files");
    const bundle = {};
    for (const file of files) bundle[file.name] = await fileBase64(file);
    const body = {
      manifest_pin: required("learning-seal-pin"),
      bundle,
      evaluation_policy: {
        id: required("learning-seal-policy-id"),
        split: required("learning-seal-split"),
        positive_label: required("learning-seal-positive"),
        minimum_recall: number("learning-seal-recall"),
        minimum_positives: integer("learning-seal-positives"),
        minimum_families: integer("learning-seal-families"),
      },
    };
    const cohort = optional("learning-seal-cohort");
    if (cohort) body.cohort_id = cohort;
    return post(`${ROOT}/candidates/seal`, body);
  });
  onSubmit("learning-predict-form", "Prediction", "predict", () => post(`${ROOT}/serving/predict`, {
    features: list("learning-predict-features").map((value) => {
      const n = Number(value);
      if (!Number.isFinite(n)) throw new Error(`feature ${JSON.stringify(value)} is not a finite number`);
      return n;
    }),
  }));
  onSubmit("learning-evaluation-form", "Evaluation record", "evaluation", () => post(`${ROOT}/evaluations`, jsonValue("learning-evaluation-json")));
  onSubmit("learning-promote-form", "Promotion", "promote", () => post(`${ROOT}/releases/promote`, {
    candidate_id: required("learning-promote-candidate"),
    evaluation_id: optional("learning-promote-evaluation"),
  }));
  onSubmit("learning-rollback-form", "Rollback", "rollback", () => post(`${ROOT}/releases/rollback`, {
    prior_release_id: required("learning-rollback-prior"),
  }));
  onSubmit("learning-evidence-form", "Evidence", "evidence", () => requestJSON(`${ROOT}/evidence/${encodeURIComponent(required("learning-evidence-kind"))}`));
  onSubmit("learning-bundle-export-form", "Bundle export", "bundle", () => post(`${ROOT}/bundles/export`, {
    release_id: required("learning-bundle-export-release"),
    name: required("learning-bundle-export-name"),
  }));
  onSubmit("learning-bundle-import-form", "Bundle import", "bundle", () => {
    const body = {name: required("learning-bundle-import-name")};
    const expected = optional("learning-bundle-import-index");
    if (expected) body.expected_index_digest = expected;
    return post(`${ROOT}/bundles/import`, body);
  });
})();
