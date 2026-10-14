/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package authorized

import (
	"context"
	"errors"
	"sort"
	"strconv"

	"github.com/phrocker/shoal-oss/internal/labelmigration"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// LabelMigrationVersion is the version of the startup label migration
// (#570), recorded in its report.
const LabelMigrationVersion uint32 = 1

// maxReportedLabelBytes bounds the raw label kept in the report before it is
// escaped, so one pathological property cannot flood the operator's log.
const maxReportedLabelBytes = 512

// LabelMigrationRecord is the report of the latest completed label
// migration run. It is stored in the policy catalog after the last document
// of every run, for -list-untranslatable-labels; it never gates a run.
type LabelMigrationRecord struct {
	Version uint32
	// Documents is every base document examined.
	Documents int
	// Unregistered documents have no catalog registration and are already
	// readable by nobody.
	Unregistered int
	// Unlabelled documents carry no label and keep their rule.
	Unlabelled int
	// Tightened documents had at least one registration narrowed.
	Tightened int
	// AlreadyTightened documents carried their labels already (ingested
	// after labels were enforced, or migrated by an interrupted run).
	AlreadyTightened int
	// HistoricalTightened counts historical revisions narrowed by their own
	// labels, beyond the current revision's.
	HistoricalTightened int
	// Untranslatable lists every document (or historical revision) whose
	// labels could not be translated; each was conjoined with the reserved,
	// never-grantable UntranslatableLabelPolicyID.
	Untranslatable []UntranslatableLabel
	// Drift lists documents whose base current revision is not the one the
	// catalog registers (an ingest that committed to the base and then
	// failed to register). Each registered revision is still narrowed by its
	// own labels; the operator retries the ingest to repair it.
	Drift []RevisionDrift
}

// RevisionDrift is one base/catalog disagreement about a document's current
// revision.
type RevisionDrift struct {
	DocumentID        shoal.ID
	SourceURI         string
	CatalogRevisionID shoal.ID
	BaseRevisionID    shoal.ID
	Reason            string
}

// UntranslatableLabel is one report entry. EscapedLabel is the stored label
// value, truncated and Go-quoted (strconv.QuoteToASCII), so it is safe to
// print whatever bytes it holds.
type UntranslatableLabel struct {
	DocumentID   shoal.ID
	RevisionID   shoal.ID
	SourceURI    string
	EscapedLabel string
	Reason       string
}

func (r LabelMigrationRecord) clone() LabelMigrationRecord {
	r.Untranslatable = append([]UntranslatableLabel(nil), r.Untranslatable...)
	r.Drift = append([]RevisionDrift(nil), r.Drift...)
	return r
}

// ExtractionRecordReader lists the base's stored extraction records. The
// embedded explorer implements it; the migration needs it to attribute
// legacy relations to the document that asserted them.
type ExtractionRecordReader interface {
	ExtractionRecords(context.Context) ([]explorer.ExtractionRecord, error)
}

// MigrateLabelledDocuments narrows the catalog rule of every document that
// was labelled before labels were enforced (#570). Such a document carries
// the bare source rule plus the free-form shoal.visibility property, so any
// holder of the source can read it. "Refuse at the next write" (#544's model)
// would leave it open until someone happens to write it, so this runs at
// startup instead, before anything is served.
//
// For each base document it reads shoal.visibility from the revision
// metadata and, when that is not empty, from the stored document node (the
// two must agree), parses it with interaction.ParseVisibility, builds
// LabelRule(sourcePolicy, labels), and calls PolicyStore.TightenRule, which
// rewrites every revision, the current projections, the extracted nodes and
// edges, the relations the base's extraction records say the document
// asserted, and the source claim. Each historical revision is then narrowed
// by its own labels too, which may differ from the current revision's.
//
// A label set that cannot be translated (it does not parse, falls outside the
// charset, or exceeds a length, term or byte bound) is not an error: the
// document is conjoined with UntranslatablePolicy instead, so it and
// everything derived from it is readable by nobody, and it is listed in the
// report. Any store or base error aborts the run, and the caller must refuse
// to serve.
//
// It runs on EVERY start, not once: a binary from before #585 (a rollback)
// can register labelled documents under the bare rule again, and the next
// start must close them. Every step is idempotent, and one index built per run
// keeps each document's work proportional to that document's registrations.
// The run holds the client's mutation lock and the store's mutation lease
// throughout. Its report is stored (PutLabelMigration) after the last
// document, for -list-untranslatable-labels; it never gates a later run.
func (c *Client) MigrateLabelledDocuments(
	ctx context.Context,
	capability *labelmigration.Capability,
) (LabelMigrationRecord, error) {
	if !capability.Granted() {
		return LabelMigrationRecord{}, shoal.NewError(
			shoal.ErrorUnauthorized,
			"the label migration requires the internal migration capability")
	}
	if err := contextFailure(ctx); err != nil {
		return LabelMigrationRecord{}, err
	}
	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()
	lease, err := c.policyStore.AcquireMutation(ctx)
	if err != nil {
		return LabelMigrationRecord{}, err
	}
	defer lease.Release()

	summaries, err := c.base.Documents(ctx)
	if err != nil {
		return LabelMigrationRecord{}, err
	}
	sort.Slice(summaries, func(left, right int) bool {
		return shoal.CompareID(
			summaries[left].Document.ID, summaries[right].Document.ID) < 0
	})
	asserted, err := c.assertedEdges(ctx)
	if err != nil {
		return LabelMigrationRecord{}, err
	}
	index, err := c.policyStore.TighteningIndex(ctx)
	if err != nil {
		return LabelMigrationRecord{}, err
	}
	run := migrationRun{client: c, index: index, asserted: asserted}
	record := LabelMigrationRecord{Version: LabelMigrationVersion}
	for _, summary := range summaries {
		if err := validateSummary(summary); err != nil {
			return LabelMigrationRecord{}, inconsistentBase()
		}
		record.Documents++
		if err := run.document(ctx, summary, &record); err != nil {
			return LabelMigrationRecord{}, err
		}
	}
	if err := c.policyStore.PutLabelMigration(ctx, record); err != nil {
		return LabelMigrationRecord{}, err
	}
	c.invalidateAuthorizedVectorAvailability()
	return record, nil
}

// assertedEdges groups the base's extraction records' edge IDs by document
// and revision. A base that can extract but cannot list its extraction
// records is refused: the migration could not attribute legacy relations.
func (c *Client) assertedEdges(
	ctx context.Context,
) (map[shoal.ID]map[shoal.ID][]shoal.ID, error) {
	asserted := make(map[shoal.ID]map[shoal.ID][]shoal.ID)
	reader, ok := c.base.(ExtractionRecordReader)
	if !ok {
		if _, extracts := c.base.(extractionBackend); extracts {
			return nil, shoal.NewError(shoal.ErrorUnavailable,
				"the label migration cannot read the base's extraction records")
		}
		return asserted, nil
	}
	records, err := reader.ExtractionRecords(ctx)
	if err != nil {
		return nil, err
	}
	for _, record := range records {
		revisions := asserted[record.DocumentID]
		if revisions == nil {
			revisions = make(map[shoal.ID][]shoal.ID)
			asserted[record.DocumentID] = revisions
		}
		revisions[record.RevisionID] = append(
			revisions[record.RevisionID], record.EdgeIDs...)
	}
	return asserted, nil
}

type migrationRun struct {
	client   *Client
	index    *TighteningIndex
	asserted map[shoal.ID]map[shoal.ID][]shoal.ID
}

// assertedFor returns the relation edges the document asserted: from every
// revision when whole is true, else from that revision only.
func (r migrationRun) assertedFor(documentID, revisionID shoal.ID, whole bool) []shoal.ID {
	var edges []shoal.ID
	for revision, ids := range r.asserted[documentID] {
		if whole || revision == revisionID {
			edges = append(edges, ids...)
		}
	}
	sortIDs(edges)
	return edges
}

func (r migrationRun) document(
	ctx context.Context,
	summary explorer.DocumentSummary,
	record *LabelMigrationRecord,
) error {
	c := r.client
	documentID := summary.Document.ID
	current, registered, err := c.policyStore.CurrentRevision(ctx, documentID)
	if err != nil {
		return err
	}
	if !registered {
		record.Unregistered++
		return nil
	}
	// Base/catalog drift: an ingest committed a new revision to the base and
	// then failed to register it, so the catalog's current revision is older
	// than the base's. That is not a label failure. Each registered revision
	// is still narrowed by its own labels, and the drift is reported so the
	// operator can retry the ingest, which repairs it under the ordinary
	// relabel rule. It must never lock the document or its claim.
	if summary.Revision.ID != current.RevisionID {
		record.Drift = append(record.Drift, RevisionDrift{
			DocumentID:        documentID,
			SourceURI:         summary.SourceURI,
			CatalogRevisionID: current.RevisionID,
			BaseRevisionID:    summary.Revision.ID,
			Reason: "the base's current revision is not the registered one; " +
				"retry the ingest of this source",
		})
	}
	labels, raw, reason, err := c.currentRevisionLabels(
		ctx, documentID, current.RevisionID)
	currentServed := true
	if errors.Is(err, errRevisionNotServed) {
		if summary.Revision.ID == current.RevisionID {
			return inconsistentBase()
		}
		// The registered current revision is not served, so it cannot be
		// read or labelled; the drift entry above already names the
		// document. Only its own tightening is skipped: every historical
		// revision the base still serves is narrowed below all the same.
		currentServed, err = false, nil
	}
	if err != nil {
		return err
	}
	if currentServed {
		outcome, err := r.tightenRevision(ctx, current, summary.SourceURI,
			r.assertedFor(documentID, current.RevisionID, true), labels, reason)
		if err != nil {
			return err
		}
		if outcome.untranslatable != "" {
			record.Untranslatable = append(record.Untranslatable, UntranslatableLabel{
				DocumentID:   documentID,
				RevisionID:   current.RevisionID,
				SourceURI:    summary.SourceURI,
				EscapedLabel: escapeLabel(raw),
				Reason:       outcome.untranslatable,
			})
		}
		switch {
		case outcome.unlabelled:
			record.Unlabelled++
		case outcome.changed:
			record.Tightened++
		default:
			record.AlreadyTightened++
		}
	}

	// Historical revisions, read after the sweep above so their rules
	// include the current revision's labels, are narrowed by their own.
	for _, revisionID := range r.index.Revisions(documentID) {
		if revisionID == current.RevisionID {
			continue
		}
		revision, ok, err := c.policyStore.Revision(ctx, documentID, revisionID)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		view, err := c.base.Document(ctx, documentID, revisionID)
		if shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			// The base no longer serves this revision, so nothing of it can
			// be read; it already carries the current labels.
			continue
		}
		if err != nil {
			return err
		}
		raw := view.Document.Metadata[interaction.PropertyVisibility]
		labels, parseErr := interaction.ParseVisibility(raw)
		reason := ""
		if parseErr != nil {
			reason = "revision visibility does not parse: " + parseErr.Error()
		}
		outcome, err := r.tightenRevision(ctx, revision, "",
			r.assertedFor(documentID, revisionID, false), labels, reason)
		if err != nil {
			return err
		}
		if outcome.changed {
			record.HistoricalTightened++
		}
		if outcome.untranslatable != "" {
			record.Untranslatable = append(record.Untranslatable, UntranslatableLabel{
				DocumentID:   documentID,
				RevisionID:   revisionID,
				SourceURI:    summary.SourceURI,
				EscapedLabel: escapeLabel(raw),
				Reason:       outcome.untranslatable,
			})
		}
	}
	return nil
}

