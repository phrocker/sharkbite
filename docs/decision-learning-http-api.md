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
`shoal-explore-web -learning-state-dir` mounts the file-backed service. A
cohort authority and exporter are host-wired; without one the datasets
route lists recorded exports and reports that no authority is mounted.
For development, `-learning-fixture-cohorts source,review` prepares the
adjudicated training demo's sealed synthetic fixture census at startup as
the authority and exporter, exports each cohort once at startup, and also
mounts `/api/v1/dataset-exports` over the same sealed bytes, so a trainer on
the host fetches exactly the export whose pin the learning service recorded
and dataset export, distillation, and promotion can be exercised end to end.
It is a finite conformance fixture regenerated on every start under a fresh
cohort identity (so exports recorded by an earlier start stay attached to
that start's cohort), never a repository sampling authority.

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
| `POST /api/v1/learning/datasets/exports` `{cohort_id}` | Request an authorized export; records its manifest pin, eligible example ids, source ids, and quarantine counts, and keeps the authorized manifest bytes verbatim under the pin, so a later offline bundle export never depends on the authority reproducing them. The bytes are served by `/api/v1/dataset-exports`, which takes the cohort id base64url-encoded as the Go dataset client sends it. |
| `GET /api/v1/learning/jobs` | Every distillation job with its receipt, requested versus actual, executor. |
| `POST /api/v1/learning/jobs` (distillation manifest) | Validate and record a manifest and its job. Every admitted cohort must have a recorded export at the pinned digest; an unadmitted cohort or a held-out leak is refused before the job exists. Every eligible model's declared egress is resolved by the host (`-learning-model-egress`); a declaration the host does not confirm, a provider it cannot resolve, or an off-host chosen teacher or base without `-learning-allow-off-host-models` is refused. |
| `GET /api/v1/learning/jobs/{pin}` | One job with its manifest, the recorded prediction set's id, and the receipt. |
| `POST /api/v1/learning/jobs/{pin}/policy` `{name, split, positive_label, minimum_recall, minimum_specificity, minimum_positives, minimum_families}` | Declare the evaluation policy the run will be judged under. It is sealed as an `evaluation_policy` record addressed by its gates (so runs judged the same way share one policy identity, which release lineage compares), and the declaration is recorded against the manifest pin. The host requires it before accepting teacher predictions or a receipt; a second declaration must be identical. `GET /jobs/{pin}` returns it as `evaluation_policy`. |
| `POST /api/v1/learning/jobs/{pin}/predictions` (teacher-predictions record) | Record the sealed teacher output for the job. The set must rebuild identically from the recorded manifest and job: predictions only, from the chosen teacher, every example inside an admitted export, none held out, not empty. |
| `POST /api/v1/learning/jobs/{pin}/run` `{}` | Ask a learning worker to train the job. Recorded once per job, after the evaluation policy is declared and before any receipt; a second request returns the first. `GET /jobs` and `GET /jobs/{pin}` carry `run`: whether it was requested, the current claim (worker, attempt, lease, whether it is live), the latest progress, and `runnable`. |
| `POST /api/v1/learning/jobs/{pin}/claim` `{worker_id, lease_seconds?}` | A worker takes a requested job under a lease (default 600 s, between 30 s and 6 h). Refused while another claim is live, when no run was requested, once a receipt exists, and once the job is exhausted (100 attempts; the view then says `exhausted` and `runnable` is false). Returns the claim and the job's exports (cohort ids and pins) so the worker knows what to fetch from `/api/v1/dataset-exports`. |
| `POST /api/v1/learning/jobs/{pin}/progress` `{claim_id, phase, message?, detail?}` | The worker's phase under its live claim: `fetching`, `training`, `recording_predictions`, `recording_receipt`, `sealing`, `done`, `failed`. A terminal phase releases the claim; a report under an ended, expired, or foreign claim is refused, as is `done` while the job has no receipt, and a claim records at most 64 reports (message ≤ 1000 characters, detail an object ≤ 2 KiB). Run records describe the work only: predictions, the receipt, and the sealed candidate still arrive through their own routes and are verified there. The newest attempt and sequence are indexed in memory, so a read costs at most two record reads whatever a job's history. |
| `POST /api/v1/learning/jobs/{pin}/receipt` (receipt) | Verify a trainer receipt against the manifest; silent deviation, prose deviations, parameters the manifest never requested, widening, and improvement claims are refused. A completed receipt must name the prediction set recorded for the job, and is refused until an evaluation policy is declared for the job. |
| `GET /api/v1/learning/candidates` | Candidates with bundle digests, lineage, and verdicts. |
| `POST /api/v1/learning/candidates` `{candidate, artifact}` | Record a sealed candidate and artifact. The candidate must name the training manifest of the run that produced it, recorded here with a completed receipt that produced exactly this bundle, published on the serving root (`-learning-serving-root`, by default `releases/` under the workspace `-state-dir`, not under `-learning-state-dir`, whose `releases/` is the release registry); it must stay inside the selection and name the policy declared for the job before its receipt. The host then seals the run again from its own records and the published bundle and accepts only the candidate and artifact that sealing produces, so `shoalctl learning candidate` and `seal_candidate.py` outputs are accepted because they are byte-identical to the host's, and nothing about a run is taken on the caller's word. An unrecorded pin, or no pin, is refused. A refusal names the serving root by role, never by host path. |
| `POST /api/v1/learning/candidates/seal` `{manifest_pin, cohort_id?, bundle: {name: base64}}` | Seal a completed run in one step. The host rebuilds the candidate, evaluation, and artifact from the manifest, receipt, prediction set, the policy declared before the receipt, and every export the run consumed (a body cannot claim what the run did not do), verifies first that the bundle is the one the receipt produced, measures the student on the reserved split of the evaluation cohort, and publishes the bundle on the serving root (without replacing an occupant) only when the disposition is `shadow_candidate`; a held run's records are recorded, its bundle is not placed (`cohort_id`, optional when the run trained on one). The evaluation records the policy it was held to: a recall gate over verified positives, a specificity gate over verified negatives, a sample gate, a family gate, and every leak between the reserved rows and any training row of any cohort. Its disposition is `shadow_candidate` or `hold`, never an authorization. `shoalctl learning candidate` and `experiments/local-ml/seal_candidate.py` seal byte-identical records from the same files plus the declared policy record, for the candidate and evaluation routes, which accept a candidate only under the policy declared here. |
| `GET /api/v1/learning/candidates/{id}/evaluations` | Verdicts (`hold`, `shadow_candidate`) per assessor with the reserved test membership and leakage reasons. |
| `POST /api/v1/learning/evaluations` (promotion_record) | Record a sealed evaluation for a recorded candidate. Its content (disposition, reasons, metrics, the policy it was held to) is not taken on the caller's word: the host measures the run again from its own records and the published bundle and accepts only the record that measurement seals. |
| `GET /api/v1/learning/releases` | Active release (the registry's pointer and its bundle digest), history, eligibility, events, and the student runtime's own report with `serves_active_release`: what is served now comes only from the runtime, and is compared with the pointer rather than assumed from it. |
| `POST /api/v1/learning/serving/predict` `{features}` | Ask the configured student runtime the student's task for one feature vector under a fixed question set. Only the runtime answers; a host without one refuses, and the runtime's own cause stays in the host log. |
| `POST /api/v1/learning/releases/promote` `{candidate_id, evaluation_id}` | Seal the approval bound to the authenticated actor and promote. A missing evaluation or one that did not pass is refused with the record's reason. |
| `POST /api/v1/learning/releases/rollback` `{prior_release_id}` | Seal a rollback approval bound to the authenticated actor and restore the prior bundle, which must still be on the serving root. |
| `GET /api/v1/learning/evidence/{kind}` | Read-only file-backed reports: `quality`, `poisoning`, `calibration`, `influence`, `challenger`. |
| `GET /api/v1/learning/bundles` | Offline release bundles with the verification result per entry. |
| `POST /api/v1/learning/bundles/export` `{release_id, name}` | Export an offline release bundle. The dataset manifest it carries is the copy recorded when the export was requested; an export recorded before copies were kept falls back to the mounted exporter and is refused when its current bytes no longer hash to the pin. |
| `POST /api/v1/learning/bundles/import` `{name, expected_index_digest?}` | Verify every entry and import; one altered byte is refused with the entry named and the per-entry result is returned. With `expected_index_digest` (quoted from the exporting side) a bundle whose index was rewritten is refused even when every entry matches the rewritten index. The import is recorded against the authenticated actor. |

Shadow evidence cannot promote a model or exclude review. Promotion grants
shadow-candidate status; activation stays an explicit, bound approval. The
console creates no authority and cannot make an unmounted service appear
available.
