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

package ivfpq

import (
	"fmt"
	"math"
)

// New constructs a VectorPQ from a pre-trained codebook.
// codebook[m][ks][dsub] where dsub = dim/m.
func New(codebook [][][]float32, dim int, codebookVersion int32) (*VectorPQ, error) {
	m := len(codebook)
	if m == 0 {
		return nil, fmt.Errorf("ivfpq: empty codebook")
	}
	ks := len(codebook[0])
	if ks == 0 || ks > 256 {
		return nil, fmt.Errorf("ivfpq: invalid ks=%d (must be 1..256)", ks)
	}
	if dim <= 0 || dim%m != 0 {
		return nil, fmt.Errorf("ivfpq: dim %d not divisible by m %d", dim, m)
	}
	dsub := dim / m
	for i, sub := range codebook {
		if len(sub) != ks {
			return nil, fmt.Errorf("ivfpq: codebook[%d] has %d centroids; want %d", i, len(sub), ks)
		}
		for c, entry := range sub {
			if len(entry) != dsub {
				return nil, fmt.Errorf("ivfpq: codebook[%d][%d] has len %d; want %d", i, c, len(entry), dsub)
			}
		}
	}
	return &VectorPQ{
		codebook:        codebook,
		m:               m,
		ks:              ks,
		dim:             dim,
		dsub:            dsub,
		codebookVersion: codebookVersion,
	}, nil
}

// Encode encodes vec into an M-byte PQ code. Each byte is the index (0..Ks-1)
// of the nearest centroid in that subspace by squared Euclidean distance.
// Ties resolve to the lowest centroid index. Returns an error if len(vec) != Dim().
func (p *VectorPQ) Encode(vec []float32) ([]byte, error) {
	if len(vec) != p.dim {
		return nil, fmt.Errorf("ivfpq: Encode: vec dim %d != PQ dim %d", len(vec), p.dim)
	}
	code := make([]byte, p.m)
	for s := 0; s < p.m; s++ {
		off := s * p.dsub
		sub := vec[off : off+p.dsub]
		best := 0
		bestDist := float32(math.MaxFloat32)
		for c := 0; c < p.ks; c++ {
			d := sqEuclidean(sub, p.codebook[s][c])
			if d < bestDist {
				bestDist = d
				best = c
			}
		}
		code[s] = byte(best)
	}
	return code, nil
}

// sqEuclidean returns the squared Euclidean distance between a and b.
func sqEuclidean(a, b []float32) float32 {
	var sum float32
	for i := range a {
		diff := a[i] - b[i]
		sum += diff * diff
	}
	return sum
}
