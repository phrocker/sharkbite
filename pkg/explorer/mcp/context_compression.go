// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied. See the License for the
// specific language governing permissions and limitations
// under the License.

package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// Context compression reduces content presented to an MCP client or model. It
// is deliberately unrelated to interaction.Fold, which records a provenance
// subgraph and is never used as a context-window mechanism.

// ContextContentType identifies a payload representation understood by the
// native compressor. Unknown types are rejected rather than treated as text.
type ContextContentType string

const (
	ContextContentText ContextContentType = "text"
	ContextContentJSON ContextContentType = "json"
)

var (
	// ErrInvalidCompressionInput identifies malformed compression input.
	ErrInvalidCompressionInput = errors.New("mcp: invalid context compression input")
	// ErrUnsupportedContextContent identifies a content representation the
	// compressor cannot validate or account for.
	ErrUnsupportedContextContent = errors.New("mcp: unsupported context content")
	// ErrIndivisibleItemExceedsBudget means one required or error item cannot
	// fit without splitting it. The compressor never truncates an item.
	ErrIndivisibleItemExceedsBudget = errors.New(
		"mcp: indivisible context item exceeds byte budget")
	// ErrRequiredContextExceedsBudget means required and error items fit
	// individually but not together.
	ErrRequiredContextExceedsBudget = errors.New(
		"mcp: required context exceeds byte budget")
)

// ContextContent is one indivisible payload. Data is measured in UTF-8 bytes,
// not runes or estimated tokens. JSON data must contain exactly one valid JSON
// value; text may be empty.
type ContextContent struct {
	Type ContextContentType
	Data string
}

// SourceReference is the complete audit and authorization description of one
// source identity used by the input. References are opaque stable locators,
// such as MCP resource URIs. The node identity remains authoritative.
//
// The compressor never caps this slice, any References slice, or the
// retrieved/cited identity sets.
type SourceReference struct {
	NodeID     shoal.ID
	References []string
	Visibility []string
}

// CompressionItem is one ordered, indivisible unit of context. Sequence is the
// caller's chronological order; ties are resolved by ID.
//
// Required items are never omitted. Error items are implicitly required so
// compression cannot turn an error into success-shaped content.
type CompressionItem struct {
	ID         string
	Sequence   uint64
	Content    ContextContent
	Required   bool
	IsError    bool
	Visibility []string

	RetrievedSourceIDs []shoal.ID
	CitedSourceIDs     []shoal.ID
}

// CompressionInput is the complete input to one deterministic compression
// pass. BudgetBytes applies only to content payload bytes. Security and
// provenance metadata is never charged against the budget because doing so
// could force source identities or visibility labels to be hidden.
type CompressionInput struct {
	BudgetBytes int
	Items       []CompressionItem
	Sources     []SourceReference
}

// CompressedItem preserves the envelope and provenance of every input item.
// Omitted items remain present with Omitted true and an empty Content value,
// so callers cannot mistake absence of payload for absence of provenance.
type CompressedItem struct {
	ID         string
	Sequence   uint64
	Content    ContextContent
	Omitted    bool
	Required   bool
	IsError    bool
	Visibility []string

	RetrievedSourceIDs []shoal.ID
	CitedSourceIDs     []shoal.ID
}

// CompressionOutput is the canonical result of compression. Items and Sources
// are deterministically ordered. RetrievedSourceIDs, CitedSourceIDs, Sources,
// and Visibility cover the entire input, including omitted item payloads.
type CompressionOutput struct {
	BudgetBytes        int
	InputBytes         int
	OutputBytes        int
	WasCompressed      bool
	Items              []CompressedItem
	OmittedItemIDs     []string
	Sources            []SourceReference
	RetrievedSourceIDs []shoal.ID
	CitedSourceIDs     []shoal.ID
	Visibility         []string
}

// ContextCompressor is the integration seam for the MCP server core. It has no
// transport or persistence responsibilities and does not record tool calls.
type ContextCompressor interface {
	CompressContext(CompressionInput) (CompressionOutput, error)
}

