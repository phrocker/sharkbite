// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
// Package decisiondatasets exports registered numeric cohorts through a mandatory
// trusted authority. Files bind provenance but cannot enforce later revocation.
package decisiondatasets

import (
	"context"
	journal "github.com/phrocker/shoal-oss/internal/decisionadjudicationstore"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
	"time"
)

// Cohort is immutable registered membership, never caller-selected labels or
// permissions. InventoryID fixes the declared finite sampling namespace.
type Cohort struct {
	ID, AuthorityRevisionID, InventoryID          shoal.ID
	Task                                          decision.TaskSpec
	Policy                                        decision.LabelPolicy
	QuestionID, FeatureSchemaID, FeatureBuilderID shoal.ID
	SplitPolicyID, SamplingPolicyID               shoal.ID
	Cutoff                                        time.Time
	// Mode is prospective or reconstructed. It describes availability provenance,
	// not model quality; checkpoint training overlap remains unknown.
	Mode    string
	Members []Member
}
type Member struct {
	ID, TargetID, RequestID, PredictionID, SubjectID shoal.ID
	FamilyIDs                                        []shoal.ID
	Split                                            string
	// Nil means unknown, not a zero-probability sample. A fixture census is only
	// a census of that registered fixture, never of a larger repository.
	InclusionProbability *float64
}

// Target contains trusted retained input and label material. Load must check
// complete current history using the adjudication service (or equivalent), and
// return the basis of the last history receipt available by the cohort cutoff.
// An empty history is allowed only when the trusted registry proves absence.
type Target struct {
	Prediction         decision.PredictionRecord
	History            []journal.Receipt
	SelectedBasis      decision.AdjudicationBasis
	Features           []float64
	FeatureInputDigest string
	FeatureReceivedAt  time.Time
	InventoryID        shoal.ID
	OutcomeReceiptIDs  []shoal.ID
	InventoryComplete  bool
	// These are results from trusted current authority, never request fields.
	// SourceAvailable includes exact retained original bytes and current read use.
	// TrainingAllowed separately checks the pinned policy's training purpose.
	SourceAvailable, TrainingAllowed, LabelWithdrawn bool
}

// Authority must independently establish provenance, exact feature derivation
// from the original picture, complete target inventories, and current source,
// label and training-purpose permission. Source read permission alone is not
// permission to export or train. Verify jointly rechecks ALL members and their
// inventory generations after all IO; a changed/incomplete generation fails the
// whole export. Unauthorized targets must not appear in exclusions or counts.
// No source/storage load may occur after Verify's final authorization check.
type Authority interface {
	Resolve(context.Context, auth.Decision, shoal.ID) (Cohort, error)
	Load(context.Context, auth.Decision, Cohort, Member) (Target, error)
	Verify(context.Context, auth.Decision, Cohort, []Target) error
}
type Config struct {
	Resolver  auth.Resolver
	Authority Authority
	Clock     func() time.Time
}
type Bundle struct {
	Dataset, Manifest []byte
	ManifestSHA256    string
}
