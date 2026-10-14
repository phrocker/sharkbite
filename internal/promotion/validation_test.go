package promotion

import (
	"errors"
	"testing"

	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/storage/memory"
)

// TestValidateDestinationWritableRejectsNilDestination proves a nil dst
// is rejected the same way as any other non-writable backend, rather
// than silently passing. A round-11 Copilot review flagged an earlier
// "if dst == nil { return nil }" bypass here: its only justification
// (validateBulkDir's nil-dst call path) does not actually apply to this
// function at all -- validateBulkDir only ever reaches
// validateBulkDirOnBackend/isBackendRootOnBackend, never
// validateDestinationWritable -- so the bypass had no real caller
// relying on it, and it let Promote reach conn.AddTableSplitsForTable
// (which mutates the real Accumulo table's splits) with a nil dst
// before StageBulkDir's first storage.Copy call ever got a chance to
// discover the problem.
func TestValidateDestinationWritableRejectsNilDestination(t *testing.T) {
	var typedNil *memory.Backend
	for name, dst := range map[string]storage.Backend{
		"untyped nil": nil,
		"typed nil":   typedNil,
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateDestinationWritable(dst); !errors.Is(err, storage.ErrReadOnly) {
				t.Fatalf("validateDestinationWritable(%s) = %v, want an error wrapping %v", name, err, storage.ErrReadOnly)
			}
		})
	}
}

// TestValidatePromotionDestinationRejectsNilDestination proves the same
// at Promote's actual entry point: validatePromotionDestination is what
// Promote calls before doing anything else, so this is the check that
// must fail closed on a nil dst in practice, not just the lower-level
// helper in isolation.
func TestValidatePromotionDestinationRejectsNilDestination(t *testing.T) {
	var typedNil *memory.Backend
	for name, dst := range map[string]storage.Backend{
		"untyped nil": nil,
		"typed nil":   typedNil,
	} {
		t.Run(name, func(t *testing.T) {
			err := validatePromotionDestination(dst, "events", "hdfs://nn/bulk/events-1")
			if !errors.Is(err, storage.ErrReadOnly) {
				t.Fatalf("validatePromotionDestination(%s, ...) = %v, want an error wrapping %v", name, err, storage.ErrReadOnly)
			}
		})
	}
}
