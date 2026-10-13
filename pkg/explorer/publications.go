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

package explorer

import (
	"context"
	"time"

	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// RevisionPublication is when the corpus published one stored revision: its
// wall-clock PublishedAt (the clock a snapshot's AsOf is taken from) and its
// PublicationSequence, which is monotonic where the wall clock is not. Either
// is zero when the record carries none.
type RevisionPublication struct {
	PublishedAt time.Time
	Sequence    uint64
}

// RevisionPublications reports the publication of each stored revision of the
// requested documents. A document with no stored revision is omitted. The
// authorized layer uses it to bound which revisions a session recorded before
// #695 could have been derived from.
func (e *Explorer) RevisionPublications(
	ctx context.Context, documentIDs []shoal.ID,
) (map[shoal.ID]map[shoal.ID]RevisionPublication, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	for _, documentID := range documentIDs {
		if err := shoal.ValidateRequiredID("document ID", documentID); err != nil {
			return nil, err
		}
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if err := e.requireOpen(); err != nil {
		return nil, err
	}
	published := make(map[shoal.ID]map[shoal.ID]RevisionPublication, len(documentIDs))
	for _, documentID := range documentIDs {
		if _, done := published[documentID]; done {
			continue
		}
		revisions := make(map[shoal.ID]RevisionPublication, len(e.documents[documentID]))
		for revisionID, record := range e.documents[documentID] {
			if record == nil {
				continue
			}
			revisions[revisionID] = RevisionPublication{
				PublishedAt: record.PublishedAt.UTC(),
				Sequence:    record.PublicationSequence,
			}
		}
		if len(revisions) > 0 {
			published[documentID] = revisions
		}
	}
	return published, nil
}

// ConnectedEdges reports which of the requested edges an application
// connected (Connect). An intrinsic, extracted or materialized edge is never
// a connected edge.
func (e *Explorer) ConnectedEdges(
	ctx context.Context, edgeIDs []shoal.ID,
) (map[shoal.ID]bool, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	for _, edgeID := range edgeIDs {
		if err := shoal.ValidateRequiredID("graph edge ID", edgeID); err != nil {
			return nil, err
		}
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	if err := e.requireOpen(); err != nil {
		return nil, err
	}
	connected := make(map[shoal.ID]bool, len(edgeIDs))
	for _, edgeID := range edgeIDs {
		if _, ok := e.edges[edgeID]; ok {
			connected[edgeID] = true
		}
	}
	return connected, nil
}
