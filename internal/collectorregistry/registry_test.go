// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package collectorregistry

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/internal/collectorattest"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/collector"
	"github.com/phrocker/shoal-oss/pkg/collector/api"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type env struct {
	registry  *Registry
	authority *auth.Authority
	clock     *clock
	signer    ed25519.PrivateKey
}

const collectorID = shoal.ID("collector:tail")

// newEnv opens a real engine-backed registry.
func newEnv(t *testing.T) *env {
	t.Helper()
	c := &clock{now: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}
	eng, e := engine.Open(t.TempDir(), engine.Options{})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = eng.Close() })
	if e = eng.CreateTable(Table, engine.TableOptions{}); e != nil {
		t.Fatal(e)
	}
	backend, e := explorercoord.NewEngineStore(eng, Table)
	if e != nil {
		t.Fatal(e)
	}
	authority, e := auth.NewAuthorityWithClock(c.Now)
	if e != nil {
		t.Fatal(e)
	}
	private := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	verifier, e := collectorattest.NewEd25519Statement(collectorattest.Ed25519Config{VerifierID: "operator-key:1", PublicKey: private.Public().(ed25519.PublicKey), MaxValidity: time.Hour})
	if e != nil {
		t.Fatal(e)
	}
	set, e := collectorattest.NewSet(verifier)
	if e != nil {
		t.Fatal(e)
	}
	r, e := New(Config{Backend: backend, Resolver: authority.Resolver(), Attestation: set, Clock: c.Now})
	if e != nil {
		t.Fatal(e)
	}
	return &env{registry: r, authority: authority, clock: c, signer: private}
}

func (v *env) as(t *testing.T, subject string, ops ...auth.Operation) context.Context {
	t.Helper()
	if len(ops) == 0 {
		ops = []auth.Operation{auth.OperationIngest, auth.OperationRead}
	}
	d, e := auth.NewDecision(auth.DecisionConfig{Subject: shoal.ID(subject), Actor: shoal.ID(subject), ClientID: "client", AuthorizationDomain: []byte("domain"), AllowedOperations: ops, PolicyGeneration: 1, AuthenticationExpires: v.clock.Now().Add(24 * time.Hour), RequestID: "request"})
	if e != nil {
		t.Fatal(e)
	}
	ctx, e := v.authority.Binder().Bind(context.Background(), d)
	if e != nil {
		t.Fatal(e)
	}
	return ctx
}

func (v *env) provision(t *testing.T, id shoal.ID, subject string, authority ...shoal.ID) {
	t.Helper()
	if _, e := v.registry.Provision(context.Background(), Provisioning{CollectorID: id, Subject: shoal.ID(subject), ClientID: "client", Domain: []byte("domain"), AuthorityPolicyIDs: authority, Control: collector.ExternalControlled, Mode: collector.ServerObserved}); e != nil {
		t.Fatal(e)
	}
}

var v1 = collector.ExtractorRef{ID: "extractor:lines", Version: "1"}
var v2 = collector.ExtractorRef{ID: "extractor:lines", Version: "2"}

func enrollRequest(id shoal.ID, authority ...shoal.ID) collector.EnrollRequest {
	return collector.EnrollRequest{CollectorID: id, RequestedAuthorityPolicyIDs: authority, Extractors: []collector.ExtractorRef{v1, v2}}
}

func artifact(t *testing.T, v *env, id string, body string) collector.ArtifactRef {
	t.Helper()
	sum := sha256.Sum256([]byte(body))
	return collector.ArtifactRef{ID: shoal.ID(id), Digest: hex.EncodeToString(sum[:]), Size: int64(len(body)), MediaType: "text/plain", ObservedAt: v.clock.Now().Add(-time.Second)}
}

