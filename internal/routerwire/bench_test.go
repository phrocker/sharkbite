// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package routerwire

import (
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
)

// BenchmarkServiceRoute is the whole shadow path on the composed world
// (memory policy store, real explorer corpus): enumeration, authorized
// mention resolution, analysis, the target-choice decision, the baseline and
// an in-memory record.
func BenchmarkServiceRoute(b *testing.B) {
	w := newWorld(b, authorized.NewMemoryPolicyStore(), true)
	ctx := w.alice()
	texts := []string{"restart payments in prod", "how risky is restarting payments", "flush the checkout cache", "tell me a joke"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := w.service.Route(ctx, texts[i%len(texts)]); err != nil {
			b.Fatal(err)
		}
	}
}
