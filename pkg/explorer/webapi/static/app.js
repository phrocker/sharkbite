const state = {
  snapshot: {},
  cursor: "",
  document: null,
  currentDocumentID: "",
  selectedSectionID: "",
  nodes: new Map(),
  edges: new Map(),
  assertions: new Map(),
  graphCursors: new Map(),
  sourceURIs: new Map(),
  selected: null,
  selectedEdge: null,
  derivationDigests: new Map(),
  identity: null,
  accessToken: null,
  auth: {configured: false},
  ontology: null,
  ontologyProposals: null,
  workspaceID: "",
  workspaceSettings: null,
  lensChoices: null,
  reauthRequired: false,
  capabilities: {
    documents: false,
    document: false,
    retrieve: false,
    neighborhood: false,
    path: false,
    vector_retrieval: false,
    ingest: false,
    extraction: false,
    decisions: false,
    decision_registrations: false,
    decision_datasets: false,
    decision_outcomes: false,
    decision_adjudications: false,
  },
  uploadLimits: {
    max_upload_files: 0,
    max_upload_file_bytes: 0,
    max_upload_total_bytes: 0,
  },
};
const $ = (id) => document.getElementById(id);
let documentGeneration = 0;
let searchGeneration = 0;
let documentsGeneration = 0;
let graphGeneration = 0;
let documentsLoading = false;
let documentsRefreshQueued = false;
let uploadLoading = false;
let ontologyGeneration = 0;

async function api(path, body) {
  const response = await fetch(`/api/v1/${path}`, {
    method: "POST",
    headers: {"content-type": "application/json", ...authHeaders()},
    body: JSON.stringify(body),
  });
  const value = await response.json();
  if (!response.ok) throw apiError(value, response);
  return value;
}

// apiError preserves the server's structured error envelope so callers can
// tell an authorization denial apart from any other failure. The workspace
// must never render a denial as if it were a generic error or an empty result.
function apiError(value, response) {
  const error = new Error((value && value.message) || (response && response.statusText) || "request failed");
  error.code = (value && value.code) || "";
  error.status = response ? response.status : 0;
  error.reauthenticate = challengedForBearer(response);
  return error;
}

// isDenied reports whether an error is an explicit authorization denial the
// server signalled, as opposed to a match that simply returned nothing. Only
// the unambiguous unauthorized signal counts: a 404 is deliberately
// indistinguishable at the server between "hidden from you" and "absent", so
// it is never claimed here as a denial.
function isDenied(error) {
  if (!error) return false;
  // A re-authentication signal is a token problem, never an authorization
  // denial. It must not read as "access denied", which would misdescribe the
  // governance state, so it is excluded here before the unauthorized test.
  if (needsReauthentication(error)) return false;
  return error.code === "unauthorized" || error.status === 401;
}

// authHeaders adds the bearer token to an API request only when one has been
// acquired in memory. With -dev-auth no token exists, so the header is absent
// and the request is byte-identical to the local development path.
function authHeaders() {
  const headers = state.accessToken ? {authorization: `Bearer ${state.accessToken}`} : {};
  if (state.workspaceID) headers["Shoal-Workspace-ID"] = state.workspaceID;
  return headers;
}

// challengedForBearer reports whether the transport answered with the standard
// bearer challenge. The central gate sets WWW-Authenticate: Bearer only when
// authentication itself failed (no or expired token); an authorization denial
// from the service is also HTTP 401 but carries no such header. This header is
// therefore the only reliable way to tell "sign in again" apart from "you are
// authenticated but not permitted": the UI never infers it from status alone.
function challengedForBearer(response) {
  if (!response || !response.headers || typeof response.headers.get !== "function") {
    return false;
  }
  const header = response.headers.get("WWW-Authenticate") || "";
  return header.toLowerCase().includes("bearer");
}

// needsReauthentication reports whether an error means the caller must obtain a
// fresh token, as opposed to being denied. It requires both a 401 status and
// the bearer challenge, so a governance 401 (no challenge) is never treated as
// a token expiry and never triggers a login redirect.
function needsReauthentication(error) {
  return Boolean(error) && error.status === 401 && error.reauthenticate === true;
}

// reauthenticationText states plainly that a token expiry is not an
// authorization decision, so re-authentication never reads as a denial and the
// governance wording elsewhere is not undermined.
function reauthenticationText() {
  return "Your sign-in has expired or is not established. This is not an " +
    "authorization denial — sign in again to continue. Your access to the " +
    "workspace is unchanged.";
}

// handleReauthentication surfaces the sign-in affordance again after a token
// expiry. It never makes a security decision: the server remains the only
// enforcement point. It is a no-op when no login is configured (the -dev-auth
// path) or when the shell has no login control (the test harness).
function handleReauthentication() {
  state.reauthRequired = true;
  if (!state.auth || !state.auth.configured) return;
  const button = document.getElementById("login");
  if (!button) return;
  button.hidden = false;
  button.textContent = "Sign in again";
}

// base64UrlEncode encodes bytes as unpadded base64url (RFC 4648 §5), the
// encoding PKCE requires for the code verifier and challenge. It avoids btoa so
// it is pure and testable without a browser global.
function base64UrlEncode(bytes) {
  const alphabet =
    "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
  let out = "";
  for (let index = 0; index < bytes.length; index += 3) {
    const b0 = bytes[index];
    const b1 = index + 1 < bytes.length ? bytes[index + 1] : 0;
    const b2 = index + 2 < bytes.length ? bytes[index + 2] : 0;
    out += alphabet[b0 >> 2];
    out += alphabet[((b0 & 3) << 4) | (b1 >> 4)];
    if (index + 1 < bytes.length) out += alphabet[((b1 & 15) << 2) | (b2 >> 6)];
    if (index + 2 < bytes.length) out += alphabet[b2 & 63];
  }
  return out;
}

// base64UrlLookup maps a base64url character code to its 6-bit value, with -1
// for any character outside the alphabet. It is the inverse of the alphabet in
// base64UrlEncode and, like it, avoids atob so decoding stays pure and testable
// without a browser global.
const base64UrlLookup = (() => {
  const alphabet =
    "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_";
  const table = new Int16Array(128).fill(-1);
  for (let index = 0; index < alphabet.length; index += 1) {
    table[alphabet.charCodeAt(index)] = index;
  }
  return table;
})();

// utf8BytesToString decodes a UTF-8 byte array to a string without TextDecoder,
// which the executable UI harness does not provide. Wire metadata values are
// arbitrary bytes, so this handles the full range rather than assuming ASCII.
function utf8BytesToString(bytes) {
  let out = "";
  let index = 0;
  while (index < bytes.length) {
    const lead = bytes[index++];
    if (lead < 0x80) {
      out += String.fromCharCode(lead);
    } else if (lead < 0xe0) {
      const b1 = bytes[index++] & 0x3f;
      out += String.fromCharCode(((lead & 0x1f) << 6) | b1);
    } else if (lead < 0xf0) {
      const b1 = bytes[index++] & 0x3f;
      const b2 = bytes[index++] & 0x3f;
      out += String.fromCharCode(((lead & 0x0f) << 12) | (b1 << 6) | b2);
    } else {
      const b1 = bytes[index++] & 0x3f;
      const b2 = bytes[index++] & 0x3f;
      const b3 = bytes[index++] & 0x3f;
      const point =
        (((lead & 0x07) << 18) | (b1 << 12) | (b2 << 6) | b3) - 0x10000;
      out += String.fromCharCode(0xd800 + (point >> 10), 0xdc00 + (point & 0x3ff));
    }
  }
  return out;
}

// base64UrlDecode is the inverse of base64UrlEncode: it turns the unpadded
// base64url the wire uses for every identifier and every metadata key and value
// back into its decoded string. The webapi encodes metadata keys AND values, so
// the graph cannot read an edge's origin or score without decoding here.
function base64UrlDecode(text) {
  const clean = String(text || "");
  const bytes = [];
  let buffer = 0;
  let bits = 0;
  for (let index = 0; index < clean.length; index += 1) {
    const value = base64UrlLookup[clean.charCodeAt(index)];
    if (value === undefined || value < 0) continue;
    buffer = (buffer << 6) | value;
    bits += 6;
    if (bits >= 8) {
      bits -= 8;
      bytes.push((buffer >> bits) & 0xff);
    }
  }
  return utf8BytesToString(bytes);
}
function randomUrlToken(byteLength) {
  const bytes = new Uint8Array(byteLength);
  crypto.getRandomValues(bytes);
  return base64UrlEncode(bytes);
}

// PKCE_METHOD is S256 and is the only challenge method this client emits. The
// plain method is deliberately absent: downgrading to plain would let anyone
// who observes the redirect replay the authorization code. Nothing here ever
// produces or accepts "plain".
const PKCE_METHOD = "S256";

// pkceChallengeFromVerifier derives the S256 challenge: base64url(SHA-256(v)).
async function pkceChallengeFromVerifier(verifier) {
  const data = new TextEncoder().encode(verifier);
  const digest = await crypto.subtle.digest("SHA-256", data);
  return base64UrlEncode(new Uint8Array(digest));
}

function encodeForm(pairs) {
  return pairs
    .map(([key, value]) => `${encodeURIComponent(key)}=${encodeURIComponent(value)}`)
    .join("&");
}

// buildAuthorizeUrl composes the Authorization Code + PKCE request. The
// challenge method is fixed to S256 by construction.
function buildAuthorizeUrl(config, params) {
  const query = encodeForm([
    ["client_id", config.client_id],
    ["response_type", "code"],
    ["redirect_uri", params.redirectUri],
    ["response_mode", "query"],
    ["scope", config.scope],
    ["state", params.state],
    ["code_challenge", params.codeChallenge],
    ["code_challenge_method", PKCE_METHOD],
  ]);
  const separator = config.authorization_endpoint.includes("?") ? "&" : "?";
  return `${config.authorization_endpoint}${separator}${query}`;
}

// verifyCallbackState is the CSRF defence on the redirect. It accepts a
// callback only when the returned state exactly matches the value generated and
// stored before redirecting. A missing stored value never matches, so a forged
// callback with no prior login attempt is refused.
function verifyCallbackState(returnedState, expectedState) {
  return Boolean(expectedState) && returnedState === expectedState;
}

// callbackParams parses a redirect query string into a flat map without relying
// on URLSearchParams, so it is pure and testable in any environment.
function callbackParams(search) {
  const params = {};
  const query = String(search || "").replace(/^\?/, "");
  if (!query) return params;
  for (const pair of query.split("&")) {
    if (!pair) continue;
    const eq = pair.indexOf("=");
    const rawKey = eq < 0 ? pair : pair.slice(0, eq);
    const rawValue = eq < 0 ? "" : pair.slice(eq + 1);
    params[decodeURIComponent(rawKey)] =
      decodeURIComponent(rawValue.replace(/\+/g, " "));
  }
  return params;
}

// exchangeAuthorizationCode verifies state and then swaps the code for an
// access token. State is checked BEFORE the code is presented, so a mismatched
// or forged callback never reaches the network. deps.fetch is injected so the
// exchange is exercisable without a browser. The returned token is handed back
// to the caller and never written to storage.
async function exchangeAuthorizationCode(config, callback, stored, deps) {
  const now = deps && typeof deps.now === "function" ? deps.now() : Date.now();
  if (!stored || !Number.isFinite(stored.createdAt) ||
      stored.createdAt > now ||
      now - stored.createdAt > LOGIN_FLOW_MAX_AGE_MS) {
    throw new Error("login attempt expired; start sign-in again");
  }
  if (!verifyCallbackState(callback.state, stored && stored.state)) {
    throw new Error("login state did not match; the callback was rejected");
  }
  const body = encodeForm([
    ["client_id", config.client_id],
    ["grant_type", "authorization_code"],
    ["code", callback.code],
    ["redirect_uri", stored.redirectUri],
    ["code_verifier", stored.verifier],
    ["scope", config.scope],
  ]);
  const response = await deps.fetch(config.token_endpoint, {
    method: "POST",
    headers: {
      "content-type": "application/x-www-form-urlencoded",
      "accept": "application/json",
    },
    body,
  });
  const value = await response.json();
  if (!response.ok) {
    throw new Error(
      (value && (value.error_description || value.error)) || "token request failed");
  }
  return value.access_token;
}

const LOGIN_FLOW_KEY = "shoal-login-flow";
const LOGIN_FLOW_MAX_AGE_MS = 10 * 60 * 1000;

function browserEnvironment() {
  return typeof window !== "undefined" &&
    typeof window.location !== "undefined" &&
    typeof window.sessionStorage !== "undefined";
}

function redirectUri() {
  return `${window.location.origin}/`;
}

function readLoginFlow() {
  try {
    return JSON.parse(window.sessionStorage.getItem(LOGIN_FLOW_KEY) || "null");
  } catch (_) {
    return null;
  }
}

function clearLoginFlow() {
  try {
    window.sessionStorage.removeItem(LOGIN_FLOW_KEY);
  } catch (_) {
    // Storage may be unavailable; the flow is single-use regardless.
  }
}

function clearCallbackFromUrl() {
  if (typeof window.history === "undefined" || !window.history.replaceState) return;
  window.history.replaceState({}, document.title, window.location.pathname);
}

// beginLogin starts the interactive Authorization Code + PKCE flow. The
// verifier and state are single-use login-flow artifacts — not the credential —
// and must survive the full-page redirect, so they live briefly in
// sessionStorage and are cleared the instant the callback is consumed. The
// access token itself is never written to any storage (see
// completeLoginFromRedirect); it is held in memory only.
async function beginLogin() {
  if (!browserEnvironment() || !state.auth || !state.auth.configured) return;
  const verifier = randomUrlToken(48);
  const codeChallenge = await pkceChallengeFromVerifier(verifier);
  const flow = {
    verifier,
    state: randomUrlToken(24),
    redirectUri: redirectUri(),
    createdAt: Date.now(),
  };
  window.sessionStorage.setItem(LOGIN_FLOW_KEY, JSON.stringify(flow));
  window.location.assign(buildAuthorizeUrl(state.auth, {
    redirectUri: flow.redirectUri,
    state: flow.state,
    codeChallenge,
  }));
}

// completeLoginFromRedirect finishes a login when the page was loaded as an
// OIDC redirect callback. It consumes and clears the stored flow and strips the
// code from the address bar before doing anything else, then exchanges the code
// for a token that is kept in memory only.
async function completeLoginFromRedirect(config) {
  if (!browserEnvironment()) return false;
  const params = callbackParams(window.location.search);
  if (!params.code && !params.error) return false;
  const stored = readLoginFlow();
  clearLoginFlow();
  clearCallbackFromUrl();
  if (params.error) {
    throw new Error(params.error_description || params.error);
  }
  const token = await exchangeAuthorizationCode(config, params, stored, {
    fetch: (url, options) => fetch(url, options),
  });
  if (!token) throw new Error("token endpoint returned no access token");
  state.accessToken = token;
  state.reauthRequired = false;
  return true;
}

async function loadAuthConfig() {
  try {
    const response = await fetch("/api/v1/auth-config", {
      headers: {"accept": "application/json"},
    });
    if (!response.ok) return {configured: false};
    return await response.json();
  } catch (_) {
    return {configured: false};
  }
}

// configureLoginUI reveals the sign-in control only when a login is configured
// and the caller holds no token yet. With -dev-auth the config reports
// unconfigured, so no login UI renders at all and the local path is unchanged.
function configureLoginUI(config) {
  const button = document.getElementById("login");
  if (!button) return;
  if (!config || !config.configured) {
    button.hidden = true;
    return;
  }
  button.onclick = () => { beginLogin(); };
  button.hidden = Boolean(state.accessToken);
  button.textContent = "Sign in";
}

function renderLoginError(error) {
  const container = document.getElementById("identity");
  if (!container) return;
  setMessage(
    container,
    `Sign-in could not be completed: ${error && error.message ? error.message : String(error)}.`,
    "error",
  );
}

function identitySubjectLabel(identity) {
  if (identity && identity.authenticated && identity.subject) return identity.subject;
  return "the current identity";
}

// deniedText describes an explicit denial and states plainly that it is a
// denial and not an empty result, so the two never read the same.
function deniedText(action, identity) {
  return `Access denied — ${identitySubjectLabel(identity)} is not authorized to ` +
    `${action}. This is a denial, not an empty result.`;
}

