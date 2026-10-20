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
(for POST) a JSON object body of at most 1 MiB. Every POST additionally
requires the `learning_write` operation on the caller's decision (403
otherwise, provider never reached), and the transport binds the
authenticated actor to the call: promotion and rollback approvals and
import provenance are sealed against that actor. A body may carry `owner`
or `importer` only to agree with it; naming anyone else is refused. Errors
are `{"code","message"}`; a provider refusal keeps its message verbatim:
`invalid_argument` 400, `conflict` 409 (refused by the records),
`not_found` 404, `unavailable` 503 (host-side causes are logged, not
sent), `internal` 500 (a response the host cannot deliver, such as one
over its size limit).

| Method and path | Purpose |
| --- | --- |
| `GET /api/v1/learning/datasets` | Registered cohorts with the latest recorded export per cohort (manifest pin, quarantine counts). Reports whether a cohort authority is mounted. |
| `POST /api/v1/learning/datasets/exports` `{cohort_id}` | Request an authorized export; records its manifest pin, eligible example ids, source ids, and quarantine counts. The bytes are served by `/api/v1/dataset-exports`. |
| `GET /api/v1/learning/jobs` | Every distillation job with its receipt, requested versus actual, executor. |
| `POST /api/v1/learning/jobs` (distillation manifest) | Validate and record a manifest and its job. Every admitted cohort must have a recorded export at the pinned digest; an unadmitted cohort or a held-out leak is refused before the job exists. Every eligible model's declared egress is resolved by the host (`-learning-model-egress`); a declaration the host does not confirm, a provider it cannot resolve, or an off-host chosen teacher without `-learning-allow-off-host-teachers` is refused. |
| `GET /api/v1/learning/jobs/{pin}` | One job with its manifest, the recorded prediction set's id, and the receipt. |
| `POST /api/v1/learning/jobs/{pin}/predictions` (teacher-predictions record) | Record the sealed teacher output for the job. The set must rebuild identically from the recorded manifest and job: predictions only, from the chosen teacher, every example inside an admitted export, none held out, not empty. |
| `POST /api/v1/learning/jobs/{pin}/receipt` (receipt) | Verify a trainer receipt against the manifest; silent deviation, prose deviations, parameters the manifest never requested, widening, and improvement claims are refused. A completed receipt must name the prediction set recorded for the job. |
| `GET /api/v1/learning/candidates` | Candidates with bundle digests, lineage, and verdicts. |
| `POST /api/v1/learning/candidates` `{candidate, artifact}` | Record a sealed candidate and artifact; the bundle must be published on the serving root. A candidate naming a training manifest must name one recorded here, with a completed receipt that produced exactly this bundle, and must stay inside its selection; an unrecorded pin is refused, not skipped. |
| `GET /api/v1/learning/candidates/{id}/evaluations` | Verdicts (`hold`, `shadow_candidate`) per assessor with the reserved test membership and leakage reasons. |
| `POST /api/v1/learning/evaluations` (promotion_record) | Record a sealed evaluation for a recorded candidate. |
| `GET /api/v1/learning/releases` | Active release (the registry's pointer and its bundle digest), history, eligibility, events, and the student runtime's own report with `serves_active_release`: what is served now comes only from the runtime, and is compared with the pointer rather than assumed from it. |
| `POST /api/v1/learning/releases/promote` `{candidate_id, evaluation_id}` | Seal the approval bound to the authenticated actor and promote. A missing evaluation or one that did not pass is refused with the record's reason. |
| `POST /api/v1/learning/releases/rollback` `{prior_release_id}` | Seal a rollback approval bound to the authenticated actor and restore the prior bundle, which must still be on the serving root. |
| `GET /api/v1/learning/evidence/{kind}` | Read-only file-backed reports: `quality`, `poisoning`, `calibration`, `influence`, `challenger`. |
| `GET /api/v1/learning/bundles` | Offline release bundles with the verification result per entry. |
| `POST /api/v1/learning/bundles/export` `{release_id, name}` | Export an offline release bundle. |
| `POST /api/v1/learning/bundles/import` `{name, expected_index_digest?}` | Verify every entry and import; one altered byte is refused with the entry named and the per-entry result is returned. With `expected_index_digest` (quoted from the exporting side) a bundle whose index was rewritten is refused even when every entry matches the rewritten index. The import is recorded against the authenticated actor. |

Shadow evidence cannot promote a model or exclude review. Promotion grants
shadow-candidate status; activation stays an explicit, bound approval. The
console creates no authority and cannot make an unmounted service appear
available.
