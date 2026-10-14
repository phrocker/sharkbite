// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package routerwire is the one adapter between the language router and the
// services it reads (#500). It is the router's reviewed authority surface.
//
// Each constructor takes a service and returns an unexported wrapper that
// holds the service in an unexported field and has only the methods of one
// router port (internal/routershadow ports.go, or router.InputValidator).
// Every one of those methods is a read, a pure check, or the in-process
// prediction. The router, which imports no service package, can hold only
// these wrappers, so asserting one to any other interface finds nothing. A
// test pins each wrapper's method set.
package routerwire

import (
	"context"
	"encoding/json"
	"time"

	"github.com/phrocker/shoal-oss/internal/decisionlinear"
	"github.com/phrocker/shoal-oss/internal/routershadow"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/extraction"
	"github.com/phrocker/shoal-oss/pkg/lexicon"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/router"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// ReasonCode is the caller-asserted reason the router's fleet reads record.
const ReasonCode = "router_shadow"

// --- Caller: resolves the caller's decision and returns it as data.

type callerWire struct{ resolver auth.Resolver }

// Caller wraps the server's resolver.
func Caller(resolver auth.Resolver) routershadow.CallerResolver { return callerWire{resolver} }

func (w callerWire) RouterCaller(ctx context.Context) (routershadow.Caller, error) {
	d, err := w.resolver.Resolve(ctx)
	if err != nil {
		return routershadow.Caller{}, err
	}
	fingerprint, err := auth.AuthorizationFingerprint(d)
	if err != nil {
		return routershadow.Caller{}, err
	}
	return routershadow.Caller{
		Principal: d.Subject(), RequestID: d.RequestID(), CorrelationID: d.CorrelationID(),
		Fingerprint: fingerprint, ExpiresAt: d.AuthenticationExpires(),
	}, nil
}

// --- Targets: one fleet List page, as data.

type targetWire struct {
	registry *fleet.Service
	resolver auth.Resolver
	clock    func() time.Time
}

// Targets wraps the fleet registry's List.
func Targets(registry *fleet.Service, resolver auth.Resolver, clock func() time.Time) routershadow.TargetLister {
	return targetWire{registry, resolver, clock}
}

func (w targetWire) RouterDescriptorPage(ctx context.Context, cursor []byte) ([]routershadow.Descriptor, []byte, error) {
	d, err := w.resolver.Resolve(ctx)
	if err != nil {
		return nil, nil, err
	}
	page, err := w.registry.List(ctx, fleet.ListRequest{
		Context: fleet.RequestContext{
			RequestID: d.RequestID(), CorrelationID: d.CorrelationID(),
			ReasonCode: ReasonCode, Deadline: w.clock().UTC().Add(30 * time.Second),
		},
		Cursor: cursor, Limit: fleet.MaxListResults,
	})
	if err != nil {
		return nil, nil, err
	}
	out := make([]routershadow.Descriptor, len(page.Descriptors))
	for i, desc := range page.Descriptors {
		out[i] = routershadow.Descriptor{ID: desc.ID, Generation: desc.Generation}
		for _, c := range desc.Capabilities {
			capability := routershadow.Capability{Name: c.Name}
			for _, a := range c.Actions {
				capability.Actions = append(capability.Actions, router.ActionSpec{
					Name: a.Name, InputSchema: append(json.RawMessage(nil), a.InputSchema...), RequiresApproval: a.RequiresApproval,
				})
			}
			out[i].Capabilities = append(out[i].Capabilities, capability)
		}
	}
	return out, page.Next, nil
}

// --- Decisions: provisioned profiles the caller may invoke.

// DecisionProfile is a provisioned decision target and the task resource
// whose invoke authorization makes it visible, as decision registration
// checks it.
type DecisionProfile struct {
	Target       routershadow.DecisionTarget
	TaskResource auth.ResourceRequest
}

type decisionWire struct {
	resolver auth.Resolver
	clock    func() time.Time
	profiles []DecisionProfile
}

// Decisions wraps the profile visibility check.
func Decisions(resolver auth.Resolver, clock func() time.Time, profiles []DecisionProfile) routershadow.DecisionGate {
	return decisionWire{resolver, clock, append([]DecisionProfile(nil), profiles...)}
}

func (w decisionWire) RouterVisibleDecisions(ctx context.Context) ([]routershadow.DecisionTarget, error) {
	d, err := w.resolver.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	now := w.clock().UTC()
	var out []routershadow.DecisionTarget
	for _, p := range w.profiles {
		err := d.AuthorizeObject(auth.OperationInvoke, p.TaskResource, now)
		if invisible(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, p.Target)
	}
	return out, nil
}

// invisible: the authorization layer answers a hidden and an absent object
// with the same shape, and the router treats both as absent.
func invisible(err error) bool {
	return shoal.IsErrorCode(err, shoal.ErrorNotFound) || shoal.IsErrorCode(err, shoal.ErrorUnauthorized)
}

// --- Lookups: whether the bundle's published ontology is visible.

// OntologyBinding names the published ontology the lexicon bundle's lookup
// templates were derived from. The bundle does not record it, so the
// operator supplies the published version; Lookups refuses a binding whose
// version does not have Identity or does not derive exactly the bundle's
// templates.
type OntologyBinding struct {
	Configured ontology.OntologyVersion
	Identity   ontology.OntologyIdentity
	Published  ontology.OntologyVersion
}

type lookupWire struct {
	client  *authorized.Client
	binding OntologyBinding
}

// Lookups wraps the published-ontology visibility check.
func Lookups(client *authorized.Client, binding OntologyBinding, bundle *lexicon.Bundle) (routershadow.LookupGate, error) {
	refuse := shoal.NewError(shoal.ErrorInvalidArgument, "router ontology binding does not match the lexicon bundle's templates")
	identity, err := ontology.NewOntologyIdentity(binding.Published)
	if err != nil || identity != binding.Identity {
		return nil, refuse
	}
	derived, err := lexicon.DeriveTemplates(binding.Published.Relationships())
	if err != nil || !sameTemplates(derived, bundle.Templates()) {
		return nil, refuse
	}
	return lookupWire{client, binding}, nil
}

func (w lookupWire) RouterLookupsVisible(ctx context.Context) (bool, error) {
	err := w.client.AuthorizePublishedOntology(ctx, w.binding.Configured, w.binding.Identity, auth.OperationNeighborhood)
	if invisible(err) {
		return false, nil
	}
	return err == nil, err
}

func sameTemplates(a, b []lexicon.Template) bool {
	if len(a) != len(b) {
		return false
	}
	same := func(x, y []shoal.ID) bool {
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i] != y[i] {
				return false
			}
		}
		return true
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].RelationKey != b[i].RelationKey || a[i].Direction != b[i].Direction ||
			a[i].PhraseKey != b[i].PhraseKey || !same(a[i].SubjectConcepts, b[i].SubjectConcepts) ||
			!same(a[i].AnswerConcepts, b[i].AnswerConcepts) {
			return false
		}
	}
	return true
}

