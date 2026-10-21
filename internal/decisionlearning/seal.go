// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionlearning

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/phrocker/shoal-oss/internal/distillation"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/weightsbundle"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	// MaxSealBundleFiles bounds the files a sealed bundle may carry.
	MaxSealBundleFiles = 16
	// MaxSealBundleBytes bounds the decoded bytes of all files together.
	MaxSealBundleBytes = 8 << 20
)

// ServingPredictor is a student runtime adapter that can answer the
// student's task for one feature vector. It is optional: a host without a
// runtime reports, and refuses to predict.
type ServingPredictor interface {
	PredictFeatures(ctx context.Context, features []float64) (map[string]any, error)
}

// SealCandidate turns a completed run into recorded candidate, evaluation,
// and artifact records in one step. The body carries the manifest pin, the
// bundle's files, and the evaluation policy declared for the run:
//
//	{"manifest_pin": "<64 hex>", "cohort_id": "<optional when the job has one>",
//	 "bundle": {"bundle.json": "<base64>", "model.json": "<base64>", "runtime.json": "<base64>"},
//	 "evaluation_policy": {"id","split","positive_label","minimum_recall","minimum_positives","minimum_families"}}
//
// The bundle must be the one the recorded receipt produced; it is verified
// in staging, published on the serving root without replacing anything, and
// the records are sealed from the recorded manifest, receipt, prediction set,
// and export, so a body cannot smuggle a claim the run did not make. The
// evaluation measures the student on the export's reserved split; its
// disposition is shadow_candidate or hold, never an authorization.
func (s *Service) SealCandidate(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var body struct {
		ManifestPin      string            `json:"manifest_pin"`
		CohortID         string            `json:"cohort_id"`
		Bundle           map[string]string `json:"bundle"`
		EvaluationPolicy json.RawMessage   `json:"evaluation_policy"`
	}
	if err := weightsbundle.StrictUnmarshal(raw, &body); err != nil {
		return nil, invalid("body: " + err.Error())
	}
	if !recordID.MatchString(body.ManifestPin) {
		return nil, invalid("manifest_pin must be a 64-hex content address")
	}
	if len(body.Bundle) == 0 || len(body.Bundle) > MaxSealBundleFiles {
		return nil, invalid(fmt.Sprintf("bundle must carry 1..%d files", MaxSealBundleFiles))
	}
	if len(body.EvaluationPolicy) == 0 {
		return nil, invalid("evaluation_policy is required: the policy is declared before the run's outputs are looked at")
	}
	policy, err := distillation.DecodeEvaluationPolicy(body.EvaluationPolicy)
	if err != nil {
		return nil, s.classified(err)
	}
	files := make(map[string][]byte, len(body.Bundle))
	total := 0
	for name, encoded := range body.Bundle {
		if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") || strings.ContainsAny(name, "/\\") {
			return nil, invalid("bundle file names must be plain file names")
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, invalid("bundle file " + name + " is not base64")
		}
		total += len(decoded)
		if total > MaxSealBundleBytes {
			return nil, invalid(fmt.Sprintf("bundle exceeds %d bytes", MaxSealBundleBytes))
		}
		files[name] = decoded
	}
	pin := body.ManifestPin
	manifestRecord, err := s.get(ctx, kindManifest, pin)
	if err != nil {
		return nil, s.classified(err)
	}
	encoded, _ := weightsbundle.Canonical(manifestRecord)
	manifest, err := distillation.Decode(encoded)
	if err != nil {
		return nil, s.classified(err)
	}
	receiptRecord, err := s.get(ctx, kindReceipt, pin)
	if err != nil {
		return nil, refused(fmt.Errorf("manifest %s has no recorded receipt; submit the run's receipt first", pin))
	}
	var receipt distillation.Receipt
	if err := decodeInto(receiptRecord, &receipt); err != nil {
		return nil, s.classified(err)
	}
	predictionsRecord, err := s.get(ctx, kindPredictions, pin)
	if err != nil {
		return nil, refused(fmt.Errorf("manifest %s has no recorded prediction set; submit the teacher's predictions first", pin))
	}
	var predictions distillation.PredictionSet
	if err := decodeInto(predictionsRecord, &predictions); err != nil {
		return nil, s.classified(err)
	}
	cohortID := body.CohortID
	if cohortID == "" {
		if len(manifest.Exports) != 1 {
			return nil, invalid("cohort_id is required when the job admits more than one cohort")
		}
		cohortID = manifest.Exports[0].CohortID
	}
	exportPin, ok := manifest.Export(cohortID)
	if !ok {
		return nil, refused(fmt.Errorf("cohort %q is not admitted by manifest %s", cohortID, pin))
	}
	exportManifest, err := s.getRaw(ctx, kindExportManifest, exportPin.ManifestSHA256)
	if err != nil {
		if shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			return nil, refused(fmt.Errorf("the export %s was recorded before manifest copies were kept; request it again before sealing", exportPin.ManifestSHA256))
		}
		return nil, s.classified(err)
	}
	if s.cfg.Exporter == nil {
		return nil, unavailable("no dataset exporter is mounted on this host; the dataset rows cannot be obtained for evaluation")
	}
	export, err := s.cfg.Exporter.Export(ctx, shoal.ID(cohortID))
	if err != nil {
		return nil, s.classified(err)
	}
	if err := os.MkdirAll(s.cfg.ServingRoot, 0o700); err != nil {
		return nil, s.classifyLogged("seal", err)
	}
	staging, err := os.MkdirTemp(s.cfg.ServingRoot, ".seal-")
	if err != nil {
		return nil, s.classifyLogged("seal", err)
	}
	defer os.RemoveAll(staging)
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(staging, name), content, 0o600); err != nil {
			return nil, s.classifyLogged("seal", err)
		}
	}
	bundle, digest, err := weightsbundle.Load(staging)
	if err != nil {
		return nil, refused(fmt.Errorf("bundle does not verify: %s", s.redactErr("seal", err)))
	}
	weightsFile, ok := bundle.File(weightsbundle.RoleWeights)
	if !ok {
		return nil, refused(errors.New("bundle has no weights file"))
	}
	sealed, err := distillation.BuildCandidateSet(distillation.CandidateInput{
		Manifest: manifest, Pin: pin, Receipt: receipt, Predictions: predictions, Bundle: bundle,
		Weights: files[weightsFile.Path], ExportManifest: exportManifest, Dataset: export.Dataset, Policy: policy,
	})
	if err != nil {
		return nil, s.classified(err)
	}
	dest := filepath.Join(s.cfg.ServingRoot, digest)
	if err := weightsbundle.PublishLocal(staging, dest); err != nil && !errors.Is(err, storage.ErrExists) {
		return nil, refused(fmt.Errorf("bundle %s could not be published on the serving root: %s", digest, s.redactErr("seal", err)))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range []struct {
		kind   string
		record weightsbundle.Record
	}{{kindCandidate, sealed.Candidate}, {kindArtifact, sealed.Artifact}, {kindEvaluation, sealed.Evaluation}} {
		if err := s.put(ctx, item.kind, str(item.record, "id"), item.record); err != nil && !errors.Is(err, storage.ErrExists) {
			return nil, s.classified(err)
		}
	}
	return marshal(map[string]any{
		"candidate": sealed.Candidate, "artifact": sealed.Artifact, "evaluation": sealed.Evaluation,
		"bundle_digest": digest, "disposition": sealed.Evaluation["disposition"], "reasons": sealed.Evaluation["reasons"],
		"disclaimer": Disclaimer,
	})
}

