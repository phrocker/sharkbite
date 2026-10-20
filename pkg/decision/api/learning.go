// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package api

import (
	"context"
	"encoding/json"
)

// MaxLearningRequestBytes bounds one learning request body: a distillation
// manifest or a training receipt is the largest thing a console submits.
const MaxLearningRequestBytes = 1 << 20

// LearningProvider is the host-supplied learning loop behind the Learning
// console: datasets, distillation jobs, candidates and evaluations,
// releases, file-backed evidence, and offline release bundles. Every method
// is also an authenticated route, and the route and the console produce
// the same records. A provider is responsible for authorization of the
// records it accepts; the transport authenticates the caller and refuses
// everything else. Reads return JSON the console renders as-is; writes take
// the sealed records or bound requests the loop defines and return the
// record they produced.
type LearningProvider interface {
	Datasets(ctx context.Context) (json.RawMessage, error)
	RequestExport(ctx context.Context, cohortID string) (json.RawMessage, error)
	Jobs(ctx context.Context) (json.RawMessage, error)
	Job(ctx context.Context, manifestPin string) (json.RawMessage, error)
	SubmitJob(ctx context.Context, manifest json.RawMessage) (json.RawMessage, error)
	SubmitReceipt(ctx context.Context, manifestPin string, receipt json.RawMessage) (json.RawMessage, error)
	Candidates(ctx context.Context) (json.RawMessage, error)
	SubmitCandidate(ctx context.Context, body json.RawMessage) (json.RawMessage, error)
	Evaluations(ctx context.Context, candidateID string) (json.RawMessage, error)
	SubmitEvaluation(ctx context.Context, evaluation json.RawMessage) (json.RawMessage, error)
	Releases(ctx context.Context) (json.RawMessage, error)
	Promote(ctx context.Context, body json.RawMessage) (json.RawMessage, error)
	Rollback(ctx context.Context, body json.RawMessage) (json.RawMessage, error)
	Evidence(ctx context.Context, kind string) (json.RawMessage, error)
	Bundles(ctx context.Context) (json.RawMessage, error)
	ExportBundle(ctx context.Context, body json.RawMessage) (json.RawMessage, error)
	ImportBundle(ctx context.Context, body json.RawMessage) (json.RawMessage, error)
}