// NativeContextCompressor implements deterministic context compression using
// only native Go data structures and standard-library validation.
type NativeContextCompressor struct{}

var _ ContextCompressor = NativeContextCompressor{}

// CompressContext is a convenience entry point for callers that do not need
// to inject a ContextCompressor.
func CompressContext(input CompressionInput) (CompressionOutput, error) {
	return NativeContextCompressor{}.CompressContext(input)
}

// CompressContext validates and canonicalizes the full input before selecting
// payloads. Required and error payloads are selected first. Remaining payloads
// are considered newest-first and retained whole when they fit; smaller older
// items may therefore fit when a newer indivisible item does not. Output item
// order remains chronological.
func (NativeContextCompressor) CompressContext(
	input CompressionInput,
) (CompressionOutput, error) {
	if input.BudgetBytes < 0 {
		return CompressionOutput{}, invalidCompression("byte budget cannot be negative")
	}

	sources, sourceByID, err := canonicalSources(input.Sources)
	if err != nil {
		return CompressionOutput{}, err
	}
	items, referenced, inputBytes, retrieved, cited, visibility, err :=
		canonicalCompressionItems(input.Items, sourceByID)
	if err != nil {
		return CompressionOutput{}, err
	}
	for _, source := range sources {
		if _, ok := referenced[source.NodeID]; !ok {
			return CompressionOutput{}, invalidCompression(
				"source %q is not referenced by any context item", source.NodeID)
		}
	}
	if len(items) == 0 {
		return CompressionOutput{BudgetBytes: input.BudgetBytes}, nil
	}

	selected := make([]bool, len(items))
	requiredBytes := 0
	for index, item := range items {
		if !item.Required && !item.IsError {
			continue
		}
		size := len(item.Content.Data)
		if size > input.BudgetBytes {
			return CompressionOutput{}, fmt.Errorf(
				"%w: item %q requires %d bytes but budget is %d",
				ErrIndivisibleItemExceedsBudget,
				item.ID,
				size,
				input.BudgetBytes,
			)
		}
		var overflow bool
		requiredBytes, overflow = addBytes(requiredBytes, size)
		if overflow {
			return CompressionOutput{}, invalidCompression(
				"required content byte accounting overflow")
		}
		selected[index] = true
	}
	if requiredBytes > input.BudgetBytes {
		return CompressionOutput{}, fmt.Errorf(
			"%w: required items need %d bytes but budget is %d",
			ErrRequiredContextExceedsBudget,
			requiredBytes,
			input.BudgetBytes,
		)
	}

	outputBytes := requiredBytes
	for index := len(items) - 1; index >= 0; index-- {
		if selected[index] {
			continue
		}
		size := len(items[index].Content.Data)
		if size <= input.BudgetBytes-outputBytes {
			selected[index] = true
			outputBytes += size
		}
	}

	outputItems := make([]CompressedItem, len(items))
	omitted := make([]string, 0)
	for index, item := range items {
		output := CompressedItem{
			ID:                 item.ID,
			Sequence:           item.Sequence,
			Required:           item.Required,
			IsError:            item.IsError,
			Visibility:         cloneStrings(item.Visibility),
			RetrievedSourceIDs: cloneIDs(item.RetrievedSourceIDs),
			CitedSourceIDs:     cloneIDs(item.CitedSourceIDs),
		}
		if selected[index] {
			output.Content = item.Content
		} else {
			output.Omitted = true
			omitted = append(omitted, item.ID)
		}
		outputItems[index] = output
	}

	return CompressionOutput{
		BudgetBytes:        input.BudgetBytes,
		InputBytes:         inputBytes,
		OutputBytes:        outputBytes,
		WasCompressed:      len(omitted) != 0,
		Items:              outputItems,
		OmittedItemIDs:     omitted,
		Sources:            sources,
		RetrievedSourceIDs: retrieved,
		CitedSourceIDs:     cited,
		Visibility:         visibility,
	}, nil
}

