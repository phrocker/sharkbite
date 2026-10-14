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
	"bytes"
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
)

// centroidsFormatVersion mirrors IvfCentroids.FORMAT_VERSION on the Java side.
const centroidsFormatVersion int32 = 1

// Centroids is the coarse IVF quantizer. Rows are unit-normalized and the
// wire format is big-endian / Java DataOutput compatible.
//
// Wire layout:
//
//	int32 FORMAT_VERSION (=1)
//	int32 codebookVersion
//	int32 k
//	int32 dim
//	float32[k*dim] (row-major, big-endian)
type Centroids struct {
	vecs            [][]float32 // k × dim, unit-normalized
	k               int
	dim             int
	codebookVersion int32
	embeddingSpace  string
}

// CentroidsFromBytes parses the veculo-compatible Centroids wire format.
func CentroidsFromBytes(b []byte) (*Centroids, error) {
	r := bytes.NewReader(b)
	var fmtVer, cbVer, k, dim int32
	for _, f := range []struct {
		name string
		out  *int32
	}{
		{"FORMAT_VERSION", &fmtVer}, {"codebookVersion", &cbVer}, {"k", &k}, {"dim", &dim},
	} {
		if err := binary.Read(r, binary.BigEndian, f.out); err != nil {
			return nil, fmt.Errorf("centroids: read %s: %w", f.name, err)
		}
	}
	if fmtVer != centroidsFormatVersion {
		return nil, fmt.Errorf("centroids: unsupported format version %d", fmtVer)
	}
	if k <= 0 || dim <= 0 {
		return nil, fmt.Errorf("centroids: invalid k=%d dim=%d", k, dim)
	}
	vecs := make([][]float32, k)
	for i := range vecs {
		vecs[i] = make([]float32, dim)
		if err := binary.Read(r, binary.BigEndian, vecs[i]); err != nil {
			return nil, fmt.Errorf("centroids: read centroid %d: %w", i, err)
		}
	}
	if r.Len() != 0 {
		return nil, fmt.Errorf("centroids: trailing bytes %d", r.Len())
	}
	return &Centroids{vecs: vecs, k: int(k), dim: int(dim), codebookVersion: cbVer}, nil
}

func (c *Centroids) WithEmbeddingSpace(identity string) (*Centroids, error) {
	if err := embeddingspace.Has(identity).Validate(); err != nil {
		return nil, err
	}
	clone := *c
	clone.embeddingSpace = identity
	return &clone, nil
}

func (c *Centroids) EmbeddingSpaceIdentity() (string, bool) {
	if c == nil || c.embeddingSpace == "" {
		return "", false
	}
	return c.embeddingSpace, true
}

func (c *Centroids) EnsureQuerySpace(identity string) error {
	if c == nil || c.embeddingSpace == "" {
		return nil
	}
	return embeddingspace.EnsureSameIdentity("compare centroids", c.embeddingSpace, identity)
}

// Bytes serialises the Centroids in veculo-compatible big-endian wire format.
func (c *Centroids) Bytes() ([]byte, error) {
	var buf bytes.Buffer
	wr := func(v any) error {
		return binary.Write(&buf, binary.BigEndian, v)
	}
	if err := wr(centroidsFormatVersion); err != nil {
		return nil, err
	}
	if err := wr(c.codebookVersion); err != nil {
		return nil, err
	}
	if err := wr(int32(c.k)); err != nil {
		return nil, err
	}
	if err := wr(int32(c.dim)); err != nil {
		return nil, err
	}
	for _, row := range c.vecs {
		if err := wr(row); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// K returns the number of centroids.
func (c *Centroids) K() int { return c.k }

// Dim returns the vector dimension.
func (c *Centroids) Dim() int { return c.dim }

// CodebookVersion returns the version tag stamped at training time.
func (c *Centroids) CodebookVersion() int32 { return c.codebookVersion }

// Assign returns the index of the centroid with the highest inner product
// with vec (assumes vec and centroids are unit-normalized). Ties resolve to
// the lowest centroid index.
func (c *Centroids) Assign(vec []float32) int {
	best := 0
	bestIP := centroidIP(vec, c.vecs[0])
	for i := 1; i < c.k; i++ {
		ip := centroidIP(vec, c.vecs[i])
		if ip > bestIP {
			bestIP = ip
			best = i
		}
	}
	return best
}

// NProbe returns the indices of the nprobe centroids with the highest inner
// products with query, in descending order. Ties resolve to the lowest index.
// If nprobe >= K(), all K() indices are returned.
func (c *Centroids) NProbe(query []float32, nprobe int) []int {
	if nprobe <= 0 {
		return nil
	}
	if nprobe > c.k {
		nprobe = c.k
	}
	type scored struct {
		idx int
		ip  float32
	}
	ss := make([]scored, c.k)
	for i := range ss {
		ss[i] = scored{i, centroidIP(query, c.vecs[i])}
	}
	sort.SliceStable(ss, func(a, b int) bool {
		if ss[a].ip != ss[b].ip {
			return ss[a].ip > ss[b].ip // descending
		}
		return ss[a].idx < ss[b].idx // ties: lowest index first
	})
	out := make([]int, nprobe)
	for i := range out {
		out[i] = ss[i].idx
	}
	return out
}

func (c *Centroids) NProbeInSpace(query []float32, nprobe int, identity string) ([]int, error) {
	if err := c.EnsureQuerySpace(identity); err != nil {
		return nil, err
	}
	return c.NProbe(query, nprobe), nil
}

// centroidIP computes the inner product between two float32 slices.
func centroidIP(a, b []float32) float32 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var s float32
	for i := 0; i < n; i++ {
		s += a[i] * b[i]
	}
	return s
}
