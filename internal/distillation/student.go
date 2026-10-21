// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package distillation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/phrocker/shoal-oss/internal/weightsbundle"
)

// A completed run is a bundle plus a receipt. Before the bundle can become a
// candidate the host needs three sealed records: the candidate (what was
// trained, on which export, under which manifest), the evaluation (how it did
// on the export's reserved split, under a policy declared before the run was
// looked at), and the artifact (the bundle's files bound to both). This file
// builds all three from the run's outputs so that the console, shoalctl, and
// the Python worker produce byte-identical records for the same inputs.

const (
	// StudentKind is the model.json kind the reference worker publishes.
	StudentKind = "linear-student"
	// MaxStudentFeatures bounds the feature width a student may declare.
	MaxStudentFeatures = 65536
	// MaxEvaluationRows bounds the dataset rows an evaluation reads.
	MaxEvaluationRows = 1 << 20
	// EvaluationLimitation is recorded on every evaluation: a reserved split
	// measured against adjudicated labels is not defect truth.
	EvaluationLimitation = "Adjudicated labels on a reserved split are not defect truth. No quality-parity or exclusion authorization; promotion grants shadow-candidate status only."
	// PermittedAction is the only action an evaluation may permit.
	PermittedAction = "full_review"
)

// LinearStudent is the student model the reference worker publishes as the
// bundle's weights file: a linear decision over a fixed-width feature vector.
type LinearStudent struct {
	Schema                  int       `json:"schema"`
	Kind                    string    `json:"kind"`
	Coefficients            []float64 `json:"coefficients"`
	Intercept               float64   `json:"intercept"`
	Threshold               float64   `json:"threshold"`
	Labels                  []string  `json:"labels"`
	TaskID                  string    `json:"task_id"`
	FeatureSchemaID         string    `json:"feature_schema_id"`
	DistillationManifestPin string    `json:"distillation_manifest_pin"`
	Teacher                 string    `json:"teacher"`
	Base                    string    `json:"base"`
	Epochs                  int       `json:"epochs"`
	BaseBundleDigest        string    `json:"base_bundle_digest,omitempty"`
}

// DecodeLinearStudent decodes and bounds a student weights file.
func DecodeLinearStudent(raw []byte) (LinearStudent, error) {
	var s LinearStudent
	if err := weightsbundle.StrictUnmarshal(raw, &s); err != nil {
		return LinearStudent{}, refuse("student weights: %v", err)
	}
	if s.Schema != 1 || s.Kind != StudentKind {
		return LinearStudent{}, refuse("student weights are not a schema-1 %s", StudentKind)
	}
	if len(s.Coefficients) == 0 || len(s.Coefficients) > MaxStudentFeatures {
		return LinearStudent{}, refuse("student declares %d coefficients; 1..%d allowed", len(s.Coefficients), MaxStudentFeatures)
	}
	for _, c := range s.Coefficients {
		if !finite(c) {
			return LinearStudent{}, refuse("student coefficients must be finite")
		}
	}
	if !finite(s.Intercept) || !finite(s.Threshold) {
		return LinearStudent{}, refuse("student intercept and threshold must be finite")
	}
	if len(s.Labels) != 2 || s.Labels[0] == "" || s.Labels[1] == "" || s.Labels[0] == s.Labels[1] {
		return LinearStudent{}, refuse("student must name two distinct labels")
	}
	if s.TaskID == "" || !validDigest(s.DistillationManifestPin) {
		return LinearStudent{}, refuse("student must name its task and distillation manifest pin")
	}
	return s, nil
}

// Predict applies the linear decision exactly as the student runtime does:
// the margin is the left-to-right dot product plus the intercept, and the
// positive label is chosen when the margin reaches the threshold.
func (s LinearStudent) Predict(features []float64) (string, float64, error) {
	if len(features) != len(s.Coefficients) {
		return "", 0, refuse("feature width %d does not match the student's %d", len(features), len(s.Coefficients))
	}
	margin := 0.0
	for i, w := range s.Coefficients {
		if !finite(features[i]) {
			return "", 0, refuse("features must be finite")
		}
		margin += w * features[i]
	}
	margin += s.Intercept
	if !finite(margin) {
		return "", 0, refuse("margin overflowed")
	}
	if margin >= s.Threshold {
		return s.Labels[1], margin, nil
	}
	return s.Labels[0], margin, nil
}

