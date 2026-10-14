// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package routershadow

import (
	"context"
	"time"

	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/lexicon"
	"github.com/phrocker/shoal-oss/pkg/router"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// The shadow router reaches the rest of Shoal only through the narrow ports
// below. It imports no package that holds a service: internal/routerwire is
// the one adapter that holds the authorized client, the fleet registry, the
// caller's authorization and the decision provider, and gives the router
// wrappers whose method sets are exactly these ports. A value the router
// holds therefore has no method that could act, whatever interface it is
// asserted to. A test enforces both halves: the import rule here
// (authority_test.go) and the wrapper method sets there.
//
// Method names are chosen so that no service type satisfies a port by
// accident: a raw *authorized.Client or *fleet.Service cannot be passed where
// a port is expected.

// Caller is the caller's identity and authorization pins, as data.
type Caller struct {
	Principal     shoal.ID
	RequestID     shoal.ID
	CorrelationID shoal.ID
	Fingerprint   [32]byte
	ExpiresAt     time.Time
}

// CallerResolver resolves the caller bound to a context.
type CallerResolver interface {
	RouterCaller(context.Context) (Caller, error)
}

// Capability and Descriptor are a listed descriptor's data.
type Capability struct {
	Name    string
	Actions []router.ActionSpec
}

type Descriptor struct {
	ID           shoal.ID
	Generation   int64
	Capabilities []Capability
}

// TargetLister reads one page of the descriptors the caller may resolve. An
// empty or short page with a continuation is normal: the registry skips what
// the caller cannot see.
type TargetLister interface {
	RouterDescriptorPage(ctx context.Context, cursor []byte) ([]Descriptor, []byte, error)
}

// DecisionGate returns the provisioned decision targets the caller may
// invoke; a target it may not is absent.
type DecisionGate interface {
	RouterVisibleDecisions(context.Context) ([]DecisionTarget, error)
}

// LookupGate reports whether the caller may see the published ontology the
// lexicon's lookup templates were derived from.
type LookupGate interface {
	RouterLookupsVisible(context.Context) (bool, error)
}

// MentionResolver links mentions in text to nodes the caller may see.
type MentionResolver interface {
	RouterMentions(ctx context.Context, bundle *lexicon.Bundle, text string) ([]lexicon.Mention, error)
}

// ConceptReader reads the ontology concept of visible nodes.
type ConceptReader interface {
	RouterNodeConcepts(ctx context.Context, ids []shoal.ID) (map[shoal.ID]shoal.ID, error)
}

// Predictor serves the target-choice decision: a pinned identity and release,
// and a prediction over a validated request and its input bytes.
type Predictor interface {
	RouterPredictorIdentity() decision.PredictorIdentity
	RouterReleaseID() shoal.ID
	RouterPredict(ctx context.Context, request decision.DecisionRequest, input []byte) (decision.ResultConfig, error)
}