func canonicalCompressionItems(
	input []CompressionItem,
	sources map[shoal.ID]SourceReference,
) (
	[]CompressionItem,
	map[shoal.ID]struct{},
	int,
	[]shoal.ID,
	[]shoal.ID,
	[]string,
	error,
) {
	items := make([]CompressionItem, 0, len(input))
	seenItems := make(map[string]struct{}, len(input))
	referenced := make(map[shoal.ID]struct{})
	var allRetrieved, allCited []shoal.ID
	visibilitySets := make([][]string, 0, len(input))
	inputBytes := 0

	for _, raw := range input {
		if !utf8.ValidString(raw.ID) || strings.TrimSpace(raw.ID) == "" {
			return nil, nil, 0, nil, nil, nil,
				invalidCompression("context item ID must be non-empty valid UTF-8")
		}
		if _, duplicate := seenItems[raw.ID]; duplicate {
			return nil, nil, 0, nil, nil, nil,
				invalidCompression("context item ID %q is duplicated", raw.ID)
		}
		seenItems[raw.ID] = struct{}{}
		if err := validateContextContent(raw.Content); err != nil {
			return nil, nil, 0, nil, nil, nil,
				fmt.Errorf("context item %q: %w", raw.ID, err)
		}
		var overflow bool
		inputBytes, overflow = addBytes(inputBytes, len(raw.Content.Data))
		if overflow {
			return nil, nil, 0, nil, nil, nil,
				invalidCompression("content byte accounting overflow")
		}

		retrieved, err := canonicalIDs(
			"context retrieved source ID", raw.RetrievedSourceIDs)
		if err != nil {
			return nil, nil, 0, nil, nil, nil,
				fmt.Errorf("context item %q: %w", raw.ID, err)
		}
		cited, err := canonicalIDs(
			"context cited source ID", raw.CitedSourceIDs)
		if err != nil {
			return nil, nil, 0, nil, nil, nil,
				fmt.Errorf("context item %q: %w", raw.ID, err)
		}
		itemVisibility, err := interaction.Conjoin(raw.Visibility)
		if err != nil {
			return nil, nil, 0, nil, nil, nil,
				invalidCompressionCause(
					err, "context item %q visibility is invalid", raw.ID)
		}
		itemSources := append(cloneIDs(retrieved), cited...)
		itemSources, err = canonicalIDs("context source ID", itemSources)
		if err != nil {
			return nil, nil, 0, nil, nil, nil,
				fmt.Errorf("context item %q: %w", raw.ID, err)
		}
		visibilityInputs := make([][]string, 0, len(itemSources)+1)
		visibilityInputs = append(visibilityInputs, itemVisibility)
		for _, id := range itemSources {
			source, ok := sources[id]
			if !ok {
				return nil, nil, 0, nil, nil, nil,
					invalidCompression(
						"context item %q references source %q without a source reference",
						raw.ID,
						id,
					)
			}
			referenced[id] = struct{}{}
			visibilityInputs = append(visibilityInputs, source.Visibility)
		}
		itemVisibility, err = interaction.Conjoin(visibilityInputs...)
		if err != nil {
			return nil, nil, 0, nil, nil, nil,
				invalidCompressionCause(
					err, "context item %q derived visibility is invalid", raw.ID)
		}

		items = append(items, CompressionItem{
			ID:                 raw.ID,
			Sequence:           raw.Sequence,
			Content:            raw.Content,
			Required:           raw.Required,
			IsError:            raw.IsError,
			Visibility:         itemVisibility,
			RetrievedSourceIDs: retrieved,
			CitedSourceIDs:     cited,
		})
		allRetrieved = append(allRetrieved, retrieved...)
		allCited = append(allCited, cited...)
		visibilitySets = append(visibilitySets, itemVisibility)
	}

	sort.Slice(items, func(i, j int) bool {
		if items[i].Sequence != items[j].Sequence {
			return items[i].Sequence < items[j].Sequence
		}
		return items[i].ID < items[j].ID
	})
	allRetrieved, err := canonicalIDs("context retrieved source ID", allRetrieved)
	if err != nil {
		return nil, nil, 0, nil, nil, nil, err
	}
	allCited, err = canonicalIDs("context cited source ID", allCited)
	if err != nil {
		return nil, nil, 0, nil, nil, nil, err
	}
	visibility, err := interaction.Conjoin(visibilitySets...)
	if err != nil {
		return nil, nil, 0, nil, nil, nil,
			invalidCompressionCause(err, "context derived visibility is invalid")
	}
	return items, referenced, inputBytes, allRetrieved, allCited, visibility, nil
}

