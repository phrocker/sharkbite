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

package authorized

// Label grants (issue #570, PR3). A grant is one (source, label) pair an
// operator gives a principal; the principal then holds LabelPolicyID(source,
// label) in its decision's PermittedPolicyIDs, which is exactly the policy a
// labelled document's AccessRule conjoins. These helpers are shared by every
// command that grants labels from operator configuration (the OIDC grant
// file, -dev-auth-labels, shoal-mcp's -identity-labels), so all of them admit
// a grant by the same rules.

import (
	"bytes"
	"strings"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	// LabelGrantSeparator separates the source from the label in the textual
	// form "<source>=<label>" that command-line grants use. The label
	// charset (interaction.ValidateLabel) excludes it, so the label is
	// exactly what follows the LAST separator and the source, which may
	// itself contain the separator, is everything before it. ':' could not be
	// used: it is a label character, so "a:b:c" would have two readings.
	LabelGrantSeparator = "="

	// LabelGrantListSeparator separates grants in a list. It is not a label
	// character either; a source that contains it cannot be named in the
	// list form, and is refused as unknown rather than split.
	LabelGrantListSeparator = ","

	// MaxLabelGrants bounds the distinct label grants one principal may be
	// given, and the grants one operator grant file may hold. A decision
	// carries them beside its other policy IDs, so the bound is kept well
	// under auth.MaxDecisionGrantIDs.
	MaxLabelGrants = 1024
)

// AdmitLabelGrant mints the label policy ID for (sourceID, label) and admits
// it as a grant, or refuses. sources is the set of source IDs the command has
// configured; sourceID must equal one of them byte for byte. The label must
// pass interaction.ValidateLabel exactly as given. The ID is refused if it is
// in the reserved, never-grantable part of the label namespace, or if
// ParseLabelPolicyID does not read it back as exactly (sourceID, label).
func AdmitLabelGrant(sourceID []byte, label string, sources [][]byte) ([]byte, error) {
	return admitLabelGrant(sourceID, label, sources, LabelPolicyID)
}

// admitLabelGrant is AdmitLabelGrant with the ID constructor injected, so a
// test can show the reserved and canonical checks hold even against a
// constructor that misbehaves.
func admitLabelGrant(
	sourceID []byte, label string, sources [][]byte,
	mint func([]byte, string) ([]byte, error),
) ([]byte, error) {
	known := false
	for _, source := range sources {
		if len(source) > 0 && bytes.Equal(source, sourceID) {
			known = true
			break
		}
	}
	if !known {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument,
			"label grant names a source that is not configured")
	}
	id, err := mint(sourceID, label)
	if err != nil {
		return nil, err
	}
	return AdmitLabelPolicyID(id, sourceID, label)
}

// AdmitLabelPolicyID checks a minted label policy ID before it is granted:
// never in the reserved namespace, and canonical for exactly (sourceID,
// label). The reserved check comes first and is reported as such, so a
// reserved ID is refused for being reserved and not only for being
// non-canonical.
func AdmitLabelPolicyID(id []byte, sourceID []byte, label string) ([]byte, error) {
	if auth.IsReservedLabelPolicyID(id) {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument,
			"label grant resolves to the reserved label namespace, which can "+
				"never be granted")
	}
	parsedSource, parsedLabel, err := ParseLabelPolicyID(id)
	if err != nil || !bytes.Equal(parsedSource, sourceID) || parsedLabel != label {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument,
			"label grant does not resolve to a canonical label policy identity")
	}
	return append([]byte(nil), id...), nil
}

// ParseLabelGrant reads one "<source>=<label>" grant and admits it against
// the configured sources. Nothing is trimmed or folded.
func ParseLabelGrant(text string, sources [][]byte) ([]byte, error) {
	cut := strings.LastIndex(text, LabelGrantSeparator)
	if cut < 0 {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument,
			"label grant must have the form <source>"+LabelGrantSeparator+"<label>")
	}
	return AdmitLabelGrant(
		[]byte(text[:cut]), text[cut+len(LabelGrantSeparator):], sources)
}

// ParseLabelGrantList reads a comma-separated list of "<source>=<label>"
// grants. An empty value grants nothing. An empty entry, a duplicate grant or
// more than MaxLabelGrants grants is refused. The result is in list order.
func ParseLabelGrantList(value string, sources [][]byte) ([][]byte, error) {
	if value == "" {
		return nil, nil
	}
	entries := strings.Split(value, LabelGrantListSeparator)
	if len(entries) > MaxLabelGrants {
		return nil, shoal.NewError(shoal.ErrorInvalidArgument,
			"label grants exceed their bound")
	}
	ids := make([][]byte, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry == "" {
			return nil, shoal.NewError(shoal.ErrorInvalidArgument,
				"label grant list has an empty entry")
		}
		id, err := ParseLabelGrant(entry, sources)
		if err != nil {
			return nil, err
		}
		if _, duplicate := seen[string(id)]; duplicate {
			return nil, shoal.NewError(shoal.ErrorInvalidArgument,
				"label grant list names a grant twice")
		}
		seen[string(id)] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}
