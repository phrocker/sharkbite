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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	decisionapi "github.com/phrocker/shoal-oss/pkg/decision/api"
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
	kindPredictions = distillation.KindPredictions
	kindExport      = "dataset-export"
	// kindExportManifest holds the authorized manifest bytes verbatim under
	// their pin, so an offline bundle export never depends on the authority
	// reproducing them.
	kindExportManifest = "dataset-export-manifest"
	kindManifest       = "distillation-manifest"
	kindJob            = "distillation-job"
	kindReceipt        = "distillation-receipt"
	kindCandidate      = weightsbundle.KindCandidate
	kindArtifact       = weightsbundle.KindArtifact
	kindEvaluation     = weightsbundle.KindEvaluation
	kindPolicy         = weightsbundle.KindEvaluationPolicy
	// kindPolicyPointer records, under the manifest pin, which sealed policy
	// was declared for the job and when; the sealed policy lives under its
	// own identity, as every sealed record does, shared by every job judged
	// under the same gates.
	kindPolicyPointer = "evaluation-policy-pointer"
)

// Cohort is what a cohort authority lists.
type Cohort struct {
	ID            shoal.ID `json:"cohort_id"`
	TaskID        string   `json:"task_id"`
	LabelPolicyID string   `json:"label_policy_id"`
	Members       int      `json:"members"`
	Mode          string   `json:"provenance_mode"`
	// Cutoff is the cohort's declared label time, RFC 3339, when the
	// authority states it; a class record's window ends at or before it.
	Cutoff string `json:"cutoff,omitempty"`
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

// EgressResolver resolves the egress posture of a model identity from the
// host's configuration. A manifest author declares a posture; the host
// decides what it is, and a declaration the host does not confirm is refused.
type EgressResolver interface {
	ResolveEgress(ctx context.Context, identity distillation.ModelIdentity) (string, error)
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
	// Egress resolves every eligible model's posture before a job is
	// recorded. Without one, no job can be submitted on this host.
	Egress EgressResolver
	// AllowOffHostModels admits a job whose chosen teacher or base resolves
	// to an off-host posture. The default refuses it: generation or
	// fine-tuning there sends every training example off the host.
	AllowOffHostModels bool
	// Log receives the causes of internal failures that are reported to
	// callers only generically.
	Log io.Writer
}

// Service implements decisionapi.LearningProvider.
type Service struct {
	cfg Config
	mu  sync.Mutex
	// runs indexes the run records (run.go).
	runs runIndex
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
	if cfg.Log == nil {
		cfg.Log = io.Discard
	}
	cfg.Prefix = strings.TrimRight(cfg.Prefix, "/")
	return &Service{cfg: cfg}, nil
}

// GenericUnavailable is what a caller sees when the service itself failed:
// the cause goes to the host log, never to the wire, since store paths and
// decoder messages describe the host, not the request.
const GenericUnavailable = "learning service could not complete the request; the cause is in the host log"

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
	return unavailable(GenericUnavailable)
}

func (s *Service) classified(err error) error { return s.classifyLogged("request", err) }

// classifyLogged is classify for errors whose cause belongs in the host log,
// with every host path redacted from what reaches the wire: a refusal that
// names a file under the bundles, serving, evidence, or state root names it
// by role, and the full path goes to the log.
func (s *Service) classifyLogged(op string, err error) error {
	out := classify(err)
	if shoal.IsErrorCode(out, shoal.ErrorUnavailable) && out.Error() != err.Error() {
		fmt.Fprintf(s.cfg.Log, "learning: %s: %v\n", op, err)
		return out
	}
	se := new(shoal.Error)
	if errors.As(out, &se) {
		if redacted := s.redact(se.Message); redacted != se.Message {
			fmt.Fprintf(s.cfg.Log, "learning: %s: %v\n", op, err)
			return shoal.NewError(se.Code, redacted)
		}
	}
	return out
}

// redact replaces the host's configured roots in a message with their role.
func (s *Service) redact(msg string) string {
	for _, root := range []struct{ path, role string }{
		{s.cfg.BundlesDir, "<bundles>"}, {s.cfg.ServingRoot, "<serving>"}, {s.cfg.EvidenceDir, "<evidence>"}, {s.cfg.Prefix, "<state>"},
	} {
		if root.path == "" {
			continue
		}
		msg = strings.ReplaceAll(msg, root.path, root.role)
		if abs, err := filepath.Abs(root.path); err == nil && abs != root.path {
			msg = strings.ReplaceAll(msg, abs, root.role)
		}
	}
	return msg
}

// redactErr is redact for a message that is sent as data, not as an error.
func (s *Service) redactErr(op string, err error) string {
	redacted := s.redact(err.Error())
	if redacted != err.Error() {
		fmt.Fprintf(s.cfg.Log, "learning: %s: %v\n", op, err)
	}
	return redacted
}

// recordID is the only shape a stored record's key may take: the 64-hex
// content address every sealed record and pin carries. Anything else in a
// path position is refused before it reaches the store.
var recordID = regexp.MustCompile(`^[0-9a-f]{64}$`)

