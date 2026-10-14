// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package eval

import (
	"context"
	"os"
	"testing"

	"github.com/phrocker/shoal-oss/internal/routerwire"
)

// Benchmarks run over the train and dev texts only; the test split is not
// read. Each iteration routes one case, cycling through them.
func benchCases(b *testing.B) (*Runner, []Case) {
	w, cases := load(b)
	model, err := os.ReadFile(modelPath)
	if err != nil {
		b.Fatal(err)
	}
	provider, err := NewProvider(model)
	if err != nil {
		b.Fatal(err)
	}
	train, err := cases.Split("train")
	if err != nil {
		b.Fatal(err)
	}
	dev, err := cases.Split("dev")
	if err != nil {
		b.Fatal(err)
	}
	selected := append(train, dev...)
	r := NewRunner(w, routerwire.Predictor(provider, ReleaseID), routerwire.Validator())
	for _, caller := range w.Callers() {
		if _, err := r.Catalog(caller); err != nil {
			b.Fatal(err)
		}
	}
	return r, selected
}

// BenchmarkAnalyze: tokenize, resolve mentions (in-memory lexicon), match
// every visible grammar and featurize.
func BenchmarkAnalyze(b *testing.B) {
	r, cases := benchCases(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := r.Analyze(cases[i%len(cases)]); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRoute: Analyze, then the target-choice decision (task, picture,
// request, decisionlinear predict, prediction record), aggregation, slot
// validation, and the baseline.
func BenchmarkRoute(b *testing.B) {
	r, cases := benchCases(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := r.Route(ctx, cases[i%len(cases)]); err != nil {
			b.Fatal(err)
		}
	}
}
