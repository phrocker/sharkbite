// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package distillation

import (
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/internal/weightsbundle"
)

// Record kinds the trainer contract exchanges.
const (
	KindJob         = "distillation-job"
	KindReceipt     = "distillation-receipt"
	KindPredictions = "teacher-predictions"
	KindComparison  = "student-comparison"
)

// ExportDescriptor is what the authorized cohort exporter produced for one
// cohort: its identities, the pinned manifest digest, and the example ids
// the export actually contains. The exporter produces only the selected
// slice, so the trainer cannot widen it; this check proves it did not.
type ExportDescriptor struct {
	CohortID       string   `json:"cohort_id"`
	TaskID         string   `json:"task_id"`
	LabelPolicyID  string   `json:"label_policy_id"`
	ManifestSHA256 string   `json:"manifest_sha256"`
	ExampleIDs     []string `json:"example_ids"`
}

// AuthorizeExport refuses an export the manifest does not admit: an
// unpinned or re-pinned cohort, a task or label policy outside the
// selection, or a held-out example present in the training slice.
func AuthorizeExport(m Manifest, export ExportDescriptor) error {
	if err := m.Validate(); err != nil {
		return err
	}
	pin, ok := m.Export(export.CohortID)
	if !ok {
		return refuse("cohort %q is not admitted by the manifest", export.CohortID)
	}
	if pin.ManifestSHA256 != export.ManifestSHA256 {
		return refuse("export manifest pin %s for cohort %q does not match the authorized pin %s", export.ManifestSHA256, export.CohortID, pin.ManifestSHA256)
	}
	task, ok := m.Task(export.TaskID)
	if !ok {
		return refuse("task %q is outside the selection", export.TaskID)
	}
	if !contains(task.CohortIDs, export.CohortID) {
		return refuse("cohort %q is not admitted for task %q", export.CohortID, export.TaskID)
	}
	if !contains(task.LabelPolicyIDs, export.LabelPolicyID) {
		return refuse("label policy %q is not admitted for task %q", export.LabelPolicyID, export.TaskID)
	}
	if len(export.ExampleIDs) == 0 {
		return refuse("export for cohort %q lists no example ids; the held-out check cannot run over an unknown slice", export.CohortID)
	}
	for _, id := range export.ExampleIDs {
		if m.HeldOut(id) {
			return refuse("export for cohort %q contains held-out example %q", export.CohortID, id)
		}
	}
	return nil
}

// Job is what a trainer consumes: the manifest pin and the exports it may
// read. Requested selections are the manifest; actual selections are what
// the receipt reports.
type Job struct {
	Schema      int                `json:"schema"`
	Kind        string             `json:"kind"`
	ManifestPin string             `json:"manifest_pin"`
	Exports     []ExportDescriptor `json:"exports"`
}

// NewJob authorizes every export against the manifest and binds the pin.
// Every admitted cohort must be present: a job over a subset would train a
// student on a narrower selection than the one recorded.
func NewJob(m Manifest, exports []ExportDescriptor) (Job, error) {
	pin, err := m.Pin()
	if err != nil {
		return Job{}, err
	}
	seen := map[string]bool{}
	for _, e := range exports {
		if err := AuthorizeExport(m, e); err != nil {
			return Job{}, err
		}
		if seen[e.CohortID] {
			return Job{}, refuse("cohort %q supplied twice", e.CohortID)
		}
		seen[e.CohortID] = true
	}
	for _, e := range m.Exports {
		if !seen[e.CohortID] {
			return Job{}, refuse("admitted cohort %q has no export", e.CohortID)
		}
	}
	return Job{Schema: 1, Kind: KindJob, ManifestPin: pin, Exports: exports}, nil
}

// Executor names who ran the job.
type Executor struct {
	ID            string `json:"id"`
	AttestationID string `json:"attestation_id"`
	Host          string `json:"host"`
}

// Deviation is one recorded difference between what the manifest requested
// and what the run did. It is structured so a receipt cannot carry prose in
// place of the difference; the verifier recomputes the list and requires an
// exact match.
type Deviation struct {
	Parameter string `json:"parameter"`
	Requested any    `json:"requested"`
	Actual    any    `json:"actual"`
}

