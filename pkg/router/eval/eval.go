// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package eval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/phrocker/shoal-oss/internal/routershadow"
	"github.com/phrocker/shoal-oss/pkg/router"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// FixedNow is the evaluation clock: receipts are reproducible.
var FixedNow = time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

// ReleaseID is the evaluation release of the router model.
const ReleaseID shoal.ID = "router-pair-v1:eval"

// Runner routes fixture cases through the same analysis, decision and
// aggregation the shadow service uses, with visibility from the fixture.
type Runner struct {
	World     *World
	Decider   *routershadow.Decider
	validator router.InputValidator
	catalogs  map[string]*router.Catalog
}

// NewRunner builds a runner over the router's predictor and input-validator
// ports; the caller wires them (internal/routerwire), as the shadow service's
// host does.
func NewRunner(w *World, predictor routershadow.Predictor, validator router.InputValidator) *Runner {
	return &Runner{
		World:     w,
		Decider:   &routershadow.Decider{Predictor: predictor, Clock: func() time.Time { return FixedNow }},
		validator: validator,
		catalogs:  map[string]*router.Catalog{},
	}
}

// Catalog is the caller's visible catalog.
func (r *Runner) Catalog(caller string) (*router.Catalog, error) {
	if c, ok := r.catalogs[caller]; ok {
		return c, nil
	}
	c, err := router.NewCatalog(r.World.Targets(caller), r.World.Grammars, r.validator)
	if err != nil {
		return nil, err
	}
	r.catalogs[caller] = c
	return c, nil
}

// Analyze analyzes one case.
func (r *Runner) Analyze(c Case) (*router.Analysis, error) {
	catalog, err := r.Catalog(c.Caller)
	if err != nil {
		return nil, err
	}
	return router.Analyze(r.World.Input(c.Caller, c.Text, catalog))
}

// Route returns the router's and the baseline's proposals for one case.
func (r *Runner) Route(ctx context.Context, c Case) (router.Proposal, router.Proposal, error) {
	a, err := r.Analyze(c)
	if err != nil {
		return router.Proposal{}, router.Proposal{}, err
	}
	fp := sha256.Sum256([]byte("eval:" + c.Caller))
	decided, err := r.Decider.Decide(ctx, routershadow.DecideInput{
		Analysis: a, PrincipalID: shoal.ID("eval:" + c.Caller), CorrelationID: shoal.ID("eval:" + c.ID),
		AuthFingerprint: hex.EncodeToString(fp[:]), AuthExpiresAt: FixedNow.Add(time.Hour),
	})
	if err != nil {
		return router.Proposal{}, router.Proposal{}, err
	}
	baseline, err := a.Baseline()
	if err != nil {
		return router.Proposal{}, router.Proposal{}, err
	}
	return decided.Proposal, baseline, nil
}

// Examples are the training pairs of the given cases: one per (case,
// visible candidate), positive when the candidate is the labelled target.
func (r *Runner) Examples(cases []Case) ([]router.Example, error) {
	var out []router.Example
	for _, c := range cases {
		a, err := r.Analyze(c)
		if err != nil {
			return nil, err
		}
		if _, early := a.Early(); early {
			continue
		}
		catalog, _ := r.Catalog(c.Caller)
		for i, key := range catalog.Keys() {
			out = append(out, router.Example{Features: a.Features()[i], Match: key == c.Expected.Target})
		}
	}
	return out, nil
}

// Outcome is one case's result under both systems.
type Outcome struct {
	Case     Case
	Router   router.Proposal
	Baseline router.Proposal
}

// Run routes every case.
func (r *Runner) Run(ctx context.Context, cases []Case) ([]Outcome, error) {
	out := make([]Outcome, 0, len(cases))
	for _, c := range cases {
		p, b, err := r.Route(ctx, c)
		if err != nil {
			return nil, fmt.Errorf("case %s: %w", c.ID, err)
		}
		out = append(out, Outcome{Case: c, Router: p, Baseline: b})
	}
	return out, nil
}

// SlotValues renders a proposal's slots as fixture values: node keys or
// enum values.
func (w *World) SlotValues(p router.Proposal) map[string]string {
	out := map[string]string{}
	for _, s := range p.Slots {
		if s.Enum != "" {
			out[s.Name] = s.Enum
		} else if len(s.NodeIDs) == 1 {
			out[s.Name] = w.NodeKey(s.NodeIDs[0])
		} else {
			out[s.Name] = "<ambiguous>"
		}
	}
	return out
}

