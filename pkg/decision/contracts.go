/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

// Package decision defines immutable task and measured-picture contracts.
// It does not authenticate provenance, authorize access, execute predictors, or
// establish that an observation is true. Services must verify those properties.
package decision

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	MaxQuestions     = 64
	MaxLabels        = 64
	MaxSources       = 1024
	MaxIncidents     = 256
	MaxSubjects      = 4096
	MaxMeasurements  = 64
	MaxManifestBytes = 4 * 1024 * 1024
)

// AnswerKind distinguishes a label, ordered rubric and proposition probability.
type AnswerKind string

const (
	Choice      AnswerKind = "choice"
	Ordinal     AnswerKind = "ordinal"
	Probability AnswerKind = "probability"
)

// Question's labels are ordered only for Ordinal. Probability has no labels.
// RubricID identifies an immutable externally retained definition, not free text
// that a source author can use to replace task instructions.
type Question struct {
	ID       shoal.ID
	Kind     AnswerKind
	RubricID shoal.ID
	Labels   []string
}

// TaskConfig references versioned policies owned by the trusted task registry.
// Roles and policies are references here; this package does not resolve them.
// IDs in these JSON-addressed contracts must be valid UTF-8 as well as bounded.
type TaskConfig struct {
	OwnerID            shoal.ID
	Name               string
	Version            string
	InputSchemaID      shoal.ID
	EvidencePolicyID   shoal.ID
	LabelPolicyID      shoal.ID
	EvaluationPolicyID shoal.ID
	PredictionUnit     string
	LabelUnit          string
	ActionUnit         string
	AggregationID      shoal.ID
	Questions          []Question
}

type TaskSpec struct {
	id     shoal.ID
	config TaskConfig
}

func NewTaskSpec(config TaskConfig) (TaskSpec, error) {
	if len(config.Questions) == 0 || len(config.Questions) > MaxQuestions {
		return TaskSpec{}, invalid("invalid question count")
	}
	if err := taskSize(config); err != nil {
		return TaskSpec{}, err
	}
	config = cloneTask(config)
	for _, id := range []shoal.ID{config.OwnerID, config.InputSchemaID, config.EvidencePolicyID, config.LabelPolicyID, config.EvaluationPolicyID, config.AggregationID} {
		if err := requiredID(id); err != nil {
			return TaskSpec{}, err
		}
	}
	for _, s := range []string{config.Name, config.Version, config.PredictionUnit, config.LabelUnit, config.ActionUnit} {
		if err := requiredText(s); err != nil {
			return TaskSpec{}, err
		}
	}
	sort.Slice(config.Questions, func(i, j int) bool { return config.Questions[i].ID < config.Questions[j].ID })
	for i := range config.Questions {
		q := &config.Questions[i]
		if err := requiredID(q.ID); err != nil {
			return TaskSpec{}, err
		}
		if err := requiredID(q.RubricID); err != nil {
			return TaskSpec{}, err
		}
		if i > 0 && config.Questions[i-1].ID == q.ID {
			return TaskSpec{}, invalid("duplicate question")
		}
		switch q.Kind {
		case Choice, Ordinal:
			if len(q.Labels) < 2 || len(q.Labels) > MaxLabels {
				return TaskSpec{}, invalid("invalid label count")
			}
		case Probability:
			if len(q.Labels) != 0 {
				return TaskSpec{}, invalid("probability question cannot have labels")
			}
			q.Labels = nil
		default:
			return TaskSpec{}, invalid("unknown answer kind")
		}
		seen := map[string]bool{}
		for _, label := range q.Labels {
			if err := requiredText(label); err != nil {
				return TaskSpec{}, err
			}
			if seen[label] {
				return TaskSpec{}, invalid("duplicate label")
			}
			seen[label] = true
		}
		if q.Kind == Choice {
			sort.Strings(q.Labels)
		}
	}
	id, err := identity("task", config)
	if err != nil {
		return TaskSpec{}, err
	}
	return TaskSpec{id, config}, nil
}
func (t TaskSpec) ID() shoal.ID       { return t.id }
func (t TaskSpec) Config() TaskConfig { return cloneTask(t.config) }
func (t TaskSpec) Validate() error {
	rebuilt, err := NewTaskSpec(t.config)
	if err != nil {
		return err
	}
	if rebuilt.id != t.id {
		return invalid("task identity mismatch")
	}
	return nil
}
func cloneTask(c TaskConfig) TaskConfig {
	c.Questions = append([]Question(nil), c.Questions...)
	for i := range c.Questions {
		c.Questions[i].Labels = append([]string(nil), c.Questions[i].Labels...)
	}
	return c
}