// AuthorizedExport is what the trainer reports it consumed for one cohort:
// the identities the authorized export manifest carried. The verifier checks
// the admitted ones against the manifest; the rest are provenance it bounds.
type AuthorizedExport struct {
	CohortID            string `json:"cohort_id"`
	TaskID              string `json:"task_id"`
	LabelPolicyID       string `json:"label_policy_id"`
	ManifestSHA256      string `json:"manifest_sha256"`
	CohortSHA256        string `json:"cohort_sha256"`
	AuthorityRevisionID string `json:"authority_revision_id"`
	InventoryID         string `json:"inventory_id"`
	TrainingPurposeID   string `json:"training_purpose_id"`
	DatasetSHA256       string `json:"dataset_sha256"`
	DatasetBytes        int64  `json:"dataset_bytes"`
	ProvenanceMode      string `json:"provenance_mode"`
	CheckpointOverlap   string `json:"checkpoint_overlap"`
}

// Receipt statuses. Anything else is refused.
const (
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusRefused   = "refused"
)

// Bounds on free-form receipt members, so a receipt cannot carry an essay.
const (
	MaxRuntimeIdentities = 64
	MaxReceiptString     = 256
)

// Receipt is what a trainer emits. It names requested versus actual
// parameters and selections, runtime identities, the executor, and the
// bundle it produced. It carries no quality claim: completion is not
// improvement, and evaluation is a separate record.
type Receipt struct {
	Schema                int                `json:"schema"`
	Kind                  string             `json:"kind"`
	ManifestPin           string             `json:"manifest_pin"`
	RequestedParameters   map[string]any     `json:"requested_parameters"`
	ActualParameters      map[string]any     `json:"actual_parameters"`
	RequestedCohorts      []string           `json:"requested_cohorts"`
	ActualCohorts         []string           `json:"actual_cohorts"`
	RequestedTeacher      ModelIdentity      `json:"requested_teacher"`
	ActualTeacher         ModelIdentity      `json:"actual_teacher"`
	RequestedBase         ModelIdentity      `json:"requested_base"`
	ActualBase            ModelIdentity      `json:"actual_base"`
	RuntimeIdentities     map[string]string  `json:"runtime_identities"`
	Executor              Executor           `json:"executor"`
	BundleDigest          string             `json:"bundle_digest"`
	TeacherPredictionsID  string             `json:"teacher_predictions_id"`
	HeldOutExcludedCount  int                `json:"held_out_excluded_count"`
	TrainedExampleCount   int                `json:"trained_example_count"`
	Status                string             `json:"status"`
	Deviations            []Deviation        `json:"deviations"`
	CandidateOnly         bool               `json:"candidate_only"`
	ImprovementClaimed    bool               `json:"improvement_claimed"`
	EvaluationPerformedBy string             `json:"evaluation_performed_by"`
	AuthorizedExports     []AuthorizedExport `json:"authorized_exports"`
}

func bounded(s string) bool { return utf8.ValidString(s) && len(s) <= MaxReceiptString }

