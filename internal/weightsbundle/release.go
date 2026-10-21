// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package weightsbundle

import (
	"errors"
	"fmt"
	"sort"
)

// Record kinds shared with experiments/decision-learning/learning.py.
const (
	KindCandidate         = "candidate"
	KindEvaluation        = "promotion_record"
	KindArtifact          = "candidate_artifact"
	KindPromotionApproval = "promotion_approval"
	KindRollbackApproval  = "rollback_approval"
	KindRelease           = "model_release"
	// KindEvaluationPolicy is the policy a run's evaluation is measured
	// under, sealed and bound to the run's manifest before its receipt
	// exists.
	KindEvaluationPolicy = "evaluation_policy"
)

// Release states.
const (
	StateActive = "active"
)

// Eligibility values recorded against a release by lineage events. A release
// is born eligible; withdrawal of a source its dataset manifest included
// marks it ineligible for new use. No unlearning is claimed.
const (
	EligibilityEligible          = "eligible"
	EligibilityWithdrawnSource   = "ineligible_withdrawn_source"
	WithdrawalLimitation         = "Withdrawal marks the release ineligible for new use; it does not claim removal of the source's influence from trained weights."
	PromotionLimitation          = "Structural release record; serving pointer mutation is external."
	RollbackLimitation           = "Structural rollback record; serving pointer mutation is external."
	ArtifactLimitationOneFile    = "One-file bundle: the weights file is the whole artifact."
	errCandidateLineage          = "candidate lineage mismatch"
	errArtifactEvaluationLineage = "artifact evaluation lineage mismatch"
)

// ErrRefused reports a promotion or rollback the records do not permit. The
// message is the verbatim reason a console must show.
var ErrRefused = errors.New("refused")

func refuse(reason string) error { return fmt.Errorf("%w: %s", ErrRefused, reason) }

// Release is the sealed model_release record. Field names match the Python
// prototype so both sides seal the same identity.
type Release struct {
	Schema                 int      `json:"schema"`
	Kind                   string   `json:"kind"`
	ID                     string   `json:"id,omitempty"`
	State                  string   `json:"state"`
	CandidateID            string   `json:"candidate_id"`
	ArtifactID             string   `json:"artifact_id"`
	BundleDigest           *string  `json:"bundle_digest"`
	EvaluationID           string   `json:"evaluation_id"`
	DatasetID              string   `json:"dataset_id"`
	DatasetManifestSHA256  *string  `json:"dataset_manifest_sha256"`
	TrainingManifestSHA256 *string  `json:"training_manifest_sha256"`
	TaskIDs                []string `json:"task_ids"`
	EvaluationPolicyID     string   `json:"evaluation_policy_id"`
	LedgerID               string   `json:"ledger_id"`
	PredecessorReleaseID   *string  `json:"predecessor_release_id"`
	RollbackOf             *string  `json:"rollback_of"`
	ApprovalID             string   `json:"approval_id"`
	Limitation             string   `json:"limitation"`
}

// Verify checks the release identity.
func (r Release) Verify() error { return VerifyStruct(r, KindRelease) }

// Record returns the release as a generic sealed record.
func (r Release) Record() (Record, error) {
	if err := r.Verify(); err != nil {
		return nil, err
	}
	return roundTrip(r)
}

// ReleaseFromRecord decodes and verifies a model_release record.
func ReleaseFromRecord(record Record) (Release, error) {
	if err := VerifyRecord(record, KindRelease); err != nil {
		return Release{}, err
	}
	var r Release
	if err := decodeInto(record, &r); err != nil {
		return Release{}, fmt.Errorf("invalid %s identity", KindRelease)
	}
	if r.TaskIDs == nil {
		r.TaskIDs = []string{}
	}
	if err := r.Verify(); err != nil {
		return Release{}, err
	}
	return r, nil
}

func seal(r Release) (Release, error) {
	r.Schema, r.Kind, r.ID = 1, KindRelease, ""
	if r.TaskIDs == nil {
		r.TaskIDs = []string{}
	}
	id, err := SealStruct(r)
	if err != nil {
		return Release{}, err
	}
	r.ID = id
	return r, nil
}