// EvaluationPolicy is declared before the run's outputs are looked at. It
// names the reserved split that is measured, the label that counts as a
// positive, and the gates a candidate must clear. A policy carries no loss
// target and authorizes no exclusion.
type EvaluationPolicy struct {
	ID               string  `json:"id"`
	Split            string  `json:"split"`
	PositiveLabel    string  `json:"positive_label"`
	MinimumRecall    float64 `json:"minimum_recall"`
	MinimumPositives int     `json:"minimum_positives"`
	MinimumFamilies  int     `json:"minimum_families"`
}

// DecodeEvaluationPolicy decodes and bounds a policy.
func DecodeEvaluationPolicy(raw []byte) (EvaluationPolicy, error) {
	var p EvaluationPolicy
	if err := weightsbundle.StrictUnmarshal(raw, &p); err != nil {
		return EvaluationPolicy{}, refuse("evaluation policy: %v", err)
	}
	return p, p.Validate()
}

// Validate refuses a policy that cannot gate anything.
func (p EvaluationPolicy) Validate() error {
	switch {
	case strings.TrimSpace(p.ID) == "" || len(p.ID) > MaxReceiptString:
		return refuse("evaluation policy needs an identity")
	case strings.TrimSpace(p.Split) == "" || strings.ContainsAny(p.Split, ": \t\n"):
		return refuse("evaluation policy must name one reserved split")
	case p.PositiveLabel == "":
		return refuse("evaluation policy must name the positive label")
	case !finite(p.MinimumRecall) || p.MinimumRecall <= 0 || p.MinimumRecall > 1:
		return refuse("minimum_recall must be in (0, 1]")
	case p.MinimumPositives < 1 || p.MinimumFamilies < 1:
		return refuse("minimum_positives and minimum_families must be at least 1")
	}
	return nil
}

type datasetRow struct {
	ID          string    `json:"id"`
	Features    []float64 `json:"features"`
	Label       *string   `json:"label"`
	LabelStatus string    `json:"label_status"`
	Split       string    `json:"split"`
}

type datasetDocument struct {
	Schema int          `json:"schema"`
	Kind   string       `json:"kind"`
	TaskID string       `json:"task_id"`
	Labels []string     `json:"labels"`
	Rows   []datasetRow `json:"rows"`
}

type exportRow struct {
	ID               string   `json:"id"`
	ExclusionReasons []string `json:"exclusion_reasons"`
	FamilyIDs        []string `json:"family_ids"`
	Sources          []struct {
		Digest string `json:"Digest"`
	} `json:"sources"`
}

// contentDigests are the identities of the source content behind a row.
// Two rows sharing one are the same content under two ids; two rows with
// equal feature vectors are not, since a coarse feature space maps distinct
// content to the same point.
func (r exportRow) contentDigests() []string {
	out := make([]string, 0, len(r.Sources))
	for _, s := range r.Sources {
		if s.Digest != "" {
			out = append(out, s.Digest)
		}
	}
	return out
}

type exportDocument struct {
	Schema        int         `json:"schema"`
	CohortID      string      `json:"cohort_id"`
	TaskID        string      `json:"task_id"`
	LabelPolicyID string      `json:"label_policy_id"`
	DatasetSHA256 string      `json:"dataset_sha256"`
	Rows          []exportRow `json:"rows"`
}

func decodeLoose(raw []byte, into any) error {
	// Exports carry many members this package does not interpret; only the
	// ones read here are typed. Decoding through the record codec refuses
	// duplicate keys at any depth before the typed view is taken.
	record, err := weightsbundle.DecodeRecord(raw)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, into)
}

// Evaluation is the measured outcome of a candidate on the export's reserved
// split, before sealing.
type Evaluation struct {
	Disposition string
	Reasons     []string
	Metrics     []map[string]any
	Comparisons []map[string]any
	Families    int
	Split       string
	TestIDs     []string
}

