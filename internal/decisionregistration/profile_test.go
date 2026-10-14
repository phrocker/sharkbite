// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionregistration

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/collectorregistry"
	"github.com/phrocker/shoal-oss/internal/decisionartifacts"
	"github.com/phrocker/shoal-oss/pkg/collector"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type profileBuilderFunc func(context.Context, BuildInput) (BuiltPicture, error)

func (f profileBuilderFunc) Build(c context.Context, in BuildInput) (BuiltPicture, error) {
	return f(c, in)
}
func testProfileBuilder(_ context.Context, in BuildInput) (BuiltPicture, error) {
	b := BuiltPicture{Input: []byte(`{"feature":1}`), InputTokens: 1, TokenBudget: 1}
	for _, s := range in.Sources {
		a, e := inference.NewDocumentAnchor(document.Citation{DocumentID: s.Source.ArtifactID, RevisionID: s.Source.RevisionID, SectionID: "all", SpanID: "all", Range: document.SourceRange{End: document.SourcePosition{Offset: int64(len(s.Bytes))}}}, string(s.Bytes))
		if e != nil {
			return BuiltPicture{}, e
		}
		b.Anchors = append(b.Anchors, a)
		b.Subjects = append(b.Subjects, decision.Subject{ID: s.Source.ID, SourceID: s.Source.ID, Disposition: decision.Supported, EvidenceIDs: []shoal.ID{a.ID()}})
	}
	return b, nil
}
func profileFixture(t *testing.T) (Profile, Selection, []collectorregistry.Material, auth.Decision, time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	ep, e := decision.NewEvidencePolicy(decision.EvidencePolicyConfig{MaxObservationAge: time.Hour, AllowedRoles: []decision.EvidenceRole{decision.Observation}, AllowedControls: []decision.Control{decision.CandidateControlled}, AuthorityPolicyIDs: []shoal.ID{"source-policy"}})
	if e != nil {
		t.Fatal(e)
	}
	rank, e := decision.NewRankingPlan(decision.RankingConfig{Terms: []decision.RankingTerm{{QuestionID: "question", Weight: 1, Labels: []decision.LabelPriority{{Label: "yes", Value: 1}, {Label: "no", Value: 0}}}}})
	if e != nil {
		t.Fatal(e)
	}
	task, e := decision.NewTaskSpec(decision.TaskConfig{OwnerID: "operator", Name: "original source property", Version: "1", InputSchemaID: "input-v1", EvidencePolicyID: ep.ID(), LabelPolicyID: "labels-v1", EvaluationPolicyID: "eval-v1", PredictionUnit: "source", LabelUnit: "property", ActionUnit: "review", AggregationID: rank.ID(), Questions: []decision.Question{{ID: "question", Kind: decision.Choice, RubricID: "rubric", Labels: []string{"yes", "no"}}}})
	if e != nil {
		t.Fatal(e)
	}
	predictor, e := decision.NewPredictorIdentity(decision.PredictorConfig{Provider: "local", RuntimeID: "runtime", WeightsDigest: strings.Repeat("a", 64), TokenizerDigest: strings.Repeat("b", 64), FormattingID: "format", PreprocessingID: "builder", CalibrationID: "uncalibrated", EnvironmentDigest: strings.Repeat("c", 64), Device: "cpu", Precision: "float64", BatchPolicyID: "batch"})
	if e != nil {
		t.Fatal(e)
	}
	p := Profile{AllowedModes: []collector.Mode{collector.ServerObserved}, ID: "profile", RevisionID: "revision-v1", BuilderID: "builder", MaterialPurposeID: "purpose", ReleaseID: "release", Query: "Classify original source", TokenizerID: "numeric-v1", OntologyProjectionID: "no-ontology-v1", SourceAuthorityPolicyID: "source-policy", Task: task, Predictor: predictor, EvidencePolicy: ep, RankingPlan: rank, TaskResource: auth.ResourceRequest{AuthorizationDomain: []byte("domain"), SourceID: []byte("tasks"), PolicyID: []byte("task-policy"), ObjectID: task.ID()}, Builder: profileBuilderFunc(testProfileBuilder), MaxDuration: time.Minute}
	d, e := auth.NewDecision(auth.DecisionConfig{Subject: "reader", Actor: "actor", ClientID: "client", AuthorizationDomain: []byte("domain"), AllowedOperations: []auth.Operation{auth.OperationRead, auth.OperationInvoke}, PolicyGeneration: 1, AuthenticationExpires: now.Add(time.Hour), RequestID: "auth-request"})
	if e != nil {
		t.Fatal(e)
	}
	raw := []byte("package source\n")
	extractor := collector.ExtractorRef{ID: "extractor", Version: "1"}
	o, e := collector.NewObservation(collector.ObservationConfig{CollectorID: "collector", ArtifactID: "artifact", Extractor: extractor, Confidence: collector.Confidence{Disposition: collector.Extracted}, SubjectID: "untrusted-subject", Kind: "source", Payload: []byte(`{}`), ObservedAt: now.Add(-time.Second)})
	if e != nil {
		t.Fatal(e)
	}
	material := collectorregistry.Material{Registration: collector.Registration{CollectorID: "collector", Subject: "collector-owner", ClientID: "collector-client", Domain: []byte("domain"), AuthorityPolicyIDs: []shoal.ID{"source-policy"}, Control: collector.CandidateControlled, Mode: collector.ServerObserved, Generation: 1, State: collector.Enrolled}, Enrollment: collectorregistry.Enrollment{ID: "enrollment", Generation: 1, Request: collector.EnrollRequest{CollectorID: "collector", RequestedAuthorityPolicyIDs: []shoal.ID{"source-policy"}, Extractors: []collector.ExtractorRef{extractor}}, EnrolledAt: now.Add(-time.Minute)}, Artifact: collectorregistry.ArtifactRecord{CollectorID: "collector", Domain: []byte("domain"), Ref: collector.ArtifactRef{ID: "artifact", Digest: profileHash(raw), Size: int64(len(raw)), MediaType: "text/plain", ObservedAt: now.Add(-time.Second)}, Generation: 1, EnrollmentID: "enrollment", ReceivedAt: now.Add(-time.Second)}, Observation: collectorregistry.ObservationRecord{Observation: o, ArtifactDigest: profileHash(raw), Generation: 1, EnrollmentID: "enrollment", ReceivedAt: now.Add(-time.Second)}}
	s := Selection{ProfileID: p.ID, ProfileRevisionID: p.RevisionID, Sources: []SourceInput{{ObservationID: o.ID(), Bytes: raw}}}
	return p, s, []collectorregistry.Material{material}, d, now
}
func TestProfileFrozenRoundTripWithHistoricalAuthPins(t *testing.T) {
	p, s, m, d, now := profileFixture(t)
	r, f, e := buildRecord(context.Background(), d, p, s, m, now, "correlation")
	if e != nil {
		t.Fatal(e)
	}
	if f.RecordSHA256 == "" || f.SelectionSHA256 != selectionDigest(s) || f.Sources[0].SourceSHA256 == f.Sources[0].ArtifactSHA256 {
		t.Fatal("unbound provenance")
	}
	if e = validateRecord(context.Background(), p, f, r, m); e != nil {
		t.Fatal(e)
	}
	if r.Bundle.Request.Config().Deadline != now.Add(time.Minute) || r.Bundle.Request.Picture().Authorization().ExpiresAt() != d.AuthenticationExpires() {
		t.Fatal("unfrozen times")
	}
	if _, ok := r.Bundle.Request.Picture().ContextPack().Ontology(); ok {
		t.Fatal("invented ontology")
	}
	encoded, e := decisionartifacts.EncodeRecord(r)
	if e != nil {
		t.Fatal(e)
	}
	r2, e := decisionartifacts.DecodeRecord(encoded)
	if e != nil {
		t.Fatal(e)
	}
	if e = validateRecord(context.Background(), p, f, r2, m); e != nil {
		t.Fatal(e)
	}
}
func TestProfileRejectsSourceAndProfileSubstitution(t *testing.T) {
	for _, mode := range []string{"bytes", "size", "digest", "domain", "enrollment", "control", "query", "predictor", "builder", "sourcepin", "authpin", "purpose", "duration"} {
		t.Run(mode, func(t *testing.T) {
			p, s, m, d, now := profileFixture(t)
			r, f, e := buildRecord(context.Background(), d, p, s, m, now, "correlation")
			if e != nil {
				t.Fatal(e)
			}
			switch mode {
			case "bytes":
				r.Sources[0].Bytes[0] = 'X'
			case "size":
				m[0].Artifact.Ref.Size++
			case "digest":
				m[0].Artifact.Ref.Digest = strings.Repeat("d", 64)
			case "domain":
				m[0].Artifact.Domain = []byte("other")
			case "enrollment":
				m[0].Artifact.EnrollmentID = "other"
			case "control":
				m[0].Registration.Control = collector.RegistryControlled
			case "query":
				p.Query = "Different prompt"
			case "predictor":
				c := p.Predictor.Config()
				c.RuntimeID = "other"
				p.Predictor, _ = decision.NewPredictorIdentity(c)
			case "builder":
				p.Builder = profileBuilderFunc(func(c context.Context, in BuildInput) (BuiltPicture, error) {
					b, e := testProfileBuilder(c, in)
					b.Input = []byte(`{"feature":2}`)
					return b, e
				})
			case "sourcepin":
				f.Sources[0].SourceSHA256 = strings.Repeat("f", 64)
			case "purpose":
				p.MaterialPurposeID = "other-purpose"
			case "duration":
				p.MaxDuration = 2 * time.Minute
			case "authpin":
				f.AuthorizationFingerprint = "auth-sha256:" + strings.Repeat("f", 64)
			}
			if e = validateRecord(context.Background(), p, f, r, m); e == nil {
				t.Fatal("substitution accepted")
			}
		})
	}
}
func TestProfileDetachedCallbacksAndNoOmission(t *testing.T) {
	for _, mode := range []string{"mutate", "omit", "bad-quote", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			p, s, m, d, now := profileFixture(t)
			original := append([]byte(nil), s.Sources[0].Bytes...)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p.Builder = profileBuilderFunc(func(c context.Context, in BuildInput) (BuiltPicture, error) {
				switch mode {
				case "mutate":
					in.Sources[0].Bytes[0] = 'X'
				case "omit":
					return BuiltPicture{Input: []byte("x")}, nil
				case "cancel":
					cancel()
				}
				b, e := testProfileBuilder(c, in)
				if mode == "bad-quote" {
					in.Sources[0].Bytes[0] = 'X'
					b, e = testProfileBuilder(c, in)
				}
				return b, e
			})
			if _, _, e := buildRecord(ctx, d, p, s, m, now, "correlation"); e == nil {
				t.Fatal("invalid builder accepted")
			}
			if !reflect.DeepEqual(original, s.Sources[0].Bytes) {
				t.Fatal("builder mutated caller bytes")
			}
		})
	}
}
func TestProfileNormalizationAndSelectionBounds(t *testing.T) {
	p, s, _, _, _ := profileFixture(t)
	normalized, e := normalizeProfile(p)
	if e != nil {
		t.Fatal(e)
	}
	normalized.TaskResource.AuthorizationDomain[0] = 'X'
	if string(p.TaskResource.AuthorizationDomain) != "domain" {
		t.Fatal("profile aliased")
	}
	if _, e = validateProfiles([]Profile{p, p}); e == nil {
		t.Fatal("duplicate profile")
	}
	s2, e := normalizeSelection(s)
	if e != nil {
		t.Fatal(e)
	}
	s2.Sources[0].Bytes[0] = 'X'
	if s.Sources[0].Bytes[0] == 'X' {
		t.Fatal("selection aliased")
	}
	s.Sources = append(s.Sources, s.Sources[0])
	if _, e = normalizeSelection(s); e == nil {
		t.Fatal("duplicate selection")
	}
	s.Sources = s.Sources[:1]
	s.Sources[0].Bytes = make([]byte, MaxSelectionBytes+1)
	if _, e = normalizeSelection(s); e == nil {
		t.Fatal("unbounded bytes")
	}
	p.Builder = nil
	if _, e = normalizeProfile(p); e == nil {
		t.Fatal("nil builder")
	}
}

