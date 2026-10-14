// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisioninventorystore

import (
	"bytes"
	"encoding/json"
	outcomes "github.com/phrocker/shoal-oss/internal/decisionoutcomes"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"io"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"
)

type attributionWire struct {
	SubjectID, ActorID, ClientID []byte
	OnBehalfOf                   [][]byte
}

func wireAttribution(a Attribution) attributionWire {
	w := attributionWire{SubjectID: []byte(a.SubjectID), ActorID: []byte(a.ActorID), ClientID: []byte(a.ClientID)}
	for _, id := range a.OnBehalfOf {
		w.OnBehalfOf = append(w.OnBehalfOf, []byte(id))
	}
	return w
}
func unwireAttribution(w attributionWire) Attribution {
	a := Attribution{SubjectID: shoal.ID(w.SubjectID), ActorID: shoal.ID(w.ActorID), ClientID: shoal.ID(w.ClientID)}
	for _, id := range w.OnBehalfOf {
		a.OnBehalfOf = append(a.OnBehalfOf, shoal.ID(id))
	}
	return a
}

type intentWire struct {
	ReceiptID, ObservationID, RequestID, PredictionID shoal.ID
	Reporter                                          attributionWire
}
type entryWire struct {
	Intent                            intentWire
	State                             State
	OpenedAt, PublishedAt, ReceivedAt time.Time
	ReceiptDigest                     string
}
type snapshotWire struct {
	ID                   shoal.ID
	Binding              Binding
	Version              int64
	CreatedAt, UpdatedAt time.Time
	Entries              []entryWire
}
type payload struct {
	ScopeDigest string
	Snapshot    snapshotWire
}
type envelope struct {
	Schema   int
	Payload  json.RawMessage
	Checksum string
}

func toWire(s Snapshot) snapshotWire {
	w := snapshotWire{ID: s.ID, Binding: s.Binding, Version: s.Version, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt}
	for _, e := range s.Entries {
		i := e.Intent
		w.Entries = append(w.Entries, entryWire{intentWire{i.ReceiptID, i.ObservationID, i.RequestID, i.PredictionID, wireAttribution(i.Reporter)}, e.State, e.OpenedAt, e.PublishedAt, e.ReceivedAt, e.ReceiptDigest})
	}
	return w
}
func fromWire(w snapshotWire) Snapshot {
	s := Snapshot{ID: w.ID, Binding: w.Binding, Version: w.Version, CreatedAt: w.CreatedAt, UpdatedAt: w.UpdatedAt}
	for _, e := range w.Entries {
		i := e.Intent
		s.Entries = append(s.Entries, Entry{Intent{i.ReceiptID, i.ObservationID, i.RequestID, i.PredictionID, unwireAttribution(i.Reporter)}, e.State, e.OpenedAt, e.PublishedAt, e.ReceivedAt, e.ReceiptDigest})
	}
	return s
}
func snapshotID(scope string, w snapshotWire) shoal.ID {
	w.ID = ""
	return shoal.ID("target-inventory-snapshot:" + hash(jsonBytes(payload{scope, w})))
}
func validate(s Snapshot) bool {
	if !validBinding(s.Binding) || len(s.Entries) > MaxEntries || !validTime(s.CreatedAt) || !validTime(s.UpdatedAt) || s.UpdatedAt.Before(s.CreatedAt) {
		return false
	}
	version := int64(1 + len(s.Entries))
	seen := map[shoal.ID]bool{}
	latest := s.CreatedAt
	lastOpen := s.CreatedAt
	for _, e := range s.Entries {
		if !validIntent(e.Intent) || seen[e.Intent.ReceiptID] || !validTime(e.OpenedAt) || e.OpenedAt.Before(lastOpen) || e.OpenedAt.After(s.UpdatedAt) {
			return false
		}
		seen[e.Intent.ReceiptID] = true
		lastOpen = e.OpenedAt
		if e.OpenedAt.After(latest) {
			latest = e.OpenedAt
		}
		switch e.State {
		case Pending:
			if !e.PublishedAt.IsZero() || !e.ReceivedAt.IsZero() || e.ReceiptDigest != "" {
				return false
			}
		case Published:
			version++
			if !validTime(e.PublishedAt) || !validTime(e.ReceivedAt) || e.PublishedAt.Before(e.OpenedAt) || e.ReceivedAt.After(e.PublishedAt) || e.PublishedAt.After(s.UpdatedAt) || !digest(e.ReceiptDigest) {
				return false
			}
			if e.PublishedAt.After(latest) {
				latest = e.PublishedAt
			}
		default:
			return false
		}
	}
	return s.Version == version && s.UpdatedAt.Equal(latest)
}
func encode(scope string, s Snapshot) ([]byte, error) {
	if !validate(s) {
		return nil, ErrCorrupt
	}
	w := toWire(s)
	w.ID = snapshotID(scope, w)
	raw := jsonBytes(payload{scope, w})
	out := jsonBytes(envelope{1, raw, hash(raw)})
	if len(out) > MaxStoredBytes {
		return nil, ErrLimit
	}
	return out, nil
}
func decode(raw []byte, scope string) (Snapshot, error) {
	var zero Snapshot
	if len(raw) > MaxStoredBytes || shape(raw) != nil {
		return zero, ErrCorrupt
	}
	var e envelope
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&e) != nil || e.Schema != 1 || e.Checksum != hash(e.Payload) {
		return zero, ErrCorrupt
	}
	var p payload
	d = json.NewDecoder(bytes.NewReader(e.Payload))
	d.DisallowUnknownFields()
	if d.Decode(&p) != nil || p.ScopeDigest != scope || p.Snapshot.ID != snapshotID(scope, p.Snapshot) {
		return zero, ErrCorrupt
	}
	s := fromWire(p.Snapshot)
	if !validate(s) {
		return zero, ErrCorrupt
	}
	canonical, err := encode(scope, s)
	if err != nil || !bytes.Equal(raw, canonical) {
		return zero, ErrCorrupt
	}
	return s, nil
}

