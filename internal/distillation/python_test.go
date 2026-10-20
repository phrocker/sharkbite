// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package distillation

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/phrocker/shoal-oss/internal/weightsbundle"
)

// The Python worker in experiments/local-ml/distill.py emits records this
// package verifies: the manifest pins identically (its raw bytes are its
// pin), the receipt satisfies the contract, and teacher outputs are
// predictions on admitted, non-held-out examples. The checked-in records
// come from distill_fixture.py; when python3 is available the worker is
// also run live and its fresh output verified the same way.
func TestPythonTrainerOutputsSatisfyTheContract(t *testing.T) {
	verifyPythonRecords(t, "testdata")
}

func TestPythonTrainerRunsLiveAgainstTheContract(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not available")
	}
	worker := filepath.Join("..", "..", "experiments", "local-ml")
	out := t.TempDir()
	cmd := exec.Command(python, "distill_fixture.py", out)
	cmd.Dir = worker
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, output)
	}
	verifyPythonRecords(t, out)
}

func verifyPythonRecords(t *testing.T, dir string) {
	t.Helper()
	read := func(name string) []byte {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	rawManifest := read("python-manifest.json")
	manifest, err := Decode(rawManifest)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := manifest.Pin()
	if err != nil {
		t.Fatal(err)
	}
	// The trainer refuses a manifest whose raw bytes do not hash to the pin,
	// so the fixture must be exactly canonical, with no trailing newline.
	if weightsbundle.SHA256Hex(rawManifest) != pin {
		t.Fatalf("manifest bytes hash to %s, go pin %s: the file is not canonical", weightsbundle.SHA256Hex(rawManifest), pin)
	}
	var export ExportDescriptor
	if err := weightsbundle.StrictUnmarshal(read("python-export.json"), &export); err != nil {
		t.Fatal(err)
	}
	if _, err := NewJob(manifest, []ExportDescriptor{export}); err != nil {
		t.Fatal(err)
	}
	var receipt Receipt
	if err := weightsbundle.StrictUnmarshal(read("python-receipt.json"), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.ManifestPin != pin {
		t.Fatalf("python pinned %s, go pinned %s", receipt.ManifestPin, pin)
	}
	if err := VerifyReceipt(manifest, receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Status != StatusCompleted || len(receipt.Deviations) != 0 || receipt.HeldOutExcludedCount != 0 {
		t.Fatalf("%+v", receipt)
	}
	var set PredictionSet
	if err := weightsbundle.StrictUnmarshal(read("python-predictions.json"), &set); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := NewPredictionSet(manifest, []ExportDescriptor{export}, set.Predictions)
	if err != nil {
		t.Fatal(err)
	}
	wantID, _ := rebuilt.ID()
	gotID, _ := set.ID()
	if wantID != gotID || receipt.TeacherPredictionsID != gotID {
		t.Fatalf("prediction set identity: python %s go %s receipt %s", gotID, wantID, receipt.TeacherPredictionsID)
	}
	if err := CheckCandidateScope(manifest, weightsbundle.Record{"training_manifest_sha256": pin, "task_ids": []any{"synthetic-task"}}); err != nil {
		t.Fatal(err)
	}
}