func TestProfileRequiresExplicitCanonicalCollectorModes(t *testing.T) {
	p, _, _, _, _ := profileFixture(t)
	for _, modes := range [][]collector.Mode{nil, {}, {collector.ServerObserved, collector.ServerObserved}, {"unknown"}} {
		candidate := p
		candidate.AllowedModes = modes
		if _, e := normalizeProfile(candidate); e == nil {
			t.Fatalf("accepted modes %v", modes)
		}
	}
	p.AllowedModes = []collector.Mode{collector.ServerObserved, collector.Imported}
	normalized, e := normalizeProfile(p)
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(normalized.AllowedModes, []collector.Mode{collector.Imported, collector.ServerObserved}) {
		t.Fatal("noncanonical modes")
	}
	normalized.AllowedModes[0] = collector.ServerObserved
	if p.AllowedModes[1] != collector.Imported {
		t.Fatal("aliased modes")
	}
}
func TestProfileCollectorModeCannotBeLaundered(t *testing.T) {
	p, s, m, d, now := profileFixture(t)
	m[0].Registration.Mode = collector.Imported
	if _, _, e := buildRecord(context.Background(), d, p, s, m, now, "correlation"); e == nil {
		t.Fatal("imported source admitted by observed-only profile")
	}
	p.AllowedModes = []collector.Mode{collector.Imported, collector.ServerObserved}
	r, f, e := buildRecord(context.Background(), d, p, s, m, now, "correlation")
	if e != nil {
		t.Fatal(e)
	}
	if f.Sources[0].Mode != collector.Imported {
		t.Fatal("mode lost")
	}
	if e = validateRecord(context.Background(), p, f, r, m); e != nil {
		t.Fatal(e)
	}
	f.Sources[0].Mode = collector.ServerObserved
	if e = validateRecord(context.Background(), p, f, r, m); e == nil {
		t.Fatal("substituted frozen mode")
	}
	f.Sources[0].Mode = collector.Imported
	m[0].Registration.Mode = collector.ServerObserved
	if e = validateRecord(context.Background(), p, f, r, m); e == nil {
		t.Fatal("changed collector mode")
	}
	m[0].Registration.Mode = collector.Imported
	p.AllowedModes = []collector.Mode{collector.Imported}
	if e = validateRecord(context.Background(), p, f, r, m); e == nil {
		t.Fatal("changed allowed modes under same revision")
	}
}