func observation(t *testing.T, v *env, artifactID string, extractor collector.ExtractorRef) collector.Observation {
	t.Helper()
	value := 0.9
	o, e := collector.NewObservation(collector.ObservationConfig{CollectorID: collectorID, ArtifactID: shoal.ID(artifactID), Extractor: extractor, Confidence: collector.Confidence{Disposition: collector.Extracted, Value: &value}, SubjectID: "host:web-1", Kind: "log_line", Payload: []byte(`{"line":"ok"}`), ObservedAt: v.clock.Now()})
	if e != nil {
		t.Fatal(e)
	}
	return o
}

func TestEnrollRefusesExcessAuthorityAndWritesNothing(t *testing.T) {
	v := newEnv(t)
	v.provision(t, collectorID, "tail", "authority:logs")
	ctx := v.as(t, "tail")
	_, e := v.registry.Enroll(ctx, []byte("key-1"), enrollRequest(collectorID, "authority:logs", "authority:admin"))
	if !errors.Is(e, api.ErrPermissionDenied) {
		t.Fatalf("excess authority: %v", e)
	}
	reg, e := v.registry.Registration(context.Background(), collectorID)
	if e != nil || reg.State != collector.Provisioned {
		t.Fatalf("registration changed: %+v %v", reg, e)
	}
	// Had the refused attempt recorded anything under key-1, a different
	// request under the same key would now conflict.
	enrollment, e := v.registry.Enroll(ctx, []byte("key-1"), enrollRequest(collectorID, "authority:logs"))
	if e != nil {
		t.Fatal(e)
	}
	if enrollment.Generation != 1 || !reflect.DeepEqual(enrollment.Request.RequestedAuthorityPolicyIDs, []shoal.ID{"authority:logs"}) {
		t.Fatalf("enrollment %+v", enrollment)
	}
}

func TestUnboundPrincipalIsRefused(t *testing.T) {
	v := newEnv(t)
	v.provision(t, collectorID, "tail", "authority:logs")
	if _, e := v.registry.Enroll(v.as(t, "tail"), []byte("k"), enrollRequest(collectorID, "authority:logs")); e != nil {
		t.Fatal(e)
	}
	intruder := v.as(t, "intruder")
	if _, e := v.registry.Enroll(intruder, []byte("k2"), enrollRequest(collectorID, "authority:logs")); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		t.Fatalf("unbound enroll: %v", e)
	}
	if _, e := v.registry.SubmitArtifact(intruder, collectorID, artifact(t, v, "a", "x")); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		t.Fatalf("unbound artifact: %v", e)
	}
	readOnly := v.as(t, "tail", auth.OperationRead)
	if _, e := v.registry.SubmitArtifact(readOnly, collectorID, artifact(t, v, "a", "x")); !shoal.IsErrorCode(e, shoal.ErrorUnauthorized) {
		t.Fatalf("read-only principal submitted: %v", e)
	}
	if _, e := v.registry.Enroll(context.Background(), []byte("k3"), enrollRequest(collectorID, "authority:logs")); !shoal.IsErrorCode(e, shoal.ErrorUnauthorized) {
		t.Fatalf("unauthenticated enroll: %v", e)
	}
}

