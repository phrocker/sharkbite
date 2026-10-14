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
	"time"

	"github.com/phrocker/shoal-oss/internal/devbackfill"
	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// sourceIndependentSelector is a PolicySelector that declares it derives its
// policy without consulting explorer.Source.
//
// The declaring method is unexported, so only a selector defined in this
// package can claim it. That is deliberate: the claim is a security assertion
// about code this package can read, not something a caller may assert about
// its own selector. The declaration is not taken on trust -- backfillRule
// checks it against behaviour for every document it registers.
type sourceIndependentSelector interface {
	PolicySelector
	ignoresIngestSource()
}

// ignoresIngestSource declares that StaticPolicySelector derives its policy
// solely from trusted construction-time identities and the resolved decision.
// SelectPolicy in selector.go discards its explorer.Source parameter outright,
// so a reconstructed source cannot change the rule it returns.
func (s *StaticPolicySelector) ignoresIngestSource() {}

// backfillSourceProbe differs from the source reconstructed for a stored
// document in every field of explorer.Source, so a selector that keys on any
// of them derives a different policy for it. Deriving the same rule from both
// is evidence that the selector consulted neither; it is not a proof, because
// a selector could key on the source in some way this one input does not
// distinguish.
var backfillSourceProbe = explorer.Source{
	URI:       "shoal-backfill-probe:source-dependence",
	Title:     "shoal backfill source-dependence probe",
	MediaType: "application/x-shoal-backfill-probe",
	Content:   "shoal backfill source-dependence probe",
	Metadata:  shoal.Metadata{"shoal-backfill-probe": "1"},
}

// BackfillExistingDocumentsForDevelopment registers the current revision of
// every base document that has no policy-catalog registration yet, and returns
// how many were registered.
//
// TEMPORARY (issue #284): this exists only because PolicyStore has no durable
// implementation. MemoryPolicyStore loses every registration when the process
// exits, so a corpus that outlives the process becomes permanently invisible
// even to the principal that ingested it. Delete this method, and
// internal/devbackfill with it, when #284 lands a durable catalog.
//
// It is development scaffolding, not a supported operation, and it is fenced
// accordingly:
//
//   - the capability is defined in a module-internal package, so no external
//     consumer of this package can construct one, and a nil or zero-valued
//     capability is refused;
//   - the decision is read from ctx through the same trusted resolver every
//     other operation uses, must authorize OperationIngest, and must satisfy
//     the generation guard, so callers need the binding capability too;
//   - the source it can reconstruct for a stored document is always lossy --
//     the original content is gone, metadata is not retained on the summary
//     (only the visibility labels are read back, from the stored document
//     node, and conjoined into the rule; see storedDocumentVisibility),
//     and the title is the stored one rather than the submitted one -- so a
//     selector that keys on any source field could derive a rule the document
//     was never ingested under. Rather than guess, this refuses unless the
//     configured selector is declared source-independent, and then verifies
//     that claim per document against a contrasting probe source.
//
// Documents that are already registered are left untouched, and any failure
// aborts with the error rather than reporting partial success.
func (c *Client) BackfillExistingDocumentsForDevelopment(
	ctx context.Context,
	capability *devbackfill.Capability,
) (int, error) {
	if !capability.Granted() {
		return 0, shoal.NewError(shoal.ErrorUnauthorized,
			"the development corpus backfill requires the internal "+
				"development capability")
	}
	if _, ok := c.policySelector.(sourceIndependentSelector); !ok {
		return 0, backfillSourceLoss()
	}
	decision, guard, now, err := c.begin(ctx, auth.OperationIngest)
	if err != nil {
		return 0, err
	}
	summaries, err := c.base.Documents(ctx)
	if err != nil {
		return 0, err
	}

	c.mutationMu.Lock()
	defer c.mutationMu.Unlock()
	lease, err := c.policyStore.AcquireMutation(ctx)
	if err != nil {
		return 0, policyCatalogWriteError(ctx, err)
	}
	defer lease.Release()
	if err := guard.Check(ctx); err != nil {
		return 0, err
	}

	// Every rule is derived before anything is written, so a document that
	// cannot be backfilled (for example one whose labels the operator does
	// not hold) refuses the whole run instead of leaving it half applied.
	pending := make([]RevisionRegistration, 0, len(summaries))
	for _, summary := range summaries {
		if err := validateSummary(summary); err != nil {
			return 0, inconsistentBase()
		}
		_, exists, err := c.policyStore.CurrentRevision(ctx, summary.Document.ID)
		if err != nil {
			return 0, policyCatalogReadError(ctx, err)
		}
		if exists {
			continue
		}
		// A stored document's labels are read back from its own document
		// node, never assumed absent: the reconstructed source has no
		// metadata, and registering a labelled document under the bare
		// source rule would publish it to every source holder (#570).
		visibility, err := c.storedDocumentVisibility(ctx, summary)
		if err != nil {
			return 0, err
		}
		rule, err := c.backfillRule(ctx, decision, summary, visibility, now)
		if err != nil {
			return 0, err
		}
		registration, err := c.existingRevisionRegistration(
			ctx, summary, visibility, rule)
		if err != nil {
			return 0, err
		}
		pending = append(pending, registration)
	}
	if err := guard.Check(ctx); err != nil {
		return 0, err
	}
	registered := 0
	for _, registration := range pending {
		if err := c.policyStore.PutRevision(ctx, registration); err != nil {
			return 0, policyCatalogWriteError(ctx, err)
		}
		registered++
	}
	if err := guard.Check(ctx); err != nil {
		return 0, err
	}
	c.invalidateAuthorizedVectorAvailability()
	return registered, nil
}

