// graph-table.js: the Graph panel's described-table source. When the host
// mounts /api/v1/graph-table (a graph laid out in any engine table and read
// through its recorded descriptor), this adds a toolbar to the Graph panel:
// a live record picker, a term lookup over the table's postings, and the
// table's ontology. Results render on the same
// canvas and lists as the workspace graph. Loaded after app.js and relies on
// its state, $, authHeaders, apiError, mergeGraph, draw, activate,
// selectNode, setGraphSource, decodedNodeProperties, graphGeneration and capability.
const graphTable = (() => {
  const ROOT = "/api/v1/graph-table";
  let description = null;
  let generation = 0;
  let browseGeneration = 0;
  let next = "";
  let browsedFilter = null;

  function el(id) {
    return document.getElementById(id);
  }

  function active() {
    const toggle = el("graph-table-active");
    return Boolean(description && toggle && toggle.checked);
  }

  function status(text, isError = false) {
    const node = el("graph-table-status");
    if (!node) return;
    node.textContent = text;
    node.className = isError ? "error" : "muted";
  }

  async function request(path, body) {
    const init = {headers: {"accept": "application/json", ...authHeaders()}};
    if (body !== undefined) {
      init.method = "POST";
      init.headers["content-type"] = "application/json";
      init.body = JSON.stringify(body);
    }
    const response = await fetch(`${ROOT}/${path}`, init);
    const value = await response.json();
    if (!response.ok) throw apiError(value, response);
    return value;
  }

  function describeStores(stores) {
    if (!stores || stores.length === 0) return "no store";
    return stores
      .map((store) => {
        if (store.address === "embedded") return "this workspace's engine";
        const range = store.start || store.end ? ` [${store.start || ""}..${store.end || ""})` : "";
        return `${store.address}${range}`;
      })
      .join(", ");
  }

  function renderOntology(ontology) {
    const body = el("graph-table-ontology-body");
    if (!body) return;
    body.replaceChildren();
    const list = document.createElement("dl");
    list.className = "kv";
    const add = (key, value) => {
      const k = document.createElement("dt");
      k.textContent = key;
      const v = document.createElement("dd");
      v.textContent = value;
      list.append(k, v);
    };
    for (const ns of ontology.namespaces || []) {
      add(`namespace ${ns.name}`, `rows ${ns.row_prefix}…${ns.kinds && ns.kinds.length ? ` kinds: ${ns.kinds.join(", ")}` : ""}`);
    }
    for (const edge of ontology.edges || []) {
      const rels = edge.relationships && edge.relationships.length ? ` relationships: ${edge.relationships.join(", ")}` : " any relationship";
      add(`edge ${edge.name}`, `family ${edge.column_family}${rels}${edge.weighted ? ", weighted" : ""}${edge.target_prefix ? `, targets ${edge.target_prefix}…` : ""}`);
    }
    for (const posting of ontology.postings || []) {
      add(`postings ${posting.name}`, `rows ${posting.term_row_prefix}<term>`);
    }
    for (const vector of ontology.vectors || []) {
      add(`vectors ${vector.name}`, `space ${vector.embedding_space}${vector.metric ? `, ${vector.metric}` : ""}`);
    }
    body.append(list);
  }

  async function apply() {
    const section = el("graph-table");
    if (!section) return;
    if (!capability("graph_table")) {
      section.hidden = true;
      description = null;
      if (graphSource === "table") setGraphSource("workspace");
      return;
    }
    const mine = ++generation;
    try {
      const value = await request("");
      if (mine !== generation) return;
      description = value;
      section.hidden = false;
      // A table-only Explorer should open on its actual graph source.
      if (!capability("neighborhood")) setGraphSource("table");
      el("graph-table-source-option").hidden = !capability("neighborhood");
      el("graph-table-term-controls").hidden = !(value.ontology.postings || []).length;
      el("graph-table-summary").textContent = "Choose a record to see its connections in the live graph.";
      el("graph-table-depth").max = String(value.bounds.max_depth);
      renderOntology(value.ontology);
      const spaces = el("graph-table-namespace");
      spaces.replaceChildren();
      for (const ns of value.ontology.namespaces || []) {
        const option = document.createElement("option");
        option.value = ns.name;
        option.textContent = ns.name;
        spaces.append(option);
      }
      // Foreman has many numbered action rows; start its customer journey
      // with named projects while leaving every kind available to browse.
      if (value.schema_id === "foreman.graph") el("graph-table-kind").value = "project";
      status("Choose a live record to see its first hop.");
      const linked = linkedRecord();
      if (linked) await loadId(linked);
      else await browse();
    } catch (error) {
      if (mine !== generation) return;
      description = null;
      // The API conceals the route from readers without its label; the UI
      // must not reveal that a table is mounted either.
      section.hidden = error.status === 404;
      if (section.hidden && graphSource === "table") setGraphSource("workspace");
      el("graph-table-ontology-body").replaceChildren();
      if (section.hidden) {
        status("");
      } else {
        el("graph-table-summary").textContent = "The graph source could not be loaded.";
        status(error.message || String(error), true);
      }
    }
  }

  function namespace(name) {
    return (description?.ontology.namespaces || []).find((item) => item.name === name);
  }

  function linkedRecord() {
    const params = new URLSearchParams(window.location.search);
    const ns = namespace(params.get("graph_namespace"));
    const kind = params.get("graph_kind");
    const identifier = params.get("graph_id");
    if (!ns || !identifier || identifier.length > 4096) return "";
    if (!ns.kind_sep) return ns.row_prefix + identifier;
    if ((ns.kind_index || 0) !== 0 || !kind || kind.length > 128 || kind.includes(ns.kind_sep)) return "";
    return ns.row_prefix + kind + ns.kind_sep + identifier;
  }

  function select(node) {
    const link = el("graph-table-link");
    if (!link) return;
    link.hidden = true;
    if (!node || !description) return;
    const ns = (description.ontology.namespaces || []).find((item) => node.id.startsWith(item.row_prefix));
    if (!ns) return;
    const key = node.id.slice(ns.row_prefix.length);
    let kind = node.kind;
    let identifier = key;
    if (ns.kind_sep) {
      if ((ns.kind_index || 0) !== 0) return;
      const cut = key.indexOf(ns.kind_sep);
      if (cut < 1) return;
      kind = key.slice(0, cut);
      identifier = key.slice(cut + ns.kind_sep.length);
    }
    if (!identifier) return;
    const url = new URL(window.location.href);
    url.searchParams.set("view", "graph");
    url.searchParams.set("graph_namespace", ns.name);
    url.searchParams.set("graph_kind", kind);
    url.searchParams.set("graph_id", identifier);
    link.href = url.toString();
    link.hidden = false;
  }

  async function browse(more = false) {
    if (!description) return;
    const filter = more ? browsedFilter : {
      namespace: el("graph-table-namespace").value,
      kind: el("graph-table-kind").value.trim(),
      identifier_prefix: el("graph-table-identifier").value.trim(),
    };
    if (!filter) return;
    const kind = filter.kind;
    const identifierPrefix = filter.identifier_prefix;
    if (identifierPrefix && !kind) {
      status("Choose a kind before filtering by identifier.", true);
      return;
    }
    const mine = ++browseGeneration;
    const after = more ? next : "";
    if (!more) {
      next = "";
      browsedFilter = filter;
      el("graph-table-results").replaceChildren();
    }
    status("Browsing live records…");
    try {
      const response = await request("browse", {
        ...filter, after, limit: 24,
      });
      if (mine !== browseGeneration) return;
      next = response.next || "";
      el("graph-table-more").hidden = !next;
      const results = el("graph-table-results");
      for (const node of response.nodes || []) {
        const button = document.createElement("button");
        button.type = "button";
        const title = (node.labels && node.labels[0]) || node.kind;
        button.textContent = `${node.kind} · ${title}`;
        button.setAttribute("aria-label", `Open ${node.kind} ${title}`);
        button.onclick = () => loadId(node.id);
        results.append(button);
      }
      status(response.nodes.length ? "Select a record to load its first hop." : "No records match this filter.");
    } catch (error) {
      if (mine !== browseGeneration) return;
      status(`Record browse failed: ${error.message || String(error)}`, true);
    }
  }

  function depth() {
    const value = Number(el("graph-table-depth").value);
    return Number.isInteger(value) && value > 0 ? value : 1;
  }

  // A table read is discarded when the workspace graph was cleared or
  // another read superseded it: both app.js's graphGeneration and this
  // module's own generation must still match.
  function stale(mine, graphMine) {
    return mine !== generation || graphMine !== graphGeneration;
  }

  async function expand(ids) {
    if (!description) return;
    const mine = ++generation;
    const graphMine = graphGeneration;
    status(`Expanding ${ids.length} node(s) ${depth()} hop(s) through the table…`);
    try {
      const response = await request("neighborhood", {
        node_ids: ids, depth: depth(), fanout: 18, max_nodes: 120,
      });
      if (stale(mine, graphMine)) return;
      mergeGraph(response.neighborhood, "table");
      const dropped = response.dropped_edges ? ` ${response.dropped_edges} edge(s) point at rows the reader cannot name and were dropped.` : "";
      $("graph-status").textContent = (response.truncated
        ? `Table ${response.table}: expansion truncated by fanout, node budget, or a row's cell bound.`
        : `Table ${response.table}: complete expansion.`) + dropped;
      status(`${response.neighborhood.nodes.length} node(s), ${response.neighborhood.edges.length} edge(s) from ${response.table}.`);
      activate("graph");
      draw();
    } catch (error) {
      if (stale(mine, graphMine)) return;
      status(`Expansion failed: ${error.message || String(error)}`, true);
    }
  }

  async function path(from, to) {
    if (!description) return;
    const mine = ++generation;
    const graphMine = graphGeneration;
    status(`Finding a directed path in the table…`);
    try {
      const response = await request("path", {from, to, max_depth: 4, fanout: 25});
      if (stale(mine, graphMine)) return;
      if (!response.found) {
        $("graph-status").textContent = `Table ${response.table}: no directed path within 4 hops.`;
        status("No path found.");
        return;
      }
      mergeGraph(response.path, "table");
      $("graph-status").textContent = `Table ${response.table}: directed path of ${response.path.edges.length} edge(s).`;
      status(`Path of ${response.path.edges.length} edge(s).`);
      draw();
    } catch (error) {
      if (stale(mine, graphMine)) return;
      status(`Path finding failed: ${error.message || String(error)}`, true);
    }
  }

  async function loadId(id) {
    if (!id || !description) return;
    ++browseGeneration;
    el("graph-table-active").checked = true;
    setGraphSource("table");
    const mine = ++generation;
    const graphMine = graphGeneration;
    status("Loading record and its connections…");
    try {
      const response = await request("nodes", {ids: [id]});
      if (stale(mine, graphMine)) return;
      const node = response.nodes[0];
      const found = node && decodedNodeProperties(node).found === "true";
      if (!found) {
        status("That record is no longer in the live table.");
        return;
      }
      mergeGraph({nodes: response.nodes, edges: []}, "table");
      activate("graph");
      selectNode(id);
      await expand([id]);
    } catch (error) {
      if (stale(mine, graphMine)) return;
      status(`Record load failed: ${error.message || String(error)}`, true);
    }
  }

  async function find() {
    const term = el("graph-table-term").value.trim();
    if (!term || !description) return;
    el("graph-table-active").checked = true;
    setGraphSource("table");
    const mine = ++generation;
    const graphMine = graphGeneration;
    status(`Looking up "${term}" in the table's postings…`);
    try {
      const response = await request("terms", {terms: [term]});
      if (stale(mine, graphMine)) return;
      const ids = (response.hits[0] && response.hits[0].node_ids) || [];
      if (ids.length === 0) {
        status(`No node is posted under "${term}".`);
        return;
      }
      const nodes = await request("nodes", {ids: ids.slice(0, 64)});
      if (stale(mine, graphMine)) return;
      mergeGraph({nodes: nodes.nodes, edges: []}, "table");
      status(`${ids.length} node(s) posted under "${term}".`);
      activate("graph");
      selectNode(ids[0]);
    } catch (error) {
      if (stale(mine, graphMine)) return;
      status(`Term lookup failed: ${error.message || String(error)}`, true);
    }
  }

  function bind() {
    const findButton = el("graph-table-find");
    if (findButton) findButton.onclick = find;
    const form = el("graph-table-browse-form");
    if (form) form.onsubmit = (event) => { event.preventDefault(); browse(); };
    const more = el("graph-table-more");
    if (more) more.onclick = () => browse(true);
    const space = el("graph-table-namespace");
    if (space) space.onchange = () => browse();
    for (const id of ["graph-table-kind", "graph-table-identifier"]) {
      const input = el(id);
      if (input) input.oninput = () => { next = ""; el("graph-table-more").hidden = true; };
    }
    const term = el("graph-table-term");
    if (term) term.onkeydown = (event) => { if (event.key === "Enter") find(); };
    const toggle = el("graph-table-active");
    if (toggle) {
      toggle.onchange = () => {
        setGraphSource(toggle.checked ? "table" : "workspace");
        $("expand").disabled = !state.selected || !(toggle.checked ? description : capability("neighborhood"));
        status(toggle.checked ? "Expansion and path finding now read the described table." : "Expansion and path finding read the workspace graph.");
      };
    }
  }

  bind();
  return {active, apply, expand, path, browse, find, select};
})();
