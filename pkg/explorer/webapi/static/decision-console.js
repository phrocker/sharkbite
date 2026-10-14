// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
//
// The decision console is deliberately a thin client over the authenticated
// decision APIs. It never treats a response as a promotion, a verified label,
// or permission to omit full review.
(() => {
  "use strict";

  const $ = (id) => document.getElementById(id);

  function setStatus(text, error = false) {
    const status = $("decision-console-status");
    if (!status) return;
    status.textContent = text;
    status.className = error ? "decision-status-error" : "muted";
  }

  function show(value) {
    const output = $("decision-console-output");
    if (!output) return;
    output.textContent = typeof value === "string"
      ? value
      : JSON.stringify(value, null, 2);
  }

  function responseError(value, response) {
    if (typeof apiError === "function") return apiError(value, response);
    const error = new Error(
      (value && value.message) || (response && response.statusText) || "request failed");
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

  function newKey() {
    const bytes = new Uint8Array(18);
    if (!window.crypto || !window.crypto.getRandomValues) {
      throw new Error("this browser cannot create an idempotency key safely");
    }
    window.crypto.getRandomValues(bytes);
    let value = "";
    for (const byte of bytes) value += String.fromCharCode(byte);
    return btoa(value).replace(/=/g, "").replace(/\+/g, "-").replace(/\//g, "_");
  }

  function required(id) {
    const input = $(id);
    const value = input && input.value.trim();
    if (!value) throw new Error(`${id} is required`);
    return value;
  }

  function jsonBody(id) {
    const raw = required(id);
    try {
      const value = JSON.parse(raw);
      if (!value || typeof value !== "object" || Array.isArray(value)) {
        throw new Error("request JSON must be an object");
      }
      return JSON.stringify(value);
    } catch (error) {
      throw new Error(`${id} must contain valid JSON: ${error.message || String(error)}`);
    }
  }

  async function run(label, request) {
    setStatus(`${label}…`);
    show("");
    try {
      const value = await request();
      show(value);
      setStatus(`${label} complete.`);
    } catch (error) {
      show({error: error.message || String(error), code: error.code || "", status: error.status || 0});
      if (typeof needsReauthentication === "function" && needsReauthentication(error)) {
        if (typeof handleReauthentication === "function") handleReauthentication();
        setStatus(`${label} requires sign-in again.`, true);
      } else {
        setStatus(`${label} failed: ${error.message || String(error)}`, true);
      }
    }
  }

  function installKeyButton(buttonID, inputID) {
    const button = $(buttonID);
    const input = $(inputID);
    if (button && input) button.onclick = () => { input.value = newKey(); };
  }

  function installDecisionForm() {
    const form = $("decision-request-form");
    if (!form) return;
    form.onsubmit = async (event) => {
      event.preventDefault();
      const mode = event.submitter && event.submitter.dataset.decisionMode || "read";
      await run(mode === "evaluate" ? "Decision evaluation" : "Decision receipt", async () => {
        const id = required("decision-request-id");
        const key = required("decision-key");
        const headers = {"idempotency-key": key};
        if (mode === "evaluate") {
          return requestJSON("/api/v1/decisions", {
            method: "POST",
            headers: {"content-type": "application/json", ...headers},
            body: JSON.stringify({request_id: id}),
          });
        }
        return requestJSON(`/api/v1/decisions/${encodeURIComponent(id)}`, {
          headers,
        });
      });
    };
  }

  function installRegistrationForm() {
    const form = $("registration-inspect-form");
    if (!form) return;
    form.onsubmit = async (event) => {
      event.preventDefault();
      await run("Registration read", () => requestJSON(
        `/api/v1/decision-registrations/${encodeURIComponent(required("registration-id"))}`));
    };
  }

  function installJSONSubmit(formID, label, path, keyID, bodyID) {
    const form = $(formID);
    if (!form) return;
    form.onsubmit = async (event) => {
      event.preventDefault();
      await run(label, () => requestJSON(path, {
        method: "POST",
        headers: {"content-type": "application/json", "idempotency-key": required(keyID)},
        body: jsonBody(bodyID),
      }));
    };
  }

  function installDatasetForm() {
    const form = $("dataset-export-form");
    if (!form) return;
    form.onsubmit = async (event) => {
      event.preventDefault();
      await run("Cohort export", () => requestJSON("/api/v1/dataset-exports", {
        method: "POST",
        headers: {"content-type": "application/json"},
        body: JSON.stringify({cohort_id: required("cohort-id")}),
      }));
    };
  }

  function installOutcomeForm() {
    const form = $("outcome-read-form");
    if (!form) return;
    form.onsubmit = async (event) => {
      event.preventDefault();
      await run("Outcome read", () => requestJSON(
        `/api/v1/outcomes/${encodeURIComponent(required("outcome-request-id"))}/${encodeURIComponent(required("outcome-prediction-id"))}`,
        {headers: {"idempotency-key": required("outcome-key")}}));
    };
  }

  function installAdjudicationForm() {
    const form = $("adjudication-history-form");
    if (!form) return;
    form.onsubmit = async (event) => {
      event.preventDefault();
      await run("Adjudication history read", () => requestJSON(
        `/api/v1/adjudications/${encodeURIComponent(required("adjudication-target-id"))}`));
    };
  }

  installKeyButton("decision-new-key", "decision-key");
  installKeyButton("outcome-new-key", "outcome-key");
  if ($("registration-key") && !$("registration-key").value) $("registration-key").value = newKey();
  if ($("outcome-submit-key") && !$("outcome-submit-key").value) $("outcome-submit-key").value = newKey();
  if ($("adjudication-key") && !$("adjudication-key").value) $("adjudication-key").value = newKey();
  if ($("decision-key") && !$("decision-key").value) $("decision-key").value = newKey();
  if ($("outcome-key") && !$("outcome-key").value) $("outcome-key").value = newKey();
  installDecisionForm();
  installRegistrationForm();
  installJSONSubmit("registration-submit-form", "Registration submission", "/api/v1/decision-registrations", "registration-key", "registration-json");
  installDatasetForm();
  installOutcomeForm();
  installJSONSubmit("outcome-submit-form", "Outcome report", "/api/v1/outcomes", "outcome-submit-key", "outcome-json");
  installAdjudicationForm();
  installJSONSubmit("adjudication-submit-form", "Adjudication submission", "/api/v1/adjudications", "adjudication-key", "adjudication-json");
})();