// EvidenceRole keeps normative documents, source observations, reported
// outcomes and predictions distinct. Role never grants authority.
type EvidenceRole string

const (
	Normative       EvidenceRole = "normative"
	Observation     EvidenceRole = "observation"
	ReportedOutcome EvidenceRole = "reported_outcome"
	Prediction      EvidenceRole = "prediction"
)

type Control string

const (
	CandidateControlled Control = "candidate_controlled"
	ExternalControlled  Control = "external_controlled"
	RegistryControlled  Control = "registry_controlled"
	UnknownControl      Control = "unknown"
)

// OriginID identifies an underlying origin, so duplicated reports can retain
// common provenance. AuthorityPolicyID references the server's classification
// policy; it does not mean this source is trusted. AttestationID is optional:
// a supplied reference still requires verification by the admitting service.
// ArtifactID is the source document identity; ID identifies this observation
// entry, allowing multiple pinned revisions of the same document. Document
// anchors must match ArtifactID and RevisionID. Graph/source associations still
// require verification by the evidence builder.
type Source struct {
	ArtifactID        shoal.ID
	ID                shoal.ID
	RevisionID        shoal.ID
	Digest            string
	OriginID          shoal.ID
	AuthorityPolicyID shoal.ID
	AttestationID     shoal.ID
	Role              EvidenceRole
	Control           Control
	ObservedAt        time.Time
	ReceivedAt        time.Time
}

// IncidentLinkState describes how far an incident can be joined to the
// deployed artifact and source change represented by a picture. A state is an
// evidence condition, not a risk label and never grants authority.
type IncidentLinkState string

const (
	IncidentLinked     IncidentLinkState = "linked"
	IncidentUnresolved IncidentLinkState = "unresolved"
	IncidentConflicted IncidentLinkState = "conflicted"
)

// IncidentReference binds an incident or alert observation to operational
// lineage. ChangeID is intentionally generic: an external showcase may use a
// pull request, changeset, or release candidate without importing GitHub into
// the decision core. Linked records require the complete chain; unresolved or
// conflicted records retain a bounded reason so consumers can abstain rather
// than mistake missing lineage for a low-risk observation.
type IncidentReference struct {
	IncidentID        shoal.ID
	ServiceID         shoal.ID
	EnvironmentID     shoal.ID
	DeploymentID      shoal.ID
	ArtifactID        shoal.ID
	RevisionID        shoal.ID
	ChangeID          shoal.ID
	AuthorityPolicyID shoal.ID
	Digest            string
	State             IncidentLinkState
	Reason            string
	ObservedAt        time.Time
	ReceivedAt        time.Time
}

type Disposition string

const (
	Supported   Disposition = "supported"
	Unsupported Disposition = "unsupported"
	Missing     Disposition = "missing"
	OutOfScope  Disposition = "out_of_scope"
)

// Subject represents one independently enumerated input, including unsupported
// constructs. Evidence references are exact anchors in the bound context pack.
type Subject struct {
	ID          shoal.ID
	SourceID    shoal.ID
	Disposition Disposition
	Reason      string
	EvidenceIDs []shoal.ID
}

// Measurement never turns an unknown denominator into complete coverage.
// Known zero is not applicable, not 100 percent. Unit and MethodID define what
// is measured; unrelated measurements cannot be added as a completeness claim.
type Measurement struct {
	ID          shoal.ID
	Unit        string
	MethodID    shoal.ID
	Numerator   uint64
	Denominator *uint64
}

// PictureConfig binds collection and serialization artifacts by identity.
// EnumerationID pins the independently retained inventory; construction cannot
// prove that an external collector enumerated every real-world subject.
// OntologyProjectionID is required even for an explicit versioned no-ontology projection.
// Policy decides freshness and evidence eligibility; this type grants neither.
type PictureConfig struct {
	TaskID               shoal.ID
	ObservationID        shoal.ID
	EnumerationID        shoal.ID
	ScopeID              shoal.ID
	BuilderID            shoal.ID
	OntologyProjectionID shoal.ID
	InputDigest          string
	TokenizerID          shoal.ID
	InputTokens          uint64
	TokenBudget          uint64
	Truncated            bool
	Cutoff               time.Time
	Sources              []Source
	Incidents            []IncidentReference `json:",omitempty"`
	Subjects             []Subject
	Measurements         []Measurement
}

