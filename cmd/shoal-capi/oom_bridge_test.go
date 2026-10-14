//go:build shoal_capi_test

package main

import (
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/accumulo"
)

func TestFailReturnsOutOfMemoryWhenErrorObjectAllocationFailsImmediately(t *testing.T) {
	testErrorAllocFailAfter(0)
	defer testErrorAllocReset()

	status, outErrorNil, message := testFailResult("shoal: invalid input")
	if status != 3 {
		t.Fatalf("status = %d, want 3", status)
	}
	if !outErrorNil {
		t.Fatal("outError should remain nil when error object allocation fails")
	}
	if message != "" {
		t.Fatalf("message = %q, want empty", message)
	}
}

func TestFailReturnsOriginalStatusWhenErrorObjectAllocationSucceeds(t *testing.T) {
	testErrorAllocFailAfter(1)
	defer testErrorAllocReset()

	status, outErrorNil, message := testFailResult("shoal: invalid input")
	if status != 1 {
		t.Fatalf("status = %d, want 1", status)
	}
	if outErrorNil {
		t.Fatal("outError should be populated when error object allocation succeeds")
	}
	if message != "shoal: invalid input" {
		t.Fatalf("message = %q, want exact propagated error", message)
	}
}

func TestFailReturnsOutOfMemoryWhenErrorMessageAllocationFails(t *testing.T) {
	testErrorAllocFailAfter(1)
	defer testErrorAllocReset()
	testErrorMessageAllocFailAfter(0)
	defer testErrorMessageAllocReset()

	status, outErrorNil, message := testFailResult("shoal: invalid input")
	if status != 3 {
		t.Fatalf("status = %d, want 3", status)
	}
	if !outErrorNil {
		t.Fatal("outError should remain nil when error message allocation fails")
	}
	if message != "" {
		t.Fatalf("message = %q, want empty", message)
	}
}

func TestFinishWriteReturnsOutOfMemoryWhenWriteFailureDetailAllocationFails(t *testing.T) {
	tests := []struct {
		name        string
		failAfter   uint
		wantMessage string
	}{
		{name: "failed extent server", failAfter: 0, wantMessage: "failed extent 0 server"},
		{name: "failed extent table id", failAfter: 1, wantMessage: "failed extent 0 table id"},
		{name: "constraint server", failAfter: 2, wantMessage: "constraint 0 server"},
		{name: "constraint class", failAfter: 3, wantMessage: "constraint 0 class"},
		{name: "constraint description", failAfter: 4, wantMessage: "constraint 0 description"},
		{name: "authorization server", failAfter: 5, wantMessage: "authorization 0 server"},
		{name: "authorization table id", failAfter: 6, wantMessage: "authorization 0 table id"},
		{name: "authorization code", failAfter: 7, wantMessage: "authorization 0 code"},
		{name: "cleanup server", failAfter: 8, wantMessage: "cleanup 0 server"},
		{name: "cleanup message", failAfter: 9, wantMessage: "cleanup 0 message"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testStringAllocFailAfter(test.failAfter)
			defer testStringAllocReset()

			status, outFailureNil, outErrorNil, message := testFinishWriteWithStructuredFailure()
			if status != 3 {
				t.Fatalf("status = %d, want 3", status)
			}
			if !outFailureNil {
				t.Fatal("outFailure should remain nil on detail allocation failure")
			}
			if outErrorNil {
				t.Fatal("outError should contain the propagated OOM failure")
			}
			if !strings.Contains(message, test.wantMessage) {
				t.Fatalf("error = %q, want substring %q", message, test.wantMessage)
			}
		})
	}
}

func TestAllocateWriteFailureReturnsPreciseErrorOnBridgeStringAllocationFailure(t *testing.T) {
	testStringAllocFailAfter(1)
	defer testStringAllocReset()

	status, message, failureNil := testAllocateWriteFailure(writeFailureData{
		failedExtents: []flattenedFailedExtent{{
			server: "server:9997",
			value: accumulo.FailedExtent{
				Extent: accumulo.TabletExtent{TableID: "5"},
			},
		}},
	})
	if status != 3 {
		t.Fatalf("status = %d, want 3", status)
	}
	if !failureNil {
		t.Fatal("failure should remain nil when table id allocation fails")
	}
	if !strings.Contains(message, "failed extent 0 table id") {
		t.Fatalf("error = %q, want failed extent allocation detail", message)
	}
}
