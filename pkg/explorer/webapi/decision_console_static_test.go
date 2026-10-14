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