func canonicalSources(
	input []SourceReference,
) ([]SourceReference, map[shoal.ID]SourceReference, error) {
	sources := make([]SourceReference, 0, len(input))
	byID := make(map[shoal.ID]SourceReference, len(input))
	for _, raw := range input {
		if err := shoal.ValidateRequiredID("context source ID", raw.NodeID); err != nil {
			return nil, nil, invalidCompression("%v", err)
		}
		if _, duplicate := byID[raw.NodeID]; duplicate {
			return nil, nil, invalidCompression(
				"context source ID %q is duplicated", raw.NodeID)
		}
		references, err := canonicalReferences(raw.References)
		if err != nil {
			return nil, nil, fmt.Errorf("context source %q: %w", raw.NodeID, err)
		}
		visibility, err := interaction.Conjoin(raw.Visibility)
		if err != nil {
			return nil, nil, invalidCompressionCause(
				err, "context source %q visibility is invalid", raw.NodeID)
		}
		source := SourceReference{
			NodeID:     raw.NodeID,
			References: references,
			Visibility: visibility,
		}
		sources = append(sources, source)
		byID[source.NodeID] = source
	}
	sort.Slice(sources, func(i, j int) bool {
		return shoal.CompareID(sources[i].NodeID, sources[j].NodeID) < 0
	})
	return sources, byID, nil
}

func canonicalReferences(input []string) ([]string, error) {
	seen := make(map[string]struct{}, len(input))
	for _, reference := range input {
		if !utf8.ValidString(reference) || strings.TrimSpace(reference) == "" {
			return nil, invalidCompression(
				"source reference must be non-empty valid UTF-8")
		}
		seen[reference] = struct{}{}
	}
	references := make([]string, 0, len(seen))
	for reference := range seen {
		references = append(references, reference)
	}
	sort.Strings(references)
	return references, nil
}

func canonicalIDs(name string, input []shoal.ID) ([]shoal.ID, error) {
	seen := make(map[shoal.ID]struct{}, len(input))
	for _, id := range input {
		if err := shoal.ValidateRequiredID(name, id); err != nil {
			return nil, invalidCompression("%v", err)
		}
		seen[id] = struct{}{}
	}
	ids := make([]shoal.ID, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		return shoal.CompareID(ids[i], ids[j]) < 0
	})
	return ids, nil
}

func validateContextContent(content ContextContent) error {
	if !utf8.ValidString(content.Data) {
		return invalidCompression("context content must be valid UTF-8")
	}
	switch content.Type {
	case ContextContentText:
		return nil
	case ContextContentJSON:
		if !json.Valid([]byte(content.Data)) {
			return invalidCompression("JSON context content is malformed")
		}
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrUnsupportedContextContent, content.Type)
	}
}

func invalidCompression(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidCompressionInput, fmt.Sprintf(format, args...))
}

func invalidCompressionCause(cause error, format string, args ...any) error {
	return fmt.Errorf(
		"%w: %s: %w",
		ErrInvalidCompressionInput,
		fmt.Sprintf(format, args...),
		cause,
	)
}

func addBytes(total, next int) (int, bool) {
	if next > math.MaxInt-total {
		return 0, true
	}
	return total + next, false
}

func cloneIDs(input []shoal.ID) []shoal.ID {
	if len(input) == 0 {
		return nil
	}
	return append([]shoal.ID(nil), input...)
}

func cloneStrings(input []string) []string {
	if len(input) == 0 {
		return nil
	}
	return append([]string(nil), input...)
}
