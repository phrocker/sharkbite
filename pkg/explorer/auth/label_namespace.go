/*
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements. See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership. The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License. You may obtain a copy of the License at
 *
 *     https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied. See the License for the
 * specific language governing permissions and limitations
 * under the License.
 */

package auth

import (
	"bytes"
	"strconv"

	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Grant-policy identities in the label namespace stand for one free-form
// visibility label on one source (issue #570). They are minted only by
// authorized.LabelPolicyID and granted only from the operator's label grant
// file, so every other path that names a grant policy must keep out of this
// namespace. The namespace is defined here, beside Decision, so that both the
// decision constructor and the ingest selectors can refuse it.
const (
	// LabelPolicyNamespace opens every grant-policy identity that is, or
	// could be mistaken for, a label policy, in any encoding version.
	LabelPolicyNamespace = "shoal.label/"

	// LabelPolicyIDPrefix opens every version-1 label policy identity.
	LabelPolicyIDPrefix = LabelPolicyNamespace + "v1/"

	// ReservedLabelPolicyIDPrefix opens the reserved, never-grantable part of
	// the label namespace. A canonical label policy ID has a decimal length
	// where this prefix has '!', and '!' is not a label character, so no
	// label can reach it.
	ReservedLabelPolicyIDPrefix = LabelPolicyIDPrefix + "!"
)

// IsLabelPolicyID reports whether id lies anywhere in the label namespace,
// canonical, reserved or malformed.
func IsLabelPolicyID(id []byte) bool {
	return bytes.HasPrefix(id, []byte(LabelPolicyNamespace))
}

// IsReservedLabelPolicyID reports whether id lies in the reserved part of the
// label namespace. Nobody may ever hold such an ID: NewDecision refuses it as
// a permitted policy identity.
func IsReservedLabelPolicyID(id []byte) bool {
	return bytes.HasPrefix(id, []byte(ReservedLabelPolicyIDPrefix))
}

// LabelPolicyID returns the grant-policy identity for one free-form label on
// one source; see authorized.LabelPolicyID, which documents the encoding. It
// lives here so that every plane that must recognize a canonical label
// policy (the dispatch confinement check among them) can parse one without
// depending on the authorized client.
func LabelPolicyID(sourceID []byte, label string) ([]byte, error) {
	if len(sourceID) == 0 {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument, "label policy source identity is required")
	}
	if err := interaction.ValidateLabel(label); err != nil {
		return nil, err
	}
	length := strconv.Itoa(len(sourceID))
	size := len(LabelPolicyIDPrefix) + len(length) + 1 + len(sourceID) + 1 + len(label)
	if size > MaxPolicyComponentBytes {
		return nil, shoal.NewError(
			shoal.ErrorInvalidArgument,
			"label policy identity exceeds the policy component byte bound",
		)
	}
	id := make([]byte, 0, size)
	id = append(id, LabelPolicyIDPrefix...)
	id = append(id, length...)
	id = append(id, '/')
	id = append(id, sourceID...)
	id = append(id, '/')
	id = append(id, label...)
	return id, nil
}

// ParseLabelPolicyID is the inverse of LabelPolicyID. It accepts only an ID
// LabelPolicyID would emit, byte for byte, and so refuses the reserved
// namespace.
func ParseLabelPolicyID(id []byte) (sourceID []byte, label string, err error) {
	invalid := func() ([]byte, string, error) {
		return nil, "", shoal.NewError(
			shoal.ErrorInvalidArgument, "label policy identity is not canonical")
	}
	if len(id) > MaxPolicyComponentBytes ||
		!bytes.HasPrefix(id, []byte(LabelPolicyIDPrefix)) {
		return invalid()
	}
	rest := id[len(LabelPolicyIDPrefix):]
	end := bytes.IndexByte(rest, '/')
	if end <= 0 {
		return invalid()
	}
	digits := rest[:end]
	for _, character := range digits {
		if character < '0' || character > '9' {
			return invalid()
		}
	}
	if digits[0] == '0' {
		return invalid()
	}
	length, convErr := strconv.Atoi(string(digits))
	rest = rest[end+1:]
	if convErr != nil || length <= 0 || length >= len(rest) || rest[length] != '/' {
		return invalid()
	}
	sourceID = append([]byte(nil), rest[:length]...)
	label = string(rest[length+1:])
	canonical, encodeErr := LabelPolicyID(sourceID, label)
	if encodeErr != nil || !bytes.Equal(canonical, id) {
		return invalid()
	}
	return sourceID, label, nil
}
