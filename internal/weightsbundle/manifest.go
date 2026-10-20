// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package weightsbundle

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ManifestName is the file that makes a bundle visible. Readers treat a
// directory without it as absent, never as a partial bundle.
const ManifestName = "bundle.json"

// Kind is the manifest record kind.
const Kind = "weights-bundle"

// MaxFiles bounds the manifest; MaxManifestBytes bounds its encoding.
const MaxFiles = 256
const MaxManifestBytes int64 = 1 << 20

// File roles. Exactly one weights file is required; the others are optional
// and each may appear at most once, so a bundle's serving identity is the
// full set the Laya adapter names: weights, tokenizer, preprocessing,
// runtime settings, calibration.
const (
	RoleWeights       = "weights"
	RoleTokenizer     = "tokenizer"
	RolePreprocessing = "preprocessing"
	RoleRuntime       = "runtime"
	RoleCalibration   = "calibration"
)

var roles = map[string]bool{RoleWeights: true, RoleTokenizer: true, RolePreprocessing: true, RoleRuntime: true, RoleCalibration: true}

// ErrVerification reports a bundle that must not be loaded. The message
// names the offending file and whether it is missing, extra, or altered.
var ErrVerification = errors.New("weights bundle verification failed")

// ErrInvalidManifest reports a manifest that is malformed before any file is
// consulted.
var ErrInvalidManifest = errors.New("invalid weights bundle manifest")