// Evaluate measures a student on the rows the export reserved for the
// policy's split, against the export's adjudicated labels, and compares it
// with the recorded teacher on the rows the teacher labelled. Every gate the
// policy declares is checked; every leak between training and evaluation
// membership is a reason. Completion is not improvement: the result is a
// disposition of shadow_candidate or hold, never an authorization.
func Evaluate(student LinearStudent, dataset, exportManifest []byte, trainingIDs []string, predictions *PredictionSet, policy EvaluationPolicy) (Evaluation, error) {
	if err := policy.Validate(); err != nil {
		return Evaluation{}, err
	}
	var data datasetDocument
	if err := decodeLoose(dataset, &data); err != nil {
		return Evaluation{}, refuse("dataset: %v", err)
	}
	var export exportDocument
	if err := decodeLoose(exportManifest, &export); err != nil {
		return Evaluation{}, refuse("export manifest: %v", err)
	}
	if data.Schema != 1 || export.Schema != 1 || len(data.Rows) == 0 || len(data.Rows) > MaxEvaluationRows || len(export.Rows) > MaxEvaluationRows {
		return Evaluation{}, refuse("dataset and export manifest must be schema 1 with bounded rows")
	}
	if sha256Hex(dataset) != export.DatasetSHA256 {
		return Evaluation{}, refuse("dataset bytes do not hash to the export manifest's dataset_sha256")
	}
	if data.TaskID != export.TaskID || student.TaskID != data.TaskID {
		return Evaluation{}, refuse("student task %q, dataset task %q, and export task %q must agree", student.TaskID, data.TaskID, export.TaskID)
	}
	if len(data.Labels) != 2 || data.Labels[0] != student.Labels[0] || data.Labels[1] != student.Labels[1] {
		return Evaluation{}, refuse("student labels %v are not the dataset's %v", student.Labels, data.Labels)
	}
	if policy.PositiveLabel != data.Labels[0] && policy.PositiveLabel != data.Labels[1] {
		return Evaluation{}, refuse("policy positive label %q is not a dataset label", policy.PositiveLabel)
	}
	rows := make(map[string]datasetRow, len(data.Rows))
	for _, r := range data.Rows {
		if r.ID == "" {
			return Evaluation{}, refuse("dataset row without an id")
		}
		if _, dup := rows[r.ID]; dup {
			return Evaluation{}, refuse("dataset row %q appears twice", r.ID)
		}
		rows[r.ID] = r
	}
	marker := "split:" + policy.Split
	var testIDs []string
	families := map[string]struct{}{}
	familyOf := map[string][]string{}
	contentOf := map[string][]string{}
	for _, r := range export.Rows {
		familyOf[r.ID] = r.FamilyIDs
		contentOf[r.ID] = r.contentDigests()
		for _, reason := range r.ExclusionReasons {
			if reason == marker {
				testIDs = append(testIDs, r.ID)
				for _, f := range r.FamilyIDs {
					families[f] = struct{}{}
				}
				break
			}
		}
	}
	sort.Strings(testIDs)
	if len(testIDs) == 0 {
		return Evaluation{}, refuse("the export reserved no rows for split %q", policy.Split)
	}
	training := map[string]struct{}{}
	for _, id := range trainingIDs {
		training[id] = struct{}{}
	}
	var reasons []string
	trainFamilies, trainContent := map[string]struct{}{}, map[string]struct{}{}
	for id := range training {
		for _, f := range familyOf[id] {
			trainFamilies[f] = struct{}{}
		}
		for _, d := range contentOf[id] {
			trainContent[d] = struct{}{}
		}
	}
	var leakedIDs, leakedFamilies, leakedContent []string
	for _, id := range testIDs {
		if _, ok := training[id]; ok {
			leakedIDs = append(leakedIDs, id)
		}
		for _, f := range familyOf[id] {
			if _, ok := trainFamilies[f]; ok {
				leakedFamilies = append(leakedFamilies, id)
				break
			}
		}
		for _, d := range contentOf[id] {
			if _, ok := trainContent[d]; ok {
				leakedContent = append(leakedContent, id)
				break
			}
		}
	}
	if len(leakedIDs) > 0 {
		reasons = append(reasons, fmt.Sprintf("training/evaluation membership overlap: %s", strings.Join(leakedIDs, ",")))
	}
	if len(leakedFamilies) > 0 {
		reasons = append(reasons, fmt.Sprintf("training/evaluation family overlap: %s", strings.Join(leakedFamilies, ",")))
	}
	if len(leakedContent) > 0 {
		reasons = append(reasons, fmt.Sprintf("training/evaluation content overlap: %s", strings.Join(leakedContent, ",")))
	}
	assessor := export.LabelPolicyID
	if assessor == "" {
		assessor = "labels"
	}
	positives, missed, unknownLowered := 0, []string{}, []string{}
	for _, id := range testIDs {
		r, ok := rows[id]
		if !ok {
			return Evaluation{}, refuse("reserved row %q is not in the dataset", id)
		}
		predicted, _, err := student.Predict(r.Features)
		if err != nil {
			return Evaluation{}, refuse("row %q: %v", id, err)
		}
		lowered := predicted != policy.PositiveLabel
		if r.LabelStatus != "verified" || r.Label == nil {
			if lowered {
				unknownLowered = append(unknownLowered, id)
			}
			continue
		}
		if *r.Label == policy.PositiveLabel {
			positives++
			if lowered {
				missed = append(missed, id)
			}
		}
	}
	metric := map[string]any{
		"assessor": assessor, "positives": positives, "missed_ids": missed,
		"unknown_lowered_ids": unknownLowered, "evaluated_ids": testIDs,
	}
	var recall any
	if positives > 0 {
		recall = float64(positives-len(missed)) / float64(positives)
	}
	metric["recall"] = recall
	if positives < policy.MinimumPositives {
		reasons = append(reasons, fmt.Sprintf("%s: sample gate failed: %d positives, %d required", assessor, positives, policy.MinimumPositives))
	}
	if recall == nil || recall.(float64) < policy.MinimumRecall {
		reasons = append(reasons, fmt.Sprintf("%s: quality gate failed: recall below %v", assessor, policy.MinimumRecall))
	}
	if len(unknownLowered) > 0 {
		reasons = append(reasons, fmt.Sprintf("%s: unresolved examples lowered: %s", assessor, strings.Join(unknownLowered, ",")))
	}
	if len(families) < policy.MinimumFamilies {
		reasons = append(reasons, fmt.Sprintf("insufficient independent families: %d, %d required", len(families), policy.MinimumFamilies))
	}
	var comparisons []map[string]any
	if predictions != nil {
		examples, agreed, disagreed := 0, 0, []string{}
		for _, p := range predictions.Predictions {
			r, ok := rows[p.ExampleID]
			if !ok {
				continue
			}
			teacherLabel, _ := p.Output["label"].(string)
			predicted, _, err := student.Predict(r.Features)
			if err != nil {
				return Evaluation{}, refuse("row %q: %v", p.ExampleID, err)
			}
			examples++
			if predicted == teacherLabel {
				agreed++
			} else {
				disagreed = append(disagreed, p.ExampleID)
			}
		}
		sort.Strings(disagreed)
		var agreement any
		if examples > 0 {
			agreement = float64(agreed) / float64(examples)
		}
		comparisons = append(comparisons, map[string]any{
			"teacher": predictions.Teacher, "teacher_egress": predictions.TeacherEgress, "examples": examples,
			"agreement": agreement, "disagreement_ids": disagreed,
		})
	}
	if reasons == nil {
		reasons = []string{}
	}
	disposition := "shadow_candidate"
	if len(reasons) > 0 {
		disposition = "hold"
	}
	return Evaluation{Disposition: disposition, Reasons: reasons, Metrics: []map[string]any{metric}, Comparisons: comparisons, Families: len(families), Split: policy.Split, TestIDs: testIDs}, nil
}

