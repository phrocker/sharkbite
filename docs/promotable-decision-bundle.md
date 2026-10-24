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
# Promotable decision bundle

> Status: accepted product direction, 2026-10-24. Tracked in #217.

Sharkbite is one decision layer with two ways to consume it. The decision
bundle is one representation of its output, not the whole product. This
document records the direction, what a bundle holds, how a decision earns its
way into one, what the model still does, and what it changes for the learning
path and the Explorer. Work is tracked in #218 to #221.

## One decision layer, two consumption modes

- **Enforcement plane.** Sharkbite sits in the path. Actions declare effects,
  the host declares what it permits, evidence and authority are checked at
  request time, and mismatches fail closed. A model may be present on this
  path when the customer allows it. This is the fleet dispatch, approval, and
  effects-gateway work under #142.
- **Promotable decision bundle.** Sharkbite sits beside the path. The model
  and an operator work on evidence at design time, and what ships into the
  enterprise is deterministic automation with its justification attached. No
  model runs in the customer's runtime.

Both read the same evidence, the same adjudications, and the same measured
record. The difference is where the decision executes and who holds the model.

The second mode exists because many of the companies this layer is for will
not run a model inside their enterprise. They want decisions fed into
automation they can read, test, version, and show to an auditor. Until now the
learning path's deliverable was a student model: a weights bundle carried by an
offline release bundle. That stays as the enforcement-plane option. It is no
longer the main road.

## What a decision bundle is

Plain files plus an index with a SHA-256 for every entry, with the same
discipline as the [offline release bundle](offline-release-bundle.md):

