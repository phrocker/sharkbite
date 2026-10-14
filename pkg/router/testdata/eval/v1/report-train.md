| Metric | router (train) | baseline (train) |
|---|---|---|
| Target precision (non-abstained) | 101 of 101 (100.0%) | 99 of 99 (100.0%) |
| End-to-end accuracy | 139 of 139 (100.0%) | 137 of 139 (98.6%) |
| Coverage (answerable) | 101 of 101 (100.0%) | 99 of 101 (98.0%) |
| Abstention rate | 38 of 139 (27.3%) | 40 of 139 (28.8%) |
| Correct abstention (unanswerable) | 38 of 38 (100.0%) | 38 of 38 (100.0%) |
| Slot exact-match (target-correct) | 101 of 101 (100.0%) | 99 of 99 (100.0%) |
| Abstention reason agreement (secondary) | 36 of 38 (94.7%) | 35 of 38 (92.1%) |

Discordant pairs (end-to-end): router right and baseline wrong 2, baseline right and router wrong 0; both right 137, both wrong 0; exact McNemar p = 0.5.

Confusion, router (rows expected, columns proposed):

| expected \ proposed | action | decision | lookup | abstain |
|---|---|---|---|---|
| action | 58 | 0 | 0 | 0 |
| decision | 0 | 19 | 0 | 0 |
| lookup | 0 | 0 | 24 | 0 |
| abstain | 0 | 0 | 0 | 38 |

End-to-end by tag, router: ambiguous_executor 2 of 2 (100.0%); ambiguous_mention 2 of 2 (100.0%); hidden_node 2 of 2 (100.0%); hidden_target 4 of 4 (100.0%); injection 2 of 2 (100.0%); missing_slot 4 of 4 (100.0%); nonexistent_node 1 of 1 (100.0%); nonexistent_target 1 of 1 (100.0%); plain 96 of 96 (100.0%); slot_mismatch 1 of 1 (100.0%); unanswerable 24 of 24 (100.0%).

Confusion, baseline (rows expected, columns proposed):

| expected \ proposed | action | decision | lookup | abstain |
|---|---|---|---|---|
| action | 58 | 0 | 0 | 0 |
| decision | 0 | 17 | 0 | 2 |
| lookup | 0 | 0 | 24 | 0 |
| abstain | 0 | 0 | 0 | 38 |

End-to-end by tag, baseline: ambiguous_executor 2 of 2 (100.0%); ambiguous_mention 2 of 2 (100.0%); hidden_node 2 of 2 (100.0%); hidden_target 4 of 4 (100.0%); injection 0 of 2 (0.0%); missing_slot 4 of 4 (100.0%); nonexistent_node 1 of 1 (100.0%); nonexistent_target 1 of 1 (100.0%); plain 96 of 96 (100.0%); slot_mismatch 1 of 1 (100.0%); unanswerable 24 of 24 (100.0%).
