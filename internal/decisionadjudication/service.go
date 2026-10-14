// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
// Package decisionadjudication composes trusted evidence authority, immutable
// basis retention and target journals. It supplies no permissive authority,
// training eligibility, automatic promotion or public transport.
package decisionadjudication

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	journal "github.com/phrocker/shoal-oss/internal/decisionadjudicationstore"
	bases "github.com/phrocker/shoal-oss/internal/decisionbasisstore"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

var ErrUnavailable = shoal.NewError(shoal.ErrorUnavailable, "adjudication unavailable")
var ErrIndeterminate = shoal.NewError(shoal.ErrorUnavailable, "adjudication commit indeterminate")
var ErrConflict = shoal.NewError(shoal.ErrorConflict, "adjudication conflict")

// MaxHistoryBasisBytes bounds cumulative hydrated content. History never returns
// a partial prefix when this bound is exceeded.
const MaxHistoryBasisBytes = 16 << 20

type Binding struct {
	Policy     decision.LabelPolicy
	Prediction decision.PredictionRecord
}
type Material struct {
	Receipt  journal.Receipt
	Policy   decision.LabelPolicy
	Proposal decision.AdjudicationProposal
	Basis    decision.AdjudicationBasis
}
type Candidate struct {
	Policy      decision.LabelPolicy
	Proposal    decision.AdjudicationProposal
	Basis       decision.AdjudicationBasis
	Adjudicator decision.BasisIdentity
	// RequiredRoles is derived from the pinned policy, not caller input.
	RequiredRoles []shoal.ID
}

// Authority is a mandatory trusted integration, never a caller-provided grant.
// Resolve uses an explicit adjudication path across principal-scoped receipts,
// authenticates registered predictions/policies and checks their current inputs.
// AuthorizeTarget gates storage access by current target/operation permission.
// Capture enumerates the COMPLETE target inventory at its retained cutoff and
// verifies receipt times, reporter/controller lineage, witnesses and authority
// revisions. It cannot accept caller claims as evidence of completeness.
//
// Verify is the final JOINT check after every storage/Resolve/Capture operation.
// It must authenticate all retained assertions against immutable authority and
// original records, historical roles as of admission, and current permission to
// every original source/outcome/witness plus combined disclosure. For candidate,
// it must check CURRENT RequiredRoles and prove witnesses support the exact
// proposed label/truth. Common provenance or model agreement is not ground truth.
// Completeness of old bases is checked as of their cutoff, not today's inventory.
// Verify must finish its own source loading before its final authorization check;
// fingerprints alone do not capture source ACL revocation. No default exists.
type Authority interface {
	Resolve(context.Context, auth.Decision, shoal.ID, shoal.ID, auth.Operation) (Binding, error)
	AuthorizeTarget(context.Context, auth.Decision, shoal.ID, auth.Operation) error
	Capture(context.Context, auth.Decision, decision.AdjudicationProposal, []Material) (decision.AdjudicationBasis, error)
	Verify(context.Context, auth.Decision, auth.Operation, shoal.ID, []Material, *Candidate) error
}
type Journal interface {
	History(context.Context, journal.Scope, shoal.ID) ([]journal.Receipt, error)
	AppendChecked(context.Context, journal.Scope, []byte, decision.AdjudicationProposal, journal.Attribution, shoal.ID, func(context.Context, []journal.Receipt) error) (journal.Receipt, error)
}
type Bases interface {
	Retain(context.Context, bases.Scope, decision.AdjudicationBasis) error
	Load(context.Context, bases.Scope, shoal.ID, decision.AdjudicationProposal) (decision.AdjudicationBasis, error)
}
type Config struct {
	Resolver  auth.Resolver
	Authority Authority
	Journal   Journal
	Bases     Bases
	Clock     func() time.Time
}
type Service struct{ config Config }

