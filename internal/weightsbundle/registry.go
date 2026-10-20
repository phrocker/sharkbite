// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package weightsbundle

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/phrocker/shoal-oss/internal/storage"
)

// Store is the backend shape a durable registry needs: exclusive creation
// for append-only records and listing to reload them. Every storage backend
// in internal/storage except HDFS provides it.
type Store interface {
	storage.ExclusiveCreator
	storage.Lister
}

// Layout under the registry prefix. Each directory is flat so Lister's
// "directly under" semantics enumerate it.
const (
	dirReleases = "releases"
	dirPointers = "pointers"
	dirLineage  = "lineage"
	dirEvents   = "events"
)

// ErrNoActiveRelease reports that nothing has been promoted.
var ErrNoActiveRelease = errors.New("no active release")

// ErrIneligible reports a release that lineage events have withdrawn from
// new use. The message carries the reason recorded against the release.
var ErrIneligible = errors.New("release ineligible for new use")

// ErrConflict reports that another writer advanced the serving pointer
// first. The caller re-reads the active release and decides again.
var ErrConflict = errors.New("release pointer conflict")

// DatasetLineage names the sources a release's dataset manifest included so
// a later withdrawal can find every release that used them.
type DatasetLineage struct {
	DatasetManifestSHA256 string   `json:"dataset_manifest_sha256"`
	SourceIDs             []string `json:"source_ids"`
}

// Eligibility is the current standing of a release for new use.
type Eligibility struct {
	Status   string    `json:"status"`
	SourceID string    `json:"source_id,omitempty"`
	At       time.Time `json:"at,omitempty"`
	Reason   string    `json:"reason,omitempty"`
}

