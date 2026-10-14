// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"unicode/utf8"
)

const maxSourceBytes = 8 << 20

type sourceEvidence struct {
	ID     string
	Bytes  []byte
	Path   string
	PR     int
	Digest string
}
type sourceRow struct {
	ID             string `json:"id"`
	SourceFile     string `json:"source_file"`
	SourceSHA256   string `json:"source_sha256"`
	Bytes          int    `json:"bytes"`
	Path           string `json:"path"`
	PR             int    `json:"pr"`
	Representation string `json:"representation"`
}
type sourceManifest struct {
	Schema                       int         `json:"schema"`
	Kind                         string      `json:"kind"`
	NumericManifestSHA256        string      `json:"numeric_manifest_sha256"`
	NumericModelSHA256           string      `json:"numeric_model_sha256"`
	ArchiveSHA256                string      `json:"archive_sha256"`
	OriginalInputsSHA256         string      `json:"original_inputs_sha256"`
	BuilderSHA256                string      `json:"builder_sha256"`
	Rows                         []sourceRow `json:"rows"`
	HistoricalTimestampsVerified bool        `json:"historical_timestamps_verified"`
	OptimizationEnabled          bool        `json:"optimization_enabled"`
	Action                       string      `json:"action"`
}

func sourceShape(raw []byte) error {
	object, e := exactObject(raw, map[string]string{"schema": "number", "kind": "string", "numeric_manifest_sha256": "string", "numeric_model_sha256": "string", "archive_sha256": "string", "original_inputs_sha256": "string", "builder_sha256": "string", "rows": "array", "historical_timestamps_verified": "bool", "optimization_enabled": "bool", "action": "string"})
	if e != nil {
		return e
	}
	var rows []json.RawMessage
	if e = json.Unmarshal(object["rows"], &rows); e != nil {
		return e
	}
	for _, row := range rows {
		if _, e = exactObject(row, map[string]string{"id": "string", "source_file": "string", "source_sha256": "string", "bytes": "number", "path": "string", "pr": "number", "representation": "string"}); e != nil {
			return e
		}
	}
	return nil
}

// loadSources checks the trusted local export's exact bytes and numeric binding.
// A digest is not source authorization. The service checks current grants against
// these registered bytes independently; source times are local import times.
func loadSources(dir, expectedSHA string, b *loadedBundle) ([]sourceEvidence, error) {
	if !validDigest(expectedSHA) || b == nil {
		return nil, errors.New("source manifest pin required")
	}
	root, e := os.OpenRoot(dir)
	if e != nil {
		return nil, e
	}
	defer root.Close()
	raw, e := readBound(root, "source-manifest.json", 1<<20)
	if e != nil {
		return nil, e
	}
	if digest(raw) != expectedSHA {
		return nil, errors.New("source manifest SHA256 mismatch")
	}
	var m sourceManifest
	if e = strictJSONShape(raw, &m, sourceShape); e != nil {
		return nil, e
	}
	if m.Schema != 1 || m.Kind != "frozen-code-source-registration" || m.NumericManifestSHA256 != b.ManifestSHA256 || m.NumericModelSHA256 != b.Manifest.ModelSHA256 || m.ArchiveSHA256 != archiveHash || m.OriginalInputsSHA256 != originalMembers["reserved/inputs-frozen.json"] || !validDigest(m.BuilderSHA256) || m.HistoricalTimestampsVerified || m.OptimizationEnabled || m.Action != "full_review" || len(m.Rows) != len(b.Manifest.Rows) {
		return nil, errors.New("source registration binding mismatch")
	}
	st, e := root.Lstat("sources")
	if e != nil {
		return nil, e
	}
	if !st.IsDir() {
		return nil, errors.New("sources is not a directory")
	}
	result := make([]sourceEvidence, 0, len(m.Rows))
	total := 0
	for i, row := range m.Rows {
		if row.ID != b.Manifest.Rows[i].ID || row.SourceFile != fmt.Sprintf("sources/%04d.txt", i) || !validDigest(row.SourceSHA256) || row.Bytes < 1 || row.Bytes > maxSourceBytes-total || row.Path == "" || len(row.Path) > 4096 || row.PR < 1 || row.Representation != "cached-v9-code-text" {
			return nil, errors.New("invalid registered source")
		}
		raw, e := readBound(root, row.SourceFile, int64(row.Bytes))
		if e != nil {
			return nil, e
		}
		if len(raw) != row.Bytes || digest(raw) != row.SourceSHA256 || !utf8.Valid(raw) {
			return nil, errors.New("source bytes mismatch")
		}
		total += len(raw)
		result = append(result, sourceEvidence{row.ID, raw, row.Path, row.PR, row.SourceSHA256})
	}
	return result, nil
}
