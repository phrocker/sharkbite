// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionartifacts

// EncodeRecord returns the exact existing canonical persisted representation of
// an immutable record. It checks that those bytes reconstruct the record. This
// pure helper performs no IO, registration, authentication or authorization;
// retaining preparation bytes never grants execution or training permission.
func EncodeRecord(r Record) ([]byte, error) {
	raw, err := encode(r)
	if err != nil {
		return nil, err
	}
	if _, err = decode(raw); err != nil {
		return nil, invalid()
	}
	return raw, nil
}

// DecodeRecord validates bounded canonical bytes using the existing persistence
// codec and returns detached record values. The caller must separately verify
// current authority and the expected request/content commitments before use.
func DecodeRecord(raw []byte) (Record, error) { return decode(raw) }