func TestExtractorVersionsShareLineageAndRevocationQuarantines(t *testing.T) {
	v := newEnv(t)
	v.provision(t, collectorID, "tail", "authority:logs")
	ctx := v.as(t, "tail")
	if _, e := v.registry.Enroll(ctx, []byte("k"), enrollRequest(collectorID, "authority:logs")); e != nil {
		t.Fatal(e)
	}
	ref := artifact(t, v, "artifact:1", "line one\n")
	if _, e := v.registry.SubmitArtifact(ctx, collectorID, ref); e != nil {
		t.Fatal(e)
	}
	first, e := v.registry.SubmitObservation(ctx, observation(t, v, "artifact:1", v1))
	if e != nil {
		t.Fatal(e)
	}
	before, e := v.registry.ReadObservation(ctx, first.Observation.ID())
	if e != nil {
		t.Fatal(e)
	}
	v.clock.Advance(time.Minute)
	second, e := v.registry.SubmitObservation(ctx, observation(t, v, "artifact:1", v2))
	if e != nil {
		t.Fatal(e)
	}
	if first.Observation.ID() == second.Observation.ID() {
		t.Fatal("extractor versions share an observation ID")
	}
	if first.Observation.Config().ArtifactID != second.Observation.Config().ArtifactID || first.ArtifactDigest != second.ArtifactDigest || first.ArtifactDigest != ref.Digest {
		t.Fatal("extractor versions do not share artifact lineage")
	}
	after, e := v.registry.ReadObservation(ctx, first.Observation.ID())
	if e != nil || !reflect.DeepEqual(before, after) || after.Quarantined {
		t.Fatalf("re-extraction changed v1: %+v %v", after, e)
	}

	reg, e := v.registry.Revoke(context.Background(), collectorID)
	if e != nil || reg.Generation != 2 || reg.State != collector.Revoked {
		t.Fatalf("revoke: %+v %v", reg, e)
	}
	for _, id := range []shoal.ID{first.Observation.ID(), second.Observation.ID()} {
		got, e := v.registry.ReadObservation(ctx, id)
		if e != nil || !got.Quarantined {
			t.Fatalf("observation not quarantined after revoke: %+v %v", got, e)
		}
	}
	if _, e := v.registry.SubmitArtifact(ctx, collectorID, artifact(t, v, "artifact:2", "two")); !errors.Is(e, api.ErrPermissionDenied) {
		t.Fatalf("revoked collector submitted artifact: %v", e)
	}
	v.clock.Advance(time.Minute)
	if _, e := v.registry.SubmitObservation(ctx, observation(t, v, "artifact:1", v1)); !errors.Is(e, api.ErrPermissionDenied) {
		t.Fatalf("revoked collector submitted observation: %v", e)
	}
	if _, e := v.registry.Enroll(ctx, []byte("k-new"), enrollRequest(collectorID, "authority:logs")); !errors.Is(e, api.ErrPermissionDenied) {
		t.Fatalf("revoked collector enrolled: %v", e)
	}

	// Re-provisioning starts generation 2; generation 1 stays quarantined and
	// its artifacts cannot carry new observations.
	v.provision(t, collectorID, "tail", "authority:logs")
	if _, e := v.registry.Enroll(ctx, []byte("k"), enrollRequest(collectorID, "authority:logs")); !shoal.IsErrorCode(e, shoal.ErrorConflict) {
		t.Fatalf("enroll key replayed across generations: %v", e)
	}
	if _, e := v.registry.Enroll(ctx, []byte("k-gen2"), enrollRequest(collectorID, "authority:logs")); e != nil {
		t.Fatal(e)
	}
	got, e := v.registry.ReadObservation(ctx, first.Observation.ID())
	if e != nil || !got.Quarantined {
		t.Fatalf("re-provisioning released quarantine: %+v %v", got, e)
	}
	// The generation-1 artifact is invisible to generation 2.
	if _, e := v.registry.SubmitObservation(ctx, observation(t, v, "artifact:1", v1)); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		t.Fatalf("observation over a revoked generation's artifact: %v", e)
	}
	if _, e := v.registry.SubmitArtifact(ctx, collectorID, ref); e != nil {
		t.Fatalf("identical artifact resubmission: %v", e)
	}
}

