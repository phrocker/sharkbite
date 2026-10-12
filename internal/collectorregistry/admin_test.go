// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package collectorregistry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/collector"
	"github.com/phrocker/shoal-oss/pkg/collector/api"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type principal struct {
	subject, actor, client, domain string
	onBehalfOf                     []shoal.ID
	ops                            []auth.Operation
}

func (v *env) bind(t *testing.T, p principal) context.Context {
	t.Helper()
	if p.actor == "" {
		p.actor = p.subject
	}
	if p.client == "" {
		p.client = "admin-client"
	}
	if p.domain == "" {
		p.domain = "domain"
	}
	if p.ops == nil {
		p.ops = []auth.Operation{auth.OperationCollectorAdmin}
	}
	d, e := auth.NewDecision(auth.DecisionConfig{Subject: shoal.ID(p.subject), Actor: shoal.ID(p.actor), ClientID: shoal.ID(p.client), OnBehalfOf: p.onBehalfOf, AuthorizationDomain: []byte(p.domain), AllowedOperations: p.ops, PolicyGeneration: 1, AuthenticationExpires: v.clock.Now().Add(24 * time.Hour), RequestID: "admin-request", CorrelationID: "admin-correlation"})
	if e != nil {
		t.Fatal(e)
	}
	ctx, e := v.authority.Binder().Bind(context.Background(), d)
	if e != nil {
		t.Fatal(e)
	}
	return ctx
}

func (v *env) admin(t *testing.T) context.Context {
	return v.bind(t, principal{subject: "admin"})
}

func grant(id shoal.ID, subject string, authority ...shoal.ID) Provisioning {
	return Provisioning{CollectorID: id, Subject: shoal.ID(subject), ClientID: "client", Domain: []byte("domain"), AuthorityPolicyIDs: authority, Control: collector.ExternalControlled, Mode: collector.ServerObserved}
}

func TestAdminProvisionIsAuditedAndWorksEndToEnd(t *testing.T) {
	v := newEnv(t)
	admin := v.admin(t)
	result, e := v.registry.AdminProvision(admin, grant(collectorID, "tail", "authority:logs"), 1)
	if e != nil {
		t.Fatal(e)
	}
	if !result.Changed || result.Registration.State != collector.Provisioned || result.Registration.Generation != 1 {
		t.Fatalf("result %+v", result)
	}
	// The provisioned principal enrolls within the ceiling and nothing more.
	tail := v.as(t, "tail")
	if _, e := v.registry.Enroll(tail, []byte("k"), enrollRequest(collectorID, "authority:logs", "authority:admin")); !errors.Is(e, api.ErrPermissionDenied) {
		t.Fatalf("excess authority: %v", e)
	}
	if _, e := v.registry.Enroll(tail, []byte("k"), enrollRequest(collectorID, "authority:logs")); e != nil {
		t.Fatal(e)
	}
	changes := v.changes(t, collectorID)
	if len(changes) != 1 {
		t.Fatalf("changes %+v", changes)
	}
	c := changes[0]
	if c.Action != ChangeProvision || c.Source != SourceCollectorAdmin || c.Admin == nil || c.Admin.Subject != "admin" || c.Admin.ClientID != "admin-client" || c.Admin.RequestID != "admin-request" || c.Admin.CorrelationID != "admin-correlation" || c.Admin.AuthorizationFingerprint == "" || !bytes.Equal(c.Admin.Domain, []byte("domain")) {
		t.Fatalf("change %+v admin %+v", c, c.Admin)
	}
	view, e := v.registry.AdminRegistration(admin, collectorID)
	if e != nil {
		t.Fatal(e)
	}
	if view.Registration.State != collector.Enrolled || view.EnrollmentID == "" || len(view.Changes) != 1 || len(view.Gaps) != 0 || len(view.Anomalies) != 0 {
		t.Fatalf("view %+v", view)
	}
	// The Go API is audited too, without a caller identity.
	if _, e := v.registry.Provision(context.Background(), grant("collector:other", "other", "authority:logs")); e != nil {
		t.Fatal(e)
	}
	if got := v.changes(t, "collector:other"); len(got) != 1 || got[0].Source != SourceOperatorAPI || got[0].Admin != nil {
		t.Fatalf("operator change %+v", got)
	}
}

func TestAdminCannotProvisionItself(t *testing.T) {
	v := newEnv(t)
	admin := v.bind(t, principal{subject: "admin", actor: "admin-actor"})
	for name, p := range map[string]Provisioning{
		"own subject and client":    {CollectorID: collectorID, Subject: "admin", ClientID: "admin-client", Domain: []byte("domain"), AuthorityPolicyIDs: []shoal.ID{"authority:logs"}, Control: collector.ExternalControlled, Mode: collector.ServerObserved},
		"own subject, no client":    {CollectorID: collectorID, Subject: "admin", Domain: []byte("domain"), AuthorityPolicyIDs: []shoal.ID{"authority:logs"}, Control: collector.ExternalControlled, Mode: collector.ServerObserved},
		"own subject, other client": grant(collectorID, "admin", "authority:logs"),
		"own actor":                 grant(collectorID, "admin-actor", "authority:logs"),
	} {
		if _, e := v.registry.AdminProvision(admin, p, 1); !errors.Is(e, api.ErrPermissionDenied) {
			t.Fatalf("%s: %v", name, e)
		}
		if v.wrote(t, collectorID) {
			t.Fatalf("%s: self-provisioning wrote a registration", name)
		}
	}
	// A registration the operator made for the admin's subject cannot be
	// reactivated or confirmed by that admin either.
	if _, e := v.registry.Provision(context.Background(), grant(collectorID, "admin", "authority:logs")); e != nil {
		t.Fatal(e)
	}
	if _, e := v.registry.AdminProvision(admin, grant(collectorID, "admin", "authority:logs"), 1); !errors.Is(e, api.ErrPermissionDenied) {
		t.Fatalf("confirm own registration: %v", e)
	}
	if _, e := v.registry.Revoke(context.Background(), collectorID); e != nil {
		t.Fatal(e)
	}
	if _, e := v.registry.AdminProvision(admin, grant(collectorID, "admin", "authority:logs", "authority:more"), 2); !errors.Is(e, api.ErrPermissionDenied) {
		t.Fatalf("reactivate own registration with more authority: %v", e)
	}
	if reg, _ := v.registry.Registration(context.Background(), collectorID); reg.State != collector.Revoked {
		t.Fatalf("own registration reactivated: %+v", reg)
	}
}

