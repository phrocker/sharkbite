// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/internal/decisiondatasets"
	inventoryset "github.com/phrocker/shoal-oss/internal/decisioninventoryset"
	inventory "github.com/phrocker/shoal-oss/internal/decisioninventorystore"
	outcomes "github.com/phrocker/shoal-oss/internal/decisionoutcomes"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func (r *registry) inventoryBinding(v *registered) inventory.Binding {
	return inventory.Binding{CoverageID: "compiled-sixteen-guarded-targets-v1", TargetID: v.target, TaskID: r.def.Task.ID(), PictureID: v.prediction.Request().PictureID(), SubjectID: v.fixture.ID, QuestionID: question}
}
func (r *registry) inventoryBindings() []inventory.Binding {
	var b []inventory.Binding
	for _, f := range fixtures(r.def.Name) {
		b = append(b, r.inventoryBinding(r.rows[f.ID]))
	}
	return b
}
func reportingRole(d auth.Decision) bool {
	return d.Subject() == "fixture-reporter" || d.Subject() == "fixture-reporter-secondary"
}
func crossReadRole(d auth.Decision) bool {
	return d.Subject() == "fixture-judge" || d.Subject() == "fixture-exporter"
}

type inventoryAdmissionAuthority struct{ r *registry }

func (a inventoryAdmissionAuthority) Resolve(_ context.Context, d auth.Decision, p decision.PredictionRecord, o decision.OutcomeObservation) (inventory.Binding, error) {
	v := a.r.byPrediction(p.Request().ID(), p.ID())
	if v == nil || !reportingRole(d) || o.PredictionID() != p.ID() || o.Config().Kind != decision.OutcomeCorrectness {
		return inventory.Binding{}, auth.ObjectNotFound()
	}
	return a.r.inventoryBinding(v), a.r.allCurrent(d, v, auth.OperationIngest)
}
func (a inventoryAdmissionAuthority) Verify(_ context.Context, d auth.Decision, b inventory.Binding, p decision.PredictionRecord, o decision.OutcomeObservation) error {
	v := a.r.byPrediction(p.Request().ID(), p.ID())
	if v == nil || !reportingRole(d) || b != a.r.inventoryBinding(v) || o.PredictionID() != p.ID() {
		return auth.ObjectNotFound()
	}
	return a.r.allCurrent(d, v, auth.OperationIngest)
}
func (r *registry) currentInventory(ctx context.Context, d auth.Decision, v *registered) (inventory.Snapshot, error) {
	if v == nil || !crossReadRole(d) {
		return inventory.Snapshot{}, auth.ObjectNotFound()
	}
	if e := r.permission(d, auth.OperationRead, v.fixture.ID); e != nil {
		return inventory.Snapshot{}, e
	}
	s, e := r.inventories.Load(ctx, inventory.Scope{Domain: d.AuthorizationDomain()}, r.inventoryBinding(v))
	if e != nil || !s.Complete() {
		return inventory.Snapshot{}, auth.ObjectNotFound()
	}
	return s, nil
}
func publishedEntry(s inventory.Snapshot, id shoal.ID) (inventory.Entry, bool) {
	for _, e := range s.Entries {
		if e.Intent.ReceiptID == id && e.State == inventory.Published {
			return e, true
		}
	}
	return inventory.Entry{}, false
}

type registeredOutcomeAuthority struct{ r *registry }

