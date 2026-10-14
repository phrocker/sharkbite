package main

import (
	"context"
	"testing"

	"github.com/phrocker/shoal-oss/accumulo"
)

func TestStatusForTableAdministrationErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int32
	}{
		{"table exists", accumulo.ErrTableExists, 19},
		{"invalid table name", accumulo.ErrInvalidTableName, 1},
		{"invalid table range", accumulo.ErrInvalidTableRange, 1},
		{"constraint number unavailable", accumulo.ErrConstraintNumberUnavailable, 16},
		{"invalid property", accumulo.ErrInvalidProperty, 1},
		{"namespace missing", accumulo.ErrNamespaceNotFound, 9},
		{"manager unavailable", accumulo.ErrManagerUnavailable, 20},
		{"client service unavailable", accumulo.ErrClientServiceUnavailable, 20},
		{"deadline", context.DeadlineExceeded, 8},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := int32(statusForError(test.err)); got != test.want {
				t.Fatalf("status = %d, want %d", got, test.want)
			}
		})
	}
}