// emptyRetrievalText describes a genuine empty match while making the
// identity scope explicit, because the server filters unauthorized content
// silently. Whether context was withheld is stated separately by
// suppressionClause, so this text no longer claims nothing was withheld.
function emptyRetrievalText(identity) {
  return `No evidence matched for ${identitySubjectLabel(identity)}. ` +
    `Results are scoped to this identity; content you are not authorized to see ` +
    `is never included here.`;
}

function emptyDocumentsText(identity) {
  return `No documents are visible to ${identitySubjectLabel(identity)}. ` +
    `The corpus is shared and filtered per identity, so this may mean nothing ` +
    `was ingested or that nothing here is authorized for you.`;
}

// suppressionClause states, in plain language, whether authorization withheld
// context from this identity. It reveals only a count: the server discloses how
// many documents were withheld and never what they are. The zero case is stated
// explicitly so the three states — results with nothing withheld, results with
// some withheld, and nothing-but-withheld — never read the same. The wording is
// careful not to overclaim: a positive count is withheld context, never a claim
// that nothing exists, and it is scoped to authorization withholding, which is
// the only thing the server counts.
function suppressionClause(count, identity) {
  const withheld = Number(count) || 0;
  const who = identitySubjectLabel(identity);
  if (withheld <= 0) {
    return ` No documents were withheld from ${who} by authorization.`;
  }
  const noun = withheld === 1 ? "document" : "documents";
  const verb = withheld === 1 ? "is" : "are";
  return ` ${withheld} ${noun} ${verb} withheld from ${who} by authorization and ` +
    `not shown. This counts withheld context and is not a confirmation that ` +
    `nothing exists; nothing about the withheld ${noun} is disclosed.`;
}

// restrictionClause states, in plain language, whether the mosaic co-occurrence
// budget withheld results from this identity. It is deliberately distinct from
// suppressionClause: these are documents the identity IS authorized to read
// individually, withheld only because observing too many distinct sensitivity
// domains together is an inference risk. It reveals only a count and never which
// domains or documents were involved, so it cannot become an oracle for content
// the identity is not cleared to read. The zero case is stated explicitly so a
// restriction never reads the same as a plain authorization withholding.
function restrictionClause(count, identity) {
  const restricted = Number(count) || 0;
  const who = identitySubjectLabel(identity);
  if (restricted <= 0) {
    return "";
  }
  const noun = restricted === 1 ? "document" : "documents";
  const verb = restricted === 1 ? "is" : "are";
  return ` ${restricted} ${noun} ${verb} restricted for ${who} by the ` +
    `co-occurrence budget: you are authorized to read ${restricted === 1 ? "it" : "them"} ` +
    `individually, but ${restricted === 1 ? "it was" : "they were"} withheld to ` +
    `limit how many distinct sensitivity domains you observe together. This is a ` +
    `restriction, not a denial, and nothing about the withheld ${noun} is disclosed.`;
}

async function loadMeta() {
  const response = await fetch("/api/v1/meta", {headers: {"accept": "application/json", ...authHeaders()}});
  const value = await response.json();
  if (!response.ok) throw apiError(value, response);
  state.capabilities = {...state.capabilities, ...(value.capabilities || {})};
  state.uploadLimits = {
    max_upload_files: value.max_upload_files || 0,
    max_upload_file_bytes: value.max_upload_file_bytes || 0,
    max_upload_total_bytes: value.max_upload_total_bytes || 0,
  };
  applyCapabilities();
}

async function loadIdentity() {
  try {
    const response = await fetch("/api/v1/identity", {headers: {"accept": "application/json", ...authHeaders()}});
    const value = await response.json();
    if (!response.ok) throw apiError(value, response);
    state.identity = value;
    renderIdentity(value);
  } catch (error) {
    state.identity = null;
    renderIdentityError(error);
  }
}

function renderIdentity(identity) {
  const container = $("identity");
  const badge = $("identity-badge");
  container.className = "identity";
  if (!identity || !identity.authenticated) {
    container.classList.add("identity-anon");
    setMessage(
      container,
      "This workspace is serving requests without an established identity. " +
        "Results are not attributed to any principal.",
    );
    badge.hidden = true;
    badge.textContent = "";
    return;
  }

  badge.hidden = false;
  badge.textContent = `▲ ${identity.subject}`;
  badge.title = `Signed in as ${identity.subject}. Shared governed workspace — ` +
    "every result is scoped to this identity.";

  const fragment = document.createDocumentFragment();

  const who = document.createElement("div");
  who.className = "identity-who";
  const label = document.createElement("span");
  label.className = "identity-label";
  label.textContent = "You are";
  const subject = document.createElement("strong");
  subject.className = "identity-subject";
  subject.textContent = identity.subject;
  subject.title = identity.subject;
  who.append(label, subject);
  fragment.append(who);

  const shared = document.createElement("p");
  shared.className = "identity-shared";
  shared.textContent =
    "Shared governed workspace. Every result below is retrieved as this " +
    "identity, so what you see is exactly what you are authorized to see.";
  fragment.append(shared);

  fragment.append(identityChips("Authorized operations", identity.operations || []));

  const details = document.createElement("dl");
  details.className = "identity-facts";
  appendFact(details, "Actor", identity.actor);
  appendFact(details, "Domain", identity.authorization_domain);
  appendFact(details, "Policy gen", identity.policy_generation
    ? String(identity.policy_generation) : "");
  appendFact(details, "Session ends", formatExpiry(identity.authentication_expires));
  appendFact(details, "Purpose", identity.audit_purpose);
  appendFact(details, "Request", identity.request_id);
  if (details.children.length > 0) fragment.append(details);

  const note = document.createElement("p");
  note.className = "identity-note muted";
  note.textContent =
    "Identity is assigned by the server, not chosen here. This build " +
    "authenticates every request as one development principal over a loopback " +
    "listener, so there is no client-side identity switch.";
  fragment.append(note);

  container.replaceChildren(fragment);
}

function identityChips(legendText, values) {
  const group = document.createElement("div");
  group.className = "identity-chips";
  group.setAttribute("role", "group");
  group.setAttribute("aria-label", legendText);
  const legend = document.createElement("span");
  legend.className = "identity-label";
  legend.textContent = legendText;
  group.append(legend);
  if (values.length === 0) {
    const none = document.createElement("span");
    none.className = "chip chip-empty";
    none.textContent = "none";
    group.append(none);
    return group;
  }
  for (const value of values) {
    const chip = document.createElement("span");
    chip.className = "chip";
    chip.textContent = value;
    group.append(chip);
  }
  return group;
}

function appendFact(list, term, value) {
  if (!value) return;
  const dt = document.createElement("dt");
  dt.textContent = term;
  const dd = document.createElement("dd");
  dd.textContent = value;
  dd.title = value;
  list.append(dt, dd);
}

function formatExpiry(value) {
  if (!value) return "";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "";
  return date.toLocaleTimeString([], {hour: "2-digit", minute: "2-digit"});
}

function renderIdentityError(error) {
  const container = $("identity");
  if (needsReauthentication(error)) {
    handleReauthentication();
    container.className = "identity identity-unknown";
    setMessage(container, reauthenticationText(), "reauth");
    const badge = $("identity-badge");
    badge.hidden = true;
    badge.textContent = "";
    return;
  }
  container.className = "identity identity-unknown";
  setMessage(
    container,
    `Identity is unavailable: ${error && error.message ? error.message : String(error)}. ` +
      "Result sets below cannot be attributed to a principal.",
    "error",
  );
  const badge = $("identity-badge");
  badge.hidden = true;
  badge.textContent = "";
}

async function loadOntology() {
  const status = $("ontology-status");
  if (!status) return;
  const generation = ++ontologyGeneration;
  setStatus(status, "Loading active ontology…");
  try {
    const response = await fetch("/api/v1/ontology", {
      headers: {"accept": "application/json", ...authHeaders()},
    });
    const value = await response.json();
    if (!response.ok) throw apiError(value, response);
    let proposals = {proposals: [], limits: {}};
    let proposalsError = null;
    try {
      const proposalResponse = await fetch("/api/v1/ontology/proposals", {
        headers: {"accept": "application/json", ...authHeaders()},
      });
      const proposalValue = await proposalResponse.json();
      if (!proposalResponse.ok) throw apiError(proposalValue, proposalResponse);
      proposals = proposalValue;
      await loadProposalBlastRadii(proposals);
    } catch (error) {
      proposalsError = error;
    }
    if (generation !== ontologyGeneration) return;
    state.ontology = value;
    state.ontologyProposals = proposalsError ? null : proposals;
    renderOntology(value, proposals, proposalsError);
  } catch (error) {
    if (generation !== ontologyGeneration) return;
    state.ontology = null;
    state.ontologyProposals = null;
    renderOntologyError(error);
  }
}

async function loadProposalBlastRadii(proposalsResponse) {
  const proposals = (proposalsResponse && proposalsResponse.proposals) || [];
  await Promise.all(proposals.map(async (proposal) => {
    try {
      const response = await fetch(
        `/api/v1/ontology/proposals/${encodeURIComponent(proposal.id)}/blast-radius`,
        {headers: {"accept": "application/json", ...authHeaders()}},
      );
      const value = await response.json();
      if (!response.ok) throw apiError(value, response);
      // This assignment is load-bearing; TestStaticWorkspaceOntologyProposalBlastRadius
      // pins that reviewers see the server-computed blast radius before using
      // approval or publish controls.
      proposal.blast_radius = value.blast_radius || value;
    } catch (error) {
      proposal.blast_radius_error = error;
    }
  }));
}

function renderOntology(ontology, proposals = {proposals: [], limits: {}}, proposalsError = null) {
  const status = $("ontology-status");
  const details = $("ontology-details");
  if (!status || !details) return;
  details.replaceChildren();
  if (!ontology || !ontology.configured) {
    setStatus(
      status,
      "No active ontology is configured. This is an explicit unconfigured state, " +
        "not an empty ontology and not a loading failure.",
      "empty-state",
    );
    return;
  }

  const concepts = ontology.concepts || [];
  const relationships = ontology.relationships || [];
  const properties = ontology.properties || [];
  setStatus(
    status,
    `Active ontology ${shortLabel(ontology.identity && ontology.identity.schema_id)} / ` +
      `${shortLabel(ontology.identity && ontology.identity.version_id)}: ` +
      `${concepts.length} concept(s), ${relationships.length} relationship(s), ` +
      `${properties.length} property definition(s).`,
    "muted",
  );

  const conceptMap = new Map(concepts.map((concept) => [concept.id, concept]));
  const propertyMap = new Map(properties.map((property) => [property.id, property]));
  const fragment = document.createDocumentFragment();
  fragment.append(ontologyIdentityFacts(ontology));
  fragment.append(ontologySection(
    "Declared joins",
    relationships,
    "This active ontology declares no relationships. It is configured, but it has no joins yet.",
    (relationship) => relationshipCard(relationship, conceptMap, propertyMap),
  ));
  fragment.append(ontologySection(
    "Concepts",
    concepts,
    "This active ontology has no concepts.",
    (concept) => conceptCard(concept, propertyMap),
  ));
  fragment.append(ontologySection(
    "Properties and constraints",
    properties,
    "This active ontology has no properties or constraints.",
    propertyCard,
  ));
  fragment.append(ontologyProposalPanel(ontology, proposals, proposalsError));
  details.replaceChildren(fragment);
}

function ontologyProposalPanel(ontology, proposalsResponse, proposalsError) {
  const section = document.createElement("section");
  section.className = "ontology-section ontology-governance";
  const heading = document.createElement("h3");
  heading.textContent = "Governed proposals";
  section.append(heading, ontologyProposalForm(ontology));

  const status = document.createElement("p");
  status.setAttribute("role", "status");
  if (proposalsError) {
    if (needsReauthentication(proposalsError)) {
      handleReauthentication();
      status.className = "reauth";
      status.textContent = reauthenticationText();
    } else if (isDenied(proposalsError)) {
      status.className = "denied";
      status.textContent = deniedText("review ontology proposals", state.identity);
    } else {
      status.className = "error";
      status.textContent = `Ontology proposals are unavailable: ${proposalsError.message || String(proposalsError)}.`;
    }
    section.append(status);
    return section;
  }

  const proposals = (proposalsResponse && proposalsResponse.proposals) || [];
  status.className = proposals.length === 0 ? "empty-state" : "muted";
  status.textContent = proposals.length === 0
    ? "No governed ontology proposals exist. This is an empty proposal list, not an authorization denial."
    : `Showing ${proposals.length} governed ontology proposal(s).`;
  section.append(status);
  if (proposals.length === 0) return section;

  const list = document.createElement("div");
  list.className = "ontology-list";
  for (const proposal of proposals) list.append(ontologyProposalCard(proposal));
  section.append(list);
  return section;
}

function ontologyProposalForm(ontology) {
  const form = document.createElement("form");
  form.className = "ontology-proposal-form";

  const limits = (ontology && ontology.limits) || {};
  // This active-ontology draft seed is load-bearing; TestStaticWorkspaceOntologyProposalEditorPrepopulatesActiveOntologyOnSubmit
  // pins that opening the editor and changing one field still submits every
  // currently active concept, relationship, property, and constraint.
  const editor = {
    draft: ontologyDraftFromActive(
      ontology,
      proposalVersionSuffix(ontology && ontology.version && ontology.version.version),
    ),
    errors: {},
  };

  const versionLabel = document.createElement("label");
  versionLabel.textContent = "Proposed version";
  const version = document.createElement("input");
  version.className = "ontology-proposal-version";
  version.value = editor.draft.version;
  version.oninput = () => {
    editor.draft.version = version.value;
    refreshDraftJSON(draft, editor.draft);
  };
  versionLabel.append(version);

  const rationaleLabel = document.createElement("label");
  rationaleLabel.textContent = "Rationale";
  const rationale = document.createElement("textarea");
  rationale.placeholder = "Why should this ontology refinement be considered?";
  rationaleLabel.append(rationale);

  const modeControls = document.createElement("div");
  modeControls.className = "ontology-proposal-modes";
  const formMode = document.createElement("button");
  formMode.type = "button";
  formMode.textContent = "Form editor";
  const rawMode = document.createElement("button");
  rawMode.type = "button";
  rawMode.textContent = "Raw JSON";
  modeControls.append(formMode, rawMode);

  const builder = document.createElement("div");
  builder.className = "ontology-proposal-builder";

  const draftLabel = document.createElement("label");
  draftLabel.textContent = "Proposed ontology JSON";
  draftLabel.hidden = true;
  const draft = document.createElement("textarea");
  draft.className = "ontology-proposal-json";
  refreshDraftJSON(draft, editor.draft);
  draft.oninput = () => {
    try {
      const parsed = parseProposalDraft(draft.value, limits);
      editor.draft = parsed;
      version.value = parsed.version || "";
      setStatus(status, "Raw JSON is valid and ready to sync back to the form.", "muted");
    } catch (error) {
      setStatus(status, `Raw proposal JSON is invalid: ${error.message || String(error)}.`, "error");
    }
  };
  draftLabel.append(draft);

  const submit = document.createElement("button");
  submit.type = "submit";
  submit.textContent = "Create draft proposal";
  const status = document.createElement("p");
  status.setAttribute("role", "status");
  status.className = "muted";
  status.textContent =
    "Draft is pre-populated from the active ontology. Use Remove controls for deliberate deletions.";

  formMode.onclick = () => {
    try {
      // This raw-to-form parse is load-bearing; TestStaticWorkspaceOntologyProposalEditorRoundTripsRawAndForms
      // pins that power-user JSON edits are rendered back into per-element
      // forms without losing optional fields.
      editor.draft = parseProposalDraft(draft.value, limits);
      version.value = editor.draft.version || "";
      renderProposalBuilder(builder, editor, draft, status, limits);
      builder.hidden = false;
      draftLabel.hidden = true;
      formMode.disabled = true;
      rawMode.disabled = false;
      setStatus(status, "Form editor synced from raw JSON.", "muted");
    } catch (error) {
      setStatus(status, `Raw proposal JSON is invalid: ${error.message || String(error)}.`, "error");
    }
  };
  rawMode.onclick = () => {
    const error = proposalEditorError(editor);
    if (error) {
      setStatus(status, error, "error");
      return;
    }
    try {
      validateProposalDraftBounds(editor.draft, limits);
      refreshDraftJSON(draft, editor.draft);
      builder.hidden = true;
      draftLabel.hidden = false;
      formMode.disabled = false;
      rawMode.disabled = true;
      setStatus(status, "Raw JSON view is synced from the per-element forms.", "muted");
    } catch (error) {
      setStatus(status, `Proposal exceeds ontology bounds: ${error.message || String(error)}.`, "error");
    }
  };

  form.onsubmit = async (event) => {
    event.preventDefault();
    if (!draftLabel.hidden) {
      try {
        editor.draft = parseProposalDraft(draft.value, limits);
      } catch (error) {
        setStatus(status, `Proposal JSON is invalid: ${error.message || String(error)}.`, "error");
        return;
      }
    }
    const error = proposalEditorError(editor);
    if (error) {
      setStatus(status, error, "error");
      return;
    }
    try {
      // This bound check is load-bearing; TestStaticWorkspaceOntologyProposalEditorBoundsErrorDoesNotTruncate
      // pins that the UI reports over-limit drafts instead of silently
      // dropping excess elements before POSTing the proposal.
      validateProposalDraftBounds(editor.draft, limits);
    } catch (error) {
      setStatus(status, `Proposal exceeds ontology bounds: ${error.message || String(error)}.`, "error");
      return;
    }
    await createOntologyProposal(version, rationale, editor.draft, status, submit);
  };
  renderProposalBuilder(builder, editor, draft, status, limits);
  formMode.disabled = true;
  form.append(versionLabel, rationaleLabel, modeControls, builder, draftLabel, submit, status);
  return form;
}

