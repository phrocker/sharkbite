// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisioninventorystore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	outcomes "github.com/phrocker/shoal-oss/internal/decisionoutcomes"
	"github.com/phrocker/shoal-oss/internal/engine"
	"github.com/phrocker/shoal-oss/internal/explorercoord"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func retainedFixture(t *testing.T) (Binding, Intent, outcomes.Receipt, time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 8, 18, 0, 0, 0, time.UTC)
	target, e := decision.AdjudicationTargetID("task", "picture", "subject", "question")
	if e != nil {
		t.Fatal(e)
	}
	b := Binding{"coverage", target, "task", "picture", "subject", "question"}
	i := Intent{ReceiptID: shoal.ID("outcome-receipt:" + strings.Repeat("a", 64)), ObservationID: "observation", RequestID: "request", PredictionID: "prediction", Reporter: Attribution{SubjectID: shoal.ID(string([]byte{0, 255})), ActorID: "actor", ClientID: "client", OnBehalfOf: []shoal.ID{"delegate"}}}
	r := outcomes.Receipt{ID: i.ReceiptID, ObservationID: i.ObservationID, ObservationConfig: decision.OutcomeObservationConfig{RequestID: i.RequestID, PredictionID: i.PredictionID, SubjectID: b.SubjectID, Kind: decision.OutcomeCorrectness, QuestionID: b.QuestionID, Label: "positive", EvidenceIDs: []shoal.ID{"evidence"}, ObservedAt: now.Add(-time.Minute)}, SubmitterID: i.Reporter.SubjectID, ActorID: i.Reporter.ActorID, ClientID: i.Reporter.ClientID, OnBehalfOf: i.Reporter.OnBehalfOf, AuthorizationFingerprint: "auth-sha256:" + strings.Repeat("b", 64), ReceivedAt: now.Add(-time.Second), State: "proposed"}
	return b, i, r, now
}
func TestPhysicalRFilePendingAndPublicationRestart(t *testing.T) {
	b, i, r, now := retainedFixture(t)
	dir := filepath.Join(t.TempDir(), "engine")
	scope := Scope{Domain: []byte{0, 255, 1}}
	open := func() (*engine.Engine, *Store) {
		eng, e := engine.Open(dir, engine.Options{})
		if e != nil {
			t.Fatal(e)
		}
		found := false
		for _, table := range eng.TableNames() {
			found = found || table == Table
		}
		if !found {
			if e = eng.CreateTable(Table, engine.TableOptions{}); e != nil {
				t.Fatal(e)
			}
		}
		backend, e := explorercoord.NewEngineStore(eng, Table)
		if e != nil {
			t.Fatal(e)
		}
		s, e := New(Config{Backend: backend, Clock: func() time.Time { return now }})
		if e != nil {
			t.Fatal(e)
		}
		return eng, s
	}
	ctx := context.Background()
	eng, s := open()
	if _, e := s.Register(ctx, scope, b); e != nil {
		t.Fatal(e)
	}
	pending, e := s.Begin(ctx, scope, b, i)
	if e != nil {
		t.Fatal(e)
	}
	if e = eng.Flush(Table); e != nil {
		t.Fatal(e)
	}
	if e = eng.Close(); e != nil {
		t.Fatal(e)
	}
	eng, s = open()
	got, e := s.Load(ctx, scope, b)
	if e != nil || !reflect.DeepEqual(got, pending) || got.Complete() {
		t.Fatal("pending lost after physical reopen", e)
	}
	now = now.Add(time.Minute)
	published, e := s.Publish(ctx, scope, b, i, r)
	if e != nil {
		t.Fatal(e)
	}
	if e = eng.Flush(Table); e != nil {
		t.Fatal(e)
	}
	if e = eng.Close(); e != nil {
		t.Fatal(e)
	}
	eng, s = open()
	defer eng.Close()
	got, e = s.Load(ctx, scope, b)
	if e != nil || !reflect.DeepEqual(got, published) || !got.Complete() {
		t.Fatal("published identity changed after reopen", e)
	}
	same, e := s.Publish(ctx, scope, b, i, r)
	if e != nil || same.ID != published.ID || same.Version != 3 {
		t.Fatal("publication retry changed state", e)
	}
	if _, e = s.Load(ctx, Scope{Domain: []byte("other")}, b); !errors.Is(e, ErrNotFound) {
		t.Fatal("scope alias", e)
	}
}
func TestCodecRejectsInvalidAndAmplifiedRecords(t *testing.T) {
	b, i, _, now := retainedFixture(t)
	scope := "scope"
	s := Snapshot{Binding: b, Version: 2, CreatedAt: now, UpdatedAt: now, Entries: []Entry{{Intent: i, State: Pending, OpenedAt: now}}}
	raw, e := encode(scope, s)
	if e != nil {
		t.Fatal(e)
	}
	good, e := decode(raw, scope)
	if e != nil {
		t.Fatal(e)
	}
	for _, change := range []func(*Snapshot){func(s *Snapshot) { s.Version++ }, func(s *Snapshot) { s.Entries[0].ReceiptDigest = strings.Repeat("a", 64) }, func(s *Snapshot) { s.Entries[0].OpenedAt = now.Add(time.Hour) }, func(s *Snapshot) { s.Entries[0].State = "aborted" }, func(s *Snapshot) { s.Entries = append(s.Entries, s.Entries[0]) }, func(s *Snapshot) { s.Binding.TargetID = "wrong-target" }} {
		copy := good
		copy.Entries = append([]Entry(nil), good.Entries...)
		change(&copy)
		if _, e = encode(scope, copy); e == nil {
			t.Fatal("invalid snapshot encoded")
		}
	}
	if _, e = decode(raw, "different-scope"); !errors.Is(e, ErrCorrupt) {
		t.Fatal("scope rebound")
	}
	for _, bad := range [][]byte{append(append([]byte{}, raw...), byte('x')), []byte(`{"Schema":1,"Schema":1}`), []byte(`{"Entries":[` + strings.Repeat(`{},`, MaxEntries) + `{}]}`), []byte(`{"OnBehalfOf":[` + strings.Repeat(`"YQ==",`, 64) + `"YQ=="]}`)} {
		if _, e = decode(bad, scope); !errors.Is(e, ErrCorrupt) {
			t.Fatal("malformed input accepted")
		}
	}
	var env envelope
	if e = json.Unmarshal(raw, &env); e != nil {
		t.Fatal(e)
	}
	var p payload
	if e = json.Unmarshal(env.Payload, &p); e != nil {
		t.Fatal(e)
	}
	p.Snapshot.Entries[0].Intent.Reporter.SubjectID = []byte("changed")
	env.Payload = jsonBytes(p)
	env.Checksum = hash(env.Payload)
	if _, e = decode(jsonBytes(env), scope); !errors.Is(e, ErrCorrupt) {
		t.Fatal("checksum replacement rebound content identity")
	}
}
func TestEntryLimitAndClockRollback(t *testing.T) {
	b, i, _, now := retainedFixture(t)
	backend := &adversarialCAS{}
	s, e := New(Config{Backend: backend, Clock: func() time.Time { return now }})
	if e != nil {
		t.Fatal(e)
	}
	scope := Scope{Domain: []byte("scope")}
	ctx := context.Background()
	if _, e = s.Register(ctx, scope, b); e != nil {
		t.Fatal(e)
	}
	for n := 0; n < MaxEntries; n++ {
		i.ReceiptID = shoal.ID(fmt.Sprintf("outcome-receipt:%064x", n))
		if _, e = s.Begin(ctx, scope, b, i); e != nil {
			t.Fatalf("entry %d: %v", n, e)
		}
	}
	i.ReceiptID = shoal.ID(fmt.Sprintf("outcome-receipt:%064x", MaxEntries))
	if _, e = s.Begin(ctx, scope, b, i); !errors.Is(e, ErrLimit) {
		t.Fatal("entry bound not enforced", e)
	}
	b.CoverageID = "different"
	if _, e = s.Register(ctx, scope, b); !errors.Is(e, ErrConflict) {
		t.Fatal("coverage changed", e)
	}
	b.CoverageID = "coverage"
	b.PictureID = "another-picture"
	b.TargetID, _ = decision.AdjudicationTargetID(b.TaskID, b.PictureID, b.SubjectID, b.QuestionID)
	if _, e = s.Register(ctx, scope, b); e != nil {
		t.Fatal(e)
	}
	now = now.Add(-time.Second)
	if _, e = s.Begin(ctx, scope, b, i); !errors.Is(e, ErrUnavailable) {
		t.Fatal("clock rollback admitted", e)
	}
}
