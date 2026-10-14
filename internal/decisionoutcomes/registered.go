// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisionoutcomes

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/coordination/allocator"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const MaxRegisteredReceiptBytes = 16 << 20

type RegisteredReference struct{ TargetID, InventoryID, ReceiptID shoal.ID }
type RegisteredBinding struct {
	Reference  RegisteredReference
	CoverageID shoal.ID
	Prediction decision.PredictionRecord
}
type RegisteredMaterial struct {
	Binding     RegisteredBinding
	Receipt     Receipt
	Observation decision.OutcomeObservation
}

// RegisteredAuthority is a mandatory trusted cross-principal read boundary.
// Resolve must authenticate exact PUBLISHED membership in the named registered
// coverage and inventory snapshot, the canonical prediction, and the actual
// caller's current cross-principal read purpose before storage access. An intent
// or caller-provided receipt reference is not evidence of membership.
// Verify must bind EVERY full original receipt to its published commitment and
// jointly check current inventory generations, original sources, outcome evidence,
// correction ancestry, and combined disclosure after finishing all its own IO.
// It receives current-to-oldest material; a failed collection may provide a
// prefix, which can never be disclosed. No default authority is supplied.
// Per-target verification does not establish an atomic multi-target cohort epoch.
type RegisteredAuthority interface {
	Resolve(context.Context, auth.Decision, RegisteredReference) (RegisteredBinding, error)
	Verify(context.Context, auth.Decision, []RegisteredMaterial) error
}
type RegisteredBackend interface {
	ReadExact(context.Context, []allocator.Coordinate) ([]allocator.Cell, error)
}
type RegisteredReaderConfig struct {
	Backend    RegisteredBackend
	Resolver   auth.Resolver
	Authority  RegisteredAuthority
	Clock      func() time.Time
	Visibility []byte
}
type RegisteredReader struct{ config RegisteredReaderConfig }

func NewRegisteredReader(c RegisteredReaderConfig) (*RegisteredReader, error) {
	if absent(c.Backend) || absent(c.Resolver) || absent(c.Authority) || c.Clock == nil || len(c.Visibility) > 4096 {
		return nil, invalid()
	}
	c.Visibility = append([]byte(nil), c.Visibility...)
	return &RegisteredReader{c}, nil
}
func registeredID(id shoal.ID) bool {
	return utf8.ValidString(string(id)) && strings.TrimSpace(string(id)) != "" && shoal.ValidateRequiredID("reference", id) == nil
}
func (r *RegisteredReader) caller(ctx context.Context, old *auth.Decision) (auth.Decision, error) {
	var zero auth.Decision
	if ctx.Err() != nil {
		return zero, auth.ObjectNotFound()
	}
	d, e := r.config.Resolver.Resolve(ctx)
	n := r.config.Clock().Round(0).UTC()
	if e != nil || n.IsZero() || n.Year() < 1 || n.Year() > 9999 || !n.Before(d.AuthenticationExpires()) {
		return zero, auth.ObjectNotFound()
	}
	f, e := auth.AuthorizationFingerprint(d)
	if e != nil {
		return zero, auth.ObjectNotFound()
	}
	if old != nil {
		before, e := auth.AuthorizationFingerprint(*old)
		if e != nil || f != before {
			return zero, auth.ObjectNotFound()
		}
	}
	if ctx.Err() != nil {
		return zero, auth.ObjectNotFound()
	}
	return d, nil
}

// This is the persisted scopeFor encoding, using stored original attribution
// and the actual caller's domain. It creates no impersonated auth.Decision.
func registeredScope(domain []byte, r Receipt) string {
	p := struct {
		Domain                 []byte
		Subject, Actor, Client []byte
		Delegates              [][]byte
	}{Domain: domain, Subject: []byte(r.SubmitterID), Actor: []byte(r.ActorID), Client: []byte(r.ClientID)}
	for _, id := range r.OnBehalfOf {
		p.Delegates = append(p.Delegates, []byte(id))
	}
	b, _ := json.Marshal(p)
	return hash(b)
}
func registeredReporterValid(r Receipt) bool {
	if shoal.ValidateRequiredID("subject", r.SubmitterID) != nil || shoal.ValidateRequiredID("actor", r.ActorID) != nil || shoal.ValidateOptionalID("client", r.ClientID) != nil || len(r.OnBehalfOf) > auth.MaxOnBehalfOfEntries {
		return false
	}
	for _, id := range r.OnBehalfOf {
		if shoal.ValidateRequiredID("delegate", id) != nil {
			return false
		}
	}
	return true
}
func detachedRegistered(m []RegisteredMaterial) []RegisteredMaterial {
	out := append([]RegisteredMaterial(nil), m...)
	for i := range out {
		if m[i].Receipt.OnBehalfOf != nil {
			out[i].Receipt.OnBehalfOf = append([]shoal.ID{}, m[i].Receipt.OnBehalfOf...)
		}
		out[i].Receipt.ObservationConfig = m[i].Observation.Config()
	}
	return out
}

