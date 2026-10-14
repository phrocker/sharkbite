// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionoutcomes

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"io"
	"time"
)

const maxStoredBytes = 2*decision.MaxManifestBytes + 64*1024

type envelope struct {
	Schema   int
	Payload  json.RawMessage
	Checksum string
}

// Authentication identifiers are opaque bytes, unlike asserted decision IDs.
// Encode them as byte strings so JSON cannot replace invalid UTF-8 code units.
// The API receipt still exposes the exact shoal.ID values returned by auth.
type receiptWire struct {
	ID                       shoal.ID
	ObservationID            shoal.ID
	ObservationConfig        decision.OutcomeObservationConfig
	SubmitterID              []byte
	ActorID                  []byte
	ClientID                 []byte
	OnBehalfOf               [][]byte
	AuthorizationFingerprint string
	ReceivedAt               time.Time
	State                    string
}
type rowWire struct {
	Receipt     receiptWire
	ScopeDigest string
	KeyDigest   string
}

func toWire(r row) rowWire {
	c := r.Receipt
	wire := receiptWire{ID: c.ID, ObservationID: c.ObservationID, ObservationConfig: c.ObservationConfig, SubmitterID: []byte(c.SubmitterID), ActorID: []byte(c.ActorID), ClientID: []byte(c.ClientID), AuthorizationFingerprint: c.AuthorizationFingerprint, ReceivedAt: c.ReceivedAt, State: c.State}
	if c.OnBehalfOf != nil {
		wire.OnBehalfOf = make([][]byte, len(c.OnBehalfOf))
		for i, id := range c.OnBehalfOf {
			wire.OnBehalfOf[i] = []byte(id)
		}
	}
	return rowWire{wire, r.ScopeDigest, r.KeyDigest}
}
func fromWire(w rowWire) row {
	c := w.Receipt
	receipt := Receipt{ID: c.ID, ObservationID: c.ObservationID, ObservationConfig: c.ObservationConfig, SubmitterID: shoal.ID(c.SubmitterID), ActorID: shoal.ID(c.ActorID), ClientID: shoal.ID(c.ClientID), AuthorizationFingerprint: c.AuthorizationFingerprint, ReceivedAt: c.ReceivedAt, State: c.State}
	if c.OnBehalfOf != nil {
		receipt.OnBehalfOf = make([]shoal.ID, len(c.OnBehalfOf))
		for i, id := range c.OnBehalfOf {
			receipt.OnBehalfOf[i] = shoal.ID(id)
		}
	}
	return row{receipt, w.ScopeDigest, w.KeyDigest}
}
func encode(r row) ([]byte, error) {
	b, e := json.Marshal(toWire(r))
	if e != nil {
		return nil, e
	}
	out, e := json.Marshal(envelope{1, b, hash(b)})
	if e != nil || len(out) > maxStoredBytes {
		return nil, errors.New("outcome exceeds bound")
	}
	return out, nil
}

// Re-encoding must match byte-for-byte. This rejects duplicate/unknown/case
// aliases, missing fields, null scalar defaults, trailing bytes and bad Unicode.
func decode(raw []byte) (row, error) {
	var r row
	var env envelope
	if len(raw) > maxStoredBytes {
		return r, ErrUnavailable
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(&env); e != nil {
		return r, e
	}
	if _, e := d.Token(); e != io.EOF {
		return r, ErrUnavailable
	}
	if env.Schema != 1 || env.Checksum != hash(env.Payload) {
		return r, ErrUnavailable
	}
	d = json.NewDecoder(bytes.NewReader(env.Payload))
	d.DisallowUnknownFields()
	var wire rowWire
	if e := d.Decode(&wire); e != nil {
		return r, e
	}
	if _, e := d.Token(); e != io.EOF {
		return r, ErrUnavailable
	}
	r = fromWire(wire)
	canonical, e := encode(r)
	if e != nil || !bytes.Equal(canonical, raw) || !validReceiptID(r.Receipt.ID) {
		return r, ErrUnavailable
	}
	return r, nil
}