// Bound arrays before wide struct decoding. Canonical re-encoding additionally
// rejects alias fields, null substitutions, invalid UTF-8 and duplicate escapes.
func shape(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	tokens := 0
	var walk func(int, string) error
	walk = func(depth int, name string) error {
		tokens++
		if tokens > 100000 || depth > 16 {
			return ErrCorrupt
		}
		v, e := d.Token()
		if e != nil {
			return ErrCorrupt
		}
		delim, ok := v.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, e := d.Token()
				s, ok := key.(string)
				if e != nil || !ok || seen[s] || len(seen) >= 32 {
					return ErrCorrupt
				}
				seen[s] = true
				if e = walk(depth+1, s); e != nil {
					return e
				}
			}
			v, e = d.Token()
			if e != nil || v != json.Delim('}') {
				return ErrCorrupt
			}
		case '[':
			limit := MaxEntries
			if strings.EqualFold(name, "OnBehalfOf") {
				limit = 64
			}
			n := 0
			for d.More() {
				n++
				if n > limit {
					return ErrCorrupt
				}
				if e = walk(depth+1, ""); e != nil {
					return e
				}
			}
			v, e = d.Token()
			if e != nil || v != json.Delim(']') {
				return ErrCorrupt
			}
		default:
			return ErrCorrupt
		}
		return nil
	}
	if e := walk(0, ""); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return ErrCorrupt
	}
	return nil
}

type fullReceiptWire struct {
	ID, ObservationID        shoal.ID
	ObservationConfig        decision.OutcomeObservationConfig
	Reporter                 attributionWire
	AuthorizationFingerprint string
	ReceivedAt               time.Time
	State                    string
}

func receiptDigest(b Binding, i Intent, r outcomes.Receipt) (string, error) {
	a := Attribution{r.SubmitterID, r.ActorID, r.ClientID, r.OnBehalfOf}
	c := r.ObservationConfig
	if r.ID != i.ReceiptID || r.ObservationID != i.ObservationID || !reflect.DeepEqual(cloneIntent(Intent{Reporter: a}).Reporter, i.Reporter) || (!strings.HasPrefix(r.AuthorizationFingerprint, "auth-sha256:") || !digest(strings.TrimPrefix(r.AuthorizationFingerprint, "auth-sha256:"))) || !validTime(r.ReceivedAt) || r.State != "proposed" || c.RequestID != i.RequestID || c.PredictionID != i.PredictionID || c.SubjectID != b.SubjectID || c.QuestionID != b.QuestionID || c.Kind != decision.OutcomeCorrectness || c.ExecutionStatus != "" || c.ActionID != "" || !validTime(c.ObservedAt) || c.ObservedAt.After(r.ReceivedAt) || len(c.EvidenceIDs) < 1 || len(c.EvidenceIDs) > decision.MaxSources || !utf8.ValidString(c.Label) || len(c.Label) > shoal.MaxSemanticStringBytes || ((c.Label == "") == (c.Truth == nil)) {
		return "", invalid()
	}
	if c.Label != "" && strings.TrimSpace(c.Label) == "" {
		return "", invalid()
	}
	if c.Supersedes != "" && !receiptID(c.Supersedes) {
		return "", invalid()
	}
	for _, id := range []shoal.ID{c.AssertedProvenance.ReporterID, c.AssertedProvenance.ModelID, c.AssertedProvenance.PromptID, c.AssertedProvenance.ToolID} {
		if id != "" && !textID(id) {
			return "", invalid()
		}
	}
	var previous shoal.ID
	for _, id := range c.EvidenceIDs {
		if !textID(id) || (previous != "" && id <= previous) {
			return "", invalid()
		}
		previous = id
	}
	raw := jsonBytes(fullReceiptWire{r.ID, r.ObservationID, c, wireAttribution(a), r.AuthorizationFingerprint, r.ReceivedAt, r.State})
	if len(raw) > decision.MaxManifestBytes*2 {
		return "", ErrLimit
	}
	return hash(raw), nil
}
