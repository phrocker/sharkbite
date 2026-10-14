// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package routerwire

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/routershadow"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/router"
)

// wrapperMethods is the complete method set of every wrapper the router can
// hold. Adding a method to a wrapper fails this test until it is added here,
// in review.
var wrapperMethods = map[string][]string{
	"callerWire":    {"RouterCaller"},
	"targetWire":    {"RouterDescriptorPage"},
	"decisionWire":  {"RouterVisibleDecisions"},
	"lookupWire":    {"RouterLookupsVisible"},
	"mentionWire":   {"RouterMentions"},
	"conceptWire":   {"RouterNodeConcepts"},
	"validatorWire": {"ValidateInput"},
	"predictorWire": {"RouterPredict", "RouterPredictorIdentity", "RouterReleaseID"},
}

func methodNames(t reflect.Type) []string {
	var names []string
	for i := 0; i < t.NumMethod(); i++ {
		names = append(names, t.Method(i).Name)
	}
	sort.Strings(names)
	return names
}

// wrappers returns one of every wrapper, as the router receives it.
func wrappers(t *testing.T) []any {
	w := newWorld(t, authorized.NewMemoryPolicyStore(), false)
	c := w.config
	return []any{c.Caller, c.Targets, c.Decisions, c.Lookups, c.Mentions, c.Concepts, c.Validator, c.Decider.Predictor}
}

// TestWrapperMethodSetsAreExact: each wrapper's method set, by value and by
// pointer, is exactly its port's, and its fields are unexported.
func TestWrapperMethodSetsAreExact(t *testing.T) {
	seen := map[string]bool{}
	for _, v := range wrappers(t) {
		typ := reflect.TypeOf(v)
		name := typ.Name()
		want, ok := wrapperMethods[name]
		if !ok || typ.PkgPath() != "github.com/phrocker/shoal-oss/internal/routerwire" {
			t.Fatalf("the router holds %v, which is not a routerwire wrapper", typ)
		}
		seen[name] = true
		for _, tt := range []reflect.Type{typ, reflect.PointerTo(typ)} {
			if got := methodNames(tt); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("%v methods = %v, want %v", tt, got, want)
			}
		}
		for i := 0; i < typ.NumField(); i++ {
			if typ.Field(i).IsExported() {
				t.Errorf("%s exports field %s", name, typ.Field(i).Name)
			}
		}
	}
	if len(seen) != len(wrapperMethods) {
		t.Fatalf("wrappers checked %v, listed %v", seen, wrapperMethods)
	}
}

// TestRoundTwoAssertionsFindNothing: the round-2 bypass asserted a held value
// to an interface with a write method. What the router holds now has none.
func TestRoundTwoAssertionsFindNothing(t *testing.T) {
	type connector interface {
		Connect(context.Context, graph.Edge) error
	}
	type registrar interface {
		Register(context.Context, fleet.RegisterRequest) (fleet.Descriptor, error)
	}
	type enqueuer interface {
		Enqueue(context.Context, fleet.EnqueueRequest) (fleet.ActionRecord, error)
	}
	for _, v := range wrappers(t) {
		if _, ok := v.(connector); ok {
			t.Errorf("%T has Connect", v)
		}
		if _, ok := v.(registrar); ok {
			t.Errorf("%T has Register", v)
		}
		if _, ok := v.(enqueuer); ok {
			t.Errorf("%T has Enqueue", v)
		}
	}
}

// TestServicesCannotBeRouterPorts: no port can be satisfied by a raw service,
// so the raw service cannot be passed in where a port is expected.
func TestServicesCannotBeRouterPorts(t *testing.T) {
	ports := []reflect.Type{
		reflect.TypeOf((*routershadow.CallerResolver)(nil)).Elem(),
		reflect.TypeOf((*routershadow.TargetLister)(nil)).Elem(),
		reflect.TypeOf((*routershadow.DecisionGate)(nil)).Elem(),
		reflect.TypeOf((*routershadow.LookupGate)(nil)).Elem(),
		reflect.TypeOf((*routershadow.MentionResolver)(nil)).Elem(),
		reflect.TypeOf((*routershadow.ConceptReader)(nil)).Elem(),
		reflect.TypeOf((*routershadow.Predictor)(nil)).Elem(),
		reflect.TypeOf((*router.InputValidator)(nil)).Elem(),
	}
	services := []reflect.Type{
		reflect.TypeOf((*fleet.Service)(nil)), reflect.TypeOf((*fleet.DispatchService)(nil)),
		reflect.TypeOf((*authorized.Client)(nil)),
	}
	for _, port := range ports {
		for _, svc := range services {
			if svc.Implements(port) {
				t.Errorf("%v satisfies %v", svc, port)
			}
		}
	}
}

// TestRouterBoundsMatchTheResolver: the router's text bounds are the
// authorized resolver's.
func TestRouterBoundsMatchTheResolver(t *testing.T) {
	if router.MaxTextBytes != authorized.MaxMentionBytes || router.MaxTokens != authorized.MaxMentionTokens {
		t.Fatal("router text bounds differ from the mention resolver's")
	}
}
