// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/phrocker/shoal-oss/internal/decisiondatasethttp"
	"github.com/phrocker/shoal-oss/internal/decisionlearning"
	"github.com/phrocker/shoal-oss/internal/distillation"
	"github.com/phrocker/shoal-oss/internal/releasebundle"
	"github.com/phrocker/shoal-oss/internal/storage/local"
	"github.com/phrocker/shoal-oss/internal/weightsbundle"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
	"github.com/phrocker/shoal-oss/pkg/model"
)

// learningHostConfig is what the host needs to mount the Learning console's
// routes. The default binary mounts nothing: with no -learning-state-dir the
// console reports the service unavailable rather than implying records.
type learningHostConfig struct {
	stateDir, evidenceDir, servingRoot, images                           string
	studentEndpoint, studentModelAlias, studentCriteriaID, studentDevice string
	// modelEgress lists provider=loopback|off_host pairs: the host's own
	// statement of where each provider's calls go. The student provider is
	// added from its endpoint. A manifest naming a provider absent here is
	// refused, since its egress cannot be resolved.
	modelEgress        string
	allowOffHostModels bool
	// fixtureCohorts names the demo task fixtures (source, review) to
	// prepare as the development cohort authority and exporter.
	fixtureCohorts string
}

// hostEgressResolver resolves model egress from the operator's
// configuration, never from the manifest author's declaration.
type hostEgressResolver map[string]string

func parseModelEgress(spec string) (hostEgressResolver, error) {
	out := hostEgressResolver{}
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		provider, posture, ok := strings.Cut(item, "=")
		provider = strings.TrimSpace(provider)
		posture = strings.TrimSpace(posture)
		if !ok || provider == "" || (posture != "loopback" && posture != "off_host") {
			return nil, fmt.Errorf("-learning-model-egress entry %q must be provider=loopback or provider=off_host", item)
		}
		if _, dup := out[provider]; dup {
			return nil, fmt.Errorf("-learning-model-egress lists provider %q twice", provider)
		}
		out[provider] = posture
	}
	return out, nil
}

func (r hostEgressResolver) ResolveEgress(_ context.Context, id distillation.ModelIdentity) (string, error) {
	posture, ok := r[id.Provider]
	if !ok {
		return "", fmt.Errorf("provider %q is not in this host's -learning-model-egress configuration", id.Provider)
	}
	return posture, nil
}

// studentServing adapts the student provider to the learning service's
// serving report: what the runtime serves right now, its device, and any
// fallback events, so the console shows the exact bundle digest served.
// The student's task is one proposition over a feature vector; the host
// asks it under a fixed question set so every prediction is comparable.
const (
	studentQuestionSet = "student-features:v1"
	studentQuestionID  = "property"
)

type studentServing struct {
	predictor *model.StudentPredictor
}

// PredictFeatures asks the runtime the student's task for one feature
// vector. The question set is fixed: one proposition, answered as p_true.
func (s studentServing) PredictFeatures(ctx context.Context, features []float64) (map[string]any, error) {
	state, err := json.Marshal(map[string]any{"features": features})
	if err != nil {
		return nil, err
	}
	result, err := s.predictor.Predict(ctx, model.TypedRequest{
		ModelAlias: s.predictor.Identity().ModelAlias, QuestionSetID: studentQuestionSet, State: state,
		Questions: []model.TypedQuestion{{ID: studentQuestionID, Kind: model.TypedPropositionProbability, Instructions: "Does the student's task proposition hold for this feature vector?"}},
	})
	if err != nil {
		return nil, err
	}
	answers := make([]map[string]any, 0, len(result.Answers))
	for _, a := range result.Answers {
		answers = append(answers, map[string]any{"question_id": a.QuestionID, "p_true": a.PTrue, "confidence": a.Confidence})
	}
	return map[string]any{
		"status": string(result.Status), "reason": result.Reason, "answers": answers,
		"release_id": s.predictor.Release().ID, "bundle_digest": s.predictor.Release().BundleDigest,
		"model_alias": result.Identity.ModelAlias, "device": result.Identity.Device, "question_id": studentQuestionID,
	}, nil
}

func (s studentServing) ServingReport(ctx context.Context) (map[string]any, error) {
	err := s.predictor.CheckHealth(ctx)
	report := s.predictor.Report()
	out := map[string]any{
		"release_id": s.predictor.Release().ID, "bundle_digest": s.predictor.Release().BundleDigest,
		"model_alias": s.predictor.Identity().ModelAlias, "egresses_off_host": s.predictor.EgressesOffHost(),
		"actual_device": report.ActualDevice, "ready": err == nil && report.Ready, "fallback_events": report.FallbackEvents,
		"recording_errors": report.RecordingErrors,
	}
	if err != nil {
		// A runtime that answered with another release, bundle, or device
		// is not ready to serve this one, whatever its own status says.
		out["health_error"] = err.Error()
	}
	return out, nil
}

