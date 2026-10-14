// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleetevents

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/webapi"
)

// TestAnOutsiderOfOnlyLabelledEvidenceSeesTheWireOfNone: an action whose only
// evidence is labelled reads, for an outsider, exactly as an action that
// recorded no evidence at all, on Status, TeamActions, the replays, and the
// HTTP wire, whose evidence_snapshot_id would otherwise say evidence was
// withheld (#398, #564).
func TestAnOutsiderOfOnlyLabelledEvidenceSeesTheWireOfNone(t *testing.T) {
	now := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
	labelled := newLabelPlaneWith(t, planeOptions{
		choose: fullLabels, wireEvents: true, label: "secret",
		now: now, evidence: "labelled",
	})
	none := newLabelPlaneWith(t, planeOptions{
		choose: fullLabels, wireEvents: true, label: "secret",
		now: now, evidence: "none",
	})
	if labelled.completed.EvidenceSnapshotID == "" || len(labelled.completed.Evidence) != 1 {
		t.Fatal("the labelled plane recorded no pinned evidence; the probe is vacuous")
	}
	read := func(p labelPlane) (fleet.ActionRecord, fleet.ActionRecord, fleet.ActionRecord, fleet.ActionRecord, []byte) {
		t.Helper()
		outsider := p.reader(t)
		request := integrationRequestContext(p.now)
		status, err := p.dispatch.Status(outsider, fleet.StatusRequest{ID: p.completed.ID, Context: request})
		if err != nil {
			t.Fatal(err)
		}
		team, err := p.dispatch.TeamActions(outsider, fleet.TeamActionListRequest{
			Limit: 10, SourceIDs: [][]byte{[]byte("source")},
			PolicyIDs: [][]byte{[]byte("policy")}, Context: request,
		})
		if err != nil || len(team.Actions) != 1 {
			t.Fatalf("TeamActions = %d, %v", len(team.Actions), err)
		}
		owner := bindPlaneSubjectWith(t, p.authority, p.now, "owner", nil,
			auth.OperationDispatch, auth.OperationInvoke)
		enqueue := integrationEnqueueRequest(p.now, "labelled", "enqueue-labelled")
		enqueued, err := p.dispatch.Enqueue(owner, enqueue)
		if err != nil {
			t.Fatal(err)
		}
		invoked, err := p.dispatch.Invoke(owner, fleet.InvokeRequest{
			Enqueue: enqueue, ClaimID: []byte("claim-labelled"), Lease: time.Minute,
		})
		if err != nil {
			t.Fatal(err)
		}
		handler, err := webapi.NewFleetDispatchHandler(p.dispatch)
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(map[string]any{
			"request_id":     base64.RawURLEncoding.EncodeToString([]byte("request")),
			"correlation_id": base64.RawURLEncoding.EncodeToString([]byte("correlation")),
			"reason_code":    "test", "deadline": p.now.Add(time.Minute),
		})
		if err != nil {
			t.Fatal(err)
		}
		httpRequest := httptest.NewRequest(http.MethodPost,
			"http://fleet.test/api/v1/fleet/actions/"+
				base64.RawURLEncoding.EncodeToString(p.completed.ID)+"/status",
			bytes.NewReader(body)).WithContext(outsider)
		httpRequest.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httpRequest)
		if response.Code != http.StatusOK {
			t.Fatalf("HTTP status = %d: %s", response.Code, response.Body.String())
		}
		return status, team.Actions[0], enqueued, invoked, response.Body.Bytes()
	}
	gotStatus, gotTeam, gotEnqueue, gotInvoke, gotWire := read(labelled)
	wantStatus, wantTeam, wantEnqueue, wantInvoke, wantWire := read(none)
	for _, path := range []struct {
		name      string
		got, want fleet.ActionRecord
	}{
		{"Status", gotStatus, wantStatus}, {"TeamActions", gotTeam, wantTeam},
		{"enqueue replay", gotEnqueue, wantEnqueue}, {"invoke replay", gotInvoke, wantInvoke},
	} {
		if !reflect.DeepEqual(path.got, path.want) {
			t.Fatalf("%s to an outsider:\n%#v\nwant, as if no evidence was recorded,\n%#v",
				path.name, path.got, path.want)
		}
	}
	if !bytes.Equal(gotWire, wantWire) {
		t.Fatalf("HTTP Status to an outsider:\n%s\nwant\n%s", gotWire, wantWire)
	}
}
