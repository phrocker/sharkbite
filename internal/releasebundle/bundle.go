// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

// Package releasebundle exports and imports offline release bundles: plain
// files plus digests that move a promoted student release to a cluster with
// no model-hub access. Export on a connected system; import on a
// disconnected one. Nothing beyond a filesystem is assumed.
package releasebundle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/internal/weightsbundle"
)

// Layout inside a bundle directory.
const (
	IndexName           = "index.json"
	ReleaseName         = "release.json"
	LineageName         = "lineage.json"
	DatasetManifestName = "dataset-manifest.json"
	EvaluationName      = "evaluation.json"
	BundleDir           = "bundle"
	Kind                = "offline-release-bundle"
)

// Entry kinds.
const (
	EntryRelease         = "release_record"
	EntryLineage         = "dataset_lineage"
	EntryDatasetManifest = "dataset_manifest"
	EntryEvaluation      = "evaluation_record"
	EntryBundleFile      = "weights_bundle_file"
	EntryBundleManifest  = "weights_bundle_manifest"
)

// ErrVerification reports a bundle that must not be imported. The message
// names the entry.
var ErrVerification = errors.New("offline release bundle verification failed")

// ErrRefused reports a bundle whose records do not describe a complete
// promotion.
var ErrRefused = errors.New("offline release bundle refused")

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ImageRef pins one container image by digest. Tags are not accepted.
type ImageRef struct {
	Name       string `json:"name"`
	Repository string `json:"repository"`
	Digest     string `json:"digest"`
}

// Entry is one file in the bundle with its size and SHA-256.
type Entry struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Index lists every entry and every image. Its own digest is the bundle
// identity an operator quotes.
type Index struct {
	Schema       int        `json:"schema"`
	Kind         string     `json:"kind"`
	ReleaseID    string     `json:"release_id"`
	BundleDigest string     `json:"bundle_digest"`
	ExportedAt   string     `json:"exported_at"`
	Images       []ImageRef `json:"images"`
	Entries      []Entry    `json:"entries"`
}

// Digest returns the index identity.
func (i Index) Digest() (string, error) { return weightsbundle.Digest(i) }

// ExportInput is everything an export binds.
type ExportInput struct {
	Release         weightsbundle.Release
	Lineage         weightsbundle.DatasetLineage
	BundleRoot      string
	DatasetManifest []byte
	Evaluation      weightsbundle.Record
	Images          []ImageRef
	Now             time.Time
}

// Export writes a complete bundle to dest, which must not exist. The
// release must be a complete promotion whose bundle verifies and whose
// evaluation and dataset manifest match the record; anything else is
// refused before a byte is written.
func Export(in ExportInput, dest string) (Index, error) {
	index, files, err := assemble(in)
	if err != nil {
		return Index{}, err
	}
	if _, err := os.Lstat(dest); err == nil {
		return Index{}, fmt.Errorf("%w: %s exists", ErrRefused, dest)
	}
	staging, err := os.MkdirTemp(filepath.Dir(dest), filepath.Base(dest)+".exporting-")
	if err != nil {
		return Index{}, err
	}
	cleanup := func() { os.RemoveAll(staging) }
	for path, content := range files {
		target := filepath.Join(staging, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			cleanup()
			return Index{}, err
		}
		if err := writeExclusive(target, content); err != nil {
			cleanup()
			return Index{}, err
		}
	}
	encoded, err := weightsbundle.Canonical(index)
	if err != nil {
		cleanup()
		return Index{}, err
	}
	if err := writeExclusive(filepath.Join(staging, IndexName), encoded); err != nil {
		cleanup()
		return Index{}, err
	}
	if _, err := Verify(staging); err != nil {
		cleanup()
		return Index{}, err
	}
	if err := os.Rename(staging, dest); err != nil {
		cleanup()
		return Index{}, err
	}
	return index, nil
}

