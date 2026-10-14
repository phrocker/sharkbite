| Metric | router (dev) | baseline (dev) |
|---|---|---|
| Target precision (non-abstained) | 37 of 37 (100.0%) | 37 of 37 (100.0%) |
| End-to-end accuracy | 50 of 50 (100.0%) | 50 of 50 (100.0%) |
| Coverage (answerable) | 37 of 37 (100.0%) | 37 of 37 (100.0%) |
| Abstention rate | 13 of 50 (26.0%) | 13 of 50 (26.0%) |
| Correct abstention (unanswerable) | 13 of 13 (100.0%) | 13 of 13 (100.0%) |
| Slot exact-match (target-correct) | 37 of 37 (100.0%) | 37 of 37 (100.0%) |
| Abstention reason agreement (secondary) | 12 of 13 (92.3%) | 11 of 13 (84.6%) |

Discordant pairs (end-to-end): router right and baseline wrong 0, baseline right and router wrong 0; both right 50, both wrong 0; exact McNemar p = 1.

Confusion, router (rows expected, columns proposed):

| expected \ proposed | action | decision | lookup | abstain |
|---|---|---|---|---|
| action | 18 | 0 | 0 | 0 |
| decision | 0 | 7 | 0 | 0 |
| lookup | 0 | 0 | 12 | 0 |
| abstain | 0 | 0 | 0 | 13 |

End-to-end by tag, router: ambiguous_executor 1 of 1 (100.0%); ambiguous_mention 1 of 1 (100.0%); hidden_node 2 of 2 (100.0%); hidden_target 2 of 2 (100.0%); injection 2 of 2 (100.0%); missing_slot 2 of 2 (100.0%); plain 33 of 33 (100.0%); unanswerable 7 of 7 (100.0%).

Confusion, baseline (rows expected, columns proposed):

| expected \ proposed | action | decision | lookup | abstain |
|---|---|---|---|---|
| action | 18 | 0 | 0 | 0 |
| decision | 0 | 7 | 0 | 0 |
| lookup | 0 | 0 | 12 | 0 |
| abstain | 0 | 0 | 0 | 13 |

End-to-end by tag, baseline: ambiguous_executor 1 of 1 (100.0%); ambiguous_mention 1 of 1 (100.0%); hidden_node 2 of 2 (100.0%); hidden_target 2 of 2 (100.0%); injection 2 of 2 (100.0%); missing_slot 2 of 2 (100.0%); plain 33 of 33 (100.0%); unanswerable 7 of 7 (100.0%).
