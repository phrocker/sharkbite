// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
// Package decisioninventoryadmission binds authenticated outcome admission to a
// previously registered durable target inventory. It neither registers coverage
// nor authenticates a principal; decisionoutcomes supplies its actual resolved
// caller and rechecks current source authorization after every hook invocation.
package decisioninventoryadmission

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"unicode/utf8"

	inventory "github.com/phrocker/shoal-oss/internal/decisioninventorystore"
	outcomes "github.com/phrocker/shoal-oss/internal/decisionoutcomes"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Authority resolves an operator-provisioned coverage barrier for the actual
// authenticated reporter and registered prediction/task/label policy. It must
// deny unmanaged targets and independently establish current reporting rights
// and all contributing evidence, controller and provenance access. Verify jointly
// rechecks the same binding and permissions AFTER inventory IO; all its own
// source loading must precede its final authorization check. No default exists.
// This first adapter supports correctness observations only, not execution reports.
type Authority interface {
	Resolve(context.Context, auth.Decision, decision.PredictionRecord, decision.OutcomeObservation) (inventory.Binding, error)
	Verify(context.Context, auth.Decision, inventory.Binding, decision.PredictionRecord, decision.OutcomeObservation) error
}
type Config struct {
	Store     *inventory.Store
	Authority Authority
}
type Adapter struct{ config Config }

var _ outcomes.Admission = (*Adapter)(nil)
var ErrUnavailable = shoal.NewError(shoal.ErrorUnavailable, "outcome inventory admission unavailable")

func absent(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return r.IsNil()
	}
	return false
}
func New(c Config) (*Adapter, error) {
	if c.Store == nil || absent(c.Authority) {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "inventory store and trusted admission authority required")
	}
	return &Adapter{c}, nil
}
func hidden() error { return auth.ObjectNotFound() }
func sanitize(e error) error {
	if e == nil {
		return nil
	}
	if shoal.IsErrorCode(e, shoal.ErrorUnauthorized) || shoal.IsErrorCode(e, shoal.ErrorNotFound) || shoal.IsErrorCode(e, shoal.ErrorInvalidArgument) || errors.Is(e, inventory.ErrNotFound) {
		return hidden()
	}
	if errors.Is(e, inventory.ErrIndeterminate) {
		return errors.Join(ErrUnavailable, inventory.ErrIndeterminate)
	}
	if errors.Is(e, inventory.ErrConflict) {
		return shoal.NewError(shoal.ErrorConflict, "outcome inventory conflict")
	}
	return ErrUnavailable
}
func textID(id shoal.ID) bool {
	return utf8.ValidString(string(id)) && strings.TrimSpace(string(id)) != "" && shoal.ValidateRequiredID("id", id) == nil
}
func intent(d auth.Decision, p decision.PredictionRecord, o decision.OutcomeObservation, id shoal.ID) inventory.Intent {
	return inventory.Intent{ReceiptID: id, ObservationID: o.ID(), RequestID: p.Request().ID(), PredictionID: p.ID(), Reporter: inventory.Attribution{SubjectID: d.Subject(), ActorID: d.Actor(), ClientID: d.ClientID(), OnBehalfOf: append([]shoal.ID(nil), d.OnBehalfOf()...)}}
}
func (a *Adapter) resolve(ctx context.Context, d auth.Decision, p decision.PredictionRecord, o decision.OutcomeObservation) (inventory.Binding, error) {
	var zero inventory.Binding
	if ctx.Err() != nil {
		return zero, ErrUnavailable
	}
	if _, e := auth.AuthorizationFingerprint(d); e != nil {
		return zero, hidden()
	}
	if p.Validate() != nil || o.Validate() != nil || o.Config().Kind != decision.OutcomeCorrectness {
		return zero, hidden()
	}
	rebuilt, e := decision.NewOutcomeObservation(p, o.Config())
	if e != nil || rebuilt.ID() != o.ID() {
		return zero, hidden()
	}
	b, e := a.config.Authority.Resolve(ctx, d, p, o)
	if e != nil {
		return zero, sanitize(e)
	}
	c := o.Config()
	target, e := decision.AdjudicationTargetID(p.Request().TaskID(), p.Request().PictureID(), c.SubjectID, c.QuestionID)
	if ctx.Err() != nil {
		return zero, ErrUnavailable
	}
	if e != nil || !textID(b.CoverageID) || b.TargetID != target || b.TaskID != p.Request().TaskID() || b.PictureID != p.Request().PictureID() || b.SubjectID != c.SubjectID || b.QuestionID != c.QuestionID {
		return zero, hidden()
	}
	return b, nil
}
func (a *Adapter) finish(ctx context.Context, d auth.Decision, b inventory.Binding, p decision.PredictionRecord, o decision.OutcomeObservation, pending error) error {
	// No storage/source resolution may follow this final joint authority check.
	if e := a.config.Authority.Verify(ctx, d, b, p, o); e != nil {
		return sanitize(e)
	}
	if ctx.Err() != nil {
		return ErrUnavailable
	}
	return sanitize(pending)
}
func matches(s inventory.Snapshot, b inventory.Binding, want inventory.Intent, published bool) bool {
	if s.Binding != b || s.ID == "" || s.Version < 1 || len(s.Entries) > inventory.MaxEntries {
		return false
	}
	for _, entry := range s.Entries {
		if entry.Intent.ReceiptID != want.ReceiptID {
			continue
		}
		if !reflect.DeepEqual(entry.Intent, want) {
			return false
		}
		return entry.State == inventory.Published || (!published && entry.State == inventory.Pending)
	}
	return false
}

