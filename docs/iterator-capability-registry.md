# Iterator capability admission

Shoal targets the Apache Accumulo 4.0 iterator contract. The versioned
registry in `internal/iterrt/registry.go` is the single inventory used by
stack construction, table-configuration resolution, compaction admission,
and compatibility reporting.

The current machine-readable inventory is
[`iterator-capabilities-v3.json`](iterator-capabilities-v3.json). Consumers
can also JSON-encode `iterrt.RegistrySnapshot()` and
`iterrt.CompatibilityReport` directly.

`vectorKNN` accepts an `embeddingSpace` option. ShoalQL always supplies it for
exact vector plans; storage backends must validate that identity against every
participating immutable file before allowing the iterator to compare scores.

Admission is fail closed:

- the requested registry and Accumulo versions must match;
- every configured Java class must have a registered native implementation;
- the implementation must support the requested `scan`, `minc`, `majc`, or
  `offline` context;
- option keys and values must initialize successfully before work starts.

`itercfg.Resolver.Resolve` returns both a `ResolvedStack` report and an error
when admission fails. Callers must not execute the partial stack. Existing
approved iterator ordering and options are preserved; equal-priority names
use Java UTF-16 ordering. Mandatory host-installed system layers are listed
separately in each context's `mandatoryStack`.

Any change to aliases, contexts, option schemas, activation semantics, or
mandatory system layers requires a registry version bump and regenerated JSON
inventory.
