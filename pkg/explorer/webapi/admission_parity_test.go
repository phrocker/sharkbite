// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package webapi

import (
	"testing"

	admissionapi "github.com/phrocker/shoal-oss/pkg/admission/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
)

// TestAdmissionContractConstantsMatchTheFleet guards the copies pkg/admission/api
// carries so that an extension need not import the fleet package. A drift
// here is a client validating against a bound the plane no longer applies.
func TestAdmissionContractConstantsMatchTheFleet(t *testing.T) {
	for _, pair := range []struct {
		name      string
		api, core any
	}{
		{"MaxIDBytes", admissionapi.MaxIDBytes, fleet.MaxActionIDBytes},
		{"MaxDisclosures", admissionapi.MaxDisclosures, fleet.MaxAdmissionDisclosures},
		{"MaxNameBytes", admissionapi.MaxNameBytes, fleet.MaxNameBytes},
		{"MaxLease", admissionapi.MaxLease, fleet.MaxActionClaimTTL},
		{"EffectReadsCorpus", admissionapi.EffectReadsCorpus, string(fleet.EffectReadsCorpus)},
		{"EffectEgressesContent", admissionapi.EffectEgressesContent, string(fleet.EffectEgressesContent)},
		{"EffectMutatesExternal", admissionapi.EffectMutatesExternal, string(fleet.EffectMutatesExternal)},
		{"OutcomeDenied", string(admissionapi.OutcomeDenied), string(fleet.AdmissionDenied)},
		{"OutcomeAllowed", string(admissionapi.OutcomeAllowed), string(fleet.AdmissionAllowed)},
		{"OutcomeObligated", string(admissionapi.OutcomeObligated), string(fleet.AdmissionObligated)},
		{"DispatchQueued", string(admissionapi.DispatchQueued), string(fleet.DispatchQueued)},
		{"DispatchClaimed", string(admissionapi.DispatchClaimed), string(fleet.DispatchClaimed)},
		{"DispatchSucceeded", string(admissionapi.DispatchSucceeded), string(fleet.DispatchSucceeded)},
		{"DispatchFailed", string(admissionapi.DispatchFailed), string(fleet.DispatchFailed)},
		{"DispatchCanceled", string(admissionapi.DispatchCanceled), string(fleet.DispatchCanceled)},
		{"CommitOutcomeHeader", admissionapi.CommitOutcomeHeader, CommitOutcomeHeader},
		{"CommitOutcomeIndeterminate", admissionapi.CommitOutcomeIndeterminate, CommitOutcomeIndeterminate},
		{"RoutePrefix", admissionapi.RoutePrefix, "/api/v1/admission/"},
		{"RequestRoute", admissionapi.RequestRoute, "/api/v1/admission/request"},
		{"ReportRoute", admissionapi.ReportRoute, "/api/v1/admission/report"},
		{"OutstandingRoute", admissionapi.OutstandingRoute, "/api/v1/admission/outstanding"},
	} {
		if pair.api != pair.core {
			t.Errorf("%s: api %v != core %v", pair.name, pair.api, pair.core)
		}
	}
}