func targetKey(p router.Proposal) string {
	if p.Target == nil {
		return ""
	}
	return p.Target.Key()
}

// TargetCorrect: a non-abstained proposal of the expected kind and target.
func TargetCorrect(c Case, p router.Proposal) bool {
	return c.Answerable() && p.Kind != router.KindAbstain && p.Kind == c.Expected.Kind && targetKey(p) == c.Expected.Target
}

func slotsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// Correct is end-to-end correctness: an answerable case needs the expected
// kind, target and exactly the expected slots (an abstention is wrong); an
// unanswerable case needs an abstention, whatever its reason.
func (w *World) Correct(c Case, p router.Proposal) bool {
	if !c.Answerable() {
		return p.Kind == router.KindAbstain
	}
	return TargetCorrect(c, p) && slotsEqual(w.SlotValues(p), c.Expected.Slots)
}

// Counts is an exact N of D.
type Counts struct {
	N int `json:"n"`
	D int `json:"d"`
}

func (c Counts) String() string {
	if c.D == 0 {
		return fmt.Sprintf("%d of %d", c.N, c.D)
	}
	return fmt.Sprintf("%d of %d (%.1f%%)", c.N, c.D, 100*float64(c.N)/float64(c.D))
}

// Report is one system's metrics on one split.
type Report struct {
	Split             string                    `json:"split"`
	System            string                    `json:"system"`
	Cases             int                       `json:"cases"`
	Answerable        int                       `json:"answerable"`
	Unanswerable      int                       `json:"unanswerable"`
	TargetPrecision   Counts                    `json:"target_precision"`
	EndToEnd          Counts                    `json:"end_to_end"`
	Coverage          Counts                    `json:"coverage"`
	Abstention        Counts                    `json:"abstention"`
	CorrectAbstention Counts                    `json:"correct_abstention"`
	SlotExact         Counts                    `json:"slot_exact"`
	ReasonAgreement   Counts                    `json:"reason_agreement"`
	Confusion         map[string]map[string]int `json:"confusion"`
	ByTag             map[string]Counts         `json:"by_tag"`
}

// Measure computes a report for one system's proposals.
func (w *World) Measure(split, system string, outcomes []Outcome, pick func(Outcome) router.Proposal) Report {
	r := Report{Split: split, System: system, Confusion: map[string]map[string]int{}, ByTag: map[string]Counts{}}
	for _, o := range outcomes {
		c, p := o.Case, pick(o)
		r.Cases++
		abstained := p.Kind == router.KindAbstain
		correct := w.Correct(c, p)
		if correct {
			r.EndToEnd.N++
		}
		if c.Answerable() {
			r.Answerable++
			r.Coverage.D++
			if !abstained {
				r.Coverage.N++
			}
		} else {
			r.Unanswerable++
			r.CorrectAbstention.D++
			if abstained {
				r.CorrectAbstention.N++
				r.ReasonAgreement.D++
				if len(p.Reasons) == 1 && p.Reasons[0] == c.Expected.Reason {
					r.ReasonAgreement.N++
				}
			}
		}
		if abstained {
			r.Abstention.N++
		} else {
			r.TargetPrecision.D++
			if TargetCorrect(c, p) {
				r.TargetPrecision.N++
				r.SlotExact.D++
				if slotsEqual(w.SlotValues(p), c.Expected.Slots) {
					r.SlotExact.N++
				}
			}
		}
		row := string(c.Expected.Kind)
		if r.Confusion[row] == nil {
			r.Confusion[row] = map[string]int{}
		}
		r.Confusion[row][string(p.Kind)]++
		tags := c.Tags
		if len(tags) == 0 {
			tags = []string{"plain"}
		}
		for _, tag := range tags {
			t := r.ByTag[tag]
			t.D++
			if correct {
				t.N++
			}
			r.ByTag[tag] = t
		}
	}
	r.EndToEnd.D = r.Cases
	r.Abstention.D = r.Cases
	return r
}

