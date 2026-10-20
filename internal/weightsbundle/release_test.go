// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package weightsbundle

import (
	"errors"
	"strings"
	"testing"
)

func TestPromoteMatchesPythonReleaseIdentity(t *testing.T) {
	c := loadChain(t)
	first, err := Promote(PromotionInput{Candidate: c["candidate"], Evaluation: c["evaluation"], Artifact: c["artifact"], Approval: c["approval"]})
	if err != nil {
		t.Fatal(err)
	}
	if want := c.release(t, "first_release"); first.ID != want.ID {
		t.Fatalf("go release %s, python release %s", first.ID, want.ID)
	}
	if first.BundleDigest == nil || *first.BundleDigest != stringFieldChain(c, "bundle_digest") || len(first.TaskIDs) != 1 || first.TaskIDs[0] != "task-v1" {
		t.Fatalf("%+v", first)
	}
	second, err := Promote(PromotionInput{Candidate: c["candidate"], Evaluation: c["evaluation"], Artifact: c["artifact"], Approval: c["approval"], Predecessor: &first})
	if err != nil {
		t.Fatal(err)
	}
	if want := c.release(t, "second_release"); second.ID != want.ID {
		t.Fatalf("second release %s, python %s", second.ID, want.ID)
	}
	rollback, err := Rollback(second, first, c["rollback_approval"])
	if err != nil {
		t.Fatal(err)
	}
	if want := c.release(t, "rollback"); rollback.ID != want.ID {
		t.Fatalf("rollback %s, python %s", rollback.ID, want.ID)
	}
	if *rollback.BundleDigest != *first.BundleDigest || *rollback.RollbackOf != second.ID {
		t.Fatalf("%+v", rollback)
	}
}

func TestPromoteRefusals(t *testing.T) {
	c := loadChain(t)
	base := func() PromotionInput {
		return PromotionInput{Candidate: clone(c["candidate"]), Evaluation: clone(c["evaluation"]), Artifact: clone(c["artifact"]), Approval: clone(c["approval"])}
	}
	cases := []struct {
		name, want string
		mutate     func(in *PromotionInput)
	}{
		{"tampered artifact", "invalid candidate_artifact identity", func(in *PromotionInput) { in.Artifact["bundle_digest"] = strings.Repeat("0", 64) }},
		{"resealed altered file", "bundle digest does not match", func(in *PromotionInput) {
			files := in.Artifact["bundle_files"].([]any)
			files[0].(map[string]any)["sha256"] = strings.Repeat("c", 64)
			in.Artifact = reseal(t, in.Artifact)
		}},
		{"weights not model digest", "weights digest does not match model digest", func(in *PromotionInput) {
			files := in.Artifact["bundle_files"].([]any)
			for _, f := range files {
				if m := f.(map[string]any); m["role"] == RoleWeights {
					m["sha256"] = strings.Repeat("c", 64)
				}
			}
			in.Artifact["bundle_digest"] = mustDigest(t, Manifest{Schema: 1, Kind: Kind, Files: manifestFiles(t, in.Artifact)})
			in.Artifact = reseal(t, in.Artifact)
		}},
		{"unbound approval", "promotion approval is not bound", func(in *PromotionInput) { in.Approval["candidate_id"] = "other"; in.Approval = reseal(t, in.Approval) }},
		{"unapproved", "promotion approval is not bound", func(in *PromotionInput) { in.Approval["approved"] = false; in.Approval = reseal(t, in.Approval) }},
		{"held evaluation", "did not pass shadow evaluation", func(in *PromotionInput) {
			in.Evaluation["disposition"] = "hold"
			in.Evaluation["reasons"] = []any{"r: quality/sample gate failed"}
			in.Evaluation = reseal(t, in.Evaluation)
			in.Artifact["evaluation_id"] = in.Evaluation["id"]
			in.Artifact = reseal(t, in.Artifact)
		}},
		{"artifact for other candidate", errCandidateLineage, func(in *PromotionInput) { in.Artifact["candidate_id"] = "other"; in.Artifact = reseal(t, in.Artifact) }},
		{"artifact for other evaluation", errArtifactEvaluationLineage, func(in *PromotionInput) { in.Artifact["evaluation_id"] = "other"; in.Artifact = reseal(t, in.Artifact) }},
		{"stale predecessor", "invalid predecessor release", func(in *PromotionInput) {
			r, _ := Promote(*in)
			r.LedgerID = "other"
			r, _ = seal(r)
			in.Predecessor = &r
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base()
			tc.mutate(&in)
			_, err := Promote(in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %s", err, tc.want)
			}
			if !strings.Contains(err.Error(), "identity") && !errors.Is(err, ErrRefused) {
				t.Fatalf("refusal not typed: %v", err)
			}
		})
	}
}

