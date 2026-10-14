<!--

    Licensed to the Apache Software Foundation (ASF) under one
    or more contributor license agreements.  See the NOTICE file
    distributed with this work for additional information
    regarding copyright ownership.  The ASF licenses this file
    to you under the Apache License, Version 2.0 (the
    "License"); you may not use this file except in compliance
    with the License.  You may obtain a copy of the License at

      https://www.apache.org/licenses/LICENSE-2.0

    Unless required by applicable law or agreed to in writing,
    software distributed under the License is distributed on an
    "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
    KIND, either express or implied.  See the License for the
    specific language governing permissions and limitations
    under the License.

-->
# Iterator forge — evaluate Java iterators in a live cluster, transpile to Go, auto-test

Status: **draft / design**. Implementation tracked by todos `if-*`.

## 1. Goal

Given a **custom Accumulo `SortedKeyValueIterator`** running in a live
cluster that shoal does not yet understand, produce a **Go `iterrt`
iterator that is byte-for-byte behaviorally equivalent**, with an
auto-generated test suite proving that equivalence, and promote it into
shoal — so that shoal's read path and (offline) compaction path can run
tables that use that iterator.

This is what unblocks arbitrary customer tables: `itercfg` today drops
any unrecognized `table.iterator.*` class into `ResolvedStack.Skipped`,
and both shadow parity and offline compaction refuse tablets with a
non-empty `Skipped`. The forge turns `Skipped` classes into
`ClassAllowlist` entries.

## 2. First principle: the LLM is never trusted

An LLM writes the candidate Go code, but **nothing about correctness
depends on the LLM being right.** The arbiter of correctness is the
existing `shadow` oracle: a generated iterator is accepted **only** when
its output is byte-exact against golden traces captured from the *real
Java iterator* running in a *real Accumulo* process, across a fixture
corpus. The LLM is a code-*suggestion* engine; the parity gate is the
*acceptance* engine.

Consequences that shape the whole design:

- Generated code lives in an **isolated sandbox** and never edits core
  `iterrt` until it passes the gate and a human approves.
- Every promoted iterator ships a **provenance manifest** (§7) so the
  origin (which model, which prompt, which Java class, which fixtures)
  is auditable — important for an OSS, patent-clean codebase.
- The pipeline is **reproducible**: same Java class + same fixture corpus
  ⇒ same golden traces ⇒ deterministic pass/fail regardless of what the
  LLM produced.

## 3. Pipeline

```
 discover ─► acquire ─► characterize ─► transpile ─► compile ─► parity ─┬─► promote
    ▲                    (live cluster)   (LLM)                  gate    │
    │                                        ▲                           │
    │                                        └──────── repair ◄──────────┘
    └─────────────────────────────── worklist                (on divergence)
```

### 3.1 Discover (`if-discover`)

Extend `shadow/itercfg` to sweep **every** table's
`table.iterator.<scope>.*` config and aggregate a deduplicated worklist
of Java classes whose `ResolvedStack.Skipped` shows no shoal coverage.
Output: `[]PortCandidate{Class, SeenOnTables, Scopes, ExampleOptions}`.
This is the forge's input queue.

### 3.2 Acquire

Obtain the iterator's **Java source** (the common, preferred case) or
fall back to **bytecode**:

- **Source path (expected for most iterators):** the operator points the
  forge at the iterator's source (the jar's `-sources` artifact or a
  source tree). Custom iterators are typically first-party code the
  operator owns, so source is available in the majority of real cases,
  and it yields by far the best transpile quality (comments, option
  descriptors, and control flow all survive).
- **Bytecode path (fallback):** when only the deployed jar is available,
  decompile the class from the cluster classpath. Lower fidelity but
  always available.

Capture, alongside the code: the fully-qualified class name + hash, the
`IteratorSetting` option keys it reads (scraped from `init()` / option
descriptors), and its declared scope compatibility.

### 3.3 Characterize — evaluate in a running cluster (`if-java-harness`)