func TestIdempotentRetries(t *testing.T) {
	v := newEnv(t)
	v.provision(t, collectorID, "tail", "authority:logs", "authority:metrics")
	ctx := v.as(t, "tail")
	first, e := v.registry.Enroll(ctx, []byte("k"), enrollRequest(collectorID, "authority:logs"))
	if e != nil {
		t.Fatal(e)
	}
	v.clock.Advance(time.Minute)
	again, e := v.registry.Enroll(ctx, []byte("k"), enrollRequest(collectorID, "authority:logs"))
	if e != nil || !reflect.DeepEqual(first, again) {
		t.Fatalf("same-key retry: %+v %v", again, e)
	}
	if _, e = v.registry.Enroll(ctx, []byte("k"), enrollRequest(collectorID, "authority:metrics")); !shoal.IsErrorCode(e, shoal.ErrorConflict) {
		t.Fatalf("conflicting retry: %v", e)
	}
	// A new key re-enrolls within the ceiling; the old key then conflicts
	// rather than rolling the enrollment back.
	second, e := v.registry.Enroll(ctx, []byte("k2"), enrollRequest(collectorID, "authority:logs", "authority:metrics"))
	if e != nil || second.ID == first.ID {
		t.Fatalf("re-enroll: %+v %v", second, e)
	}
	if again, e = v.registry.Enroll(ctx, []byte("k"), enrollRequest(collectorID, "authority:logs")); e != nil || again.ID != first.ID {
		t.Fatalf("superseded receipt: %+v %v", again, e)
	}
	if row, _, _ := v.registry.readRegistration(ctx, collectorID); row.Enrollment.ID != second.ID {
		t.Fatal("old key rolled the enrollment back")
	}

	ref := artifact(t, v, "artifact:1", "body")
	a1, e := v.registry.SubmitArtifact(ctx, collectorID, ref)
	if e != nil {
		t.Fatal(e)
	}
	v.clock.Advance(time.Minute)
	a2, e := v.registry.SubmitArtifact(ctx, collectorID, ref)
	if e != nil || !reflect.DeepEqual(a1, a2) {
		t.Fatalf("artifact retry: %+v %v", a2, e)
	}
	changed := ref
	changed.Size++
	if _, e = v.registry.SubmitArtifact(ctx, collectorID, changed); !shoal.IsErrorCode(e, shoal.ErrorConflict) {
		t.Fatalf("conflicting artifact: %v", e)
	}
	o := observation(t, v, "artifact:1", v1)
	o1, e := v.registry.SubmitObservation(ctx, o)
	if e != nil {
		t.Fatal(e)
	}
	v.clock.Advance(time.Minute)
	o2, e := v.registry.SubmitObservation(ctx, o)
	if e != nil || !reflect.DeepEqual(o1, o2) {
		t.Fatalf("observation retry: %+v %v", o2, e)
	}
}

func TestSubmitRequiresOwnArtifactAndDeclaredExtractor(t *testing.T) {
	v := newEnv(t)
	v.provision(t, collectorID, "tail", "authority:logs")
	v.provision(t, "collector:other", "other", "authority:logs")
	ctx := v.as(t, "tail")
	other := v.as(t, "other")
	if _, e := v.registry.Enroll(ctx, []byte("k"), collector.EnrollRequest{CollectorID: collectorID, RequestedAuthorityPolicyIDs: []shoal.ID{"authority:logs"}, Extractors: []collector.ExtractorRef{v1}}); e != nil {
		t.Fatal(e)
	}
	if _, e := v.registry.Enroll(other, []byte("k"), enrollRequest("collector:other", "authority:logs")); e != nil {
		t.Fatal(e)
	}
	if _, e := v.registry.SubmitArtifact(other, "collector:other", artifact(t, v, "artifact:shared", "x")); e != nil {
		t.Fatal(e)
	}
	// The same artifact ID recorded by another collector is not this one's.
	if _, e := v.registry.SubmitObservation(ctx, observation(t, v, "artifact:shared", v1)); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		t.Fatalf("observation over another collector's artifact: %v", e)
	}
	if _, e := v.registry.SubmitArtifact(ctx, collectorID, artifact(t, v, "artifact:own", "y")); e != nil {
		t.Fatal(e)
	}
	if _, e := v.registry.SubmitObservation(ctx, observation(t, v, "artifact:own", v2)); !errors.Is(e, api.ErrPermissionDenied) {
		t.Fatalf("undeclared extractor accepted: %v", e)
	}
	// One collector cannot submit as another even with a valid observation.
	if _, e := v.registry.SubmitObservation(other, observation(t, v, "artifact:own", v1)); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		t.Fatalf("cross-collector submission: %v", e)
	}
	future := artifact(t, v, "artifact:future", "z")
	future.ObservedAt = v.clock.Now().Add(time.Hour)
	if _, e := v.registry.SubmitArtifact(ctx, collectorID, future); !shoal.IsErrorCode(e, shoal.ErrorInvalidArgument) {
		t.Fatalf("future artifact: %v", e)
	}
}

