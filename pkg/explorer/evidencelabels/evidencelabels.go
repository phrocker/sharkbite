// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

// Package evidencelabels holds the one definition of "may this reader see
// evidence carrying these visibility labels" for evidence that is *stored*
// rather than scanned.
//
// These labels are enforced nowhere else, and the reason first given for this
// package said otherwise. It said label enforcement in this plane happens at
// the scan — that a document a reader may not see does not come back from
// storage, so no service-layer filter was needed, and an evidence reference
// merely escaped that.
//
// That does not hold for the free-form labels an evidence reference carries.
// interaction.PropertyVisibility ("shoal.visibility") is written into a
// node's *properties* by explorer.setVisibility, reached from parse.go, and
// never into a cell visibility — so the scan has nothing to filter on and
// internal/visfilter never sees it.
//
// This paragraph said something else until #570 landed, and the way it went
// stale is worth leaving on the record. It said authorized reads gate only on
// the access rule's domain, source and policy, so a reader granted a source
// receives every labelled document, span and node in it. That was true when
// written and is now false: #570 translates each (source, label) pair into a
// policy and conjoins it into the labelled document's AccessRule, which
// ruleAllows enforces on every authorized read. Documents, spans and nodes
// are covered.
//
// Evidence references are not, and that is what Filter is still for. An
// EvidenceRef is a field of a fleet ActionRecord rather than a labelled
// document, so no AccessRule governs it and ruleAllows never sees it — #570
// did not touch it and #564 has not yet wired an evaluator. So Filter
// remains the only check here, which keeps the nil case load-bearing rather
// than defensive: a deployment with no Visibility withholds labelled
// evidence precisely because nothing else would.
//
// The narrower claim is the durable one. "Nothing enforces these labels" was
// true of the whole system for a while and is now true only of this seam, so
// state it about this seam.
//
// An evidence reference recorded on another record — a dispatch action
// (#369), or the lifecycle event published for it (#562) — was recorded by
// that record's principal and is stored as a field of it, so it is returned
// to whoever may read the record rather than to whoever holds its labels.
// Every such path filters through Filter here, so the rule cannot drift
// between them.
//
// A leaf package on purpose: the dispatch plane and the event plane both
// import it, and neither imports the other for it.
package evidencelabels

