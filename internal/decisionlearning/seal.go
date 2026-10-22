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
// bundle's files, and, when the run trained on several cohorts, the one to
// evaluate on:
//
//	{"manifest_pin": "<64 hex>", "cohort_id": "<optional when the run trained on one>",
//	 "bundle": {"bundle.json": "<base64>", "model.json": "<base64>", "runtime.json": "<base64>"}}
//
// The bundle must be the one the recorded receipt produced, and is checked
// before anything else is fetched; it is verified in staging, published on
// the serving root without replacing anything, and the records are sealed
// from the recorded manifest, receipt, prediction set, the policy declared
// before the receipt, and every export the run consumed, so a body cannot
// smuggle a claim the run did not make. The evaluation measures the student
// on the chosen export's reserved split; its disposition is shadow_candidate
// or hold, never an authorization.
func (s *Service) SealCandidate(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var body struct {
		ManifestPin string            `json:"manifest_pin"`
		CohortID    string            `json:"cohort_id"`
		Bundle      map[string]string `json:"bundle"`
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
	run, err := s.recordedRun(ctx, pin)
	if err != nil {
		return nil, err
	}
	// The bundle is verified and matched to the receipt before any export is
	// fetched, so a body that is not the run's bundle costs the host nothing
	// beyond the staging write.
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
	cohortID, err := run.evaluationCohort(body.CohortID)
	if err != nil {
		return nil, err
	}
	sealed, digest, err := s.rebuild(ctx, run, staging, cohortID)
	if err != nil {
		return nil, err
	}
	// A held run's records are recorded, so the hold and its reasons are on
	// the host, but its bundle does not go on the serving root: nothing that
	// did not pass is placed where a candidate route could bind it.
	if sealed.Evaluation["disposition"] == "shadow_candidate" {
		dest := filepath.Join(s.cfg.ServingRoot, digest)
		if err := weightsbundle.PublishLocal(staging, dest); err != nil && !errors.Is(err, storage.ErrExists) {
			return nil, refused(fmt.Errorf("bundle %s could not be published on the serving root: %s", digest, s.redactErr("seal", err)))
		}
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
		"published": sealed.Evaluation["disposition"] == "shadow_candidate", "disclaimer": Disclaimer,
	})
}

// recordedRun is what the host holds for a completed run: the manifest, the
// receipt, the prediction set, and the policy declared before the receipt.
type recordedRun struct {
	pin         string
	manifest    distillation.Manifest
	receipt     distillation.Receipt
	predictions distillation.PredictionSet
	policy      weightsbundle.Record
}

// recordedRun loads the run recorded under a manifest pin. Each absence is a
// refusal naming what to submit first; every other store error is its own.
func (s *Service) recordedRun(ctx context.Context, pin string) (recordedRun, error) {
	if !recordID.MatchString(pin) {
		return recordedRun{}, invalid("manifest pin")
	}
	manifestRecord, err := s.get(ctx, kindManifest, pin)
	if err != nil {
		return recordedRun{}, s.classified(err)
	}
	encoded, _ := weightsbundle.Canonical(manifestRecord)
	manifest, err := distillation.Decode(encoded)
	if err != nil {
		return recordedRun{}, s.classified(err)
	}
	receiptRecord, err := s.get(ctx, kindReceipt, pin)
	if err != nil {
		if shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			return recordedRun{}, refused(fmt.Errorf("manifest %s has no recorded receipt; submit the run's receipt first", pin))
		}
		return recordedRun{}, s.classified(err)
	}
	var receipt distillation.Receipt
	if err := decodeInto(receiptRecord, &receipt); err != nil {
		return recordedRun{}, s.classified(err)
	}
	predictionsRecord, err := s.get(ctx, kindPredictions, pin)
	if err != nil {
		if shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			return recordedRun{}, refused(fmt.Errorf("manifest %s has no recorded prediction set; submit the teacher's predictions first", pin))
		}
		return recordedRun{}, s.classified(err)
	}
	var predictions distillation.PredictionSet
	if err := decodeInto(predictionsRecord, &predictions); err != nil {
		return recordedRun{}, s.classified(err)
	}
	policy, err := s.policyFor(ctx, pin)
	if err != nil {
		if shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			return recordedRun{}, refused(fmt.Errorf("manifest %s has no declared evaluation policy; the run cannot be judged under a policy chosen after its outputs exist", pin))
		}
		return recordedRun{}, s.classified(err)
	}
	if len(receipt.ActualCohorts) == 0 {
		return recordedRun{}, refused(fmt.Errorf("the recorded receipt for manifest %s names no cohorts", pin))
	}
	return recordedRun{pin: pin, manifest: manifest, receipt: receipt, predictions: predictions, policy: policy}, nil
}

