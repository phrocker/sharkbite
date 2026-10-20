// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

package api

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDelayedActionTimeSurvivesBothRecordDecoders(t *testing.T) {
	due := time.Date(2026, 9, 6, 12, 30, 0, 0, time.UTC)
	fromAction, err := DecodeRecord(Action{ID: EncodeID([]byte("action")), NotBefore: due})
	if err != nil || !fromAction.NotBefore.Equal(due) {
		t.Fatalf("decoded action = %#v, %v", fromAction, err)
	}
	var wire recordWire
	if err := json.Unmarshal([]byte(`{"id":"YWN0aW9u","not_before":"2026-09-06T12:30:00Z"}`), &wire); err != nil {
		t.Fatal(err)
	}
	fromWire, err := decodeRecord(wire)
	if err != nil || !fromWire.NotBefore.Equal(due) {
		t.Fatalf("decoded HTTP record = %#v, %v", fromWire, err)
	}
}
