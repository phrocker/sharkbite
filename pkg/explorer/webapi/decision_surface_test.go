// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package webapi

import "testing"

func TestDecisionSurfaceRejectsIncompleteCompositionBeforeMounting(t *testing.T) {
	var surface DecisionSurface
	if err := surface.validate(); err == nil {
		t.Fatal("incomplete decision surface was accepted")
	}
}