type PictureManifest struct {
	id     shoal.ID
	pack   inference.ContextPack
	config PictureConfig
}

func NewPictureManifest(pack inference.ContextPack, config PictureConfig) (PictureManifest, error) {
	if err := pack.Validate(); err != nil {
		return PictureManifest{}, err
	}
	if len(config.Sources) == 0 || len(config.Sources) > MaxSources || len(config.Incidents) > MaxIncidents || len(config.Subjects) == 0 || len(config.Subjects) > MaxSubjects || len(config.Measurements) > MaxMeasurements {
		return PictureManifest{}, invalid("invalid picture counts")
	}
	// Check nested counts before cloning caller-owned slices.
	for _, s := range config.Subjects {
		if len(s.EvidenceIDs) > inference.MaxEvidenceAnchors {
			return PictureManifest{}, invalid("too many subject anchors")
		}
	}
	if err := pictureSize(config); err != nil {
		return PictureManifest{}, err
	}
	config = clonePicture(config)
	for _, id := range []shoal.ID{config.TaskID, config.ObservationID, config.EnumerationID, config.ScopeID, config.BuilderID, config.OntologyProjectionID, config.TokenizerID} {
		if err := requiredID(id); err != nil {
			return PictureManifest{}, err
		}
	}
	if !digestValid(config.InputDigest) {
		return PictureManifest{}, invalid("invalid input digest")
	}
	if !validTime(config.Cutoff) {
		return PictureManifest{}, invalid("invalid cutoff")
	}
	config.Cutoff = utc(config.Cutoff)
	if config.TokenBudget == 0 || config.InputTokens > config.TokenBudget {
		return PictureManifest{}, invalid("input exceeds token budget")
	}
	if config.Cutoff.Before(pack.Snapshot().AsOf()) || !config.Cutoff.Before(pack.Authorization().ExpiresAt()) {
		return PictureManifest{}, invalid("cutoff outside snapshot/authorization interval")
	}
	sort.Slice(config.Sources, func(i, j int) bool { return config.Sources[i].ID < config.Sources[j].ID })
	sources := map[shoal.ID]Source{}
	for i := range config.Sources {
		s := &config.Sources[i]
		for _, id := range []shoal.ID{s.ID, s.ArtifactID, s.RevisionID, s.OriginID, s.AuthorityPolicyID} {
			if err := requiredID(id); err != nil {
				return PictureManifest{}, err
			}
		}
		if s.AttestationID != "" {
			if err := requiredID(s.AttestationID); err != nil {
				return PictureManifest{}, err
			}
		}
		if _, exists := sources[s.ID]; exists {
			return PictureManifest{}, invalid("duplicate source")
		}
		sources[s.ID] = *s
		if !digestValid(s.Digest) {
			return PictureManifest{}, invalid("invalid source digest")
		}
		switch s.Role {
		case Normative, Observation, ReportedOutcome, Prediction:
		default:
			return PictureManifest{}, invalid("unknown evidence role")
		}
		switch s.Control {
		case CandidateControlled, ExternalControlled, RegistryControlled, UnknownControl:
		default:
			return PictureManifest{}, invalid("unknown author control")
		}
		if !validTime(s.ObservedAt) || !validTime(s.ReceivedAt) || s.ReceivedAt.Before(s.ObservedAt) || s.ReceivedAt.After(config.Cutoff) {
			return PictureManifest{}, invalid("invalid observation chronology")
		}
		s.ObservedAt = utc(s.ObservedAt)
		s.ReceivedAt = utc(s.ReceivedAt)
	}
	sort.Slice(config.Incidents, func(i, j int) bool { return config.Incidents[i].IncidentID < config.Incidents[j].IncidentID })
	incidents := map[shoal.ID]struct{}{}
	for i := range config.Incidents {
		incident := &config.Incidents[i]
		if err := requiredID(incident.IncidentID); err != nil {
			return PictureManifest{}, err
		}
		if _, exists := incidents[incident.IncidentID]; exists {
			return PictureManifest{}, invalid("duplicate incident")
		}
		incidents[incident.IncidentID] = struct{}{}
		if !digestValid(incident.Digest) {
			return PictureManifest{}, invalid("invalid incident digest")
		}
		if !validTime(incident.ObservedAt) || !validTime(incident.ReceivedAt) || incident.ReceivedAt.Before(incident.ObservedAt) || incident.ReceivedAt.After(config.Cutoff) {
			return PictureManifest{}, invalid("invalid incident chronology")
		}
		incident.ObservedAt = utc(incident.ObservedAt)
		incident.ReceivedAt = utc(incident.ReceivedAt)
		switch incident.State {
		case IncidentLinked:
			for _, id := range []shoal.ID{incident.ServiceID, incident.EnvironmentID, incident.DeploymentID, incident.ArtifactID, incident.RevisionID, incident.ChangeID, incident.AuthorityPolicyID} {
				if err := requiredID(id); err != nil {
					return PictureManifest{}, invalid("linked incident is missing lineage")
				}
			}
			matchedSource := false
			for _, source := range config.Sources {
				if source.ArtifactID == incident.ArtifactID && source.RevisionID == incident.RevisionID {
					matchedSource = true
					break
				}
			}
			if !matchedSource {
				return PictureManifest{}, invalid("linked incident artifact and revision are outside sources")
			}
			if incident.Reason != "" {
				return PictureManifest{}, invalid("linked incident cannot carry unresolved reason")
			}
		case IncidentUnresolved, IncidentConflicted:
			if err := requiredText(incident.Reason); err != nil {
				return PictureManifest{}, invalid("unresolved incident requires reason")
			}
		default:
			return PictureManifest{}, invalid("unknown incident link state")
		}
	}
	anchors := map[shoal.ID]inference.EvidenceAnchor{}
	used := map[shoal.ID]bool{}
	for _, a := range pack.Evidence() {
		anchors[a.ID()] = a
	}
	sort.Slice(config.Subjects, func(i, j int) bool { return config.Subjects[i].ID < config.Subjects[j].ID })
	for i := range config.Subjects {
		s := &config.Subjects[i]
		if err := requiredID(s.ID); err != nil {
			return PictureManifest{}, err
		}
		if _, exists := sources[s.SourceID]; !exists {
			return PictureManifest{}, invalid("unknown subject source")
		}
		if i > 0 && config.Subjects[i-1].ID == s.ID {
			return PictureManifest{}, invalid("duplicate subject")
		}
		switch s.Disposition {
		case Supported:
			if len(s.EvidenceIDs) == 0 {
				return PictureManifest{}, invalid("supported subject requires evidence")
			}
		case Unsupported, Missing, OutOfScope:
			if err := requiredText(s.Reason); err != nil {
				return PictureManifest{}, err
			}
		default:
			return PictureManifest{}, invalid("unknown subject disposition")
		}
		if s.Reason != "" {
			if err := requiredText(s.Reason); err != nil {
				return PictureManifest{}, err
			}
		}
		sort.Slice(s.EvidenceIDs, func(i, j int) bool { return s.EvidenceIDs[i] < s.EvidenceIDs[j] })
		for j, id := range s.EvidenceIDs {
			anchor, exists := anchors[id]
			if !exists {
				return PictureManifest{}, invalid("subject anchor outside context pack")
			}
			source := sources[s.SourceID]
			if citation, _, document := anchor.Document(); document && (citation.DocumentID != source.ArtifactID || citation.RevisionID != source.RevisionID) {
				return PictureManifest{}, invalid("document evidence does not match source revision")
			}
			if j > 0 && s.EvidenceIDs[j-1] == id {
				return PictureManifest{}, invalid("duplicate subject anchor")
			}
			used[id] = true
		}
	}
	if len(used) != len(anchors) {
		return PictureManifest{}, invalid("unaccounted context evidence")
	}
	sort.Slice(config.Measurements, func(i, j int) bool { return config.Measurements[i].ID < config.Measurements[j].ID })
	for i, m := range config.Measurements {
		if err := requiredID(m.ID); err != nil {
			return PictureManifest{}, err
		}
		if err := requiredID(m.MethodID); err != nil {
			return PictureManifest{}, err
		}
		if err := requiredText(m.Unit); err != nil {
			return PictureManifest{}, err
		}
		if i > 0 && config.Measurements[i-1].ID == m.ID {
			return PictureManifest{}, invalid("duplicate measurement")
		}
		if m.Denominator != nil && m.Numerator > *m.Denominator {
			return PictureManifest{}, invalid("numerator exceeds denominator")
		}
	}
	// Pack identity already binds exact anchors, ontology and authorization. Do
	// not serialize its private fields as an empty JSON object.
	payload := struct {
		PackID shoal.ID
		Config any
	}{pack.ID(), pictureIdentityConfig(config)}
	id, err := identity("picture", payload)
	if err != nil {
		return PictureManifest{}, err
	}
	return PictureManifest{id, pack, config}, nil
}

