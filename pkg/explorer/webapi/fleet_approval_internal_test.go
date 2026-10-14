// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package webapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestApprovalErrorsNeverSurfaceAsInternal maps every approval sentinel,
// bare and wrapped, through the transport. writeError starts from 500, so a
// sentinel that reached it without a code would read to a client as a server
// fault — and "approval required" is the one answer a client must branch on.
func TestApprovalErrorsNeverSurfaceAsInternal(t *testing.T) {
	for _, probe := range []struct {
		name   string
		mapped error
		status int
		code   shoal.ErrorCode
	}{
		{"bare approval required", fleetDispatchError(fleet.ErrApprovalRequired),
			http.StatusConflict, shoal.ErrorConflict},
		{"wrapped approval required", fleetDispatchError(errors.Join(
			errors.New("context"), fleet.ErrApprovalRequired)),
			http.StatusConflict, shoal.ErrorConflict},
		{"approval conflict", fleetApprovalError(fleet.ErrApprovalConflict),
			http.StatusConflict, shoal.ErrorConflict},
		{"approval expired", fleetApprovalError(fleet.ErrApprovalExpired),
			http.StatusConflict, shoal.ErrorConflict},
		{"approval superseded", fleetApprovalError(fleet.ErrApprovalSuperseded),
			http.StatusConflict, shoal.ErrorConflict},
		{"approver mapping moved", fleetApprovalError(fleet.ErrApproverMappingMoved),
			http.StatusConflict, shoal.ErrorConflict},
		{"wrapped approver mapping moved", fleetApprovalError(errors.Join(
			errors.New("context"), fleet.ErrApproverMappingMoved)),
			http.StatusConflict, shoal.ErrorConflict},
		{"approval not found", fleetApprovalError(fleet.ErrApprovalNotFound),
			http.StatusNotFound, shoal.ErrorNotFound},
	} {
		recorder := httptest.NewRecorder()
		writeError(recorder, probe.mapped)
		var body struct {
			Code shoal.ErrorCode `json:"code"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != probe.status || body.Code != probe.code {
			t.Fatalf("%s = %d %q, want %d %q", probe.name,
				recorder.Code, body.Code, probe.status, probe.code)
		}
	}
}

// TestAnUnpublishedApprovalEventIsAnUnmarkedRetry (#653): a transition that
// committed and whose event did not publish answers 503, so the caller
// retries, and is not marked indeterminate, because nothing external was
// contacted. Its cause never decides the answer, not even a conflict, which
// on its own maps to 409.
func TestAnUnpublishedApprovalEventIsAnUnmarkedRetry(t *testing.T) {
	for name, err := range map[string]error{
		"bare": fleet.ErrApprovalEventUnpublished,
		"with a conflicting cause": errors.Join(
			fleet.ErrApprovalEventUnpublished, fleet.ErrApprovalConflict),
		"coded": shoal.WrapError(shoal.ErrorUnavailable, "event backend down",
			fleet.ErrApprovalEventUnpublished),
	} {
		recorder := httptest.NewRecorder()
		writeError(recorder, fleetApprovalError(err))
		if recorder.Code != http.StatusServiceUnavailable ||
			recorder.Header().Get(CommitOutcomeHeader) != "" {
			t.Fatalf("%s = %d, outcome %q", name, recorder.Code,
				recorder.Header().Get(CommitOutcomeHeader))
		}
	}
	// A committed action beside it is still marked: the exclusion holds.
	recorder := httptest.NewRecorder()
	writeError(recorder, fleetApprovalError(errors.Join(
		fleet.ErrApprovalEventUnpublished, fleet.ErrActionCommitted)))
	if recorder.Header().Get(CommitOutcomeHeader) != CommitOutcomeIndeterminate {
		t.Fatal("a committed action joined with an unpublished event lost its mark")
	}
}