// Event is an append-only note recorded against a release: a withdrawal,
// a load failure, or an import. Kind is free text chosen by the recorder.
type Event struct {
	Sequence  int64     `json:"sequence"`
	Kind      string    `json:"kind"`
	ReleaseID string    `json:"release_id"`
	SourceID  string    `json:"source_id,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	At        time.Time `json:"at"`
}

// EventLoadFailure is the event kind a provider records when a promoted
// bundle fails verification at load.
const EventLoadFailure = "load_failure"

// EventSourceWithdrawn is recorded once per affected release.
const EventSourceWithdrawn = "source_withdrawn"

type pointer struct {
	Schema    int    `json:"schema"`
	Kind      string `json:"kind"`
	Sequence  int64  `json:"sequence"`
	ReleaseID string `json:"release_id"`
	Previous  string `json:"previous_release_id"`
}

// Registry owns the single serving pointer and the append-only history of
// releases. Reads of the active release are one atomic load, so a request
// resolves exactly one release even while a promotion or rollback commits.
type Registry struct {
	mu          sync.Mutex
	store       Store
	prefix      string
	releases    map[string]Release
	order       []string
	lineage     map[string]DatasetLineage
	eligibility map[string]Eligibility
	events      []Event
	sequence    int64
	active      atomic.Pointer[Release]
}

// NewRegistry returns an in-memory registry with no durable store.
func NewRegistry() *Registry {
	return &Registry{releases: map[string]Release{}, lineage: map[string]DatasetLineage{}, eligibility: map[string]Eligibility{}}
}

// OpenRegistry loads a durable registry from store under prefix, replaying
// every release, pointer, lineage record, and event. A pointer naming an
// unknown release, or a release whose identity fails, refuses to open.
func OpenRegistry(ctx context.Context, store Store, prefix string) (*Registry, error) {
	if store == nil {
		return nil, errors.New("registry store is required")
	}
	r := NewRegistry()
	r.store, r.prefix = store, strings.TrimRight(prefix, "/")
	releases, err := r.list(ctx, dirReleases)
	if err != nil {
		return nil, err
	}
	for _, p := range releases {
		raw, err := readObject(ctx, store, p, MaxRecordBytes)
		if err != nil {
			return nil, fmt.Errorf("registry: %w", err)
		}
		record, err := DecodeRecord(raw)
		if err != nil {
			return nil, fmt.Errorf("registry: %s: %w", p, err)
		}
		release, err := ReleaseFromRecord(record)
		if err != nil {
			return nil, fmt.Errorf("registry: %s: %w", p, err)
		}
		if path.Base(p) != release.ID+".json" {
			return nil, fmt.Errorf("registry: %s does not hold release %s", p, release.ID)
		}
		r.releases[release.ID] = release
		r.eligibility[release.ID] = Eligibility{Status: EligibilityEligible}
	}
	lineage, err := r.list(ctx, dirLineage)
	if err != nil {
		return nil, err
	}
	for _, p := range lineage {
		raw, err := readObject(ctx, store, p, MaxRecordBytes)
		if err != nil {
			return nil, fmt.Errorf("registry: %w", err)
		}
		var l DatasetLineage
		if err := strictDecode(raw, &l); err != nil {
			return nil, fmt.Errorf("registry: %s: %w", p, err)
		}
		id := strings.TrimSuffix(path.Base(p), ".json")
		if _, ok := r.releases[id]; !ok {
			return nil, fmt.Errorf("registry: lineage for unknown release %s", id)
		}
		r.lineage[id] = l
	}
	pointers, err := r.list(ctx, dirPointers)
	if err != nil {
		return nil, err
	}
	sort.Strings(pointers)
	var previous string
	for i, p := range pointers {
		raw, err := readObject(ctx, store, p, MaxRecordBytes)
		if err != nil {
			return nil, fmt.Errorf("registry: %w", err)
		}
		var ptr pointer
		if err := strictDecode(raw, &ptr); err != nil {
			return nil, fmt.Errorf("registry: %s: %w", p, err)
		}
		if ptr.Schema != 1 || ptr.Kind != "release_pointer" || ptr.Sequence != int64(i+1) || ptr.Previous != previous || path.Base(p) != sequenceName(ptr.Sequence) {
			return nil, fmt.Errorf("registry: pointer %s out of sequence", p)
		}
		release, ok := r.releases[ptr.ReleaseID]
		if !ok {
			return nil, fmt.Errorf("registry: pointer %s names unknown release %s", p, ptr.ReleaseID)
		}
		r.order = append(r.order, release.ID)
		r.sequence = ptr.Sequence
		previous = ptr.ReleaseID
		active := release
		r.active.Store(&active)
	}
	events, err := r.list(ctx, dirEvents)
	if err != nil {
		return nil, err
	}
	sort.Strings(events)
	for _, p := range events {
		raw, err := readObject(ctx, store, p, MaxRecordBytes)
		if err != nil {
			return nil, fmt.Errorf("registry: %w", err)
		}
		var ev Event
		if err := strictDecode(raw, &ev); err != nil {
			return nil, fmt.Errorf("registry: %s: %w", p, err)
		}
		r.events = append(r.events, ev)
		r.applyEvent(ev)
	}
	return r, nil
}

func (r *Registry) list(ctx context.Context, dir string) ([]string, error) {
	if r.store == nil {
		return nil, nil
	}
	out, err := r.store.List(ctx, r.path(dir)+"/")
	if err != nil {
		return nil, fmt.Errorf("registry: list %s: %w", dir, err)
	}
	filtered := out[:0]
	for _, p := range out {
		if strings.HasSuffix(p, ".json") {
			filtered = append(filtered, p)
		}
	}
	return filtered, nil
}

func (r *Registry) path(parts ...string) string {
	if r.prefix == "" {
		return path.Join(parts...)
	}
	return r.prefix + "/" + path.Join(parts...)
}

func sequenceName(seq int64) string { return fmt.Sprintf("%020d.json", seq) }

// Active returns the release every new request resolves right now.
func (r *Registry) Active() (Release, bool) {
	p := r.active.Load()
	if p == nil {
		return Release{}, false
	}
	return *p, true
}

// Get returns a release by identity.
func (r *Registry) Get(id string) (Release, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rel, ok := r.releases[id]
	return rel, ok
}

// History returns the pointer sequence: every release in the order it became
// active, including rollbacks.
func (r *Registry) History() []Release {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Release, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.releases[id])
	}
	return out
}

// Resolve returns the active release for a new request, refusing one that
// lineage events have made ineligible.
func (r *Registry) Resolve() (Release, error) {
	rel, ok := r.Active()
	if !ok {
		return Release{}, ErrNoActiveRelease
	}
	if e := r.EligibilityOf(rel.ID); e.Status != EligibilityEligible {
		return Release{}, fmt.Errorf("%w: %s: %s", ErrIneligible, e.Status, e.Reason)
	}
	return rel, nil
}

// EligibilityOf reports a release's standing.
func (r *Registry) EligibilityOf(id string) Eligibility {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.eligibility[id]
	if !ok {
		return Eligibility{Status: EligibilityEligible}
	}
	return e
}

// Lineage returns the dataset lineage recorded at promotion.
func (r *Registry) Lineage(id string) (DatasetLineage, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.lineage[id]
	return l, ok
}

// Events returns every recorded event in sequence.
func (r *Registry) Events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

// Promote seals and commits a release. The input's Predecessor must be the
// current active release (or nil when none exists): a promotion decided
// against a stale pointer is refused rather than silently reordered. The
// lineage's manifest pin must match the candidate's recorded pin.
func (r *Registry) Promote(ctx context.Context, in PromotionInput, lineage DatasetLineage) (Release, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	active := r.active.Load()
	switch {
	case active == nil && in.Predecessor != nil:
		return Release{}, fmt.Errorf("%w: no active release to succeed", ErrConflict)
	case active != nil && (in.Predecessor == nil || in.Predecessor.ID != active.ID):
		return Release{}, fmt.Errorf("%w: active release is %s", ErrConflict, active.ID)
	}
	if pin := optionalString(in.Candidate, "dataset_manifest_sha256"); pin != nil && lineage.DatasetManifestSHA256 != *pin {
		return Release{}, refuse("dataset lineage does not match the candidate's manifest pin")
	}
	release, err := Promote(in)
	if err != nil {
		return Release{}, err
	}
	if _, exists := r.releases[release.ID]; exists {
		return Release{}, fmt.Errorf("%w: release %s already recorded", ErrConflict, release.ID)
	}
	if err := r.commit(ctx, release, &lineage); err != nil {
		return Release{}, err
	}
	return release, nil
}

// Rollback restores the exact prior bundle under a new active release. The
// approval must name the current active release; the prior release must be
// recorded and still eligible.
func (r *Registry) Rollback(ctx context.Context, priorID string, approval Record) (Release, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	active := r.active.Load()
	if active == nil {
		return Release{}, ErrNoActiveRelease
	}
	prior, ok := r.releases[priorID]
	if !ok {
		return Release{}, refuse("prior release is not recorded")
	}
	if e := r.eligibility[priorID]; e.Status != "" && e.Status != EligibilityEligible {
		return Release{}, fmt.Errorf("%w: %s: %s", ErrIneligible, e.Status, e.Reason)
	}
	release, err := Rollback(*active, prior, approval)
	if err != nil {
		return Release{}, err
	}
	lineage := r.lineage[priorID]
	if err := r.commit(ctx, release, &lineage); err != nil {
		return Release{}, err
	}
	return release, nil
}

func (r *Registry) commit(ctx context.Context, release Release, lineage *DatasetLineage) error {
	seq := r.sequence + 1
	previous := ""
	if active := r.active.Load(); active != nil {
		previous = active.ID
	}
	if r.store != nil {
		record, err := release.Record()
		if err != nil {
			return err
		}
		encoded, err := EncodeRecord(record)
		if err != nil {
			return err
		}
		if err := r.put(ctx, r.path(dirReleases, release.ID+".json"), encoded); err != nil && !errors.Is(err, storage.ErrExists) {
			return err
		}
		if lineage != nil {
			encoded, err := json.Marshal(lineage)
			if err != nil {
				return err
			}
			if err := r.put(ctx, r.path(dirLineage, release.ID+".json"), encoded); err != nil && !errors.Is(err, storage.ErrExists) {
				return err
			}
		}
		encoded, err = json.Marshal(pointer{Schema: 1, Kind: "release_pointer", Sequence: seq, ReleaseID: release.ID, Previous: previous})
		if err != nil {
			return err
		}
		if err := r.put(ctx, r.path(dirPointers, sequenceName(seq)), encoded); err != nil {
			if errors.Is(err, storage.ErrExists) {
				return fmt.Errorf("%w: pointer %d already written", ErrConflict, seq)
			}
			return err
		}
	}
	r.releases[release.ID] = release
	r.order = append(r.order, release.ID)
	if lineage != nil {
		r.lineage[release.ID] = *lineage
	}
	if _, ok := r.eligibility[release.ID]; !ok {
		r.eligibility[release.ID] = Eligibility{Status: EligibilityEligible}
	}
	r.sequence = seq
	// Everything the release needs is durable before the pointer swaps, so a
	// reader never observes a release whose records are missing.
	r.active.Store(&release)
	return nil
}

func (r *Registry) put(ctx context.Context, p string, b []byte) error {
	w, err := r.store.CreateExclusive(ctx, p)
	if err != nil {
		return fmt.Errorf("registry: %s: %w", p, err)
	}
	if _, err := w.Write(b); err != nil {
		if a, ok := w.(storage.Aborter); ok {
			a.Abort()
		}
		return fmt.Errorf("registry: %s: %w", p, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("registry: %s: %w", p, err)
	}
	return nil
}

// WithdrawSource marks every release whose recorded dataset lineage included
// sourceID ineligible for new use and records one event per release. It
// returns the affected release identities. The active pointer is untouched;
// Resolve refuses the active release if it is affected.
func (r *Registry) WithdrawSource(ctx context.Context, sourceID string, now time.Time) ([]string, error) {
	if sourceID == "" {
		return nil, errors.New("source id is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var affected []string
	for id, l := range r.lineage {
		for _, s := range l.SourceIDs {
			if s == sourceID {
				affected = append(affected, id)
				break
			}
		}
	}
	sort.Strings(affected)
	for _, id := range affected {
		if e := r.eligibility[id]; e.Status == EligibilityWithdrawnSource {
			continue
		}
		ev := Event{Kind: EventSourceWithdrawn, ReleaseID: id, SourceID: sourceID, Detail: WithdrawalLimitation, At: now.UTC()}
		if err := r.record(ctx, ev); err != nil {
			return affected, err
		}
	}
	return affected, nil
}

// RecordEvent appends an event against a recorded release, such as a load
// failure of its bundle. Unknown releases are refused.
func (r *Registry) RecordEvent(ctx context.Context, ev Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.releases[ev.ReleaseID]; !ok {
		return refuse("event names an unrecorded release")
	}
	if ev.Kind == "" || ev.Kind == EventSourceWithdrawn {
		return refuse("event kind is reserved or empty")
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	return r.record(ctx, ev)
}

func (r *Registry) record(ctx context.Context, ev Event) error {
	ev.Sequence = int64(len(r.events) + 1)
	if r.store != nil {
		encoded, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		if err := r.put(ctx, r.path(dirEvents, sequenceName(ev.Sequence)), encoded); err != nil {
			if errors.Is(err, storage.ErrExists) {
				return fmt.Errorf("%w: event %d already written", ErrConflict, ev.Sequence)
			}
			return err
		}
	}
	r.events = append(r.events, ev)
	r.applyEvent(ev)
	return nil
}

func (r *Registry) applyEvent(ev Event) {
	if ev.Kind == EventSourceWithdrawn {
		r.eligibility[ev.ReleaseID] = Eligibility{Status: EligibilityWithdrawnSource, SourceID: ev.SourceID, At: ev.At, Reason: "dataset manifest included withdrawn source " + ev.SourceID}
	}
}

func strictDecode(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	// Free-form members keep their number literals so an integer never
	// re-encodes as a float and changes a content address.
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	var trailing any
	if err := dec.Decode(&trailing); err == nil {
		return errors.New("trailing data")
	}
	return nil
}

func decodeInto(record Record, v any) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return strictDecode(raw, v)
}

// SourcesFromExportManifest reads the source digests an authorized dataset
// export manifest (internal/decisiondatasets) recorded for its rows, so a
// promotion can bind lineage without re-parsing the export elsewhere.
func SourcesFromExportManifest(manifest []byte) (DatasetLineage, error) {
	record, err := DecodeRecord(manifest)
	if err != nil {
		return DatasetLineage{}, err
	}
	if stringField(record, "kind") != "authorized-numeric-training-export" {
		return DatasetLineage{}, errors.New("not an authorized export manifest")
	}
	seen := map[string]struct{}{}
	rows, _ := record["rows"].([]any)
	for _, row := range rows {
		obj, _ := row.(map[string]any)
		sources, _ := obj["sources"].([]any)
		for _, s := range sources {
			src, _ := s.(map[string]any)
			for _, key := range []string{"Digest", "digest"} {
				if d, ok := src[key].(string); ok && d != "" {
					seen[d] = struct{}{}
				}
			}
		}
	}
	out := DatasetLineage{DatasetManifestSHA256: SHA256Hex(manifest), SourceIDs: make([]string, 0, len(seen))}
	for d := range seen {
		out.SourceIDs = append(out.SourceIDs, d)
	}
	sort.Strings(out.SourceIDs)
	return out, nil
}

// ParseSequence is exported for operators inspecting pointer files.
func ParseSequence(name string) (int64, error) {
	return strconv.ParseInt(strings.TrimSuffix(path.Base(name), ".json"), 10, 64)
}

// ResolveForTask resolves the active release for a request on task and
// refuses a release whose recorded scope does not include it.
func (r *Registry) ResolveForTask(task string) (Release, error) {
	rel, err := r.Resolve()
	if err != nil {
		return Release{}, err
	}
	if !rel.ServesTask(task) {
		return Release{}, fmt.Errorf("%w: release %s is scoped to %v, not task %q", ErrRefused, rel.ID, rel.TaskIDs, task)
	}
	return rel, nil
}

// RecordStudentEvent lets a serving provider record a load failure, health
// mismatch, or device fallback against the release it serves.
func (r *Registry) RecordStudentEvent(ctx context.Context, releaseID, kind, detail string) error {
	return r.RecordEvent(ctx, Event{Kind: kind, ReleaseID: releaseID, Detail: detail})
}

// EventImported is recorded when a release arrives through an offline
// release bundle rather than a local promotion.
const EventImported = "release_imported"

// ImportRelease commits an already-sealed release from an offline release
// bundle and makes it active. The record's identity is verified; its
// predecessor chain is the connected side's and is recorded as-is. A release
// already recorded is not re-imported: if it is active this is a no-op, and
// if it is a prior release the caller must roll back rather than import.
func (r *Registry) ImportRelease(ctx context.Context, release Release, lineage DatasetLineage) (bool, error) {
	if err := release.Verify(); err != nil {
		return false, err
	}
	if release.State != StateActive || release.BundleDigest == nil || release.ApprovalID == "" || release.EvaluationID == "" || release.ArtifactID == "" {
		return false, refuse("release record is not a complete promotion")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.releases[release.ID]; exists {
		if active := r.active.Load(); active != nil && active.ID == release.ID {
			return false, nil
		}
		return false, refuse("release " + release.ID + " is already imported; roll back to it instead of importing again")
	}
	if err := r.commit(ctx, release, &lineage); err != nil {
		return false, err
	}
	return true, nil
}
