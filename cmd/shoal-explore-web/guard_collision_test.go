// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleetevents"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func collisionDigest(tag string, parts ...[]byte) []byte {
	hash := sha256.New()
	field := func(value []byte) {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write(value)
	}
	field([]byte(tag))
	for _, part := range parts {
		field(part)
	}
	return hash.Sum(nil)
}

// collidingPublication is a public event publication whose guard entity,
// {'P', hex(eventID)}, is the guard entity of an approval whose ID is that hex
// string. Every input is the caller's own: its subject and fingerprint, a
// token and a retry deadline it chose.
func collidingPublication(
	t *testing.T, h *approvalHarness, who principal, token string,
) (fleetevents.PublishRequest, string) {
	t.Helper()
	decision, err := h.authority.Resolver().Resolve(h.as(who))
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := auth.AuthorizationFingerprint(decision)
	if err != nil {
		t.Fatal(err)
	}
	retryUntil := h.now().Add(time.Hour).UTC()
	scoped := collisionDigest("fleet-event-publication-token-v2",
		[]byte(decision.Subject()), fingerprint.Bytes(), []byte(token))
	publicationID := collisionDigest("fleet-event-publication-id-v2", scoped)
	eventID := collisionDigest("fleet-event-id-v2", publicationID,
		[]byte(retryUntil.Format(time.RFC3339Nano)))
	return fleetevents.PublishRequest{
		Token: []byte(token), RetryUntil: retryUntil,
		Event: fleetevents.Event{
			Kind: "custom.note", ProducerID: []byte("gateway"),
			ProducerGeneration: h.generationOf("gateway"),
			ActionID:           []byte("anything"), TransitionID: []byte("t"),
			Evidence: []fleetevents.Evidence{{
				SourceID: workspaceSourceID, PolicyID: workspaceGrantPolicyID,
				ObjectID: "release-7",
			}},
			OccurredAt: h.now(),
		},
	}, hex.EncodeToString(eventID)
}

// TestOneStoreCannotAdoptAnothersGuardHead: approvals and fleet-event
// publications both use guard kind 'P', and the guard head key has no store
// in it. A caller holding event_publish and dispatch can precompute an event
// ID and request an approval at its hex, or the other way round. Before the
// guard checked head ownership, the publication's ModeMutate adopted the
// approval's head. The approval was then unreadable for good ("not yet
// readable at its committed epoch") and could never be decided.
//
// Both orders are refused, and in both the first store's record is intact.
// The first, a publication adopting an approval's head, is what the
// head-ownership fix refuses; it passed before. The second was already
// refused, because an approval's first write creates absent-or-identical,
// which always compared the owner. It is kept so that stays true.
func TestOneStoreCannotAdoptAnothersGuardHead(t *testing.T) {
	h := newApprovalHarness(t)
	attacker := requester
	attacker.operations = append([]auth.Operation{
		auth.OperationEventPublish,
	}, requester.operations...)

	// Approval first, publication second.
	publication, id := collidingPublication(t, h, attacker, "collide-1")
	request := h.held(id)
	receipt := h.mustRequest(attacker, request, fleet.ApprovalPending)
	if _, err := h.opened.fleetEvents.Publish(
		h.as(attacker), publication); err == nil {
		t.Fatal("a publication adopted an approval's guard head")
	}
	if status := h.statusAs(approver, request.id); status.Approval.State !=
		fleet.ApprovalPending {
		t.Fatalf("the approval after the attempt is %q", status.Approval.State)
	}
	if _, err := h.decide(approver, receipt, fleet.ApprovalVerdictApprove); err != nil {
		t.Fatalf("the approval can no longer be decided: %v", err)
	}

	// Publication first, approval second.
	publication, id = collidingPublication(t, h, attacker, "collide-2")
	if _, err := h.opened.fleetEvents.Publish(
		h.as(attacker), publication); err != nil {
		t.Fatalf("the first publication: %v", err)
	}
	if _, err := h.request(attacker, h.held(id)); err == nil {
		t.Fatal("an approval adopted a publication's guard head")
	}
	// The publication is unharmed: an exact retry is still recognised as
	// the same publication rather than refused.
	repeated, err := h.opened.fleetEvents.Publish(h.as(attacker), publication)
	if err != nil || !repeated.Repeated {
		t.Fatalf("the publication's retry after the attempt = %+v, %v", repeated, err)
	}
	if _, err := h.opened.approvals.Status(h.as(attacker), fleet.ApprovalStatusRequest{
		ID: []byte(id), Context: h.context(h.now().Add(time.Minute)),
	}); !shoal.IsErrorCode(err, shoal.ErrorNotFound) &&
		!shoal.IsErrorCode(err, shoal.ErrorUnavailable) {
		t.Fatalf("status of the refused approval = %v", err)
	}
}

// ingestVictim ingests one source at uri through the authorized client, as
// a workspace ingester, and returns its document ID.
func ingestVictim(t *testing.T, h *approvalHarness, uri string) (shoal.ID, error) {
	t.Helper()
	if h.opened.client == nil {
		t.Fatal("the service exposes no authorized client")
	}
	ctx, err := h.authority.Binder().Bind(context.Background(), askDecision(
		t, h.now(), "victim-ingest",
		auth.OperationIngest, auth.OperationRead, auth.OperationList))
	if err != nil {
		t.Fatal(err)
	}
	result, err := h.opened.client.Ingest(ctx, explorer.Source{
		URI: uri, Title: "Victim", MediaType: "text/markdown",
		Content: "# Victim\n\nA document someone else wants.\n",
	})
	if err != nil {
		return "", err
	}
	return result.Document.ID, nil
}

// TestDispatchCannotOccupyADocumentsGuardEntity is the documents half of
// #669. A document is guarded at {'D', document ID}, and a dispatch action at
// {'D', action ID}, in one domain. The document ID is a digest of the source
// URI, so anyone can compute it before the URI is ingested. Before the fix, a
// principal holding dispatch could enqueue an action at that ID, and the
// later ingestion conflicted on a head that belonged to dispatch, for good.
//
// Both entry points that name a new action refuse the shape, and the
// ingestion then succeeds. They refuse it again after the ingestion, the same
// way, so the refusal says nothing about which documents exist.
func TestDispatchCannotOccupyADocumentsGuardEntity(t *testing.T) {
	// The ID an upload of this name will have, learned from another
	// workspace: it depends on the URI alone.
	predicted, err := ingestVictim(t, newApprovalHarness(t), "shoal://victim/release-notes.md")
	if err != nil {
		t.Fatal(err)
	}

	h := newApprovalHarness(t)
	occupy := func(when string) {
		t.Helper()
		plain := h.held(string(predicted))
		plain.action = "status"
		if _, err := h.opened.fleetDispatch.Enqueue(
			h.as(requester), h.enqueue(plain)); !shoal.IsErrorCode(
			err, shoal.ErrorInvalidArgument) {
			t.Fatalf("%s: an enqueue at a document's ID = %v, want refused", when, err)
		}
		if _, err := h.request(requester, h.held(string(predicted))); !shoal.IsErrorCode(
			err, shoal.ErrorInvalidArgument) {
			t.Fatalf("%s: an approval request at a document's ID = %v, want refused",
				when, err)
		}
	}
	occupy("before the ingestion")
	got, err := ingestVictim(t, h, "shoal://victim/release-notes.md")
	if err != nil {
		t.Fatalf("the ingestion after an attempted squat: %v", err)
	}
	if got != predicted {
		t.Fatalf("the document ID is %q, predicted %q", got, predicted)
	}
	occupy("after the ingestion")
}
