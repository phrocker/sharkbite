package deployment

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveLoadAndAtomicReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	state, err := NewState(ModeSingle)
	if err != nil {
		t.Fatal(err)
	}
	state.Generation = 10
	state.LastValidation = "single healthy"
	if err := Save(path, state); err != nil {
		t.Fatalf("initial Save: %v", err)
	}

	next, err := Transition(state, ModeAccumulo)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(path, next.State); err != nil {
		t.Fatalf("replacement Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded != next.State {
		t.Fatalf("loaded %+v, want %+v", loaded, next.State)
	}

	matches, err := filepath.Glob(filepath.Join(dir, ".state.json.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary files remain after atomic replacement: %v", matches)
	}
}

func TestSaveFailureDoesNotReplaceExistingState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	state, err := NewState(ModeSingle)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(path, state); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	invalid := state
	invalid.Authority = AuthorityAccumulo
	if err := Save(path, invalid); err == nil {
		t.Fatal("Save accepted invalid state")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("failed Save modified the existing state")
	}
}

func TestLoadRejectsCorruptState(t *testing.T) {
	valid, err := NewState(ModeSingle)
	if err != nil {
		t.Fatal(err)
	}
	validJSON, err := Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		content string
	}{
		{"malformed", `{"currentMode":`},
		{"unknown field", `{
  "currentMode": "single",
  "desiredMode": "single",
  "phase": "ready",
  "authority": "local",
  "generation": 0,
  "lastValidation": "not-run",
  "unexpected": true
}`},
		{"trailing value", string(validJSON) + `{"extra":true}`},
		{"inconsistent", `{
  "currentMode": "single",
  "desiredMode": "accumulo",
  "phase": "ready",
  "authority": "local",
  "generation": 1,
  "lastValidation": "ok"
}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("Load accepted corrupt state")
			}
		})
	}
}

func TestLoadMissingAndSaveMissingDirectoryFail(t *testing.T) {
	dir := t.TempDir()
	if _, err := Load(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("Load accepted a missing state file")
	}
	state, err := NewState(ModeSingle)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(filepath.Join(dir, "missing", "state.json"), state); err == nil {
		t.Fatal("Save created a missing parent directory")
	}
}