func TestProvisioningIsTheOnlyAuthority(t *testing.T) {
	v := newEnv(t)
	v.provision(t, collectorID, "tail", "authority:logs")
	if _, e := v.registry.Provision(context.Background(), Provisioning{CollectorID: collectorID, Subject: "tail", ClientID: "client", Domain: []byte("domain"), AuthorityPolicyIDs: []shoal.ID{"authority:logs", "authority:admin"}, Control: collector.ExternalControlled, Mode: collector.ServerObserved}); !shoal.IsErrorCode(e, shoal.ErrorConflict) {
		t.Fatalf("active registration re-provisioned with more authority: %v", e)
	}
	v.provision(t, collectorID, "tail", "authority:logs") // identical: no-op
	if _, e := v.registry.Revoke(context.Background(), "collector:missing"); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		t.Fatalf("revoke missing: %v", e)
	}
}

func signedAttestation(t *testing.T, v *env, key []byte, expires time.Time) *collector.AttestationReport {
	t.Helper()
	evidence, e := collectorattest.SignStatement(v.signer, collectorattest.Statement{CollectorID: base64.RawURLEncoding.EncodeToString([]byte(collectorID)), ImageDigest: strings.Repeat("e", 64), IssuedAt: v.clock.Now().Add(-time.Minute), ExpiresAt: expires, Nonce: collector.EnrollNonce(collectorID, key)})
	if e != nil {
		t.Fatal(e)
	}
	return &collector.AttestationReport{Kind: collectorattest.Ed25519StatementKind, Format: collectorattest.Ed25519StatementFormat, Evidence: evidence}
}

func TestEnrollAttestation(t *testing.T) {
	v := newEnv(t)
	v.provision(t, collectorID, "tail", "authority:logs")
	ctx := v.as(t, "tail")
	stale := enrollRequest(collectorID, "authority:logs")
	stale.Attestation = signedAttestation(t, v, []byte("k1"), v.clock.Now().Add(-time.Second))
	if _, e := v.registry.Enroll(ctx, []byte("k1"), stale); !errors.Is(e, api.ErrPermissionDenied) {
		t.Fatalf("stale attestation: %v", e)
	}
	replayed := enrollRequest(collectorID, "authority:logs")
	replayed.Attestation = signedAttestation(t, v, []byte("k1"), v.clock.Now().Add(time.Minute))
	if _, e := v.registry.Enroll(ctx, []byte("k2"), replayed); !errors.Is(e, api.ErrPermissionDenied) {
		t.Fatalf("statement replayed under another key: %v", e)
	}
	if reg, _ := v.registry.Registration(ctx, collectorID); reg.State != collector.Provisioned {
		t.Fatal("refused attestation changed registration")
	}
	verified, e := v.registry.Enroll(ctx, []byte("k1"), replayed)
	if e != nil || verified.Attestation == nil || verified.Attestation.Status != collector.AttestationVerified {
		t.Fatalf("verified enrollment: %+v %v", verified, e)
	}
	claimed := enrollRequest(collectorID, "authority:logs")
	claimed.Attestation = &collector.AttestationReport{Kind: "nitro", Format: "cbor", Evidence: []byte("opaque")}
	claim, e := v.registry.Enroll(ctx, []byte("k3"), claimed)
	if e != nil || claim.Attestation.Status != collector.AttestationClaim || claim.Attestation.ID() != "" {
		t.Fatalf("claim enrollment: %+v %v", claim, e)
	}
}

