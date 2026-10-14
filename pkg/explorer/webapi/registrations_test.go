// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package webapi

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	decisionapi "github.com/phrocker/shoal-oss/pkg/decision/api"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type registrationHTTPStub struct {
	calls int
	reply decisionapi.RegistrationReceipt
	err   error
}

func (p *registrationHTTPStub) Register(_ context.Context, s decisionapi.RegistrationSelection, _ []byte) (decisionapi.RegistrationReceipt, error) {
	p.calls++
	p.reply.Snapshot.ProfileID = s.ProfileID
	p.reply.Snapshot.ProfileRevisionID = s.ProfileRevisionID
	return p.reply, p.err
}
func (p *registrationHTTPStub) Read(_ context.Context, id shoal.ID) (decisionapi.RegistrationReceipt, error) {
	p.calls++
	if p.reply.Snapshot.RequestID != id {
		return decisionapi.RegistrationReceipt{}, shoal.NewError(shoal.ErrorNotFound, "missing")
	}
	return p.reply, p.err
}

func registrationHTTPReply() decisionapi.RegistrationReceipt {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	id := shoal.ID("decision-registration:" + strings.Repeat("a", 64))
	o := shoal.ID("observation:" + strings.Repeat("b", 64))
	return decisionapi.RegistrationReceipt{ID: id, Scope: decisionapi.RegistrationScope{Domain: []byte("domain"), SubjectID: "subject"}, State: "ready", Version: 2, Snapshot: decisionapi.RegistrationSnapshot{SelectionSHA256: strings.Repeat("c", 64), ProfileID: "profile", ProfileRevisionID: "revision", BuilderID: "builder", RequestID: id, TaskID: "task", PictureID: "picture", PredictorID: "predictor", Sources: []decisionapi.RegistrationSourcePin{{CollectorID: "collector", ObservationID: o, ArtifactID: "artifact", EnrollmentID: "enrollment", AuthorityPolicyID: "policy", Mode: "imported", Generation: 1, ArtifactSHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", SourceSHA256: strings.Repeat("d", 64), ReceivedAt: now}}, AcceptedAt: now, AuthenticationExpiresAt: now.Add(time.Hour), AuthorizationFingerprint: "auth-sha256:" + strings.Repeat("e", 64), RecordSHA256: strings.Repeat("f", 64), RecordBytes: 1}, FrozenSHA256: strings.Repeat("1", 64), CreatedAt: now, UpdatedAt: now, ReadyAt: now}
}

func TestRegistrationPathRejectsAmbiguousURLs(t *testing.T) {
	id := shoal.ID("decision-registration:" + strings.Repeat("a", 64))
	encoded := decisionapi.EncodeID(id)
	for _, tc := range []struct {
		name string
		path string
	}{
		{"query", RegistrationsRoute + "/" + encoded + "?x=1"},
		{"double", RegistrationsRoute + "/" + encoded + "/extra"},
		{"empty", RegistrationsRoute + "/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if _, err := registrationPath(r); err == nil {
				t.Fatal("accepted ambiguous registration path")
			}
		})
	}
}

func TestRegistrationWireRejectsWriteAndReadConfusion(t *testing.T) {
	selection := decisionapi.RegistrationSelection{ProfileID: "profile", ProfileRevisionID: "revision", Sources: []decisionapi.RegistrationSourceInput{{ObservationID: shoal.ID("observation:" + strings.Repeat("a", 64)), Bytes: []byte("source")}}}
	body, err := decisionapi.EncodeRegistrationRequest(selection)
	if err != nil {
		t.Fatal(err)
	}
	get := httptest.NewRequest(http.MethodGet, RegistrationsRoute+"/"+decisionapi.EncodeID(shoal.ID("decision-registration:"+strings.Repeat("b", 64))), bytes.NewReader([]byte("body")))
	get.Header.Set("Idempotency-Key", decisionapi.EncodeKey([]byte("key")))
	if _, _, _, err := decodeRegistrationWire(get); err == nil {
		t.Fatal("accepted GET body/idempotency key")
	}
	post := httptest.NewRequest(http.MethodPost, RegistrationsRoute, bytes.NewReader(body))
	post.Header.Set("Content-Type", "application/json")
	if _, _, _, err := decodeRegistrationWire(post); err == nil {
		t.Fatal("accepted POST without idempotency key")
	}
	post.Header.Set("Idempotency-Key", decisionapi.EncodeKey([]byte("key")))
	post.Header.Set("Content-Encoding", "gzip")
	if _, _, _, err := decodeRegistrationWire(post); err == nil {
		t.Fatal("accepted compressed registration body")
	}
}

func TestRegistrationHTTPRoundTripAndBinding(t *testing.T) {
	h, _, authority, _ := decisionHTTPFixture(t)
	stub := &registrationHTTPStub{reply: registrationHTTPReply()}
	if err := h.MountRegistrations(stub, authority.Resolver()); err != nil {
		t.Fatal(err)
	}
	selection := decisionapi.RegistrationSelection{ProfileID: "profile", ProfileRevisionID: "revision", Sources: []decisionapi.RegistrationSourceInput{{ObservationID: stub.reply.Snapshot.Sources[0].ObservationID, Bytes: nil}}}
	body, err := decisionapi.EncodeRegistrationRequest(selection)
	if err != nil {
		t.Fatal(err)
	}
	post := httptest.NewRequest(http.MethodPost, "http://example.test"+RegistrationsRoute, bytes.NewReader(body))
	post.Header.Set("Content-Type", "application/json")
	post.Header.Set("Idempotency-Key", decisionapi.EncodeKey([]byte("key")))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, post)
	if w.Code != http.StatusOK || stub.calls != 1 {
		t.Fatalf("post %d %s calls=%d", w.Code, w.Body, stub.calls)
	}
	get := httptest.NewRequest(http.MethodGet, "http://example.test"+RegistrationsRoute+"/"+decisionapi.EncodeID(stub.reply.ID), nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, get)
	if w.Code != http.StatusOK || stub.calls != 2 {
		t.Fatalf("get %d %s calls=%d", w.Code, w.Body, stub.calls)
	}
}
