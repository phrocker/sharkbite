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

package teamoverview_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer"
	"github.com/phrocker/shoal-oss/pkg/explorer/authorized/authorizedtest"
	"github.com/phrocker/shoal-oss/pkg/explorer/fleet"
	"github.com/phrocker/shoal-oss/pkg/explorer/teamoverview"
	"github.com/phrocker/shoal-oss/pkg/graph"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

const (
	teamLabelledSession shoal.ID = "interaction.session_team_labelled"
	teamPlainSession    shoal.ID = "interaction.session_team_plain"
)

// labelRoster is a one-person team whose member is the holder, with no
// agents or fleet actions, so the overview's only activity is the holder's
// sessions.
type labelRoster struct{}

func (labelRoster) BoundedNeighborhood(
	context.Context, explorer.BoundedNeighborhoodRequest,
) (explorer.BoundedNeighborhood, error) {
	return explorer.BoundedNeighborhood{Neighborhood: explorer.Neighborhood{
		Nodes: []graph.Node{
			{ID: "label-team", Kind: teamoverview.KindTeam,
				Properties: shoal.Metadata{"name": "Label Team"}},
			{ID: "label-person", Kind: teamoverview.KindPerson,
				Properties: shoal.Metadata{
					"name": "Recorder", "subject_id": authorizedtest.HolderSubject,
				}},
		},
		Edges: []graph.Edge{{
			ID: "label-member", From: "label-person", To: "label-team",
			Type: teamoverview.RelationMemberOf,
		}},
	}}, nil
}

func (labelRoster) List(context.Context, fleet.ListRequest) (fleet.ListPage, error) {
	return fleet.ListPage{}, nil
}

func (labelRoster) TeamActions(
	context.Context, fleet.TeamActionListRequest,
) (fleet.ActionPage, error) {
	return fleet.ActionPage{}, nil
}

// consumedInteractions records exactly what the authorized client handed the
// team overview, so the test checks the records the view is built from as
// well as the view itself.
type consumedInteractions struct {
	source  teamoverview.InteractionSource
	records []explorer.InteractionRecord
}

func (c *consumedInteractions) InteractionRecordsPage(
	ctx context.Context, after shoal.ID, limit uint32,
) (explorer.InteractionRecordPage, error) {
	page, err := c.source.InteractionRecordsPage(ctx, after, limit)
	c.records = append(c.records, page.Records...)
	return page, err
}

// TestOverviewShowsLabelledSessionsOnlyToTeammatesHoldingTheLabels: team
// overview is cross-principal by design, so a teammate sees another's
// sessions. A session recorded under labels reaches a teammate holding them
// exactly as stored, and is absent, with no trace, for one who does not
// (#564, #567).
func TestOverviewShowsLabelledSessionsOnlyToTeammatesHoldingTheLabels(t *testing.T) {
	f := authorizedtest.New(t)
	f.Record(t, authorizedtest.HolderSubject, teamLabelledSession,
		authorizedtest.SecretLabels)
	f.Record(t, authorizedtest.HolderSubject, teamPlainSession, nil)
	now := time.Now().UTC()

	overview := func(subject string) (teamoverview.Response, []explorer.InteractionRecord) {
		t.Helper()
		consumed := &consumedInteractions{source: f.Client}
		service, err := teamoverview.NewService(teamoverview.Config{
			Graph: labelRoster{}, Agents: labelRoster{}, Actions: labelRoster{},
			Interactions: consumed, Resolver: f.Authority.Resolver(),
			Clock: func() time.Time { return now },
		})
		if err != nil {
			t.Fatal(err)
		}
		response, err := service.Overview(f.Context(t, subject), teamoverview.Request{
			TeamID:   "label-team",
			SourceID: authorizedtest.SourceID, PolicyID: authorizedtest.PolicyID,
		})
		if err != nil {
			t.Fatal(err)
		}
		return response, consumed.records
	}
	activities := func(response teamoverview.Response) map[string]int {
		seen := map[string]int{}
		for index, activity := range response.Activities {
			seen[activity.ID] = index
		}
		return seen
	}

	held, heldRecords := overview(authorizedtest.HolderSubject)
	seen := activities(held)
	if _, ok := seen[string(teamLabelledSession)]; !ok {
		t.Fatalf("a teammate holding the labels lost the session: %+v", held.Activities)
	}
	for _, record := range heldRecords {
		stored, err := f.Corpus.InteractionRecord(context.Background(), record.Summary.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(record, stored) {
			t.Fatalf("the holder's overview was built from\n%#v\nnot the stored\n%#v",
				record, stored)
		}
	}

	outside, outsideRecords := overview(authorizedtest.OutsiderSubject)
	body, err := json.Marshal(outside)
	if err != nil {
		t.Fatal(err)
	}
	if leaked := authorizedtest.Leaks(body); len(leaked) != 0 {
		t.Fatalf("team overview leaked %v: %s", leaked, body)
	}
	outsideSeen := activities(outside)
	if _, ok := outsideSeen[string(teamLabelledSession)]; ok || len(outside.Activities) != 1 {
		t.Fatalf("a teammate without the labels saw %+v", outside.Activities)
	}
	plainIndex, ok := outsideSeen[string(teamPlainSession)]
	if !ok {
		t.Fatalf("the unlabelled session was lost: %+v", outside.Activities)
	}
	if !reflect.DeepEqual(outside.Activities[plainIndex],
		held.Activities[seen[string(teamPlainSession)]]) {
		t.Fatal("the unlabelled session differs between the two teammates")
	}
	var wantRecords []explorer.InteractionRecord
	for _, record := range heldRecords {
		if record.Summary.SessionID == teamPlainSession {
			wantRecords = append(wantRecords, record)
		}
	}
	gotBytes, err := json.Marshal(outsideRecords)
	if err != nil {
		t.Fatal(err)
	}
	wantBytes, err := json.Marshal(wantRecords)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(outsideRecords, wantRecords) || string(gotBytes) != string(wantBytes) {
		t.Fatalf("the outsider's overview was built from\n%s\nwant, as if the "+
			"labelled session had never been written,\n%s", gotBytes, wantBytes)
	}
}