// evaluationCohort resolves which of the run's cohorts is measured.
func (r recordedRun) evaluationCohort(requested string) (string, error) {
	if requested == "" {
		if len(r.receipt.ActualCohorts) != 1 {
			return "", invalid("cohort_id is required when the run trained on more than one cohort")
		}
		return r.receipt.ActualCohorts[0], nil
	}
	if !contains(r.receipt.ActualCohorts, requested) {
		return "", refused(fmt.Errorf("the run under manifest %s did not train on cohort %q", r.pin, requested))
	}
	return requested, nil
}

// rebuild seals the run's candidate, evaluation, and artifact from the
// host's records plus the bundle in bundleDir, which must be the one the
// receipt produced. It is the one way records come to exist here: the seal
// route seals from a staged upload, and the candidate and evaluation routes
// accept a record only when it is the one this rebuild produces from the
// bundle on the serving root.
func (s *Service) rebuild(ctx context.Context, run recordedRun, bundleDir, cohortID string) (distillation.CandidateSet, string, error) {
	bundle, digest, err := weightsbundle.Load(bundleDir)
	if err != nil {
		return distillation.CandidateSet{}, "", refused(fmt.Errorf("bundle does not verify: %s", s.redactErr("seal", err)))
	}
	if digest != run.receipt.BundleDigest {
		return distillation.CandidateSet{}, "", refused(fmt.Errorf("the recorded receipt for manifest %s produced bundle %s, not the bundle presented %s", run.pin, run.receipt.BundleDigest, digest))
	}
	weightsFile, ok := bundle.File(weightsbundle.RoleWeights)
	if !ok {
		return distillation.CandidateSet{}, "", refused(errors.New("bundle has no weights file"))
	}
	weights, err := os.ReadFile(filepath.Join(bundleDir, weightsFile.Path))
	if err != nil {
		return distillation.CandidateSet{}, "", s.classifyLogged("seal", err)
	}
	if s.cfg.Exporter == nil {
		return distillation.CandidateSet{}, "", unavailable("no dataset exporter is mounted on this host; the dataset rows cannot be obtained for evaluation")
	}
	// Every export the run consumed is presented, so a reserved row whose
	// family or content is behind a training row of another cohort is seen.
	var exports []distillation.ExportInput
	for _, cohort := range run.receipt.ActualCohorts {
		exportPin, ok := run.manifest.Export(cohort)
		if !ok {
			return distillation.CandidateSet{}, "", refused(fmt.Errorf("cohort %q is not admitted by manifest %s", cohort, run.pin))
		}
		exportManifest, err := s.getRaw(ctx, kindExportManifest, exportPin.ManifestSHA256)
		if err != nil {
			if shoal.IsErrorCode(err, shoal.ErrorNotFound) {
				return distillation.CandidateSet{}, "", refused(fmt.Errorf("the export %s was recorded before manifest copies were kept; request it again before sealing", exportPin.ManifestSHA256))
			}
			return distillation.CandidateSet{}, "", s.classified(err)
		}
		export, err := s.cfg.Exporter.Export(ctx, shoal.ID(cohort))
		if err != nil {
			return distillation.CandidateSet{}, "", s.classified(err)
		}
		exports = append(exports, distillation.ExportInput{Manifest: exportManifest, Dataset: export.Dataset})
	}
	sealed, err := distillation.BuildCandidateSet(distillation.CandidateInput{
		Manifest: run.manifest, Pin: run.pin, Receipt: run.receipt, Predictions: run.predictions, Bundle: bundle,
		Weights: weights, Exports: exports, EvaluationCohort: cohortID, Policy: run.policy,
	})
	if err != nil {
		return distillation.CandidateSet{}, "", s.classified(err)
	}
	return sealed, digest, nil
}

// rebuildFromServingRoot seals the run from the bundle its receipt names, as
// published on the serving root. A run whose bundle is not there (a held
// run, or one never sealed here) cannot be rebuilt, and nothing about it is
// accepted on a caller's word.
func (s *Service) rebuildFromServingRoot(ctx context.Context, pin, cohortID string) (distillation.CandidateSet, string, error) {
	run, err := s.recordedRun(ctx, pin)
	if err != nil {
		return distillation.CandidateSet{}, "", err
	}
	cohort, err := run.evaluationCohort(cohortID)
	if err != nil {
		return distillation.CandidateSet{}, "", err
	}
	return s.rebuild(ctx, run, filepath.Join(s.cfg.ServingRoot, run.receipt.BundleDigest), cohort)
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
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
