// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

// Package decisionlearning is the host-side learning service behind the
// Learning console and its authenticated routes: datasets, distillation
// jobs, candidates and evaluations, releases, file-backed evidence, and
// offline release bundles. It keeps sealed records in an append-only
// store over any exclusive-create backend, drives the release registry,
// and never creates authority: a promotion needs the same bound approval
// the records require whether it arrives from the console or a route.
package decisionlearning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/phrocker/shoal-oss/internal/distillation"
	"github.com/phrocker/shoal-oss/internal/releasebundle"
	"github.com/phrocker/shoal-oss/internal/storage"
	"github.com/phrocker/shoal-oss/internal/weightsbundle"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Disclaimer is stated on every release read: shadow evidence cannot
// promote a model or exclude review.
const Disclaimer = "Shadow evidence cannot promote a model or exclude review. Promotion grants shadow-candidate status only; activation is an explicit, bound approval."

// Evidence kinds the read-only evidence service exposes. Each is a
// directory of sealed JSON reports under the evidence root.
var EvidenceKinds = []string{"quality", "poisoning", "calibration", "influence", "challenger"}

// Record kinds kept in the store.
const (
	kindExport     = "dataset-export"
	kindManifest   = "distillation-manifest"
	kindJob        = "distillation-job"
	kindReceipt    = "distillation-receipt"
	kindCandidate  = weightsbundle.KindCandidate
	kindArtifact   = weightsbundle.KindArtifact
	kindEvaluation = weightsbundle.KindEvaluation
)

// Cohort is what a cohort authority lists.
type Cohort struct {
	ID            shoal.ID `json:"cohort_id"`
	TaskID        string   `json:"task_id"`
	LabelPolicyID string   `json:"label_policy_id"`
	Members       int      `json:"members"`
	Mode          string   `json:"provenance_mode"`
}

// CohortLister lists the cohorts registered with the host's authority.
type CohortLister interface {
	ListCohorts(ctx context.Context) ([]Cohort, error)
}

// Exporter produces an authorized immutable export for a registered cohort.
type Exporter interface {
	Export(ctx context.Context, cohortID shoal.ID) (Export, error)
}

// Export is the exporter's result: the dataset bytes and the manifest.
type Export struct {
	Dataset, Manifest []byte
}

// ServingReporter reports what the student runtime serves right now.
type ServingReporter interface {
	ServingReport(ctx context.Context) (map[string]any, error)
}

// Config composes a service.
type Config struct {
	Registry    *weightsbundle.Registry
	Store       weightsbundle.Store
	Prefix      string
	EvidenceDir string
	BundlesDir  string
	ServingRoot string
	Cohorts     CohortLister
	Exporter    Exporter
	Serving     ServingReporter
	Images      []releasebundle.ImageRef
	Now         func() time.Time
}

// Service implements decisionapi.LearningProvider.
type Service struct {
	cfg Config
	mu  sync.Mutex
}

