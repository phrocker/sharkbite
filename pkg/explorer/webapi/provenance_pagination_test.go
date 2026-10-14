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

package webapi

import (
	"context"
	"sort"
	"testing"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

type pagedProvenanceClient struct {
	interactions []explorer.InteractionSummary
	folds        []explorer.FoldSummary
}

func (c pagedProvenanceClient) InteractionRecordsPage(
	_ context.Context, after shoal.ID, limit uint32,
) (explorer.InteractionRecordPage, error) {
	values := append([]explorer.InteractionSummary(nil), c.interactions...)
	sort.Slice(values, func(i, j int) bool {
		return shoal.CompareID(values[i].SessionID, values[j].SessionID) < 0
	})
	page := explorer.InteractionRecordPage{}
	for _, value := range values {
		if shoal.CompareID(value.SessionID, after) <= 0 {
			continue
		}
		if len(page.Records) == int(limit) {
			page.NextAfter = page.Records[len(page.Records)-1].Summary.SessionID
			break
		}
		page.Records = append(page.Records, explorer.InteractionRecord{
			Summary: value,
		})
	}
	return page, nil
}

func (pagedProvenanceClient) Interaction(
	context.Context, shoal.ID,
) (interaction.Session, error) {
	return interaction.Session{}, nil
}

func (c pagedProvenanceClient) FoldsPage(
	_ context.Context, after shoal.ID, limit uint32,
) (explorer.FoldSummaryPage, error) {
	values := append([]explorer.FoldSummary(nil), c.folds...)
	sort.Slice(values, func(i, j int) bool {
		return shoal.CompareID(values[i].FoldID, values[j].FoldID) < 0
	})
	page := explorer.FoldSummaryPage{}
	for _, value := range values {
		if shoal.CompareID(value.FoldID, after) <= 0 {
			continue
		}
		if len(page.Folds) == int(limit) {
			page.NextAfter = page.Folds[len(page.Folds)-1].FoldID
			break
		}
		page.Folds = append(page.Folds, value)
	}
	return page, nil
}

func (pagedProvenanceClient) FoldInteractions(
	context.Context, explorer.FoldRequest,
) (explorer.FoldResult, error) {
	return explorer.FoldResult{}, nil
}

func (pagedProvenanceClient) RehydrateFold(
	context.Context, shoal.ID,
) (interaction.Fold, error) {
	return interaction.Fold{}, nil
}

func TestProvenanceListUsesBoundedStableCursor(t *testing.T) {
	service := &InteractionService{client: pagedProvenanceClient{
		interactions: []explorer.InteractionSummary{
			{SessionID: "session-b"}, {SessionID: "session-a"},
		},
		folds: []explorer.FoldSummary{{FoldID: "fold-a"}},
	}}
	first, err := service.ListProvenance(
		context.Background(), ProvenanceListRequest{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Interactions)+len(first.Folds) != 2 ||
		first.NextCursor == "" {
		t.Fatalf("first provenance page = %#v", first)
	}
	second, err := service.ListProvenance(
		context.Background(), ProvenanceListRequest{
			Limit: 2, Cursor: first.NextCursor,
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Interactions)+len(second.Folds) != 1 ||
		second.NextCursor != "" {
		t.Fatalf("second provenance page = %#v", second)
	}
	if _, err := service.ListProvenance(
		context.Background(), ProvenanceListRequest{
			Limit: MaxProvenancePageSize + 1,
		},
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("oversized provenance page = %v", err)
	}
	if _, err := service.ListProvenance(
		context.Background(), ProvenanceListRequest{Cursor: "not-base64!"},
	); !shoal.IsErrorCode(err, shoal.ErrorInvalidArgument) {
		t.Fatalf("invalid provenance cursor = %v", err)
	}
}
