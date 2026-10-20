// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package distillation

import (
	"errors"
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/internal/weightsbundle"
)

func f(v float64) *float64 { return &v }

func identity(provider, model, egress string) ModelIdentity {
	return ModelIdentity{Provider: provider, Model: model, Version: "v1", CacheIdentity: provider + ":" + model + ":v1", Egress: egress}
}

func selection(role string, chosen string, rejectedReason string, candidates ...Candidate) ModelSelection {
	s := ModelSelection{Role: role, Eligible: candidates, Rule: Rule{Metric: "dev_recall", Direction: Maximize, Minimum: f(0.5)}, Chosen: chosen}
	for _, c := range candidates {
		if c.Alias != chosen {
			s.Rejected = append(s.Rejected, Rejection{Alias: c.Alias, Reason: rejectedReason})
		}
	}
	return s
}

func measurement(metric string, v float64) Measurement {
	return Measurement{Metric: metric, Value: v, Split: SplitDevelopment, EvidenceID: "evidence:" + metric}
}

func fixture() Manifest {
	teacherA := Candidate{Alias: "teacher-a", Identity: identity("local", "big-a", EgressLoopback), Measurements: []Measurement{measurement("dev_recall", 0.9)}}
	teacherB := Candidate{Alias: "teacher-b", Identity: identity("hosted", "big-b", EgressOffHost), Measurements: []Measurement{measurement("dev_recall", 0.8)}}
	base := Candidate{Alias: "base-small", Identity: identity("local", "small", EgressLoopback), Measurements: []Measurement{measurement("dev_recall", 0.6)}}
	return Manifest{
		Schema: 1, Kind: Kind, TrainingManifestSHA256: strings.Repeat("1", 64),
		Teacher:                   selection(RoleTeacher, "teacher-a", "lower development recall", teacherA, teacherB),
		Base:                      selection(RoleBase, "base-small", "", base),
		TeacherEgressAtGeneration: EgressLoopback,
		Data: DataSelection{
			Tasks:                []TaskSelection{{TaskID: "task-v1", CohortIDs: []string{"cohort-1", "cohort-2"}, LabelPolicyIDs: []string{"labels:1"}}},
			EvidenceFamilies:     []string{"code", "review"},
			PromptVersion:        "prompt:3",
			GenerationParameters: map[string]any{"temperature": 0.0, "max_tokens": 64},
			HeldOutExampleIDs:    []string{"ex-7", "ex-9"},
		},
		Exports: []ExportPin{{CohortID: "cohort-1", ManifestSHA256: strings.Repeat("a", 64)}, {CohortID: "cohort-2", ManifestSHA256: strings.Repeat("b", 64)}},
	}
}

