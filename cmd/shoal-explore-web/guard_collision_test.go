// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"testing"
	"time"

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
