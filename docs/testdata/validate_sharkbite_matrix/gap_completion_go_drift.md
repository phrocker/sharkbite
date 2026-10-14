## Fixture matrix

| ID | Sharkbite | Shoal Go | Shoal C ABI | Evidence | Status | Notes |
| --- | --- | --- | --- | --- | --- | --- |
| SB-CONN-004 | — | — | — | — | Missing Go | |
| SB-SEC-001 | — | — | — | — | Behavior mismatch | |
| SB-SEC-002 | — | — | — | — | Behavior mismatch | |
| SB-NS-001 | — | — | — | — | Behavior mismatch | |
| SB-TABLE-001 | — | — | — | — | Behavior mismatch | |
| SB-TABLE-010 | — | — | — | — | Behavior mismatch | |
| SB-CPP-016 | — | — | — | — | Behavior mismatch | |
| SB-CPP-017 | — | — | — | — | Behavior mismatch | |
| SB-XCUT-012 | — | — | — | — | Covered | |

### 23.1 Stage 1 — Go parity (blocks everything)

| ID | Gap | Matrix rows | Existing issue/PR | Notes |
| --- | --- | --- | --- | --- |
| SB-GAP-GO-001 | Security operations | SB-SEC-001…SB-SEC-002, SB-CONN-004 | merged | **Complete in Go.** |
| SB-GAP-GO-002 | Namespace operations | SB-NS-001 | merged | **Complete in Go.** |

### 23.2 Stage 2 — C ABI parity (blocked by Stage 1 per row)

| ID | Gap | Matrix rows | Existing issue/PR | Notes |
| --- | --- | --- | --- | --- |
| SB-GAP-C-001 | Table administration on the ABI | SB-TABLE-001, SB-CPP-016, SB-CPP-017 | merged | **Complete on the ABI for the listed connector-scoped entry points.** |
| SB-GAP-C-002 | Capability discovery | SB-XCUT-012 | merged | **Complete on the ABI.** |
| SB-GAP-C-004 | Security, namespace, and table-split ABI | SB-SEC-*, SB-NS-001, SB-TABLE-010 | merged | **Complete on the ABI.** |
