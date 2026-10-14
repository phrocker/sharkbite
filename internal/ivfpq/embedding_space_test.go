// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file distributed
// with this work for additional information regarding copyright ownership.
// The ASF licenses this file to you under the Apache License, Version 2.0.
package ivfpq

import (
	"errors"
	"testing"

	"github.com/phrocker/shoal-oss/internal/embeddingspace"
)

func TestTrainRejectsDifferingEmbeddingSpacesSameDimension(t *testing.T) {
	samples := []TrainingSample{
		{Vector: []float32{1, 0}, EmbeddingSpace: "provider:model-a:2:l2"},
		{Vector: []float32{0, 1}, EmbeddingSpace: "provider:model-b:2:l2"},
	}
	if _, err := TrainCentroidsForSamples(samples, 1, 1, 1, 1); !errors.Is(err, embeddingspace.ErrMismatch) {
		t.Fatalf("TrainCentroidsForSamples error = %v, want ErrMismatch", err)
	}
	if _, err := TrainPQForSamples(samples, 1, 1, 1, 1, 1); !errors.Is(err, embeddingspace.ErrMismatch) {
		t.Fatalf("TrainPQForSamples error = %v, want ErrMismatch", err)
	}
}

func TestCompareRejectsDifferingEmbeddingSpacesSameDimension(t *testing.T) {
	pq, err := TrainPQInSpace([][]float32{{1, 0}, {0, 1}}, 1, 1, 1, 1, 1, "provider:model-a:2:l2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pq.InnerProductTableInSpace([]float32{1, 0}, "provider:model-b:2:l2"); !errors.Is(err, embeddingspace.ErrMismatch) {
		t.Fatalf("InnerProductTableInSpace error = %v, want ErrMismatch", err)
	}
	centroids, err := TrainCentroidsInSpace([][]float32{{1, 0}, {0, 1}}, 1, 1, 1, 1, "provider:model-a:2:l2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := centroids.NProbeInSpace([]float32{1, 0}, 1, "provider:model-b:2:l2"); !errors.Is(err, embeddingspace.ErrMismatch) {
		t.Fatalf("NProbeInSpace error = %v, want ErrMismatch", err)
	}
}
