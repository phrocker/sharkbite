// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisiondatasets

import (
	"reflect"
	"time"

	journal "github.com/phrocker/shoal-oss/internal/decisionadjudicationstore"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// MaxHydratedBytes limits retained input, including the private immutable
// prediction/picture/context pack and basis payloads, before another load begins.
// Accounting conservatively includes container headers and backing storage.
const MaxHydratedBytes = 32 << 20

func cloneMember(m Member) Member {
	m.FamilyIDs = append([]shoal.ID(nil), m.FamilyIDs...)
	if m.InclusionProbability != nil {
		p := *m.InclusionProbability
		m.InclusionProbability = &p
	}
	return m
}
func cloneCohort(c Cohort) Cohort {
	if c.Members != nil {
		members := make([]Member, len(c.Members))
		for i, m := range c.Members {
			members[i] = cloneMember(m)
		}
		c.Members = members
	}
	return c
}
func cloneTarget(t Target) Target {
	if t.Features != nil {
		t.Features = append([]float64{}, t.Features...)
	}
	if t.OutcomeReceiptIDs != nil {
		t.OutcomeReceiptIDs = append([]shoal.ID{}, t.OutcomeReceiptIDs...)
	}
	if t.History != nil {
		rows := make([]journal.Receipt, len(t.History))
		copy(rows, t.History)
		for i := range rows {
			c := &rows[i].ProposalConfig
			if c.ObservationReceiptIDs != nil {
				c.ObservationReceiptIDs = append([]shoal.ID{}, c.ObservationReceiptIDs...)
			}
			if c.WitnessIDs != nil {
				c.WitnessIDs = append([]shoal.ID{}, c.WitnessIDs...)
			}
			if c.Truth != nil {
				v := *c.Truth
				c.Truth = &v
			}
			if rows[i].Adjudicator.OnBehalfOf != nil {
				rows[i].Adjudicator.OnBehalfOf = append([]shoal.ID{}, rows[i].Adjudicator.OnBehalfOf...)
			}
		}
		t.History = rows
	}
	return t
}
func cloneTargets(t []Target) []Target {
	out := make([]Target, len(t))
	for i := range t {
		out[i] = cloneTarget(t[i])
	}
	return out
}

// Walk reflection without Interface or serialization, including private fields.
// This accounts for immutable objects without first allocating their Config
// copies. time.Time's internal location cache is process state, not payload.
func hydratedSize(value any, limit int) (int, bool) {
	left, nodes := limit, 1000000
	var walk func(reflect.Value, int) bool
	walk = func(v reflect.Value, depth int) bool {
		if !v.IsValid() {
			return true
		}
		nodes--
		if nodes < 0 || depth > 64 {
			return false
		}
		charge := func(n int) bool {
			if n < 0 || n > left {
				return false
			}
			left -= n
			return true
		}
		if !charge(int(v.Type().Size())) {
			return false
		}
		if v.Type() == reflect.TypeOf(time.Time{}) {
			return true
		}
		switch v.Kind() {
		case reflect.String:
			return charge(v.Len())
		case reflect.Interface, reflect.Pointer:
			if !v.IsNil() {
				return walk(v.Elem(), depth+1)
			}
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				if !walk(v.Field(i), depth+1) {
					return false
				}
			}
		case reflect.Array, reflect.Slice:
			if v.Len() > 1000000 {
				return false
			}
			k := v.Type().Elem().Kind()
			if k == reflect.Uint8 || k == reflect.Float64 || k == reflect.Int || k == reflect.Bool {
				return v.Len() <= left/int(v.Type().Elem().Size()) && charge(v.Len()*int(v.Type().Elem().Size()))
			}
			for i := 0; i < v.Len(); i++ {
				if !walk(v.Index(i), depth+1) {
					return false
				}
			}
		case reflect.Map:
			if v.Len() > 4096 {
				return false
			}
			it := v.MapRange()
			for it.Next() {
				if !walk(it.Key(), depth+1) || !walk(it.Value(), depth+1) {
					return false
				}
			}
		}
		return true
	}
	ok := walk(reflect.ValueOf(value), 0)
	return limit - left, ok
}
func preflightTarget(t Target, remaining int) (int, bool) {
	if len(t.Features) == 0 || len(t.Features) > 65536 || len(t.History) > 128 || len(t.OutcomeReceiptIDs) > 256 {
		return 0, false
	}
	for _, r := range t.History {
		c := r.ProposalConfig
		if len(c.ObservationReceiptIDs) > 256 || len(c.WitnessIDs) > 1024 || len(r.Adjudicator.OnBehalfOf) > 64 || len(c.Label) > shoal.MaxSemanticStringBytes || len(c.Reason) > shoal.MaxSemanticStringBytes {
			return 0, false
		}
		for _, ids := range [][]shoal.ID{c.ObservationReceiptIDs, c.WitnessIDs, r.Adjudicator.OnBehalfOf} {
			for _, id := range ids {
				if len(id) > shoal.MaxIDBytes {
					return 0, false
				}
			}
		}
	}
	return hydratedSize(t, remaining)
}
