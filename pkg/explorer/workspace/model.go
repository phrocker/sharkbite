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

// Package workspace provides durable, versioned settings that can only narrow
// an Explorer authorization decision.
package workspace

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"sort"

	"github.com/phrocker/shoal-oss/pkg/explorer/auth"
	"github.com/phrocker/shoal-oss/pkg/ontology"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	MaxOutputPolicies = 64
	MaxRetrievalTopK  = 50
	MaxGraphDepth     = 4
	MaxGraphFanout    = 50
	MaxGraphNodes     = 250
	MaxOutputBytes    = 64 << 20
)

// OperationSelection distinguishes inheriting the caller's complete operation
// set from selecting an explicit subset.
type OperationSelection struct {
	Present bool
	Values  []auth.Operation
}

// IDSelection distinguishes inheriting a complete caller scope from selecting
// an explicit subset. Present with an empty Values slice means deny the entire
// scope; absent means inherit it.
type IDSelection struct {
	Present bool
	Values  [][]byte
}

// Budgets contains optional upper bounds. A nil field inherits the caller's
// bound; a non-nil field lowers it.
type Budgets struct {
	RetrievalTopK *uint32
	GraphDepth    *uint32
	GraphFanout   *uint32
	GraphNodes    *uint32
	OutputBytes   *uint64
}

// Limits is the concrete budget in force for one effective decision.
type Limits struct {
	RetrievalTopK uint32
	GraphDepth    uint32
	GraphFanout   uint32
	GraphNodes    uint32
	OutputBytes   uint64
}

// MaximumLimits returns the public workspace budget ceilings.
func MaximumLimits() Limits {
	return Limits{
		RetrievalTopK: MaxRetrievalTopK,
		GraphDepth:    MaxGraphDepth,
		GraphFanout:   MaxGraphFanout,
		GraphNodes:    MaxGraphNodes,
		OutputBytes:   MaxOutputBytes,
	}
}

// OutputPolicySpec identifies an existing authorization policy whose labels
// must be conjoined onto outputs. The provider supplies the caller's trusted
// authorization domain and, for service callers, service role.
type OutputPolicySpec struct {
	SourceID      []byte
	GrantPolicyID []byte
	Epoch         int64
}

// OntologySelection is an explicit governed read-time ontology choice.
type OntologySelection struct {
	Present  bool
	Identity ontology.OntologyIdentity
}

// UpdateNarrowing is the caller-facing settings mutation.
type UpdateNarrowing struct {
	AllowedOperations  OperationSelection
	PermittedSourceIDs IDSelection
	PermittedPolicyIDs IDSelection
	Budgets            Budgets
	OutputPolicies     []OutputPolicySpec
	SelectedOntology   OntologySelection
}

// UpdateRequest applies one expected-revision compare-and-swap. MutationID is
// an opaque idempotency key.
type UpdateRequest struct {
	ExpectedRevision uint64
	MutationID       shoal.ID
	Narrowing        UpdateNarrowing
}

// Narrowing is the normalized durable settings payload.
type Narrowing struct {
	AllowedOperations  OperationSelection
	PermittedSourceIDs IDSelection
	PermittedPolicyIDs IDSelection
	Budgets            Budgets
	OutputPolicies     []auth.Policy
	SelectedOntology   OntologySelection
}

// Settings is one durable workspace settings revision.
type Settings struct {
	WorkspaceID         shoal.ID
	SettingsID          shoal.ID
	Owner               shoal.ID
	AuthorizationDomain []byte
	Revision            uint64
	LastMutationID      shoal.ID
	Narrowing           Narrowing
}

func (s Settings) clone() Settings {
	s.AuthorizationDomain = append([]byte(nil), s.AuthorizationDomain...)
	s.Narrowing = s.Narrowing.clone()
	return s
}

func (n Narrowing) clone() Narrowing {
	n.AllowedOperations.Values = append(
		[]auth.Operation(nil), n.AllowedOperations.Values...)
	n.PermittedSourceIDs.Values = cloneByteSlices(n.PermittedSourceIDs.Values)
	n.PermittedPolicyIDs.Values = cloneByteSlices(n.PermittedPolicyIDs.Values)
	n.Budgets = cloneBudgets(n.Budgets)
	n.OutputPolicies = append([]auth.Policy(nil), n.OutputPolicies...)
	return n
}

