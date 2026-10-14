# Retrospective authorization shadow pilot

Protocol: `shoal-authorization-shadow-pilot-v1`. Report: `8bcceeaf6c9b444b88eebc01f7da1d5acf3037a132e41f9fd4e27b14fefa43ea`.

This run preserves full review for every input. Frontier assessments are proposed
relevance labels; the tables below measure agreement, not defect recall or accuracy.

| Observation | Count |
| --- | ---: |
| Pinned PRs | 4 |
| Changed files accounted for | 31 |
| Changed/disposition units | 146 |
| Changed Go functions/methods | 93 |
| Complete function inputs sent to Laya | 55 |
| Functions withheld from Laya due to token limit | 38 |
| Non-function/unsupported units retained for full review | 53 |

Raw Laya labels: `{"insufficient": 14, "review": 10, "routine": 31}`.
Median inference time: 14.35 ms across 55 requests;
first request: 307.60 ms. These are local calls, not service throughput.

## Proposed assessment: codex-frontier

All-unit labels: `{"review": 86, "routine": 47, "unknown": 13}`.

Only the inputs that fit the model are compared below. Unknowns remain visible.

| Laya answer | Proposed reviewer label | Count |
| --- | --- | ---: |
| insufficient | review | 3 |
| insufficient | routine | 11 |
| review | review | 7 |
| review | routine | 3 |
| routine | review | 21 |
| routine | routine | 10 |

Baseline on the same inputs:

| Path/term baseline | Proposed reviewer label | Count |
| --- | --- | ---: |
| review | review | 19 |
| review | routine | 1 |
| routine | review | 12 |
| routine | routine | 23 |

## Proposed assessment: claude-opus-full-diff

All-unit labels: `{"review": 68, "routine": 78}`.

Only the inputs that fit the model are compared below. Unknowns remain visible.

| Laya answer | Proposed reviewer label | Count |
| --- | --- | ---: |
| insufficient | routine | 14 |
| review | review | 6 |
| review | routine | 4 |
| routine | review | 12 |
| routine | routine | 19 |

Baseline on the same inputs:

| Path/term baseline | Proposed reviewer label | Count |
| --- | --- | ---: |
| review | review | 13 |
| review | routine | 7 |
| routine | review | 5 |
| routine | routine | 30 |

Reviewers agree on 105/146 proposed labels. Agreement does not verify those labels.

## Limits

- Retrospective convenience cohort; not held-out quality evidence.
- Proposed frontier relevance labels; no independently confirmed defect oracle.
- Function input excludes dependency bodies; no unit is eligible for exclusion.
- No paired reduced-context downstream reviews, realized token savings, or total-cost estimate.
- Local content-addressed experiment artifacts are not production Shoal decision/API receipts.
