// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package interaction

import (
	"unicode"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// ValidateCorrelationID checks the shape a session's CorrelationID must have:
// empty, or a bounded, valid-UTF-8, printable identifier with no spaces. It is
// the same shape the hosted authenticators enforce when they mint a decision
// from a Shoal-Correlation-ID header (#527), repeated here because the session
// is a durable record something eventually renders, and a decision can be
// minted by paths other than those authenticators.
//
// The value is caller-supplyable grouping provenance. It is never an
// identity, a digest input or an authorization input.
func ValidateCorrelationID(id shoal.ID) error {
	if id == "" {
		return nil
	}
	if err := shoal.ValidateRequiredID(
		"interaction correlation ID", id,
	); err != nil {
		return err
	}
	if !utf8.ValidString(string(id)) {
		return shoal.NewError(
			shoal.ErrorInvalidArgument,
			"interaction correlation ID must be valid UTF-8")
	}
	for _, character := range string(id) {
		if !unicode.IsPrint(character) || character == ' ' {
			return shoal.NewError(
				shoal.ErrorInvalidArgument,
				"interaction correlation ID must be printable and contain "+
					"no spaces")
		}
	}
	return nil
}

// RecordableCorrelationID returns id when it is a valid session correlation
// and the empty string when it is not.
//
// It is what a trusted sink stamps from a decision. A malformed correlation is
// dropped rather than refused: correlation is metadata about an operation, and
// an operation that was otherwise authorized and audited must not fail, or
// leave no audit, because its grouping label was unusable. Dropping records
// the truth — this session has no usable correlation — and never substitutes a
// made-up one.
func RecordableCorrelationID(id shoal.ID) shoal.ID {
	if ValidateCorrelationID(id) != nil {
		return ""
	}
	return id
}
