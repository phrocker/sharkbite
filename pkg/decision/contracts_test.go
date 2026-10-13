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

package decision_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/document"
	"github.com/phrocker/shoal-oss/pkg/inference"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

var now = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func taskConfig() decision.TaskConfig {
	return decision.TaskConfig{OwnerID: "owner", Name: "source-priority", Version: "v1", InputSchemaID: "schema:1", EvidencePolicyID: "evidence:1", LabelPolicyID: "labels:1", EvaluationPolicyID: "evaluation:1", PredictionUnit: "subject", LabelUnit: "finding", ActionUnit: "inspection", AggregationID: "rank:1", Questions: []decision.Question{
		{ID: "priority", Kind: decision.Ordinal, RubricID: "priority:1", Labels: []string{"low", "high"}},
		{ID: "relevance", Kind: decision.Choice, RubricID: "relevance:1", Labels: []string{"yes", "no"}},
	}}
}
func fixture(t *testing.T) (inference.ContextPack, decision.PictureConfig) {
	t.Helper()
	a, err := inference.NewDocumentAnchor(document.Citation{DocumentID: "source", RevisionID: "revision", SectionID: "section", SpanID: "span", Range: document.SourceRange{Start: document.SourcePosition{Offset: 0}, End: document.SourcePosition{Offset: 4}}}, "code")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := inference.NewSnapshotPin("snapshot", now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	auth, err := inference.NewAuthPin("auth", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	pack, err := inference.NewContextPack("inspect", []inference.EvidenceAnchor{a}, nil, snap, auth, nil)
	if err != nil {
		t.Fatal(err)
	}
	task, err := decision.NewTaskSpec(taskConfig())
	if err != nil {
		t.Fatal(err)
	}
	one := uint64(1)
	return pack, decision.PictureConfig{TaskID: task.ID(), ObservationID: "run1", EnumerationID: "inventory:1", ScopeID: "scope:1", BuilderID: "builder:1", OntologyProjectionID: "no-ontology:1", InputDigest: strings.Repeat("a", 64), TokenizerID: "tokenizer:1", InputTokens: 1, TokenBudget: 100, Cutoff: now, Sources: []decision.Source{{ID: "source", ArtifactID: "source", RevisionID: "revision", Digest: strings.Repeat("b", 64), OriginID: "origin:1", AuthorityPolicyID: "authority:1", Role: decision.Observation, Control: decision.CandidateControlled, ObservedAt: now.Add(-time.Minute), ReceivedAt: now}}, Subjects: []decision.Subject{{ID: "subject1", SourceID: "source", Disposition: decision.Supported, EvidenceIDs: []shoal.ID{a.ID()}}}, Measurements: []decision.Measurement{{ID: "coverage", Unit: "subject", MethodID: "count:1", Numerator: 1, Denominator: &one}}}
}
func TestTaskCanonicalityAndIsolation(t *testing.T) {
	c := taskConfig()
	a, err := decision.NewTaskSpec(c)
	if err != nil {
		t.Fatal(err)
	}
	c.Questions[0], c.Questions[1] = c.Questions[1], c.Questions[0]
	c.Questions[0].Labels[0], c.Questions[0].Labels[1] = c.Questions[0].Labels[1], c.Questions[0].Labels[0]
	b, err := decision.NewTaskSpec(c)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID() != b.ID() {
		t.Fatal("unordered questions/choices changed identity")
	}
	c.Questions[1].Labels[0] = "changed"
	out := a.Config()
	out.Questions[0].Labels[0] = "mutated"
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	if a.Config().Questions[0].Labels[0] != "low" {
		t.Fatal("task aliases caller storage")
	}
	c = a.Config()
	c.Questions[0].Labels[0], c.Questions[0].Labels[1] = c.Questions[0].Labels[1], c.Questions[0].Labels[0]
	b, err = decision.NewTaskSpec(c)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID() == b.ID() {
		t.Fatal("ordinal order missing from identity")
	}
}
func TestTaskRejectsInvalidContracts(t *testing.T) {
	cases := map[string]func(*decision.TaskConfig){
		"no questions":       func(c *decision.TaskConfig) { c.Questions = nil },
		"unknown kind":       func(c *decision.TaskConfig) { c.Questions[0].Kind = "bogus" },
		"duplicate question": func(c *decision.TaskConfig) { c.Questions[1].ID = c.Questions[0].ID },
		"duplicate label":    func(c *decision.TaskConfig) { c.Questions[0].Labels = []string{"x", "x"} },
		"probability labels": func(c *decision.TaskConfig) { c.Questions[0].Kind = decision.Probability },
		"invalid UTF8 ID":    func(c *decision.TaskConfig) { c.OwnerID = shoal.ID(string([]byte{255})) },
		"missing policy":     func(c *decision.TaskConfig) { c.LabelPolicyID = "" },
		"too many labels":    func(c *decision.TaskConfig) { c.Questions[0].Labels = make([]string, decision.MaxLabels+1) },
		"oversize text":      func(c *decision.TaskConfig) { c.Name = strings.Repeat("x", decision.MaxManifestBytes) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			c := taskConfig()
			mutate(&c)
			if _, err := decision.NewTaskSpec(c); err == nil {
				t.Fatal("accepted invalid contract")
			}
		})
	}
	if (decision.TaskSpec{}).Validate() == nil {
		t.Fatal("accepted zero task")
	}
}
func TestPictureCopyIsolationAndCoverage(t *testing.T) {
	pack, c := fixture(t)
	c.Subjects = append(c.Subjects, decision.Subject{ID: "subject2", SourceID: "source", Disposition: decision.Unsupported, Reason: "unresolved dispatch"})
	p, err := decision.NewPictureManifest(pack, c)
	if err != nil {
		t.Fatal(err)
	}
	c.Subjects[0].EvidenceIDs[0] = "mutated"
	*c.Measurements[0].Denominator = 99
	c.Sources[0].Digest = "bad"
	out := p.Config()
	out.Subjects[0].EvidenceIDs[0] = "also mutated"
	*out.Measurements[0].Denominator = 100
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if *p.Config().Measurements[0].Denominator != 1 {
		t.Fatal("measurement aliases caller")
	}
	if got, total := p.InventoryCoverage(); got != 1 || total != 2 {
		t.Fatalf("coverage %d/%d", got, total)
	}
	if p.ContextPackID() != pack.ID() || p.Snapshot().ID() != pack.Snapshot().ID() {
		t.Fatal("lost source pins")
	}
}

func TestPictureWithoutIncidentsRetainsLegacyIdentity(t *testing.T) {
	pack, config := fixture(t)
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"Incidents"`) {
		t.Fatalf("empty incident field changed legacy JSON shape: %s", encoded)
	}
	picture, err := decision.NewPictureManifest(pack, config)
	if err != nil {
		t.Fatal(err)
	}
	const legacyID = "decision:picture:v1:868409a1a63f5386bc0869a876baf6741c2fd23467f9ea19dd5b8fa8318352ad"
	if picture.ID() != legacyID {
		t.Fatalf("legacy picture identity changed: got %s, want %s", picture.ID(), legacyID)
	}
}

func TestPictureIdentityAndChronology(t *testing.T) {
	pack, c := fixture(t)
	a, err := decision.NewPictureManifest(pack, c)
	if err != nil {
		t.Fatal(err)
	}
	zone := time.FixedZone("elsewhere", 3600)
	c.Cutoff = c.Cutoff.In(zone)
	c.Sources[0].ObservedAt = c.Sources[0].ObservedAt.In(zone)
	c.Sources[0].ReceivedAt = c.Sources[0].ReceivedAt.In(zone)
	b, err := decision.NewPictureManifest(pack, c)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID() != b.ID() {
		t.Fatal("equivalent times changed identity")
	}
	changes := map[string]func(*decision.PictureConfig){
		"new observation": func(c *decision.PictureConfig) { c.ObservationID = "run2" },
		"later receipt": func(c *decision.PictureConfig) {
			c.Cutoff = c.Cutoff.Add(time.Minute)
			c.Sources[0].ReceivedAt = c.Cutoff
		},
		"new builder":                        func(c *decision.PictureConfig) { c.BuilderID = "builder2" },
		"new authority policy":               func(c *decision.PictureConfig) { c.Sources[0].AuthorityPolicyID = "authority2" },
		"prediction rather than observation": func(c *decision.PictureConfig) { c.Sources[0].Role = decision.Prediction },
		"unknown denominator":                func(c *decision.PictureConfig) { c.Measurements[0].Denominator = nil },
		"truncation":                         func(c *decision.PictureConfig) { c.Truncated = true },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			cfg := a.Config()
			change(&cfg)
			b, err := decision.NewPictureManifest(pack, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if b.ID() == a.ID() {
				t.Fatal("changed evidence not bound")
			}
			if b.Snapshot().AsOf() != a.Snapshot().AsOf() {
				t.Fatal("rewrote source snapshot time")
			}
		})
	}
}

func linkedIncident() decision.IncidentReference {
	return decision.IncidentReference{
		IncidentID:        "incident:outage-1",
		ServiceID:         "service:api",
		EnvironmentID:     "environment:prod",
		DeploymentID:      "deployment:2026-10-13",
		ArtifactID:        "source",
		RevisionID:        "revision",
		ChangeID:          "change:pr-1",
		AuthorityPolicyID: "incident-authority:1",
		Digest:            strings.Repeat("c", 64),
		State:             decision.IncidentLinked,
		ObservedAt:        now.Add(-2 * time.Minute),
		ReceivedAt:        now.Add(-time.Minute),
	}
}

func TestPictureIncidentLineageIsBoundAndCanonical(t *testing.T) {
	pack, c := fixture(t)
	c.Incidents = []decision.IncidentReference{linkedIncident()}
	a, err := decision.NewPictureManifest(pack, c)
	if err != nil {
		t.Fatal(err)
	}
	if got := a.Config().Incidents[0].State; got != decision.IncidentLinked {
		t.Fatalf("state = %q, want linked", got)
	}
	changed := a.Config()
	changed.Incidents[0].ChangeID = "change:pr-2"
	b, err := decision.NewPictureManifest(pack, changed)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID() == b.ID() {
		t.Fatal("incident lineage substitution did not change picture identity")
	}
	changed = a.Config()
	changed.Incidents[0].ObservedAt = changed.Incidents[0].ObservedAt.In(time.FixedZone("elsewhere", 3600))
	changed.Incidents[0].ReceivedAt = changed.Incidents[0].ReceivedAt.In(time.FixedZone("elsewhere", 3600))
	b, err = decision.NewPictureManifest(pack, changed)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID() != b.ID() {
		t.Fatal("equivalent incident times changed identity")
	}
}

func TestPictureIncidentUncertaintyIsRetained(t *testing.T) {
	pack, c := fixture(t)
	c.Incidents = []decision.IncidentReference{{
		IncidentID: "incident:unresolved", Digest: strings.Repeat("d", 64),
		State: decision.IncidentUnresolved, Reason: "deployment mapping unavailable",
		ObservedAt: now.Add(-time.Minute), ReceivedAt: now,
	}}
	p, err := decision.NewPictureManifest(pack, c)
	if err != nil {
		t.Fatal(err)
	}
	got := p.Config().Incidents[0]
	if got.State != decision.IncidentUnresolved || got.Reason == "" || got.DeploymentID != "" {
		t.Fatalf("incident uncertainty was not retained: %#v", got)
	}
}

func TestPictureRejectsInvalidIncidentLineage(t *testing.T) {
	cases := map[string]func(*decision.PictureConfig){
		"duplicate": func(c *decision.PictureConfig) {
			c.Incidents = []decision.IncidentReference{linkedIncident(), linkedIncident()}
		},
		"missing linked lineage": func(c *decision.PictureConfig) {
			i := linkedIncident()
			i.RevisionID = ""
			c.Incidents = []decision.IncidentReference{i}
		},
		"linked source mismatch": func(c *decision.PictureConfig) {
			i := linkedIncident()
			i.ArtifactID, i.RevisionID = "other-artifact", "other-revision"
			c.Incidents = []decision.IncidentReference{i}
		},
		"unresolved reason": func(c *decision.PictureConfig) {
			i := linkedIncident()
			i.State, i.Reason = decision.IncidentUnresolved, ""
			c.Incidents = []decision.IncidentReference{i}
		},
		"linked reason": func(c *decision.PictureConfig) {
			i := linkedIncident()
			i.Reason = "ambiguous"
			c.Incidents = []decision.IncidentReference{i}
		},
		"future receipt": func(c *decision.PictureConfig) {
			i := linkedIncident()
			i.ReceivedAt = c.Cutoff.Add(time.Second)
			c.Incidents = []decision.IncidentReference{i}
		},
		"bad digest": func(c *decision.PictureConfig) {
			i := linkedIncident()
			i.Digest = strings.Repeat("G", 64)
			c.Incidents = []decision.IncidentReference{i}
		},
		"unknown state": func(c *decision.PictureConfig) {
			i := linkedIncident()
			i.State = "predicted"
			c.Incidents = []decision.IncidentReference{i}
		},
		"too many": func(c *decision.PictureConfig) {
			i := linkedIncident()
			c.Incidents = make([]decision.IncidentReference, decision.MaxIncidents+1)
			for n := range c.Incidents {
				i.IncidentID = shoal.ID("incident:" + string(rune('a'+n%26)) + string(rune('0'+n/26)))
				c.Incidents[n] = i
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			pack, c := fixture(t)
			mutate(&c)
			if _, err := decision.NewPictureManifest(pack, c); err == nil {
				t.Fatal("accepted invalid incident lineage")
			}
		})
	}
}
func TestPictureRejectsInvalidEvidence(t *testing.T) {
	cases := map[string]func(*decision.PictureConfig){
		"future receipt":     func(c *decision.PictureConfig) { c.Sources[0].ReceivedAt = c.Cutoff.Add(time.Second) },
		"future observation": func(c *decision.PictureConfig) { c.Sources[0].ObservedAt = c.Sources[0].ReceivedAt.Add(time.Second) },
		"zero time":          func(c *decision.PictureConfig) { c.Cutoff = time.Time{} },
		"expired auth":       func(c *decision.PictureConfig) { c.Cutoff = now.Add(time.Hour) },
		"predates snapshot":  func(c *decision.PictureConfig) { c.Cutoff = now.Add(-2 * time.Hour) },
		"unbound evidence":   func(c *decision.PictureConfig) { c.Subjects[0].EvidenceIDs = []shoal.ID{"elsewhere"} },
		"missing evidence":   func(c *decision.PictureConfig) { c.Subjects[0].EvidenceIDs = nil },
		"duplicate anchor": func(c *decision.PictureConfig) {
			c.Subjects[0].EvidenceIDs = append(c.Subjects[0].EvidenceIDs, c.Subjects[0].EvidenceIDs[0])
		},
		"duplicate subject":    func(c *decision.PictureConfig) { c.Subjects = append(c.Subjects, c.Subjects[0]) },
		"duplicate source":     func(c *decision.PictureConfig) { c.Sources = append(c.Sources, c.Sources[0]) },
		"substituted revision": func(c *decision.PictureConfig) { c.Sources[0].RevisionID = "another-revision" },
		"substituted document": func(c *decision.PictureConfig) { c.Sources[0].ArtifactID = "another-document" },
		"unknown source":       func(c *decision.PictureConfig) { c.Subjects[0].SourceID = "unknown" },
		"unknown role":         func(c *decision.PictureConfig) { c.Sources[0].Role = "safe" },
		"unknown control":      func(c *decision.PictureConfig) { c.Sources[0].Control = "trusted" },
		"unknown disposition":  func(c *decision.PictureConfig) { c.Subjects[0].Disposition = "safe" },
		"unaccounted anchors": func(c *decision.PictureConfig) {
			c.Subjects[0].EvidenceIDs = nil
			c.Subjects[0].Disposition = decision.Missing
			c.Subjects[0].Reason = "absent"
		},
		"missing reason":        func(c *decision.PictureConfig) { c.Subjects[0].Disposition = decision.Unsupported },
		"false coverage":        func(c *decision.PictureConfig) { c.Measurements[0].Numerator = 2 },
		"duplicate measurement": func(c *decision.PictureConfig) { c.Measurements = append(c.Measurements, c.Measurements[0]) },
		"bad digest":            func(c *decision.PictureConfig) { c.InputDigest = strings.Repeat("G", 64) },
		"too many tokens":       func(c *decision.PictureConfig) { c.InputTokens = 101 },
		"unknown budget":        func(c *decision.PictureConfig) { c.TokenBudget = 0 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			pack, c := fixture(t)
			mutate(&c)
			if _, err := decision.NewPictureManifest(pack, c); err == nil {
				t.Fatal("accepted invalid picture")
			}
		})
	}
	if (decision.PictureManifest{}).Validate() == nil {
		t.Fatal("accepted zero picture")
	}
}
func TestUnknownAndZeroDenominatorsStayDistinct(t *testing.T) {
	pack, c := fixture(t)
	c.Measurements[0].Numerator = 0
	c.Measurements[0].Denominator = nil
	unknown, err := decision.NewPictureManifest(pack, c)
	if err != nil {
		t.Fatal(err)
	}
	zero := uint64(0)
	c.Measurements[0].Denominator = &zero
	na, err := decision.NewPictureManifest(pack, c)
	if err != nil {
		t.Fatal(err)
	}
	if unknown.ID() == na.ID() {
		t.Fatal("unknown denominator collapsed to known zero")
	}
	if unknown.Config().Measurements[0].Denominator != nil {
		t.Fatal("unknown became complete")
	}
}
func TestNonCodeTaskUsesSameContracts(t *testing.T) {
	c := taskConfig()
	c.Name = "service-restart-prerequisites"
	c.PredictionUnit = "operation"
	c.LabelUnit = "verified prerequisite"
	c.ActionUnit = "inspection"
	c.Questions = []decision.Question{{ID: "capacity_verified", Kind: decision.Probability, RubricID: "capacity:1"}}
	task, err := decision.NewTaskSpec(c)
	if err != nil {
		t.Fatal(err)
	}
	pack, picture := fixture(t)
	picture.TaskID = task.ID()
	picture.Sources[0].Role = decision.Normative
	picture.Sources[0].Control = decision.RegistryControlled
	if _, err := decision.NewPictureManifest(pack, picture); err != nil {
		t.Fatal(err)
	}
}

func TestPictureSetOrderAndPackPins(t *testing.T) {
	pack, c := fixture(t)
	c.Sources = append(c.Sources, c.Sources[0])
	c.Sources[1].ID = "second-observation"
	c.Subjects = append(c.Subjects, decision.Subject{ID: "second-subject", SourceID: "second-observation", Disposition: decision.Unsupported, Reason: "missing type resolution"})
	c.Measurements = append(c.Measurements, decision.Measurement{ID: "unresolved", Unit: "reference", MethodID: "parser:1"})
	a, err := decision.NewPictureManifest(pack, c)
	if err != nil {
		t.Fatal(err)
	}
	c.Sources[0], c.Sources[1] = c.Sources[1], c.Sources[0]
	c.Subjects[0], c.Subjects[1] = c.Subjects[1], c.Subjects[0]
	c.Measurements[0], c.Measurements[1] = c.Measurements[1], c.Measurements[0]
	b, err := decision.NewPictureManifest(pack, c)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID() != b.ID() {
		t.Fatal("set order changed picture identity")
	}
	auth, err := inference.NewAuthPin("different-scope", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	other, err := inference.NewContextPack(pack.Query(), pack.Evidence(), nil, pack.Snapshot(), auth, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err = decision.NewPictureManifest(other, c)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID() == b.ID() {
		t.Fatal("authorization pin missing from picture identity")
	}
}

func TestAggregatePreflightBound(t *testing.T) {
	c := taskConfig()
	c.Questions = nil
	for i := 0; i < decision.MaxQuestions; i++ {
		labels := make([]string, decision.MaxLabels)
		for j := range labels {
			labels[j] = strings.Repeat("x", shoal.MaxSemanticStringBytes)
		}
		c.Questions = append(c.Questions, decision.Question{ID: "q", Kind: decision.Choice, RubricID: "r", Labels: labels})
	}
	if _, err := decision.NewTaskSpec(c); err == nil {
		t.Fatal("accepted aggregate oversized task")
	}
	pack, p := fixture(t)
	p.Subjects = make([]decision.Subject, decision.MaxSubjects)
	for i := range p.Subjects {
		p.Subjects[i] = decision.Subject{ID: "s", SourceID: "source", Reason: strings.Repeat("x", shoal.MaxSemanticStringBytes), Disposition: decision.Unsupported}
	}
	if _, err := decision.NewPictureManifest(pack, p); err == nil {
		t.Fatal("accepted aggregate oversized picture")
	}
}
