// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package weightsbundle

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func stageBundle(t *testing.T, files map[string]string, roles map[string]string) (string, Manifest) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "staging")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for p, content := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m, err := Build(root, roles)
	if err != nil {
		t.Fatal(err)
	}
	return root, m
}

var svmRoles = map[string]string{"model.json": RoleWeights, "runtime.json": RoleRuntime, "calibration/thresholds.json": RoleCalibration}
var svmFiles = map[string]string{"model.json": `{"schema":1,"kind":"linear-svm"}`, "runtime.json": `{"python":"3.12"}`, "calibration/thresholds.json": `{"threshold":0}`}

func TestBuildOrdersByPathAndRefusesStrays(t *testing.T) {
	root, m := stageBundle(t, svmFiles, svmRoles)
	if got := []string{m.Files[0].Path, m.Files[1].Path, m.Files[2].Path}; got[0] != "calibration/thresholds.json" || got[1] != "model.json" || got[2] != "runtime.json" {
		t.Fatalf("order %v", got)
	}
	if err := Verify(root, m); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "stray.bin"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(root, svmRoles); err == nil || !strings.Contains(err.Error(), `extra "stray.bin"`) {
		t.Fatalf("stray accepted: %v", err)
	}
	if _, err := Build(root, map[string]string{"model.json": RoleWeights, "missing.json": RoleRuntime, "stray.bin": RoleTokenizer, "runtime.json": RoleRuntime, "calibration/thresholds.json": RoleCalibration}); err == nil || !strings.Contains(err.Error(), `missing "missing.json"`) {
		t.Fatalf("missing accepted: %v", err)
	}
}

func TestVerifyNamesMissingExtraAndAlteredFiles(t *testing.T) {
	root, m := stageBundle(t, svmFiles, svmRoles)
	cases := []struct {
		name, want string
		mutate     func()
	}{
		{"missing", `missing "runtime.json"`, func() { os.Remove(filepath.Join(root, "runtime.json")) }},
		{"extra", `extra "extra.json"`, func() { os.WriteFile(filepath.Join(root, "extra.json"), []byte("{}"), 0o600) }},
		{"altered-size", `altered "model.json" (size)`, func() { os.WriteFile(filepath.Join(root, "model.json"), []byte("{}"), 0o600) }},
		{"altered-digest", `altered "model.json" (digest)`, func() {
			os.WriteFile(filepath.Join(root, "model.json"), []byte(strings.Replace(svmFiles["model.json"], "linear", "LINEAR", 1)), 0o600)
		}},
		{"symlink", `not a regular file`, func() {
			os.Remove(filepath.Join(root, "runtime.json"))
			os.Symlink("model.json", filepath.Join(root, "runtime.json"))
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fresh, _ := stageBundle(t, svmFiles, svmRoles)
			root = fresh
			c.mutate()
			err := Verify(root, m)
			if !errors.Is(err, ErrVerification) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %s", err, c.want)
			}
		})
	}
}

