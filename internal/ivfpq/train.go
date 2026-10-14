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
	"math/rand"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
)

type TrainingSample struct {
	Vector         []float32
	EmbeddingSpace string
}

func TrainCentroidsForSamples(samples []TrainingSample, k, maxIter int, seed int64, codebookVersion int32) (*Centroids, error) {
	vectors, identity, err := normalizeTrainingSamples("train centroids", samples)
	if err != nil {
		return nil, err
	}
	return TrainCentroidsInSpace(vectors, k, maxIter, seed, codebookVersion, identity)
}

func TrainPQForSamples(samples []TrainingSample, m, ks, maxIter int, seed int64, codebookVersion int32) (*VectorPQ, error) {
	vectors, identity, err := normalizeTrainingSamples("train product quantizer", samples)
	if err != nil {
		return nil, err
	}
	return TrainPQInSpace(vectors, m, ks, maxIter, seed, codebookVersion, identity)
}

func normalizeTrainingSamples(operation string, samples []TrainingSample) ([][]float32, string, error) {
	if len(samples) == 0 {
		return nil, "", nil
	}
	identity := samples[0].EmbeddingSpace
	if err := requireSampleSpace(operation, identity); err != nil {
		return nil, "", err
	}
	vectors := make([][]float32, len(samples))
	for i, sample := range samples {
		if err := embeddingspace.EnsureSameIdentity(operation, identity, sample.EmbeddingSpace); err != nil {
			return nil, "", err
		}
		vectors[i] = sample.Vector
	}
	return vectors, identity, nil
}

// TrainCentroids trains k coarse IVF centroids using spherical k-means:
// samples are unit-normalised, assignment is by maximum inner product, and
// centroids are re-normalised after each Lloyd iteration.  Empty clusters
// retain their previous centroid.  Training is fully deterministic for a given
// (samples, k, maxIter, seed) tuple.
func TrainCentroids(samples [][]float32, k, maxIter int, seed int64, codebookVersion int32) (*Centroids, error) {
	if len(samples) == 0 {
		return nil, fmt.Errorf("ivfpq: TrainCentroids: no samples")
	}
	if k <= 0 || k > len(samples) {
		return nil, fmt.Errorf("ivfpq: TrainCentroids: k=%d out of range [1,%d]", k, len(samples))
	}
	dim := len(samples[0])

	// Unit-normalise samples into a local copy.
	norm := make([][]float32, len(samples))
	for i, s := range samples {
		norm[i] = unitNorm(s)
	}

	rng := rand.New(rand.NewSource(seed))
	centroids := kppSphere(norm, k, rng)

	assign := make([]int, len(norm))
	for iter := 0; iter < maxIter; iter++ {
		// Assignment: argmax inner product, ties → lowest index.
		for i, v := range norm {
			best := 0
			bestIP := centroidIP(v, centroids[0])
			for c := 1; c < k; c++ {
				ip := centroidIP(v, centroids[c])
				if ip > bestIP {
					bestIP = ip
					best = c
				}
			}
			assign[i] = best
		}

		// Update: mean of assigned vectors then normalise.
		next := make([][]float32, k)
		for c := range next {
			next[c] = make([]float32, dim)
		}
		counts := make([]int, k)
		for i, c := range assign {
			counts[c]++
			for d := 0; d < dim; d++ {
				next[c][d] += norm[i][d]
			}
		}
		for c := 0; c < k; c++ {
			if counts[c] == 0 {
				// Empty cluster: keep previous centroid unchanged.
				copy(next[c], centroids[c])
			} else {
				n := float32(counts[c])
				for d := 0; d < dim; d++ {
					next[c][d] /= n
				}
				normInPlace(next[c])
			}
		}
		centroids = next
	}

	return &Centroids{
		vecs:            centroids,
		k:               k,
		dim:             dim,
		codebookVersion: codebookVersion,
	}, nil
}

func TrainCentroidsInSpace(samples [][]float32, k, maxIter int, seed int64, codebookVersion int32, identity string) (*Centroids, error) {
	if err := requireSampleSpace("train centroids", identity); err != nil {
		return nil, err
	}
	c, err := TrainCentroids(samples, k, maxIter, seed, codebookVersion)
	if err != nil {
		return nil, err
	}
	c.embeddingSpace = identity
	return c, nil
}