This is the **"evaluate iterators in a running Accumulo cluster"**
component and the behavioral spec generator.

A small Java sidecar (`shoal-iterforge-java`, using MiniAccumulo / the
RFile + iterator APIs) runs the *real* Java iterator over a curated set
of input RFiles, for each `(options, scope)` combination in the fixture
matrix, and dumps the exact output as **golden traces**:

```
GoldenTrace {
  javaClass, classHash
  scope, options
  inputRFileRefs (hashes)
  // The ground truth: the iterator's full output as canonical
  // key/value BYTES in Accumulo sort order — each entry is the
  // serialized (row, cf, cq, vis, timestamp, deleteFlag) key plus the
  // raw value bytes, exactly as an RFile would encode them. Stored as
  // opaque bytes, NOT as any in-memory Cell struct, so the trace is
  // serializable and the parity gate compares byte-for-byte.
  outputEntries [](keyBytes, valueBytes)
}
```

The fixture corpus is deliberately adversarial: empty inputs, single
cell, deletes/tombstones, multiple versions per key, visibility labels,
column-family boundaries, large values, and — where the class advertises
options — option permutations. Golden traces are **both** the LLM's
behavioral reference **and** the parity gate's ground truth **and** the
seed for the auto-generated Go tests. One capture, three uses.

### 3.4 Transpile — LLM (`if-transpiler`)

A vendor-agnostic `Transpiler` interface:

```go
type Transpiler interface {
    Generate(ctx context.Context, req TranspileRequest) (TranspileResult, error)
}
```

`TranspileRequest` bundles the prompt context:

- the Java source/decompilation,
- the `iterrt.SortedKeyValueIterator` interface contract + `Init`/`Seek`/
  `HasTop`/`Next`/`DeepCopy` semantics (verbatim from the codebase),
- **exemplar** Go iterators already in `iterrt` (e.g. `VersioningIterator`,
  a filter, a buffering/transforming one) as few-shot style anchors,
- a representative subset of golden I/O traces (so the model sees
  concrete expected behavior, not just prose),
- the option keys the iterator must parse.

`TranspileResult` is a Go source file implementing the interface, plus a
proposed `IterSpec` name and a class→alias mapping proposal.

The interface is **OSS-friendly and vendor-neutral**: implementations
for an OpenAI-compatible endpoint, Anthropic, and a local runtime
(Ollama) all satisfy it; the default reference backend is the
OpenAI-compatible one (widest compatibility), selected/config'd purely
via env so the core repo has **no hard vendor dependency**.

### 3.5 Compile + sandbox (`if-sandbox`)

`go build` the generated file in an isolated package with its own
sandbox registry (mirrors `iterrt.newIterator` but separate). The core
`iterrt` registry is untouched. Compile failures feed straight into the
repair loop (§3.7) with the compiler output.

### 3.6 Parity gate (`if-parity`)

The acceptance test. For every golden trace:

1. Build a shoal leaf from the trace's input RFiles.
2. Stack the generated iterator with the trace's options/scope.
3. Drain it and diff its emitted key/value byte stream against the
   trace's `outputEntries` using the **shadow** oracle's **T2**
   comparator — a streaming digest over the canonical cell sequence that
   proves position-for-position equality (see `internal/shadow/
   compare.go`). This is the same byte-exact machinery already trusted
   for compaction parity.

> Note: the shadow oracle's T3 (random point-lookup sampling) is
> **vestigial** under the current streaming-hash design — `compare.go`
> always leaves `T3.Attempted=false`, because equal T2 digests already
> prove the cell sequences are identical. The parity gate therefore
> keys off **T2 only**; it does not rely on T3.

Pass = byte-exact across the **entire** corpus. On pass, auto-generate a
committed table-driven `_test.go` from the golden traces so the
equivalence is permanently regression-guarded in CI (independent of the
LLM and of any live cluster).

### 3.7 Repair loop (`if-repair`)