// Begin retains a pending intent before the outcome store attempts its CAS.
// Exact retries can find an already published intent. No coverage auto-registration
// or abort is attempted, including when final authorization has been revoked.
func (a *Adapter) Begin(ctx context.Context, d auth.Decision, p decision.PredictionRecord, o decision.OutcomeObservation, id shoal.ID) error {
	b, e := a.resolve(ctx, d, p, o)
	if e != nil {
		return e
	}
	i := intent(d, p, o, id)
	snapshot, pending := a.config.Store.Begin(ctx, inventory.Scope{Domain: d.AuthorizationDomain()}, b, i)
	if pending == nil && !matches(snapshot, b, i, false) {
		pending = ErrUnavailable
	}
	return a.finish(ctx, d, b, p, o, pending)
}

// Publish reconciles the exact original immutable receipt; grant refreshes do
// not rewrite its authenticated attribution, original fingerprint or receipt time.
func (a *Adapter) Publish(ctx context.Context, d auth.Decision, p decision.PredictionRecord, o decision.OutcomeObservation, r outcomes.Receipt) error {
	// Retain the canonical observation and detached delegation before authority IO.
	canonical := o.Config()
	if !reflect.DeepEqual(r.ObservationConfig, canonical) {
		return hidden()
	}
	r.ObservationConfig = canonical
	r.OnBehalfOf = append([]shoal.ID(nil), r.OnBehalfOf...)
	b, e := a.resolve(ctx, d, p, o)
	if e != nil {
		return e
	}
	i := intent(d, p, o, r.ID)
	if r.ObservationID != o.ID() || r.SubmitterID != i.Reporter.SubjectID || r.ActorID != i.Reporter.ActorID || r.ClientID != i.Reporter.ClientID || !reflect.DeepEqual(r.OnBehalfOf, i.Reporter.OnBehalfOf) {
		return hidden()
	}
	snapshot, pending := a.config.Store.Publish(ctx, inventory.Scope{Domain: d.AuthorizationDomain()}, b, i, r)
	if pending == nil && (!matches(snapshot, b, i, true) || snapshot.Binding != b) {
		pending = ErrUnavailable
	}
	return a.finish(ctx, d, b, p, o, pending)
}