func TestOnlyAnUndelegatedCollectorAdminAdministers(t *testing.T) {
	v := newEnv(t)
	v.provision(t, collectorID, "tail", "authority:logs")
	for name, ctx := range map[string]context.Context{
		"unauthenticated":         context.Background(),
		"collector principal":     v.as(t, "tail"),
		"every other operation":   v.bind(t, principal{subject: "other", ops: []auth.Operation{auth.OperationIngest, auth.OperationRead, auth.OperationWorkspaceSettingsWrite, auth.OperationAgentRegister, auth.OperationDelegate, auth.OperationDispatch, auth.OperationInvoke, auth.OperationAgentRevoke}}),
		"delegated admin":         v.bind(t, principal{subject: "admin", onBehalfOf: []shoal.ID{"someone"}}),
		"delegated for the admin": v.bind(t, principal{subject: "agent", onBehalfOf: []shoal.ID{"admin"}}),
	} {
		if _, e := v.registry.AdminProvision(ctx, grant("collector:new", "new", "authority:logs"), 1); !shoal.IsErrorCode(e, shoal.ErrorUnauthorized) {
			t.Fatalf("%s provision: %v", name, e)
		}
		if _, e := v.registry.AdminRevoke(ctx, collectorID, 1); !shoal.IsErrorCode(e, shoal.ErrorUnauthorized) {
			t.Fatalf("%s revoke: %v", name, e)
		}
		if _, e := v.registry.AdminRegistration(ctx, collectorID); !shoal.IsErrorCode(e, shoal.ErrorUnauthorized) {
			t.Fatalf("%s read: %v", name, e)
		}
	}
	if v.wrote(t, "collector:new") {
		t.Fatal("a refused admin wrote a registration")
	}
	if reg, _ := v.registry.Registration(context.Background(), collectorID); reg.State != collector.Provisioned || len(v.changes(t, collectorID)) != 1 {
		t.Fatalf("a refused admin changed the registration: %+v", reg)
	}
	// Collector admin implies nothing on the collector routes.
	if _, e := v.registry.Enroll(v.bind(t, principal{subject: "tail", client: "client"}), []byte("k"), enrollRequest(collectorID, "authority:logs")); !shoal.IsErrorCode(e, shoal.ErrorUnauthorized) {
		t.Fatalf("admin enrolled as a collector: %v", e)
	}
}

// Another domain's collector is indistinguishable from a nonexistent one.
func TestAdminIsConfinedToItsDomain(t *testing.T) {
	v := newEnv(t)
	v.provision(t, collectorID, "tail", "authority:logs")
	other := v.bind(t, principal{subject: "admin", domain: "other-domain"})
	_, absentRevoke := v.registry.AdminRevoke(other, "collector:absent", 1)
	_, absentRead := v.registry.AdminRegistration(other, "collector:absent")
	_, revoke := v.registry.AdminRevoke(other, collectorID, 1)
	_, read := v.registry.AdminRegistration(other, collectorID)
	if !shoal.IsErrorCode(revoke, shoal.ErrorNotFound) || !shoal.IsErrorCode(read, shoal.ErrorNotFound) || revoke.Error() != absentRevoke.Error() || read.Error() != absentRead.Error() {
		t.Fatalf("cross-domain: revoke %v (absent %v), read %v (absent %v)", revoke, absentRevoke, read, absentRead)
	}
	// Provisioning into another domain is refused before anything is read.
	p := grant("collector:new", "new", "authority:logs")
	if _, e := v.registry.AdminProvision(other, p, 1); !errors.Is(e, api.ErrPermissionDenied) {
		t.Fatalf("provision into another domain: %v", e)
	}
	// Collector IDs are one namespace. An ID taken in another domain is a
	// conflict, answered exactly as an identity mismatch in the admin's own.
	p.Domain = []byte("other-domain")
	p.CollectorID = collectorID
	_, taken := v.registry.AdminProvision(other, p, 1)
	_, mismatch := v.registry.AdminProvision(v.admin(t), grant(collectorID, "someone-else", "authority:logs"), 1)
	if !shoal.IsErrorCode(taken, shoal.ErrorConflict) || taken.Error() != mismatch.Error() {
		t.Fatalf("taken %v, mismatch %v", taken, mismatch)
	}
	if reg, _ := v.registry.Registration(context.Background(), collectorID); reg.State != collector.Provisioned || len(v.changes(t, collectorID)) != 1 {
		t.Fatalf("cross-domain admin changed the registration: %+v", reg)
	}
}

func TestAdminProvisionIsIdempotentAndNeverMerges(t *testing.T) {
	v := newEnv(t)
	admin := v.admin(t)
	p := grant(collectorID, "tail", "authority:logs")
	if _, e := v.registry.AdminProvision(admin, p, 1); e != nil {
		t.Fatal(e)
	}
	again, e := v.registry.AdminProvision(admin, p, 1)
	if e != nil || again.Changed || again.Registration.Generation != 1 {
		t.Fatalf("retry %+v %v", again, e)
	}
	for name, change := range map[string]func(*Provisioning, *int64){
		"more authority":    func(p *Provisioning, _ *int64) { p.AuthorityPolicyIDs = append(p.AuthorityPolicyIDs, "authority:more") },
		"other authority":   func(p *Provisioning, _ *int64) { p.AuthorityPolicyIDs = []shoal.ID{"authority:other"} },
		"other control":     func(p *Provisioning, _ *int64) { p.Control = collector.RegistryControlled },
		"other mode":        func(p *Provisioning, _ *int64) { p.Mode = collector.Imported },
		"other subject":     func(p *Provisioning, _ *int64) { p.Subject = "someone" },
		"other client":      func(p *Provisioning, _ *int64) { p.ClientID = "other-client" },
		"future generation": func(_ *Provisioning, g *int64) { *g = 2 },
	} {
		q, generation := p, int64(1)
		q.AuthorityPolicyIDs = slices.Clone(p.AuthorityPolicyIDs)
		change(&q, &generation)
		if _, e := v.registry.AdminProvision(admin, q, generation); !shoal.IsErrorCode(e, shoal.ErrorConflict) {
			t.Fatalf("%s: %v", name, e)
		}
	}
	reg, _ := v.registry.Registration(context.Background(), collectorID)
	if !slices.Equal(reg.AuthorityPolicyIDs, []shoal.ID{"authority:logs"}) || reg.Control != collector.ExternalControlled || reg.Mode != collector.ServerObserved || len(v.changes(t, collectorID)) != 1 {
		t.Fatalf("conflicting requests changed the registration: %+v", reg)
	}
	if _, e := v.registry.AdminProvision(admin, grant("collector:fresh", "fresh", "authority:logs"), 2); !shoal.IsErrorCode(e, shoal.ErrorConflict) || v.wrote(t, "collector:fresh") {
		t.Fatalf("create at generation 2: %v", e)
	}
}