func pin(t *testing.T, m Manifest) string {
	t.Helper()
	p, err := m.Pin()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAnySelectionChangeChangesThePin(t *testing.T) {
	base := fixture()
	original := pin(t, base)
	mutations := map[string]func(m *Manifest){
		"teacher choice": func(m *Manifest) {
			m.Teacher.Chosen = "teacher-b"
			m.Teacher.Rejected = []Rejection{{Alias: "teacher-a", Reason: "operator preferred local teacher despite rule"}}
			m.TeacherEgressAtGeneration = EgressOffHost
			m.Teacher.Eligible[1].Measurements[0].Value = 0.95
		},
		"eligible set":   func(m *Manifest) { m.Teacher.Eligible = m.Teacher.Eligible[:1]; m.Teacher.Rejected = nil },
		"rule threshold": func(m *Manifest) { m.Teacher.Rule.Minimum = f(0.6) },
		"measurement":    func(m *Manifest) { m.Teacher.Eligible[0].Measurements[0].Value = 0.91 },
		"cohorts": func(m *Manifest) {
			m.Data.Tasks[0].CohortIDs = []string{"cohort-1"}
			m.Exports = m.Exports[:1]
		},
		"label policy":        func(m *Manifest) { m.Data.Tasks[0].LabelPolicyIDs = []string{"labels:2"} },
		"evidence families":   func(m *Manifest) { m.Data.EvidenceFamilies = []string{"code"} },
		"prompt version":      func(m *Manifest) { m.Data.PromptVersion = "prompt:4" },
		"generation params":   func(m *Manifest) { m.Data.GenerationParameters["temperature"] = 0.5 },
		"held-out":            func(m *Manifest) { m.Data.HeldOutExampleIDs = []string{"ex-7"} },
		"export pin":          func(m *Manifest) { m.Exports[0].ManifestSHA256 = strings.Repeat("c", 64) },
		"training manifest":   func(m *Manifest) { m.TrainingManifestSHA256 = strings.Repeat("2", 64) },
		"base cache identity": func(m *Manifest) { m.Base.Eligible[0].Identity.CacheIdentity = "other" },
	}
	seen := map[string]string{original: "original"}
	for name, mutate := range mutations {
		m := fixture()
		mutate(&m)
		p := pin(t, m)
		if prior, dup := seen[p]; dup {
			t.Errorf("%s produced the same pin as %s", name, prior)
		}
		seen[p] = name
	}
	if pin(t, fixture()) != original {
		t.Fatal("pin is not deterministic")
	}
	encoded, _ := base.Encode()
	decoded, err := Decode(encoded)
	if err != nil || pin(t, decoded) != original {
		t.Fatalf("decode: %v", err)
	}
	if _, err := Decode(append(encoded, '{', '}')); err == nil {
		t.Fatal("trailing data accepted")
	}
}

func TestModelSelectionRefusals(t *testing.T) {
	cases := map[string]struct {
		mutate func(m *Manifest)
		want   string
	}{
		"chosen outside eligible":  {func(m *Manifest) { m.Teacher.Chosen = "teacher-z" }, "not in the eligible set"},
		"chosen fails rule":        {func(m *Manifest) { m.Teacher.Eligible[0].Measurements[0].Value = 0.4 }, "outside the rule's threshold"},
		"held-out measurement":     {func(m *Manifest) { m.Teacher.Eligible[0].Measurements[0].Split = "held_out" }, "only development evidence selects a model"},
		"unexplained rejection":    {func(m *Manifest) { m.Teacher.Rejected = nil }, "neither chosen nor rejected with a reason"},
		"rejection without reason": {func(m *Manifest) { m.Teacher.Rejected[0].Reason = "" }, "needs an eligible, unchosen alias and a reason"},
		"declared egress":          {func(m *Manifest) { m.Teacher.Eligible[0].Identity.Egress = "none" }, "not a resolved posture"},
		"egress at generation":     {func(m *Manifest) { m.TeacherEgressAtGeneration = EgressOffHost }, "does not match the chosen teacher's resolved posture"},
		"chosen without metric":    {func(m *Manifest) { m.Base.Eligible[0].Measurements = nil }, "has no measurement"},
		"export not admitted": {func(m *Manifest) {
			m.Exports = append(m.Exports, ExportPin{CohortID: "cohort-9", ManifestSHA256: strings.Repeat("9", 64)})
		}, "not an admitted cohort"},
		"admitted without pin": {func(m *Manifest) { m.Exports = m.Exports[:1] }, "has no export pin"},
		"unsorted held-out":    {func(m *Manifest) { m.Data.HeldOutExampleIDs = []string{"ex-9", "ex-7"} }, "sorted"},
		"no generation params": {func(m *Manifest) { m.Data.GenerationParameters = nil }, "generation parameters"},
		"wrong role":           {func(m *Manifest) { m.Base.Role = RoleTeacher }, "has role"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := fixture()
			tc.mutate(&m)
			_, err := m.Pin()
			if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %s", err, tc.want)
			}
		})
	}
	m := fixture()
	if got, err := m.Teacher.Apply(); err != nil || got != "teacher-a" {
		t.Fatalf("rule applied: %s %v", got, err)
	}
	// A better-measuring rejected alternative is allowed only with a reason.
	m.Teacher.Eligible[1].Measurements[0].Value = 0.95
	if _, err := m.Pin(); err != nil {
		t.Fatalf("reasoned override refused: %v", err)
	}
	if got, _ := m.Teacher.Apply(); got != "teacher-b" {
		t.Fatalf("rule now selects %s", got)
	}
}

func TestExportAuthorizationAndJob(t *testing.T) {
	m := fixture()
	good := ExportDescriptor{CohortID: "cohort-1", TaskID: "task-v1", LabelPolicyID: "labels:1", ManifestSHA256: strings.Repeat("a", 64), ExampleIDs: []string{"ex-1", "ex-2"}}
	other := ExportDescriptor{CohortID: "cohort-2", TaskID: "task-v1", LabelPolicyID: "labels:1", ManifestSHA256: strings.Repeat("b", 64), ExampleIDs: []string{"ex-3"}}
	if err := AuthorizeExport(m, good); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		export ExportDescriptor
		want   string
	}{
		"unadmitted cohort": {ExportDescriptor{CohortID: "cohort-9", TaskID: "task-v1", LabelPolicyID: "labels:1", ManifestSHA256: strings.Repeat("a", 64)}, "not admitted by the manifest"},
		"re-pinned":         {ExportDescriptor{CohortID: "cohort-1", TaskID: "task-v1", LabelPolicyID: "labels:1", ManifestSHA256: strings.Repeat("c", 64)}, "does not match the authorized pin"},
		"other task":        {ExportDescriptor{CohortID: "cohort-1", TaskID: "task-v2", LabelPolicyID: "labels:1", ManifestSHA256: strings.Repeat("a", 64)}, "outside the selection"},
		"other policy":      {ExportDescriptor{CohortID: "cohort-1", TaskID: "task-v1", LabelPolicyID: "labels:9", ManifestSHA256: strings.Repeat("a", 64)}, "not admitted for task"},
		"held-out leak":     {ExportDescriptor{CohortID: "cohort-1", TaskID: "task-v1", LabelPolicyID: "labels:1", ManifestSHA256: strings.Repeat("a", 64), ExampleIDs: []string{"ex-1", "ex-9"}}, "contains held-out example"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := AuthorizeExport(m, tc.export)
			if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %s", err, tc.want)
			}
		})
	}
	// A manifest admitting a cohort its exports did not authorize is refused before training.
	if _, err := NewJob(m, []ExportDescriptor{good}); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "has no export") {
		t.Fatalf("partial job: %v", err)
	}
	job, err := NewJob(m, []ExportDescriptor{good, other})
	if err != nil || job.ManifestPin != pin(t, m) {
		t.Fatalf("%v", err)
	}
}

func receipt(t *testing.T, m Manifest) Receipt {
	t.Helper()
	teacher, _ := m.Teacher.chosen()
	base, _ := m.Base.chosen()
	return Receipt{
		Schema: 1, Kind: KindReceipt, ManifestPin: pin(t, m),
		RequestedParameters: m.Data.GenerationParameters, ActualParameters: m.Data.GenerationParameters,
		RequestedCohorts: []string{"cohort-1", "cohort-2"}, ActualCohorts: []string{"cohort-2", "cohort-1"},
		RequestedTeacher: teacher.Identity, ActualTeacher: teacher.Identity, RequestedBase: base.Identity, ActualBase: base.Identity,
		RuntimeIdentities: map[string]string{"python": "3.12.3", "torch": "2.13.0"},
		Executor:          Executor{ID: "worker-1", AttestationID: "attest:1", Host: "cpu-node"},
		BundleDigest:      strings.Repeat("d", 64), TeacherPredictionsID: strings.Repeat("e", 64),
		HeldOutExcludedCount: 2, TrainedExampleCount: 3, Status: "completed", Deviations: []string{}, CandidateOnly: true,
	}
}

func TestReceiptVerification(t *testing.T) {
	m := fixture()
	if err := VerifyReceipt(m, receipt(t, m)); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		mutate func(r *Receipt)
		want   string
	}{
		"wrong pin":              {func(r *Receipt) { r.ManifestPin = strings.Repeat("0", 64) }, "does not match"},
		"improvement claimed":    {func(r *Receipt) { r.ImprovementClaimed = true }, "never claims improvement"},
		"not candidate only":     {func(r *Receipt) { r.CandidateOnly = false }, "candidate-only"},
		"evaluation in receipt":  {func(r *Receipt) { r.EvaluationPerformedBy = "trainer" }, "separate record"},
		"no executor":            {func(r *Receipt) { r.Executor = Executor{} }, "no executor"},
		"no runtime":             {func(r *Receipt) { r.RuntimeIdentities = nil }, "no runtime identities"},
		"requested params drift": {func(r *Receipt) { r.RequestedParameters = map[string]any{"temperature": 1.0} }, "requested parameters differ"},
		"widened cohort":         {func(r *Receipt) { r.ActualCohorts = append(r.ActualCohorts, "cohort-9") }, "outside the selection"},
		"other teacher": {func(r *Receipt) {
			r.ActualTeacher = identity("hosted", "big-b", EgressOffHost)
		}, "not the chosen teacher"},
		"other base":                 {func(r *Receipt) { r.ActualBase.Version = "v2" }, "not the chosen base"},
		"silent deviation":           {func(r *Receipt) { r.ActualParameters = map[string]any{"temperature": 0.2} }, "without a recorded deviation"},
		"completed without bundle":   {func(r *Receipt) { r.BundleDigest = "" }, "without a bundle digest"},
		"completed without teacher":  {func(r *Receipt) { r.TeacherPredictionsID = "" }, "without recorded teacher predictions"},
		"completed without examples": {func(r *Receipt) { r.TrainedExampleCount = 0 }, "trained no examples"},
		"failed with bundle":         {func(r *Receipt) { r.Status = "failed" }, "must not report a bundle"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := receipt(t, m)
			tc.mutate(&r)
			err := VerifyReceipt(m, r)
			if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %s", err, tc.want)
			}
		})
	}
	r := receipt(t, m)
	r.ActualParameters = map[string]any{"temperature": 0.2, "max_tokens": 64}
	r.Deviations = []string{"temperature raised to 0.2 by executor policy"}
	if err := VerifyReceipt(m, r); err != nil {
		t.Fatalf("recorded deviation refused: %v", err)
	}
	failed := receipt(t, m)
	failed.Status, failed.BundleDigest = "failed", ""
	if err := VerifyReceipt(m, failed); err != nil {
		t.Fatalf("failed receipt is a record, not a refusal: %v", err)
	}
}

func TestTeacherOutputsArePredictionsAndNeverHeldOut(t *testing.T) {
	m := fixture()
	good := []Prediction{{ExampleID: "ex-1", Kind: PredictionKind, Teacher: "teacher-a", Output: map[string]any{"label": "review", "p": 0.9}}}
	set, err := NewPredictionSet(m, good)
	if err != nil || set.TeacherEgress != EgressLoopback || set.PromptVersion != "prompt:3" {
		t.Fatalf("%+v %v", set, err)
	}
	cases := map[string]struct {
		p    Prediction
		want string
	}{
		"as label":       {Prediction{ExampleID: "ex-1", Kind: "reviewer_assessment", Teacher: "teacher-a", Output: map[string]any{}}, "teacher outputs are predictions"},
		"as observation": {Prediction{ExampleID: "ex-1", Kind: "observed_outcome", Teacher: "teacher-a", Output: map[string]any{}}, "teacher outputs are predictions"},
		"other teacher":  {Prediction{ExampleID: "ex-1", Kind: PredictionKind, Teacher: "teacher-b", Output: map[string]any{}}, "not the chosen teacher"},
		"held-out":       {Prediction{ExampleID: "ex-9", Kind: PredictionKind, Teacher: "teacher-a", Output: map[string]any{}}, "held-out example"},
		"no output":      {Prediction{ExampleID: "ex-1", Kind: PredictionKind, Teacher: "teacher-a"}, "no output"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NewPredictionSet(m, []Prediction{tc.p})
			if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %s", err, tc.want)
			}
		})
	}
}

func TestComparisonNamesEveryTeacherOnOnePicture(t *testing.T) {
	a := fixture()
	b := fixture()
	b.Teacher.Chosen = "teacher-b"
	b.Teacher.Rejected = []Rejection{{Alias: "teacher-a", Reason: "compared against a second teacher"}}
	b.TeacherEgressAtGeneration = EgressOffHost
	picture := strings.Repeat("f", 64)
	entries := []Entry{
		{Role: EntryStudent, Alias: "student-a", ManifestPin: pin(t, a), Metrics: map[string]float64{"recall": 0.8}},
		{Role: EntryStudent, Alias: "student-b", ManifestPin: pin(t, b), Metrics: map[string]float64{"recall": 0.7}},
		{Role: EntryTeacher, Alias: "teacher-a", Metrics: map[string]float64{"recall": 0.9}},
		{Role: EntryTeacher, Alias: "teacher-b", Metrics: map[string]float64{"recall": 0.85}},
		{Role: EntryIncumbent, Alias: "svm", ReleaseID: "release:1", Metrics: map[string]float64{"recall": 0.75}},
	}
	c, err := NewComparison(picture, "evaluation:1", []Manifest{a, b}, entries)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(c.Teachers, ",") != "teacher-a,teacher-b" {
		t.Fatalf("teachers %v", c.Teachers)
	}
	if _, err := NewComparison(picture, "evaluation:1", []Manifest{a, b}, entries[:4]); err == nil || !strings.Contains(err.Error(), "exactly one incumbent") {
		t.Fatalf("%v", err)
	}
	if _, err := NewComparison(picture, "evaluation:1", []Manifest{a, b}, append(entries[:2], entries[2], entries[4])); err == nil || !strings.Contains(err.Error(), `teacher "teacher-b" has no entry`) {
		t.Fatalf("%v", err)
	}
	narrow := fixture()
	narrow.Data.Tasks[0].CohortIDs = []string{"cohort-1"}
	narrow.Exports = narrow.Exports[:1]
	if _, err := NewComparison(picture, "evaluation:1", []Manifest{a, narrow}, entries); err == nil || !strings.Contains(err.Error(), "different data selections") {
		t.Fatalf("%v", err)
	}
}

func TestPromotionScopeFollowsTheSelection(t *testing.T) {
	m := fixture()
	scope := PromotionScope(m)
	if len(scope) != 1 || scope[0] != "task-v1" {
		t.Fatalf("%v", scope)
	}
	r := weightsbundle.Release{TaskIDs: scope}
	if !r.ServesTask("task-v1") || r.ServesTask("task-v2") {
		t.Fatal("release scope")
	}
}

func TestCandidateScopeIsBoundToTheSelection(t *testing.T) {
	m := fixture()
	p := pin(t, m)
	good := weightsbundle.Record{"training_manifest_sha256": p, "task_ids": []any{"task-v1"}}
	if err := CheckCandidateScope(m, good); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]weightsbundle.Record{
		"other task":  {"training_manifest_sha256": p, "task_ids": []any{"task-v1", "task-v2"}},
		"no scope":    {"training_manifest_sha256": p},
		"other pin":   {"training_manifest_sha256": strings.Repeat("0", 64), "task_ids": []any{"task-v1"}},
		"missing pin": {"task_ids": []any{"task-v1"}},
	} {
		if err := CheckCandidateScope(m, c); !errors.Is(err, ErrRefused) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
