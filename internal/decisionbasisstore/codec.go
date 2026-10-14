// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionbasisstore

import (
	"bytes"
	"encoding/json"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"io"
	"strings"
)

type payload struct {
	ProposalID, BasisID shoal.ID
	Config              decision.AdjudicationBasisConfig
}
type envelope struct {
	Schema   int
	Payload  json.RawMessage
	Checksum string
}

func encode(b decision.AdjudicationBasis) ([]byte, error) {
	raw, e := json.Marshal(payload{b.Proposal().ID(), b.ID(), b.Config()})
	if e != nil {
		return nil, ErrCorrupt
	}
	out, e := json.Marshal(envelope{1, raw, hash(raw)})
	if e != nil || len(out) > MaxStoredBytes {
		return nil, ErrCorrupt
	}
	return out, nil
}
func decode(raw []byte, id shoal.ID, proposal decision.AdjudicationProposal) (decision.AdjudicationBasis, error) {
	var zero decision.AdjudicationBasis
	if len(raw) > MaxStoredBytes || boundedShape(raw) != nil {
		return zero, ErrCorrupt
	}
	var env envelope
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(&env); e != nil {
		return zero, ErrCorrupt
	}
	if _, e := d.Token(); e != io.EOF || env.Schema != 1 || env.Checksum != hash(env.Payload) {
		return zero, ErrCorrupt
	}
	var p payload
	d = json.NewDecoder(bytes.NewReader(env.Payload))
	d.DisallowUnknownFields()
	if e := d.Decode(&p); e != nil {
		return zero, ErrCorrupt
	}
	if _, e := d.Token(); e != io.EOF || p.BasisID != id || p.ProposalID != proposal.ID() {
		return zero, ErrCorrupt
	}
	rebuilt, e := decision.NewAdjudicationBasis(proposal, p.Config)
	if e != nil || rebuilt.ID() != id {
		return zero, ErrCorrupt
	}
	canonical, e := encode(rebuilt)
	if e != nil || !bytes.Equal(canonical, raw) {
		return zero, ErrCorrupt
	}
	return rebuilt, nil
}

// Preflight bounded token structure BEFORE allocating outcome/witness structs.
// The public constructor subsequently enforces aggregate byte/count budgets.
func boundedShape(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var visit func(int, string) error
	values := 0
	visit = func(depth int, name string) error {
		values++
		if values > decision.MaxManifestBytes/16 {
			return ErrCorrupt
		}
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
				if len(seen) >= 32 {
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
			limit := decision.MaxBasisIdentities
			switch strings.ToLower(name) {
			case "outcomes":
				limit = decision.MaxAdjudicationObservations
			case "witnesses":
				limit = decision.MaxAdjudicationWitnesses
			case "onbehalfof":
				limit = auth.MaxOnBehalfOfEntries
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
