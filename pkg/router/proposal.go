// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package router turns short text into a proposal: which registered action,
// decision task or lookup template the text refers to, and the subjects and
// parameters it names, or an abstention with a named reason.
//
// The package is pure. It reads no clock, network, store or model; callers
// supply the tokens, the mentions the caller may see, the catalog of targets
// the caller may see, and the target-choice predictions. A proposal carries no
// authority: an action still goes through admission and approval, a decision
// is still computed by its own predictor over its own evidence, and a lookup
// is still answered by its template over the graph. See
// docs/router-evaluation.md and docs/local-language.md.
package router

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"

	"github.com/phrocker/shoal-oss/pkg/lexicon"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Version names this router's proposal contract.
const Version = "shoal.router/v1"

// BaselineVersion names the lexical baseline comparator.
const BaselineVersion = "shoal.router.baseline/v1"

// Kind is what a proposal refers to.
type Kind string

const (
	KindAction   Kind = "action"
	KindDecision Kind = "decision"
	KindLookup   Kind = "lookup"
	KindAbstain  Kind = "abstain"
)

// Kinds is every Kind, in a fixed order.
var Kinds = []Kind{KindAction, KindDecision, KindLookup, KindAbstain}

// Reason is a closed abstention code.
type Reason string

const (
	// ReasonEmptyText: the text has no tokens.
	ReasonEmptyText Reason = "empty_text"
	// ReasonTextOutOfBounds: the text exceeds the mention byte or token bound.
	ReasonTextOutOfBounds Reason = "text_out_of_bounds"
	// ReasonNoTarget: no visible target was selected.
	ReasonNoTarget Reason = "no_target"
	// ReasonAmbiguousTarget: two or more visible targets were selected.
	ReasonAmbiguousTarget Reason = "ambiguous_target"
	// ReasonAmbiguousExecutor: the selected action is offered by two or more
	// visible descriptors.
	ReasonAmbiguousExecutor Reason = "ambiguous_executor"
	// ReasonAmbiguousMention: a slot is covered by a mention naming two or
	// more visible nodes, or two mentions could fill it.
	ReasonAmbiguousMention Reason = "ambiguous_mention"
	// ReasonMissingSlot: a slot the target requires was not filled.
	ReasonMissingSlot Reason = "missing_slot"
	// ReasonSlotMismatch: a slot was filled by a node of the wrong kind or
	// concept.
	ReasonSlotMismatch Reason = "slot_mismatch"
	// ReasonInvalidInput: the filled input does not validate against the
	// target's schema.
	ReasonInvalidInput Reason = "invalid_input"
)

// Reasons is the closed set of abstention reasons, in a fixed order.
var Reasons = []Reason{
	ReasonEmptyText, ReasonTextOutOfBounds, ReasonNoTarget,
	ReasonAmbiguousTarget, ReasonAmbiguousExecutor, ReasonAmbiguousMention,
	ReasonMissingSlot, ReasonSlotMismatch, ReasonInvalidInput,
}

func (r Reason) valid() bool {
	for _, known := range Reasons {
		if r == known {
			return true
		}
	}
	return false
}

// ActionRef names one action on one registered descriptor.
type ActionRef struct {
	AgentID          shoal.ID `json:"agent_id"`
	AgentGeneration  int64    `json:"agent_generation"`
	Capability       string   `json:"capability"`
	Action           string   `json:"action"`
	RequiresApproval bool     `json:"requires_approval"`
}

// DecisionRef names one registered decision profile revision and its task.
type DecisionRef struct {
	ProfileID         shoal.ID `json:"profile_id"`
	ProfileRevisionID shoal.ID `json:"profile_revision_id"`
	TaskID            shoal.ID `json:"task_id"`
}

// LookupRef names one lookup template.
type LookupRef struct {
	TemplateID  string `json:"template_id"`
	RelationKey string `json:"relation_key"`
	Direction   string `json:"direction"`
}

// TargetRef is one of an action, a decision or a lookup. Exactly the variant
// named by Kind is set.
type TargetRef struct {
	Kind     Kind         `json:"kind"`
	Action   *ActionRef   `json:"action,omitempty"`
	Decision *DecisionRef `json:"decision,omitempty"`
	Lookup   *LookupRef   `json:"lookup,omitempty"`
}

// Key is the target's grammar binding key: "action:<capability>/<action>",
// "decision:<profile id>" or "lookup:<template id>". Two descriptors that
// offer one action share a key.
func (t TargetRef) Key() string {
	switch {
	case t.Kind == KindAction && t.Action != nil:
		return actionKey(t.Action.Capability, t.Action.Action)
	case t.Kind == KindDecision && t.Decision != nil:
		return "decision:" + string(t.Decision.ProfileID)
	case t.Kind == KindLookup && t.Lookup != nil:
		return "lookup:" + t.Lookup.TemplateID
	}
	return ""
}

func actionKey(capability, action string) string {
	return "action:" + capability + "/" + action
}

func (t TargetRef) validate() error {
	set := 0
	for _, present := range []bool{t.Action != nil, t.Decision != nil, t.Lookup != nil} {
		if present {
			set++
		}
	}
	if set != 1 || t.Key() == "" {
		return invalid("router target must set exactly the variant its kind names")
	}
	return nil
}

func (t TargetRef) clone() TargetRef {
	if t.Action != nil {
		a := *t.Action
		t.Action = &a
	}
	if t.Decision != nil {
		d := *t.Decision
		t.Decision = &d
	}
	if t.Lookup != nil {
		l := *t.Lookup
		t.Lookup = &l
	}
	return t
}

