// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package api

import (
	"context"
	"encoding/json"
)

// MaxLearningRequestBytes bounds one learning request body: a distillation
// manifest or a training receipt is the largest thing a console submits.
const MaxLearningRequestBytes = 1 << 20

// LearningActor is the authenticated principal behind a learning write. The
// transport binds it to the context from the trusted decision; a provider
// seals approvals and import provenance against it and refuses a write that
// arrives without one. It is never taken from a request body.
type LearningActor struct {
	Subject string
	Actor   string
}

type learningActorKey struct{}

// WithLearningActor binds the authenticated actor to ctx.
func WithLearningActor(ctx context.Context, actor LearningActor) context.Context {
	return context.WithValue(ctx, learningActorKey{}, actor)
}

// LearningActorFrom returns the bound actor; false when the transport bound
// none, which every write must treat as unauthenticated.
func LearningActorFrom(ctx context.Context) (LearningActor, bool) {
	actor, ok := ctx.Value(learningActorKey{}).(LearningActor)
	return actor, ok && actor.Actor != ""
}

// LearningProvider is the host-supplied learning loop behind the Learning
// console: datasets, distillation jobs, candidates and evaluations,
// releases, file-backed evidence, and offline release bundles. Every method
// is also an authenticated route, and the route and the console produce
// the same records. The transport authenticates every call, requires the
// learning_write operation for every write, and binds the actor with
// WithLearningActor; the provider seals approvals against that actor and
// validates every record it accepts. Reads return JSON the console renders
// as-is; writes take the sealed records or bound requests the loop defines
// and return the record they produced.
type LearningProvider interface {
	Datasets(ctx context.Context) (json.RawMessage, error)
	RequestExport(ctx context.Context, cohortID string) (json.RawMessage, error)
	Jobs(ctx context.Context) (json.RawMessage, error)
	Job(ctx context.Context, manifestPin string) (json.RawMessage, error)
	SubmitJob(ctx context.Context, manifest json.RawMessage) (json.RawMessage, error)
	SubmitPredictions(ctx context.Context, manifestPin string, predictions json.RawMessage) (json.RawMessage, error)
	SubmitReceipt(ctx context.Context, manifestPin string, receipt json.RawMessage) (json.RawMessage, error)
	Candidates(ctx context.Context) (json.RawMessage, error)
	SubmitCandidate(ctx context.Context, body json.RawMessage) (json.RawMessage, error)
	// SealCandidate records candidate, evaluation, and artifact for a completed
	// run from its bundle and a predeclared evaluation policy.
	SealCandidate(ctx context.Context, body json.RawMessage) (json.RawMessage, error)
	Evaluations(ctx context.Context, candidateID string) (json.RawMessage, error)
	SubmitEvaluation(ctx context.Context, evaluation json.RawMessage) (json.RawMessage, error)
	Releases(ctx context.Context) (json.RawMessage, error)
	// Predict asks the configured student runtime to answer for one feature
	// vector; a host without a runtime refuses.
	Predict(ctx context.Context, body json.RawMessage) (json.RawMessage, error)
	Promote(ctx context.Context, body json.RawMessage) (json.RawMessage, error)
	Rollback(ctx context.Context, body json.RawMessage) (json.RawMessage, error)
	Evidence(ctx context.Context, kind string) (json.RawMessage, error)
	Bundles(ctx context.Context) (json.RawMessage, error)
	ExportBundle(ctx context.Context, body json.RawMessage) (json.RawMessage, error)
	ImportBundle(ctx context.Context, body json.RawMessage) (json.RawMessage, error)
}
