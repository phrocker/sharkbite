// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

// Package distillation defines the selective distillation job manifest and
// the trainer contract. Selection has two dimensions and both are recorded
// inputs: what the student learns (tasks, admitted cohorts and label
// policies, evidence families) and which models it learns from and is built
// on (eligible teachers and bases, the rule, the measurements, the choice,
// and every rejection). The manifest is content-addressed; changing any
// selection changes the pin and therefore the bundle lineage.
package distillation

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/phrocker/shoal-oss/internal/weightsbundle"
)

// Kind is the manifest record kind.
const Kind = "distillation-manifest"

// Egress postures. They are resolved from configuration by the caller
// (model.EgressesOffHost), never declared by the manifest author.
const (
	EgressLoopback = "loopback"
	EgressOffHost  = "off_host"
)

// Model roles in a selection.
const (
	RoleTeacher = "teacher"
	RoleBase    = "base"
)

// SplitDevelopment is the only split whose measurements may inform model
// selection. Held-out evidence never selects a model.
const SplitDevelopment = "development"

// Rule directions.
const (
	Maximize = "maximize"
	Minimize = "minimize"
)

// Bounds.
const (
	MaxEligible     = 64
	MaxTasks        = 64
	MaxCohorts      = 256
	MaxHeldOut      = 1 << 16
	MaxMeasurements = 256
)

// ErrInvalid reports a manifest that cannot be pinned.
var ErrInvalid = errors.New("invalid distillation manifest")

// ErrRefused reports a job, export, receipt, or prediction set the manifest
// does not admit. The message is the verbatim reason.
var ErrRefused = errors.New("distillation refused")

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, args...))
}

// ModelIdentity names one teacher or base exactly.
type ModelIdentity struct {
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	Version       string `json:"version"`
	CacheIdentity string `json:"cache_identity"`
	Egress        string `json:"egress"`
	BundleDigest  string `json:"bundle_digest,omitempty"`
}

// Measurement is one development-split observation the selection rule used.
type Measurement struct {
	Metric     string  `json:"metric"`
	Value      float64 `json:"value"`
	Split      string  `json:"split"`
	EvidenceID string  `json:"evidence_id"`
}

// Candidate is an eligible model with the measurements recorded for it.
type Candidate struct {
	Alias        string        `json:"alias"`
	Identity     ModelIdentity `json:"identity"`
	Measurements []Measurement `json:"measurements"`
}

// Rule is the declarative selection rule: pick the eligible model whose
// recorded metric is best in Direction, subject to an optional threshold.
type Rule struct {
	Metric    string   `json:"metric"`
	Direction string   `json:"direction"`
	Minimum   *float64 `json:"minimum"`
	Maximum   *float64 `json:"maximum"`
}

// Rejection records why an eligible model was not chosen.
type Rejection struct {
	Alias  string `json:"alias"`
	Reason string `json:"reason"`
}

// ModelSelection is one dimension of model choice: the eligible set, the
// rule, the measurements behind it, the choice, and each rejection.
type ModelSelection struct {
	Role     string      `json:"role"`
	Eligible []Candidate `json:"eligible"`
	Rule     Rule        `json:"rule"`
	Chosen   string      `json:"chosen"`
	Rejected []Rejection `json:"rejected"`
}

// TaskSelection admits cohorts and label policies for one task.
type TaskSelection struct {
	TaskID         string   `json:"task_id"`
	CohortIDs      []string `json:"cohort_ids"`
	LabelPolicyIDs []string `json:"label_policy_ids"`
}

// DataSelection is what the student may learn.
type DataSelection struct {
	Tasks                []TaskSelection `json:"tasks"`
	EvidenceFamilies     []string        `json:"evidence_families"`
	PromptVersion        string          `json:"prompt_version"`
	GenerationParameters map[string]any  `json:"generation_parameters"`
	HeldOutExampleIDs    []string        `json:"held_out_example_ids"`
}

// ExportPin names one authorized cohort export the job may consume.
type ExportPin struct {
	CohortID       string `json:"cohort_id"`
	ManifestSHA256 string `json:"manifest_sha256"`
}

