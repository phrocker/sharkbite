/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package code

import (
	"crypto/sha256"
	"encoding/hex"
	pathpkg "path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// ContentHash is the SHA-256 digest of the exact source bytes.
type ContentHash struct {
	digest [sha256.Size]byte
	set    bool
}

// HashContent computes the content hash for source bytes.
func HashContent(content []byte) ContentHash {
	return ContentHash{digest: sha256.Sum256(content), set: true}
}

// ParseContentHash parses the canonical sha256:<lowercase-hex> representation.
func ParseContentHash(value string) (ContentHash, error) {
	algorithm, encoded, found := strings.Cut(value, ":")
	if !found || algorithm != "sha256" || len(encoded) != sha256.Size*2 {
		return ContentHash{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "invalid content hash")
	}
	decoded, err := hex.DecodeString(encoded)
	if err != nil || hex.EncodeToString(decoded) != encoded {
		return ContentHash{}, shoal.NewError(
			shoal.ErrorInvalidArgument, "invalid content hash")
	}
	var digest [sha256.Size]byte
	copy(digest[:], decoded)
	return ContentHash{digest: digest, set: true}, nil
}

// Validate checks that the hash was explicitly supplied.
func (h ContentHash) Validate() error {
	if !h.set {
		return shoal.NewError(shoal.ErrorInvalidArgument, "content hash is required")
	}
	return nil
}

// Digest returns a copy of the raw SHA-256 digest.
func (h ContentHash) Digest() [sha256.Size]byte {
	return h.digest
}

func (h ContentHash) String() string {
	if !h.set {
		return ""
	}
	return "sha256:" + hex.EncodeToString(h.digest[:])
}

// Repository identifies the logical source repository independently of any
// local checkout path.
type Repository struct {
	locator string
}

// NewRepository creates a repository value from a stable URI or locator.
func NewRepository(locator string) (Repository, error) {
	repository := Repository{locator: locator}
	if err := repository.Validate(); err != nil {
		return Repository{}, err
	}
	return repository, nil
}

// Validate checks that the repository has an exact non-empty locator.
func (r Repository) Validate() error {
	if !requiredExact(r.locator) {
		return shoal.NewError(shoal.ErrorInvalidArgument, "repository locator is required")
	}
	return nil
}

// Locator returns the repository URI or logical locator.
func (r Repository) Locator() string {
	return r.locator
}

// Source identifies one immutable repository file revision and its exact
// content. Ref is the caller-visible branch, tag, or equivalent selector;
// Revision is the immutable source-control revision resolved from that ref.
type Source struct {
	repository  Repository
	ref         string
	path        string
	revision    string
	contentHash ContentHash
	sizeBytes   uint64
}

// NewSource creates an immutable source identity.
func NewSource(repository Repository, ref, sourcePath, revision string,
	contentHash ContentHash, sizeBytes uint64) (Source, error) {
	source := Source{
		repository:  repository,
		ref:         ref,
		path:        sourcePath,
		revision:    revision,
		contentHash: contentHash,
		sizeBytes:   sizeBytes,
	}
	if err := source.Validate(); err != nil {
		return Source{}, err
	}
	return source, nil
}

// Validate checks repository, ref, repository-relative path, immutable
// revision, and content-hash requirements.
func (s Source) Validate() error {
	if err := s.repository.Validate(); err != nil {
		return err
	}
	if !requiredExact(s.ref) {
		return shoal.NewError(shoal.ErrorInvalidArgument, "source ref is required")
	}
	if !validSourcePath(s.path) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "source path must be repository-relative and canonical")
	}
	if !requiredExact(s.revision) {
		return shoal.NewError(shoal.ErrorInvalidArgument, "source revision is required")
	}
	if err := s.contentHash.Validate(); err != nil {
		return err
	}
	return nil
}

