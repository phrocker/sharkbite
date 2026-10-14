// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package routershadow runs the language router in shadow mode (#500): it
// routes a caller's text to a proposal under the caller's current
// authorization, runs the lexical baseline beside it, and records both.
//
// Nothing it produces is executed, enqueued, invoked, evaluated or
// registered, and nothing it holds can do so: it imports no package that
// holds a service, and reaches Shoal only through the ports in ports.go,
// which internal/routerwire implements with wrappers that have no other
// methods. See authority_test.go and docs/local-language.md.
package routershadow

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/phrocker/shoal-oss/pkg/lexicon"
	"github.com/phrocker/shoal-oss/pkg/router"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// EnumerationTimeout bounds, in wall time, the fleet listing one routing
// performs. Only what the caller can see is bounded by count (router.MaxTargets
// targets and as many descriptors): fleet List scans a bounded number of
// stored entries per call and may return an empty or short page with a
// continuation when the entries it scanned were hidden, so counting pages
// would let hidden descriptors decide the outcome. Pages are therefore read
// until the listing ends, limited only by this timeout and the caller's
// context. How long that takes, and how many store reads it makes, still
// grows with hidden entries; that residual is documented in
// docs/local-language.md.
const EnumerationTimeout = 10 * time.Second

var errEnumeration = shoal.NewError(shoal.ErrorUnavailable, "router target enumeration did not finish")

// DecisionTarget is a host-provisioned decision profile the router may
// propose, as data. The adapter returns only those the caller may invoke.
type DecisionTarget struct {
	ProfileID         shoal.ID
	ProfileRevisionID shoal.ID
	TaskID            shoal.ID
	Name              string
	// SlotSchema is the router-side input schema, in the fleet subset.
	SlotSchema json.RawMessage
}

// MinHostKeyBytes and minHostKeyDistinct bound the utterance key. A key of
// fewer distinct byte values than minHostKeyDistinct (an all-zero or
// repeated-byte key, say) is refused as not random; a random 32-byte key has
// about 30 distinct values, and fewer than 16 has negligible probability.
const (
	MinHostKeyBytes    = 32
	minHostKeyDistinct = 16
)

func hostKeyValid(key []byte) bool {
	if len(key) < MinHostKeyBytes {
		return false
	}
	distinct := map[byte]bool{}
	for _, b := range key {
		distinct[b] = true
	}
	return len(distinct) >= minHostKeyDistinct
}

// Config wires the shadow router to its ports. Lookups is optional: without
// it no lookup template is offered.
type Config struct {
	Caller    CallerResolver
	Targets   TargetLister
	Decisions DecisionGate
	Lookups   LookupGate
	Mentions  MentionResolver
	Concepts  ConceptReader
	Validator router.InputValidator
	Lexicon   *lexicon.Bundle
	Grammars  *router.GrammarSet
	Decider   *Decider
	Recorder  Recorder
	// HostKey keys the utterance HMAC: at least 32 random bytes.
	HostKey []byte
	Clock   func() time.Time
}

// Service is the shadow router.
type Service struct {
	config Config
}

// New validates the configuration.
func New(config Config) (*Service, error) {
	if config.Caller == nil || config.Targets == nil || config.Decisions == nil || config.Mentions == nil ||
		config.Concepts == nil || config.Validator == nil || config.Lexicon == nil ||
		config.Decider == nil || config.Decider.Predictor == nil || config.Recorder == nil || config.Clock == nil {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "router shadow dependencies are required")
	}
	if !hostKeyValid(config.HostKey) {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "router shadow host key is too short or not random")
	}
	config.HostKey = append([]byte(nil), config.HostKey...)
	return &Service{config: config}, nil
}