func assemble(in ExportInput) (Index, map[string][]byte, error) {
	if err := checkRelease(in.Release); err != nil {
		return Index{}, nil, err
	}
	manifest, digest, err := weightsbundle.Load(in.BundleRoot)
	if err != nil {
		return Index{}, nil, err
	}
	if digest != *in.Release.BundleDigest {
		return Index{}, nil, fmt.Errorf("%w: bundle digest %s is not the release's %s", ErrRefused, digest, *in.Release.BundleDigest)
	}
	if err := checkEvaluation(in.Release, in.Evaluation); err != nil {
		return Index{}, nil, err
	}
	if err := checkDatasetManifest(in.Release, in.Lineage, in.DatasetManifest); err != nil {
		return Index{}, nil, err
	}
	if err := checkImages(in.Images); err != nil {
		return Index{}, nil, err
	}
	files := map[string][]byte{}
	releaseRecord, err := in.Release.Record()
	if err != nil {
		return Index{}, nil, err
	}
	if files[ReleaseName], err = weightsbundle.EncodeRecord(releaseRecord); err != nil {
		return Index{}, nil, err
	}
	if files[LineageName], err = weightsbundle.Canonical(in.Lineage); err != nil {
		return Index{}, nil, err
	}
	files[DatasetManifestName] = in.DatasetManifest
	if files[EvaluationName], err = weightsbundle.EncodeRecord(in.Evaluation); err != nil {
		return Index{}, nil, err
	}
	for _, f := range manifest.Files {
		content, err := os.ReadFile(filepath.Join(in.BundleRoot, filepath.FromSlash(f.Path)))
		if err != nil {
			return Index{}, nil, fmt.Errorf("%w: %v", ErrVerification, err)
		}
		files[BundleDir+"/"+f.Path] = content
	}
	if files[BundleDir+"/"+weightsbundle.ManifestName], err = manifest.Encode(); err != nil {
		return Index{}, nil, err
	}
	index := Index{Schema: 1, Kind: Kind, ReleaseID: in.Release.ID, BundleDigest: digest, ExportedAt: in.Now.UTC().Format(time.RFC3339), Images: append([]ImageRef(nil), in.Images...)}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		sum := sha256.Sum256(files[p])
		index.Entries = append(index.Entries, Entry{Path: p, Kind: entryKind(p), Size: int64(len(files[p])), SHA256: hex.EncodeToString(sum[:])})
	}
	return index, files, nil
}

// checkImages applies the same rule at export and at verification: every
// image is a repository pinned by a sha256 digest, never a tag, and an
// offline bundle without a pinned runtime image is incomplete.
func checkImages(images []ImageRef) error {
	if len(images) == 0 {
		return fmt.Errorf("%w: at least one image reference by digest is required", ErrRefused)
	}
	seen := map[string]bool{}
	for _, img := range images {
		if img.Name == "" || img.Repository == "" || !digestPattern.MatchString(img.Digest) || strings.Contains(img.Repository, "@") || strings.Contains(img.Repository, ":") && strings.LastIndex(img.Repository, ":") > strings.LastIndex(img.Repository, "/") {
			return fmt.Errorf("%w: image %q must be a repository with a sha256 digest, not a tag", ErrRefused, img.Name)
		}
		if seen[img.Name] {
			return fmt.Errorf("%w: image %q listed twice", ErrRefused, img.Name)
		}
		seen[img.Name] = true
	}
	return nil
}

func entryKind(p string) string {
	switch p {
	case ReleaseName:
		return EntryRelease
	case LineageName:
		return EntryLineage
	case DatasetManifestName:
		return EntryDatasetManifest
	case EvaluationName:
		return EntryEvaluation
	case BundleDir + "/" + weightsbundle.ManifestName:
		return EntryBundleManifest
	}
	return EntryBundleFile
}

func checkRelease(r weightsbundle.Release) error {
	if err := r.Verify(); err != nil {
		return fmt.Errorf("%w: %v", ErrRefused, err)
	}
	if r.State != weightsbundle.StateActive || r.BundleDigest == nil || r.ApprovalID == "" || r.EvaluationID == "" || r.ArtifactID == "" || r.CandidateID == "" {
		return fmt.Errorf("%w: release record is not a complete promotion (needs active state, bundle digest, artifact, evaluation, and bound approval)", ErrRefused)
	}
	if r.DatasetManifestSHA256 == nil || *r.DatasetManifestSHA256 == "" {
		return fmt.Errorf("%w: release record carries no dataset manifest pin; an offline bundle cannot bind its dataset without one", ErrRefused)
	}
	return nil
}

