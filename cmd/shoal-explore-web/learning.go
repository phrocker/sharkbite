// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/phrocker/shoal-oss/internal/decisionlearning"
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
}

// studentServing adapts the student provider to the learning service's
// serving report: what the runtime serves right now, its device, and any
// fallback events, so the console shows the exact bundle digest served.
type studentServing struct {
	predictor *model.StudentPredictor
}

func (s studentServing) ServingReport(ctx context.Context) (map[string]any, error) {
	err := s.predictor.CheckHealth(ctx)
	report := s.predictor.Report()
	out := map[string]any{
		"release_id": s.predictor.Release().ID, "bundle_digest": s.predictor.Release().BundleDigest,
		"model_alias": s.predictor.Identity().ModelAlias, "egresses_off_host": s.predictor.EgressesOffHost(),
		"actual_device": report.ActualDevice, "ready": report.Ready, "fallback_events": report.FallbackEvents,
	}
	if err != nil {
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
func mountLearning(ctx context.Context, handler *webapi.Handler, resolver auth.Resolver, cfg learningHostConfig, output io.Writer) error {
	images, err := parseImagePins(cfg.images)
	if err != nil {
		return err
	}
	for _, sub := range []string{"releases/releases", "releases/pointers", "releases/lineage", "releases/events", "records", "bundles"} {
		if err := os.MkdirAll(filepath.Join(cfg.stateDir, sub), 0o700); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(cfg.servingRoot, 0o700); err != nil {
		return err
	}
	store := local.New()
	registry, err := weightsbundle.OpenRegistry(ctx, store, filepath.Join(cfg.stateDir, "releases"))
	if err != nil {
		return fmt.Errorf("learning registry: %w", err)
	}
	var serving decisionlearning.ServingReporter
	if cfg.studentEndpoint != "" {
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
				return fmt.Errorf("student runtime for release %s: %w", active.ID, err)
			}
			if predictor.EgressesOffHost() {
				return errors.New("-student-endpoint is not loopback: an off-host student runtime is classified as egressing and is not served by this host")
			}
			serving = studentServing{predictor}
		}
	}
	service, err := decisionlearning.New(decisionlearning.Config{
		Registry: registry, Store: store, Prefix: cfg.stateDir, EvidenceDir: cfg.evidenceDir,
		BundlesDir: filepath.Join(cfg.stateDir, "bundles"), ServingRoot: cfg.servingRoot,
		Serving: serving, Images: images,
	})
	if err != nil {
		return err
	}
	return handler.MountLearningSurface(webapi.LearningSurface{Resolver: resolver, Provider: service})
}
