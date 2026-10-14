// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.

package eval

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/phrocker/shoal-oss/internal/decisionlinear"
	"github.com/phrocker/shoal-oss/internal/routerwire"
	"github.com/phrocker/shoal-oss/pkg/decision"
	"github.com/phrocker/shoal-oss/pkg/router"
)

var update = flag.Bool("update", false, "rewrite the trained model and evaluation reports")

const (
	fixtureDir = "../testdata/eval/v1"
	modelPath  = "../testdata/model/router-pair-v1.json"
)

func load(t testing.TB) (*World, *Cases) {
	t.Helper()
	w, err := LoadWorld(os.DirFS(fixtureDir))
	if err != nil {
		t.Fatal(err)
	}
	cases, err := LoadCases(os.DirFS(fixtureDir))
	if err != nil {
		t.Fatal(err)
	}
	return w, cases
}

// NewProvider loads a decisionlinear model for the evaluation release.
func NewProvider(model []byte) (*decisionlinear.Provider, error) {
	sum := sha256.Sum256(model)
	return decisionlinear.New(decisionlinear.Config{ModelBytes: model, ExpectedSHA256: hex.EncodeToString(sum[:]), ReleaseID: ReleaseID})
}

func trainModel(t testing.TB, w *World, cases *Cases) []byte {
	t.Helper()
	// Features do not depend on the model, so any provider serves the
	// analysis; the runner's decider is not used here.
	r := &Runner{World: w, validator: routerwire.Validator(), catalogs: map[string]*router.Catalog{}}
	train, err := cases.Split("train")
	if err != nil {
		t.Fatal(err)
	}
	examples, err := r.Examples(train)
	if err != nil {
		t.Fatal(err)
	}
	config := router.DefaultTrainConfig()
	weights, bias := router.Train(examples, config)
	task, err := router.TaskSpec()
	if err != nil {
		t.Fatal(err)
	}
	model, err := router.ModelJSON(task, weights, bias, cases.Digest("train"), config)
	if err != nil {
		t.Fatal(err)
	}
	return model
}