func parseImagePins(spec string) ([]releasebundle.ImageRef, error) {
	var out []releasebundle.ImageRef
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, ref, ok := strings.Cut(item, "=")
		repo, digest, ok2 := strings.Cut(ref, "@")
		if !ok || !ok2 || name == "" || repo == "" || !strings.HasPrefix(digest, "sha256:") {
			return nil, fmt.Errorf("-learning-images entry %q must be name=repository@sha256:digest", item)
		}
		out = append(out, releasebundle.ImageRef{Name: name, Repository: repo, Digest: digest})
	}
	return out, nil
}

// mountLearning composes the file-backed learning service and mounts the
// learning surface atomically. No cohort authority or exporter is wired by
// the default binary, and the service says so rather than faking one.
func mountLearning(ctx context.Context, handler *webapi.Handler, resolver auth.Resolver, cfg learningHostConfig, output io.Writer) (io.Closer, error) {
	images, err := parseImagePins(cfg.images)
	if err != nil {
		return nil, err
	}
	for _, sub := range []string{"releases/releases", "releases/pointers", "releases/lineage", "releases/events", "records", "bundles"} {
		if err := os.MkdirAll(filepath.Join(cfg.stateDir, sub), 0o700); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(cfg.servingRoot, 0o700); err != nil {
		return nil, err
	}
	store := local.New()
	registry, err := weightsbundle.OpenRegistry(ctx, store, filepath.Join(cfg.stateDir, "releases"))
	if err != nil {
		return nil, fmt.Errorf("learning registry: %w", err)
	}
	egress, err := parseModelEgress(cfg.modelEgress)
	if err != nil {
		return nil, err
	}
	var serving decisionlearning.ServingReporter
	if cfg.studentEndpoint != "" {
		// The endpoint is classified before anything else: an off-host
		// student runtime is refused at startup whether or not a release is
		// active, so "local-student" is only ever resolved as loopback when
		// it is one.
		if model.EndpointEgressesOffHost(cfg.studentEndpoint) {
			return nil, errors.New("-student-endpoint is not loopback: an off-host student runtime is classified as egressing and is not served by this host")
		}
		active, ok := registry.Active()
		switch {
		case !ok:
			fmt.Fprintln(output, "student runtime configured but no release is active; nothing is served until a release is imported or promoted")
		case active.BundleDigest == nil:
			fmt.Fprintln(output, "student runtime configured but the active release has no weights bundle; nothing is served")
		default:
			predictor, err := model.NewStudentPredictor(ctx, model.StudentConfig{
				BaseURL: cfg.studentEndpoint, BundleRoot: filepath.Join(cfg.servingRoot, *active.BundleDigest),
				Release:    model.StudentRelease{ID: active.ID, BundleDigest: *active.BundleDigest},
				ModelAlias: cfg.studentModelAlias, CriteriaID: cfg.studentCriteriaID, Device: cfg.studentDevice,
				DistributionTolerance: 0.01, Recorder: registry,
			})
			if err != nil {
				return nil, fmt.Errorf("student runtime for release %s: %w", active.ID, err)
			}
			if predictor.EgressesOffHost() {
				return nil, errors.New("-student-endpoint is not loopback: an off-host student runtime is classified as egressing and is not served by this host")
			}
			serving = studentServing{predictor}
		}
		if _, listed := egress[model.StudentProvider]; !listed {
			egress[model.StudentProvider] = "loopback"
		}
	}
	var cohorts decisionlearning.CohortLister
	var exporter decisionlearning.Exporter
	var closer io.Closer = nopCloser{}
	if tasks, err := parseFixtureCohortTasks(cfg.fixtureCohorts); err != nil {
		return nil, err
	} else if len(tasks) > 0 {
		fixtures, err := mountFixtureCohorts(cfg.stateDir, tasks, output)
		if err != nil {
			return nil, err
		}
		cohorts, exporter, closer = fixtures, fixtures, fixtures
		// The bytes behind every recorded pin are served on the dataset
		// exports route, so a trainer on this host can fetch exactly what
		// the learning service recorded.
		datasets, err := decisiondatasethttp.New(fixtureDatasetProvider{fixtures})
		if err != nil {
			fixtures.Close()
			return nil, err
		}
		if err := handler.MountDatasetExports(datasets, resolver); err != nil {
			fixtures.Close()
			return nil, err
		}
	}
	service, err := decisionlearning.New(decisionlearning.Config{
		Registry: registry, Store: store, Prefix: cfg.stateDir, EvidenceDir: cfg.evidenceDir,
		BundlesDir: filepath.Join(cfg.stateDir, "bundles"), ServingRoot: cfg.servingRoot,
		Serving: serving, Images: images, Egress: egress, AllowOffHostModels: cfg.allowOffHostModels, Log: output,
		Cohorts: cohorts, Exporter: exporter,
	})
	if err != nil {
		return nil, err
	}
	if err := handler.MountLearningSurface(webapi.LearningSurface{Resolver: resolver, Provider: service}); err != nil {
		closer.Close()
		return nil, err
	}
	return closer, nil
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }
