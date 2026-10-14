// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decisiondatasets

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type Service struct{ config Config }

func nilValue(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Func, reflect.Slice:
		return r.IsNil()
	}
	return false
}
func New(c Config) (*Service, error) {
	if nilValue(c.Resolver) || nilValue(c.Authority) || c.Clock == nil {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument, "invalid dataset exporter configuration")
	}
	return &Service{c}, nil
}
func denied() error { return auth.ObjectNotFound() }
func idOK(id shoal.ID) bool {
	return utf8.ValidString(string(id)) && strings.TrimSpace(string(id)) != "" && shoal.ValidateRequiredID("id", id) == nil
}
func stamp(t time.Time) bool { return !t.IsZero() && t.Year() >= 1 && t.Year() <= 9999 }
func sum(b []byte) string    { x := sha256.Sum256(b); return hex.EncodeToString(x[:]) }
func featureDigest(features []float64) string {
	b := make([]byte, len("shoal.numeric.features.v1\x00")+4+8*len(features))
	n := copy(b, []byte("shoal.numeric.features.v1\x00"))
	binary.LittleEndian.PutUint32(b[n:], uint32(len(features)))
	n += 4
	for _, f := range features {
		binary.LittleEndian.PutUint64(b[n:], math.Float64bits(f))
		n += 8
	}
	return sum(b)
}
func cohortDigest(c Cohort) string {
	return sum(encode(struct {
		C                Cohort
		TaskID, PolicyID shoal.ID
	}{c, c.Task.ID(), c.Policy.ID()}))
}
func encode(v any) []byte { b, _ := json.Marshal(v); return b }
func (s *Service) caller(ctx context.Context, old *auth.Decision) (auth.Decision, error) {
	d, e := s.config.Resolver.Resolve(ctx)
	if e != nil || ctx.Err() != nil {
		return d, denied()
	}
	now := s.config.Clock().UTC()
	if !stamp(now) || !now.Before(d.AuthenticationExpires()) {
		return d, denied()
	}
	fp, e := auth.AuthorizationFingerprint(d)
	if e != nil {
		return d, denied()
	}
	if old != nil {
		prev, e := auth.AuthorizationFingerprint(*old)
		if e != nil || prev != fp {
			return d, denied()
		}
	}
	return d, nil
}
func cohortValid(c Cohort, requested shoal.ID, now time.Time) ([]string, error) {
	if c.ID != requested || c.Task.Validate() != nil || c.Policy.Validate() != nil || c.Task.Config().LabelPolicyID != c.Policy.ID() || len(c.Members) < 1 || len(c.Members) > 256 || !stamp(c.Cutoff) || c.Cutoff.After(now) || (c.Mode != "prospective" && c.Mode != "reconstructed") {
		return nil, denied()
	}
	for _, id := range []shoal.ID{c.ID, c.AuthorityRevisionID, c.InventoryID, c.QuestionID, c.FeatureSchemaID, c.FeatureBuilderID, c.SplitPolicyID, c.SamplingPolicyID} {
		if !idOK(id) {
			return nil, denied()
		}
	}
	var labels []string
	for _, q := range c.Task.Config().Questions {
		if q.ID == c.QuestionID && q.Kind == decision.Choice && len(q.Labels) == 2 {
			labels = q.Labels
		}
	}
	if len(labels) != 2 {
		return nil, denied()
	}
	budget := 0
	seen := map[shoal.ID]bool{}
	targets := map[shoal.ID]bool{}
	families := map[shoal.ID]string{}
	for _, m := range c.Members {
		for _, id := range []shoal.ID{m.ID, m.TargetID, m.RequestID, m.PredictionID, m.SubjectID} {
			if !idOK(id) {
				return nil, denied()
			}
		}
		if seen[m.ID] || targets[m.TargetID] {
			return nil, denied()
		}
		seen[m.ID] = true
		targets[m.TargetID] = true
		switch m.Split {
		case "train", "calibration", "validation", "test":
		default:
			return nil, denied()
		}
		if len(m.FamilyIDs) < 1 || len(m.FamilyIDs) > 1024 {
			return nil, denied()
		}
		own := map[shoal.ID]bool{}
		for _, id := range m.FamilyIDs {
			if !idOK(id) || own[id] || (families[id] != "" && families[id] != m.Split) {
				return nil, denied()
			}
			budget += len(id) + 16
			if budget > 3<<20 {
				return nil, denied()
			}
			own[id] = true
			families[id] = m.Split
		}
		if p := m.InclusionProbability; p != nil && (math.IsNaN(*p) || math.IsInf(*p, 0) || *p <= 0 || *p > 1) {
			return nil, denied()
		}
	}
	if len(encode(c)) > 4<<20 {
		return nil, denied()
	}
	return labels, nil
}
func receiptID(id shoal.ID, prefix string) bool {
	v := strings.TrimPrefix(string(id), prefix)
	if len(v) != 64 || v == string(id) {
		return false
	}
	b, e := hex.DecodeString(v)
	return e == nil && len(b) == 32 && strings.ToLower(v) == v
}
func idsEqual(a, b []shoal.ID) bool {
	if len(a) != len(b) {
		return false
	}
	aa := append([]shoal.ID(nil), a...)
	bb := append([]shoal.ID(nil), b...)
	sort.Slice(aa, func(i, j int) bool { return aa[i] < aa[j] })
	sort.Slice(bb, func(i, j int) bool { return bb[i] < bb[j] })
	for i := range aa {
		if aa[i] != bb[i] || (i > 0 && aa[i] == aa[i-1]) {
			return false
		}
	}
	return true
}