// Manifest is the content-addressed job input.
type Manifest struct {
	Schema                    int            `json:"schema"`
	Kind                      string         `json:"kind"`
	TrainingManifestSHA256    string         `json:"training_manifest_sha256"`
	Teacher                   ModelSelection `json:"teacher"`
	Base                      ModelSelection `json:"base"`
	TeacherEgressAtGeneration string         `json:"teacher_egress_at_generation"`
	Data                      DataSelection  `json:"data"`
	Exports                   []ExportPin    `json:"exports"`
}

// Validate checks the manifest is a complete, internally consistent record
// of both selections. A chosen model outside the eligible set, a chosen
// model whose measurements do not satisfy the rule, an unexplained rejection,
// or a measurement from a non-development split is refused.
func (m Manifest) Validate() error {
	if m.Schema != 1 || m.Kind != Kind {
		return invalid("schema %d kind %q", m.Schema, m.Kind)
	}
	if !validDigest(m.TrainingManifestSHA256) {
		return invalid("training_manifest_sha256")
	}
	if err := m.Teacher.validate(RoleTeacher); err != nil {
		return err
	}
	if err := m.Base.validate(RoleBase); err != nil {
		return err
	}
	teacher, _ := m.Teacher.chosen()
	if m.TeacherEgressAtGeneration != teacher.Identity.Egress {
		return invalid("teacher egress at generation %q does not match the chosen teacher's resolved posture %q", m.TeacherEgressAtGeneration, teacher.Identity.Egress)
	}
	if err := m.Data.validate(); err != nil {
		return err
	}
	if len(m.Exports) == 0 || len(m.Exports) > MaxCohorts {
		return invalid("%d exports", len(m.Exports))
	}
	admitted := map[string]bool{}
	for _, t := range m.Data.Tasks {
		for _, c := range t.CohortIDs {
			admitted[c] = true
		}
	}
	seen := map[string]bool{}
	for _, e := range m.Exports {
		if e.CohortID == "" || !validDigest(e.ManifestSHA256) {
			return invalid("export pin for %q", e.CohortID)
		}
		if seen[e.CohortID] {
			return invalid("export %q pinned twice", e.CohortID)
		}
		seen[e.CohortID] = true
		if !admitted[e.CohortID] {
			return invalid("export %q is not an admitted cohort", e.CohortID)
		}
	}
	for c := range admitted {
		if !seen[c] {
			return invalid("admitted cohort %q has no export pin", c)
		}
	}
	return nil
}

// Pin returns the content address of the manifest.
func (m Manifest) Pin() (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}
	return weightsbundle.Digest(m)
}

// Encode returns canonical manifest bytes.
func (m Manifest) Encode() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return weightsbundle.Canonical(m)
}