// New validates the composition. Registry, store, and bundles directory are
// required; cohort authority, exporter, serving reporter, and evidence are
// optional and their absence is reported, never faked.
func New(cfg Config) (*Service, error) {
	if cfg.Registry == nil || cfg.Store == nil || cfg.BundlesDir == "" || cfg.ServingRoot == "" {
		return nil, errors.New("learning service needs a registry, a record store, a bundles directory, and a serving root")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	cfg.Prefix = strings.TrimRight(cfg.Prefix, "/")
	return &Service{cfg: cfg}, nil
}

func refused(err error) error   { return shoal.NewError(shoal.ErrorConflict, err.Error()) }
func invalid(msg string) error  { return shoal.NewError(shoal.ErrorInvalidArgument, msg) }
func notFound(msg string) error { return shoal.NewError(shoal.ErrorNotFound, msg) }
func unavailable(msg string) error {
	return shoal.NewError(shoal.ErrorUnavailable, msg)
}

// classify maps package errors onto wire codes while keeping the message
// verbatim, so a refusal reason reaches the console unchanged.
func classify(err error) error {
	switch {
	case err == nil:
		return nil
	case shoal.IsErrorCode(err, shoal.ErrorInvalidArgument), shoal.IsErrorCode(err, shoal.ErrorConflict), shoal.IsErrorCode(err, shoal.ErrorNotFound), shoal.IsErrorCode(err, shoal.ErrorUnavailable):
		return err
	case errors.Is(err, weightsbundle.ErrRefused), errors.Is(err, weightsbundle.ErrIneligible), errors.Is(err, weightsbundle.ErrConflict), errors.Is(err, weightsbundle.ErrNoActiveRelease),
		errors.Is(err, distillation.ErrRefused), errors.Is(err, releasebundle.ErrRefused), errors.Is(err, weightsbundle.ErrVerification), errors.Is(err, releasebundle.ErrVerification), errors.Is(err, weightsbundle.ErrExists):
		return refused(err)
	case errors.Is(err, distillation.ErrInvalid), errors.Is(err, weightsbundle.ErrInvalidManifest):
		return invalid(err.Error())
	}
	return unavailable(err.Error())
}

func (s *Service) recordPath(kind, id string) string {
	p := path.Join("records", kind, id+".json")
	if s.cfg.Prefix != "" {
		return s.cfg.Prefix + "/" + p
	}
	return p
}

func (s *Service) put(ctx context.Context, kind, id string, value any) error {
	raw, err := weightsbundle.Canonical(value)
	if err != nil {
		return err
	}
	w, err := s.cfg.Store.CreateExclusive(ctx, s.recordPath(kind, id))
	if err != nil {
		return err
	}
	if _, err := w.Write(raw); err != nil {
		if a, ok := w.(storage.Aborter); ok {
			a.Abort()
		}
		return err
	}
	return w.Close()
}

func (s *Service) get(ctx context.Context, kind, id string) (weightsbundle.Record, error) {
	f, err := s.cfg.Store.Open(ctx, s.recordPath(kind, id))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, notFound(kind + " " + id + " is not recorded")
		}
		return nil, err
	}
	defer f.Close()
	raw := make([]byte, f.Size())
	if _, err := f.ReadAt(raw, 0); err != nil && f.Size() != 0 {
		return nil, err
	}
	return weightsbundle.DecodeRecord(raw)
}