func checkEvaluation(r weightsbundle.Release, evaluation weightsbundle.Record) error {
	if err := weightsbundle.VerifyRecord(evaluation, weightsbundle.KindEvaluation); err != nil {
		return fmt.Errorf("%w: %v", ErrRefused, err)
	}
	if id, _ := evaluation["id"].(string); id != r.EvaluationID {
		return fmt.Errorf("%w: evaluation %s is not the release's evaluation %s", ErrRefused, id, r.EvaluationID)
	}
	if d, _ := evaluation["disposition"].(string); d != "shadow_candidate" {
		return fmt.Errorf("%w: evaluation disposition %q is not shadow_candidate", ErrRefused, d)
	}
	return nil
}

func checkDatasetManifest(r weightsbundle.Release, lineage weightsbundle.DatasetLineage, manifest []byte) error {
	if len(manifest) == 0 {
		return fmt.Errorf("%w: dataset manifest is required", ErrRefused)
	}
	sum := sha256.Sum256(manifest)
	actual := hex.EncodeToString(sum[:])
	if lineage.DatasetManifestSHA256 != actual {
		return fmt.Errorf("%w: dataset manifest digest %s is not the lineage pin %s", ErrRefused, actual, lineage.DatasetManifestSHA256)
	}
	if r.DatasetManifestSHA256 == nil || *r.DatasetManifestSHA256 != actual {
		return fmt.Errorf("%w: dataset manifest digest %s is not the release pin", ErrRefused, actual)
	}
	// The lineage file is bound to nothing sealed on its own, so it must be
	// exactly what the pinned manifest derives: the manifest is bound by the
	// release record, and the source ids follow from the manifest.
	expected, err := weightsbundle.SourcesFromExportManifest(manifest)
	if err != nil {
		return fmt.Errorf("%w: dataset manifest: %v", ErrRefused, err)
	}
	if !slices.Equal(expected.SourceIDs, lineage.SourceIDs) {
		return fmt.Errorf("%w: lineage source ids %v are not those of the pinned dataset manifest %v", ErrRefused, lineage.SourceIDs, expected.SourceIDs)
	}
	return nil
}

// Verified is what Verify establishes about a bundle on disk.
type Verified struct {
	Index       Index
	IndexDigest string
	Release     weightsbundle.Release
	Lineage     weightsbundle.DatasetLineage
	Evaluation  weightsbundle.Record
	BundleRoot  string
	// Results lists every entry and whether it verified, for a console.
	Results []EntryResult
}

// EntryResult is the verification outcome of one entry.
type EntryResult struct {
	Path     string `json:"path"`
	Kind     string `json:"kind"`
	Verified bool   `json:"verified"`
	Detail   string `json:"detail,omitempty"`
}

