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
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

func TestCompressContextEmpty(t *testing.T) {
	got, err := CompressContext(CompressionInput{})
	if err != nil {
		t.Fatalf("compress empty context: %v", err)
	}
	want := CompressionOutput{}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("empty output mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestCompressContextAlreadySmallCanonicalizesWithoutOmission(t *testing.T) {
	input := CompressionInput{
		BudgetBytes: 32,
		Items: []CompressionItem{
			{
				ID:                 "later",
				Sequence:           2,
				Content:            ContextContent{Type: ContextContentJSON, Data: `{"ok":true}`},
				Visibility:         []string{"team"},
				RetrievedSourceIDs: []shoal.ID{"source-b", "source-a", "source-a"},
				CitedSourceIDs:     []shoal.ID{"source-b"},
			},
			{
				ID:         "earlier",
				Sequence:   1,
				Content:    ContextContent{Type: ContextContentText, Data: "hello"},
				Required:   true,
				Visibility: []string{"base"},
			},
		},
		Sources: []SourceReference{
			{
				NodeID:     "source-b",
				References: []string{"mcp://b", "mcp://b"},
				Visibility: []string{"restricted"},
			},
			{
				NodeID:     "source-a",
				References: []string{"mcp://z", "mcp://a"},
				Visibility: []string{"team"},
			},
		},
	}

	got, err := CompressContext(input)
	if err != nil {
		t.Fatalf("compress context: %v", err)
	}
	if got.WasCompressed || len(got.OmittedItemIDs) != 0 {
		t.Fatalf("small context was compressed: %#v", got)
	}
	if got.InputBytes != 16 || got.OutputBytes != 16 {
		t.Fatalf("byte accounting = %d/%d, want 16/16", got.InputBytes, got.OutputBytes)
	}
	if got.Items[0].ID != "earlier" || got.Items[1].ID != "later" {
		t.Fatalf("items not canonical: %#v", got.Items)
	}
	if want := []shoal.ID{"source-a", "source-b"}; !reflect.DeepEqual(
		got.RetrievedSourceIDs, want,
	) {
		t.Fatalf("retrieved IDs = %v, want %v", got.RetrievedSourceIDs, want)
	}
	if want := []shoal.ID{"source-b"}; !reflect.DeepEqual(got.CitedSourceIDs, want) {
		t.Fatalf("cited IDs = %v, want %v", got.CitedSourceIDs, want)
	}
	if want := []string{"base", "restricted", "team"}; !reflect.DeepEqual(
		got.Visibility, want,
	) {
		t.Fatalf("visibility = %v, want %v", got.Visibility, want)
	}
	if want := []string{"mcp://a", "mcp://z"}; !reflect.DeepEqual(
		got.Sources[0].References, want,
	) {
		t.Fatalf("source references = %v, want %v", got.Sources[0].References, want)
	}
	if want := []string{"restricted", "team"}; !reflect.DeepEqual(
		got.Items[1].Visibility, want,
	) {
		t.Fatalf("derived item visibility = %v, want %v", got.Items[1].Visibility, want)
	}
}

func TestCompressContextDeterministicAcrossInputOrdering(t *testing.T) {
	itemA := CompressionItem{
		ID:                 "a",
		Sequence:           1,
		Content:            ContextContent{Type: ContextContentText, Data: "aa"},
		RetrievedSourceIDs: []shoal.ID{"source-a"},
	}
	itemB := CompressionItem{
		ID:             "b",
		Sequence:       2,
		Content:        ContextContent{Type: ContextContentText, Data: "bbb"},
		CitedSourceIDs: []shoal.ID{"source-b"},
	}
	sourceA := SourceReference{NodeID: "source-a", Visibility: []string{"a"}}
	sourceB := SourceReference{NodeID: "source-b", Visibility: []string{"b"}}

	first, err := CompressContext(CompressionInput{
		BudgetBytes: 3,
		Items:       []CompressionItem{itemA, itemB},
		Sources:     []SourceReference{sourceA, sourceB},
	})
	if err != nil {
		t.Fatalf("first compression: %v", err)
	}
	second, err := CompressContext(CompressionInput{
		BudgetBytes: 3,
		Items:       []CompressionItem{itemB, itemA},
		Sources:     []SourceReference{sourceB, sourceA},
	})
	if err != nil {
		t.Fatalf("second compression: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("compression depends on input ordering:\nfirst: %#v\nsecond: %#v", first, second)
	}
	if !first.Items[0].Omitted || first.Items[1].Omitted {
		t.Fatalf("newest fitting item was not selected: %#v", first.Items)
	}
	if !reflect.DeepEqual(first.Items[0].RetrievedSourceIDs, []shoal.ID{"source-a"}) ||
		len(first.Sources) != 2 ||
		!reflect.DeepEqual(first.Visibility, []string{"a", "b"}) {
		t.Fatalf("omission lost provenance: %#v", first)
	}
}

func TestCompressContextBudgetBoundary(t *testing.T) {
	input := CompressionInput{
		Items: []CompressionItem{
			{ID: "old", Sequence: 1, Content: ContextContent{Type: ContextContentText, Data: "aa"}},
			{ID: "new", Sequence: 2, Content: ContextContent{Type: ContextContentText, Data: "bbb"}},
		},
	}
	exact := input
	exact.BudgetBytes = 5
	got, err := CompressContext(exact)
	if err != nil {
		t.Fatalf("exact budget: %v", err)
	}
	if got.WasCompressed || got.OutputBytes != 5 {
		t.Fatalf("exact budget output = %#v", got)
	}

	below := input
	below.BudgetBytes = 4
	got, err = CompressContext(below)
	if err != nil {
		t.Fatalf("one below budget: %v", err)
	}
	if !got.WasCompressed || got.OutputBytes != 3 ||
		!reflect.DeepEqual(got.OmittedItemIDs, []string{"old"}) {
		t.Fatalf("one below budget output = %#v", got)
	}
}

func TestCompressContextZeroBudgetRetainsOnlyZeroByteItems(t *testing.T) {
	got, err := CompressContext(CompressionInput{
		Items: []CompressionItem{
			{
				ID:       "empty",
				Sequence: 1,
				Content:  ContextContent{Type: ContextContentText},
			},
			{
				ID:       "nonempty",
				Sequence: 2,
				Content:  ContextContent{Type: ContextContentText, Data: "x"},
			},
		},
	})
	if err != nil {
		t.Fatalf("zero budget: %v", err)
	}
	if got.Items[0].Omitted || !got.Items[1].Omitted || got.OutputBytes != 0 {
		t.Fatalf("zero budget output = %#v", got)
	}
}

func TestCompressContextRequiredAndErrorItemsTakePrecedence(t *testing.T) {
	got, err := CompressContext(CompressionInput{
		BudgetBytes: 6,
		Items: []CompressionItem{
			{
				ID:       "required",
				Sequence: 1,
				Content:  ContextContent{Type: ContextContentText, Data: "rr"},
				Required: true,
			},
			{
				ID:       "error",
				Sequence: 2,
				Content:  ContextContent{Type: ContextContentText, Data: "fail"},
				IsError:  true,
			},
			{
				ID:       "ordinary",
				Sequence: 3,
				Content:  ContextContent{Type: ContextContentText, Data: "ok"},
			},
		},
	})
	if err != nil {
		t.Fatalf("compress required/error context: %v", err)
	}
	if got.OutputBytes != 6 || got.Items[0].Omitted || got.Items[1].Omitted ||
		!got.Items[1].IsError || !got.Items[2].Omitted {
		t.Fatalf("required/error preservation failed: %#v", got)
	}
}

func TestCompressContextIndivisibleItems(t *testing.T) {
	got, err := CompressContext(CompressionInput{
		BudgetBytes: 3,
		Items: []CompressionItem{
			{ID: "small", Sequence: 1, Content: ContextContent{Type: ContextContentText, Data: "fit"}},
			{ID: "large", Sequence: 2, Content: ContextContent{Type: ContextContentText, Data: "too large"}},
		},
	})
	if err != nil {
		t.Fatalf("compress optional oversized item: %v", err)
	}
	if got.Items[0].Omitted || !got.Items[1].Omitted || got.OutputBytes != 3 {
		t.Fatalf("optional indivisible selection = %#v", got)
	}

	for _, item := range []CompressionItem{
		{
			ID:       "required",
			Content:  ContextContent{Type: ContextContentText, Data: "four"},
			Required: true,
		},
		{
			ID:      "error",
			Content: ContextContent{Type: ContextContentText, Data: "four"},
			IsError: true,
		},
	} {
		_, err := CompressContext(CompressionInput{
			BudgetBytes: 3,
			Items:       []CompressionItem{item},
		})
		if !errors.Is(err, ErrIndivisibleItemExceedsBudget) {
			t.Fatalf("%s oversized error = %v", item.ID, err)
		}
	}
}

func TestCompressContextRequiredAggregateExceedsBudget(t *testing.T) {
	_, err := CompressContext(CompressionInput{
		BudgetBytes: 3,
		Items: []CompressionItem{
			{
				ID:       "a",
				Content:  ContextContent{Type: ContextContentText, Data: "aa"},
				Required: true,
			},
			{
				ID:       "b",
				Content:  ContextContent{Type: ContextContentText, Data: "bb"},
				Required: true,
			},
		},
	})
	if !errors.Is(err, ErrRequiredContextExceedsBudget) {
		t.Fatalf("aggregate required error = %v", err)
	}
}

func TestCompressContextUsesUTF8ByteAccounting(t *testing.T) {
	got, err := CompressContext(CompressionInput{
		BudgetBytes: 4,
		Items: []CompressionItem{
			{
				ID:       "accent",
				Sequence: 1,
				Content:  ContextContent{Type: ContextContentText, Data: "é"},
			},
			{
				ID:       "emoji",
				Sequence: 2,
				Content:  ContextContent{Type: ContextContentText, Data: "🙂"},
			},
		},
	})
	if err != nil {
		t.Fatalf("compress unicode context: %v", err)
	}
	if got.InputBytes != 6 || got.OutputBytes != 4 ||
		!got.Items[0].Omitted || got.Items[1].Omitted ||
		got.Items[1].Content.Data != "🙂" {
		t.Fatalf("unicode byte accounting = %#v", got)
	}
}

func TestCompressContextRejectsMalformedOrUnsupportedContent(t *testing.T) {
	tests := []struct {
		name    string
		input   CompressionInput
		wantErr error
	}{
		{
			name:    "negative budget",
			input:   CompressionInput{BudgetBytes: -1},
			wantErr: ErrInvalidCompressionInput,
		},
		{
			name: "invalid utf8",
			input: CompressionInput{Items: []CompressionItem{{
				ID:      "bad",
				Content: ContextContent{Type: ContextContentText, Data: string([]byte{0xff})},
			}}},
			wantErr: ErrInvalidCompressionInput,
		},
		{
			name: "malformed json",
			input: CompressionInput{Items: []CompressionItem{{
				ID:      "bad",
				Content: ContextContent{Type: ContextContentJSON, Data: `{"open":`},
			}}},
			wantErr: ErrInvalidCompressionInput,
		},
		{
			name: "unsupported",
			input: CompressionInput{Items: []CompressionItem{{
				ID:      "image",
				Content: ContextContent{Type: "image", Data: "bytes"},
			}}},
			wantErr: ErrUnsupportedContextContent,
		},
		{
			name: "missing item id",
			input: CompressionInput{Items: []CompressionItem{{
				Content: ContextContent{Type: ContextContentText, Data: "x"},
			}}},
			wantErr: ErrInvalidCompressionInput,
		},
		{
			name: "duplicate item id",
			input: CompressionInput{Items: []CompressionItem{
				{ID: "same", Content: ContextContent{Type: ContextContentText}},
				{ID: "same", Content: ContextContent{Type: ContextContentText}},
			}},
			wantErr: ErrInvalidCompressionInput,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := CompressContext(test.input)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want errors.Is(%v)", err, test.wantErr)
			}
		})
	}
}

