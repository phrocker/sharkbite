// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package webapi

import "testing"

func TestDecisionCapabilitiesRemainOffUntilEveryRouteIsMounted(t *testing.T) {
	h := &Handler{}
	for _, capability := range []Capability{
		CapabilityRegistrations,
		CapabilityDatasets,
		CapabilityOutcomes,
		CapabilityAdjudications,
	} {
		h.enableDecisionCapability(capability)
		if h.decisionCapabilities.Decisions {
			t.Fatalf("aggregate decision capability enabled before %s", capability)
		}
	}
	h.enableDecisionCapability(CapabilityDecisions)
	if !h.decisionCapabilities.Decisions {
		t.Fatal("aggregate decision capability stayed disabled after every route")
	}
}

func TestDecisionCapabilitiesOverlayPreservesCoreCapabilities(t *testing.T) {
	base := AllCapabilities()
	got := mergeDecisionCapabilities(base, Capabilities{
		Decisions: true, Registrations: true, Datasets: true,
		Outcomes: true, Adjudications: true,
	})
	if !got.Documents || !got.Retrieve || !got.Ingest {
		t.Fatalf("decision overlay changed core capabilities: %+v", got)
	}
	if !got.Decisions || !got.Registrations || !got.Datasets ||
		!got.Outcomes || !got.Adjudications {
		t.Fatalf("decision overlay did not advertise the complete surface: %+v", got)
	}
	cleared := mergeDecisionCapabilities(got, Capabilities{})
	if cleared.Decisions || cleared.Registrations || cleared.Datasets ||
		cleared.Outcomes || cleared.Adjudications {
		t.Fatalf("unmounted decision capabilities remained advertised: %+v", cleared)
	}
}
