// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package narrate renders Shoal records as text, deterministically and
// without a model.
//
// It covers approval status (pkg/explorer/fleet ApprovalStatus), dispatch
// action records and their transitions (ActionRecord, ActionTransition),
// decision prediction records and evidence eligibility (pkg/decision), and
// caller-asserted reasons (pkg/interaction). Every sentence comes from a
// message catalog keyed by record kind, outcome and condition; nothing is
// generated, so a sentence can say only what its record says.
//
// The rules are in docs/narrate.md:
//
//   - Coverage: every state, condition, transition, error code and decision
//     result has a template. The tests derive the enumerations from the
//     source constants, so adding a value without a template fails them, and
//     a catalog missing any template cannot be loaded.
//   - Grounding: every Sentence carries its record ID and the evidence
//     references it was derived from.
//   - Untrusted text — action input and output, executor error codes Shoal
//     does not define, predictor and evidence-builder reasons, caller-asserted
//     reasons, and identifiers that are not plain tokens — is never prose. It
//     appears only as a quoted, attributed, escaped, length-bounded span.
package narrate

import (
	"fmt"
	"sort"
	"time"
)

// Role is a sentence's place in a narration. Sentences are returned in role
// order: what happened, why, what is missing, how it got here, what is
// blocking the next step, what can happen next, and quoted detail.
type Role string

const (
	RoleOutcome Role = "outcome"
	RoleReason  Role = "reason"
	RoleGap     Role = "gap"
	RoleHistory Role = "history"
	RoleBlocked Role = "blocked"
	RoleNext    Role = "next"
	RoleDetail  Role = "detail"
)

var roleRank = map[Role]int{
	RoleOutcome: 0, RoleReason: 1, RoleGap: 2, RoleHistory: 3,
	RoleBlocked: 4, RoleNext: 5, RoleDetail: 6,
}

// Ref is an evidence or record reference a sentence was derived from. ID is
// the referenced identity as a plain token, or "hex:" and its bytes in hex
// when it is not one.
type Ref struct {
	Kind string
	ID   string
}

// Sentence is one rendered sentence and where it came from.
type Sentence struct {
	Role Role
	// Key is the catalog message the sentence was realized from.
	Key  string
	Text string
	// RecordID names the record, as "<kind>:<id>". Opaque byte IDs that are
	// not plain tokens are hex-encoded ("hex:…").
	RecordID string
	Refs     []Ref
	// Quotes are the untrusted spans inside Text, in order.
	Quotes []Quote
	// Spans mark, by byte offset and in order, every quoted span and every
	// bare identifier in Text.
	Spans []Span
}

// Options adjusts a rendering. The zero value is valid.
type Options struct {
	// Now enables statements that depend on the current time: whether a
	// lease is live, whether a deadline has passed, how long remains. When
	// zero, those are omitted and next steps are stated with their
	// conditions instead. It is never read from the clock.
	Now time.Time
	// QuoteInput adds the action input as a quoted detail sentence.
	QuoteInput bool
	// QuoteOutput adds the action output as a quoted detail sentence.
	QuoteOutput bool
	// MaxQuoteRunes bounds each quoted span (default DefaultQuoteRunes).
	MaxQuoteRunes int
	// MaxListItems bounds a rendered list before "and N more"
	// (default DefaultListItems).
	MaxListItems int
	// MaxAnswers bounds per-answer sentences of a prediction
	// (default DefaultAnswers).
	MaxAnswers int
}

const (
	DefaultListItems = 5
	DefaultAnswers   = 20
)

func (o Options) listItems() int {
	if o.MaxListItems <= 0 {
		return DefaultListItems
	}
	return o.MaxListItems
}

func (o Options) answers() int {
	if o.MaxAnswers <= 0 {
		return DefaultAnswers
	}
	return o.MaxAnswers
}

// Renderer renders records with one catalog. It is safe for concurrent use.
type Renderer struct {
	catalog *Catalog
}

// New returns a renderer over catalog, or the English catalog when nil.
func New(catalog *Catalog) *Renderer {
	if catalog == nil {
		catalog = English()
	}
	return &Renderer{catalog: catalog}
}

// builder accumulates the sentences of one record.
type builder struct {
	r        *Renderer
	opts     Options
	recordID string
	out      []Sentence
	err      error
}

func (r *Renderer) begin(recordID string, opts Options) *builder {
	return &builder{r: r, opts: opts, recordID: recordID}
}

func (b *builder) add(role Role, key string, args Args, refs ...Ref) {
	if b.err != nil {
		return
	}
	if !requiredKeySet()[key] {
		b.err = fmt.Errorf("narrate: key %q is not registered", key)
		return
	}
	fr, err := b.r.catalog.format(key, args)
	if err != nil {
		b.err = err
		return
	}
	// Reference IDs are record data, often caller-supplied. They are kept
	// when they are plain tokens and hex-encoded otherwise, so a reader that
	// prints a reference cannot be handed a line break or a bidi override,
	// and the encoding stays reversible for lookups.
	safeRefs := make([]Ref, 0, len(refs))
	for _, ref := range refs {
		if ref.ID == "" {
			continue
		}
		safeRefs = append(safeRefs, Ref{Kind: ref.Kind, ID: opaqueID([]byte(ref.ID))})
	}
	b.out = append(b.out, Sentence{
		Role: role, Key: key, Text: fr.text, RecordID: b.recordID,
		Refs: safeRefs, Quotes: fr.quotes, Spans: fr.spans,
	})
}

// frag renders a catalog message as a fragment for use inside another.
func (b *builder) frag(key string, args Args) Fragment {
	if b.err != nil {
		return Fragment{}
	}
	if !requiredKeySet()[key] {
		b.err = fmt.Errorf("narrate: key %q is not registered", key)
		return Fragment{}
	}
	fr, err := b.r.catalog.format(key, args)
	if err != nil {
		b.err = err
	}
	return fr
}

func (b *builder) fail(err error) {
	if b.err == nil {
		b.err = err
	}
}

func (b *builder) ident(value string) Fragment {
	return ident(value, b.opts.MaxQuoteRunes)
}

func (b *builder) quote(attribution Attribution, by, value string) Fragment {
	return quoted(attribution, by, value, b.opts.MaxQuoteRunes)
}

// bounded caps a list, replacing the tail with "N more".
func (b *builder) bounded(items []Fragment) []Fragment {
	limit := b.opts.listItems()
	if len(items) <= limit {
		return items
	}
	out := append([]Fragment(nil), items[:limit]...)
	return append(out, b.frag("list.more", Args{"n": len(items) - limit}))
}

func (b *builder) finish() ([]Sentence, error) {
	if b.err != nil {
		return nil, b.err
	}
	sort.SliceStable(b.out, func(i, j int) bool {
		return roleRank[b.out[i].Role] < roleRank[b.out[j].Role]
	})
	return b.out, nil
}

func yesNo(v bool) Selector {
	if v {
		return "yes"
	}
	return "no"
}
