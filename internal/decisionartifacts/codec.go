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

package decisionartifacts

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"sort"
	"time"

	"github.com/phrocker/shoal-oss/internal/decisionservice"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// SourceBytes retains a complete original artifact revision, not just its quote.
// ID addresses the source observation entry in PictureConfig.Sources.
type SourceBytes struct {
	ID    shoal.ID
	Bytes []byte
}
type Record struct {
	Bundle  decisionservice.Bundle
	Sources []SourceBytes
}

// Bounds cover aggregate source bytes and the entire encoded row, respectively.
const MaxSourceBytes = 8 * 1024 * 1024
const MaxStoredBytes = 48 * 1024 * 1024

type anchorWire struct {
	ID         shoal.ID
	Kind       inference.AnchorKind
	Citation   document.Citation
	Quote      []byte
	Path       graph.Path
	Assertions []interaction.AssertionReference
}
type packWire struct {
	ID                  shoal.ID
	Query               []byte
	Evidence            []anchorWire
	SchemaID, VersionID shoal.ID
	SnapshotID          shoal.ID
	AsOf                time.Time
	Fingerprint         shoal.ID
	ExpiresAt           time.Time
	Metadata            shoal.Metadata
}
type recordWire struct {
	RequestID      shoal.ID
	Task           decision.TaskConfig
	Picture        decision.PictureConfig
	Predictor      decision.PredictorConfig
	Request        decision.RequestConfig
	Pack           packWire
	EvidencePolicy decision.EvidencePolicyConfig
	Ranking        decision.RankingConfig
	TaskResource   auth.ResourceRequest
	Input          []byte
	Sources        []SourceBytes
}
type envelope struct {
	Schema   int
	Payload  json.RawMessage
	Checksum string
}