func TestSourceAdapterMapsServerOwnedFactsByValue(t *testing.T) {
	v := newEnv(t)
	v.provision(t, collectorID, "tail", "authority:logs", "authority:metrics")
	ctx := v.as(t, "tail")
	request := enrollRequest(collectorID, "authority:logs")
	request.Attestation = signedAttestation(t, v, []byte("k"), v.clock.Now().Add(time.Minute))
	enrollment, e := v.registry.Enroll(ctx, []byte("k"), request)
	if e != nil {
		t.Fatal(e)
	}
	ref := artifact(t, v, "artifact:1", "body")
	if _, e = v.registry.SubmitArtifact(ctx, collectorID, ref); e != nil {
		t.Fatal(e)
	}
	o := observation(t, v, "artifact:1", v1)
	if _, e = v.registry.SubmitObservation(ctx, o); e != nil {
		t.Fatal(e)
	}
	record, e := v.registry.ReadObservation(ctx, o.ID())
	if e != nil {
		t.Fatal(e)
	}
	reg, _ := v.registry.Registration(ctx, collectorID)
	source, e := Source(reg, enrollment, record, "authority:logs")
	if e != nil {
		t.Fatal(e)
	}
	want := decision.Source{ArtifactID: "artifact:1", ID: o.ID(), RevisionID: shoal.ID("sha256:" + ref.Digest), Digest: ref.Digest, OriginID: collectorID, AuthorityPolicyID: "authority:logs", AttestationID: enrollment.Attestation.ID(), Role: decision.Observation, Control: decision.ExternalControlled, ObservedAt: o.Config().ObservedAt, ReceivedAt: record.ReceivedAt}
	if !reflect.DeepEqual(source, want) || source.AttestationID == "" {
		t.Fatalf("source\n got %+v\nwant %+v", source, want)
	}
	if string(collector.ExternalControlled) != string(decision.ExternalControlled) || string(collector.CandidateControlled) != string(decision.CandidateControlled) || string(collector.RegistryControlled) != string(decision.RegistryControlled) || string(collector.UnknownControl) != string(decision.UnknownControl) {
		t.Fatal("collector control no longer mirrors decision control by value")
	}
	// Provisioned but not granted by this enrollment.
	if _, e = Source(reg, enrollment, record, "authority:metrics"); e == nil {
		t.Fatal("ungranted authority mapped")
	}
	quarantined := record
	quarantined.Quarantined = true
	if _, e = Source(reg, enrollment, quarantined, "authority:logs"); e == nil {
		t.Fatal("quarantined observation mapped")
	}
	// A claim never becomes an attestation ID.
	claimed := enrollment
	claim := collector.ClaimFor(collectorID, collector.AttestationReport{Kind: "nitro", Format: "cbor", Evidence: []byte("x")})
	claimed.Attestation = &claim
	if s, e := Source(reg, claimed, record, "authority:logs"); e != nil || s.AttestationID != "" {
		t.Fatalf("claim mapped to attestation: %+v %v", s, e)
	}
}

func TestCorruptRowsFailClosed(t *testing.T) {
	v := newEnv(t)
	v.provision(t, collectorID, "tail", "authority:logs")
	cell, e := v.registry.read(context.Background(), v.registry.registrationCoordinate(collectorID))
	if e != nil {
		t.Fatal(e)
	}
	var row registrationRow
	if e = decode(cell.Value, &row); e != nil {
		t.Fatal(e)
	}
	tampered := []byte(strings.Replace(string(cell.Value), `"Schema":1`, `"Schema":1 `, 1))
	if decode(tampered, &row) == nil {
		t.Fatal("non-canonical row accepted")
	}
}

