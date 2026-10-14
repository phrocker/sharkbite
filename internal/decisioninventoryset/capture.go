// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
// Package decisioninventoryset captures a consistent cut of registered target
// inventories using two equal, non-overlapping collections. It grants no access
// and makes no atomicity claim about source permissions or adjudication heads.
package decisioninventoryset

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
	"unsafe"

	inventory "github.com/phrocker/shoal-oss/internal/decisioninventorystore"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const MaxTargets = 256

// MaxRetainedBytes bounds conservatively accounted first-pass material, including
// backing capacities and string bytes. A single bounded Load's transient decode
// allocations and the one-at-a-time second-pass snapshot are additional.
const MaxRetainedBytes = 32 << 20

var (
	ErrUnavailable = errors.New("inventory set unavailable")
	ErrIncomplete  = errors.New("inventory set has pending admissions")
	ErrChanged     = errors.New("inventory set changed during collection")
	ErrLimit       = errors.New("inventory set retained material limit")
)

type Config struct {
	Store *inventory.Store
	Clock func() time.Time
}
type Reader struct{ config Config }
type Window struct{ StartedAt, FirstPassFinishedAt, SecondPassStartedAt, CompletedAt time.Time }
type Capture struct {
	VectorID  shoal.ID
	Window    Window
	Snapshots []inventory.Snapshot
}

func New(c Config) (*Reader, error) {
	if c.Store == nil || c.Clock == nil {
		return nil, invalid()
	}
	return &Reader{c}, nil
}
func invalid() error {
	return shoal.NewError(shoal.ErrorInvalidArgument, "invalid inventory set request")
}
func textID(id shoal.ID) bool {
	return utf8.ValidString(string(id)) && strings.TrimSpace(string(id)) != "" && shoal.ValidateRequiredID("id", id) == nil
}
func bindingOK(b inventory.Binding) bool {
	for _, id := range []shoal.ID{b.CoverageID, b.TargetID, b.TaskID, b.PictureID, b.SubjectID, b.QuestionID} {
		if !textID(id) {
			return false
		}
	}
	id, e := decision.AdjudicationTargetID(b.TaskID, b.PictureID, b.SubjectID, b.QuestionID)
	return e == nil && id == b.TargetID
}
func (r *Reader) stamp(ctx context.Context, previous time.Time) (time.Time, error) {
	if ctx.Err() != nil {
		return time.Time{}, ErrUnavailable
	}
	n := r.config.Clock().Round(0).UTC()
	if ctx.Err() != nil || n.IsZero() || n.Year() < 1 || n.Year() > 9999 || n.Before(previous) {
		return time.Time{}, ErrUnavailable
	}
	return n, nil
}
func bindingBytes(b inventory.Binding) int {
	return len(b.CoverageID) + len(b.TargetID) + len(b.TaskID) + len(b.PictureID) + len(b.SubjectID) + len(b.QuestionID)
}

// No JSON serialization is used to discover a size bound. Store.Load already
// enforces per-record limits; this accounts for retained objects before appending
// them to the first-pass collection, without copying their payloads.
func snapshotBytes(s inventory.Snapshot) int {
	n := int(unsafe.Sizeof(s)) + len(s.ID) + bindingBytes(s.Binding) + cap(s.Entries)*int(unsafe.Sizeof(inventory.Entry{}))
	for _, e := range s.Entries {
		i := e.Intent
		a := i.Reporter
		n += len(e.State) + len(e.ReceiptDigest) + len(i.ReceiptID) + len(i.ObservationID) + len(i.RequestID) + len(i.PredictionID) + len(a.SubjectID) + len(a.ActorID) + len(a.ClientID) + cap(a.OnBehalfOf)*int(unsafe.Sizeof(shoal.ID("")))
		for _, id := range a.OnBehalfOf {
			n += len(id)
		}
	}
	return n
}

type pin struct {
	Binding    inventory.Binding
	SnapshotID shoal.ID
	Version    int64
}