func (a registeredOutcomeAuthority) Resolve(ctx context.Context, d auth.Decision, ref outcomes.RegisteredReference) (outcomes.RegisteredBinding, error) {
	v := a.r.byTarget(ref.TargetID)
	s, e := a.r.currentInventory(ctx, d, v)
	if e != nil || s.ID != ref.InventoryID {
		return outcomes.RegisteredBinding{}, auth.ObjectNotFound()
	}
	entry, ok := publishedEntry(s, ref.ReceiptID)
	if !ok || entry.Intent.PredictionID != v.prediction.ID() || entry.Intent.RequestID != v.prediction.Request().ID() {
		return outcomes.RegisteredBinding{}, auth.ObjectNotFound()
	}
	if e = a.r.allCurrent(d, v, auth.OperationRead); e != nil {
		return outcomes.RegisteredBinding{}, e
	}
	return outcomes.RegisteredBinding{Reference: ref, CoverageID: s.Binding.CoverageID, Prediction: v.prediction}, nil
}
func (a registeredOutcomeAuthority) Verify(ctx context.Context, d auth.Decision, material []outcomes.RegisteredMaterial) error {
	if len(material) == 0 || !crossReadRole(d) {
		return auth.ObjectNotFound()
	}
	pins := map[shoal.ID]shoal.ID{}
	for _, m := range material {
		ref := m.Binding.Reference
		v := a.r.byTarget(ref.TargetID)
		s, e := a.r.currentInventory(ctx, d, v)
		if e != nil || s.ID != ref.InventoryID || m.Binding.CoverageID != s.Binding.CoverageID || m.Binding.Prediction.ID() != v.prediction.ID() {
			return auth.ObjectNotFound()
		}
		entry, ok := publishedEntry(s, ref.ReceiptID)
		if !ok {
			return auth.ObjectNotFound()
		}
		digest, e := inventory.ReceiptDigest(s.Binding, entry.Intent, m.Receipt)
		if e != nil || digest != entry.ReceiptDigest || m.Observation.ID() != entry.Intent.ObservationID {
			return auth.ObjectNotFound()
		}
		if e = a.r.allCurrent(d, v, auth.OperationRead); e != nil {
			return e
		}
		pins[ref.TargetID] = s.ID
	}
	for target, id := range pins {
		v := a.r.byTarget(target)
		s, e := a.r.currentInventory(ctx, d, v)
		if e != nil || s.ID != id {
			return auth.ObjectNotFound()
		}
	}
	for target := range pins {
		if e := a.r.permission(d, auth.OperationRead, a.r.byTarget(target).fixture.ID); e != nil {
			return e
		}
	}
	if ctx.Err() != nil || !crossReadRole(d) {
		return auth.ObjectNotFound()
	}
	return nil
}
func (r *registry) publishedReceipts(ctx context.Context, d auth.Decision, v *registered, s inventory.Snapshot) ([]outcomes.Receipt, error) {
	if !s.Complete() || s.Binding != r.inventoryBinding(v) {
		return nil, auth.ObjectNotFound()
	}
	out := make([]outcomes.Receipt, 0, len(s.Entries))
	for _, entry := range s.Entries {
		receipt, e := r.registeredOutcomes.ReadRegistered(ctx, outcomes.RegisteredReference{TargetID: v.target, InventoryID: s.ID, ReceiptID: entry.Intent.ReceiptID})
		if e != nil {
			return nil, e
		}
		digest, e := inventory.ReceiptDigest(s.Binding, entry.Intent, receipt)
		if e != nil || digest != entry.ReceiptDigest {
			return nil, auth.ObjectNotFound()
		}
		out = append(out, receipt)
	}
	return out, nil
}

type inventoryPin struct {
	Binding              inventory.Binding
	TargetID, SnapshotID shoal.ID
	Version              int64
}
type captureProof struct {
	VectorID shoal.ID
	Window   inventoryset.Window
	Pins     []inventoryPin
}

func proof(c inventoryset.Capture) captureProof {
	p := captureProof{VectorID: c.VectorID, Window: c.Window}
	for _, s := range c.Snapshots {
		p.Pins = append(p.Pins, inventoryPin{s.Binding, s.Binding.TargetID, s.ID, s.Version})
	}
	return p
}

type captureEntry struct {
	ReceiptID               shoal.ID
	ReceiptDigest           string
	ReceivedAt, PublishedAt time.Time
}
type historicalCapture struct {
	Snapshot          []byte
	Schema            int
	Kind              string
	Domain            []byte
	Capture           captureProof
	SnapshotUpdatedAt time.Time
	Entries           []captureEntry
}

const historicalCaptureLimit = 8 << 20