function renderProposalBuilder(container, editor, raw, status, limits) {
  editor.errors = {};
  container.replaceChildren(
    ontologyProposalElementSection(
      "Properties",
      "property",
      editor.draft.properties,
      "No properties. Removing all properties is allowed only when it is visible here.",
      (property, index) => propertyDraftEditor(property, index, editor, raw, status),
      () => {
        editor.draft.properties.push({
          key: "", name: "", description: "", value_type: "string", constraints: [],
        });
      },
      container,
      editor,
      raw,
      status,
      limits,
    ),
    ontologyProposalElementSection(
      "Concepts",
      "concept",
      editor.draft.concepts,
      "No concepts. Removing all concepts is allowed only when it is visible here.",
      (concept, index) => conceptDraftEditor(concept, index, editor, raw),
      () => {
        editor.draft.concepts.push({key: "", name: "", description: "", properties: []});
      },
      container,
      editor,
      raw,
      status,
      limits,
    ),
    ontologyProposalElementSection(
      "Relationships",
      "relationship",
      editor.draft.relationships,
      "No relationships. Removing all relationships is allowed only when it is visible here.",
      (relationship, index) => relationshipDraftEditor(relationship, index, editor, raw),
      () => {
        editor.draft.relationships.push({
          key: "", name: "", description: "", from_concepts: [], to_concepts: [],
          properties: [], directed: true,
        });
      },
      container,
      editor,
      raw,
      status,
      limits,
    ),
  );
}

function ontologyProposalElementSection(
  title,
  kind,
  values,
  emptyText,
  renderItem,
  addItem,
  container,
  editor,
  raw,
  status,
  limits,
) {
  const section = document.createElement("section");
  section.className = "ontology-proposal-elements";
  const heading = document.createElement("h4");
  heading.textContent = `${title} (${values.length})`;
  const add = document.createElement("button");
  add.type = "button";
  add.textContent = `Add ${kind}`;
  add.onclick = () => {
    addItem();
    refreshDraftJSON(raw, editor.draft);
    renderProposalBuilder(container, editor, raw, status, limits);
  };
  section.append(heading, add);
  if (values.length === 0) {
    const empty = document.createElement("p");
    empty.className = "empty-state";
    empty.textContent = emptyText;
    section.append(empty);
    return section;
  }
  const list = document.createElement("div");
  list.className = "ontology-proposal-element-list";
  values.forEach((value, index) => {
    const card = renderItem(value, index);
    // This explicit per-element remove control is load-bearing; TestStaticWorkspaceOntologyProposalEditorPrepopulatesActiveOntologyOnSubmit
    // pins that deletion is visible as an action rather than an accidental
    // omission from the generated draft.
    const remove = document.createElement("button");
    remove.type = "button";
    remove.className = `ontology-remove-${kind}`;
    remove.textContent = `Remove ${kind}`;
    remove.onclick = () => {
      values.splice(index, 1);
      refreshDraftJSON(raw, editor.draft);
      renderProposalBuilder(container, editor, raw, status, limits);
      setStatus(status, `${kind} removed from draft. This deletion will appear in blast radius after creation.`, "withheld");
    };
    card.append(remove);
    list.append(card);
  });
  section.append(list);
  return section;
}

function propertyDraftEditor(property, index, editor, raw, status) {
  const card = proposalDraftCard("property", property.key, index);
  card.append(
    proposalInput("ontology-proposal-property-key", "Key", property.key, (value) => {
      property.key = value;
      refreshDraftJSON(raw, editor.draft);
    }),
    proposalInput("ontology-proposal-property-name", "Name", property.name, (value) => {
      property.name = value;
      refreshDraftJSON(raw, editor.draft);
    }),
    proposalTextarea(
      "ontology-proposal-property-description",
      "Description",
      property.description || "",
      (value) => {
        property.description = value;
        refreshDraftJSON(raw, editor.draft);
      },
    ),
    proposalInput(
      "ontology-proposal-property-value-type",
      "Value type",
      property.value_type || "string",
      (value) => {
        property.value_type = value;
        refreshDraftJSON(raw, editor.draft);
      },
      "string, integer, number, boolean, timestamp, or reference",
    ),
    proposalTextarea(
      "ontology-proposal-property-constraints",
      "Constraints JSON",
      JSON.stringify(property.constraints || [], null, 2),
      (value) => {
        try {
          const constraints = JSON.parse(value || "[]");
          if (!Array.isArray(constraints)) throw new Error("constraints must be an array");
          property.constraints = constraints;
          setProposalEditorError(editor, `property-${index}-constraints`, "");
          refreshDraftJSON(raw, editor.draft);
        } catch (error) {
          setProposalEditorError(
            editor,
            `property-${index}-constraints`,
            `Property ${property.key || index + 1} constraints JSON is invalid: ${error.message || String(error)}.`,
          );
          setStatus(status, proposalEditorError(editor), "error");
        }
      },
    ),
  );
  return card;
}

function conceptDraftEditor(concept, index, editor, raw) {
  const card = proposalDraftCard("concept", concept.key, index);
  card.append(
    proposalInput("ontology-proposal-concept-key", "Key", concept.key, (value) => {
      concept.key = value;
      refreshDraftJSON(raw, editor.draft);
    }),
    proposalInput("ontology-proposal-concept-name", "Name", concept.name, (value) => {
      concept.name = value;
      refreshDraftJSON(raw, editor.draft);
    }),
    proposalTextarea(
      "ontology-proposal-concept-description",
      "Description",
      concept.description || "",
      (value) => {
        concept.description = value;
        refreshDraftJSON(raw, editor.draft);
      },
    ),
    proposalTextarea(
      "ontology-proposal-concept-properties",
      "Property keys",
      keyListText(concept.properties),
      (value) => {
        concept.properties = parseKeyList(value);
        refreshDraftJSON(raw, editor.draft);
      },
    ),
  );
  return card;
}

function relationshipDraftEditor(relationship, index, editor, raw) {
  const card = proposalDraftCard("relationship", relationship.key, index);
  card.append(
    proposalInput("ontology-proposal-relationship-key", "Key", relationship.key, (value) => {
      relationship.key = value;
      refreshDraftJSON(raw, editor.draft);
    }),
    proposalInput("ontology-proposal-relationship-name", "Name", relationship.name, (value) => {
      relationship.name = value;
      refreshDraftJSON(raw, editor.draft);
    }),
    proposalTextarea(
      "ontology-proposal-relationship-description",
      "Description",
      relationship.description || "",
      (value) => {
        relationship.description = value;
        refreshDraftJSON(raw, editor.draft);
      },
    ),
    proposalTextarea(
      "ontology-proposal-relationship-from",
      "From concept keys",
      keyListText(relationship.from_concepts),
      (value) => {
        relationship.from_concepts = parseKeyList(value);
        refreshDraftJSON(raw, editor.draft);
      },
    ),
    proposalTextarea(
      "ontology-proposal-relationship-to",
      "To concept keys",
      keyListText(relationship.to_concepts),
      (value) => {
        relationship.to_concepts = parseKeyList(value);
        refreshDraftJSON(raw, editor.draft);
      },
    ),
    proposalTextarea(
      "ontology-proposal-relationship-properties",
      "Property keys",
      keyListText(relationship.properties),
      (value) => {
        relationship.properties = parseKeyList(value);
        refreshDraftJSON(raw, editor.draft);
      },
    ),
    proposalCheckbox("ontology-proposal-relationship-directed", "Directed", relationship.directed === true, (value) => {
      relationship.directed = value;
      refreshDraftJSON(raw, editor.draft);
    }),
  );
  return card;
}

function proposalDraftCard(kind, key, index) {
  const card = document.createElement("article");
  card.className = `ontology-proposal-element ontology-proposal-${kind}`;
  const heading = document.createElement("h5");
  heading.textContent = `${kind} ${key || index + 1}`;
  card.append(heading);
  return card;
}

function proposalInput(className, labelText, value, oninput, placeholder = "") {
  const label = document.createElement("label");
  label.textContent = labelText;
  const input = document.createElement("input");
  input.className = className;
  input.value = value || "";
  input.placeholder = placeholder;
  input.oninput = () => oninput(input.value);
  label.append(input);
  return label;
}

function proposalTextarea(className, labelText, value, oninput) {
  const label = document.createElement("label");
  label.textContent = labelText;
  const input = document.createElement("textarea");
  input.className = className;
  input.value = value || "";
  input.oninput = () => oninput(input.value);
  label.append(input);
  return label;
}

function proposalCheckbox(className, labelText, checked, onchange) {
  const label = document.createElement("label");
  label.className = "ontology-proposal-checkbox";
  const input = document.createElement("input");
  input.className = className;
  input.type = "checkbox";
  input.checked = checked;
  input.onchange = () => onchange(input.checked);
  label.append(input);
  const text = document.createElement("span");
  text.textContent = labelText;
  label.append(text);
  return label;
}

function setProposalEditorError(editor, key, message) {
  if (message) editor.errors[key] = message;
  else delete editor.errors[key];
}

function proposalEditorError(editor) {
  const keys = Object.keys(editor.errors).sort();
  return keys.length === 0 ? "" : editor.errors[keys[0]];
}

function keyListText(values) {
  return (values || []).join(", ");
}

function parseKeyList(value) {
  return String(value || "")
    .split(/[,\n]+/)
    .map((item) => item.trim())
    .filter(Boolean);
}

function refreshDraftJSON(raw, draft) {
  raw.value = JSON.stringify(draft, null, 2);
}

function parseProposalDraft(text, limits) {
  const draft = JSON.parse(text);
  normalizeProposalDraft(draft);
  return draft;
}

function normalizeProposalDraft(draft) {
  if (!draft || typeof draft !== "object" || Array.isArray(draft)) {
    throw new Error("proposal draft must be an object");
  }
  draft.version = String(draft.version || "");
  draft.properties = Array.isArray(draft.properties) ? draft.properties : [];
  draft.concepts = Array.isArray(draft.concepts) ? draft.concepts : [];
  draft.relationships = Array.isArray(draft.relationships) ? draft.relationships : [];
  for (const property of draft.properties) {
    property.constraints = Array.isArray(property.constraints) ? property.constraints : [];
  }
  for (const concept of draft.concepts) {
    concept.properties = Array.isArray(concept.properties) ? concept.properties : [];
  }
  for (const relationship of draft.relationships) {
    relationship.from_concepts = Array.isArray(relationship.from_concepts) ? relationship.from_concepts : [];
    relationship.to_concepts = Array.isArray(relationship.to_concepts) ? relationship.to_concepts : [];
    relationship.properties = Array.isArray(relationship.properties) ? relationship.properties : [];
    relationship.directed = relationship.directed === true;
  }
}

function validateProposalDraftBounds(draft, limits) {
  const bound = (name) => Number(limits && limits[name] || 0);
  enforceDraftBound("concept", (draft.concepts || []).length, bound("max_concepts"));
  enforceDraftBound("relationship", (draft.relationships || []).length, bound("max_relationships"));
  enforceDraftBound("property", (draft.properties || []).length, bound("max_properties"));
  for (const concept of draft.concepts || []) {
    enforceDraftBound(
      `concept ${concept.key || ""} property`,
      (concept.properties || []).length,
      bound("max_definition_properties"),
    );
  }
  for (const relationship of draft.relationships || []) {
    enforceDraftBound(
      `relationship ${relationship.key || ""} from concept`,
      (relationship.from_concepts || []).length,
      bound("max_relationship_endpoint_sets"),
    );
    enforceDraftBound(
      `relationship ${relationship.key || ""} to concept`,
      (relationship.to_concepts || []).length,
      bound("max_relationship_endpoint_sets"),
    );
    enforceDraftBound(
      `relationship ${relationship.key || ""} property`,
      (relationship.properties || []).length,
      bound("max_definition_properties"),
    );
  }
  for (const property of draft.properties || []) {
    enforceDraftBound(
      `property ${property.key || ""} constraint`,
      (property.constraints || []).length,
      bound("max_constraints_per_property"),
    );
    for (const constraint of property.constraints || []) {
      enforceDraftBound(
        `property ${property.key || ""} allowed value`,
        (constraint.allowed_values || []).length,
        bound("max_allowed_values"),
      );
    }
  }
}

function enforceDraftBound(label, count, limit) {
  if (limit > 0 && count > limit) {
    throw new Error(`${label} count ${count} exceeds limit ${limit}`);
  }
}

function ontologyProposalCard(proposal) {
  const card = document.createElement("article");
  card.className = "ontology-card ontology-proposal-card";
  const heading = document.createElement("h4");
  heading.textContent = `${proposal.state || "unknown"} proposal`;
  heading.title = proposal.id || "";
  const summary = document.createElement("p");
  const proposed = proposal.proposed_ontology || {};
  summary.className = "muted";
  summary.textContent = `Proposes ${shortLabel(proposed.identity && proposed.identity.version_id)} ` +
    `by ${proposal.proposed_by || "unknown"}: ${proposal.rationale || "no rationale"}`;
  card.append(heading, summary);

  const facts = document.createElement("dl");
  facts.className = "identity-facts";
  appendFact(facts, "Base version", proposal.base_version_id);
  appendFact(facts, "Created", proposal.created_at);
  appendFact(facts, "Updated", proposal.updated_at);
  card.append(facts);

  const transitions = proposal.transitions || [];
  const history = document.createElement("ol");
  history.className = "ontology-transitions";
  if (transitions.length === 0) {
    const item = document.createElement("li");
    item.textContent = "Draft created; no governance transitions recorded yet.";
    history.append(item);
  } else {
    for (const transition of transitions) {
      const item = document.createElement("li");
      item.textContent = `${transition.from} → ${transition.to} by ` +
        `${transition.actor}: ${transition.note}`;
      history.append(item);
    }
  }
  card.append(ontologyBlastRadiusPanel(proposal), history, ontologyProposalActions(proposal));
  return card;
}