// NewArtifact seals a candidate_artifact that binds a verified bundle to a
// candidate and its evaluation. modelDigest is the weights file digest, which
// keeps the one-file SVM artifact's model_digest meaning intact.
func NewArtifact(candidateID, evaluationID, datasetID string, m Manifest, runtimeDigest string) (Record, error) {
	bundleDigest, err := m.Digest()
	if err != nil {
		return nil, err
	}
	weights, _ := m.File(RoleWeights)
	if candidateID == "" || evaluationID == "" || datasetID == "" || runtimeDigest == "" {
		return nil, refuse("artifact requires candidate, evaluation, dataset, and runtime identities")
	}
	files := make([]any, 0, len(m.Files))
	for _, f := range m.Files {
		files = append(files, map[string]any{"path": f.Path, "role": f.Role, "size": f.Size, "sha256": f.SHA256})
	}
	return Seal(KindArtifact, map[string]any{
		"candidate_id": candidateID, "evaluation_id": evaluationID, "dataset_id": datasetID,
		"model_digest": weights.SHA256, "runtime_digest": runtimeDigest,
		"bundle_digest": bundleDigest, "bundle_files": files,
	})
}

// NewPromotionApproval seals an approval bound to one candidate and one
// evaluation. Sealing records the binding; it does not authenticate owner.
func NewPromotionApproval(candidateID, evaluationID, owner string) (Record, error) {
	if owner == "" {
		return nil, refuse("promotion approval requires an owner")
	}
	return Seal(KindPromotionApproval, map[string]any{"candidate_id": candidateID, "evaluation_id": evaluationID, "owner": owner, "approved": true})
}

// NewRollbackApproval seals an approval bound to the active and prior release.
func NewRollbackApproval(activeReleaseID, priorReleaseID, owner string) (Record, error) {
	if owner == "" {
		return nil, refuse("rollback approval requires an owner")
	}
	return Seal(KindRollbackApproval, map[string]any{"active_release_id": activeReleaseID, "prior_release_id": priorReleaseID, "owner": owner, "approved": true})
}