// ExpectedDeviations recomputes the deviation list a receipt must carry:
// one entry per requested parameter whose actual value differs, plus one
// for the cohort set when it differs. Parameters are listed in sorted order.
func ExpectedDeviations(m Manifest, r Receipt) []Deviation {
	keys := make([]string, 0, len(m.Data.GenerationParameters))
	for k := range m.Data.GenerationParameters {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := []Deviation{}
	for _, k := range keys {
		want, _ := weightsbundle.Canonical(m.Data.GenerationParameters[k])
		got, _ := weightsbundle.Canonical(r.ActualParameters[k])
		if string(want) != string(got) {
			out = append(out, Deviation{Parameter: k, Requested: m.Data.GenerationParameters[k], Actual: r.ActualParameters[k]})
		}
	}
	requested := make([]string, 0, len(m.Exports))
	for _, e := range m.Exports {
		requested = append(requested, e.CohortID)
	}
	sort.Strings(requested)
	actual := sortedCopy(r.ActualCohorts)
	if strings.Join(requested, "\x00") != strings.Join(actual, "\x00") {
		out = append(out, Deviation{Parameter: "cohorts", Requested: requested, Actual: actual})
	}
	return out
}

// VerifyReceipt checks a receipt against the manifest it claims. Any
// difference between requested and actual must be listed in Deviations; a
// receipt that claims improvement, or that trained outside the selection,
// is refused. A status other than "completed" is a recorded failure, not a
// candidate.
func VerifyReceipt(m Manifest, r Receipt) error {
	pin, err := m.Pin()
	if err != nil {
		return err
	}
	if r.Schema != 1 || r.Kind != KindReceipt {
		return refuse("receipt kind")
	}
	if r.ManifestPin != pin {
		return refuse("receipt manifest pin %s does not match %s", r.ManifestPin, pin)
	}
	if !r.CandidateOnly || r.ImprovementClaimed {
		return refuse("a training receipt is candidate-only and never claims improvement")
	}
	if r.EvaluationPerformedBy != "" {
		return refuse("evaluation is a separate record, not part of the receipt")
	}
	if r.Status != StatusCompleted && r.Status != StatusFailed && r.Status != StatusRefused {
		return refuse("receipt status %q is not completed, failed, or refused", r.Status)
	}
	if r.Executor.ID == "" {
		return refuse("receipt names no executor")
	}
	if !bounded(r.Executor.ID) || !bounded(r.Executor.AttestationID) || !bounded(r.Executor.Host) {
		return refuse("executor fields exceed %d bytes", MaxReceiptString)
	}
	if len(r.RuntimeIdentities) == 0 {
		return refuse("receipt names no runtime identities")
	}
	if len(r.RuntimeIdentities) > MaxRuntimeIdentities {
		return refuse("receipt names more than %d runtime identities", MaxRuntimeIdentities)
	}
	for k, v := range r.RuntimeIdentities {
		if k == "" || !bounded(k) || !bounded(v) {
			return refuse("runtime identity %q exceeds %d bytes or is empty", k, MaxReceiptString)
		}
	}
	if r.HeldOutExcludedCount < 0 || r.TrainedExampleCount < 0 {
		return refuse("negative example count")
	}
	wantParams, _ := weightsbundle.Canonical(m.Data.GenerationParameters)
	gotParams, _ := weightsbundle.Canonical(r.RequestedParameters)
	if string(wantParams) != string(gotParams) {
		return refuse("requested parameters differ from the manifest")
	}
	requested := make([]string, 0, len(m.Exports))
	for _, e := range m.Exports {
		requested = append(requested, e.CohortID)
	}
	sort.Strings(requested)
	if strings.Join(requested, "\x00") != strings.Join(sortedCopy(r.RequestedCohorts), "\x00") {
		return refuse("requested cohorts differ from the manifest")
	}
	for _, c := range r.ActualCohorts {
		if !contains(requested, c) {
			return refuse("trained on cohort %q outside the selection", c)
		}
	}
	teacher, _ := m.Teacher.chosen()
	base, _ := m.Base.chosen()
	if r.RequestedTeacher != teacher.Identity || r.RequestedBase != base.Identity {
		return refuse("requested teacher or base differs from the manifest's choice")
	}
	if r.ActualTeacher != teacher.Identity {
		return refuse("actual teacher %s/%s is not the chosen teacher", r.ActualTeacher.Provider, r.ActualTeacher.Model)
	}
	if r.ActualBase != base.Identity {
		return refuse("actual base %s/%s is not the chosen base", r.ActualBase.Provider, r.ActualBase.Model)
	}
	// Actual parameters are the requested ones, each possibly changed:
	// a key the request never had is a claim, not a parameter.
	for k := range r.ActualParameters {
		if _, ok := m.Data.GenerationParameters[k]; !ok {
			return refuse("actual parameter %q was never requested", k)
		}
	}
	if len(r.ActualParameters) != len(m.Data.GenerationParameters) {
		return refuse("actual parameters omit a requested parameter; every requested parameter has an actual value")
	}
	// Deviations are exactly the recomputed differences, no more, no less.
	expected := ExpectedDeviations(m, r)
	wantDev, _ := weightsbundle.Canonical(expected)
	recorded := r.Deviations
	if recorded == nil {
		recorded = []Deviation{}
	}
	gotDev, _ := weightsbundle.Canonical(recorded)
	if string(wantDev) != string(gotDev) {
		if len(expected) > 0 && len(r.Deviations) == 0 {
			return refuse("actual run deviated from the request without a recorded deviation")
		}
		return refuse("deviations must list exactly the differences between the request and the run: expected %s", wantDev)
	}
	if err := checkAuthorizedExports(m, r); err != nil {
		return err
	}
	if r.Status != StatusCompleted {
		if r.BundleDigest != "" {
			return refuse("a %s run must not report a bundle", r.Status)
		}
		if r.TeacherPredictionsID != "" {
			return refuse("a %s run must not report teacher predictions", r.Status)
		}
		return nil
	}
	if !validDigest(r.BundleDigest) {
		return refuse("completed run without a bundle digest")
	}
	if r.TeacherPredictionsID == "" {
		return refuse("completed run without recorded teacher predictions")
	}
	if !validDigest(r.TeacherPredictionsID) {
		return refuse("teacher predictions id is not a content address")
	}
	if r.TrainedExampleCount <= 0 {
		return refuse("completed run trained no examples")
	}
	return nil
}

// checkAuthorizedExports requires the receipt's export provenance to name
// exactly the cohorts the run consumed, each at the manifest's pin and
// inside the task and label-policy selection, with bounded identities.
func checkAuthorizedExports(m Manifest, r Receipt) error {
	seen := map[string]bool{}
	for _, e := range r.AuthorizedExports {
		pin, ok := m.Export(e.CohortID)
		if !ok {
			return refuse("authorized export names cohort %q, which the manifest does not admit", e.CohortID)
		}
		if pin.ManifestSHA256 != e.ManifestSHA256 {
			return refuse("authorized export for cohort %q is at pin %s, not the manifest's %s", e.CohortID, e.ManifestSHA256, pin.ManifestSHA256)
		}
		task, ok := m.Task(e.TaskID)
		if !ok || !contains(task.CohortIDs, e.CohortID) || !contains(task.LabelPolicyIDs, e.LabelPolicyID) {
			return refuse("authorized export for cohort %q names task %q and label policy %q outside the selection", e.CohortID, e.TaskID, e.LabelPolicyID)
		}
		if seen[e.CohortID] {
			return refuse("authorized export for cohort %q listed twice", e.CohortID)
		}
		seen[e.CohortID] = true
		for _, s := range []string{e.CohortSHA256, e.AuthorityRevisionID, e.InventoryID, e.TrainingPurposeID, e.DatasetSHA256, e.ProvenanceMode, e.CheckpointOverlap} {
			if !bounded(s) {
				return refuse("authorized export for cohort %q carries an oversized identity", e.CohortID)
			}
		}
		if e.DatasetBytes < 0 {
			return refuse("authorized export for cohort %q reports negative dataset bytes", e.CohortID)
		}
	}
	for _, c := range r.ActualCohorts {
		if !seen[c] {
			return refuse("run consumed cohort %q without an authorized export entry", c)
		}
	}
	if len(seen) != len(r.ActualCohorts) {
		return refuse("authorized exports name cohorts the run did not consume")
	}
	return nil
}

// ID returns the content address of a verified receipt.
func (r Receipt) ID() (string, error) { return weightsbundle.Digest(r) }

// Prediction is one teacher output. It is a prediction with provenance,
// never an observation or a label; the kind is fixed so a downstream export
// cannot mistake it for assessment evidence.
type Prediction struct {
	ExampleID string         `json:"example_id"`
	Kind      string         `json:"kind"`
	Teacher   string         `json:"teacher"`
	Output    map[string]any `json:"output"`
}

// PredictionKind is the only kind a teacher output may carry.
const PredictionKind = "prediction"

// PredictionSet is the sealed teacher output for a job.
type PredictionSet struct {
	Schema        int          `json:"schema"`
	Kind          string       `json:"kind"`
	ManifestPin   string       `json:"manifest_pin"`
	Teacher       string       `json:"teacher"`
	TeacherEgress string       `json:"teacher_egress"`
	PromptVersion string       `json:"prompt_version"`
	Predictions   []Prediction `json:"predictions"`
}

// NewPredictionSet validates teacher outputs against the manifest and the
// job's admitted exports: the teacher is the chosen one, its egress posture
// is the recorded one, every output is a prediction, every example is in an
// admitted export, no held-out example was shown to the teacher, and the set
// is not empty.
func NewPredictionSet(m Manifest, exports []ExportDescriptor, predictions []Prediction) (PredictionSet, error) {
	pin, err := m.Pin()
	if err != nil {
		return PredictionSet{}, err
	}
	if len(predictions) == 0 {
		return PredictionSet{}, refuse("teacher produced no predictions")
	}
	admitted := map[string]bool{}
	for _, e := range exports {
		if err := AuthorizeExport(m, e); err != nil {
			return PredictionSet{}, err
		}
		for _, id := range e.ExampleIDs {
			admitted[id] = true
		}
	}
	teacher, _ := m.Teacher.chosen()
	seen := map[string]bool{}
	for _, p := range predictions {
		if p.Kind != PredictionKind {
			return PredictionSet{}, refuse("teacher output for %q recorded as %q; teacher outputs are predictions", p.ExampleID, p.Kind)
		}
		if p.Teacher != teacher.Alias {
			return PredictionSet{}, refuse("output for %q from %q, not the chosen teacher %q", p.ExampleID, p.Teacher, teacher.Alias)
		}
		if p.ExampleID == "" || seen[p.ExampleID] {
			return PredictionSet{}, refuse("example %q empty or predicted twice", p.ExampleID)
		}
		seen[p.ExampleID] = true
		if m.HeldOut(p.ExampleID) {
			return PredictionSet{}, refuse("held-out example %q was shown to the teacher", p.ExampleID)
		}
		if !admitted[p.ExampleID] {
			return PredictionSet{}, refuse("example %q is in no admitted export", p.ExampleID)
		}
		if p.Output == nil {
			return PredictionSet{}, refuse("prediction for %q has no output", p.ExampleID)
		}
	}
	return PredictionSet{Schema: 1, Kind: KindPredictions, ManifestPin: pin, Teacher: teacher.Alias, TeacherEgress: teacher.Identity.Egress, PromptVersion: m.Data.PromptVersion, Predictions: predictions}, nil
}

// ID returns the content address of a prediction set.
func (s PredictionSet) ID() (string, error) { return weightsbundle.Digest(s) }

// Entry is one model's result on a shared picture.
type Entry struct {
	Role        string             `json:"role"`
	Alias       string             `json:"alias"`
	ManifestPin string             `json:"manifest_pin,omitempty"`
	ReleaseID   string             `json:"release_id,omitempty"`
	Metrics     map[string]float64 `json:"metrics"`
}

// Comparison roles.
const (
	EntryStudent   = "student"
	EntryTeacher   = "teacher"
	EntryIncumbent = "incumbent"
)

// TeacherRef names a compared teacher by alias and by exact identity, so
// two manifests that alias different models the same way are never
// conflated.
type TeacherRef struct {
	Alias    string        `json:"alias"`
	Identity ModelIdentity `json:"identity"`
}

// Comparison evaluates students, their teachers, and the incumbent on the
// same picture, so an extraction change and a model change stay
// distinguishable. Every entry shares the picture digest; every student
// names its teacher.
type Comparison struct {
	Schema        int          `json:"schema"`
	Kind          string       `json:"kind"`
	PictureDigest string       `json:"picture_digest"`
	EvaluationID  string       `json:"evaluation_id"`
	Entries       []Entry      `json:"entries"`
	Teachers      []TeacherRef `json:"teachers"`
}

// NewComparison builds and validates a comparison record. Each student
// manifest's chosen teacher must appear as a teacher entry; the incumbent is
// required so a student is never compared only against its own teacher.
func NewComparison(pictureDigest, evaluationID string, students []Manifest, entries []Entry) (Comparison, error) {
	if !validDigest(pictureDigest) || evaluationID == "" {
		return Comparison{}, refuse("comparison needs a picture digest and an evaluation id")
	}
	if len(students) == 0 {
		return Comparison{}, refuse("comparison needs at least one student")
	}
	pins := map[string]string{}
	teachers := map[string]ModelIdentity{}
	for _, s := range students {
		pin, err := s.Pin()
		if err != nil {
			return Comparison{}, err
		}
		teacher, _ := s.Teacher.chosen()
		pins[pin] = teacher.Alias
		if prior, ok := teachers[teacher.Alias]; ok && prior != teacher.Identity {
			return Comparison{}, refuse("teacher alias %q names two identities (%s/%s and %s/%s); aliases must name one model", teacher.Alias, prior.Provider, prior.Model, teacher.Identity.Provider, teacher.Identity.Model)
		}
		teachers[teacher.Alias] = teacher.Identity
	}
	// Students on the same data selection are comparable; refuse mixed data.
	first, _ := weightsbundle.Canonical(students[0].Data)
	for _, s := range students[1:] {
		other, _ := weightsbundle.Canonical(s.Data)
		if string(other) != string(first) {
			return Comparison{}, refuse("students were distilled from different data selections and are not comparable on one picture")
		}
	}
	roles := map[string]int{}
	seenStudents := map[string]bool{}
	seenTeachers := map[string]bool{}
	for _, e := range entries {
		if len(e.Metrics) == 0 {
			return Comparison{}, refuse("entry %q has no metrics", e.Alias)
		}
		for k, v := range e.Metrics {
			if k == "" || !finite(v) {
				return Comparison{}, refuse("entry %q metric %q is not finite", e.Alias, k)
			}
		}
		roles[e.Role]++
		switch e.Role {
		case EntryStudent:
			if _, ok := pins[e.ManifestPin]; !ok {
				return Comparison{}, refuse("student entry %q names no compared manifest", e.Alias)
			}
			seenStudents[e.ManifestPin] = true
		case EntryTeacher:
			if _, ok := teachers[e.Alias]; !ok {
				return Comparison{}, refuse("teacher entry %q is not a teacher of any compared student", e.Alias)
			}
			seenTeachers[e.Alias] = true
		case EntryIncumbent:
			if e.ReleaseID == "" {
				return Comparison{}, refuse("incumbent entry names no release")
			}
		default:
			return Comparison{}, refuse("entry role %q", e.Role)
		}
	}
	if roles[EntryIncumbent] != 1 {
		return Comparison{}, refuse("comparison needs exactly one incumbent")
	}
	for pin := range pins {
		if !seenStudents[pin] {
			return Comparison{}, refuse("student %s has no entry", pin)
		}
	}
	for t := range teachers {
		if !seenTeachers[t] {
			return Comparison{}, refuse("teacher %q has no entry on the picture", t)
		}
	}
	names := make([]TeacherRef, 0, len(teachers))
	for alias, id := range teachers {
		names = append(names, TeacherRef{Alias: alias, Identity: id})
	}
	sort.Slice(names, func(i, j int) bool { return names[i].Alias < names[j].Alias })
	return Comparison{Schema: 1, Kind: KindComparison, PictureDigest: pictureDigest, EvaluationID: evaluationID, Entries: entries, Teachers: names}, nil
}

// ID returns the content address of a comparison.
func (c Comparison) ID() (string, error) { return weightsbundle.Digest(c) }

// PromotionScope returns the task ids a candidate distilled under m may be
// promoted for. A release records them; a promotion for any other task is
// refused by weightsbundle.Release.ServesTask.
func PromotionScope(m Manifest) []string { return m.TaskIDs() }

func contains(values []string, v string) bool {
	for _, x := range values {
		if x == v {
			return true
		}
	}
	return false
}

func sortedCopy(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}

// CheckCandidateScope refuses a candidate record whose recorded task scope
// or training manifest pin is not the manifest's: a student trained on one
// selection cannot be promoted for a task outside it, and its release must
// bind the pin that produced it.
func CheckCandidateScope(m Manifest, candidate weightsbundle.Record) error {
	pin, err := m.Pin()
	if err != nil {
		return err
	}
	if got, _ := candidate["training_manifest_sha256"].(string); got != pin {
		return refuse("candidate training manifest %q is not the distillation manifest pin %s", got, pin)
	}
	scope := m.TaskIDs()
	tasks, _ := candidate["task_ids"].([]any)
	if len(tasks) == 0 {
		return refuse("candidate records no task scope")
	}
	for _, t := range tasks {
		s, _ := t.(string)
		if !contains(scope, s) {
			return refuse("candidate task %q is outside the selection %v", s, scope)
		}
	}
	return nil
}
