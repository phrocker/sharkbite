// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package collector_test

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/collector"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

var observed = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

func config() collector.ObservationConfig {
	return collector.ObservationConfig{CollectorID: "collector", ArtifactID: "artifact", Extractor: collector.ExtractorRef{ID: "extractor", Version: "1"}, Confidence: collector.Confidence{Disposition: collector.Extracted}, Kind: "log_line", Payload: []byte("x"), ObservedAt: observed}
}

func TestObservationIdentityBindsExtractorVersionAndContent(t *testing.T) {
	base, e := collector.NewObservation(config())
	if e != nil {
		t.Fatal(e)
	}
	if !collector.ValidObservationID(base.ID()) || base.Validate() != nil {
		t.Fatal("invalid base identity")
	}
	again, _ := collector.NewObservation(config())
	if again.ID() != base.ID() {
		t.Fatal("identity is not deterministic")
	}
	value := 0.5
	for name, change := range map[string]func(*collector.ObservationConfig){
		"version":    func(c *collector.ObservationConfig) { c.Extractor.Version = "2" },
		"extractor":  func(c *collector.ObservationConfig) { c.Extractor.ID = "other" },
		"artifact":   func(c *collector.ObservationConfig) { c.ArtifactID = "other" },
		"collector":  func(c *collector.ObservationConfig) { c.CollectorID = "other" },
		"payload":    func(c *collector.ObservationConfig) { c.Payload = []byte("y") },
		"confidence": func(c *collector.ObservationConfig) { c.Confidence.Value = &value },
		"time":       func(c *collector.ObservationConfig) { c.ObservedAt = observed.Add(time.Second) },
		"subject":    func(c *collector.ObservationConfig) { c.SubjectID = "s" },
	} {
		c := config()
		change(&c)
		o, e := collector.NewObservation(c)
		if e != nil || o.ID() == base.ID() {
			t.Fatalf("%s did not change identity: %v", name, e)
		}
	}
	empty, nilPayload := config(), config()
	empty.Payload, nilPayload.Payload = []byte{}, nil
	a, _ := collector.NewObservation(empty)
	b, _ := collector.NewObservation(nilPayload)
	if a.ID() != b.ID() || a.Config().Payload != nil {
		t.Fatal("empty and absent payloads differ")
	}
	// The returned config does not alias the observation.
	c := base.Config()
	c.Payload[0] = 'z'
	if base.Config().Payload[0] != 'x' {
		t.Fatal("config aliases observation payload")
	}
}

func TestObservationValidation(t *testing.T) {
	nan, high := math.NaN(), 1.5
	for name, change := range map[string]func(*collector.ObservationConfig){
		"no collector":          func(c *collector.ObservationConfig) { c.CollectorID = "" },
		"no version":            func(c *collector.ObservationConfig) { c.Extractor.Version = "" },
		"spaced version":        func(c *collector.ObservationConfig) { c.Extractor.Version = "1 0" },
		"bad disposition":       func(c *collector.ObservationConfig) { c.Confidence.Disposition = "trusted" },
		"nan confidence":        func(c *collector.ObservationConfig) { c.Confidence.Value = &nan },
		"confidence above one":  func(c *collector.ObservationConfig) { c.Confidence.Value = &high },
		"oversize payload":      func(c *collector.ObservationConfig) { c.Payload = make([]byte, collector.MaxPayloadBytes+1) },
		"unextractable payload": func(c *collector.ObservationConfig) { c.Confidence.Disposition = collector.Unextractable },
		"local time":            func(c *collector.ObservationConfig) { c.ObservedAt = observed.In(time.FixedZone("x", 3600)) },
		"zero time":             func(c *collector.ObservationConfig) { c.ObservedAt = time.Time{} },
		"no kind":               func(c *collector.ObservationConfig) { c.Kind = "" },
		"invalid utf8 subject":  func(c *collector.ObservationConfig) { c.SubjectID = "\xff" },
	} {
		c := config()
		change(&c)
		if _, e := collector.NewObservation(c); e == nil {
			t.Fatal(name, "accepted")
		}
	}
}