On compile failure or parity divergence, re-invoke the transpiler with
the previous candidate + the **exact** failure signal: the compiler
error, and/or the **T2 first-divergence diff** (the first position where
the generated iterator's key/value bytes differ from the golden trace).
Bounded to N rounds (default 3). If still failing, the class is reported
as **needs-human-port** with the captured diffs attached — the forge
degrades to a very good assistant rather than shipping something wrong.

### 3.8 Promote (`if-promote`)

On a green corpus:

1. Write the iterator source + generated test into `iterrt` (or a
   clearly-marked `iterrt/ported` subpackage).
2. Register the constructor in `newIterator` and the class→alias in
   `itercfg.ClassAllowlist`.
3. Emit the **provenance manifest** (§7).
4. **Require human review** (the promotion is a PR, not an auto-merge).
   LLM-authored code crossing into the trusted core is a reviewed event.

## 4. Reused vs new

| Concern | Reuse | New |
|---|---|---|
| Iterator contract | `iterrt.SortedKeyValueIterator` | — |
| Byte-exact diff | `shadow` T2 comparator | — |
| Config discovery | `shadow/itercfg` | worklist aggregation |
| Class→alias map | `itercfg.ClassAllowlist` | promotion writer |
| Run Java iterator | — | `shoal-iterforge-java` sidecar |
| LLM call | — | `Transpiler` interface + backends |
| Orchestration | — | `internal/iterforge` + `cmd/shoal-iterforge` |

## 5. Determinism & reproducibility

- Golden traces are content-addressed by `(classHash, fixtureHash,
  options, scope)`; re-running characterization on an unchanged class +
  corpus reproduces identical traces.
- The parity gate and generated tests depend **only** on golden traces +
  generated Go — never on a live cluster or the LLM at CI time.
- A promoted iterator's manifest pins the corpus hash, so CI can detect
  "the corpus changed but the iterator wasn't re-validated".

## 6. Safety boundaries

- Generated code compiles and runs **only** in the sandbox until
  promoted; a transpile that misbehaves cannot affect production reads.
- Promotion is human-gated; the forge never self-merges into core.
- An iterator that cannot reach byte-exact parity is **never** promoted —
  it is surfaced for manual porting. Silent approximation is disallowed
  (mirrors offline compaction's refusal to drop a Skipped iterator).

## 7. Provenance manifest

Committed next to each promoted iterator:

```yaml
iterator:      <shoal IterSpec name>
java_class:    <fqcn>
java_class_hash: sha256:...
transpiler:
  backend:     openai-compatible|anthropic|ollama|...
  model:       <model id/version>
  prompt_hash: sha256:...
  repair_rounds: <n>
fixture_corpus_hash: sha256:...
parity: { cells: exact, lookups: exact, corpus_cases: <n> }
generated_at: <rfc3339>
reviewed_by:  <human, filled at PR merge>
```

Auditable lineage matters for an OSS/patent-clean repo: anyone can see
exactly how a piece of machine-generated code entered the trusted set and
reproduce its validation.

## 8. CLI surface (`cmd/shoal-iterforge`)

```
shoal-iterforge discover  --zk ... --instance ...      # emit worklist
shoal-iterforge forge     --class <fqcn> --source <path|jar> \
                          --java-harness <cmd> \
                          --backend <name> --model <id> \
                          [--repair-rounds 3] [--out <dir>]
shoal-iterforge verify    --iterator <dir>             # re-run parity gate
shoal-iterforge promote   --iterator <dir>             # prep the promotion PR
```

## 9. Open decisions

- **D-1 (default LLM backend):** OpenAI-compatible endpoint as the
  reference (vendor-neutral interface, any backend pluggable). Confirm
  preference — Anthropic / local Ollama are equally supported.
- **D-2 (source vs bytecode):** support both acquisition paths; prefer
  source when available for transpile quality.
- **D-3 (ported code location):** `iterrt/ported/` subpackage vs inline
  in `iterrt`. Lean: `iterrt/ported/` to keep machine-origin code
  visibly separate until it earns its way in.
