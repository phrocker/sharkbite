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

package webapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	uploadMetadataKind      = "source_kind"
	uploadMetadataLabel     = "source_label"
	uploadMetadataMediaType = "source_media_type"
)

type preparedUpload struct {
	name      string
	mediaType string
	skillFile *SkillFileResult
	source    explorer.Source
}

func (s *EmbeddedService) Ingest(
	ctx context.Context, request IngestRequest,
) (IngestResponse, error) {
	prepared, err := prepareUploads(request)
	if err != nil {
		return IngestResponse{}, err
	}
	ctx, err = declassificationReasonContext(ctx, request.DeclassificationReason)
	if err != nil {
		return IngestResponse{}, err
	}
	results := make([]IngestFileResult, 0, len(prepared))
	for index, item := range prepared {
		result, err := s.client.Ingest(ctx, item.source)
		if err != nil {
			wrapped := error(&uploadFileError{
				name: item.name,
				cause: shoal.WrapError(
					primaryErrorCode(err), "ingest upload file", err),
			})
			// Files are ingested one at a time, so once an earlier file of
			// this batch went through, a failure leaves the batch partly
			// applied. The caller cannot tell which files took effect from
			// the error alone, so it is marked indeterminate, as
			// RemoteService marks every failed multi-file upload. A failure
			// on the first file changed nothing and stays definite.
			if index > 0 {
				wrapped = explorer.MarkIndeterminateCommit(wrapped)
			}
			return IngestResponse{}, wrapped
		}
		results = append(results, IngestFileResult{
			Name:         item.name,
			MediaType:    item.mediaType,
			Disposition:  result.Disposition,
			Document:     result.Document,
			Revision:     result.Revision,
			SectionCount: result.SectionCount,
			SpanCount:    result.SpanCount,
			SkillFile:    item.skillFile,
		})
	}
	snapshot, err := s.client.Snapshot(ctx)
	if err != nil {
		return IngestResponse{}, err
	}
	return IngestResponse{Snapshot: fromExplorerSnapshot(snapshot), Files: results}, nil
}

// uploadFileError names the upload file whose ingest failed. The name is
// the sanitized filename the caller supplied, so it can be shown back to
// that caller.
type uploadFileError struct {
	name  string
	cause error
}

func (e *uploadFileError) Error() string { return e.cause.Error() }

func (e *uploadFileError) Unwrap() error { return e.cause }

// historicalUploadMessage is the public form of
// authorized.ErrHistoricalRevision for upload file name (#674).
func historicalUploadMessage(name string) string {
	return fmt.Sprintf("upload file %q matches an earlier revision of its "+
		"document, so the document was left unchanged; change the content "+
		"to upload it", name)
}

const (
	invalidDeclassificationReasonMessage  = "declassification reason is invalid"
	repeatedDeclassificationReasonMessage = "upload declassification reason is repeated"
	// declassificationReasonRequiredMessage is the public form of
	// authorized.IsDeclassificationReasonRequired.
	declassificationReasonRequiredMessage = "this upload removes visibility " +
		"labels from a document and the workspace requires a " +
		DeclassificationReasonField
)

// declassificationReasonContext attaches request's reason to ctx. An empty
// reason leaves ctx as it is. An invalid one is refused with one stable
// public error, whichever rule it breaks.
func declassificationReasonContext(
	ctx context.Context, reason string,
) (context.Context, error) {
	if reason == "" {
		return ctx, nil
	}
	bound, err := authorized.WithDeclassificationReason(ctx, reason)
	if err != nil {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, invalidDeclassificationReasonMessage)
	}
	return bound, nil
}

func prepareUploads(request IngestRequest) ([]preparedUpload, error) {
	if len(request.Files) == 0 {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "at least one upload file is required")
	}
	if uint32(len(request.Files)) > MaxUploadFiles {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "upload file count exceeds the server bound")
	}
	var total uint64
	prepared := make([]preparedUpload, 0, len(request.Files))
	seen := make(map[string]struct{}, len(request.Files))
	for _, file := range request.Files {
		name, err := sanitizeUploadFilename(file.Name)
		if err != nil {
			return nil, err
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, shoal.NewError(shoal.ErrorInvalidArgument, "upload filenames must be unique")
		}
		seen[name] = struct{}{}
		size := uint64(len(file.Content))
		if size > MaxUploadFileBytes {
			return nil, shoal.NewError(shoal.ErrorInvalidArgument, "upload file exceeds the server bound")
		}
		total += size
		if total > MaxUploadTotalBytes {
			return nil, shoal.NewError(shoal.ErrorInvalidArgument, "upload request exceeds the server bound")
		}
		if !utf8.Valid(file.Content) {
			return nil, shoal.NewError(shoal.ErrorInvalidArgument, "upload file must be valid UTF-8")
		}
		if hasBinaryControlBytes(file.Content) {
			return nil, shoal.NewError(shoal.ErrorInvalidArgument, "upload file must be textual UTF-8")
		}
		mediaType, err := inspectUploadMediaType(name, file.Content)
		if err != nil {
			return nil, err
		}
		content := string(file.Content)
		skillFile := inspectUploadSkillFile(name, mediaType, content)
		source := explorer.Source{
			URI:       "upload://workspace/" + url.PathEscape(name),
			Title:     name,
			MediaType: mediaType,
			Content:   content,
			Metadata: shoal.Metadata{
				uploadMetadataKind:      "browser_upload",
				uploadMetadataLabel:     name,
				uploadMetadataMediaType: mediaType,
			},
		}
		if err := explorer.ValidateSource(source); err != nil {
			return nil, err
		}
		prepared = append(prepared, preparedUpload{
			name:      name,
			mediaType: mediaType,
			skillFile: skillFile,
			source:    source,
		})
	}
	return prepared, nil
}