// Route routes text for the caller bound to ctx, records the result, and
// returns the router's proposal. It never acts on it.
func (s *Service) Route(ctx context.Context, text string) (router.Proposal, error) {
	start := s.config.Clock()
	var latency StageLatency
	stage := func(into *int64, from time.Time) time.Time {
		now := s.config.Clock()
		*into = now.Sub(from).Nanoseconds()
		return now
	}
	caller, err := s.config.Caller.RouterCaller(ctx)
	if err != nil {
		return router.Proposal{}, err
	}
	targets, err := s.visibleTargets(ctx)
	if err != nil {
		return router.Proposal{}, err
	}
	catalog, err := router.NewCatalog(targets, s.config.Grammars, s.config.Validator)
	if err != nil {
		return router.Proposal{}, err
	}
	t := stage(&latency.EnumerateNS, start)

	tokens := lexicon.Tokenize(text)
	input := router.Input{Tokens: tokens, Catalog: catalog, NodeConcepts: map[shoal.ID]shoal.ID{}}
	if len(text) > router.MaxTextBytes || len(tokens) > router.MaxTokens {
		input.OutOfBounds = true
	} else if len(tokens) > 0 {
		input.Mentions, err = s.config.Mentions.RouterMentions(ctx, s.config.Lexicon, text)
		if err != nil {
			return router.Proposal{}, err
		}
		if err := s.concepts(ctx, catalog, input.Mentions, input.NodeConcepts); err != nil {
			return router.Proposal{}, err
		}
	}
	t = stage(&latency.ResolveNS, t)

	analysis, err := router.Analyze(input)
	if err != nil {
		return router.Proposal{}, err
	}
	t = stage(&latency.AnalyzeNS, t)
	correlation := caller.CorrelationID
	if correlation == "" {
		correlation = caller.RequestID
	}
	decided, err := s.config.Decider.Decide(ctx, DecideInput{
		Analysis: analysis, PrincipalID: caller.Principal, CorrelationID: correlation,
		AuthFingerprint: hex.EncodeToString(caller.Fingerprint[:]), AuthExpiresAt: caller.ExpiresAt,
	})
	if err != nil {
		return router.Proposal{}, err
	}
	t = stage(&latency.DecideNS, t)
	baseline, err := analysis.Baseline()
	if err != nil {
		return router.Proposal{}, err
	}
	stage(&latency.BaselineNS, t)
	latency.TotalNS = s.config.Clock().Sub(start).Nanoseconds()

	record := Record{
		Version:         RecordVersion,
		RecordedAt:      s.config.Clock().UTC(),
		Principal:       caller.Principal,
		AuthFingerprint: hex.EncodeToString(caller.Fingerprint[:]),
		UtteranceKey:    UtteranceKey(s.config.HostKey, caller.Principal, caller.Fingerprint, tokens),
		TokenCount:      len(tokens),
		LexiconBundleID: s.config.Lexicon.ID().String(),
		Proposal:        decided.Proposal,
		Baseline:        baseline,
		Agree:           sameOutcome(decided.Proposal, baseline),
		Latency:         latency,
	}
	for _, m := range input.Mentions {
		record.Mentions = append(record.Mentions, MentionRecord{
			TokenSpan: m.TokenSpan, NodeIDs: append([]shoal.ID(nil), m.NodeIDs...), Ambiguous: m.Ambiguous,
		})
	}
	if err := s.config.Recorder.Record(ctx, record); err != nil {
		return router.Proposal{}, err
	}
	return decided.Proposal, nil
}