// pictureIdentityConfig preserves the pre-incident JSON shape for existing
// pictures. Adding an optional field to PictureConfig must not change the
// identity of a legacy picture that has no incident references; otherwise
// durable receipts and replayed requests would appear to point at new input.
func pictureIdentityConfig(c PictureConfig) any {
	if len(c.Incidents) != 0 {
		return c
	}
	return struct {
		TaskID               shoal.ID
		ObservationID        shoal.ID
		EnumerationID        shoal.ID
		ScopeID              shoal.ID
		BuilderID            shoal.ID
		OntologyProjectionID shoal.ID
		InputDigest          string
		TokenizerID          shoal.ID
		InputTokens          uint64
		TokenBudget          uint64
		Truncated            bool
		Cutoff               time.Time
		Sources              []Source
		Subjects             []Subject
		Measurements         []Measurement
	}{
		TaskID:               c.TaskID,
		ObservationID:        c.ObservationID,
		EnumerationID:        c.EnumerationID,
		ScopeID:              c.ScopeID,
		BuilderID:            c.BuilderID,
		OntologyProjectionID: c.OntologyProjectionID,
		InputDigest:          c.InputDigest,
		TokenizerID:          c.TokenizerID,
		InputTokens:          c.InputTokens,
		TokenBudget:          c.TokenBudget,
		Truncated:            c.Truncated,
		Cutoff:               c.Cutoff,
		Sources:              c.Sources,
		Subjects:             c.Subjects,
		Measurements:         c.Measurements,
	}
}