func sanitizeUploadFilename(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" || name != raw {
		return "", invalidUploadFilenameError()
	}
	if filepath.IsAbs(name) || path.IsAbs(name) || hasWindowsDrivePrefix(name) {
		return "", invalidUploadFilenameError()
	}
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") ||
		strings.Contains(name, ":") {
		return "", invalidUploadFilenameError()
	}
	for _, r := range name {
		if unicode.In(r, unicode.Cc, unicode.Cf) {
			return "", invalidUploadFilenameError()
		}
	}
	if trimmed := strings.TrimRight(name, ". "); trimmed == "" || trimmed != name {
		return "", invalidUploadFilenameError()
	}
	if isWindowsReservedDevice(name) {
		return "", invalidUploadFilenameError()
	}
	return name, nil
}

func invalidUploadFilenameError() error {
	return shoal.NewError(shoal.ErrorInvalidArgument, "upload filename is not allowed")
}

func hasWindowsDrivePrefix(name string) bool {
	return len(name) >= 2 && name[1] == ':' &&
		((name[0] >= 'A' && name[0] <= 'Z') || (name[0] >= 'a' && name[0] <= 'z'))
}

func isWindowsReservedDevice(name string) bool {
	base := strings.ToUpper(strings.TrimSuffix(name, filepath.Ext(name)))
	switch base {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) &&
		base[3] >= '1' && base[3] <= '9' {
		return true
	}
	return false
}

func inspectUploadMediaType(name string, content []byte) (string, error) {
	detected := http.DetectContentType(content)
	if !strings.HasPrefix(detected, "text/") {
		return "", shoal.NewError(shoal.ErrorInvalidArgument, "upload media type is not supported")
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md", ".markdown":
		return explorer.MediaTypeMarkdown, nil
	case ".txt", ".text":
		return explorer.MediaTypeText, nil
	case ".c", ".cc", ".cpp", ".cs", ".css", ".go", ".h", ".hpp", ".html", ".java",
		".js", ".jsx", ".kt", ".lua", ".php", ".py", ".rb", ".rs", ".sh", ".sql",
		".swift", ".ts", ".tsx", ".vb", ".xml", ".yaml", ".yml":
		return explorer.MediaTypeSource, nil
	default:
		return "", shoal.NewError(
			shoal.ErrorInvalidArgument,
			fmt.Sprintf("upload media type is not supported for extension %q", filepath.Ext(name)),
		)
	}
}

func hasBinaryControlBytes(content []byte) bool {
	for _, b := range content {
		if b < 0x20 && b != '\t' && b != '\n' && b != '\r' {
			return true
		}
		if b == 0x7f {
			return true
		}
	}
	return false
}

func knownExplorerMediaType(mediaType string) bool {
	switch mediaType {
	case explorer.MediaTypeMarkdown, explorer.MediaTypeText, explorer.MediaTypeSource:
		return true
	default:
		return false
	}
}

func inspectUploadSkillFile(
	name string, mediaType string, content string,
) *SkillFileResult {
	if mediaType != explorer.MediaTypeMarkdown {
		return nil
	}
	// This branch is load-bearing; TestHTTPIngestRecognizesSkillFiles pins that
	// directory-safe names ending in "__SKILL.md" are still treated as expected
	// skill files after the browser removes path separators.
	expected := expectedSkillUploadName(name)
	values, frontmatter := parseSkillFrontmatter(content)
	skillName := strings.TrimSpace(values["name"])
	description := strings.TrimSpace(values["description"])
	recognized := expected && frontmatter && skillName != "" && description != ""
	if recognized {
		return &SkillFileResult{
			Expected:    true,
			Recognized:  true,
			Name:        skillName,
			Description: description,
			Message:     "Recognized agent skills file with YAML frontmatter name and description.",
		}
	}
	if expected {
		return &SkillFileResult{
			Expected:   true,
			Recognized: false,
			Message: "Expected an agent skills file because the filename is SKILL.md, " +
				"but YAML frontmatter must include non-empty name and description fields.",
		}
	}
	return nil
}

func expectedSkillUploadName(name string) bool {
	lower := strings.ToLower(name)
	return lower == "skill.md" || strings.HasSuffix(lower, "__skill.md")
}

func parseSkillFrontmatter(content string) (map[string]string, bool) {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return nil, false
	}
	values := make(map[string]string)
	for _, line := range strings.Split(normalized[len("---\n"):], "\n") {
		if line == "---" {
			return values, true
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(strings.ToLower(key))
		switch key {
		case "name", "description":
			values[key] = trimYAMLScalar(value)
		}
	}
	return nil, false
}

func trimYAMLScalar(value string) string {
	trimmed := strings.TrimSpace(value)
	if len(trimmed) >= 2 {
		quote := trimmed[0]
		if (quote == '"' || quote == '\'') && trimmed[len(trimmed)-1] == quote {
			return strings.TrimSpace(trimmed[1 : len(trimmed)-1])
		}
	}
	return trimmed
}