func cloneBudgets(value Budgets) Budgets {
	return Budgets{
		RetrievalTopK: cloneUint32(value.RetrievalTopK),
		GraphDepth:    cloneUint32(value.GraphDepth),
		GraphFanout:   cloneUint32(value.GraphFanout),
		GraphNodes:    cloneUint32(value.GraphNodes),
		OutputBytes:   cloneUint64(value.OutputBytes),
	}
}

func cloneUint32(value *uint32) *uint32 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneUint64(value *uint64) *uint64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func cloneByteSlices(values [][]byte) [][]byte {
	if values == nil {
		return nil
	}
	cloned := make([][]byte, len(values))
	for i := range values {
		cloned[i] = append([]byte(nil), values[i]...)
	}
	return cloned
}

func normalizeOperations(selection OperationSelection) (OperationSelection, error) {
	if !selection.Present {
		if len(selection.Values) != 0 {
			return OperationSelection{}, invalid("operations have values while omitted")
		}
		return OperationSelection{}, nil
	}
	if len(selection.Values) == 0 {
		return OperationSelection{}, invalid("allowed operations cannot be empty")
	}
	if len(selection.Values) > auth.MaxDecisionOperations {
		return OperationSelection{}, invalid("allowed operations exceed the public bound")
	}
	values := append([]auth.Operation(nil), selection.Values...)
	for _, operation := range values {
		if err := operation.Validate(); err != nil {
			return OperationSelection{}, err
		}
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	values = uniqueOperations(values)
	return OperationSelection{Present: true, Values: values}, nil
}

func normalizeIDs(name string, selection IDSelection) (IDSelection, error) {
	if !selection.Present {
		if len(selection.Values) != 0 {
			return IDSelection{}, invalid(name + " have values while omitted")
		}
		return IDSelection{}, nil
	}
	if len(selection.Values) > auth.MaxDecisionGrantIDs {
		return IDSelection{}, invalid(name + " exceed the public ID bound")
	}
	values := cloneByteSlices(selection.Values)
	for _, value := range values {
		if len(value) == 0 || len(value) > auth.MaxPolicyComponentBytes {
			return IDSelection{}, invalid(name + " contain an invalid identity")
		}
	}
	sort.Slice(values, func(i, j int) bool {
		return bytes.Compare(values[i], values[j]) < 0
	})
	values = uniqueBytes(values)
	return IDSelection{Present: true, Values: values}, nil
}

func normalizeBudgets(budgets Budgets) (Budgets, error) {
	if err := validateBound("retrieval top-k", budgets.RetrievalTopK, MaxRetrievalTopK); err != nil {
		return Budgets{}, err
	}
	if err := validateBound("graph depth", budgets.GraphDepth, MaxGraphDepth); err != nil {
		return Budgets{}, err
	}
	if err := validateBound("graph fanout", budgets.GraphFanout, MaxGraphFanout); err != nil {
		return Budgets{}, err
	}
	if err := validateBound("graph nodes", budgets.GraphNodes, MaxGraphNodes); err != nil {
		return Budgets{}, err
	}
	if budgets.OutputBytes != nil &&
		(*budgets.OutputBytes == 0 || *budgets.OutputBytes > MaxOutputBytes) {
		return Budgets{}, invalid("output bytes are outside the public bound")
	}
	return cloneBudgets(budgets), nil
}

func validateBound(name string, value *uint32, maximum uint32) error {
	if value != nil && (*value == 0 || *value > maximum) {
		return invalid(name + " is outside the public bound")
	}
	return nil
}

func normalizeLimits(limits Limits) (Limits, error) {
	if limits.RetrievalTopK == 0 || limits.RetrievalTopK > MaxRetrievalTopK ||
		limits.GraphDepth == 0 || limits.GraphDepth > MaxGraphDepth ||
		limits.GraphFanout == 0 || limits.GraphFanout > MaxGraphFanout ||
		limits.GraphNodes == 0 || limits.GraphNodes > MaxGraphNodes ||
		limits.OutputBytes == 0 || limits.OutputBytes > MaxOutputBytes {
		return Limits{}, invalid("base limits are outside the public bounds")
	}
	return limits, nil
}

func normalizePolicies(policies []auth.Policy) ([]auth.Policy, error) {
	if len(policies) > MaxOutputPolicies {
		return nil, invalid("output policies exceed the public bound")
	}
	type encodedPolicy struct {
		encoded []byte
		policy  auth.Policy
	}
	encoded := make([]encodedPolicy, 0, len(policies))
	for _, policy := range policies {
		value, err := policy.Encode()
		if err != nil {
			return nil, err
		}
		cloned, err := auth.DecodePolicy(value)
		if err != nil {
			return nil, err
		}
		encoded = append(encoded, encodedPolicy{
			encoded: value,
			policy:  cloned,
		})
	}
	sort.Slice(encoded, func(i, j int) bool {
		return bytes.Compare(encoded[i].encoded, encoded[j].encoded) < 0
	})
	result := make([]auth.Policy, 0, len(encoded))
	var previous []byte
	for _, item := range encoded {
		if previous != nil && bytes.Equal(previous, item.encoded) {
			continue
		}
		previous = item.encoded
		result = append(result, item.policy)
	}
	if len(result) > 0 {
		if _, err := auth.ConjoinPolicies(result...); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func validateOntology(selection OntologySelection) error {
	if !selection.Present {
		if selection.Identity.Known() {
			return invalid("selected ontology has a value while omitted")
		}
		return nil
	}
	if !selection.Identity.Known() {
		return invalid("selected ontology is required when present")
	}
	return selection.Identity.Validate()
}

func normalizeNarrowing(value Narrowing) (Narrowing, error) {
	operations, err := normalizeOperations(value.AllowedOperations)
	if err != nil {
		return Narrowing{}, err
	}
	sources, err := normalizeIDs("permitted source IDs", value.PermittedSourceIDs)
	if err != nil {
		return Narrowing{}, err
	}
	policies, err := normalizeIDs("permitted policy IDs", value.PermittedPolicyIDs)
	if err != nil {
		return Narrowing{}, err
	}
	budgets, err := normalizeBudgets(value.Budgets)
	if err != nil {
		return Narrowing{}, err
	}
	outputPolicies, err := normalizePolicies(value.OutputPolicies)
	if err != nil {
		return Narrowing{}, err
	}
	if err := validateOntology(value.SelectedOntology); err != nil {
		return Narrowing{}, err
	}
	return Narrowing{
		AllowedOperations: operations, PermittedSourceIDs: sources,
		PermittedPolicyIDs: policies, Budgets: budgets,
		OutputPolicies: outputPolicies, SelectedOntology: value.SelectedOntology,
	}, nil
}

func validateSettings(value Settings) error {
	if err := shoal.ValidateRequiredID("workspace ID", value.WorkspaceID); err != nil {
		return err
	}
	if err := shoal.ValidateRequiredID("settings ID", value.SettingsID); err != nil {
		return err
	}
	if err := shoal.ValidateRequiredID("settings owner", value.Owner); err != nil {
		return err
	}
	if len(value.AuthorizationDomain) == 0 ||
		len(value.AuthorizationDomain) > auth.MaxPolicyComponentBytes {
		return invalid("settings authorization domain is invalid")
	}
	if err := shoal.ValidateRequiredID("settings mutation ID", value.LastMutationID); err != nil {
		return err
	}
	if value.Revision == 0 {
		return invalid("settings revision must be positive")
	}
	if value.SettingsID != settingsIdentity(
		value.WorkspaceID, value.Owner, value.AuthorizationDomain) {
		return invalid("settings identity does not match workspace ownership")
	}
	_, err := normalizeNarrowing(value.Narrowing)
	return err
}

func settingsIdentity(
	workspaceID, owner shoal.ID,
	authorizationDomain []byte,
) shoal.ID {
	hash := sha256.New()
	writeDigestPart(hash, []byte("shoal-workspace-settings-v1"))
	writeDigestPart(hash, []byte(workspaceID))
	writeDigestPart(hash, []byte(owner))
	writeDigestPart(hash, authorizationDomain)
	return shoal.ID("workspace-settings-" + hex.EncodeToString(hash.Sum(nil)))
}

func uniqueOperations(values []auth.Operation) []auth.Operation {
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func uniqueBytes(values [][]byte) [][]byte {
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || !bytes.Equal(result[len(result)-1], value) {
			result = append(result, value)
		}
	}
	return result
}

func invalid(message string) error {
	return shoal.NewError(shoal.ErrorInvalidArgument, message)
}
