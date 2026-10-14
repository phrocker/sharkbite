# Ontology-guided extraction

`pkg/extraction` is the runtime boundary between a low-level
`model.TextGenerator` and the immutable contracts in `pkg/ontology` and
`pkg/inference`.

The orchestrator:

- accepts one validated ontology version and an exact, ontology-pinned
  `inference.ContextPack`;
- constructs a deterministic bounded prompt from ontology definitions,
  caller instructions, and cited evidence anchors only (the context query and
  metadata are not copied into the prompt);
- requires one strict JSON object and rejects unknown or duplicate fields,
  malformed or oversized bodies, excessive nesting and counts, invalid value
  types, non-finite confidence, duplicate entities or relations, unknown
  ontology members, disallowed relation endpoints, ungrounded node IDs, and
  evidence IDs outside the context pack;
- validates cardinality, ownership, uniqueness, patterns, allowed values, and
  numeric bounds through `ontology.ExtractionResult.ValidateFor`;
- creates `inference.InferenceResult` claims tied to the exact context anchors;
- derives stable entity, edge, assertion, and claim identities and marks every
  planned item `inferred` and `proposed`; and
- returns a publication plan without writing storage.

Every assertion the orchestrator emits records the
`ontology.OntologyIdentity` -- schema ID plus version ID -- of the version the
request pinned, so the meaning that applied is recoverable from the assertion
itself rather than only through the result to request chain. Definition IDs are
derived from namespace and key alone and are deliberately stable across
versions, so the recorded identity is the only thing that fixes which meaning
was in force. `ontology.ExtractionResult.ValidateFor` rejects an assertion that
records a different version than the request pinned; an assertion that recorded
nothing is reported by `UnresolvedOntologyAssertions` and is never stamped with
the request's version.

An existing graph node may only be referenced when its ID occurs in a supplied
graph evidence path and the output cites that graph anchor. Graph IDs are
represented in prompts and model output as reversible `node-base64:` tokens,
so arbitrary opaque ID bytes remain exact. Plans retain both the raw graph ID
and a canonical UTF-8 contract ID used by ontology assertions and inference
claims. A referenced node's plan action is `reference`, so it is not recreated,
overwritten, or relabeled. New entities use `create`. Consumers must submit
plans through an atomic high-level Explorer publication API when one becomes
available; model output must never be translated directly into graph-row
writes.

`HeuristicExtractor` is an explicit no-model option with `heuristic`
provenance. It is never invoked as a fallback after a model timeout,
unavailability, malformed response, or validation failure.

`internal/agentmem.Client.PlanEnrichment` exposes the complete validated plan
without flattening it into the legacy Veculo entity write path. Consolidation
no longer interprets model response substrings. It can produce a structured
plan and hand it to a caller-supplied high-level publisher; without that
publisher it performs no model call or graph mutation.
