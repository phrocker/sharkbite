// graph-table.js: the Graph panel's described-table source. When the host
// mounts /api/v1/graph-table (a graph laid out in any engine table and read
// through its recorded descriptor), this adds a toolbar to the Graph panel:
// a switch that makes expansion and path finding read the table instead of
// the workspace graph, a seed loader by row key, a term lookup over the
// table's postings, and the table's ontology. Results render on the same
// canvas and lists as the workspace graph. Loaded after app.js and relies on
// its state, $, authHeaders, apiError, mergeGraph, draw, activate,
// selectNode and capability.
const graphTable = (() => {
  const ROOT = "/api/v1/graph-table";
  let description = null;
  let generation = 0;

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
      return;
    }
    const mine = ++generation;
    try {
      const value = await request("");
      if (mine !== generation) return;
      description = value;
      section.hidden = false;
      el("graph-table-summary").textContent =
        `Table ${value.table} (${value.schema_id}@${value.version}) under label ${value.label}, ` +
        `read from ${describeStores(value.stores)}. Edges are followed outward only.`;
      el("graph-table-depth").max = String(value.bounds.max_depth);
      renderOntology(value.ontology);
      status("Choose the table source, then load a node by row key or find one by term.");
    } catch (error) {
      if (mine !== generation) return;
      description = null;
      // A reader without the table's label is told nothing is mounted;
      // the panel says so rather than showing an empty toolbar.
      section.hidden = false;
      el("graph-table-summary").textContent =
        "A described table is mounted but is not visible to you: its label is not among your grants.";
      el("graph-table-ontology-body").replaceChildren();
      status(error.message || String(error), true);
    }
  }

  function depth() {
    const value = Number(el("graph-table-depth").value);
    return Number.isInteger(value) && value > 0 ? value : 1;
  }

  async function expand(ids) {
    if (!description) return;
    const mine = ++generation;
    status(`Expanding ${ids.length} node(s) ${depth()} hop(s) through the table…`);
    try {
      const response = await request("neighborhood", {
        node_ids: ids, depth: depth(), fanout: 18, max_nodes: 120,
      });
      if (mine !== generation) return;
      mergeGraph(response.neighborhood);
      $("graph-status").textContent = response.truncated
        ? `Table ${response.table}: expansion truncated by fanout or node budget.`
        : `Table ${response.table}: complete expansion.`;
      status(`${response.neighborhood.nodes.length} node(s), ${response.neighborhood.edges.length} edge(s) from ${response.table}.`);
      activate("graph");
      draw();
    } catch (error) {
      if (mine !== generation) return;
      status(`Expansion failed: ${error.message || String(error)}`, true);
    }
  }

  async function path(from, to) {
    if (!description) return;
    const mine = ++generation;
    status(`Finding a directed path in the table…`);
    try {
      const response = await request("path", {from, to, max_depth: 4, fanout: 25});
      if (mine !== generation) return;
      if (!response.found) {
        $("graph-status").textContent = `Table ${response.table}: no directed path within 4 hops.`;
        status("No path found.");
        return;
      }
      mergeGraph(response.path);
      $("graph-status").textContent = `Table ${response.table}: directed path of ${response.path.edges.length} edge(s).`;
      status(`Path of ${response.path.edges.length} edge(s).`);
      draw();
    } catch (error) {
      if (mine !== generation) return;
      status(`Path finding failed: ${error.message || String(error)}`, true);
    }
  }

  async function load() {
    const id = el("graph-table-seed").value.trim();
    if (!id || !description) return;
    el("graph-table-active").checked = true;
    const mine = ++generation;
    status(`Loading ${id}…`);
    try {
      const response = await request("nodes", {ids: [id]});
      if (mine !== generation) return;
      mergeGraph({nodes: response.nodes, edges: []});
      const node = response.nodes[0];
      const found = node && node.properties && node.properties.some((p) => p.key === "found" && p.value === "true");
      status(found ? `Loaded ${id}; expand it to follow its edges.` : `${id} is in the namespace but the table holds no cell for it.`);
      activate("graph");
      selectNode(id);
    } catch (error) {
      if (mine !== generation) return;
      status(`Load failed: ${error.message || String(error)}`, true);
    }
  }

  async function find() {
    const term = el("graph-table-term").value.trim();
    if (!term || !description) return;
    el("graph-table-active").checked = true;
    const mine = ++generation;
    status(`Looking up "${term}" in the table's postings…`);
    try {
      const response = await request("terms", {terms: [term]});
      if (mine !== generation) return;
      const ids = (response.hits[0] && response.hits[0].node_ids) || [];
      if (ids.length === 0) {
        status(`No node is posted under "${term}".`);
        return;
      }
      const nodes = await request("nodes", {ids: ids.slice(0, 64)});
      if (mine !== generation) return;
      mergeGraph({nodes: nodes.nodes, edges: []});
      status(`${ids.length} node(s) posted under "${term}".`);
      activate("graph");
      selectNode(ids[0]);
    } catch (error) {
      if (mine !== generation) return;
      status(`Term lookup failed: ${error.message || String(error)}`, true);
    }
  }

  function bind() {
    const loadButton = el("graph-table-load");
    const findButton = el("graph-table-find");
    if (loadButton) loadButton.onclick = load;
    if (findButton) findButton.onclick = find;
    const seed = el("graph-table-seed");
    if (seed) seed.onkeydown = (event) => { if (event.key === "Enter") load(); };
    const term = el("graph-table-term");
    if (term) term.onkeydown = (event) => { if (event.key === "Enter") find(); };
    const toggle = el("graph-table-active");
    if (toggle) {
      toggle.onchange = () => {
        $("expand").disabled = !state.selected || !(toggle.checked ? description : capability("neighborhood"));
        status(toggle.checked ? "Expansion and path finding now read the described table." : "Expansion and path finding read the workspace graph.");
      };
    }
  }

  bind();
  return {active, apply, expand, path, load, find};
})();