// TrainPQ trains a PQ codebook by running Euclidean k-means independently on
// each of the M sub-vector spaces.  Training is fully deterministic for a
// given (samples, m, ks, maxIter, seed) tuple (each subspace uses
// seed+subspace_index as its own RNG seed).
func TrainPQ(samples [][]float32, m, ks, maxIter int, seed int64, codebookVersion int32) (*VectorPQ, error) {
	if len(samples) == 0 {
		return nil, fmt.Errorf("ivfpq: TrainPQ: no samples")
	}
	if m <= 0 {
		return nil, fmt.Errorf("ivfpq: TrainPQ: m=%d must be > 0", m)
	}
	dim := len(samples[0])
	if dim%m != 0 {
		return nil, fmt.Errorf("ivfpq: TrainPQ: dim %d not divisible by m %d", dim, m)
	}
	if ks < 1 || ks > 256 {
		return nil, fmt.Errorf("ivfpq: TrainPQ: ks=%d must be in [1,256]", ks)
	}
	if ks > len(samples) {
		return nil, fmt.Errorf("ivfpq: TrainPQ: ks=%d > n=%d samples", ks, len(samples))
	}
	dsub := dim / m

	codebook := make([][][]float32, m)
	for s := 0; s < m; s++ {
		// Extract sub-vectors for this subspace (slice into originals, read-only).
		subs := make([][]float32, len(samples))
		for i, v := range samples {
			subs[i] = v[s*dsub : (s+1)*dsub]
		}

		rng := rand.New(rand.NewSource(seed + int64(s)))
		centroids := kppEuclidean(subs, ks, rng)

		assign := make([]int, len(subs))
		for iter := 0; iter < maxIter; iter++ {
			// Assignment: argmin squared Euclidean, ties → lowest index.
			for i, v := range subs {
				best := 0
				bestD := sqEuclidean(v, centroids[0])
				for c := 1; c < ks; c++ {
					d := sqEuclidean(v, centroids[c])
					if d < bestD {
						bestD = d
						best = c
					}
				}
				assign[i] = best
			}

			// Update: mean of assigned sub-vectors.
			next := make([][]float32, ks)
			for c := range next {
				next[c] = make([]float32, dsub)
			}
			counts := make([]int, ks)
			for i, c := range assign {
				counts[c]++
				for d := 0; d < dsub; d++ {
					next[c][d] += subs[i][d]
				}
			}
			for c := 0; c < ks; c++ {
				if counts[c] == 0 {
					// Empty cluster: keep previous centroid unchanged.
					copy(next[c], centroids[c])
				} else {
					n := float32(counts[c])
					for d := 0; d < dsub; d++ {
						next[c][d] /= n
					}
				}
			}
			centroids = next
		}
		codebook[s] = centroids
	}

	return New(codebook, dim, codebookVersion)
}

func TrainPQInSpace(samples [][]float32, m, ks, maxIter int, seed int64, codebookVersion int32, identity string) (*VectorPQ, error) {
	if err := requireSampleSpace("train product quantizer", identity); err != nil {
		return nil, err
	}
	pq, err := TrainPQ(samples, m, ks, maxIter, seed, codebookVersion)
	if err != nil {
		return nil, err
	}
	pq.embeddingSpace = identity
	return pq, nil
}

func requireSampleSpace(operation, identity string) error {
	if err := embeddingspace.Has(identity).Validate(); err != nil {
		return err
	}
	return nil
}

// kppSphere picks k initial centroids from unit-normalised samples using
// k-means++ seeding where "distance" is 1 - inner_product (higher IP = closer).
func kppSphere(samples [][]float32, k int, rng *rand.Rand) [][]float32 {
	centroids := make([][]float32, 0, k)

	// First centroid uniformly at random.
	c0 := copyVec(samples[rng.Intn(len(samples))])
	centroids = append(centroids, c0)

	dists := make([]float64, len(samples))
	for pick := 1; pick < k; pick++ {
		var total float64
		for i, v := range samples {
			maxIP := float32(-math.MaxFloat32)
			for _, c := range centroids {
				if ip := centroidIP(v, c); ip > maxIP {
					maxIP = ip
				}
			}
			d := float64(1.0-maxIP) * float64(1.0-maxIP)
			if d < 0 {
				d = 0
			}
			dists[i] = d
			total += d
		}
		target := rng.Float64() * total
		cum := 0.0
		chosen := len(samples) - 1
		for i, d := range dists {
			cum += d
			if cum >= target {
				chosen = i
				break
			}
		}
		centroids = append(centroids, copyVec(samples[chosen]))
	}
	return centroids
}

// kppEuclidean picks k initial centroids using k-means++ with squared
// Euclidean distance.
func kppEuclidean(samples [][]float32, k int, rng *rand.Rand) [][]float32 {
	centroids := make([][]float32, 0, k)

	c0 := copyVec(samples[rng.Intn(len(samples))])
	centroids = append(centroids, c0)

	dists := make([]float64, len(samples))
	for pick := 1; pick < k; pick++ {
		var total float64
		for i, v := range samples {
			minD := math.MaxFloat64
			for _, c := range centroids {
				if d := float64(sqEuclidean(v, c)); d < minD {
					minD = d
				}
			}
			dists[i] = minD
			total += minD
		}
		target := rng.Float64() * total
		cum := 0.0
		chosen := len(samples) - 1
		for i, d := range dists {
			cum += d
			if cum >= target {
				chosen = i
				break
			}
		}
		centroids = append(centroids, copyVec(samples[chosen]))
	}
	return centroids
}

// unitNorm returns a unit-normalised copy of v.
func unitNorm(v []float32) []float32 {
	out := make([]float32, len(v))
	copy(out, v)
	normInPlace(out)
	return out
}

// normInPlace unit-normalises v in place (no-op for zero vectors).
func normInPlace(v []float32) {
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		return
	}
	n := float32(math.Sqrt(sum))
	for i := range v {
		v[i] /= n
	}
}

func copyVec(v []float32) []float32 {
	out := make([]float32, len(v))
	copy(out, v)
	return out
}