// Slot is one filled parameter: a graph node (NodeIDs, exactly one in v1) or
// a closed enum value from the grammar (Enum). TokenSpan is where in the
// tokenized text it was found. A slot never holds text from the input.
type Slot struct {
	Name      string       `json:"name"`
	NodeIDs   []shoal.ID   `json:"node_ids,omitempty"`
	Enum      string       `json:"enum,omitempty"`
	TokenSpan lexicon.Span `json:"token_span"`
}

// Receipt pins what a proposal was computed from. It holds no text and
// nothing derived from a target the caller cannot see: CatalogDigest covers
// the visible targets and the grammars bound to them only, and the decision
// identities are those of the target-choice request built from them.
type Receipt struct {
	Router           string   `json:"router"`
	GrammarSetDigest string   `json:"grammar_set_digest"`
	CatalogDigest    string   `json:"catalog_digest"`
	Candidates       int      `json:"candidates"`
	FeatureSchemaID  string   `json:"feature_schema_id,omitempty"`
	TaskID           shoal.ID `json:"task_id,omitempty"`
	PictureID        shoal.ID `json:"picture_id,omitempty"`
	RequestID        shoal.ID `json:"request_id,omitempty"`
	PredictorID      shoal.ID `json:"predictor_id,omitempty"`
	PredictionID     shoal.ID `json:"prediction_id,omitempty"`
}

// Proposal is the router's only output. It is a proposal: nothing has run.
type Proposal struct {
	ID     string     `json:"id"`
	Kind   Kind       `json:"kind"`
	Target *TargetRef `json:"target,omitempty"`
	Slots  []Slot     `json:"slots,omitempty"`
	// Input is the canonical validated input: for an action, exactly the
	// bytes an enqueue would store.
	Input   json.RawMessage `json:"input,omitempty"`
	Reasons []Reason        `json:"reasons,omitempty"`
	// Missing names the slots behind a slot abstention.
	Missing []string `json:"missing,omitempty"`
	Receipt Receipt  `json:"receipt"`
}

// proposalBody is everything the ID covers.
type proposalBody struct {
	Kind    Kind            `json:"kind"`
	Target  *TargetRef      `json:"target,omitempty"`
	Slots   []Slot          `json:"slots,omitempty"`
	Input   json.RawMessage `json:"input,omitempty"`
	Reasons []Reason        `json:"reasons,omitempty"`
	Missing []string        `json:"missing,omitempty"`
	Receipt Receipt         `json:"receipt"`
}

// ProposalID is the deterministic identity of a proposal's content: SHA-256
// over its canonical JSON without the ID.
func ProposalID(p Proposal) (string, error) {
	b, err := json.Marshal(proposalBody{p.Kind, p.Target, p.Slots, p.Input, p.Reasons, p.Missing, p.Receipt})
	if err != nil {
		return "", invalid("router proposal cannot be encoded")
	}
	sum := sha256.Sum256(b)
	return "router.proposal:v1:" + hex.EncodeToString(sum[:]), nil
}

// seal normalizes and identifies a proposal.
func seal(p Proposal) Proposal {
	sort.Slice(p.Slots, func(i, j int) bool { return p.Slots[i].Name < p.Slots[j].Name })
	sort.Slice(p.Reasons, func(i, j int) bool { return p.Reasons[i] < p.Reasons[j] })
	sort.Strings(p.Missing)
	id, err := ProposalID(p)
	if err != nil {
		// Unreachable: every field is a plain value.
		panic(err)
	}
	p.ID = id
	return p
}

func abstain(reason Reason, receipt Receipt) Proposal {
	return seal(Proposal{Kind: KindAbstain, Reasons: []Reason{reason}, Receipt: receipt})
}

// Validate checks a proposal's shape: a known kind; an abstention has one or
// more known reasons and no input; any other kind has a target of that kind,
// a valid JSON input and no reasons; and the ID matches the content.
func (p Proposal) Validate() error {
	switch p.Kind {
	case KindAbstain:
		if len(p.Reasons) == 0 || len(p.Input) != 0 {
			return invalid("router abstention requires reasons and no input")
		}
		for i, r := range p.Reasons {
			if !r.valid() || (i > 0 && p.Reasons[i-1] >= r) {
				return invalid("router abstention reasons must be known, sorted and unique")
			}
		}
		if p.Target != nil {
			if err := p.Target.validate(); err != nil {
				return err
			}
		}
	case KindAction, KindDecision, KindLookup:
		if p.Target == nil || p.Target.Kind != p.Kind || len(p.Reasons) != 0 || len(p.Missing) != 0 {
			return invalid("router proposal requires a target of its kind and no reasons")
		}
		if err := p.Target.validate(); err != nil {
			return err
		}
		if !json.Valid(p.Input) || bytes.TrimSpace(p.Input) == nil {
			return invalid("router proposal input is not valid JSON")
		}
	default:
		return invalid("router proposal kind is unknown")
	}
	for i, s := range p.Slots {
		if s.Name == "" || (i > 0 && p.Slots[i-1].Name >= s.Name) {
			return invalid("router slots must be named, sorted and unique")
		}
		if (len(s.NodeIDs) == 0) == (s.Enum == "") {
			return invalid("router slot must hold nodes or an enum value")
		}
	}
	id, err := ProposalID(p)
	if err != nil {
		return err
	}
	if id != p.ID {
		return invalid("router proposal identity mismatch")
	}
	return nil
}

func invalid(message string) error { return shoal.NewError(shoal.ErrorInvalidArgument, message) }

var errInternal = errors.New("router: internal invariant violated")