| Entry | Contents | Exists today |
| --- | --- | --- |
| Executable decision | A predicate over observations plus an action with declared effects | No. Decisions are answers to questions over a context pack; see [Observations and predicates](#observations-and-predicates) |
| Evidence | The picture pinned to snapshot and revision, with the adjudication receipts that shaped the class | Yes: `pkg/decision` picture manifests, prediction, outcome, and adjudication records |
| Measured record | Acted on versus dismissed in shadow, unknown and disputed kept in the denominator, sample size, window, cohort | Per prediction only; nothing names a class or aggregates one (#218) |
| Approval | Who, when, and the policy under which the decision may run unattended | Enforcement side: fleet approvals and `Effects` in `pkg/explorer/fleet` |
| Executor and replay | A deterministic evaluator, and a harness that runs the bundle against the customer's own history before it is enabled | `cmd/shoal-frozen-code-replay` has the shape for one frozen predictor; a rule needs the same shape (#221) |
| Digests | A SHA-256 on every entry; import verifies before anything is read | Yes, for the weights and release bundles |

Nothing in a bundle is produced by a model at the customer's runtime, and a
bundle has no model runtime among its dependencies.

## Decision classes and the measured record

A decision class is the unit that is measured and promoted. It is the
content-addressed identity of a task, a question, and a label, with the label
policy that governs its adjudications. A single prediction, approval, or
outcome is an instance of a class; promotion is never a judgement on one
instance.

The measured record of a class, over a declared window and cohort, counts
outcomes acted on, dismissed, unknown, and disputed, with sample size and the
first and last observation. Unknown and disputed stay in the denominator and
are never folded into clean negatives, as [Local
decisions](local-decisions.md#measurement-and-promotion) already requires. The
record carries the visibility of every receipt it counts: a reader who cannot
see a receipt does not see it counted.

## How a decision is promoted

A class graduates in steps, each recorded in an append-only graduation record
with the measured record it was decided on, the approver, the policy, and the
reason:

1. **Proposed.** The model proposes; a person approves each instance.
2. **Audited.** The rule fires; a person audits a declared sample.
3. **Unattended.** The rule fires under policy; the audit continues at a
   declared cadence.

Demotion is the same gate run backwards, with the reason recorded. The gate's
triggers are declared on the class before any promotion: minimum sample,
window, maximum unknown share, calibration age, audit cadence. Undeclared
triggers prohibit promotion, exactly as undefined triggers prohibit canary
promotion in the weights path. A thin sample or an undeclared window is a
refusal with a reason, not a warning.

## Observations and predicates

This is the entry the repository does not have. Decisions in `pkg/decision`
are answers to questions (choice, ordinal, probability) over a context pack.
They are not predicates over observations, and there is no observation schema.
So the first design work is to define both (#220).

The predicate form is Sharkbite's own, informed by SAG, not an export of it.
SAG's guardrail clause (`DO verb(args) BECAUSE expr`) shows the shape worth
keeping: a boolean expression over named observation paths, comparison and
boolean operators, scalar literals, evaluated against a context, bound to one
action. Its grammar also carries a note on exactly how an inverted precedence
ladder makes a guardrail fail open while reading correctly; that is a test to
write, not a mistake to repeat. SAG's message framing, headers, correlation,
and wire form are not taken, and a bundle's identity has no SAG dependency.
Whatever language a customer's automation speaks is an export target chosen
later.

The pieces:

- **Observation.** A content-addressed record naming an observation schema,
  the subject, the observed fields as a flat path-to-value map, the supporting
  evidence ids, the snapshot pin, and when it was observed. Collectors assert
  observations and admission verifies them, as outcomes are handled today. A
  model's output is never an observation.
- **Observation schema.** Declares paths, types, and which are required.
  Content-addressed; a predicate's identity includes the schema id, so it
  cannot be evaluated against fields it was not written for.
- **Predicate.** Comparisons and boolean connectives over paths and literals,
  parsed to a canonical tree, content-addressed from that form, with a
  deterministic evaluator that returns true, false, or undetermined when a
  path is absent. Undetermined never fires the action. Arithmetic, functions,
  quantifiers, and time windows are deliberately absent until real classes
  show the need.
- **Executable decision.** Predicate id, schema id, action, declared effects,
  and the class it implements, as one record.

## Where the model stays

- Proposing candidate rules from patterns in a class's adjudications.
- Drafting the predicate and explaining the evidence to the reviewer. A draft
  is a proposal with provenance and abstention allowed; it becomes an
  executable decision only when a person records it.
- Handling the residue that will not reduce to a predicate, such as
  content-quality judgements. That residue lives on Sharkbite's side of the
  line or with a human, never silently inside a bundle.

## What this means for the learning path

The loop measures decision classes first and model weights second. The
selective-distillation work under #188 stands: the weights bundle, the pinned
local provider, and the offline release bundle are the enforcement-plane
option for customers who permit a model. Its remaining pieces are tracked in
#222.

## What this means for the Explorer

The Explorer's primary user is the operator who turns decisions into
automation. The [product plan](platform-product-plan.md#graph-and-document-explorer)
and the [experience specification](explorer-experience-spec.md) already say it
is an evidence-first interface and not a generic node-link visualizer, with
coordinated views and administrative views only after the core workflow is
coherent. The shipped console is a tab per feature over a form per endpoint,
and the one screen it lacks is the one the operator needs.

- The primary screen is an **inbox** of items awaiting a person: pending
  approvals, candidates awaiting promotion, and discovered proposals, grouped
  under their decision class with the class's measured record beside them
  (#219). Each row expands into the chain: registration, evidence with cited
  spans, policy evaluation, class record, and the decision controls.
- Graph, ontology, provenance, and learning become **inspectors** reached
  from an item with that item selected, not top-level destinations.
- Nobody at the screen types or reads a storage key (#223).
- Information architecture before restyling. The fix for an unattractive
  console and the fix for one that does not facilitate the job are the same
  redesign, and the second has to come first.

## Delivery order

1. #218 Class identity and measured record: a read over records that exist.
2. #219 Operator inbox over the pending-approvals and decide routes that
   already exist, with the class record beside each group.
3. #220 Observation record, schema, and predicate form, with golden tests
   pinning canonical form and identity.
4. #221 Bundle format, graduation record, executor, and replay, with promote,
   demote, and export in the inbox.

The weights-path follow-ups (#222) and the described-table follow-ups (#223)
sit behind these four.

## The limit to say out loud

A bundle can only hold decisions that are expressible as predicates over
things Sharkbite observes. Thin observations make thin rules. Today the
observation model is the context pack, so the first classes to measure will
tell us how much real decision volume reduces cleanly, and the measured record
is the instrument for finding out. A class that will not reduce stays on the
enforcement plane or with a person; that is a correct outcome, not a failure
of the bundle.