// Decode parses manifest bytes strictly, validates them, and refuses bytes
// that do not round-trip: a member the typed form drops or rewrites (an
// explicit empty optional, a number spelled another way) would let two
// different byte strings share one pin.
func Decode(raw []byte) (Manifest, error) {
	var m Manifest
	if err := weightsbundle.StrictUnmarshal(raw, &m); err != nil {
		return Manifest{}, invalid("%v", err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	generic, err := weightsbundle.DecodeRecord(raw)
	if err != nil {
		return Manifest{}, invalid("%v", err)
	}
	typed, err := weightsbundle.Canonical(m)
	if err != nil {
		return Manifest{}, invalid("%v", err)
	}
	loose, err := weightsbundle.Canonical(generic)
	if err != nil {
		return Manifest{}, invalid("%v", err)
	}
	if string(typed) != string(loose) {
		return Manifest{}, invalid("manifest does not round-trip canonically: a member is absent, explicitly empty, or spelled in a form the typed record rewrites (a float such as a measurement value or rule bound must be written with a decimal point, e.g. 1.0 not 1)")
	}
	return m, nil
}

// TaskIDs returns the selected task scope in sorted order.
func (m Manifest) TaskIDs() []string {
	out := make([]string, 0, len(m.Data.Tasks))
	for _, t := range m.Data.Tasks {
		out = append(out, t.TaskID)
	}
	sort.Strings(out)
	return out
}

// Task returns the selection for a task.
func (m Manifest) Task(id string) (TaskSelection, bool) {
	for _, t := range m.Data.Tasks {
		if t.TaskID == id {
			return t, true
		}
	}
	return TaskSelection{}, false
}

// Export returns the pin for a cohort.
func (m Manifest) Export(cohortID string) (ExportPin, bool) {
	for _, e := range m.Exports {
		if e.CohortID == cohortID {
			return e, true
		}
	}
	return ExportPin{}, false
}

// HeldOut reports whether an example is excluded from generation,
// selection, and training.
func (m Manifest) HeldOut(exampleID string) bool {
	i := sort.SearchStrings(m.Data.HeldOutExampleIDs, exampleID)
	return i < len(m.Data.HeldOutExampleIDs) && m.Data.HeldOutExampleIDs[i] == exampleID
}

func (s ModelSelection) chosen() (Candidate, bool) {
	for _, c := range s.Eligible {
		if c.Alias == s.Chosen {
			return c, true
		}
	}
	return Candidate{}, false
}

func (s ModelSelection) validate(role string) error {
	if s.Role != role {
		return invalid("%s selection has role %q", role, s.Role)
	}
	if len(s.Eligible) == 0 || len(s.Eligible) > MaxEligible {
		return invalid("%s: %d eligible models", role, len(s.Eligible))
	}
	if s.Rule.Metric == "" || (s.Rule.Direction != Maximize && s.Rule.Direction != Minimize) {
		return invalid("%s: rule needs a metric and a direction", role)
	}
	if (s.Rule.Minimum != nil && !finite(*s.Rule.Minimum)) || (s.Rule.Maximum != nil && !finite(*s.Rule.Maximum)) {
		return invalid("%s: rule threshold must be finite", role)
	}
	aliases := map[string]bool{}
	for _, c := range s.Eligible {
		if c.Alias == "" || aliases[c.Alias] {
			return invalid("%s: alias %q empty or repeated", role, c.Alias)
		}
		aliases[c.Alias] = true
		if err := c.Identity.validate(); err != nil {
			return invalid("%s %q: %v", role, c.Alias, err)
		}
		if len(c.Measurements) > MaxMeasurements {
			return invalid("%s %q: too many measurements", role, c.Alias)
		}
		metrics := map[string]bool{}
		for _, meas := range c.Measurements {
			if meas.Split != SplitDevelopment {
				return invalid("%s %q: measurement %q from split %q; only development evidence selects a model", role, c.Alias, meas.Metric, meas.Split)
			}
			if meas.Metric == "" || meas.EvidenceID == "" || !finite(meas.Value) {
				return invalid("%s %q: measurement needs metric, finite value, and evidence", role, c.Alias)
			}
			if metrics[meas.Metric] {
				return invalid("%s %q: metric %q measured twice; one recorded value per metric", role, c.Alias, meas.Metric)
			}
			metrics[meas.Metric] = true
		}
	}
	chosen, ok := s.chosen()
	if !ok {
		return invalid("%s: chosen %q is not in the eligible set", role, s.Chosen)
	}
	chosenValue, ok := chosen.value(s.Rule.Metric)
	if !ok {
		return invalid("%s: chosen %q has no measurement of %q", role, s.Chosen, s.Rule.Metric)
	}
	if !s.Rule.satisfied(chosenValue) {
		return invalid("%s: chosen %q measures %s=%v, outside the rule's threshold", role, s.Chosen, s.Rule.Metric, chosenValue)
	}
	rejected := map[string]string{}
	for _, r := range s.Rejected {
		if r.Alias == "" || r.Reason == "" || !aliases[r.Alias] || r.Alias == s.Chosen {
			return invalid("%s: rejection of %q needs an eligible, unchosen alias and a reason", role, r.Alias)
		}
		if _, dup := rejected[r.Alias]; dup {
			return invalid("%s: %q rejected twice", role, r.Alias)
		}
		rejected[r.Alias] = r.Reason
	}
	for _, c := range s.Eligible {
		if c.Alias == s.Chosen {
			continue
		}
		if _, ok := rejected[c.Alias]; !ok {
			return invalid("%s: eligible %q neither chosen nor rejected with a reason", role, c.Alias)
		}
	}
	// The rule is advisory over the recorded measurements: the choice may
	// depart from what Apply() would pick only by rejecting the rule's
	// preference with a recorded reason, which the loop above requires for
	// every unchosen model. Apply() lets a reader see when that happened.
	return nil
}

func (c Candidate) value(metric string) (float64, bool) {
	for _, m := range c.Measurements {
		if m.Metric == metric {
			return m.Value, true
		}
	}
	return 0, false
}

func (r Rule) satisfied(v float64) bool {
	if r.Minimum != nil && v < *r.Minimum {
		return false
	}
	if r.Maximum != nil && v > *r.Maximum {
		return false
	}
	return true
}

func (r Rule) better(candidate, chosen float64) bool {
	if r.Direction == Maximize {
		return candidate > chosen
	}
	return candidate < chosen
}

// Apply evaluates the rule over the eligible set and returns the alias the
// rule selects, so a caller can check that the recorded choice is the rule's
// choice before trusting the manifest.
func (s ModelSelection) Apply() (string, error) {
	best, bestValue := "", math.NaN()
	for _, c := range s.Eligible {
		v, ok := c.value(s.Rule.Metric)
		if !ok || !s.Rule.satisfied(v) {
			continue
		}
		if best == "" || s.Rule.better(v, bestValue) {
			best, bestValue = c.Alias, v
		}
	}
	if best == "" {
		return "", refuse("%s: no eligible model satisfies the rule", s.Role)
	}
	return best, nil
}

func (id ModelIdentity) validate() error {
	if id.Provider == "" || id.Model == "" || id.Version == "" || id.CacheIdentity == "" {
		return errors.New("identity needs provider, model, version, and cache identity")
	}
	if id.Egress != EgressLoopback && id.Egress != EgressOffHost {
		return fmt.Errorf("egress %q is not a resolved posture", id.Egress)
	}
	if id.BundleDigest != "" && !validDigest(id.BundleDigest) {
		return errors.New("bundle digest")
	}
	return nil
}

func (d DataSelection) validate() error {
	if len(d.Tasks) == 0 || len(d.Tasks) > MaxTasks {
		return invalid("%d tasks selected", len(d.Tasks))
	}
	tasks := map[string]bool{}
	cohorts := 0
	for _, t := range d.Tasks {
		if t.TaskID == "" || tasks[t.TaskID] {
			return invalid("task %q empty or repeated", t.TaskID)
		}
		tasks[t.TaskID] = true
		if len(t.CohortIDs) == 0 || len(t.LabelPolicyIDs) == 0 {
			return invalid("task %q admits no cohorts or no label policies", t.TaskID)
		}
		if !uniqueNonEmpty(t.CohortIDs) || !uniqueNonEmpty(t.LabelPolicyIDs) {
			return invalid("task %q cohorts and label policies must be unique and non-empty", t.TaskID)
		}
		cohorts += len(t.CohortIDs)
	}
	if cohorts > MaxCohorts {
		return invalid("%d cohorts", cohorts)
	}
	if len(d.EvidenceFamilies) == 0 || !uniqueNonEmpty(d.EvidenceFamilies) {
		return invalid("evidence families must be a non-empty unique set")
	}
	if strings.TrimSpace(d.PromptVersion) == "" {
		return invalid("prompt version")
	}
	if d.GenerationParameters == nil {
		return invalid("generation parameters must be recorded (use an empty object)")
	}
	if len(d.HeldOutExampleIDs) > MaxHeldOut || !sort.StringsAreSorted(d.HeldOutExampleIDs) || !uniqueNonEmpty(d.HeldOutExampleIDs) {
		return invalid("held-out example ids must be sorted, unique, and bounded")
	}
	return nil
}

func uniqueNonEmpty(values []string) bool {
	seen := map[string]bool{}
	for _, v := range values {
		if v == "" || seen[v] {
			return false
		}
		seen[v] = true
	}
	return true
}

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

func validDigest(d string) bool {
	if len(d) != 64 || strings.ToLower(d) != d {
		return false
	}
	for _, r := range d {
		if !(r >= '0' && r <= '9') && !(r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}