func TestAdminRevokeQuarantinesAtReadTimeAndCannotBeUndoneByRetry(t *testing.T) {
	v := newEnv(t)
	admin := v.admin(t)
	p := grant(collectorID, "tail", "authority:logs")
	if _, e := v.registry.AdminProvision(admin, p, 1); e != nil {
		t.Fatal(e)
	}
	tail := v.as(t, "tail")
	if _, e := v.registry.Enroll(tail, []byte("k"), enrollRequest(collectorID, "authority:logs")); e != nil {
		t.Fatal(e)
	}
	if _, e := v.registry.SubmitArtifact(tail, collectorID, artifact(t, v, "artifact:1", "x")); e != nil {
		t.Fatal(e)
	}
	o, e := v.registry.SubmitObservation(tail, observation(t, v, "artifact:1", v1))
	if e != nil {
		t.Fatal(e)
	}
	if r, e := v.registry.ReadObservation(tail, o.Observation.ID()); e != nil || r.Quarantined {
		t.Fatalf("before revoke: %+v %v", r, e)
	}
	if _, e := v.registry.AdminRevoke(admin, collectorID, 2); !shoal.IsErrorCode(e, shoal.ErrorConflict) {
		t.Fatalf("revoke a future generation: %v", e)
	}
	revoked, e := v.registry.AdminRevoke(admin, collectorID, 1)
	if e != nil || !revoked.Changed || revoked.Registration.State != collector.Revoked || revoked.Registration.Generation != 2 {
		t.Fatalf("revoke %+v %v", revoked, e)
	}
	if r, e := v.registry.ReadObservation(tail, o.Observation.ID()); e != nil || !r.Quarantined {
		t.Fatalf("after revoke: %+v %v", r, e)
	}
	if again, e := v.registry.AdminRevoke(admin, collectorID, 1); e != nil || again.Changed || again.Registration.Generation != 2 {
		t.Fatalf("revoke retry %+v %v", again, e)
	}
	for _, generation := range []int64{2, 3} {
		if _, e := v.registry.AdminRevoke(admin, collectorID, generation); !shoal.IsErrorCode(e, shoal.ErrorConflict) {
			t.Fatalf("revoke generation %d of a registration revoked at 1: %v", generation, e)
		}
	}
	// A retried provisioning from before the revocation cannot undo it.
	if _, e := v.registry.AdminProvision(admin, p, 1); !shoal.IsErrorCode(e, shoal.ErrorConflict) {
		t.Fatalf("stale provision retry: %v", e)
	}
	if reg, _ := v.registry.Registration(context.Background(), collectorID); reg.State != collector.Revoked {
		t.Fatalf("revocation undone: %+v", reg)
	}
	// Reactivation at the current generation, with new terms; the old
	// generation stays quarantined, computed from the current registration.
	reactivated, e := v.registry.AdminProvision(admin, grant(collectorID, "tail", "authority:metrics"), 2)
	if e != nil || !reactivated.Changed || reactivated.Registration.State != collector.Provisioned {
		t.Fatalf("reactivate %+v %v", reactivated, e)
	}
	if r, e := v.registry.ReadObservation(tail, o.Observation.ID()); e != nil || !r.Quarantined {
		t.Fatalf("after reactivation: %+v %v", r, e)
	}
	if _, e := v.registry.AdminRevoke(admin, collectorID, 1); !shoal.IsErrorCode(e, shoal.ErrorConflict) {
		t.Fatalf("revoke an old generation of an active registration: %v", e)
	}
	got := v.changes(t, collectorID)
	if len(got) != 3 || got[0].Action != ChangeProvision || got[1].Action != ChangeRevoke || got[2].Action != ChangeProvision || got[1].Registration.Generation != 2 || got[2].Registration.Generation != 2 {
		t.Fatalf("changes %+v", got)
	}
	view, e := v.registry.AdminRegistration(admin, collectorID)
	if e != nil || len(view.Changes) != 3 || view.Changes[0].Action != ChangeProvision || view.Changes[0].Generation() != 2 || view.Changes[1].Action != ChangeRevoke || view.Changes[2].Generation() != 1 || len(view.Gaps) != 0 || len(view.Anomalies) != 0 {
		t.Fatalf("view %+v %v", view, e)
	}
}

// Concurrent admins: the CAS serializes them, every change has exactly one
// entry, and the log has no gaps.
func TestConcurrentAdminChangesAreSerializedAndEachAudited(t *testing.T) {
	v := newEnv(t)
	admin := v.admin(t)
	p := grant(collectorID, "tail", "authority:logs")
	var wg sync.WaitGroup
	var created, revoked atomic.Int32
	for range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if r, e := v.registry.AdminProvision(admin, p, 1); e == nil && r.Changed {
				created.Add(1)
			}
		}()
		go func() {
			defer wg.Done()
			if r, e := v.registry.AdminRevoke(admin, collectorID, 1); e == nil && r.Changed {
				revoked.Add(1)
			}
		}()
	}
	wg.Wait()
	if created.Load() != 1 || revoked.Load() > 1 {
		t.Fatalf("created %d, revoked %d", created.Load(), revoked.Load())
	}
	got := v.changes(t, collectorID)
	if int32(len(got)) != created.Load()+revoked.Load() {
		t.Fatalf("%d changes for %d writes", len(got), created.Load()+revoked.Load())
	}
	v.cleanLog(t, collectorID)
}

// lostAck applies a mutation and then reports that its outcome is unknown.
type lostAck struct {
	inner interface {
		ReadExact(context.Context, []allocator.Coordinate) ([]allocator.Cell, error)
		CompareAndMutate(context.Context, allocator.Mutation) (allocator.Status, error)
	}
	drop atomic.Bool
}

func (l *lostAck) ScanRowPrefix(ctx context.Context, row, family, start, visibility []byte, limit int) ([]allocator.Cell, error) {
	return l.inner.(rowScanner).ScanRowPrefix(ctx, row, family, start, visibility, limit)
}

func (l *lostAck) ReadExact(ctx context.Context, c []allocator.Coordinate) ([]allocator.Cell, error) {
	return l.inner.ReadExact(ctx, c)
}
func (l *lostAck) CompareAndMutate(ctx context.Context, m allocator.Mutation) (allocator.Status, error) {
	status, e := l.inner.CompareAndMutate(ctx, m)
	if e == nil && l.drop.Load() {
		return allocator.StatusUnknown, allocator.ErrConditionalUnknown
	}
	return status, e
}

// A lost acknowledgement reconciles by reading the exact change entry back,
// so the write reports success once, with one entry.
func TestAdminWriteReconcilesALostAcknowledgement(t *testing.T) {
	v := newEnv(t)
	backend := &lostAck{inner: v.registry.config.Backend}
	config := v.registry.config
	config.Backend = backend
	r, e := New(config)
	if e != nil {
		t.Fatal(e)
	}
	backend.drop.Store(true)
	result, e := r.AdminProvision(v.admin(t), grant(collectorID, "tail", "authority:logs"), 1)
	if e != nil || !result.Changed {
		t.Fatalf("lost-ack provision %+v %v", result, e)
	}
	if got := v.changes(t, collectorID); len(got) != 1 {
		t.Fatalf("changes %+v", got)
	}
}