// visibleTargets enumerates what the caller can currently see: actions on
// every descriptor the registry lists for it, decision profiles it may
// invoke, and lookup templates when it may see the bundle's published
// ontology. A target it cannot see is absent exactly as one that does not
// exist. More than router.MaxTargets fails closed.
func (s *Service) visibleTargets(ctx context.Context) ([]router.Target, error) {
	var targets []router.Target
	add := func(t router.Target) error {
		if len(targets) == router.MaxTargets {
			return router.ErrTooManyTargets
		}
		targets = append(targets, t)
		return nil
	}
	listCtx, cancel := context.WithTimeout(ctx, EnumerationTimeout)
	defer cancel()
	var cursor []byte
	descriptors := 0
	for {
		if listCtx.Err() != nil {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, errEnumeration
		}
		listed, next, err := s.config.Targets.RouterDescriptorPage(listCtx, cursor)
		if err != nil {
			if listCtx.Err() != nil && ctx.Err() == nil {
				return nil, errEnumeration
			}
			return nil, err
		}
		descriptors += len(listed)
		if descriptors > router.MaxTargets {
			return nil, router.ErrTooManyTargets
		}
		for _, d := range listed {
			for _, c := range d.Capabilities {
				for _, a := range c.Actions {
					action := a
					if err := add(router.Target{
						Ref: router.TargetRef{Kind: router.KindAction, Action: &router.ActionRef{
							AgentID: d.ID, AgentGeneration: d.Generation, Capability: c.Name,
							Action: a.Name, RequiresApproval: a.RequiresApproval,
						}},
						Action: &action, Name: a.Name,
					}); err != nil {
						return nil, err
					}
				}
			}
		}
		if len(next) == 0 {
			break
		}
		if bytes.Compare(next, cursor) <= 0 {
			// A continuation that does not advance would never end.
			return nil, errEnumeration
		}
		cursor = next
	}
	decisions, err := s.config.Decisions.RouterVisibleDecisions(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range decisions {
		if err := add(router.Target{
			Ref: router.TargetRef{Kind: router.KindDecision, Decision: &router.DecisionRef{
				ProfileID: d.ProfileID, ProfileRevisionID: d.ProfileRevisionID, TaskID: d.TaskID,
			}},
			SlotSchema: d.SlotSchema, Name: d.Name,
		}); err != nil {
			return nil, err
		}
	}
	if s.config.Lookups != nil {
		visible, err := s.config.Lookups.RouterLookupsVisible(ctx)
		if err != nil {
			return nil, err
		}
		if visible {
			for _, t := range s.config.Lexicon.Templates() {
				if err := add(router.Target{
					Ref: router.TargetRef{Kind: router.KindLookup, Lookup: &router.LookupRef{
						TemplateID: t.ID, RelationKey: t.RelationKey, Direction: t.Direction.String(),
					}},
					SubjectConcepts: t.SubjectConcepts, Name: t.RelationKey,
				}); err != nil {
					return nil, err
				}
			}
		}
	}
	return targets, nil
}

// concepts reads the ontology concept of each unambiguous mentioned node,
// when a lookup is a candidate. Every mentioned node is one the caller may
// see, so the read depends only on visible nodes.
func (s *Service) concepts(ctx context.Context, catalog *router.Catalog, mentions []lexicon.Mention, into map[shoal.ID]shoal.ID) error {
	lookup := false
	for _, key := range catalog.Keys() {
		lookup = lookup || len(key) > 7 && key[:7] == "lookup:"
	}
	var ids []shoal.ID
	for _, m := range mentions {
		if !m.Ambiguous {
			ids = append(ids, m.NodeIDs[0])
		}
	}
	if !lookup || len(ids) == 0 {
		return nil
	}
	concepts, err := s.config.Concepts.RouterNodeConcepts(ctx, ids)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if concept, ok := concepts[id]; ok {
			into[id] = concept
		}
	}
	return nil
}

func sameOutcome(a, b router.Proposal) bool {
	if a.Kind != b.Kind || string(a.Input) != string(b.Input) {
		return false
	}
	if (a.Target == nil) != (b.Target == nil) {
		return false
	}
	if a.Target != nil && a.Target.Key() != b.Target.Key() {
		return false
	}
	if len(a.Reasons) != len(b.Reasons) {
		return false
	}
	for i := range a.Reasons {
		if a.Reasons[i] != b.Reasons[i] {
			return false
		}
	}
	return true
}