func checkRecordID(id string) error {
	if !recordID.MatchString(id) {
		return invalid("record id must be a 64-hex content address")
	}
	return nil
}

// actorFor returns the authenticated actor a write is bound to. A body may
// name the owner only to confirm it; it can never choose one.
func actorFor(ctx context.Context, claimed string) (string, error) {
	actor, ok := decisionapi.LearningActorFrom(ctx)
	if !ok {
		return "", shoal.NewError(shoal.ErrorUnauthorized, "learning writes are bound to an authenticated actor")
	}
	if claimed != "" && claimed != actor.Actor {
		return "", invalid("owner must be the authenticated actor " + actor.Actor + ", not " + claimed)
	}
	return actor.Actor, nil
}

func (s *Service) recordPath(kind, id string) string {
	p := path.Join("records", kind, id+".json")
	if s.cfg.Prefix != "" {
		return s.cfg.Prefix + "/" + p
	}
	return p
}

func (s *Service) put(ctx context.Context, kind, id string, value any) error {
	if err := checkRecordID(id); err != nil {
		return err
	}
	raw, err := weightsbundle.Canonical(value)
	if err != nil {
		return err
	}
	w, err := s.cfg.Store.CreateExclusive(ctx, s.recordPath(kind, id))
	if err != nil {
		return err
	}
	if n, err := w.Write(raw); err != nil || n != len(raw) {
		if a, ok := w.(storage.Aborter); ok {
			_ = a.Abort()
		}
		if err == nil {
			err = io.ErrShortWrite
		}
		return err
	}
	return w.Close()
}

// putRaw stores bytes verbatim under a kind and content address. Unlike put
// it does not canonicalize: the bytes are the sealed thing, and their digest
// must stay the key that names them.
func (s *Service) putRaw(ctx context.Context, kind, id string, raw []byte) error {
	if err := checkRecordID(id); err != nil {
		return err
	}
	if int64(len(raw)) > weightsbundle.MaxRecordBytes {
		return fmt.Errorf("%s %s exceeds %d bytes", kind, id, weightsbundle.MaxRecordBytes)
	}
	w, err := s.cfg.Store.CreateExclusive(ctx, s.recordPath(kind, id))
	if err != nil {
		return err
	}
	if n, err := w.Write(raw); err != nil || n != len(raw) {
		if a, ok := w.(storage.Aborter); ok {
			_ = a.Abort()
		}
		if err == nil {
			err = io.ErrShortWrite
		}
		return err
	}
	return w.Close()
}

// getRaw reads bytes stored by putRaw and verifies they are the bytes the
// key names: a file under the key whose digest is not the key is not them.
func (s *Service) getRaw(ctx context.Context, kind, id string) ([]byte, error) {
	if err := checkRecordID(id); err != nil {
		return nil, err
	}
	f, err := s.cfg.Store.Open(ctx, s.recordPath(kind, id))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, notFound(kind + " " + id + " is not recorded")
		}
		return nil, err
	}
	defer f.Close()
	if f.Size() < 0 || f.Size() > weightsbundle.MaxRecordBytes {
		return nil, fmt.Errorf("%s %s exceeds %d bytes", kind, id, weightsbundle.MaxRecordBytes)
	}
	raw := make([]byte, f.Size())
	if _, err := f.ReadAt(raw, 0); err != nil && f.Size() != 0 {
		return nil, err
	}
	if weightsbundle.SHA256Hex(raw) != id {
		return nil, refused(fmt.Errorf("%s %s does not hash to its key", kind, id))
	}
	return raw, nil
}

func (s *Service) get(ctx context.Context, kind, id string) (weightsbundle.Record, error) {
	if err := checkRecordID(id); err != nil {
		return nil, err
	}
	f, err := s.cfg.Store.Open(ctx, s.recordPath(kind, id))
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return nil, notFound(kind + " " + id + " is not recorded")
		}
		return nil, err
	}
	defer f.Close()
	if f.Size() > weightsbundle.MaxRecordBytes {
		return nil, fmt.Errorf("%s %s exceeds %d bytes", kind, id, weightsbundle.MaxRecordBytes)
	}
	raw := make([]byte, f.Size())
	if _, err := f.ReadAt(raw, 0); err != nil && f.Size() != 0 {
		return nil, err
	}
	record, err := weightsbundle.DecodeRecord(raw)
	if err != nil {
		return nil, err
	}
	// What the store returns must be the record the key names: a file of
	// another kind, or a sealed record under the wrong key, is not it.
	if str(record, "kind") != kind {
		return nil, notFound(kind + " " + id + " is not recorded")
	}
	if recorded := str(record, "id"); recorded != "" && recorded != id {
		return nil, notFound(kind + " " + id + " is not recorded")
	}
	return record, nil
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
		return nil, s.classified(err)
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
		return nil, s.classified(err)
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
		return nil, s.classified(err)
	}
	manifest, err := weightsbundle.DecodeRecord(export.Manifest)
	if err != nil {
		return nil, s.classified(err)
	}
	lineage, err := weightsbundle.SourcesFromExportManifest(export.Manifest)
	if err != nil {
		return nil, s.classified(err)
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
	// The manifest bytes are kept verbatim under their pin: an authority
	// that regenerates (a fixture under a fresh identity, a cutoff that
	// moves) cannot reproduce them, and the offline bundle needs exactly
	// these bytes to carry the release's lineage.
	if err := s.putRaw(ctx, kindExportManifest, id, export.Manifest); err != nil && !errors.Is(err, storage.ErrExists) {
		return nil, s.classified(err)
	}
	// An existing object or an ambiguous write is only usable if readback
	// proves that this pin names the manifest we just authorized.
	stored, err := s.getRaw(ctx, kindExportManifest, id)
	if err != nil {
		return nil, s.classified(err)
	}
	if !bytes.Equal(stored, export.Manifest) {
		return nil, refused(errors.New("stored export manifest differs from the authorized bytes"))
	}
	if err := s.put(ctx, kindExport, id, record); err != nil && !errors.Is(err, storage.ErrExists) {
		return nil, s.classified(err)
	}
	return marshal(record)
}

