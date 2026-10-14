package main

import (
	"context"
	"errors"
	"testing"

	"github.com/phrocker/shoal-oss/accumulo"
)

func TestFlattenWriteFailurePreservesStructuredDetails(t *testing.T) {
	rejection := &accumulo.MutationRejectionError{
		Server: "server:9997",
		FailedExtents: []accumulo.FailedExtent{{
			Extent: accumulo.TabletExtent{
				TableID: "5",
				PrevRow: []byte("a"),
				EndRow:  []byte("z"),
			},
			Submitted: 3,
			Committed: 2,
		}},
		ConstraintViolations: []accumulo.ConstraintViolation{{
			ConstraintClass:            "Constraint",
			ViolationCode:              7,
			Description:                "bad mutation",
			NumberOfViolatingMutations: 4,
		}},
		AuthorizationFailures: []accumulo.AuthorizationFailure{{
			Extent: accumulo.TabletExtent{TableID: "5"},
			Code:   "PERMISSION_DENIED",
		}},
	}
	cleanup := &accumulo.BatchWriterCleanupError{
		Server: "server:9997",
		Err:    errors.New("cancel failed"),
	}
	data := flattenWriteFailure(errors.Join(
		accumulo.ErrBatchWriterFailed,
		accumulo.ErrBatchWriterRetryExhausted,
		accumulo.ErrBatchWriterAutoFlush,
		rejection,
		cleanup,
	))

	if uint32(data.flags) != 7 {
		t.Fatalf("flags = %d, want 7", uint32(data.flags))
	}
	if len(data.failedExtents) != 1 ||
		data.failedExtents[0].value.Committed != 2 {
		t.Fatalf("failed extents = %#v", data.failedExtents)
	}
	if len(data.constraints) != 1 ||
		data.constraints[0].value.ViolationCode != 7 {
		t.Fatalf("constraints = %#v", data.constraints)
	}
	if len(data.authorizations) != 1 ||
		data.authorizations[0].value.Code != "PERMISSION_DENIED" {
		t.Fatalf("authorizations = %#v", data.authorizations)
	}
	if len(data.cleanups) != 1 ||
		data.cleanups[0].message != "cancel failed" {
		t.Fatalf("cleanups = %#v", data.cleanups)
	}
}

func TestStatusForWriterErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int32
	}{
		{"closed", accumulo.ErrBatchWriterClosed, 6},
		{"retry", accumulo.ErrBatchWriterRetryExhausted, 16},
		{
			"rejection",
			&accumulo.MutationRejectionError{Server: "server:9997"},
			17,
		},
		{
			"ambiguous",
			errors.Join(
				accumulo.ErrBatchWriterFailed,
				&accumulo.MutationRejectionError{Server: "server:9997"},
			),
			18,
		},
		{
			"ambiguous deadline",
			errors.Join(
				accumulo.ErrBatchWriterFailed,
				context.DeadlineExceeded,
			),
			18,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := int32(statusForError(test.err)); got != test.want {
				t.Fatalf("status = %d, want %d", got, test.want)
			}
		})
	}
}