func absent(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Pointer, reflect.Func, reflect.Map, reflect.Slice, reflect.Interface, reflect.Chan:
		return r.IsNil()
	}
	return false
}
func New(c Config) (*Service, error) {
	if absent(c.Resolver) || absent(c.Authority) || absent(c.Journal) || absent(c.Bases) || c.Clock == nil {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "invalid adjudication configuration")
	}
	return &Service{c}, nil
}
func hidden() error { return auth.ObjectNotFound() }
func sanitize(e error) error {
	if e == nil {
		return nil
	}
	if shoal.IsErrorCode(e, shoal.ErrorNotFound) || shoal.IsErrorCode(e, shoal.ErrorUnauthorized) {
		return hidden()
	}
	return ErrUnavailable
}
func (s *Service) now() (time.Time, error) {
	n := s.config.Clock().Round(0).UTC()
	if n.IsZero() || n.Year() < 1 || n.Year() > 9999 {
		return n, ErrUnavailable
	}
	return n, nil
}
func (s *Service) caller(ctx context.Context, before *auth.Decision) (auth.Decision, error) {
	var zero auth.Decision
	if ctx.Err() != nil {
		return zero, ErrUnavailable
	}
	d, e := s.config.Resolver.Resolve(ctx)
	if e != nil {
		return zero, sanitize(e)
	}
	n, e := s.now()
	if e != nil {
		return zero, e
	}
	if !n.Before(d.AuthenticationExpires()) {
		return zero, hidden()
	}
	fp, e := auth.AuthorizationFingerprint(d)
	if e != nil {
		return zero, hidden()
	}
	if before != nil {
		original, e := auth.AuthorizationFingerprint(*before)
		if e != nil || original != fp {
			return zero, hidden()
		}
	}
	if ctx.Err() != nil {
		return zero, ErrUnavailable
	}
	return d, nil
}
func attribution(d auth.Decision) journal.Attribution {
	fp, _ := auth.AuthorizationFingerprint(d)
	return journal.Attribution{SubjectID: d.Subject(), ActorID: d.Actor(), ClientID: d.ClientID(), OnBehalfOf: d.OnBehalfOf(), AuthorizationFingerprint: fp.String()}
}
func basisIdentity(a journal.Attribution) decision.BasisIdentity {
	out := decision.BasisIdentity{SubjectID: []byte(a.SubjectID), ActorID: []byte(a.ActorID), ClientID: []byte(a.ClientID)}
	for _, id := range a.OnBehalfOf {
		out.OnBehalfOf = append(out.OnBehalfOf, []byte(id))
	}
	return out
}
func (s *Service) proposal(ctx context.Context, d auth.Decision, c decision.AdjudicationProposalConfig, op auth.Operation) (Binding, decision.AdjudicationProposal, error) {
	var zero decision.AdjudicationProposal
	if shoal.ValidateRequiredID("request", c.RequestID) != nil || shoal.ValidateRequiredID("prediction", c.PredictionID) != nil {
		return Binding{}, zero, hidden()
	}
	b, e := s.config.Authority.Resolve(ctx, d, c.RequestID, c.PredictionID, op)
	if e != nil {
		return b, zero, sanitize(e)
	}
	if b.Policy.Validate() != nil || b.Prediction.Validate() != nil || b.Prediction.ID() != c.PredictionID || b.Prediction.Request().ID() != c.RequestID {
		return b, zero, hidden()
	}
	p, e := decision.NewAdjudicationProposal(b.Policy, b.Prediction, c)
	if e != nil {
		return b, zero, hidden()
	}
	return b, p, nil
}
func (s *Service) finish(ctx context.Context, d auth.Decision, op auth.Operation, target shoal.ID, m []Material, c *Candidate, pending error) error {
	// Nothing that loads sources or stored records may follow this joint check.
	if e := s.config.Authority.Verify(ctx, d, op, target, m, c); e != nil {
		return sanitize(e)
	}
	if _, e := s.caller(ctx, &d); e != nil {
		return e
	}
	return pending
}
func candidate(policy decision.LabelPolicy, p decision.AdjudicationProposal, b decision.AdjudicationBasis, a journal.Attribution, nonFirst bool) *Candidate {
	roles := []shoal.ID{policy.Config().AdjudicatorRoleID}
	// Conservative: every later verified transition needs dispute resolution,
	// even when an intervening unresolved state would hide an earlier dispute.
	if nonFirst && p.Config().Disposition == decision.AdjudicationVerified {
		roles = append(roles, policy.Config().DisputeResolverRoleID)
	}
	return &Candidate{policy, p, b, basisIdentity(a), roles}
}
func (s *Service) hydrate(ctx context.Context, d auth.Decision, target shoal.ID, rows []journal.Receipt) ([]Material, error) {
	if len(rows) > journal.MaxEntries {
		return nil, hidden()
	}
	var material []Material
	var bytesUsed int
	var previous shoal.ID
	var previousTime time.Time
	for i, r := range rows {
		if r.TargetID != target || r.Version != int64(i+1) || r.ProposalConfig.ExpectedHeadID != previous || r.ProposalConfig.ExpectedVersion != int64(i) {
			return material, hidden()
		}
		b, p, e := s.proposal(ctx, d, r.ProposalConfig, auth.OperationRead)
		if e != nil {
			return material, hidden()
		}
		if p.ID() != r.ProposalID || p.TargetID() != target || p.TaskID() != r.TaskID || p.PictureID() != r.PictureID || p.PolicyID() != r.PolicyID {
			return material, hidden()
		}
		basis, e := s.config.Bases.Load(ctx, bases.Scope{Domain: d.AuthorizationDomain()}, r.BasisID, p)
		if e != nil {
			return material, hidden()
		}
		now, e := s.now()
		if e != nil {
			return material, e
		}
		if basis.Validate() != nil || basis.ID() != r.BasisID || basis.Proposal().ID() != p.ID() || r.ReceivedAt.IsZero() || r.ReceivedAt.Before(previousTime) || r.ReceivedAt.After(now) || basis.Config().CapturedAt.After(r.ReceivedAt) {
			return material, hidden()
		}
		if e := validateAdmission(b.Policy, p, basis, basisIdentity(r.Adjudicator)); e != nil {
			return material, hidden()
		}
		raw, e := json.Marshal(basis.Config())
		if e != nil {
			return material, hidden()
		}
		bytesUsed += len(raw)
		if bytesUsed > MaxHistoryBasisBytes {
			return material, hidden()
		}
		material = append(material, Material{r, b.Policy, p, basis})
		previous = r.ID
		previousTime = r.ReceivedAt
	}
	return material, nil
}

