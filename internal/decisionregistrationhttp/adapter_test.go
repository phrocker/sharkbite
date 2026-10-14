// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionregistrationhttp

import (
	"context"
	"errors"
	"github.com/phrocker/shoal-oss/internal/decisionregistration"
	registrations "github.com/phrocker/shoal-oss/internal/decisionregistrationstore"
	"github.com/phrocker/shoal-oss/pkg/decision/api"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"reflect"
	"strings"
	"testing"
	"time"
)

func fixture() registrations.Registration {
	now := time.Date(2026, 10, 8, 18, 0, 0, 0, time.UTC)
	id := shoal.ID("decision-registration:" + strings.Repeat("a", 64))
	return registrations.Registration{ID: id, Scope: registrations.Scope{Domain: []byte{0, 255}, SubjectID: "original-subject", ActorID: "", ClientID: shoal.ID(string([]byte{253, 0})), OnBehalfOf: []shoal.ID{shoal.ID(string([]byte{254, 0}))}}, State: registrations.Ready, Version: 2, Frozen: registrations.Frozen{SelectionSHA256: strings.Repeat("b", 64), ProfileID: "profile", ProfileRevisionID: "revision", BuilderID: "builder", RequestID: id, TaskID: "task", PictureID: "picture", PredictorID: "predictor", Sources: []registrations.SourcePin{{CollectorID: "collector", ObservationID: shoal.ID("observation:" + strings.Repeat("c", 64)), ArtifactID: "artifact", EnrollmentID: "enrollment", AuthorityPolicyID: "policy", Mode: "server_observed", Generation: 1, ArtifactSHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", SourceSHA256: strings.Repeat("d", 64), ReceivedAt: now}}, AcceptedAt: now, AuthenticationExpiresAt: now.Add(time.Hour), AuthorizationFingerprint: "auth-sha256:" + strings.Repeat("e", 64), RecordSHA256: strings.Repeat("f", 64), RecordBytes: 123}, FrozenSHA256: strings.Repeat("1", 64), CreatedAt: now, UpdatedAt: now.Add(time.Second), ReadyAt: now.Add(time.Second)}
}
func TestProjectionOriginalOpaqueScopeAndSelection(t *testing.T) {
	r := fixture()
	got, e := project(r)
	if e != nil {
		t.Fatal(e)
	}
	if got.Scope.SubjectID != r.Scope.SubjectID || got.Scope.ActorID != "" || got.Scope.ClientID != r.Scope.ClientID || !reflect.DeepEqual(got.Scope.OnBehalfOf, r.Scope.OnBehalfOf) || got.Snapshot.AuthorizationFingerprint != r.Frozen.AuthorizationFingerprint || got.Snapshot.AcceptedAt != r.Frozen.AcceptedAt {
		t.Fatal("attribution changed")
	}
	selection := api.RegistrationSelection{ProfileID: "profile", ProfileRevisionID: "revision", Sources: []api.RegistrationSourceInput{{ObservationID: r.Frozen.Sources[0].ObservationID, Bytes: []byte{}}}}
	if !api.MatchRegistrationSelection(selection, got) {
		t.Fatal("empty source mismatch")
	}
	selection.Sources[0].Bytes = []byte("other")
	if api.MatchRegistrationSelection(selection, got) {
		t.Fatal("different source matched")
	}
	got.Scope.Domain[0] = 9
	got.Scope.OnBehalfOf[0] = "different"
	got.Snapshot.Sources[0].CollectorID = "other"
	if r.Scope.Domain[0] == 9 || r.Scope.OnBehalfOf[0] == "different" || r.Frozen.Sources[0].CollectorID == "other" {
		t.Fatal("aliased projection")
	}
}
func TestProjectionRejectsPreparingAndMalformed(t *testing.T) {
	for _, mode := range []string{"preparing", "version", "hash"} {
		r := fixture()
		switch mode {
		case "preparing":
			r.State = registrations.Preparing
		case "version":
			r.Version = 1
		case "hash":
			r.Frozen.RecordSHA256 = "bad"
		}
		if _, e := project(r); !shoal.IsErrorCode(e, shoal.ErrorUnavailable) {
			t.Fatalf("%s %v", mode, e)
		}
	}
}
func TestAdapterNilAndUncertaintyMapping(t *testing.T) {
	if _, e := New(nil); e == nil {
		t.Fatal("nil accepted")
	}
	var a *Adapter
	if _, e := a.Read(context.Background(), "request"); errors.Is(e, api.ErrIndeterminate) || e == nil {
		t.Fatal("read uncertainty")
	}
	if _, e := a.Register(context.Background(), api.RegistrationSelection{}, nil); e == nil {
		t.Fatal("nil register")
	}
	cause := errors.Join(decisionregistration.ErrIndeterminate, errors.New("secret storage details"))
	mapped := registerError(cause)
	if !errors.Is(mapped, api.ErrIndeterminate) || strings.Contains(mapped.Error(), "secret") {
		t.Fatal("uncertainty lost or details leaked")
	}
	known := shoal.NewError(shoal.ErrorInvalidArgument, "invalid selection")
	if registerError(known) != known || errors.Is(registerError(known), api.ErrIndeterminate) {
		t.Fatal("prewrite error changed")
	}
}

func TestProjectionCollectorModePreserved(t *testing.T) {
	r := fixture()
	r.Frozen.Sources[0].Mode = "imported"
	got, e := project(r)
	if e != nil || got.Snapshot.Sources[0].Mode != "imported" {
		t.Fatalf("mode lost %v", e)
	}
	r.Frozen.Sources[0].Mode = ""
	if _, e = project(r); e == nil {
		t.Fatal("unclassified source projected")
	}
}