function ontologyBlastRadiusPanel(proposal) {
  const section = document.createElement("section");
  section.className = "ontology-blast-radius";
  const heading = document.createElement("h5");
  heading.textContent = "Blast radius";
  section.append(heading);
  if (proposal.blast_radius_error) {
    const error = proposal.blast_radius_error;
    const status = document.createElement("p");
    if (needsReauthentication(error)) {
      handleReauthentication();
      status.className = "reauth";
      status.textContent = reauthenticationText();
    } else if (isDenied(error)) {
      status.className = "denied";
      status.textContent = deniedText("review ontology proposal blast radius", state.identity);
    } else {
      status.className = "error";
      status.textContent = `Blast radius unavailable: ${error.message || String(error)}.`;
    }
    section.append(status);
    return section;
  }
  const blast = proposal.blast_radius;
  if (!blast) {
    const missing = document.createElement("p");
    missing.className = "muted";
    missing.textContent = "Blast radius was not loaded.";
    section.append(missing);
    return section;
  }
  const summary = blast.summary || {};
  const overview = document.createElement("p");
  overview.className = (summary.destructive_changes || 0) > 0 ? "withheld" : "muted";
  overview.textContent = `Blast radius: ${summary.destructive_changes || 0} destructive ` +
    `change(s), ${summary.additive_changes || 0} additive change(s).`;
  section.append(overview);
  const groups = [
    ["Removed concepts", blast.removed_concepts || [], (item) =>
      `${blastConceptName(item)} removed`],
    ["Removed relationships", blast.removed_relationships || [], (item) =>
      `${blastRelationName(item)} removed`],
    ["Removed properties", blast.removed_properties || [], (item) =>
      `${blastPropertyName(item)} removed`],
    ["Changed concepts", blast.changed_concepts || [], (item) =>
      `${blastConceptName(item.before)} changed ${fieldList(item.fields)}`],
    ["Changed relationships", blast.changed_relationships || [], (item) =>
      `${blastRelationName(item.before)} changed ${fieldList(item.fields)}`],
    ["Changed properties", blast.changed_properties || [], (item) =>
      `${blastPropertyName(item.before)} changed ${fieldList(item.fields)}`],
    ["Added concepts", blast.added_concepts || [], (item) =>
      `${blastConceptName(item)} added`],
    ["Added relationships", blast.added_relationships || [], (item) =>
      `${blastRelationName(item)} added`],
    ["Added properties", blast.added_properties || [], (item) =>
      `${blastPropertyName(item)} added`],
  ];
  let rendered = false;
  for (const [title, values, render] of groups) {
    if (values.length === 0) continue;
    rendered = true;
    section.append(blastRadiusGroup(title, values, render));
  }
  if (!rendered) {
    const none = document.createElement("p");
    none.className = "muted";
    none.textContent = "No structural ontology changes detected.";
    section.append(none);
  }
  return section;
}

function blastRadiusGroup(title, values, render) {
  const details = document.createElement("details");
  details.open = true;
  const summary = document.createElement("summary");
  summary.textContent = `${title} (${values.length})`;
  const list = document.createElement("ul");
  list.className = "ontology-blast-list";
  for (const value of values) {
    const item = document.createElement("li");
    item.textContent = render(value);
    list.append(item);
  }
  details.append(summary, list);
  return details;
}

function fieldList(fields) {
  return (fields || []).length > 0 ? (fields || []).join(", ") : "definition";
}

function blastConceptName(concept) {
  return blastDefinitionName(concept, "concept");
}

function blastRelationName(relationship) {
  return blastDefinitionName(relationship, "relationship");
}

function blastPropertyName(property) {
  return blastDefinitionName(property, "property");
}

function blastDefinitionName(value, fallback) {
  if (!value) return fallback;
  return `${value.name || value.key || fallback} (${value.key || shortLabel(value.id)})`;
}

function ontologyProposalActions(proposal) {
  const group = document.createElement("div");
  group.className = "ontology-proposal-actions";
  const note = document.createElement("input");
  note.placeholder = "Transition note";
  group.append(note);
  const actions = proposalActionsForState(proposal.state);
  for (const action of actions) {
    const button = document.createElement("button");
    button.type = "button";
    button.textContent = action.label;
    button.onclick = async () => {
      await transitionOntologyProposal(
        proposal.id,
        action.state,
        note.value || `Ontology proposal ${action.state} from Explorer UI.`,
        group,
      );
    };
    group.append(button);
  }
  if (actions.length === 0) {
    const done = document.createElement("span");
    done.className = "muted";
    done.textContent = "Terminal state.";
    group.append(done);
  }
  return group;
}

function proposalActionsForState(stateName) {
  switch (stateName) {
  case "draft":
    return [{state: "submitted", label: "Submit"}, {state: "withdrawn", label: "Withdraw"}];
  case "submitted":
    return [
      {state: "approved", label: "Approve"},
      {state: "rejected", label: "Reject"},
      {state: "withdrawn", label: "Withdraw"},
    ];
  case "approved":
    return [{state: "published", label: "Publish"}, {state: "withdrawn", label: "Withdraw"}];
  default:
    return [];
  }
}

async function createOntologyProposal(version, rationale, draft, status, button) {
  let proposed;
  try {
    proposed = draft && typeof draft.value === "string"
      ? JSON.parse(draft.value)
      : JSON.parse(JSON.stringify(draft));
  } catch (error) {
    setStatus(status, `Proposal JSON is invalid: ${error.message || String(error)}.`, "error");
    return;
  }
  proposed.version = version.value;
  button.disabled = true;
  setStatus(status, "Creating draft proposal…");
  try {
    const response = await fetch("/api/v1/ontology/proposals", {
      method: "POST",
      headers: {
        "accept": "application/json",
        "content-type": "application/json",
        "x-shoal-workspace-request": "1",
        ...authHeaders(),
      },
      body: JSON.stringify({rationale: rationale.value, proposed_version: proposed}),
    });
    const value = await response.json();
    if (!response.ok) throw apiError(value, response);
    setStatus(status, "Draft proposal created.");
    await loadOntology();
  } catch (error) {
    showActionError(status, error, "create ontology proposals");
  } finally {
    button.disabled = false;
  }
}

async function transitionOntologyProposal(proposalID, nextState, note, container) {
  setMessage(container, `Moving proposal to ${nextState}…`);
  try {
    const response = await fetch(`/api/v1/ontology/proposals/${encodeURIComponent(proposalID)}/transition`, {
      method: "POST",
      headers: {
        "accept": "application/json",
        "content-type": "application/json",
        "x-shoal-workspace-request": "1",
        ...authHeaders(),
      },
      body: JSON.stringify({state: nextState, note}),
    });
    const value = await response.json();
    if (!response.ok) throw apiError(value, response);
    await loadOntology();
  } catch (error) {
    showActionError(container, error, `move ontology proposal to ${nextState}`);
  }
}

function ontologyDraftFromActive(ontology, version) {
  const properties = ontology.properties || [];
  const concepts = ontology.concepts || [];
  const relationships = ontology.relationships || [];
  const propertyKeys = new Map(properties.map((property) => [property.id, property.key]));
  const conceptKeys = new Map(concepts.map((concept) => [concept.id, concept.key]));
  return {
    version,
    properties: properties.map((property) => ({
      key: property.key,
      name: property.name,
      description: property.description || "",
      value_type: property.value_type,
      constraints: (property.constraints || []).map(ontologyConstraintDraft),
    })),
    concepts: concepts.map((concept) => ({
      key: concept.key,
      name: concept.name,
      description: concept.description || "",
      properties: (concept.properties || []).map((id) => propertyKeys.get(id) || id),
    })),
    relationships: relationships.map((relationship) => ({
      key: relationship.key,
      name: relationship.name,
      description: relationship.description || "",
      from_concepts: (relationship.from_concepts || []).map((id) => conceptKeys.get(id) || id),
      to_concepts: (relationship.to_concepts || []).map((id) => conceptKeys.get(id) || id),
      properties: (relationship.properties || []).map((id) => propertyKeys.get(id) || id),
      directed: relationship.directed === true,
    })),
  };
}

function ontologyConstraintDraft(constraint) {
  const draft = {kind: constraint.kind};
  if (Object.prototype.hasOwnProperty.call(constraint, "count")) draft.count = constraint.count;
  if (constraint.value) draft.value = constraint.value;
  if (constraint.pattern) draft.pattern = constraint.pattern;
  if ((constraint.allowed_values || []).length > 0) {
    draft.allowed_values = constraint.allowed_values;
  }
  return draft;
}

function proposalVersionSuffix(base) {
  const value = String(base || "").trim();
  const match = value.match(/^v(\d+)$/i);
  if (match) return `v${Number(match[1]) + 1}`;
  return value ? `${value}-proposal` : "proposal";
}

function ontologyIdentityFacts(ontology) {
  const section = document.createElement("section");
  section.className = "ontology-identity";
  const heading = document.createElement("h3");
  heading.textContent = "Active identity";
  const facts = document.createElement("dl");
  facts.className = "identity-facts";
  appendFact(facts, "Schema ID", ontology.identity && ontology.identity.schema_id);
  appendFact(facts, "Version ID", ontology.identity && ontology.identity.version_id);
  appendFact(facts, "Reading", ontology.identity && ontology.identity.reading);
  appendFact(facts, "Schema", ontology.schema && ontology.schema.name);
  appendFact(facts, "Version", ontology.version && ontology.version.version);
  appendFact(facts, "Created", ontology.version && ontology.version.created_at);
  section.append(heading, facts);
  return section;
}

function ontologySection(title, values, emptyText, renderItem) {
  const section = document.createElement("section");
  section.className = "ontology-section";
  const heading = document.createElement("h3");
  heading.textContent = title;
  section.append(heading);
  if (values.length === 0) {
    const empty = document.createElement("p");
    empty.className = "empty-state";
    empty.textContent = emptyText;
    section.append(empty);
    return section;
  }
  const list = document.createElement("div");
  list.className = "ontology-list";
  for (const value of values) list.append(renderItem(value));
  section.append(list);
  return section;
}

function relationshipCard(relationship, conceptMap, propertyMap) {
  const card = ontologyCard(relationship);
  const join = document.createElement("p");
  join.className = "ontology-join";
  const arrow = relationship.directed ? "→" : "↔";
  join.textContent = `${conceptNames(relationship.from_concepts, conceptMap)} ` +
    `${arrow} ${conceptNames(relationship.to_concepts, conceptMap)}`;
  card.append(join);
  const direction = document.createElement("p");
  direction.className = "muted";
  direction.textContent = relationship.directed
    ? "Directed relationship: source concepts join to target concepts."
    : "Undirected relationship: either endpoint set may be read from the other.";
  card.append(direction);
  appendPropertyChips(card, relationship.properties || [], propertyMap);
  return card;
}

function conceptCard(concept, propertyMap) {
  const card = ontologyCard(concept);
  appendPropertyChips(card, concept.properties || [], propertyMap);
  return card;
}

function propertyCard(property) {
  const card = ontologyCard(property);
  const type = document.createElement("p");
  type.className = "muted";
  type.textContent = `Value type: ${property.value_type || "unknown"}`;
  card.append(type);
  const constraints = property.constraints || [];
  if (constraints.length === 0) {
    const none = document.createElement("p");
    none.className = "muted";
    none.textContent = "No constraints.";
    card.append(none);
    return card;
  }
  const list = document.createElement("ul");
  list.className = "ontology-constraints";
  for (const constraint of constraints) {
    const item = document.createElement("li");
    item.textContent = constraintText(constraint);
    list.append(item);
  }
  card.append(list);
  return card;
}

function ontologyCard(value) {
  const card = document.createElement("article");
  card.className = "ontology-card";
  const heading = document.createElement("h4");
  heading.textContent = value.name || value.key || shortLabel(value.id);
  heading.title = value.id || "";
  card.append(heading);
  if (value.description) {
    const description = document.createElement("p");
    description.className = "muted";
    description.textContent = value.description;
    card.append(description);
  }
  const id = document.createElement("code");
  id.textContent = value.id || "";
  id.title = value.id || "";
  card.append(id);
  return card;
}

function appendPropertyChips(card, propertyIDs, propertyMap) {
  const group = document.createElement("div");
  group.className = "identity-chips";
  const label = document.createElement("span");
  label.className = "identity-label";
  label.textContent = "Properties";
  group.append(label);
  if (propertyIDs.length === 0) {
    const none = document.createElement("span");
    none.className = "chip chip-empty";
    none.textContent = "none";
    group.append(none);
  } else {
    for (const id of propertyIDs) {
      const chip = document.createElement("span");
      chip.className = "chip";
      const property = propertyMap.get(id);
      chip.textContent = property ? property.name : shortLabel(id);
      chip.title = id;
      group.append(chip);
    }
  }
  card.append(group);
}

function conceptNames(ids, conceptMap) {
  const names = (ids || []).map((id) => {
    const concept = conceptMap.get(id);
    return concept ? concept.name : shortLabel(id);
  });
  return names.length === 0 ? "(none)" : names.join(", ");
}

function constraintText(constraint) {
  const kind = constraint.kind || "constraint";
  if (Object.prototype.hasOwnProperty.call(constraint, "count")) {
    return `${kind}: ${constraint.count}`;
  }
  if (constraint.value) {
    return `${kind}: ${formatOntologyValue(constraint.value)}`;
  }
  if (constraint.pattern) {
    return `${kind}: ${constraint.pattern}`;
  }
  if ((constraint.allowed_values || []).length > 0) {
    return `${kind}: ${(constraint.allowed_values || []).map(formatOntologyValue).join(", ")}`;
  }
  return kind;
}

function formatOntologyValue(value) {
  if (!value) return "";
  return `${value.type || "value"} ${String(value.value)}`;
}

function renderOntologyError(error) {
  const status = $("ontology-status");
  const details = $("ontology-details");
  if (!status || !details) return;
  details.replaceChildren();
  if (needsReauthentication(error)) {
    handleReauthentication();
    setStatus(status, reauthenticationText(), "reauth");
    return;
  }
  if (isDenied(error)) {
    setStatus(status, deniedText("describe the active ontology", state.identity), "denied");
    return;
  }
  setStatus(
    status,
    `Ontology is unavailable: ${error && error.message ? error.message : String(error)}.`,
    "error",
  );
}

function capability(name) {
  return state.capabilities[name] === true;
}

function vectorRetrievalCapability() {
  return capability("vector_retrieval");
}

function applyCapabilities() {
  const canRetrieve = capability("retrieve");
  $("query").disabled = !canRetrieve;
  $("search-button").disabled = !canRetrieve;
  $("modes").disabled = !canRetrieve;
  applyVectorCapability(canRetrieve);

  const evidenceStatus = $("evidence-status");
  if (!canRetrieve) {
    evidenceStatus.dataset.capabilityPlaceholder = "retrieve";
    setStatus(evidenceStatus, "Retrieval is unavailable from this Explorer service.");
    $("evidence-results").replaceChildren();
  } else if (evidenceStatus.dataset.capabilityPlaceholder === "retrieve") {
    delete evidenceStatus.dataset.capabilityPlaceholder;
    setStatus(evidenceStatus, "Run a retrieval query to inspect exact evidence and explanations.");
  }

  const canNeighborhood = capability("neighborhood");
  $("expand").hidden = !canNeighborhood;
  $("expand").disabled = !canNeighborhood || !state.selected;
  updateContinueButton();

  const canPath = capability("path");
  document.querySelectorAll("[for='path-from'],[for='path-to']").forEach((label) => {
    label.hidden = !canPath;
  });
  $("path-from").hidden = !canPath;
  $("path-to").hidden = !canPath;
  $("find-path").hidden = !canPath;
  $("find-path").disabled = !canPath;
  const graphStatus = $("graph-status");
  if (!canNeighborhood && !canPath) {
    graphStatus.dataset.capabilityPlaceholder = "graph";
    graphStatus.textContent =
      "Graph expansion and path finding are unavailable from this Explorer service.";
  } else if (graphStatus.dataset.capabilityPlaceholder === "graph") {
    delete graphStatus.dataset.capabilityPlaceholder;
    graphStatus.textContent = "No graph expansion yet.";
  }

  if (!capability("documents")) {
    setStatus($("documents-status"), "Document listing is unavailable from this Explorer service.");
    $("documents").replaceChildren();
    $("more").hidden = true;
  }
  applyIngestCapability();
  document.querySelectorAll("[data-extract-document]").forEach((button) => {
    button.hidden = !capability("extraction");
    button.disabled = !capability("extraction");
  });
  applyDecisionCapability();
}

function applyDecisionCapability() {
  const tab = $("tab-decisions");
  const panel = $("decisions");
  const available = capability("decisions");
  if (!tab || !panel) return;
  tab.disabled = !available;
  tab.setAttribute("aria-disabled", String(!available));
  tab.classList.toggle("unavailable", !available);
  tab.title = available
    ? "Inspect authenticated local decision records"
    : "Decision providers are not mounted by this host";
  panel.dataset.capabilityUnavailable = String(!available);
  panel.querySelectorAll("form input, form textarea, form button").forEach((control) => {
    control.disabled = !available;
  });
  const status = $("decision-console-status");
  if (!available && status) {
    status.textContent =
      "Decision providers are unavailable on this Explorer host. " +
      "No decision records are being implied or synthesized.";
  }
}