// An admin identity that cannot be recorded faithfully is refused before
// anything is written, so no entry exists that its own read API would
// refuse.
func TestUnrecordableAdminIdentitiesAreRefused(t *testing.T) {
	for name, p := range map[string]principal{
		"actor not UTF-8":           {subject: "admin", actor: "adm\xff"},
		"actor all 0xff":            {subject: "admin", actor: string(bytes.Repeat([]byte{0xff}, 1000))},
		"subject not UTF-8":         {subject: "adm\xffin"},
		"client not UTF-8":          {subject: "admin", client: "cli\xff"},
		"subject control character": {subject: "adm\x01in"},
	} {
		v := newEnv(t)
		if _, e := v.registry.AdminProvision(v.bind(t, p), grant(collectorID, "tail", "authority:logs"), 1); !shoal.IsErrorCode(e, shoal.ErrorUnauthorized) {
			t.Fatalf("%s: %v", name, e)
		}
		if v.wrote(t, collectorID) {
			t.Fatalf("%s: wrote", name)
		}
	}
	for name, d := range map[string]auth.DecisionConfig{
		"request ID not UTF-8":     {RequestID: "req\xff"},
		"correlation ID not UTF-8": {RequestID: "request", CorrelationID: "corr\xff"},
		"correlation ID space":     {RequestID: "request", CorrelationID: "corr elation"},
	} {
		v := newEnv(t)
		d.Subject, d.Actor, d.ClientID, d.AuthorizationDomain, d.AllowedOperations, d.PolicyGeneration, d.AuthenticationExpires = "admin", "admin", "client", []byte("domain"), []auth.Operation{auth.OperationCollectorAdmin}, 1, v.clock.Now().Add(time.Hour)
		decision, e := auth.NewDecision(d)
		if e != nil {
			t.Fatalf("%s: %v", name, e)
		}
		ctx, _ := v.authority.Binder().Bind(context.Background(), decision)
		if _, e := v.registry.AdminProvision(ctx, grant(collectorID, "tail", "authority:logs"), 1); !shoal.IsErrorCode(e, shoal.ErrorUnauthorized) {
			t.Fatalf("%s: %v", name, e)
		}
		if v.wrote(t, collectorID) {
			t.Fatalf("%s: wrote", name)
		}
	}
	// An OIDC-style subject with spaces is a valid admin.
	v := newEnv(t)
	if _, e := v.registry.AdminProvision(v.bind(t, principal{subject: "oidc:https://issuer#sub# admin 7 "}), grant(collectorID, "tail", "authority:logs"), 1); e != nil {
		t.Fatalf("spaced subject: %v", e)
	}
	if _, e := v.registry.AdminRegistration(v.bind(t, principal{subject: "oidc:https://issuer#sub# admin 7 "}), collectorID); e != nil {
		t.Fatalf("spaced subject read: %v", e)
	}
}

// prepareChange refuses any entry that would not read back as itself, even
// one validChange accepts: here a timestamp with a monotonic reading and a
// non-UTC location, which encoding/json does not preserve.
func TestChangesThatWouldNotReadBackAreNeverWritten(t *testing.T) {
	v := newEnv(t)
	at := time.Now().In(time.FixedZone("x", 3600))
	_, _, e := v.registry.provision(context.Background(), grant(collectorID, "tail", "authority:logs"), 0, attribution{source: SourceOperatorAPI, at: at})
	if !shoal.IsErrorCode(e, shoal.ErrorInvalidArgument) {
		t.Fatalf("non-round-tripping entry: %v", e)
	}
	if v.wrote(t, collectorID) {
		t.Fatal("wrote an entry that cannot be read back")
	}
	// Below the admin check, validChange refuses an attribution that
	// round-trips through JSON but is not printable.
	bad := &AdminAttribution{Subject: "admin", Actor: "a\x01", RequestID: "request", Domain: []byte("domain"), AuthorizationFingerprint: "auth-sha256:00"}
	_, _, e = v.registry.provision(context.Background(), grant(collectorID, "tail", "authority:logs"), 1, attribution{source: SourceCollectorAdmin, admin: bad, at: v.clock.Now()})
	if !shoal.IsErrorCode(e, shoal.ErrorInvalidArgument) || v.wrote(t, collectorID) {
		t.Fatalf("unprintable attribution: %v", e)
	}
}

// interleave runs hook once, right after the first armed read of the
// registration cell returns, so a change lands between that read and the
// head read that follows it.
type interleave struct {
	lostAck
	armed atomic.Bool
	// skip is how many armed registration reads pass before the hook runs.
	skip atomic.Int32
	hook func()
}

func (i *interleave) ReadExact(ctx context.Context, c []allocator.Coordinate) ([]allocator.Cell, error) {
	cells, e := i.inner.ReadExact(ctx, c)
	if len(c) == 1 && string(c[0].Qualifier) == "registration" && i.armed.Load() && i.skip.Add(-1) < 0 && i.armed.CompareAndSwap(true, false) {
		i.hook()
	}
	return cells, e
}

// The admin read is coherent: a revocation landing between its registration
// read and its head read is not returned as a registration that disagrees
// with the newest change.
func TestAdminRegistrationIsCoherent(t *testing.T) {
	v := newEnv(t)
	admin := v.admin(t)
	if _, e := v.registry.AdminProvision(admin, grant(collectorID, "tail", "authority:logs"), 1); e != nil {
		t.Fatal(e)
	}
	backend := &interleave{lostAck: lostAck{inner: v.registry.config.Backend}}
	backend.hook = func() {
		if _, e := v.registry.Revoke(context.Background(), collectorID); e != nil {
			t.Error(e)
		}
	}
	config := v.registry.config
	config.Backend = backend
	r, e := New(config)
	if e != nil {
		t.Fatal(e)
	}
	backend.armed.Store(true)
	view, e := r.AdminRegistration(admin, collectorID)
	if e != nil {
		t.Fatalf("read across a concurrent revoke: %v", e)
	}
	if backend.armed.Load() {
		t.Fatal("the revocation did not interleave; the test is vacuous")
	}
	if view.Registration.State != collector.Revoked || len(view.Changes) != 2 || view.Changes[0].Action != ChangeRevoke || len(view.Gaps) != 0 || len(view.Anomalies) != 0 {
		t.Fatalf("torn view: %+v", view)
	}
}

// put writes value at coord and timestamp unconditionally (test damage).
func put(t *testing.T, v *env, coord allocator.Coordinate, timestamp int64, value []byte) {
	t.Helper()
	cells, e := v.registry.config.Backend.ReadExact(context.Background(), []allocator.Coordinate{coord})
	if e != nil {
		t.Fatal(e)
	}
	condition := allocator.Condition{Coordinate: coord, Absent: true}
	if len(cells) > 0 {
		condition = allocator.Condition{Coordinate: coord, Value: cells[0].Value, Timestamp: cells[0].Timestamp, TimestampSet: true}
	}
	if status, e := v.registry.config.Backend.CompareAndMutate(context.Background(), allocator.Mutation{Row: coord.Row, Conditions: []allocator.Condition{condition}, Updates: []allocator.Update{{Coordinate: coord, Timestamp: timestamp, Value: value}}}); e != nil || status != allocator.StatusAccepted {
		t.Fatal(status, e)
	}
}