func TestRollbackRefusals(t *testing.T) {
	c := loadChain(t)
	first, second := c.release(t, "first_release"), c.release(t, "second_release")
	approval := clone(c["rollback_approval"])
	approval["prior_release_id"] = "other"
	if _, err := Rollback(second, first, reseal(t, approval)); err == nil || !strings.Contains(err.Error(), "rollback approval is not bound") {
		t.Fatalf("%v", err)
	}
	other := first
	other.LedgerID = "other"
	other, _ = seal(other)
	if _, err := Rollback(second, other, c["rollback_approval"]); err == nil || !strings.Contains(err.Error(), "rollback lineage mismatch") {
		t.Fatalf("%v", err)
	}
	tampered := second
	tampered.BundleDigest = nil
	if _, err := Rollback(tampered, first, c["rollback_approval"]); err == nil || !strings.Contains(err.Error(), "invalid model_release identity") {
		t.Fatalf("%v", err)
	}
}

func TestLegacyOneFileArtifactStillPromotes(t *testing.T) {
	c := loadChain(t)
	artifact, err := Seal(KindArtifact, map[string]any{"candidate_id": stringField(c["candidate"], "id"), "evaluation_id": stringField(c["evaluation"], "id"), "dataset_id": stringField(c["candidate"], "dataset_id"), "model_digest": strings.Repeat("a", 64), "runtime_digest": "runtime"})
	if err != nil {
		t.Fatal(err)
	}
	release, err := Promote(PromotionInput{Candidate: c["candidate"], Evaluation: c["evaluation"], Artifact: artifact, Approval: c["approval"]})
	if err != nil {
		t.Fatal(err)
	}
	if release.BundleDigest != nil {
		t.Fatal("legacy artifact acquired a bundle digest")
	}
}

func TestNewArtifactBindsBundle(t *testing.T) {
	_, m := stageBundle(t, svmFiles, svmRoles)
	artifact, err := NewArtifact("c", "e", "d", m, "runtime")
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyRecord(artifact, KindArtifact); err != nil {
		t.Fatal(err)
	}
	back, err := ArtifactManifest(artifact)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := m.Digest()
	b, _ := back.Digest()
	if a != b || stringField(artifact, "model_digest") != m.Files[1].SHA256 {
		t.Fatalf("%s %s", a, b)
	}
	if _, err := NewArtifact("", "e", "d", m, "runtime"); !errors.Is(err, ErrRefused) {
		t.Fatal(err)
	}
}

func clone(r Record) Record {
	raw, _ := Canonical(r)
	out, _ := DecodeRecord(raw)
	return out
}

func reseal(t *testing.T, r Record) Record {
	t.Helper()
	fields := map[string]any{}
	for k, v := range r {
		if k != "id" && k != "kind" && k != "schema" {
			fields[k] = v
		}
	}
	out, err := Seal(stringField(r, "kind"), fields)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func mustDigest(t *testing.T, m Manifest) string {
	t.Helper()
	d, err := m.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func manifestFiles(t *testing.T, artifact Record) []File {
	t.Helper()
	var out []File
	for _, item := range artifact["bundle_files"].([]any) {
		obj := item.(map[string]any)
		size, _ := numberInt64(obj["size"])
		out = append(out, File{Path: stringField(obj, "path"), Role: stringField(obj, "role"), Size: size, SHA256: stringField(obj, "sha256")})
	}
	return out
}
