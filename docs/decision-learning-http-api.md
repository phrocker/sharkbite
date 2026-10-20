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
# Learning HTTP API

The Learning console in the Explorer web workspace is a thin client over
these routes. Every action in the console has one of them, and the route
and the console produce identical records. The routes exist only when a
host mounts a complete `webapi.LearningSurface` (provider plus the trusted
resolver of its authenticated transport); an incomplete surface mounts
nothing, every learning route then answers 401 unauthenticated and 404
authenticated, and `GET /api/v1/meta` reports `decision_learning: false`.
`shoal-explore-web -learning-state-dir` mounts the file-backed service.

All routes need a bearer token, same-origin requests, no query string, and
(for POST) a JSON object body of at most 1 MiB. Errors are
`{"code","message"}`; a provider refusal keeps its message verbatim:
`invalid_argument` 400, `conflict` 409 (refused by the records),
`not_found` 404, `unavailable` 503.

| Method and path | Purpose |
| --- | --- |
| `GET /api/v1/learning/datasets` | Registered cohorts with the latest recorded export per cohort (manifest pin, quarantine counts). Reports whether a cohort authority is mounted. |
| `POST /api/v1/learning/datasets/exports` `{cohort_id}` | Request an authorized export; records its manifest pin, eligible example ids, source ids, and quarantine counts. The bytes are served by `/api/v1/dataset-exports`. |
| `GET /api/v1/learning/jobs` | Every distillation job with its receipt, requested versus actual, executor. |
| `POST /api/v1/learning/jobs` (distillation manifest) | Validate and record a manifest and its job. Every admitted cohort must have a recorded export at the pinned digest; an unadmitted cohort or a held-out leak is refused before the job exists. |
| `GET /api/v1/learning/jobs/{pin}` | One job with its manifest. |
| `POST /api/v1/learning/jobs/{pin}/receipt` (receipt) | Verify a trainer receipt against the manifest; silent deviation, widening, and improvement claims are refused. |
| `GET /api/v1/learning/candidates` | Candidates with bundle digests, lineage, and verdicts. |
| `POST /api/v1/learning/candidates` `{candidate, artifact}` | Record a sealed candidate and artifact; the bundle must be published on the serving root and the candidate must stay inside its manifest's selection. |
| `GET /api/v1/learning/candidates/{id}/evaluations` | Verdicts (`hold`, `shadow_candidate`) per assessor with the reserved test membership and leakage reasons. |
| `POST /api/v1/learning/evaluations` (promotion_record) | Record a sealed evaluation for a recorded candidate. |
| `GET /api/v1/learning/releases` | Active release with the exact bundle digest served now, history, eligibility, events, and the student runtime's report. |
| `POST /api/v1/learning/releases/promote` `{candidate_id, evaluation_id, owner}` | Seal the bound approval and promote. A missing evaluation or one that did not pass is refused with the record's reason. |
| `POST /api/v1/learning/releases/rollback` `{prior_release_id, owner}` | Seal a bound rollback approval and restore the prior bundle. |
| `GET /api/v1/learning/evidence/{kind}` | Read-only file-backed reports: `quality`, `poisoning`, `calibration`, `influence`, `challenger`. |
| `GET /api/v1/learning/bundles` | Offline release bundles with the verification result per entry. |
| `POST /api/v1/learning/bundles/export` `{release_id, name}` | Export an offline release bundle. |
| `POST /api/v1/learning/bundles/import` `{name, importer}` | Verify every entry and import; one altered byte is refused with the entry named and the per-entry result is returned. |

Shadow evidence cannot promote a model or exclude review. Promotion grants
shadow-candidate status; activation stays an explicit, bound approval. The
console creates no authority and cannot make an unmounted service appear
available.