func provisionedEnv(t *testing.T) *env {
	t.Helper()
	v := newEnv(t)
	if _, e := v.registry.AdminProvision(v.admin(t), grant(collectorID, "tail", "authority:logs"), 1); e != nil {
		t.Fatal(e)
	}
	return v
}

// deleteCell removes the newest cell at coord.
func deleteCell(t *testing.T, v *env, coord allocator.Coordinate) {
	t.Helper()
	cell, e := v.registry.read(context.Background(), coord)
	if e != nil {
		t.Fatal(e)
	}
	if status, e := v.registry.config.Backend.CompareAndMutate(context.Background(), allocator.Mutation{Row: coord.Row, Conditions: []allocator.Condition{{Coordinate: coord, Value: cell.Value, Timestamp: cell.Timestamp, TimestampSet: true}}, Updates: []allocator.Update{{Coordinate: coord, Timestamp: cell.Timestamp, Delete: true}}}); e != nil || status != allocator.StatusAccepted {
		t.Fatal(status, e)
	}
}

// tombstone leaves a delete marker at coord and timestamp, whether or not a
// cell exists there, conditioned on the collector's registration cell.
func tombstone(t *testing.T, v *env, coord allocator.Coordinate, timestamp int64) {
	t.Helper()
	reg, e := v.registry.read(context.Background(), v.registry.registrationCoordinate(collectorID))
	if e != nil {
		t.Fatal(e)
	}
	if status, e := v.registry.config.Backend.CompareAndMutate(context.Background(), allocator.Mutation{Row: coord.Row, Conditions: []allocator.Condition{{Coordinate: reg.Coordinate, Value: reg.Value, Timestamp: reg.Timestamp, TimestampSet: true}}, Updates: []allocator.Update{{Coordinate: coord, Timestamp: timestamp, Delete: true}}}); e != nil || status != allocator.StatusAccepted {
		t.Fatal(status, e)
	}
}

// Ordinary churn — provisions, revocations and reads racing — never reads as
// damage: no unavailable and no indeterminate outcome.
func TestChurnIsNeverReportedAsDamage(t *testing.T) {
	v := provisionedEnv(t)
	admin := v.admin(t)
	var wg sync.WaitGroup
	var bad atomic.Int32
	report := func(e error) {
		if e != nil && !shoal.IsErrorCode(e, shoal.ErrorConflict) {
			bad.Add(1)
			t.Log(e)
		}
	}
	for worker := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 15 {
				switch worker % 3 {
				case 0:
					reg, e := v.registry.Registration(context.Background(), collectorID)
					report(e)
					if reg.State == collector.Revoked {
						_, e = v.registry.AdminProvision(admin, grant(collectorID, "tail", "authority:logs"), reg.Generation)
					} else {
						_, e = v.registry.AdminRevoke(admin, collectorID, reg.Generation)
					}
					report(e)
				case 1:
					_, e := v.registry.AdminRegistration(admin, collectorID)
					report(e)
				case 2:
					log, e := v.registry.Changes(context.Background(), collectorID, 1, MaxChangesGenerations)
					report(e)
					if e == nil && (len(log.Gaps) != 0 || len(log.Anomalies) != 0) {
						bad.Add(1)
						t.Logf("gaps %v anomalies %v", log.Gaps, log.Anomalies)
					}
				}
			}
		}()
	}
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatalf("%d outcomes reported damage under ordinary churn", bad.Load())
	}
	v.cleanLog(t, collectorID)
}

func wrapped(t *testing.T, v *env, backend interface {
	ReadExact(context.Context, []allocator.Coordinate) ([]allocator.Cell, error)
	CompareAndMutate(context.Context, allocator.Mutation) (allocator.Status, error)
}) *Registry {
	t.Helper()
	config := v.registry.config
	config.Backend = backend
	r, e := New(config)
	if e != nil {
		t.Fatal(e)
	}
	return r
}

// changes returns id's whole change log, oldest first, failing the test on
// a read error.
func (v *env) changes(t *testing.T, id shoal.ID) []Change {
	t.Helper()
	log, e := v.registry.Changes(context.Background(), id, 1, MaxChangesGenerations)
	if e != nil {
		t.Fatal(e)
	}
	return log.Entries
}

// cleanLog asserts id's log has no gaps and no anomalies.
func (v *env) cleanLog(t *testing.T, id shoal.ID) ChangeLog {
	t.Helper()
	log, e := v.registry.Changes(context.Background(), id, 1, MaxChangesGenerations)
	if e != nil || len(log.Gaps) != 0 || len(log.Anomalies) != 0 {
		t.Fatalf("log of %s: gaps %v anomalies %v err %v", id, log.Gaps, log.Anomalies, e)
	}
	return log
}

// logCells lists every change-log cell stored for id.
func (v *env) logCells(t *testing.T, id shoal.ID) []allocator.Cell {
	t.Helper()
	cells, e := v.registry.config.Backend.(rowScanner).ScanRowPrefix(context.Background(), v.registry.registrationCoordinate(id).Row, []byte(changeFamily), nil, nil, 1000)
	if e != nil {
		t.Fatal(e)
	}
	return cells
}

// wrote reports whether id has a registration, and fails if a log exists
// without one.
func (v *env) wrote(t *testing.T, id shoal.ID) bool {
	t.Helper()
	_, e := v.registry.Registration(context.Background(), id)
	if e == nil {
		return true
	}
	if !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		t.Fatal(e)
	}
	if cells := v.logCells(t, id); len(cells) != 0 {
		t.Fatalf("change log written without a registration: %d cells", len(cells))
	}
	return false
}

// entryCoordinate is the coordinate of the stored entry for a slot.
func (v *env) entryCoordinate(t *testing.T, id shoal.ID, generation int64, action ChangeAction) allocator.Coordinate {
	t.Helper()
	for _, cell := range v.logCells(t, id) {
		if g, a, _, ok := parseQualifier(cell.Coordinate.Qualifier); ok && g == generation && a == action {
			return cell.Coordinate
		}
	}
	t.Fatalf("no entry at (%d, %s)", generation, action)
	return allocator.Coordinate{}
}

// cycled provisions id and revokes and reactivates it until it sits at
// generation g, provisioned.
func cycled(t *testing.T, g int64) *env {
	t.Helper()
	v := provisionedEnv(t)
	for range g - 1 {
		if _, e := v.registry.Revoke(context.Background(), collectorID); e != nil {
			t.Fatal(e)
		}
		if _, e := v.registry.Provision(context.Background(), grant(collectorID, "tail", "authority:logs")); e != nil {
			t.Fatal(e)
		}
	}
	return v
}

