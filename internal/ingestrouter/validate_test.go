// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.

package ingestrouter

import (
	"errors"
	"math"
	"testing"
)

func TestVisibilityValidation(t *testing.T) {
	valid := []string{
		"",
		"A",
		"A&B",
		"A|(B&C)",
		`A&("team one"|B)`,
		`"quote\""`,
		`"back\\slash"`,
	}
	for _, value := range valid {
		t.Run("valid_"+value, func(t *testing.T) {
			if err := validateVisibility([]byte(value), 64); err != nil {
				t.Fatalf("validateVisibility(%q): %v", value, err)
			}
		})
	}
	invalid := []string{
		"&A",
		"A&",
		"A|B&C",
		"(A",
		"A)",
		"()",
		`""`,
		`"bad\q"`,
		`"unterminated`,
		"A B",
	}
	for _, value := range invalid {
		t.Run("invalid_"+value, func(t *testing.T) {
			if err := validateVisibility([]byte(value), 64); err == nil {
				t.Fatalf("validateVisibility(%q) succeeded", value)
			}
		})
	}
	if err := validateVisibility([]byte("((A))"), 1); err == nil {
		t.Fatal("visibility nesting limit was not enforced")
	}
}

func TestValidationLimitsAndTimestampPolicy(t *testing.T) {
	extent := Extent{TableID: "5"}
	base := Request{ID: "request", Batches: []Batch{testBatch(extent, "row")}}
	tests := []struct {
		name   string
		change func(*Limits, *Request)
	}{
		{
			name: "request bytes",
			change: func(l *Limits, _ *Request) {
				l.MaxRequestBytes = 1
			},
		},
		{
			name: "row bytes",
			change: func(l *Limits, _ *Request) {
				l.MaxRowBytes = 2
			},
		},
		{
			name: "extent bytes",
			change: func(l *Limits, r *Request) {
				l.MaxExtentRowBytes = 1
				r.Batches[0].Extent.EndRow = []byte("zz")
			},
		},
		{
			name: "column bytes",
			change: func(l *Limits, _ *Request) {
				l.MaxColumnBytes = 1
			},
		},
		{
			name: "visibility bytes",
			change: func(l *Limits, _ *Request) {
				l.MaxVisibilityBytes = 2
			},
		},
		{
			name: "value bytes",
			change: func(l *Limits, _ *Request) {
				l.MaxValueBytes = 2
			},
		},
		{
			name: "timestamp minimum",
			change: func(l *Limits, r *Request) {
				l.MinTimestamp = 18
				r.Batches[0].Mutations[0].Updates[0].Timestamp.Value = 17
			},
		},
		{
			name: "timestamp maximum",
			change: func(l *Limits, r *Request) {
				l.MaxTimestamp = 16
				r.Batches[0].Mutations[0].Updates[0].Timestamp.Value = 17
			},
		},
		{
			name: "delete value",
			change: func(_ *Limits, r *Request) {
				r.Batches[0].Mutations[0].Updates[0].Delete = true
			},
		},
		{
			name: "server timestamp carries value",
			change: func(_ *Limits, r *Request) {
				r.Batches[0].Mutations[0].Updates[0].Timestamp = Timestamp{Value: 1}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			limits := DefaultLimits()
			request := cloneTestRequest(base)
			test.change(&limits, &request)
			if _, err := validateRequest("5", limits, request); !errors.Is(err, ErrInvalidBatch) {
				t.Fatalf("validateRequest error = %v", err)
			}
		})
	}

	limits := DefaultLimits()
	limits.MinTimestamp = math.MinInt64
	limits.MaxTimestamp = math.MaxInt64
	request := cloneTestRequest(base)
	request.Batches[0].Mutations[0].Updates[0].Timestamp = Timestamp{Set: false}
	if _, err := validateRequest("5", limits, request); err != nil {
		t.Fatalf("server-assigned timestamp: %v", err)
	}
}

func TestValidatedRequestOwnsInputBytes(t *testing.T) {
	extent := Extent{TableID: "5"}
	request := Request{ID: "copy", Batches: []Batch{testBatch(extent, "row")}}
	validated, err := validateRequest("5", DefaultLimits(), request)
	if err != nil {
		t.Fatalf("validateRequest: %v", err)
	}
	request.Batches[0].Mutations[0].Row[0] = 'X'
	request.Batches[0].Mutations[0].Updates[0].Value[0] = 'X'
	got := validated.request.Batches[0].Mutations[0]
	if string(got.Row) != "row" || string(got.Updates[0].Value) != "value" {
		t.Fatalf("validated bytes changed through caller aliases: row=%q value=%q",
			got.Row, got.Updates[0].Value)
	}
}

func cloneTestRequest(in Request) Request {
	out := Request{ID: in.ID, Batches: make([]Batch, len(in.Batches))}
	for i, batch := range in.Batches {
		out.Batches[i].Extent = batch.Extent.clone()
		out.Batches[i].Mutations = cloneMutations(batch.Mutations)
	}
	return out
}
