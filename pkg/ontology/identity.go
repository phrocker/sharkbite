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

package ontology

import "github.com/phrocker/shoal-oss/pkg/shoal"

// OntologyIdentity names one immutable ontology snapshot: the schema and the
// exact version within it. Definition IDs are derived from namespace and key
// alone, so concept:Person means whatever the version holding it says it
// means. An identity is therefore the only thing that fixes which meaning was
// in force.
//
// The zero value is the explicitly unresolved identity, reported by Known as
// false. It is never equal to a resolved identity and is never filled in from
// ambient state: a value that was not recorded is unknown, not current.
type OntologyIdentity struct {
	schemaID  shoal.ID
	versionID shoal.ID
}

// UnknownOntology returns the explicitly unresolved ontology identity. It
// exists so callers can name the state rather than pass a bare zero value.
func UnknownOntology() OntologyIdentity {
	return OntologyIdentity{}
}

// NewOntologyIdentity extracts the schema and version identities from a
// validated ontology version.
func NewOntologyIdentity(version OntologyVersion) (OntologyIdentity, error) {
	if err := version.Validate(); err != nil {
		return OntologyIdentity{}, err
	}
	return NewOntologyIdentityFromIDs(version.Schema().ID(), version.ID())
}

// NewOntologyIdentityFromIDs creates an ontology identity when the caller
// already has validated schema and version identifiers.
func NewOntologyIdentityFromIDs(
	schemaID, versionID shoal.ID,
) (OntologyIdentity, error) {
	identity := OntologyIdentity{schemaID: schemaID, versionID: versionID}
	if err := identity.Validate(); err != nil {
		return OntologyIdentity{}, err
	}
	return identity, nil
}

// Validate accepts only a fully resolved identity. The unresolved identity is
// a legal state for a holder to be in, but it is not a valid identity, so
// holders test Known before validating. A half-populated identity is neither
// unresolved nor resolved and is always rejected.
func (i OntologyIdentity) Validate() error {
	if err := ValidateID(i.schemaID); err != nil {
		return err
	}
	if IDNamespace(i.schemaID) != "schema" {
		return invalid("ontology schema ID has an unexpected namespace")
	}
	if err := ValidateID(i.versionID); err != nil {
		return err
	}
	if IDNamespace(i.versionID) != "ontology-version" {
		return invalid("ontology version ID has an unexpected namespace")
	}
	return nil
}

// Known reports whether anything at all was recorded. It is deliberately true
// for a half-populated identity so that malformed input reaches Validate
// instead of being silently read as unresolved.
func (i OntologyIdentity) Known() bool {
	return i != OntologyIdentity{}
}

func (i OntologyIdentity) SchemaID() shoal.ID  { return i.schemaID }
func (i OntologyIdentity) VersionID() shoal.ID { return i.versionID }

// String renders the identity for diagnostics, naming the unresolved state
// rather than rendering as empty.
func (i OntologyIdentity) String() string {
	if !i.Known() {
		return "unknown"
	}
	return string(i.schemaID) + "/" + string(i.versionID)
}

func (i OntologyIdentity) canonical() string {
	return canonicalParts(string(i.schemaID), string(i.versionID))
}

// OntologyReading classifies a recorded ontology identity against the identity
// a reader holds. It is what makes reinterpretation under a different version
// detectable instead of silent.
type OntologyReading string

const (
	// OntologyUnresolved means the comparison could not be made, because the
	// value carries no ontology identity or because the reader holds none. It
	// is the absorbing state: an unresolved reading is never upgraded to a
	// match, and it is distinct from every other reading.
	OntologyUnresolved OntologyReading = "unresolved"
	// OntologyMalformed means an identity was recorded but is not valid, so no
	// claim about agreement can be made from it.
	OntologyMalformed OntologyReading = "malformed"
	// OntologySameVersion means the value was made under exactly the snapshot
	// the reader holds.
	OntologySameVersion OntologyReading = "same_version"
	// OntologyOtherVersion means the schema agrees but the version does not,
	// so the definitions the value refers to may have moved underneath it.
	OntologyOtherVersion OntologyReading = "other_version"
	// OntologyOtherSchema means the value was made under a different schema
	// entirely, so its definition IDs are not comparable to the reader's.
	OntologyOtherSchema OntologyReading = "other_schema"
)

// Resolved reports whether the reading established which ontology applied.
func (r OntologyReading) Resolved() bool {
	switch r {
	case OntologySameVersion, OntologyOtherVersion, OntologyOtherSchema:
		return true
	default:
		return false
	}
}

// ReadOntologyUnder compares a recorded ontology identity against the identity
// a reader holds. It never guesses: an absent identity on either side degrades
// to OntologyUnresolved rather than being read as agreement.
func ReadOntologyUnder(recorded, reader OntologyIdentity) OntologyReading {
	if !recorded.Known() || !reader.Known() {
		return OntologyUnresolved
	}
	if recorded.Validate() != nil || reader.Validate() != nil {
		return OntologyMalformed
	}
	switch {
	case recorded.schemaID != reader.schemaID:
		return OntologyOtherSchema
	case recorded.versionID != reader.versionID:
		return OntologyOtherVersion
	default:
		return OntologySameVersion
	}
}