// Verify checks every digest in dir and that the records describe a
// complete promotion. It writes nothing. A single altered byte fails with
// the entry named; an unlisted file is an extra entry and also fails.
func Verify(dir string) (Verified, error) {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	raw, err := readBounded(filepath.Join(dir, IndexName), weightsbundle.MaxRecordBytes)
	if err != nil {
		return Verified{}, fmt.Errorf("%w: index: %v", ErrVerification, err)
	}
	var index Index
	if err := weightsbundle.StrictUnmarshal(raw, &index); err != nil {
		return Verified{}, fmt.Errorf("%w: index: %v", ErrVerification, err)
	}
	if index.Schema != 1 || index.Kind != Kind || len(index.Entries) == 0 {
		return Verified{}, fmt.Errorf("%w: index is not an offline release bundle", ErrVerification)
	}
	out := Verified{Index: index, BundleRoot: filepath.Join(dir, BundleDir)}
	out.IndexDigest, _ = index.Digest()
	present := map[string]struct{}{}
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		rel = filepath.ToSlash(rel)
		if !d.Type().IsRegular() {
			return fmt.Errorf("%w: %q is not a regular file", ErrVerification, rel)
		}
		present[rel] = struct{}{}
		return nil
	})
	if err != nil {
		return out, err
	}
	delete(present, IndexName)
	var failure error
	for _, e := range index.Entries {
		result := EntryResult{Path: e.Path, Kind: e.Kind}
		if _, ok := present[e.Path]; !ok {
			result.Detail = "missing"
		} else if e.Kind != entryKind(e.Path) {
			delete(present, e.Path)
			result.Detail = "kind mismatch"
		} else {
			delete(present, e.Path)
			size, digest, err := hashFile(filepath.Join(dir, filepath.FromSlash(e.Path)))
			switch {
			case err != nil:
				result.Detail = err.Error()
			case size != e.Size:
				result.Detail = "altered (size)"
			case digest != e.SHA256:
				result.Detail = "altered (digest)"
			default:
				result.Verified = true
			}
		}
		if !result.Verified && failure == nil {
			failure = fmt.Errorf("%w: %s %q", ErrVerification, result.Detail, e.Path)
		}
		out.Results = append(out.Results, result)
	}
	for p := range present {
		out.Results = append(out.Results, EntryResult{Path: p, Kind: "unlisted", Detail: "extra"})
		if failure == nil {
			failure = fmt.Errorf("%w: extra %q", ErrVerification, p)
		}
	}
	if failure != nil {
		return out, failure
	}
	releaseRaw, _ := readBounded(filepath.Join(dir, ReleaseName), weightsbundle.MaxRecordBytes)
	record, err := weightsbundle.DecodeRecord(releaseRaw)
	if err != nil {
		return out, fmt.Errorf("%w: release record: %v", ErrRefused, err)
	}
	release, err := weightsbundle.ReleaseFromRecord(record)
	if err != nil {
		return out, fmt.Errorf("%w: %v", ErrRefused, err)
	}
	if err := checkRelease(release); err != nil {
		return out, err
	}
	if release.ID != index.ReleaseID || *release.BundleDigest != index.BundleDigest {
		return out, fmt.Errorf("%w: index names release %s bundle %s; record is %s bundle %s", ErrRefused, index.ReleaseID, index.BundleDigest, release.ID, *release.BundleDigest)
	}
	out.Release = release
	lineageRaw, _ := readBounded(filepath.Join(dir, LineageName), weightsbundle.MaxRecordBytes)
	if err := weightsbundle.StrictUnmarshal(lineageRaw, &out.Lineage); err != nil {
		return out, fmt.Errorf("%w: lineage: %v", ErrRefused, err)
	}
	evaluationRaw, _ := readBounded(filepath.Join(dir, EvaluationName), weightsbundle.MaxRecordBytes)
	out.Evaluation, err = weightsbundle.DecodeRecord(evaluationRaw)
	if err != nil {
		return out, fmt.Errorf("%w: evaluation: %v", ErrRefused, err)
	}
	if err := checkEvaluation(release, out.Evaluation); err != nil {
		return out, err
	}
	datasetManifest, _ := readBounded(filepath.Join(dir, DatasetManifestName), weightsbundle.MaxRecordBytes)
	if err := checkDatasetManifest(release, out.Lineage, datasetManifest); err != nil {
		return out, err
	}
	if _, digest, err := weightsbundle.Load(out.BundleRoot); err != nil {
		return out, err
	} else if digest != index.BundleDigest {
		return out, fmt.Errorf("%w: weights bundle digest %s is not the index's %s", ErrVerification, digest, index.BundleDigest)
	}
	if err := checkImages(index.Images); err != nil {
		return out, err
	}
	return out, nil
}

// VerifyPinned is Verify plus the out-of-band identity: the operator quotes
// the index digest from the exporting side, and a bundle whose index was
// rewritten (images, entries) after export is refused even when every entry
// still matches the rewritten index.
func VerifyPinned(dir, expectedIndexDigest string) (Verified, error) {
	if expectedIndexDigest == "" {
		return Verified{}, fmt.Errorf("%w: an expected index digest is required", ErrVerification)
	}
	out, err := Verify(dir)
	if err != nil {
		return out, err
	}
	if out.IndexDigest != expectedIndexDigest {
		return out, fmt.Errorf("%w: index digest %s is not the expected %s", ErrVerification, out.IndexDigest, expectedIndexDigest)
	}
	return out, nil
}

