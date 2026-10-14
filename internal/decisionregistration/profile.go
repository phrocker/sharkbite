// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionregistration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/internal/collectorregistry"
	"github.com/phrocker/shoal-oss/internal/decisionartifacts"
	registrationstore "github.com/phrocker/shoal-oss/internal/decisionregistrationstore"
	"github.com/phrocker/shoal-oss/internal/decisionservice"
	"github.com/phrocker/shoal-oss/pkg/collector"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const MaxSelectionSources = 64
const MaxSelectionBytes = 8 << 20

// Profile is provisioned by the host, never by a registration caller. Its builder
// and all versioned definitions must remain immutable under this revision.
// Provisioning this profile makes no assertion about classifier quality.
type Profile struct {
	AllowedModes                                               []collector.Mode
	ID, RevisionID, BuilderID, MaterialPurposeID, ReleaseID    shoal.ID
	Query                                                      string
	TokenizerID, OntologyProjectionID, SourceAuthorityPolicyID shoal.ID
	Task                                                       decision.TaskSpec
	Predictor                                                  decision.PredictorIdentity
	EvidencePolicy                                             decision.EvidencePolicy
	RankingPlan                                                decision.RankingPlan
	TaskResource                                               auth.ResourceRequest
	Builder                                                    Builder
	MaxDuration                                                time.Duration
}
type SourceInput struct {
	ObservationID shoal.ID
	Bytes         []byte
}
type Selection struct {
	ProfileID, ProfileRevisionID shoal.ID
	Sources                      []SourceInput
}

// ResolvedSource contains original bytes and verified historical metadata only.
// Current grants, current policy state, reports and labels are not model inputs.
type ResolvedSource struct {
	Source decision.Source
	Bytes  []byte
}
type BuildInput struct {
	Sources []ResolvedSource
	Cutoff  time.Time
}
type BuiltPicture struct {
	Input                    []byte
	Anchors                  []inference.EvidenceAnchor
	Subjects                 []decision.Subject
	Measurements             []decision.Measurement
	InputTokens, TokenBudget uint64
	Truncated                bool
}

// Builder is trusted host code, selected by immutable profile revision. It must
// be deterministic, bounded, context-aware and side-effect free. The service
// verifies it again against retained original bytes; it cannot mint authority.
type Builder interface {
	Build(context.Context, BuildInput) (BuiltPicture, error)
}

