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
# Decision classes and the measured record

A decision class is the unit the decision layer measures and, later,
promotes: one task, one question, the label policy that governs its
adjudications, and the label the predictor selected. An abstention is its own
class. A class is an identity over records that already exist; it grants
nothing and asserts nothing about quality on its own. This is the first slice
of the [promotable decision bundle](promotable-decision-bundle.md) direction,
tracked as #218.

## Identity

`decision.DecisionClass{TaskID, QuestionID, PolicyID, Label, Abstained}` has a
content address, `decision-class:<sha256>`, over its canonical encoding. Two
hosts that see the same task, question, policy, and label name the same class.
The label must be empty exactly when the class is the abstention class.

## The measured record

A record is computed for one registered cohort over a declared observation
window. The cohort fixes the task, question, label policy, membership, and
**cutoff**, which is the cohort's declared label time. The window is the
reader's: `from` is required, and `to` defaults to the cutoff and may not pass
it. A record is never computed over an undeclared span.

For every member of the cohort the host takes a **class sample** row under
the same authority as a dataset export: the class the member's prediction
falls in, when its evidence was last observed, the adjudication in force at
the cutoff, and its outcome inventory. A member the caller may not read in
full refuses the whole sample. A record is never thinned to the visible part
of a cohort, exactly as an export is never thinned.

The record of each class with a target observed in the window:

| Field | Meaning |
| --- | --- |
| `targets` | Members of the class observed in the window. Every other count is over these. |
| `adjudication.verified_agree` | The adjudication in force at the cutoff is verified and its label is the predicted label. |
| `adjudication.verified_disagree` | Verified with a different label, or verified for an abstention. |
| `adjudication.disputed`, `adjudication.unresolved`, `adjudication.none` | The disposition in force, or no adjudication at the cutoff. These stay in the denominator. |
| `adjudication.withdrawn`, `adjudication.stale`, `adjudication.later`, `adjudication.training_denied` | Qualifiers, each a condition under which an export withholds a verified label: the authority withdrew it, the selected adjudication's basis does not match the outcome inventory, an adjudication was received after the cutoff, or the pinned policy does not permit the label for training. A verified label under any of them is counted as unresolved. The record never applies a label an export would null. |
| `outcomes.receipts` | Outcome receipts in the inventory, of every kind. |
| `outcomes.correctness`, `outcomes.superseded` | Correctness receipts in the selected adjudication's basis, a superseded receipt counted once under its successor. A basis carries only correctness receipts, so execution receipts appear in `receipts` and nowhere else; their status is not read in this slice. |
| `precision` | `verified_agree / (verified_agree + verified_disagree)`, or null when nothing is verified. It says nothing about the unverified share, which `targets` and the adjudication counts report. |
| `first_observed`, `last_observed` | The span of evidence observation inside the window. |

The response states the cohort, cutoff, window, the members in and outside
the window, and every class sorted by identity. Outcome receipt contents are
not read in this slice, so "acted on" in the sense of an execution receipt's
status is not yet in the record; the inventory count is. A prediction that
failed decided nothing and is in no class; a cohort holding one has no
record, as it has no export.

A basis that cites a receipt the inventory no longer holds is counted as
stale rather than refused, exactly as the export marks that row.

## Route

`POST /api/v1/decision-classes` with
`{"cohort_id": <base64url cohort id>, "from": <RFC 3339>, "to": <RFC 3339, optional>}`
behind the host's authenticated transport. It is mounted beside the dataset
exports route when a cohort authority is wired, and `GET /api/v1/meta`
reports `decision_classes`. The Decisions console has a "Decision classes"
card that takes the cohort id as the Learning console lists it, a window, and
renders the table.

For development, `-learning-fixture-cohorts` seals the class sample of each
fixture cohort at startup beside its export, under the demo's exporter
session, and aggregates a record over the window a caller declares. The
sealed fixture's baseline predictor selects one label for every fixture, so
its cohort has one class, and the record says what the baseline is worth:
half of the verified adjudications disagree with it.

## What this is not

A record is a read. It promotes nothing, exports nothing, and is not a
training-eligibility statement; the dataset export decides that under its own
rules. The graduation gate that reads records is #221, and the inbox that
shows them beside pending approvals is #219.
