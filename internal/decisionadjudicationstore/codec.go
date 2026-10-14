// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionadjudicationstore

import (
	"bytes"
	"encoding/json"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"io"
	"strings"
	"unicode/utf8"
)

type attributionWire struct {
	SubjectID, ActorID, ClientID []byte
	OnBehalfOf                   [][]byte
	AuthorizationFingerprint     string
}

func wireAttribution(a Attribution) attributionWire {
	w := attributionWire{SubjectID: []byte(a.SubjectID), ActorID: []byte(a.ActorID), ClientID: []byte(a.ClientID), AuthorizationFingerprint: a.AuthorizationFingerprint}
	if a.OnBehalfOf != nil {
		w.OnBehalfOf = make([][]byte, len(a.OnBehalfOf))
		for i, id := range a.OnBehalfOf {
			w.OnBehalfOf[i] = []byte(id)
		}
	}
	return w
}
func unwireAttribution(w attributionWire) Attribution {
	a := Attribution{SubjectID: shoal.ID(w.SubjectID), ActorID: shoal.ID(w.ActorID), ClientID: shoal.ID(w.ClientID), AuthorizationFingerprint: w.AuthorizationFingerprint}
	if w.OnBehalfOf != nil {
		a.OnBehalfOf = make([]shoal.ID, len(w.OnBehalfOf))
		for i, id := range w.OnBehalfOf {
			a.OnBehalfOf[i] = shoal.ID(id)
		}
	}
	return a
}

type receiptWire struct {
	Receipt
	Adjudicator attributionWire
}
type entryWire struct {
	Receipt                   receiptWire
	KeyDigest, IdentityDigest string
}
type journalWire struct {
	ScopeDigest string
	TargetID    shoal.ID
	Entries     []entryWire
}
type envelope struct {
	Schema   int
	Payload  json.RawMessage
	Checksum string
}

