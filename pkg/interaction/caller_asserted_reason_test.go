// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package interaction_test

import (
	"strings"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestCallerAssertedReasonValidate(t *testing.T) {
	digest := interaction.Digest("detail")
	source := "atpl:policy:v1:" + strings.Repeat("a", 64)
	for name, reason := range map[string]interaction.CallerAssertedReason{
		"zero":        {},
		"code only":   {Code: "operator_request"},
		"with digest": {Code: "operator_request", DetailDigest: digest},
		"with source": {Code: "atpl-apply", Source: source},
	} {
		if err := reason.Validate(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for name, reason := range map[string]interaction.CallerAssertedReason{
		"source without code": {Source: source},
		"digest without code": {DetailDigest: digest},
		"code charset":        {Code: "two words"},
		"code too long":       {Code: strings.Repeat("a", interaction.MaxVisibilityLabelSz+1)},
		"source charset":      {Code: "c", Source: "free <text>"},
		"source too long": {
			Code: "c", Source: strings.Repeat("a", interaction.MaxVisibilityLabelSz+1),
		},
		"raw detail as digest": {Code: "c", DetailDigest: "raw detail"},
		"digest and source":    {Code: "c", DetailDigest: digest, Source: source},
	} {
		if err := reason.Validate(); !shoal.IsErrorCode(
			err, shoal.ErrorInvalidArgument,
		) {
			t.Fatalf("%s: error = %v", name, err)
		}
	}
}
