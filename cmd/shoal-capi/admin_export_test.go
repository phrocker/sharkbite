package main

import (
	"testing"

	"github.com/phrocker/shoal-oss/accumulo"
)

func TestStatusForAdministrationErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int32
	}{
		{"namespace exists", accumulo.ErrNamespaceExists, 19},
		{"namespace not empty", accumulo.ErrNamespaceNotEmpty, 21},
		{"invalid namespace", accumulo.ErrInvalidNamespaceName, 1},
		{"invalid split", accumulo.ErrInvalidTableSplit, 1},
		{"table offline", accumulo.ErrTableOffline, 22},
		{"splits incomplete", accumulo.ErrTableSplitsIncomplete, 26},
		{"user exists", accumulo.ErrUserExists, 19},
		{"user missing", accumulo.ErrUserNotFound, 23},
		{"bad credentials", accumulo.ErrBadCredentials, 24},
		{"invalid authorizations", accumulo.ErrInvalidAuthorizations, 1},
		{"invalid permission", accumulo.ErrInvalidPermission, 1},
		{"unsupported", accumulo.ErrUnsupportedOperation, 4},
		{"security unavailable", accumulo.ErrSecurityUnavailable, 25},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := int32(statusForError(test.err)); got != test.want {
				t.Fatalf("status = %d, want %d", got, test.want)
			}
		})
	}
}