// SubmitJob records a distillation manifest and the job over it. Every
// admitted cohort must have a recorded export at the pinned digest; the
// export is authorized against the selection before the job exists.
func (s *Service) SubmitJob(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	manifest, err := distillation.Decode(raw)
	if err != nil {
		return nil, s.classified(err)
	}
	if err := s.resolveEgress(ctx, manifest); err != nil {
		return nil, err
	}
	pin, _ := manifest.Pin()
	var descriptors []distillation.ExportDescriptor
	for _, e := range manifest.Exports {
		export, err := s.get(ctx, kindExport, e.ManifestSHA256)
		if err != nil {
			return nil, refused(fmt.Errorf("cohort %q has no recorded export at pin %s", e.CohortID, e.ManifestSHA256))
		}
		if str(export, "cohort_id") != e.CohortID {
			return nil, refused(fmt.Errorf("export at pin %s is cohort %q, not %q", e.ManifestSHA256, str(export, "cohort_id"), e.CohortID))
		}
		var ids []string
		for _, v := range toSlice(export["eligible_example_ids"]) {
			ids = append(ids, fmt.Sprint(v))
		}
		descriptors = append(descriptors, distillation.ExportDescriptor{CohortID: str(export, "cohort_id"), TaskID: str(export, "task_id"), LabelPolicyID: str(export, "label_policy_id"), ManifestSHA256: str(export, "manifest_sha256"), ExampleIDs: ids})
	}
	job, err := distillation.NewJob(manifest, descriptors)
	if err != nil {
		return nil, s.classified(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	encoded, _ := manifest.Encode()
	generic, err := weightsbundle.DecodeRecord(encoded)
	if err != nil {
		return nil, s.classified(err)
	}
	if err := s.put(ctx, kindManifest, pin, generic); err != nil && !errors.Is(err, storage.ErrExists) {
		return nil, s.classified(err)
	}
	record := map[string]any{"schema": 1, "kind": kindJob, "manifest_pin": pin, "status": "submitted", "submitted_at": s.cfg.Now().UTC().Format(time.RFC3339Nano), "requested": summarize(manifest), "exports": job.Exports}
	if err := s.put(ctx, kindJob, pin, record); err != nil {
		if errors.Is(err, storage.ErrExists) {
			return nil, refused(fmt.Errorf("a job for manifest %s is already recorded", pin))
		}
		return nil, s.classified(err)
	}
	return marshal(record)
}

// resolveEgress confirms every declared egress posture against the host's
// resolver and applies the host's off-host teacher policy. The manifest
// author's declaration is a claim; the host's resolution is the fact.
func (s *Service) resolveEgress(ctx context.Context, m distillation.Manifest) error {
	if s.cfg.Egress == nil {
		return unavailable("no model egress resolver is mounted on this host; a manifest's declared egress cannot be confirmed, so no job can be recorded")
	}
	for _, sel := range []distillation.ModelSelection{m.Teacher, m.Base} {
		for _, c := range sel.Eligible {
			resolved, err := s.cfg.Egress.ResolveEgress(ctx, c.Identity)
			if err != nil {
				return refused(fmt.Errorf("%s %q (%s/%s): egress could not be resolved: %v", sel.Role, c.Alias, c.Identity.Provider, c.Identity.Model, err))
			}
			if resolved != c.Identity.Egress {
				return refused(fmt.Errorf("%s %q (%s/%s) declares egress %q but this host resolves it as %q", sel.Role, c.Alias, c.Identity.Provider, c.Identity.Model, c.Identity.Egress, resolved))
			}
		}
	}
	if s.cfg.AllowOffHostModels {
		return nil
	}
	if m.TeacherEgressAtGeneration == distillation.EgressOffHost {
		return refused(errors.New("the chosen teacher generates off host; this host does not authorize off-host models (every training example would leave the host)"))
	}
	for _, c := range m.Base.Eligible {
		if c.Alias == m.Base.Chosen && c.Identity.Egress == distillation.EgressOffHost {
			return refused(errors.New("the chosen base is fine-tuned off host; this host does not authorize off-host models (every training example would leave the host)"))
		}
	}
	return nil
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
		return nil, s.classified(err)
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
		return nil, s.classified(err)
	}
	view, err := s.jobView(ctx, job)
	if err != nil {
		return nil, err
	}
	manifest, err := s.get(ctx, kindManifest, pin)
	if err != nil {
		return nil, s.classified(err)
	}
	view["manifest"] = manifest
	return marshal(view)
}

func (s *Service) jobView(ctx context.Context, job weightsbundle.Record) (map[string]any, error) {
	view := map[string]any{"job": job, "receipt": nil, "requested": job["requested"], "actual": nil, "executor": nil, "deviations": nil, "predictions_id": nil, "evaluation_policy": nil}
	if policy, err := s.policyFor(ctx, str(job, "manifest_pin")); err == nil {
		view["evaluation_policy"] = policy
	} else if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		return nil, s.classified(err)
	}
	if set, err := s.get(ctx, kindPredictions, str(job, "manifest_pin")); err == nil {
		if id, err := weightsbundle.Digest(set); err == nil {
			view["predictions_id"] = id
			view["predictions_count"] = len(toSlice(set["predictions"]))
		}
	}
	receipt, err := s.get(ctx, kindReceipt, str(job, "manifest_pin"))
	if err == nil {
		view["receipt"] = receipt
		view["actual"] = map[string]any{"parameters": receipt["actual_parameters"], "cohorts": receipt["actual_cohorts"], "teacher": receipt["actual_teacher"], "base": receipt["actual_base"], "status": receipt["status"], "bundle_digest": receipt["bundle_digest"]}
		view["executor"] = receipt["executor"]
		view["deviations"] = receipt["deviations"]
	} else if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		return nil, s.classified(err)
	}
	run, err := s.runView(ctx, str(job, "manifest_pin"))
	if err != nil {
		return nil, err
	}
	view["run"] = run
	return view, nil
}

