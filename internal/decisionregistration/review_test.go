// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionregistration

import (
	"context"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/collectorregistry"
	"github.com/phrocker/shoal-oss/internal/decisionartifacts"
	registrations "github.com/phrocker/shoal-oss/internal/decisionregistrationstore"
	"github.com/phrocker/shoal-oss/pkg/collector"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type reviewMutatingAccess struct {
	*rootAccess
	mode string
}

func (a reviewMutatingAccess) Verify(ctx context.Context, d auth.Decision, op auth.Operation, p Profile, r registrations.Registration, record decisionartifacts.Record, m []collectorregistry.Material) error {
	switch a.mode {
	case "profile":
		p.TaskResource.PolicyID[0] = 'X'
	case "domain":
		m[0].Registration.Domain[0] = 'X'
	case "enrollment":
		m[0].Enrollment.Request.RequestedAuthorityPolicyIDs[0] = "rewritten-policy"
	case "observation":
		cfg := m[0].Observation.Observation.Config()
		cfg.Payload = []byte("changed observation")
		o, e := collector.NewObservation(cfg)
		if e != nil {
			return e
		}
		m[0].Observation.Observation = o
	}
	// Return success deliberately: the service must reject verification of changed material.
	return nil
}
func TestReviewRejectsAuthorityMaterialAndProfileRewrite(t *testing.T) {
	for _, mode := range []string{"profile", "domain", "enrollment", "observation"} {
		t.Run(mode, func(t *testing.T) {
			p, provider := rootProfile(t, "source")
			a := auth.NewAuthority()
			access := &rootAccess{}
			runtime := rootOpen(t, t.TempDir(), a, p, provider, access)
			defer runtime.eng.Close()
			collectCtx, _ := rootBind(t, a, "collector-principal")
			cid := shoal.ID("collector:review")
			if _, e := runtime.registry.Provision(context.Background(), collectorregistry.Provisioning{CollectorID: cid, Subject: "collector-principal", ClientID: "client", Domain: []byte("domain"), AuthorityPolicyIDs: []shoal.ID{"authority:source"}, Control: collector.CandidateControlled, Mode: collector.Imported}); e != nil {
				t.Fatal(e)
			}
			ex := collector.ExtractorRef{ID: "extractor:review", Version: "1"}
			if _, e := runtime.registry.Enroll(collectCtx, []byte("enroll"), collector.EnrollRequest{CollectorID: cid, RequestedAuthorityPolicyIDs: []shoal.ID{"authority:source"}, Extractors: []collector.ExtractorRef{ex}}); e != nil {
				t.Fatal(e)
			}
			body := []byte("source remains source")
			artifact := collector.ArtifactRef{ID: "artifact:review", Digest: rootHash(body), Size: int64(len(body)), MediaType: "text/plain", ObservedAt: time.Now().UTC().Add(-time.Second)}
			if _, e := runtime.registry.SubmitArtifact(collectCtx, cid, artifact); e != nil {
				t.Fatal(e)
			}
			o, e := collector.NewObservation(collector.ObservationConfig{CollectorID: cid, ArtifactID: artifact.ID, Extractor: ex, SubjectID: "source", Kind: "source_document", Confidence: collector.Confidence{Disposition: collector.Extracted}, Payload: []byte("original"), ObservedAt: time.Now().UTC()})
			if e != nil {
				t.Fatal(e)
			}
			if _, e = runtime.registry.SubmitObservation(collectCtx, o); e != nil {
				t.Fatal(e)
			}
			ctx, d := rootBind(t, a, "decision-principal", o.ID())
			selection := Selection{ProfileID: p.ID, ProfileRevisionID: p.RevisionID, Sources: []SourceInput{{ObservationID: o.ID(), Bytes: body}}}
			runtime.service.config.Authority = reviewMutatingAccess{access, mode}
			key := []byte("mutation")
			if got, e := runtime.service.Register(ctx, key, selection); e == nil || got.ID != "" {
				t.Fatal("accepted changed verification inputs", e)
			}
			if _, e := runtime.service.config.Registrations.ReadByKey(ctx, scopeFor(d), key); e == nil {
				t.Fatal("invalid candidate became a durable preparation")
			}
			runtime.service.config.Authority = access
			if got, e := runtime.service.Register(ctx, key, selection); e != nil || got.State != registrations.Ready {
				t.Fatal("callback poisoned original inputs", e)
			}
		})
	}
}