func TestEnrollRequestCanonicalization(t *testing.T) {
	r := collector.EnrollRequest{CollectorID: "c", RequestedAuthorityPolicyIDs: []shoal.ID{"b", "a"}, Extractors: []collector.ExtractorRef{{ID: "x", Version: "2"}, {ID: "x", Version: "1"}}}
	c, e := r.Canonical()
	if e != nil || c.RequestedAuthorityPolicyIDs[0] != "a" || c.Extractors[0].Version != "1" {
		t.Fatalf("canonical: %+v %v", c, e)
	}
	d1, _ := r.Digest()
	d2, _ := c.Digest()
	if d1 != d2 {
		t.Fatal("digest depends on order")
	}
	for name, bad := range map[string]collector.EnrollRequest{
		"no authority":         {CollectorID: "c", Extractors: r.Extractors},
		"duplicate authority":  {CollectorID: "c", RequestedAuthorityPolicyIDs: []shoal.ID{"a", "a"}, Extractors: r.Extractors},
		"no extractors":        {CollectorID: "c", RequestedAuthorityPolicyIDs: []shoal.ID{"a"}},
		"duplicate extractors": {CollectorID: "c", RequestedAuthorityPolicyIDs: []shoal.ID{"a"}, Extractors: []collector.ExtractorRef{{ID: "x", Version: "1"}, {ID: "x", Version: "1"}}},
		"empty evidence":       {CollectorID: "c", RequestedAuthorityPolicyIDs: []shoal.ID{"a"}, Extractors: r.Extractors, Attestation: &collector.AttestationReport{Kind: "k", Format: "f"}},
		"oversize evidence":    {CollectorID: "c", RequestedAuthorityPolicyIDs: []shoal.ID{"a"}, Extractors: r.Extractors, Attestation: &collector.AttestationReport{Kind: "k", Format: "f", Evidence: make([]byte, collector.MaxEvidenceBytes+1)}},
	} {
		if _, e := bad.Canonical(); e == nil {
			t.Fatal(name, "accepted")
		}
	}
	if collector.EnrollNonce("c", []byte("k1")) == collector.EnrollNonce("c", []byte("k2")) || collector.EnrollNonce("c", []byte("k")) == collector.EnrollNonce("d", []byte("k")) {
		t.Fatal("nonce does not bind collector and key")
	}
}

func TestRegistrationPermitsOnlyProvisionedAuthority(t *testing.T) {
	r, e := collector.Registration{CollectorID: "c", Subject: "s", Domain: []byte("d"), AuthorityPolicyIDs: []shoal.ID{"b", "a"}, Control: collector.ExternalControlled, Mode: collector.Imported, Generation: 1, State: collector.Provisioned}.Canonical()
	if e != nil {
		t.Fatal(e)
	}
	if !r.Permits([]shoal.ID{"a"}) || !r.Permits([]shoal.ID{"a", "b"}) || r.Permits([]shoal.ID{"a", "c"}) {
		t.Fatal("subset check wrong")
	}
	for name, bad := range map[string]collector.Registration{
		"control":    {CollectorID: "c", Subject: "s", Domain: []byte("d"), AuthorityPolicyIDs: []shoal.ID{"a"}, Control: "trusted", Mode: collector.Imported, Generation: 1, State: collector.Provisioned},
		"mode":       {CollectorID: "c", Subject: "s", Domain: []byte("d"), AuthorityPolicyIDs: []shoal.ID{"a"}, Control: collector.UnknownControl, Mode: "observed", Generation: 1, State: collector.Provisioned},
		"generation": {CollectorID: "c", Subject: "s", Domain: []byte("d"), AuthorityPolicyIDs: []shoal.ID{"a"}, Control: collector.UnknownControl, Mode: collector.Imported, State: collector.Provisioned},
		"authority":  {CollectorID: "c", Subject: "s", Domain: []byte("d"), Control: collector.UnknownControl, Mode: collector.Imported, Generation: 1, State: collector.Provisioned},
	} {
		if _, e := bad.Canonical(); e == nil {
			t.Fatal(name, "accepted")
		}
	}
}

func TestArtifactValidation(t *testing.T) {
	good := collector.ArtifactRef{ID: "a", Digest: strings.Repeat("a", 64), Size: 1, MediaType: "text/plain", ObservedAt: observed}
	if good.Validate() != nil {
		t.Fatal("valid artifact refused")
	}
	for name, change := range map[string]func(*collector.ArtifactRef){
		"digest case": func(a *collector.ArtifactRef) { a.Digest = strings.Repeat("A", 64) },
		"short":       func(a *collector.ArtifactRef) { a.Digest = "abc" },
		"negative":    func(a *collector.ArtifactRef) { a.Size = -1 },
		"media":       func(a *collector.ArtifactRef) { a.MediaType = "text" },
		"time":        func(a *collector.ArtifactRef) { a.ObservedAt = time.Time{} },
	} {
		a := good
		change(&a)
		if a.Validate() == nil {
			t.Fatal(name, "accepted")
		}
	}
}

// Finding 3: -0 and +0 confidence are one observation.
func TestNegativeZeroConfidenceIsNormalized(t *testing.T) {
	zero, negative := 0.0, math.Copysign(0, -1)
	a, b := config(), config()
	a.Confidence.Value, b.Confidence.Value = &zero, &negative
	x, e1 := collector.NewObservation(a)
	y, e2 := collector.NewObservation(b)
	if e1 != nil || e2 != nil || x.ID() != y.ID() || math.Signbit(*y.Config().Confidence.Value) {
		t.Fatal("-0 confidence minted a distinct observation")
	}
}