// Finding 1: a revoked collector cannot be re-provisioned into another
// domain or principal, rows keep their own domain, and a new generation never
// sees an earlier generation's artifact.
func TestIdentityIsFixedAndRowsKeepTheirDomain(t *testing.T) {
	v := newEnv(t)
	v.provision(t, collectorID, "tail", "authority:logs")
	ctx := v.as(t, "tail")
	if _, e := v.registry.Enroll(ctx, []byte("k"), enrollRequest(collectorID, "authority:logs")); e != nil {
		t.Fatal(e)
	}
	ref := artifact(t, v, "artifact:1", "secret")
	if _, e := v.registry.SubmitArtifact(ctx, collectorID, ref); e != nil {
		t.Fatal(e)
	}
	o := observation(t, v, "artifact:1", v1)
	if _, e := v.registry.SubmitObservation(ctx, o); e != nil {
		t.Fatal(e)
	}
	if _, e := v.registry.Revoke(context.Background(), collectorID); e != nil {
		t.Fatal(e)
	}
	for name, p := range map[string]Provisioning{
		"domain":  {CollectorID: collectorID, Subject: "tail", ClientID: "client", Domain: []byte("other"), AuthorityPolicyIDs: []shoal.ID{"authority:logs"}, Control: collector.ExternalControlled, Mode: collector.ServerObserved},
		"subject": {CollectorID: collectorID, Subject: "mallory", ClientID: "client", Domain: []byte("domain"), AuthorityPolicyIDs: []shoal.ID{"authority:logs"}, Control: collector.ExternalControlled, Mode: collector.ServerObserved},
		"client":  {CollectorID: collectorID, Subject: "tail", ClientID: "other", Domain: []byte("domain"), AuthorityPolicyIDs: []shoal.ID{"authority:logs"}, Control: collector.ExternalControlled, Mode: collector.ServerObserved},
	} {
		if _, e := v.registry.Provision(context.Background(), p); !shoal.IsErrorCode(e, shoal.ErrorConflict) {
			t.Fatalf("revoked collector re-provisioned with new %s: %v", name, e)
		}
	}
	if reg, _ := v.registry.Registration(context.Background(), collectorID); !bytes.Equal(reg.Domain, []byte("domain")) || reg.Subject != "tail" {
		t.Fatalf("identity changed: %+v", reg)
	}
	// A reader in another domain cannot see the row, whatever the registry
	// says now; the original domain still can (quarantined).
	other, e := auth.NewDecision(auth.DecisionConfig{Subject: "reader", Actor: "reader", ClientID: "client", AuthorizationDomain: []byte("other"), AllowedOperations: []auth.Operation{auth.OperationRead}, PolicyGeneration: 1, AuthenticationExpires: v.clock.Now().Add(time.Hour), RequestID: "r"})
	if e != nil {
		t.Fatal(e)
	}
	otherCtx, _ := v.authority.Binder().Bind(context.Background(), other)
	if _, e := v.registry.ReadObservation(otherCtx, o.ID()); !shoal.IsErrorCode(e, shoal.ErrorNotFound) {
		t.Fatalf("cross-domain read: %v", e)
	}
	if got, e := v.registry.ReadObservation(ctx, o.ID()); e != nil || !got.Quarantined {
		t.Fatalf("original domain read: %+v %v", got, e)
	}

	// Re-provision with the same identity (and new terms). The old artifact
	// ID is a fresh, distinct record in generation 2, never the old one.
	if _, e := v.registry.Provision(context.Background(), Provisioning{CollectorID: collectorID, Subject: "tail", ClientID: "client", Domain: []byte("domain"), AuthorityPolicyIDs: []shoal.ID{"authority:logs", "authority:metrics"}, Control: collector.ExternalControlled, Mode: collector.ServerObserved}); e != nil {
		t.Fatal(e)
	}
	if _, e := v.registry.Enroll(ctx, []byte("k2"), enrollRequest(collectorID, "authority:logs")); e != nil {
		t.Fatal(e)
	}
	v.clock.Advance(time.Minute)
	different := ref
	different.Size++
	got, e := v.registry.SubmitArtifact(ctx, collectorID, different)
	if e != nil || got.Generation != 2 || !reflect.DeepEqual(got.Ref, different) {
		t.Fatalf("generation 2 artifact under an old ID: %+v %v", got, e)
	}
	// Within generation 2 the ID is now taken by the new reference.
	if _, e := v.registry.SubmitArtifact(ctx, collectorID, ref); !shoal.IsErrorCode(e, shoal.ErrorConflict) {
		t.Fatalf("conflicting ref in generation 2: %v", e)
	}
}

