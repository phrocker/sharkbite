// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package iterrt

import (
	"fmt"
	"strconv"
	"testing"
)

// TestGraphRankParity_DeterministicIncomingOrder pins the rank value for a
// vertex with many incoming neighbors whose contributions have different
// magnitudes. Without canonical incoming-neighbor ordering, Go map iteration
// changes the floating-point addition order and the exact emitted value can
// drift even though the graph is unchanged.
func TestGraphRankParity_DeterministicIncomingOrder(t *testing.T) {
	const sourceCount = 160
	const damping = 1e16
	cells := make([]kv, 0, sourceCount*4)
	cells = append(cells, kv{mk("target", "V", "_label", "", 900), []byte("target")})
	for i := 0; i < sourceCount; i++ {
		src := fmt.Sprintf("src%03d", i)
		cells = append(cells,
			kv{mk(src, "V", "_label", "", int64(i+1)), []byte(src)},
			kv{mk(src, "E_rank", "target", "", int64(1000+i)), []byte("{}")},
		)
		if i%2 == 1 {
			for booster := 0; booster < 2; booster++ {
				boost := fmt.Sprintf("boost%03d_%d", i, booster)
				cells = append(cells,
					kv{mk(boost, "V", "_label", "", int64(2000+i*2+booster)), []byte(boost)},
					kv{mk(boost, "E_rank", src, "", int64(3000+i*2+booster)), []byte("{}")},
				)
			}
		}
	}
	cells = sortGraphRankInput(cells)

	g := initGraphRank(t, newSliceSource(cells...), map[string]string{
		GraphRankDampingFactor:        fmt.Sprintf("%.0f", damping),
		GraphRankMaxIterations:        "2",
		GraphRankConvergenceThreshold: "0",
	})
	got, err := drain(g)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	ranks := graphRankRankCells(t, got, "V", "_rank")
	target, ok := ranks["target"]
	if !ok {
		t.Fatalf("missing target rank")
	}

	n := float64(1 + sourceCount + sourceCount)
	base := (1.0 - damping) / n
	iter1Initial := 1.0 / n
	expectedSum := 0.0
	for i := 0; i < sourceCount; i++ {
		sourceRank := base
		if i%2 == 1 {
			sourceRank += damping * (2 * iter1Initial)
		}
		expectedSum += sourceRank
	}
	expected := base + damping*expectedSum
	want := strconv.FormatFloat(expected, 'g', -1, 64)
	if string(target.v) != want {
		t.Fatalf("target rank = %q, want deterministic sorted-order value %q", target.v, want)
	}
}