// ID returns the source's stable identity. Ref is intentionally excluded
// because multiple mutable refs may resolve to the same immutable revision.
func (s Source) ID() ID {
	if err := s.Validate(); err != nil {
		return ID{}
	}
	id, _ := deriveID(
		"source",
		s.repository.locator,
		s.revision,
		s.path,
		s.contentHash.String(),
		strconv.FormatUint(s.sizeBytes, 10),
	)
	return id
}

func (s Source) Repository() Repository {
	return s.repository
}

func (s Source) Ref() string {
	return s.ref
}

func (s Source) Path() string {
	return s.path
}

func (s Source) Revision() string {
	return s.revision
}

func (s Source) ContentHash() ContentHash {
	return s.contentHash
}

func (s Source) SizeBytes() uint64 {
	return s.sizeBytes
}

// Equal reports whether two values identify the same ref and immutable source.
func (s Source) Equal(other Source) bool {
	return s == other
}

// Language identifies the parser-neutral source language, version, and
// dialect. Version and dialect may be empty when the source does not specify
// them.
type Language struct {
	id      string
	version string
	dialect string
}

// NewLanguage creates a language provenance value.
func NewLanguage(id, version, dialect string) (Language, error) {
	language := Language{id: id, version: version, dialect: dialect}
	if err := language.Validate(); err != nil {
		return Language{}, err
	}
	return language, nil
}

func (l Language) Validate() error {
	if !requiredExact(l.id) {
		return shoal.NewError(shoal.ErrorInvalidArgument, "language ID is required")
	}
	if !optionalExact(l.version) || !optionalExact(l.dialect) {
		return shoal.NewError(shoal.ErrorInvalidArgument, "invalid language provenance")
	}
	return nil
}

func (l Language) ID() string {
	return l.id
}

func (l Language) Version() string {
	return l.version
}

func (l Language) Dialect() string {
	return l.dialect
}

// ParserProvenance identifies the parser implementation and every
// configuration input that can change its output. Parsers with no explicit
// options use HashContent(nil) as their configuration hash.
type ParserProvenance struct {
	name              string
	version           string
	configurationHash ContentHash
}

// NewParserProvenance creates parser provenance.
func NewParserProvenance(name, version string,
	configurationHash ContentHash) (ParserProvenance, error) {
	provenance := ParserProvenance{
		name:              name,
		version:           version,
		configurationHash: configurationHash,
	}
	if err := provenance.Validate(); err != nil {
		return ParserProvenance{}, err
	}
	return provenance, nil
}

func (p ParserProvenance) Validate() error {
	if !requiredExact(p.name) || !requiredExact(p.version) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "parser name and version are required")
	}
	if err := p.configurationHash.Validate(); err != nil {
		return shoal.NewError(
			shoal.ErrorInvalidArgument, "parser configuration hash is required")
	}
	return nil
}

func (p ParserProvenance) Name() string {
	return p.name
}

func (p ParserProvenance) Version() string {
	return p.version
}

func (p ParserProvenance) ConfigurationHash() ContentHash {
	return p.configurationHash
}

func requiredExact(value string) bool {
	return value != "" && strings.TrimSpace(value) == value
}

func optionalExact(value string) bool {
	return value == "" || strings.TrimSpace(value) == value
}

func validSourcePath(sourcePath string) bool {
	return requiredExact(sourcePath) &&
		!strings.Contains(sourcePath, `\`) &&
		!pathpkg.IsAbs(sourcePath) &&
		filepath.VolumeName(sourcePath) == "" &&
		!hasWindowsDriveVolume(sourcePath) &&
		sourcePath != "." &&
		sourcePath != ".." &&
		!strings.HasPrefix(sourcePath, "../") &&
		pathpkg.Clean(sourcePath) == sourcePath
}

func hasWindowsDriveVolume(sourcePath string) bool {
	if len(sourcePath) < 2 || sourcePath[1] != ':' {
		return false
	}
	drive := sourcePath[0]
	return drive >= 'A' && drive <= 'Z' || drive >= 'a' && drive <= 'z'
}
