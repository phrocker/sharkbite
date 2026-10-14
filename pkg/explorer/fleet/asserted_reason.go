// Licensed to the Apache Software Foundation (ASF) under one or more
// contributor license agreements. See the NOTICE file distributed with this
// work for additional information regarding copyright ownership.
package fleet

import (
	"strconv"

	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	// ReasonCodeATPLApply is the reason code `shoalctl policy apply` sends on
	// every registration it makes. Its reason detail must be the compiled
	// policy digest, which the durable lifecycle receipt retains verbatim.
	ReasonCodeATPLApply = "atpl-apply"
	// ATPLPolicyDigestPrefix namespaces an ATPL policy content digest. It
	// matches pkg/atpl.DigestPrefix, which cannot be imported here.
	ATPLPolicyDigestPrefix = "atpl:policy:v1:"
	atplPolicyDigestHexLen = 64
)

// CallerAssertedRegistryReason converts a registry request's reason code and
// detail into the caller-asserted reason its lifecycle receipt records.
//
// The value is an assertion by the authenticated caller, not something Shoal
// verifies, and nothing authorizes on it. It is shaped so no free text reaches
// the durable record:
//
//   - reason code "atpl-apply" requires detail exactly
//     "atpl:policy:v1:<64 lowercase hex>", retained verbatim as Source, and
//     refuses anything else;
//   - any other code keeps its detail only as a SHA-256 digest, as the
//     dispatch path does.
//
// The code itself is retained verbatim and must fit the interaction reason
// charset ([A-Za-z0-9_.:-]).
func CallerAssertedRegistryReason(
	code, detail string,
) (interaction.CallerAssertedReason, error) {
	if code == "" {
		if detail != "" {
			return interaction.CallerAssertedReason{}, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"reason detail requires a reason code",
			)
		}
		return interaction.CallerAssertedReason{}, nil
	}
	var reason interaction.CallerAssertedReason
	if code == ReasonCodeATPLApply {
		if !IsATPLPolicyDigest(detail) {
			return interaction.CallerAssertedReason{}, shoal.NewError(
				shoal.ErrorInvalidArgument,
				"reason detail for reason code "+
					strconv.Quote(ReasonCodeATPLApply)+
					" must be a policy digest "+ATPLPolicyDigestPrefix+
					"<64 lowercase hex>",
			)
		}
		reason = interaction.CallerAssertedReason{Code: code, Source: detail}
	} else {
		reason = interaction.CallerAssertedReason{Code: code}
		if detail != "" {
			reason.DetailDigest = interaction.Digest(detail)
		}
	}
	if err := reason.Validate(); err != nil {
		return interaction.CallerAssertedReason{}, err
	}
	return reason, nil
}

// IsATPLPolicyDigest reports whether value is exactly
// "atpl:policy:v1:" followed by 64 lowercase hexadecimal characters.
func IsATPLPolicyDigest(value string) bool {
	if len(value) != len(ATPLPolicyDigestPrefix)+atplPolicyDigestHexLen ||
		value[:len(ATPLPolicyDigestPrefix)] != ATPLPolicyDigestPrefix {
		return false
	}
	for index := len(ATPLPolicyDigestPrefix); index < len(value); index++ {
		character := value[index]
		if (character < '0' || character > '9') &&
			(character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
