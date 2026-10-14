// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package narrate

import (
	"fmt"

	"github.com/phrocker/shoal-oss/pkg/router"
)

// slotReasons are the abstentions the router gives after it selected a
// target, so the sentence names that target and the slots behind it.
var slotReasons = map[router.Reason]bool{
	router.ReasonAmbiguousMention: true,
	router.ReasonMissingSlot:      true,
	router.ReasonSlotMismatch:     true,
	router.ReasonInvalidInput:     true,
}

// Proposal narrates a router proposal (pkg/router). Every sentence says it is
// a proposal or an abstention and that nothing ran: a proposal carries no
// authority, so an action still goes through admission and, where declared,
// approval; a decision is still computed by its own predictor over its own
// evidence; a lookup is still answered by its template over the graph.
//
// A proposal holds no text from the input, only registry and grammar
// identifiers (agent, capability, action, profile, template, node IDs, enum
// values), and these are rendered as identifiers. The receipt is carried in
// the sentences' references. A proposal that does not validate is refused:
// it can only come from a router out of step with this package.
func (r *Renderer) Proposal(p router.Proposal, opts Options) ([]Sentence, error) {
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("narrate: invalid router proposal: %w", err)
	}
	b := r.begin("proposal:"+opaqueID([]byte(p.ID)), opts)
	refs := proposalRefs(p)
	args := Args{}
	if p.Target != nil {
		target, targetRefs := b.routerTarget(*p.Target)
		args["target"] = target
		refs = append(refs, targetRefs...)
	}
	if p.Kind == router.KindAbstain {
		reason := p.Reasons[0]
		if slotReasons[reason] && p.Target == nil {
			return nil, fmt.Errorf("narrate: router abstention %q names no target", reason)
		}
		missing := make([]Fragment, len(p.Missing))
		for i, name := range p.Missing {
			missing[i] = b.ident(name)
		}
		args["missing"] = b.bounded(missing)
		args["n"] = len(p.Missing)
		for _, reason := range p.Reasons {
			b.add(RoleOutcome, "router.abstain."+string(reason), args, refs...)
		}
		b.add(RoleNext, "router.next.abstain", args)
		return b.finish()
	}
	slots := make([]Fragment, len(p.Slots))
	for i, s := range p.Slots {
		value := s.Enum
		if value == "" {
			value = string(s.NodeIDs[0])
			refs = append(refs, Ref{Kind: "node", ID: value})
		}
		slots[i] = b.frag("router.slot", Args{"name": b.ident(s.Name), "value": b.ident(value)})
	}
	args["slots"] = b.bounded(slots)
	approval := false
	if p.Target.Action != nil {
		approval = p.Target.Action.RequiresApproval
	}
	args["approval"] = yesNo(approval)
	b.add(RoleOutcome, "router.proposal."+string(p.Kind), args, refs...)
	b.add(RoleNext, "router.next."+string(p.Kind), args)
	return b.finish()
}

func (b *builder) routerTarget(t router.TargetRef) (Fragment, []Ref) {
	switch {
	case t.Action != nil:
		return b.frag("router.target.action", Args{
				"agent": b.ident(string(t.Action.AgentID)), "capability": b.ident(t.Action.Capability),
				"action": b.ident(t.Action.Action),
			}), []Ref{
				{Kind: "agent", ID: string(t.Action.AgentID)},
			}
	case t.Decision != nil:
		return b.frag("router.target.decision", Args{
				"profile": b.ident(string(t.Decision.ProfileID)), "revision": b.ident(string(t.Decision.ProfileRevisionID)),
				"task": b.ident(string(t.Decision.TaskID)),
			}), []Ref{
				{Kind: "profile", ID: string(t.Decision.ProfileID)},
				{Kind: "task", ID: string(t.Decision.TaskID)},
			}
	case t.Lookup != nil:
		return b.frag("router.target.lookup", Args{"template": b.ident(t.Lookup.TemplateID)}),
			[]Ref{{Kind: "template", ID: t.Lookup.TemplateID}}
	}
	b.fail(fmt.Errorf("narrate: router target has no variant"))
	return Fragment{}, nil
}

// proposalRefs is the receipt: the proposal, the visible catalog and grammar
// set it was routed over, and the target-choice decision behind it.
func proposalRefs(p router.Proposal) []Ref {
	rc := p.Receipt
	return []Ref{
		{Kind: "proposal", ID: p.ID},
		{Kind: "router", ID: rc.Router},
		{Kind: "catalog", ID: rc.CatalogDigest},
		{Kind: "grammar_set", ID: rc.GrammarSetDigest},
		{Kind: "feature_schema", ID: rc.FeatureSchemaID},
		{Kind: "router_task", ID: string(rc.TaskID)},
		{Kind: "picture", ID: string(rc.PictureID)},
		{Kind: "decision_request", ID: string(rc.RequestID)},
		{Kind: "predictor", ID: string(rc.PredictorID)},
		{Kind: "prediction", ID: string(rc.PredictionID)},
	}
}