// ContextPack returns the immutable evidence pack pinned by this picture.
func (p PictureManifest) ContextPack() inference.ContextPack { return p.pack }

func (p PictureManifest) ID() shoal.ID                     { return p.id }
func (p PictureManifest) ContextPackID() shoal.ID          { return p.pack.ID() }
func (p PictureManifest) Snapshot() inference.SnapshotPin  { return p.pack.Snapshot() }
func (p PictureManifest) Authorization() inference.AuthPin { return p.pack.Authorization() }
func (p PictureManifest) Config() PictureConfig            { return clonePicture(p.config) }
func (p PictureManifest) Validate() error {
	rebuilt, err := NewPictureManifest(p.pack, p.config)
	if err != nil {
		return err
	}
	if rebuilt.id != p.id {
		return invalid("picture identity mismatch")
	}
	return nil
}

// InventoryCoverage counts only enumerated subjects with evidence. It says
// nothing about undiscovered dependencies or the truth of the evidence.
func (p PictureManifest) InventoryCoverage() (supported, total int) {
	for _, s := range p.config.Subjects {
		if s.Disposition == Supported {
			supported++
		}
	}
	return supported, len(p.config.Subjects)
}
func clonePicture(c PictureConfig) PictureConfig {
	c.Sources = append([]Source(nil), c.Sources...)
	c.Incidents = append([]IncidentReference(nil), c.Incidents...)
	c.Subjects = append([]Subject(nil), c.Subjects...)
	for i := range c.Subjects {
		c.Subjects[i].EvidenceIDs = append([]shoal.ID(nil), c.Subjects[i].EvidenceIDs...)
	}
	c.Measurements = append([]Measurement(nil), c.Measurements...)
	for i := range c.Measurements {
		if c.Measurements[i].Denominator != nil {
			n := *c.Measurements[i].Denominator
			c.Measurements[i].Denominator = &n
		}
	}
	return c
}
func requiredID(id shoal.ID) error {
	if err := shoal.ValidateRequiredID("decision reference", id); err != nil {
		return err
	}
	if !utf8.ValidString(string(id)) {
		return invalid("decision references require UTF-8")
	}
	return nil
}
func requiredText(s string) error {
	if !utf8.ValidString(s) || strings.TrimSpace(s) == "" || len(s) > shoal.MaxSemanticStringBytes {
		return invalid("invalid bounded text")
	}
	return nil
}
func validTime(t time.Time) bool { return !t.IsZero() && t.Year() >= 1 && t.Year() <= 9999 }
func utc(t time.Time) time.Time  { return t.Round(0).UTC() }
func digestValid(s string) bool {
	if len(s) != 64 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
func identity(kind string, v any) (shoal.ID, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", invalid("cannot encode decision record")
	}
	if len(b) > MaxManifestBytes {
		return "", invalid("decision record exceeds byte bound")
	}
	sum := sha256.Sum256(b)
	return shoal.ID("decision:" + kind + ":v1:" + hex.EncodeToString(sum[:])), nil
}
func invalid(s string) error { return shoal.NewError(shoal.ErrorInvalidArgument, s) }