// Export materializes one registered cohort. It performs all reads before the
// final joint check and never turns an unverified observation into a negative.
func (s *Service) Export(ctx context.Context, cohortID shoal.ID) (Bundle, error) {
	var zero Bundle
	if !idOK(cohortID) {
		return zero, denied()
	}
	d, e := s.caller(ctx, nil)
	if e != nil {
		return zero, e
	}
	c, e := s.config.Authority.Resolve(ctx, d, cohortID)
	if e != nil {
		return zero, denied()
	}
	if _, ok := hydratedSize(c, 4<<20); !ok {
		return zero, denied()
	}
	c = cloneCohort(c)
	now := s.config.Clock().Round(0).UTC()
	labels, e := cohortValid(c, cohortID, now)
	if e != nil {
		return zero, e
	}
	targets := make([]Target, len(c.Members))
	hydrated, featureCount := 0, 0
	for i, m := range c.Members {
		loaded, err := s.config.Authority.Load(ctx, d, cloneCohort(c), cloneMember(m))
		if err != nil {
			return zero, denied()
		}
		n, ok := preflightTarget(loaded, MaxHydratedBytes-hydrated)
		featureCount += len(loaded.Features)
		if !ok || featureCount > 1000000 {
			return zero, denied()
		}
		hydrated += n
		targets[i] = cloneTarget(loaded)
	}
	rows := make([]map[string]any, 0, len(targets))
	provenance := make([]map[string]any, 0, len(targets))
	contents := map[string]string{}
	width, scalars := 0, 0
	metadataBytes := 0
	for i, t := range targets {
		m := c.Members[i]
		r := t.Prediction.Request()
		pc := r.Picture().Config()
		target, e := decision.AdjudicationTargetID(c.Task.ID(), r.Picture().ID(), m.SubjectID, c.QuestionID)
		if e != nil || t.Prediction.Validate() != nil || t.Prediction.ID() != m.PredictionID || r.ID() != m.RequestID || r.Task().ID() != c.Task.ID() || target != m.TargetID || !t.SourceAvailable || !t.InventoryComplete || !idOK(t.InventoryID) || !stamp(t.FeatureReceivedAt) || t.FeatureReceivedAt.After(c.Cutoff) || pc.Cutoff.After(c.Cutoff) || t.FeatureInputDigest != pc.InputDigest {
			return zero, denied()
		}
		metadataBytes += len(encode(pc.Sources)) + len(encode(m.FamilyIDs)) + 4096
		if metadataBytes > 4<<20 {
			return zero, denied()
		}
		subject := false
		for _, id := range r.Config().SubjectIDs {
			subject = subject || id == m.SubjectID
		}
		if !subject {
			return zero, denied()
		}
		if len(t.Features) < 1 || len(t.Features) > 65536 || len(pc.Sources) == 0 {
			return zero, denied()
		}
		if width == 0 {
			width = len(t.Features)
		}
		if width != len(t.Features) {
			return zero, denied()
		}
		scalars += len(t.Features)
		if scalars > 1000000 {
			return zero, denied()
		}
		for _, f := range t.Features {
			if math.IsNaN(f) || math.IsInf(f, 0) || math.Abs(f) > 1000000 {
				return zero, denied()
			}
		}
		observed := pc.Sources[0].ObservedAt
		received := t.FeatureReceivedAt
		digests := []string{}
		for _, source := range pc.Sources {
			if source.Role == decision.ReportedOutcome || source.Role == decision.Prediction || source.ObservedAt.After(c.Cutoff) || source.ReceivedAt.After(c.Cutoff) {
				return zero, denied()
			}
			if source.ObservedAt.After(observed) {
				observed = source.ObservedAt
			}
			if source.ReceivedAt.After(received) {
				received = source.ReceivedAt
			}
			if prior := contents[source.Digest]; prior != "" && prior != m.Split {
				return zero, denied()
			}
			contents[source.Digest] = m.Split
			digests = append(digests, source.Digest)
		}
		if observed.After(received) {
			return zero, denied()
		}
		if len(t.History) > 128 || len(t.OutcomeReceiptIDs) > 256 {
			return zero, denied()
		}
		outcomeSet := map[shoal.ID]bool{}
		for _, id := range t.OutcomeReceiptIDs {
			if !receiptID(id, "outcome-receipt:") || outcomeSet[id] {
				return zero, denied()
			}
			outcomeSet[id] = true
		}
		selected := -1
		var previous shoal.ID
		var previousTime time.Time
		for j, h := range t.History {
			cfg := h.ProposalConfig
			pid, err := decision.AdjudicationProposalID(c.Policy.ID(), target, cfg)
			if err != nil || pid != h.ProposalID || h.TargetID != target || h.TaskID != c.Task.ID() || h.PictureID != r.Picture().ID() || h.PolicyID != c.Policy.ID() || cfg.SubjectID != m.SubjectID || cfg.QuestionID != c.QuestionID || h.Version != int64(j+1) || cfg.ExpectedHeadID != previous || cfg.ExpectedVersion != int64(j) || !receiptID(h.ID, "adjudication-receipt:") || !idOK(h.BasisID) || !stamp(h.ReceivedAt) || h.ReceivedAt.Before(previousTime) || h.ReceivedAt.After(now) {
				return zero, denied()
			}
			previous = h.ID
			previousTime = h.ReceivedAt
			if !h.ReceivedAt.After(c.Cutoff) {
				selected = j
			}
		}
		status := "unknown"
		var label any
		labelTime := c.Cutoff
		var selectedTime any
		var selectedID, basisID shoal.ID
		var selectedVersion int64
		reasons := []string{}
		eligible := false
		if selected >= 0 {
			h := t.History[selected]
			b := t.SelectedBasis
			p := b.Proposal()
			if b.Validate() != nil || b.ID() != h.BasisID || p.ID() != h.ProposalID || p.TargetID() != target || p.PolicyID() != c.Policy.ID() || b.Config().CapturedAt.After(h.ReceivedAt) || b.Config().Cutoff.After(c.Cutoff) {
				return zero, denied()
			}
			if h.ReceivedAt.Before(observed) {
				return zero, denied()
			}
			selectedID, basisID, selectedVersion = h.ID, h.BasisID, h.Version
			labelTime = h.ReceivedAt
			selectedTime = labelTime.UTC().Format(time.RFC3339Nano)
			switch h.ProposalConfig.Disposition {
			case decision.AdjudicationVerified:
				status = "verified"
				label = h.ProposalConfig.Label
				if label != labels[0] && label != labels[1] {
					return zero, denied()
				}
				eligible = true
			case decision.AdjudicationDisputed:
				status = "disputed"
			default:
				status = "unknown"
			}
			inventory := []shoal.ID{}
			for _, o := range b.Config().Outcomes {
				inventory = append(inventory, o.ReceiptID)
			}
			if !idsEqual(inventory, t.OutcomeReceiptIDs) {
				reasons = append(reasons, "unadjudicated_inventory")
				eligible = false
			}
			if selected != len(t.History)-1 {
				reasons = append(reasons, "later_adjudication")
				eligible = false
			}
		} else {
			reasons = append(reasons, "no_label_at_cutoff")
		}
		if status != "verified" {
			reasons = append(reasons, "label_status:"+status)
		}
		if !t.TrainingAllowed {
			reasons = append(reasons, "training_not_allowed")
			eligible = false
		}
		if t.LabelWithdrawn {
			reasons = append(reasons, "label_withdrawn")
			eligible = false
		}
		if !eligible && status == "verified" {
			status = "unknown"
			label = nil
		}
		if m.Split != "train" {
			reasons = append(reasons, "split:"+m.Split)
		}
		families := append([]shoal.ID(nil), m.FamilyIDs...)
		sort.Slice(families, func(i, j int) bool { return families[i] < families[j] })
		sort.Strings(digests)
		featureHash := featureDigest(t.Features)
		rows = append(rows, map[string]any{"id": m.ID, "family": sum(encode(families)), "content_sha256": sum(encode(digests)), "source_revision": sum(encode(pc.Sources)), "observed_at": observed.UTC().Format(time.RFC3339Nano), "received_at": received.UTC().Format(time.RFC3339Nano), "label_received_at": labelTime.UTC().Format(time.RFC3339Nano), "label_status": status, "training_allowed": t.TrainingAllowed, "split": m.Split, "features": t.Features, "label": label})
		provenance = append(provenance, map[string]any{"id": m.ID, "request_id": m.RequestID, "prediction_id": m.PredictionID, "picture_id": r.Picture().ID(), "target_id": target, "selected_receipt_id": selectedID, "selected_version": selectedVersion, "selected_basis_id": basisID, "selected_label_received_at": selectedTime, "current_head_id": previous, "current_head_version": len(t.History), "inventory_id": t.InventoryID, "sources": pc.Sources, "family_ids": families, "feature_sha256": featureHash, "input_digest": t.FeatureInputDigest, "split": m.Split, "inclusion_probability": m.InclusionProbability, "exclusion_reasons": reasons})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i]["id"].(shoal.ID) < rows[j]["id"].(shoal.ID) })
	sort.Slice(provenance, func(i, j int) bool { return provenance[i]["id"].(shoal.ID) < provenance[j]["id"].(shoal.ID) })
	data := encode(map[string]any{"schema": 1, "kind": "numeric-training-dataset", "task_id": c.Task.ID(), "question_id": c.QuestionID, "feature_schema_id": c.FeatureSchemaID, "labels": labels, "cutoff": c.Cutoff.UTC().Format(time.RFC3339Nano), "provenance": "trusted-export", "rows": rows})
	manifest := encode(map[string]any{"schema": 1, "kind": "authorized-numeric-training-export", "cohort_id": c.ID, "cohort_sha256": cohortDigest(c), "authority_revision_id": c.AuthorityRevisionID, "inventory_id": c.InventoryID, "task_id": c.Task.ID(), "question_id": c.QuestionID, "label_policy_id": c.Policy.ID(), "training_purpose_id": c.Policy.Config().TrainingPurposeID, "feature_schema_id": c.FeatureSchemaID, "feature_builder_id": c.FeatureBuilderID, "cutoff": c.Cutoff.UTC().Format(time.RFC3339Nano), "created_at": now.Format(time.RFC3339Nano), "provenance_mode": c.Mode, "split_policy_id": c.SplitPolicyID, "sampling_policy_id": c.SamplingPolicyID, "checkpoint_overlap": "unknown", "dataset_sha256": sum(data), "dataset_bytes": len(data), "rows": provenance})
	if len(data) > 16<<20 || len(manifest) > 4<<20 {
		return zero, denied()
	}
	verifiedCohort, verifiedTargets := cloneCohort(c), cloneTargets(targets)
	if e = s.config.Authority.Verify(ctx, d, verifiedCohort, verifiedTargets); e != nil || !reflect.DeepEqual(c, verifiedCohort) || !reflect.DeepEqual(targets, verifiedTargets) {
		return zero, denied()
	}
	// DeepEqual treats signed zeros as equal, while the portable feature
	// commitment binds their exact IEEE-754 bits.
	for i := range targets {
		if featureDigest(targets[i].Features) != featureDigest(verifiedTargets[i].Features) {
			return zero, denied()
		}
	}
	if _, e = s.caller(ctx, &d); e != nil {
		return zero, e
	}
	return Bundle{data, manifest, sum(manifest)}, nil
}