// recordedJob returns the manifest and job descriptors recorded for a pin.
func (s *Service) recordedJob(ctx context.Context, pin string) (distillation.Manifest, []distillation.ExportDescriptor, error) {
	if !recordID.MatchString(pin) {
		return distillation.Manifest{}, nil, invalid("manifest pin")
	}
	manifestRecord, err := s.get(ctx, kindManifest, pin)
	if err != nil {
		return distillation.Manifest{}, nil, s.classified(err)
	}
	job, err := s.get(ctx, kindJob, pin)
	if err != nil {
		return distillation.Manifest{}, nil, s.classified(err)
	}
	encoded, _ := weightsbundle.Canonical(manifestRecord)
	manifest, err := distillation.Decode(encoded)
	if err != nil {
		return distillation.Manifest{}, nil, s.classified(err)
	}
	exportsRaw, _ := weightsbundle.Canonical(job["exports"])
	var exports []distillation.ExportDescriptor
	if err := weightsbundle.StrictUnmarshal(exportsRaw, &exports); err != nil {
		return distillation.Manifest{}, nil, s.classifyLogged("recorded job exports", err)
	}
	return manifest, exports, nil
}

// SubmitPredictions records the sealed teacher output for a job. The set
// must rebuild identically from the recorded manifest and job: every
// output a prediction from the chosen teacher, every example inside an
// admitted export and never held out. A receipt that names a prediction set
// is admitted only after that set is recorded here.
func (s *Service) SubmitPredictions(ctx context.Context, pin string, raw json.RawMessage) (json.RawMessage, error) {
	manifest, exports, err := s.recordedJob(ctx, pin)
	if err != nil {
		return nil, err
	}
	var set distillation.PredictionSet
	if err := weightsbundle.StrictUnmarshal(raw, &set); err != nil {
		return nil, invalid("predictions: " + err.Error())
	}
	rebuilt, err := distillation.NewPredictionSet(manifest, exports, set.Predictions)
	if err != nil {
		return nil, s.classified(err)
	}
	wantID, _ := rebuilt.ID()
	gotID, err := set.ID()
	if err != nil || wantID != gotID {
		return nil, refused(errors.New("prediction set does not rebuild from the recorded manifest and job: its teacher, egress, prompt version, or pin differs"))
	}
	generic, err := weightsbundle.DecodeRecord(raw)
	if err != nil {
		return nil, invalid(err.Error())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.policyFor(ctx, pin); err != nil {
		if shoal.IsErrorCode(err, shoal.ErrorNotFound) {
			return nil, refused(fmt.Errorf("declare an evaluation policy for manifest %s before recording teacher predictions", pin))
		}
		return nil, s.classified(err)
	}
	if err := s.put(ctx, kindPredictions, pin, generic); err != nil {
		if errors.Is(err, storage.ErrExists) {
			return nil, refused(fmt.Errorf("teacher predictions for manifest %s are already recorded", pin))
		}
		return nil, s.classifyLogged("record predictions", err)
	}
	return marshal(map[string]any{"manifest_pin": pin, "predictions_id": gotID, "count": len(set.Predictions), "teacher": set.Teacher, "teacher_egress": set.TeacherEgress})
}

// policyFor returns the sealed evaluation policy declared for a manifest,
// or a not-found error when none is.
func (s *Service) policyFor(ctx context.Context, pin string) (weightsbundle.Record, error) {
	pointer, err := s.get(ctx, kindPolicyPointer, pin)
	if err != nil {
		return nil, err
	}
	id := str(pointer, "policy_id")
	if !recordID.MatchString(id) {
		return nil, refused(fmt.Errorf("evaluation policy pointer for manifest %s names no record", pin))
	}
	policy, err := s.get(ctx, kindPolicy, id)
	if err != nil {
		return nil, err
	}
	if err := weightsbundle.VerifyRecord(policy, kindPolicy); err != nil {
		return nil, refused(fmt.Errorf("evaluation policy %s recorded for manifest %s does not verify", id, pin))
	}
	return policy, nil
}

// DeclarePolicy seals the evaluation policy a run will be judged under and
// records, under the manifest pin, that it was declared for this job. It is
// accepted only while no predictions or receipt are recorded for the job,
// so the gates are fixed before the host accepts any run output; a second
// declaration must be identical.
// The policy itself is addressed by its gates, so runs judged the same way
// share it and a release can name its predecessor's policy.
func (s *Service) DeclarePolicy(ctx context.Context, pin string, raw json.RawMessage) (json.RawMessage, error) {
	if _, _, err := s.recordedJob(ctx, pin); err != nil {
		return nil, err
	}
	policy, err := distillation.DecodeEvaluationPolicy(raw)
	if err != nil {
		return nil, s.classified(err)
	}
	record, err := distillation.SealPolicy(policy)
	if err != nil {
		return nil, s.classified(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.get(ctx, kindReceipt, pin); err == nil {
		return nil, refused(fmt.Errorf("a receipt is already recorded for manifest %s; a policy declared after the run's outputs exist is not predeclared", pin))
	} else if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		return nil, s.classified(err)
	}
	if _, err := s.get(ctx, kindPredictions, pin); err == nil {
		return nil, refused(fmt.Errorf("teacher predictions are already recorded for manifest %s; a policy declared after the run's outputs exist is not predeclared", pin))
	} else if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		return nil, s.classified(err)
	}
	if existing, err := s.policyFor(ctx, pin); err == nil {
		if str(existing, "id") != str(record, "id") {
			return nil, refused(fmt.Errorf("evaluation policy %s is already declared for manifest %s; a run is judged under one policy", str(existing, "id"), pin))
		}
		return marshal(map[string]any{"manifest_pin": pin, "evaluation_policy": existing, "policy_id": str(existing, "id")})
	} else if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		return nil, s.classified(err)
	}
	id := str(record, "id")
	if err := s.put(ctx, kindPolicy, id, record); err != nil && !errors.Is(err, storage.ErrExists) {
		return nil, s.classified(err)
	}
	pointer := map[string]any{"schema": 1, "kind": kindPolicyPointer, "manifest_pin": pin, "policy_id": id, "declared_at": s.cfg.Now().UTC().Format(time.RFC3339Nano)}
	if err := s.put(ctx, kindPolicyPointer, pin, pointer); err != nil {
		if errors.Is(err, storage.ErrExists) {
			return nil, refused(fmt.Errorf("an evaluation policy is already declared for manifest %s", pin))
		}
		return nil, s.classified(err)
	}
	return marshal(map[string]any{"manifest_pin": pin, "evaluation_policy": record, "policy_id": str(record, "id")})
}