type tightenOutcome struct {
	changed        bool
	unlabelled     bool
	untranslatable string
}

// tightenRevision derives the labelled rule for one revision registration and
// applies it through TightenRule. A non-empty reason, or a label set
// LabelRule refuses, conjoins UntranslatablePolicy instead.
//
// TightenRule is called whenever the revision carries any label policy, even
// when its own rule already has them all: the call then sweeps the derived
// registrations again from the bare source rule, which completes a durable
// write a crash tore, closes registrations a rolled-back binary wrote, and is
// otherwise a no-op.
func (r migrationRun) tightenRevision(
	ctx context.Context,
	revision RevisionRegistration,
	sourceURI string,
	assertedEdges []shoal.ID,
	labels []string,
	reason string,
) (tightenOutcome, error) {
	bare, anchor, sources, err := splitLabelledRule(revision.Rule)
	if err != nil {
		return tightenOutcome{}, err
	}
	var outcome tightenOutcome
	var target AccessRule
	if reason == "" && len(labels) > 0 && sources != 1 {
		reason = "the document rule has " + strconv.Itoa(sources) +
			" source policies, so a label cannot be bound to one"
	}
	if reason == "" {
		labelled, labelErr := LabelRule(anchor, labels)
		if labelErr == nil {
			target, labelErr = conjoinRules(revision.Rule, labelled)
		}
		if labelErr != nil {
			reason = "labels cannot be translated: " + labelErr.Error()
		}
	}
	if reason != "" {
		untranslatable, policyErr := UntranslatablePolicy(anchor)
		if policyErr != nil {
			return tightenOutcome{}, policyErr
		}
		components := append(revision.Rule.components(), untranslatable)
		target, err = NewAccessRule(components...)
		if err != nil {
			return tightenOutcome{}, err
		}
		outcome.untranslatable = reason
	}
	from := revision.Rule
	if target.equal(revision.Rule) {
		if bare.equal(revision.Rule) {
			outcome.unlabelled = true
			return outcome, nil
		}
		// Already labelled: sweep from the bare rule.
		from = bare
	}
	outcome.changed, err = r.client.policyStore.TightenRule(ctx, r.index, RuleTightening{
		DocumentID:      revision.DocumentID,
		RevisionID:      revision.RevisionID,
		SourceURI:       sourceURI,
		From:            from,
		To:              target,
		AssertedEdgeIDs: assertedEdges,
	})
	if err != nil {
		return tightenOutcome{}, err
	}
	return outcome, nil
}

