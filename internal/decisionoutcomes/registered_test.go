// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionoutcomes

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type registeredTestAuthority struct {
	prediction  decision.PredictionRecord
	reference   RegisteredReference
	coverage    shoal.ID
	receipts    map[shoal.ID]Receipt
	deny        bool
	resolveHook func(RegisteredReference)
	verifyHook  func([]RegisteredMaterial)
	verified    int
}

func (a *registeredTestAuthority) Resolve(_ context.Context, d auth.Decision, ref RegisteredReference) (RegisteredBinding, error) {
	if a.resolveHook != nil {
		a.resolveHook(ref)
	}
	if a.deny || d.Subject() != "reviewer" || ref.TargetID != a.reference.TargetID || ref.InventoryID != a.reference.InventoryID {
		return RegisteredBinding{}, auth.ObjectNotFound()
	}
	if _, ok := a.receipts[ref.ReceiptID]; !ok {
		return RegisteredBinding{}, auth.ObjectNotFound()
	}
	return RegisteredBinding{ref, a.coverage, a.prediction}, nil
}
func (a *registeredTestAuthority) Verify(_ context.Context, d auth.Decision, m []RegisteredMaterial) error {
	a.verified++
	if a.verifyHook != nil {
		a.verifyHook(m)
	}
	if a.deny || d.Subject() != "reviewer" {
		return auth.ObjectNotFound()
	}
	for _, v := range m {
		original, ok := a.receipts[v.Receipt.ID]
		if !ok || !reflect.DeepEqual(original, v.Receipt) || v.Binding.Reference.InventoryID != a.reference.InventoryID {
			return auth.ObjectNotFound()
		}
	}
	return nil
}
func registeredFixture(t *testing.T) (*RegisteredReader, *registeredTestAuthority, *Store, *memoryCAS, context.Context, context.Context, decision.OutcomeObservationConfig) {
	t.Helper()
	s, b, a, reportCtx, c := storeFixture(t)
	original, e := appendCfg(s, reportCtx, "first", c)
	if e != nil {
		t.Fatal(e)
	}
	target, e := decision.AdjudicationTargetID(a.prediction.Request().TaskID(), a.prediction.Request().PictureID(), c.SubjectID, c.QuestionID)
	if e != nil {
		t.Fatal(e)
	}
	ref := RegisteredReference{target, "inventory-snapshot:1", original.ID}
	gate := &registeredTestAuthority{prediction: a.prediction, reference: ref, coverage: "registered-coverage:1", receipts: map[shoal.ID]Receipt{original.ID: original}}
	d, e := auth.NewDecision(auth.DecisionConfig{Subject: "reviewer", Actor: "review-actor", AuthorizationDomain: []byte("domain"), AllowedOperations: []auth.Operation{auth.OperationRead}, PermittedSourceIDs: [][]byte{[]byte("evidence")}, PermittedPolicyIDs: [][]byte{[]byte("read-policy")}, PolicyGeneration: 1, AuthenticationExpires: a.now().Add(time.Hour), RequestID: "review-read"})
	if e != nil {
		t.Fatal(e)
	}
	capability, e := auth.NewAuthorityWithClock(a.now)
	if e != nil {
		t.Fatal(e)
	}
	readCtx, e := capability.Binder().Bind(context.Background(), d)
	if e != nil {
		t.Fatal(e)
	}
	r, e := NewRegisteredReader(RegisteredReaderConfig{Backend: b, Resolver: capability.Resolver(), Authority: gate, Clock: a.now})
	if e != nil {
		t.Fatal(e)
	}
	return r, gate, s, b, reportCtx, readCtx, c
}
func TestRegisteredReadCrossPrincipalAndOrdinaryIsolation(t *testing.T) {
	r, a, s, b, reportCtx, readCtx, c := registeredFixture(t)
	before := b.writes.Load()
	got, e := r.ReadRegistered(readCtx, a.reference)
	if e != nil || !reflect.DeepEqual(got, a.receipts[a.reference.ReceiptID]) || got.SubmitterID != "principal" {
		t.Fatal("registered read lost original attribution", e)
	}
	if _, e = s.Read(readCtx, c.RequestID, c.PredictionID, []byte("first")); e == nil {
		t.Fatal("ordinary read crossed principal boundary")
	}
	if _, e = s.Read(reportCtx, c.RequestID, c.PredictionID, []byte("first")); e != nil {
		t.Fatal(e)
	}
	if b.writes.Load() != before {
		t.Fatal("read mutated storage")
	}
}
func TestRegisteredReaderCorrectionBranchesAndMissingMembership(t *testing.T) {
	r, a, s, _, reportCtx, readCtx, c := registeredFixture(t)
	c.Supersedes = a.reference.ReceiptID
	left, e := appendCfg(s, reportCtx, "left", c)
	if e != nil {
		t.Fatal(e)
	}
	c.Label = "ordinary"
	right, e := appendCfg(s, reportCtx, "right", c)
	if e != nil {
		t.Fatal(e)
	}
	a.receipts[left.ID] = left
	a.receipts[right.ID] = right
	for _, receipt := range []Receipt{left, right} {
		ref := a.reference
		ref.ReceiptID = receipt.ID
		seen := 0
		a.verifyHook = func(m []RegisteredMaterial) { seen = len(m) }
		got, e := r.ReadRegistered(readCtx, ref)
		if e != nil || got.ID != receipt.ID || seen != 2 {
			t.Fatal("branch ancestry lost", e)
		}
	}
	delete(a.receipts, a.reference.ReceiptID)
	ref := a.reference
	ref.ReceiptID = left.ID
	before := a.verified
	if got, e := r.ReadRegistered(readCtx, ref); e == nil || got.ID != "" || a.verified != before+1 {
		t.Fatal("missing ancestor disclosed partial receipt", e)
	}
}
func TestRegisteredReaderFinalRevocationAndMutation(t *testing.T) {
	for _, mode := range []string{"revocation", "generation", "mutation", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			r, a, _, _, _, ctx, _ := registeredFixture(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			a.verifyHook = func(m []RegisteredMaterial) {
				switch mode {
				case "revocation":
					a.deny = true
				case "generation":
					a.reference.InventoryID = "inventory-snapshot:2"
				case "mutation":
					m[0].Receipt.ObservationConfig.EvidenceIDs[0] = "changed"
				case "cancel":
					cancel()
				}
			}
			if got, e := r.ReadRegistered(ctx, a.reference); e == nil || got.ID != "" {
				t.Fatal("final rejection disclosed receipt", e)
			}
		})
	}
}
func TestRegisteredReaderRejectsMissingAuthority(t *testing.T) {
	r, _, _, _, _, _, _ := registeredFixture(t)
	var nilGate *registeredTestAuthority
	c := r.config
	c.Authority = nilGate
	if _, e := NewRegisteredReader(c); e == nil {
		t.Fatal("typed nil authority accepted")
	}
}

