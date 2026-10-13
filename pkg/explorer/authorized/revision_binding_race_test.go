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

package authorized_test

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phrocker/shoal-oss/pkg/explorer/authorized"
	"github.com/phrocker/shoal-oss/pkg/interaction"
	"github.com/phrocker/shoal-oss/pkg/shoal"
)

// relabelAfterValidation runs after once, just after the first snapshot
// validation it serves succeeds. It also serves as a snapshot validator that
// is not a PublicationReader.
type relabelAfterValidation struct {
	inner authorized.EvidenceSnapshotValidator
	after func()
}

func (v *relabelAfterValidation) fire(err error) error {
	if err == nil && v.after != nil {
		after := v.after
		v.after = nil
		after()
	}
	return err
}

func (v *relabelAfterValidation) ValidateSnapshot(
	ctx context.Context, id shoal.ID, asOf time.Time, nodeIDs []shoal.ID,
) error {
	return v.fire(v.inner.ValidateSnapshot(ctx, id, asOf, nodeIDs))
}

func (v *relabelAfterValidation) ValidateEvidenceSnapshot(
	ctx context.Context, id shoal.ID, asOf time.Time,
	nodeIDs, edgeIDs []shoal.ID, references []interaction.EvidenceReference,
) error {
	return v.fire(v.inner.ValidateEvidenceSnapshot(
		ctx, id, asOf, nodeIDs, edgeIDs, references))
}

// movingCatalog moves one document node to another revision once moved is
// set: the catalog naming a different revision for the same node at a later
// read of one recording, as a registration landing mid-recording does.
type movingCatalog struct {
	authorized.PolicyStore
	nodeID, revisionID shoal.ID
	moved              atomic.Bool
}

func (s *movingCatalog) Nodes(
	ctx context.Context, nodeIDs []shoal.ID,
) (map[shoal.ID]authorized.NodeRegistration, error) {
	resolved, err := s.PolicyStore.Nodes(ctx, nodeIDs)
	if err != nil || !s.moved.Load() {
		return resolved, err
	}
	if registration, ok := resolved[s.nodeID]; ok {
		registration.RevisionID = s.revisionID
		resolved[s.nodeID] = registration
	}
	return resolved, nil
}

// TestRecordingBindsWhatTheCatalogNamedAtEitherRead: the binding is the
// union of the revisions the catalog named before the sources were
// authorized and after the snapshot was validated. Each read alone misses
// one of them.
func TestRecordingBindsWhatTheCatalogNamedAtEitherRead(t *testing.T) {
	withPolicyStores(t, func(t *testing.T, store authorized.PolicyStore) {
		w := newBindingWorld(t, newFixture(t), store)
		inner, ok := trustedSnapshotValidator(w.f.base).(authorized.EvidenceSnapshotValidator)
		if !ok {
			t.Fatal("the corpus does not validate evidence snapshots")
		}
		moving := &movingCatalog{
			PolicyStore: store, nodeID: w.closed.Document.ID,
			revisionID: "revision-registered-mid-recording",
		}
		validator := &relabelAfterValidation{inner: inner, after: func() {
			moving.moved.Store(true)
		}}
		selector := w.f.staticSelector(t, w.f.sourceA, w.f.policyA)
		recorder, err := authorized.NewClient(authorized.Config{
			Base:              w.f.base,
			InteractionWriter: trustedInteractionWriter(w.f.base),
			InteractionReader: trustedInteractionReader(w.f.base),
			SnapshotValidator: validator,
			FoldStore:         trustedFoldStore(w.f.base),
			Resolver:          w.f.authority.Resolver(),
			PolicySelector:    selector,
			PolicyStore:       moving,
			GenerationReader:  w.f.reader,
			Clock:             w.f.clock.Now,
		})
		if err != nil {
			t.Fatal(err)
		}
		w.record(recorder, bindingSession, w.closed.Document.ID)
		if !moving.moved.Load() {
			t.Fatal("the catalog did not move during the recording")
		}
		recorded, err := store.InteractionRevisions(
			context.Background(), []shoal.ID{bindingSession})
		if err != nil {
			t.Fatal(err)
		}
		want := []authorized.RevisionReference{
			{DocumentID: w.closed.Document.ID, RevisionID: moving.revisionID},
			{DocumentID: w.closed.Document.ID, RevisionID: w.closed.Revision.ID},
		}
		if want[1].RevisionID < want[0].RevisionID {
			want[0], want[1] = want[1], want[0]
		}
		if !reflect.DeepEqual(recorded[bindingSession], want) {
			t.Fatalf("recorded %+v, want both revisions %+v",
				recorded[bindingSession], want)
		}
	})
}