// --- Mentions and concepts: authorized reads.

type mentionWire struct{ client *authorized.Client }

// Mentions wraps ResolveMentions.
func Mentions(client *authorized.Client) routershadow.MentionResolver { return mentionWire{client} }

func (w mentionWire) RouterMentions(ctx context.Context, bundle *lexicon.Bundle, text string) ([]lexicon.Mention, error) {
	return w.client.ResolveMentions(ctx, bundle, text)
}

type conceptWire struct{ client *authorized.Client }

// Concepts wraps Neighborhood, returning only each node's ontology concept.
func Concepts(client *authorized.Client) routershadow.ConceptReader { return conceptWire{client} }

func (w conceptWire) RouterNodeConcepts(ctx context.Context, ids []shoal.ID) (map[shoal.ID]shoal.ID, error) {
	neighborhood, err := w.client.Neighborhood(ctx, explorer.NeighborhoodRequest{NodeIDs: ids})
	if err != nil {
		return nil, err
	}
	wanted := map[shoal.ID]bool{}
	for _, id := range ids {
		wanted[id] = true
	}
	out := map[shoal.ID]shoal.ID{}
	for _, n := range neighborhood.Nodes {
		if concept := n.Properties[extraction.GraphPropertyOntologyConceptID]; wanted[n.ID] && concept != "" {
			out[n.ID] = shoal.ID(concept)
		}
	}
	return out, nil
}

// --- Validator: Enqueue's own schema check.

type validatorWire struct{}

// Validator wraps fleet.ValidateActionInput.
func Validator() router.InputValidator { return validatorWire{} }

func (validatorWire) ValidateInput(schema, input json.RawMessage) (json.RawMessage, error) {
	return fleet.ValidateActionInput(fleet.Action{InputSchema: schema}, input)
}

// --- Predictor: the in-process linear model.

type predictorWire struct {
	provider *decisionlinear.Provider
	release  shoal.ID
}

// Predictor wraps a decisionlinear provider for one release.
func Predictor(provider *decisionlinear.Provider, release shoal.ID) routershadow.Predictor {
	return predictorWire{provider, release}
}

func (w predictorWire) RouterPredictorIdentity() decision.PredictorIdentity {
	return w.provider.Identity()
}

func (w predictorWire) RouterReleaseID() shoal.ID { return w.release }

func (w predictorWire) RouterPredict(ctx context.Context, request decision.DecisionRequest, input []byte) (decision.ResultConfig, error) {
	predictor, err := w.provider.Resolve(ctx, w.release, request.PredictorID())
	if err != nil {
		return decision.ResultConfig{}, err
	}
	return predictor.Predict(ctx, request, input)
}
