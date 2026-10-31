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
# Operator inbox

The inbox is the Explorer's first screen for the person who turns decisions
into automation: what is awaiting them on this host, each item with the
chain that justifies it and the one action it needs. It is the first slice of
the Explorer redesign in [promotable decision bundle](promotable-decision-bundle.md),
tracked as #219. Graph, evidence, and learning are reached from an item; the
tabs stay, but they stop being the front door.

## What is listed

`POST /api/v1/inbox` composes two sections from providers that already serve
those objects, and adds no authority of its own.

- **Held requests** awaiting a decision: every pending fleet approval the
  caller is eligible to decide, read through the approval service's own
  pending route, page by page, and grouped by the action class the request
  names (its capability and action). A caller who is not an eligible approver
  sees an empty section that says so. The listing carries what the approval
  route carries on a page: no input. Expanding an item reads its status,
  which carries the input, the effective state beside the stored one, and the
  condition when it can no longer progress.
- **Candidates** awaiting promotion: every learning candidate sealed as a
  shadow candidate that no release, active or past, has taken, with its
  evaluations, lineage, and the evaluation a promotion would cite. Beside
  each candidate is the [measured record](decision-classes.md) of every cohort
  its job consumed, over a window that begins `-inbox-record-window` (one
  year by default) before now and ends at the cohort's own cutoff; the record
  states the window it was served for. A cohort whose record this caller
  cannot read, or whose cutoff the window does not reach, is listed with that
  reason, never with a thinned record; a record the class route would refuse
  for a substituted cohort or window is refused here too; a candidate whose
  training job cannot be read says so instead of listing cohorts.

A section whose provider is not mounted says so. A read that fails for any
reason other than "not an approver" fails the inbox, including a releases
listing that cannot be read, since that would let a taken candidate reappear;
nothing is composed under an authority that moved while it was being read.
The pending route filters each page to what the caller may decide, so a page
can be empty with more behind it; the inbox walks until the cursor ends or
sixteen pages have been read, lists at most 256 approvals and 32 candidates,
and says when it stopped.

The request body carries the same `context` the fleet routes take
(`request_id`, `reason_code`, `deadline`); it is required when an approval
service is mounted, because the approval service records it.

## What a person does there

- **Decide a held request.** Expanding a request shows the requester, the
  agent, the capability and action, the declared effects from the agent's
  current descriptor, the input, the deadline, and the policy generation the
  request was made under. Approve and refuse post to the approval route with
  the request digest and policy generation the row was served with, so a
  person can only decide the request they were shown. A row whose status no
  longer matches the listing is marked stale and must be reloaded, not
  decided.
- **Promote a candidate.** Expanding a candidate shows its evaluations, its
  lineage, and the class records. Promote posts to the learning route with
  the candidate and the evaluation it cites; the learning service applies its
  own gates unchanged.
- **Open an inspector.** An item's object opens the Evidence view with that
  object as the query; a candidate opens the Learning view on its
  evaluations. Selecting an item selects it everywhere it appears.

## What the inbox does not do

It does not decide, promote, or evaluate anything itself, does not list a
request the approval service would not serve the caller, and does not compute
a record the class route would not serve the caller. It carries no storage
keys; every identifier it shows is the identifier the underlying route uses.
Cases, phases, and the waiting states beyond "decision" are #225.

## Development

`-dev-auth` mints one identity that is every requester and so can never be
an approver; the held-requests section is empty under it by design. The
candidates section works with `-learning-state-dir` and
`-learning-fixture-cohorts`. The end-to-end test for the held-requests
section runs the real approval service with distinct requester and approver
principals.