function applyIngestCapability() {
  const canIngest = capability("ingest");
  $("upload-section").hidden = !canIngest;
  $("upload-files").disabled = !canIngest || uploadLoading;
  $("upload-directory").disabled = !canIngest || uploadLoading;
  $("upload-button").disabled = !canIngest || uploadLoading;
  if (!canIngest) {
    $("upload-results").replaceChildren();
    setStatus($("upload-status"), "Uploads are unavailable from this Explorer service.");
  } else if (!$("upload-status").textContent) {
    setStatus($("upload-status"), uploadLimitText());
  }
}

function uploadLimitText() {
  const files = state.uploadLimits.max_upload_files || "bounded";
  const bytes = state.uploadLimits.max_upload_file_bytes || 0;
  const size = bytes ? `${Math.floor(bytes / 1024)} KiB` : "bounded";
  return `Upload up to ${files} file(s), ${size} each.`;
}

function applyVectorCapability(canRetrieve) {
  const checkbox = $("mode-vector");
  const control = $("mode-vector-control");
  const status = $("vector-mode-status");
  const canVector = vectorRetrievalCapability();
  checkbox.disabled = !canRetrieve || !canVector;
  control.classList.toggle("unavailable", !canVector);
  control.setAttribute("aria-disabled", String(!canVector));
  if (!canVector) {
    checkbox.checked = false;
    status.hidden = false;
    status.textContent =
      "Vector retrieval is disabled because this Explorer service did not advertise vector support.";
  } else {
    status.hidden = true;
    status.textContent = "";
  }
}

function setMessage(element, message, className = "muted") {
  const paragraph = document.createElement("p");
  paragraph.className = className;
  paragraph.textContent = message;
  element.replaceChildren(paragraph);
}

function setStatus(element, message, className = "muted") {
  element.className = className;
  element.textContent = message;
}

function showError(element, error) {
  if (element.getAttribute && element.getAttribute("role") === "status") {
    setStatus(element, error.message || String(error), "error");
    return;
  }
  setMessage(element, error.message || String(error), "error");
}

// showActionError renders an explicit authorization denial distinctly from any
// other failure. A denied action gets the "denied" treatment and reports true;
// every other error falls back to the generic error rendering.
function showActionError(element, error, action) {
  if (needsReauthentication(error)) {
    handleReauthentication();
    if (element.getAttribute && element.getAttribute("role") === "status") {
      setStatus(element, reauthenticationText(), "reauth");
    } else {
      setMessage(element, reauthenticationText(), "reauth");
    }
    return true;
  }
  if (isDenied(error)) {
    if (element.getAttribute && element.getAttribute("role") === "status") {
      setStatus(element, deniedText(action, state.identity), "denied");
    } else {
      setMessage(element, deniedText(action, state.identity), "denied");
    }
    return true;
  }
  showError(element, error);
  return false;
}

function pin(snapshot) {
  state.snapshot = snapshot;
  $("snapshot").textContent =
    `snapshot ${String(snapshot.id || "").slice(0, 10)} · frontier ${snapshot.frontier}`;
}

async function loadDocuments(reset = true) {
  if (!capability("documents")) return;
  if (documentsLoading) {
    if (reset) {
      documentsRefreshQueued = true;
      documentsGeneration++;
    }
    return;
  }
  const generation = ++documentsGeneration;
  documentsLoading = true;
  $("more").disabled = true;
  $("documents").setAttribute("aria-busy", "true");
  if (reset) {
    state.cursor = "";
    setStatus($("documents-status"), "Loading documents…");
  }
  try {
    const response = await api("documents", {
      snapshot: state.snapshot,
      page: {limit: 25, cursor: state.cursor},
    });
    if (generation !== documentsGeneration) return;
    pin(response.snapshot);
    if (reset) $("documents").replaceChildren();
    const documents = response.documents || [];
    const suppressed = response.suppressed || 0;
    const restricted = response.restricted || 0;
    const className = restricted > 0 ? "restricted" : suppressed > 0 ? "withheld" : null;
    if (documents.length === 0 && reset) {
      setStatus(
        $("documents-status"),
        emptyDocumentsText(state.identity) +
          suppressionClause(suppressed, state.identity) +
          restrictionClause(restricted, state.identity),
        className || "empty-state",
      );
    } else {
      const total = $("documents").children.length + documents.length;
      setStatus(
        $("documents-status"),
        `Showing ${total} document(s).` +
          suppressionClause(suppressed, state.identity) +
          restrictionClause(restricted, state.identity),
        className || "muted",
      );
    }
    const fragment = document.createDocumentFragment();
    for (const item of documents) fragment.append(createDocumentCard(item));
    $("documents").append(fragment);
    state.cursor = response.next_cursor || "";
    $("more").hidden = !state.cursor;
  } catch (error) {
    if (generation === documentsGeneration) {
      if (isDenied(error)) $("documents").replaceChildren();
      showActionError($("documents-status"), error, "list documents");
    }
  } finally {
    documentsLoading = false;
    $("documents").setAttribute("aria-busy", "false");
    $("more").disabled = false;
    if (documentsRefreshQueued) {
      documentsRefreshQueued = false;
      loadDocuments(true);
    }
  }
}

async function uploadFiles(fileList) {
  if (!capability("ingest") || uploadLoading) return;
  const files = [...(fileList || [])];
  if (files.length === 0) {
    setStatus($("upload-status"), "Choose at least one file to upload.", "empty-state");
    return;
  }
  uploadLoading = true;
  applyIngestCapability();
  $("upload-results").replaceChildren();
  setStatus($("upload-status"), `Preparing ${files.length} file(s)…`);
  try {
    const plan = planUpload(files);
    const results = [...plan.skipped];
    let uploaded = 0;
    let failed = 0;
    const failureMessages = [];
    let latestSnapshot = null;
    for (let index = 0; index < plan.batches.length; index++) {
      const batch = plan.batches[index];
      setStatus(
        $("upload-status"),
        `Uploading batch ${index + 1} of ${plan.batches.length} (${batch.length} file(s))…`,
      );
      try {
        const value = await uploadBatch(batch);
        const uploadedFiles = value.files || [];
        uploaded += uploadedFiles.length;
        results.push(...uploadedFiles);
        latestSnapshot = value.snapshot || latestSnapshot;
      } catch (error) {
        failed += batch.length;
        failureMessages.push(error.message || String(error));
        // This append is load-bearing; TestStaticWorkspaceUploadReportsMidBatchFailure pins
        // that earlier successful batches remain visible when a later batch fails.
        results.push(...batch.map((entry) => failedUploadResult(entry, error)));
      }
    }
    if (uploaded > 0 || failed > 0) {
      // This reset is load-bearing; TestStaticWorkspaceUploadReportsMidBatchFailure pins
      // that document views refresh from upload state without discarding partial results.
      clearSnapshotDependentViews();
      if (latestSnapshot) {
        // This pin is load-bearing; TestStaticWorkspaceUploadReportsMidBatchFailure pins
        // that the document refresh uses the last successful upload snapshot.
        pin(latestSnapshot);
      } else {
        clearPinnedSnapshot();
      }
      renderUploadResults(results, uploadSummary(files.length, uploaded, failed, plan.skipped.length, failureMessages));
      await loadDocuments(true);
    } else {
      renderUploadResults(results, uploadSummary(files.length, uploaded, failed, plan.skipped.length, failureMessages));
    }
  } catch (error) {
    showActionError($("upload-status"), error, "upload files");
  } finally {
    uploadLoading = false;
    applyIngestCapability();
    $("upload-files").value = "";
    $("upload-directory").value = "";
  }
}

function planUpload(files) {
  const bounds = uploadBounds();
  const entries = uploadEntries(files);
  const batches = [];
  const skipped = [];
  let batch = [];
  let batchBytes = 0;
  for (const entry of entries) {
    if (entry.size > bounds.maxFileBytes) {
      // This branch is load-bearing; TestStaticWorkspaceDirectoryUploadSkipsOversizedFiles
      // pins that one oversized directory file is reported without aborting its siblings.
      skipped.push(skippedUploadResult(entry, `exceeds the per-file limit of ${formatBytes(bounds.maxFileBytes)}`));
      continue;
    }
    if (entry.size > bounds.maxTotalBytes) {
      skipped.push(skippedUploadResult(entry, `exceeds the per-request limit of ${formatBytes(bounds.maxTotalBytes)}`));
      continue;
    }
    if (batch.length > 0 &&
      (batch.length >= bounds.maxFiles || batchBytes + entry.size > bounds.maxTotalBytes)) {
      // This boundary is load-bearing; TestStaticWorkspaceDirectoryUploadBatchesAtServerLimit
      // pins that MaxUploadFiles + 1 creates a second request instead of truncating.
      batches.push(batch);
      batch = [];
      batchBytes = 0;
    }
    if (batch.length >= bounds.maxFiles || batchBytes + entry.size > bounds.maxTotalBytes) {
      throw new Error("Upload bounds cannot fit a file into a request; no files were truncated.");
    }
    batch.push(entry);
    batchBytes += entry.size;
  }
  if (batch.length > 0) batches.push(batch);
  return {batches, skipped};
}

function uploadBounds() {
  const maxFiles = Number(state.uploadLimits.max_upload_files) || 0;
  const maxFileBytes = Number(state.uploadLimits.max_upload_file_bytes) || 0;
  const maxTotalBytes = Number(state.uploadLimits.max_upload_total_bytes) || 0;
  if (maxFiles <= 0 || maxFileBytes <= 0 || maxTotalBytes <= 0) {
    // This error is load-bearing; TestStaticWorkspaceUploadRequiresMetadataBounds pins
    // that missing /api/v1/meta bounds stop upload instead of silently truncating files.
    throw new Error("Upload bounds are unavailable from /api/v1/meta; reload before uploading.");
  }
  return {maxFiles, maxFileBytes, maxTotalBytes};
}

function uploadEntries(files) {
  const entries = files.map((file, index) => {
    const sourcePath = uploadSourcePath(file, index);
    return {
      file,
      index,
      sourcePath,
      // This name derivation is load-bearing; TestStaticWorkspaceDirectoryUploadRenamesSkillCollisions
      // pins that repeated SKILL.md basenames from different directories reach the server uniquely.
      uploadName: safeUploadName(sourcePath, index),
      size: Math.max(0, Number(file.size) || 0),
    };
  });
  // This sort is load-bearing; TestStaticWorkspaceDirectoryUploadRenamesSkillCollisions
  // pins deterministic directory batch order independent of browser FileList order.
  entries.sort((left, right) =>
    left.sourcePath.localeCompare(right.sourcePath) || left.index - right.index);
  const seen = new Map();
  for (const entry of entries) {
    const count = (seen.get(entry.uploadName) || 0) + 1;
    seen.set(entry.uploadName, count);
    if (count > 1) entry.uploadName = `${count}__${entry.uploadName}`;
  }
  return entries;
}

function uploadSourcePath(file, index) {
  const relative = String((file && file.webkitRelativePath) || "").trim();
  const name = String((file && file.name) || `upload-${index + 1}.txt`).trim();
  return relative || name || `upload-${index + 1}.txt`;
}

function safeUploadName(sourcePath, index) {
  const parts = String(sourcePath || "")
    .split(/[\\/]+/)
    .map(safeUploadSegment)
    .filter(Boolean);
  const name = parts.join("__") || `upload-${index + 1}.txt`;
  return safeUploadSegment(name);
}

