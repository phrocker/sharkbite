// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package collectorregistry

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/decisionstore"
	"github.com/phrocker/shoal-oss/pkg/collector"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type materialGate struct {
	deny      bool
	calls     int
	verify    func([]Material)
	authorize func([]shoal.ID)
}

func (g *materialGate) Authorize(_ context.Context, d auth.Decision, p shoal.ID, ids []shoal.ID) error {
	if g.authorize != nil {
		g.authorize(ids)
	}
	if g.deny || p != "purpose:decision" {
		return auth.ObjectNotFound()
	}
	return nil
}
func (g *materialGate) Verify(_ context.Context, d auth.Decision, p shoal.ID, m []Material) error {
	g.calls++
	if g.verify != nil {
		g.verify(m)
	}
	if g.deny {
		return auth.ObjectNotFound()
	}
	return nil
}

type materialProbe struct {
	decisionstore.CAS
	reads, writes int
	hook          func(int)
	change        func(allocator.Coordinate, []allocator.Cell) []allocator.Cell
}

func (p *materialProbe) ReadExact(c context.Context, coords []allocator.Coordinate) ([]allocator.Cell, error) {
	cells, e := p.CAS.ReadExact(c, coords)
	p.reads++
	if p.hook != nil {
		p.hook(p.reads)
	}
	if p.change != nil && len(coords) == 1 {
		cells = p.change(coords[0], cells)
	}
	return cells, e
}
func (p *materialProbe) CompareAndMutate(c context.Context, m allocator.Mutation) (allocator.Status, error) {
	p.writes++
	return p.CAS.CompareAndMutate(c, m)
}
func materialFixture(t *testing.T) (*env, *MaterialResolver, *materialGate, context.Context, collector.Observation) {
	t.Helper()
	v := newEnv(t)
	v.provision(t, collectorID, "tail", "authority:logs")
	ctx := v.as(t, "tail")
	if _, e := v.registry.Enroll(ctx, []byte("key"), enrollRequest(collectorID, "authority:logs")); e != nil {
		t.Fatal(e)
	}
	if _, e := v.registry.SubmitArtifact(ctx, collectorID, artifact(t, v, "artifact:one", "hello")); e != nil {
		t.Fatal(e)
	}
	o := observation(t, v, "artifact:one", v1)
	if _, e := v.registry.SubmitObservation(ctx, o); e != nil {
		t.Fatal(e)
	}
	gate := &materialGate{}
	r, e := NewMaterialResolver(MaterialResolverConfig{Registry: v.registry, Authority: gate})
	if e != nil {
		t.Fatal(e)
	}
	return v, r, gate, v.as(t, "reader", auth.OperationRead), o
}
func TestMaterialActualReaderAndDetachedBatch(t *testing.T) {
	v, r, g, ctx, o := materialFixture(t)
	p := &materialProbe{CAS: v.registry.config.Backend}
	v.registry.config.Backend = p
	g.authorize = func(ids []shoal.ID) { ids[0] = "mutated-callback-copy" }
	got, e := r.Resolve(ctx, "purpose:decision", []shoal.ID{o.ID()})
	if e != nil || len(got) != 1 {
		t.Fatalf("%v %#v", e, got)
	}
	if p.writes != 0 || g.calls != 1 || p.reads != 5 {
		t.Fatalf("IO %d/%d verify%d", p.reads, p.writes, g.calls)
	}
	if got[0].Observation.Observation.ID() != o.ID() || got[0].Artifact.Ref.Size != 5 {
		t.Fatal("wrong material")
	}
	src, e := Source(got[0].Registration, got[0].Enrollment, got[0].Observation, "authority:logs")
	if e != nil || src.AttestationID != "" || src.Digest != got[0].Artifact.Ref.Digest {
		t.Fatal("mapping")
	}
	got[0].Registration.Domain[0] = 'x'
	got[0].Enrollment.Request.Extractors[0].Version = "changed"
	again, e := r.Resolve(ctx, "purpose:decision", []shoal.ID{o.ID()})
	if e != nil || !bytes.Equal(again[0].Registration.Domain, []byte("domain")) || again[0].Enrollment.Request.Extractors[0].Version != "1" {
		t.Fatal("aliased")
	}
}
func TestMaterialRequiresSeparateAuthorityAndRead(t *testing.T) {
	v, r, g, ctx, o := materialFixture(t)
	if _, e := NewMaterialResolver(MaterialResolverConfig{Registry: v.registry}); e == nil {
		t.Fatal("missing authority")
	}
	var nilGate *materialGate
	if _, e := NewMaterialResolver(MaterialResolverConfig{Registry: v.registry, Authority: nilGate}); e == nil {
		t.Fatal("typed nil")
	}
	g.deny = true
	p := &materialProbe{CAS: v.registry.config.Backend}
	v.registry.config.Backend = p
	if _, e := r.Resolve(ctx, "purpose:decision", []shoal.ID{o.ID()}); !shoal.IsErrorCode(e, shoal.ErrorNotFound) || p.reads != 0 {
		t.Fatal("domain-read alone admitted")
	}
	g.deny = false
	if _, e := r.Resolve(v.as(t, "reader", auth.OperationIngest), "purpose:decision", []shoal.ID{o.ID()}); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		t.Fatal("missing read")
	}
	if _, e := r.Resolve(context.Background(), "purpose:decision", []shoal.ID{o.ID()}); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		t.Fatal("unbound")
	}
}
func TestMaterialCurrentEnrollmentOnly(t *testing.T) {
	v, r, _, ctx, o := materialFixture(t)
	if _, e := v.registry.Enroll(v.as(t, "tail"), []byte("second"), enrollRequest(collectorID, "authority:logs")); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Resolve(ctx, "purpose:decision", []shoal.ID{o.ID()}); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		t.Fatalf("superseded enrollment: %v", e)
	}
	if _, e := v.registry.Revoke(context.Background(), collectorID); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Resolve(ctx, "purpose:decision", []shoal.ID{o.ID()}); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		t.Fatalf("revoked: %v", e)
	}
}
func TestMaterialRevocationDuringReadsAndFinalVerification(t *testing.T) {
	for _, at := range []int{1, 2, 3, 4, 5, 6} {
		t.Run(string(rune('0'+at)), func(t *testing.T) {
			v, r, g, ctx, o := materialFixture(t)
			p := &materialProbe{CAS: v.registry.config.Backend}
			v.registry.config.Backend = p
			p.hook = func(n int) {
				if n == at {
					g.deny = true
				}
			}
			if at == 6 {
				g.verify = func([]Material) { g.deny = true }
			}
			got, e := r.Resolve(ctx, "purpose:decision", []shoal.ID{o.ID()})
			if !shoal.IsErrorCode(e, shoal.ErrorNotFound) || got != nil || p.writes != 0 {
				t.Fatalf("%v %v writes%d", got, e, p.writes)
			}
		})
	}
}
func TestMaterialRegistrationChangedOnFinalRead(t *testing.T) {
	v, r, _, ctx, o := materialFixture(t)
	p := &materialProbe{CAS: v.registry.config.Backend}
	v.registry.config.Backend = p
	p.hook = func(n int) {
		if n == 4 {
			p.hook = nil
			if _, e := v.registry.Revoke(context.Background(), collectorID); e != nil {
				t.Fatal(e)
			}
		}
	}
	got, e := r.Resolve(ctx, "purpose:decision", []shoal.ID{o.ID()})
	if !shoal.IsErrorCode(e, shoal.ErrorNotFound) || got != nil {
		t.Fatalf("%v %v", got, e)
	}
}
func TestMaterialVerifierCannotMutateReturn(t *testing.T) {
	for _, mode := range []string{"registration", "enrollment", "observation", "artifact"} {
		t.Run(mode, func(t *testing.T) {
			_, r, g, ctx, o := materialFixture(t)
			g.verify = func(m []Material) {
				switch mode {
				case "registration":
					m[0].Registration.Domain[0] = 'x'
				case "enrollment":
					m[0].Enrollment.Request.Extractors[0].Version = "changed"
				case "observation":
					c := m[0].Observation.Observation.Config()
					c.Payload = []byte("changed")
					m[0].Observation.Observation, _ = collector.NewObservation(c)
				case "artifact":
					m[0].Artifact.Ref.Size++
				}
			}
			if got, e := r.Resolve(ctx, "purpose:decision", []shoal.ID{o.ID()}); e == nil || got != nil {
				t.Fatal("mutated authority output escaped")
			}
		})
	}
}
func TestMaterialStorageFailureMaskedAfterRevocation(t *testing.T) {
	v, r, g, ctx, o := materialFixture(t)
	p := &materialProbe{CAS: v.registry.config.Backend}
	v.registry.config.Backend = p
	p.change = func(_ allocator.Coordinate, c []allocator.Cell) []allocator.Cell { g.deny = true; return nil }
	if got, e := r.Resolve(ctx, "purpose:decision", []shoal.ID{o.ID()}); got != nil || !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		t.Fatalf("%v %v", got, e)
	}
}
func TestMaterialExpiryCancellationAndBounds(t *testing.T) {
	for _, mode := range []string{"expiry", "cancel", "oversize", "shape"} {
		t.Run(mode, func(t *testing.T) {
			v, r, g, ctx, o := materialFixture(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			switch mode {
			case "expiry":
				g.verify = func([]Material) { v.clock.Advance(25 * time.Hour) }
			case "cancel":
				g.verify = func([]Material) { cancel() }
			default:
				p := &materialProbe{CAS: v.registry.config.Backend}
				v.registry.config.Backend = p
				p.change = func(_ allocator.Coordinate, c []allocator.Cell) []allocator.Cell {
					if len(c) > 0 {
						c = append([]allocator.Cell(nil), c...)
						if mode == "oversize" {
							c[0].Value = make([]byte, maxStoredBytes+1)
						} else {
							c[0].Value = []byte(`[[[[[[[[[[[[[[[[[[0]]]]]]]]]]]]]]]]]]`)
						}
					}
					return c
				}
			}
			got, e := r.Resolve(ctx, "purpose:decision", []shoal.ID{o.ID()})
			if e == nil || got != nil {
				t.Fatal("accepted")
			}
		})
	}
	_, r, _, ctx, o := materialFixture(t)
	if _, e := r.Resolve(ctx, "purpose:decision", []shoal.ID{o.ID(), o.ID()}); e == nil {
		t.Fatal("duplicate")
	}
	if _, e := r.Resolve(ctx, "purpose:decision", make([]shoal.ID, MaxMaterials+1)); e == nil {
		t.Fatal("count")
	}
}
func TestMaterialJointBatchAndNoIOAfterVerify(t *testing.T) {
	v, r, g, ctx, o := materialFixture(t)
	writer := v.as(t, "tail")
	if _, e := v.registry.SubmitArtifact(writer, collectorID, artifact(t, v, "artifact:two", "second")); e != nil {
		t.Fatal(e)
	}
	o2 := observation(t, v, "artifact:two", v1)
	if _, e := v.registry.SubmitObservation(writer, o2); e != nil {
		t.Fatal(e)
	}
	p := &materialProbe{CAS: v.registry.config.Backend}
	v.registry.config.Backend = p
	reads := 0
	g.verify = func(m []Material) {
		reads = p.reads
		if len(m) != 2 || m[0].Observation.Observation.ID() != o.ID() || m[1].Observation.Observation.ID() != o2.ID() {
			t.Fatal("partial/order")
		}
		g.deny = true
	}
	got, e := r.Resolve(ctx, "purpose:decision", []shoal.ID{o.ID(), o2.ID()})
	if !shoal.IsErrorCode(e, shoal.ErrorNotFound) || got != nil || p.reads != reads || p.writes != 0 {
		t.Fatalf("%v reads%d/%d", e, reads, p.reads)
	}
}
func TestMaterialRechecksBoundEnrollmentRow(t *testing.T) {
	v, r, _, ctx, o := materialFixture(t)
	p := &materialProbe{CAS: v.registry.config.Backend}
	v.registry.config.Backend = p
	p.change = func(coord allocator.Coordinate, c []allocator.Cell) []allocator.Cell {
		if string(coord.Qualifier) == "enrollment" && len(c) == 1 {
			var e enrollmentRow
			if decode(c[0].Value, &e) != nil {
				t.Fatal("decode")
			}
			e.Enrollment.Request.Extractors[0].Version = "substituted"
			c = append([]allocator.Cell(nil), c...)
			c[0].Value, _ = encode(e)
		}
		return c
	}
	got, e := r.Resolve(ctx, "purpose:decision", []shoal.ID{o.ID()})
	if got != nil || !errors.Is(e, ErrUnavailable) {
		t.Fatalf("%v %v", got, e)
	}
}
func TestMaterialShapeValidAndBounded(t *testing.T) {
	for _, raw := range [][]byte{[]byte(`{"x":[1,"ok",null]}`), []byte(`null`)} {
		if e := materialShape(raw); e != nil {
			t.Fatal(e)
		}
	}
	if materialShape([]byte("["+strings.Repeat("0,", 1024)+"0]")) == nil {
		t.Fatal("unbounded array")
	}
}

func TestMaterialStoredDomainAndGeneration(t *testing.T) {
	v, r, _, ctx, o := materialFixture(t)
	d, e := auth.NewDecision(auth.DecisionConfig{Subject: "reader", Actor: "reader", RequestID: "material-test", AuthorizationDomain: []byte("other-domain"), AllowedOperations: []auth.Operation{auth.OperationRead}, PolicyGeneration: 1, AuthenticationExpires: v.clock.Now().Add(time.Hour)})
	if e != nil {
		t.Fatal(e)
	}
	other, e := v.authority.Binder().Bind(context.Background(), d)
	if e != nil {
		t.Fatal(e)
	}
	if got, e := r.Resolve(other, "purpose:decision", []shoal.ID{o.ID()}); got != nil || !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		t.Fatalf("crossdomain %v", e)
	}
	if _, e = v.registry.Revoke(context.Background(), collectorID); e != nil {
		t.Fatal(e)
	}
	v.provision(t, collectorID, "tail", "authority:logs")
	if _, e = v.registry.Enroll(v.as(t, "tail"), []byte("newgeneration"), enrollRequest(collectorID, "authority:logs")); e != nil {
		t.Fatal(e)
	}
	if got, e := r.Resolve(ctx, "purpose:decision", []shoal.ID{o.ID()}); got != nil || !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		t.Fatalf("old generation %v", e)
	}
}
func TestMaterialPreservesAttestationCoverageWithoutPromotingIt(t *testing.T) {
	v := newEnv(t)
	v.provision(t, collectorID, "tail", "authority:logs")
	ctx := v.as(t, "tail")
	req := enrollRequest(collectorID, "authority:logs")
	req.Attestation = signedAttestation(t, v, []byte("key"), v.clock.Now().Add(time.Minute))
	if _, e := v.registry.Enroll(ctx, []byte("key"), req); e != nil {
		t.Fatal(e)
	}
	if _, e := v.registry.SubmitArtifact(ctx, collectorID, artifact(t, v, "artifact:attested", "raw")); e != nil {
		t.Fatal(e)
	}
	early := observation(t, v, "artifact:attested", v1)
	if _, e := v.registry.SubmitObservation(ctx, early); e != nil {
		t.Fatal(e)
	}
	v.clock.Advance(2 * time.Minute)
	late := observation(t, v, "artifact:attested", v2)
	if _, e := v.registry.SubmitObservation(ctx, late); e != nil {
		t.Fatal(e)
	}
	r, _ := NewMaterialResolver(MaterialResolverConfig{Registry: v.registry, Authority: &materialGate{}})
	got, e := r.Resolve(v.as(t, "reader"), "purpose:decision", []shoal.ID{early.ID(), late.ID()})
	if e != nil {
		t.Fatal(e)
	}
	for i, m := range got {
		src, e := Source(m.Registration, m.Enrollment, m.Observation, "authority:logs")
		if e != nil || (src.AttestationID == "") != (i == 1) {
			t.Fatalf("coverage%d %v", i, e)
		}
	}
}

func TestMaterialCumulativeBudgetBeforeDecode(t *testing.T) {
	v, _, _, ctx, o := materialFixture(t)
	b := &materialBackend{CAS: v.registry.config.Backend, used: MaxMaterialBytes - 1}
	cells, e := b.ReadExact(ctx, []allocator.Coordinate{v.registry.observationCoordinate(o.ID())})
	if e == nil || cells != nil || b.used != MaxMaterialBytes-1 {
		t.Fatalf("budget exceeded: used%d err%v", b.used, e)
	}
}