func TestRegisteredShapeBoundsBeforeDecode(t *testing.T) {
	for name, raw := range map[string]string{
		"evidence":  `{"EvidenceIDs":[` + strings.Repeat(`"x",`, decision.MaxSources) + `"x"]}`,
		"delegates": `{"OnBehalfOf":[` + strings.Repeat(`"eA==",`, auth.MaxOnBehalfOfEntries) + `"eA=="]}`,
		"depth":     strings.Repeat(`[`, 18) + `0` + strings.Repeat(`]`, 18),
		"duplicate": `{"ID":"first","ID":"second"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if registeredShape([]byte(raw)) {
				t.Fatal("amplified or ambiguous JSON accepted")
			}
		})
	}
	_, _, a, _, c := storeFixture(t)
	c.EvidenceIDs = make([]shoal.ID, decision.MaxSources)
	for i := range c.EvidenceIDs {
		c.EvidenceIDs[i] = shoal.ID(fmt.Sprintf("evidence:%06d", i))
	}
	o, e := decision.NewOutcomeObservation(a.prediction, c)
	if e != nil {
		t.Fatal(e)
	}
	// The shape gate must admit the full legal evidence count in a canonical
	// persisted envelope; semantic authentication remains the subsequent check.
	receipt := Receipt{ID: shoal.ID("outcome-receipt:" + strings.Repeat("a", 64)), ObservationID: o.ID(), ObservationConfig: o.Config()}
	raw, e := encode(row{Receipt: receipt})
	if e != nil {
		t.Fatal(e)
	}
	if !registeredShape(raw) {
		t.Fatal("legal maximum evidence inventory rejected")
	}
}

func TestRegisteredDetachPreservesEmptyDelegation(t *testing.T) {
	r, a, _, b, _, ctx, _ := registeredFixture(t)
	cell := b.cells[string(a.reference.ReceiptID)]
	stored, e := decode(cell.Value)
	if e != nil {
		t.Fatal(e)
	}
	stored.Receipt.OnBehalfOf = []shoal.ID{}
	cell.Value, e = encode(stored)
	if e != nil {
		t.Fatal(e)
	}
	b.cells[string(a.reference.ReceiptID)] = cell
	a.receipts[stored.Receipt.ID] = stored.Receipt
	got, e := r.ReadRegistered(ctx, a.reference)
	if e != nil || got.OnBehalfOf == nil {
		t.Fatal("detachment changed empty delegation representation", e)
	}
}