func TestManifestValidation(t *testing.T) {
	good := File{Path: "model.json", Role: RoleWeights, Size: 1, SHA256: strings.Repeat("a", 64)}
	cases := map[string]Manifest{
		"schema":        {Schema: 2, Kind: Kind, Files: []File{good}},
		"kind":          {Schema: 1, Kind: "other", Files: []File{good}},
		"empty":         {Schema: 1, Kind: Kind},
		"no-weights":    {Schema: 1, Kind: Kind, Files: []File{{Path: "r", Role: RoleRuntime, Size: 1, SHA256: good.SHA256}}},
		"dup-path":      {Schema: 1, Kind: Kind, Files: []File{good, {Path: "model.json", Role: RoleRuntime, Size: 1, SHA256: good.SHA256}}},
		"dup-role":      {Schema: 1, Kind: Kind, Files: []File{good, {Path: "other", Role: RoleWeights, Size: 1, SHA256: good.SHA256}}},
		"bad-role":      {Schema: 1, Kind: Kind, Files: []File{good, {Path: "other", Role: "notes", Size: 1, SHA256: good.SHA256}}},
		"manifest-path": {Schema: 1, Kind: Kind, Files: []File{{Path: ManifestName, Role: RoleWeights, Size: 1, SHA256: good.SHA256}}},
		"abs-path":      {Schema: 1, Kind: Kind, Files: []File{{Path: "/etc/passwd", Role: RoleWeights, Size: 1, SHA256: good.SHA256}}},
		"dotdot":        {Schema: 1, Kind: Kind, Files: []File{{Path: "../x", Role: RoleWeights, Size: 1, SHA256: good.SHA256}}},
		"unclean":       {Schema: 1, Kind: Kind, Files: []File{{Path: "a//b", Role: RoleWeights, Size: 1, SHA256: good.SHA256}}},
		"upper-digest":  {Schema: 1, Kind: Kind, Files: []File{{Path: "m", Role: RoleWeights, Size: 1, SHA256: strings.Repeat("A", 64)}}},
		"short-digest":  {Schema: 1, Kind: Kind, Files: []File{{Path: "m", Role: RoleWeights, Size: 1, SHA256: "abc"}}},
		"negative-size": {Schema: 1, Kind: Kind, Files: []File{{Path: "m", Role: RoleWeights, Size: -1, SHA256: good.SHA256}}},
	}
	for name, m := range cases {
		if err := m.Validate(); !errors.Is(err, ErrInvalidManifest) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := (Manifest{Schema: 1, Kind: Kind, Files: []File{good}}).Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeManifestIsStrict(t *testing.T) {
	_, m := stageBundle(t, svmFiles, svmRoles)
	encoded, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeManifest(encoded)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := m.Digest()
	b, _ := decoded.Digest()
	if a != b {
		t.Fatal("digest changed through encode/decode")
	}
	for _, raw := range []string{
		strings.Replace(string(encoded), `"files"`, `"files":[],"x"`, 1),
		strings.Replace(string(encoded), `"path":"model.json"`, `"path":"model.json","path":"model.json"`, 1),
		strings.Replace(string(encoded), `"size":`, `"size":1.5,"size":`, 1),
		regexp.MustCompile(`"size":[0-9]+`).ReplaceAllString(string(encoded), `"size":1.5`),
		regexp.MustCompile(`"size":[0-9]+`).ReplaceAllString(string(encoded), `"size":"5"`),
		string(encoded) + "{}",
	} {
		if _, err := DecodeManifest([]byte(raw)); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
}

func TestCompareNamesDivergingFiles(t *testing.T) {
	_, first := stageBundle(t, svmFiles, svmRoles)
	altered := map[string]string{}
	for k, v := range svmFiles {
		altered[k] = v
	}
	altered["model.json"] = `{"schema":1,"kind":"linear-svm","intercept":1}`
	_, second := stageBundle(t, altered, svmRoles)
	result, err := Compare(first, second)
	if err != nil {
		t.Fatal(err)
	}
	if result.Reproducible || len(result.Diverging) != 1 || result.Diverging[0] != "model.json" {
		t.Fatalf("%+v", result)
	}
	_, same := stageBundle(t, svmFiles, svmRoles)
	result, _ = Compare(first, same)
	if !result.Reproducible || len(result.Diverging) != 0 {
		t.Fatalf("%+v", result)
	}
	reordered := Manifest{Schema: 1, Kind: Kind, Files: []File{first.Files[1], first.Files[0], first.Files[2]}}
	result, _ = Compare(first, reordered)
	if result.Reproducible || strings.Join(result.Diverging, ",") != "calibration/thresholds.json,model.json" {
		t.Fatalf("order divergence %+v", result)
	}
}

func TestLoadRequiresManifestAndVerifies(t *testing.T) {
	root, m := stageBundle(t, svmFiles, svmRoles)
	if _, _, err := Load(root); !errors.Is(err, ErrVerification) {
		t.Fatalf("unpublished root loaded: %v", err)
	}
	digest, err := Stage(root, m)
	if err != nil {
		t.Fatal(err)
	}
	loaded, got, err := Load(root)
	if err != nil || got != digest || len(loaded.Files) != 3 {
		t.Fatalf("%v %s", err, got)
	}
	if _, err := Stage(root, m); !errors.Is(err, ErrExists) {
		t.Fatalf("restage: %v", err)
	}
	os.WriteFile(filepath.Join(root, "runtime.json"), []byte("{}"), 0o600)
	if _, _, err := Load(root); !errors.Is(err, ErrVerification) {
		t.Fatalf("altered bundle loaded: %v", err)
	}
}
