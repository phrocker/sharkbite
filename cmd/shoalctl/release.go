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
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/internal/releasebundle"
	"github.com/phrocker/shoal-oss/internal/storage/local"
	"github.com/phrocker/shoal-oss/internal/weightsbundle"
)

const releaseUsage = "expected release command: export, verify, import, or rollback"

// runRelease moves promoted student releases between connected and
// disconnected clusters as plain files plus digests.
func runRelease(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New(releaseUsage)
	}
	switch args[0] {
	case "export":
		return runReleaseExport(args[1:], stdout, stderr)
	case "verify":
		return runReleaseVerify(args[1:], stdout, stderr)
	case "import":
		return runReleaseImport(args[1:], stdout, stderr)
	case "rollback":
		return runReleaseRollback(args[1:], stdout, stderr)
	default:
		return fmt.Errorf("unknown release command %q; %s", args[0], releaseUsage)
	}
}

type imageList []releasebundle.ImageRef

func (l *imageList) String() string { return fmt.Sprint(*l) }
func (l *imageList) Set(v string) error {
	name, ref, ok := strings.Cut(v, "=")
	repo, digest, ok2 := strings.Cut(ref, "@")
	if !ok || !ok2 || name == "" {
		return fmt.Errorf("image %q must be name=repository@sha256:digest", v)
	}
	*l = append(*l, releasebundle.ImageRef{Name: name, Repository: repo, Digest: digest})
	return nil
}

func openRegistry(ctx context.Context, dir string) (*weightsbundle.Registry, error) {
	if dir == "" {
		return nil, errors.New("-registry is required")
	}
	for _, sub := range []string{"releases", "pointers", "lineage", "events"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, err
		}
	}
	return weightsbundle.OpenRegistry(ctx, local.New(), dir)
}

func readRecord(path string) (weightsbundle.Record, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return weightsbundle.DecodeRecord(raw)
}

func runReleaseExport(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("export", flag.ContinueOnError)
	flags.SetOutput(stderr)
	registryDir := flags.String("registry", "", "release registry directory on the connected side")
	releaseID := flags.String("release", "", "release identity to export (default: the active release)")
	bundleRoot := flags.String("bundle-root", "", "published weights bundle directory for the release")
	datasetManifest := flags.String("dataset-manifest", "", "authorized dataset export manifest the release was trained from")
	evaluation := flags.String("evaluation", "", "sealed evaluation (promotion_record) the release binds")
	output := flags.String("output", "", "directory to create for the offline release bundle")
	var images imageList
	flags.Var(&images, "image", "container image pinned by digest, name=repository@sha256:...; repeatable")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *bundleRoot == "" || *datasetManifest == "" || *evaluation == "" || *output == "" {
		return errors.New("release export requires -registry, -bundle-root, -dataset-manifest, -evaluation, -output, and at least one -image")
	}
	ctx := context.Background()
	registry, err := openRegistry(ctx, *registryDir)
	if err != nil {
		return err
	}
	var release weightsbundle.Release
	if *releaseID == "" {
		active, ok := registry.Active()
		if !ok {
			return weightsbundle.ErrNoActiveRelease
		}
		release = active
	} else {
		rel, ok := registry.Get(*releaseID)
		if !ok {
			return fmt.Errorf("release %s is not recorded", *releaseID)
		}
		release = rel
	}
	if e := registry.EligibilityOf(release.ID); e.Status != weightsbundle.EligibilityEligible {
		return fmt.Errorf("%w: %s", weightsbundle.ErrIneligible, e.Reason)
	}
	lineage, ok := registry.Lineage(release.ID)
	if !ok {
		return fmt.Errorf("release %s has no recorded dataset lineage", release.ID)
	}
	manifest, err := os.ReadFile(*datasetManifest)
	if err != nil {
		return err
	}
	evaluationRecord, err := readRecord(*evaluation)
	if err != nil {
		return err
	}
	index, err := releasebundle.Export(releasebundle.ExportInput{Release: release, Lineage: lineage, BundleRoot: *bundleRoot, DatasetManifest: manifest, Evaluation: evaluationRecord, Images: images, Now: time.Now()}, *output)
	if err != nil {
		return err
	}
	digest, _ := index.Digest()
	return json.NewEncoder(stdout).Encode(map[string]any{"release_id": index.ReleaseID, "bundle_digest": index.BundleDigest, "index_digest": digest, "entries": len(index.Entries), "output": *output})
}

func runReleaseVerify(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dir := flags.String("bundle", "", "offline release bundle directory")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dir == "" || flags.NArg() != 0 {
		return errors.New("release verify requires -bundle")
	}
	verified, err := releasebundle.Verify(*dir)
	report := map[string]any{"verified": err == nil, "entries": verified.Results, "release_id": verified.Index.ReleaseID, "bundle_digest": verified.Index.BundleDigest, "index_digest": verified.IndexDigest}
	if err != nil {
		report["error"] = err.Error()
	}
	if encodeErr := json.NewEncoder(stdout).Encode(report); encodeErr != nil {
		return encodeErr
	}
	return err
}

func runReleaseImport(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("import", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dir := flags.String("bundle", "", "offline release bundle directory")
	registryDir := flags.String("registry", "", "release registry directory on the disconnected side")
	servingRoot := flags.String("serving-root", "", "directory where verified weights bundles are published by digest")
	importer := flags.String("importer", "", "identity of the operator performing the import")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dir == "" || *servingRoot == "" || *importer == "" || flags.NArg() != 0 {
		return errors.New("release import requires -bundle, -registry, -serving-root, and -importer")
	}
	ctx := context.Background()
	registry, err := openRegistry(ctx, *registryDir)
	if err != nil {
		return err
	}
	result, err := releasebundle.Import(ctx, *dir, releasebundle.ImportOptions{Registry: registry, ServingRoot: *servingRoot, Importer: *importer, Now: time.Now()})
	report := map[string]any{"imported": err == nil, "activated": result.Activated, "already_present": result.AlreadyThere, "serving_path": result.ServingPath, "entries": result.Verified.Results, "release_id": result.Verified.Index.ReleaseID, "bundle_digest": result.Verified.Index.BundleDigest}
	if err != nil {
		report["error"] = err.Error()
	}
	if encodeErr := json.NewEncoder(stdout).Encode(report); encodeErr != nil {
		return encodeErr
	}
	return err
}

func runReleaseRollback(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("rollback", flag.ContinueOnError)
	flags.SetOutput(stderr)
	registryDir := flags.String("registry", "", "release registry directory")
	prior := flags.String("prior", "", "identity of the previously imported or promoted release to restore")
	owner := flags.String("owner", "", "identity of the approver bound to this rollback")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *prior == "" || *owner == "" || flags.NArg() != 0 {
		return errors.New("release rollback requires -registry, -prior, and -owner")
	}
	ctx := context.Background()
	registry, err := openRegistry(ctx, *registryDir)
	if err != nil {
		return err
	}
	active, ok := registry.Active()
	if !ok {
		return weightsbundle.ErrNoActiveRelease
	}
	approval, err := weightsbundle.NewRollbackApproval(active.ID, *prior, *owner)
	if err != nil {
		return err
	}
	release, err := registry.Rollback(ctx, *prior, approval)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(map[string]any{"release_id": release.ID, "rollback_of": *release.RollbackOf, "bundle_digest": *release.BundleDigest, "approval_id": release.ApprovalID})
}
