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
# Grounded inference cache and deterministic evaluation

`pkg/inference/harness` supports an optional in-process cache for completed
grounded inference runs. Caching is off by default; callers opt in with
`NewCachedGenerator` and a bounded `MemoryCache`.

The cache key is a non-disclosing digest over the validated session identity:
context pack, snapshot pin and `as_of`, authorization fingerprint and expiry,
model provider/name/version/parameters/seed, prompt provenance, budgets, harness
version, tool policy, and stable runner/tool-host identities supplied through
`CacheIdentityProvider`. Cache entries are never served across different
snapshot or authorization pins. If identity material cannot be established or
appears unsafe for cache use, the harness fails closed by bypassing the cache.
Explorer tool hosts may also be given explicit stable dependency identities
when a production client cannot implement `CacheIdentityProvider` directly.
`ModelRunner` cache identity requires an explicit `ClockIdentity` because the
clock affects generated timestamps and result IDs; omit it to fail closed and
bypass caching for non-deterministic clocks.

`MemoryCache` bounds both entry count and byte size and evicts least-recently
used entries deterministically.

The same package exposes a fixture evaluation harness:

- `LoadFixtureEvaluationCases(root, now)` builds deterministic cases from
  `testdata/explorer-eval`;
- `Evaluate(ctx, generator, cases, now)` runs a generator and reports only
  computed metrics: evidence support, unsupported outcomes, citation validity,
  budget use, iteration counts, and stop-reason distribution;
- `EvaluationReport.CanonicalJSON()` emits stable JSON suitable for CI
  regression checks.

The fixture evaluation uses the deterministic fake provider in tests and
requires no network access or credentials. For historical fixture timestamps,
configure the generator with `SetClock` before calling `Evaluate` so
authorization freshness is evaluated against the same deterministic clock.