func profileInvalid() error {
	return shoal.NewError(shoal.ErrorInvalidArgument, "invalid registered profile or source preparation")
}
func profileID(id shoal.ID) bool {
	return shoal.ValidateRequiredID("profile identity", id) == nil && utf8.ValidString(string(id)) && strings.TrimSpace(string(id)) != ""
}
func profileHash(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func profileJSON(v any) []byte    { b, _ := json.Marshal(v); return b }
func profileIdentity(kind string, v any) shoal.ID {
	return shoal.ID("decision-registration-" + kind + ":" + profileHash(profileJSON(struct {
		Kind  string
		Value any
	}{kind, v})))
}

// profileCommitment binds the effective operator configuration across restart.
// Builder code itself remains a trusted implementation pinned by BuilderID.
func profileCommitment(p Profile) string {
	return profileHash(profileJSON(struct {
		Kind                                                                                                string
		ID, RevisionID, BuilderID, PurposeID, ReleaseID                                                     shoal.ID
		Query                                                                                               string
		TokenizerID, OntologyProjectionID, SourcePolicyID, TaskID, PredictorID, EvidencePolicyID, RankingID shoal.ID
		AllowedModes                                                                                        []collector.Mode
		Resource                                                                                            auth.ResourceRequest
		MaxDuration                                                                                         time.Duration
	}{"operator-profile-v1", p.ID, p.RevisionID, p.BuilderID, p.MaterialPurposeID, p.ReleaseID, p.Query, p.TokenizerID, p.OntologyProjectionID, p.SourceAuthorityPolicyID, p.Task.ID(), p.Predictor.ID(), p.EvidencePolicy.ID(), p.RankingPlan.ID(), p.AllowedModes, p.TaskResource, p.MaxDuration}))
}
func profileKey(id, revision shoal.ID) string { return string(profileJSON([]shoal.ID{id, revision})) }
func normalizeProfile(p Profile) (Profile, error) {
	if len(p.AllowedModes) == 0 || len(p.AllowedModes) > 2 {
		return Profile{}, profileInvalid()
	}
	p.AllowedModes = append([]collector.Mode(nil), p.AllowedModes...)
	sort.Slice(p.AllowedModes, func(i, j int) bool { return p.AllowedModes[i] < p.AllowedModes[j] })
	for i, mode := range p.AllowedModes {
		if (mode != collector.Imported && mode != collector.ServerObserved) || (i > 0 && mode == p.AllowedModes[i-1]) {
			return Profile{}, profileInvalid()
		}
	}

	for _, id := range []shoal.ID{p.ID, p.RevisionID, p.BuilderID, p.MaterialPurposeID, p.ReleaseID, p.TokenizerID, p.OntologyProjectionID, p.SourceAuthorityPolicyID} {
		if !profileID(id) {
			return Profile{}, profileInvalid()
		}
	}
	if strings.TrimSpace(p.Query) == "" || !utf8.ValidString(p.Query) || len(p.Query) > inference.MaxQueryBytes || p.MaxDuration <= 0 || p.MaxDuration > 24*time.Hour || p.Builder == nil {
		return Profile{}, profileInvalid()
	}
	v := reflect.ValueOf(p.Builder)
	if (v.Kind() == reflect.Pointer || v.Kind() == reflect.Func || v.Kind() == reflect.Interface || v.Kind() == reflect.Map || v.Kind() == reflect.Slice) && v.IsNil() {
		return Profile{}, profileInvalid()
	}
	if p.Task.Validate() != nil || p.Predictor.Validate() != nil || p.EvidencePolicy.Validate() != nil || p.RankingPlan.ValidateTask(p.Task) != nil || p.Task.Config().EvidencePolicyID != p.EvidencePolicy.ID() {
		return Profile{}, profileInvalid()
	}
	resource, e := p.TaskResource.Normalize()
	if e != nil || len(resource.AuthorizationDomain) == 0 || len(resource.SourceID) == 0 || len(resource.PolicyID) == 0 || resource.ObjectID != p.Task.ID() {
		return Profile{}, profileInvalid()
	}
	permitted := false
	for _, id := range p.EvidencePolicy.Config().AuthorityPolicyIDs {
		permitted = permitted || id == p.SourceAuthorityPolicyID
	}
	if !permitted {
		return Profile{}, profileInvalid()
	}
	p.TaskResource = resource
	p.Task, _ = decision.NewTaskSpec(p.Task.Config())
	p.Predictor, _ = decision.NewPredictorIdentity(p.Predictor.Config())
	p.EvidencePolicy, _ = decision.NewEvidencePolicy(p.EvidencePolicy.Config())
	p.RankingPlan, _ = decision.NewRankingPlan(p.RankingPlan.Config())
	return p, nil
}
func validateProfiles(in []Profile) (map[string]Profile, error) {
	if len(in) == 0 || len(in) > 256 {
		return nil, profileInvalid()
	}
	out := make(map[string]Profile, len(in))
	for _, p := range in {
		p, e := normalizeProfile(p)
		if e != nil {
			return nil, e
		}
		key := profileKey(p.ID, p.RevisionID)
		if _, ok := out[key]; ok {
			return nil, profileInvalid()
		}
		out[key] = p
	}
	return out, nil
}
func normalizeSelection(s Selection) (Selection, error) {
	if !profileID(s.ProfileID) || !profileID(s.ProfileRevisionID) || len(s.Sources) == 0 || len(s.Sources) > MaxSelectionSources {
		return Selection{}, profileInvalid()
	}
	out := Selection{ProfileID: s.ProfileID, ProfileRevisionID: s.ProfileRevisionID, Sources: make([]SourceInput, len(s.Sources))}
	size := 0
	for i, v := range s.Sources {
		if !collector.ValidObservationID(v.ObservationID) || len(v.Bytes) > MaxSelectionBytes-size {
			return Selection{}, profileInvalid()
		}
		size += len(v.Bytes)
		out.Sources[i] = SourceInput{v.ObservationID, bytes.Clone(v.Bytes)}
	}
	sort.Slice(out.Sources, func(i, j int) bool { return out.Sources[i].ObservationID < out.Sources[j].ObservationID })
	for i := 1; i < len(out.Sources); i++ {
		if out.Sources[i-1].ObservationID == out.Sources[i].ObservationID {
			return Selection{}, profileInvalid()
		}
	}
	return out, nil
}
func selectionDigest(s Selection) string {
	type source struct {
		ObservationID shoal.ID
		SHA256        string
		Bytes         int
	}
	v := struct {
		Kind                  string
		ProfileID, RevisionID shoal.ID
		Sources               []source
	}{Kind: "collector-selection-v1", ProfileID: s.ProfileID, RevisionID: s.ProfileRevisionID}
	for _, x := range s.Sources {
		v.Sources = append(v.Sources, source{x.ObservationID, profileHash(x.Bytes), len(x.Bytes)})
	}
	sort.Slice(v.Sources, func(i, j int) bool { return v.Sources[i].ObservationID < v.Sources[j].ObservationID })
	return profileHash(profileJSON(v))
}
func resolveSources(p Profile, s Selection, materials []collectorregistry.Material) ([]ResolvedSource, []registrationstore.SourcePin, error) {
	if s.ProfileID != p.ID || s.ProfileRevisionID != p.RevisionID || len(materials) != len(s.Sources) {
		return nil, nil, profileInvalid()
	}
	byID := make(map[shoal.ID]collectorregistry.Material, len(materials))
	for _, m := range materials {
		id := m.Observation.Observation.ID()
		if _, ok := byID[id]; ok {
			return nil, nil, profileInvalid()
		}
		byID[id] = m
	}
	sources := make([]ResolvedSource, 0, len(s.Sources))
	pins := make([]registrationstore.SourcePin, 0, len(s.Sources))
	for _, input := range s.Sources {
		m, ok := byID[input.ObservationID]
		if !ok {
			return nil, nil, profileInvalid()
		}
		modeAllowed := false
		for _, mode := range p.AllowedModes {
			modeAllowed = modeAllowed || mode == m.Registration.Mode
		}
		if !modeAllowed {
			return nil, nil, profileInvalid()
		}
		c := m.Observation.Observation.Config()
		a := m.Artifact
		if !bytes.Equal(m.Registration.Domain, p.TaskResource.AuthorizationDomain) || !bytes.Equal(a.Domain, p.TaskResource.AuthorizationDomain) || a.CollectorID != c.CollectorID || a.Generation != m.Registration.Generation || a.EnrollmentID != m.Enrollment.ID || a.Ref.ID != c.ArtifactID || a.Ref.Validate() != nil || a.Ref.Digest != m.Observation.ArtifactDigest || a.Ref.Size != int64(len(input.Bytes)) || profileHash(input.Bytes) != a.Ref.Digest {
			return nil, nil, profileInvalid()
		}
		source, e := collectorregistry.Source(m.Registration, m.Enrollment, m.Observation, p.SourceAuthorityPolicyID)
		if e != nil {
			return nil, nil, profileInvalid()
		}
		sources = append(sources, ResolvedSource{source, bytes.Clone(input.Bytes)})
		pins = append(pins, registrationstore.SourcePin{CollectorID: c.CollectorID, ObservationID: input.ObservationID, ArtifactID: source.ArtifactID, EnrollmentID: m.Enrollment.ID, AuthorityPolicyID: source.AuthorityPolicyID, Mode: m.Registration.Mode, Generation: m.Observation.Generation, ArtifactSHA256: a.Ref.Digest, SourceSHA256: profileHash(profileJSON(source)), ReceivedAt: source.ReceivedAt})
	}
	return sources, pins, nil
}
func cloneBuildInput(in BuildInput) BuildInput {
	out := BuildInput{Cutoff: in.Cutoff, Sources: make([]ResolvedSource, len(in.Sources))}
	for i, s := range in.Sources {
		out.Sources[i] = ResolvedSource{s.Source, bytes.Clone(s.Bytes)}
	}
	return out
}
func buildRecord(ctx context.Context, d auth.Decision, p Profile, s Selection, materials []collectorregistry.Material, accepted time.Time, correlation shoal.ID) (decisionartifacts.Record, registrationstore.Frozen, error) {
	var zero decisionartifacts.Record
	var frozen registrationstore.Frozen
	var e error
	p, e = normalizeProfile(p)
	if e != nil {
		return zero, frozen, e
	}
	s, e = normalizeSelection(s)
	if e != nil {
		return zero, frozen, e
	}
	accepted = accepted.Round(0).UTC()
	if accepted.IsZero() || accepted.Year() < 1 || accepted.Year() > 9999 || !accepted.Before(d.AuthenticationExpires()) || !bytes.Equal(d.AuthorizationDomain(), p.TaskResource.AuthorizationDomain) {
		return zero, frozen, profileInvalid()
	}
	fp, e := auth.AuthorizationFingerprint(d)
	if e != nil {
		return zero, frozen, e
	}
	sources, pins, e := resolveSources(p, s, materials)
	if e != nil {
		return zero, frozen, e
	}
	record, e := assembleRecord(ctx, p, s, sources, pins, accepted, d.AuthenticationExpires().Round(0).UTC(), shoal.ID(fp.String()), d.Subject(), correlation)
	if e != nil {
		return zero, frozen, e
	}
	raw, e := decisionartifacts.EncodeRecord(record)
	if e != nil {
		return zero, frozen, e
	}
	frozen = registrationstore.Frozen{SelectionSHA256: selectionDigest(s), ProfileID: p.ID, ProfileRevisionID: p.RevisionID, BuilderID: p.BuilderID, RequestID: record.Bundle.Request.ID(), TaskID: p.Task.ID(), PictureID: record.Bundle.Request.Picture().ID(), PredictorID: p.Predictor.ID(), Sources: pins, AcceptedAt: accepted, AuthenticationExpiresAt: d.AuthenticationExpires().Round(0).UTC(), AuthorizationFingerprint: fp.String(), RecordSHA256: profileHash(raw), RecordBytes: len(raw)}
	return record, frozen, nil
}
func assembleRecord(ctx context.Context, p Profile, s Selection, sources []ResolvedSource, pins []registrationstore.SourcePin, accepted, expires time.Time, fingerprint, principal, correlation shoal.ID) (decisionartifacts.Record, error) {
	var zero decisionartifacts.Record
	if ctx.Err() != nil {
		return zero, ctx.Err()
	}
	for _, source := range sources {
		if source.Source.ReceivedAt.After(accepted) || source.Source.ObservedAt.After(accepted) {
			return zero, profileInvalid()
		}
	}
	builderInput := BuildInput{Sources: sources, Cutoff: accepted}
	callbackInput := cloneBuildInput(builderInput)
	built, e := p.Builder.Build(ctx, callbackInput)
	if e != nil {
		return zero, e
	}
	if ctx.Err() != nil {
		return zero, ctx.Err()
	}
	if !reflect.DeepEqual(callbackInput, builderInput) {
		return zero, profileInvalid()
	}
	if len(built.Input) == 0 || len(built.Input) > inference.MaxContextPackBytes || len(built.Subjects) != len(sources) || len(built.Anchors) > inference.MaxEvidenceAnchors || len(built.Measurements) > decision.MaxMeasurements {
		return zero, profileInvalid()
	}
	// The first builder boundary represents each selected observation exactly
	// once, including explicit unsupported entries. It cannot omit difficult input.
	sourceSet := map[shoal.ID]bool{}
	for _, source := range sources {
		sourceSet[source.Source.ID] = false
	}
	for _, sub := range built.Subjects {
		seen, ok := sourceSet[sub.SourceID]
		if !ok || seen {
			return zero, profileInvalid()
		}
		sourceSet[sub.SourceID] = true
	}
	enumeration := profileIdentity("selected-inventory-v1", struct {
		ProfileID, RevisionID shoal.ID
		Selection             string
		ProfileSHA256         string
		Sources               []registrationstore.SourcePin
	}{p.ID, p.RevisionID, selectionDigest(s), profileCommitment(p), pins})
	snapshot, e := inference.NewSnapshotPin(profileIdentity("snapshot-v1", struct {
		Enumeration shoal.ID
		Cutoff      time.Time
	}{enumeration, accepted}), accepted)
	if e != nil {
		return zero, e
	}
	pin, e := inference.NewAuthPin(fingerprint, expires)
	if e != nil {
		return zero, e
	}
	pack, e := inference.NewContextPack(p.Query, built.Anchors, nil, snapshot, pin, nil)
	if e != nil {
		return zero, e
	}
	pc := decision.PictureConfig{TaskID: p.Task.ID(), ObservationID: enumeration, EnumerationID: enumeration, ScopeID: profileIdentity("scope-v1", struct {
		Domain                []byte
		ProfileID, RevisionID shoal.ID
	}{p.TaskResource.AuthorizationDomain, p.ID, p.RevisionID}), BuilderID: p.BuilderID, OntologyProjectionID: p.OntologyProjectionID, InputDigest: profileHash(built.Input), TokenizerID: p.TokenizerID, InputTokens: built.InputTokens, TokenBudget: built.TokenBudget, Truncated: built.Truncated, Cutoff: accepted, Subjects: built.Subjects, Measurements: built.Measurements}
	retained := make([]decisionartifacts.SourceBytes, len(sources))
	for i, source := range sources {
		pc.Sources = append(pc.Sources, source.Source)
		retained[i] = decisionartifacts.SourceBytes{ID: source.Source.ID, Bytes: bytes.Clone(source.Bytes)}
	}
	picture, e := decision.NewPictureManifest(pack, pc)
	if e != nil {
		return zero, e
	}
	deadline := accepted.Add(p.MaxDuration)
	if expires.Before(deadline) {
		deadline = expires
	}
	rc := decision.RequestConfig{PrincipalID: principal, ReleaseID: p.ReleaseID, CorrelationID: correlation, RequestedAt: accepted, Deadline: deadline}
	for _, sub := range picture.Config().Subjects {
		rc.SubjectIDs = append(rc.SubjectIDs, sub.ID)
	}
	request, e := decision.NewDecisionRequest(p.Task, picture, p.Predictor, rc)
	if e != nil {
		return zero, e
	}
	record := decisionartifacts.Record{Bundle: decisionservice.Bundle{Request: request, EvidencePolicy: p.EvidencePolicy, RankingPlan: p.RankingPlan, TaskResource: p.TaskResource, Input: bytes.Clone(built.Input)}, Sources: retained}
	raw, e := decisionartifacts.EncodeRecord(record)
	if e != nil {
		return zero, e
	}
	return decisionartifacts.DecodeRecord(raw)
}

// validateRecord reproduces the frozen original using the original pins as
// structural values only. It never resolves or impersonates historical auth.
// Current access must be verified independently after all IO by the service.
func validateRecord(ctx context.Context, p Profile, f registrationstore.Frozen, r decisionartifacts.Record, materials []collectorregistry.Material) error {
	var e error
	p, e = normalizeProfile(p)
	if e != nil {
		return e
	}
	raw, e := decisionartifacts.EncodeRecord(r)
	if e != nil {
		return e
	}
	if len(raw) != f.RecordBytes || profileHash(raw) != f.RecordSHA256 || p.ID != f.ProfileID || p.RevisionID != f.ProfileRevisionID || p.BuilderID != f.BuilderID || p.Task.ID() != f.TaskID || p.Predictor.ID() != f.PredictorID || r.Bundle.Request.ID() != f.RequestID || r.Bundle.Request.Picture().ID() != f.PictureID {
		return profileInvalid()
	}
	selection := Selection{ProfileID: p.ID, ProfileRevisionID: p.RevisionID}
	for _, source := range r.Sources {
		selection.Sources = append(selection.Sources, SourceInput{source.ID, source.Bytes})
	}
	selection, e = normalizeSelection(selection)
	if e != nil {
		return e
	}
	if selectionDigest(selection) != f.SelectionSHA256 {
		return profileInvalid()
	}
	sources, pins, e := resolveSources(p, selection, materials)
	if e != nil {
		return e
	}
	if !bytes.Equal(profileJSON(pins), profileJSON(f.Sources)) {
		return profileInvalid()
	}
	rebuilt, e := assembleRecord(ctx, p, selection, sources, pins, f.AcceptedAt, f.AuthenticationExpiresAt, shoal.ID(f.AuthorizationFingerprint), r.Bundle.Request.Config().PrincipalID, r.Bundle.Request.Config().CorrelationID)
	if e != nil {
		return e
	}
	reencoded, e := decisionartifacts.EncodeRecord(rebuilt)
	if e != nil {
		return e
	}
	if !bytes.Equal(raw, reencoded) {
		return profileInvalid()
	}
	return nil
}