// Conservative preflight bounds copying and JSON allocation, including worst
// case JSON string escaping and field overhead. Final encoded size is checked
// again before deriving an identity.
type byteBudget int

func (b *byteBudget) charge(n int) error {
	if n < 0 || n > MaxManifestBytes-int(*b) {
		return invalid("decision record exceeds preflight byte bound")
	}
	*b += byteBudget(n)
	return nil
}
func (b *byteBudget) text(s string) error {
	if len(s) > MaxManifestBytes/6 {
		return invalid("decision field exceeds byte bound")
	}
	return b.charge(6*len(s) + 16)
}
func (b *byteBudget) ids(ids ...shoal.ID) error {
	for _, id := range ids {
		if err := b.text(string(id)); err != nil {
			return err
		}
	}
	return nil
}
func taskSize(c TaskConfig) error {
	var b byteBudget
	if err := b.charge(2048); err != nil {
		return err
	}
	if err := b.ids(c.OwnerID, c.InputSchemaID, c.EvidencePolicyID, c.LabelPolicyID, c.EvaluationPolicyID, c.AggregationID); err != nil {
		return err
	}
	for _, s := range []string{c.Name, c.Version, c.PredictionUnit, c.LabelUnit, c.ActionUnit} {
		if err := b.text(s); err != nil {
			return err
		}
	}
	for _, q := range c.Questions {
		if len(q.Labels) > MaxLabels {
			return invalid("too many labels")
		}
		if err := b.charge(256); err != nil {
			return err
		}
		if err := b.ids(q.ID, q.RubricID); err != nil {
			return err
		}
		if err := b.text(string(q.Kind)); err != nil {
			return err
		}
		for _, l := range q.Labels {
			if err := b.text(l); err != nil {
				return err
			}
		}
	}
	return nil
}
func pictureSize(c PictureConfig) error {
	var b byteBudget
	if err := b.charge(4096); err != nil {
		return err
	}
	if err := b.ids(c.TaskID, c.ObservationID, c.EnumerationID, c.ScopeID, c.BuilderID, c.OntologyProjectionID, c.TokenizerID); err != nil {
		return err
	}
	if err := b.text(c.InputDigest); err != nil {
		return err
	}
	for _, s := range c.Sources {
		if err := b.charge(1024); err != nil {
			return err
		}
		if err := b.ids(s.ID, s.ArtifactID, s.RevisionID, s.OriginID, s.AuthorityPolicyID, s.AttestationID); err != nil {
			return err
		}
		for _, v := range []string{s.Digest, string(s.Role), string(s.Control)} {
			if err := b.text(v); err != nil {
				return err
			}
		}
	}
	for _, incident := range c.Incidents {
		if err := b.charge(1024); err != nil {
			return err
		}
		if err := b.ids(incident.IncidentID, incident.ServiceID, incident.EnvironmentID, incident.DeploymentID, incident.ArtifactID, incident.RevisionID, incident.ChangeID, incident.AuthorityPolicyID); err != nil {
			return err
		}
		for _, v := range []string{incident.Digest, string(incident.State), incident.Reason} {
			if err := b.text(v); err != nil {
				return err
			}
		}
	}
	for _, s := range c.Subjects {
		if err := b.charge(256); err != nil {
			return err
		}
		if err := b.ids(s.ID, s.SourceID); err != nil {
			return err
		}
		for _, v := range []string{s.Reason, string(s.Disposition)} {
			if err := b.text(v); err != nil {
				return err
			}
		}
		if err := b.ids(s.EvidenceIDs...); err != nil {
			return err
		}
	}
	for _, m := range c.Measurements {
		if err := b.charge(256); err != nil {
			return err
		}
		if err := b.ids(m.ID, m.MethodID); err != nil {
			return err
		}
		if err := b.text(m.Unit); err != nil {
			return err
		}
	}
	return nil
}