// prospectiveBound prevents committing a history that this service could never
// hydrate. It is checked again against the exact pre-CAS history in the guard.
func prospectiveBound(m []Material, basis decision.AdjudicationBasis) error {
	total := 0
	for _, v := range m {
		raw, e := json.Marshal(v.Basis.Config())
		if e != nil {
			return hidden()
		}
		total += len(raw)
	}
	raw, e := json.Marshal(basis.Config())
	if e != nil {
		return hidden()
	}
	if total+len(raw) > MaxHistoryBasisBytes {
		return hidden()
	}
	return nil
}
func matching(m []Material, id, proposal shoal.ID) *Material {
	for i := range m {
		if m[i].Receipt.ID == id && m[i].Proposal.ID() == proposal {
			return &m[i]
		}
	}
	return nil
}
func (s *Service) load(ctx context.Context, d auth.Decision, target shoal.ID) ([]Material, error) {
	rows, e := s.config.Journal.History(ctx, journal.Scope{Domain: d.AuthorizationDomain()}, target)
	if e != nil {
		return nil, hidden()
	}
	return s.hydrate(ctx, d, target, rows)
}

// History discloses a complete, currently authorized history or no history at all.
func (s *Service) History(ctx context.Context, target shoal.ID) ([]journal.Receipt, error) {
	if shoal.ValidateRequiredID("target", target) != nil {
		return nil, hidden()
	}
	d, e := s.caller(ctx, nil)
	if e != nil {
		return nil, e
	}
	if e = s.config.Authority.AuthorizeTarget(ctx, d, target, auth.OperationRead); e != nil {
		return nil, sanitize(e)
	}
	m, pending := s.load(ctx, d, target)
	if len(m) == 0 && pending == nil {
		pending = hidden()
	}
	if e = s.finish(ctx, d, auth.OperationRead, target, m, nil, pending); e != nil {
		return nil, e
	}
	result := make([]journal.Receipt, len(m))
	for i, v := range m {
		result[i] = v.Receipt
	}
	return result, nil
}