func wire(r Record) (recordWire, error) {
	b := r.Bundle
	if b.Request.Validate() != nil || b.EvidencePolicy.Validate() != nil || b.RankingPlan.ValidateTask(b.Request.Task()) != nil || b.EvidencePolicy.ID() != b.Request.Task().Config().EvidencePolicyID {
		return recordWire{}, invalid()
	}
	if len(b.Input) == 0 || len(b.Input) > inference.MaxContextPackBytes || digest(b.Input) != b.Request.Picture().Config().InputDigest {
		return recordWire{}, invalid()
	}
	resource, err := b.TaskResource.Normalize()
	if err != nil || resource.ObjectID != b.Request.TaskID() || len(resource.SourceID) == 0 || len(resource.PolicyID) == 0 {
		return recordWire{}, invalid()
	}
	pc := b.Request.Picture().Config()
	if len(r.Sources) != len(pc.Sources) {
		return recordWire{}, invalid()
	}
	sources := make(map[shoal.ID][]byte, len(r.Sources))
	total := 0
	for _, src := range r.Sources {
		if _, exists := sources[src.ID]; exists || len(src.Bytes) > MaxSourceBytes-total {
			return recordWire{}, invalid()
		}
		total += len(src.Bytes)
		sources[src.ID] = src.Bytes
	}
	for _, src := range pc.Sources {
		raw, ok := sources[src.ID]
		if !ok || digest(raw) != src.Digest {
			return recordWire{}, invalid()
		}
	}
	pack := b.Request.Picture().ContextPack()
	pw := packWire{ID: pack.ID(), Query: []byte(pack.Query()), SnapshotID: pack.Snapshot().ID(), AsOf: pack.Snapshot().AsOf(), Fingerprint: pack.Authorization().Fingerprint(), ExpiresAt: pack.Authorization().ExpiresAt(), Metadata: pack.Metadata()}
	if ontology, ok := pack.Ontology(); ok {
		pw.SchemaID = ontology.SchemaID()
		pw.VersionID = ontology.VersionID()
	}
	// Index associations once rather than rescanning the full inventory for
	// every anchor (large bounded packs must not trigger quadratic work).
	anchorSources := make(map[shoal.ID]map[shoal.ID]struct{})
	for _, subject := range pc.Subjects {
		for _, id := range subject.EvidenceIDs {
			if anchorSources[id] == nil {
				anchorSources[id] = make(map[shoal.ID]struct{})
			}
			anchorSources[id][subject.SourceID] = struct{}{}
		}
	}
	for _, a := range pack.Evidence() {
		aw := anchorWire{ID: a.ID(), Kind: a.Kind()}
		if citation, quote, ok := a.Document(); ok {
			aw.Citation = citation
			aw.Quote = []byte(quote)
			// Quotes must be exact bytes of every source revision to which their
			// subjects bind them, including unrequested subjects in the shared input.
			for sourceID := range anchorSources[a.ID()] {
				raw := sources[sourceID]
				start, end := citation.Range.Start.Offset, citation.Range.End.Offset
				if start < 0 || end < start || end > int64(len(raw)) || !bytes.Equal(raw[start:end], aw.Quote) {
					return recordWire{}, invalid()
				}
			}
		} else {
			aw.Path, _ = a.Path()
			ref, err := a.EvidenceReference()
			if err != nil {
				return recordWire{}, invalid()
			}
			aw.Assertions = ref.Assertions
		}
		pw.Evidence = append(pw.Evidence, aw)
	}
	retained := append([]SourceBytes(nil), r.Sources...)
	sort.Slice(retained, func(i, j int) bool { return retained[i].ID < retained[j].ID })
	return recordWire{RequestID: b.Request.ID(), Task: b.Request.Task().Config(), Picture: pc, Predictor: b.Request.Predictor().Config(), Request: b.Request.Config(), Pack: pw, EvidencePolicy: b.EvidencePolicy.Config(), Ranking: b.RankingPlan.Config(), TaskResource: resource, Input: b.Input, Sources: retained}, nil
}
func restore(w recordWire) (Record, error) {
	task, err := decision.NewTaskSpec(w.Task)
	if err != nil {
		return Record{}, err
	}
	snapshot, err := inference.NewSnapshotPin(w.Pack.SnapshotID, w.Pack.AsOf)
	if err != nil {
		return Record{}, err
	}
	pin, err := inference.NewAuthPin(w.Pack.Fingerprint, w.Pack.ExpiresAt)
	if err != nil {
		return Record{}, err
	}
	var ontology *inference.OntologyIdentity
	if w.Pack.SchemaID != "" || w.Pack.VersionID != "" {
		o, err := inference.NewOntologyIdentityFromIDs(w.Pack.SchemaID, w.Pack.VersionID)
		if err != nil {
			return Record{}, err
		}
		ontology = &o
	}
	var anchors []inference.EvidenceAnchor
	for _, aw := range w.Pack.Evidence {
		var a inference.EvidenceAnchor
		switch aw.Kind {
		case inference.AnchorDocument:
			a, err = inference.NewDocumentAnchor(aw.Citation, string(aw.Quote))
		case inference.AnchorGraph:
			a, err = inference.NewGraphAnchorWithAssertions(aw.Path, aw.Assertions)
		default:
			return Record{}, invalid()
		}
		if err != nil || a.ID() != aw.ID {
			return Record{}, invalid()
		}
		anchors = append(anchors, a)
	}
	pack, err := inference.NewContextPack(string(w.Pack.Query), anchors, ontology, snapshot, pin, w.Pack.Metadata)
	if err != nil || pack.ID() != w.Pack.ID {
		return Record{}, invalid()
	}
	picture, err := decision.NewPictureManifest(pack, w.Picture)
	if err != nil {
		return Record{}, err
	}
	predictor, err := decision.NewPredictorIdentity(w.Predictor)
	if err != nil {
		return Record{}, err
	}
	request, err := decision.NewDecisionRequest(task, picture, predictor, w.Request)
	if err != nil || request.ID() != w.RequestID {
		return Record{}, invalid()
	}
	policy, err := decision.NewEvidencePolicy(w.EvidencePolicy)
	if err != nil {
		return Record{}, err
	}
	ranking, err := decision.NewRankingPlan(w.Ranking)
	if err != nil {
		return Record{}, err
	}
	return Record{Bundle: decisionservice.Bundle{Request: request, EvidencePolicy: policy, RankingPlan: ranking, TaskResource: w.TaskResource, Input: w.Input}, Sources: w.Sources}, nil
}
func encode(r Record) ([]byte, error) {
	w, err := wire(r)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(w)
	if err != nil {
		return nil, invalid()
	}
	b, err := json.Marshal(envelope{1, payload, digest(payload)})
	if err != nil || len(b) > MaxStoredBytes {
		return nil, invalid()
	}
	return b, nil
}
func decode(b []byte) (Record, error) {
	if len(b) == 0 || len(b) > MaxStoredBytes {
		return Record{}, ErrUnavailable
	}
	var e envelope
	if strict(b, &e) != nil || e.Schema != 1 || digest(e.Payload) != e.Checksum {
		return Record{}, ErrUnavailable
	}
	var w recordWire
	if strict(e.Payload, &w) != nil {
		return Record{}, ErrUnavailable
	}
	r, err := restore(w)
	if err != nil {
		return Record{}, ErrUnavailable
	}
	canonical, err := encode(r)
	if err != nil || !bytes.Equal(b, canonical) {
		return Record{}, ErrUnavailable
	}
	return r, nil
}
func strict(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return ErrUnavailable
	}
	return nil
}
func digest(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func invalid() error {
	return shoal.NewError(shoal.ErrorInvalidArgument, "invalid retained decision artifacts")
}
