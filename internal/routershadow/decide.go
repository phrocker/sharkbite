// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package routershadow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/router"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// DecisionTimeout bounds one target-choice decision.
const DecisionTimeout = 5 * time.Second

// Decider serves the target-choice decision in process through the Predictor
// port (internal/routerwire wraps an unchanged internal/decisionlinear
// provider). The decision's evidence is the numeric
// feature artifact alone: the request carries no text, no token and no target
// identity, only opaque candidate subjects and their feature vectors.
type Decider struct {
	Predictor Predictor
	Clock     func() time.Time
}

// DecideInput is the caller binding of one decision. Every field comes from
// the caller's resolved authorization, never from the text.
type DecideInput struct {
	Analysis        *router.Analysis
	PrincipalID     shoal.ID
	CorrelationID   shoal.ID
	AuthFingerprint string
	AuthExpiresAt   time.Time
}

// Decided is a routed proposal with the decision records behind it.
type Decided struct {
	Proposal   router.Proposal
	Request    *decision.DecisionRequest
	Prediction *decision.PredictionRecord
}

// Decide builds the task, picture and request, resolves and runs the
// predictor, validates the response into a prediction record, and aggregates
// it into a proposal. An early abstention (empty or out-of-bounds text) or an
// empty catalog requests no decision.
func (d *Decider) Decide(ctx context.Context, in DecideInput) (Decided, error) {
	a := in.Analysis
	receipt := a.Receipt()
	if _, early := a.Early(); early || a.Candidates() == 0 {
		p, err := a.Propose(make([]bool, a.Candidates()), receipt)
		return Decided{Proposal: p}, err
	}
	task, err := router.TaskSpec()
	if err != nil {
		return Decided{}, err
	}
	now := d.Clock().UTC()
	artifact := a.FeatureArtifact()
	digest := sha256Hex(artifact.Bytes)
	artifactID := shoal.ID("router.features:" + digest)
	revisionID := shoal.ID("sha256:" + digest)
	anchors := make([]inference.EvidenceAnchor, len(artifact.Subjects))
	subjects := make([]decision.Subject, len(artifact.Subjects))
	for i, id := range artifact.Subjects {
		span := artifact.Ranges[i]
		anchor, err := inference.NewDocumentAnchor(document.Citation{
			DocumentID: artifactID, RevisionID: revisionID,
			SectionID: "router.subjects", SpanID: id,
			Range: document.SourceRange{
				Start: document.SourcePosition{Offset: int64(span.Start)},
				End:   document.SourcePosition{Offset: int64(span.End)},
			},
		}, string(artifact.Bytes[span.Start:span.End]))
		if err != nil {
			return Decided{}, err
		}
		anchors[i] = anchor
		subjects[i] = decision.Subject{ID: id, SourceID: "router.features.source", Disposition: decision.Supported, EvidenceIDs: []shoal.ID{anchor.ID()}}
	}
	snapshot, err := inference.NewSnapshotPin(shoal.ID("router.catalog:"+receipt.CatalogDigest), now)
	if err != nil {
		return Decided{}, err
	}
	authPin, err := inference.NewAuthPin(shoal.ID("router.auth:"+in.AuthFingerprint), in.AuthExpiresAt)
	if err != nil {
		return Decided{}, err
	}
	pack, err := inference.NewContextPack("router.target-choice", anchors, nil, snapshot, authPin, nil)
	if err != nil {
		return Decided{}, err
	}
	picture, err := decision.NewPictureManifest(pack, decision.PictureConfig{
		TaskID: task.ID(), ObservationID: shoal.ID("router.observation:" + digest),
		EnumerationID: shoal.ID("router.catalog:" + receipt.CatalogDigest),
		ScopeID:       shoal.ID("router.scope:" + in.AuthFingerprint),
		BuilderID:     router.FeatureSchemaID, OntologyProjectionID: "router.no-ontology/v1",
		InputDigest: digest, TokenizerID: "lexicon.normalization/v2",
		InputTokens: uint64(a.TokenCount()), TokenBudget: router.MaxTokens, Cutoff: now,
		Sources: []decision.Source{{
			ArtifactID: artifactID, ID: "router.features.source", RevisionID: revisionID,
			Digest: digest, OriginID: "router.featurizer", AuthorityPolicyID: "router.authority.self-computed/v1",
			Role: decision.Observation, Control: decision.CandidateControlled,
			ObservedAt: now, ReceivedAt: now,
		}},
		Subjects: subjects,
	})
	if err != nil {
		return Decided{}, err
	}
	deadline := now.Add(DecisionTimeout)
	if deadline.After(in.AuthExpiresAt) {
		deadline = in.AuthExpiresAt
	}
	request, err := decision.NewDecisionRequest(task, picture, d.Predictor.RouterPredictorIdentity(), decision.RequestConfig{
		PrincipalID: in.PrincipalID, ReleaseID: d.Predictor.RouterReleaseID(), CorrelationID: in.CorrelationID,
		RequestedAt: now, Deadline: deadline, SubjectIDs: artifact.Subjects,
	})
	if err != nil {
		return Decided{}, err
	}
	// The request deadline is on the injected clock; the call itself is
	// bounded in real time by the same budget.
	ctx, cancel := context.WithTimeout(ctx, deadline.Sub(now))
	defer cancel()
	result, err := d.Predictor.RouterPredict(ctx, request, artifact.Bytes)
	if err != nil {
		return Decided{}, err
	}
	result.CompletedAt = d.Clock().UTC()
	if result.CompletedAt.Before(now) {
		result.CompletedAt = now
	}
	record, err := decision.NewPredictionRecord(request, result)
	if err != nil {
		return Decided{}, err
	}
	matched := make([]bool, len(artifact.Subjects))
	index := make(map[shoal.ID]int, len(artifact.Subjects))
	for i, id := range artifact.Subjects {
		index[id] = i
	}
	config := record.Config()
	if config.Status != decision.Completed {
		return Decided{}, shoal.NewError(shoal.ErrorUnavailable, "router target choice did not complete")
	}
	for _, answer := range config.Answers {
		if answer.Status == decision.Answered && answer.Label == router.LabelMatch {
			matched[index[answer.SubjectID]] = true
		}
	}
	receipt.FeatureSchemaID = router.FeatureSchemaID
	receipt.TaskID = task.ID()
	receipt.PictureID = picture.ID()
	receipt.RequestID = request.ID()
	receipt.PredictorID = request.PredictorID()
	receipt.PredictionID = record.ID()
	proposal, err := a.Propose(matched, receipt)
	if err != nil {
		return Decided{}, err
	}
	return Decided{Proposal: proposal, Request: &request, Prediction: &record}, nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