// ImportOptions name where an import lands.
type ImportOptions struct {
	Registry    *weightsbundle.Registry
	ServingRoot string
	Importer    string
	Now         time.Time
	// ExpectedIndexDigest, when set, is the index identity quoted from the
	// exporting side; the import is refused unless the index matches it.
	ExpectedIndexDigest string
}

// ImportResult is what an import did.
type ImportResult struct {
	Verified     Verified
	ServingPath  string
	Activated    bool
	AlreadyThere bool
}

// Import verifies every digest, then publishes the weights bundle under
// ServingRoot/<digest> with no-replace publication, then commits the release
// to the registry and records the import as a provenance event. Nothing is
// written to the serving path before verification succeeds.
func Import(ctx context.Context, dir string, opts ImportOptions) (ImportResult, error) {
	var verified Verified
	var err error
	if opts.ExpectedIndexDigest != "" {
		verified, err = VerifyPinned(dir, opts.ExpectedIndexDigest)
	} else {
		verified, err = Verify(dir)
	}
	if err != nil {
		return ImportResult{Verified: verified}, err
	}
	if opts.Registry == nil || opts.ServingRoot == "" || opts.Importer == "" {
		return ImportResult{}, errors.New("import needs a registry, serving root, and importer identity")
	}
	if err := os.MkdirAll(opts.ServingRoot, 0o700); err != nil {
		return ImportResult{}, err
	}
	result := ImportResult{Verified: verified, ServingPath: filepath.Join(opts.ServingRoot, verified.Index.BundleDigest)}
	if _, digest, err := weightsbundle.Load(result.ServingPath); err == nil && digest == verified.Index.BundleDigest {
		result.AlreadyThere = true
	} else {
		staging := result.ServingPath + ".importing"
		os.RemoveAll(staging)
		if err := copyTree(verified.BundleRoot, staging); err != nil {
			os.RemoveAll(staging)
			return result, err
		}
		if err := weightsbundle.PublishLocal(staging, result.ServingPath); err != nil {
			os.RemoveAll(staging)
			if errors.Is(err, weightsbundle.ErrVerification) {
				return result, fmt.Errorf("%w: serving path %s: %v", ErrVerification, result.ServingPath, err)
			}
			if !errors.Is(err, weightsbundle.ErrExists) {
				return result, err
			}
			result.AlreadyThere = true
		}
	}
	// The pointer must never advance to a release whose serving path does
	// not load as exactly the verified bundle: a pre-existing directory at
	// the digest's path that is partial, foreign, or altered is refused.
	if _, digest, err := weightsbundle.Load(result.ServingPath); err != nil {
		return result, fmt.Errorf("%w: serving path %s does not hold the verified bundle: %v", ErrVerification, result.ServingPath, err)
	} else if digest != verified.Index.BundleDigest {
		return result, fmt.Errorf("%w: serving path %s holds bundle %s, not %s", ErrVerification, result.ServingPath, digest, verified.Index.BundleDigest)
	}
	activated, err := opts.Registry.ImportRelease(ctx, verified.Release, verified.Lineage)
	if err != nil {
		return result, err
	}
	result.Activated = activated
	if !activated {
		// The active release was re-imported unchanged: nothing happened, so
		// no provenance event claims an import that changed nothing.
		return result, nil
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	detail := fmt.Sprintf("imported by %s from offline release bundle %s (index %s)", opts.Importer, filepath.Base(dir), verified.IndexDigest)
	if err := opts.Registry.RecordEvent(ctx, weightsbundle.Event{Kind: weightsbundle.EventImported, ReleaseID: verified.Release.ID, Detail: detail, At: now.UTC()}); err != nil {
		return result, err
	}
	return result, nil
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return writeExclusive(target, content)
	})
}

func writeExclusive(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(b)
	return errors.Join(writeErr, f.Sync(), f.Close())
}

func hashFile(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

func readBounded(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s exceeds %d bytes", filepath.Base(path), max)
	}
	return b, nil
}