func vectorID(scope inventory.Scope, snapshots []inventory.Snapshot) shoal.ID {
	pins := make([]pin, len(snapshots))
	for i, s := range snapshots {
		pins[i] = pin{s.Binding, s.ID, s.Version}
	}
	raw, _ := json.Marshal(struct {
		Kind   string
		Domain []byte
		Pins   []pin
	}{"registered-inventory-vector-v1", scope.Domain, pins})
	sum := sha256.Sum256(raw)
	return shoal.ID("inventory-vector:" + hex.EncodeToString(sum[:]))
}

// CaptureSet performs exactly two full scans on success, and no retry on change.
// Matching monotonic IDs/versions establish a cut between the first pass ending
// and second pass starting. All writers must honor the registered coverage, row
// CAS must be linearizable, and deletion/rollback/recreation must be excluded.
// Pending intents block capture regardless of timestamps. This historical cut
// is not a guarantee that every inventory stays current until response delivery.
func (r *Reader) CaptureSet(ctx context.Context, scope inventory.Scope, bindings []inventory.Binding) (Capture, error) {
	return r.captureSet(ctx, scope, bindings, MaxRetainedBytes)
}

func (r *Reader) captureSet(ctx context.Context, scope inventory.Scope, bindings []inventory.Binding, byteLimit int) (Capture, error) {
	var zero Capture
	if len(bindings) < 1 || len(bindings) > MaxTargets || len(scope.Domain) < 1 || len(scope.Domain) > auth.MaxPolicyComponentBytes {
		return zero, invalid()
	}
	seen := map[shoal.ID]bool{}
	for _, b := range bindings {
		if !bindingOK(b) || seen[b.TargetID] {
			return zero, invalid()
		}
		seen[b.TargetID] = true
	}
	scope.Domain = append([]byte(nil), scope.Domain...)
	bindings = append([]inventory.Binding(nil), bindings...)
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].TargetID < bindings[j].TargetID })
	started, e := r.stamp(ctx, time.Time{})
	if e != nil {
		return zero, e
	}
	out := Capture{Window: Window{StartedAt: started}, Snapshots: make([]inventory.Snapshot, 0, len(bindings))}
	retained := cap(out.Snapshots)*int(unsafe.Sizeof(inventory.Snapshot{})) + cap(bindings)*int(unsafe.Sizeof(inventory.Binding{})) + len(scope.Domain)
	for _, b := range bindings {
		retained += bindingBytes(b)
	}
	for _, b := range bindings {
		s, e := r.config.Store.Load(ctx, scope, b)
		if e != nil {
			return zero, e
		}
		if !s.Complete() {
			return zero, ErrIncomplete
		}
		size := snapshotBytes(s)
		if size > byteLimit-retained {
			return zero, ErrLimit
		}
		retained += size
		out.Snapshots = append(out.Snapshots, s)
	}
	out.Window.FirstPassFinishedAt, e = r.stamp(ctx, out.Window.StartedAt)
	if e != nil {
		return zero, e
	}
	out.Window.SecondPassStartedAt, e = r.stamp(ctx, out.Window.FirstPassFinishedAt)
	if e != nil {
		return zero, e
	}
	for i, b := range bindings {
		current, e := r.config.Store.Load(ctx, scope, b)
		if e != nil {
			return zero, e
		}
		if !current.Complete() {
			return zero, ErrIncomplete
		}
		previous := out.Snapshots[i]
		if current.Binding != previous.Binding || current.Version != previous.Version || current.ID != previous.ID {
			return zero, ErrChanged
		}
	}
	out.Window.CompletedAt, e = r.stamp(ctx, out.Window.SecondPassStartedAt)
	if e != nil {
		return zero, e
	}
	out.VectorID = vectorID(scope, out.Snapshots)
	if ctx.Err() != nil {
		return zero, ErrUnavailable
	}
	return out, nil
}