// File is one immutable member of a bundle. Path is relative to the bundle
// root, clean, and never the manifest itself.
type File struct {
	Path   string `json:"path"`
	Role   string `json:"role"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest is the ordered file list. Order is part of the bundle identity:
// two trainers that emit the same files in a different order produce
// different digests, which the reproducibility comparison names.
type Manifest struct {
	Schema int    `json:"schema"`
	Kind   string `json:"kind"`
	Files  []File `json:"files"`
}

// Validate checks the manifest shape without touching the filesystem.
func (m Manifest) Validate() error {
	if m.Schema != 1 || m.Kind != Kind {
		return fmt.Errorf("%w: schema %d kind %q", ErrInvalidManifest, m.Schema, m.Kind)
	}
	if len(m.Files) == 0 || len(m.Files) > MaxFiles {
		return fmt.Errorf("%w: %d files", ErrInvalidManifest, len(m.Files))
	}
	seenPath := make(map[string]struct{}, len(m.Files))
	seenRole := make(map[string]struct{}, len(m.Files))
	for _, f := range m.Files {
		if err := validatePath(f.Path); err != nil {
			return err
		}
		if _, dup := seenPath[f.Path]; dup {
			return fmt.Errorf("%w: duplicate path %q", ErrInvalidManifest, f.Path)
		}
		seenPath[f.Path] = struct{}{}
		if !roles[f.Role] {
			return fmt.Errorf("%w: unknown role %q for %q", ErrInvalidManifest, f.Role, f.Path)
		}
		if _, dup := seenRole[f.Role]; dup {
			return fmt.Errorf("%w: role %q appears twice", ErrInvalidManifest, f.Role)
		}
		seenRole[f.Role] = struct{}{}
		if f.Size < 0 {
			return fmt.Errorf("%w: negative size for %q", ErrInvalidManifest, f.Path)
		}
		if !validDigest(f.SHA256) {
			return fmt.Errorf("%w: invalid digest for %q", ErrInvalidManifest, f.Path)
		}
	}
	if _, ok := seenRole[RoleWeights]; !ok {
		return fmt.Errorf("%w: no weights file", ErrInvalidManifest)
	}
	return nil
}

func validatePath(p string) error {
	clean := filepath.ToSlash(filepath.Clean(p))
	if p == "" || clean != p || filepath.IsAbs(p) || p == "." || p == ".." || strings.HasPrefix(p, "../") || strings.Contains(p, "\\") || p == ManifestName {
		return fmt.Errorf("%w: path %q must be relative, clean, and not the manifest", ErrInvalidManifest, p)
	}
	for _, segment := range strings.Split(p, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("%w: path %q must be relative and clean", ErrInvalidManifest, p)
		}
	}
	return nil
}

func validDigest(d string) bool {
	if len(d) != sha256.Size*2 || strings.ToLower(d) != d {
		return false
	}
	_, err := hex.DecodeString(d)
	return err == nil
}

// Digest is the bundle digest: the SHA-256 of the canonical manifest. It
// covers every path, role, size, digest, and their order.
func (m Manifest) Digest() (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}
	return Digest(m)
}

// Encode produces the canonical manifest bytes written as bundle.json.
func (m Manifest) Encode() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return Canonical(m)
}

// File returns the member with role, if present.
func (m Manifest) File(role string) (File, bool) {
	for _, f := range m.Files {
		if f.Role == role {
			return f, true
		}
	}
	return File{}, false
}

// DecodeManifest parses manifest bytes strictly and validates them.
func DecodeManifest(raw []byte) (Manifest, error) {
	if int64(len(raw)) > MaxManifestBytes {
		return Manifest{}, fmt.Errorf("%w: exceeds %d bytes", ErrInvalidManifest, MaxManifestBytes)
	}
	record, err := DecodeRecord(raw)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: %v", ErrInvalidManifest, err)
	}
	for key := range record {
		if key != "schema" && key != "kind" && key != "files" {
			return Manifest{}, fmt.Errorf("%w: unknown member %q", ErrInvalidManifest, key)
		}
	}
	var m Manifest
	if !isOne(record["schema"]) {
		return Manifest{}, fmt.Errorf("%w: schema", ErrInvalidManifest)
	}
	m.Schema = 1
	m.Kind, _ = record["kind"].(string)
	files, _ := record["files"].([]any)
	for _, item := range files {
		obj, ok := item.(map[string]any)
		if !ok {
			return Manifest{}, fmt.Errorf("%w: file entry", ErrInvalidManifest)
		}
		for key := range obj {
			if key != "path" && key != "role" && key != "size" && key != "sha256" {
				return Manifest{}, fmt.Errorf("%w: unknown file member %q", ErrInvalidManifest, key)
			}
		}
		var f File
		f.Path, _ = obj["path"].(string)
		f.Role, _ = obj["role"].(string)
		f.SHA256, _ = obj["sha256"].(string)
		size, err := numberInt64(obj["size"])
		if err != nil {
			return Manifest{}, fmt.Errorf("%w: size for %q", ErrInvalidManifest, f.Path)
		}
		f.Size = size
		m.Files = append(m.Files, f)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// Build hashes the files of a staged bundle. roles maps each relative path
// to its role; every regular file under root must be named or Build refuses,
// so a stray file never becomes an unrecorded part of the bundle. Entries are
// ordered by path so independent trainers that emit the same files agree.
func Build(root string, roles map[string]string) (Manifest, error) {
	m := Manifest{Schema: 1, Kind: Kind}
	present, err := listRegularFiles(root)
	if err != nil {
		return Manifest{}, err
	}
	paths := make([]string, 0, len(roles))
	for p := range roles {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if _, ok := present[p]; !ok {
			return Manifest{}, fmt.Errorf("%w: missing %q", ErrVerification, p)
		}
		size, digest, err := hashFile(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil {
			return Manifest{}, err
		}
		m.Files = append(m.Files, File{Path: p, Role: roles[p], Size: size, SHA256: digest})
	}
	for p := range present {
		if _, ok := roles[p]; !ok && p != ManifestName {
			return Manifest{}, fmt.Errorf("%w: extra %q", ErrVerification, p)
		}
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// Verify checks every member of m under root and that nothing else is there.
// It fails before any caller loads a byte: a missing, extra, or altered file
// is named in the error.
func Verify(root string, m Manifest) error {
	if err := m.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(root) == "" {
		return fmt.Errorf("%w: root is required", ErrVerification)
	}
	present, err := listRegularFiles(root)
	if err != nil {
		return err
	}
	for _, f := range m.Files {
		info, ok := present[f.Path]
		if !ok {
			return fmt.Errorf("%w: missing %q", ErrVerification, f.Path)
		}
		if info.Size() != f.Size {
			return fmt.Errorf("%w: altered %q (size)", ErrVerification, f.Path)
		}
		_, digest, err := hashFile(filepath.Join(root, filepath.FromSlash(f.Path)))
		if err != nil {
			return err
		}
		if digest != f.SHA256 {
			return fmt.Errorf("%w: altered %q (digest)", ErrVerification, f.Path)
		}
		delete(present, f.Path)
	}
	delete(present, ManifestName)
	if len(present) != 0 {
		extra := make([]string, 0, len(present))
		for p := range present {
			extra = append(extra, p)
		}
		sort.Strings(extra)
		return fmt.Errorf("%w: extra %q", ErrVerification, extra[0])
	}
	return nil
}

// Load reads and verifies a published bundle, returning its manifest and
// digest. A root without a manifest is absent, not partial.
func Load(root string) (Manifest, string, error) {
	raw, err := readBounded(filepath.Join(root, ManifestName), MaxManifestBytes)
	if err != nil {
		return Manifest{}, "", fmt.Errorf("%w: manifest: %v", ErrVerification, err)
	}
	m, err := DecodeManifest(raw)
	if err != nil {
		return Manifest{}, "", err
	}
	if err := Verify(root, m); err != nil {
		return Manifest{}, "", err
	}
	digest, err := m.Digest()
	if err != nil {
		return Manifest{}, "", err
	}
	return m, digest, nil
}

// Reproducibility records whether two trainer runs on the same manifest
// agree. Diverging names every path whose digest, size, role, position, or
// presence differs; an empty list with Reproducible false cannot occur.
type Reproducibility struct {
	Reproducible bool     `json:"reproducible"`
	FirstDigest  string   `json:"first_digest"`
	SecondDigest string   `json:"second_digest"`
	Diverging    []string `json:"diverging"`
}

// Compare names the files that differ between two bundles.
func Compare(first, second Manifest) (Reproducibility, error) {
	firstDigest, err := first.Digest()
	if err != nil {
		return Reproducibility{}, err
	}
	secondDigest, err := second.Digest()
	if err != nil {
		return Reproducibility{}, err
	}
	result := Reproducibility{Reproducible: firstDigest == secondDigest, FirstDigest: firstDigest, SecondDigest: secondDigest, Diverging: []string{}}
	if result.Reproducible {
		return result, nil
	}
	index := func(m Manifest) map[string]int {
		out := make(map[string]int, len(m.Files))
		for i, f := range m.Files {
			out[f.Path] = i
		}
		return out
	}
	a, b := index(first), index(second)
	seen := map[string]struct{}{}
	for p, i := range a {
		j, ok := b[p]
		if !ok || i != j || first.Files[i] != second.Files[j] {
			seen[p] = struct{}{}
		}
	}
	for p := range b {
		if _, ok := a[p]; !ok {
			seen[p] = struct{}{}
		}
	}
	for p := range seen {
		result.Diverging = append(result.Diverging, p)
	}
	sort.Strings(result.Diverging)
	return result, nil
}

func listRegularFiles(root string) (map[string]fs.FileInfo, error) {
	out := map[string]fs.FileInfo{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("%w: %v", ErrVerification, err)
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("%w: %v", ErrVerification, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: %q is not a regular file", ErrVerification, rel)
		}
		out[rel] = info
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrVerification) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", ErrVerification, err)
	}
	return out, nil
}

func hashFile(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", fmt.Errorf("%w: %v", ErrVerification, err)
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return 0, "", fmt.Errorf("%w: read %s: %v", ErrVerification, filepath.Base(path), err)
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

func numberInt64(v any) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case int:
		return int64(n), nil
	case interface{ Int64() (int64, error) }:
		return n.Int64()
	}
	return 0, errors.New("not an integer")
}
