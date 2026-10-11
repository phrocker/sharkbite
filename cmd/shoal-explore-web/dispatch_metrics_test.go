// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
)

type skipReporterStub struct {
	webapi.FleetDispatchProvider
	skipped uint64
}

func (s skipReporterStub) SkippedTransitionPublications() uint64 {
	return s.skipped
}

// TestDispatchMetricsCarriesOnlyTheSkipCounter is the disclosure half of #642.
//
// The health port is unauthenticated, so this writer may carry counters of the
// process's own behaviour and nothing that tracks tenant workload. A count of
// pending work, or an oldest-pending age, names no object and still reports how
// much work a tenant has outstanding, so it is refused on this surface even
// though it would pass a "contains no identifiers" test.
func TestDispatchMetricsCarriesOnlyTheSkipCounter(t *testing.T) {
	writer := dispatchMetrics(skipReporterStub{skipped: 7})
	if writer == nil {
		t.Fatal("a provider reporting skips produced no writer")
	}
	var builder strings.Builder
	writer(&builder)
	rendered := builder.String()

	if !strings.Contains(rendered,
		"shoal_fleet_transition_publications_skipped_total 7") {
		t.Fatalf("rendered = %q, want the skip counter at 7", rendered)
	}
	// Prometheus text needs both, or a scraper rejects the sample.
	for _, want := range []string{
		"# HELP shoal_fleet_transition_publications_skipped_total",
		"# TYPE shoal_fleet_transition_publications_skipped_total counter",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendered = %q, missing %q", rendered, want)
		}
	}
	// The refusals this surface exists to keep. Named individually so a
	// future addition has to delete an assertion and read why.
	for _, forbidden := range []string{
		"pending", "in_flight", "queue_depth", "oldest", "age_seconds",
		"action_id", "subject", "principal", "policy", "source",
	} {
		if strings.Contains(rendered, forbidden) {
			t.Fatalf("rendered = %q, which carries %q: the health port is "+
				"unauthenticated, so it takes counters of this process's own "+
				"behaviour and nothing that tracks tenant workload or names "+
				"an object (#642, #398)", rendered, forbidden)
		}
	}
}

// TestDispatchMetricsIsNilWithoutAReporter pins that a provider that cannot
// report leaves the surface as it was, with /metrics a 404, rather than
// serving an empty body that reads as "zero skips".
func TestDispatchMetricsIsNilWithoutAReporter(t *testing.T) {
	if writer := dispatchMetrics(nil); writer != nil {
		t.Fatal("a nil provider produced a writer")
	}
	var bare webapi.FleetDispatchProvider = bareProviderStub{}
	if writer := dispatchMetrics(bare); writer != nil {
		t.Fatal("a provider without the accessor produced a writer")
	}
}

type bareProviderStub struct{ webapi.FleetDispatchProvider }