func capturePath(dir string, id shoal.ID) (string, error) {
	const prefix = "fixture-inventory-capture:"
	value := strings.TrimPrefix(string(id), prefix)
	decoded, e := hex.DecodeString(value)
	if !strings.HasPrefix(string(id), prefix) || e != nil || len(decoded) != 32 || hex.EncodeToString(decoded) != value {
		return "", auth.ObjectNotFound()
	}
	return filepath.Join(dir, "captures", value+".json"), nil
}
func (r *registry) retainCapture(c inventoryset.Capture) (shoal.ID, error) {
	if len(c.Snapshots) != 1 || !c.Snapshots[0].Complete() {
		return "", auth.ObjectNotFound()
	}
	s := c.Snapshots[0]
	snapshotBytes, e := inventory.EncodeSnapshot(inventory.Scope{Domain: []byte(domain)}, s)
	if e != nil {
		return "", e
	}
	h := historicalCapture{Schema: 1, Kind: "sealed-historical-inventory-capture", Domain: []byte(domain), Capture: proof(c), SnapshotUpdatedAt: s.UpdatedAt, Snapshot: snapshotBytes}
	for _, e := range s.Entries {
		h.Entries = append(h.Entries, captureEntry{e.Intent.ReceiptID, e.ReceiptDigest, e.ReceivedAt, e.PublishedAt})
	}
	raw, e := json.Marshal(h)
	if e != nil || len(raw) > historicalCaptureLimit {
		return "", auth.ObjectNotFound()
	}
	id := shoal.ID("fixture-inventory-capture:" + hash(raw))
	path, e := capturePath(r.dir, id)
	if e != nil {
		return "", e
	}
	if e = writeExclusive(path, raw); e != nil {
		return "", e
	}
	return id, nil
}
func (r *registry) verifyHistoricalCapture(ctx context.Context, d auth.Decision, v *registered, b decision.AdjudicationBasis) error {
	id := b.Config().EnumerationID
	path, e := capturePath(r.dir, id)
	if e != nil {
		return e
	}
	raw, e := inquiryRead(path, historicalCaptureLimit)
	if e != nil || string(id) != "fixture-inventory-capture:"+hash(raw) || !captureShape(raw) {
		return auth.ObjectNotFound()
	}
	var h historicalCapture
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&h) != nil {
		return auth.ObjectNotFound()
	}
	if _, e = decoder.Token(); e != io.EOF {
		return auth.ObjectNotFound()
	}
	canonical, e := json.Marshal(h)
	if e != nil || !bytes.Equal(raw, canonical) || h.Schema != 1 || h.Kind != "sealed-historical-inventory-capture" || !bytes.Equal(h.Domain, d.AuthorizationDomain()) || len(h.Capture.Pins) != 1 || h.Capture.Pins[0].Binding != r.inventoryBinding(v) || len(h.Entries) > inventory.MaxEntries || len(h.Entries) != len(b.Config().Outcomes) || h.SnapshotUpdatedAt.After(b.Config().Cutoff) || h.Capture.Window.CompletedAt.After(b.Config().Cutoff) {
		return auth.ObjectNotFound()
	}
	originalSnapshot, decodeErr := inventory.DecodeSnapshot(inventory.Scope{Domain: h.Domain}, h.Snapshot)
	if decodeErr != nil || !originalSnapshot.Complete() || originalSnapshot.Binding != r.inventoryBinding(v) || h.Capture.Pins[0].SnapshotID != originalSnapshot.ID || h.Capture.Pins[0].Version != originalSnapshot.Version || h.Capture.Pins[0].TargetID != v.target || len(h.Entries) != len(originalSnapshot.Entries) || h.SnapshotUpdatedAt != originalSnapshot.UpdatedAt {
		return auth.ObjectNotFound()
	}
	for i, e := range originalSnapshot.Entries {
		summary := h.Entries[i]
		if summary.ReceiptID != e.Intent.ReceiptID || summary.ReceiptDigest != e.ReceiptDigest || summary.ReceivedAt != e.ReceivedAt || summary.PublishedAt != e.PublishedAt {
			return auth.ObjectNotFound()
		}
	}
	current, e := r.currentInventory(ctx, d, v)
	if e != nil {
		return e
	}
	outcomesByID := map[shoal.ID]decision.BasisOutcome{}
	for _, o := range b.Config().Outcomes {
		outcomesByID[o.ReceiptID] = o
	}
	seen := map[shoal.ID]bool{}
	for _, original := range h.Entries {
		entry, ok := publishedEntry(current, original.ReceiptID)
		if !ok || seen[original.ReceiptID] || entry.ReceiptDigest != original.ReceiptDigest || entry.ReceivedAt != original.ReceivedAt || entry.PublishedAt != original.PublishedAt || original.PublishedAt.After(b.Config().Cutoff) {
			return auth.ObjectNotFound()
		}
		seen[original.ReceiptID] = true
		receipt, e := r.registeredOutcomes.ReadRegistered(ctx, outcomes.RegisteredReference{TargetID: v.target, InventoryID: current.ID, ReceiptID: original.ReceiptID})
		if e != nil {
			return e
		}
		digest, e := inventory.ReceiptDigest(current.Binding, entry.Intent, receipt)
		if e != nil || digest != original.ReceiptDigest {
			return auth.ObjectNotFound()
		}
		expected := basisOutcome(r, v, receipt)
		if !reflect.DeepEqual(outcomesByID[receipt.ID], expected) {
			return auth.ObjectNotFound()
		}
	}
	return nil
}
func basisOutcome(r *registry, v *registered, receipt outcomes.Receipt) decision.BasisOutcome {
	who := decision.BasisIdentity{SubjectID: []byte(receipt.SubmitterID), ActorID: []byte(receipt.ActorID), ClientID: []byte(receipt.ClientID)}
	for _, id := range receipt.OnBehalfOf {
		who.OnBehalfOf = append(who.OnBehalfOf, []byte(id))
	}
	return decision.BasisOutcome{ReceiptID: receipt.ID, ObservationID: receipt.ObservationID, RequestID: receipt.ObservationConfig.RequestID, PredictionID: receipt.ObservationConfig.PredictionID, TaskID: r.def.Task.ID(), PictureID: v.prediction.Request().PictureID(), SubjectID: v.fixture.ID, QuestionID: question, Kind: decision.OutcomeCorrectness, Reporter: who, ReceivedAt: receipt.ReceivedAt, Supersedes: receipt.ObservationConfig.Supersedes}
}
func exportSession(ctx context.Context, r *registry, resolver auth.Resolver, id shoal.ID) (*decisiondatasets.Service, *cohortAuthority, error) {
	if id != r.cohort.ID || !r.sealed {
		return nil, nil, auth.ObjectNotFound()
	}
	d, e := resolver.Resolve(ctx)
	if e != nil || d.Subject() != "fixture-exporter" || !r.training {
		return nil, nil, auth.ObjectNotFound()
	}
	for _, v := range r.rows {
		if e = r.permission(d, auth.OperationRead, v.fixture.ID); e != nil {
			return nil, nil, e
		}
	}
	capture, e := r.inventorySets.CaptureSet(ctx, inventory.Scope{Domain: d.AuthorizationDomain()}, r.inventoryBindings())
	if e != nil {
		return nil, nil, auth.ObjectNotFound()
	}
	c := cloneCohort(r.cohort)
	c.InventoryID = capture.VectorID
	authority := &cohortAuthority{r: r, cohort: c, initialCapture: capture}
	service, e := decisiondatasets.New(decisiondatasets.Config{Resolver: resolver, Authority: authority, Clock: r.now})
	return service, authority, e
}
func freshExport(ctx context.Context, r *registry, resolver auth.Resolver, id shoal.ID) (decisiondatasets.Bundle, error) {
	service, _, e := exportSession(ctx, r, resolver, id)
	if e != nil {
		return decisiondatasets.Bundle{}, e
	}
	return service.Export(ctx, id)
}