// CandidateInput is everything a run leaves behind plus the predeclared
// policy. Dataset and ExportManifest are the authorized export's bytes; the
// manifest bytes must hash to the pin the distillation manifest recorded.
type CandidateInput struct {
	Manifest       Manifest
	Pin            string
	Receipt        Receipt
	Predictions    PredictionSet
	Bundle         weightsbundle.Manifest
	Weights        []byte
	ExportManifest []byte
	Dataset        []byte
	Policy         EvaluationPolicy
}

// CandidateSet is the three sealed records the host records for a run.
type CandidateSet struct {
	Candidate  weightsbundle.Record `json:"candidate"`
	Evaluation weightsbundle.Record `json:"evaluation"`
	Artifact   weightsbundle.Record `json:"artifact"`
}

// BuildCandidateSet verifies that the run's outputs belong together and seals
// the candidate, evaluation, and artifact for them. It records what was
// measured; it decides nothing about promotion.
func BuildCandidateSet(in CandidateInput) (CandidateSet, error) {
	pin, err := in.Manifest.Pin()
	if err != nil {
		return CandidateSet{}, err
	}
	if pin != in.Pin {
		return CandidateSet{}, refuse("manifest pin %s is not the declared pin %s", pin, in.Pin)
	}
	if err := VerifyReceipt(in.Manifest, in.Receipt); err != nil {
		return CandidateSet{}, err
	}
	if in.Receipt.Status != StatusCompleted {
		return CandidateSet{}, refuse("the run is %q, not completed", in.Receipt.Status)
	}
	bundleDigest, err := in.Bundle.Digest()
	if err != nil {
		return CandidateSet{}, err
	}
	if in.Receipt.BundleDigest != bundleDigest {
		return CandidateSet{}, refuse("the receipt names bundle %s, not the bundle presented %s", in.Receipt.BundleDigest, bundleDigest)
	}
	if in.Predictions.ManifestPin != pin {
		return CandidateSet{}, refuse("prediction set belongs to manifest %s, not %s", in.Predictions.ManifestPin, pin)
	}
	predictionsID, err := weightsbundle.Digest(in.Predictions)
	if err != nil {
		return CandidateSet{}, err
	}
	if in.Receipt.TeacherPredictionsID != predictionsID {
		return CandidateSet{}, refuse("the receipt names prediction set %s, not the set presented %s", in.Receipt.TeacherPredictionsID, predictionsID)
	}
	weights, ok := in.Bundle.File(weightsbundle.RoleWeights)
	if !ok {
		return CandidateSet{}, refuse("bundle has no weights file")
	}
	if sha256Hex(in.Weights) != weights.SHA256 {
		return CandidateSet{}, refuse("weights bytes do not hash to the bundle's weights file")
	}
	runtime, ok := in.Bundle.File(weightsbundle.RoleRuntime)
	if !ok {
		return CandidateSet{}, refuse("bundle has no runtime file; the artifact needs a runtime identity")
	}
	student, err := DecodeLinearStudent(in.Weights)
	if err != nil {
		return CandidateSet{}, err
	}
	if student.DistillationManifestPin != pin {
		return CandidateSet{}, refuse("student was trained under manifest %s, not %s", student.DistillationManifestPin, pin)
	}
	if !contains(in.Manifest.TaskIDs(), student.TaskID) {
		return CandidateSet{}, refuse("student task %q is outside the selection %v", student.TaskID, in.Manifest.TaskIDs())
	}
	var export exportDocument
	if err := decodeLoose(in.ExportManifest, &export); err != nil {
		return CandidateSet{}, refuse("export manifest: %v", err)
	}
	exportPin := sha256Hex(in.ExportManifest)
	pinned, ok := in.Manifest.Export(export.CohortID)
	if !ok || pinned.ManifestSHA256 != exportPin {
		return CandidateSet{}, refuse("export %s for cohort %q is not the one the manifest pinned", exportPin, export.CohortID)
	}
	if !contains(in.Receipt.ActualCohorts, export.CohortID) {
		return CandidateSet{}, refuse("the run did not train on cohort %q", export.CohortID)
	}
	trainingIDs := make([]string, 0, len(in.Predictions.Predictions))
	for _, p := range in.Predictions.Predictions {
		trainingIDs = append(trainingIDs, p.ExampleID)
	}
	sort.Strings(trainingIDs)
	evaluation, err := Evaluate(student, in.Dataset, in.ExportManifest, trainingIDs, &in.Predictions, in.Policy)
	if err != nil {
		return CandidateSet{}, err
	}
	candidate, err := weightsbundle.Seal(weightsbundle.KindCandidate, map[string]any{
		"dataset_id": export.CohortID, "evaluation_policy_id": in.Policy.ID, "training_ids": trainingIDs,
		"model_digest": weights.SHA256, "policy_predeclared": true, "task_ids": in.Manifest.TaskIDs(),
		"dataset_manifest_sha256": exportPin, "training_manifest_sha256": pin,
		"teacher_predictions_id": predictionsID, "student_kind": student.Kind,
	})
	if err != nil {
		return CandidateSet{}, err
	}
	candidateID, _ := candidate["id"].(string)
	fields := map[string]any{
		"candidate_id": candidateID, "dataset_id": export.CohortID, "evaluation_policy_id": in.Policy.ID,
		"dataset_manifest_sha256": exportPin, "predictions_id": predictionsID, "split": evaluation.Split,
		"evaluated_ids": evaluation.TestIDs, "metrics": evaluation.Metrics, "comparisons": evaluation.Comparisons,
		"families": evaluation.Families, "reasons": evaluation.Reasons, "disposition": evaluation.Disposition,
		"permitted_action": PermittedAction, "optimization_enabled": false, "limitation": EvaluationLimitation,
	}
	if fields["comparisons"] == nil {
		fields["comparisons"] = []map[string]any{}
	}
	sealedEvaluation, err := weightsbundle.Seal(weightsbundle.KindEvaluation, fields)
	if err != nil {
		return CandidateSet{}, err
	}
	evaluationID, _ := sealedEvaluation["id"].(string)
	artifact, err := weightsbundle.NewArtifact(candidateID, evaluationID, export.CohortID, in.Bundle, runtime.SHA256)
	if err != nil {
		return CandidateSet{}, err
	}
	return CandidateSet{Candidate: candidate, Evaluation: sealedEvaluation, Artifact: artifact}, nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