// The slots a registration implies, and an entry per change in each.
func TestTheLogIsOneEntryPerSlot(t *testing.T) {
	v := cycled(t, 3)
	if _, e := v.registry.Revoke(context.Background(), collectorID); e != nil {
		t.Fatal(e)
	}
	log := v.cleanLog(t, collectorID)
	want := []slot{{1, ChangeProvision}, {2, ChangeRevoke}, {2, ChangeProvision}, {3, ChangeRevoke}, {3, ChangeProvision}, {4, ChangeRevoke}}
	if len(log.Entries) != len(want) {
		t.Fatalf("entries %d", len(log.Entries))
	}
	for i, c := range log.Entries {
		if (slot{c.Generation(), c.Action}) != want[i] {
			t.Fatalf("entry %d at (%d, %s), want %v", i, c.Generation(), c.Action, want[i])
		}
	}
	if cells := v.logCells(t, collectorID); len(cells) != len(want) {
		t.Fatalf("%d cells for %d changes", len(cells), len(want))
	}
	// Ranges.
	part, e := v.registry.Changes(context.Background(), collectorID, 3, 1)
	if e != nil || len(part.Entries) != 2 || part.Entries[0].Generation() != 3 || len(part.Gaps) != 0 {
		t.Fatalf("range %+v %v", part, e)
	}
}

// No cell in the log, whatever it holds, can block a write: every write
// lands at its own nonce-qualified cell and reads nothing else.
func TestNoLogDamageBlocksAWrite(t *testing.T) {
	v := provisionedEnv(t)
	g1 := v.entryCoordinate(t, collectorID, 1, ChangeProvision)
	// Damage of every kind: garbage at the existing entry, strays shaped like
	// the next slots' qualifiers, garbage and markers anywhere in the family.
	put(t, v, g1, 7, []byte("garbage"))
	for _, q := range []string{
		entryQualifier(2, ChangeRevoke, strings.Repeat("0", 32)),
		entryQualifier(2, ChangeProvision, strings.Repeat("f", 32)),
		"change-head", "entry:", "entry:zzz", "\xff\xff",
	} {
		put(t, v, v.registry.changeCoordinate(collectorID, q), 1, []byte("stray"))
	}
	tombstone(t, v, v.registry.changeCoordinate(collectorID, entryQualifier(2, ChangeRevoke, strings.Repeat("1", 32))), math.MaxInt64)
	if _, e := v.registry.AdminRevoke(v.admin(t), collectorID, 1); e != nil {
		t.Fatalf("revoke over a damaged log: %v", e)
	}
	if _, e := v.registry.AdminProvision(v.admin(t), grant(collectorID, "tail", "authority:logs"), 2); e != nil {
		t.Fatalf("provision over a damaged log: %v", e)
	}
	// Reads report the damage and the real entries alike.
	view, e := v.registry.AdminRegistration(v.admin(t), collectorID)
	if e != nil {
		t.Fatalf("read: %v", e)
	}
	if len(view.Changes) != 2 || len(view.Gaps) != 1 || view.Gaps[0] != (LogGap{1, ChangeProvision}) || len(view.Anomalies) == 0 {
		t.Fatalf("view changes %d gaps %v anomalies %v", len(view.Changes), view.Gaps, view.Anomalies)
	}
	reasons := map[AnomalyReason]bool{}
	for _, a := range view.Anomalies {
		reasons[a.Reason] = true
	}
	if !reasons[AnomalyUndecodable] {
		t.Fatalf("anomalies %v", view.Anomalies)
	}
}

// A store without row scans still takes every write; only reads need scans.
func TestWritesNeedNoRowScan(t *testing.T) {
	v := newEnv(t)
	r := wrapped(t, v, &noScan{inner: v.registry.config.Backend})
	if _, e := r.AdminProvision(v.admin(t), grant(collectorID, "tail", "authority:logs"), 1); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Revoke(context.Background(), collectorID); e != nil {
		t.Fatal(e)
	}
	if _, e := r.AdminRegistration(v.admin(t), collectorID); !shoal.IsErrorCode(e, shoal.ErrorUnavailable) {
		t.Fatalf("read without scans: %v", e)
	}
	v.cleanLog(t, collectorID)
}

// noScan hides the store's row scan.
type noScan struct {
	inner interface {
		ReadExact(context.Context, []allocator.Coordinate) ([]allocator.Cell, error)
		CompareAndMutate(context.Context, allocator.Mutation) (allocator.Status, error)
	}
}

func (n *noScan) ReadExact(ctx context.Context, c []allocator.Coordinate) ([]allocator.Cell, error) {
	return n.inner.ReadExact(ctx, c)
}
func (n *noScan) CompareAndMutate(ctx context.Context, m allocator.Mutation) (allocator.Status, error) {
	return n.inner.CompareAndMutate(ctx, m)
}

// A missing entry is a gap: reported, never a reason to refuse a write, and
// later changes are logged normally.
func TestAMissingEntryIsAReportedGap(t *testing.T) {
	v := cycled(t, 2)
	deleteCell(t, v, v.entryCoordinate(t, collectorID, 2, ChangeRevoke))
	tombstone(t, v, v.entryCoordinate(t, collectorID, 1, ChangeProvision), math.MaxInt64)
	if _, e := v.registry.AdminRevoke(v.admin(t), collectorID, 2); e != nil {
		t.Fatalf("revoke: %v", e)
	}
	log, e := v.registry.Changes(context.Background(), collectorID, 1, MaxChangesGenerations)
	if e != nil || len(log.Entries) != 2 || !slices.Equal(log.Gaps, []LogGap{{1, ChangeProvision}, {2, ChangeRevoke}}) || len(log.Anomalies) != 0 {
		t.Fatalf("log %+v %v", log, e)
	}
}

// A registration from before the log existed reads with gaps for every slot
// it implies, and its first logged change is recorded like any other.
func TestALegacyRegistrationReadsWithGaps(t *testing.T) {
	v := newEnv(t)
	if _, e := v.registry.updateRegistration(context.Background(), collectorID, true, func(row *registrationRow, _ bool) (bool, error) {
		*row = registrationRow{Registration: collector.Registration{CollectorID: collectorID, Subject: "tail", ClientID: "client", Domain: []byte("domain"), AuthorityPolicyIDs: []shoal.ID{"authority:logs"}, Control: collector.ExternalControlled, Mode: collector.ServerObserved, Generation: 2, State: collector.Provisioned}}
		return true, nil
	}); e != nil {
		t.Fatal(e)
	}
	view, e := v.registry.AdminRegistration(v.admin(t), collectorID)
	if e != nil || len(view.Changes) != 0 || len(view.Gaps) != 3 || len(view.Anomalies) != 0 {
		t.Fatalf("legacy view %+v %v", view, e)
	}
	if _, e := v.registry.AdminRevoke(v.admin(t), collectorID, 2); e != nil {
		t.Fatal(e)
	}
	log, e := v.registry.Changes(context.Background(), collectorID, 1, MaxChangesGenerations)
	if e != nil || len(log.Entries) != 1 || log.Entries[0].Generation() != 3 || len(log.Gaps) != 3 {
		t.Fatalf("log %+v %v", log, e)
	}
}

