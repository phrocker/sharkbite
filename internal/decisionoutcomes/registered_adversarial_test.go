// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionoutcomes

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type adversarialRegisteredAuthority struct {
	pred          decision.PredictionRecord
	ref           RegisteredReference
	receipts      map[shoal.ID]Receipt
	revoked       map[shoal.ID]bool
	stale         bool
	resolveHook   func(*RegisteredBinding)
	verifyHook    func([]RegisteredMaterial)
	verifies      int
	allowMutation bool
	seen          []RegisteredMaterial
}

func (a *adversarialRegisteredAuthority) Resolve(_ context.Context, d auth.Decision, ref RegisteredReference) (RegisteredBinding, error) {
	if d.Actor() != "reviewer-actor" || ref.TargetID != a.ref.TargetID || ref.InventoryID != a.ref.InventoryID {
		return RegisteredBinding{}, auth.ObjectNotFound()
	}
	if _, ok := a.receipts[ref.ReceiptID]; !ok {
		return RegisteredBinding{}, auth.ObjectNotFound()
	}
	b := RegisteredBinding{Reference: ref, CoverageID: "closed-published-namespace", Prediction: a.pred}
	if a.resolveHook != nil {
		a.resolveHook(&b)
	}
	return b, nil
}
func (a *adversarialRegisteredAuthority) Verify(_ context.Context, d auth.Decision, m []RegisteredMaterial) error {
	a.verifies++
	a.seen = append([]RegisteredMaterial(nil), m...)
	if a.verifyHook != nil {
		a.verifyHook(m)
	}
	if a.allowMutation {
		return nil
	}
	if a.stale || d.Actor() != "reviewer-actor" {
		return auth.ObjectNotFound()
	}
	for _, v := range m {
		if !reflect.DeepEqual(v.Receipt, a.receipts[v.Receipt.ID]) {
			return auth.ObjectNotFound()
		}
		for _, id := range v.Receipt.ObservationConfig.EvidenceIDs {
			if a.revoked[id] {
				return auth.ObjectNotFound()
			}
		}
	}
	return nil
}

type adversarialRegisteredBackend struct {
	backend *memoryCAS
	hook    func(int)
	reads   int
}

func (b *adversarialRegisteredBackend) ReadExact(ctx context.Context, c []allocator.Coordinate) ([]allocator.Cell, error) {
	cells, err := b.backend.ReadExact(ctx, c)
	b.reads++
	if b.hook != nil {
		b.hook(b.reads)
	}
	return cells, err
}

type adversarialRegisteredFixture struct {
	store        *Store
	backend      *memoryCAS
	reader       *RegisteredReader
	wrapped      *adversarialRegisteredBackend
	authority    *adversarialRegisteredAuthority
	readerCtx    context.Context
	root, parent Receipt
	ref          RegisteredReference
}

