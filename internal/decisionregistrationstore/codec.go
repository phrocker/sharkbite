// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionregistrationstore

import (
	"bytes"
	"encoding/json"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"io"
	"time"
)

type scopeWire struct {
	Domain, SubjectID, ActorID, ClientID []byte
	OnBehalfOf                           [][]byte
}
type primaryWire struct {
	ID                            shoal.ID
	Scope                         scopeWire
	KeySHA256                     string
	State                         State
	Version                       int64
	Frozen                        Frozen
	FrozenSHA256                  string
	CreatedAt, UpdatedAt, ReadyAt time.Time
}
type alias struct {
	PrimaryID, RequestID       shoal.ID
	FrozenSHA256, RecordSHA256 string
}
type envelope struct {
	Schema   int
	Payload  json.RawMessage
	Checksum string
}

func scopeToWire(s Scope) scopeWire {
	w := scopeWire{Domain: bytes.Clone(s.Domain), SubjectID: []byte(s.SubjectID), ActorID: []byte(s.ActorID), ClientID: []byte(s.ClientID)}
	for _, id := range s.OnBehalfOf {
		w.OnBehalfOf = append(w.OnBehalfOf, []byte(id))
	}
	return w
}
func scopeFromWire(w scopeWire) Scope {
	s := Scope{Domain: bytes.Clone(w.Domain), SubjectID: shoal.ID(w.SubjectID), ActorID: shoal.ID(w.ActorID), ClientID: shoal.ID(w.ClientID)}
	for _, id := range w.OnBehalfOf {
		s.OnBehalfOf = append(s.OnBehalfOf, shoal.ID(id))
	}
	return s
}
func encode(v any) ([]byte, error) {
	p, e := json.Marshal(v)
	if e != nil || len(p) > maxPrimaryBytes {
		return nil, invalid()
	}
	b, e := json.Marshal(envelope{1, p, hash(p)})
	if e != nil || len(b) > maxPrimaryBytes {
		return nil, invalid()
	}
	return b, nil
}

// Bound JSON allocation before unmarshalling source/delegation slices or maps.
func preflight(b []byte) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	nodes := 0
	var walk func(string, int) error
	walk = func(field string, depth int) error {
		nodes++
		if nodes > 8192 || depth > 12 {
			return ErrCorrupt
		}
		t, e := d.Token()
		if e != nil {
			return e
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			n := 0
			for d.More() {
				n++
				if n > 32 {
					return ErrCorrupt
				}
				k, e := d.Token()
				if e != nil {
					return e
				}
				name, ok := k.(string)
				if !ok || len(name) > 64 {
					return ErrCorrupt
				}
				if e = walk(name, depth+1); e != nil {
					return e
				}
			}
		case '[':
			max := 0
			switch field {
			case "Sources":
				max = MaxSources
			case "OnBehalfOf":
				max = 64
			default:
				return ErrCorrupt
			}
			n := 0
			for d.More() {
				n++
				if n > max {
					return ErrCorrupt
				}
				if e = walk("", depth+1); e != nil {
					return e
				}
			}
		default:
			return ErrCorrupt
		}
		_, e = d.Token()
		return e
	}
	if e := walk("", 0); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return ErrCorrupt
	}
	return nil
}
func decode(b []byte, v any) error {
	if len(b) == 0 || len(b) > maxPrimaryBytes || preflight(b) != nil {
		return ErrCorrupt
	}
	var env envelope
	strict := func(raw []byte, out any) error {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.DisallowUnknownFields()
		if e := d.Decode(out); e != nil {
			return e
		}
		if _, e := d.Token(); e != io.EOF {
			return ErrCorrupt
		}
		return nil
	}
	if strict(b, &env) != nil || env.Schema != 1 || env.Checksum != hash(env.Payload) || strict(env.Payload, v) != nil {
		return ErrCorrupt
	}
	canonical, e := encode(v)
	if e != nil || !bytes.Equal(canonical, b) {
		return ErrCorrupt
	}
	return nil
}
func encodePrimary(r Registration, keySHA string) ([]byte, error) {
	return encode(primaryWire{r.ID, scopeToWire(r.Scope), keySHA, r.State, r.Version, r.Frozen, r.FrozenSHA256, r.CreatedAt, r.UpdatedAt, r.ReadyAt})
}
func decodePrimary(b []byte) (Registration, string, error) {
	var w primaryWire
	if decode(b, &w) != nil {
		return Registration{}, "", ErrCorrupt
	}
	s := scopeFromWire(w.Scope)
	f, e := normalizeFrozen(w.Frozen)
	if e != nil || !validScope(s) || !digest(w.KeySHA256) || w.ID != primaryID(s, w.KeySHA256) || w.FrozenSHA256 != hash(jsonBytes(f)) || !bytes.Equal(jsonBytes(f), jsonBytes(w.Frozen)) || !validTime(w.CreatedAt) || !validTime(w.UpdatedAt) || w.CreatedAt.Before(f.AcceptedAt) || w.UpdatedAt.Before(w.CreatedAt) {
		return Registration{}, "", ErrCorrupt
	}
	if (w.State == Preparing && (w.Version != 1 || !w.ReadyAt.IsZero() || w.CreatedAt != w.UpdatedAt)) || (w.State == Ready && (w.Version != 2 || !validTime(w.ReadyAt) || w.ReadyAt != w.UpdatedAt)) || (w.State != Preparing && w.State != Ready) {
		return Registration{}, "", ErrCorrupt
	}
	return Registration{w.ID, s, w.State, w.Version, f, w.FrozenSHA256, w.CreatedAt, w.UpdatedAt, w.ReadyAt}, w.KeySHA256, nil
}