// TestModelArtifactIsReproducible retrains from the train split alone and
// requires the checked-in model to be byte-identical.
func TestModelArtifactIsReproducible(t *testing.T) {
	w, cases := load(t)
	model := trainModel(t, w, cases)
	if *update {
		if err := os.WriteFile(modelPath, model, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := os.ReadFile(modelPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, model) {
		t.Fatalf("checked-in model differs from a retrain on the train split; run with -update and review")
	}
	var fields map[string]any
	if err := json.Unmarshal(model, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["dataset_sha256"] != cases.Digest("train") || fields["recipe_sha256"] != router.DefaultTrainConfig().Digest() {
		t.Fatal("model does not pin the train split and recipe")
	}
}

// Golden identities of the checked-in model.
const (
	goldenModelSHA256 = "f11fa3aca2f97d33cbd72d674d26b01fe4c4dc074bf6068de312bc63c1d82a08"
	// goldenPredictorID is the provider's predictor identity on the
	// recorded environment. The identity includes the serving environment
	// (Go version, OS, architecture), so it is computed for that environment
	// with decisionlinear.IdentityFor and checked on every toolchain.
	goldenGoVersion   = "go1.26.4"
	goldenGOOS        = "linux"
	goldenGOARCH      = "amd64"
	goldenPredictorID = "decision:predictor:v1:b17d76b7609525501b7adce5fcd62cbab6de6e58217ad992634487ef486d3127"
)

func TestGoldenPredictorID(t *testing.T) {
	model, err := os.ReadFile(modelPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(model)
	if got := hex.EncodeToString(sum[:]); got != goldenModelSHA256 {
		t.Fatalf("model SHA-256 = %s, want %s", got, goldenModelSHA256)
	}
	provider, err := NewProvider(model)
	if err != nil {
		t.Fatal(err)
	}
	identity := provider.Identity()
	config := identity.Config()
	if config.Provider != "local-linear-svm" || config.WeightsDigest != goldenModelSHA256 ||
		string(config.PreprocessingID) != router.FeatureSchemaID || config.Device != "cpu" {
		t.Fatalf("predictor config = %+v", config)
	}
	pinned, err := decisionlinear.IdentityFor(goldenModelSHA256, router.FeatureSchemaID, goldenGoVersion, goldenGOOS, goldenGOARCH)
	if err != nil {
		t.Fatal(err)
	}
	if pinned.ID() != goldenPredictorID {
		t.Fatalf("predictor ID on %s/%s/%s = %s, want %s", goldenGoVersion, goldenGOOS, goldenGOARCH, pinned.ID(), goldenPredictorID)
	}
	// On the running toolchain the identity is the same function of its own
	// environment.
	running, err := decisionlinear.IdentityFor(goldenModelSHA256, router.FeatureSchemaID, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	if err != nil || identity.ID() != running.ID() {
		t.Fatalf("provider identity %s, IdentityFor running %s (%v)", identity.ID(), running.ID(), err)
	}
	task, _ := router.TaskSpec()
	if _, err := decision.NewTaskSpec(task.Config()); err != nil {
		t.Fatal(err)
	}
}

func runSplit(t *testing.T, split string) (*World, []Outcome) {
	t.Helper()
	w, cases := load(t)
	model, err := os.ReadFile(modelPath)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := NewProvider(model)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := cases.PreRegistered(split)
	if err != nil {
		t.Fatal(err)
	}
	outcomes, err := NewRunner(w, routerwire.Predictor(provider, ReleaseID), routerwire.Validator()).Run(context.Background(), selected)
	if err != nil {
		t.Fatal(err)
	}
	return w, outcomes
}

func report(t *testing.T, split string) {
	w, outcomes := runSplit(t, split)
	reports := []Report{
		w.Measure(split, "router", outcomes, func(o Outcome) router.Proposal { return o.Router }),
		w.Measure(split, "baseline", outcomes, func(o Outcome) router.Proposal { return o.Baseline }),
	}
	text := Markdown(reports, w.Compare(outcomes))
	t.Log("\n" + text)
	for _, o := range outcomes {
		if err := o.Router.Validate(); err != nil {
			t.Fatalf("case %s: invalid proposal: %v", o.Case.ID, err)
		}
		if err := o.Baseline.Validate(); err != nil {
			t.Fatalf("case %s: invalid baseline proposal: %v", o.Case.ID, err)
		}
		reasonDiffers := !o.Case.Answerable() && len(o.Router.Reasons) == 1 && o.Router.Reasons[0] != o.Case.Expected.Reason
		if testing.Verbose() && (!w.Correct(o.Case, o.Router) || reasonDiffers) {
			t.Logf("router wrong %s %q (%s): got %s %s %v %v", o.Case.ID, o.Case.Text, o.Case.Caller, o.Router.Kind, targetKey(o.Router), w.SlotValues(o.Router), o.Router.Reasons)
		}
	}
	path := filepath.Join(fixtureDir, "report-"+split+".md")
	if *update {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The reports are goldens: a change to the router, its fixtures or its
	// model that moves any number fails here until the report is rewritten
	// with -update, so the change is visible in review. The test report is
	// the pre-registered run; rewriting it is a new result, not a correction.
	stored, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(stored) != text {
		t.Fatalf("%s differs from this run; rerun with -update and review", path)
	}
}

func TestEvaluationTrain(t *testing.T) { report(t, "train") }
func TestEvaluationDev(t *testing.T)   { report(t, "dev") }

// TestEvaluationTest runs the held-out test split. PreRegistered releases it
// only when its digest is the one recorded in docs/router-evaluation.md
// before the first run; until a digest is registered it is skipped.
func TestEvaluationTest(t *testing.T) {
	if PreRegisteredTestDigest == "" {
		t.Skip("the test split is not pre-registered yet")
	}
	report(t, "test")
}