func newAdversarialRegistered(t *testing.T, opaque bool) *adversarialRegisteredFixture {
	t.Helper()
	s, b, a, ctx, c := storeFixture(t)
	if opaque {
		d, err := auth.NewDecision(auth.DecisionConfig{Subject: shoal.ID(string([]byte{255, 0})), Actor: shoal.ID(string([]byte{254, 1})), ClientID: shoal.ID(string([]byte{253})), OnBehalfOf: []shoal.ID{shoal.ID(string([]byte{252}))}, AuthorizationDomain: []byte("domain"), AllowedOperations: []auth.Operation{auth.OperationRead, auth.OperationIngest, auth.OperationRetrieve}, PermittedSourceIDs: [][]byte{[]byte("tasks"), []byte("evidence")}, PermittedPolicyIDs: [][]byte{[]byte("task-policy"), []byte("evidence-policy")}, PolicyGeneration: 1, AuthenticationExpires: a.now().Add(time.Hour), RequestID: "opaque"})
		if err != nil {
			t.Fatal(err)
		}
		ctx = rebind(t, s, d, a.now)
	}
	c.EvidenceIDs = []shoal.ID{"old-evidence"}
	parent, err := appendCfg(s, ctx, "parent", c)
	if err != nil {
		t.Fatal(err)
	}
	c.Supersedes = parent.ID
	c.EvidenceIDs = []shoal.ID{"new-evidence"}
	root, err := appendCfg(s, ctx, "root", c)
	if err != nil {
		t.Fatal(err)
	}
	target, err := decision.AdjudicationTargetID(a.prediction.Request().TaskID(), a.prediction.Request().PictureID(), c.SubjectID, c.QuestionID)
	if err != nil {
		t.Fatal(err)
	}
	ref := RegisteredReference{TargetID: target, InventoryID: "snapshot:generation-3", ReceiptID: root.ID}
	ra := &adversarialRegisteredAuthority{pred: a.prediction, ref: ref, receipts: map[shoal.ID]Receipt{root.ID: root, parent.ID: parent}, revoked: map[shoal.ID]bool{}}
	readerCtx := rebind(t, s, caller(t, a.now(), "reviewer-actor", 1), a.now)
	wrapped := &adversarialRegisteredBackend{backend: b}
	reader, err := NewRegisteredReader(RegisteredReaderConfig{Backend: wrapped, Resolver: s.config.Resolver, Authority: ra, Clock: a.now})
	if err != nil {
		t.Fatal(err)
	}
	return &adversarialRegisteredFixture{s, b, reader, wrapped, ra, readerCtx, root, parent, ref}
}
func TestRegisteredAdversarialAncestryCurrentAuthorization(t *testing.T) {
	for _, revoked := range []shoal.ID{"new-evidence", "old-evidence"} {
		t.Run(string(revoked), func(t *testing.T) {
			f := newAdversarialRegistered(t, false)
			before := f.backend.writes.Load()
			f.wrapped.hook = func(reads int) {
				if reads == 2 {
					f.authority.revoked[revoked] = true
				}
			}
			got, err := f.reader.ReadRegistered(f.readerCtx, f.ref)
			if !shoal.IsErrorCode(err, shoal.ErrorNotFound) || got.ID != "" {
				t.Fatalf("revoked evidence disclosed: %+v %v", got, err)
			}
			if f.authority.verifies != 1 || len(f.authority.seen) != 2 || f.authority.seen[0].Receipt.ID != f.root.ID || f.authority.seen[1].Receipt.ID != f.parent.ID {
				t.Fatal("final joint authority omitted ancestry")
			}
			if f.backend.writes.Load() != before {
				t.Fatal("registered read wrote state")
			}
		})
	}
}
func TestRegisteredAdversarialMissingAncestorAndSnapshotChange(t *testing.T) {
	for _, mode := range []string{"missing-record", "missing-membership", "snapshot-changed", "ancestor-snapshot", "ancestor-coverage"} {
		t.Run(mode, func(t *testing.T) {
			f := newAdversarialRegistered(t, false)
			switch mode {
			case "missing-record":
				f.backend.mu.Lock()
				delete(f.backend.cells, string(f.parent.ID))
				f.backend.mu.Unlock()
			case "missing-membership":
				delete(f.authority.receipts, f.parent.ID)
			case "snapshot-changed":
				f.wrapped.hook = func(n int) {
					if n == 2 {
						f.authority.stale = true
					}
				}
			case "ancestor-snapshot":
				f.authority.resolveHook = func(b *RegisteredBinding) {
					if b.Reference.ReceiptID == f.parent.ID {
						b.Reference.InventoryID = "other-snapshot"
					}
				}
			case "ancestor-coverage":
				f.authority.resolveHook = func(b *RegisteredBinding) {
					if b.Reference.ReceiptID == f.parent.ID {
						b.CoverageID = "other-coverage"
					}
				}
			}
			got, err := f.reader.ReadRegistered(f.readerCtx, f.ref)
			if !shoal.IsErrorCode(err, shoal.ErrorNotFound) || got.ID != "" {
				t.Fatalf("incomplete/substituted ancestry disclosed: %v", err)
			}
			if f.authority.verifies != 1 {
				t.Fatal("collected prefix skipped final authority")
			}
		})
	}
}
func TestRegisteredAdversarialAuthorityMutationCannotRewriteReceipt(t *testing.T) {
	for _, mutate := range []func([]RegisteredMaterial){func(m []RegisteredMaterial) { m[0].Receipt.ObservationConfig.EvidenceIDs[0] = "rewrite" }, func(m []RegisteredMaterial) { m[0].Receipt.ObservationConfig.Label = "ordinary" }, func(m []RegisteredMaterial) { m[0].Receipt.ActorID = "rewrite" }, func(m []RegisteredMaterial) { m[0].Binding.Reference.InventoryID = "rewrite" }, func(m []RegisteredMaterial) { m[1].Receipt.OnBehalfOf[0] = "rewrite" }} {
		f := newAdversarialRegistered(t, true)
		f.authority.verifyHook = mutate
		f.authority.allowMutation = true
		got, err := f.reader.ReadRegistered(f.readerCtx, f.ref)
		if !shoal.IsErrorCode(err, shoal.ErrorNotFound) || got.ID != "" {
			t.Fatalf("authority mutation disclosed: %v", err)
		}
		f.authority.verifyHook = nil
		f.authority.allowMutation = false
		got, err = f.reader.ReadRegistered(f.readerCtx, f.ref)
		if err != nil || !reflect.DeepEqual(got, f.root) {
			t.Fatalf("mutation changed persisted original: %v", err)
		}
	}
}
func TestRegisteredAdversarialOpaqueIdentityAndOrdinaryReadBoundary(t *testing.T) {
	f := newAdversarialRegistered(t, true)
	before := f.backend.writes.Load()
	got, err := f.reader.ReadRegistered(f.readerCtx, f.ref)
	if err != nil || !reflect.DeepEqual(got, f.root) {
		t.Fatalf("opaque original lost: %v", err)
	}
	if _, err = f.store.Read(f.readerCtx, f.root.ObservationConfig.RequestID, f.root.ObservationConfig.PredictionID, []byte("root")); !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		t.Fatal("ordinary Read acquired cross-principal privilege")
	}
	if f.backend.writes.Load() != before {
		t.Fatal("read changed outcomes")
	}
	other, err := auth.NewDecision(auth.DecisionConfig{Subject: "principal", Actor: "reviewer-actor", AuthorizationDomain: []byte("different-domain"), AllowedOperations: []auth.Operation{auth.OperationRead}, PermittedSourceIDs: [][]byte{[]byte("tasks")}, PermittedPolicyIDs: [][]byte{[]byte("task-policy")}, PolicyGeneration: 1, AuthenticationExpires: f.store.config.Clock().Add(time.Hour), RequestID: "cross-domain"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := rebind(t, f.store, other, f.store.config.Clock)
	f.reader.config.Resolver = f.store.config.Resolver
	if got, err = f.reader.ReadRegistered(ctx, f.ref); !shoal.IsErrorCode(err, shoal.ErrorNotFound) || got.ID != "" {
		t.Fatal("stored reporter bypassed actual caller domain")
	}
}

func TestRegisteredAdversarialExpiryAndCancellationAfterVerification(t *testing.T) {
	for _, cancelInstead := range []bool{false, true} {
		f := newAdversarialRegistered(t, false)
		now := f.store.config.Clock()
		f.reader.config.Clock = func() time.Time { return now }
		ctx, cancel := context.WithCancel(f.readerCtx)
		f.authority.verifyHook = func(_ []RegisteredMaterial) {
			if cancelInstead {
				cancel()
			} else {
				now = now.Add(2 * time.Hour)
			}
		}
		got, err := f.reader.ReadRegistered(ctx, f.ref)
		cancel()
		if !shoal.IsErrorCode(err, shoal.ErrorNotFound) || got.ID != "" {
			t.Fatalf("expired/canceled caller disclosed receipt: %v", err)
		}
		if f.wrapped.reads != 2 {
			t.Fatal("unexpected storage IO after final verification")
		}
	}
}