func (s *Service) list(ctx context.Context, kind string) ([]weightsbundle.Record, error) {
	paths, err := s.cfg.Store.List(ctx, s.recordPath(kind, "")[:len(s.recordPath(kind, ""))-len(".json")])
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	out := make([]weightsbundle.Record, 0, len(paths))
	for _, p := range paths {
		if !strings.HasSuffix(p, ".json") {
			continue
		}
		id := strings.TrimSuffix(path.Base(p), ".json")
		r, err := s.get(ctx, kind, id)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func marshal(v any) (json.RawMessage, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

func str(r weightsbundle.Record, key string) string { s, _ := r[key].(string); return s }

var safeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// Datasets lists registered cohorts with the latest recorded export for each.
func (s *Service) Datasets(ctx context.Context) (json.RawMessage, error) {
	exports, err := s.list(ctx, kindExport)
	if err != nil {
		return nil, classify(err)
	}
	latest := map[string]weightsbundle.Record{}
	for _, e := range exports {
		cohort := str(e, "cohort_id")
		if prior, ok := latest[cohort]; !ok || str(e, "exported_at") > str(prior, "exported_at") {
			latest[cohort] = e
		}
	}
	out := map[string]any{"authority_mounted": s.cfg.Cohorts != nil, "cohorts": []any{}, "exports": exports}
	if s.cfg.Cohorts == nil {
		out["note"] = "No cohort authority is mounted on this host; recorded exports are listed, no cohorts are implied."
		return marshal(out)
	}
	cohorts, err := s.cfg.Cohorts.ListCohorts(ctx)
	if err != nil {
		return nil, classify(err)
	}
	rows := make([]any, 0, len(cohorts))
	for _, c := range cohorts {
		row := map[string]any{"cohort": c, "latest_export": nil}
		if e, ok := latest[string(c.ID)]; ok {
			row["latest_export"] = e
		}
		rows = append(rows, row)
	}
	out["cohorts"] = rows
	return marshal(out)
}

// RequestExport asks the authority for an authorized export and records its
// manifest pin and quarantine counts. The bytes are not returned here; the
// dataset-exports route serves them.
func (s *Service) RequestExport(ctx context.Context, cohortID string) (json.RawMessage, error) {
	if s.cfg.Exporter == nil {
		return nil, unavailable("no dataset exporter is mounted on this host")
	}
	if cohortID == "" {
		return nil, invalid("cohort_id is required")
	}
	export, err := s.cfg.Exporter.Export(ctx, shoal.ID(cohortID))
	if err != nil {
		return nil, classify(err)
	}
	manifest, err := weightsbundle.DecodeRecord(export.Manifest)
	if err != nil {
		return nil, classify(err)
	}
	lineage, err := weightsbundle.SourcesFromExportManifest(export.Manifest)
	if err != nil {
		return nil, classify(err)
	}
	quarantine := map[string]int{}
	var exampleIDs []string
	rows, _ := manifest["rows"].([]any)
	for _, row := range rows {
		obj, _ := row.(map[string]any)
		reasons, _ := obj["exclusion_reasons"].([]any)
		if len(reasons) == 0 {
			exampleIDs = append(exampleIDs, str(obj, "id"))
			continue
		}
		for _, r := range reasons {
			quarantine[fmt.Sprint(r)]++
		}
	}
	sort.Strings(exampleIDs)
	record := map[string]any{
		"schema": 1, "kind": kindExport, "cohort_id": str(manifest, "cohort_id"), "task_id": str(manifest, "task_id"),
		"label_policy_id": str(manifest, "label_policy_id"), "manifest_sha256": lineage.DatasetManifestSHA256,
		"dataset_sha256": str(manifest, "dataset_sha256"), "rows": len(rows), "eligible_example_ids": exampleIDs,
		"quarantine": quarantine, "source_ids": lineage.SourceIDs, "exported_at": s.cfg.Now().UTC().Format(time.RFC3339Nano),
	}
	id := lineage.DatasetManifestSHA256
	if err := s.put(ctx, kindExport, id, record); err != nil && !errors.Is(err, storage.ErrExists) {
		return nil, classify(err)
	}
	return marshal(record)
}

// SubmitJob records a distillation manifest and the job over it. Every
// admitted cohort must have a recorded export at the pinned digest; the
// export is authorized against the selection before the job exists.
func (s *Service) SubmitJob(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	manifest, err := distillation.Decode(raw)
	if err != nil {
		return nil, classify(err)
	}
	pin, _ := manifest.Pin()
	var descriptors []distillation.ExportDescriptor
	for _, e := range manifest.Exports {
		export, err := s.get(ctx, kindExport, e.ManifestSHA256)
		if err != nil {
			return nil, refused(fmt.Errorf("cohort %q has no recorded export at pin %s", e.CohortID, e.ManifestSHA256))
		}
		var ids []string
		for _, v := range toSlice(export["eligible_example_ids"]) {
			ids = append(ids, fmt.Sprint(v))
		}
		descriptors = append(descriptors, distillation.ExportDescriptor{CohortID: str(export, "cohort_id"), TaskID: str(export, "task_id"), LabelPolicyID: str(export, "label_policy_id"), ManifestSHA256: str(export, "manifest_sha256"), ExampleIDs: ids})
	}
	job, err := distillation.NewJob(manifest, descriptors)
	if err != nil {
		return nil, classify(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	encoded, _ := manifest.Encode()
	generic, err := weightsbundle.DecodeRecord(encoded)
	if err != nil {
		return nil, classify(err)
	}
	if err := s.put(ctx, kindManifest, pin, generic); err != nil && !errors.Is(err, storage.ErrExists) {
		return nil, classify(err)
	}
	record := map[string]any{"schema": 1, "kind": kindJob, "manifest_pin": pin, "status": "submitted", "submitted_at": s.cfg.Now().UTC().Format(time.RFC3339Nano), "requested": summarize(manifest), "exports": job.Exports}
	if err := s.put(ctx, kindJob, pin, record); err != nil {
		if errors.Is(err, storage.ErrExists) {
			return nil, refused(fmt.Errorf("a job for manifest %s is already recorded", pin))
		}
		return nil, classify(err)
	}
	return marshal(record)
}

func summarize(m distillation.Manifest) map[string]any {
	teacher, _ := m.Teacher.Eligible[0], 0
	for _, c := range m.Teacher.Eligible {
		if c.Alias == m.Teacher.Chosen {
			teacher = c
		}
	}
	base := m.Base.Eligible[0]
	for _, c := range m.Base.Eligible {
		if c.Alias == m.Base.Chosen {
			base = c
		}
	}
	return map[string]any{
		"tasks": m.Data.Tasks, "evidence_families": m.Data.EvidenceFamilies, "prompt_version": m.Data.PromptVersion,
		"generation_parameters": m.Data.GenerationParameters, "held_out_count": len(m.Data.HeldOutExampleIDs),
		"teacher":                      map[string]any{"alias": teacher.Alias, "identity": teacher.Identity, "rule": m.Teacher.Rule, "rejected": m.Teacher.Rejected},
		"base":                         map[string]any{"alias": base.Alias, "identity": base.Identity, "rule": m.Base.Rule, "rejected": m.Base.Rejected},
		"teacher_egress_at_generation": m.TeacherEgressAtGeneration,
	}
}

func toSlice(v any) []any { s, _ := v.([]any); return s }

// Jobs lists every job with its receipt when one was submitted.
func (s *Service) Jobs(ctx context.Context) (json.RawMessage, error) {
	jobs, err := s.list(ctx, kindJob)
	if err != nil {
		return nil, classify(err)
	}
	out := make([]any, 0, len(jobs))
	for _, j := range jobs {
		view, err := s.jobView(ctx, j)
		if err != nil {
			return nil, err
		}
		out = append(out, view)
	}
	return marshal(map[string]any{"jobs": out})
}

// Job returns one job: manifest, requested versus actual, executor
// provenance, and the receipt.
func (s *Service) Job(ctx context.Context, pin string) (json.RawMessage, error) {
	if !safeID.MatchString(pin) {
		return nil, invalid("manifest pin")
	}
	job, err := s.get(ctx, kindJob, pin)
	if err != nil {
		return nil, classify(err)
	}
	view, err := s.jobView(ctx, job)
	if err != nil {
		return nil, err
	}
	manifest, err := s.get(ctx, kindManifest, pin)
	if err != nil {
		return nil, classify(err)
	}
	view["manifest"] = manifest
	return marshal(view)
}

func (s *Service) jobView(ctx context.Context, job weightsbundle.Record) (map[string]any, error) {
	view := map[string]any{"job": job, "receipt": nil, "requested": job["requested"], "actual": nil, "executor": nil, "deviations": nil}
	receipt, err := s.get(ctx, kindReceipt, str(job, "manifest_pin"))
	if err == nil {
		view["receipt"] = receipt
		view["actual"] = map[string]any{"parameters": receipt["actual_parameters"], "cohorts": receipt["actual_cohorts"], "teacher": receipt["actual_teacher"], "base": receipt["actual_base"], "status": receipt["status"], "bundle_digest": receipt["bundle_digest"]}
		view["executor"] = receipt["executor"]
		view["deviations"] = receipt["deviations"]
	} else if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		return nil, classify(err)
	}
	return view, nil
}

// SubmitReceipt verifies a trainer receipt against the recorded manifest.
func (s *Service) SubmitReceipt(ctx context.Context, pin string, raw json.RawMessage) (json.RawMessage, error) {
	if !safeID.MatchString(pin) {
		return nil, invalid("manifest pin")
	}
	manifestRecord, err := s.get(ctx, kindManifest, pin)
	if err != nil {
		return nil, classify(err)
	}
	if _, err := s.get(ctx, kindJob, pin); err != nil {
		return nil, classify(err)
	}
	encoded, _ := weightsbundle.Canonical(manifestRecord)
	manifest, err := distillation.Decode(encoded)
	if err != nil {
		return nil, classify(err)
	}
	var receipt distillation.Receipt
	if err := weightsbundle.StrictUnmarshal(raw, &receipt); err != nil {
		return nil, invalid("receipt: " + err.Error())
	}
	if err := distillation.VerifyReceipt(manifest, receipt); err != nil {
		return nil, classify(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.put(ctx, kindReceipt, pin, receipt); err != nil {
		if errors.Is(err, storage.ErrExists) {
			return nil, refused(fmt.Errorf("a receipt for manifest %s is already recorded", pin))
		}
		return nil, classify(err)
	}
	record, _ := s.get(ctx, kindReceipt, pin)
	return marshal(map[string]any{"receipt": record, "status": receipt.Status, "candidate_only": true, "note": "Completion of a run is not improvement; evaluation is a separate record."})
}

// SubmitCandidate records a sealed candidate and its artifact. The artifact
// must bind a bundle that verifies on the serving root, and a candidate
// bound to a distillation manifest must stay inside that selection.
func (s *Service) SubmitCandidate(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var body struct {
		Candidate weightsbundle.Record `json:"candidate"`
		Artifact  weightsbundle.Record `json:"artifact"`
	}
	if err := weightsbundle.StrictUnmarshal(raw, &body); err != nil {
		return nil, invalid("body: " + err.Error())
	}
	if err := weightsbundle.VerifyRecord(body.Candidate, kindCandidate); err != nil {
		return nil, invalid(err.Error())
	}
	if err := weightsbundle.VerifyRecord(body.Artifact, kindArtifact); err != nil {
		return nil, invalid(err.Error())
	}
	if str(body.Artifact, "candidate_id") != str(body.Candidate, "id") {
		return nil, refused(errors.New("artifact is not bound to the candidate"))
	}
	manifest, err := weightsbundle.ArtifactManifest(body.Artifact)
	if err != nil {
		return nil, classify(err)
	}
	digest, _ := manifest.Digest()
	if _, got, err := weightsbundle.Load(filepath.Join(s.cfg.ServingRoot, digest)); err != nil {
		return nil, refused(fmt.Errorf("bundle %s is not published on the serving root: %v", digest, err))
	} else if got != digest {
		return nil, refused(fmt.Errorf("published bundle digest %s differs from artifact %s", got, digest))
	}
	if pin := str(body.Candidate, "training_manifest_sha256"); pin != "" {
		if record, err := s.get(ctx, kindManifest, pin); err == nil {
			encoded, _ := weightsbundle.Canonical(record)
			if m, err := distillation.Decode(encoded); err == nil {
				if err := distillation.CheckCandidateScope(m, body.Candidate); err != nil {
					return nil, classify(err)
				}
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.put(ctx, kindCandidate, str(body.Candidate, "id"), body.Candidate); err != nil && !errors.Is(err, storage.ErrExists) {
		return nil, classify(err)
	}
	if err := s.put(ctx, kindArtifact, str(body.Artifact, "id"), body.Artifact); err != nil && !errors.Is(err, storage.ErrExists) {
		return nil, classify(err)
	}
	return marshal(map[string]any{"candidate": body.Candidate, "artifact": body.Artifact, "bundle_digest": digest})
}

// Candidates lists candidates with bundle digests and lineage.
func (s *Service) Candidates(ctx context.Context) (json.RawMessage, error) {
	candidates, err := s.list(ctx, kindCandidate)
	if err != nil {
		return nil, classify(err)
	}
	artifacts, err := s.list(ctx, kindArtifact)
	if err != nil {
		return nil, classify(err)
	}
	evaluations, err := s.list(ctx, kindEvaluation)
	if err != nil {
		return nil, classify(err)
	}
	out := make([]any, 0, len(candidates))
	for _, c := range candidates {
		id := str(c, "id")
		view := map[string]any{"candidate": c, "artifacts": []any{}, "evaluations": []any{}, "lineage": map[string]any{"dataset_id": c["dataset_id"], "dataset_manifest_sha256": c["dataset_manifest_sha256"], "training_manifest_sha256": c["training_manifest_sha256"], "task_ids": c["task_ids"], "model_digest": c["model_digest"]}}
		for _, a := range artifacts {
			if str(a, "candidate_id") == id {
				view["artifacts"] = append(view["artifacts"].([]any), map[string]any{"id": a["id"], "bundle_digest": a["bundle_digest"], "bundle_files": a["bundle_files"], "evaluation_id": a["evaluation_id"], "runtime_digest": a["runtime_digest"]})
			}
		}
		for _, e := range evaluations {
			if str(e, "candidate_id") == id {
				view["evaluations"] = append(view["evaluations"].([]any), evaluationView(e))
			}
		}
		out = append(out, view)
	}
	return marshal(map[string]any{"candidates": out})
}

func evaluationView(e weightsbundle.Record) map[string]any {
	return map[string]any{"id": e["id"], "disposition": e["disposition"], "reasons": e["reasons"], "verdicts": e["metrics"], "families": e["families"], "evaluation_policy_id": e["evaluation_policy_id"], "predictions_id": e["predictions_id"], "permitted_action": e["permitted_action"], "limitation": e["limitation"], "record": e}
}

// SubmitEvaluation records a sealed promotion_record for a recorded candidate.
func (s *Service) SubmitEvaluation(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	record, err := weightsbundle.DecodeRecord(raw)
	if err != nil {
		return nil, invalid(err.Error())
	}
	if err := weightsbundle.VerifyRecord(record, kindEvaluation); err != nil {
		return nil, invalid(err.Error())
	}
	if _, err := s.get(ctx, kindCandidate, str(record, "candidate_id")); err != nil {
		return nil, classify(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.put(ctx, kindEvaluation, str(record, "id"), record); err != nil && !errors.Is(err, storage.ErrExists) {
		return nil, classify(err)
	}
	return marshal(evaluationView(record))
}

// Evaluations lists the verdicts recorded for a candidate: disposition,
// per-assessor results, reserved test membership and leakage reasons.
func (s *Service) Evaluations(ctx context.Context, candidateID string) (json.RawMessage, error) {
	if !safeID.MatchString(candidateID) {
		return nil, invalid("candidate id")
	}
	evaluations, err := s.list(ctx, kindEvaluation)
	if err != nil {
		return nil, classify(err)
	}
	out := []any{}
	for _, e := range evaluations {
		if str(e, "candidate_id") == candidateID {
			out = append(out, evaluationView(e))
		}
	}
	return marshal(map[string]any{"candidate_id": candidateID, "evaluations": out, "disclaimer": Disclaimer})
}

// Releases reports the active release, the exact bundle digest served now,
// the history, eligibility, and events.
func (s *Service) Releases(ctx context.Context) (json.RawMessage, error) {
	r := s.cfg.Registry
	out := map[string]any{"disclaimer": Disclaimer, "active": nil, "history": []any{}, "events": r.Events(), "serving": nil}
	if active, ok := r.Active(); ok {
		out["active"] = map[string]any{"release": active, "eligibility": r.EligibilityOf(active.ID), "bundle_digest_served_now": active.BundleDigest}
	}
	history := []any{}
	for _, rel := range r.History() {
		history = append(history, map[string]any{"release": rel, "eligibility": r.EligibilityOf(rel.ID)})
	}
	out["history"] = history
	if s.cfg.Serving != nil {
		report, err := s.cfg.Serving.ServingReport(ctx)
		if err != nil {
			out["serving"] = map[string]any{"error": err.Error()}
		} else {
			out["serving"] = report
		}
	} else {
		out["serving_note"] = "No student runtime is configured on this host; the active release is recorded, not served here."
	}
	return marshal(out)
}

// Promote builds the bound approval the records require and promotes a
// recorded candidate through the registry. A missing evaluation or an
// evaluation that did not pass is refused with the record's reason.
func (s *Service) Promote(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var body struct {
		CandidateID  string `json:"candidate_id"`
		EvaluationID string `json:"evaluation_id"`
		ArtifactID   string `json:"artifact_id"`
		Owner        string `json:"owner"`
	}
	if err := weightsbundle.StrictUnmarshal(raw, &body); err != nil {
		return nil, invalid("body: " + err.Error())
	}
	if body.CandidateID == "" || body.Owner == "" {
		return nil, invalid("candidate_id and owner are required")
	}
	candidate, err := s.get(ctx, kindCandidate, body.CandidateID)
	if err != nil {
		return nil, classify(err)
	}
	if body.EvaluationID == "" {
		return nil, refused(errors.New("promotion requires its evaluation: no evaluation_id was supplied"))
	}
	evaluation, err := s.get(ctx, kindEvaluation, body.EvaluationID)
	if err != nil {
		return nil, refused(fmt.Errorf("promotion requires its evaluation: evaluation %s is not recorded", body.EvaluationID))
	}
	artifacts, err := s.list(ctx, kindArtifact)
	if err != nil {
		return nil, classify(err)
	}
	var artifact weightsbundle.Record
	for _, a := range artifacts {
		if str(a, "candidate_id") == body.CandidateID && str(a, "evaluation_id") == body.EvaluationID && (body.ArtifactID == "" || str(a, "id") == body.ArtifactID) {
			artifact = a
		}
	}
	if artifact == nil {
		return nil, refused(errors.New("no artifact binds this candidate to this evaluation"))
	}
	approval, err := weightsbundle.NewPromotionApproval(body.CandidateID, body.EvaluationID, body.Owner)
	if err != nil {
		return nil, classify(err)
	}
	lineage := weightsbundle.DatasetLineage{}
	if pin := str(candidate, "dataset_manifest_sha256"); pin != "" {
		export, err := s.get(ctx, kindExport, pin)
		if err != nil {
			return nil, refused(fmt.Errorf("candidate dataset manifest %s has no recorded export; lineage cannot be bound", pin))
		}
		lineage.DatasetManifestSHA256 = pin
		for _, v := range toSlice(export["source_ids"]) {
			lineage.SourceIDs = append(lineage.SourceIDs, fmt.Sprint(v))
		}
	}
	in := weightsbundle.PromotionInput{Candidate: candidate, Evaluation: evaluation, Artifact: artifact, Approval: approval}
	if active, ok := s.cfg.Registry.Active(); ok {
		in.Predecessor = &active
	}
	release, err := s.cfg.Registry.Promote(ctx, in, lineage)
	if err != nil {
		return nil, classify(err)
	}
	return marshal(map[string]any{"release": release, "approval": approval, "disclaimer": Disclaimer})
}

// Rollback restores a prior release under a bound rollback approval.
func (s *Service) Rollback(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var body struct {
		PriorReleaseID string `json:"prior_release_id"`
		Owner          string `json:"owner"`
	}
	if err := weightsbundle.StrictUnmarshal(raw, &body); err != nil {
		return nil, invalid("body: " + err.Error())
	}
	if body.PriorReleaseID == "" || body.Owner == "" {
		return nil, invalid("prior_release_id and owner are required")
	}
	active, ok := s.cfg.Registry.Active()
	if !ok {
		return nil, refused(weightsbundle.ErrNoActiveRelease)
	}
	approval, err := weightsbundle.NewRollbackApproval(active.ID, body.PriorReleaseID, body.Owner)
	if err != nil {
		return nil, classify(err)
	}
	release, err := s.cfg.Registry.Rollback(ctx, body.PriorReleaseID, approval)
	if err != nil {
		return nil, classify(err)
	}
	return marshal(map[string]any{"release": release, "approval": approval})
}

// Evidence lists the file-backed sealed reports of one kind. Reports are
// read, never written, and an unreadable report is reported as such.
func (s *Service) Evidence(ctx context.Context, kind string) (json.RawMessage, error) {
	known := false
	for _, k := range EvidenceKinds {
		known = known || k == kind
	}
	if !known {
		return nil, invalid("evidence kind must be one of " + strings.Join(EvidenceKinds, ", "))
	}
	if s.cfg.EvidenceDir == "" {
		return nil, unavailable("no evidence directory is configured on this host")
	}
	dir := filepath.Join(s.cfg.EvidenceDir, kind)
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, classify(err)
	}
	reports := []any{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		item := map[string]any{"file": e.Name()}
		if err != nil {
			item["error"] = err.Error()
		} else if record, err := weightsbundle.DecodeRecord(raw); err != nil {
			item["error"] = err.Error()
		} else {
			item["record"] = record
			item["kind"] = record["kind"]
			item["id"] = record["id"]
			if recordKind := str(record, "kind"); recordKind != "" {
				item["sealed"] = weightsbundle.VerifyRecord(record, recordKind) == nil
			}
		}
		reports = append(reports, item)
	}
	return marshal(map[string]any{"kind": kind, "reports": reports, "read_only": true})
}

// Bundles lists offline release bundles present in the bundles directory
// with their verification result per entry.
func (s *Service) Bundles(ctx context.Context) (json.RawMessage, error) {
	entries, err := os.ReadDir(s.cfg.BundlesDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, classify(err)
	}
	out := []any{}
	for _, e := range entries {
		if !e.IsDir() || strings.HasSuffix(e.Name(), ".exporting") {
			continue
		}
		out = append(out, s.bundleView(e.Name()))
	}
	return marshal(map[string]any{"bundles": out, "images": s.cfg.Images})
}

func (s *Service) bundleView(name string) map[string]any {
	verified, err := releasebundle.Verify(filepath.Join(s.cfg.BundlesDir, name))
	view := map[string]any{"name": name, "verified": err == nil, "entries": verified.Results, "release_id": verified.Index.ReleaseID, "bundle_digest": verified.Index.BundleDigest, "index_digest": verified.IndexDigest, "images": verified.Index.Images}
	if err != nil {
		view["error"] = err.Error()
	}
	return view
}

// ExportBundle writes an offline release bundle for a recorded release.
func (s *Service) ExportBundle(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var body struct {
		ReleaseID string `json:"release_id"`
		Name      string `json:"name"`
	}
	if err := weightsbundle.StrictUnmarshal(raw, &body); err != nil {
		return nil, invalid("body: " + err.Error())
	}
	if !safeID.MatchString(body.Name) {
		return nil, invalid("name must be a safe directory name")
	}
	r := s.cfg.Registry
	release, ok := r.Get(body.ReleaseID)
	if !ok {
		return nil, notFound("release " + body.ReleaseID + " is not recorded")
	}
	if e := r.EligibilityOf(release.ID); e.Status != weightsbundle.EligibilityEligible {
		return nil, refused(fmt.Errorf("%w: %s", weightsbundle.ErrIneligible, e.Reason))
	}
	if release.BundleDigest == nil {
		return nil, refused(errors.New("release has no weights bundle"))
	}
	lineage, _ := r.Lineage(release.ID)
	evaluation, err := s.get(ctx, kindEvaluation, release.EvaluationID)
	if err != nil {
		return nil, classify(err)
	}
	export, err := s.get(ctx, kindExport, lineage.DatasetManifestSHA256)
	if err != nil {
		return nil, refused(errors.New("the release's dataset export is not recorded on this host"))
	}
	datasetManifest, err := s.exportManifestBytes(ctx, export)
	if err != nil {
		return nil, err
	}
	if len(s.cfg.Images) == 0 {
		return nil, unavailable("no image digests are configured on this host; an offline bundle needs the runtime image pinned by digest")
	}
	if err := os.MkdirAll(s.cfg.BundlesDir, 0o700); err != nil {
		return nil, classify(err)
	}
	index, err := releasebundle.Export(releasebundle.ExportInput{Release: release, Lineage: lineage, BundleRoot: filepath.Join(s.cfg.ServingRoot, *release.BundleDigest), DatasetManifest: datasetManifest, Evaluation: evaluation, Images: s.cfg.Images, Now: s.cfg.Now()}, filepath.Join(s.cfg.BundlesDir, body.Name))
	if err != nil {
		return nil, classify(err)
	}
	view := s.bundleView(body.Name)
	view["index"] = index
	return marshal(view)
}

// exportManifestBytes re-obtains the authorized manifest bytes for a recorded
// export through the exporter, verifying the pin, since the service records
// pins and counts rather than copies of exports.
func (s *Service) exportManifestBytes(ctx context.Context, export weightsbundle.Record) ([]byte, error) {
	if s.cfg.Exporter == nil {
		return nil, unavailable("no dataset exporter is mounted on this host; the dataset manifest cannot be re-obtained for export")
	}
	result, err := s.cfg.Exporter.Export(ctx, shoal.ID(str(export, "cohort_id")))
	if err != nil {
		return nil, classify(err)
	}
	if weightsbundle.SHA256Hex(result.Manifest) != str(export, "manifest_sha256") {
		return nil, refused(errors.New("the authority's current export no longer matches the recorded manifest pin"))
	}
	return result.Manifest, nil
}

// ImportBundle imports an offline release bundle from the bundles directory.
func (s *Service) ImportBundle(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var body struct {
		Name     string `json:"name"`
		Importer string `json:"importer"`
	}
	if err := weightsbundle.StrictUnmarshal(raw, &body); err != nil {
		return nil, invalid("body: " + err.Error())
	}
	if !safeID.MatchString(body.Name) || body.Importer == "" {
		return nil, invalid("name must be a safe directory name and importer is required")
	}
	result, err := releasebundle.Import(ctx, filepath.Join(s.cfg.BundlesDir, body.Name), releasebundle.ImportOptions{Registry: s.cfg.Registry, ServingRoot: s.cfg.ServingRoot, Importer: body.Importer, Now: s.cfg.Now()})
	view := map[string]any{"name": body.Name, "entries": result.Verified.Results, "activated": result.Activated, "already_present": result.AlreadyThere, "serving_path": result.ServingPath, "release_id": result.Verified.Index.ReleaseID, "bundle_digest": result.Verified.Index.BundleDigest}
	if err != nil {
		view["error"] = err.Error()
		encoded, _ := json.Marshal(view)
		return encoded, classify(err)
	}
	if err := s.put(ctx, kindEvaluation, str(result.Verified.Evaluation, "id"), result.Verified.Evaluation); err != nil && !errors.Is(err, storage.ErrExists) {
		return nil, classify(err)
	}
	return marshal(view)
}