// ArtifactManifest reconstructs the bundle manifest an artifact recorded.
func ArtifactManifest(artifact Record) (Manifest, error) {
	if err := VerifyRecord(artifact, KindArtifact); err != nil {
		return Manifest{}, err
	}
	files, _ := artifact["bundle_files"].([]any)
	m := Manifest{Schema: 1, Kind: Kind}
	for _, item := range files {
		obj, _ := item.(map[string]any)
		size, err := numberInt64(obj["size"])
		if err != nil {
			return Manifest{}, fmt.Errorf("%w: artifact bundle_files", ErrInvalidManifest)
		}
		m.Files = append(m.Files, File{Path: stringField(obj, "path"), Role: stringField(obj, "role"), Size: size, SHA256: stringField(obj, "sha256")})
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	digest, _ := m.Digest()
	if digest != stringField(artifact, "bundle_digest") {
		return Manifest{}, refuse("artifact bundle digest does not match its files")
	}
	return m, nil
}

// PromotionInput carries the sealed records a promotion binds.
type PromotionInput struct {
	Candidate   Record
	Evaluation  Record
	Artifact    Record
	Approval    Record
	Predecessor *Release
}

// Promote performs the structural promotion check from learning.py and
// seals an active release bound to the bundle digest, the candidate's dataset
// manifest pin and training manifest, the evaluation, and the approval. It
// mutates nothing; Registry.Promote supplies durability and the single
// serving pointer.
func Promote(in PromotionInput) (Release, error) {
	c, e, a, ap := in.Candidate, in.Evaluation, in.Artifact, in.Approval
	for _, check := range []struct {
		r    Record
		kind string
	}{{c, KindCandidate}, {e, KindEvaluation}, {a, KindArtifact}, {ap, KindPromotionApproval}} {
		if err := VerifyRecord(check.r, check.kind); err != nil {
			return Release{}, err
		}
	}
	candidateID := stringField(c, "id")
	if stringField(e, "candidate_id") != candidateID || stringField(e, "dataset_id") != stringField(c, "dataset_id") || stringField(e, "evaluation_policy_id") != stringField(c, "evaluation_policy_id") || stringField(a, "candidate_id") != candidateID {
		return Release{}, refuse(errCandidateLineage)
	}
	if stringField(a, "evaluation_id") != stringField(e, "id") || stringField(a, "dataset_id") != stringField(c, "dataset_id") {
		return Release{}, refuse(errArtifactEvaluationLineage)
	}
	if stringField(a, "runtime_digest") == "" || stringField(a, "model_digest") == "" || stringField(a, "model_digest") != stringField(c, "model_digest") {
		return Release{}, refuse("artifact model mismatch")
	}
	var bundleDigest *string
	if _, ok := a["bundle_digest"]; ok {
		m, err := ArtifactManifest(a)
		if err != nil {
			return Release{}, err
		}
		weights, _ := m.File(RoleWeights)
		if weights.SHA256 != stringField(a, "model_digest") {
			return Release{}, refuse("artifact weights digest does not match model digest")
		}
		d := stringField(a, "bundle_digest")
		bundleDigest = &d
	}
	if stringField(e, "disposition") != "shadow_candidate" || hasReasons(e) {
		return Release{}, refuse("candidate did not pass shadow evaluation")
	}
	if !isTrue(ap["approved"]) || stringField(ap, "owner") == "" || stringField(ap, "candidate_id") != candidateID || stringField(ap, "evaluation_id") != stringField(e, "id") {
		return Release{}, refuse("promotion approval is not bound")
	}
	tasks := candidateTasks(c)
	var predecessorID *string
	if in.Predecessor != nil {
		p := *in.Predecessor
		if err := p.Verify(); err != nil {
			return Release{}, err
		}
		if p.State != StateActive || p.DatasetID != stringField(c, "dataset_id") || p.EvaluationPolicyID != stringField(c, "evaluation_policy_id") || p.LedgerID != stringField(e, "ledger_id") {
			return Release{}, refuse("invalid predecessor release")
		}
		id := p.ID
		predecessorID = &id
	}
	return seal(Release{
		State: StateActive, CandidateID: candidateID, ArtifactID: stringField(a, "id"), BundleDigest: bundleDigest,
		EvaluationID: stringField(e, "id"), DatasetID: stringField(c, "dataset_id"),
		DatasetManifestSHA256: optionalString(c, "dataset_manifest_sha256"), TrainingManifestSHA256: optionalString(c, "training_manifest_sha256"),
		TaskIDs: tasks, EvaluationPolicyID: stringField(c, "evaluation_policy_id"), LedgerID: stringField(e, "ledger_id"),
		PredecessorReleaseID: predecessorID, RollbackOf: nil, ApprovalID: stringField(ap, "id"), Limitation: PromotionLimitation,
	})
}

// Rollback seals a new active release that restores prior's exact bundle,
// artifact, and evaluation. It never rewrites active.
func Rollback(active, prior Release, approval Record) (Release, error) {
	if err := active.Verify(); err != nil {
		return Release{}, err
	}
	if err := prior.Verify(); err != nil {
		return Release{}, err
	}
	if err := VerifyRecord(approval, KindRollbackApproval); err != nil {
		return Release{}, err
	}
	if active.State != StateActive || prior.State != StateActive {
		return Release{}, refuse("rollback requires active releases")
	}
	if active.DatasetID != prior.DatasetID || active.EvaluationPolicyID != prior.EvaluationPolicyID || active.LedgerID != prior.LedgerID {
		return Release{}, refuse("rollback lineage mismatch")
	}
	if !isTrue(approval["approved"]) || stringField(approval, "owner") == "" || stringField(approval, "active_release_id") != active.ID || stringField(approval, "prior_release_id") != prior.ID {
		return Release{}, refuse("rollback approval is not bound")
	}
	activeID := active.ID
	return seal(Release{
		State: StateActive, CandidateID: prior.CandidateID, ArtifactID: prior.ArtifactID, BundleDigest: prior.BundleDigest,
		EvaluationID: prior.EvaluationID, DatasetID: prior.DatasetID, DatasetManifestSHA256: prior.DatasetManifestSHA256,
		TrainingManifestSHA256: prior.TrainingManifestSHA256, TaskIDs: append([]string{}, prior.TaskIDs...),
		EvaluationPolicyID: prior.EvaluationPolicyID, LedgerID: prior.LedgerID,
		PredecessorReleaseID: &activeID, RollbackOf: &activeID, ApprovalID: stringField(approval, "id"), Limitation: RollbackLimitation,
	})
}

func hasReasons(e Record) bool {
	switch v := e["reasons"].(type) {
	case nil:
		return false
	case []any:
		return len(v) != 0
	}
	return true
}

func optionalString(r Record, key string) *string {
	s, ok := r[key].(string)
	if !ok || s == "" {
		return nil
	}
	return &s
}

// candidateTasks reads the task scope a candidate was trained for. A
// candidate records task_ids (selection scope) or a single task_id; a
// candidate without either has an unbounded scope recorded as empty.
func candidateTasks(c Record) []string {
	tasks := []string{}
	switch v := c["task_ids"].(type) {
	case []any:
		for _, t := range v {
			if s, ok := t.(string); ok && s != "" {
				tasks = append(tasks, s)
			}
		}
	}
	if s := stringField(c, "task_id"); s != "" {
		tasks = append(tasks, s)
	}
	sort.Strings(tasks)
	return dedupe(tasks)
}

func dedupe(in []string) []string {
	out := in[:0]
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// ServesTask reports whether a release may serve task. A release with no
// recorded scope serves nothing beyond what its records bind, so the caller
// must have bound a task elsewhere; a scoped release serves only its tasks.
func (r Release) ServesTask(task string) bool {
	for _, t := range r.TaskIDs {
		if t == task {
			return true
		}
	}
	return false
}
