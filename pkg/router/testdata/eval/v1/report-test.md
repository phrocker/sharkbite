| Metric | router (test) | baseline (test) |
|---|---|---|
| Target precision (non-abstained) | 9 of 9 (100.0%) | 32 of 32 (100.0%) |
| End-to-end accuracy | 36 of 103 (35.0%) | 59 of 103 (57.3%) |
| Coverage (answerable) | 9 of 74 (12.2%) | 32 of 74 (43.2%) |
| Abstention rate | 94 of 103 (91.3%) | 71 of 103 (68.9%) |
| Correct abstention (unanswerable) | 29 of 29 (100.0%) | 29 of 29 (100.0%) |
| Slot exact-match (target-correct) | 7 of 9 (77.8%) | 30 of 32 (93.8%) |
| Abstention reason agreement (secondary) | 24 of 29 (82.8%) | 23 of 29 (79.3%) |

Discordant pairs (end-to-end): router right and baseline wrong 4, baseline right and router wrong 27; both right 32, both wrong 40; exact McNemar p = 3.395e-05.

Confusion, router (rows expected, columns proposed):

| expected \ proposed | action | decision | lookup | abstain |
|---|---|---|---|---|
| action | 7 | 0 | 0 | 29 |
| decision | 0 | 2 | 0 | 14 |
| lookup | 0 | 0 | 0 | 22 |
| abstain | 0 | 0 | 0 | 29 |

End-to-end by tag, router: ambiguous_executor 2 of 2 (100.0%); ambiguous_mention 2 of 2 (100.0%); hidden_node 2 of 2 (100.0%); hidden_target 2 of 4 (50.0%); injection 2 of 2 (100.0%); missing_slot 3 of 3 (100.0%); nonexistent_node 1 of 1 (100.0%); nonexistent_target 1 of 1 (100.0%); plain 4 of 69 (5.8%); slot_mismatch 1 of 1 (100.0%); unanswerable 16 of 16 (100.0%).

Confusion, baseline (rows expected, columns proposed):

| expected \ proposed | action | decision | lookup | abstain |
|---|---|---|---|---|
| action | 16 | 0 | 0 | 20 |
| decision | 0 | 6 | 0 | 10 |
| lookup | 0 | 0 | 10 | 12 |
| abstain | 0 | 0 | 0 | 29 |

End-to-end by tag, baseline: ambiguous_executor 2 of 2 (100.0%); ambiguous_mention 2 of 2 (100.0%); hidden_node 2 of 2 (100.0%); hidden_target 3 of 4 (75.0%); injection 0 of 2 (0.0%); missing_slot 3 of 3 (100.0%); nonexistent_node 1 of 1 (100.0%); nonexistent_target 1 of 1 (100.0%); plain 28 of 69 (40.6%); slot_mismatch 1 of 1 (100.0%); unanswerable 16 of 16 (100.0%).
