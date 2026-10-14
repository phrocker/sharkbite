// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package explorerfleet

import (
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/guard"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/transaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// TestPublicErrorNamesTheObjectTheCallerAsked is #633's diagnosability half.
//
// publicError was shared by the agent, action and approval stores with one
// hardcoded "agent not found". A transient not-found on a dispatch action read
// therefore reported a missing *agent*, and two investigations went to the
// agent head cell and proved it sound before anyone noticed the message
// belonged to another store. A wrong noun in an error is not cosmetic: it is
// the first thing anyone reads, and it sends them to the wrong component.
func TestPublicErrorNamesTheObjectTheCallerAsked(t *testing.T) {
	for _, probe := range []struct {
		subject string
		source  error
	}{
		{"agent", transaction.ErrNotFound},
		{"action", transaction.ErrNotFound},
		{"approval", transaction.ErrNotFound},
		{"action", guard.ErrNotFound},
	} {
		err := publicError(probe.subject, probe.source)
		if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			t.Fatalf("publicError(%q, %v) code = %v, want not-found",
				probe.subject, probe.source, err)
		}
		if !strings.Contains(err.Error(), probe.subject+" not found") {
			t.Fatalf("publicError(%q, %v) = %q, want it to name %q",
				probe.subject, probe.source, err.Error(), probe.subject)
		}
		// And it must not name a different store's object. This is the
		// assertion that fails if the subject is ignored and the old
		// hardcoded noun comes back.
		for _, other := range []string{"agent", "action", "approval"} {
			if other == probe.subject {
				continue
			}
			if strings.Contains(err.Error(), other+" not found") {
				t.Fatalf("publicError(%q, %v) = %q, which names %q — a read "+
					"of one object must not report another as missing",
					probe.subject, probe.source, err.Error(), other)
			}
		}
	}
}

// TestPublicErrorSubjectOnlyAffectsNotFound pins that the subject is not
// spliced into the errors that are genuinely store-neutral, so the parameter
// cannot quietly change what every other arm reports.
func TestPublicErrorSubjectOnlyAffectsNotFound(t *testing.T) {
	conflict := publicError("action", transaction.ErrConflict)
	if !shoal.IsErrorCode(conflict, shoal.ErrorConflict) {
		t.Fatalf("conflict code = %v", conflict)
	}
	if strings.Contains(conflict.Error(), "action") {
		t.Fatalf("conflict = %q, want the store-neutral wording",
			conflict.Error())
	}
	invalid := publicError("approval", transaction.ErrInvalid)
	if !shoal.IsErrorCode(invalid, shoal.ErrorInvalidArgument) {
		t.Fatalf("invalid code = %v", invalid)
	}
}