// decodeInto takes the typed view of a stored record, ignoring the identity
// the store sealed onto it.
func decodeInto(record weightsbundle.Record, into any) error {
	copied := make(weightsbundle.Record, len(record))
	for k, v := range record {
		if k != "id" {
			copied[k] = v
		}
	}
	encoded, err := weightsbundle.Canonical(copied)
	if err != nil {
		return err
	}
	return weightsbundle.StrictUnmarshal(encoded, into)
}

// Predict asks the configured student runtime to answer the student's task
// for one feature vector: {"features": [..]}. What answers is only the
// runtime; a host without one refuses rather than computing anything itself.
func (s *Service) Predict(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var body struct {
		Features []float64 `json:"features"`
	}
	if err := weightsbundle.StrictUnmarshal(raw, &body); err != nil {
		return nil, invalid("body: " + err.Error())
	}
	if len(body.Features) == 0 || len(body.Features) > distillation.MaxStudentFeatures {
		return nil, invalid(fmt.Sprintf("features must carry 1..%d numbers", distillation.MaxStudentFeatures))
	}
	predictor, ok := s.cfg.Serving.(ServingPredictor)
	if s.cfg.Serving == nil || !ok {
		return nil, unavailable("no student runtime is configured on this host; nothing answers for the active release")
	}
	out, err := predictor.PredictFeatures(ctx, body.Features)
	if err != nil {
		return nil, s.classifyLogged("predict", err)
	}
	return marshal(out)
}
