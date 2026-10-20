// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package distillation

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/phrocker/shoal-oss/internal/weightsbundle"
)

// The Python worker in experiments/local-ml/distill.py emits records this
// package verifies: the manifest pins identically, the receipt satisfies the
// contract, and teacher outputs are predictions on non-held-out examples.
func TestPythonTrainerOutputsSatisfyTheContract(t *testing.T) {
	read := func(name string) []byte {
		raw, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	manifest, err := Decode(read("python-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	pin, err := manifest.Pin()
	if err != nil {
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
	var set PredictionSet
	if err := weightsbundle.StrictUnmarshal(read("python-predictions.json"), &set); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := NewPredictionSet(manifest, set.Predictions)
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
