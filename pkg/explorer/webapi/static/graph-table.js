// graph-table.js: the Graph panel's described-table source. When the host
// mounts /api/v1/graph-table (a graph laid out in any engine table and read
// through its recorded descriptor), this adds a toolbar to the Graph panel:
// a switch that makes expansion and path finding read the table instead of
// the workspace graph, a seed loader by row key, a term lookup over the
// table's postings, and the table's ontology. Results render on the same
// canvas and lists as the workspace graph. Loaded after app.js and relies on
// its state, $, authHeaders, apiError, mergeGraph, draw, activate,
// selectNode, decodedNodeProperties, graphGeneration and capability.
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
      // A table-only Explorer should open on its actual graph source.
      if (!capability("neighborhood")) el("graph-table-active").checked = true;
      el("graph-table-summary").textContent =
        `Table ${value.table} (${value.schema_id}@${value.version}) under label ${value.label}, ` +
        `read from ${describeStores(value.stores)}. Edges are followed outward only.`;
      el("graph-table-depth").max = String(value.bounds.max_depth);
      renderOntology(value.ontology);
      status("Load a node by row key to see its first hop, or find one by term.");
    } catch (error) {
      if (mine !== generation) return;
      description = null;
      // The API conceals the route from readers without its label; the UI
      // must not reveal that a table is mounted either.
      section.hidden = error.status === 404;
      el("graph-table-ontology-body").replaceChildren();
      if (section.hidden) {
        status("");
      } else {
        el("graph-table-summary").textContent = "The graph source could not be loaded.";
        status(error.message || String(error), true);
      }
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
      mergeGraph(response.neighborhood);
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
      mergeGraph(response.path);
      $("graph-status").textContent = `Table ${response.table}: directed path of ${response.path.edges.length} edge(s).`;
      status(`Path of ${response.path.edges.length} edge(s).`);
      draw();
    } catch (error) {
      if (stale(mine, graphMine)) return;
      status(`Path finding failed: ${error.message || String(error)}`, true);
    }
  }

  async function load() {
    const id = el("graph-table-seed").value.trim();
    if (!id || !description) return;
    el("graph-table-active").checked = true;
    const mine = ++generation;
    const graphMine = graphGeneration;
    status(`Loading ${id}…`);
    try {
      const response = await request("nodes", {ids: [id]});
      if (stale(mine, graphMine)) return;
      const node = response.nodes[0];
      const found = node && decodedNodeProperties(node).found === "true";
      if (!found) {
        status(`${id} is in the namespace but the table holds no cell for it.`);
        return;
      }
      mergeGraph({nodes: response.nodes, edges: []});
      activate("graph");
      selectNode(id);
      await expand([id]);
    } catch (error) {
      if (stale(mine, graphMine)) return;
      status(`Load failed: ${error.message || String(error)}`, true);
    }
  }

  async function find() {
    const term = el("graph-table-term").value.trim();
    if (!term || !description) return;
    el("graph-table-active").checked = true;
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
      mergeGraph({nodes: nodes.nodes, edges: []});
      status(`${ids.length} node(s) posted under "${term}".`);
      activate("graph");
      selectNode(ids[0]);
    } catch (error) {
      if (stale(mine, graphMine)) return;
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