func (s *Service) Adjudicate(ctx context.Context, key []byte, cfg decision.AdjudicationProposalConfig) (journal.Receipt, error) {
	var zero journal.Receipt
	if len(key) == 0 || len(key) > shoal.MaxIDBytes {
		return zero, shoal.NewError(shoal.ErrorInvalidArgument, "invalid adjudication key")
	}
	key = append([]byte(nil), key...)
	d, e := s.caller(ctx, nil)
	if e != nil {
		return zero, e
	}
	b, p, e := s.proposal(ctx, d, cfg, auth.OperationIngest)
	if e != nil {
		return zero, e
	}
	target := p.TargetID()
	if e = s.config.Authority.AuthorizeTarget(ctx, d, target, auth.OperationIngest); e != nil {
		return zero, sanitize(e)
	}
	m, pending := s.load(ctx, d, target)
	if e = s.finish(ctx, d, auth.OperationIngest, target, m, nil, pending); e != nil {
		return zero, e
	}
	a := attribution(d)
	id, e := journal.ReceiptID(journal.Scope{Domain: d.AuthorizationDomain()}, target, key, a)
	if e != nil {
		return zero, ErrUnavailable
	}
	for _, old := range m {
		if old.Receipt.ID != id {
			continue
		}
		replay := candidate(old.Policy, old.Proposal, old.Basis, a, old.Receipt.Version > 1)
		var conflict error
		if old.Proposal.ID() != p.ID() {
			conflict = ErrConflict
		}
		if e = s.finish(ctx, d, auth.OperationIngest, target, m, replay, conflict); e != nil {
			return zero, e
		}
		return old.Receipt, nil
	}
	basis, e := s.config.Authority.Capture(ctx, d, p, m)
	if e != nil {
		return zero, s.finish(ctx, d, auth.OperationIngest, target, m, nil, sanitize(e))
	}
	now, e := s.now()
	if e != nil {
		return zero, e
	}
	if basis.Validate() != nil || basis.Proposal().ID() != p.ID() || basis.Config().CapturedAt.After(now) {
		return zero, s.finish(ctx, d, auth.OperationIngest, target, m, nil, hidden())
	}
	c := candidate(b.Policy, p, basis, a, len(m) > 0)
	if e = validateAdmission(b.Policy, p, basis, c.Adjudicator); e != nil {
		return zero, s.finish(ctx, d, auth.OperationIngest, target, m, c, hidden())
	}
	if e = s.finish(ctx, d, auth.OperationIngest, target, m, c, prospectiveBound(m, basis)); e != nil {
		return zero, e
	}
	if e = s.config.Bases.Retain(ctx, bases.Scope{Domain: d.AuthorizationDomain()}, basis); e != nil {
		return zero, s.finish(ctx, d, auth.OperationIngest, target, m, c, ErrUnavailable)
	}
	var guardRejected bool
	guard := func(checkCtx context.Context, rows []journal.Receipt) error {
		current, pending := s.hydrate(checkCtx, d, target, rows)
		// Required roles depend on the actual head used by this append, not the
		// earlier history read used for collection.
		check := candidate(b.Policy, p, basis, a, len(rows) > 0)
		if old := matching(current, id, p.ID()); old != nil {
			check = candidate(old.Policy, old.Proposal, old.Basis, a, old.Receipt.Version > 1)
		} else if pending == nil {
			pending = prospectiveBound(current, basis)
		}
		err := s.finish(checkCtx, d, auth.OperationIngest, target, current, check, pending)
		guardRejected = err != nil
		return err
	}
	_, appendErr := s.config.Journal.AppendChecked(ctx, journal.Scope{Domain: d.AuthorizationDomain()}, key, p, a, basis.ID(), guard)
	if guardRejected {
		return zero, sanitize(appendErr)
	}
	// Append may have committed even if its final readback, source authorization
	// or caller resolution fails. Preserve that uncertainty without disclosing it.
	after, loadErr := s.load(ctx, d, target)
	winner := matching(after, id, p.ID())
	if winner != nil {
		c = candidate(winner.Policy, winner.Proposal, winner.Basis, a, winner.Receipt.Version > 1)
	}
	finalErr := s.finish(ctx, d, auth.OperationIngest, target, after, c, loadErr)
	if finalErr != nil {
		return zero, errors.Join(ErrIndeterminate, finalErr)
	}
	// The caller's payload is the proposal. Concurrent captures may differ in
	// server-derived basis/time; an authorized exact-proposal winner reconciles
	// both an unknown acknowledgement and that internal basis conflict.
	if winner != nil {
		return winner.Receipt, nil
	}
	if appendErr != nil {
		if errors.Is(appendErr, journal.ErrConflict) || errors.Is(appendErr, journal.ErrLimit) {
			return zero, ErrConflict
		}
		return zero, ErrIndeterminate
	}
	return zero, ErrIndeterminate
}
