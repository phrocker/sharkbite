// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisioninventorystore

// EncodeSnapshot retains an existing snapshot in the store's canonical,
// byte-preserving encoding. It verifies the supplied scoped snapshot identity.
// It performs no IO and establishes no coverage, membership or access authority.
func EncodeSnapshot(scope Scope, s Snapshot) ([]byte, error) {
	sd, e := scopeDigest(scope)
	if e != nil || !validate(s) {
		return nil, ErrCorrupt
	}
	// Bound aggregate payload before wire conversion and JSON allocation. Each
	// individually validated field is bounded; this stops a synthetic in-memory
	// snapshot from multiplying every per-field maximum across all entries.
	remaining := MaxStoredBytes
	charge := func(n int) bool {
		if n > remaining {
			return false
		}
		remaining -= n
		return true
	}
	for _, id := range []string{string(s.ID), string(s.Binding.CoverageID), string(s.Binding.TargetID), string(s.Binding.TaskID), string(s.Binding.PictureID), string(s.Binding.SubjectID), string(s.Binding.QuestionID)} {
		if !charge(len(id)) {
			return nil, ErrLimit
		}
	}
	for _, entry := range s.Entries {
		i := entry.Intent
		a := i.Reporter
		for _, id := range []string{string(i.ReceiptID), string(i.ObservationID), string(i.RequestID), string(i.PredictionID), string(a.SubjectID), string(a.ActorID), string(a.ClientID), entry.ReceiptDigest} {
			if !charge(len(id)) {
				return nil, ErrLimit
			}
		}
		for _, id := range a.OnBehalfOf {
			if !charge(len(id)) {
				return nil, ErrLimit
			}
		}
	}
	if s.ID != snapshotID(sd, toWire(s)) {
		return nil, ErrCorrupt
	}
	return encode(sd, s)
}

// DecodeSnapshot verifies bounded canonical retained bytes and their scoped
// identity. Successful decoding proves structural identity, not authenticity or
// present-day completeness; callers must authenticate the retained capture.
func DecodeSnapshot(scope Scope, raw []byte) (Snapshot, error) {
	sd, e := scopeDigest(scope)
	if e != nil {
		return Snapshot{}, ErrCorrupt
	}
	return decode(raw, sd)
}