func TestResubmittingOldGenerationObservationStaysQuarantined(t *testing.T) {
	v := newEnv(t)
	v.provision(t, collectorID, "tail", "authority:logs")
	ctx := v.as(t, "tail")
	_, _ = v.registry.Enroll(ctx, []byte("k"), enrollRequest(collectorID, "authority:logs"))
	ref := artifact(t, v, "artifact:1", "x")
	_, _ = v.registry.SubmitArtifact(ctx, collectorID, ref)
	o := observation(t, v, "artifact:1", v1)
	if _, e := v.registry.SubmitObservation(ctx, o); e != nil {
		t.Fatal(e)
	}
	_, _ = v.registry.Revoke(context.Background(), collectorID)
	v.provision(t, collectorID, "tail", "authority:logs")
	_, _ = v.registry.Enroll(ctx, []byte("k2"), enrollRequest(collectorID, "authority:logs"))
	if _, e := v.registry.SubmitArtifact(ctx, collectorID, ref); e != nil {
		t.Fatal(e)
	}
	if _, e := v.registry.SubmitObservation(ctx, o); !errors.Is(e, api.ErrPermissionDenied) {
		t.Fatalf("old-generation observation resubmitted: %v", e)
	}
	if got, e := v.registry.ReadObservation(ctx, o.ID()); e != nil || !got.Quarantined {
		t.Fatalf("old observation released: %+v %v", got, e)
	}
}

// Finding 2: an attestation that expired before an observation does not
// lend it an AttestationID, though the enrollment is still current.
func TestSourceOmitsLapsedAttestation(t *testing.T) {
	v := newEnv(t)
	v.provision(t, collectorID, "tail", "authority:logs")
	ctx := v.as(t, "tail")
	request := enrollRequest(collectorID, "authority:logs")
	request.Attestation = signedAttestation(t, v, []byte("k"), v.clock.Now().Add(time.Minute))
	enrollment, e := v.registry.Enroll(ctx, []byte("k"), request)
	if e != nil {
		t.Fatal(e)
	}
	ref := artifact(t, v, "artifact:1", "body")
	_, _ = v.registry.SubmitArtifact(ctx, collectorID, ref)
	early := observation(t, v, "artifact:1", v1)
	if _, e = v.registry.SubmitObservation(ctx, early); e != nil {
		t.Fatal(e)
	}
	v.clock.Advance(2 * time.Minute)
	late := observation(t, v, "artifact:1", v2)
	if _, e = v.registry.SubmitObservation(ctx, late); e != nil {
		t.Fatal(e)
	}
	// A same-key retry after expiry returns the original receipt.
	if again, e := v.registry.Enroll(ctx, []byte("k"), request); e != nil || again.ID != enrollment.ID {
		t.Fatalf("retry after expiry: %v", e)
	}
	reg, _ := v.registry.Registration(ctx, collectorID)
	for _, tc := range []struct {
		o      collector.Observation
		lapsed bool
	}{{early, false}, {late, true}} {
		record, e := v.registry.ReadObservation(ctx, tc.o.ID())
		if e != nil {
			t.Fatal(e)
		}
		source, e := Source(reg, enrollment, record, "authority:logs")
		if e != nil {
			t.Fatal(e)
		}
		if AttestationLapsed(enrollment, record) != tc.lapsed || (source.AttestationID == "") != tc.lapsed {
			t.Fatalf("lapsed=%v attestation=%q", tc.lapsed, source.AttestationID)
		}
	}
}