type freshDatasetExporter struct {
	r        *registry
	resolver auth.Resolver
}

func (e freshDatasetExporter) Export(ctx context.Context, id shoal.ID) (decisiondatasets.Bundle, error) {
	return freshExport(ctx, e.r, e.resolver, id)
}

func captureShape(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	nodes := 0
	var walk func(int, string) bool
	walk = func(depth int, name string) bool {
		nodes++
		if nodes > 100000 || depth > 16 {
			return false
		}
		v, e := d.Token()
		if e != nil {
			return false
		}
		delim, ok := v.(json.Delim)
		if !ok {
			return true
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				v, e := d.Token()
				key, ok := v.(string)
				if e != nil || !ok || seen[key] || len(seen) >= 32 {
					return false
				}
				seen[key] = true
				if !walk(depth+1, key) {
					return false
				}
			}
			v, e = d.Token()
			return e == nil && v == json.Delim('}')
		case '[':
			limit := 256
			if name == "Pins" {
				limit = 1
			}
			if name == "OnBehalfOf" {
				limit = 64
			}
			count := 0
			for d.More() {
				count++
				if count > limit || !walk(depth+1, "") {
					return false
				}
			}
			v, e = d.Token()
			return e == nil && v == json.Delim(']')
		default:
			return false
		}
	}
	if !walk(0, "") {
		return false
	}
	_, e := d.Token()
	return e == io.EOF
}