// Comparison is the paired end-to-end comparison of router and baseline.
type Comparison struct {
	BothRight    int     `json:"both_right"`
	RouterOnly   int     `json:"router_only"`
	BaselineOnly int     `json:"baseline_only"`
	BothWrong    int     `json:"both_wrong"`
	ExactP       float64 `json:"exact_mcnemar_p"`
}

// Compare counts discordant pairs and the exact two-sided McNemar p-value
// (a binomial test on the discordant pairs at 1/2).
func (w *World) Compare(outcomes []Outcome) Comparison {
	var c Comparison
	for _, o := range outcomes {
		r, b := w.Correct(o.Case, o.Router), w.Correct(o.Case, o.Baseline)
		switch {
		case r && b:
			c.BothRight++
		case r:
			c.RouterOnly++
		case b:
			c.BaselineOnly++
		default:
			c.BothWrong++
		}
	}
	c.ExactP = exactMcNemar(c.RouterOnly, c.BaselineOnly)
	return c
}

func exactMcNemar(b, c int) float64 {
	n := b + c
	if n == 0 {
		return 1
	}
	k := b
	if c < k {
		k = c
	}
	tail := new(big.Int)
	for i := 0; i <= k; i++ {
		tail.Add(tail, new(big.Int).Binomial(int64(n), int64(i)))
	}
	p, _ := new(big.Rat).SetFrac(new(big.Int).Mul(tail, big.NewInt(2)), new(big.Int).Lsh(big.NewInt(1), uint(n))).Float64()
	return math.Min(1, p)
}

// Markdown renders reports and a comparison as tables.
func Markdown(reports []Report, comparison Comparison) string {
	var b strings.Builder
	b.WriteString("| Metric | " + headers(reports) + " |\n|---|" + strings.Repeat("---|", len(reports)) + "\n")
	rows := []struct {
		name string
		get  func(Report) Counts
	}{
		{"Target precision (non-abstained)", func(r Report) Counts { return r.TargetPrecision }},
		{"End-to-end accuracy", func(r Report) Counts { return r.EndToEnd }},
		{"Coverage (answerable)", func(r Report) Counts { return r.Coverage }},
		{"Abstention rate", func(r Report) Counts { return r.Abstention }},
		{"Correct abstention (unanswerable)", func(r Report) Counts { return r.CorrectAbstention }},
		{"Slot exact-match (target-correct)", func(r Report) Counts { return r.SlotExact }},
		{"Abstention reason agreement (secondary)", func(r Report) Counts { return r.ReasonAgreement }},
	}
	for _, row := range rows {
		b.WriteString("| " + row.name + " |")
		for _, r := range reports {
			b.WriteString(" " + row.get(r).String() + " |")
		}
		b.WriteString("\n")
	}
	b.WriteString(fmt.Sprintf("\nDiscordant pairs (end-to-end): router right and baseline wrong %d, baseline right and router wrong %d; both right %d, both wrong %d; exact McNemar p = %.4g.\n",
		comparison.RouterOnly, comparison.BaselineOnly, comparison.BothRight, comparison.BothWrong, comparison.ExactP))
	for _, r := range reports {
		b.WriteString("\nConfusion, " + r.System + " (rows expected, columns proposed):\n\n| expected \\ proposed |")
		for _, k := range router.Kinds {
			b.WriteString(" " + string(k) + " |")
		}
		b.WriteString("\n|---|" + strings.Repeat("---|", len(router.Kinds)) + "\n")
		for _, row := range router.Kinds {
			b.WriteString("| " + string(row) + " |")
			for _, col := range router.Kinds {
				b.WriteString(fmt.Sprintf(" %d |", r.Confusion[string(row)][string(col)]))
			}
			b.WriteString("\n")
		}
		var tags []string
		for tag := range r.ByTag {
			tags = append(tags, tag)
		}
		sort.Strings(tags)
		b.WriteString("\nEnd-to-end by tag, " + r.System + ": ")
		for i, tag := range tags {
			if i > 0 {
				b.WriteString("; ")
			}
			b.WriteString(tag + " " + r.ByTag[tag].String())
		}
		b.WriteString(".\n")
	}
	return b.String()
}

func headers(reports []Report) string {
	var parts []string
	for _, r := range reports {
		parts = append(parts, r.System+" ("+r.Split+")")
	}
	return strings.Join(parts, " | ")
}
