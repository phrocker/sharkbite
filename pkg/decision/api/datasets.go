// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const MaxDatasetBytes = 16 << 20
const MaxDatasetManifestBytes = 4 << 20
const MaxDatasetResponseBytes = 28 << 20

// DatasetExport is a currently authorized snapshot serialization. Its transport
// validation binds bytes and cohort identity, not training eligibility. A pinned
// trainer must separately perform full manifest admission before using any row.
type DatasetExport struct {
	CohortID          shoal.ID
	Dataset, Manifest []byte
	ManifestSHA256    string
}
type DatasetProvider interface {
	Export(context.Context, shoal.ID) (DatasetExport, error)
}
type datasetWire struct {
	Schema         int    `json:"schema"`
	CohortID       string `json:"cohort_id"`
	Dataset        string `json:"dataset"`
	Manifest       string `json:"manifest"`
	ManifestSHA256 string `json:"manifest_sha256"`
}

func datasetHash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func datasetID(id shoal.ID) bool {
	return shoal.ValidateRequiredID("cohort", id) == nil && utf8.ValidString(string(id))
}
func datasetInvalid() error { return errors.New("invalid dataset export") }
func (v DatasetExport) Validate() error {
	if !datasetID(v.CohortID) || len(v.Dataset) == 0 || len(v.Dataset) > MaxDatasetBytes || len(v.Manifest) == 0 || len(v.Manifest) > MaxDatasetManifestBytes || v.ManifestSHA256 != datasetHash(v.Manifest) {
		return datasetInvalid()
	}
	var fields map[string]json.RawMessage
	if err := decodeStrict(v.Manifest, &fields); err != nil || fields == nil {
		return datasetInvalid()
	}
	// Other manifest fields are intentionally interpreted only by the pinned
	// training adapter. Strict parsing still rejects duplicate keys at any depth.
	var schema int
	var kind, cohort, digest string
	var size int
	for _, f := range []struct {
		name  string
		value any
	}{{"schema", &schema}, {"kind", &kind}, {"cohort_id", &cohort}, {"dataset_sha256", &digest}, {"dataset_bytes", &size}} {
		for name := range fields {
			if name != f.name && strings.EqualFold(name, f.name) {
				return datasetInvalid()
			}
		}
		raw, ok := fields[f.name]
		if !ok || string(raw) == "null" || json.Unmarshal(raw, f.value) != nil {
			return datasetInvalid()
		}
	}
	if schema != 1 || kind != "authorized-numeric-training-export" || cohort != string(v.CohortID) || digest != datasetHash(v.Dataset) || size != len(v.Dataset) {
		return datasetInvalid()
	}
	return nil
}
func EncodeDatasetExport(v DatasetExport) ([]byte, error) {
	if err := v.Validate(); err != nil {
		return nil, err
	}
	b, err := json.Marshal(datasetWire{1, EncodeID(v.CohortID), base64.StdEncoding.EncodeToString(v.Dataset), base64.StdEncoding.EncodeToString(v.Manifest), v.ManifestSHA256})
	if err != nil || len(b) > MaxDatasetResponseBytes {
		return nil, datasetInvalid()
	}
	return b, nil
}
func decodeDatasetBytes(s string, max int) ([]byte, error) {
	if len(s) == 0 || len(s) > base64.StdEncoding.EncodedLen(max) {
		return nil, datasetInvalid()
	}
	b, err := base64.StdEncoding.Strict().DecodeString(s)
	if err != nil || len(b) == 0 || len(b) > max || base64.StdEncoding.EncodeToString(b) != s {
		return nil, datasetInvalid()
	}
	return b, nil
}
func DecodeDatasetExport(raw []byte) (DatasetExport, error) {
	var out DatasetExport
	if len(raw) == 0 || len(raw) > MaxDatasetResponseBytes {
		return out, datasetInvalid()
	}
	var wire datasetWire
	if err := decodeStrict(raw, &wire); err != nil || wire.Schema != 1 {
		return out, datasetInvalid()
	}
	id, err := DecodeID(wire.CohortID)
	if err != nil || !datasetID(id) {
		return out, datasetInvalid()
	}
	dataset, err := decodeDatasetBytes(wire.Dataset, MaxDatasetBytes)
	if err != nil {
		return out, err
	}
	manifest, err := decodeDatasetBytes(wire.Manifest, MaxDatasetManifestBytes)
	if err != nil {
		return out, err
	}
	out = DatasetExport{id, dataset, manifest, wire.ManifestSHA256}
	if err = out.Validate(); err != nil {
		return DatasetExport{}, err
	}
	return out, nil
}