// Bound container allocation before decoding the existing persisted format.
// The unchanged codec subsequently requires exact canonical byte equality.
func registeredShape(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	nodes := 0
	var walk func(int, string) bool
	walk = func(depth int, name string) bool {
		nodes++
		if nodes > 100000 || depth > 16 {
			return false
		}
		v, e := d.Token()
		if e != nil {
			return false
		}
		delim, ok := v.(json.Delim)
		if !ok {
			return true
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				v, e := d.Token()
				key, ok := v.(string)
				if e != nil || !ok || seen[key] || len(seen) >= 32 {
					return false
				}
				seen[key] = true
				if !walk(depth+1, key) {
					return false
				}
			}
			v, e = d.Token()
			return e == nil && v == json.Delim('}')
		case '[':
			limit := decision.MaxSources
			if strings.EqualFold(name, "OnBehalfOf") {
				limit = auth.MaxOnBehalfOfEntries
			}
			n := 0
			for d.More() {
				n++
				if n > limit || !walk(depth+1, "") {
					return false
				}
			}
			v, e = d.Token()
			return e == nil && v == json.Delim(']')
		}
		return false
	}
	if !walk(0, "") {
		return false
	}
	_, e := d.Token()
	return e == io.EOF
}

// ReadRegistered returns one authenticated original receipt, never a partial
// ancestry or inventory. It does not alter ordinary principal-scoped Store.Read
// and grants neither training permission nor inventory completeness.
func (r *RegisteredReader) ReadRegistered(ctx context.Context, ref RegisteredReference) (Receipt, error) {
	deny := auth.ObjectNotFound
	if r == nil || !registeredID(ref.TargetID) || !registeredID(ref.InventoryID) || !validReceiptID(ref.ReceiptID) {
		return Receipt{}, deny()
	}
	d, e := r.caller(ctx, nil)
	if e != nil {
		return Receipt{}, deny()
	}
	var material []RegisteredMaterial
	pending := func() error {
		seen := map[shoal.ID]bool{}
		current := ref
		total := 0
		for {
			if len(material) > MaxOutcomeAncestors || seen[current.ReceiptID] {
				return deny()
			}
			seen[current.ReceiptID] = true
			binding, e := r.config.Authority.Resolve(ctx, d, current)
			if e != nil || binding.Reference != current || !registeredID(binding.CoverageID) || binding.Prediction.Validate() != nil {
				return deny()
			}
			if len(material) > 0 && (binding.CoverageID != material[0].Binding.CoverageID || binding.Prediction.ID() != material[0].Binding.Prediction.ID()) {
				return deny()
			}
			coord := allocator.Coordinate{Row: []byte(current.ReceiptID), Family: []byte("o"), Qualifier: []byte("observation"), Visibility: append([]byte(nil), r.config.Visibility...)}
			cells, e := r.config.Backend.ReadExact(ctx, []allocator.Coordinate{coord})
			if e != nil || ctx.Err() != nil || len(cells) != 1 || cells[0].Timestamp != 1 || !sameCoordinate(cells[0].Coordinate, coord) || len(cells[0].Value) > maxStoredBytes || len(cells[0].Value) > MaxRegisteredReceiptBytes-total {
				return deny()
			}
			total += len(cells[0].Value)
			raw := append([]byte(nil), cells[0].Value...)
			if !registeredShape(raw) {
				return deny()
			}
			stored, e := decode(raw)
			if e != nil {
				return deny()
			}
			receipt := stored.Receipt
			// Retain one canonical prediction for the identical request, including
			// the prediction carried privately by each ancestor observation.
			p := binding.Prediction
			if len(material) > 0 {
				p = material[0].Binding.Prediction
				binding.Prediction = p
			}
			c := receipt.ObservationConfig
			n := r.config.Clock().Round(0).UTC()
			if !registeredReporterValid(receipt) || stored.ScopeDigest != registeredScope(d.AuthorizationDomain(), receipt) || !validHash(stored.KeyDigest) || receipt.ID != current.ReceiptID || receiptID(stored.ScopeDigest, c.RequestID, stored.KeyDigest) != receipt.ID || receipt.State != "proposed" || !strings.HasPrefix(receipt.AuthorizationFingerprint, "auth-sha256:") || !validHash(strings.TrimPrefix(receipt.AuthorizationFingerprint, "auth-sha256:")) || receipt.ReceivedAt.IsZero() || receipt.ReceivedAt.Year() < 1 || receipt.ReceivedAt.Year() > 9999 || receipt.ReceivedAt != receipt.ReceivedAt.Round(0).UTC() || receipt.ReceivedAt.After(n) || receipt.ReceivedAt.Before(p.Config().CompletedAt) || c.ObservedAt.After(receipt.ReceivedAt) || c.RequestID != p.Request().ID() || c.PredictionID != p.ID() || c.Kind != decision.OutcomeCorrectness {
				return deny()
			}
			o, e := decision.NewOutcomeObservation(p, c)
			if e != nil || o.ID() != receipt.ObservationID || !reflect.DeepEqual(o.Config(), c) {
				return deny()
			}
			target, e := decision.AdjudicationTargetID(p.Request().TaskID(), p.Request().PictureID(), c.SubjectID, c.QuestionID)
			if e != nil || target != ref.TargetID {
				return deny()
			}
			if len(material) > 0 {
				child := material[len(material)-1].Receipt
				if registeredScope(d.AuthorizationDomain(), child) != stored.ScopeDigest || receipt.ReceivedAt.After(child.ReceivedAt) {
					return deny()
				}
			}
			material = append(material, RegisteredMaterial{binding, receipt, o})
			if c.Supersedes == "" {
				return nil
			}
			current.ReceiptID = c.Supersedes
		}
	}()
	if len(material) == 0 {
		return Receipt{}, deny()
	}
	checked := detachedRegistered(material)
	if e = r.config.Authority.Verify(ctx, d, checked); e != nil || !reflect.DeepEqual(material, checked) {
		return Receipt{}, deny()
	}
	if _, e = r.caller(ctx, &d); e != nil || pending != nil {
		return Receipt{}, deny()
	}
	return material[0].Receipt, nil
}