func TestCompressContextRejectsMalformedSourceReferences(t *testing.T) {
	tests := []struct {
		name  string
		input CompressionInput
	}{
		{
			name: "missing source record",
			input: CompressionInput{Items: []CompressionItem{{
				ID:                 "item",
				Content:            ContextContent{Type: ContextContentText},
				RetrievedSourceIDs: []shoal.ID{"missing"},
			}}},
		},
		{
			name: "unreferenced source record",
			input: CompressionInput{
				Items: []CompressionItem{{
					ID:      "item",
					Content: ContextContent{Type: ContextContentText},
				}},
				Sources: []SourceReference{{NodeID: "extra"}},
			},
		},
		{
			name: "duplicate source record",
			input: CompressionInput{
				Items: []CompressionItem{{
					ID:                 "item",
					Content:            ContextContent{Type: ContextContentText},
					RetrievedSourceIDs: []shoal.ID{"same"},
				}},
				Sources: []SourceReference{{NodeID: "same"}, {NodeID: "same"}},
			},
		},
		{
			name: "blank opaque reference",
			input: CompressionInput{
				Items: []CompressionItem{{
					ID:                 "item",
					Content:            ContextContent{Type: ContextContentText},
					RetrievedSourceIDs: []shoal.ID{"source"},
				}},
				Sources: []SourceReference{{NodeID: "source", References: []string{" "}}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := CompressContext(test.input)
			if !errors.Is(err, ErrInvalidCompressionInput) {
				t.Fatalf("error = %v, want invalid compression input", err)
			}
		})
	}
}

func TestCompressContextJoinsSourceVisibilityFailClosed(t *testing.T) {
	got, err := CompressContext(CompressionInput{
		Items: []CompressionItem{{
			ID:                 "item",
			Content:            ContextContent{Type: ContextContentText},
			Visibility:         []string{"item"},
			RetrievedSourceIDs: []shoal.ID{"retrieved"},
			CitedSourceIDs:     []shoal.ID{"cited"},
		}},
		Sources: []SourceReference{
			{NodeID: "cited", Visibility: []string{"cit"}},
			{NodeID: "retrieved", Visibility: []string{"ret"}},
		},
	})
	if err != nil {
		t.Fatalf("compress visible context: %v", err)
	}
	want := []string{"cit", "item", "ret"}
	if !reflect.DeepEqual(got.Visibility, want) ||
		!reflect.DeepEqual(got.Items[0].Visibility, want) {
		t.Fatalf("restrictive visibility join = %v / %v, want %v",
			got.Visibility, got.Items[0].Visibility, want)
	}

	_, err = CompressContext(CompressionInput{
		Items: []CompressionItem{{
			ID:                 "item",
			Content:            ContextContent{Type: ContextContentText},
			RetrievedSourceIDs: []shoal.ID{"source"},
		}},
		Sources: []SourceReference{{
			NodeID:     "source",
			Visibility: []string{"invalid&label"},
		}},
	})
	if !errors.Is(err, ErrInvalidCompressionInput) {
		t.Fatalf("invalid source visibility error = %v", err)
	}
}

func TestCompressContextDoesNotCapSourceIdentitySets(t *testing.T) {
	count := interaction.MaxTouchedNodes + 1
	ids := make([]shoal.ID, count)
	sources := make([]SourceReference, count)
	for index := range count {
		id := shoal.ID(fmt.Sprintf("source-%05d", index))
		ids[index] = id
		sources[index] = SourceReference{NodeID: id}
	}
	got, err := CompressContext(CompressionInput{
		Items: []CompressionItem{{
			ID:                 "item",
			Content:            ContextContent{Type: ContextContentText},
			RetrievedSourceIDs: ids,
			CitedSourceIDs:     ids,
		}},
		Sources: sources,
	})
	if err != nil {
		t.Fatalf("compress uncapped provenance: %v", err)
	}
	if len(got.RetrievedSourceIDs) != count ||
		len(got.CitedSourceIDs) != count ||
		len(got.Sources) != count ||
		len(got.Items[0].RetrievedSourceIDs) != count ||
		len(got.Items[0].CitedSourceIDs) != count {
		t.Fatalf("source identity set was capped: retrieved=%d cited=%d sources=%d",
			len(got.RetrievedSourceIDs), len(got.CitedSourceIDs), len(got.Sources))
	}
}