func encode(j journal) ([]byte, error) {
	w := journalWire{ScopeDigest: j.ScopeDigest, TargetID: j.TargetID, Entries: make([]entryWire, len(j.Entries))}
	for i, e := range j.Entries {
		r := e.Receipt
		r.Adjudicator = Attribution{}
		w.Entries[i] = entryWire{receiptWire{r, wireAttribution(e.Receipt.Adjudicator)}, e.KeyDigest, e.IdentityDigest}
	}
	payload, e := json.Marshal(w)
	if e != nil {
		return nil, ErrCorrupt
	}
	raw, e := json.Marshal(envelope{1, payload, hash(payload)})
	if e != nil {
		return nil, ErrCorrupt
	}
	if len(raw) > MaxStoredBytes {
		return nil, ErrLimit
	}
	return raw, nil
}
func decode(raw []byte) (journal, error) {
	var j journal
	var env envelope
	if len(raw) > MaxStoredBytes {
		return j, ErrCorrupt
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(&env); e != nil {
		return j, ErrCorrupt
	}
	if _, e := d.Token(); e != io.EOF || env.Schema != 1 || env.Checksum != hash(env.Payload) {
		return j, ErrCorrupt
	}
	if e := boundedShape(env.Payload); e != nil {
		return j, ErrCorrupt
	}
	var w journalWire
	d = json.NewDecoder(bytes.NewReader(env.Payload))
	d.DisallowUnknownFields()
	if e := d.Decode(&w); e != nil {
		return j, ErrCorrupt
	}
	if _, e := d.Token(); e != io.EOF {
		return j, ErrCorrupt
	}
	j = journal{ScopeDigest: w.ScopeDigest, TargetID: w.TargetID, Entries: make([]entry, len(w.Entries))}
	for i, e := range w.Entries {
		r := e.Receipt.Receipt
		r.Adjudicator = unwireAttribution(e.Receipt.Adjudicator)
		j.Entries[i] = entry{r, e.KeyDigest, e.IdentityDigest}
	}
	canonical, e := encode(j)
	if e != nil || !bytes.Equal(canonical, raw) {
		return journal{}, ErrCorrupt
	}
	if e = validateJournal(j); e != nil {
		return journal{}, e
	}
	return j, nil
}
func textID(id shoal.ID) bool {
	return shoal.ValidateRequiredID("id", id) == nil && utf8.ValidString(string(id)) && strings.TrimSpace(string(id)) != ""
}
func sortedReferences(ids []shoal.ID, prefix string, min, max int) bool {
	if len(ids) < min || len(ids) > max {
		return false
	}
	for i, id := range ids {
		if !textID(id) || (i > 0 && ids[i-1] >= id) || (prefix != "" && (!strings.HasPrefix(string(id), prefix) || !validHash(strings.TrimPrefix(string(id), prefix)))) {
			return false
		}
	}
	return true
}
func validateJournal(j journal) error {
	if !validHash(j.ScopeDigest) || !textID(j.TargetID) || len(j.Entries) == 0 || len(j.Entries) > MaxEntries {
		return ErrCorrupt
	}
	seen := map[shoal.ID]bool{}
	for i, e := range j.Entries {
		r := e.Receipt
		c := r.ProposalConfig
		if !textID(r.BasisID) || !validHash(e.KeyDigest) || !validHash(e.IdentityDigest) || !validAttribution(r.Adjudicator) || identityDigest(r.Adjudicator) != e.IdentityDigest || r.ID != receiptID(j.ScopeDigest, j.TargetID, e.IdentityDigest, e.KeyDigest) || seen[r.ID] || r.Version != int64(i+1) || r.TargetID != j.TargetID || !validTime(r.ReceivedAt) {
			return ErrCorrupt
		}
		seen[r.ID] = true
		var previous shoal.ID
		if i > 0 {
			previous = j.Entries[i-1].Receipt.ID
			if r.ReceivedAt.Before(j.Entries[i-1].Receipt.ReceivedAt) || r.PolicyID != j.Entries[0].Receipt.PolicyID {
				return ErrCorrupt
			}
		}
		if c.ExpectedHeadID != previous || c.ExpectedVersion != int64(i) || !textID(r.TaskID) || !textID(r.PictureID) || !textID(r.PolicyID) || !textID(r.ProposalID) || !textID(c.RequestID) || !textID(c.PredictionID) || !sortedReferences(c.ObservationReceiptIDs, "outcome-receipt:", 1, decision.MaxAdjudicationObservations) || !sortedReferences(c.WitnessIDs, "", 0, decision.MaxAdjudicationWitnesses) {
			return ErrCorrupt
		}
		target, err := decision.AdjudicationTargetID(r.TaskID, r.PictureID, c.SubjectID, c.QuestionID)
		if err != nil || target != j.TargetID {
			return ErrCorrupt
		}
		proposal, err := decision.AdjudicationProposalID(r.PolicyID, r.TargetID, c)
		if err != nil || proposal != r.ProposalID {
			return ErrCorrupt
		}
		switch c.Disposition {
		case decision.AdjudicationVerified:
			if c.Reason != "" || len(c.WitnessIDs) == 0 || (c.Label == "") == (c.Truth == nil) {
				return ErrCorrupt
			}
		case decision.AdjudicationDisputed, decision.AdjudicationUnresolved:
			if c.Label != "" || c.Truth != nil || strings.TrimSpace(c.Reason) == "" || (c.Disposition == decision.AdjudicationDisputed && len(c.WitnessIDs) == 0) {
				return ErrCorrupt
			}
		default:
			return ErrCorrupt
		}
	}
	return nil
}

// Check collection counts before decoding wide receipt structs. A byte bound
// alone would permit millions of tiny array items with disproportionate heap
// allocations. Counts here are structural; validateJournal enforces semantics.
func boundedShape(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var visit func(int, string) error
	visit = func(depth int, name string) error {
		if depth > 16 {
			return ErrCorrupt
		}
		token, e := d.Token()
		if e != nil {
			return ErrCorrupt
		}
		delimiter, container := token.(json.Delim)
		if !container {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				if len(seen) >= 64 {
					return ErrCorrupt
				}
				token, e := d.Token()
				if e != nil {
					return ErrCorrupt
				}
				key, ok := token.(string)
				if !ok || seen[key] {
					return ErrCorrupt
				}
				seen[key] = true
				if e = visit(depth+1, key); e != nil {
					return e
				}
			}
			token, e = d.Token()
			if e != nil || token != json.Delim('}') {
				return ErrCorrupt
			}
		case '[':
			limit := decision.MaxAdjudicationWitnesses
			if strings.EqualFold(name, "Entries") {
				limit = MaxEntries
			}
			count := 0
			for d.More() {
				count++
				if count > limit {
					return ErrCorrupt
				}
				if e = visit(depth+1, ""); e != nil {
					return e
				}
			}
			token, e = d.Token()
			if e != nil || token != json.Delim(']') {
				return ErrCorrupt
			}
		default:
			return ErrCorrupt
		}
		return nil
	}
	if e := visit(0, ""); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return ErrCorrupt
	}
	return nil
}
