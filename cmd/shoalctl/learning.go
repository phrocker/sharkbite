// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/phrocker/shoal-oss/internal/distillation"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/weightsbundle"
)

const learningUsage = "expected learning command: candidate"

// runLearning turns a completed distillation run into the records the
// learning service accepts, from files, with no host involved.
func runLearning(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New(learningUsage)
	}
	switch args[0] {
	case "candidate":
		return runLearningCandidate(args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown learning command %q; %s", args[0], learningUsage)
	}
}

// runLearningCandidate seals the candidate, evaluation, and artifact for a
// run from its manifest, receipt, prediction set, bundle, the authorized
// export it trained on, and a predeclared evaluation policy. It writes
// candidate.json ({candidate, artifact}), evaluation.json, promote.json, and
// candidate-set.json, ready for the learning routes or the console, and with
// -serving-root publishes the bundle at <root>/<digest> without replacing
// anything. The same inputs seal to the same bytes here, in the console, and
// in experiments/local-ml/seal_candidate.py.
func runLearningCandidate(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("candidate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	manifestPath := flags.String("manifest", "", "distillation manifest exactly as submitted (its bytes are its pin)")
	pin := flags.String("manifest-pin", "", "the manifest pin the host recorded")
	receiptPath := flags.String("receipt", "", "the run's distillation-receipt.json")
	predictionsPath := flags.String("predictions", "", "the run's teacher-predictions.json")
	bundleDir := flags.String("bundle", "", "the run's bundle directory (bundle.json, weights, runtime)")
	exportPath := flags.String("export-manifest", "", "the authorized export manifest the run trained on")
	datasetPath := flags.String("dataset", "", "the authorized export's dataset bytes")
	policyPath := flags.String("policy", "", "evaluation policy JSON: id, split, positive_label, minimum_recall, minimum_positives, minimum_families")
	output := flags.String("output", "", "directory for the sealed records")
	servingRoot := flags.String("serving-root", "", "optional: publish the bundle at <serving-root>/<digest>")
	if err := flags.Parse(args); err != nil {
		return err
	}
	for name, value := range map[string]string{"-manifest": *manifestPath, "-manifest-pin": *pin, "-receipt": *receiptPath, "-predictions": *predictionsPath, "-bundle": *bundleDir, "-export-manifest": *exportPath, "-dataset": *datasetPath, "-policy": *policyPath, "-output": *output} {
		if value == "" {
			return fmt.Errorf("learning candidate requires %s", name)
		}
	}
	if flags.NArg() != 0 {
		return errors.New("learning candidate accepts no positional arguments")
	}
	rawManifest, err := os.ReadFile(*manifestPath)
	if err != nil {
		return err
	}
	if weightsbundle.SHA256Hex(rawManifest) != *pin {
		return fmt.Errorf("manifest bytes hash to %s, not the declared pin %s", weightsbundle.SHA256Hex(rawManifest), *pin)
	}
	manifest, err := distillation.Decode(rawManifest)
	if err != nil {
		return err
	}
	var receipt distillation.Receipt
	if err := readStrict(*receiptPath, &receipt); err != nil {
		return err
	}
	var predictions distillation.PredictionSet
	if err := readStrict(*predictionsPath, &predictions); err != nil {
		return err
	}
	bundle, digest, err := weightsbundle.Load(*bundleDir)
	if err != nil {
		return err
	}
	weightsFile, ok := bundle.File(weightsbundle.RoleWeights)
	if !ok {
		return errors.New("bundle has no weights file")
	}
	weights, err := os.ReadFile(filepath.Join(*bundleDir, weightsFile.Path))
	if err != nil {
		return err
	}
	exportManifest, err := os.ReadFile(*exportPath)
	if err != nil {
		return err
	}
	dataset, err := os.ReadFile(*datasetPath)
	if err != nil {
		return err
	}
	rawPolicy, err := os.ReadFile(*policyPath)
	if err != nil {
		return err
	}
	policy, err := distillation.DecodeEvaluationPolicy(rawPolicy)
	if err != nil {
		return err
	}
	sealed, err := distillation.BuildCandidateSet(distillation.CandidateInput{
		Manifest: manifest, Pin: *pin, Receipt: receipt, Predictions: predictions, Bundle: bundle,
		Weights: weights, ExportManifest: exportManifest, Dataset: dataset, Policy: policy,
	})
	if err != nil {
		return err
	}
	placement := "not published"
	if *servingRoot != "" {
		placement, err = publishBundle(*bundleDir, *servingRoot, digest)
		if err != nil {
			return err
		}
	}
	if err := os.MkdirAll(*output, 0o700); err != nil {
		return err
	}
	for name, value := range map[string]any{
		"candidate-set.json": sealed,
		"candidate.json":     map[string]any{"candidate": sealed.Candidate, "artifact": sealed.Artifact},
		"evaluation.json":    sealed.Evaluation,
		"promote.json":       map[string]any{"candidate_id": sealed.Candidate["id"], "evaluation_id": sealed.Evaluation["id"]},
	} {
		encoded, err := weightsbundle.Canonical(value)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(*output, name), encoded, 0o600); err != nil {
			return err
		}
	}
	return json.NewEncoder(stdout).Encode(map[string]any{
		"bundle_digest": digest, "candidate_id": sealed.Candidate["id"], "evaluation_id": sealed.Evaluation["id"],
		"disposition": sealed.Evaluation["disposition"], "reasons": sealed.Evaluation["reasons"], "bundle": placement,
	})
}

// publishBundle copies the verified bundle into a staging directory under the
// serving root and publishes it at <root>/<digest>. A same-digest occupant is
// reported as present; anything else under the digest is refused.
func publishBundle(bundleDir, servingRoot, digest string) (string, error) {
	if err := os.MkdirAll(servingRoot, 0o700); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(servingRoot, ".publish-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	entries, err := os.ReadDir(bundleDir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			return "", fmt.Errorf("bundle entry %s is not a regular file", e.Name())
		}
		content, err := os.ReadFile(filepath.Join(bundleDir, e.Name()))
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(staging, e.Name()), content, 0o600); err != nil {
			return "", err
		}
	}
	err = weightsbundle.PublishLocal(staging, filepath.Join(servingRoot, digest))
	switch {
	case err == nil:
		return "published", nil
	case errors.Is(err, storage.ErrExists):
		return "present", nil
	default:
		return "", err
	}
}

func readStrict(path string, into any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := weightsbundle.StrictUnmarshal(raw, into); err != nil {
		return fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return nil
}