function safeUploadSegment(segment) {
  let value = String(segment || "")
    .trim()
    .replace(/[\u0000-\u001f\u007f-\u009f\u00ad\u061c\u200b-\u200f\u202a-\u202e\u2060-\u206f\ufeff<>:"|?*]/g, "_")
    .replace(/\.\.+/g, "._")
    .replace(/[. ]+$/g, "");
  if (!value) value = "file";
  if (/^(con|prn|aux|nul|com[1-9]|lpt[1-9])(?:\..*)?$/i.test(value)) {
    value = `${value}_`;
  }
  return value;
}

// uploadReason is the optional declassification reason (#664). The server
// owns the rule; an empty field sends nothing.
function uploadReason() {
  const input = $("upload-reason");
  return String((input && input.value) || "").trim();
}

async function uploadBatch(batch) {
  const body = new FormData();
  const reason = uploadReason();
  if (reason) body.append("declassification_reason", reason);
  for (const entry of batch) body.append("files", entry.file, entry.uploadName);
  const response = await fetch("/api/v1/ingest", {
    method: "POST",
    headers: {"accept": "application/json", "x-shoal-workspace-request": "1", ...authHeaders()},
    body,
  });
  const value = await response.json();
  if (!response.ok) throw apiError(value, response);
  return value;
}

function skippedUploadResult(entry, reason) {
  return {
    name: entry.uploadName,
    original_name: entry.sourcePath,
    disposition: "skipped",
    media_type: "",
    span_count: 0,
    message: `Skipped ${entry.sourcePath}: ${formatBytes(entry.size)} ${reason}.`,
  };
}

function failedUploadResult(entry, error) {
  return {
    name: entry.uploadName,
    original_name: entry.sourcePath,
    disposition: "failed",
    media_type: "",
    span_count: 0,
    message: `Failed ${entry.sourcePath}: ${error.message || String(error)}`,
  };
}

function uploadSummary(total, uploaded, failed, skipped, failureMessages = []) {
  if (failed > 0 || skipped > 0) {
    const parts = [`Uploaded ${uploaded} of ${total} file(s)`];
    if (failed > 0) parts.push(`${failed} failed`);
    if (skipped > 0) parts.push(`${skipped} skipped`);
    const detail = failureMessages.length > 0 ? `: ${failureMessages[0]}` : "";
    return `${parts.join("; ")}${detail}.`;
  }
  return `Uploaded ${uploaded} file(s).`;
}

function formatBytes(bytes) {
  const size = Number(bytes) || 0;
  if (size >= 1024 * 1024) return `${(size / (1024 * 1024)).toFixed(1)} MiB`;
  if (size >= 1024) return `${Math.ceil(size / 1024)} KiB`;
  return `${size} B`;
}

function clearPinnedSnapshot() {
  state.snapshot = {};
  $("snapshot").textContent = "snapshot latest";
}

function clearSnapshotDependentViews() {
  documentGeneration++;
  searchGeneration++;
  graphGeneration++;
  state.document = null;
  state.currentDocumentID = "";
  state.selectedSectionID = "";
  state.selected = null;
  state.nodes.clear();
  state.edges.clear();
  state.graphCursors.clear();
  positions.clear();
  $("hierarchy").replaceChildren();
  setStatus($("hierarchy-status"), "Select a document.");
  $("evidence-results").replaceChildren();
  setStatus($("evidence-status"), "Run a retrieval query to inspect exact evidence and explanations.");
  $("graph-status").textContent = "No graph expansion yet.";
  $("selection").className = "muted";
  setMessage($("selection"), "Select evidence, a hierarchy section, or a graph node.");
  renderGraphList();
  updateContinueButton();
  draw();
}

function renderUploadResults(files, statusText = "") {
  const results = $("upload-results");
  results.replaceChildren();
  if (files.length === 0) {
    setStatus($("upload-status"), "No files were ingested.", "empty-state");
    return;
  }
  setStatus($("upload-status"), statusText || `Uploaded ${files.length} file(s).`);
  for (const file of files) {
    const item = document.createElement("li");
    const details = [];
    if (file.media_type) details.push(file.media_type);
    if (file.span_count !== undefined) details.push(`${file.span_count} span(s)`);
    let text = `${file.name}: ${file.disposition}`;
    if (details.length > 0) text += ` (${details.join(", ")})`;
    if (file.original_name && file.original_name !== file.name) text += ` from ${file.original_name}`;
    if (file.message) text += `. ${file.message}`;
    // This rendering is load-bearing; TestStaticWorkspaceDirectoryUploadRenamesSkillCollisions
    // pins that recognized agent skill metadata is visible per file.
    if (file.skill_file) text += `. ${skillFileText(file.skill_file)}`;
    item.textContent = text;
    results.append(item);
  }
}

function skillFileText(value) {
  if (value.recognized) {
    return `Agent skill file recognized: ${value.name} — ${value.description}`;
  }
  if (value.expected) {
    return value.message || "Expected an agent skills file, but it was not recognized.";
  }
  return value.message || "Markdown skills metadata was inspected.";
}

$("upload").onsubmit = async (event) => {
  event.preventDefault();
  await uploadFiles(selectedUploadFiles());
};

function selectedUploadFiles() {
  return [
    ...($("upload-files").files || []),
    ...($("upload-directory").files || []),
  ];
}

function updateUploadReadyStatus() {
  const count = selectedUploadFiles().length;
  setStatus(
    $("upload-status"),
    count ? `${count} file(s) ready to upload.` : uploadLimitText(),
  );
}

$("upload-files").onchange = updateUploadReadyStatus;
$("upload-directory").onchange = updateUploadReadyStatus;

$("upload-drop").ondragover = (event) => {
  event.preventDefault();
  if (capability("ingest")) $("upload-drop").classList.add("dragging");
};

$("upload-drop").ondragleave = () => {
  $("upload-drop").classList.remove("dragging");
};

$("upload-drop").ondrop = async (event) => {
  event.preventDefault();
  $("upload-drop").classList.remove("dragging");
  await uploadFiles(event.dataTransfer && event.dataTransfer.files);
};

function createDocumentCard(item) {
  const documentValue = item.document || {};
  const title = documentValue.title || "(untitled document)";
  const documentID = documentValue.id || "";
  const sourceURI = item.source_uri || "";
  const sourceMediaType = item.source_media_type || "";
  if (documentID) state.sourceURIs.set(documentID, sourceURI);

  const card = document.createElement("div");
  card.className = "doc-card";
  card.dataset.document = documentID;

  const button = document.createElement("button");
  button.className = "doc";
  button.type = "button";
  button.disabled = !capability("document");
  button.title = sourceLabel(sourceURI, documentID);
  button.onclick = () => loadDocument(documentID, item.revision && item.revision.id);

  const titleElement = document.createElement("strong");
  titleElement.textContent = title;
  const sourceElement = document.createElement("small");
  sourceElement.className = "source-label";
  sourceElement.textContent = sourceLabel(sourceURI, documentID);
  button.append(titleElement, sourceElement);
  if (sourceMediaType) {
    const media = document.createElement("span");
    media.className = "media-badge";
    media.textContent = sourceMediaType;
    button.append(media);
  }
  card.append(button);
  if (capability("extraction")) {
    const extract = document.createElement("button");
    extract.type = "button";
    extract.className = "extract-skill";
    extract.dataset.extractDocument = documentID;
    extract.textContent = "Extract skills";
    // Load-bearing UI action; TestStaticWorkspaceUploadBehavior pins that extraction is explicitly user-triggered after upload.
    extract.onclick = async () => {
      await extractDocument(documentID, item.revision && item.revision.id, card);
    };
    card.append(extract);
  }

  if (sourceURI) {
    const details = document.createElement("details");
    details.className = "source-details";
    const summary = document.createElement("summary");
    summary.textContent = "Full source URI";
    const code = document.createElement("code");
    code.textContent = sourceURI;
    details.append(summary, code);
    card.append(details);
  }

  async function extractDocument(documentID, revisionID, card) {
    if (!capability("extraction")) return;
    const status = document.createElement("p");
    status.className = "muted";
    status.setAttribute("role", "status");
    status.textContent = "Extracting ontology entities…";
    card.append(status);
    try {
      const response = await api("extract", {
        snapshot: state.snapshot,
        document_id: documentID,
        revision_id: revisionID,
      });
      clearSnapshotDependentViews();
      pin(response.snapshot);
      setStatus(
        status,
        `Extracted ${response.entity_count || 0} entit(ies), ` +
          `${response.relation_count || 0} ontology relation(s), and ` +
          `${response.graph_edge_count || 0} graph edge(s).`,
      );
      await loadDocuments(true);
      if ((response.entity_node_ids || []).length > 0 && capability("neighborhood")) {
        await expandIDs([documentID]);
      }
    } catch (error) {
      showActionError(status, error, "extract ontology entities from this document");
    }
  }
  updateDocumentCardSelection(card);
  return card;
}

function sourceLabel(uri, fallback = "") {
  const value = String(uri || "").trim();
  if (!value) return fallback || "source unavailable";
  try {
    const parsed = new URL(value);
    if (parsed.protocol === "file:") return basename(parsed.pathname) || "local file";
    if (parsed.protocol === "http:" || parsed.protocol === "https:") {
      const leaf = basename(parsed.pathname);
      return leaf ? `${parsed.hostname} / ${leaf}` : parsed.hostname;
    }
  } catch (_) {
    // Fall through to generic path handling.
  }
  return basename(value) || value;
}

function basename(value) {
  const withoutQuery = String(value || "").split(/[?#]/, 1)[0];
  const normalized = withoutQuery.replace(/\\/g, "/").replace(/\/+$/, "");
  const name = normalized.slice(normalized.lastIndexOf("/") + 1);
  try {
    return decodeURIComponent(name);
  } catch (_) {
    return name;
  }
}

async function loadDocument(documentID, revisionID) {
  if (!capability("document")) return;
  const generation = ++documentGeneration;
  state.currentDocumentID = documentID;
  state.document = null;
  state.selectedSectionID = "";
  updateDocumentCardSelection();
  $("hierarchy").replaceChildren();
  $("hierarchy").setAttribute("aria-busy", "true");
  setStatus($("hierarchy-status"), "Loading document hierarchy…");
  try {
    const response = await api("document", {
      snapshot: state.snapshot,
      document_id: documentID,
      revision_id: revisionID,
    });
    if (generation !== documentGeneration) return;
    pin(response.snapshot);
    state.document = response.document;
    state.sourceURIs.set(response.document.document.id, response.document.source_uri || "");
    renderHierarchy(response.document.root);
    mergeGraph({nodes: [nodeFromDocument(response.document.document)], edges: []});
    draw();
  } catch (error) {
    if (generation === documentGeneration) showActionError($("hierarchy-status"), error, "open this document");
  } finally {
    if (generation === documentGeneration) $("hierarchy").setAttribute("aria-busy", "false");
  }
}

function updateDocumentCardSelection(card) {
  const cards = card ? [card] : [...document.querySelectorAll(".doc-card")];
  for (const candidate of cards) {
    const selected = candidate.dataset.document === state.currentDocumentID;
    candidate.classList.toggle("selected", selected);
    const button = candidate.querySelector(".doc");
    if (button) button.setAttribute("aria-current", selected ? "true" : "false");
  }
}

function renderHierarchy(root) {
  const hierarchy = $("hierarchy");
  if (!root) {
    hierarchy.replaceChildren();
    setStatus($("hierarchy-status"), "This document has no hierarchy.", "empty-state");
    return;
  }
  setStatus($("hierarchy-status"), "Document hierarchy loaded.");
  hierarchy.replaceChildren(sectionList(root));
}

function sectionList(view) {
  const list = document.createElement("ul");
  const item = document.createElement("li");
  const button = document.createElement("button");
  button.className = "section-node";
  button.type = "button";
  button.dataset.section = view.section.id;
  button.textContent = view.section.heading || "(untitled)";
  button.setAttribute("aria-pressed", String(view.section.id === state.selectedSectionID));
  button.onclick = () => selectSection(view.section.id);
  item.append(button);
  for (const span of view.spans || []) {
    const snippet = document.createElement("div");
    snippet.className = "citation span-snippet";
    snippet.textContent = span.text || "";
    item.append(snippet);
  }
  for (const child of view.children || []) item.append(sectionList(child));
  list.append(item);
  return list;
}

function selectSection(id) {
  if (!state.document || !state.document.root) return;
  const find = (view) =>
    view.section.id === id ? view : (view.children || []).map(find).find(Boolean);
  const view = find(state.document.root);
  if (!view) return;
  state.selectedSectionID = id;
  updateSectionSelection();
  showSelection([
    ["Section", view.section.heading || "(untitled)"],
    ["ID", id],
    ["Range", `${view.section.range.start.offset}–${view.section.range.end.offset}`],
  ]);
  if (capability("neighborhood")) expandIDs([id]);
}

function updateSectionSelection() {
  $("hierarchy").querySelectorAll("[data-section]").forEach((element) => {
    element.setAttribute(
      "aria-pressed",
      String(element.dataset.section === state.selectedSectionID),
    );
  });
}

function nodeFromDocument(documentValue) {
  return {
    id: documentValue.id,
    kind: "document",
    labels: [documentValue.title],
    properties: documentValue.metadata || {},
  };
}

$("search").onsubmit = async (event) => {
  event.preventDefault();
  if (!capability("retrieve")) return;
  const generation = ++searchGeneration;
  setStatus($("evidence-status"), "Searching evidence…");
  $("evidence-results").replaceChildren();
  try {
    const response = await api("retrieve", {
      snapshot: state.snapshot,
      query: {
        text: $("query").value,
        top_k: 20,
        modes: [...document.querySelectorAll("#modes input:checked:not(:disabled)")].map(
          (input) => input.value,
        ),
        explain: true,
        as_of: state.snapshot.as_of,
      },
    });
    if (generation !== searchGeneration) return;
    pin(response.snapshot);
    renderEvidence(response.retrieval, response.suppressed || 0, response.restricted || 0);
  } catch (error) {
    if (generation === searchGeneration) {
      $("evidence-results").replaceChildren();
      showActionError($("evidence-status"), error, "run this retrieval");
    }
  }
};

function renderEvidence(response, suppressed = 0, restricted = 0) {
  const results = response.results || [];
  const withheld = Number(suppressed) || 0;
  const barred = Number(restricted) || 0;
  const className = barred > 0 ? "restricted" : withheld > 0 ? "withheld" : null;
  if (results.length === 0) {
    $("evidence-results").replaceChildren();
    setStatus(
      $("evidence-status"),
      emptyRetrievalText(state.identity) +
        suppressionClause(withheld, state.identity) +
        restrictionClause(barred, state.identity),
      className || "empty-state",
    );
    draw();
    return;
  }
  setStatus(
    $("evidence-status"),
    `Showing ${results.length} evidence result(s).` +
      suppressionClause(withheld, state.identity) +
      restrictionClause(barred, state.identity),
    className || "muted",
  );
  const fragment = document.createDocumentFragment();
  results.forEach((result) => {
    const element = document.createElement("article");
    element.className = "result";

    const header = document.createElement("div");
    header.className = "result-header";
    const title = document.createElement("b");
    title.className = "result-title";
    title.textContent = result.id;
    title.title = result.id;
    const score = document.createElement("span");
    score.className = "score";
    score.textContent = Number(result.score).toFixed(3);
    header.append(title, score);
    element.append(header);

    (result.evidence || []).forEach((item) => {
      const quote = document.createElement("button");
      quote.className = "evidence-quote";
      quote.type = "button";
      quote.textContent = item.quote || "";
      quote.onclick = () => selectEvidence(result, item);
      const citation = document.createElement("div");
      citation.className = "citation";
      citation.textContent = `${item.citation.document_id} / ${item.citation.revision_id} / bytes ` +
        `${item.citation.range.start.offset}–${item.citation.range.end.offset}`;
      element.append(quote, citation);
      mergeGraph(item.path || {});
    });

    if (result.explanation) {
      const explanation = document.createElement("pre");
      explanation.className = "explanation";
      explanation.textContent = `${result.explanation.summary || ""}\n` +
        JSON.stringify(result.explanation.scores || {}, null, 2);
      element.append(explanation);
    }
    fragment.append(element);
  });
  $("evidence-results").replaceChildren(fragment);
  draw();
}

function selectEvidence(result, evidence) {
  const source = state.sourceURIs.get(evidence.citation.document_id) || "";
  showSelection([
    ["Result", result.id],
    ["Quote", evidence.quote],
    ["Document", sourceLabel(source, evidence.citation.document_id)],
    ["Section", evidence.citation.section_id],
    ["Span", evidence.citation.span_id],
  ]);
  loadDocument(evidence.citation.document_id, evidence.citation.revision_id);
}

function showSelection(entries) {
  $("selection").classList.remove("muted");
  $("selection").replaceChildren(kvBlock(entries));
}

// kvBlock renders a label/value grid. It is shared by every selection inspector
// so a graph node, a piece of evidence and a derived edge all read identically.
function kvBlock(entries) {
  const container = document.createElement("div");
  container.className = "kv";
  for (const [key, value] of entries) {
    const label = document.createElement("b");
    label.textContent = key;
    const text = document.createElement("span");
    text.textContent = value == null ? "" : String(value);
    text.title = text.textContent;
    container.append(label, text);
  }
  return container;
}

// Property keys latentAssertionGraphEdge stamps on a derived edge. The origin
// key is authoritative and always present on the edge itself, so the graph can
// tell a derived edge from an asserted one without the assertion list.
const EDGE_PROPERTY_ORIGIN = "ontology.assertion.origin";
const EDGE_PROPERTY_ASSERTION_ID = "ontology.assertion.id";
const EDGE_PROPERTY_DERIVATION_ID = "ontology.assertion.derivation.id";
const EDGE_PROPERTY_DERIVATION_SCORE = "ontology.assertion.derivation.score";
const GRAPH_DERIVED_ORIGIN = "derived";

// decodedEdgeProperties turns an edge's wire properties — a list of base64url
// key/value pairs — into a decoded string map. It memoizes on the edge so the
// per-frame draw loop never re-decodes, and a fresh expansion replaces the edge
// object and so refreshes the cache.
function decodedEdgeProperties(edge) {
  if (!edge) return {};
  if (edge._decodedProperties) return edge._decodedProperties;
  const decoded = {};
  for (const entry of edge.properties || []) {
    if (!entry) continue;
    decoded[base64UrlDecode(entry.key)] = base64UrlDecode(entry.value);
  }
  edge._decodedProperties = decoded;
  return decoded;
}

// edgeIsDerived reports whether an edge carries a latent, similarity-derived
// ontology assertion rather than one asserted from a document.
function edgeIsDerived(edge) {
  return decodedEdgeProperties(edge)[EDGE_PROPERTY_ORIGIN] === GRAPH_DERIVED_ORIGIN;
}

// edgeDerivationScore reads the similarity score the producer recorded on a
// derived edge, or null when the edge is not derived or carries no score.
function edgeDerivationScore(edge) {
  const raw = decodedEdgeProperties(edge)[EDGE_PROPERTY_DERIVATION_SCORE];
  if (raw === undefined || raw === "") return null;
  const parsed = Number(raw);
  return Number.isFinite(parsed) ? parsed : null;
}

function mergeGraph(graph) {
  const previousNodeCount = state.nodes.size;
  for (const node of graph.nodes || []) state.nodes.set(node.id, node);
  for (const edge of graph.edges || []) state.edges.set(edge.id, edge);
  for (const assertion of graph.assertions || []) {
    state.assertions.set(assertion.id, assertion);
  }
  if (state.nodes.size !== previousNodeCount) positions.clear();
  renderGraphList();
}

async function expandIDs(ids, cursor = "") {
  if (!capability("neighborhood")) return;
  const generation = graphGeneration;
  $("graph-status").textContent = cursor ? "Loading more neighbors…" : "Expanding graph…";
  try {
    const response = await api("neighborhood", {
      snapshot: state.snapshot,
      node_ids: ids,
      depth: 1,
      fanout: 18,
      max_nodes: 120,
      cursor,
    });
    if (generation !== graphGeneration) return;
    pin(response.snapshot);
    mergeGraph(response.neighborhood);
    if (ids.length === 1) {
      if (response.next_cursor) state.graphCursors.set(ids[0], response.next_cursor);
      else state.graphCursors.delete(ids[0]);
      updateContinueButton();
    }
    $("graph-status").textContent = response.truncated
      ? `Bounded result truncated at snapshot frontier ${response.snapshot.frontier}.`
      : `Complete requested expansion at snapshot frontier ${response.snapshot.frontier}.`;
    activate("graph");
    draw();
  } catch (error) {
    if (generation !== graphGeneration) return;
    if (needsReauthentication(error)) {
      handleReauthentication();
      $("graph-status").textContent = reauthenticationText();
    } else if (isDenied(error)) {
      $("graph-status").textContent = deniedText("expand this neighborhood", state.identity);
    } else {
      $("graph-status").textContent = `Graph expansion failed: ${error.message || String(error)}`;
    }
    showActionError($("selection"), error, "expand this neighborhood");
  }
}

$("expand").onclick = () => state.selected && expandIDs([state.selected]);
$("continue-expansion").onclick = () => {
  const cursor = state.graphCursors.get(state.selected);
  if (cursor) expandIDs([state.selected], cursor);
};
$("more").onclick = () => loadDocuments(false);
$("find-path").onclick = async () => {
  if (!capability("path")) return;
  const generation = graphGeneration;
  $("graph-status").textContent = "Finding directed path…";
  try {
    const response = await api("path", {
      snapshot: state.snapshot,
      from: $("path-from").value,
      to: $("path-to").value,
      max_depth: 4,
      fanout: 25,
    });
    if (generation !== graphGeneration) return;
    pin(response.snapshot);
    mergeGraph(response.path);
    $("graph-status").textContent =
      `Directed path at snapshot frontier ${response.snapshot.frontier}.`;
    draw();
  } catch (error) {
    if (generation !== graphGeneration) return;
    if (needsReauthentication(error)) {
      handleReauthentication();
      $("graph-status").textContent = reauthenticationText();
    } else if (isDenied(error)) {
      $("graph-status").textContent = deniedText("find a path", state.identity);
    } else {
      $("graph-status").textContent = `Path finding failed: ${error.message || String(error)}`;
    }
    showActionError($("selection"), error, "find a path");
  }
};

document.querySelectorAll(".tab").forEach((tab) => {
  tab.onclick = () => activate(tab.dataset.panel);
  tab.onkeydown = (event) => {
    if (event.key !== "ArrowLeft" && event.key !== "ArrowRight") return;
    const tabs = [...document.querySelectorAll(".tab")];
    const offset = event.key === "ArrowRight" ? 1 : -1;
    const target = tabs[(tabs.indexOf(tab) + offset + tabs.length) % tabs.length];
    activate(target.dataset.panel);
    target.focus();
  };
});

function activate(id) {
  if (id === "decisions" && !capability("decisions")) return;
  document.querySelectorAll(".tab,.panel").forEach((element) => {
    element.classList.remove("active");
  });
  document.querySelectorAll(".tab").forEach((tab) => {
    const selected = tab.dataset.panel === id;
    tab.setAttribute("aria-selected", String(selected));
    tab.tabIndex = selected ? 0 : -1;
  });
  document.querySelector(`[data-panel="${id}"]`).classList.add("active");
  $(id).classList.add("active");
  if (id === "graph") draw();
}

function renderGraphList() {
  const nodesElement = $("graph-nodes");
  const edgesElement = $("graph-edges");
  nodesElement.replaceChildren();
  edgesElement.replaceChildren();
  if (state.nodes.size === 0) {
    setMessage(nodesElement, "No graph nodes yet.", "empty-state");
  } else {
    for (const node of state.nodes.values()) {
      const button = document.createElement("button");
      button.type = "button";
      button.textContent = nodeName(node, node.id);
      button.title = button.textContent;
      button.setAttribute("aria-label", `${button.textContent}, node ${node.id}`);
      button.setAttribute("aria-pressed", String(node.id === state.selected));
      button.onclick = () => selectNode(node.id);
      nodesElement.append(button);
    }
  }
  if (state.edges.size === 0) {
    const item = document.createElement("li");
    item.className = "muted";
    item.textContent = "No graph edges yet.";
    edgesElement.append(item);
  } else {
    for (const edge of state.edges.values()) {
      const item = document.createElement("li");
      const from = state.nodes.get(edge.from);
      const to = state.nodes.get(edge.to);
      const derived = edgeIsDerived(edge);
      const label = `${nodeName(from, edge.from)} → ${nodeName(to, edge.to)} (${edge.type})`;
      const button = document.createElement("button");
      button.type = "button";
      button.className = derived ? "graph-edge derived" : "graph-edge";
      button.textContent = derived ? `◆ ${label}` : label;
      button.title = button.textContent;
      button.setAttribute("aria-pressed", String(edge.id === state.selectedEdge));
      button.setAttribute(
        "aria-label",
        `${derived ? "Derived edge, " : ""}${nodeName(from, edge.from)} ${edge.from} ` +
          `to ${nodeName(to, edge.to)} ${edge.to}, ${edge.type}`,
      );
      button.onclick = () => selectEdge(edge.id);
      item.append(button);
      edgesElement.append(item);
    }
  }
}

function nodeName(node, fallback) {
  return node ? ((node.labels && node.labels[0]) || node.kind || node.id) : fallback;
}

function selectNode(id) {
  state.selected = id;
  state.selectedEdge = null;
  $("expand").disabled = !id || !capability("neighborhood");
  updateContinueButton();
  const node = state.nodes.get(id);
  if (node) {
    showSelection([
      ["Node", id],
      ["Kind", node.kind],
      ["Labels", (node.labels || []).join(", ")],
      ["Properties", JSON.stringify(node.properties || {})],
    ]);
    $("path-from").value = id;
  } else {
    $("selection").className = "muted";
    setMessage($("selection"), "Select evidence, a hierarchy section, or a graph node.");
  }

  renderGraphList();
  draw();
}

// selectEdge opens the derivation inspector for a graph edge. Selecting an edge
// clears any node selection so the neighborhood expansion controls, which act
// on a node, do not appear to apply to the edge.
function selectEdge(id) {
  const edge = state.edges.get(id);
  if (!edge) {
    selectNode(null);
    return;
  }
  state.selectedEdge = id;
  state.selected = null;
  $("expand").disabled = true;
  updateContinueButton();
  renderDerivationInspector(edge);
  renderGraphList();
  draw();
}

// renderDerivationInspector shows how a selected edge's assertion was derived —
// the producer identity, the similarity score and the derivation id — and, for
// a derived edge, offers a deterministic recompute. An asserted edge shows its
// plain endpoints and no producer, so a reader can see at a glance that it was
// stated rather than inferred.
function renderDerivationInspector(edge) {
  const selection = $("selection");
  selection.classList.remove("muted");
  const properties = decodedEdgeProperties(edge);
  const derived = edgeIsDerived(edge);
  const from = state.nodes.get(edge.from);
  const to = state.nodes.get(edge.to);
  const entries = [
    ["Edge", `${nodeName(from, edge.from)} → ${nodeName(to, edge.to)}`],
    ["Type", edge.type],
    [
      "Origin",
      derived
        ? "derived (latent similarity)"
        : properties[EDGE_PROPERTY_ORIGIN] || "asserted",
    ],
  ];
  if (!derived) {
    selection.replaceChildren(kvBlock(entries));
    return;
  }
  const score = edgeDerivationScore(edge);
  if (score !== null) entries.push(["Similarity score", score]);
  if (properties[EDGE_PROPERTY_DERIVATION_ID]) {
    entries.push(["Derivation ID", properties[EDGE_PROPERTY_DERIVATION_ID]]);
  }
  entries.push(["Assertion ID", edge.id]);
  const assertion = state.assertions.get(edge.id);
  const derivation =
    assertion && assertion.evidence && assertion.evidence[0]
      ? assertion.evidence[0].derivation
      : null;
  if (derivation) {
    if (derivation.embedding_model) {
      entries.push(["Embedding model", derivationModelLabel(derivation)]);
    }
    if (derivation.similarity_metric) {
      entries.push(["Similarity metric", derivation.similarity_metric]);
    }
    if (derivation.threshold !== undefined && derivation.threshold !== null) {
      entries.push(["Threshold", derivation.threshold]);
    }
    if (derivation.tessellation_cell) {
      entries.push(["Tessellation cell", derivation.tessellation_cell]);
    }
    if (derivation.iterator_name) {
      entries.push(["Iterator", derivation.iterator_name]);
    }
    const options = decodeMetadataList(derivation.iterator_options);
    if (options) entries.push(["Iterator options", options]);
  }
  if (assertion && assertion.provenance && assertion.provenance.provider) {
    entries.push(["Producer", providerLabel(assertion.provenance)]);
  }
  const inspector = document.createElement("div");
  inspector.className = "derivation-inspector";
  inspector.append(kvBlock(entries), buildRecomputeControl(edge.id));
  selection.replaceChildren(inspector);
}

function derivationModelLabel(derivation) {
  return derivation.embedding_model_version
    ? `${derivation.embedding_model} (${derivation.embedding_model_version})`
    : derivation.embedding_model;
}

function providerLabel(provenance) {
  return [provenance.provider, provenance.model, provenance.model_version]
    .filter(Boolean)
    .join(" · ");
}

// decodeMetadataList renders a wire metadata list (base64url key/value pairs)
// as a readable "key=value" summary for the inspector.
function decodeMetadataList(entries) {
  if (!entries || entries.length === 0) return "";
  return entries
    .map((entry) => `${base64UrlDecode(entry.key)}=${base64UrlDecode(entry.value)}`)
    .join(", ");
}

// buildRecomputeControl renders the recompute affordance. The first recompute
// of an edge captures its digest; a second recompute confirms the derivation
// reproduces that digest byte-for-byte, which is the deterministic guarantee
// the feature exists to surface.
function buildRecomputeControl(assertionID) {
  const wrap = document.createElement("div");
  wrap.className = "recompute";
  const button = document.createElement("button");
  button.type = "button";
  button.className = "recompute-button";
  button.textContent = "Recompute derivation";
  const status = document.createElement("p");
  status.className = "recompute-status muted";
  status.setAttribute("role", "status");
  status.textContent = state.derivationDigests.has(assertionID)
    ? "Recompute again to confirm this derivation still reproduces byte-identically."
    : "Recompute re-runs the derivation and captures its deterministic digest.";
  button.onclick = () => recomputeDerivation(assertionID, button, status);
  wrap.append(button, status);
  return wrap;
}

async function recomputeDerivation(assertionID, button, status) {
  button.disabled = true;
  status.className = "recompute-status muted";
  status.textContent = "Recomputing…";
  const priorDigest = state.derivationDigests.get(assertionID) || "";
  try {
    const response = await api("derivation/recompute", {
      snapshot: state.snapshot,
      assertion_id: assertionID,
      digest: priorDigest,
    });
    pin(response.snapshot);
    state.derivationDigests.set(assertionID, response.digest);
    if (!priorDigest) {
      status.className = "recompute-status muted";
      status.textContent =
        `Captured digest ${shortLabel(response.digest)}. ` +
        "Recompute again to verify it reproduces byte-identically.";
    } else if (response.unchanged) {
      status.className = "recompute-status ok";
      status.textContent =
        `Reproduced byte-identically — digest ${shortLabel(response.digest)} unchanged.`;
    } else {
      status.className = "recompute-status changed";
      const score = response.detail ? response.detail.score : "?";
      status.textContent =
        `Inputs changed — new digest ${shortLabel(response.digest)}, ` +
        `similarity score is now ${score}.`;
    }
  } catch (error) {
    if (needsReauthentication(error)) {
      handleReauthentication();
      status.className = "recompute-status muted";
      status.textContent = reauthenticationText();
    } else if (isDenied(error)) {
      status.className = "recompute-status error";
      status.textContent = deniedText("recompute this derivation", state.identity);
    } else if (error && error.code === "unavailable") {
      status.className = "recompute-status muted";
      status.textContent = "Recompute is unavailable on this workspace.";
    } else if (error && (error.status === 404 || error.code === "not_found")) {
      status.className = "recompute-status changed";
      status.textContent =
        "Inputs changed — this derivation no longer reproduces at the current snapshot.";
    } else {
      status.className = "recompute-status error";
      status.textContent = `Recompute failed: ${error.message || String(error)}`;
    }
  } finally {
    button.disabled = false;
  }
}

function updateContinueButton() {
  $("continue-expansion").hidden =
    !capability("neighborhood") ||
    !state.selected ||
    !state.graphCursors.has(state.selected);
}

function shortLabel(value) {
  const text = String(value || "");
  return text.length > 42 ? `${text.slice(0, 20)}…${text.slice(-18)}` : text;
}

const canvas = $("canvas");
const context = canvas.getContext("2d");
const positions = new Map();

function draw() {
  const rectangle = canvas.getBoundingClientRect();
  if (rectangle.width === 0 || rectangle.height === 0) return;
  const ratio = devicePixelRatio || 1;
  canvas.width = rectangle.width * ratio;
  canvas.height = rectangle.height * ratio;
  context.setTransform(ratio, 0, 0, ratio, 0, 0);
  const nodes = [...state.nodes.values()];
  const centerX = rectangle.width / 2;
  const centerY = rectangle.height / 2;
  const radius = Math.min(centerX, centerY) * 0.7;
  nodes.forEach((node, index) => {
    if (!positions.has(node.id)) {
      const angle = 2 * Math.PI * index / Math.max(nodes.length, 1);
      positions.set(node.id, {
        x: centerX + Math.cos(angle) * radius,
        y: centerY + Math.sin(angle) * radius,
      });
    }
  });
  context.clearRect(0, 0, rectangle.width, rectangle.height);
  if (nodes.length === 0) return;
  for (const edge of state.edges.values()) {
    const from = positions.get(edge.from);
    const to = positions.get(edge.to);
    if (!from || !to) continue;
    const derived = edgeIsDerived(edge);
    const selected = edge.id === state.selectedEdge;
    context.save();
    // Derived edges are dashed and amber so a latent similarity assertion is
    // never mistaken for an asserted relationship at a glance.
    context.setLineDash(derived ? [6, 4] : []);
    context.lineWidth = selected ? 2.5 : 1;
    context.strokeStyle = derived ? "#f6c667" : selected ? "#ffd166" : "#536a8d";
    context.beginPath();
    context.moveTo(from.x, from.y);
    context.lineTo(to.x, to.y);
    context.stroke();
    context.restore();
    const score = derived ? edgeDerivationScore(edge) : null;
    const label = score !== null ? `${edge.type} ~${score}` : edge.type;
    context.fillStyle = derived ? "#f6c667" : "#8fa3bf";
    context.fillText(shortLabel(label), (from.x + to.x) / 2, (from.y + to.y) / 2);
  }
  for (const node of nodes) {
    const position = positions.get(node.id);
    context.beginPath();
    context.arc(position.x, position.y, node.id === state.selected ? 10 : 7, 0, Math.PI * 2);
    context.fillStyle = node.id === state.selected ? "#ffd166" : "#55c2ff";
    context.fill();
    context.fillStyle = "#e5edf8";
    context.fillText(shortLabel(nodeName(node, node.id)), position.x + 11, position.y + 4);
  }
}

canvas.onclick = (event) => {
  const rectangle = canvas.getBoundingClientRect();
  const x = event.clientX - rectangle.left;
  const y = event.clientY - rectangle.top;
  let hit = null;
  let distance = 18;
  for (const [id, position] of positions) {
    const candidate = Math.hypot(position.x - x, position.y - y);
    if (candidate < distance) {
      hit = id;
      distance = candidate;
    }
  }
  selectNode(hit);
};

new ResizeObserver(() => {
  positions.clear();
  draw();
}).observe(canvas);
renderGraphList();
applyCapabilities();

function wirePath(workspaceID, suffix) {
  return `/api/v1/workspaces/${encodeURIComponent(workspaceID)}/settings${suffix || ""}`;
}

async function jsonRequest(path, options = {}) {
  const headers = {"accept": "application/json", ...authHeaders(), ...(options.headers || {})};
  const response = await fetch(path, {...options, headers});
  const value = await response.json();
  if (!response.ok) throw apiError(value, response);
  return value;
}

function currentWorkspaceID() {
  const input = document.getElementById("workspace-id");
  return String(input && input.value || "").trim();
}

function setWorkspaceID(value) {
  state.workspaceID = String(value || "").trim();
}

function resetIdentityBoundUI() {
  state.workspaceID = "";
  state.workspaceSettings = null;
  state.lensChoices = null;
  if (typeof window !== "undefined" && window.ShoalTeamOverview &&
      typeof window.ShoalTeamOverview.reset === "function") {
    window.ShoalTeamOverview.reset();
  }
  const workspace = document.getElementById("workspace-id");
  if (workspace) workspace.value = "";
  for (const id of ["chat-results", "provenance-results"]) {
    const element = document.getElementById(id);
    if (element) element.replaceChildren();
  }
  const settings = document.getElementById("settings-json");
  if (settings) settings.value = "{}";
  const lens = document.getElementById("lens-select");
  if (lens) lens.replaceChildren();
}

function logout() {
  state.accessToken = null;
  state.identity = null;
  state.reauthRequired = false;
  clearLoginFlow();
  resetIdentityBoundUI();
  configureLoginUI(state.auth);
  loadIdentity();
}

function appendText(parent, tag, text, className) {
  const element = document.createElement(tag);
  if (className) element.className = className;
  element.textContent = String(text === undefined || text === null ? "" : text);
  parent.append(element);
  return element;
}

function displayOpaqueID(value) {
  const encoded = String(value || "");
  if (!encoded) return "(none)";
  try {
    return `${encoded} (${base64UrlDecode(encoded)})`;
  } catch (_) {
    return encoded;
  }
}

function claimText(claim) {
  const value = claim && claim.object;
  if (!value) return "(structured claim)";
  if (Object.prototype.hasOwnProperty.call(value, "string")) return value.string;
  if (Object.prototype.hasOwnProperty.call(value, "id")) return displayOpaqueID(value.id);
  return JSON.stringify(value);
}

function safeSourceHref(value) {
  const source = String(value || "");
  try {
    const parsed = new URL(source, window.location.href);
    return parsed.protocol === "http:" || parsed.protocol === "https:" ? parsed.href : "";
  } catch (_) {
    return "";
  }
}

function renderCitation(parent, evidence) {
  const item = document.createElement("li");
  const verified = evidence && evidence.verification_status === "verified";
  item.className = `citation-item ${verified ? "verified" : "unverified"}`;
  appendText(item, "strong", `${verified ? "Verified" : "Unverified"} ${evidence.use || "evidence"}`);
  appendText(item, "div", `Anchor: ${displayOpaqueID(evidence.anchor_id)}`, "provenance-meta");
  const citation = evidence.citation;
  if (citation) {
    appendText(item, "div", `Document: ${displayOpaqueID(citation.document_id)}`, "provenance-meta");
    appendText(item, "div", `Revision: ${displayOpaqueID(citation.revision_id)}`, "provenance-meta");
    if (citation.section_id) appendText(item, "div", `Section: ${displayOpaqueID(citation.section_id)}`, "provenance-meta");
    if (citation.span_id) appendText(item, "div", `Span: ${displayOpaqueID(citation.span_id)}`, "provenance-meta");
  }
  if (evidence.source_uri) {
    const href = safeSourceHref(evidence.source_uri);
    if (href) {
      const link = document.createElement("a");
      link.textContent = evidence.source_uri;
      link.href = href;
      link.rel = "noreferrer";
      item.append(link);
    } else {
      appendText(item, "div", `Source: ${evidence.source_uri}`, "provenance-meta");
    }
  }
  appendText(item, "div", `Sources: ${(evidence.source_ids || []).map(displayOpaqueID).join(", ")}`, "provenance-meta");
  appendText(item, "div", `Visibility: ${(evidence.visibility || []).join(" & ") || "public"}`, "provenance-meta");
  if (evidence.quote) appendText(item, "q", evidence.quote, "citation-quote");
  parent.append(item);
}

function renderChatResponse(value) {
  const results = document.getElementById("chat-results");
  results.replaceChildren();
  const verified = value && value.finalized === true &&
    value.durably_recorded === true && value.verification === "verified";
  const card = document.createElement("article");
  card.className = `chat-response ${verified ? "verified" : "unverified"}`;
  appendText(card, "h3", verified
    ? "Verified and durably recorded response"
    : "Response not verified");
  appendText(card, "p", `Output label: ${value.output_visibility || "public"}`);
  const interpretation = value.ontology_interpretation || {status: "unresolved"};
  appendText(card, "p", interpretation.status === "selected"
    ? `Ontology interpretation: selected ${displayOpaqueID(interpretation.schema_id)} / ${displayOpaqueID(interpretation.version_id)}`
    : "Ontology interpretation: unresolved; no trusted lens was applied.");
  appendText(card, "p", `Session: ${displayOpaqueID(value.session_id)}`, "provenance-meta");
  for (const claim of value.claims || []) {
    const claimCard = document.createElement("section");
    appendText(claimCard, "h4", claimText(claim), "chat-answer");
    appendText(claimCard, "p", `Status: ${claim.status}; confidence: ${claim.confidence}`);
    const citations = document.createElement("ol");
    citations.className = "citation-list";
    for (const evidence of claim.citations || []) renderCitation(citations, evidence);
    for (const evidence of claim.derived_evidence || []) renderCitation(citations, evidence);
    claimCard.append(citations);
    card.append(claimCard);
  }
  for (const issue of value.issues || []) {
    appendText(card, "p", `${issue.kind}: ${issue.reason || issue.input}`, "unverified");
  }
  const sources = document.createElement("details");
  const sourceSummary = document.createElement("summary");
  sourceSummary.textContent = `Complete touched source set (${(value.sources || []).length})`;
  const sourceList = document.createElement("ul");
  sourceList.className = "source-list";
  for (const source of value.sources || []) {
    appendText(sourceList, "li",
      `${displayOpaqueID(source.id)} · anchors ${(source.anchor_ids || []).map(displayOpaqueID).join(", ")} · visibility ${(source.visibility || []).join(" & ") || "public"}`);
  }
  sources.append(sourceSummary, sourceList);
  card.append(sources);
  results.append(card);
}

async function submitChat() {
  const status = document.getElementById("chat-status");
  const question = String(document.getElementById("chat-question").value || "").trim();
  if (!question) {
    setStatus(status, "Enter a question.", "empty-state");
    return;
  }
  setWorkspaceID(currentWorkspaceID());
  setStatus(status, "Retrieving authorized evidence, reasoning, verifying, and recording…");
  document.getElementById("chat-send").disabled = true;
  try {
    const topK = Number(document.getElementById("chat-top-k").value) || 0;
    const value = await jsonRequest("/api/v1/ask", {
      method: "POST",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({question, top_k: topK}),
    });
    renderChatResponse(value);
    setStatus(status, "Final response received after durable provenance capture.");
    await loadProvenance();
  } catch (error) {
    showActionError(status, error, "ask the workspace");
  } finally {
    document.getElementById("chat-send").disabled = false;
  }
}

async function loadWorkspaceSettings() {
  const status = document.getElementById("settings-status");
  const workspaceID = currentWorkspaceID();
  if (!workspaceID) {
    setStatus(status, "Enter a workspace wire ID.", "empty-state");
    return;
  }
  setWorkspaceID(workspaceID);
  try {
    const settings = await jsonRequest(wirePath(workspaceID));
    const lenses = await jsonRequest(wirePath(workspaceID, "/lens"));
    state.workspaceSettings = settings;
    state.lensChoices = lenses;
    document.getElementById("settings-json").value =
      JSON.stringify(settings.settings || {}, null, 2);
    renderLensChoices(lenses);
    setStatus(status, `Loaded revision ${settings.revision}.`);
  } catch (error) {
    showActionError(status, error, "read workspace settings");
  }
}

function renderLensChoices(value) {
  const select = document.getElementById("lens-select");
  select.replaceChildren();
  for (const choice of value.choices || []) {
    const option = document.createElement("option");
    option.value = JSON.stringify({
      schema_id: choice.schema_id,
      version_id: choice.version_id,
    });
    option.textContent = `${displayOpaqueID(choice.schema_id)} / ${displayOpaqueID(choice.version_id)}${choice.active ? " (active)" : ""}`;
    const selected = value.selected_ontology;
    option.selected = Boolean(selected &&
      selected.schema_id === choice.schema_id &&
      selected.version_id === choice.version_id);
    select.append(option);
  }
}

async function saveWorkspaceSettings() {
  const status = document.getElementById("settings-status");
  if (!state.workspaceSettings) {
    setStatus(status, "Load settings before saving.", "empty-state");
    return;
  }
  try {
    const narrowing = JSON.parse(document.getElementById("settings-json").value || "{}");
    const value = await jsonRequest(wirePath(state.workspaceID), {
      method: "PUT",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({
        expected_revision: state.workspaceSettings.revision,
        mutation_id: randomUrlToken(24),
        settings: narrowing,
      }),
    });
    state.workspaceSettings = value;
    setStatus(status, `Saved revision ${value.revision}.`);
    await loadWorkspaceSettings();
  } catch (error) {
    showActionError(status, error, "update workspace settings");
  }
}

async function saveLens() {
  const status = document.getElementById("settings-status");
  const select = document.getElementById("lens-select");
  if (!state.workspaceSettings || !select.value) {
    setStatus(status, "Load settings and choose a trusted lens first.", "empty-state");
    return;
  }
  try {
    const value = await jsonRequest(wirePath(state.workspaceID, "/lens"), {
      method: "PUT",
      headers: {"content-type": "application/json"},
      body: JSON.stringify({
        expected_revision: state.workspaceSettings.revision,
        mutation_id: randomUrlToken(24),
        selected_ontology: JSON.parse(select.value),
      }),
    });
    state.workspaceSettings = value;
    setStatus(status, `Selected lens at revision ${value.revision}.`);
    await loadWorkspaceSettings();
  } catch (error) {
    showActionError(status, error, "select the ontology lens");
  }
}

async function inspectProvenance(sessionID, target) {
  try {
    const value = await jsonRequest(`/api/v1/provenance/${encodeURIComponent(sessionID)}`);
    appendText(target, "p", `Model: ${value.model.provider || "unknown"} / ${value.model.model || "unknown"}`);
    appendText(target, "p", `Retrieved: ${(value.retrieved_source_ids || []).map(displayOpaqueID).join(", ") || "none"}`, "provenance-meta");
    appendText(target, "p", `Cited: ${(value.cited_source_ids || []).map(displayOpaqueID).join(", ") || "none"}`, "provenance-meta");
    appendText(target, "p", `Turns: ${(value.turns || []).length}; stop: ${value.stop_reason || "unknown"}`);
  } catch (error) {
    appendText(target, "p", error.message || String(error), "error");
  }
}

async function foldSelectedProvenance(container) {
  const selected = [...container.querySelectorAll("input")]
    .filter((input) => input.checked)
    .map((input) => input.value);
  if (!selected.length) throw new Error("Select at least one interaction to fold.");
  const value = await jsonRequest("/api/v1/provenance/fold", {
    method: "POST",
    headers: {"content-type": "application/json"},
    body: JSON.stringify({session_ids: selected}),
  });
  return value;
}

// A provenance record's output label is shown only to the principal that
// recorded it until per-reader label evaluation lands (#564, #567, #568).
// Every other reader receives it empty, exactly as for a record with no label,
// so an empty label is rendered as nothing at all: calling it "public" would
// misstate a withheld label, and any other wording would mark it.
function outputLabelClause(value) {
  return value ? `; output: ${value}` : "";
}

function appendProvenanceCards(results, value, before = null) {
  const append = (card) => before ? results.insertBefore(card, before) : results.append(card);
  for (const item of value.interactions || []) {
    const card = document.createElement("article");
    card.className = "provenance-card";
    const label = document.createElement("label");
    const checkbox = document.createElement("input");
    checkbox.type = "checkbox";
    checkbox.value = item.session_id;
    label.append(checkbox);
    appendText(label, "span", ` ${item.operation} · ${new Date(item.recorded_at).toLocaleString()}`);
    card.append(label);
    appendText(card, "p", `Session: ${displayOpaqueID(item.session_id)}`, "provenance-meta");
    appendText(card, "p", `Actor: ${displayOpaqueID(item.actor && item.actor.actor_id)}${outputLabelClause(item.output_visibility)}`, "provenance-meta");
    const inspect = document.createElement("button");
    inspect.type = "button";
    inspect.textContent = "Inspect exact provenance";
    inspect.onclick = () => inspectProvenance(item.session_id, card);
    card.append(inspect);
    append(card);
  }
  for (const item of value.folds || []) {
    const card = document.createElement("article");
    card.className = "provenance-card";
    appendText(card, "strong", `Native provenance fold (${item.member_count} sessions)`);
    appendText(card, "p", `Fold: ${displayOpaqueID(item.fold_id)}${outputLabelClause(item.output_visibility)}`, "provenance-meta");
    const unfold = document.createElement("button");
    unfold.type = "button";
    unfold.textContent = "Unfold exact members";
    unfold.onclick = async () => {
      try {
        const hydrated = await jsonRequest("/api/v1/provenance/unfold", {
          method: "POST",
          headers: {"content-type": "application/json"},
          body: JSON.stringify({fold_id: item.fold_id}),
        });
        appendText(card, "p", (hydrated.members || [])
          .map((member) => displayOpaqueID(member.session_id)).join(", "), "provenance-meta");
      } catch (error) {
        appendText(card, "p", error.message || String(error), "error");
      }
    };
    card.append(unfold);
    append(card);
  }
}

async function loadProvenance() {
  const status = document.getElementById("provenance-status");
  const results = document.getElementById("provenance-results");
  if (!status || !results) return;
  setWorkspaceID(currentWorkspaceID());
  try {
    const value = await jsonRequest("/api/v1/provenance");
    results.replaceChildren();
    const controls = document.createElement("div");
    controls.className = "button-row";
    const fold = document.createElement("button");
    fold.type = "button";
    fold.textContent = "Fold selected provenance";
    fold.onclick = async () => {
      try {
        const created = await foldSelectedProvenance(results);
        setStatus(status, `Fold ${displayOpaqueID(created.fold_id)} ${created.created ? "created" : "already existed"}.`);
        await loadProvenance();
      } catch (error) {
        showActionError(status, error, "fold provenance");
      }
    };
    controls.append(fold);
    results.append(controls);
    appendProvenanceCards(results, value);
    let interactions = (value.interactions || []).length;
    let folds = (value.folds || []).length;
    if (value.next_cursor) {
      const more = document.createElement("button");
      more.type = "button";
      more.textContent = "More provenance";
      let cursor = value.next_cursor;
      more.onclick = async () => {
        more.disabled = true;
        try {
          const next = await jsonRequest(`/api/v1/provenance?cursor=${encodeURIComponent(cursor)}`);
          appendProvenanceCards(results, next, more);
          interactions += (next.interactions || []).length;
          folds += (next.folds || []).length;
          cursor = next.next_cursor || "";
          more.hidden = !cursor;
          setStatus(status, `Loaded ${interactions} interaction(s) and ${folds} fold(s).`);
        } catch (error) {
          showActionError(status, error, "load more provenance");
        } finally {
          more.disabled = false;
        }
      };
      results.append(more);
    }
    setStatus(status, `Loaded ${interactions} interaction(s) and ${folds} fold(s).`);
  } catch (error) {
    results.replaceChildren();
    showActionError(status, error, "inspect provenance");
  }
}

function installProductChatUI() {
  const chatForm = document.getElementById("chat-form");
  if (chatForm) chatForm.onsubmit = async (event) => {
    event.preventDefault();
    await submitChat();
  };
  const load = document.getElementById("settings-load");
  if (load) load.onclick = loadWorkspaceSettings;
  const save = document.getElementById("settings-save");
  if (save) save.onclick = saveWorkspaceSettings;
  const lens = document.getElementById("lens-save");
  if (lens) lens.onclick = saveLens;
  const refresh = document.getElementById("provenance-refresh");
  if (refresh) refresh.onclick = loadProvenance;
  const logoutButton = document.getElementById("logout");
  if (logoutButton) logoutButton.onclick = logout;
}
installProductChatUI();

// bootstrap resolves any login before the first identity/data calls so those
// calls carry a bearer token when one is configured. With -dev-auth the config
// reports unconfigured, no login UI renders, and the sequence is the same as
// before: identity, then metadata, then documents.
async function bootstrap() {
  const config = await loadAuthConfig();
  state.auth = config;
  configureLoginUI(config);
  if (config.configured) {
    try {
      await completeLoginFromRedirect(config);
    } catch (error) {
      renderLoginError(error);
    }
  }
  loadIdentity();
  loadOntology();
  loadMeta()
    .then(() => loadDocuments())
    .catch((error) => showError($("documents"), error));
}
bootstrap();
