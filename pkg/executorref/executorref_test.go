// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

package executorref_test

import (
	"testing"

	"github.com/phrocker/shoal-oss/pkg/executorref"
	"github.com/phrocker/shoal-oss/pkg/executorref/executorreftest"
)

func TestValidExecutorRef(t *testing.T) {
	for _, probe := range executorreftest.Probes() {
		err := executorref.ValidExecutorRef(probe.Ref)
		if (err == nil) != probe.Valid {
			t.Errorf("%s (%q): ValidExecutorRef = %v, want valid=%v",
				probe.Name, probe.Ref, err, probe.Valid)
		}
	}
}
