(() => {
  "use strict";

  const workspaceHeader = "Shoal-Workspace-ID";
  const storageKey = "shoal.workspace.id";
  const nativeFetch = window.fetch.bind(window);
  let workspaceID = readWorkspaceID();
  let identityReady = false;
  let elements = null;
  let lensState = null;
  let loadGeneration = 0;
  let pendingLensMutation = null;

  function encodeOpaqueID(bytes) {
    const alphabet =
      "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
    let output = "";
    for (let index = 0; index < bytes.length; index += 3) {
      const first = bytes[index];
      const second = index + 1 < bytes.length ? bytes[index + 1] : 0;
      const third = index + 2 < bytes.length ? bytes[index + 2] : 0;
      output += alphabet[first >> 2];
      output += alphabet[((first & 3) << 4) | (second >> 4)];
      if (index + 1 < bytes.length) {
        output += alphabet[((second & 15) << 2) | (third >> 6)];
      }
      if (index + 2 < bytes.length) output += alphabet[third & 63];
    }
    return output;
  }

  function canonicalOpaqueID(value) {
    if (typeof value !== "string" || value === "" ||
        !/^[A-Za-z0-9_-]+$/.test(value) || value.length % 4 === 1) {
      return "";
    }
    try {
      const padded = value.replace(/-/g, "+").replace(/_/g, "/") +
        "=".repeat((4 - value.length % 4) % 4);
      const decoded = atob(padded);
      const bytes = Uint8Array.from(decoded, (character) =>
        character.charCodeAt(0));
      return encodeOpaqueID(bytes) === value ? value : "";
    } catch (_) {
      return "";
    }
  }

  function readWorkspaceID() {
    try {
      const stored = sessionStorage.getItem(storageKey) || "";
      const canonical = canonicalOpaqueID(stored);
      if (stored && !canonical) sessionStorage.removeItem(storageKey);
      return canonical;
    } catch (_) {
      return "";
    }
  }

  function persistWorkspaceID(value) {
    workspaceID = value;
    try {
      if (value) sessionStorage.setItem(storageKey, value);
      else sessionStorage.removeItem(storageKey);
    } catch (_) {
      // Settings remain usable for this page even when storage is disabled.
    }
  }

  function workspaceManagementPath(pathname) {
    return /^\/api\/v1\/workspaces\/[^/]+\/settings(?:\/lens)?$/.test(
      pathname);
  }

  function workspaceAppliesTo(pathname) {
    if (pathname === "/api/v1/auth-config" ||
        workspaceManagementPath(pathname)) return false;
    return pathname.startsWith("/api/v1/");
  }

  function mergeRequestHeaders(input, init) {
    const headers = new Headers(
      typeof Request !== "undefined" && input instanceof Request
        ? input.headers
        : undefined,
    );
    if (init && init.headers) {
      new Headers(init.headers).forEach((value, name) =>
        headers.set(name, value));
    }
    headers.set(workspaceHeader, workspaceID);
    return headers;
  }

  function requestURL(input) {
    const value =
      typeof Request !== "undefined" && input instanceof Request
        ? input.url
        : String(input);
    return new URL(value, window.location.href);
  }

  window.fetch = async (input, init) => {
    const url = requestURL(input);
    const attach = workspaceID && url.origin === window.location.origin &&
      workspaceAppliesTo(url.pathname);
    const response = attach
      ? await nativeFetch(input, {
          ...(init || {}),
          headers: mergeRequestHeaders(input, init),
        })
      : await nativeFetch(input, init);
    const bearerChallenge = response.headers &&
      /^Bearer(?:\s|$)/i.test(
        response.headers.get("WWW-Authenticate") || "");
    if (url.origin === window.location.origin &&
        url.pathname === "/api/v1/identity" &&
        (response.ok || response.status === 404 ||
          (response.status === 401 && !bearerChallenge))) {
      identityReady = true;
      queueLensLoad();
    }
    return response;
  };

  function issuerHeaders() {
    try {
      if (typeof authHeaders === "function") return authHeaders();
    } catch (_) {
      // Authentication remains owned by app.js and the server.
    }
    return {};
  }

  function setStatus(message, kind = "muted") {
    if (!elements) return;
    elements.status.className = kind;
    elements.status.textContent = message;
  }

  function shortOpaqueID(value) {
    return value.length > 24
      ? `${value.slice(0, 10)}…${value.slice(-10)}`
      : value;
  }

  async function responseValue(response) {
    let value = {};
    try {
      value = await response.json();
    } catch (_) {
      // A structured server envelope is preferred but not assumed.
    }
    if (!response.ok) {
      const error = new Error(
        value.message || response.statusText || "request failed");
      error.indeterminate = Boolean(value.indeterminate) ||
        Boolean(response.headers &&
          response.headers.get("Shoal-Commit-Outcome") === "indeterminate");
      throw error;
    }
    return value;
  }

  function identityKey(identity) {
    return `${identity.schema_id}.${identity.version_id}`;
  }

  function choiceKey(choice) {
    return identityKey(choice.identity);
  }

  function renderChoices(value) {
    pendingLensMutation = null;
    lensState = value;
    const selected = value.selected_ontology
      ? identityKey(value.selected_ontology)
      : "";
    elements.lens.replaceChildren();
    let defaultValue = selected;
    for (const choice of value.choices || []) {
      const option = document.createElement("option");
      option.value = choiceKey(choice);
      const isSelected = option.value === selected;
      option.textContent =
        `${isSelected ? "Selected · " : ""}` +
        `${choice.active ? "Active · " : ""}` +
        `${choice.version ? `${choice.version} · ` : ""}` +
        `${shortOpaqueID(choice.identity.schema_id)} / ` +
        shortOpaqueID(choice.identity.version_id);
      elements.lens.append(option);
      if (!defaultValue && choice.active) defaultValue = option.value;
    }
    if (defaultValue) elements.lens.value = defaultValue;
    const available = elements.lens.options.length > 0;
    const mutable = available && !selected;
    elements.lens.disabled = !mutable;
    elements.apply.disabled = !mutable;
    if (!available) {
      setStatus("No governed ontology lens is currently selectable.");
      return;
    }
    const revision = Number(value.settings_revision || 0);
    setStatus(
      selected
        ? `Workspace revision ${revision}; selected lens is enforced.`
        : `Workspace revision ${revision}; choose a governed lens.`,
    );
  }

  async function loadChoices() {
    if (!elements || !workspaceID) return;
    const generation = ++loadGeneration;
    elements.refresh.disabled = true;
    elements.apply.disabled = true;
    setStatus("Loading governed ontology lenses…");
    try {
      const response = await window.fetch(
        `/api/v1/workspaces/${workspaceID}/settings/lens`,
        {headers: {"accept": "application/json", ...issuerHeaders()}},
      );
      const value = await responseValue(response);
      if (generation === loadGeneration) renderChoices(value);
    } catch (error) {
      if (generation === loadGeneration) {
        lensState = null;
        elements.lens.replaceChildren();
        elements.lens.disabled = true;
        setStatus(`Unable to load workspace lenses: ${error.message}`, "error");
      }
    } finally {
      if (generation === loadGeneration) {
        elements.refresh.disabled = false;
      }
    }
  }

  function queueLensLoad() {
    if (!elements || !workspaceID || !identityReady) return;
    window.setTimeout(() => loadChoices(), 0);
  }

  function mutationID() {
    if (!window.crypto || typeof window.crypto.getRandomValues !== "function") {
      throw new Error("secure mutation identifiers are unavailable");
    }
    const bytes = new Uint8Array(16);
    window.crypto.getRandomValues(bytes);
    return encodeOpaqueID(bytes);
  }

  async function applyLens() {
    if (!lensState || !workspaceID) return;
    if (lensState.selected_ontology) {
      setStatus("The selected workspace lens is immutable.");
      return;
    }
    const selected = (lensState.choices || []).find(
      (choice) => choiceKey(choice) === elements.lens.value);
    if (!selected) {
      setStatus("Select one governed ontology lens.", "error");
      return;
    }
    elements.apply.disabled = true;
    elements.refresh.disabled = true;
    setStatus("Applying the governed ontology lens…");
    try {
      const expectedRevision = Number(lensState.settings_revision || 0);
      const selection = choiceKey(selected);
      if (!pendingLensMutation ||
          pendingLensMutation.expectedRevision !== expectedRevision ||
          pendingLensMutation.selection !== selection) {
        pendingLensMutation = {
          expectedRevision,
          mutationID: mutationID(),
          selection,
        };
      }
      let response;
      try {
        response = await window.fetch(
          `/api/v1/workspaces/${workspaceID}/settings/lens`,
          {
            method: "PUT",
            headers: {
              "content-type": "application/json",
              "accept": "application/json",
              ...issuerHeaders(),
            },
            body: JSON.stringify({
              expected_revision: pendingLensMutation.expectedRevision,
              mutation_id: pendingLensMutation.mutationID,
              selected_ontology: selected.identity,
            }),
          },
        );
      } catch (error) {
        const indeterminate = new Error(
          error && error.message ? error.message : "request failed");
        indeterminate.indeterminate = true;
        throw indeterminate;
      }
      const value = await responseValue(response);
      pendingLensMutation = null;
      setStatus(
        `Lens saved at workspace revision ${Number(value.revision || 0)}; ` +
        "reloading under the narrowed decision.",
      );
      window.location.reload();
    } catch (error) {
      if (!error.indeterminate) pendingLensMutation = null;
      setStatus(
        error.indeterminate
          ? `Lens outcome is indeterminate; retry will reuse the same ` +
            `mutation ID: ${error.message}`
          : `Unable to apply workspace lens: ${error.message}`,
        "error",
      );
      elements.apply.disabled = false;
      elements.refresh.disabled = false;
    }
  }

  function initialize() {
    elements = {
      form: document.getElementById("workspace-settings-form"),
      input: document.getElementById("workspace-settings-id"),
      fresh: document.getElementById("workspace-settings-new"),
      clear: document.getElementById("workspace-settings-clear"),
      lens: document.getElementById("workspace-settings-lens"),
      refresh: document.getElementById("workspace-settings-refresh"),
      apply: document.getElementById("workspace-settings-apply"),
      status: document.getElementById("workspace-settings-status"),
    };
    if (Object.values(elements).some((element) => !element)) return;
    elements.input.value = workspaceID;
    elements.clear.disabled = !workspaceID;
    elements.refresh.disabled = !workspaceID;
    if (workspaceID) {
      setStatus(
        identityReady
          ? "Loading governed ontology lenses…"
          : "Workspace selected; waiting for authenticated identity…",
      );
    }
    elements.form.addEventListener("submit", (event) => {
      event.preventDefault();
      const selected = canonicalOpaqueID(elements.input.value.trim());
      if (!selected) {
        setStatus(
          "Workspace ID must be canonical unpadded base64url.", "error");
        return;
      }
      persistWorkspaceID(selected);
      window.location.reload();
    });
    elements.fresh.addEventListener("click", () => {
      persistWorkspaceID(mutationID());
      window.location.reload();
    });
    elements.clear.addEventListener("click", () => {
      persistWorkspaceID("");
      window.location.reload();
    });
    elements.refresh.addEventListener("click", () => loadChoices());
    elements.apply.addEventListener("click", () => applyLens());
    queueLensLoad();
  }

  window.ShoalWorkspaceSettings = Object.freeze({
    selectedWorkspaceID: () => workspaceID,
    requestHeaders: () =>
      workspaceID ? {[workspaceHeader]: workspaceID} : {},
    refreshOntologyChoices: () => loadChoices(),
  });

  window.addEventListener("DOMContentLoaded", initialize, {once: true});
})();
