// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package webapi_test

import (
	"strings"
	"testing"
)

func TestStaticWorkspaceMountsDecisionConsole(t *testing.T) {
	index := readStaticAsset(t, "static/index.html")
	for _, id := range []string{
		`id="tab-decisions"`,
		`id="decisions"`,
		`id="decision-request-form"`,
		`id="registration-inspect-form"`,
		`id="registration-submit-form"`,
		`id="dataset-export-form"`,
		`id="outcome-read-form"`,
		`id="outcome-submit-form"`,
		`id="adjudication-history-form"`,
		`id="adjudication-submit-form"`,
		`/assets/decision-console.js`,
	} {
		if !strings.Contains(index, id) {
			t.Fatalf("decision console HTML missing %s", id)
		}
	}

	script := readStaticAsset(t, "static/decision-console.js")
	for _, required := range []string{
		`/api/v1/decisions`,
		`/api/v1/decision-registrations/`,
		`/api/v1/decision-registrations`,
		`/api/v1/dataset-exports`,
		`/api/v1/outcomes/`,
		`/api/v1/outcomes`,
		`/api/v1/adjudications/`,
		`/api/v1/adjudications`,
		`"idempotency-key"`,
		`cannot promote a model`,
		`Shadow measurements remain informational`,
	} {
		if !strings.Contains(index+script, required) {
			t.Fatalf("decision console missing %q", required)
		}
	}
}

func TestStaticWorkspaceMountsLearningConsole(t *testing.T) {
	index := readStaticAsset(t, "static/index.html")
	for _, id := range []string{
		`id="tab-learning"`,
		`id="learning"`,
		`id="learning-export-form"`,
		`id="learning-job-inspect-form"`,
		`id="learning-job-compose-form"`,
		`id="learning-predictions-form"`,
		`id="learning-policy-form"`,
		`id="learning-receipt-form"`,
		`id="learning-evaluations-form"`,
		`id="learning-seal-form"`,
		`id="learning-candidate-form"`,
		`id="learning-evaluation-form"`,
		`id="learning-predict-form"`,
		`id="learning-promote-form"`,
		`id="learning-rollback-form"`,
		`id="learning-evidence-form"`,
		`id="learning-bundle-export-form"`,
		`id="learning-bundle-import-form"`,
		`data-learning-load="datasets"`,
		`data-learning-load="jobs"`,
		`data-learning-load="candidates"`,
		`data-learning-load="releases"`,
		`data-learning-load="bundles"`,
		`/assets/learning-console.js`,
		`Shadow evidence cannot promote a model or exclude review`,
		`creates no authority`,
	} {
		if !strings.Contains(index, id) {
			t.Fatalf("learning console HTML missing %s", id)
		}
	}
	script := readStaticAsset(t, "static/learning-console.js")
	for _, required := range []string{
		`/api/v1/learning`,
		`datasets/exports`,
		`/jobs`,
		`/receipt`,
		`/candidates`,
		`/evaluations`,
		`releases/promote`,
		`candidates/seal`,
		`serving/predict`,
		`releases/rollback`,
		`/evidence/`,
		`bundles/export`,
		`bundles/import`,
		`shown verbatim`,
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("learning console script missing %q", required)
		}
	}
	app := readStaticAsset(t, "static/app.js")
	for _, required := range []string{`decision_learning`, `applyLearningCapability`, `learning-console-status`} {
		if !strings.Contains(app, required) {
			t.Fatalf("app.js missing %q", required)
		}
	}
}