// forge writes a copy of the entry at a slot, with fields changed, under a
// fresh attempt, as an outside writer could.
func forge(t *testing.T, v *env, generation int64, action ChangeAction, change func(*Change)) {
	t.Helper()
	cell, e := v.registry.read(context.Background(), v.entryCoordinate(t, collectorID, generation, action))
	if e != nil {
		t.Fatal(e)
	}
	c, e := decodeChange(cell.Value)
	if e != nil {
		t.Fatal(e)
	}
	c.Attempt = strings.Repeat("9", 32)
	change(&c)
	value, e := encodeChange(c)
	if e != nil {
		t.Fatal(e)
	}
	put(t, v, v.registry.changeCoordinate(collectorID, entryQualifier(c.Generation(), c.Action, c.Attempt)), 1, value)
}

// Cells that are not exactly one valid entry for their slot are anomalies,
// each named, and none is presented as an entry.
func TestAnomaliesAreReportedNotPresented(t *testing.T) {
	cases := map[AnomalyReason]func(*env){
		AnomalyDuplicate: func(v *env) { forge(t, v, 1, ChangeProvision, func(*Change) {}) },
		AnomalyUnexpected: func(v *env) {
			forge(t, v, 1, ChangeProvision, func(c *Change) { c.Registration.Generation = 2 })
		},
		AnomalyInconsistent: func(v *env) {
			coord := v.entryCoordinate(t, collectorID, 1, ChangeProvision)
			forge(t, v, 1, ChangeProvision, func(c *Change) { c.Registration.Control = collector.RegistryControlled })
			deleteCell(t, v, coord)
		},
		AnomalyUnknownVersion: func(v *env) {
			value, _ := encode(struct{ Version int }{2})
			put(t, v, v.registry.changeCoordinate(collectorID, entryQualifier(1, ChangeRevoke, strings.Repeat("2", 32))), 1, value)
		},
		AnomalyMisplaced: func(v *env) {
			cell, _ := v.registry.read(context.Background(), v.entryCoordinate(t, collectorID, 1, ChangeProvision))
			put(t, v, v.registry.changeCoordinate(collectorID, entryQualifier(1, ChangeRevoke, strings.Repeat("3", 32))), 1, cell.Value)
		},
	}
	for reason, damage := range cases {
		v := provisionedEnv(t)
		damage(v)
		view, e := v.registry.AdminRegistration(v.admin(t), collectorID)
		if e != nil {
			t.Fatalf("%s: %v", reason, e)
		}
		found := false
		for _, a := range view.Anomalies {
			found = found || a.Reason == reason
		}
		if !found {
			t.Fatalf("%s not reported: %+v", reason, view.Anomalies)
		}
		for _, c := range view.Changes {
			if c.Attempt == strings.Repeat("9", 32) && reason != AnomalyInconsistent {
				t.Fatalf("%s: forged entry presented", reason)
			}
		}
		// The wire projection carries them too.
		provider, _ := NewAdminProvider(v.registry)
		if record, e := provider.ReadRegistration(v.admin(t), collectorID); e != nil || len(record.Anomalies) == 0 {
			t.Fatalf("%s: wire %+v %v", reason, record, e)
		}
		// And writes go on.
		if _, e := v.registry.AdminRevoke(v.admin(t), collectorID, 1); e != nil {
			t.Fatalf("%s: revoke: %v", reason, e)
		}
	}
}

// Entry formats are versioned: a known version decodes strictly, an unknown
// or missing one is refused explicitly, not guessed.
func TestEntryVersions(t *testing.T) {
	v := provisionedEnv(t)
	cell, e := v.registry.read(context.Background(), v.entryCoordinate(t, collectorID, 1, ChangeProvision))
	if e != nil {
		t.Fatal(e)
	}
	if c, e := decodeChange(cell.Value); e != nil || c.Version != 1 {
		t.Fatalf("v1: %+v %v", c, e)
	}
	for _, version := range []int{0, 2, -1} {
		value, _ := encode(struct{ Version int }{version})
		if _, e := decodeChange(value); !errors.Is(e, errUnknownVersion) {
			t.Fatalf("version %d: %v", version, e)
		}
	}
	if _, e := decodeChange([]byte("garbage")); e == nil || errors.Is(e, errUnknownVersion) {
		t.Fatalf("garbage: %v", e)
	}
}

// beforeWrite runs hook once, just before the first armed change-log
// mutation reaches the store.
type beforeWrite struct {
	lostAck
	armed atomic.Bool
	hook  func()
}

func (b *beforeWrite) CompareAndMutate(ctx context.Context, m allocator.Mutation) (allocator.Status, error) {
	logs := false
	for _, u := range m.Updates {
		logs = logs || string(u.Coordinate.Family) == changeFamily
	}
	if logs && b.armed.CompareAndSwap(true, false) {
		b.hook()
	}
	return b.inner.CompareAndMutate(ctx, m)
}

// A writer that loses the race to an identical change (same admin, same
// instant, so byte-identical registrations) is told apart by its own
// attempt's cell: it reports no change of its own, and there is one entry.
func TestAnIdenticalWinnerIsNotMistakenForOurWrite(t *testing.T) {
	v := newEnv(t)
	admin := v.admin(t)
	p := grant(collectorID, "tail", "authority:logs")
	backend := &beforeWrite{lostAck: lostAck{inner: v.registry.config.Backend}}
	backend.hook = func() {
		if r, e := v.registry.AdminProvision(admin, p, 1); e != nil || !r.Changed {
			t.Errorf("winner %+v %v", r, e)
		}
	}
	backend.armed.Store(true)
	result, e := wrapped(t, v, backend).AdminProvision(admin, p, 1)
	if e != nil || result.Changed {
		t.Fatalf("loser reported %+v %v", result, e)
	}
	if got := v.changes(t, collectorID); len(got) != 1 {
		t.Fatalf("%d entries", len(got))
	}
}

// validChange holds the slot invariants: a revocation enters a generation
// of at least 2 and leaves the registration revoked; a provisioning leaves it
// provisioned; the version is this build's.
func TestValidChangeHoldsSlotInvariants(t *testing.T) {
	v := provisionedEnv(t)
	good := v.changes(t, collectorID)[0]
	if validChange(good, collectorID) != nil {
		t.Fatal("a stored entry is invalid")
	}
	for name, mutate := range map[string]func(*Change){
		"other version":       func(c *Change) { c.Version = 2 },
		"revoke into 1":       func(c *Change) { c.Action = ChangeRevoke; c.Registration.State = collector.Revoked },
		"revoke not revoked":  func(c *Change) { c.Action = ChangeRevoke; c.Registration.Generation = 2 },
		"provision revoked":   func(c *Change) { c.Registration.State = collector.Revoked },
		"provision enrolled":  func(c *Change) { c.Registration.State = collector.Enrolled },
		"short attempt":       func(c *Change) { c.Attempt = "00" },
		"other collector":     func(c *Change) { c.CollectorID = "collector:other" },
		"operator with admin": func(c *Change) { c.Source = SourceOperatorAPI },
	} {
		c := good
		mutate(&c)
		if validChange(c, collectorID) == nil {
			t.Fatalf("%s accepted", name)
		}
	}
}