import (
	"context"
	"errors"
	"time"

	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Visibility answers whether the current reader holds the labels an evidence
// reference carries.
//
// Implemented by the host that owns the request's decision:
// authorized.LabelVisibility (#564) decides structured grant labels against
// the reader's own auth.Decision and, for a trusted service, its configured
// ceiling. A free-form label is held by nobody, which is why Translator
// exists.
type Visibility interface {
	// VisibleToReader reports whether the reader behind ctx holds the labels
	// in a visibility expression. An error is not a false answer: it is the
	// question failing, and the caller returns it rather than silently
	// withholding.
	VisibleToReader(ctx context.Context, visibility []string) (bool, error)
}

// Translator rewrites, at record time, the visibility an evidence reference
// carries into the terms Visibility can decide.
//
// An executor reports the visibility of the nodes it retrieved as their
// shoal.visibility property: free-form ingest labels such as "secret". No
// decision holds such a label, so a reference stored with one is withheld
// from every reader, holders included. Since #570 each such label is enforced
// as a structured policy per (source, label) conjoined into the node's
// AccessRule, and that policy's grant labels (d:, s:, g:) are what a reader
// can be shown to hold. Translator replaces each free-form term with them.
//
// Implemented by the host that owns the policy catalog
// (authorized.LabelTranslator). A term it cannot translate exactly is kept as
// it was, so the reference stays withheld rather than opened.
type Translator interface {
	// StructuredVisibility returns visibility with every free-form term
	// replaced by the structured terms of the label policies that enforce it
	// on nodeIDs and the endpoints of edgeIDs. An error is the catalog read
	// failing, never a refusal.
	StructuredVisibility(
		ctx context.Context, nodeIDs, edgeIDs []shoal.ID, visibility []string,
	) ([]string, error)
}

// Filter returns the values the reader behind ctx may see, in order, and
// whether anything was withheld. labels returns a value's visibility
// expression.
//
// The rule, which is the whole of #369's decided shape:
//
//   - A value with no visibility expression is kept. It carries no label to
//     lack, so withholding it would redact something nothing asked to be
//     protected — and that is what keeps this from gutting the grounding every
//     deployment without labels relies on.
//   - A nil evaluator withholds every labelled value. A plane that cannot
//     evaluate a label must not hand out the identifiers the label exists to
//     protect.
//   - A labelled value the evaluator says the reader may not see is dropped
//     whole. There is no count of what was dropped, and the caller must not
//     return one: a count that disagrees with the list is an existence oracle
//     for the rest (#398). A partial list is therefore not a completeness
//     claim — a reader cannot tell "recorded no evidence" from "recorded
//     evidence you may not see".
//   - An evaluator error is returned, not treated as a redaction, so a
//     transient fault is not indistinguishable from a permanent refusal.
//
// values is never modified. When nothing is withheld the input slice itself is
// returned with withheld false.
func Filter[T any](
	ctx context.Context, evaluator Visibility, values []T, labels func(T) []string,
) (kept []T, withheld bool, err error) {
	if len(values) == 0 {
		return values, false, nil
	}
	readable := make([]T, 0, len(values))
	for _, value := range values {
		visibility := labels(value)
		if len(visibility) == 0 {
			readable = append(readable, value)
			continue
		}
		if evaluator == nil {
			// Nothing can evaluate the label, so nothing may be shown it.
			continue
		}
		visible, err := evaluator.VisibleToReader(ctx, visibility)
		if err != nil {
			return nil, false, err
		}
		if visible {
			readable = append(readable, value)
		}
	}
	if len(readable) == len(values) {
		return values, false, nil
	}
	return readable, true, nil
}

// Graph is what a stored evidence reference names in the corpus: its nodes,
// its edges and, for a document reference, the revision it cites. A gate
// decides the reference by the current rules of all of them.
type Graph struct {
	NodeIDs []shoal.ID
	EdgeIDs []shoal.ID
	// DocumentID and RevisionID name the revision a document reference
	// cites. Section and span identities do not depend on the revision, so
	// the cited revision's own rule must be asked too: a labelled historical
	// revision must not be readable through an unlabelled current one.
	DocumentID shoal.ID
	RevisionID shoal.ID
}

// Empty reports whether the reference names nothing current to consult.
func (g Graph) Empty() bool {
	return len(g.NodeIDs) == 0 && len(g.EdgeIDs) == 0 &&
		g.DocumentID == "" && g.RevisionID == ""
}

// NodeGate answers whether the reader behind ctx may see stored evidence
// references under the CURRENT access rules of what they name: every node,
// every edge's effective rule and both of its endpoints, and the cited
// revision's own rule. It is the gate interaction reads already apply
// (#567), offered to the planes that store evidence references, so that a
// document relabelled after an action recorded evidence from it governs that
// evidence as it governs the document. Implemented by the host that owns the
// policy catalog (authorized.NodeGate).
type NodeGate interface {
	// GraphsVisibleToReader decides every graph in one batch, so that a page
	// of references costs a bounded number of catalog reads rather than
	// several per reference. Anything the catalog does not know is not
	// visible. An error is the question failing, never a refusal.
	GraphsVisibleToReader(ctx context.Context, graphs []Graph) ([]bool, error)
	// GraphEvidenceValid reports, at record time, whether every graph
	// reference is authoritative at the snapshot its executor pinned: its
	// edges join its nodes in sequence (edge i from node i to node i+1, as
	// the catalog records it), and its assertions are exactly the ones the
	// corpus records on those edges, checked as the interaction recorder
	// checks them. A false answer refuses the evidence.
	GraphEvidenceValid(
		ctx context.Context, snapshotID shoal.ID, snapshotAsOf time.Time,
		references []interaction.EvidenceReference,
	) (bool, error)
}

// Verdicts decides every value at once:
//
//   - A value that names graph members (nodes, edges, a cited revision) is
//     decided by their current rules, through one batched gate call. Its
//     stored visibility is provenance only and is not consulted: after a
//     relabel it is wrong in both directions, admitting too much once a label
//     was added and withholding too much once one was removed. A nil gate
//     withholds it.
//   - A value that names nothing has nothing current to consult, so it is
//     decided by its stored labels exactly as Filter decides them.
//
// An error is returned rather than read as a refusal, as for Filter.
func Verdicts[T any](
	ctx context.Context, visibility Visibility, gate NodeGate, values []T,
	labels func(T) []string, graph func(T) Graph,
) ([]bool, error) {
	verdicts := make([]bool, len(values))
	var graphs []Graph
	var gated []int
	for index, value := range values {
		members := graph(value)
		if !members.Empty() {
			graphs = append(graphs, members)
			gated = append(gated, index)
			continue
		}
		// Nothing current to consult: the stored labels are all there is.
		kept, _, err := Filter(ctx, visibility, []T{value}, labels)
		if err != nil {
			return nil, err
		}
		verdicts[index] = len(kept) == 1
	}
	if len(graphs) == 0 || gate == nil {
		return verdicts, nil
	}
	decided, err := gate.GraphsVisibleToReader(ctx, graphs)
	if err != nil {
		return nil, err
	}
	if len(decided) != len(graphs) {
		return nil, errors.New("evidence gate answered a different number of references")
	}
	for position, index := range gated {
		verdicts[index] = decided[position]
	}
	return verdicts, nil
}

// FilterReferences applies Verdicts and returns the values the reader may
// see, in order, and whether anything was withheld. Withheld values are
// dropped whole with no count. values is never modified, and when nothing is
// withheld the input slice itself is returned.
func FilterReferences[T any](
	ctx context.Context, visibility Visibility, gate NodeGate, values []T,
	labels func(T) []string, graph func(T) Graph,
) (kept []T, withheld bool, err error) {
	if len(values) == 0 {
		return values, false, nil
	}
	verdicts, err := Verdicts(ctx, visibility, gate, values, labels, graph)
	if err != nil {
		return nil, false, err
	}
	return Apply(values, verdicts)
}

// Apply keeps the values whose verdict is true.
func Apply[T any](values []T, verdicts []bool) (kept []T, withheld bool, err error) {
	if len(verdicts) != len(values) {
		return nil, false, errors.New("evidence verdicts do not align with their values")
	}
	readable := make([]T, 0, len(values))
	for index, value := range values {
		if verdicts[index] {
			readable = append(readable, value)
		}
	}
	if len(readable) == len(values) {
		return values, false, nil
	}
	return readable, true, nil
}