// splitLabelledRule separates a rule into its non-label components (the
// bare rule), the first of them (the anchor label policies derive from), and
// how many there are. A rule made only of label policies is refused: no
// writer produces one.
func splitLabelledRule(rule AccessRule) (AccessRule, auth.Policy, int, error) {
	var sources []auth.Policy
	for _, policy := range rule.components() {
		if auth.IsLabelPolicyID(policy.GrantPolicyID()) {
			continue
		}
		sources = append(sources, policy)
	}
	if len(sources) == 0 {
		return AccessRule{}, auth.Policy{}, 0, shoal.NewError(
			shoal.ErrorInternal,
			"a catalog rule holds no source policy, only label policies")
	}
	bare, err := NewAccessRule(sources...)
	if err != nil {
		return AccessRule{}, auth.Policy{}, 0, err
	}
	return bare, sources[0], len(sources), nil
}

// errRevisionNotServed reports that the base does not serve a revision the
// catalog registers: drift, never a label failure.
var errRevisionNotServed = shoal.NewError(
	shoal.ErrorNotFound, "the base does not serve the registered revision")

// currentRevisionLabels reads the labels of one registered revision from
// that revision's OWN metadata, the declaration parse.go copied onto its
// nodes. When the stored document node names this same revision, its labels
// are read too and conjoined (a disagreement only narrows); when it names
// another revision, the base has moved past the catalog (an ingest whose
// registration failed), which is drift and is ignored here.
//
// Only a label that does not parse is returned as a reason (the document then
// becomes untranslatable). A revision the base does not serve returns
// errRevisionNotServed; any other base failure is an error.
func (c *Client) currentRevisionLabels(
	ctx context.Context,
	documentID, revisionID shoal.ID,
) (labels []string, raw string, reason string, err error) {
	view, err := c.base.Document(ctx, documentID, revisionID)
	if shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		return nil, "", "", errRevisionNotServed
	}
	if err != nil {
		return nil, "", "", err
	}
	declared := view.Document.Metadata[interaction.PropertyVisibility]
	labels, parseErr := interaction.ParseVisibility(declared)
	if parseErr != nil {
		return nil, declared, "revision visibility does not parse: " +
			parseErr.Error(), nil
	}
	if declared == "" {
		// parse.go derives the nodes' labels from this metadata alone, so an
		// unlabelled revision has unlabelled nodes: skip the graph read.
		return nil, "", "", nil
	}
	neighborhood, err := c.base.Neighborhood(ctx, explorer.NeighborhoodRequest{
		NodeIDs:   []shoal.ID{documentID},
		Depth:     1,
		EdgeTypes: []string{"contains"},
	})
	if shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		return labels, declared, "", nil
	}
	if err != nil {
		return nil, "", "", err
	}
	for _, node := range neighborhood.Nodes {
		if node.ID != documentID ||
			node.Properties["revision_id"] != string(revisionID) {
			continue
		}
		nodeLabels, parseErr := interaction.NodeVisibility(node)
		if parseErr != nil {
			return nil, node.Properties[interaction.PropertyVisibility],
				"document node visibility does not parse: " + parseErr.Error(), nil
		}
		union, err := interaction.Conjoin(labels, nodeLabels)
		if err != nil {
			return nil, declared, "visibility does not parse: " + err.Error(), nil
		}
		return union, declared, "", nil
	}
	return labels, declared, "", nil
}

func escapeLabel(raw string) string {
	if len(raw) > maxReportedLabelBytes {
		return strconv.QuoteToASCII(raw[:maxReportedLabelBytes]) + "..."
	}
	return strconv.QuoteToASCII(raw)
}