// The version 1 entry format is pinned: a fixed entry encodes to the
// committed bytes, and those bytes decode back to it. Changing the live
// types cannot move it; changing the format takes a new version.
//
// Regenerate only for a deliberate new version:
// SHOAL_UPDATE_GOLDEN=1 go test ./internal/collectorregistry -run TestChangeV1FormatIsPinned
func TestChangeV1FormatIsPinned(t *testing.T) {
	c := Change{Version: 1, CollectorID: collectorID, Action: ChangeProvision, Source: SourceCollectorAdmin,
		Registration: collector.Registration{CollectorID: collectorID, Subject: "tail", ClientID: "client", Domain: []byte("domain"), AuthorityPolicyIDs: []shoal.ID{"authority:logs"}, Control: collector.ExternalControlled, Mode: collector.ServerObserved, Generation: 1, State: collector.Provisioned},
		Admin:        &AdminAttribution{Subject: "admin", Actor: "admin", ClientID: "admin-client", Domain: []byte("domain"), RequestID: "request", CorrelationID: "correlation", AuthorizationFingerprint: "auth-sha256:" + strings.Repeat("0", 64)},
		At:           time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC), Attempt: strings.Repeat("a", 32)}
	got, e := encodeChange(c)
	if e != nil {
		t.Fatal(e)
	}
	path := "testdata/change_v1.golden.json"
	if os.Getenv("SHOAL_UPDATE_GOLDEN") == "1" {
		if e := os.WriteFile(path, append(bytes.Clone(got), '\n'), 0o644); e != nil {
			t.Fatal(e)
		}
	}
	want, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	want = bytes.TrimSuffix(want, []byte("\n"))
	if !bytes.Equal(got, want) {
		t.Fatalf("version 1 encoding changed:\n got %s\nwant %s", got, want)
	}
	back, e := decodeChange(want)
	if e != nil || !reflect.DeepEqual(back, c) {
		t.Fatalf("decoded %+v %v", back, e)
	}
}

// An entry the registry accepts is one the HTTP read can present: cells
// outside the wire's bounds are anomalies, and the read still succeeds.
func TestEntriesOutsideTheWireBoundsAreAnomalies(t *testing.T) {
	for name, change := range map[string]func(*Change){
		"year 0":           func(c *Change) { c.At = time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC) },
		"long fingerprint": func(c *Change) { c.Admin.AuthorizationFingerprint = strings.Repeat("f", 129) },
	} {
		v := provisionedEnv(t)
		coord := v.entryCoordinate(t, collectorID, 1, ChangeProvision)
		forge(t, v, 1, ChangeProvision, change)
		deleteCell(t, v, coord)
		provider, _ := NewAdminProvider(v.registry)
		record, e := provider.ReadRegistration(v.admin(t), collectorID)
		if e != nil || len(record.Changes) != 0 || len(record.Anomalies) != 1 || record.Anomalies[0].Reason != api.AnomalyUndecodable {
			t.Fatalf("%s: %+v %v", name, record, e)
		}
	}
}

// A bounded anomaly listing keeps the anomalies about presented or withheld
// entries, and says how many it left out.
func TestAnomalyTruncationKeepsTheImportantOnes(t *testing.T) {
	v := provisionedEnv(t)
	coord := v.entryCoordinate(t, collectorID, 1, ChangeProvision)
	forge(t, v, 1, ChangeProvision, func(c *Change) { c.Registration.Control = collector.RegistryControlled })
	deleteCell(t, v, coord)
	for i := range 70 {
		// Strays in later generations, which sort ahead of generation 1 by
		// generation alone.
		put(t, v, v.registry.changeCoordinate(collectorID, entryQualifier(int64(2+i), ChangeProvision, fmt.Sprintf("%032x", i))), 1, []byte("stray"))
	}
	provider, _ := NewAdminProvider(v.registry)
	record, e := provider.ReadRegistration(v.admin(t), collectorID)
	if e != nil || len(record.Anomalies) != api.MaxRegistrationAnomalies || record.MoreAnomalies != 71-api.MaxRegistrationAnomalies || record.Anomalies[0].Reason != api.AnomalyInconsistent {
		t.Fatalf("record anomalies %d more %d first %+v err %v", len(record.Anomalies), record.MoreAnomalies, record.Anomalies[0], e)
	}
}

// afterApply applies a mutation, runs hook, then reports the outcome as
// unknown.
type afterApply struct {
	lostAck
	armed atomic.Bool
	hook  func()
}

func (a *afterApply) CompareAndMutate(ctx context.Context, m allocator.Mutation) (allocator.Status, error) {
	status, e := a.inner.CompareAndMutate(ctx, m)
	if e == nil && a.armed.CompareAndSwap(true, false) {
		a.hook()
		return allocator.StatusUnknown, allocator.ErrConditionalUnknown
	}
	return status, e
}

// A committed change whose acknowledgement was lost is reported as made
// even when the registration has moved on since (an enrollment), because
// the attempt's own entry cell proves it.
func TestALostAckIsReconciledAfterTheRowMovedOn(t *testing.T) {
	v := newEnv(t)
	backend := &afterApply{lostAck: lostAck{inner: v.registry.config.Backend}}
	backend.hook = func() {
		if _, e := v.registry.Enroll(v.as(t, "tail"), []byte("k"), enrollRequest(collectorID, "authority:logs")); e != nil {
			t.Error(e)
		}
	}
	backend.armed.Store(true)
	result, e := wrapped(t, v, backend).AdminProvision(v.admin(t), grant(collectorID, "tail", "authority:logs"), 1)
	if e != nil || !result.Changed {
		t.Fatalf("provision %+v %v", result, e)
	}
	v.cleanLog(t, collectorID)
}

// The frozen version 1 types carry exactly the live types' fields, so a field
// added to the live types fails here instead of making every audited write
// refuse its own entry at run time; such a change takes a new version.
func TestFrozenV1TypesMatchTheLiveTypes(t *testing.T) {
	for _, pair := range [][2]reflect.Type{
		{reflect.TypeFor[Change](), reflect.TypeFor[changeV1]()},
		{reflect.TypeFor[collector.Registration](), reflect.TypeFor[registrationV1]()},
		{reflect.TypeFor[AdminAttribution](), reflect.TypeFor[attributionV1]()},
	} {
		live, frozen := pair[0], pair[1]
		if live.NumField() != frozen.NumField() {
			t.Fatalf("%s has %d fields, %s %d: add a new entry version", live, live.NumField(), frozen, frozen.NumField())
		}
		for i := range live.NumField() {
			if live.Field(i).Name != frozen.Field(i).Name {
				t.Fatalf("%s field %d is %s, %s has %s: add a new entry version", live, i, live.Field(i).Name, frozen, frozen.Field(i).Name)
			}
		}
	}
	if _, e := (&Registry{}).Changes(context.Background(), collectorID, math.MaxInt64-1, 2); !shoal.IsErrorCode(e, shoal.ErrorInvalidArgument) {
		t.Fatalf("overflowing range: %v", e)
	}
}