// backfillRule derives the rule for a stored document and refuses if the
// selector observably depends on the source it was given. The source that can
// be reconstructed for a stored document is lossy, so a source-dependent
// selector would register the document under a rule nobody selected for it --
// possibly a more permissive one. That is worse than leaving it hidden.
func (c *Client) backfillRule(
	ctx context.Context,
	decision auth.Decision,
	summary explorer.DocumentSummary,
	visibility string,
	now time.Time,
) (AccessRule, error) {
	var metadata shoal.Metadata
	if visibility != "" {
		metadata = shoal.Metadata{interaction.PropertyVisibility: visibility}
	}
	rule, err := c.selectIngestRule(ctx, decision, explorer.Source{
		URI:       summary.SourceURI,
		Title:     summary.Document.Title,
		MediaType: summary.SourceMediaType,
		Metadata:  metadata,
	}, now)
	if err != nil {
		return AccessRule{}, err
	}
	// The probe carries the same stored labels, so the comparison still
	// isolates the selector's dependence on the source: the labels are
	// conjoined after selection and are the same on both sides.
	probe := cloneSource(backfillSourceProbe)
	if visibility != "" {
		probe.Metadata[interaction.PropertyVisibility] = visibility
	}
	probed, err := c.selectIngestRule(ctx, decision, probe, now)
	if err != nil {
		return AccessRule{}, err
	}
	if !rule.equal(probed) {
		return AccessRule{}, backfillSourceLoss()
	}
	return rule, nil
}

// storedDocumentVisibility reads the canonical visibility expression stored on
// the current revision's document node. The node is where parse.go records a
// source's labels for the graph, so it is the authority the backfill uses. A
// missing node, a node of another revision, or a visibility that cannot be
// read refuses the backfill: the document is left unregistered (hidden) rather
// than registered unlabelled.
func (c *Client) storedDocumentVisibility(
	ctx context.Context,
	summary explorer.DocumentSummary,
) (string, error) {
	neighborhood, err := c.base.Neighborhood(ctx, explorer.NeighborhoodRequest{
		NodeIDs:   []shoal.ID{summary.Document.ID},
		Depth:     1,
		EdgeTypes: []string{"contains"},
	})
	if err != nil {
		return "", err
	}
	for _, node := range neighborhood.Nodes {
		if node.ID != summary.Document.ID {
			continue
		}
		if node.Properties["revision_id"] != string(summary.Revision.ID) {
			return "", inconsistentBase()
		}
		labels, err := interaction.NodeVisibility(node)
		if err != nil {
			return "", backfillLabelsUnreadable()
		}
		return interaction.Expression(labels), nil
	}
	return "", inconsistentBase()
}

func backfillLabelsUnreadable() error {
	return shoal.NewError(shoal.ErrorInvalidArgument,
		"refusing to backfill the policy catalog: a stored document's "+
			"visibility labels cannot be read, and it is never registered "+
			"as unlabelled")
}

func backfillSourceLoss() error {
	return shoal.NewError(shoal.ErrorInvalidArgument,
		"refusing to backfill the policy catalog: the configured policy "+
			"selector may derive a rule from the ingest source, and the "+
			"source of an already-stored document cannot be reconstructed "+
			"without loss")
}

// existingRevisionRegistration derives the catalog record for a base document
// using the same view verification, node identity, digest, and intrinsic edge
// derivation that Ingest performs, so the read path verifies a backfilled
// registration exactly as it verifies one written at ingest time.
func (c *Client) existingRevisionRegistration(
	ctx context.Context,
	summary explorer.DocumentSummary,
	visibility string,
	rule AccessRule,
) (RevisionRegistration, error) {
	view, err := c.base.Document(
		ctx, summary.Document.ID, summary.Revision.ID)
	if err != nil {
		return RevisionRegistration{}, err
	}
	if view.Document.ID != summary.Document.ID ||
		view.Document.RevisionID != summary.Revision.ID ||
		view.Revision.ID != summary.Revision.ID ||
		view.Revision.DocumentID != summary.Document.ID {
		return RevisionRegistration{}, inconsistentBase()
	}
	// The revision's own metadata must declare exactly the labels read from
	// its document node; any disagreement refuses rather than picks one.
	declared, err := interaction.ParseVisibility(
		view.Document.Metadata[interaction.PropertyVisibility])
	if err != nil || interaction.Expression(declared) != visibility {
		return RevisionRegistration{}, backfillLabelsUnreadable()
	}
	nodeIDs, err := documentViewNodeIDs(view)
	if err != nil {
		return RevisionRegistration{}, inconsistentBase()
	}
	digest, err := documentViewDigest(view)
	if err != nil {
		return RevisionRegistration{}, inconsistentBase()
	}
	intrinsicEdges, err := c.intrinsicEdges(ctx, nodeIDs)
	if err != nil {
		return RevisionRegistration{}, err
	}
	return RevisionRegistration{
		DocumentID:     summary.Document.ID,
		RevisionID:     summary.Revision.ID,
		NodeIDs:        nodeIDs,
		IntrinsicEdges: intrinsicEdges,
		ContentDigest:  digest,
		Rule:           rule,
		Current:        true,
	}, nil
}