// SubmitReceipt verifies a trainer receipt against the recorded manifest.
func (s *Service) SubmitReceipt(ctx context.Context, pin string, raw json.RawMessage) (json.RawMessage, error) {
	manifest, _, err := s.recordedJob(ctx, pin)
	if err != nil {
		return nil, err
	}
	var receipt distillation.Receipt
	if err := weightsbundle.StrictUnmarshal(raw, &receipt); err != nil {
		return nil, invalid("receipt: " + err.Error())
	}
	if err := distillation.VerifyReceipt(manifest, receipt); err != nil {
		return nil, s.classified(err)
	}
	if receipt.Status == distillation.StatusCompleted {
		// The policy the run will be judged under is sealed before the host
		// accepts outputs. A completed run without one is refused here, while
		// the omission is still recoverable, rather than at sealing time.
		if _, err := s.policyFor(ctx, pin); err != nil {
			if shoal.IsErrorCode(err, shoal.ErrorNotFound) {
				return nil, refused(fmt.Errorf("no evaluation policy is declared for manifest %s; declare it before submitting the run's receipt", pin))
			}
			return nil, s.classified(err)
		}
		set, err := s.get(ctx, kindPredictions, pin)
		if err != nil {
			if shoal.IsErrorCode(err, shoal.ErrorNotFound) {
				return nil, refused(fmt.Errorf("receipt names teacher predictions %s, but no prediction set is recorded for manifest %s; submit the predictions first", receipt.TeacherPredictionsID, pin))
			}
			return nil, s.classified(err)
		}
		if id, _ := weightsbundle.Digest(set); id != receipt.TeacherPredictionsID {
			return nil, refused(fmt.Errorf("receipt names teacher predictions %s; the recorded set for this manifest is %s", receipt.TeacherPredictionsID, id))
		}
		// A student trains on teacher predictions: it cannot have trained on
		// more examples than the recorded set covers.
		if n := len(toSlice(set["predictions"])); receipt.TrainedExampleCount > n {
			return nil, refused(fmt.Errorf("receipt claims %d trained examples; the recorded prediction set covers %d", receipt.TrainedExampleCount, n))
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.put(ctx, kindReceipt, pin, receipt); err != nil {
		if errors.Is(err, storage.ErrExists) {
			return nil, refused(fmt.Errorf("a receipt for manifest %s is already recorded", pin))
		}
		return nil, s.classified(err)
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
		return nil, s.classified(err)
	}
	digest, _ := manifest.Digest()
	if _, got, err := weightsbundle.Load(filepath.Join(s.cfg.ServingRoot, digest)); err != nil {
		return nil, refused(fmt.Errorf("bundle %s is not published on the serving root: %s", digest, s.redactErr("candidate", err)))
	} else if got != digest {
		return nil, refused(fmt.Errorf("published bundle digest %s differs from artifact %s", got, digest))
	}
	// A candidate is bound to the run that produced it: the manifest and a
	// completed receipt for exactly this bundle must be recorded here, the
	// candidate's task scope must be the selection's, its policy must be the
	// one declared before the receipt, and the record itself must be the one
	// this host seals from its own records and the published bundle. A
	// candidate naming no manifest, or an unrecorded one, is refused: nothing
	// about a run is taken on the caller's word.
	pin := str(body.Candidate, "training_manifest_sha256")
	if pin == "" {
		return nil, invalid("candidate must name the training manifest of the run that produced it")
	}
	if !recordID.MatchString(pin) {
		return nil, invalid("candidate training_manifest_sha256 must be a 64-hex content address")
	}
	record, err := s.get(ctx, kindManifest, pin)
	if err != nil {
		return nil, refused(fmt.Errorf("candidate names training manifest %s, which is not recorded on this host", pin))
	}
	encoded, _ := weightsbundle.Canonical(record)
	m, err := distillation.Decode(encoded)
	if err != nil {
		return nil, s.classified(err)
	}
	if err := distillation.CheckCandidateScope(m, body.Candidate); err != nil {
		return nil, s.classified(err)
	}
	receipt, err := s.get(ctx, kindReceipt, pin)
	if err != nil {
		return nil, refused(fmt.Errorf("candidate names training manifest %s, which has no recorded receipt", pin))
	}
	if str(receipt, "status") != distillation.StatusCompleted {
		return nil, refused(fmt.Errorf("the recorded run for manifest %s is %q, not completed", pin, str(receipt, "status")))
	}
	if str(receipt, "bundle_digest") != digest {
		return nil, refused(fmt.Errorf("the recorded receipt for manifest %s produced bundle %s, not the artifact's %s", pin, str(receipt, "bundle_digest"), digest))
	}
	// The candidate's policy must be the one declared here before the
	// receipt; a policy sealed elsewhere, after the fact, is not it.
	policy, err := s.policyFor(ctx, pin)
	if err != nil {
		return nil, refused(fmt.Errorf("candidate names training manifest %s, which has no declared evaluation policy", pin))
	}
	if str(body.Candidate, "evaluation_policy_id") != str(policy, "id") {
		return nil, refused(fmt.Errorf("candidate's evaluation policy %s is not the policy %s declared for manifest %s before its receipt", str(body.Candidate, "evaluation_policy_id"), str(policy, "id"), pin))
	}
	sealed, _, err := s.rebuildFromServingRoot(ctx, pin, str(body.Candidate, "dataset_id"))
	if err != nil {
		return nil, err
	}
	if str(sealed.Candidate, "id") != str(body.Candidate, "id") {
		return nil, refused(fmt.Errorf("candidate %s is not the candidate this host seals for manifest %s from its own records (%s)", str(body.Candidate, "id"), pin, str(sealed.Candidate, "id")))
	}
	if str(sealed.Artifact, "id") != str(body.Artifact, "id") {
		return nil, refused(fmt.Errorf("artifact %s is not the artifact this host seals for manifest %s (%s); it must bind the host's evaluation", str(body.Artifact, "id"), pin, str(sealed.Artifact, "id")))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.put(ctx, kindCandidate, str(body.Candidate, "id"), body.Candidate); err != nil && !errors.Is(err, storage.ErrExists) {
		return nil, s.classified(err)
	}
	if err := s.put(ctx, kindArtifact, str(body.Artifact, "id"), body.Artifact); err != nil && !errors.Is(err, storage.ErrExists) {
		return nil, s.classified(err)
	}
	return marshal(map[string]any{"candidate": body.Candidate, "artifact": body.Artifact, "bundle_digest": digest})
}

// Candidates lists candidates with bundle digests and lineage.
func (s *Service) Candidates(ctx context.Context) (json.RawMessage, error) {
	candidates, err := s.list(ctx, kindCandidate)
	if err != nil {
		return nil, s.classified(err)
	}
	artifacts, err := s.list(ctx, kindArtifact)
	if err != nil {
		return nil, s.classified(err)
	}
	evaluations, err := s.list(ctx, kindEvaluation)
	if err != nil {
		return nil, s.classified(err)
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
	candidate, err := s.get(ctx, kindCandidate, str(record, "candidate_id"))
	if err != nil {
		return nil, s.classified(err)
	}
	if str(record, "evaluation_policy_id") != str(candidate, "evaluation_policy_id") {
		return nil, refused(fmt.Errorf("evaluation is under policy %s; the candidate was sealed under %s", str(record, "evaluation_policy_id"), str(candidate, "evaluation_policy_id")))
	}
	// An evaluation's content (disposition, reasons, metrics, the policy it
	// was held to) is never taken on the caller's word: the host measures
	// the run again from its own records and the published bundle, and
	// accepts only the record that measurement seals.
	sealed, _, err := s.rebuildFromServingRoot(ctx, str(candidate, "training_manifest_sha256"), str(candidate, "dataset_id"))
	if err != nil {
		return nil, err
	}
	if str(sealed.Evaluation, "id") != str(record, "id") {
		return nil, refused(fmt.Errorf("evaluation %s is not the evaluation this host seals for candidate %s from its own records (%s)", str(record, "id"), str(candidate, "id"), str(sealed.Evaluation, "id")))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.put(ctx, kindEvaluation, str(record, "id"), record); err != nil && !errors.Is(err, storage.ErrExists) {
		return nil, s.classified(err)
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
		return nil, s.classified(err)
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
	active, hasActive := r.Active()
	if hasActive {
		out["active"] = map[string]any{"release": active, "eligibility": r.EligibilityOf(active.ID), "bundle_digest": active.BundleDigest}
	}
	history := []any{}
	for _, rel := range r.History() {
		history = append(history, map[string]any{"release": rel, "eligibility": r.EligibilityOf(rel.ID)})
	}
	out["history"] = history
	if s.cfg.Serving != nil {
		// What is served now comes only from the runtime's own report; the
		// registry's pointer says what should be served, and the two are
		// compared rather than conflated.
		report, err := s.cfg.Serving.ServingReport(ctx)
		if err != nil {
			out["serving"] = map[string]any{"error": err.Error(), "ready": false}
		} else {
			served, _ := report["bundle_digest"].(string)
			if hasActive && active.BundleDigest != nil {
				report["serves_active_release"] = served == *active.BundleDigest && report["ready"] == true
			} else {
				report["serves_active_release"] = false
			}
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
	owner, err := actorFor(ctx, body.Owner)
	if err != nil {
		return nil, err
	}
	if body.CandidateID == "" {
		return nil, invalid("candidate_id is required")
	}
	for _, id := range []string{body.CandidateID, body.EvaluationID, body.ArtifactID} {
		if id != "" && !recordID.MatchString(id) {
			return nil, invalid("record ids must be 64-hex content addresses")
		}
	}
	candidate, err := s.get(ctx, kindCandidate, body.CandidateID)
	if err != nil {
		return nil, s.classified(err)
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
		return nil, s.classified(err)
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
	approval, err := weightsbundle.NewPromotionApproval(body.CandidateID, body.EvaluationID, owner)
	if err != nil {
		return nil, s.classified(err)
	}
	lineage := weightsbundle.DatasetLineage{}
	if pin := str(candidate, "dataset_manifest_sha256"); pin != "" {
		if !recordID.MatchString(pin) {
			return nil, refused(errors.New("candidate dataset manifest pin is not a content address"))
		}
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
		return nil, s.classified(err)
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
	owner, err := actorFor(ctx, body.Owner)
	if err != nil {
		return nil, err
	}
	if body.PriorReleaseID == "" {
		return nil, invalid("prior_release_id is required")
	}
	active, ok := s.cfg.Registry.Active()
	if !ok {
		return nil, refused(weightsbundle.ErrNoActiveRelease)
	}
	// The prior bundle must still be on the serving path, exactly: a
	// rollback to a release the runtime cannot load would point at nothing.
	if prior, ok := s.cfg.Registry.Get(body.PriorReleaseID); ok && prior.BundleDigest != nil {
		if _, got, err := weightsbundle.Load(filepath.Join(s.cfg.ServingRoot, *prior.BundleDigest)); err != nil || got != *prior.BundleDigest {
			return nil, refused(fmt.Errorf("prior release %s bundle %s is not on the serving root; re-import it before rolling back", body.PriorReleaseID, *prior.BundleDigest))
		}
	}
	approval, err := weightsbundle.NewRollbackApproval(active.ID, body.PriorReleaseID, owner)
	if err != nil {
		return nil, s.classified(err)
	}
	release, err := s.cfg.Registry.Rollback(ctx, body.PriorReleaseID, approval)
	if err != nil {
		return nil, s.classified(err)
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
		return nil, s.classified(err)
	}
	reports := []any{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		item := map[string]any{"file": e.Name()}
		if err != nil {
			item["error"] = s.redactErr("evidence", err)
		} else if record, err := weightsbundle.DecodeRecord(raw); err != nil {
			item["error"] = s.redactErr("evidence", err)
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
		return nil, s.classified(err)
	}
	out := []any{}
	for _, e := range entries {
		if !e.IsDir() || strings.Contains(e.Name(), ".exporting") {
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
		view["error"] = s.redactErr("bundle view", err)
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
		return nil, s.classified(err)
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
		return nil, s.classified(err)
	}
	index, err := releasebundle.Export(releasebundle.ExportInput{Release: release, Lineage: lineage, BundleRoot: filepath.Join(s.cfg.ServingRoot, *release.BundleDigest), DatasetManifest: datasetManifest, Evaluation: evaluation, Images: s.cfg.Images, Now: s.cfg.Now()}, filepath.Join(s.cfg.BundlesDir, body.Name))
	if err != nil {
		return nil, s.classified(err)
	}
	view := s.bundleView(body.Name)
	view["index"] = index
	return marshal(view)
}

// exportManifestBytes returns the authorized manifest bytes for a recorded
// export: the copy recorded under the pin when the export was requested, or,
// for an export recorded before copies were kept, the exporter's current
// bytes when they still hash to the pin.
func (s *Service) exportManifestBytes(ctx context.Context, export weightsbundle.Record) ([]byte, error) {
	pin := str(export, "manifest_sha256")
	recorded, err := s.getRaw(ctx, kindExportManifest, pin)
	if err == nil {
		return recorded, nil
	}
	if !shoal.IsErrorCode(err, shoal.ErrorNotFound) {
		return nil, s.classified(err)
	}
	if s.cfg.Exporter == nil {
		return nil, unavailable("the export's manifest bytes were not recorded and no dataset exporter is mounted on this host to re-obtain them")
	}
	result, err := s.cfg.Exporter.Export(ctx, shoal.ID(str(export, "cohort_id")))
	if err != nil {
		return nil, s.classified(err)
	}
	if weightsbundle.SHA256Hex(result.Manifest) != str(export, "manifest_sha256") {
		return nil, refused(errors.New("the authority's current export no longer matches the recorded manifest pin"))
	}
	return result.Manifest, nil
}

// ImportBundle imports an offline release bundle from the bundles directory.
func (s *Service) ImportBundle(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	var body struct {
		Name                string `json:"name"`
		Importer            string `json:"importer"`
		ExpectedIndexDigest string `json:"expected_index_digest"`
	}
	if err := weightsbundle.StrictUnmarshal(raw, &body); err != nil {
		return nil, invalid("body: " + err.Error())
	}
	importer, err := actorFor(ctx, body.Importer)
	if err != nil {
		return nil, err
	}
	if !safeID.MatchString(body.Name) {
		return nil, invalid("name must be a safe directory name")
	}
	if body.ExpectedIndexDigest != "" && !recordID.MatchString(body.ExpectedIndexDigest) {
		return nil, invalid("expected_index_digest must be a 64-hex digest")
	}
	result, err := releasebundle.Import(ctx, filepath.Join(s.cfg.BundlesDir, body.Name), releasebundle.ImportOptions{Registry: s.cfg.Registry, ServingRoot: s.cfg.ServingRoot, Importer: importer, Now: s.cfg.Now(), ExpectedIndexDigest: body.ExpectedIndexDigest})
	view := map[string]any{"name": body.Name, "entries": result.Verified.Results, "activated": result.Activated, "already_present": result.AlreadyThere, "release_id": result.Verified.Index.ReleaseID, "bundle_digest": result.Verified.Index.BundleDigest, "index_digest": result.Verified.IndexDigest, "importer": importer}
	if err != nil {
		classified := s.classifyLogged("import bundle", err)
		view["error"] = classified.Error()
		encoded, _ := json.Marshal(view)
		return encoded, classified
	}
	if err := s.put(ctx, kindEvaluation, str(result.Verified.Evaluation, "id"), result.Verified.Evaluation); err != nil && !errors.Is(err, storage.ErrExists) {
		return nil, s.classified(err)
	}
	return marshal(view)
}
